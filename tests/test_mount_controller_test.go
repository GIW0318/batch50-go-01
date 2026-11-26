package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-16"
)

var clockBase = time.Unix(1700000000, 0)

func testLimits() impl.MountLimits {
	return impl.MountLimits{MaxMounts: 4, MaxEvents: 8}
}

func mountSpec(id, owner string) impl.MountSpec {
	return impl.MountSpec{
		ID:             id,
		Owner:          owner,
		MaxDurability:  30,
		GuideMillis:    2000,
		CooldownMillis: 5000,
	}
}

func newController() *impl.MountController {
	return impl.NewMountController(testLimits(), clockBase)
}

func mustRegister(t *testing.T, c *impl.MountController, spec impl.MountSpec) {
	t.Helper()
	if err := c.RegisterMount(spec); err != nil {
		t.Fatalf("register mount %q: %v", spec.ID, err)
	}
}

func mustSummon(t *testing.T, c *impl.MountController, player, mount string) impl.SummonID {
	t.Helper()
	id, err := c.Summon(player, mount)
	if err != nil {
		t.Fatalf("summon %q for %q: %v", mount, player, err)
	}
	if id == "" {
		t.Fatalf("summon of %q returned an empty summon id", mount)
	}
	return id
}

func mustMount(t *testing.T, c *impl.MountController, player, mount string, guide time.Duration) {
	t.Helper()
	mustSummon(t, c, player, mount)
	if _, err := c.Advance(clockBase.Add(guide)); err != nil {
		t.Fatalf("advance past the summon guide: %v", err)
	}
	if err := c.MountUp(player, mount); err != nil {
		t.Fatalf("mount up %q: %v", mount, err)
	}
}

func TestRegisterMountValidatesSpec(t *testing.T) {
	c := impl.NewMountController(impl.MountLimits{MaxMounts: 2, MaxEvents: 8}, clockBase)

	bad := mountSpec("", "p1")
	if err := c.RegisterMount(bad); !errors.Is(err, impl.ErrBadMountSpec) {
		t.Fatalf("blank mount id must fail with ErrBadMountSpec, got %v", err)
	}
	noDurability := mountSpec("m-bad", "p1")
	noDurability.MaxDurability = 0
	if err := c.RegisterMount(noDurability); !errors.Is(err, impl.ErrBadMountSpec) {
		t.Fatalf("zero durability must fail with ErrBadMountSpec, got %v", err)
	}
	negativeGuide := mountSpec("m-bad2", "p1")
	negativeGuide.GuideMillis = -1
	if err := c.RegisterMount(negativeGuide); !errors.Is(err, impl.ErrBadMountSpec) {
		t.Fatalf("negative guide must fail with ErrBadMountSpec, got %v", err)
	}
	if err := c.RegisterMount(mountSpec("m1", "p1")); err != nil {
		t.Fatalf("valid mount must be accepted, got %v", err)
	}
	if err := c.RegisterMount(mountSpec("m1", "p1")); !errors.Is(err, impl.ErrMountExists) {
		t.Fatalf("duplicate mount id must fail with ErrMountExists, got %v", err)
	}
	if err := c.RegisterMount(mountSpec("m2", "p1")); err != nil {
		t.Fatalf("second mount must be accepted while under the cap, got %v", err)
	}
	if err := c.RegisterMount(mountSpec("m3", "p1")); !errors.Is(err, impl.ErrMountLimit) {
		t.Fatalf("third mount must hit MaxMounts, got %v", err)
	}
}

func TestSummonStartsGuideWithoutMounting(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))

	id := mustSummon(t, c, "p1", "m1")

	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state after summon: %v", err)
	}
	if st.Phase != impl.PhaseSummoning {
		t.Fatalf("phase after summon = %q, want %q", st.Phase, impl.PhaseSummoning)
	}
	if st.MountID != "m1" || st.Summon != id {
		t.Fatalf("state = %+v, want mount m1 with summon %q", st, id)
	}
	if st.GuideLeftMs != 2000 {
		t.Fatalf("guide left = %d, want the registered 2000 ms", st.GuideLeftMs)
	}
	if st.Durability != 30 || st.MaxDurability != 30 {
		t.Fatalf("durability = %d/%d, want a full 30/30 instance", st.Durability, st.MaxDurability)
	}
	events := c.Events("p1")
	if len(events) != 1 {
		t.Fatalf("events after summon = %d, want 1", len(events))
	}
	if events[0].Kind != impl.EventSummonStarted || events[0].MountID != "m1" {
		t.Fatalf("event = %+v, want a summon_started event for m1", events[0])
	}
}

