package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-02"
)

func testLimits() impl.Limits {
	return impl.Limits{MaxSeats: 4, MaxPassengers: 2, GlideSeconds: 60, MaxEventsKept: 16}
}

func seatPlan() []impl.SeatSpec {
	return []impl.SeatSpec{
		{Index: 0, Role: impl.RoleDriver},
		{Index: 1, Role: impl.RoleGunner},
		{Index: 2, Role: impl.RolePassenger},
		{Index: 3, Role: impl.RolePassenger},
	}
}

func spawnOne(t *testing.T, g *impl.VehicleBoardingGuard, vehicleID string) {
	t.Helper()
	if err := g.Spawn(vehicleID, seatPlan()); err != nil {
		t.Fatalf("spawn %s: %v", vehicleID, err)
	}
}

func TestSpawnRegistersVehicleAndRejectsDuplicates(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if got := len(g.Occupants("veh-1")); got != 0 {
		t.Fatalf("fresh vehicle has %d occupants, want 0", got)
	}
	if err := g.Spawn("veh-1", seatPlan()); !errors.Is(err, impl.ErrVehicleExists) {
		t.Fatalf("spawning the same id twice must fail with ErrVehicleExists, got %v", err)
	}
	dup := []impl.SeatSpec{{Index: 0, Role: impl.RoleDriver}, {Index: 0, Role: impl.RolePassenger}}
	if err := g.Spawn("veh-2", dup); !errors.Is(err, impl.ErrBadSeat) {
		t.Fatalf("duplicate seat indexes must fail with ErrBadSeat, got %v", err)
	}
	if _, err := g.SeatOf("veh-404", "p1"); !errors.Is(err, impl.ErrVehicleNotFound) {
		t.Fatalf("seats of an unknown vehicle must fail with ErrVehicleNotFound, got %v", err)
	}
}

func TestSpawnValidatesSeatLayout(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	noDriver := []impl.SeatSpec{{Index: 0, Role: impl.RoleGunner}, {Index: 1, Role: impl.RolePassenger}}
	if err := g.Spawn("veh-1", noDriver); !errors.Is(err, impl.ErrNoDriverSeat) {
		t.Fatalf("a layout without a driver seat must fail with ErrNoDriverSeat, got %v", err)
	}
	twoDrivers := []impl.SeatSpec{{Index: 0, Role: impl.RoleDriver}, {Index: 1, Role: impl.RoleDriver}}
	if err := g.Spawn("veh-2", twoDrivers); !errors.Is(err, impl.ErrBadSeat) {
		t.Fatalf("two driver seats must fail with ErrBadSeat, got %v", err)
	}
	badRole := []impl.SeatSpec{{Index: 0, Role: impl.RoleDriver}, {Index: 1, Role: "pilot"}}
	if err := g.Spawn("veh-3", badRole); !errors.Is(err, impl.ErrBadSeat) {
		t.Fatalf("an unknown seat role must fail with ErrBadSeat, got %v", err)
	}
	tooMany := []impl.SeatSpec{
		{Index: 0, Role: impl.RoleDriver},
		{Index: 1, Role: impl.RoleGunner},
		{Index: 2, Role: impl.RolePassenger},
		{Index: 3, Role: impl.RolePassenger},
		{Index: 4, Role: impl.RolePassenger},
	}
	if err := g.Spawn("veh-4", tooMany); !errors.Is(err, impl.ErrSeatLimit) {
		t.Fatalf("more seats than MaxSeats must fail with ErrSeatLimit, got %v", err)
	}
}

func TestBoardClaimsSeatAndReportsRole(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	b, err := g.Board("veh-1", "p1", 0)
	if err != nil {
		t.Fatalf("board the driver seat: %v", err)
	}
	if b.VehicleID != "veh-1" || b.PlayerID != "p1" || b.SeatIndex != 0 {
		t.Fatalf("boarding receipt = %+v, want veh-1/p1/seat 0", b)
	}
	if b.Role != impl.RoleDriver {
		t.Fatalf("seat 0 role = %q, want %q", b.Role, impl.RoleDriver)
	}
	if b.Seq <= 0 {
		t.Fatalf("boarding sequence = %d, want a positive number", b.Seq)
	}
	if b.Reconnected {
		t.Fatalf("a fresh boarding must not be flagged as a reconnect")
	}
	state, err := g.SeatOf("veh-1", "p1")
	if err != nil {
		t.Fatalf("seat of p1: %v", err)
	}
	if !state.Occupied || state.SeatIndex != 0 || state.Role != impl.RoleDriver {
		t.Fatalf("seat state = %+v, want an occupied driver seat at index 0", state)
	}
	occupants := g.Occupants("veh-1")
	if len(occupants) != 1 || occupants[0].PlayerID != "p1" {
		t.Fatalf("occupants = %+v, want only p1", occupants)
	}
	glide, err := g.GlideState("veh-1")
	if err != nil {
		t.Fatalf("glide state: %v", err)
	}
	if glide.Gliding {
		t.Fatalf("a vehicle with a driver must not be gliding")
	}
}

