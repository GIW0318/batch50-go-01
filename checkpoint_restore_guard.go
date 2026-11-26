// Package task exports, inspects, restores and destroys sandbox job
// checkpoints while keeping credentials out of the image and keeping a
// restore from ever producing two running instances.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks every behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Snapshot states reported through Manifest.State.
const (
	SnapshotSealed  = "sealed"
	SnapshotVoid    = "void"
	SnapshotExpired = "expired"
)

// Audit operations.
const (
	OpStartJob   = "start_job"
	OpCheckpoint = "checkpoint"
	OpInspect    = "inspect"
	OpRestore    = "restore"
	OpDestroy    = "destroy"
	OpExpire     = "expire"
)

// DefaultRedactMarkers are the substrings that make an environment key
// sensitive even when the caller does not list it explicitly.
var DefaultRedactMarkers = []string{"PASSWORD", "SECRET", "TOKEN", "CREDENTIAL", "PRIVATE_KEY"}

// JobID, SnapshotID and NodeID keep the three namespaces apart.
type (
	JobID      string
	SnapshotID string
	NodeID     string
)

// Errors returned by the guard.
var (
	ErrBadJob             = errors.New("job specification is not acceptable")
	ErrNoJob              = errors.New("job not found")
	ErrNoSnapshot         = errors.New("snapshot not found")
	ErrSnapshotLimit      = errors.New("per tenant snapshot limit reached")
	ErrCapacityExceeded   = errors.New("tenant snapshot capacity exceeded")
	ErrOversizeSnapshot   = errors.New("snapshot exceeds the requested size cap")
	ErrCrossTenant        = errors.New("target node belongs to another tenant")
	ErrUnknownNode        = errors.New("target node is not registered")
	ErrIncompatibleNode   = errors.New("target node cannot host this snapshot")
	ErrRedactionFailed    = errors.New("credential redaction failed")
	ErrSnapshotVoid       = errors.New("snapshot was voided and cannot be used")
	ErrBadEnvelope        = errors.New("snapshot envelope payload is invalid")
	ErrDuplicateRestore   = errors.New("snapshot restore already in flight")
	ErrSnapshotDestroyed  = errors.New("snapshot has been destroyed")
	ErrMountIdentityDrift = errors.New("mounted content no longer matches the manifest")
)

// NamespaceSpec is the namespace layout recorded in the manifest.
type NamespaceSpec struct {
	PidNS    string `json:"pid_ns"`
	NetNS    string `json:"net_ns"`
	MountNS  string `json:"mount_ns"`
	UserNS   string `json:"user_ns"`
	CgroupNS string `json:"cgroup_ns"`
}

// MountEntry is one bind mount captured by the checkpoint.
type MountEntry struct {
	Target   string `json:"target"`
	Source   string `json:"source"`
	FSType   string `json:"fs_type"`
	ReadOnly bool   `json:"read_only"`
	Digest   string `json:"digest"`
}

// OpenFile is one file descriptor the job held open at export time.
type OpenFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// JobSpec is everything the guard needs to know about a running job.
type JobSpec struct {
	Tenant      string        `json:"tenant"`
	Node        NodeID        `json:"node"`
	Env         []string      `json:"env"`
	Mounts      []MountEntry  `json:"mounts"`
	Namespaces  NamespaceSpec `json:"namespaces"`
	NetIdentity string        `json:"net_identity"`
	DiskBytes   int64         `json:"disk_bytes"`
	OpenFiles   []OpenFile    `json:"open_files"`
}

// Options tunes one export.
type Options struct {
	Redact   []string
	Freeze   bool
	MaxBytes int64
	Reason   string
}

// Manifest is what Inspect returns and what a restore is validated against.
type Manifest struct {
	ID            SnapshotID    `json:"id"`
	Tenant        string        `json:"tenant"`
	Source        NodeID        `json:"source"`
	State         string        `json:"state"`
	Created       time.Time     `json:"created"`
	Bytes         int64         `json:"bytes"`
	Digest        string        `json:"digest"`
	Redacted      []string      `json:"redacted"`
	Namespaces    NamespaceSpec `json:"namespaces"`
	Mounts        []MountEntry  `json:"mounts"`
	NetIdentity   string        `json:"net_identity"`
	KeyGeneration int           `json:"key_generation"`
	OpenFiles     []OpenFile    `json:"open_files"`
}

