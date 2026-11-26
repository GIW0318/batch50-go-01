package task

import (
	"errors"
	"math"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-04"
)

func ksmLimits() impl.Limits {
	return impl.Limits{
		MaxPagesPerNode: 32,
		MinScanInterval: time.Second,
		StatsWindow:     time.Hour,
		MaxAlerts:       16,
	}
}

func ksmPolicy() impl.KsmPolicy {
	return impl.KsmPolicy{
		Enabled:          true,
		ScanInterval:     5 * time.Second,
		MaxPages:         16,
		AlertDedupeRatio: 95,
		AlertWALatencyMS: 500,
		Retention:        time.Hour,
	}
}

func page(id, tenant, hash string) impl.Page {
	return impl.Page{
		ID:          id,
		Tenant:      tenant,
		Context:     "ctx-a",
		MountTenant: tenant,
		Hash:        hash,
		SizeKB:      4,
		Anon:        true,
		WriteRate:   100,
	}
}

func baseTime() time.Time {
	return time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
}

func TestEnableRejectsBadTenant(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("", ksmPolicy()); !errors.Is(err, impl.ErrBadTenant) {
		t.Fatalf("empty tenant err = %v, want ErrBadTenant", err)
	}
	if err := g.Enable("   ", ksmPolicy()); !errors.Is(err, impl.ErrBadTenant) {
		t.Fatalf("blank tenant err = %v, want ErrBadTenant", err)
	}
	if got := len(g.StatsAll()); got != 0 {
		t.Fatalf("rejected tenants must not be registered, stats = %d", got)
	}
}

func TestEnableRejectsShortScanInterval(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	policy := ksmPolicy()
	policy.ScanInterval = 500 * time.Millisecond
	if err := g.Enable("t-1", policy); !errors.Is(err, impl.ErrScanInterval) {
		t.Fatalf("short interval err = %v, want ErrScanInterval", err)
	}
	policy.ScanInterval = time.Second
	if err := g.Enable("t-1", policy); err != nil {
		t.Fatalf("interval at the floor must be accepted, got %v", err)
	}
}

func TestEnableRejectsPolicyOverLimit(t *testing.T) {
	limits := ksmLimits()
	limits.MaxPagesPerNode = 8
	g := impl.NewKsmTenantGuard(limits)
	policy := ksmPolicy()
	policy.MaxPages = 9
	if err := g.Enable("t-1", policy); !errors.Is(err, impl.ErrPageLimit) {
		t.Fatalf("tenant page cap over the node cap err = %v, want ErrPageLimit", err)
	}
	policy = ksmPolicy()
	policy.AlertDedupeRatio = 140
	if err := g.Enable("t-1", policy); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("out of range ratio err = %v, want ErrBadPolicy", err)
	}
}

func TestTrackRejectsMalformedPage(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	if err := g.Track(impl.PageSet{{Tenant: "t-1", Hash: "h1", Anon: true}}); !errors.Is(err, impl.ErrBadPage) {
		t.Fatalf("page without id err = %v, want ErrBadPage", err)
	}
	if err := g.Track(impl.PageSet{{ID: "p-1", Hash: "h1", Anon: true}}); !errors.Is(err, impl.ErrBadPage) {
		t.Fatalf("page without tenant err = %v, want ErrBadPage", err)
	}
	if err := g.Track(impl.PageSet{{ID: "p-1", Tenant: "t-1", Anon: true}}); !errors.Is(err, impl.ErrBadPage) {
		t.Fatalf("page without hash err = %v, want ErrBadPage", err)
	}
	if err := g.Track(impl.PageSet{page("p-1", "t-1", "h1")}); err != nil {
		t.Fatalf("well formed page must be tracked, got %v", err)
	}
}

