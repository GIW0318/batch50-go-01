package task

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	impl "gsb/batch50-go-game-a-21"
)

func tutorialLimits() impl.Limits {
	return impl.Limits{MaxPlayers: 8, MaxRewardsPerPlayer: 4, MaxStepsPerFlow: 6}
}

func tutorialSteps() []impl.StepDef {
	return []impl.StepDef{
		{ID: "move", Title: "Move to the drill ground", Reward: 10},
		{ID: "attack", Title: "Land the first hit", Reward: 20},
		{ID: "shop", Title: "Buy a starter weapon", Reward: 30},
	}
}

func newTutorialGuard(t *testing.T) *impl.TutorialProgressGuard {
	t.Helper()
	g := impl.NewTutorialProgressGuard(tutorialLimits())
	if err := g.SetFlow("tutorial-v1", tutorialSteps()); err != nil {
		t.Fatalf("registering the flow must succeed, got %v", err)
	}
	return g
}

func TestBeginOpensFirstStep(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	step, err := g.Current("player-1")
	if err != nil {
		t.Fatalf("current step must resolve, got %v", err)
	}
	if step.FlowID != "tutorial-v1" || step.ID != "move" {
		t.Fatalf("current step = %+v, want the move step of tutorial-v1", step)
	}
	if step.Index != 1 || step.Total != 3 {
		t.Fatalf("step position = %d/%d, want 1/3", step.Index, step.Total)
	}
	if step.Done || step.Skipped {
		t.Fatalf("a fresh step must be open: done=%v skipped=%v", step.Done, step.Skipped)
	}
}

func TestCompleteAdvancesInOrder(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	progress, err := g.Complete("player-1", "move")
	if err != nil {
		t.Fatalf("completing the first step must succeed, got %v", err)
	}
	if progress.StepID != "attack" || progress.Index != 2 {
		t.Fatalf("progress after first step = %+v, want the attack step at index 2", progress)
	}
	if progress.Seq <= 0 {
		t.Fatalf("progress sequence = %d, want a positive number", progress.Seq)
	}
	step, err := g.Current("player-1")
	if err != nil {
		t.Fatalf("current step must resolve, got %v", err)
	}
	if step.ID != "attack" || step.Index != 2 {
		t.Fatalf("current step = %+v, want attack at index 2", step)
	}
}

func TestCompleteOutOfOrderKeepsPosition(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if _, err := g.Complete("player-1", "shop"); !errors.Is(err, impl.ErrOutOfOrder) {
		t.Fatalf("skipping ahead must report ErrOutOfOrder, got %v", err)
	}
	step, err := g.Current("player-1")
	if err != nil {
		t.Fatalf("current step must resolve, got %v", err)
	}
	if step.ID != "move" || step.Index != 1 || step.Done {
		t.Fatalf("rejected completion moved the position to %+v", step)
	}
	if state, ok := g.State("player-1"); !ok || state.Index != 1 {
		t.Fatalf("durable position = %+v ok=%v, want index 1", state, ok)
	}
}

func TestCompleteUnknownStepRejected(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if _, err := g.Complete("player-1", "dance"); !errors.Is(err, impl.ErrUnknownStep) {
		t.Fatalf("unknown step must report ErrUnknownStep, got %v", err)
	}
	if _, err := g.Complete("ghost", "move"); !errors.Is(err, impl.ErrUnknownPlayer) {
		t.Fatalf("unknown player must report ErrUnknownPlayer, got %v", err)
	}
}

func TestBeginRejectsUnknownFlowAndBlankPlayer(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v9"); !errors.Is(err, impl.ErrUnknownFlow) {
		t.Fatalf("unknown flow must report ErrUnknownFlow, got %v", err)
	}
	if err := g.Begin("", "tutorial-v1"); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("blank player must report ErrBadPlayer, got %v", err)
	}
}

func TestCompleteSameStepIsIdempotent(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	first, err := g.Complete("player-1", "move")
	if err != nil {
		t.Fatalf("first completion must succeed, got %v", err)
	}
	second, err := g.Complete("player-1", "move")
	if err != nil {
		t.Fatalf("repeated completion must be idempotent, got %v", err)
	}
	if second.Index != first.Index || second.StepID != first.StepID {
		t.Fatalf("repeated completion returned %+v, want the same position as %+v", second, first)
	}
	if second.Seq != first.Seq {
		t.Fatalf("repeated completion bumped the sequence from %d to %d", first.Seq, second.Seq)
	}
	step, err := g.Current("player-1")
	if err != nil {
		t.Fatalf("current step must resolve, got %v", err)
	}
	if step.ID != "attack" {
		t.Fatalf("current step = %q after a repeated completion, want attack", step.ID)
	}
}

