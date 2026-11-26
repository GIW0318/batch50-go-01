package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-09"
)

func guardLimits() impl.GuardLimits {
	return impl.GuardLimits{MaxGrantsPerOwner: 4, MaxRangeLen: 1024, MaxAudit: 32}
}

func memDevice(offset, length int64) impl.Device {
	return impl.Device{
		Node:      "/dev/mem",
		Kind:      impl.KindMem,
		Base:      0,
		Size:      4096,
		Offset:    offset,
		Length:    length,
		HostInode: 771,
	}
}

func caller(ticket string, caps ...string) impl.Caller {
	return impl.Caller{Owner: "job-42", Group: "/svc/db", Ticket: ticket, Caps: caps}
}

func newGuard(t *testing.T) *impl.DevmemAccessGuard {
	t.Helper()
	g := impl.NewDevmemAccessGuard(guardLimits())
	if err := g.AddDevice(memDevice(0, 0)); err != nil {
		t.Fatalf("add /dev/mem: %v", err)
	}
	if err := g.SetNow(100); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	return g
}

func withWindow(t *testing.T, g *impl.DevmemAccessGuard) {
	t.Helper()
	if err := g.SetWindow("job-42", 50, 150); err != nil {
		t.Fatalf("set window: %v", err)
	}
}

func TestAuthorizeBuildsGrantWithinWindow(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1001", impl.CapRawIO), memDevice(1024, 512), impl.ReadWrite)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if grant.ID == "" {
		t.Fatalf("grant id must not be empty")
	}
	if grant.Owner != "job-42" || grant.Group != "/svc/db" || grant.Ticket != "OPS-1001" {
		t.Fatalf("grant identity = %+v, want the caller identity and ticket", grant)
	}
	if grant.Device != "/dev/mem" {
		t.Fatalf("grant device = %q, want /dev/mem", grant.Device)
	}
	if grant.Offset != 1024 || grant.Length != 512 {
		t.Fatalf("grant range = [%d,%d], want [1024,512]", grant.Offset, grant.Length)
	}
	if grant.RW != impl.ReadWrite {
		t.Fatalf("grant mode = %d, want %d", grant.RW, impl.ReadWrite)
	}
	if grant.ExpiresAt != 150 {
		t.Fatalf("grant expiry = %d, want the window close 150", grant.ExpiresAt)
	}
	if grant.Generation != 0 || grant.Consumed {
		t.Fatalf("fresh grant = %+v, want generation 0 and not consumed", grant)
	}
	audit := g.Audit()
	if len(audit) != 1 {
		t.Fatalf("audit = %+v, want one granted record", audit)
	}
	rec := audit[0]
	if rec.Phase != impl.PhaseGranted || rec.Owner != "job-42" || rec.Group != "/svc/db" {
		t.Fatalf("audit rec = %+v, want a granted record for job-42", rec)
	}
	if rec.Ticket != "OPS-1001" || rec.Offset != 1024 || rec.Length != 512 {
		t.Fatalf("audit rec = %+v, want the ticket and the granted range", rec)
	}
}

