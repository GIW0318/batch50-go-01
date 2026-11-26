package task

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-01"
)

func baseLimits() impl.Limits {
	return impl.Limits{MaxPatches: 8, MaxFactorSum: 0, MaxEdgeFactor: 64}
}

func baseView(frame int64) impl.ViewParams {
	return impl.ViewParams{
		CameraDistance:   10,
		PixelsPerUnit:    100,
		PixelErrorBudget: 1,
		FrameID:          frame,
	}
}

func singleEdge(patchID, edgeID string, length float64) []impl.Edge {
	return []impl.Edge{{EdgeID: edgeID, NeighborID: "", Length: length}}
}

func TestRegisterPatchStoresAndSorts(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	edgesB := []impl.Edge{
		{EdgeID: "e-b1", NeighborID: "p-a", Length: 4},
		{EdgeID: "e-b2", NeighborID: "", Length: 3},
	}
	if err := p.RegisterPatch("p-b", edgesB); err != nil {
		t.Fatalf("register p-b: %v", err)
	}
	if err := p.RegisterPatch("p-a", singleEdge("p-a", "e-a1", 2)); err != nil {
		t.Fatalf("register p-a: %v", err)
	}
	got := p.Patches()
	want := []string{"p-a", "p-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patches = %v, want the ascending list %v", got, want)
	}
	edges, ok := p.PatchEdges("p-b")
	if !ok {
		t.Fatalf("registered patch p-b must be readable")
	}
	if len(edges) != 2 || edges[0].EdgeID != "e-b1" || edges[1].Length != 3 {
		t.Fatalf("edges of p-b = %+v, want e-b1 then e-b2 with their lengths", edges)
	}
	if _, ok := p.PatchEdges("p-404"); ok {
		t.Fatalf("unknown patch must not be readable")
	}
	if st := p.Stats(); st.PatchCount != 2 {
		t.Fatalf("patch count = %d, want 2", st.PatchCount)
	}
}

func TestRegisterPatchRejectsBadInput(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	cases := []struct {
		name  string
		patch string
		edges []impl.Edge
	}{
		{"empty patch id", "  ", singleEdge("p", "e-1", 1)},
		{"no edges", "p-1", nil},
		{"empty edge id", "p-1", []impl.Edge{{EdgeID: "", Length: 1}}},
		{"zero length", "p-1", singleEdge("p-1", "e-1", 0)},
		{"negative length", "p-1", singleEdge("p-1", "e-1", -2)},
		{"nan length", "p-1", []impl.Edge{{EdgeID: "e-1", Length: nan()}}},
	}
	for _, c := range cases {
		if err := p.RegisterPatch(c.patch, c.edges); !errors.Is(err, impl.ErrBadPatch) {
			t.Fatalf("%s must be rejected with ErrBadPatch, got %v", c.name, err)
		}
	}
	if got := len(p.Patches()); got != 0 {
		t.Fatalf("rejected patches must not enter the table, got %d entries", got)
	}
}

func nan() float64 {
	z := 0.0
	return z / z
}

func TestRegisterPatchRejectsDuplicateAndLimit(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(impl.Limits{MaxPatches: 2, MaxEdgeFactor: 16})
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1)); err != nil {
		t.Fatalf("register p-1: %v", err)
	}
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-9", 2)); !errors.Is(err, impl.ErrDuplicatePatch) {
		t.Fatalf("duplicate patch id must fail with ErrDuplicatePatch, got %v", err)
	}
	if err := p.RegisterPatch("p-2", singleEdge("p-2", "e-2", 1)); err != nil {
		t.Fatalf("register p-2 under the cap: %v", err)
	}
	if err := p.RegisterPatch("p-3", singleEdge("p-3", "e-3", 1)); !errors.Is(err, impl.ErrPatchLimit) {
		t.Fatalf("third patch must fail with ErrPatchLimit, got %v", err)
	}
	if got := p.Patches(); !reflect.DeepEqual(got, []string{"p-1", "p-2"}) {
		t.Fatalf("patches = %v, want the two accepted ids", got)
	}
}

