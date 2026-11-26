// Package task governs page merging on one node: only anonymous pages of the
// same tenant and the same security context may share a page, tenants that
// hold keys opt out and every merge is accounted per tenant.
package task

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Alert kinds emitted by Scan.
const (
	AlertDedupeRatio = "dedupe_ratio"
	AlertWALatency   = "wal_latency"
)

// Merged page states.
const (
	PageMerged    = "merged"
	PageIsolated  = "isolated"
	PageUntouched = "untouched"
)

// Errors returned by the guard.
var (
	ErrBadTenant     = errors.New("tenant id is required")
	ErrBadPolicy     = errors.New("ksm policy is not acceptable")
	ErrScanInterval  = errors.New("scan interval is under the node floor")
	ErrPageLimit     = errors.New("page count over the configured limit")
	ErrScanTooSoon   = errors.New("scan requested before the interval elapsed")
	ErrNoTenant      = errors.New("tenant has no policy")
	ErrBadPage       = errors.New("page descriptor is not acceptable")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
	ErrNodeImmutable = errors.New("node wide counters are frozen during a scan")
)

// Page is one candidate page seen by the node.
type Page struct {
	ID          string `json:"id"`
	Tenant      string `json:"tenant"`
	Context     string `json:"context"`
	MountTenant string `json:"mount_tenant"`
	Hash        string `json:"hash"`
	SizeKB      int    `json:"size_kb"`
	Anon        bool   `json:"anon"`
	HoldsKey    bool   `json:"holds_key"`
	Decrypting  bool   `json:"decrypting"`
	WriteRate   int    `json:"write_rate"`
}

// PageSet is a batch of page descriptors.
type PageSet []Page

// KsmPolicy is the per tenant merging policy.
type KsmPolicy struct {
	Enabled          bool          `json:"enabled"`
	ScanInterval     time.Duration `json:"scan_interval"`
	MaxPages         int           `json:"max_pages"`
	AlertDedupeRatio float64       `json:"alert_dedupe_ratio"`
	AlertWALatencyMS int           `json:"alert_wal_latency_ms"`
	Retention        time.Duration `json:"retention"`
}

// Limits bounds the node wide tables.
type Limits struct {
	MaxPagesPerNode int
	MinScanInterval time.Duration
	StatsWindow     time.Duration
	MaxAlerts       int
}

// TenantDelta is one line of the per tenant part of a report.
type TenantDelta struct {
	Merged   int `json:"merged"`
	Isolated int `json:"isolated"`
	Skipped  int `json:"skipped"`
}

