package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-07"
)

var soldAt = time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)

func ledgerLimits() impl.Limits {
	return impl.Limits{
		SlotsPerPlayer:  3,
		MaxItemsPerSell: 3,
		BuybackWindow:   10 * time.Minute,
		MaxUnitPrice:    100000,
	}
}

func mustPrice(t *testing.T, l *impl.BuybackLedger, itemID string, unit int64) {
	t.Helper()
	if err := l.SetMarketPrice(itemID, unit); err != nil {
		t.Fatalf("SetMarketPrice(%q, %d): unexpected error: %v", itemID, unit, err)
	}
}

func mustWallet(t *testing.T, l *impl.BuybackLedger, playerID string, gold int64) {
	t.Helper()
	if err := l.SetWallet(playerID, gold); err != nil {
		t.Fatalf("SetWallet(%q, %d): unexpected error: %v", playerID, gold, err)
	}
}

func mustBag(t *testing.T, l *impl.BuybackLedger, playerID string, free int) {
	t.Helper()
	if err := l.SetBagFree(playerID, free); err != nil {
		t.Fatalf("SetBagFree(%q, %d): unexpected error: %v", playerID, free, err)
	}
}

func mustSell(t *testing.T, l *impl.BuybackLedger, playerID string, items []impl.ItemRef, at time.Time) impl.SellReceipt {
	t.Helper()
	receipt, err := l.Sell(playerID, items, at)
	if err != nil {
		t.Fatalf("Sell(%q, %d items): unexpected error: %v", playerID, len(items), err)
	}
	return receipt
}

func TestSetMarketPriceRejectsNonPositivePrice(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	if err := l.SetMarketPrice("sword", 0); !errors.Is(err, impl.ErrInvalidPrice) {
		t.Fatalf("SetMarketPrice unit 0: got %v, want ErrInvalidPrice", err)
	}
	if err := l.SetMarketPrice("sword", -5); !errors.Is(err, impl.ErrInvalidPrice) {
		t.Fatalf("SetMarketPrice unit -5: got %v, want ErrInvalidPrice", err)
	}
}

func TestSellRejectsEmptyItemList(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	if _, err := l.Sell("alice", nil, soldAt); !errors.Is(err, impl.ErrEmptySell) {
		t.Fatalf("Sell(nil): got %v, want ErrEmptySell", err)
	}
	if _, err := l.Sell("alice", []impl.ItemRef{}, soldAt); !errors.Is(err, impl.ErrEmptySell) {
		t.Fatalf("Sell(empty): got %v, want ErrEmptySell", err)
	}
}

func TestSellRejectsBoundItem(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 100)
	items := []impl.ItemRef{{ItemID: "sword", Count: 1}, {ItemID: "sword", Count: 1, Bound: true}}
	if _, err := l.Sell("alice", items, soldAt); !errors.Is(err, impl.ErrBoundItem) {
		t.Fatalf("Sell with bound item: got %v, want ErrBoundItem", err)
	}
	used, err := l.SlotsUsed("alice")
	if err != nil {
		t.Fatalf("SlotsUsed: unexpected error: %v", err)
	}
	if used != 0 {
		t.Fatalf("slots after rejected sell: got %d, want 0", used)
	}
}

func TestSellRejectsTooManyItems(t *testing.T) {
	limits := ledgerLimits()
	limits.MaxItemsPerSell = 2
	l := impl.NewBuybackLedger(limits)
	mustPrice(t, l, "sword", 100)
	items := []impl.ItemRef{
		{ItemID: "sword", Count: 1},
		{ItemID: "sword", Count: 1},
		{ItemID: "sword", Count: 1},
	}
	if _, err := l.Sell("alice", items, soldAt); !errors.Is(err, impl.ErrTooManyItems) {
		t.Fatalf("Sell with 3 items under cap 2: got %v, want ErrTooManyItems", err)
	}
}

func TestSellRejectsNonPositiveCount(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 100)
	if _, err := l.Sell("alice", []impl.ItemRef{{ItemID: "sword", Count: 0}}, soldAt); !errors.Is(err, impl.ErrInvalidCount) {
		t.Fatalf("Sell count 0: got %v, want ErrInvalidCount", err)
	}
	if _, err := l.Sell("alice", []impl.ItemRef{{ItemID: "sword", Count: -1}}, soldAt); !errors.Is(err, impl.ErrInvalidCount) {
		t.Fatalf("Sell count -1: got %v, want ErrInvalidCount", err)
	}
}

