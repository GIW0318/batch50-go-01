// Package task replays hand crafted JSON bodies through every layer of a chain
// and attributes disagreements between the parsers to a concrete layer.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	ReasonAgreement       = "reason_layers_agree"
	ReasonAcceptanceSplit = "reason_acceptance_split"
	ReasonValueSplit      = "reason_value_split"
	ReasonUnstable        = "reason_unstable"
	ReasonRewrittenEcho   = "reason_rewritten_echo"
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

const snapshotVersion = "json-parse-differential/v1"

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

type targetState struct {
	mu        sync.Mutex
	budget    int
	lastProbe time.Time
}

// JsonParseDifferential owns the case registry, the observations and the verdicts.
type JsonParseDifferential struct {
	mu sync.Mutex

	cfg   Config
	cases map[string]Case
	chain Chain

	targets map[string]*targetState

	observations []Observation
	verdicts     []Verdict
	verdictDiffs map[string][]Diff

	nextObservation int
	nextVerdict     int
	inflight        int
	epoch           uint64
}

// NewJsonParseDifferential builds a differential over the given cases.
func NewJsonParseDifferential(cfg Config, cases []Case) *JsonParseDifferential {
	registry := make(map[string]Case, len(cases))
	for _, c := range cases {
		registry[c.CaseID] = c
	}
	return &JsonParseDifferential{
		cfg:             cfg,
		cases:           registry,
		targets:         make(map[string]*targetState),
		verdictDiffs:    make(map[string][]Diff),
		nextObservation: 1,
		nextVerdict:     1,
	}
}

