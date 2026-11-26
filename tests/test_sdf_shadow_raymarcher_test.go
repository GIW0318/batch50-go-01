package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-10"
)

func marcherLimits() impl.Limits {
	return impl.Limits{MaxFields: 4, MaxLights: 4, MaxVoxels: 16, MaxSteps: 32, BaseBias: 0.002}
}

func unitBox() impl.Voxel {
	return impl.Voxel{Min: impl.Vec3{X: -1, Y: -1, Z: -1}, Max: impl.Vec3{X: 1, Y: 1, Z: 1}, Density: 1}
}

func keyLight() impl.LightSpec {
	return impl.LightSpec{Position: impl.Vec3{X: 0, Y: 5, Z: 0}, Radius: 0.5, Intensity: 1}
}

func readyMarcher(t *testing.T) *impl.SdfShadowRaymarcher {
	t.Helper()
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	if err := m.LoadField("room", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("load room: %v", err)
	}
	if err := m.SetLight("key", keyLight()); err != nil {
		t.Fatalf("set key light: %v", err)
	}
	return m
}

func TestNewNormalisesLimits(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(impl.Limits{})
	got := m.Limits()
	if got.MaxFields != impl.DefaultMaxFields || got.MaxLights != impl.DefaultMaxLights {
		t.Fatalf("default table caps = %+v, want %d fields and %d lights", got, impl.DefaultMaxFields, impl.DefaultMaxLights)
	}
	if got.MaxVoxels != impl.DefaultMaxVoxels || got.MaxSteps != impl.DefaultMaxSteps {
		t.Fatalf("default march caps = %+v, want %d voxels and %d steps", got, impl.DefaultMaxVoxels, impl.DefaultMaxSteps)
	}
	if got.BaseBias != impl.DefaultBaseBias {
		t.Fatalf("default base bias = %v, want %v", got.BaseBias, impl.DefaultBaseBias)
	}
	if steps := m.StepBudget(); steps != impl.DefaultMaxSteps {
		t.Fatalf("default step budget = %d, want %d", steps, impl.DefaultMaxSteps)
	}
	custom := impl.NewSdfShadowRaymarcher(impl.Limits{MaxFields: -2, MaxSteps: 8, BaseBias: 0.004})
	cg := custom.Limits()
	if cg.MaxFields != impl.DefaultMaxFields {
		t.Fatalf("negative field cap fell back to %d, want %d", cg.MaxFields, impl.DefaultMaxFields)
	}
	if cg.MaxSteps != 8 || cg.BaseBias != 0.004 {
		t.Fatalf("explicit caps lost: %+v", cg)
	}
	if steps := custom.StepBudget(); steps != 8 {
		t.Fatalf("step budget = %d, want 8", steps)
	}
}

func TestLoadFieldStoresVoxels(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	extra := impl.Voxel{Min: impl.Vec3{X: 2, Y: 0, Z: 0}, Max: impl.Vec3{X: 3, Y: 1, Z: 1}, Density: 0.75}
	if err := m.LoadField("room", []impl.Voxel{unitBox(), extra}); err != nil {
		t.Fatalf("load room: %v", err)
	}
	if fields := m.Fields(); len(fields) != 1 || fields[0] != "room" {
		t.Fatalf("fields = %v, want [room]", fields)
	}
	if field, light := m.Active(); field != "room" || light != "" {
		t.Fatalf("active = (%q,%q), want the room field and no light", field, light)
	}
	stats := m.Stats()
	if stats.Fields != 1 || stats.Voxels != 2 {
		t.Fatalf("stats = %+v, want one field with two voxels", stats)
	}
	if err := m.LoadField("attic", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("load attic: %v", err)
	}
	fields := m.Fields()
	if len(fields) != 2 || fields[0] != "attic" || fields[1] != "room" {
		t.Fatalf("fields = %v, want ascending ids", fields)
	}
	if field, _ := m.Active(); field != "room" {
		t.Fatalf("loading a second field must not steal the selection, active = %q", field)
	}
}

func TestLoadFieldRejectsBadField(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	if err := m.LoadField("", []impl.Voxel{unitBox()}); !errors.Is(err, impl.ErrBadField) {
		t.Fatalf("blank field id must fail with ErrBadField, got %v", err)
	}
	if err := m.LoadField("   ", []impl.Voxel{unitBox()}); !errors.Is(err, impl.ErrBadField) {
		t.Fatalf("whitespace field id must fail with ErrBadField, got %v", err)
	}
	if err := m.LoadField("room", nil); !errors.Is(err, impl.ErrBadField) {
		t.Fatalf("nil voxel slice must fail with ErrBadField, got %v", err)
	}
	if err := m.LoadField("room", []impl.Voxel{}); !errors.Is(err, impl.ErrBadField) {
		t.Fatalf("empty voxel slice must fail with ErrBadField, got %v", err)
	}
	if fields := m.Fields(); len(fields) != 0 {
		t.Fatalf("rejected fields must not enter the table, got %v", fields)
	}
}

