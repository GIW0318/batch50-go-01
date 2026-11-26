package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-d-02"
)

func siblingTopology(cores int) []impl.CPU {
	cpus := make([]impl.CPU, 0, cores*2)
	for c := 0; c < cores; c++ {
		for th := 0; th < 2; th++ {
			cpus = append(cpus, impl.CPU{ID: c*2 + th, Core: c, Thread: th, Online: true})
		}
	}
	return cpus
}

func newIsolator(t *testing.T, cores int) *impl.SmtSiblingIsolator {
	t.Helper()
	s := impl.NewSmtSiblingIsolator(impl.Limits{YieldReleaseLoad: 4})
	if err := s.SetTopology(siblingTopology(cores)); err != nil {
		t.Fatalf("set topology with %d cores: %v", cores, err)
	}
	return s
}

func TestSetTopologyBuildsCoreGroups(t *testing.T) {
	s := impl.NewSmtSiblingIsolator(impl.Limits{YieldReleaseLoad: 4})
	if err := s.SetTopology(siblingTopology(4)); err != nil {
		t.Fatalf("set topology: %v", err)
	}
	cores := s.Topology()
	if len(cores) != 4 {
		t.Fatalf("topology has %d cores, want 4", len(cores))
	}
	for i, core := range cores {
		if core.ID != i {
			t.Fatalf("core %d has id %d, want cores ordered by id", i, core.ID)
		}
		if len(core.CPUs) != 2 {
			t.Fatalf("core %d holds %d processors, want the sibling pair", core.ID, len(core.CPUs))
		}
		if core.CPUs[0] != i*2 || core.CPUs[1] != i*2+1 {
			t.Fatalf("core %d processors = %v, want [%d %d]", core.ID, core.CPUs, i*2, i*2+1)
		}
		if !core.Online {
			t.Fatalf("core %d reported offline while both processors are online", core.ID)
		}
	}
}

func TestSetTopologyRejectsInvalid(t *testing.T) {
	s := impl.NewSmtSiblingIsolator(impl.Limits{YieldReleaseLoad: 4})
	if err := s.SetTopology(nil); !errors.Is(err, impl.ErrBadTopology) {
		t.Fatalf("empty topology must fail with ErrBadTopology, got %v", err)
	}
	dup := siblingTopology(1)
	dup = append(dup, impl.CPU{ID: 1, Core: 1, Thread: 0, Online: true})
	if err := s.SetTopology(dup); !errors.Is(err, impl.ErrBadTopology) {
		t.Fatalf("a duplicate processor id must fail with ErrBadTopology, got %v", err)
	}
	wide := []impl.CPU{
		{ID: 0, Core: 0, Thread: 0, Online: true},
		{ID: 1, Core: 0, Thread: 1, Online: true},
		{ID: 2, Core: 0, Thread: 2, Online: true},
	}
	if err := s.SetTopology(wide); !errors.Is(err, impl.ErrBadTopology) {
		t.Fatalf("a core with three processors must fail with ErrBadTopology, got %v", err)
	}
	if err := s.SetTopology([]impl.CPU{{ID: -1, Core: 0, Thread: 0, Online: true}}); !errors.Is(err, impl.ErrBadTopology) {
		t.Fatalf("a negative processor id must fail with ErrBadTopology, got %v", err)
	}
}

func TestReserveValidatesRequest(t *testing.T) {
	s := newIsolator(t, 2)
	if _, err := s.Reserve("   ", impl.Resource{Threads: 1}); !errors.Is(err, impl.ErrBadDomain) {
		t.Fatalf("a blank domain must fail with ErrBadDomain, got %v", err)
	}
	if _, err := s.Reserve("web", impl.Resource{Threads: 0}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("zero threads must fail with ErrBadRequest, got %v", err)
	}
	if _, err := s.Reserve("web", impl.Resource{Threads: -2}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("negative threads must fail with ErrBadRequest, got %v", err)
	}
	if got := len(s.Allocations()); got != 0 {
		t.Fatalf("rejected requests left %d allocations behind", got)
	}
}

