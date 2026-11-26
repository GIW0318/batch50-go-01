// Package task brokers userspace filesystem mounts inside the sandbox: mount
// admission, cross user access, identity mapping and request backpressure.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
)

// Mount lifecycle states.
const (
	MountActive = "active"
)

const snapshotVersion = 1

// Errors returned by the broker.
var (
	ErrBadMountpoint = errors.New("mountpoint is invalid")
	ErrBadPerm       = errors.New("default permission is invalid")
	ErrBadOwner      = errors.New("owner or downgrade identity is invalid")
	ErrMountExists   = errors.New("mountpoint is already mounted")
	ErrMountOverlap  = errors.New("mountpoint is shadowed by another mount")
	ErrMountLimit    = errors.New("mount limit reached")
	ErrUnknownMount  = errors.New("mount not found")
	ErrAccessDenied  = errors.New("caller is not allowed to use this mount")
	ErrBadMapping    = errors.New("identity mapping is invalid")
	ErrBadRequest    = errors.New("forwarded request is invalid")
	ErrQueueFull     = errors.New("request queue is full")
	ErrQueueBusy     = errors.New("mount still has queued requests")
	ErrBackendDown   = errors.New("backend is disconnected")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Limits bounds the broker tables.
type Limits struct {
	MaxMounts     int
	MaxQueueDepth int
	MaxMapping    int
}

// MountSpec is one mount request.
type MountSpec struct {
	Mountpoint  string `json:"mountpoint"`
	OwnerUID    int    `json:"owner_uid"`
	AllowOther  bool   `json:"allow_other"`
	Perm        string `json:"perm"`
	Backend     string `json:"backend"`
	DowngradeTo int    `json:"downgrade_to"`
}

// Mount is the live record of one mount.
type Mount struct {
	ID         string `json:"id"`
	Mountpoint string `json:"mountpoint"`
	OwnerUID   int    `json:"owner_uid"`
	AllowOther bool   `json:"allow_other"`
	Perm       string `json:"perm"`
	Backend    string `json:"backend"`
	State      string `json:"state"`
	Queued     int    `json:"queued"`
	Gen        int64  `json:"gen"`
}

// IDMap is one host to container identity translation.
type IDMap struct {
	HostID      int `json:"host_id"`
	ContainerID int `json:"container_id"`
}

// Request is one filesystem operation forwarded by the kernel.
type Request struct {
	ID         string `json:"id"`
	CallerUID  int    `json:"caller_uid"`
	Path       string `json:"path"`
	Op         string `json:"op"`
	Idempotent bool   `json:"idempotent"`
}

// Reply is the outcome of one forwarded request.
type Reply struct {
	Code      int    `json:"code"`
	MappedUID int    `json:"mapped_uid"`
	Queued    bool   `json:"queued"`
	Retry     bool   `json:"retry"`
	Reason    string `json:"reason"`
}

// Stats counts the traffic of one mount.
type Stats struct {
	Requests int   `json:"requests"`
	Denied   int   `json:"denied"`
	Queued   int   `json:"queued"`
	Dropped  int   `json:"dropped"`
	Gen      int64 `json:"gen"`
}

type queuedRequest struct {
	Req       Request
	MappedUID int
}

type mountRecord struct {
	Mount       Mount
	DowngradeTo int
	Mapping     map[int]int
	Queue       []queuedRequest
	Online      bool
	Stats       Stats
}

// FuseMountBroker owns the mount table, the identity maps and the queues.
type FuseMountBroker struct {
	mu        sync.Mutex
	limits    Limits
	mounts    map[string]*mountRecord
	pointIDs  map[string]string
	nextMount int64
	nextGen   int64
}

// NewFuseMountBroker builds a broker with the given caps.
func NewFuseMountBroker(limits Limits) *FuseMountBroker {
	return &FuseMountBroker{
		limits:   limits,
		mounts:   make(map[string]*mountRecord),
		pointIDs: make(map[string]string),
	}
}

// Mount validates the spec and registers one mount.
func (b *FuseMountBroker) Mount(spec MountSpec) (Mount, error) {
	if !validMountpoint(spec.Mountpoint) {
		return Mount{}, ErrBadMountpoint
	}
	if spec.Perm != "ro" && spec.Perm != "rw" {
		return Mount{}, ErrBadPerm
	}
	if spec.OwnerUID < 0 || spec.DowngradeTo < 0 {
		return Mount{}, ErrBadOwner
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.pointIDs[spec.Mountpoint]; ok {
		return Mount{}, ErrMountExists
	}
	for _, rec := range b.mounts {
		if mountpointsConflict(spec.Mountpoint, rec.Mount.Mountpoint) {
			return Mount{}, ErrMountOverlap
		}
	}
	if limitReached(len(b.mounts), b.limits.MaxMounts) {
		return Mount{}, ErrMountLimit
	}

	id := b.newMountIDLocked()
	b.nextGen++
	if b.nextGen <= 0 {
		b.nextGen = 1
	}
	gen := b.nextGen
	rec := &mountRecord{
		Mount: Mount{
			ID:         id,
			Mountpoint: spec.Mountpoint,
			OwnerUID:   spec.OwnerUID,
			AllowOther: spec.AllowOther,
			Perm:       spec.Perm,
			Backend:    spec.Backend,
			State:      MountActive,
			Gen:        gen,
		},
		DowngradeTo: spec.DowngradeTo,
		Mapping:     make(map[int]int),
		Queue:       make([]queuedRequest, 0),
		Online:      true,
		Stats:       Stats{Gen: gen},
	}
	b.mounts[id] = rec
	b.pointIDs[spec.Mountpoint] = id
	return rec.publicMount(), nil
}

// Unmount removes one drained mount.
func (b *FuseMountBroker) Unmount(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return ErrUnknownMount
	}
	if len(rec.Queue) != 0 {
		return ErrQueueBusy
	}
	delete(b.mounts, id)
	delete(b.pointIDs, rec.Mount.Mountpoint)
	return nil
}

// Forward admits, maps and dispatches one kernel request.
func (b *FuseMountBroker) Forward(id string, req Request) (Reply, error) {
	if req.ID == "" || req.Path == "" {
		return Reply{}, ErrBadRequest
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return Reply{}, ErrUnknownMount
	}

	mapped, allowed := authorize(rec, req)
	if !allowed {
		rec.Stats.Denied++
		return Reply{Code: 403, Reason: "access denied"}, ErrAccessDenied
	}

	if !rec.Online {
		if limitReached(len(rec.Queue), b.limits.MaxQueueDepth) {
			rec.Stats.Dropped++
			return Reply{
				Code:      503,
				MappedUID: mapped,
				Retry:     true,
				Reason:    "queue full",
			}, ErrQueueFull
		}
		rec.Queue = append(rec.Queue, queuedRequest{Req: req, MappedUID: mapped})
		rec.Stats.Requests++
		rec.syncQueuedCounts()
		return Reply{
			Code:      202,
			MappedUID: mapped,
			Queued:    true,
			Retry:     true,
			Reason:    "backend offline",
		}, nil
	}

	rec.Stats.Requests++
	return Reply{Code: 200, MappedUID: mapped}, nil
}

// SetMapping replaces the identity map of one mount.
func (b *FuseMountBroker) SetMapping(id string, mapping []IDMap) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return ErrUnknownMount
	}
	translated, err := buildMapping(mapping, b.limits.MaxMapping)
	if err != nil {
		return err
	}
	rec.Mapping = translated
	return nil
}

