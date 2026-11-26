// Package task owns the per pixel sampling sequence and the accumulation buffer
// of an interactive path tracer: it hands out low discrepancy sample indices,
// accumulates radiance and tracks how far every pixel has converged.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Defaults used when the matching config field is left at zero or below.
const (
	DefaultWidth              = 64
	DefaultHeight             = 64
	DefaultFrameSamples       = 1024
	DefaultMaxSamplesPerPixel = 512
	DefaultConvergeSamples    = 128
)

// Errors reported by the sequencer.
var (
	ErrBadConfig       = errors.New("sample configuration is invalid")
	ErrBadFrame        = errors.New("frame number must strictly increase")
	ErrNoFrame         = errors.New("no frame is open")
	ErrBadPixel        = errors.New("pixel index is out of range")
	ErrBadCount        = errors.New("sample count must be positive")
	ErrBudgetExhausted = errors.New("frame sample budget is exhausted")
	ErrBadColor        = errors.New("sample value is not a valid radiance")
	ErrBadSample       = errors.New("sample does not belong to this pixel")
	ErrSampleCap       = errors.New("pixel reached its sample cap")
	ErrBadReason       = errors.New("reset reason is required")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// SampleConfig is the immutable setup of one sequencer.
type SampleConfig struct {
	Width              int
	Height             int
	FrameSamples       int
	MaxSamplesPerPixel int
	ConvergeSamples    int
}

// Sample is one stratified position inside a pixel together with the sequence
// index it was issued for.
type Sample struct {
	Pixel int
	Index int
	U     float64
	V     float64
}

// Color is a linear radiance value.
type Color struct {
	R float64
	G float64
	B float64
}

// ConvergenceState reports the accumulated estimate of one pixel.
type ConvergenceState struct {
	Pixel     int
	Samples   int
	Mean      Color
	Converged bool
}

// SequencerStats reports the counters of the sequencer.
type SequencerStats struct {
	Frame        int
	FrameSamples int
	Budget       int
	Pixels       int
	Accumulated  int64
}

type accumulationState struct {
	count int
	mean  Color
	seen  map[int]struct{}
}

// PathSampleSequencer owns the sequence counters, the accumulation buffer and
// the frame budget.
type PathSampleSequencer struct {
	mu          sync.RWMutex
	cfg         SampleConfig
	frame       int
	frameOpen   bool
	budgetUsed  int
	issued      []int
	priorities  []int
	pixels      []accumulationState
	accumulated int64
}

type persistedPixel struct {
	Count   int   `json:"count"`
	Mean    Color `json:"mean"`
	Indices []int `json:"indices"`
}

type persistedState struct {
	Version     int              `json:"version"`
	Config      SampleConfig     `json:"config"`
	Frame       int              `json:"frame"`
	FrameOpen   bool             `json:"frame_open"`
	BudgetUsed  int              `json:"budget_used"`
	Issued      []int            `json:"issued"`
	Priorities  []int            `json:"priorities"`
	Pixels      []persistedPixel `json:"pixels"`
	Accumulated int64            `json:"accumulated"`
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Data     json.RawMessage `json:"data"`
}

const (
	snapshotMagic   = "path-sample-sequencer-v1"
	snapshotVersion = 1
)

// NewPathSampleSequencer builds a sequencer, filling unset config fields with
// their defaults.
func NewPathSampleSequencer(cfg SampleConfig) *PathSampleSequencer {
	cfg = normalizeConfig(cfg)
	pixels := cfg.Width * cfg.Height
	return &PathSampleSequencer{
		cfg:        cfg,
		issued:     make([]int, pixels),
		priorities: make([]int, pixels),
		pixels:     make([]accumulationState, pixels),
	}
}

func normalizeConfig(cfg SampleConfig) SampleConfig {
	if cfg.Width <= 0 {
		cfg.Width = DefaultWidth
	}
	if cfg.Height <= 0 {
		cfg.Height = DefaultHeight
	}
	if cfg.FrameSamples <= 0 {
		cfg.FrameSamples = DefaultFrameSamples
	}
	if cfg.MaxSamplesPerPixel <= 0 {
		cfg.MaxSamplesPerPixel = DefaultMaxSamplesPerPixel
	}
	if cfg.ConvergeSamples <= 0 {
		cfg.ConvergeSamples = DefaultConvergeSamples
	}
	return cfg
}

// Config reports the configuration in force after defaults have been applied.
func (p *PathSampleSequencer) Config() SampleConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// BeginFrame opens a new frame and resets the per frame sample budget.
func (p *PathSampleSequencer) BeginFrame(frame int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.frameOpen && frame <= p.frame {
		return ErrBadFrame
	}

	p.frame = frame
	p.frameOpen = true
	p.budgetUsed = 0
	return nil
}

// Frame reports the frame number currently open, or the last closed one.
func (p *PathSampleSequencer) Frame() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.frame
}

