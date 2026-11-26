package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-d-10"
)

func chainConfig() impl.ChainConfig {
	return impl.ChainConfig{MaxEntries: 16, MaxPayload: 32, AnchorEvery: 0}
}

func appendN(t *testing.T, g *impl.AuditChainGuard, payloads ...string) {
	t.Helper()
	for i, payload := range payloads {
		if _, err := g.Append("alice", fmt.Sprintf("step-%d", i), []byte(payload)); err != nil {
			t.Fatalf("append %q: %v", payload, err)
		}
	}
}

func tamperField(t *testing.T, snap []byte, index int, field string, value any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(snap, &doc); err != nil {
		t.Fatalf("snapshot must be JSON: %v", err)
	}
	entries, ok := doc["entries"].([]any)
	if !ok || len(entries) <= index {
		t.Fatalf("snapshot must expose an entries array longer than %d", index)
	}
	row, ok := entries[index].(map[string]any)
	if !ok {
		t.Fatalf("entry %d must be a JSON object", index)
	}
	row[field] = value
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode tampered snapshot: %v", err)
	}
	return out
}

func TestAppendStartsChainAtOne(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	entry, err := g.Append("alice", "login", []byte("ok"))
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if entry.Seq != 1 {
		t.Fatalf("first sequence = %d, want 1", entry.Seq)
	}
	if entry.PrevHash != strings.Repeat("0", 64) {
		t.Fatalf("genesis predecessor = %q, want 64 zeroes", entry.PrevHash)
	}
	if len(entry.Hash) != 64 {
		t.Fatalf("digest length = %d, want 64", len(entry.Hash))
	}
	if strings.Trim(entry.Hash, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not lowercase hex", entry.Hash)
	}
	if entry.Actor != "alice" || entry.Action != "login" || entry.Payload != "ok" {
		t.Fatalf("entry content = %+v, want actor alice action login payload ok", entry)
	}
	if got := g.Len(); got != 1 {
		t.Fatalf("chain length = %d, want 1", got)
	}
	head, ok := g.Head()
	if !ok || head.Seq != 1 || head.Hash != entry.Hash {
		t.Fatalf("head = %+v (ok=%v), want the single appended entry", head, ok)
	}
}

func TestAppendLinksEveryEntry(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second", "third")
	entries := g.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i, entry := range entries {
		if entry.Seq != int64(i+1) {
			t.Fatalf("entries[%d].Seq = %d, want %d", i, entry.Seq, i+1)
		}
	}
	if entries[1].PrevHash != entries[0].Hash || entries[2].PrevHash != entries[1].Hash {
		t.Fatalf("predecessor links are broken: %q / %q / %q", entries[1].PrevHash, entries[0].Hash, entries[2].PrevHash)
	}
	entries[0].Actor = "mallory"
	if g.Entries()[0].Actor != "alice" {
		t.Fatalf("Entries must return a copy, the stored actor was rewritten")
	}
}

func TestAppendRejectsBlankActorAndAction(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	if _, err := g.Append("   ", "login", nil); !errors.Is(err, impl.ErrBlankActor) {
		t.Fatalf("blank actor must fail with ErrBlankActor, got %v", err)
	}
	if _, err := g.Append("alice", "", nil); !errors.Is(err, impl.ErrBlankAction) {
		t.Fatalf("blank action must fail with ErrBlankAction, got %v", err)
	}
	if got := g.Len(); got != 0 {
		t.Fatalf("rejected appends grew the chain to %d", got)
	}
}

func TestAppendRejectsOversizePayload(t *testing.T) {
	g := impl.NewAuditChainGuard(impl.ChainConfig{MaxEntries: 4, MaxPayload: 8, AnchorEvery: 0})
	if _, err := g.Append("alice", "bulk", []byte("123456789")); !errors.Is(err, impl.ErrPayloadTooLarge) {
		t.Fatalf("oversize payload must fail with ErrPayloadTooLarge, got %v", err)
	}
	if _, err := g.Append("alice", "bulk", []byte("12345678")); err != nil {
		t.Fatalf("payload at the limit must be accepted, got %v", err)
	}
	if got := g.Len(); got != 1 {
		t.Fatalf("chain length = %d, want 1", got)
	}
}

