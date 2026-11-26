// Package task chooses how each frame of an interlaced source is rebuilt:
// field order first, then a per frame weave or interpolation decision, then the
// cadence driven field repeats, all while keeping the output frame count.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

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
	mu          sync.Mutex
	threshold   float64
	cadence     Cadence
	confirm     int
	order       FieldOrder
	lastShot    int64
	shotSeen    bool
	confirming  bool
	confirmLeft int
	accumEven   float64
	accumOdd    float64
	prevY       []uint8
	prevWidth   int
	prevHeight  int
	method      Method
	offset      int
	stats       DeintStats
}

// NewDeinterlacePlanner builds a planner with the given configuration.
func NewDeinterlacePlanner(cfg Config) *DeinterlacePlanner {
	threshold := cfg.MotionThreshold
	if threshold == 0 {
		threshold = DefaultMotionThreshold
	}
	confirm := cfg.ConfirmFrames
	if confirm == 0 {
		confirm = DefaultConfirmFrames
	}
	cadence := cfg.Cadence
	if !validCadence(cadence) {
		cadence = CadenceNone
	}
	return &DeinterlacePlanner{
		threshold: threshold,
		cadence:   cadence,
		confirm:   confirm,
		order:     FieldOrderUnknown,
		method:    MethodWeave,
	}
}

func validCadence(c Cadence) bool {
	switch c.Pattern {
	case CadenceNone.Pattern, Cadence32.Pattern, Cadence22.Pattern:
		return true
	}
	return false
}

func cadencePeriod(c Cadence) int {
	switch c.Pattern {
	case Cadence32.Pattern:
		return 5
	case Cadence22.Pattern:
		return 2
	}
	return 0
}

func isRepeatPosition(c Cadence, pos int) bool {
	switch c.Pattern {
	case Cadence32.Pattern:
		return pos == 1 || pos == 3
	case Cadence22.Pattern:
		return pos == 1
	}
	return false
}

func validateFrame(f Frame) error {
	if f.Width <= 0 || f.Height <= 0 {
		return ErrBadFrame
	}
	if len(f.Y) != f.Width*f.Height {
		return ErrBadFrame
	}
	return nil
}

// fieldDiffs returns the mean absolute luma difference between two frames of
// identical geometry, split into even and odd row groups.
func fieldDiffs(a, b Frame) (even, odd float64) {
	w, h := a.Width, a.Height
	var evenSum, oddSum float64
	var evenN, oddN int
	for r := 0; r < h; r++ {
		for x := 0; x < w; x++ {
			d := float64(a.Y[r*w+x]) - float64(b.Y[r*w+x])
			if d < 0 {
				d = -d
			}
			if r%2 == 0 {
				evenSum += d
				evenN++
			} else {
				oddSum += d
				oddN++
			}
		}
	}
	if evenN > 0 {
		even = evenSum / float64(evenN)
	}
	if oddN > 0 {
		odd = oddSum / float64(oddN)
	}
	return even, odd
}

// decideOrder maps the even/odd field differences to a field order. A field
// group must exceed the other by more than twenty percent to win.
func decideOrder(even, odd float64) FieldOrder {
	if odd > even*1.2 {
		return FieldOrderTopFirst
	}
	if even > odd*1.2 {
		return FieldOrderBottomFirst
	}
	return FieldOrderUnknown
}

