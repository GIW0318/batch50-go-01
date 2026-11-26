package task

import (
	"errors"
	"math"
	"testing"

	impl "gsb/batch50-go-game-b-11"
)

func testPickConfig() impl.PickConfig {
	return impl.PickConfig{MaxInstances: 16, MaxQueriesPerFrame: 8, MaxNodesPerFrame: 256}
}

func pickBox(x0, y0, z0, x1, y1, z1 float64) impl.AABB {
	return impl.AABB{Min: impl.Vec3{X: x0, Y: y0, Z: z0}, Max: impl.Vec3{X: x1, Y: y1, Z: z1}}
}

func forwardRay() impl.Ray {
	return impl.Ray{
		Origin: impl.Vec3{X: 0, Y: 0, Z: 0},
		Dir:    impl.Vec3{X: 0, Y: 0, Z: 1},
	}
}

func pickQuery(mask uint32, maxDist float64) impl.PickQuery {
	return impl.PickQuery{Ray: forwardRay(), LayerMask: mask, MaxDistance: maxDist}
}

func solidInstance(id string, z0, z1 float64) impl.Instance {
	return impl.Instance{ID: id, Layer: "solid", Box: pickBox(-1, -1, z0, 1, 1, z1)}
}

func mustAdd(t *testing.T, s *impl.PickQueryService, inst impl.Instance) {
	t.Helper()
	if err := s.AddInstance(inst); err != nil {
		t.Fatalf("register instance %q: %v", inst.ID, err)
	}
}

func mustMask(t *testing.T, s *impl.PickQueryService, layer string, mask uint32) {
	t.Helper()
	if err := s.SetLayerMask(layer, mask); err != nil {
		t.Fatalf("bind layer %q: %v", layer, err)
	}
}

func TestAddInstanceRejectsBrokenRecords(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	good := pickBox(-1, -1, 5, 1, 1, 6)
	if err := s.AddInstance(impl.Instance{ID: "", Layer: "solid", Box: good}); !errors.Is(err, impl.ErrBadInstance) {
		t.Fatalf("empty id must be rejected with ErrBadInstance, got %v", err)
	}
	if err := s.AddInstance(impl.Instance{ID: "a", Layer: "  ", Box: good}); !errors.Is(err, impl.ErrBadInstance) {
		t.Fatalf("blank layer must be rejected with ErrBadInstance, got %v", err)
	}
	if err := s.AddInstance(impl.Instance{ID: "a", Layer: "solid", Box: pickBox(0, 0, 8, 0, 0, 4)}); !errors.Is(err, impl.ErrBadInstance) {
		t.Fatalf("inverted box must be rejected with ErrBadInstance, got %v", err)
	}
	nan := pickBox(0, 0, 5, 1, 1, 6)
	nan.Min.X = math.NaN()
	if err := s.AddInstance(impl.Instance{ID: "a", Layer: "solid", Box: nan}); !errors.Is(err, impl.ErrBadInstance) {
		t.Fatalf("NaN bounds must be rejected with ErrBadInstance, got %v", err)
	}
	if got := s.Instances(); len(got) != 0 {
		t.Fatalf("rejected records must not enter the table, got %v", got)
	}
}

func TestAddInstanceRejectsDuplicateAndOverflow(t *testing.T) {
	s := impl.NewPickQueryService(impl.PickConfig{MaxInstances: 2, MaxQueriesPerFrame: 4, MaxNodesPerFrame: 32})
	mustAdd(t, s, solidInstance("a", 5, 6))
	if err := s.AddInstance(solidInstance("a", 5, 6)); !errors.Is(err, impl.ErrDuplicateInstance) {
		t.Fatalf("duplicate id must be rejected with ErrDuplicateInstance, got %v", err)
	}
	mustAdd(t, s, solidInstance("b", 9, 10))
	if err := s.AddInstance(solidInstance("c", 13, 14)); !errors.Is(err, impl.ErrInstanceLimit) {
		t.Fatalf("third instance must hit ErrInstanceLimit, got %v", err)
	}
	ids := s.Instances()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("instance table = %v, want the ascending pair [a b]", ids)
	}
}

