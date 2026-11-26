package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-20"
)

var rerollClock = time.Unix(1700000000, 0)

func baseLimits() impl.Limits {
	return impl.Limits{MaxItems: 8, MaxLocks: 2, MaxHistory: 4, PreviewTTLMillis: 60000}
}

func newGuard() *impl.AttributeRerollGuard {
	return impl.NewAttributeRerollGuard(baseLimits(), rerollClock, 7)
}

func epicPool() impl.Pool {
	return impl.Pool{Tier: "epic", Entries: []impl.PoolEntry{
		{Name: "attack", Weight: 40, MinValue: 5, MaxValue: 9},
		{Name: "defense", Weight: 30, MinValue: 4, MaxValue: 8},
		{Name: "crit", Weight: 15, MinValue: 2, MaxValue: 6},
		{Name: "haste", Weight: 10, MinValue: 1, MaxValue: 4},
		{Name: "resist", Weight: 5, MinValue: 1, MaxValue: 3},
	}}
}

func bladeSpec() impl.ItemSpec {
	return impl.ItemSpec{ID: "blade", Tier: "epic", Entries: []impl.Entry{
		{Name: "attack", Value: 7},
		{Name: "defense", Value: 6},
		{Name: "crit", Value: 4},
	}}
}

func mustPool(t *testing.T, g *impl.AttributeRerollGuard, pool impl.Pool) {
	t.Helper()
	if err := g.SetPool(pool); err != nil {
		t.Fatalf("set pool for tier %q: %v", pool.Tier, err)
	}
}

func mustItem(t *testing.T, g *impl.AttributeRerollGuard, spec impl.ItemSpec) {
	t.Helper()
	if err := g.RegisterItem(spec); err != nil {
		t.Fatalf("register item %q: %v", spec.ID, err)
	}
}

func readyGuard(t *testing.T) *impl.AttributeRerollGuard {
	t.Helper()
	g := newGuard()
	mustPool(t, g, epicPool())
	mustItem(t, g, bladeSpec())
	if err := g.GrantShards("blade", 100); err != nil {
		t.Fatalf("grant shards: %v", err)
	}
	return g
}

func mustEntries(t *testing.T, g *impl.AttributeRerollGuard, itemID string) []impl.Entry {
	t.Helper()
	entries, err := g.Entries(itemID)
	if err != nil {
		t.Fatalf("entries of %q: %v", itemID, err)
	}
	return entries
}

func sameEntries(a, b []impl.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRegisterItemValidatesSpec(t *testing.T) {
	g := newGuard()
	mustPool(t, g, epicPool())

	if err := g.RegisterItem(impl.ItemSpec{Tier: "epic", Entries: []impl.Entry{{Name: "attack", Value: 1}}}); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("blank item id must fail with ErrBadItem, got %v", err)
	}
	if err := g.RegisterItem(impl.ItemSpec{ID: "empty", Tier: "epic"}); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("an item without entries must fail with ErrBadItem, got %v", err)
	}
	orphan := bladeSpec()
	orphan.ID = "orphan"
	orphan.Tier = "legendary"
	if err := g.RegisterItem(orphan); !errors.Is(err, impl.ErrUnknownPool) {
		t.Fatalf("an item whose tier has no pool must fail with ErrUnknownPool, got %v", err)
	}
	mustItem(t, g, bladeSpec())
	if err := g.RegisterItem(bladeSpec()); !errors.Is(err, impl.ErrItemExists) {
		t.Fatalf("duplicate item id must fail with ErrItemExists, got %v", err)
	}
}