func TestBoardRejectsTakenSeat(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 1); err != nil {
		t.Fatalf("board the gunner seat: %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 1); !errors.Is(err, impl.ErrSeatTaken) {
		t.Fatalf("boarding a taken seat must fail with ErrSeatTaken, got %v", err)
	}
	if got := len(g.Occupants("veh-1")); got != 1 {
		t.Fatalf("occupants after a refused boarding = %d, want 1", got)
	}
	if _, err := g.SeatOf("veh-1", "p2"); !errors.Is(err, impl.ErrNotOnBoard) {
		t.Fatalf("a refused player must not appear on the vehicle, got %v", err)
	}
}

func TestBoardRejectsInvalidSeatAndVehicle(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 9); !errors.Is(err, impl.ErrBadSeat) {
		t.Fatalf("out of range seat must fail with ErrBadSeat, got %v", err)
	}
	if _, err := g.Board("veh-1", "p1", -1); !errors.Is(err, impl.ErrBadSeat) {
		t.Fatalf("negative seat index must fail with ErrBadSeat, got %v", err)
	}
	if _, err := g.Board("veh-404", "p1", 0); !errors.Is(err, impl.ErrVehicleNotFound) {
		t.Fatalf("boarding an unknown vehicle must fail with ErrVehicleNotFound, got %v", err)
	}
	if _, err := g.Board("veh-1", "  ", 0); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("a blank player id must fail with ErrBadPlayer, got %v", err)
	}
}

func TestBoardEnforcesPassengerLimit(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(impl.Limits{MaxSeats: 4, MaxPassengers: 1, GlideSeconds: 60, MaxEventsKept: 16})
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 0); err != nil {
		t.Fatalf("board the driver: %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 1); err != nil {
		t.Fatalf("the gunner must not count against the passenger cap: %v", err)
	}
	if _, err := g.Board("veh-1", "p3", 2); err != nil {
		t.Fatalf("the first passenger must fit: %v", err)
	}
	if _, err := g.Board("veh-1", "p4", 3); !errors.Is(err, impl.ErrPassengerLimit) {
		t.Fatalf("a second passenger must fail with ErrPassengerLimit, got %v", err)
	}
	if got := len(g.Occupants("veh-1")); got != 3 {
		t.Fatalf("occupants = %d, want the 3 accepted players", got)
	}
}

