package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-a-03"
)

func dungeonConfig() impl.RunConfig {
	return impl.RunConfig{MaxRuns: 2, MaxParty: 4, Checkpoints: 3, MaxGrants: 8}
}

func openParty(t *testing.T, d *impl.DungeonCheckpoint, runID string, party ...string) impl.Run {
	t.Helper()
	run, err := d.OpenRun(runID, party)
	if err != nil {
		t.Fatalf("open run %s: %v", runID, err)
	}
	return run
}

func advanceTo(t *testing.T, d *impl.DungeonCheckpoint, runID string, checkpoint int) {
	t.Helper()
	if _, err := d.Advance(runID, checkpoint); err != nil {
		t.Fatalf("advance %s to %d: %v", runID, checkpoint, err)
	}
}

func TestOpenRunCreatesEntranceParty(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	run := openParty(t, d, "run-1", "p1", "p2", "p3")
	if run.ID != "run-1" {
		t.Fatalf("run id = %q, want run-1", run.ID)
	}
	if run.State != impl.RunActive {
		t.Fatalf("run state = %q, want %q", run.State, impl.RunActive)
	}
	if run.Checkpoint != 0 {
		t.Fatalf("entrance checkpoint = %d, want 0", run.Checkpoint)
	}
	if len(run.Members) != 3 {
		t.Fatalf("party size = %d, want 3", len(run.Members))
	}
	for i, member := range run.Members {
		if member.Ordinal != i {
			t.Fatalf("member %d ordinal = %d, want %d", i, member.Ordinal, i)
		}
		if member.State != impl.MemberOnline || member.Checkpoint != 0 {
			t.Fatalf("member %d = %+v, want online at the entrance", i, member)
		}
	}
	if run.Members[1].PlayerID != "p2" {
		t.Fatalf("members must keep join order, got %+v", run.Members)
	}
	if len(run.Grants) != 0 {
		t.Fatalf("fresh run already has %d grants", len(run.Grants))
	}
	if _, ok := d.Run("run-1"); !ok {
		t.Fatalf("opened run must be readable")
	}
}

func TestOpenRunRejectsBadParty(t *testing.T) {
	d := impl.NewDungeonCheckpoint(impl.RunConfig{MaxRuns: 2, MaxParty: 2, Checkpoints: 3, MaxGrants: 4})
	if _, err := d.OpenRun("   ", []string{"p1"}); !errors.Is(err, impl.ErrBlankRun) {
		t.Fatalf("blank run id must fail with ErrBlankRun, got %v", err)
	}
	if _, err := d.OpenRun("run-0", nil); !errors.Is(err, impl.ErrEmptyParty) {
		t.Fatalf("empty party must fail with ErrEmptyParty, got %v", err)
	}
	if _, err := d.OpenRun("run-0", []string{"p1", "  "}); !errors.Is(err, impl.ErrBlankMember) {
		t.Fatalf("blank member must fail with ErrBlankMember, got %v", err)
	}
	if _, err := d.OpenRun("run-0", []string{"p1", "p1"}); !errors.Is(err, impl.ErrDuplicateMember) {
		t.Fatalf("duplicate member must fail with ErrDuplicateMember, got %v", err)
	}
	if _, err := d.OpenRun("run-0", []string{"p1", "p2", "p3"}); !errors.Is(err, impl.ErrPartyFull) {
		t.Fatalf("party of three over a limit of two must fail with ErrPartyFull, got %v", err)
	}
	if _, err := d.OpenRun("run-1", []string{"p1", "p2"}); err != nil {
		t.Fatalf("open run-1: %v", err)
	}
	if _, err := d.OpenRun("run-1", []string{"p3"}); !errors.Is(err, impl.ErrRunExists) {
		t.Fatalf("reopening run-1 must fail with ErrRunExists, got %v", err)
	}
	if _, err := d.OpenRun("run-2", []string{"p1"}); err != nil {
		t.Fatalf("open run-2: %v", err)
	}
	if _, err := d.OpenRun("run-3", []string{"p1"}); !errors.Is(err, impl.ErrRunLimit) {
		t.Fatalf("third run over a limit of two must fail with ErrRunLimit, got %v", err)
	}
}

