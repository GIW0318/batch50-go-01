// Package task lays one frame's render graph out over a graphics queue and a
// compute queue, keeps cross queue overlap honest and carries jobs that do not
// fit the compute time slice into the next frame.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxJobs       = 256
	DefaultComputeBudget = 1000
	snapshotVersion      = 1
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

type jobRecord struct {
	State          JobState `json:"state"`
	CancelReported bool     `json:"cancel_reported"`
	Deferrals      int      `json:"deferrals"`
}

type snapshotData struct {
	Version        int           `json:"version"`
	Frame          int           `json:"frame"`
	Open           bool          `json:"open"`
	NextHandle     JobHandle     `json:"next_handle"`
	BarrierPending bool          `json:"barrier_pending"`
	Jobs           []snapshotJob `json:"jobs"`
}

type snapshotJob struct {
	State          JobState `json:"state"`
	CancelReported bool     `json:"cancel_reported"`
	Deferrals      int      `json:"deferrals"`
}

// AsyncComputeScheduler owns the job table, the frame counter and the queue
// timelines.
type AsyncComputeScheduler struct {
	mu             sync.RWMutex
	limits         Limits
	frame          int
	open           bool
	nextHandle     JobHandle
	jobs           map[JobHandle]*jobRecord
	order          []JobHandle
	barrierPending bool
}

// NewAsyncComputeScheduler builds a scheduler with the given caps.
func NewAsyncComputeScheduler(limits Limits) *AsyncComputeScheduler {
	if limits.MaxJobs <= 0 {
		limits.MaxJobs = DefaultMaxJobs
	}
	if limits.ComputeBudget <= 0 {
		limits.ComputeBudget = DefaultComputeBudget
	}
	return &AsyncComputeScheduler{
		limits:     limits,
		nextHandle: 1,
		jobs:       make(map[JobHandle]*jobRecord),
		order:      make([]JobHandle, 0),
	}
}

// Submit queues one job on the given hardware queue.
func (s *AsyncComputeScheduler) Submit(queue QueueKind, job JobSpec) (JobHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.open {
		return 0, ErrNoFrame
	}
	if queue != QueueGraphics && queue != QueueCompute {
		return 0, ErrBadQueue
	}
	if err := validateJobSpec(job); err != nil {
		return 0, err
	}
	if s.activeJobsLocked() >= s.limits.MaxJobs {
		return 0, ErrJobLimit
	}

	handle := s.nextHandle
	s.nextHandle++
	state := JobState{
		Handle:   handle,
		Name:     job.Name,
		Queue:    queue,
		Reads:    cloneStrings(job.Reads),
		Writes:   cloneStrings(job.Writes),
		Cost:     job.Cost,
		Priority: job.Priority,
		Frame:    s.frame,
		Status:   StatusPending,
		Waits:    make([]JobHandle, 0),
	}
	s.jobs[handle] = &jobRecord{State: state}
	s.order = append(s.order, handle)
	return handle, nil
}

// Barrier declares that handle may only run after every handle in waits.
func (s *AsyncComputeScheduler) Barrier(handle JobHandle, waits []JobHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	owner, ok := s.jobs[handle]
	if !ok {
		return ErrUnknownJob
	}
	if len(waits) == 0 {
		return ErrBadBarrier
	}
	for _, wait := range waits {
		if wait == handle {
			return ErrBadBarrier
		}
	}
	for _, wait := range waits {
		if _, ok := s.jobs[wait]; !ok {
			return ErrUnknownJob
		}
	}
	for _, wait := range waits {
		if s.jobs[wait].State.Queue == owner.State.Queue {
			return ErrBarrierSameQueue
		}
	}

	merged := make([]JobHandle, 0, len(owner.State.Waits)+len(waits))
	seen := make(map[JobHandle]struct{}, cap(merged))
	addWait := func(wait JobHandle) {
		if _, duplicate := seen[wait]; duplicate {
			return
		}
		seen[wait] = struct{}{}
		merged = append(merged, wait)
	}
	for _, wait := range owner.State.Waits {
		addWait(wait)
	}
	for _, wait := range waits {
		addWait(wait)
	}
	owner.State.Waits = merged
	s.barrierPending = true

	for _, wait := range merged {
		if target := s.jobs[wait]; target != nil && target.State.Status == StatusCancelled {
			s.cascadeCancellationLocked(wait)
		}
	}
	return nil
}

