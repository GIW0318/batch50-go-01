// Package task admits eBPF programs through an instruction whitelist, a map
// access check, attach target validation and a per tenant load quota.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Program lifecycle states kept in the gate table.
const (
	ProgLoaded   = "loaded"
	ProgAttached = "attached"
)

// Defaults applied when the gate policy leaves a field unset.
const (
	DefaultMaxPrograms      = 16
	DefaultMaxInsns         = 4096
	DefaultMaxMapsPerProg   = 8
	DefaultMaxAttachTargets = 4
	DefaultRequiredLicense  = "GPL"
)

// Errors reported by the gate.
var (
	ErrBlankTenant       = errors.New("tenant is required")
	ErrBlankProgramName  = errors.New("program name is required")
	ErrEmptyProgram      = errors.New("program carries no instruction")
	ErrBlankMapName      = errors.New("map name is required")
	ErrBadMapSpec        = errors.New("map spec is not usable")
	ErrUnknownMap        = errors.New("map is not registered")
	ErrMapReadOnly       = errors.New("map is read only for this program")
	ErrMapTenantMismatch = errors.New("map belongs to another tenant")
	ErrOpNotAllowed      = errors.New("instruction op is not whitelisted")
	ErrProgTypeDenied    = errors.New("program type is not allowed")
	ErrProgramTooLarge   = errors.New("program exceeds the instruction budget")
	ErrTooManyMaps       = errors.New("program references too many maps")
	ErrLicenseRequired   = errors.New("program license is not accepted")
	ErrQuotaExceeded     = errors.New("tenant load quota is exhausted")
	ErrUnknownHandle     = errors.New("unknown program handle")
	ErrBadAttachTarget   = errors.New("attach target is incomplete")
	ErrAttachLimit       = errors.New("program reached its attach target budget")
	ErrStillAttached     = errors.New("program is still attached")
	ErrNotAttached       = errors.New("program has no attach target")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Instruction is one verified eBPF instruction.
type Instruction struct {
	Op  string `json:"op"`
	Imm int64  `json:"imm"`
	Map string `json:"map"`
}

// Program is the object handed to the loader.
type Program struct {
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Insns         []Instruction  `json:"insns"`
	Maps          []string       `json:"maps"`
	License       string         `json:"license"`
	AttachTargets []AttachTarget `json:"attach_targets,omitempty"`
	Targets       []AttachTarget `json:"targets,omitempty"`
}

// MapSpec is one entry of the map registry.
type MapSpec struct {
	Name       string `json:"name"`
	Tenant     string `json:"tenant"`
	MaxEntries int    `json:"max_entries"`
	Writable   bool   `json:"writable"`
}

// AttachTarget is where the program wants to hook in.
type AttachTarget struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	IfIndex int    `json:"if_index"`
}

// GatePolicy bounds the gate tables.
type GatePolicy struct {
	AllowedOps       []string
	AllowedProgTypes []string
	MaxPrograms      int
	MaxInsns         int
	MaxMapsPerProg   int
	MaxAttachTargets int
	RequiredLicense  string
}

