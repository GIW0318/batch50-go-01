// Package task evaluates infrastructure-as-code execution plans against a
// fixed security rule catalogue.
package task

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	VerdictViolation = "violation"
	VerdictUncertain = "uncertain"
	VerdictError     = "error"
	VerdictPass      = "pass"

	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionReplace = "replace"
	ActionRead    = "read"

	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"

	DeltaAdded   = "added"
	DeltaRemoved = "removed"
	DeltaChanged = "changed"

	persistenceVersion = 1
	maskedValue        = "********"
)

var (
	ErrBadPlan          = errors.New("plan document is invalid")
	ErrTooManyResources = errors.New("plan exceeds the resource cap")
	ErrNoSnapshot       = errors.New("snapshot is not registered")
	ErrNoFinding        = errors.New("finding is not registered")
	ErrTooManyFindings  = errors.New("finding count exceeds the cap")
	ErrUnknownRule      = errors.New("rule id is unknown")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

type Rule struct {
	ID        string
	Name      string
	Severity  string
	Types     []string
	Attribute string
}

type Resource struct {
	Address   string
	Type      string
	Action    string
	Before    map[string]string
	After     map[string]string
	Unknown   []string
	Sensitive []string
}

type Snapshot struct {
	ID        string
	Digest    string
	Resources []Resource
}

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

type Delta struct {
	Address string
	Kind    string
	Paths   []string
}

type Limits struct {
	MaxResources int
	MaxFindings  int
}

type IacPlanEvaluator struct {
	mu       sync.Mutex
	limits   Limits
	rules    []Rule
	seq      int
	order    []string
	snaps    map[string]Snapshot
	rawPlans map[string][]byte
	attrs    map[string]map[string]attributeData
	findings map[string][]Finding
	traces   map[string]map[string]Trace
}

type attributeData struct {
	Before map[string]any
	After  map[string]any
}

type evaluatedFinding struct {
	Finding Finding
	Trace   Trace
}

type ruleIssue struct {
	Path    string
	Verdict string
	Detail  string
	Unknown bool
	HasVal  bool
	Value   any
}

type evalContext struct {
	resource  Resource
	attrs     map[string]any
	unknown   map[string]struct{}
	sensitive map[string]struct{}
	source    string
}

type rawResourceDTO struct {
	Address   string          `json:"address"`
	Type      string          `json:"type"`
	Action    string          `json:"action"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	Unknown   []string        `json:"unknown"`
	Sensitive []string        `json:"sensitive"`
}

type rawModuleDTO struct {
	Name      string           `json:"name"`
	Count     int              `json:"count"`
	Resources []rawResourceDTO `json:"resources"`
}

type persistedState struct {
	Version   int                         `json:"version"`
	Limits    Limits                      `json:"limits"`
	RuleIDs   []string                    `json:"rule_ids"`
	Seq       int                         `json:"seq"`
	Order     []string                    `json:"order"`
	Snapshots map[string]Snapshot         `json:"snapshots"`
	RawPlans  map[string][]byte           `json:"raw_plans"`
	Findings  map[string][]Finding        `json:"findings"`
	Traces    map[string]map[string]Trace `json:"traces"`
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

func NewIacPlanEvaluator(limits Limits) *IacPlanEvaluator {
	if limits.MaxResources < 0 {
		limits.MaxResources = 0
	}
	if limits.MaxFindings < 0 {
		limits.MaxFindings = 0
	}
	e := &IacPlanEvaluator{limits: limits}
	e.initLocked()
	return e
}

func (e *IacPlanEvaluator) initLocked() {
	if e.rules == nil {
		e.rules = builtinRules()
	}
	if e.snaps == nil {
		e.snaps = make(map[string]Snapshot)
	}
	if e.rawPlans == nil {
		e.rawPlans = make(map[string][]byte)
	}
	if e.attrs == nil {
		e.attrs = make(map[string]map[string]attributeData)
	}
	if e.findings == nil {
		e.findings = make(map[string][]Finding)
	}
	if e.traces == nil {
		e.traces = make(map[string]map[string]Trace)
	}
}

func builtinRules() []Rule {
	return []Rule{
		{ID: "R01", Name: "s3-bucket-acl-must-be-private", Severity: SeverityCritical, Types: []string{"aws_s3_bucket"}, Attribute: "acl"},
		{ID: "R02", Name: "policy-principal-must-not-allow-wildcard", Severity: SeverityHigh, Types: []string{"aws_s3_bucket_policy", "aws_iam_policy", "aws_iam_role", "aws_iam_role_policy", "aws_iam_user_policy", "aws_iam_group_policy", "aws_iam_policy_document"}, Attribute: "principal"},
		{ID: "R03", Name: "ebs-volume-must-be-encrypted", Severity: SeverityHigh, Types: []string{"aws_ebs_volume"}, Attribute: "encrypted"},
		{ID: "R04", Name: "security-group-ingress-must-not-be-open", Severity: SeverityCritical, Types: []string{"aws_security_group", "aws_security_group_rule", "aws_vpc_security_group_ingress_rule"}, Attribute: "ingress"},
		{ID: "R05", Name: "database-must-not-be-publicly-accessible", Severity: SeverityCritical, Types: []string{"aws_db_instance"}, Attribute: "publicly_accessible"},
		{ID: "R06", Name: "kms-key-rotation-must-be-enabled", Severity: SeverityMedium, Types: []string{"aws_kms_key"}, Attribute: "rotation"},
	}
}

func builtinRuleMap() map[string]Rule {
	rules := builtinRules()
	out := make(map[string]Rule, len(rules))
	for _, rule := range rules {
		out[rule.ID] = rule
	}
	return out
}

func (e *IacPlanEvaluator) Load(plan []byte) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()

	snapshot, attributes, err := parsePlan(plan)
	if err != nil {
		return Snapshot{}, err
	}
	if len(snapshot.Resources) > e.limits.MaxResources {
		return Snapshot{}, ErrTooManyResources
	}

	e.seq++
	snapshot.ID = fmt.Sprintf("snap-%04d", e.seq)
	stored := cloneSnapshot(snapshot)
	e.snaps[snapshot.ID] = stored
	e.rawPlans[snapshot.ID] = append([]byte(nil), plan...)
	e.attrs[snapshot.ID] = attributes
	e.order = append(e.order, snapshot.ID)
	return cloneSnapshot(stored), nil
}

func parsePlan(plan []byte) (Snapshot, map[string]attributeData, error) {
	if len(bytes.TrimSpace(plan)) == 0 {
		return Snapshot{}, nil, ErrBadPlan
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(plan, &document); err != nil {
		return Snapshot{}, nil, fmt.Errorf("%w: invalid JSON: %v", ErrBadPlan, err)
	}
	resourcesRaw, ok := document["resources"]
	if !ok || rawIsNull(resourcesRaw) {
		return Snapshot{}, nil, fmt.Errorf("%w: resources is required", ErrBadPlan)
	}

	var rootResources []rawResourceDTO
	if err := json.Unmarshal(resourcesRaw, &rootResources); err != nil {
		return Snapshot{}, nil, fmt.Errorf("%w: resources must be an array: %v", ErrBadPlan, err)
	}
	var modules []rawModuleDTO
	if modulesRaw, exists := document["modules"]; exists && !rawIsNull(modulesRaw) {
		if err := json.Unmarshal(modulesRaw, &modules); err != nil {
			return Snapshot{}, nil, fmt.Errorf("%w: modules must be an array: %v", ErrBadPlan, err)
		}
	}

	expanded := make([]Resource, 0, len(rootResources))
	attributes := make(map[string]attributeData)
	seen := make(map[string]struct{})

	add := func(dto rawResourceDTO, prefix string) error {
		address := strings.TrimSpace(dto.Address)
		resourceType := strings.TrimSpace(dto.Type)
		if address == "" || resourceType == "" {
			return fmt.Errorf("%w: address and type are required", ErrBadPlan)
		}
		if !validAction(dto.Action) {
			return fmt.Errorf("%w: invalid action %q", ErrBadPlan, dto.Action)
		}
		before, err := decodeObject(dto.Before)
		if err != nil {
			return err
		}
		after, err := decodeObject(dto.After)
		if err != nil {
			return err
		}

		address = prefix + normalizeAddress(address)
		if _, duplicate := seen[address]; duplicate {
			return fmt.Errorf("%w: duplicate address %s", ErrBadPlan, address)
		}
		seen[address] = struct{}{}

		resource := Resource{
			Address:   address,
			Type:      resourceType,
			Action:    dto.Action,
			Before:    toStringAttributes(before),
			After:     toStringAttributes(after),
			Unknown:   append([]string(nil), dto.Unknown...),
			Sensitive: append([]string(nil), dto.Sensitive...),
		}
		expanded = append(expanded, resource)
		attributes[address] = attributeData{Before: before, After: after}
		return nil
	}

	for _, dto := range rootResources {
		if err := add(dto, ""); err != nil {
			return Snapshot{}, nil, err
		}
	}
	for _, module := range modules {
		name := strings.TrimSpace(module.Name)
		if name == "" || module.Count < 0 {
			return Snapshot{}, nil, fmt.Errorf("%w: module name and non-negative count are required", ErrBadPlan)
		}
		for i := 0; i < module.Count; i++ {
			prefix := "module." + name + "[" + strconv.Itoa(i) + "]."
			for _, dto := range module.Resources {
				if err := add(dto, prefix); err != nil {
					return Snapshot{}, nil, err
				}
			}
		}
	}

	sort.SliceStable(expanded, func(i, j int) bool { return expanded[i].Address < expanded[j].Address })
	snapshot := Snapshot{Resources: expanded}
	canonical, err := json.Marshal(snapshot.Resources)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("%w: digest failed: %v", ErrBadPlan, err)
	}
	digest := sha256.Sum256(canonical)
	snapshot.Digest = hex.EncodeToString(digest[:])
	return snapshot, attributes, nil
}

func validAction(action string) bool {
	switch action {
	case ActionCreate, ActionUpdate, ActionDelete, ActionReplace, ActionRead:
		return true
	}
	return false
}

func rawIsNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || rawIsNull(raw) {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: invalid attributes: %v", ErrBadPlan, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: before and after must be objects", ErrBadPlan)
	}
	return object, nil
}

func toStringAttributes(values map[string]any) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = displayValue(value)
	}
	return out
}

func displayValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

func normalizeAddress(address string) string {
	var out strings.Builder
	for i := 0; i < len(address); {
		if address[i] != '[' {
			out.WriteByte(address[i])
			i++
			continue
		}
		relativeEnd := strings.IndexByte(address[i+1:], ']')
		if relativeEnd < 0 {
			out.WriteString(address[i:])
			break
		}
		end := i + 1 + relativeEnd
		index := strings.TrimSpace(address[i+1 : end])
		out.WriteByte('[')
		if _, err := strconv.Atoi(index); err == nil {
			out.WriteString(index)
		} else if strings.HasPrefix(index, `"`) {
			if unquoted, err := strconv.Unquote(index); err == nil {
				out.WriteString(strconv.Quote(unquoted))
			} else {
				out.WriteString(index)
			}
		} else if index != "" {
			out.WriteString(strconv.Quote(index))
		}
		out.WriteByte(']')
		i = end + 1
	}
	return out.String()
}

func canonicalPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "after.")
	path = strings.TrimPrefix(path, "before.")
	return normalizeAddress(path)
}

func pathSetHas(set map[string]struct{}, path string) bool {
	target := canonicalPath(path)
	if _, ok := set[target]; ok {
		return true
	}
	for candidate := range set {
		if strings.EqualFold(candidate, target) || isParentPath(candidate, target) || isParentPath(target, candidate) {
			return true
		}
	}
	return false
}

func isParentPath(parent, child string) bool {
	if parent == "" || parent == child || !strings.HasPrefix(child, parent) {
		return false
	}
	rest := child[len(parent):]
	return strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "[")
}

func (e *IacPlanEvaluator) Evaluate(snap Snapshot) ([]Finding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()

	registered, ok := e.snaps[snap.ID]
	if !ok {
		return nil, ErrNoSnapshot
	}
	results := evaluateSnapshot(registered, e.attrs[snap.ID], e.rules)

	findings := make([]Finding, 0, len(results))
	traces := make(map[string]Trace, len(results))
	for _, result := range results {
		if _, exists := traces[result.Finding.ID]; exists {
			continue
		}
		findings = append(findings, result.Finding)
		traces[result.Finding.ID] = result.Trace
	}
	if len(findings) > e.limits.MaxFindings {
		return nil, ErrTooManyFindings
	}

	e.findings[snap.ID] = findings
	e.traces[snap.ID] = traces
	return cloneFindings(findings), nil
}