func TestUpdateAndRemoveInstanceTrackGeneration(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("a", 5, 6))
	first, ok := s.Instance("a")
	if !ok {
		t.Fatalf("registered instance must be readable")
	}
	if first.Generation == 0 {
		t.Fatalf("a fresh instance must carry a non zero generation")
	}
	if err := s.UpdateInstance("a", pickBox(-1, -1, 12, 1, 1, 13)); err != nil {
		t.Fatalf("update instance: %v", err)
	}
	second, _ := s.Instance("a")
	if second.Generation <= first.Generation {
		t.Fatalf("update generation = %d, must advance past %d", second.Generation, first.Generation)
	}
	if second.Box.Min.Z != 12 {
		t.Fatalf("updated box min z = %v, want 12", second.Box.Min.Z)
	}
	if err := s.UpdateInstance("ghost", pickBox(0, 0, 1, 1, 1, 2)); !errors.Is(err, impl.ErrUnknownInstance) {
		t.Fatalf("unknown instance update must fail with ErrUnknownInstance, got %v", err)
	}
	if err := s.UpdateInstance("a", pickBox(0, 0, 9, 0, 0, 3)); !errors.Is(err, impl.ErrBadInstance) {
		t.Fatalf("inverted box must fail with ErrBadInstance, got %v", err)
	}
	after, _ := s.Instance("a")
	if after.Generation != second.Generation {
		t.Fatalf("rejected update moved the generation to %d, want it pinned at %d", after.Generation, second.Generation)
	}
	if err := s.RemoveInstance("a"); err != nil {
		t.Fatalf("remove instance: %v", err)
	}
	if _, ok := s.Instance("a"); ok {
		t.Fatalf("removed instance must disappear from the table")
	}
	if err := s.RemoveInstance("a"); !errors.Is(err, impl.ErrUnknownInstance) {
		t.Fatalf("second removal must fail with ErrUnknownInstance, got %v", err)
	}
}

func TestSetLayerMaskBindsSingleBit(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustMask(t, s, "solid", 1<<0)
	if mask, ok := s.LayerMask("solid"); !ok || mask != 1 {
		t.Fatalf("layer mask = %d ok=%v, want 1/true", mask, ok)
	}
	if err := s.SetLayerMask("", 1<<1); !errors.Is(err, impl.ErrBadLayer) {
		t.Fatalf("blank layer must fail with ErrBadLayer, got %v", err)
	}
	if err := s.SetLayerMask("water", 0); !errors.Is(err, impl.ErrBadLayer) {
		t.Fatalf("zero mask must fail with ErrBadLayer, got %v", err)
	}
	if err := s.SetLayerMask("water", 0b11); !errors.Is(err, impl.ErrBadLayer) {
		t.Fatalf("multi bit mask must fail with ErrBadLayer, got %v", err)
	}
	if err := s.SetLayerMask("solid", 1<<1); !errors.Is(err, impl.ErrLayerConflict) {
		t.Fatalf("rebinding the same layer must fail with ErrLayerConflict, got %v", err)
	}
	if err := s.SetLayerMask("glass", 1<<0); !errors.Is(err, impl.ErrLayerConflict) {
		t.Fatalf("sharing one bit between layers must fail with ErrLayerConflict, got %v", err)
	}
	if _, ok := s.LayerMask("glass"); ok {
		t.Fatalf("rejected layer binding must not be stored")
	}
}

