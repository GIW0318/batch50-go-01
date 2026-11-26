package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-13"
)

func bindingLimits(now func() time.Time) impl.Limits {
	return impl.Limits{
		MaxInflight:      4,
		BindingTTL:       5 * time.Minute,
		MaxExporterBytes: 32,
		MaxTrace:         16,
		TrustedProxies:   []string{"proxy-1"},
		Now:              now,
	}
}

func openSession(t *testing.T, v *impl.ChannelBindingVerifier, sessionID string) impl.Binding {
	t.Helper()
	b, err := v.Open(sessionID, "alice", impl.ChannelRef{ID: "chan-1", Source: impl.SourceDirect})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	return b
}

func exporterDigest(exporter []byte) string {
	sum := sha256.Sum256(exporter)
	return hex.EncodeToString(sum[:])
}

func TestOpenRegistersUnboundSession(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	b, err := v.Open("sess-1", "alice", impl.ChannelRef{ID: "chan-1", Source: impl.SourceDirect})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if b.SessionID != "sess-1" || b.Principal != "alice" {
		t.Fatalf("session identity mismatch: %+v", b)
	}
	if b.ChannelID != "chan-1" || b.Source != impl.SourceDirect {
		t.Fatalf("channel reference mismatch: %+v", b)
	}
	if b.State != impl.StateUnbound {
		t.Fatalf("state = %q, want unbound", b.State)
	}
	if b.Seq != 1 {
		t.Fatalf("seq = %d, want 1 after open", b.Seq)
	}
	if got := v.Pending(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	if got := v.State("sess-1"); got != impl.StateUnbound {
		t.Fatalf("state query = %q, want unbound", got)
	}
	stored, ok := v.BindingOf("sess-1")
	if !ok || stored.Principal != "alice" {
		t.Fatalf("binding query = %+v ok=%v, want alice", stored, ok)
	}
}

func TestOpenRejectsBlankIdentity(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	if _, err := v.Open("  ", "alice", impl.ChannelRef{ID: "chan-1", Source: impl.SourceDirect}); !errors.Is(err, impl.ErrEmptyIdentity) {
		t.Fatalf("blank session must return ErrEmptyIdentity, got %v", err)
	}
	if _, err := v.Open("sess-1", "", impl.ChannelRef{ID: "chan-1", Source: impl.SourceDirect}); !errors.Is(err, impl.ErrEmptyIdentity) {
		t.Fatalf("blank principal must return ErrEmptyIdentity, got %v", err)
	}
	if _, err := v.Open("sess-1", "alice", impl.ChannelRef{ID: " ", Source: impl.SourceDirect}); !errors.Is(err, impl.ErrEmptyIdentity) {
		t.Fatalf("blank channel id must return ErrEmptyIdentity, got %v", err)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("rejected sessions must not be counted, got %d", got)
	}
}

func TestOpenRejectsDuplicateSession(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	if _, err := v.Open("sess-1", "bob", impl.ChannelRef{ID: "chan-2", Source: impl.SourceDirect}); !errors.Is(err, impl.ErrSessionExists) {
		t.Fatalf("duplicate session must return ErrSessionExists, got %v", err)
	}
	stored, _ := v.BindingOf("sess-1")
	if stored.Principal != "alice" {
		t.Fatalf("duplicate open must not replace the principal, got %q", stored.Principal)
	}
}

func TestOpenEnforcesInflightLimit(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	limits := bindingLimits(func() time.Time { return now })
	limits.MaxInflight = 2
	v := impl.NewChannelBindingVerifier(limits)
	openSession(t, v, "sess-1")
	openSession(t, v, "sess-2")
	if _, err := v.Open("sess-3", "alice", impl.ChannelRef{ID: "chan-3", Source: impl.SourceDirect}); !errors.Is(err, impl.ErrInflightLimit) {
		t.Fatalf("third session must return ErrInflightLimit, got %v", err)
	}
	if got := v.Pending(); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}

func TestOpenRejectsUnknownSource(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	if _, err := v.Open("sess-1", "alice", impl.ChannelRef{ID: "chan-1", Source: "raw"}); !errors.Is(err, impl.ErrBadSource) {
		t.Fatalf("unknown source must return ErrBadSource, got %v", err)
	}
	if got := v.State("sess-1"); got != "" {
		t.Fatalf("rejected open must not register a session, got %q", got)
	}
}

func TestBindStoresDigest(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	exporter := []byte("exporter-value-1")
	b, err := v.Bind("sess-1", exporter)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if b.State != impl.StateBound {
		t.Fatalf("state = %q, want bound", b.State)
	}
	if want := exporterDigest(exporter); b.Digest != want {
		t.Fatalf("digest = %q, want %q", b.Digest, want)
	}
	if !b.BoundAt.Equal(now) {
		t.Fatalf("bound at = %v, want %v", b.BoundAt, now)
	}
	if got := b.ExpiresAt.Sub(b.BoundAt); got != 5*time.Minute {
		t.Fatalf("binding ttl = %v, want 5m", got)
	}
	if b.Seq != 2 {
		t.Fatalf("seq = %d, want 2 after bind", b.Seq)
	}
}

func TestBindRejectsEmptyExporter(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	if _, err := v.Bind("sess-1", nil); !errors.Is(err, impl.ErrBadExporter) {
		t.Fatalf("empty exporter must return ErrBadExporter, got %v", err)
	}
	if got := v.State("sess-1"); got != impl.StateUnbound {
		t.Fatalf("rejected bind must keep the session unbound, got %q", got)
	}
	stored, _ := v.BindingOf("sess-1")
	if stored.Seq != 1 {
		t.Fatalf("rejected bind must not advance the sequence, got %d", stored.Seq)
	}
}

func TestBindRejectsOversizeExporter(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	if _, err := v.Bind("sess-1", make([]byte, 33)); !errors.Is(err, impl.ErrExporterTooLarge) {
		t.Fatalf("33 byte exporter must return ErrExporterTooLarge, got %v", err)
	}
	if got := v.State("sess-1"); got != impl.StateUnbound {
		t.Fatalf("oversize exporter must leave the session unbound, got %q", got)
	}
}

func TestBindOnceOnly(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	first := []byte("exporter-first")
	if _, err := v.Bind("sess-1", first); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if _, err := v.Bind("sess-1", []byte("exporter-second")); !errors.Is(err, impl.ErrAlreadyBound) {
		t.Fatalf("second bind must return ErrAlreadyBound, got %v", err)
	}
	stored, _ := v.BindingOf("sess-1")
	if stored.Digest != exporterDigest(first) {
		t.Fatalf("second bind must not replace the exporter digest")
	}
}

func TestVerifyAcceptsMatchingProof(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	exporter := []byte("exporter-value-1")
	if _, err := v.Bind("sess-1", exporter); err != nil {
		t.Fatalf("bind: %v", err)
	}
	verdict, err := v.Verify("sess-1", exporter)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verdict.Result != impl.VerdictMatch {
		t.Fatalf("verdict = %q, want match", verdict.Result)
	}
	if verdict.Reason != impl.ReasonMatch {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonMatch)
	}
	if verdict.Digest != exporterDigest(exporter) {
		t.Fatalf("verdict digest = %q, want %q", verdict.Digest, exporterDigest(exporter))
	}
	if !verdict.At.Equal(now) {
		t.Fatalf("verdict time = %v, want %v", verdict.At, now)
	}
	if got := v.State("sess-1"); got != impl.StateVerified {
		t.Fatalf("state = %q, want verified", got)
	}
	stored, _ := v.BindingOf("sess-1")
	if stored.Verifies != 1 {
		t.Fatalf("verify count = %d, want 1", stored.Verifies)
	}
	if stored.Seq != 3 {
		t.Fatalf("seq = %d, want 3 after verify", stored.Seq)
	}
}

