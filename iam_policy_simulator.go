// Package task simulates cloud IAM authorisation: policies are granted per
// layer, a request is matched against actions, resources, principals and
// conditions, and the resulting decision records which layer settled it.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	return &IamPolicySimulator{
		limits:   limits,
		policies: make(map[string]Policy),
		traces:   make(map[string]Trace),
	}
}

// Grant validates a policy and adds it to the policy table.
func (s *IamPolicySimulator) Grant(policy Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(policy.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrBadPolicy)
	}
	if _, exists := s.policies[policy.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicatePolicy, policy.ID)
	}
	if s.limits.MaxPolicies > 0 && len(s.policies) >= s.limits.MaxPolicies {
		return ErrTooManyPolicies
	}
	if !validLayer(policy.Layer) || !validEffect(policy.Effect) {
		return fmt.Errorf("%w: unsupported layer or effect", ErrBadPolicy)
	}
	if len(policy.Actions) == 0 && len(policy.NotActions) == 0 {
		return fmt.Errorf("%w: actions and notActions are both empty", ErrBadPolicy)
	}
	if len(policy.Resources) == 0 {
		return fmt.Errorf("%w: resources are required", ErrBadPolicy)
	}
	for _, pattern := range append(append([]string(nil), policy.Actions...), policy.NotActions...) {
		if !validActionPattern(pattern) {
			return fmt.Errorf("%w: unsupported action pattern %q", ErrBadPolicy, pattern)
		}
	}
	for _, pattern := range policy.Resources {
		if pattern == "" {
			return fmt.Errorf("%w: blank resource pattern", ErrBadPolicy)
		}
	}
	for raw, value := range policy.Conditions {
		if _, _, ok := splitCondition(raw); !ok {
			return fmt.Errorf("%w: unsupported operator in %q", ErrBadCondition, raw)
		}
		if _, err := compileCondition(raw, value); err != nil {
			return fmt.Errorf("%w: %v", ErrBadCondition, err)
		}
	}

	s.policies[policy.ID] = clonePolicy(policy)
	return nil
}

// Revoke removes one granted policy.
func (s *IamPolicySimulator) Revoke(policyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[policyID]; !ok {
		return ErrNoPolicy
	}
	delete(s.policies, policyID)
	return nil
}

// Evaluate answers one request and records the decision.
func (s *IamPolicySimulator) Evaluate(req Request) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateRequest(req); err != nil {
		return Decision{}, err
	}
	return s.evaluateLocked(req), nil
}

// Simulate answers a batch of requests, preserving their order.
func (s *IamPolicySimulator) Simulate(batch []Request) ([]Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits.MaxBatch > 0 && len(batch) > s.limits.MaxBatch {
		return nil, ErrTooManyRequests
	}
	for _, req := range batch {
		if err := validateRequest(req); err != nil {
			return nil, err
		}
	}
	decisions := make([]Decision, 0, len(batch))
	for _, req := range batch {
		decisions = append(decisions, s.evaluateLocked(req))
	}
	return decisions, nil
}

// Explain returns the policies and steps behind one decision.
func (s *IamPolicySimulator) Explain(decisionID string) (Trace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	trace, ok := s.traces[decisionID]
	if !ok {
		return Trace{}, ErrNoDecision
	}
	return cloneTrace(trace), nil
}

// Policies lists the granted policies in ascending id order.
func (s *IamPolicySimulator) Policies() []Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.policies))
	for id := range s.policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Policy, 0, len(ids))
	for _, id := range ids {
		out = append(out, clonePolicy(s.policies[id]))
	}
	return out
}

// DecisionFor reads one recorded decision.
func (s *IamPolicySimulator) DecisionFor(decisionID string) (Decision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, decision := range s.decisions {
		if decision.ID == decisionID {
			return decision, true
		}
	}
	return Decision{}, false
}

