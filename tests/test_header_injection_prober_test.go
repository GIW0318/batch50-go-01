package task

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-33"
)

func proberLimits() impl.Limits {
	return impl.Limits{
		MaxPoints:            4,
		MaxCasesPerPoint:     8,
		MaxRequestsPerWindow: 6,
		Window:               time.Minute,
	}
}

func testPoint() impl.HeaderPoint {
	return impl.HeaderPoint{
		ID:     "point-search",
		Method: "GET",
		Path:   "/search",
		Param:  "q",
		Header: "X-Search-Term",
	}
}

func plannedCases(t *testing.T) (*impl.HeaderInjectionProber, []impl.Case) {
	t.Helper()
	p := impl.NewHeaderInjectionProber(proberLimits())
	set, err := p.Plan(testPoint())
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	return p, set.Cases
}

func findSink(cases []impl.Case, sink string) (impl.Case, bool) {
	for _, c := range cases {
		if c.Sink == sink {
			return c, true
		}
	}
	return impl.Case{}, false
}

func TestPlanRejectsBlankPoint(t *testing.T) {
	p := impl.NewHeaderInjectionProber(proberLimits())
	if _, err := p.Plan(impl.HeaderPoint{Path: "/x", Param: "q", Header: "H"}); !errors.Is(err, impl.ErrBlankPoint) {
		t.Fatalf("point without an id must return ErrBlankPoint, got %v", err)
	}
	if _, err := p.Plan(impl.HeaderPoint{ID: "p", Param: "q", Header: "H"}); !errors.Is(err, impl.ErrBlankPoint) {
		t.Fatalf("point without a path must return ErrBlankPoint, got %v", err)
	}
	if _, err := p.Plan(impl.HeaderPoint{ID: "p", Path: "/x", Header: "H"}); !errors.Is(err, impl.ErrBlankPoint) {
		t.Fatalf("point without a param must return ErrBlankPoint, got %v", err)
	}
}

func TestPlanBuildsEightCases(t *testing.T) {
	_, cases := plannedCases(t)
	if len(cases) != 8 {
		t.Fatalf("case count = %d, want 8 (four encodings times two sinks)", len(cases))
	}
	seen := map[string]bool{}
	variants := map[string]int{}
	sinks := map[string]int{}
	for _, c := range cases {
		if c.ID == "" || c.PointID != "point-search" {
			t.Fatalf("case identity = %+v", c)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		variants[c.Variant]++
		sinks[c.Sink]++
		if c.Payload == "" {
			t.Fatalf("case %s has an empty payload", c.ID)
		}
	}
	for _, variant := range []string{impl.VariantCRLF, impl.VariantLF, impl.VariantDoublePercent, impl.VariantUTF16} {
		if variants[variant] != 2 {
			t.Fatalf("variant %q appears %d times, want 2", variant, variants[variant])
		}
	}
	if sinks[impl.SinkQuery] != 4 || sinks[impl.SinkHeader] != 4 {
		t.Fatalf("sink distribution = %v, want four query and four header cases", sinks)
	}
}

func TestPlanEnforcesPointLimit(t *testing.T) {
	limits := proberLimits()
	limits.MaxPoints = 1
	p := impl.NewHeaderInjectionProber(limits)
	if _, err := p.Plan(testPoint()); err != nil {
		t.Fatalf("first point must be planned, got %v", err)
	}
	second := testPoint()
	second.ID = "point-second"
	if _, err := p.Plan(second); !errors.Is(err, impl.ErrPointLimit) {
		t.Fatalf("second point must hit the point limit, got %v", err)
	}
	if points := p.Points(); len(points) != 1 || points[0] != "point-search" {
		t.Fatalf("planned points = %v, want only point-search", points)
	}
}

func TestPlanEnforcesCaseLimit(t *testing.T) {
	limits := proberLimits()
	limits.MaxCasesPerPoint = 4
	p := impl.NewHeaderInjectionProber(limits)
	if _, err := p.Plan(testPoint()); !errors.Is(err, impl.ErrCaseLimit) {
		t.Fatalf("case budget below the variant set must return ErrCaseLimit, got %v", err)
	}
	if points := p.Points(); len(points) != 0 {
		t.Fatalf("rejected plan must not register the point, got %v", points)
	}
}

func TestCraftUnknownCase(t *testing.T) {
	p := impl.NewHeaderInjectionProber(proberLimits())
	if _, err := p.Craft("no-such-case"); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("craft of an unknown case must return ErrUnknownCase, got %v", err)
	}
}

