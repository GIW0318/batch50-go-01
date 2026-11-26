// Package task renders projects offline in fixed audio blocks: it advances
// faster than real time, flushes the tail, aligns plugin latency and resumes
// interrupted jobs from the last finished block.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"
)

// Job states reported by Status.
const (
	JobRunning   = "running"
	JobDone      = "done"
	JobCancelled = "cancelled"
)

const (
	defaultMaxJobs          = 4
	defaultMaxBlocksPerCall = 64
	defaultTail             = 2 * time.Second
	defaultTailCap          = 10 * time.Second
	snapshotMagic           = "gsb-offline-bounce-snapshot"
	snapshotVersion         = 1
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
	mu sync.Mutex

	opts      Options
	jobStates map[JobID]*bounceJobState
	order     []JobID
	nextID    JobID
	tail      time.Duration
	latencyMs float64
}

type bounceJobState struct {
	id           JobID
	job          BounceJob
	tailDuration time.Duration
	tailBlocks   int
	latencyMs    float64
	done         int
	state        string
	checksum     uint64
}

// NewOfflineBounceRenderer builds a renderer for the given options.
func NewOfflineBounceRenderer(opts Options) *OfflineBounceRenderer {
	effective := effectiveOptions(opts)
	initialTail := defaultTail
	if initialTail > effective.TailCap {
		initialTail = effective.TailCap
	}
	return &OfflineBounceRenderer{
		opts:      effective,
		jobStates: make(map[JobID]*bounceJobState),
		nextID:    1,
		tail:      initialTail,
	}
}

// Start validates a job and puts it at the head of the render queue.
func (r *OfflineBounceRenderer) Start(job BounceJob) (JobID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := validateJob(job); err != nil {
		return 0, err
	}

	for _, id := range r.order {
		state := r.jobStates[id]
		if state.state == JobRunning && state.job.Project == job.Project {
			return 0, ErrJobActive
		}
	}
	if r.activeJobsLocked() >= r.opts.MaxJobs {
		return 0, ErrJobLimit
	}

	tailBlocks := tailBlocksFor(job, r.tail)
	var reusedDone int
	var reusedChecksum uint64
	for i := len(r.order) - 1; i >= 0; i-- {
		state := r.jobStates[r.order[i]]
		if state.state != JobCancelled || state.done == 0 {
			continue
		}
		if sameJob(state.job, job) && state.tailDuration == r.tail && state.latencyMs == r.latencyMs {
			reusedDone = state.done
			reusedChecksum = state.checksum
			break
		}
	}

	id := r.nextID
	r.jobStates[id] = &bounceJobState{
		id:           id,
		job:          cloneJob(job),
		tailDuration: r.tail,
		tailBlocks:   tailBlocks,
		latencyMs:    r.latencyMs,
		done:         reusedDone,
		state:        JobRunning,
		checksum:     reusedChecksum,
	}
	r.order = append(r.order, id)
	r.nextID++
	return id, nil
}

// Advance renders up to blocks more blocks of one job.
func (r *OfflineBounceRenderer) Advance(job JobID, blocks int) (Progress, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.jobStates[job]
	if !ok {
		return Progress{}, ErrUnknownJob
	}
	if state.state != JobRunning {
		return Progress{}, ErrJobDone
	}
	if blocks <= 0 || blocks > r.opts.MaxBlocksPerCall {
		return Progress{}, ErrBadBlocks
	}

	remaining := state.job.Blocks + state.tailBlocks - state.done
	if blocks > remaining {
		blocks = remaining
	}

	seed := jobSeed(state.job, state.tailDuration, state.latencyMs)
	for i := 0; i < blocks; i++ {
		block := state.done + i
		state.checksum = mixChecksum(state.checksum, blockDigest(seed, state, block))
	}
	state.done += blocks
	if state.done >= state.job.Blocks+state.tailBlocks {
		state.state = JobDone
	}
	return state.progress(), nil
}

