package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-a-10"
)

func runnerLimits() impl.Limits {
	return impl.Limits{
		MaxNodesPerGraph: 8,
		MaxSessions:      4,
		MaxEdgeVisits:    3,
		MaxVariables:     8,
	}
}

func linearGraph() []impl.Node {
	return []impl.Node{
		{ID: "n1", Text: "meet the elder", Kind: impl.NodeLine, Next: "n2"},
		{ID: "n2", Text: "the end", Kind: impl.NodeEnd},
	}
}

func mustLoad(t *testing.T, r *impl.DialogueGraphRunner, graphID string, nodes []impl.Node) int {
	t.Helper()
	version, err := r.LoadGraph(graphID, nodes)
	if err != nil {
		t.Fatalf("LoadGraph(%q, %d nodes): unexpected error: %v", graphID, len(nodes), err)
	}
	return version
}

func mustStart(t *testing.T, r *impl.DialogueGraphRunner, graphID, characterID string) impl.DialogueState {
	t.Helper()
	st, err := r.Start(graphID, characterID)
	if err != nil {
		t.Fatalf("Start(%q, %q): unexpected error: %v", graphID, characterID, err)
	}
	return st
}

func optionIDs(options []impl.Option) []string {
	ids := make([]string, 0, len(options))
	for _, opt := range options {
		ids = append(ids, opt.ID)
	}
	return ids
}

func TestLoadGraphRejectsEmptyGraph(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	if _, err := r.LoadGraph("g1", nil); !errors.Is(err, impl.ErrBadGraph) {
		t.Fatalf("LoadGraph with no nodes: got %v, want ErrBadGraph", err)
	}
	if _, err := r.LoadGraph("", linearGraph()); !errors.Is(err, impl.ErrBadGraph) {
		t.Fatalf("LoadGraph with empty id: got %v, want ErrBadGraph", err)
	}
}

func TestLoadGraphRejectsDuplicateNodeIDs(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{ID: "n1", Kind: impl.NodeLine},
		{ID: "n1", Kind: impl.NodeEnd},
	}
	if _, err := r.LoadGraph("g1", nodes); !errors.Is(err, impl.ErrDuplicateNode) {
		t.Fatalf("LoadGraph with duplicate ids: got %v, want ErrDuplicateNode", err)
	}
}

func TestLoadGraphRejectsDanglingNext(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{ID: "n1", Kind: impl.NodeLine, Next: "n404"},
	}
	if _, err := r.LoadGraph("g1", nodes); !errors.Is(err, impl.ErrMissingNode) {
		t.Fatalf("LoadGraph with dangling next: got %v, want ErrMissingNode", err)
	}
}

func TestLoadGraphEnforcesNodeLimit(t *testing.T) {
	limits := runnerLimits()
	limits.MaxNodesPerGraph = 2
	r := impl.NewDialogueGraphRunner(limits)
	nodes := []impl.Node{
		{ID: "n1", Kind: impl.NodeLine, Next: "n2"},
		{ID: "n2", Kind: impl.NodeLine, Next: "n3"},
		{ID: "n3", Kind: impl.NodeEnd},
	}
	if _, err := r.LoadGraph("g1", nodes); !errors.Is(err, impl.ErrGraphTooLarge) {
		t.Fatalf("LoadGraph above node budget: got %v, want ErrGraphTooLarge", err)
	}
}

func TestStartRejectsUnknownGraph(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	if _, err := r.Start("g-404", "char-1"); !errors.Is(err, impl.ErrGraphNotFound) {
		t.Fatalf("Start unknown graph: got %v, want ErrGraphNotFound", err)
	}
}

func TestStartBeginsAtFirstNode(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	version := mustLoad(t, r, "g1", linearGraph())
	if version != 1 {
		t.Fatalf("first LoadGraph version: got %d, want 1", version)
	}
	st := mustStart(t, r, "g1", "char-1")
	if st.NodeID != "n1" || st.Text != "meet the elder" {
		t.Fatalf("Start state: got node %q text %q, want n1 with its text", st.NodeID, st.Text)
	}
	if st.GraphVersion != 1 || st.CharacterID != "char-1" {
		t.Fatalf("Start state: got version %d character %q, want 1 and char-1", st.GraphVersion, st.CharacterID)
	}
	if st.Finished {
		t.Fatalf("Start state: got Finished=true on a line node, want false")
	}
}