func TestReserveExclusiveTakesWholeCore(t *testing.T) {
	s := newIsolator(t, 4)
	a, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve two exclusive threads: %v", err)
	}
	if a.ID == "" || a.Domain != "web" {
		t.Fatalf("allocation id=%q domain=%q, want a named web allocation", a.ID, a.Domain)
	}
	if len(a.Cores) != 1 || a.Cores[0] != 0 {
		t.Fatalf("allocation cores = %v, want the single lowest core [0]", a.Cores)
	}
	if len(a.CPUs) != 2 || a.CPUs[0] != 0 || a.CPUs[1] != 1 {
		t.Fatalf("allocation cpus = %v, want both siblings [0 1]", a.CPUs)
	}
	if a.Threads != 2 || a.Degraded || a.Gen <= 0 {
		t.Fatalf("allocation threads=%d degraded=%v gen=%d, want 2 threads, no degradation, positive gen", a.Threads, a.Degraded, a.Gen)
	}
	got, ok := s.Allocation(a.ID)
	if !ok || got.Domain != "web" || got.Threads != 2 {
		t.Fatalf("allocation lookup = %+v ok=%v, want the stored record", got, ok)
	}
}

func TestReserveSharesCoreWithinSameDomain(t *testing.T) {
	s := newIsolator(t, 4)
	first, err := s.Reserve("web", impl.Resource{Threads: 1})
	if err != nil {
		t.Fatalf("reserve the first thread: %v", err)
	}
	if len(first.CPUs) != 1 || first.CPUs[0] != 0 {
		t.Fatalf("first allocation cpus = %v, want [0]", first.CPUs)
	}
	second, err := s.Reserve("web", impl.Resource{Threads: 1})
	if err != nil {
		t.Fatalf("reserve the sibling for the same domain: %v", err)
	}
	if len(second.CPUs) != 1 || second.CPUs[0] != 1 {
		t.Fatalf("second allocation cpus = %v, want the sibling [1]", second.CPUs)
	}
	if len(second.Cores) != 1 || second.Cores[0] != 0 {
		t.Fatalf("second allocation cores = %v, want [0]", second.Cores)
	}
	usage := s.DomainUsage("web")
	if usage.Threads != 2 || usage.Allocations != 2 {
		t.Fatalf("usage threads=%d allocations=%d, want 2 and 2", usage.Threads, usage.Allocations)
	}
	if len(usage.Cores) != 1 || usage.Cores[0] != 0 {
		t.Fatalf("usage cores = %v, want the shared core [0]", usage.Cores)
	}
}

func TestReserveBlocksSiblingAcrossDomains(t *testing.T) {
	s := newIsolator(t, 4)
	first, err := s.Reserve("web", impl.Resource{Threads: 1})
	if err != nil {
		t.Fatalf("reserve for web: %v", err)
	}
	other, err := s.Reserve("db", impl.Resource{Threads: 1})
	if err != nil {
		t.Fatalf("reserve for db: %v", err)
	}
	if len(other.Cores) != 1 || other.Cores[0] != 1 {
		t.Fatalf("db allocation cores = %v, want the next free core [1]", other.Cores)
	}
	if len(other.CPUs) != 1 || other.CPUs[0] != 2 {
		t.Fatalf("db allocation cpus = %v, want [2] on the next free core", other.CPUs)
	}
	if other.Cores[0] == first.Cores[0] || other.CPUs[0] == first.CPUs[0] {
		t.Fatalf("two domains landed on the same core: web=%+v db=%+v", first, other)
	}
}

