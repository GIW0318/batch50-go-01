// Package task plans the near and far planes, the depth clear value and the
// compare direction of the render passes of one camera.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	cfg DepthConfig
}

// NewDepthPrecisionPlanner builds a planner with the given configuration.
func NewDepthPrecisionPlanner(cfg DepthConfig) *DepthPrecisionPlanner {
	return &DepthPrecisionPlanner{cfg: cfg}
}

// AddPass registers one render pass and the range it asks for.
func (p *DepthPrecisionPlanner) AddPass(passID string, rangeHint DepthRange) error {
	return errNotImplemented
}

// RemovePass drops one render pass.
func (p *DepthPrecisionPlanner) RemovePass(passID string) error {
	return errNotImplemented
}

// Passes lists the registered pass ids in ascending order.
func (p *DepthPrecisionPlanner) Passes() []string {
	return nil
}

// SetBounds registers or replaces the bounds of one scene object.
func (p *DepthPrecisionPlanner) SetBounds(objectID string, b Bounds) error {
	return errNotImplemented
}

// RemoveObject drops one scene object.
func (p *DepthPrecisionPlanner) RemoveObject(objectID string) error {
	return errNotImplemented
}

// Objects lists the registered object ids in ascending order.
func (p *DepthPrecisionPlanner) Objects() []string {
	return nil
}

// Plan computes the camera range and the per pass depth state.
func (p *DepthPrecisionPlanner) Plan(camera CameraParams) ([]PassDepth, error) {
	return nil, errNotImplemented
}

// Validate lists the passes whose depth precision is below the budget.
func (p *DepthPrecisionPlanner) Validate() []DepthIssue {
	return nil
}

// SetReversedZ switches the depth convention of the pipeline.
func (p *DepthPrecisionPlanner) SetReversedZ(on bool) error {
	return errNotImplemented
}

// ReversedZ reports the depth convention in use.
func (p *DepthPrecisionPlanner) ReversedZ() bool {
	return false
}

// Precision returns the smallest distinguishable depth step at a distance.
func (p *DepthPrecisionPlanner) Precision(atDepth float64) (float64, error) {
	return 0, errNotImplemented
}

// Stats returns a copy of the planner counters.
func (p *DepthPrecisionPlanner) Stats() DepthStats {
	return DepthStats{}
}

// Snapshot serialises the pass table, the bounds and the planned ranges.
func (p *DepthPrecisionPlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the planner state with a snapshot, all or nothing.
func (p *DepthPrecisionPlanner) Restore(data []byte) error {
	return errNotImplemented
}
