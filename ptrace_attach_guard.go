// Package task authorises ptrace attach requests against the Yama scope, the
// parent child relation and a one shot attach ticket.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Ticket states.
const (
	TicketActive  = "active"
	TicketUsed    = "used"
	TicketRevoked = "revoked"
	TicketExpired = "expired"
)

// Attach modes accepted by the guard.
const (
	ModeAttach = "attach"
	ModeSeize  = "seize"
)

// Reasons attached to a scope verdict or an attach result.
const (
	ReasonAllowed        = "allowed"
	ReasonScopeDenied    = "scope_denied"
	ReasonNotParent      = "not_parent"
	ReasonNotRoot        = "not_root"
	ReasonSelfAttach     = "self_attach"
	ReasonUIDMismatch    = "uid_mismatch"
	ReasonAlreadyTraced  = "already_traced"
	ReasonTicketLimit    = "ticket_limit"
	ReasonTicketConsumed = "ticket_consumed"
	ReasonTicketExpired  = "ticket_expired"
	ReasonOwnerMismatch  = "ticket_owner_mismatch"
	ReasonExecReplaced   = "tracee_exec"
)

// Defaults applied when the policy leaves a field unset.
const (
	DefaultMaxTickets  = 32
	DefaultTicketTTLMs = 60000
	DefaultMaxTracked  = 256
)

