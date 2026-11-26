// Package task runs branching story dialogue graphs: node loading, session
// state, option conditions, one shot nodes inside loops and graph version
// pinning for sessions that are already running.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Node kinds accepted by LoadGraph.
const (
	NodeLine     = "line"
	NodeChoice   = "choice"
	NodeFallback = "fallback"
	NodeEnd      = "end"
)

// Comparison operators accepted by Condition.
const (
	CondEq = "eq"
	CondGe = "ge"
	CondLt = "lt"
)

const (
	snapshotFormat        = "dialogue-graph-runner"
	snapshotFormatVersion = 1
)

// Errors reported by the runner.
var (
	ErrBadGraph        = errors.New("graph is malformed")
	ErrDuplicateNode   = errors.New("node id is duplicated")
	ErrMissingNode     = errors.New("node reference points nowhere")
	ErrGraphNotFound   = errors.New("graph is not loaded")
	ErrGraphTooLarge   = errors.New("graph exceeds the node budget")
	ErrSessionLimit    = errors.New("too many live sessions")
	ErrSessionNotFound = errors.New("session is not known")
	ErrSessionClosed   = errors.New("session already ended")
	ErrOptionLocked    = errors.New("option is not selectable")
	ErrChoicePending   = errors.New("node waits for a choice")
	ErrNoOption        = errors.New("no option and no fallback is available")
	ErrCycleLimit      = errors.New("edge visit budget is exhausted")
	ErrVariableLimit   = errors.New("session variable budget is exhausted")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Limits bounds the nodes of one graph, the live sessions, the visits of one
// edge and the variables of one session.
type Limits struct {
	MaxNodesPerGraph int
	MaxSessions      int
	MaxEdgeVisits    int
	MaxVariables     int
}

// Condition gates one option on a session variable or on a quest flag.
type Condition struct {
	Key   string
	Op    string
	Value int
	Quest string
}

// Option is one selectable answer of a choice node.
type Option struct {
	ID       string
	Text     string
	Next     string
	Once     bool
	Requires []Condition
	Effects  map[string]int
}

// Node is one node of a dialogue graph.
type Node struct {
	ID       string
	Text     string
	Kind     string
	Next     string
	Options  []Option
	Once     bool
	Effects  map[string]int
	Fallback string
	Ending   string
}

// DialogueState is the public view of one running session.
type DialogueState struct {
	SessionID    string
	GraphID      string
	GraphVersion int
	CharacterID  string
	NodeID       string
	Text         string
	Variables    map[string]int
	Visited      []string
	Finished     bool
	Ended        bool
}

// DialogueGraphRunner owns the loaded graphs, their versions and every live
// session.
type DialogueGraphRunner struct {
	mu             sync.RWMutex
	limits         Limits
	nextSessionSeq uint64
	graphs         map[string]*graphLineage
	sessions       map[string]*sessionState
}

type graphLineage struct {
	current  int
	versions map[int]*graphVersion
}

type graphVersion struct {
	version int
	nodes   []Node
	byID    map[string]Node
}

type sessionState struct {
	ID           string          `json:"id"`
	GraphID      string          `json:"graph_id"`
	GraphVersion int             `json:"graph_version"`
	CharacterID  string          `json:"character_id"`
	NodeID       string          `json:"node_id"`
	Variables    map[string]int  `json:"variables"`
	Quests       map[string]bool `json:"quests"`
	Visited      []string        `json:"visited"`
	Finished     bool            `json:"finished"`
	Ended        bool            `json:"ended"`
	EndReason    string          `json:"end_reason,omitempty"`
	EdgeVisits   map[string]int  `json:"edge_visits"`
	UsedOptions  map[string]bool `json:"used_options"`
	NodeRewards  map[string]bool `json:"node_rewards"`
}

type snapshotEnvelope struct {
	Format         string                   `json:"format"`
	FormatVersion  int                      `json:"format_version"`
	NextSessionSeq uint64                   `json:"next_session_seq"`
	Graphs         map[string]snapshotGraph `json:"graphs"`
	Sessions       map[string]*sessionState `json:"sessions"`
}

type snapshotGraph struct {
	Current  int                          `json:"current"`
	Versions map[int]snapshotGraphVersion `json:"versions"`
}

type snapshotGraphVersion struct {
	Version int    `json:"version"`
	Nodes   []Node `json:"nodes"`
}

type restoredState struct {
	graphs         map[string]*graphLineage
	sessions       map[string]*sessionState
	nextSessionSeq uint64
}

// NewDialogueGraphRunner builds a runner with the given limits.
func NewDialogueGraphRunner(limits Limits) *DialogueGraphRunner {
	return &DialogueGraphRunner{
		limits:   limits,
		graphs:   make(map[string]*graphLineage),
		sessions: make(map[string]*sessionState),
	}
}

// LoadGraph validates one graph, stores it and returns its new version.
func (r *DialogueGraphRunner) LoadGraph(graphID string, nodes []Node) (int, error) {
	version, graph, err := compileGraph(graphID, 0, nodes, r.limits)
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	lineage := r.graphs[graphID]
	if lineage == nil {
		lineage = &graphLineage{versions: make(map[int]*graphVersion)}
		r.graphs[graphID] = lineage
	}
	if lineage.versions == nil {
		lineage.versions = make(map[int]*graphVersion)
	}

	version = lineage.current + 1
	graph.version = version
	lineage.versions[version] = graph
	lineage.current = version
	return version, nil
}

// GraphVersion reports the version of one loaded graph.
func (r *DialogueGraphRunner) GraphVersion(graphID string) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	lineage, ok := r.graphs[graphID]
	if !ok {
		return 0, ErrGraphNotFound
	}
	return lineage.current, nil
}

