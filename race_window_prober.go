// Package task drives window aligned duplicate requests at a suspicious endpoint
// and turns the before and after difference into an explainable race verdict.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lifecycle states of one probe.
const (
	StatePlanned   = "planned"
	StateFiring    = "firing"
	StateFired     = "fired"
	StateCancelled = "cancelled"
)

// Verdict results of one verified probe.
const (
	ResultConfirmed = "confirmed"
	ResultSafe      = "safe"
	ResultUncertain = "uncertain"
)

// Reason codes carried by a verdict.
const (
	ReasonDuplicateEffects = "reason_duplicate_side_effects"
	ReasonSingleEffect     = "reason_single_effect"
	ReasonIncompleteWindow = "reason_incomplete_window"
)

// Errors returned by the prober.
var (
	ErrBadOperation     = errors.New("operation key, endpoint or request count is invalid")
	ErrRequestLimit     = errors.New("request count above the probe cap")
	ErrConcurrencyLimit = errors.New("concurrency above the request count or the cap")
	ErrRetryLimit       = errors.New("retry count above the cap")
	ErrProbeLimit       = errors.New("probe cap reached")
	ErrUnknownProbe     = errors.New("unknown probe")
	ErrProbeInFlight    = errors.New("another probe of the same business key is in flight")
	ErrAlreadyFired     = errors.New("probe already fired")
	ErrProbeCancelled   = errors.New("probe was cancelled")
	ErrNotFired         = errors.New("probe has not been fired")
	ErrNoTransport      = errors.New("no transport attached")
	ErrCooldown         = errors.New("target is inside the cooldown window")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

const snapshotVersion = 1

// Config bounds one prober instance.
type Config struct {
	MaxRequests    int
	MaxConcurrency int
	MaxRetries     int
	MaxProbes      int
	Cooldown       time.Duration
}

// Operation is the business action under test. Every probe of one Operation
// replays the same action, and IdempotencyKey mirrors whatever the client
// already sends so the server side behaviour stays unchanged.
type Operation struct {
	Key            string
	Endpoint       string
	Method         string
	Body           string
	Requests       int
	Concurrency    int
	Retries        int
	IdempotencyKey string
}

// Probe is the frozen plan of one window.
type Probe struct {
	ProbeID     string    `json:"probe_id"`
	Key         string    `json:"key"`
	Endpoint    string    `json:"endpoint"`
	Planned     int       `json:"planned"`
	Concurrency int       `json:"concurrency"`
	Retries     int       `json:"retries"`
	State       string    `json:"state"`
	FiredAt     time.Time `json:"fired_at"`
}

// Request is one HTTP level call issued by Fire.
type Request struct {
	ProbeID  string `json:"probe_id"`
	Ordinal  int    `json:"ordinal"`
	Attempt  int    `json:"attempt"`
	Key      string `json:"key"`
	Endpoint string `json:"endpoint"`
	Body     string `json:"body"`
}

// Response is what the endpoint answered. Applied says whether the server
// executed the business action on this call.
type Response struct {
	Status  int    `json:"status"`
	Body    string `json:"body"`
	Applied bool   `json:"applied"`
}

// Outcome is the recorded result of one request.
type Outcome struct {
	Ordinal int    `json:"ordinal"`
	Attempt int    `json:"attempt"`
	Status  int    `json:"status"`
	Applied bool   `json:"applied"`
	Err     string `json:"err"`
}

// Verdict is the conclusion drawn from the state difference of one probe.
type Verdict struct {
	VerdictID  string `json:"verdict_id"`
	ProbeID    string `json:"probe_id"`
	Key        string `json:"key"`
	Result     string `json:"result"`
	Executions int    `json:"executions"`
	Duplicates int    `json:"duplicates"`
	Attempts   int    `json:"attempts"`
	Reason     string `json:"reason"`
}

// Transport is the endpoint driver injected by the caller. Send performs one
// call, Counter returns the current server side side effect count for a
// business key.
type Transport interface {
	Send(req Request) (Response, error)
	Counter(key string) int
}

// RaceWindowProber owns the planned probes, their outcomes and their verdicts.
type RaceWindowProber struct {
	cfg Config

	mu        sync.RWMutex
	transport Transport
	now       func() time.Time

	nextProbeSeq   int
	nextVerdictSeq int
	probes         map[string]*probeRecord
	order          []string
	attempts       map[string]int
	inFlight       map[string]string
	cooldownUntil  map[string]time.Time
}

type probeRecord struct {
	Probe         Probe     `json:"probe"`
	Operation     Operation `json:"operation"`
	Outcomes      []Outcome `json:"outcomes,omitempty"`
	BeforeCounter int       `json:"before_counter"`
	AfterCounter  int       `json:"after_counter"`
	CounterReady  bool      `json:"counter_ready"`
	AttemptTotal  int       `json:"attempt_total"`
	RetryApplied  bool      `json:"retry_applied"`
	Verdict       *Verdict  `json:"verdict,omitempty"`
}

type snapshotData struct {
	Version        int                     `json:"version"`
	NextProbeSeq   int                     `json:"next_probe_seq"`
	NextVerdictSeq int                     `json:"next_verdict_seq"`
	Order          []string                `json:"order"`
	Probes         map[string]*probeRecord `json:"probes"`
	Attempts       map[string]int          `json:"attempts"`
	CooldownUntil  map[string]time.Time    `json:"cooldown_until,omitempty"`
}

// NewRaceWindowProber builds a prober with the given caps.
func NewRaceWindowProber(cfg Config) *RaceWindowProber {
	return &RaceWindowProber{
		cfg:            cfg,
		now:            time.Now,
		nextProbeSeq:   1,
		nextVerdictSeq: 1,
		probes:         make(map[string]*probeRecord),
		attempts:       make(map[string]int),
		inFlight:       make(map[string]string),
		cooldownUntil:  make(map[string]time.Time),
	}
}

// SetTransport attaches the endpoint driver.
func (p *RaceWindowProber) SetTransport(t Transport) error {
	if t == nil {
		return fmt.Errorf("%w: transport is nil", ErrNoTransport)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.transport = t
	return nil
}

// SetClock replaces the clock used for cooldown decisions.
func (p *RaceWindowProber) SetClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now == nil {
		p.now = time.Now
		return
	}
	p.now = now
}

// Plan validates an operation and freezes it into a probe.
func (p *RaceWindowProber) Plan(op Operation) (Probe, error) {
	if err := p.cfg.validateOperation(op); err != nil {
		return Probe{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.order) >= p.cfg.MaxProbes {
		return Probe{}, ErrProbeLimit
	}

	id := fmt.Sprintf("probe-%d", p.nextProbeSeq)
	p.nextProbeSeq++
	probe := Probe{
		ProbeID:     id,
		Key:         op.Key,
		Endpoint:    op.Endpoint,
		Planned:     op.Requests,
		Concurrency: op.Concurrency,
		Retries:     op.Retries,
		State:       StatePlanned,
	}
	p.probes[id] = &probeRecord{Probe: probe, Operation: op}
	p.order = append(p.order, id)
	return probe, nil
}

// Fire issues the planned requests inside one window and records the outcomes.
func (p *RaceWindowProber) Fire(probeID string) ([]Outcome, error) {
	startedAt := p.clockNow()

	p.mu.Lock()
	transport := p.transport
	if transport == nil {
		p.mu.Unlock()
		return nil, ErrNoTransport
	}
	rec, ok := p.probes[probeID]
	if !ok {
		p.mu.Unlock()
		return nil, ErrUnknownProbe
	}

	op := rec.Operation
	switch rec.Probe.State {
	case StateCancelled:
		p.mu.Unlock()
		return nil, ErrProbeCancelled
	case StateFired, StateFiring:
		p.mu.Unlock()
		return nil, ErrAlreadyFired
	}
	if owner := p.inFlight[op.Key]; owner != "" {
		p.mu.Unlock()
		return nil, ErrProbeInFlight
	}
	if err := p.cooldownLocked(op.Key, startedAt); err != nil {
		p.mu.Unlock()
		return nil, err
	}

	rec.Probe.State = StateFiring
	rec.Probe.FiredAt = startedAt
	p.inFlight[op.Key] = probeID
	planned := rec.Probe
	p.mu.Unlock()

	defer p.releaseInFlight(probeID, op.Key)

	before, beforeReady := safeCounter(transport, op.Key)
	outcomes, attemptTotal, retryApplied := sendWindow(transport, planned, op)
	after, afterReady := safeCounter(transport, op.Key)
	completedAt := p.clockNow()

	p.completeFire(fireCompletion{
		probeID:       probeID,
		key:           op.Key,
		record:        rec,
		outcomes:      outcomes,
		before:        before,
		after:         after,
		countersReady: beforeReady && afterReady,
		attempts:      attemptTotal,
		retryApplied:  retryApplied,
		completedAt:   completedAt,
	})

	return cloneOutcomes(outcomes), nil
}

// Verify turns the recorded state difference of one probe into a verdict.
func (p *RaceWindowProber) Verify(probeID string) (Verdict, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	rec, ok := p.probes[probeID]
	if !ok {
		return Verdict{}, ErrUnknownProbe
	}
	if rec.Probe.State != StateFired {
		return Verdict{}, ErrNotFired
	}
	if rec.Verdict != nil {
		return *rec.Verdict, nil
	}

	verdict := buildVerdict(rec, p.nextVerdictSeq)
	rec.Verdict = &verdict
	p.nextVerdictSeq++
	return verdict, nil
}

// Cancel drops a probe that has not been fired yet.
func (p *RaceWindowProber) Cancel(probeID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rec, ok := p.probes[probeID]
	if !ok {
		return ErrUnknownProbe
	}
	switch rec.Probe.State {
	case StateCancelled:
		return nil
	case StatePlanned:
		rec.Probe.State = StateCancelled
		return nil
	default:
		return ErrAlreadyFired
	}
}

// State reads one probe.
func (p *RaceWindowProber) State(probeID string) (Probe, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rec, ok := p.probes[probeID]
	if !ok {
		return Probe{}, false
	}
	return rec.Probe, true
}

// Probes lists the known probe ids in creation order.
func (p *RaceWindowProber) Probes() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.order...)
}

