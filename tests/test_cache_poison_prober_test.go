package task

import (
	"errors"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-26"
)

// fakeTier simulates one cache layer. It reflects a fixed set of request
// headers into the response body, which is what makes a header influential.
type fakeTier struct {
	name     string
	baseline string
	reflect  []string

	mu     sync.Mutex
	stored *impl.Response
	purges int
}

func (f *fakeTier) Name() string { return f.name }

func (f *fakeTier) Fetch(headers map[string]string) (impl.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stored != nil {
		return *f.stored, nil
	}
	body := f.baseline
	for _, h := range f.reflect {
		if v, ok := headers[h]; ok {
			body += ";probe=" + v
		}
	}
	return impl.Response{Status: 200, Body: body}, nil
}

func (f *fakeTier) Store(resp impl.Response) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := resp
	f.stored = &copied
	return nil
}

func (f *fakeTier) Purge() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges++
	f.stored = nil
	return nil
}

func (f *fakeTier) purgeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.purges
}

func testLimits() impl.Limits {
	return impl.Limits{MaxProbesPerScope: 32, MaxConcurrentScopes: 4, MaxTickets: 8}
}

func testScope(id string) impl.Scope {
	return impl.Scope{
		ID:  id,
		Key: "/article/42",
		Tiers: []impl.CacheTier{
			&fakeTier{name: "edge", baseline: "article", reflect: []string{"x-tenant", "x-forwarded-host"}},
			&fakeTier{name: "mid", baseline: "article"},
			&fakeTier{name: "origin", baseline: "article"},
		},
		CandidateHeaders: []string{"user-agent", "x-tenant", "x-forwarded-host"},
		LowTraffic:       true,
	}
}

func testPayload(header, marker string) impl.Payload {
	return impl.Payload{Header: header, Value: "tenant-a", Marker: marker, Visible: false}
}

func TestInferKeyListsOnlyInfluentialHeaders(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	model, err := p.InferKey(testScope("shop"))
	if err != nil {
		t.Fatalf("infer key: %v", err)
	}
	if model.ScopeID != "shop" {
		t.Fatalf("model scope = %q, want shop", model.ScopeID)
	}
	want := []string{"x-forwarded-host", "x-tenant"}
	if len(model.Headers) != len(want) {
		t.Fatalf("influential headers = %v, want %v", model.Headers, want)
	}
	for i, h := range want {
		if model.Headers[i] != h {
			t.Fatalf("influential headers = %v, want the sorted list %v", model.Headers, want)
		}
	}
	if model.ProbesUsed != 4 {
		t.Fatalf("probes used = %d, want one baseline plus three candidates = 4", model.ProbesUsed)
	}
	if got := p.ProbesUsed("shop"); got != 4 {
		t.Fatalf("scope probe counter = %d, want 4", got)
	}
}

func TestInferKeyRejectsIncompleteScope(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	blank := testScope("   ")
	if _, err := p.InferKey(blank); !errors.Is(err, impl.ErrUnknownScope) {
		t.Fatalf("blank scope id must fail with ErrUnknownScope, got %v", err)
	}
	tierless := impl.Scope{ID: "lonely", Key: "/x", CandidateHeaders: []string{"host"}, LowTraffic: true}
	if _, err := p.InferKey(tierless); !errors.Is(err, impl.ErrUnknownScope) {
		t.Fatalf("scope without tiers must fail with ErrUnknownScope, got %v", err)
	}
}

func TestInferKeyEnforcesScopeLimit(t *testing.T) {
	p := impl.NewCachePoisonProber(impl.Limits{MaxProbesPerScope: 32, MaxConcurrentScopes: 1, MaxTickets: 8})
	if _, err := p.InferKey(testScope("alpha")); err != nil {
		t.Fatalf("first scope must be accepted, got %v", err)
	}
	if _, err := p.InferKey(testScope("beta")); !errors.Is(err, impl.ErrScopeBusy) {
		t.Fatalf("second tracked scope must fail with ErrScopeBusy, got %v", err)
	}
	if err := p.Release("alpha"); err != nil {
		t.Fatalf("release alpha: %v", err)
	}
	if _, err := p.InferKey(testScope("beta")); err != nil {
		t.Fatalf("a released slot must be reusable, got %v", err)
	}
}