// Stats returns the decision counters.
func (s *IamPolicySimulator) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Snapshot serialises the policy table and the decision log.
func (s *IamPolicySimulator) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body := snapshotBody{
		Version:   snapshotVersion,
		Policies:  s.sortedPoliciesLocked(),
		Decisions: append([]Decision(nil), s.decisions...),
		Traces:    make([]Trace, 0, len(s.traces)),
		Seq:       s.seq,
		Stats:     s.stats,
	}
	for _, decision := range s.decisions {
		if trace, ok := s.traces[decision.ID]; ok {
			body.Traces = append(body.Traces, cloneTrace(trace))
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	envelope := snapshotEnvelope{Checksum: hex.EncodeToString(sum[:]), Body: json.RawMessage(raw)}
	return json.Marshal(envelope)
}

// Restore replaces the simulator state with a snapshot, all or nothing.
func (s *IamPolicySimulator) Restore(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Checksum == "" || len(envelope.Body) == 0 {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Body)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return ErrBadSnapshot
	}
	var body snapshotBody
	if err := json.Unmarshal(envelope.Body, &body); err != nil {
		return ErrBadSnapshot
	}
	if body.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	policies := make(map[string]Policy, len(body.Policies))
	for _, policy := range body.Policies {
		if err := validateRestoredPolicy(policy, policies, s.limits); err != nil {
			return ErrBadSnapshot
		}
		policies[policy.ID] = clonePolicy(policy)
	}

	decisionIDs := make(map[string]bool, len(body.Decisions))
	maxSeq := 0
	stats := Stats{}
	for _, decision := range body.Decisions {
		if decision.ID == "" || decisionIDs[decision.ID] {
			return ErrBadSnapshot
		}
		n, ok := decisionSequence(decision.ID)
		if !ok {
			return ErrBadSnapshot
		}
		decisionIDs[decision.ID] = true
		if n > maxSeq {
			maxSeq = n
		}
		switch decision.Result {
		case ResultAllow:
			stats.Allowed++
		case ResultDeny:
			stats.Denied++
		case ResultImplicit:
			stats.Implicit++
		default:
			return ErrBadSnapshot
		}
		stats.Evaluated++
	}
	traces := make(map[string]Trace, len(body.Traces))
	for _, trace := range body.Traces {
		if trace.DecisionID == "" || !decisionIDs[trace.DecisionID] || len(trace.Steps) < 2 {
			return ErrBadSnapshot
		}
		if _, exists := traces[trace.DecisionID]; exists {
			return ErrBadSnapshot
		}
		traces[trace.DecisionID] = cloneTrace(trace)
	}
	if len(traces) != len(decisionIDs) {
		return ErrBadSnapshot
	}
	seq := body.Seq
	if seq < maxSeq {
		seq = maxSeq
	}

	s.policies = policies
	s.decisions = append([]Decision(nil), body.Decisions...)
	s.traces = traces
	s.seq = seq
	s.stats = stats
	return nil
}

// Reset drops every policy and decision.
func (s *IamPolicySimulator) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies = make(map[string]Policy)
	s.decisions = nil
	s.traces = make(map[string]Trace)
	s.seq = 0
	s.stats = Stats{}
	return nil
}

const snapshotVersion = 1

type snapshotEnvelope struct {
	Checksum string          `json:"checksum"`
	Body     json.RawMessage `json:"body"`
}

type snapshotBody struct {
	Version   int        `json:"version"`
	Policies  []Policy   `json:"policies"`
	Decisions []Decision `json:"decisions"`
	Traces    []Trace    `json:"traces"`
	Seq       int        `json:"seq"`
	Stats     Stats      `json:"stats"`
}

var supportedServices = map[string]bool{
	"s3":   true,
	"iam":  true,
	"ec2":  true,
	"kms":  true,
	"sts":  true,
	"logs": true,
}

func validLayer(layer string) bool {
	switch layer {
	case LayerIdentity, LayerResource, LayerBoundary, LayerSCP:
		return true
	default:
		return false
	}
}

