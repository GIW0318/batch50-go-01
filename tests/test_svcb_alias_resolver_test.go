package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-19"
)

var svcbBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func svcbConfig(clock *time.Time, answers map[string][]impl.Record) impl.ResolverConfig {
	return impl.ResolverConfig{
		MaxAliasDepth: 2,
		MaxHints:      4,
		MaxRecords:    6,
		ChainTTL:      5 * time.Minute,
		Lookup: func(name string) ([]impl.Record, error) {
			return answers[name], nil
		},
		HintLookup: func(name string) ([]string, error) { return nil, nil },
		Now:        func() time.Time { return *clock },
	}
}

func alpnParam(value string) impl.Param {
	return impl.Param{Key: impl.KeyALPN, Value: value}
}

func TestResolveServiceModeSortsByPriority(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"svc.example": {
			{Name: "svc.example", Priority: 10, Target: "svc.example", Params: []impl.Param{alpnParam("h2")}},
			{Name: "svc.example", Priority: 5, Target: "svc.example", Params: []impl.Param{alpnParam("h3")}},
		},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	chain, err := r.Resolve("svc.example")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if chain.Mode != impl.ModeService {
		t.Fatalf("mode = %q, want %q", chain.Mode, impl.ModeService)
	}
	if len(chain.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(chain.Records))
	}
	if chain.Records[0].Priority != 5 || chain.Records[1].Priority != 10 {
		t.Fatalf("record order = %d then %d, want 5 then 10", chain.Records[0].Priority, chain.Records[1].Priority)
	}
	if !chain.Expires.Equal(svcbBase.Add(5 * time.Minute)) {
		t.Fatalf("expires = %v, want %v", chain.Expires, svcbBase.Add(5*time.Minute))
	}
	if chain.Depth != 0 {
		t.Fatalf("depth = %d, want 0 for a direct service answer", chain.Depth)
	}
}

func TestResolveAliasFollowsTarget(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"www.example": {
			{Name: "www.example", Priority: 0, Target: "svc.example"},
		},
		"svc.example": {
			{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{alpnParam("h2,h3")}},
		},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	chain, err := r.Resolve("www.example")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if chain.Mode != impl.ModeService {
		t.Fatalf("mode = %q, want %q after following the alias", chain.Mode, impl.ModeService)
	}
	if chain.Depth != 1 {
		t.Fatalf("depth = %d, want 1", chain.Depth)
	}
	if len(chain.Aliases) != 1 || chain.Aliases[0] != "svc.example" {
		t.Fatalf("aliases = %+v, want [svc.example]", chain.Aliases)
	}
	if chain.Name != "www.example" {
		t.Fatalf("chain name = %q, want the queried name", chain.Name)
	}
	if len(chain.Records) != 1 || chain.Records[0].Name != "svc.example" {
		t.Fatalf("resolved records = %+v, want the service RRset", chain.Records)
	}
}

func TestResolveRejectsAliasLoop(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"a.example": {{Name: "a.example", Priority: 0, Target: "b.example"}},
		"b.example": {{Name: "b.example", Priority: 0, Target: "a.example"}},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	if _, err := r.Resolve("a.example"); !errors.Is(err, impl.ErrAliasLoop) {
		t.Fatalf("loop: err = %v, want ErrAliasLoop", err)
	}
	if _, ok := r.Cached("a.example"); ok {
		t.Fatalf("failed resolution was cached")
	}
}

func TestResolveEnforcesAliasDepth(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"n1.example": {{Name: "n1.example", Priority: 0, Target: "n2.example"}},
		"n2.example": {{Name: "n2.example", Priority: 0, Target: "n3.example"}},
		"n3.example": {{Name: "n3.example", Priority: 0, Target: "n4.example"}},
		"n4.example": {{Name: "n4.example", Priority: 1, Target: "n4.example", Params: []impl.Param{alpnParam("h2")}}},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	if _, err := r.Resolve("n1.example"); !errors.Is(err, impl.ErrAliasDepth) {
		t.Fatalf("depth: err = %v, want ErrAliasDepth", err)
	}
	if _, err := r.Resolve("n2.example"); err != nil {
		t.Fatalf("chain within the depth cap must resolve, got %v", err)
	}
}

func TestResolveRejectsMixedPriorities(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"mixed.example": {
			{Name: "mixed.example", Priority: 0, Target: "svc.example"},
			{Name: "mixed.example", Priority: 1, Target: "svc.example", Params: []impl.Param{alpnParam("h2")}},
		},
		"svc.example": {{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{alpnParam("h2")}}},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	if _, err := r.Resolve("mixed.example"); !errors.Is(err, impl.ErrMixedMode) {
		t.Fatalf("mixed rrset: err = %v, want ErrMixedMode", err)
	}
}

