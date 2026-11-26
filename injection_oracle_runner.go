// Package task runs injection validation cases and judges them from two
// independent channels: echoed output and controlled latency.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"sort"
	"strings"
	"sync"
)

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
	// BaselineSamples optionally carries extra baseline timings collected
	// before judgement when the first baseline sample is noisy.
	BaselineSamples []int `json:"baseline_samples,omitempty"`
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

const snapshotVersion = 1

const (
	attemptPending   = "pending"
	attemptSubmitted = "submitted"
	attemptAborted   = "aborted"
)

type runnerSnapshot struct {
	Version        int                       `json:"version"`
	NextCaseSeq    int                       `json:"next_case_seq"`
	NextAttemptSeq int                       `json:"next_attempt_seq"`
	NextSetSeq     int                       `json:"next_set_seq"`
	Points         map[string]*pointRecord   `json:"points"`
	Cases          map[string]*caseRecord    `json:"cases"`
	Attempts       map[string]*attemptRecord `json:"attempts"`
}

type pointRecord struct {
	Key             string   `json:"key"`
	SetID           string   `json:"set_id"`
	CaseIDs         []string `json:"case_ids"`
	Submitted       int      `json:"submitted"`
	SpentMillis     int      `json:"spent_millis"`
	InFlightAttempt string   `json:"in_flight_attempt,omitempty"`
	RateLimited     bool     `json:"rate_limited,omitempty"`
}

type caseRecord struct {
	Case
	Payload        Payload  `json:"payload"`
	AttemptIDs     []string `json:"attempt_ids"`
	PendingAttempt string   `json:"pending_attempt,omitempty"`
	Outcome        *Outcome `json:"outcome,omitempty"`
	AbortReason    string   `json:"abort_reason,omitempty"`
}

type attemptRecord struct {
	ID          string    `json:"id"`
	CaseID      string    `json:"case_id"`
	Channel     string    `json:"channel"`
	Ordinal     int       `json:"ordinal"`
	Status      string    `json:"status"`
	Response    *Response `json:"response,omitempty"`
	RateLimited bool      `json:"rate_limited,omitempty"`
}

// InjectionOracleRunner owns the case sets, attempts and outcomes.
type InjectionOracleRunner struct {
	mu    sync.Mutex
	opts  Options
	state runnerSnapshot
}

// NewInjectionOracleRunner builds a runner with the given caps.
func NewInjectionOracleRunner(opts Options) *InjectionOracleRunner {
	return &InjectionOracleRunner{
		opts: opts,
		state: runnerSnapshot{
			Version:  snapshotVersion,
			Points:   make(map[string]*pointRecord),
			Cases:    make(map[string]*caseRecord),
			Attempts: make(map[string]*attemptRecord),
		},
	}
}

