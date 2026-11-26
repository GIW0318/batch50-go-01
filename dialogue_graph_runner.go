// Package task runs branching story dialogue graphs: node loading, session
// state, option conditions, one shot nodes inside loops and graph version
// pinning for sessions that are already running.
package task

import (
	"errors"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	limits Limits
}

// NewDialogueGraphRunner builds a runner with the given limits.
func NewDialogueGraphRunner(limits Limits) *DialogueGraphRunner {
	return &DialogueGraphRunner{limits: limits}
}

// LoadGraph validates one graph, stores it and returns its new version.
func (r *DialogueGraphRunner) LoadGraph(graphID string, nodes []Node) (int, error) {
	return 0, errNotImplemented
}

// GraphVersion reports the version of one loaded graph.
func (r *DialogueGraphRunner) GraphVersion(graphID string) (int, error) {
	return 0, errNotImplemented
}

// Start opens one session pinned to the current graph version.
func (r *DialogueGraphRunner) Start(graphID, characterID string) (DialogueState, error) {
	return DialogueState{}, errNotImplemented
}

// Choose takes one option of the current node.
func (r *DialogueGraphRunner) Choose(sessionID, optionID string) (DialogueState, error) {
	return DialogueState{}, errNotImplemented
}

// Advance follows the linear next node or the fallback of the current node.
func (r *DialogueGraphRunner) Advance(sessionID string) (DialogueState, error) {
	return DialogueState{}, errNotImplemented
}

// VisibleOptions lists the options that the session can select right now.
func (r *DialogueGraphRunner) VisibleOptions(sessionID string) ([]Option, error) {
	return nil, errNotImplemented
}

// State reports the public state of one session.
func (r *DialogueGraphRunner) State(sessionID string) (DialogueState, error) {
	return DialogueState{}, errNotImplemented
}

// End closes one session with a reason.
func (r *DialogueGraphRunner) End(sessionID, reason string) error {
	return errNotImplemented
}

// SetVariable writes one session variable.
func (r *DialogueGraphRunner) SetVariable(sessionID, key string, value int) error {
	return errNotImplemented
}

// MarkQuest records one quest flag on the session.
func (r *DialogueGraphRunner) MarkQuest(sessionID, quest string) error {
	return errNotImplemented
}

// SessionCount reports the number of live sessions.
func (r *DialogueGraphRunner) SessionCount() int {
	return 0
}

// Snapshot serialises graphs, versions and every session.
func (r *DialogueGraphRunner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *DialogueGraphRunner) Restore(data []byte) error {
	return errNotImplemented
}