func TestSellPricesBatchFromMarketSnapshot(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 120)
	receipt := mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 3, Enhance: 2}}, soldAt)
	if receipt.Total != 360 {
		t.Fatalf("receipt total: got %d, want 360", receipt.Total)
	}
	if len(receipt.Slots) != 1 || receipt.Slots[0] != 0 {
		t.Fatalf("receipt slots: got %v, want [0]", receipt.Slots)
	}
	mustPrice(t, l, "sword", 999)
	quote, err := l.Quote("alice", 0)
	if err != nil {
		t.Fatalf("Quote(slot 0): unexpected error: %v", err)
	}
	if quote.Price != 360 {
		t.Fatalf("quote price after market move: got %d, want 360", quote.Price)
	}
	if quote.Item.Enhance != 2 || quote.Item.Count != 3 {
		t.Fatalf("quote item: got %+v, want count 3 enhance 2", quote.Item)
	}
}

func TestSellEvictsOldestBatchWhenSlotsFull(t *testing.T) {
	limits := ledgerLimits()
	limits.SlotsPerPlayer = 2
	l := impl.NewBuybackLedger(limits)
	for _, id := range []string{"item-a", "item-b", "item-c"} {
		mustPrice(t, l, id, 50)
	}
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "item-a", Count: 1}}, soldAt)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "item-b", Count: 1}}, soldAt.Add(time.Second))
	receipt := mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "item-c", Count: 1}}, soldAt.Add(2*time.Second))
	if len(receipt.Replaced) != 1 || receipt.Replaced[0].Item.ItemID != "item-a" {
		t.Fatalf("replaced batches: got %+v, want the item-a batch", receipt.Replaced)
	}
	if len(receipt.Slots) != 1 || receipt.Slots[0] != 0 {
		t.Fatalf("reused slot: got %v, want [0]", receipt.Slots)
	}
	entries, err := l.List("alice")
	if err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries after eviction: got %d, want 2", len(entries))
	}
	if entries[0].Item.ItemID != "item-c" || entries[1].Item.ItemID != "item-b" {
		t.Fatalf("entries after eviction: got %q at 0 and %q at 1, want item-c and item-b", entries[0].Item.ItemID, entries[1].Item.ItemID)
	}
}

func TestQuoteReturnsSlotDetails(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 100)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 2}}, soldAt)
	quote, err := l.Quote("alice", 0)
	if err != nil {
		t.Fatalf("Quote: unexpected error: %v", err)
	}
	if !quote.Valid || quote.Price != 200 || quote.Slot != 0 {
		t.Fatalf("quote: got %+v, want valid slot 0 priced 200", quote)
	}
	wantExpiry := soldAt.Add(10 * time.Minute)
	if !quote.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("quote expiry: got %v, want %v", quote.ExpiresAt, wantExpiry)
	}
}

func TestQuoteAndBuyBackRejectBadSlots(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	if _, err := l.Quote("alice", 0); !errors.Is(err, impl.ErrSlotEmpty) {
		t.Fatalf("Quote on empty slot: got %v, want ErrSlotEmpty", err)
	}
	if _, err := l.Quote("alice", 9); !errors.Is(err, impl.ErrInvalidSlot) {
		t.Fatalf("Quote slot 9: got %v, want ErrInvalidSlot", err)
	}
	if _, err := l.BuyBack("alice", -1, soldAt); !errors.Is(err, impl.ErrInvalidSlot) {
		t.Fatalf("BuyBack slot -1: got %v, want ErrInvalidSlot", err)
	}
}

func TestBuyBackDeductsGoldAndReleasesSlot(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 120)
	mustWallet(t, l, "alice", 1000)
	mustBag(t, l, "alice", 10)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 3}}, soldAt)
	item, err := l.BuyBack("alice", 0, soldAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("BuyBack: unexpected error: %v", err)
	}
	if item.ItemID != "sword" || item.Count != 3 {
		t.Fatalf("bought back item: got %+v, want sword x3", item)
	}
	if got := l.Wallet("alice"); got != 640 {
		t.Fatalf("wallet after buyback: got %d, want 640", got)
	}
	if got := l.Spent("alice"); got != 360 {
		t.Fatalf("spent after buyback: got %d, want 360", got)
	}
	if got := l.BagFree("alice"); got != 9 {
		t.Fatalf("bag free after buyback: got %d, want 9", got)
	}
	used, err := l.SlotsUsed("alice")
	if err != nil {
		t.Fatalf("SlotsUsed: unexpected error: %v", err)
	}
	if used != 0 {
		t.Fatalf("slots after buyback: got %d, want 0", used)
	}
}

