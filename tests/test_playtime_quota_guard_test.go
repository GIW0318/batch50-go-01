package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-04"
)

func testPolicy() impl.Policy {
	return impl.Policy{
		DailyMinutes:        120,
		CurfewStartHour:     22,
		CurfewEndHour:       8,
		GraceMinutes:        60,
		HeartbeatGapMinutes: 60,
		UTCOffsetMinutes:    0,
	}
}

func dayStart(day int) time.Time {
	return time.Date(2026, time.March, day, 0, 0, 0, 0, time.UTC)
}

func at(hour, minute int) time.Time {
	return dayStart(1).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

func TestBeginSessionAdmitsWithinQuota(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	a, err := g.BeginSession("acc-1", at(10, 0))
	if err != nil {
		t.Fatalf("begin session inside the quota: %v", err)
	}
	if !a.Allowed || a.Reason != impl.ReasonAllowed {
		t.Fatalf("admission = %+v, want allowed/%q", a, impl.ReasonAllowed)
	}
	if a.AccountID != "acc-1" {
		t.Fatalf("admission account = %q, want acc-1", a.AccountID)
	}
	if a.RemainingMinutes != 120 {
		t.Fatalf("remaining minutes = %d, want the full 120", a.RemainingMinutes)
	}
	if a.Seq <= 0 {
		t.Fatalf("admission sequence = %d, want a positive number", a.Seq)
	}
	u, err := g.Usage("acc-1", at(10, 0))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.Day != "2026-03-01" {
		t.Fatalf("usage day = %q, want 2026-03-01", u.Day)
	}
	if u.UsedMinutes != 0 || u.LimitMinutes != 120 || u.RemainingMinutes != 120 {
		t.Fatalf("fresh usage = %+v, want 0 of 120", u)
	}
}

func TestBeginSessionRejectsSecondSession(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := g.BeginSession("acc-1", at(10, 5)); !errors.Is(err, impl.ErrAlreadyActive) {
		t.Fatalf("a second session must fail with ErrAlreadyActive, got %v", err)
	}
	if _, err := g.BeginSession("acc-2", at(10, 5)); err != nil {
		t.Fatalf("another account must not be blocked, got %v", err)
	}
}

func TestBeginSessionRejectsBlankAccount(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("   ", at(10, 0)); !errors.Is(err, impl.ErrBadAccount) {
		t.Fatalf("blank account must fail with ErrBadAccount, got %v", err)
	}
	if _, err := g.Usage("", at(10, 0)); !errors.Is(err, impl.ErrBadAccount) {
		t.Fatalf("usage of a blank account must fail with ErrBadAccount, got %v", err)
	}
	if _, err := g.Heartbeat("  ", at(10, 0)); !errors.Is(err, impl.ErrBadAccount) {
		t.Fatalf("heartbeat of a blank account must fail with ErrBadAccount, got %v", err)
	}
	if err := g.EndSession("", "quit"); !errors.Is(err, impl.ErrBadAccount) {
		t.Fatalf("end of a blank account must fail with ErrBadAccount, got %v", err)
	}
}

func TestHeartbeatReportsAccumulatedUsage(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	a, err := g.BeginSession("acc-1", at(10, 0))
	if err != nil {
		t.Fatalf("begin session: %v", err)
	}
	s1, err := g.Heartbeat("acc-1", at(10, 30))
	if err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if s1.UsedMinutes != 30 {
		t.Fatalf("used minutes after 30 minutes = %d, want 30", s1.UsedMinutes)
	}
	if !s1.Active {
		t.Fatalf("a renewed session must stay active")
	}
	if s1.AccountID != "acc-1" {
		t.Fatalf("session account = %q, want acc-1", s1.AccountID)
	}
	if s1.Seq <= a.Seq {
		t.Fatalf("heartbeat sequence = %d, must pass the admission %d", s1.Seq, a.Seq)
	}
	s2, err := g.Heartbeat("acc-1", at(10, 45))
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if s2.UsedMinutes != 45 {
		t.Fatalf("used minutes after 45 minutes = %d, want 45", s2.UsedMinutes)
	}
	u, err := g.Usage("acc-1", at(10, 45))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 45 || u.RemainingMinutes != 75 {
		t.Fatalf("usage = %+v, want 45 used and 75 left", u)
	}
}

func TestHeartbeatWithoutSessionFails(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.Heartbeat("acc-1", at(10, 0)); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("heartbeat without a session must fail with ErrNoSession, got %v", err)
	}
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(10, 20)); err != nil {
		t.Fatalf("heartbeat of the open session: %v", err)
	}
	if err := g.EndSession("acc-1", "quit"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(10, 30)); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("heartbeat after the session closed must fail with ErrNoSession, got %v", err)
	}
}

