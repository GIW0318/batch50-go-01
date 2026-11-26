package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-11"
)

func wardenLimits() impl.Limits {
	return impl.Limits{
		MaxProcessesPerTenant: 4,
		MaxLeasesPerProcess:   2,
		MaxLeaseBytes:         1 << 16,
		MaxRegionsPerProcess:  64,
		LeaseGrace:            5 * time.Second,
		Now:                   func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func jitProfile() impl.Profile {
	return impl.Profile{Tenant: "tenant-a", Name: "jvm-service", AllowJit: true}
}

func anonWxMapping() impl.Mapping {
	return impl.Mapping{
		Start:      0x7f0000100000,
		End:        0x7f0000110000,
		Writable:   true,
		Executable: true,
		Anonymous:  true,
		Backing:    "anon:jit-code-cache",
	}
}

func TestRegisterRejectsBlankProfile(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	_, err := w.Register(101, impl.Profile{Tenant: "", Name: "worker"})
	if !errors.Is(err, impl.ErrBlankProfile) {
		t.Fatalf("register with blank tenant = %v, want ErrBlankProfile", err)
	}
}

func TestRegisterRejectsDuplicatePID(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	if _, err := w.Register(202, jitProfile()); err != nil {
		t.Fatalf("first register must succeed, got %v", err)
	}
	if _, err := w.Register(202, jitProfile()); !errors.Is(err, impl.ErrProcessExists) {
		t.Fatalf("second register of the same pid = %v, want ErrProcessExists", err)
	}
}

func TestRegisterEnforcesTenantProcessLimit(t *testing.T) {
	limits := wardenLimits()
	limits.MaxProcessesPerTenant = 2
	w := impl.NewWxRegionWarden(limits)
	for _, pid := range []int{301, 302} {
		if _, err := w.Register(pid, jitProfile()); err != nil {
			t.Fatalf("register pid %d must succeed, got %v", pid, err)
		}
	}
	if _, err := w.Register(303, jitProfile()); !errors.Is(err, impl.ErrTenantProcessLimit) {
		t.Fatalf("third process of the tenant = %v, want ErrTenantProcessLimit", err)
	}
}

func TestRegisterReturnsHandleWithProcessIdentity(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(404, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if h.PID != 404 {
		t.Fatalf("handle pid = %d, want 404", h.PID)
	}
	if h.Tenant != "tenant-a" {
		t.Fatalf("handle tenant = %q, want tenant-a", h.Tenant)
	}
	if h.Seq == 0 {
		t.Fatalf("handle sequence = %d, want a positive registration number", h.Seq)
	}
}

func TestFeedRejectsInvertedRange(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(505, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	n, err := w.Feed(h, []impl.Mapping{{Start: 0x9000, End: 0x1000, Writable: true, Executable: true, Anonymous: true}})
	if !errors.Is(err, impl.ErrBadMapping) {
		t.Fatalf("feed inverted range = %v, want ErrBadMapping", err)
	}
	if n != 0 {
		t.Fatalf("feed accepted %d mappings, want 0", n)
	}
}

func TestScanReportsWritableExecutableAnonymousRegion(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(606, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if _, err := w.Feed(h, []impl.Mapping{anonWxMapping()}); err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	rep, err := w.Scan(h)
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if len(rep.Verdicts) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(rep.Verdicts))
	}
	v := rep.Verdicts[0]
	if v.Action != impl.ActionAlert {
		t.Fatalf("action = %q, want %q", v.Action, impl.ActionAlert)
	}
	if v.Reason != impl.ReasonAnonWX {
		t.Fatalf("reason = %q, want %q", v.Reason, impl.ReasonAnonWX)
	}
	if rep.Alerts != 1 || rep.Restricted != 0 {
		t.Fatalf("alerts = %d restricted = %d, want 1 and 0", rep.Alerts, rep.Restricted)
	}
	if len(rep.Regions) != 1 {
		t.Fatalf("regions = %d, want 1", len(rep.Regions))
	}
	if rep.Regions[0].Start != anonWxMapping().Start {
		t.Fatalf("region start = %#x, want %#x", rep.Regions[0].Start, anonWxMapping().Start)
	}
	if rep.Regions[0].Level != impl.LevelAlert {
		t.Fatalf("region level = %d, want %d", rep.Regions[0].Level, impl.LevelAlert)
	}
}

func TestScanIgnoresMappingsThatAreNotWritableAndExecutable(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(707, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	maps := []impl.Mapping{
		{Start: 0x1000, End: 0x2000, Writable: true, Executable: false, Anonymous: true},
		{Start: 0x3000, End: 0x4000, Writable: false, Executable: true, Backing: "file:/usr/lib/libc.so"},
	}
	if _, err := w.Feed(h, maps); err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	rep, err := w.Scan(h)
	if err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	if len(rep.Verdicts) != 0 || len(rep.Regions) != 0 {
		t.Fatalf("verdicts = %d regions = %d, want 0 and 0", len(rep.Verdicts), len(rep.Regions))
	}
	if rep.Observed != 0 || rep.Alerts != 0 || rep.Restricted != 0 {
		t.Fatalf("counters = %d/%d/%d, want all zero", rep.Observed, rep.Alerts, rep.Restricted)
	}
}

func TestScanKeepsSeverityAcrossScans(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(808, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if _, err := w.Feed(h, []impl.Mapping{anonWxMapping()}); err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	first, err := w.Scan(h)
	if err != nil {
		t.Fatalf("first scan must succeed, got %v", err)
	}
	if len(first.Regions) != 1 {
		t.Fatalf("regions = %d, want 1", len(first.Regions))
	}
	regionID := first.Regions[0].ID
	action, _, err := w.Restrict(h, regionID)
	if err != nil {
		t.Fatalf("restrict must succeed, got %v", err)
	}
	if action != impl.ActionRestrict {
		t.Fatalf("restricted action = %q, want %q", action, impl.ActionRestrict)
	}
	if _, err := w.Feed(h, []impl.Mapping{anonWxMapping()}); err != nil {
		t.Fatalf("second feed must succeed, got %v", err)
	}
	second, err := w.Scan(h)
	if err != nil {
		t.Fatalf("second scan must succeed, got %v", err)
	}
	if len(second.Regions) != 1 || second.Regions[0].Level != impl.LevelRestrict {
		t.Fatalf("stored level after restrict = %+v, want level %d", second.Regions, impl.LevelRestrict)
	}
	action, _, err = w.Disposition(h, regionID)
	if err != nil {
		t.Fatalf("disposition must succeed, got %v", err)
	}
	if action != impl.ActionRestrict {
		t.Fatalf("disposition = %q, want %q", action, impl.ActionRestrict)
	}
	if second.Restricted != 1 {
		t.Fatalf("restricted count = %d, want 1", second.Restricted)
	}
}

func TestGrantJitLeaseRejectsProfileWithoutJit(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	profile := jitProfile()
	profile.AllowJit = false
	h, err := w.Register(909, profile)
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if _, err := w.GrantJitLease(h, 4096, time.Minute); !errors.Is(err, impl.ErrJitNotAllowed) {
		t.Fatalf("lease without jit permission = %v, want ErrJitNotAllowed", err)
	}
}

func TestGrantJitLeaseEnforcesPerProcessLimit(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(1010, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := w.GrantJitLease(h, 4096, time.Minute); err != nil {
			t.Fatalf("lease %d must succeed, got %v", i, err)
		}
	}
	if _, err := w.GrantJitLease(h, 4096, time.Minute); !errors.Is(err, impl.ErrLeaseLimit) {
		t.Fatalf("third lease = %v, want ErrLeaseLimit", err)
	}
}

func TestGrantJitLeaseRejectsOversize(t *testing.T) {
	limits := wardenLimits()
	limits.MaxLeaseBytes = 4096
	w := impl.NewWxRegionWarden(limits)
	h, err := w.Register(1111, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if _, err := w.GrantJitLease(h, 8192, time.Minute); !errors.Is(err, impl.ErrLeaseTooLarge) {
		t.Fatalf("oversize lease = %v, want ErrLeaseTooLarge", err)
	}
	if _, err := w.GrantJitLease(h, 4096, time.Minute); err != nil {
		t.Fatalf("lease at the cap must succeed, got %v", err)
	}
}

func TestLeaseStateReportsExpiry(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	now := base
	limits := wardenLimits()
	limits.Now = func() time.Time { return now }
	w := impl.NewWxRegionWarden(limits)
	h, err := w.Register(1212, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	lease, err := w.GrantJitLease(h, 4096, 30*time.Second)
	if err != nil {
		t.Fatalf("grant must succeed, got %v", err)
	}
	if lease.State != impl.LeaseActive {
		t.Fatalf("fresh lease state = %q, want %q", lease.State, impl.LeaseActive)
	}
	if !lease.ExpiresAt.Equal(base.Add(30 * time.Second)) {
		t.Fatalf("lease expiry = %v, want %v", lease.ExpiresAt, base.Add(30*time.Second))
	}
	now = base.Add(31 * time.Second)
	got, err := w.LeaseState(lease.ID)
	if err != nil {
		t.Fatalf("lease state must be readable, got %v", err)
	}
	if got.State != impl.LeaseExpired {
		t.Fatalf("expired lease state = %q, want %q", got.State, impl.LeaseExpired)
	}
}

func TestReleaseLeaseTwiceReturnsLeaseReleased(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	h, err := w.Register(1313, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	lease, err := w.GrantJitLease(h, 2048, time.Minute)
	if err != nil {
		t.Fatalf("grant must succeed, got %v", err)
	}
	if err := w.ReleaseLease(lease.ID); err != nil {
		t.Fatalf("release must succeed, got %v", err)
	}
	if err := w.ReleaseLease(lease.ID); !errors.Is(err, impl.ErrLeaseReleased) {
		t.Fatalf("second release = %v, want ErrLeaseReleased", err)
	}
	if _, err := w.LeaseState("lease-that-never-existed"); !errors.Is(err, impl.ErrUnknownLease) {
		t.Fatalf("unknown lease state = %v, want ErrUnknownLease", err)
	}
}

func TestSnapshotRestoreKeepsLeaseAndRegions(t *testing.T) {
	limits := wardenLimits()
	w := impl.NewWxRegionWarden(limits)
	h, err := w.Register(1414, jitProfile())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	lease, err := w.GrantJitLease(h, 8192, time.Hour)
	if err != nil {
		t.Fatalf("grant must succeed, got %v", err)
	}
	if _, err := w.Feed(h, []impl.Mapping{anonWxMapping()}); err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	if _, err := w.Scan(h); err != nil {
		t.Fatalf("scan must succeed, got %v", err)
	}
	data, err := w.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload is empty")
	}
	restored := impl.NewWxRegionWarden(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	got, err := restored.LeaseState(lease.ID)
	if err != nil {
		t.Fatalf("restored lease must be readable, got %v", err)
	}
	if !got.ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("restored expiry = %v, want %v", got.ExpiresAt, lease.ExpiresAt)
	}
	if got.State != impl.LeaseActive {
		t.Fatalf("restored lease state = %q, want %q", got.State, impl.LeaseActive)
	}
	regions := restored.Regions(h)
	if len(regions) != 1 {
		t.Fatalf("restored regions = %d, want 1", len(regions))
	}
	if regions[0].Level != impl.LevelAlert {
		t.Fatalf("restored region level = %d, want %d", regions[0].Level, impl.LevelAlert)
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	w := impl.NewWxRegionWarden(wardenLimits())
	if err := w.Restore([]byte("this is not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("restore garbage = %v, want ErrBadSnapshot", err)
	}
}