// BeginFrame opens the frame with the given number.
func (s *AsyncComputeScheduler) BeginFrame(frame int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if frame <= s.frame {
		return ErrBadFrame
	}
	s.frame = frame
	s.open = true
	return nil
}

// EndFrame resolves the queue timelines and reports the frame layout.
func (s *AsyncComputeScheduler) EndFrame() (FrameReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.open {
		return FrameReport{}, ErrNoFrame
	}

	for _, handle := range append([]JobHandle(nil), s.order...) {
		if record := s.jobs[handle]; record != nil && record.State.Status == StatusCancelled {
			s.cascadeCancellationLocked(handle)
		}
	}

	report := FrameReport{
		Frame:     s.frame,
		Graphics:  make([]JobHandle, 0),
		Compute:   make([]JobHandle, 0),
		Deferred:  make([]JobHandle, 0),
		Cancelled: make([]JobHandle, 0),
	}
	active := make(map[JobHandle]struct{})
	graphics := make([]JobHandle, 0)
	computeAscending := make([]JobHandle, 0)
	for _, handle := range s.order {
		record := s.jobs[handle]
		if record == nil || !isActiveStatus(record.State.Status) {
			continue
		}
		if _, exists := active[handle]; exists {
			continue
		}
		active[handle] = struct{}{}
		switch record.State.Queue {
		case QueueGraphics:
			graphics = append(graphics, handle)
		case QueueCompute:
			computeAscending = append(computeAscending, handle)
		}
	}

	report.Serialized = s.barrierPending || s.hasActiveCrossQueueBarrierLocked(active)

	for _, handle := range graphics {
		s.jobs[handle].State.Status = StatusDone
		report.Graphics = append(report.Graphics, handle)
	}

	computeOrder, writeParents := s.orderComputeLocked(computeAscending)
	deferredNow := make(map[JobHandle]struct{})
	budgetUsed := 0
	for _, handle := range computeOrder {
		record := s.jobs[handle]
		if s.writePredecessorDeferredLocked(handle, writeParents, deferredNow) {
			s.deferJobLocked(handle)
			deferredNow[handle] = struct{}{}
			report.Deferred = append(report.Deferred, handle)
			continue
		}

		if record.State.Cost > s.limits.ComputeBudget-budgetUsed {
			s.deferJobLocked(handle)
			deferredNow[handle] = struct{}{}
			report.Deferred = append(report.Deferred, handle)
			continue
		}

		record.State.Status = StatusDone
		budgetUsed += record.State.Cost
		report.Compute = append(report.Compute, handle)
	}
	report.ComputeUse = budgetUsed

	for _, handle := range s.order {
		record := s.jobs[handle]
		if record == nil || record.State.Status != StatusCancelled || record.CancelReported {
			continue
		}
		report.Cancelled = append(report.Cancelled, handle)
		record.CancelReported = true
	}

	sort.Slice(report.Deferred, func(i, j int) bool {
		return report.Deferred[i] < report.Deferred[j]
	})

	s.open = false
	s.barrierPending = false
	return report, nil
}

// Cancel drops a job that has not run yet.
func (s *AsyncComputeScheduler) Cancel(handle JobHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.jobs[handle]
	if !ok {
		return ErrUnknownJob
	}
	if record.State.Status == StatusDone {
		return ErrJobDone
	}
	if record.State.Status != StatusCancelled {
		record.State.Status = StatusCancelled
		record.CancelReported = false
	}
	s.cascadeCancellationLocked(handle)
	return nil
}