func TestAuthorizeRequiresCapability(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	if _, err := g.Authorize(caller("OPS-1002", impl.CapPortIO), memDevice(0, 64), impl.ReadOnly); !errors.Is(err, impl.ErrNoCapability) {
		t.Fatalf("a caller without cap_rawio must fail with ErrNoCapability, got %v", err)
	}
	if _, err := g.Authorize(caller("OPS-1003", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly); err != nil {
		t.Fatalf("a caller holding cap_rawio must be allowed: %v", err)
	}
	if _, err := g.Authorize(caller("  ", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly); !errors.Is(err, impl.ErrNoTicket) {
		t.Fatalf("a blank ticket must fail with ErrNoTicket, got %v", err)
	}
}

func TestAuthorizeRejectsClosedWindow(t *testing.T) {
	g := newGuard(t)
	if _, err := g.Authorize(caller("OPS-1004", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly); !errors.Is(err, impl.ErrWindowClosed) {
		t.Fatalf("a caller without a window must fail with ErrWindowClosed, got %v", err)
	}
	withWindow(t, g)
	if err := g.SetNow(200); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	if _, err := g.Authorize(caller("OPS-1005", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly); !errors.Is(err, impl.ErrWindowClosed) {
		t.Fatalf("a caller past the window must fail with ErrWindowClosed, got %v", err)
	}
	if err := g.SetWindow("job-42", 300, 300); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("an empty window must fail with ErrBadRequest, got %v", err)
	}
}

func TestAuthorizeRejectsRangeOutsideDevice(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	if _, err := g.Authorize(caller("OPS-1006", impl.CapRawIO), memDevice(4000, 512), impl.ReadWrite); !errors.Is(err, impl.ErrOutOfRange) {
		t.Fatalf("a range running past the device must fail with ErrOutOfRange, got %v", err)
	}
	if _, err := g.Authorize(caller("OPS-1007", impl.CapRawIO), memDevice(-1, 64), impl.ReadWrite); !errors.Is(err, impl.ErrOutOfRange) {
		t.Fatalf("a negative offset must fail with ErrOutOfRange, got %v", err)
	}
	if _, err := g.Authorize(caller("OPS-1008", impl.CapRawIO), memDevice(0, 0), impl.ReadWrite); !errors.Is(err, impl.ErrOutOfRange) {
		t.Fatalf("a zero length range must fail with ErrOutOfRange, got %v", err)
	}
	if _, err := g.Authorize(caller("OPS-1009", impl.CapRawIO), memDevice(64, 64), impl.ReadOnly); err != nil {
		t.Fatalf("a range inside the device must be allowed: %v", err)
	}
}

func TestAuthorizeEnforcesRangeLengthCap(t *testing.T) {
	g := impl.NewDevmemAccessGuard(impl.GuardLimits{MaxGrantsPerOwner: 4, MaxRangeLen: 300, MaxAudit: 32})
	if err := g.AddDevice(memDevice(0, 0)); err != nil {
		t.Fatalf("add device: %v", err)
	}
	if err := g.SetNow(100); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	withWindow(t, g)
	if _, err := g.Authorize(caller("OPS-1010", impl.CapRawIO), memDevice(0, 301), impl.ReadOnly); !errors.Is(err, impl.ErrRangeTooLong) {
		t.Fatalf("301 bytes with a 300 byte cap must fail with ErrRangeTooLong, got %v", err)
	}
	if _, err := g.Authorize(caller("OPS-1011", impl.CapRawIO), memDevice(0, 300), impl.ReadOnly); err != nil {
		t.Fatalf("a range of exactly the cap must be allowed: %v", err)
	}
}

func TestAuthorizeEnforcesGrantCapPerOwner(t *testing.T) {
	g := impl.NewDevmemAccessGuard(impl.GuardLimits{MaxGrantsPerOwner: 2, MaxRangeLen: 1024, MaxAudit: 32})
	if err := g.AddDevice(memDevice(0, 0)); err != nil {
		t.Fatalf("add device: %v", err)
	}
	if err := g.SetNow(100); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	withWindow(t, g)
	for i := 0; i < 2; i++ {
		if _, err := g.Authorize(caller("OPS-11", impl.CapRawIO), memDevice(int64(i*64), 64), impl.ReadOnly); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if _, err := g.Authorize(caller("OPS-12", impl.CapRawIO), memDevice(256, 64), impl.ReadOnly); !errors.Is(err, impl.ErrGrantLimit) {
		t.Fatalf("a third grant with a cap of two must fail with ErrGrantLimit, got %v", err)
	}
	if got := len(g.Grants("job-42")); got != 2 {
		t.Fatalf("grants = %d, want the two accepted ones", got)
	}
}

func TestOpenRecordsAuditTrail(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1013", impl.CapRawIO), memDevice(512, 128), impl.ReadWrite)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	file, err := g.Open(grant.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if file.FD != 3 {
		t.Fatalf("first fd = %d, want 3", file.FD)
	}
	if file.GrantID != grant.ID || file.Owner != "job-42" || file.Device != "/dev/mem" {
		t.Fatalf("file = %+v, want it bound to the grant", file)
	}
	if file.Offset != 512 || file.Length != 128 || file.RW != impl.ReadWrite {
		t.Fatalf("file range = %+v, want the granted range and mode", file)
	}
	if file.State != impl.FileOpen {
		t.Fatalf("file state = %q, want %q", file.State, impl.FileOpen)
	}
	audit := g.Audit()
	if len(audit) != 2 {
		t.Fatalf("audit = %+v, want granted and opened records", audit)
	}
	if audit[0].Phase != impl.PhaseGranted || audit[1].Phase != impl.PhaseOpened {
		t.Fatalf("audit phases = %q/%q, want granted then opened", audit[0].Phase, audit[1].Phase)
	}
	if audit[1].Seq <= audit[0].Seq {
		t.Fatalf("audit sequence must increase, got %d then %d", audit[0].Seq, audit[1].Seq)
	}
	if audit[1].Owner != "job-42" || audit[1].Group != "/svc/db" || audit[1].Ticket != "OPS-1013" {
		t.Fatalf("opened record = %+v, want the caller identity and ticket", audit[1])
	}
	if audit[1].Offset != 512 || audit[1].Length != 128 {
		t.Fatalf("opened record range = %+v, want the granted range", audit[1])
	}
}

func TestOpenRejectsUnknownGrant(t *testing.T) {
	g := newGuard(t)
	if _, err := g.Open("grant-404"); !errors.Is(err, impl.ErrUnknownGrant) {
		t.Fatalf("an unknown grant must fail with ErrUnknownGrant, got %v", err)
	}
	if _, err := g.Open(""); !errors.Is(err, impl.ErrUnknownGrant) {
		t.Fatalf("an empty grant id must fail with ErrUnknownGrant, got %v", err)
	}
	if got := g.Files("job-42"); len(got) != 0 {
		t.Fatalf("a rejected open must not create a file, got %+v", got)
	}
}

func TestOpenIsSingleUsePerGrant(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1014", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := g.Open(grant.ID); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := g.Open(grant.ID); !errors.Is(err, impl.ErrGrantConsumed) {
		t.Fatalf("a second open of the same grant must fail with ErrGrantConsumed, got %v", err)
	}
	if got := g.Files("job-42"); len(got) != 1 {
		t.Fatalf("files = %+v, want exactly one descriptor", got)
	}
}

func TestAuditFullBlocksOpen(t *testing.T) {
	g := impl.NewDevmemAccessGuard(impl.GuardLimits{MaxGrantsPerOwner: 4, MaxRangeLen: 1024, MaxAudit: 3})
	if err := g.AddDevice(memDevice(0, 0)); err != nil {
		t.Fatalf("add device: %v", err)
	}
	if err := g.SetNow(100); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	withWindow(t, g)
	var first impl.Grant
	for i := 0; i < 3; i++ {
		grant, err := g.Authorize(caller("OPS-1015", impl.CapRawIO), memDevice(int64(i*64), 64), impl.ReadOnly)
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		if i == 0 {
			first = grant
		}
	}
	if _, err := g.Open(first.ID); !errors.Is(err, impl.ErrAuditFull) {
		t.Fatalf("a full audit buffer must block the open with ErrAuditFull, got %v", err)
	}
	if got := len(g.Audit()); got != 3 {
		t.Fatalf("audit records = %d, want the buffer left untouched at 3", got)
	}
	if got := g.Files("job-42"); len(got) != 0 {
		t.Fatalf("a blocked open must not create a descriptor, got %+v", got)
	}
}

func TestRevokeFlagsInFlightFilesForReclaim(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1016", impl.CapRawIO), memDevice(0, 64), impl.ReadWrite)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	file, err := g.Open(grant.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := g.Revoke("job-42"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	files := g.Files("job-42")
	if len(files) != 1 || files[0].State != impl.FileQuarantined {
		t.Fatalf("files after revoke = %+v, want one quarantined descriptor", files)
	}
	if queued := g.Quarantined(); len(queued) != 1 || queued[0].FD != file.FD {
		t.Fatalf("quarantined = %+v, want the in flight descriptor", queued)
	}
	if _, err := g.Open(grant.ID); !errors.Is(err, impl.ErrGrantRevoked) {
		t.Fatalf("opening a revoked grant must fail with ErrGrantRevoked, got %v", err)
	}
	fresh, err := g.Authorize(caller("OPS-1017", impl.CapRawIO), memDevice(0, 64), impl.ReadWrite)
	if err != nil {
		t.Fatalf("authorize after revoke: %v", err)
	}
	if fresh.Generation != grant.Generation+1 {
		t.Fatalf("generation = %d, want the revoked generation %d plus one", fresh.Generation, grant.Generation)
	}
	next, err := g.Open(fresh.ID)
	if err != nil {
		t.Fatalf("open after revoke: %v", err)
	}
	if next.FD != file.FD+1 {
		t.Fatalf("next fd = %d, want %d", next.FD, file.FD+1)
	}
	if err := g.Reclaim(file.FD); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if got := len(g.Quarantined()); got != 0 {
		t.Fatalf("quarantined after reclaim = %d, want 0", got)
	}
}

func TestRevokeRejectsUnknownOwner(t *testing.T) {
	g := newGuard(t)
	if err := g.Revoke("job-nobody"); !errors.Is(err, impl.ErrUnknownOwner) {
		t.Fatalf("revoking an owner without grants must fail with ErrUnknownOwner, got %v", err)
	}
}

func TestCloseReleasesFileDescriptor(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1018", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	file, err := g.Open(grant.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := g.Close(file.FD); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := g.Files("job-42"); len(got) != 0 {
		t.Fatalf("files after close = %+v, want none", got)
	}
	if err := g.Close(file.FD); !errors.Is(err, impl.ErrUnknownFile) {
		t.Fatalf("closing a closed descriptor must fail with ErrUnknownFile, got %v", err)
	}
}

func TestConcurrentOpenOnlyOneSucceeds(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1019", impl.CapRawIO), memDevice(0, 64), impl.ReadOnly)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	opened, consumed, other := 0, 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.Open(grant.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				opened++
			case errors.Is(err, impl.ErrGrantConsumed):
				consumed++
			default:
				other++
			}
		}()
	}
	wg.Wait()
	if opened != 1 || consumed != 7 || other != 0 {
		t.Fatalf("concurrent opens got opened=%d consumed=%d other=%d, want 1/7/0", opened, consumed, other)
	}
	if got := g.Files("job-42"); len(got) != 1 {
		t.Fatalf("files = %+v, want a single descriptor", got)
	}
}

func TestSnapshotRestoreKeepsRevocation(t *testing.T) {
	g := newGuard(t)
	withWindow(t, g)
	grant, err := g.Authorize(caller("OPS-1020", impl.CapRawIO), memDevice(0, 64), impl.ReadWrite)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := g.Open(grant.ID); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := g.Revoke("job-42"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewDevmemAccessGuard(guardLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := restored.Open(grant.ID); !errors.Is(err, impl.ErrGrantRevoked) {
		t.Fatalf("a restored guard must keep the grant revoked, got %v", err)
	}
	files := restored.Files("job-42")
	if len(files) != 1 || files[0].State != impl.FileQuarantined {
		t.Fatalf("restored files = %+v, want the quarantined descriptor", files)
	}
	grants := restored.Grants("job-42")
	if len(grants) != 1 || grants[0].Generation != grant.Generation {
		t.Fatalf("restored grants = %+v, want the original generation", grants)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
}
