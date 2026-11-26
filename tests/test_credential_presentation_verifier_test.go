package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-12"
)

func issuerRegistry() []impl.IssuerCert {
	return []impl.IssuerCert{
		{
			ID:        "issuer-1",
			NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:  time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			ID:        "issuer-2",
			ParentID:  "issuer-1",
			NotBefore: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:  time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
}

func testOptions(now func() time.Time) impl.Options {
	return impl.Options{
		MaxRequests:  4,
		MaxAttempts:  3,
		RequestTTL:   2 * time.Minute,
		MaxDisclosed: 4,
		Now:          now,
		Issuers:      issuerRegistry(),
	}
}

func openRequest(t *testing.T, v *impl.CredentialPresentationVerifier) impl.RequestID {
	t.Helper()
	id, err := v.Request(impl.PresentationSpec{
		VerifierID: "verifier-1",
		Purpose:    "alcohol_age_check",
		Required:   []string{"birthdate", "nationality"},
		Optional:   []string{"name"},
		Issuers:    []string{"issuer-1"},
	})
	if err != nil {
		t.Fatalf("open request: %v", err)
	}
	return id
}

func presentationFor(t *testing.T, v *impl.CredentialPresentationVerifier, id impl.RequestID) impl.Presentation {
	t.Helper()
	nonce, err := v.Nonce(string(id))
	if err != nil {
		t.Fatalf("read nonce: %v", err)
	}
	return impl.Presentation{
		RequestID:    string(id),
		CredentialID: "cred-1",
		IssuerID:     "issuer-1",
		Subject:      "subject-1",
		Nonce:        string(nonce),
		IssuedAt:     time.Date(2026, 2, 1, 8, 0, 0, 0, time.UTC),
		Disclosed: map[string]string{
			"birthdate":   "2000-01-01",
			"nationality": "CN",
		},
	}
}

func lowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func TestRequestAssignsSequentialIDs(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	first := openRequest(t, v)
	second := openRequest(t, v)
	if first != "req-1" {
		t.Fatalf("first request = %q, want req-1", first)
	}
	if second != "req-2" {
		t.Fatalf("second request = %q, want req-2", second)
	}
	if got := v.Pending(); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}

func TestRequestRejectsBadSpec(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	cases := []impl.PresentationSpec{
		{VerifierID: "  ", Purpose: "check", Required: []string{"a"}, Issuers: []string{"issuer-1"}},
		{VerifierID: "verifier-1", Purpose: " ", Required: []string{"a"}, Issuers: []string{"issuer-1"}},
		{VerifierID: "verifier-1", Purpose: "check", Required: nil, Issuers: []string{"issuer-1"}},
	}
	for i, spec := range cases {
		if _, err := v.Request(spec); !errors.Is(err, impl.ErrBadSpec) {
			t.Fatalf("case %d must return ErrBadSpec, got %v", i, err)
		}
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("rejected specs must not open requests, got %d pending", got)
	}
}

func TestRequestRejectsUnknownAllowedIssuer(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	spec := impl.PresentationSpec{
		VerifierID: "verifier-1",
		Purpose:    "check",
		Required:   []string{"a"},
		Issuers:    []string{"issuer-404"},
	}
	if _, err := v.Request(spec); !errors.Is(err, impl.ErrUnknownIssuer) {
		t.Fatalf("unknown allowed issuer must return ErrUnknownIssuer, got %v", err)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("rejected spec must not open a request, got %d pending", got)
	}
}

func TestRequestEnforcesInflightLimit(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	opts := testOptions(func() time.Time { return now })
	opts.MaxRequests = 2
	v := impl.NewCredentialPresentationVerifier(opts)
	openRequest(t, v)
	openRequest(t, v)
	if _, err := v.Request(impl.PresentationSpec{
		VerifierID: "verifier-1",
		Purpose:    "check",
		Required:   []string{"a"},
		Issuers:    []string{"issuer-1"},
	}); !errors.Is(err, impl.ErrRequestLimit) {
		t.Fatalf("third request must hit ErrRequestLimit, got %v", err)
	}
	if got := v.Pending(); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}

func TestNonceStableAndShaped(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	first := openRequest(t, v)
	second := openRequest(t, v)
	n1, err := v.Nonce(string(first))
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	if len(n1) != 32 || !lowerHex(string(n1)) {
		t.Fatalf("nonce = %q, want 32 lowercase hex characters", string(n1))
	}
	again, err := v.Nonce(string(first))
	if err != nil {
		t.Fatalf("nonce again: %v", err)
	}
	if again != n1 {
		t.Fatalf("nonce of one request must stay stable, got %q then %q", string(n1), string(again))
	}
	n2, err := v.Nonce(string(second))
	if err != nil {
		t.Fatalf("second nonce: %v", err)
	}
	if n2 == n1 {
		t.Fatalf("different requests must get different nonces, both are %q", string(n1))
	}
}

func TestVerifyAcceptsScopedPresentation(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	res, err := v.Verify(string(id), presentationFor(t, v, id))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Decision != impl.DecisionAccepted {
		t.Fatalf("decision = %q, want accepted", res.Decision)
	}
	if res.Reason != impl.ReasonAccepted {
		t.Fatalf("reason = %q, want %q", res.Reason, impl.ReasonAccepted)
	}
	if res.ID != "res-1" {
		t.Fatalf("result id = %q, want res-1", res.ID)
	}
	if res.IssuerID != "issuer-1" {
		t.Fatalf("result issuer = %q, want issuer-1", res.IssuerID)
	}
	if len(res.Disclosed) != 2 || res.Disclosed[0] != "birthdate" || res.Disclosed[1] != "nationality" {
		t.Fatalf("disclosed fields = %v, want [birthdate nationality]", res.Disclosed)
	}
	if !res.At.Equal(now) {
		t.Fatalf("result time = %v, want %v", res.At, now)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("pending after acceptance = %d, want 0", got)
	}
	stored, ok := v.ResultAt(res.ID)
	if !ok || stored.Decision != impl.DecisionAccepted {
		t.Fatalf("stored result = %+v ok=%v, want accepted", stored, ok)
	}
	trace, err := v.Explain(res.ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	want := []string{"request", "nonce", "revocation", "issuer", "scope"}
	if len(trace.Steps) != len(want) {
		t.Fatalf("trace steps = %v, want %v", trace.Steps, want)
	}
	for i := range want {
		if trace.Steps[i] != want[i] {
			t.Fatalf("trace steps = %v, want %v", trace.Steps, want)
		}
	}
}

func TestVerifyRejectsMissingRequiredField(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	p := presentationFor(t, v, id)
	delete(p.Disclosed, "nationality")
	if _, err := v.Verify(string(id), p); !errors.Is(err, impl.ErrScopeMismatch) {
		t.Fatalf("missing required field must return ErrScopeMismatch, got %v", err)
	}
	if got := v.Attempts(string(id)); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if got := v.Pending(); got != 1 {
		t.Fatalf("request must stay open below the attempt cap, pending = %d", got)
	}
}

func TestVerifyRejectsNonceMismatch(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	bad := presentationFor(t, v, id)
	bad.Nonce = "00000000000000000000000000000000"
	if _, err := v.Verify(string(id), bad); !errors.Is(err, impl.ErrNonceMismatch) {
		t.Fatalf("wrong nonce must return ErrNonceMismatch, got %v", err)
	}
	if got := v.Attempts(string(id)); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	res, err := v.Verify(string(id), presentationFor(t, v, id))
	if err != nil {
		t.Fatalf("a corrected presentation below the cap must pass: %v", err)
	}
	if res.Decision != impl.DecisionAccepted {
		t.Fatalf("decision = %q, want accepted", res.Decision)
	}
}

func TestVerifyRejectsIssuerOutsideSpec(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	p := presentationFor(t, v, id)
	p.IssuerID = "issuer-2"
	if _, err := v.Verify(string(id), p); !errors.Is(err, impl.ErrIssuerNotAllowed) {
		t.Fatalf("issuer outside the request must return ErrIssuerNotAllowed, got %v", err)
	}
	if got := v.Attempts(string(id)); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestVerifyRevokedCredentialRejected(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	if err := v.Revoke("cred-1", "key_compromise"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !v.Revoked("cred-1") {
		t.Fatalf("credential must read as revoked")
	}
	res, err := v.Verify(string(id), presentationFor(t, v, id))
	if err != nil {
		t.Fatalf("a revoked credential must be a result, not an error: %v", err)
	}
	if res.Decision != impl.DecisionRejected || res.Reason != impl.ReasonRevoked {
		t.Fatalf("result = %+v, want rejected with reason_revoked", res)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("pending after rejection = %d, want 0", got)
	}
	trace, err := v.Explain(res.ID)
	if err != nil {
		t.Fatalf("explain revoked result: %v", err)
	}
	if trace.Reason != impl.ReasonRevoked {
		t.Fatalf("trace reason = %q, want %q", trace.Reason, impl.ReasonRevoked)
	}
}

func TestVerifyClosesAfterAttemptLimit(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	opts := testOptions(func() time.Time { return now })
	opts.MaxAttempts = 2
	v := impl.NewCredentialPresentationVerifier(opts)
	id := openRequest(t, v)
	for i := 0; i < 2; i++ {
		bad := presentationFor(t, v, id)
		bad.Nonce = "ffffffffffffffffffffffffffffffff"
		if _, err := v.Verify(string(id), bad); !errors.Is(err, impl.ErrNonceMismatch) {
			t.Fatalf("attempt %d must return ErrNonceMismatch, got %v", i, err)
		}
	}
	if got := v.Attempts(string(id)); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if _, err := v.Verify(string(id), presentationFor(t, v, id)); !errors.Is(err, impl.ErrTooManyAttempts) {
		t.Fatalf("exhausted request must return ErrTooManyAttempts, got %v", err)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("exhausted request must be closed, pending = %d", got)
	}
}

func TestVerifyRejectsExpiredRequest(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	p := presentationFor(t, v, id)
	now = now.Add(2 * time.Minute)
	if _, err := v.Verify(string(id), p); !errors.Is(err, impl.ErrRequestExpired) {
		t.Fatalf("expired request must return ErrRequestExpired, got %v", err)
	}
	if got := v.Pending(); got != 0 {
		t.Fatalf("expired request must leave the pending set, got %d", got)
	}
	if _, err := v.Nonce(string(id)); !errors.Is(err, impl.ErrRequestExpired) {
		t.Fatalf("expired request must not hand out a nonce, got %v", err)
	}
}

func TestVerifyRejectsReplayAfterAccept(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	id := openRequest(t, v)
	p := presentationFor(t, v, id)
	if _, err := v.Verify(string(id), p); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(string(id), p); !errors.Is(err, impl.ErrRequestClosed) {
		t.Fatalf("replay must return ErrRequestClosed, got %v", err)
	}
}

func TestRevokeValidation(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	v := impl.NewCredentialPresentationVerifier(testOptions(func() time.Time { return now }))
	if err := v.Revoke("cred-404", "lost"); !errors.Is(err, impl.ErrUnknownCredential) {
		t.Fatalf("unknown credential must return ErrUnknownCredential, got %v", err)
	}
	if err := v.Revoke("cred-1", "   "); !errors.Is(err, impl.ErrBadReason) {
		t.Fatalf("blank reason must return ErrBadReason, got %v", err)
	}
	if v.Revoked("cred-1") {
		t.Fatalf("rejected revocation must not mark the credential")
	}
	if err := v.Revoke("cred-1", "key_compromise"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := v.Revoke("cred-1", "again"); !errors.Is(err, impl.ErrAlreadyRevoked) {
		t.Fatalf("second revoke must return ErrAlreadyRevoked, got %v", err)
	}
	if _, err := v.Explain("res-404"); !errors.Is(err, impl.ErrResultNotFound) {
		t.Fatalf("unknown result must return ErrResultNotFound, got %v", err)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	v := impl.NewCredentialPresentationVerifier(testOptions(clock))
	acceptedID := openRequest(t, v)
	res, err := v.Verify(string(acceptedID), presentationFor(t, v, acceptedID))
	if err != nil {
		t.Fatalf("accept first request: %v", err)
	}
	openID := openRequest(t, v)
	bad := presentationFor(t, v, openID)
	bad.Nonce = "11111111111111111111111111111111"
	if _, err := v.Verify(string(openID), bad); !errors.Is(err, impl.ErrNonceMismatch) {
		t.Fatalf("failed attempt before snapshot: %v", err)
	}
	if err := v.Revoke("cred-2", "device_lost"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	storedNonce, err := v.Nonce(string(openID))
	if err != nil {
		t.Fatalf("nonce before snapshot: %v", err)
	}
	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewCredentialPresentationVerifier(testOptions(clock))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, ok := restored.ResultAt(res.ID)
	if !ok || got.Decision != impl.DecisionAccepted {
		t.Fatalf("restored result = %+v ok=%v, want accepted", got, ok)
	}
	if !restored.Revoked("cred-2") {
		t.Fatalf("revocation must survive the snapshot")
	}
	if got := restored.Attempts(string(openID)); got != 1 {
		t.Fatalf("restored attempts = %d, want 1", got)
	}
	nonce, err := restored.Nonce(string(openID))
	if err != nil {
		t.Fatalf("restored nonce: %v", err)
	}
	if nonce != storedNonce {
		t.Fatalf("restored nonce changed: %q vs %q", string(nonce), string(storedNonce))
	}
	again, err := restored.Verify(string(openID), presentationFor(t, restored, openID))
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if again.Decision != impl.DecisionAccepted {
		t.Fatalf("restored request decision = %q, want accepted", again.Decision)
	}
	next, err := restored.Request(impl.PresentationSpec{
		VerifierID: "verifier-1",
		Purpose:    "check",
		Required:   []string{"a"},
		Issuers:    []string{"issuer-1"},
	})
	if err != nil {
		t.Fatalf("request after restore: %v", err)
	}
	if next != "req-3" {
		t.Fatalf("request id after restore = %q, want req-3", next)
	}
	broken := impl.NewCredentialPresentationVerifier(testOptions(clock))
	if err := broken.Restore([]byte("{not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if got := broken.Pending(); got != 0 {
		t.Fatalf("failed restore must not apply partial state, got %d pending", got)
	}
}
