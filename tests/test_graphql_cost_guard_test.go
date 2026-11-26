package task

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-08"
)

func costLimits() impl.Limits {
	return impl.Limits{
		MaxDepth:          6,
		MaxFields:         20,
		MaxCost:           1000,
		TenantBudget:      1000,
		WindowLength:      time.Minute,
		FieldCost:         1,
		MaxListMultiplier: 100,
		IntrospectionCost: 50,
		CacheSize:         32,
		ParseTimeout:      2 * time.Second,
	}
}

func mustPlan(t *testing.T, g *impl.GraphqlCostGuard, query string, vars map[string]any) impl.Plan {
	t.Helper()
	plan, err := g.Analyze(query, vars)
	if err != nil {
		t.Fatalf("analyze %q: %v", query, err)
	}
	return plan
}

func TestAnalyzeCountsFieldsDepthAndCost(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	nested := mustPlan(t, g, "{a{b{c}}}", nil)
	if nested.Depth != 3 {
		t.Fatalf("depth of {a{b{c}}} = %d, want 3", nested.Depth)
	}
	if nested.Fields != 3 {
		t.Fatalf("field count of {a{b{c}}} = %d, want 3", nested.Fields)
	}
	if nested.Cost != 3 {
		t.Fatalf("cost of {a{b{c}}} = %d, want 3", nested.Cost)
	}
	aliased := mustPlan(t, g, "{ x: user { id } y: user { id } }", nil)
	if aliased.Depth != 2 {
		t.Fatalf("depth of the aliased document = %d, want 2", aliased.Depth)
	}
	if aliased.Fields != 4 {
		t.Fatalf("field count of the aliased document = %d, want 4", aliased.Fields)
	}
	if aliased.Cost != 4 {
		t.Fatalf("cost of the aliased document = %d, want 4", aliased.Cost)
	}
}

func TestAnalyzeAppliesListMultiplier(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	plan := mustPlan(t, g, "{ users(first: 5) { id name } }", nil)
	if plan.Cost != 11 {
		t.Fatalf("cost with first:5 = %d, want 1 + 5*2 = 11", plan.Cost)
	}
	if plan.Fields != 3 || plan.Depth != 2 {
		t.Fatalf("plan fields=%d depth=%d, want 3 fields at depth 2", plan.Fields, plan.Depth)
	}
	clamped := mustPlan(t, g, "{ users(first: 999) { id } }", nil)
	if clamped.Cost != 101 {
		t.Fatalf("cost with first:999 = %d, want the multiplier clamped to 100 plus the parent field", clamped.Cost)
	}
}

func TestAnalyzeRejectsDepthLimit(t *testing.T) {
	limits := costLimits()
	limits.MaxDepth = 4
	g := impl.NewGraphqlCostGuard(limits)
	if _, err := g.Analyze("{a{b{c{d{e}}}}}", nil); !errors.Is(err, impl.ErrDepthLimit) {
		t.Fatalf("five level document must fail with ErrDepthLimit, got %v", err)
	}
	ok := mustPlan(t, g, "{a{b{c{d}}}}", nil)
	if ok.Depth != 4 {
		t.Fatalf("four level document depth = %d, want 4", ok.Depth)
	}
}

func TestAnalyzeRejectsFieldLimit(t *testing.T) {
	limits := costLimits()
	limits.MaxFields = 4
	g := impl.NewGraphqlCostGuard(limits)
	if _, err := g.Analyze("{a b c d e}", nil); !errors.Is(err, impl.ErrFieldLimit) {
		t.Fatalf("five fields must fail with ErrFieldLimit, got %v", err)
	}
	ok := mustPlan(t, g, "{a b c d}", nil)
	if ok.Fields != 4 {
		t.Fatalf("field count = %d, want 4", ok.Fields)
	}
}

func TestAnalyzeExpandsFragments(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	plan := mustPlan(t, g, "query { ...F } fragment F on Query { a b }", nil)
	if plan.Fields != 2 || plan.Cost != 2 {
		t.Fatalf("fragment plan fields=%d cost=%d, want 2 and 2", plan.Fields, plan.Cost)
	}
	inline := mustPlan(t, g, "query { ... on Query { a { id } } }", nil)
	if inline.Fields != 2 || inline.Cost != 2 {
		t.Fatalf("inline fragment plan fields=%d cost=%d, want 2 and 2", inline.Fields, inline.Cost)
	}
}

func TestAnalyzeRejectsFragmentCycleAndUnknownSpread(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	cycle := "fragment A on Query { ...B } fragment B on Query { ...A } query { ...A }"
	if _, err := g.Analyze(cycle, nil); !errors.Is(err, impl.ErrFragmentCycle) {
		t.Fatalf("cyclic fragments must fail with ErrFragmentCycle, got %v", err)
	}
	if _, err := g.Analyze("query { ...Missing }", nil); !errors.Is(err, impl.ErrUnknownFragment) {
		t.Fatalf("unknown fragment must fail with ErrUnknownFragment, got %v", err)
	}
}

