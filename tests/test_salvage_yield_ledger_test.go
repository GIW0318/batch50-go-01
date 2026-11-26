package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-a-24"
)

func newSalvageLedger(t *testing.T) *impl.SalvageYieldLedger {
	t.Helper()
	l := impl.NewSalvageYieldLedger(impl.Limits{MaxPlayers: 4, MaxBatch: 6, MaxHistory: 8})
	base := impl.Table{Plus: 0, Materials: []impl.Yield{
		{Material: "ore", Count: 2},
		{Material: "dust", Count: 1},
	}}
	if err := l.SetTable("rare", base); err != nil {
		t.Fatalf("registering the base table must succeed, got %v", err)
	}
	plusFive := impl.Table{Plus: 5, Materials: []impl.Yield{{Material: "ore", Count: 3}}}
	if err := l.SetTable("rare", plusFive); err != nil {
		t.Fatalf("registering the plus five table must succeed, got %v", err)
	}
	return l
}

func rareSword(id string) impl.ItemRef {
	return impl.ItemRef{ID: id, Tier: "rare", Plus: 5}
}

func rareAxe(id string) impl.ItemRef {
	return impl.ItemRef{ID: id, Tier: "rare", Plus: 0}
}

func TestSetTableThenYieldOf(t *testing.T) {
	l := newSalvageLedger(t)
	yields, err := l.YieldOf(rareAxe("axe-1"))
	if err != nil {
		t.Fatalf("resolving the base table must succeed, got %v", err)
	}
	if len(yields) != 2 {
		t.Fatalf("yield count = %d, want 2 materials", len(yields))
	}
	if yields[0].Material != "dust" || yields[0].Count != 1 {
		t.Fatalf("first yield = %+v, want dust x1 in ascending material order", yields[0])
	}
	if yields[1].Material != "ore" || yields[1].Count != 2 {
		t.Fatalf("second yield = %+v, want ore x2", yields[1])
	}
	high, err := l.YieldOf(rareSword("sword-1"))
	if err != nil {
		t.Fatalf("resolving the plus five table must succeed, got %v", err)
	}
	if len(high) != 1 || high[0].Material != "ore" || high[0].Count != 3 {
		t.Fatalf("plus five yield = %+v, want ore x3", high)
	}
}

func TestYieldOfUsesHighestDefinedPlusBelow(t *testing.T) {
	l := newSalvageLedger(t)
	yields, err := l.YieldOf(impl.ItemRef{ID: "helm-1", Tier: "rare", Plus: 3})
	if err != nil {
		t.Fatalf("resolving an intermediate plus must succeed, got %v", err)
	}
	if len(yields) != 2 || yields[1].Count != 2 {
		t.Fatalf("plus three yield = %+v, want the base table with ore x2", yields)
	}
}

func TestYieldOfRejectsUnknownTier(t *testing.T) {
	l := newSalvageLedger(t)
	if _, err := l.YieldOf(impl.ItemRef{ID: "wand-1", Tier: "epic"}); !errors.Is(err, impl.ErrUnknownTier) {
		t.Fatalf("an unconfigured tier must report ErrUnknownTier, got %v", err)
	}
	if _, err := l.YieldOf(impl.ItemRef{ID: "", Tier: "rare"}); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("a blank item id must report ErrBadItem, got %v", err)
	}
}

func TestSetTableRejectsInvalidDefinition(t *testing.T) {
	l := newSalvageLedger(t)
	duplicate := impl.Table{Plus: 0, Materials: []impl.Yield{{Material: "ore", Count: 1}}}
	if err := l.SetTable("rare", duplicate); !errors.Is(err, impl.ErrBadTable) {
		t.Fatalf("a duplicate plus level must report ErrBadTable, got %v", err)
	}
	negativePlus := impl.Table{Plus: -1, Materials: []impl.Yield{{Material: "ore", Count: 1}}}
	if err := l.SetTable("rare", negativePlus); !errors.Is(err, impl.ErrBadTable) {
		t.Fatalf("a negative plus level must report ErrBadTable, got %v", err)
	}
	zeroCount := impl.Table{Plus: 9, Materials: []impl.Yield{{Material: "ore", Count: 0}}}
	if err := l.SetTable("rare", zeroCount); !errors.Is(err, impl.ErrBadTable) {
		t.Fatalf("a zero material count must report ErrBadTable, got %v", err)
	}
	if err := l.SetTable("", duplicate); !errors.Is(err, impl.ErrBadTable) {
		t.Fatalf("a blank tier must report ErrBadTable, got %v", err)
	}
}

