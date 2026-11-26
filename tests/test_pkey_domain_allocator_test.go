package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-16"
)

func testConfig() impl.Config {
	return impl.Config{MaxKeys: 8, MaxRegionsPerDomain: 4, MaxPinsPerDomain: 2}
}

func mustDomain(t *testing.T, a *impl.PkeyDomainAllocator, name string, mode impl.PkeyMode) impl.Domain {
	t.Helper()
	d, err := a.NewDomain(name, mode)
	if err != nil {
		t.Fatalf("new domain %s: %v", name, err)
	}
	return d
}

func TestNewDomainAssignsSequentialIDs(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	d1 := mustDomain(t, a, "renderer", impl.ModeShared)
	if d1.ID != "dom-1" {
		t.Fatalf("first domain id = %q, want dom-1", d1.ID)
	}
	if d1.Name != "renderer" || d1.Mode != impl.ModeShared {
		t.Fatalf("domain record = %+v, want renderer/shared", d1)
	}
	if d1.Seq != 1 {
		t.Fatalf("first domain seq = %d, want 1", d1.Seq)
	}
	if len(d1.Keys) != 0 {
		t.Fatalf("fresh domain holds %d keys, want none", len(d1.Keys))
	}
	d2 := mustDomain(t, a, "codec", impl.ModeExclusive)
	if d2.ID != "dom-2" || d2.Seq != 2 {
		t.Fatalf("second domain = %+v, want dom-2 with seq 2", d2)
	}
	all := a.Domains()
	if len(all) != 2 || all[0].ID != "dom-1" || all[1].ID != "dom-2" {
		t.Fatalf("domains = %+v, want creation order dom-1,dom-2", all)
	}
	got, ok := a.Domain("dom-1")
	if !ok || got.Name != "renderer" {
		t.Fatalf("domain lookup = %+v ok=%v, want renderer", got, ok)
	}
	if _, ok := a.Domain("dom-404"); ok {
		t.Fatalf("unknown domain must not be reported as present")
	}
}

func TestNewDomainRejectsBlankNameAndUnknownMode(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	for _, name := range []string{"", "   "} {
		if _, err := a.NewDomain(name, impl.ModeShared); !errors.Is(err, impl.ErrInvalidDomain) {
			t.Fatalf("blank name %q must be rejected with ErrInvalidDomain, got %v", name, err)
		}
	}
	if _, err := a.NewDomain("ok", impl.PkeyMode("kernel")); !errors.Is(err, impl.ErrInvalidDomain) {
		t.Fatalf("unknown mode must be rejected with ErrInvalidDomain, got %v", err)
	}
	if got := len(a.Domains()); got != 0 {
		t.Fatalf("rejected domains left %d records, want 0", got)
	}
}

func TestNewDomainRejectsDuplicateName(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	mustDomain(t, a, "renderer", impl.ModeShared)
	if _, err := a.NewDomain("renderer", impl.ModeExclusive); !errors.Is(err, impl.ErrDomainExists) {
		t.Fatalf("duplicate name must be rejected with ErrDomainExists, got %v", err)
	}
	if got := len(a.Domains()); got != 1 {
		t.Fatalf("duplicate attempt created a domain, count = %d", got)
	}
	d := mustDomain(t, a, "audio", impl.ModeShared)
	if d.ID != "dom-2" {
		t.Fatalf("domain id after a rejected duplicate = %q, want dom-2", d.ID)
	}
}

func TestAllocKeyReturnsLowestFreeKeys(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(impl.Config{MaxKeys: 4, MaxRegionsPerDomain: 4, MaxPinsPerDomain: 2})
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	for i := 1; i <= 4; i++ {
		key, err := a.AllocKey(d.ID)
		if err != nil {
			t.Fatalf("alloc key %d: %v", i, err)
		}
		if int(key) != i {
			t.Fatalf("alloc key %d returned %d, want the slot number %d", i, key, i)
		}
		st, ok := a.PkeyState(key)
		if !ok {
			t.Fatalf("key %d is not tracked after allocation", key)
		}
		if st.DomainID != d.ID {
			t.Fatalf("key %d belongs to %q, want %q", key, st.DomainID, d.ID)
		}
		if st.Rights != impl.RightsRead {
			t.Fatalf("key %d starts with rights %d, want read only", key, st.Rights)
		}
		if st.Version != 1 {
			t.Fatalf("key %d starts at version %d, want 1", key, st.Version)
		}
	}
	got := a.Domains()[0].Keys
	if len(got) != 4 {
		t.Fatalf("domain holds %d keys, want 4", len(got))
	}
	for i, key := range got {
		if int(key) != i+1 {
			t.Fatalf("domain keys = %v, want the ascending slots 1..4", got)
		}
	}
}

