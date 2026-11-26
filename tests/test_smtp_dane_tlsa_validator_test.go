package task

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-16"
)

func spkiDigest(spki []byte) []byte {
	sum := sha256.Sum256(spki)
	return sum[:]
}

func daneLeaf(now time.Time) impl.Cert {
	return impl.Cert{
		Subject:  "mx1.acme.example",
		DER:      []byte("leaf-certificate-der"),
		SPKI:     []byte("leaf-subject-public-key-info"),
		NotAfter: now.Add(90 * 24 * time.Hour),
	}
}

func danePolicyValidator(now time.Time, mx []impl.MxHost, answers map[string]impl.TlsaAnswer) *impl.SmtpDaneTlsaValidator {
	cfg := impl.DaneConfig{
		MaxMxPerDomain: 4,
		MaxTlsaRecords: 8,
		MaxAttempts:    3,
		PolicyTTL:      10 * time.Minute,
		LookupMx: func(domain string) ([]impl.MxHost, error) {
			return mx, nil
		},
		LookupTlsa: func(host string) (impl.TlsaAnswer, error) {
			return answers[host], nil
		},
		Now: func() time.Time { return now },
	}
	return impl.NewSmtpDaneTlsaValidator(cfg)
}

func daneTwoMx() []impl.MxHost {
	return []impl.MxHost{
		{Host: "mx2.acme.example", Priority: 20},
		{Host: "mx1.acme.example", Priority: 10},
	}
}

func TestPolicyOrdersMxByPreference(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	mx := []impl.MxHost{
		{Host: "mx2.acme.example", Priority: 20},
		{Host: "mx1.acme.example", Priority: 10},
		{Host: "mx3.acme.example", Priority: 10},
		{Host: "MX3.ACME.EXAMPLE", Priority: 15},
	}
	v := danePolicyValidator(now, mx, nil)
	p, err := v.Policy("acme.example")
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if p.Domain != "acme.example" {
		t.Fatalf("domain = %q, want acme.example", p.Domain)
	}
	if len(p.Mx) != 3 {
		t.Fatalf("mx count = %d, want 3 after dedupe: %+v", len(p.Mx), p.Mx)
	}
	want := []string{"mx1.acme.example", "mx3.acme.example", "mx2.acme.example"}
	for i, host := range want {
		if p.Mx[i].Host != host {
			t.Fatalf("mx[%d] = %q, want %q", i, p.Mx[i].Host, host)
		}
	}
	if !p.Expires.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("expires = %v, want %v", p.Expires, now.Add(10*time.Minute))
	}
	stored, ok := v.Cached("acme.example")
	if !ok || len(stored.Mx) != 3 {
		t.Fatalf("cached policy missing after resolve: %+v ok=%v", stored, ok)
	}
}

func TestPolicyRejectsBlankDomain(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	lookups := 0
	cfg := impl.DaneConfig{
		MaxMxPerDomain: 4,
		MaxTlsaRecords: 8,
		MaxAttempts:    3,
		PolicyTTL:      time.Minute,
		LookupMx: func(domain string) ([]impl.MxHost, error) {
			lookups++
			return daneTwoMx(), nil
		},
		LookupTlsa: func(host string) (impl.TlsaAnswer, error) { return impl.TlsaAnswer{}, nil },
		Now:        func() time.Time { return now },
	}
	v := impl.NewSmtpDaneTlsaValidator(cfg)
	if _, err := v.Policy("   "); !errors.Is(err, impl.ErrBlankDomain) {
		t.Fatalf("blank domain: err = %v, want ErrBlankDomain", err)
	}
	if _, err := v.Policy(""); !errors.Is(err, impl.ErrBlankDomain) {
		t.Fatalf("empty domain: err = %v, want ErrBlankDomain", err)
	}
	if lookups != 0 {
		t.Fatalf("lookup ran %d times for a blank domain", lookups)
	}
}