func validEffect(effect string) bool {
	return effect == EffectAllow || effect == EffectDeny
}

func validateRequest(req Request) error {
	if strings.TrimSpace(req.Action) == "" || strings.TrimSpace(req.Resource) == "" {
		return ErrBadRequest
	}
	return nil
}

func validActionPattern(pattern string) bool {
	if pattern == "*" {
		return true
	}
	index := strings.IndexByte(pattern, ':')
	if index <= 0 || index == len(pattern)-1 {
		return false
	}
	return supportedServices[pattern[:index]]
}

func actionService(action string) (string, bool) {
	index := strings.IndexByte(action, ':')
	if index <= 0 || index == len(action)-1 {
		return "", false
	}
	service := action[:index]
	return service, supportedServices[service]
}

func clonePolicy(policy Policy) Policy {
	out := policy
	out.Actions = append([]string(nil), policy.Actions...)
	out.NotActions = append([]string(nil), policy.NotActions...)
	out.Resources = append([]string(nil), policy.Resources...)
	if policy.Conditions != nil {
		out.Conditions = make(map[string]string, len(policy.Conditions))
		for key, value := range policy.Conditions {
			out.Conditions[key] = value
		}
	}
	return out
}

func cloneTrace(trace Trace) Trace {
	out := trace
	out.Policies = append([]string(nil), trace.Policies...)
	out.Steps = append([]string(nil), trace.Steps...)
	return out
}

func (s *IamPolicySimulator) sortedPolicyIDsLocked() []string {
	ids := make([]string, 0, len(s.policies))
	for id := range s.policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *IamPolicySimulator) sortedPoliciesLocked() []Policy {
	ids := s.sortedPolicyIDsLocked()
	out := make([]Policy, 0, len(ids))
	for _, id := range ids {
		out = append(out, clonePolicy(s.policies[id]))
	}
	return out
}

func (s *IamPolicySimulator) evaluateLocked(req Request) Decision {
	steps := []string{fmt.Sprintf(
		"request validated: principal=%q account=%q action=%q resource=%q",
		req.Principal, req.Account, req.Action, req.Resource,
	)}
	if _, ok := actionService(req.Action); !ok {
		steps = append(steps, fmt.Sprintf("action %q is not supported; denying implicitly", req.Action))
		return s.recordDecisionLocked(req, ResultImplicit, ReasonUnsupportedActor, "", "", nil, steps)
	}

	ids := s.sortedPolicyIDsLocked()
	steps = append(steps, fmt.Sprintf("evaluating %d policies in ascending id order", len(ids)))

	matched := make([]string, 0)
	denyIDs := make([]string, 0)
	identityAllows := make([]string, 0)
	resourceAllows := make([]string, 0)
	boundaryAllows := make([]string, 0)
	scpAllows := make([]string, 0)
	boundaryPolicies := 0
	scpPolicies := 0
	boundaryBlocked := false

	for _, id := range ids {
		policy := s.policies[id]
		if policy.Layer == LayerBoundary {
			boundaryPolicies++
		}
		if policy.Layer == LayerSCP {
			scpPolicies++
		}
		if !statementMatches(policy, req) {
			continue
		}
		matched = append(matched, id)
		if policy.Effect == EffectDeny {
			if policy.Layer == LayerBoundary {
				boundaryBlocked = true
			} else {
				denyIDs = append(denyIDs, id)
			}
			continue
		}
		switch policy.Layer {
		case LayerIdentity:
			identityAllows = append(identityAllows, id)
		case LayerResource:
			resourceAllows = append(resourceAllows, id)
		case LayerBoundary:
			boundaryAllows = append(boundaryAllows, id)
		case LayerSCP:
			scpAllows = append(scpAllows, id)
		}
	}

	if len(denyIDs) > 0 {
		policy := s.policies[denyIDs[0]]
		steps = append(steps, fmt.Sprintf("explicit deny matched policy %q in layer %q", policy.ID, policy.Layer))
		return s.recordDecisionLocked(req, ResultDeny, ReasonExplicitDeny, policy.ID, policy.Layer, matched, steps)
	}

	sourceID := ""
	sourceLayer := ""
	sourceReason := ""
	if len(identityAllows) > 0 {
		sourceID = identityAllows[0]
		sourceLayer = LayerIdentity
		sourceReason = ReasonIdentity
		steps = append(steps, fmt.Sprintf("identity allow matched policy %q", sourceID))
	} else {
		for _, id := range resourceAllows {
			if resourcePolicySameAccount(s.policies[id], req) {
				sourceID = id
				sourceLayer = LayerResource
				sourceReason = ReasonResource
				steps = append(steps, fmt.Sprintf("same-account resource allow matched policy %q", sourceID))
				break
			}
		}
		if sourceID == "" && len(resourceAllows) > 0 {
			steps = append(steps, "resource allow matched only a cross-account principal; an identity allow is required")
		}
	}

	if sourceID == "" {
		steps = append(steps, "no effective allow matched; denying implicitly")
		return s.recordDecisionLocked(req, ResultImplicit, ReasonNoAllow, "", "", matched, steps)
	}

	if boundaryPolicies > 0 && (boundaryBlocked || len(boundaryAllows) == 0) {
		steps = append(steps, "permissions boundary caps the otherwise allowed request")
		return s.recordDecisionLocked(req, ResultImplicit, ReasonBoundary, "", "", matched, steps)
	}
	if scpPolicies > 0 && len(scpAllows) == 0 {
		steps = append(steps, "service control policy caps the otherwise allowed request")
		return s.recordDecisionLocked(req, ResultImplicit, ReasonSCP, "", "", matched, steps)
	}

	steps = append(steps, fmt.Sprintf("allow granted by policy %q in layer %q", sourceID, sourceLayer))
	return s.recordDecisionLocked(req, ResultAllow, sourceReason, sourceID, sourceLayer, matched, steps)
}