// Attempts counts the requests actually issued for one business key.
func (p *RaceWindowProber) Attempts(key string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.attempts[key]
}

// Pending counts the probes that were fired but not verified yet.
func (p *RaceWindowProber) Pending() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pending := 0
	for _, id := range p.order {
		rec := p.probes[id]
		if rec == nil || rec.Verdict != nil {
			continue
		}
		if rec.Probe.State == StateFiring || rec.Probe.State == StateFired {
			pending++
		}
	}
	return pending
}

// Snapshot serialises probes, outcomes, verdicts and the attempt counters.
func (p *RaceWindowProber) Snapshot() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	probes := make(map[string]*probeRecord, len(p.probes))
	for id, rec := range p.probes {
		copyRecord := *rec
		copyRecord.Outcomes = cloneOutcomes(rec.Outcomes)
		if rec.Verdict != nil {
			verdict := *rec.Verdict
			copyRecord.Verdict = &verdict
		}
		probes[id] = &copyRecord
	}

	attempts := make(map[string]int, len(p.attempts))
	for key, count := range p.attempts {
		attempts[key] = count
	}
	cooldown := make(map[string]time.Time, len(p.cooldownUntil))
	for key, until := range p.cooldownUntil {
		cooldown[key] = until
	}

	snapshot := snapshotData{
		Version:        snapshotVersion,
		NextProbeSeq:   p.nextProbeSeq,
		NextVerdictSeq: p.nextVerdictSeq,
		Order:          append(make([]string, 0, len(p.order)), p.order...),
		Probes:         probes,
		Attempts:       attempts,
		CooldownUntil:  cooldown,
	}
	return json.Marshal(snapshot)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *RaceWindowProber) Restore(data []byte) error {
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.nextProbeSeq = snapshot.NextProbeSeq
	p.nextVerdictSeq = snapshot.NextVerdictSeq
	p.probes = snapshot.Probes
	p.order = append([]string(nil), snapshot.Order...)
	p.attempts = snapshot.Attempts
	p.cooldownUntil = snapshot.CooldownUntil
	if p.cooldownUntil == nil {
		p.cooldownUntil = make(map[string]time.Time)
	}
	p.inFlight = make(map[string]string)
	return nil
}