// SetBackend flips the backend connection state of one mount.
func (b *FuseMountBroker) SetBackend(id string, online bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return ErrUnknownMount
	}
	rec.Online = online
	return nil
}

// Flush drains the queued requests of one mount to its backend.
func (b *FuseMountBroker) Flush(id string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return 0, ErrUnknownMount
	}
	if !rec.Online {
		return 0, ErrBackendDown
	}

	drained := 0
	for _, queued := range rec.Queue {
		if queued.Req.Idempotent {
			drained++
			continue
		}
		// A reconnect cannot safely replay an unconfirmed mutation.
		rec.Stats.Dropped++
	}
	rec.Queue = make([]queuedRequest, 0)
	rec.syncQueuedCounts()
	return drained, nil
}

// Depth reports the current queue depth of one mount.
func (b *FuseMountBroker) Depth(id string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return 0, ErrUnknownMount
	}
	return len(rec.Queue), nil
}

// Stats reads the counters of one mount.
func (b *FuseMountBroker) Stats(id string) (Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	rec, ok := b.mounts[id]
	if !ok {
		return Stats{}, ErrUnknownMount
	}
	rec.syncQueuedCounts()
	return rec.Stats, nil
}

// Mounts lists the live mounts ordered by mountpoint.
func (b *FuseMountBroker) Mounts() []Mount {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Mount, 0, len(b.mounts))
	for _, rec := range b.mounts {
		out = append(out, rec.publicMount())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Mountpoint < out[j].Mountpoint
	})
	return out
}

type snapshotQueued struct {
	ID         string `json:"id"`
	CallerUID  int    `json:"caller_uid"`
	Path       string `json:"path"`
	Op         string `json:"op"`
	Idempotent bool   `json:"idempotent"`
	MappedUID  int    `json:"mapped_uid"`
}