func TestAllocKeyExhaustion(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(impl.Config{MaxKeys: 3, MaxRegionsPerDomain: 4, MaxPinsPerDomain: 2})
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	for i := 0; i < 3; i++ {
		if _, err := a.AllocKey(d.ID); err != nil {
			t.Fatalf("alloc key %d: %v", i, err)
		}
	}
	if _, err := a.AllocKey(d.ID); !errors.Is(err, impl.ErrPkeyExhausted) {
		t.Fatalf("fourth allocation must fail with ErrPkeyExhausted, got %v", err)
	}
	if got := len(a.Domains()[0].Keys); got != 3 {
		t.Fatalf("domain holds %d keys after exhaustion, want 3", got)
	}
	if _, ok := a.PkeyState(impl.Pkey(4)); ok {
		t.Fatalf("a key slot beyond the budget must not be tracked")
	}
}

func TestKeyAndPinLookupsRejectUnknown(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	if _, err := a.AllocKey("dom-404"); !errors.Is(err, impl.ErrUnknownDomain) {
		t.Fatalf("alloc on an unknown domain must fail with ErrUnknownDomain, got %v", err)
	}
	if err := a.SetRights(impl.Pkey(9), impl.RightsRead); !errors.Is(err, impl.ErrPkeyUnknown) {
		t.Fatalf("rights on an unallocated key must fail with ErrPkeyUnknown, got %v", err)
	}
	if _, ok := a.PkeyState(impl.Pkey(9)); ok {
		t.Fatalf("unallocated key must not have a state record")
	}
	if _, err := a.PinThread("dom-404"); !errors.Is(err, impl.ErrUnknownDomain) {
		t.Fatalf("pin on an unknown domain must fail with ErrUnknownDomain, got %v", err)
	}
	if err := a.UnpinThread(impl.Pin{ID: "pin-404", DomainID: "dom-404"}); !errors.Is(err, impl.ErrUnknownPin) {
		t.Fatalf("unpin of an unknown pin must fail with ErrUnknownPin, got %v", err)
	}
}

func TestSetRightsValidatesAndBumpsVersion(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	key, err := a.AllocKey(d.ID)
	if err != nil {
		t.Fatalf("alloc key: %v", err)
	}
	if err := a.SetRights(key, impl.Rights(0)); !errors.Is(err, impl.ErrBadRights) {
		t.Fatalf("empty mask must fail with ErrBadRights, got %v", err)
	}
	if err := a.SetRights(key, impl.RightsWrite); !errors.Is(err, impl.ErrBadRights) {
		t.Fatalf("write without read must fail with ErrBadRights, got %v", err)
	}
	if err := a.SetRights(key, impl.RightsFull); err != nil {
		t.Fatalf("full mask must be accepted, got %v", err)
	}
	st, _ := a.PkeyState(key)
	if st.Rights != impl.RightsFull || st.Version != 2 {
		t.Fatalf("state after full mask = %+v, want full rights at version 2", st)
	}
	if err := a.SetRights(key, impl.RightsRead); err != nil {
		t.Fatalf("read only mask must be accepted, got %v", err)
	}
	st, _ = a.PkeyState(key)
	if st.Rights != impl.RightsRead || st.Version != 3 {
		t.Fatalf("state after read only = %+v, want read rights at version 3", st)
	}
}