func TestResolveRejectsEmptyAnswerAndLookupFailure(t *testing.T) {
	clock := svcbBase
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, map[string][]impl.Record{}))
	if _, err := r.Resolve("empty.example"); !errors.Is(err, impl.ErrNoRecord) {
		t.Fatalf("empty answer: err = %v, want ErrNoRecord", err)
	}
	cfg := svcbConfig(&clock, nil)
	cfg.Lookup = func(name string) ([]impl.Record, error) {
		return nil, errors.New("servfail")
	}
	broken := impl.NewSvcbAliasResolver(cfg)
	if _, err := broken.Resolve("svc.example"); !errors.Is(err, impl.ErrLookup) {
		t.Fatalf("lookup failure: err = %v, want ErrLookup", err)
	}
	if _, err := broken.Resolve("   "); !errors.Is(err, impl.ErrBlankName) {
		t.Fatalf("blank name: err = %v, want ErrBlankName", err)
	}
}

func TestValidateRejectsUnknownMandatory(t *testing.T) {
	r := impl.NewSvcbAliasResolver(svcbConfig(nil, nil))
	rr := impl.Record{
		Name:     "svc.example",
		Priority: 1,
		Target:   "svc.example",
		Params:   []impl.Param{{Key: impl.KeyMandatory, Value: "tls-supported-groups"}},
	}
	if err := r.Validate(rr); !errors.Is(err, impl.ErrUnknownMandatory) {
		t.Fatalf("unknown mandatory key: err = %v, want ErrUnknownMandatory", err)
	}
}

func TestValidateRejectsDuplicateParam(t *testing.T) {
	r := impl.NewSvcbAliasResolver(svcbConfig(nil, nil))
	rr := impl.Record{
		Name:     "svc.example",
		Priority: 1,
		Target:   "svc.example",
		Params:   []impl.Param{alpnParam("h2"), alpnParam("h3")},
	}
	if err := r.Validate(rr); !errors.Is(err, impl.ErrDuplicateParam) {
		t.Fatalf("duplicate key: err = %v, want ErrDuplicateParam", err)
	}
}

func TestValidateRejectsBadPortAndMissingMandatory(t *testing.T) {
	r := impl.NewSvcbAliasResolver(svcbConfig(nil, nil))
	base := impl.Record{Name: "svc.example", Priority: 1, Target: "svc.example"}
	high := base
	high.Params = []impl.Param{{Key: impl.KeyPort, Value: "70000"}}
	if err := r.Validate(high); !errors.Is(err, impl.ErrBadPort) {
		t.Fatalf("port 70000: err = %v, want ErrBadPort", err)
	}
	word := base
	word.Params = []impl.Param{{Key: impl.KeyPort, Value: "https"}}
	if err := r.Validate(word); !errors.Is(err, impl.ErrBadPort) {
		t.Fatalf("non numeric port: err = %v, want ErrBadPort", err)
	}
	missing := base
	missing.Params = []impl.Param{{Key: impl.KeyMandatory, Value: "alpn"}}
	if err := r.Validate(missing); !errors.Is(err, impl.ErrMandatoryMissing) {
		t.Fatalf("mandatory without the key: err = %v, want ErrMandatoryMissing", err)
	}
}

func TestParamsDefaultsPortAndALPN(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"svc.example": {
			{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{alpnParam("h2,h3")}},
		},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	chain, err := r.Resolve("svc.example")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := r.Params(chain)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if params.Port != impl.DefaultHTTPSPort {
		t.Fatalf("port = %d, want %d", params.Port, impl.DefaultHTTPSPort)
	}
	if len(params.ALPN) != 2 || params.ALPN[0] != "h2" || params.ALPN[1] != "h3" {
		t.Fatalf("alpn = %+v, want [h2 h3]", params.ALPN)
	}
	if params.Name != "svc.example" || params.Target != "svc.example" {
		t.Fatalf("params identity = %+v", params)
	}
	if params.RecordCount != 1 {
		t.Fatalf("record count = %d, want 1", params.RecordCount)
	}
	if _, err := r.Params(impl.Chain{Name: "www.example", Mode: impl.ModeAlias}); !errors.Is(err, impl.ErrNoServiceRecord) {
		t.Fatalf("alias chain params: err = %v, want ErrNoServiceRecord", err)
	}
}

