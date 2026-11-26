// Package task folds scan observations into asset instances and keeps every
// merge explainable, idempotent and reversible.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

	snapshotVersion      = 1
	reducedGenericWeight = 10
	firstClusterSequence = 0
)

// Errors returned by the clusterer.
var (
	ErrEmptyObservation = errors.New("observation id and host are required")
	ErrObservationLimit = errors.New("observation cap reached")
	ErrClusterLimit     = errors.New("cluster cap reached")
	ErrMemberLimit      = errors.New("cluster member cap reached")
	ErrUnknownCluster   = errors.New("unknown cluster")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
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

type evidenceRecord struct {
	Signal
	ObsA string `json:"obs_a"`
	ObsB string `json:"obs_b"`
}

type clusterState struct {
	id         string
	members    []string
	confidence int
	pinned     bool
	lastSeen   time.Time
	evidence   []evidenceRecord
}

type snapshotData struct {
	Version      int               `json:"version"`
	NextCluster  int               `json:"next_cluster"`
	Observations []Observation     `json:"observations"`
	Clusters     []snapshotCluster `json:"clusters"`
}

type snapshotCluster struct {
	ID         string           `json:"id"`
	Members    []string         `json:"members"`
	Confidence int              `json:"confidence"`
	Pinned     bool             `json:"pinned"`
	LastSeen   time.Time        `json:"last_seen"`
	Evidence   []evidenceRecord `json:"evidence"`
}

// AssetFingerprintClusterer owns the observations and the merged instances.
type AssetFingerprintClusterer struct {
	mu           sync.RWMutex
	cfg          Config
	nextCluster  int
	clusters     map[string]*clusterState
	observations map[string]Observation
	obsCluster   map[string]string
}

// NewAssetFingerprintClusterer builds a clusterer with the given caps.
func NewAssetFingerprintClusterer(cfg Config) *AssetFingerprintClusterer {
	return &AssetFingerprintClusterer{
		cfg:          cfg,
		nextCluster:  firstClusterSequence,
		clusters:     make(map[string]*clusterState),
		observations: make(map[string]Observation),
		obsCluster:   make(map[string]string),
	}
}

// Add folds one observation into the closest cluster or opens a new one.
func (c *AssetFingerprintClusterer) Add(obs Observation) (string, error) {
	if strings.TrimSpace(obs.ObsID) == "" || strings.TrimSpace(obs.Host) == "" {
		return "", ErrEmptyObservation
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if clusterID, ok := c.obsCluster[obs.ObsID]; ok {
		return clusterID, nil
	}
	if exceedsCap(c.cfg.MaxObs, len(c.observations)+1) {
		return "", ErrObservationLimit
	}

	bestCluster := ""
	bestFromObs := ""
	bestScore := -1
	var bestEvidence []evidenceRecord

	for clusterID, cluster := range c.clusters {
		for _, memberID := range cluster.members {
			previous, ok := c.observations[memberID]
			if !ok {
				continue
			}
			score, evidence, comparable := compareObservations(previous, obs)
			if !comparable || score < c.cfg.Threshold {
				continue
			}
			if bestCluster == "" || score > bestScore ||
				(score == bestScore && lessCandidate(clusterID, memberID, bestCluster, bestFromObs)) {
				bestCluster = clusterID
				bestFromObs = memberID
				bestScore = score
				bestEvidence = evidence
			}
		}
	}

	if bestCluster != "" {
		cluster := c.clusters[bestCluster]
		if exceedsCap(c.cfg.MaxMembers, len(cluster.members)+1) {
			return "", ErrMemberLimit
		}

		cluster.members = insertSorted(cluster.members, obs.ObsID)
		cluster.confidence = min(bestScore, MaxConfidence)
		cluster.evidence = append(cluster.evidence, bestEvidence...)
		if obs.SeenAt.After(cluster.lastSeen) {
			cluster.lastSeen = obs.SeenAt
		}
		c.storeObservationLocked(obs, bestCluster)
		return bestCluster, nil
	}

	if exceedsCap(c.cfg.MaxClusters, len(c.clusters)+1) {
		return "", ErrClusterLimit
	}
	if exceedsCap(c.cfg.MaxMembers, 1) {
		return "", ErrMemberLimit
	}

	clusterID := c.nextClusterIDLocked()
	c.clusters[clusterID] = &clusterState{
		id:         clusterID,
		members:    []string{obs.ObsID},
		confidence: MaxConfidence,
		lastSeen:   obs.SeenAt,
	}
	c.storeObservationLocked(obs, clusterID)
	return clusterID, nil
}

// Merge folds two clusters into one instance and returns the new id.
func (c *AssetFingerprintClusterer) Merge(a, b string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if a == b {
		if _, ok := c.clusters[a]; !ok {
			return "", ErrUnknownCluster
		}
		return a, nil
	}

	left, leftOK := c.clusters[a]
	if !leftOK {
		return "", ErrUnknownCluster
	}
	right, rightOK := c.clusters[b]
	if !rightOK {
		return "", ErrUnknownCluster
	}

	members := unionSorted(left.members, right.members)
	if exceedsCap(c.cfg.MaxMembers, len(members)) {
		return "", ErrMemberLimit
	}

	confidence := min(left.confidence, right.confidence) * MergeDecayKeep / MergeDecayDenom
	lastSeen := left.lastSeen
	if right.lastSeen.After(lastSeen) {
		lastSeen = right.lastSeen
	}
	evidence := make([]evidenceRecord, 0, len(left.evidence)+len(right.evidence))
	evidence = append(evidence, left.evidence...)
	evidence = append(evidence, right.evidence...)

	mergedID := c.nextClusterIDLocked()
	merged := &clusterState{
		id:         mergedID,
		members:    members,
		confidence: confidence,
		pinned:     left.pinned || right.pinned,
		lastSeen:   lastSeen,
		evidence:   evidence,
	}
	c.clusters[mergedID] = merged
	for _, memberID := range members {
		c.obsCluster[memberID] = mergedID
	}
	delete(c.clusters, a)
	delete(c.clusters, b)

	return mergedID, nil
}

// Split breaks one cluster into one cluster per member and returns the new ids.
func (c *AssetFingerprintClusterer) Split(clusterID string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return nil, ErrUnknownCluster
	}

	memberCount := len(cluster.members)
	projectedClusters := len(c.clusters) - 1 + memberCount
	if exceedsCap(c.cfg.MaxClusters, projectedClusters) {
		return nil, ErrClusterLimit
	}
	if exceedsCap(c.cfg.MaxMembers, 1) {
		return nil, ErrMemberLimit
	}

	ids := make([]string, 0, memberCount)
	for _, memberID := range cluster.members {
		id := c.nextClusterIDLocked()
		seenAt := time.Time{}
		if observation, exists := c.observations[memberID]; exists {
			seenAt = observation.SeenAt
		}
		c.clusters[id] = &clusterState{
			id:         id,
			members:    []string{memberID},
			confidence: MaxConfidence,
			lastSeen:   seenAt,
		}
		c.obsCluster[memberID] = id
		ids = append(ids, id)
	}
	delete(c.clusters, clusterID)

	return ids, nil
}

// Explain lists the evidence behind a cluster, strongest signal first.
func (c *AssetFingerprintClusterer) Explain(clusterID string) ([]Signal, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return nil, ErrUnknownCluster
	}

	signals := make([]Signal, 0, len(cluster.evidence))
	for _, evidence := range cluster.evidence {
		signals = append(signals, evidence.Signal)
	}
	sortSignals(signals)
	return signals, nil
}