func TestSubmitRejectsBrokenQuery(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("a", 5, 6))
	mustMask(t, s, "solid", 1<<0)
	skewed := pickQuery(1, 100)
	skewed.Ray.Dir = impl.Vec3{X: 0, Y: 0, Z: 2}
	if _, err := s.Submit(skewed); !errors.Is(err, impl.ErrBadQuery) {
		t.Fatalf("non unit direction must fail with ErrBadQuery, got %v", err)
	}
	if _, err := s.Submit(pickQuery(1, 0)); !errors.Is(err, impl.ErrBadQuery) {
		t.Fatalf("zero max distance must fail with ErrBadQuery, got %v", err)
	}
	if _, err := s.Submit(pickQuery(0, 100)); !errors.Is(err, impl.ErrBadQuery) {
		t.Fatalf("zero layer mask must fail with ErrBadQuery, got %v", err)
	}
	if got := s.Stats().Queued; got != 0 {
		t.Fatalf("rejected queries queued %d entries, want 0", got)
	}
}

func TestRunFrameReturnsNearestHit(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("far", 9, 10))
	mustAdd(t, s, solidInstance("near", 5, 6))
	mustMask(t, s, "solid", 1<<0)
	id, err := s.Submit(pickQuery(1, 100))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id == "" {
		t.Fatalf("submit must hand back a query id")
	}
	if got := s.RunFrame(); got != 1 {
		t.Fatalf("RunFrame executed %d queries, want 1", got)
	}
	hit, ok, err := s.Resolve(id)
	if err != nil || !ok {
		t.Fatalf("resolve = ok=%v err=%v, want a resolved query", ok, err)
	}
	if hit.InstanceID != "near" {
		t.Fatalf("hit instance = %q, want the nearest one", hit.InstanceID)
	}
	if hit.Distance < 4.999 || hit.Distance > 5.001 {
		t.Fatalf("hit distance = %v, want 5", hit.Distance)
	}
	if hit.Layer != "solid" {
		t.Fatalf("hit layer = %q, want solid", hit.Layer)
	}
	inst, _ := s.Instance("near")
	if hit.Generation != inst.Generation {
		t.Fatalf("hit generation = %d, want the live instance generation %d", hit.Generation, inst.Generation)
	}
	if hit.QueryID != id {
		t.Fatalf("hit query id = %q, want %q", hit.QueryID, id)
	}
}

func TestRunFrameTieBreaksByInstanceID(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("z-inst", 5, 6))
	mustAdd(t, s, solidInstance("a-inst", 5, 6))
	mustMask(t, s, "solid", 1<<0)
	id, err := s.Submit(pickQuery(1, 100))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	s.RunFrame()
	hit, ok, err := s.Resolve(id)
	if err != nil || !ok {
		t.Fatalf("resolve = ok=%v err=%v, want a resolved query", ok, err)
	}
	if hit.InstanceID != "a-inst" {
		t.Fatalf("tie break picked %q, want the smaller id a-inst", hit.InstanceID)
	}
}

func TestRunFrameHonoursMaxDistanceAndRemoval(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("near", 5, 6))
	mustAdd(t, s, solidInstance("far", 9, 10))
	mustMask(t, s, "solid", 1<<0)
	id, err := s.Submit(pickQuery(1, 7))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	s.RunFrame()
	hit, ok, err := s.Resolve(id)
	if err != nil || !ok {
		t.Fatalf("resolve = ok=%v err=%v, want a resolved query", ok, err)
	}
	if hit.InstanceID != "near" {
		t.Fatalf("hit = %q, want near because far is past the max distance", hit.InstanceID)
	}
	missID, err := s.Submit(pickQuery(1, 4))
	if err != nil {
		t.Fatalf("submit short query: %v", err)
	}
	s.RunFrame()
	miss, ok, err := s.Resolve(missID)
	if err != nil || !ok {
		t.Fatalf("short query = ok=%v err=%v, want it executed", ok, err)
	}
	if miss.InstanceID != "" {
		t.Fatalf("short query hit %q, want an empty instance id", miss.InstanceID)
	}
	if err := s.RemoveInstance("near"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	third, err := s.Submit(pickQuery(1, 100))
	if err != nil {
		t.Fatalf("submit after removal: %v", err)
	}
	s.RunFrame()
	after, _, err := s.Resolve(third)
	if err != nil {
		t.Fatalf("resolve after removal: %v", err)
	}
	if after.InstanceID != "far" {
		t.Fatalf("hit after removal = %q, want far", after.InstanceID)
	}
}

