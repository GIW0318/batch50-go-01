// Package task consumes managed host process events, folds the attach, remote
// write and remote thread steps into one injection chain per target and blocks
// or alerts on the chain that crosses the configured thresholds.
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
	"time"
)

const snapshotVersion = 1

// Event kinds consumed by the sentinel.
const (
	KindAttach       = "ptrace_attach"
	KindWriteVM      = "process_vm_write"
	KindRemoteThread = "remote_thread"
	KindExec         = "exec"
)

// Verdict actions.
const (
	ActionAllow = "allow"
	ActionAlert = "alert"
	ActionBlock = "block"
)

// Reason codes attached to a verdict.
const (
	ReasonScoreLow   = "score_low"
	ReasonChainAlert = "chain_alert"
	ReasonChainBlock = "chain_block"
	ReasonAllowRule  = "allow_rule"
	ReasonPriorBlock = "prior_block"
	ReasonSuppressed = "suppressed"
)

// Step weights and the two decision thresholds.
const (
	ScoreAttach       = 30
	ScoreWriteVM      = 40
	ScoreRemoteThread = 30
	ScoreExec         = 10
	AlertThreshold    = 30
	BlockThreshold    = 70
)

// Event is one process event reported by the host agent. TargetStart identifies
// the process instance so that a recycled pid is not mistaken for the old one.
type Event struct {
	PID         int
	TargetPID   int
	Kind        string
	TS          int64
	ParentPID   int
	Signer      string
	TargetStart int64
}

// AllowRule exempts one kind of legitimate debugger attach from scoring.
type AllowRule struct {
	ID        string
	Name      string
	ParentPID int
	Signer    string
	ExpiresAt int64
}

// RuleID identifies one registered allow rule.
type RuleID string

// Verdict is the explainable outcome of one ingest call.
type Verdict struct {
	PID        int
	TargetPID  int
	Action     string
	Score      int
	Chain      []string
	Reason     string
	Blocked    bool
	Duplicate  bool
	Suppressed bool
	Seq        int64
}

// Trace is the per target event and verdict history.
type Trace struct {
	PID      int
	Events   []Event
	Verdicts []Verdict
}

// Config bounds the per target buffer, the rule table and the chain length.
type Config struct {
	MaxEventsPerTarget int
	MaxRules           int
	MaxChainSteps      int
	Window             int64
}