func TestTrackEnforcesNodePageLimit(t *testing.T) {
	limits := ksmLimits()
	limits.MaxPagesPerNode = 2
	g := impl.NewKsmTenantGuard(limits)
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	batch := impl.PageSet{page("p-1", "t-1", "h1"), page("p-2", "t-1", "h1"), page("p-3", "t-1", "h1")}
	if err := g.Track(batch); !errors.Is(err, impl.ErrPageLimit) {
		t.Fatalf("over node limit err = %v, want ErrPageLimit", err)
	}
	if err := g.Track(impl.PageSet{page("p-1", "t-1", "h1"), page("p-2", "t-1", "h1")}); err != nil {
		t.Fatalf("tracking up to the limit must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Scanned != 2 {
		t.Fatalf("scanned pages = %d, want 2", report.Scanned)
	}
}

func TestScanMergesSameTenantAnonPages(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	batch := impl.PageSet{
		page("p-1", "t-1", "h1"),
		page("p-2", "t-1", "h1"),
		page("p-3", "t-1", "h1"),
		page("p-4", "t-1", "h1"),
	}
	if err := g.Track(batch); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Scanned != 4 || report.Merged != 4 || report.Skipped != 0 {
		t.Fatalf("report = %+v, want 4 scanned and 4 merged", report)
	}
	if math.Abs(report.DedupeRatio-100) > 0.01 {
		t.Fatalf("dedupe ratio = %v, want 100", report.DedupeRatio)
	}
	if got := len(g.MergedPages("t-1")); got != 4 {
		t.Fatalf("merged pages = %d, want 4", got)
	}
	stats := g.Stats("t-1")
	if stats.ControlledPages != 4 || stats.MergedPages != 4 {
		t.Fatalf("stats = %+v, want 4 controlled and 4 merged", stats)
	}
	if !stats.LastScan.Equal(baseTime()) {
		t.Fatalf("last scan = %v, want %v", stats.LastScan, baseTime())
	}
}

func TestScanDoesNotMergeAcrossTenants(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable t-1 must succeed, got %v", err)
	}
	if err := g.Enable("t-2", ksmPolicy()); err != nil {
		t.Fatalf("enable t-2 must succeed, got %v", err)
	}
	if err := g.Track(impl.PageSet{page("p-1", "t-1", "same"), page("p-2", "t-2", "same")}); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Merged != 0 {
		t.Fatalf("cross tenant pages must never merge, report = %+v", report)
	}
	if len(g.MergedPages("t-1")) != 0 || len(g.MergedPages("t-2")) != 0 {
		t.Fatalf("no tenant may own a merged page here")
	}
	if math.Abs(report.DedupeRatio) > 0.01 {
		t.Fatalf("dedupe ratio = %v, want 0", report.DedupeRatio)
	}
}

func TestScanSkipsKeyHolders(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	holder := page("p-3", "t-1", "h1")
	holder.HoldsKey = true
	if err := g.Track(impl.PageSet{page("p-1", "t-1", "h1"), page("p-2", "t-1", "h1"), holder}); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Merged != 2 || report.Skipped != 1 {
		t.Fatalf("report = %+v, want 2 merged and 1 skipped key holder", report)
	}
	if got := len(g.MergedPages("t-1")); got != 2 {
		t.Fatalf("merged pages = %d, want 2", got)
	}
}

func TestScanSkipsDecrypting(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	busy := page("p-3", "t-1", "h2")
	busy.Decrypting = true
	if err := g.Track(impl.PageSet{page("p-1", "t-1", "h2"), page("p-2", "t-1", "h2"), busy}); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Merged != 2 || report.Skipped != 1 {
		t.Fatalf("report = %+v, want 2 merged and the decrypting page skipped", report)
	}
}

func TestScanSkipsNonAnonPages(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	filePage1 := page("p-3", "t-1", "file")
	filePage1.Anon = false
	filePage2 := page("p-4", "t-1", "file")
	filePage2.Anon = false
	batch := impl.PageSet{page("p-1", "t-1", "h3"), page("p-2", "t-1", "h3"), filePage1, filePage2}
	if err := g.Track(batch); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Merged != 2 || report.Skipped != 2 {
		t.Fatalf("report = %+v, want 2 merged and 2 file backed pages skipped", report)
	}
	if math.Abs(report.DedupeRatio-50) > 0.01 {
		t.Fatalf("dedupe ratio = %v, want 50", report.DedupeRatio)
	}
}

