package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-b-15"
)

func brokerConfig() impl.BrokerConfig {
	return impl.BrokerConfig{
		MaxSlotsPerPurpose: 4,
		DefaultPoolSlots:   2,
		MaxBytes:           1000,
		MaxLatencyTicks:    3,
	}
}

func newBroker(t *testing.T, cfg impl.BrokerConfig) *impl.ReadbackBroker {
	t.Helper()
	return impl.NewReadbackBroker(cfg)
}

func payload(n int) []byte {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte(i + 1)
	}
	return buf
}

func mustRequest(t *testing.T, b *impl.ReadbackBroker, purpose string, size int) impl.Ticket {
	t.Helper()
	ticket, err := b.Request(purpose, size)
	if err != nil {
		t.Fatalf("request %d bytes on %q: %v", size, purpose, err)
	}
	if ticket == 0 {
		t.Fatalf("request on %q returned the zero ticket", purpose)
	}
	return ticket
}

func mustSubmit(t *testing.T, b *impl.ReadbackBroker, ticket impl.Ticket, size int) {
	t.Helper()
	if err := b.Submit(ticket, payload(size)); err != nil {
		t.Fatalf("submit ticket %d: %v", ticket, err)
	}
}

func mustFence(t *testing.T, b *impl.ReadbackBroker, ticket impl.Ticket) {
	t.Helper()
	if err := b.CompleteFence(ticket); err != nil {
		t.Fatalf("complete fence on ticket %d: %v", ticket, err)
	}
}

func mustDeliver(t *testing.T, b *impl.ReadbackBroker, ticket impl.Ticket) impl.Result {
	t.Helper()
	res, ok, err := b.Poll(ticket)
	if err != nil {
		t.Fatalf("poll ticket %d: %v", ticket, err)
	}
	if !ok {
		t.Fatalf("poll ticket %d came back empty", ticket)
	}
	return res
}

func TestRequestRejectsBadInputAndBytes(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if _, err := b.Request("  ", 10); !errors.Is(err, impl.ErrBadPurpose) {
		t.Fatalf("blank purpose must fail with ErrBadPurpose, got %v", err)
	}
	if _, err := b.Request("pick", 0); !errors.Is(err, impl.ErrBadSize) {
		t.Fatalf("zero size must fail with ErrBadSize, got %v", err)
	}
	if _, err := b.Request("pick", 1001); !errors.Is(err, impl.ErrBadSize) {
		t.Fatalf("size above the byte cap must fail with ErrBadSize, got %v", err)
	}
	mustRequest(t, b, "pick", 600)
	if _, err := b.Request("pick", 600); !errors.Is(err, impl.ErrBytesLimit) {
		t.Fatalf("a second 600 byte request must fail with ErrBytesLimit, got %v", err)
	}
	st := b.Latency("pick")
	if st.Requests != 1 || st.BytesInFlight != 600 {
		t.Fatalf("stats = %+v, want one request holding 600 bytes", st)
	}
}

func TestPoolSizeValidationAndFullRing(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.SetPoolSize("pick", 2); err != nil {
		t.Fatalf("set pool size: %v", err)
	}
	if err := b.SetPoolSize("pick", 0); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("zero slot count must fail with ErrBadPool, got %v", err)
	}
	if err := b.SetPoolSize("pick", 9); !errors.Is(err, impl.ErrBadPool) {
		t.Fatalf("slot count above the cap must fail with ErrBadPool, got %v", err)
	}
	if err := b.SetPoolSize("", 1); !errors.Is(err, impl.ErrBadPurpose) {
		t.Fatalf("blank purpose must fail with ErrBadPurpose, got %v", err)
	}
	first := mustRequest(t, b, "pick", 10)
	second := mustRequest(t, b, "pick", 10)
	if _, err := b.Request("pick", 10); !errors.Is(err, impl.ErrPoolFull) {
		t.Fatalf("a third request on a two slot ring must fail with ErrPoolFull, got %v", err)
	}
	mustSubmit(t, b, first, 10)
	mustFence(t, b, first)
	if res := mustDeliver(t, b, first); res.Ticket != first {
		t.Fatalf("delivered ticket = %d, want %d", res.Ticket, first)
	}
	third := mustRequest(t, b, "pick", 10)
	if third == second {
		t.Fatalf("the reused slot must hand out a new ticket, got %d twice", third)
	}
	if err := b.SetPoolSize("pick", 1); !errors.Is(err, impl.ErrPoolBusy) {
		t.Fatalf("shrinking below the in use slots must fail with ErrPoolBusy, got %v", err)
	}
}

