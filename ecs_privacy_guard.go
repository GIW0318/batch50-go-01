// Package task trims EDNS client subnet data for a recursive resolver and
// isolates the answer cache by effective prefix.
package task

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Address families used by a scope.
const (
	FamilyV4   = "ipv4"
	FamilyV6   = "ipv6"
	FamilyNone = "none"
)

// Errors surfaced by the guard.
var (
	ErrInvalidAddr  = errors.New("client address is invalid")
	ErrZoneAddr     = errors.New("client address carries a zone")
	ErrBadPolicy    = errors.New("tenant policy is invalid")
	ErrScopeOmitted = errors.New("scope does not carry a client subnet")
	ErrBadScope     = errors.New("scope cannot be rewritten")
	ErrCapacity     = errors.New("cache capacity must be positive")
	ErrBadSnapshot  = errors.New("snapshot payload invalid")
)

// Question is the resolver question a client asked.
type Question struct {
	Name  string
	Type  uint16
	Class uint16
}

// TenantPolicy is the per tenant subnet policy.
type TenantPolicy struct {
	Tenant    string
	Enabled   bool
	V4Prefix  int
	V6Prefix  int
	MaxPrefix int
}

// Scope is the effective client subnet attached to one lookup.
type Scope struct {
	Tenant    string
	Family    string
	PrefixLen int
	Prefix    netip.Prefix
	Address   netip.Addr
	Omit      bool
}

// CacheStats reports the live counters of the guard.
type CacheStats struct {
	Entries    int
	Inflight   int
	Merged     int
	Evictions  int
	Suppressed int
}

// LookupToken identifies one joined cache lookup.
type LookupToken struct {
	Key    string
	Leader bool

	state *lookupState
}

type lookupState struct {
	done chan struct{}
	err  error
}

// CacheConfig bounds the answer cache.
type CacheConfig struct {
	Capacity            int
	MaxEntriesPerTenant int
	MinTTL              uint32
}

type cacheEntry struct {
	key       string
	ttl       uint32
	prefixLen int
	tenant    string
	answerID  string
	seq       uint64
}

// EcsPrivacyGuard owns the scope decisions and the prefix keyed cache.
type EcsPrivacyGuard struct {
	mu sync.Mutex

	cfg CacheConfig

	entries       map[string]*cacheEntry
	tenantEntries map[string]int
	inflight      map[string]*lookupState
	nextSeq       uint64

	merged     int
	evictions  int
	suppressed int
}

// NewEcsPrivacyGuard builds a guard with the given caps.
func NewEcsPrivacyGuard(cfg CacheConfig) *EcsPrivacyGuard {
	return &EcsPrivacyGuard{
		cfg:           cfg,
		entries:       make(map[string]*cacheEntry),
		tenantEntries: make(map[string]int),
		inflight:      make(map[string]*lookupState),
	}
}

// Decide truncates a client address to the effective policy prefix.
func (g *EcsPrivacyGuard) Decide(clientAddr netip.Addr, q Question, policy TenantPolicy) (Scope, error) {
	_ = q

	tenant := strings.TrimSpace(policy.Tenant)
	if tenant == "" {
		return Scope{}, ErrBadPolicy
	}
	if !clientAddr.IsValid() {
		return Scope{}, ErrInvalidAddr
	}
	if clientAddr.Zone() != "" {
		return Scope{}, ErrZoneAddr
	}
	if policy.V4Prefix < 0 || policy.V4Prefix > 32 ||
		policy.V6Prefix < 0 || policy.V6Prefix > 128 || policy.MaxPrefix < 0 {
		return Scope{}, ErrBadPolicy
	}

	clientAddr = clientAddr.Unmap()
	family, bits, configured := FamilyV4, 32, policy.V4Prefix
	if clientAddr.Is6() {
		family, bits, configured = FamilyV6, 128, policy.V6Prefix
	}

	if isNonPublicClient(clientAddr) || !policy.Enabled {
		g.mu.Lock()
		g.suppressed++
		g.mu.Unlock()
		return Scope{Tenant: tenant, Family: FamilyNone, PrefixLen: 0, Omit: true}, nil
	}

	prefixLen := effectivePrefixLen(configured, policy.MaxPrefix, bits)
	prefix := netip.PrefixFrom(clientAddr, prefixLen).Masked()
	return Scope{
		Tenant:    tenant,
		Family:    family,
		PrefixLen: prefixLen,
		Prefix:    prefix,
		Address:   prefix.Addr(),
	}, nil
}

