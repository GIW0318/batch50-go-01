package task

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-04"
)

func depthCfg() impl.DepthConfig {
	return impl.DepthConfig{MinNear: 0.5, MaxFar: 500, PrecisionBudget: 0.5}
}

func addDefaultPasses(t *testing.T, p *impl.DepthPrecisionPlanner) {
	t.Helper()
	if err := p.AddPass("shadow", impl.DepthRange{Near: 0, Far: 0}); err != nil {
		t.Fatalf("add shadow pass: %v", err)
	}
	if err := p.AddPass("main", impl.DepthRange{Near: 0, Far: 0}); err != nil {
		t.Fatalf("add main pass: %v", err)
	}
}

func passByID(steps []impl.PassDepth, id string) (impl.PassDepth, bool) {
	for _, s := range steps {
		if s.PassID == id {
			return s, true
		}
	}
	return impl.PassDepth{}, false
}

func TestAddPassStoresAndSorts(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	if err := p.AddPass("shadow", impl.DepthRange{Near: 1, Far: 50}); err != nil {
		t.Fatalf("add shadow: %v", err)
	}
	if err := p.AddPass("main", impl.DepthRange{Near: 0.5, Far: 200}); err != nil {
		t.Fatalf("add main: %v", err)
	}
	want := []string{"main", "shadow"}
	if got := p.Passes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("passes = %v, want the ascending list %v", got, want)
	}
	if st := p.Stats(); st.Passes != 2 || st.Adds != 2 {
		t.Fatalf("stats passes=%d adds=%d, want 2 and 2", st.Passes, st.Adds)
	}
}

func TestAddPassRejectsBadInput(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	if err := p.AddPass("  ", impl.DepthRange{Near: 1, Far: 2}); !errors.Is(err, impl.ErrBadPass) {
		t.Fatalf("blank pass id must fail with ErrBadPass, got %v", err)
	}
	if err := p.AddPass("main", impl.DepthRange{Near: 5, Far: 5}); !errors.Is(err, impl.ErrBadPass) {
		t.Fatalf("an empty range must fail with ErrBadPass, got %v", err)
	}
	if err := p.AddPass("main", impl.DepthRange{Near: -1, Far: 5}); !errors.Is(err, impl.ErrBadPass) {
		t.Fatalf("a negative near plane must fail with ErrBadPass, got %v", err)
	}
	if err := p.AddPass("main", impl.DepthRange{Near: 1, Far: 50}); err != nil {
		t.Fatalf("add main: %v", err)
	}
	if err := p.AddPass("main", impl.DepthRange{Near: 1, Far: 60}); !errors.Is(err, impl.ErrDuplicatePass) {
		t.Fatalf("duplicate pass id must fail with ErrDuplicatePass, got %v", err)
	}
	if got := len(p.Passes()); got != 1 {
		t.Fatalf("pass count = %d, want 1", got)
	}
}

func TestSetBoundsRejectsBadBounds(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	if err := p.SetBounds("", impl.Bounds{Min: 1, Max: 5}); !errors.Is(err, impl.ErrBadBounds) {
		t.Fatalf("blank object id must fail with ErrBadBounds, got %v", err)
	}
	if err := p.SetBounds("o1", impl.Bounds{Min: 5, Max: 5}); !errors.Is(err, impl.ErrBadBounds) {
		t.Fatalf("degenerate bounds must fail with ErrBadBounds, got %v", err)
	}
	if err := p.SetBounds("o1", impl.Bounds{Min: 9, Max: 2}); !errors.Is(err, impl.ErrBadBounds) {
		t.Fatalf("inverted bounds must fail with ErrBadBounds, got %v", err)
	}
	if err := p.SetBounds("o1", impl.Bounds{Min: math.NaN(), Max: 5}); !errors.Is(err, impl.ErrBadBounds) {
		t.Fatalf("nan bounds must fail with ErrBadBounds, got %v", err)
	}
	if got := len(p.Objects()); got != 0 {
		t.Fatalf("object count = %d, want 0 after the rejects", got)
	}
}