func TestSetPoolValidatesEntries(t *testing.T) {
	g := newGuard()

	if err := g.SetPool(impl.Pool{Tier: "epic"}); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("a pool without entries must fail with ErrBadPool, got %v", err)
	}
	if err := g.SetPool(impl.Pool{Tier: "", Entries: epicPool().Entries}); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("a pool without a tier must fail with ErrBadPool, got %v", err)
	}
	zeroWeight := epicPool()
	zeroWeight.Entries[2].Weight = 0
	if err := g.SetPool(zeroWeight); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("a zero weight entry must fail with ErrBadPool, got %v", err)
	}
	badRange := epicPool()
	badRange.Entries[1].MinValue = 9
	badRange.Entries[1].MaxValue = 4
	if err := g.SetPool(badRange); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("an inverted value range must fail with ErrBadPool, got %v", err)
	}
	blankName := epicPool()
	blankName.Entries[0].Name = "  "
	if err := g.SetPool(blankName); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("a blank entry name must fail with ErrBadPool, got %v", err)
	}
	if err := g.SetPool(epicPool()); err != nil {
		t.Fatalf("a valid pool must be accepted, got %v", err)
	}
}

func TestPreviewRejectsBadIndices(t *testing.T) {
	g := readyGuard(t)

	if _, err := g.Preview("blade", []int{-1}); !errors.Is(err, impl.ErrBadLock) {
		t.Fatalf("a negative locked index must fail with ErrBadLock, got %v", err)
	}
	if _, err := g.Preview("blade", []int{3}); !errors.Is(err, impl.ErrBadLock) {
		t.Fatalf("a locked index past the last entry must fail with ErrBadLock, got %v", err)
	}
	if _, err := g.Preview("blade", []int{0, 0}); !errors.Is(err, impl.ErrBadLock) {
		t.Fatalf("a repeated locked index must fail with ErrBadLock, got %v", err)
	}
	if _, err := g.Preview("nope", nil); !errors.Is(err, impl.ErrUnknownItem) {
		t.Fatalf("previewing an unregistered item must fail with ErrUnknownItem, got %v", err)
	}
}

func TestPreviewRespectsLockLimit(t *testing.T) {
	g := readyGuard(t)

	if _, err := g.Preview("blade", []int{0, 1, 2}); !errors.Is(err, impl.ErrLockLimit) {
		t.Fatalf("three locks under a cap of two must fail with ErrLockLimit, got %v", err)
	}
	if _, err := g.Preview("blade", []int{0, 1}); err != nil {
		t.Fatalf("two locks must be accepted, got %v", err)
	}
}

func TestPreviewCostGrowsWithLocks(t *testing.T) {
	g := readyGuard(t)

	free, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview without locks: %v", err)
	}
	if free.Cost != impl.BaseRollCost {
		t.Fatalf("cost without locks = %d, want the base %d", free.Cost, impl.BaseRollCost)
	}
	if len(free.Entries) != 3 {
		t.Fatalf("preview returned %d entries, want one per current entry", len(free.Entries))
	}
	if len(free.Locked) != 0 {
		t.Fatalf("preview without locks reported locked %v", free.Locked)
	}
	locked, err := g.Preview("blade", []int{0, 2})
	if err != nil {
		t.Fatalf("preview with two locks: %v", err)
	}
	want := impl.BaseRollCost + 2*impl.LockSurcharge
	if locked.Cost != want {
		t.Fatalf("cost with two locks = %d, want %d", locked.Cost, want)
	}
	if len(locked.Locked) != 2 || locked.Locked[0] != 0 || locked.Locked[1] != 2 {
		t.Fatalf("preview reported locked %v, want [0 2]", locked.Locked)
	}
}

func TestPreviewDoesNotCommit(t *testing.T) {
	g := readyGuard(t)
	before := mustEntries(t, g, "blade")

	roll, err := g.Preview("blade", []int{1})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if roll.Seq <= 0 {
		t.Fatalf("preview sequence = %d, want a positive number", roll.Seq)
	}
	after := mustEntries(t, g, "blade")
	if !sameEntries(before, after) {
		t.Fatalf("preview replaced the entries: before %+v after %+v", before, after)
	}
	if got := len(g.History("blade")); got != 0 {
		t.Fatalf("preview wrote %d history entries, want 0", got)
	}
	if got := g.Shards("blade"); got != 100 {
		t.Fatalf("preview charged shards, balance is %d", got)
	}
}

