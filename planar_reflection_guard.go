// Package task builds mirror cameras for planar reflections, keeps the oblique
// near plane of each mirror consistent with the reflecting plane and paces the
// reflection atlas updates of every registered plane.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxPlanes   = 16
	DefaultAtlasWidth  = 4096
	DefaultAtlasHeight = 4096
)

// Errors reported by the reflection guard.
var (
	ErrBadPlane       = errors.New("plane specification is invalid")
	ErrDuplicatePlane = errors.New("plane is already registered")
	ErrPlaneLimit     = errors.New("plane table is full")
	ErrUnknownPlane   = errors.New("plane is not registered")
	ErrBadCamera      = errors.New("camera parameters are invalid")
	ErrBadUpdateRate  = errors.New("update rate must be positive")
	ErrBadFrame       = errors.New("frame number must strictly increase")
	ErrBadResolution  = errors.New("reflection resolution is invalid")
	ErrAtlasBudget    = errors.New("reflection atlas budget is exceeded")
	ErrNoFrame        = errors.New("no frame is open")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Vec3 is a position or direction in world space.
type Vec3 struct {
	X float64
	Y float64
	Z float64
}

// Vec4 is a clip plane written as (x, y, z, w); the kept half space is the one
// where x*X+y*Y+z*Z+w is not negative.
type Vec4 struct {
	X float64
	Y float64
	Z float64
	W float64
}

// Limits bounds the number of planes and the shared reflection atlas.
type Limits struct {
	MaxPlanes   int
	AtlasWidth  int
	AtlasHeight int
}

// PlaneSpec describes one reflecting plane.
type PlaneSpec struct {
	Point    Vec3
	Normal   Vec3
	Priority int
	Width    int
	Height   int
}

// CameraParams describes the player camera used to build a mirror camera.
type CameraParams struct {
	Position Vec3
	Forward  Vec3
	Up       Vec3
	FOV      float64
	Near     float64
	Far      float64
}

// MirrorCamera is the reflected camera with the oblique near plane applied.
type MirrorCamera struct {
	PlaneID   string
	Position  Vec3
	Forward   Vec3
	Up        Vec3
	Near      float64
	Far       float64
	ClipPlane Vec4
	Enabled   bool
	Reason    string
}

// AtlasEntry is the viewport one plane owns inside the shared atlas.
type AtlasEntry struct {
	PlaneID string
	X       int
	Y       int
	W       int
	H       int
}

// AtlasLayout is the current packing of the shared reflection atlas.
type AtlasLayout struct {
	Width   int
	Height  int
	Entries []AtlasEntry
}

// ReflectionState is what State and Commit report for one plane.
type ReflectionState struct {
	PlaneID    string
	Generation int64
	Frame      int
	EveryN     int
	Updated    bool
	AtlasX     int
	AtlasY     int
	AtlasW     int
	AtlasH     int
	Enabled    bool
}

// ReflectionStats counts the atlas traffic of the guard.
type ReflectionStats struct {
	Planes  int
	Updates int64
	Skipped int64
}

// PlanarReflectionGuard owns the plane table, the atlas packing and the frame
// counter used for update pacing.
type PlanarReflectionGuard struct {
	limits Limits
}

// NewPlanarReflectionGuard builds a guard with the given caps.
func NewPlanarReflectionGuard(limits Limits) *PlanarReflectionGuard {
	return &PlanarReflectionGuard{limits: limits}
}

// RegisterPlane adds one reflecting plane to the table and the atlas.
func (g *PlanarReflectionGuard) RegisterPlane(planeID string, plane PlaneSpec) error {
	return errNotImplemented
}

// BuildMirrorCamera reflects the camera about the given plane.
func (g *PlanarReflectionGuard) BuildMirrorCamera(planeID string, camera CameraParams) (MirrorCamera, error) {
	return MirrorCamera{}, errNotImplemented
}

// SetUpdateRate changes how many frames apart one plane may be re-rendered.
func (g *PlanarReflectionGuard) SetUpdateRate(planeID string, everyN int) error {
	return errNotImplemented
}

// Commit records one reflection render attempt for the current frame.
func (g *PlanarReflectionGuard) Commit(planeID string) (ReflectionState, error) {
	return ReflectionState{}, errNotImplemented
}

// Invalidate forces the next commit of a plane regardless of the update rate.
func (g *PlanarReflectionGuard) Invalidate(planeID string) error {
	return errNotImplemented
}

// State reads the reflection state of one plane.
func (g *PlanarReflectionGuard) State(planeID string) (ReflectionState, bool) {
	return ReflectionState{}, false
}

// Planes lists the registered plane ids in ascending order.
func (g *PlanarReflectionGuard) Planes() []string {
	return nil
}

// SetResolution changes the reflection resolution of one plane.
func (g *PlanarReflectionGuard) SetResolution(planeID string, width, height int) error {
	return errNotImplemented
}

// BeginFrame opens the frame used by Commits.
func (g *PlanarReflectionGuard) BeginFrame(frame int) error {
	return errNotImplemented
}

// Frame reports the frame number currently open, or the last closed one.
func (g *PlanarReflectionGuard) Frame() int {
	return 0
}

// Atlas reports the current atlas packing.
func (g *PlanarReflectionGuard) Atlas() AtlasLayout {
	return AtlasLayout{}
}

// Stats reports the number of planes and the commit counters.
func (g *PlanarReflectionGuard) Stats() ReflectionStats {
	return ReflectionStats{}
}

// Snapshot serialises the plane table, the atlas and the frame counter.
func (g *PlanarReflectionGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PlanarReflectionGuard) Restore(data []byte) error {
	return errNotImplemented
}
