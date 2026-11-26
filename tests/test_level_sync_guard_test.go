package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-a-25"
)

func newLevelSyncGuard(t *testing.T) *impl.LevelSyncGuard {
	t.Helper()
	g := impl.NewLevelSyncGuard(impl.Limits{MaxPlayers: 4, MaxZones: 4})
	if err := g.SetSyncRule(impl.SyncRule{PercentPerLevel: 5, MinPercent: 40}); err != nil {
		t.Fatalf("setting the sync rule must succeed, got %v", err)
	}
	if err := g.SetZoneCap("starter", 20); err != nil {
		t.Fatalf("registering the starter zone must succeed, got %v", err)
	}
	if err := g.SetZoneCap("capital", 60); err != nil {
		t.Fatalf("registering the capital zone must succeed, got %v", err)
	}
	return g
}

func bigStats() impl.Stats {
	return impl.Stats{HP: 1000, Attack: 200, Defense: 100, Speed: 50}
}

func TestEnterWithoutCapReturnsBase(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 10); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering a zone above the player level must succeed, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	if effective != bigStats() {
		t.Fatalf("effective stats = %+v, want the untouched base %+v", effective, bigStats())
	}
}

func TestEnterSuppressesAtZoneCap(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	want := impl.Stats{HP: 400, Attack: 80, Defense: 40, Speed: 20}
	if effective != want {
		t.Fatalf("effective stats = %+v, want %+v at forty percent", effective, want)
	}
}

func TestEnterReturnsBaselineSnapshot(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	baseline, err := g.Enter("player-1", "starter")
	if err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	if baseline != bigStats() {
		t.Fatalf("returned baseline = %+v, want the unsuppressed %+v", baseline, bigStats())
	}
	state, err := g.State("player-1")
	if err != nil {
		t.Fatalf("reading the sync state must succeed, got %v", err)
	}
	if !state.Active || state.Zone != "starter" || state.ZoneCap != 20 {
		t.Fatalf("state = %+v, want an active starter sync at cap 20", state)
	}
}

func TestEnterRejectsUnknownZone(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetPlayerLevel("player-1", 10); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "desert"); !errors.Is(err, impl.ErrUnknownZone) {
		t.Fatalf("an unregistered zone must report ErrUnknownZone, got %v", err)
	}
	if _, err := g.Enter("", "starter"); !errors.Is(err, impl.ErrBadZone) {
		t.Fatalf("a blank player must report ErrBadZone, got %v", err)
	}
}

func TestSetZoneCapRejectsNonPositive(t *testing.T) {
	g := impl.NewLevelSyncGuard(impl.Limits{MaxPlayers: 4, MaxZones: 4})
	if err := g.SetZoneCap("starter", 0); !errors.Is(err, impl.ErrBadCap) {
		t.Fatalf("a zero cap must report ErrBadCap, got %v", err)
	}
	if err := g.SetZoneCap("starter", -20); !errors.Is(err, impl.ErrBadCap) {
		t.Fatalf("a negative cap must report ErrBadCap, got %v", err)
	}
	if err := g.SetZoneCap("", 20); !errors.Is(err, impl.ErrBadZone) {
		t.Fatalf("a blank zone must report ErrBadZone, got %v", err)
	}
	if err := g.SetZoneCap("starter", 20); err != nil {
		t.Fatalf("a valid cap must be accepted, got %v", err)
	}
}

func TestSetSyncRuleRejectsOutOfRange(t *testing.T) {
	g := impl.NewLevelSyncGuard(impl.Limits{MaxPlayers: 4, MaxZones: 4})
	if err := g.SetSyncRule(impl.SyncRule{PercentPerLevel: -1, MinPercent: 40}); !errors.Is(err, impl.ErrBadRule) {
		t.Fatalf("a negative percent must report ErrBadRule, got %v", err)
	}
	if err := g.SetSyncRule(impl.SyncRule{PercentPerLevel: 5, MinPercent: 150}); !errors.Is(err, impl.ErrBadRule) {
		t.Fatalf("an out of range floor must report ErrBadRule, got %v", err)
	}
	if err := g.SetSyncRule(impl.SyncRule{PercentPerLevel: 101, MinPercent: 40}); !errors.Is(err, impl.ErrBadRule) {
		t.Fatalf("a percent above one hundred must report ErrBadRule, got %v", err)
	}
	if err := g.SetSyncRule(impl.SyncRule{PercentPerLevel: 5, MinPercent: 40}); err != nil {
		t.Fatalf("a valid rule must be accepted, got %v", err)
	}
}

func TestEffectiveBeforeEnterIsBase(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	if effective != bigStats() {
		t.Fatalf("effective stats = %+v before entering, want the base %+v", effective, bigStats())
	}
	state, err := g.State("player-1")
	if err != nil {
		t.Fatalf("reading the sync state must succeed, got %v", err)
	}
	if state.Active {
		t.Fatalf("state before entering = %+v, want inactive", state)
	}
}