// Plan turns one injection point into a serial case set.
func (r *InjectionOracleRunner) Plan(point InjectionPoint) (CaseSet, error) {
	if strings.TrimSpace(point.Key) == "" || strings.TrimSpace(point.Param) == "" {
		return CaseSet{}, ErrBlankPoint
	}
	if len(point.Payloads) == 0 {
		return CaseSet{}, ErrBadPayload
	}
	for _, payload := range point.Payloads {
		if strings.TrimSpace(payload.Name) == "" ||
			strings.TrimSpace(payload.Value) == "" ||
			strings.TrimSpace(payload.Marker) == "" {
			return CaseSet{}, ErrBadPayload
		}
		if payload.DelayMillis < 0 {
			payload.DelayMillis = 0
		}
	}
	if point.SinkKind != "" && point.SinkKind != SinkCommand && point.SinkKind != SinkTemplate {
		return CaseSet{}, fmt.Errorf("%w: %q", ErrBadSink, point.SinkKind)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.state.Points[point.Key]; exists {
		return CaseSet{}, ErrPointExists
	}
	neededAttempts := len(point.Payloads) * AttemptsPerCase
	if neededAttempts > r.opts.MaxAttemptsPerPoint {
		return CaseSet{}, ErrAttemptLimit
	}
	if r.activePointsLocked() >= r.opts.MaxConcurrentPoints {
		return CaseSet{}, ErrPointLimit
	}

	r.state.NextSetSeq++
	setID := fmt.Sprintf("set-%d", r.state.NextSetSeq)
	pointRec := &pointRecord{
		Key:     point.Key,
		SetID:   setID,
		CaseIDs: make([]string, 0, len(point.Payloads)),
	}
	set := CaseSet{
		ID:              setID,
		PointKey:        point.Key,
		Cases:           make([]Case, 0, len(point.Payloads)),
		AttemptsPerCase: AttemptsPerCase,
	}

	for _, payload := range point.Payloads {
		r.state.NextCaseSeq++
		caseID := fmt.Sprintf("case-%d", r.state.NextCaseSeq)
		publicCase := Case{
			ID:          caseID,
			PointKey:    point.Key,
			PayloadName: payload.Name,
			State:       CasePlanned,
		}
		r.state.Cases[caseID] = &caseRecord{
			Case:    publicCase,
			Payload: payload,
		}
		pointRec.CaseIDs = append(pointRec.CaseIDs, caseID)
		set.Cases = append(set.Cases, publicCase)
	}
	r.state.Points[point.Key] = pointRec

	return set, nil
}

// Run hands out the next attempt of a case in channel order.
func (r *InjectionOracleRunner) Run(caseID string) (Attempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	caseRec, ok := r.state.Cases[caseID]
	if !ok {
		return Attempt{}, ErrUnknownCase
	}
	if caseRec.State == CaseAborted {
		return Attempt{}, ErrCaseAborted
	}
	if caseRec.PendingAttempt != "" {
		return Attempt{}, ErrAttemptPending
	}
	if len(caseRec.AttemptIDs) >= AttemptsPerCase {
		return Attempt{}, ErrCaseClosed
	}

	pointRec := r.state.Points[caseRec.PointKey]
	if pointRec == nil {
		return Attempt{}, ErrUnknownCase
	}
	if pointRec.InFlightAttempt != "" {
		return Attempt{}, ErrCasePending
	}
	// A detected throttle invalidates latency attribution, but does not turn
	// the serial state machine into an unrecoverable point-wide lock.
	if pointRec.SpentMillis >= r.opts.TotalDelayBudgetMillis {
		return Attempt{}, ErrDelayBudget
	}

	channel := channelForOrdinal(len(caseRec.AttemptIDs) + 1)
	r.state.NextAttemptSeq++
	attemptID := fmt.Sprintf("attempt-%d", r.state.NextAttemptSeq)
	attemptRec := &attemptRecord{
		ID:      attemptID,
		CaseID:  caseID,
		Channel: channel,
		Ordinal: len(caseRec.AttemptIDs) + 1,
		Status:  attemptPending,
	}
	r.state.Attempts[attemptID] = attemptRec
	caseRec.AttemptIDs = append(caseRec.AttemptIDs, attemptID)
	caseRec.PendingAttempt = attemptID
	caseRec.State = CaseRunning
	pointRec.InFlightAttempt = attemptID

	return Attempt{
		ID:      attemptID,
		CaseID:  caseID,
		Channel: channel,
		Ordinal: attemptRec.Ordinal,
		Payload: caseRec.Payload,
		Budget:  r.budgetLocked(pointRec),
	}, nil
}

// Submit records the response of one attempt.
func (r *InjectionOracleRunner) Submit(attemptID string, resp Response) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	attemptRec, ok := r.state.Attempts[attemptID]
	if !ok {
		return ErrUnknownAttempt
	}
	if resp.AttemptID != attemptID {
		return ErrBadResponse
	}
	if attemptRec.Status == attemptSubmitted {
		return ErrAttemptClosed
	}
	if attemptRec.Status == attemptAborted {
		return ErrCaseAborted
	}

	caseRec := r.state.Cases[attemptRec.CaseID]
	if caseRec == nil {
		return ErrUnknownCase
	}
	pointRec := r.state.Points[caseRec.PointKey]
	if pointRec == nil {
		return ErrUnknownCase
	}

	attemptRec.Status = attemptSubmitted
	responseCopy := resp
	attemptRec.Response = &responseCopy
	attemptRec.RateLimited = responseIndicatesRateLimit(resp)
	if attemptRec.RateLimited {
		pointRec.RateLimited = true
	} else if attemptRec.Channel == ChannelBaseline {
		// A fresh, non-throttled baseline is the recheck point after a
		// rate-limit pause and may resume latency attribution.
		pointRec.RateLimited = false
	}

	pointRec.Submitted++
	if resp.ElapsedMillis > 0 {
		pointRec.SpentMillis += resp.ElapsedMillis
	}
	if pointRec.InFlightAttempt == attemptID {
		pointRec.InFlightAttempt = ""
	}
	if caseRec.PendingAttempt == attemptID {
		caseRec.PendingAttempt = ""
	}

	return nil
}

