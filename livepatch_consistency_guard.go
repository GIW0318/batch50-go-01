// Package task validates kernel live patches against target hosts and orders
// their loading into a deterministic, transactional stack.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors returned by the guard.
var (
	ErrBlankPatch        = errors.New("patch id is required")
	ErrPatchExists       = errors.New("patch already registered")
	ErrPatchLimit        = errors.New("patch table is full")
	ErrCyclicPatch       = errors.New("patch depends on itself")
	ErrBadVersionRange   = errors.New("kernel version range is invalid")
	ErrBlankHost         = errors.New("host name and kernel version are required")
	ErrHostExists        = errors.New("host already registered")
	ErrHostLimit         = errors.New("host table is full")
	ErrUnknownHost       = errors.New("unknown host")
	ErrUnknownPatch      = errors.New("unknown patch")
	ErrPrereqMissing     = errors.New("prerequisite patch is missing")
	ErrKernelMismatch    = errors.New("patch does not apply to this kernel")
	ErrPatchConflict     = errors.New("patches conflict with each other")
	ErrUnknownPlan       = errors.New("unknown plan")
	ErrPlanConsumed      = errors.New("plan has already been applied")
	ErrPlanNotApplied    = errors.New("plan was never applied")
	ErrStackLimit        = errors.New("patch stack depth limit reached")
	ErrHostBusy          = errors.New("host already has a patch transaction in flight")
	ErrBadRollbackReason = errors.New("rollback reason is required")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Patch describes one live patch and its declarations.
type Patch struct {
	ID        string
	Prereqs   []string
	Conflicts []string
	MinKernel string
	MaxKernel string
	BuildID   string
	Seq       int64
}

// PatchID is the identity of a registered patch.
type PatchID string

// Plan is a deterministic loading order for one host.
type Plan struct {
	ID     string
	Host   string
	Order  []PatchID
	Kernel string
	Seq    uint64
}

// Result is the outcome of one applied plan.
type Result struct {
	PlanID  string
	Host    string
	Applied []PatchID
	Depth   int
}

// HostState is the live projection of one target host.
type HostState struct {
	Host         string
	Kernel       string
	BuildID      string
	Stack        []PatchID
	Busy         bool
	LastRollback string
	LastError    string
}

// Limits bounds the host table, the patch table and the stack depth.
type Limits struct {
	MaxStackDepth int
	MaxHosts      int
	MaxPatches    int
	Now           func() time.Time
}

type hostRecord struct {
	mu sync.Mutex

	name         string
	kernel       string
	buildID      string
	stack        []PatchID
	busy         bool
	lastRollback string
	lastError    string
	pending      *pendingTransaction
}

type pendingTransaction struct {
	PlanID     string
	StartStack []PatchID
	Applied    []PatchID
	StartedAt  time.Time
}

type planRecord struct {
	plan     Plan
	consumed bool
	applied  bool
	loaded   []PatchID
}

// LivepatchConsistencyGuard owns the patch table, the host table and the plans.
type LivepatchConsistencyGuard struct {
	limits Limits

	ops sync.RWMutex
	mu  sync.RWMutex

	patches    map[PatchID]Patch
	patchOrder []PatchID
	hosts      map[string]*hostRecord
	hostOrder  []string
	plans      map[string]*planRecord
	planOrder  []string
	nextPlan   uint64
}

// NewLivepatchConsistencyGuard builds a guard with the given caps.
func NewLivepatchConsistencyGuard(limits Limits) *LivepatchConsistencyGuard {
	g := &LivepatchConsistencyGuard{limits: limits}
	g.initLocked()
	return g
}

func (g *LivepatchConsistencyGuard) initLocked() {
	if g.patches == nil {
		g.patches = make(map[PatchID]Patch)
	}
	if g.hosts == nil {
		g.hosts = make(map[string]*hostRecord)
	}
	if g.plans == nil {
		g.plans = make(map[string]*planRecord)
	}
}

func (g *LivepatchConsistencyGuard) now() time.Time {
	if g.limits.Now != nil {
		return g.limits.Now()
	}
	return time.Now()
}

func limitReached(limit, count int) bool {
	return limit > 0 && count >= limit
}

func limitExceeded(limit, count int) bool {
	return limit > 0 && count > limit
}

func containsID(ids []PatchID, want PatchID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func containsString(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func cloneIDs(ids []PatchID) []PatchID {
	if len(ids) == 0 {
		return nil
	}
	return append([]PatchID(nil), ids...)
}

func cloneStrings(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return append([]string(nil), ids...)
}

func clonePatch(p Patch) Patch {
	p.Prereqs = cloneStrings(p.Prereqs)
	p.Conflicts = cloneStrings(p.Conflicts)
	return p
}

func numericSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0"
	}
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return "0"
	}
	s = s[:digits]
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

func compareVersion(a, b string) int {
	as := strings.Split(strings.TrimSpace(a), ".")
	bs := strings.Split(strings.TrimSpace(b), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := "0", "0"
		if i < len(as) {
			av = numericSegment(as[i])
		}
		if i < len(bs) {
			bv = numericSegment(bs[i])
		}
		if len(av) != len(bv) {
			if len(av) < len(bv) {
				return -1
			}
			return 1
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func badVersionRange(p Patch) bool {
	return p.MinKernel != "" && p.MaxKernel != "" && compareVersion(p.MinKernel, p.MaxKernel) > 0
}

func kernelInRange(kernel string, p Patch) bool {
	if p.MinKernel != "" && compareVersion(kernel, p.MinKernel) < 0 {
		return false
	}
	if p.MaxKernel != "" && compareVersion(kernel, p.MaxKernel) > 0 {
		return false
	}
	return true
}

func patchesConflict(a, b Patch) bool {
	if a.ID == b.ID {
		return false
	}
	return containsString(a.Conflicts, b.ID) || containsString(b.Conflicts, a.ID)
}

// RegisterHost records one target host with its kernel and build identity.
func (g *LivepatchConsistencyGuard) RegisterHost(host, kernel, buildID string) error {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(kernel) == "" || strings.TrimSpace(buildID) == "" {
		return ErrBlankHost
	}

	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	if _, ok := g.hosts[host]; ok {
		return ErrHostExists
	}
	if limitReached(g.limits.MaxHosts, len(g.hosts)) {
		return ErrHostLimit
	}
	g.hosts[host] = &hostRecord{name: host, kernel: kernel, buildID: buildID}
	g.hostOrder = append(g.hostOrder, host)
	return nil
}

// Register records one patch and returns its identifier.
func (g *LivepatchConsistencyGuard) Register(p Patch) (PatchID, error) {
	if strings.TrimSpace(p.ID) == "" {
		return "", ErrBlankPatch
	}
	p = clonePatch(p)
	id := PatchID(p.ID)

	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	if _, ok := g.patches[id]; ok {
		return "", ErrPatchExists
	}
	if containsString(p.Prereqs, p.ID) {
		return "", ErrCyclicPatch
	}
	if badVersionRange(p) {
		return "", ErrBadVersionRange
	}
	if limitReached(g.limits.MaxPatches, len(g.patches)) {
		return "", ErrPatchLimit
	}

	g.patches[id] = p
	g.patchOrder = append(g.patchOrder, id)
	return id, nil
}

func (g *LivepatchConsistencyGuard) patchActiveLocked(h *hostRecord, p Patch) bool {
	return p.BuildID != "" && h.buildID == p.BuildID
}

func (g *LivepatchConsistencyGuard) patchLoadedLocked(h *hostRecord, id PatchID) bool {
	if containsID(h.stack, id) {
		return true
	}
	p, ok := g.patches[id]
	return ok && g.patchActiveLocked(h, p)
}

func (g *LivepatchConsistencyGuard) conflictWithLoadedLocked(h *hostRecord, p Patch, added []PatchID) bool {
	for _, id := range h.stack {
		other, ok := g.patches[id]
		if ok && patchesConflict(p, other) {
			return true
		}
	}
	for _, id := range added {
		other, ok := g.patches[id]
		if ok && patchesConflict(p, other) {
			return true
		}
	}
	for id, other := range g.patches {
		if containsID(h.stack, id) || containsID(added, id) {
			continue
		}
		if g.patchActiveLocked(h, other) && patchesConflict(p, other) {
			return true
		}
	}
	return false
}

// Plan computes the deterministic loading order for one host.
func (g *LivepatchConsistencyGuard) Plan(host string) (Plan, error) {
	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	h, ok := g.hosts[host]
	if !ok {
		return Plan{}, ErrUnknownHost
	}

	selected := make(map[PatchID]Patch)
	for _, id := range g.patchOrder {
		p := g.patches[id]
		if containsID(h.stack, id) || g.patchActiveLocked(h, p) {
			continue
		}
		selected[id] = p
	}

	for _, id := range g.patchOrder {
		p, ok := selected[id]
		if !ok {
			continue
		}
		if !kernelInRange(h.kernel, p) {
			return Plan{}, ErrKernelMismatch
		}
		for _, prereq := range p.Prereqs {
			pid := PatchID(prereq)
			if _, exists := g.patches[pid]; !exists {
				return Plan{}, ErrPrereqMissing
			}
			if g.patchLoadedLocked(h, pid) {
				continue
			}
			if _, included := selected[pid]; !included {
				return Plan{}, ErrPrereqMissing
			}
		}
		if g.conflictWithLoadedLocked(h, p, nil) {
			return Plan{}, ErrPatchConflict
		}
	}

	selectedIDs := make([]PatchID, 0, len(selected))
	for id := range selected {
		selectedIDs = append(selectedIDs, id)
	}
	sort.Slice(selectedIDs, func(i, j int) bool { return selectedIDs[i] < selectedIDs[j] })
	for i, id := range selectedIDs {
		for j := i + 1; j < len(selectedIDs); j++ {
			if patchesConflict(selected[id], selected[selectedIDs[j]]) {
				return Plan{}, ErrPatchConflict
			}
		}
	}

	indegree := make(map[PatchID]int, len(selected))
	dependents := make(map[PatchID][]PatchID, len(selected))
	for id := range selected {
		indegree[id] = 0
	}
	for id, p := range selected {
		seen := make(map[PatchID]bool)
		for _, prereq := range p.Prereqs {
			pid := PatchID(prereq)
			if seen[pid] {
				continue
			}
			seen[pid] = true
			if _, included := selected[pid]; !included {
				continue
			}
			indegree[id]++
			dependents[pid] = append(dependents[pid], id)
		}
	}

	ready := make([]PatchID, 0, len(selected))
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })

	order := make([]PatchID, 0, len(selected))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := dependents[id]
		sort.Slice(next, func(i, j int) bool { return next[i] < next[j] })
		for _, dependent := range next {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	}
	if len(order) != len(selected) {
		return Plan{}, ErrCyclicPatch
	}

	g.nextPlan++
	plan := Plan{
		ID:     fmt.Sprintf("plan-%d", g.nextPlan),
		Host:   host,
		Order:  cloneIDs(order),
		Kernel: h.kernel,
		Seq:    g.nextPlan,
	}
	g.plans[plan.ID] = &planRecord{plan: plan}
	g.planOrder = append(g.planOrder, plan.ID)
	result := plan
	result.Order = cloneIDs(plan.Order)
	return result, nil
}

// Apply loads every patch of one plan in order, all or nothing.
func (g *LivepatchConsistencyGuard) Apply(planID string) (result Result, err error) {
	g.ops.RLock()
	defer g.ops.RUnlock()

	g.mu.RLock()
	pr, ok := g.plans[planID]
	if !ok {
		g.mu.RUnlock()
		return Result{}, ErrUnknownPlan
	}
	if pr.consumed {
		g.mu.RUnlock()
		return Result{}, ErrPlanConsumed
	}
	h, ok := g.hosts[pr.plan.Host]
	g.mu.RUnlock()
	if !ok {
		return Result{}, ErrUnknownHost
	}

	if !h.mu.TryLock() {
		return Result{}, ErrHostBusy
	}
	defer h.mu.Unlock()

	g.mu.Lock()
	pr, ok = g.plans[planID]
	if !ok {
		g.mu.Unlock()
		return Result{}, ErrUnknownPlan
	}
	if pr.consumed {
		g.mu.Unlock()
		return Result{}, ErrPlanConsumed
	}
	h, ok = g.hosts[pr.plan.Host]
	if !ok {
		g.mu.Unlock()
		return Result{}, ErrUnknownHost
	}

	startStack := cloneIDs(h.stack)
	tx := &pendingTransaction{
		PlanID:     planID,
		StartStack: cloneIDs(startStack),
	}
	h.busy = true
	h.pending = tx
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		h.busy = false
		h.pending = nil
		if err != nil {
			h.lastError = err.Error()
		} else {
			h.lastError = ""
		}
		g.mu.Unlock()
	}()

	base := make(map[PatchID]bool, len(startStack))
	for _, id := range startStack {
		base[id] = true
	}
	added := make([]PatchID, 0, len(pr.plan.Order))

	for _, id := range pr.plan.Order {
		g.mu.RLock()
		p, exists := g.patches[id]
		if !exists {
			g.mu.RUnlock()
			return Result{}, ErrUnknownPatch
		}
		if base[id] || containsID(added, id) {
			g.mu.RUnlock()
			continue
		}
		if g.patchActiveLocked(h, p) {
			g.mu.RUnlock()
			continue
		}
		if !kernelInRange(h.kernel, p) {
			g.mu.RUnlock()
			return Result{}, ErrKernelMismatch
		}
		for _, prereq := range p.Prereqs {
			pid := PatchID(prereq)
			if _, exists := g.patches[pid]; !exists {
				g.mu.RUnlock()
				return Result{}, ErrPrereqMissing
			}
			if !base[pid] && !containsID(added, pid) && !g.patchLoadedLocked(h, pid) {
				g.mu.RUnlock()
				return Result{}, ErrPrereqMissing
			}
		}
		if g.conflictWithLoadedLocked(h, p, added) {
			g.mu.RUnlock()
			return Result{}, ErrPatchConflict
		}
		if limitExceeded(g.limits.MaxStackDepth, len(startStack)+len(added)+1) {
			g.mu.RUnlock()
			return Result{}, ErrStackLimit
		}
		added = append(added, id)
		tx.Applied = cloneIDs(added)
		g.mu.RUnlock()

		stepAt := g.now()
		g.mu.Lock()
		if h.pending == tx && tx.StartedAt.IsZero() {
			tx.StartedAt = stepAt
		}
		g.mu.Unlock()
	}

	g.mu.Lock()
	newStack := append(cloneIDs(startStack), added...)
	h.stack = newStack
	h.busy = false
	h.pending = nil
	h.lastError = ""
	pr.consumed = true
	pr.applied = true
	pr.loaded = cloneIDs(added)
	g.mu.Unlock()

	return Result{
		PlanID:  planID,
		Host:    h.name,
		Applied: cloneIDs(added),
		Depth:   len(newStack),
	}, nil
}

// Rollback removes the patches loaded by one plan, newest first.
func (g *LivepatchConsistencyGuard) Rollback(planID string, reason string) error {
	g.ops.RLock()
	defer g.ops.RUnlock()

	g.mu.RLock()
	pr, ok := g.plans[planID]
	if !ok {
		g.mu.RUnlock()
		return ErrUnknownPlan
	}
	if strings.TrimSpace(reason) == "" {
		g.mu.RUnlock()
		return ErrBadRollbackReason
	}
	if !pr.applied {
		g.mu.RUnlock()
		return ErrPlanNotApplied
	}
	h, ok := g.hosts[pr.plan.Host]
	g.mu.RUnlock()
	if !ok {
		return ErrUnknownHost
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	pr, ok = g.plans[planID]
	if !ok {
		return ErrUnknownPlan
	}
	if !pr.applied {
		return ErrPlanNotApplied
	}
	h, ok = g.hosts[pr.plan.Host]
	if !ok {
		return ErrUnknownHost
	}

	remove := make(map[PatchID]bool)
	for _, id := range pr.loaded {
		remove[id] = true
	}

	for changed := true; changed; {
		changed = false
		for _, id := range h.stack {
			if remove[id] {
				continue
			}
			p, exists := g.patches[id]
			if !exists {
				continue
			}
			for _, prereq := range p.Prereqs {
				if remove[PatchID(prereq)] {
					remove[id] = true
					changed = true
					break
				}
			}
		}
	}

	if len(remove) > 0 {
		newStack := make([]PatchID, 0, len(h.stack))
		for i := len(h.stack) - 1; i >= 0; i-- {
			id := h.stack[i]
			if !remove[id] {
				newStack = append(newStack, id)
			}
		}
		for i, j := 0, len(newStack)-1; i < j; i, j = i+1, j-1 {
			newStack[i], newStack[j] = newStack[j], newStack[i]
		}
		h.stack = newStack
	}
	h.lastRollback = reason
	h.lastError = ""
	return nil
}

// Status reads the live state of one host.
func (g *LivepatchConsistencyGuard) Status(host string) (HostState, error) {
	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.RLock()
	defer g.mu.RUnlock()

	h, ok := g.hosts[host]
	if !ok {
		return HostState{}, ErrUnknownHost
	}
	return HostState{
		Host:         h.name,
		Kernel:       h.kernel,
		BuildID:      h.buildID,
		Stack:        cloneIDs(h.stack),
		Busy:         h.busy,
		LastRollback: h.lastRollback,
		LastError:    h.lastError,
	}, nil
}

// Hosts lists the registered hosts in registration order.
func (g *LivepatchConsistencyGuard) Hosts() []string {
	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]string(nil), g.hostOrder...)
}