func (c Config) validateOperation(op Operation) error {
	if strings.TrimSpace(op.Key) == "" || strings.TrimSpace(op.Endpoint) == "" || op.Requests < 2 {
		return ErrBadOperation
	}
	if op.Requests > c.MaxRequests {
		return ErrRequestLimit
	}
	if op.Concurrency < 1 || op.Concurrency > op.Requests || op.Concurrency > c.MaxConcurrency {
		return ErrConcurrencyLimit
	}
	if op.Retries < 0 || op.Retries > c.MaxRetries {
		return ErrRetryLimit
	}
	return nil
}

func (p *RaceWindowProber) clockNow() time.Time {
	p.mu.RLock()
	now := p.now
	p.mu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

func (p *RaceWindowProber) cooldownLocked(key string, now time.Time) error {
	if until, ok := p.cooldownUntil[key]; ok && now.Before(until) {
		return ErrCooldown
	}
	return nil
}

func (p *RaceWindowProber) releaseInFlight(probeID, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner := p.inFlight[key]; owner == probeID {
		delete(p.inFlight, key)
	}
}

type fireCompletion struct {
	probeID       string
	key           string
	record        *probeRecord
	outcomes      []Outcome
	before        int
	after         int
	countersReady bool
	attempts      int
	retryApplied  bool
	completedAt   time.Time
}

func (p *RaceWindowProber) completeFire(completion fireCompletion) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if current, ok := p.probes[completion.probeID]; ok && current == completion.record {
		completion.record.Outcomes = cloneOutcomes(completion.outcomes)
		completion.record.BeforeCounter = completion.before
		completion.record.AfterCounter = completion.after
		completion.record.CounterReady = completion.countersReady
		completion.record.AttemptTotal = completion.attempts
		completion.record.RetryApplied = completion.retryApplied
		completion.record.Probe.State = StateFired
	}

	p.attempts[completion.key] += completion.attempts
	if p.cfg.Cooldown > 0 {
		p.cooldownUntil[completion.key] = completion.completedAt.Add(p.cfg.Cooldown)
	}
	if owner := p.inFlight[completion.key]; owner == completion.probeID {
		delete(p.inFlight, completion.key)
	}
}

