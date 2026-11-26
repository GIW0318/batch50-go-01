package task

import (
	"errors"
	"fmt"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-38"
)

const (
	digestGood  = "3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b"
	digestOther = "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4"
)

func newVerifier() *impl.TimestampAuthorityVerifier {
	return impl.NewTimestampAuthorityVerifier(impl.Limits{
		MaxBatch:  8,
		CacheSize: 32,
		Now:       time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	})
}

func at(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func defaultChain() []impl.Cert {
	return []impl.Cert{
		{
			Subject: "cn=tsa-a", Issuer: "cn=root", Serial: "10",
			NotBefore: at(2024, time.January, 1), NotAfter: at(2025, time.January, 1),
			EKUs: []string{"timestamping"},
		},
		{
			Subject: "cn=root", Issuer: "cn=root", Serial: "1",
			NotBefore: at(2020, time.January, 1), NotAfter: at(2030, time.January, 1),
			EKUs: []string{"cert-sign"},
		},
	}
}

func makeToken(tsa, serial, genTime, digest, nonce string) []byte {
	return []byte(fmt.Sprintf(
		`{"tsa":%q,"serial":%q,"genTime":%q,"digest":%q,"nonce":%q}`,
		tsa, serial, genTime, digest, nonce))
}

func signedToken() []byte {
	return makeToken("cn=tsa-a", "10", "2024-06-01T10:00:00Z", digestGood, "n-1")
}

func goodImprint() impl.Imprint {
	return impl.Imprint{Algorithm: "sha256", Digest: digestGood, Nonce: "n-1"}
}

func mustTrust(t *testing.T, v *impl.TimestampAuthorityVerifier) {
	t.Helper()
	if err := v.Trust("cn=tsa-a", defaultChain()); err != nil {
		t.Fatalf("trust: %v", err)
	}
}

func mustVerify(t *testing.T, v *impl.TimestampAuthorityVerifier, token []byte, imprint impl.Imprint) impl.Verdict {
	t.Helper()
	vd, err := v.Verify(token, imprint)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return vd
}

func TestVerifyRejectsMalformedToken(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	docs := [][]byte{
		[]byte("not-a-token"),
		makeToken("", "10", "2024-06-01T10:00:00Z", digestGood, "n-1"),
		makeToken("cn=tsa-a", "", "2024-06-01T10:00:00Z", digestGood, "n-1"),
		makeToken("cn=tsa-a", "10", "2024-06-01T10:00:00Z", "", "n-1"),
	}
	for _, doc := range docs {
		if _, err := v.Verify(doc, goodImprint()); !errors.Is(err, impl.ErrBadToken) {
			t.Fatalf("token %s must fail with ErrBadToken, got %v", doc, err)
		}
	}
	if stats := v.Stats(); stats.Verified != 0 || stats.Rejected != 0 {
		t.Fatalf("a malformed token must not be counted, got %+v", stats)
	}
}

func TestVerifyRejectsMissingNonce(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	doc := makeToken("cn=tsa-a", "10", "2024-06-01T10:00:00Z", digestGood, "")
	if _, err := v.Verify(doc, goodImprint()); !errors.Is(err, impl.ErrNonceMissing) {
		t.Fatalf("a token without a nonce must fail with ErrNonceMissing, got %v", err)
	}
}

func TestVerifyRejectsNonceMismatchAndBadImprint(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	doc := makeToken("cn=tsa-a", "10", "2024-06-01T10:00:00Z", digestGood, "n-2")
	if _, err := v.Verify(doc, goodImprint()); !errors.Is(err, impl.ErrNonceMismatch) {
		t.Fatalf("a different nonce must fail with ErrNonceMismatch, got %v", err)
	}
	token := signedToken()
	badAlg := impl.Imprint{Algorithm: "md5", Digest: digestGood, Nonce: "n-1"}
	if _, err := v.Verify(token, badAlg); !errors.Is(err, impl.ErrImprintMismatch) {
		t.Fatalf("an unsupported digest algorithm must fail with ErrImprintMismatch, got %v", err)
	}
	badHex := impl.Imprint{Algorithm: "sha256", Digest: "zz", Nonce: "n-1"}
	if _, err := v.Verify(token, badHex); !errors.Is(err, impl.ErrImprintMismatch) {
		t.Fatalf("a non hexadecimal digest must fail with ErrImprintMismatch, got %v", err)
	}
}

func TestVerifyMarksImprintMismatch(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	doc := makeToken("cn=tsa-a", "10", "2024-06-01T10:00:00Z", digestOther, "n-1")
	vd := mustVerify(t, v, doc, goodImprint())
	if vd.Valid {
		t.Fatalf("a token over another digest must be invalid")
	}
	if vd.Reason != impl.ReasonImprint {
		t.Fatalf("reason = %q, want %q", vd.Reason, impl.ReasonImprint)
	}
	if len(vd.TokenID) != 64 {
		t.Fatalf("token id = %q, want 64 hex characters", vd.TokenID)
	}
	if stats := v.Stats(); stats.Rejected != 1 || stats.Verified != 0 {
		t.Fatalf("stats = %+v, want one rejected verdict", stats)
	}
}

func TestVerifyAcceptsValidToken(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	vd := mustVerify(t, v, signedToken(), goodImprint())
	if !vd.Valid || vd.Reason != impl.ReasonOK {
		t.Fatalf("verdict = %v/%q, want a valid verdict", vd.Valid, vd.Reason)
	}
	if vd.TSA != "cn=tsa-a" {
		t.Fatalf("tsa = %q, want cn=tsa-a", vd.TSA)
	}
	if !vd.GenTime.Equal(at(2024, time.June, 1).Add(10 * time.Hour)) {
		t.Fatalf("gen time = %s, want the time carried by the token", vd.GenTime)
	}
	if vd.Cached {
		t.Fatalf("the first verification must not be reported as cached")
	}
	if len(vd.TokenID) != 64 {
		t.Fatalf("token id = %q, want 64 hex characters", vd.TokenID)
	}
	stored, ok := v.VerdictFor(vd.TokenID)
	if !ok || !stored.Valid {
		t.Fatalf("stored verdict = %+v, want the valid verdict", stored)
	}
	stats := v.Stats()
	if stats.Verified != 1 || stats.Trusted != 1 {
		t.Fatalf("stats = %+v, want one verified and one trust anchor", stats)
	}
}

func TestVerifyUsesSigningTimeForCertWindow(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	vd := mustVerify(t, v, signedToken(), goodImprint())
	if !vd.Valid {
		t.Fatalf("a token signed inside the certificate window must stay valid after the certificate expires, got %q", vd.Reason)
	}
}

func TestVerifyRejectsTokenSignedBeforeCert(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	doc := makeToken("cn=tsa-a", "10", "2023-06-01T10:00:00Z", digestGood, "n-1")
	vd := mustVerify(t, v, doc, goodImprint())
	if vd.Valid || vd.Reason != impl.ReasonCertWindow {
		t.Fatalf("verdict = %v/%q, want an invalid verdict with %q", vd.Valid, vd.Reason, impl.ReasonCertWindow)
	}
}

func TestVerifyRejectsRevokedCertificate(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	if err := v.Revoke("10", at(2024, time.January, 1)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	vd := mustVerify(t, v, signedToken(), goodImprint())
	if vd.Valid || vd.Reason != impl.ReasonRevoked {
		t.Fatalf("verdict = %v/%q, want a revoked verdict", vd.Valid, vd.Reason)
	}

	later := newVerifier()
	mustTrust(t, later)
	if err := later.Revoke("10", at(2025, time.January, 1)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if vd := mustVerify(t, later, signedToken(), goodImprint()); !vd.Valid {
		t.Fatalf("a revocation after the signing time must not invalidate the token, got %q", vd.Reason)
	}
	if err := later.Revoke("", at(2024, time.January, 1)); !errors.Is(err, impl.ErrBadRevocation) {
		t.Fatalf("a blank serial must fail with ErrBadRevocation, got %v", err)
	}
}

func TestVerifyRejectsUntrustedTSA(t *testing.T) {
	v := newVerifier()
	vd := mustVerify(t, v, signedToken(), goodImprint())
	if vd.Valid || vd.Reason != impl.ReasonUntrusted {
		t.Fatalf("verdict = %v/%q, want an untrusted verdict", vd.Valid, vd.Reason)
	}
	if err := v.Trust("", defaultChain()); !errors.Is(err, impl.ErrBadTrust) {
		t.Fatalf("a blank authority name must fail with ErrBadTrust, got %v", err)
	}
	if err := v.Trust("cn=tsa-a", nil); !errors.Is(err, impl.ErrBadTrust) {
		t.Fatalf("an empty chain must fail with ErrBadTrust, got %v", err)
	}
	broken := defaultChain()
	broken[0].Issuer = "cn=elsewhere"
	if err := v.Trust("cn=tsa-a", broken); !errors.Is(err, impl.ErrBadTrust) {
		t.Fatalf("a chain that does not link must fail with ErrBadTrust, got %v", err)
	}
	if err := v.Trust("cn=other", defaultChain()); !errors.Is(err, impl.ErrBadTrust) {
		t.Fatalf("a leaf whose subject differs from the authority must fail with ErrBadTrust, got %v", err)
	}
	if got := v.Trusted(); len(got) != 0 {
		t.Fatalf("a rejected trust anchor must not be registered, got %v", got)
	}
}

func TestTrustedListsAnchorsSorted(t *testing.T) {
	v := newVerifier()
	second := defaultChain()
	second[0].Subject = "cn=tsa-b"
	if err := v.Trust("cn=tsa-b", second); err != nil {
		t.Fatalf("trust b: %v", err)
	}
	mustTrust(t, v)
	got := v.Trusted()
	if len(got) != 2 || got[0] != "cn=tsa-a" || got[1] != "cn=tsa-b" {
		t.Fatalf("trusted = %v, want [cn=tsa-a cn=tsa-b]", got)
	}
	if stats := v.Stats(); stats.Trusted != 2 {
		t.Fatalf("stats trusted = %d, want 2", stats.Trusted)
	}
}

func TestBatchIsolatesFailures(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	second := makeToken("cn=tsa-a", "11", "2024-07-01T10:00:00Z", digestGood, "n-1")
	tokens := [][]byte{signedToken(), []byte("garbage"), second}
	verdicts, err := v.Batch(tokens, goodImprint())
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(verdicts) != 3 {
		t.Fatalf("verdicts = %d, want 3", len(verdicts))
	}
	if !verdicts[0].Valid || verdicts[0].Reason != impl.ReasonOK {
		t.Fatalf("first verdict = %v/%q, want valid", verdicts[0].Valid, verdicts[0].Reason)
	}
	if verdicts[1].Valid || verdicts[1].Reason != impl.ReasonMalformed {
		t.Fatalf("second verdict = %v/%q, want malformed", verdicts[1].Valid, verdicts[1].Reason)
	}
	if !verdicts[2].Valid {
		t.Fatalf("the failure of one token must not stop the next one, got %q", verdicts[2].Reason)
	}
	stats := v.Stats()
	if stats.Verified != 2 || stats.Rejected != 1 {
		t.Fatalf("stats = %+v, want two verified and one rejected", stats)
	}
}

func TestBatchEnforcesLimit(t *testing.T) {
	v := impl.NewTimestampAuthorityVerifier(impl.Limits{MaxBatch: 2, CacheSize: 8, Now: at(2026, time.June, 1)})
	mustTrust(t, v)
	tokens := [][]byte{signedToken(), signedToken(), signedToken()}
	if _, err := v.Batch(tokens, goodImprint()); !errors.Is(err, impl.ErrTooManyTokens) {
		t.Fatalf("an oversized batch must fail with ErrTooManyTokens, got %v", err)
	}
}

func TestRepeatedVerifyHitsCache(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	first := mustVerify(t, v, signedToken(), goodImprint())
	if first.Cached {
		t.Fatalf("the first verification must miss the cache")
	}
	second := mustVerify(t, v, signedToken(), goodImprint())
	if !second.Cached {
		t.Fatalf("a repeated verification must hit the cache")
	}
	if second.Reason != first.Reason || second.TokenID != first.TokenID {
		t.Fatalf("cached verdict = %+v, want the first verdict", second)
	}
	stats := v.Stats()
	if stats.Verified != 1 || stats.Cached != 1 {
		t.Fatalf("stats = %+v, want one verified and one cached", stats)
	}
}

func TestStatsCountsMixedVerdicts(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	mustVerify(t, v, signedToken(), goodImprint())
	mustVerify(t, v, makeToken("cn=tsa-a", "10", "2024-06-01T10:00:00Z", digestOther, "n-1"), goodImprint())
	mustVerify(t, v, signedToken(), goodImprint())
	stats := v.Stats()
	if stats.Verified != 1 || stats.Rejected != 1 || stats.Cached != 1 || stats.Trusted != 1 {
		t.Fatalf("stats = %+v, want 1/1/1/1", stats)
	}
}

func TestSnapshotRestoreKeepsTrustAndCache(t *testing.T) {
	v := newVerifier()
	mustTrust(t, v)
	if err := v.Revoke("10", at(2025, time.January, 1)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	vd := mustVerify(t, v, signedToken(), goodImprint())
	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newVerifier()
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Trusted(); len(got) != 1 || got[0] != "cn=tsa-a" {
		t.Fatalf("restored anchors = %v, want [cn=tsa-a]", got)
	}
	stored, ok := restored.VerdictFor(vd.TokenID)
	if !ok || !stored.Valid {
		t.Fatalf("restored verdict = %+v, want the valid verdict", stored)
	}
	again := mustVerify(t, restored, signedToken(), goodImprint())
	if !again.Cached {
		t.Fatalf("a restored verdict must still be served from the cache")
	}
	if err := restored.Restore([]byte("###broken###")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Trusted(); len(got) != 1 {
		t.Fatalf("a rejected restore must not touch the trust store, got %v", got)
	}
}