// Judge evaluates a case from the echo and latency channels.
func (r *InjectionOracleRunner) Judge(caseID string) (Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	caseRec, ok := r.state.Cases[caseID]
	if !ok {
		return Outcome{}, ErrUnknownCase
	}
	if caseRec.State == CaseAborted {
		return Outcome{}, ErrCaseAborted
	}
	if caseRec.State == CaseJudged && caseRec.Outcome != nil {
		return *caseRec.Outcome, nil
	}
	if len(caseRec.AttemptIDs) < AttemptsPerCase {
		return Outcome{}, ErrChannelsPending
	}

	responses := make([]Response, AttemptsPerCase)
	for i, attemptID := range caseRec.AttemptIDs[:AttemptsPerCase] {
		attemptRec := r.state.Attempts[attemptID]
		if attemptRec == nil || attemptRec.Status != attemptSubmitted || attemptRec.Response == nil {
			return Outcome{}, ErrChannelsPending
		}
		responses[i] = *attemptRec.Response
	}

	baselineResp := responses[0]
	echoResp := responses[1]
	timeResp := responses[2]

	echoMatched := strings.Contains(normalizeEcho(echoResp.Body), normalizeEcho(caseRec.Payload.Marker))
	baselineMillis := reviewedBaselineMillis(baselineResp)
	observedMillis := timeResp.ElapsedMillis
	deltaMillis := observedMillis - baselineMillis
	thresholdMillis := max(TimeHitFloorMillis, caseRec.Payload.DelayMillis/2)
	latencyHit := deltaMillis >= thresholdMillis

	pointRec := r.state.Points[caseRec.PointKey]
	rateLimited := responseIndicatesRateLimit(baselineResp) ||
		responseIndicatesRateLimit(timeResp) ||
		(pointRec != nil && pointRec.RateLimited)
	if rateLimited {
		latencyHit = false
	}

	outcome := Outcome{
		CaseID:          caseID,
		PointKey:        caseRec.PointKey,
		EchoMatched:     echoMatched,
		BaselineMillis:  baselineMillis,
		ObservedMillis:  observedMillis,
		DeltaMillis:     deltaMillis,
		ThresholdMillis: thresholdMillis,
	}

	switch {
	case echoMatched && latencyHit:
		outcome.Hit = true
		outcome.Channel = ChannelBoth
		outcome.Confidence = ConfidenceHigh
		outcome.Reason = ReasonBothHit
	case echoMatched:
		outcome.Hit = true
		outcome.Channel = ChannelEcho
		outcome.Confidence = ConfidenceMedium
		outcome.Reason = ReasonEchoHit
	case latencyHit:
		outcome.Hit = true
		outcome.Channel = ChannelTime
		outcome.Confidence = ConfidenceMedium
		outcome.Reason = ReasonTimeHit
	default:
		outcome.Hit = false
		outcome.Channel = ""
		outcome.Confidence = ""
		outcome.Reason = ReasonNoHit
	}

	caseRec.Outcome = &outcome
	caseRec.State = CaseJudged
	for _, attemptID := range caseRec.AttemptIDs {
		if attemptRec := r.state.Attempts[attemptID]; attemptRec != nil {
			attemptRec.Response = nil
		}
	}

	return outcome, nil
}

