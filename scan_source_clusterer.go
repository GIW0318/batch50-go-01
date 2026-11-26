// Package task clusters scattered probe events into scan activities and
// keeps their lifecycle, including cooling and archiving, across restarts.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Errors returned by the clusterer.
var (
	ErrEmptyEvent           = errors.New("source and target are required")
	ErrDuplicateEvent       = errors.New("event already ingested")
	ErrActivityLimit        = errors.New("tracked activity limit reached")
	ErrEventLimit           = errors.New("event limit reached for this activity")
	ErrUnknownActivity      = errors.New("unknown activity")
	ErrUnknownCluster       = errors.New("unknown cluster")
	ErrInsufficientEvidence = errors.New("not enough evidence to cluster")
	ErrSameActivity         = errors.New("cannot merge an activity with itself")
	ErrBadTTL               = errors.New("cool down ttl must be positive")
	ErrBadSnapshot          = errors.New("snapshot payload is invalid")
)

// snapshotVersion tags the snapshot layout so Restore can reject foreign data.
const snapshotVersion = 1

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

// activityState is the internal record of one source's event stream.
type activityState struct {
	ID        string       `json:"id"`
	Source    string       `json:"source"`
	Events    []ProbeEvent `json:"events"`
	ClusterID string       `json:"cluster_id"`
	Archived  bool         `json:"archived"`
}

// clusterState is the internal record of one scan activity cluster.
type clusterState struct {
	ID           string    `json:"id"`
	ActivityIDs  []string  `json:"activity_ids"`
	CoolingUntil time.Time `json:"cooling_until"`
	Archived     bool      `json:"archived"`
}

// snapshot is the serialised form of the whole clusterer state.
type snapshot struct {
	Version      int               `json:"version"`
	NextActivity int               `json:"next_activity"`
	NextCluster  int               `json:"next_cluster"`
	Activities   []*activityState  `json:"activities"`
	Clusters     []*clusterState   `json:"clusters"`
	MergedInto   map[string]string `json:"merged_into"`
	Whitelist    []string          `json:"whitelist"`
}

// ScanSourceClusterer owns activities, clusters and their cool down windows.
type ScanSourceClusterer struct {
	limits Limits

	mu           sync.Mutex
	now          func() time.Time
	activities   map[string]*activityState
	liveBySource map[string]string
	eventIDs     map[string]struct{}
	clusters     map[string]*clusterState
	mergedInto   map[string]string
	whitelist    map[string]struct{}
	nextActivity int
	nextCluster  int
}

// NewScanSourceClusterer builds a clusterer with the given caps.
func NewScanSourceClusterer(limits Limits) *ScanSourceClusterer {
	return &ScanSourceClusterer{
		limits:       limits,
		now:          time.Now,
		activities:   map[string]*activityState{},
		liveBySource: map[string]string{},
		eventIDs:     map[string]struct{}{},
		clusters:     map[string]*clusterState{},
		mergedInto:   map[string]string{},
		whitelist:    map[string]struct{}{},
	}
}

// SetClock replaces the clock used for cool down expiry.
func (c *ScanSourceClusterer) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now != nil {
		c.now = now
	}
}

// Allow marks a source as a trusted monitor whose probes are kept for audit
// but never treated as scan evidence.
func (c *ScanSourceClusterer) Allow(source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.whitelist[source] = struct{}{}
}

// Allowed lists the whitelisted sources in ascending order.
func (c *ScanSourceClusterer) Allowed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.whitelist))
	for source := range c.whitelist {
		out = append(out, source)
	}
	sort.Strings(out)
	return out
}