// Cluster reads one cluster.
func (c *AssetFingerprintClusterer) Cluster(clusterID string) (Cluster, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return Cluster{}, false
	}
	return Cluster{
		ID:         cluster.id,
		Members:    append([]string(nil), cluster.members...),
		Confidence: cluster.confidence,
		Pinned:     cluster.pinned,
	}, true
}

// Clusters lists the live cluster ids in ascending order.
func (c *AssetFingerprintClusterer) Clusters() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]string, 0, len(c.clusters))
	for id := range c.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Members lists the observation ids of one cluster in ascending order.
func (c *AssetFingerprintClusterer) Members(clusterID string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return []string{}
	}
	return append([]string(nil), cluster.members...)
}

// Confidence reads the confidence of one cluster.
func (c *AssetFingerprintClusterer) Confidence(clusterID string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return 0
	}
	return cluster.confidence
}

// Evict drops clusters whose newest observation is older than the TTL.
func (c *AssetFingerprintClusterer) Evict(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	evicted := 0
	for id, cluster := range c.clusters {
		if cluster.pinned {
			continue
		}
		if now.Sub(cluster.lastSeen) <= c.cfg.TTL {
			continue
		}
		for _, memberID := range cluster.members {
			delete(c.obsCluster, memberID)
			delete(c.observations, memberID)
		}
		delete(c.clusters, id)
		evicted++
	}
	return evicted
}