func TestGrantOncePerStep(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if _, err := g.Complete("player-1", "move"); err != nil {
		t.Fatalf("completing the first step must succeed, got %v", err)
	}
	if err := g.Grant("player-1", "move"); err != nil {
		t.Fatalf("granting the first step reward must succeed, got %v", err)
	}
	rewards := g.Rewards("player-1")
	if len(rewards) != 1 {
		t.Fatalf("reward ledger length = %d, want 1", len(rewards))
	}
	if rewards[0].StepID != "move" || rewards[0].Amount != 10 {
		t.Fatalf("reward record = %+v, want move worth 10", rewards[0])
	}
	if err := g.Grant("player-1", "move"); !errors.Is(err, impl.ErrAlreadyRewarded) {
		t.Fatalf("second grant must report ErrAlreadyRewarded, got %v", err)
	}
	if got := len(g.Rewards("player-1")); got != 1 {
		t.Fatalf("reward ledger length = %d after a repeated grant, want 1", got)
	}
}

func TestGrantRequiresCompletedStep(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if err := g.Grant("player-1", "attack"); !errors.Is(err, impl.ErrStepNotReached) {
		t.Fatalf("granting an unreached step must report ErrStepNotReached, got %v", err)
	}
	if got := len(g.Rewards("player-1")); got != 0 {
		t.Fatalf("reward ledger length = %d, want 0", got)
	}
}

func TestSkipMarksFlowSkipped(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if err := g.Skip("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("skipping the flow must succeed, got %v", err)
	}
	step, err := g.Current("player-1")
	if err != nil {
		t.Fatalf("current step must resolve after a skip, got %v", err)
	}
	if !step.Skipped || !step.Done {
		t.Fatalf("step after skip = %+v, want done and skipped", step)
	}
	if _, err := g.Complete("player-1", "move"); !errors.Is(err, impl.ErrFlowSkipped) {
		t.Fatalf("completing a skipped flow must report ErrFlowSkipped, got %v", err)
	}
	if _, err := g.Complete("player-1", "attack"); !errors.Is(err, impl.ErrFlowSkipped) {
		t.Fatalf("completing a later step must report ErrFlowSkipped, got %v", err)
	}
	if got := len(g.Rewards("player-1")); got != 0 {
		t.Fatalf("reward ledger length = %d after a skip, want 0", got)
	}
}

func TestSetFlowValidation(t *testing.T) {
	g := impl.NewTutorialProgressGuard(tutorialLimits())
	duplicate := []impl.StepDef{{ID: "move", Reward: 10}, {ID: "move", Reward: 20}}
	if err := g.SetFlow("dup", duplicate); !errors.Is(err, impl.ErrDuplicateStep) {
		t.Fatalf("duplicate step ids must report ErrDuplicateStep, got %v", err)
	}
	if err := g.SetFlow("", tutorialSteps()); !errors.Is(err, impl.ErrBadFlow) {
		t.Fatalf("blank flow id must report ErrBadFlow, got %v", err)
	}
	long := []impl.StepDef{
		{ID: "s1"}, {ID: "s2"}, {ID: "s3"}, {ID: "s4"}, {ID: "s5"}, {ID: "s6"}, {ID: "s7"},
	}
	if err := g.SetFlow("long", long); !errors.Is(err, impl.ErrLimit) {
		t.Fatalf("seven steps must report ErrLimit, got %v", err)
	}
	if got := len(g.Flow("dup")); got != 0 {
		t.Fatalf("rejected flow definition left %d steps behind", got)
	}
	if err := g.SetFlow("tutorial-v1", tutorialSteps()); err != nil {
		t.Fatalf("a valid flow must be accepted, got %v", err)
	}
	if got := len(g.Flow("tutorial-v1")); got != 3 {
		t.Fatalf("accepted flow has %d steps, want 3", got)
	}
}

func TestPlayerLimitEnforced(t *testing.T) {
	g := impl.NewTutorialProgressGuard(impl.Limits{MaxPlayers: 2, MaxRewardsPerPlayer: 4, MaxStepsPerFlow: 6})
	if err := g.SetFlow("tutorial-v1", tutorialSteps()); err != nil {
		t.Fatalf("registering the flow must succeed, got %v", err)
	}
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("first player must be accepted, got %v", err)
	}
	if err := g.Begin("player-2", "tutorial-v1"); err != nil {
		t.Fatalf("second player must be accepted, got %v", err)
	}
	if err := g.Begin("player-3", "tutorial-v1"); !errors.Is(err, impl.ErrLimit) {
		t.Fatalf("third player must report ErrLimit, got %v", err)
	}
	if _, ok := g.State("player-3"); ok {
		t.Fatalf("a player rejected by the cap must not be recorded")
	}
}