func TestAnalyzeRejectsMalformedDocument(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	for _, query := range []string{"", "   ", "{ a ", "a b", "{ a { } }"} {
		if _, err := g.Analyze(query, nil); !errors.Is(err, impl.ErrBadQuery) {
			t.Fatalf("document %q must fail with ErrBadQuery, got %v", query, err)
		}
	}
}

func TestAnalyzeHashIncludesVariables(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	first := mustPlan(t, g, "query Ping { a }", map[string]any{"x": 1})
	if len(first.QueryHash) != 64 {
		t.Fatalf("query hash length = %d, want 64 hex characters", len(first.QueryHash))
	}
	if first.QueryHash != strings.ToLower(first.QueryHash) {
		t.Fatalf("query hash must be lower case hex, got %q", first.QueryHash)
	}
	same := mustPlan(t, g, "query Ping { a }", map[string]any{"x": 1})
	if same.QueryHash != first.QueryHash {
		t.Fatalf("identical query and variables produced different hashes")
	}
	otherVars := mustPlan(t, g, "query Ping { a }", map[string]any{"x": 2})
	if otherVars.QueryHash == first.QueryHash {
		t.Fatalf("different variable values must change the query fingerprint")
	}
	otherQuery := mustPlan(t, g, "query Ping { b }", map[string]any{"x": 1})
	if otherQuery.QueryHash == first.QueryHash {
		t.Fatalf("different documents must not share a query fingerprint")
	}
}

func TestAnalyzeCacheReturnsIsolatedPlans(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	first := mustPlan(t, g, "{a{b}}", nil)
	if first.Cached {
		t.Fatalf("first analysis must not report a cache hit")
	}
	second := mustPlan(t, g, "{a{b}}", nil)
	if !second.Cached {
		t.Fatalf("repeated analysis must report a cache hit")
	}
	if second.Fields != 2 || second.Cost != 2 {
		t.Fatalf("cached plan fields=%d cost=%d, want 2 and 2", second.Fields, second.Cost)
	}
	first.Fields = 99
	first.Cost = 99
	third := mustPlan(t, g, "{a{b}}", nil)
	if third.Fields != 2 || third.Cost != 2 {
		t.Fatalf("mutating a returned plan must not disturb the cache, got fields=%d cost=%d", third.Fields, third.Cost)
	}
	stats := g.CacheStats()
	if stats.Hits != 2 || stats.Misses != 1 {
		t.Fatalf("cache stats = %+v, want 2 hits and 1 miss", stats)
	}
}

func TestAdmitChargesTenantWindow(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	plan := mustPlan(t, g, "{a b}", nil)
	decision, err := g.Admit("tenant-1", plan)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("admit of a cheap plan must be allowed, got %+v", decision)
	}
	if decision.Cost != 2 || decision.Remaining != 998 {
		t.Fatalf("decision = %+v, want cost 2 and 998 remaining", decision)
	}
	if decision.Reason != impl.ReasonAllowed {
		t.Fatalf("decision reason = %q, want %q", decision.Reason, impl.ReasonAllowed)
	}
	if err := g.Charge("tenant-1", 8); err != nil {
		t.Fatalf("charge: %v", err)
	}
	window := g.Usage("tenant-1")
	if window.Used != 10 || window.Limit != 1000 || window.Remaining != 990 {
		t.Fatalf("window = %+v, want 10 used of 1000", window)
	}
	if other := g.Usage("tenant-2"); other.Used != 0 {
		t.Fatalf("unrelated tenant usage = %d, want 0", other.Used)
	}
}

func TestAdmitRejectsInvalidInput(t *testing.T) {
	g := impl.NewGraphqlCostGuard(costLimits())
	plan := mustPlan(t, g, "{a}", nil)
	if _, err := g.Admit("", plan); !errors.Is(err, impl.ErrBadTenant) {
		t.Fatalf("blank tenant must fail with ErrBadTenant, got %v", err)
	}
	if err := g.Charge("tenant-1", 0); !errors.Is(err, impl.ErrBadCost) {
		t.Fatalf("zero charge must fail with ErrBadCost, got %v", err)
	}
	if err := g.Charge("tenant-1", -3); !errors.Is(err, impl.ErrBadCost) {
		t.Fatalf("negative charge must fail with ErrBadCost, got %v", err)
	}
	if err := g.Refund("tenant-1", 1); !errors.Is(err, impl.ErrRefundExceeds) {
		t.Fatalf("refund without a charge must fail with ErrRefundExceeds, got %v", err)
	}
	if window := g.Usage("tenant-1"); window.Used != 0 {
		t.Fatalf("rejected charges must not move the window, got %d", window.Used)
	}
}

