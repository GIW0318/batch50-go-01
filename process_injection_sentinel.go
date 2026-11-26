// Package task consumes managed host process events, folds the attach, remote
// write and remote thread steps into one injection chain per target and blocks
// or alerts on the chain that crosses the configured thresholds.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

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
	cfg Config
}

// NewInjectionSentinel builds a sentinel with the given caps.
func NewInjectionSentinel(cfg Config) *InjectionSentinel {
	return &InjectionSentinel{cfg: cfg}
}

// Ingest scores one event against the chain of its target.
func (s *InjectionSentinel) Ingest(ev Event) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Allow registers one exemption rule and returns its id.
func (s *InjectionSentinel) Allow(rule AllowRule) (RuleID, error) {
	return "", errNotImplemented
}

// Rule reads one exemption rule.
func (s *InjectionSentinel) Rule(ruleID string) (AllowRule, bool) {
	return AllowRule{}, false
}

// Rules lists the exemption rules in id order.
func (s *InjectionSentinel) Rules() []AllowRule {
	return nil
}

// Suppress suspends the exemption of one rule for the given window.
func (s *InjectionSentinel) Suppress(ruleID string, ttl time.Duration) error {
	return errNotImplemented
}

// Release drops the block of one target after operator review.
func (s *InjectionSentinel) Release(pid int) error {
	return errNotImplemented
}

// Blocked lists the blocked target pids in ascending order.
func (s *InjectionSentinel) Blocked() []int {
	return nil
}

// Explain returns the event and verdict history of one target.
func (s *InjectionSentinel) Explain(pid int) (Trace, error) {
	return Trace{}, errNotImplemented
}

// Snapshot serialises the rules, the blocks and the per target buffers.
func (s *InjectionSentinel) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *InjectionSentinel) Restore(data []byte) error {
	return errNotImplemented
}