// Job reads one job record.
func (s *AsyncComputeScheduler) Job(handle JobHandle) (JobState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.jobs[handle]
	if !ok {
		return JobState{}, false
	}
	return cloneJobState(record.State), true
}

// Pending lists the not yet finished jobs of one queue in submission order.
func (s *AsyncComputeScheduler) Pending(queue QueueKind) []JobHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if queue != QueueGraphics && queue != QueueCompute {
		return nil
	}
	pending := make([]JobHandle, 0)
	for _, handle := range s.order {
		record := s.jobs[handle]
		if record != nil && record.State.Queue == queue && isActiveStatus(record.State.Status) {
			pending = append(pending, handle)
		}
	}
	return pending
}

// Depends lists the barriers declared for one job, duplicates removed.
func (s *AsyncComputeScheduler) Depends(handle JobHandle) []JobHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.jobs[handle]
	if !ok {
		return nil
	}
	return cloneHandles(record.State.Waits)
}

// Frame reports the frame number currently open, or the last closed one.
func (s *AsyncComputeScheduler) Frame() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frame
}

// Limits reports the caps in force after defaults have been applied.
func (s *AsyncComputeScheduler) Limits() Limits {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.limits
}

// Snapshot serialises the frame counter, the job table and the handle watermark.
func (s *AsyncComputeScheduler) Snapshot() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data := snapshotData{
		Version:        snapshotVersion,
		Frame:          s.frame,
		Open:           s.open,
		NextHandle:     s.nextHandle,
		BarrierPending: s.barrierPending,
		Jobs:           make([]snapshotJob, 0, len(s.order)),
	}
	for _, handle := range s.order {
		record := s.jobs[handle]
		if record == nil {
			continue
		}
		data.Jobs = append(data.Jobs, snapshotJob{
			State:          cloneJobState(record.State),
			CancelReported: record.CancelReported,
			Deferrals:      record.Deferrals,
		})
	}
	return json.Marshal(data)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *AsyncComputeScheduler) Restore(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var decoded snapshotData
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&decoded); err != nil {
		return err
	}

	restoredJobs := make(map[JobHandle]*jobRecord, len(decoded.Jobs))
	restoredOrder := make([]JobHandle, 0, len(decoded.Jobs))
	for _, saved := range decoded.Jobs {
		state := cloneJobState(saved.State)
		if state.Waits == nil {
			state.Waits = make([]JobHandle, 0)
		}
		record := &jobRecord{
			State:          state,
			CancelReported: saved.CancelReported,
			Deferrals:      saved.Deferrals,
		}
		restoredJobs[state.Handle] = record
		restoredOrder = append(restoredOrder, state.Handle)
	}
	if err := validateRestoredDependencies(restoredJobs); err != nil {
		return err
	}

	s.frame = decoded.Frame
	s.open = decoded.Open
	s.nextHandle = decoded.NextHandle
	s.barrierPending = decoded.BarrierPending
	s.jobs = restoredJobs
	s.order = restoredOrder
	return nil
}

func validateJobSpec(job JobSpec) error {
	if strings.TrimSpace(job.Name) == "" {
		return ErrBadJob
	}
	if job.Cost <= 0 {
		return ErrBadJob
	}
	if hasBlankOrDuplicateResource(job.Reads) || hasBlankOrDuplicateResource(job.Writes) {
		return ErrBadJob
	}
	return nil
}

func hasBlankOrDuplicateResource(resources []string) bool {
	seen := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if resource == "" {
			return true
		}
		if _, duplicate := seen[resource]; duplicate {
			return true
		}
		seen[resource] = struct{}{}
	}
	return false
}

func isActiveStatus(status string) bool {
	return status == StatusPending || status == StatusDeferred
}

func (s *AsyncComputeScheduler) activeJobsLocked() int {
	count := 0
	for _, record := range s.jobs {
		if record != nil && isActiveStatus(record.State.Status) {
			count++
		}
	}
	return count
}

