// Package task rebuilds GPU resources after a device loss: it invalidates the
// old handle generation, rebuilds pipelines, textures, buffers and render
// targets in dependency order and freezes a degradation level once quality has
// been traded away.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

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

// kindOrder fixes the rebuild order: pipelines, textures, buffers, targets.
var kindOrder = []string{KindPipeline, KindTexture, KindBuffer, KindTarget}

var knownKinds = map[string]bool{
	KindPipeline: true,
	KindTexture:  true,
	KindBuffer:   true,
	KindTarget:   true,
}

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

// resourceEntry is one registered resource and the handle it currently owns.
type resourceEntry struct {
	spec   ResourceSpec
	handle Handle
}

// DeviceLostRecovery owns the resource table, the handle generation, the loss
// state and the queued render requests.
type DeviceLostRecovery struct {
	mu sync.Mutex

	maxResources  int
	maxAttempts   int
	maxLevel      int
	rebuildBudget time.Duration

	entries    []*resourceEntry
	byName     map[string]*resourceEntry
	byHandle   map[Handle]*resourceEntry
	watermark  int64
	generation int

	lost   bool
	reason string

	attempts     int
	firstAttempt time.Time

	lossCount           int
	degradedThisEpisode bool
	level               int
	historyValid        bool
	queue               []RenderRequest
}

// NewDeviceLostRecovery builds a manager with the given caps.
func NewDeviceLostRecovery(limits Limits) *DeviceLostRecovery {
	maxResources := limits.MaxResources
	if maxResources <= 0 {
		maxResources = DefaultMaxResources
	}
	maxAttempts := limits.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	maxLevel := limits.MaxLevel
	if maxLevel <= 0 {
		maxLevel = DefaultMaxLevel
	}
	return &DeviceLostRecovery{
		maxResources:  maxResources,
		maxAttempts:   maxAttempts,
		maxLevel:      maxLevel,
		rebuildBudget: limits.RebuildBudget,
		byName:        make(map[string]*resourceEntry),
		byHandle:      make(map[Handle]*resourceEntry),
		generation:    1,
		historyValid:  true,
	}
}

// Register adds one resource description to the rebuild table.
func (d *DeviceLostRecovery) Register(spec ResourceSpec) (Handle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if strings.TrimSpace(spec.Name) == "" || !knownKinds[spec.Kind] || spec.Bytes <= 0 {
		return 0, ErrBadResource
	}
	for _, dep := range spec.DependsOn {
		if dep == spec.Name {
			return 0, ErrBadResource
		}
	}
	if d.lost {
		return 0, ErrDeviceLost
	}
	if _, ok := d.byName[spec.Name]; ok {
		return 0, ErrDuplicateResource
	}
	if len(d.entries) >= d.maxResources {
		return 0, ErrResourceLimit
	}
	for _, dep := range spec.DependsOn {
		if _, ok := d.byName[dep]; ok {
			continue
		}
		// A dependency on a name that sorts later is a forward reference:
		// it lets mutually dependent resources register in name order and
		// any cycle is judged at rebuild time. A reference to an earlier
		// name that was never registered is simply unknown.
		if dep <= spec.Name {
			return 0, ErrMissingDependency
		}
	}

	d.watermark++
	entry := &resourceEntry{spec: spec, handle: Handle(d.watermark)}
	d.entries = append(d.entries, entry)
	d.byName[spec.Name] = entry
	d.byHandle[entry.handle] = entry
	return entry.handle, nil
}

// OnLost marks the device as lost and invalidates every live handle.
func (d *DeviceLostRecovery) OnLost(reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if strings.TrimSpace(reason) == "" {
		return ErrInvalidReason
	}
	if d.lost {
		return ErrAlreadyLost
	}
	d.lost = true
	d.reason = reason
	d.attempts = 0
	d.firstAttempt = time.Time{}
	d.lossCount++
	d.degradedThisEpisode = false
	return nil
}

