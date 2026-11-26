// Package task governs page merging on one node: only anonymous pages of the
// same tenant and the same security context may share a page, tenants that
// hold keys opt out and every merge is accounted per tenant.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks every behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

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
	limits Limits
}

// NewKsmTenantGuard builds a guard with the given node caps.
func NewKsmTenantGuard(limits Limits) *KsmTenantGuard {
	return &KsmTenantGuard{limits: limits}
}

// Enable installs the merging policy of one tenant.
func (g *KsmTenantGuard) Enable(tenantID string, p KsmPolicy) error {
	return errNotImplemented
}

// Disable stops merging for one tenant and takes its pages apart.
func (g *KsmTenantGuard) Disable(tenantID string) error {
	return errNotImplemented
}

// Track registers pages observed on the node.
func (g *KsmTenantGuard) Track(pages PageSet) error {
	return errNotImplemented
}

// Scan merges what is mergeable at the given instant and returns the report.
func (g *KsmTenantGuard) Scan(now time.Time) (Report, error) {
	return Report{}, errNotImplemented
}

// Isolate pins pages so they never join a merge group again.
func (g *KsmTenantGuard) Isolate(pages PageSet) error {
	return errNotImplemented
}

// Release drops pages that went away and rolls the counters back.
func (g *KsmTenantGuard) Release(pages PageSet) error {
	return errNotImplemented
}

// Stats reports the counters of one tenant.
func (g *KsmTenantGuard) Stats(tenantID string) KsmStats {
	return KsmStats{}
}

// StatsAll reports every tenant that has a policy, ordered by tenant id.
func (g *KsmTenantGuard) StatsAll() []KsmStats {
	return nil
}

// MergedPages lists the page ids of one tenant that currently share a page.
func (g *KsmTenantGuard) MergedPages(tenantID string) []string {
	return nil
}

// Alerts returns the alert ring, oldest first.
func (g *KsmTenantGuard) Alerts() []Alert {
	return nil
}

// Export serialises policies, tracked pages and counters.
func (g *KsmTenantGuard) Export() ([]byte, error) {
	return nil, errNotImplemented
}

// Import replaces the whole state with an exported payload, all or nothing.
func (g *KsmTenantGuard) Import(data []byte) error {
	return errNotImplemented
}