func TestPolicyEnforcesMxCap(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	mx := []impl.MxHost{
		{Host: "mx1.acme.example", Priority: 10},
		{Host: "mx2.acme.example", Priority: 20},
		{Host: "mx3.acme.example", Priority: 30},
	}
	cfg := impl.DaneConfig{
		MaxMxPerDomain: 2,
		MaxTlsaRecords: 8,
		MaxAttempts:    3,
		PolicyTTL:      time.Minute,
		LookupMx: func(domain string) ([]impl.MxHost, error) {
			return mx, nil
		},
		LookupTlsa: func(host string) (impl.TlsaAnswer, error) { return impl.TlsaAnswer{}, nil },
		Now:        func() time.Time { return now },
	}
	v := impl.NewSmtpDaneTlsaValidator(cfg)
	if _, err := v.Policy("acme.example"); !errors.Is(err, impl.ErrMxLimit) {
		t.Fatalf("over cap: err = %v, want ErrMxLimit", err)
	}
	if _, ok := v.Cached("acme.example"); ok {
		t.Fatalf("rejected policy must not be cached")
	}
}

func TestPolicyRejectsMissingMxAndLookupFailure(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	empty := danePolicyValidator(now, nil, nil)
	if _, err := empty.Policy("acme.example"); !errors.Is(err, impl.ErrNoMx) {
		t.Fatalf("empty answer: err = %v, want ErrNoMx", err)
	}
	broken := impl.NewSmtpDaneTlsaValidator(impl.DaneConfig{
		MaxMxPerDomain: 4,
		MaxTlsaRecords: 8,
		MaxAttempts:    3,
		PolicyTTL:      time.Minute,
		LookupMx: func(domain string) ([]impl.MxHost, error) {
			return nil, errors.New("resolver timeout")
		},
		LookupTlsa: func(host string) (impl.TlsaAnswer, error) { return impl.TlsaAnswer{}, nil },
		Now:        func() time.Time { return now },
	})
	if _, err := broken.Policy("acme.example"); !errors.Is(err, impl.ErrLookup) {
		t.Fatalf("lookup failure: err = %v, want ErrLookup", err)
	}
	if _, ok := broken.Cached("acme.example"); ok {
		t.Fatalf("failed lookup must not leave a cached policy")
	}
}

func TestPolicyCachesUntilTtlExpires(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	lookups := 0
	cfg := impl.DaneConfig{
		MaxMxPerDomain: 4,
		MaxTlsaRecords: 8,
		MaxAttempts:    3,
		PolicyTTL:      10 * time.Minute,
		LookupMx: func(domain string) ([]impl.MxHost, error) {
			lookups++
			return daneTwoMx(), nil
		},
		LookupTlsa: func(host string) (impl.TlsaAnswer, error) { return impl.TlsaAnswer{}, nil },
		Now:        func() time.Time { return now },
	}
	v := impl.NewSmtpDaneTlsaValidator(cfg)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("first policy: %v", err)
	}
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("second policy: %v", err)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1 while the policy is fresh", lookups)
	}
	now = now.Add(11 * time.Minute)
	if _, ok := v.Cached("acme.example"); ok {
		t.Fatalf("policy must expire after its ttl")
	}
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy after expiry: %v", err)
	}
	if lookups != 2 {
		t.Fatalf("lookups = %d, want 2 after expiry", lookups)
	}
}

