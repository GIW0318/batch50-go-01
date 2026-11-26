// Package task chooses how each frame of an interlaced source is rebuilt:
// field order first, then a per frame weave or interpolation decision, then the
// cadence driven field repeats, all while keeping the output frame count.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults applied when a configuration field is left at zero.
const (
	DefaultMotionThreshold = 0.25
	DefaultConfirmFrames   = 2
	DefaultCadence         = "none"
)

// FieldOrder is the dominance of the two fields of a frame.
type FieldOrder string

// Field orders reported by Probe.
const (
	FieldOrderUnknown     FieldOrder = "unknown"
	FieldOrderTopFirst    FieldOrder = "top_first"
	FieldOrderBottomFirst FieldOrder = "bottom_first"
)

// Method is the rebuild strategy chosen for one frame.
type Method string

// Methods returned by Plan and accepted by Apply.
const (
	MethodWeave       Method = "weave"
	MethodInterpolate Method = "interpolate"
	MethodFieldBlend  Method = "field_blend"
	MethodRepeat      Method = "repeat"
)

// Cadence is the field repetition rhythm of the source.
type Cadence struct {
	Pattern string
}

// Cadences understood by SetCadence.
var (
	CadenceNone = Cadence{Pattern: "none"}
	Cadence32   = Cadence{Pattern: "3:2"}
	Cadence22   = Cadence{Pattern: "2:2"}
)

// Errors reported by the planner.
var (
	ErrBadFrames    = errors.New("probe needs two compatible frames")
	ErrBadFrame     = errors.New("frame geometry or luma plane is invalid")
	ErrBadThreshold = errors.New("motion threshold is out of range")
	ErrBadCadence   = errors.New("cadence is not supported")
	ErrBadPlan      = errors.New("plan method is not supported")
	ErrBadSnapshot  = errors.New("snapshot payload is invalid")
)

// Frame is one interlaced picture. Luma is row major, top left first, and Shot
// groups consecutive frames that belong to the same camera shot.
type Frame struct {
	Index  int64
	Width  int
	Height int
	Y      []uint8
	PTS    time.Duration
	Shot   int64
}

// Plan is the decision handed to Apply.
type Plan struct {
	Index  int64      `json:"index"`
	Method Method     `json:"method"`
	Order  FieldOrder `json:"order"`
	Shot   int64      `json:"shot"`
	Comb   float64    `json:"comb"`
}

// DeintStats counts what the planner decided over its lifetime.
type DeintStats struct {
	Frames      int        `json:"frames"`
	Weave       int        `json:"weave"`
	Interpolate int        `json:"interpolate"`
	Blend       int        `json:"blend"`
	Repeat      int        `json:"repeat"`
	Flips       int        `json:"flips"`
	Order       FieldOrder `json:"order"`
}

// Config carries the motion threshold, the cadence and the confirmation count.
type Config struct {
	MotionThreshold float64
	Cadence         Cadence
	ConfirmFrames   int
}

// DeinterlacePlanner owns the field order, the cadence position and the
// hysteretic method state.
type DeinterlacePlanner struct {
	mu        sync.Mutex
	threshold float64
	cadence   Cadence
	confirm   int
	order     FieldOrder
	lastShot  int64
	shotSeen  bool
	method    Method
	offset    int
	stats     DeintStats
}

// NewDeinterlacePlanner builds a planner with the given configuration.
func NewDeinterlacePlanner(cfg Config) *DeinterlacePlanner {
	return &DeinterlacePlanner{threshold: cfg.MotionThreshold, cadence: cfg.Cadence, confirm: cfg.ConfirmFrames}
}

// Probe decides the field order from consecutive frames.
func (p *DeinterlacePlanner) Probe(frames []Frame) (FieldOrder, error) {
	return FieldOrderUnknown, errNotImplemented
}

// Plan picks the rebuild method for one frame.
func (p *DeinterlacePlanner) Plan(frame Frame) (Method, error) {
	return Method(""), errNotImplemented
}

// Apply rebuilds a frame with the method of the given plan.
func (p *DeinterlacePlanner) Apply(plan Plan, frame Frame) (Frame, error) {
	return Frame{}, errNotImplemented
}

// SetMotionThreshold changes the comb metric threshold.
func (p *DeinterlacePlanner) SetMotionThreshold(v float64) error {
	return errNotImplemented
}

// SetCadence changes the field repetition rhythm.
func (p *DeinterlacePlanner) SetCadence(c Cadence) error {
	return errNotImplemented
}

// Order reports the field order currently in force.
func (p *DeinterlacePlanner) Order() FieldOrder {
	return FieldOrderUnknown
}

// Cadence reports the rhythm currently in force.
func (p *DeinterlacePlanner) Cadence() Cadence {
	return Cadence{}
}

// Reset drops the confirmation state, the cadence position and the counters.
func (p *DeinterlacePlanner) Reset() error {
	return errNotImplemented
}

// Stats reads the lifetime counters.
func (p *DeinterlacePlanner) Stats() DeintStats {
	return DeintStats{}
}

// Snapshot serialises the field order, the cadence position and the counters.
func (p *DeinterlacePlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *DeinterlacePlanner) Restore(data []byte) error {
	return errNotImplemented
}
