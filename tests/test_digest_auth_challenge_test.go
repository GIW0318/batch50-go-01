package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-02"
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

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newGuard(cfg impl.Config) (*impl.DigestChallengeGuard, *testClock) {
	clock := &testClock{at: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	cfg.Now = clock.Now
	if cfg.Realm == "" {
		cfg.Realm = "corp"
	}
	if cfg.NonceWindow == 0 {
		cfg.NonceWindow = 60 * time.Second
	}
	if cfg.MaxNoncesPerUser == 0 {
		cfg.MaxNoncesPerUser = 8
	}
	if cfg.MaxSessionsPerUser == 0 {
		cfg.MaxSessionsPerUser = 4
	}
	return impl.NewDigestChallengeGuard(cfg), clock
}

func sha256Hex(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])
}

func digestResponse(username, realm, password, nonce, method, uri, qop, cnonce string, nc int64) string {
	ha1 := sha256Hex(username + ":" + realm + ":" + password)
	ha2 := sha256Hex(method + ":" + uri)
	return sha256Hex(fmt.Sprintf("%s:%s:%08x:%s:%s:%s", ha1, nonce, nc, cnonce, qop, ha2))
}

func authRequest(h impl.Header, username, password, method, uri, cnonce string, nc int64) impl.AuthRequest {
	req := impl.AuthRequest{
		Username:    username,
		Realm:       h.Realm,
		Nonce:       h.Nonce,
		URI:         uri,
		Method:      method,
		Qop:         impl.QopAuth,
		NC:          nc,
		CNonce:      cnonce,
		Algorithm:   impl.AlgorithmSHA256,
		Fingerprint: "fp-1",
	}
	req.Response = digestResponse(username, h.Realm, password, h.Nonce, method, uri, req.Qop, cnonce, nc)
	return req
}

func TestChallengeHeaderFields(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if h.Realm != "corp" {
		t.Fatalf("challenge realm = %q, want corp", h.Realm)
	}
	if h.Nonce == "" || h.Opaque == "" {
		t.Fatalf("challenge must carry a nonce and an opaque token, got nonce=%q opaque=%q", h.Nonce, h.Opaque)
	}
	if h.Algorithm != impl.AlgorithmSHA256 {
		t.Fatalf("challenge algorithm = %q, want %q", h.Algorithm, impl.AlgorithmSHA256)
	}
	if h.Qop != impl.QopAuth {
		t.Fatalf("challenge qop = %q, want %q", h.Qop, impl.QopAuth)
	}
	if h.Stale {
		t.Fatalf("a fresh challenge must not be flagged stale")
	}
}

func TestChallengeRejectsBlankIdentityOrForeignRealm(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if _, err := g.Challenge("corp", "   "); !errors.Is(err, impl.ErrBadChallenge) {
		t.Fatalf("blank username must fail with ErrBadChallenge, got %v", err)
	}
	if _, err := g.Challenge("other", "alice"); !errors.Is(err, impl.ErrBadChallenge) {
		t.Fatalf("a foreign realm must fail with ErrBadChallenge, got %v", err)
	}
	if got := g.ActiveNonces("alice"); got != 0 {
		t.Fatalf("rejected challenges must not record nonces, got %d", got)
	}
}

func TestVerifyAcceptsCorrectDigest(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	req := authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)
	p, err := g.Verify(req)
	if err != nil {
		t.Fatalf("verify with the right digest: %v", err)
	}
	if p.Username != "alice" {
		t.Fatalf("principal username = %q, want alice", p.Username)
	}
	if p.NC != 1 {
		t.Fatalf("principal nonce count = %d, want 1", p.NC)
	}
	if usage := g.Usage("alice"); usage.MaxNC != 1 {
		t.Fatalf("usage watermark = %d, want 1", usage.MaxNC)
	}
}

