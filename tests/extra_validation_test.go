package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-11"
)

func extraLimits(now func() time.Time) impl.Limits {
	return impl.Limits{
		MaxProcessesPerTenant: 8,
		MaxLeasesPerProcess:   2,
		MaxLeaseBytes:         1 << 20,
		MaxRegionsPerProcess:  64,
		LeaseGrace:            5 * time.Second,
		Now:                   now,
	}
}

func TestExtraJitLeaseMustCoverRegion(t *testing.T) {
	w := impl.NewWxRegionWarden(extraLimits(nil))
	h, _ := w.Register(2001, impl.Profile{Tenant: "t", Name: "jvm", AllowJit: true})
	if _, err := w.GrantJitLease(h, 1<<16, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Feed(h, []impl.Mapping{{Start: 0x1000, End: 0x11000, Writable: true, Executable: true, Anonymous: true, Backing: "jit"}}); err != nil {
		t.Fatal(err)
	}
	r, _ := w.Scan(h)
	if len(r.Verdicts) != 1 || r.Verdicts[0].Action != impl.ActionObserve || r.Verdicts[0].Reason != impl.ReasonJitLeaseActive {
		t.Fatalf("lease-covered scan = %+v", r.Verdicts)
	}
}

func TestExtraAliasMappingsMergeToOneRegion(t *testing.T) {
	w := impl.NewWxRegionWarden(extraLimits(nil))
	h, _ := w.Register(2002, impl.Profile{Tenant: "t", Name: "jvm", AllowJit: true})
	maps := []impl.Mapping{
		{Start: 0x1000, End: 0x2000, Writable: true, Anonymous: true, Backing: "same-page"},
		{Start: 0x9000, End: 0xa000, Executable: true, Anonymous: true, Backing: "same-page"},
	}
	if n, err := w.Feed(h, maps); err != nil || n != 2 {
		t.Fatalf("feed = %d, %v", n, err)
	}
	r, _ := w.Scan(h)
	if len(r.Verdicts) != 1 || r.Verdicts[0].Action != impl.ActionAlert {
		t.Fatalf("alias scan = %+v", r.Verdicts)
	}
	if !r.Regions[0].Writable || !r.Regions[0].Executable {
		t.Fatalf("alias region flags = %+v", r.Regions[0])
	}
}

func TestExtraRestrictDeniedByCoveringLeaseAndOwnership(t *testing.T) {
	w := impl.NewWxRegionWarden(extraLimits(nil))
	a, _ := w.Register(2003, impl.Profile{Tenant: "t", Name: "a", AllowJit: true})
	b, _ := w.Register(2004, impl.Profile{Tenant: "t", Name: "b", AllowJit: true})
	m := impl.Mapping{Start: 0x1000, End: 0x11000, Writable: true, Executable: true, Anonymous: true, Backing: "jit"}
	w.Feed(a, []impl.Mapping{m})
	w.Feed(b, []impl.Mapping{m})
	w.GrantJitLease(a, 1<<16, time.Minute)
	ra, _ := w.Scan(a)
	w.Scan(b)
	if _, reason, err := w.Restrict(a, ra.Regions[0].ID); err != nil || reason != impl.ReasonRestrictDenied {
		t.Fatalf("active lease restrict = %q, %v", reason, err)
	}
	if _, reason, err := w.Restrict(b, ra.Regions[0].ID); err != nil || reason != impl.ReasonRegionNotOwned {
		t.Fatalf("foreign region restrict = %q, %v", reason, err)
	}
	if _, _, err := w.Restrict(b, "missing"); !errors.Is(err, impl.ErrUnknownRegion) {
		t.Fatalf("missing region = %v", err)
	}
}

func TestExtraExpiryGraceAndMonotonicLevel(t *testing.T) {
	base := time.Unix(1700000000, 0)
	now := base
	w := impl.NewWxRegionWarden(extraLimits(func() time.Time { return now }))
	h, _ := w.Register(2005, impl.Profile{Tenant: "t", Name: "jvm", AllowJit: true})
	lease, _ := w.GrantJitLease(h, 1<<16, 30*time.Second)
	m := impl.Mapping{Start: 0x1000, End: 0x11000, Writable: true, Executable: true, Anonymous: true, Backing: "jit"}
	w.Feed(h, []impl.Mapping{m})
	first, _ := w.Scan(h)
	if first.Verdicts[0].Level != impl.LevelObserve {
		t.Fatalf("active lease scan = %+v", first.Verdicts)
	}
	id := first.Regions[0].ID
	now = base.Add(31 * time.Second)
	if got, _ := w.LeaseState(lease.ID); got.State != impl.LeaseExpired {
		t.Fatalf("expired state = %q", got.State)
	}
	grace, _ := w.Scan(h)
	if grace.Verdicts[0].Reason != impl.ReasonLeaseGrace || grace.Verdicts[0].Action != impl.ActionObserve {
		t.Fatalf("grace scan = %+v", grace.Verdicts)
	}
	again, _ := w.Scan(h)
	if again.Verdicts[0].Reason != impl.ReasonLeaseGrace {
		t.Fatalf("grace should be stable = %+v", again.Verdicts)
	}
	if _, reason, err := w.Restrict(h, id); err != nil || reason != impl.ReasonLeaseGrace {
		t.Fatalf("grace restrict = %q, %v", reason, err)
	}
	now = base.Add(36 * time.Second)
	after, _ := w.Scan(h)
	if after.Verdicts[0].Action != impl.ActionAlert {
		t.Fatalf("post-grace scan = %+v", after.Verdicts)
	}
	if _, reason, err := w.Restrict(h, id); err != nil || reason != impl.ReasonRestrictApplied {
		t.Fatalf("post-grace restrict = %q, %v", reason, err)
	}
	w.Feed(h, []impl.Mapping{{Start: 0x1000, End: 0x11000, Writable: true, Executable: true, Anonymous: false, Backing: "clean"}})
	clean, _ := w.Scan(h)
	if clean.Verdicts[0].Level != impl.LevelRestrict {
		t.Fatalf("severity regressed = %+v", clean.Verdicts)
	}
}

func TestExtraFeedAtomicityAndHandleErrors(t *testing.T) {
	w := impl.NewWxRegionWarden(extraLimits(nil))
	h, _ := w.Register(2006, impl.Profile{Tenant: "t", Name: "jvm"})
	good := []impl.Mapping{{Start: 0x1000, End: 0x2000, Writable: true, Executable: true, Anonymous: true, Backing: "good"}}
	if _, err := w.Feed(h, good); err != nil {
		t.Fatal(err)
	}
	first, _ := w.Scan(h)
	if _, err := w.Feed(h, []impl.Mapping{good[0], {Start: 0x3000, End: 0x3000}}); !errors.Is(err, impl.ErrBadMapping) {
		t.Fatalf("bad batch = %v", err)
	}
	second, _ := w.Scan(h)
	if len(second.Regions) != 1 || second.Regions[0].ID != first.Regions[0].ID {
		t.Fatalf("feed wrote partial table: %+v", second.Regions)
	}
	if _, _, err := w.Disposition(impl.Handle{PID: 999, Tenant: "t", Seq: 99}, first.Regions[0].ID); !errors.Is(err, impl.ErrUnknownHandle) {
		t.Fatalf("unknown handle = %v", err)
	}
}

func TestExtraForkIsolationAndSnapshotHandle(t *testing.T) {
	now := time.Unix(1700000000, 0)
	w := impl.NewWxRegionWarden(extraLimits(func() time.Time { return now }))
	parent, _ := w.Register(2010, impl.Profile{Tenant: "t", Name: "parent", AllowJit: true})
	child, _ := w.Register(2011, impl.Profile{Tenant: "t", Name: "child", AllowJit: true})
	m := impl.Mapping{Start: 0x1000, End: 0x11000, Writable: true, Executable: true, Anonymous: true, Backing: "forked-jit"}
	w.Feed(parent, []impl.Mapping{m})
	w.Feed(child, []impl.Mapping{m})
	parentReport, _ := w.Scan(parent)
	w.Restrict(parent, parentReport.Regions[0].ID)
	w.GrantJitLease(child, 1<<16, time.Hour)
	childReport, _ := w.Scan(child)
	if childReport.Verdicts[0].Action != impl.ActionObserve || childReport.Verdicts[0].Reason != impl.ReasonJitLeaseActive {
		t.Fatalf("parent restriction leaked to child: %+v", childReport.Verdicts)
	}

	data, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored := impl.NewWxRegionWarden(extraLimits(func() time.Time { return now }))
	if err := restored.Restore(data); err != nil {
		t.Fatal(err)
	}
	if got := restored.Regions(child); len(got) != 1 || got[0].Level != impl.LevelObserve {
		t.Fatalf("restored child regions = %+v", got)
	}
	if got := restored.Regions(impl.Handle{PID: parent.PID, Tenant: parent.Tenant}); len(got) != 1 || got[0].Level != impl.LevelRestrict {
		t.Fatalf("restored parent lookup by pid/tenant = %+v", got)
	}
	if err := restored.Restore([]byte(`{"version":2}`)); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("bad restore = %v", err)
	}
	if got := restored.Regions(child); len(got) != 1 {
		t.Fatalf("failed restore mutated state: %+v", got)
	}
}

