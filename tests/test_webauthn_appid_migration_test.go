package task

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-a-06"
)

const testTenant = "tenant-a"

func testLimits() impl.Limits {
	return impl.Limits{
		MaxCredentialsPerUser: 4,
		MaxAppIDMappings:      8,
		MaxMigrationBatch:     4,
	}
}

func nativeAttestation(credIDHex string, counter uint32) impl.Attestation {
	credID, _ := hex.DecodeString(credIDHex)
	return impl.Attestation{
		TenantID:         testTenant,
		CredID:           credID,
		AAGUID:           "aaguid-9f21",
		Counter:          counter,
		CounterSupported: true,
	}
}

func legacyAttestation(appid, keyHandleHex, credIDHex string, counter uint32) impl.Attestation {
	att := nativeAttestation(credIDHex, counter)
	att.AppID = appid
	att.KeyHandle, _ = hex.DecodeString(keyHandleHex)
	return att
}

func credHex(c impl.Credential) string {
	return hex.EncodeToString(c.CredID)
}

func TestRegisterStoresNativeCredential(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	cred, err := g.Register("user-1", nativeAttestation("aa01", 3))
	if err != nil {
		t.Fatalf("registration of a native credential must succeed, got %v", err)
	}
	if cred.UserID != "user-1" || credHex(cred) != "aa01" {
		t.Fatalf("credential identity mismatch: user=%q cred=%s", cred.UserID, credHex(cred))
	}
	if cred.Scheme != impl.SchemeNative {
		t.Fatalf("scheme = %q, want %q", cred.Scheme, impl.SchemeNative)
	}
	if cred.AppID != "" {
		t.Fatalf("native credential must not carry a legacy appid, got %q", cred.AppID)
	}
	if cred.Counter != 3 {
		t.Fatalf("stored counter = %d, want 3", cred.Counter)
	}
	if cred.Created <= 0 {
		t.Fatalf("created sequence = %d, want a positive monotonic number", cred.Created)
	}
	list := g.Credentials("user-1")
	if len(list) != 1 {
		t.Fatalf("credential list length = %d, want 1", len(list))
	}
	if got := g.Stats().Credentials; got != 1 {
		t.Fatalf("stats credentials = %d, want 1", got)
	}
}

func TestRegisterRejectsBlankIdentity(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	if _, err := g.Register("   ", nativeAttestation("aa02", 1)); !errors.Is(err, impl.ErrBlankIdentity) {
		t.Fatalf("blank user id must be rejected with ErrBlankIdentity, got %v", err)
	}
	att := nativeAttestation("aa02", 1)
	att.CredID = nil
	if _, err := g.Register("user-1", att); !errors.Is(err, impl.ErrBlankIdentity) {
		t.Fatalf("empty credential id must be rejected with ErrBlankIdentity, got %v", err)
	}
	if got := len(g.Credentials("user-1")); got != 0 {
		t.Fatalf("rejected registrations must not leave credentials, got %d", got)
	}
}

func TestRegisterRejectsUnregisteredAppID(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	att := legacyAttestation("https://unknown.example.com", "bb01", "aa03", 1)
	if _, err := g.Register("user-1", att); !errors.Is(err, impl.ErrAppIDMismatch) {
		t.Fatalf("unknown legacy appid must be rejected with ErrAppIDMismatch, got %v", err)
	}
	if got := len(g.Credentials("user-1")); got != 0 {
		t.Fatalf("rejected registration must not leave credentials, got %d", got)
	}
}

func TestRegisterEnforcesCredentialLimit(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(impl.Limits{
		MaxCredentialsPerUser: 2,
		MaxAppIDMappings:      8,
		MaxMigrationBatch:     4,
	})
	if _, err := g.Register("user-2", nativeAttestation("cc01", 1)); err != nil {
		t.Fatalf("first credential: %v", err)
	}
	if _, err := g.Register("user-2", nativeAttestation("cc02", 1)); err != nil {
		t.Fatalf("second credential: %v", err)
	}
	if _, err := g.Register("user-2", nativeAttestation("cc03", 1)); !errors.Is(err, impl.ErrCredentialLimit) {
		t.Fatalf("third credential must hit the per user cap, got %v", err)
	}
	if got := len(g.Credentials("user-2")); got != 2 {
		t.Fatalf("credential count = %d, want the configured cap 2", got)
	}
}

