// Package task schedules normal rebuilds for skinned and deformed meshes:
// smooth group accumulation, seam aware splitting and per frame budgets.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Vec2 is a two component texture coordinate.
type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Vec3 is a three component vector.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Tri indexes three vertices of a mesh.
type Tri struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

// RebuildMesh is the geometry the scheduler rebuilds normals for.
type RebuildMesh struct {
	ID    string `json:"id"`
	Verts []Vec3 `json:"verts"`
	UVs   []Vec2 `json:"uvs"`
	Tris  []Tri  `json:"tris"`
}

// SeamPolicy selects how duplicated seam vertices are treated.
type SeamPolicy int

// Supported seam policies.
const (
	SeamSplit SeamPolicy = iota
	SeamMerge
)

// NormalStats is the accounting view of the scheduler.
type NormalStats struct {
	Meshes      int     `json:"meshes"`
	Dirty       int     `json:"dirty"`
	Rebuilt     int     `json:"rebuilt"`
	Deferred    int     `json:"deferred"`
	SmoothAngle float64 `json:"smooth_angle"`
}

// Errors returned by the scheduler.
var (
	ErrBadName     = errors.New("normal scheduler: mesh name is required")
	ErrMeshExists  = errors.New("normal scheduler: mesh already registered")
	ErrBadMesh     = errors.New("normal scheduler: mesh has no triangles")
	ErrUnknownMesh = errors.New("normal scheduler: unknown mesh")
	ErrBadVertex   = errors.New("normal scheduler: vertex index out of range")
	ErrBadBudget   = errors.New("normal scheduler: budget must be positive")
	ErrBadAngle    = errors.New("normal scheduler: smooth angle must be within 0 and 180 degrees")
	ErrBadSeam     = errors.New("normal scheduler: unsupported seam policy")
	ErrBadSnapshot = errors.New("normal scheduler: snapshot payload is invalid")
)

// NormalRebuildScheduler owns the registered meshes, the dirty sets, the
// smooth angle and the seam policy.
type NormalRebuildScheduler struct {
	smoothAngle float64
	seamPolicy  SeamPolicy
}

// NewNormalRebuildScheduler builds a scheduler with a 60 degree smooth angle
// and seam splitting enabled.
func NewNormalRebuildScheduler() *NormalRebuildScheduler {
	return &NormalRebuildScheduler{smoothAngle: 60, seamPolicy: SeamSplit}
}

// RegisterMesh stores one mesh so its normals can be rebuilt later.
func (s *NormalRebuildScheduler) RegisterMesh(name string, mesh RebuildMesh) error {
	return errNotImplemented
}

// MarkDirty adds vertices to the dirty set of one mesh.
func (s *NormalRebuildScheduler) MarkDirty(mesh string, verts []int) error {
	return errNotImplemented
}

// Rebuild recomputes at most budget vertices of one mesh and reports how many
// vertices were actually rebuilt.
func (s *NormalRebuildScheduler) Rebuild(mesh string, budget int) (int, error) {
	return 0, errNotImplemented
}

// Normals returns a copy of the current vertex normals of one mesh.
func (s *NormalRebuildScheduler) Normals(mesh string) ([]Vec3, error) {
	return nil, errNotImplemented
}

// SetSmoothAngle sets the angle above which an edge becomes hard.
func (s *NormalRebuildScheduler) SetSmoothAngle(deg float64) error {
	return errNotImplemented
}

// SmoothAngle reports the configured smooth angle.
func (s *NormalRebuildScheduler) SmoothAngle() float64 {
	return 0
}

// SetSeamPolicy selects how seam duplicates are handled.
func (s *NormalRebuildScheduler) SetSeamPolicy(p SeamPolicy) error {
	return errNotImplemented
}

// DirtyCount reports how many vertices still wait for a rebuild.
func (s *NormalRebuildScheduler) DirtyCount(mesh string) int {
	return 0
}

// Stats reports the accounting of the scheduler.
func (s *NormalRebuildScheduler) Stats() NormalStats {
	return NormalStats{}
}

// Snapshot serialises meshes, dirty sets, normals and settings.
func (s *NormalRebuildScheduler) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *NormalRebuildScheduler) Restore(data []byte) error {
	return errNotImplemented
}