// NodeCaps describes one restore target.
type NodeCaps struct {
	Tenant  string `json:"tenant"`
	Kernel  string `json:"kernel"`
	Runtime string `json:"runtime"`
	Arch    string `json:"arch"`
	Seccomp bool   `json:"seccomp"`
}

// Config caps the snapshot store.
type Config struct {
	MaxSnapshotsPerTenant   int
	MaxTotalBytes           int64
	SnapshotTTL             time.Duration
	EncryptionKeyGeneration int
}

// Usage is the per tenant store accounting.
type Usage struct {
	Snapshots int   `json:"snapshots"`
	Bytes     int64 `json:"bytes"`
}

// AuditEntry is one line of the append only operation log.
type AuditEntry struct {
	Seq      int64      `json:"seq"`
	Op       string     `json:"op"`
	Snapshot SnapshotID `json:"snapshot"`
	Job      JobID      `json:"job"`
	Detail   string     `json:"detail"`
	At       time.Time  `json:"at"`
}

// CheckpointRestoreGuard owns the job table, the snapshot store, the node
// capability registry and the audit trail.
type CheckpointRestoreGuard struct {
	cfg Config
}

// NewCheckpointRestoreGuard builds a guard with the given caps.
func NewCheckpointRestoreGuard(cfg Config) *CheckpointRestoreGuard {
	return &CheckpointRestoreGuard{cfg: cfg}
}

// StartJob registers a running job so it can be checkpointed.
func (g *CheckpointRestoreGuard) StartJob(id JobID, spec JobSpec) error {
	return errNotImplemented
}

// RegisterNode records the capabilities and tenant of a restore target.
func (g *CheckpointRestoreGuard) RegisterNode(id NodeID, caps NodeCaps) error {
	return errNotImplemented
}

// Checkpoint exports one job into a sealed snapshot.
func (g *CheckpointRestoreGuard) Checkpoint(jobID JobID, opts Options) (SnapshotID, error) {
	return "", errNotImplemented
}

// Inspect returns the manifest of one snapshot.
func (g *CheckpointRestoreGuard) Inspect(id SnapshotID) (Manifest, error) {
	return Manifest{}, errNotImplemented
}

// Restore starts the snapshot on a target node and returns the new job id.
func (g *CheckpointRestoreGuard) Restore(id SnapshotID, target NodeID) (JobID, error) {
	return "", errNotImplemented
}

// Destroy drops a snapshot and frees its quota.
func (g *CheckpointRestoreGuard) Destroy(id SnapshotID) error {
	return errNotImplemented
}

// Expire destroys snapshots whose TTL passed and returns how many went away.
func (g *CheckpointRestoreGuard) Expire(now time.Time) int {
	return 0
}

// Snapshots lists the live snapshot ids of one tenant, oldest first.
func (g *CheckpointRestoreGuard) Snapshots(tenant string) []SnapshotID {
	return nil
}

// Instances reports how many running instances one job id has.
func (g *CheckpointRestoreGuard) Instances(jobID JobID) int {
	return 0
}

// InstanceCount reports the total number of running instances.
func (g *CheckpointRestoreGuard) InstanceCount() int {
	return 0
}

// Usage reports the store accounting of one tenant.
func (g *CheckpointRestoreGuard) Usage(tenant string) Usage {
	return Usage{}
}

// Audit returns the operation log, oldest first.
func (g *CheckpointRestoreGuard) Audit() []AuditEntry {
	return nil
}

// Export serialises jobs, snapshots, nodes and the audit counter.
func (g *CheckpointRestoreGuard) Export() ([]byte, error) {
	return nil, errNotImplemented
}

// Import replaces the whole state with an exported payload, all or nothing.
func (g *CheckpointRestoreGuard) Import(data []byte) error {
	return errNotImplemented
}
