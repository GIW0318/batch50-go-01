// Package task lays one frame's render graph out over a graphics queue and a
// compute queue, keeps cross queue overlap honest and carries jobs that do not
// fit the compute time slice into the next frame.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxJobs       = 256
	DefaultComputeBudget = 1000
)

// QueueKind identifies the hardware queue a job is submitted to.
type QueueKind int

// Queue values accepted by Submit. The zero value is not a valid queue.
const (
	QueueGraphics QueueKind = 1
	QueueCompute  QueueKind = 2
)

// Job status values reported by Job and FrameReport.
const (
	StatusPending   = "pending"
	StatusDone      = "done"
	StatusDeferred  = "deferred"
	StatusCancelled = "cancelled"
)

// Errors reported by the scheduler.
var (
	ErrNoFrame          = errors.New("no frame is open")
	ErrBadFrame         = errors.New("frame number must strictly increase")
	ErrBadQueue         = errors.New("unknown queue")
	ErrBadJob           = errors.New("job specification is invalid")
	ErrJobLimit         = errors.New("job table is full")
	ErrUnknownJob       = errors.New("unknown job handle")
	ErrJobDone          = errors.New("job already finished")
	ErrBadBarrier       = errors.New("barrier waits are invalid")
	ErrBarrierSameQueue = errors.New("a cross queue barrier is required")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Limits bounds the live job table and the compute time slice of one frame.
type Limits struct {
	MaxJobs       int
	ComputeBudget int
}

// JobSpec is one unit of GPU work. Reads and Writes name the resources the job
// touches, Cost is the compute time it needs in the same unit as the budget.
type JobSpec struct {
	Name     string
	Reads    []string
	Writes   []string
	Cost     int
	Priority int
}

// JobHandle is a scheduler owned identifier. The zero value is never valid.
type JobHandle int64

// JobState is the record kept for one submitted job.
type JobState struct {
	Handle   JobHandle
	Name     string
	Queue    QueueKind
	Reads    []string
	Writes   []string
	Cost     int
	Priority int
	Frame    int
	Status   string
	Waits    []JobHandle
	Reason   string
}

// FrameReport describes how one frame was laid out.
type FrameReport struct {
	Frame      int
	Graphics   []JobHandle
	Compute    []JobHandle
	Deferred   []JobHandle
	Cancelled  []JobHandle
	Serialized bool
	ComputeUse int
}

// AsyncComputeScheduler owns the job table, the frame counter and the queue
// timelines.
type AsyncComputeScheduler struct {
	limits Limits
}

// NewAsyncComputeScheduler builds a scheduler with the given caps.
func NewAsyncComputeScheduler(limits Limits) *AsyncComputeScheduler {
	return &AsyncComputeScheduler{limits: limits}
}

// Submit queues one job on the given hardware queue.
func (s *AsyncComputeScheduler) Submit(queue QueueKind, job JobSpec) (JobHandle, error) {
	return 0, errNotImplemented
}

// Barrier declares that handle may only run after every handle in waits.
func (s *AsyncComputeScheduler) Barrier(handle JobHandle, waits []JobHandle) error {
	return errNotImplemented
}

// BeginFrame opens the frame with the given number.
func (s *AsyncComputeScheduler) BeginFrame(frame int) error {
	return errNotImplemented
}

// EndFrame resolves the queue timelines and reports the frame layout.
func (s *AsyncComputeScheduler) EndFrame() (FrameReport, error) {
	return FrameReport{}, errNotImplemented
}

// Cancel drops a job that has not run yet.
func (s *AsyncComputeScheduler) Cancel(handle JobHandle) error {
	return errNotImplemented
}

// Job reads one job record.
func (s *AsyncComputeScheduler) Job(handle JobHandle) (JobState, bool) {
	return JobState{}, false
}

// Pending lists the not yet finished jobs of one queue in submission order.
func (s *AsyncComputeScheduler) Pending(queue QueueKind) []JobHandle {
	return nil
}

// Depends lists the barriers declared for one job, duplicates removed.
func (s *AsyncComputeScheduler) Depends(handle JobHandle) []JobHandle {
	return nil
}

// Frame reports the frame number currently open, or the last closed one.
func (s *AsyncComputeScheduler) Frame() int {
	return 0
}

// Limits reports the caps in force after defaults have been applied.
func (s *AsyncComputeScheduler) Limits() Limits {
	return s.limits
}

// Snapshot serialises the frame counter, the job table and the handle watermark.
func (s *AsyncComputeScheduler) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *AsyncComputeScheduler) Restore(data []byte) error {
	return errNotImplemented
}