// AddTarget registers one chain under test.
func (d *JsonParseDifferential) AddTarget(t Target) error {
	if strings.TrimSpace(t.TargetID) == "" {
		return ErrBadTarget
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.targets[t.TargetID]; ok {
		return nil
	}
	budget := d.cfg.MaxProbesPerTarget
	if budget < 0 {
		budget = 0
	}
	d.targets[t.TargetID] = &targetState{budget: budget}
	return nil
}

// SetChain attaches the transport used by Probe.
func (d *JsonParseDifferential) SetChain(c Chain) error {
	if nilInterface(c) {
		return ErrNoChain
	}
	d.mu.Lock()
	d.chain = c
	d.mu.Unlock()
	return nil
}

// Case returns the immutable payload of one registered case.
func (d *JsonParseDifferential) Case(name string) (Payload, error) {
	d.mu.Lock()
	c, ok := d.cases[name]
	d.mu.Unlock()
	if !ok {
		return Payload{}, ErrUnknownCase
	}
	return payloadFor(c), nil
}

// Probe replays one case through every layer of one target.
func (d *JsonParseDifferential) Probe(target Target, c Case) (Observation, error) {
	d.mu.Lock()
	state, ok := d.targets[target.TargetID]
	if !ok {
		d.mu.Unlock()
		return Observation{}, ErrUnknownTarget
	}
	registered, ok := d.cases[c.CaseID]
	if !ok {
		d.mu.Unlock()
		return Observation{}, ErrUnknownCase
	}
	if nilInterface(d.chain) {
		d.mu.Unlock()
		return Observation{}, ErrNoChain
	}
	d.mu.Unlock()

	// One target advances serially. Other targets do not share this lock, so they
	// can continue probing in parallel.
	state.mu.Lock()
	defer state.mu.Unlock()

	d.mu.Lock()
	current, ok := d.targets[target.TargetID]
	if !ok || current != state {
		d.mu.Unlock()
		return Observation{}, ErrUnknownTarget
	}
	registered, ok = d.cases[c.CaseID]
	if !ok {
		d.mu.Unlock()
		return Observation{}, ErrUnknownCase
	}
	chain := d.chain
	if nilInterface(chain) {
		d.mu.Unlock()
		return Observation{}, ErrNoChain
	}
	layers := append([]string(nil), d.cfg.Layers...)
	if len(layers) == 0 {
		d.mu.Unlock()
		return Observation{}, ErrNoChain
	}
	if state.budget <= 0 {
		d.mu.Unlock()
		return Observation{}, ErrBudgetExhausted
	}
	if d.cfg.Cooldown > 0 && !state.lastProbe.IsZero() && time.Since(state.lastProbe) < d.cfg.Cooldown {
		d.mu.Unlock()
		return Observation{}, ErrCooldown
	}
	if d.cfg.MaxObservations > 0 && len(d.observations)+d.inflight >= d.cfg.MaxObservations {
		d.mu.Unlock()
		return Observation{}, ErrObservationLimit
	}
	d.inflight++
	probeEpoch := d.epoch
	d.mu.Unlock()

	views := make([]LayerView, 0, len(layers))
	sent := false
	var layerErr error
	for _, layer := range layers {
		sent = true
		result, err := chain.Send(layer, registered.Body)
		if err != nil {
			layerErr = fmt.Errorf("%w: layer %s: %v", ErrLayerFailure, layer, err)
			break
		}
		views = append(views, LayerView{
			Layer:     layer,
			Accepted:  result.Accepted,
			Value:     result.Value,
			Rewritten: result.Rewritten,
		})
	}
	finishedAt := time.Now()

	d.mu.Lock()
	if d.epoch != probeEpoch {
		// A restore replaced the whole state while this request was in flight.
		d.mu.Unlock()
		if layerErr != nil {
			return Observation{}, layerErr
		}
		return Observation{}, ErrUnknownTarget
	}
	d.inflight--
	if sent {
		if state.budget > 0 {
			state.budget--
		}
		state.lastProbe = finishedAt
	}
	if layerErr != nil {
		d.mu.Unlock()
		return Observation{}, layerErr
	}

	obs := Observation{
		ObsID:    fmt.Sprintf("obs-%d", d.nextObservation),
		CaseID:   registered.CaseID,
		TargetID: target.TargetID,
		Views:    views,
		Digest:   digestOf(registered.Body),
	}
	d.nextObservation++
	d.observations = append(d.observations, cloneObservation(obs))
	d.mu.Unlock()

	return cloneObservation(obs), nil
}

// Compare grades a set of observations that share one case.
func (d *JsonParseDifferential) Compare(obs []Observation) (Verdict, error) {
	if len(obs) == 0 {
		return Verdict{}, ErrNoObservations
	}
	caseID := obs[0].CaseID
	for _, o := range obs {
		if o.CaseID != caseID {
			return Verdict{}, ErrMixedCases
		}
	}

	// Keep repeated probes of the same target together. A target whose repeated
	// answers differ is unstable; that uncertainty takes precedence over every
	// apparent layer discrepancy.
	groups := make(map[string][]Observation)
	targetOrder := make([]string, 0)
	for _, o := range obs {
		if _, exists := groups[o.TargetID]; !exists {
			targetOrder = append(targetOrder, o.TargetID)
		}
		groups[o.TargetID] = append(groups[o.TargetID], cloneObservation(o))
	}
	representatives := make([]Observation, 0, len(targetOrder))
	for _, targetID := range targetOrder {
		group := groups[targetID]
		for i := 1; i < len(group); i++ {
			if !sameViews(group[0].Views, group[i].Views) {
				return d.storeVerdict(caseID, LevelUncertain, ReasonUnstable, nil), nil
			}
		}
		representatives = append(representatives, group[0])
	}

	level := LevelConsistent
	reason := ReasonAgreement
	var diffs []Diff
	for _, observation := range representatives {
		obsLevel, obsReason, obsDiffs := analyzeObservation(observation, d.caseImpact(observation.CaseID))
		diffs = append(diffs, obsDiffs...)
		if severityRank(obsLevel) > severityRank(level) {
			level = obsLevel
		}
		if reasonRank(obsReason) > reasonRank(reason) {
			reason = obsReason
		}
	}
	if level == LevelConsistent && len(diffs) != 0 {
		level = LevelLow
	}

	return d.storeVerdict(caseID, level, reason, diffs), nil
}

// Explain returns the diffs behind one verdict.
func (d *JsonParseDifferential) Explain(verdictID string) ([]Diff, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	diffs, ok := d.verdictDiffs[verdictID]
	if !ok {
		return nil, ErrUnknownVerdict
	}
	return cloneDiffs(diffs), nil
}

// Targets lists the registered targets in ascending order.
func (d *JsonParseDifferential) Targets() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.targets))
	for id := range d.targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// BudgetLeft reports the probes still allowed for one target.