func TestBindRejectsCrossDomainOverlap(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	a1 := mustDomain(t, a, "renderer", impl.ModeShared)
	b1 := mustDomain(t, a, "codec", impl.ModeShared)
	if err := a.Bind(a1.ID, impl.Region{Start: 0, Len: 100}); err != nil {
		t.Fatalf("bind first region: %v", err)
	}
	if err := a.Bind(a1.ID, impl.Region{Start: 100, Len: 50}); err != nil {
		t.Fatalf("adjacent region must be accepted, got %v", err)
	}
	if err := a.Bind(a1.ID, impl.Region{Start: 50, Len: 100}); !errors.Is(err, impl.ErrRegionOverlap) {
		t.Fatalf("overlap inside one domain must fail with ErrRegionOverlap, got %v", err)
	}
	if err := a.Bind(b1.ID, impl.Region{Start: 50, Len: 10}); !errors.Is(err, impl.ErrDomainViolation) {
		t.Fatalf("overlap with another domain must fail with ErrDomainViolation, got %v", err)
	}
	if err := a.Bind(a1.ID, impl.Region{Start: 300, Len: 0}); !errors.Is(err, impl.ErrBadRegion) {
		t.Fatalf("zero length region must fail with ErrBadRegion, got %v", err)
	}
	if err := a.Bind("dom-404", impl.Region{Start: 0, Len: 10}); !errors.Is(err, impl.ErrUnknownDomain) {
		t.Fatalf("bind on an unknown domain must fail with ErrUnknownDomain, got %v", err)
	}
	if got := len(a.Domains()[0].Regions); got != 2 {
		t.Fatalf("domain keeps %d regions after the rejected binds, want 2", got)
	}
}

func TestBindEnforcesRegionLimit(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(impl.Config{MaxKeys: 8, MaxRegionsPerDomain: 2, MaxPinsPerDomain: 2})
	ex := mustDomain(t, a, "codec", impl.ModeExclusive)
	if err := a.Bind(ex.ID, impl.Region{Start: 0, Len: 10}); err != nil {
		t.Fatalf("first exclusive region: %v", err)
	}
	if err := a.Bind(ex.ID, impl.Region{Start: 20, Len: 10}); !errors.Is(err, impl.ErrRegionLimit) {
		t.Fatalf("exclusive domain must reject a second region with ErrRegionLimit, got %v", err)
	}
	sh := mustDomain(t, a, "renderer", impl.ModeShared)
	for i := uint64(0); i < 2; i++ {
		if err := a.Bind(sh.ID, impl.Region{Start: 1000 + i*10, Len: 10}); err != nil {
			t.Fatalf("shared region %d: %v", i, err)
		}
	}
	if err := a.Bind(sh.ID, impl.Region{Start: 2000, Len: 10}); !errors.Is(err, impl.ErrRegionLimit) {
		t.Fatalf("shared domain must reject the region over the cap with ErrRegionLimit, got %v", err)
	}
}

func TestCheckEnforcesDomainBoundary(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	owner := mustDomain(t, a, "renderer", impl.ModeShared)
	other := mustDomain(t, a, "codec", impl.ModeShared)
	if err := a.Bind(owner.ID, impl.Region{Start: 1000, Len: 512}); err != nil {
		t.Fatalf("bind owned region: %v", err)
	}
	cases := []struct {
		domain string
		region impl.Region
		err    error
	}{
		{owner.ID, impl.Region{Start: 1000, Len: 512}, nil},
		{owner.ID, impl.Region{Start: 1200, Len: 100}, nil},
		{owner.ID, impl.Region{Start: 1500, Len: 100}, impl.ErrDomainViolation},
		{owner.ID, impl.Region{Start: 0, Len: 10}, impl.ErrDomainViolation},
		{other.ID, impl.Region{Start: 1000, Len: 10}, impl.ErrDomainViolation},
		{owner.ID, impl.Region{Start: 0, Len: 0}, impl.ErrBadRegion},
		{"dom-404", impl.Region{Start: 0, Len: 10}, impl.ErrUnknownDomain},
	}
	for _, c := range cases {
		err := a.Check(c.domain, c.region)
		if c.err == nil {
			if err != nil {
				t.Fatalf("check %v on %s = %v, want allowed", c.region, c.domain, err)
			}
			continue
		}
		if !errors.Is(err, c.err) {
			t.Fatalf("check %v on %s = %v, want %v", c.region, c.domain, err, c.err)
		}
	}
}