func TestBuyBackRejectsInsufficientGold(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 120)
	mustWallet(t, l, "alice", 100)
	mustBag(t, l, "alice", 10)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 3}}, soldAt)
	if _, err := l.BuyBack("alice", 0, soldAt.Add(time.Minute)); !errors.Is(err, impl.ErrInsufficientGold) {
		t.Fatalf("BuyBack without gold: got %v, want ErrInsufficientGold", err)
	}
	if got := l.Wallet("alice"); got != 100 {
		t.Fatalf("wallet after rejected buyback: got %d, want 100", got)
	}
	used, err := l.SlotsUsed("alice")
	if err != nil {
		t.Fatalf("SlotsUsed: unexpected error: %v", err)
	}
	if used != 1 {
		t.Fatalf("slots after rejected buyback: got %d, want 1", used)
	}
}

func TestExpirePurgesClosedWindowsAndIsIdempotent(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 100)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 1}}, soldAt)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 1}}, soldAt.Add(2*time.Minute))
	purged, err := l.Expire(soldAt.Add(11 * time.Minute))
	if err != nil {
		t.Fatalf("Expire: unexpected error: %v", err)
	}
	if purged != 1 {
		t.Fatalf("first Expire: got %d, want 1", purged)
	}
	purged, err = l.Expire(soldAt.Add(11*time.Minute + 30*time.Second))
	if err != nil {
		t.Fatalf("second Expire: unexpected error: %v", err)
	}
	if purged != 0 {
		t.Fatalf("second Expire: got %d, want 0", purged)
	}
	used, err := l.SlotsUsed("alice")
	if err != nil {
		t.Fatalf("SlotsUsed: unexpected error: %v", err)
	}
	if used != 1 {
		t.Fatalf("slots after expire: got %d, want 1", used)
	}
}

func TestListSortsEntriesBySlot(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	for _, id := range []string{"item-a", "item-b", "item-c"} {
		mustPrice(t, l, id, 10)
	}
	mustSell(t, l, "alice", []impl.ItemRef{
		{ItemID: "item-a", Count: 1},
		{ItemID: "item-b", Count: 1},
		{ItemID: "item-c", Count: 1},
	}, soldAt)
	entries, err := l.List("alice")
	if err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries: got %d, want 3", len(entries))
	}
	for i, want := range []string{"item-a", "item-b", "item-c"} {
		if entries[i].Slot != i || entries[i].Item.ItemID != want {
			t.Fatalf("entry %d: got slot %d item %q, want slot %d item %q", i, entries[i].Slot, entries[i].Item.ItemID, i, want)
		}
	}
}

func TestSnapshotRestoreKeepsSlotsWalletAndBag(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 120)
	mustWallet(t, l, "alice", 1000)
	mustBag(t, l, "alice", 10)
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 3}}, soldAt)
	if _, err := l.BuyBack("alice", 0, soldAt.Add(time.Minute)); err != nil {
		t.Fatalf("BuyBack: unexpected error: %v", err)
	}
	mustSell(t, l, "alice", []impl.ItemRef{{ItemID: "sword", Count: 1}}, soldAt.Add(2*time.Minute))
	data, err := l.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Snapshot: got empty payload")
	}
	b := impl.NewBuybackLedger(ledgerLimits())
	if err := b.Restore(data); err != nil {
		t.Fatalf("Restore: unexpected error: %v", err)
	}
	if got := b.Wallet("alice"); got != 640 {
		t.Fatalf("wallet after restore: got %d, want 640", got)
	}
	if got := b.Spent("alice"); got != 360 {
		t.Fatalf("spent after restore: got %d, want 360", got)
	}
	if got := b.BagFree("alice"); got != 9 {
		t.Fatalf("bag free after restore: got %d, want 9", got)
	}
	quote, err := b.Quote("alice", 0)
	if err != nil {
		t.Fatalf("Quote after restore: unexpected error: %v", err)
	}
	if !quote.Valid || quote.Price != 120 || quote.Item.Count != 1 {
		t.Fatalf("quote after restore: got %+v, want valid price 120 count 1", quote)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	l := impl.NewBuybackLedger(ledgerLimits())
	mustPrice(t, l, "sword", 100)
	mustWallet(t, l, "alice", 500)
	if err := l.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore malformed payload: got %v, want ErrBadSnapshot", err)
	}
	if err := l.Restore(nil); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore empty payload: got %v, want ErrBadSnapshot", err)
	}
	if got := l.Wallet("alice"); got != 500 {
		t.Fatalf("wallet after rejected Restore: got %d, want 500", got)
	}
}