func TestBindLegacyEnforcesMappingLimit(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(impl.Limits{
		MaxCredentialsPerUser: 4,
		MaxAppIDMappings:      1,
		MaxMigrationBatch:     4,
	})
	kh1, _ := hex.DecodeString("dd01")
	cid1, _ := hex.DecodeString("ee01")
	if err := g.BindLegacy("https://legacy.example.com", kh1, cid1); err != nil {
		t.Fatalf("first mapping must be accepted, got %v", err)
	}
	kh2, _ := hex.DecodeString("dd02")
	cid2, _ := hex.DecodeString("ee02")
	if err := g.BindLegacy("https://legacy.example.com", kh2, cid2); !errors.Is(err, impl.ErrMappingLimit) {
		t.Fatalf("second mapping must hit the mapping cap, got %v", err)
	}
	if _, ok := g.Mapping("https://legacy.example.com", kh2); ok {
		t.Fatalf("rejected mapping must not be stored")
	}
	if got := g.Stats().Mappings; got != 1 {
		t.Fatalf("stats mappings = %d, want 1", got)
	}
}

func TestBindLegacyRejectsConflictingCredential(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	kh, _ := hex.DecodeString("dd11")
	cid1, _ := hex.DecodeString("ee11")
	cid2, _ := hex.DecodeString("ee12")
	if err := g.BindLegacy("https://legacy.example.com", kh, cid1); err != nil {
		t.Fatalf("first mapping: %v", err)
	}
	if err := g.BindLegacy("https://legacy.example.com", kh, cid2); !errors.Is(err, impl.ErrMappingConflict) {
		t.Fatalf("second credential for the same key handle must conflict, got %v", err)
	}
	got, ok := g.Mapping("https://legacy.example.com", kh)
	if !ok {
		t.Fatalf("mapping must still resolve after a rejected conflict")
	}
	if hex.EncodeToString(got) != "ee11" {
		t.Fatalf("mapping resolved to %s, want the originally bound ee11", hex.EncodeToString(got))
	}
}

func TestAuthenticateAdvancesCounter(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	if _, err := g.Register("user-3", nativeAttestation("ff01", 5)); err != nil {
		t.Fatalf("register: %v", err)
	}
	credID, _ := hex.DecodeString("ff01")
	res, err := g.Authenticate("user-3", impl.Assertion{TenantID: testTenant, CredID: credID, Counter: 6})
	if err != nil {
		t.Fatalf("assertion with an advanced counter must succeed, got %v", err)
	}
	if res.Counter != 6 {
		t.Fatalf("result counter = %d, want 6", res.Counter)
	}
	if res.UserID != "user-3" {
		t.Fatalf("result user = %q, want user-3", res.UserID)
	}
	if res.Degraded {
		t.Fatalf("native assertion must not be reported as degraded")
	}
	res, err = g.Authenticate("user-3", impl.Assertion{TenantID: testTenant, CredID: credID, Counter: 7})
	if err != nil {
		t.Fatalf("second assertion: %v", err)
	}
	if res.Counter != 7 {
		t.Fatalf("second result counter = %d, want 7", res.Counter)
	}
	if got := g.Credentials("user-3")[0].Counter; got != 7 {
		t.Fatalf("stored counter = %d, want 7", got)
	}
}

