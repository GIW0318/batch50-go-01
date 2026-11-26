package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-19"
)

var advClock = time.Unix(1700000000, 0)

func newGuard() *impl.ClassAdvancementGuard {
	return impl.NewClassAdvancementGuard(impl.Limits{MaxHistory: 4, CooldownMillis: 30000}, advClock)
}

func seedClasses(t *testing.T, g *impl.ClassAdvancementGuard) {
	t.Helper()
	for _, spec := range []impl.ClassSpec{
		{ID: "peasant", Skills: []string{"slash", "farm"}, AllowedEquip: []string{"hoe"}},
		{ID: "warrior", Skills: []string{"slash", "shieldwall"}, AllowedEquip: []string{"sword", "shield"}},
		{ID: "rogue", Skills: []string{"stab"}, AllowedEquip: []string{"dagger"}},
		{ID: "knight", Skills: []string{"slash", "lance"}, AllowedEquip: []string{"lance"}},
	} {
		if err := g.RegisterClass(spec); err != nil {
			t.Fatalf("register class %q: %v", spec.ID, err)
		}
	}
	for class, req := range map[string]impl.Requirement{
		"warrior": {MinLevel: 10, Prereq: []string{"peasant"}, Quests: []string{"q-trial"}},
		"rogue":   {MinLevel: 10, Prereq: []string{"peasant"}, Quests: []string{"q-trial"}},
		"knight":  {MinLevel: 20, Prereq: []string{"warrior"}, Quests: []string{"q-knight"}},
	} {
		if err := g.SetRequirement(class, req); err != nil {
			t.Fatalf("set requirement for %q: %v", class, err)
		}
	}
}

func mustEnroll(t *testing.T, g *impl.ClassAdvancementGuard, player, class string) {
	t.Helper()
	if err := g.Enroll(player, class); err != nil {
		t.Fatalf("enroll %q as %q: %v", player, class, err)
	}
}

func readyForWarrior(t *testing.T, g *impl.ClassAdvancementGuard) {
	t.Helper()
	seedClasses(t, g)
	mustEnroll(t, g, "p1", "peasant")
	if err := g.SetPlayerLevel("p1", 12); err != nil {
		t.Fatalf("set level: %v", err)
	}
	if err := g.SetQuestFlag("p1", "q-trial"); err != nil {
		t.Fatalf("set quest flag: %v", err)
	}
}

func mustState(t *testing.T, g *impl.ClassAdvancementGuard, player string) impl.PlayerState {
	t.Helper()
	st, err := g.State(player)
	if err != nil {
		t.Fatalf("state of %q: %v", player, err)
	}
	return st
}

func TestRegisterClassValidatesSpec(t *testing.T) {
	g := newGuard()

	if err := g.RegisterClass(impl.ClassSpec{Skills: []string{"slash"}}); !errors.Is(err, impl.ErrBadClass) {
		t.Fatalf("blank class id must fail with ErrBadClass, got %v", err)
	}
	if err := g.RegisterClass(impl.ClassSpec{ID: "peasant", Skills: []string{"farm"}}); err != nil {
		t.Fatalf("valid class must be accepted, got %v", err)
	}
	if err := g.RegisterClass(impl.ClassSpec{ID: "peasant", Skills: []string{"farm"}}); !errors.Is(err, impl.ErrClassExists) {
		t.Fatalf("duplicate class id must fail with ErrClassExists, got %v", err)
	}
	if err := g.SetRequirement("nobody", impl.Requirement{MinLevel: 1}); !errors.Is(err, impl.ErrUnknownClass) {
		t.Fatalf("a requirement for an unregistered class must fail with ErrUnknownClass, got %v", err)
	}
	if err := g.SetRequirement("peasant", impl.Requirement{MinLevel: -1}); !errors.Is(err, impl.ErrBadRequirement) {
		t.Fatalf("a negative level gate must fail with ErrBadRequirement, got %v", err)
	}
	if err := g.SetRequirement("peasant", impl.Requirement{MinLevel: 1}); err != nil {
		t.Fatalf("a valid requirement must be accepted, got %v", err)
	}
}

