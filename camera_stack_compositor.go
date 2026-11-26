// Package task composes the base camera and the overlay cameras of one frame
// into an ordered list of render steps.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Caps applied when the matching limit is left at zero or below.
const (
	DefaultMaxOverlays = 8
	DefaultMaxSteps    = 16

	snapshotVersion = 1
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

type overlayRecord struct {
	spec  CameraSpec
	order int
}

// CameraStackCompositor owns the base camera, the overlay table and the frame
// viewport.
type CameraStackCompositor struct {
	mu       sync.Mutex
	limits   Limits
	base     CameraSpec
	hasBase  bool
	overlays []overlayRecord
	viewport Rect
	stats    ComposeStats
}

type snapshotData struct {
	Version  int              `json:"version"`
	Base     *snapshotCamera  `json:"base,omitempty"`
	Overlays []snapshotCamera `json:"overlays"`
	Viewport Rect             `json:"viewport"`
}

type snapshotCamera struct {
	Camera CameraSpec `json:"camera"`
	Order  int        `json:"order"`
}

// NewCameraStackCompositor builds a compositor with the given caps.
func NewCameraStackCompositor(limits Limits) *CameraStackCompositor {
	return &CameraStackCompositor{limits: limits}
}

// Push registers the base camera of the stack.
func (c *CameraStackCompositor) Push(base CameraSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if blankCameraID(base.ID) {
		return c.rejectLocked(ErrBadCamera)
	}
	if c.hasBase {
		return c.rejectLocked(ErrBaseExists)
	}

	c.base = base
	c.hasBase = true
	c.stats.Registered++
	return nil
}

// Overlay registers one overlay camera with its sort order.
func (c *CameraStackCompositor) Overlay(camera CameraSpec, order int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.hasBase {
		return c.rejectLocked(ErrNoBase)
	}
	if blankCameraID(camera.ID) {
		return c.rejectLocked(ErrBadCamera)
	}
	if camera.ID == c.base.ID || c.overlayIndexLocked(camera.ID) >= 0 {
		return c.rejectLocked(ErrDuplicateCamera)
	}
	if len(c.overlays) >= c.maxOverlaysLocked() {
		return c.rejectLocked(ErrOverlayLimit)
	}

	camera.Order = order
	c.overlays = append(c.overlays, overlayRecord{spec: camera, order: order})
	c.stats.Registered++
	return nil
}

// Remove drops one overlay camera.
func (c *CameraStackCompositor) Remove(cameraID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasBase && cameraID == c.base.ID {
		return c.rejectLocked(ErrBaseRequired)
	}

	index := c.overlayIndexLocked(cameraID)
	if index < 0 {
		return c.rejectLocked(ErrNotFound)
	}

	last := len(c.overlays) - 1
	copy(c.overlays[index:], c.overlays[index+1:])
	c.overlays[last] = overlayRecord{}
	c.overlays = c.overlays[:last]
	c.stats.Removed++
	return nil
}

// Compose turns the stack into the ordered render steps of one frame.
func (c *CameraStackCompositor) Compose() ([]RenderStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.hasBase {
		return nil, c.rejectLocked(ErrNoBase)
	}

	totalSteps := len(c.overlays) + 1
	if totalSteps > c.maxStepsLocked() {
		return nil, c.rejectLocked(ErrStepLimit)
	}

	orderedOverlays := c.orderedOverlaysLocked()
	steps := make([]RenderStep, 0, totalSteps)
	seq := 1

	steps = append(steps, newRenderStep(c.base, c.base.Order, seq, c.viewport))
	seq++

	for _, overlay := range orderedOverlays {
		steps = append(steps, newRenderStep(overlay.spec, overlay.order, seq, c.viewport))
		seq++
	}

	c.stats.Composes++
	return steps, nil
}

// SetViewport sets the frame rectangle used by cameras without their own.
func (c *CameraStackCompositor) SetViewport(rect Rect) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !validViewport(rect) {
		return c.rejectLocked(ErrBadViewport)
	}

	c.viewport = rect
	return nil
}

// Viewport reads the frame rectangle in use.
func (c *CameraStackCompositor) Viewport() Rect {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.viewport
}

// SetOrder changes the sort order of one overlay camera.
func (c *CameraStackCompositor) SetOrder(cameraID string, order int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasBase && cameraID == c.base.ID {
		return c.rejectLocked(ErrBaseRequired)
	}

	index := c.overlayIndexLocked(cameraID)
	if index < 0 {
		return c.rejectLocked(ErrNotFound)
	}

	c.overlays[index].order = order
	c.overlays[index].spec.Order = order
	return nil
}

// Base reads the registered base camera.
func (c *CameraStackCompositor) Base() (CameraSpec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.hasBase {
		return CameraSpec{}, false
	}
	return c.base, true
}

// Overlays lists the overlay cameras in compose order.
func (c *CameraStackCompositor) Overlays() []CameraSpec {
	c.mu.Lock()
	defer c.mu.Unlock()

	ordered := c.orderedOverlaysLocked()
	result := make([]CameraSpec, 0, len(ordered))
	for _, overlay := range ordered {
		result = append(result, overlay.spec)
	}
	return result
}

