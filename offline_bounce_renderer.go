// Package task renders projects offline in fixed audio blocks: it advances
// faster than real time, flushes the tail, aligns plugin latency and resumes
// interrupted jobs from the last finished block.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Job states reported by Status.
const (
	JobRunning   = "running"
	JobDone      = "done"
	JobCancelled = "cancelled"
)

// JobID identifies one bounce job.
type JobID int64

// BounceJob is one offline render request.
type BounceJob struct {
	Project    string
	SampleRate int
	BlockSize  int
	Blocks     int
	TrackRings []int
}

// Options bounds the renderer. Zero values fall back to four concurrent jobs,
// 64 blocks per advance and a ten second tail cap.
type Options struct {
	MaxJobs          int
	MaxBlocksPerCall int
	TailCap          time.Duration
}

// Progress is the observable state of one job.
type Progress struct {
	Job       JobID
	Project   string
	State     string
	Content   int
	Tail      int
	Done      int
	Samples   int64
	Checksum  uint64
	Silent    bool
	LatencyMs float64
}

// Errors reported by the renderer.
var (
	ErrBadJob      = errors.New("bounce job is invalid")
	ErrJobLimit    = errors.New("concurrent job limit reached")
	ErrJobActive   = errors.New("project already has an active job")
	ErrUnknownJob  = errors.New("unknown bounce job")
	ErrJobDone     = errors.New("bounce job already finished")
	ErrBadBlocks   = errors.New("block count is out of range")
	ErrBadTail     = errors.New("tail length is invalid")
	ErrBadLatency  = errors.New("latency compensation is invalid")
	ErrBadSnapshot = errors.New("snapshot payload is invalid")
)

// OfflineBounceRenderer owns the job table, the render settings and the job id
// watermark.
type OfflineBounceRenderer struct {
	opts Options
}

// NewOfflineBounceRenderer builds a renderer for the given options.
func NewOfflineBounceRenderer(opts Options) *OfflineBounceRenderer {
	return &OfflineBounceRenderer{opts: opts}
}

// Start validates a job and puts it at the head of the render queue.
func (r *OfflineBounceRenderer) Start(job BounceJob) (JobID, error) {
	return 0, errNotImplemented
}

// Advance renders up to blocks more blocks of one job.
func (r *OfflineBounceRenderer) Advance(job JobID, blocks int) (Progress, error) {
	return Progress{}, errNotImplemented
}

// Cancel stops a running job and keeps its finished blocks.
func (r *OfflineBounceRenderer) Cancel(job JobID) error {
	return errNotImplemented
}

// Status reads the progress of one job.
func (r *OfflineBounceRenderer) Status(job JobID) (Progress, error) {
	return Progress{}, errNotImplemented
}

// SetTail replaces the tail length used by jobs started afterwards.
func (r *OfflineBounceRenderer) SetTail(d time.Duration) error {
	return errNotImplemented
}

// Tail returns the active tail length.
func (r *OfflineBounceRenderer) Tail() time.Duration {
	return 0
}

// SetLatencyComp replaces the plugin latency compensation in milliseconds.
func (r *OfflineBounceRenderer) SetLatencyComp(ms float64) error {
	return errNotImplemented
}

// LatencyComp returns the active latency compensation in milliseconds.
func (r *OfflineBounceRenderer) LatencyComp() float64 {
	return 0
}

// Jobs lists the known job ids in ascending order.
func (r *OfflineBounceRenderer) Jobs() []JobID {
	return nil
}

// Limits returns the effective renderer options.
func (r *OfflineBounceRenderer) Limits() Options {
	return Options{}
}

// Snapshot serialises the job table, the settings and the id watermark.
func (r *OfflineBounceRenderer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *OfflineBounceRenderer) Restore(data []byte) error {
	return errNotImplemented
}