// Start opens one session pinned to the current graph version.
func (r *DialogueGraphRunner) Start(graphID, characterID string) (DialogueState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	graph, err := r.currentGraphLocked(graphID)
	if err != nil {
		return DialogueState{}, err
	}
	if capacityReached(r.liveSessionCountLocked(), r.limits.MaxSessions) {
		return DialogueState{}, ErrSessionLimit
	}

	r.nextSessionSeq++
	sessionID := fmt.Sprintf("session-%d", r.nextSessionSeq)
	session := newSession(sessionID, graphID, graph.version, characterID)
	firstNode := graph.nodes[0]
	if err := applyNodeEntryEffects(session, firstNode, r.limits.MaxVariables); err != nil {
		return DialogueState{}, err
	}

	session.NodeID = firstNode.ID
	session.Visited = []string{firstNode.ID}
	if firstNode.Kind == NodeEnd {
		session.Finished = true
	}

	r.sessions[sessionID] = session
	return session.publicState(graph), nil
}

// Choose takes one option of the current node.
func (r *DialogueGraphRunner) Choose(sessionID, optionID string) (DialogueState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, graph, err := r.sessionGraphLocked(sessionID)
	if err != nil {
		return DialogueState{}, err
	}
	if session.Ended || session.Finished {
		return DialogueState{}, ErrSessionClosed
	}

	node, ok := graph.byID[session.NodeID]
	if !ok {
		return DialogueState{}, ErrMissingNode
	}
	option, optionKey, err := selectableOption(session, node, optionID)
	if err != nil {
		return DialogueState{}, err
	}
	target, ok := graph.byID[option.Next]
	if !ok {
		return DialogueState{}, ErrMissingNode
	}

	nextVariables := maps.Clone(session.Variables)
	if err := applyEffects(nextVariables, option.Effects, r.limits.MaxVariables); err != nil {
		return DialogueState{}, err
	}
	entryEffects := nodeEntryEffects(session, target)
	if err := applyEffects(nextVariables, entryEffects, r.limits.MaxVariables); err != nil {
		return DialogueState{}, err
	}
	edgeKey, edgeCount, err := reserveEdgeVisit(session, node.ID, target.ID, r.limits.MaxEdgeVisits)
	if err != nil {
		return DialogueState{}, err
	}

	session.Variables = nextVariables
	if option.Once {
		session.UsedOptions[optionKey] = true
	}
	if target.Once && !session.NodeRewards[target.ID] {
		session.NodeRewards[target.ID] = true
	}
	session.EdgeVisits[edgeKey] = edgeCount
	session.NodeID = target.ID
	session.Visited = append(session.Visited, target.ID)
	if target.Kind == NodeEnd {
		session.Finished = true
	}

	return session.publicState(graph), nil
}