func (d *JsonParseDifferential) BudgetLeft(targetID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.targets[targetID]
	if !ok {
		return 0
	}
	return state.budget
}

// Observations returns a copy of every recorded observation.
func (d *JsonParseDifferential) Observations() []Observation {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Observation, len(d.observations))
	for i, observation := range d.observations {
		out[i] = cloneObservation(observation)
	}
	return out
}

// Verdicts lists the verdict ids in creation order.
func (d *JsonParseDifferential) Verdicts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.verdicts))
	for i, verdict := range d.verdicts {
		out[i] = verdict.VerdictID
	}
	return out
}

type snapshotTarget struct {
	TargetID  string    `json:"target_id"`
	Budget    int       `json:"budget"`
	LastProbe time.Time `json:"last_probe,omitempty"`
}

type differentialSnapshot struct {
	Version         string           `json:"version"`
	Targets         []snapshotTarget `json:"targets"`
	Observations    []Observation    `json:"observations"`
	Verdicts        []Verdict        `json:"verdicts"`
	NextObservation int              `json:"next_observation"`
	NextVerdict     int              `json:"next_verdict"`
}

// Snapshot serialises the targets, observations, verdicts and the budgets.
func (d *JsonParseDifferential) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	snap := differentialSnapshot{
		Version:         snapshotVersion,
		Targets:         make([]snapshotTarget, 0, len(d.targets)),
		Observations:    make([]Observation, len(d.observations)),
		Verdicts:        make([]Verdict, len(d.verdicts)),
		NextObservation: d.nextObservation,
		NextVerdict:     d.nextVerdict,
	}
	for id, state := range d.targets {
		snap.Targets = append(snap.Targets, snapshotTarget{
			TargetID:  id,
			Budget:    state.budget,
			LastProbe: state.lastProbe,
		})
	}
	sort.Slice(snap.Targets, func(i, j int) bool {
		return snap.Targets[i].TargetID < snap.Targets[j].TargetID
	})
	for i, observation := range d.observations {
		snap.Observations[i] = cloneObservation(observation)
	}
	for i, verdict := range d.verdicts {
		snap.Verdicts[i] = cloneVerdict(verdict)
	}
	return json.Marshal(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *JsonParseDifferential) Restore(data []byte) error {
	snap, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}
	if err := validateSnapshot(snap); err != nil {
		return ErrBadSnapshot
	}

	targets := make(map[string]*targetState, len(snap.Targets))
	for _, target := range snap.Targets {
		targets[target.TargetID] = &targetState{
			budget:    target.Budget,
			lastProbe: target.LastProbe,
		}
	}
	observations := make([]Observation, len(snap.Observations))
	for i, observation := range snap.Observations {
		observations[i] = cloneObservation(observation)
	}
	verdicts := make([]Verdict, len(snap.Verdicts))
	verdictDiffs := make(map[string][]Diff, len(snap.Verdicts))
	for i, verdict := range snap.Verdicts {
		verdicts[i] = cloneVerdict(verdict)
		verdictDiffs[verdict.VerdictID] = cloneDiffs(verdict.Diffs)
	}

	d.mu.Lock()
	d.targets = targets
	d.observations = observations
	d.verdicts = verdicts
	d.verdictDiffs = verdictDiffs
	d.nextObservation = snap.NextObservation
	d.nextVerdict = snap.NextVerdict
	d.inflight = 0
	d.epoch++
	d.mu.Unlock()
	return nil
}

func decodeSnapshot(data []byte) (differentialSnapshot, error) {
	var snap differentialSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snap); err != nil {
		return differentialSnapshot{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return differentialSnapshot{}, ErrBadSnapshot
	}
	return snap, nil
}

