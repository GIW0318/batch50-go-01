// Package task simulates cloud IAM authorisation: policies are granted per
// layer, a request is matched against actions, resources, principals and
// conditions, and the resulting decision records which layer settled it.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Policy layers.
const (
	LayerIdentity = "identity"
	LayerResource = "resource"
	LayerBoundary = "boundary"
	LayerSCP      = "scp"
)

// Policy effects.
const (
	EffectAllow = "Allow"
	EffectDeny  = "Deny"
)

// Decision results.
const (
	ResultAllow    = "allow"
	ResultDeny     = "deny"
	ResultImplicit = "implicit-deny"
)

// Decision reasons.
const (
	ReasonExplicitDeny     = "explicit-deny"
	ReasonIdentity         = "identity"
	ReasonResource         = "resource"
	ReasonBoundary         = "boundary"
	ReasonSCP              = "scp"
	ReasonNoAllow          = "no-allow"
	ReasonUnsupportedActor = "unsupported-action"
)

// Condition operators understood by the simulator.
const (
	OpStringEquals       = "StringEquals"
	OpStringLike         = "StringLike"
	OpArnLike            = "ArnLike"
	OpBool               = "Bool"
	OpDateLessThan       = "DateLessThan"
	OpNull               = "Null"
	OpForAllValuesEquals = "ForAllValues:StringEquals"
)

// Built-in condition keys.
const (
	KeyPrincipalAccount = "aws:PrincipalAccount"
	KeyRequestedRegion  = "aws:RequestedRegion"
	KeyCurrentTime      = "aws:CurrentTime"
	KeyPrincipalTag     = "aws:PrincipalTag/"
)

// Errors reported by the simulator.
var (
	ErrBadPolicy       = errors.New("policy document is invalid")
	ErrDuplicatePolicy = errors.New("policy id is already granted")
	ErrTooManyPolicies = errors.New("policy count exceeds the cap")
	ErrBadCondition    = errors.New("condition expression is invalid")
	ErrBadRequest      = errors.New("request is incomplete")
	ErrTooManyRequests = errors.New("batch size exceeds the cap")
	ErrNoDecision      = errors.New("decision is not registered")
	ErrNoPolicy        = errors.New("policy is not granted")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Policy is one statement granted under one layer.
type Policy struct {
	ID         string
	Layer      string
	Effect     string
	Actions    []string
	NotActions []string
	Resources  []string
	Principal  string
	Conditions map[string]string
}

// Request is one authorisation question.
type Request struct {
	Principal string
	Account   string
	Action    string
	Resource  string
	Region    string
	Time      time.Time
	Tags      map[string]string
}

// Decision is the outcome of one request.
type Decision struct {
	ID       string
	Result   string
	Reason   string
	PolicyID string
	Layer    string
}

// Trace explains which policies were consulted for one decision.
type Trace struct {
	DecisionID string
	Result     string
	Reason     string
	Policies   []string
	Steps      []string
}

// Stats counts the decisions produced since the last reset.
type Stats struct {
	Evaluated int
	Allowed   int
	Denied    int
	Implicit  int
}

// Limits bounds the policy table and the batch size.
type Limits struct {
	MaxPolicies int
	MaxBatch    int
}

// IamPolicySimulator owns the granted policies and the decision log.
type IamPolicySimulator struct {
	mu        sync.Mutex
	limits    Limits
	policies  map[string]Policy
	decisions []Decision
	traces    map[string]Trace
	seq       int
	stats     Stats
}

// NewIamPolicySimulator builds a simulator with the given caps.
func NewIamPolicySimulator(limits Limits) *IamPolicySimulator {
	return &IamPolicySimulator{limits: limits}
}

// Grant validates a policy and adds it to the policy table.
func (s *IamPolicySimulator) Grant(policy Policy) error {
	return errNotImplemented
}

// Revoke removes one granted policy.
func (s *IamPolicySimulator) Revoke(policyID string) error {
	return errNotImplemented
}

// Evaluate answers one request and records the decision.
func (s *IamPolicySimulator) Evaluate(req Request) (Decision, error) {
	return Decision{}, errNotImplemented
}

// Simulate answers a batch of requests, preserving their order.
func (s *IamPolicySimulator) Simulate(batch []Request) ([]Decision, error) {
	return nil, errNotImplemented
}

// Explain returns the policies and steps behind one decision.
func (s *IamPolicySimulator) Explain(decisionID string) (Trace, error) {
	return Trace{}, errNotImplemented
}

// Policies lists the granted policies in ascending id order.
func (s *IamPolicySimulator) Policies() []Policy {
	return nil
}

// DecisionFor reads one recorded decision.
func (s *IamPolicySimulator) DecisionFor(decisionID string) (Decision, bool) {
	return Decision{}, false
}

// Stats returns the decision counters.
func (s *IamPolicySimulator) Stats() Stats {
	return Stats{}
}

// Snapshot serialises the policy table and the decision log.
func (s *IamPolicySimulator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the simulator state with a snapshot, all or nothing.
func (s *IamPolicySimulator) Restore(data []byte) error {
	return errNotImplemented
}

// Reset drops every policy and decision.
func (s *IamPolicySimulator) Reset() error {
	return errNotImplemented
}
