package task

import (
	"errors"
	"net/netip"
	"testing"

	impl "gsb/batch50-go-safety-b-03"
)

func cacheConfig() impl.CacheConfig {
	return impl.CacheConfig{Capacity: 4, MaxEntriesPerTenant: 4, MinTTL: 1}
}

func tenantPolicy() impl.TenantPolicy {
	return impl.TenantPolicy{Tenant: "t1", Enabled: true, V4Prefix: 24, V6Prefix: 48, MaxPrefix: 64}
}

func v4Question() impl.Question {
	return impl.Question{Name: "cdn.example.com", Type: 1, Class: 1}
}

func TestDecideTruncatesIPv4ToPolicyPrefix(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if scope.Omit {
		t.Fatalf("public address must emit a client subnet")
	}
	if scope.Tenant != "t1" || scope.Family != impl.FamilyV4 {
		t.Fatalf("scope identity = %+v", scope)
	}
	if scope.PrefixLen != 24 {
		t.Fatalf("prefix length = %d, want 24", scope.PrefixLen)
	}
	if scope.Prefix.String() != "203.0.113.0/24" {
		t.Fatalf("prefix = %s, want 203.0.113.0/24", scope.Prefix)
	}
	if scope.Address.String() != "203.0.113.0" {
		t.Fatalf("address = %s, want 203.0.113.0", scope.Address)
	}
}

func TestDecideTruncatesIPv6ToPolicyPrefix(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("2001:db8:abcd:12::5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if scope.Family != impl.FamilyV6 {
		t.Fatalf("family = %q, want %q", scope.Family, impl.FamilyV6)
	}
	if scope.PrefixLen != 48 {
		t.Fatalf("prefix length = %d, want 48", scope.PrefixLen)
	}
	if scope.Prefix.String() != "2001:db8:abcd::/48" {
		t.Fatalf("prefix = %s, want 2001:db8:abcd::/48", scope.Prefix)
	}
	if scope.Address.String() != "2001:db8:abcd::" {
		t.Fatalf("address = %s, want 2001:db8:abcd::", scope.Address)
	}
}

func TestDecideSuppressesPrivateAndLoopback(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	for _, raw := range []string{"10.1.2.3", "172.16.9.9", "192.168.5.4", "127.0.0.1", "169.254.9.9", "0.0.0.0", "::1", "fe80::44"} {
		scope, err := g.Decide(netip.MustParseAddr(raw), v4Question(), tenantPolicy())
		if err != nil {
			t.Fatalf("%s: decide: %v", raw, err)
		}
		if !scope.Omit {
			t.Fatalf("%s must not emit an option, got %+v", raw, scope)
		}
		if scope.PrefixLen != 0 || scope.Family != impl.FamilyNone {
			t.Fatalf("%s scope = %+v, want no family and prefix 0", raw, scope)
		}
	}
}

func TestDecideSuppressesDisabledTenant(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	off := tenantPolicy()
	off.Enabled = false
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), off)
	if err != nil {
		t.Fatalf("decide disabled tenant: %v", err)
	}
	if !scope.Omit {
		t.Fatalf("disabled tenant must not emit an option, got %+v", scope)
	}
	blank := tenantPolicy()
	blank.Tenant = "   "
	if _, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), blank); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("blank tenant: err = %v, want ErrBadPolicy", err)
	}
}

func TestDecideRejectsZoneAndInvalidAddress(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	if _, err := g.Decide(netip.MustParseAddr("fe80::1%eth0"), v4Question(), tenantPolicy()); !errors.Is(err, impl.ErrZoneAddr) {
		t.Fatalf("zoned address: err = %v, want ErrZoneAddr", err)
	}
	if _, err := g.Decide(netip.Addr{}, v4Question(), tenantPolicy()); !errors.Is(err, impl.ErrInvalidAddr) {
		t.Fatalf("zero address: err = %v, want ErrInvalidAddr", err)
	}
}