func TestReserveExclusiveRoundsOddRequestToWholeCore(t *testing.T) {
	s := newIsolator(t, 4)
	a, err := s.Reserve("web", impl.Resource{Threads: 3, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve three exclusive threads: %v", err)
	}
	if len(a.Cores) != 2 || a.Cores[0] != 0 || a.Cores[1] != 1 {
		t.Fatalf("allocation cores = %v, want two whole cores [0 1]", a.Cores)
	}
	if len(a.CPUs) != 4 || a.Threads != 4 {
		t.Fatalf("allocation cpus=%v threads=%d, want all four siblings of both cores", a.CPUs, a.Threads)
	}
	if !a.Degraded || a.Reason != impl.ReasonWholeCore {
		t.Fatalf("allocation degraded=%v reason=%q, want the whole core rounding reason", a.Degraded, a.Reason)
	}
}

func TestReserveCapacityReportsBlockedSiblings(t *testing.T) {
	s := newIsolator(t, 1)
	if _, err := s.Reserve("web", impl.Resource{Threads: 1}); err != nil {
		t.Fatalf("reserve the first thread: %v", err)
	}
	if _, err := s.Reserve("db", impl.Resource{Threads: 1}); !errors.Is(err, impl.ErrCapacity) {
		t.Fatalf("the only sibling belongs to web, want ErrCapacity, got %v", err)
	}
	if reason := s.DomainUsage("db").LastReason; reason != impl.ReasonSiblingBlocked {
		t.Fatalf("degradation reason = %q, want %q", reason, impl.ReasonSiblingBlocked)
	}
	if _, err := s.Reserve("db", impl.Resource{Threads: 5}); !errors.Is(err, impl.ErrCapacity) {
		t.Fatalf("an oversized request must fail with ErrCapacity, got %v", err)
	}
	if reason := s.DomainUsage("db").LastReason; reason != impl.ReasonCapacityExhausted {
		t.Fatalf("degradation reason = %q, want %q", reason, impl.ReasonCapacityExhausted)
	}
	if got := len(s.Allocations()); got != 1 {
		t.Fatalf("allocations = %d, want only the web reservation", got)
	}
}

func TestReleaseRestoresCapacity(t *testing.T) {
	s := newIsolator(t, 2)
	first, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve the first core: %v", err)
	}
	second, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve the second core: %v", err)
	}
	if _, err := s.Reserve("db", impl.Resource{Threads: 2, Exclusive: true}); !errors.Is(err, impl.ErrCapacity) {
		t.Fatalf("a third whole core reservation must fail, got %v", err)
	}
	if err := s.Release(first.ID); err != nil {
		t.Fatalf("release the first allocation: %v", err)
	}
	if err := s.Release(first.ID); !errors.Is(err, impl.ErrUnknownAlloc) {
		t.Fatalf("releasing twice must fail with ErrUnknownAlloc, got %v", err)
	}
	if err := s.Release("alloc-404"); !errors.Is(err, impl.ErrUnknownAlloc) {
		t.Fatalf("releasing an unknown allocation must fail with ErrUnknownAlloc, got %v", err)
	}
	reused, err := s.Reserve("db", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("the freed core must be handed out again: %v", err)
	}
	if len(reused.Cores) != 1 || reused.Cores[0] != 0 {
		t.Fatalf("reused cores = %v, want the lowest freed core [0]", reused.Cores)
	}
	usage := s.DomainUsage("web")
	if usage.Threads != 2 || usage.Allocations != 1 || len(usage.Cores) != 1 || usage.Cores[0] != second.Cores[0] {
		t.Fatalf("web usage after the release = %+v, want only the second allocation", usage)
	}
}

func TestConcurrentReservationsKeepCoresExclusive(t *testing.T) {
	s := newIsolator(t, 4)
	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	capped := 0
	cores := make([]int, 0, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := s.Reserve(fmt.Sprintf("domain-%02d", i), impl.Resource{Threads: 2, Exclusive: true})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted++
				if len(a.Cores) == 1 {
					cores = append(cores, a.Cores[0])
				}
			case errors.Is(err, impl.ErrCapacity):
				capped++
			}
		}(i)
	}
	wg.Wait()
	if granted != 4 || capped != 12 {
		t.Fatalf("concurrent reservations granted=%d capped=%d, want 4 and 12", granted, capped)
	}
	seen := make(map[int]bool, 4)
	for _, core := range cores {
		if seen[core] {
			t.Fatalf("core %d was handed to two domains", core)
		}
		seen[core] = true
	}
	if got := len(s.Allocations()); got != 4 {
		t.Fatalf("allocations = %d, want 4", got)
	}
}