func TestParamsDedupesHints(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"svc.example": {
			{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{
				alpnParam("h2"),
				{Key: impl.KeyIPv4Hint, Value: "192.0.2.1, 192.0.2.1,192.0.2.2"},
				{Key: impl.KeyIPv6Hint, Value: "2001:db8::1,2001:db8::1"},
			}},
		},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	chain, err := r.Resolve("svc.example")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := r.Params(chain)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if len(params.IPv4Hints) != 2 || params.IPv4Hints[0] != "192.0.2.1" || params.IPv4Hints[1] != "192.0.2.2" {
		t.Fatalf("ipv4 hints = %+v, want deduplicated order preserving list", params.IPv4Hints)
	}
	if len(params.IPv6Hints) != 1 || params.IPv6Hints[0] != "2001:db8::1" {
		t.Fatalf("ipv6 hints = %+v, want one deduplicated entry", params.IPv6Hints)
	}
}

func TestParamsRejectsHintOverflow(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"svc.example": {
			{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{
				alpnParam("h2"),
				{Key: impl.KeyIPv4Hint, Value: "192.0.2.1,192.0.2.2,192.0.2.3,192.0.2.4,192.0.2.5"},
			}},
		},
	}
	cfg := svcbConfig(&clock, answers)
	cfg.MaxHints = 2
	r := impl.NewSvcbAliasResolver(cfg)
	chain, err := r.Resolve("svc.example")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := r.Params(chain); !errors.Is(err, impl.ErrHintLimit) {
		t.Fatalf("hint overflow: err = %v, want ErrHintLimit", err)
	}
}

func TestParamsRejectsNoDefaultAlpnWithoutAlpn(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"svc.example": {
			{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{
				{Key: impl.KeyPort, Value: "8443"},
				{Key: impl.KeyNoDefaultALPN, Value: ""},
			}},
		},
	}
	r := impl.NewSvcbAliasResolver(svcbConfig(&clock, answers))
	chain, err := r.Resolve("svc.example")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := r.Params(chain); !errors.Is(err, impl.ErrParamConflict) {
		t.Fatalf("no-default-alpn without alpn: err = %v, want ErrParamConflict", err)
	}
}

func TestCacheExpiresAndRefresh(t *testing.T) {
	clock := svcbBase
	lookups := 0
	answers := map[string][]impl.Record{
		"svc.example": {{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{alpnParam("h2")}}},
	}
	cfg := svcbConfig(&clock, answers)
	cfg.Lookup = func(name string) ([]impl.Record, error) {
		lookups++
		return answers[name], nil
	}
	r := impl.NewSvcbAliasResolver(cfg)
	if _, err := r.Resolve("svc.example"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := r.Resolve("svc.example"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1 while the chain is fresh", lookups)
	}
	if got := r.Stats().CacheHits; got != 1 {
		t.Fatalf("cache hits = %d, want 1", got)
	}
	clock = svcbBase.Add(6 * time.Minute)
	if _, ok := r.Cached("svc.example"); ok {
		t.Fatalf("chain must expire after its ttl")
	}
	if _, err := r.Resolve("svc.example"); err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if lookups != 2 {
		t.Fatalf("lookups = %d, want 2 after expiry", lookups)
	}
	if _, err := r.Refresh("svc.example"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if lookups != 3 {
		t.Fatalf("lookups = %d, want 3 after refresh", lookups)
	}
	if !r.Flush("svc.example") {
		t.Fatalf("flush of a cached name must report true")
	}
	if r.Flush("svc.example") {
		t.Fatalf("second flush must report false")
	}
	if _, ok := r.Cached("svc.example"); ok {
		t.Fatalf("flushed chain is still cached")
	}
}

func TestSnapshotRestoreKeepsChains(t *testing.T) {
	clock := svcbBase
	answers := map[string][]impl.Record{
		"svc.example": {
			{Name: "svc.example", Priority: 1, Target: "svc.example", Params: []impl.Param{alpnParam("h2,h3")}},
		},
	}
	cfg := svcbConfig(&clock, answers)
	r := impl.NewSvcbAliasResolver(cfg)
	if _, err := r.Resolve("svc.example"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewSvcbAliasResolver(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	chain, ok := restored.Cached("svc.example")
	if !ok {
		t.Fatalf("restored resolver lost the chain")
	}
	params, err := restored.Params(chain)
	if err != nil {
		t.Fatalf("params after restore: %v", err)
	}
	if params.Port != impl.DefaultHTTPSPort || len(params.ALPN) != 2 {
		t.Fatalf("params after restore = %+v", params)
	}
	if got := restored.Stats().Resolved; got != 1 {
		t.Fatalf("resolved counter after restore = %d, want 1", got)
	}
	if err := restored.Restore([]byte("broken payload")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt restore: err = %v, want ErrBadSnapshot", err)
	}
	if _, ok := restored.Cached("svc.example"); !ok {
		t.Fatalf("corrupt restore wiped the cache")
	}
}