func TestVerifyRejectsMismatch(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	exporter := []byte("exporter-value-1")
	if _, err := v.Bind("sess-1", exporter); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := v.Verify("sess-1", []byte("forged-proof")); !errors.Is(err, impl.ErrBindingMismatch) {
		t.Fatalf("wrong proof must return ErrBindingMismatch, got %v", err)
	}
	stored, _ := v.BindingOf("sess-1")
	if stored.Mismatches != 1 {
		t.Fatalf("mismatch count = %d, want 1", stored.Mismatches)
	}
	if stored.State != impl.StateBound {
		t.Fatalf("mismatch must keep the session bound, got %q", stored.State)
	}
	if _, err := v.Verify("sess-1", exporter); err != nil {
		t.Fatalf("correct proof after a mismatch must pass: %v", err)
	}
}

func TestVerifyUnknownSession(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	if _, err := v.Verify("sess-404", []byte("proof")); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("unknown session must return ErrSessionNotFound, got %v", err)
	}
	if got := v.State("sess-404"); got != "" {
		t.Fatalf("unknown session state = %q, want empty", got)
	}
	if _, ok := v.BindingOf("sess-404"); ok {
		t.Fatalf("unknown session must not have a binding")
	}
	if _, err := v.Explain("sess-404"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("explain of unknown session must return ErrSessionNotFound, got %v", err)
	}
}