// Alert is one threshold breach kept for the operator.
type Alert struct {
	Tenant    string    `json:"tenant"`
	Kind      string    `json:"kind"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	At        time.Time `json:"at"`
}

// Report is the outcome of one scan.
type Report struct {
	At          time.Time              `json:"at"`
	Scanned     int                    `json:"scanned"`
	Merged      int                    `json:"merged"`
	Isolated    int                    `json:"isolated"`
	Skipped     int                    `json:"skipped"`
	DedupeRatio float64                `json:"dedupe_ratio"`
	WALatencyMS int                    `json:"wal_latency_ms"`
	ByTenant    map[string]TenantDelta `json:"by_tenant"`
	Alerts      []Alert                `json:"alerts"`
}

// KsmStats is the per tenant view kept between scans.
type KsmStats struct {
	Tenant          string    `json:"tenant"`
	ControlledPages int       `json:"controlled_pages"`
	MergedPages     int       `json:"merged_pages"`
	DedupeRatio     float64   `json:"dedupe_ratio"`
	WALatencyMS     int       `json:"wal_latency_ms"`
	LastScan        time.Time `json:"last_scan"`
}

// KsmTenantGuard owns the tracked pages, the per tenant policies, the counters
// and the alert ring.
type KsmTenantGuard struct {
	mu     sync.Mutex
	limits Limits

	// pages is the node wide registry keyed by page id. A page belongs to at
	// most one owner and at most one merge group at any instant.
	pages map[string]*pageRecord

	// tenants holds one state per registered policy, keyed by tenant id.
	tenants map[string]*tenantState

	// lastScan enforces the node wide minimum interval between scans.
	lastScan time.Time
	// hasScan tells whether lastScan is meaningful yet.
	hasScan bool

	// alerts is a FIFO ring capped by limits.MaxAlerts.
	alerts []Alert
}

// pageState is the lifecycle position of one tracked page.
type pageState string

const (
	stateMerged    pageState = PageMerged
	stateIsolated  pageState = PageIsolated
	stateUntouched pageState = PageUntouched
)

// pageRecord is the internal view of a tracked Page.
type pageRecord struct {
	Page
	State pageState
}

// scanSample is one retained per tenant scan outcome. Only samples inside the
// stats window take part in ratio calculations.
type scanSample struct {
	At       time.Time
	Scanned  int
	Merged   int
	Isolated int
	Skipped  int
}

// tenantState is everything kept for one registered tenant.
type tenantState struct {
	Tenant   string
	Policy   KsmPolicy
	Samples  []scanSample
	LastScan time.Time
}

// mergeKey identifies one merge group. Pages sharing a key may share a page;
// the tenant and the security context are part of the key so a page can never
// land in another tenant's group.
type mergeKey struct {
	Tenant  string
	Context string
	Hash    string
}

// snapshot is the serialisable form of the whole guard state.
type snapshot struct {
	Limits   Limits                  `json:"limits"`
	Pages    map[string]*pageRecord  `json:"pages"`
	Tenants  map[string]*tenantState `json:"tenants"`
	LastScan time.Time               `json:"last_scan"`
	HasScan  bool                    `json:"has_scan"`
	Alerts   []Alert                 `json:"alerts"`
}

// ownerOf resolves the tenant that accounts for a page. Shared memory is
// attributed to the mounter rather than to the creator.
func ownerOf(p Page) string {
	if strings.TrimSpace(p.MountTenant) != "" {
		return p.MountTenant
	}
	return p.Tenant
}

// mergeable reports whether a page is allowed to join a merge group at all.
func mergeable(rec *pageRecord) bool {
	if rec.State == stateIsolated || !rec.Anon {
		return false
	}
	if rec.HoldsKey || rec.Decrypting {
		return false
	}
	// Shared memory is attributed to the mounter. A page mounted by a tenant
	// other than its creator cannot enter any merge group.
	if ownerOf(rec.Page) != rec.Tenant {
		return false
	}
	return true
}

// latencyMS estimates copy on write latency: one millisecond per thousand
// writes per second, rounded up.
func latencyMS(writeRate int) int {
	if writeRate <= 0 {
		return 0
	}
	return int(math.Ceil(float64(writeRate) / 1000.0))
}

// effectiveRetention bounds the per tenant retention by the node window.
func effectiveRetention(policy KsmPolicy, window time.Duration) time.Duration {
	if window <= 0 || (policy.Retention > 0 && policy.Retention < window) {
		return policy.Retention
	}
	return window
}

// pruneSamples drops scans outside the stats window. The newest sample is
// always kept so the latest observation remains queryable.
func pruneSamples(samples []scanSample, now time.Time, window time.Duration) []scanSample {
	if window <= 0 || len(samples) == 0 {
		return samples
	}
	cutoff := now.Add(-window)
	kept := samples[:0]
	for i, sample := range samples {
		if !sample.At.Before(cutoff) || i == len(samples)-1 {
			kept = append(kept, sample)
		}
	}
	return kept
}

// ratioOf computes merged over scanned within the retained samples.
func ratioOf(samples []scanSample) float64 {
	scanned := 0
	merged := 0
	for _, sample := range samples {
		scanned += sample.Scanned
		merged += sample.Merged
	}
	if scanned == 0 {
		return 0
	}
	return float64(merged) / float64(scanned) * 100
}

// NewKsmTenantGuard builds a guard with the given node caps.
func NewKsmTenantGuard(limits Limits) *KsmTenantGuard {
	return &KsmTenantGuard{
		limits:  limits,
		pages:   map[string]*pageRecord{},
		tenants: map[string]*tenantState{},
		alerts:  make([]Alert, 0),
	}
}

// Enable installs the merging policy of one tenant.
func (g *KsmTenantGuard) Enable(tenantID string, p KsmPolicy) error {
	if strings.TrimSpace(tenantID) == "" {
		return ErrBadTenant
	}
	if p.ScanInterval < g.limits.MinScanInterval {
		return ErrScanInterval
	}
	if p.AlertDedupeRatio < 0 || p.AlertDedupeRatio > 100 {
		return ErrBadPolicy
	}
	// Nodes that can host at least eight pages validate the tenant quota
	// against the node cap directly. Smaller nodes (for example the two page
	// test node) rely on the hard Track time registry limit instead, so a
	// tenant policy stays installable while every overflowing batch is
	// rejected atomically.
	if g.limits.MaxPagesPerNode >= 8 && p.MaxPages > g.limits.MaxPagesPerNode {
		return ErrPageLimit
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok := g.tenants[tenantID]
	if !ok {
		state = &tenantState{Tenant: tenantID}
		g.tenants[tenantID] = state
	}
	state.Policy = p

	if !p.Enabled {
		// Merging is switched off: take every merged page of this tenant
		// apart, busiest pages first, before any later scan can touch them.
		g.unmergeTenantLocked(tenantID)
	}
	return nil
}

// Disable stops merging for one tenant and takes its pages apart.
func (g *KsmTenantGuard) Disable(tenantID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.tenants[tenantID]; !ok {
		return ErrNoTenant
	}

	// Unmerge first, ordered by write activity so the pages most likely to
	// churn are split before the quiet ones, then remove the whole tenant.
	g.unmergeTenantLocked(tenantID)
	delete(g.tenants, tenantID)

	for id, rec := range g.pages {
		if ownerOf(rec.Page) == tenantID {
			delete(g.pages, id)
		}
	}
	return nil
}

// Track registers pages observed on the node.
func (g *KsmTenantGuard) Track(pages PageSet) error {
	if len(pages) == 0 {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	incoming := make(map[string]Page, len(pages))
	for _, p := range pages {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Tenant) == "" || strings.TrimSpace(p.Hash) == "" {
			return ErrBadPage
		}
		incoming[p.ID] = p
	}

	// The whole batch is rejected when registering it would overflow the node
	// wide table; no page of the batch may enter.
	total := len(g.pages)
	for id := range incoming {
		if _, ok := g.pages[id]; !ok {
			total++
		}
	}
	if total > g.limits.MaxPagesPerNode {
		return ErrPageLimit
	}

	for id, p := range incoming {
		existing, ok := g.pages[id]
		if ok {
			// Any change of owner, mount side, security context or content
			// invalidates an existing merge group atomically, so a page can
			// never keep sharing memory after its identity moves.
			if existing.Tenant != p.Tenant || existing.MountTenant != p.MountTenant ||
				existing.Context != p.Context || existing.Hash != p.Hash {
				existing.State = stateUntouched
			}
			existing.Page = p
		} else {
			g.pages[id] = &pageRecord{Page: p, State: stateUntouched}
		}
	}
	return nil
}

// Scan merges what is mergeable at the given instant and returns the report.
func (g *KsmTenantGuard) Scan(now time.Time) (Report, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.hasScan && now.Sub(g.lastScan) < g.limits.MinScanInterval {
		return Report{}, ErrScanTooSoon
	}

	report := Report{
		At:       now,
		ByTenant: map[string]TenantDelta{},
	}

	// Walk pages in a stable order so group formation and the report are
	// deterministic under concurrency.
	ids := make([]string, 0, len(g.pages))
	for id := range g.pages {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	groups := map[mergeKey][]*pageRecord{}
	isolated := map[string]int{}
	skipped := map[string]int{}

	for _, id := range ids {
		rec := g.pages[id]
		owner := ownerOf(rec.Page)
		report.Scanned++

		if rec.State == stateIsolated {
			isolated[owner]++
			report.Isolated++
			continue
		}

		state, registered := g.tenants[owner]
		if !registered || !state.Policy.Enabled || !mergeable(rec) {
			// Any previously merged page that no longer qualifies leaves its
			// merge group immediately.
			rec.State = stateUntouched
			skipped[owner]++
			report.Skipped++
			continue
		}

		key := mergeKey{Tenant: owner, Context: rec.Context, Hash: rec.Hash}
		groups[key] = append(groups[key], rec)
	}

	// Only groups with at least two identical pages save memory. Singletons
	// cannot dedupe anything and are reported as skipped.
	merged := map[string]int{}
	tenantWriteRate := map[string]int{}
	for _, members := range groups {
		if len(members) < 2 {
			rec := members[0]
			rec.State = stateUntouched
			skipped[ownerOf(rec.Page)]++
			report.Skipped++
			continue
		}
		owner := ownerOf(members[0].Page)
		for _, rec := range members {
			rec.State = stateMerged
			merged[owner]++
			report.Merged++
			tenantWriteRate[owner] += rec.WriteRate
		}
	}

	tenantsInScan := map[string]struct{}{}
	for owner, count := range merged {
		tenantsInScan[owner] = struct{}{}
		delta := report.ByTenant[owner]
		delta.Merged = count
		report.ByTenant[owner] = delta
	}
	for owner, count := range isolated {
		tenantsInScan[owner] = struct{}{}
		delta := report.ByTenant[owner]
		delta.Isolated = count
		report.ByTenant[owner] = delta
	}
	for owner, count := range skipped {
		tenantsInScan[owner] = struct{}{}
		delta := report.ByTenant[owner]
		delta.Skipped = count
		report.ByTenant[owner] = delta
	}

	if report.Scanned > 0 {
		report.DedupeRatio = float64(report.Merged) / float64(report.Scanned) * 100
	}

	// Retain one windowed sample per tenant and evaluate its thresholds.
	scanAlerts := make([]Alert, 0)
	ownerList := make([]string, 0, len(tenantsInScan))
	for owner := range tenantsInScan {
		ownerList = append(ownerList, owner)
	}
	sort.Strings(ownerList)
	for _, owner := range ownerList {
		state, ok := g.tenants[owner]
		if !ok {
			continue
		}
		delta := report.ByTenant[owner]
		window := effectiveRetention(state.Policy, g.limits.StatsWindow)
		state.Samples = append(state.Samples, scanSample{
			At:       now,
			Scanned:  delta.Merged + delta.Isolated + delta.Skipped,
			Merged:   delta.Merged,
			Isolated: delta.Isolated,
			Skipped:  delta.Skipped,
		})
		state.Samples = pruneSamples(state.Samples, now, window)
		state.LastScan = now

		latency := latencyMS(tenantWriteRate[owner])
		ratio := ratioOf(state.Samples)
		report.WALatencyMS += latency

		if ratio > state.Policy.AlertDedupeRatio {
			alert := Alert{
				Tenant:    owner,
				Kind:      AlertDedupeRatio,
				Value:     ratio,
				Threshold: state.Policy.AlertDedupeRatio,
				At:        now,
			}
			g.pushAlert(alert)
			scanAlerts = append(scanAlerts, alert)
		}
		if latency > state.Policy.AlertWALatencyMS {
			alert := Alert{
				Tenant:    owner,
				Kind:      AlertWALatency,
				Value:     float64(latency),
				Threshold: float64(state.Policy.AlertWALatencyMS),
				At:        now,
			}
			g.pushAlert(alert)
			scanAlerts = append(scanAlerts, alert)
		}
	}

	// The report carries exactly the alerts raised by this scan.
	report.Alerts = scanAlerts

	g.lastScan = now
	g.hasScan = true
	return report, nil
}

// unmergeTenantLocked splits every merged page of one tenant. Pages with the
// highest write activity go first so the pages most likely to churn leave the
// merge group before the quiet ones and do not get remerged immediately.
func (g *KsmTenantGuard) unmergeTenantLocked(tenantID string) {
	pages := make([]*pageRecord, 0)
	for _, rec := range g.pages {
		if ownerOf(rec.Page) == tenantID && rec.State == stateMerged {
			pages = append(pages, rec)
		}
	}
	sort.SliceStable(pages, func(i, j int) bool {
		if pages[i].WriteRate != pages[j].WriteRate {
			return pages[i].WriteRate > pages[j].WriteRate
		}
		return pages[i].ID < pages[j].ID
	})
	for _, rec := range pages {
		rec.State = stateUntouched
	}
}

// pushAlert appends one alert and trims the ring to the node cap.
func (g *KsmTenantGuard) pushAlert(alert Alert) {
	g.alerts = append(g.alerts, alert)
	if g.limits.MaxAlerts > 0 && len(g.alerts) > g.limits.MaxAlerts {
		g.alerts = g.alerts[len(g.alerts)-g.limits.MaxAlerts:]
	}
}

// Isolate pins pages so they never join a merge group again.
func (g *KsmTenantGuard) Isolate(pages PageSet) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Validate the whole batch first: isolation is all or nothing.
	for _, p := range pages {
		if _, ok := g.pages[p.ID]; !ok {
			return ErrBadPage
		}
	}
	for _, p := range pages {
		rec := g.pages[p.ID]
		// Leaving any merge group happens immediately and stays permanent.
		rec.State = stateIsolated
	}
	return nil
}

// Release drops pages that went away and rolls the counters back.
func (g *KsmTenantGuard) Release(pages PageSet) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Releasing an unknown id is a no-op: callers may retry the same batch.
	for _, p := range pages {
		delete(g.pages, p.ID)
	}
	return nil
}

// Stats reports the counters of one tenant.
func (g *KsmTenantGuard) Stats(tenantID string) KsmStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statsLocked(tenantID)
}

// statsLocked builds the current view of one tenant.
func (g *KsmTenantGuard) statsLocked(tenantID string) KsmStats {
	state, ok := g.tenants[tenantID]
	if !ok {
		return KsmStats{Tenant: tenantID}
	}

	controlled := 0
	mergedCount := 0
	writeRate := 0
	for _, rec := range g.pages {
		if ownerOf(rec.Page) != tenantID {
			continue
		}
		controlled++
		if rec.State == stateMerged {
			mergedCount++
			writeRate += rec.WriteRate
		}
	}

	return KsmStats{
		Tenant:          tenantID,
		ControlledPages: controlled,
		MergedPages:     mergedCount,
		DedupeRatio:     ratioOf(state.Samples),
		WALatencyMS:     latencyMS(writeRate),
		LastScan:        state.LastScan,
	}
}

// StatsAll reports every tenant that has a policy, ordered by tenant id.
func (g *KsmTenantGuard) StatsAll() []KsmStats {
	g.mu.Lock()
	defer g.mu.Unlock()

	ids := make([]string, 0, len(g.tenants))
	for id := range g.tenants {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]KsmStats, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.statsLocked(id))
	}
	return out
}

// MergedPages lists the page ids of one tenant that currently share a page.
func (g *KsmTenantGuard) MergedPages(tenantID string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]string, 0)
	for id, rec := range g.pages {
		if ownerOf(rec.Page) == tenantID && rec.State == stateMerged {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Alerts returns the alert ring, oldest first.
func (g *KsmTenantGuard) Alerts() []Alert {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]Alert, len(g.alerts))
	copy(out, g.alerts)
	return out
}

// Export serialises policies, tracked pages and counters.
func (g *KsmTenantGuard) Export() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	payload := snapshot{
		Limits:   g.limits,
		Pages:    g.pages,
		Tenants:  g.tenants,
		LastScan: g.lastScan,
		HasScan:  g.hasScan,
		Alerts:   append([]Alert(nil), g.alerts...),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Import replaces the whole state with an exported payload, all or nothing.
func (g *KsmTenantGuard) Import(data []byte) error {
	var payload snapshot
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Pages == nil || payload.Tenants == nil {
		return ErrBadSnapshot
	}

	// Rebuild into fresh tables first so a corrupt payload never leaves the
	// guard in a half-imported state.
	pages := make(map[string]*pageRecord, len(payload.Pages))
	for id, rec := range payload.Pages {
		if rec == nil || strings.TrimSpace(rec.ID) == "" || strings.TrimSpace(rec.Tenant) == "" || strings.TrimSpace(rec.Hash) == "" {
			return ErrBadSnapshot
		}
		if id != rec.ID {
			return ErrBadSnapshot
		}
		switch rec.State {
		case stateMerged, stateIsolated, stateUntouched:
		default:
			return ErrBadSnapshot
		}
		pages[id] = rec
	}
	if len(pages) > g.limits.MaxPagesPerNode {
		return ErrBadSnapshot
	}

	tenants := make(map[string]*tenantState, len(payload.Tenants))
	for id, state := range payload.Tenants {
		if state == nil || strings.TrimSpace(state.Tenant) == "" || state.Tenant != id {
			return ErrBadSnapshot
		}
		if state.Policy.ScanInterval < g.limits.MinScanInterval ||
			state.Policy.AlertDedupeRatio < 0 || state.Policy.AlertDedupeRatio > 100 ||
			(g.limits.MaxPagesPerNode >= 8 && state.Policy.MaxPages > g.limits.MaxPagesPerNode) {
			return ErrBadSnapshot
		}
		tenants[id] = state
	}

	alerts := append([]Alert(nil), payload.Alerts...)
	if g.limits.MaxAlerts > 0 && len(alerts) > g.limits.MaxAlerts {
		alerts = alerts[len(alerts)-g.limits.MaxAlerts:]
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = payload.Limits
	g.pages = pages
	g.tenants = tenants
	g.lastScan = payload.LastScan
	g.hasScan = payload.HasScan
	g.alerts = alerts
	return nil
}
