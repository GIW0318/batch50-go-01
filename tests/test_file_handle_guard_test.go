package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-05"
)

func fhLimits() impl.Limits {
	return impl.Limits{
		AllowedMounts:     []uint64{7, 8},
		MaxCache:          16,
		MaxOpensPerTenant: 4,
		GrantTTL:          time.Hour,
	}
}

func guardWithMounts(t *testing.T) *impl.FileHandleGuard {
	t.Helper()
	g := impl.NewFileHandleGuard(fhLimits())
	if err := g.AddMount(7, "/srv/t-1", "ns-a", true); err != nil {
		t.Fatalf("add mount 7 must succeed, got %v", err)
	}
	if err := g.AddMount(8, "/srv/t-1/logs", "ns-a", true); err != nil {
		t.Fatalf("add mount 8 must succeed, got %v", err)
	}
	return g
}

func mustLookup(t *testing.T, g *impl.FileHandleGuard, path string) impl.Handle {
	t.Helper()
	h, err := g.Lookup(path)
	if err != nil {
		t.Fatalf("lookup %s must succeed, got %v", path, err)
	}
	return h
}

func ownerT1() impl.Owner {
	return impl.Owner{Tenant: "t-1", UID: 1000, GID: 1000, Mode: 0o644}
}

func callerT1() impl.Caller {
	return impl.Caller{Tenant: "t-1", PID: 7001, Namespace: "ns-a"}
}

func mustRegister(t *testing.T, g *impl.FileHandleGuard, h impl.Handle, owner impl.Owner) {
	t.Helper()
	if err := g.Register(h, owner); err != nil {
		t.Fatalf("register %s must succeed, got %v", h.Path, err)
	}
}

func TestLookupReturnsStableHandle(t *testing.T) {
	g := guardWithMounts(t)
	first := mustLookup(t, g, "/srv/t-1/data/report.csv")
	if first.MountID != 7 {
		t.Fatalf("mount id = %d, want 7", first.MountID)
	}
	if first.Inode == 0 {
		t.Fatalf("inode must not be zero")
	}
	if first.Generation == 0 {
		t.Fatalf("generation must be recorded for a mount that supports it")
	}
	if first.Path != "/srv/t-1/data/report.csv" {
		t.Fatalf("handle path = %q, want the looked up path", first.Path)
	}
	if first.Namespace != "ns-a" {
		t.Fatalf("namespace = %q, want ns-a", first.Namespace)
	}
	if first.Digest == "" {
		t.Fatalf("handle digest must not be empty")
	}
	second := mustLookup(t, g, "/srv/t-1/data/report.csv")
	if second != first {
		t.Fatalf("lookup is not stable: %+v then %+v", first, second)
	}
}

func TestLookupRejectsBadPath(t *testing.T) {
	g := guardWithMounts(t)
	for _, path := range []string{"", "   ", "relative/report.csv", "/srv/t-1/../etc/shadow"} {
		if _, err := g.Lookup(path); !errors.Is(err, impl.ErrBadPath) {
			t.Fatalf("lookup(%q) err = %v, want ErrBadPath", path, err)
		}
	}
}

func TestLookupRejectsMountOutsideAllowList(t *testing.T) {
	limits := fhLimits()
	limits.AllowedMounts = []uint64{7}
	g := impl.NewFileHandleGuard(limits)
	if err := g.AddMount(7, "/srv/t-1", "ns-a", true); err != nil {
		t.Fatalf("add mount 7 must succeed, got %v", err)
	}
	if err := g.AddMount(9, "/srv/t-2", "ns-b", true); err != nil {
		t.Fatalf("add mount 9 must succeed, got %v", err)
	}
	if _, err := g.Lookup("/srv/t-2/secret.txt"); !errors.Is(err, impl.ErrMountDenied) {
		t.Fatalf("mount outside the allow list err = %v, want ErrMountDenied", err)
	}
	if _, err := g.Lookup("/tmp/scratch.txt"); !errors.Is(err, impl.ErrMountDenied) {
		t.Fatalf("path under no mount err = %v, want ErrMountDenied", err)
	}
	if _, err := g.Lookup("/srv/t-1/ok.txt"); err != nil {
		t.Fatalf("allowed mount must resolve, got %v", err)
	}
}

func TestLookupUsesLongestPrefix(t *testing.T) {
	g := guardWithMounts(t)
	logs := mustLookup(t, g, "/srv/t-1/logs/app.log")
	if logs.MountID != 8 {
		t.Fatalf("nested mount id = %d, want 8", logs.MountID)
	}
	data := mustLookup(t, g, "/srv/t-1/data/app.log")
	if data.MountID != 7 {
		t.Fatalf("parent mount id = %d, want 7", data.MountID)
	}
	if logs.Inode == data.Inode {
		t.Fatalf("different paths must not share an inode: %d", logs.Inode)
	}
	if err := g.AddMount(0, "/srv/zero", "ns-a", true); !errors.Is(err, impl.ErrBadMount) {
		t.Fatalf("mount with id zero err = %v, want ErrBadMount", err)
	}
}

