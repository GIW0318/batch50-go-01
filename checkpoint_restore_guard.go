// Package task exports, inspects, restores and destroys sandbox job
// checkpoints while keeping credentials out of the image and keeping a
// restore from ever producing two running instances.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

type jobRecord struct {
	ID            JobID      `json:"id"`
	Spec          JobSpec    `json:"spec"`
	Instances     int        `json:"instances"`
	RestoredFrom  SnapshotID `json:"restored_from,omitempty"`
	RestoreTarget NodeID     `json:"restore_target,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type snapshotRecord struct {
	Seq           int64     `json:"seq"`
	Manifest      Manifest  `json:"manifest"`
	SafeEnv       []string  `json:"safe_env,omitempty"`
	SourceCaps    *NodeCaps `json:"source_caps,omitempty"`
	RestoredJob   JobID     `json:"restored_job,omitempty"`
	RestoreTarget NodeID    `json:"restore_target,omitempty"`
	RestoreCount  int       `json:"restore_count"`
}

type destroyedTombstone struct {
	At time.Time `json:"at"`
}

type persistedState struct {
	Config      Config                            `json:"config"`
	Seq         int64                             `json:"audit_seq"`
	SnapshotSeq int64                             `json:"snapshot_seq"`
	JobSeq      int64                             `json:"job_seq"`
	Jobs        map[JobID]*jobRecord              `json:"jobs"`
	Nodes       map[NodeID]NodeCaps               `json:"nodes"`
	Snapshots   map[SnapshotID]*snapshotRecord    `json:"snapshots"`
	Destroyed   map[SnapshotID]destroyedTombstone `json:"destroyed"`
	Audit       []AuditEntry                      `json:"audit"`
}

type envelope struct {
	Format   string          `json:"format"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

const envelopeFormat = "batch50-checkpoint-restore-guard/v1"

var (
	versionNumberRE = regexp.MustCompile(`\d+(?:\.\d+)*`)
	runtimeFamilyRE = regexp.MustCompile(`^[A-Za-z]+`)
)

// CheckpointRestoreGuard owns the job table, the snapshot store, the node
// capability registry and the audit trail.
type CheckpointRestoreGuard struct {
	mu          sync.RWMutex
	cfg         Config
	seq         int64
	snapshotSeq int64
	jobSeq      int64
	jobs        map[JobID]*jobRecord
	nodes       map[NodeID]NodeCaps
	snapshots   map[SnapshotID]*snapshotRecord
	destroyed   map[SnapshotID]destroyedTombstone
	audit       []AuditEntry
}

// NewCheckpointRestoreGuard builds a guard with the given caps.
func NewCheckpointRestoreGuard(cfg Config) *CheckpointRestoreGuard {
	return &CheckpointRestoreGuard{
		cfg:       cfg,
		jobs:      make(map[JobID]*jobRecord),
		nodes:     make(map[NodeID]NodeCaps),
		snapshots: make(map[SnapshotID]*snapshotRecord),
		destroyed: make(map[SnapshotID]destroyedTombstone),
		audit:     make([]AuditEntry, 0),
	}
}

// StartJob registers a running job so it can be checkpointed.
func (g *CheckpointRestoreGuard) StartJob(id JobID, spec JobSpec) error {
	if strings.TrimSpace(string(id)) == "" || strings.TrimSpace(spec.Tenant) == "" {
		g.mu.Lock()
		g.auditLocked(OpStartJob, id, "", "rejected: "+ErrBadJob.Error())
		g.mu.Unlock()
		return ErrBadJob
	}

	spec = cloneJobSpec(spec)
	now := time.Now().UTC()

	g.mu.Lock()
	defer g.mu.Unlock()

	if existing, ok := g.jobs[id]; ok {
		existing.Spec = spec
		if existing.Instances <= 0 {
			existing.Instances = 1
		}
		return nil
	}

	g.jobs[id] = &jobRecord{
		ID:        id,
		Spec:      spec,
		Instances: 1,
		CreatedAt: now,
	}
	g.auditLocked(OpStartJob, id, "", "")
	return nil
}

// RegisterNode records the capabilities and tenant of a restore target.
func (g *CheckpointRestoreGuard) RegisterNode(id NodeID, caps NodeCaps) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[id] = caps
	return nil
}

