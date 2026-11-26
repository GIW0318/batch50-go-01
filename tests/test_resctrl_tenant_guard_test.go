package task

import (
	"errors"
	"math/bits"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-10"
)

func guardLimits() impl.GuardLimits {
	return impl.GuardLimits{MaxGroups: 4, MaxMonitorIDs: 2, MaskBits: 8}
}

func newNode(t *testing.T) *impl.ResctrlTenantGuard {
	t.Helper()
	g := impl.NewResctrlTenantGuard(guardLimits())
	domains := []impl.Domain{{ID: 0, CPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, Ways: 8}}
	if err := g.RegisterNode("node-a", domains, 1000); err != nil {
		t.Fatalf("register node-a: %v", err)
	}
	return g
}

func create(t *testing.T, g *impl.ResctrlTenantGuard, tenant string, spec impl.ResctrlSpec) impl.Group {
	t.Helper()
	spec.NodeID = "node-a"
	id, err := g.Create(tenant, spec)
	if err != nil {
		t.Fatalf("create %s: %v", tenant, err)
	}
	group, ok := g.Group(id)
	if !ok {
		t.Fatalf("group %s is missing after create", id)
	}
	return group
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestCreateAllocatesDisjointMasks(t *testing.T) {
	g := newNode(t)
	first := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 4, Weight: 1})
	second := create(t, g, "tenant-b", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2, Weight: 1})
	if first.ID != "node-a/tenant-a" {
		t.Fatalf("group id = %q, want node-a/tenant-a", first.ID)
	}
	if first.Mask != 0x0F || first.MaskBits != 4 {
		t.Fatalf("first mask = %#x/%d bits, want 0xf over 4 bits", first.Mask, first.MaskBits)
	}
	if second.Mask != 0x30 || second.MaskBits != 2 {
		t.Fatalf("second mask = %#x/%d bits, want 0x30 over 2 bits", second.Mask, second.MaskBits)
	}
	if first.Mask&second.Mask != 0 {
		t.Fatalf("masks must not overlap, got %#x and %#x", first.Mask, second.Mask)
	}
	if first.MonitorID != 0 || second.MonitorID != 1 {
		t.Fatalf("monitor ids = %d/%d, want the lowest free ids 0 and 1", first.MonitorID, second.MonitorID)
	}
}

func TestCreateRejectsExplicitOverlappingMask(t *testing.T) {
	g := newNode(t)
	explicit := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, Mask: 0x0F})
	if explicit.MaskBits != 4 {
		t.Fatalf("mask bits = %d, want the width of the explicit mask 4", explicit.MaskBits)
	}
	if _, err := g.Create("tenant-b", impl.ResctrlSpec{NodeID: "node-a", Class: impl.ClassDedicated, Mask: 0x03}); !errors.Is(err, impl.ErrMaskOverlap) {
		t.Fatalf("an overlapping explicit mask must fail with ErrMaskOverlap, got %v", err)
	}
	if _, err := g.Create("tenant-c", impl.ResctrlSpec{NodeID: "node-a", Class: impl.ClassDedicated, Mask: 0x100}); !errors.Is(err, impl.ErrBadSpec) {
		t.Fatalf("a mask beyond the node width must fail with ErrBadSpec, got %v", err)
	}
	if _, err := g.Create("tenant-d", impl.ResctrlSpec{NodeID: "node-a", Class: impl.ClassDedicated}); !errors.Is(err, impl.ErrBadSpec) {
		t.Fatalf("a spec without mask or width must fail with ErrBadSpec, got %v", err)
	}
	if _, err := g.Create("  ", impl.ResctrlSpec{NodeID: "node-a", Class: impl.ClassDedicated, MaskBits: 2}); !errors.Is(err, impl.ErrBadTenant) {
		t.Fatalf("a blank tenant must fail with ErrBadTenant, got %v", err)
	}
	if _, err := g.Create("tenant-e", impl.ResctrlSpec{NodeID: "node-missing", Class: impl.ClassDedicated, MaskBits: 2}); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("a spec for an unregistered node must fail with ErrUnknownNode, got %v", err)
	}
}

func TestCreateEnforcesMaskCapacity(t *testing.T) {
	g := newNode(t)
	create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 4})
	create(t, g, "tenant-b", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 4})
	if _, err := g.Create("tenant-c", impl.ResctrlSpec{NodeID: "node-a", Class: impl.ClassDedicated, MaskBits: 1}); !errors.Is(err, impl.ErrMaskExhausted) {
		t.Fatalf("an exhausted mask must fail with ErrMaskExhausted, got %v", err)
	}
}

