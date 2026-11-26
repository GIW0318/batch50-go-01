// Package task matches gadget chains across indexed dependency artifacts and
// keeps every intermediate hop attributable to its artifact coordinate.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

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

const (
	snapshotFormat  = "gsb/gadget-chain-matcher"
	snapshotVersion = 1
)

// ArtifactID is the coordinate of one indexed artifact.
type ArtifactID struct {
	Group    string `json:"group"`
	Artifact string `json:"artifact"`
	Version  string `json:"version"`
}

// Key renders the canonical coordinate string.
func (a ArtifactID) Key() string {
	return strings.Join([]string{a.Group, a.Artifact, a.Version}, ":")
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

// Options bounds the index and the query. Non-positive caps mean unlimited.
type Options struct {
	MaxDepth            int
	MaxNodesPerArtifact int
	MaxChains           int
}

type indexedArtifact struct {
	id         ArtifactID
	generation uint64
	classes    []ClassNode
	nodeCount  int
}

type indexedMethod struct {
	key         string
	artifact    ArtifactID
	artifactKey string
	generation  uint64
	class       string
	name        string
	desc        string
	calls       []Call
}

// matcherState is immutable after publication. Updates make a new state and
// swap it under stateMu, so readers always observe a complete index.
type matcherState struct {
	mutation   uint64
	artifacts  map[string]indexedArtifact
	methods    map[string][]indexedMethod
	chains     map[string]Chain
	watermarks map[string]uint64
}

// GadgetChainMatcher owns the artifact index and the query results.
type GadgetChainMatcher struct {
	opts Options

	stateMu sync.RWMutex
	state   *matcherState

	artifactLocksMu sync.Mutex
	artifactLocks   map[string]*sync.Mutex
}

// NewGadgetChainMatcher builds a matcher with the given caps.
func NewGadgetChainMatcher(opts Options) *GadgetChainMatcher {
	return &GadgetChainMatcher{
		opts:          opts,
		state:         buildState(0, nil, nil, nil),
		artifactLocks: make(map[string]*sync.Mutex),
	}
}

// Index adds or replaces one artifact in the method index.
func (m *GadgetChainMatcher) Index(dep Dependency) (ArtifactID, error) {
	if strings.TrimSpace(dep.Group) == "" || strings.TrimSpace(dep.Artifact) == "" || strings.TrimSpace(dep.Version) == "" {
		return ArtifactID{}, ErrBlankArtifact
	}
	if !validVersion(dep.Version) {
		return ArtifactID{}, ErrBadVersion
	}

	nodeCount := countMethods(dep.Classes)
	if cap := m.opts.MaxNodesPerArtifact; cap > 0 && nodeCount > cap {
		return ArtifactID{}, ErrNodeLimit
	}

	id := ArtifactID{Group: dep.Group, Artifact: dep.Artifact, Version: dep.Version}
	record := indexedArtifact{
		id:        id,
		classes:   cloneClasses(dep.Classes),
		nodeCount: nodeCount,
	}

	unlock := m.lockArtifact(id.Key())
	defer unlock()

	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	old := m.state
	generation := uint64(1)
	if previous, ok := old.artifacts[id.Key()]; ok {
		generation = previous.generation + 1
	}
	if watermark := old.watermarks[id.Key()]; watermark >= generation {
		generation = watermark + 1
	}
	record.generation = generation

	artifacts := cloneArtifacts(old.artifacts)
	artifacts[id.Key()] = record

	watermarks := cloneWatermarks(old.watermarks)
	watermarks[id.Key()] = generation

	chains := staleChains(old.chains)
	m.state = buildState(old.mutation+1, artifacts, chains, watermarks)
	return id, nil
}

// Query walks the index from one entry point to one sink.
func (m *GadgetChainMatcher) Query(entry EntryPoint, sink Sink) ([]Chain, error) {
	if strings.TrimSpace(entry.Class) == "" || strings.TrimSpace(entry.Method) == "" {
		return nil, ErrBlankEntry
	}

	state := m.currentState()
	starts := state.methods[methodKey(entry.Class, entry.Method)]
	if len(starts) == 0 {
		return nil, ErrNoPath
	}

	maxDepth := m.opts.MaxDepth
	if maxDepth < 0 {
		maxDepth = 0
	}

	type candidate struct {
		chain   Chain
		sortKey string
	}

	candidates := make([]candidate, 0)
	seenIDs := make(map[string]struct{})

	for _, start := range starts {
		hops := []Hop{hopFor(start)}
		generations := []uint64{start.generation}
		edges := make([]string, 0, maxDepth)
		visited := map[string]bool{start.key: true}

		var walk func(indexedMethod, int, bool)
		walk = func(node indexedMethod, depth int, dynamic bool) {
			if node.class == sink.Class && node.name == sink.Method {
				chain, order := makeChain(hops, generations, edges, dynamic)
				if _, duplicate := seenIDs[chain.ID]; !duplicate {
					seenIDs[chain.ID] = struct{}{}
					candidates = append(candidates, candidate{chain: chain, sortKey: order})
				}
				return
			}
			if maxDepth > 0 && depth >= maxDepth {
				return
			}

			for _, call := range node.calls {
				nextMethods := state.resolve(node.artifactKey, call)
				if len(nextMethods) == 0 {
					continue
				}
				nextDynamic := dynamic || call.Kind == CallDynamic
				for _, next := range nextMethods {
					if visited[next.key] {
						continue
					}

					visited[next.key] = true
					hops = append(hops, hopFor(next))
					generations = append(generations, next.generation)
					edges = append(edges, call.Kind)
					walk(next, depth+1, nextDynamic)

					edges = edges[:len(edges)-1]
					generations = generations[:len(generations)-1]
					hops = hops[:len(hops)-1]
					delete(visited, next.key)
				}
			}
		}

		walk(start, 0, false)
	}

	if len(candidates) == 0 {
		return nil, ErrNoPath
	}

	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.chain.Depth != right.chain.Depth {
			return left.chain.Depth < right.chain.Depth
		}
		if left.sortKey != right.sortKey {
			return left.sortKey < right.sortKey
		}
		return left.chain.ID < right.chain.ID
	})

	if cap := m.opts.MaxChains; cap > 0 && len(candidates) > cap {
		candidates = candidates[:cap]
	}

	result := make([]Chain, len(candidates))
	for i := range candidates {
		result[i] = cloneChain(candidates[i].chain)
	}
	m.rememberChains(state.mutation, result)
	return result, nil
}

