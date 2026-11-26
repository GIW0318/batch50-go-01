package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-b-09"
)

func diodeConfig() impl.Config {
	return impl.Config{
		WindowMessages:   10,
		WindowBytes:      4096,
		MaxQueueMessages: 50,
		MaxPayloadBytes:  64,
		MaxGeneration:    3,
		MaxPollBacklog:   16,
	}
}

func diodeMeta() impl.Meta {
	return impl.Meta{
		Kind:        "batch",
		SourceID:    "edge-a",
		ContentType: "application/octet-stream",
		Correlation: "corr-1",
	}
}

func digestOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func mustSubmit(t *testing.T, b *impl.DiodeBridge, payload string) impl.Seq {
	t.Helper()
	seq, err := b.Submit([]byte(payload), diodeMeta())
	if err != nil {
		t.Fatalf("submit %q: %v", payload, err)
	}
	return seq
}

func okAck(seq impl.Seq, generation int) impl.Ack {
	return impl.Ack{Seq: seq, Generation: generation, Status: impl.AckOK}
}

func TestSubmitAssignsMonotonicSeqAndDigest(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	first := mustSubmit(t, b, "alpha")
	if first != impl.Seq(1) {
		t.Fatalf("first sequence = %d, want 1", first)
	}
	second := mustSubmit(t, b, "beta")
	if second != impl.Seq(2) {
		t.Fatalf("second sequence = %d, want 2", second)
	}
	msg, ok := b.Lookup(first)
	if !ok {
		t.Fatalf("lookup of the first message failed")
	}
	if msg.Digest != digestOf("alpha") {
		t.Fatalf("digest = %q, want the sha256 of the payload", msg.Digest)
	}
	if msg.Generation != 1 {
		t.Fatalf("message generation = %d, want 1", msg.Generation)
	}
	if msg.Size != 5 || string(msg.Payload) != "alpha" {
		t.Fatalf("message size=%d payload=%q, want 5 bytes of alpha", msg.Size, string(msg.Payload))
	}
	if msg.Acked {
		t.Fatalf("fresh message must not be marked acknowledged")
	}
	stats := b.Stats()
	if stats.Queued != 2 || stats.InFlight != 2 {
		t.Fatalf("stats = %+v, want two queued and two in flight", stats)
	}
	if stats.BytesInFlight != 9 {
		t.Fatalf("bytes in flight = %d, want 9", stats.BytesInFlight)
	}
	if stats.Watermark != 0 {
		t.Fatalf("watermark = %d, want 0 before any acknowledgement", stats.Watermark)
	}
	if stats.Generation != 1 {
		t.Fatalf("generation = %d, want 1", stats.Generation)
	}
}

func TestSubmitRejectsOversizeAndEmptyPayload(t *testing.T) {
	limits := diodeConfig()
	limits.MaxPayloadBytes = 16
	b := impl.NewDiodeBridge(limits)
	if _, err := b.Submit(nil, diodeMeta()); !errors.Is(err, impl.ErrEmptyPayload) {
		t.Fatalf("empty payload must fail with ErrEmptyPayload, got %v", err)
	}
	if _, err := b.Submit([]byte("0123456789abcdefg"), diodeMeta()); !errors.Is(err, impl.ErrPayloadTooLarge) {
		t.Fatalf("payload above the limit must fail with ErrPayloadTooLarge, got %v", err)
	}
	seq := mustSubmit(t, b, "small")
	if seq != impl.Seq(1) {
		t.Fatalf("rejected payloads must not consume a sequence, got %d", seq)
	}
}

func TestSubmitRejectsBlankMeta(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	noKind := diodeMeta()
	noKind.Kind = "   "
	if _, err := b.Submit([]byte("data"), noKind); !errors.Is(err, impl.ErrBadMeta) {
		t.Fatalf("blank kind must fail with ErrBadMeta, got %v", err)
	}
	noSource := diodeMeta()
	noSource.SourceID = ""
	if _, err := b.Submit([]byte("data"), noSource); !errors.Is(err, impl.ErrBadMeta) {
		t.Fatalf("blank source must fail with ErrBadMeta, got %v", err)
	}
	if _, ok := b.Lookup(1); ok {
		t.Fatalf("rejected submissions must not be retained")
	}
	seq := mustSubmit(t, b, "data")
	if seq != impl.Seq(1) {
		t.Fatalf("rejected metadata must not consume a sequence, got %d", seq)
	}
}

