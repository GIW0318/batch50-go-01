package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-13"
)

func passLimits() impl.Limits {
	return impl.Limits{
		MaxOrdersPerAccount: 3,
		MaxDays:             400,
		DailyReward:         100,
		MaxLedgerEntries:    20,
	}
}

func passBaseTime() time.Time {
	return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
}

func TestPurchaseCreatesActiveEntitlement(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	e, err := l.Purchase("acc-1", impl.SkuPass30, at)
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if e.OrderID == "" {
		t.Fatalf("a purchase must carry an order id")
	}
	if e.AccountID != "acc-1" || e.SKU != impl.SkuPass30 {
		t.Fatalf("order = %+v, want acc-1/%s", e, impl.SkuPass30)
	}
	if e.Days != 30 {
		t.Fatalf("order days = %d, want 30", e.Days)
	}
	if !e.StartAt.Equal(at) {
		t.Fatalf("order start = %v, want the purchase instant %v", e.StartAt, at)
	}
	if want := at.AddDate(0, 0, 30); !e.EndAt.Equal(want) {
		t.Fatalf("order end = %v, want %v", e.EndAt, want)
	}
	if e.Refunded {
		t.Fatalf("a fresh order must not be refunded")
	}
	state, err := l.EntitlementOf("acc-1", at)
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if !state.Active {
		t.Fatalf("the pass must be active right after the purchase")
	}
	if want := at.AddDate(0, 0, 30); !state.EndAt.Equal(want) {
		t.Fatalf("state end = %v, want %v", state.EndAt, want)
	}
	if state.Days != 30 {
		t.Fatalf("state days = %d, want 30", state.Days)
	}
	if state.ClaimedToday {
		t.Fatalf("a fresh pass must not report a claimed reward")
	}
	if state.Today != "2026-03-01" {
		t.Fatalf("state day key = %q, want 2026-03-01", state.Today)
	}
	if orders := l.Orders("acc-1"); len(orders) != 1 || orders[0].OrderID != e.OrderID {
		t.Fatalf("orders = %+v, want exactly the new order", orders)
	}
}

func TestPurchaseRejectsBlankAccount(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	if _, err := l.Purchase("   ", impl.SkuPass30, passBaseTime()); !errors.Is(err, impl.ErrBlankAccount) {
		t.Fatalf("blank account must be rejected with ErrBlankAccount, got %v", err)
	}
	if got := len(l.Orders("   ")); got != 0 {
		t.Fatalf("rejected purchases must not create orders, got %d", got)
	}
	if got := len(l.Ledger("   ")); got != 0 {
		t.Fatalf("rejected purchases must not write ledger lines, got %d", got)
	}
}

func TestPurchaseRejectsUnknownSKU(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	if _, err := l.Purchase("acc-2", "pass-weekly", passBaseTime()); !errors.Is(err, impl.ErrUnknownSKU) {
		t.Fatalf("unknown sku must be rejected with ErrUnknownSKU, got %v", err)
	}
	if got := len(l.Orders("acc-2")); got != 0 {
		t.Fatalf("rejected purchases must not create orders, got %d", got)
	}
}

func TestPurchaseEnforcesOrderLimit(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(impl.Limits{
		MaxOrdersPerAccount: 2,
		MaxDays:             400,
		DailyReward:         100,
		MaxLedgerEntries:    20,
	})
	at := passBaseTime()
	for i := 0; i < 2; i++ {
		if _, err := l.Purchase("acc-3", impl.SkuPass30, at.AddDate(0, 0, i)); err != nil {
			t.Fatalf("purchase %d must be accepted, got %v", i, err)
		}
	}
	if _, err := l.Purchase("acc-3", impl.SkuPass30, at.AddDate(0, 0, 2)); !errors.Is(err, impl.ErrOrderLimit) {
		t.Fatalf("third order must hit ErrOrderLimit, got %v", err)
	}
	if got := len(l.Orders("acc-3")); got != 2 {
		t.Fatalf("orders = %d, want the configured cap 2", got)
	}
}