// combMetric measures combing: the mean absolute difference between odd rows
// and the average of their vertical neighbours, normalised by peak luma.
func combMetric(f Frame) float64 {
	w, h := f.Width, f.Height
	if h < 2 {
		return 0
	}
	var sum float64
	var n int
	for r := 1; r < h; r += 2 {
		prev := r - 1
		next := r + 1
		if next >= h {
			next = prev
		}
		for x := 0; x < w; x++ {
			v := float64(f.Y[r*w+x])
			m := (float64(f.Y[prev*w+x]) + float64(f.Y[next*w+x])) / 2
			d := v - m
			if d < 0 {
				d = -d
			}
			sum += d
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n) / 255
}

// Probe decides the field order from consecutive frames.
func (p *DeinterlacePlanner) Probe(frames []Frame) (FieldOrder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(frames) < 2 {
		return FieldOrderUnknown, ErrBadFrames
	}
	for _, f := range frames {
		if err := validateFrame(f); err != nil {
			return FieldOrderUnknown, err
		}
	}
	for _, f := range frames[1:] {
		if f.Width != frames[0].Width || f.Height != frames[0].Height {
			return FieldOrderUnknown, ErrBadFrames
		}
	}
	even, odd := fieldDiffs(frames[0], frames[1])
	order := decideOrder(even, odd)
	p.order = order
	p.confirming = false
	p.confirmLeft = 0
	p.accumEven, p.accumOdd = 0, 0
	p.prevY = nil
	p.lastShot = frames[len(frames)-1].Shot
	p.shotSeen = true
	p.stats.Order = order
	return order, nil
}

// Plan picks the rebuild method for one frame.
func (p *DeinterlacePlanner) Plan(frame Frame) (Method, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateFrame(frame); err != nil {
		return "", err
	}

	if !p.shotSeen || frame.Shot != p.lastShot {
		if p.shotSeen {
			p.confirming = true
			p.confirmLeft = p.confirm
			p.accumEven, p.accumOdd = 0, 0
			p.prevY = nil
			p.order = FieldOrderUnknown
		}
		p.lastShot = frame.Shot
		p.shotSeen = true
	}

	var method Method
	switch {
	case p.confirming:
		if p.prevY != nil && p.prevWidth == frame.Width && p.prevHeight == frame.Height {
			prev := Frame{Width: p.prevWidth, Height: p.prevHeight, Y: p.prevY}
			even, odd := fieldDiffs(prev, frame)
			p.accumEven += even
			p.accumOdd += odd
		}
		p.prevY = append(p.prevY[:0], frame.Y...)
		p.prevWidth, p.prevHeight = frame.Width, frame.Height
		p.confirmLeft--
		if p.confirmLeft <= 0 {
			p.order = decideOrder(p.accumEven, p.accumOdd)
			p.confirming = false
		}
		method = MethodFieldBlend
	case p.order != FieldOrderTopFirst && p.order != FieldOrderBottomFirst:
		method = MethodFieldBlend
	default:
		period := cadencePeriod(p.cadence)
		if period > 0 && isRepeatPosition(p.cadence, p.offset%period) {
			method = MethodRepeat
			break
		}
		comb := combMetric(frame)
		method = p.method
		switch p.method {
		case MethodInterpolate:
			if comb < p.threshold*0.8 {
				p.method = MethodWeave
				p.stats.Flips++
				method = MethodWeave
			}
		default:
			if comb > p.threshold*1.2 {
				p.method = MethodInterpolate
				p.stats.Flips++
				method = MethodInterpolate
			} else {
				method = MethodWeave
			}
		}
	}

	p.stats.Frames++
	switch method {
	case MethodWeave:
		p.stats.Weave++
	case MethodInterpolate:
		p.stats.Interpolate++
	case MethodFieldBlend:
		p.stats.Blend++
	case MethodRepeat:
		p.stats.Repeat++
	}
	p.stats.Order = p.order
	p.offset++
	return method, nil
}

// Apply rebuilds a frame with the method of the given plan.
func (p *DeinterlacePlanner) Apply(plan Plan, frame Frame) (Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateFrame(frame); err != nil {
		return Frame{}, err
	}
	w, h := frame.Width, frame.Height
	switch plan.Method {
	case MethodWeave, MethodRepeat:
		return frame, nil
	case MethodInterpolate:
		out := frame
		out.Y = make([]uint8, len(frame.Y))
		copy(out.Y, frame.Y)
		for r := 1; r < h; r += 2 {
			prev := r - 1
			next := r + 1
			if next >= h {
				next = prev
			}
			for x := 0; x < w; x++ {
				out.Y[r*w+x] = uint8((uint16(frame.Y[prev*w+x]) + uint16(frame.Y[next*w+x])) / 2)
			}
		}
		return out, nil
	case MethodFieldBlend:
		out := frame
		out.Y = make([]uint8, len(frame.Y))
		copy(out.Y, frame.Y)
		for r := 0; r+1 < h; r += 2 {
			for x := 0; x < w; x++ {
				avg := uint8((uint16(frame.Y[r*w+x]) + uint16(frame.Y[(r+1)*w+x])) / 2)
				out.Y[r*w+x] = avg
				out.Y[(r+1)*w+x] = avg
			}
		}
		return out, nil
	default:
		return Frame{}, ErrBadPlan
	}
}

// SetMotionThreshold changes the comb metric threshold.
func (p *DeinterlacePlanner) SetMotionThreshold(v float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v <= 0 || v > 1 {
		return ErrBadThreshold
	}
	p.threshold = v
	return nil
}

// SetCadence changes the field repetition rhythm.
func (p *DeinterlacePlanner) SetCadence(c Cadence) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !validCadence(c) {
		return ErrBadCadence
	}
	p.cadence = c
	p.offset = 0
	return nil
}