// Snapshot serialises observations, clusters and the pinned flags.
func (c *AssetFingerprintClusterer) Snapshot() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	dto := snapshotData{
		Version:     snapshotVersion,
		NextCluster: c.nextCluster,
	}

	observationIDs := make([]string, 0, len(c.observations))
	for id := range c.observations {
		observationIDs = append(observationIDs, id)
	}
	sort.Strings(observationIDs)
	for _, id := range observationIDs {
		dto.Observations = append(dto.Observations, c.observations[id])
	}

	clusterIDs := make([]string, 0, len(c.clusters))
	for id := range c.clusters {
		clusterIDs = append(clusterIDs, id)
	}
	sort.Strings(clusterIDs)
	for _, id := range clusterIDs {
		cluster := c.clusters[id]
		dto.Clusters = append(dto.Clusters, snapshotCluster{
			ID:         cluster.id,
			Members:    append([]string(nil), cluster.members...),
			Confidence: cluster.confidence,
			Pinned:     cluster.pinned,
			LastSeen:   cluster.lastSeen,
			Evidence:   append([]evidenceRecord(nil), cluster.evidence...),
		})
	}

	data, err := json.MarshalIndent(dto, "", "  ")
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *AssetFingerprintClusterer) Restore(data []byte) error {
	dto, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	observations := make(map[string]Observation, len(dto.Observations))
	for _, observation := range dto.Observations {
		if strings.TrimSpace(observation.ObsID) == "" || strings.TrimSpace(observation.Host) == "" {
			return ErrBadSnapshot
		}
		if _, exists := observations[observation.ObsID]; exists {
			return ErrBadSnapshot
		}
		observations[observation.ObsID] = cloneObservation(observation)
	}

	clusters := make(map[string]*clusterState, len(dto.Clusters))
	obsCluster := make(map[string]string, len(observations))
	nextCluster := dto.NextCluster

	for _, saved := range dto.Clusters {
		if strings.TrimSpace(saved.ID) == "" {
			return ErrBadSnapshot
		}
		if _, exists := clusters[saved.ID]; exists {
			return ErrBadSnapshot
		}
		if len(saved.Members) == 0 {
			return ErrBadSnapshot
		}
		if saved.Confidence < 0 || saved.Confidence > MaxConfidence {
			return ErrBadSnapshot
		}

		members := make([]string, 0, len(saved.Members))
		lastSeen := time.Time{}
		for _, memberID := range saved.Members {
			if strings.TrimSpace(memberID) == "" {
				return ErrBadSnapshot
			}
			observation, exists := observations[memberID]
			if !exists {
				return ErrBadSnapshot
			}
			if _, assigned := obsCluster[memberID]; assigned {
				return ErrBadSnapshot
			}
			obsCluster[memberID] = saved.ID
			members = append(members, memberID)
			if observation.SeenAt.After(lastSeen) {
				lastSeen = observation.SeenAt
			}
		}
		sort.Strings(members)

		for _, evidence := range saved.Evidence {
			if evidence.Weight < 0 || evidence.Weight > MaxConfidence {
				return ErrBadSnapshot
			}
			if !knownSignalName(evidence.Name) {
				return ErrBadSnapshot
			}
		}

		clusters[saved.ID] = &clusterState{
			id:         saved.ID,
			members:    members,
			confidence: saved.Confidence,
			pinned:     saved.Pinned,
			lastSeen:   lastSeen,
			evidence:   append([]evidenceRecord(nil), saved.Evidence...),
		}
		if sequence, ok := clusterSequence(saved.ID); ok && sequence > nextCluster {
			nextCluster = sequence
		}
	}

	if len(obsCluster) != len(observations) {
		return ErrBadSnapshot
	}
	if nextCluster < 0 {
		return ErrBadSnapshot
	}

	c.mu.Lock()
	c.nextCluster = nextCluster
	c.observations = observations
	c.obsCluster = obsCluster
	c.clusters = clusters
	c.mu.Unlock()
	return nil
}

