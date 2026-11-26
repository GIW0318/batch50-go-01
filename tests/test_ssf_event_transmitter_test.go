package task

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-a-09"
)

const (
	tenantID = "tenant-a"
	signKey  = "tenant-a-signing-key"
)

type sendRecord struct {
	receiverID string
	endpoint   string
	token      string
}

type fakeSender struct {
	mu    sync.Mutex
	sends []sendRecord
	code  int
	err   error
}

func newFakeSender() *fakeSender {
	return &fakeSender{code: 200}
}

func (s *fakeSender) Deliver(receiverID, endpoint, token string) (int, error) {
	s.mu.Lock()
	s.sends = append(s.sends, sendRecord{receiverID: receiverID, endpoint: endpoint, token: token})
	code, err := s.code, s.err
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return code, nil
}

func (s *fakeSender) setResponse(code int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
	s.err = err
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

func (s *fakeSender) tokensFor(receiverID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.sends {
		if r.receiverID == receiverID {
			out = append(out, r.token)
		}
	}
	return out
}

func transmitterLimits() impl.Limits {
	return impl.Limits{MaxEvents: 8, MaxInFlight: 8}
}

func spec(id string, eventTypes, subjects []string, maxAttempts int) impl.ReceiverSpec {
	return impl.ReceiverSpec{
		ID:          id,
		TenantID:    tenantID,
		Endpoint:    "https://" + id + ".example.com/events",
		EventTypes:  eventTypes,
		Subjects:    subjects,
		MaxAttempts: maxAttempts,
	}
}

type tokenClaims struct {
	Iss    string   `json:"iss"`
	Aud    string   `json:"aud"`
	JTI    string   `json:"jti"`
	Iat    int64    `json:"iat"`
	Events []string `json:"events"`
}

type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

func decodeSegment(t *testing.T, segment string, out any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("segment is not the expected JSON: %v", err)
	}
}

func parseToken(t *testing.T, token string) (tokenHeader, tokenClaims, string, []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	var hdr tokenHeader
	decodeSegment(t, parts[0], &hdr)
	var claims tokenClaims
	decodeSegment(t, parts[1], &claims)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	return hdr, claims, parts[0] + "." + parts[1], sig
}

func TestPublishDeliversToMatchingReceiver(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-1", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-1",
		Scope:    []string{"u-1"},
		IssuedAt: 100,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if evtID == "" {
		t.Fatalf("publish must return a ledger id")
	}
	states, err := tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(states))
	}
	row := states[0]
	if row.ReceiverID != "rcv-1" {
		t.Fatalf("receiver = %q, want rcv-1", row.ReceiverID)
	}
	if row.Status != impl.StatusDelivered {
		t.Fatalf("status = %q, want %q", row.Status, impl.StatusDelivered)
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if row.LastCode != 200 {
		t.Fatalf("last code = %d, want 200", row.LastCode)
	}
	if got := snd.count(); got != 1 {
		t.Fatalf("sender calls = %d, want 1", got)
	}
}

func TestPublishRejectsInvalidEvent(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if _, err := tr.Publish(impl.SecurityEvent{TenantID: "  ", Type: impl.EventSessionRevoked, Scope: []string{"u-1"}}); !errors.Is(err, impl.ErrBlankTenant) {
		t.Fatalf("blank tenant must be rejected with ErrBlankTenant, got %v", err)
	}
	if _, err := tr.Publish(impl.SecurityEvent{TenantID: tenantID, Type: "password-guess", Scope: []string{"u-1"}}); !errors.Is(err, impl.ErrUnknownEventType) {
		t.Fatalf("unknown type must be rejected with ErrUnknownEventType, got %v", err)
	}
	if _, err := tr.Publish(impl.SecurityEvent{TenantID: tenantID, Type: impl.EventSessionRevoked}); !errors.Is(err, impl.ErrEmptySubject) {
		t.Fatalf("empty scope must be rejected with ErrEmptySubject, got %v", err)
	}
	if got := snd.count(); got != 0 {
		t.Fatalf("rejected events must not be sent, got %d calls", got)
	}
}

func TestSubscribeRejectsBlankReceiver(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.Subscribe(spec("  ", []string{impl.EventSessionRevoked}, nil, 3)); !errors.Is(err, impl.ErrBlankReceiver) {
		t.Fatalf("blank receiver id must be rejected with ErrBlankReceiver, got %v", err)
	}
	noEndpoint := spec("rcv-1", []string{impl.EventSessionRevoked}, nil, 3)
	noEndpoint.Endpoint = ""
	if err := tr.Subscribe(noEndpoint); !errors.Is(err, impl.ErrBlankReceiver) {
		t.Fatalf("blank endpoint must be rejected with ErrBlankReceiver, got %v", err)
	}
	if err := tr.Subscribe(spec("rcv-1", nil, nil, 3)); !errors.Is(err, impl.ErrNoEventTypes) {
		t.Fatalf("subscription without event types must be rejected with ErrNoEventTypes, got %v", err)
	}
	if got := len(tr.Subscribers(tenantID)); got != 0 {
		t.Fatalf("rejected subscriptions must not be stored, got %d", got)
	}
}