func TestConcurrentAllocKeyRespectsBudget(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(impl.Config{MaxKeys: 4, MaxRegionsPerDomain: 4, MaxPinsPerDomain: 2})
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := make([]int, 0, 4)
	exhausted := 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := a.AllocKey(d.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got = append(got, int(key))
			case errors.Is(err, impl.ErrPkeyExhausted):
				exhausted++
			}
		}()
	}
	wg.Wait()
	if len(got) != 4 {
		t.Fatalf("concurrent allocation handed out %d keys %v, want exactly 4", len(got), got)
	}
	if exhausted != 20 {
		t.Fatalf("concurrent allocation reported %d exhausted errors, want 20", exhausted)
	}
	seen := map[int]bool{}
	for _, k := range got {
		if k < 1 || k > 4 {
			t.Fatalf("key %d is outside the budget 1..4", k)
		}
		if seen[k] {
			t.Fatalf("key %d was handed out twice: %v", k, got)
		}
		seen[k] = true
	}
	if n := len(a.Domains()[0].Keys); n != 4 {
		t.Fatalf("domain holds %d keys after the race, want 4", n)
	}
}

func TestReleaseDomainFreesKeysAndBindings(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	key, err := a.AllocKey(d.ID)
	if err != nil {
		t.Fatalf("alloc key: %v", err)
	}
	if err := a.Bind(d.ID, impl.Region{Start: 0, Len: 100}); err != nil {
		t.Fatalf("bind region: %v", err)
	}
	if err := a.ReleaseDomain(d.ID); err != nil {
		t.Fatalf("release domain: %v", err)
	}
	if _, ok := a.Domain(d.ID); ok {
		t.Fatalf("released domain must be gone")
	}
	if _, ok := a.PkeyState(key); ok {
		t.Fatalf("released domain left key %d behind", key)
	}
	if err := a.Check(d.ID, impl.Region{Start: 0, Len: 10}); !errors.Is(err, impl.ErrUnknownDomain) {
		t.Fatalf("check on a released domain must fail with ErrUnknownDomain, got %v", err)
	}
	if _, err := a.AllocKey(d.ID); !errors.Is(err, impl.ErrUnknownDomain) {
		t.Fatalf("alloc on a released domain must fail with ErrUnknownDomain, got %v", err)
	}
	if err := a.ReleaseDomain(d.ID); !errors.Is(err, impl.ErrUnknownDomain) {
		t.Fatalf("second release must fail with ErrUnknownDomain, got %v", err)
	}
	next := mustDomain(t, a, "audio", impl.ModeShared)
	reused, err := a.AllocKey(next.ID)
	if err != nil {
		t.Fatalf("alloc after release: %v", err)
	}
	if int(reused) != 1 {
		t.Fatalf("freed slot was not reused: got %d, want 1", reused)
	}
}