// Explain returns the hops and status of one matched chain.
func (m *GadgetChainMatcher) Explain(chainID string) (ChainTrace, error) {
	state := m.currentState()
	chain, ok := state.chains[chainID]
	if !ok {
		return ChainTrace{}, ErrUnknownChain
	}
	return ChainTrace{
		ChainID: chain.ID,
		Status:  chain.Status,
		Hops:    append([]Hop(nil), chain.Hops...),
		Reason:  reasonForChain(chain),
	}, nil
}

// Validate checks one chain against a resolved environment.
func (m *GadgetChainMatcher) Validate(chainID string, env EnvSpec) (Verdict, error) {
	state := m.currentState()
	chain, ok := state.chains[chainID]
	if !ok {
		return Verdict{}, ErrUnknownChain
	}

	verdict := Verdict{ChainID: chainID}
	if chain.Status == ChainStale {
		verdict.Reason = ReasonInvalidated
		return verdict, nil
	}

	type coordinate struct {
		group    string
		artifact string
	}

	available := make(map[coordinate][]string)
	for _, artifact := range env.Artifacts {
		key := coordinate{group: artifact.Group, artifact: artifact.Artifact}
		available[key] = append(available[key], artifact.Version)
	}

	missing := make([]string, 0)
	versionMismatch := false
	for _, artifact := range chain.Artifacts {
		versions, found := available[coordinate{group: artifact.Group, artifact: artifact.Artifact}]
		if !found || len(versions) == 0 {
			missing = append(missing, artifact.Key())
			continue
		}

		wantMajor := majorVersion(artifact.Version)
		compatible := false
		for _, version := range versions {
			if majorVersion(version) == wantMajor {
				compatible = true
				break
			}
		}
		if !compatible {
			versionMismatch = true
		}
	}
	if len(missing) > 0 {
		verdict.Reason = ReasonArtifactMissing
		verdict.Missing = missing
		return verdict, ErrArtifactMissing
	}
	if versionMismatch {
		return verdict, ErrVersionMismatch
	}

	excluded := make(map[string]struct{}, len(env.ExcludedClasses))
	for _, class := range env.ExcludedClasses {
		excluded[class] = struct{}{}
	}
	for _, hop := range chain.Hops {
		if _, blocked := excluded[hop.Class]; blocked {
			verdict.Reason = ReasonClassExcluded
			return verdict, nil
		}
	}

	if chain.Status == ChainPending && !env.AllowDynamic {
		verdict.Reason = ReasonDynamicPending
		verdict.Pending = pendingHops(chain)
		return verdict, nil
	}

	verdict.Usable = true
	verdict.Reason = ReasonUsable
	return verdict, nil
}