func evaluateSnapshot(snapshot Snapshot, attributes map[string]attributeData, rules []Rule) []evaluatedFinding {
	results := make([]evaluatedFinding, 0)
	for _, resource := range snapshot.Resources {
		for _, rule := range rules {
			if !ruleApplies(rule, resource.Type) {
				continue
			}
			context := newEvalContext(resource, attributes[resource.Address])
			func() {
				defer func() {
					if recover() != nil {
						results = append(results, context.finish(rule, ruleIssue{
							Path: rule.Attribute, Verdict: VerdictError, Detail: "rule evaluation failed and was isolated",
						}))
					}
				}()
				for _, issue := range runRule(rule, context) {
					results = append(results, context.finish(rule, issue))
				}
			}()
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i].Finding, results[j].Finding
		if left.RuleID != right.RuleID {
			return left.RuleID < right.RuleID
		}
		if left.Address != right.Address {
			return left.Address < right.Address
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Verdict < right.Verdict
	})
	return results
}

func ruleApplies(rule Rule, resourceType string) bool {
	if rule.ID == "R02" {
		return true
	}
	for _, candidate := range rule.Types {
		if candidate == resourceType {
			return true
		}
	}
	return false
}

func newEvalContext(resource Resource, data attributeData) evalContext {
	effective := data.After
	fallback := resource.After
	source := "after"
	if resource.Action == ActionDelete {
		effective = data.Before
		fallback = resource.Before
		source = "before"
	}
	if effective == nil {
		effective = make(map[string]any, len(fallback))
		for key, value := range fallback {
			effective[key] = value
		}
	}

	unknown := make(map[string]struct{}, len(resource.Unknown))
	for _, path := range resource.Unknown {
		unknown[canonicalPath(path)] = struct{}{}
	}
	sensitive := make(map[string]struct{}, len(resource.Sensitive))
	for _, path := range resource.Sensitive {
		sensitive[canonicalPath(path)] = struct{}{}
	}
	return evalContext{resource: resource, attrs: effective, unknown: unknown, sensitive: sensitive, source: source}
}