type snapshotMount struct {
	ID          string           `json:"id"`
	Mountpoint  string           `json:"mountpoint"`
	OwnerUID    int              `json:"owner_uid"`
	AllowOther  bool             `json:"allow_other"`
	Perm        string           `json:"perm"`
	Backend     string           `json:"backend"`
	DowngradeTo int              `json:"downgrade_to"`
	Gen         int64            `json:"gen"`
	Mapping     []IDMap          `json:"mapping"`
	Online      bool             `json:"online"`
	Queue       []snapshotQueued `json:"queue"`
	Stats       Stats            `json:"stats"`
}

type snapshotState struct {
	Version int             `json:"version"`
	NextID  int64           `json:"next_id"`
	NextGen int64           `json:"next_gen"`
	Mounts  []snapshotMount `json:"mounts"`
}

// Snapshot serialises the mounts, the queues and the maps.
func (b *FuseMountBroker) Snapshot() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := snapshotState{
		Version: snapshotVersion,
		NextID:  b.nextMount,
		NextGen: b.nextGen,
		Mounts:  make([]snapshotMount, 0, len(b.mounts)),
	}
	for _, rec := range b.mounts {
		rec.syncQueuedCounts()
		queue := make([]snapshotQueued, 0, len(rec.Queue))
		for _, queued := range rec.Queue {
			queue = append(queue, snapshotQueued{
				ID:         queued.Req.ID,
				CallerUID:  queued.Req.CallerUID,
				Path:       queued.Req.Path,
				Op:         queued.Req.Op,
				Idempotent: queued.Req.Idempotent,
				MappedUID:  queued.MappedUID,
			})
		}
		snapshot.Mounts = append(snapshot.Mounts, snapshotMount{
			ID:          rec.Mount.ID,
			Mountpoint:  rec.Mount.Mountpoint,
			OwnerUID:    rec.Mount.OwnerUID,
			AllowOther:  rec.Mount.AllowOther,
			Perm:        rec.Mount.Perm,
			Backend:     rec.Mount.Backend,
			DowngradeTo: rec.DowngradeTo,
			Gen:         rec.Mount.Gen,
			Mapping:     mappingSlice(rec.Mapping),
			Online:      rec.Online,
			Queue:       queue,
			Stats:       rec.Stats,
		})
	}
	sort.Slice(snapshot.Mounts, func(i, j int) bool {
		return snapshot.Mounts[i].Mountpoint < snapshot.Mounts[j].Mountpoint
	})
	return json.Marshal(snapshot)
}

// Restore replaces the whole broker state with a snapshot, all or nothing.
func (b *FuseMountBroker) Restore(data []byte) error {
	var snapshot snapshotState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ErrBadSnapshot
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrBadSnapshot
	}

	if snapshot.Version != snapshotVersion || snapshot.NextID < 0 || snapshot.NextGen < 0 || snapshot.Mounts == nil {
		return ErrBadSnapshot
	}
	if limitExceeded(len(snapshot.Mounts), b.limits.MaxMounts) {
		return ErrBadSnapshot
	}

	stagedMounts := make(map[string]*mountRecord, len(snapshot.Mounts))
	stagedPoints := make(map[string]string, len(snapshot.Mounts))
	var maxGen int64
	for _, stored := range snapshot.Mounts {
		if stored.ID == "" || !validMountpoint(stored.Mountpoint) || !validPerm(stored.Perm) ||
			stored.OwnerUID < 0 || stored.DowngradeTo < 0 || stored.Gen <= 0 ||
			stored.Mapping == nil || stored.Queue == nil ||
			stored.Stats.Requests < 0 || stored.Stats.Denied < 0 ||
			stored.Stats.Dropped < 0 || stored.Stats.Queued < 0 ||
			stored.Stats.Gen != stored.Gen || stored.Stats.Queued != len(stored.Queue) ||
			stored.Stats.Requests < stored.Stats.Queued {
			return ErrBadSnapshot
		}
		if _, exists := stagedMounts[stored.ID]; exists {
			return ErrBadSnapshot
		}
		if _, exists := stagedPoints[stored.Mountpoint]; exists {
			return ErrBadSnapshot
		}
		for _, rec := range stagedMounts {
			if mountpointsConflict(stored.Mountpoint, rec.Mount.Mountpoint) {
				return ErrBadSnapshot
			}
		}
		mapping, err := buildMapping(stored.Mapping, b.limits.MaxMapping)
		if err != nil {
			return ErrBadSnapshot
		}
		if limitExceeded(len(stored.Queue), b.limits.MaxQueueDepth) {
			return ErrBadSnapshot
		}
		queue := make([]queuedRequest, 0, len(stored.Queue))
		for _, queued := range stored.Queue {
			if queued.ID == "" || queued.Path == "" || queued.MappedUID < 0 {
				return ErrBadSnapshot
			}
			queue = append(queue, queuedRequest{
				Req: Request{
					ID:         queued.ID,
					CallerUID:  queued.CallerUID,
					Path:       queued.Path,
					Op:         queued.Op,
					Idempotent: queued.Idempotent,
				},
				MappedUID: queued.MappedUID,
			})
		}
		stats := stored.Stats
		stats.Queued = len(queue)
		stats.Gen = stored.Gen
		rec := &mountRecord{
			Mount: Mount{
				ID:         stored.ID,
				Mountpoint: stored.Mountpoint,
				OwnerUID:   stored.OwnerUID,
				AllowOther: stored.AllowOther,
				Perm:       stored.Perm,
				Backend:    stored.Backend,
				State:      MountActive,
				Queued:     len(queue),
				Gen:        stored.Gen,
			},
			DowngradeTo: stored.DowngradeTo,
			Mapping:     mapping,
			Queue:       queue,
			Online:      stored.Online,
			Stats:       stats,
		}
		stagedMounts[stored.ID] = rec
		stagedPoints[stored.Mountpoint] = stored.ID
		if stored.Gen > maxGen {
			maxGen = stored.Gen
		}
	}
	if snapshot.NextGen < maxGen {
		return ErrBadSnapshot
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.mounts = stagedMounts
	b.pointIDs = stagedPoints
	b.nextMount = snapshot.NextID
	b.nextGen = snapshot.NextGen
	return nil
}