// Abort closes a case with an operator supplied reason.
func (r *InjectionOracleRunner) Abort(caseID string, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrAbortReason
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	caseRec, ok := r.state.Cases[caseID]
	if !ok {
		return ErrUnknownCase
	}
	if caseRec.State == CaseAborted {
		return nil
	}

	caseRec.State = CaseAborted
	caseRec.AbortReason = reason
	if caseRec.PendingAttempt != "" {
		if attemptRec := r.state.Attempts[caseRec.PendingAttempt]; attemptRec != nil {
			attemptRec.Status = attemptAborted
		}
		caseRec.PendingAttempt = ""
	}
	if pointRec := r.state.Points[caseRec.PointKey]; pointRec != nil {
		if pointRec.InFlightAttempt != "" {
			if attemptRec := r.state.Attempts[pointRec.InFlightAttempt]; attemptRec != nil &&
				attemptRec.CaseID == caseID {
				pointRec.InFlightAttempt = ""
			}
		}
	}
	return nil
}

// Case reads one planned case.
func (r *InjectionOracleRunner) Case(caseID string) (Case, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	caseRec, ok := r.state.Cases[caseID]
	if !ok {
		return Case{}, false
	}
	return caseRec.Case, true
}

// Cases lists the cases of one injection point in planning order.
func (r *InjectionOracleRunner) Cases(pointKey string) []Case {
	r.mu.Lock()
	defer r.mu.Unlock()

	pointRec, ok := r.state.Points[pointKey]
	if !ok {
		return []Case{}
	}
	out := make([]Case, 0, len(pointRec.CaseIDs))
	for _, caseID := range pointRec.CaseIDs {
		if caseRec := r.state.Cases[caseID]; caseRec != nil {
			out = append(out, caseRec.Case)
		}
	}
	return out
}

// Budget reads the remaining allowance of a case set.
func (r *InjectionOracleRunner) Budget(caseID string) (BudgetView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	caseRec, ok := r.state.Cases[caseID]
	if !ok {
		return BudgetView{}, ErrUnknownCase
	}
	pointRec := r.state.Points[caseRec.PointKey]
	if pointRec == nil {
		return BudgetView{}, ErrUnknownCase
	}
	return r.budgetLocked(pointRec), nil
}

// Snapshot serialises plans, attempts, responses and outcomes.
func (r *InjectionOracleRunner) Snapshot() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := runnerSnapshot{
		Version:        snapshotVersion,
		NextCaseSeq:    r.state.NextCaseSeq,
		NextAttemptSeq: r.state.NextAttemptSeq,
		NextSetSeq:     r.state.NextSetSeq,
		Points:         r.state.Points,
		Cases:          r.state.Cases,
		Attempts:       r.state.Attempts,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *InjectionOracleRunner) Restore(data []byte) error {
	var restored runnerSnapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&restored); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = restored
	return nil
}

func (r *InjectionOracleRunner) budgetLocked(pointRec *pointRecord) BudgetView {
	return BudgetView{
		AttemptsLeft: r.opts.MaxAttemptsPerPoint - pointRec.Submitted,
		MillisLeft:   r.opts.TotalDelayBudgetMillis - pointRec.SpentMillis,
	}
}