func (c evalContext) isUnknown(path string) bool { return pathSetHas(c.unknown, path) }

func (c evalContext) isSensitive(path string) bool { return pathSetHas(c.sensitive, path) }

func (c evalContext) value(path string) (any, bool) {
	value, ok := c.attrs[path]
	return value, ok
}

func findingID(ruleID, address, path string) string {
	return ruleID + ":" + address + "#" + path
}

func (c evalContext) finish(rule Rule, issue ruleIssue) evaluatedFinding {
	masked := c.isSensitive(issue.Path)
	id := findingID(rule.ID, c.resource.Address, issue.Path)
	steps := []string{
		fmt.Sprintf("loaded %s (%s, action=%s); evaluating %s-change attributes", c.resource.Address, c.resource.Type, c.resource.Action, c.source),
		fmt.Sprintf("rule %s checks attribute path %s", rule.ID, issue.Path),
	}
	switch {
	case issue.Unknown:
		steps = append(steps, fmt.Sprintf("path %s is marked unknown and is not treated as empty", issue.Path))
	case masked:
		steps = append(steps, fmt.Sprintf("path %s is sensitive; observed value is masked as %s", issue.Path, maskedValue))
	case !issue.HasVal:
		steps = append(steps, fmt.Sprintf("path %s is not present", issue.Path))
	default:
		steps = append(steps, fmt.Sprintf("observed %s = %s", issue.Path, truncateValue(displayValue(issue.Value), 180)))
	}
	steps = append(steps, fmt.Sprintf("%s/%s: %s", rule.ID, issue.Verdict, issue.Detail))

	finding := Finding{ID: id, RuleID: rule.ID, Rule: rule.Name, Address: c.resource.Address, Path: issue.Path, Severity: rule.Severity, Verdict: issue.Verdict, Detail: issue.Detail}
	trace := Trace{FindingID: id, RuleID: rule.ID, Rule: rule.Name, Address: c.resource.Address, Path: issue.Path, Verdict: issue.Verdict, Masked: masked, Steps: steps}
	return evaluatedFinding{Finding: finding, Trace: trace}
}

func truncateValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func runRule(rule Rule, context evalContext) []ruleIssue {
	switch rule.ID {
	case "R01":
		return evalACL(rule, context)
	case "R02":
		return evalPrincipal(rule, context)
	case "R03":
		return evalBoolean(context, rule, []string{rule.Attribute}, false)
	case "R04":
		return evalIngress(rule, context)
	case "R05":
		return evalBoolean(context, rule, []string{rule.Attribute}, true)
	case "R06":
		return evalBoolean(context, rule, []string{rule.Attribute, "rotation_enabled", "enable_key_rotation", "key_rotation_enabled"}, false)
	default:
		return nil
	}
}

