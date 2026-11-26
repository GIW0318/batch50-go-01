package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-13"
)

const deintWidth = 4

func deintFrame(index int64, rows []uint8) impl.Frame {
	y := make([]uint8, deintWidth*len(rows))
	for r, v := range rows {
		for x := 0; x < deintWidth; x++ {
			y[r*deintWidth+x] = v
		}
	}
	return impl.Frame{Index: index, Width: deintWidth, Height: len(rows), Y: y}
}

func deintFlat(index int64, height int, v uint8) impl.Frame {
	rows := make([]uint8, height)
	for i := range rows {
		rows[i] = v
	}
	return deintFrame(index, rows)
}

func deintAlternating(index int64) impl.Frame {
	return deintFrame(index, []uint8{0, 255, 0, 255, 0, 255, 0, 255})
}

func deintTopFieldMoved(index int64) impl.Frame {
	return deintFrame(index, []uint8{0, 100, 0, 100, 0, 100, 0, 100})
}

func deintBottomFieldMoved(index int64) impl.Frame {
	return deintFrame(index, []uint8{100, 0, 100, 0, 100, 0, 100, 0})
}

func deintPlanner(cadence impl.Cadence) *impl.DeinterlacePlanner {
	return impl.NewDeinterlacePlanner(impl.Config{MotionThreshold: 0.4, Cadence: cadence, ConfirmFrames: 2})
}

func deintProbed(t *testing.T, cadence impl.Cadence) *impl.DeinterlacePlanner {
	t.Helper()
	p := deintPlanner(cadence)
	order, err := p.Probe([]impl.Frame{deintFlat(1, 8, 0), deintTopFieldMoved(2)})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if order != impl.FieldOrderTopFirst {
		t.Fatalf("probe order = %q, want %q", order, impl.FieldOrderTopFirst)
	}
	return p
}

func TestProbeDetectsTopFieldFirst(t *testing.T) {
	p := deintPlanner(impl.CadenceNone)
	order, err := p.Probe([]impl.Frame{deintFlat(1, 8, 0), deintTopFieldMoved(2)})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if order != impl.FieldOrderTopFirst {
		t.Fatalf("order = %q, want top field first", order)
	}
	if got := p.Order(); got != impl.FieldOrderTopFirst {
		t.Fatalf("order in force = %q, want top field first", got)
	}
}

func TestProbeDetectsBottomFieldFirst(t *testing.T) {
	p := deintPlanner(impl.CadenceNone)
	order, err := p.Probe([]impl.Frame{deintFlat(1, 8, 0), deintBottomFieldMoved(2)})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if order != impl.FieldOrderBottomFirst {
		t.Fatalf("order = %q, want bottom field first", order)
	}
	if got := p.Order(); got != impl.FieldOrderBottomFirst {
		t.Fatalf("order in force = %q, want bottom field first", got)
	}
}

func TestProbeReportsUnknownWhenBothFieldsMoveAlike(t *testing.T) {
	p := deintPlanner(impl.CadenceNone)
	order, err := p.Probe([]impl.Frame{deintFlat(1, 8, 0), deintFlat(2, 8, 50)})
	if err != nil {
		t.Fatalf("an unclear pair must not fail the probe, got %v", err)
	}
	if order != impl.FieldOrderUnknown {
		t.Fatalf("order = %q, want unknown when both fields moved alike", order)
	}
	if got := p.Order(); got != impl.FieldOrderUnknown {
		t.Fatalf("order in force = %q, want unknown", got)
	}
}

func TestPlanWeavesStaticFrames(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	static := deintFlat(3, 8, 120)
	for i := 0; i < 2; i++ {
		method, err := p.Plan(static)
		if err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
		if method != impl.MethodWeave {
			t.Fatalf("plan %d method = %q, want weave for a frame without combing", i, method)
		}
	}
}

func TestPlanInterpolatesMovingFrames(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	for i := 0; i < 2; i++ {
		method, err := p.Plan(deintAlternating(int64(3 + i)))
		if err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
		if i == 1 && method != impl.MethodInterpolate {
			t.Fatalf("second moving frame method = %q, want interpolate", method)
		}
	}
	method, err := p.Plan(deintAlternating(9))
	if err != nil {
		t.Fatalf("third plan: %v", err)
	}
	if method != impl.MethodInterpolate {
		t.Fatalf("third moving frame method = %q, want interpolate", method)
	}
}

func TestPlanFollowsThreeTwoCadence(t *testing.T) {
	p := deintProbed(t, impl.Cadence32)
	static := deintFlat(3, 8, 120)
	want := []impl.Method{
		impl.MethodWeave,
		impl.MethodRepeat,
		impl.MethodWeave,
		impl.MethodRepeat,
		impl.MethodWeave,
	}
	for i, expected := range want {
		method, err := p.Plan(static)
		if err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
		if method != expected {
			t.Fatalf("plan %d method = %q, want %q in the 3:2 field repeat pattern", i, method, expected)
		}
	}
}

func TestPlanBlendsWhileFieldOrderIsUnknown(t *testing.T) {
	p := deintPlanner(impl.CadenceNone)
	method, err := p.Plan(deintFlat(3, 8, 120))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if method != impl.MethodFieldBlend {
		t.Fatalf("method before any probe = %q, want the conservative field blend", method)
	}
}

