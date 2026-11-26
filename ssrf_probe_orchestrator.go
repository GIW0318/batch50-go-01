// Package task orchestrates controlled outbound probes for SSRF validation.
// Every redirect hop is judged on its own resolved address before the next
// request is handed to the operator.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Supported probe schemes.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// Step kinds handed out by Next.
const (
	KindPrimary  = "primary"
	KindRedirect = "redirect"
)

// Probe lifecycle states.
const (
	StateQueued    = "queued"
	StateActive    = "active"
	StateBlocked   = "blocked"
	StateDone      = "done"
	StateExhausted = "exhausted"
	StateCancelled = "cancelled"
)

// Address families returned by ClassifyAddress.
const (
	FamilyPublic      = "public"
	FamilyLoopback    = "loopback"
	FamilyPrivate     = "private"
	FamilyLinkLocal   = "link_local"
	FamilyMulticast   = "multicast"
	FamilyUnspecified = "unspecified"
	FamilyInvalid     = "invalid"
)

// Reason codes attached to a verdict.
const (
	ReasonCompleted          = "reason_completed"
	ReasonBlockedLoopback    = "reason_blocked_loopback"
	ReasonBlockedPrivate     = "reason_blocked_private"
	ReasonBlockedLinkLocal   = "reason_blocked_link_local"
	ReasonBlockedMulticast   = "reason_blocked_multicast"
	ReasonBlockedUnspecified = "reason_blocked_unspecified"
	ReasonBlockedInvalid     = "reason_blocked_invalid"
	ReasonHopBudget          = "reason_hop_budget"
	ReasonTimeBudget         = "reason_time_budget"
	ReasonRequestBudget      = "reason_request_budget"
	ReasonCancelled          = "reason_cancelled"
	ReasonLookupFailed       = "reason_lookup_failed"
)

// Errors reported by the orchestrator.
var (
	ErrBlankTarget     = errors.New("target scheme, host and parameter are required")
	ErrBadScheme       = errors.New("only http and https targets are supported")
	ErrDuplicateProbe  = errors.New("an identical target is already in flight")
	ErrProbeLimit      = errors.New("concurrent probe limit reached")
	ErrUnknownProbe    = errors.New("unknown probe")
	ErrUnknownStep     = errors.New("unknown step")
	ErrStepPending     = errors.New("the current step has not been reported yet")
	ErrStepClosed      = errors.New("step already reported")
	ErrBlockedTarget   = errors.New("target resolves to a blocked address family")
	ErrBudgetExhausted = errors.New("probe budget exhausted")
	ErrBadRedirect     = errors.New("redirect location must be an absolute http or https url")
	ErrProbeCancelled  = errors.New("probe cancelled")
	ErrProbeClosed     = errors.New("probe has no runnable steps")
	ErrLookupFailed    = errors.New("host resolution failed")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Target is one injection point candidate submitted for probing.
type Target struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Method string `json:"method"`
	Param  string `json:"param"`
}

// Options bounds a probe run and injects host resolution.
type Options struct {
	MaxConcurrentProbes int
	MaxHops             int
	TotalMillis         int
	MaxRequestsPerProbe int
	Lookup              func(host string) ([]string, error)
}

// BudgetView is the remaining budget snapshot attached to a step.
type BudgetView struct {
	HopsLeft   int `json:"hops_left"`
	MillisLeft int `json:"millis_left"`
	Requests   int `json:"requests"`
}

// Step is one outbound request the operator must execute and report back.
type Step struct {
	ID      string     `json:"id"`
	ProbeID string     `json:"probe_id"`
	Kind    string     `json:"kind"`
	Hop     int        `json:"hop"`
	URL     string     `json:"url"`
	Budget  BudgetView `json:"budget"`
}

// Observation is what the operator saw when executing one step.
type Observation struct {
	StepID        string `json:"step_id"`
	StatusCode    int    `json:"status_code"`
	Location      string `json:"location"`
	ElapsedMillis int    `json:"elapsed_millis"`
	Error         string `json:"error"`
}

// Verdict is the final, auditable outcome of one probe.
type Verdict struct {
	ProbeID   string   `json:"probe_id"`
	Blocked   bool     `json:"blocked"`
	Cancelled bool     `json:"cancelled"`
	Done      bool     `json:"done"`
	Reason    string   `json:"reason"`
	Hop       int      `json:"hop"`
	Evidence  []string `json:"evidence"`
	Requests  int      `json:"requests"`
}

// ProbeState is a read only view of one probe's bookkeeping.
type ProbeState struct {
	ProbeID       string `json:"probe_id"`
	Target        Target `json:"target"`
	State         string `json:"state"`
	Hop           int    `json:"hop"`
	Requests      int    `json:"requests"`
	ElapsedMillis int    `json:"elapsed_millis"`
	Reason        string `json:"reason"`
}

// SsrfProbeOrchestrator owns the probe table, the hop budgets and the verdicts.
type SsrfProbeOrchestrator struct {
	opts Options
}

// NewSsrfProbeOrchestrator builds an orchestrator with the given caps.
func NewSsrfProbeOrchestrator(opts Options) *SsrfProbeOrchestrator {
	return &SsrfProbeOrchestrator{opts: opts}
}

// Submit registers one probe for a target and returns its identifier.
func (o *SsrfProbeOrchestrator) Submit(target Target) (string, error) {
	return "", errNotImplemented
}

// Next hands out the next step of a probe in chain order.
func (o *SsrfProbeOrchestrator) Next(probeID string) (Step, error) {
	return Step{}, errNotImplemented
}

// Report records the observation of one executed step.
func (o *SsrfProbeOrchestrator) Report(stepID string, obs Observation) error {
	return errNotImplemented
}

// Verdict returns the final judgement of a probe.
func (o *SsrfProbeOrchestrator) Verdict(probeID string) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Cancel stops a probe and releases its concurrency slot.
func (o *SsrfProbeOrchestrator) Cancel(probeID string) error {
	return errNotImplemented
}

// Probe reads one probe's bookkeeping.
func (o *SsrfProbeOrchestrator) Probe(probeID string) (ProbeState, bool) {
	return ProbeState{}, false
}

// Pending lists the probes that still hold a concurrency slot, ordered by id.
func (o *SsrfProbeOrchestrator) Pending() []string {
	return nil
}

// ClassifyAddress maps one literal address to its address family.
func (o *SsrfProbeOrchestrator) ClassifyAddress(addr string) string {
	return ""
}

// Snapshot serialises the probe table, chain positions and budgets.
func (o *SsrfProbeOrchestrator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (o *SsrfProbeOrchestrator) Restore(data []byte) error {
	return errNotImplemented
}