func TestExpireThenVerifyFails(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	exporter := []byte("exporter-value-1")
	if _, err := v.Bind("sess-1", exporter); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := v.Expire("sess-1"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if got := v.State("sess-1"); got != impl.StateExpired {
		t.Fatalf("state = %q, want expired", got)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("pending after expire = %d, want 0", got)
	}
	if _, err := v.Verify("sess-1", exporter); !errors.Is(err, impl.ErrBindingExpired) {
		t.Fatalf("verify on an expired binding must return ErrBindingExpired, got %v", err)
	}
	if err := v.Expire("sess-1"); !errors.Is(err, impl.ErrAlreadyExpired) {
		t.Fatalf("second expire must return ErrAlreadyExpired, got %v", err)
	}
	if _, err := v.Bind("sess-1", exporter); !errors.Is(err, impl.ErrBindingExpired) {
		t.Fatalf("bind on an expired session must return ErrBindingExpired, got %v", err)
	}
}

func TestDowngradeRequiresReason(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	v := impl.NewChannelBindingVerifier(bindingLimits(func() time.Time { return now }))
	openSession(t, v, "sess-1")
	if err := v.Downgrade("sess-1", "   "); !errors.Is(err, impl.ErrBadReason) {
		t.Fatalf("blank reason must return ErrBadReason, got %v", err)
	}
	if err := v.Downgrade("sess-1", "no_exporter"); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if got := v.State("sess-1"); got != impl.StateDowngraded {
		t.Fatalf("state = %q, want downgraded", got)
	}
	if _, err := v.Verify("sess-1", []byte("proof")); !errors.Is(err, impl.ErrNotBound) {
		t.Fatalf("verify on a downgraded session must return ErrNotBound, got %v", err)
	}
	if err := v.Downgrade("sess-1", "again"); !errors.Is(err, impl.ErrAlreadyDowngraded) {
		t.Fatalf("second downgrade must return ErrAlreadyDowngraded, got %v", err)
	}
	trace, err := v.Explain("sess-1")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	want := []string{"open", "downgrade"}
	if len(trace.Steps) != len(want) || trace.Steps[0] != want[0] || trace.Steps[1] != want[1] {
		t.Fatalf("trace steps = %v, want %v", trace.Steps, want)
	}
}

func TestSnapshotRestoreInvalidatesBindings(t *testing.T) {
	now := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	v := impl.NewChannelBindingVerifier(bindingLimits(clock))
	openSession(t, v, "sess-1")
	exporter := []byte("exporter-value-1")
	if _, err := v.Bind("sess-1", exporter); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := v.Verify("sess-1", exporter); err != nil {
		t.Fatalf("verify: %v", err)
	}
	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewChannelBindingVerifier(bindingLimits(clock))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.State("sess-1"); got != impl.StateUnbound {
		t.Fatalf("restored state = %q, want unbound because the old channel is gone", got)
	}
	b, ok := restored.BindingOf("sess-1")
	if !ok {
		t.Fatalf("restored session must exist")
	}
	if b.Digest != "" || !b.BoundAt.IsZero() {
		t.Fatalf("restored binding must drop the digest and bound time, got %+v", b)
	}
	if b.Verifies != 1 {
		t.Fatalf("restored verify count = %d, want 1", b.Verifies)
	}
	newExporter := []byte("exporter-value-2")
	if _, err := restored.Bind("sess-1", newExporter); err != nil {
		t.Fatalf("bind on the restored session: %v", err)
	}
	verdict, err := restored.Verify("sess-1", newExporter)
	if err != nil {
		t.Fatalf("verify after rebind: %v", err)
	}
	if verdict.Result != impl.VerdictMatch {
		t.Fatalf("verdict = %q, want match", verdict.Result)
	}
	broken := impl.NewChannelBindingVerifier(bindingLimits(clock))
	if err := broken.Restore([]byte("{not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if got := broken.State("sess-1"); got != "" {
		t.Fatalf("failed restore must not apply partial state, got %q", got)
	}
}
