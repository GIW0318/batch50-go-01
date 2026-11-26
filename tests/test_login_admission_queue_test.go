package task

import (
	"errors"
	"fmt"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-09"
)

var queueAt = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

func queueConfig() impl.Config {
	return impl.Config{
		Capacity:        map[string]int{"cn-east": 2},
		TicketTTL:       time.Minute,
		GrantTTL:        30 * time.Second,
		BatchSize:       2,
		PriorityQuota:   1,
		StarvationLimit: 3,
	}
}

func mustEnqueue(t *testing.T, q *impl.LoginAdmissionQueue, accountID, region string, at time.Time) impl.Ticket {
	t.Helper()
	ticket, err := q.Enqueue(accountID, region, at)
	if err != nil {
		t.Fatalf("Enqueue(%q, %q): unexpected error: %v", accountID, region, err)
	}
	return ticket
}

func mustAdmit(t *testing.T, q *impl.LoginAdmissionQueue, at time.Time) []impl.Grant {
	t.Helper()
	grants, err := q.Admit(at)
	if err != nil {
		t.Fatalf("Admit: unexpected error: %v", err)
	}
	return grants
}

func TestEnqueueRejectsEmptyAccount(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	if _, err := q.Enqueue("", "cn-east", queueAt); !errors.Is(err, impl.ErrBadArgument) {
		t.Fatalf("Enqueue empty account: got %v, want ErrBadArgument", err)
	}
	if _, err := q.Enqueue("a1", "", queueAt); !errors.Is(err, impl.ErrBadArgument) {
		t.Fatalf("Enqueue empty region: got %v, want ErrBadArgument", err)
	}
}

func TestEnqueueRejectsUnknownRegion(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	if _, err := q.Enqueue("a1", "eu-west", queueAt); !errors.Is(err, impl.ErrUnknownRegion) {
		t.Fatalf("Enqueue unknown region: got %v, want ErrUnknownRegion", err)
	}
}

func TestEnqueueAssignsIncreasingSeqAndPosition(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	first := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	second := mustEnqueue(t, q, "a2", "cn-east", queueAt.Add(time.Second))
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("ticket seq: got %d and %d, want 1 and 2", first.Seq, second.Seq)
	}
	st1, err := q.Status(first.TicketID)
	if err != nil {
		t.Fatalf("Status(first): unexpected error: %v", err)
	}
	if st1.Status != impl.TicketWaiting || st1.Position != 0 {
		t.Fatalf("first ticket state: got %+v, want waiting with position 0", st1)
	}
	st2, err := q.Status(second.TicketID)
	if err != nil {
		t.Fatalf("Status(second): unexpected error: %v", err)
	}
	if st2.Status != impl.TicketWaiting || st2.Position != 1 {
		t.Fatalf("second ticket state: got %+v, want waiting with position 1", st2)
	}
}

func TestAdmitGrantsInFIFOOrder(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	first := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	second := mustEnqueue(t, q, "a2", "cn-east", queueAt.Add(time.Second))
	third := mustEnqueue(t, q, "a3", "cn-east", queueAt.Add(2*time.Second))
	grants := mustAdmit(t, q, queueAt.Add(3*time.Second))
	if len(grants) != 2 {
		t.Fatalf("grants: got %d, want 2", len(grants))
	}
	if grants[0].TicketID != first.TicketID || grants[1].TicketID != second.TicketID {
		t.Fatalf("grant order: got %q then %q, want %q then %q", grants[0].TicketID, grants[1].TicketID, first.TicketID, second.TicketID)
	}
	waiting, err := q.Status(third.TicketID)
	if err != nil {
		t.Fatalf("Status(third): unexpected error: %v", err)
	}
	if waiting.Status != impl.TicketWaiting {
		t.Fatalf("third ticket status: got %q, want %q", waiting.Status, impl.TicketWaiting)
	}
}

