package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-02"
)

func testConfig() impl.Config {
	return impl.Config{
		MaxSnapshotsPerTenant:   3,
		MaxTotalBytes:           20 << 20,
		SnapshotTTL:             time.Hour,
		EncryptionKeyGeneration: 4,
	}
}

func startJob(t *testing.T, g *impl.CheckpointRestoreGuard, id impl.JobID, tenant string, disk int64) {
	t.Helper()
	spec := impl.JobSpec{
		Tenant: tenant,
		Node:   impl.NodeID("node-a"),
		Env: []string{
			"PATH=/usr/bin:/bin",
			"DB_PASSWORD=hunter2",
			"API_TOKEN=tok_9f31",
			"SESSION_COOKIE=s%3Aabc",
		},
		Mounts: []impl.MountEntry{
			{Target: "/work", Source: "/srv/work/" + string(id), FSType: "ext4", Digest: "sha256:aa11"},
			{Target: "/data", Source: "/srv/data", FSType: "xfs", ReadOnly: true, Digest: "sha256:bb22"},
		},
		Namespaces: impl.NamespaceSpec{
			PidNS:    "pid:" + string(id),
			NetNS:    "net:" + string(id),
			MountNS:  "mnt:" + string(id),
			UserNS:   "usr:" + string(id),
			CgroupNS: "cgr:" + string(id),
		},
		NetIdentity: "netid-" + string(id),
		DiskBytes:   disk,
		OpenFiles:   []impl.OpenFile{{Path: "/work/report.csv", Digest: "sha256:cc33"}},
	}
	if err := g.StartJob(id, spec); err != nil {
		t.Fatalf("start job %s must succeed, got %v", id, err)
	}
}

func TestStartJobRejectsBlankTenant(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if err := g.StartJob(impl.JobID("job-1"), impl.JobSpec{}); !errors.Is(err, impl.ErrBadJob) {
		t.Fatalf("job without tenant err = %v, want ErrBadJob", err)
	}
	if err := g.StartJob(impl.JobID("job-2"), impl.JobSpec{Tenant: "   "}); !errors.Is(err, impl.ErrBadJob) {
		t.Fatalf("blank tenant err = %v, want ErrBadJob", err)
	}
	if err := g.StartJob(impl.JobID("  "), impl.JobSpec{Tenant: "t-1"}); !errors.Is(err, impl.ErrBadJob) {
		t.Fatalf("blank job id err = %v, want ErrBadJob", err)
	}
	if got := g.InstanceCount(); got != 0 {
		t.Fatalf("rejected jobs must not run, instance count = %d", got)
	}
}

func TestCheckpointReturnsSealedSnapshot(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	startJob(t, g, impl.JobID("job-1"), "t-1", 4096)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{Freeze: true, Reason: "nightly"})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	if string(id) == "" || !strings.HasPrefix(string(id), "snap-") {
		t.Fatalf("snapshot id = %q, want a snap- prefixed id", id)
	}
	m, err := g.Inspect(id)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if m.State != impl.SnapshotSealed {
		t.Fatalf("state = %q, want %q", m.State, impl.SnapshotSealed)
	}
	if m.Tenant != "t-1" || m.Source != impl.NodeID("node-a") {
		t.Fatalf("manifest origin = %q/%q, want t-1/node-a", m.Tenant, m.Source)
	}
	if m.Digest == "" {
		t.Fatalf("manifest digest must not be empty")
	}
	if m.Created.IsZero() {
		t.Fatalf("manifest creation time must be set")
	}
	if m.Bytes != 4096 {
		t.Fatalf("snapshot bytes = %d, want 4096", m.Bytes)
	}
	if got := len(g.Snapshots("t-1")); got != 1 {
		t.Fatalf("tenant snapshots = %d, want 1", got)
	}
}

func TestCheckpointRedactsSensitiveEnv(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	startJob(t, g, impl.JobID("job-1"), "t-1", 4096)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{Redact: []string{"SESSION_COOKIE"}})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	m, err := g.Inspect(id)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	redacted := strings.Join(m.Redacted, ",")
	for _, key := range []string{"DB_PASSWORD", "API_TOKEN", "SESSION_COOKIE"} {
		if !strings.Contains(redacted, key) {
			t.Fatalf("redacted keys %q must list %s", redacted, key)
		}
	}
	if strings.Contains(redacted, "PATH") {
		t.Fatalf("harmless keys must stay, redacted list = %q", redacted)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("manifest must be serialisable, got %v", err)
	}
	for _, secret := range []string{"hunter2", "tok_9f31", "s%3Aabc"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("manifest leaked secret %q", secret)
		}
	}
}

func TestCheckpointRejectsUnknownJob(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if _, err := g.Checkpoint(impl.JobID("job-missing"), impl.Options{}); !errors.Is(err, impl.ErrNoJob) {
		t.Fatalf("unknown job err = %v, want ErrNoJob", err)
	}
	if got := g.Usage("t-1").Snapshots; got != 0 {
		t.Fatalf("failed checkpoint must not consume quota, snapshots = %d", got)
	}
}

