package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-a-01"
)

func mustEngine(limits impl.Limits) *impl.ScimFilterEngine {
	e := impl.NewScimFilterEngine(limits)
	schemas := []impl.AttrSchema{
		{Path: "userName", Kind: impl.KindString, CaseExact: true},
		{Path: "displayName", Kind: impl.KindString, CaseExact: false},
		{Path: "quota", Kind: impl.KindNumber},
		{Path: "active", Kind: impl.KindBool},
		{Path: "emails", Sub: "value", Kind: impl.KindString, CaseExact: false},
	}
	for _, s := range schemas {
		_ = e.RegisterSchema(s)
	}
	return e
}

func strValue(s string) impl.Value {
	return impl.Value{Kind: impl.KindString, Str: s}
}

func numValue(n float64) impl.Value {
	return impl.Value{Kind: impl.KindNumber, Num: n}
}

func userResource(id string, attrs map[string][]impl.Value) impl.Resource {
	return impl.Resource{ID: id, Kind: "User", Attrs: attrs}
}

func pageIDs(page impl.QueryPage) []string {
	ids := make([]string, 0, len(page.Resources))
	for _, r := range page.Resources {
		ids = append(ids, r.ID)
	}
	return ids
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestParseRejectsMalformedFilters(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	for _, filter := range []string{"", "   ", "(userName eq \"a\"", "userName eq", "userName eq \"a\") and"} {
		if _, err := e.Parse(filter); !errors.Is(err, impl.ErrBadFilter) {
			t.Fatalf("filter %q must be rejected with ErrBadFilter, got %v", filter, err)
		}
	}
}

func TestExplainListsSortedAttributeReferences(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	expr, err := e.Parse(`userName eq "u-1" and emails.value co "corp" and quota gt 3`)
	if err != nil {
		t.Fatalf("parse filter: %v", err)
	}
	refs := e.Explain(expr)
	want := []impl.AttrRef{
		{Path: "emails", Sub: "value"},
		{Path: "quota"},
		{Path: "userName"},
	}
	if len(refs) != len(want) {
		t.Fatalf("explained references = %+v, want %+v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("explained reference %d = %+v, want %+v", i, refs[i], want[i])
		}
	}
	wide, err := e.Parse(`(userName eq "u-1" or userName sw "u-") and quota le 9`)
	if err != nil {
		t.Fatalf("parse second filter: %v", err)
	}
	if got := e.Explain(wide); len(got) != 2 {
		t.Fatalf("duplicate paths must collapse, explained references = %+v", got)
	}
}

func TestMatchEqUsesCaseExactSchema(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	expr, err := e.Parse(`userName eq "Alice"`)
	if err != nil {
		t.Fatalf("parse filter: %v", err)
	}
	res := userResource("u-1", map[string][]impl.Value{
		"userName": {strValue("Alice")},
	})
	ok, err := e.Match(expr, res)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !ok {
		t.Fatalf("case exact attribute must match the identical value")
	}
	res.Attrs["userName"] = []impl.Value{strValue("alice")}
	ok, err = e.Match(expr, res)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ok {
		t.Fatalf("case exact attribute must not match a differently cased value")
	}
}

func TestMatchEqIgnoresCaseWhenSchemaAllows(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	expr, err := e.Parse(`displayName eq "Zhang San"`)
	if err != nil {
		t.Fatalf("parse filter: %v", err)
	}
	res := userResource("u-2", map[string][]impl.Value{
		"displayName": {strValue("zhang san")},
	})
	ok, err := e.Match(expr, res)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !ok {
		t.Fatalf("case insensitive attribute must match regardless of case")
	}
	exact, err := e.Parse(`userName eq "ZHANG SAN"`)
	if err != nil {
		t.Fatalf("parse filter: %v", err)
	}
	res.Attrs["userName"] = []impl.Value{strValue("zhang san")}
	ok, err = e.Match(exact, res)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ok {
		t.Fatalf("case exact attribute must stay case sensitive for eq")
	}
}

func TestMatchNumericComparisons(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	res := userResource("u-3", map[string][]impl.Value{
		"quota": {numValue(10)},
	})
	cases := []struct {
		filter string
		want   bool
	}{
		{`quota gt 7`, true},
		{`quota ge 10`, true},
		{`quota lt 10`, false},
		{`quota le 9`, false},
		{`quota le 10`, true},
	}
	for _, c := range cases {
		expr, err := e.Parse(c.filter)
		if err != nil {
			t.Fatalf("parse %q: %v", c.filter, err)
		}
		ok, err := e.Match(expr, res)
		if err != nil {
			t.Fatalf("match %q: %v", c.filter, err)
		}
		if ok != c.want {
			t.Fatalf("match %q = %v, want %v", c.filter, ok, c.want)
		}
	}
}

func TestMatchNotBindsTighterThanAndOr(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	res := userResource("u-4", map[string][]impl.Value{
		"quota":       {numValue(10)},
		"displayName": {strValue("ACME Corp")},
		"userName":    {strValue("u-4")},
	})
	grouped, err := e.Parse(`not (quota lt 5) and (displayName sw "ACME" or userName eq "nobody")`)
	if err != nil {
		t.Fatalf("parse grouped filter: %v", err)
	}
	ok, err := e.Match(grouped, res)
	if err != nil {
		t.Fatalf("match grouped filter: %v", err)
	}
	if !ok {
		t.Fatalf("grouped filter must evaluate to true")
	}
	plain, err := e.Parse(`not quota lt 5 and displayName ew "zzz"`)
	if err != nil {
		t.Fatalf("parse ungrouped filter: %v", err)
	}
	ok, err = e.Match(plain, res)
	if err != nil {
		t.Fatalf("match ungrouped filter: %v", err)
	}
	if ok {
		t.Fatalf("not must bind to the single following condition, so the filter is false")
	}
}

func TestQueryFiltersAndSortsStably(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	quotas := map[string]float64{"u-1": 30, "u-2": 10, "u-3": 20, "u-4": 20}
	for _, id := range []string{"u-1", "u-2", "u-3", "u-4"} {
		res := userResource(id, map[string][]impl.Value{"quota": {numValue(quotas[id])}})
		if err := e.Put(res); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 20`, SortBy: "quota", PageSize: 10})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := pageIDs(page); !sameIDs(got, []string{"u-3", "u-4", "u-1"}) {
		t.Fatalf("ascending order = %v, want [u-3 u-4 u-1]", got)
	}
	if page.Total != 3 {
		t.Fatalf("total = %d, want 3", page.Total)
	}
	page, err = e.Query("User", impl.QueryRequest{Filter: `quota ge 20`, SortBy: "quota", Descending: true, PageSize: 10})
	if err != nil {
		t.Fatalf("descending query: %v", err)
	}
	if got := pageIDs(page); !sameIDs(got, []string{"u-1", "u-4", "u-3"}) {
		t.Fatalf("descending order = %v, want [u-1 u-4 u-3]", got)
	}
}

func TestQueryClampsPageSizeToCap(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 3, MaxResults: 100})
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"} {
		if err := e.Put(userResource(id, map[string][]impl.Value{"quota": {numValue(1)}})); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Resources) != 3 {
		t.Fatalf("page holds %d resources, want the cap 3", len(page.Resources))
	}
	if page.Total != 7 {
		t.Fatalf("total = %d, want 7 even though the page is clamped", page.Total)
	}
	if page.NextCursor == "" {
		t.Fatalf("a clamped page must hand out a next cursor")
	}
}

func TestQueryPaginatesCursorWithoutDuplicates(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 4, MaxResults: 100})
	want := []string{"r1", "r2", "r3", "r4", "r5"}
	for _, id := range want {
		if err := e.Put(userResource(id, map[string][]impl.Value{"quota": {numValue(1)}})); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	var got []string
	cursor := ""
	for step := 0; step < 5; step++ {
		page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", step, err)
		}
		if page.Total != len(want) {
			t.Fatalf("page %d total = %d, want %d", step, page.Total, len(want))
		}
		got = append(got, pageIDs(page)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !sameIDs(got, want) {
		t.Fatalf("paged ids = %v, want %v without duplicates", got, want)
	}
}

func TestQueryRejectsTamperedCursor(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 2, MaxResults: 100})
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := e.Put(userResource(id, map[string][]impl.Value{"quota": {numValue(1)}})); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatalf("first page must hand out a cursor")
	}
	tampered := page.NextCursor + "zz"
	_, err = e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 2, Cursor: tampered})
	if !errors.Is(err, impl.ErrCursorInvalid) {
		t.Fatalf("altered cursor must fail with ErrCursorInvalid, got %v", err)
	}
}

func TestQueryRejectsCursorFromAnotherFilter(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 2, MaxResults: 100})
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		attrs := map[string][]impl.Value{"quota": {numValue(1)}}
		if id == "r4" {
			attrs = map[string][]impl.Value{"quota": {numValue(9)}}
		}
		if err := e.Put(userResource(id, attrs)); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	_, err = e.Query("User", impl.QueryRequest{Filter: `quota ge 9`, PageSize: 2, Cursor: page.NextCursor})
	if !errors.Is(err, impl.ErrCursorMismatch) {
		t.Fatalf("a cursor from another expression must fail with ErrCursorMismatch, got %v", err)
	}
}

func TestQueryEnforcesResultLimit(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 4})
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
		if err := e.Put(userResource(id, map[string][]impl.Value{"quota": {numValue(2)}})); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	if _, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 2}); !errors.Is(err, impl.ErrResultLimit) {
		t.Fatalf("six matches against a limit of four must fail with ErrResultLimit, got %v", err)
	}
	page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 5`, PageSize: 2})
	if err != nil {
		t.Fatalf("empty result must not be rejected, got %v", err)
	}
	if page.Total != 0 || len(page.Resources) != 0 {
		t.Fatalf("empty result carried total=%d resources=%d", page.Total, len(page.Resources))
	}
}