// Errors returned by the sentinel.
var (
	ErrBadEvent       = errors.New("process event is invalid")
	ErrBadRule        = errors.New("allow rule is invalid")
	ErrRuleLimit      = errors.New("allow rule limit reached")
	ErrUnknownRule    = errors.New("unknown allow rule")
	ErrBadSuppression = errors.New("suppression window is invalid")
	ErrNoTrace        = errors.New("no trace for the target")
	ErrUnknownBlock   = errors.New("target is not blocked")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// InjectionSentinel owns the rule table, the per target buffers and the block
// list.
type InjectionSentinel struct {
	mu            sync.Mutex
	cfg           Config
	seq           int64
	ruleSeq       int64
	rules         []AllowRule
	suppressUntil map[string]time.Time
	targets       map[int]*targetState
}

type targetState struct {
	PID         int                `json:"pid"`
	TargetStart int64              `json:"target_start"`
	Events      []Event            `json:"events,omitempty"`
	Verdicts    []Verdict          `json:"verdicts,omitempty"`
	Seen        map[string]Verdict `json:"seen,omitempty"`
	StepTimes   map[string][]int64 `json:"step_times,omitempty"`
	Chain       []string           `json:"chain,omitempty"`
	Score       int                `json:"score"`
	Blocked     bool               `json:"blocked"`
}

type blockSnapshot struct {
	PID         int   `json:"pid"`
	TargetStart int64 `json:"target_start"`
}

type snapshotData struct {
	Version       int                      `json:"version"`
	Seq           int64                    `json:"seq"`
	RuleSeq       int64                    `json:"rule_seq"`
	Rules         []AllowRule              `json:"rules"`
	SuppressTTL   map[string]time.Duration `json:"suppress_ttl,omitempty"`
	SuppressUntil map[string]time.Time     `json:"suppress_until,omitempty"`
	Targets       []targetState            `json:"targets"`
	Blocks        []blockSnapshot          `json:"blocks"`
}

// NewInjectionSentinel builds a sentinel with the given caps.
func NewInjectionSentinel(cfg Config) *InjectionSentinel {
	if cfg.MaxEventsPerTarget < 0 {
		cfg.MaxEventsPerTarget = 0
	}
	if cfg.MaxRules < 0 {
		cfg.MaxRules = 0
	}
	if cfg.MaxChainSteps < 0 {
		cfg.MaxChainSteps = 0
	}
	if cfg.Window < 0 {
		cfg.Window = 0
	}
	return &InjectionSentinel{
		cfg:           cfg,
		suppressUntil: make(map[string]time.Time),
		targets:       make(map[int]*targetState),
	}
}

// Ingest scores one event against the chain of its target.
func (s *InjectionSentinel) Ingest(ev Event) (Verdict, error) {
	if err := validateEvent(ev); err != nil {
		return Verdict{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.targets[ev.TargetPID]
	if st == nil || st.TargetStart != ev.TargetStart {
		st = &targetState{
			PID:         ev.TargetPID,
			TargetStart: ev.TargetStart,
			Seen:        make(map[string]Verdict),
			StepTimes:   make(map[string][]int64),
		}
		s.targets[ev.TargetPID] = st
	}

	key := eventKey(ev)
	if previous, ok := st.Seen[key]; ok {
		duplicate := cloneVerdict(previous)
		duplicate.Duplicate = true
		return duplicate, nil
	}

	s.seq++
	seq := s.seq

	st.Events = append(st.Events, ev)
	st.trimEvents(s.cfg.MaxEventsPerTarget)

	verdict := Verdict{
		PID:       ev.PID,
		TargetPID: ev.TargetPID,
		Seq:       seq,
	}

	switch {
	case st.Blocked:
		verdict.Action = ActionBlock
		verdict.Reason = ReasonPriorBlock
		verdict.Blocked = true
		verdict.Score = st.Score
		verdict.Chain = cloneStrings(st.Chain)

	default:
		_, suppressionActive, matched := s.matchAllowRuleLocked(ev)
		if matched && !suppressionActive {
			verdict.Action = ActionAllow
			verdict.Reason = ReasonAllowRule
			verdict.Chain = []string{}
			break
		}

		st.recordStep(ev.Kind, ev.TS)
		chain, score := st.evaluate(ev.TS, s.cfg)
		st.Chain = chain
		st.Score = score
		verdict.Score = score
		verdict.Chain = cloneStrings(chain)

		switch {
		case score >= BlockThreshold:
			st.Blocked = true
			verdict.Action = ActionBlock
			verdict.Reason = ReasonChainBlock
			verdict.Blocked = true
		case score >= AlertThreshold:
			if suppressionActive {
				verdict.Action = ActionAllow
				verdict.Reason = ReasonSuppressed
				verdict.Suppressed = true
			} else {
				verdict.Action = ActionAlert
				verdict.Reason = ReasonChainAlert
			}
		default:
			verdict.Action = ActionAllow
			verdict.Reason = ReasonScoreLow
		}
	}

	storedVerdict := cloneVerdict(verdict)
	st.Verdicts = append(st.Verdicts, storedVerdict)
	st.Seen[key] = cloneVerdict(verdict)

	return cloneVerdict(verdict), nil
}

// Allow registers one exemption rule and returns its id.
func (s *InjectionSentinel) Allow(rule AllowRule) (RuleID, error) {
	if err := validateAllowRule(rule); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.rules) >= s.cfg.MaxRules {
		return "", ErrRuleLimit
	}

	s.ruleSeq++
	id := fmt.Sprintf("rule-%d", s.ruleSeq)
	rule.ID = id
	s.rules = append(s.rules, rule)
	return RuleID(id), nil
}

// Rule reads one exemption rule.
func (s *InjectionSentinel) Rule(ruleID string) (AllowRule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rule := range s.rules {
		if rule.ID == ruleID {
			return rule, true
		}
	}
	return AllowRule{}, false
}

// Rules lists the exemption rules in id order.
func (s *InjectionSentinel) Rules() []AllowRule {
	s.mu.Lock()
	defer s.mu.Unlock()

	rules := make([]AllowRule, len(s.rules))
	copy(rules, s.rules)
	return rules
}

// Suppress suspends the exemption of one rule for the given window.
func (s *InjectionSentinel) Suppress(ruleID string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	known := false
	for _, rule := range s.rules {
		if rule.ID == ruleID {
			known = true
			break
		}
	}

	switch {
	case !known && ttl <= 0:
		return errors.Join(ErrUnknownRule, ErrBadSuppression)
	case !known:
		return ErrUnknownRule
	case ttl <= 0:
		return ErrBadSuppression
	}

	if s.suppressUntil == nil {
		s.suppressUntil = make(map[string]time.Time)
	}
	s.suppressUntil[ruleID] = time.Now().Add(ttl)
	return nil
}

// Release drops the block of one target after operator review.
func (s *InjectionSentinel) Release(pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.targets[pid]
	if st == nil || !st.Blocked {
		return ErrUnknownBlock
	}
	st.Blocked = false
	return nil
}

// Blocked lists the blocked target pids in ascending order.
func (s *InjectionSentinel) Blocked() []int {
	s.mu.Lock()
	defer s.mu.Unlock()

	blocked := make([]int, 0)
	for pid, st := range s.targets {
		if st.Blocked {
			blocked = append(blocked, pid)
		}
	}
	sort.Ints(blocked)
	return blocked
}

// Explain returns the event and verdict history of one target.
func (s *InjectionSentinel) Explain(pid int) (Trace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.targets[pid]
	if st == nil {
		return Trace{}, ErrNoTrace
	}

	events := append([]Event(nil), st.Events...)
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].TS < events[j].TS
	})

	verdicts := make([]Verdict, len(st.Verdicts))
	for i, verdict := range st.Verdicts {
		verdicts[i] = cloneVerdict(verdict)
	}

	return Trace{PID: pid, Events: events, Verdicts: verdicts}, nil
}