// Ingest records one probe event under the activity of its source.
func (c *ScanSourceClusterer) Ingest(ev ProbeEvent) (string, error) {
	if ev.Source == "" || ev.Target == "" {
		return "", ErrEmptyEvent
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.eventIDs[ev.ID]; dup {
		return "", ErrDuplicateEvent
	}
	actID, ok := c.liveBySource[ev.Source]
	var act *activityState
	if !ok {
		if c.limits.MaxTrackedActivities > 0 && len(c.liveBySource) >= c.limits.MaxTrackedActivities {
			return "", ErrActivityLimit
		}
		c.nextActivity++
		actID = fmt.Sprintf("act-%06d", c.nextActivity)
		act = &activityState{ID: actID, Source: ev.Source}
		c.activities[actID] = act
		c.liveBySource[ev.Source] = actID
	} else {
		act = c.activities[actID]
	}
	if c.limits.MaxEventsPerActivity > 0 && len(act.Events) >= c.limits.MaxEventsPerActivity {
		return "", ErrEventLimit
	}
	pos := sort.Search(len(act.Events), func(i int) bool { return act.Events[i].At.After(ev.At) })
	act.Events = append(act.Events, ProbeEvent{})
	copy(act.Events[pos+1:], act.Events[pos:])
	act.Events[pos] = ev
	c.eventIDs[ev.ID] = struct{}{}
	return actID, nil
}

// Cluster turns one activity into a cluster once it has enough evidence.
func (c *ScanSourceClusterer) Cluster(activityID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	act, ok := c.activities[activityID]
	if !ok {
		return "", ErrUnknownActivity
	}
	if act.ClusterID != "" {
		return act.ClusterID, nil
	}
	if len(act.Events) < c.limits.MinEventsToCluster {
		return "", ErrInsufficientEvidence
	}
	if _, trusted := c.whitelist[act.Source]; trusted {
		return "", ErrInsufficientEvidence
	}
	c.nextCluster++
	clusterID := fmt.Sprintf("clu-%06d", c.nextCluster)
	c.clusters[clusterID] = &clusterState{ID: clusterID, ActivityIDs: []string{activityID}}
	act.ClusterID = clusterID
	return clusterID, nil
}

// Merge folds two activities into the cluster of the first one.
func (c *ScanSourceClusterer) Merge(a, b string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	actA, ok := c.activities[a]
	if !ok {
		return "", ErrUnknownActivity
	}
	actB, ok := c.activities[b]
	if !ok {
		return "", ErrUnknownActivity
	}
	if a == b {
		return "", ErrSameActivity
	}
	if actA.ClusterID == "" || actB.ClusterID == "" {
		return "", ErrInsufficientEvidence
	}
	idA := c.resolveCluster(actA.ClusterID)
	idB := c.resolveCluster(actB.ClusterID)
	if idA == idB {
		return idA, nil
	}
	clA := c.clusters[idA]
	clB := c.clusters[idB]
	for _, actID := range clB.ActivityIDs {
		c.activities[actID].ClusterID = idA
		clA.ActivityIDs = append(clA.ActivityIDs, actID)
	}
	sort.Strings(clA.ActivityIDs)
	if !clB.CoolingUntil.IsZero() && (clA.CoolingUntil.IsZero() || clB.CoolingUntil.Before(clA.CoolingUntil)) {
		clA.CoolingUntil = clB.CoolingUntil
	}
	delete(c.clusters, idB)
	c.mergedInto[idB] = idA
	return idA, nil
}

// resolveCluster follows merge redirects to the retained cluster id.
func (c *ScanSourceClusterer) resolveCluster(clusterID string) string {
	for {
		next, ok := c.mergedInto[clusterID]
		if !ok {
			return clusterID
		}
		clusterID = next
	}
}

// Explain returns the features of one cluster, archived or live.
func (c *ScanSourceClusterer) Explain(clusterID string) (Features, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.clusters[c.resolveCluster(clusterID)]
	if !ok {
		return Features{}, ErrUnknownCluster
	}
	features := Features{
		ClusterID:    cl.ID,
		CoolingUntil: cl.CoolingUntil,
		Archived:     cl.Archived,
	}
	sources := map[string]struct{}{}
	targets := map[string]struct{}{}
	for _, actID := range cl.ActivityIDs {
		act := c.activities[actID]
		features.Activities = append(features.Activities, actID)
		sources[act.Source] = struct{}{}
		features.Events += len(act.Events)
		for _, ev := range act.Events {
			targets[ev.Target] = struct{}{}
			if features.FirstAt.IsZero() || ev.At.Before(features.FirstAt) {
				features.FirstAt = ev.At
			}
			if ev.At.After(features.LastAt) {
				features.LastAt = ev.At
			}
		}
	}
	sort.Strings(features.Activities)
	for source := range sources {
		features.Sources = append(features.Sources, source)
	}
	sort.Strings(features.Sources)
	for target := range targets {
		features.Targets = append(features.Targets, target)
	}
	sort.Strings(features.Targets)
	return features, nil
}

// CoolDown starts a cool down window on one cluster.
func (c *ScanSourceClusterer) CoolDown(clusterID string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.clusters[c.resolveCluster(clusterID)]
	if !ok {
		return ErrUnknownCluster
	}
	if ttl <= 0 {
		return ErrBadTTL
	}
	cl.CoolingUntil = c.now().Add(ttl)
	return nil
}

// Expire archives every cluster whose cool down window has passed.
func (c *ScanSourceClusterer) Expire() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	archived := 0
	for _, cl := range c.clusters {
		if cl.Archived || cl.CoolingUntil.IsZero() || now.Before(cl.CoolingUntil) {
			continue
		}
		cl.Archived = true
		for _, actID := range cl.ActivityIDs {
			act := c.activities[actID]
			act.Archived = true
			delete(c.liveBySource, act.Source)
		}
		archived++
	}
	return archived, nil
}

