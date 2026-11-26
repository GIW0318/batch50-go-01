package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-a-23"
)

func newMasteryLedger(t *testing.T) *impl.HeroMasteryLedger {
	t.Helper()
	l := impl.NewHeroMasteryLedger(impl.Limits{MaxPairs: 4, MaxLevel: 8, MaxXPPerMatch: 1000})
	if err := l.SetModeFactor("ranked", 1.0); err != nil {
		t.Fatalf("registering the ranked factor must succeed, got %v", err)
	}
	if err := l.SetModeFactor("casual", 0.5); err != nil {
		t.Fatalf("registering the casual factor must succeed, got %v", err)
	}
	return l
}

func TestAwardAccumulatesExperience(t *testing.T) {
	l := newMasteryLedger(t)
	gain, err := l.Award("account-1", "hero-a", 50, "ranked")
	if err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if gain.Effective != 50 || gain.XPAfter != 50 {
		t.Fatalf("gain = %+v, want 50 effective experience and 50 total", gain)
	}
	if gain.LevelBefore != 1 || gain.LevelAfter != 1 || gain.LevelsGained != 0 {
		t.Fatalf("level movement = %d -> %d (%d gained), want to stay at level 1", gain.LevelBefore, gain.LevelAfter, gain.LevelsGained)
	}
	if gain.Capped {
		t.Fatalf("fifty experience below the per match cap must not be capped")
	}
	progress, err := l.Progress("account-1", "hero-a")
	if err != nil {
		t.Fatalf("reading progress must succeed, got %v", err)
	}
	if progress.XP != 50 || progress.Level != 1 {
		t.Fatalf("progress = %+v, want 50 experience at level 1", progress)
	}
}

func TestAwardPromotesAtThreshold(t *testing.T) {
	l := newMasteryLedger(t)
	gain, err := l.Award("account-1", "hero-a", 120, "ranked")
	if err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if gain.LevelAfter != 2 || gain.LevelsGained != 1 {
		t.Fatalf("gain = %+v, want promotion from level 1 to level 2", gain)
	}
	level, err := l.Level("account-1", "hero-a")
	if err != nil {
		t.Fatalf("reading the level must succeed, got %v", err)
	}
	if level.Level != 2 || level.XP != 120 || level.NextXP != 360 {
		t.Fatalf("level view = %+v, want level 2 with 120 experience and the next threshold at 360", level)
	}
}

func TestAwardCrossesMultipleLevels(t *testing.T) {
	l := newMasteryLedger(t)
	gain, err := l.Award("account-1", "hero-a", 400, "ranked")
	if err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if gain.LevelAfter != 3 {
		t.Fatalf("level after 400 experience = %d, want 3", gain.LevelAfter)
	}
	if gain.LevelsGained != 2 {
		t.Fatalf("levels gained = %d, want 2", gain.LevelsGained)
	}
	if gain.XPAfter != 400 {
		t.Fatalf("total experience = %d, want the excess over level 3 to be kept", gain.XPAfter)
	}
}

func TestAwardCapsSingleMatchExperience(t *testing.T) {
	l := impl.NewHeroMasteryLedger(impl.Limits{MaxPairs: 4, MaxLevel: 8, MaxXPPerMatch: 200})
	if err := l.SetModeFactor("ranked", 1.0); err != nil {
		t.Fatalf("registering the ranked factor must succeed, got %v", err)
	}
	gain, err := l.Award("account-1", "hero-a", 900, "ranked")
	if err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if gain.Effective != 200 {
		t.Fatalf("effective experience = %d, want the 200 per match cap", gain.Effective)
	}
	if !gain.Capped {
		t.Fatalf("award above the per match cap must be reported as capped")
	}
	if gain.XPAfter != 200 {
		t.Fatalf("total experience = %d, want 200 with the excess dropped", gain.XPAfter)
	}
}

func TestModeFactorAppliedBeforeCap(t *testing.T) {
	l := newMasteryLedger(t)
	gain, err := l.Award("account-1", "hero-a", 300, "casual")
	if err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if gain.Effective != 150 {
		t.Fatalf("effective experience = %d, want 150 after the 0.5 factor", gain.Effective)
	}
	if gain.Capped {
		t.Fatalf("150 experience below the cap must not be reported as capped")
	}
	if gain.XPAfter != 150 {
		t.Fatalf("total experience = %d, want 150", gain.XPAfter)
	}
}

