package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-03"
)

func bakerLimits() impl.Limits {
	return impl.Limits{MaxLUTSize: 256}
}

func defaultCurve() impl.CurveParams {
	return impl.CurveParams{Exposure: 0, Contrast: 1, Shoulder: 2, Toe: 0.5}
}

func TestSampleEndpointsAreExact(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	lo, err := b.Sample(0)
	if err != nil {
		t.Fatalf("sample 0: %v", err)
	}
	if lo != 0 {
		t.Fatalf("sample(0) = %v, want exactly 0", lo)
	}
	hi, err := b.Sample(1)
	if err != nil {
		t.Fatalf("sample 1: %v", err)
	}
	if hi != 1 {
		t.Fatalf("sample(1) = %v, want exactly 1", hi)
	}
	if st := b.Stats(); st.Samples != 2 {
		t.Fatalf("sample counter = %d, want 2", st.Samples)
	}
}

func TestSampleClampsFiniteInput(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	low, err := b.Sample(-0.5)
	if err != nil {
		t.Fatalf("sample below zero: %v", err)
	}
	if low != 0 {
		t.Fatalf("sample(-0.5) = %v, want the low end 0", low)
	}
	high, err := b.Sample(1.5)
	if err != nil {
		t.Fatalf("sample above one: %v", err)
	}
	if high != 1 {
		t.Fatalf("sample(1.5) = %v, want the high end 1", high)
	}
	top, err := b.Sample(4)
	if err != nil {
		t.Fatalf("sample 4: %v", err)
	}
	if top != 1 {
		t.Fatalf("sample(4) = %v, want the high end 1", top)
	}
}

func TestSampleRejectsNonFinite(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if _, err := b.Sample(math.NaN()); !errors.Is(err, impl.ErrBadInput) {
		t.Fatalf("nan input must fail with ErrBadInput, got %v", err)
	}
	if _, err := b.Sample(math.Inf(1)); !errors.Is(err, impl.ErrBadInput) {
		t.Fatalf("infinite input must fail with ErrBadInput, got %v", err)
	}
	if _, err := b.Sample(math.Inf(-1)); !errors.Is(err, impl.ErrBadInput) {
		t.Fatalf("negative infinite input must fail with ErrBadInput, got %v", err)
	}
}

func TestSampleIsNonDecreasing(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if err := b.SetCurve(impl.CurveParams{Exposure: 1, Contrast: 1.5, Shoulder: 2, Toe: 0.5}); err != nil {
		t.Fatalf("set curve: %v", err)
	}
	prev := -1.0
	for i := 0; i <= 100; i++ {
		x := float64(i) / 100
		v, err := b.Sample(x)
		if err != nil {
			t.Fatalf("sample %v: %v", x, err)
		}
		if v < prev {
			t.Fatalf("curve drops at x=%v: %v came after %v", x, v, prev)
		}
		if v < 0 || v > 1 {
			t.Fatalf("sample %v = %v, want it inside 0..1", x, v)
		}
		prev = v
	}
}

func TestSampleRisesWithExposure(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if err := b.SetCurve(impl.CurveParams{Exposure: 0, Contrast: 1, Shoulder: 2, Toe: 0}); err != nil {
		t.Fatalf("set base curve: %v", err)
	}
	base, err := b.Sample(0.25)
	if err != nil {
		t.Fatalf("sample at base exposure: %v", err)
	}
	if err := b.SetCurve(impl.CurveParams{Exposure: 2, Contrast: 1, Shoulder: 2, Toe: 0}); err != nil {
		t.Fatalf("set lifted curve: %v", err)
	}
	lifted, err := b.Sample(0.25)
	if err != nil {
		t.Fatalf("sample at raised exposure: %v", err)
	}
	if lifted <= base {
		t.Fatalf("sample(0.25) at +2 EV = %v, want it above the base %v", lifted, base)
	}
}

