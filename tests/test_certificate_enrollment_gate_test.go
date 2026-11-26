package task

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-05"
)

func baseConfig() impl.Config {
	return impl.Config{
		DomainSuffix:         "corp.example.com",
		MinRSABits:           2048,
		MaxAttemptsPerDevice: 5,
		MaxQueue:             8,
		MaxPendingAge:        10 * time.Minute,
		RetryAfter:           5 * time.Second,
	}
}

func ecKey(t *testing.T) crypto.Signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	return key
}

func rsaKey(t *testing.T, bits int) crypto.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return key
}

func csrWith(t *testing.T, key crypto.Signer, cn string, dns []string, isCA bool) []byte {
	t.Helper()
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: cn},
		DNSNames: dns,
	}
	if isCA {
		value, err := asn1.Marshal(struct {
			IsCA bool `asn1:"optional"`
		}{IsCA: true})
		if err != nil {
			t.Fatalf("encode basic constraints: %v", err)
		}
		tmpl.ExtraExtensions = []pkix.Extension{{
			Id:       asn1.ObjectIdentifier{2, 5, 29, 19},
			Critical: true,
			Value:    value,
		}}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return der
}

func validCSR(t *testing.T, cn string) []byte {
	t.Helper()
	return csrWith(t, ecKey(t), cn, []string{cn + ".corp.example.com"}, false)
}

func enrollRequest(device, tenant, token string, csr []byte) impl.EnrollRequest {
	return impl.EnrollRequest{
		DeviceID:       device,
		TenantID:       tenant,
		BootstrapToken: token,
		CSR:            csr,
	}
}

func TestEnrollAcceptsValidCSR(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	en, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", validCSR(t, "svc-1")))
	if err != nil {
		t.Fatalf("enroll a valid csr: %v", err)
	}
	if en.ID == "" {
		t.Fatalf("enrollment must carry an id")
	}
	if en.State != impl.StateQueued {
		t.Fatalf("enrollment state = %q, want %q", en.State, impl.StateQueued)
	}
	if en.DeviceID != "dev-1" || en.TenantID != "tenant-a" {
		t.Fatalf("enrollment identity = %+v, want dev-1 in tenant-a", en)
	}
	if en.RetryAfter != 5*time.Second {
		t.Fatalf("retry after = %v, want the configured 5s", en.RetryAfter)
	}
	if en.CAEpoch != 1 {
		t.Fatalf("ca epoch = %d, want the first generation 1", en.CAEpoch)
	}
	budget, err := g.Budget("dev-1")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if budget.Queued != 1 {
		t.Fatalf("queued enrollments = %d, want 1", budget.Queued)
	}
	if got := g.Pending("tenant-a"); got != 1 {
		t.Fatalf("tenant pending = %d, want 1", got)
	}
}

func TestEnrollRejectsBadRequests(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	csr := validCSR(t, "svc-1")
	if _, err := g.Enroll(context.Background(), enrollRequest("  ", "tenant-a", "tok-1", csr)); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("blank device id must fail with ErrBadRequest, got %v", err)
	}
	if _, err := g.Enroll(context.Background(), enrollRequest("dev-1", "", "tok-1", csr)); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("blank tenant id must fail with ErrBadRequest, got %v", err)
	}
	if _, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-unknown", csr)); !errors.Is(err, impl.ErrBootstrapRejected) {
		t.Fatalf("an unknown bootstrap token must fail with ErrBootstrapRejected, got %v", err)
	}
	if got := g.Pending("tenant-a"); got != 0 {
		t.Fatalf("rejected requests must not queue anything, got %d pending", got)
	}
}

func TestBootstrapTokenIsSingleUse(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	if _, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", validCSR(t, "svc-1"))); err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	_, err := g.Enroll(context.Background(), enrollRequest("dev-2", "tenant-a", "tok-1", validCSR(t, "svc-2")))
	if !errors.Is(err, impl.ErrBootstrapRejected) {
		t.Fatalf("a consumed bootstrap token must fail with ErrBootstrapRejected, got %v", err)
	}
}

func TestEnrollRejectsWeakKey(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	csr := csrWith(t, rsaKey(t, 1024), "svc-weak", []string{"svc-weak.corp.example.com"}, false)
	_, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", csr))
	if !errors.Is(err, impl.ErrKeyTooWeak) {
		t.Fatalf("a 1024 bit rsa key must fail with ErrKeyTooWeak, got %v", err)
	}
}

func TestEnrollRejectsSANOutsideTenant(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	csr := csrWith(t, ecKey(t), "svc-foreign", []string{"svc.other.example"}, false)
	_, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", csr))
	if !errors.Is(err, impl.ErrSANOutsideTenant) {
		t.Fatalf("a san outside the tenant domain must fail with ErrSANOutsideTenant, got %v", err)
	}
}

