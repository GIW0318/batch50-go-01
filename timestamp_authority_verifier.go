// Package task verifies RFC 3161 style timestamp tokens against a trust store:
// it binds the token to a message imprint and a nonce, and it re-validates the
// timestamping certificate at the moment the token was signed instead of at the
// moment the check runs.
package task

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verdict reasons.
const (
	ReasonOK         = "ok"
	ReasonImprint    = "imprint"
	ReasonCertWindow = "cert-window"
	ReasonRevoked    = "revoked"
	ReasonUntrusted  = "untrusted"
	ReasonPolicy     = "policy"
	ReasonMalformed  = "malformed"
)

// Errors reported by the verifier.
var (
	ErrBadToken        = errors.New("timestamp token is malformed")
	ErrNonceMissing    = errors.New("token carries no nonce")
	ErrNonceMismatch   = errors.New("token nonce does not match the request")
	ErrImprintMismatch = errors.New("message imprint is invalid")
	ErrBadTrust        = errors.New("trust anchor is invalid")
	ErrBadRevocation   = errors.New("revocation entry is invalid")
	ErrTooManyTokens   = errors.New("batch exceeds the token cap")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

const snapshotVersion = 1

// Imprint is the message digest the token must cover, plus the nonce the
// requester sent with it.
type Imprint struct {
	Algorithm string
	Digest    string
	Nonce     string
}

// Cert is one certificate of a timestamping chain.
type Cert struct {
	Subject   string
	Issuer    string
	Serial    string
	NotBefore time.Time
	NotAfter  time.Time
	EKUs      []string
}

// Verdict is the outcome of checking one token.
type Verdict struct {
	TokenID string
	TSA     string
	Valid   bool
	Reason  string
	GenTime time.Time
	Cached  bool
	Skew    time.Duration
}

// Stats counts the verification work done so far.
type Stats struct {
	Verified int
	Rejected int
	Cached   int
	Trusted  int
}

// Limits bounds the batch size, the verdict cache and supplies the local clock.
type Limits struct {
	MaxBatch  int
	CacheSize int
	Now       time.Time
}

type tokenDocument struct {
	TSA     string `json:"tsa"`
	Serial  string `json:"serial"`
	GenTime string `json:"genTime"`
	Digest  string `json:"digest"`
	Nonce   string `json:"nonce"`
	Policy  string `json:"policy"`
}

type cacheEntry struct {
	key     string
	verdict Verdict
}

type snapshotDocument struct {
	Version    int                  `json:"version"`
	Trust      map[string][]Cert    `json:"trust"`
	Policies   []string             `json:"policies"`
	Revoked    map[string]time.Time `json:"revoked"`
	Cache      map[string]Verdict   `json:"cache"`
	CacheOrder []string             `json:"cacheOrder"`
	Verdicts   map[string]Verdict   `json:"verdicts"`
	Stats      Stats                `json:"stats"`
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

// TimestampAuthorityVerifier owns the trust store, the revocation list and the
// verdict cache.
type TimestampAuthorityVerifier struct {
	mu       sync.Mutex
	limits   Limits
	trust    map[string][]Cert
	policies map[string]struct{}
	revoked  map[string]time.Time
	cache    map[string]*list.Element
	lru      *list.List
	verdicts map[string]Verdict
	stats    Stats
}

// NewTimestampAuthorityVerifier builds a verifier with the given limits.
func NewTimestampAuthorityVerifier(limits Limits) *TimestampAuthorityVerifier {
	if limits.MaxBatch < 0 {
		limits.MaxBatch = 0
	}
	if limits.CacheSize < 0 {
		limits.CacheSize = 0
	}
	return &TimestampAuthorityVerifier{
		limits:   limits,
		trust:    make(map[string][]Cert),
		policies: make(map[string]struct{}),
		revoked:  make(map[string]time.Time),
		cache:    make(map[string]*list.Element),
		lru:      list.New(),
		verdicts: make(map[string]Verdict),
	}
}

// Trust registers a timestamping authority and its certificate chain.
func (v *TimestampAuthorityVerifier) Trust(tsa string, chain []Cert) error {
	if err := validateTrustAnchor(tsa, chain); err != nil {
		return err
	}

	copied := cloneChain(chain)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.trust == nil {
		v.trust = make(map[string][]Cert)
	}
	v.trust[tsa] = copied
	v.stats.Trusted = len(v.trust)
	return nil
}

// AllowPolicy registers a timestamp policy identifier as acceptable.
func (v *TimestampAuthorityVerifier) AllowPolicy(policyID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.policies == nil {
		v.policies = make(map[string]struct{})
	}
	v.policies[policyID] = struct{}{}
	return nil
}

// Revoke marks a certificate serial as revoked from the given moment.
func (v *TimestampAuthorityVerifier) Revoke(serial string, at time.Time) error {
	if strings.TrimSpace(serial) == "" {
		return fmt.Errorf("%w: serial must not be empty", ErrBadRevocation)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.revoked == nil {
		v.revoked = make(map[string]time.Time)
	}
	v.revoked[serial] = at
	return nil
}

// Verify checks one token against the imprint.
func (v *TimestampAuthorityVerifier) Verify(token []byte, imprint Imprint) (Verdict, error) {
	doc, genTime, tokenID, err := parseToken(token)
	if err != nil {
		return Verdict{}, err
	}

	normalizedImprint, err := normalizeImprint(imprint)
	if err != nil {
		return Verdict{}, err
	}
	if doc.Nonce == "" {
		return Verdict{}, ErrNonceMissing
	}
	if doc.Nonce != normalizedImprint.Nonce {
		return Verdict{}, ErrNonceMismatch
	}

	cacheKey := makeCacheKey(tokenID, normalizedImprint)
	v.mu.Lock()
	defer v.mu.Unlock()

	if cached, ok := v.cacheGetLocked(cacheKey); ok {
		v.stats.Cached++
		cached.Cached = true
		return cached, nil
	}

	verdict := v.evaluateLocked(doc, genTime, tokenID, normalizedImprint)
	stored := verdict
	stored.Cached = false
	v.verdicts[tokenID] = stored
	v.cachePutLocked(cacheKey, stored)
	if verdict.Valid {
		v.stats.Verified++
	} else {
		v.stats.Rejected++
	}
	return verdict, nil
}

// Batch checks a list of tokens and keeps their order.
func (v *TimestampAuthorityVerifier) Batch(tokens [][]byte, imprint Imprint) ([]Verdict, error) {
	if len(tokens) > v.limits.MaxBatch {
		return nil, fmt.Errorf("%w: %d tokens exceed %d", ErrTooManyTokens, len(tokens), v.limits.MaxBatch)
	}
	if _, err := normalizeImprint(imprint); err != nil {
		return nil, err
	}

	verdicts := make([]Verdict, len(tokens))
	for i, token := range tokens {
		verdict, err := v.Verify(token, imprint)
		if err == nil {
			verdicts[i] = verdict
			continue
		}

		switch {
		case errors.Is(err, ErrBadToken):
			verdicts[i] = v.recordMalformed(token)
		default:
			return verdicts, err
		}
	}
	return verdicts, nil
}

// Stats returns the counters.
func (v *TimestampAuthorityVerifier) Stats() Stats {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.statsLocked()
}

// Trusted lists the registered authority names in ascending order.
func (v *TimestampAuthorityVerifier) Trusted() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	names := make([]string, 0, len(v.trust))
	for name := range v.trust {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// VerdictFor reads the stored verdict of one token.
func (v *TimestampAuthorityVerifier) VerdictFor(tokenID string) (Verdict, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	verdict, ok := v.verdicts[tokenID]
	return verdict, ok
}

// Snapshot serialises the trust store, revocations and the verdict cache.
func (v *TimestampAuthorityVerifier) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	doc := snapshotDocument{
		Version:    snapshotVersion,
		Trust:      make(map[string][]Cert, len(v.trust)),
		Policies:   make([]string, 0, len(v.policies)),
		Revoked:    make(map[string]time.Time, len(v.revoked)),
		Cache:      make(map[string]Verdict, v.lru.Len()),
		CacheOrder: make([]string, 0, v.lru.Len()),
		Verdicts:   make(map[string]Verdict, len(v.verdicts)),
		Stats:      v.statsLocked(),
	}

	for tsa, chain := range v.trust {
		doc.Trust[tsa] = cloneChain(chain)
	}
	for policy := range v.policies {
		doc.Policies = append(doc.Policies, policy)
	}
	sort.Strings(doc.Policies)
	for serial, at := range v.revoked {
		doc.Revoked[serial] = at
	}
	for _, element := range v.lruBackToFront() {
		entry := element.Value.(*cacheEntry)
		verdict := entry.verdict
		verdict.Cached = false
		doc.Cache[entry.key] = verdict
		doc.CacheOrder = append(doc.CacheOrder, entry.key)
	}
	for id, verdict := range v.verdicts {
		verdict.Cached = false
		doc.Verdicts[id] = verdict
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Payload:  payload,
		Checksum: hex.EncodeToString(digest[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the verifier state with a snapshot, all or nothing.
func (v *TimestampAuthorityVerifier) Restore(data []byte) error {
	doc, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	cache := make(map[string]*list.Element, len(doc.Cache))
	lru := list.New()
	for _, key := range doc.CacheOrder {
		verdict := doc.Cache[key]
		verdict.Cached = false
		element := lru.PushBack(&cacheEntry{key: key, verdict: verdict})
		cache[key] = element
	}
	for v.limits.CacheSize > 0 && lru.Len() > v.limits.CacheSize {
		oldest := lru.Front()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		delete(cache, entry.key)
		lru.Remove(oldest)
	}

	policies := make(map[string]struct{}, len(doc.Policies))
	for _, policy := range doc.Policies {
		policies[policy] = struct{}{}
	}
	verdicts := make(map[string]Verdict, len(doc.Verdicts))
	for id, verdict := range doc.Verdicts {
		verdict.Cached = false
		verdicts[id] = verdict
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.trust = doc.Trust
	v.policies = policies
	v.revoked = doc.Revoked
	v.cache = cache
	v.lru = lru
	v.verdicts = verdicts
	v.stats = doc.Stats
	v.stats.Trusted = len(v.trust)
	return nil
}

// Reset drops the trust store, revocations and every stored verdict.
func (v *TimestampAuthorityVerifier) Reset() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.trust = make(map[string][]Cert)
	v.policies = make(map[string]struct{})
	v.revoked = make(map[string]time.Time)
	v.cache = make(map[string]*list.Element)
	v.lru = list.New()
	v.verdicts = make(map[string]Verdict)
	v.stats = Stats{}
	return nil
}

func parseToken(raw []byte) (tokenDocument, time.Time, string, error) {
	var doc tokenDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return tokenDocument{}, time.Time{}, "", fmt.Errorf("%w: invalid JSON: %v", ErrBadToken, err)
	}
	if strings.TrimSpace(doc.TSA) == "" ||
		strings.TrimSpace(doc.Serial) == "" ||
		strings.TrimSpace(doc.GenTime) == "" ||
		strings.TrimSpace(doc.Digest) == "" {
		return tokenDocument{}, time.Time{}, "", fmt.Errorf("%w: required field is missing", ErrBadToken)
	}

	genTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(doc.GenTime))
	if err != nil {
		return tokenDocument{}, time.Time{}, "", fmt.Errorf("%w: invalid genTime: %v", ErrBadToken, err)
	}
	return doc, genTime, tokenID(raw), nil
}

func tokenID(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func normalizeImprint(imprint Imprint) (Imprint, error) {
	if imprint.Algorithm != "sha256" {
		return Imprint{}, fmt.Errorf("%w: algorithm %q is not supported", ErrImprintMismatch, imprint.Algorithm)
	}
	if len(imprint.Digest) != 64 {
		return Imprint{}, fmt.Errorf("%w: digest must be 64 hex characters", ErrImprintMismatch)
	}
	decoded, err := hex.DecodeString(imprint.Digest)
	if err != nil || len(decoded) != sha256.Size {
		return Imprint{}, fmt.Errorf("%w: digest is not hexadecimal", ErrImprintMismatch)
	}
	imprint.Digest = strings.ToLower(imprint.Digest)
	return imprint, nil
}

func makeCacheKey(tokenID string, imprint Imprint) string {
	identity, _ := json.Marshal(struct {
		TokenID   string `json:"tokenId"`
		Algorithm string `json:"algorithm"`
		Digest    string `json:"digest"`
		Nonce     string `json:"nonce"`
	}{
		TokenID:   tokenID,
		Algorithm: imprint.Algorithm,
		Digest:    imprint.Digest,
		Nonce:     imprint.Nonce,
	})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}

func validateTrustAnchor(tsa string, chain []Cert) error {
	if strings.TrimSpace(tsa) == "" {
		return fmt.Errorf("%w: authority name is empty", ErrBadTrust)
	}
	if len(chain) == 0 {
		return fmt.Errorf("%w: certificate chain is empty", ErrBadTrust)
	}
	if chain[0].Subject != tsa {
		return fmt.Errorf("%w: leaf subject %q does not match authority %q", ErrBadTrust, chain[0].Subject, tsa)
	}
	for i := 0; i+1 < len(chain); i++ {
		if chain[i].Issuer != chain[i+1].Subject {
			return fmt.Errorf("%w: certificate %d is not linked to the next certificate", ErrBadTrust, i)
		}
	}
	return nil
}

func cloneChain(chain []Cert) []Cert {
	copied := make([]Cert, len(chain))
	for i, cert := range chain {
		cert.EKUs = append([]string(nil), cert.EKUs...)
		copied[i] = cert
	}
	return copied
}

func (v *TimestampAuthorityVerifier) currentNowLocked() time.Time {
	if v.limits.Now.IsZero() {
		return time.Now().UTC()
	}
	return v.limits.Now
}

func (v *TimestampAuthorityVerifier) evaluateLocked(
	doc tokenDocument,
	genTime time.Time,
	id string,
	imprint Imprint,
) Verdict {
	verdict := Verdict{
		TokenID: id,
		TSA:     doc.TSA,
		GenTime: genTime,
		Skew:    v.currentNowLocked().Sub(genTime),
	}

	if !strings.EqualFold(strings.TrimSpace(doc.Digest), imprint.Digest) {
		verdict.Reason = ReasonImprint
		return verdict
	}

	chain, trusted := v.trust[doc.TSA]
	if !trusted {
		verdict.Reason = ReasonUntrusted
		return verdict
	}
	if !v.policyAllowedLocked(doc.Policy) {
		verdict.Reason = ReasonPolicy
		return verdict
	}
	if !hasTimestampingEKU(chain[0].EKUs) {
		verdict.Reason = ReasonUntrusted
		return verdict
	}

	leaf := chain[0]
	if genTime.Before(leaf.NotBefore) || genTime.After(leaf.NotAfter) {
		verdict.Reason = ReasonCertWindow
		return verdict
	}

	if revokedAt, revoked := v.revoked[doc.Serial]; revoked && !revokedAt.After(genTime) {
		verdict.Reason = ReasonRevoked
		return verdict
	}

	verdict.Valid = true
	verdict.Reason = ReasonOK
	return verdict
}

func (v *TimestampAuthorityVerifier) policyAllowedLocked(policy string) bool {
	if len(v.policies) == 0 {
		return true
	}
	if _, allowed := v.policies["*"]; allowed {
		return true
	}
	_, allowed := v.policies[policy]
	return allowed
}

func hasTimestampingEKU(ekus []string) bool {
	if len(ekus) == 0 {
		return true
	}
	for _, eku := range ekus {
		switch strings.ToLower(strings.TrimSpace(eku)) {
		case "timestamping", "timestamp", "time-stamping", "timestamp-signing",
			"1.3.6.1.5.5.7.3.8":
			return true
		}
	}
	return false
}

func (v *TimestampAuthorityVerifier) cacheGetLocked(key string) (Verdict, bool) {
	element, ok := v.cache[key]
	if !ok {
		return Verdict{}, false
	}
	v.lru.MoveToBack(element)
	return element.Value.(*cacheEntry).verdict, true
}

func (v *TimestampAuthorityVerifier) cachePutLocked(key string, verdict Verdict) {
	stored := verdict
	stored.Cached = false

	if element, ok := v.cache[key]; ok {
		element.Value.(*cacheEntry).verdict = stored
		v.lru.MoveToBack(element)
		return
	}

	element := v.lru.PushBack(&cacheEntry{key: key, verdict: stored})
	v.cache[key] = element

	if v.limits.CacheSize > 0 {
		for v.lru.Len() > v.limits.CacheSize {
			oldest := v.lru.Front()
			if oldest == nil {
				break
			}
			entry := oldest.Value.(*cacheEntry)
			delete(v.cache, entry.key)
			v.lru.Remove(oldest)
		}
	}
}

func (v *TimestampAuthorityVerifier) recordMalformed(token []byte) Verdict {
	id := tokenID(token)
	verdict := Verdict{
		TokenID: id,
		Valid:   false,
		Reason:  ReasonMalformed,
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.verdicts[id] = verdict
	v.stats.Rejected++
	return verdict
}

func (v *TimestampAuthorityVerifier) statsLocked() Stats {
	stats := v.stats
	stats.Trusted = len(v.trust)
	return stats
}

func (v *TimestampAuthorityVerifier) lruBackToFront() []*list.Element {
	elements := make([]*list.Element, 0, v.lru.Len())
	for element := v.lru.Back(); element != nil; element = element.Prev() {
		elements = append(elements, element)
	}
	// Reverse to return front-to-back while keeping the helper explicit.
	for i, j := 0, len(elements)-1; i < j; i, j = i+1, j-1 {
		elements[i], elements[j] = elements[j], elements[i]
	}
	return elements
}

func decodeSnapshot(data []byte) (*snapshotDocument, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrBadSnapshot)
	}

	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: invalid envelope: %v", ErrBadSnapshot, err)
	}
	if envelope.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported envelope version %d", ErrBadSnapshot, envelope.Version)
	}
	if len(envelope.Payload) == 0 || envelope.Checksum == "" {
		return nil, fmt.Errorf("%w: payload or checksum is missing", ErrBadSnapshot)
	}

	expectedChecksum := sha256.Sum256(envelope.Payload)
	actualChecksum, err := hex.DecodeString(envelope.Checksum)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid checksum encoding", ErrBadSnapshot)
	}
	if subtle.ConstantTimeCompare(actualChecksum, expectedChecksum[:]) != 1 {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var doc snapshotDocument
	if err := json.Unmarshal(envelope.Payload, &doc); err != nil {
		return nil, fmt.Errorf("%w: invalid document: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshotDocument(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func validateSnapshotDocument(doc *snapshotDocument) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadSnapshot, fmt.Sprintf(format, args...))
	}

	if doc.Version != snapshotVersion {
		return invalid("unsupported version %d", doc.Version)
	}
	if doc.Trust == nil || doc.Policies == nil || doc.Revoked == nil ||
		doc.Cache == nil || doc.CacheOrder == nil || doc.Verdicts == nil {
		return invalid("a required snapshot section is missing")
	}
	for tsa, chain := range doc.Trust {
		if err := validateTrustAnchor(tsa, chain); err != nil {
			return invalid("invalid trust anchor %q: %v", tsa, err)
		}
	}
	for serial := range doc.Revoked {
		if strings.TrimSpace(serial) == "" {
			return invalid("empty revocation serial")
		}
	}

	seenCacheKey := make(map[string]struct{}, len(doc.CacheOrder))
	for _, key := range doc.CacheOrder {
		if _, ok := doc.Cache[key]; !ok {
			return invalid("cache order references missing key %q", key)
		}
		if _, duplicate := seenCacheKey[key]; duplicate {
			return invalid("duplicate cache order key %q", key)
		}
		seenCacheKey[key] = struct{}{}
	}
	if len(doc.CacheOrder) != len(doc.Cache) {
		return invalid("cache order does not cover every cache entry")
	}
	for key, verdict := range doc.Cache {
		if key == "" || len(verdict.TokenID) != sha256.Size*2 {
			return invalid("cache entry %q is incomplete", key)
		}
		if verdict.Reason == "" {
			return invalid("cache entry %q has no reason", key)
		}
	}
	for id, verdict := range doc.Verdicts {
		if len(id) != sha256.Size*2 || verdict.TokenID != id {
			return invalid("verdict key %q is inconsistent", id)
		}
		if verdict.Reason == "" {
			return invalid("verdict %q has no reason", id)
		}
	}
	if doc.Stats.Verified < 0 || doc.Stats.Rejected < 0 ||
		doc.Stats.Cached < 0 || doc.Stats.Trusted < 0 {
		return invalid("statistics must not be negative")
	}
	if doc.Stats.Trusted != len(doc.Trust) {
		return invalid("trusted counter does not match trust store")
	}
	return nil
}
