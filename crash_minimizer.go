// Package task shrinks a crashing input down to the shortest form that still
// reaches the same crash cluster: every cut is replayed, the best input only
// moves forward after the replay stays in the original cluster, and the replay
// budget is a hard stop.
package task

import "errors"

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Job states.
const (
	StatusRunning   = "running"
	StatusConverged = "converged"
	StatusBudget    = "budget_exhausted"
	StatusCancelled = "cancelled"
)

// Errors reported by the minimizer.
var (
	ErrBadSample       = errors.New("sample data is required")
	ErrJobLimit        = errors.New("too many jobs are tracked")
	ErrUnknownJob      = errors.New("unknown job")
	ErrNoCandidate     = errors.New("no candidate is waiting for a result")
	ErrBudgetExhausted = errors.New("replay budget exhausted")
	ErrJobCancelled    = errors.New("job is cancelled")
	ErrConverged       = errors.New("no further candidate is available")
	ErrDifferentCrash  = errors.New("candidate crashed in a different cluster")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// ClusterID names one crash cluster reported by the target.
type ClusterID string

// Sample is the original crashing input.
type Sample struct {
	ID   string
	Data []byte
}

// Input is one candidate handed to the replayer.
type Input struct {
	Data []byte
}

// Job is one minimisation run.
type Job struct {
	ID      string
	Cluster ClusterID
	Status  string
}

// Progress reports the counters of one job.
type Progress struct {
	Replays  int
	Accepted int
	Rejected int
	BestLen  int
	Status   string
}

// Limits bounds the job table and the replay budget.
type Limits struct {
	MaxReplays int
	MaxJobs    int
}

// CrashMinimizer owns the job table and the trimming state of every job.
type CrashMinimizer struct {
	limits Limits
}

// NewCrashMinimizer builds a minimizer with the given caps.
func NewCrashMinimizer(limits Limits) *CrashMinimizer {
	return &CrashMinimizer{limits: limits}
}

// Start opens a job for one sample and its original crash cluster.
func (m *CrashMinimizer) Start(sample Sample, cluster ClusterID) (Job, error) {
	return Job{}, errNotImplemented
}

// Candidate returns the cut that has to be replayed next.
func (m *CrashMinimizer) Candidate(jobID string) (Input, error) {
	return Input{}, errNotImplemented
}

// Result records the replay outcome of the outstanding candidate.
func (m *CrashMinimizer) Result(jobID string, crashed bool, cluster ClusterID) error {
	return errNotImplemented
}

// Best returns the smallest input proven to keep the original crash cluster.
func (m *CrashMinimizer) Best(jobID string) (Input, error) {
	return Input{}, errNotImplemented
}

// Cancel stops one job and keeps its best input readable.
func (m *CrashMinimizer) Cancel(jobID string) error {
	return errNotImplemented
}

// Progress reports the counters of one job.
func (m *CrashMinimizer) Progress(jobID string) (Progress, error) {
	return Progress{}, errNotImplemented
}

// Status reports the state of one job.
func (m *CrashMinimizer) Status(jobID string) (string, error) {
	return "", errNotImplemented
}

// Jobs lists the tracked jobs in ascending order.
func (m *CrashMinimizer) Jobs() []string {
	return nil
}

// Snapshot serialises the job table and the best inputs.
func (m *CrashMinimizer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the job table with a snapshot, all or nothing.
func (m *CrashMinimizer) Restore(data []byte) error {
	return errNotImplemented
}