func TestProbeHeaderBeforeInferenceFails(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	if _, err := p.ProbeHeader(testScope("shop"), "x-tenant"); !errors.Is(err, impl.ErrNotInferred) {
		t.Fatalf("probing an inferred-less scope must fail with ErrNotInferred, got %v", err)
	}
	if got := p.ProbesUsed("shop"); got != 0 {
		t.Fatalf("a rejected probe must not spend budget, counter = %d", got)
	}
}

func TestProbeHeaderRepeatDoesNotConsumeBudget(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	first, err := p.ProbeHeader(scope, "x-tenant")
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if !first.Influential {
		t.Fatalf("x-tenant changes the response and must be influential")
	}
	second, err := p.ProbeHeader(scope, "x-tenant")
	if err != nil {
		t.Fatalf("repeat probe: %v", err)
	}
	if second.Influential != first.Influential {
		t.Fatalf("repeat probe changed the influence: %v then %v", first.Influential, second.Influential)
	}
	if second.ProbesUsed != first.ProbesUsed {
		t.Fatalf("repeat probe spent more budget: %d then %d", first.ProbesUsed, second.ProbesUsed)
	}
}

func TestProbeHeaderRejectsBlankName(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	if _, err := p.ProbeHeader(scope, "   "); !errors.Is(err, impl.ErrBadHeader) {
		t.Fatalf("blank header must fail with ErrBadHeader, got %v", err)
	}
}

func TestPoisonRejectsEmptyMarker(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	if _, err := p.Poison(scope, impl.Payload{Header: "x-tenant", Value: "tenant-a"}); !errors.Is(err, impl.ErrBadPayload) {
		t.Fatalf("empty marker must fail with ErrBadPayload, got %v", err)
	}
	if got := len(p.Tickets("shop")); got != 0 {
		t.Fatalf("a rejected payload must not open a ticket, got %d", got)
	}
}

func TestPoisonRejectsUnprovenHeader(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	if _, err := p.Poison(scope, testPayload("x-unknown", "gsb-mk-1")); !errors.Is(err, impl.ErrHeaderNotInfluential) {
		t.Fatalf("unknown header must fail with ErrHeaderNotInfluential, got %v", err)
	}
	if _, err := p.Poison(scope, testPayload("user-agent", "gsb-mk-2")); !errors.Is(err, impl.ErrHeaderNotInfluential) {
		t.Fatalf("a header that does not change the response must be rejected, got %v", err)
	}
}

func TestPoisonAssignsMonotonicSeq(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	first, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-1"))
	if err != nil {
		t.Fatalf("first poison: %v", err)
	}
	second, err := p.Poison(scope, testPayload("x-forwarded-host", "gsb-mk-2"))
	if err != nil {
		t.Fatalf("second poison: %v", err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("ticket sequence = %d then %d, want 1 then 2", first.Seq, second.Seq)
	}
	if first.ID == "" || first.ID == second.ID {
		t.Fatalf("ticket ids must be unique and non empty, got %q and %q", first.ID, second.ID)
	}
	if first.State != impl.TicketPending {
		t.Fatalf("fresh ticket state = %q, want %q", first.State, impl.TicketPending)
	}
}

func TestPoisonEnforcesTicketCap(t *testing.T) {
	p := impl.NewCachePoisonProber(impl.Limits{MaxProbesPerScope: 32, MaxConcurrentScopes: 4, MaxTickets: 1})
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	if _, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-1")); err != nil {
		t.Fatalf("first poison: %v", err)
	}
	if _, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-2")); !errors.Is(err, impl.ErrTicketLimit) {
		t.Fatalf("second ticket must hit the cap with ErrTicketLimit, got %v", err)
	}
	if got := len(p.Tickets("shop")); got != 1 {
		t.Fatalf("ticket count = %d, want the cap 1", got)
	}
}