// Invalidate drops one artifact from the index.
func (m *GadgetChainMatcher) Invalidate(artifactID string) error {
	unlock := m.lockArtifact(artifactID)
	defer unlock()

	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	old := m.state
	artifact, ok := old.artifacts[artifactID]
	if !ok {
		return ErrUnknownArtifact
	}

	artifacts := cloneArtifacts(old.artifacts)
	delete(artifacts, artifactID)

	watermarks := cloneWatermarks(old.watermarks)
	watermarks[artifactID] = artifact.generation

	chains := staleChains(old.chains)
	m.state = buildState(old.mutation+1, artifacts, chains, watermarks)
	return nil
}

// Artifacts lists the indexed coordinates, ordered by key.
func (m *GadgetChainMatcher) Artifacts() []ArtifactID {
	state := m.currentState()
	keys := sortedArtifactKeys(state.artifacts)
	result := make([]ArtifactID, 0, len(keys))
	for _, key := range keys {
		result = append(result, state.artifacts[key].id)
	}
	return result
}

// Chain reads one matched chain.
func (m *GadgetChainMatcher) Chain(chainID string) (Chain, bool) {
	state := m.currentState()
	chain, ok := state.chains[chainID]
	if !ok {
		return Chain{}, false
	}
	return cloneChain(chain), true
}

// NodeCount reports the indexed method count of one artifact.
func (m *GadgetChainMatcher) NodeCount(artifactID string) int {
	state := m.currentState()
	artifact, ok := state.artifacts[artifactID]
	if !ok {
		return 0
	}
	return artifact.nodeCount
}

type snapshotArtifact struct {
	ID         ArtifactID  `json:"id"`
	Generation uint64      `json:"generation"`
	Classes    []ClassNode `json:"classes"`
}

type snapshotWatermark struct {
	Key        string `json:"key"`
	Generation uint64 `json:"generation"`
}

type snapshotEnvelope struct {
	Format     string              `json:"format"`
	Version    int                 `json:"version"`
	Artifacts  []snapshotArtifact  `json:"artifacts"`
	Chains     []Chain             `json:"chains"`
	Watermarks []snapshotWatermark `json:"watermarks"`
}