// Snapshot serialises the rules, the blocks and the per target buffers.
func (s *InjectionSentinel) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	data := snapshotData{
		Version:       snapshotVersion,
		Seq:           s.seq,
		RuleSeq:       s.ruleSeq,
		Rules:         append([]AllowRule(nil), s.rules...),
		SuppressTTL:   make(map[string]time.Duration),
		SuppressUntil: make(map[string]time.Time),
		Targets:       make([]targetState, 0, len(s.targets)),
		Blocks:        make([]blockSnapshot, 0),
	}

	for ruleID, until := range s.suppressUntil {
		if remaining := until.Sub(now); remaining > 0 {
			data.SuppressTTL[ruleID] = remaining
			data.SuppressUntil[ruleID] = until
		}
	}

	pids := make([]int, 0, len(s.targets))
	for pid := range s.targets {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	for _, pid := range pids {
		st := s.targets[pid]
		data.Targets = append(data.Targets, *st.snapshotCopy())
		if st.Blocked {
			data.Blocks = append(data.Blocks, blockSnapshot{
				PID:         pid,
				TargetStart: st.TargetStart,
			})
		}
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *InjectionSentinel) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}

	var snapshot snapshotData
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: trailing snapshot data", ErrBadSnapshot)
	}

	targets, ruleSeq, seq, suppressUntil, err := prepareSnapshot(&snapshot, s.cfg)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq = seq
	s.ruleSeq = ruleSeq
	s.rules = append([]AllowRule(nil), snapshot.Rules...)
	s.suppressUntil = suppressUntil
	s.targets = targets
	return nil
}

func validateEvent(ev Event) error {
	if ev.PID <= 0 || ev.TargetPID <= 0 || ev.TS <= 0 || !validKind(ev.Kind) {
		return ErrBadEvent
	}
	return nil
}

func validKind(kind string) bool {
	switch kind {
	case KindAttach, KindWriteVM, KindRemoteThread, KindExec:
		return true
	default:
		return false
	}
}

func eventWeight(kind string) (int, bool) {
	switch kind {
	case KindAttach:
		return ScoreAttach, true
	case KindWriteVM:
		return ScoreWriteVM, true
	case KindRemoteThread:
		return ScoreRemoteThread, true
	case KindExec:
		return ScoreExec, true
	default:
		return 0, false
	}
}

func eventKey(ev Event) string {
	return strings.Join([]string{
		strconv.Itoa(ev.PID),
		strconv.Itoa(ev.TargetPID),
		ev.Kind,
		strconv.FormatInt(ev.TS, 10),
	}, "|")
}