func (r *InjectionOracleRunner) activePointsLocked() int {
	active := 0
	for _, pointRec := range r.state.Points {
		for _, caseID := range pointRec.CaseIDs {
			caseRec := r.state.Cases[caseID]
			if caseRec == nil {
				continue
			}
			if caseRec.State != CaseJudged && caseRec.State != CaseAborted {
				active++
				break
			}
		}
	}
	return active
}

func channelForOrdinal(ordinal int) string {
	switch ordinal {
	case 1:
		return ChannelBaseline
	case 2:
		return ChannelEcho
	default:
		return ChannelTime
	}
}

func normalizeEcho(body string) string {
	normalized := html.UnescapeString(body)
	for i := 0; i < 2; i++ {
		next := html.UnescapeString(normalized)
		if next == normalized {
			break
		}
		normalized = next
	}
	return strings.Join(strings.Fields(normalized), " ")
}

func reviewedBaselineMillis(resp Response) int {
	if len(resp.BaselineSamples) == 0 {
		return resp.ElapsedMillis
	}
	samples := make([]int, 0, len(resp.BaselineSamples)+1)
	for _, sample := range resp.BaselineSamples {
		if sample >= 0 {
			samples = append(samples, sample)
		}
	}
	if resp.ElapsedMillis >= 0 {
		samples = append(samples, resp.ElapsedMillis)
	}
	if len(samples) == 0 {
		return resp.ElapsedMillis
	}
	sort.Ints(samples)
	return samples[(len(samples)-1)/2]
}

func responseIndicatesRateLimit(resp Response) bool {
	if resp.RateLimited || resp.StatusCode == 429 {
		return true
	}
	body := strings.ToLower(normalizeEcho(resp.Body))
	for _, marker := range []string{"rate limit", "rate-limit", "too many requests", "限流"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func validateSnapshot(snapshot *runnerSnapshot) error {
	if snapshot.Version != snapshotVersion {
		return errors.New("unsupported snapshot version")
	}
	if snapshot.Points == nil || snapshot.Cases == nil || snapshot.Attempts == nil {
		return errors.New("missing state maps")
	}
	for pointKey, pointRec := range snapshot.Points {
		if pointRec == nil || pointRec.Key != pointKey {
			return errors.New("invalid point record")
		}
		for _, caseID := range pointRec.CaseIDs {
			caseRec := snapshot.Cases[caseID]
			if caseRec == nil || caseRec.ID != caseID || caseRec.PointKey != pointKey {
				return errors.New("invalid point case reference")
			}
		}
		if pointRec.InFlightAttempt != "" {
			attemptRec := snapshot.Attempts[pointRec.InFlightAttempt]
			if attemptRec == nil || attemptRec.Status != attemptPending {
				return errors.New("invalid in-flight attempt")
			}
		}
	}
	for caseID, caseRec := range snapshot.Cases {
		if caseRec == nil || caseRec.ID != caseID {
			return errors.New("invalid case record")
		}
		if _, ok := snapshot.Points[caseRec.PointKey]; !ok {
			return errors.New("case references unknown point")
		}
		if caseRec.PendingAttempt != "" {
			attemptRec := snapshot.Attempts[caseRec.PendingAttempt]
			if attemptRec == nil || attemptRec.Status != attemptPending || attemptRec.CaseID != caseID {
				return errors.New("invalid pending attempt")
			}
		}
		for i, attemptID := range caseRec.AttemptIDs {
			attemptRec := snapshot.Attempts[attemptID]
			if attemptRec == nil || attemptRec.CaseID != caseID || attemptRec.Ordinal != i+1 {
				return errors.New("invalid case attempt reference")
			}
		}
	}
	for attemptID, attemptRec := range snapshot.Attempts {
		if attemptRec == nil || attemptRec.ID != attemptID {
			return errors.New("invalid attempt record")
		}
		if _, ok := snapshot.Cases[attemptRec.CaseID]; !ok {
			return errors.New("attempt references unknown case")
		}
	}
	return nil
}