// Handle is the gate record for one loaded program.
type Handle struct {
	ID          string   `json:"id"`
	Tenant      string   `json:"tenant"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Fingerprint string   `json:"fingerprint"`
	State       string   `json:"state"`
	Targets     []string `json:"targets"`
}

// GateStats counts what the gate let through and what it turned away.
type GateStats struct {
	Loaded   int `json:"loaded"`
	Attached int `json:"attached"`
	Detached int `json:"detached"`
	Unloaded int `json:"unloaded"`
	Rejected int `json:"rejected"`
}

// EbpfLoadGate owns the map registry, the handle table and the quota counters.
type EbpfLoadGate struct {
	mu        sync.RWMutex
	policy    GatePolicy
	allowedOp map[string]struct{}
	allowedTy map[string]struct{}
	maps      map[string]MapSpec
	handles   map[string]*handleRecord
	byFinger  map[fingerprintKey]string
	stats     GateStats
	nextID    uint64
}

type fingerprintKey struct {
	tenant      string
	fingerprint string
}

type handleRecord struct {
	handle  Handle
	targets []AttachTarget
}

// NewEbpfLoadGate builds a gate and fills the unset policy defaults.
func NewEbpfLoadGate(policy GatePolicy) *EbpfLoadGate {
	policy = normalizePolicy(policy)
	allowedOp := make(map[string]struct{}, len(policy.AllowedOps))
	for _, op := range policy.AllowedOps {
		allowedOp[op] = struct{}{}
	}
	allowedTy := make(map[string]struct{}, len(policy.AllowedProgTypes))
	for _, typ := range policy.AllowedProgTypes {
		allowedTy[typ] = struct{}{}
	}
	return &EbpfLoadGate{
		policy:    policy,
		allowedOp: allowedOp,
		allowedTy: allowedTy,
		maps:      make(map[string]MapSpec),
		handles:   make(map[string]*handleRecord),
		byFinger:  make(map[fingerprintKey]string),
	}
}

// RegisterMap records one map so programs may reference it.
func (g *EbpfLoadGate) RegisterMap(spec MapSpec) error {
	if isBlank(spec.Name) {
		return ErrBlankMapName
	}
	if isBlank(spec.Tenant) {
		return ErrBlankTenant
	}
	if spec.MaxEntries <= 0 {
		return ErrBadMapSpec
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	if old, ok := g.maps[spec.Name]; ok {
		if old == spec {
			return nil
		}
		return ErrBadMapSpec
	}
	g.maps[spec.Name] = spec
	return nil
}

// CheckMapAccess decides whether a tenant may reach one map.
func (g *EbpfLoadGate) CheckMapAccess(tenant, name string, write bool) error {
	if isBlank(tenant) {
		return ErrBlankTenant
	}
	if isBlank(name) {
		return ErrBlankMapName
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.checkMapAccessLocked(tenant, name, write)
}

// CheckInstructions walks the program body against the op whitelist.
func (g *EbpfLoadGate) CheckInstructions(prog Program) error {
	return g.checkInstructions(prog)
}

// Fingerprint derives the stable identity of a program body.
func (g *EbpfLoadGate) Fingerprint(prog Program) string {
	return fingerprintProgram(prog)
}

// Quota reports the programs a tenant currently holds.
func (g *EbpfLoadGate) Quota(tenant string) (used int, limit int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, rec := range g.handles {
		if rec.handle.Tenant == tenant {
			used++
		}
	}
	return used, g.policy.MaxPrograms
}

// Load runs every pre-flight check and records the program handle.
func (g *EbpfLoadGate) Load(tenant string, prog Program) (Handle, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	if isBlank(tenant) {
		return g.rejectLoadLocked(ErrBlankTenant)
	}
	if err := g.checkInstructions(prog); err != nil {
		return g.rejectLoadLocked(err)
	}
	if _, ok := g.allowedTy[prog.Type]; !ok {
		return g.rejectLoadLocked(ErrProgTypeDenied)
	}
	if prog.License != g.policy.RequiredLicense {
		return g.rejectLoadLocked(ErrLicenseRequired)
	}
	if err := g.checkProgramMapsLocked(tenant, prog); err != nil {
		return g.rejectLoadLocked(err)
	}
	intended, err := intendedTargets(prog)
	if err != nil {
		return g.rejectLoadLocked(err)
	}
	if err := g.checkIntendedTargetsLocked(tenant, prog.Type, intended); err != nil {
		return g.rejectLoadLocked(err)
	}

	fingerprint := fingerprintProgram(prog)
	key := fingerprintKey{tenant: tenant, fingerprint: fingerprint}
	if id, ok := g.byFinger[key]; ok {
		if rec, exists := g.handles[id]; exists {
			return copyHandle(rec.handle), nil
		}
		delete(g.byFinger, key)
	}
	if g.tenantUsedLocked(tenant) >= g.policy.MaxPrograms {
		return g.rejectLoadLocked(ErrQuotaExceeded)
	}

	id := g.nextHandleIDLocked()
	rec := &handleRecord{handle: Handle{
		ID:          id,
		Tenant:      tenant,
		Name:        prog.Name,
		Type:        prog.Type,
		Fingerprint: fingerprint,
		State:       ProgLoaded,
	}}
	if len(intended) > 0 {
		rec.targets = append([]AttachTarget(nil), intended...)
		rec.handle.Targets = targetNames(rec.targets)
		rec.handle.State = ProgAttached
		g.stats.Attached += len(rec.targets)
	}
	g.handles[id] = rec
	g.byFinger[key] = id
	g.stats.Loaded++
	return copyHandle(rec.handle), nil
}

// Attach binds a loaded program to one target.
func (g *EbpfLoadGate) Attach(handleID string, target AttachTarget) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	rec, ok := g.handles[handleID]
	if !ok {
		return ErrUnknownHandle
	}
	if isBlank(target.Name) || isBlank(target.Kind) || target.Kind != rec.handle.Type {
		return ErrBadAttachTarget
	}
	for _, existing := range rec.targets {
		if existing == target {
			return nil
		}
	}
	if len(rec.targets) >= g.policy.MaxAttachTargets ||
		g.tenantTargetsLocked(rec.handle.Tenant)+1 > g.tenantAttachLimit() {
		return ErrAttachLimit
	}

	rec.targets = append(rec.targets, target)
	rec.handle.Targets = targetNames(rec.targets)
	rec.handle.State = ProgAttached
	g.stats.Attached++
	return nil
}

// Detach releases every target of one program.
func (g *EbpfLoadGate) Detach(handleID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	rec, ok := g.handles[handleID]
	if !ok {
		return ErrUnknownHandle
	}
	if len(rec.targets) == 0 {
		return ErrNotAttached
	}
	rec.targets = nil
	rec.handle.Targets = nil
	rec.handle.State = ProgLoaded
	g.stats.Detached++
	return nil
}

// Unload drops the handle and returns its quota slot.
func (g *EbpfLoadGate) Unload(handleID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	rec, ok := g.handles[handleID]
	if !ok {
		return ErrUnknownHandle
	}
	if len(rec.targets) > 0 {
		return ErrStillAttached
	}
	delete(g.handles, handleID)
	delete(g.byFinger, fingerprintKey{tenant: rec.handle.Tenant, fingerprint: rec.handle.Fingerprint})
	g.stats.Unloaded++
	return nil
}

// Handle reads one gate record.
func (g *EbpfLoadGate) Handle(id string) (Handle, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rec, ok := g.handles[id]
	if !ok {
		return Handle{}, false
	}
	return copyHandle(rec.handle), true
}

// Attached lists the attached programs of one tenant by handle id.
func (g *EbpfLoadGate) Attached(tenant string) []Handle {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make([]string, 0)
	for id, rec := range g.handles {
		if rec.handle.Tenant == tenant && rec.handle.State == ProgAttached && len(rec.targets) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]Handle, 0, len(ids))
	for _, id := range ids {
		out = append(out, copyHandle(g.handles[id].handle))
	}
	return out
}

// Stats returns the gate counters.
func (g *EbpfLoadGate) Stats() GateStats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.stats
}

// Snapshot serialises the handle table, the map registry and the counters.
func (g *EbpfLoadGate) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	state := snapshotState{
		Version:    snapshotVersion,
		NextHandle: g.nextID,
		Maps:       make([]MapSpec, 0, len(g.maps)),
		Handles:    make([]snapshotHandle, 0, len(g.handles)),
		Stats:      g.stats,
	}
	for _, spec := range g.maps {
		state.Maps = append(state.Maps, spec)
	}
	sort.Slice(state.Maps, func(i, j int) bool { return state.Maps[i].Name < state.Maps[j].Name })
	for _, rec := range g.handles {
		state.Handles = append(state.Handles, snapshotHandle{
			Handle:        copyHandle(rec.handle),
			AttachTargets: append([]AttachTarget(nil), rec.targets...),
		})
	}
	sort.Slice(state.Handles, func(i, j int) bool { return state.Handles[i].ID < state.Handles[j].ID })

	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	doc := snapshotDocument{State: state, Checksum: hex.EncodeToString(sum[:])}
	return json.Marshal(doc)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *EbpfLoadGate) Restore(data []byte) error {
	var doc snapshotDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return ErrBadSnapshot
	}
	if doc.State.Version != snapshotVersion || doc.Checksum == "" {
		return ErrBadSnapshot
	}
	payload, err := json.Marshal(doc.State)
	if err != nil {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(payload)
	if doc.Checksum != hex.EncodeToString(sum[:]) {
		return ErrBadSnapshot
	}

	restored, err := g.validateSnapshot(doc.State)
	if err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.maps = restored.maps
	g.handles = restored.handles
	g.byFinger = restored.byFinger
	g.stats = mergeStats(g.stats, restored.stats)
	g.nextID = restored.nextID
	return nil
}

const snapshotVersion = 1

type snapshotState struct {
	Version    int              `json:"version"`
	NextHandle uint64           `json:"next_handle"`
	Maps       []MapSpec        `json:"maps"`
	Handles    []snapshotHandle `json:"handles"`
	Stats      GateStats        `json:"stats"`
}

type snapshotDocument struct {
	State    snapshotState `json:"state"`
	Checksum string        `json:"checksum"`
}

type snapshotHandle struct {
	Handle        `json:",inline"`
	AttachTargets []AttachTarget `json:"attach_targets"`
}

type validatedSnapshot struct {
	maps     map[string]MapSpec
	handles  map[string]*handleRecord
	byFinger map[fingerprintKey]string
	stats    GateStats
	nextID   uint64
}

func normalizePolicy(policy GatePolicy) GatePolicy {
	policy.AllowedOps = append([]string(nil), policy.AllowedOps...)
	policy.AllowedProgTypes = append([]string(nil), policy.AllowedProgTypes...)
	if policy.MaxPrograms <= 0 {
		policy.MaxPrograms = DefaultMaxPrograms
	}
	if policy.MaxInsns <= 0 {
		policy.MaxInsns = DefaultMaxInsns
	}
	if policy.MaxMapsPerProg <= 0 {
		policy.MaxMapsPerProg = DefaultMaxMapsPerProg
	}
	if policy.MaxAttachTargets <= 0 {
		policy.MaxAttachTargets = DefaultMaxAttachTargets
	}
	if policy.RequiredLicense == "" {
		policy.RequiredLicense = DefaultRequiredLicense
	}
	return policy
}

func (g *EbpfLoadGate) initLocked() {
	if g.maps == nil {
		g.maps = make(map[string]MapSpec)
	}
	if g.handles == nil {
		g.handles = make(map[string]*handleRecord)
	}
	if g.byFinger == nil {
		g.byFinger = make(map[fingerprintKey]string)
	}
}

func (g *EbpfLoadGate) checkInstructions(prog Program) error {
	if isBlank(prog.Name) {
		return ErrBlankProgramName
	}
	if len(prog.Insns) == 0 {
		return ErrEmptyProgram
	}
	if len(prog.Insns) > g.policy.MaxInsns {
		return ErrProgramTooLarge
	}
	for _, insn := range prog.Insns {
		if _, ok := g.allowedOp[insn.Op]; !ok {
			return ErrOpNotAllowed
		}
	}
	return nil
}

func (g *EbpfLoadGate) checkMapAccessLocked(tenant, name string, write bool) error {
	spec, ok := g.maps[name]
	if !ok {
		return ErrUnknownMap
	}
	if spec.Tenant != tenant {
		return ErrMapTenantMismatch
	}
	if write && !spec.Writable {
		return ErrMapReadOnly
	}
	return nil
}

func (g *EbpfLoadGate) checkProgramMapsLocked(tenant string, prog Program) error {
	if len(prog.Maps) > g.policy.MaxMapsPerProg {
		return ErrTooManyMaps
	}
	refs := make([]string, 0, len(prog.Maps)+len(prog.Insns))
	seen := make(map[string]struct{}, len(prog.Maps)+len(prog.Insns))
	refCount := len(prog.Maps)
	for _, name := range prog.Maps {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			refs = append(refs, name)
		}
	}
	for _, insn := range prog.Insns {
		if insn.Map == "" {
			continue
		}
		if _, declared := seen[insn.Map]; !declared {
			refCount++
			if refCount > g.policy.MaxMapsPerProg {
				return ErrTooManyMaps
			}
			seen[insn.Map] = struct{}{}
			refs = append(refs, insn.Map)
		}
	}
	for _, name := range refs {
		if err := g.checkMapAccessLocked(tenant, name, false); err != nil {
			return err
		}
	}
	return nil
}

func intendedTargets(prog Program) ([]AttachTarget, error) {
	out := make([]AttachTarget, 0, len(prog.AttachTargets)+len(prog.Targets))
	seen := make(map[AttachTarget]struct{}, len(prog.AttachTargets)+len(prog.Targets))
	for _, target := range append(append([]AttachTarget(nil), prog.AttachTargets...), prog.Targets...) {
		if isBlank(target.Name) || isBlank(target.Kind) {
			return nil, ErrBadAttachTarget
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out, nil
}

func (g *EbpfLoadGate) checkIntendedTargetsLocked(tenant, progType string, targets []AttachTarget) error {
	if len(targets) > g.policy.MaxAttachTargets {
		return ErrAttachLimit
	}
	for _, target := range targets {
		if target.Kind != progType {
			return ErrBadAttachTarget
		}
	}
	if g.tenantTargetsLocked(tenant)+len(targets) > g.tenantAttachLimit() {
		return ErrAttachLimit
	}
	return nil
}

func (g *EbpfLoadGate) rejectLoadLocked(err error) (Handle, error) {
	g.stats.Rejected++
	return Handle{}, err
}

func (g *EbpfLoadGate) tenantUsedLocked(tenant string) int {
	used := 0
	for _, rec := range g.handles {
		if rec.handle.Tenant == tenant {
			used++
		}
	}
	return used
}

func (g *EbpfLoadGate) tenantTargetsLocked(tenant string) int {
	used := 0
	for _, rec := range g.handles {
		if rec.handle.Tenant == tenant {
			used += len(rec.targets)
		}
	}
	return used
}

func (g *EbpfLoadGate) tenantAttachLimit() int {
	if g.policy.MaxPrograms > 0 && g.policy.MaxAttachTargets > 0 &&
		g.policy.MaxPrograms <= int(^uint(0)>>1)/g.policy.MaxAttachTargets {
		return g.policy.MaxPrograms * g.policy.MaxAttachTargets
	}
	return int(^uint(0) >> 1)
}

func needsNewHandle(id string) bool {
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "h-"), 10, 64)
	return err == nil && n > 0 && strings.HasPrefix(id, "h-")
}

func (g *EbpfLoadGate) nextHandleIDLocked() string {
	for {
		g.nextID++
		id := fmt.Sprintf("h-%d", g.nextID)
		if _, exists := g.handles[id]; !exists {
			return id
		}
	}
}

func fingerprintProgram(prog Program) string {
	payload, err := json.Marshal(prog)
	if err != nil {
		payload = []byte(fmt.Sprintf("%#v", prog))
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func copyHandle(h Handle) Handle {
	h.Targets = append([]string(nil), h.Targets...)
	return h
}

func targetNames(targets []AttachTarget) []string {
	names := make([]string, len(targets))
	for i, target := range targets {
		names[i] = target.Name
	}
	return names
}

func mergeStats(current, restored GateStats) GateStats {
	if restored.Loaded > current.Loaded {
		current.Loaded = restored.Loaded
	}
	if restored.Attached > current.Attached {
		current.Attached = restored.Attached
	}
	if restored.Detached > current.Detached {
		current.Detached = restored.Detached
	}
	if restored.Unloaded > current.Unloaded {
		current.Unloaded = restored.Unloaded
	}
	if restored.Rejected > current.Rejected {
		current.Rejected = restored.Rejected
	}
	return current
}

func isBlank(s string) bool {
	return strings.TrimSpace(s) == ""
}

func (g *EbpfLoadGate) validateSnapshot(state snapshotState) (*validatedSnapshot, error) {
	if state.Version != snapshotVersion || state.Stats.Loaded < 0 || state.Stats.Attached < 0 ||
		state.Stats.Detached < 0 || state.Stats.Unloaded < 0 || state.Stats.Rejected < 0 {
		return nil, ErrBadSnapshot
	}
	maps := make(map[string]MapSpec, len(state.Maps))
	for _, spec := range state.Maps {
		if isBlank(spec.Name) || isBlank(spec.Tenant) || spec.MaxEntries <= 0 {
			return nil, ErrBadSnapshot
		}
		if _, exists := maps[spec.Name]; exists {
			return nil, ErrBadSnapshot
		}
		maps[spec.Name] = spec
	}

	handles := make(map[string]*handleRecord, len(state.Handles))
	byFinger := make(map[fingerprintKey]string, len(state.Handles))
	tenantCount := make(map[string]int)
	tenantTargets := make(map[string]int)
	var maxID uint64
	for _, snap := range state.Handles {
		h := snap.Handle
		if isBlank(h.ID) || isBlank(h.Tenant) || isBlank(h.Name) || isBlank(h.Type) ||
			isBlank(h.Fingerprint) || (h.State != ProgLoaded && h.State != ProgAttached) {
			return nil, ErrBadSnapshot
		}
		if _, exists := handles[h.ID]; exists {
			return nil, ErrBadSnapshot
		}
		if _, ok := g.allowedTy[h.Type]; !ok {
			return nil, ErrBadSnapshot
		}
		if len(snap.AttachTargets) > g.policy.MaxAttachTargets {
			return nil, ErrBadSnapshot
		}
		if h.State == ProgAttached && len(snap.AttachTargets) == 0 {
			return nil, ErrBadSnapshot
		}
		if h.State == ProgLoaded && len(snap.AttachTargets) > 0 {
			return nil, ErrBadSnapshot
		}
		if len(h.Targets) != len(snap.AttachTargets) {
			return nil, ErrBadSnapshot
		}
		seenTargets := make(map[AttachTarget]struct{}, len(snap.AttachTargets))
		for i, target := range snap.AttachTargets {
			if isBlank(target.Name) || isBlank(target.Kind) || target.Kind != h.Type || h.Targets[i] != target.Name {
				return nil, ErrBadSnapshot
			}
			if _, exists := seenTargets[target]; exists {
				return nil, ErrBadSnapshot
			}
			seenTargets[target] = struct{}{}
		}
		key := fingerprintKey{tenant: h.Tenant, fingerprint: h.Fingerprint}
		if _, exists := byFinger[key]; exists {
			return nil, ErrBadSnapshot
		}
		tenantCount[h.Tenant]++
		if tenantCount[h.Tenant] > g.policy.MaxPrograms {
			return nil, ErrBadSnapshot
		}
		tenantTargets[h.Tenant] += len(snap.AttachTargets)
		if tenantTargets[h.Tenant] > g.tenantAttachLimit() {
			return nil, ErrBadSnapshot
		}
		if needsNewHandle(h.ID) {
			n, _ := strconv.ParseUint(strings.TrimPrefix(h.ID, "h-"), 10, 64)
			if n > maxID {
				maxID = n
			}
		}
		rec := &handleRecord{
			handle:  copyHandle(h),
			targets: append([]AttachTarget(nil), snap.AttachTargets...),
		}
		handles[h.ID] = rec
		byFinger[key] = h.ID
	}
	if state.NextHandle < maxID {
		return nil, ErrBadSnapshot
	}
	return &validatedSnapshot{
		maps:     maps,
		handles:  handles,
		byFinger: byFinger,
		stats:    state.Stats,
		nextID:   state.NextHandle,
	}, nil
}