func validateAllowRule(rule AllowRule) error {
	if strings.TrimSpace(rule.Name) == "" ||
		rule.ParentPID <= 0 ||
		strings.TrimSpace(rule.Signer) == "" ||
		rule.ExpiresAt < 0 {
		return ErrBadRule
	}
	return nil
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneVerdict(verdict Verdict) Verdict {
	verdict.Chain = cloneStrings(verdict.Chain)
	return verdict
}

func (st *targetState) trimEvents(limit int) {
	if limit < 0 {
		limit = 0
	}
	if len(st.Events) <= limit {
		return
	}
	sort.SliceStable(st.Events, func(i, j int) bool {
		return st.Events[i].TS < st.Events[j].TS
	})
	if limit == 0 {
		st.Events = nil
		return
	}
	kept := make([]Event, limit)
	copy(kept, st.Events[len(st.Events)-limit:])
	st.Events = kept
}

func (st *targetState) recordStep(kind string, ts int64) {
	if st.StepTimes == nil {
		st.StepTimes = make(map[string][]int64)
	}
	st.StepTimes[kind] = append(st.StepTimes[kind], ts)
}

func (st *targetState) evaluate(currentTS int64, cfg Config) ([]string, int) {
	type candidate struct {
		kind string
		ts   int64
	}

	candidates := make([]candidate, 0, len(st.StepTimes))
	for kind, times := range st.StepTimes {
		if _, ok := eventWeight(kind); !ok {
			continue
		}
		nearestTS, ok := nearestStepTime(currentTS, times, cfg.Window)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{kind: kind, ts: nearestTS})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ts != candidates[j].ts {
			return candidates[i].ts < candidates[j].ts
		}
		return candidates[i].kind < candidates[j].kind
	})

	if cfg.MaxChainSteps >= 0 && len(candidates) > cfg.MaxChainSteps {
		candidates = candidates[:cfg.MaxChainSteps]
	}

	chain := make([]string, 0, len(candidates))
	score := 0
	for _, candidate := range candidates {
		weight, _ := eventWeight(candidate.kind)
		chain = append(chain, candidate.kind)
		score += weight
	}
	return chain, score
}

func nearestStepTime(currentTS int64, times []int64, window int64) (int64, bool) {
	if window < 0 {
		window = 0
	}

	found := false
	var nearest int64
	var bestDistance int64
	for _, ts := range times {
		distance := absoluteDistance(currentTS, ts)
		if distance <= window && (!found || distance < bestDistance) {
			found = true
			nearest = ts
			bestDistance = distance
		}
	}
	return nearest, found
}

func absoluteDistance(a, b int64) int64 {
	if a >= b {
		return a - b
	}
	return b - a
}

func (s *InjectionSentinel) matchAllowRuleLocked(ev Event) (AllowRule, bool, bool) {
	var matched AllowRule
	found := false
	allSuppressed := true
	now := time.Now()

	for _, rule := range s.rules {
		if rule.ParentPID != ev.ParentPID || rule.Signer != ev.Signer {
			continue
		}
		if rule.ExpiresAt > 0 && ev.TS > rule.ExpiresAt {
			continue
		}

		if !found {
			matched = rule
			found = true
		}

		until, suppressed := s.suppressUntil[rule.ID]
		if suppressed && now.Before(until) {
			continue
		}
		if suppressed {
			delete(s.suppressUntil, rule.ID)
		}
		allSuppressed = false
	}

	if !found {
		return AllowRule{}, false, false
	}
	return matched, allSuppressed, true
}

func (st *targetState) snapshotCopy() *targetState {
	copyState := *st
	copyState.Events = append([]Event(nil), st.Events...)
	copyState.Chain = cloneStrings(st.Chain)
	copyState.Verdicts = make([]Verdict, len(st.Verdicts))
	for i, verdict := range st.Verdicts {
		copyState.Verdicts[i] = cloneVerdict(verdict)
	}
	copyState.Seen = make(map[string]Verdict, len(st.Seen))
	for key, verdict := range st.Seen {
		copyState.Seen[key] = cloneVerdict(verdict)
	}
	copyState.StepTimes = make(map[string][]int64, len(st.StepTimes))
	for kind, times := range st.StepTimes {
		copyState.StepTimes[kind] = append([]int64(nil), times...)
	}
	return &copyState
}