// Snapshot serialises the index and the matched chains.
func (m *GadgetChainMatcher) Snapshot() ([]byte, error) {
	state := m.currentState()

	artifactKeys := sortedArtifactKeys(state.artifacts)
	artifacts := make([]snapshotArtifact, 0, len(artifactKeys))
	for _, key := range artifactKeys {
		artifact := state.artifacts[key]
		artifacts = append(artifacts, snapshotArtifact{
			ID:         artifact.id,
			Generation: artifact.generation,
			Classes:    artifact.classes,
		})
	}

	chainIDs := make([]string, 0, len(state.chains))
	for id := range state.chains {
		chainIDs = append(chainIDs, id)
	}
	sort.Strings(chainIDs)
	chains := make([]Chain, 0, len(chainIDs))
	for _, id := range chainIDs {
		chains = append(chains, cloneChain(state.chains[id]))
	}

	watermarkKeys := make([]string, 0, len(state.watermarks))
	for key := range state.watermarks {
		watermarkKeys = append(watermarkKeys, key)
	}
	sort.Strings(watermarkKeys)
	watermarks := make([]snapshotWatermark, 0, len(watermarkKeys))
	for _, key := range watermarkKeys {
		watermarks = append(watermarks, snapshotWatermark{Key: key, Generation: state.watermarks[key]})
	}

	return json.Marshal(snapshotEnvelope{
		Format:     snapshotFormat,
		Version:    snapshotVersion,
		Artifacts:  artifacts,
		Chains:     chains,
		Watermarks: watermarks,
	})
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (m *GadgetChainMatcher) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Format != snapshotFormat || envelope.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	artifacts := make(map[string]indexedArtifact, len(envelope.Artifacts))
	for _, snapshot := range envelope.Artifacts {
		if strings.TrimSpace(snapshot.ID.Group) == "" || strings.TrimSpace(snapshot.ID.Artifact) == "" || strings.TrimSpace(snapshot.ID.Version) == "" {
			return ErrBadSnapshot
		}
		if !validVersion(snapshot.ID.Version) || snapshot.Generation == 0 {
			return ErrBadSnapshot
		}
		key := snapshot.ID.Key()
		if _, duplicate := artifacts[key]; duplicate {
			return ErrBadSnapshot
		}
		count := countMethods(snapshot.Classes)
		if cap := m.opts.MaxNodesPerArtifact; cap > 0 && count > cap {
			return ErrBadSnapshot
		}
		artifacts[key] = indexedArtifact{
			id:         snapshot.ID,
			generation: snapshot.Generation,
			classes:    cloneClasses(snapshot.Classes),
			nodeCount:  count,
		}
	}

	chains := make(map[string]Chain, len(envelope.Chains))
	for _, chain := range envelope.Chains {
		if chain.ID == "" || len(chain.Hops) == 0 || chain.Depth != len(chain.Hops)-1 {
			return ErrBadSnapshot
		}
		if _, duplicate := chains[chain.ID]; duplicate {
			return ErrBadSnapshot
		}
		chains[chain.ID] = cloneChain(chain)
	}

	watermarks := make(map[string]uint64, len(envelope.Watermarks))
	for _, watermark := range envelope.Watermarks {
		if watermark.Key == "" || watermark.Generation == 0 {
			return ErrBadSnapshot
		}
		if _, duplicate := watermarks[watermark.Key]; duplicate {
			return ErrBadSnapshot
		}
		watermarks[watermark.Key] = watermark.Generation
	}

	next := buildState(0, artifacts, chains, watermarks)

	m.stateMu.Lock()
	next.mutation = m.state.mutation + 1
	m.state = next
	m.stateMu.Unlock()
	return nil
}

func (m *GadgetChainMatcher) currentState() *matcherState {
	m.stateMu.RLock()
	state := m.state
	m.stateMu.RUnlock()
	return state
}

func (m *GadgetChainMatcher) lockArtifact(key string) func() {
	m.artifactLocksMu.Lock()
	lock := m.artifactLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.artifactLocks[key] = lock
	}
	m.artifactLocksMu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func (m *GadgetChainMatcher) rememberChains(capturedMutation uint64, chains []Chain) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	current := m.state
	nextChains := cloneChains(current.chains)
	if current.mutation != capturedMutation {
		for _, chain := range chains {
			if _, exists := nextChains[chain.ID]; exists {
				continue
			}
			stale := cloneChain(chain)
			stale.Status = ChainStale
			nextChains[stale.ID] = stale
		}
	} else {
		for _, chain := range chains {
			nextChains[chain.ID] = cloneChain(chain)
		}
	}

	next := *current
	next.chains = nextChains
	m.state = &next
}