func TestScanEnforcesMinInterval(t *testing.T) {
	limits := ksmLimits()
	limits.MinScanInterval = 5 * time.Second
	g := impl.NewKsmTenantGuard(limits)
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	if err := g.Track(impl.PageSet{page("p-1", "t-1", "h1"), page("p-2", "t-1", "h1")}); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	start := baseTime()
	if _, err := g.Scan(start); err != nil {
		t.Fatalf("first scan must succeed, got %v", err)
	}
	if _, err := g.Scan(start.Add(2 * time.Second)); !errors.Is(err, impl.ErrScanTooSoon) {
		t.Fatalf("early scan err = %v, want ErrScanTooSoon", err)
	}
	if _, err := g.Scan(start.Add(5 * time.Second)); err != nil {
		t.Fatalf("scan at the interval must succeed, got %v", err)
	}
}

func TestScanReportsPerTenantCounters(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable t-1 must succeed, got %v", err)
	}
	if err := g.Enable("t-2", ksmPolicy()); err != nil {
		t.Fatalf("enable t-2 must succeed, got %v", err)
	}
	batch := impl.PageSet{
		page("p-1", "t-1", "h1"),
		page("p-2", "t-1", "h1"),
		page("p-3", "t-2", "h2"),
		page("p-4", "t-2", "h3"),
	}
	if err := g.Track(batch); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.Scanned != 4 || report.Merged != 2 {
		t.Fatalf("report = %+v, want 4 scanned and 2 merged", report)
	}
	if math.Abs(report.DedupeRatio-50) > 0.01 {
		t.Fatalf("dedupe ratio = %v, want 50", report.DedupeRatio)
	}
	delta, ok := report.ByTenant["t-1"]
	if !ok || delta.Merged != 2 {
		t.Fatalf("tenant delta for t-1 = %+v, want 2 merged", delta)
	}
	delta, ok = report.ByTenant["t-2"]
	if !ok || delta.Merged != 0 || delta.Skipped != 2 {
		t.Fatalf("tenant delta for t-2 = %+v, want 2 skipped", delta)
	}
	stats := g.StatsAll()
	if len(stats) != 2 || stats[0].Tenant != "t-1" || stats[1].Tenant != "t-2" {
		t.Fatalf("stats = %+v, want t-1 then t-2", stats)
	}
}

func TestScanRaisesAlertsOverThreshold(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	policy := ksmPolicy()
	policy.AlertDedupeRatio = 50
	policy.AlertWALatencyMS = 2
	if err := g.Enable("t-1", policy); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	p1 := page("p-1", "t-1", "h1")
	p1.WriteRate = 1200
	p2 := page("p-2", "t-1", "h1")
	p2.WriteRate = 1200
	if err := g.Track(impl.PageSet{p1, p2}); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	report, err := g.Scan(baseTime())
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if report.WALatencyMS != 3 {
		t.Fatalf("write latency = %d, want 3ms for 2400 writes per second", report.WALatencyMS)
	}
	kinds := map[string]impl.Alert{}
	for _, alert := range g.Alerts() {
		kinds[alert.Kind] = alert
	}
	ratioAlert, ok := kinds[impl.AlertDedupeRatio]
	if !ok {
		t.Fatalf("expected a dedupe ratio alert, got %+v", g.Alerts())
	}
	if ratioAlert.Tenant != "t-1" || math.Abs(ratioAlert.Value-100) > 0.01 || ratioAlert.Threshold != 50 {
		t.Fatalf("ratio alert = %+v, want tenant t-1 value 100 threshold 50", ratioAlert)
	}
	latencyAlert, ok := kinds[impl.AlertWALatency]
	if !ok {
		t.Fatalf("expected a write latency alert, got %+v", g.Alerts())
	}
	if latencyAlert.Tenant != "t-1" || math.Abs(latencyAlert.Value-3) > 0.01 || latencyAlert.Threshold != 2 {
		t.Fatalf("latency alert = %+v, want tenant t-1 value 3 threshold 2", latencyAlert)
	}
	if len(report.Alerts) != 2 {
		t.Fatalf("report alerts = %d, want 2", len(report.Alerts))
	}
}