func TestCheckpointEnforcesPerTenantLimit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxSnapshotsPerTenant = 2
	g := impl.NewCheckpointRestoreGuard(cfg)
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	startJob(t, g, impl.JobID("job-2"), "t-1", 1024)
	startJob(t, g, impl.JobID("job-3"), "t-1", 1024)
	startJob(t, g, impl.JobID("job-4"), "t-2", 1024)
	for _, job := range []impl.JobID{"job-1", "job-2"} {
		if _, err := g.Checkpoint(job, impl.Options{}); err != nil {
			t.Fatalf("checkpoint %s must succeed, got %v", job, err)
		}
	}
	if _, err := g.Checkpoint(impl.JobID("job-3"), impl.Options{}); !errors.Is(err, impl.ErrSnapshotLimit) {
		t.Fatalf("third snapshot err = %v, want ErrSnapshotLimit", err)
	}
	if _, err := g.Checkpoint(impl.JobID("job-4"), impl.Options{}); err != nil {
		t.Fatalf("neighbour tenant must keep its quota, got %v", err)
	}
	if got := len(g.Snapshots("t-1")); got != 2 {
		t.Fatalf("t-1 snapshots = %d, want 2", got)
	}
}

func TestCheckpointEnforcesTotalCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTotalBytes = 10 << 20
	g := impl.NewCheckpointRestoreGuard(cfg)
	startJob(t, g, impl.JobID("job-1"), "t-1", 8<<20)
	startJob(t, g, impl.JobID("job-2"), "t-1", 8<<20)
	if _, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{}); err != nil {
		t.Fatalf("first checkpoint must succeed, got %v", err)
	}
	if _, err := g.Checkpoint(impl.JobID("job-2"), impl.Options{}); !errors.Is(err, impl.ErrCapacityExceeded) {
		t.Fatalf("over capacity err = %v, want ErrCapacityExceeded", err)
	}
	u := g.Usage("t-1")
	if u.Snapshots != 1 || u.Bytes != 8<<20 {
		t.Fatalf("usage = %+v, want 1 snapshot and 8MiB", u)
	}
}

func TestCheckpointRejectsOversizeSnapshot(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	startJob(t, g, impl.JobID("job-1"), "t-1", 4096)
	if _, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{MaxBytes: 1024}); !errors.Is(err, impl.ErrOversizeSnapshot) {
		t.Fatalf("oversize export err = %v, want ErrOversizeSnapshot", err)
	}
	if got := g.Usage("t-1"); got.Snapshots != 0 || got.Bytes != 0 {
		t.Fatalf("rejected export must not consume quota, usage = %+v", got)
	}
}

func TestInspectUnknownSnapshot(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if _, err := g.Inspect(impl.SnapshotID("snap-nope")); !errors.Is(err, impl.ErrNoSnapshot) {
		t.Fatalf("unknown snapshot err = %v, want ErrNoSnapshot", err)
	}
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	if _, err := g.Inspect(id); err != nil {
		t.Fatalf("known snapshot must be inspectable, got %v", err)
	}
}

func TestInspectKeepsNamespaceAndMountIdentity(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	startJob(t, g, impl.JobID("job-1"), "t-1", 2048)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	m, err := g.Inspect(id)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if m.Namespaces.PidNS != "pid:job-1" || m.Namespaces.NetNS != "net:job-1" {
		t.Fatalf("namespace identity = %+v, want the job namespaces", m.Namespaces)
	}
	if m.Namespaces.UserNS != "usr:job-1" || m.Namespaces.CgroupNS != "cgr:job-1" {
		t.Fatalf("namespace identity = %+v, want user and cgroup namespaces", m.Namespaces)
	}
	if m.NetIdentity != "netid-job-1" {
		t.Fatalf("network identity = %q, want netid-job-1", m.NetIdentity)
	}
	if len(m.Mounts) != 2 {
		t.Fatalf("mount entries = %d, want 2", len(m.Mounts))
	}
	if m.Mounts[1].Target != "/data" || !m.Mounts[1].ReadOnly || m.Mounts[1].Digest != "sha256:bb22" {
		t.Fatalf("second mount = %+v, want a read only /data with its digest", m.Mounts[1])
	}
	if len(m.OpenFiles) != 1 || m.OpenFiles[0].Digest != "sha256:cc33" {
		t.Fatalf("open files = %+v, want the recorded report.csv", m.OpenFiles)
	}
	if m.KeyGeneration != 4 {
		t.Fatalf("key generation = %d, want 4", m.KeyGeneration)
	}
}