func TestRefreshReplacesPolicyAndKeepsOldOnFailure(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	answers := [][]impl.MxHost{
		daneTwoMx(),
		{{Host: "mx9.acme.example", Priority: 5}},
	}
	calls := 0
	failing := false
	cfg := impl.DaneConfig{
		MaxMxPerDomain: 4,
		MaxTlsaRecords: 8,
		MaxAttempts:    3,
		PolicyTTL:      10 * time.Minute,
		LookupMx: func(domain string) ([]impl.MxHost, error) {
			if failing {
				return nil, errors.New("resolver refused")
			}
			out := answers[calls]
			calls++
			return out, nil
		},
		LookupTlsa: func(host string) (impl.TlsaAnswer, error) { return impl.TlsaAnswer{}, nil },
		Now:        func() time.Time { return now },
	}
	v := impl.NewSmtpDaneTlsaValidator(cfg)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if err := v.Refresh("acme.example"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	order, err := v.MXOrder("acme.example")
	if err != nil {
		t.Fatalf("mx order: %v", err)
	}
	if len(order) != 1 || order[0].Host != "mx9.acme.example" {
		t.Fatalf("order after refresh = %+v, want the new single mx", order)
	}
	failing = true
	if err := v.Refresh("acme.example"); !errors.Is(err, impl.ErrLookup) {
		t.Fatalf("failed refresh: err = %v, want ErrLookup", err)
	}
	order, err = v.MXOrder("acme.example")
	if err != nil {
		t.Fatalf("mx order after failed refresh: %v", err)
	}
	if len(order) != 1 || order[0].Host != "mx9.acme.example" {
		t.Fatalf("failed refresh wiped the cached policy: %+v", order)
	}
}

func TestVerifyAcceptsMatchingSpkiSha256(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	leaf := daneLeaf(now)
	records := []impl.TlsaRecord{{
		Usage:        impl.UsageDaneEE,
		Selector:     impl.SelectorSPKI,
		MatchingType: impl.MatchSHA256,
		Data:         spkiDigest(leaf.SPKI),
	}}
	v := danePolicyValidator(now, daneTwoMx(), map[string]impl.TlsaAnswer{
		"mx1.acme.example": {AD: true, TTL: time.Minute, Tlsa: records},
	})
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	verdict, err := v.Verify("mx1.acme.example", []impl.Cert{leaf})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verdict.Secure || verdict.Mode != impl.ModeDane {
		t.Fatalf("verdict = %+v, want a secure dane verdict", verdict)
	}
	if verdict.MX != "mx1.acme.example" {
		t.Fatalf("verdict mx = %q, want mx1.acme.example", verdict.MX)
	}
	if verdict.Records != 1 || verdict.Matched != 1 {
		t.Fatalf("records=%d matched=%d, want 1 and 1", verdict.Records, verdict.Matched)
	}
	if verdict.Cause != "" {
		t.Fatalf("secure verdict carries cause %q", verdict.Cause)
	}
}

func TestVerifyRejectsMismatchWhenAdPresent(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	leaf := daneLeaf(now)
	records := []impl.TlsaRecord{{
		Usage:        impl.UsageDaneEE,
		Selector:     impl.SelectorSPKI,
		MatchingType: impl.MatchSHA256,
		Data:         spkiDigest([]byte("some other mx public key")),
	}}
	v := danePolicyValidator(now, daneTwoMx(), map[string]impl.TlsaAnswer{
		"mx1.acme.example": {AD: true, TTL: time.Minute, Tlsa: records},
	})
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	verdict, err := v.Verify("mx1.acme.example", []impl.Cert{leaf})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verdict.Secure {
		t.Fatalf("mismatching chain reported secure: %+v", verdict)
	}
	if verdict.Mode != impl.ModeReject || verdict.Cause != impl.CauseTlsaMismatch {
		t.Fatalf("verdict = %+v, want reject/%s", verdict, impl.CauseTlsaMismatch)
	}
}

func TestVerifyRejectsUnusableRecordSet(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	leaf := daneLeaf(now)
	records := []impl.TlsaRecord{
		{
			Usage:        impl.UsageDaneEE,
			Selector:     impl.SelectorSPKI,
			MatchingType: impl.MatchSHA256,
			Data:         spkiDigest(leaf.SPKI),
		},
		{
			Usage:        impl.UsageDaneEE,
			Selector:     9,
			MatchingType: impl.MatchSHA256,
			Data:         spkiDigest(leaf.SPKI),
		},
	}
	v := danePolicyValidator(now, daneTwoMx(), map[string]impl.TlsaAnswer{
		"mx1.acme.example": {AD: true, TTL: time.Minute, Tlsa: records},
	})
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	verdict, err := v.Verify("mx1.acme.example", []impl.Cert{leaf})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verdict.Secure || verdict.Mode != impl.ModeReject {
		t.Fatalf("record set with an out of range selector must fail as a whole: %+v", verdict)
	}
	if verdict.Cause != impl.CauseTlsaInvalid {
		t.Fatalf("cause = %q, want %q", verdict.Cause, impl.CauseTlsaInvalid)
	}
}

func TestVerifyRejectsExpiredCertificate(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	leaf := daneLeaf(now)
	leaf.NotAfter = now.Add(-time.Hour)
	records := []impl.TlsaRecord{{
		Usage:        impl.UsageDaneEE,
		Selector:     impl.SelectorSPKI,
		MatchingType: impl.MatchSHA256,
		Data:         spkiDigest(leaf.SPKI),
	}}
	v := danePolicyValidator(now, daneTwoMx(), map[string]impl.TlsaAnswer{
		"mx1.acme.example": {AD: true, TTL: time.Minute, Tlsa: records},
	})
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	verdict, err := v.Verify("mx1.acme.example", []impl.Cert{leaf})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verdict.Secure || verdict.Mode != impl.ModeReject || verdict.Cause != impl.CauseCertExpired {
		t.Fatalf("expired leaf: %+v, want reject/%s", verdict, impl.CauseCertExpired)
	}
}

func TestVerifyRejectsUnknownMxAndEmptyChain(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	leaf := daneLeaf(now)
	v := danePolicyValidator(now, daneTwoMx(), nil)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := v.Verify("mx7.other.example", []impl.Cert{leaf}); !errors.Is(err, impl.ErrUnknownMx) {
		t.Fatalf("unknown mx: err = %v, want ErrUnknownMx", err)
	}
	if _, err := v.Verify("mx1.acme.example", nil); !errors.Is(err, impl.ErrEmptyChain) {
		t.Fatalf("empty chain: err = %v, want ErrEmptyChain", err)
	}
}

func TestFallbackMovesToNextMx(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	v := danePolicyValidator(now, daneTwoMx(), nil)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	verdict, err := v.Fallback("mx1.acme.example", "connect_timeout")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if verdict.Mode != impl.ModeFallback {
		t.Fatalf("mode = %q, want %q", verdict.Mode, impl.ModeFallback)
	}
	if verdict.Next != "mx2.acme.example" {
		t.Fatalf("next = %q, want mx2.acme.example", verdict.Next)
	}
	if verdict.Secure {
		t.Fatalf("fallback verdict must not claim a secure channel")
	}
}

func TestFallbackRejectsWhenMxListExhausted(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	v := danePolicyValidator(now, daneTwoMx(), nil)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	verdict, err := v.Fallback("mx2.acme.example", "tls_mismatch")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if verdict.Mode != impl.ModeReject || verdict.Cause != impl.CauseMxExhausted {
		t.Fatalf("verdict = %+v, want reject/%s", verdict, impl.CauseMxExhausted)
	}
	if verdict.Next != "" {
		t.Fatalf("exhausted mx list offers next hop %q", verdict.Next)
	}
	if _, err := v.Fallback("mx1.acme.example", "   "); !errors.Is(err, impl.ErrBadCause) {
		t.Fatalf("blank cause: err = %v, want ErrBadCause", err)
	}
}

func TestSnapshotRestoreKeepsPolicyAndCounters(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	leaf := daneLeaf(now)
	records := []impl.TlsaRecord{{
		Usage:        impl.UsageDaneEE,
		Selector:     impl.SelectorSPKI,
		MatchingType: impl.MatchSHA256,
		Data:         spkiDigest(leaf.SPKI),
	}}
	answers := map[string]impl.TlsaAnswer{
		"mx1.acme.example": {AD: true, TTL: time.Minute, Tlsa: records},
	}
	v := danePolicyValidator(now, daneTwoMx(), answers)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := v.Verify("mx1.acme.example", []impl.Cert{leaf}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := danePolicyValidator(now, daneTwoMx(), answers)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, ok := restored.Cached("acme.example"); !ok {
		t.Fatalf("restored validator lost the cached policy")
	}
	order, err := restored.MXOrder("acme.example")
	if err != nil {
		t.Fatalf("mx order: %v", err)
	}
	if len(order) != 2 || order[0].Host != "mx1.acme.example" {
		t.Fatalf("restored order = %+v, want mx1 then mx2", order)
	}
	if got := restored.Stats().Policies; got != 1 {
		t.Fatalf("restored policy counter = %d, want 1", got)
	}
	verdict, err := restored.Verify("mx1.acme.example", []impl.Cert{leaf})
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if !verdict.Secure {
		t.Fatalf("restored validator cannot verify a matching chain: %+v", verdict)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	v := danePolicyValidator(now, daneTwoMx(), nil)
	if _, err := v.Policy("acme.example"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	for _, payload := range [][]byte{nil, []byte("null"), []byte("{not json"), []byte(`{"version":99}`)} {
		if err := v.Restore(payload); !errors.Is(err, impl.ErrBadSnapshot) {
			t.Fatalf("payload %q: err = %v, want ErrBadSnapshot", string(payload), err)
		}
	}
	if _, ok := v.Cached("acme.example"); !ok {
		t.Fatalf("corrupt restore wiped the live policy")
	}
}