func TestPlanRoundsToPowerOfTwo(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	edges := []impl.Edge{
		{EdgeID: "e-1", Length: 1},
		{EdgeID: "e-2", Length: 1.6},
		{EdgeID: "e-3", Length: 1.7},
	}
	if err := p.RegisterPatch("p-1", edges); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	factors, err := p.Plan(baseView(3))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(factors) != 1 {
		t.Fatalf("plan length = %d, want 1", len(factors))
	}
	want := []int{16, 16, 32}
	if !reflect.DeepEqual(factors[0].EdgeFactors, want) {
		t.Fatalf("edge factors = %v, want %v", factors[0].EdgeFactors, want)
	}
	if factors[0].InnerFactor != 16 {
		t.Fatalf("inner factor = %d, want half of the largest edge factor", factors[0].InnerFactor)
	}
	if factors[0].Level != 5 {
		t.Fatalf("level = %d, want the exponent 5 of the largest factor", factors[0].Level)
	}
	if st := p.Stats(); st.Plans != 1 {
		t.Fatalf("plan counter = %d, want 1", st.Plans)
	}
	planID, ok := p.PendingPlan()
	if !ok || planID == "" {
		t.Fatalf("a finished plan must stay pending until it is committed")
	}
}

func TestPlanClampsToMaxEdgeFactor(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(impl.Limits{MaxPatches: 4, MaxEdgeFactor: 8})
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 10)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	factors, err := p.Plan(baseView(4))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if factors[0].EdgeFactors[0] != 8 {
		t.Fatalf("edge factor = %d, want the cap 8", factors[0].EdgeFactors[0])
	}
	if factors[0].InnerFactor != 4 {
		t.Fatalf("inner factor = %d, want 4", factors[0].InnerFactor)
	}
	if factors[0].Level != 3 {
		t.Fatalf("level = %d, want 3", factors[0].Level)
	}
}

func TestPlanAppliesDisplacementHeight(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1.6)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	if err := p.SetDisplacement("p-1", 1); err != nil {
		t.Fatalf("set displacement: %v", err)
	}
	factors, err := p.Plan(baseView(5))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if factors[0].EdgeFactors[0] != 32 {
		t.Fatalf("edge factor = %d, want 32 after doubling the continuous factor", factors[0].EdgeFactors[0])
	}
	if factors[0].InnerFactor != 16 {
		t.Fatalf("inner factor = %d, want 16", factors[0].InnerFactor)
	}
}

func TestPlanRejectsBadViewAndEmptyTable(t *testing.T) {
	empty := impl.NewTessellationFactorPlanner(baseLimits())
	if _, err := empty.Plan(baseView(1)); !errors.Is(err, impl.ErrNoPatches) {
		t.Fatalf("planning without a patch must fail with ErrNoPatches, got %v", err)
	}
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	bad := baseView(2)
	bad.CameraDistance = 0
	if _, err := p.Plan(bad); !errors.Is(err, impl.ErrBadView) {
		t.Fatalf("camera distance 0 must fail with ErrBadView, got %v", err)
	}
	bad = baseView(2)
	bad.PixelErrorBudget = -1
	if _, err := p.Plan(bad); !errors.Is(err, impl.ErrBadView) {
		t.Fatalf("negative pixel budget must fail with ErrBadView, got %v", err)
	}
	bad = baseView(2)
	bad.PixelsPerUnit = nan()
	if _, err := p.Plan(bad); !errors.Is(err, impl.ErrBadView) {
		t.Fatalf("nan pixel scale must fail with ErrBadView, got %v", err)
	}
}