func TestCraftBuildsRequestPerSink(t *testing.T) {
	p, cases := plannedCases(t)
	queryCase, ok := findSink(cases, impl.SinkQuery)
	if !ok {
		t.Fatalf("planned cases must include a query sink")
	}
	headerCase, ok := findSink(cases, impl.SinkHeader)
	if !ok {
		t.Fatalf("planned cases must include a header sink")
	}
	queryReq, err := p.Craft(queryCase.ID)
	if err != nil {
		t.Fatalf("craft query case: %v", err)
	}
	if queryReq.CaseID != queryCase.ID || queryReq.Method != "GET" || queryReq.Path != "/search" {
		t.Fatalf("query request = %+v", queryReq)
	}
	if !strings.Contains(queryReq.Target, "q=") {
		t.Fatalf("query target = %q, want the payload placed in the q parameter", queryReq.Target)
	}
	if queryReq.Headers["X-Probe-Case"] != queryCase.ID {
		t.Fatalf("query request must tag the case, headers = %v", queryReq.Headers)
	}
	headerReq, err := p.Craft(headerCase.ID)
	if err != nil {
		t.Fatalf("craft header case: %v", err)
	}
	if headerReq.Headers["X-Search-Term"] == "" {
		t.Fatalf("header request must carry the payload in X-Search-Term, headers = %v", headerReq.Headers)
	}
	if headerReq.Headers["X-Probe-Case"] != headerCase.ID {
		t.Fatalf("header request must tag the case, headers = %v", headerReq.Headers)
	}
}

func TestSubmitBeforeCraft(t *testing.T) {
	p, cases := plannedCases(t)
	if err := p.Submit(cases[0].ID, impl.Response{Status: 200}); !errors.Is(err, impl.ErrCaseNotCrafted) {
		t.Fatalf("submit without a crafted request must return ErrCaseNotCrafted, got %v", err)
	}
}

func TestSubmitTwice(t *testing.T) {
	p, cases := plannedCases(t)
	caseID := cases[0].ID
	if _, err := p.Craft(caseID); err != nil {
		t.Fatalf("craft must succeed, got %v", err)
	}
	if err := p.Submit(caseID, impl.Response{Status: 200}); err != nil {
		t.Fatalf("first submit must succeed, got %v", err)
	}
	if err := p.Submit(caseID, impl.Response{Status: 500}); !errors.Is(err, impl.ErrCaseAlreadySubmitted) {
		t.Fatalf("second submit must return ErrCaseAlreadySubmitted, got %v", err)
	}
}

func TestJudgeBeforeSubmit(t *testing.T) {
	p, cases := plannedCases(t)
	caseID := cases[0].ID
	if _, err := p.Craft(caseID); err != nil {
		t.Fatalf("craft must succeed, got %v", err)
	}
	if _, err := p.Judge(caseID); !errors.Is(err, impl.ErrNotSubmitted) {
		t.Fatalf("judge before submit must return ErrNotSubmitted, got %v", err)
	}
}

func TestJudgeDetectsInjectedHeader(t *testing.T) {
	p, cases := plannedCases(t)
	caseID := cases[0].ID
	if _, err := p.Craft(caseID); err != nil {
		t.Fatalf("craft must succeed, got %v", err)
	}
	resp := impl.Response{
		Status: 200,
		Headers: []impl.Header{
			{Name: "Content-Type", Value: "text/html"},
			{Name: "X-Gsb-Probe", Value: "hit"},
		},
	}
	if err := p.Submit(caseID, resp); err != nil {
		t.Fatalf("submit must succeed, got %v", err)
	}
	outcome, err := p.Judge(caseID)
	if err != nil {
		t.Fatalf("judge must succeed, got %v", err)
	}
	if !outcome.Hit {
		t.Fatalf("outcome = %+v, want a hit from the injected response header", outcome)
	}
	if outcome.Location != impl.LocationHeader {
		t.Fatalf("location = %q, want %q", outcome.Location, impl.LocationHeader)
	}
	if !strings.EqualFold(outcome.Header, impl.ProbeHeader) {
		t.Fatalf("header name = %q, want %q", outcome.Header, impl.ProbeHeader)
	}
	if outcome.Value != "hit" {
		t.Fatalf("header value = %q, want hit", outcome.Value)
	}
}

