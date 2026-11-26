package task

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-a-10"
)

const (
	scramTenant   = "tenant-a"
	scramUsername = "alice"
	scramPassword = "pencil-42"
	scramIters    = 2048
)

func hmacSHA256(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	var out []byte
	for block := 1; len(out) < keyLen; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iterations; i++ {
			mac := hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func makeCredential(t *testing.T, username, password string, iterations int) impl.StoredCredential {
	t.Helper()
	salt := []byte("salt:" + username)
	salted := pbkdf2SHA256([]byte(password), salt, iterations, 32)
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	return impl.StoredCredential{
		TenantID:   scramTenant,
		Username:   username,
		Salt:       salt,
		Iterations: iterations,
		StoredKey:  storedKey[:],
		ServerKey:  hmacSHA256(salted, "Server Key"),
	}
}

type staticStore struct {
	mu    sync.Mutex
	creds map[string]impl.StoredCredential
}

func newStore() *staticStore {
	return &staticStore{creds: map[string]impl.StoredCredential{}}
}

func (s *staticStore) put(c impl.StoredCredential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[c.TenantID+"/"+c.Username] = c
}

func (s *staticStore) Lookup(tenantID, username string) (impl.StoredCredential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[tenantID+"/"+username]
	return c, ok
}

func scramPolicy() impl.ScramPolicy {
	return impl.ScramPolicy{MinIterations: 1024, MaxInflight: 4, StateTTL: 60, MaxFailures: 3}
}

func clientFirstMsg(gs2, username, nonce string) ([]byte, string) {
	bare := "n=" + username + ",r=" + nonce
	return []byte(gs2 + bare), bare
}

func clientFinalMsg(cbindB64, nonce, proofB64 string) ([]byte, string) {
	noProof := "c=" + cbindB64 + ",r=" + nonce
	return []byte(noProof + ",p=" + proofB64), noProof
}

func cbindValue(gs2 string, data []byte) string {
	return base64.StdEncoding.EncodeToString(append([]byte(gs2), data...))
}

type serverFirstParts struct {
	nonce      string
	salt       []byte
	iterations int
}

func parseServerFirst(t *testing.T, raw []byte) serverFirstParts {
	t.Helper()
	var out serverFirstParts
	out.iterations = -1
	for _, field := range strings.Split(string(raw), ",") {
		switch {
		case strings.HasPrefix(field, "r="):
			out.nonce = field[2:]
		case strings.HasPrefix(field, "s="):
			salt, err := base64.StdEncoding.DecodeString(field[2:])
			if err != nil {
				t.Fatalf("salt is not base64: %v", err)
			}
			out.salt = salt
		case strings.HasPrefix(field, "i="):
			n := 0
			for _, ch := range field[2:] {
				if ch < '0' || ch > '9' {
					t.Fatalf("iteration field %q is not a number", field)
				}
				n = n*10 + int(ch-'0')
			}
			out.iterations = n
		}
	}
	if out.nonce == "" || out.salt == nil || out.iterations < 0 {
		t.Fatalf("server-first %q is missing a field", string(raw))
	}
	return out
}

func scramProof(t *testing.T, cred impl.StoredCredential, password, clientFirstBare string, serverFirst []byte, finalNoProof string) (string, []byte) {
	t.Helper()
	salted := pbkdf2SHA256([]byte(password), cred.Salt, cred.Iterations, 32)
	clientKey := hmacSHA256(salted, "Client Key")
	authMessage := clientFirstBare + "," + string(serverFirst) + "," + finalNoProof
	clientSig := hmacSHA256(cred.StoredKey, authMessage)
	return base64.StdEncoding.EncodeToString(xorBytes(clientKey, clientSig)), hmacSHA256(cred.ServerKey, authMessage)
}

func serverSignatureFrom(t *testing.T, principal impl.Principal) []byte {
	t.Helper()
	msg := string(principal.ServerFinal)
	if !strings.HasPrefix(msg, "v=") {
		t.Fatalf("server-final %q does not start with v=", msg)
	}
	sig, err := base64.StdEncoding.DecodeString(msg[2:])
	if err != nil {
		t.Fatalf("server signature is not base64: %v", err)
	}
	return sig
}

func TestBeginReturnsServerFirst(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, _ := clientFirstMsg("n,,", scramUsername, "client-nonce-9f3a")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if len(sf) == 0 || stateID == "" {
		t.Fatalf("begin must return a server-first message and a state id")
	}
	parts := parseServerFirst(t, sf)
	if !strings.HasPrefix(parts.nonce, "client-nonce-9f3a") || parts.nonce == "client-nonce-9f3a" {
		t.Fatalf("server nonce %q must extend the client nonce", parts.nonce)
	}
	if parts.iterations != scramIters {
		t.Fatalf("iteration count = %d, want %d", parts.iterations, scramIters)
	}
	if string(parts.salt) != string(cred.Salt) {
		t.Fatalf("salt = %q, want the stored salt", string(parts.salt))
	}
	if got := g.Pending(); got != 1 {
		t.Fatalf("pending handshakes = %d, want 1", got)
	}
}

func TestBeginRejectsMalformedClientFirst(t *testing.T) {
	store := newStore()
	store.put(makeCredential(t, scramUsername, scramPassword, scramIters))
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	bad := [][]byte{
		[]byte("n=alice,r=client-nonce-1234"),
		[]byte("x,,n=alice,r=client-nonce-1234"),
		[]byte("n,,r=client-nonce-1234"),
		[]byte("n,,n=alice"),
		[]byte("n,,n=alice,r=abc"),
		[]byte(""),
	}
	for _, msg := range bad {
		if _, _, err := g.Begin(msg); !errors.Is(err, impl.ErrBadClientFirst) {
			t.Fatalf("client-first %q must be rejected with ErrBadClientFirst, got %v", string(msg), err)
		}
	}
	if got := g.Pending(); got != 0 {
		t.Fatalf("rejected messages must not create states, got %d", got)
	}
}

func TestBeginRejectsUnknownUser(t *testing.T) {
	store := newStore()
	store.put(makeCredential(t, scramUsername, scramPassword, scramIters))
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, _ := clientFirstMsg("n,,", "mallory", "client-nonce-5555")
	if _, _, err := g.Begin(cf); !errors.Is(err, impl.ErrUnknownUser) {
		t.Fatalf("unknown account must be rejected with ErrUnknownUser, got %v", err)
	}
	if got := g.Pending(); got != 0 {
		t.Fatalf("rejected account must not create a state, got %d", got)
	}
}

func TestBeginRejectsIterationsBelowPolicy(t *testing.T) {
	store := newStore()
	store.put(makeCredential(t, scramUsername, scramPassword, 512))
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, _ := clientFirstMsg("n,,", scramUsername, "client-nonce-7777")
	if _, _, err := g.Begin(cf); !errors.Is(err, impl.ErrIterationsTooLow) {
		t.Fatalf("low iteration verifier must be rejected with ErrIterationsTooLow, got %v", err)
	}
	if got := g.Pending(); got != 0 {
		t.Fatalf("rejected verifier must not create a state, got %d", got)
	}
}

func TestInflightLimitAndAbort(t *testing.T) {
	store := newStore()
	store.put(makeCredential(t, scramUsername, scramPassword, scramIters))
	policy := impl.ScramPolicy{MinIterations: 1024, MaxInflight: 2, StateTTL: 60, MaxFailures: 3}
	g := impl.NewScramServerGuard(scramTenant, store, policy)
	cf, _ := clientFirstMsg("n,,", scramUsername, "client-nonce-1111")
	_, firstID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, _, err := g.Begin(cf); err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if _, _, err := g.Begin(cf); !errors.Is(err, impl.ErrInflightLimit) {
		t.Fatalf("third handshake must hit the inflight cap, got %v", err)
	}
	if got := g.Pending(); got != 2 {
		t.Fatalf("pending handshakes = %d, want 2", got)
	}
	if err := g.Abort(firstID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := g.Pending(); got != 1 {
		t.Fatalf("pending after abort = %d, want 1", got)
	}
	if _, _, err := g.Begin(cf); err != nil {
		t.Fatalf("begin after abort must succeed, got %v", err)
	}
	if err := g.Abort(firstID); !errors.Is(err, impl.ErrUnknownState) {
		t.Fatalf("abort of a dropped state must fail with ErrUnknownState, got %v", err)
	}
}

func TestContinueCompletesHandshake(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-9f3a")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	proof, wantSig := scramProof(t, cred, scramPassword, bare, sf, "c="+cbindValue("n,,", nil)+",r="+parts.nonce)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
	principal, err := g.Continue(stateID, final)
	if err != nil {
		t.Fatalf("continue must succeed, got %v", err)
	}
	if principal.Username != scramUsername {
		t.Fatalf("principal username = %q, want %q", principal.Username, scramUsername)
	}
	if principal.TenantID != scramTenant {
		t.Fatalf("principal tenant = %q, want %q", principal.TenantID, scramTenant)
	}
	if principal.AuthzID != "" {
		t.Fatalf("principal authzid = %q, want empty", principal.AuthzID)
	}
	if principal.Iterations != scramIters {
		t.Fatalf("principal iterations = %d, want %d", principal.Iterations, scramIters)
	}
	if got := serverSignatureFrom(t, principal); !hmac.Equal(got, wantSig) {
		t.Fatalf("server signature does not verify against the stored server key")
	}
	if got := g.Pending(); got != 0 {
		t.Fatalf("pending after a finished handshake = %d, want 0", got)
	}
}

func TestContinueRejectsNonceMismatch(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-9f3a")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	otherNonce := parts.nonce + "tampered"
	proof, _ := scramProof(t, cred, scramPassword, bare, sf, "c="+cbindValue("n,,", nil)+",r="+otherNonce)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), otherNonce, proof)
	if _, err := g.Continue(stateID, final); !errors.Is(err, impl.ErrNonceMismatch) {
		t.Fatalf("tampered nonce must be rejected with ErrNonceMismatch, got %v", err)
	}
	if _, err := g.Continue(stateID, final); !errors.Is(err, impl.ErrUnknownState) {
		t.Fatalf("a rejected handshake must be dropped, got %v", err)
	}
}

func TestContinueIsSingleUse(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-9f3a")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	finalNoProof := "c=" + cbindValue("n,,", nil) + ",r=" + parts.nonce
	proof, _ := scramProof(t, cred, scramPassword, bare, sf, finalNoProof)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
	if _, err := g.Continue(stateID, final); err != nil {
		t.Fatalf("first continue: %v", err)
	}
	if _, err := g.Continue(stateID, final); !errors.Is(err, impl.ErrUnknownState) {
		t.Fatalf("replaying a finished handshake must fail with ErrUnknownState, got %v", err)
	}
}

func TestContinueRejectsWrongPassword(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-9f3a")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	finalNoProof := "c=" + cbindValue("n,,", nil) + ",r=" + parts.nonce
	proof, _ := scramProof(t, cred, "not-the-password", bare, sf, finalNoProof)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
	if _, err := g.Continue(stateID, final); !errors.Is(err, impl.ErrAuthenticationFailed) {
		t.Fatalf("wrong password must fail with ErrAuthenticationFailed, got %v", err)
	}
	if got := g.Pending(); got != 0 {
		t.Fatalf("failed handshake must not stay pending, got %d", got)
	}
}

func TestChannelBindingMustMatch(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	gs2 := "p=tls-unique,,"
	connDigest := []byte("connection-cert-digest-A")
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	g.SetChannelBinding(connDigest)

	cf, bare := clientFirstMsg(gs2, scramUsername, "client-nonce-cb01")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin with channel binding: %v", err)
	}
	parts := parseServerFirst(t, sf)
	wrongCbind := cbindValue(gs2, []byte("connection-cert-digest-B"))
	finalNoProof := "c=" + wrongCbind + ",r=" + parts.nonce
	proof, _ := scramProof(t, cred, scramPassword, bare, sf, finalNoProof)
	final, _ := clientFinalMsg(wrongCbind, parts.nonce, proof)
	if _, err := g.Continue(stateID, final); !errors.Is(err, impl.ErrChannelBindingMismatch) {
		t.Fatalf("foreign channel binding must be rejected, got %v", err)
	}

	sf, stateID, err = g.Begin(cf)
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	parts = parseServerFirst(t, sf)
	rightCbind := cbindValue(gs2, connDigest)
	finalNoProof = "c=" + rightCbind + ",r=" + parts.nonce
	proof, wantSig := scramProof(t, cred, scramPassword, bare, sf, finalNoProof)
	final, _ = clientFinalMsg(rightCbind, parts.nonce, proof)
	principal, err := g.Continue(stateID, final)
	if err != nil {
		t.Fatalf("matching channel binding must succeed, got %v", err)
	}
	if got := serverSignatureFrom(t, principal); !hmac.Equal(got, wantSig) {
		t.Fatalf("server signature mismatch on the bound handshake")
	}
}

