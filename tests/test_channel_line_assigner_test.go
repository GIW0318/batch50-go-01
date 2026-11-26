package task

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-06"
)

var atOpen = time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)

func lineLimits() impl.Limits {
	return impl.Limits{
		MaxLinesPerMap:       4,
		MaxPartySize:         5,
		MaxMovesPerRebalance: 3,
		SwitchCooldown:       30 * time.Second,
		SoftRatio:            0.5,
	}
}

func mustOpenLine(t *testing.T, a *impl.ChannelLineAssigner, mapID string, capacity int) impl.LineState {
	t.Helper()
	st, err := a.OpenLine(mapID, capacity)
	if err != nil {
		t.Fatalf("OpenLine(%q, %d): unexpected error: %v", mapID, capacity, err)
	}
	return st
}

func mustEnter(t *testing.T, a *impl.ChannelLineAssigner, mapID, playerID, partyID string, at time.Time) impl.Assignment {
	t.Helper()
	as, err := a.Enter(mapID, playerID, partyID, at)
	if err != nil {
		t.Fatalf("Enter(%q, %q, %q): unexpected error: %v", mapID, playerID, partyID, err)
	}
	return as
}

func TestOpenLineRejectsNonPositiveCapacity(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	if _, err := a.OpenLine("map-a", 0); !errors.Is(err, impl.ErrInvalidCapacity) {
		t.Fatalf("OpenLine capacity 0: got %v, want ErrInvalidCapacity", err)
	}
	if _, err := a.OpenLine("map-a", -2); !errors.Is(err, impl.ErrInvalidCapacity) {
		t.Fatalf("OpenLine capacity -2: got %v, want ErrInvalidCapacity", err)
	}
}

func TestOpenLinesGetSequentialIDsAndSoftCaps(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	first := mustOpenLine(t, a, "map-a", 10)
	if first.LineID != "line-1" || first.Capacity != 10 || first.SoftCap != 5 {
		t.Fatalf("first line: got %+v, want id line-1 capacity 10 soft cap 5", first)
	}
	if first.Status != impl.LineOpen {
		t.Fatalf("first line status: got %q, want %q", first.Status, impl.LineOpen)
	}
	second := mustOpenLine(t, a, "map-a", 7)
	if second.LineID != "line-2" || second.SoftCap != 3 {
		t.Fatalf("second line: got %+v, want id line-2 soft cap 3", second)
	}
}

func TestOpenLineRespectsPerMapLineBudget(t *testing.T) {
	limits := lineLimits()
	limits.MaxLinesPerMap = 2
	a := impl.NewChannelLineAssigner(limits)
	mustOpenLine(t, a, "map-a", 4)
	mustOpenLine(t, a, "map-a", 4)
	if _, err := a.OpenLine("map-a", 4); !errors.Is(err, impl.ErrLineLimitReached) {
		t.Fatalf("third OpenLine: got %v, want ErrLineLimitReached", err)
	}
}

func TestEnterSeatsSoloPlayersOnLeastOccupiedLine(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 6)
	mustOpenLine(t, a, "map-a", 6)
	first := mustEnter(t, a, "map-a", "p1", "", atOpen)
	second := mustEnter(t, a, "map-a", "p2", "", atOpen)
	third := mustEnter(t, a, "map-a", "p3", "", atOpen)
	if first.LineID != "line-1" || second.LineID != "line-2" || third.LineID != "line-1" {
		t.Fatalf("solo spread: got %q %q %q, want line-1 line-2 line-1", first.LineID, second.LineID, third.LineID)
	}
	loc, ok := a.Locate("map-a", "p2")
	if !ok || loc.LineID != "line-2" {
		t.Fatalf("Locate(p2): got %+v ok=%v, want line-2", loc, ok)
	}
}

func TestEnterMayExceedSoftCapWithoutEvicting(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	mustEnter(t, a, "map-a", "p2", "", atOpen)
	as := mustEnter(t, a, "map-a", "p3", "", atOpen)
	if as.LineID != "line-1" {
		t.Fatalf("soft overflow: got line %q, want line-1", as.LineID)
	}
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines: unexpected error: %v", err)
	}
	if len(states) != 1 || states[0].Occupancy != 3 {
		t.Fatalf("occupancy after soft overflow: got %+v, want line-1 with 3 players", states)
	}
}