// Checkpoint exports one job into a sealed snapshot.
func (g *CheckpointRestoreGuard) Checkpoint(jobID JobID, opts Options) (SnapshotID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	job, ok := g.jobs[jobID]
	if !ok {
		return "", ErrNoJob
	}

	imageBytes := job.Spec.DiskBytes
	chargeBytes := imageBytes
	if chargeBytes < 0 {
		chargeBytes = 0
	}
	if opts.MaxBytes > 0 && imageBytes > opts.MaxBytes {
		g.auditLocked(OpCheckpoint, jobID, "", "rejected: "+ErrOversizeSnapshot.Error())
		return "", ErrOversizeSnapshot
	}

	g.snapshotSeq++
	id := SnapshotID(fmt.Sprintf("snap-%d", g.snapshotSeq))
	now := time.Now().UTC()

	safeEnv, redacted, _, redactErr := redactEnvironment(job.Spec.Env, opts.Redact)
	manifest := Manifest{
		ID:            id,
		Tenant:        job.Spec.Tenant,
		Source:        job.Spec.Node,
		State:         SnapshotSealed,
		Created:       now,
		Bytes:         imageBytes,
		Redacted:      redacted,
		Namespaces:    job.Spec.Namespaces,
		Mounts:        cloneMounts(job.Spec.Mounts),
		NetIdentity:   job.Spec.NetIdentity,
		KeyGeneration: g.cfg.EncryptionKeyGeneration,
		OpenFiles:     cloneOpenFiles(job.Spec.OpenFiles),
	}

	var sourceCaps *NodeCaps
	if caps, ok := g.nodes[job.Spec.Node]; ok {
		capsCopy := caps
		sourceCaps = &capsCopy
	}

	if redactErr != nil {
		manifest.State = SnapshotVoid
		manifest.Digest = digestForManifest(manifest, safeEnv)
		g.snapshots[id] = &snapshotRecord{
			Seq:        g.snapshotSeq,
			Manifest:   manifest,
			SafeEnv:    safeEnv,
			SourceCaps: sourceCaps,
		}
		g.auditLocked(OpCheckpoint, jobID, id, "redaction_failed")
		return id, fmt.Errorf("%w: %v", ErrRedactionFailed, redactErr)
	}

	tenantUsage := g.usageLocked(job.Spec.Tenant)
	if g.cfg.MaxSnapshotsPerTenant > 0 && tenantUsage.Snapshots >= g.cfg.MaxSnapshotsPerTenant {
		g.auditLocked(OpCheckpoint, jobID, "", "rejected: "+ErrSnapshotLimit.Error())
		return "", ErrSnapshotLimit
	}
	totalBytes := g.totalBytesLocked()
	if g.cfg.MaxTotalBytes > 0 && totalBytes+chargeBytes > g.cfg.MaxTotalBytes {
		g.auditLocked(OpCheckpoint, jobID, "", "rejected: "+ErrCapacityExceeded.Error())
		return "", ErrCapacityExceeded
	}

	manifest.Digest = digestForManifest(manifest, safeEnv)
	record := &snapshotRecord{
		Seq:        g.snapshotSeq,
		Manifest:   manifest,
		SafeEnv:    safeEnv,
		SourceCaps: sourceCaps,
	}
	g.snapshots[id] = record
	g.auditLocked(OpCheckpoint, jobID, id, opts.Reason)
	return id, nil
}

// Inspect returns the manifest of one snapshot.
func (g *CheckpointRestoreGuard) Inspect(id SnapshotID) (Manifest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.snapshots[id]
	if !ok {
		if _, destroyed := g.destroyed[id]; destroyed {
			return Manifest{}, fmt.Errorf("%w: %w", ErrNoSnapshot, ErrSnapshotDestroyed)
		}
		return Manifest{}, ErrNoSnapshot
	}

	manifest := cloneManifest(record.Manifest)
	g.auditLocked(OpInspect, "", id, "")
	return manifest, nil
}

