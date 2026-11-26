// Package task plans the near and far planes, the depth clear value and the
// compare direction of the render passes of one camera.
package task

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
)

// Compare functions written into a render step.
const (
	CompareLess    = "less"
	CompareGreater = "greater"
)

// Depth issue codes reported by Validate.
const (
	IssuePrecision = "precision"
)

// Errors reported by the planner.
var (
	ErrBadPass       = errors.New("pass definition is invalid")
	ErrDuplicatePass = errors.New("pass is already registered")
	ErrUnknownPass   = errors.New("pass is not registered")
	ErrBadBounds     = errors.New("object bounds are invalid")
	ErrBadCamera     = errors.New("camera cannot see the registered scene")
	ErrNoPlan        = errors.New("no depth range has been planned yet")
	ErrBadDepth      = errors.New("sample depth is outside the planned range")
	ErrNotFound      = errors.New("object is not registered")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// DepthConfig bounds the tightest near plane, the farthest far plane and the
// largest depth error the pipeline accepts in world units.
type DepthConfig struct {
	MinNear         float64
	MaxFar          float64
	PrecisionBudget float64
}

// Bounds is the extent of one scene object along the view axis, in world
// coordinates.
type Bounds struct {
	Min float64
	Max float64
}

// DepthRange is the range one pass asks for, measured from the camera.
type DepthRange struct {
	Near float64
	Far  float64
}

// CameraParams is the camera state used for one planning round.
type CameraParams struct {
	Position float64
	Bits     int
}

// PassDepth is the planned depth state of one pass.
type PassDepth struct {
	PassID      string
	Near        float64
	Far         float64
	Reversed    bool
	ClearValue  float64
	CompareFunc string
}

// DepthIssue is one problem found by Validate.
type DepthIssue struct {
	PassID string
	Near   float64
	Far    float64
	Code   string
}

// DepthStats reports the counters kept by the planner.
type DepthStats struct {
	Plans   int
	Adds    int
	Removes int
	Rejects int
	Objects int
	Passes  int
}

// DepthPrecisionPlanner owns the pass table, the object bounds and the last
// planned ranges.
type DepthPrecisionPlanner struct {
	// One planner belongs to one camera: every field below is per instance,
	// so child and parent cameras never share a near/far mapping.
	mu       sync.Mutex
	cfg      DepthConfig
	passes   map[string]DepthRange
	objects  map[string]Bounds
	reversed bool
	last     *planState
	stats    DepthStats
}

// planState is the result of the last successful Plan. It is replaced
// atomically, never mutated in place, so concurrent readers can never observe
// a half-updated plan.
type planState struct {
	Near   float64
	Far    float64
	Bits   int
	Passes []PassDepth
}

// plannerSnapshot is the cross-process serialisation format.
type plannerSnapshot struct {
	Passes   map[string]DepthRange
	Objects  map[string]Bounds
	Reversed bool
	Plan     *planState
	Stats    DepthStats
}

// validRange reports whether a pass range hint is usable. The zero range
// {0, 0} is the follow-camera sentinel.
func validRange(r DepthRange) bool {
	if math.IsNaN(r.Near) || math.IsNaN(r.Far) {
		return false
	}
	if r.Near == 0 && r.Far == 0 {
		return true
	}
	return r.Near >= 0 && r.Far > r.Near
}

// validBounds reports whether object bounds describe a positive extent.
func validBounds(b Bounds) bool {
	if math.IsNaN(b.Min) || math.IsNaN(b.Max) {
		return false
	}
	return b.Max > b.Min
}

// NewDepthPrecisionPlanner builds a planner with the given configuration.
func NewDepthPrecisionPlanner(cfg DepthConfig) *DepthPrecisionPlanner {
	return &DepthPrecisionPlanner{
		cfg:     cfg,
		passes:  map[string]DepthRange{},
		objects: map[string]Bounds{},
	}
}

// AddPass registers one render pass and the range it asks for.
func (p *DepthPrecisionPlanner) AddPass(passID string, rangeHint DepthRange) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(passID) == "" || !validRange(rangeHint) {
		p.stats.Rejects++
		return ErrBadPass
	}
	if _, ok := p.passes[passID]; ok {
		p.stats.Rejects++
		return ErrDuplicatePass
	}
	p.passes[passID] = rangeHint
	p.stats.Adds++
	p.stats.Passes = len(p.passes)
	return nil
}

