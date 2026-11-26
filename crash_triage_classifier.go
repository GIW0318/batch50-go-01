// Package task clusters fuzzing crashes by their crash fingerprint and grades
// each cluster by write controllability under a monotonic policy.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	opts Options
}

// NewCrashTriageClassifier builds a classifier with the given caps.
func NewCrashTriageClassifier(opts Options) *CrashTriageClassifier {
	return &CrashTriageClassifier{opts: opts}
}

// Submit files one crash sample under its fingerprint cluster.
func (c *CrashTriageClassifier) Submit(sample Sample) (string, error) {
	return "", errNotImplemented
}

// Cluster reports the cluster currently holding one crash.
func (c *CrashTriageClassifier) Cluster(crashID string) (string, error) {
	return "", errNotImplemented
}

// Grade computes and applies the monotonic grade of one cluster.
func (c *CrashTriageClassifier) Grade(clusterID string) (Grade, error) {
	return Grade{}, errNotImplemented
}

// Recheck recomputes the grade with a formal review that may lower it.
func (c *CrashTriageClassifier) Recheck(clusterID string) (Grade, error) {
	return Grade{}, errNotImplemented
}

// Merge folds cluster a into cluster b and keeps b's grading policy.
func (c *CrashTriageClassifier) Merge(a, b string) error {
	return errNotImplemented
}

// Clusters lists the active clusters, ordered by id.
func (c *CrashTriageClassifier) Clusters() []ClusterView {
	return nil
}

// Crashes lists the retained crashes of one cluster, oldest first.
func (c *CrashTriageClassifier) Crashes(clusterID string) []string {
	return nil
}

// History returns the grade history of one cluster.
func (c *CrashTriageClassifier) History(clusterID string) []GradeChange {
	return nil
}

// Snapshot serialises clusters, crash assignment and grade history.
func (c *CrashTriageClassifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *CrashTriageClassifier) Restore(data []byte) error {
	return errNotImplemented
}
