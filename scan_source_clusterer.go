// Package task clusters scattered probe events into scan activities and
// keeps their lifecycle, including cooling and archiving, across restarts.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Errors returned by the clusterer.
var (
	ErrEmptyEvent          = errors.New("source and target are required")
	ErrDuplicateEvent      = errors.New("event already ingested")
	ErrActivityLimit       = errors.New("tracked activity limit reached")
	ErrEventLimit          = errors.New("event limit reached for this activity")
	ErrUnknownActivity     = errors.New("unknown activity")
	ErrUnknownCluster      = errors.New("unknown cluster")
	ErrInsufficientEvidence = errors.New("not enough evidence to cluster")
	ErrSameActivity        = errors.New("cannot merge an activity with itself")
	ErrBadTTL              = errors.New("cool down ttl must be positive")
	ErrBadSnapshot         = errors.New("snapshot payload is invalid")
)

// ProbeEvent is one observed probe against one target.
type ProbeEvent struct {
	ID     string    `json:"id"`
	Source string    `json:"source"`
	Target string    `json:"target"`
	Port   int       `json:"port"`
	Path   string    `json:"path"`
	At     time.Time `json:"at"`
}

// Activity is the per source event stream kept by the clusterer.
type Activity struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Events    int       `json:"events"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	ClusterID string    `json:"cluster_id"`
}

// Features is the explainable view of one cluster.
type Features struct {
	ClusterID    string    `json:"cluster_id"`
	Activities   []string  `json:"activities"`
	Sources      []string  `json:"sources"`
	Targets      []string  `json:"targets"`
	Events       int       `json:"events"`
	FirstAt      time.Time `json:"first_at"`
	LastAt       time.Time `json:"last_at"`
	CoolingUntil time.Time `json:"cooling_until"`
	Archived     bool      `json:"archived"`
}

// Limits bounds the live tables.
type Limits struct {
	MaxTrackedActivities int
	MaxEventsPerActivity int
	MinEventsToCluster   int
}

// ScanSourceClusterer owns activities, clusters and their cool down windows.
type ScanSourceClusterer struct {
	limits Limits
	now    func() time.Time
}

// NewScanSourceClusterer builds a clusterer with the given caps.
func NewScanSourceClusterer(limits Limits) *ScanSourceClusterer {
	return &ScanSourceClusterer{limits: limits, now: time.Now}
}

// SetClock replaces the clock used for cool down expiry.
func (c *ScanSourceClusterer) SetClock(now func() time.Time) {
	c.now = now
}

// Ingest records one probe event under the activity of its source.
func (c *ScanSourceClusterer) Ingest(ev ProbeEvent) (string, error) {
	return "", errNotImplemented
}

// Cluster turns one activity into a cluster once it has enough evidence.
func (c *ScanSourceClusterer) Cluster(activityID string) (string, error) {
	return "", errNotImplemented
}

// Merge folds two activities into the cluster of the first one.
func (c *ScanSourceClusterer) Merge(a, b string) (string, error) {
	return "", errNotImplemented
}

// Explain returns the features of one cluster, archived or live.
func (c *ScanSourceClusterer) Explain(clusterID string) (Features, error) {
	return Features{}, errNotImplemented
}

// CoolDown starts a cool down window on one cluster.
func (c *ScanSourceClusterer) CoolDown(clusterID string, ttl time.Duration) error {
	return errNotImplemented
}

// Expire archives every cluster whose cool down window has passed.
func (c *ScanSourceClusterer) Expire() (int, error) {
	return 0, errNotImplemented
}

// Events lists the events of one activity in ascending time order.
func (c *ScanSourceClusterer) Events(activityID string) ([]ProbeEvent, error) {
	return nil, errNotImplemented
}

// Activities lists the live activity ids in ascending order.
func (c *ScanSourceClusterer) Activities() []string {
	return nil
}

// Archived lists the archived cluster ids in ascending order.
func (c *ScanSourceClusterer) Archived() []string {
	return nil
}

// Snapshot serialises activities, events, clusters and cool down windows.
func (c *ScanSourceClusterer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *ScanSourceClusterer) Restore(data []byte) error {
	return errNotImplemented
}