func TestLoadFieldRejectsBadVoxel(t *testing.T) {
	cases := [][]impl.Voxel{
		{{Min: impl.Vec3{X: 1, Y: 0, Z: 0}, Max: impl.Vec3{X: -1, Y: 1, Z: 1}, Density: 1}},
		{{Min: impl.Vec3{X: math.NaN(), Y: 0, Z: 0}, Max: impl.Vec3{X: 1, Y: 1, Z: 1}, Density: 1}},
		{{Min: impl.Vec3{}, Max: impl.Vec3{X: 1, Y: 1, Z: 1}, Density: -0.1}},
		{{Min: impl.Vec3{}, Max: impl.Vec3{X: 1, Y: 1, Z: 1}, Density: 1.5}},
	}
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	for i, voxels := range cases {
		if err := m.LoadField("room", voxels); !errors.Is(err, impl.ErrBadVoxel) {
			t.Fatalf("voxel case %d must fail with ErrBadVoxel, got %v", i, err)
		}
	}
	if fields := m.Fields(); len(fields) != 0 {
		t.Fatalf("rejected voxels must not create a field, got %v", fields)
	}
	if err := m.LoadField("room", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("a valid field after rejected ones: %v", err)
	}
}

func TestLoadFieldEnforcesLimits(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(impl.Limits{MaxFields: 2, MaxLights: 4, MaxVoxels: 2, MaxSteps: 16})
	if err := m.LoadField("f1", []impl.Voxel{unitBox(), unitBox()}); err != nil {
		t.Fatalf("load f1: %v", err)
	}
	if err := m.LoadField("f2", []impl.Voxel{unitBox(), unitBox(), unitBox()}); !errors.Is(err, impl.ErrVoxelLimit) {
		t.Fatalf("three voxels over a cap of two must fail with ErrVoxelLimit, got %v", err)
	}
	if err := m.LoadField("f2", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("load f2: %v", err)
	}
	if err := m.LoadField("f3", []impl.Voxel{unitBox()}); !errors.Is(err, impl.ErrFieldLimit) {
		t.Fatalf("third field over a cap of two must fail with ErrFieldLimit, got %v", err)
	}
	if fields := m.Fields(); len(fields) != 2 {
		t.Fatalf("field count = %d, want 2", len(fields))
	}
}

func TestSetLightValidates(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(impl.Limits{MaxFields: 2, MaxLights: 2, MaxVoxels: 8, MaxSteps: 16})
	bad := []impl.LightSpec{
		{Radius: -1, Intensity: 1},
		{Radius: 0, Intensity: 0},
		{Radius: math.NaN(), Intensity: 1},
		{Radius: 0, Intensity: math.Inf(1)},
	}
	for i, light := range bad {
		if err := m.SetLight("key", light); !errors.Is(err, impl.ErrBadLight) {
			t.Fatalf("light case %d must fail with ErrBadLight, got %v", i, err)
		}
	}
	if err := m.SetLight("", keyLight()); !errors.Is(err, impl.ErrBadLight) {
		t.Fatalf("blank light id must fail with ErrBadLight, got %v", err)
	}
	if err := m.SetLight("key", keyLight()); err != nil {
		t.Fatalf("set key light: %v", err)
	}
	if err := m.SetLight("fill", impl.LightSpec{Position: impl.Vec3{X: 3}, Intensity: 0.5}); err != nil {
		t.Fatalf("set fill light: %v", err)
	}
	if err := m.SetLight("rim", impl.LightSpec{Intensity: 0.5}); !errors.Is(err, impl.ErrLightLimit) {
		t.Fatalf("third light over a cap of two must fail with ErrLightLimit, got %v", err)
	}
	if lights := m.Lights(); len(lights) != 2 || lights[0] != "fill" || lights[1] != "key" {
		t.Fatalf("lights = %v, want ascending ids", lights)
	}
	if _, light := m.Active(); light != "key" {
		t.Fatalf("the first registered light must stay active, got %q", light)
	}
}