func TestSnapshotRestoreKeepsDomainsAndKeys(t *testing.T) {
	cfg := testConfig()
	a := impl.NewPkeyDomainAllocator(cfg)
	d1 := mustDomain(t, a, "renderer", impl.ModeShared)
	key1, _ := a.AllocKey(d1.ID)
	key2, _ := a.AllocKey(d1.ID)
	if err := a.SetRights(key1, impl.RightsFull); err != nil {
		t.Fatalf("set rights: %v", err)
	}
	if err := a.Bind(d1.ID, impl.Region{Start: 0, Len: 100}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	d2 := mustDomain(t, a, "codec", impl.ModeExclusive)
	key3, _ := a.AllocKey(d2.ID)
	d3 := mustDomain(t, a, "scratch", impl.ModeShared)
	if err := a.ReleaseDomain(d3.ID); err != nil {
		t.Fatalf("release scratch: %v", err)
	}
	data, err := a.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	b := impl.NewPkeyDomainAllocator(cfg)
	if err := b.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	domains := b.Domains()
	if len(domains) != 2 || domains[0].ID != "dom-1" || domains[1].ID != "dom-2" {
		t.Fatalf("restored domains = %+v, want dom-1 and dom-2", domains)
	}
	gotKeys := domains[0].Keys
	if len(gotKeys) != 2 || gotKeys[0] != key1 || gotKeys[1] != key2 {
		t.Fatalf("restored keys = %v, want [%d %d]", gotKeys, key1, key2)
	}
	st, ok := b.PkeyState(key1)
	if !ok || st.Rights != impl.RightsFull || st.Version != 2 {
		t.Fatalf("restored key state = %+v ok=%v, want full rights at version 2", st, ok)
	}
	if err := b.Check("dom-1", impl.Region{Start: 50, Len: 10}); err != nil {
		t.Fatalf("restored region must still own its range, got %v", err)
	}
	if got := b.Domains()[1].Keys; len(got) != 1 || got[0] != key3 {
		t.Fatalf("second restored domain keys = %v, want [%d]", got, key3)
	}
	fresh := mustDomain(t, b, "fresh", impl.ModeShared)
	if fresh.ID != "dom-4" {
		t.Fatalf("domain id after restore = %q, want dom-4", fresh.ID)
	}
	next, err := b.AllocKey(fresh.ID)
	if err != nil {
		t.Fatalf("alloc after restore: %v", err)
	}
	if int(next) != 4 {
		t.Fatalf("key after restore = %d, want the free slot 4", next)
	}
}

func TestRestoreRejectsCorruptSnapshotKeepsState(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(testConfig())
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	key, err := a.AllocKey(d.ID)
	if err != nil {
		t.Fatalf("alloc key: %v", err)
	}
	if err := a.Bind(d.ID, impl.Region{Start: 0, Len: 100}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := a.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(a.Domains()); got != 1 {
		t.Fatalf("failed restore changed the domain table, count = %d", got)
	}
	if _, ok := a.PkeyState(key); !ok {
		t.Fatalf("failed restore dropped key %d", key)
	}
	if err := a.Check(d.ID, impl.Region{Start: 10, Len: 10}); err != nil {
		t.Fatalf("region must keep working after a rejected restore, got %v", err)
	}
	next, err := a.AllocKey(d.ID)
	if err != nil {
		t.Fatalf("alloc after rejected restore: %v", err)
	}
	if int(next) != 2 {
		t.Fatalf("next key after rejected restore = %d, want 2", next)
	}
}

func TestPinThreadBindsDomainKey(t *testing.T) {
	a := impl.NewPkeyDomainAllocator(impl.Config{MaxKeys: 4, MaxRegionsPerDomain: 4, MaxPinsPerDomain: 2})
	d := mustDomain(t, a, "renderer", impl.ModeShared)
	if _, err := a.PinThread(d.ID); !errors.Is(err, impl.ErrPkeyUnknown) {
		t.Fatalf("pin before any key allocation must fail with ErrPkeyUnknown, got %v", err)
	}
	key, err := a.AllocKey(d.ID)
	if err != nil {
		t.Fatalf("alloc key: %v", err)
	}
	first, err := a.PinThread(d.ID)
	if err != nil {
		t.Fatalf("pin thread: %v", err)
	}
	if first.ID != "pin-1" || first.DomainID != d.ID || first.Pkey != key {
		t.Fatalf("first pin = %+v, want pin-1 on %s with key %d", first, d.ID, key)
	}
	second, err := a.PinThread(d.ID)
	if err != nil {
		t.Fatalf("second pin: %v", err)
	}
	if second.ID != "pin-2" {
		t.Fatalf("second pin id = %q, want pin-2", second.ID)
	}
	if _, err := a.PinThread(d.ID); !errors.Is(err, impl.ErrPinLimit) {
		t.Fatalf("third pin must hit ErrPinLimit, got %v", err)
	}
	if err := a.UnpinThread(first); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if err := a.UnpinThread(first); !errors.Is(err, impl.ErrUnknownPin) {
		t.Fatalf("double unpin must fail with ErrUnknownPin, got %v", err)
	}
	if err := a.UnpinThread(second); err != nil {
		t.Fatalf("unpin second: %v", err)
	}
	again, err := a.PinThread(d.ID)
	if err != nil {
		t.Fatalf("pin after unpin: %v", err)
	}
	if again.ID != "pin-3" {
		t.Fatalf("pin id after reuse = %q, want pin-3", again.ID)
	}
}