func TestVerifyReportsPoisonedTicket(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	ticket, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-1"))
	if err != nil {
		t.Fatalf("poison: %v", err)
	}
	verdict, err := p.Verify(ticket.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verdict.Poisoned {
		t.Fatalf("planted marker is served by the edge tier, verdict must be poisoned")
	}
	if !strings.Contains(verdict.Evidence, "edge") {
		t.Fatalf("evidence %q must name the tier that served the marker", verdict.Evidence)
	}
	if !strings.Contains(verdict.Evidence, "gsb-mk-1") {
		t.Fatalf("evidence %q must quote the marker", verdict.Evidence)
	}
}

func TestVerifyUnknownTicketFails(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	if _, err := p.Verify("ticket-404"); !errors.Is(err, impl.ErrUnknownTicket) {
		t.Fatalf("unknown ticket must fail with ErrUnknownTicket, got %v", err)
	}
	if _, err := p.Verify(""); !errors.Is(err, impl.ErrUnknownTicket) {
		t.Fatalf("empty ticket id must fail with ErrUnknownTicket, got %v", err)
	}
}

func TestPurgeClearsEveryTier(t *testing.T) {
	edge := &fakeTier{name: "edge", baseline: "article", reflect: []string{"x-tenant"}}
	mid := &fakeTier{name: "mid", baseline: "article"}
	origin := &fakeTier{name: "origin", baseline: "article"}
	scope := impl.Scope{
		ID:               "shop",
		Key:              "/article/42",
		Tiers:            []impl.CacheTier{edge, mid, origin},
		CandidateHeaders: []string{"x-tenant"},
		LowTraffic:       true,
	}
	p := impl.NewCachePoisonProber(testLimits())
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	ticket, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-9"))
	if err != nil {
		t.Fatalf("poison: %v", err)
	}
	if err := p.Purge(ticket.ID); err != nil {
		t.Fatalf("purge a healthy stack must succeed, got %v", err)
	}
	for _, tier := range []*fakeTier{edge, mid, origin} {
		if got := tier.purgeCount(); got != 1 {
			t.Fatalf("tier %s was purged %d times, want exactly 1", tier.name, got)
		}
	}
	verdict, err := p.Verify(ticket.ID)
	if err != nil {
		t.Fatalf("verify after purge: %v", err)
	}
	if verdict.Poisoned {
		t.Fatalf("purged ticket must not stay poisoned")
	}
	if verdict.Reason != "clean" {
		t.Fatalf("clean verdict reason = %q, want clean", verdict.Reason)
	}
	tickets := p.Tickets("shop")
	if len(tickets) != 1 || tickets[0].State != impl.TicketPurged {
		t.Fatalf("ticket state after purge = %+v, want one purged ticket", tickets)
	}
	if len(p.PendingPurge()) != 0 {
		t.Fatalf("a purged ticket must leave the pending queue")
	}
}

func TestSnapshotRestoreKeepsPendingPurge(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	ticket, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-7"))
	if err != nil {
		t.Fatalf("poison: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewCachePoisonProber(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := restored.Attach(scope); err != nil {
		t.Fatalf("attach after restore: %v", err)
	}
	pending := restored.PendingPurge()
	if len(pending) != 1 {
		t.Fatalf("restored pending queue = %+v, want the one unpurged ticket", pending)
	}
	if pending[0].ID != ticket.ID || pending[0].Seq != ticket.Seq {
		t.Fatalf("restored ticket = %+v, want id %q seq %d", pending[0], ticket.ID, ticket.Seq)
	}
	verdict, err := restored.Verify(ticket.ID)
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if !verdict.Poisoned {
		t.Fatalf("an unpurged ticket must still verify as poisoned after restore")
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	p := impl.NewCachePoisonProber(testLimits())
	scope := testScope("shop")
	if _, err := p.InferKey(scope); err != nil {
		t.Fatalf("infer key: %v", err)
	}
	ticket, err := p.Poison(scope, testPayload("x-tenant", "gsb-mk-5"))
	if err != nil {
		t.Fatalf("poison: %v", err)
	}
	if err := p.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := p.Tickets("shop"); len(got) != 1 || got[0].ID != ticket.ID {
		t.Fatalf("a rejected restore must not touch live tickets, got %+v", got)
	}
}