func TestAppendRejectsWhenChainFull(t *testing.T) {
	g := impl.NewAuditChainGuard(impl.ChainConfig{MaxEntries: 3, MaxPayload: 32, AnchorEvery: 0})
	appendN(t, g, "a", "b", "c")
	if _, err := g.Append("alice", "overflow", []byte("d")); !errors.Is(err, impl.ErrChainFull) {
		t.Fatalf("append past the limit must fail with ErrChainFull, got %v", err)
	}
	if got := g.Len(); got != 3 {
		t.Fatalf("chain length = %d, want 3", got)
	}
}

func TestVerifyAcceptsIntactAndEmptyChain(t *testing.T) {
	empty := impl.NewAuditChainGuard(chainConfig())
	if err := empty.Verify(); err != nil {
		t.Fatalf("empty chain must verify clean, got %v", err)
	}
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second", "third")
	if err := g.Verify(); err != nil {
		t.Fatalf("intact chain must verify clean, got %v", err)
	}
	if got := len(g.Alarms()); got != 0 {
		t.Fatalf("clean verification raised %d alarms, want 0", got)
	}
}

func TestVerifyDetectsTamperedPayloadAndFreezesAppends(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second", "third")
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	tampered := tamperField(t, snap, 1, "payload", "forged")
	if err := g.Restore(tampered); err != nil {
		t.Fatalf("a structurally intact snapshot must restore, got %v", err)
	}
	if err := g.Verify(); !errors.Is(err, impl.ErrChainBroken) {
		t.Fatalf("edited payload must fail with ErrChainBroken, got %v", err)
	}
	alarms := g.Alarms()
	if len(alarms) != 1 {
		t.Fatalf("alarm feed = %+v, want exactly one finding", alarms)
	}
	if alarms[0].Seq != 2 || alarms[0].Kind != impl.AlarmKindBrokenChain {
		t.Fatalf("alarm = %+v, want broken_chain at sequence 2", alarms[0])
	}
	if err := g.Verify(); !errors.Is(err, impl.ErrChainBroken) {
		t.Fatalf("second verification must still fail with ErrChainBroken, got %v", err)
	}
	if got := len(g.Alarms()); got != 1 {
		t.Fatalf("repeated verification duplicated the alarm, feed has %d", got)
	}
	if _, err := g.Append("alice", "late", []byte("x")); !errors.Is(err, impl.ErrChainBroken) {
		t.Fatalf("appends on a broken chain must fail with ErrChainBroken, got %v", err)
	}
}

func TestRotateAnchorsHeadAndRejectsWhenCurrent(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second", "third")
	head, ok := g.Head()
	if !ok {
		t.Fatalf("head missing before rotation")
	}
	anchor, err := g.Rotate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if anchor.Seq != head.Seq || anchor.Head != head.Hash {
		t.Fatalf("anchor = %+v, want head sequence %d digest %q", anchor, head.Seq, head.Hash)
	}
	if anchor.Index != 1 {
		t.Fatalf("first anchor index = %d, want 1", anchor.Index)
	}
	latest, ok := g.LatestAnchor()
	if !ok || latest.Head != head.Hash {
		t.Fatalf("latest anchor = %+v (ok=%v), want the fresh anchor", latest, ok)
	}
	if _, err := g.Rotate(); !errors.Is(err, impl.ErrAnchorCurrent) {
		t.Fatalf("rotating an already anchored head must fail with ErrAnchorCurrent, got %v", err)
	}
	empty := impl.NewAuditChainGuard(chainConfig())
	if _, err := empty.Rotate(); !errors.Is(err, impl.ErrNoEntries) {
		t.Fatalf("rotating an empty chain must fail with ErrNoEntries, got %v", err)
	}
}

func TestAutoAnchorAtFixedInterval(t *testing.T) {
	g := impl.NewAuditChainGuard(impl.ChainConfig{MaxEntries: 16, MaxPayload: 32, AnchorEvery: 4})
	appendN(t, g, "1", "2", "3", "4", "5", "6", "7", "8", "9")
	anchors := g.Anchors()
	if len(anchors) != 2 {
		t.Fatalf("anchors = %+v, want one at sequence 4 and one at 8", anchors)
	}
	if anchors[0].Seq != 4 || anchors[0].Index != 1 {
		t.Fatalf("first anchor = %+v, want sequence 4 index 1", anchors[0])
	}
	if anchors[1].Seq != 8 || anchors[1].Index != 2 {
		t.Fatalf("second anchor = %+v, want sequence 8 index 2", anchors[1])
	}
}