func TestLeaveReleasesSeat(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 2); err != nil {
		t.Fatalf("board p1: %v", err)
	}
	if err := g.Leave("veh-1", "p1"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := g.SeatOf("veh-1", "p1"); !errors.Is(err, impl.ErrNotOnBoard) {
		t.Fatalf("a player who left must be gone from the seat table, got %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 2); err != nil {
		t.Fatalf("the released seat must be reusable, got %v", err)
	}
}

func TestLeaveRejectsPlayersThatAreNotOnBoard(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if err := g.Leave("veh-1", "p404"); !errors.Is(err, impl.ErrNotOnBoard) {
		t.Fatalf("leaving a vehicle you never boarded must fail with ErrNotOnBoard, got %v", err)
	}
	if err := g.Leave("veh-404", "p1"); !errors.Is(err, impl.ErrVehicleNotFound) {
		t.Fatalf("leaving an unknown vehicle must fail with ErrVehicleNotFound, got %v", err)
	}
	if err := g.Leave("veh-1", ""); !errors.Is(err, impl.ErrBadPlayer) {
		t.Fatalf("a blank player id must fail with ErrBadPlayer, got %v", err)
	}
}

func TestTransferDriverSwapsSeats(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 0); err != nil {
		t.Fatalf("board p1: %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 2); err != nil {
		t.Fatalf("board p2: %v", err)
	}
	if err := g.TransferDriver("veh-1", "p1", "p2"); err != nil {
		t.Fatalf("transfer the driving right: %v", err)
	}
	driver, err := g.SeatOf("veh-1", "p2")
	if err != nil {
		t.Fatalf("seat of p2: %v", err)
	}
	if driver.SeatIndex != 0 || driver.Role != impl.RoleDriver {
		t.Fatalf("p2 seat = %+v, want the driver seat 0", driver)
	}
	old, err := g.SeatOf("veh-1", "p1")
	if err != nil {
		t.Fatalf("seat of p1: %v", err)
	}
	if old.SeatIndex != 2 || old.Role != impl.RolePassenger {
		t.Fatalf("p1 seat = %+v, want the passenger seat 2", old)
	}
	if got := len(g.Occupants("veh-1")); got != 2 {
		t.Fatalf("occupants after the transfer = %d, want 2", got)
	}
	if err := g.TransferDriver("veh-1", "p1", "p2"); !errors.Is(err, impl.ErrNotDriver) {
		t.Fatalf("the old driver must not transfer again, got %v", err)
	}
}

func TestTransferDriverRejectsBadRequests(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 0); err != nil {
		t.Fatalf("board p1: %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 1); err != nil {
		t.Fatalf("board p2: %v", err)
	}
	if err := g.TransferDriver("veh-1", "p2", "p1"); !errors.Is(err, impl.ErrNotDriver) {
		t.Fatalf("a passenger must not hand over the driving right, got %v", err)
	}
	if err := g.TransferDriver("veh-1", "p1", "p404"); !errors.Is(err, impl.ErrNotOnBoard) {
		t.Fatalf("transferring to a player outside the vehicle must fail with ErrNotOnBoard, got %v", err)
	}
	if err := g.TransferDriver("veh-404", "p1", "p2"); !errors.Is(err, impl.ErrVehicleNotFound) {
		t.Fatalf("transferring on an unknown vehicle must fail with ErrVehicleNotFound, got %v", err)
	}
	if err := g.TransferDriver("veh-1", "p1", "p1"); !errors.Is(err, impl.ErrNotOnBoard) && !errors.Is(err, impl.ErrNotDriver) {
		t.Fatalf("transferring to the current driver must be refused, got %v", err)
	}
}

func TestForceDismountClearsEverySeatOnce(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	for i, player := range []string{"p1", "p2", "p3"} {
		if _, err := g.Board("veh-1", player, i); err != nil {
			t.Fatalf("board %s: %v", player, err)
		}
	}
	got, err := g.ForceDismount("veh-1", impl.ReasonDestroyed)
	if err != nil {
		t.Fatalf("force dismount: %v", err)
	}
	want := []string{"p1", "p2", "p3"}
	if len(got) != len(want) {
		t.Fatalf("dismounted players = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dismounted players = %v, want %v in seat order", got, want)
		}
	}
	if n := len(g.Occupants("veh-1")); n != 0 {
		t.Fatalf("occupants after the destroy = %d, want 0", n)
	}
	events := g.Events("veh-1")
	if len(events) != 3 {
		t.Fatalf("dismount events = %d, want one per pushed player", len(events))
	}
	seen := map[string]int{}
	for _, ev := range events {
		if ev.Reason != impl.ReasonDestroyed {
			t.Fatalf("event reason = %q, want %q", ev.Reason, impl.ReasonDestroyed)
		}
		if ev.Seq <= 0 {
			t.Fatalf("event sequence = %d, want a positive number", ev.Seq)
		}
		seen[ev.PlayerID]++
	}
	for _, player := range want {
		if seen[player] != 1 {
			t.Fatalf("player %s appears %d times in the event log, want exactly once", player, seen[player])
		}
	}
	if _, err := g.SeatOf("veh-1", "p1"); !errors.Is(err, impl.ErrNotOnBoard) {
		t.Fatalf("a destroyed vehicle must not keep seat state, got %v", err)
	}
}

func TestForceDismountUnknownAndEmptyVehicle(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	if _, err := g.ForceDismount("veh-404", impl.ReasonDestroyed); !errors.Is(err, impl.ErrVehicleNotFound) {
		t.Fatalf("destroying an unknown vehicle must fail with ErrVehicleNotFound, got %v", err)
	}
	spawnOne(t, g, "veh-1")
	got, err := g.ForceDismount("veh-1", impl.ReasonDestroyed)
	if err != nil {
		t.Fatalf("destroying an empty vehicle must not fail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty vehicle returned %v, want no players", got)
	}
	if n := len(g.Events("veh-1")); n != 0 {
		t.Fatalf("an empty vehicle wrote %d events, want 0", n)
	}
}