func TestEnrollRejectsCAFlag(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	csr := csrWith(t, ecKey(t), "svc-ca", []string{"svc-ca.corp.example.com"}, true)
	_, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", csr))
	if !errors.Is(err, impl.ErrCAFlagForbidden) {
		t.Fatalf("a csr asking for ca basic constraints must fail with ErrCAFlagForbidden, got %v", err)
	}
}

func TestEnrollRejectsBroadWildcard(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	if err := g.RegisterBootstrapToken("tok-2", "tenant-a"); err != nil {
		t.Fatalf("register second token: %v", err)
	}
	broad := csrWith(t, ecKey(t), "svc-wild", []string{"*.example.com"}, false)
	if _, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", broad)); !errors.Is(err, impl.ErrWildcardTooBroad) {
		t.Fatalf("a wildcard above the tenant suffix must fail with ErrWildcardTooBroad, got %v", err)
	}
	narrow := csrWith(t, ecKey(t), "svc-wild", []string{"*.corp.example.com"}, false)
	if _, err := g.Enroll(context.Background(), enrollRequest("dev-2", "tenant-a", "tok-2", narrow)); err != nil {
		t.Fatalf("a wildcard inside the tenant suffix must be accepted, got %v", err)
	}
}

func TestEnrollRejectsMissingSAN(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	csr := csrWith(t, ecKey(t), "svc-bare", nil, false)
	_, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", csr))
	if !errors.Is(err, impl.ErrSANMissing) {
		t.Fatalf("a csr without san must fail with ErrSANMissing, got %v", err)
	}
}

func TestEnrollIsIdempotentForSameCSR(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	for _, token := range []string{"tok-1", "tok-2"} {
		if err := g.RegisterBootstrapToken(token, "tenant-a"); err != nil {
			t.Fatalf("register %s: %v", token, err)
		}
	}
	csr := validCSR(t, "svc-1")
	first, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", csr))
	if err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	second, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-2", csr))
	if err != nil {
		t.Fatalf("resubmitting the same csr must not fail, got %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("resubmission returned id %q, want the in flight enrollment %q", second.ID, first.ID)
	}
	if got := g.Pending("tenant-a"); got != 1 {
		t.Fatalf("tenant pending = %d, want a single queued enrollment", got)
	}
}

func TestEnrollEnforcesQueueCap(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxQueue = 2
	g := impl.NewEnrollmentGate(cfg)
	for i := 0; i < 3; i++ {
		token := "tok-" + string(rune('1'+i))
		if err := g.RegisterBootstrapToken(token, "tenant-a"); err != nil {
			t.Fatalf("register %s: %v", token, err)
		}
	}
	for i := 0; i < 2; i++ {
		token := "tok-" + string(rune('1'+i))
		csr := validCSR(t, "svc-"+string(rune('1'+i)))
		if _, err := g.Enroll(context.Background(), enrollRequest("dev-"+string(rune('1'+i)), "tenant-a", token, csr)); err != nil {
			t.Fatalf("enrollment %d: %v", i, err)
		}
	}
	third := validCSR(t, "svc-3")
	_, err := g.Enroll(context.Background(), enrollRequest("dev-3", "tenant-a", "tok-3", third))
	if !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("the third enrollment must fail with ErrQueueFull, got %v", err)
	}
	if got := g.Pending("tenant-a"); got != 2 {
		t.Fatalf("tenant pending = %d, want the cap 2", got)
	}
}

func TestEnrollExhaustsAttempts(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxAttemptsPerDevice = 2
	g := impl.NewEnrollmentGate(cfg)
	for i := 0; i < 3; i++ {
		token := "tok-" + string(rune('1'+i))
		if err := g.RegisterBootstrapToken(token, "tenant-a"); err != nil {
			t.Fatalf("register %s: %v", token, err)
		}
	}
	weak := func() []byte {
		return csrWith(t, rsaKey(t, 1024), "svc-weak", []string{"svc-weak.corp.example.com"}, false)
	}
	for i := 0; i < 2; i++ {
		token := "tok-" + string(rune('1'+i))
		if _, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", token, weak())); !errors.Is(err, impl.ErrKeyTooWeak) {
			t.Fatalf("attempt %d must fail with ErrKeyTooWeak, got %v", i, err)
		}
	}
	_, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-3", validCSR(t, "svc-1")))
	if !errors.Is(err, impl.ErrAttemptsExhausted) {
		t.Fatalf("a device over its attempt budget must fail with ErrAttemptsExhausted, got %v", err)
	}
	budget, err := g.Budget("dev-1")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if budget.Attempts != 2 {
		t.Fatalf("used attempts = %d, want 2", budget.Attempts)
	}
	if budget.Queued != 0 {
		t.Fatalf("queued enrollments = %d, want 0", budget.Queued)
	}
}