func TestAwardRejectsUnknownMode(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Award("account-1", "hero-a", 50, "siege"); !errors.Is(err, impl.ErrUnknownMode) {
		t.Fatalf("an unregistered mode must report ErrUnknownMode, got %v", err)
	}
	if _, err := l.Award("account-1", "hero-a", 50, ""); !errors.Is(err, impl.ErrBadMode) {
		t.Fatalf("a blank mode must report ErrBadMode, got %v", err)
	}
}

func TestAwardRejectsNonPositiveExperience(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Award("account-1", "hero-a", 0, "ranked"); !errors.Is(err, impl.ErrBadXP) {
		t.Fatalf("zero experience must report ErrBadXP, got %v", err)
	}
	if _, err := l.Award("account-1", "hero-a", -10, "ranked"); !errors.Is(err, impl.ErrBadXP) {
		t.Fatalf("negative experience must report ErrBadXP, got %v", err)
	}
	if _, err := l.Award("", "hero-a", 10, "ranked"); !errors.Is(err, impl.ErrBadIdentity) {
		t.Fatalf("a blank account must report ErrBadIdentity, got %v", err)
	}
}

func TestSetModeFactorRejectsOutOfRange(t *testing.T) {
	l := newMasteryLedger(t)
	if err := l.SetModeFactor("ranked", 0); !errors.Is(err, impl.ErrBadFactor) {
		t.Fatalf("a zero factor must report ErrBadFactor, got %v", err)
	}
	if err := l.SetModeFactor("ranked", 3.5); !errors.Is(err, impl.ErrBadFactor) {
		t.Fatalf("a factor above three must report ErrBadFactor, got %v", err)
	}
	if err := l.SetModeFactor("", 1.0); !errors.Is(err, impl.ErrBadMode) {
		t.Fatalf("a blank mode must report ErrBadMode, got %v", err)
	}
}

func TestClaimMilestoneOnce(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Award("account-1", "hero-a", 400, "ranked"); err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	reward, err := l.ClaimMilestone("account-1", "hero-a", 3)
	if err != nil {
		t.Fatalf("claiming the level three milestone must succeed, got %v", err)
	}
	if reward.Level != 3 || reward.Amount != 300 {
		t.Fatalf("reward = %+v, want 300 for level 3", reward)
	}
	if _, err := l.ClaimMilestone("account-1", "hero-a", 3); !errors.Is(err, impl.ErrAlreadyClaimed) {
		t.Fatalf("a repeated claim must report ErrAlreadyClaimed, got %v", err)
	}
	progress, err := l.Progress("account-1", "hero-a")
	if err != nil {
		t.Fatalf("reading progress must succeed, got %v", err)
	}
	if len(progress.Claimed) != 1 || progress.Claimed[0] != 3 {
		t.Fatalf("claimed milestones = %v, want exactly level 3", progress.Claimed)
	}
}

func TestClaimMilestoneRequiresReachedLevel(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Award("account-1", "hero-a", 120, "ranked"); err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if _, err := l.ClaimMilestone("account-1", "hero-a", 4); !errors.Is(err, impl.ErrLevelNotReached) {
		t.Fatalf("claiming an unreached level must report ErrLevelNotReached, got %v", err)
	}
	if _, err := l.ClaimMilestone("account-1", "hero-a", 1); !errors.Is(err, impl.ErrBadLevel) {
		t.Fatalf("claiming level one must report ErrBadLevel, got %v", err)
	}
}