func TestVerifyRejectsWrongPasswordAndUnknownUserAlike(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	badPassword := authRequest(h, "alice", "guess", "GET", "/api/orders", "c-1", 1)
	if _, err := g.Verify(badPassword); !errors.Is(err, impl.ErrAuthFailed) {
		t.Fatalf("wrong password must fail with ErrAuthFailed, got %v", err)
	}
	unknownUser := authRequest(h, "mallory", "guess", "GET", "/api/orders", "c-1", 1)
	if _, err := g.Verify(unknownUser); !errors.Is(err, impl.ErrAuthFailed) {
		t.Fatalf("unknown user must fail with ErrAuthFailed, got %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); err != nil {
		t.Fatalf("a failed attempt must not burn the nonce, got %v", err)
	}
}

func TestVerifyRejectsWeakAlgorithms(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if err := g.ValidateAlgorithm(impl.AlgorithmSHA256); err != nil {
		t.Fatalf("SHA-256 must be accepted, got %v", err)
	}
	for _, alg := range []string{impl.AlgorithmMD5, impl.AlgorithmNone, "SHA-1"} {
		if err := g.ValidateAlgorithm(alg); !errors.Is(err, impl.ErrAlgorithmRejected) {
			t.Fatalf("algorithm %q must be rejected, got %v", alg, err)
		}
		req := authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)
		req.Algorithm = alg
		if _, err := g.Verify(req); !errors.Is(err, impl.ErrAlgorithmRejected) {
			t.Fatalf("verify with algorithm %q must fail with ErrAlgorithmRejected, got %v", alg, err)
		}
	}
}

func TestVerifyRejectsNonceFromForeignRealm(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	req := authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)
	req.Realm = "other"
	req.Response = digestResponse("alice", "other", "s3cr3t", h.Nonce, "GET", "/api/orders", impl.QopAuth, "c-1", 1)
	if _, err := g.Verify(req); !errors.Is(err, impl.ErrNonceInvalid) {
		t.Fatalf("a nonce replayed under another realm must fail with ErrNonceInvalid, got %v", err)
	}
}

func TestVerifyRejectsExpiredNonce(t *testing.T) {
	g, clock := newGuard(impl.Config{NonceWindow: 30 * time.Second})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	clock.Advance(31 * time.Second)
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); !errors.Is(err, impl.ErrStaleNonce) {
		t.Fatalf("an expired nonce must fail with ErrStaleNonce, got %v", err)
	}
}

func TestVerifyRejectsReplayedNonceCount(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); err != nil {
		t.Fatalf("first use of nc=1 must succeed, got %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); !errors.Is(err, impl.ErrReplay) {
		t.Fatalf("the second use of nc=1 must fail with ErrReplay, got %v", err)
	}
}

func TestConcurrentReplaySucceedsOnce(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	req := authRequest(h, "alice", "s3cr3t", "POST", "/api/pay", "c-1", 1)
	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, replayed := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.Verify(req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, impl.ErrReplay):
				replayed++
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("concurrent replay succeeded %d times, want exactly 1", ok)
	}
	if replayed != workers-1 {
		t.Fatalf("concurrent replay reported %d ErrReplay, want %d", replayed, workers-1)
	}
}

func TestVerifyRejectsLowerNonceCount(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 5)); err != nil {
		t.Fatalf("nc=5 must be accepted as a first use, got %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 3)); !errors.Is(err, impl.ErrReplay) {
		t.Fatalf("nc=3 after nc=5 must fail with ErrReplay, got %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 6)); err != nil {
		t.Fatalf("nc=6 must be accepted after the watermark moved to 5, got %v", err)
	}
}

func TestUsageReportsNonceAccounting(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); !errors.Is(err, impl.ErrReplay) {
		t.Fatalf("second use must fail with ErrReplay, got %v", err)
	}
	usage := g.Usage("alice")
	if usage.Issued != 1 {
		t.Fatalf("usage issued = %d, want 1", usage.Issued)
	}
	if usage.Active != 1 {
		t.Fatalf("usage active = %d, want 1", usage.Active)
	}
	if usage.Replays != 1 {
		t.Fatalf("usage replays = %d, want 1", usage.Replays)
	}
	if usage.MaxNC != 1 {
		t.Fatalf("usage max nc = %d, want 1", usage.MaxNC)
	}
}

