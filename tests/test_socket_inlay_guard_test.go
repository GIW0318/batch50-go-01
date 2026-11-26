package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-a-17"
)

func newGuard(maxHistory int) *impl.SocketInlayGuard {
	return impl.NewSocketInlayGuard(impl.Limits{MaxItems: 8, MaxHistory: maxHistory})
}

func mustGem(t *testing.T, g *impl.SocketInlayGuard, spec impl.GemSpec) {
	t.Helper()
	if err := g.RegisterGem(spec); err != nil {
		t.Fatalf("register gem %q: %v", spec.ID, err)
	}
}

func mustItem(t *testing.T, g *impl.SocketInlayGuard, spec impl.ItemSpec) {
	t.Helper()
	if err := g.RegisterItem(spec); err != nil {
		t.Fatalf("register item %q: %v", spec.ID, err)
	}
}

func mustRule(t *testing.T, g *impl.SocketInlayGuard, tier string, rule impl.SocketRule) {
	t.Helper()
	if err := g.SetSocketRule(tier, rule); err != nil {
		t.Fatalf("set socket rule for %q: %v", tier, err)
	}
}

func mustGrant(t *testing.T, g *impl.SocketInlayGuard, player, gem string, n int) {
	t.Helper()
	if err := g.GrantGem(player, gem, n); err != nil {
		t.Fatalf("grant %d x %q to %q: %v", n, gem, player, err)
	}
}

func redGem() impl.GemSpec {
	return impl.GemSpec{ID: "g-red", Color: impl.ColorRed, SetID: "fire", Attack: 5, Defense: 2, Crit: 1, SetThreshold: 3}
}

func blueGem() impl.GemSpec {
	return impl.GemSpec{ID: "g-blue", Color: impl.ColorBlue, SetID: "ice", Attack: 3, Defense: 4, Crit: 0, SetThreshold: 3}
}

func TestRegisterItemValidatesSpec(t *testing.T) {
	g := impl.NewSocketInlayGuard(impl.Limits{MaxItems: 2, MaxHistory: 4})
	mustRule(t, g, "epic", impl.SocketRule{Tier: "epic", MaxSockets: 3, ExtractPolicy: impl.ExtractReturn})

	if err := g.RegisterItem(impl.ItemSpec{Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed}}); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("blank item id must fail with ErrBadItem, got %v", err)
	}
	if err := g.RegisterItem(impl.ItemSpec{ID: "no-sockets", Owner: "p1", Tier: "epic"}); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("an item without socket colours must fail with ErrBadItem, got %v", err)
	}
	tooMany := impl.ItemSpec{ID: "wide", Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed, impl.ColorBlue, impl.ColorGreen, impl.ColorYellow}}
	if err := g.RegisterItem(tooMany); !errors.Is(err, impl.ErrSocketLimit) {
		t.Fatalf("four sockets under a tier capped at three must fail with ErrSocketLimit, got %v", err)
	}
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed, impl.ColorBlue}})
	if err := g.RegisterItem(impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed}}); !errors.Is(err, impl.ErrItemExists) {
		t.Fatalf("duplicate item id must fail with ErrItemExists, got %v", err)
	}
	mustItem(t, g, impl.ItemSpec{ID: "shield", Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed}})
	err := g.RegisterItem(impl.ItemSpec{ID: "helm", Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed}})
	if !errors.Is(err, impl.ErrItemLimit) {
		t.Fatalf("third item under MaxItems 2 must fail with ErrItemLimit, got %v", err)
	}
}

func TestInlayRejectsColorMismatch(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustGem(t, g, blueGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p1", "g-blue", 2)

	if _, err := g.Inlay("p1", "sword", 0, "g-blue"); !errors.Is(err, impl.ErrColorMismatch) {
		t.Fatalf("a blue gem in a red socket must fail with ErrColorMismatch, got %v", err)
	}
	if got := g.GemCount("p1", "g-blue"); got != 2 {
		t.Fatalf("the rejected inlay consumed a gem, bag now holds %d", got)
	}
	sockets, err := g.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if len(sockets) != 1 || sockets[0].Gem != "" {
		t.Fatalf("sockets after the rejected inlay = %+v, want one empty socket", sockets)
	}
	if got := len(g.History("sword")); got != 0 {
		t.Fatalf("the rejected inlay wrote %d history entries, want 0", got)
	}
}

func TestInlayRejectsMissingGem(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})

	if _, err := g.Inlay("p1", "sword", 0, "g-red"); !errors.Is(err, impl.ErrGemNotFound) {
		t.Fatalf("inlaying a gem the player does not hold must fail with ErrGemNotFound, got %v", err)
	}
	if _, err := g.Inlay("p1", "sword", 0, "g-missing"); !errors.Is(err, impl.ErrUnknownGem) {
		t.Fatalf("inlaying an unregistered gem must fail with ErrUnknownGem, got %v", err)
	}
	sockets, err := g.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if sockets[0].Gem != "" {
		t.Fatalf("a rejected inlay filled the socket with %q", sockets[0].Gem)
	}
}