func TestJudgeMissReturnsReason(t *testing.T) {
	p, cases := plannedCases(t)
	caseID := cases[1].ID
	if _, err := p.Craft(caseID); err != nil {
		t.Fatalf("craft must succeed, got %v", err)
	}
	if err := p.Submit(caseID, impl.Response{Status: 200, Headers: []impl.Header{{Name: "Content-Type", Value: "text/plain"}}, Body: "ok"}); err != nil {
		t.Fatalf("submit must succeed, got %v", err)
	}
	outcome, err := p.Judge(caseID)
	if err != nil {
		t.Fatalf("judge must succeed, got %v", err)
	}
	if outcome.Hit {
		t.Fatalf("outcome = %+v, want a miss for a clean response", outcome)
	}
	if outcome.Reason == "" {
		t.Fatalf("a miss must carry a reason code")
	}
	if outcome.CaseID != caseID {
		t.Fatalf("outcome case id = %q, want %q", outcome.CaseID, caseID)
	}
}

func TestCraftRateLimit(t *testing.T) {
	limits := proberLimits()
	limits.MaxRequestsPerWindow = 2
	limits.Window = time.Minute
	p := impl.NewHeaderInjectionProber(limits)
	now := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	p.SetClock(func() time.Time { return now })
	cases, err := p.Plan(testPoint())
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.Craft(cases.Cases[i].ID); err != nil {
			t.Fatalf("craft %d within the window must succeed, got %v", i, err)
		}
	}
	if _, err := p.Craft(cases.Cases[2].ID); !errors.Is(err, impl.ErrRateLimit) {
		t.Fatalf("third craft within the window must return ErrRateLimit, got %v", err)
	}
	now = now.Add(61 * time.Second)
	if _, err := p.Craft(cases.Cases[2].ID); err != nil {
		t.Fatalf("craft after the window must succeed, got %v", err)
	}
}

func TestCleanupCancelsCase(t *testing.T) {
	p, cases := plannedCases(t)
	caseID := cases[0].ID
	if _, err := p.Craft(caseID); err != nil {
		t.Fatalf("craft must succeed, got %v", err)
	}
	if err := p.Submit(caseID, impl.Response{Status: 200}); err != nil {
		t.Fatalf("submit must succeed, got %v", err)
	}
	if err := p.Cleanup(caseID); err != nil {
		t.Fatalf("cleanup must succeed, got %v", err)
	}
	if _, err := p.Judge(caseID); !errors.Is(err, impl.ErrCaseCancelled) {
		t.Fatalf("judge after cleanup must return ErrCaseCancelled, got %v", err)
	}
	if err := p.Cleanup(caseID); err != nil {
		t.Fatalf("repeated cleanup must stay successful, got %v", err)
	}
	if err := p.Cleanup("no-such-case"); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("cleanup of an unknown case must return ErrUnknownCase, got %v", err)
	}
}

func TestSnapshotRestoreProgress(t *testing.T) {
	p, cases := plannedCases(t)
	caseID := cases[0].ID
	if _, err := p.Craft(caseID); err != nil {
		t.Fatalf("craft must succeed, got %v", err)
	}
	if err := p.Submit(caseID, impl.Response{Status: 200, Headers: []impl.Header{{Name: "x-gsb-probe", Value: "yes"}}}); err != nil {
		t.Fatalf("submit must succeed, got %v", err)
	}
	blob, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewHeaderInjectionProber(proberLimits())
	if err := restored.Restore(blob); err != nil {
		t.Fatalf("restore must accept its own snapshot, got %v", err)
	}
	outcome, err := restored.Judge(caseID)
	if err != nil {
		t.Fatalf("judge after restore must succeed, got %v", err)
	}
	if !outcome.Hit || outcome.Value != "yes" {
		t.Fatalf("restored outcome = %+v, want the stored hit", outcome)
	}
	submitted, total := restored.Progress("point-search")
	if submitted != 1 || total != 8 {
		t.Fatalf("restored progress = %d/%d, want 1/8", submitted, total)
	}
	if err := restored.Restore([]byte("not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if submitted, total := restored.Progress("point-search"); submitted != 1 || total != 8 {
		t.Fatalf("failed restore must keep progress, got %d/%d", submitted, total)
	}
}

func TestConcurrentCraftRespectsRateLimit(t *testing.T) {
	limits := proberLimits()
	limits.MaxRequestsPerWindow = 3
	limits.Window = time.Minute
	p := impl.NewHeaderInjectionProber(limits)
	now := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	p.SetClock(func() time.Time { return now })
	set, err := p.Plan(testPoint())
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	crafted, limited := 0, 0
	for _, c := range set.Cases {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := p.Craft(id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				crafted++
			case errors.Is(err, impl.ErrRateLimit):
				limited++
			}
		}(c.ID)
	}
	wg.Wait()
	if crafted != 3 {
		t.Fatalf("crafted requests = %d, want exactly the window cap 3", crafted)
	}
	if limited != 5 {
		t.Fatalf("rate limited requests = %d, want 5", limited)
	}
}