func evalACL(rule Rule, context evalContext) []ruleIssue {
	path := rule.Attribute
	if context.isUnknown(path) {
		return []ruleIssue{{Path: path, Verdict: VerdictUncertain, Detail: "bucket ACL is unknown", Unknown: true}}
	}
	value, hasValue := context.value(path)
	if !hasValue {
		value = ""
	}
	if displayValue(value) != "private" {
		return []ruleIssue{{Path: path, Verdict: VerdictViolation, Detail: "bucket ACL is not private", HasVal: hasValue, Value: value}}
	}
	return nil
}

func evalBoolean(context evalContext, rule Rule, paths []string, violateOn bool) []ruleIssue {
	issues := make([]ruleIssue, 0)
	for _, path := range paths {
		if context.isUnknown(path) {
			issues = append(issues, ruleIssue{Path: path, Verdict: VerdictUncertain, Detail: "boolean attribute is unknown", Unknown: true})
			continue
		}
		value, ok := context.value(path)
		if !ok {
			continue
		}
		parsed, err := booleanValue(value)
		if err != nil {
			issues = append(issues, ruleIssue{Path: path, Verdict: VerdictError, Detail: "boolean attribute has an invalid literal", HasVal: true, Value: value})
			continue
		}
		if parsed == violateOn {
			detail := "boolean attribute has the violating value"
			if violateOn {
				detail = "boolean attribute is true"
			} else {
				detail = "boolean attribute is false"
			}
			issues = append(issues, ruleIssue{Path: path, Verdict: VerdictViolation, Detail: detail, HasVal: true, Value: value})
		}
	}
	return issues
}

func booleanValue(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(typed))
	case json.Number:
		return strconv.ParseBool(typed.String())
	case float64:
		if typed == 0 || typed == 1 {
			return typed == 1, nil
		}
	case int:
		if typed == 0 || typed == 1 {
			return typed == 1, nil
		}
	case int64:
		if typed == 0 || typed == 1 {
			return typed == 1, nil
		}
	}
	return false, errors.New("invalid boolean")
}

func evalPrincipal(rule Rule, context evalContext) []ruleIssue {
	for _, path := range []string{rule.Attribute, "Principal"} {
		if context.isUnknown(rule.Attribute) || context.isUnknown(path) {
			return []ruleIssue{{Path: rule.Attribute, Verdict: VerdictUncertain, Detail: "principal is unknown", Unknown: true}}
		}
		if value, ok := context.value(path); ok && principalHasStar(value) {
			return []ruleIssue{{Path: rule.Attribute, Verdict: VerdictViolation, Detail: "policy principal contains a wildcard", HasVal: true, Value: value}}
		}
	}

	policyPaths := make([]string, 0)
	for path := range context.attrs {
		lower := strings.ToLower(path)
		if (strings.Contains(lower, "policy") || strings.Contains(lower, "principal")) && path != rule.Attribute && path != "Principal" {
			policyPaths = append(policyPaths, path)
		}
	}
	sort.Strings(policyPaths)
	for _, path := range policyPaths {
		if context.isUnknown(path) {
			return []ruleIssue{{Path: path, Verdict: VerdictUncertain, Detail: "policy is unknown", Unknown: true}}
		}
		if value, ok := context.value(path); ok && policyHasWildcardPrincipal(value) {
			return []ruleIssue{{Path: path, Verdict: VerdictViolation, Detail: "policy principal contains a wildcard", HasVal: true, Value: value}}
		}
	}
	return nil
}

func principalHasStar(value any) bool {
	switch typed := value.(type) {
	case string:
		var document any
		if err := json.Unmarshal([]byte(typed), &document); err == nil && policyHasWildcardPrincipal(document) {
			return true
		}
		return strings.Contains(typed, "*")
	case map[string]any:
		for _, child := range typed {
			if principalHasStar(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if principalHasStar(child) {
				return true
			}
		}
	default:
		return strings.Contains(displayValue(value), "*")
	}
	return false
}

func policyHasWildcardPrincipal(value any) bool {
	if encoded, ok := value.(string); ok {
		var document any
		if err := json.Unmarshal([]byte(encoded), &document); err != nil {
			return strings.Contains(encoded, "Principal") && strings.Contains(encoded, "*")
		}
		value = document
	}
	return documentHasWildcardPrincipal(value)
}

func documentHasWildcardPrincipal(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "Principal") && principalHasStar(child) {
				return true
			}
			if documentHasWildcardPrincipal(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if documentHasWildcardPrincipal(child) {
				return true
			}
		}
	}
	return false
}