func TestAdmitRespectsRegionCapacity(t *testing.T) {
	cfg := queueConfig()
	cfg.Capacity = map[string]int{"cn-east": 1}
	q := impl.NewLoginAdmissionQueue(cfg)
	mustEnqueue(t, q, "a1", "cn-east", queueAt)
	mustEnqueue(t, q, "a2", "cn-east", queueAt.Add(time.Second))
	grants := mustAdmit(t, q, queueAt.Add(3*time.Second))
	if len(grants) != 1 {
		t.Fatalf("grants under capacity 1: got %d, want 1", len(grants))
	}
	again := mustAdmit(t, q, queueAt.Add(4*time.Second))
	if len(again) != 0 {
		t.Fatalf("second Admit while the only seat is granted: got %d grants, want 0", len(again))
	}
}

func TestAdmitRespectsBatchSize(t *testing.T) {
	cfg := queueConfig()
	cfg.Capacity = map[string]int{"cn-east": 10}
	cfg.BatchSize = 2
	q := impl.NewLoginAdmissionQueue(cfg)
	for i := 0; i < 5; i++ {
		mustEnqueue(t, q, fmt.Sprintf("a%d", i+1), "cn-east", queueAt.Add(time.Duration(i)*time.Second))
	}
	grants := mustAdmit(t, q, queueAt.Add(10*time.Second))
	if len(grants) != 2 {
		t.Fatalf("grants under batch size 2: got %d, want 2", len(grants))
	}
}

func TestRedeemConsumesGrantOnce(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	ticket := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	grants := mustAdmit(t, q, queueAt.Add(time.Second))
	if len(grants) != 1 {
		t.Fatalf("grants: got %d, want 1", len(grants))
	}
	if err := q.Redeem(ticket.TicketID, grants[0].Nonce, queueAt.Add(2*time.Second)); err != nil {
		t.Fatalf("first Redeem: unexpected error: %v", err)
	}
	st, err := q.Status(ticket.TicketID)
	if err != nil {
		t.Fatalf("Status after Redeem: unexpected error: %v", err)
	}
	if st.Status != impl.TicketAdmitted {
		t.Fatalf("status after Redeem: got %q, want %q", st.Status, impl.TicketAdmitted)
	}
	if got := q.Occupied("cn-east"); got != 1 {
		t.Fatalf("occupied after Redeem: got %d, want 1", got)
	}
	if err := q.Redeem(ticket.TicketID, grants[0].Nonce, queueAt.Add(3*time.Second)); !errors.Is(err, impl.ErrGrantUsed) {
		t.Fatalf("second Redeem: got %v, want ErrGrantUsed", err)
	}
}

func TestRedeemRejectsUnknownNonceAndTicket(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	ticket := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	grants := mustAdmit(t, q, queueAt.Add(time.Second))
	if len(grants) != 1 {
		t.Fatalf("grants: got %d, want 1", len(grants))
	}
	if err := q.Redeem(ticket.TicketID, "nonce-404", queueAt.Add(2*time.Second)); !errors.Is(err, impl.ErrGrantNotFound) {
		t.Fatalf("Redeem with wrong nonce: got %v, want ErrGrantNotFound", err)
	}
	if err := q.Redeem("tk-404", grants[0].Nonce, queueAt.Add(2*time.Second)); !errors.Is(err, impl.ErrTicketNotFound) {
		t.Fatalf("Redeem unknown ticket: got %v, want ErrTicketNotFound", err)
	}
}

func TestLeaveClosesWaitingTicket(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	ticket := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	if err := q.Leave(ticket.TicketID); err != nil {
		t.Fatalf("first Leave: unexpected error: %v", err)
	}
	if err := q.Leave(ticket.TicketID); !errors.Is(err, impl.ErrTicketClosed) {
		t.Fatalf("second Leave: got %v, want ErrTicketClosed", err)
	}
	if _, err := q.Status(ticket.TicketID); err != nil {
		t.Fatalf("Status after Leave: unexpected error: %v", err)
	}
}

func TestStatusAndHeartbeatRejectUnknownTicket(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	if _, err := q.Status("tk-404"); !errors.Is(err, impl.ErrTicketNotFound) {
		t.Fatalf("Status unknown ticket: got %v, want ErrTicketNotFound", err)
	}
	if _, err := q.Heartbeat("tk-404", queueAt); !errors.Is(err, impl.ErrTicketNotFound) {
		t.Fatalf("Heartbeat unknown ticket: got %v, want ErrTicketNotFound", err)
	}
}

