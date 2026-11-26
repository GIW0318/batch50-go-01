package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-a-01"
)

func testConfig(boards int) impl.Config {
	return impl.Config{Boards: boards, FloatWindow: 1, MaxRounds: 5}
}

func roster(n int) []impl.Player {
	players := make([]impl.Player, 0, n)
	for i := 1; i <= n; i++ {
		players = append(players, impl.Player{
			ID:     "p" + string(rune('0'+i)),
			Rating: 1700 - i*100,
		})
	}
	return players
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func TestRegisterRejectsDuplicateAndBlankPlayers(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(nil); !errors.Is(err, impl.ErrNoPlayers) {
		t.Fatalf("empty roster must fail with ErrNoPlayers, got %v", err)
	}
	dup := []impl.Player{{ID: "p1", Rating: 1600}, {ID: "p1", Rating: 1500}}
	if err := d.Register(dup); !errors.Is(err, impl.ErrDuplicatePlayer) {
		t.Fatalf("duplicate player ids must fail with ErrDuplicatePlayer, got %v", err)
	}
	blank := []impl.Player{{ID: "   ", Rating: 1500}}
	if err := d.Register(blank); !errors.Is(err, impl.ErrBlankPlayer) {
		t.Fatalf("blank player id must fail with ErrBlankPlayer, got %v", err)
	}
	if got := len(d.Standings()); got != 0 {
		t.Fatalf("rejected rosters must not be loaded, got %d standing rows", got)
	}
}

func TestRegisterIsClosedAfterFirstPairing(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if err := d.Register([]impl.Player{{ID: "late-1", Rating: 1750}}); !errors.Is(err, impl.ErrRosterLocked) {
		t.Fatalf("roster changes after the first pairing must fail with ErrRosterLocked, got %v", err)
	}
	if got := len(d.Standings()); got != 4 {
		t.Fatalf("standings rows = %d, want the original 4 players", got)
	}
}

func TestFirstRoundPairsTopHalfWithBottomHalf(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(6)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	ps, err := d.PairRound(1)
	if err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if len(ps) != 3 {
		t.Fatalf("round 1 has %d entries, want 3 tables", len(ps))
	}
	want := [][2]string{{"p1", "p4"}, {"p2", "p5"}, {"p3", "p6"}}
	for i, w := range want {
		if ps[i].Round != 1 {
			t.Fatalf("entry %d round = %d, want 1", i, ps[i].Round)
		}
		if ps[i].Table != i+1 {
			t.Fatalf("entry %d table = %d, want %d", i, ps[i].Table, i+1)
		}
		if ps[i].WhiteID != w[0] || ps[i].BlackID != w[1] {
			t.Fatalf("table %d = %s vs %s, want %s vs %s", i+1, ps[i].WhiteID, ps[i].BlackID, w[0], w[1])
		}
		if ps[i].Float != impl.FloatNone {
			t.Fatalf("table %d float = %q, want %q in the first round", i+1, ps[i].Float, impl.FloatNone)
		}
		if ps[i].ByeID != "" {
			t.Fatalf("table %d must not carry a bye, got %q", i+1, ps[i].ByeID)
		}
	}
}

func TestOddRosterGivesByeToLowestSeed(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(5)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	ps, err := d.PairRound(1)
	if err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if len(ps) != 3 {
		t.Fatalf("round 1 has %d entries, want two tables plus one bye", len(ps))
	}
	last := ps[len(ps)-1]
	if last.ByeID != "p5" {
		t.Fatalf("bye went to %q, want the lowest seed p5", last.ByeID)
	}
	if last.Table != 0 || last.WhiteID != "" || last.BlackID != "" {
		t.Fatalf("bye entry = %+v, want table 0 and no players", last)
	}
	if ps[0].WhiteID != "p1" || ps[0].BlackID != "p3" {
		t.Fatalf("table 1 = %s vs %s, want p1 vs p3", ps[0].WhiteID, ps[0].BlackID)
	}
	if ps[1].WhiteID != "p2" || ps[1].BlackID != "p4" {
		t.Fatalf("table 2 = %s vs %s, want p2 vs p4", ps[1].WhiteID, ps[1].BlackID)
	}
	rows := d.Standings()
	if len(rows) != 5 {
		t.Fatalf("standings rows = %d, want 5", len(rows))
	}
	if rows[0].PlayerID != "p5" {
		t.Fatalf("standings leader = %q, want the bye holder p5", rows[0].PlayerID)
	}
	if rows[0].Score != impl.PointBye {
		t.Fatalf("bye holder score = %v, want %v", rows[0].Score, impl.PointBye)
	}
	if rows[0].Byes != 1 {
		t.Fatalf("bye holder bye count = %d, want 1", rows[0].Byes)
	}
}

func TestSecondRoundAvoidsRepeatingAPairing(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); err != nil {
		t.Fatalf("report table 1: %v", err)
	}
	if err := d.ReportResult(1, 2, "p2", false); err != nil {
		t.Fatalf("report table 2: %v", err)
	}
	ps, err := d.PairRound(2)
	if err != nil {
		t.Fatalf("pair round 2: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("round 2 has %d entries, want 2 tables", len(ps))
	}
	got := map[string]bool{}
	for _, p := range ps {
		if p.ByeID != "" {
			t.Fatalf("round 2 must not create a bye, got %+v", p)
		}
		got[pairKey(p.WhiteID, p.BlackID)] = true
	}
	for _, want := range []string{pairKey("p1", "p2"), pairKey("p3", "p4")} {
		if !got[want] {
			t.Fatalf("round 2 pairings %v are missing %s", got, want)
		}
	}
	if got[pairKey("p1", "p3")] || got[pairKey("p2", "p4")] {
		t.Fatalf("round 2 repeated a round 1 pairing: %v", got)
	}
}

func TestPairRoundWaitsForPreviousResults(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if _, err := d.PairRound(2); !errors.Is(err, impl.ErrResultsPending) {
		t.Fatalf("pairing round 2 with an open round 1 must fail with ErrResultsPending, got %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); err != nil {
		t.Fatalf("report table 1: %v", err)
	}
	if _, err := d.PairRound(2); !errors.Is(err, impl.ErrResultsPending) {
		t.Fatalf("a single open table must still block round 2, got %v", err)
	}
}

func TestPairRoundRejectsRepeatedAndOutOfRangeRounds(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(0); !errors.Is(err, impl.ErrBadRound) {
		t.Fatalf("round 0 must fail with ErrBadRound, got %v", err)
	}
	if _, err := d.PairRound(6); !errors.Is(err, impl.ErrBadRound) {
		t.Fatalf("round 6 of a five round event must fail with ErrBadRound, got %v", err)
	}
	if _, err := d.PairRound(2); !errors.Is(err, impl.ErrRoundNotPaired) && !errors.Is(err, impl.ErrResultsPending) {
		t.Fatalf("round 2 before round 1 must be refused, got %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if _, err := d.PairRound(1); !errors.Is(err, impl.ErrRoundPaired) {
		t.Fatalf("pairing an already paired round must fail with ErrRoundPaired, got %v", err)
	}
}

func TestReportResultUpdatesStandings(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); err != nil {
		t.Fatalf("report a win: %v", err)
	}
	if err := d.ReportResult(1, 2, "", true); err != nil {
		t.Fatalf("report a draw: %v", err)
	}
	rows := d.Standings()
	if len(rows) != 4 {
		t.Fatalf("standings rows = %d, want 4", len(rows))
	}
	wantOrder := []string{"p1", "p2", "p4", "p3"}
	wantScore := map[string]float64{"p1": impl.PointWin, "p2": impl.PointDraw, "p4": impl.PointDraw, "p3": 0}
	for i, id := range wantOrder {
		if rows[i].PlayerID != id {
			t.Fatalf("rank %d = %s, want %s (order %v)", i+1, rows[i].PlayerID, id, wantOrder)
		}
		if rows[i].Rank != i+1 {
			t.Fatalf("row %s rank = %d, want %d", rows[i].PlayerID, rows[i].Rank, i+1)
		}
		if rows[i].Score != wantScore[id] {
			t.Fatalf("%s score = %v, want %v", id, rows[i].Score, wantScore[id])
		}
	}
	if len(rows[0].Opponents) != 1 || rows[0].Opponents[0] != "p3" {
		t.Fatalf("p1 opponents = %v, want [p3]", rows[0].Opponents)
	}
}

func TestReportResultIsIdempotentForTheSameOutcome(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := d.ReportResult(1, 1, "p1", false); err != nil {
			t.Fatalf("repeat report %d must be accepted idempotently, got %v", i, err)
		}
	}
	if err := d.ReportResult(1, 1, "p3", false); !errors.Is(err, impl.ErrResultConflict) {
		t.Fatalf("a conflicting report must fail with ErrResultConflict, got %v", err)
	}
	rows := d.Standings()
	if len(rows) == 0 {
		t.Fatalf("standings must not be empty after reporting")
	}
	for _, r := range rows {
		switch r.PlayerID {
		case "p1":
			if r.Score != impl.PointWin {
				t.Fatalf("p1 score = %v, want exactly one win point", r.Score)
			}
		case "p3":
			if r.Score != 0 {
				t.Fatalf("p3 score = %v, want 0 after the conflict", r.Score)
			}
		}
	}
}

func TestReportResultValidatesTableAndWinner(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); !errors.Is(err, impl.ErrRoundNotPaired) {
		t.Fatalf("reporting before pairing must fail with ErrRoundNotPaired, got %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if err := d.ReportResult(1, 9, "p1", false); !errors.Is(err, impl.ErrNoTable) {
		t.Fatalf("unknown table must fail with ErrNoTable, got %v", err)
	}
	if err := d.ReportResult(1, 1, "", false); !errors.Is(err, impl.ErrBadWinner) {
		t.Fatalf("a win without a winner must fail with ErrBadWinner, got %v", err)
	}
	if err := d.ReportResult(1, 1, "p4", false); !errors.Is(err, impl.ErrBadWinner) {
		t.Fatalf("a winner from another table must fail with ErrBadWinner, got %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", true); !errors.Is(err, impl.ErrBadWinner) {
		t.Fatalf("a draw carrying a winner must fail with ErrBadWinner, got %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); err != nil {
		t.Fatalf("a rejected report must leave the table open, got %v", err)
	}
}

func TestWithdrawRemovesPlayerFromLaterPairings(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(5)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if err := d.Withdraw("p2"); err != nil {
		t.Fatalf("withdraw p2: %v", err)
	}
	if err := d.Withdraw("p2"); !errors.Is(err, impl.ErrWithdrawn) {
		t.Fatalf("withdrawing twice must fail with ErrWithdrawn, got %v", err)
	}
	if err := d.Withdraw("p404"); !errors.Is(err, impl.ErrNotRegistered) {
		t.Fatalf("withdrawing an unknown player must fail with ErrNotRegistered, got %v", err)
	}
	ps, err := d.PairRound(1)
	if err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("round 1 has %d entries, want 2 tables out of the remaining 4 players", len(ps))
	}
	for _, p := range ps {
		if p.WhiteID == "p2" || p.BlackID == "p2" || p.ByeID == "p2" {
			t.Fatalf("withdrawn player still paired: %+v", p)
		}
	}
}

func TestPairRoundEnforcesBoardLimit(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(1))
	if err := d.Register(roster(4)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); !errors.Is(err, impl.ErrBoardLimit) {
		t.Fatalf("two tables on a single board event must fail with ErrBoardLimit, got %v", err)
	}
	if ps, err := d.Round(1); err == nil || len(ps) != 0 {
		t.Fatalf("a rejected pairing must not leave a round behind, got %d entries and %v", len(ps), err)
	}
}

func TestByeIsCountedOnceAndKeptInHistory(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(5)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	history := d.ByeHistory("p5")
	if len(history) != 1 || history[0] != 1 {
		t.Fatalf("p5 bye history = %v, want [1]", history)
	}
	if got := d.ByeHistory("p1"); len(got) != 0 {
		t.Fatalf("p1 bye history = %v, want empty", got)
	}
	if got := d.Floats("p1"); got != 0 {
		t.Fatalf("p1 float count = %d, want 0 in the first round", got)
	}
	rows := d.Standings()
	if len(rows) != 5 {
		t.Fatalf("standings rows = %d, want 5", len(rows))
	}
}

func TestSnapshotRestoreKeepsPairingsAndByes(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(5)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); err != nil {
		t.Fatalf("report table 1: %v", err)
	}
	if err := d.ReportResult(1, 2, "p2", false); err != nil {
		t.Fatalf("report table 2: %v", err)
	}
	before := d.Standings()
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewSwissPairingDirector(testConfig(4))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after := restored.Standings()
	if len(after) != len(before) {
		t.Fatalf("restored standings rows = %d, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i].PlayerID != before[i].PlayerID || after[i].Score != before[i].Score || after[i].Byes != before[i].Byes {
			t.Fatalf("row %d after restore = %+v, want %+v", i, after[i], before[i])
		}
	}
	if got := restored.ByeHistory("p5"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("restored bye history for p5 = %v, want [1]", got)
	}
	ps, err := restored.PairRound(2)
	if err != nil {
		t.Fatalf("pair round 2 after restore: %v", err)
	}
	if len(ps) != 3 {
		t.Fatalf("round 2 after restore has %d entries, want 3", len(ps))
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	d := impl.NewSwissPairingDirector(testConfig(4))
	if err := d.Register(roster(5)); err != nil {
		t.Fatalf("register roster: %v", err)
	}
	if _, err := d.PairRound(1); err != nil {
		t.Fatalf("pair round 1: %v", err)
	}
	if err := d.ReportResult(1, 1, "p1", false); err != nil {
		t.Fatalf("report table 1: %v", err)
	}
	if err := d.Restore([]byte("{\"rounds\": not-a-book")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	rows := d.Standings()
	if len(rows) != 5 {
		t.Fatalf("failed restore must keep the roster, got %d rows", len(rows))
	}
	ps, err := d.Round(1)
	if err != nil {
		t.Fatalf("round 1 after a failed restore: %v", err)
	}
	if len(ps) != 3 {
		t.Fatalf("round 1 after a failed restore has %d entries, want 3", len(ps))
	}
}
