package task

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-a-07"
)

const tenantA = "tenant-a"

func ringLimits() impl.Limits {
	return impl.Limits{MaxKeysPerTenant: 8, MaxNestedDepth: 2, MaxPayloadBytes: 256, MaxInflight: 4}
}

type fakeDecrypter struct {
	mu       sync.Mutex
	calls    int
	payloads map[string][]byte
	failures map[string]error
}

func newFakeDecrypter() *fakeDecrypter {
	return &fakeDecrypter{payloads: map[string][]byte{}, failures: map[string]error{}}
}

func (f *fakeDecrypter) Unwrap(k impl.KeyEntry, ciphertext []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if err, ok := f.failures[string(ciphertext)]; ok {
		return nil, err
	}
	if p, ok := f.payloads[string(ciphertext)]; ok {
		return p, nil
	}
	return nil, errors.New("no key material for this ciphertext")
}

func (f *fakeDecrypter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func assertionPayload(t *testing.T, tenant, subject string, issuedAt int64) []byte {
	t.Helper()
	raw, err := json.Marshal(impl.Assertion{
		TenantID:   tenant,
		Subject:    subject,
		IssuedAt:   issuedAt,
		Attributes: []string{"role=admin", "dept=fin"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

func envelopeBytes(t *testing.T, env impl.Envelope) []byte {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

func generationKey(version, recipient, alg string) impl.KeyEntry {
	return impl.KeyEntry{
		Version:    impl.Version(version),
		Recipient:  recipient,
		Algorithm:  alg,
		PrivateKey: []byte("material-" + version),
	}
}

func TestAddKeyReturnsGeneration(t *testing.T) {
	kr := impl.NewEncryptedAssertionKeyring(newFakeDecrypter(), ringLimits())
	v, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256))
	if err != nil {
		t.Fatalf("first generation must be accepted, got %v", err)
	}
	if v != impl.Version("g1") {
		t.Fatalf("returned version = %q, want g1", string(v))
	}
	cur, err := kr.Current(tenantA)
	if err != nil {
		t.Fatalf("current generation: %v", err)
	}
	if cur != impl.Version("g1") {
		t.Fatalf("current generation = %q, want g1", string(cur))
	}
	keys, err := kr.Keys(tenantA)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("key count = %d, want 1", len(keys))
	}
	if keys[0].Version != impl.Version("g1") || keys[0].Recipient != "sp-a" || keys[0].Algorithm != impl.AlgRSAOAEP256 {
		t.Fatalf("stored generation mismatch: %+v", keys[0])
	}
	if keys[0].Retired {
		t.Fatalf("fresh generation must not be retired")
	}
	if got := kr.Count(tenantA); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

func TestAddKeyRejectsBlankFields(t *testing.T) {
	kr := impl.NewEncryptedAssertionKeyring(newFakeDecrypter(), ringLimits())
	if _, err := kr.AddKey("  ", generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); !errors.Is(err, impl.ErrBlankTenant) {
		t.Fatalf("blank tenant must be rejected with ErrBlankTenant, got %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("   ", "sp-a", impl.AlgRSAOAEP256)); !errors.Is(err, impl.ErrBlankVersion) {
		t.Fatalf("blank version must be rejected with ErrBlankVersion, got %v", err)
	}
	if got := kr.Count(tenantA); got != 0 {
		t.Fatalf("rejected keys must not be stored, got %d", got)
	}
}

func TestAddKeyRejectsDuplicateVersion(t *testing.T) {
	kr := impl.NewEncryptedAssertionKeyring(newFakeDecrypter(), ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-b", impl.AlgRSAOAEP1)); !errors.Is(err, impl.ErrDuplicateKey) {
		t.Fatalf("duplicate generation must be rejected with ErrDuplicateKey, got %v", err)
	}
	keys, err := kr.Keys(tenantA)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 1 || keys[0].Recipient != "sp-a" {
		t.Fatalf("duplicate add must not overwrite the stored generation: %+v", keys)
	}
}

func TestAddKeyEnforcesKeyLimit(t *testing.T) {
	kr := impl.NewEncryptedAssertionKeyring(newFakeDecrypter(), impl.Limits{
		MaxKeysPerTenant: 2,
		MaxNestedDepth:   2,
		MaxPayloadBytes:  256,
		MaxInflight:      4,
	})
	for _, version := range []string{"g1", "g2"} {
		if _, err := kr.AddKey(tenantA, generationKey(version, "sp-a", impl.AlgRSAOAEP256)); err != nil {
			t.Fatalf("add %s: %v", version, err)
		}
	}
	if _, err := kr.AddKey(tenantA, generationKey("g3", "sp-a", impl.AlgRSAOAEP256)); !errors.Is(err, impl.ErrKeyLimit) {
		t.Fatalf("third generation must hit the ring cap, got %v", err)
	}
	if got := kr.Count(tenantA); got != 2 {
		t.Fatalf("count = %d, want the configured cap 2", got)
	}
}

func TestAddKeyRejectsForbiddenAlgorithm(t *testing.T) {
	kr := impl.NewEncryptedAssertionKeyring(newFakeDecrypter(), ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSA15)); !errors.Is(err, impl.ErrAlgorithmForbidden) {
		t.Fatalf("rsa1_5 must be rejected with ErrAlgorithmForbidden, got %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("g2", "sp-a", "rsa-oaep-md5")); !errors.Is(err, impl.ErrAlgorithmNotAllowed) {
		t.Fatalf("algorithm outside the allow list must be rejected with ErrAlgorithmNotAllowed, got %v", err)
	}
	if err := kr.SetAlgorithmAllowlist([]string{impl.AlgRSAOAEP256, "rsa-oaep-sha1"}); err != nil {
		t.Fatalf("allow list update: %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("g3", "sp-a", "rsa-oaep-sha1")); err != nil {
		t.Fatalf("newly allowed algorithm must be accepted, got %v", err)
	}
	if err := kr.SetAlgorithmAllowlist([]string{impl.AlgRSA15}); err != nil {
		t.Fatalf("allow list update: %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("g4", "sp-a", impl.AlgRSA15)); !errors.Is(err, impl.ErrAlgorithmForbidden) {
		t.Fatalf("rsa1_5 must stay forbidden even when allow listed, got %v", err)
	}
}

func TestDecryptResponseReturnsAssertion(t *testing.T) {
	dec := newFakeDecrypter()
	dec.payloads["ct-happy"] = assertionPayload(t, tenantA, "alice", 1700)
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	raw := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		Depth:      0,
		Ciphertext: []byte("ct-happy"),
	})
	got, err := kr.DecryptResponse(tenantA, raw)
	if err != nil {
		t.Fatalf("decrypt must succeed, got %v", err)
	}
	if got.Subject != "alice" || got.IssuedAt != 1700 {
		t.Fatalf("assertion subject/time = %q/%d, want alice/1700", got.Subject, got.IssuedAt)
	}
	if got.TenantID != tenantA {
		t.Fatalf("assertion tenant = %q, want %q", got.TenantID, tenantA)
	}
	if got.Recipient != "sp-a" {
		t.Fatalf("assertion recipient = %q, want sp-a", got.Recipient)
	}
	if got.KeyVersion != impl.Version("g1") {
		t.Fatalf("assertion key version = %q, want g1", string(got.KeyVersion))
	}
	if len(got.Attributes) != 2 || got.Attributes[0] != "role=admin" {
		t.Fatalf("attributes not preserved: %+v", got.Attributes)
	}
	if calls := dec.callCount(); calls != 1 {
		t.Fatalf("unwrap calls = %d, want 1", calls)
	}
}

func TestDecryptResponseRejectsMalformedEnvelope(t *testing.T) {
	dec := newFakeDecrypter()
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if _, err := kr.DecryptResponse(tenantA, []byte("{not json")); !errors.Is(err, impl.ErrBadEnvelope) {
		t.Fatalf("malformed body must be rejected with ErrBadEnvelope, got %v", err)
	}
	if _, err := kr.DecryptResponse(tenantA, nil); !errors.Is(err, impl.ErrBadEnvelope) {
		t.Fatalf("empty body must be rejected with ErrBadEnvelope, got %v", err)
	}
	if calls := dec.callCount(); calls != 0 {
		t.Fatalf("malformed bodies must not reach the decrypter, got %d calls", calls)
	}
}

func TestDecryptResponseRejectsUnsupportedType(t *testing.T) {
	dec := newFakeDecrypter()
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	for _, dataType := range []string{impl.TypeResponse, "", "attribute"} {
		raw := envelopeBytes(t, impl.Envelope{
			Type:       dataType,
			Recipient:  "sp-a",
			Algorithm:  impl.AlgRSAOAEP256,
			Ciphertext: []byte("ct-type"),
		})
		if _, err := kr.DecryptResponse(tenantA, raw); !errors.Is(err, impl.ErrUnsupportedType) {
			t.Fatalf("type %q must be rejected with ErrUnsupportedType, got %v", dataType, err)
		}
	}
	if calls := dec.callCount(); calls != 0 {
		t.Fatalf("unsupported types must not reach the decrypter, got %d calls", calls)
	}
}

func TestDecryptResponseRejectsNestedDepth(t *testing.T) {
	kr := impl.NewEncryptedAssertionKeyring(newFakeDecrypter(), ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	tooDeep := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		Depth:      3,
		Ciphertext: []byte("ct-deep"),
	})
	if _, err := kr.DecryptResponse(tenantA, tooDeep); !errors.Is(err, impl.ErrNestedTooDeep) {
		t.Fatalf("depth above the cap must be rejected with ErrNestedTooDeep, got %v", err)
	}
	negative := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		Depth:      -1,
		Ciphertext: []byte("ct-negative"),
	})
	if _, err := kr.DecryptResponse(tenantA, negative); !errors.Is(err, impl.ErrBadEnvelope) {
		t.Fatalf("negative depth must be rejected with ErrBadEnvelope, got %v", err)
	}
}

func TestDecryptResponseRejectsRetiredKey(t *testing.T) {
	dec := newFakeDecrypter()
	dec.payloads["ct-old"] = assertionPayload(t, tenantA, "bob", 1800)
	dec.payloads["ct-new"] = assertionPayload(t, tenantA, "bob", 1801)
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add g1: %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("g2", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add g2: %v", err)
	}
	if err := kr.Retire(tenantA, "g1"); err != nil {
		t.Fatalf("retire g1: %v", err)
	}
	if err := kr.Retire(tenantA, "g1"); err != nil {
		t.Fatalf("repeated retire must be idempotent, got %v", err)
	}
	cur, err := kr.Current(tenantA)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur != impl.Version("g2") {
		t.Fatalf("current generation after retire = %q, want g2", string(cur))
	}
	keys, err := kr.Keys(tenantA)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("key count = %d, want 2", len(keys))
	}
	if keys[0].Version != impl.Version("g1") || !keys[0].Retired {
		t.Fatalf("retirement tombstone missing on the oldest generation: %+v", keys[0])
	}
	if keys[1].Version != impl.Version("g2") || keys[1].Retired {
		t.Fatalf("newest generation must stay usable: %+v", keys[1])
	}
	oldEnvelope := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		KeyVersion: "g1",
		Ciphertext: []byte("ct-old"),
	})
	if _, err := kr.DecryptResponse(tenantA, oldEnvelope); !errors.Is(err, impl.ErrKeyRetired) {
		t.Fatalf("retired generation must reject new responses with ErrKeyRetired, got %v", err)
	}
	newEnvelope := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		KeyVersion: "g2",
		Ciphertext: []byte("ct-new"),
	})
	if _, err := kr.DecryptResponse(tenantA, newEnvelope); err != nil {
		t.Fatalf("current generation must still decrypt, got %v", err)
	}
}