func TestContinueAcceptsAuthzID(t *testing.T) {
	store := newStore()
	alice := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(alice)
	store.put(makeCredential(t, "bob", "bob-password", scramIters))
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,a=bob,,", scramUsername, "client-nonce-az01")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	cb := cbindValue("n,a=bob,,", nil)
	finalNoProof := "c=" + cb + ",r=" + parts.nonce
	proof, _ := scramProof(t, alice, scramPassword, bare, sf, finalNoProof)
	final, _ := clientFinalMsg(cb, parts.nonce, proof)
	principal, err := g.Continue(stateID, final)
	if err != nil {
		t.Fatalf("continue with an existing authzid: %v", err)
	}
	if principal.Username != scramUsername || principal.AuthzID != "bob" {
		t.Fatalf("principal = %q acting as %q, want alice acting as bob", principal.Username, principal.AuthzID)
	}
}

func TestSweepExpiresStateAfterTTL(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	clock := int64(1_000_000)
	g := impl.NewScramServerGuard(scramTenant, store, impl.ScramPolicy{
		MinIterations: 1024,
		MaxInflight:   4,
		StateTTL:      60,
		MaxFailures:   3,
	})
	g.SetClock(func() int64 { return clock })
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-ttl1")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	clock += 30
	if got := g.Sweep(); got != 0 {
		t.Fatalf("sweep inside the TTL dropped %d states, want 0", got)
	}
	if got := g.Pending(); got != 1 {
		t.Fatalf("pending inside the TTL = %d, want 1", got)
	}
	clock += 31
	if got := g.Sweep(); got != 1 {
		t.Fatalf("sweep past the TTL dropped %d states, want 1", got)
	}
	if got := g.Pending(); got != 0 {
		t.Fatalf("pending after the sweep = %d, want 0", got)
	}
	finalNoProof := "c=" + cbindValue("n,,", nil) + ",r=" + parts.nonce
	proof, _ := scramProof(t, cred, scramPassword, bare, sf, finalNoProof)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
	if _, err := g.Continue(stateID, final); !errors.Is(err, impl.ErrUnknownState) {
		t.Fatalf("expired state must be rejected with ErrUnknownState, got %v", err)
	}
}

