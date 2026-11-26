package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-11"
)

func testLimits(now func() time.Time) impl.Limits {
	return impl.Limits{
		MaxPendingPerDesktop: 3,
		DefaultTTL:           90 * time.Second,
		MaxTTL:               5 * time.Minute,
		MaxChallengeBytes:    64,
		MaxAudit:             32,
		Now:                  now,
	}
}

func validProof(relay impl.Relay) impl.Proof {
	return impl.Proof{
		Token:    relay.PairToken,
		PhoneKey: "phone-key-1",
		Nonce:    []byte("nonce-0001"),
	}
}

func mustStart(t *testing.T, r *impl.HybridTransportRelay, desktop string) impl.Relay {
	t.Helper()
	relay, err := r.Start(desktop, impl.RelayOpts{})
	if err != nil {
		t.Fatalf("start relay: %v", err)
	}
	return relay
}

func mustAttach(t *testing.T, r *impl.HybridTransportRelay, relay impl.Relay, phone string) impl.Channel {
	t.Helper()
	ch, err := r.Attach(relay.ID, phone, validProof(relay))
	if err != nil {
		t.Fatalf("attach phone: %v", err)
	}
	return ch
}

func assertionDigest(relayID string, assertion []byte) string {
	sum := sha256.Sum256([]byte(relayID + ":" + string(assertion)))
	return hex.EncodeToString(sum[:])
}

func TestStartCreatesPendingRelay(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay, err := r.Start("desktop-1", impl.RelayOpts{})
	if err != nil {
		t.Fatalf("start relay: %v", err)
	}
	if relay.ID != "relay-1" {
		t.Fatalf("relay id = %q, want relay-1", relay.ID)
	}
	if relay.State != impl.RelayPending {
		t.Fatalf("relay state = %q, want %q", relay.State, impl.RelayPending)
	}
	if relay.DesktopID != "desktop-1" {
		t.Fatalf("relay desktop = %q, want desktop-1", relay.DesktopID)
	}
	if relay.PairToken != "pair-relay-1" {
		t.Fatalf("pair token = %q, want pair-relay-1", relay.PairToken)
	}
	if !relay.CreatedAt.Equal(now) {
		t.Fatalf("created at = %v, want %v", relay.CreatedAt, now)
	}
	if got := relay.ExpiresAt.Sub(relay.CreatedAt); got != 90*time.Second {
		t.Fatalf("default ttl = %v, want 90s", got)
	}
	if relay.Seq != 1 {
		t.Fatalf("relay seq = %d, want 1 after start", relay.Seq)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayPending {
		t.Fatalf("relay state query = %q, want pending", got)
	}
	if got := r.Pending("desktop-1"); got != 1 {
		t.Fatalf("pending count = %d, want 1", got)
	}
	events := r.Audit(relay.ID)
	if len(events) != 1 {
		t.Fatalf("audit length = %d, want 1", len(events))
	}
	if events[0].Kind != impl.EventStarted || events[0].Actor != "desktop-1" {
		t.Fatalf("start event = %+v, want kind started by desktop-1", events[0])
	}
}

func TestStartRejectsBlankDesktop(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	if _, err := r.Start("   ", impl.RelayOpts{}); !errors.Is(err, impl.ErrEmptyIdentity) {
		t.Fatalf("blank desktop must return ErrEmptyIdentity, got %v", err)
	}
	if got := r.Pending("   "); got != 0 {
		t.Fatalf("rejected start must not be counted, got %d", got)
	}
	if got := r.RelayState("relay-1"); got != "" {
		t.Fatalf("unknown relay state = %q, want empty", got)
	}
}

func TestStartEnforcesPendingLimit(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	limits := testLimits(func() time.Time { return now })
	limits.MaxPendingPerDesktop = 2
	r := impl.NewHybridTransportRelay(limits)
	mustStart(t, r, "desktop-9")
	mustStart(t, r, "desktop-9")
	if _, err := r.Start("desktop-9", impl.RelayOpts{}); !errors.Is(err, impl.ErrRelayLimit) {
		t.Fatalf("third relay must hit ErrRelayLimit, got %v", err)
	}
	if got := r.Pending("desktop-9"); got != 2 {
		t.Fatalf("pending count = %d, want 2", got)
	}
	if got := r.RelayState("relay-3"); got != "" {
		t.Fatalf("rejected relay must stay unknown, got state %q", got)
	}
}

func TestStartClampsLimits(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	defaulted, err := r.Start("desktop-1", impl.RelayOpts{})
	if err != nil {
		t.Fatalf("start with zero ttl: %v", err)
	}
	if got := defaulted.ExpiresAt.Sub(defaulted.CreatedAt); got != 90*time.Second {
		t.Fatalf("zero ttl must use the 90s default, got %v", got)
	}
	capped, err := r.Start("desktop-1", impl.RelayOpts{TTL: time.Hour, MaxBytes: 4096})
	if err != nil {
		t.Fatalf("start with large ttl: %v", err)
	}
	if got := capped.ExpiresAt.Sub(capped.CreatedAt); got != 5*time.Minute {
		t.Fatalf("ttl must be capped at 5m, got %v", got)
	}
	if capped.MaxBytes != 64 {
		t.Fatalf("byte budget = %d, want the 64 byte cap", capped.MaxBytes)
	}
	if _, err := r.Start("desktop-1", impl.RelayOpts{MaxBytes: -1}); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("negative byte budget must return ErrBadLimit, got %v", err)
	}
	if got := r.Pending("desktop-1"); got != 2 {
		t.Fatalf("pending count after rejected start = %d, want 2", got)
	}
}

