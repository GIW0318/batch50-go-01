// Package task probes candidate parameters with boolean and time based blind
// injection, converges on one character per position and keeps the inference
// reproducible across restarts.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Payload kinds.
const (
	PayloadBaseline = "baseline"
	PayloadBoolean  = "boolean"
	PayloadTime     = "time"
)

// Case lifecycle states.
const (
	StateInferring = "inferring"
	StateStopped   = "stopped"
	StateComplete  = "complete"
)

// Reason codes recorded on an inference step.
const (
	ReasonConfirmed  = "reason_character_confirmed"
	ReasonEliminated = "reason_candidate_eliminated"
	ReasonPending    = "reason_awaiting_probe"
)

// Judging anchors.
const (
	DefaultChangeFloor = 90
	TimeHitFloorMillis = 100
	BaselineExpression = "1=1"
)

// Errors reported by the prober.
var (
	ErrBlankPoint       = errors.New("parameter id and name are required")
	ErrBadSampleCount   = errors.New("baseline sample count below the configured minimum")
	ErrFetchFailed      = errors.New("baseline sampling failed")
	ErrBaselineUnstable = errors.New("baseline samples disagree on the response body")
	ErrPointExists      = errors.New("parameter already has a baseline")
	ErrPointLimit       = errors.New("concurrent parameter limit reached")
	ErrNoBaseline       = errors.New("parameter has no stable baseline")
	ErrUnknownCase      = errors.New("unknown inference case")
	ErrBadPayload       = errors.New("payload needs a known kind, an expression and an alphabet character")
	ErrBudgetExhausted  = errors.New("request budget exhausted")
	ErrCaseStopped      = errors.New("inference case is stopped")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// ParamPoint is one candidate request parameter.
type ParamPoint struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Method  string `json:"method"`
}

// Sample is one observed response used for a baseline.
type Sample struct {
	Body          string `json:"body"`
	ElapsedMillis int    `json:"elapsed_millis"`
}

// Baseline is the stabilised response signature of one parameter.
type Baseline struct {
	CaseID       string `json:"case_id"`
	PointID      string `json:"point_id"`
	Signature    string `json:"signature"`
	Samples      int    `json:"samples"`
	MeanMillis   int    `json:"mean_millis"`
	JitterMillis int    `json:"jitter_millis"`
	Stable       bool   `json:"stable"`
}

// Payload is one boolean or time based probe.
type Payload struct {
	Kind        string `json:"kind"`
	Expression  string `json:"expression"`
	ProbeChar   string `json:"probe_char"`
	DelayMillis int    `json:"delay_millis"`
}

// Diff is the differential observation of one probe.
type Diff struct {
	CaseID          string `json:"case_id"`
	PointID         string `json:"point_id"`
	Changed         bool   `json:"changed"`
	Similarity      int    `json:"similarity"`
	MeanMillis      int    `json:"mean_millis"`
	DeltaMillis     int    `json:"delta_millis"`
	ThresholdMillis int    `json:"threshold_millis"`
	LatencyHit      bool   `json:"latency_hit"`
	Position        int    `json:"position"`
}

// Inference is the current convergence state of one case.
type Inference struct {
	CaseID     string   `json:"case_id"`
	Position   int      `json:"position"`
	Candidates []string `json:"candidates"`
	Confirmed  string   `json:"confirmed"`
	Extracted  string   `json:"extracted"`
	Complete   bool     `json:"complete"`
}

// BudgetView is the remaining allowance of one case.
type BudgetView struct {
	RequestsLeft  int `json:"requests_left"`
	PositionsLeft int `json:"positions_left"`
	MillisLeft    int `json:"millis_left"`
}

// CaseView is a read only view of one inference case.
type CaseView struct {
	CaseID    string `json:"case_id"`
	PointID   string `json:"point_id"`
	State     string `json:"state"`
	Position  int    `json:"position"`
	Extracted string `json:"extracted"`
	Requests  int    `json:"requests"`
}

// Options bounds a prober and injects response sampling.
type Options struct {
	MaxRequestsPerPoint   int
	MaxConcurrentPoints   int
	MinBaselineSamples    int
	MaxPositions          int
	Alphabet              string
	TotalMillisBudget     int
	ChangeSimilarityFloor int
	Fetch                 func(point ParamPoint, payload Payload) (Sample, error)
}

// BlindSqliProber owns baselines, inference positions and budgets.
type BlindSqliProber struct {
	opts Options
}

// NewBlindSqliProber builds a prober with the given caps.
func NewBlindSqliProber(opts Options) *BlindSqliProber {
	return &BlindSqliProber{opts: opts}
}

// Baseline samples one parameter and stabilises its response signature.
func (p *BlindSqliProber) Baseline(point ParamPoint, samples int) (Baseline, error) {
	return Baseline{}, errNotImplemented
}

// Probe runs one boolean or time payload and returns its differential.
func (p *BlindSqliProber) Probe(point ParamPoint, payload Payload) (Diff, error) {
	return Diff{}, errNotImplemented
}

// Infer reports the converged candidates of one case.
func (p *BlindSqliProber) Infer(caseID string) (Inference, error) {
	return Inference{}, errNotImplemented
}

// Budget reads the remaining allowance of one case.
func (p *BlindSqliProber) Budget(caseID string) (BudgetView, error) {
	return BudgetView{}, errNotImplemented
}

// Stop freezes a case without discarding its evidence.
func (p *BlindSqliProber) Stop(caseID string) error {
	return errNotImplemented
}

// Case reads one inference case.
func (p *BlindSqliProber) Case(caseID string) (CaseView, bool) {
	return CaseView{}, false
}

// Points lists the parameter ids that already have a baseline, ordered by id.
func (p *BlindSqliProber) Points() []string {
	return nil
}

// Snapshot serialises baselines, positions, evidence and budgets.
func (p *BlindSqliProber) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *BlindSqliProber) Restore(data []byte) error {
	return errNotImplemented
}