// Cancel stops a running job and keeps its finished blocks.
func (r *OfflineBounceRenderer) Cancel(job JobID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.jobStates[job]
	if !ok {
		return ErrUnknownJob
	}
	if state.state != JobRunning {
		return ErrJobDone
	}
	state.state = JobCancelled
	return nil
}

// Status reads the progress of one job.
func (r *OfflineBounceRenderer) Status(job JobID) (Progress, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.jobStates[job]
	if !ok {
		return Progress{}, ErrUnknownJob
	}
	return state.progress(), nil
}

// SetTail replaces the tail length used by jobs started afterwards.
func (r *OfflineBounceRenderer) SetTail(d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if d < 0 || d > r.opts.TailCap {
		return ErrBadTail
	}
	r.tail = d
	return nil
}

// Tail returns the active tail length.
func (r *OfflineBounceRenderer) Tail() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tail
}

// SetLatencyComp replaces the plugin latency compensation in milliseconds.
func (r *OfflineBounceRenderer) SetLatencyComp(ms float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 || ms > 500 {
		return ErrBadLatency
	}
	r.latencyMs = ms
	return nil
}

// LatencyComp returns the active latency compensation in milliseconds.
func (r *OfflineBounceRenderer) LatencyComp() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latencyMs
}

// Jobs lists the known job ids in ascending order.
func (r *OfflineBounceRenderer) Jobs() []JobID {
	r.mu.Lock()
	defer r.mu.Unlock()

	jobs := make([]JobID, len(r.order))
	copy(jobs, r.order)
	return jobs
}

// Limits returns the effective renderer options.
func (r *OfflineBounceRenderer) Limits() Options {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opts
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Payload  json.RawMessage `json:"payload"`
}

type snapshotState struct {
	Options   Options       `json:"options"`
	Tail      time.Duration `json:"tail"`
	LatencyMs float64       `json:"latency_ms"`
	NextID    JobID         `json:"next_id"`
	Jobs      []snapshotJob `json:"jobs"`
}

type snapshotJob struct {
	ID           JobID         `json:"id"`
	Job          BounceJob     `json:"job"`
	TailDuration time.Duration `json:"tail_duration"`
	TailBlocks   int           `json:"tail_blocks"`
	LatencyMs    float64       `json:"latency_ms"`
	Done         int           `json:"done"`
	State        string        `json:"state"`
	Checksum     uint64        `json:"checksum"`
}

// Snapshot serialises the job table, the settings and the id watermark.
func (r *OfflineBounceRenderer) Snapshot() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := snapshotState{
		Options:   r.opts,
		Tail:      r.tail,
		LatencyMs: r.latencyMs,
		NextID:    r.nextID,
		Jobs:      make([]snapshotJob, 0, len(r.order)),
	}
	for _, id := range r.order {
		job := r.jobStates[id]
		state.Jobs = append(state.Jobs, snapshotJob{
			ID:           job.id,
			Job:          cloneJob(job.job),
			TailDuration: job.tailDuration,
			TailBlocks:   job.tailBlocks,
			LatencyMs:    job.latencyMs,
			Done:         job.done,
			State:        job.state,
			Checksum:     job.checksum,
		})
	}

	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(digest[:]),
		Payload:  payload,
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *OfflineBounceRenderer) Restore(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var envelope snapshotEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return ErrBadSnapshot
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrBadSnapshot
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion || len(envelope.Payload) == 0 {
		return ErrBadSnapshot
	}
	digest := sha256.Sum256(envelope.Payload)
	if envelope.Checksum != hex.EncodeToString(digest[:]) {
		return ErrBadSnapshot
	}

	var state snapshotState
	payloadDecoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	payloadDecoder.DisallowUnknownFields()
	if err := payloadDecoder.Decode(&state); err != nil {
		return ErrBadSnapshot
	}
	if err := payloadDecoder.Decode(&extra); err != io.EOF {
		return ErrBadSnapshot
	}
	if state.Options.MaxJobs <= 0 || state.Options.MaxBlocksPerCall <= 0 || state.Options.TailCap <= 0 {
		return ErrBadSnapshot
	}
	if state.Tail < 0 || state.Tail > state.Options.TailCap {
		return ErrBadSnapshot
	}
	if math.IsNaN(state.LatencyMs) || math.IsInf(state.LatencyMs, 0) || state.LatencyMs < 0 || state.LatencyMs > 500 {
		return ErrBadSnapshot
	}
	if state.NextID < 1 {
		return ErrBadSnapshot
	}

	jobStates := make(map[JobID]*bounceJobState, len(state.Jobs))
	order := make([]JobID, 0, len(state.Jobs))
	activeProjects := make(map[string]struct{})
	activeCount := 0
	var maxID JobID
	for _, job := range state.Jobs {
		if err := validateSnapshotJob(state, job, jobStates); err != nil {
			return ErrBadSnapshot
		}
		if job.ID <= maxID {
			return ErrBadSnapshot
		}
		maxID = job.ID
		inserted := &bounceJobState{
			id:           job.ID,
			job:          cloneJob(job.Job),
			tailDuration: job.TailDuration,
			tailBlocks:   job.TailBlocks,
			latencyMs:    job.LatencyMs,
			done:         job.Done,
			state:        job.State,
			checksum:     job.Checksum,
		}
		jobStates[job.ID] = inserted
		order = append(order, job.ID)
		if job.State == JobRunning {
			activeCount++
			if _, exists := activeProjects[job.Job.Project]; exists {
				return ErrBadSnapshot
			}
			activeProjects[job.Job.Project] = struct{}{}
		}
	}
	if activeCount > state.Options.MaxJobs {
		return ErrBadSnapshot
	}

	r.opts = state.Options
	r.tail = state.Tail
	r.latencyMs = state.LatencyMs
	r.nextID = state.NextID
	r.jobStates = jobStates
	r.order = order
	return nil
}