func TestAdvanceRejectsUnknownClass(t *testing.T) {
	g := newGuard()
	seedClasses(t, g)

	if _, err := g.Advance("   ", "warrior"); !errors.Is(err, impl.ErrBlankPlayer) {
		t.Fatalf("a blank player must fail with ErrBlankPlayer, got %v", err)
	}
	mustEnroll(t, g, "p1", "peasant")
	if _, err := g.Advance("p1", "nobody"); !errors.Is(err, impl.ErrUnknownClass) {
		t.Fatalf("advancing to an unregistered class must fail with ErrUnknownClass, got %v", err)
	}
	if _, err := g.Advance("p9", "warrior"); !errors.Is(err, impl.ErrUnknownPlayer) {
		t.Fatalf("advancing an enrolled player that does not exist must fail with ErrUnknownPlayer, got %v", err)
	}
	st := mustState(t, g, "p1")
	if st.Class != "peasant" {
		t.Fatalf("a rejected advancement changed the class to %q", st.Class)
	}
}

func TestPreviewListsMissingRequirements(t *testing.T) {
	g := newGuard()
	seedClasses(t, g)
	mustEnroll(t, g, "p1", "peasant")
	if err := g.SetPlayerLevel("p1", 5); err != nil {
		t.Fatalf("set level: %v", err)
	}

	plan, err := g.Preview("p1", "knight")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if plan.OK {
		t.Fatalf("a plan that misses every gate reported ok")
	}
	want := []string{"class:warrior", "level", "quest:q-knight"}
	if len(plan.Missing) != len(want) {
		t.Fatalf("missing entries = %v, want %v", plan.Missing, want)
	}
	for i, name := range want {
		if plan.Missing[i] != name {
			t.Fatalf("missing entries = %v, want the sorted list %v", plan.Missing, want)
		}
	}
	if plan.From != "peasant" || plan.To != "knight" {
		t.Fatalf("plan identity = %s -> %s, want peasant -> knight", plan.From, plan.To)
	}
	if plan.Refund != 0 || len(plan.Unequip) != 0 {
		t.Fatalf("plan of a bare character = refund %d unequip %v, want both empty", plan.Refund, plan.Unequip)
	}
	st := mustState(t, g, "p1")
	if st.Class != "peasant" || st.Level != 5 {
		t.Fatalf("preview changed the character to %+v", st)
	}
	if got := len(g.History("p1")); got != 0 {
		t.Fatalf("preview wrote %d history entries, want 0", got)
	}
}

func TestAdvanceRejectsMissingLevel(t *testing.T) {
	g := newGuard()
	seedClasses(t, g)
	mustEnroll(t, g, "p1", "peasant")
	if err := g.SetPlayerLevel("p1", 5); err != nil {
		t.Fatalf("set level: %v", err)
	}
	if err := g.SetQuestFlag("p1", "q-trial"); err != nil {
		t.Fatalf("set quest flag: %v", err)
	}

	if _, err := g.Advance("p1", "warrior"); !errors.Is(err, impl.ErrRequirementNotMet) {
		t.Fatalf("advancing below the level gate must fail with ErrRequirementNotMet, got %v", err)
	}
	st := mustState(t, g, "p1")
	if st.Class != "peasant" {
		t.Fatalf("a rejected advancement changed the class to %q", st.Class)
	}
	if got := len(g.History("p1")); got != 0 {
		t.Fatalf("a rejected advancement wrote %d history entries", got)
	}
}