func TestLeaveMarksOfflineIdempotently(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	if err := d.Leave("run-1", "p1"); err != nil {
		t.Fatalf("leave p1: %v", err)
	}
	members := d.Members("run-1")
	if len(members) != 2 {
		t.Fatalf("party size after leave = %d, want 2", len(members))
	}
	if members[0].State != impl.MemberOffline || members[1].State != impl.MemberOnline {
		t.Fatalf("member states after leave = %q / %q, want offline / online", members[0].State, members[1].State)
	}
	if members[0].Checkpoint != 0 {
		t.Fatalf("offline member lost its checkpoint, got %d", members[0].Checkpoint)
	}
	if err := d.Leave("run-1", "p1"); err != nil {
		t.Fatalf("leaving twice must stay successful, got %v", err)
	}
	if err := d.Leave("run-1", "p9"); !errors.Is(err, impl.ErrUnknownMember) {
		t.Fatalf("leaving a stranger must fail with ErrUnknownMember, got %v", err)
	}
	if err := d.Leave("run-9", "p1"); !errors.Is(err, impl.ErrUnknownRun) {
		t.Fatalf("leaving an unknown run must fail with ErrUnknownRun, got %v", err)
	}
}

func TestRejoinBindsToInstanceCheckpoint(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	advanceTo(t, d, "run-1", 2)
	if err := d.Leave("run-1", "p2"); err != nil {
		t.Fatalf("leave p2: %v", err)
	}
	member, err := d.Join("run-1", "p2", 2)
	if err != nil {
		t.Fatalf("rejoin p2: %v", err)
	}
	if member.State != impl.MemberOnline || member.Checkpoint != 2 {
		t.Fatalf("rejoined member = %+v, want online at checkpoint 2", member)
	}
	run, _ := d.Run("run-1")
	if run.Checkpoint != 2 {
		t.Fatalf("rejoin moved the instance checkpoint to %d, want 2", run.Checkpoint)
	}
	if _, err := d.Join("run-1", "p1", 2); !errors.Is(err, impl.ErrMemberOnline) {
		t.Fatalf("joining an online member must fail with ErrMemberOnline, got %v", err)
	}
	if _, err := d.Join("run-1", "p9", 2); !errors.Is(err, impl.ErrUnknownMember) {
		t.Fatalf("joining a stranger must fail with ErrUnknownMember, got %v", err)
	}
	if _, err := d.Join("run-9", "p1", 2); !errors.Is(err, impl.ErrUnknownRun) {
		t.Fatalf("joining an unknown run must fail with ErrUnknownRun, got %v", err)
	}
}

func TestAdvanceRequiresEveryoneOnline(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	if err := d.Leave("run-1", "p2"); err != nil {
		t.Fatalf("leave p2: %v", err)
	}
	if _, err := d.Advance("run-1", 1); !errors.Is(err, impl.ErrMemberOffline) {
		t.Fatalf("advance with an offline member must fail with ErrMemberOffline, got %v", err)
	}
	run, _ := d.Run("run-1")
	if run.Checkpoint != 0 {
		t.Fatalf("rejected advance moved the run to checkpoint %d", run.Checkpoint)
	}
	if _, err := d.Join("run-1", "p2", 0); err != nil {
		t.Fatalf("rejoin p2: %v", err)
	}
	advanceTo(t, d, "run-1", 1)
	run, _ = d.Run("run-1")
	if run.Checkpoint != 1 {
		t.Fatalf("checkpoint after advance = %d, want 1", run.Checkpoint)
	}
}

