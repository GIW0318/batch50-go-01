// Package task plans privilege escalation paths across a cluster RBAC graph and
// keeps the reasoning traceable across reloads and restarts.
package task

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Object kinds accepted by Load.
const (
	KindClusterRole        = "ClusterRole"
	KindRole               = "Role"
	KindClusterRoleBinding = "ClusterRoleBinding"
	KindRoleBinding        = "RoleBinding"
	KindServiceAccount     = "ServiceAccount"
)

// Subject kinds that may appear as a binding subject or as a query subject.
const (
	SubjectUser           = "User"
	SubjectGroup          = "Group"
	SubjectServiceAccount = "ServiceAccount"
)

// Ways a step of a path may be taken.
const (
	ViaDirect      = "direct"
	ViaAggregate   = "aggregate"
	ViaEscalate    = "escalate"
	ViaBind        = "bind"
	ViaImpersonate = "impersonate"
	ViaBuiltin     = "builtin"
)

// Verbs and resource names that move a subject onto another binding. The
// built in administrator group never needs a binding of its own.
const (
	VerbEscalate      = "escalate"
	VerbBind          = "bind"
	VerbImpersonate   = "impersonate"
	VerbWildcard      = "*"
	ResourceWildcard  = "*"
	SubresourceMarker = "/"
	BuiltinAdminGroup = "system:masters"
)

// Delta operations reported by Diff.
const (
	DeltaAdded   = "added"
	DeltaRemoved = "removed"
	DeltaChanged = "changed"
)

const (
	snapshotFormat = "rbac-escalation-planner.snapshot/v1"
	snapshotSchema = 1
)