func TestAdvanceRejectsMissingQuestFlag(t *testing.T) {
	g := newGuard()
	seedClasses(t, g)
	mustEnroll(t, g, "p1", "peasant")
	if err := g.SetPlayerLevel("p1", 12); err != nil {
		t.Fatalf("set level: %v", err)
	}

	if _, err := g.Advance("p1", "warrior"); !errors.Is(err, impl.ErrRequirementNotMet) {
		t.Fatalf("advancing without the quest flag must fail with ErrRequirementNotMet, got %v", err)
	}
	plan, err := g.Preview("p1", "warrior")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(plan.Missing) != 1 || plan.Missing[0] != "quest:q-trial" {
		t.Fatalf("missing entries = %v, want only the quest flag", plan.Missing)
	}
}

func TestAdvanceSucceedsAndChangesClass(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)

	res, err := g.Advance("p1", "warrior")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.From != "peasant" || res.To != "warrior" {
		t.Fatalf("result = %s -> %s, want peasant -> warrior", res.From, res.To)
	}
	if res.Seq <= 0 {
		t.Fatalf("result sequence = %d, want a positive number", res.Seq)
	}
	st := mustState(t, g, "p1")
	if st.Class != "warrior" {
		t.Fatalf("class after the advancement = %q, want warrior", st.Class)
	}
	if st.CooldownLeftMs != 30000 {
		t.Fatalf("cooldown after the advancement = %d ms, want 30000", st.CooldownLeftMs)
	}
	hist := g.History("p1")
	if len(hist) != 1 {
		t.Fatalf("history after the advancement = %d entries, want 1", len(hist))
	}
	if hist[0].From != "peasant" || hist[0].To != "warrior" || hist[0].Reverted {
		t.Fatalf("history entry = %+v, want a committed peasant -> warrior", hist[0])
	}
}

func TestAdvanceRefundsInvestedPoints(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	if err := g.SetSkillPoints("p1", 10); err != nil {
		t.Fatalf("set skill points: %v", err)
	}
	if err := g.LearnSkill("p1", "farm", 5); err != nil {
		t.Fatalf("learn farm: %v", err)
	}

	res, err := g.Advance("p1", "warrior")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Refunded != 5 {
		t.Fatalf("refunded points = %d, want the 5 invested in farm", res.Refunded)
	}
	st := mustState(t, g, "p1")
	if st.Points != 10 {
		t.Fatalf("available points after the refund = %d, want 5 unspent plus 5 refunded", st.Points)
	}
	if len(st.Skills) != 0 {
		t.Fatalf("skills after the refund = %+v, want an empty table", st.Skills)
	}
}

func TestAdvanceUnequipsRestrictedGear(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	for _, item := range []string{"plate", "sword"} {
		if err := g.Equip("p1", item); err != nil {
			t.Fatalf("equip %q: %v", item, err)
		}
	}
	if err := g.SetBagFree("p1", 2); err != nil {
		t.Fatalf("set bag free: %v", err)
	}

	res, err := g.Advance("p1", "warrior")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(res.Unequipped) != 1 || res.Unequipped[0] != "plate" {
		t.Fatalf("unequipped = %v, want only the plate the warrior cannot wear", res.Unequipped)
	}
	st := mustState(t, g, "p1")
	if len(st.Equipment) != 1 || st.Equipment[0] != "sword" {
		t.Fatalf("equipment after the advancement = %v, want only the sword", st.Equipment)
	}
	if st.BagFree != 1 {
		t.Fatalf("bag free slots = %d, want 1 after one item moved into the bag", st.BagFree)
	}
}