// Advance follows the linear next node or the fallback of the current node.
func (r *DialogueGraphRunner) Advance(sessionID string) (DialogueState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, graph, err := r.sessionGraphLocked(sessionID)
	if err != nil {
		return DialogueState{}, err
	}
	if session.Ended || session.Finished {
		return DialogueState{}, ErrSessionClosed
	}

	node, ok := graph.byID[session.NodeID]
	if !ok {
		return DialogueState{}, ErrMissingNode
	}
	destination, err := advanceDestination(session, node)
	if err != nil {
		return DialogueState{}, err
	}
	target, ok := graph.byID[destination]
	if !ok {
		return DialogueState{}, ErrMissingNode
	}

	nextVariables := maps.Clone(session.Variables)
	entryEffects := nodeEntryEffects(session, target)
	if err := applyEffects(nextVariables, entryEffects, r.limits.MaxVariables); err != nil {
		return DialogueState{}, err
	}
	edgeKey, edgeCount, err := reserveEdgeVisit(session, node.ID, target.ID, r.limits.MaxEdgeVisits)
	if err != nil {
		return DialogueState{}, err
	}

	session.Variables = nextVariables
	if target.Once && !session.NodeRewards[target.ID] {
		session.NodeRewards[target.ID] = true
	}
	session.EdgeVisits[edgeKey] = edgeCount
	session.NodeID = target.ID
	session.Visited = append(session.Visited, target.ID)
	if target.Kind == NodeEnd {
		session.Finished = true
	}

	return session.publicState(graph), nil
}

// VisibleOptions lists the options that the session can select right now.
func (r *DialogueGraphRunner) VisibleOptions(sessionID string) ([]Option, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	session, graph, err := r.sessionGraphLocked(sessionID)
	if err != nil {
		return nil, err
	}
	if session.Ended {
		return nil, ErrSessionClosed
	}
	node, ok := graph.byID[session.NodeID]
	if !ok {
		return nil, ErrMissingNode
	}

	options := make([]Option, 0, len(node.Options))
	for i := range node.Options {
		option := node.Options[i]
		optionKey := usedOptionKey(node.ID, option.ID)
		if optionAvailable(session, option, optionKey) {
			options = append(options, cloneOption(option))
		}
	}
	return options, nil
}

// State reports the public state of one session.
func (r *DialogueGraphRunner) State(sessionID string) (DialogueState, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	session, graph, err := r.sessionGraphLocked(sessionID)
	if err != nil {
		return DialogueState{}, err
	}
	return session.publicState(graph), nil
}

// End closes one session with a reason.
func (r *DialogueGraphRunner) End(sessionID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	if session.Ended {
		return nil
	}
	session.Ended = true
	session.EndReason = reason
	return nil
}

// SetVariable writes one session variable.
func (r *DialogueGraphRunner) SetVariable(sessionID, key string, value int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	if session.Ended {
		return ErrSessionClosed
	}
	if _, exists := session.Variables[key]; !exists {
		if limitExceeded(len(session.Variables)+1, r.limits.MaxVariables) {
			return ErrVariableLimit
		}
	}
	session.Variables[key] = value
	return nil
}

// MarkQuest records one quest flag on the session.
func (r *DialogueGraphRunner) MarkQuest(sessionID, quest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	if session.Ended {
		return ErrSessionClosed
	}
	if session.Quests == nil {
		session.Quests = make(map[string]bool)
	}
	session.Quests[quest] = true
	return nil
}

// SessionCount reports the number of live sessions.
func (r *DialogueGraphRunner) SessionCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.liveSessionCountLocked()
}