func TestRunFrameRespectsQueryBudget(t *testing.T) {
	s := impl.NewPickQueryService(impl.PickConfig{MaxInstances: 8, MaxQueriesPerFrame: 2, MaxNodesPerFrame: 64})
	mustAdd(t, s, solidInstance("a", 5, 6))
	mustMask(t, s, "solid", 1<<0)
	ids := make([]impl.QueryID, 0, 5)
	for i := 0; i < 5; i++ {
		id, err := s.Submit(pickQuery(1, 100+float64(i)))
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if got := s.RunFrame(); got != 2 {
		t.Fatalf("first frame executed %d queries, want the budget 2", got)
	}
	st := s.Stats()
	if st.Executed != 2 || st.Queued != 3 || st.Deferred != 3 {
		t.Fatalf("stats after frame 1 = executed %d queued %d deferred %d, want 2/3/3", st.Executed, st.Queued, st.Deferred)
	}
	if _, ok, err := s.Resolve(ids[2]); err != nil || ok {
		t.Fatalf("deferred query = ok=%v err=%v, want pending without error", ok, err)
	}
	if got := s.RunFrame(); got != 2 {
		t.Fatalf("second frame executed %d queries, want 2", got)
	}
	if got := s.RunFrame(); got != 1 {
		t.Fatalf("third frame executed %d queries, want the last one", got)
	}
	if st := s.Stats(); st.Queued != 0 || st.Frame != 3 {
		t.Fatalf("final stats queued %d frame %d, want 0/3", st.Queued, st.Frame)
	}
}

func TestRunFrameRespectsNodeBudget(t *testing.T) {
	s := impl.NewPickQueryService(impl.PickConfig{MaxInstances: 8, MaxQueriesPerFrame: 8, MaxNodesPerFrame: 4})
	mustAdd(t, s, solidInstance("a", 5, 6))
	mustAdd(t, s, solidInstance("b", 7, 8))
	mustAdd(t, s, solidInstance("c", 9, 10))
	mustMask(t, s, "solid", 1<<0)
	for i := 0; i < 3; i++ {
		if _, err := s.Submit(pickQuery(1, 100+float64(i))); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if got := s.RunFrame(); got != 1 {
		t.Fatalf("frame executed %d queries, want 1 because the node budget only fits one", got)
	}
	st := s.Stats()
	if st.NodesVisited != 3 {
		t.Fatalf("visited nodes = %d, want 3 for the single executed query", st.NodesVisited)
	}
	if st.Queued != 2 || st.Deferred != 2 {
		t.Fatalf("stats queued %d deferred %d, want 2/2", st.Queued, st.Deferred)
	}
	if got := s.RunFrame(); got != 1 {
		t.Fatalf("next frame executed %d queries, want 1 with the fresh node budget", got)
	}
}

func TestResolveRejectsUnknownQuery(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	if _, _, err := s.Resolve("q-404"); !errors.Is(err, impl.ErrUnknownQuery) {
		t.Fatalf("unknown query must fail with ErrUnknownQuery, got %v", err)
	}
	if _, _, err := s.Resolve(""); !errors.Is(err, impl.ErrUnknownQuery) {
		t.Fatalf("empty query id must fail with ErrUnknownQuery, got %v", err)
	}
}

func TestResolveBatchKeepsInputOrderAndMisses(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("near", 5, 6))
	mustAdd(t, s, solidInstance("far", 9, 10))
	mustMask(t, s, "solid", 1<<0)
	hits, err := s.ResolveBatch([]impl.PickQuery{pickQuery(1, 100), pickQuery(1, 2)})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("batch returned %d hits, want one per input query", len(hits))
	}
	if hits[0].InstanceID != "near" {
		t.Fatalf("first batch hit = %q, want near", hits[0].InstanceID)
	}
	if hits[1].InstanceID != "" || hits[1].Distance != 0 {
		t.Fatalf("second batch hit = %+v, want an empty miss", hits[1])
	}
	bad, err := s.ResolveBatch([]impl.PickQuery{pickQuery(1, 100), pickQuery(0, 100)})
	if !errors.Is(err, impl.ErrBadQuery) {
		t.Fatalf("batch with a broken query must fail with ErrBadQuery, got %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("failed batch returned %d partial hits, want none", len(bad))
	}
	st := s.Stats()
	if st.Submitted != 0 || st.Frame != 0 || st.NodesVisited != 0 {
		t.Fatalf("batch must not touch the frame counters, got %+v", st)
	}
}

func TestStatsCountSubmittedAndExecuted(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("a", 5, 6))
	mustMask(t, s, "solid", 1<<0)
	for i := 0; i < 3; i++ {
		if _, err := s.Submit(pickQuery(1, 100+float64(i))); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if st := s.Stats(); st.Submitted != 3 || st.Queued != 3 || st.Frame != 0 {
		t.Fatalf("stats before running = %+v, want 3 submitted, 3 queued, frame 0", st)
	}
	if got := s.RunFrame(); got != 3 {
		t.Fatalf("frame executed %d queries, want 3", got)
	}
	st := s.Stats()
	if st.Frame != 1 || st.Executed != 3 || st.Queued != 0 || st.Deferred != 0 || st.NodesVisited != 3 {
		t.Fatalf("stats after frame = %+v, want frame 1, executed 3, nodes 3", st)
	}
}

func TestSnapshotRestoreKeepsQueueAndGeneration(t *testing.T) {
	cfg := impl.PickConfig{MaxInstances: 8, MaxQueriesPerFrame: 1, MaxNodesPerFrame: 64}
	s := impl.NewPickQueryService(cfg)
	mustAdd(t, s, solidInstance("a", 5, 6))
	mustAdd(t, s, solidInstance("b", 9, 10))
	mustMask(t, s, "solid", 1<<0)
	genA, _ := s.Instance("a")
	if _, err := s.Submit(pickQuery(1, 100)); err != nil {
		t.Fatalf("submit first: %v", err)
	}
	second, err := s.Submit(pickQuery(1, 50))
	if err != nil {
		t.Fatalf("submit second: %v", err)
	}
	if got := s.RunFrame(); got != 1 {
		t.Fatalf("first frame executed %d queries, want 1", got)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewPickQueryService(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ids := restored.Instances()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("restored instances = %v, want [a b]", ids)
	}
	back, ok := restored.Instance("a")
	if !ok || back.Generation != genA.Generation {
		t.Fatalf("restored generation = %d ok=%v, want %d", back.Generation, ok, genA.Generation)
	}
	if _, ok, err := restored.Resolve(second); err != nil || ok {
		t.Fatalf("restored pending query = ok=%v err=%v, want it still queued", ok, err)
	}
	if got := restored.RunFrame(); got != 1 {
		t.Fatalf("restored frame executed %d queries, want the carried over one", got)
	}
	hit, ok, err := restored.Resolve(second)
	if err != nil || !ok {
		t.Fatalf("resolve carried query = ok=%v err=%v", ok, err)
	}
	if hit.InstanceID != "a" {
		t.Fatalf("carried query hit %q, want a", hit.InstanceID)
	}
	if st := restored.Stats(); st.Frame != 2 {
		t.Fatalf("restored frame counter = %d, want 2 after the carried frame", st.Frame)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	s := impl.NewPickQueryService(testPickConfig())
	mustAdd(t, s, solidInstance("a", 5, 6))
	before, _ := s.Instance("a")
	if err := s.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	ids := s.Instances()
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("failed restore touched the live table, got %v", ids)
	}
	after, ok := s.Instance("a")
	if !ok || after.Generation != before.Generation {
		t.Fatalf("failed restore moved the generation to %d, want %d", after.Generation, before.Generation)
	}
}