func TestLeaveRestoresBaseStats(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	if err := g.Leave("player-1"); err != nil {
		t.Fatalf("leaving the zone must succeed, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	if effective != bigStats() {
		t.Fatalf("effective stats after leaving = %+v, want the base %+v", effective, bigStats())
	}
	state, err := g.State("player-1")
	if err != nil {
		t.Fatalf("reading the sync state must succeed, got %v", err)
	}
	if state.Active || state.Zone != "" {
		t.Fatalf("state after leaving = %+v, want inactive with no zone", state)
	}
}

func TestLeaveWithoutEnterRejected(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.Leave("player-1"); !errors.Is(err, impl.ErrNotSynced) {
		t.Fatalf("leaving without entering must report ErrNotSynced, got %v", err)
	}
}

func TestGearLevelClampedToCap(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if err := g.SetGear("player-1", 80, impl.Stats{Attack: 3}); err != nil {
		t.Fatalf("setting the gear must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	state, err := g.State("player-1")
	if err != nil {
		t.Fatalf("reading the sync state must succeed, got %v", err)
	}
	if state.GearLevel != 80 {
		t.Fatalf("stored gear level = %d, want the original 80", state.GearLevel)
	}
	if state.EffectiveGearLevel != 20 {
		t.Fatalf("effective gear level = %d, want the clamped 20", state.EffectiveGearLevel)
	}
	if state.Gear.Attack != 60 {
		t.Fatalf("gear attack bonus = %d, want 60 from twenty clamped levels", state.Gear.Attack)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	if effective.Attack != 104 {
		t.Fatalf("effective attack = %d, want 104 from (200+60) at forty percent", effective.Attack)
	}
}

func TestGearBonusScalesWithLevel(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetGear("player-1", 10, impl.Stats{HP: 10, Attack: 2}); err != nil {
		t.Fatalf("setting the gear must succeed, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	if effective.HP != 1100 {
		t.Fatalf("effective HP = %d, want 1100 from ten gear levels of 10", effective.HP)
	}
	if effective.Attack != 220 {
		t.Fatalf("effective attack = %d, want 220 from ten gear levels of 2", effective.Attack)
	}
}

func TestBuffBeforeEnterIncluded(t *testing.T) {
	g := newLevelSyncGuard(t)
	base := impl.Stats{HP: 100, Attack: 100, Defense: 50, Speed: 10}
	if err := g.SetBaseStats("player-1", base); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if err := g.AddBuff("player-1", impl.Stats{Attack: 20}); err != nil {
		t.Fatalf("adding a buff must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	if effective.Attack != 48 {
		t.Fatalf("effective attack = %d, want 48 from (100+20) at forty percent", effective.Attack)
	}
	if effective.HP != 40 {
		t.Fatalf("effective HP = %d, want 40 from 100 at forty percent", effective.HP)
	}
}

func TestSetPlayerLevelRejectsNonPositive(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetPlayerLevel("player-1", 0); !errors.Is(err, impl.ErrBadLevel) {
		t.Fatalf("a zero level must report ErrBadLevel, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", -3); !errors.Is(err, impl.ErrBadLevel) {
		t.Fatalf("a negative level must report ErrBadLevel, got %v", err)
	}
	if err := g.SetBaseStats("player-1", impl.Stats{HP: -1}); !errors.Is(err, impl.ErrBadStats) {
		t.Fatalf("negative stats must report ErrBadStats, got %v", err)
	}
	if err := g.AddBuff("player-1", impl.Stats{Attack: -5}); !errors.Is(err, impl.ErrBadStats) {
		t.Fatalf("a negative buff must report ErrBadStats, got %v", err)
	}
	if err := g.SetGear("player-1", -1, impl.Stats{Attack: 2}); !errors.Is(err, impl.ErrBadStats) {
		t.Fatalf("a negative gear level must report ErrBadStats, got %v", err)
	}
}

func TestSnapshotRestoreKeepsSyncState(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if err := g.SetGear("player-1", 80, impl.Stats{Attack: 3}); err != nil {
		t.Fatalf("setting the gear must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	want, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the effective stats must succeed, got %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewLevelSyncGuard(impl.Limits{MaxPlayers: 4, MaxZones: 4})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	state, err := restored.State("player-1")
	if err != nil {
		t.Fatalf("reading the restored state must succeed, got %v", err)
	}
	if !state.Active || state.Zone != "starter" || state.ZoneCap != 20 {
		t.Fatalf("restored state = %+v, want an active starter sync at cap 20", state)
	}
	if state.PlayerLevel != 60 || state.GearLevel != 80 || state.EffectiveGearLevel != 20 {
		t.Fatalf("restored levels = player %d gear %d effective %d, want 60/80/20", state.PlayerLevel, state.GearLevel, state.EffectiveGearLevel)
	}
	got, err := restored.Effective("player-1")
	if err != nil {
		t.Fatalf("resolving the restored effective stats must succeed, got %v", err)
	}
	if got != want {
		t.Fatalf("restored effective stats = %+v, want the pre-snapshot %+v", got, want)
	}
}

func TestRestoreRejectsGarbageSnapshot(t *testing.T) {
	g := newLevelSyncGuard(t)
	if err := g.SetBaseStats("player-1", bigStats()); err != nil {
		t.Fatalf("setting the base stats must succeed, got %v", err)
	}
	if err := g.SetPlayerLevel("player-1", 60); err != nil {
		t.Fatalf("setting the player level must succeed, got %v", err)
	}
	if _, err := g.Enter("player-1", "starter"); err != nil {
		t.Fatalf("entering the zone must succeed, got %v", err)
	}
	if err := g.Restore([]byte("garbage snapshot body")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a garbage snapshot must report ErrBadSnapshot, got %v", err)
	}
	effective, err := g.Effective("player-1")
	if err != nil {
		t.Fatalf("the guard must stay usable after a failed restore, got %v", err)
	}
	want := impl.Stats{HP: 400, Attack: 80, Defense: 40, Speed: 20}
	if effective != want {
		t.Fatalf("effective stats after a failed restore = %+v, want the suppressed %+v", effective, want)
	}
}