// Restore starts the snapshot on a target node and returns the new job id.
func (g *CheckpointRestoreGuard) Restore(id SnapshotID, target NodeID) (JobID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.snapshots[id]
	if !ok {
		if _, destroyed := g.destroyed[id]; destroyed {
			return "", fmt.Errorf("%w: %w", ErrNoSnapshot, ErrSnapshotDestroyed)
		}
		return "", ErrNoSnapshot
	}
	if record.Manifest.State == SnapshotVoid {
		return "", ErrSnapshotVoid
	}
	if record.Manifest.State == SnapshotExpired {
		return "", fmt.Errorf("%w: snapshot TTL expired", ErrNoSnapshot)
	}

	node, ok := g.nodes[target]
	if !ok {
		return "", ErrUnknownNode
	}
	if node.Tenant != record.Manifest.Tenant {
		return "", ErrCrossTenant
	}

	if record.RestoredJob != "" {
		if record.RestoreTarget == target {
			record.RestoreCount++
			g.auditLocked(OpRestore, record.RestoredJob, id, "idempotent_retry")
			return record.RestoredJob, nil
		}
		return "", fmt.Errorf("%w: snapshot %s is already restored as %s on %s",
			ErrDuplicateRestore, id, record.RestoredJob, record.RestoreTarget)
	}

	if err := checkCompatibility(record.SourceCaps, node); err != nil {
		g.auditLocked(OpRestore, "", id, "rejected: "+err.Error())
		return "", err
	}
	if err := verifyMountIdentity(record.Manifest); err != nil {
		g.auditLocked(OpRestore, "", id, "rejected: "+err.Error())
		return "", err
	}

	g.jobSeq++
	restoredID := JobID(fmt.Sprintf("job-restore-%06d", g.jobSeq))
	now := time.Now().UTC()
	spec := jobSpecFromManifest(record.Manifest, record.SafeEnv, target)

	g.jobs[restoredID] = &jobRecord{
		ID:            restoredID,
		Spec:          spec,
		Instances:     1,
		RestoredFrom:  id,
		RestoreTarget: target,
		CreatedAt:     now,
	}
	record.RestoredJob = restoredID
	record.RestoreTarget = target
	record.RestoreCount = 1
	g.auditLocked(OpRestore, restoredID, id, string(target))
	return restoredID, nil
}

// Destroy drops a snapshot and frees its quota.
func (g *CheckpointRestoreGuard) Destroy(id SnapshotID) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, alreadyDestroyed := g.destroyed[id]; alreadyDestroyed {
		return nil
	}
	record, ok := g.snapshots[id]
	if !ok {
		return ErrNoSnapshot
	}

	delete(g.snapshots, id)
	g.destroyed[id] = destroyedTombstone{At: time.Now().UTC()}
	detail := record.Manifest.State
	g.auditLocked(OpDestroy, record.RestoredJob, id, detail)
	return nil
}

// Expire destroys snapshots whose TTL passed and returns how many went away.
func (g *CheckpointRestoreGuard) Expire(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cfg.SnapshotTTL <= 0 {
		return 0
	}

	expired := 0
	for id, record := range g.snapshots {
		if record.Manifest.State != SnapshotSealed {
			continue
		}
		created := record.Manifest.Created
		if created.IsZero() {
			continue
		}
		if now.Sub(created) < g.cfg.SnapshotTTL {
			continue
		}

		record.Manifest.State = SnapshotExpired
		expired++
		g.auditLocked(OpExpire, record.RestoredJob, id, "ttl_expired")
	}
	return expired
}

// Snapshots lists the live snapshot ids of one tenant, oldest first.
func (g *CheckpointRestoreGuard) Snapshots(tenant string) []SnapshotID {
	g.mu.RLock()
	defer g.mu.RUnlock()

	ids := make([]SnapshotID, 0)
	for _, record := range g.snapshots {
		if record.Manifest.State != SnapshotSealed {
			continue
		}
		if record.Manifest.Tenant != tenant {
			continue
		}
		ids = append(ids, record.Manifest.ID)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := g.snapshots[ids[i]]
		right := g.snapshots[ids[j]]
		if left.Seq != right.Seq {
			return left.Seq < right.Seq
		}
		return left.Manifest.Created.Before(right.Manifest.Created)
	})
	return ids
}

