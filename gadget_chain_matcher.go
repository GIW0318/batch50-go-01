// Package task matches gadget chains across indexed dependency artifacts and
// keeps every intermediate hop attributable to its artifact coordinate.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Call kinds recorded between methods.
const (
	CallVirtual = "virtual"
	CallStatic  = "static"
	CallDynamic = "dynamic"
)

// Chain status values.
const (
	ChainUsable  = "usable"
	ChainPending = "pending"
	ChainStale   = "stale"
)

// Reason codes attached to a verdict or a chain trace.
const (
	ReasonUsable          = "reason_usable"
	ReasonClassExcluded   = "reason_class_excluded"
	ReasonArtifactMissing = "reason_artifact_missing"
	ReasonDynamicPending  = "reason_dynamic_pending"
	ReasonInvalidated     = "reason_artifact_invalidated"
)

// Errors reported by the matcher.
var (
	ErrBlankArtifact   = errors.New("artifact group, name and version are required")
	ErrBadVersion      = errors.New("artifact version must have three numeric segments")
	ErrNodeLimit       = errors.New("artifact exceeds the indexed node limit")
	ErrUnknownArtifact = errors.New("unknown artifact")
	ErrUnknownChain    = errors.New("unknown chain")
	ErrBlankEntry      = errors.New("entry class and method are required")
	ErrNoPath          = errors.New("no chain reaches the sink")
	ErrArtifactMissing = errors.New("environment is missing a chain artifact")
	ErrVersionMismatch = errors.New("environment artifact version is incompatible")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// ArtifactID is the coordinate of one indexed artifact.
type ArtifactID struct {
	Group    string `json:"group"`
	Artifact string `json:"artifact"`
	Version  string `json:"version"`
}

// Key renders the canonical coordinate string.
func (a ArtifactID) Key() string {
	return ""
}

// ClassNode is one indexed class.
type ClassNode struct {
	Name    string       `json:"name"`
	Methods []MethodNode `json:"methods"`
}

// MethodNode is one indexed method and its outgoing calls.
type MethodNode struct {
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Calls []Call `json:"calls"`
}

// Call is one outgoing invocation recorded on a method.
type Call struct {
	TargetClass  string `json:"target_class"`
	TargetMethod string `json:"target_method"`
	Desc         string `json:"desc"`
	Kind         string `json:"kind"`
}

// Dependency is one artifact manifest with its indexed classes.
type Dependency struct {
	Group    string      `json:"group"`
	Artifact string      `json:"artifact"`
	Version  string      `json:"version"`
	Classes  []ClassNode `json:"classes"`
}

// EntryPoint is the deserialization entry to start from.
type EntryPoint struct {
	Class  string `json:"class"`
	Method string `json:"method"`
}

// Sink is the dangerous terminal method.
type Sink struct {
	Class  string `json:"class"`
	Method string `json:"method"`
}

// Hop is one method on a matched chain, attributed to its artifact.
type Hop struct {
	Artifact ArtifactID `json:"artifact"`
	Class    string     `json:"class"`
	Method   string     `json:"method"`
}

// Chain is one matched path from entry to sink.
type Chain struct {
	ID        string       `json:"id"`
	Artifacts []ArtifactID `json:"artifacts"`
	Hops      []Hop        `json:"hops"`
	Depth     int          `json:"depth"`
	Status    string       `json:"status"`
}

// ChainTrace is the explanation of one matched chain.
type ChainTrace struct {
	ChainID string `json:"chain_id"`
	Status  string `json:"status"`
	Hops    []Hop  `json:"hops"`
	Reason  string `json:"reason"`
}

// EnvSpec is the resolved environment a chain is checked against.
type EnvSpec struct {
	Artifacts       []ArtifactID `json:"artifacts"`
	AllowDynamic    bool         `json:"allow_dynamic"`
	ExcludedClasses []string     `json:"excluded_classes"`
}

// Verdict is the usability decision for one chain in one environment.
type Verdict struct {
	ChainID string   `json:"chain_id"`
	Usable  bool     `json:"usable"`
	Reason  string   `json:"reason"`
	Missing []string `json:"missing"`
	Pending []string `json:"pending"`
}

// Options bounds the index and the query.
type Options struct {
	MaxDepth            int
	MaxNodesPerArtifact int
	MaxChains           int
}

// GadgetChainMatcher owns the artifact index and the query results.
type GadgetChainMatcher struct {
	opts Options
}

// NewGadgetChainMatcher builds a matcher with the given caps.
func NewGadgetChainMatcher(opts Options) *GadgetChainMatcher {
	return &GadgetChainMatcher{opts: opts}
}

// Index adds or replaces one artifact in the method index.
func (m *GadgetChainMatcher) Index(dep Dependency) (ArtifactID, error) {
	return ArtifactID{}, errNotImplemented
}

// Query walks the index from one entry point to one sink.
func (m *GadgetChainMatcher) Query(entry EntryPoint, sink Sink) ([]Chain, error) {
	return nil, errNotImplemented
}

// Explain returns the hops and status of one matched chain.
func (m *GadgetChainMatcher) Explain(chainID string) (ChainTrace, error) {
	return ChainTrace{}, errNotImplemented
}

// Validate checks one chain against a resolved environment.
func (m *GadgetChainMatcher) Validate(chainID string, env EnvSpec) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Invalidate drops one artifact from the index.
func (m *GadgetChainMatcher) Invalidate(artifactID string) error {
	return errNotImplemented
}

// Artifacts lists the indexed coordinates, ordered by key.
func (m *GadgetChainMatcher) Artifacts() []ArtifactID {
	return nil
}

// Chain reads one matched chain.
func (m *GadgetChainMatcher) Chain(chainID string) (Chain, bool) {
	return Chain{}, false
}

// NodeCount reports the indexed method count of one artifact.
func (m *GadgetChainMatcher) NodeCount(artifactID string) int {
	return 0
}

// Snapshot serialises the index and the matched chains.
func (m *GadgetChainMatcher) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (m *GadgetChainMatcher) Restore(data []byte) error {
	return errNotImplemented
}