func TestInlayRejectsOccupiedSocket(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustGem(t, g, blueGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p1", "g-red", 2)

	if _, err := g.Inlay("p1", "sword", 0, "g-red"); err != nil {
		t.Fatalf("first inlay: %v", err)
	}
	if _, err := g.Inlay("p1", "sword", 0, "g-red"); !errors.Is(err, impl.ErrSocketOccupied) {
		t.Fatalf("a second inlay into the same socket must fail with ErrSocketOccupied, got %v", err)
	}
	if got := g.GemCount("p1", "g-red"); got != 1 {
		t.Fatalf("the rejected inlay consumed a gem, bag now holds %d", got)
	}
}

func TestInlayRejectsBadSocketIndex(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustRule(t, g, "epic", impl.SocketRule{Tier: "epic", MaxSockets: 3, ExtractPolicy: impl.ExtractReturn})
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "epic", SocketColors: []string{impl.ColorRed, impl.ColorBlue, impl.ColorGreen}})
	mustGrant(t, g, "p1", "g-red", 3)

	if _, err := g.Inlay("p1", "sword", -1, "g-red"); !errors.Is(err, impl.ErrSocketIndex) {
		t.Fatalf("a negative socket index must fail with ErrSocketIndex, got %v", err)
	}
	if _, err := g.Inlay("p1", "sword", 5, "g-red"); !errors.Is(err, impl.ErrSocketIndex) {
		t.Fatalf("a socket index past the item must fail with ErrSocketIndex, got %v", err)
	}
	mustRule(t, g, "epic", impl.SocketRule{Tier: "epic", MaxSockets: 2, ExtractPolicy: impl.ExtractReturn})
	if _, err := g.Inlay("p1", "sword", 2, "g-red"); !errors.Is(err, impl.ErrSocketLocked) {
		t.Fatalf("a socket closed by the tier cap must fail with ErrSocketLocked, got %v", err)
	}
	if got := g.GemCount("p1", "g-red"); got != 3 {
		t.Fatalf("the rejected inlays consumed gems, bag now holds %d", got)
	}
}

func TestInlayRejectsForeignItem(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p2", "g-red", 1)

	if _, err := g.Inlay("p2", "sword", 0, "g-red"); !errors.Is(err, impl.ErrNotOwner) {
		t.Fatalf("inlaying into another player's item must fail with ErrNotOwner, got %v", err)
	}
	if _, err := g.Inlay("p1", "nope", 0, "g-red"); !errors.Is(err, impl.ErrUnknownItem) {
		t.Fatalf("inlaying into an unregistered item must fail with ErrUnknownItem, got %v", err)
	}
	if _, err := g.Inlay("  ", "sword", 0, "g-red"); !errors.Is(err, impl.ErrBlankPlayer) {
		t.Fatalf("a blank player must fail with ErrBlankPlayer, got %v", err)
	}
}

func TestInlaySucceedsAndRecordsHistory(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p1", "g-red", 2)

	res, err := g.Inlay("p1", "sword", 0, "g-red")
	if err != nil {
		t.Fatalf("inlay: %v", err)
	}
	if res.ItemID != "sword" || res.Socket != 0 || res.Gem != "g-red" || res.SetID != "fire" {
		t.Fatalf("result = %+v, want the socket and gem identity", res)
	}
	if res.Seq <= 0 {
		t.Fatalf("result sequence = %d, want a positive number", res.Seq)
	}
	if got := g.GemCount("p1", "g-red"); got != 1 {
		t.Fatalf("bag after the inlay = %d, want 1", got)
	}
	sockets, err := g.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if sockets[0].Gem != "g-red" || sockets[0].Color != impl.ColorRed {
		t.Fatalf("socket = %+v, want the red socket holding g-red", sockets[0])
	}
	if res.Stats.Attack != 5 || res.Stats.Defense != 2 || res.Stats.Crit != 1 {
		t.Fatalf("stats in the result = %+v, want the gem contribution", res.Stats)
	}
	hist := g.History("sword")
	if len(hist) != 1 {
		t.Fatalf("history after the inlay = %d entries, want 1", len(hist))
	}
	if hist[0].Action != impl.ActionInlay || hist[0].GemID != "g-red" || hist[0].Socket != 0 {
		t.Fatalf("history entry = %+v, want an inlay of g-red into socket 0", hist[0])
	}
}