// SamplesFor hands out count samples for one pixel in the open frame.
func (p *PathSampleSequencer) SamplesFor(pixel, count int) ([]Sample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.issueLocked(pixel, count)
}

// NextSample hands out a single sample for one pixel.
func (p *PathSampleSequencer) NextSample(pixel int) (Sample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	samples, err := p.issueLocked(pixel, 1)
	if err != nil {
		return Sample{}, err
	}
	return samples[0], nil
}

func (p *PathSampleSequencer) issueLocked(pixel, count int) ([]Sample, error) {
	if !p.frameOpen {
		return nil, ErrNoFrame
	}
	if !p.validPixelLocked(pixel) {
		return nil, ErrBadPixel
	}
	if count <= 0 {
		return nil, ErrBadCount
	}

	remaining := p.cfg.FrameSamples - p.budgetUsed
	if count > remaining {
		return nil, ErrBudgetExhausted
	}

	firstIndex := p.issued[pixel] + 1
	samples := make([]Sample, count)
	for offset := 0; offset < count; offset++ {
		index := firstIndex + offset
		u, v := sampleCoordinates(p.cfg, pixel, index)
		samples[offset] = Sample{Pixel: pixel, Index: index, U: u, V: v}
	}

	p.issued[pixel] += count
	p.budgetUsed += count
	return samples, nil
}

func (p *PathSampleSequencer) validPixelLocked(pixel int) bool {
	return pixel >= 0 && pixel < len(p.pixels)
}

func sampleCoordinates(cfg SampleConfig, pixel, index int) (float64, float64) {
	configSeed := mix64(
		uint64(cfg.Width)*0x9E3779B97F4A7C15 ^
			uint64(cfg.Height)*0xBF58476D1CE4E5B9 ^
			uint64(cfg.FrameSamples)*0x94D049BB133111EB ^
			uint64(cfg.MaxSamplesPerPixel)*0xC2B2AE3D27D4EB4F ^
			uint64(cfg.ConvergeSamples)*0x165667B19E3779F9,
	)
	uOffset := pixelOffsetUnit(pixel, configSeed^0x243F6A8885A308D3)
	vOffset := pixelOffsetUnit(pixel, configSeed^0x13198A2E03707344)

	u := wrapUnit(radicalInverse(2, index) + uOffset)
	v := wrapUnit(radicalInverse(3, index) + vOffset)
	return u, v
}

func pixelOffsetUnit(pixel int, key uint64) float64 {
	const bits = 52
	permuted := permute52(uint64(pixel), key)
	return float64(permuted) / float64(uint64(1)<<bits)
}

func permute52(value, key uint64) uint64 {
	const halfBits = 26
	const halfMask = uint64(1<<halfBits) - 1

	left := (value >> halfBits) & halfMask
	right := value & halfMask
	for round := uint64(0); round < 6; round++ {
		function := mix64(right^key^round*0x9E3779B97F4A7C15) & halfMask
		nextLeft := right
		nextRight := left ^ function
		left = nextLeft
		right = nextRight
	}
	return (left << halfBits) | right
}

func radicalInverse(base uint64, index int) float64 {
	digitCount := uint64(index)
	result := 0.0
	place := 1.0 / float64(base)
	inverseBase := place

	for digitCount > 0 {
		digit := digitCount % base
		result += float64(digit) * place
		digitCount /= base
		place *= inverseBase
	}
	return result
}

func mix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func wrapUnit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	value -= math.Floor(value)
	if value < 0 {
		return 0
	}
	if value >= 1 {
		return math.Nextafter(1, 0)
	}
	return value
}

