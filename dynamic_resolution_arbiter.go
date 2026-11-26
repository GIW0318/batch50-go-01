// Package task keeps one render scale tier per viewport and drives render
// target rebuilds from frame time feedback.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// ResolutionLadder is the fixed set of render scales a viewport may pick.
var ResolutionLadder = []float64{0.50, 0.60, 0.70, 0.80, 0.90, 1.00}

// Defaults applied when the matching ArbiterConfig field is left unset.
const (
	DefaultTargetMs       = 16.0
	DefaultUpStreak       = 8
	DefaultDownStreak     = 3
	DefaultCooldownFrames = 0
)

// Errors reported by the arbiter.
var (
	ErrBadViewport       = errors.New("viewport is invalid")
	ErrDuplicateViewport = errors.New("viewport already registered")
	ErrUnknownViewport   = errors.New("viewport not found")
	ErrBadRange          = errors.New("scale range is invalid")
	ErrBadSample         = errors.New("frame sample is invalid")
	ErrBadReason         = errors.New("freeze reason is required")
	ErrNotFrozen         = errors.New("viewport is not frozen")
	ErrNoRebuild         = errors.New("no pending rebuild")
	ErrRebuildBlocked    = errors.New("rebuild blocked by in flight frames")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// ArbiterConfig tunes the tier ladder policy.
type ArbiterConfig struct {
	TargetMs       float64
	UpStreak       int
	DownStreak     int
	CooldownFrames int
}

// FrameSample is one frame time report of a viewport.
type FrameSample struct {
	FrameTimeMs float64
	InFlight    int
}

// RebuildRequest is the render target rebuild queued by a tier decision.
type RebuildRequest struct {
	Viewport string
	From     float64
	To       float64
	Sample   uint64
}

// ViewportStats is the counter block of one viewport.
type ViewportStats struct {
	Reports    int
	UpShifts   int
	DownShifts int
	Rebuilds   int
	Frozen     bool
	StreakUp   int
	StreakDown int
}

// DynamicResolutionArbiter owns the per viewport tiers, the streak counters
// and the pending rebuild requests.
type DynamicResolutionArbiter struct {
	cfg ArbiterConfig
}

// NewDynamicResolutionArbiter builds an arbiter with the given policy.
func NewDynamicResolutionArbiter(cfg ArbiterConfig) *DynamicResolutionArbiter {
	return &DynamicResolutionArbiter{cfg: cfg}
}

// AddViewport registers a viewport at an initial ladder step.
func (a *DynamicResolutionArbiter) AddViewport(viewport string, initial float64) error {
	return errNotImplemented
}

// RemoveViewport drops a viewport and its counters.
func (a *DynamicResolutionArbiter) RemoveViewport(viewport string) error {
	return errNotImplemented
}

// Viewports lists the registered viewports in ascending order.
func (a *DynamicResolutionArbiter) Viewports() []string {
	return nil
}

// Scale returns the active render scale of a viewport.
func (a *DynamicResolutionArbiter) Scale(viewport string) float64 {
	return 0
}

// Ladder returns the ladder steps allowed by the current range.
func (a *DynamicResolutionArbiter) Ladder(viewport string) []float64 {
	return nil
}

// OverlayScale returns the scale used by UI and subtitle layers, which never
// follow the render tier.
func (a *DynamicResolutionArbiter) OverlayScale(viewport string) float64 {
	return 0
}

// SetRange narrows the allowed ladder steps of a viewport.
func (a *DynamicResolutionArbiter) SetRange(viewport string, min, max float64) error {
	return errNotImplemented
}

// Report feeds one frame time sample into the tier decision.
func (a *DynamicResolutionArbiter) Report(viewport string, sample FrameSample) error {
	return errNotImplemented
}

// Freeze pins the current tier and records the operator reason.
func (a *DynamicResolutionArbiter) Freeze(viewport, reason string) error {
	return errNotImplemented
}

// Resume releases a frozen viewport.
func (a *DynamicResolutionArbiter) Resume(viewport string) error {
	return errNotImplemented
}

// Frozen reports the freeze state and its reason.
func (a *DynamicResolutionArbiter) Frozen(viewport string) (bool, string) {
	return false, ""
}

// PendingRebuild reads the queued rebuild request of a viewport.
func (a *DynamicResolutionArbiter) PendingRebuild(viewport string) (RebuildRequest, bool) {
	return RebuildRequest{}, false
}

// CommitRebuild applies or drops the queued rebuild request.
func (a *DynamicResolutionArbiter) CommitRebuild(viewport string, ok bool) error {
	return errNotImplemented
}

// Stats returns the counter block of one viewport.
func (a *DynamicResolutionArbiter) Stats(viewport string) ViewportStats {
	return ViewportStats{}
}

// Snapshot serialises every viewport tier, range, streak and pending rebuild.
func (a *DynamicResolutionArbiter) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot payload.
func (a *DynamicResolutionArbiter) Restore(data []byte) error {
	return errNotImplemented
}
