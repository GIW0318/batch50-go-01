package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-22"
)

func baseMoment() time.Time {
	return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
}

func newEarningsLedger(t *testing.T) *impl.OfflineEarningsLedger {
	t.Helper()
	l := impl.NewOfflineEarningsLedger(impl.Limits{MaxPlayers: 4, MaxBonuses: 4})
	if err := l.SetCap(8 * time.Hour); err != nil {
		t.Fatalf("setting the offline cap must succeed, got %v", err)
	}
	return l
}

func TestSettleAccruesAtBaseRate(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	report, err := l.Settle("player-1", base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settling two hours must succeed, got %v", err)
	}
	if report.From != base || report.To != base.Add(2*time.Hour) {
		t.Fatalf("settle window = %v..%v, want %v..%v", report.From, report.To, base, base.Add(2*time.Hour))
	}
	if report.Seconds != 7200 {
		t.Fatalf("settled seconds = %d, want 7200", report.Seconds)
	}
	if report.Amount != 200 {
		t.Fatalf("settled amount = %d, want 200", report.Amount)
	}
	if report.Capped {
		t.Fatalf("a two hour window below the eight hour cap must not be capped")
	}
	pending, ok := l.Pending("player-1")
	if !ok {
		t.Fatalf("a settlement must leave a pending entry")
	}
	if pending.Amount != 200 {
		t.Fatalf("pending entry = %+v, want 200 units", pending)
	}
	if got := l.Bank("player-1"); got != 0 {
		t.Fatalf("bank after a settlement = %d, want 0 before the claim", got)
	}
}

func TestSettleTruncatesToCap(t *testing.T) {
	l := newEarningsLedger(t)
	if err := l.SetCap(4 * time.Hour); err != nil {
		t.Fatalf("setting the offline cap must succeed, got %v", err)
	}
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	report, err := l.Settle("player-1", base.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("settling ten hours must succeed, got %v", err)
	}
	if report.Seconds != 14400 {
		t.Fatalf("settled seconds = %d, want the four hour cap in seconds", report.Seconds)
	}
	if report.Amount != 400 {
		t.Fatalf("settled amount = %d, want 400 under the cap", report.Amount)
	}
	if !report.Capped {
		t.Fatalf("a ten hour window above the cap must be reported as capped")
	}
	if report.To != base.Add(10*time.Hour) {
		t.Fatalf("report end = %v, want the settle moment %v", report.To, base.Add(10*time.Hour))
	}
}

func TestSettleRejectsEarlierMoment(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	if _, err := l.Settle("player-1", base.Add(-time.Second)); !errors.Is(err, impl.ErrBadTime) {
		t.Fatalf("settling before the last online moment must report ErrBadTime, got %v", err)
	}
	report, err := l.Settle("player-1", base)
	if err != nil {
		t.Fatalf("settling at the last online moment must succeed, got %v", err)
	}
	if report.Seconds != 0 || report.Amount != 0 {
		t.Fatalf("zero length window settled %d units over %d seconds, want nothing", report.Amount, report.Seconds)
	}
	if report.Capped {
		t.Fatalf("a zero length window must not be reported as capped")
	}
}

func TestPreviewDoesNotAccruePending(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	preview, err := l.Preview("player-1", base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("preview must succeed, got %v", err)
	}
	if preview.Amount != 200 {
		t.Fatalf("preview = %+v, want 200 units", preview)
	}
	if pending, ok := l.Pending("player-1"); ok && pending.Amount != 0 {
		t.Fatalf("preview created a pending entry of %d units", pending.Amount)
	}
	again, err := l.Preview("player-1", base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second preview must succeed, got %v", err)
	}
	if again.Amount != preview.Amount {
		t.Fatalf("second preview = %d, want the same %d", again.Amount, preview.Amount)
	}
	report, err := l.Settle("player-1", base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settling after previews must succeed, got %v", err)
	}
	if report.Amount != 200 {
		t.Fatalf("settled amount = %d after previews, want 200", report.Amount)
	}
}