func validateSnapshot(snap differentialSnapshot) error {
	if snap.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if snap.NextObservation < 1 || snap.NextVerdict < 1 {
		return ErrBadSnapshot
	}

	targetIDs := make(map[string]struct{}, len(snap.Targets))
	for _, target := range snap.Targets {
		if strings.TrimSpace(target.TargetID) == "" || target.Budget < 0 {
			return ErrBadSnapshot
		}
		if _, exists := targetIDs[target.TargetID]; exists {
			return ErrBadSnapshot
		}
		targetIDs[target.TargetID] = struct{}{}
	}

	observationIDs := make(map[string]struct{}, len(snap.Observations))
	maxObservation := 0
	for _, observation := range snap.Observations {
		if observation.ObsID == "" || observation.CaseID == "" {
			return ErrBadSnapshot
		}
		n, ok := numericID(observation.ObsID, "obs-")
		if !ok || !validDigest(observation.Digest) {
			return ErrBadSnapshot
		}
		if _, exists := observationIDs[observation.ObsID]; exists {
			return ErrBadSnapshot
		}
		if _, exists := targetIDs[observation.TargetID]; !exists {
			return ErrBadSnapshot
		}
		observationIDs[observation.ObsID] = struct{}{}
		if n > maxObservation {
			maxObservation = n
		}
	}
	if snap.NextObservation <= maxObservation {
		return ErrBadSnapshot
	}

	verdictIDs := make(map[string]struct{}, len(snap.Verdicts))
	maxVerdict := 0
	for _, verdict := range snap.Verdicts {
		if verdict.VerdictID == "" || verdict.CaseID == "" {
			return ErrBadSnapshot
		}
		n, ok := numericID(verdict.VerdictID, "verdict-")
		if !ok || !knownLevel(verdict.Level) || !knownReason(verdict.Reason) {
			return ErrBadSnapshot
		}
		for _, diff := range verdict.Diffs {
			if diff.LayerA == "" || diff.LayerB == "" || !knownDiffField(diff.Field) {
				return ErrBadSnapshot
			}
		}
		if _, exists := verdictIDs[verdict.VerdictID]; exists {
			return ErrBadSnapshot
		}
		verdictIDs[verdict.VerdictID] = struct{}{}
		if n > maxVerdict {
			maxVerdict = n
		}
	}
	if snap.NextVerdict <= maxVerdict {
		return ErrBadSnapshot
	}
	return nil
}

func numericID(id, prefix string) (int, bool) {
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	return n, err == nil && n > 0
}

func validDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func knownLevel(level string) bool {
	switch level {
	case LevelConsistent, LevelLow, LevelMedium, LevelHigh, LevelUncertain:
		return true
	default:
		return false
	}
}

func knownReason(reason string) bool {
	switch reason {
	case ReasonAgreement, ReasonAcceptanceSplit, ReasonValueSplit, ReasonUnstable, ReasonRewrittenEcho:
		return true
	default:
		return false
	}
}

func knownDiffField(field string) bool {
	switch field {
	case "accepted", "value", "rewritten":
		return true
	default:
		return false
	}
}

func (d *JsonParseDifferential) caseImpact(caseID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cases[caseID].Impact
}

func (d *JsonParseDifferential) storeVerdict(caseID, level, reason string, diffs []Diff) Verdict {
	d.mu.Lock()
	defer d.mu.Unlock()
	verdict := Verdict{
		VerdictID: fmt.Sprintf("verdict-%d", d.nextVerdict),
		CaseID:    caseID,
		Level:     level,
		Reason:    reason,
		Diffs:     cloneDiffs(diffs),
	}
	d.nextVerdict++
	d.verdicts = append(d.verdicts, cloneVerdict(verdict))
	d.verdictDiffs[verdict.VerdictID] = cloneDiffs(verdict.Diffs)
	return cloneVerdict(verdict)
}

func analyzeObservation(observation Observation, impact string) (string, string, []Diff) {
	views := observation.Views
	if len(views) == 0 {
		return LevelConsistent, ReasonAgreement, nil
	}

	anyAccepted := false
	allAccepted := true
	allRejected := true
	for _, view := range views {
		if view.Accepted {
			anyAccepted = true
			allRejected = false
		} else {
			allAccepted = false
		}
	}
	if allRejected {
		return LevelConsistent, ReasonAgreement, nil
	}
	if anyAccepted && !allAccepted {
		return LevelHigh, ReasonAcceptanceSplit, acceptanceDiffs(views)
	}

	values := valueDiffs(views)
	if len(values) != 0 {
		level := LevelLow
		if impact == ImpactAuth || impact == ImpactWrite {
			level = LevelHigh
		}
		return level, ReasonValueSplit, values
	}

	if rewritten := rewriteDiffs(views); len(rewritten) != 0 {
		return LevelMedium, ReasonRewrittenEcho, rewritten
	}
	return LevelConsistent, ReasonAgreement, nil
}