// Instances reports how many running instances one job id has.
func (g *CheckpointRestoreGuard) Instances(jobID JobID) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if job, ok := g.jobs[jobID]; ok {
		return job.Instances
	}
	return 0
}

// InstanceCount reports the total number of running instances.
func (g *CheckpointRestoreGuard) InstanceCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	total := 0
	for _, job := range g.jobs {
		total += job.Instances
	}
	return total
}

// Usage reports the store accounting of one tenant.
func (g *CheckpointRestoreGuard) Usage(tenant string) Usage {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.usageLocked(tenant)
}

// Audit returns the operation log, oldest first.
func (g *CheckpointRestoreGuard) Audit() []AuditEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]AuditEntry{}, g.audit...)
}

// Export serialises jobs, snapshots, nodes and the audit counter.
func (g *CheckpointRestoreGuard) Export() ([]byte, error) {
	g.mu.RLock()
	state := persistedState{
		Config:      g.cfg,
		Seq:         g.seq,
		SnapshotSeq: g.snapshotSeq,
		JobSeq:      g.jobSeq,
		Jobs:        g.jobs,
		Nodes:       g.nodes,
		Snapshots:   g.snapshots,
		Destroyed:   g.destroyed,
		Audit:       g.audit,
	}
	state = redactedExportState(state)
	stateData, marshalErr := json.Marshal(state)
	g.mu.RUnlock()
	if marshalErr != nil {
		return nil, marshalErr
	}

	sum := sha256.Sum256(stateData)
	payload := envelope{
		Format:   envelopeFormat,
		Checksum: hex.EncodeToString(sum[:]),
		State:    stateData,
	}
	return json.Marshal(payload)
}

func redactedExportState(state persistedState) persistedState {
	redactKeys := make(map[string]struct{})
	for _, record := range state.Snapshots {
		if record == nil {
			continue
		}
		for _, key := range record.Manifest.Redacted {
			redactKeys[strings.ToUpper(strings.TrimSpace(key))] = struct{}{}
		}
	}

	jobs := make(map[JobID]*jobRecord, len(state.Jobs))
	for id, job := range state.Jobs {
		if job == nil {
			jobs[id] = nil
			continue
		}
		jobCopy := *job
		spec := cloneJobSpec(job.Spec)
		for index, entry := range spec.Env {
			key, _, hasEquals := strings.Cut(entry, "=")
			upperKey := strings.ToUpper(strings.TrimSpace(key))
			if hasEquals && (isSensitiveKey(upperKey) || func() bool {
				_, ok := redactKeys[upperKey]
				return ok
			}()) {
				spec.Env[index] = key + "="
			}
		}
		jobCopy.Spec = spec
		jobs[id] = &jobCopy
	}
	state.Jobs = jobs
	return state
}

// Import replaces the whole state with an exported payload, all or nothing.
func (g *CheckpointRestoreGuard) Import(data []byte) error {
	var payload envelope
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: invalid JSON envelope: %v", ErrBadEnvelope, err)
	}
	if payload.Format != envelopeFormat {
		return fmt.Errorf("%w: unsupported envelope format", ErrBadEnvelope)
	}
	if len(payload.State) == 0 {
		return fmt.Errorf("%w: missing state", ErrBadEnvelope)
	}

	sum := sha256.Sum256(payload.State)
	expectedChecksum, err := hex.DecodeString(payload.Checksum)
	if err != nil || !equalBytes(expectedChecksum, sum[:]) {
		return fmt.Errorf("%w: checksum mismatch", ErrBadEnvelope)
	}

	var state persistedState
	if err := json.Unmarshal(payload.State, &state); err != nil {
		return fmt.Errorf("%w: invalid state: %v", ErrBadEnvelope, err)
	}
	if err := validatePersistedState(&state); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.seq = state.Seq
	g.snapshotSeq = state.SnapshotSeq
	g.jobSeq = state.JobSeq
	g.jobs = state.Jobs
	g.nodes = state.Nodes
	g.snapshots = state.Snapshots
	g.destroyed = state.Destroyed
	g.audit = state.Audit
	return nil
}

