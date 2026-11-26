package task

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-03"
)

const submissionOne = `{"descriptor_map":[{"id":"d1","path":"$.credentials[0]","format":"vc+sd-jwt"}]}`

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

func baseConfig() impl.Config {
	return impl.Config{
		MaxPendingPerTenant:      4,
		MaxCredentialsPerRequest: 2,
		TTL:                      5 * time.Minute,
		Formats:                  []string{"vc+sd-jwt"},
		Audience:                 "verifier-gateway",
	}
}

func newVerifier(clock *testClock, cfg impl.Config) *impl.PresentationVerifier {
	cfg.Now = clock.Now
	return impl.NewPresentationVerifier(cfg)
}

func degreeDefinition() impl.PresentationDefinition {
	return impl.PresentationDefinition{
		ID: "pd-degree",
		Descriptors: []impl.InputDescriptor{{
			ID:      "d1",
			Formats: []string{"vc+sd-jwt"},
			Fields: []impl.FieldConstraint{{
				Path:   "degree.type",
				Values: []string{"BachelorDegree", "MasterDegree"},
			}},
		}},
	}
}

type credStatus struct {
	URI   string `json:"uri"`
	Index int    `json:"index"`
	Epoch int64  `json:"epoch"`
}

type keyBinding struct {
	Aud   string `json:"aud"`
	Nonce string `json:"nonce"`
	Iat   int64  `json:"iat"`
	Iss   string `json:"iss"`
}

type credential struct {
	ID     string            `json:"id"`
	Format string            `json:"format"`
	Holder string            `json:"holder"`
	Claims map[string]string `json:"claims"`
	Status *credStatus       `json:"status,omitempty"`
	KB     *keyBinding       `json:"kb,omitempty"`
}

type vpToken struct {
	Credentials []credential `json:"credentials"`
}

func encodeToken(t *testing.T, creds ...credential) string {
	t.Helper()
	raw, err := json.Marshal(vpToken{Credentials: creds})
	if err != nil {
		t.Fatalf("encode vp_token: %v", err)
	}
	return string(raw)
}

func goodCredential(req impl.Request, clock *testClock) credential {
	return credential{
		ID:     "c1",
		Format: "vc+sd-jwt",
		Holder: "did:ex:alice",
		Claims: map[string]string{"degree.type": "BachelorDegree"},
		KB: &keyBinding{
			Aud:   "verifier-gateway",
			Nonce: req.Nonce,
			Iat:   clock.Now().Unix(),
			Iss:   "did:ex:alice",
		},
	}
}