func acceptanceDiffs(views []LayerView) []Diff {
	diffs := make([]Diff, 0)
	for i := 0; i < len(views); i++ {
		for j := i + 1; j < len(views); j++ {
			if views[i].Accepted == views[j].Accepted {
				continue
			}
			diffs = append(diffs, Diff{
				Field:  "accepted",
				LayerA: views[i].Layer,
				LayerB: views[j].Layer,
				ValueA: strconv.FormatBool(views[i].Accepted),
				ValueB: strconv.FormatBool(views[j].Accepted),
			})
		}
	}
	return diffs
}

func valueDiffs(views []LayerView) []Diff {
	diffs := make([]Diff, 0)
	for i := 0; i < len(views); i++ {
		for j := i + 1; j < len(views); j++ {
			if views[i].Value == views[j].Value {
				continue
			}
			diffs = append(diffs, Diff{
				Field:  "value",
				LayerA: views[i].Layer,
				LayerB: views[j].Layer,
				ValueA: views[i].Value,
				ValueB: views[j].Value,
			})
		}
	}
	return diffs
}

func rewriteDiffs(views []LayerView) []Diff {
	rewritten := make([]int, 0)
	untouched := make([]int, 0)
	for i, view := range views {
		if view.Rewritten {
			rewritten = append(rewritten, i)
		} else {
			untouched = append(untouched, i)
		}
	}
	if len(rewritten) == 0 {
		return nil
	}

	diffs := make([]Diff, 0, len(rewritten))
	for _, r := range rewritten {
		other := r - 1
		if other < 0 {
			other = r + 1
		}
		if len(untouched) != 0 {
			other = untouched[0]
		}
		if other < 0 || other >= len(views) || other == r {
			continue
		}
		diffs = append(diffs, Diff{
			Field:  "rewritten",
			LayerA: views[r].Layer,
			LayerB: views[other].Layer,
			ValueA: "true",
			ValueB: strconv.FormatBool(views[other].Rewritten),
		})
	}
	return diffs
}

func severityRank(level string) int {
	switch level {
	case LevelHigh:
		return 3
	case LevelMedium:
		return 2
	case LevelLow:
		return 1
	default:
		return 0
	}
}

func reasonRank(reason string) int {
	switch reason {
	case ReasonAcceptanceSplit:
		return 4
	case ReasonValueSplit:
		return 3
	case ReasonRewrittenEcho:
		return 2
	case ReasonUnstable:
		return 5
	default:
		return 1
	}
}

func sameViews(a, b []LayerView) bool {
	if len(a) != len(b) {
		return false
	}
	left := canonicalViews(a)
	right := canonicalViews(b)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func canonicalViews(views []LayerView) []string {
	out := make([]string, len(views))
	for i, view := range views {
		out[i] = fmt.Sprintf("%s\x00%t\x00%s\x00%t", view.Layer, view.Accepted, view.Value, view.Rewritten)
	}
	sort.Strings(out)
	return out
}

func payloadFor(c Case) Payload {
	return Payload{
		CaseID:    c.CaseID,
		Dimension: c.Dimension,
		Impact:    c.Impact,
		Body:      c.Body,
		Digest:    digestOf(c.Body),
	}
}

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func cloneObservation(observation Observation) Observation {
	clone := observation
	clone.Views = append([]LayerView(nil), observation.Views...)
	return clone
}

func cloneVerdict(verdict Verdict) Verdict {
	clone := verdict
	clone.Diffs = cloneDiffs(verdict.Diffs)
	return clone
}

func cloneDiffs(diffs []Diff) []Diff {
	if diffs == nil {
		return nil
	}
	return append([]Diff(nil), diffs...)
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
