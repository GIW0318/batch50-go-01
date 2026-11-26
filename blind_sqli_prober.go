// Package task probes candidate parameters with boolean and time based blind
// injection, converges on one character per position and keeps the inference
// reproducible across restarts.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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

const (
	snapshotVersion   = 1
	minProbeSpacingMS = 5
	volatileToken     = "<volatile>"
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

type probeRecord struct {
	Payload Payload `json:"payload"`
	Sample  Sample  `json:"sample"`
	Diff    Diff    `json:"diff"`
	Hit     bool    `json:"hit"`
	Reason  string  `json:"reason"`
}

type caseState struct {
	// opMu serialises probes for this parameter. The remaining mutable
	// fields are guarded by BlindSqliProber.mu.
	opMu sync.Mutex

	caseID          string
	point           ParamPoint
	baseline        Baseline
	baselineSamples []Sample
	state           string
	position        int
	candidates      []string
	confirmed       string
	extracted       string
	requests        int
	elapsedMillis   int
	evidence        []probeRecord
	lastProbeAt     time.Time
}

// BlindSqliProber owns baselines, inference positions and budgets.
type BlindSqliProber struct {
	opts Options

	mu         sync.Mutex
	cases      map[string]*caseState
	points     map[string]*caseState
	pending    map[string]struct{}
	alphabet   []string
	generation uint64
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	State    json.RawMessage `json:"state"`
	Checksum string          `json:"checksum"`
}

type snapshotState struct {
	Points []string       `json:"points"`
	Cases  []snapshotCase `json:"cases"`
}

type snapshotCase struct {
	CaseID          string        `json:"case_id"`
	Point           ParamPoint    `json:"point"`
	Baseline        Baseline      `json:"baseline"`
	BaselineSamples []Sample      `json:"baseline_samples"`
	State           string        `json:"state"`
	Position        int           `json:"position"`
	Candidates      []string      `json:"candidates"`
	Confirmed       string        `json:"confirmed"`
	Extracted       string        `json:"extracted"`
	Requests        int           `json:"requests"`
	ElapsedMillis   int           `json:"elapsed_millis"`
	Evidence        []probeRecord `json:"evidence"`
	LastProbeAt     time.Time     `json:"last_probe_at"`
}

var caseSequence atomic.Uint64

// NewBlindSqliProber builds a prober with the given caps.
func NewBlindSqliProber(opts Options) *BlindSqliProber {
	return &BlindSqliProber{
		opts:     opts,
		cases:    make(map[string]*caseState),
		points:   make(map[string]*caseState),
		pending:  make(map[string]struct{}),
		alphabet: splitAlphabet(opts.Alphabet),
	}
}

// Baseline samples one parameter and stabilises its response signature.
func (p *BlindSqliProber) Baseline(point ParamPoint, samples int) (Baseline, error) {
	if strings.TrimSpace(point.ID) == "" || strings.TrimSpace(point.Name) == "" {
		return Baseline{}, ErrBlankPoint
	}
	if samples <= 0 || samples < p.opts.MinBaselineSamples {
		return Baseline{}, ErrBadSampleCount
	}
	if p.opts.Fetch == nil {
		return Baseline{}, ErrFetchFailed
	}

	p.mu.Lock()
	p.initLocked()
	if _, exists := p.points[point.ID]; exists {
		p.mu.Unlock()
		return Baseline{}, ErrPointExists
	}
	if _, exists := p.pending[point.ID]; exists {
		p.mu.Unlock()
		return Baseline{}, ErrPointExists
	}
	if p.opts.MaxConcurrentPoints > 0 && len(p.points)+len(p.pending) >= p.opts.MaxConcurrentPoints {
		p.mu.Unlock()
		return Baseline{}, ErrPointLimit
	}
	p.pending[point.ID] = struct{}{}
	generation := p.generation
	p.mu.Unlock()

	releasePending := func() {
		p.mu.Lock()
		delete(p.pending, point.ID)
		p.mu.Unlock()
	}

	observed := make([]Sample, 0, samples)
	signature := ""
	for i := 0; i < samples; i++ {
		sample, err := p.opts.Fetch(point, Payload{Kind: PayloadBaseline, Expression: BaselineExpression})
		if err != nil {
			releasePending()
			return Baseline{}, fmt.Errorf("%w: %v", ErrFetchFailed, err)
		}
		normalized := normalizeBody(sample.Body)
		if i == 0 {
			signature = normalized
		}
		observed = append(observed, sample)
	}

	stable := true
	for i := 1; i < len(observed); i++ {
		if normalizeBody(observed[i].Body) != signature {
			stable = false
			break
		}
	}
	if !stable {
		releasePending()
		return Baseline{}, ErrBaselineUnstable
	}

	mean, jitter := timingSummary(observed)
	base := Baseline{
		CaseID:       nextCaseID(),
		PointID:      point.ID,
		Signature:    signature,
		Samples:      samples,
		MeanMillis:   mean,
		JitterMillis: jitter,
		Stable:       true,
	}
	cs := &caseState{
		caseID:          base.CaseID,
		point:           point,
		baseline:        base,
		baselineSamples: cloneSamples(observed),
		state:           StateInferring,
		position:        1,
		candidates:      p.alphabetCopy(),
		elapsedMillis:   sumElapsed(observed),
		evidence:        make([]probeRecord, 0),
	}
	if p.opts.MaxPositions <= 0 {
		cs.state = StateComplete
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, point.ID)
	if p.generation != generation {
		return Baseline{}, ErrFetchFailed
	}
	if _, exists := p.points[point.ID]; exists {
		return Baseline{}, ErrPointExists
	}
	p.cases[base.CaseID] = cs
	p.points[point.ID] = cs
	return base, nil
}

// Probe runs one boolean or time payload and returns its differential.
func (p *BlindSqliProber) Probe(point ParamPoint, payload Payload) (Diff, error) {
	p.mu.Lock()
	p.initLocked()
	cs, ok := p.points[point.ID]
	if !ok {
		p.mu.Unlock()
		return Diff{}, ErrNoBaseline
	}
	if cs.state == StateStopped {
		p.mu.Unlock()
		return Diff{}, ErrCaseStopped
	}
	p.mu.Unlock()

	if err := p.validatePayload(payload); err != nil {
		return Diff{}, err
	}
	if p.opts.Fetch == nil {
		return Diff{}, ErrFetchFailed
	}

	cs.opMu.Lock()
	defer cs.opMu.Unlock()

	p.mu.Lock()
	if p.cases[cs.caseID] != cs {
		p.mu.Unlock()
		return Diff{}, ErrNoBaseline
	}
	if cs.state == StateStopped {
		p.mu.Unlock()
		return Diff{}, ErrCaseStopped
	}
	if err := p.budgetCheckLocked(cs); err != nil {
		p.mu.Unlock()
		return Diff{}, err
	}
	var wait time.Duration
	if !cs.lastProbeAt.IsZero() {
		next := cs.lastProbeAt.Add(minProbeSpacingMS * time.Millisecond)
		if now := time.Now(); now.Before(next) {
			wait = next.Sub(now)
		}
	}
	p.mu.Unlock()

	if wait > 0 {
		time.Sleep(wait)
	}

	p.mu.Lock()
	if p.cases[cs.caseID] != cs {
		p.mu.Unlock()
		return Diff{}, ErrNoBaseline
	}
	if cs.state == StateStopped {
		p.mu.Unlock()
		return Diff{}, ErrCaseStopped
	}
	if err := p.budgetCheckLocked(cs); err != nil {
		p.mu.Unlock()
		return Diff{}, err
	}
	position := cs.position
	baseline := cs.baseline
	generation := p.generation
	cs.requests++
	cs.lastProbeAt = time.Now()
	p.mu.Unlock()

	sample, fetchErr := p.opts.Fetch(point, payload)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cases[cs.caseID] != cs || p.generation != generation {
		if fetchErr != nil {
			return Diff{}, fmt.Errorf("%w: %v", ErrFetchFailed, fetchErr)
		}
		return Diff{}, ErrNoBaseline
	}
	if fetchErr != nil {
		return Diff{}, fmt.Errorf("%w: %v", ErrFetchFailed, fetchErr)
	}

	normalized := normalizeBody(sample.Body)
	similarity := similarityPercent(baseline.Signature, normalized)
	changed := similarity < p.changeFloor()
	delta := sample.ElapsedMillis - baseline.MeanMillis
	threshold := maxInt(TimeHitFloorMillis, payload.DelayMillis/2, baseline.JitterMillis*3)
	latencyHit := delta >= threshold
	diff := Diff{
		CaseID:          cs.caseID,
		PointID:         cs.point.ID,
		Changed:         changed,
		Similarity:      similarity,
		MeanMillis:      baseline.MeanMillis,
		DeltaMillis:     delta,
		ThresholdMillis: threshold,
		LatencyHit:      latencyHit,
		Position:        position,
	}

	cs.elapsedMillis += sample.ElapsedMillis
	if cs.state == StateStopped {
		return Diff{}, ErrCaseStopped
	}
	hit := probeHit(payload.Kind, changed, latencyHit)
	reason := ReasonPending
	if hit {
		reason = ReasonConfirmed
	} else if payload.ProbeChar != "" {
		reason = ReasonEliminated
	}
	if cs.state != StateComplete {
		p.applyProbeLocked(cs, payload, hit)
	}
	cs.evidence = append(cs.evidence, probeRecord{
		Payload: payload,
		Sample:  sample,
		Diff:    diff,
		Hit:     hit,
		Reason:  reason,
	})
	return diff, nil
}

// Infer reports the converged candidates of one case.
func (p *BlindSqliProber) Infer(caseID string) (Inference, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	cs, ok := p.cases[caseID]
	if !ok {
		return Inference{}, ErrUnknownCase
	}
	if cs.state == StateStopped {
		return Inference{}, ErrCaseStopped
	}
	return Inference{
		CaseID:     cs.caseID,
		Position:   cs.position,
		Candidates: cloneStrings(cs.candidates),
		Confirmed:  cs.confirmed,
		Extracted:  cs.extracted,
		Complete:   cs.state == StateComplete,
	}, nil
}

// Budget reads the remaining allowance of one case.
func (p *BlindSqliProber) Budget(caseID string) (BudgetView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	cs, ok := p.cases[caseID]
	if !ok {
		return BudgetView{}, ErrUnknownCase
	}

	requestsLeft := 0
	if p.opts.MaxRequestsPerPoint > 0 {
		requestsLeft = p.opts.MaxRequestsPerPoint - cs.requests
	}
	positionsLeft := 0
	if p.opts.MaxPositions > 0 {
		positionsLeft = p.opts.MaxPositions - len([]rune(cs.extracted))
	}
	millisLeft := 0
	if p.opts.TotalMillisBudget > 0 {
		millisLeft = p.opts.TotalMillisBudget - cs.elapsedMillis
	}
	if requestsLeft < 0 {
		requestsLeft = 0
	}
	if positionsLeft < 0 {
		positionsLeft = 0
	}
	if millisLeft < 0 {
		millisLeft = 0
	}
	return BudgetView{
		RequestsLeft:  requestsLeft,
		PositionsLeft: positionsLeft,
		MillisLeft:    millisLeft,
	}, nil
}

// Stop freezes a case without discarding its evidence.
func (p *BlindSqliProber) Stop(caseID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	cs, ok := p.cases[caseID]
	if !ok {
		return ErrUnknownCase
	}
	cs.state = StateStopped
	return nil
}

// Case reads one inference case.
func (p *BlindSqliProber) Case(caseID string) (CaseView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	cs, ok := p.cases[caseID]
	if !ok {
		return CaseView{}, false
	}
	return CaseView{
		CaseID:    cs.caseID,
		PointID:   cs.point.ID,
		State:     cs.state,
		Position:  cs.position,
		Extracted: cs.extracted,
		Requests:  cs.requests,
	}, true
}

// Points lists the parameter ids that already have a baseline, ordered by id.
func (p *BlindSqliProber) Points() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	out := make([]string, 0, len(p.points))
	for id := range p.points {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Snapshot serialises baselines, positions, evidence and budgets.
func (p *BlindSqliProber) Snapshot() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()

	state := snapshotState{
		Points: make([]string, 0, len(p.points)),
		Cases:  make([]snapshotCase, 0, len(p.cases)),
	}
	for id := range p.points {
		state.Points = append(state.Points, id)
	}
	sort.Strings(state.Points)
	for _, cs := range p.cases {
		state.Cases = append(state.Cases, snapshotCase{
			CaseID:          cs.caseID,
			Point:           cs.point,
			Baseline:        cs.baseline,
			BaselineSamples: cloneSamples(cs.baselineSamples),
			State:           cs.state,
			Position:        cs.position,
			Candidates:      cloneStrings(cs.candidates),
			Confirmed:       cs.confirmed,
			Extracted:       cs.extracted,
			Requests:        cs.requests,
			ElapsedMillis:   cs.elapsedMillis,
			Evidence:        cloneEvidence(cs.evidence),
			LastProbeAt:     cs.lastProbeAt,
		})
	}
	sort.Slice(state.Cases, func(i, j int) bool {
		return state.Cases[i].CaseID < state.Cases[j].CaseID
	})

	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		State:    raw,
		Checksum: checksum(raw),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *BlindSqliProber) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || len(envelope.State) == 0 ||
		!strings.EqualFold(envelope.Checksum, checksum(envelope.State)) {
		return ErrBadSnapshot
	}
	var state snapshotState
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.Points == nil || state.Cases == nil {
		return ErrBadSnapshot
	}

	restoredCases := make(map[string]*caseState, len(state.Cases))
	restoredPoints := make(map[string]*caseState, len(state.Cases))
	for _, sc := range state.Cases {
		if !validSnapshotCase(sc) {
			return ErrBadSnapshot
		}
		if _, exists := restoredCases[sc.CaseID]; exists {
			return ErrBadSnapshot
		}
		if _, exists := restoredPoints[sc.Point.ID]; exists {
			return ErrBadSnapshot
		}
		cs := &caseState{
			caseID:          sc.CaseID,
			point:           sc.Point,
			baseline:        sc.Baseline,
			baselineSamples: cloneSamples(sc.BaselineSamples),
			state:           sc.State,
			position:        sc.Position,
			candidates:      cloneStrings(sc.Candidates),
			confirmed:       sc.Confirmed,
			extracted:       sc.Extracted,
			requests:        sc.Requests,
			elapsedMillis:   sc.ElapsedMillis,
			evidence:        cloneEvidence(sc.Evidence),
			lastProbeAt:     sc.LastProbeAt,
		}
		restoredCases[sc.CaseID] = cs
		restoredPoints[sc.Point.ID] = cs
	}

	derivedPoints := make([]string, 0, len(restoredPoints))
	for id := range restoredPoints {
		derivedPoints = append(derivedPoints, id)
	}
	sort.Strings(derivedPoints)
	if len(derivedPoints) != len(state.Points) {
		return ErrBadSnapshot
	}
	for i := range derivedPoints {
		if derivedPoints[i] != state.Points[i] {
			return ErrBadSnapshot
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	p.cases = restoredCases
	p.points = restoredPoints
	p.pending = make(map[string]struct{})
	p.generation++
	return nil
}

func (p *BlindSqliProber) initLocked() {
	if p.cases == nil {
		p.cases = make(map[string]*caseState)
	}
	if p.points == nil {
		p.points = make(map[string]*caseState)
	}
	if p.pending == nil {
		p.pending = make(map[string]struct{})
	}
	if p.alphabet == nil {
		p.alphabet = splitAlphabet(p.opts.Alphabet)
	}
}

func (p *BlindSqliProber) alphabetCopy() []string {
	out := make([]string, len(p.alphabet))
	copy(out, p.alphabet)
	return out
}

func (p *BlindSqliProber) validatePayload(payload Payload) error {
	if payload.Kind != PayloadBoolean && payload.Kind != PayloadTime {
		return ErrBadPayload
	}
	if strings.TrimSpace(payload.Expression) == "" {
		return ErrBadPayload
	}
	if payload.ProbeChar != "" && !p.inAlphabet(payload.ProbeChar) {
		return ErrBadPayload
	}
	return nil
}

func (p *BlindSqliProber) inAlphabet(char string) bool {
	for _, candidate := range p.alphabet {
		if candidate == char {
			return true
		}
	}
	return false
}

func (p *BlindSqliProber) budgetCheckLocked(cs *caseState) error {
	if p.opts.MaxRequestsPerPoint > 0 && cs.requests >= p.opts.MaxRequestsPerPoint {
		return ErrBudgetExhausted
	}
	if p.opts.TotalMillisBudget > 0 && cs.elapsedMillis >= p.opts.TotalMillisBudget {
		return ErrBudgetExhausted
	}
	return nil
}

func (p *BlindSqliProber) changeFloor() int {
	if p.opts.ChangeSimilarityFloor <= 0 {
		return DefaultChangeFloor
	}
	return p.opts.ChangeSimilarityFloor
}

func (p *BlindSqliProber) applyProbeLocked(cs *caseState, payload Payload, hit bool) {
	if !hit {
		if payload.ProbeChar != "" {
			cs.candidates = eliminateExact(cs.candidates, payload.ProbeChar)
		}
		return
	}
	if payload.ProbeChar == "" {
		cs.state = StateComplete
		return
	}
	cs.position++
	cs.confirmed += payload.ProbeChar
	cs.extracted += payload.ProbeChar
	cs.candidates = p.alphabetCopy()
	if p.opts.MaxPositions > 0 && len([]rune(cs.extracted)) >= p.opts.MaxPositions {
		cs.state = StateComplete
	}
}

func probeHit(kind string, changed, latencyHit bool) bool {
	switch kind {
	case PayloadBoolean:
		return changed
	case PayloadTime:
		return latencyHit
	default:
		return false
	}
}

func nextCaseID() string {
	return fmt.Sprintf("case-%x-%x", time.Now().UnixNano(), caseSequence.Add(1))
}

func splitAlphabet(alphabet string) []string {
	out := make([]string, 0, len(alphabet))
	seen := make(map[string]struct{}, len(alphabet))
	for _, r := range alphabet {
		candidate := string(r)
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func timingSummary(samples []Sample) (int, int) {
	if len(samples) == 0 {
		return 0, 0
	}
	sum := 0
	minimum := samples[0].ElapsedMillis
	maximum := samples[0].ElapsedMillis
	for i, sample := range samples {
		sum += sample.ElapsedMillis
		if i == 0 {
			continue
		}
		if sample.ElapsedMillis < minimum {
			minimum = sample.ElapsedMillis
		}
		if sample.ElapsedMillis > maximum {
			maximum = sample.ElapsedMillis
		}
	}
	return sum / len(samples), maximum - minimum
}

func sumElapsed(samples []Sample) int {
	total := 0
	for _, sample := range samples {
		total += sample.ElapsedMillis
	}
	return total
}

func similarityPercent(a, b string) int {
	if a == b {
		return 100
	}
	ar := []rune(a)
	br := []rune(b)
	limit := len(ar)
	if len(br) < limit {
		limit = len(br)
	}
	prefix := 0
	for prefix < limit && ar[prefix] == br[prefix] {
		prefix++
	}
	denominator := len(ar)
	if len(br) > denominator {
		denominator = len(br)
	}
	if denominator == 0 {
		return 100
	}
	return prefix * 100 / denominator
}

func eliminateExact(candidates []string, probeChar string) []string {
	out := make([]string, 0, len(candidates))
	removed := false
	for _, candidate := range candidates {
		if !removed && candidate == probeChar {
			removed = true
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func maxInt(values ...int) int {
	best := values[0]
	for _, value := range values[1:] {
		if value > best {
			best = value
		}
	}
	return best
}

func cloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneSamples(in []Sample) []Sample {
	out := make([]Sample, len(in))
	copy(out, in)
	return out
}

func cloneEvidence(in []probeRecord) []probeRecord {
	out := make([]probeRecord, len(in))
	copy(out, in)
	return out
}

func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validSnapshotCase(sc snapshotCase) bool {
	if sc.CaseID == "" || sc.Point.ID == "" || sc.Point.Name == "" {
		return false
	}
	if sc.Baseline.CaseID != sc.CaseID || sc.Baseline.PointID != sc.Point.ID || !sc.Baseline.Stable {
		return false
	}
	if sc.State != StateInferring && sc.State != StateStopped && sc.State != StateComplete {
		return false
	}
	if sc.Position < 1 || sc.Requests < 0 || sc.ElapsedMillis < 0 {
		return false
	}
	if sc.Baseline.Samples < 1 || sc.Baseline.MeanMillis < 0 || sc.Baseline.JitterMillis < 0 {
		return false
	}
	if sc.Candidates == nil || sc.BaselineSamples == nil || sc.Evidence == nil {
		return false
	}
	return true
}

var (
	reUUID  = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reISO   = regexp.MustCompile(`\b\d{4}[-/]\d{1,2}[-/]\d{1,2}(?:[Tt ]+\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?\b`)
	reRFC   = regexp.MustCompile(`(?i)\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)?\s*(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}(?:st|nd|rd|th)?(?:,\s*|\s+)\d{4}(?:\s+\d{1,2}:\d{2}(?::\d{2})?)?\b`)
	reEpoch = regexp.MustCompile(`\b(?:1\d{9}|1\d{12}|2\d{9}|2\d{12})\b`)
	reHex   = regexp.MustCompile(`(?i)\b[0-9a-f]{16,}\b`)
	reToken = regexp.MustCompile(`(?i)\b(?:req|request|rid|trace|span|nonce|token|csrf)[-_][A-Za-z0-9._~-]{4,}\b`)
	reKeyed = regexp.MustCompile(`(?i)\b((?:["']?)(?:x-request-id|x-correlation-id|x-trace-id|request[ _-]?(?:id|no|number|token|time)|req[ _-]?(?:id|no|number|token)|trace[ _-]?(?:id|no|parent)|span[ _-]?id|correlation[ _-]?id|session[ _-]?id|transaction[ _-]?id|csrf[ _-]?token|access[ _-]?token|auth[ _-]?token|server[ _-]?time|response[ _-]?time|time[ _-]?stamp|timestamp|token|nonce|rid|sid|ts|date|now|etag|request|req|id)(?:["']?)[ \t]*[:=][ \t]*)(?:"[^"]*"|'[^']*'|[^\s,;&<>()]+)`)
	reClock = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?(?:\.\d+)?\b`)
)

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = reUUID.ReplaceAllString(body, volatileToken)
	body = reISO.ReplaceAllString(body, volatileToken)
	body = reRFC.ReplaceAllString(body, volatileToken)
	body = reEpoch.ReplaceAllString(body, volatileToken)
	body = reHex.ReplaceAllString(body, volatileToken)
	body = reToken.ReplaceAllString(body, volatileToken)
	body = reKeyed.ReplaceAllString(body, "${1}"+volatileToken)
	body = reClock.ReplaceAllString(body, volatileToken)
	return strings.TrimSpace(body)
}
