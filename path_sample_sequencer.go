// Package task owns the per pixel sampling sequence and the accumulation buffer
// of an interactive path tracer: it hands out low discrepancy sample indices,
// accumulates radiance and tracks how far every pixel has converged.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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

// PathSampleSequencer owns the sequence counters, the accumulation buffer and
// the frame budget.
type PathSampleSequencer struct {
	cfg SampleConfig
}

// NewPathSampleSequencer builds a sequencer, filling unset config fields with
// their defaults.
func NewPathSampleSequencer(cfg SampleConfig) *PathSampleSequencer {
	return &PathSampleSequencer{cfg: cfg}
}

// Config reports the configuration in force after defaults have been applied.
func (p *PathSampleSequencer) Config() SampleConfig {
	return p.cfg
}

// BeginFrame opens a new frame and resets the per frame sample budget.
func (p *PathSampleSequencer) BeginFrame(frame int) error {
	return errNotImplemented
}

// Frame reports the frame number currently open, or the last closed one.
func (p *PathSampleSequencer) Frame() int {
	return 0
}

// SamplesFor hands out count samples for one pixel in the open frame.
func (p *PathSampleSequencer) SamplesFor(pixel, count int) ([]Sample, error) {
	return nil, errNotImplemented
}

// NextSample hands out a single sample for one pixel.
func (p *PathSampleSequencer) NextSample(pixel int) (Sample, error) {
	return Sample{}, errNotImplemented
}

// Accumulate adds one traced value to the estimate of a pixel.
func (p *PathSampleSequencer) Accumulate(pixel int, sample Sample, value Color) error {
	return errNotImplemented
}

// Reset drops the accumulation buffer after a camera or scene change.
func (p *PathSampleSequencer) Reset(reason string) error {
	return errNotImplemented
}

// Convergence reads the estimate and the sample count of one pixel.
func (p *PathSampleSequencer) Convergence(pixel int) (ConvergenceState, error) {
	return ConvergenceState{}, errNotImplemented
}

// SampleCount reports how many samples one pixel has accumulated.
func (p *PathSampleSequencer) SampleCount(pixel int) int {
	return 0
}

// BudgetRemaining reports how many samples the open frame may still hand out.
func (p *PathSampleSequencer) BudgetRemaining() int {
	return 0
}

// SetPriority raises or lowers the refill priority of one pixel.
func (p *PathSampleSequencer) SetPriority(pixel, priority int) error {
	return errNotImplemented
}

// Stats reports the frame counter and the sample counters.
func (p *PathSampleSequencer) Stats() SequencerStats {
	return SequencerStats{}
}

// Snapshot serialises the frame counter, the buffer and the issued indices.
func (p *PathSampleSequencer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *PathSampleSequencer) Restore(data []byte) error {
	return errNotImplemented
}