func TestCreateReclaimsIdleGroupAtCap(t *testing.T) {
	g := impl.NewResctrlTenantGuard(impl.GuardLimits{MaxGroups: 2, MaxMonitorIDs: 2, MaskBits: 8})
	domains := []impl.Domain{{ID: 0, CPUs: []int{0, 1, 2, 3}, Ways: 4}}
	if err := g.RegisterNode("node-a", domains, 1000); err != nil {
		t.Fatalf("register: %v", err)
	}
	busy := create(t, g, "tenant-busy", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	idle := create(t, g, "tenant-idle", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if err := g.Assign(500, busy.ID); err != nil {
		t.Fatalf("assign 500: %v", err)
	}
	fresh := create(t, g, "tenant-fresh", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if _, ok := g.Group(idle.ID); ok {
		t.Fatalf("the idle group %s must be reclaimed for the new tenant", idle.ID)
	}
	if fresh.ID != "node-a/tenant-fresh" {
		t.Fatalf("new group id = %q, want node-a/tenant-fresh", fresh.ID)
	}
	groups := g.Groups("node-a")
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want the cap 2 after reclaiming", len(groups))
	}
	if got, _ := g.Group(busy.ID); !equalInts(got.PIDs, []int{500}) {
		t.Fatalf("the busy group pids = %v, want it untouched", got.PIDs)
	}
}

func TestSharedTenantsLandInOneGroup(t *testing.T) {
	g := newNode(t)
	first := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassShared, MaskBits: 2})
	second := create(t, g, "tenant-b", impl.ResctrlSpec{Class: impl.ClassShared, MaskBits: 2})
	if first.ID != second.ID {
		t.Fatalf("shared tenants got different groups %q and %q", first.ID, second.ID)
	}
	if first.ID != "node-a/shared" {
		t.Fatalf("shared group id = %q, want node-a/shared", first.ID)
	}
	group, ok := g.Group(first.ID)
	if !ok {
		t.Fatalf("shared group must exist")
	}
	if len(group.Tenants) != 2 || group.Tenants[0] != "tenant-a" || group.Tenants[1] != "tenant-b" {
		t.Fatalf("tenants = %v, want both shared tenants in join order", group.Tenants)
	}
	if group.MaskBits != 2 {
		t.Fatalf("shared mask bits = %d, want the first request width 2", group.MaskBits)
	}
	if got := len(g.Groups("node-a")); got != 1 {
		t.Fatalf("groups = %d, want a single shared group", got)
	}
	if err := g.Assign(700, group.ID); err != nil {
		t.Fatalf("assign to shared group: %v", err)
	}
	if got, _ := g.Group(group.ID); !equalInts(got.PIDs, []int{700}) {
		t.Fatalf("shared pids = %v, want [700]", got.PIDs)
	}
}

