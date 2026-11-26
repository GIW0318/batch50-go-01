// Package task batches linear PCM resampling jobs for the voice asset pipeline:
// it validates sample rates and channel layout, runs jobs in submission order,
// applies a shared dither budget and guards the output peak against clipping.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const snapshotVersion = 1

// SupportedRates is the closed set of sample rates the pipeline accepts.
var SupportedRates = []int{8000, 16000, 22050, 24000, 32000, 44100, 48000}

// Job states.
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobDone      = "done"
	JobCancelled = "cancelled"
)

// Errors returned by the batch runner.
var (
	ErrBlankJob        = errors.New("job id is required")
	ErrJobExists       = errors.New("job id already exists")
	ErrUnknownJob      = errors.New("job not found")
	ErrQueueFull       = errors.New("pending job limit reached")
	ErrBadChannels     = errors.New("channel count is outside the allowed range")
	ErrUnsupportedRate = errors.New("sample rate is not supported")
	ErrSameRate        = errors.New("input and output sample rate are identical")
	ErrBadFrames       = errors.New("sample count does not match the channel layout")
	ErrFrameLimit      = errors.New("input frame limit exceeded")
	ErrJobOrder        = errors.New("job is not at the head of the queue")
	ErrJobDone         = errors.New("job is already finished")
	ErrJobCancelled    = errors.New("job was cancelled")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// BatchConfig bounds one resample batch.
type BatchConfig struct {
	MaxJobs      int `json:"max_jobs"`
	MaxInFrames  int `json:"max_in_frames"`
	MaxChannels  int `json:"max_channels"`
	ClipCeiling  int `json:"clip_ceiling"`
	DitherBudget int `json:"dither_budget"`
}

// JobSpec is one queued resample request.
type JobSpec struct {
	ID       string  `json:"id"`
	Channels int     `json:"channels"`
	RateIn   int     `json:"rate_in"`
	RateOut  int     `json:"rate_out"`
	InFrames int     `json:"in_frames"`
	PCM      []int16 `json:"pcm"`
	State    string  `json:"state"`
	Seq      int64   `json:"seq"`
}

// ResamplePlan is the frame arithmetic of one job before it runs.
type ResamplePlan struct {
	JobID        string `json:"job_id"`
	InFrames     int    `json:"in_frames"`
	OutFrames    int    `json:"out_frames"`
	RateIn       int    `json:"rate_in"`
	RateOut      int    `json:"rate_out"`
	DitherBudget int    `json:"dither_budget"`
}

// ResampleResult is the finished PCM plus its protection counters.
type ResampleResult struct {
	JobID         string  `json:"job_id"`
	Channels      int     `json:"channels"`
	RateIn        int     `json:"rate_in"`
	RateOut       int     `json:"rate_out"`
	OutFrames     int     `json:"out_frames"`
	PCM           []int16 `json:"pcm"`
	ClippedFrames int     `json:"clipped_frames"`
	DitherUsed    int     `json:"dither_used"`
	Seq           int64   `json:"seq"`
}

type jobState struct {
	spec      JobSpec
	result    ResampleResult
	hasResult bool
}

type snapshotData struct {
	Jobs    []JobSpec        `json:"jobs"`
	Results []ResampleResult `json:"results"`
	Queue   []string         `json:"queue"`
	Order   []string         `json:"order"`
	Seq     int64            `json:"seq"`
	Version int              `json:"version"`
}

// AudioResampleBatch owns the queue, the finished results and the counters.
type AudioResampleBatch struct {
	mu    sync.RWMutex
	cfg   BatchConfig
	seq   int64
	jobs  map[string]*jobState
	order []string
	queue []string
}

// NewAudioResampleBatch builds an empty batch runner with the given bounds.
func NewAudioResampleBatch(cfg BatchConfig) *AudioResampleBatch {
	return &AudioResampleBatch{
		cfg:   cfg,
		jobs:  make(map[string]*jobState),
		order: make([]string, 0),
		queue: make([]string, 0),
	}
}

// Submit validates one job and puts it at the tail of the queue.
func (b *AudioResampleBatch) Submit(id string, pcm []int16, channels, rateIn, rateOut int) (JobSpec, error) {
	if strings.TrimSpace(id) == "" {
		return JobSpec{}, ErrBlankJob
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.jobs[id]; exists {
		return JobSpec{}, ErrJobExists
	}
	if channels < 1 || channels > b.cfg.MaxChannels {
		return JobSpec{}, ErrBadChannels
	}
	if !supportedRate(rateIn) || !supportedRate(rateOut) {
		return JobSpec{}, ErrUnsupportedRate
	}
	if rateIn == rateOut {
		return JobSpec{}, ErrSameRate
	}
	if len(pcm)%channels != 0 {
		return JobSpec{}, ErrBadFrames
	}

	inFrames := len(pcm) / channels
	if inFrames > b.cfg.MaxInFrames {
		return JobSpec{}, ErrFrameLimit
	}
	if len(b.queue) >= b.cfg.MaxJobs {
		return JobSpec{}, ErrQueueFull
	}

	b.seq++
	spec := JobSpec{
		ID:       id,
		Channels: channels,
		RateIn:   rateIn,
		RateOut:  rateOut,
		InFrames: inFrames,
		PCM:      clonePCM(pcm),
		State:    JobPending,
		Seq:      b.seq,
	}
	b.jobs[id] = &jobState{spec: spec}
	b.order = append(b.order, id)
	b.queue = append(b.queue, id)

	return cloneSpec(spec), nil
}

// Plan reports the frame arithmetic of one queued job.
func (b *AudioResampleBatch) Plan(id string) (ResamplePlan, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	state, ok := b.jobs[id]
	if !ok {
		return ResamplePlan{}, ErrUnknownJob
	}
	spec := state.spec

	return ResamplePlan{
		JobID:        id,
		InFrames:     spec.InFrames,
		OutFrames:    outputFrameCount(spec.InFrames, spec.RateIn, spec.RateOut),
		RateIn:       spec.RateIn,
		RateOut:      spec.RateOut,
		DitherBudget: b.cfg.DitherBudget,
	}, nil
}

// Run processes the job at the head of the queue.
func (b *AudioResampleBatch) Run(id string) (ResampleResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	state, ok := b.jobs[id]
	if !ok {
		return ResampleResult{}, ErrUnknownJob
	}
	switch state.spec.State {
	case JobDone:
		return ResampleResult{}, ErrJobDone
	case JobCancelled:
		return ResampleResult{}, ErrJobCancelled
	}
	if len(b.queue) == 0 || b.queue[0] != id {
		return ResampleResult{}, ErrJobOrder
	}

	result := resampleJob(state.spec, b.cfg)
	b.queue = b.queue[1:]
	state.spec.State = JobDone
	state.result = cloneResult(result)
	state.hasResult = true

	return cloneResult(result), nil
}

// Result reads the finished output of one job.
func (b *AudioResampleBatch) Result(id string) (ResampleResult, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	state, ok := b.jobs[id]
	if !ok || !state.hasResult {
		return ResampleResult{}, false
	}
	return cloneResult(state.result), true
}

// Cancel drops one pending job from the queue.
func (b *AudioResampleBatch) Cancel(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	state, ok := b.jobs[id]
	if !ok {
		return ErrUnknownJob
	}
	if state.spec.State == JobDone {
		return ErrJobDone
	}
	if state.spec.State == JobCancelled {
		return nil
	}

	for i, queuedID := range b.queue {
		if queuedID == id {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			break
		}
	}
	state.spec.State = JobCancelled
	return nil
}

// Queue lists the pending job ids in submission order.
func (b *AudioResampleBatch) Queue() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	queue := make([]string, len(b.queue))
	copy(queue, b.queue)
	return queue
}

// Jobs returns a copy of every job spec ordered by submission sequence.
func (b *AudioResampleBatch) Jobs() []JobSpec {
	b.mu.RLock()
	defer b.mu.RUnlock()

	jobs := make([]JobSpec, 0, len(b.order))
	for _, id := range b.order {
		if state, ok := b.jobs[id]; ok {
			jobs = append(jobs, cloneSpec(state.spec))
		}
	}
	return jobs
}

// Pending counts the jobs still waiting in the queue.
func (b *AudioResampleBatch) Pending() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.queue)
}