func decodeSnapshot(data []byte) (snapshotData, error) {
	var dto snapshotData
	if len(bytes.TrimSpace(data)) == 0 {
		return dto, ErrBadSnapshot
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&dto); err != nil {
		return dto, ErrBadSnapshot
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return dto, ErrBadSnapshot
		}
		return dto, ErrBadSnapshot
	}
	if dto.Version != snapshotVersion {
		return dto, ErrBadSnapshot
	}
	return dto, nil
}

func (c *AssetFingerprintClusterer) nextClusterIDLocked() string {
	c.nextCluster++
	return fmt.Sprintf("cluster-%d", c.nextCluster)
}

func (c *AssetFingerprintClusterer) storeObservationLocked(obs Observation, clusterID string) {
	c.observations[obs.ObsID] = cloneObservation(obs)
	c.obsCluster[obs.ObsID] = clusterID
}

func cloneObservation(obs Observation) Observation {
	obs.SANs = append([]string(nil), obs.SANs...)
	obs.HeaderOrder = append([]string(nil), obs.HeaderOrder...)
	return obs
}

func compareObservations(previous, incoming Observation) (int, []evidenceRecord, bool) {
	certKey := strings.TrimSpace(previous.CertPubKey)
	incomingCertKey := strings.TrimSpace(incoming.CertPubKey)
	if certKey != "" && incomingCertKey != "" && certKey != incomingCertKey {
		return 0, nil, false
	}

	evidence := make([]evidenceRecord, 0, 6)
	add := func(signal Signal, value string, extra string) {
		signal.Detail = matchDetail(previous.ObsID, incoming.ObsID, signal.Name, value, extra)
		evidence = append(evidence, evidenceRecord{
			Signal: signal,
			ObsA:   previous.ObsID,
			ObsB:   incoming.ObsID,
		})
	}

	if certKey != "" && certKey == incomingCertKey {
		weight := WeightCertPubKey
		extra := ""
		if isSharedCDNCert(certKey) {
			weight = reducedGenericWeight
			extra = fmt.Sprintf("known shared content-distribution certificate; weight reduced from %d to %d", WeightCertPubKey, weight)
		}
		add(Signal{Name: SignalCertPubKey, Weight: weight}, certKey, extra)
	}

	if sansEqual(previous.SANs, incoming.SANs) {
		value := strings.Join(normalizeSet(previous.SANs), ",")
		add(Signal{Name: SignalSANSet, Weight: WeightSANSet}, value, "")
	}

	if favicon := strings.TrimSpace(previous.FaviconHash); favicon != "" && favicon == strings.TrimSpace(incoming.FaviconHash) {
		weight := WeightFavicon
		extra := ""
		if isDefaultErrorFavicon(favicon) {
			weight = reducedGenericWeight
			extra = fmt.Sprintf("known default error-page favicon; weight reduced from %d to %d", WeightFavicon, weight)
		}
		add(Signal{Name: SignalFavicon, Weight: weight}, favicon, extra)
	}

	if headersEqual(previous.HeaderOrder, incoming.HeaderOrder) {
		value := strings.Join(normalizeHeaders(previous.HeaderOrder), ",")
		add(Signal{Name: SignalHeaderOrder, Weight: WeightHeaderOrder}, value, "")
	}

	if server := strings.TrimSpace(previous.ServerHeader); server != "" && server == strings.TrimSpace(incoming.ServerHeader) {
		add(Signal{Name: SignalServerHeader, Weight: WeightServerHeader}, server, "")
	}

	if redirect := strings.TrimSpace(previous.RedirectFinal); redirect != "" && redirect == strings.TrimSpace(incoming.RedirectFinal) {
		add(Signal{Name: SignalRedirectFinal, Weight: WeightRedirectFinal}, redirect, "")
	}

	sortEvidence(evidence)
	score := 0
	for _, item := range evidence {
		score += item.Weight
	}
	return score, evidence, true
}

