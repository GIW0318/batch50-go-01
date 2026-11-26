// Package task runs injection validation cases and judges them from two
// independent channels: echoed output and controlled latency.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Injection sinks understood by the planner.
const (
	SinkCommand  = "command"
	SinkTemplate = "template"
)

// Attempt channels.
const (
	ChannelBaseline = "baseline"
	ChannelEcho     = "echo"
	ChannelTime     = "time"
)

// Combined channel value used when both channels hit.
const ChannelBoth = "both"

// Case lifecycle states.
const (
	CasePlanned = "planned"
	CaseRunning = "running"
	CaseJudged  = "judged"
	CaseAborted = "aborted"
)

// Confidence tiers attached to a hit.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Reason codes attached to an outcome.
const (
	ReasonEchoHit = "reason_echo_hit"
	ReasonTimeHit = "reason_time_hit"
	ReasonBothHit = "reason_both_channels_hit"
	ReasonNoHit   = "reason_no_channel_hit"
	ReasonAborted = "reason_aborted"
	ReasonAttempt = "reason_attempt_issued"
)

// Judging anchors.
const (
	AttemptsPerCase    = 3
	TimeHitFloorMillis = 100
)

// Errors reported by the runner.
var (
	ErrBlankPoint      = errors.New("injection point key and parameter are required")
	ErrBadSink         = errors.New("unknown injection sink kind")
	ErrBadPayload      = errors.New("each payload needs a name, a value and a marker")
	ErrPointExists     = errors.New("injection point already planned")
	ErrPointLimit      = errors.New("concurrent injection point limit reached")
	ErrAttemptLimit    = errors.New("attempt budget cannot cover the planned cases")
	ErrUnknownCase     = errors.New("unknown case")
	ErrUnknownAttempt  = errors.New("unknown attempt")
	ErrAttemptPending  = errors.New("the current attempt has not been submitted yet")
	ErrCasePending     = errors.New("another case of the same point still has an open attempt")
	ErrAttemptClosed   = errors.New("attempt already submitted")
	ErrChannelsPending = errors.New("not all channels have a response")
	ErrCaseClosed      = errors.New("case has no remaining attempts")
	ErrCaseAborted     = errors.New("case aborted")
	ErrAbortReason     = errors.New("abort reason is required")
	ErrDelayBudget     = errors.New("total delay budget exhausted")
	ErrBadResponse     = errors.New("response does not match the attempt")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Payload is one injection value with its echo marker and declared delay.
type Payload struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Marker      string `json:"marker"`
	DelayMillis int    `json:"delay_millis"`
}

// InjectionPoint is one candidate parameter with the payloads to try.
type InjectionPoint struct {
	Key         string    `json:"key"`
	Param       string    `json:"param"`
	BaseRequest string    `json:"base_request"`
	SinkKind    string    `json:"sink_kind"`
	Payloads    []Payload `json:"payloads"`
}

// Case is one payload bound to one injection point.
type Case struct {
	ID          string `json:"id"`
	PointKey    string `json:"point_key"`
	PayloadName string `json:"payload_name"`
	State       string `json:"state"`
}

// CaseSet is the plan produced for one injection point.
type CaseSet struct {
	ID              string `json:"id"`
	PointKey        string `json:"point_key"`
	Cases           []Case `json:"cases"`
	AttemptsPerCase int    `json:"attempts_per_case"`
}

// Attempt is one channel request handed to the operator.
type Attempt struct {
	ID      string     `json:"id"`
	CaseID  string     `json:"case_id"`
	Channel string     `json:"channel"`
	Ordinal int        `json:"ordinal"`
	Payload Payload    `json:"payload"`
	Budget  BudgetView `json:"budget"`
}

// Response is what the operator observed for one attempt.
type Response struct {
	AttemptID     string `json:"attempt_id"`
	StatusCode    int    `json:"status_code"`
	Body          string `json:"body"`
	ElapsedMillis int    `json:"elapsed_millis"`
	RateLimited   bool   `json:"rate_limited"`
}

// Outcome is the judged result of one case.
type Outcome struct {
	CaseID          string `json:"case_id"`
	PointKey        string `json:"point_key"`
	Hit             bool   `json:"hit"`
	Channel         string `json:"channel"`
	Confidence      string `json:"confidence"`
	Reason          string `json:"reason"`
	EchoMatched     bool   `json:"echo_matched"`
	BaselineMillis  int    `json:"baseline_millis"`
	ObservedMillis  int    `json:"observed_millis"`
	DeltaMillis     int    `json:"delta_millis"`
	ThresholdMillis int    `json:"threshold_millis"`
}

// BudgetView is the remaining allowance of a case set.
type BudgetView struct {
	AttemptsLeft int `json:"attempts_left"`
	MillisLeft   int `json:"millis_left"`
}

// Options bounds a runner and its delay budget.
type Options struct {
	MaxAttemptsPerPoint    int
	MaxConcurrentPoints    int
	TotalDelayBudgetMillis int
}

// InjectionOracleRunner owns the case sets, attempts and outcomes.
type InjectionOracleRunner struct {
	opts Options
}

// NewInjectionOracleRunner builds a runner with the given caps.
func NewInjectionOracleRunner(opts Options) *InjectionOracleRunner {
	return &InjectionOracleRunner{opts: opts}
}

// Plan turns one injection point into a serial case set.
func (r *InjectionOracleRunner) Plan(point InjectionPoint) (CaseSet, error) {
	return CaseSet{}, errNotImplemented
}

// Run hands out the next attempt of a case in channel order.
func (r *InjectionOracleRunner) Run(caseID string) (Attempt, error) {
	return Attempt{}, errNotImplemented
}

// Submit records the response of one attempt.
func (r *InjectionOracleRunner) Submit(attemptID string, resp Response) error {
	return errNotImplemented
}

// Judge evaluates a case from the echo and latency channels.
func (r *InjectionOracleRunner) Judge(caseID string) (Outcome, error) {
	return Outcome{}, errNotImplemented
}

// Abort closes a case with an operator supplied reason.
func (r *InjectionOracleRunner) Abort(caseID string, reason string) error {
	return errNotImplemented
}

// Case reads one planned case.
func (r *InjectionOracleRunner) Case(caseID string) (Case, bool) {
	return Case{}, false
}

// Cases lists the cases of one injection point in planning order.
func (r *InjectionOracleRunner) Cases(pointKey string) []Case {
	return nil
}

// Budget reads the remaining allowance of a case set.
func (r *InjectionOracleRunner) Budget(caseID string) (BudgetView, error) {
	return BudgetView{}, errNotImplemented
}

// Snapshot serialises plans, attempts, responses and outcomes.
func (r *InjectionOracleRunner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *InjectionOracleRunner) Restore(data []byte) error {
	return errNotImplemented
}