func TestSelectorsRejectUnknownIDs(t *testing.T) {
	m := readyMarcher(t)
	if err := m.LoadField("attic", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("load attic: %v", err)
	}
	if err := m.SetLight("fill", impl.LightSpec{Intensity: 2}); err != nil {
		t.Fatalf("set fill light: %v", err)
	}
	if err := m.SetActiveField("attic"); err != nil {
		t.Fatalf("switch field: %v", err)
	}
	if field, _ := m.Active(); field != "attic" {
		t.Fatalf("active field = %q, want attic", field)
	}
	if err := m.SetActiveField("cellar"); !errors.Is(err, impl.ErrUnknownField) {
		t.Fatalf("unknown field must fail with ErrUnknownField, got %v", err)
	}
	if field, _ := m.Active(); field != "attic" {
		t.Fatalf("a rejected switch must keep the selection, got %q", field)
	}
	if err := m.SetActiveLight("fill"); err != nil {
		t.Fatalf("switch light: %v", err)
	}
	if _, light := m.Active(); light != "fill" {
		t.Fatalf("active light = %q, want fill", light)
	}
	if err := m.SetActiveLight("flash"); !errors.Is(err, impl.ErrUnknownLight) {
		t.Fatalf("unknown light must fail with ErrUnknownLight, got %v", err)
	}
}

func TestTraceRequiresFieldAndLight(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	if _, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10); !errors.Is(err, impl.ErrNoField) {
		t.Fatalf("tracing without a field must fail with ErrNoField, got %v", err)
	}
	if err := m.LoadField("room", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("load room: %v", err)
	}
	if _, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10); !errors.Is(err, impl.ErrNoLight) {
		t.Fatalf("tracing without a light must fail with ErrNoLight, got %v", err)
	}
	if err := m.SetLight("key", keyLight()); err != nil {
		t.Fatalf("set key light: %v", err)
	}
	if _, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10); err != nil {
		t.Fatalf("trace with a field and a light: %v", err)
	}
}

func TestTraceRejectsBadRay(t *testing.T) {
	m := readyMarcher(t)
	bad := []struct {
		origin  impl.Vec3
		dir     impl.Vec3
		maxDist float64
	}{
		{impl.Vec3{Z: -5}, impl.Vec3{}, 10},
		{impl.Vec3{Z: -5}, impl.Vec3{X: math.NaN(), Z: 1}, 10},
		{impl.Vec3{Z: -5}, impl.Vec3{Z: math.Inf(1)}, 10},
		{impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 0},
		{impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, -5},
		{impl.Vec3{Z: math.NaN()}, impl.Vec3{Z: 1}, 10},
	}
	for i, ray := range bad {
		if _, err := m.Trace(ray.origin, ray.dir, ray.maxDist); !errors.Is(err, impl.ErrBadRay) {
			t.Fatalf("ray case %d must fail with ErrBadRay, got %v", i, err)
		}
	}
	if stats := m.Stats(); stats.Traces != 0 {
		t.Fatalf("rejected rays must not be counted, got %d", stats.Traces)
	}
}

func TestTraceHitsOccluder(t *testing.T) {
	m := readyMarcher(t)
	res, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if !res.Occluded {
		t.Fatalf("a ray into the box must be occluded, got %+v", res)
	}
	if !res.Certain {
		t.Fatalf("a hit found inside the step budget must be certain, got %+v", res)
	}
	if res.Steps <= 0 || res.Steps > 32 {
		t.Fatalf("step count = %d, want between 1 and the budget", res.Steps)
	}
	if res.Depth < 3.5 || res.Depth > 4.5 {
		t.Fatalf("hit depth = %v, want about 4", res.Depth)
	}
	if res.Penumbra <= 0 || res.Penumbra > 1 {
		t.Fatalf("penumbra = %v, want a value in (0,1] for a lit area light", res.Penumbra)
	}
	if stats := m.Stats(); stats.Traces != 1 || stats.Steps <= 0 {
		t.Fatalf("stats = %+v, want one traced ray with a positive step count", stats)
	}
}

func TestTraceClearPathIsCertain(t *testing.T) {
	m := readyMarcher(t)
	behind, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: -1}, 10)
	if err != nil {
		t.Fatalf("trace away from the box: %v", err)
	}
	if behind.Occluded {
		t.Fatalf("a ray leaving the box must not be occluded, got %+v", behind)
	}
	if !behind.Certain {
		t.Fatalf("a finished clear march must be certain, got %+v", behind)
	}
	if behind.Depth != 10 {
		t.Fatalf("clear march depth = %v, want maxDist 10", behind.Depth)
	}
	if behind.Penumbra != 0 {
		t.Fatalf("clear march penumbra = %v, want 0", behind.Penumbra)
	}
	beside, err := m.Trace(impl.Vec3{X: 3, Z: -5}, impl.Vec3{Z: 1}, 10)
	if err != nil {
		t.Fatalf("trace beside the box: %v", err)
	}
	if beside.Occluded || !beside.Certain || beside.Depth != 10 {
		t.Fatalf("a ray passing beside the box = %+v, want a certain miss at maxDist", beside)
	}
}