// Recreate rebuilds every registered resource and returns a new generation.
func (d *DeviceLostRecovery) Recreate(now time.Time) (RecoveryReport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.lost {
		return RecoveryReport{}, ErrNotLost
	}
	if d.attempts >= d.maxAttempts {
		return RecoveryReport{}, ErrRecoveryBudget
	}
	if d.rebuildBudget > 0 && d.attempts > 0 && now.Sub(d.firstAttempt) > d.rebuildBudget {
		return RecoveryReport{}, ErrRecoveryBudget
	}
	d.attempts++
	if d.attempts == 1 {
		d.firstAttempt = now
	}

	// A repeated loss within the same run rebuilds one quality level lower;
	// the level never climbs back on its own.
	if d.lossCount >= 2 && !d.degradedThisEpisode {
		if d.level < d.maxLevel {
			d.level++
		}
		d.degradedThisEpisode = true
	}

	if d.hasDependencyCycle() {
		return RecoveryReport{}, ErrDependencyCycle
	}

	d.generation++
	d.byHandle = make(map[Handle]*resourceEntry, len(d.entries))
	rebuilt := make([]Handle, 0, len(d.entries))
	for _, kind := range kindOrder {
		for _, entry := range d.entries {
			if entry.spec.Kind != kind {
				continue
			}
			d.watermark++
			entry.handle = Handle(d.watermark)
			d.byHandle[entry.handle] = entry
			rebuilt = append(rebuilt, entry.handle)
		}
	}

	report := RecoveryReport{
		Reason:     d.reason,
		Generation: d.generation,
		Rebuilt:    rebuilt,
		Level:      d.level,
		Attempts:   d.attempts,
		Queued:     len(d.queue),
	}
	d.lost = false
	d.reason = ""
	// TAA and post-processing history buffers belong to the dead generation.
	d.historyValid = false
	return report, nil
}

// hasDependencyCycle reports whether the declared dependencies form a cycle.
func (d *DeviceLostRecovery) hasDependencyCycle() bool {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(d.entries))
	var visit func(name string) bool
	visit = func(name string) bool {
		color[name] = gray
		entry := d.byName[name]
		for _, dep := range entry.spec.DependsOn {
			switch color[dep] {
			case gray:
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		color[name] = black
		return false
	}
	for _, entry := range d.entries {
		if color[entry.spec.Name] == white && visit(entry.spec.Name) {
			return true
		}
	}
	return false
}

// Resolve reads the state behind a live handle.
func (d *DeviceLostRecovery) Resolve(handle Handle) (ResourceState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if handle <= 0 || int64(handle) > d.watermark {
		return ResourceState{}, ErrUnknownHandle
	}
	if d.lost {
		return ResourceState{}, ErrStaleHandle
	}
	entry, ok := d.byHandle[handle]
	if !ok {
		return ResourceState{}, ErrStaleHandle
	}
	return ResourceState{
		Handle:     handle,
		Name:       entry.spec.Name,
		Kind:       entry.spec.Kind,
		Bytes:      entry.spec.Bytes,
		Valid:      true,
		Generation: d.generation,
		Degraded:   d.level > 0,
	}, nil
}

// Degrade freezes the quality level used by later rebuilds.
func (d *DeviceLostRecovery) Degrade(level int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if level < 0 || level > d.maxLevel {
		return ErrBadDegradeLevel
	}
	if level == d.level {
		return nil
	}
	if level < d.level {
		return ErrDegradeLocked
	}
	d.level = level
	return nil
}

// DegradeLevel reports the level currently in force.
func (d *DeviceLostRecovery) DegradeLevel() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.level
}

// Resources lists the live handles in ascending order.
func (d *DeviceLostRecovery) Resources() []Handle {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.lost {
		return nil
	}
	handles := make([]Handle, 0, len(d.byHandle))
	for handle := range d.byHandle {
		handles = append(handles, handle)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	return handles
}

// Status reports StatusReady or StatusLost.
func (d *DeviceLostRecovery) Status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lost {
		return StatusLost
	}
	return StatusReady
}

// Generation reports the current device generation.
func (d *DeviceLostRecovery) Generation() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.generation
}

// QueueRender parks a render request that arrives while recovery is running.
func (d *DeviceLostRecovery) QueueRender(req RenderRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queue = append(d.queue, req)
	return nil
}

// DrainRender releases the parked render requests in arrival order.
func (d *DeviceLostRecovery) DrainRender() []RenderRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 {
		return nil
	}
	out := make([]RenderRequest, len(d.queue))
	copy(out, d.queue)
	d.queue = nil
	return out
}

// HistoryValid reports whether the TAA and post-processing history buffers
// still match the current generation.
func (d *DeviceLostRecovery) HistoryValid() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.historyValid
}

// snapshotResource is the serialized form of one registered resource.
type snapshotResource struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Bytes     int      `json:"bytes"`
	DependsOn []string `json:"depends_on"`
	Handle    int64    `json:"handle"`
}