func TestSubmitEnforcesMessageWindow(t *testing.T) {
	limits := diodeConfig()
	limits.WindowMessages = 2
	b := impl.NewDiodeBridge(limits)
	mustSubmit(t, b, "one")
	mustSubmit(t, b, "two")
	if _, err := b.Submit([]byte("three"), diodeMeta()); !errors.Is(err, impl.ErrWindowFull) {
		t.Fatalf("third message must fail with ErrWindowFull, got %v", err)
	}
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("accept ack for seq 1: %v", err)
	}
	seq := mustSubmit(t, b, "three")
	if seq != impl.Seq(3) {
		t.Fatalf("sequence after the window drained = %d, want 3", seq)
	}
	stats := b.Stats()
	if stats.InFlight != 2 || stats.Watermark != 1 {
		t.Fatalf("stats = %+v, want two in flight and watermark 1", stats)
	}
}

func TestSubmitEnforcesByteWindow(t *testing.T) {
	limits := diodeConfig()
	limits.WindowMessages = 10
	limits.WindowBytes = 20
	limits.MaxPayloadBytes = 64
	b := impl.NewDiodeBridge(limits)
	mustSubmit(t, b, "aaaaaaaaaaaa")
	if _, err := b.Submit([]byte("bbbbbbbbbbbb"), diodeMeta()); !errors.Is(err, impl.ErrWindowFull) {
		t.Fatalf("second twelve byte payload must fail with ErrWindowFull, got %v", err)
	}
	if stats := b.Stats(); stats.BytesInFlight != 12 {
		t.Fatalf("bytes in flight = %d, want 12", stats.BytesInFlight)
	}
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("accept ack: %v", err)
	}
	seq := mustSubmit(t, b, "bbbbbbbbbbbb")
	if seq != impl.Seq(2) {
		t.Fatalf("sequence after the byte window drained = %d, want 2", seq)
	}
	if stats := b.Stats(); stats.BytesInFlight != 12 {
		t.Fatalf("bytes in flight after the retry = %d, want 12", stats.BytesInFlight)
	}
}

func TestSubmitEnforcesQueueCap(t *testing.T) {
	limits := diodeConfig()
	limits.WindowMessages = 10
	limits.MaxQueueMessages = 3
	b := impl.NewDiodeBridge(limits)
	for i := 0; i < 3; i++ {
		mustSubmit(t, b, fmt.Sprintf("m%d", i))
	}
	if _, err := b.Submit([]byte("m3"), diodeMeta()); !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("fourth message must fail with ErrQueueFull, got %v", err)
	}
	if _, ok := b.Lookup(4); ok {
		t.Fatalf("rejected message must not be retained")
	}
	if stats := b.Stats(); stats.Queued != 3 {
		t.Fatalf("queued messages = %d, want the configured cap 3", stats.Queued)
	}
}

func TestAcceptAckAdvancesWatermarkContiguously(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	mustSubmit(t, b, "one")
	mustSubmit(t, b, "two")
	mustSubmit(t, b, "three")
	if err := b.AcceptAck(okAck(2, 1)); err != nil {
		t.Fatalf("out of order ack must be accepted, got %v", err)
	}
	if stats := b.Stats(); stats.Watermark != 0 {
		t.Fatalf("watermark = %d, want 0 while seq 1 is unacknowledged", stats.Watermark)
	}
	if stats := b.Stats(); stats.InFlight != 2 {
		t.Fatalf("in flight = %d, want 2", stats.InFlight)
	}
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("ack for seq 1: %v", err)
	}
	if stats := b.Stats(); stats.Watermark != 2 {
		t.Fatalf("watermark = %d, want the contiguous prefix 2", stats.Watermark)
	}
	if stats := b.Stats(); stats.InFlight != 1 {
		t.Fatalf("in flight = %d, want 1", stats.InFlight)
	}
	if err := b.AcceptAck(okAck(3, 1)); err != nil {
		t.Fatalf("ack for seq 3: %v", err)
	}
	stats := b.Stats()
	if stats.Watermark != 3 || stats.InFlight != 0 || stats.Acked != 3 {
		t.Fatalf("stats after all acks = %+v, want watermark 3 and no messages in flight", stats)
	}
}