func TestSetBoundsUpdatesExisting(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	if err := p.SetBounds("o2", impl.Bounds{Min: 2, Max: 8}); err != nil {
		t.Fatalf("set o2: %v", err)
	}
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set o1: %v", err)
	}
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 90}); err != nil {
		t.Fatalf("replace o1: %v", err)
	}
	want := []string{"o1", "o2"}
	if got := p.Objects(); !reflect.DeepEqual(got, want) {
		t.Fatalf("objects = %v, want %v", got, want)
	}
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, ok := passByID(steps, "main")
	if !ok {
		t.Fatalf("plan lost the main pass")
	}
	if main.Far != 90 {
		t.Fatalf("far plane = %v, want the replaced bound 90", main.Far)
	}
	if st := p.Stats(); st.Objects != 2 {
		t.Fatalf("object counter = %d, want 2", st.Objects)
	}
}

func TestPlanWithoutObjectsUsesFullRange(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	steps, err := p.Plan(impl.CameraParams{Position: 7, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.Near != 0.5 || main.Far != 500 {
		t.Fatalf("range with no object = %v..%v, want the configured 0.5..500", main.Near, main.Far)
	}
	if st := p.Stats(); st.Plans != 1 {
		t.Fatalf("plan counter = %d, want 1", st.Plans)
	}
}

func TestPlanTightensToObjects(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	if err := p.SetBounds("near", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set near object: %v", err)
	}
	if err := p.SetBounds("far", impl.Bounds{Min: 10, Max: 50}); err != nil {
		t.Fatalf("set far object: %v", err)
	}
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.Near != 1 {
		t.Fatalf("near plane = %v, want the closest surface 1", main.Near)
	}
	if main.Far != 50 {
		t.Fatalf("far plane = %v, want the farthest surface 50", main.Far)
	}
}

func TestPlanClampsToMaxFar(t *testing.T) {
	cfg := depthCfg()
	cfg.MaxFar = 40
	p := impl.NewDepthPrecisionPlanner(cfg)
	addDefaultPasses(t, p)
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set o1: %v", err)
	}
	if err := p.SetBounds("o2", impl.Bounds{Min: 10, Max: 300}); err != nil {
		t.Fatalf("set o2: %v", err)
	}
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.Far != 40 {
		t.Fatalf("far plane = %v, want the configured cap 40", main.Far)
	}
}

func TestPlanIntersectsPassHints(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	if err := p.AddPass("main", impl.DepthRange{Near: 0, Far: 0}); err != nil {
		t.Fatalf("add main: %v", err)
	}
	if err := p.AddPass("shadow", impl.DepthRange{Near: 2, Far: 20}); err != nil {
		t.Fatalf("add shadow: %v", err)
	}
	if err := p.AddPass("wide", impl.DepthRange{Near: 5, Far: 900}); err != nil {
		t.Fatalf("add wide: %v", err)
	}
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set o1: %v", err)
	}
	if err := p.SetBounds("o2", impl.Bounds{Min: 10, Max: 40}); err != nil {
		t.Fatalf("set o2: %v", err)
	}
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.Near != 1 || main.Far != 40 {
		t.Fatalf("main range = %v..%v, want the camera range 1..40", main.Near, main.Far)
	}
	shadow, _ := passByID(steps, "shadow")
	if shadow.Near != 2 || shadow.Far != 20 {
		t.Fatalf("shadow range = %v..%v, want its own hint 2..20", shadow.Near, shadow.Far)
	}
	wide, _ := passByID(steps, "wide")
	if wide.Near != 5 || wide.Far != 40 {
		t.Fatalf("wide range = %v..%v, want the hint clipped to 5..40", wide.Near, wide.Far)
	}
}

func TestPlanRejectsCameraBehindObjects(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set o1: %v", err)
	}
	if _, err := p.Plan(impl.CameraParams{Position: 100, Bits: 24}); !errors.Is(err, impl.ErrBadCamera) {
		t.Fatalf("a camera past every object must fail with ErrBadCamera, got %v", err)
	}
	if _, err := p.Plan(impl.CameraParams{Position: 0, Bits: 0}); !errors.Is(err, impl.ErrBadCamera) {
		t.Fatalf("a camera with no depth bits must fail with ErrBadCamera, got %v", err)
	}
	if _, ok := passByID(mustSteps(t, p), "main"); !ok {
		t.Fatalf("a valid camera must still plan after the rejects")
	}
}