func TestSetTopologyRejectedWhileAllocated(t *testing.T) {
	s := newIsolator(t, 2)
	a, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.SetTopology(siblingTopology(2)); !errors.Is(err, impl.ErrBusy) {
		t.Fatalf("changing the topology with live allocations must fail with ErrBusy, got %v", err)
	}
	if err := s.Release(a.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.SetTopology(siblingTopology(2)); err != nil {
		t.Fatalf("the topology must be changeable once idle, got %v", err)
	}
}

func TestTopologyIsReadOnlyCopy(t *testing.T) {
	s := newIsolator(t, 2)
	first := s.Topology()
	if len(first) != 2 {
		t.Fatalf("topology = %d cores, want 2", len(first))
	}
	first[0].ID = 42
	first[0].CPUs[0] = 99
	again := s.Topology()
	if again[0].ID != 0 {
		t.Fatalf("topology core id changed to %d through the returned slice", again[0].ID)
	}
	if len(again[0].CPUs) != 2 || again[0].CPUs[0] != 0 {
		t.Fatalf("topology processors = %v after mutating the copy, want [0 1]", again[0].CPUs)
	}
}

func TestSetLoadValidatesAndTracksDomains(t *testing.T) {
	s := newIsolator(t, 4)
	if err := s.SetLoad("web", -1); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("a negative load must fail with ErrBadRequest, got %v", err)
	}
	if _, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true}); err != nil {
		t.Fatalf("reserve web: %v", err)
	}
	if _, err := s.Reserve("db", impl.Resource{Threads: 2, Exclusive: true}); err != nil {
		t.Fatalf("reserve db: %v", err)
	}
	domains := s.Domains()
	if len(domains) != 2 || domains[0] != "db" || domains[1] != "web" {
		t.Fatalf("domains = %v, want [db web]", domains)
	}
	if err := s.SetLoad("db", 3); err != nil {
		t.Fatalf("set load: %v", err)
	}
	if got := s.DomainUsage("db").Load; got != 3 {
		t.Fatalf("db load = %d, want 3", got)
	}
}

func TestRebalanceReclaimsReleasedDomains(t *testing.T) {
	s := newIsolator(t, 2)
	a, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.Release(a.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.Rebalance(); err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	if got := len(s.Domains()); got != 0 {
		t.Fatalf("domains after rebalance = %d, want 0", got)
	}
	if got := len(s.Allocations()); got != 0 {
		t.Fatalf("allocations after rebalance = %d, want 0", got)
	}
	if usage := s.DomainUsage("web"); usage.Threads != 0 || usage.Allocations != 0 {
		t.Fatalf("web usage after rebalance = %+v, want an empty record", usage)
	}
	full, err := s.Reserve("db", impl.Resource{Threads: 4, Exclusive: true})
	if err != nil {
		t.Fatalf("the reclaimed capacity must be allocatable again: %v", err)
	}
	if len(full.Cores) != 2 || full.Threads != 4 {
		t.Fatalf("full allocation cores=%v threads=%d, want both cores and four threads", full.Cores, full.Threads)
	}
}

func TestSnapshotRestoreKeepsAllocations(t *testing.T) {
	limits := impl.Limits{YieldReleaseLoad: 4}
	s := impl.NewSmtSiblingIsolator(limits)
	if err := s.SetTopology(siblingTopology(4)); err != nil {
		t.Fatalf("set topology: %v", err)
	}
	web, err := s.Reserve("web", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve web: %v", err)
	}
	db, err := s.Reserve("db", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve db: %v", err)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewSmtSiblingIsolator(limits)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(r.Topology()); got != 4 {
		t.Fatalf("restored topology = %d cores, want 4", got)
	}
	if got := len(r.Allocations()); got != 2 {
		t.Fatalf("restored allocations = %d, want 2", got)
	}
	if usage := r.DomainUsage("web"); usage.Threads != 2 || usage.Allocations != 1 {
		t.Fatalf("restored web usage = %+v, want one two thread allocation", usage)
	}
	if err := r.Release(web.ID); err != nil {
		t.Fatalf("release a restored allocation: %v", err)
	}
	cache, err := r.Reserve("cache", impl.Resource{Threads: 2, Exclusive: true})
	if err != nil {
		t.Fatalf("reserve after restore: %v", err)
	}
	if cache.Gen <= db.Gen || cache.Gen <= web.Gen {
		t.Fatalf("allocation generation after restore = %d, want it to continue past %d", cache.Gen, db.Gen)
	}
	if err := r.Restore([]byte("broken-isolator-state")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a broken snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(r.Allocations()); got != 2 {
		t.Fatalf("allocations after the rejected restore = %d, want the live 2", got)
	}
}