func TestPreviewHasNoDuplicateEntries(t *testing.T) {
	g := readyGuard(t)
	roll, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range roll.Entries {
		if seen[entry.Name] {
			t.Fatalf("preview repeated the entry %q in %+v", entry.Name, roll.Entries)
		}
		seen[entry.Name] = true
	}
	if len(seen) != 3 {
		t.Fatalf("preview produced %d distinct entries, want 3", len(seen))
	}
}

func TestApplyRejectsStaleRoll(t *testing.T) {
	g := readyGuard(t)

	if _, err := g.Apply("blade", impl.Roll{ItemID: "blade"}); !errors.Is(err, impl.ErrStaleRoll) {
		t.Fatalf("applying a roll that was never previewed must fail with ErrStaleRoll, got %v", err)
	}
	if _, err := g.Apply("blade", impl.Roll{ItemID: "blade", Seq: 99}); !errors.Is(err, impl.ErrStaleRoll) {
		t.Fatalf("applying an unknown sequence must fail with ErrStaleRoll, got %v", err)
	}
	if got := g.Shards("blade"); got != 100 {
		t.Fatalf("rejected applications charged shards, balance is %d", got)
	}
}

func TestApplyRejectsInsufficientShards(t *testing.T) {
	g := newGuard()
	mustPool(t, g, epicPool())
	mustItem(t, g, bladeSpec())
	if err := g.GrantShards("blade", 5); err != nil {
		t.Fatalf("grant shards: %v", err)
	}
	roll, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	if _, err := g.Apply("blade", roll); !errors.Is(err, impl.ErrInsufficientShards) {
		t.Fatalf("applying without enough shards must fail with ErrInsufficientShards, got %v", err)
	}
	if got := g.Shards("blade"); got != 5 {
		t.Fatalf("a rejected apply changed the balance to %d, want 5", got)
	}
	if got := len(g.History("blade")); got != 0 {
		t.Fatalf("a rejected apply wrote %d history entries", got)
	}
}

func TestApplyDeductsShardsAndReplacesEntries(t *testing.T) {
	g := readyGuard(t)
	roll, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	res, err := g.Apply("blade", roll)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Cost != roll.Cost {
		t.Fatalf("charged %d shards, want the previewed cost %d", res.Cost, roll.Cost)
	}
	if res.Shards != 100-roll.Cost {
		t.Fatalf("balance after the reroll = %d, want %d", res.Shards, 100-roll.Cost)
	}
	if !sameEntries(res.Entries, roll.Entries) {
		t.Fatalf("committed entries %+v differ from the previewed %+v", res.Entries, roll.Entries)
	}
	current := mustEntries(t, g, "blade")
	if !sameEntries(current, roll.Entries) {
		t.Fatalf("item entries after the reroll = %+v, want the previewed %+v", current, roll.Entries)
	}
	hist := g.History("blade")
	if len(hist) != 1 {
		t.Fatalf("history after the reroll = %d entries, want 1", len(hist))
	}
	if hist[0].Cost != roll.Cost || !sameEntries(hist[0].Entries, roll.Entries) {
		t.Fatalf("history entry = %+v, want the committed reroll", hist[0])
	}
}

func TestApplyConsumesPreviewOnce(t *testing.T) {
	g := readyGuard(t)
	roll, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := g.Apply("blade", roll); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	if _, err := g.Apply("blade", roll); !errors.Is(err, impl.ErrStaleRoll) {
		t.Fatalf("replaying the same preview must fail with ErrStaleRoll, got %v", err)
	}
	if got := g.Shards("blade"); got != 100-roll.Cost {
		t.Fatalf("the replayed apply charged again, balance is %d", got)
	}
	if got := len(g.History("blade")); got != 1 {
		t.Fatalf("history after the replayed apply = %d entries, want 1", got)
	}
}