func TestSubscribeFiltersByEventType(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-revoke", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe revoke: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-disable", []string{impl.EventAccountDisabled}, nil, 3)); err != nil {
		t.Fatalf("subscribe disable: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventAccountDisabled,
		Subject:  "u-2",
		Scope:    []string{"u-2"},
		IssuedAt: 200,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	states, err := tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if len(states) != 1 || states[0].ReceiverID != "rcv-disable" {
		t.Fatalf("ledger rows = %+v, want only rcv-disable", states)
	}
	if got := snd.tokensFor("rcv-revoke"); len(got) != 0 {
		t.Fatalf("receiver subscribed to another type was targeted: %v", got)
	}
}

func TestSubscribeFiltersBySubject(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-scoped", []string{impl.EventSessionRevoked}, []string{"u-1"}, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	otherID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-2",
		Scope:    []string{"u-2"},
		IssuedAt: 300,
	})
	if err != nil {
		t.Fatalf("publish other subject: %v", err)
	}
	otherStates, err := tr.Delivery(string(otherID))
	if err != nil {
		t.Fatalf("delivery other subject: %v", err)
	}
	if len(otherStates) != 0 {
		t.Fatalf("out of scope event produced ledger rows: %+v", otherStates)
	}
	if got := snd.count(); got != 0 {
		t.Fatalf("out of scope event was sent, got %d calls", got)
	}
	matchingID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-1",
		Scope:    []string{"u-1", "u-2"},
		IssuedAt: 301,
	})
	if err != nil {
		t.Fatalf("publish matching subject: %v", err)
	}
	states, err := tr.Delivery(string(matchingID))
	if err != nil {
		t.Fatalf("delivery matching subject: %v", err)
	}
	if len(states) != 1 || states[0].Status != impl.StatusDelivered {
		t.Fatalf("matching event ledger = %+v, want one delivered row", states)
	}
}

func TestTokenCarriesAudienceAndUniqueJTI(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	for _, id := range []string{"rcv-a", "rcv-b"} {
		if err := tr.Subscribe(spec(id, []string{impl.EventCredentialChanged}, nil, 3)); err != nil {
			t.Fatalf("subscribe %s: %v", id, err)
		}
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventCredentialChanged,
		Subject:  "u-3",
		Scope:    []string{"u-3"},
		IssuedAt: 400,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	states, err := tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("ledger rows = %d, want one per receiver", len(states))
	}
	seenJTI := map[string]string{}
	for _, id := range []string{"rcv-a", "rcv-b"} {
		tokens := snd.tokensFor(id)
		if len(tokens) != 1 {
			t.Fatalf("receiver %s got %d tokens, want 1", id, len(tokens))
		}
		_, claims, _, _ := parseToken(t, tokens[0])
		if claims.Aud != id {
			t.Fatalf("token audience = %q, want %q", claims.Aud, id)
		}
		if claims.Iss != tenantID {
			t.Fatalf("token issuer = %q, want %q", claims.Iss, tenantID)
		}
		if claims.Iat != 400 {
			t.Fatalf("token issued at = %d, want 400", claims.Iat)
		}
		if claims.JTI == "" {
			t.Fatalf("token jti is empty")
		}
		if other, ok := seenJTI[claims.JTI]; ok {
			t.Fatalf("receivers %s and %s share the jti %q", other, id, claims.JTI)
		}
		seenJTI[claims.JTI] = id
		if len(claims.Events) != 1 || claims.Events[0] != impl.EventCredentialChanged {
			t.Fatalf("token events = %v, want the published type", claims.Events)
		}
	}
}

func TestTokenSignatureVerifies(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-sig", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-4",
		Scope:    []string{"u-4"},
		IssuedAt: 500,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	tokens := snd.tokensFor("rcv-sig")
	if len(tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(tokens))
	}
	hdr, _, signingInput, sig := parseToken(t, tokens[0])
	if hdr.Alg != "HS256" || hdr.Typ != "JWT" {
		t.Fatalf("token header = %+v, want HS256 and JWT", hdr)
	}
	mac := hmac.New(sha256.New, []byte(signKey))
	mac.Write([]byte(signingInput))
	want := mac.Sum(nil)
	if !hmac.Equal(sig, want) {
		t.Fatalf("signature does not verify against the tenant key")
	}
}

