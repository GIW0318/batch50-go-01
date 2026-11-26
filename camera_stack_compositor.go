// Package task composes the base camera and the overlay cameras of one frame
// into an ordered list of render steps.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Caps applied when the matching limit is left at zero or below.
const (
	DefaultMaxOverlays = 8
	DefaultMaxSteps    = 16
)

// Errors reported by the compositor.
var (
	ErrBadCamera       = errors.New("camera definition is invalid")
	ErrBaseExists      = errors.New("a base camera is already registered")
	ErrNoBase          = errors.New("no base camera is registered")
	ErrBaseRequired    = errors.New("the base camera cannot be used this way")
	ErrDuplicateCamera = errors.New("camera id is already used")
	ErrOverlayLimit    = errors.New("overlay camera limit is reached")
	ErrStepLimit       = errors.New("render step limit is reached")
	ErrNotFound        = errors.New("camera is not registered")
	ErrBadViewport     = errors.New("viewport is invalid")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Limits bounds the overlay table and the number of steps in one frame.
type Limits struct {
	MaxOverlays int
	MaxSteps    int
}

// Rect is the screen rectangle a camera renders into.
type Rect struct {
	X float64
	Y float64
	W float64
	H float64
}

// CameraSpec is one camera of the stack. ClearColor and ClearDepth say which
// buffers the camera clears before it draws, a zero Viewport means the stack
// viewport applies.
type CameraSpec struct {
	ID           string
	Order        int
	ClearColor   bool
	ClearDepth   bool
	DepthEnabled bool
	IsUI         bool
	Viewport     Rect
}

// RenderStep is one entry of the composed frame.
type RenderStep struct {
	CameraID     string
	Seq          int
	Order        int
	ClearColor   bool
	ClearDepth   bool
	DepthEnabled bool
	IsUI         bool
	Rect         Rect
}

// ComposeStats reports the counters kept by the compositor.
type ComposeStats struct {
	Composes   int
	Registered int
	Removed    int
	Rejects    int
}

// CameraStackCompositor owns the base camera, the overlay table and the frame
// viewport.
type CameraStackCompositor struct {
	limits Limits
}

// NewCameraStackCompositor builds a compositor with the given caps.
func NewCameraStackCompositor(limits Limits) *CameraStackCompositor {
	return &CameraStackCompositor{limits: limits}
}

// Push registers the base camera of the stack.
func (c *CameraStackCompositor) Push(base CameraSpec) error {
	return errNotImplemented
}

// Overlay registers one overlay camera with its sort order.
func (c *CameraStackCompositor) Overlay(camera CameraSpec, order int) error {
	return errNotImplemented
}

// Remove drops one overlay camera.
func (c *CameraStackCompositor) Remove(cameraID string) error {
	return errNotImplemented
}

// Compose turns the stack into the ordered render steps of one frame.
func (c *CameraStackCompositor) Compose() ([]RenderStep, error) {
	return nil, errNotImplemented
}

// SetViewport sets the frame rectangle used by cameras without their own.
func (c *CameraStackCompositor) SetViewport(rect Rect) error {
	return errNotImplemented
}

// Viewport reads the frame rectangle in use.
func (c *CameraStackCompositor) Viewport() Rect {
	return Rect{}
}

// SetOrder changes the sort order of one overlay camera.
func (c *CameraStackCompositor) SetOrder(cameraID string, order int) error {
	return errNotImplemented
}

// Base reads the registered base camera.
func (c *CameraStackCompositor) Base() (CameraSpec, bool) {
	return CameraSpec{}, false
}

// Overlays lists the overlay cameras in compose order.
func (c *CameraStackCompositor) Overlays() []CameraSpec {
	return nil
}

// Cameras lists the whole stack, base camera first.
func (c *CameraStackCompositor) Cameras() []CameraSpec {
	return nil
}

// Stats returns a copy of the compositor counters.
func (c *CameraStackCompositor) Stats() ComposeStats {
	return ComposeStats{}
}

// Snapshot serialises the base camera, the overlays and the viewport.
func (c *CameraStackCompositor) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the compositor state with a snapshot, all or nothing.
func (c *CameraStackCompositor) Restore(data []byte) error {
	return errNotImplemented
}