// Errors returned by the planner.
var (
	ErrBadObject       = errors.New("object kind or name is invalid")
	ErrDuplicateObject = errors.New("object already loaded")
	ErrObjectLimit     = errors.New("object limit reached")
	ErrPathLimit       = errors.New("path limit reached")
	ErrNoPath          = errors.New("no escalation path within the hop limit")
	ErrPathNotFound    = errors.New("unknown path id")
	ErrBadSubject      = errors.New("subject name is required")
	ErrBadTarget       = errors.New("target resource and verb are required")
	ErrAggregateCycle  = errors.New("aggregate label selector is cyclic")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Rule is one policy rule of a role. Condition is optional: a rule carrying a
// condition is only usable once the planner has been told the condition holds.
type Rule struct {
	APIGroups     []string `json:"api_groups"`
	Resources     []string `json:"resources"`
	Verbs         []string `json:"verbs"`
	ResourceNames []string `json:"resource_names"`
	Condition     string   `json:"condition"`
}

// SubjectRef is one subject listed on a binding.
type SubjectRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Object is any node of the graph: a role, a binding or a service account.
// Aggregate holds the label selector of a cluster role that aggregates others.
type Object struct {
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Rules     []Rule            `json:"rules"`
	RoleRef   string            `json:"role_ref"`
	Subjects  []SubjectRef      `json:"subjects"`
	Labels    map[string]string `json:"labels"`
	Aggregate map[string]string `json:"aggregate"`
}

// Subject is the actor a path starts from. Groups carries every group the
// actor belongs to, so a single query can cover nested group membership.
type Subject struct {
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Groups    []string `json:"groups"`
}

// Target is the privilege the path must reach.
type Target struct {
	Namespace string `json:"namespace"`
	Resource  string `json:"resource"`
	Verb      string `json:"verb"`
	Name      string `json:"name"`
}

// Step is one hop of a path: the binding that was used and what it granted.
type Step struct {
	Ordinal int    `json:"ordinal"`
	Binding string `json:"binding"`
	Role    string `json:"role"`
	Via     string `json:"via"`
	Grant   string `json:"grant"`
}

// Path is one reachable escalation chain, ordered by hops then path id.
type Path struct {
	PathID  string  `json:"path_id"`
	Subject Subject `json:"subject"`
	Target  Target  `json:"target"`
	Hops    int     `json:"hops"`
	Steps   []Step  `json:"steps"`
}

// Delta is one difference between two graph revisions.
type Delta struct {
	Op        string `json:"op"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
}

// Graph is an immutable revision of the loaded objects.
type Graph struct {
	Version     int      `json:"version"`
	ObjectCount int      `json:"object_count"`
	Objects     []Object `json:"objects"`
}

// Limits bounds the graph, the search depth and the number of paths kept.
type Limits struct {
	MaxObjects        int
	MaxHops           int
	MaxPaths          int
	MaxAggregateDepth int
}

// RbacEscalationPlanner owns the loaded graph and the paths computed from it.
type RbacEscalationPlanner struct {
	limits  Limits
	current atomic.Pointer[graphState]
}

type objectKey struct {
	Kind      string
	Namespace string
	Name      string
}

func (k objectKey) String() string {
	return k.Kind + "/" + k.Namespace + "/" + k.Name
}

type effectiveRule struct {
	Rule
	Source     objectKey
	Aggregated bool
}

type graphState struct {
	graph      Graph
	objects    map[objectKey]Object
	order      []objectKey
	aggDirect  map[string][]string
	aggregated map[string][]string
	effective  map[objectKey][]effectiveRule
	pathMu     sync.RWMutex
	paths      map[string]Path
}

type snapshotData struct {
	Format     string              `json:"format"`
	Schema     int                 `json:"schema"`
	Limits     Limits              `json:"limits"`
	Graph      Graph               `json:"graph"`
	Aggregates map[string][]string `json:"aggregates"`
	Paths      []Path              `json:"paths"`
}

type specialPermission struct {
	verb       string
	resource   string
	grant      string
	rule       Rule
	source     objectKey
	aggregated bool
}

type trail map[string]struct{}

// NewRbacEscalationPlanner builds a planner with the given limits.
func NewRbacEscalationPlanner(limits Limits) *RbacEscalationPlanner {
	return &RbacEscalationPlanner{limits: limits}
}

// Load validates and installs a whole batch of objects as one revision.
func (p *RbacEscalationPlanner) Load(objects []Object) error {
	version := 1
	if current := p.current.Load(); current != nil {
		version = current.graph.Version + 1
	}

	next, err := buildState(objects, version, p.limits)
	if err != nil {
		return err
	}
	p.current.Store(next)
	return nil
}

// Graph returns the active graph revision.
func (p *RbacEscalationPlanner) Graph() Graph {
	current := p.current.Load()
	if current == nil {
		return Graph{}
	}
	return current.snapshotGraph()
}

// Holds reports whether the subject already has the target privilege.
func (p *RbacEscalationPlanner) Holds(subject Subject, target Target) bool {
	if validateSubject(subject) != nil || validateTarget(target) != nil {
		return false
	}
	current := p.current.Load()
	if current == nil {
		return false
	}
	return current.subjectHolds(canonicalSubject(subject), target)
}

// Paths returns every escalation path that reaches the target.
func (p *RbacEscalationPlanner) Paths(subject Subject, target Target) ([]Path, error) {
	if err := validateSubject(subject); err != nil {
		return nil, err
	}
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	current := p.current.Load()
	if current == nil {
		return nil, fmt.Errorf("%w: no graph loaded", ErrNoPath)
	}

	paths, err := current.computePaths(canonicalSubject(subject), target, p.limits)
	if err != nil {
		return nil, err
	}
	current.rememberPaths(paths)
	return clonePaths(paths), nil
}

// Explain returns the steps of one computed path in ordinal order.
func (p *RbacEscalationPlanner) Explain(pathID string) ([]Step, error) {
	current := p.current.Load()
	if current == nil {
		return nil, fmt.Errorf("%w: %q", ErrPathNotFound, pathID)
	}
	steps, ok := current.lookupPath(pathID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPathNotFound, pathID)
	}
	return cloneSteps(steps), nil
}

// Diff compares two graph revisions and reports added, removed and changed objects.
func (p *RbacEscalationPlanner) Diff(prev, next Graph) ([]Delta, error) {
	previous := make(map[objectKey]Object, len(prev.Objects))
	for _, object := range prev.Objects {
		previous[keyOf(object)] = object
	}
	following := make(map[objectKey]Object, len(next.Objects))
	for _, object := range next.Objects {
		following[keyOf(object)] = object
	}

	keys := make(map[objectKey]struct{}, len(previous)+len(following))
	for key := range previous {
		keys[key] = struct{}{}
	}
	for key := range following {
		keys[key] = struct{}{}
	}
	orderedKeys := make([]objectKey, 0, len(keys))
	for key := range keys {
		orderedKeys = append(orderedKeys, key)
	}
	sortObjectKeys(orderedKeys)

	deltas := make([]Delta, 0, len(orderedKeys))
	for _, key := range orderedKeys {
		before, inBefore := previous[key]
		after, inAfter := following[key]
		switch {
		case inBefore && !inAfter:
			deltas = append(deltas, Delta{Op: DeltaRemoved, Kind: key.Kind, Namespace: key.Namespace, Name: key.Name})
		case !inBefore && inAfter:
			deltas = append(deltas, Delta{Op: DeltaAdded, Kind: key.Kind, Namespace: key.Namespace, Name: key.Name})
		case !reflect.DeepEqual(before, after):
			deltas = append(deltas, Delta{
				Op:        DeltaChanged,
				Kind:      key.Kind,
				Namespace: key.Namespace,
				Name:      key.Name,
				Detail:    describeChange(before, after),
			})
		}
	}
	return deltas, nil
}

// Privileges lists the privileges held by the subject as sorted "verb resource" pairs.
func (p *RbacEscalationPlanner) Privileges(subject Subject) []string {
	if validateSubject(subject) != nil {
		return []string{}
	}
	current := p.current.Load()
	if current == nil {
		return []string{}
	}
	identity := canonicalSubject(subject)
	if isBuiltinAdmin(identity) {
		return []string{VerbWildcard + " " + ResourceWildcard}
	}

	held := make(map[string]struct{})
	for _, binding := range current.matchingBindings(identity, "", false) {
		roleKey, _, ok := current.resolveRole(binding)
		if !ok {
			continue
		}
		for _, grant := range current.effective[roleKey] {
			if strings.TrimSpace(grant.Condition) != "" {
				continue
			}
			for _, verb := range grant.Verbs {
				for _, resource := range grant.Resources {
					held[verb+" "+resource] = struct{}{}
				}
			}
		}
	}

	result := make([]string, 0, len(held))
	for grant := range held {
		result = append(result, grant)
	}
	sort.Strings(result)
	return result
}

// Aggregated lists the roles pulled in by a cluster role label selector.
func (p *RbacEscalationPlanner) Aggregated(role string) []string {
	current := p.current.Load()
	if current == nil {
		return []string{}
	}
	children, ok := current.aggregated[role]
	if !ok {
		return []string{}
	}
	return append([]string{}, children...)
}

// Snapshot serialises the graph revision, aggregates and computed paths.
func (p *RbacEscalationPlanner) Snapshot() ([]byte, error) {
	payload := snapshotData{
		Format:     snapshotFormat,
		Schema:     snapshotSchema,
		Limits:     p.limits,
		Aggregates: map[string][]string{},
		Paths:      []Path{},
	}

	current := p.current.Load()
	if current != nil {
		payload.Graph = current.snapshotGraph()
		payload.Aggregates = cloneStringSliceMap(current.aggregated)
		payload.Paths = current.snapshotPaths()
	} else {
		payload.Graph = Graph{Objects: []Object{}}
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *RbacEscalationPlanner) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("%w: empty payload", ErrBadSnapshot)
	}

	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrBadSnapshot, err)
	}
	if payload.Format != snapshotFormat || payload.Schema != snapshotSchema {
		return fmt.Errorf("%w: unknown format", ErrBadSnapshot)
	}
	if payload.Graph.Version < 0 || payload.Graph.ObjectCount < 0 {
		return fmt.Errorf("%w: negative graph revision fields", ErrBadSnapshot)
	}
	if payload.Graph.ObjectCount != len(payload.Graph.Objects) {
		return fmt.Errorf("%w: object count does not match objects", ErrBadSnapshot)
	}
	if len(payload.Graph.Objects) > 0 && payload.Graph.Version == 0 {
		return fmt.Errorf("%w: non-empty graph has no version", ErrBadSnapshot)
	}

	next, err := buildState(payload.Graph.Objects, payload.Graph.Version, p.limits)
	if err != nil {
		return fmt.Errorf("%w: rebuild graph: %v", ErrBadSnapshot, err)
	}
	if !stringSliceMapsEqual(payload.Aggregates, next.aggregated) {
		return fmt.Errorf("%w: aggregate expansion mismatch", ErrBadSnapshot)
	}

	seenIDs := make(map[string]struct{}, len(payload.Paths))
	for _, path := range payload.Paths {
		if err := validateStoredPath(path); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
		if _, duplicated := seenIDs[path.PathID]; duplicated {
			return fmt.Errorf("%w: duplicate path id %q", ErrBadSnapshot, path.PathID)
		}
		seenIDs[path.PathID] = struct{}{}

		expectedID := fingerprintPath(path.Subject, path.Target, path.Steps)
		if expectedID != path.PathID {
			return fmt.Errorf("%w: path id mismatch for %q", ErrBadSnapshot, path.PathID)
		}
		computed, searchErr := next.computePaths(
			canonicalSubject(path.Subject), path.Target, p.limits)
		if searchErr != nil {
			return fmt.Errorf("%w: path %q cannot be rebuilt: %v", ErrBadSnapshot, path.PathID, searchErr)
		}
		if !containsPathID(computed, path.PathID) {
			return fmt.Errorf("%w: path %q is not reproducible", ErrBadSnapshot, path.PathID)
		}
	}

	next.rememberPaths(payload.Paths)
	p.current.Store(next)
	return nil
}

// Reset drops the graph and every computed path.
func (p *RbacEscalationPlanner) Reset() {
	p.current.Store(nil)
}

func buildState(objects []Object, version int, limits Limits) (*graphState, error) {
	if len(objects) > limits.MaxObjects {
		return nil, fmt.Errorf("%w: %d objects exceed %d", ErrObjectLimit, len(objects), limits.MaxObjects)
	}

	state := &graphState{
		objects:    make(map[objectKey]Object, len(objects)),
		order:      make([]objectKey, 0, len(objects)),
		aggDirect:  map[string][]string{},
		aggregated: map[string][]string{},
		effective:  map[objectKey][]effectiveRule{},
		paths:      map[string]Path{},
	}
	seen := make(map[objectKey]struct{}, len(objects))

	for _, source := range objects {
		if !knownObjectKind(source.Kind) || strings.TrimSpace(source.Name) == "" {
			return nil, fmt.Errorf("%w: kind=%q name=%q", ErrBadObject, source.Kind, source.Name)
		}
		object := cloneObject(source)
		key := keyOf(object)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateObject, key)
		}
		seen[key] = struct{}{}
		state.objects[key] = object
		state.order = append(state.order, key)
	}

	if err := state.buildAggregates(limits); err != nil {
		return nil, err
	}
	for _, key := range state.order {
		state.effective[key] = state.buildEffectiveRules(key, map[objectKey]bool{})
	}

	state.graph = Graph{
		Version:     version,
		ObjectCount: len(objects),
		Objects:     cloneObjects(objects),
	}
	return state, nil
}

func knownObjectKind(kind string) bool {
	switch kind {
	case KindClusterRole, KindRole, KindClusterRoleBinding, KindRoleBinding, KindServiceAccount:
		return true
	default:
		return false
	}
}

func isClusterKind(kind string) bool {
	return kind == KindClusterRole || kind == KindClusterRoleBinding
}

func isBindingKind(kind string) bool {
	return kind == KindRoleBinding || kind == KindClusterRoleBinding
}

func keyOf(object Object) objectKey {
	return makeObjectKey(object.Kind, object.Namespace, object.Name)
}

func makeObjectKey(kind, namespace, name string) objectKey {
	if isClusterKind(kind) {
		namespace = ""
	}
	return objectKey{Kind: kind, Namespace: namespace, Name: name}
}

func sortObjectKeys(keys []objectKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
}

func cloneObjects(objects []Object) []Object {
	if objects == nil {
		return nil
	}
	result := make([]Object, len(objects))
	for i := range objects {
		result[i] = cloneObject(objects[i])
	}
	return result
}

func cloneObject(object Object) Object {
	result := object
	result.Rules = cloneRules(object.Rules)
	result.Subjects = append([]SubjectRef(nil), object.Subjects...)
	result.Labels = cloneStringMap(object.Labels)
	result.Aggregate = cloneStringMap(object.Aggregate)
	return result
}

func cloneRules(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	result := make([]Rule, len(rules))
	for i, rule := range rules {
		result[i] = rule
		result[i].APIGroups = append([]string(nil), rule.APIGroups...)
		result[i].Resources = append([]string(nil), rule.Resources...)
		result[i].Verbs = append([]string(nil), rule.Verbs...)
		result[i].ResourceNames = append([]string(nil), rule.ResourceNames...)
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStringSliceMap(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string{}, values...)
	}
	return result
}

func stringSliceMapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, valuesA := range a {
		valuesB, ok := b[key]
		if !ok || !reflect.DeepEqual(valuesA, valuesB) {
			return false
		}
	}
	return true
}

func cloneSubject(subject Subject) Subject {
	result := subject
	result.Groups = append([]string(nil), subject.Groups...)
	return result
}

func cloneSteps(steps []Step) []Step {
	return append([]Step(nil), steps...)
}

func clonePath(path Path) Path {
	result := path
	result.Subject = cloneSubject(path.Subject)
	result.Steps = cloneSteps(path.Steps)
	return result
}

func clonePaths(paths []Path) []Path {
	result := make([]Path, len(paths))
	for i := range paths {
		result[i] = clonePath(paths[i])
	}
	return result
}

func (state *graphState) snapshotGraph() Graph {
	return Graph{
		Version:     state.graph.Version,
		ObjectCount: state.graph.ObjectCount,
		Objects:     cloneObjects(state.graph.Objects),
	}
}

func (state *graphState) buildAggregates(limits Limits) error {
	aggregateNames := make([]string, 0)
	for _, key := range state.order {
		object := state.objects[key]
		if object.Kind != KindClusterRole || len(object.Aggregate) == 0 {
			continue
		}
		selected := make([]string, 0)
		for _, candidateKey := range state.order {
			candidate := state.objects[candidateKey]
			if candidate.Kind == KindClusterRole && labelsMatch(candidate.Labels, object.Aggregate) {
				selected = append(selected, candidate.Name)
			}
		}
		sort.Strings(selected)
		selected = compactStrings(selected)
		state.aggDirect[object.Name] = selected
		aggregateNames = append(aggregateNames, object.Name)
	}
	sort.Strings(aggregateNames)

	for _, name := range aggregateNames {
		if err := state.walkAggregate(name, nil, limits.MaxAggregateDepth); err != nil {
			return err
		}
	}
	return nil
}

func (state *graphState) walkAggregate(name string, stack []string, maxDepth int) error {
	for _, ancestor := range stack {
		if ancestor == name {
			cycle := append(append([]string{}, stack...), name)
			return fmt.Errorf("%w: %s", ErrAggregateCycle, strings.Join(cycle, " -> "))
		}
	}

	direct := state.aggDirect[name]
	for _, child := range direct {
		if maxDepth > 0 && len(stack)+1 > maxDepth {
			return fmt.Errorf("%w: expansion depth exceeds %d", ErrAggregateCycle, maxDepth)
		}
		if err := state.walkAggregate(child, append(stack, name), maxDepth); err != nil {
			return err
		}
	}

	closure := make(map[string]struct{})
	for _, child := range direct {
		closure[child] = struct{}{}
		for _, descendant := range state.aggregated[child] {
			closure[descendant] = struct{}{}
		}
	}
	names := make([]string, 0, len(closure))
	for child := range closure {
		names = append(names, child)
	}
	sort.Strings(names)
	state.aggregated[name] = names
	return nil
}

func labelsMatch(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func (state *graphState) buildEffectiveRules(key objectKey, active map[objectKey]bool) []effectiveRule {
	if active[key] {
		return nil
	}
	active[key] = true

	object := state.objects[key]
	result := make([]effectiveRule, 0, len(object.Rules))
	for _, rule := range object.Rules {
		result = append(result, effectiveRule{Rule: cloneRule(rule), Source: key})
	}

	if key.Kind == KindClusterRole {
		for _, childName := range state.aggDirect[key.Name] {
			childKey := makeObjectKey(KindClusterRole, "", childName)
			for _, childRule := range state.buildEffectiveRules(childKey, active) {
				childRule.Aggregated = true
				result = append(result, childRule)
			}
		}
	}
	return result
}

func cloneRule(rule Rule) Rule {
	result := rule
	result.APIGroups = append([]string(nil), rule.APIGroups...)
	result.Resources = append([]string(nil), rule.Resources...)
	result.Verbs = append([]string(nil), rule.Verbs...)
	result.ResourceNames = append([]string(nil), rule.ResourceNames...)
	return result
}

func validateSubject(subject Subject) error {
	if strings.TrimSpace(subject.Name) == "" {
		return fmt.Errorf("%w: name is empty", ErrBadSubject)
	}
	return nil
}

func validateTarget(target Target) error {
	if strings.TrimSpace(target.Resource) == "" || strings.TrimSpace(target.Verb) == "" {
		return fmt.Errorf("%w: verb=%q resource=%q", ErrBadTarget, target.Verb, target.Resource)
	}
	return nil
}

func canonicalSubject(subject Subject) Subject {
	result := subject
	if strings.TrimSpace(result.Kind) == "" {
		result.Kind = SubjectUser
	}
	groups := append([]string(nil), subject.Groups...)
	sort.Strings(groups)
	result.Groups = compactStrings(groups)
	return result
}

func isBuiltinAdmin(subject Subject) bool {
	if subject.Kind == SubjectGroup && subject.Name == BuiltinAdminGroup {
		return true
	}
	for _, group := range subject.Groups {
		if group == BuiltinAdminGroup {
			return true
		}
	}
	return false
}

func subjectKey(subject Subject) string {
	canonical := canonicalSubject(subject)
	return "subject:" + strings.Join([]string{
		canonical.Kind, canonical.Namespace, canonical.Name, strings.Join(canonical.Groups, ","),
	}, "/")
}

func subjectLabel(subject Subject) string {
	if subject.Namespace != "" {
		return subject.Kind + ":" + subject.Namespace + "/" + subject.Name
	}
	return subject.Kind + ":" + subject.Name
}

func bindingMatchesSubject(binding Object, subject Subject, bindingNamespace string) bool {
	for _, ref := range binding.Subjects {
		if subjectRefMatches(ref, subject, bindingNamespace) {
			return true
		}
	}
	return false
}

func subjectRefMatches(ref SubjectRef, subject Subject, bindingNamespace string) bool {
	switch ref.Kind {
	case SubjectUser:
		return (subject.Kind == "" || subject.Kind == SubjectUser) && ref.Name == subject.Name
	case SubjectGroup:
		if subject.Kind == SubjectGroup && ref.Name == subject.Name {
			return true
		}
		for _, group := range subject.Groups {
			if group == ref.Name {
				return true
			}
		}
		return false
	case SubjectServiceAccount:
		if subject.Kind != SubjectServiceAccount || ref.Name != subject.Name {
			return false
		}
		refNamespace := ref.Namespace
		if refNamespace == "" {
			refNamespace = bindingNamespace
		}
		return subject.Namespace == "" || refNamespace == "" || refNamespace == subject.Namespace
	default:
		return false
	}
}

func bindingTargetsNamespace(binding Object, namespace string) bool {
	if binding.Kind == KindClusterRoleBinding {
		return true
	}
	return binding.Namespace == namespace
}

func (state *graphState) matchingBindings(subject Subject, namespace string, scopeToNamespace bool) []Object {
	result := make([]Object, 0)
	for _, key := range state.order {
		object := state.objects[key]
		if !isBindingKind(object.Kind) {
			continue
		}
		if scopeToNamespace && !bindingTargetsNamespace(object, namespace) {
			continue
		}
		if bindingMatchesSubject(object, subject, object.Namespace) {
			result = append(result, object)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := keyOf(result[i]), keyOf(result[j])
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		return left.Name < right.Name
	})
	return result
}

func (state *graphState) resolveRole(binding Object) (objectKey, Object, bool) {
	if binding.Kind == KindClusterRoleBinding {
		key := makeObjectKey(KindClusterRole, "", binding.RoleRef)
		role, ok := state.objects[key]
		return key, role, ok
	}

	namespaced := makeObjectKey(KindRole, binding.Namespace, binding.RoleRef)
	if role, ok := state.objects[namespaced]; ok {
		return namespaced, role, true
	}
	cluster := makeObjectKey(KindClusterRole, "", binding.RoleRef)
	if role, ok := state.objects[cluster]; ok {
		return cluster, role, true
	}
	return objectKey{}, Object{}, false
}

func ruleAllows(rule Rule, target Target) bool {
	if strings.TrimSpace(rule.Condition) != "" {
		return false
	}

	verbAllowed := false
	for _, verb := range rule.Verbs {
		if verb == VerbWildcard || verb == target.Verb {
			verbAllowed = true
			break
		}
	}
	if !verbAllowed {
		return false
	}

	resourceAllowed := false
	for _, resource := range rule.Resources {
		if resourcePatternMatches(resource, target.Resource) {
			resourceAllowed = true
			break
		}
	}
	if !resourceAllowed {
		return false
	}

	if len(rule.ResourceNames) > 0 {
		if target.Name == "" {
			return false
		}
		nameAllowed := false
		for _, name := range rule.ResourceNames {
			if name == target.Name {
				nameAllowed = true
				break
			}
		}
		if !nameAllowed {
			return false
		}
	}
	return true
}

func resourcePatternMatches(pattern, resource string) bool {
	if pattern == ResourceWildcard {
		return true
	}
	if pattern == resource {
		return true
	}
	patternParts := strings.Split(pattern, SubresourceMarker)
	resourceParts := strings.Split(resource, SubresourceMarker)
	if len(patternParts) != len(resourceParts) {
		return false
	}
	for i := range patternParts {
		if patternParts[i] == VerbWildcard {
			continue
		}
		if patternParts[i] != resourceParts[i] {
			return false
		}
	}
	return true
}

func roleGrantsTarget(rules []effectiveRule, target Target) (allowed bool, aggregated bool) {
	aggregateMatch := false
	for _, grant := range rules {
		if !ruleAllows(grant.Rule, target) {
			continue
		}
		if !grant.Aggregated {
			return true, false
		}
		aggregateMatch = true
	}
	return aggregateMatch, aggregateMatch
}

func (state *graphState) subjectHolds(subject Subject, target Target) bool {
	if isBuiltinAdmin(subject) {
		return true
	}
	for _, binding := range state.matchingBindings(subject, target.Namespace, true) {
		roleKey, _, ok := state.resolveRole(binding)
		if !ok {
			continue
		}
		if allowed, _ := roleGrantsTarget(state.effective[roleKey], target); allowed {
			return true
		}
	}
	return false
}

func targetGrant(target Target) string {
	return target.Verb + " " + target.Resource
}

type pathCollector struct {
	state  *graphState
	root   Subject
	target Target
	limits Limits
	paths  []Path
	ids    map[string]struct{}
}

func (state *graphState) computePaths(subject Subject, target Target, limits Limits) ([]Path, error) {
	collector := &pathCollector{
		state:  state,
		root:   subject,
		target: target,
		limits: limits,
		paths:  []Path{},
		ids:    map[string]struct{}{},
	}
	rootTrail := trail{subjectKey(subject): {}}
	if err := collector.fromSubject(subject, nil, rootTrail); err != nil {
		return nil, err
	}
	if len(collector.paths) == 0 {
		return nil, fmt.Errorf("%w: %s %s", ErrNoPath, targetGrant(target), target.Namespace)
	}
	sort.SliceStable(collector.paths, func(i, j int) bool {
		if collector.paths[i].Hops != collector.paths[j].Hops {
			return collector.paths[i].Hops < collector.paths[j].Hops
		}
		return collector.paths[i].PathID < collector.paths[j].PathID
	})
	return collector.paths, nil
}

func (collector *pathCollector) emit(steps []Step) error {
	clean := withOrdinals(steps)
	id := fingerprintPath(collector.root, collector.target, clean)
	if _, exists := collector.ids[id]; exists {
		return nil
	}
	if len(collector.paths) >= collector.limits.MaxPaths {
		return fmt.Errorf("%w: more than %d", ErrPathLimit, collector.limits.MaxPaths)
	}
	collector.ids[id] = struct{}{}
	collector.paths = append(collector.paths, Path{
		PathID:  id,
		Subject: cloneSubject(collector.root),
		Target:  collector.target,
		Hops:    len(clean),
		Steps:   clean,
	})
	return nil
}

func withOrdinals(steps []Step) []Step {
	result := cloneSteps(steps)
	for i := range result {
		result[i].Ordinal = i + 1
	}
	return result
}

func appendStep(prefix []Step, step Step) []Step {
	result := make([]Step, 0, len(prefix)+1)
	result = append(result, prefix...)
	result = append(result, step)
	return result
}

func (current trail) has(key string) bool {
	_, ok := current[key]
	return ok
}

func (current trail) add(key string) trail {
	next := make(trail, len(current)+1)
	for item := range current {
		next[item] = struct{}{}
	}
	next[key] = struct{}{}
	return next
}

func (collector *pathCollector) fromSubject(subject Subject, prefix []Step, visited trail) error {
	directRoutes := collector.directRoutes(subject, prefix, visited)
	if len(directRoutes) > 0 {
		for _, route := range directRoutes {
			if err := collector.emit(route); err != nil {
				return err
			}
		}
		return nil
	}

	if len(prefix) >= collector.limits.MaxHops {
		return nil
	}

	for _, binding := range collector.state.matchingBindings(subject, collector.target.Namespace, true) {
		bindingKey := keyOf(binding).String()
		if visited.has(bindingKey) {
			continue
		}
		roleKey, _, ok := collector.state.resolveRole(binding)
		if !ok || visited.has(roleKey.String()) {
			continue
		}

		nextVisited := visited.add(bindingKey).add(roleKey.String())
		base := Step{Binding: binding.Name, Role: binding.RoleRef}
		if err := collector.exploreRole(roleKey, base, prefix, nextVisited); err != nil {
			return err
		}
	}
	return nil
}

func (collector *pathCollector) directRoutes(subject Subject, prefix []Step, visited trail) [][]Step {
	routes := make([][]Step, 0)
	nextOrdinal := len(prefix) + 1
	if nextOrdinal > collector.limits.MaxHops {
		return routes
	}

	if isBuiltinAdmin(subject) {
		step := Step{
			Ordinal: nextOrdinal,
			Binding: BuiltinAdminGroup,
			Role:    BuiltinAdminGroup,
			Via:     ViaBuiltin,
			Grant:   targetGrant(collector.target),
		}
		routes = append(routes, appendStep(prefix, step))
	}

	for _, binding := range collector.state.matchingBindings(subject, collector.target.Namespace, true) {
		bindingKey := keyOf(binding).String()
		roleKey, _, ok := collector.state.resolveRole(binding)
		if !ok || visited.has(bindingKey) || visited.has(roleKey.String()) {
			continue
		}
		allowed, aggregated := roleGrantsTarget(collector.state.effective[roleKey], collector.target)
		if !allowed {
			continue
		}
		via := ViaDirect
		if aggregated {
			via = ViaAggregate
		}
		step := Step{
			Ordinal: nextOrdinal,
			Binding: binding.Name,
			Role:    binding.RoleRef,
			Via:     via,
			Grant:   targetGrant(collector.target),
		}
		routes = append(routes, appendStep(prefix, step))
	}
	return routes
}

func (collector *pathCollector) exploreRole(roleKey objectKey, base Step, prefix []Step, visited trail) error {
	base.Ordinal = len(prefix) + 1
	if base.Ordinal > collector.limits.MaxHops {
		return nil
	}

	rules := collector.state.effective[roleKey]
	if allowed, _ := roleGrantsTarget(rules, collector.target); allowed {
		if base.Via == "" {
			_, aggregated := roleGrantsTarget(rules, collector.target)
			if aggregated {
				base.Via = ViaAggregate
			} else {
				base.Via = ViaDirect
			}
		}
		base.Grant = targetGrant(collector.target)
		return collector.emit(appendStep(prefix, base))
	}

	permissions := specialPermissions(rules)
	for _, permission := range permissions {
		current := base
		current.Grant = permission.grant
		if current.Via == "" {
			current.Via = ViaDirect
			if permission.aggregated {
				current.Via = ViaAggregate
			}
		}
		rolePrefix := appendStep(prefix, current)

		switch permission.verb {
		case VerbBind, VerbEscalate:
			if err := collector.searchBoundRoles(permission, current.Binding, rolePrefix, visited); err != nil {
				return err
			}
		case VerbImpersonate:
			if err := collector.searchImpersonatedSubjects(permission, current.Binding, rolePrefix, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

func (collector *pathCollector) searchBoundRoles(permission specialPermission, bindingName string, prefix []Step, visited trail) error {
	if len(prefix) >= collector.limits.MaxHops {
		return nil
	}
	for _, role := range collector.state.escalationRoleCandidates(collector.target.Namespace) {
		roleKey := keyOf(role)
		if visited.has(roleKey.String()) || !permission.allowsRole(role) {
			continue
		}
		next := Step{
			Binding: bindingName,
			Role:    role.Name,
			Via:     permission.verb,
		}
		nextVisited := visited.add(roleKey.String())
		if err := collector.exploreRole(roleKey, next, prefix, nextVisited); err != nil {
			return err
		}
	}
	return nil
}

func (permission specialPermission) allowsRole(role Object) bool {
	if len(permission.rule.ResourceNames) == 0 {
		return true
	}
	scopedName := role.Namespace + "/" + role.Name
	for _, name := range permission.rule.ResourceNames {
		if name == role.Name || name == scopedName {
			return true
		}
	}
	return false
}

func (state *graphState) escalationRoleCandidates(namespace string) []Object {
	result := make([]Object, 0)
	for _, key := range state.order {
		object := state.objects[key]
		if object.Kind != KindClusterRole && object.Kind != KindRole {
			continue
		}
		if object.Kind == KindRole && object.Namespace != namespace {
			continue
		}
		result = append(result, object)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := keyOf(result[i]), keyOf(result[j])
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		return left.Name < right.Name
	})
	return result
}

func specialPermissions(rules []effectiveRule) []specialPermission {
	result := make([]specialPermission, 0)
	seen := make(map[string]struct{})
	specialVerbs := []string{VerbBind, VerbEscalate, VerbImpersonate}

	for _, rule := range rules {
		if strings.TrimSpace(rule.Condition) != "" {
			continue
		}
		for _, verbPattern := range rule.Verbs {
			for _, resource := range rule.Resources {
				for _, specialVerb := range specialVerbs {
					if verbPattern != VerbWildcard && verbPattern != specialVerb {
						continue
					}
					permission := specialPermission{
						verb:       specialVerb,
						resource:   resource,
						grant:      specialVerb + " " + resource,
						rule:       cloneRule(rule.Rule),
						source:     rule.Source,
						aggregated: rule.Aggregated,
					}
					key := strings.Join([]string{
						permission.verb, permission.resource, permission.source.String(),
						strings.Join(permission.rule.ResourceNames, ","),
					}, "|")
					if _, exists := seen[key]; exists {
						continue
					}
					seen[key] = struct{}{}
					result = append(result, permission)
				}
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].grant < result[j].grant
	})
	return result
}

func (collector *pathCollector) searchImpersonatedSubjects(permission specialPermission, bindingName string, rolePrefix []Step, visited trail) error {
	impersonateOrdinal := len(rolePrefix) + 1
	if impersonateOrdinal > collector.limits.MaxHops {
		return nil
	}

	for _, identity := range collector.impersonationCandidates(permission) {
		identityKey := subjectKey(identity)
		if visited.has(identityKey) {
			continue
		}
		impersonateStep := Step{
			Ordinal: impersonateOrdinal,
			Binding: bindingName,
			Role:    subjectLabel(identity),
			Via:     ViaImpersonate,
			Grant:   permission.grant,
		}
		prefix := appendStep(rolePrefix, impersonateStep)
		if err := collector.fromSubject(identity, prefix, visited.add(identityKey)); err != nil {
			return err
		}
	}
	return nil
}

func (collector *pathCollector) impersonationCandidates(permission specialPermission) []Subject {
	kinds := impersonateKinds(permission.resource)
	candidates := make([]Subject, 0)
	seen := make(map[string]struct{})
	add := func(subject Subject) {
		subject = canonicalSubject(subject)
		if subject.Name == "" || !kinds[subject.Kind] {
			return
		}
		key := subjectKey(subject)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, subject)
	}

	if len(permission.rule.ResourceNames) == 0 {
		for _, subject := range collector.state.knownSubjects() {
			add(subject)
		}
	} else {
		for _, resourceName := range permission.rule.ResourceNames {
			if kinds[SubjectUser] {
				add(Subject{Kind: SubjectUser, Name: resourceName})
			}
			if kinds[SubjectGroup] {
				add(Subject{Kind: SubjectGroup, Name: resourceName})
			}
			if kinds[SubjectServiceAccount] {
				namespace, name := collector.target.Namespace, resourceName
				if strings.Contains(resourceName, "/") {
					parts := strings.SplitN(resourceName, "/", 2)
					namespace, name = parts[0], parts[1]
				}
				add(Subject{Kind: SubjectServiceAccount, Namespace: namespace, Name: name})
			}
			for _, known := range collector.state.knownSubjects() {
				if known.Name == resourceName ||
					(known.Kind == SubjectServiceAccount && known.Namespace+"/"+known.Name == resourceName) {
					add(known)
				}
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return subjectKey(candidates[i]) < subjectKey(candidates[j])
	})
	return candidates
}

func impersonateKinds(resource string) map[string]bool {
	kinds := map[string]bool{}
	normalized := strings.ToLower(resource)
	if normalized == ResourceWildcard || normalized == "*/*" {
		kinds[SubjectUser] = true
		kinds[SubjectGroup] = true
		kinds[SubjectServiceAccount] = true
		return kinds
	}
	if strings.Contains(normalized, "serviceaccount") {
		kinds[SubjectServiceAccount] = true
	}
	if strings.Contains(normalized, "group") {
		kinds[SubjectGroup] = true
	}
	if strings.Contains(normalized, "user") {
		kinds[SubjectUser] = true
	}
	if len(kinds) == 0 {
		kinds[SubjectUser] = true
		kinds[SubjectGroup] = true
		kinds[SubjectServiceAccount] = true
	}
	return kinds
}

func (state *graphState) knownSubjects() []Subject {
	result := make([]Subject, 0)
	seen := make(map[string]struct{})
	add := func(subject Subject) {
		subject = canonicalSubject(subject)
		if subject.Name == "" {
			return
		}
		key := subjectKey(subject)
		if _, ok := seen[key]; ok {
			return
		}
		result = append(result, subject)
	}

	add(Subject{Kind: SubjectGroup, Name: BuiltinAdminGroup})
	for _, key := range state.order {
		object := state.objects[key]
		switch object.Kind {
		case KindServiceAccount:
			add(Subject{Kind: SubjectServiceAccount, Namespace: object.Namespace, Name: object.Name})
		case KindRoleBinding, KindClusterRoleBinding:
			for _, ref := range object.Subjects {
				add(Subject{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name})
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return subjectKey(result[i]) < subjectKey(result[j])
	})
	return result
}

func fingerprintPath(subject Subject, target Target, steps []Step) string {
	type fingerprint struct {
		Subject Subject `json:"subject"`
		Target  Target  `json:"target"`
		Steps   []Step  `json:"steps"`
	}
	payload := fingerprint{
		Subject: canonicalSubject(subject),
		Target:  target,
		Steps:   withOrdinals(steps),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		sum := sha1.Sum([]byte(subjectKey(subject) + targetGrant(target)))
		return "path-" + hex.EncodeToString(sum[:])[:16]
	}
	sum := sha1.Sum(data)
	return "path-" + hex.EncodeToString(sum[:])[:16]
}

func (state *graphState) rememberPaths(paths []Path) {
	state.pathMu.Lock()
	defer state.pathMu.Unlock()
	for _, path := range paths {
		state.paths[path.PathID] = clonePath(path)
	}
}

func (state *graphState) lookupPath(id string) ([]Step, bool) {
	state.pathMu.RLock()
	defer state.pathMu.RUnlock()
	path, ok := state.paths[id]
	if !ok {
		return nil, false
	}
	return cloneSteps(path.Steps), true
}

func (state *graphState) snapshotPaths() []Path {
	state.pathMu.RLock()
	paths := make([]Path, 0, len(state.paths))
	for _, path := range state.paths {
		paths = append(paths, clonePath(path))
	}
	state.pathMu.RUnlock()

	sort.SliceStable(paths, func(i, j int) bool {
		if paths[i].Hops != paths[j].Hops {
			return paths[i].Hops < paths[j].Hops
		}
		return paths[i].PathID < paths[j].PathID
	})
	return paths
}

func containsPathID(paths []Path, id string) bool {
	for _, path := range paths {
		if path.PathID == id {
			return true
		}
	}
	return false
}

func validateStoredPath(path Path) error {
	if strings.TrimSpace(path.PathID) == "" {
		return errors.New("empty path id")
	}
	if path.Hops != len(path.Steps) {
		return fmt.Errorf("path %q has hops=%d steps=%d", path.PathID, path.Hops, len(path.Steps))
	}
	if err := validateSubject(path.Subject); err != nil {
		return err
	}
	if err := validateTarget(path.Target); err != nil {
		return err
	}
	for index, step := range path.Steps {
		if step.Ordinal != index+1 {
			return fmt.Errorf("path %q has non-continuous ordinal at %d", path.PathID, index)
		}
	}
	return nil
}

func describeChange(before, after Object) string {
	parts := make([]string, 0, 5)
	if len(before.Rules) != len(after.Rules) {
		parts = append(parts, fmt.Sprintf("rules:%d->%d", len(before.Rules), len(after.Rules)))
	} else if !reflect.DeepEqual(before.Rules, after.Rules) {
		parts = append(parts, "rules:modified")
	}
	if before.RoleRef != after.RoleRef {
		parts = append(parts, fmt.Sprintf("roleRef:%s->%s", before.RoleRef, after.RoleRef))
	}
	if len(before.Subjects) != len(after.Subjects) {
		parts = append(parts, fmt.Sprintf("subjects:%d->%d", len(before.Subjects), len(after.Subjects)))
	} else if !reflect.DeepEqual(before.Subjects, after.Subjects) {
		parts = append(parts, "subjects:modified")
	}
	if len(before.Labels) != len(after.Labels) {
		parts = append(parts, fmt.Sprintf("labels:%d->%d", len(before.Labels), len(after.Labels)))
	} else if !reflect.DeepEqual(before.Labels, after.Labels) {
		parts = append(parts, "labels:modified")
	}
	if len(before.Aggregate) != len(after.Aggregate) {
		parts = append(parts, fmt.Sprintf("aggregate:%d->%d", len(before.Aggregate), len(after.Aggregate)))
	} else if !reflect.DeepEqual(before.Aggregate, after.Aggregate) {
		parts = append(parts, "aggregate:modified")
	}
	if len(parts) == 0 {
		return "object:modified"
	}
	return strings.Join(parts, "; ")
}