func TestRestoreSameTenantCreatesOneInstance(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if err := g.RegisterNode(impl.NodeID("node-b"), impl.NodeCaps{Tenant: "t-1", Kernel: "6.8.0", Runtime: "runsc-2025", Arch: "amd64", Seccomp: true}); err != nil {
		t.Fatalf("register node must succeed, got %v", err)
	}
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	restored, err := g.Restore(id, impl.NodeID("node-b"))
	if err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	if restored == "" || restored == impl.JobID("job-1") {
		t.Fatalf("restored job id = %q, want a fresh id", restored)
	}
	if got := g.Instances(restored); got != 1 {
		t.Fatalf("instances of %s = %d, want 1", restored, got)
	}
	if got := g.InstanceCount(); got != 2 {
		t.Fatalf("total instances = %d, want 2 (original plus restored)", got)
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if err := g.RegisterNode(impl.NodeID("node-b"), impl.NodeCaps{Tenant: "t-1", Kernel: "6.8.0", Runtime: "runsc-2025", Arch: "amd64", Seccomp: true}); err != nil {
		t.Fatalf("register node must succeed, got %v", err)
	}
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	first, err := g.Restore(id, impl.NodeID("node-b"))
	if err != nil {
		t.Fatalf("first restore must succeed, got %v", err)
	}
	second, err := g.Restore(id, impl.NodeID("node-b"))
	if err != nil {
		t.Fatalf("repeated restore must be idempotent, got %v", err)
	}
	if second != first {
		t.Fatalf("repeated restore returned %q, want the same %q", second, first)
	}
	if got := g.InstanceCount(); got != 2 {
		t.Fatalf("total instances = %d, want 2 and never 3", got)
	}
}

func TestRestoreRejectsCrossTenantNode(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if err := g.RegisterNode(impl.NodeID("node-foreign"), impl.NodeCaps{Tenant: "t-2", Kernel: "6.8.0", Runtime: "runsc-2025", Arch: "amd64", Seccomp: true}); err != nil {
		t.Fatalf("register node must succeed, got %v", err)
	}
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	if _, err := g.Restore(id, impl.NodeID("node-foreign")); !errors.Is(err, impl.ErrCrossTenant) {
		t.Fatalf("cross tenant restore err = %v, want ErrCrossTenant", err)
	}
	if got := g.InstanceCount(); got != 1 {
		t.Fatalf("rejected restore must not start anything, instances = %d", got)
	}
}

func TestRestoreRejectsUnknownNode(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	if _, err := g.Restore(id, impl.NodeID("node-ghost")); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("unknown node restore err = %v, want ErrUnknownNode", err)
	}
	if got := g.InstanceCount(); got != 1 {
		t.Fatalf("rejected restore must not start anything, instances = %d", got)
	}
}

func TestDestroyFreesQuotaAndBlocksInspect(t *testing.T) {
	cfg := testConfig()
	cfg.MaxSnapshotsPerTenant = 1
	g := impl.NewCheckpointRestoreGuard(cfg)
	startJob(t, g, impl.JobID("job-1"), "t-1", 2048)
	startJob(t, g, impl.JobID("job-2"), "t-1", 2048)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	if err := g.Destroy(id); err != nil {
		t.Fatalf("destroy must succeed, got %v", err)
	}
	if _, err := g.Inspect(id); !errors.Is(err, impl.ErrNoSnapshot) {
		t.Fatalf("inspect after destroy err = %v, want ErrNoSnapshot", err)
	}
	if got := g.Usage("t-1"); got.Snapshots != 0 || got.Bytes != 0 {
		t.Fatalf("destroy must free the quota, usage = %+v", got)
	}
	if _, err := g.Checkpoint(impl.JobID("job-2"), impl.Options{}); err != nil {
		t.Fatalf("quota must be reusable after destroy, got %v", err)
	}
	if err := g.Destroy(id); err != nil {
		t.Fatalf("repeated destroy must stay a no op, got %v", err)
	}
}

func TestAuditTrailRecordsOperations(t *testing.T) {
	g := impl.NewCheckpointRestoreGuard(testConfig())
	if err := g.RegisterNode(impl.NodeID("node-b"), impl.NodeCaps{Tenant: "t-1", Kernel: "6.8.0", Runtime: "runsc-2025", Arch: "amd64", Seccomp: true}); err != nil {
		t.Fatalf("register node must succeed, got %v", err)
	}
	startJob(t, g, impl.JobID("job-1"), "t-1", 1024)
	id, err := g.Checkpoint(impl.JobID("job-1"), impl.Options{})
	if err != nil {
		t.Fatalf("checkpoint must succeed, got %v", err)
	}
	if _, err := g.Restore(id, impl.NodeID("node-b")); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	if err := g.Destroy(id); err != nil {
		t.Fatalf("destroy must succeed, got %v", err)
	}
	entries := g.Audit()
	want := []string{impl.OpStartJob, impl.OpCheckpoint, impl.OpRestore, impl.OpDestroy}
	if len(entries) < len(want) {
		t.Fatalf("audit entries = %d, want at least %d", len(entries), len(want))
	}
	for i, op := range want {
		if entries[i].Op != op {
			t.Fatalf("audit[%d].Op = %q, want %q", i, entries[i].Op, op)
		}
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq <= entries[i-1].Seq {
			t.Fatalf("audit sequence must grow: %d then %d", entries[i-1].Seq, entries[i].Seq)
		}
	}
	if entries[len(entries)-1].Snapshot != id {
		t.Fatalf("destroy entry snapshot = %q, want %q", entries[len(entries)-1].Snapshot, id)
	}
}