func evalIngress(rule Rule, context evalContext) []ruleIssue {
	for _, path := range []string{rule.Attribute, "cidr_blocks", "ipv6_cidr_blocks"} {
		if context.isUnknown(path) {
			return []ruleIssue{{Path: rule.Attribute, Verdict: VerdictUncertain, Detail: "ingress is unknown", Unknown: true}}
		}
		value, ok := context.value(path)
		if !ok || !valueHasOpenCIDR(value) {
			continue
		}
		reportedPath := path
		if path != rule.Attribute {
			if _, hasIngress := context.value(rule.Attribute); hasIngress {
				reportedPath = rule.Attribute
			}
		}
		return []ruleIssue{{Path: reportedPath, Verdict: VerdictViolation, Detail: "ingress is open to the world", HasVal: true, Value: value}}
	}
	return nil
}

func valueHasOpenCIDR(value any) bool {
	encoded := displayValue(value)
	return strings.Contains(encoded, "0.0.0.0/0") || strings.Contains(encoded, "::/0")
}

func (e *IacPlanEvaluator) Explain(findingID string) (Trace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	for _, snapshotID := range e.order {
		if trace, ok := e.traces[snapshotID][findingID]; ok {
			return cloneTrace(trace), nil
		}
	}
	return Trace{}, ErrNoFinding
}

func (e *IacPlanEvaluator) Diff(prev, next Snapshot) ([]Delta, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	previous := make(map[string]Resource, len(prev.Resources))
	for _, resource := range prev.Resources {
		previous[resource.Address] = resource
	}
	following := make(map[string]Resource, len(next.Resources))
	for _, resource := range next.Resources {
		following[resource.Address] = resource
	}
	deltas := make([]Delta, 0)
	for address, prevResource := range previous {
		nextResource, ok := following[address]
		if !ok {
			deltas = append(deltas, Delta{Address: address, Kind: DeltaRemoved})
			continue
		}
		if paths := diffAttributes(prevResource, nextResource); len(paths) > 0 {
			deltas = append(deltas, Delta{Address: address, Kind: DeltaChanged, Paths: paths})
		}
	}
	for address := range following {
		if _, ok := previous[address]; !ok {
			deltas = append(deltas, Delta{Address: address, Kind: DeltaAdded})
		}
	}
	sort.SliceStable(deltas, func(i, j int) bool { return deltas[i].Address < deltas[j].Address })
	return deltas, nil
}

