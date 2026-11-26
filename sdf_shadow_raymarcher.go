// Package task marches shadow rays through a voxelised signed distance field,
// keeps the self shadow bias tied to the voxel size and reports cone shaped
// penumbra for area lights.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxFields = 8
	DefaultMaxLights = 8
	DefaultMaxVoxels = 4096
	DefaultMaxSteps  = 64
	DefaultBaseBias  = 0.001
)

// Errors reported by the raymarcher.
var (
	ErrBadField      = errors.New("field definition is invalid")
	ErrFieldLimit    = errors.New("field table is full")
	ErrVoxelLimit    = errors.New("voxel count exceeds the per field cap")
	ErrBadVoxel      = errors.New("voxel definition is invalid")
	ErrBadLight      = errors.New("light specification is invalid")
	ErrLightLimit    = errors.New("light table is full")
	ErrUnknownField  = errors.New("field is not loaded")
	ErrUnknownLight  = errors.New("light is not registered")
	ErrNoField       = errors.New("no active field")
	ErrNoLight       = errors.New("no active light")
	ErrBadRay        = errors.New("ray parameters are invalid")
	ErrBadStepBudget = errors.New("step budget must be positive")
	ErrBadDistance   = errors.New("bias distance is invalid")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Vec3 is a position or direction in world space.
type Vec3 struct {
	X float64
	Y float64
	Z float64
}

// Voxel is one axis aligned cell of the distance field. Cells whose density is
// at least 0.5 count as occluders.
type Voxel struct {
	Min     Vec3
	Max     Vec3
	Density float64
}

// LightSpec is an area light; Radius 0 means a point light without penumbra.
type LightSpec struct {
	Position  Vec3
	Radius    float64
	Intensity float64
}

// ShadowResult is the outcome of one shadow ray.
type ShadowResult struct {
	Occluded bool
	Certain  bool
	Steps    int
	Depth    float64
	Penumbra float64
}

// Limits bounds the field and light tables and the march itself.
type Limits struct {
	MaxFields int
	MaxLights int
	MaxVoxels int
	MaxSteps  int
	BaseBias  float64
}

// TraceStats counts the marching work done so far.
type TraceStats struct {
	Fields int
	Voxels int
	Traces int64
	Steps  int64
}

// SdfShadowRaymarcher owns the voxel fields, the lights and the step budget.
type SdfShadowRaymarcher struct {
	limits Limits
}

// NewSdfShadowRaymarcher builds a raymarcher with the given caps.
func NewSdfShadowRaymarcher(limits Limits) *SdfShadowRaymarcher {
	return &SdfShadowRaymarcher{limits: limits}
}

// LoadField stores one voxel field and makes the first loaded field active.
func (m *SdfShadowRaymarcher) LoadField(fieldID string, voxels []Voxel) error {
	return errNotImplemented
}

// SetLight stores one light and makes the first registered light active.
func (m *SdfShadowRaymarcher) SetLight(lightID string, light LightSpec) error {
	return errNotImplemented
}

// Trace marches a shadow ray from origin along dir up to maxDist.
func (m *SdfShadowRaymarcher) Trace(origin, dir Vec3, maxDist float64) (ShadowResult, error) {
	return ShadowResult{}, errNotImplemented
}

// SetStepBudget changes how many marching steps one ray may take.
func (m *SdfShadowRaymarcher) SetStepBudget(n int) error {
	return errNotImplemented
}

// StepBudget reports the step budget in force.
func (m *SdfShadowRaymarcher) StepBudget() int {
	return 0
}

// Bias returns the self shadow offset for a surface distance.
func (m *SdfShadowRaymarcher) Bias(distance float64) (float64, error) {
	return 0, errNotImplemented
}

// Fields lists the loaded field ids in ascending order.
func (m *SdfShadowRaymarcher) Fields() []string {
	return nil
}

// Lights lists the registered light ids in ascending order.
func (m *SdfShadowRaymarcher) Lights() []string {
	return nil
}

// Active reports the active field id and the active light id.
func (m *SdfShadowRaymarcher) Active() (string, string) {
	return "", ""
}

// SetActiveField switches the field used by Trace.
func (m *SdfShadowRaymarcher) SetActiveField(fieldID string) error {
	return errNotImplemented
}

// SetActiveLight switches the light used for the penumbra estimate.
func (m *SdfShadowRaymarcher) SetActiveLight(lightID string) error {
	return errNotImplemented
}

// Limits reports the caps in force after defaults have been applied.
func (m *SdfShadowRaymarcher) Limits() Limits {
	return m.limits
}

// Stats reports the field table size and the marching counters.
func (m *SdfShadowRaymarcher) Stats() TraceStats {
	return TraceStats{}
}

// Snapshot serialises the fields, the lights, the selection and the counters.
func (m *SdfShadowRaymarcher) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (m *SdfShadowRaymarcher) Restore(data []byte) error {
	return errNotImplemented
}