// RemovePass drops one render pass.
func (p *DepthPrecisionPlanner) RemovePass(passID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.passes[passID]; !ok {
		p.stats.Rejects++
		return ErrUnknownPass
	}
	delete(p.passes, passID)
	p.stats.Removes++
	p.stats.Passes = len(p.passes)
	return nil
}

// Passes lists the registered pass ids in ascending order.
func (p *DepthPrecisionPlanner) Passes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.passes))
	for id := range p.passes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SetBounds registers or replaces the bounds of one scene object.
func (p *DepthPrecisionPlanner) SetBounds(objectID string, b Bounds) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(objectID) == "" || !validBounds(b) {
		p.stats.Rejects++
		return ErrBadBounds
	}
	p.objects[objectID] = b
	p.stats.Objects = len(p.objects)
	return nil
}

// RemoveObject drops one scene object.
func (p *DepthPrecisionPlanner) RemoveObject(objectID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.objects[objectID]; !ok {
		p.stats.Rejects++
		return ErrNotFound
	}
	delete(p.objects, objectID)
	p.stats.Removes++
	p.stats.Objects = len(p.objects)
	return nil
}

// Objects lists the registered object ids in ascending order.
func (p *DepthPrecisionPlanner) Objects() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.objects))
	for id := range p.objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Plan computes the camera range and the per pass depth state.
func (p *DepthPrecisionPlanner) Plan(camera CameraParams) ([]PassDepth, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if camera.Bits < 1 {
		p.stats.Rejects++
		return nil, ErrBadCamera
	}

	near := p.cfg.MinNear
	far := p.cfg.MaxFar
	if len(p.objects) > 0 {
		nearest := math.Inf(1)
		farthest := math.Inf(-1)
		for _, b := range p.objects {
			if d := b.Min - camera.Position; d < nearest {
				nearest = d
			}
			if d := b.Max - camera.Position; d > farthest {
				farthest = d
			}
		}
		// Every surface is at or behind the camera plane.
		if farthest <= 0 {
			p.stats.Rejects++
			return nil, ErrBadCamera
		}
		near = math.Max(p.cfg.MinNear, nearest)
		far = math.Min(p.cfg.MaxFar, farthest)
	}
	if far <= near {
		p.stats.Rejects++
		return nil, ErrBadCamera
	}

	compare := CompareLess
	clearValue := 1.0
	if p.reversed {
		compare = CompareGreater
		clearValue = 0.0
	}

	state := &planState{
		Near:   near,
		Far:    far,
		Bits:   camera.Bits,
		Passes: make([]PassDepth, 0, len(p.passes)),
	}
	ids := make([]string, 0, len(p.passes))
	for id := range p.passes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		hint := p.passes[id]
		passNear, passFar := near, far
		if !(hint.Near == 0 && hint.Far == 0) {
			passNear = math.Max(near, hint.Near)
			passFar = math.Min(far, hint.Far)
		}
		// Equality on the far side is visible; only an empty or inverted
		// intersection is rejected.
		if passFar <= passNear {
			p.stats.Rejects++
			return nil, ErrBadCamera
		}
		state.Passes = append(state.Passes, PassDepth{
			PassID:      id,
			Near:        passNear,
			Far:         passFar,
			Reversed:    p.reversed,
			ClearValue:  clearValue,
			CompareFunc: compare,
		})
	}

	p.last = state
	p.stats.Plans++
	out := make([]PassDepth, len(state.Passes))
	copy(out, state.Passes)
	return out, nil
}