func TestPreviewRejectsUnknownPlayer(t *testing.T) {
	l := newEarningsLedger(t)
	if _, err := l.Preview("ghost", baseMoment()); !errors.Is(err, impl.ErrUnknownPlayer) {
		t.Fatalf("preview for an unknown player must report ErrUnknownPlayer, got %v", err)
	}
	if _, err := l.Preview("", baseMoment()); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("preview with a blank player must report ErrBadPlayer, got %v", err)
	}
}

func TestClaimTransfersPendingOnce(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	if _, err := l.Settle("player-1", base.Add(2*time.Hour)); err != nil {
		t.Fatalf("settling two hours must succeed, got %v", err)
	}
	claimed, err := l.Claim("player-1")
	if err != nil {
		t.Fatalf("claiming must succeed, got %v", err)
	}
	if claimed.Amount != 200 {
		t.Fatalf("claimed amount = %d, want 200", claimed.Amount)
	}
	if got := l.Bank("player-1"); got != 200 {
		t.Fatalf("bank after the claim = %d, want 200", got)
	}
	if pending, _ := l.Pending("player-1"); pending.Amount != 0 {
		t.Fatalf("pending after the claim = %d, want 0", pending.Amount)
	}
	repeat, err := l.Claim("player-1")
	if err != nil {
		t.Fatalf("a repeated claim must be idempotent, got %v", err)
	}
	if repeat.Amount != 0 {
		t.Fatalf("repeated claim returned %d units, want 0", repeat.Amount)
	}
	if got := l.Bank("player-1"); got != 200 {
		t.Fatalf("bank after the repeated claim = %d, want 200", got)
	}
}

func TestClaimWithoutSettleIsEmpty(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	claimed, err := l.Claim("player-1")
	if err != nil {
		t.Fatalf("claiming without a settlement must not fail, got %v", err)
	}
	if claimed.Amount != 0 {
		t.Fatalf("claim without a settlement returned %d units, want 0", claimed.Amount)
	}
	if got := l.Bank("player-1"); got != 0 {
		t.Fatalf("bank = %d after an empty claim, want 0", got)
	}
}

func TestSameKindBonusKeepsHighest(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	rate := impl.Rate{Base: 100, Bonuses: []impl.Bonus{
		{Kind: "vip", Percent: 20},
		{Kind: "vip", Percent: 50},
	}}
	if err := l.SetRate("player-1", rate); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	report, err := l.Settle("player-1", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("settling one hour must succeed, got %v", err)
	}
	if report.Amount != 150 {
		t.Fatalf("settled amount = %d, want 150 with the highest vip bonus only", report.Amount)
	}
}

func TestDifferentBonusKindsMultiply(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	rate := impl.Rate{Base: 100, Bonuses: []impl.Bonus{
		{Kind: "vip", Percent: 50},
		{Kind: "event", Percent: 20},
	}}
	if err := l.SetRate("player-1", rate); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	report, err := l.Settle("player-1", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("settling one hour must succeed, got %v", err)
	}
	if report.Amount != 180 {
		t.Fatalf("settled amount = %d, want 180 with vip and event multiplied", report.Amount)
	}
}

func TestSetRateRejectsInvalidRate(t *testing.T) {
	l := newEarningsLedger(t)
	if err := l.SetRate("player-1", impl.Rate{Base: 0}); !errors.Is(err, impl.ErrBadRate) {
		t.Fatalf("zero base rate must report ErrBadRate, got %v", err)
	}
	negative := impl.Rate{Base: 100, Bonuses: []impl.Bonus{{Kind: "vip", Percent: -5}}}
	if err := l.SetRate("player-1", negative); !errors.Is(err, impl.ErrBadRate) {
		t.Fatalf("a negative bonus must report ErrBadRate, got %v", err)
	}
	if err := l.SetRate("", impl.Rate{Base: 100}); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("a blank player must report ErrBadPlayer, got %v", err)
	}
}

func TestSetCapRejectsNonPositive(t *testing.T) {
	l := newEarningsLedger(t)
	if err := l.SetCap(0); !errors.Is(err, impl.ErrBadCap) {
		t.Fatalf("a zero cap must report ErrBadCap, got %v", err)
	}
	if err := l.SetCap(-time.Hour); !errors.Is(err, impl.ErrBadCap) {
		t.Fatalf("a negative cap must report ErrBadCap, got %v", err)
	}
	if err := l.SetCap(6 * time.Hour); err != nil {
		t.Fatalf("a valid cap must be accepted, got %v", err)
	}
}