func TestPreviewDoesNotRecord(t *testing.T) {
	l := newSalvageLedger(t)
	items := []impl.ItemRef{rareSword("sword-1"), rareSword("sword-2")}
	yields, err := l.Preview(items)
	if err != nil {
		t.Fatalf("preview must succeed, got %v", err)
	}
	if len(yields) != 1 || yields[0].Material != "ore" || yields[0].Count != 6 {
		t.Fatalf("preview yield = %+v, want ore x6 summed per item", yields)
	}
	if got := len(l.History("player-1")); got != 0 {
		t.Fatalf("preview left %d history entries behind", got)
	}
	if l.AlreadySalvaged("sword-1") {
		t.Fatalf("preview must not mark an item as salvaged")
	}
	if got := l.Bag("player-1")["ore"]; got != 0 {
		t.Fatalf("preview put %d ore into the bag", got)
	}
	result, err := l.Salvage("player-1", items)
	if err != nil {
		t.Fatalf("salvaging after a preview must succeed, got %v", err)
	}
	if result.Yields[0].Count != 6 {
		t.Fatalf("salvaged amount = %d, want 6 after the preview", result.Yields[0].Count)
	}
}

func TestSalvageRecordsHistoryAndBag(t *testing.T) {
	l := newSalvageLedger(t)
	result, err := l.Salvage("player-1", []impl.ItemRef{rareSword("sword-1"), rareAxe("axe-1")})
	if err != nil {
		t.Fatalf("salvaging two items must succeed, got %v", err)
	}
	if result.Player != "player-1" || result.Items != 2 {
		t.Fatalf("batch result = %+v, want two items for player-1", result)
	}
	if result.Seq <= 0 {
		t.Fatalf("batch sequence = %d, want a positive number", result.Seq)
	}
	if len(result.Yields) != 2 {
		t.Fatalf("batch yields = %+v, want dust and ore", result.Yields)
	}
	if result.Yields[0].Material != "dust" || result.Yields[0].Count != 1 {
		t.Fatalf("first batch yield = %+v, want dust x1", result.Yields[0])
	}
	if result.Yields[1].Material != "ore" || result.Yields[1].Count != 5 {
		t.Fatalf("second batch yield = %+v, want ore x5", result.Yields[1])
	}
	bag := l.Bag("player-1")
	if bag["ore"] != 5 || bag["dust"] != 1 {
		t.Fatalf("bag = %+v, want ore 5 and dust 1", bag)
	}
	history := l.History("player-1")
	if len(history) != 1 {
		t.Fatalf("history length = %d, want 1", len(history))
	}
	if len(history[0].Items) != 2 || history[0].Items[0] != "sword-1" || history[0].Items[1] != "axe-1" {
		t.Fatalf("history items = %v, want the batch in submission order", history[0].Items)
	}
	if !l.AlreadySalvaged("sword-1") || !l.AlreadySalvaged("axe-1") {
		t.Fatalf("both items must be marked as salvaged")
	}
}

func TestSalvageRejectsBoundItem(t *testing.T) {
	l := newSalvageLedger(t)
	bound := rareSword("bound-1")
	bound.Bound = true
	if _, err := l.Salvage("player-1", []impl.ItemRef{bound}); !errors.Is(err, impl.ErrBoundItem) {
		t.Fatalf("a bound item must report ErrBoundItem, got %v", err)
	}
	if got := len(l.History("player-1")); got != 0 {
		t.Fatalf("a rejected bound item left %d history entries", got)
	}
}

func TestSalvageRejectsQuestItem(t *testing.T) {
	l := newSalvageLedger(t)
	quest := rareAxe("quest-1")
	quest.Quest = true
	if _, err := l.Salvage("player-1", []impl.ItemRef{quest}); !errors.Is(err, impl.ErrQuestItem) {
		t.Fatalf("a quest item must report ErrQuestItem, got %v", err)
	}
	if got := l.Bag("player-1")["ore"]; got != 0 {
		t.Fatalf("a rejected quest item produced %d ore", got)
	}
}

func TestSalvageRejectsEmptyBatch(t *testing.T) {
	l := newSalvageLedger(t)
	if _, err := l.Salvage("player-1", nil); !errors.Is(err, impl.ErrEmptyBatch) {
		t.Fatalf("an empty batch must report ErrEmptyBatch, got %v", err)
	}
	if _, err := l.Preview(nil); !errors.Is(err, impl.ErrEmptyBatch) {
		t.Fatalf("an empty preview must report ErrEmptyBatch, got %v", err)
	}
	if _, err := l.Salvage("", []impl.ItemRef{rareSword("sword-1")}); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("a blank player must report ErrBadPlayer, got %v", err)
	}
}

func TestSalvageRejectsUnknownTier(t *testing.T) {
	l := newSalvageLedger(t)
	if _, err := l.Salvage("player-1", []impl.ItemRef{{ID: "wand-1", Tier: "epic"}}); !errors.Is(err, impl.ErrUnknownTier) {
		t.Fatalf("an unconfigured tier must report ErrUnknownTier, got %v", err)
	}
	if got := len(l.History("player-1")); got != 0 {
		t.Fatalf("a rejected tier left %d history entries", got)
	}
}