func TestAdvanceRejectsGapRegressAndLimit(t *testing.T) {
	d := impl.NewDungeonCheckpoint(impl.RunConfig{MaxRuns: 1, MaxParty: 2, Checkpoints: 3, MaxGrants: 4})
	openParty(t, d, "run-1", "p1", "p2")
	if _, err := d.Advance("run-1", 2); !errors.Is(err, impl.ErrCheckpointGap) {
		t.Fatalf("skipping a checkpoint must fail with ErrCheckpointGap, got %v", err)
	}
	advanceTo(t, d, "run-1", 1)
	if _, err := d.Advance("run-1", 1); !errors.Is(err, impl.ErrCheckpointRegress) {
		t.Fatalf("advancing to the same checkpoint must fail with ErrCheckpointRegress, got %v", err)
	}
	advanceTo(t, d, "run-1", 2)
	advanceTo(t, d, "run-1", 3)
	if _, err := d.Advance("run-1", 4); !errors.Is(err, impl.ErrCheckpointLimit) {
		t.Fatalf("advancing past the last checkpoint must fail with ErrCheckpointLimit, got %v", err)
	}
}

func TestGrantRewardRecordsLedgerEntry(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	grant, err := d.GrantReward("run-1", "p1", "sword", 2)
	if err != nil {
		t.Fatalf("grant sword: %v", err)
	}
	if grant.RunID != "run-1" || grant.PlayerID != "p1" || grant.ItemID != "sword" {
		t.Fatalf("grant identity = %+v, want run-1/p1/sword", grant)
	}
	if grant.Qty != 2 {
		t.Fatalf("grant quantity = %d, want 2", grant.Qty)
	}
	if grant.Checkpoint != 1 {
		t.Fatalf("grant checkpoint = %d, want the current checkpoint 1", grant.Checkpoint)
	}
	if grant.ID == "" || grant.Seq <= 0 {
		t.Fatalf("grant must carry an id and a sequence, got %+v", grant)
	}
	grants := d.Grants("run-1")
	if len(grants) != 1 || grants[0].Seq != grant.Seq {
		t.Fatalf("ledger = %+v, want exactly the new grant", grants)
	}
}

func TestGrantRewardIsIdempotentAndValidated(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	first, err := d.GrantReward("run-1", "p1", "sword", 1)
	if err != nil {
		t.Fatalf("grant sword: %v", err)
	}
	again, err := d.GrantReward("run-1", "p1", "sword", 1)
	if err != nil {
		t.Fatalf("repeating the same grant must stay successful, got %v", err)
	}
	if again.Seq != first.Seq || again.ID != first.ID {
		t.Fatalf("repeated grant = %+v, want the original %+v", again, first)
	}
	if got := len(d.Grants("run-1")); got != 1 {
		t.Fatalf("ledger after a repeated grant = %d entries, want 1", got)
	}
	if _, err := d.GrantReward("run-1", "p1", "  ", 1); !errors.Is(err, impl.ErrBlankItem) {
		t.Fatalf("blank item must fail with ErrBlankItem, got %v", err)
	}
	if _, err := d.GrantReward("run-1", "p1", "sword", 0); !errors.Is(err, impl.ErrBadQuantity) {
		t.Fatalf("zero quantity must fail with ErrBadQuantity, got %v", err)
	}
	if _, err := d.GrantReward("run-1", "p9", "sword", 1); !errors.Is(err, impl.ErrUnknownMember) {
		t.Fatalf("grant to a stranger must fail with ErrUnknownMember, got %v", err)
	}
	if err := d.Leave("run-1", "p2"); err != nil {
		t.Fatalf("leave p2: %v", err)
	}
	if _, err := d.GrantReward("run-1", "p2", "shield", 1); !errors.Is(err, impl.ErrMemberOffline) {
		t.Fatalf("grant to an offline member must fail with ErrMemberOffline, got %v", err)
	}
	if _, err := d.GrantReward("run-9", "p1", "shield", 1); !errors.Is(err, impl.ErrUnknownRun) {
		t.Fatalf("grant in an unknown run must fail with ErrUnknownRun, got %v", err)
	}
}