// Order reports the field order currently in force.
func (p *DeinterlacePlanner) Order() FieldOrder {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.order
}

// Cadence reports the rhythm currently in force.
func (p *DeinterlacePlanner) Cadence() Cadence {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cadence
}

// Reset drops the confirmation state, the cadence position and the counters.
func (p *DeinterlacePlanner) Reset() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.order = FieldOrderUnknown
	p.lastShot = 0
	p.shotSeen = false
	p.confirming = false
	p.confirmLeft = 0
	p.accumEven, p.accumOdd = 0, 0
	p.prevY = nil
	p.method = MethodWeave
	p.offset = 0
	p.stats = DeintStats{Order: FieldOrderUnknown}
	return nil
}

// Stats reads the lifetime counters.
func (p *DeinterlacePlanner) Stats() DeintStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats
	stats.Order = p.order
	return stats
}

// plannerSnapshot is the serialised form of the planner state.
type plannerSnapshot struct {
	Threshold   float64    `json:"threshold"`
	Cadence     string     `json:"cadence"`
	Confirm     int        `json:"confirm"`
	Order       FieldOrder `json:"order"`
	Method      Method     `json:"method"`
	Offset      int        `json:"offset"`
	LastShot    int64      `json:"last_shot"`
	ShotSeen    bool       `json:"shot_seen"`
	Confirming  bool       `json:"confirming"`
	ConfirmLeft int        `json:"confirm_left"`
	Stats       DeintStats `json:"stats"`
}

// Snapshot serialises the field order, the cadence position and the counters.
func (p *DeinterlacePlanner) Snapshot() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := plannerSnapshot{
		Threshold:   p.threshold,
		Cadence:     p.cadence.Pattern,
		Confirm:     p.confirm,
		Order:       p.order,
		Method:      p.method,
		Offset:      p.offset,
		LastShot:    p.lastShot,
		ShotSeen:    p.shotSeen,
		Confirming:  p.confirming,
		ConfirmLeft: p.confirmLeft,
		Stats:       p.stats,
	}
	snap.Stats.Order = p.order
	return json.Marshal(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *DeinterlacePlanner) Restore(data []byte) error {
	var snap plannerSnapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return ErrBadSnapshot
	}
	cadence := Cadence{Pattern: snap.Cadence}
	if !validCadence(cadence) {
		return ErrBadSnapshot
	}
	switch snap.Order {
	case FieldOrderUnknown, FieldOrderTopFirst, FieldOrderBottomFirst:
	default:
		return ErrBadSnapshot
	}
	switch snap.Method {
	case MethodWeave, MethodInterpolate:
	default:
		return ErrBadSnapshot
	}
	if snap.Threshold <= 0 || snap.Threshold > 1 {
		return ErrBadSnapshot
	}
	if snap.Offset < 0 || snap.Confirm < 0 || snap.ConfirmLeft < 0 {
		return ErrBadSnapshot
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.threshold = snap.Threshold
	p.cadence = cadence
	p.confirm = snap.Confirm
	p.order = snap.Order
	p.method = snap.Method
	p.offset = snap.Offset
	p.lastShot = snap.LastShot
	p.shotSeen = snap.ShotSeen
	p.confirming = snap.Confirming
	p.confirmLeft = snap.ConfirmLeft
	p.accumEven, p.accumOdd = 0, 0
	p.prevY = nil
	p.stats = snap.Stats
	p.stats.Order = snap.Order
	return nil
}