func TestVisibleOptionsFilterByConditions(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{
			ID:   "n1",
			Kind: impl.NodeChoice,
			Options: []impl.Option{
				{ID: "opt-rep", Next: "n2", Requires: []impl.Condition{{Key: "rep", Op: impl.CondGe, Value: 5}}},
				{ID: "opt-quest", Next: "n2", Requires: []impl.Condition{{Quest: "quest-a"}}},
			},
		},
		{ID: "n2", Kind: impl.NodeEnd},
	}
	mustLoad(t, r, "g1", nodes)
	st := mustStart(t, r, "g1", "char-1")
	if got := optionIDs(mustVisible(t, r, st.SessionID)); len(got) != 0 {
		t.Fatalf("visible options before conditions: got %v, want none", got)
	}
	if err := r.SetVariable(st.SessionID, "rep", 5); err != nil {
		t.Fatalf("SetVariable(rep): unexpected error: %v", err)
	}
	got := optionIDs(mustVisible(t, r, st.SessionID))
	if len(got) != 1 || got[0] != "opt-rep" {
		t.Fatalf("visible options with rep 5: got %v, want [opt-rep]", got)
	}
	if err := r.MarkQuest(st.SessionID, "quest-a"); err != nil {
		t.Fatalf("MarkQuest: unexpected error: %v", err)
	}
	got = optionIDs(mustVisible(t, r, st.SessionID))
	if len(got) != 2 || got[0] != "opt-rep" || got[1] != "opt-quest" {
		t.Fatalf("visible options with quest flag: got %v, want [opt-rep opt-quest]", got)
	}
}

func mustVisible(t *testing.T, r *impl.DialogueGraphRunner, sessionID string) []impl.Option {
	t.Helper()
	options, err := r.VisibleOptions(sessionID)
	if err != nil {
		t.Fatalf("VisibleOptions(%q): unexpected error: %v", sessionID, err)
	}
	return options
}

func TestChooseAppliesEffectsAndAdvances(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{
			ID:   "n1",
			Kind: impl.NodeChoice,
			Options: []impl.Option{
				{ID: "opt-gold", Next: "n2", Effects: map[string]int{"gold": 10}},
			},
		},
		{ID: "n2", Kind: impl.NodeEnd},
	}
	mustLoad(t, r, "g1", nodes)
	st := mustStart(t, r, "g1", "char-1")
	next, err := r.Choose(st.SessionID, "opt-gold")
	if err != nil {
		t.Fatalf("Choose(opt-gold): unexpected error: %v", err)
	}
	if next.NodeID != "n2" {
		t.Fatalf("node after Choose: got %q, want n2", next.NodeID)
	}
	if next.Variables["gold"] != 10 {
		t.Fatalf("gold after Choose: got %d, want 10", next.Variables["gold"])
	}
}

func TestChooseRejectsLockedOption(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{
			ID:   "n1",
			Kind: impl.NodeChoice,
			Options: []impl.Option{
				{ID: "opt-rep", Next: "n2", Requires: []impl.Condition{{Key: "rep", Op: impl.CondGe, Value: 5}}},
			},
		},
		{ID: "n2", Kind: impl.NodeEnd},
	}
	mustLoad(t, r, "g1", nodes)
	st := mustStart(t, r, "g1", "char-1")
	if _, err := r.Choose(st.SessionID, "opt-rep"); !errors.Is(err, impl.ErrOptionLocked) {
		t.Fatalf("Choose locked option: got %v, want ErrOptionLocked", err)
	}
}

func TestFallbackUsedWhenEveryOptionLocked(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{
			ID:       "n1",
			Kind:     impl.NodeChoice,
			Fallback: "f1",
			Options: []impl.Option{
				{ID: "opt-a", Next: "n2", Requires: []impl.Condition{{Key: "rep", Op: impl.CondGe, Value: 99}}},
			},
		},
		{ID: "f1", Text: "the elder walks away", Kind: impl.NodeFallback, Next: "n2"},
		{ID: "n2", Kind: impl.NodeEnd},
	}
	mustLoad(t, r, "g1", nodes)
	st := mustStart(t, r, "g1", "char-1")
	next, err := r.Advance(st.SessionID)
	if err != nil {
		t.Fatalf("Advance into fallback: unexpected error: %v", err)
	}
	if next.NodeID != "f1" || next.Text != "the elder walks away" {
		t.Fatalf("fallback node: got %q text %q, want f1", next.NodeID, next.Text)
	}
}