// PatchInfo reads one registered patch.
func (g *LivepatchConsistencyGuard) PatchInfo(id PatchID) (Patch, bool) {
	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.RLock()
	defer g.mu.RUnlock()
	p, ok := g.patches[id]
	if !ok {
		return Patch{}, false
	}
	return clonePatch(p), true
}

const (
	snapshotFormat  = "livepatch-consistency-guard"
	snapshotVersion = 1
)

type snapshotData struct {
	Format      string         `json:"format"`
	Version     int            `json:"version"`
	NextPlanSeq uint64         `json:"next_plan_seq"`
	Patches     []Patch        `json:"patches"`
	Hosts       []snapshotHost `json:"hosts"`
	Plans       []snapshotPlan `json:"plans"`
}

type snapshotHost struct {
	Name         string               `json:"name"`
	Kernel       string               `json:"kernel"`
	BuildID      string               `json:"build_id"`
	Stack        []PatchID            `json:"stack"`
	Busy         bool                 `json:"busy"`
	LastRollback string               `json:"last_rollback"`
	LastError    string               `json:"last_error"`
	Pending      *snapshotTransaction `json:"pending,omitempty"`
}

type snapshotTransaction struct {
	PlanID     string    `json:"plan_id"`
	StartStack []PatchID `json:"start_stack"`
	Applied    []PatchID `json:"applied"`
	StartedAt  time.Time `json:"started_at"`
}