func TestCreateRequestIssuesNonceAndPendingState(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if req.ID == "" || req.Nonce == "" {
		t.Fatalf("request must carry an id and a nonce, got id=%q nonce=%q", req.ID, req.Nonce)
	}
	if req.TenantID != "tenant-a" {
		t.Fatalf("request tenant = %q, want tenant-a", req.TenantID)
	}
	if req.DefinitionID != "pd-degree" {
		t.Fatalf("request definition = %q, want pd-degree", req.DefinitionID)
	}
	if !req.ExpiresAt.After(clock.Now()) {
		t.Fatalf("request expiry %v must lie in the future", req.ExpiresAt)
	}
	st, err := v.State(req.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Status != impl.StatusPending {
		t.Fatalf("fresh request status = %q, want %q", st.Status, impl.StatusPending)
	}
	if st.Submitted != 0 {
		t.Fatalf("fresh request submitted count = %d, want 0", st.Submitted)
	}
}

func TestCreateRequestRejectsBadDefinitions(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	if _, err := v.CreateRequest("tenant-a", impl.PresentationDefinition{ID: "pd-empty"}); !errors.Is(err, impl.ErrBadDefinition) {
		t.Fatalf("a definition without descriptors must fail with ErrBadDefinition, got %v", err)
	}
	wide := degreeDefinition()
	wide.Descriptors = append(wide.Descriptors,
		impl.InputDescriptor{ID: "d2", Formats: []string{"vc+sd-jwt"}},
		impl.InputDescriptor{ID: "d3", Formats: []string{"vc+sd-jwt"}},
	)
	if _, err := v.CreateRequest("tenant-a", wide); !errors.Is(err, impl.ErrBadDefinition) {
		t.Fatalf("three descriptors against a cap of two must fail with ErrBadDefinition, got %v", err)
	}
}

func TestCreateRequestRejectsUnsupportedFormat(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	def := degreeDefinition()
	def.Descriptors[0].Formats = []string{"mdl"}
	if _, err := v.CreateRequest("tenant-a", def); !errors.Is(err, impl.ErrUnsupportedFormat) {
		t.Fatalf("an unsupported descriptor format must fail with ErrUnsupportedFormat, got %v", err)
	}
	got := v.SupportedFormats()
	if len(got) != 1 || got[0] != "vc+sd-jwt" {
		t.Fatalf("supported formats = %v, want [vc+sd-jwt]", got)
	}
}

func TestCreateRequestEnforcesPendingCapPerTenant(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	cfg := baseConfig()
	cfg.MaxPendingPerTenant = 2
	v := newVerifier(clock, cfg)
	for i := 0; i < 2; i++ {
		if _, err := v.CreateRequest("tenant-a", degreeDefinition()); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if _, err := v.CreateRequest("tenant-a", degreeDefinition()); !errors.Is(err, impl.ErrRequestLimit) {
		t.Fatalf("third pending request must fail with ErrRequestLimit, got %v", err)
	}
	if _, err := v.CreateRequest("tenant-b", degreeDefinition()); err != nil {
		t.Fatalf("another tenant must not share the cap, got %v", err)
	}
	if got := len(v.PendingRequests("tenant-a")); got != 2 {
		t.Fatalf("tenant-a pending requests = %d, want 2", got)
	}
}

func TestVerifyAcceptsMatchingCredential(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	token := encodeToken(t, goodCredential(req, clock))
	res, err := v.Verify(req.ID, token, submissionOne)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Status != impl.StatusVerified || res.Reason != impl.ReasonOK {
		t.Fatalf("result status=%q reason=%q, want verified/ok", res.Status, res.Reason)
	}
	if len(res.Matched) != 1 || res.Matched[0] != "d1" {
		t.Fatalf("matched descriptors = %v, want [d1]", res.Matched)
	}
	if res.Subject != "did:ex:alice" {
		t.Fatalf("subject = %q, want did:ex:alice", res.Subject)
	}
	st, err := v.State(req.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Status != impl.StatusVerified || st.Submitted != 1 {
		t.Fatalf("state status=%q submitted=%d, want verified and 1", st.Status, st.Submitted)
	}
}

func TestVerifyRejectsMissingClaimValue(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	cred := goodCredential(req, clock)
	cred.Claims = map[string]string{"degree.level": "bachelor"}
	token := encodeToken(t, cred)
	res, err := v.Verify(req.ID, token, submissionOne)
	if err != nil {
		t.Fatalf("verify returned an error instead of a rejection: %v", err)
	}
	if res.Status != impl.StatusRejected || res.Reason != impl.ReasonConstraintFailed {
		t.Fatalf("result status=%q reason=%q, want rejected/constraint_failed", res.Status, res.Reason)
	}
	st, err := v.State(req.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Status != impl.StatusRejected {
		t.Fatalf("state status = %q, want rejected", st.Status)
	}
}

func TestVerifyRejectsNonceMismatch(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	cred := goodCredential(req, clock)
	cred.KB.Nonce = "nonce-from-another-request"
	res, err := v.Verify(req.ID, encodeToken(t, cred), submissionOne)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Status != impl.StatusRejected || res.Reason != impl.ReasonNonceMismatch {
		t.Fatalf("result status=%q reason=%q, want rejected/nonce_mismatch", res.Status, res.Reason)
	}
}

func TestVerifyRejectsAudienceMismatch(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	cred := goodCredential(req, clock)
	cred.KB.Aud = "some-other-verifier"
	res, err := v.Verify(req.ID, encodeToken(t, cred), submissionOne)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Status != impl.StatusRejected || res.Reason != impl.ReasonAudienceMismatch {
		t.Fatalf("result status=%q reason=%q, want rejected/audience_mismatch", res.Status, res.Reason)
	}
}

func TestVerifyRejectsHolderMismatch(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	cred := goodCredential(req, clock)
	cred.KB.Iss = "did:ex:mallory"
	res, err := v.Verify(req.ID, encodeToken(t, cred), submissionOne)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Status != impl.StatusRejected || res.Reason != impl.ReasonHolderMismatch {
		t.Fatalf("result status=%q reason=%q, want rejected/holder_mismatch", res.Status, res.Reason)
	}
}

func TestVerifyRejectsRevokedCredential(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := v.PutStatusList(impl.StatusList{
		URI:     "https://issuer.example/status/1",
		Epoch:   3,
		Bits:    64,
		Revoked: []int{7},
	}); err != nil {
		t.Fatalf("put status list: %v", err)
	}
	if epoch, ok := v.StatusListEpoch("https://issuer.example/status/1"); !ok || epoch != 3 {
		t.Fatalf("cached status epoch = %d ok=%v, want 3 true", epoch, ok)
	}
	cred := goodCredential(req, clock)
	cred.Status = &credStatus{URI: "https://issuer.example/status/1", Index: 7, Epoch: 3}
	res, err := v.Verify(req.ID, encodeToken(t, cred), submissionOne)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Status != impl.StatusRejected || res.Reason != impl.ReasonRevoked {
		t.Fatalf("result status=%q reason=%q, want rejected/credential_revoked", res.Status, res.Reason)
	}
}

func TestVerifyRejectsStaleStatusEpoch(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := v.PutStatusList(impl.StatusList{
		URI:   "https://issuer.example/status/2",
		Epoch: 3,
		Bits:  64,
	}); err != nil {
		t.Fatalf("put status list: %v", err)
	}
	cred := goodCredential(req, clock)
	cred.Status = &credStatus{URI: "https://issuer.example/status/2", Index: 1, Epoch: 9}
	res, err := v.Verify(req.ID, encodeToken(t, cred), submissionOne)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Status != impl.StatusRejected || res.Reason != impl.ReasonStatusEpoch {
		t.Fatalf("result status=%q reason=%q, want rejected/status_epoch_mismatch", res.Status, res.Reason)
	}
}

func TestVerifyIsSingleShot(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	token := encodeToken(t, goodCredential(req, clock))
	if _, err := v.Verify(req.ID, token, submissionOne); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(req.ID, token, submissionOne); !errors.Is(err, impl.ErrRequestClosed) {
		t.Fatalf("second verify must fail with ErrRequestClosed, got %v", err)
	}
	st, err := v.State(req.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Status != impl.StatusVerified || st.Submitted != 1 {
		t.Fatalf("a rejected resubmission must not rewrite the conclusion, got status=%q submitted=%d", st.Status, st.Submitted)
	}
}

func TestConcurrentVerifyAllowsOneWinner(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	v := newVerifier(clock, baseConfig())
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	token := encodeToken(t, goodCredential(req, clock))
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, closed := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Verify(req.ID, token, submissionOne)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, impl.ErrRequestClosed):
				closed++
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("concurrent submissions produced %d winners, want 1", winners)
	}
	if closed != workers-1 {
		t.Fatalf("concurrent submissions reported %d ErrRequestClosed, want %d", closed, workers-1)
	}
}

func TestExpireSweepsOnlyPendingRequests(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	cfg := baseConfig()
	cfg.TTL = time.Minute
	v := newVerifier(clock, cfg)
	var ids []string
	for i := 0; i < 3; i++ {
		req, err := v.CreateRequest("tenant-a", degreeDefinition())
		if err != nil {
			t.Fatalf("create request %d: %v", i, err)
		}
		ids = append(ids, req.ID)
	}
	settled, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create settled request: %v", err)
	}
	token := encodeToken(t, goodCredential(settled, clock))
	if _, err := v.Verify(settled.ID, token, submissionOne); err != nil {
		t.Fatalf("verify the settled request: %v", err)
	}
	clock.Advance(2 * time.Minute)
	if swept := v.Expire(clock.Now()); swept != 3 {
		t.Fatalf("expire swept %d requests, want the 3 pending ones", swept)
	}
	for _, id := range ids {
		st, err := v.State(id)
		if err != nil {
			t.Fatalf("state of %s: %v", id, err)
		}
		if st.Status != impl.StatusExpired {
			t.Fatalf("request %s status = %q, want expired", id, st.Status)
		}
	}
	settledState, err := v.State(settled.ID)
	if err != nil {
		t.Fatalf("state of the settled request: %v", err)
	}
	if settledState.Status != impl.StatusVerified {
		t.Fatalf("a concluded request must not be expired, got %q", settledState.Status)
	}
	if _, err := v.Verify(ids[1], `{"credentials":[]}`, submissionOne); !errors.Is(err, impl.ErrRequestClosed) {
		t.Fatalf("an expired request must refuse submissions with ErrRequestClosed, got %v", err)
	}
}

func TestSnapshotRestoreKeepsConclusions(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)}
	cfg := baseConfig()
	v := newVerifier(clock, cfg)
	req, err := v.CreateRequest("tenant-a", degreeDefinition())
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	token := encodeToken(t, goodCredential(req, clock))
	if _, err := v.Verify(req.ID, token, submissionOne); err != nil {
		t.Fatalf("verify: %v", err)
	}
	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := newVerifier(clock, cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := restored.State(req.ID)
	if err != nil {
		t.Fatalf("state after restore: %v", err)
	}
	if st.Status != impl.StatusVerified || st.Subject != "did:ex:alice" {
		t.Fatalf("restored state status=%q subject=%q, want verified/did:ex:alice", st.Status, st.Subject)
	}
	if _, err := restored.Verify(req.ID, token, submissionOne); !errors.Is(err, impl.ErrRequestClosed) {
		t.Fatalf("a restored conclusion must stay closed, got %v", err)
	}
}