func TestPolicyChangeKeepsInflightSnapshot(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-pol1")
	sf, stateID, err := g.Begin(cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	next := impl.ScramPolicy{MinIterations: 8192, MaxInflight: 2, StateTTL: 30, MaxFailures: 1}
	if err := g.SetPolicy(next); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	cf2, _ := clientFirstMsg("n,,", scramUsername, "client-nonce-pol2")
	if _, _, err := g.Begin(cf2); !errors.Is(err, impl.ErrIterationsTooLow) {
		t.Fatalf("a new handshake must obey the raised iteration floor, got %v", err)
	}
	finalNoProof := "c=" + cbindValue("n,,", nil) + ",r=" + parts.nonce
	proof, _ := scramProof(t, cred, scramPassword, bare, sf, finalNoProof)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
	if _, err := g.Continue(stateID, final); err != nil {
		t.Fatalf("the handshake started before the change must still finish, got %v", err)
	}
	if err := g.SetPolicy(impl.ScramPolicy{MinIterations: 0, MaxInflight: 2, StateTTL: 30, MaxFailures: 1}); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("invalid policy must be rejected with ErrBadPolicy, got %v", err)
	}
	if got := g.Policy().MinIterations; got != 8192 {
		t.Fatalf("rejected policy changed the floor to %d, want 8192", got)
	}
}

