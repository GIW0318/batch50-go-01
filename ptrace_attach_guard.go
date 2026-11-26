// Package task authorises ptrace attach requests against the Yama scope, the
// parent child relation and a one shot attach ticket.
package task

import "errors"

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

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
	policy ScopePolicy
}

// NewPtraceAttachGuard builds a guard and fills the unset policy defaults.
func NewPtraceAttachGuard(policy ScopePolicy) *PtraceAttachGuard {
	return &PtraceAttachGuard{policy: policy}
}

// SetPolicy replaces the scope policy after validating it.
func (g *PtraceAttachGuard) SetPolicy(policy ScopePolicy) error {
	return errNotImplemented
}

// Track records or refreshes one process in the table.
func (g *PtraceAttachGuard) Track(p ProcessInfo) error {
	return errNotImplemented
}

// Process reads one tracked process.
func (g *PtraceAttachGuard) Process(pid int) (ProcessInfo, bool) {
	return ProcessInfo{}, false
}

// ScopeVerdict grades the Yama scope for one tracer tracee pair.
func (g *PtraceAttachGuard) ScopeVerdict(tracerPID, traceePID int) (string, error) {
	return "", errNotImplemented
}

// RequestTicket issues the single attach ticket for one pair.
func (g *PtraceAttachGuard) RequestTicket(tracerPID, traceePID int, at int64) (Ticket, error) {
	return Ticket{}, errNotImplemented
}

// Attach consumes a ticket and starts tracing.
func (g *PtraceAttachGuard) Attach(req AttachRequest) (AttachResult, error) {
	return AttachResult{}, errNotImplemented
}

// Detach stops tracing one pair.
func (g *PtraceAttachGuard) Detach(tracerPID, traceePID int) error {
	return errNotImplemented
}

// MarkExec records that a traced process replaced its image.
func (g *PtraceAttachGuard) MarkExec(pid int, newImage string, at int64) error {
	return errNotImplemented
}

// Ticket reads one ticket.
func (g *PtraceAttachGuard) Ticket(id string) (Ticket, bool) {
	return Ticket{}, false
}

// Tickets lists every ticket by id in ascending order.
func (g *PtraceAttachGuard) Tickets() []Ticket {
	return nil
}

// TracerOf reports who currently traces one process.
func (g *PtraceAttachGuard) TracerOf(traceePID int) (int, bool) {
	return 0, false
}

// Stats returns the guard counters.
func (g *PtraceAttachGuard) Stats() GuardStats {
	return GuardStats{}
}

// Snapshot serialises the process table, the tickets and the counters.
func (g *PtraceAttachGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PtraceAttachGuard) Restore(data []byte) error {
	return errNotImplemented
}