func TestSetCurveRejectsBadValues(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	bad := []impl.CurveParams{
		{Exposure: math.NaN(), Contrast: 1, Shoulder: 2, Toe: 0.5},
		{Exposure: 0, Contrast: math.NaN(), Shoulder: 2, Toe: 0.5},
		{Exposure: 0, Contrast: 1, Shoulder: 0, Toe: 0.5},
		{Exposure: 0, Contrast: 1, Shoulder: 2, Toe: -1},
		{Exposure: 9, Contrast: 1, Shoulder: 2, Toe: 0.5},
		{Exposure: 0, Contrast: -5, Shoulder: 2, Toe: 0.5},
	}
	for i, params := range bad {
		if err := b.SetCurve(params); !errors.Is(err, impl.ErrBadCurve) {
			t.Fatalf("bad curve %d must fail with ErrBadCurve, got %v", i, err)
		}
	}
	got, ok := b.Current()
	if !ok {
		t.Fatalf("a rejected curve must leave a usable curve behind")
	}
	if got != defaultCurve() {
		t.Fatalf("curve after the rejects = %+v, want the default %+v", got, defaultCurve())
	}
	if st := b.Stats(); st.Rejects != 6 || st.Sets != 0 {
		t.Fatalf("stats rejects=%d sets=%d, want 6 and 0", st.Rejects, st.Sets)
	}
}

func TestValidateReportsNegativeContrast(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if issues := b.Validate(); len(issues) != 0 {
		t.Fatalf("default curve must validate clean, got %+v", issues)
	}
	if err := b.SetCurve(impl.CurveParams{Exposure: 0, Contrast: -1, Shoulder: 2, Toe: 0.5}); err != nil {
		t.Fatalf("negative contrast is structurally valid and must be accepted, got %v", err)
	}
	issues := b.Validate()
	if len(issues) == 0 {
		t.Fatalf("an inverted curve must report at least one issue")
	}
	if issues[0].Code != impl.IssueNonMonotonic {
		t.Fatalf("issue code = %q, want %q", issues[0].Code, impl.IssueNonMonotonic)
	}
}

func TestBakeRejectsNonMonotonicCurve(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if err := b.SetCurve(impl.CurveParams{Exposure: 0, Contrast: -2, Shoulder: 2, Toe: 0.5}); err != nil {
		t.Fatalf("set inverted curve: %v", err)
	}
	if _, err := b.Bake(impl.LUTFormat1D); !errors.Is(err, impl.ErrNonMonotonic) {
		t.Fatalf("baking an inverted curve must fail with ErrNonMonotonic, got %v", err)
	}
	if st := b.Stats(); st.Bakes != 0 {
		t.Fatalf("bake counter = %d, want 0 after the rejection", st.Bakes)
	}
}

func TestBake1DProducesConfiguredSize(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	lut, err := b.Bake(impl.LUTFormat1D)
	if err != nil {
		t.Fatalf("bake 1d: %v", err)
	}
	if lut.Format != impl.LUTFormat1D {
		t.Fatalf("format = %q, want %q", lut.Format, impl.LUTFormat1D)
	}
	if lut.Size != impl.DefaultLUTSize || len(lut.Values) != impl.DefaultLUTSize {
		t.Fatalf("size=%d len=%d, want %d entries", lut.Size, len(lut.Values), impl.DefaultLUTSize)
	}
	stored, ok := b.LUT()
	if !ok || stored.Size != impl.DefaultLUTSize {
		t.Fatalf("the baked table must be readable back, got %+v ok=%v", stored, ok)
	}
	if st := b.Stats(); st.Bakes != 1 || st.LastSize != impl.DefaultLUTSize {
		t.Fatalf("stats bakes=%d last=%d, want 1 and %d", st.Bakes, st.LastSize, impl.DefaultLUTSize)
	}
}

func TestBake3DUsesCubeSize(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	lut, err := b.Bake(impl.LUTFormat3D)
	if err != nil {
		t.Fatalf("bake 3d: %v", err)
	}
	if lut.Size != impl.LUTSize3D {
		t.Fatalf("3d size = %d, want %d", lut.Size, impl.LUTSize3D)
	}
	if want := impl.LUTSize3D * impl.LUTSize3D * impl.LUTSize3D; len(lut.Values) != want {
		t.Fatalf("3d entries = %d, want %d", len(lut.Values), want)
	}
}

func TestBakeHalfClampsToOne(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	lut, err := b.Bake(impl.LUTFormatHalf)
	if err != nil {
		t.Fatalf("bake half: %v", err)
	}
	if len(lut.Half) != impl.DefaultLUTSize {
		t.Fatalf("half entries = %d, want %d", len(lut.Half), impl.DefaultLUTSize)
	}
	for i, raw := range lut.Half {
		if raw > 0x3C00 {
			t.Fatalf("half entry %d = 0x%04X, want it clamped to at most 0x3C00", i, raw)
		}
	}
}