// Accumulate adds one traced value to the estimate of a pixel. Repeating the
// same pixel/index pair is an idempotent no-op after the first valid value.
func (p *PathSampleSequencer) Accumulate(pixel int, sample Sample, value Color) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.frameOpen {
		return ErrNoFrame
	}
	if !p.validPixelLocked(pixel) {
		return ErrBadPixel
	}
	if sample.Pixel != pixel || sample.Index <= 0 {
		return ErrBadSample
	}
	if !validColor(value) {
		return ErrBadColor
	}

	state := &p.pixels[pixel]
	if _, repeated := state.seen[sample.Index]; repeated {
		return nil
	}
	if state.count >= p.cfg.MaxSamplesPerPixel {
		return ErrSampleCap
	}

	denominator := float64(state.count + 1)
	state.mean.R += (value.R - state.mean.R) / denominator
	state.mean.G += (value.G - state.mean.G) / denominator
	state.mean.B += (value.B - state.mean.B) / denominator
	state.count++

	if state.seen == nil {
		state.seen = make(map[int]struct{})
	}
	state.seen[sample.Index] = struct{}{}
	p.accumulated++
	return nil
}

func validColor(value Color) bool {
	for _, component := range [3]float64{value.R, value.G, value.B} {
		if math.IsNaN(component) || math.IsInf(component, 0) || component < 0 {
			return false
		}
	}
	return true
}

// Reset drops every pixel's accumulation buffer after a camera or scene
// change. A blank reason is rejected before any state is cleared.
func (p *PathSampleSequencer) Reset(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrBadReason
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pixels = make([]accumulationState, len(p.pixels))
	p.accumulated = 0
	return nil
}

// Convergence reads the estimate and the sample count of one pixel.
func (p *PathSampleSequencer) Convergence(pixel int) (ConvergenceState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.validPixelLocked(pixel) {
		return ConvergenceState{}, ErrBadPixel
	}

	state := p.pixels[pixel]
	return ConvergenceState{
		Pixel:     pixel,
		Samples:   state.count,
		Mean:      state.mean,
		Converged: state.count >= p.cfg.ConvergeSamples,
	}, nil
}

// SampleCount reports how many samples one pixel has accumulated.
func (p *PathSampleSequencer) SampleCount(pixel int) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.validPixelLocked(pixel) {
		return 0
	}
	return p.pixels[pixel].count
}

// BudgetRemaining reports how many samples the open frame may still hand out.
func (p *PathSampleSequencer) BudgetRemaining() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.FrameSamples - p.budgetUsed
}

// SetPriority raises or lowers the refill priority of one pixel. Higher values
// mean the pixel should be resumed before lower-priority dirty pixels when the
// renderer allocates a limited frame budget.
func (p *PathSampleSequencer) SetPriority(pixel, priority int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.validPixelLocked(pixel) {
		return ErrBadPixel
	}
	p.priorities[pixel] = priority
	return nil
}

// Stats reports the frame counter and the sample counters.
func (p *PathSampleSequencer) Stats() SequencerStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return SequencerStats{
		Frame:        p.frame,
		FrameSamples: p.budgetUsed,
		Budget:       p.cfg.FrameSamples - p.budgetUsed,
		Pixels:       len(p.pixels),
		Accumulated:  p.accumulated,
	}
}

// Snapshot serialises the frame counter, the buffer and the issued indices.
func (p *PathSampleSequencer) Snapshot() ([]byte, error) {
	p.mu.RLock()
	state := persistedState{
		Version:     snapshotVersion,
		Config:      p.cfg,
		Frame:       p.frame,
		FrameOpen:   p.frameOpen,
		BudgetUsed:  p.budgetUsed,
		Issued:      append([]int(nil), p.issued...),
		Priorities:  append([]int(nil), p.priorities...),
		Pixels:      make([]persistedPixel, len(p.pixels)),
		Accumulated: p.accumulated,
	}

	for pixel, buffer := range p.pixels {
		indices := make([]int, 0, len(buffer.seen))
		for index := range buffer.seen {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		state.Pixels[pixel] = persistedPixel{
			Count:   buffer.count,
			Mean:    buffer.mean,
			Indices: indices,
		}
	}
	p.mu.RUnlock()

	rawState, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode sequencer state: %w", err)
	}
	checksum := sha256.Sum256(rawState)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(checksum[:]),
		Data:     rawState,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode sequencer snapshot: %w", err)
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *PathSampleSequencer) Restore(data []byte) error {
	next, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = next.cfg
	p.frame = next.frame
	p.frameOpen = next.frameOpen
	p.budgetUsed = next.budgetUsed
	p.issued = next.issued
	p.priorities = next.priorities
	p.pixels = next.pixels
	p.accumulated = next.accumulated
	return nil
}

