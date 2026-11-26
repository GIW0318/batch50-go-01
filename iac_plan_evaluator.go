// Package task evaluates security policy over infrastructure-as-code execution
// plans. A plan is expanded into fully qualified resource addresses, every rule
// is applied to the attribute set the change leaves behind, and each result can
// be traced back to the rule and the attribute path that produced it.
package task

import (
	"errors"
	"sync"
)

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Finding verdicts.
const (
	VerdictViolation = "violation"
	VerdictUncertain = "uncertain"
	VerdictError     = "error"
	VerdictPass      = "pass"
)

// Plan actions.
const (
	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionReplace = "replace"
	ActionRead    = "read"
)

// Severities.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// Delta kinds.
const (
	DeltaAdded   = "added"
	DeltaRemoved = "removed"
	DeltaChanged = "changed"
)

// Errors reported by the evaluator.
var (
	ErrBadPlan          = errors.New("plan document is invalid")
	ErrTooManyResources = errors.New("plan exceeds the resource cap")
	ErrNoSnapshot       = errors.New("snapshot is not registered")
	ErrNoFinding        = errors.New("finding is not registered")
	ErrTooManyFindings  = errors.New("finding count exceeds the cap")
	ErrUnknownRule      = errors.New("rule id is unknown")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Rule is one policy check: the attribute it reads and the resource types it
// applies to.
type Rule struct {
	ID        string
	Name      string
	Severity  string
	Types     []string
	Attribute string
}

// Resource is one change in the expanded plan.
type Resource struct {
	Address   string
	Type      string
	Action    string
	Before    map[string]string
	After     map[string]string
	Unknown   []string
	Sensitive []string
}

// Snapshot is an expanded plan registered under an identifier.
type Snapshot struct {
	ID        string
	Digest    string
	Resources []Resource
}

// Finding is one policy result for one resource attribute.
type Finding struct {
	ID       string
	RuleID   string
	Rule     string
	Address  string
	Path     string
	Severity string
	Verdict  string
	Detail   string
}

// Trace explains how one finding was reached.
type Trace struct {
	FindingID string
	RuleID    string
	Rule      string
	Address   string
	Path      string
	Verdict   string
	Masked    bool
	Steps     []string
}

// Delta is the difference of one resource address between two snapshots.
type Delta struct {
	Address string
	Kind    string
	Paths   []string
}

// Limits bounds the work one evaluator accepts.
type Limits struct {
	MaxResources int
	MaxFindings  int
}

// IacPlanEvaluator owns the registered snapshots, the active rule set and the
// findings of the last evaluation.
type IacPlanEvaluator struct {
	mu       sync.Mutex
	limits   Limits
	rules    []Rule
	snaps    map[string]Snapshot
	order    []string
	findings map[string][]Finding
	traces   map[string]Trace
	seq      int
}

// NewIacPlanEvaluator builds an evaluator with the full rule set and the given
// caps.
func NewIacPlanEvaluator(limits Limits) *IacPlanEvaluator {
	return &IacPlanEvaluator{limits: limits}
}

// Load parses an execution plan, expands module resources into fully qualified
// addresses and registers the result.
func (e *IacPlanEvaluator) Load(plan []byte) (Snapshot, error) {
	return Snapshot{}, errNotImplemented
}

// Evaluate applies the active rules to one registered snapshot and returns the
// findings ordered by rule id, address and attribute path.
func (e *IacPlanEvaluator) Evaluate(snap Snapshot) ([]Finding, error) {
	return nil, errNotImplemented
}

// Explain returns the evaluation steps behind one finding.
func (e *IacPlanEvaluator) Explain(findingID string) (Trace, error) {
	return Trace{}, errNotImplemented
}

// Diff compares two snapshots by address and reports added, removed and changed
// resources.
func (e *IacPlanEvaluator) Diff(prev, next Snapshot) ([]Delta, error) {
	return nil, errNotImplemented
}

// Snapshot serialises every registered snapshot, the rule set and the stored
// findings.
func (e *IacPlanEvaluator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the evaluator state with a snapshot, all or nothing.
func (e *IacPlanEvaluator) Restore(data []byte) error {
	return errNotImplemented
}

// Rules lists the active rules in ascending id order.
func (e *IacPlanEvaluator) Rules() []Rule {
	return nil
}

// SetRules narrows the active rule set to the given ids.
func (e *IacPlanEvaluator) SetRules(ids []string) error {
	return errNotImplemented
}

// Findings returns the findings stored for one snapshot.
func (e *IacPlanEvaluator) Findings(snapshotID string) ([]Finding, bool) {
	return nil, false
}

// Snapshots lists the registered snapshot ids in registration order.
func (e *IacPlanEvaluator) Snapshots() []string {
	return nil
}

// Reset drops every registered snapshot, finding and trace.
func (e *IacPlanEvaluator) Reset() error {
	return errNotImplemented
}
