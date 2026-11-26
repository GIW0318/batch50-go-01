// Package task clusters fuzzing crashes by their crash fingerprint and grades
// each cluster by write controllability under a monotonic policy.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Exploitability levels.
const (
	LevelNone        = 0
	LevelTentative   = 1
	LevelProbable    = 2
	LevelExploitable = 3
)

// Labels rendered for each level.
const (
	LabelNone        = "none"
	LabelTentative   = "tentative"
	LabelProbable    = "probable"
	LabelExploitable = "exploitable"
)

// Reason codes recorded on every grade change.
const (
	ReasonGradeRaised      = "reason_grade_raised"
	ReasonGradeKept        = "reason_grade_kept_monotonic"
	ReasonRecheckDowngrade = "reason_recheck_downgrade"
	ReasonRecheckKeep      = "reason_recheck_keep"
)

// Errors reported by the classifier.
var (
	ErrBlankSample          = errors.New("sample needs an input id, a frame and a fault address")
	ErrClusterLimit         = errors.New("cluster limit reached")
	ErrUnknownCrash         = errors.New("unknown crash")
	ErrUnknownCluster       = errors.New("unknown cluster")
	ErrInsufficientEvidence = errors.New("not enough retained samples to grade")
	ErrMergeSelf            = errors.New("a cluster cannot be merged into itself")
	ErrClusterMerged        = errors.New("cluster was already merged")
	ErrBadSnapshot          = errors.New("snapshot payload is invalid")
)

// Frame is one stack frame captured at the fault.
type Frame struct {
	Module   string `json:"module"`
	Function string `json:"function"`
	Offset   int    `json:"offset"`
}

// Sample is one fuzzing crash sample.
type Sample struct {
	InputID         string            `json:"input_id"`
	Frames          []Frame           `json:"frames"`
	FaultAddress    string            `json:"fault_address"`
	Registers       map[string]string `json:"registers"`
	WriteControlled bool              `json:"write_controlled"`
	Reproducibility int               `json:"reproducibility"`
	Signal          string            `json:"signal"`
}

// ClusterView is a read only view of one crash cluster.
type ClusterView struct {
	ID          string   `json:"id"`
	Fingerprint string   `json:"fingerprint"`
	Crashes     []string `json:"crashes"`
	Level       int      `json:"level"`
	Label       string   `json:"label"`
	MergedInto  string   `json:"merged_into"`
}