func TestConcurrentBoardingKeepsSeatExclusive(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, taken := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := g.Board("veh-1", fmt.Sprintf("p%02d", i), 2)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, impl.ErrSeatTaken):
				taken++
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("concurrent boardings claimed the seat %d times, want exactly 1", ok)
	}
	if taken != 11 {
		t.Fatalf("concurrent boardings reported %d seat conflicts, want 11", taken)
	}
	if n := len(g.Occupants("veh-1")); n != 1 {
		t.Fatalf("occupants after the race = %d, want 1", n)
	}
}

func TestGlideWindowExpiresWithoutATakeover(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(impl.Limits{MaxSeats: 4, MaxPassengers: 2, GlideSeconds: 60, MaxEventsKept: 16})
	spawnOne(t, g, "veh-1")
	if _, err := g.Board("veh-1", "p1", 0); err != nil {
		t.Fatalf("board the driver: %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 2); err != nil {
		t.Fatalf("board the passenger: %v", err)
	}
	if err := g.Leave("veh-1", "p1"); err != nil {
		t.Fatalf("the driver leaves: %v", err)
	}
	info, err := g.GlideState("veh-1")
	if err != nil {
		t.Fatalf("glide state: %v", err)
	}
	if !info.Gliding || info.DriverID != "p1" {
		t.Fatalf("glide state = %+v, want a window opened by p1", info)
	}
	early, err := g.Tick(time.Now().Add(10 * time.Second))
	if err != nil {
		t.Fatalf("tick inside the window: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("a tick inside the glide window pushed %d players, want 0", len(early))
	}
	if n := len(g.Occupants("veh-1")); n != 1 {
		t.Fatalf("occupants inside the glide window = %d, want the remaining passenger", n)
	}
	late, err := g.Tick(time.Now().Add(120 * time.Second))
	if err != nil {
		t.Fatalf("tick after the window: %v", err)
	}
	if len(late) != 1 || late[0].PlayerID != "p2" || late[0].Reason != impl.ReasonGlideTimeout {
		t.Fatalf("expired glide window pushed %+v, want p2 with %q", late, impl.ReasonGlideTimeout)
	}
	if n := len(g.Occupants("veh-1")); n != 0 {
		t.Fatalf("occupants after the glide timeout = %d, want 0", n)
	}
	after, err := g.GlideState("veh-1")
	if err != nil {
		t.Fatalf("glide state after the timeout: %v", err)
	}
	if after.Gliding {
		t.Fatalf("the glide window must close after the timeout")
	}
}

func TestSnapshotRestoreKeepsSeatsAndSequence(t *testing.T) {
	g := impl.NewVehicleBoardingGuard(testLimits())
	spawnOne(t, g, "veh-1")
	first, err := g.Board("veh-1", "p1", 0)
	if err != nil {
		t.Fatalf("board p1: %v", err)
	}
	if _, err := g.Board("veh-1", "p2", 1); err != nil {
		t.Fatalf("board p2: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewVehicleBoardingGuard(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n := len(restored.Occupants("veh-1")); n != 2 {
		t.Fatalf("restored occupants = %d, want 2", n)
	}
	driver, err := restored.SeatOf("veh-1", "p1")
	if err != nil {
		t.Fatalf("seat of p1 after restore: %v", err)
	}
	if driver.Role != impl.RoleDriver || driver.SeatIndex != 0 {
		t.Fatalf("restored driver seat = %+v, want role %q at index 0", driver, impl.RoleDriver)
	}
	if err := restored.Spawn("veh-1", seatPlan()); !errors.Is(err, impl.ErrVehicleExists) {
		t.Fatalf("the restored vehicle must still be registered, got %v", err)
	}
	next, err := restored.Board("veh-1", "p3", 2)
	if err != nil {
		t.Fatalf("board after restore: %v", err)
	}
	if next.Seq <= first.Seq {
		t.Fatalf("boarding sequence after restore = %d, must continue past %d", next.Seq, first.Seq)
	}
	if err := restored.Restore([]byte("{}garbage")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if n := len(restored.Occupants("veh-1")); n != 3 {
		t.Fatalf("a rejected restore must not touch live seats, got %d occupants", n)
	}
}