func TestApplyInterpolateFillsOffPhaseRows(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	frame := deintFrame(4, []uint8{10, 90, 30, 70})
	out, err := p.Apply(impl.Plan{Index: 4, Method: impl.MethodInterpolate, Order: impl.FieldOrderTopFirst}, frame)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Width != deintWidth || out.Height != 4 {
		t.Fatalf("output geometry = %dx%d, want %dx4", out.Width, out.Height, deintWidth)
	}
	want := []uint8{10, 20, 30, 30}
	for r, expected := range want {
		if got := out.Y[r*deintWidth]; got != expected {
			t.Fatalf("output row %d = %d, want %d", r, got, expected)
		}
	}
}

func TestApplyRejectsUnknownMethod(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	frame := deintFlat(5, 4, 80)
	if _, err := p.Apply(impl.Plan{Index: 5, Method: impl.Method("bob-two")}, frame); !errors.Is(err, impl.ErrBadPlan) {
		t.Fatalf("unknown method must report ErrBadPlan, got %v", err)
	}
}

func TestSetMotionThresholdRejectsOutOfRange(t *testing.T) {
	p := deintPlanner(impl.CadenceNone)
	for _, v := range []float64{0, -0.2, 1.5} {
		if err := p.SetMotionThreshold(v); !errors.Is(err, impl.ErrBadThreshold) {
			t.Fatalf("threshold %v must report ErrBadThreshold, got %v", v, err)
		}
	}
	if err := p.SetMotionThreshold(0.35); err != nil {
		t.Fatalf("threshold 0.35 must be accepted, got %v", err)
	}
}

func TestSetCadenceRejectsUnknownPattern(t *testing.T) {
	p := deintPlanner(impl.CadenceNone)
	if err := p.SetCadence(impl.Cadence{Pattern: "4:1"}); !errors.Is(err, impl.ErrBadCadence) {
		t.Fatalf("unsupported cadence must report ErrBadCadence, got %v", err)
	}
	if err := p.SetCadence(impl.Cadence32); err != nil {
		t.Fatalf("3:2 cadence must be accepted, got %v", err)
	}
	if got := p.Cadence().Pattern; got != "3:2" {
		t.Fatalf("cadence in force = %q, want 3:2", got)
	}
}

func TestStatsCountsDecisions(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	for i := 0; i < 2; i++ {
		if _, err := p.Plan(deintFlat(int64(3+i), 8, 120)); err != nil {
			t.Fatalf("static plan %d: %v", i, err)
		}
	}
	if _, err := p.Plan(deintAlternating(9)); err != nil {
		t.Fatalf("moving plan: %v", err)
	}
	stats := p.Stats()
	if stats.Frames != 3 {
		t.Fatalf("planned frames = %d, want 3", stats.Frames)
	}
	if stats.Weave != 2 {
		t.Fatalf("weave count = %d, want 2", stats.Weave)
	}
	if stats.Interpolate != 1 {
		t.Fatalf("interpolate count = %d, want 1", stats.Interpolate)
	}
	if stats.Order != impl.FieldOrderTopFirst {
		t.Fatalf("stats order = %q, want top field first", stats.Order)
	}
}

func TestSnapshotRestoreKeepsCadencePosition(t *testing.T) {
	p := deintProbed(t, impl.Cadence32)
	static := deintFlat(3, 8, 120)
	for i := 0; i < 5; i++ {
		if _, err := p.Plan(static); err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
	}
	if got := p.Stats().Repeat; got != 2 {
		t.Fatalf("repeat count after one pattern = %d, want 2", got)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := deintPlanner(impl.Cadence32)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Stats().Repeat; got != 2 {
		t.Fatalf("restored repeat count = %d, want 2", got)
	}
	if got := restored.Stats().Frames; got != 5 {
		t.Fatalf("restored frame count = %d, want 5", got)
	}
	method, err := restored.Plan(static)
	if err != nil {
		t.Fatalf("plan after restore: %v", err)
	}
	if method != impl.MethodWeave {
		t.Fatalf("first frame of the next pattern = %q, want weave", method)
	}
	if got := restored.Stats().Frames; got != 6 {
		t.Fatalf("frame count after restore and one more plan = %d, want 6", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	if _, err := p.Plan(deintFlat(3, 8, 120)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := p.Restore([]byte("{\"frames\":\"NaN\"}")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must report ErrBadSnapshot, got %v", err)
	}
	if got := p.Stats().Frames; got != 1 {
		t.Fatalf("frames after a rejected restore = %d, want 1", got)
	}
	if got := p.Order(); got != impl.FieldOrderTopFirst {
		t.Fatalf("order after a rejected restore = %q, want top field first", got)
	}
}

func TestConcurrentPlanCountsEveryFrame(t *testing.T) {
	p := deintProbed(t, impl.CadenceNone)
	static := deintFlat(3, 8, 120)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Plan(static); err != nil {
				t.Errorf("concurrent plan: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := p.Stats().Frames; got != 40 {
		t.Fatalf("planned frames after 40 concurrent calls = %d, want 40", got)
	}
}