func (s *matcherState) resolve(callerArtifact string, call Call) []indexedMethod {
	if call.TargetClass == "" || call.TargetMethod == "" {
		return nil
	}

	all := s.methods[methodKey(call.TargetClass, call.TargetMethod)]
	if len(all) == 0 {
		return nil
	}

	matches := make([]indexedMethod, 0, len(all))
	for _, method := range all {
		if call.Desc == "" || call.Desc == method.desc {
			matches = append(matches, method)
		}
	}
	if len(matches) == 0 {
		return nil
	}

	sameArtifact := make([]indexedMethod, 0, len(matches))
	foreignArtifacts := make(map[string]struct{})
	for _, method := range matches {
		if method.artifactKey == callerArtifact {
			sameArtifact = append(sameArtifact, method)
		} else {
			foreignArtifacts[method.artifactKey] = struct{}{}
		}
	}
	if len(sameArtifact) > 0 {
		return sameArtifact
	}

	// A call without a declaring artifact can only be resolved unambiguously
	// when exactly one foreign artifact provides the target. This prevents a
	// duplicate class in several artifacts from fabricating a cross-artifact
	// path that no single runtime classpath could select.
	if len(foreignArtifacts) == 1 {
		return matches
	}
	return nil
}

func buildState(mutation uint64, artifacts map[string]indexedArtifact, chains map[string]Chain, watermarks map[string]uint64) *matcherState {
	if artifacts == nil {
		artifacts = make(map[string]indexedArtifact)
	}
	if chains == nil {
		chains = make(map[string]Chain)
	}
	if watermarks == nil {
		watermarks = make(map[string]uint64)
	}

	methods := make(map[string][]indexedMethod)
	for _, artifactKey := range sortedArtifactKeys(artifacts) {
		artifact := artifacts[artifactKey]
		for _, class := range artifact.classes {
			for _, method := range class.Methods {
				entry := indexedMethod{
					key:         nodeKey(artifactKey, class.Name, method.Name, method.Desc),
					artifact:    artifact.id,
					artifactKey: artifactKey,
					generation:  artifact.generation,
					class:       class.Name,
					name:        method.Name,
					desc:        method.Desc,
					calls:       method.Calls,
				}
				key := methodKey(class.Name, method.Name)
				methods[key] = append(methods[key], entry)
			}
		}
	}
	for key := range methods {
		sort.Slice(methods[key], func(i, j int) bool {
			return methods[key][i].key < methods[key][j].key
		})
	}

	return &matcherState{
		mutation:   mutation,
		artifacts:  artifacts,
		methods:    methods,
		chains:     chains,
		watermarks: watermarks,
	}
}

type chainHopIdentity struct {
	Artifact   string `json:"artifact"`
	Generation uint64 `json:"generation"`
	Class      string `json:"class"`
	Method     string `json:"method"`
}

type chainIdentity struct {
	Hops  []chainHopIdentity `json:"hops"`
	Edges []string           `json:"edges"`
}

func makeChain(hops []Hop, generations []uint64, edges []string, dynamic bool) (Chain, string) {
	chainHops := append([]Hop(nil), hops...)
	identity := chainIdentity{
		Hops:  make([]chainHopIdentity, 0, len(chainHops)),
		Edges: append([]string(nil), edges...),
	}
	for i, hop := range chainHops {
		identity.Hops = append(identity.Hops, chainHopIdentity{
			Artifact:   hop.Artifact.Key(),
			Generation: generations[i],
			Class:      hop.Class,
			Method:     hop.Method,
		})
	}
	encoded, _ := json.Marshal(identity)
	sum := sha256.Sum256(encoded)

	seenArtifacts := make(map[string]bool)
	chainArtifacts := make([]ArtifactID, 0, len(chainHops))
	for _, hop := range chainHops {
		key := hop.Artifact.Key()
		if seenArtifacts[key] {
			continue
		}
		seenArtifacts[key] = true
		chainArtifacts = append(chainArtifacts, hop.Artifact)
	}

	status := ChainUsable
	if dynamic {
		status = ChainPending
	}
	return Chain{
		ID:        "chain-" + hex.EncodeToString(sum[:]),
		Artifacts: chainArtifacts,
		Hops:      chainHops,
		Depth:     len(chainHops) - 1,
		Status:    status,
	}, chainSortKey(chainHops)
}