func (g *CheckpointRestoreGuard) auditLocked(op string, jobID JobID, snapshotID SnapshotID, detail string) AuditEntry {
	g.seq++
	entry := AuditEntry{
		Seq:      g.seq,
		Op:       op,
		Snapshot: snapshotID,
		Job:      jobID,
		Detail:   detail,
		At:       time.Now().UTC(),
	}
	g.audit = append(g.audit, entry)
	return entry
}

func (g *CheckpointRestoreGuard) usageLocked(tenant string) Usage {
	usage := Usage{}
	for _, record := range g.snapshots {
		if record.Manifest.State != SnapshotSealed {
			continue
		}
		if record.Manifest.Tenant != tenant {
			continue
		}
		usage.Snapshots++
		if record.Manifest.Bytes > 0 {
			usage.Bytes += record.Manifest.Bytes
		}
	}
	return usage
}

func (g *CheckpointRestoreGuard) totalBytesLocked() int64 {
	var total int64
	for _, record := range g.snapshots {
		if record.Manifest.State == SnapshotSealed && record.Manifest.Bytes > 0 {
			total += record.Manifest.Bytes
		}
	}
	return total
}

func cloneJobSpec(spec JobSpec) JobSpec {
	clone := spec
	clone.Env = append([]string(nil), spec.Env...)
	clone.Mounts = cloneMounts(spec.Mounts)
	clone.OpenFiles = cloneOpenFiles(spec.OpenFiles)
	return clone
}

func cloneMounts(mounts []MountEntry) []MountEntry {
	return append([]MountEntry(nil), mounts...)
}

func cloneOpenFiles(files []OpenFile) []OpenFile {
	return append([]OpenFile(nil), files...)
}

func cloneManifest(manifest Manifest) Manifest {
	clone := manifest
	clone.Redacted = append([]string(nil), manifest.Redacted...)
	clone.Mounts = cloneMounts(manifest.Mounts)
	clone.OpenFiles = cloneOpenFiles(manifest.OpenFiles)
	return clone
}

func jobSpecFromManifest(manifest Manifest, env []string, target NodeID) JobSpec {
	return JobSpec{
		Tenant:      manifest.Tenant,
		Node:        target,
		Env:         append([]string(nil), env...),
		Mounts:      cloneMounts(manifest.Mounts),
		Namespaces:  manifest.Namespaces,
		NetIdentity: manifest.NetIdentity,
		DiskBytes:   manifest.Bytes,
		OpenFiles:   cloneOpenFiles(manifest.OpenFiles),
	}
}

func isSensitiveKey(upperKey string) bool {
	for _, marker := range DefaultRedactMarkers {
		if strings.Contains(upperKey, marker) {
			return true
		}
	}
	return false
}

func redactEnvironment(env []string, extra []string) ([]string, []string, []string, error) {
	explicit := make(map[string]string, len(extra))
	for _, key := range extra {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			return nil, nil, nil, errors.New("redaction key is empty")
		}
		explicit[strings.ToUpper(trimmed)] = trimmed
	}

	safeEnv := make([]string, 0, len(env))
	redacted := make([]string, 0)
	seenNames := make(map[string]bool)
	seenUpper := make(map[string]bool)
	secrets := make([]string, 0)

	for index, entry := range env {
		if entry == "" {
			return nil, nil, nil, fmt.Errorf("environment entry %d is empty", index)
		}
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, nil, nil, fmt.Errorf("environment entry %d contains NUL", index)
		}

		key, value, hasEquals := strings.Cut(entry, "=")
		if !hasEquals {
			key = entry
			value = ""
		}
		if strings.TrimSpace(key) == "" {
			return nil, nil, nil, fmt.Errorf("environment entry %d has an empty key", index)
		}

		upperKey := strings.ToUpper(key)
		seenUpper[upperKey] = true
		if isSensitiveKey(upperKey) {
			if !seenNames[key] {
				redacted = append(redacted, key)
				seenNames[key] = true
			}
			if value != "" {
				secrets = append(secrets, value)
			}
			safeEnv = append(safeEnv, key+"=")
			continue
		}
		if _, requested := explicit[upperKey]; requested {
			if !seenNames[key] {
				redacted = append(redacted, key)
				seenNames[key] = true
			}
			if value != "" {
				secrets = append(secrets, value)
			}
			safeEnv = append(safeEnv, key+"=")
			continue
		}

		safeEnv = append(safeEnv, entry)
	}

	for _, key := range extra {
		trimmed := strings.TrimSpace(key)
		upperKey := strings.ToUpper(trimmed)
		if !seenUpper[upperKey] && !seenNames[trimmed] {
			redacted = append(redacted, trimmed)
			seenNames[trimmed] = true
		}
	}

	return safeEnv, redacted, secrets, nil
}

