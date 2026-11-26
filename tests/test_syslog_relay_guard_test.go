package task

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-04"
)

var baseTime = time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)

func relayPolicy() impl.RelayPolicy {
	return impl.RelayPolicy{
		Framing:             impl.FramingOctetCounted,
		MaxBodyBytes:        64,
		QueueMaxMessages:    8,
		QueueMaxBytes:       4096,
		HighPrioritySources: []string{"edge-hi"},
		HighPriorityReserve: 2,
		AllowedCAs:          []string{"ca-alpha", "ca-beta"},
		ServerName:          "collector.example.com",
	}
}

func newGuard(t *testing.T, p impl.RelayPolicy) *impl.SyslogRelayGuard {
	t.Helper()
	g, err := impl.NewSyslogRelayGuard(p)
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	return g
}

func logMessage(source, body string) impl.Message {
	return impl.Message{
		Source:    source,
		Priority:  14,
		AppName:   "sshd",
		Timestamp: baseTime,
		Body:      []byte(body),
	}
}

func TestNewRelayRejectsBadPolicy(t *testing.T) {
	bad := relayPolicy()
	bad.Framing = "rfc9999"
	if _, err := impl.NewSyslogRelayGuard(bad); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("unknown framing: err = %v, want ErrBadPolicy", err)
	}
	bad = relayPolicy()
	bad.QueueMaxMessages = 0
	if _, err := impl.NewSyslogRelayGuard(bad); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("zero queue cap: err = %v, want ErrBadPolicy", err)
	}
	if _, err := impl.NewSyslogRelayGuard(relayPolicy()); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
}

func TestWriteQueuesPendingInOrder(t *testing.T) {
	g := newGuard(t, relayPolicy())
	if err := g.Write(logMessage("edge-1", "a")); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := g.Write(logMessage("edge-2", "bb")); err != nil {
		t.Fatalf("write second: %v", err)
	}
	pending := g.Pending()
	if len(pending) != 2 {
		t.Fatalf("pending length = %d, want 2", len(pending))
	}
	if pending[0].Source != "edge-1" || pending[1].Source != "edge-2" {
		t.Fatalf("pending order = %q, %q", pending[0].Source, pending[1].Source)
	}
	if pending[0].MsgID == "" || pending[1].MsgID == "" {
		t.Fatalf("queued messages must carry a message id")
	}
	if pending[0].MsgID == pending[1].MsgID {
		t.Fatalf("message ids collide: %q", pending[0].MsgID)
	}
	msgs, bytes := g.QueueDepth()
	if msgs != 2 || bytes != 3 {
		t.Fatalf("queue depth = (%d, %d), want (2, 3)", msgs, bytes)
	}
}

func TestWriteRejectsBlankSourceAndEmptyBody(t *testing.T) {
	g := newGuard(t, relayPolicy())
	if err := g.Write(logMessage("   ", "hello")); !errors.Is(err, impl.ErrBadMessage) {
		t.Fatalf("blank source: err = %v, want ErrBadMessage", err)
	}
	if err := g.Write(logMessage("edge-1", "")); !errors.Is(err, impl.ErrBadMessage) {
		t.Fatalf("empty body: err = %v, want ErrBadMessage", err)
	}
	if got := len(g.Pending()); got != 0 {
		t.Fatalf("rejected writes left %d pending messages", got)
	}
}

func TestNormalizeTruncatesOversizeBody(t *testing.T) {
	g := newGuard(t, relayPolicy())
	long := make([]byte, 100)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	msg := logMessage("edge-1", string(long))
	msg.MsgID = "m-9"
	out, err := g.Normalize(msg)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(out.Body) != 64 {
		t.Fatalf("body length = %d, want 64", len(out.Body))
	}
	if out.OriginalLen != 100 {
		t.Fatalf("original length = %d, want 100", out.OriginalLen)
	}
	short := logMessage("edge-1", "0123456")
	out, err = g.Normalize(short)
	if err != nil {
		t.Fatalf("normalize short: %v", err)
	}
	if len(out.Body) != 7 || out.OriginalLen != 7 {
		t.Fatalf("short message = %+v, want body and original length 7", out)
	}
}

