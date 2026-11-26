package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-08"
)

var reqAt = time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)

func retireLimits() impl.Limits {
	return impl.Limits{
		MaxPendingPerAccount: 2,
		RetireCoolDown:       72 * time.Hour,
		NameHoldDuration:     30 * 24 * time.Hour,
		MaxTombstones:        8,
	}
}

func mustRegister(t *testing.T, g *impl.CharacterRetireGuard, accountID, characterID, name string) impl.CharacterState {
	t.Helper()
	st, err := g.Register(accountID, characterID, name)
	if err != nil {
		t.Fatalf("Register(%q, %q, %q): unexpected error: %v", accountID, characterID, name, err)
	}
	return st
}

func mustRetire(t *testing.T, g *impl.CharacterRetireGuard, accountID, characterID string, at time.Time) impl.RetireTicket {
	t.Helper()
	ticket, err := g.Request(accountID, characterID, at)
	if err != nil {
		t.Fatalf("Request(%q, %q): unexpected error: %v", accountID, characterID, err)
	}
	return ticket
}

func TestRegisterRejectsDuplicateNameOrID(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	if _, err := g.Register("acc-2", "char-2", "Aria"); !errors.Is(err, impl.ErrNameTaken) {
		t.Fatalf("Register with taken name: got %v, want ErrNameTaken", err)
	}
	if _, err := g.Register("acc-1", "char-1", "Bryn"); !errors.Is(err, impl.ErrCharacterExists) {
		t.Fatalf("Register with duplicate id: got %v, want ErrCharacterExists", err)
	}
}

func TestStateOfUnknownCharacterReportsNotFound(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	if _, err := g.StateOf("char-404"); !errors.Is(err, impl.ErrCharacterNotFound) {
		t.Fatalf("StateOf unknown character: got %v, want ErrCharacterNotFound", err)
	}
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	st, err := g.StateOf("char-1")
	if err != nil {
		t.Fatalf("StateOf(char-1): unexpected error: %v", err)
	}
	if st.Status != impl.StatusActive || st.Name != "Aria" || st.AccountID != "acc-1" {
		t.Fatalf("StateOf(char-1): got %+v, want active Aria of acc-1", st)
	}
}

func TestRequestRejectsUnknownCharacter(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	if _, err := g.Request("acc-1", "char-404", reqAt); !errors.Is(err, impl.ErrCharacterNotFound) {
		t.Fatalf("Request unknown character: got %v, want ErrCharacterNotFound", err)
	}
}

func TestRequestRejectsForeignAccount(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	if _, err := g.Request("acc-2", "char-1", reqAt); !errors.Is(err, impl.ErrAccountMismatch) {
		t.Fatalf("Request from another account: got %v, want ErrAccountMismatch", err)
	}
}

func TestRequestIsIdempotent(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	first := mustRetire(t, g, "acc-1", "char-1", reqAt)
	second := mustRetire(t, g, "acc-1", "char-1", reqAt.Add(time.Hour))
	if first.TicketID != second.TicketID {
		t.Fatalf("repeated Request: got ticket %q then %q, want the same ticket", first.TicketID, second.TicketID)
	}
	if !first.PurgeAt.Equal(reqAt.Add(72 * time.Hour)) {
		t.Fatalf("purge time: got %v, want %v", first.PurgeAt, reqAt.Add(72*time.Hour))
	}
	if !second.PurgeAt.Equal(first.PurgeAt) {
		t.Fatalf("repeated Request moved purge time: got %v, want %v", second.PurgeAt, first.PurgeAt)
	}
	pending, err := g.Pending("acc-1")
	if err != nil {
		t.Fatalf("Pending: unexpected error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending after repeated Request: got %d, want 1", len(pending))
	}
	st, err := g.StateOf("char-1")
	if err != nil {
		t.Fatalf("StateOf: unexpected error: %v", err)
	}
	if st.Status != impl.StatusPending {
		t.Fatalf("status after Request: got %q, want %q", st.Status, impl.StatusPending)
	}
}

func TestRequestEnforcesPendingLimit(t *testing.T) {
	limits := retireLimits()
	limits.MaxPendingPerAccount = 1
	g := impl.NewCharacterRetireGuard(limits)
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	mustRegister(t, g, "acc-1", "char-2", "Bryn")
	mustRetire(t, g, "acc-1", "char-1", reqAt)
	if _, err := g.Request("acc-1", "char-2", reqAt); !errors.Is(err, impl.ErrPendingLimit) {
		t.Fatalf("Request above pending limit: got %v, want ErrPendingLimit", err)
	}
}