func TestLearnSkillRejectsOverBudget(t *testing.T) {
	g := newGuard()
	seedClasses(t, g)
	mustEnroll(t, g, "p1", "peasant")
	if err := g.SetSkillPoints("p1", 3); err != nil {
		t.Fatalf("set skill points: %v", err)
	}

	if err := g.LearnSkill("p1", "farm", 5); !errors.Is(err, impl.ErrNotEnoughPoints) {
		t.Fatalf("learning more than the available points must fail with ErrNotEnoughPoints, got %v", err)
	}
	if err := g.LearnSkill("p1", "farm", 0); !errors.Is(err, impl.ErrBadPoints) {
		t.Fatalf("learning zero points must fail with ErrBadPoints, got %v", err)
	}
	if err := g.LearnSkill("p1", "stab", 1); !errors.Is(err, impl.ErrUnknownSkill) {
		t.Fatalf("learning a skill outside the class must fail with ErrUnknownSkill, got %v", err)
	}
	st := mustState(t, g, "p1")
	if st.Points != 3 || len(st.Skills) != 0 {
		t.Fatalf("rejected learnings changed the character to %+v", st)
	}
	if err := g.LearnSkill("p1", "farm", 3); err != nil {
		t.Fatalf("learn within the budget: %v", err)
	}
	st = mustState(t, g, "p1")
	if st.Points != 0 || len(st.Skills) != 1 || st.Skills[0].Name != "farm" || st.Skills[0].Points != 3 {
		t.Fatalf("state after a valid learning = %+v, want three points in farm", st)
	}
}

func TestCooldownBlocksDifferentTarget(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	if _, err := g.Advance("p1", "warrior"); err != nil {
		t.Fatalf("first advancement: %v", err)
	}

	if _, err := g.Advance("p1", "rogue"); !errors.Is(err, impl.ErrCooldownActive) {
		t.Fatalf("a second advancement during the cooldown must fail with ErrCooldownActive, got %v", err)
	}
	st := mustState(t, g, "p1")
	if st.Class != "warrior" {
		t.Fatalf("the blocked advancement changed the class to %q", st.Class)
	}
	if got := len(g.History("p1")); got != 1 {
		t.Fatalf("history after the blocked advancement = %d entries, want 1", got)
	}
}

func TestCooldownExpiresWithClock(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	if _, err := g.Advance("p1", "warrior"); err != nil {
		t.Fatalf("first advancement: %v", err)
	}
	if got := g.CooldownLeft("p1"); got != 30000 {
		t.Fatalf("cooldown left = %d ms, want 30000", got)
	}

	if err := g.AdvanceClock(advClock.Add(30000 * time.Millisecond)); err != nil {
		t.Fatalf("advance the clock: %v", err)
	}
	if got := g.CooldownLeft("p1"); got != 0 {
		t.Fatalf("cooldown left after the window = %d ms, want 0", got)
	}
	res, err := g.Advance("p1", "rogue")
	if err != nil {
		t.Fatalf("advance after the cooldown: %v", err)
	}
	if res.From != "warrior" || res.To != "rogue" {
		t.Fatalf("second result = %s -> %s, want warrior -> rogue", res.From, res.To)
	}
	if err := g.AdvanceClock(advClock.Add(10000 * time.Millisecond)); !errors.Is(err, impl.ErrTimeWentBackwards) {
		t.Fatalf("moving the clock backwards must fail with ErrTimeWentBackwards, got %v", err)
	}
}