func TestLeaderboardOrderAndLimit(t *testing.T) {
	l := newMasteryLedger(t)
	awards := []struct {
		account string
		xp      int
	}{
		{"account-1", 120},
		{"account-2", 360},
		{"account-3", 120},
	}
	for _, a := range awards {
		if _, err := l.Award(a.account, "hero-a", a.xp, "ranked"); err != nil {
			t.Fatalf("awarding %d experience to %s must succeed, got %v", a.xp, a.account, err)
		}
	}
	rows := l.Leaderboard("hero-a", 10)
	if len(rows) != 3 {
		t.Fatalf("leaderboard length = %d, want 3", len(rows))
	}
	if rows[0].Account != "account-2" || rows[0].XP != 360 {
		t.Fatalf("leader = %+v, want account-2 with 360 experience", rows[0])
	}
	if rows[1].Account != "account-1" || rows[2].Account != "account-3" {
		t.Fatalf("tie order = %q then %q, want account-1 before account-3", rows[1].Account, rows[2].Account)
	}
	top := l.Leaderboard("hero-a", 2)
	if len(top) != 2 {
		t.Fatalf("limited leaderboard length = %d, want 2", len(top))
	}
	if got := len(l.Leaderboard("hero-a", 0)); got != 0 {
		t.Fatalf("leaderboard with n=0 returned %d rows, want none", got)
	}
}

func TestPairLimitEnforced(t *testing.T) {
	l := impl.NewHeroMasteryLedger(impl.Limits{MaxPairs: 1, MaxLevel: 8, MaxXPPerMatch: 1000})
	if err := l.SetModeFactor("ranked", 1.0); err != nil {
		t.Fatalf("registering the ranked factor must succeed, got %v", err)
	}
	if _, err := l.Award("account-1", "hero-a", 50, "ranked"); err != nil {
		t.Fatalf("the first pair must be accepted, got %v", err)
	}
	if _, err := l.Award("account-1", "hero-b", 50, "ranked"); !errors.Is(err, impl.ErrLimit) {
		t.Fatalf("a second pair must report ErrLimit, got %v", err)
	}
}

func TestProgressRejectsUnknownPair(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Progress("ghost", "hero-a"); !errors.Is(err, impl.ErrUnknownPair) {
		t.Fatalf("an unknown pair must report ErrUnknownPair, got %v", err)
	}
	if _, err := l.Level("account-1", "hero-a"); !errors.Is(err, impl.ErrUnknownPair) {
		t.Fatalf("level of an unknown pair must report ErrUnknownPair, got %v", err)
	}
}

func TestSnapshotRestoreKeepsExperienceAndClaims(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Award("account-1", "hero-a", 400, "ranked"); err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if _, err := l.ClaimMilestone("account-1", "hero-a", 3); err != nil {
		t.Fatalf("claiming the level three milestone must succeed, got %v", err)
	}
	data, err := l.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewHeroMasteryLedger(impl.Limits{MaxPairs: 4, MaxLevel: 8, MaxXPPerMatch: 1000})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	progress, err := restored.Progress("account-1", "hero-a")
	if err != nil {
		t.Fatalf("reading restored progress must succeed, got %v", err)
	}
	if progress.XP != 400 || progress.Level != 3 {
		t.Fatalf("restored progress = %+v, want 400 experience at level 3", progress)
	}
	if len(progress.Claimed) != 1 || progress.Claimed[0] != 3 {
		t.Fatalf("restored claims = %v, want exactly level 3", progress.Claimed)
	}
	if _, err := restored.ClaimMilestone("account-1", "hero-a", 3); !errors.Is(err, impl.ErrAlreadyClaimed) {
		t.Fatalf("reclaiming after restore must report ErrAlreadyClaimed, got %v", err)
	}
	if rows := restored.Leaderboard("hero-a", 1); len(rows) != 1 || rows[0].XP != 400 {
		t.Fatalf("restored leaderboard = %+v, want one row with 400 experience", rows)
	}
}

func TestRestoreRejectsGarbageSnapshot(t *testing.T) {
	l := newMasteryLedger(t)
	if _, err := l.Award("account-1", "hero-a", 120, "ranked"); err != nil {
		t.Fatalf("awarding experience must succeed, got %v", err)
	}
	if err := l.Restore([]byte("<<< broken snapshot >>>")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a garbage snapshot must report ErrBadSnapshot, got %v", err)
	}
	level, err := l.Level("account-1", "hero-a")
	if err != nil {
		t.Fatalf("the ledger must stay usable after a failed restore, got %v", err)
	}
	if level.Level != 2 || level.XP != 120 {
		t.Fatalf("level after a failed restore = %+v, want level 2 with 120 experience", level)
	}
}