// Errors reported by the guard.
var (
	ErrBadScope       = errors.New("yama scope is out of range")
	ErrBadProcess     = errors.New("process identity is not usable")
	ErrUnknownProcess = errors.New("process is not tracked")
	ErrTrackedLimit   = errors.New("tracked process table is full")
	ErrScopeDenied    = errors.New("yama scope denies this attach")
	ErrAlreadyTraced  = errors.New("tracee already has a tracer")
	ErrTicketLimit    = errors.New("active ticket budget is exhausted")
	ErrUnknownTicket  = errors.New("unknown attach ticket")
	ErrBadMode        = errors.New("attach mode is not supported")
	ErrNotTraced      = errors.New("process is not being traced")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

const ptraceSnapshotVersion = 1

// ScopePolicy is the Yama configuration plus the guard budgets.
type ScopePolicy struct {
	Scope        int
	AllowSameUID bool
	MaxTickets   int
	TicketTTLMs  int64
	MaxTracked   int
}

// ProcessInfo is one entry of the tracked process table.
type ProcessInfo struct {
	PID      int    `json:"pid"`
	PPID     int    `json:"ppid"`
	UID      int    `json:"uid"`
	Image    string `json:"image"`
	Dumpable bool   `json:"dumpable"`
}

// Ticket authorises exactly one attach between two processes.
type Ticket struct {
	ID        string `json:"id"`
	TracerPID int    `json:"tracer_pid"`
	TraceePID int    `json:"tracee_pid"`
	IssuedAt  int64  `json:"issued_at"`
	ExpiresAt int64  `json:"expires_at"`
	Scope     int    `json:"scope"`
	State     string `json:"state"`
}

// AttachRequest is one attach attempt presented to the guard.
type AttachRequest struct {
	TicketID  string `json:"ticket_id"`
	TracerPID int    `json:"tracer_pid"`
	TraceePID int    `json:"tracee_pid"`
	At        int64  `json:"at"`
	Mode      string `json:"mode"`
}

// AttachResult is the explainable outcome of one attach attempt.
type AttachResult struct {
	TicketID string `json:"ticket_id"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason"`
	Traced   bool   `json:"traced"`
}

// GuardStats counts tickets and attach outcomes.
type GuardStats struct {
	Issued   int `json:"issued"`
	Denied   int `json:"denied"`
	Attached int `json:"attached"`
	Detached int `json:"detached"`
}

// PtraceAttachGuard owns the process table, the tickets and the trace pairs.
type PtraceAttachGuard struct {
	mu        sync.RWMutex
	policy    ScopePolicy
	processes map[int]ProcessInfo
	tickets   map[string]Ticket
	tracers   map[int]int
	execAt    map[int]int64
	stats     GuardStats
	ticketSeq uint64
}

type ptraceSnapshot struct {
	Version   int                 `json:"version"`
	Policy    ScopePolicy         `json:"policy"`
	Processes map[int]ProcessInfo `json:"processes"`
	Tickets   map[string]Ticket   `json:"tickets"`
	Tracers   map[int]int         `json:"tracers"`
	ExecAt    map[int]int64       `json:"exec_at"`
	Stats     GuardStats          `json:"stats"`
	TicketSeq uint64              `json:"ticket_seq"`
}

// NewPtraceAttachGuard builds a guard and fills the unset policy defaults.
func NewPtraceAttachGuard(policy ScopePolicy) *PtraceAttachGuard {
	policy = normalizeScopePolicy(policy)
	return &PtraceAttachGuard{
		policy:    policy,
		processes: make(map[int]ProcessInfo),
		tickets:   make(map[string]Ticket),
		tracers:   make(map[int]int),
		execAt:    make(map[int]int64),
	}
}

// SetPolicy replaces the scope policy after validating it.
func (g *PtraceAttachGuard) SetPolicy(policy ScopePolicy) error {
	if err := validateScope(policy.Scope); err != nil {
		return err
	}
	policy = normalizeScopePolicy(policy)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = policy
	return nil
}

// Track records or refreshes one process in the table.
func (g *PtraceAttachGuard) Track(p ProcessInfo) error {
	if p.PID <= 0 || strings.TrimSpace(p.Image) == "" {
		return fmt.Errorf("%w: pid=%d", ErrBadProcess, p.PID)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.processes[p.PID]; !exists && len(g.processes) >= g.policy.MaxTracked {
		return fmt.Errorf("%w: max=%d", ErrTrackedLimit, g.policy.MaxTracked)
	}
	g.processes[p.PID] = p
	return nil
}

// Process reads one tracked process.
func (g *PtraceAttachGuard) Process(pid int) (ProcessInfo, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	p, ok := g.processes[pid]
	return p, ok
}

// ScopeVerdict grades the Yama scope for one tracer tracee pair.
func (g *PtraceAttachGuard) ScopeVerdict(tracerPID, traceePID int) (string, error) {
	if tracerPID <= 0 || traceePID <= 0 {
		return "", fmt.Errorf("%w: tracer=%d tracee=%d", ErrBadProcess, tracerPID, traceePID)
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	if err := validateScope(g.policy.Scope); err != nil {
		return "", err
	}
	tracer, ok := g.processes[tracerPID]
	if !ok {
		return "", fmt.Errorf("%w: tracer %d", ErrUnknownProcess, tracerPID)
	}
	tracee, ok := g.processes[traceePID]
	if !ok {
		return "", fmt.Errorf("%w: tracee %d", ErrUnknownProcess, traceePID)
	}
	return scopeReason(g.policy, tracer, tracee), nil
}

// RequestTicket issues the single attach ticket for one pair.
func (g *PtraceAttachGuard) RequestTicket(tracerPID, traceePID int, at int64) (Ticket, error) {
	if tracerPID <= 0 || traceePID <= 0 {
		return Ticket{}, fmt.Errorf("%w: tracer=%d tracee=%d", ErrBadProcess, tracerPID, traceePID)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if err := validateScope(g.policy.Scope); err != nil {
		return Ticket{}, err
	}
	tracer, ok := g.processes[tracerPID]
	if !ok {
		return Ticket{}, fmt.Errorf("%w: tracer %d", ErrUnknownProcess, tracerPID)
	}
	tracee, ok := g.processes[traceePID]
	if !ok {
		return Ticket{}, fmt.Errorf("%w: tracee %d", ErrUnknownProcess, traceePID)
	}

	reason := scopeReason(g.policy, tracer, tracee)
	if reason != ReasonAllowed {
		g.stats.Denied++
		return Ticket{}, fmt.Errorf("%w: %s", ErrScopeDenied, reason)
	}

	g.expireTicketsLocked(at)

	activeCount := 0
	activeForTracee := false
	for _, ticket := range g.tickets {
		if ticket.State != TicketActive {
			continue
		}
		activeCount++
		if ticket.TraceePID == traceePID {
			activeForTracee = true
		}
	}

	if currentTracer, traced := g.tracers[traceePID]; traced || activeForTracee {
		g.stats.Denied++
		return Ticket{}, fmt.Errorf("%w: tracee %d current_tracer=%d active_ticket=%v",
			ErrAlreadyTraced, traceePID, currentTracer, activeForTracee)
	}
	if activeCount >= g.policy.MaxTickets {
		g.stats.Denied++
		return Ticket{}, fmt.Errorf("%w: active=%d max=%d", ErrTicketLimit, activeCount, g.policy.MaxTickets)
	}

	g.ticketSeq++
	ticket := Ticket{
		ID:        fmt.Sprintf("t-%016x", g.ticketSeq),
		TracerPID: tracerPID,
		TraceePID: traceePID,
		IssuedAt:  at,
		ExpiresAt: at + g.policy.TicketTTLMs,
		Scope:     g.policy.Scope,
		State:     TicketActive,
	}
	g.tickets[ticket.ID] = ticket
	g.stats.Issued++
	return ticket, nil
}

// Attach consumes a ticket and starts tracing.
func (g *PtraceAttachGuard) Attach(req AttachRequest) (AttachResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ticket, ticketKnown := g.tickets[req.TicketID]

	if req.Mode != ModeAttach && req.Mode != ModeSeize {
		if ticketKnown {
			g.consumeActiveTicketLocked(&ticket)
		}
		return AttachResult{TicketID: req.TicketID, Allowed: false}, fmt.Errorf("%w: %q", ErrBadMode, req.Mode)
	}

	if !ticketKnown {
		return AttachResult{TicketID: req.TicketID, Allowed: false},
			fmt.Errorf("%w: %s", ErrUnknownTicket, req.TicketID)
	}

	denied := func(reason string) (AttachResult, error) {
		g.stats.Denied++
		return AttachResult{TicketID: ticket.ID, Allowed: false, Reason: reason, Traced: false}, nil
	}

	if ticket.TracerPID != req.TracerPID || ticket.TraceePID != req.TraceePID {
		g.consumeActiveTicketLocked(&ticket)
		return denied(ReasonOwnerMismatch)
	}

	tracer, tracerKnown := g.processes[req.TracerPID]
	tracee, traceeKnown := g.processes[req.TraceePID]
	if !tracerKnown || !traceeKnown {
		return AttachResult{TicketID: ticket.ID, Allowed: false},
			fmt.Errorf("%w: tracer=%v tracee=%v", ErrUnknownProcess, tracerKnown, traceeKnown)
	}

	if ticket.State == TicketRevoked || g.execAt[ticket.TraceePID] > ticket.IssuedAt {
		if ticket.State != TicketRevoked {
			ticket.State = TicketRevoked
			g.tickets[ticket.ID] = ticket
		}
		return denied(ReasonExecReplaced)
	}

	if (ticket.State == TicketActive || ticket.State == TicketUsed) && req.At >= ticket.ExpiresAt {
		if ticket.State == TicketActive {
			ticket.State = TicketExpired
			g.tickets[ticket.ID] = ticket
		}
		return denied(ReasonTicketExpired)
	}

	switch ticket.State {
	case TicketUsed:
		return denied(ReasonTicketConsumed)
	case TicketExpired:
		return denied(ReasonTicketExpired)
	case TicketRevoked:
		return denied(ReasonExecReplaced)
	}

	if _, traced := g.tracers[ticket.TraceePID]; traced {
		g.consumeActiveTicketLocked(&ticket)
		return denied(ReasonAlreadyTraced)
	}

	if reason := scopeReason(g.policy, tracer, tracee); reason != ReasonAllowed {
		g.consumeActiveTicketLocked(&ticket)
		return denied(reason)
	}

	ticket.State = TicketUsed
	g.tickets[ticket.ID] = ticket
	g.tracers[ticket.TraceePID] = ticket.TracerPID
	g.stats.Attached++

	return AttachResult{
		TicketID: ticket.ID,
		Allowed:  true,
		Reason:   ReasonAllowed,
		Traced:   true,
	}, nil
}

// Detach stops tracing one pair.
func (g *PtraceAttachGuard) Detach(tracerPID, traceePID int) error {
	if tracerPID <= 0 || traceePID <= 0 {
		return fmt.Errorf("%w: tracer=%d tracee=%d", ErrBadProcess, tracerPID, traceePID)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	currentTracer, traced := g.tracers[traceePID]
	if !traced || currentTracer != tracerPID {
		return fmt.Errorf("%w: %d -> %d", ErrNotTraced, tracerPID, traceePID)
	}
	delete(g.tracers, traceePID)
	g.stats.Detached++
	return nil
}

// MarkExec records that a traced process replaced its image.
func (g *PtraceAttachGuard) MarkExec(pid int, newImage string, at int64) error {
	if pid <= 0 || strings.TrimSpace(newImage) == "" {
		return fmt.Errorf("%w: pid=%d", ErrBadProcess, pid)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	process, ok := g.processes[pid]
	if !ok {
		return fmt.Errorf("%w: pid %d", ErrUnknownProcess, pid)
	}
	process.Image = newImage
	g.processes[pid] = process
	g.execAt[pid] = at

	for id, ticket := range g.tickets {
		if ticket.TraceePID != pid {
			continue
		}
		if ticket.State == TicketExpired {
			continue
		}
		ticket.State = TicketRevoked
		g.tickets[id] = ticket
	}
	return nil
}

// Ticket reads one ticket.
func (g *PtraceAttachGuard) Ticket(id string) (Ticket, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ticket, ok := g.tickets[id]
	return ticket, ok
}

// Tickets lists every ticket by id in ascending order.
func (g *PtraceAttachGuard) Tickets() []Ticket {
	g.mu.RLock()
	defer g.mu.RUnlock()

	ids := make([]string, 0, len(g.tickets))
	for id := range g.tickets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tickets := make([]Ticket, 0, len(ids))
	for _, id := range ids {
		tickets = append(tickets, g.tickets[id])
	}
	return tickets
}

// TracerOf reports who currently traces one process.
func (g *PtraceAttachGuard) TracerOf(traceePID int) (int, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	tracer, ok := g.tracers[traceePID]
	return tracer, ok
}

// Stats returns the guard counters.
func (g *PtraceAttachGuard) Stats() GuardStats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.stats
}

// Snapshot serialises the process table, the tickets and the counters.
func (g *PtraceAttachGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	snapshot := ptraceSnapshot{
		Version:   ptraceSnapshotVersion,
		Policy:    g.policy,
		Processes: cloneProcesses(g.processes),
		Tickets:   cloneTickets(g.tickets),
		Tracers:   cloneTracers(g.tracers),
		ExecAt:    cloneExecAt(g.execAt),
		Stats:     g.stats,
		TicketSeq: g.ticketSeq,
	}
	g.mu.RUnlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal ptrace guard snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PtraceAttachGuard) Restore(data []byte) error {
	var snapshot ptraceSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = snapshot.Policy
	g.processes = snapshot.Processes
	g.tickets = snapshot.Tickets
	g.tracers = snapshot.Tracers
	g.execAt = snapshot.ExecAt
	g.stats = snapshot.Stats
	g.ticketSeq = snapshot.TicketSeq
	return nil
}

func normalizeScopePolicy(policy ScopePolicy) ScopePolicy {
	if policy.MaxTickets <= 0 {
		policy.MaxTickets = DefaultMaxTickets
	}
	if policy.TicketTTLMs <= 0 {
		policy.TicketTTLMs = DefaultTicketTTLMs
	}
	if policy.MaxTracked <= 0 {
		policy.MaxTracked = DefaultMaxTracked
	}
	return policy
}

func validateScope(scope int) error {
	if scope < 0 || scope > 3 {
		return fmt.Errorf("%w: %d", ErrBadScope, scope)
	}
	return nil
}

func scopeReason(policy ScopePolicy, tracer, tracee ProcessInfo) string {
	if tracer.PID == tracee.PID {
		return ReasonSelfAttach
	}

	switch policy.Scope {
	case 0:
		if policy.AllowSameUID && tracer.UID == tracee.UID {
			return ReasonAllowed
		}
		return ReasonUIDMismatch
	case 1:
		if tracee.PPID == tracer.PID {
			return ReasonAllowed
		}
		return ReasonNotParent
	case 2:
		if tracer.UID == 0 {
			return ReasonAllowed
		}
		return ReasonNotRoot
	case 3:
		return ReasonScopeDenied
	default:
		return ReasonScopeDenied
	}
}

func (g *PtraceAttachGuard) expireTicketsLocked(at int64) {
	for id, ticket := range g.tickets {
		if ticket.State == TicketActive && at >= ticket.ExpiresAt {
			ticket.State = TicketExpired
			g.tickets[id] = ticket
		}
	}
}

func (g *PtraceAttachGuard) consumeActiveTicketLocked(ticket *Ticket) {
	if ticket.State != TicketActive {
		return
	}
	ticket.State = TicketUsed
	g.tickets[ticket.ID] = *ticket
}

func cloneProcesses(src map[int]ProcessInfo) map[int]ProcessInfo {
	dst := make(map[int]ProcessInfo, len(src))
	for pid, process := range src {
		dst[pid] = process
	}
	return dst
}

func cloneTickets(src map[string]Ticket) map[string]Ticket {
	dst := make(map[string]Ticket, len(src))
	for id, ticket := range src {
		dst[id] = ticket
	}
	return dst
}

func cloneTracers(src map[int]int) map[int]int {
	dst := make(map[int]int, len(src))
	for tracee, tracer := range src {
		dst[tracee] = tracer
	}
	return dst
}

func cloneExecAt(src map[int]int64) map[int]int64 {
	dst := make(map[int]int64, len(src))
	for pid, at := range src {
		dst[pid] = at
	}
	return dst
}

func validateSnapshot(snapshot *ptraceSnapshot) error {
	if snapshot.Version != ptraceSnapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}
	if err := validateScope(snapshot.Policy.Scope); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	snapshot.Policy = normalizeScopePolicy(snapshot.Policy)

	if snapshot.Processes == nil {
		snapshot.Processes = make(map[int]ProcessInfo)
	}
	if snapshot.Tickets == nil {
		snapshot.Tickets = make(map[string]Ticket)
	}
	if snapshot.Tracers == nil {
		snapshot.Tracers = make(map[int]int)
	}
	if snapshot.ExecAt == nil {
		snapshot.ExecAt = make(map[int]int64)
	}

	for pid, process := range snapshot.Processes {
		if pid <= 0 || process.PID != pid || strings.TrimSpace(process.Image) == "" {
			return fmt.Errorf("%w: invalid process %d", ErrBadSnapshot, pid)
		}
	}
	if len(snapshot.Processes) > snapshot.Policy.MaxTracked {
		return fmt.Errorf("%w: process count %d exceeds %d",
			ErrBadSnapshot, len(snapshot.Processes), snapshot.Policy.MaxTracked)
	}

	activeTickets := 0
	for id, ticket := range snapshot.Tickets {
		if id == "" || ticket.ID != id {
			return fmt.Errorf("%w: ticket id mismatch %q", ErrBadSnapshot, id)
		}
		if ticket.TracerPID <= 0 || ticket.TraceePID <= 0 {
			return fmt.Errorf("%w: ticket %s has invalid pid", ErrBadSnapshot, id)
		}
		if _, ok := snapshot.Processes[ticket.TracerPID]; !ok {
			return fmt.Errorf("%w: ticket %s has unknown tracer", ErrBadSnapshot, id)
		}
		if _, ok := snapshot.Processes[ticket.TraceePID]; !ok {
			return fmt.Errorf("%w: ticket %s has unknown tracee", ErrBadSnapshot, id)
		}
		if ticket.IssuedAt < 0 || ticket.ExpiresAt < ticket.IssuedAt {
			return fmt.Errorf("%w: ticket %s has invalid time window", ErrBadSnapshot, id)
		}
		if err := validateScope(ticket.Scope); err != nil {
			return fmt.Errorf("%w: ticket %s: %v", ErrBadSnapshot, id, err)
		}
		switch ticket.State {
		case TicketActive:
			activeTickets++
		case TicketUsed, TicketRevoked, TicketExpired:
		default:
			return fmt.Errorf("%w: ticket %s has state %q", ErrBadSnapshot, id, ticket.State)
		}
	}
	if activeTickets > snapshot.Policy.MaxTickets {
		return fmt.Errorf("%w: active tickets %d exceeds %d",
			ErrBadSnapshot, activeTickets, snapshot.Policy.MaxTickets)
	}

	for traceePID, tracerPID := range snapshot.Tracers {
		if traceePID <= 0 || tracerPID <= 0 {
			return fmt.Errorf("%w: invalid trace pair %d -> %d", ErrBadSnapshot, tracerPID, traceePID)
		}
		if _, ok := snapshot.Processes[traceePID]; !ok {
			return fmt.Errorf("%w: tracee %d is unknown", ErrBadSnapshot, traceePID)
		}
		if _, ok := snapshot.Processes[tracerPID]; !ok {
			return fmt.Errorf("%w: tracer %d is unknown", ErrBadSnapshot, tracerPID)
		}
	}
	for pid := range snapshot.ExecAt {
		if _, ok := snapshot.Processes[pid]; !ok {
			return fmt.Errorf("%w: exec marker for unknown process %d", ErrBadSnapshot, pid)
		}
	}

	if snapshot.Stats.Issued < 0 || snapshot.Stats.Denied < 0 ||
		snapshot.Stats.Attached < 0 || snapshot.Stats.Detached < 0 {
		return fmt.Errorf("%w: negative counters", ErrBadSnapshot)
	}
	if snapshot.Stats.Issued != len(snapshot.Tickets) {
		return fmt.Errorf("%w: issued=%d tickets=%d",
			ErrBadSnapshot, snapshot.Stats.Issued, len(snapshot.Tickets))
	}
	if snapshot.TicketSeq != uint64(snapshot.Stats.Issued) {
		return fmt.Errorf("%w: ticket sequence %d does not match issued %d",
			ErrBadSnapshot, snapshot.TicketSeq, snapshot.Stats.Issued)
	}
	if snapshot.Stats.Attached < snapshot.Stats.Detached {
		return fmt.Errorf("%w: detached exceeds attached", ErrBadSnapshot)
	}
	if currentTracers := snapshot.Stats.Attached - snapshot.Stats.Detached; currentTracers != len(snapshot.Tracers) {
		return fmt.Errorf("%w: trace count %d does not match attached-detached %d",
			ErrBadSnapshot, len(snapshot.Tracers), currentTracers)
	}
	return nil
}
