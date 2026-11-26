package task

import (
	"errors"
	"math"
	"testing"
	"time"

	impl "gsb/batch50-go-game-b-16"
)

func newAnimator() *impl.MaterialAnimator {
	return impl.NewMaterialAnimator(8, 16, 8)
}

func sharedBase() map[string]float64 {
	return map[string]float64{
		impl.PropEmissive:  0.25,
		impl.PropOpacity:   1,
		impl.PropUVOffsetU: 0,
		impl.PropUVOffsetV: 0,
	}
}

func rampCurve(t *testing.T, a *impl.MaterialAnimator, id impl.CurveID) {
	t.Helper()
	keys := []impl.Key{
		{At: 0, Value: 0},
		{At: 100 * time.Millisecond, Value: 1},
	}
	if err := a.SetCurve(id, keys); err != nil {
		t.Fatalf("set curve %q: %v", id, err)
	}
}

func near(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func TestAddMaterialExposesBaseValues(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	if got := a.Stats().Materials; got != 1 {
		t.Fatalf("material count = %d, want 1", got)
	}
	v, err := a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("value of unbound property: %v", err)
	}
	if !near(v, 0.25) {
		t.Fatalf("base emissive = %v, want 0.25", v)
	}
	v, err = a.Value("mat-1", impl.PropOpacity)
	if err != nil {
		t.Fatalf("value of unbound opacity: %v", err)
	}
	if !near(v, 1) {
		t.Fatalf("base opacity = %v, want 1", v)
	}
}

func TestAddMaterialRejectsDuplicateAndBlank(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	if err := a.AddMaterial("mat-1", sharedBase()); !errors.Is(err, impl.ErrDuplicateMaterial) {
		t.Fatalf("duplicate material error = %v, want ErrDuplicateMaterial", err)
	}
	if err := a.AddMaterial("   ", sharedBase()); !errors.Is(err, impl.ErrBadMaterial) {
		t.Fatalf("blank material error = %v, want ErrBadMaterial", err)
	}
	if got := a.Stats().Materials; got != 1 {
		t.Fatalf("material count after rejects = %d, want 1", got)
	}
}

func TestBindRequiresRegisteredMaterial(t *testing.T) {
	a := newAnimator()
	rampCurve(t, a, "curve-a")
	if _, err := a.Bind("ghost", impl.PropEmissive, "curve-a"); !errors.Is(err, impl.ErrUnknownMaterial) {
		t.Fatalf("bind on unknown material = %v, want ErrUnknownMaterial", err)
	}
	if got := len(a.Bindings("ghost")); got != 0 {
		t.Fatalf("unknown material got %d bindings, want 0", got)
	}
}

func TestBindRejectsUnknownProperty(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	rampCurve(t, a, "curve-a")
	if _, err := a.Bind("mat-1", "specular", "curve-a"); !errors.Is(err, impl.ErrUnknownProperty) {
		t.Fatalf("bind unknown property = %v, want ErrUnknownProperty", err)
	}
	if got := a.Stats().Bindings; got != 0 {
		t.Fatalf("binding count after reject = %d, want 0", got)
	}
}

func TestBindRequiresExistingCurve(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	if _, err := a.Bind("mat-1", impl.PropEmissive, "missing-curve"); !errors.Is(err, impl.ErrUnknownCurve) {
		t.Fatalf("bind with missing curve = %v, want ErrUnknownCurve", err)
	}
	if got := a.Stats().Bindings; got != 0 {
		t.Fatalf("binding count after reject = %d, want 0", got)
	}
}

func TestBindIsIdempotentPerProperty(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	rampCurve(t, a, "curve-a")
	first, err := a.Bind("mat-1", impl.PropEmissive, "curve-a")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if first == "" {
		t.Fatalf("binding id must not be empty")
	}
	second, err := a.Bind("mat-1", impl.PropEmissive, "curve-a")
	if err != nil {
		t.Fatalf("second bind: %v", err)
	}
	if second != first {
		t.Fatalf("rebinding one property returned %q then %q, want a stable id", first, second)
	}
	if got := len(a.Bindings("mat-1")); got != 1 {
		t.Fatalf("binding list length = %d, want 1", got)
	}
	if got := a.Stats().Bindings; got != 1 {
		t.Fatalf("binding count = %d, want 1", got)
	}
}