func diffAttributes(prev, next Resource) []string {
	prevAttributes := effectiveStringAttributes(prev)
	nextAttributes := effectiveStringAttributes(next)
	pathSet := make(map[string]struct{})
	for path := range prevAttributes {
		pathSet[path] = struct{}{}
	}
	for path := range nextAttributes {
		pathSet[path] = struct{}{}
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		if prevAttributes[path] != nextAttributes[path] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func effectiveStringAttributes(resource Resource) map[string]string {
	if resource.Action == ActionDelete {
		return resource.Before
	}
	return resource.After
}

func (e *IacPlanEvaluator) Snapshot() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()

	ruleIDs := make([]string, 0, len(e.rules))
	for _, rule := range e.rules {
		ruleIDs = append(ruleIDs, rule.ID)
	}
	state := persistedState{
		Version: persistenceVersion, Limits: e.limits, RuleIDs: ruleIDs, Seq: e.seq,
		Order: append([]string(nil), e.order...), Snapshots: cloneSnapshotMap(e.snaps),
		RawPlans: cloneByteMap(e.rawPlans), Findings: cloneFindingMap(e.findings), Traces: cloneTraceMap(e.traces),
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("%w: encode state: %v", ErrBadSnapshot, err)
	}
	digest := sha256.Sum256(payload)
	data, err := json.Marshal(snapshotEnvelope{Version: persistenceVersion, Checksum: hex.EncodeToString(digest[:]), State: payload})
	if err != nil {
		return nil, fmt.Errorf("%w: encode envelope: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

func (e *IacPlanEvaluator) Restore(data []byte) error {
	state, attributes, rules, err := decodePersistedSnapshot(data)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	e.limits = state.Limits
	e.rules = rules
	e.seq = state.Seq
	e.order = append([]string(nil), state.Order...)
	e.snaps = state.Snapshots
	e.rawPlans = state.RawPlans
	e.attrs = attributes
	e.findings = state.Findings
	e.traces = state.Traces
	return nil
}

func decodePersistedSnapshot(data []byte) (persistedState, map[string]map[string]attributeData, []Rule, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return persistedState{}, nil, nil, fmt.Errorf("%w: invalid envelope: %v", ErrBadSnapshot, err)
	}
	if envelope.Version != persistenceVersion || envelope.Checksum == "" || len(envelope.State) == 0 {
		return persistedState{}, nil, nil, fmt.Errorf("%w: incomplete envelope", ErrBadSnapshot)
	}
	var state persistedState
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return persistedState{}, nil, nil, fmt.Errorf("%w: invalid state: %v", ErrBadSnapshot, err)
	}
	canonical, err := json.Marshal(state)
	if err != nil {
		return persistedState{}, nil, nil, fmt.Errorf("%w: verify state: %v", ErrBadSnapshot, err)
	}
	digest := sha256.Sum256(canonical)
	expected, err := hex.DecodeString(envelope.Checksum)
	if err != nil || subtle.ConstantTimeCompare(expected, digest[:]) != 1 {
		return persistedState{}, nil, nil, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}
	if state.Version != persistenceVersion || state.Limits.MaxResources < 0 || state.Limits.MaxFindings < 0 || state.Seq < 0 {
		return persistedState{}, nil, nil, fmt.Errorf("%w: invalid version or limits", ErrBadSnapshot)
	}
	rules, err := restoreRules(state.RuleIDs)
	if err != nil {
		return persistedState{}, nil, nil, err
	}
	if state.Snapshots == nil || state.RawPlans == nil || state.Findings == nil || state.Traces == nil || len(state.Order) != len(state.Snapshots) {
		return persistedState{}, nil, nil, fmt.Errorf("%w: incomplete state", ErrBadSnapshot)
	}

	knownRules := builtinRuleMap()
	attributes := make(map[string]map[string]attributeData, len(state.Order))
	seenOrder := make(map[string]struct{}, len(state.Order))
	for _, id := range state.Order {
		if id == "" {
			return persistedState{}, nil, nil, fmt.Errorf("%w: empty snapshot id", ErrBadSnapshot)
		}
		if _, dup := seenOrder[id]; dup {
			return persistedState{}, nil, nil, fmt.Errorf("%w: duplicate snapshot", ErrBadSnapshot)
		}
		seenOrder[id] = struct{}{}
		snapshot, ok := state.Snapshots[id]
		if !ok || snapshot.ID != id || len(snapshot.Digest) != 64 {
			return persistedState{}, nil, nil, fmt.Errorf("%w: snapshot %s incomplete", ErrBadSnapshot, id)
		}
		if _, err := hex.DecodeString(snapshot.Digest); err != nil {
			return persistedState{}, nil, nil, fmt.Errorf("%w: bad digest", ErrBadSnapshot)
		}
		rawPlan, ok := state.RawPlans[id]
		if !ok || len(rawPlan) == 0 {
			return persistedState{}, nil, nil, fmt.Errorf("%w: missing raw plan", ErrBadSnapshot)
		}
		parsed, parsedAttributes, err := parsePlan(rawPlan)
		if err != nil {
			return persistedState{}, nil, nil, fmt.Errorf("%w: invalid raw plan: %v", ErrBadSnapshot, err)
		}
		if parsed.Digest != snapshot.Digest || !reflect.DeepEqual(parsed.Resources, snapshot.Resources) {
			return persistedState{}, nil, nil, fmt.Errorf("%w: snapshot drift", ErrBadSnapshot)
		}
		attributes[id] = parsedAttributes
	}

	for snapshotID, findings := range state.Findings {
		if _, ok := state.Snapshots[snapshotID]; !ok {
			return persistedState{}, nil, nil, fmt.Errorf("%w: orphan findings", ErrBadSnapshot)
		}
		traces := state.Traces[snapshotID]
		if traces == nil {
			return persistedState{}, nil, nil, fmt.Errorf("%w: missing traces", ErrBadSnapshot)
		}
		for _, finding := range findings {
			if finding.ID == "" || finding.RuleID == "" || finding.Address == "" || finding.Path == "" {
				return persistedState{}, nil, nil, fmt.Errorf("%w: incomplete finding", ErrBadSnapshot)
			}
			if _, ok := knownRules[finding.RuleID]; !ok || finding.ID != findingID(finding.RuleID, finding.Address, finding.Path) {
				return persistedState{}, nil, nil, fmt.Errorf("%w: finding drift", ErrBadSnapshot)
			}
			trace, ok := traces[finding.ID]
			if !ok || trace.FindingID != finding.ID || len(trace.Steps) < 2 {
				return persistedState{}, nil, nil, fmt.Errorf("%w: trace drift", ErrBadSnapshot)
			}
		}
	}
	for snapshotID, traces := range state.Traces {
		if _, ok := state.Snapshots[snapshotID]; !ok {
			return persistedState{}, nil, nil, fmt.Errorf("%w: orphan trace snapshot", ErrBadSnapshot)
		}
		allowed := make(map[string]struct{})
		for _, finding := range state.Findings[snapshotID] {
			allowed[finding.ID] = struct{}{}
		}
		for id, trace := range traces {
			if id != trace.FindingID {
				return persistedState{}, nil, nil, fmt.Errorf("%w: trace key drift", ErrBadSnapshot)
			}
			if _, ok := allowed[id]; !ok {
				return persistedState{}, nil, nil, fmt.Errorf("%w: orphan trace", ErrBadSnapshot)
			}
		}
	}
	return state, attributes, rules, nil
}

func restoreRules(ids []string) ([]Rule, error) {
	known := builtinRuleMap()
	rules := make([]Rule, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		rule, ok := known[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownRule, id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		rules = append(rules, cloneRule(rule))
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules, nil
}

func (e *IacPlanEvaluator) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rules == nil {
		return cloneRules(builtinRules())
	}
	return cloneRules(e.rules)
}

func (e *IacPlanEvaluator) SetRules(ids []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	rules, err := restoreRules(ids)
	if err != nil {
		return err
	}
	e.rules = rules
	return nil
}

func (e *IacPlanEvaluator) Findings(snapshotID string) ([]Finding, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	findings, ok := e.findings[snapshotID]
	if !ok {
		return nil, false
	}
	return cloneFindings(findings), true
}

func (e *IacPlanEvaluator) Snapshots() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.order...)
}

func (e *IacPlanEvaluator) Reset() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq = 0
	e.order = nil
	e.snaps = make(map[string]Snapshot)
	e.rawPlans = make(map[string][]byte)
	e.attrs = make(map[string]map[string]attributeData)
	e.findings = make(map[string][]Finding)
	e.traces = make(map[string]map[string]Trace)
	return nil
}