func TestFailureLockoutByAccountAndSource(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	policy := impl.ScramPolicy{MinIterations: 1024, MaxInflight: 8, StateTTL: 60, MaxFailures: 2}
	g := impl.NewScramServerGuard(scramTenant, store, policy)
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-lock")
	attempt := func(source string) error {
		sf, stateID, err := g.BeginFrom(source, cf)
		if err != nil {
			return err
		}
		parts := parseServerFirst(t, sf)
		finalNoProof := "c=" + cbindValue("n,,", nil) + ",r=" + parts.nonce
		proof, _ := scramProof(t, cred, "bad-password", bare, sf, finalNoProof)
		final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
		_, err = g.Continue(stateID, final)
		return err
	}
	for i := 0; i < 2; i++ {
		if err := attempt("10.0.0.1"); !errors.Is(err, impl.ErrAuthenticationFailed) {
			t.Fatalf("failure %d must be ErrAuthenticationFailed, got %v", i, err)
		}
	}
	if got := g.Failures(scramUsername, "10.0.0.1"); got != 2 {
		t.Fatalf("failure count = %d, want 2", got)
	}
	if _, _, err := g.BeginFrom("10.0.0.1", cf); !errors.Is(err, impl.ErrLocked) {
		t.Fatalf("locked account and source must be refused with ErrLocked, got %v", err)
	}
	if _, _, err := g.BeginFrom("10.0.0.2", cf); err != nil {
		t.Fatalf("another source must not be locked, got %v", err)
	}
	if got := g.Failures(scramUsername, "10.0.0.2"); got != 0 {
		t.Fatalf("fresh source failure count = %d, want 0", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	store := newStore()
	cred := makeCredential(t, scramUsername, scramPassword, scramIters)
	store.put(cred)
	g := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	cf, bare := clientFirstMsg("n,,", scramUsername, "client-nonce-snap")
	sf, stateID, err := g.BeginFrom("10.0.0.9", cf)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parts := parseServerFirst(t, sf)
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewScramServerGuard(scramTenant, store, scramPolicy())
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("restore of a fresh snapshot must succeed, got %v", err)
	}
	if got := restored.Pending(); got != 1 {
		t.Fatalf("restored pending handshakes = %d, want 1", got)
	}
	if err := restored.Restore([]byte("scram-garbage")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must be rejected with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Pending(); got != 1 {
		t.Fatalf("rejected restore must keep the live states, got %d", got)
	}
	finalNoProof := "c=" + cbindValue("n,,", nil) + ",r=" + parts.nonce
	proof, wantSig := scramProof(t, cred, scramPassword, bare, sf, finalNoProof)
	final, _ := clientFinalMsg(cbindValue("n,,", nil), parts.nonce, proof)
	principal, err := restored.Continue(stateID, final)
	if err != nil {
		t.Fatalf("restored handshake must still finish, got %v", err)
	}
	if got := serverSignatureFrom(t, principal); !hmac.Equal(got, wantSig) {
		t.Fatalf("server signature mismatch after restore")
	}
}