func TestDecryptResponseCollapsesDecryptFailures(t *testing.T) {
	dec := newFakeDecrypter()
	dec.failures["ct-padding"] = errors.New("bad padding in block")
	dec.failures["ct-mismatch"] = errors.New("key does not match recipient")
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	for _, ct := range []string{"ct-padding", "ct-mismatch"} {
		raw := envelopeBytes(t, impl.Envelope{
			Type:       impl.TypeAssertion,
			Recipient:  "sp-a",
			Algorithm:  impl.AlgRSAOAEP256,
			Ciphertext: []byte(ct),
		})
		_, err := kr.DecryptResponse(tenantA, raw)
		if !errors.Is(err, impl.ErrDecryptFailed) {
			t.Fatalf("unwrap failure for %s must surface as ErrDecryptFailed, got %v", ct, err)
		}
		if strings.Contains(err.Error(), "padding") || strings.Contains(err.Error(), "recipient") {
			t.Fatalf("failure cause leaked to the caller: %v", err)
		}
	}
}

func TestDecryptResponseRejectsOversizedPayload(t *testing.T) {
	dec := newFakeDecrypter()
	dec.payloads["ct-big"] = []byte(strings.Repeat("x", 512))
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	raw := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		Ciphertext: []byte("ct-big"),
	})
	if _, err := kr.DecryptResponse(tenantA, raw); !errors.Is(err, impl.ErrPayloadTooLarge) {
		t.Fatalf("oversized decrypted payload must be rejected with ErrPayloadTooLarge, got %v", err)
	}
}