func TestCancelReturnsCharacterToActive(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	st, err := g.Cancel("acc-1", ticket.TicketID)
	if err != nil {
		t.Fatalf("Cancel: unexpected error: %v", err)
	}
	if st.Status != impl.StatusActive || st.CharacterID != "char-1" {
		t.Fatalf("Cancel state: got %+v, want active char-1", st)
	}
	pending, err := g.Pending("acc-1")
	if err != nil {
		t.Fatalf("Pending: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after Cancel: got %d, want 0", len(pending))
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	if _, err := g.Cancel("acc-1", ticket.TicketID); err != nil {
		t.Fatalf("first Cancel: unexpected error: %v", err)
	}
	st, err := g.Cancel("acc-1", ticket.TicketID)
	if err != nil {
		t.Fatalf("second Cancel: unexpected error: %v", err)
	}
	if st.Status != impl.StatusActive {
		t.Fatalf("second Cancel state: got %q, want %q", st.Status, impl.StatusActive)
	}
}

func TestCancelRejectsWrongAccount(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	if _, err := g.Cancel("acc-2", ticket.TicketID); !errors.Is(err, impl.ErrAccountMismatch) {
		t.Fatalf("Cancel from another account: got %v, want ErrAccountMismatch", err)
	}
	if _, err := g.Cancel("acc-1", "rt-404"); !errors.Is(err, impl.ErrTicketNotFound) {
		t.Fatalf("Cancel unknown ticket: got %v, want ErrTicketNotFound", err)
	}
}

func TestFinalizeRejectsBeforeCoolDown(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	if _, err := g.Finalize(ticket.TicketID, reqAt.Add(71*time.Hour)); !errors.Is(err, impl.ErrTooEarly) {
		t.Fatalf("Finalize inside cool down: got %v, want ErrTooEarly", err)
	}
	st, err := g.StateOf("char-1")
	if err != nil {
		t.Fatalf("StateOf: unexpected error: %v", err)
	}
	if st.Status != impl.StatusPending {
		t.Fatalf("status after rejected Finalize: got %q, want %q", st.Status, impl.StatusPending)
	}
}

func TestFinalizePurgesAndReportsTombstone(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	result, err := g.Finalize(ticket.TicketID, reqAt.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("Finalize after cool down: unexpected error: %v", err)
	}
	if !result.Purged {
		t.Fatalf("Finalize result: got Purged=false, want true")
	}
	if result.CharacterID != "char-1" || result.Name != "Aria" {
		t.Fatalf("Finalize result: got %+v, want char-1 Aria", result)
	}
	if result.TombstoneID == "" {
		t.Fatalf("Finalize result: got empty TombstoneID, want a tombstone record")
	}
	st, err := g.StateOf("char-1")
	if err != nil {
		t.Fatalf("StateOf after purge: unexpected error: %v", err)
	}
	if st.Status != impl.StatusPurged {
		t.Fatalf("status after purge: got %q, want %q", st.Status, impl.StatusPurged)
	}
}

func TestFinalizeRejectsSecondPurge(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	if _, err := g.Finalize(ticket.TicketID, reqAt.Add(72*time.Hour)); err != nil {
		t.Fatalf("first Finalize: unexpected error: %v", err)
	}
	if _, err := g.Finalize(ticket.TicketID, reqAt.Add(73*time.Hour)); !errors.Is(err, impl.ErrTicketClosed) {
		t.Fatalf("second Finalize: got %v, want ErrTicketClosed", err)
	}
}

func TestPendingCharacterCannotEnterMatch(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	if err := g.EnterMatch("char-1"); !errors.Is(err, impl.ErrCharacterRetired) {
		t.Fatalf("EnterMatch while pending: got %v, want ErrCharacterRetired", err)
	}
	if _, err := g.Cancel("acc-1", ticket.TicketID); err != nil {
		t.Fatalf("Cancel: unexpected error: %v", err)
	}
	if err := g.EnterMatch("char-1"); err != nil {
		t.Fatalf("EnterMatch after Cancel: unexpected error: %v", err)
	}
}

func TestSnapshotRestoreKeepsTicketsAndTombstones(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	mustRegister(t, g, "acc-1", "char-2", "Bryn")
	ticket := mustRetire(t, g, "acc-1", "char-1", reqAt)
	if _, err := g.Finalize(ticket.TicketID, reqAt.Add(72*time.Hour)); err != nil {
		t.Fatalf("Finalize: unexpected error: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Snapshot: got empty payload")
	}
	h := impl.NewCharacterRetireGuard(retireLimits())
	if err := h.RestoreSnapshot(data); err != nil {
		t.Fatalf("RestoreSnapshot: unexpected error: %v", err)
	}
	purged, err := h.StateOf("char-1")
	if err != nil {
		t.Fatalf("StateOf(char-1) after restore: unexpected error: %v", err)
	}
	if purged.Status != impl.StatusPurged {
		t.Fatalf("char-1 status after restore: got %q, want %q", purged.Status, impl.StatusPurged)
	}
	alive, err := h.StateOf("char-2")
	if err != nil {
		t.Fatalf("StateOf(char-2) after restore: unexpected error: %v", err)
	}
	if alive.Status != impl.StatusActive || alive.Name != "Bryn" {
		t.Fatalf("char-2 after restore: got %+v, want active Bryn", alive)
	}
	pending, err := h.Pending("acc-1")
	if err != nil {
		t.Fatalf("Pending after restore: unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after restore: got %d, want 0", len(pending))
	}
}

func TestRestoreSnapshotRejectsCorruptPayload(t *testing.T) {
	g := impl.NewCharacterRetireGuard(retireLimits())
	mustRegister(t, g, "acc-1", "char-1", "Aria")
	if err := g.RestoreSnapshot([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("RestoreSnapshot malformed payload: got %v, want ErrBadSnapshot", err)
	}
	if err := g.RestoreSnapshot(nil); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("RestoreSnapshot empty payload: got %v, want ErrBadSnapshot", err)
	}
	st, err := g.StateOf("char-1")
	if err != nil {
		t.Fatalf("StateOf after rejected restore: unexpected error: %v", err)
	}
	if st.Status != impl.StatusActive {
		t.Fatalf("status after rejected restore: got %q, want %q", st.Status, impl.StatusActive)
	}
}
