// Package task selects a decryption key generation for encrypted SAML
// assertions, enforces the transport algorithm allow list and keeps the key
// ring generations consistent across restarts.
package task

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// Key transport algorithms understood by the key ring.
const (
	AlgRSAOAEP256 = "rsa-oaep-256"
	AlgRSAOAEP1   = "rsa-oaep-1"
	AlgRSA15      = "rsa1_5"
)

// Encrypted data types accepted from a response.
const (
	TypeAssertion = "assertion"
	TypeResponse  = "response"
)

// Errors returned by the key ring.
var (
	ErrBlankTenant         = errors.New("tenant id is required")
	ErrBlankVersion        = errors.New("key version is required")
	ErrKeyLimit            = errors.New("key ring limit reached for this tenant")
	ErrDuplicateKey        = errors.New("key version already exists for this tenant")
	ErrAlgorithmForbidden  = errors.New("key transport algorithm is forbidden")
	ErrAlgorithmNotAllowed = errors.New("key transport algorithm is not in the allow list")
	ErrUnknownKey          = errors.New("unknown key version for this tenant")
	ErrKeyRetired          = errors.New("key version is retired")
	ErrRecipientMismatch   = errors.New("assertion recipient does not match the key recipient")
	ErrBadEnvelope         = errors.New("encrypted assertion envelope is invalid")
	ErrUnsupportedType     = errors.New("encrypted data type is not supported")
	ErrNestedTooDeep       = errors.New("nested encryption exceeds the depth limit")
	ErrDecryptFailed       = errors.New("assertion could not be decrypted")
	ErrPayloadTooLarge     = errors.New("decrypted assertion exceeds the size limit")
	ErrTenantMismatch      = errors.New("assertion tenant does not match the key ring tenant")
	ErrBadPayload          = errors.New("decrypted assertion payload is invalid")
	ErrBadSnapshot         = errors.New("snapshot payload is invalid")
)

const snapshotFormatVersion = 1

// Version labels one key generation of a tenant.
type Version string

// KeyEntry is one private key generation held for a tenant.
type KeyEntry struct {
	Version    Version `json:"version"`
	Recipient  string  `json:"recipient"`
	Algorithm  string  `json:"algorithm"`
	PrivateKey []byte  `json:"private_key"`
	Retired    bool    `json:"retired"`
}

// Limits bounds the key ring and one unwrap.
type Limits struct {
	MaxKeysPerTenant int
	MaxNestedDepth   int
	MaxPayloadBytes  int
	MaxInflight      int
}

// Decrypter unwraps the ciphertext of one envelope with the given key. It is
// provided by the HSM or KMS that holds the private key material.
type Decrypter interface {
	Unwrap(k KeyEntry, ciphertext []byte) ([]byte, error)
}

// Envelope is the JSON body posted by the identity provider.
type Envelope struct {
	Type       string `json:"type"`
	Recipient  string `json:"recipient"`
	Algorithm  string `json:"algorithm"`
	KeyVersion string `json:"key_version"`
	Depth      int    `json:"depth"`
	Ciphertext []byte `json:"ciphertext"`
}

// Assertion is the decrypted and validated assertion.
type Assertion struct {
	TenantID   string   `json:"tenant_id"`
	Recipient  string   `json:"recipient"`
	Subject    string   `json:"subject"`
	IssuedAt   int64    `json:"issued_at"`
	Attributes []string `json:"attributes"`
	KeyVersion Version  `json:"key_version"`
}

type tenantRing struct {
	entries []KeyEntry
}

type snapshotData struct {
	FormatVersion int                   `json:"format_version"`
	Allowlist     []string              `json:"algorithm_allowlist"`
	Tenants       map[string][]KeyEntry `json:"tenants"`
}

// EncryptedAssertionKeyring owns the per tenant generations, the retirement
// tombstones and the algorithm allow list.
type EncryptedAssertionKeyring struct {
	mu        sync.RWMutex
	dec       Decrypter
	limits    Limits
	tenants   map[string]*tenantRing
	allowList []string
	allow     map[string]struct{}
	inflight  chan struct{}
}