// Snapshot serialises the queue, every job spec and every finished result.
func (b *AudioResampleBatch) Snapshot() ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	snapshot := snapshotData{
		Jobs:    make([]JobSpec, 0, len(b.order)),
		Results: make([]ResampleResult, 0, len(b.jobs)),
		Queue:   cloneStrings(b.queue),
		Order:   cloneStrings(b.order),
		Seq:     b.seq,
		Version: snapshotVersion,
	}
	for _, id := range b.order {
		state := b.jobs[id]
		snapshot.Jobs = append(snapshot.Jobs, cloneSpec(state.spec))
		if state.hasResult {
			snapshot.Results = append(snapshot.Results, cloneResult(state.result))
		}
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return raw, nil
}

// Restore replaces the whole batch state, all or nothing.
func (b *AudioResampleBatch) Restore(data []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ErrBadSnapshot
	}

	jobs, order, queue, err := buildSnapshotState(&snapshot)
	if err != nil {
		return ErrBadSnapshot
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq = snapshot.Seq
	b.jobs = jobs
	b.order = order
	b.queue = queue
	return nil
}

func resampleJob(spec JobSpec, cfg BatchConfig) ResampleResult {
	outFrames := outputFrameCount(spec.InFrames, spec.RateIn, spec.RateOut)
	channels := spec.Channels
	result := ResampleResult{
		JobID:     spec.ID,
		Channels:  channels,
		RateIn:    spec.RateIn,
		RateOut:   spec.RateOut,
		OutFrames: outFrames,
		PCM:       make([]int16, outFrames*channels),
		Seq:       spec.Seq,
	}

	ditherBudget := cfg.DitherBudget
	if ditherBudget < 0 {
		ditherBudget = 0
	}
	ceiling := int64(cfg.ClipCeiling)

	for outFrame := 0; outFrame < outFrames; outFrame++ {
		pos := int64(outFrame) * int64(spec.RateIn)
		srcFrame64 := pos / int64(spec.RateOut)
		rem := pos % int64(spec.RateOut)
		srcFrame := int(srcFrame64)

		nextFrame := srcFrame + 1
		if nextFrame >= spec.InFrames {
			nextFrame = srcFrame
		}

		dither := int64(0)
		if result.DitherUsed < ditherBudget {
			dither = 1
			result.DitherUsed++
		}

		frameClipped := false
		for channel := 0; channel < channels; channel++ {
			first := int64(spec.PCM[srcFrame*channels+channel])
			second := int64(spec.PCM[nextFrame*channels+channel])
			numerator := first*(int64(spec.RateOut)-rem) + second*rem
			value := roundHalfAwayFromZero(numerator, int64(spec.RateOut)) + dither

			if value > ceiling {
				value = ceiling
				frameClipped = true
			}
			if value < -ceiling {
				value = -ceiling
				frameClipped = true
			}

			result.PCM[outFrame*channels+channel] = int16(value)
		}

		if frameClipped {
			result.ClippedFrames++
		}
	}

	return result
}