func TestPurchaseEnforcesDayLimit(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(impl.Limits{
		MaxOrdersPerAccount: 5,
		MaxDays:             40,
		DailyReward:         100,
		MaxLedgerEntries:    20,
	})
	at := passBaseTime()
	if _, err := l.Purchase("acc-4", impl.SkuPass30, at); err != nil {
		t.Fatalf("first pass must be accepted, got %v", err)
	}
	if _, err := l.Purchase("acc-4", impl.SkuPass30, at); !errors.Is(err, impl.ErrDayLimit) {
		t.Fatalf("stacking past the day cap must fail with ErrDayLimit, got %v", err)
	}
	state, err := l.EntitlementOf("acc-4", at)
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if state.Days != 30 {
		t.Fatalf("state days after the rejected purchase = %d, want 30", state.Days)
	}
	if got := len(l.Orders("acc-4")); got != 1 {
		t.Fatalf("orders = %d, want only the accepted one", got)
	}
}

func TestPurchaseStacksOnActiveEntitlement(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	first, err := l.Purchase("acc-5", impl.SkuPass30, at)
	if err != nil {
		t.Fatalf("first purchase: %v", err)
	}
	renewAt := at.AddDate(0, 0, 10)
	second, err := l.Purchase("acc-5", impl.SkuPass30, renewAt)
	if err != nil {
		t.Fatalf("renewal purchase: %v", err)
	}
	if !second.StartAt.Equal(first.EndAt) {
		t.Fatalf("renewal start = %v, want it to continue at the old end %v", second.StartAt, first.EndAt)
	}
	if want := at.AddDate(0, 0, 60); !second.EndAt.Equal(want) {
		t.Fatalf("renewal end = %v, want %v", second.EndAt, want)
	}
	state, err := l.EntitlementOf("acc-5", renewAt)
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if state.Days != 60 {
		t.Fatalf("state days after the renewal = %d, want 60", state.Days)
	}
	if want := at.AddDate(0, 0, 60); !state.EndAt.Equal(want) {
		t.Fatalf("state end after the renewal = %v, want %v", state.EndAt, want)
	}
	if orders := l.Orders("acc-5"); len(orders) != 2 {
		t.Fatalf("orders = %d, want 2", len(orders))
	}
}