type snapshotPlan struct {
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	Order    []PatchID `json:"order"`
	Kernel   string    `json:"kernel"`
	Seq      uint64    `json:"seq"`
	Consumed bool      `json:"consumed"`
	Applied  bool      `json:"applied"`
	Loaded   []PatchID `json:"loaded"`
}

// Snapshot serialises patches, hosts, stacks and plans.
func (g *LivepatchConsistencyGuard) Snapshot() ([]byte, error) {
	g.ops.RLock()
	defer g.ops.RUnlock()
	g.mu.RLock()
	defer g.mu.RUnlock()

	snap := snapshotData{
		Format:      snapshotFormat,
		Version:     snapshotVersion,
		NextPlanSeq: g.nextPlan,
		Patches:     make([]Patch, 0, len(g.patchOrder)),
		Hosts:       make([]snapshotHost, 0, len(g.hostOrder)),
		Plans:       make([]snapshotPlan, 0, len(g.planOrder)),
	}
	for _, id := range g.patchOrder {
		snap.Patches = append(snap.Patches, clonePatch(g.patches[id]))
	}
	for _, name := range g.hostOrder {
		h := g.hosts[name]
		sh := snapshotHost{
			Name:         h.name,
			Kernel:       h.kernel,
			BuildID:      h.buildID,
			Stack:        cloneIDs(h.stack),
			Busy:         h.busy,
			LastRollback: h.lastRollback,
			LastError:    h.lastError,
		}
		if h.pending != nil {
			sh.Pending = &snapshotTransaction{
				PlanID:     h.pending.PlanID,
				StartStack: cloneIDs(h.pending.StartStack),
				Applied:    cloneIDs(h.pending.Applied),
				StartedAt:  h.pending.StartedAt,
			}
		}
		snap.Hosts = append(snap.Hosts, sh)
	}
	for _, id := range g.planOrder {
		pr := g.plans[id]
		snap.Plans = append(snap.Plans, snapshotPlan{
			ID:       pr.plan.ID,
			Host:     pr.plan.Host,
			Order:    cloneIDs(pr.plan.Order),
			Kernel:   pr.plan.Kernel,
			Seq:      pr.plan.Seq,
			Consumed: pr.consumed,
			Applied:  pr.applied,
			Loaded:   cloneIDs(pr.loaded),
		})
	}
	return json.Marshal(snap)
}