func TestRewardLedgerLimitEnforced(t *testing.T) {
	g := impl.NewTutorialProgressGuard(impl.Limits{MaxPlayers: 8, MaxRewardsPerPlayer: 2, MaxStepsPerFlow: 6})
	if err := g.SetFlow("tutorial-v1", tutorialSteps()); err != nil {
		t.Fatalf("registering the flow must succeed, got %v", err)
	}
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	for _, step := range []string{"move", "attack"} {
		if _, err := g.Complete("player-1", step); err != nil {
			t.Fatalf("completing %q must succeed, got %v", step, err)
		}
		if err := g.Grant("player-1", step); err != nil {
			t.Fatalf("granting %q must succeed, got %v", step, err)
		}
	}
	if got := len(g.Rewards("player-1")); got != 2 {
		t.Fatalf("reward ledger length = %d, want 2", got)
	}
	if _, err := g.Complete("player-1", "shop"); err != nil {
		t.Fatalf("completing the third step must succeed, got %v", err)
	}
	if err := g.Grant("player-1", "shop"); !errors.Is(err, impl.ErrLimit) {
		t.Fatalf("the third reward must report ErrLimit, got %v", err)
	}
	if got := len(g.Rewards("player-1")); got != 2 {
		t.Fatalf("reward ledger length = %d after the cap, want 2", got)
	}
}

func TestSnapshotRestoreKeepsProgress(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if _, err := g.Complete("player-1", "move"); err != nil {
		t.Fatalf("completing the first step must succeed, got %v", err)
	}
	if err := g.Grant("player-1", "move"); err != nil {
		t.Fatalf("granting the first reward must succeed, got %v", err)
	}
	before, ok := g.State("player-1")
	if !ok {
		t.Fatalf("state must exist before the snapshot")
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewTutorialProgressGuard(tutorialLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	if got := len(restored.Flow("tutorial-v1")); got != 3 {
		t.Fatalf("restored flow has %d steps, want 3", got)
	}
	step, err := restored.Current("player-1")
	if err != nil {
		t.Fatalf("current step must resolve after restore, got %v", err)
	}
	if step.ID != "attack" || step.Index != 2 {
		t.Fatalf("restored step = %+v, want attack at index 2", step)
	}
	after, ok := restored.State("player-1")
	if !ok {
		t.Fatalf("restored state is missing")
	}
	if after.Seq != before.Seq {
		t.Fatalf("restored state = %+v, want the sequence to continue at %d", after, before.Seq)
	}
	rewards := restored.Rewards("player-1")
	if len(rewards) != 1 || rewards[0].Amount != 10 {
		t.Fatalf("restored rewards = %+v, want the move reward worth 10", rewards)
	}
}

func TestRestoreRejectsGarbageSnapshot(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if err := g.Restore([]byte("{this is not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a garbage snapshot must report ErrBadSnapshot, got %v", err)
	}
	step, err := g.Current("player-1")
	if err != nil {
		t.Fatalf("existing progress must survive a failed restore, got %v", err)
	}
	if step.ID != "move" {
		t.Fatalf("current step = %q after a failed restore, want move", step.ID)
	}
}

func TestConcurrentGrantLandsOnce(t *testing.T) {
	g := newTutorialGuard(t)
	if err := g.Begin("player-1", "tutorial-v1"); err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if _, err := g.Complete("player-1", "move"); err != nil {
		t.Fatalf("completing the first step must succeed, got %v", err)
	}
	var granted int32
	var rejected int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.Grant("player-1", "move")
			switch {
			case err == nil:
				atomic.AddInt32(&granted, 1)
			case errors.Is(err, impl.ErrAlreadyRewarded):
				atomic.AddInt32(&rejected, 1)
			}
		}()
	}
	wg.Wait()
	if granted != 1 {
		t.Fatalf("concurrent grants succeeded %d times, want exactly 1", granted)
	}
	if rejected != 7 {
		t.Fatalf("concurrent grants were rejected %d times, want 7", rejected)
	}
	if got := len(g.Rewards("player-1")); got != 1 {
		t.Fatalf("reward ledger length = %d after the race, want 1", got)
	}
}