func TestPublishWithoutSigningKey(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.Subscribe(spec("rcv-nokey", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-5",
		Scope:    []string{"u-5"},
		IssuedAt: 600,
	})
	if !errors.Is(err, impl.ErrNoSigningKey) {
		t.Fatalf("publish without a key must fail with ErrNoSigningKey, got %v", err)
	}
	if got := snd.count(); got != 0 {
		t.Fatalf("unsigned events must not be sent, got %d calls", got)
	}
	if _, err := tr.Delivery(string(evtID)); !errors.Is(err, impl.ErrUnknownEvent) {
		t.Fatalf("failed publish must not leave a ledger entry, got %v", err)
	}
	if got := tr.Pending(); got != 0 {
		t.Fatalf("pending deliveries = %d, want 0", got)
	}
}

func TestFailedDeliveryStaysPendingUntilRetry(t *testing.T) {
	snd := newFakeSender()
	snd.setResponse(500, nil)
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-retry", []string{impl.EventSessionRevoked}, nil, 4)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-6",
		Scope:    []string{"u-6"},
		IssuedAt: 700,
	})
	if err != nil {
		t.Fatalf("publish must record the failure instead of failing, got %v", err)
	}
	states, err := tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(states))
	}
	if states[0].Status != impl.StatusPending {
		t.Fatalf("status = %q, want %q", states[0].Status, impl.StatusPending)
	}
	if states[0].Attempts != 1 || states[0].LastCode != 500 {
		t.Fatalf("first failure recorded as %+v, want one attempt with code 500", states[0])
	}
	if states[0].NextIn != 1 {
		t.Fatalf("backoff after the first attempt = %d, want 1", states[0].NextIn)
	}
	firstToken := snd.tokensFor("rcv-retry")[0]
	snd.setResponse(200, nil)
	if err := tr.Retry(string(evtID), "rcv-retry"); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	states, err = tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery after retry: %v", err)
	}
	if states[0].Status != impl.StatusDelivered || states[0].Attempts != 2 || states[0].LastCode != 200 {
		t.Fatalf("ledger after retry = %+v, want delivered on the second attempt", states[0])
	}
	tokens := snd.tokensFor("rcv-retry")
	if len(tokens) != 2 {
		t.Fatalf("sender calls for the receiver = %d, want 2", len(tokens))
	}
	if tokens[1] != firstToken {
		t.Fatalf("retry minted a different token, breaking receiver side idempotency")
	}
}

func TestRetryRejectsTerminalStates(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-term", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-7",
		Scope:    []string{"u-7"},
		IssuedAt: 800,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := tr.Retry(string(evtID), "rcv-term"); !errors.Is(err, impl.ErrAlreadyDelivered) {
		t.Fatalf("retry of a delivered row must fail with ErrAlreadyDelivered, got %v", err)
	}
	if err := tr.Retry("evt-missing", "rcv-term"); !errors.Is(err, impl.ErrUnknownEvent) {
		t.Fatalf("retry of an unknown event must fail with ErrUnknownEvent, got %v", err)
	}
	if err := tr.Retry(string(evtID), "rcv-other"); !errors.Is(err, impl.ErrUnknownReceiver) {
		t.Fatalf("retry for a receiver outside the event must fail with ErrUnknownReceiver, got %v", err)
	}
	if got := snd.count(); got != 1 {
		t.Fatalf("rejected retries must not send, got %d calls", got)
	}
}

func TestDeliveryExpiresAfterMaxAttempts(t *testing.T) {
	snd := newFakeSender()
	snd.setResponse(503, nil)
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-expire", []string{impl.EventSessionRevoked}, nil, 2)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-8",
		Scope:    []string{"u-8"},
		IssuedAt: 900,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := tr.Retry(string(evtID), "rcv-expire"); err != nil {
		t.Fatalf("second attempt must be accepted, got %v", err)
	}
	states, err := tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if states[0].Status != impl.StatusFailed {
		t.Fatalf("status after the last allowed attempt = %q, want %q", states[0].Status, impl.StatusFailed)
	}
	if states[0].Attempts != 2 || states[0].LastCode != 503 {
		t.Fatalf("ledger after the last attempt = %+v, want 2 attempts with code 503", states[0])
	}
	if err := tr.Retry(string(evtID), "rcv-expire"); !errors.Is(err, impl.ErrDeliveryExpired) {
		t.Fatalf("retry past the attempt cap must fail with ErrDeliveryExpired, got %v", err)
	}
	if got := snd.count(); got != 2 {
		t.Fatalf("sender calls = %d, want 2", got)
	}
}