type digestPayload struct {
	Manifest Manifest `json:"manifest"`
	SafeEnv  []string `json:"safe_env,omitempty"`
}

func digestForManifest(manifest Manifest, safeEnv []string) string {
	manifest.Digest = ""
	data, err := json.Marshal(digestPayload{Manifest: manifest, SafeEnv: safeEnv})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func parseVersionNumbers(raw string) []int {
	match := versionNumberRE.FindString(raw)
	if match == "" {
		return nil
	}
	parts := strings.Split(match, ".")
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		numbers = append(numbers, value)
	}
	return numbers
}

func compareVersions(left, right []int) int {
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	for i := 0; i < length; i++ {
		leftValue := 0
		rightValue := 0
		if i < len(left) {
			leftValue = left[i]
		}
		if i < len(right) {
			rightValue = right[i]
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func runtimeFamily(raw string) string {
	return strings.ToLower(runtimeFamilyRE.FindString(strings.TrimSpace(raw)))
}

func normalizeRuntimeVersion(family string, version []int) []int {
	if family == "runsc" && len(version) > 0 && version[0] > 19000000 {
		version[0] /= 10000
	}
	return version
}

func kernelTooOld(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	version := parseVersionNumbers(raw)
	if len(version) == 0 {
		return true
	}
	return compareVersions(version, []int{5, 4}) < 0
}

func runtimeTooOld(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	family := runtimeFamily(raw)
	version := normalizeRuntimeVersion(family, parseVersionNumbers(raw))
	switch family {
	case "runsc":
		if len(version) == 0 {
			return false
		}
		year := version[0]
		if year > 19000000 {
			year /= 10000
		}
		return year < 2024
	case "runc":
		return compareVersions(version, []int{1, 1}) < 0
	default:
		return false
	}
}

func checkCompatibility(source *NodeCaps, target NodeCaps) error {
	if source != nil {
		if strings.TrimSpace(source.Kernel) != "" {
			sourceVersion := parseVersionNumbers(source.Kernel)
			targetVersion := parseVersionNumbers(target.Kernel)
			if len(sourceVersion) == 0 || len(targetVersion) == 0 ||
				compareVersions(targetVersion, sourceVersion) < 0 {
				return fmt.Errorf("%w: kernel %q cannot satisfy %q",
					ErrIncompatibleNode, target.Kernel, source.Kernel)
			}
		} else if kernelTooOld(target.Kernel) {
			return fmt.Errorf("%w: kernel %q is too old", ErrIncompatibleNode, target.Kernel)
		}

		if strings.TrimSpace(source.Runtime) != "" {
			sourceFamily := runtimeFamily(source.Runtime)
			targetFamily := runtimeFamily(target.Runtime)
			if sourceFamily != "" && targetFamily != sourceFamily {
				return fmt.Errorf("%w: runtime family %q cannot satisfy %q",
					ErrIncompatibleNode, targetFamily, sourceFamily)
			}
			sourceVersion := normalizeRuntimeVersion(sourceFamily, parseVersionNumbers(source.Runtime))
			targetVersion := normalizeRuntimeVersion(targetFamily, parseVersionNumbers(target.Runtime))
			if len(sourceVersion) > 0 && (len(targetVersion) == 0 || compareVersions(targetVersion, sourceVersion) < 0) {
				return fmt.Errorf("%w: runtime %q cannot satisfy %q",
					ErrIncompatibleNode, target.Runtime, source.Runtime)
			}
		} else if runtimeTooOld(target.Runtime) {
			return fmt.Errorf("%w: runtime %q is too old", ErrIncompatibleNode, target.Runtime)
		}

		if source.Arch != "" && target.Arch != "" && !strings.EqualFold(source.Arch, target.Arch) {
			return fmt.Errorf("%w: arch %q cannot satisfy %q", ErrIncompatibleNode, target.Arch, source.Arch)
		}
		if source.Seccomp && !target.Seccomp {
			return fmt.Errorf("%w: target lacks seccomp", ErrIncompatibleNode)
		}
		return nil
	}

	if kernelTooOld(target.Kernel) {
		return fmt.Errorf("%w: kernel %q is too old", ErrIncompatibleNode, target.Kernel)
	}
	if runtimeTooOld(target.Runtime) {
		return fmt.Errorf("%w: runtime %q is too old", ErrIncompatibleNode, target.Runtime)
	}
	return nil
}

func parseSHA256Digest(claimed string) ([sha256.Size]byte, bool) {
	var result [sha256.Size]byte
	normalized := strings.TrimSpace(claimed)
	if strings.HasPrefix(strings.ToLower(normalized), "sha256:") {
		normalized = normalized[len("sha256:"):]
	}
	if len(normalized) != hex.EncodedLen(sha256.Size) {
		return result, false
	}
	decoded, err := hex.DecodeString(normalized)
	if err != nil || len(decoded) != sha256.Size {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

func hasCanonicalSHA256(claimed string) bool {
	_, ok := parseSHA256Digest(claimed)
	return ok
}

func verifyPathDigest(path, claimed string) error {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(claimed) == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && hasCanonicalSHA256(claimed) {
			return fmt.Errorf("%w: %s is missing", ErrMountIdentityDrift, path)
		}
		return nil
	}
	if err != nil {
		if hasCanonicalSHA256(claimed) {
			return fmt.Errorf("%w: cannot stat %s: %v", ErrMountIdentityDrift, path, err)
		}
		return nil
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: cannot read %s: %v", ErrMountIdentityDrift, path, err)
	}
	actual := sha256.Sum256(content)
	expected, ok := parseSHA256Digest(claimed)
	if !ok || expected != actual {
		return fmt.Errorf("%w: digest mismatch for %s", ErrMountIdentityDrift, path)
	}
	return nil
}

func verifyMountIdentity(manifest Manifest) error {
	for _, file := range manifest.OpenFiles {
		if err := verifyPathDigest(file.Path, file.Digest); err != nil {
			return err
		}
	}
	for _, mount := range manifest.Mounts {
		if err := verifyPathDigest(mount.Source, mount.Digest); err != nil {
			return err
		}
	}
	return nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validatePersistedState(state *persistedState) error {
	if state == nil {
		return fmt.Errorf("%w: empty state", ErrBadEnvelope)
	}
	if state.Jobs == nil {
		state.Jobs = make(map[JobID]*jobRecord)
	}
	if state.Nodes == nil {
		state.Nodes = make(map[NodeID]NodeCaps)
	}
	if state.Snapshots == nil {
		state.Snapshots = make(map[SnapshotID]*snapshotRecord)
	}
	if state.Destroyed == nil {
		state.Destroyed = make(map[SnapshotID]destroyedTombstone)
	}
	if state.Audit == nil {
		state.Audit = make([]AuditEntry, 0)
	}

	var maxAuditSeq int64
	for _, entry := range state.Audit {
		if entry.Seq > maxAuditSeq {
			maxAuditSeq = entry.Seq
		}
	}
	if state.Seq < maxAuditSeq {
		state.Seq = maxAuditSeq
	}

	var maxSnapshotSeq int64
	for id, record := range state.Snapshots {
		if record == nil {
			return fmt.Errorf("%w: snapshot %q is null", ErrBadEnvelope, id)
		}
		if record.Manifest.ID == "" {
			record.Manifest.ID = id
		}
		if record.Seq > maxSnapshotSeq {
			maxSnapshotSeq = record.Seq
		}
	}
	if state.SnapshotSeq < maxSnapshotSeq {
		state.SnapshotSeq = maxSnapshotSeq
	}

	var maxJobSeq int64
	for id, job := range state.Jobs {
		if job == nil {
			return fmt.Errorf("%w: job %q is null", ErrBadEnvelope, id)
		}
		if job.ID == "" {
			job.ID = id
		}
	}
	if state.JobSeq < maxJobSeq {
		state.JobSeq = maxJobSeq
	}
	return nil
}
