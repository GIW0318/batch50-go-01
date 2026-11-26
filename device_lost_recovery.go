// Package task rebuilds GPU resources after a device loss: it invalidates the
// old handle generation, rebuilds pipelines, textures, buffers and render
// targets in dependency order and freezes a degradation level once quality has
// been traded away.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxResources = 128
	DefaultMaxAttempts  = 3
	DefaultMaxLevel     = 3
)

// Resource kinds. The rebuild order follows this listing.
const (
	KindPipeline = "pipeline"
	KindTexture  = "texture"
	KindBuffer   = "buffer"
	KindTarget   = "render_target"
)

// Device states reported by Status.
const (
	StatusReady = "ready"
	StatusLost  = "lost"
)

// Errors reported by the recovery manager.
var (
	ErrBadResource       = errors.New("resource specification is invalid")
	ErrDuplicateResource = errors.New("resource name is already registered")
	ErrResourceLimit     = errors.New("resource table is full")
	ErrMissingDependency = errors.New("declared dependency is not registered")
	ErrUnknownHandle     = errors.New("unknown resource handle")
	ErrStaleHandle       = errors.New("resource handle belongs to a lost device")
	ErrDeviceLost        = errors.New("device is lost")
	ErrInvalidReason     = errors.New("loss reason is required")
	ErrAlreadyLost       = errors.New("device is already lost")
	ErrNotLost           = errors.New("device is not lost")
	ErrDependencyCycle   = errors.New("resource dependencies form a cycle")
	ErrRecoveryBudget    = errors.New("rebuild budget is exhausted")
	ErrBadDegradeLevel   = errors.New("degradation level is out of range")
	ErrDegradeLocked     = errors.New("quality cannot be raised again")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Limits bounds the resource table, the rebuild attempts of one loss episode
// and the deepest degradation level the manager may reach.
type Limits struct {
	MaxResources  int
	MaxAttempts   int
	MaxLevel      int
	RebuildBudget time.Duration
}

// Handle identifies one GPU resource of the current device generation. The
// zero value is never valid.
type Handle int64

// ResourceSpec is the description kept for a resource so it can be rebuilt.
type ResourceSpec struct {
	Name      string
	Kind      string
	Bytes     int
	DependsOn []string
}

// ResourceState is what Resolve reports for a live handle.
type ResourceState struct {
	Handle     Handle
	Name       string
	Kind       string
	Bytes      int
	Valid      bool
	Generation int
	Degraded   bool
}

// RenderRequest is one frame worth of draw work handed in by the renderer.
type RenderRequest struct {
	ID   string
	View string
}

// RecoveryReport describes one successful rebuild.
type RecoveryReport struct {
	Reason     string
	Generation int
	Rebuilt    []Handle
	Level      int
	Attempts   int
	Queued     int
}

// DeviceLostRecovery owns the resource table, the handle generation, the loss
// state and the queued render requests.
type DeviceLostRecovery struct {
	limits Limits
}

// NewDeviceLostRecovery builds a manager with the given caps.
func NewDeviceLostRecovery(limits Limits) *DeviceLostRecovery {
	return &DeviceLostRecovery{limits: limits}
}

// Register adds one resource description to the rebuild table.
func (d *DeviceLostRecovery) Register(spec ResourceSpec) (Handle, error) {
	return 0, errNotImplemented
}

// OnLost marks the device as lost and invalidates every live handle.
func (d *DeviceLostRecovery) OnLost(reason string) error {
	return errNotImplemented
}

// Recreate rebuilds every registered resource and returns a new generation.
func (d *DeviceLostRecovery) Recreate(now time.Time) (RecoveryReport, error) {
	return RecoveryReport{}, errNotImplemented
}

// Resolve reads the state behind a live handle.
func (d *DeviceLostRecovery) Resolve(handle Handle) (ResourceState, error) {
	return ResourceState{}, errNotImplemented
}

// Degrade freezes the quality level used by later rebuilds.
func (d *DeviceLostRecovery) Degrade(level int) error {
	return errNotImplemented
}

// DegradeLevel reports the level currently in force.
func (d *DeviceLostRecovery) DegradeLevel() int {
	return 0
}

// Resources lists the live handles in ascending order.
func (d *DeviceLostRecovery) Resources() []Handle {
	return nil
}

// Status reports StatusReady or StatusLost.
func (d *DeviceLostRecovery) Status() string {
	return ""
}

// Generation reports the current device generation.
func (d *DeviceLostRecovery) Generation() int {
	return 0
}

// QueueRender parks a render request that arrives while recovery is running.
func (d *DeviceLostRecovery) QueueRender(req RenderRequest) error {
	return errNotImplemented
}

// DrainRender releases the parked render requests in arrival order.
func (d *DeviceLostRecovery) DrainRender() []RenderRequest {
	return nil
}

// HistoryValid reports whether temporal history buffers survive the last rebuild.
func (d *DeviceLostRecovery) HistoryValid() bool {
	return false
}

// Snapshot serialises the table, the generation watermark and the level.
func (d *DeviceLostRecovery) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *DeviceLostRecovery) Restore(data []byte) error {
	return errNotImplemented
}