func TestSummonIsIdempotentWhileGuiding(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))

	first := mustSummon(t, c, "p1", "m1")
	second := mustSummon(t, c, "p1", "m1")
	if second != first {
		t.Fatalf("repeated summon returned %q, want the original %q", second, first)
	}
	if got := len(c.Events("p1")); got != 1 {
		t.Fatalf("events after the repeated summon = %d, want 1", got)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.GuideLeftMs != 2000 {
		t.Fatalf("guide left = %d, want the guide untouched by the repeated summon", st.GuideLeftMs)
	}
}

func TestSummonRejectsForeignAndUnknownMount(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))

	if _, err := c.Summon("p2", "m1"); !errors.Is(err, impl.ErrMountNotOwned) {
		t.Fatalf("summoning another player's mount must fail with ErrMountNotOwned, got %v", err)
	}
	if _, err := c.Summon("p1", "m404"); !errors.Is(err, impl.ErrUnknownMount) {
		t.Fatalf("summoning an unregistered mount must fail with ErrUnknownMount, got %v", err)
	}
	if _, err := c.Summon("   ", "m1"); !errors.Is(err, impl.ErrBlankPlayer) {
		t.Fatalf("blank player must fail with ErrBlankPlayer, got %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Phase != impl.PhaseIdle {
		t.Fatalf("rejected summons left the player in phase %q, want %q", st.Phase, impl.PhaseIdle)
	}
}

func TestMountUpRequiresElapsedGuide(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustSummon(t, c, "p1", "m1")

	if err := c.MountUp("p1", "m1"); !errors.Is(err, impl.ErrGuideIncomplete) {
		t.Fatalf("mounting up during the guide must fail with ErrGuideIncomplete, got %v", err)
	}
	if _, err := c.Advance(clockBase.Add(1500 * time.Millisecond)); err != nil {
		t.Fatalf("advance inside the guide: %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state inside the guide: %v", err)
	}
	if st.Phase != impl.PhaseSummoning || st.GuideLeftMs != 500 {
		t.Fatalf("state inside the guide = %+v, want summoning with 500 ms left", st)
	}
	if _, err := c.Advance(clockBase.Add(2000 * time.Millisecond)); err != nil {
		t.Fatalf("advance to the end of the guide: %v", err)
	}
	st, err = c.State("p1")
	if err != nil {
		t.Fatalf("state at the end of the guide: %v", err)
	}
	if st.Phase != impl.PhaseReady {
		t.Fatalf("phase at the end of the guide = %q, want %q", st.Phase, impl.PhaseReady)
	}
	events := c.Events("p1")
	last := events[len(events)-1]
	if last.Kind != impl.EventSummonReady {
		t.Fatalf("last event = %q, want %q", last.Kind, impl.EventSummonReady)
	}
	if err := c.MountUp("p1", "m1"); err != nil {
		t.Fatalf("mount up a finished guide: %v", err)
	}
	st, err = c.State("p1")
	if err != nil {
		t.Fatalf("state after mount up: %v", err)
	}
	if st.Phase != impl.PhaseMounted || st.GuideLeftMs != 0 {
		t.Fatalf("state after mount up = %+v, want mounted with no guide left", st)
	}
}

func TestSummonRejectsSecondActiveMount(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustRegister(t, c, mountSpec("m2", "p1"))

	mustSummon(t, c, "p1", "m1")
	if _, err := c.Summon("p1", "m2"); !errors.Is(err, impl.ErrSummonPending) {
		t.Fatalf("summoning a second mount during a guide must fail with ErrSummonPending, got %v", err)
	}
	if _, err := c.Advance(clockBase.Add(2000 * time.Millisecond)); err != nil {
		t.Fatalf("advance past the guide: %v", err)
	}
	if err := c.MountUp("p1", "m1"); err != nil {
		t.Fatalf("mount up: %v", err)
	}
	if _, err := c.Summon("p1", "m2"); !errors.Is(err, impl.ErrAlreadyMounted) {
		t.Fatalf("summoning while mounted must fail with ErrAlreadyMounted, got %v", err)
	}
}