func TestDecryptResponseRejectsUnknownKeyVersion(t *testing.T) {
	dec := newFakeDecrypter()
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	raw := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		KeyVersion: "g9",
		Ciphertext: []byte("ct-unknown"),
	})
	if _, err := kr.DecryptResponse(tenantA, raw); !errors.Is(err, impl.ErrUnknownKey) {
		t.Fatalf("unknown generation must be rejected with ErrUnknownKey, got %v", err)
	}
	if calls := dec.callCount(); calls != 0 {
		t.Fatalf("unknown generation must not reach the decrypter, got %d calls", calls)
	}
}

func TestConcurrentAddKeyKeepsRingConsistent(t *testing.T) {
	dec := newFakeDecrypter()
	kr := impl.NewEncryptedAssertionKeyring(dec, impl.Limits{
		MaxKeysPerTenant: 16,
		MaxNestedDepth:   2,
		MaxPayloadBytes:  256,
		MaxInflight:      4,
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			version := impl.Version("c" + string(rune('a'+n)))
			_, _ = kr.AddKey(tenantA, generationKey(string(version), "sp-a", impl.AlgRSAOAEP256))
		}(i)
	}
	var mu sync.Mutex
	badView := false
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys, err := kr.Keys(tenantA)
			if err != nil {
				return
			}
			for _, k := range keys {
				if k.Version == "" {
					mu.Lock()
					badView = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if badView {
		t.Fatalf("a concurrent reader observed a half written generation")
	}
	keys, err := kr.Keys(tenantA)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 8 {
		t.Fatalf("generation count = %d, want 8", len(keys))
	}
	seen := map[impl.Version]bool{}
	for _, k := range keys {
		if seen[k.Version] {
			t.Fatalf("generation %q listed twice", string(k.Version))
		}
		seen[k.Version] = true
	}
	if got := kr.Count(tenantA); got != 8 {
		t.Fatalf("count = %d, want 8", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	dec := newFakeDecrypter()
	dec.payloads["ct-snap"] = assertionPayload(t, tenantA, "carol", 1900)
	kr := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if _, err := kr.AddKey(tenantA, generationKey("g1", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add g1: %v", err)
	}
	if _, err := kr.AddKey(tenantA, generationKey("g2", "sp-a", impl.AlgRSAOAEP256)); err != nil {
		t.Fatalf("add g2: %v", err)
	}
	if err := kr.Retire(tenantA, "g1"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	snap, err := kr.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewEncryptedAssertionKeyring(dec, ringLimits())
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("restore of a fresh snapshot must succeed, got %v", err)
	}
	if got := restored.Count(tenantA); got != 2 {
		t.Fatalf("restored generation count = %d, want 2", got)
	}
	keys, err := restored.Keys(tenantA)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 2 || keys[0].Version != impl.Version("g1") || !keys[0].Retired {
		t.Fatalf("restored tombstones are wrong: %+v", keys)
	}
	cur, err := restored.Current(tenantA)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur != impl.Version("g2") {
		t.Fatalf("restored current generation = %q, want g2", string(cur))
	}
	raw := envelopeBytes(t, impl.Envelope{
		Type:       impl.TypeAssertion,
		Recipient:  "sp-a",
		Algorithm:  impl.AlgRSAOAEP256,
		Ciphertext: []byte("ct-snap"),
	})
	if _, err := restored.DecryptResponse(tenantA, raw); err != nil {
		t.Fatalf("restored ring must still decrypt, got %v", err)
	}
	if err := restored.Restore([]byte("|broken|")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must be rejected with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Count(tenantA); got != 2 {
		t.Fatalf("rejected restore must keep the live ring, got %d generations", got)
	}
}