func TestEndSessionFreesSlot(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(10, 40)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := g.EndSession("acc-1", "quit"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	u, err := g.Usage("acc-1", at(10, 50))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 40 {
		t.Fatalf("booked minutes = %d, want the 40 minutes up to the last heartbeat", u.UsedMinutes)
	}
	if err := g.EndSession("acc-1", "quit"); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("ending a closed session must fail with ErrNoSession, got %v", err)
	}
	a, err := g.BeginSession("acc-1", at(10, 55))
	if err != nil {
		t.Fatalf("begin after the slot was freed: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("a freed slot must admit a new session, got %+v", a)
	}
}

func TestUsageAccumulatesAcrossSessions(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("acc-1", at(9, 0)); err != nil {
		t.Fatalf("begin first session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(9, 40)); err != nil {
		t.Fatalf("heartbeat of the first session: %v", err)
	}
	if err := g.EndSession("acc-1", "quit"); err != nil {
		t.Fatalf("end first session: %v", err)
	}
	if _, err := g.BeginSession("acc-1", at(11, 0)); err != nil {
		t.Fatalf("begin second session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(11, 30)); err != nil {
		t.Fatalf("heartbeat of the second session: %v", err)
	}
	if err := g.EndSession("acc-1", "quit"); err != nil {
		t.Fatalf("end second session: %v", err)
	}
	u, err := g.Usage("acc-1", at(12, 0))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 70 {
		t.Fatalf("used minutes = %d, want 40+30=70", u.UsedMinutes)
	}
	if u.RemainingMinutes != 50 {
		t.Fatalf("remaining minutes = %d, want 50", u.RemainingMinutes)
	}
}

func TestBeginSessionRejectsExhaustedQuota(t *testing.T) {
	policy := testPolicy()
	policy.DailyMinutes = 60
	g := impl.NewPlaytimeQuotaGuard(policy)
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(11, 30)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := g.EndSession("acc-1", "quit"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	u, err := g.Usage("acc-1", at(12, 0))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 60 || u.RemainingMinutes != 0 {
		t.Fatalf("usage after overrun = %+v, want 60 used and none left", u)
	}
	a, err := g.BeginSession("acc-1", at(12, 10))
	if err != nil {
		t.Fatalf("begin on an exhausted day: %v", err)
	}
	if a.Allowed || a.Reason != impl.ReasonQuotaExhausted {
		t.Fatalf("admission = %+v, want refused with %q", a, impl.ReasonQuotaExhausted)
	}
	if a.RemainingMinutes != 0 {
		t.Fatalf("remaining minutes = %d, want 0", a.RemainingMinutes)
	}
	if _, err := g.Heartbeat("acc-1", at(12, 20)); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("a refused admission must not open a session, got %v", err)
	}
}

func TestUsageIsIsolatedPerLocalDay(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("begin day one session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(10, 40)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := g.EndSession("acc-1", "quit"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	first, err := g.Usage("acc-1", at(10, 40))
	if err != nil {
		t.Fatalf("day one usage: %v", err)
	}
	if first.UsedMinutes != 40 {
		t.Fatalf("day one used = %d, want 40", first.UsedMinutes)
	}
	secondDay := dayStart(2).Add(10 * time.Hour)
	second, err := g.Usage("acc-1", secondDay)
	if err != nil {
		t.Fatalf("day two usage: %v", err)
	}
	if second.Day != "2026-03-02" {
		t.Fatalf("day two key = %q, want 2026-03-02", second.Day)
	}
	if second.UsedMinutes != 0 || second.RemainingMinutes != 120 {
		t.Fatalf("day two usage = %+v, want a fresh 120 minute budget", second)
	}
	a, err := g.BeginSession("acc-1", secondDay)
	if err != nil {
		t.Fatalf("begin on day two: %v", err)
	}
	if !a.Allowed || a.RemainingMinutes != 120 {
		t.Fatalf("day two admission = %+v, want the full budget", a)
	}
}

func TestCurfewBlocksAdmission(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	late, err := g.BeginSession("acc-1", at(22, 30))
	if err != nil {
		t.Fatalf("begin inside the curfew: %v", err)
	}
	if late.Allowed || late.Reason != impl.ReasonCurfew {
		t.Fatalf("admission = %+v, want refused with %q", late, impl.ReasonCurfew)
	}
	want := time.Date(2026, time.March, 2, 8, 0, 0, 0, time.UTC)
	if !late.CurfewUntil.Equal(want) {
		t.Fatalf("curfew until = %v, want %v", late.CurfewUntil, want)
	}
	if late.RemainingMinutes != 120 {
		t.Fatalf("curfew must not consume the quota, remaining = %d", late.RemainingMinutes)
	}
	early, err := g.BeginSession("acc-2", at(7, 30))
	if err != nil {
		t.Fatalf("begin before the curfew ends: %v", err)
	}
	if early.Allowed || early.Reason != impl.ReasonCurfew {
		t.Fatalf("a wrapped curfew must still block, got %+v", early)
	}
	u, err := g.Usage("acc-1", at(23, 0))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 0 {
		t.Fatalf("a refused admission booked %d minutes, want 0", u.UsedMinutes)
	}
}

func TestCurfewGraceAllowsCurrentSession(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	a, err := g.BeginSession("acc-1", at(21, 50))
	if err != nil {
		t.Fatalf("begin before the curfew: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("the session before the curfew must be admitted, got %+v", a)
	}
	if _, err := g.Heartbeat("acc-1", at(22, 20)); err != nil {
		t.Fatalf("heartbeat inside the grace window: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(22, 40)); err != nil {
		t.Fatalf("second heartbeat inside the grace window: %v", err)
	}
	state, err := g.Heartbeat("acc-1", at(23, 10))
	if !errors.Is(err, impl.ErrCurfewGraceExpired) {
		t.Fatalf("a heartbeat past the grace deadline must fail with ErrCurfewGraceExpired, got %v", err)
	}
	if state.Active {
		t.Fatalf("the session must be closed once the grace deadline passed")
	}
	if state.UsedMinutes != 50 {
		t.Fatalf("closed session used = %d, want the 50 minutes up to the last heartbeat", state.UsedMinutes)
	}
	u, err := g.Usage("acc-1", at(23, 10))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 50 {
		t.Fatalf("booked minutes = %d, want 50 without the missed tail", u.UsedMinutes)
	}
	if next, err := g.BeginSession("acc-1", at(23, 20)); err != nil {
		t.Fatalf("begin inside the curfew: %v", err)
	} else if next.Allowed || next.Reason != impl.ReasonCurfew {
		t.Fatalf("a new match inside the curfew must be refused, got %+v", next)
	}
}

func TestHeartbeatGapTruncatesAtLastHeartbeat(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(10, 30)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	state, err := g.Heartbeat("acc-1", at(12, 0))
	if !errors.Is(err, impl.ErrHeartbeatGap) {
		t.Fatalf("a 90 minute gap must fail with ErrHeartbeatGap, got %v", err)
	}
	if state.Active {
		t.Fatalf("a missed heartbeat must close the session")
	}
	if state.UsedMinutes != 30 {
		t.Fatalf("closed session used = %d, want the 30 minutes up to the last heartbeat", state.UsedMinutes)
	}
	u, err := g.Usage("acc-1", at(12, 0))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.UsedMinutes != 30 {
		t.Fatalf("booked minutes = %d, want 30 and the gap left unbooked", u.UsedMinutes)
	}
	if _, err := g.Heartbeat("acc-1", at(12, 10)); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("the closed session must be gone, got %v", err)
	}
}

func TestGrantExemptionRequiresOperator(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	until := at(23, 0)
	if err := g.GrantExemption("acc-1", until, "   "); !errors.Is(err, impl.ErrBadExemption) {
		t.Fatalf("an exemption without an operator must fail with ErrBadExemption, got %v", err)
	}
	if err := g.GrantExemption("  ", until, "guardian-7"); !errors.Is(err, impl.ErrBadAccount) {
		t.Fatalf("an exemption for a blank account must fail with ErrBadAccount, got %v", err)
	}
	if err := g.GrantExemption("acc-1", until, "guardian-7"); err != nil {
		t.Fatalf("grant exemption: %v", err)
	}
	ex, ok := g.ExemptionOf("acc-1")
	if !ok {
		t.Fatalf("the granted exemption must be readable")
	}
	if ex.AccountID != "acc-1" || ex.By != "guardian-7" {
		t.Fatalf("exemption = %+v, want acc-1 approved by guardian-7", ex)
	}
	if !ex.Until.Equal(until) {
		t.Fatalf("exemption deadline = %v, want %v", ex.Until, until)
	}
	if ex.Seq <= 0 {
		t.Fatalf("exemption sequence = %d, want a positive number", ex.Seq)
	}
	if err := g.RevokeExemption("acc-1"); err != nil {
		t.Fatalf("revoke exemption: %v", err)
	}
	if _, ok := g.ExemptionOf("acc-1"); ok {
		t.Fatalf("a revoked exemption must not be readable")
	}
	if err := g.RevokeExemption("acc-1"); !errors.Is(err, impl.ErrNoExemption) {
		t.Fatalf("revoking an absent exemption must fail with ErrNoExemption, got %v", err)
	}
}

func TestSnapshotRestoreKeepsUsageAndExemption(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	a, err := g.BeginSession("acc-1", at(10, 0))
	if err != nil {
		t.Fatalf("begin session: %v", err)
	}
	last, err := g.Heartbeat("acc-1", at(10, 30))
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := g.GrantExemption("acc-1", at(23, 0), "guardian-7"); err != nil {
		t.Fatalf("grant exemption: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewPlaytimeQuotaGuard(testPolicy())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	u, err := restored.Usage("acc-1", at(10, 30))
	if err != nil {
		t.Fatalf("usage after restore: %v", err)
	}
	if u.UsedMinutes != 30 {
		t.Fatalf("restored usage = %d, want 30", u.UsedMinutes)
	}
	if _, ok := restored.ExemptionOf("acc-1"); !ok {
		t.Fatalf("the exemption must survive the restore")
	}
	next, err := restored.Heartbeat("acc-1", at(10, 40))
	if err != nil {
		t.Fatalf("the restored session must still be renewable: %v", err)
	}
	if next.UsedMinutes != 40 {
		t.Fatalf("usage after the restored heartbeat = %d, want 40", next.UsedMinutes)
	}
	if next.Seq <= last.Seq || next.Seq <= a.Seq {
		t.Fatalf("heartbeat sequence after restore = %d, must pass %d and %d", next.Seq, last.Seq, a.Seq)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	g := impl.NewPlaytimeQuotaGuard(testPolicy())
	if _, err := g.BeginSession("acc-1", at(10, 0)); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	if _, err := g.Heartbeat("acc-1", at(10, 20)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := g.Restore([]byte("##not-a-guard##")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	u, err := g.Usage("acc-1", at(10, 20))
	if err != nil {
		t.Fatalf("usage after the rejected restore: %v", err)
	}
	if u.UsedMinutes != 20 {
		t.Fatalf("a rejected restore must not touch live state, used = %d, want 20", u.UsedMinutes)
	}
	next, err := g.Heartbeat("acc-1", at(10, 30))
	if err != nil {
		t.Fatalf("the live session must keep renewing after the rejected restore: %v", err)
	}
	if next.UsedMinutes != 30 {
		t.Fatalf("usage after the live heartbeat = %d, want 30", next.UsedMinutes)
	}
}