func (s *IamPolicySimulator) recordDecisionLocked(
	req Request,
	result string,
	reason string,
	policyID string,
	layer string,
	policies []string,
	steps []string,
) Decision {
	s.seq++
	decision := Decision{
		ID:       "d-" + strconv.Itoa(s.seq),
		Result:   result,
		Reason:   reason,
		PolicyID: policyID,
		Layer:    layer,
	}
	trace := Trace{
		DecisionID: decision.ID,
		Result:     result,
		Reason:     reason,
		Policies:   append([]string(nil), policies...),
		Steps:      append([]string(nil), steps...),
	}
	if len(trace.Steps) < 2 {
		trace.Steps = append(trace.Steps, "decision recorded")
	}
	s.decisions = append(s.decisions, decision)
	s.traces[decision.ID] = trace
	s.stats.Evaluated++
	switch result {
	case ResultAllow:
		s.stats.Allowed++
	case ResultDeny:
		s.stats.Denied++
	case ResultImplicit:
		s.stats.Implicit++
	}
	return decision
}

func statementMatches(policy Policy, req Request) bool {
	if !actionMatches(policy, req.Action) {
		return false
	}
	if !resourceListMatches(policy.Resources, req.Resource) {
		return false
	}
	if policy.Layer == LayerResource && !principalMatches(policy.Principal, req) {
		return false
	}
	return conditionsMatch(policy.Conditions, req)
}