func TestDismountValidation(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustMount(t, c, "p1", "m1", 2000*time.Millisecond)

	if err := c.Dismount("p1", "   "); !errors.Is(err, impl.ErrBadReason) {
		t.Fatalf("dismount without a reason must fail with ErrBadReason, got %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Phase != impl.PhaseMounted {
		t.Fatalf("a rejected dismount changed the phase to %q, want %q", st.Phase, impl.PhaseMounted)
	}
	if err := c.Dismount("p9", "leave"); !errors.Is(err, impl.ErrNotMounted) {
		t.Fatalf("dismount of an idle player must fail with ErrNotMounted, got %v", err)
	}
}

func TestDismountReturnsToIdle(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustMount(t, c, "p1", "m1", 2000*time.Millisecond)

	if err := c.Dismount("p1", "leave"); err != nil {
		t.Fatalf("dismount: %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state after dismount: %v", err)
	}
	if st.Phase != impl.PhaseIdle {
		t.Fatalf("phase after dismount = %q, want %q", st.Phase, impl.PhaseIdle)
	}
	if st.CooldownLeftMs != 0 {
		t.Fatalf("a voluntary dismount left %d ms of cooldown, want none", st.CooldownLeftMs)
	}
	events := c.Events("p1")
	last := events[len(events)-1]
	if last.Kind != impl.EventDismount || last.Reason != "leave" {
		t.Fatalf("last event = %+v, want a dismount carrying the cause", last)
	}
	if _, err := c.Summon("p1", "m1"); err != nil {
		t.Fatalf("summon after a voluntary dismount must not be blocked, got %v", err)
	}
}

func TestZoneRuleForbidsSummon(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))

	if err := c.SetZoneRule("", impl.ZoneRule{ForbidSummon: true}); !errors.Is(err, impl.ErrBadZone) {
		t.Fatalf("blank zone must fail with ErrBadZone, got %v", err)
	}
	if err := c.SetZoneRule("arena", impl.ZoneRule{ForbidMount: true, ForbidSummon: true}); err != nil {
		t.Fatalf("set zone rule: %v", err)
	}
	if _, err := c.EnterZone("p1", "arena"); err != nil {
		t.Fatalf("enter zone: %v", err)
	}
	if _, err := c.Summon("p1", "m1"); !errors.Is(err, impl.ErrZoneForbidsSummon) {
		t.Fatalf("summon inside a closed zone must fail with ErrZoneForbidsSummon, got %v", err)
	}
	if err := c.SetZoneRule("field", impl.ZoneRule{}); err != nil {
		t.Fatalf("set open zone rule: %v", err)
	}
	if _, err := c.EnterZone("p1", "field"); err != nil {
		t.Fatalf("enter open zone: %v", err)
	}
	if _, err := c.Summon("p1", "m1"); err != nil {
		t.Fatalf("summon in an open zone must be accepted, got %v", err)
	}
}

func TestEnterZoneForcesDismount(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustMount(t, c, "p1", "m1", 2000*time.Millisecond)
	if err := c.SetZoneRule("arena", impl.ZoneRule{ForbidMount: true}); err != nil {
		t.Fatalf("set zone rule: %v", err)
	}

	events, err := c.EnterZone("p1", "arena")
	if err != nil {
		t.Fatalf("enter zone: %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state after entering the closed zone: %v", err)
	}
	if st.Phase != impl.PhaseIdle {
		t.Fatalf("phase after entering a closed zone = %q, want %q", st.Phase, impl.PhaseIdle)
	}
	if len(events) != 1 {
		t.Fatalf("entering a closed zone produced %d events, want 1", len(events))
	}
	if events[0].Kind != impl.EventDismount || events[0].Reason != impl.ReasonZoneForbidden {
		t.Fatalf("zone event = %+v, want dismount/zone_forbidden", events[0])
	}
	if last := c.Events("p1"); last[len(last)-1].Reason != impl.ReasonZoneForbidden {
		t.Fatalf("the forced dismount was not recorded in the event log")
	}
}