func TestPollReturnsStateAndUnknownFails(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	en, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", validCSR(t, "svc-1")))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	st, err := g.Poll(en.ID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if st.ID != en.ID || st.State != impl.StateQueued {
		t.Fatalf("polled state = %+v, want the queued enrollment %q", st, en.ID)
	}
	if st.Attempts != 0 {
		t.Fatalf("a queued enrollment reports %d attempts, want 0", st.Attempts)
	}
	if _, err := g.Poll("enr-404"); !errors.Is(err, impl.ErrUnknownEnrollment) {
		t.Fatalf("polling an unknown enrollment must fail with ErrUnknownEnrollment, got %v", err)
	}
}

func TestConcurrentEnrollSameDeviceSinglePending(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	const workers = 8
	for i := 0; i < workers; i++ {
		token := "tok-" + string(rune('a'+i))
		if err := g.RegisterBootstrapToken(token, "tenant-a"); err != nil {
			t.Fatalf("register %s: %v", token, err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make(map[string]int)
	var failures []error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			token := "tok-" + string(rune('a'+slot))
			cn := "svc-" + string(rune('a'+slot))
			en, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", token, validCSR(t, cn)))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			ids[en.ID]++
		}(i)
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("concurrent enrollment reported %d errors, first %v", len(failures), failures[0])
	}
	if len(ids) != 1 {
		t.Fatalf("concurrent enrollment produced %d distinct ids, want 1", len(ids))
	}
	for id, count := range ids {
		if count != workers {
			t.Fatalf("enrollment %q was returned %d times, want %d", id, count, workers)
		}
	}
	if got := g.Pending("tenant-a"); got != 1 {
		t.Fatalf("tenant pending = %d, want 1 in flight enrollment", got)
	}
	if budget, err := g.Budget("dev-1"); err != nil || budget.Queued != 1 {
		t.Fatalf("device budget = %+v err=%v, want one queued enrollment", budget, err)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	g := impl.NewEnrollmentGate(baseConfig())
	if err := g.RegisterBootstrapToken("tok-1", "tenant-a"); err != nil {
		t.Fatalf("register token: %v", err)
	}
	en, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", validCSR(t, "svc-1")))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := g.Revoke(en.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	st, err := g.Poll(en.ID)
	if err != nil {
		t.Fatalf("poll after revoke: %v", err)
	}
	if st.State != impl.StateRevoked {
		t.Fatalf("state after revoke = %q, want %q", st.State, impl.StateRevoked)
	}
	if err := g.Revoke(en.ID); err != nil {
		t.Fatalf("revoking twice must be idempotent, got %v", err)
	}
	if err := g.Revoke("enr-404"); !errors.Is(err, impl.ErrUnknownEnrollment) {
		t.Fatalf("revoking an unknown enrollment must fail with ErrUnknownEnrollment, got %v", err)
	}
	if budget, err := g.Budget("dev-1"); err != nil || budget.Queued != 0 {
		t.Fatalf("device budget = %+v err=%v, want no queued enrollment after revoke", budget, err)
	}
}

func TestSnapshotRestoreKeepsQueueAndBudget(t *testing.T) {
	cfg := baseConfig()
	g := impl.NewEnrollmentGate(cfg)
	for _, token := range []string{"tok-1", "tok-2"} {
		if err := g.RegisterBootstrapToken(token, "tenant-a"); err != nil {
			t.Fatalf("register %s: %v", token, err)
		}
	}
	en, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-1", validCSR(t, "svc-1")))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	weak := csrWith(t, rsaKey(t, 1024), "svc-weak", []string{"svc-weak.corp.example.com"}, false)
	if _, err := g.Enroll(context.Background(), enrollRequest("dev-1", "tenant-a", "tok-2", weak)); !errors.Is(err, impl.ErrKeyTooWeak) {
		t.Fatalf("weak attempt must fail with ErrKeyTooWeak, got %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewEnrollmentGate(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := restored.Poll(en.ID)
	if err != nil {
		t.Fatalf("poll after restore: %v", err)
	}
	if st.State != impl.StateQueued {
		t.Fatalf("restored state = %q, want queued", st.State)
	}
	budget, err := restored.Budget("dev-1")
	if err != nil {
		t.Fatalf("budget after restore: %v", err)
	}
	if budget.Attempts != 1 || budget.Queued != 1 {
		t.Fatalf("restored budget = %+v, want 1 spent attempt and 1 queued enrollment", budget)
	}
	if _, err := restored.Enroll(context.Background(), enrollRequest("dev-9", "tenant-a", "tok-2", validCSR(t, "svc-9"))); !errors.Is(err, impl.ErrBootstrapRejected) {
		t.Fatalf("a token consumed before the snapshot must stay consumed, got %v", err)
	}
}