func TestLockReturnsProjectedCost(t *testing.T) {
	g := readyGuard(t)

	cost, err := g.Lock("blade", 0)
	if err != nil {
		t.Fatalf("lock index 0: %v", err)
	}
	if cost.Locked != 1 || cost.Amount != impl.BaseRollCost+impl.LockSurcharge {
		t.Fatalf("cost after one lock = %+v, want one lock at the surcharged price", cost)
	}
	if got := g.Locked("blade"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("locked set = %v, want [0]", got)
	}
	cost, err = g.Lock("blade", 2)
	if err != nil {
		t.Fatalf("lock index 2: %v", err)
	}
	if cost.Locked != 2 || cost.Amount != impl.BaseRollCost+2*impl.LockSurcharge {
		t.Fatalf("cost after two locks = %+v, want two locks at the surcharged price", cost)
	}
	if _, err := g.Lock("blade", 1); !errors.Is(err, impl.ErrLockLimit) {
		t.Fatalf("a third lock under a cap of two must fail with ErrLockLimit, got %v", err)
	}
	if _, err := g.Lock("blade", 0); !errors.Is(err, impl.ErrBadLock) {
		t.Fatalf("locking an already locked index must fail with ErrBadLock, got %v", err)
	}
	if got := g.Locked("blade"); len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("locked set = %v, want [0 2]", got)
	}
}

func TestUnlockAndUnlockAll(t *testing.T) {
	g := readyGuard(t)
	if _, err := g.Lock("blade", 0); err != nil {
		t.Fatalf("lock index 0: %v", err)
	}
	if err := g.Unlock("blade", 1); !errors.Is(err, impl.ErrNotLocked) {
		t.Fatalf("unlocking an index that was never locked must fail with ErrNotLocked, got %v", err)
	}
	if err := g.Unlock("blade", 0); err != nil {
		t.Fatalf("unlock index 0: %v", err)
	}
	if got := g.Locked("blade"); len(got) != 0 {
		t.Fatalf("locked set after unlock = %v, want empty", got)
	}

	if _, err := g.Lock("blade", 0); err != nil {
		t.Fatalf("lock index 0 again: %v", err)
	}
	if _, err := g.Lock("blade", 1); err != nil {
		t.Fatalf("lock index 1: %v", err)
	}
	if err := g.UnlockAll("blade"); err != nil {
		t.Fatalf("unlock all: %v", err)
	}
	if got := g.Locked("blade"); len(got) != 0 {
		t.Fatalf("locked set after unlock all = %v, want empty", got)
	}
	if err := g.Unlock("blade", 0); !errors.Is(err, impl.ErrNotLocked) {
		t.Fatalf("unlocking after a full clear must fail with ErrNotLocked, got %v", err)
	}
}

func TestSnapshotRestoreKeepsEntriesAndHistory(t *testing.T) {
	g := readyGuard(t)
	roll, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	res, err := g.Apply("blade", roll)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := g.Lock("blade", 1); err != nil {
		t.Fatalf("lock index 1: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	restored := impl.NewAttributeRerollGuard(baseLimits(), rerollClock.Add(-time.Hour), 99)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := mustEntries(t, restored, "blade"); !sameEntries(got, res.Entries) {
		t.Fatalf("restored entries = %+v, want the committed %+v", got, res.Entries)
	}
	if got := restored.Shards("blade"); got != 100-roll.Cost {
		t.Fatalf("restored balance = %d, want %d", got, 100-roll.Cost)
	}
	if got := len(restored.History("blade")); got != 1 {
		t.Fatalf("restored history = %d entries, want 1", got)
	}
	if got := restored.Locked("blade"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("restored locks = %v, want [1]", got)
	}
	next, err := restored.Preview("blade", []int{1})
	if err != nil {
		t.Fatalf("preview after restore: %v", err)
	}
	if _, err := restored.Apply("blade", next); err != nil {
		t.Fatalf("apply after restore: %v", err)
	}
	if got := len(restored.History("blade")); got != 2 {
		t.Fatalf("history after the restored reroll = %d entries, want 2", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	g := readyGuard(t)
	roll, err := g.Preview("blade", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	res, err := g.Apply("blade", roll)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if err := g.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := mustEntries(t, g, "blade"); !sameEntries(got, res.Entries) {
		t.Fatalf("a rejected restore changed the entries to %+v", got)
	}
	if got := len(g.History("blade")); got != 1 {
		t.Fatalf("a rejected restore changed the history to %d entries", got)
	}
}