func sendWindow(transport Transport, probe Probe, op Operation) ([]Outcome, int, bool) {
	outcomes := make([]Outcome, probe.Planned)
	jobs := make(chan int)
	var wg sync.WaitGroup
	var attemptTotal atomic.Int64
	var retryApplied atomic.Bool

	for worker := 0; worker < probe.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ordinal := range jobs {
				outcomes[ordinal-1] = executeOrdinal(transport, probe.ProbeID, op, ordinal, probe.Retries, &attemptTotal, &retryApplied)
			}
		}()
	}

	for ordinal := 1; ordinal <= probe.Planned; ordinal++ {
		jobs <- ordinal
	}
	close(jobs)
	wg.Wait()

	sort.SliceStable(outcomes, func(i, j int) bool {
		return outcomes[i].Ordinal < outcomes[j].Ordinal
	})
	return outcomes, int(attemptTotal.Load()), retryApplied.Load()
}

func executeOrdinal(
	transport Transport,
	probeID string,
	op Operation,
	ordinal int,
	retries int,
	attemptTotal *atomic.Int64,
	retryApplied *atomic.Bool,
) Outcome {
	var outcome Outcome
	for attempt := 1; attempt <= retries+1; attempt++ {
		request := Request{
			ProbeID:  probeID,
			Ordinal:  ordinal,
			Attempt:  attempt,
			Key:      op.Key,
			Endpoint: op.Endpoint,
			Body:     op.Body,
		}
		attemptTotal.Add(1)
		response, err := safeSend(transport, request)

		outcome = Outcome{
			Ordinal: ordinal,
			Attempt: attempt,
			Status:  response.Status,
			Applied: response.Applied,
		}
		if err != nil {
			outcome.Err = err.Error()
		} else if response.Status != 0 && (response.Status < 200 || response.Status >= 300) {
			outcome.Err = fmt.Sprintf("transport returned unsuccessful status %d", response.Status)
		}
		if attempt > 1 && response.Applied {
			retryApplied.Store(true)
		}
		if outcome.Err == "" {
			break
		}
	}
	return outcome
}