func TestBatchLimitRejectsOversizeBatch(t *testing.T) {
	l := newSalvageLedger(t)
	if err := l.SetBatchLimit(2); err != nil {
		t.Fatalf("setting the batch limit must succeed, got %v", err)
	}
	batch := []impl.ItemRef{rareSword("sword-1"), rareSword("sword-2"), rareSword("sword-3")}
	if _, err := l.Salvage("player-1", batch); !errors.Is(err, impl.ErrBatchTooLarge) {
		t.Fatalf("a batch above the limit must report ErrBatchTooLarge, got %v", err)
	}
	if got := len(l.History("player-1")); got != 0 {
		t.Fatalf("a rejected batch left %d history entries", got)
	}
	result, err := l.Salvage("player-1", batch[:2])
	if err != nil {
		t.Fatalf("a batch at the limit must be accepted, got %v", err)
	}
	if result.Items != 2 {
		t.Fatalf("accepted batch size = %d, want 2", result.Items)
	}
}

func TestBagIsolatedPerPlayer(t *testing.T) {
	l := newSalvageLedger(t)
	if _, err := l.Salvage("player-1", []impl.ItemRef{rareSword("sword-1")}); err != nil {
		t.Fatalf("the first salvage must succeed, got %v", err)
	}
	if got := l.Bag("player-1")["ore"]; got != 3 {
		t.Fatalf("player-1 ore = %d, want 3", got)
	}
	if got := len(l.Bag("player-2")); got != 0 {
		t.Fatalf("player-2 bag has %d materials, want none", got)
	}
	if got := len(l.History("player-2")); got != 0 {
		t.Fatalf("player-2 history length = %d, want 0", got)
	}
	if !l.AlreadySalvaged("sword-1") {
		t.Fatalf("the salvaged set must be global across players")
	}
}

func TestSetBatchLimitRejectsNonPositive(t *testing.T) {
	l := newSalvageLedger(t)
	if err := l.SetBatchLimit(0); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("a zero batch limit must report ErrBadLimit, got %v", err)
	}
	if err := l.SetBatchLimit(-3); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("a negative batch limit must report ErrBadLimit, got %v", err)
	}
	if err := l.SetBagCapacity(-1); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("a negative bag capacity must report ErrBadLimit, got %v", err)
	}
	if err := l.SetBatchLimit(4); err != nil {
		t.Fatalf("a valid batch limit must be accepted, got %v", err)
	}
}

func TestSnapshotRestoreKeepsHistoryAndBag(t *testing.T) {
	l := newSalvageLedger(t)
	if _, err := l.Salvage("player-1", []impl.ItemRef{rareSword("sword-1")}); err != nil {
		t.Fatalf("the first salvage must succeed, got %v", err)
	}
	data, err := l.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewSalvageYieldLedger(impl.Limits{MaxPlayers: 4, MaxBatch: 6, MaxHistory: 8})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	if got := restored.Bag("player-1")["ore"]; got != 3 {
		t.Fatalf("restored ore = %d, want 3", got)
	}
	if got := len(restored.History("player-1")); got != 1 {
		t.Fatalf("restored history length = %d, want 1", got)
	}
	if !restored.AlreadySalvaged("sword-1") {
		t.Fatalf("the salvaged set must survive a restart")
	}
	before := restored.History("player-1")[0].Seq
	result, err := restored.Salvage("player-1", []impl.ItemRef{rareAxe("axe-1")})
	if err != nil {
		t.Fatalf("salvaging after restore must succeed, got %v", err)
	}
	if result.Seq <= before {
		t.Fatalf("batch sequence after restore = %d, want it above the restored %d", result.Seq, before)
	}
	if got := len(restored.History("player-1")); got != 2 {
		t.Fatalf("history length after restore = %d, want 2", got)
	}
	if got := restored.Bag("player-1")["ore"]; got != 5 {
		t.Fatalf("ore after restore = %d, want 5", got)
	}
}

func TestRestoreRejectsGarbageSnapshot(t *testing.T) {
	l := newSalvageLedger(t)
	if _, err := l.Salvage("player-1", []impl.ItemRef{rareSword("sword-1")}); err != nil {
		t.Fatalf("the first salvage must succeed, got %v", err)
	}
	if err := l.Restore([]byte("}}} not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a garbage snapshot must report ErrBadSnapshot, got %v", err)
	}
	if got := l.Bag("player-1")["ore"]; got != 3 {
		t.Fatalf("bag after a failed restore = %d ore, want 3", got)
	}
	if got := len(l.History("player-1")); got != 1 {
		t.Fatalf("history after a failed restore = %d entries, want 1", got)
	}
}