func TestRotateSecretInvalidatesOutstandingNonces(t *testing.T) {
	g, _ := newGuard(impl.Config{})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	old, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if err := g.RotateSecret(); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	if _, err := g.Verify(authRequest(old, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); !errors.Is(err, impl.ErrNonceInvalid) {
		t.Fatalf("a nonce from the retired generation must fail with ErrNonceInvalid, got %v", err)
	}
	fresh, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge after rotation: %v", err)
	}
	if _, err := g.Verify(authRequest(fresh, "alice", "s3cr3t", "GET", "/api/orders", "c-2", 1)); err != nil {
		t.Fatalf("a nonce from the new generation must work, got %v", err)
	}
}

func TestSweepReclaimsExpiredNonces(t *testing.T) {
	g, clock := newGuard(impl.Config{NonceWindow: 20 * time.Second})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := g.Challenge("corp", "alice"); err != nil {
			t.Fatalf("challenge %d: %v", i, err)
		}
		clock.Advance(time.Second)
	}
	if got := g.ActiveNonces("alice"); got != 3 {
		t.Fatalf("active nonces = %d, want 3", got)
	}
	clock.Advance(60 * time.Second)
	if swept := g.Sweep(clock.Now()); swept != 3 {
		t.Fatalf("sweep reclaimed %d nonces, want 3", swept)
	}
	if got := g.ActiveNonces("alice"); got != 0 {
		t.Fatalf("active nonces after the sweep = %d, want 0", got)
	}
}

func TestNonceCapacityEvictsOldest(t *testing.T) {
	g, clock := newGuard(impl.Config{NonceWindow: time.Hour, MaxNoncesPerUser: 2})
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	first, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge 1: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := g.Challenge("corp", "alice"); err != nil {
		t.Fatalf("challenge 2: %v", err)
	}
	clock.Advance(time.Second)
	third, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge 3: %v", err)
	}
	if got := g.ActiveNonces("alice"); got != 2 {
		t.Fatalf("active nonces = %d, want the cap 2", got)
	}
	if _, err := g.Verify(authRequest(first, "alice", "s3cr3t", "GET", "/api/orders", "c-1", 1)); !errors.Is(err, impl.ErrNonceInvalid) {
		t.Fatalf("the evicted nonce must fail with ErrNonceInvalid, got %v", err)
	}
	if _, err := g.Verify(authRequest(third, "alice", "s3cr3t", "GET", "/api/orders", "c-3", 1)); err != nil {
		t.Fatalf("the newest nonce must still work, got %v", err)
	}
}

func TestSnapshotRestoreKeepsNonceWatermarks(t *testing.T) {
	cfg := impl.Config{Realm: "corp", NonceWindow: time.Hour, MaxNoncesPerUser: 4, MaxSessionsPerUser: 2}
	g, _ := newGuard(cfg)
	if err := g.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h, err := g.Challenge("corp", "alice")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, err := g.Verify(authRequest(h, "alice", "s3cr3t", "POST", "/api/pay", "c-1", 2)); err != nil {
		t.Fatalf("verify nc=2: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored, _ := newGuard(cfg)
	if err := restored.SetPassword("alice", "s3cr3t"); err != nil {
		t.Fatalf("set password on the restored guard: %v", err)
	}
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := restored.Verify(authRequest(h, "alice", "s3cr3t", "POST", "/api/pay", "c-1", 2)); !errors.Is(err, impl.ErrReplay) {
		t.Fatalf("a restored watermark must still reject nc=2, got %v", err)
	}
	if _, err := restored.Verify(authRequest(h, "alice", "s3cr3t", "POST", "/api/pay", "c-1", 3)); err != nil {
		t.Fatalf("nc=3 must pass on the restored guard, got %v", err)
	}
}