func outputFrameCount(inFrames, rateIn, rateOut int) int {
	if inFrames <= 0 || rateIn <= 0 || rateOut <= 0 {
		return 0
	}
	numerator := int64(inFrames) * int64(rateOut)
	denominator := int64(rateIn)
	return int((numerator + denominator - 1) / denominator)
}

func roundHalfAwayFromZero(numerator, denominator int64) int64 {
	if denominator == 0 {
		return 0
	}

	sign := int64(1)
	if numerator < 0 {
		sign = -1
		numerator = -numerator
	}

	quotient := numerator / denominator
	remainder := numerator % denominator
	if remainder*2 >= denominator {
		quotient++
	}
	return sign * quotient
}

func supportedRate(rate int) bool {
	for _, supported := range SupportedRates {
		if rate == supported {
			return true
		}
	}
	return false
}

func buildSnapshotState(snapshot *snapshotData) (map[string]*jobState, []string, []string, error) {
	if snapshot.Version != snapshotVersion {
		return nil, nil, nil, fmt.Errorf("%w: unsupported snapshot version", ErrBadSnapshot)
	}
	if snapshot.Seq < 0 {
		return nil, nil, nil, fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}

	specs := make(map[string]JobSpec, len(snapshot.Jobs))
	var maxSeq int64
	for _, spec := range snapshot.Jobs {
		if strings.TrimSpace(spec.ID) == "" {
			return nil, nil, nil, fmt.Errorf("%w: missing job id", ErrBadSnapshot)
		}
		if _, exists := specs[spec.ID]; exists {
			return nil, nil, nil, fmt.Errorf("%w: duplicate job %q", ErrBadSnapshot, spec.ID)
		}
		if spec.Seq <= 0 || spec.Seq > snapshot.Seq {
			return nil, nil, nil, fmt.Errorf("%w: invalid sequence for %q", ErrBadSnapshot, spec.ID)
		}
		if spec.Seq > maxSeq {
			maxSeq = spec.Seq
		}
		if !validJobState(spec.State) {
			return nil, nil, nil, fmt.Errorf("%w: invalid state for %q", ErrBadSnapshot, spec.ID)
		}
		if spec.Channels < 1 {
			return nil, nil, nil, fmt.Errorf("%w: invalid channels for %q", ErrBadSnapshot, spec.ID)
		}
		if !supportedRate(spec.RateIn) || !supportedRate(spec.RateOut) || spec.RateIn == spec.RateOut {
			return nil, nil, nil, fmt.Errorf("%w: invalid rates for %q", ErrBadSnapshot, spec.ID)
		}
		if spec.InFrames < 0 {
			return nil, nil, nil, fmt.Errorf("%w: invalid frame count for %q", ErrBadSnapshot, spec.ID)
		}
		if int64(len(spec.PCM)) != int64(spec.InFrames)*int64(spec.Channels) {
			return nil, nil, nil, fmt.Errorf("%w: pcm does not match frames for %q", ErrBadSnapshot, spec.ID)
		}
		specs[spec.ID] = spec
	}
	if snapshot.Seq < maxSeq {
		return nil, nil, nil, fmt.Errorf("%w: sequence is behind jobs", ErrBadSnapshot)
	}
	if len(snapshot.Order) != len(snapshot.Jobs) {
		return nil, nil, nil, fmt.Errorf("%w: order length mismatch", ErrBadSnapshot)
	}
	for i, id := range snapshot.Order {
		if id != snapshot.Jobs[i].ID {
			return nil, nil, nil, fmt.Errorf("%w: order mismatch at %d", ErrBadSnapshot, i)
		}
	}

	pendingCount := 0
	for _, spec := range specs {
		if spec.State == JobPending {
			pendingCount++
		}
	}
	if len(snapshot.Queue) != pendingCount {
		return nil, nil, nil, fmt.Errorf("%w: queue length mismatch", ErrBadSnapshot)
	}

	queueSeen := make(map[string]struct{}, len(snapshot.Queue))
	for _, id := range snapshot.Queue {
		spec, ok := specs[id]
		if !ok {
			return nil, nil, nil, fmt.Errorf("%w: unknown queued job %q", ErrBadSnapshot, id)
		}
		if spec.State != JobPending {
			return nil, nil, nil, fmt.Errorf("%w: non-pending job queued as %q", ErrBadSnapshot, id)
		}
		if _, seen := queueSeen[id]; seen {
			return nil, nil, nil, fmt.Errorf("%w: duplicate queue entry %q", ErrBadSnapshot, id)
		}
		queueSeen[id] = struct{}{}
	}

	results := make(map[string]ResampleResult, len(snapshot.Results))
	for _, result := range snapshot.Results {
		if strings.TrimSpace(result.JobID) == "" {
			return nil, nil, nil, fmt.Errorf("%w: result missing job id", ErrBadSnapshot)
		}
		if _, exists := results[result.JobID]; exists {
			return nil, nil, nil, fmt.Errorf("%w: duplicate result for %q", ErrBadSnapshot, result.JobID)
		}
		spec, ok := specs[result.JobID]
		if !ok || spec.State != JobDone {
			return nil, nil, nil, fmt.Errorf("%w: result for unfinished job %q", ErrBadSnapshot, result.JobID)
		}

		expectedFrames := outputFrameCount(spec.InFrames, spec.RateIn, spec.RateOut)
		expectedSamples := int64(expectedFrames) * int64(spec.Channels)
		if result.Channels != spec.Channels || result.RateIn != spec.RateIn || result.RateOut != spec.RateOut {
			return nil, nil, nil, fmt.Errorf("%w: result header mismatch for %q", ErrBadSnapshot, result.JobID)
		}
		if result.OutFrames != expectedFrames || int64(len(result.PCM)) != expectedSamples {
			return nil, nil, nil, fmt.Errorf("%w: result pcm length mismatch for %q", ErrBadSnapshot, result.JobID)
		}
		if result.Seq != spec.Seq {
			return nil, nil, nil, fmt.Errorf("%w: result sequence mismatch for %q", ErrBadSnapshot, result.JobID)
		}
		if result.DitherUsed < 0 || result.DitherUsed > expectedFrames || result.ClippedFrames < 0 || result.ClippedFrames > expectedFrames {
			return nil, nil, nil, fmt.Errorf("%w: invalid counters for %q", ErrBadSnapshot, result.JobID)
		}
		results[result.JobID] = result
	}

	jobs := make(map[string]*jobState, len(snapshot.Jobs))
	order := cloneStrings(snapshot.Order)
	queue := cloneStrings(snapshot.Queue)
	for _, spec := range snapshot.Jobs {
		state := &jobState{spec: cloneSpec(spec)}
		if result, ok := results[spec.ID]; ok {
			state.result = cloneResult(result)
			state.hasResult = true
		}
		if spec.State == JobDone && !state.hasResult {
			return nil, nil, nil, fmt.Errorf("%w: missing result for %q", ErrBadSnapshot, spec.ID)
		}
		jobs[spec.ID] = state
	}

	return jobs, order, queue, nil
}

func validJobState(state string) bool {
	switch state {
	case JobPending, JobRunning, JobDone, JobCancelled:
		return true
	default:
		return false
	}
}

func clonePCM(pcm []int16) []int16 {
	if pcm == nil {
		return nil
	}
	cloned := make([]int16, len(pcm))
	copy(cloned, pcm)
	return cloned
}

func cloneSpec(spec JobSpec) JobSpec {
	spec.PCM = clonePCM(spec.PCM)
	return spec
}

func cloneResult(result ResampleResult) ResampleResult {
	result.PCM = clonePCM(result.PCM)
	return result
}

func cloneStrings(values []string) []string {
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}