func TestIsolateUnmergesPages(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable must succeed, got %v", err)
	}
	batch := impl.PageSet{
		page("p-1", "t-1", "h1"),
		page("p-2", "t-1", "h1"),
		page("p-3", "t-1", "h1"),
		page("p-4", "t-1", "h1"),
	}
	if err := g.Track(batch); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	start := baseTime()
	if _, err := g.Scan(start); err != nil {
		t.Fatalf("first scan must succeed, got %v", err)
	}
	if got := len(g.MergedPages("t-1")); got != 4 {
		t.Fatalf("merged pages before isolate = %d, want 4", got)
	}
	if err := g.Isolate(impl.PageSet{page("p-1", "t-1", "h1"), page("p-2", "t-1", "h1")}); err != nil {
		t.Fatalf("isolate must succeed, got %v", err)
	}
	if got := len(g.MergedPages("t-1")); got != 2 {
		t.Fatalf("merged pages after isolate = %d, want 2", got)
	}
	if stats := g.Stats("t-1"); stats.MergedPages != 2 {
		t.Fatalf("stats merged pages = %d, want 2", stats.MergedPages)
	}
	report, err := g.Scan(start.Add(10 * time.Second))
	if err != nil {
		t.Fatalf("second scan must succeed, got %v", err)
	}
	if report.Isolated != 2 || report.Merged != 2 {
		t.Fatalf("report = %+v, want 2 isolated and 2 merged", report)
	}
	if err := g.Isolate(impl.PageSet{page("p-99", "t-1", "h1")}); !errors.Is(err, impl.ErrBadPage) {
		t.Fatalf("isolate of an untracked page err = %v, want ErrBadPage", err)
	}
}

func TestDisableUnmergesTenantPages(t *testing.T) {
	g := impl.NewKsmTenantGuard(ksmLimits())
	if err := g.Enable("t-1", ksmPolicy()); err != nil {
		t.Fatalf("enable t-1 must succeed, got %v", err)
	}
	if err := g.Enable("t-2", ksmPolicy()); err != nil {
		t.Fatalf("enable t-2 must succeed, got %v", err)
	}
	batch := impl.PageSet{
		page("p-1", "t-1", "h1"),
		page("p-2", "t-1", "h1"),
		page("p-3", "t-2", "h2"),
		page("p-4", "t-2", "h2"),
	}
	if err := g.Track(batch); err != nil {
		t.Fatalf("track must succeed, got %v", err)
	}
	start := baseTime()
	if _, err := g.Scan(start); err != nil {
		t.Fatalf("first scan must succeed, got %v", err)
	}
	if err := g.Disable("t-1"); err != nil {
		t.Fatalf("disable must succeed, got %v", err)
	}
	if got := len(g.MergedPages("t-1")); got != 0 {
		t.Fatalf("merged pages of the disabled tenant = %d, want 0", got)
	}
	if got := g.Stats("t-1").MergedPages; got != 0 {
		t.Fatalf("stats of the disabled tenant = %d, want 0", got)
	}
	if got := len(g.MergedPages("t-2")); got != 2 {
		t.Fatalf("merged pages of the neighbour tenant = %d, want 2", got)
	}
	report, err := g.Scan(start.Add(10 * time.Second))
	if err != nil {
		t.Fatalf("second scan must succeed, got %v", err)
	}
	if report.Merged != 2 {
		t.Fatalf("report merged = %d, want 2 for the surviving tenant", report.Merged)
	}
	if err := g.Disable("t-9"); !errors.Is(err, impl.ErrNoTenant) {
		t.Fatalf("disable of an unknown tenant err = %v, want ErrNoTenant", err)
	}
}