func TestHeartbeatRejectsExpiredTicket(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	ticket := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	if _, err := q.Heartbeat(ticket.TicketID, queueAt.Add(2*time.Minute)); !errors.Is(err, impl.ErrTicketExpired) {
		t.Fatalf("Heartbeat after ttl: got %v, want ErrTicketExpired", err)
	}
	st, err := q.Status(ticket.TicketID)
	if err != nil {
		t.Fatalf("Status: unexpected error: %v", err)
	}
	if st.Status != impl.TicketExpired {
		t.Fatalf("status after stale heartbeat: got %q, want %q", st.Status, impl.TicketExpired)
	}
}

func TestEvictExpiredReturnsCount(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	mustEnqueue(t, q, "a1", "cn-east", queueAt)
	mustEnqueue(t, q, "a2", "cn-east", queueAt.Add(time.Second))
	mustEnqueue(t, q, "a3", "cn-east", queueAt.Add(2*time.Second))
	evicted, err := q.EvictExpired(queueAt.Add(3 * time.Minute))
	if err != nil {
		t.Fatalf("EvictExpired: unexpected error: %v", err)
	}
	if evicted != 3 {
		t.Fatalf("evicted tickets: got %d, want 3", evicted)
	}
	again, err := q.EvictExpired(queueAt.Add(4 * time.Minute))
	if err != nil {
		t.Fatalf("second EvictExpired: unexpected error: %v", err)
	}
	if again != 0 {
		t.Fatalf("second EvictExpired: got %d, want 0", again)
	}
}

func TestSetCapacityRejectsBadInput(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	if err := q.SetCapacity("", 2); !errors.Is(err, impl.ErrBadArgument) {
		t.Fatalf("SetCapacity empty region: got %v, want ErrBadArgument", err)
	}
	if err := q.SetCapacity("cn-east", -1); !errors.Is(err, impl.ErrInvalidCapacity) {
		t.Fatalf("SetCapacity negative: got %v, want ErrInvalidCapacity", err)
	}
	if err := q.SetCapacity("cn-east", 0); err != nil {
		t.Fatalf("SetCapacity zero: unexpected error: %v", err)
	}
}

func TestSnapshotRestoreKeepsQueuesAndGrants(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	first := mustEnqueue(t, q, "a1", "cn-east", queueAt)
	mustEnqueue(t, q, "a2", "cn-east", queueAt.Add(time.Second))
	grants := mustAdmit(t, q, queueAt.Add(2*time.Second))
	if len(grants) != 2 {
		t.Fatalf("grants: got %d, want 2", len(grants))
	}
	data, err := q.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Snapshot: got empty payload")
	}
	r := impl.NewLoginAdmissionQueue(queueConfig())
	if err := r.Restore(data); err != nil {
		t.Fatalf("Restore: unexpected error: %v", err)
	}
	st, err := r.Status(first.TicketID)
	if err != nil {
		t.Fatalf("Status after Restore: unexpected error: %v", err)
	}
	if st.Status != impl.TicketGranted {
		t.Fatalf("status after Restore: got %q, want %q", st.Status, impl.TicketGranted)
	}
	if got := len(r.Grants("cn-east")); got != 2 {
		t.Fatalf("outstanding grants after Restore: got %d, want 2", got)
	}
	if err := r.Redeem(first.TicketID, grants[0].Nonce, queueAt.Add(3*time.Second)); err != nil {
		t.Fatalf("Redeem after Restore: unexpected error: %v", err)
	}
	if got := r.Occupied("cn-east"); got != 1 {
		t.Fatalf("occupied after Restore and Redeem: got %d, want 1", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	q := impl.NewLoginAdmissionQueue(queueConfig())
	mustEnqueue(t, q, "a1", "cn-east", queueAt)
	if err := q.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore malformed payload: got %v, want ErrBadSnapshot", err)
	}
	if err := q.Restore(nil); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("Restore empty payload: got %v, want ErrBadSnapshot", err)
	}
	if got := q.Occupied("cn-east"); got != 0 {
		t.Fatalf("occupied after rejected Restore: got %d, want 0", got)
	}
}