// Cameras lists the whole stack, base camera first.
func (c *CameraStackCompositor) Cameras() []CameraSpec {
	c.mu.Lock()
	defer c.mu.Unlock()

	ordered := c.orderedOverlaysLocked()
	result := make([]CameraSpec, 0, len(ordered)+1)
	if c.hasBase {
		result = append(result, c.base)
	}
	for _, overlay := range ordered {
		result = append(result, overlay.spec)
	}
	return result
}

// Stats returns a copy of the compositor counters.
func (c *CameraStackCompositor) Stats() ComposeStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.stats
}

// Snapshot serialises the base camera, the overlays and the viewport.
func (c *CameraStackCompositor) Snapshot() ([]byte, error) {
	c.mu.Lock()
	payload := snapshotData{
		Version:  snapshotVersion,
		Overlays: make([]snapshotCamera, 0, len(c.overlays)),
		Viewport: c.viewport,
	}
	if c.hasBase {
		payload.Base = &snapshotCamera{Camera: c.base, Order: c.base.Order}
	}
	for _, overlay := range c.overlays {
		payload.Overlays = append(payload.Overlays, snapshotCamera{
			Camera: overlay.spec,
			Order:  overlay.order,
		})
	}
	c.mu.Unlock()

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode camera stack snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the compositor state with a snapshot, all or nothing.
func (c *CameraStackCompositor) Restore(data []byte) error {
	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	base, hasBase, overlays, viewport, err := payload.validState()
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.base = base
	c.hasBase = hasBase
	c.overlays = overlays
	c.viewport = viewport
	return nil
}

func (c *CameraStackCompositor) maxOverlaysLocked() int {
	if c.limits.MaxOverlays <= 0 {
		return DefaultMaxOverlays
	}
	return c.limits.MaxOverlays
}

func (c *CameraStackCompositor) maxStepsLocked() int {
	if c.limits.MaxSteps <= 0 {
		return DefaultMaxSteps
	}
	return c.limits.MaxSteps
}

func (c *CameraStackCompositor) rejectLocked(err error) error {
	c.stats.Rejects++
	return err
}

func (c *CameraStackCompositor) overlayIndexLocked(cameraID string) int {
	for i := range c.overlays {
		if c.overlays[i].spec.ID == cameraID {
			return i
		}
	}
	return -1
}

func (c *CameraStackCompositor) orderedOverlaysLocked() []overlayRecord {
	ordered := make([]overlayRecord, len(c.overlays))
	copy(ordered, c.overlays)

	sort.SliceStable(ordered, func(i, j int) bool {
		left := ordered[i].spec
		right := ordered[j].spec
		if left.IsUI != right.IsUI {
			return !left.IsUI
		}
		if ordered[i].order != ordered[j].order {
			return ordered[i].order < ordered[j].order
		}
		return false
	})

	return ordered
}

func (s snapshotData) validState() (CameraSpec, bool, []overlayRecord, Rect, error) {
	if s.Version != snapshotVersion {
		return CameraSpec{}, false, nil, Rect{}, fmt.Errorf("%w: unsupported snapshot version", ErrBadSnapshot)
	}
	if s.Viewport != (Rect{}) && !validViewport(s.Viewport) {
		return CameraSpec{}, false, nil, Rect{}, fmt.Errorf("%w: invalid viewport", ErrBadSnapshot)
	}

	ids := make(map[string]struct{}, len(s.Overlays)+1)
	var base CameraSpec
	hasBase := false
	if s.Base != nil {
		base = s.Base.Camera
		if blankCameraID(base.ID) {
			return CameraSpec{}, false, nil, Rect{}, fmt.Errorf("%w: blank base camera id", ErrBadSnapshot)
		}
		if _, exists := ids[base.ID]; exists {
			return CameraSpec{}, false, nil, Rect{}, fmt.Errorf("%w: duplicate camera id %q", ErrBadSnapshot, base.ID)
		}
		ids[base.ID] = struct{}{}
		base.Order = s.Base.Order
		hasBase = true
	}

	overlays := make([]overlayRecord, 0, len(s.Overlays))
	for _, saved := range s.Overlays {
		spec := saved.Camera
		if blankCameraID(spec.ID) {
			return CameraSpec{}, false, nil, Rect{}, fmt.Errorf("%w: blank overlay camera id", ErrBadSnapshot)
		}
		if _, exists := ids[spec.ID]; exists {
			return CameraSpec{}, false, nil, Rect{}, fmt.Errorf("%w: duplicate camera id %q", ErrBadSnapshot, spec.ID)
		}
		ids[spec.ID] = struct{}{}

		spec.Order = saved.Order
		overlays = append(overlays, overlayRecord{spec: spec, order: saved.Order})
	}

	return base, hasBase, overlays, s.Viewport, nil
}

func newRenderStep(camera CameraSpec, order, seq int, stackViewport Rect) RenderStep {
	rect := camera.Viewport
	if rect == (Rect{}) {
		rect = stackViewport
	}

	depthEnabled := camera.DepthEnabled
	if camera.IsUI {
		depthEnabled = false
	}

	return RenderStep{
		CameraID:     camera.ID,
		Seq:          seq,
		Order:        order,
		ClearColor:   camera.ClearColor,
		ClearDepth:   camera.ClearDepth,
		DepthEnabled: depthEnabled,
		IsUI:         camera.IsUI,
		Rect:         rect,
	}
}

func blankCameraID(id string) bool {
	return strings.TrimSpace(id) == ""
}

func validViewport(rect Rect) bool {
	return rect.W > 0 && rect.H > 0
}