func TestEnterFailsWhenEveryLineIsAtHardCap(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 1)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	if _, err := a.Enter("map-a", "p2", "", atOpen); !errors.Is(err, impl.ErrLineFull) {
		t.Fatalf("Enter on full map: got %v, want ErrLineFull", err)
	}
}

func TestEnterSamePlayerTwiceKeepsOneSeat(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	first := mustEnter(t, a, "map-a", "p1", "", atOpen)
	second := mustEnter(t, a, "map-a", "p1", "", atOpen)
	if first.LineID != second.LineID {
		t.Fatalf("duplicate Enter moved player: got %q then %q", first.LineID, second.LineID)
	}
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines: unexpected error: %v", err)
	}
	if states[0].Occupancy != 1 {
		t.Fatalf("occupancy after duplicate Enter: got %d, want 1", states[0].Occupancy)
	}
}

func TestLeaveFreesSeatAndUnknownPlayerReportsError(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 1)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	if err := a.Leave("map-a", "p1"); err != nil {
		t.Fatalf("Leave(p1): unexpected error: %v", err)
	}
	mustEnter(t, a, "map-a", "p2", "", atOpen)
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines: unexpected error: %v", err)
	}
	if states[0].Occupancy != 1 {
		t.Fatalf("occupancy after leave and re-enter: got %d, want 1", states[0].Occupancy)
	}
	if err := a.Leave("map-a", "p9"); !errors.Is(err, impl.ErrPlayerNotOnMap) {
		t.Fatalf("Leave unknown player: got %v, want ErrPlayerNotOnMap", err)
	}
}

func TestSwitchEnforcesCooldownWindow(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	mustOpenLine(t, a, "map-a", 4)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	moved, err := a.Switch("map-a", "p1", "line-2", atOpen)
	if err != nil {
		t.Fatalf("Switch(line-2): unexpected error: %v", err)
	}
	if moved.LineID != "line-2" {
		t.Fatalf("Switch target: got %q, want line-2", moved.LineID)
	}
	wantUntil := atOpen.Add(30 * time.Second)
	if !moved.CooldownUntil.Equal(wantUntil) {
		t.Fatalf("cooldown until: got %v, want %v", moved.CooldownUntil, wantUntil)
	}
	if _, err := a.Switch("map-a", "p1", "line-1", atOpen.Add(10*time.Second)); !errors.Is(err, impl.ErrSwitchCooling) {
		t.Fatalf("Switch inside cooldown: got %v, want ErrSwitchCooling", err)
	}
	if _, err := a.Switch("map-a", "p1", "line-1", wantUntil); err != nil {
		t.Fatalf("Switch after cooldown: unexpected error: %v", err)
	}
}

func TestSwitchRejectedWhileMapLocked(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	mustOpenLine(t, a, "map-a", 4)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	if err := a.LockSwitching("map-a", true); err != nil {
		t.Fatalf("LockSwitching: unexpected error: %v", err)
	}
	locked, err := a.IsLocked("map-a")
	if err != nil {
		t.Fatalf("IsLocked: unexpected error: %v", err)
	}
	if !locked {
		t.Fatalf("IsLocked: got false, want true after locking")
	}
	if _, err := a.Switch("map-a", "p1", "line-2", atOpen); !errors.Is(err, impl.ErrSwitchLocked) {
		t.Fatalf("Switch while locked: got %v, want ErrSwitchLocked", err)
	}
}

func TestCloseLineEvictsPlayersInSortedOrder(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	mustEnter(t, a, "map-a", "p3", "", atOpen)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	mustEnter(t, a, "map-a", "p2", "", atOpen)
	evicted, err := a.CloseLine("map-a", "line-1", atOpen.Add(time.Minute))
	if err != nil {
		t.Fatalf("CloseLine: unexpected error: %v", err)
	}
	if !sort.StringsAreSorted(evicted) {
		t.Fatalf("evicted players not sorted: %v", evicted)
	}
	want := []string{"p1", "p2", "p3"}
	if len(evicted) != len(want) {
		t.Fatalf("evicted players: got %v, want %v", evicted, want)
	}
	for i := range want {
		if evicted[i] != want[i] {
			t.Fatalf("evicted players: got %v, want %v", evicted, want)
		}
	}
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines: unexpected error: %v", err)
	}
	if states[0].Status != impl.LineClosed || states[0].Occupancy != 0 {
		t.Fatalf("closed line state: got %+v, want closed with zero occupancy", states[0])
	}
	if _, ok := a.Locate("map-a", "p1"); ok {
		t.Fatalf("Locate(p1) after CloseLine: got ok=true, want player evicted")
	}
}