func TestRevertRestoresPreviousState(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	if err := g.SetSkillPoints("p1", 10); err != nil {
		t.Fatalf("set skill points: %v", err)
	}
	if err := g.LearnSkill("p1", "farm", 5); err != nil {
		t.Fatalf("learn farm: %v", err)
	}
	if err := g.Equip("p1", "plate"); err != nil {
		t.Fatalf("equip plate: %v", err)
	}
	if err := g.SetBagFree("p1", 1); err != nil {
		t.Fatalf("set bag free: %v", err)
	}
	if _, err := g.Advance("p1", "warrior"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	after := mustState(t, g, "p1")
	if after.Class != "warrior" || after.Points != 10 || after.BagFree != 0 || len(after.Equipment) != 0 {
		t.Fatalf("state after the advancement = %+v, want the warrior with the gear unequipped", after)
	}

	if err := g.Revert("p1"); err != nil {
		t.Fatalf("revert: %v", err)
	}
	st := mustState(t, g, "p1")
	if st.Class != "peasant" {
		t.Fatalf("class after the revert = %q, want peasant", st.Class)
	}
	if st.Points != 5 {
		t.Fatalf("points after the revert = %d, want the 5 that were invested again", st.Points)
	}
	if len(st.Skills) != 1 || st.Skills[0].Name != "farm" || st.Skills[0].Points != 5 {
		t.Fatalf("skills after the revert = %+v, want farm restored", st.Skills)
	}
	if len(st.Equipment) != 1 || st.Equipment[0] != "plate" {
		t.Fatalf("equipment after the revert = %v, want the plate back", st.Equipment)
	}
	if st.BagFree != 1 {
		t.Fatalf("bag free slots after the revert = %d, want 1", st.BagFree)
	}
	hist := g.History("p1")
	if len(hist) != 1 || !hist[0].Reverted {
		t.Fatalf("history after the revert = %+v, want the single transition marked reverted", hist)
	}
	if err := g.Revert("p1"); !errors.Is(err, impl.ErrNoTransition) {
		t.Fatalf("a second revert must fail with ErrNoTransition, got %v", err)
	}
}

func TestRefundSkillsReturnsInvestedPoints(t *testing.T) {
	g := newGuard()
	seedClasses(t, g)
	mustEnroll(t, g, "p1", "peasant")
	if err := g.SetSkillPoints("p1", 7); err != nil {
		t.Fatalf("set skill points: %v", err)
	}
	if err := g.LearnSkill("p1", "slash", 4); err != nil {
		t.Fatalf("learn slash: %v", err)
	}
	if err := g.LearnSkill("p1", "farm", 3); err != nil {
		t.Fatalf("learn farm: %v", err)
	}

	refunded, err := g.RefundSkills("p1")
	if err != nil {
		t.Fatalf("refund skills: %v", err)
	}
	if refunded != 7 {
		t.Fatalf("refunded points = %d, want every invested point", refunded)
	}
	st := mustState(t, g, "p1")
	if st.Points != 7 || len(st.Skills) != 0 {
		t.Fatalf("state after the refund = %+v, want 7 points and an empty skill table", st)
	}
	if st.Class != "peasant" {
		t.Fatalf("a skill refund changed the class to %q", st.Class)
	}
}

func TestSnapshotRestoreKeepsClassAndCooldown(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	if err := g.Equip("p1", "plate"); err != nil {
		t.Fatalf("equip plate: %v", err)
	}
	if err := g.SetBagFree("p1", 1); err != nil {
		t.Fatalf("set bag free: %v", err)
	}
	if _, err := g.Advance("p1", "warrior"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	restored := impl.NewClassAdvancementGuard(impl.Limits{MaxHistory: 4, CooldownMillis: 30000}, advClock.Add(-time.Hour))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st := mustState(t, restored, "p1")
	if st.Class != "warrior" || st.CooldownLeftMs != 30000 {
		t.Fatalf("restored state = %+v, want the warrior with the cooldown untouched", st)
	}
	if got := len(restored.History("p1")); got != 1 {
		t.Fatalf("restored history = %d entries, want 1", got)
	}
	if err := restored.AdvanceClock(advClock.Add(30000 * time.Millisecond)); err != nil {
		t.Fatalf("advance the restored clock: %v", err)
	}
	res, err := restored.Advance("p1", "rogue")
	if err != nil {
		t.Fatalf("advance after the restored cooldown: %v", err)
	}
	if res.From != "warrior" || res.To != "rogue" {
		t.Fatalf("restored advancement = %s -> %s, want warrior -> rogue", res.From, res.To)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	g := newGuard()
	readyForWarrior(t, g)
	if _, err := g.Advance("p1", "warrior"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if err := g.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	st := mustState(t, g, "p1")
	if st.Class != "warrior" || st.CooldownLeftMs != 30000 {
		t.Fatalf("a rejected restore changed the live character to %+v", st)
	}
	if got := len(g.History("p1")); got != 1 {
		t.Fatalf("a rejected restore changed the history to %d entries", got)
	}
}