func TestDecideCapsPrefixAtTenantMaximum(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	policy := tenantPolicy()
	policy.V4Prefix = 24
	policy.MaxPrefix = 16
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), policy)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if scope.PrefixLen != 16 || scope.Prefix.String() != "203.0.0.0/16" {
		t.Fatalf("capped scope = %+v, want 203.0.0.0/16", scope)
	}
	policy.V4Prefix = 8
	policy.MaxPrefix = 16
	scope, err = g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), policy)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if scope.PrefixLen != 8 {
		t.Fatalf("narrower policy prefix = %d, want 8", scope.PrefixLen)
	}
}

func TestRewriteEmitsIPv4OptionBytes(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	got, err := g.Rewrite(v4Question(), scope)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	want := []byte{0x00, 0x08, 0x00, 0x07, 0x00, 0x01, 24, 0, 203, 0, 113}
	if len(got) != len(want) {
		t.Fatalf("option bytes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("option byte %d = %#x, want %#x (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestRewriteEmitsIPv6OptionBytes(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("2001:db8:abcd:12::5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	got, err := g.Rewrite(v4Question(), scope)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	want := []byte{0x00, 0x08, 0x00, 0x0a, 0x00, 0x02, 48, 0, 0x20, 0x01, 0x0d, 0xb8, 0xab, 0xcd}
	if len(got) != len(want) {
		t.Fatalf("option bytes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("option byte %d = %#x, want %#x (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestRewriteRejectsOmittedScopeAndHandlesGlobal(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	off := tenantPolicy()
	off.Enabled = false
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), off)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, err := g.Rewrite(v4Question(), scope); !errors.Is(err, impl.ErrScopeOmitted) {
		t.Fatalf("omitted scope: err = %v, want ErrScopeOmitted", err)
	}
	global := impl.Scope{Tenant: "t1", Family: impl.FamilyV4, PrefixLen: 0}
	got, err := g.Rewrite(v4Question(), global)
	if err != nil {
		t.Fatalf("rewrite global: %v", err)
	}
	want := []byte{0x00, 0x08, 0x00, 0x04, 0x00, 0x01, 0x00, 0x00}
	if len(got) != len(want) {
		t.Fatalf("global option bytes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("global option byte %d = %#x, want %#x", i, got[i], want[i])
		}
	}
}

func TestCacheKeySharesSameEffectivePrefix(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	s1, err := g.Decide(netip.MustParseAddr("203.0.113.5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	s2, err := g.Decide(netip.MustParseAddr("203.0.113.77"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	k1, err := g.CacheKey(v4Question(), s1)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	k2, err := g.CacheKey(v4Question(), s2)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if k1 == "" {
		t.Fatalf("cache key must not be empty")
	}
	if k1 != k2 {
		t.Fatalf("same /24 clients got different keys: %q vs %q", k1, k2)
	}
	s3, err := g.Decide(netip.MustParseAddr("198.51.100.5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	k3, err := g.CacheKey(v4Question(), s3)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if k1 == k3 {
		t.Fatalf("different subnets share the key %q", k1)
	}
	other := tenantPolicy()
	other.Tenant = "t2"
	s4, err := g.Decide(netip.MustParseAddr("203.0.113.5"), v4Question(), other)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	k4, err := g.CacheKey(v4Question(), s4)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if k1 == k4 {
		t.Fatalf("different tenants share the key %q", k1)
	}
}

func TestCacheKeyGlobalWhenPrefixIsZero(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	a := impl.Scope{Tenant: "t1", Family: impl.FamilyV4, PrefixLen: 0}
	b := impl.Scope{Tenant: "t1", Family: impl.FamilyV4, PrefixLen: 0, Address: netip.MustParseAddr("198.51.100.9")}
	ka, err := g.CacheKey(v4Question(), a)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	kb, err := g.CacheKey(v4Question(), b)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if ka != kb {
		t.Fatalf("global scopes got different keys: %q vs %q", ka, kb)
	}
	s, err := g.Decide(netip.MustParseAddr("203.0.113.5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	kc, err := g.CacheKey(v4Question(), s)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if ka == kc {
		t.Fatalf("global key collides with a /24 key: %q", ka)
	}
}

func TestCacheKeySeparatesNameTypeAndClass(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	upper := v4Question()
	upper.Name = "CDN.Example.COM"
	lower, err := g.CacheKey(v4Question(), scope)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	mixed, err := g.CacheKey(upper, scope)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if lower != mixed {
		t.Fatalf("question name case changed the key: %q vs %q", lower, mixed)
	}
	aaaa := v4Question()
	aaaa.Type = 28
	otherType, err := g.CacheKey(aaaa, scope)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if otherType == lower {
		t.Fatalf("different record types share the key %q", lower)
	}
	otherClass := v4Question()
	otherClass.Class = 3
	classKey, err := g.CacheKey(otherClass, scope)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if classKey == lower {
		t.Fatalf("different classes share the key %q", lower)
	}
}

func TestObserveAnswerStoresTTL(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	key, err := g.CacheKey(v4Question(), scope)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	if _, ok := g.TTLFor(key); ok {
		t.Fatalf("unknown key must not report a TTL")
	}
	if err := g.ObserveAnswer(key, 300, 24); err != nil {
		t.Fatalf("observe answer: %v", err)
	}
	ttl, ok := g.TTLFor(key)
	if !ok {
		t.Fatalf("observed key must be readable")
	}
	if ttl != 300 {
		t.Fatalf("ttl = %d, want 300", ttl)
	}
	if got := g.Stats().Entries; got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
}

func TestBeginLookupMergesInflightFetch(t *testing.T) {
	g := impl.NewEcsPrivacyGuard(cacheConfig())
	scope, err := g.Decide(netip.MustParseAddr("203.0.113.5"), v4Question(), tenantPolicy())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	key, err := g.CacheKey(v4Question(), scope)
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	token, leader := g.BeginLookup(key)
	if !leader {
		t.Fatalf("first lookup must lead")
	}
	if token.Key != key || !token.Leader {
		t.Fatalf("leader token = %+v", token)
	}
	second, leader := g.BeginLookup(key)
	if leader {
		t.Fatalf("second lookup on the same key must join instead of leading")
	}
	if second.Leader {
		t.Fatalf("joined token must not be marked leader")
	}
	stats := g.Stats()
	if stats.Inflight != 1 {
		t.Fatalf("inflight = %d, want 1", stats.Inflight)
	}
	if stats.Merged != 1 {
		t.Fatalf("merged = %d, want 1", stats.Merged)
	}
	if err := g.EndLookup(token, 60); err != nil {
		t.Fatalf("end lookup: %v", err)
	}
	if stats := g.Stats(); stats.Inflight != 0 {
		t.Fatalf("inflight after end = %d, want 0", stats.Inflight)
	}
	if _, leader := g.BeginLookup("another-key"); !leader {
		t.Fatalf("a key without an in-flight lookup must be led")
	}
}

func TestCapacityEvictsOldestEntry(t *testing.T) {
	cfg := cacheConfig()
	cfg.Capacity = 2
	g := impl.NewEcsPrivacyGuard(cfg)
	if err := g.ObserveAnswer("k1", 100, 24); err != nil {
		t.Fatalf("observe k1: %v", err)
	}
	if err := g.ObserveAnswer("k2", 100, 24); err != nil {
		t.Fatalf("observe k2: %v", err)
	}
	if err := g.ObserveAnswer("k3", 100, 24); err != nil {
		t.Fatalf("observe k3: %v", err)
	}
	stats := g.Stats()
	if stats.Entries != 2 || stats.Evictions != 1 {
		t.Fatalf("stats = %+v, want 2 entries and 1 eviction", stats)
	}
	if _, ok := g.TTLFor("k1"); ok {
		t.Fatalf("oldest entry must be evicted")
	}
	if _, ok := g.TTLFor("k3"); !ok {
		t.Fatalf("newest entry must survive")
	}
	if err := g.SetCapacity(0); !errors.Is(err, impl.ErrCapacity) {
		t.Fatalf("zero capacity: err = %v, want ErrCapacity", err)
	}
}