func TestExtractReturnsGemPerRule(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustRule(t, g, "rare", impl.SocketRule{Tier: "rare", MaxSockets: 1, ExtractPolicy: impl.ExtractReturn})
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p1", "g-red", 1)
	if _, err := g.Inlay("p1", "sword", 0, "g-red"); err != nil {
		t.Fatalf("inlay: %v", err)
	}

	gem, err := g.Extract("p1", "sword", 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if gem.ID != "g-red" || gem.Destroyed {
		t.Fatalf("extracted gem = %+v, want g-red returned to the bag", gem)
	}
	if got := g.GemCount("p1", "g-red"); got != 1 {
		t.Fatalf("bag after a returning extract = %d, want 1", got)
	}
	sockets, err := g.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if sockets[0].Gem != "" {
		t.Fatalf("socket still holds %q after extract", sockets[0].Gem)
	}
	if got := len(g.History("sword")); got != 2 {
		t.Fatalf("history after inlay plus extract = %d entries, want 2", got)
	}
}

func TestExtractDestroysGemPerRule(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustRule(t, g, "cursed", impl.SocketRule{Tier: "cursed", MaxSockets: 1, ExtractPolicy: impl.ExtractDestroy})
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "cursed", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p1", "g-red", 1)
	if _, err := g.Inlay("p1", "sword", 0, "g-red"); err != nil {
		t.Fatalf("inlay: %v", err)
	}

	gem, err := g.Extract("p1", "sword", 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !gem.Destroyed {
		t.Fatalf("extract under the destroy policy reported %+v, want a destroyed gem", gem)
	}
	if got := g.GemCount("p1", "g-red"); got != 0 {
		t.Fatalf("bag after a destroying extract = %d, want 0", got)
	}
	sockets, err := g.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if sockets[0].Gem != "" {
		t.Fatalf("socket still holds %q after extract", sockets[0].Gem)
	}
	hist := g.History("sword")
	if len(hist) != 2 || hist[1].Action != impl.ActionExtract {
		t.Fatalf("history after inlay plus extract = %+v, want an extract entry", hist)
	}
}

func TestExtractEmptySocketReturnsEmpty(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})

	gem, err := g.Extract("p1", "sword", 0)
	if !errors.Is(err, impl.ErrSocketEmpty) {
		t.Fatalf("extracting an empty socket must fail with ErrSocketEmpty, got %v", err)
	}
	if gem != (impl.Gem{}) {
		t.Fatalf("extracting an empty socket returned %+v, want a zero gem", gem)
	}
	if got := len(g.History("sword")); got != 0 {
		t.Fatalf("the rejected extract wrote %d history entries, want 0", got)
	}
}

func TestSummarySumsGemStats(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustGem(t, g, blueGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed, impl.ColorBlue}})
	mustGrant(t, g, "p1", "g-red", 1)
	mustGrant(t, g, "p1", "g-blue", 1)

	if _, err := g.Inlay("p1", "sword", 0, "g-red"); err != nil {
		t.Fatalf("inlay red: %v", err)
	}
	res, err := g.Inlay("p1", "sword", 1, "g-blue")
	if err != nil {
		t.Fatalf("inlay blue: %v", err)
	}
	if res.Stats.ActiveSets != 0 {
		t.Fatalf("two different sets reported %d active sets, want 0", res.Stats.ActiveSets)
	}
	sum, err := g.Summary("p1")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Attack != 8 || sum.Defense != 6 || sum.Crit != 1 {
		t.Fatalf("summary = %+v, want attack 8, defense 6, crit 1", sum)
	}
	if sum.ActiveSets != 0 {
		t.Fatalf("summary reported %d active sets, want 0 under both thresholds", sum.ActiveSets)
	}
}