func matchDetail(obsA, obsB, name, value, extra string) string {
	detail := fmt.Sprintf("observations %q and %q matched %s at %q", obsA, obsB, name, value)
	if extra != "" {
		detail += "; " + extra
	}
	return detail
}

func normalizeSet(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(set))
	for value := range set {
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized
}

func sansEqual(a, b []string) bool {
	left := normalizeSet(a)
	right := normalizeSet(b)
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func normalizeHeaders(headers []string) []string {
	normalized := make([]string, 0, len(headers))
	for _, header := range headers {
		value := strings.ToLower(strings.TrimSpace(header))
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func headersEqual(a, b []string) bool {
	left := normalizeHeaders(a)
	right := normalizeHeaders(b)
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func isDefaultErrorFavicon(hash string) bool {
	normalized := strings.ToLower(strings.TrimSpace(hash))
	return containsAny(normalized,
		"default",
		"error-page",
		"error_page",
		"defaulterror",
		"http-error",
		"batch-default",
	)
}

func isSharedCDNCert(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return containsAny(normalized,
		"cdn",
		"cloudfront",
		"akamai",
		"fastly",
		"cloudflare",
		"shared",
		"shared-content",
		"content-distribution",
		"content_delivery",
	)
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func knownSignalName(name string) bool {
	switch name {
	case SignalCertPubKey, SignalSANSet, SignalHeaderOrder, SignalServerHeader, SignalFavicon, SignalRedirectFinal:
		return true
	default:
		return false
	}
}

func sortEvidence(evidence []evidenceRecord) {
	sort.SliceStable(evidence, func(i, j int) bool {
		left := evidence[i].Signal
		right := evidence[j].Signal
		if left.Weight != right.Weight {
			return left.Weight > right.Weight
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Detail < right.Detail
	})
}

func sortSignals(signals []Signal) {
	sort.SliceStable(signals, func(i, j int) bool {
		if signals[i].Weight != signals[j].Weight {
			return signals[i].Weight > signals[j].Weight
		}
		if signals[i].Name != signals[j].Name {
			return signals[i].Name < signals[j].Name
		}
		return signals[i].Detail < signals[j].Detail
	})
}

func insertSorted(values []string, value string) []string {
	index := sort.SearchStrings(values, value)
	if index < len(values) && values[index] == value {
		return values
	}
	values = append(values, "")
	copy(values[index+1:], values[index:])
	values[index] = value
	return values
}

func unionSorted(a, b []string) []string {
	union := make([]string, 0, len(a)+len(b))
	union = append(union, a...)
	union = append(union, b...)
	sort.Strings(union)

	deduped := union[:0]
	for i, value := range union {
		if i == 0 || value != union[i-1] {
			deduped = append(deduped, value)
		}
	}
	return append([]string(nil), deduped...)
}

func lessCandidate(clusterA, memberA, clusterB, memberB string) bool {
	if clusterA != clusterB {
		return clusterA < clusterB
	}
	return memberA < memberB
}

func exceedsCap(limit, resultingSize int) bool {
	return limit >= 0 && resultingSize > limit
}

func clusterSequence(id string) (int, bool) {
	const prefix = "cluster-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	sequence, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if err != nil || sequence < 0 {
		return 0, false
	}
	return sequence, true
}
