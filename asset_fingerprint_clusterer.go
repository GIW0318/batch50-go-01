// Package task folds scan observations into asset instances and keeps every
// merge explainable, idempotent and reversible.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Signal names reported by Explain.
const (
	SignalCertPubKey    = "cert_pubkey"
	SignalSANSet        = "san_set"
	SignalHeaderOrder   = "header_order"
	SignalServerHeader  = "server_header"
	SignalFavicon       = "favicon"
	SignalRedirectFinal = "redirect_final"
)

// Signal weights used when two observations are compared.
const (
	WeightCertPubKey    = 60
	WeightSANSet        = 25
	WeightFavicon       = 20
	WeightHeaderOrder   = 15
	WeightServerHeader  = 10
	WeightRedirectFinal = 10
)

// Confidence anchors.
const (
	MaxConfidence   = 100
	MergeDecayDenom = 10
	MergeDecayKeep  = 9
)

// Errors returned by the clusterer.
var (
	ErrEmptyObservation   = errors.New("observation id and host are required")
	ErrObservationLimit   = errors.New("observation cap reached")
	ErrClusterLimit       = errors.New("cluster cap reached")
	ErrMemberLimit        = errors.New("cluster member cap reached")
	ErrUnknownCluster     = errors.New("unknown cluster")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// Config bounds one clusterer instance.
type Config struct {
	Threshold   int
	MaxClusters int
	MaxMembers  int
	MaxObs      int
	TTL         time.Duration
}

// Observation is one endpoint fingerprint collected by a scanner.
type Observation struct {
	ObsID         string    `json:"obs_id"`
	Host          string    `json:"host"`
	CertPubKey    string    `json:"cert_pub_key"`
	SANs          []string  `json:"sans"`
	HeaderOrder   []string  `json:"header_order"`
	ServerHeader  string    `json:"server_header"`
	FaviconHash   string    `json:"favicon_hash"`
	RedirectFinal string    `json:"redirect_final"`
	SeenAt        time.Time `json:"seen_at"`
}

// Cluster is one asset instance.
type Cluster struct {
	ID         string   `json:"id"`
	Members    []string `json:"members"`
	Confidence int      `json:"confidence"`
	Pinned     bool     `json:"pinned"`
}

// Signal is one piece of evidence behind a cluster.
type Signal struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"`
	Detail string `json:"detail"`
}

// AssetFingerprintClusterer owns the observations and the merged instances.
type AssetFingerprintClusterer struct {
	cfg Config
}

// NewAssetFingerprintClusterer builds a clusterer with the given caps.
func NewAssetFingerprintClusterer(cfg Config) *AssetFingerprintClusterer {
	return &AssetFingerprintClusterer{cfg: cfg}
}

// Add folds one observation into the closest cluster or opens a new one.
func (c *AssetFingerprintClusterer) Add(obs Observation) (string, error) {
	return "", errNotImplemented
}

// Merge folds two clusters into one instance and returns the new id.
func (c *AssetFingerprintClusterer) Merge(a, b string) (string, error) {
	return "", errNotImplemented
}

// Split breaks one cluster into one cluster per member and returns the new ids.
func (c *AssetFingerprintClusterer) Split(clusterID string) ([]string, error) {
	return nil, errNotImplemented
}

// Explain lists the evidence behind a cluster, strongest signal first.
func (c *AssetFingerprintClusterer) Explain(clusterID string) ([]Signal, error) {
	return nil, errNotImplemented
}

// Cluster reads one cluster.
func (c *AssetFingerprintClusterer) Cluster(clusterID string) (Cluster, bool) {
	return Cluster{}, false
}

// Clusters lists the live cluster ids in ascending order.
func (c *AssetFingerprintClusterer) Clusters() []string {
	return nil
}

// Members lists the observation ids of one cluster in ascending order.
func (c *AssetFingerprintClusterer) Members(clusterID string) []string {
	return nil
}

// Confidence reads the confidence of one cluster.
func (c *AssetFingerprintClusterer) Confidence(clusterID string) int {
	return 0
}

// Evict drops clusters whose newest observation is older than the TTL.
func (c *AssetFingerprintClusterer) Evict(now time.Time) int {
	return 0
}

// Snapshot serialises observations, clusters and the pinned flags.
func (c *AssetFingerprintClusterer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *AssetFingerprintClusterer) Restore(data []byte) error {
	return errNotImplemented
}