func prepareSnapshot(data *snapshotData, cfg Config) (map[int]*targetState, int64, int64, map[string]time.Time, error) {
	bad := func(message string) error {
		return fmt.Errorf("%w: %s", ErrBadSnapshot, message)
	}

	if data.Version != snapshotVersion {
		return nil, 0, 0, nil, bad("unsupported snapshot version")
	}
	if data.Seq < 0 || data.RuleSeq < 0 {
		return nil, 0, 0, nil, bad("negative sequence")
	}

	ruleIDs := make(map[string]struct{}, len(data.Rules))
	ruleSeq := data.RuleSeq
	for _, rule := range data.Rules {
		if rule.ID == "" {
			return nil, 0, 0, nil, bad("rule without id")
		}
		if _, exists := ruleIDs[rule.ID]; exists {
			return nil, 0, 0, nil, bad("duplicate rule id")
		}
		ruleIDs[rule.ID] = struct{}{}
		if err := validateAllowRule(rule); err != nil {
			return nil, 0, 0, nil, bad("invalid rule")
		}
		if strings.HasPrefix(rule.ID, "rule-") {
			if number, err := strconv.ParseInt(strings.TrimPrefix(rule.ID, "rule-"), 10, 64); err == nil && number > ruleSeq {
				ruleSeq = number
			}
		}
	}
	if cfg.MaxRules >= 0 && len(data.Rules) > cfg.MaxRules {
		return nil, 0, 0, nil, bad("too many rules")
	}

	suppressUntil := make(map[string]time.Time, len(data.SuppressTTL)+len(data.SuppressUntil))
	now := time.Now()
	for ruleID, ttl := range data.SuppressTTL {
		if _, exists := ruleIDs[ruleID]; !exists {
			return nil, 0, 0, nil, bad("suppression for unknown rule")
		}
		if ttl < 0 {
			return nil, 0, 0, nil, bad("negative suppression ttl")
		}
	}
	for ruleID, until := range data.SuppressUntil {
		if _, exists := ruleIDs[ruleID]; !exists {
			return nil, 0, 0, nil, bad("suppression for unknown rule")
		}
		if until.IsZero() {
			return nil, 0, 0, nil, bad("zero suppression deadline")
		}
		if now.Before(until) {
			suppressUntil[ruleID] = until
		}
	}
	for ruleID, ttl := range data.SuppressTTL {
		if _, hasAbsoluteDeadline := data.SuppressUntil[ruleID]; hasAbsoluteDeadline {
			continue
		}
		if ttl > 0 {
			suppressUntil[ruleID] = now.Add(ttl)
		}
	}

	targets := make(map[int]*targetState, len(data.Targets))
	seq := data.Seq
	for i := range data.Targets {
		st := data.Targets[i]
		if st.PID <= 0 {
			return nil, 0, 0, nil, bad("target without pid")
		}
		if _, exists := targets[st.PID]; exists {
			return nil, 0, 0, nil, bad("duplicate target")
		}
		if cfg.MaxEventsPerTarget >= 0 && len(st.Events) > cfg.MaxEventsPerTarget {
			return nil, 0, 0, nil, bad("target event buffer too large")
		}
		for _, ev := range st.Events {
			if err := validateEvent(ev); err != nil {
				return nil, 0, 0, nil, bad("invalid buffered event")
			}
			if ev.TargetPID != st.PID || ev.TargetStart != st.TargetStart {
				return nil, 0, 0, nil, bad("buffered event target mismatch")
			}
		}
		for _, verdict := range st.Verdicts {
			if verdict.Seq <= 0 || verdict.TargetPID != st.PID {
				return nil, 0, 0, nil, bad("invalid verdict sequence")
			}
			if verdict.Seq > seq {
				seq = verdict.Seq
			}
		}
		for key, verdict := range st.Seen {
			if key == "" || verdict.Seq <= 0 {
				return nil, 0, 0, nil, bad("invalid idempotency record")
			}
			if verdict.Seq > seq {
				seq = verdict.Seq
			}
		}
		for kind, times := range st.StepTimes {
			if !validKind(kind) || len(times) == 0 {
				return nil, 0, 0, nil, bad("invalid chain step")
			}
			for _, ts := range times {
				if ts <= 0 {
					return nil, 0, 0, nil, bad("invalid chain step timestamp")
				}
			}
		}
		for _, kind := range st.Chain {
			if !validKind(kind) {
				return nil, 0, 0, nil, bad("invalid chain kind")
			}
		}
		if st.Score < 0 {
			return nil, 0, 0, nil, bad("negative score")
		}
		if st.Seen == nil {
			st.Seen = make(map[string]Verdict)
		}
		if st.StepTimes == nil {
			st.StepTimes = make(map[string][]int64)
		}
		targets[st.PID] = &st
	}

	blocked := make(map[int]blockSnapshot, len(data.Blocks))
	for _, block := range data.Blocks {
		if block.PID <= 0 {
			return nil, 0, 0, nil, bad("block without pid")
		}
		if _, exists := blocked[block.PID]; exists {
			return nil, 0, 0, nil, bad("duplicate block")
		}
		blocked[block.PID] = block
		st := targets[block.PID]
		if st == nil || !st.Blocked || st.TargetStart != block.TargetStart {
			return nil, 0, 0, nil, bad("inconsistent block")
		}
	}
	for pid, st := range targets {
		if st.Blocked {
			if _, exists := blocked[pid]; !exists {
				return nil, 0, 0, nil, bad("missing block record")
			}
		}
	}

	return targets, ruleSeq, seq, suppressUntil, nil
}