func TestClaimDailyGrantsOncePerUTCDay(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	if _, err := l.Purchase("acc-6", impl.SkuPass30, at); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	g, err := l.ClaimDaily("acc-6", at.Add(time.Hour))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if g.AccountID != "acc-6" {
		t.Fatalf("grant account = %q, want acc-6", g.AccountID)
	}
	if g.Day != "2026-03-01" {
		t.Fatalf("grant day = %q, want the UTC day key 2026-03-01", g.Day)
	}
	if g.MailID == "" {
		t.Fatalf("the daily reward must go through a mail, got an empty mail id")
	}
	if g.Amount != 100 {
		t.Fatalf("grant amount = %d, want the configured 100", g.Amount)
	}
	state, err := l.EntitlementOf("acc-6", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if !state.ClaimedToday {
		t.Fatalf("state must report the reward as claimed for the day")
	}
	entries := l.Ledger("acc-6")
	if len(entries) != 2 {
		t.Fatalf("ledger lines = %d, want a purchase line and a claim line", len(entries))
	}
	last := entries[len(entries)-1]
	if last.Kind != impl.LedgerClaim || last.Delta != 100 {
		t.Fatalf("last ledger line = %+v, want a 100 claim", last)
	}
}

func TestClaimDailyRejectsRepeatedSameDay(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	if _, err := l.Purchase("acc-7", impl.SkuPass30, at); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := l.ClaimDaily("acc-7", at); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := l.ClaimDaily("acc-7", at.Add(3*time.Hour)); !errors.Is(err, impl.ErrAlreadyClaimed) {
		t.Fatalf("second claim in the same UTC day must fail with ErrAlreadyClaimed, got %v", err)
	}
	nextDay := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	g, err := l.ClaimDaily("acc-7", nextDay)
	if err != nil {
		t.Fatalf("claim on the next UTC day: %v", err)
	}
	if g.Day != "2026-03-02" {
		t.Fatalf("next day grant key = %q, want 2026-03-02", g.Day)
	}
	if got := len(l.Ledger("acc-7")); got != 3 {
		t.Fatalf("ledger lines = %d, want purchase plus two claims", got)
	}
}

func TestClaimDailyRejectsWithoutEntitlement(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	if _, err := l.ClaimDaily("acc-8", at); !errors.Is(err, impl.ErrExpired) {
		t.Fatalf("claiming without a pass must fail with ErrExpired, got %v", err)
	}
	if _, err := l.Purchase("acc-8", impl.SkuPass30, at); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := l.ClaimDaily("acc-8", at.AddDate(0, 0, 31)); !errors.Is(err, impl.ErrExpired) {
		t.Fatalf("claiming after the pass ran out must fail with ErrExpired, got %v", err)
	}
}

func TestEntitlementOfReportsInactiveAfterExpiry(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	if _, err := l.Purchase("acc-9", impl.SkuPass30, at); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	fresh, err := l.EntitlementOf("acc-9", at.AddDate(0, 0, 29))
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if !fresh.Active {
		t.Fatalf("the pass must still be active one day before it ends")
	}
	stale, err := l.EntitlementOf("acc-9", at.AddDate(0, 0, 31))
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if stale.Active {
		t.Fatalf("the pass must be inactive after the end date")
	}
	if want := at.AddDate(0, 0, 30); !stale.EndAt.Equal(want) {
		t.Fatalf("state end after expiry = %v, want %v", stale.EndAt, want)
	}
}

func TestRefundReclaimsRemainingDays(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	order, err := l.Purchase("acc-10", impl.SkuPass30, at)
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	refundAt := at.AddDate(0, 0, 10)
	res, err := l.Refund("acc-10", order.OrderID, refundAt)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.OrderID != order.OrderID || res.AccountID != "acc-10" {
		t.Fatalf("refund result = %+v, want it bound to the refunded order", res)
	}
	if res.ReclaimedDays != 20 {
		t.Fatalf("reclaimed days = %d, want the 20 days left on the pass", res.ReclaimedDays)
	}
	if res.ReclaimedValue != 2000 {
		t.Fatalf("reclaimed value = %d, want 20 days at 100 each", res.ReclaimedValue)
	}
	if res.Debt != 0 {
		t.Fatalf("a whole day refund must not book debt, got %d", res.Debt)
	}
	if !res.EndAt.Equal(refundAt) {
		t.Fatalf("refund end = %v, want the refund instant %v", res.EndAt, refundAt)
	}
	state, err := l.EntitlementOf("acc-10", refundAt)
	if err != nil {
		t.Fatalf("entitlement of: %v", err)
	}
	if state.Active {
		t.Fatalf("a refunded pass must not stay active")
	}
	if orders := l.Orders("acc-10"); len(orders) != 1 || !orders[0].Refunded {
		t.Fatalf("orders after the refund = %+v, want the order flagged as refunded", orders)
	}
	if got := l.Debt("acc-10"); got != 0 {
		t.Fatalf("account debt = %d, want 0", got)
	}
	entries := l.Ledger("acc-10")
	last := entries[len(entries)-1]
	if last.Kind != impl.LedgerRefund || last.Delta != -20 {
		t.Fatalf("last ledger line = %+v, want a -20 day refund", last)
	}
}

func TestRefundRejectsUnknownOrder(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	other, err := l.Purchase("acc-11", impl.SkuPass30, at)
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := l.Refund("acc-12", other.OrderID, at); !errors.Is(err, impl.ErrUnknownOrder) {
		t.Fatalf("refunding another account's order must fail with ErrUnknownOrder, got %v", err)
	}
	if _, err := l.Refund("acc-11", "ord-404", at); !errors.Is(err, impl.ErrUnknownOrder) {
		t.Fatalf("refunding an unknown order must fail with ErrUnknownOrder, got %v", err)
	}
	if orders := l.Orders("acc-11"); len(orders) != 1 || orders[0].Refunded {
		t.Fatalf("rejected refunds must not touch the order, got %+v", orders)
	}
}

func TestRefundRejectsSecondRefund(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	order, err := l.Purchase("acc-13", impl.SkuPass30, at)
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := l.Refund("acc-13", order.OrderID, at.AddDate(0, 0, 5)); err != nil {
		t.Fatalf("first refund: %v", err)
	}
	if _, err := l.Refund("acc-13", order.OrderID, at.AddDate(0, 0, 6)); !errors.Is(err, impl.ErrAlreadyRefunded) {
		t.Fatalf("second refund of one order must fail with ErrAlreadyRefunded, got %v", err)
	}
	if res := len(l.Ledger("acc-13")); res != 2 {
		t.Fatalf("ledger lines = %d, want the purchase and the single refund", res)
	}
}

func TestSweepExpiresAccountsOnce(t *testing.T) {
	l := impl.NewSubscriptionPassLedger(passLimits())
	at := passBaseTime()
	if _, err := l.Purchase("acc-14", impl.SkuPass30, at); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	n, err := l.Sweep(at.AddDate(0, 0, 10))
	if err != nil {
		t.Fatalf("sweep while active: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep while the pass runs reported %d changes, want 0", n)
	}
	n, err = l.Sweep(at.AddDate(0, 0, 31))
	if err != nil {
		t.Fatalf("sweep after expiry: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep after the end reported %d changes, want 1", n)
	}
	n, err = l.Sweep(at.AddDate(0, 0, 32))
	if err != nil {
		t.Fatalf("second sweep after expiry: %v", err)
	}
	if n != 0 {
		t.Fatalf("the account must only be folded once, second sweep reported %d", n)
	}
	if state, _ := l.EntitlementOf("acc-14", at.AddDate(0, 0, 33)); state.Active {
		t.Fatalf("the folded pass must stay inactive")
	}
}

func TestSnapshotRestoreKeepsOrdersAndLedger(t *testing.T) {
	limits := passLimits()
	l := impl.NewSubscriptionPassLedger(limits)
	at := passBaseTime()
	if _, err := l.Purchase("acc-15", impl.SkuPass30, at); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := l.ClaimDaily("acc-15", at.Add(time.Hour)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	data, err := l.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewSubscriptionPassLedger(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if orders := restored.Orders("acc-15"); len(orders) != 1 {
		t.Fatalf("restored orders = %d, want 1", len(orders))
	}
	if entries := restored.Ledger("acc-15"); len(entries) != 2 {
		t.Fatalf("restored ledger lines = %d, want 2", len(entries))
	}
	if _, err := restored.ClaimDaily("acc-15", at.Add(2*time.Hour)); !errors.Is(err, impl.ErrAlreadyClaimed) {
		t.Fatalf("the claim mark must survive the restart, got %v", err)
	}
	renewed, err := restored.Purchase("acc-15", impl.SkuPass30, at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("purchase after restore: %v", err)
	}
	if want := at.AddDate(0, 0, 60); !renewed.EndAt.Equal(want) {
		t.Fatalf("renewal after restore ended at %v, want it stacked to %v", renewed.EndAt, want)
	}
	if err := restored.Restore([]byte("not-a-pass-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if orders := restored.Orders("acc-15"); len(orders) != 2 {
		t.Fatalf("a rejected restore must not touch live orders, got %d", len(orders))
	}
}
