package task

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-04"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func baseConfig() impl.Config {
	return impl.Config{
		Endpoint:             "https://as.example/authorize",
		MaxObjectBytes:       4096,
		MaxParams:            32,
		MaxInFlightPerClient: 8,
		MaxTokenAge:          5 * time.Minute,
		JTITTL:               10 * time.Minute,
	}
}

func newGuard(clock *testClock, cfg impl.Config) *impl.RequestObjectGuard {
	cfg.Now = clock.Now
	return impl.NewRequestObjectGuard(cfg)
}

func newKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

func b64(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func signWith(t *testing.T, priv ed25519.PrivateKey, alg, kid string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "oauth-authz-req+jwt", "kid": kid}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("encode header: %v", err)
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encode claims: %v", err)
	}
	signing := b64(hb) + "." + b64(pb)
	return signing + "." + b64(ed25519.Sign(priv, []byte(signing)))
}

func baseClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":           "client-a",
		"client_id":     "client-a",
		"aud":           "https://as.example/authorize",
		"response_type": "code",
		"redirect_uri":  "https://client.example/cb",
		"scope":         "openid profile",
		"exp":           now.Add(2 * time.Minute).Unix(),
		"iat":           now.Unix(),
		"jti":           "jti-1",
	}
}

func registeredClient(pub ed25519.PublicKey) impl.Client {
	return impl.Client{
		ClientID: "client-a",
		Algs:     []string{impl.AlgEdDSA},
		Keys:     []impl.ClientKey{{KID: "k1", PublicKey: pub}},
	}
}

func TestParseRejectsMalformedJWS(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	g := newGuard(clock, baseConfig())
	for _, raw := range []string{"", "abc", "a.b", "a.b.c.d", "!!!.###.$$$"} {
		if _, err := g.Parse(raw); !errors.Is(err, impl.ErrMalformed) {
			t.Fatalf("request object %q must fail with ErrMalformed, got %v", raw, err)
		}
	}
}

func TestParseExtractsHeaderClaimsAndSigningInput(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	_, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	raw := signWith(t, priv, impl.AlgEdDSA, "k1", baseClaims(clock.Now()))
	ro, err := g.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := ro.Header["alg"]; got != impl.AlgEdDSA {
		t.Fatalf("header alg = %v, want %q", got, impl.AlgEdDSA)
	}
	if got := ro.Header["kid"]; got != "k1" {
		t.Fatalf("header kid = %v, want k1", got)
	}
	if got := ro.Claims["iss"]; got != "client-a" {
		t.Fatalf("claim iss = %v, want client-a", got)
	}
	if want := raw[:strings.LastIndex(raw, ".")]; ro.SigningInput != want {
		t.Fatalf("signing input = %q, want the first two segments %q", ro.SigningInput, want)
	}
	if len(ro.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature length = %d, want %d", len(ro.Signature), ed25519.SignatureSize)
	}
}

func TestVerifyAcceptsRegisteredEd25519Client(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	client, ok := g.Client("client-a")
	if !ok {
		t.Fatalf("registered client must be readable")
	}
	raw := signWith(t, priv, impl.AlgEdDSA, "k1", baseClaims(clock.Now()))
	ro, err := g.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := g.Verify(ro, client); err != nil {
		t.Fatalf("verify a well formed request object: %v", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	client, _ := g.Client("client-a")
	raw := signWith(t, priv, impl.AlgEdDSA, "k1", baseClaims(clock.Now()))
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("signed object must have three segments, got %d", len(parts))
	}
	swapped := baseClaims(clock.Now())
	swapped["scope"] = "openid profile admin"
	pb, err := json.Marshal(swapped)
	if err != nil {
		t.Fatalf("encode swapped claims: %v", err)
	}
	tampered := parts[0] + "." + b64(pb) + "." + parts[2]
	ro, err := g.Parse(tampered)
	if err != nil {
		t.Fatalf("parse tampered object: %v", err)
	}
	if err := g.Verify(ro, client); !errors.Is(err, impl.ErrBadSignature) {
		t.Fatalf("a replaced payload must fail with ErrBadSignature, got %v", err)
	}
}

func TestVerifyRejectsNoneAndHMACAlgorithms(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	client := registeredClient(pub)
	client.Algs = append(client.Algs, impl.AlgHS256, impl.AlgNone)
	if err := g.RegisterClient(client); err != nil {
		t.Fatalf("register client: %v", err)
	}
	for _, alg := range []string{impl.AlgNone, impl.AlgHS256} {
		raw := signWith(t, priv, alg, "k1", baseClaims(clock.Now()))
		ro, err := g.Parse(raw)
		if err != nil {
			t.Fatalf("parse object with alg %q: %v", alg, err)
		}
		if err := g.Verify(ro, client); !errors.Is(err, impl.ErrAlgNotAllowed) {
			t.Fatalf("alg %q must fail with ErrAlgNotAllowed, got %v", alg, err)
		}
	}
}

func TestVerifyRejectsIssuerMismatch(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	client, _ := g.Client("client-a")
	claims := baseClaims(clock.Now())
	claims["iss"] = "client-b"
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k1", claims))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := g.Verify(ro, client); !errors.Is(err, impl.ErrIssuerMismatch) {
		t.Fatalf("iss differing from client_id must fail with ErrIssuerMismatch, got %v", err)
	}
}

func TestVerifyRejectsAudienceMismatch(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	client, _ := g.Client("client-a")
	claims := baseClaims(clock.Now())
	claims["aud"] = "https://as.example/other"
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k1", claims))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := g.Verify(ro, client); !errors.Is(err, impl.ErrAudienceMismatch) {
		t.Fatalf("aud without this endpoint must fail with ErrAudienceMismatch, got %v", err)
	}
}