func TestAttachBindsBothEnds(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	ch, err := r.Attach(relay.ID, "phone-1", validProof(relay))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if ch.ID != "chan-1" {
		t.Fatalf("channel id = %q, want chan-1", ch.ID)
	}
	if ch.RelayID != relay.ID || ch.DesktopID != "desktop-1" || ch.PhoneID != "phone-1" {
		t.Fatalf("channel binding mismatch: %+v", ch)
	}
	if ch.State != impl.ChannelOpen {
		t.Fatalf("channel state = %q, want open", ch.State)
	}
	if ch.Credential != "cred-chan-1" {
		t.Fatalf("channel credential = %q, want cred-chan-1", ch.Credential)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayAttached {
		t.Fatalf("relay state after attach = %q, want attached", got)
	}
	stored, ok := r.ChannelInfo(ch.ID)
	if !ok {
		t.Fatalf("channel must be readable after attach")
	}
	if stored.PhoneID != "phone-1" {
		t.Fatalf("stored channel phone = %q, want phone-1", stored.PhoneID)
	}
	events := r.Audit(relay.ID)
	if len(events) != 2 || events[1].Kind != impl.EventAttached {
		t.Fatalf("attach audit = %+v, want an attached event", events)
	}
}

func TestAttachRejectsUnknownRelay(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	proof := impl.Proof{Token: "pair-relay-404", PhoneKey: "phone-key-1", Nonce: []byte("nonce-0001")}
	if _, err := r.Attach("relay-404", "phone-1", proof); !errors.Is(err, impl.ErrRelayNotFound) {
		t.Fatalf("unknown relay must return ErrRelayNotFound, got %v", err)
	}
	if _, ok := r.ChannelInfo("chan-1"); ok {
		t.Fatalf("failed attach must not create a channel")
	}
}

func TestAttachRejectsBadToken(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	bad := impl.Proof{Token: "pair-relay-2", PhoneKey: "phone-key-1", Nonce: []byte("nonce-0001")}
	if _, err := r.Attach(relay.ID, "phone-1", bad); !errors.Is(err, impl.ErrAttachDenied) {
		t.Fatalf("wrong pair token must return ErrAttachDenied, got %v", err)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayPending {
		t.Fatalf("denied attach must keep the relay pending, got %q", got)
	}
	if got := len(r.Audit(relay.ID)); got != 1 {
		t.Fatalf("denied attach must not write audit, got %d events", got)
	}
}

func TestAttachSecondAttemptDenied(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	first := mustAttach(t, r, relay, "phone-1")
	if _, err := r.Attach(relay.ID, "phone-2", validProof(relay)); !errors.Is(err, impl.ErrAttachDenied) {
		t.Fatalf("second attach must return ErrAttachDenied, got %v", err)
	}
	stored, ok := r.ChannelInfo(first.ID)
	if !ok || stored.PhoneID != "phone-1" {
		t.Fatalf("channel identity must stay on phone-1, got %+v ok=%v", stored, ok)
	}
	if got := r.Pending("desktop-1"); got != 1 {
		t.Fatalf("pending count = %d, want 1", got)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayAttached {
		t.Fatalf("relay state = %q, want attached", got)
	}
}

func TestForwardReturnsSeqEnvelope(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	ch := mustAttach(t, r, relay, "phone-1")
	first, err := r.Forward(ch.ID, []byte("challenge-1"))
	if err != nil {
		t.Fatalf("first forward: %v", err)
	}
	if want := fmt.Sprintf("channel=%s;seq=1;", ch.ID); string(first) != want {
		t.Fatalf("first envelope = %q, want %q", string(first), want)
	}
	second, err := r.Forward(ch.ID, []byte("challenge-2"))
	if err != nil {
		t.Fatalf("second forward: %v", err)
	}
	if want := fmt.Sprintf("channel=%s;seq=2;", ch.ID); string(second) != want {
		t.Fatalf("second envelope = %q, want %q", string(second), want)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayForwarded {
		t.Fatalf("relay state = %q, want forwarded", got)
	}
	stored, _ := r.ChannelInfo(ch.ID)
	if stored.Forwards != 2 {
		t.Fatalf("channel forwards = %d, want 2", stored.Forwards)
	}
	events := r.Audit(relay.ID)
	if len(events) != 3 || events[2].Kind != impl.EventForwarded {
		t.Fatalf("forward audit = %+v, want a forwarded event", events)
	}
}

func TestForwardRejectsOversizeChallenge(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	ch := mustAttach(t, r, relay, "phone-1")
	if _, err := r.Forward(ch.ID, make([]byte, 65)); !errors.Is(err, impl.ErrChallengeTooLarge) {
		t.Fatalf("65 byte challenge must return ErrChallengeTooLarge, got %v", err)
	}
	if _, err := r.Forward(ch.ID, nil); !errors.Is(err, impl.ErrEmptyChallenge) {
		t.Fatalf("empty challenge must return ErrEmptyChallenge, got %v", err)
	}
	resp, err := r.Forward(ch.ID, []byte("ok"))
	if err != nil {
		t.Fatalf("valid forward: %v", err)
	}
	if want := fmt.Sprintf("channel=%s;seq=1;", ch.ID); string(resp) != want {
		t.Fatalf("rejected forwards must not consume the sequence, got %q want %q", string(resp), want)
	}
	if got := len(r.Audit(relay.ID)); got != 3 {
		t.Fatalf("audit length = %d, want 3 events only", got)
	}
}

func TestCompleteReturnsDigestAndAudit(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	ch := mustAttach(t, r, relay, "phone-1")
	if _, err := r.Forward(ch.ID, []byte("challenge")); err != nil {
		t.Fatalf("forward: %v", err)
	}
	assertion := []byte("assertion-bytes")
	res, err := r.Complete(relay.ID, assertion)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.State != impl.RelayCompleted {
		t.Fatalf("result state = %q, want completed", res.State)
	}
	if want := assertionDigest(relay.ID, assertion); res.Digest != want {
		t.Fatalf("result digest = %q, want %q", res.Digest, want)
	}
	if res.Bytes != len(assertion) {
		t.Fatalf("result bytes = %d, want %d", res.Bytes, len(assertion))
	}
	if !res.CompletedAt.Equal(now) {
		t.Fatalf("completed at = %v, want %v", res.CompletedAt, now)
	}
	if res.Seq != 4 {
		t.Fatalf("result seq = %d, want 4 after the fourth transition", res.Seq)
	}
	events := r.Audit(relay.ID)
	if len(events) != 4 {
		t.Fatalf("audit length = %d, want 4", len(events))
	}
	last := events[3]
	if last.Kind != impl.EventCompleted || last.Digest != res.Digest {
		t.Fatalf("complete event = %+v, want the assertion digest", last)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayCompleted {
		t.Fatalf("relay state = %q, want completed", got)
	}
	if got := r.Pending("desktop-1"); got != 0 {
		t.Fatalf("pending count after complete = %d, want 0", got)
	}
}

func TestCompleteRejectsEmptyAssertion(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	ch := mustAttach(t, r, relay, "phone-1")
	if _, err := r.Forward(ch.ID, []byte("challenge")); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if _, err := r.Complete(relay.ID, nil); !errors.Is(err, impl.ErrEmptyAssertion) {
		t.Fatalf("empty assertion must return ErrEmptyAssertion, got %v", err)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayForwarded {
		t.Fatalf("relay state after rejection = %q, want forwarded", got)
	}
	if got := len(r.Audit(relay.ID)); got != 3 {
		t.Fatalf("rejected complete must not write audit, got %d events", got)
	}
}

func TestCancelClosesRelay(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	if err := r.Cancel(relay.ID, "desktop"); err != nil {
		t.Fatalf("cancel by desktop: %v", err)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayCanceled {
		t.Fatalf("relay state = %q, want canceled", got)
	}
	if got := r.Pending("desktop-1"); got != 0 {
		t.Fatalf("pending count = %d, want 0 after cancel", got)
	}
	events := r.Audit(relay.ID)
	last := events[len(events)-1]
	if last.Kind != impl.EventCanceled || last.Actor != "desktop" || last.Reason != impl.ReasonCanceledByDesktop {
		t.Fatalf("cancel event = %+v, want canceled by desktop", last)
	}
	if err := r.Cancel(relay.ID, "desktop"); !errors.Is(err, impl.ErrRelayClosed) {
		t.Fatalf("second cancel must return ErrRelayClosed, got %v", err)
	}
	if err := r.Cancel("relay-404", "desktop"); !errors.Is(err, impl.ErrRelayNotFound) {
		t.Fatalf("unknown relay must return ErrRelayNotFound, got %v", err)
	}
	other := mustStart(t, r, "desktop-1")
	if err := r.Cancel(other.ID, "browser"); !errors.Is(err, impl.ErrBadActor) {
		t.Fatalf("unknown actor must return ErrBadActor, got %v", err)
	}
	if got := r.RelayState(other.ID); got != impl.RelayPending {
		t.Fatalf("bad actor must not change state, got %q", got)
	}
	if err := r.Cancel(other.ID, "phone"); err != nil {
		t.Fatalf("cancel by phone: %v", err)
	}
	events = r.Audit(other.ID)
	last = events[len(events)-1]
	if last.Actor != "phone" || last.Reason != impl.ReasonCanceledByPhone {
		t.Fatalf("phone cancel event = %+v, want canceled by phone", last)
	}
}

func TestSweepExpiresRelays(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := impl.NewHybridTransportRelay(testLimits(func() time.Time { return now }))
	relay := mustStart(t, r, "desktop-1")
	if got := r.Sweep(); got != 0 {
		t.Fatalf("sweep before the deadline = %d, want 0", got)
	}
	now = now.Add(90 * time.Second)
	if got := r.Sweep(); got != 1 {
		t.Fatalf("sweep at the deadline = %d, want 1", got)
	}
	if got := r.RelayState(relay.ID); got != impl.RelayExpired {
		t.Fatalf("relay state = %q, want expired", got)
	}
	if got := r.Pending("desktop-1"); got != 0 {
		t.Fatalf("pending count = %d, want 0 after expiry", got)
	}
	events := r.Audit(relay.ID)
	last := events[len(events)-1]
	if last.Kind != impl.EventExpired || last.Reason != impl.ReasonRelayTimeout {
		t.Fatalf("expiry event = %+v, want expired with the timeout reason", last)
	}
	if got := r.Sweep(); got != 0 {
		t.Fatalf("second sweep = %d, want 0", got)
	}
	if _, err := r.Attach(relay.ID, "phone-1", validProof(relay)); !errors.Is(err, impl.ErrRelayExpired) {
		t.Fatalf("attach on an expired relay must return ErrRelayExpired, got %v", err)
	}
}

func TestSnapshotRestoreKeepsForwardedRelayAndAudit(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	r := impl.NewHybridTransportRelay(testLimits(clock))
	open := mustStart(t, r, "desktop-1")
	ch := mustAttach(t, r, open, "phone-1")
	if _, err := r.Forward(ch.ID, []byte("challenge")); err != nil {
		t.Fatalf("forward: %v", err)
	}
	closed := mustStart(t, r, "desktop-1")
	ch2 := mustAttach(t, r, closed, "phone-2")
	if _, err := r.Forward(ch2.ID, []byte("challenge")); err != nil {
		t.Fatalf("forward second relay: %v", err)
	}
	assertion := []byte("assertion-2")
	res, err := r.Complete(closed.ID, assertion)
	if err != nil {
		t.Fatalf("complete second relay: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewHybridTransportRelay(testLimits(clock))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.RelayState(open.ID); got != impl.RelayForwarded {
		t.Fatalf("restored open relay state = %q, want forwarded", got)
	}
	stored, ok := restored.ChannelInfo(ch.ID)
	if !ok || stored.PhoneID != "phone-1" || stored.Credential != "cred-chan-1" {
		t.Fatalf("restored channel = %+v ok=%v, want the original binding", stored, ok)
	}
	if got := restored.Pending("desktop-1"); got != 1 {
		t.Fatalf("restored pending count = %d, want 1", got)
	}
	if got := restored.RelayState(closed.ID); got != impl.RelayCompleted {
		t.Fatalf("restored completed relay state = %q, want completed", got)
	}
	closedEvents := restored.Audit(closed.ID)
	if len(closedEvents) == 0 || closedEvents[len(closedEvents)-1].Digest != res.Digest {
		t.Fatalf("restored audit must keep the assertion digest, got %+v", closedEvents)
	}
	replayed, err := restored.Complete(open.ID, []byte("assertion-1"))
	if err != nil {
		t.Fatalf("complete the restored open relay: %v", err)
	}
	if want := assertionDigest(open.ID, []byte("assertion-1")); replayed.Digest != want {
		t.Fatalf("restored complete digest = %q, want %q", replayed.Digest, want)
	}
	next := mustStart(t, restored, "desktop-1")
	if next.ID != "relay-3" {
		t.Fatalf("relay id after restore = %q, want relay-3", next.ID)
	}
	broken := impl.NewHybridTransportRelay(testLimits(clock))
	if err := broken.Restore([]byte("{not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if got := broken.RelayState(open.ID); got != "" {
		t.Fatalf("failed restore must not apply partial state, got %q", got)
	}
}