func TestRedactAppendsTombstone(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second", "third")
	entry, err := g.Redact(2, "pii-request-77")
	if err != nil {
		t.Fatalf("redact sequence 2: %v", err)
	}
	if entry.Action != "redact" {
		t.Fatalf("tombstone action = %q, want redact", entry.Action)
	}
	if entry.Seq != 4 {
		t.Fatalf("tombstone sequence = %d, want 4", entry.Seq)
	}
	if !strings.Contains(entry.Payload, "2") || !strings.Contains(entry.Payload, "pii-request-77") {
		t.Fatalf("tombstone payload = %q, want the target sequence and the reason", entry.Payload)
	}
	entries := g.Entries()
	if len(entries) != 4 || entries[1].Payload != "second" {
		t.Fatalf("entries after redaction = %+v, want the original payload untouched", entries)
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("redaction must keep the chain verifiable, got %v", err)
	}
}

func TestRedactRejectsUnknownAndDuplicate(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second")
	if _, err := g.Redact(9, "cleanup"); !errors.Is(err, impl.ErrBadRedaction) {
		t.Fatalf("unknown sequence must fail with ErrBadRedaction, got %v", err)
	}
	if _, err := g.Redact(1, "   "); !errors.Is(err, impl.ErrBadRedaction) {
		t.Fatalf("blank reason must fail with ErrBadRedaction, got %v", err)
	}
	if _, err := g.Redact(1, "cleanup"); err != nil {
		t.Fatalf("first redaction: %v", err)
	}
	if _, err := g.Redact(1, "cleanup-again"); !errors.Is(err, impl.ErrBadRedaction) {
		t.Fatalf("second redaction of the same sequence must fail with ErrBadRedaction, got %v", err)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	g := impl.NewAuditChainGuard(impl.ChainConfig{MaxEntries: 16, MaxPayload: 32, AnchorEvery: 2})
	appendN(t, g, "first", "second", "third", "fourth", "fifth")
	saved, ok := g.Head()
	if !ok {
		t.Fatalf("head missing before snapshot")
	}
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	appendN(t, g, "sixth", "seventh")
	if err := g.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := g.Len(); got != 5 {
		t.Fatalf("chain length after restore = %d, want 5", got)
	}
	head, ok := g.Head()
	if !ok || head.Hash != saved.Hash || head.Seq != saved.Seq {
		t.Fatalf("head after restore = %+v (ok=%v), want sequence %d digest %q", head, ok, saved.Seq, saved.Hash)
	}
	if got := len(g.Anchors()); got != 2 {
		t.Fatalf("anchors after restore = %d, want 2", got)
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("restored chain must verify clean, got %v", err)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second")
	if err := g.Restore([]byte("{\"entries\": [ {\"seq\": 1,")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("truncated payload must fail with ErrBadSnapshot, got %v", err)
	}
	if got := g.Len(); got != 2 {
		t.Fatalf("failed restore changed the live chain to %d entries", got)
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("failed restore damaged the chain: %v", err)
	}
}

func TestRestoreRejectsAnchorMismatch(t *testing.T) {
	g := impl.NewAuditChainGuard(chainConfig())
	appendN(t, g, "first", "second", "third")
	if _, err := g.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	saved, _ := g.Head()
	tampered := tamperField(t, snap, 2, "hash", strings.Repeat("ab", 32))
	if err := g.Restore(tampered); !errors.Is(err, impl.ErrAnchorMismatch) {
		t.Fatalf("a rewritten head under an anchor must fail with ErrAnchorMismatch, got %v", err)
	}
	if got := g.Len(); got != 3 {
		t.Fatalf("rejected restore changed the live chain to %d entries", got)
	}
	head, ok := g.Head()
	if !ok || head.Hash != saved.Hash {
		t.Fatalf("rejected restore moved the head to %+v, want %q", head, saved.Hash)
	}
}

func TestConcurrentAppendKeepsEveryRequest(t *testing.T) {
	g := impl.NewAuditChainGuard(impl.ChainConfig{MaxEntries: 64, MaxPayload: 32, AnchorEvery: 0})
	const writers = 40
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := g.Append(fmt.Sprintf("actor-%02d", n), "tick", []byte("x"))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append returned %v", err)
		}
	}
	if got := g.Len(); got != writers {
		t.Fatalf("chain length after concurrent appends = %d, want %d", got, writers)
	}
	if _, ok := g.Head(); !ok {
		t.Fatalf("chain head missing after concurrent appends")
	}
}