func TestAcceptAckIsIdempotent(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	mustSubmit(t, b, "one")
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("first ack: %v", err)
	}
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("duplicate ack must be idempotent, got %v", err)
	}
	stats := b.Stats()
	if stats.Acked != 1 {
		t.Fatalf("acknowledged counter = %d, want 1", stats.Acked)
	}
	if stats.Watermark != 1 || stats.InFlight != 0 {
		t.Fatalf("stats after duplicate ack = %+v, want watermark 1 and nothing in flight", stats)
	}
}

func TestAcceptAckRejectsUnknownSeqAndBadGeneration(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	mustSubmit(t, b, "one")
	if err := b.AcceptAck(okAck(99, 1)); !errors.Is(err, impl.ErrUnknownSeq) {
		t.Fatalf("ack for an unknown sequence must fail with ErrUnknownSeq, got %v", err)
	}
	if err := b.AcceptAck(okAck(1, 2)); !errors.Is(err, impl.ErrBadAck) {
		t.Fatalf("ack from another generation must fail with ErrBadAck, got %v", err)
	}
	bad := okAck(1, 1)
	bad.Status = "rejected"
	if err := b.AcceptAck(bad); !errors.Is(err, impl.ErrBadAck) {
		t.Fatalf("ack with an unsupported status must fail with ErrBadAck, got %v", err)
	}
	stats := b.Stats()
	if stats.Acked != 0 || stats.Watermark != 0 || stats.InFlight != 1 {
		t.Fatalf("rejected acks must not move the window, got %+v", stats)
	}
}

func TestPollAckReturnsArrivalOrderThenEmpty(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	mustSubmit(t, b, "one")
	mustSubmit(t, b, "two")
	mustSubmit(t, b, "three")
	for _, seq := range []impl.Seq{1, 3, 2} {
		if err := b.AcceptAck(okAck(seq, 1)); err != nil {
			t.Fatalf("accept ack %d: %v", seq, err)
		}
	}
	for _, want := range []impl.Seq{1, 3, 2} {
		ack, err := b.PollAck()
		if err != nil {
			t.Fatalf("poll ack: %v", err)
		}
		if ack.Seq != want {
			t.Fatalf("polled ack sequence = %d, want arrival order %d", ack.Seq, want)
		}
		if ack.Status != impl.AckOK {
			t.Fatalf("polled ack status = %q, want %q", ack.Status, impl.AckOK)
		}
	}
	if _, err := b.PollAck(); !errors.Is(err, impl.ErrNoAckPending) {
		t.Fatalf("drained ack channel must report ErrNoAckPending, got %v", err)
	}
}

func TestRotateKeyLabelsNewGeneration(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	if got := b.Generation(); got != 1 {
		t.Fatalf("initial generation = %d, want 1", got)
	}
	if err := b.RotateKey(); err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if got := b.Generation(); got != 2 {
		t.Fatalf("generation after first rotation = %d, want 2", got)
	}
	seq := mustSubmit(t, b, "after-rotation")
	msg, ok := b.Lookup(seq)
	if !ok {
		t.Fatalf("lookup after rotation failed")
	}
	if msg.Generation != 2 {
		t.Fatalf("message generation = %d, want the current generation 2", msg.Generation)
	}
	if err := b.RotateKey(); err != nil {
		t.Fatalf("second rotation: %v", err)
	}
	if err := b.RotateKey(); !errors.Is(err, impl.ErrKeyGenExhausted) {
		t.Fatalf("rotation past the cap must fail with ErrKeyGenExhausted, got %v", err)
	}
	if got := b.Generation(); got != 3 {
		t.Fatalf("generation after a rejected rotation = %d, want 3", got)
	}
}