func cloneRules(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	for i, rule := range rules {
		out[i] = cloneRule(rule)
	}
	return out
}

func cloneRule(rule Rule) Rule {
	rule.Types = append([]string(nil), rule.Types...)
	return rule
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Resources = append([]Resource(nil), snapshot.Resources...)
	for i := range snapshot.Resources {
		snapshot.Resources[i].Before = cloneStringMap(snapshot.Resources[i].Before)
		snapshot.Resources[i].After = cloneStringMap(snapshot.Resources[i].After)
		snapshot.Resources[i].Unknown = append([]string(nil), snapshot.Resources[i].Unknown...)
		snapshot.Resources[i].Sensitive = append([]string(nil), snapshot.Resources[i].Sensitive...)
	}
	return snapshot
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneFindings(findings []Finding) []Finding {
	return append([]Finding(nil), findings...)
}

func cloneTrace(trace Trace) Trace {
	trace.Steps = append([]string(nil), trace.Steps...)
	return trace
}

func cloneSnapshotMap(values map[string]Snapshot) map[string]Snapshot {
	out := make(map[string]Snapshot, len(values))
	for key, value := range values {
		out[key] = cloneSnapshot(value)
	}
	return out
}

func cloneByteMap(values map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(values))
	for key, value := range values {
		out[key] = append([]byte(nil), value...)
	}
	return out
}

func cloneFindingMap(values map[string][]Finding) map[string][]Finding {
	out := make(map[string][]Finding, len(values))
	for key, value := range values {
		out[key] = cloneFindings(value)
	}
	return out
}

func cloneTraceMap(values map[string]map[string]Trace) map[string]map[string]Trace {
	out := make(map[string]map[string]Trace, len(values))
	for snapshotID, traces := range values {
		out[snapshotID] = make(map[string]Trace, len(traces))
		for findingID, trace := range traces {
			out[snapshotID][findingID] = cloneTrace(trace)
		}
	}
	return out
}
