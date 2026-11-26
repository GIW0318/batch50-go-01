// Package task brokers userspace filesystem mounts inside the sandbox: mount
// admission, cross user access, identity mapping and request backpressure.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Mount lifecycle states.
const (
	MountActive = "active"
)

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

// FuseMountBroker owns the mount table, the identity maps and the queues.
type FuseMountBroker struct {
	limits Limits
}

// NewFuseMountBroker builds a broker with the given caps.
func NewFuseMountBroker(limits Limits) *FuseMountBroker {
	return &FuseMountBroker{limits: limits}
}

// Mount validates the spec and registers one mount.
func (b *FuseMountBroker) Mount(spec MountSpec) (Mount, error) {
	return Mount{}, errNotImplemented
}

// Unmount removes one drained mount.
func (b *FuseMountBroker) Unmount(id string) error {
	return errNotImplemented
}

// Forward admits, maps and dispatches one kernel request.
func (b *FuseMountBroker) Forward(id string, req Request) (Reply, error) {
	return Reply{}, errNotImplemented
}

// SetMapping replaces the identity map of one mount.
func (b *FuseMountBroker) SetMapping(id string, mapping []IDMap) error {
	return errNotImplemented
}

// SetBackend flips the backend connection state of one mount.
func (b *FuseMountBroker) SetBackend(id string, online bool) error {
	return errNotImplemented
}

// Flush drains the queued requests of one mount to its backend.
func (b *FuseMountBroker) Flush(id string) (int, error) {
	return 0, errNotImplemented
}

// Depth reports the current queue depth of one mount.
func (b *FuseMountBroker) Depth(id string) (int, error) {
	return 0, errNotImplemented
}

// Stats reads the counters of one mount.
func (b *FuseMountBroker) Stats(id string) (Stats, error) {
	return Stats{}, errNotImplemented
}

// Mounts lists the live mounts ordered by mountpoint.
func (b *FuseMountBroker) Mounts() []Mount {
	return nil
}

// Snapshot serialises the mounts, the queues and the maps.
func (b *FuseMountBroker) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole broker state with a snapshot, all or nothing.
func (b *FuseMountBroker) Restore(data []byte) error {
	return errNotImplemented
}