func TestAssignMovesPIDBetweenGroups(t *testing.T) {
	g := newNode(t)
	first := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	second := create(t, g, "tenant-b", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if err := g.Assign(100, first.ID); err != nil {
		t.Fatalf("assign 100: %v", err)
	}
	if got, _ := g.Group(first.ID); !equalInts(got.PIDs, []int{100}) {
		t.Fatalf("pids = %v, want [100]", got.PIDs)
	}
	if err := g.Assign(100, second.ID); err != nil {
		t.Fatalf("move 100: %v", err)
	}
	leftOver, _ := g.Group(first.ID)
	if len(leftOver.PIDs) != 0 {
		t.Fatalf("the old group still holds %v, want the task moved", leftOver.PIDs)
	}
	moved, _ := g.Group(second.ID)
	if !equalInts(moved.PIDs, []int{100}) {
		t.Fatalf("the new group pids = %v, want [100]", moved.PIDs)
	}
	if err := g.Assign(102, first.ID); err != nil {
		t.Fatalf("assign 102: %v", err)
	}
	if err := g.Assign(101, first.ID); err != nil {
		t.Fatalf("assign 101: %v", err)
	}
	if got, _ := g.Group(first.ID); !equalInts(got.PIDs, []int{101, 102}) {
		t.Fatalf("pids = %v, want them sorted ascending", got.PIDs)
	}
}

func TestAssignRejectsUnknownGroup(t *testing.T) {
	g := newNode(t)
	group := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if err := g.Assign(10, "node-a/missing"); !errors.Is(err, impl.ErrUnknownGroup) {
		t.Fatalf("assigning into an unknown group must fail with ErrUnknownGroup, got %v", err)
	}
	if err := g.Assign(0, group.ID); !errors.Is(err, impl.ErrBadPID) {
		t.Fatalf("pid 0 must fail with ErrBadPID, got %v", err)
	}
	if err := g.Assign(-3, group.ID); !errors.Is(err, impl.ErrBadPID) {
		t.Fatalf("a negative pid must fail with ErrBadPID, got %v", err)
	}
	if got, _ := g.Group(group.ID); len(got.PIDs) != 0 {
		t.Fatalf("rejected assignments must not change the group, got %v", got.PIDs)
	}
}

func TestDeleteRequiresEmptyGroup(t *testing.T) {
	g := newNode(t)
	first := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	second := create(t, g, "tenant-b", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if err := g.Assign(200, first.ID); err != nil {
		t.Fatalf("assign 200: %v", err)
	}
	if err := g.Delete(first.ID); !errors.Is(err, impl.ErrGroupNotEmpty) {
		t.Fatalf("deleting a group holding a task must fail with ErrGroupNotEmpty, got %v", err)
	}
	if err := g.Assign(200, second.ID); err != nil {
		t.Fatalf("move 200: %v", err)
	}
	if err := g.Delete(first.ID); err != nil {
		t.Fatalf("delete an empty group: %v", err)
	}
	if _, ok := g.Group(first.ID); ok {
		t.Fatalf("a deleted group must not be readable")
	}
	if err := g.Delete(first.ID); !errors.Is(err, impl.ErrUnknownGroup) {
		t.Fatalf("deleting a missing group must fail with ErrUnknownGroup, got %v", err)
	}
}

func TestDeleteReleasesMaskAndMonitorID(t *testing.T) {
	g := newNode(t)
	first := create(t, g, "tenant-a", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	second := create(t, g, "tenant-b", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if first.Mask != 0x03 || second.Mask != 0x0C {
		t.Fatalf("masks = %#x/%#x, want 0x3 and 0xc", first.Mask, second.Mask)
	}
	if err := g.Delete(first.ID); err != nil {
		t.Fatalf("delete first: %v", err)
	}
	reused := create(t, g, "tenant-c", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if reused.Mask != 0x03 {
		t.Fatalf("reused mask = %#x, want the released low mask 0x3", reused.Mask)
	}
	if reused.MonitorID != 0 {
		t.Fatalf("reused monitor id = %d, want the released id 0", reused.MonitorID)
	}
}

func TestMonitorIDsReusedBeyondCap(t *testing.T) {
	g := impl.NewResctrlTenantGuard(impl.GuardLimits{MaxGroups: 5, MaxMonitorIDs: 2, MaskBits: 8})
	domains := []impl.Domain{{ID: 0, CPUs: []int{0, 1, 2, 3}, Ways: 8}}
	if err := g.RegisterNode("node-a", domains, 1000); err != nil {
		t.Fatalf("register: %v", err)
	}
	seen := make(map[int]bool, 2)
	for i := 0; i < 4; i++ {
		group := create(t, g, "tenant-"+string(rune('a'+i)), impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
		if group.MonitorID < 0 || group.MonitorID >= 2 {
			t.Fatalf("monitor id = %d, want it inside [0,2)", group.MonitorID)
		}
		seen[group.MonitorID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("distinct monitor ids = %d, want both ids of the pool reused", len(seen))
	}
	if got := len(g.Groups("node-a")); got != 4 {
		t.Fatalf("groups = %d, want 4", got)
	}
}

func TestWeightMapsToBandwidthThreshold(t *testing.T) {
	g := newNode(t)
	heavy := create(t, g, "tenant-heavy", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2, Weight: 3})
	light := create(t, g, "tenant-light", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2, Weight: 1})
	if heavy.MBps != 750 || light.MBps != 250 {
		t.Fatalf("bandwidth = %d/%d, want 750/250 over a 1000 MBps node", heavy.MBps, light.MBps)
	}
	stats, err := g.Stats(heavy.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.MBpsLimit != 750 {
		t.Fatalf("stats limit = %d, want 750", stats.MBpsLimit)
	}
	if stats.MonitorID != heavy.MonitorID {
		t.Fatalf("stats monitor = %d, want %d", stats.MonitorID, heavy.MonitorID)
	}
}

func TestStatsRejectsUnknownGroup(t *testing.T) {
	g := newNode(t)
	if _, err := g.Stats("node-a/missing"); !errors.Is(err, impl.ErrUnknownGroup) {
		t.Fatalf("stats of an unknown group must fail with ErrUnknownGroup, got %v", err)
	}
}

func TestConcurrentCreateDoesNotReuseMask(t *testing.T) {
	g := impl.NewResctrlTenantGuard(impl.GuardLimits{MaxGroups: 8, MaxMonitorIDs: 8, MaskBits: 8})
	domains := []impl.Domain{{ID: 0, CPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, Ways: 8}}
	if err := g.RegisterNode("node-a", domains, 1000); err != nil {
		t.Fatalf("register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := impl.ResctrlSpec{NodeID: "node-a", Class: impl.ClassDedicated, MaskBits: 1, Weight: 1}
			_, _ = g.Create("tenant-"+string(rune('a'+i)), spec)
		}(i)
	}
	wg.Wait()
	groups := g.Groups("node-a")
	if len(groups) != 8 {
		t.Fatalf("groups = %d, want 8 successful creates", len(groups))
	}
	used := make(map[uint64]bool, 8)
	var union uint64
	for _, group := range groups {
		if bits.OnesCount64(group.Mask) != 1 {
			t.Fatalf("mask %#x of %s must hold exactly one bit", group.Mask, group.ID)
		}
		if used[group.Mask] {
			t.Fatalf("mask %#x was handed out twice", group.Mask)
		}
		used[group.Mask] = true
		union |= group.Mask
	}
	if union != 0xFF {
		t.Fatalf("union of masks = %#x, want every bit of the node used exactly once", union)
	}
}

func TestRebalanceReclaimsIdleGroups(t *testing.T) {
	g := newNode(t)
	busy := create(t, g, "tenant-busy", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	idle := create(t, g, "tenant-idle", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2})
	if err := g.Assign(300, busy.ID); err != nil {
		t.Fatalf("assign 300: %v", err)
	}
	freed, err := g.Rebalance("node-a")
	if err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	if freed != 1 {
		t.Fatalf("reclaimed groups = %d, want the single idle group", freed)
	}
	if _, ok := g.Group(idle.ID); ok {
		t.Fatalf("the idle group %s must be reclaimed", idle.ID)
	}
	groups := g.Groups("node-a")
	if len(groups) != 1 || groups[0].ID != busy.ID {
		t.Fatalf("groups = %+v, want only the busy group", groups)
	}
}

func TestSnapshotRestoreKeepsMasksAndTasks(t *testing.T) {
	g := newNode(t)
	heavy := create(t, g, "tenant-heavy", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2, Weight: 2})
	light := create(t, g, "tenant-light", impl.ResctrlSpec{Class: impl.ClassDedicated, MaskBits: 2, Weight: 1})
	if err := g.Assign(400, heavy.ID); err != nil {
		t.Fatalf("assign 400: %v", err)
	}
	if err := g.Assign(401, heavy.ID); err != nil {
		t.Fatalf("assign 401: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewResctrlTenantGuard(guardLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, ok := restored.Group(heavy.ID)
	if !ok {
		t.Fatalf("restored guard lost the heavy group")
	}
	if got.Mask != heavy.Mask || !equalInts(got.PIDs, []int{400, 401}) {
		t.Fatalf("restored heavy group = %+v, want mask %#x and pids [400 401]", got, heavy.Mask)
	}
	other, ok := restored.Group(light.ID)
	if !ok || other.Mask != light.Mask {
		t.Fatalf("restored light group = %+v ok=%v, want mask %#x", other, ok, light.Mask)
	}
	stats, err := restored.Stats(heavy.ID)
	if err != nil {
		t.Fatalf("stats after restore: %v", err)
	}
	if stats.MBpsLimit != got.MBps {
		t.Fatalf("restored bandwidth limit = %d, want %d", stats.MBpsLimit, got.MBps)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := restored.Group(heavy.ID); !ok {
		t.Fatalf("a rejected restore must leave the live state intact")
	}
}