func TestRollbackMovesRunBackAndDropsLaterGrants(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	if _, err := d.GrantReward("run-1", "p1", "sword", 1); err != nil {
		t.Fatalf("grant sword at checkpoint 1: %v", err)
	}
	advanceTo(t, d, "run-1", 2)
	advanceTo(t, d, "run-1", 3)
	if _, err := d.GrantReward("run-1", "p1", "gem", 5); err != nil {
		t.Fatalf("grant gem at checkpoint 3: %v", err)
	}
	run, err := d.Rollback("run-1", 1)
	if err != nil {
		t.Fatalf("rollback to checkpoint 1: %v", err)
	}
	if run.Checkpoint != 1 {
		t.Fatalf("checkpoint after rollback = %d, want 1", run.Checkpoint)
	}
	for _, member := range run.Members {
		if member.Checkpoint != 1 {
			t.Fatalf("member %s stayed at checkpoint %d after rollback", member.PlayerID, member.Checkpoint)
		}
	}
	grants := d.Grants("run-1")
	if len(grants) != 1 || grants[0].ItemID != "sword" || grants[0].Checkpoint != 1 {
		t.Fatalf("ledger after rollback = %+v, want only the sword from checkpoint 1", grants)
	}
}

func TestRollbackRejectsForwardAndUnknown(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	advanceTo(t, d, "run-1", 2)
	if _, err := d.Rollback("run-1", 2); !errors.Is(err, impl.ErrRollbackForward) {
		t.Fatalf("rollback to the current checkpoint must fail with ErrRollbackForward, got %v", err)
	}
	if _, err := d.Rollback("run-1", -1); !errors.Is(err, impl.ErrCheckpointRegress) {
		t.Fatalf("negative rollback target must fail with ErrCheckpointRegress, got %v", err)
	}
	if _, err := d.Rollback("run-9", 1); !errors.Is(err, impl.ErrUnknownRun) {
		t.Fatalf("rollback of an unknown run must fail with ErrUnknownRun, got %v", err)
	}
	run, _ := d.Run("run-1")
	if run.Checkpoint != 2 {
		t.Fatalf("rejected rollback moved the run to checkpoint %d", run.Checkpoint)
	}
}

func TestClaimReclaimWithoutPendingIsRejected(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	if _, err := d.GrantReward("run-1", "p1", "sword", 1); err != nil {
		t.Fatalf("grant sword: %v", err)
	}
	if _, err := d.ClaimReclaim("run-1", "p1", "sword"); !errors.Is(err, impl.ErrUnknownReclaim) {
		t.Fatalf("claiming a live reward must fail with ErrUnknownReclaim, got %v", err)
	}
	if _, err := d.ClaimReclaim("run-1", "p2", "shield"); !errors.Is(err, impl.ErrUnknownReclaim) {
		t.Fatalf("claiming a reward that never existed must fail with ErrUnknownReclaim, got %v", err)
	}
	if _, err := d.ClaimReclaim("run-9", "p1", "sword"); !errors.Is(err, impl.ErrUnknownRun) {
		t.Fatalf("claiming in an unknown run must fail with ErrUnknownRun, got %v", err)
	}
}