type restoredSnapshot struct {
	cfg         SampleConfig
	frame       int
	frameOpen   bool
	budgetUsed  int
	issued      []int
	priorities  []int
	pixels      []accumulationState
	accumulated int64
}

func decodeSnapshot(data []byte) (*restoredSnapshot, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: invalid envelope: %v", ErrBadSnapshot, err)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported snapshot envelope", ErrBadSnapshot)
	}

	var state persistedState
	if err := json.Unmarshal(envelope.Data, &state); err != nil {
		return nil, fmt.Errorf("%w: invalid state: %v", ErrBadSnapshot, err)
	}
	canonicalState, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize state: %v", ErrBadSnapshot, err)
	}
	checksum := sha256.Sum256(canonicalState)
	if envelope.Checksum != hex.EncodeToString(checksum[:]) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}
	if state.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported state version", ErrBadSnapshot)
	}

	cfg := normalizeConfig(state.Config)
	pixelCount := cfg.Width * cfg.Height
	if pixelCount <= 0 {
		return nil, fmt.Errorf("%w: invalid pixel count", ErrBadSnapshot)
	}
	if state.BudgetUsed < 0 || state.BudgetUsed > cfg.FrameSamples {
		return nil, fmt.Errorf("%w: invalid used budget", ErrBadSnapshot)
	}
	if len(state.Issued) != pixelCount || len(state.Priorities) != pixelCount || len(state.Pixels) != pixelCount {
		return nil, fmt.Errorf("%w: snapshot buffers have the wrong length", ErrBadSnapshot)
	}

	issued := append([]int(nil), state.Issued...)
	for _, watermark := range issued {
		if watermark < 0 {
			return nil, fmt.Errorf("%w: negative index watermark", ErrBadSnapshot)
		}
	}

	priorities := append([]int(nil), state.Priorities...)
	pixels := make([]accumulationState, pixelCount)
	var accumulated int64

	for pixel, persisted := range state.Pixels {
		if persisted.Count < 0 || persisted.Count > cfg.MaxSamplesPerPixel {
			return nil, fmt.Errorf("%w: invalid accumulated count for pixel %d", ErrBadSnapshot, pixel)
		}
		if len(persisted.Indices) != persisted.Count {
			return nil, fmt.Errorf("%w: index set does not match count for pixel %d", ErrBadSnapshot, pixel)
		}
		if !validColor(persisted.Mean) {
			return nil, fmt.Errorf("%w: invalid mean for pixel %d", ErrBadSnapshot, pixel)
		}
		if persisted.Count == 0 && persisted.Mean != (Color{}) {
			return nil, fmt.Errorf("%w: empty pixel has non-zero mean %d", ErrBadSnapshot, pixel)
		}

		seen := make(map[int]struct{}, len(persisted.Indices))
		for _, index := range persisted.Indices {
			if index <= 0 {
				return nil, fmt.Errorf("%w: invalid accumulated index for pixel %d", ErrBadSnapshot, pixel)
			}
			if _, exists := seen[index]; exists {
				return nil, fmt.Errorf("%w: duplicate accumulated index for pixel %d", ErrBadSnapshot, pixel)
			}
			seen[index] = struct{}{}
		}

		pixels[pixel] = accumulationState{
			count: persisted.Count,
			mean:  persisted.Mean,
			seen:  seen,
		}
		accumulated += int64(persisted.Count)
	}

	if state.Accumulated != accumulated || accumulated < 0 {
		return nil, fmt.Errorf("%w: accumulated total mismatch", ErrBadSnapshot)
	}

	return &restoredSnapshot{
		cfg:         cfg,
		frame:       state.Frame,
		frameOpen:   state.FrameOpen,
		budgetUsed:  state.BudgetUsed,
		issued:      issued,
		priorities:  priorities,
		pixels:      pixels,
		accumulated: accumulated,
	}, nil
}