func TestAdmitRejectsOverSingleQueryBudget(t *testing.T) {
	limits := costLimits()
	limits.MaxCost = 10
	g := impl.NewGraphqlCostGuard(limits)
	plan := mustPlan(t, g, "{ users(first: 5) { id name } }", nil)
	decision, err := g.Admit("tenant-3", plan)
	if err != nil {
		t.Fatalf("admit must not return an error for a policy rejection, got %v", err)
	}
	if decision.Allowed {
		t.Fatalf("plan above the single query budget must be rejected")
	}
	if decision.Reason != impl.ReasonCostLimit {
		t.Fatalf("rejection reason = %q, want %q", decision.Reason, impl.ReasonCostLimit)
	}
	if window := g.Usage("tenant-3"); window.Used != 0 {
		t.Fatalf("rejected plan must not be charged, got %d used", window.Used)
	}
}

func TestAdmitRejectsExhaustedBudgetAndRefundRestores(t *testing.T) {
	limits := costLimits()
	limits.TenantBudget = 50
	g := impl.NewGraphqlCostGuard(limits)
	heavy := mustPlan(t, g, "{ users(first: 29) { id } }", nil)
	if heavy.Cost != 30 {
		t.Fatalf("plan cost = %d, want 30", heavy.Cost)
	}
	light := mustPlan(t, g, "{ users(first: 24) { id } }", nil)
	if light.Cost != 25 {
		t.Fatalf("plan cost = %d, want 25", light.Cost)
	}
	first, err := g.Admit("tenant-4", heavy)
	if err != nil || !first.Allowed {
		t.Fatalf("first admit must be allowed, got %+v err=%v", first, err)
	}
	if first.Remaining != 20 {
		t.Fatalf("remaining after the first admit = %d, want 20", first.Remaining)
	}
	second, err := g.Admit("tenant-4", light)
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if second.Allowed || second.Reason != impl.ReasonBudgetExhausted {
		t.Fatalf("second admit must be rejected for the budget, got %+v", second)
	}
	if window := g.Usage("tenant-4"); window.Used != 30 {
		t.Fatalf("usage after the rejected admit = %d, want 30", window.Used)
	}
	if err := g.Refund("tenant-4", 30); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if window := g.Usage("tenant-4"); window.Used != 0 {
		t.Fatalf("usage after refund = %d, want 0", window.Used)
	}
	third, err := g.Admit("tenant-4", light)
	if err != nil || !third.Allowed {
		t.Fatalf("admit after refund must be allowed, got %+v err=%v", third, err)
	}
	if third.Remaining != 25 {
		t.Fatalf("remaining after the refunded admit = %d, want 25", third.Remaining)
	}
}

func TestConcurrentAdmitNeverOversells(t *testing.T) {
	limits := costLimits()
	limits.TenantBudget = 100
	g := impl.NewGraphqlCostGuard(limits)
	plan := mustPlan(t, g, "{a b c d e}", nil)
	if plan.Cost != 5 {
		t.Fatalf("plan cost = %d, want 5", plan.Cost)
	}
	var allowed, rejected atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := g.Admit("tenant-5", plan)
			if err != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			} else {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent admits returned an unexpected error", failures.Load())
	}
	if allowed.Load() != 20 {
		t.Fatalf("allowed admits = %d, want exactly the budget divided by the cost", allowed.Load())
	}
	if rejected.Load() != 20 {
		t.Fatalf("rejected admits = %d, want 20", rejected.Load())
	}
	if window := g.Usage("tenant-5"); window.Used != 100 || window.Remaining != 0 {
		t.Fatalf("window after the race = %+v, want the budget fully used", window)
	}
}

func TestSlidingWindowTickExpiresOldCharges(t *testing.T) {
	limits := costLimits()
	limits.WindowLength = time.Minute
	g := impl.NewGraphqlCostGuard(limits)
	t0 := time.Now()
	if n := g.Tick(t0); n != 0 {
		t.Fatalf("first tick expired %d charges, want 0", n)
	}
	if err := g.Charge("tenant-6", 5); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if n := g.Tick(t0.Add(30 * time.Second)); n != 0 {
		t.Fatalf("tick inside the window expired %d charges, want 0", n)
	}
	if err := g.Charge("tenant-6", 3); err != nil {
		t.Fatalf("second charge: %v", err)
	}
	if window := g.Usage("tenant-6"); window.Used != 8 {
		t.Fatalf("usage before expiry = %d, want 8", window.Used)
	}
	if n := g.Tick(t0.Add(61 * time.Second)); n != 1 {
		t.Fatalf("tick after the window expired %d charges, want only the first one", n)
	}
	window := g.Usage("tenant-6")
	if window.Used != 3 {
		t.Fatalf("usage after the first charge expired = %d, want 3", window.Used)
	}
	if window.Remaining != 997 {
		t.Fatalf("remaining after expiry = %d, want 997", window.Remaining)
	}
}