func TestPlanIsSortedAndDeterministic(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	for _, id := range []string{"p-c", "p-a", "p-b"} {
		if err := p.RegisterPatch(id, singleEdge(id, "e-"+id, 1)); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	first, err := p.Plan(baseView(6))
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("plan length = %d, want 3", len(first))
	}
	wantOrder := []string{"p-a", "p-b", "p-c"}
	for i, id := range wantOrder {
		if first[i].PatchID != id {
			t.Fatalf("plan[%d] = %q, want %q", i, first[i].PatchID, id)
		}
	}
	second, err := p.Plan(baseView(6))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two plans of the same view differ:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestPlanDegradesWholeTierToFitBudget(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(impl.Limits{MaxPatches: 4, MaxFactorSum: 24, MaxEdgeFactor: 64})
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1.6)); err != nil {
		t.Fatalf("register p-1: %v", err)
	}
	if err := p.RegisterPatch("p-2", singleEdge("p-2", "e-2", 1.6)); err != nil {
		t.Fatalf("register p-2: %v", err)
	}
	if err := p.SetScreenRatio("p-1", 0.2); err != nil {
		t.Fatalf("set ratio p-1: %v", err)
	}
	if err := p.SetScreenRatio("p-2", 0.8); err != nil {
		t.Fatalf("set ratio p-2: %v", err)
	}
	factors, err := p.Plan(baseView(7))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range factors {
		if f.EdgeFactors[0] != 8 {
			t.Fatalf("patch %s edge factor = %d, want the whole tier halved to 8", f.PatchID, f.EdgeFactors[0])
		}
		if f.InnerFactor != 4 {
			t.Fatalf("patch %s inner factor = %d, want 4", f.PatchID, f.InnerFactor)
		}
		if f.Level != 3 {
			t.Fatalf("patch %s level = %d, want 3", f.PatchID, f.Level)
		}
	}
	if st := p.Stats(); st.DegradeRounds != 1 {
		t.Fatalf("degrade rounds = %d, want exactly 1", st.DegradeRounds)
	}
}

func TestPlanReportsBudgetExceeded(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(impl.Limits{MaxPatches: 4, MaxFactorSum: 1, MaxEdgeFactor: 64})
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1)); err != nil {
		t.Fatalf("register p-1: %v", err)
	}
	if err := p.RegisterPatch("p-2", singleEdge("p-2", "e-2", 1)); err != nil {
		t.Fatalf("register p-2: %v", err)
	}
	if _, err := p.Plan(baseView(8)); !errors.Is(err, impl.ErrBudgetExceeded) {
		t.Fatalf("a budget below the minimum must fail with ErrBudgetExceeded, got %v", err)
	}
	if _, ok := p.Factor("p-1"); ok {
		t.Fatalf("a rejected plan must not publish factors")
	}
	if _, ok := p.PendingPlan(); ok {
		t.Fatalf("a rejected plan must not stay pending")
	}
}

func TestSetDisplacementAndRatioValidation(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	if err := p.SetDisplacement("p-404", 1); !errors.Is(err, impl.ErrUnknownPatch) {
		t.Fatalf("unknown patch must fail with ErrUnknownPatch, got %v", err)
	}
	if err := p.SetDisplacement("p-1", -1); !errors.Is(err, impl.ErrBadDisplacement) {
		t.Fatalf("negative height must fail with ErrBadDisplacement, got %v", err)
	}
	if err := p.SetDisplacement("p-1", nan()); !errors.Is(err, impl.ErrBadDisplacement) {
		t.Fatalf("nan height must fail with ErrBadDisplacement, got %v", err)
	}
	if err := p.SetScreenRatio("p-1", 1.5); !errors.Is(err, impl.ErrBadRatio) {
		t.Fatalf("ratio above 1 must fail with ErrBadRatio, got %v", err)
	}
	if err := p.SetScreenRatio("p-1", -0.1); !errors.Is(err, impl.ErrBadRatio) {
		t.Fatalf("negative ratio must fail with ErrBadRatio, got %v", err)
	}
	if err := p.SetScreenRatio("p-404", 0.5); !errors.Is(err, impl.ErrUnknownPatch) {
		t.Fatalf("ratio for an unknown patch must fail with ErrUnknownPatch, got %v", err)
	}
}