func badSnapshot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadSnapshot, fmt.Sprintf(format, args...))
}

func validStackRefs(stack []PatchID, patches map[PatchID]Patch) bool {
	seen := make(map[PatchID]bool, len(stack))
	for _, id := range stack {
		if _, ok := patches[id]; !ok || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func stackIsConsistent(host snapshotHost, patches map[PatchID]Patch) bool {
	position := make(map[PatchID]int, len(host.Stack))
	for i, id := range host.Stack {
		position[id] = i
	}
	for i, id := range host.Stack {
		p := patches[id]
		for _, prereq := range p.Prereqs {
			pid := PatchID(prereq)
			pos, ok := position[pid]
			if ok {
				if pos >= i {
					return false
				}
				continue
			}
			q, exists := patches[pid]
			if !exists || q.BuildID == "" || q.BuildID != host.BuildID {
				return false
			}
		}
	}
	for i, id := range host.Stack {
		for j := i + 1; j < len(host.Stack); j++ {
			if patchesConflict(patches[id], patches[host.Stack[j]]) {
				return false
			}
		}
	}
	return true
}

func (g *LivepatchConsistencyGuard) stateFromSnapshot(snap snapshotData) (*LivepatchConsistencyGuard, bool) {
	if snap.Format != snapshotFormat || snap.Version != snapshotVersion {
		return nil, false
	}
	if limitExceeded(g.limits.MaxPatches, len(snap.Patches)) || limitExceeded(g.limits.MaxHosts, len(snap.Hosts)) {
		return nil, false
	}

	next := &LivepatchConsistencyGuard{limits: g.limits, nextPlan: snap.NextPlanSeq}
	next.initLocked()

	for _, p := range snap.Patches {
		if strings.TrimSpace(p.ID) == "" || badVersionRange(p) || containsString(p.Prereqs, p.ID) {
			return nil, false
		}
		id := PatchID(p.ID)
		if _, ok := next.patches[id]; ok {
			return nil, false
		}
		p = clonePatch(p)
		next.patches[id] = p
		next.patchOrder = append(next.patchOrder, id)
	}
	hosts := make(map[string]snapshotHost, len(snap.Hosts))
	for _, sh := range snap.Hosts {
		if strings.TrimSpace(sh.Name) == "" || strings.TrimSpace(sh.Kernel) == "" || strings.TrimSpace(sh.BuildID) == "" {
			return nil, false
		}
		if _, ok := hosts[sh.Name]; ok {
			return nil, false
		}
		if limitExceeded(g.limits.MaxStackDepth, len(sh.Stack)) {
			return nil, false
		}
		if !validStackRefs(sh.Stack, next.patches) || !stackIsConsistent(sh, next.patches) {
			return nil, false
		}
		hosts[sh.Name] = sh
	}

	plans := make(map[string]*planRecord, len(snap.Plans))
	for _, sp := range snap.Plans {
		if strings.TrimSpace(sp.ID) == "" || sp.Seq == 0 {
			return nil, false
		}
		if _, ok := plans[sp.ID]; ok {
			return nil, false
		}
		sh, ok := hosts[sp.Host]
		if !ok || sh.Kernel != sp.Kernel {
			return nil, false
		}
		if !validStackRefs(sp.Order, next.patches) || !validStackRefs(sp.Loaded, next.patches) {
			return nil, false
		}
		loadedSeen := make(map[PatchID]bool, len(sp.Loaded))
		for _, id := range sp.Loaded {
			if loadedSeen[id] || !containsID(sp.Order, id) {
				return nil, false
			}
			loadedSeen[id] = true
		}
		if sp.Applied && !sp.Consumed {
			return nil, false
		}
		if sp.Seq > next.nextPlan {
			next.nextPlan = sp.Seq
		}
		pr := &planRecord{
			plan: Plan{
				ID:     sp.ID,
				Host:   sp.Host,
				Order:  cloneIDs(sp.Order),
				Kernel: sp.Kernel,
				Seq:    sp.Seq,
			},
			consumed: sp.Consumed,
			applied:  sp.Applied,
			loaded:   cloneIDs(sp.Loaded),
		}
		plans[sp.ID] = pr
		next.plans[sp.ID] = pr
		next.planOrder = append(next.planOrder, sp.ID)
	}

	for _, sh := range snap.Hosts {
		h := &hostRecord{
			name:         sh.Name,
			kernel:       sh.Kernel,
			buildID:      sh.BuildID,
			stack:        cloneIDs(sh.Stack),
			busy:         sh.Busy,
			lastRollback: sh.LastRollback,
			lastError:    sh.LastError,
		}
		if sh.Pending != nil {
			if limitExceeded(g.limits.MaxStackDepth, len(sh.Pending.StartStack)) ||
				limitExceeded(g.limits.MaxStackDepth, len(sh.Pending.StartStack)+len(sh.Pending.Applied)) {
				return nil, false
			}
			if !validStackRefs(sh.Pending.StartStack, next.patches) ||
				!validStackRefs(sh.Pending.Applied, next.patches) {
				return nil, false
			}
			pr, ok := plans[sh.Pending.PlanID]
			if !ok || pr.plan.Host != sh.Name || pr.consumed {
				return nil, false
			}
			if len(sh.Pending.Applied) > len(pr.plan.Order) {
				return nil, false
			}
			nextOrderIndex := 0
			for _, id := range sh.Pending.Applied {
				found := false
				for nextOrderIndex < len(pr.plan.Order) {
					candidate := pr.plan.Order[nextOrderIndex]
					nextOrderIndex++
					if candidate == id {
						found = true
						break
					}
				}
				if !found {
					return nil, false
				}
			}
			if !validStackRefs(sh.Pending.StartStack, next.patches) ||
				!stackIsConsistent(snapshotHost{
					Name:    sh.Name,
					Kernel:  sh.Kernel,
					BuildID: sh.BuildID,
					Stack:   sh.Pending.StartStack,
				}, next.patches) {
				return nil, false
			}
			h.stack = cloneIDs(sh.Pending.StartStack)
			h.busy = false
			h.pending = nil
		} else if sh.Busy {
			return nil, false
		}
		next.hosts[sh.Name] = h
		next.hostOrder = append(next.hostOrder, sh.Name)
	}

	return next, true
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *LivepatchConsistencyGuard) Restore(data []byte) error {
	var snap snapshotData
	if err := json.Unmarshal(data, &snap); err != nil {
		return badSnapshot("%v", err)
	}
	next, ok := g.stateFromSnapshot(snap)
	if !ok {
		return badSnapshot("state validation failed")
	}

	g.ops.Lock()
	defer g.ops.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.patches = next.patches
	g.patchOrder = next.patchOrder
	g.hosts = next.hosts
	g.hostOrder = next.hostOrder
	g.plans = next.plans
	g.planOrder = next.planOrder
	g.nextPlan = next.nextPlan
	return nil
}