func mustSteps(t *testing.T, p *impl.DepthPrecisionPlanner) []impl.PassDepth {
	t.Helper()
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan after the rejects: %v", err)
	}
	return steps
}

func TestSetReversedZFlipsCompare(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.CompareFunc != impl.CompareLess || main.Reversed {
		t.Fatalf("default compare = %q reversed=%v, want less and false", main.CompareFunc, main.Reversed)
	}
	if main.ClearValue != 1 {
		t.Fatalf("standard clear value = %v, want 1", main.ClearValue)
	}
	if err := p.SetReversedZ(true); err != nil {
		t.Fatalf("enable reversed z: %v", err)
	}
	if !p.ReversedZ() {
		t.Fatalf("reversed z must report true after the switch")
	}
	steps, err = p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan in reversed z: %v", err)
	}
	main, _ = passByID(steps, "main")
	if main.CompareFunc != impl.CompareGreater || !main.Reversed {
		t.Fatalf("reversed compare = %q reversed=%v, want greater and true", main.CompareFunc, main.Reversed)
	}
	if err := p.SetReversedZ(false); err != nil {
		t.Fatalf("disable reversed z: %v", err)
	}
	steps, err = p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan after switching back: %v", err)
	}
	main, _ = passByID(steps, "main")
	if main.CompareFunc != impl.CompareLess || main.Reversed {
		t.Fatalf("restored compare = %q reversed=%v, want less and false", main.CompareFunc, main.Reversed)
	}
}

func TestPrecisionReportsSurfaceResolution(t *testing.T) {
	cfg := impl.DepthConfig{MinNear: 1, MaxFar: 100, PrecisionBudget: 1}
	p := impl.NewDepthPrecisionPlanner(cfg)
	if _, err := p.Precision(1); !errors.Is(err, impl.ErrNoPlan) {
		t.Fatalf("precision before a plan must fail with ErrNoPlan, got %v", err)
	}
	addDefaultPasses(t, p)
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 100}); err != nil {
		t.Fatalf("set bounds: %v", err)
	}
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.Near != 1 || main.Far != 100 {
		t.Fatalf("range = %v..%v, want 1..100", main.Near, main.Far)
	}
	got, err := p.Precision(1)
	if err != nil {
		t.Fatalf("precision at the near plane: %v", err)
	}
	want := 1.0 * 99.0 / (100.0 * 1.0 * 16777216.0)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("precision at depth 1 = %v, want %v", got, want)
	}
	if _, err := p.Precision(0); !errors.Is(err, impl.ErrBadDepth) {
		t.Fatalf("depth 0 must fail with ErrBadDepth, got %v", err)
	}
	if _, err := p.Precision(120); !errors.Is(err, impl.ErrBadDepth) {
		t.Fatalf("depth past the far plane must fail with ErrBadDepth, got %v", err)
	}
}

func TestValidateReportsPrecisionIssue(t *testing.T) {
	low := impl.NewDepthPrecisionPlanner(impl.DepthConfig{MinNear: 0.5, MaxFar: 500, PrecisionBudget: 1})
	if len(low.Validate()) != 0 {
		t.Fatalf("validate before a plan must be empty")
	}
	addDefaultPasses(t, low)
	if err := low.SetBounds("o1", impl.Bounds{Min: 0.5, Max: 500}); err != nil {
		t.Fatalf("set bounds: %v", err)
	}
	if _, err := low.Plan(impl.CameraParams{Position: 0, Bits: 8}); err != nil {
		t.Fatalf("plan with 8 bits: %v", err)
	}
	issues := low.Validate()
	if len(issues) == 0 {
		t.Fatalf("a 0.5..500 range at 8 bits must report a precision issue")
	}
	if issues[0].Code != impl.IssuePrecision {
		t.Fatalf("issue code = %q, want %q", issues[0].Code, impl.IssuePrecision)
	}
	wide := impl.NewDepthPrecisionPlanner(impl.DepthConfig{MinNear: 2, MaxFar: 40, PrecisionBudget: 1})
	addDefaultPasses(t, wide)
	if err := wide.SetBounds("o1", impl.Bounds{Min: 2, Max: 40}); err != nil {
		t.Fatalf("set tight bounds: %v", err)
	}
	if _, err := wide.Plan(impl.CameraParams{Position: 0, Bits: 24}); err != nil {
		t.Fatalf("plan with 24 bits: %v", err)
	}
	if got := wide.Validate(); len(got) != 0 {
		t.Fatalf("a 2..40 range at 24 bits must validate clean, got %+v", got)
	}
}

