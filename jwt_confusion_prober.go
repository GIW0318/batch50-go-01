// Package task runs a controlled battery of token validation probes against one
// target: it builds the case matrix from a captured sample, keeps every case on
// its own marker, records the single response observed per case and turns that
// response into an evidence backed outcome.
package task

import "errors"

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Case kinds in the probe matrix.
const (
	CaseAlgorithm = "algorithm"
	CaseClaim     = "claim"
	CaseSignature = "signature"
)

// Case states kept by the prober.
const (
	CaseOpen      = "open"
	CaseSubmitted = "submitted"
	CaseJudged    = "judged"
	CaseCleaned   = "cleaned"
	CaseVoid      = "void"
)

// Errors reported by the prober.
var (
	ErrBadSample        = errors.New("sample token is incomplete")
	ErrCaseLimit        = errors.New("case matrix exceeds the per target cap")
	ErrBatchInFlight    = errors.New("a batch is still open for this target")
	ErrTargetLimit      = errors.New("too many targets are tracked")
	ErrUnknownTarget    = errors.New("unknown target")
	ErrUnknownCase      = errors.New("unknown case")
	ErrAlreadySubmitted = errors.New("case already has a response")
	ErrNotSubmitted     = errors.New("case has no response yet")
	ErrNotJudged        = errors.New("case is not judged yet")
	ErrCaseClosed       = errors.New("case is closed")
	ErrTokenVoid        = errors.New("case token is void")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Token is one captured token of the target.
type Token struct {
	Target    string
	Alg       string
	Header    string
	Payload   string
	Signature string
	Claims    []string
}

// Case is one probe in the matrix.
type Case struct {
	ID     string
	Target string
	Kind   string
	Name   string
	State  string
}

// Matrix is the whole probe battery of one target.
type Matrix struct {
	Target string
	Cases  []Case
}

// Response is the single observation collected for one case.
type Response struct {
	Status    int
	Body      string
	Protected bool
}

// Outcome is the judged result of one case.
type Outcome struct {
	CaseID   string
	Hit      bool
	Evidence string
	Reason   string
}

// Limits bounds the probe battery.
type Limits struct {
	MaxCasesPerTarget int
	MaxTargets        int
}

// JwtConfusionProber owns the case tables of every tracked target.
type JwtConfusionProber struct {
	limits Limits
}

// NewJwtConfusionProber builds a prober with the given caps.
func NewJwtConfusionProber(limits Limits) *JwtConfusionProber {
	return &JwtConfusionProber{limits: limits}
}

// Enumerate builds the probe matrix of one target from a captured sample.
func (p *JwtConfusionProber) Enumerate(sample Token) (Matrix, error) {
	return Matrix{}, errNotImplemented
}

// Craft returns the probe token of one case.
func (p *JwtConfusionProber) Craft(caseID string) (Token, error) {
	return Token{}, errNotImplemented
}

// Submit records the single response observed for one case.
func (p *JwtConfusionProber) Submit(caseID string, resp Response) error {
	return errNotImplemented
}

// Judge turns the recorded response of one case into an outcome.
func (p *JwtConfusionProber) Judge(caseID string) (Outcome, error) {
	return Outcome{}, errNotImplemented
}

// Cleanup closes one judged case.
func (p *JwtConfusionProber) Cleanup(caseID string) error {
	return errNotImplemented
}

// CleanupAll closes every case of a target and reports how many were closed.
func (p *JwtConfusionProber) CleanupAll(target string) (int, error) {
	return 0, errNotImplemented
}

// Close drops the case table of a target and frees its slot.
func (p *JwtConfusionProber) Close(target string) error {
	return errNotImplemented
}

// Cases lists the cases of one target in matrix order.
func (p *JwtConfusionProber) Cases(target string) []Case {
	return nil
}

// Progress reports how many cases are judged out of the whole matrix.
func (p *JwtConfusionProber) Progress(target string) (done, total int) {
	return 0, 0
}

// Snapshot serialises the case tables and their progress.
func (p *JwtConfusionProber) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the case tables with a snapshot, all or nothing.
func (p *JwtConfusionProber) Restore(data []byte) error {
	return errNotImplemented
}