// Rewrite renders the ECS option bytes for one scope.
func (g *EcsPrivacyGuard) Rewrite(q Question, scope Scope) ([]byte, error) {
	_ = q

	if scope.Omit || scope.Family == FamilyNone {
		return nil, ErrScopeOmitted
	}

	familyCode, bits, err := familyDetails(scope.Family)
	if err != nil {
		return nil, err
	}
	if scope.PrefixLen < 0 || scope.PrefixLen > bits {
		return nil, ErrBadScope
	}

	var addr netip.Addr
	if scope.PrefixLen > 0 {
		addr, err = prefixAddress(scope, scope.Family, scope.PrefixLen)
		if err != nil {
			return nil, err
		}
	}

	addrBytes := (scope.PrefixLen + 7) / 8
	option := make([]byte, 8+addrBytes)
	option[1] = 8 // ECS option code.
	binary.BigEndian.PutUint16(option[2:4], uint16(4+addrBytes))
	binary.BigEndian.PutUint16(option[4:6], familyCode)
	option[6] = byte(scope.PrefixLen)
	option[7] = 0
	if addrBytes > 0 {
		raw := addr.AsSlice()
		copy(option[8:], raw[:addrBytes])
	}
	return option, nil
}

// CacheKey builds the cache key of one question under one scope.
func (g *EcsPrivacyGuard) CacheKey(q Question, scope Scope) (string, error) {
	tenant := strings.TrimSpace(scope.Tenant)
	if tenant == "" {
		return "", ErrBadPolicy
	}

	family := scope.Family
	prefixLen := scope.PrefixLen
	if scope.Omit {
		family, prefixLen = FamilyNone, 0
	} else if family == "" {
		if prefixLen != 0 {
			return "", ErrBadScope
		}
		family = FamilyNone
	}

	addrToken := "-"
	switch family {
	case FamilyNone:
		if prefixLen != 0 {
			return "", ErrBadScope
		}
	case FamilyV4, FamilyV6:
		_, bits, err := familyDetails(family)
		if err != nil || prefixLen < 0 || prefixLen > bits {
			return "", ErrBadScope
		}
		if prefixLen > 0 {
			addr, err := prefixAddress(scope, family, prefixLen)
			if err != nil {
				return "", err
			}
			addrToken = maskedPrefixToken(family, prefixLen, addr)
		}
	default:
		return "", ErrBadScope
	}

	return buildCacheKey(tenant, family, prefixLen, addrToken, q), nil
}