func TestSetBecomesActiveAtThreshold(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, impl.GemSpec{ID: "g-fa", Color: impl.ColorRed, SetID: "fire", Attack: 4, SetThreshold: 2})
	mustGem(t, g, impl.GemSpec{ID: "g-fb", Color: impl.ColorBlue, SetID: "fire", Attack: 6, SetThreshold: 2})
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed, impl.ColorBlue}})
	mustGrant(t, g, "p1", "g-fa", 1)
	mustGrant(t, g, "p1", "g-fb", 1)

	first, err := g.Inlay("p1", "sword", 0, "g-fa")
	if err != nil {
		t.Fatalf("inlay first set gem: %v", err)
	}
	if first.SetCount != 1 || first.SetActive {
		t.Fatalf("one gem of a two piece set reported count %d active %v, want 1 and false", first.SetCount, first.SetActive)
	}
	second, err := g.Inlay("p1", "sword", 1, "g-fb")
	if err != nil {
		t.Fatalf("inlay second set gem: %v", err)
	}
	if second.SetCount != 2 || !second.SetActive {
		t.Fatalf("two gems of a two piece set reported count %d active %v, want 2 and true", second.SetCount, second.SetActive)
	}
	if second.Stats.ActiveSets != 1 {
		t.Fatalf("stats reported %d active sets, want 1", second.Stats.ActiveSets)
	}
	if want := 4 + 6 + impl.SetBonusAttack; second.Stats.Attack != want {
		t.Fatalf("attack with the set active = %d, want %d", second.Stats.Attack, want)
	}
	if second.Stats.Defense != impl.SetBonusDefense || second.Stats.Crit != impl.SetBonusCrit {
		t.Fatalf("set bonus defence/crit = %d/%d, want %d/%d", second.Stats.Defense, second.Stats.Crit, impl.SetBonusDefense, impl.SetBonusCrit)
	}
}

func TestHistoryCappedPerItem(t *testing.T) {
	g := newGuard(2)
	mustGem(t, g, redGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed, impl.ColorRed, impl.ColorRed}})
	mustGrant(t, g, "p1", "g-red", 3)

	seqs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		res, err := g.Inlay("p1", "sword", i, "g-red")
		if err != nil {
			t.Fatalf("inlay %d: %v", i, err)
		}
		seqs = append(seqs, res.Seq)
	}
	hist := g.History("sword")
	if len(hist) != 2 {
		t.Fatalf("retained history = %d entries, want the cap of 2", len(hist))
	}
	if hist[0].Seq != seqs[1] || hist[1].Seq != seqs[2] {
		t.Fatalf("retained sequences = [%d %d], want the newest window [%d %d]", hist[0].Seq, hist[1].Seq, seqs[1], seqs[2])
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("inlay sequence must be strictly increasing, got %v", seqs)
		}
	}
}

func TestSnapshotRestoreKeepsSocketsAndHistory(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustGem(t, g, blueGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed, impl.ColorBlue}})
	mustGrant(t, g, "p1", "g-red", 2)
	mustGrant(t, g, "p1", "g-blue", 2)
	if _, err := g.Inlay("p1", "sword", 0, "g-red"); err != nil {
		t.Fatalf("inlay red: %v", err)
	}
	if _, err := g.Inlay("p1", "sword", 1, "g-blue"); err != nil {
		t.Fatalf("inlay blue: %v", err)
	}
	if _, err := g.Extract("p1", "sword", 1); err != nil {
		t.Fatalf("extract blue: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	restored := impl.NewSocketInlayGuard(impl.Limits{MaxItems: 8, MaxHistory: 4})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	sockets, err := restored.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets after restore: %v", err)
	}
	if len(sockets) != 2 || sockets[0].Gem != "g-red" || sockets[1].Gem != "" {
		t.Fatalf("restored sockets = %+v, want red filled and blue empty", sockets)
	}
	if got := len(restored.History("sword")); got != 3 {
		t.Fatalf("restored history = %d entries, want 3", got)
	}
	if got := restored.GemCount("p1", "g-red"); got != 1 {
		t.Fatalf("restored bag holds %d red gems, want 1", got)
	}
	if _, err := restored.Inlay("p1", "sword", 1, "g-blue"); err != nil {
		t.Fatalf("inlay after restore: %v", err)
	}
	sum, err := restored.Summary("p1")
	if err != nil {
		t.Fatalf("summary after restore: %v", err)
	}
	if sum.Attack != 8 || sum.Defense != 6 {
		t.Fatalf("summary after restore = %+v, want attack 8 defence 6", sum)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	g := newGuard(4)
	mustGem(t, g, redGem())
	mustItem(t, g, impl.ItemSpec{ID: "sword", Owner: "p1", Tier: "rare", SocketColors: []string{impl.ColorRed}})
	mustGrant(t, g, "p1", "g-red", 1)
	if _, err := g.Inlay("p1", "sword", 0, "g-red"); err != nil {
		t.Fatalf("inlay: %v", err)
	}

	if err := g.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	sockets, err := g.Sockets("sword")
	if err != nil {
		t.Fatalf("sockets after a rejected restore: %v", err)
	}
	if sockets[0].Gem != "g-red" {
		t.Fatalf("a rejected restore cleared the socket, got %q", sockets[0].Gem)
	}
	if got := len(g.History("sword")); got != 1 {
		t.Fatalf("a rejected restore changed the history to %d entries, want 1", got)
	}
}