// Snapshot serialises graphs, versions and every session.
func (r *DialogueGraphRunner) Snapshot() ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	graphs := make(map[string]snapshotGraph, len(r.graphs))
	for graphID, lineage := range r.graphs {
		versions := make(map[int]snapshotGraphVersion, len(lineage.versions))
		for version, graph := range lineage.versions {
			versions[version] = snapshotGraphVersion{
				Version: version,
				Nodes:   cloneNodes(graph.nodes),
			}
		}
		graphs[graphID] = snapshotGraph{Current: lineage.current, Versions: versions}
	}

	payload := snapshotEnvelope{
		Format:         snapshotFormat,
		FormatVersion:  snapshotFormatVersion,
		NextSessionSeq: r.nextSessionSeq,
		Graphs:         graphs,
		Sessions:       r.sessions,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *DialogueGraphRunner) Restore(data []byte) error {
	restored, err := decodeSnapshot(data, r.limits)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.graphs = restored.graphs
	r.sessions = restored.sessions
	r.nextSessionSeq = restored.nextSessionSeq
	return nil
}

func compileGraph(graphID string, version int, input []Node, limits Limits) (int, *graphVersion, error) {
	if graphID == "" || len(input) == 0 {
		return 0, nil, ErrBadGraph
	}
	if limitExceeded(len(input), limits.MaxNodesPerGraph) {
		return 0, nil, ErrGraphTooLarge
	}

	nodes := cloneNodes(input)
	byID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		if _, exists := byID[node.ID]; exists {
			return 0, nil, ErrDuplicateNode
		}
		byID[node.ID] = node
	}

	for _, node := range nodes {
		if err := validateNodeReference(node.Next, byID); err != nil {
			return 0, nil, err
		}
		if err := validateNodeReference(node.Fallback, byID); err != nil {
			return 0, nil, err
		}
		for _, option := range node.Options {
			if err := validateNodeReference(option.Next, byID); err != nil {
				return 0, nil, err
			}
		}
	}

	if version == 0 {
		version = 1
	}
	return version, &graphVersion{version: version, nodes: nodes, byID: byID}, nil
}

func validateNodeReference(reference string, byID map[string]Node) error {
	if reference == "" {
		return nil
	}
	if _, ok := byID[reference]; !ok {
		return ErrMissingNode
	}
	return nil
}

func cloneNodes(input []Node) []Node {
	nodes := make([]Node, len(input))
	for i, node := range input {
		node.Effects = maps.Clone(node.Effects)
		if node.Options != nil {
			options := make([]Option, len(node.Options))
			for j, option := range node.Options {
				option.Requires = slices.Clone(option.Requires)
				option.Effects = maps.Clone(option.Effects)
				options[j] = option
			}
			node.Options = options
		}
		nodes[i] = node
	}
	return nodes
}

func cloneOption(option Option) Option {
	option.Requires = slices.Clone(option.Requires)
	option.Effects = maps.Clone(option.Effects)
	return option
}

func newSession(id, graphID string, graphVersion int, characterID string) *sessionState {
	return &sessionState{
		ID:           id,
		GraphID:      graphID,
		GraphVersion: graphVersion,
		CharacterID:  characterID,
		Variables:    make(map[string]int),
		Quests:       make(map[string]bool),
		Visited:      make([]string, 0, 1),
		EdgeVisits:   make(map[string]int),
		UsedOptions:  make(map[string]bool),
		NodeRewards:  make(map[string]bool),
	}
}

func (r *DialogueGraphRunner) currentGraphLocked(graphID string) (*graphVersion, error) {
	lineage, ok := r.graphs[graphID]
	if !ok || lineage == nil {
		return nil, ErrGraphNotFound
	}
	graph, ok := lineage.versions[lineage.current]
	if !ok || graph == nil {
		return nil, ErrGraphNotFound
	}
	return graph, nil
}

func (r *DialogueGraphRunner) sessionGraphLocked(sessionID string) (*sessionState, *graphVersion, error) {
	session, ok := r.sessions[sessionID]
	if !ok || session == nil {
		return nil, nil, ErrSessionNotFound
	}
	lineage, ok := r.graphs[session.GraphID]
	if !ok || lineage == nil {
		return nil, nil, ErrGraphNotFound
	}
	graph, ok := lineage.versions[session.GraphVersion]
	if !ok || graph == nil {
		return nil, nil, ErrGraphNotFound
	}
	return session, graph, nil
}