func TestFrameOctetCountedUsesByteLength(t *testing.T) {
	g := newGuard(t, relayPolicy())
	msg := logMessage("edge-1", "hello")
	msg.MsgID = "m-1"
	frame, err := g.Frame(msg)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	sp := bytes.IndexByte(frame, ' ')
	if sp <= 0 {
		t.Fatalf("frame has no octet count prefix: %q", frame)
	}
	count, err := strconv.Atoi(string(frame[:sp]))
	if err != nil {
		t.Fatalf("octet count is not a number: %v", err)
	}
	payload := frame[sp+1:]
	if count != len(payload) {
		t.Fatalf("octet count = %d, payload bytes = %d", count, len(payload))
	}
	want := "<14>1 2026-11-26T08:00:00Z edge-1 sshd - m-1 - hello"
	if string(payload) != want {
		t.Fatalf("payload = %q, want %q", payload, want)
	}
}

func TestFrameNonTransparentTerminatesWithNewline(t *testing.T) {
	g := newGuard(t, relayPolicy())
	msg := logMessage("edge-1", "reconnect storm")
	msg.MsgID = "m-1"
	frame, err := g.FrameNonTransparent(msg)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	want := "<14>1 2026-11-26T08:00:00Z edge-1 sshd - m-1 - reconnect storm\n"
	if string(frame) != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
}

func TestVerifyPeerAcceptsTrustedCAAgainstSNI(t *testing.T) {
	g := newGuard(t, relayPolicy())
	chain := []impl.CertInfo{{
		CA:       "ca-alpha",
		Subject:  "CN=collector",
		DNSNames: []string{"collector.example.com"},
		NotAfter: baseTime.Add(24 * time.Hour),
	}}
	state, err := g.VerifyPeer(chain, "collector.example.com")
	if err != nil {
		t.Fatalf("verify peer: %v", err)
	}
	if !state.Verified || state.CA != "ca-alpha" {
		t.Fatalf("peer state = %+v", state)
	}
	if state.ServerName != "collector.example.com" {
		t.Fatalf("server name = %q", state.ServerName)
	}
	if state.Generation != 1 {
		t.Fatalf("generation = %d, want 1", state.Generation)
	}
	if got := g.Peer(); got != state {
		t.Fatalf("Peer() = %+v, want %+v", got, state)
	}
}

func TestVerifyPeerRejectsUnknownCA(t *testing.T) {
	g := newGuard(t, relayPolicy())
	chain := []impl.CertInfo{{
		CA:       "ca-rogue",
		DNSNames: []string{"collector.example.com"},
		NotAfter: baseTime.Add(24 * time.Hour),
	}}
	if _, err := g.VerifyPeer(chain, "collector.example.com"); !errors.Is(err, impl.ErrPeerUntrusted) {
		t.Fatalf("rogue CA: err = %v, want ErrPeerUntrusted", err)
	}
	if got := g.Peer(); got.Verified {
		t.Fatalf("failed verification must leave the peer unverified: %+v", got)
	}
}

func TestVerifyPeerRejectsServerNameMismatch(t *testing.T) {
	g := newGuard(t, relayPolicy())
	chain := []impl.CertInfo{{
		CA:       "ca-alpha",
		DNSNames: []string{"collector.example.com"},
		NotAfter: baseTime.Add(24 * time.Hour),
	}}
	if _, err := g.VerifyPeer(chain, "other.example.com"); !errors.Is(err, impl.ErrSNIMismatch) {
		t.Fatalf("name mismatch: err = %v, want ErrSNIMismatch", err)
	}
}

func TestVerifyPeerAllowsRotationWindowCA(t *testing.T) {
	g := newGuard(t, relayPolicy())
	chain := []impl.CertInfo{{
		CA:       "ca-beta",
		DNSNames: []string{"collector.example.com"},
		NotAfter: baseTime.Add(72 * time.Hour),
	}}
	state, err := g.VerifyPeer(chain, "collector.example.com")
	if err != nil {
		t.Fatalf("second CA must be accepted during rotation: %v", err)
	}
	if !state.Verified || state.CA != "ca-beta" {
		t.Fatalf("peer state = %+v", state)
	}
}

