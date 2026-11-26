// Package task plans privilege escalation paths across a cluster RBAC graph and
// keeps the reasoning traceable across reloads and restarts.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	limits Limits
}

// NewRbacEscalationPlanner builds a planner with the given limits.
func NewRbacEscalationPlanner(limits Limits) *RbacEscalationPlanner {
	return &RbacEscalationPlanner{limits: limits}
}

// Load validates and installs a whole batch of objects as one revision.
func (p *RbacEscalationPlanner) Load(objects []Object) error {
	return errNotImplemented
}

// Graph returns the active graph revision.
func (p *RbacEscalationPlanner) Graph() Graph {
	return Graph{}
}

// Holds reports whether the subject already has the target privilege.
func (p *RbacEscalationPlanner) Holds(subject Subject, target Target) bool {
	return false
}

// Paths returns every escalation path that reaches the target.
func (p *RbacEscalationPlanner) Paths(subject Subject, target Target) ([]Path, error) {
	return nil, errNotImplemented
}

// Explain returns the steps of one computed path in ordinal order.
func (p *RbacEscalationPlanner) Explain(pathID string) ([]Step, error) {
	return nil, errNotImplemented
}

// Diff compares two graph revisions and reports added, removed and changed objects.
func (p *RbacEscalationPlanner) Diff(prev, next Graph) ([]Delta, error) {
	return nil, errNotImplemented
}

// Privileges lists the privileges held by the subject as sorted "verb resource" pairs.
func (p *RbacEscalationPlanner) Privileges(subject Subject) []string {
	return nil
}

// Aggregated lists the roles pulled in by a cluster role label selector.
func (p *RbacEscalationPlanner) Aggregated(role string) []string {
	return nil
}

// Snapshot serialises the graph revision, aggregates and computed paths.
func (p *RbacEscalationPlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *RbacEscalationPlanner) Restore(data []byte) error {
	return errNotImplemented
}

// Reset drops the graph and every computed path.
func (p *RbacEscalationPlanner) Reset() {
}