func TestVerifyRejectsExpiredObject(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	client, _ := g.Client("client-a")
	claims := baseClaims(clock.Now())
	claims["iat"] = clock.Now().Add(-10 * time.Minute).Unix()
	claims["exp"] = clock.Now().Add(-time.Minute).Unix()
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k1", claims))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := g.Verify(ro, client); !errors.Is(err, impl.ErrExpired) {
		t.Fatalf("an expired object must fail with ErrExpired, got %v", err)
	}
}

func TestVerifyRejectsUnknownKeyID(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	client, _ := g.Client("client-a")
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k9", baseClaims(clock.Now())))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := g.Verify(ro, client); !errors.Is(err, impl.ErrUnknownKey) {
		t.Fatalf("an unregistered kid must fail with ErrUnknownKey, got %v", err)
	}
}

func TestMergeInheritsOnlyMissingParams(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	_, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	claims := baseClaims(clock.Now())
	claims["nonce"] = "n-9"
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k1", claims))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := url.Values{}
	query.Set("state", "st-42")
	query.Set("prompt", "login")
	merged, err := g.Merge(query, ro)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.ClientID != "client-a" || merged.ResponseType != "code" {
		t.Fatalf("merged client=%q response_type=%q, want client-a/code", merged.ClientID, merged.ResponseType)
	}
	if merged.Scope != "openid profile" {
		t.Fatalf("merged scope = %q, want the request object value", merged.Scope)
	}
	if merged.State != "st-42" {
		t.Fatalf("merged state = %q, want the query value st-42", merged.State)
	}
	if merged.Nonce != "n-9" {
		t.Fatalf("merged nonce = %q, want n-9", merged.Nonce)
	}
	if merged.Extra["prompt"] != "login" {
		t.Fatalf("merged extra prompt = %q, want login", merged.Extra["prompt"])
	}
	if _, ok := merged.Extra["state"]; ok {
		t.Fatalf("a known parameter must not leak into the extra map")
	}
}

func TestMergeRejectsConflictingValues(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	_, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	claims := baseClaims(clock.Now())
	claims["state"] = "from-object"
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k1", claims))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := url.Values{}
	query.Set("state", "from-query")
	if _, err := g.Merge(query, ro); !errors.Is(err, impl.ErrParamConflict) {
		t.Fatalf("differing values for one parameter must fail with ErrParamConflict, got %v", err)
	}
	same := url.Values{}
	same.Set("state", "from-object")
	if _, err := g.Merge(same, ro); err != nil {
		t.Fatalf("identical values must merge, got %v", err)
	}
}

func TestMergeRejectsRequestAndRequestURI(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	_, priv := newKeyPair(t)
	g := newGuard(clock, baseConfig())
	ro, err := g.Parse(signWith(t, priv, impl.AlgEdDSA, "k1", baseClaims(clock.Now())))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := url.Values{}
	query.Set("request", "eyJhbGciOiJFZERTQSJ9.e30.c2ln")
	query.Set("request_uri", "https://as.example/req/1")
	if _, err := g.Merge(query, ro); !errors.Is(err, impl.ErrRequestConflict) {
		t.Fatalf("request together with request_uri must fail with ErrRequestConflict, got %v", err)
	}
}

func TestConsumeIsSingleUseAndConcurrent(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	g := newGuard(clock, baseConfig())
	if err := g.Consume("jti-1"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if !g.Consumed("jti-1") {
		t.Fatalf("a consumed jti must be reported as burnt")
	}
	if err := g.Consume("jti-1"); !errors.Is(err, impl.ErrJTReused) {
		t.Fatalf("second consume must fail with ErrJTReused, got %v", err)
	}
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, reused := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.Consume("jti-2")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, impl.ErrJTReused):
				reused++
			}
		}()
	}
	wg.Wait()
	if winners != 1 || reused != workers-1 {
		t.Fatalf("concurrent consume winners=%d reused=%d, want 1 and %d", winners, reused, workers-1)
	}
}

func TestSnapshotRestoreKeepsClientAndTombstones(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	pub, _ := newKeyPair(t)
	cfg := baseConfig()
	g := newGuard(clock, cfg)
	if err := g.RegisterClient(registeredClient(pub)); err != nil {
		t.Fatalf("register client: %v", err)
	}
	if err := g.Consume("jti-3"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := newGuard(clock, cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, ok := restored.Client("client-a"); !ok {
		t.Fatalf("restored guard lost the client registration")
	}
	if !restored.Consumed("jti-3") {
		t.Fatalf("restored guard lost the jti tombstone")
	}
	if err := restored.Consume("jti-3"); !errors.Is(err, impl.ErrJTReused) {
		t.Fatalf("a restored tombstone must still reject the jti, got %v", err)
	}
}

func TestParseRejectsOversizedObject(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)}
	cfg := baseConfig()
	cfg.MaxObjectBytes = 64
	_, priv := newKeyPair(t)
	g := newGuard(clock, cfg)
	claims := baseClaims(clock.Now())
	claims["scope"] = strings.Repeat("openid ", 20)
	raw := signWith(t, priv, impl.AlgEdDSA, "k1", claims)
	if len(raw) <= cfg.MaxObjectBytes {
		t.Fatalf("test object is %d bytes, must exceed the %d byte cap", len(raw), cfg.MaxObjectBytes)
	}
	if _, err := g.Parse(raw); !errors.Is(err, impl.ErrObjectTooLarge) {
		t.Fatalf("an oversized object must fail with ErrObjectTooLarge, got %v", err)
	}
}