func TestSetLUTSizeValidates(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if err := b.SetLUTSize(0); !errors.Is(err, impl.ErrBadLUTSize) {
		t.Fatalf("size 0 must fail with ErrBadLUTSize, got %v", err)
	}
	if err := b.SetLUTSize(bakerLimits().MaxLUTSize + 1); !errors.Is(err, impl.ErrBadLUTSize) {
		t.Fatalf("a size above the cap must fail with ErrBadLUTSize, got %v", err)
	}
	if err := b.SetLUTSize(32); err != nil {
		t.Fatalf("size 32 must be accepted, got %v", err)
	}
	if got := b.LUTSize(); got != 32 {
		t.Fatalf("lut size = %d, want 32", got)
	}
	lut, err := b.Bake(impl.LUTFormat1D)
	if err != nil {
		t.Fatalf("bake after resize: %v", err)
	}
	if len(lut.Values) != 32 {
		t.Fatalf("baked entries = %d, want 32", len(lut.Values))
	}
}

func TestBakeRejectsUnknownFormat(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if _, err := b.Bake("cube"); !errors.Is(err, impl.ErrBadFormat) {
		t.Fatalf("unknown format must fail with ErrBadFormat, got %v", err)
	}
	if _, ok := b.LUT(); ok {
		t.Fatalf("a rejected bake must not publish a table")
	}
	if err := b.SetWhitePoint(0); !errors.Is(err, impl.ErrBadWhitePoint) {
		t.Fatalf("white point 0 must fail with ErrBadWhitePoint, got %v", err)
	}
	if err := b.SetWhitePoint(1000); err != nil {
		t.Fatalf("white point 1000 must be accepted, got %v", err)
	}
	if got := b.WhitePoint(); got != 1000 {
		t.Fatalf("white point = %v, want 1000", got)
	}
}

func TestSnapshotRestoreKeepsCurveAndLUT(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	params := impl.CurveParams{Exposure: 0.7, Contrast: 1.2, Shoulder: 3, Toe: 0.8}
	if err := b.SetCurve(params); err != nil {
		t.Fatalf("set curve: %v", err)
	}
	if err := b.SetWhitePoint(800); err != nil {
		t.Fatalf("set white point: %v", err)
	}
	if _, err := b.Bake(impl.LUTFormat1D); err != nil {
		t.Fatalf("bake: %v", err)
	}
	data, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewTonemapCurveBaker(bakerLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, ok := restored.Current()
	if !ok || got != params {
		t.Fatalf("restored curve = %+v ok=%v, want %+v", got, ok, params)
	}
	if w := restored.WhitePoint(); w != 800 {
		t.Fatalf("restored white point = %v, want 800", w)
	}
	stored, ok := restored.LUT()
	if !ok || len(stored.Values) != impl.DefaultLUTSize {
		t.Fatalf("restored table = %+v ok=%v, want %d entries", stored, ok, impl.DefaultLUTSize)
	}
	if err := restored.Restore([]byte("<xml/>")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, err := restored.Sample(0.5); err != nil {
		t.Fatalf("a rejected restore must not break sampling, got %v", err)
	}
}

func TestConcurrentBakeSerialized(t *testing.T) {
	b := impl.NewTonemapCurveBaker(bakerLimits())
	if err := b.SetCurve(impl.CurveParams{Exposure: 0.5, Contrast: 1.1, Shoulder: 2.5, Toe: 0.6}); err != nil {
		t.Fatalf("set curve: %v", err)
	}
	var wg sync.WaitGroup
	results := make([]impl.LUT, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = b.Bake(impl.LUTFormat1D)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent bake %d: %v", i, err)
		}
	}
	for i, lut := range results {
		if len(lut.Values) != impl.DefaultLUTSize {
			t.Fatalf("bake %d produced %d entries, want %d", i, len(lut.Values), impl.DefaultLUTSize)
		}
	}
	if st := b.Stats(); st.Bakes != 8 {
		t.Fatalf("bake counter = %d, want 8", st.Bakes)
	}
	if _, ok := b.LUT(); !ok {
		t.Fatalf("a finished bake must leave a readable table")
	}
}