func TestExtraConcurrentFeedScanSerialized(t *testing.T) {
	w := impl.NewWxRegionWarden(extraLimits(nil))
	h, _ := w.Register(2020, impl.Profile{Tenant: "t", Name: "jvm", AllowJit: true})
	a := []impl.Mapping{{Start: 0x1000, End: 0x12000, Writable: true, Executable: true, Anonymous: true, Backing: "a"}}
	b := []impl.Mapping{{Start: 0x13000, End: 0x24000, Writable: true, Executable: true, Anonymous: true, Backing: "b"}}

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if (worker+i)%2 == 0 {
					w.Feed(h, a)
				} else {
					w.Feed(h, b)
				}
				if _, err := w.Scan(h); err != nil {
					t.Errorf("scan: %v", err)
					return
				}
				if _, err := w.GrantJitLease(h, 1<<16, time.Second); err != nil && !errors.Is(err, impl.ErrLeaseLimit) {
					t.Errorf("grant: %v", err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()

	regions := w.Regions(h)
	if len(regions) != 1 {
		t.Fatalf("half mapping table exposed: %+v", regions)
	}
	if regions[0].Start != 0x1000 && regions[0].Start != 0x13000 {
		t.Fatalf("unexpected concurrent region: %+v", regions[0])
	}
}