func TestCommitAppliesAndIsIdempotent(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	planned, err := p.Plan(baseView(9))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planID, ok := p.PendingPlan()
	if !ok {
		t.Fatalf("pending plan must be reported before the commit")
	}
	if err := p.Commit(planID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	applied, ok := p.Factor("p-1")
	if !ok {
		t.Fatalf("committed factors must be readable")
	}
	if !reflect.DeepEqual(applied, planned[0]) {
		t.Fatalf("committed factors = %+v, want the planned %+v", applied, planned[0])
	}
	if err := p.Commit(planID); err != nil {
		t.Fatalf("committing the same plan twice must succeed, got %v", err)
	}
	if st := p.Stats(); st.Commits != 1 {
		t.Fatalf("commit counter = %d, want 1", st.Commits)
	}
	if err := p.Commit("plan-404"); !errors.Is(err, impl.ErrUnknownPlan) {
		t.Fatalf("unknown plan must fail with ErrUnknownPlan, got %v", err)
	}
}

func TestPendingPlanIsSupersededAndCancellable(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	if _, err := p.Plan(baseView(10)); err != nil {
		t.Fatalf("first plan: %v", err)
	}
	first, _ := p.PendingPlan()
	if _, err := p.Plan(baseView(11)); err != nil {
		t.Fatalf("second plan: %v", err)
	}
	second, _ := p.PendingPlan()
	if first == second {
		t.Fatalf("a newer plan must get a new id, both were %q", first)
	}
	if err := p.Commit(first); !errors.Is(err, impl.ErrUnknownPlan) {
		t.Fatalf("a superseded plan must fail with ErrUnknownPlan, got %v", err)
	}
	if err := p.Cancel(second); err != nil {
		t.Fatalf("cancel the pending plan: %v", err)
	}
	if _, ok := p.PendingPlan(); ok {
		t.Fatalf("no plan may stay pending after the cancel")
	}
	if err := p.Commit(second); !errors.Is(err, impl.ErrUnknownPlan) {
		t.Fatalf("a cancelled plan must fail with ErrUnknownPlan, got %v", err)
	}
	if err := p.Cancel("plan-404"); !errors.Is(err, impl.ErrUnknownPlan) {
		t.Fatalf("cancelling an unknown plan must fail with ErrUnknownPlan, got %v", err)
	}
	if st := p.Stats(); st.Cancels != 1 {
		t.Fatalf("cancel counter = %d, want 1", st.Cancels)
	}
}

func TestSnapshotRestoreKeepsCommittedPlan(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1.6)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	if err := p.SetDisplacement("p-1", 1); err != nil {
		t.Fatalf("set displacement: %v", err)
	}
	if err := p.SetScreenRatio("p-1", 0.4); err != nil {
		t.Fatalf("set ratio: %v", err)
	}
	if _, err := p.Plan(baseView(21)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	planID, _ := p.PendingPlan()
	if err := p.Commit(planID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewTessellationFactorPlanner(baseLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, ok := restored.Factor("p-1")
	if !ok {
		t.Fatalf("restored planner lost the committed factors")
	}
	if got.EdgeFactors[0] != 32 || got.InnerFactor != 16 {
		t.Fatalf("restored factors = %+v, want the edge 32 and inner 16", got)
	}
	if st := restored.Stats(); st.CommittedFrameID != 21 {
		t.Fatalf("restored frame id = %d, want 21", st.CommittedFrameID)
	}
	if err := restored.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := restored.Factor("p-1"); !ok {
		t.Fatalf("a rejected restore must not drop the live state")
	}
}

func TestConcurrentPlanAndCommit(t *testing.T) {
	p := impl.NewTessellationFactorPlanner(baseLimits())
	if err := p.RegisterPatch("p-1", singleEdge("p-1", "e-1", 1.6)); err != nil {
		t.Fatalf("register patch: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := p.Plan(baseView(int64(30 + i))); err != nil {
				return
			}
			planID, ok := p.PendingPlan()
			if !ok {
				return
			}
			_ = p.Commit(planID)
		}(i)
	}
	wg.Wait()
	if st := p.Stats(); st.Plans != 8 {
		t.Fatalf("plan counter = %d, want 8", st.Plans)
	}
	applied, ok := p.Factor("p-1")
	if !ok {
		t.Fatalf("at least one concurrent commit must publish factors")
	}
	if len(applied.EdgeFactors) != 1 || applied.InnerFactor != applied.EdgeFactors[0]/2 {
		t.Fatalf("published factors = %+v, want a self consistent pair", applied)
	}
}