// snapshotState is the serialized form of the whole manager.
type snapshotState struct {
	Version      int                `json:"version"`
	Generation   int                `json:"generation"`
	Watermark    int64              `json:"watermark"`
	Level        int                `json:"level"`
	Lost         bool               `json:"lost"`
	Reason       string             `json:"reason"`
	Attempts     int                `json:"attempts"`
	FirstAttempt time.Time          `json:"first_attempt"`
	LossCount    int                `json:"loss_count"`
	Degraded     bool               `json:"degraded_this_episode"`
	HistoryValid bool               `json:"history_valid"`
	Resources    []snapshotResource `json:"resources"`
	Queue        []RenderRequest    `json:"queue"`
}

const snapshotVersion = 1

// snapshotEnvelope wraps the serialized state with an integrity checksum so a
// corrupted payload is rejected as a whole instead of being applied halfway.
type snapshotEnvelope struct {
	Payload json.RawMessage `json:"payload"`
	Sum     string          `json:"sum"`
}

// Snapshot serializes the resource table, the dependencies, the handle
// watermark, the generation and the degradation level.
func (d *DeviceLostRecovery) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := snapshotState{
		Version:      snapshotVersion,
		Generation:   d.generation,
		Watermark:    d.watermark,
		Level:        d.level,
		Lost:         d.lost,
		Reason:       d.reason,
		Attempts:     d.attempts,
		FirstAttempt: d.firstAttempt,
		LossCount:    d.lossCount,
		Degraded:     d.degradedThisEpisode,
		HistoryValid: d.historyValid,
		Resources:    make([]snapshotResource, 0, len(d.entries)),
		Queue:        append([]RenderRequest(nil), d.queue...),
	}
	for _, entry := range d.entries {
		state.Resources = append(state.Resources, snapshotResource{
			Name:      entry.spec.Name,
			Kind:      entry.spec.Kind,
			Bytes:     entry.spec.Bytes,
			DependsOn: append([]string(nil), entry.spec.DependsOn...),
			Handle:    int64(entry.handle),
		})
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	return json.Marshal(snapshotEnvelope{
		Payload: payload,
		Sum:     hex.EncodeToString(sum[:]),
	})
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *DeviceLostRecovery) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Payload)
	if len(envelope.Payload) == 0 || hex.EncodeToString(sum[:]) != envelope.Sum {
		return ErrBadSnapshot
	}
	var state snapshotState
	if err := json.Unmarshal(envelope.Payload, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.Version != snapshotVersion || state.Generation < 1 || state.Watermark < 0 {
		return ErrBadSnapshot
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if state.Level < 0 || state.Level > d.maxLevel {
		return ErrBadSnapshot
	}
	entries := make([]*resourceEntry, 0, len(state.Resources))
	byName := make(map[string]*resourceEntry, len(state.Resources))
	byHandle := make(map[Handle]*resourceEntry, len(state.Resources))
	for _, res := range state.Resources {
		if strings.TrimSpace(res.Name) == "" || !knownKinds[res.Kind] || res.Bytes <= 0 {
			return ErrBadSnapshot
		}
		if res.Handle <= 0 || res.Handle > state.Watermark {
			return ErrBadSnapshot
		}
		if _, ok := byName[res.Name]; ok {
			return ErrBadSnapshot
		}
		handle := Handle(res.Handle)
		if _, ok := byHandle[handle]; ok {
			return ErrBadSnapshot
		}
		entry := &resourceEntry{
			spec: ResourceSpec{
				Name:      res.Name,
				Kind:      res.Kind,
				Bytes:     res.Bytes,
				DependsOn: append([]string(nil), res.DependsOn...),
			},
			handle: handle,
		}
		entries = append(entries, entry)
		byName[res.Name] = entry
		byHandle[handle] = entry
	}
	for _, entry := range entries {
		for _, dep := range entry.spec.DependsOn {
			if dep == entry.spec.Name {
				return ErrBadSnapshot
			}
			if _, ok := byName[dep]; !ok {
				return ErrBadSnapshot
			}
		}
	}

	d.entries = entries
	d.byName = byName
	d.byHandle = byHandle
	d.watermark = state.Watermark
	d.generation = state.Generation
	d.level = state.Level
	d.lost = state.Lost
	d.reason = state.Reason
	d.attempts = state.Attempts
	d.firstAttempt = state.FirstAttempt
	d.lossCount = state.LossCount
	d.degradedThisEpisode = state.Degraded
	d.historyValid = state.HistoryValid
	d.queue = append([]RenderRequest(nil), state.Queue...)
	return nil
}