func TestSetCurveRejectsUnorderedKeys(t *testing.T) {
	a := newAnimator()
	keys := []impl.Key{
		{At: 100 * time.Millisecond, Value: 0},
		{At: 50 * time.Millisecond, Value: 1},
	}
	if err := a.SetCurve("curve-bad", keys); !errors.Is(err, impl.ErrBadKeys) {
		t.Fatalf("unordered keys error = %v, want ErrBadKeys", err)
	}
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	if _, err := a.Bind("mat-1", impl.PropEmissive, "curve-bad"); !errors.Is(err, impl.ErrUnknownCurve) {
		t.Fatalf("rejected curve must not be stored, bind error = %v", err)
	}
	if got := a.Stats().Curves; got != 0 {
		t.Fatalf("curve count after reject = %d, want 0", got)
	}
}

func TestSetCurveEnforcesKeyLimit(t *testing.T) {
	a := impl.NewMaterialAnimator(4, 2, 4)
	keys := []impl.Key{
		{At: 0, Value: 0},
		{At: 50 * time.Millisecond, Value: 0.5},
		{At: 100 * time.Millisecond, Value: 1},
	}
	if err := a.SetCurve("curve-too-long", keys); !errors.Is(err, impl.ErrKeyLimit) {
		t.Fatalf("three keys over a cap of two = %v, want ErrKeyLimit", err)
	}
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	if _, err := a.Bind("mat-1", impl.PropEmissive, "curve-too-long"); !errors.Is(err, impl.ErrUnknownCurve) {
		t.Fatalf("over long curve must not be stored, bind error = %v", err)
	}
}

func TestValueRejectsUnregisteredProperty(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", map[string]float64{impl.PropEmissive: 0.4}); err != nil {
		t.Fatalf("add material: %v", err)
	}
	if _, err := a.Value("mat-1", "specular"); !errors.Is(err, impl.ErrUnknownProperty) {
		t.Fatalf("value of unknown property = %v, want ErrUnknownProperty", err)
	}
	if _, err := a.Value("ghost", impl.PropEmissive); !errors.Is(err, impl.ErrUnknownMaterial) {
		t.Fatalf("value of unknown material = %v, want ErrUnknownMaterial", err)
	}
}

func TestBindSamplesCurveLinearly(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	rampCurve(t, a, "curve-a")
	if _, err := a.Bind("mat-1", impl.PropEmissive, "curve-a"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	v, err := a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample at phase zero: %v", err)
	}
	if !near(v, 0) {
		t.Fatalf("sample at phase zero = %v, want 0", v)
	}
	if err := a.Advance(50 * time.Millisecond); err != nil {
		t.Fatalf("advance: %v", err)
	}
	v, err = a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample at mid phase: %v", err)
	}
	if !near(v, 0.5) {
		t.Fatalf("sample at 50ms = %v, want 0.5", v)
	}
	if got := a.Stats().Frames; got != 1 {
		t.Fatalf("frame counter = %d, want 1 after one advance", got)
	}
}

func TestUnbindRestoresBaseValue(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	rampCurve(t, a, "curve-a")
	id, err := a.Bind("mat-1", impl.PropOpacity, "curve-a")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := a.Advance(50 * time.Millisecond); err != nil {
		t.Fatalf("advance: %v", err)
	}
	v, err := a.Value("mat-1", impl.PropOpacity)
	if err != nil {
		t.Fatalf("sample before unbind: %v", err)
	}
	if !near(v, 0.5) {
		t.Fatalf("animated opacity = %v, want 0.5", v)
	}
	if err := a.Unbind(id); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	v, err = a.Value("mat-1", impl.PropOpacity)
	if err != nil {
		t.Fatalf("sample after unbind: %v", err)
	}
	if !near(v, 1) {
		t.Fatalf("opacity after unbind = %v, want the shared base 1", v)
	}
	if got := len(a.Bindings("mat-1")); got != 0 {
		t.Fatalf("binding list length after unbind = %d, want 0", got)
	}
	if err := a.Unbind(id); !errors.Is(err, impl.ErrUnknownBinding) {
		t.Fatalf("second unbind = %v, want ErrUnknownBinding", err)
	}
}

