// Package task plans response header injection probes, tracks their
// lifecycle and judges the returned responses.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Payload encodings tried for every injection point.
const (
	VariantCRLF          = "crlf"
	VariantLF            = "lf"
	VariantDoublePercent = "double_percent"
	VariantUTF16         = "utf16"
)

// Sinks a payload can travel through.
const (
	SinkQuery  = "query"
	SinkHeader = "header"
)

// Probe locations reported by Judge.
const (
	LocationHeader = "header"
	LocationBody   = "body"
	LocationNone   = "none"
)

// Reason codes attached to a missed case.
const (
	ReasonFiltered     = "reason_filtered"
	ReasonNormalized   = "reason_normalized"
	ReasonNoReflection = "reason_no_reflection"
)

// ProbeHeader is the response header name a successful injection creates.
const ProbeHeader = "x-gsb-probe"

// Errors returned by the prober.
var (
	ErrBlankPoint         = errors.New("injection point id, path, param and header are required")
	ErrPointLimit         = errors.New("injection point limit reached")
	ErrCaseLimit          = errors.New("per point case budget is below the required variant set")
	ErrUnknownCase        = errors.New("unknown case")
	ErrCaseNotCrafted     = errors.New("case has no crafted request")
	ErrCaseAlreadySubmitted = errors.New("case already has a submitted response")
	ErrNotSubmitted       = errors.New("case has no submitted response")
	ErrCaseCancelled      = errors.New("case was cleaned up")
	ErrRateLimit          = errors.New("request rate limit reached")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// HeaderPoint is one request field that may reach a response header.
type HeaderPoint struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Param  string `json:"param"`
	Header string `json:"header"`
}

// Case is one crafted payload for one sink of an injection point.
type Case struct {
	ID      string `json:"id"`
	PointID string `json:"point_id"`
	Variant string `json:"variant"`
	Payload string `json:"payload"`
	Sink    string `json:"sink"`
}

// CaseSet is the planned plan for one injection point.
type CaseSet struct {
	PointID string `json:"point_id"`
	Cases   []Case `json:"cases"`
}

// Request is the concrete request built for one case.
type Request struct {
	CaseID  string            `json:"case_id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Target  string            `json:"target"`
	Headers map[string]string `json:"headers"`
}

// Header is one parsed response header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Response is what came back for one case.
type Response struct {
	CaseID  string   `json:"case_id"`
	Status  int      `json:"status"`
	Raw     []byte   `json:"raw"`
	Headers []Header `json:"headers"`
	Body    string   `json:"body"`
}

// Outcome is the judged result of one case.
type Outcome struct {
	CaseID   string `json:"case_id"`
	Hit      bool   `json:"hit"`
	Location string `json:"location"`
	Reason   string `json:"reason"`
	Header   string `json:"header"`
	Value    string `json:"value"`
}

// Limits bounds the plan size and the request rate.
type Limits struct {
	MaxPoints            int
	MaxCasesPerPoint     int
	MaxRequestsPerWindow int
	Window               time.Duration
}

// HeaderInjectionProber owns the case table and the request rate window.
type HeaderInjectionProber struct {
	limits Limits
	now    func() time.Time
}

// NewHeaderInjectionProber builds a prober with the given caps.
func NewHeaderInjectionProber(limits Limits) *HeaderInjectionProber {
	return &HeaderInjectionProber{limits: limits, now: time.Now}
}

// SetClock replaces the clock used for the request rate window.
func (p *HeaderInjectionProber) SetClock(now func() time.Time) {
	p.now = now
}

// Plan builds the fixed variant set for one injection point.
func (p *HeaderInjectionProber) Plan(point HeaderPoint) (CaseSet, error) {
	return CaseSet{}, errNotImplemented
}

// Craft builds the request for one case and charges the rate window.
func (p *HeaderInjectionProber) Craft(caseID string) (Request, error) {
	return Request{}, errNotImplemented
}

// Submit stores the response of a crafted case.
func (p *HeaderInjectionProber) Submit(caseID string, resp Response) error {
	return errNotImplemented
}

// Judge decides whether the submitted response carries the injected header.
func (p *HeaderInjectionProber) Judge(caseID string) (Outcome, error) {
	return Outcome{}, errNotImplemented
}

// Cleanup cancels a case and any cache entry the probe created.
func (p *HeaderInjectionProber) Cleanup(caseID string) error {
	return errNotImplemented
}

// Cases lists the cases of one point in planning order.
func (p *HeaderInjectionProber) Cases(pointID string) []Case {
	return nil
}

// Progress reports submitted and planned case counts of one point.
func (p *HeaderInjectionProber) Progress(pointID string) (int, int) {
	return 0, 0
}

// Points lists the planned injection points in ascending order.
func (p *HeaderInjectionProber) Points() []string {
	return nil
}

// Snapshot serialises points, cases, responses and the rate window.
func (p *HeaderInjectionProber) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *HeaderInjectionProber) Restore(data []byte) error {
	return errNotImplemented
}