// Validate lists the passes whose depth precision is below the budget.
func (p *DepthPrecisionPlanner) Validate() []DepthIssue {
	p.mu.Lock()
	defer p.mu.Unlock()
	issues := []DepthIssue{}
	if p.last == nil {
		return issues
	}
	steps := math.Ldexp(1.0, p.last.Bits)
	for _, pass := range p.last.Passes {
		resolution := pass.Far * pass.Far * (pass.Far - pass.Near) /
			(pass.Far * pass.Near * steps)
		if resolution > p.cfg.PrecisionBudget {
			issues = append(issues, DepthIssue{
				PassID: pass.PassID,
				Near:   pass.Near,
				Far:    pass.Far,
				Code:   IssuePrecision,
			})
		}
	}
	return issues
}

// SetReversedZ switches the depth convention of the pipeline.
func (p *DepthPrecisionPlanner) SetReversedZ(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reversed = on
	return nil
}

// ReversedZ reports the depth convention in use.
func (p *DepthPrecisionPlanner) ReversedZ() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reversed
}

// Precision returns the smallest distinguishable depth step at a distance.
func (p *DepthPrecisionPlanner) Precision(atDepth float64) (float64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		return 0, ErrNoPlan
	}
	// The far plane itself is a visible surface, so equality is accepted and
	// only depths strictly past it are rejected.
	if math.IsNaN(atDepth) || atDepth <= 0 || atDepth > p.last.Far {
		return 0, ErrBadDepth
	}
	steps := math.Ldexp(1.0, p.last.Bits)
	return atDepth * atDepth * (p.last.Far - p.last.Near) /
		(p.last.Far * p.last.Near * steps), nil
}

// Stats returns a copy of the planner counters.
func (p *DepthPrecisionPlanner) Stats() DepthStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats
	stats.Passes = len(p.passes)
	stats.Objects = len(p.objects)
	return stats
}

// Snapshot serialises the pass table, the bounds and the planned ranges.
func (p *DepthPrecisionPlanner) Snapshot() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := plannerSnapshot{
		Passes:   make(map[string]DepthRange, len(p.passes)),
		Objects:  make(map[string]Bounds, len(p.objects)),
		Reversed: p.reversed,
		Stats:    p.stats,
	}
	for id, hint := range p.passes {
		snap.Passes[id] = hint
	}
	for id, b := range p.objects {
		snap.Objects[id] = b
	}
	if p.last != nil {
		plan := *p.last
		plan.Passes = append([]PassDepth(nil), p.last.Passes...)
		snap.Plan = &plan
	}
	data, err := json.Marshal(&snap)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the planner state with a snapshot, all or nothing.
func (p *DepthPrecisionPlanner) Restore(data []byte) error {
	var snap plannerSnapshot
	if len(data) == 0 {
		return ErrBadSnapshot
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}
	for id, hint := range snap.Passes {
		if strings.TrimSpace(id) == "" || !validRange(hint) {
			return ErrBadSnapshot
		}
	}
	for id, b := range snap.Objects {
		if strings.TrimSpace(id) == "" || !validBounds(b) {
			return ErrBadSnapshot
		}
	}
	if snap.Plan != nil && (snap.Plan.Bits < 1 || snap.Plan.Far <= snap.Plan.Near) {
		return ErrBadSnapshot
	}

	passes := make(map[string]DepthRange, len(snap.Passes))
	for id, hint := range snap.Passes {
		passes[id] = hint
	}
	objects := make(map[string]Bounds, len(snap.Objects))
	for id, b := range snap.Objects {
		objects[id] = b
	}
	var plan *planState
	if snap.Plan != nil {
		restored := *snap.Plan
		restored.Passes = append([]PassDepth(nil), snap.Plan.Passes...)
		plan = &restored
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.passes = passes
	p.objects = objects
	p.reversed = snap.Reversed
	p.last = plan
	p.stats = snap.Stats
	p.stats.Passes = len(passes)
	p.stats.Objects = len(objects)
	return nil
}