func validateSnapshotJob(state snapshotState, job snapshotJob, existing map[JobID]*bounceJobState) error {
	if job.ID < 1 || job.ID >= state.NextID {
		return ErrBadSnapshot
	}
	if _, exists := existing[job.ID]; exists {
		return ErrBadSnapshot
	}
	if validateJob(job.Job) != nil {
		return ErrBadSnapshot
	}
	if job.TailDuration < 0 || job.TailDuration > state.Options.TailCap {
		return ErrBadSnapshot
	}
	if job.TailBlocks != tailBlocksFor(job.Job, job.TailDuration) {
		return ErrBadSnapshot
	}
	if math.IsNaN(job.LatencyMs) || math.IsInf(job.LatencyMs, 0) || job.LatencyMs < 0 || job.LatencyMs > 500 {
		return ErrBadSnapshot
	}

	total := job.Job.Blocks + job.TailBlocks
	if job.Done < 0 || job.Done > total {
		return ErrBadSnapshot
	}
	switch job.State {
	case JobRunning, JobCancelled:
		if job.Done >= total {
			return ErrBadSnapshot
		}
	case JobDone:
		if job.Done != total {
			return ErrBadSnapshot
		}
	default:
		return ErrBadSnapshot
	}
	return nil
}

func (r *OfflineBounceRenderer) activeJobsLocked() int {
	active := 0
	for _, id := range r.order {
		if r.jobStates[id].state == JobRunning {
			active++
		}
	}
	return active
}

func (j *bounceJobState) progress() Progress {
	return Progress{
		Job:       j.id,
		Project:   j.job.Project,
		State:     j.state,
		Content:   j.job.Blocks,
		Tail:      j.tailBlocks,
		Done:      j.done,
		Samples:   int64(j.done) * int64(j.job.BlockSize),
		Checksum:  j.checksum,
		Silent:    j.state == JobDone,
		LatencyMs: j.latencyMs,
	}
}

func effectiveOptions(opts Options) Options {
	if opts.MaxJobs <= 0 {
		opts.MaxJobs = defaultMaxJobs
	}
	if opts.MaxBlocksPerCall <= 0 {
		opts.MaxBlocksPerCall = defaultMaxBlocksPerCall
	}
	if opts.TailCap <= 0 {
		opts.TailCap = defaultTailCap
	}
	return opts
}