func TestSnapshotRestoreContinuesSequence(t *testing.T) {
	cfg := diodeConfig()
	b := impl.NewDiodeBridge(cfg)
	mustSubmit(t, b, "one")
	mustSubmit(t, b, "two")
	mustSubmit(t, b, "three")
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("ack seq 1: %v", err)
	}
	data, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewDiodeBridge(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	stats := restored.Stats()
	if stats.Watermark != 1 || stats.InFlight != 2 {
		t.Fatalf("restored stats = %+v, want watermark 1 and two in flight", stats)
	}
	if _, ok := restored.Lookup(1); ok {
		t.Fatalf("acknowledged message must stay pruned after restore")
	}
	if _, ok := restored.Lookup(2); !ok {
		t.Fatalf("restored bridge lost the unacknowledged message")
	}
	seq := mustSubmit(t, restored, "four")
	if seq != impl.Seq(4) {
		t.Fatalf("sequence after restore = %d, want 4", seq)
	}
	if err := restored.AcceptAck(okAck(2, 1)); err != nil {
		t.Fatalf("ack seq 2 after restore: %v", err)
	}
	if got := restored.Stats().Watermark; got != 2 {
		t.Fatalf("watermark after restored ack = %d, want 2", got)
	}
}

func TestRestoreRejectsCorruptSnapshotAtomically(t *testing.T) {
	b := impl.NewDiodeBridge(diodeConfig())
	mustSubmit(t, b, "one")
	if err := b.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := b.Lookup(1); !ok {
		t.Fatalf("rejected restore must leave the live queue untouched")
	}
	if stats := b.Stats(); stats.InFlight != 1 || stats.Watermark != 0 {
		t.Fatalf("live stats after a rejected restore = %+v", stats)
	}
	if seq := mustSubmit(t, b, "two"); seq != impl.Seq(2) {
		t.Fatalf("sequence after a rejected restore = %d, want 2", seq)
	}
}

func TestConcurrentSubmitNeverReusesSeq(t *testing.T) {
	cfg := diodeConfig()
	cfg.WindowMessages = 200
	cfg.WindowBytes = 1 << 20
	cfg.MaxQueueMessages = 200
	b := impl.NewDiodeBridge(cfg)
	const workers = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[impl.Seq]string, workers)
	failures := make([]string, 0)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := fmt.Sprintf("payload-%03d", i)
			seq, err := b.Submit([]byte(payload), diodeMeta())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err.Error())
				return
			}
			if prev, dup := seen[seq]; dup {
				failures = append(failures, fmt.Sprintf("sequence %d reused by %q and %q", seq, prev, payload))
				return
			}
			seen[seq] = payload
		}(i)
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("concurrent submissions failed: %v", failures)
	}
	if len(seen) != workers {
		t.Fatalf("distinct sequences = %d, want %d", len(seen), workers)
	}
	for seq, payload := range seen {
		msg, ok := b.Lookup(seq)
		if !ok {
			t.Fatalf("message %d is missing from the queue", seq)
		}
		if msg.Digest != digestOf(payload) {
			t.Fatalf("message %d digest does not match its payload", seq)
		}
	}
	if stats := b.Stats(); stats.Queued != workers {
		t.Fatalf("queued messages = %d, want %d", stats.Queued, workers)
	}
}

func TestStatsReflectsWindowUsage(t *testing.T) {
	limits := diodeConfig()
	limits.WindowMessages = 3
	limits.WindowBytes = 1000
	b := impl.NewDiodeBridge(limits)
	mustSubmit(t, b, "aa")
	mustSubmit(t, b, "bbb")
	stats := b.Stats()
	if stats.InFlight != 2 || stats.Queued != 2 {
		t.Fatalf("stats = %+v, want two messages held", stats)
	}
	if stats.BytesInFlight != 5 {
		t.Fatalf("bytes in flight = %d, want 5", stats.BytesInFlight)
	}
	if stats.Blocked {
		t.Fatalf("window with spare capacity must not report blocked")
	}
	mustSubmit(t, b, "c")
	if stats := b.Stats(); !stats.Blocked {
		t.Fatalf("full message window must report blocked, got %+v", stats)
	}
	if err := b.AcceptAck(okAck(1, 1)); err != nil {
		t.Fatalf("ack seq 1: %v", err)
	}
	stats = b.Stats()
	if stats.Blocked {
		t.Fatalf("window must unblock after an ack, got %+v", stats)
	}
	if stats.InFlight != 2 || stats.BytesInFlight != 4 || stats.Watermark != 1 {
		t.Fatalf("stats after the ack = %+v, want two in flight and four bytes", stats)
	}
}