func safeSend(transport Transport, request Request) (response Response, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = Response{}
			err = fmt.Errorf("transport send panic: %v", recovered)
		}
	}()
	return transport.Send(request)
}

func safeCounter(transport Transport, key string) (count int, ready bool) {
	defer func() {
		if recover() != nil {
			count = 0
			ready = false
		}
	}()
	return transport.Counter(key), true
}

func buildVerdict(rec *probeRecord, sequence int) Verdict {
	delta := rec.AfterCounter - rec.BeforeCounter
	verdict := Verdict{
		VerdictID:  fmt.Sprintf("verdict-%d", sequence),
		ProbeID:    rec.Probe.ProbeID,
		Key:        rec.Probe.Key,
		Executions: delta,
		Attempts:   rec.AttemptTotal,
	}

	incomplete := rec.RetryApplied || !rec.CounterReady
	for _, outcome := range rec.Outcomes {
		if outcome.Err != "" {
			incomplete = true
			break
		}
	}

	switch {
	case incomplete || delta <= 0:
		verdict.Result = ResultUncertain
		verdict.Reason = ReasonIncompleteWindow
	case delta > 1:
		verdict.Result = ResultConfirmed
		verdict.Reason = ReasonDuplicateEffects
		verdict.Duplicates = delta - 1
	default:
		verdict.Result = ResultSafe
		verdict.Reason = ReasonSingleEffect
	}
	return verdict
}

func cloneOutcomes(outcomes []Outcome) []Outcome {
	if outcomes == nil {
		return nil
	}
	cloned := make([]Outcome, len(outcomes))
	copy(cloned, outcomes)
	return cloned
}

func decodeSnapshot(data []byte) (snapshotData, error) {
	var snapshot snapshotData
	if len(bytes.TrimSpace(data)) == 0 {
		return snapshot, fmt.Errorf("%w: empty snapshot", ErrBadSnapshot)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return snapshot, fmt.Errorf("%w: multiple JSON values", ErrBadSnapshot)
		}
		return snapshot, fmt.Errorf("%w: trailing snapshot data: %v", ErrBadSnapshot, err)
	}
	if err := snapshot.validate(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *snapshotData) validate() error {
	if s.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if s.NextProbeSeq < 1 || s.NextVerdictSeq < 1 || s.Probes == nil || s.Attempts == nil {
		return fmt.Errorf("%w: missing sequence or state", ErrBadSnapshot)
	}
	if s.Order == nil {
		s.Order = []string{}
	}
	if len(s.Order) != len(s.Probes) {
		return fmt.Errorf("%w: probe index mismatch", ErrBadSnapshot)
	}

	seen := make(map[string]bool, len(s.Order))
	attemptsByKey := make(map[string]int)
	maxProbeSeq := 0
	maxVerdictSeq := 0

	for _, id := range s.Order {
		if id == "" || seen[id] {
			return fmt.Errorf("%w: duplicate or empty probe id", ErrBadSnapshot)
		}
		seen[id] = true

		probeSeq, err := canonicalSequence(id, "probe")
		if err != nil {
			return err
		}
		maxProbeSeq = max(maxProbeSeq, probeSeq)

		rec := s.Probes[id]
		if rec == nil {
			return fmt.Errorf("%w: missing probe %s", ErrBadSnapshot, id)
		}
		if err := rec.validate(id, attemptsByKey, &maxVerdictSeq); err != nil {
			return err
		}
	}

	for id := range s.Probes {
		if !seen[id] {
			return fmt.Errorf("%w: unindexed probe %s", ErrBadSnapshot, id)
		}
	}
	for key, count := range s.Attempts {
		if count < 0 || attemptsByKey[key] != count {
			return fmt.Errorf("%w: attempt counter mismatch for %s", ErrBadSnapshot, key)
		}
	}
	for key, count := range attemptsByKey {
		if count < 0 || s.Attempts[key] != count {
			return fmt.Errorf("%w: missing attempt counter for %s", ErrBadSnapshot, key)
		}
	}

	s.NextProbeSeq = max(s.NextProbeSeq, maxProbeSeq+1)
	s.NextVerdictSeq = max(s.NextVerdictSeq, maxVerdictSeq+1)
	return nil
}