func TestQueueCapacityRejectsPublish(t *testing.T) {
	snd := newFakeSender()
	snd.setResponse(500, nil)
	tr := impl.NewSetEventTransmitter(snd, impl.Limits{MaxEvents: 8, MaxInFlight: 1})
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-queue", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	firstID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-9",
		Scope:    []string{"u-9"},
		IssuedAt: 1000,
	})
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if got := tr.Pending(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	if _, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-10",
		Scope:    []string{"u-10"},
		IssuedAt: 1001,
	}); !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("publish over the in flight cap must fail with ErrQueueFull, got %v", err)
	}
	snd.setResponse(200, nil)
	if err := tr.Retry(string(firstID), "rcv-queue"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := tr.Pending(); got != 0 {
		t.Fatalf("pending after delivery = %d, want 0", got)
	}
	if _, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-10",
		Scope:    []string{"u-10"},
		IssuedAt: 1002,
	}); err != nil {
		t.Fatalf("publish after the queue drained must succeed, got %v", err)
	}
}

func TestRetentionEvictsOldestFinishedEvent(t *testing.T) {
	snd := newFakeSender()
	tr := impl.NewSetEventTransmitter(snd, impl.Limits{MaxEvents: 2, MaxInFlight: 8})
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-retain", []string{impl.EventSessionRevoked}, nil, 3)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := tr.Publish(impl.SecurityEvent{
			TenantID: tenantID,
			Type:     impl.EventSessionRevoked,
			Subject:  "u-11",
			Scope:    []string{"u-11"},
			IssuedAt: int64(1100 + i),
		})
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		ids = append(ids, string(id))
	}
	if _, err := tr.Delivery(ids[0]); !errors.Is(err, impl.ErrUnknownEvent) {
		t.Fatalf("oldest event must be evicted with ErrUnknownEvent, got %v", err)
	}
	for _, id := range ids[1:] {
		states, err := tr.Delivery(id)
		if err != nil {
			t.Fatalf("retained event %s: %v", id, err)
		}
		if len(states) != 1 || states[0].Status != impl.StatusDelivered {
			t.Fatalf("retained event %s ledger = %+v", id, states)
		}
	}
}

func TestConcurrentRetryDeliversOnce(t *testing.T) {
	snd := newFakeSender()
	snd.setResponse(500, nil)
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-race", []string{impl.EventSessionRevoked}, nil, 5)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-12",
		Scope:    []string{"u-12"},
		IssuedAt: 1200,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	snd.setResponse(200, nil)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := tr.Retry(string(evtID), "rcv-race"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successful concurrent retries = %d, want exactly 1", successes)
	}
	states, err := tr.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if states[0].Status != impl.StatusDelivered || states[0].Attempts != 2 {
		t.Fatalf("ledger after the race = %+v, want delivered with 2 attempts", states[0])
	}
	if got := len(snd.tokensFor("rcv-race")); got != 2 {
		t.Fatalf("tokens sent = %d, want one failure plus one success", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	snd := newFakeSender()
	snd.setResponse(500, nil)
	tr := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := tr.SetKey(tenantID, []byte(signKey)); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := tr.Subscribe(spec("rcv-snap", []string{impl.EventSessionRevoked}, nil, 4)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	evtID, err := tr.Publish(impl.SecurityEvent{
		TenantID: tenantID,
		Type:     impl.EventSessionRevoked,
		Subject:  "u-13",
		Scope:    []string{"u-13"},
		IssuedAt: 1300,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	firstToken := snd.tokensFor("rcv-snap")[0]
	snap, err := tr.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewSetEventTransmitter(snd, transmitterLimits())
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("restore of a fresh snapshot must succeed, got %v", err)
	}
	if got := len(restored.Subscribers(tenantID)); got != 1 {
		t.Fatalf("restored subscriptions = %d, want 1", got)
	}
	if got := restored.Pending(); got != 1 {
		t.Fatalf("restored pending deliveries = %d, want 1", got)
	}
	states, err := restored.Delivery(string(evtID))
	if err != nil {
		t.Fatalf("restored delivery: %v", err)
	}
	if states[0].Status != impl.StatusPending || states[0].Attempts != 1 || states[0].LastCode != 500 {
		t.Fatalf("restored ledger row = %+v, want the pending first attempt", states[0])
	}
	snd.setResponse(200, nil)
	if err := restored.Retry(string(evtID), "rcv-snap"); err != nil {
		t.Fatalf("retry after restore: %v", err)
	}
	tokens := snd.tokensFor("rcv-snap")
	if len(tokens) != 2 {
		t.Fatalf("tokens after restore retry = %d, want 2", len(tokens))
	}
	if tokens[1] != firstToken {
		t.Fatalf("retry after restore minted a new token, losing the jti")
	}
	if err := restored.Restore([]byte("garbage{}")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must be rejected with ErrBadSnapshot, got %v", err)
	}
	if _, err := restored.Delivery(string(evtID)); err != nil {
		t.Fatalf("rejected restore must keep the live ledger, got %v", err)
	}
}
