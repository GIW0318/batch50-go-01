// Package task replays hand crafted JSON bodies through every layer of a chain
// and attributes disagreements between the parsers to a concrete layer.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Dimensions a case may exercise. One case changes exactly one of them.
const (
	DimensionDuplicateKey    = "duplicate_key"
	DimensionNumberPrecision = "number_precision"
	DimensionEscape          = "escape"
	DimensionSurrogate       = "surrogate"
)

// Impact classes of one case.
const (
	ImpactDisplay = "display"
	ImpactAuth    = "auth"
	ImpactWrite   = "write"
)

// Severity levels of a verdict.
const (
	LevelConsistent = "consistent"
	LevelLow        = "low"
	LevelMedium     = "medium"
	LevelHigh       = "high"
	LevelUncertain  = "uncertain"
)

// Reason codes carried by a verdict.
const (
	ReasonAgreement      = "reason_layers_agree"
	ReasonAcceptanceSplit = "reason_acceptance_split"
	ReasonValueSplit     = "reason_value_split"
	ReasonUnstable       = "reason_unstable"
	ReasonRewrittenEcho  = "reason_rewritten_echo"
)

// Errors returned by the differential.
var (
	ErrUnknownCase      = errors.New("unknown case")
	ErrUnknownTarget    = errors.New("unknown target")
	ErrBadTarget        = errors.New("target id is required")
	ErrNoChain          = errors.New("no chain attached")
	ErrBudgetExhausted  = errors.New("probe budget for this target is exhausted")
	ErrCooldown         = errors.New("target is inside the cooldown window")
	ErrNoObservations   = errors.New("at least one observation is required")
	ErrMixedCases       = errors.New("observations of one comparison must share a case")
	ErrUnknownVerdict   = errors.New("unknown verdict")
	ErrObservationLimit = errors.New("observation cap reached")
	ErrLayerFailure     = errors.New("a layer did not answer the probe")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Config bounds one differential instance.
type Config struct {
	MaxProbesPerTarget int
	MaxObservations    int
	Cooldown           time.Duration
	Layers             []string
}

// Case is one crafted payload together with the dimension it exercises.
type Case struct {
	CaseID    string `json:"case_id"`
	Name      string `json:"name"`
	Dimension string `json:"dimension"`
	Impact    string `json:"impact"`
	Body      string `json:"body"`
}

// Payload is the immutable form of a case handed to the caller.
type Payload struct {
	CaseID    string `json:"case_id"`
	Dimension string `json:"dimension"`
	Impact    string `json:"impact"`
	Body      string `json:"body"`
	Digest    string `json:"digest"`
}

// Target is one chain under test.
type Target struct {
	TargetID string `json:"target_id"`
}

// LayerResult is what one layer answered for the replayed body.
type LayerResult struct {
	Accepted  bool   `json:"accepted"`
	Value     string `json:"value"`
	Rewritten bool   `json:"rewritten"`
}

// Chain is the injected transport driven by Probe.
type Chain interface {
	Send(layer string, body string) (LayerResult, error)
}

// LayerView is one layer answer recorded with an observation.
type LayerView struct {
	Layer     string `json:"layer"`
	Accepted  bool   `json:"accepted"`
	Value     string `json:"value"`
	Rewritten bool   `json:"rewritten"`
}

// Observation is one replay of one case against one target.
type Observation struct {
	ObsID    string      `json:"obs_id"`
	CaseID   string      `json:"case_id"`
	TargetID string      `json:"target_id"`
	Views    []LayerView `json:"views"`
	Digest   string      `json:"digest"`
}

// Diff is one disagreement between two layers.
type Diff struct {
	Field  string `json:"field"`
	LayerA string `json:"layer_a"`
	LayerB string `json:"layer_b"`
	ValueA string `json:"value_a"`
	ValueB string `json:"value_b"`
}

// Verdict is the graded outcome of one comparison.
type Verdict struct {
	VerdictID string `json:"verdict_id"`
	CaseID    string `json:"case_id"`
	Level     string `json:"level"`
	Reason    string `json:"reason"`
	Diffs     []Diff `json:"diffs"`
}

// JsonParseDifferential owns the case registry, the observations and the verdicts.
type JsonParseDifferential struct {
	cfg   Config
	cases []Case
	chain Chain
}

// NewJsonParseDifferential builds a differential over the given cases.
func NewJsonParseDifferential(cfg Config, cases []Case) *JsonParseDifferential {
	return &JsonParseDifferential{cfg: cfg, cases: cases}
}

// AddTarget registers one chain under test.
func (d *JsonParseDifferential) AddTarget(t Target) error {
	return errNotImplemented
}

// SetChain attaches the transport used by Probe.
func (d *JsonParseDifferential) SetChain(c Chain) error {
	return errNotImplemented
}

// Case returns the immutable payload of one registered case.
func (d *JsonParseDifferential) Case(name string) (Payload, error) {
	return Payload{}, errNotImplemented
}

// Probe replays one case through every layer of one target.
func (d *JsonParseDifferential) Probe(target Target, c Case) (Observation, error) {
	return Observation{}, errNotImplemented
}

// Compare grades a set of observations that share one case.
func (d *JsonParseDifferential) Compare(obs []Observation) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Explain returns the diffs behind one verdict.
func (d *JsonParseDifferential) Explain(verdictID string) ([]Diff, error) {
	return nil, errNotImplemented
}

// Targets lists the registered targets in ascending order.
func (d *JsonParseDifferential) Targets() []string {
	return nil
}

// BudgetLeft reports the probes still allowed for one target.
func (d *JsonParseDifferential) BudgetLeft(targetID string) int {
	return 0
}

// Observations returns a copy of every recorded observation.
func (d *JsonParseDifferential) Observations() []Observation {
	return nil
}

// Verdicts lists the verdict ids in creation order.
func (d *JsonParseDifferential) Verdicts() []string {
	return nil
}

// Snapshot serialises the targets, observations, verdicts and the budgets.
func (d *JsonParseDifferential) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *JsonParseDifferential) Restore(data []byte) error {
	return errNotImplemented
}