func TestRebalanceStopsAtMoveBudget(t *testing.T) {
	limits := lineLimits()
	limits.MaxMovesPerRebalance = 1
	a := impl.NewChannelLineAssigner(limits)
	mustOpenLine(t, a, "map-a", 4)
	mustOpenLine(t, a, "map-a", 4)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	mustEnter(t, a, "map-a", "p2", "", atOpen)
	mustEnter(t, a, "map-a", "p3", "", atOpen)
	mustEnter(t, a, "map-a", "p4", "", atOpen)
	for _, id := range []string{"p1", "p3"} {
		if _, err := a.Switch("map-a", id, "line-2", atOpen); err != nil {
			t.Fatalf("Switch(%s, line-2): unexpected error: %v", id, err)
		}
	}
	moves, err := a.Rebalance("map-a", atOpen.Add(time.Minute))
	if err != nil {
		t.Fatalf("Rebalance: unexpected error: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("moves under budget 1: got %d, want 1 (%+v)", len(moves), moves)
	}
	if moves[0].FromLine != "line-2" || moves[0].ToLine != "line-1" || moves[0].PlayerID != "p1" {
		t.Fatalf("move: got %+v, want p1 line-2 -> line-1", moves[0])
	}
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines: unexpected error: %v", err)
	}
	if states[1].Occupancy != 3 {
		t.Fatalf("line-2 occupancy after one move: got %d, want 3", states[1].Occupancy)
	}
}

func TestSnapshotRestoreKeepsSeatingAndLock(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	mustEnter(t, a, "map-a", "p1", "", atOpen)
	if err := a.LockSwitching("map-a", true); err != nil {
		t.Fatalf("LockSwitching: unexpected error: %v", err)
	}
	data, err := a.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Snapshot: got empty payload")
	}
	b := impl.NewChannelLineAssigner(lineLimits())
	if err := b.Restore(data); err != nil {
		t.Fatalf("Restore: unexpected error: %v", err)
	}
	loc, ok := b.Locate("map-a", "p1")
	if !ok || loc.LineID != "line-1" {
		t.Fatalf("Locate after Restore: got %+v ok=%v, want line-1", loc, ok)
	}
	states, err := b.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines after Restore: unexpected error: %v", err)
	}
	if states[0].Occupancy != 1 {
		t.Fatalf("occupancy after Restore: got %d, want 1", states[0].Occupancy)
	}
	locked, err := b.IsLocked("map-a")
	if err != nil {
		t.Fatalf("IsLocked after Restore: unexpected error: %v", err)
	}
	if !locked {
		t.Fatalf("IsLocked after Restore: got false, want true")
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 4)
	if err := a.Restore([]byte("{not-json")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore malformed payload: got %v, want ErrBadSnapshot", err)
	}
	if err := a.Restore(nil); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore empty payload: got %v, want ErrBadSnapshot", err)
	}
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines after rejected Restore: unexpected error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("lines after rejected Restore: got %d, want 1 untouched line", len(states))
	}
}

func TestConcurrentEnterAndLeaveKeepCountsExact(t *testing.T) {
	a := impl.NewChannelLineAssigner(lineLimits())
	mustOpenLine(t, a, "map-a", 200)
	var wg sync.WaitGroup
	errCh := make(chan error, 200)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			playerID := fmt.Sprintf("p%03d", i)
			if _, err := a.Enter("map-a", playerID, "", atOpen); err != nil {
				errCh <- fmt.Errorf("Enter(%s): %w", playerID, err)
				return
			}
			if err := a.Leave("map-a", playerID); err != nil {
				errCh <- fmt.Errorf("Leave(%s): %w", playerID, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent enter and leave: %v", err)
	}
	states, err := a.Lines("map-a")
	if err != nil {
		t.Fatalf("Lines: unexpected error: %v", err)
	}
	if states[0].Occupancy != 0 {
		t.Fatalf("occupancy after every player left: got %d, want 0", states[0].Occupancy)
	}
}