// ObserveAnswer stores the answer TTL under one cache key.
func (g *EcsPrivacyGuard) ObserveAnswer(key string, ttl uint32, prefixLen int) error {
	if key == "" || prefixLen < 0 || prefixLen > 128 {
		return ErrBadScope
	}
	if info, ok := parseCacheKey(key); ok {
		switch info.family {
		case FamilyNone:
			if prefixLen != 0 {
				return ErrBadScope
			}
		case FamilyV4:
			if prefixLen > 32 {
				return ErrBadScope
			}
		case FamilyV6:
			// The 0..128 bounds were checked above.
		default:
			return ErrBadScope
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.observeAnswerLocked(key, ttl, prefixLen)
}

// TTLFor reads the stored TTL of one cache key.
func (g *EcsPrivacyGuard) TTLFor(key string) (uint32, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.entries[key]
	if !ok {
		return 0, false
	}
	return entry.ttl, true
}

// BeginLookup joins one in-flight upstream lookup, or leads a new one.
func (g *EcsPrivacyGuard) BeginLookup(key string) (LookupToken, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	if state, ok := g.inflight[key]; ok {
		g.merged++
		return LookupToken{Key: key, Leader: false, state: state}, false
	}

	state := &lookupState{done: make(chan struct{})}
	g.inflight[key] = state
	return LookupToken{Key: key, Leader: true, state: state}, true
}

// EndLookup finishes a leading lookup and publishes its TTL.
func (g *EcsPrivacyGuard) EndLookup(token LookupToken, ttl uint32) error {
	if !token.Leader {
		if token.state != nil {
			<-token.state.done
			return token.state.err
		}
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	state, ok := g.inflight[token.Key]
	if !ok || (token.state != nil && state != token.state) {
		return nil
	}
	delete(g.inflight, token.Key)

	prefixLen := 0
	if info, ok := parseCacheKey(token.Key); ok {
		prefixLen = info.prefixLen
	}
	state.err = g.observeAnswerLocked(token.Key, ttl, prefixLen)
	close(state.done)
	return state.err
}

// UpstreamRejected re-homes a lookup after the upstream refused EDNS.
func (g *EcsPrivacyGuard) UpstreamRejected(key string) (Scope, error) {
	if key == "" {
		return Scope{}, ErrBadScope
	}
	info, ok := parseCacheKey(key)
	if !ok {
		return Scope{}, ErrBadScope
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	if _, ok := g.entries[key]; ok {
		g.removeEntryLocked(key)
	}
	if state, ok := g.inflight[key]; ok {
		delete(g.inflight, key)
		close(state.done)
	}

	return Scope{Tenant: info.tenant, Family: FamilyNone, PrefixLen: 0, Omit: true}, nil
}

// Stats returns a snapshot of the live counters.
func (g *EcsPrivacyGuard) Stats() CacheStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return CacheStats{
		Entries:    len(g.entries),
		Inflight:   len(g.inflight),
		Merged:     g.merged,
		Evictions:  g.evictions,
		Suppressed: g.suppressed,
	}
}

// SetCapacity changes the answer cache capacity.
func (g *EcsPrivacyGuard) SetCapacity(n int) error {
	if n <= 0 {
		return ErrCapacity
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()
	g.cfg.Capacity = n
	for len(g.entries) > n {
		if !g.evictOldestLocked(nil) {
			break
		}
	}
	return nil
}

// Reset clears every cached answer and counter.
func (g *EcsPrivacyGuard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()

	for key, state := range g.inflight {
		delete(g.inflight, key)
		close(state.done)
	}
	g.entries = make(map[string]*cacheEntry)
	g.tenantEntries = make(map[string]int)
	g.inflight = make(map[string]*lookupState)
	g.nextSeq = 0
	g.merged = 0
	g.evictions = 0
	g.suppressed = 0
}

func (g *EcsPrivacyGuard) observeAnswerLocked(key string, ttl uint32, prefixLen int) error {
	g.ensureLocked()
	if ttl < g.cfg.MinTTL {
		ttl = g.cfg.MinTTL
	}

	tenant, answerID := "", key
	if info, ok := parseCacheKey(key); ok {
		tenant, answerID = info.tenant, info.answerID()
	}

	if entry, ok := g.entries[key]; ok {
		if ttl < entry.ttl {
			entry.ttl = ttl
		}
		g.applyAnswerMinLocked(answerID)
		return nil
	}

	if g.cfg.Capacity <= 0 {
		return ErrCapacity
	}

	if max := g.cfg.MaxEntriesPerTenant; max > 0 {
		for g.tenantEntries[tenant] >= max {
			if !g.evictOldestLocked(func(entry *cacheEntry) bool {
				return entry.tenant == tenant
			}) {
				break
			}
		}
	}

	for len(g.entries) >= g.cfg.Capacity {
		if !g.evictOldestLocked(nil) {
			break
		}
	}

	minTTL := ttl
	for _, entry := range g.entries {
		if entry.answerID == answerID && entry.ttl < minTTL {
			minTTL = entry.ttl
		}
	}

	g.nextSeq++
	g.entries[key] = &cacheEntry{
		key:       key,
		ttl:       minTTL,
		prefixLen: prefixLen,
		tenant:    tenant,
		answerID:  answerID,
		seq:       g.nextSeq,
	}
	g.tenantEntries[tenant]++
	g.applyAnswerMinLocked(answerID)
	return nil
}

func (g *EcsPrivacyGuard) applyAnswerMinLocked(answerID string) {
	minTTL := uint32(0)
	found := false
	for _, entry := range g.entries {
		if entry.answerID != answerID {
			continue
		}
		if !found || entry.ttl < minTTL {
			minTTL, found = entry.ttl, true
		}
	}
	if !found {
		return
	}
	for _, entry := range g.entries {
		if entry.answerID == answerID && entry.ttl > minTTL {
			entry.ttl = minTTL
		}
	}
}

func (g *EcsPrivacyGuard) evictOldestLocked(match func(*cacheEntry) bool) bool {
	var oldest *cacheEntry
	for _, entry := range g.entries {
		if match != nil && !match(entry) {
			continue
		}
		if oldest == nil || entry.seq < oldest.seq {
			oldest = entry
		}
	}
	if oldest == nil {
		return false
	}
	g.removeEntryLocked(oldest.key)
	g.evictions++
	return true
}

func (g *EcsPrivacyGuard) removeEntryLocked(key string) {
	entry, ok := g.entries[key]
	if !ok {
		return
	}
	delete(g.entries, key)
	g.tenantEntries[entry.tenant]--
	if g.tenantEntries[entry.tenant] <= 0 {
		delete(g.tenantEntries, entry.tenant)
	}
}

func (g *EcsPrivacyGuard) ensureLocked() {
	if g.entries == nil {
		g.entries = make(map[string]*cacheEntry)
	}
	if g.tenantEntries == nil {
		g.tenantEntries = make(map[string]int)
	}
	if g.inflight == nil {
		g.inflight = make(map[string]*lookupState)
	}
}

func effectivePrefixLen(configured, maximum, bits int) int {
	if maximum > bits {
		maximum = bits
	}
	if configured > maximum {
		return maximum
	}
	return configured
}

func isNonPublicClient(addr netip.Addr) bool {
	return addr.IsPrivate() ||
		addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsUnspecified()
}

func familyDetails(family string) (uint16, int, error) {
	switch family {
	case FamilyV4:
		return 1, 32, nil
	case FamilyV6:
		return 2, 128, nil
	default:
		return 0, 0, ErrBadScope
	}
}

func prefixAddress(scope Scope, family string, prefixLen int) (netip.Addr, error) {
	if scope.Prefix.IsValid() {
		addr := scope.Prefix.Addr()
		if addr.Zone() != "" {
			return netip.Addr{}, ErrZoneAddr
		}
		addr = addr.Unmap()
		if scope.Prefix.Bits() == prefixLen && addrFamily(addr) == family {
			return netip.PrefixFrom(addr, prefixLen).Masked().Addr(), nil
		}
	}

	if !scope.Address.IsValid() {
		return netip.Addr{}, ErrBadScope
	}
	addr := scope.Address
	if addr.Zone() != "" {
		return netip.Addr{}, ErrZoneAddr
	}
	addr = addr.Unmap()
	if addrFamily(addr) != family {
		return netip.Addr{}, ErrBadScope
	}
	return netip.PrefixFrom(addr, prefixLen).Masked().Addr(), nil
}

func addrFamily(addr netip.Addr) string {
	if !addr.IsValid() {
		return FamilyNone
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return FamilyV4
	}
	if addr.Is6() {
		return FamilyV6
	}
	return FamilyNone
}

func maskedPrefixToken(family string, prefixLen int, addr netip.Addr) string {
	masked := netip.PrefixFrom(addr, prefixLen).Masked().String()
	sum := sha256.Sum256([]byte(family + "\x00" + strconv.Itoa(prefixLen) + "\x00" + masked))
	return hex.EncodeToString(sum[:])
}

func buildCacheKey(tenant, family string, prefixLen int, addrToken string, q Question) string {
	return strings.Join([]string{
		"ecs1",
		"t=" + url.QueryEscape(tenant),
		"f=" + family,
		"p=" + strconv.Itoa(prefixLen),
		"a=" + addrToken,
		"q=" + url.QueryEscape(strings.ToLower(q.Name)),
		"ty=" + strconv.FormatUint(uint64(q.Type), 10),
		"cl=" + strconv.FormatUint(uint64(q.Class), 10),
	}, "|")
}

type parsedCacheKey struct {
	tenant    string
	family    string
	prefixLen int
	name      string
	qtype     uint16
	qclass    uint16
}

func (k parsedCacheKey) answerID() string {
	return strings.Join([]string{
		k.tenant,
		k.name,
		strconv.FormatUint(uint64(k.qtype), 10),
		strconv.FormatUint(uint64(k.qclass), 10),
	}, "\x00")
}

func parseCacheKey(key string) (parsedCacheKey, bool) {
	parts := strings.Split(key, "|")
	if len(parts) != 8 || parts[0] != "ecs1" {
		return parsedCacheKey{}, false
	}

	tenant, ok := parseKeyPart(parts[1], "t=", true)
	if !ok {
		return parsedCacheKey{}, false
	}
	family, ok := parseKeyPart(parts[2], "f=", true)
	if !ok {
		return parsedCacheKey{}, false
	}
	prefixRaw, ok := parseKeyPart(parts[3], "p=", true)
	if !ok {
		return parsedCacheKey{}, false
	}
	prefixLen, err := strconv.Atoi(prefixRaw)
	if err != nil || prefixLen < 0 {
		return parsedCacheKey{}, false
	}
	if _, ok := parseKeyPart(parts[4], "a=", true); !ok {
		return parsedCacheKey{}, false
	}
	name, ok := parseKeyPart(parts[5], "q=", true)
	if !ok {
		return parsedCacheKey{}, false
	}
	typeRaw, ok := parseKeyPart(parts[6], "ty=", true)
	if !ok {
		return parsedCacheKey{}, false
	}
	qtype, err := strconv.ParseUint(typeRaw, 10, 16)
	if err != nil {
		return parsedCacheKey{}, false
	}
	classRaw, ok := parseKeyPart(parts[7], "cl=", true)
	if !ok {
		return parsedCacheKey{}, false
	}
	qclass, err := strconv.ParseUint(classRaw, 10, 16)
	if err != nil {
		return parsedCacheKey{}, false
	}

	switch family {
	case FamilyNone:
		if prefixLen != 0 {
			return parsedCacheKey{}, false
		}
	case FamilyV4:
		if prefixLen > 32 {
			return parsedCacheKey{}, false
		}
	case FamilyV6:
		if prefixLen > 128 {
			return parsedCacheKey{}, false
		}
	default:
		return parsedCacheKey{}, false
	}

	return parsedCacheKey{
		tenant:    tenant,
		family:    family,
		prefixLen: prefixLen,
		name:      name,
		qtype:     uint16(qtype),
		qclass:    uint16(qclass),
	}, true
}

func parseKeyPart(part, label string, allowEmpty bool) (string, bool) {
	if !strings.HasPrefix(part, label) {
		return "", false
	}
	raw := strings.TrimPrefix(part, label)
	if !allowEmpty && raw == "" {
		return "", false
	}
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		return "", false
	}
	return decoded, true
}