// Events lists the events of one activity in ascending time order.
func (c *ScanSourceClusterer) Events(activityID string) ([]ProbeEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	act, ok := c.activities[activityID]
	if !ok {
		return nil, ErrUnknownActivity
	}
	out := make([]ProbeEvent, len(act.Events))
	copy(out, act.Events)
	return out, nil
}

// Activities lists the live activity ids in ascending order.
func (c *ScanSourceClusterer) Activities() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.activities))
	for id, act := range c.activities {
		if !act.Archived {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Archived lists the archived cluster ids in ascending order.
func (c *ScanSourceClusterer) Archived() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.clusters))
	for id, cl := range c.clusters {
		if cl.Archived {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Snapshot serialises activities, events, clusters and cool down windows.
func (c *ScanSourceClusterer) Snapshot() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := snapshot{
		Version:    snapshotVersion,
		Activities: make([]*activityState, 0, len(c.activities)),
		Clusters:   make([]*clusterState, 0, len(c.clusters)),
		MergedInto: make(map[string]string, len(c.mergedInto)),
		Whitelist:  make([]string, 0, len(c.whitelist)),
	}
	snap.NextActivity = c.nextActivity
	snap.NextCluster = c.nextCluster
	for _, act := range c.activities {
		snap.Activities = append(snap.Activities, act)
	}
	sort.Slice(snap.Activities, func(i, j int) bool { return snap.Activities[i].ID < snap.Activities[j].ID })
	for _, cl := range c.clusters {
		snap.Clusters = append(snap.Clusters, cl)
	}
	sort.Slice(snap.Clusters, func(i, j int) bool { return snap.Clusters[i].ID < snap.Clusters[j].ID })
	for from, to := range c.mergedInto {
		snap.MergedInto[from] = to
	}
	for source := range c.whitelist {
		snap.Whitelist = append(snap.Whitelist, source)
	}
	sort.Strings(snap.Whitelist)
	return json.Marshal(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *ScanSourceClusterer) Restore(data []byte) error {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}
	if snap.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	activities := make(map[string]*activityState, len(snap.Activities))
	liveBySource := make(map[string]string, len(snap.Activities))
	eventIDs := make(map[string]struct{})
	for _, act := range snap.Activities {
		if act == nil || act.ID == "" || act.Source == "" {
			return ErrBadSnapshot
		}
		if _, dup := activities[act.ID]; dup {
			return ErrBadSnapshot
		}
		activities[act.ID] = act
		if !act.Archived {
			if _, dup := liveBySource[act.Source]; dup {
				return ErrBadSnapshot
			}
			liveBySource[act.Source] = act.ID
		}
		for _, ev := range act.Events {
			if _, dup := eventIDs[ev.ID]; dup {
				return ErrBadSnapshot
			}
			eventIDs[ev.ID] = struct{}{}
		}
	}
	clusters := make(map[string]*clusterState, len(snap.Clusters))
	for _, cl := range snap.Clusters {
		if cl == nil || cl.ID == "" {
			return ErrBadSnapshot
		}
		if _, dup := clusters[cl.ID]; dup {
			return ErrBadSnapshot
		}
		for _, actID := range cl.ActivityIDs {
			if _, ok := activities[actID]; !ok {
				return ErrBadSnapshot
			}
		}
		clusters[cl.ID] = cl
	}
	mergedInto := make(map[string]string, len(snap.MergedInto))
	for from, to := range snap.MergedInto {
		if from == "" || to == "" {
			return ErrBadSnapshot
		}
		if _, ok := clusters[from]; ok {
			return ErrBadSnapshot
		}
		mergedInto[from] = to
	}
	whitelist := make(map[string]struct{}, len(snap.Whitelist))
	for _, source := range snap.Whitelist {
		if source == "" {
			return ErrBadSnapshot
		}
		whitelist[source] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activities = activities
	c.liveBySource = liveBySource
	c.eventIDs = eventIDs
	c.clusters = clusters
	c.mergedInto = mergedInto
	c.whitelist = whitelist
	c.nextActivity = snap.NextActivity
	c.nextCluster = snap.NextCluster
	return nil
}