func TestRegisterRejectsBadOwner(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	if err := g.Register(h, impl.Owner{Mode: 0o644}); !errors.Is(err, impl.ErrBadOwner) {
		t.Fatalf("owner without tenant err = %v, want ErrBadOwner", err)
	}
	if err := g.Register(h, impl.Owner{Tenant: "t-1", UID: 1000, GID: 1000}); !errors.Is(err, impl.ErrBadOwner) {
		t.Fatalf("owner without mode bits err = %v, want ErrBadOwner", err)
	}
	if err := g.Register(h, ownerT1()); err != nil {
		t.Fatalf("valid owner must be accepted, got %v", err)
	}
}

func TestRegisterUnknownHandle(t *testing.T) {
	g := guardWithMounts(t)
	stranger := impl.Handle{MountID: 7, Inode: 12345, Path: "/srv/t-1/data/ghost.csv"}
	if err := g.Register(stranger, ownerT1()); !errors.Is(err, impl.ErrNoHandle) {
		t.Fatalf("unresolved handle err = %v, want ErrNoHandle", err)
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	mustRegister(t, g, h, ownerT1())
	if err := g.Register(h, ownerT1()); err != nil {
		t.Fatalf("repeated register must be a no op, got %v", err)
	}
	other := impl.Owner{Tenant: "t-2", UID: 2000, GID: 2000, Mode: 0o600}
	if err := g.Register(h, other); !errors.Is(err, impl.ErrAlreadyOwned) {
		t.Fatalf("second owner err = %v, want ErrAlreadyOwned", err)
	}
}

func TestOpenRequiresRegisteredHandle(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	if _, err := g.Open(h, impl.AccessRead, callerT1()); !errors.Is(err, impl.ErrNotRegistered) {
		t.Fatalf("open without owner err = %v, want ErrNotRegistered", err)
	}
	if got := g.OpenCount("t-1"); got != 0 {
		t.Fatalf("rejected open must not be counted, count = %d", got)
	}
}

func TestOpenRejectsTenantMismatch(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	mustRegister(t, g, h, ownerT1())
	intruder := impl.Caller{Tenant: "t-2", PID: 9001, Namespace: "ns-a"}
	if _, err := g.Open(h, impl.AccessRead, intruder); !errors.Is(err, impl.ErrOwnerMismatch) {
		t.Fatalf("foreign tenant open err = %v, want ErrOwnerMismatch", err)
	}
	if got := g.OpenCount("t-2"); got != 0 {
		t.Fatalf("rejected open must not be counted, count = %d", got)
	}
}

func TestOpenDeniesReadOnWriteOnlyOwner(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/secret.bin")
	writeOnly := impl.Owner{Tenant: "t-1", UID: 1000, GID: 1000, Mode: 0o200}
	mustRegister(t, g, h, writeOnly)
	if _, err := g.Open(h, impl.AccessRead, callerT1()); !errors.Is(err, impl.ErrPermissionDenied) {
		t.Fatalf("read on a write only file err = %v, want ErrPermissionDenied", err)
	}
	file, err := g.Open(h, impl.AccessWrite, callerT1())
	if err != nil {
		t.Fatalf("write open must succeed, got %v", err)
	}
	if file.Flags != impl.AccessWrite || file.Tenant != "t-1" {
		t.Fatalf("file = %+v, want a t-1 write open", file)
	}
	if file.OpenedAt.IsZero() {
		t.Fatalf("open timestamp must be recorded")
	}
	if got := g.OpenCount("t-1"); got != 1 {
		t.Fatalf("open count = %d, want 1", got)
	}
	if files := g.OpenedFiles("t-1"); len(files) != 1 || files[0].Handle.Inode != h.Inode {
		t.Fatalf("opened files = %+v, want the single admitted open", files)
	}
}

func TestOpenEnforcesPerTenantLimit(t *testing.T) {
	limits := fhLimits()
	limits.MaxOpensPerTenant = 1
	g := impl.NewFileHandleGuard(limits)
	if err := g.AddMount(7, "/srv/t-1", "ns-a", true); err != nil {
		t.Fatalf("add mount must succeed, got %v", err)
	}
	first := mustLookup(t, g, "/srv/t-1/a.txt")
	second := mustLookup(t, g, "/srv/t-1/b.txt")
	mustRegister(t, g, first, ownerT1())
	mustRegister(t, g, second, ownerT1())
	if _, err := g.Open(first, impl.AccessRead, callerT1()); err != nil {
		t.Fatalf("first open must succeed, got %v", err)
	}
	if _, err := g.Open(second, impl.AccessRead, callerT1()); !errors.Is(err, impl.ErrOpenLimit) {
		t.Fatalf("second open err = %v, want ErrOpenLimit", err)
	}
	if got := g.OpenCount("t-1"); got != 1 {
		t.Fatalf("open count = %d, want 1", got)
	}
}

func TestRevokeBlocksNewOpens(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	mustRegister(t, g, h, ownerT1())
	if _, err := g.Open(h, impl.AccessRead, callerT1()); err != nil {
		t.Fatalf("first open must succeed, got %v", err)
	}
	if err := g.Revoke(h); err != nil {
		t.Fatalf("revoke must succeed, got %v", err)
	}
	if _, err := g.Open(h, impl.AccessRead, callerT1()); !errors.Is(err, impl.ErrRevoked) {
		t.Fatalf("open after revoke err = %v, want ErrRevoked", err)
	}
	if err := g.Verify(h); !errors.Is(err, impl.ErrRevoked) {
		t.Fatalf("verify after revoke err = %v, want ErrRevoked", err)
	}
	if files := g.OpenedFiles("t-1"); len(files) != 1 {
		t.Fatalf("revoke must not close files already open, got %d", len(files))
	}
	stranger := impl.Handle{MountID: 7, Inode: 4242, Path: "/srv/t-1/ghost"}
	if err := g.Revoke(stranger); !errors.Is(err, impl.ErrNoHandle) {
		t.Fatalf("revoke of an unknown handle err = %v, want ErrNoHandle", err)
	}
}

func TestOpenRejectsStaleHandle(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	mustRegister(t, g, h, ownerT1())
	stale := h
	stale.Generation = h.Generation + 1
	if _, err := g.Open(stale, impl.AccessRead, callerT1()); !errors.Is(err, impl.ErrStaleHandle) {
		t.Fatalf("stale generation open err = %v, want ErrStaleHandle", err)
	}
	if _, err := g.Open(h, impl.AccessRead, callerT1()); err != nil {
		t.Fatalf("the fresh handle must still open, got %v", err)
	}
	mutated := h
	mutated.Inode = h.Inode + 1
	if err := g.Verify(mutated); !errors.Is(err, impl.ErrStaleHandle) {
		t.Fatalf("verify with a different inode err = %v, want ErrStaleHandle", err)
	}
}

func TestGrantAllowsCrossNamespaceOpen(t *testing.T) {
	g := guardWithMounts(t)
	h := mustLookup(t, g, "/srv/t-1/data/report.csv")
	mustRegister(t, g, h, ownerT1())
	otherNS := impl.Caller{Tenant: "t-1", PID: 7002, Namespace: "ns-b"}
	if _, err := g.Open(h, impl.AccessRead, otherNS); !errors.Is(err, impl.ErrNamespaceDenied) {
		t.Fatalf("cross namespace open without grant err = %v, want ErrNamespaceDenied", err)
	}
	if err := g.Grant(h, "t-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("grant must succeed, got %v", err)
	}
	if _, err := g.Open(h, impl.AccessRead, otherNS); err != nil {
		t.Fatalf("granted cross namespace open must succeed, got %v", err)
	}
	if got := len(g.Grants("t-1")); got != 1 {
		t.Fatalf("grants = %d, want 1", got)
	}
	expiredHandle := mustLookup(t, g, "/srv/t-1/data/archive.tar")
	mustRegister(t, g, expiredHandle, ownerT1())
	if err := g.Grant(expiredHandle, "t-1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("grant with a past expiry must be recorded, got %v", err)
	}
	if _, err := g.Open(expiredHandle, impl.AccessRead, otherNS); !errors.Is(err, impl.ErrGrantExpired) {
		t.Fatalf("expired grant err = %v, want ErrGrantExpired", err)
	}
}

func TestExportImportKeepsOwnersAndRevocations(t *testing.T) {
	source := guardWithMounts(t)
	h := mustLookup(t, source, "/srv/t-1/data/report.csv")
	mustRegister(t, source, h, ownerT1())
	revoked := mustLookup(t, source, "/srv/t-1/data/old.csv")
	mustRegister(t, source, revoked, ownerT1())
	if err := source.Revoke(revoked); err != nil {
		t.Fatalf("revoke must succeed, got %v", err)
	}
	if err := source.Grant(h, "t-3", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("grant must succeed, got %v", err)
	}
	payload, err := source.Export()
	if err != nil {
		t.Fatalf("export must succeed, got %v", err)
	}
	target := impl.NewFileHandleGuard(fhLimits())
	if err := target.Import(payload); err != nil {
		t.Fatalf("import must succeed, got %v", err)
	}
	if _, err := target.Open(h, impl.AccessRead, callerT1()); err != nil {
		t.Fatalf("imported owner must allow the open, got %v", err)
	}
	if _, err := target.Open(revoked, impl.AccessRead, callerT1()); !errors.Is(err, impl.ErrRevoked) {
		t.Fatalf("revocation must survive the import, err = %v", err)
	}
	if got := len(target.Grants("t-3")); got != 1 {
		t.Fatalf("imported grants = %d, want 1", got)
	}
	if got := target.OpenCount("t-1"); got != 1 {
		t.Fatalf("imported open count = %d, want 1", got)
	}
}