func (r *DialogueGraphRunner) liveSessionCountLocked() int {
	count := 0
	for _, session := range r.sessions {
		if session != nil && !session.Ended {
			count++
		}
	}
	return count
}

func capacityReached(count, limit int) bool {
	if limit <= 0 {
		return true
	}
	return count >= limit
}

func limitExceeded(count, limit int) bool {
	if limit <= 0 {
		return count > 0
	}
	return count > limit
}

func (session *sessionState) publicState(graph *graphVersion) DialogueState {
	node := graph.byID[session.NodeID]
	return DialogueState{
		SessionID:    session.ID,
		GraphID:      session.GraphID,
		GraphVersion: session.GraphVersion,
		CharacterID:  session.CharacterID,
		NodeID:       session.NodeID,
		Text:         node.Text,
		Variables:    maps.Clone(session.Variables),
		Visited:      slices.Clone(session.Visited),
		Finished:     session.Finished,
		Ended:        session.Ended,
	}
}

func conditionsSatisfied(session *sessionState, conditions []Condition) bool {
	for _, condition := range conditions {
		if condition.Quest != "" && !session.Quests[condition.Quest] {
			return false
		}
		if condition.Key != "" && !variableConditionSatisfied(session.Variables[condition.Key], condition) {
			return false
		}
	}
	return true
}

func variableConditionSatisfied(value int, condition Condition) bool {
	switch condition.Op {
	case CondGe:
		return value >= condition.Value
	case CondEq:
		return value == condition.Value
	case CondLt:
		return value < condition.Value
	default:
		return false
	}
}

func usedOptionKey(nodeID, optionID string) string {
	return nodeID + "\x00" + optionID
}

func optionAvailable(session *sessionState, option Option, optionKey string) bool {
	if option.Once && session.UsedOptions[optionKey] {
		return false
	}
	return conditionsSatisfied(session, option.Requires)
}

func selectableOption(session *sessionState, node Node, optionID string) (Option, string, error) {
	matched := false
	for i := range node.Options {
		option := node.Options[i]
		if option.ID != optionID {
			continue
		}
		matched = true
		optionKey := usedOptionKey(node.ID, option.ID)
		if optionAvailable(session, option, optionKey) {
			return option, optionKey, nil
		}
	}
	if matched {
		return Option{}, "", ErrOptionLocked
	}
	return Option{}, "", ErrOptionLocked
}

func advanceDestination(session *sessionState, node Node) (string, error) {
	if len(node.Options) > 0 {
		for i := range node.Options {
			option := node.Options[i]
			optionKey := usedOptionKey(node.ID, option.ID)
			if optionAvailable(session, option, optionKey) {
				return "", ErrChoicePending
			}
		}
		if node.Fallback == "" {
			return "", ErrNoOption
		}
		return node.Fallback, nil
	}

	if node.Next != "" {
		return node.Next, nil
	}
	if node.Fallback != "" {
		return node.Fallback, nil
	}
	return "", ErrNoOption
}

func nodeEntryEffects(session *sessionState, node Node) map[string]int {
	if node.Once && session.NodeRewards[node.ID] {
		return nil
	}
	return node.Effects
}

func applyNodeEntryEffects(session *sessionState, node Node, maxVariables int) error {
	effects := nodeEntryEffects(session, node)
	if err := applyEffects(session.Variables, effects, maxVariables); err != nil {
		return err
	}
	if node.Once {
		session.NodeRewards[node.ID] = true
	}
	return nil
}

func applyEffects(variables map[string]int, effects map[string]int, maxVariables int) error {
	nextCount := len(variables)
	for key := range effects {
		if _, exists := variables[key]; !exists {
			nextCount++
			if limitExceeded(nextCount, maxVariables) {
				return ErrVariableLimit
			}
		}
	}
	for key, delta := range effects {
		variables[key] += delta
	}
	return nil
}

func reserveEdgeVisit(session *sessionState, from, to string, maxEdgeVisits int) (string, int, error) {
	edgeKey := from + "\x00" + to
	nextCount := session.EdgeVisits[edgeKey] + 1
	if limitExceeded(nextCount, maxEdgeVisits) {
		return "", 0, ErrCycleLimit
	}
	return edgeKey, nextCount, nil
}