func (r *probeRecord) validate(id string, attemptsByKey map[string]int, maxVerdictSeq *int) error {
	if r.Probe.ProbeID != id {
		return fmt.Errorf("%w: probe id mismatch", ErrBadSnapshot)
	}
	op := r.Operation
	if strings.TrimSpace(op.Key) == "" || strings.TrimSpace(op.Endpoint) == "" || op.Requests < 2 {
		return fmt.Errorf("%w: invalid operation for %s", ErrBadSnapshot, id)
	}
	if r.Probe.Key != op.Key || r.Probe.Endpoint != op.Endpoint || r.Probe.Planned != op.Requests ||
		r.Probe.Concurrency != op.Concurrency || r.Probe.Retries != op.Retries {
		return fmt.Errorf("%w: frozen probe mismatch for %s", ErrBadSnapshot, id)
	}
	switch r.Probe.State {
	case StatePlanned, StateFiring, StateFired, StateCancelled:
	default:
		return fmt.Errorf("%w: invalid state for %s", ErrBadSnapshot, id)
	}

	if r.AttemptTotal < 0 {
		return fmt.Errorf("%w: negative attempt total for %s", ErrBadSnapshot, id)
	}
	if r.Probe.State == StateFired {
		if len(r.Outcomes) != op.Requests || r.AttemptTotal < op.Requests {
			return fmt.Errorf("%w: incomplete outcomes for %s", ErrBadSnapshot, id)
		}
		seenOrdinals := make(map[int]bool, op.Requests)
		for index, outcome := range r.Outcomes {
			if outcome.Ordinal != index+1 || seenOrdinals[outcome.Ordinal] || outcome.Attempt < 1 {
				return fmt.Errorf("%w: invalid outcome order for %s", ErrBadSnapshot, id)
			}
			seenOrdinals[outcome.Ordinal] = true
		}
		attemptsByKey[op.Key] += r.AttemptTotal
	} else if len(r.Outcomes) != 0 || r.Verdict != nil {
		return fmt.Errorf("%w: unfinished probe has terminal data for %s", ErrBadSnapshot, id)
	}

	if r.Verdict != nil {
		verdict := *r.Verdict
		verdictSeq, err := canonicalSequence(verdict.VerdictID, "verdict")
		if err != nil {
			return err
		}
		*maxVerdictSeq = max(*maxVerdictSeq, verdictSeq)
		if verdict.ProbeID != id || verdict.Key != op.Key {
			return fmt.Errorf("%w: verdict mismatch for %s", ErrBadSnapshot, id)
		}
		switch verdict.Result {
		case ResultConfirmed, ResultSafe, ResultUncertain:
		default:
			return fmt.Errorf("%w: invalid verdict result for %s", ErrBadSnapshot, id)
		}
		switch verdict.Reason {
		case ReasonDuplicateEffects, ReasonSingleEffect, ReasonIncompleteWindow:
		default:
			return fmt.Errorf("%w: invalid verdict reason for %s", ErrBadSnapshot, id)
		}
	}
	return nil
}

func canonicalSequence(id, prefix string) (int, error) {
	prefixValue := prefix + "-"
	if !strings.HasPrefix(id, prefixValue) {
		return 0, fmt.Errorf("%w: invalid %s id %q", ErrBadSnapshot, prefix, id)
	}
	raw := strings.TrimPrefix(id, prefixValue)
	sequence, err := strconv.Atoi(raw)
	if err != nil || sequence <= 0 || raw != strconv.Itoa(sequence) {
		return 0, fmt.Errorf("%w: invalid %s id %q", ErrBadSnapshot, prefix, id)
	}
	return sequence, nil
}