func TestAdvanceRejectsNegativeDelta(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	rampCurve(t, a, "curve-a")
	if _, err := a.Bind("mat-1", impl.PropEmissive, "curve-a"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := a.Advance(50 * time.Millisecond); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := a.Advance(-time.Millisecond); !errors.Is(err, impl.ErrBadDelta) {
		t.Fatalf("negative delta = %v, want ErrBadDelta", err)
	}
	v, err := a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample after rejected delta: %v", err)
	}
	if !near(v, 0.5) {
		t.Fatalf("phase moved after a rejected delta, sample = %v, want 0.5", v)
	}
	if got := a.Stats().Frames; got != 1 {
		t.Fatalf("frame counter = %d, want 1: a rejected delta must not count a frame", got)
	}
}

func TestRepeatLoopWrapsPhase(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	mode, err := a.Loop("mat-1")
	if err != nil {
		t.Fatalf("read default loop mode: %v", err)
	}
	if mode != impl.LoopRepeat {
		t.Fatalf("default loop mode = %v, want LoopRepeat", mode)
	}
	rampCurve(t, a, "curve-a")
	if _, err := a.Bind("mat-1", impl.PropEmissive, "curve-a"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := a.Advance(150 * time.Millisecond); err != nil {
		t.Fatalf("advance: %v", err)
	}
	v, err := a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample after wrap: %v", err)
	}
	if !near(v, 0.5) {
		t.Fatalf("sample after 150ms on a 100ms loop = %v, want 0.5", v)
	}
}

func TestPauseFreezesMaterialPhase(t *testing.T) {
	a := newAnimator()
	for _, name := range []string{"mat-1", "mat-2"} {
		if err := a.AddMaterial(name, sharedBase()); err != nil {
			t.Fatalf("add material %s: %v", name, err)
		}
	}
	rampCurve(t, a, "curve-a")
	for _, name := range []string{"mat-1", "mat-2"} {
		if _, err := a.Bind(name, impl.PropEmissive, "curve-a"); err != nil {
			t.Fatalf("bind %s: %v", name, err)
		}
	}
	if err := a.SetPaused("mat-1", true); err != nil {
		t.Fatalf("pause mat-1: %v", err)
	}
	if err := a.Advance(50 * time.Millisecond); err != nil {
		t.Fatalf("advance: %v", err)
	}
	paused, err := a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample paused material: %v", err)
	}
	if !near(paused, 0) {
		t.Fatalf("paused material sample = %v, want 0", paused)
	}
	running, err := a.Value("mat-2", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample running material: %v", err)
	}
	if !near(running, 0.5) {
		t.Fatalf("unpaused material sample = %v, want 0.5", running)
	}
	if err := a.SetPaused("mat-1", false); err != nil {
		t.Fatalf("resume mat-1: %v", err)
	}
	if err := a.Advance(50 * time.Millisecond); err != nil {
		t.Fatalf("advance after resume: %v", err)
	}
	resumed, err := a.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample resumed material: %v", err)
	}
	if !near(resumed, 0.5) {
		t.Fatalf("resumed material sample = %v, want 0.5", resumed)
	}
	if err := a.SetPaused("ghost", true); !errors.Is(err, impl.ErrUnknownMaterial) {
		t.Fatalf("pause on unknown material = %v, want ErrUnknownMaterial", err)
	}
}

func TestSnapshotRestoreKeepsBindingsAndPhase(t *testing.T) {
	a := newAnimator()
	if err := a.AddMaterial("mat-1", sharedBase()); err != nil {
		t.Fatalf("add material: %v", err)
	}
	rampCurve(t, a, "curve-a")
	if _, err := a.Bind("mat-1", impl.PropEmissive, "curve-a"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := a.Advance(50 * time.Millisecond); err != nil {
		t.Fatalf("advance: %v", err)
	}
	payload, err := a.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := newAnimator()
	if err := restored.Restore(payload); err != nil {
		t.Fatalf("restore: %v", err)
	}
	v, err := restored.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample after restore: %v", err)
	}
	if !near(v, 0.5) {
		t.Fatalf("sample after restore = %v, want the stored phase 0.5", v)
	}
	if got := len(restored.Bindings("mat-1")); got != 1 {
		t.Fatalf("binding list after restore = %d, want 1", got)
	}
	if err := restored.Advance(25 * time.Millisecond); err != nil {
		t.Fatalf("advance after restore: %v", err)
	}
	v, err = restored.Value("mat-1", impl.PropEmissive)
	if err != nil {
		t.Fatalf("sample after advancing the restored phase: %v", err)
	}
	if !near(v, 0.75) {
		t.Fatalf("restored phase did not continue, sample = %v, want 0.75", v)
	}
}