func TestFlushMovesPendingToInflightInOrder(t *testing.T) {
	g := newGuard(t, relayPolicy())
	if err := g.Write(logMessage("edge-1", "first")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Write(logMessage("edge-2", "second")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := len(g.Pending()); got != 0 {
		t.Fatalf("pending after flush = %d, want 0", got)
	}
	inflight := g.Inflight()
	if len(inflight) != 2 {
		t.Fatalf("inflight length = %d, want 2", len(inflight))
	}
	if inflight[0].Source != "edge-1" || inflight[1].Source != "edge-2" {
		t.Fatalf("inflight order = %q, %q", inflight[0].Source, inflight[1].Source)
	}
	if msgs, _ := g.QueueDepth(); msgs != 0 {
		t.Fatalf("queue depth after flush = %d, want 0", msgs)
	}
}

func TestAckRemovesInflightAndIsIdempotent(t *testing.T) {
	g := newGuard(t, relayPolicy())
	if err := g.Write(logMessage("edge-1", "first")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Write(logMessage("edge-2", "second")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	inflight := g.Inflight()
	if len(inflight) != 2 {
		t.Fatalf("inflight length = %d, want 2", len(inflight))
	}
	first := inflight[0].MsgID
	if err := g.Ack(first); err != nil {
		t.Fatalf("ack first: %v", err)
	}
	if got := len(g.Inflight()); got != 1 {
		t.Fatalf("inflight after first ack = %d, want 1", got)
	}
	if err := g.Ack(first); err != nil {
		t.Fatalf("repeated ack must stay idempotent, got %v", err)
	}
	if got := len(g.Inflight()); got != 1 {
		t.Fatalf("repeated ack changed the batch to %d", got)
	}
	if err := g.Ack(inflight[1].MsgID); err != nil {
		t.Fatalf("ack second: %v", err)
	}
	if got := len(g.Inflight()); got != 0 {
		t.Fatalf("inflight after all acks = %d, want 0", got)
	}
}

func TestQueueCapacityRejectsWhenFull(t *testing.T) {
	p := relayPolicy()
	p.QueueMaxMessages = 2
	p.QueueMaxBytes = 1 << 20
	g := newGuard(t, p)
	if err := g.Write(logMessage("edge-hi", "aa")); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := g.Write(logMessage("edge-hi", "bb")); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if err := g.Write(logMessage("edge-hi", "cc")); !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("third write: err = %v, want ErrQueueFull", err)
	}
	msgs, _ := g.QueueDepth()
	if msgs != 2 {
		t.Fatalf("queue depth = %d, want 2", msgs)
	}
}

func TestSetPolicyRejectsUnknownFraming(t *testing.T) {
	g := newGuard(t, relayPolicy())
	if err := g.Write(logMessage("edge-1", "keep me")); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := relayPolicy()
	bad.Framing = "rfc3164"
	if err := g.SetPolicy(bad); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("bad policy: err = %v, want ErrBadPolicy", err)
	}
	if got := len(g.Pending()); got != 1 {
		t.Fatalf("rejected policy change dropped queued messages: %d", got)
	}
	msg := logMessage("edge-1", "hello")
	msg.MsgID = "m-2"
	frame, err := g.Frame(msg)
	if err != nil {
		t.Fatalf("frame after rejected policy: %v", err)
	}
	if !bytes.Contains(frame, []byte("<14>1 ")) {
		t.Fatalf("framing no longer emits an RFC 5424 header: %q", frame)
	}
	good := relayPolicy()
	good.Framing = impl.FramingNonTransparent
	if err := g.SetPolicy(good); err != nil {
		t.Fatalf("valid policy change: %v", err)
	}
}

func TestSnapshotRestoreKeepsPendingAndWatermark(t *testing.T) {
	g := newGuard(t, relayPolicy())
	if err := g.Write(logMessage("edge-1", "alpha")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Write(logMessage("edge-2", "beta")); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := g.Pending()
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newGuard(t, relayPolicy())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after := restored.Pending()
	if len(after) != 2 {
		t.Fatalf("pending after restore = %d, want 2", len(after))
	}
	if after[0].MsgID != before[0].MsgID || after[1].MsgID != before[1].MsgID {
		t.Fatalf("restored message ids = %q, %q, want %q, %q", after[0].MsgID, after[1].MsgID, before[0].MsgID, before[1].MsgID)
	}
	if err := restored.Write(logMessage("edge-3", "gamma")); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
	fresh := restored.Pending()
	last := fresh[len(fresh)-1]
	for _, old := range before {
		if last.MsgID == old.MsgID {
			t.Fatalf("message id %q reused after restore", last.MsgID)
		}
	}
}