func TestRecordRejectsBlankPlayer(t *testing.T) {
	l := newEarningsLedger(t)
	if err := l.Record("", baseMoment()); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("recording a blank player must report ErrBadPlayer, got %v", err)
	}
	if err := l.Record("player-1", baseMoment()); err != nil {
		t.Fatalf("recording a valid player must succeed, got %v", err)
	}
	if _, err := l.Settle("ghost", baseMoment().Add(time.Hour)); !errors.Is(err, impl.ErrUnknownPlayer) {
		t.Fatalf("settling an unknown player must report ErrUnknownPlayer, got %v", err)
	}
}

func TestPlayerLimitEnforced(t *testing.T) {
	l := impl.NewOfflineEarningsLedger(impl.Limits{MaxPlayers: 2, MaxBonuses: 4})
	if err := l.SetCap(8 * time.Hour); err != nil {
		t.Fatalf("setting the offline cap must succeed, got %v", err)
	}
	base := baseMoment()
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("first player must be accepted, got %v", err)
	}
	if err := l.Record("player-2", base); err != nil {
		t.Fatalf("second player must be accepted, got %v", err)
	}
	if err := l.Record("player-3", base); !errors.Is(err, impl.ErrLimit) {
		t.Fatalf("third player must report ErrLimit, got %v", err)
	}
	if _, ok := l.Pending("player-3"); ok {
		t.Fatalf("a player rejected by the cap must not be recorded")
	}
}

func TestSnapshotRestoreKeepsBaselineAndPending(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the first rate must succeed, got %v", err)
	}
	if err := l.SetRate("player-2", impl.Rate{Base: 50}); err != nil {
		t.Fatalf("setting the second rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording player-1 must succeed, got %v", err)
	}
	if err := l.Record("player-2", base); err != nil {
		t.Fatalf("recording player-2 must succeed, got %v", err)
	}
	if _, err := l.Settle("player-1", base.Add(2*time.Hour)); err != nil {
		t.Fatalf("settling player-1 must succeed, got %v", err)
	}
	data, err := l.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewOfflineEarningsLedger(impl.Limits{MaxPlayers: 4, MaxBonuses: 4})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	pending, ok := restored.Pending("player-1")
	if !ok || pending.Amount != 200 {
		t.Fatalf("restored pending = %+v ok=%v, want 200 units", pending, ok)
	}
	if got := restored.Bank("player-1"); got != 0 {
		t.Fatalf("restored bank = %d, want 0 before the claim", got)
	}
	claimed, err := restored.Claim("player-1")
	if err != nil {
		t.Fatalf("claiming after restore must succeed, got %v", err)
	}
	if claimed.Amount != 200 {
		t.Fatalf("claimed amount after restore = %d, want 200", claimed.Amount)
	}
	report, err := restored.Settle("player-2", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("settling player-2 after restore must succeed, got %v", err)
	}
	if report.Amount != 50 {
		t.Fatalf("player-2 amount after restore = %d, want 50 from the restored baseline", report.Amount)
	}
}

func TestRestoreRejectsGarbageSnapshot(t *testing.T) {
	l := newEarningsLedger(t)
	base := baseMoment()
	if err := l.SetRate("player-1", impl.Rate{Base: 100}); err != nil {
		t.Fatalf("setting the rate must succeed, got %v", err)
	}
	if err := l.Record("player-1", base); err != nil {
		t.Fatalf("recording the last online moment must succeed, got %v", err)
	}
	if err := l.Restore([]byte("not a snapshot at all")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a garbage snapshot must report ErrBadSnapshot, got %v", err)
	}
	if err := l.Record("player-1", base.Add(time.Hour)); err != nil {
		t.Fatalf("the ledger must stay usable after a failed restore, got %v", err)
	}
	report, err := l.Settle("player-1", base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settling after a failed restore must succeed, got %v", err)
	}
	if report.Amount != 100 {
		t.Fatalf("settled amount = %d after a failed restore, want 100", report.Amount)
	}
}