func TestDamageBreaksMountAndStartsCooldown(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustMount(t, c, "p1", "m1", 2000*time.Millisecond)

	d, err := c.Damage("m1", 10)
	if err != nil {
		t.Fatalf("damage: %v", err)
	}
	if d.Current != 20 || d.Max != 30 || d.Broken {
		t.Fatalf("durability after 10 damage = %+v, want 20/30 not broken", d)
	}
	d, err = c.Damage("m1", 20)
	if err != nil {
		t.Fatalf("damage to zero: %v", err)
	}
	if d.Current != 0 || !d.Broken {
		t.Fatalf("durability after breaking = %+v, want 0 and broken", d)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state after the mount broke: %v", err)
	}
	if st.Phase != impl.PhaseCooldown {
		t.Fatalf("phase after the mount broke = %q, want %q", st.Phase, impl.PhaseCooldown)
	}
	if st.CooldownLeftMs != 5000 {
		t.Fatalf("cooldown after the mount broke = %d ms, want the registered 5000", st.CooldownLeftMs)
	}
	foundBroken := false
	for _, ev := range c.Events("p1") {
		if ev.Kind == impl.EventMountBroken {
			foundBroken = true
		}
	}
	if !foundBroken {
		t.Fatalf("breaking the mount did not append a mount_broken event")
	}
}

func TestDamageRejectsInvalidAmount(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustMount(t, c, "p1", "m1", 2000*time.Millisecond)

	if _, err := c.Damage("m1", 0); !errors.Is(err, impl.ErrBadAmount) {
		t.Fatalf("zero damage must fail with ErrBadAmount, got %v", err)
	}
	if _, err := c.Damage("m1", -7); !errors.Is(err, impl.ErrBadAmount) {
		t.Fatalf("negative damage must fail with ErrBadAmount, got %v", err)
	}
	if _, err := c.Damage("m404", 5); !errors.Is(err, impl.ErrNoSummon) {
		t.Fatalf("damaging a mount without an active instance must fail with ErrNoSummon, got %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Durability != 30 {
		t.Fatalf("rejected damage changed the durability to %d, want 30", st.Durability)
	}
}

func TestSummonBlockedByCooldown(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	mustMount(t, c, "p1", "m1", 2000*time.Millisecond)
	if _, err := c.Damage("m1", 30); err != nil {
		t.Fatalf("break the mount: %v", err)
	}

	if _, err := c.Summon("p1", "m1"); !errors.Is(err, impl.ErrCooldownActive) {
		t.Fatalf("summon during the cooldown must fail with ErrCooldownActive, got %v", err)
	}
	if _, err := c.Advance(clockBase.Add(7100 * time.Millisecond)); err != nil {
		t.Fatalf("advance past the cooldown: %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state after the cooldown: %v", err)
	}
	if st.Phase != impl.PhaseIdle || st.CooldownLeftMs != 0 {
		t.Fatalf("state after the cooldown = %+v, want idle with no cooldown left", st)
	}
	if last := c.Events("p1"); last[len(last)-1].Kind != impl.EventCooldownEnd {
		t.Fatalf("the finished cooldown did not append a cooldown_end event")
	}
	if _, err := c.Summon("p1", "m1"); err != nil {
		t.Fatalf("summon after the cooldown must be accepted, got %v", err)
	}
}

func TestSnapshotRestoreKeepsCooldownAndCounter(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	first := mustSummon(t, c, "p1", "m1")
	if _, err := c.Advance(clockBase.Add(2000 * time.Millisecond)); err != nil {
		t.Fatalf("advance past the guide: %v", err)
	}
	if err := c.MountUp("p1", "m1"); err != nil {
		t.Fatalf("mount up: %v", err)
	}
	if _, err := c.Damage("m1", 30); err != nil {
		t.Fatalf("break the mount: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload is empty")
	}

	restored := impl.NewMountController(testLimits(), clockBase)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := restored.State("p1")
	if err != nil {
		t.Fatalf("state after restore: %v", err)
	}
	if st.Phase != impl.PhaseCooldown || st.CooldownLeftMs != 5000 {
		t.Fatalf("restored state = %+v, want the cooldown and its 5000 ms budget", st)
	}
	if _, err := restored.Advance(clockBase.Add(7200 * time.Millisecond)); err != nil {
		t.Fatalf("advance restored cooldown: %v", err)
	}
	second, err := restored.Summon("p1", "m1")
	if err != nil {
		t.Fatalf("summon after the restored cooldown: %v", err)
	}
	if second == first {
		t.Fatalf("summon after restore reused the id %q", first)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	c := newController()
	mustRegister(t, c, mountSpec("m1", "p1"))
	id := mustSummon(t, c, "p1", "m1")

	if err := c.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	st, err := c.State("p1")
	if err != nil {
		t.Fatalf("state after a rejected restore: %v", err)
	}
	if st.Phase != impl.PhaseSummoning || st.Summon != id {
		t.Fatalf("a rejected restore changed the live state to %+v", st)
	}
}