func TestRemovePassAndObject(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set bounds: %v", err)
	}
	if err := p.RemovePass("nope"); !errors.Is(err, impl.ErrUnknownPass) {
		t.Fatalf("removing an unknown pass must fail with ErrUnknownPass, got %v", err)
	}
	if err := p.RemoveObject("nope"); !errors.Is(err, impl.ErrNotFound) {
		t.Fatalf("removing an unknown object must fail with ErrNotFound, got %v", err)
	}
	if err := p.RemovePass("shadow"); err != nil {
		t.Fatalf("remove shadow: %v", err)
	}
	if err := p.RemoveObject("o1"); err != nil {
		t.Fatalf("remove o1: %v", err)
	}
	if got := p.Passes(); !reflect.DeepEqual(got, []string{"main"}) {
		t.Fatalf("passes = %v, want only main", got)
	}
	if got := len(p.Objects()); got != 0 {
		t.Fatalf("object count = %d, want 0", got)
	}
	steps, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan after the removals: %v", err)
	}
	if len(steps) != 1 || steps[0].PassID != "main" {
		t.Fatalf("planned steps = %+v, want only main", steps)
	}
	if st := p.Stats(); st.Removes != 2 {
		t.Fatalf("remove counter = %d, want 2", st.Removes)
	}
}

func TestSnapshotRestoreKeepsDepthState(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set o1: %v", err)
	}
	if err := p.SetBounds("o2", impl.Bounds{Min: 10, Max: 60}); err != nil {
		t.Fatalf("set o2: %v", err)
	}
	if err := p.SetReversedZ(true); err != nil {
		t.Fatalf("enable reversed z: %v", err)
	}
	if _, err := p.Plan(impl.CameraParams{Position: 0, Bits: 24}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewDepthPrecisionPlanner(depthCfg())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !restored.ReversedZ() {
		t.Fatalf("restored planner lost the reversed z flag")
	}
	if got := restored.Objects(); !reflect.DeepEqual(got, []string{"o1", "o2"}) {
		t.Fatalf("restored objects = %v, want o1 and o2", got)
	}
	steps, err := restored.Plan(impl.CameraParams{Position: 0, Bits: 24})
	if err != nil {
		t.Fatalf("plan after restore: %v", err)
	}
	main, _ := passByID(steps, "main")
	if main.Far != 60 || main.CompareFunc != impl.CompareGreater {
		t.Fatalf("restored plan = %+v, want far 60 and the greater compare", main)
	}
	if err := restored.Restore([]byte("}{")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, err := restored.Plan(impl.CameraParams{Position: 0, Bits: 24}); err != nil {
		t.Fatalf("a rejected restore must not break planning, got %v", err)
	}
}

func TestConcurrentPlanSerialized(t *testing.T) {
	p := impl.NewDepthPrecisionPlanner(depthCfg())
	addDefaultPasses(t, p)
	if err := p.SetBounds("o1", impl.Bounds{Min: 1, Max: 5}); err != nil {
		t.Fatalf("set o1: %v", err)
	}
	if err := p.SetBounds("o2", impl.Bounds{Min: 10, Max: 80}); err != nil {
		t.Fatalf("set o2: %v", err)
	}
	var wg sync.WaitGroup
	results := make([][]impl.PassDepth, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.Plan(impl.CameraParams{Position: 0, Bits: 24})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent plan %d: %v", i, err)
		}
	}
	for i, steps := range results {
		main, ok := passByID(steps, "main")
		if !ok || main.Far != 80 {
			t.Fatalf("plan %d main pass = %+v ok=%v, want far 80", i, main, ok)
		}
	}
	if st := p.Stats(); st.Plans != 8 {
		t.Fatalf("plan counter = %d, want 8", st.Plans)
	}
}