func (s *AsyncComputeScheduler) cascadeCancellationLocked(root JobHandle) {
	queue := []JobHandle{root}
	for index := 0; index < len(queue); index++ {
		upstream := queue[index]
		for _, downstream := range s.order {
			record := s.jobs[downstream]
			if record == nil || !isActiveStatus(record.State.Status) {
				continue
			}
			if !containsHandle(record.State.Waits, upstream) {
				continue
			}
			record.State.Status = StatusCancelled
			record.State.Reason = fmt.Sprintf(
				"job %d was dragged down by cancelled upstream job %d; 被上游取消任务 %d 拖下水",
				downstream, upstream, upstream,
			)
			record.CancelReported = false
			queue = append(queue, downstream)
		}
	}
}

func (s *AsyncComputeScheduler) hasActiveCrossQueueBarrierLocked(active map[JobHandle]struct{}) bool {
	for handle := range active {
		owner := s.jobs[handle]
		if owner == nil {
			continue
		}
		for _, wait := range owner.State.Waits {
			if _, waitActive := active[wait]; !waitActive {
				continue
			}
			target := s.jobs[wait]
			if target != nil && target.State.Queue != owner.State.Queue {
				return true
			}
		}
	}
	return false
}

// orderComputeLocked priority-reorders carry-over work with new work. Write
// hazards still force earlier same-queue writers before later writers.
func (s *AsyncComputeScheduler) orderComputeLocked(ascending []JobHandle) ([]JobHandle, map[JobHandle][]JobHandle) {
	desired := append([]JobHandle(nil), ascending...)
	hasCarryOver := false
	for _, handle := range ascending {
		if s.jobs[handle].State.Status == StatusDeferred {
			hasCarryOver = true
			break
		}
	}
	if hasCarryOver {
		sort.SliceStable(desired, func(i, j int) bool {
			left := s.jobs[desired[i]]
			right := s.jobs[desired[j]]
			leftPriority := left.State.Priority + left.Deferrals
			rightPriority := right.State.Priority + right.Deferrals
			if leftPriority != rightPriority {
				return leftPriority > rightPriority
			}
			return desired[i] < desired[j]
		})
	}

	parents := make(map[JobHandle][]JobHandle, len(ascending))
	children := make(map[JobHandle][]JobHandle)
	indegree := make(map[JobHandle]int, len(ascending))
	lastWriter := make(map[string]JobHandle)
	for _, handle := range ascending {
		record := s.jobs[handle]
		for _, resource := range record.State.Writes {
			parent, exists := lastWriter[resource]
			lastWriter[resource] = handle
			if !exists || parent == handle || containsHandle(parents[handle], parent) {
				continue
			}
			parents[handle] = append(parents[handle], parent)
			children[parent] = append(children[parent], handle)
			indegree[handle]++
		}
	}

	ordered := make([]JobHandle, 0, len(desired))
	for len(desired) > 0 {
		pick := -1
		for index, handle := range desired {
			if indegree[handle] == 0 {
				pick = index
				break
			}
		}
		if pick == -1 {
			ordered = append(ordered, desired...)
			break
		}

		handle := desired[pick]
		desired = append(desired[:pick], desired[pick+1:]...)
		ordered = append(ordered, handle)
		for _, child := range children[handle] {
			indegree[child]--
		}
	}
	return ordered, parents
}

func (s *AsyncComputeScheduler) writePredecessorDeferredLocked(
	handle JobHandle,
	parents map[JobHandle][]JobHandle,
	deferred map[JobHandle]struct{},
) bool {
	for _, parent := range parents[handle] {
		if _, ok := deferred[parent]; ok {
			return true
		}
	}
	return false
}

func (s *AsyncComputeScheduler) deferJobLocked(handle JobHandle) {
	record := s.jobs[handle]
	record.State.Status = StatusDeferred
	record.Deferrals++
}