// GradeChange is one entry of the grade history.
type GradeChange struct {
	Seq    int    `json:"seq"`
	Level  int    `json:"level"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// Grade is the current exploitability grade of one cluster.
type Grade struct {
	ClusterID string        `json:"cluster_id"`
	Level     int           `json:"level"`
	Label     string        `json:"label"`
	Reason    string        `json:"reason"`
	History   []GradeChange `json:"history"`
}

// Options bounds the cluster table and the grading policy.
type Options struct {
	MaxClusters          int
	MaxSamplesPerCluster int
	MinSamplesForGrade   int
	MinReproducibility   int
}

// CrashTriageClassifier owns the clusters, the sample retention and the grades.
type CrashTriageClassifier struct {
	mu          sync.Mutex
	opts        Options
	clusters    map[string]*clusterState
	order       []string
	crashIndex  map[string]string
	nextCluster int
	nextCrash   int
}

// retainedSample is one crash sample kept inside a cluster window.
type retainedSample struct {
	CrashID         string `json:"crash_id"`
	WriteControlled bool   `json:"write_controlled"`
	Reproducibility int    `json:"reproducibility"`
}

// clusterState is the mutable state of one cluster.
type clusterState struct {
	ID          string           `json:"id"`
	Fingerprint string           `json:"fingerprint"`
	Retained    []retainedSample `json:"retained"`
	Level       int              `json:"level"`
	History     []GradeChange    `json:"history"`
	MergedInto  string           `json:"merged_into"`
}

func (cl *clusterState) merged() bool { return cl.MergedInto != "" }

// snapshotPayload is the serialised form of the whole classifier.
type snapshotPayload struct {
	Version     int                      `json:"version"`
	Opts        Options                  `json:"opts"`
	NextCluster int                      `json:"next_cluster"`
	NextCrash   int                      `json:"next_crash"`
	Order       []string                 `json:"order"`
	Clusters    map[string]*clusterState `json:"clusters"`
	CrashIndex  map[string]string        `json:"crash_index"`
}

// NewCrashTriageClassifier builds a classifier with the given caps.
func NewCrashTriageClassifier(opts Options) *CrashTriageClassifier {
	return &CrashTriageClassifier{
		opts:        opts,
		clusters:    make(map[string]*clusterState),
		crashIndex:  make(map[string]string),
		nextCluster: 1,
		nextCrash:   1,
	}
}

// labelFor renders the label paired with a level.
func labelFor(level int) string {
	switch level {
	case LevelTentative:
		return LabelTentative
	case LevelProbable:
		return LabelProbable
	case LevelExploitable:
		return LabelExploitable
	default:
		return LabelNone
	}
}

// fingerprintOf hashes the stack frames, the fault address and the sorted
// register pairs; input ids and grading signals never take part.
func fingerprintOf(sample Sample) string {
	var b strings.Builder
	for _, frame := range sample.Frames {
		fmt.Fprintf(&b, "frame\x00%s\x00%s\x00%d\x00", frame.Module, frame.Function, frame.Offset)
	}
	fmt.Fprintf(&b, "fault\x00%s\x00", sample.FaultAddress)
	keys := make([]string, 0, len(sample.Registers))
	for key := range sample.Registers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "reg\x00%s\x00%s\x00", key, sample.Registers[key])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// activeClusters counts the clusters that are not merged away.
func (c *CrashTriageClassifier) activeClusters() int {
	active := 0
	for _, id := range c.order {
		if !c.clusters[id].merged() {
			active++
		}
	}
	return active
}

// trimLocked enforces the per-cluster retention window, evicting the oldest
// samples together with their crash ids.
func (c *CrashTriageClassifier) trimLocked(cl *clusterState) {
	if c.opts.MaxSamplesPerCluster <= 0 {
		return
	}
	for len(cl.Retained) > c.opts.MaxSamplesPerCluster {
		delete(c.crashIndex, cl.Retained[0].CrashID)
		cl.Retained = cl.Retained[1:]
	}
}

// Submit files one crash sample under its fingerprint cluster.
func (c *CrashTriageClassifier) Submit(sample Sample) (string, error) {
	if strings.TrimSpace(sample.InputID) == "" || len(sample.Frames) == 0 ||
		strings.TrimSpace(sample.FaultAddress) == "" {
		return "", ErrBlankSample
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	fingerprint := fingerprintOf(sample)
	var cluster *clusterState
	for _, id := range c.order {
		candidate := c.clusters[id]
		if !candidate.merged() && candidate.Fingerprint == fingerprint {
			cluster = candidate
			break
		}
	}
	if cluster == nil {
		if c.opts.MaxClusters > 0 && c.activeClusters() >= c.opts.MaxClusters {
			return "", ErrClusterLimit
		}
		cluster = &clusterState{
			ID:          fmt.Sprintf("cluster-%06d", c.nextCluster),
			Fingerprint: fingerprint,
		}
		c.nextCluster++
		c.clusters[cluster.ID] = cluster
		c.order = append(c.order, cluster.ID)
	}

	crashID := fmt.Sprintf("crash-%06d", c.nextCrash)
	c.nextCrash++
	cluster.Retained = append(cluster.Retained, retainedSample{
		CrashID:         crashID,
		WriteControlled: sample.WriteControlled,
		Reproducibility: sample.Reproducibility,
	})
	c.crashIndex[crashID] = cluster.ID
	c.trimLocked(cluster)
	return crashID, nil
}

// Cluster reports the cluster currently holding one crash.
func (c *CrashTriageClassifier) Cluster(crashID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clusterID, ok := c.crashIndex[crashID]
	if !ok {
		return "", ErrUnknownCrash
	}
	return clusterID, nil
}

// computeLevel derives the grade from the retained samples.
func computeLevel(retained []retainedSample, minReproducibility int) int {
	allLow := true
	controlled := false
	for _, sample := range retained {
		if sample.Reproducibility >= minReproducibility {
			allLow = false
		}
		if sample.WriteControlled {
			controlled = true
		}
	}
	if allLow {
		return LevelTentative
	}
	if controlled {
		return LevelExploitable
	}
	return LevelProbable
}

// appendHistory records one grade change with the next sequence number.
func appendHistory(cl *clusterState, level int, reason string) {
	cl.History = append(cl.History, GradeChange{
		Seq:    len(cl.History) + 1,
		Level:  level,
		Label:  labelFor(level),
		Reason: reason,
	})
}

// gradeView builds the public grade of a cluster.
func gradeView(cl *clusterState, reason string) Grade {
	history := make([]GradeChange, len(cl.History))
	copy(history, cl.History)
	return Grade{
		ClusterID: cl.ID,
		Level:     cl.Level,
		Label:     labelFor(cl.Level),
		Reason:    reason,
		History:   history,
	}
}

// activeCluster resolves a cluster id to its unmerged state.
func (c *CrashTriageClassifier) activeCluster(clusterID string) (*clusterState, error) {
	cluster, ok := c.clusters[clusterID]
	if !ok {
		return nil, ErrUnknownCluster
	}
	if cluster.merged() {
		return nil, ErrClusterMerged
	}
	return cluster, nil
}

// gradeableCluster resolves a cluster and checks the evidence floor.
func (c *CrashTriageClassifier) gradeableCluster(clusterID string) (*clusterState, error) {
	cluster, err := c.activeCluster(clusterID)
	if err != nil {
		return nil, err
	}
	if len(cluster.Retained) < c.opts.MinSamplesForGrade {
		return nil, ErrInsufficientEvidence
	}
	return cluster, nil
}

// Grade computes and applies the monotonic grade of one cluster.
func (c *CrashTriageClassifier) Grade(clusterID string) (Grade, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster, err := c.gradeableCluster(clusterID)
	if err != nil {
		return Grade{}, err
	}
	computed := computeLevel(cluster.Retained, c.opts.MinReproducibility)
	switch {
	case computed > cluster.Level:
		cluster.Level = computed
		appendHistory(cluster, computed, ReasonGradeRaised)
		return gradeView(cluster, ReasonGradeRaised), nil
	case computed < cluster.Level:
		appendHistory(cluster, cluster.Level, ReasonGradeKept)
		return gradeView(cluster, ReasonGradeKept), nil
	default:
		return gradeView(cluster, ReasonGradeKept), nil
	}
}

// Recheck recomputes the grade with a formal review that may lower it.
func (c *CrashTriageClassifier) Recheck(clusterID string) (Grade, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster, err := c.gradeableCluster(clusterID)
	if err != nil {
		return Grade{}, err
	}
	computed := computeLevel(cluster.Retained, c.opts.MinReproducibility)
	if computed < cluster.Level {
		cluster.Level = computed
		appendHistory(cluster, computed, ReasonRecheckDowngrade)
		return gradeView(cluster, ReasonRecheckDowngrade), nil
	}
	appendHistory(cluster, cluster.Level, ReasonRecheckKeep)
	return gradeView(cluster, ReasonRecheckKeep), nil
}

// Merge folds cluster a into cluster b and keeps b's grading policy.
func (c *CrashTriageClassifier) Merge(a, b string) error {
	if a == b {
		return ErrMergeSelf
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	source, ok := c.clusters[a]
	if !ok {
		return ErrUnknownCluster
	}
	target, ok := c.clusters[b]
	if !ok {
		return ErrUnknownCluster
	}
	if source.merged() || target.merged() {
		return ErrClusterMerged
	}

	for _, sample := range source.Retained {
		target.Retained = append(target.Retained, sample)
		c.crashIndex[sample.CrashID] = target.ID
	}
	source.Retained = nil
	source.MergedInto = target.ID
	c.trimLocked(target)
	return nil
}

// Clusters lists the active clusters, ordered by id.
func (c *CrashTriageClassifier) Clusters() []ClusterView {
	c.mu.Lock()
	defer c.mu.Unlock()

	views := make([]ClusterView, 0, len(c.order))
	for _, id := range c.order {
		cluster := c.clusters[id]
		if cluster.merged() {
			continue
		}
		views = append(views, clusterView(cluster))
	}
	return views
}

// clusterView snapshots one cluster for read only consumers.
func clusterView(cluster *clusterState) ClusterView {
	crashes := make([]string, len(cluster.Retained))
	for i, sample := range cluster.Retained {
		crashes[i] = sample.CrashID
	}
	return ClusterView{
		ID:          cluster.ID,
		Fingerprint: cluster.Fingerprint,
		Crashes:     crashes,
		Level:       cluster.Level,
		Label:       labelFor(cluster.Level),
		MergedInto:  cluster.MergedInto,
	}
}

// Crashes lists the retained crashes of one cluster, oldest first.
func (c *CrashTriageClassifier) Crashes(clusterID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return nil
	}
	crashes := make([]string, len(cluster.Retained))
	for i, sample := range cluster.Retained {
		crashes[i] = sample.CrashID
	}
	return crashes
}

// History returns the grade history of one cluster.
func (c *CrashTriageClassifier) History(clusterID string) []GradeChange {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster, ok := c.clusters[clusterID]
	if !ok {
		return nil
	}
	history := make([]GradeChange, len(cluster.History))
	copy(history, cluster.History)
	return history
}

// Snapshot serialises clusters, crash assignment and grade history.
func (c *CrashTriageClassifier) Snapshot() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	payload := snapshotPayload{
		Version:     1,
		Opts:        c.opts,
		NextCluster: c.nextCluster,
		NextCrash:   c.nextCrash,
		Order:       append([]string(nil), c.order...),
		Clusters:    make(map[string]*clusterState, len(c.clusters)),
		CrashIndex:  make(map[string]string, len(c.crashIndex)),
	}
	for id, cluster := range c.clusters {
		clone := *cluster
		clone.Retained = append([]retainedSample(nil), cluster.Retained...)
		clone.History = append([]GradeChange(nil), cluster.History...)
		payload.Clusters[id] = &clone
	}
	for crashID, clusterID := range c.crashIndex {
		payload.CrashIndex[crashID] = clusterID
	}
	return json.Marshal(payload)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *CrashTriageClassifier) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Version != 1 || payload.Clusters == nil || payload.CrashIndex == nil {
		return ErrBadSnapshot
	}
	for _, id := range payload.Order {
		if _, ok := payload.Clusters[id]; !ok {
			return ErrBadSnapshot
		}
	}
	for crashID, clusterID := range payload.CrashIndex {
		if strings.TrimSpace(crashID) == "" {
			return ErrBadSnapshot
		}
		if _, ok := payload.Clusters[clusterID]; !ok {
			return ErrBadSnapshot
		}
	}

	clusters := make(map[string]*clusterState, len(payload.Clusters))
	for id, cluster := range payload.Clusters {
		clone := *cluster
		clone.Retained = append([]retainedSample(nil), cluster.Retained...)
		clone.History = append([]GradeChange(nil), cluster.History...)
		clusters[id] = &clone
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts = payload.Opts
	c.clusters = clusters
	c.order = append([]string(nil), payload.Order...)
	c.crashIndex = make(map[string]string, len(payload.CrashIndex))
	for crashID, clusterID := range payload.CrashIndex {
		c.crashIndex[crashID] = clusterID
	}
	c.nextCluster = payload.NextCluster
	c.nextCrash = payload.NextCrash
	if c.nextCluster < 1 {
		c.nextCluster = 1
	}
	if c.nextCrash < 1 {
		c.nextCrash = 1
	}
	return nil
}