func validMountpoint(point string) bool {
	if point == "" || point == "/" {
		return false
	}
	return path.IsAbs(point) && path.Clean(point) == point
}

func validPerm(perm string) bool {
	return perm == "ro" || perm == "rw"
}

func mountpointsConflict(a, b string) bool {
	if a == b {
		return true
	}
	return isBelow(a, b) || isBelow(b, a)
}

func isBelow(child, parent string) bool {
	return len(child) > len(parent) && strings.HasPrefix(child, parent) && child[len(parent)] == '/'
}

func limitReached(current, limit int) bool {
	if limit < 0 {
		limit = 0
	}
	return current >= limit
}

func limitExceeded(current, limit int) bool {
	if limit < 0 {
		limit = 0
	}
	return current > limit
}

func buildMapping(mapping []IDMap, limit int) (map[int]int, error) {
	if limit < 0 {
		limit = 0
	}
	if len(mapping) > limit {
		return nil, ErrBadMapping
	}
	hosts := make(map[int]struct{}, len(mapping))
	containers := make(map[int]struct{}, len(mapping))
	out := make(map[int]int, len(mapping))
	for _, item := range mapping {
		if item.HostID < 0 || item.ContainerID < 0 {
			return nil, ErrBadMapping
		}
		if _, exists := hosts[item.HostID]; exists {
			return nil, ErrBadMapping
		}
		if _, exists := containers[item.ContainerID]; exists {
			return nil, ErrBadMapping
		}
		hosts[item.HostID] = struct{}{}
		containers[item.ContainerID] = struct{}{}
		out[item.HostID] = item.ContainerID
	}
	return out, nil
}

func mappingSlice(mapping map[int]int) []IDMap {
	out := make([]IDMap, 0, len(mapping))
	for host, container := range mapping {
		out = append(out, IDMap{HostID: host, ContainerID: container})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].HostID < out[j].HostID
	})
	return out
}

func authorize(rec *mountRecord, req Request) (int, bool) {
	if req.CallerUID == rec.Mount.OwnerUID {
		return 0, true
	}
	if !rec.Mount.AllowOther {
		return 0, false
	}
	mapped, ok := rec.Mapping[req.CallerUID]
	if !ok {
		return 0, false
	}
	if req.CallerUID == 0 {
		mapped = rec.DowngradeTo
	}
	return mapped, true
}

func (b *FuseMountBroker) newMountIDLocked() string {
	for {
		b.nextMount++
		id := fmt.Sprintf("mnt-%d", b.nextMount)
		if _, exists := b.mounts[id]; !exists {
			return id
		}
	}
}

func (rec *mountRecord) publicMount() Mount {
	rec.syncQueuedCounts()
	return rec.Mount
}

func (rec *mountRecord) syncQueuedCounts() {
	n := len(rec.Queue)
	rec.Mount.Queued = n
	rec.Stats.Queued = n
}