func containsHandle(handles []JobHandle, target JobHandle) bool {
	for _, handle := range handles {
		if handle == target {
			return true
		}
	}
	return false
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneHandles(values []JobHandle) []JobHandle {
	if values == nil {
		return nil
	}
	cloned := make([]JobHandle, len(values))
	copy(cloned, values)
	return cloned
}

func cloneJobState(state JobState) JobState {
	state.Reads = cloneStrings(state.Reads)
	state.Writes = cloneStrings(state.Writes)
	state.Waits = cloneHandles(state.Waits)
	return state
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if snapshot.Frame < 0 || snapshot.NextHandle <= 0 {
		return fmt.Errorf("%w: bad frame or handle watermark", ErrBadSnapshot)
	}

	seen := make(map[JobHandle]struct{}, len(snapshot.Jobs))
	var previous JobHandle
	maxHandle := JobHandle(0)
	for index := range snapshot.Jobs {
		state := snapshot.Jobs[index].State
		if state.Handle <= 0 {
			return fmt.Errorf("%w: invalid job handle", ErrBadSnapshot)
		}
		if _, duplicate := seen[state.Handle]; duplicate {
			return fmt.Errorf("%w: duplicate job handle", ErrBadSnapshot)
		}
		if state.Handle <= previous {
			return fmt.Errorf("%w: jobs are not in submission order", ErrBadSnapshot)
		}
		previous = state.Handle
		seen[state.Handle] = struct{}{}
		if state.Handle > maxHandle {
			maxHandle = state.Handle
		}
		if state.Queue != QueueGraphics && state.Queue != QueueCompute {
			return fmt.Errorf("%w: invalid queue", ErrBadSnapshot)
		}
		if strings.TrimSpace(state.Name) == "" || state.Cost <= 0 {
			return fmt.Errorf("%w: invalid job", ErrBadSnapshot)
		}
		if hasBlankOrDuplicateResource(state.Reads) || hasBlankOrDuplicateResource(state.Writes) {
			return fmt.Errorf("%w: invalid resources", ErrBadSnapshot)
		}
		if state.Frame < 0 || state.Frame > snapshot.Frame {
			return fmt.Errorf("%w: invalid job frame", ErrBadSnapshot)
		}
		switch state.Status {
		case StatusPending, StatusDone, StatusDeferred, StatusCancelled:
		default:
			return fmt.Errorf("%w: invalid status", ErrBadSnapshot)
		}
		if snapshot.Jobs[index].Deferrals < 0 {
			return fmt.Errorf("%w: invalid deferral count", ErrBadSnapshot)
		}
		if snapshot.Jobs[index].CancelReported && state.Status != StatusCancelled {
			return fmt.Errorf("%w: invalid cancellation marker", ErrBadSnapshot)
		}
		waitSeen := make(map[JobHandle]struct{}, len(state.Waits))
		for _, wait := range state.Waits {
			if wait == state.Handle {
				return fmt.Errorf("%w: self barrier", ErrBadSnapshot)
			}
			if _, duplicate := waitSeen[wait]; duplicate {
				return fmt.Errorf("%w: duplicate barrier", ErrBadSnapshot)
			}
			waitSeen[wait] = struct{}{}
		}
	}
	if snapshot.NextHandle != maxHandle+1 {
		return fmt.Errorf("%w: handle watermark is inconsistent", ErrBadSnapshot)
	}
	return nil
}

func validateRestoredDependencies(jobs map[JobHandle]*jobRecord) error {
	for handle, record := range jobs {
		waitSeen := make(map[JobHandle]struct{}, len(record.State.Waits))
		for _, wait := range record.State.Waits {
			if wait == handle {
				return fmt.Errorf("%w: self barrier", ErrBadSnapshot)
			}
			if _, duplicate := waitSeen[wait]; duplicate {
				return fmt.Errorf("%w: duplicate barrier", ErrBadSnapshot)
			}
			target, ok := jobs[wait]
			if !ok {
				return fmt.Errorf("%w: missing barrier target", ErrBadSnapshot)
			}
			if target.State.Queue == record.State.Queue {
				return fmt.Errorf("%w: same queue barrier", ErrBadSnapshot)
			}
		}
	}
	return nil
}