// NewEncryptedAssertionKeyring builds a key ring on top of a decrypter. The
// default transport allow list holds rsa-oaep-256 and rsa-oaep-1.
func NewEncryptedAssertionKeyring(dec Decrypter, limits Limits) *EncryptedAssertionKeyring {
	k := &EncryptedAssertionKeyring{
		dec:      dec,
		limits:   limits,
		tenants:  make(map[string]*tenantRing),
		inflight: make(chan struct{}, maxInt(1, limits.MaxInflight)),
	}
	_ = k.SetAlgorithmAllowlist([]string{AlgRSAOAEP256, AlgRSAOAEP1})
	return k
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func cloneKey(entry KeyEntry) KeyEntry {
	clone := entry
	if entry.PrivateKey != nil {
		clone.PrivateKey = append([]byte(nil), entry.PrivateKey...)
	}
	return clone
}

func cloneKeys(entries []KeyEntry) []KeyEntry {
	cloned := make([]KeyEntry, len(entries))
	for i := range entries {
		cloned[i] = cloneKey(entries[i])
	}
	return cloned
}

// AddKey installs one generation for a tenant and returns its version label.
func (k *EncryptedAssertionKeyring) AddKey(tenantID string, entry KeyEntry) (Version, error) {
	if isBlank(tenantID) {
		return "", ErrBlankTenant
	}
	if isBlank(string(entry.Version)) {
		return "", ErrBlankVersion
	}
	if err := k.checkAlgorithm(entry.Algorithm); err != nil {
		return "", err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	ring := k.tenants[tenantID]
	if ring == nil {
		ring = &tenantRing{}
	}
	for _, existing := range ring.entries {
		if existing.Version == entry.Version {
			return "", ErrDuplicateKey
		}
	}
	if len(ring.entries) >= k.limits.MaxKeysPerTenant {
		return "", ErrKeyLimit
	}

	stored := cloneKey(entry)
	stored.Retired = false
	next := make([]KeyEntry, len(ring.entries)+1)
	copy(next, ring.entries)
	next[len(ring.entries)] = stored
	ring.entries = next

	if k.tenants == nil {
		k.tenants = make(map[string]*tenantRing)
	}
	k.tenants[tenantID] = ring
	return stored.Version, nil
}

// Retire marks a generation as retired so it stops accepting new responses.
func (k *EncryptedAssertionKeyring) Retire(tenantID, version string) error {
	if isBlank(tenantID) {
		return ErrBlankTenant
	}
	if isBlank(version) {
		return ErrBlankVersion
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	ring := k.tenants[tenantID]
	if ring == nil {
		return ErrUnknownKey
	}
	index := k.findIndexLocked(ring, Version(version))
	if index < 0 {
		return ErrUnknownKey
	}
	if ring.entries[index].Retired {
		return nil
	}

	next := cloneKeys(ring.entries)
	next[index].Retired = true
	ring.entries = next
	return nil
}

func (k *EncryptedAssertionKeyring) findIndexLocked(ring *tenantRing, version Version) int {
	for i := range ring.entries {
		if ring.entries[i].Version == version {
			return i
		}
	}
	return -1
}

// Keys lists the generations of one tenant, oldest first, retired ones included.
func (k *EncryptedAssertionKeyring) Keys(tenantID string) ([]KeyEntry, error) {
	if isBlank(tenantID) {
		return nil, ErrBlankTenant
	}

	k.mu.RLock()
	defer k.mu.RUnlock()

	ring := k.tenants[tenantID]
	if ring == nil {
		return []KeyEntry{}, nil
	}
	return cloneKeys(ring.entries), nil
}

// Current reports the newest generation that still accepts new responses.
func (k *EncryptedAssertionKeyring) Current(tenantID string) (Version, error) {
	if isBlank(tenantID) {
		return "", ErrBlankTenant
	}

	k.mu.RLock()
	defer k.mu.RUnlock()

	ring := k.tenants[tenantID]
	if ring == nil || len(ring.entries) == 0 {
		return "", ErrUnknownKey
	}
	for i := len(ring.entries) - 1; i >= 0; i-- {
		if !ring.entries[i].Retired {
			return ring.entries[i].Version, nil
		}
	}
	return "", ErrKeyRetired
}

// DecryptResponse parses one response envelope, picks the generation it needs
// and returns the decrypted assertion.
func (k *EncryptedAssertionKeyring) DecryptResponse(tenantID string, raw []byte) (Assertion, error) {
	if isBlank(tenantID) {
		return Assertion{}, ErrBlankTenant
	}

	envelope, err := parseEnvelope(raw)
	if err != nil {
		return Assertion{}, err
	}
	if err := k.validateEnvelope(envelope); err != nil {
		return Assertion{}, err
	}
	candidates, err := k.candidateKeys(tenantID, envelope)
	if err != nil {
		return Assertion{}, err
	}

	if k.inflight != nil {
		select {
		case k.inflight <- struct{}{}:
			defer func() { <-k.inflight }()
		default:
			return Assertion{}, ErrDecryptFailed
		}
	}

	assertion, _, err := k.decryptCandidates(tenantID, envelope, candidates, 0)
	return assertion, err
}

func parseEnvelope(raw []byte) (Envelope, error) {
	var decoded *Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return Envelope{}, ErrBadEnvelope
	}
	envelope := *decoded
	envelope.KeyVersion = strings.TrimSpace(envelope.KeyVersion)
	return envelope, nil
}

func (k *EncryptedAssertionKeyring) validateEnvelope(envelope Envelope) error {
	if envelope.Depth < 0 {
		return ErrBadEnvelope
	}
	if envelope.Type != TypeAssertion {
		return ErrUnsupportedType
	}
	if envelope.Depth > k.limits.MaxNestedDepth {
		return ErrNestedTooDeep
	}
	return k.checkAlgorithm(envelope.Algorithm)
}

func (k *EncryptedAssertionKeyring) checkAlgorithm(algorithm string) error {
	if algorithm == AlgRSA15 {
		return ErrAlgorithmForbidden
	}
	if isBlank(algorithm) {
		return ErrAlgorithmNotAllowed
	}

	k.mu.RLock()
	_, allowed := k.allow[algorithm]
	k.mu.RUnlock()
	if !allowed {
		return ErrAlgorithmNotAllowed
	}
	return nil
}

func (k *EncryptedAssertionKeyring) candidateKeys(tenantID string, envelope Envelope) ([]KeyEntry, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()

	ring := k.tenants[tenantID]
	if ring == nil || len(ring.entries) == 0 {
		return nil, ErrUnknownKey
	}

	if envelope.KeyVersion != "" {
		version := Version(envelope.KeyVersion)
		index := k.findIndexLocked(ring, version)
		if index < 0 {
			return nil, ErrUnknownKey
		}
		key := ring.entries[index]
		if key.Retired {
			return nil, ErrKeyRetired
		}
		if envelope.Recipient != key.Recipient {
			return nil, ErrRecipientMismatch
		}
		return []KeyEntry{cloneKey(key)}, nil
	}

	var candidates []KeyEntry
	for _, key := range ring.entries {
		if key.Retired || key.Recipient != envelope.Recipient {
			continue
		}
		candidates = append(candidates, cloneKey(key))
	}
	if len(candidates) == 0 {
		for _, key := range ring.entries {
			if !key.Retired {
				return nil, ErrRecipientMismatch
			}
		}
		return nil, ErrKeyRetired
	}
	return candidates, nil
}

func (k *EncryptedAssertionKeyring) decryptCandidates(
	tenantID string,
	envelope Envelope,
	candidates []KeyEntry,
	depth int,
) (Assertion, KeyEntry, error) {
	if envelope.KeyVersion != "" || len(candidates) == 1 {
		return k.decryptWithKey(tenantID, envelope, candidates[0], depth)
	}

	var best Assertion
	var bestKey KeyEntry
	bestIndex := -1
	var firstErr error

	for index, key := range candidates {
		assertion, usedKey, err := k.decryptWithKey(tenantID, envelope, key, depth)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if bestIndex == -1 ||
			assertion.IssuedAt > best.IssuedAt ||
			(assertion.IssuedAt == best.IssuedAt && index > bestIndex) {
			best = assertion
			bestKey = usedKey
			bestIndex = index
		}
	}

	if bestIndex >= 0 {
		return best, bestKey, nil
	}
	if firstErr != nil {
		return Assertion{}, KeyEntry{}, firstErr
	}
	return Assertion{}, KeyEntry{}, ErrDecryptFailed
}

func (k *EncryptedAssertionKeyring) decryptWithKey(
	tenantID string,
	envelope Envelope,
	key KeyEntry,
	depth int,
) (Assertion, KeyEntry, error) {
	if k.dec == nil {
		return Assertion{}, KeyEntry{}, ErrDecryptFailed
	}

	unwrapKey := cloneKey(key)
	unwrapKey.Algorithm = envelope.Algorithm
	plaintext, err := k.dec.Unwrap(unwrapKey, envelope.Ciphertext)
	if err != nil {
		return Assertion{}, KeyEntry{}, ErrDecryptFailed
	}
	if len(plaintext) > k.limits.MaxPayloadBytes {
		return Assertion{}, KeyEntry{}, ErrPayloadTooLarge
	}

	var probe struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	_ = json.Unmarshal(plaintext, &probe)
	if len(probe.Ciphertext) > 0 {
		if depth >= k.limits.MaxNestedDepth {
			return Assertion{}, KeyEntry{}, ErrNestedTooDeep
		}
		return k.decryptNested(tenantID, plaintext, depth+1)
	}

	var assertion Assertion
	if err := json.Unmarshal(plaintext, &assertion); err != nil {
		return Assertion{}, KeyEntry{}, ErrBadPayload
	}
	if assertion.TenantID != tenantID {
		return Assertion{}, KeyEntry{}, ErrTenantMismatch
	}
	assertion.Recipient = key.Recipient
	assertion.KeyVersion = key.Version
	return assertion, key, nil
}

func (k *EncryptedAssertionKeyring) decryptNested(
	tenantID string,
	raw []byte,
	depth int,
) (Assertion, KeyEntry, error) {
	envelope, err := parseEnvelope(raw)
	if err != nil {
		return Assertion{}, KeyEntry{}, err
	}
	if err := k.validateEnvelope(envelope); err != nil {
		return Assertion{}, KeyEntry{}, err
	}
	candidates, err := k.candidateKeys(tenantID, envelope)
	if err != nil {
		return Assertion{}, KeyEntry{}, err
	}
	return k.decryptCandidates(tenantID, envelope, candidates, depth)
}

// SetAlgorithmAllowlist replaces the accepted key transport algorithms.
func (k *EncryptedAssertionKeyring) SetAlgorithmAllowlist(algorithms []string) error {
	list := make([]string, len(algorithms))
	copy(list, algorithms)
	allowed := make(map[string]struct{}, len(list))
	for _, algorithm := range list {
		if !isBlank(algorithm) {
			allowed[algorithm] = struct{}{}
		}
	}

	k.mu.Lock()
	k.allowList = list
	k.allow = allowed
	k.mu.Unlock()
	return nil
}

// Purge drops every generation of one tenant.
func (k *EncryptedAssertionKeyring) Purge(tenantID string) error {
	if isBlank(tenantID) {
		return ErrBlankTenant
	}

	k.mu.Lock()
	if k.tenants != nil {
		delete(k.tenants, tenantID)
	}
	k.mu.Unlock()
	return nil
}

// Count reports how many generations a tenant holds, retired ones included.
func (k *EncryptedAssertionKeyring) Count(tenantID string) int {
	k.mu.RLock()
	defer k.mu.RUnlock()

	ring := k.tenants[tenantID]
	if ring == nil {
		return 0
	}
	return len(ring.entries)
}

// Snapshot serialises every generation, the tombstones and the allow list.
func (k *EncryptedAssertionKeyring) Snapshot() ([]byte, error) {
	k.mu.RLock()
	allowlist := make([]string, len(k.allowList))
	copy(allowlist, k.allowList)
	state := snapshotData{
		FormatVersion: snapshotFormatVersion,
		Allowlist:     allowlist,
		Tenants:       make(map[string][]KeyEntry, len(k.tenants)),
	}
	for tenantID, ring := range k.tenants {
		state.Tenants[tenantID] = cloneKeys(ring.entries)
	}
	k.mu.RUnlock()

	raw, err := json.Marshal(state)
	if err != nil {
		return nil, ErrBadSnapshot
	}
	return raw, nil
}

// Restore replaces the whole key ring with a snapshot, all or nothing.
func (k *EncryptedAssertionKeyring) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}

	var state snapshotData
	if err := json.Unmarshal(data, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.FormatVersion != snapshotFormatVersion || state.Allowlist == nil {
		return ErrBadSnapshot
	}

	tenants := make(map[string]*tenantRing, len(state.Tenants))
	for tenantID, entries := range state.Tenants {
		if isBlank(tenantID) {
			return ErrBadSnapshot
		}
		ring := &tenantRing{entries: make([]KeyEntry, 0, len(entries))}
		seen := make(map[Version]struct{}, len(entries))
		for _, entry := range entries {
			if isBlank(string(entry.Version)) {
				return ErrBadSnapshot
			}
			if _, duplicate := seen[entry.Version]; duplicate {
				return ErrBadSnapshot
			}
			seen[entry.Version] = struct{}{}
			ring.entries = append(ring.entries, cloneKey(entry))
		}
		tenants[tenantID] = ring
	}

	allowed := make(map[string]struct{}, len(state.Allowlist))
	for _, algorithm := range state.Allowlist {
		if !isBlank(algorithm) {
			allowed[algorithm] = struct{}{}
		}
	}

	k.mu.Lock()
	k.tenants = tenants
	allowlist := make([]string, len(state.Allowlist))
	copy(allowlist, state.Allowlist)
	k.allowList = allowlist
	k.allow = allowed
	k.mu.Unlock()
	return nil
}