func TestAuthenticateRejectsCounterRollback(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	if _, err := g.Register("user-4", nativeAttestation("ff11", 10)); err != nil {
		t.Fatalf("register: %v", err)
	}
	credID, _ := hex.DecodeString("ff11")
	if _, err := g.Authenticate("user-4", impl.Assertion{TenantID: testTenant, CredID: credID, Counter: 10}); !errors.Is(err, impl.ErrCounterRollback) {
		t.Fatalf("equal counter must be rejected with ErrCounterRollback, got %v", err)
	}
	if got := g.Credentials("user-4")[0].Counter; got != 10 {
		t.Fatalf("rejected assertion must not move the watermark, got %d", got)
	}
	if got := g.Credentials("user-4")[0].Flag; got != impl.FlagSuspicious {
		t.Fatalf("flagged credential = %q, want %q", got, impl.FlagSuspicious)
	}
	if _, err := g.Authenticate("user-4", impl.Assertion{TenantID: testTenant, CredID: credID, Counter: 11}); !errors.Is(err, impl.ErrCredentialRevoked) {
		t.Fatalf("flagged credential must refuse further assertions, got %v", err)
	}
}

func TestAuthenticateRejectsUnknownCredential(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	credID, _ := hex.DecodeString("aabb")
	_, err := g.Authenticate("user-5", impl.Assertion{TenantID: testTenant, CredID: credID, Counter: 1})
	if !errors.Is(err, impl.ErrUnknownCredential) {
		t.Fatalf("unknown credential must be rejected with ErrUnknownCredential, got %v", err)
	}
}

func TestAuthenticateRejectsAppIDMismatch(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	appid := "https://legacy.example.com"
	kh, _ := hex.DecodeString("bb21")
	cid, _ := hex.DecodeString("aa21")
	if err := g.BindLegacy(appid, kh, cid); err != nil {
		t.Fatalf("bind legacy: %v", err)
	}
	if _, err := g.Register("user-6", legacyAttestation(appid, "bb21", "aa21", 2)); err != nil {
		t.Fatalf("legacy registration: %v", err)
	}
	_, err := g.Authenticate("user-6", impl.Assertion{
		TenantID: testTenant,
		CredID:   cid,
		AppID:    "https://evil.example.com",
		Counter:  3,
	})
	if !errors.Is(err, impl.ErrAppIDMismatch) {
		t.Fatalf("foreign appid must be rejected with ErrAppIDMismatch, got %v", err)
	}
	if got := g.Credentials("user-6")[0].Counter; got != 2 {
		t.Fatalf("rejected assertion must not move the counter, got %d", got)
	}
}

func TestRegisterRewritesLegacyKeyHandle(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	appid := "https://legacy.example.com"
	kh, _ := hex.DecodeString("bb31")
	cid, _ := hex.DecodeString("aa31")
	if err := g.BindLegacy(appid, kh, cid); err != nil {
		t.Fatalf("bind legacy: %v", err)
	}
	cred, err := g.Register("user-7", legacyAttestation(appid, "bb31", "aa31", 4))
	if err != nil {
		t.Fatalf("legacy registration through a mapping must succeed, got %v", err)
	}
	if cred.Scheme != impl.SchemeLegacy {
		t.Fatalf("scheme = %q, want %q", cred.Scheme, impl.SchemeLegacy)
	}
	if cred.AppID != appid {
		t.Fatalf("credential appid = %q, want %q", cred.AppID, appid)
	}
	if credHex(cred) != "aa31" {
		t.Fatalf("credential id = %s, want the mapped aa31", credHex(cred))
	}
	other, _ := hex.DecodeString("aa32")
	att := legacyAttestation(appid, "bb31", "aa32", 4)
	att.CredID = other
	if _, err := g.Register("user-7", att); !errors.Is(err, impl.ErrMappingConflict) {
		t.Fatalf("same key handle with another credential must conflict, got %v", err)
	}
}