func decodeSnapshot(data []byte, limits Limits) (restoredState, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return restoredState{}, ErrBadSnapshot
	}
	if envelope.Format != snapshotFormat || envelope.FormatVersion != snapshotFormatVersion {
		return restoredState{}, ErrBadSnapshot
	}

	graphs := make(map[string]*graphLineage, len(envelope.Graphs))
	for graphID, snapshotLineage := range envelope.Graphs {
		if graphID == "" || len(snapshotLineage.Versions) == 0 {
			return restoredState{}, ErrBadSnapshot
		}
		lineage := &graphLineage{
			current:  snapshotLineage.Current,
			versions: make(map[int]*graphVersion, len(snapshotLineage.Versions)),
		}
		for version, snapshotGraph := range snapshotLineage.Versions {
			if version <= 0 {
				return restoredState{}, ErrBadSnapshot
			}
			_, graph, err := compileGraph(graphID, version, snapshotGraph.Nodes, limits)
			if err != nil {
				return restoredState{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
			}
			lineage.versions[version] = graph
		}
		if _, ok := lineage.versions[lineage.current]; !ok {
			return restoredState{}, ErrBadSnapshot
		}
		graphs[graphID] = lineage
	}

	sessions := make(map[string]*sessionState, len(envelope.Sessions))
	nextSessionSeq := envelope.NextSessionSeq
	activeSessions := 0
	for sessionID, session := range envelope.Sessions {
		if session == nil || sessionID == "" || session.ID != sessionID {
			return restoredState{}, ErrBadSnapshot
		}
		lineage, ok := graphs[session.GraphID]
		if !ok {
			return restoredState{}, ErrBadSnapshot
		}
		graph, ok := lineage.versions[session.GraphVersion]
		if !ok {
			return restoredState{}, ErrBadSnapshot
		}
		if _, ok := graph.byID[session.NodeID]; !ok {
			return restoredState{}, ErrBadSnapshot
		}
		if limitExceeded(len(session.Variables), limits.MaxVariables) {
			return restoredState{}, ErrBadSnapshot
		}
		if err := validateRestoredSession(session, graph, limits); err != nil {
			return restoredState{}, err
		}
		if !session.Ended {
			activeSessions++
		}
		if sequence := trailingSessionSequence(session.ID); sequence > nextSessionSeq {
			nextSessionSeq = sequence
		}
		sessions[sessionID] = session
	}
	if limitExceeded(activeSessions, limits.MaxSessions) {
		return restoredState{}, ErrBadSnapshot
	}

	return restoredState{
		graphs:         graphs,
		sessions:       sessions,
		nextSessionSeq: nextSessionSeq,
	}, nil
}

func validateRestoredSession(session *sessionState, graph *graphVersion, limits Limits) error {
	if session.Variables == nil {
		session.Variables = make(map[string]int)
	}
	if session.Quests == nil {
		session.Quests = make(map[string]bool)
	}
	if session.EdgeVisits == nil {
		session.EdgeVisits = make(map[string]int)
	}
	if session.UsedOptions == nil {
		session.UsedOptions = make(map[string]bool)
	}
	if session.NodeRewards == nil {
		session.NodeRewards = make(map[string]bool)
	}

	for _, visitCount := range session.EdgeVisits {
		if visitCount < 0 || limitExceeded(visitCount, limits.MaxEdgeVisits) {
			return ErrBadSnapshot
		}
	}
	for _, nodeID := range session.Visited {
		if _, ok := graph.byID[nodeID]; !ok {
			return ErrBadSnapshot
		}
	}

	node := graph.byID[session.NodeID]
	if session.Finished && node.Kind != NodeEnd {
		return ErrBadSnapshot
	}
	if node.Kind == NodeEnd {
		session.Finished = true
	}
	return nil
}

func trailingSessionSequence(sessionID string) uint64 {
	separator := strings.LastIndex(sessionID, "-")
	if separator < 0 || separator == len(sessionID)-1 {
		return 0
	}
	sequence, err := strconv.ParseUint(sessionID[separator+1:], 10, 64)
	if err != nil {
		return 0
	}
	return sequence
}