func validateJob(job BounceJob) error {
	if strings.TrimSpace(job.Project) == "" || job.SampleRate <= 0 || job.BlockSize <= 0 || job.Blocks <= 0 || len(job.TrackRings) == 0 {
		return ErrBadJob
	}
	for _, ring := range job.TrackRings {
		if ring < 0 {
			return ErrBadJob
		}
	}
	return nil
}

func cloneJob(job BounceJob) BounceJob {
	job.TrackRings = append([]int(nil), job.TrackRings...)
	return job
}

func sameJob(a, b BounceJob) bool {
	if a.Project != b.Project || a.SampleRate != b.SampleRate || a.BlockSize != b.BlockSize || a.Blocks != b.Blocks || len(a.TrackRings) != len(b.TrackRings) {
		return false
	}
	for i := range a.TrackRings {
		if a.TrackRings[i] != b.TrackRings[i] {
			return false
		}
	}
	return true
}

func tailBlocksFor(job BounceJob, tail time.Duration) int {
	maxRing := 0
	for _, ring := range job.TrackRings {
		if ring > maxRing {
			maxRing = ring
		}
	}
	if tail <= 0 || maxRing == 0 {
		return 0
	}

	// blocks = ceil(tail nanoseconds * sample rate / (block size seconds)).
	// big.Int keeps the conversion exact for large sample rates and block sizes.
	numerator := new(big.Int).Mul(big.NewInt(tail.Nanoseconds()), big.NewInt(int64(job.SampleRate)))
	denominator := new(big.Int).Mul(big.NewInt(int64(job.BlockSize)), big.NewInt(int64(time.Second)))
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if quotient.Cmp(big.NewInt(int64(maxRing))) >= 0 {
		return maxRing
	}
	return int(quotient.Int64())
}

func jobSeed(job BounceJob, tail time.Duration, latencyMs float64) uint64 {
	h := fnv.New64a()
	writeString(h, job.Project)
	writeInt64(h, int64(job.SampleRate))
	writeInt64(h, int64(job.BlockSize))
	writeInt64(h, int64(job.Blocks))
	writeInt64(h, int64(len(job.TrackRings)))
	for _, ring := range job.TrackRings {
		writeInt64(h, int64(ring))
	}
	writeInt64(h, int64(tail))
	writeUint64(h, math.Float64bits(latencyMs))
	return h.Sum64()
}

func blockDigest(seed uint64, state *bounceJobState, block int) uint64 {
	h := fnv.New64a()
	writeUint64(h, seed)
	writeInt64(h, int64(block))

	latencySamples := int64(state.latencyMs * float64(state.job.SampleRate) / 1000.0)
	writeInt64(h, latencySamples)
	writeInt64(h, int64(block)*int64(state.job.BlockSize)+latencySamples)

	if block < state.job.Blocks {
		writeUint64(h, 1)
		writeInt64(h, int64(len(state.job.TrackRings)))
		for i, ring := range state.job.TrackRings {
			writeInt64(h, int64(i))
			writeInt64(h, int64(ring))
		}
		return h.Sum64()
	}

	writeUint64(h, 2)
	tailBlock := block - state.job.Blocks
	active := 0
	for _, ring := range state.job.TrackRings {
		if ring > tailBlock {
			active++
		}
	}
	writeInt64(h, int64(active))
	for i, ring := range state.job.TrackRings {
		if ring > tailBlock {
			writeInt64(h, int64(i))
			writeInt64(h, int64(ring))
		}
	}
	return h.Sum64()
}

func mixChecksum(sum, block uint64) uint64 {
	sum ^= block
	sum *= 1099511628211
	sum ^= sum >> 29
	if sum == 0 {
		sum = 14695981039346656037
	}
	return sum
}

func writeString(h io.Writer, value string) {
	writeInt64(h, int64(len(value)))
	_, _ = io.WriteString(h, value)
}

func writeInt64(h io.Writer, value int64) {
	writeUint64(h, uint64(value))
}

func writeUint64(h io.Writer, value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, _ = h.Write(buffer[:])
}