func TestAdvanceMarksEndNodeFinished(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	mustLoad(t, r, "g1", linearGraph())
	st := mustStart(t, r, "g1", "char-1")
	next, err := r.Advance(st.SessionID)
	if err != nil {
		t.Fatalf("Advance: unexpected error: %v", err)
	}
	if next.NodeID != "n2" || !next.Finished {
		t.Fatalf("state after Advance: got node %q finished %v, want n2 finished", next.NodeID, next.Finished)
	}
	if _, err := r.Advance(st.SessionID); !errors.Is(err, impl.ErrSessionClosed) {
		t.Fatalf("Advance past the end node: got %v, want ErrSessionClosed", err)
	}
}

func TestEndClosesSession(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	mustLoad(t, r, "g1", linearGraph())
	st := mustStart(t, r, "g1", "char-1")
	if err := r.End(st.SessionID, "player quit"); err != nil {
		t.Fatalf("End: unexpected error: %v", err)
	}
	if err := r.End(st.SessionID, "player quit"); err != nil {
		t.Fatalf("second End: unexpected error: %v", err)
	}
	if _, err := r.Choose(st.SessionID, "opt-a"); !errors.Is(err, impl.ErrSessionClosed) {
		t.Fatalf("Choose after End: got %v, want ErrSessionClosed", err)
	}
	after, err := r.State(st.SessionID)
	if err != nil {
		t.Fatalf("State after End: unexpected error: %v", err)
	}
	if !after.Ended {
		t.Fatalf("State after End: got Ended=false, want true")
	}
}

func TestSessionLimitEnforced(t *testing.T) {
	limits := runnerLimits()
	limits.MaxSessions = 1
	r := impl.NewDialogueGraphRunner(limits)
	mustLoad(t, r, "g1", linearGraph())
	mustStart(t, r, "g1", "char-1")
	if _, err := r.Start("g1", "char-2"); !errors.Is(err, impl.ErrSessionLimit) {
		t.Fatalf("Start above session budget: got %v, want ErrSessionLimit", err)
	}
	if got := r.SessionCount(); got != 1 {
		t.Fatalf("live sessions: got %d, want 1", got)
	}
}

func TestSnapshotRestoreKeepsSessions(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	nodes := []impl.Node{
		{
			ID:   "n1",
			Kind: impl.NodeChoice,
			Options: []impl.Option{
				{ID: "opt-a", Next: "n2", Effects: map[string]int{"gold": 5}},
			},
		},
		{
			ID:   "n2",
			Kind: impl.NodeChoice,
			Options: []impl.Option{
				{ID: "opt-b", Next: "n3", Requires: []impl.Condition{{Quest: "quest-a"}}},
			},
		},
		{ID: "n3", Kind: impl.NodeEnd},
	}
	mustLoad(t, r, "g1", nodes)
	st := mustStart(t, r, "g1", "char-1")
	if err := r.MarkQuest(st.SessionID, "quest-a"); err != nil {
		t.Fatalf("MarkQuest: unexpected error: %v", err)
	}
	if _, err := r.Choose(st.SessionID, "opt-a"); err != nil {
		t.Fatalf("Choose(opt-a): unexpected error: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Snapshot: got empty payload")
	}
	restored := impl.NewDialogueGraphRunner(runnerLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("Restore: unexpected error: %v", err)
	}
	state, err := restored.State(st.SessionID)
	if err != nil {
		t.Fatalf("State after Restore: unexpected error: %v", err)
	}
	if state.NodeID != "n2" || state.Variables["gold"] != 5 {
		t.Fatalf("restored state: got node %q gold %d, want n2 with gold 5", state.NodeID, state.Variables["gold"])
	}
	got := optionIDs(mustVisible(t, restored, st.SessionID))
	if len(got) != 1 || got[0] != "opt-b" {
		t.Fatalf("visible options after Restore: got %v, want [opt-b]", got)
	}
	if got := restored.SessionCount(); got != 1 {
		t.Fatalf("live sessions after Restore: got %d, want 1", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	r := impl.NewDialogueGraphRunner(runnerLimits())
	mustLoad(t, r, "g1", linearGraph())
	st := mustStart(t, r, "g1", "char-1")
	if err := r.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore malformed payload: got %v, want ErrBadSnapshot", err)
	}
	if err := r.Restore(nil); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore empty payload: got %v, want ErrBadSnapshot", err)
	}
	if _, err := r.State(st.SessionID); err != nil {
		t.Fatalf("State after rejected Restore: unexpected error: %v", err)
	}
}