func TestSetStepBudgetValidates(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	if err := m.SetStepBudget(0); !errors.Is(err, impl.ErrBadStepBudget) {
		t.Fatalf("zero budget must fail with ErrBadStepBudget, got %v", err)
	}
	if err := m.SetStepBudget(-4); !errors.Is(err, impl.ErrBadStepBudget) {
		t.Fatalf("negative budget must fail with ErrBadStepBudget, got %v", err)
	}
	if steps := m.StepBudget(); steps != 32 {
		t.Fatalf("budget after rejected calls = %d, want 32", steps)
	}
	if err := m.SetStepBudget(8); err != nil {
		t.Fatalf("set budget 8: %v", err)
	}
	if steps := m.StepBudget(); steps != 8 {
		t.Fatalf("budget = %d, want 8", steps)
	}
}

func TestBiasGrowsWithDistance(t *testing.T) {
	m := impl.NewSdfShadowRaymarcher(marcherLimits())
	if err := m.LoadField("room", []impl.Voxel{unitBox()}); err != nil {
		t.Fatalf("load room: %v", err)
	}
	if _, err := m.Bias(-1); !errors.Is(err, impl.ErrBadDistance) {
		t.Fatalf("negative distance must fail with ErrBadDistance, got %v", err)
	}
	if _, err := m.Bias(math.NaN()); !errors.Is(err, impl.ErrBadDistance) {
		t.Fatalf("NaN distance must fail with ErrBadDistance, got %v", err)
	}
	b0, err := m.Bias(0)
	if err != nil {
		t.Fatalf("bias at zero: %v", err)
	}
	if math.Abs(b0-0.002) > 1e-12 {
		t.Fatalf("bias at zero distance = %v, want the base bias 0.002", b0)
	}
	b1, err := m.Bias(1)
	if err != nil {
		t.Fatalf("bias at one: %v", err)
	}
	b2, err := m.Bias(2)
	if err != nil {
		t.Fatalf("bias at two: %v", err)
	}
	if b1 <= b0 || b2 <= b1 {
		t.Fatalf("bias must grow with distance, got %v %v %v", b0, b1, b2)
	}
}

func TestSnapshotRestoreKeepsFieldAndLights(t *testing.T) {
	m := readyMarcher(t)
	if err := m.SetLight("fill", impl.LightSpec{Position: impl.Vec3{X: 4}, Intensity: 0.5}); err != nil {
		t.Fatalf("set fill light: %v", err)
	}
	if err := m.SetActiveLight("fill"); err != nil {
		t.Fatalf("switch light: %v", err)
	}
	if err := m.SetStepBudget(16); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	first, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10)
	if err != nil {
		t.Fatalf("trace before snapshot: %v", err)
	}
	data, err := m.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewSdfShadowRaymarcher(marcherLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if fields := restored.Fields(); len(fields) != 1 || fields[0] != "room" {
		t.Fatalf("restored fields = %v, want [room]", fields)
	}
	if lights := restored.Lights(); len(lights) != 2 {
		t.Fatalf("restored lights = %v, want two", lights)
	}
	field, light := restored.Active()
	if field != "room" || light != "fill" {
		t.Fatalf("restored selection = (%q,%q), want (room,fill)", field, light)
	}
	if steps := restored.StepBudget(); steps != 16 {
		t.Fatalf("restored step budget = %d, want 16", steps)
	}
	if stats := restored.Stats(); stats.Traces != 1 || stats.Voxels != 1 {
		t.Fatalf("restored stats = %+v, want one trace over one voxel", stats)
	}
	again, err := restored.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10)
	if err != nil {
		t.Fatalf("trace after restore: %v", err)
	}
	if again.Occluded != first.Occluded || again.Depth != first.Depth {
		t.Fatalf("restored trace = %+v, want the same hit as %+v", again, first)
	}
	if err := restored.Restore([]byte("scrambled")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if fields := restored.Fields(); len(fields) != 1 {
		t.Fatalf("rejected restore must not touch the field table, got %v", fields)
	}
}

func TestConcurrentTraceIsStable(t *testing.T) {
	m := readyMarcher(t)
	want, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10)
	if err != nil {
		t.Fatalf("reference trace: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := m.Trace(impl.Vec3{Z: -5}, impl.Vec3{Z: 1}, 10)
			if err != nil {
				t.Errorf("concurrent trace: %v", err)
				return
			}
			if got.Occluded != want.Occluded || got.Depth != want.Depth || got.Certain != want.Certain {
				t.Errorf("concurrent trace = %+v, want %+v", got, want)
			}
		}()
	}
	wg.Wait()
	if stats := m.Stats(); stats.Traces != 17 {
		t.Fatalf("trace counter = %d, want 17", stats.Traces)
	}
}
