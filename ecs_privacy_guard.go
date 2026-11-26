// Package task trims EDNS client subnet data for a recursive resolver and
// isolates the answer cache by effective prefix.
package task

import (
	"errors"
	"net/netip"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
}

// CacheConfig bounds the answer cache.
type CacheConfig struct {
	Capacity            int
	MaxEntriesPerTenant int
	MinTTL              uint32
}

// EcsPrivacyGuard owns the scope decisions and the prefix keyed cache.
type EcsPrivacyGuard struct {
	cfg CacheConfig
}

// NewEcsPrivacyGuard builds a guard with the given caps.
func NewEcsPrivacyGuard(cfg CacheConfig) *EcsPrivacyGuard {
	return &EcsPrivacyGuard{cfg: cfg}
}

// Decide truncates a client address to the effective policy prefix.
func (g *EcsPrivacyGuard) Decide(clientAddr netip.Addr, q Question, policy TenantPolicy) (Scope, error) {
	return Scope{}, errNotImplemented
}

// Rewrite renders the ECS option bytes for one scope.
func (g *EcsPrivacyGuard) Rewrite(q Question, scope Scope) ([]byte, error) {
	return nil, errNotImplemented
}

// CacheKey builds the cache key of one question under one scope.
func (g *EcsPrivacyGuard) CacheKey(q Question, scope Scope) (string, error) {
	return "", errNotImplemented
}

// ObserveAnswer stores the answer TTL under one cache key.
func (g *EcsPrivacyGuard) ObserveAnswer(key string, ttl uint32, prefixLen int) error {
	return errNotImplemented
}

// TTLFor reads the stored TTL of one cache key.
func (g *EcsPrivacyGuard) TTLFor(key string) (uint32, bool) {
	return 0, false
}

// BeginLookup joins one in-flight upstream lookup, or leads a new one.
func (g *EcsPrivacyGuard) BeginLookup(key string) (LookupToken, bool) {
	return LookupToken{}, false
}

// EndLookup finishes a leading lookup and publishes its TTL.
func (g *EcsPrivacyGuard) EndLookup(token LookupToken, ttl uint32) error {
	return errNotImplemented
}

// UpstreamRejected re-homes a lookup after the upstream refused EDNS.
func (g *EcsPrivacyGuard) UpstreamRejected(key string) (Scope, error) {
	return Scope{}, errNotImplemented
}

// Stats returns a snapshot of the live counters.
func (g *EcsPrivacyGuard) Stats() CacheStats {
	return CacheStats{}
}

// SetCapacity changes the answer cache capacity.
func (g *EcsPrivacyGuard) SetCapacity(n int) error {
	return errNotImplemented
}

// Reset clears every cached answer and counter.
func (g *EcsPrivacyGuard) Reset() {
}