func chainSortKey(hops []Hop) string {
	var builder strings.Builder
	for _, hop := range hops {
		builder.WriteString(hop.Class)
		builder.WriteByte(0)
		builder.WriteString(hop.Method)
		builder.WriteByte(0)
		builder.WriteString(hop.Artifact.Key())
		builder.WriteByte(0)
	}
	return builder.String()
}

func hopFor(method indexedMethod) Hop {
	return Hop{Artifact: method.artifact, Class: method.class, Method: method.name}
}

func reasonForChain(chain Chain) string {
	if chain.Status == ChainStale {
		return ReasonInvalidated
	}
	if chain.Status == ChainPending {
		return ReasonDynamicPending
	}
	return ReasonUsable
}

func pendingHops(chain Chain) []string {
	pending := make([]string, 0, len(chain.Hops))
	seen := make(map[string]bool)
	for _, hop := range chain.Hops {
		label := hop.Class + "." + hop.Method
		if seen[label] {
			continue
		}
		seen[label] = true
		pending = append(pending, label)
	}
	return pending
}

func methodKey(class, method string) string {
	return class + "\x00" + method
}

func nodeKey(artifactKey, class, method, desc string) string {
	return artifactKey + "\x00" + class + "\x00" + method + "\x00" + desc
}

func validVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func majorVersion(version string) string {
	major := version
	if index := strings.IndexByte(major, '.'); index >= 0 {
		major = major[:index]
	}
	major = strings.TrimLeft(major, "0")
	if major == "" {
		return "0"
	}
	return major
}

func countMethods(classes []ClassNode) int {
	count := 0
	for _, class := range classes {
		count += len(class.Methods)
	}
	return count
}

func cloneClasses(classes []ClassNode) []ClassNode {
	if classes == nil {
		return nil
	}
	result := make([]ClassNode, len(classes))
	for i, class := range classes {
		result[i] = class
		if class.Methods != nil {
			result[i].Methods = make([]MethodNode, len(class.Methods))
			for j, method := range class.Methods {
				result[i].Methods[j] = method
				if method.Calls != nil {
					result[i].Methods[j].Calls = append([]Call(nil), method.Calls...)
				}
			}
		}
	}
	return result
}

func cloneArtifacts(artifacts map[string]indexedArtifact) map[string]indexedArtifact {
	result := make(map[string]indexedArtifact, len(artifacts))
	for key, artifact := range artifacts {
		result[key] = artifact
	}
	return result
}

func cloneChains(chains map[string]Chain) map[string]Chain {
	result := make(map[string]Chain, len(chains))
	for id, chain := range chains {
		result[id] = cloneChain(chain)
	}
	return result
}

func staleChains(chains map[string]Chain) map[string]Chain {
	result := make(map[string]Chain, len(chains))
	for id, chain := range chains {
		chain.Status = ChainStale
		result[id] = chain
	}
	return result
}

func cloneWatermarks(watermarks map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(watermarks))
	for key, generation := range watermarks {
		result[key] = generation
	}
	return result
}

func cloneChain(chain Chain) Chain {
	chain.Artifacts = append([]ArtifactID(nil), chain.Artifacts...)
	chain.Hops = append([]Hop(nil), chain.Hops...)
	return chain
}

func sortedArtifactKeys(artifacts map[string]indexedArtifact) []string {
	keys := make([]string, 0, len(artifacts))
	for key := range artifacts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