func TestSubmitValidatesStateAndSize(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.Submit(999, payload(4)); !errors.Is(err, impl.ErrUnknownTicket) {
		t.Fatalf("unknown ticket must fail with ErrUnknownTicket, got %v", err)
	}
	ticket := mustRequest(t, b, "pick", 4)
	if err := b.Submit(ticket, payload(3)); !errors.Is(err, impl.ErrSizeMismatch) {
		t.Fatalf("short payload must fail with ErrSizeMismatch, got %v", err)
	}
	if err := b.Submit(ticket, payload(4)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := b.Submit(ticket, payload(4)); !errors.Is(err, impl.ErrBadState) {
		t.Fatalf("second submit must fail with ErrBadState, got %v", err)
	}
}

func TestPollPendingTicket(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if _, _, err := b.Poll(999); !errors.Is(err, impl.ErrUnknownTicket) {
		t.Fatalf("unknown ticket must fail with ErrUnknownTicket, got %v", err)
	}
	ticket := mustRequest(t, b, "pick", 8)
	mustSubmit(t, b, ticket, 8)
	res, ok, err := b.Poll(ticket)
	if err != nil {
		t.Fatalf("poll before the fence: %v", err)
	}
	if ok {
		t.Fatalf("poll before the fence returned a result: %+v", res)
	}
	if res.Ticket != 0 {
		t.Fatalf("empty poll must return the zero result, got %+v", res)
	}
	if got := b.Pending("pick"); got != 1 {
		t.Fatalf("pending tickets = %d, want 1", got)
	}
}

func TestPollDeliversCopiedData(t *testing.T) {
	b := newBroker(t, brokerConfig())
	ticket := mustRequest(t, b, "pick", 4)
	buf := []byte{1, 2, 3, 4}
	if err := b.Submit(ticket, buf); err != nil {
		t.Fatalf("submit: %v", err)
	}
	buf[0] = 99
	mustFence(t, b, ticket)
	res := mustDeliver(t, b, ticket)
	if len(res.Data) != 4 || res.Data[0] != 1 || res.Data[3] != 4 {
		t.Fatalf("delivered data = %v, want the submitted copy", res.Data)
	}
	if res.Purpose != "pick" || res.Seq != 1 || res.Degraded || res.At != 0 {
		t.Fatalf("result = %+v, want purpose pick, seq 1, not degraded, clock 0", res)
	}
	if _, _, err := b.Poll(ticket); !errors.Is(err, impl.ErrTicketConsumed) {
		t.Fatalf("polling a delivered ticket must fail with ErrTicketConsumed, got %v", err)
	}
	if got := b.Pending("pick"); got != 0 {
		t.Fatalf("pending tickets after delivery = %d, want 0", got)
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	b := newBroker(t, brokerConfig())
	ticket := mustRequest(t, b, "pick", 8)
	mustSubmit(t, b, ticket, 8)
	if err := b.Cancel(ticket); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := b.Poll(ticket); !errors.Is(err, impl.ErrTicketCancelled) {
		t.Fatalf("polling a cancelled ticket must fail with ErrTicketCancelled, got %v", err)
	}
	if err := b.Submit(ticket, payload(8)); !errors.Is(err, impl.ErrTicketCancelled) {
		t.Fatalf("submitting after the cancel must fail with ErrTicketCancelled, got %v", err)
	}
	if err := b.Cancel(ticket); err != nil {
		t.Fatalf("cancelling twice must be idempotent, got %v", err)
	}
	if err := b.CompleteFence(ticket); err != nil {
		t.Fatalf("the fence of a cancelled ticket must still complete, got %v", err)
	}
	if err := b.Cancel(999); !errors.Is(err, impl.ErrUnknownTicket) {
		t.Fatalf("cancelling an unknown ticket must fail with ErrUnknownTicket, got %v", err)
	}
}

func TestCancelRejectedAfterResult(t *testing.T) {
	b := newBroker(t, brokerConfig())
	delivered := mustRequest(t, b, "pick", 4)
	mustSubmit(t, b, delivered, 4)
	mustFence(t, b, delivered)
	mustDeliver(t, b, delivered)
	if err := b.Cancel(delivered); !errors.Is(err, impl.ErrBadState) {
		t.Fatalf("cancelling a delivered ticket must fail with ErrBadState, got %v", err)
	}
	degraded := mustRequest(t, b, "pick", 4)
	mustSubmit(t, b, degraded, 4)
	if err := b.SetClock(10); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	res := mustDeliver(t, b, degraded)
	if !res.Degraded {
		t.Fatalf("result = %+v, want a degraded one", res)
	}
	if err := b.Cancel(degraded); !errors.Is(err, impl.ErrBadState) {
		t.Fatalf("cancelling a degraded ticket must fail with ErrBadState, got %v", err)
	}
}

func TestTimeoutReturnsDegradedResult(t *testing.T) {
	cfg := brokerConfig()
	b := newBroker(t, cfg)
	if err := b.SetPoolSize("shot", 1); err != nil {
		t.Fatalf("set pool size: %v", err)
	}
	ticket := mustRequest(t, b, "shot", 8)
	mustSubmit(t, b, ticket, 8)
	if err := b.SetClock(10); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	res := mustDeliver(t, b, ticket)
	if !res.Degraded || res.Ticket != ticket || res.Purpose != "shot" {
		t.Fatalf("result = %+v, want a degraded result for the same ticket", res)
	}
	if res.Data != nil {
		t.Fatalf("degraded result carries %d bytes, want none", len(res.Data))
	}
	if res.At != 10 {
		t.Fatalf("degraded result clock = %d, want 10", res.At)
	}
	if got := b.Latency("shot").Degraded; got != 1 {
		t.Fatalf("degraded counter = %d, want 1", got)
	}
}

func TestDegradedSlotFencedBeforeReuse(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.SetPoolSize("shot", 1); err != nil {
		t.Fatalf("set pool size: %v", err)
	}
	ticket := mustRequest(t, b, "shot", 40)
	mustSubmit(t, b, ticket, 40)
	if err := b.SetClock(10); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	mustDeliver(t, b, ticket)
	if _, err := b.Request("shot", 10); !errors.Is(err, impl.ErrPoolFull) {
		t.Fatalf("the slot of a degraded ticket must stay occupied until the fence, got %v", err)
	}
	if got := b.Latency("shot").BytesInFlight; got != 40 {
		t.Fatalf("bytes in flight = %d before the fence, want 40", got)
	}
	mustFence(t, b, ticket)
	if got := b.Latency("shot").BytesInFlight; got != 0 {
		t.Fatalf("bytes in flight = %d after the fence, want 0", got)
	}
	mustRequest(t, b, "shot", 10)
}

func TestLatencyCounters(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.SetPoolSize("pick", 4); err != nil {
		t.Fatalf("set pool size: %v", err)
	}
	done := mustRequest(t, b, "pick", 4)
	timedOut := mustRequest(t, b, "pick", 4)
	dropped := mustRequest(t, b, "pick", 4)
	waiting := mustRequest(t, b, "pick", 4)
	for _, ticket := range []impl.Ticket{done, timedOut, dropped, waiting} {
		mustSubmit(t, b, ticket, 4)
	}
	mustFence(t, b, done)
	mustDeliver(t, b, done)
	if err := b.Cancel(dropped); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := b.SetClock(10); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	mustDeliver(t, b, timedOut)
	st := b.Latency("pick")
	if st.Requests != 4 || st.Delivered != 1 || st.Degraded != 1 || st.Cancelled != 1 {
		t.Fatalf("stats = %+v, want 4 requests with one delivery, one degrade and one cancel", st)
	}
	if st.InFlight != 1 {
		t.Fatalf("in flight = %d, want only the still waiting ticket", st.InFlight)
	}
	if st.MaxLatencyTicks != 3 {
		t.Fatalf("latency budget = %d, want the configured 3", st.MaxLatencyTicks)
	}
	if got := b.Pending("pick"); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	purposes := b.Purposes()
	if len(purposes) != 1 || purposes[0] != "pick" {
		t.Fatalf("purposes = %v, want [pick]", purposes)
	}
}

func TestClockRejectsRegression(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.SetClock(-1); !errors.Is(err, impl.ErrBadClock) {
		t.Fatalf("negative clock must fail with ErrBadClock, got %v", err)
	}
	if err := b.SetClock(5); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	if err := b.SetClock(3); !errors.Is(err, impl.ErrBadClock) {
		t.Fatalf("backwards clock must fail with ErrBadClock, got %v", err)
	}
	if got := b.Clock(); got != 5 {
		t.Fatalf("clock = %d, want it pinned at 5", got)
	}
}

func TestPurposesHaveIndependentPools(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.SetPoolSize("pick", 1); err != nil {
		t.Fatalf("set pick pool: %v", err)
	}
	if err := b.SetPoolSize("shot", 1); err != nil {
		t.Fatalf("set shot pool: %v", err)
	}
	mustRequest(t, b, "pick", 8)
	if _, err := b.Request("pick", 8); !errors.Is(err, impl.ErrPoolFull) {
		t.Fatalf("the pick ring must be full, got %v", err)
	}
	if _, err := b.Request("shot", 8); err != nil {
		t.Fatalf("the shot ring must stay usable while pick is full, got %v", err)
	}
	purposes := b.Purposes()
	if len(purposes) != 2 || purposes[0] != "pick" || purposes[1] != "shot" {
		t.Fatalf("purposes = %v, want the ascending pair [pick shot]", purposes)
	}
}

func TestSequencePerPurposeAndCrossPurposeDelivery(t *testing.T) {
	b := newBroker(t, brokerConfig())
	if err := b.SetPoolSize("pick", 4); err != nil {
		t.Fatalf("set pick pool: %v", err)
	}
	if err := b.SetPoolSize("shot", 1); err != nil {
		t.Fatalf("set shot pool: %v", err)
	}
	first := mustRequest(t, b, "pick", 4)
	second := mustRequest(t, b, "pick", 4)
	shot := mustRequest(t, b, "shot", 4)
	for _, ticket := range []impl.Ticket{first, second, shot} {
		mustSubmit(t, b, ticket, 4)
		mustFence(t, b, ticket)
	}
	if res := mustDeliver(t, b, shot); res.Seq != 1 || res.Purpose != "shot" {
		t.Fatalf("shot result = %+v, want its own first sequence", res)
	}
	if res := mustDeliver(t, b, first); res.Seq != 1 {
		t.Fatalf("first pick result seq = %d, want 1", res.Seq)
	}
	if res := mustDeliver(t, b, second); res.Seq != 2 {
		t.Fatalf("second pick result seq = %d, want 2", res.Seq)
	}
}

func TestSnapshotRestoreInvalidatesPending(t *testing.T) {
	cfg := brokerConfig()
	b := newBroker(t, cfg)
	if err := b.SetPoolSize("pick", 2); err != nil {
		t.Fatalf("set pool size: %v", err)
	}
	ticket := mustRequest(t, b, "pick", 8)
	mustSubmit(t, b, ticket, 8)
	data, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newBroker(t, cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, _, err := restored.Poll(ticket); !errors.Is(err, impl.ErrTicketInvalidated) {
		t.Fatalf("a ticket from before the restart must fail with ErrTicketInvalidated, got %v", err)
	}
	st := restored.Latency("pick")
	if st.InFlight != 0 || st.BytesInFlight != 0 {
		t.Fatalf("restored stats = %+v, want no ticket still in flight", st)
	}
	next := mustRequest(t, restored, "pick", 8)
	if next <= ticket {
		t.Fatalf("ticket %d must continue past the pre restart %d", next, ticket)
	}
	purposes := restored.Purposes()
	if len(purposes) != 1 || purposes[0] != "pick" {
		t.Fatalf("restored purposes = %v, want [pick]", purposes)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	b := newBroker(t, brokerConfig())
	ticket := mustRequest(t, b, "pick", 4)
	if err := b.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if err := b.Submit(ticket, payload(4)); err != nil {
		t.Fatalf("the live ticket must keep working after a rejected restore, got %v", err)
	}
	mustFence(t, b, ticket)
	if res := mustDeliver(t, b, ticket); len(res.Data) != 4 {
		t.Fatalf("delivered data = %d bytes, want 4", len(res.Data))
	}
}