func TestMigrateRewritesLegacyCredentials(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	appid := "https://legacy.example.com"
	for i, pair := range [][2]string{{"bb41", "aa41"}, {"bb42", "aa42"}} {
		kh, _ := hex.DecodeString(pair[0])
		cid, _ := hex.DecodeString(pair[1])
		if err := g.BindLegacy(appid, kh, cid); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		if _, err := g.Register("user-8", legacyAttestation(appid, pair[0], pair[1], 1)); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	n, err := g.Migrate("user-8")
	if err != nil {
		t.Fatalf("migration must succeed, got %v", err)
	}
	if n != 2 {
		t.Fatalf("migrated count = %d, want 2", n)
	}
	for _, c := range g.Credentials("user-8") {
		if c.Scheme != impl.SchemeNative {
			t.Fatalf("credential %s scheme = %q, want %q", credHex(c), c.Scheme, impl.SchemeNative)
		}
		if c.AppID != "" {
			t.Fatalf("migrated credential %s still carries appid %q", credHex(c), c.AppID)
		}
	}
	if got := g.Stats().Migrated; got != 2 {
		t.Fatalf("stats migrated = %d, want 2", got)
	}
	again, err := g.Migrate("user-8")
	if err != nil {
		t.Fatalf("second migration must succeed as a no-op, got %v", err)
	}
	if again != 0 {
		t.Fatalf("second migration count = %d, want 0", again)
	}
}

func TestMigrateEnforcesBatchLimit(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(impl.Limits{
		MaxCredentialsPerUser: 8,
		MaxAppIDMappings:      8,
		MaxMigrationBatch:     1,
	})
	appid := "https://legacy.example.com"
	for _, pair := range [][2]string{{"bb51", "aa51"}, {"bb52", "aa52"}} {
		kh, _ := hex.DecodeString(pair[0])
		cid, _ := hex.DecodeString(pair[1])
		if err := g.BindLegacy(appid, kh, cid); err != nil {
			t.Fatalf("bind: %v", err)
		}
		if _, err := g.Register("user-9", legacyAttestation(appid, pair[0], pair[1], 1)); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	n, err := g.Migrate("user-9")
	if !errors.Is(err, impl.ErrBatchLimit) {
		t.Fatalf("oversized batch must be rejected with ErrBatchLimit, got %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected batch must report 0 rewritten records, got %d", n)
	}
	for _, c := range g.Credentials("user-9") {
		if c.Scheme != impl.SchemeLegacy {
			t.Fatalf("credential %s was rewritten despite the rejected batch", credHex(c))
		}
	}
}

func TestConcurrentAuthenticateAdvancesOnce(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	if _, err := g.Register("user-10", nativeAttestation("ff21", 100)); err != nil {
		t.Fatalf("register: %v", err)
	}
	credID, _ := hex.DecodeString("ff21")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := g.Authenticate("user-10", impl.Assertion{TenantID: testTenant, CredID: credID, Counter: 101})
			_ = res
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("successful concurrent assertions = %d, want exactly 1", ok)
	}
	if got := g.Credentials("user-10")[0].Counter; got != 101 {
		t.Fatalf("stored counter = %d, want 101", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	g := impl.NewAppIdMigrationGuard(testLimits())
	appid := "https://legacy.example.com"
	kh, _ := hex.DecodeString("bb61")
	cid, _ := hex.DecodeString("aa61")
	if err := g.BindLegacy(appid, kh, cid); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := g.Register("user-11", legacyAttestation(appid, "bb61", "aa61", 7)); err != nil {
		t.Fatalf("register: %v", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewAppIdMigrationGuard(testLimits())
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("restore of a fresh snapshot must succeed, got %v", err)
	}
	list := restored.Credentials("user-11")
	if len(list) != 1 {
		t.Fatalf("restored credential count = %d, want 1", len(list))
	}
	if list[0].Scheme != impl.SchemeLegacy || list[0].AppID != appid {
		t.Fatalf("restored credential lost its legacy binding: scheme=%q appid=%q", list[0].Scheme, list[0].AppID)
	}
	if list[0].Counter != 7 {
		t.Fatalf("restored counter = %d, want 7", list[0].Counter)
	}
	if got, ok := restored.Mapping(appid, kh); !ok || hex.EncodeToString(got) != "aa61" {
		t.Fatalf("restored mapping missing or wrong: %v %t", got, ok)
	}
	if err := restored.Restore([]byte("{not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must be rejected with ErrBadSnapshot, got %v", err)
	}
	if got := len(restored.Credentials("user-11")); got != 1 {
		t.Fatalf("rejected restore must keep the live state, got %d credentials", got)
	}
}