func TestConcurrentQueriesShareCompiledFilter(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 5, MaxResults: 100})
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		if err := e.Put(userResource(id, map[string][]impl.Value{"quota": {numValue(3)}})); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	counts := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			expr, err := e.Parse(`quota ge 1 and userName eq "none"`)
			if err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
				return
			}
			_ = expr
			page, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 5})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			counts[slot] = len(page.Resources)
		}(i)
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("concurrent queries reported %d errors, first %v", len(failures), failures[0])
	}
	for slot, n := range counts {
		if n != 5 {
			t.Fatalf("worker %d saw %d resources, want 5", slot, n)
		}
	}
	if stats := e.CacheStats(); stats.Entries != 1 {
		t.Fatalf("compiled cache holds %d entries for one filter, want 1", stats.Entries)
	}
}

func TestSnapshotRestoreKeepsResourcesAndTombstones(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := e.Put(userResource(id, map[string][]impl.Value{"quota": {numValue(4)}})); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	if err := e.Delete("User", "r2"); err != nil {
		t.Fatalf("delete r2: %v", err)
	}
	data, err := e.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewScimFilterEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, ok := restored.Get("User", "r1"); !ok {
		t.Fatalf("restored engine lost r1")
	}
	if _, ok := restored.Get("User", "r2"); ok {
		t.Fatalf("restored engine resurrected the deleted r2")
	}
	page, err := restored.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 10})
	if err != nil {
		t.Fatalf("query after restore: %v", err)
	}
	if got := pageIDs(page); !sameIDs(got, []string{"r1", "r3"}) {
		t.Fatalf("restored query ids = %v, want [r1 r3]", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	e := mustEngine(impl.Limits{MaxCacheEntries: 8, MaxPageSize: 10, MaxResults: 100})
	if err := e.Put(userResource("r1", map[string][]impl.Value{"quota": {numValue(1)}})); err != nil {
		t.Fatalf("put r1: %v", err)
	}
	if err := e.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := e.Get("User", "r1"); !ok {
		t.Fatalf("a rejected restore must not touch the live resource table")
	}
	if _, err := e.Query("User", impl.QueryRequest{Filter: `quota ge 1`, PageSize: 10}); err != nil {
		t.Fatalf("the live engine must keep answering after a rejected restore, got %v", err)
	}
}