func TestCloseRunRejectsFurtherProgress(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1", "p2")
	if err := d.CloseRun("run-1"); err != nil {
		t.Fatalf("close run-1: %v", err)
	}
	if err := d.CloseRun("run-1"); err != nil {
		t.Fatalf("closing twice must stay successful, got %v", err)
	}
	run, _ := d.Run("run-1")
	if run.State != impl.RunClosed {
		t.Fatalf("run state after close = %q, want %q", run.State, impl.RunClosed)
	}
	if _, err := d.Advance("run-1", 1); !errors.Is(err, impl.ErrRunClosed) {
		t.Fatalf("advance on a closed run must fail with ErrRunClosed, got %v", err)
	}
	if _, err := d.GrantReward("run-1", "p1", "sword", 1); !errors.Is(err, impl.ErrRunClosed) {
		t.Fatalf("grant on a closed run must fail with ErrRunClosed, got %v", err)
	}
	if _, err := d.Rollback("run-1", 0); !errors.Is(err, impl.ErrRunClosed) {
		t.Fatalf("rollback on a closed run must fail with ErrRunClosed, got %v", err)
	}
	if err := d.CloseRun("run-9"); !errors.Is(err, impl.ErrUnknownRun) {
		t.Fatalf("closing an unknown run must fail with ErrUnknownRun, got %v", err)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	d := impl.NewDungeonCheckpoint(impl.RunConfig{MaxRuns: 2, MaxParty: 2, Checkpoints: 3, MaxGrants: 4})
	openParty(t, d, "run-1", "p1", "p2")
	advanceTo(t, d, "run-1", 1)
	advanceTo(t, d, "run-1", 2)
	if err := d.Leave("run-1", "p2"); err != nil {
		t.Fatalf("leave p2: %v", err)
	}
	if _, err := d.GrantReward("run-1", "p1", "sword", 2); err != nil {
		t.Fatalf("grant sword: %v", err)
	}
	snap, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := d.Join("run-1", "p2", 2); err != nil {
		t.Fatalf("rejoin p2 before restore: %v", err)
	}
	advanceTo(t, d, "run-1", 3)
	if err := d.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	run, ok := d.Run("run-1")
	if !ok {
		t.Fatalf("run missing after restore")
	}
	if run.Checkpoint != 2 || run.State != impl.RunActive {
		t.Fatalf("restored run = %+v, want active at checkpoint 2", run)
	}
	members := d.Members("run-1")
	if len(members) != 2 {
		t.Fatalf("party size after restore = %d, want 2", len(members))
	}
	if members[1].PlayerID != "p2" || members[1].State != impl.MemberOffline || members[1].Checkpoint != 2 {
		t.Fatalf("restored p2 = %+v, want offline at checkpoint 2", members[1])
	}
	grants := d.Grants("run-1")
	if len(grants) != 1 || grants[0].ItemID != "sword" || grants[0].Qty != 2 {
		t.Fatalf("restored ledger = %+v, want the sword grant", grants)
	}
	member, err := d.Join("run-1", "p2", 2)
	if err != nil {
		t.Fatalf("rejoin p2 after restore: %v", err)
	}
	if member.Checkpoint != 2 || member.State != impl.MemberOnline {
		t.Fatalf("p2 after restored reconnect = %+v, want online at checkpoint 2", member)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	d := impl.NewDungeonCheckpoint(dungeonConfig())
	openParty(t, d, "run-1", "p1")
	if err := d.Restore([]byte("{\"runs\": [ {\"id\":")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("truncated payload must fail with ErrBadSnapshot, got %v", err)
	}
	run, ok := d.Run("run-1")
	if !ok {
		t.Fatalf("failed restore dropped the live run")
	}
	if run.Checkpoint != 0 || len(run.Members) != 1 {
		t.Fatalf("failed restore damaged the live run: %+v", run)
	}
}

func TestConcurrentSameGrantYieldsOneEntry(t *testing.T) {
	d := impl.NewDungeonCheckpoint(impl.RunConfig{MaxRuns: 1, MaxParty: 4, Checkpoints: 3, MaxGrants: 8})
	openParty(t, d, "run-1", "p1", "p2", "p3")
	advanceTo(t, d, "run-1", 1)
	const workers = 24
	seqs := make(chan int64, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grant, err := d.GrantReward("run-1", "p1", "sword", 3)
			if err != nil {
				errs <- err
				return
			}
			seqs <- grant.Seq
		}()
	}
	wg.Wait()
	close(errs)
	close(seqs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent grant returned %v", err)
		}
	}
	grants := d.Grants("run-1")
	if len(grants) != 1 {
		t.Fatalf("ledger after concurrent identical grants = %d entries, want 1", len(grants))
	}
	seen := 0
	for seq := range seqs {
		if seq != grants[0].Seq {
			t.Fatalf("concurrent grant returned sequence %d, stored %d", seq, grants[0].Seq)
		}
		seen++
	}
	if seen != workers {
		t.Fatalf("collected %d successful grants, want %d", seen, workers)
	}
}