func actionMatches(policy Policy, action string) bool {
	allowed := len(policy.Actions) == 0
	for _, pattern := range policy.Actions {
		if globMatch(pattern, action) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	for _, pattern := range policy.NotActions {
		if globMatch(pattern, action) {
			return false
		}
	}
	return true
}

func resourceListMatches(patterns []string, resource string) bool {
	for _, pattern := range patterns {
		if resourceMatch(pattern, resource) {
			return true
		}
	}
	return false
}

func resourceMatch(pattern, resource string) bool {
	if pattern == "*" {
		return true
	}
	patternParts := splitResourceSegments(pattern)
	resourceParts := splitResourceSegments(resource)
	if len(patternParts) != len(resourceParts) {
		return false
	}
	for index := range patternParts {
		if !globMatch(patternParts[index], resourceParts[index]) {
			return false
		}
	}
	return true
}

func splitResourceSegments(value string) []string {
	parts := make([]string, 0, 8)
	start := 0
	for index := 0; index < len(value); index++ {
		if value[index] == ':' || value[index] == '/' {
			parts = append(parts, value[start:index])
			start = index + 1
		}
	}
	return append(parts, value[start:])
}

func globMatch(pattern, value string) bool {
	patternRunes := []rune(pattern)
	valueRunes := []rune(value)
	patternIndex := 0
	valueIndex := 0
	starIndex := -1
	starValueIndex := 0

	for valueIndex < len(valueRunes) {
		if patternIndex < len(patternRunes) &&
			(patternRunes[patternIndex] == '?' || patternRunes[patternIndex] == valueRunes[valueIndex]) {
			patternIndex++
			valueIndex++
			continue
		}
		if patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
			starIndex = patternIndex
			starValueIndex = valueIndex
			patternIndex++
			continue
		}
		if starIndex >= 0 {
			patternIndex = starIndex + 1
			starValueIndex++
			valueIndex = starValueIndex
			continue
		}
		return false
	}
	for patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(patternRunes)
}

func principalMatches(pattern string, req Request) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == req.Principal || pattern == req.Account {
		return true
	}
	if globMatch(pattern, req.Principal) || globMatch(pattern, req.Account) {
		return true
	}
	if !strings.HasPrefix(pattern, "arn:") {
		return false
	}
	account, ok := arnAccount(pattern)
	if !ok || account != req.Account {
		return false
	}
	if strings.HasSuffix(pattern, ":root") {
		return true
	}
	index := strings.LastIndexByte(pattern, ':')
	if index < 0 || index == len(pattern)-1 {
		return true
	}
	resourcePart := pattern[index+1:]
	if slash := strings.LastIndexByte(resourcePart, '/'); slash >= 0 {
		resourcePart = resourcePart[slash+1:]
	}
	return globMatch(resourcePart, req.Principal)
}

func resourcePolicySameAccount(policy Policy, req Request) bool {
	for _, pattern := range policy.Resources {
		account, ok := arnAccount(pattern)
		if !ok {
			continue
		}
		if account != req.Account {
			return false
		}
	}
	return true
}

func arnAccount(value string) (string, bool) {
	if !strings.HasPrefix(value, "arn:") {
		return "", false
	}
	parts := strings.SplitN(value, ":", 6)
	if len(parts) < 6 || parts[4] == "" || strings.ContainsAny(parts[4], "*?") {
		return "", false
	}
	return parts[4], true
}

type compiledCondition struct {
	operator     string
	key          string
	expected     string
	expectedBool bool
	expectedTime time.Time
}

func splitCondition(raw string) (string, string, bool) {
	operators := []string{
		OpForAllValuesEquals,
		OpStringEquals,
		OpStringLike,
		OpArnLike,
		OpBool,
		OpDateLessThan,
		OpNull,
	}
	for _, operator := range operators {
		prefix := operator + ":"
		if strings.HasPrefix(raw, prefix) {
			return operator, raw[len(prefix):], true
		}
	}
	return "", "", false
}

func compileCondition(raw, value string) (compiledCondition, error) {
	operator, key, ok := splitCondition(raw)
	if !ok {
		return compiledCondition{}, fmt.Errorf("unknown operator")
	}
	condition := compiledCondition{operator: operator, key: key, expected: value}
	switch operator {
	case OpDateLessThan:
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return compiledCondition{}, fmt.Errorf("invalid date %q", value)
		}
		condition.expectedTime = parsed
	case OpBool, OpNull:
		parsed, err := parseStrictBool(value)
		if err != nil {
			return compiledCondition{}, err
		}
		condition.expectedBool = parsed
	}
	return condition, nil
}

func parseStrictBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", value)
	}
}

func conditionsMatch(conditions map[string]string, req Request) bool {
	rawConditions := make([]string, 0, len(conditions))
	for raw := range conditions {
		rawConditions = append(rawConditions, raw)
	}
	sort.Strings(rawConditions)
	for _, raw := range rawConditions {
		condition, err := compileCondition(raw, conditions[raw])
		if err != nil || !conditionMatches(condition, req) {
			return false
		}
	}
	return true
}

func conditionMatches(condition compiledCondition, req Request) bool {
	if !knownConditionKey(condition.key) {
		return false
	}
	actual, exists := conditionValue(condition.key, req)
	if condition.operator == OpNull {
		if condition.expectedBool {
			return exists
		}
		return !exists
	}
	if !exists {
		return false
	}
	switch condition.operator {
	case OpStringEquals:
		return actual == condition.expected
	case OpStringLike, OpArnLike:
		return globMatch(condition.expected, actual)
	case OpBool:
		actualBool, err := parseStrictBool(actual)
		return err == nil && actualBool == condition.expectedBool
	case OpDateLessThan:
		if condition.key == KeyCurrentTime {
			return req.Time.Before(condition.expectedTime)
		}
		actualTime, err := time.Parse(time.RFC3339, actual)
		return err == nil && actualTime.Before(condition.expectedTime)
	case OpForAllValuesEquals:
		for _, value := range strings.Split(actual, ",") {
			if value != condition.expected {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func knownConditionKey(key string) bool {
	switch key {
	case KeyPrincipalAccount, KeyRequestedRegion, KeyCurrentTime:
		return true
	}
	return strings.HasPrefix(key, KeyPrincipalTag) && len(key) > len(KeyPrincipalTag)
}

func conditionValue(key string, req Request) (string, bool) {
	switch key {
	case KeyPrincipalAccount:
		return req.Account, req.Account != ""
	case KeyRequestedRegion:
		return req.Region, req.Region != ""
	case KeyCurrentTime:
		return req.Time.Format(time.RFC3339Nano), true
	}
	if strings.HasPrefix(key, KeyPrincipalTag) {
		name := strings.TrimPrefix(key, KeyPrincipalTag)
		if name == "" {
			return "", false
		}
		value, ok := req.Tags[name]
		return value, ok
	}
	return "", false
}

func validateRestoredPolicy(policy Policy, existing map[string]Policy, limits Limits) error {
	if strings.TrimSpace(policy.ID) == "" {
		return ErrBadPolicy
	}
	if _, ok := existing[policy.ID]; ok {
		return ErrDuplicatePolicy
	}
	if limits.MaxPolicies > 0 && len(existing) >= limits.MaxPolicies {
		return ErrTooManyPolicies
	}
	if !validLayer(policy.Layer) || !validEffect(policy.Effect) {
		return ErrBadPolicy
	}
	if len(policy.Actions) == 0 && len(policy.NotActions) == 0 {
		return ErrBadPolicy
	}
	if len(policy.Resources) == 0 {
		return ErrBadPolicy
	}
	for _, pattern := range append(append([]string(nil), policy.Actions...), policy.NotActions...) {
		if !validActionPattern(pattern) {
			return ErrBadPolicy
		}
	}
	for _, pattern := range policy.Resources {
		if pattern == "" {
			return ErrBadPolicy
		}
	}
	for raw, value := range policy.Conditions {
		if _, _, ok := splitCondition(raw); !ok {
			return ErrBadCondition
		}
		if _, err := compileCondition(raw, value); err != nil {
			return ErrBadCondition
		}
	}
	return nil
}

func decisionSequence(id string) (int, bool) {
	if !strings.HasPrefix(id, "d-") {
		return 0, false
	}
	sequence, err := strconv.Atoi(strings.TrimPrefix(id, "d-"))
	return sequence, err == nil && sequence > 0
}
