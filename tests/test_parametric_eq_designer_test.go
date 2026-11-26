package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-01"
)

func eqLimits() impl.Limits {
	return impl.Limits{MaxBands: 6, MinQ: 0.1, MaxQ: 12, MaxGainDB: 18}
}

func peakBand(freq, q, gain float64) impl.BandSpec {
	return impl.BandSpec{Type: impl.BandTypePeak, FreqHz: freq, Q: q, GainDB: gain}
}

// rbjPeak recomputes the RBJ peaking section independently from the package
// under test so coefficient assertions are not tautological.
func rbjPeak(fs, f0, q, gainDB float64) impl.Biquad {
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * f0 / fs
	alpha := math.Sin(w0) / (2 * q)
	cw := math.Cos(w0)
	b0 := 1 + alpha*a
	b1 := -2 * cw
	b2 := 1 - alpha*a
	a0 := 1 + alpha/a
	a1 := -2 * cw
	a2 := 1 - alpha/a
	return impl.Biquad{B0: b0 / a0, B1: b1 / a0, B2: b2 / a0, A1: a1 / a0, A2: a2 / a0}
}

func showMag(c impl.Biquad, fs, f float64) float64 {
	w := 2 * math.Pi * f / fs
	c1, s1 := math.Cos(w), -math.Sin(w)
	c2, s2 := math.Cos(2*w), -math.Sin(2*w)
	nr := c.B0 + c.B1*c1 + c.B2*c2
	ni := c.B1*s1 + c.B2*s2
	dr := 1 + c.A1*c1 + c.A2*c2
	di := c.A1*s1 + c.A2*s2
	return 20 * math.Log10(math.Hypot(nr, ni)/math.Hypot(dr, di))
}

func nearly(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestDesignerDefaults(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	if got := d.SampleRate(); got != impl.DefaultSampleRate {
		t.Fatalf("fresh designer rate = %d, want %d", got, impl.DefaultSampleRate)
	}
	if got := len(d.Bands()); got != 0 {
		t.Fatalf("fresh designer has %d bands, want 0", got)
	}
	if d.Bypassed() {
		t.Fatalf("fresh designer must not be bypassed")
	}
	if got := d.Revision(); got != 0 {
		t.Fatalf("fresh revision = %d, want 0", got)
	}
	if got := d.OutputGain(); got != 0 {
		t.Fatalf("fresh output gain = %v, want 0", got)
	}
	if got := d.GainCompensation(); got != 0 {
		t.Fatalf("compensation while disabled = %v, want 0", got)
	}
}

func TestSetSampleRateValidatesAndRedesigns(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	if err := d.SetSampleRate(8000); !errors.Is(err, impl.ErrSampleRateOutOfRange) {
		t.Fatalf("rate below the floor must fail, got %v", err)
	}
	if err := d.SetSampleRate(400000); !errors.Is(err, impl.ErrSampleRateOutOfRange) {
		t.Fatalf("rate above the ceiling must fail, got %v", err)
	}
	if got := d.SampleRate(); got != impl.DefaultSampleRate {
		t.Fatalf("rejected rates changed the design rate to %d", got)
	}
	id, err := d.AddBand(peakBand(2000, 1.0, 6))
	if err != nil {
		t.Fatalf("add band: %v", err)
	}
	if err := d.SetSampleRate(96000); err != nil {
		t.Fatalf("valid rate switch: %v", err)
	}
	if got := d.SampleRate(); got != 96000 {
		t.Fatalf("rate after switch = %d, want 96000", got)
	}
	c, err := d.Coefficients(id)
	if err != nil {
		t.Fatalf("coefficients after switch: %v", err)
	}
	want := rbjPeak(96000, 2000, 1.0, 6)
	if !nearly(c.B0, want.B0, 1e-9) || !nearly(c.A2, want.A2, 1e-9) {
		t.Fatalf("coefficients were not redesigned for 96 kHz: got %+v want %+v", c, want)
	}
}

func TestAddBandAssignsIDsAndCascadeOrder(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	first, err := d.AddBand(peakBand(3000, 1, 3))
	if err != nil {
		t.Fatalf("add 3 kHz: %v", err)
	}
	second, err := d.AddBand(peakBand(100, 1, 3))
	if err != nil {
		t.Fatalf("add 100 Hz: %v", err)
	}
	third, err := d.AddBand(peakBand(1000, 1, 3))
	if err != nil {
		t.Fatalf("add 1 kHz: %v", err)
	}
	if first == second || second == third || first == third {
		t.Fatalf("band ids must be distinct, got %d %d %d", first, second, third)
	}
	order := d.Bands()
	if len(order) != 3 || order[0] != second || order[1] != third || order[2] != first {
		t.Fatalf("cascade order = %v, want %d %d %d (frequency ascending)", order, second, third, first)
	}
	fourth, err := d.AddBand(peakBand(100, 2, -3))
	if err != nil {
		t.Fatalf("add second 100 Hz band: %v", err)
	}
	order = d.Bands()
	if len(order) != 4 || order[0] != second || order[1] != fourth {
		t.Fatalf("equal frequencies must be ordered by id, got %v", order)
	}
	if err := d.RemoveBand(second); err != nil {
		t.Fatalf("remove band: %v", err)
	}
	fifth, err := d.AddBand(peakBand(2000, 1, 0))
	if err != nil {
		t.Fatalf("add after removal: %v", err)
	}
	if fifth == second {
		t.Fatalf("band id %d was reused after removal", fifth)
	}
}

func TestAddBandRejectsBadSpecs(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	cases := []struct {
		name string
		spec impl.BandSpec
		want error
	}{
		{"unknown type", impl.BandSpec{Type: "notch", FreqHz: 1000, Q: 1}, impl.ErrBadBandType},
		{"frequency too low", peakBand(10, 1, 3), impl.ErrBadFreq},
		{"frequency over the guard band", peakBand(24000, 1, 3), impl.ErrBadFreq},
		{"q too small", peakBand(1000, 0.01, 3), impl.ErrBadQ},
		{"q too big", peakBand(1000, 20, 3), impl.ErrBadQ},
		{"gain too big", peakBand(1000, 1, 30), impl.ErrBadGain},
		{"gain too small", peakBand(1000, 1, -30), impl.ErrBadGain},
	}
	for _, tc := range cases {
		if _, err := d.AddBand(tc.spec); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := len(d.Bands()); got != 0 {
		t.Fatalf("rejected specs created %d bands", got)
	}
	shelf := impl.BandSpec{Type: impl.BandTypeLowShelf, FreqHz: 200, Q: 0.001, GainDB: 6}
	if _, err := d.AddBand(shelf); err != nil {
		t.Fatalf("shelf bands ignore Q and must accept this spec, got %v", err)
	}
}

func TestAddBandEnforcesLimit(t *testing.T) {
	limits := eqLimits()
	limits.MaxBands = 2
	d := impl.NewParametricEqDesigner(limits)
	for _, f := range []float64{100, 1000} {
		if _, err := d.AddBand(peakBand(f, 1, 3)); err != nil {
			t.Fatalf("add %v Hz: %v", f, err)
		}
	}
	if _, err := d.AddBand(peakBand(5000, 1, 3)); !errors.Is(err, impl.ErrBandLimit) {
		t.Fatalf("third band must hit the limit, got %v", err)
	}
	if got := len(d.Bands()); got != 2 {
		t.Fatalf("band count after the limit = %d, want 2", got)
	}
}

func TestUpdateBandRecomputesAndBumpsRevision(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	id, err := d.AddBand(peakBand(1000, 1, 6))
	if err != nil {
		t.Fatalf("add band: %v", err)
	}
	if got := d.Revision(); got != 1 {
		t.Fatalf("revision after one add = %d, want 1", got)
	}
	if err := d.UpdateBand(id, peakBand(4000, 2, -6)); err != nil {
		t.Fatalf("update band: %v", err)
	}
	if got := d.Revision(); got != 2 {
		t.Fatalf("revision after update = %d, want 2", got)
	}
	state, ok := d.Band(id)
	if !ok {
		t.Fatalf("updated band disappeared")
	}
	if state.Spec.FreqHz != 4000 || state.Spec.Q != 2 || state.Spec.GainDB != -6 {
		t.Fatalf("band spec after update = %+v", state.Spec)
	}
	if state.Revision != 2 {
		t.Fatalf("band revision = %d, want the design revision 2", state.Revision)
	}
	c, err := d.Coefficients(id)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	want := rbjPeak(48000, 4000, 2, -6)
	if !nearly(c.B0, want.B0, 1e-9) || !nearly(c.B1, want.B1, 1e-9) || !nearly(c.B2, want.B2, 1e-9) {
		t.Fatalf("coefficients were not recomputed: got %+v want %+v", c, want)
	}
	if err := d.UpdateBand(id, peakBand(1000, 1, 30)); !errors.Is(err, impl.ErrBadGain) {
		t.Fatalf("invalid update must be rejected, got %v", err)
	}
	if got := d.Revision(); got != 2 {
		t.Fatalf("rejected update changed the revision to %d", got)
	}
}

func TestUnknownBandOperationsFail(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	if err := d.UpdateBand(9, peakBand(1000, 1, 3)); !errors.Is(err, impl.ErrBandNotFound) {
		t.Fatalf("updating a missing band must fail, got %v", err)
	}
	if err := d.RemoveBand(9); !errors.Is(err, impl.ErrBandNotFound) {
		t.Fatalf("removing a missing band must fail, got %v", err)
	}
	if _, err := d.Coefficients(9); !errors.Is(err, impl.ErrBandNotFound) {
		t.Fatalf("coefficients of a missing band must fail, got %v", err)
	}
	if _, ok := d.Band(9); ok {
		t.Fatalf("missing band must not be readable")
	}
	id, err := d.AddBand(peakBand(500, 1, 3))
	if err != nil {
		t.Fatalf("add band: %v", err)
	}
	if err := d.RemoveBand(id); err != nil {
		t.Fatalf("remove the band: %v", err)
	}
	if _, err := d.Coefficients(id); !errors.Is(err, impl.ErrBandNotFound) {
		t.Fatalf("coefficients after removal must fail, got %v", err)
	}
}

func TestPeakCoefficientsMatchRbj(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	id, err := d.AddBand(peakBand(1000, 1.0, 6))
	if err != nil {
		t.Fatalf("add band: %v", err)
	}
	got, err := d.Coefficients(id)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	want := rbjPeak(48000, 1000, 1.0, 6)
	if !nearly(got.B0, want.B0, 1e-9) || !nearly(got.B1, want.B1, 1e-9) ||
		!nearly(got.B2, want.B2, 1e-9) || !nearly(got.A1, want.A1, 1e-9) ||
		!nearly(got.A2, want.A2, 1e-9) {
		t.Fatalf("peak section = %+v, want the normalised RBJ section %+v", got, want)
	}
}

func TestPeakResponseAtCenterMatchesGain(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	if _, err := d.AddBand(peakBand(1000, 1.0, 6)); err != nil {
		t.Fatalf("add band: %v", err)
	}
	center, err := d.Response(1000)
	if err != nil {
		t.Fatalf("response at the centre: %v", err)
	}
	if !nearly(center, 6, 1e-6) {
		t.Fatalf("response at the centre = %v dB, want 6", center)
	}
	low, err := d.Response(100)
	if err != nil {
		t.Fatalf("response at 100 Hz: %v", err)
	}
	if math.Abs(low) > 0.5 {
		t.Fatalf("response two decades below the centre = %v dB, want near 0", low)
	}
	high, err := d.Response(8000)
	if err != nil {
		t.Fatalf("response at 8 kHz: %v", err)
	}
	if math.Abs(high) > 0.5 {
		t.Fatalf("response above the centre = %v dB, want near 0", high)
	}
}

func TestCascadeResponseAddsBandContributions(t *testing.T) {
	oneBand := impl.NewParametricEqDesigner(eqLimits())
	if _, err := oneBand.AddBand(peakBand(200, 1, 6)); err != nil {
		t.Fatalf("add first band: %v", err)
	}
	both := impl.NewParametricEqDesigner(eqLimits())
	if _, err := both.AddBand(peakBand(200, 1, 6)); err != nil {
		t.Fatalf("add first band: %v", err)
	}
	if _, err := both.AddBand(peakBand(5000, 1, -9)); err != nil {
		t.Fatalf("add second band: %v", err)
	}
	other := impl.NewParametricEqDesigner(eqLimits())
	if _, err := other.AddBand(peakBand(5000, 1, -9)); err != nil {
		t.Fatalf("add isolated band: %v", err)
	}
	a, err := oneBand.Response(1000)
	if err != nil {
		t.Fatalf("single band response: %v", err)
	}
	b, err := both.Response(1000)
	if err != nil {
		t.Fatalf("cascade response: %v", err)
	}
	c, err := other.Response(1000)
	if err != nil {
		t.Fatalf("isolated band response: %v", err)
	}
	if !nearly(b, a+c, 1e-6) {
		t.Fatalf("cascade response %v dB, want the sum %v of the two isolated responses", b, a+c)
	}
	if len(both.Bands()) != 2 {
		t.Fatalf("cascade lost a band")
	}
}

func TestShelfAndPassTypeResponse(t *testing.T) {
	low := impl.NewParametricEqDesigner(eqLimits())
	if _, err := low.AddBand(impl.BandSpec{Type: impl.BandTypeLowShelf, FreqHz: 200, Q: 0.7, GainDB: 6}); err != nil {
		t.Fatalf("add low shelf: %v", err)
	}
	dc, err := low.Response(1)
	if err != nil {
		t.Fatalf("low shelf response at dc: %v", err)
	}
	if !nearly(dc, 6, 1e-3) {
		t.Fatalf("low shelf at dc = %v dB, want its full gain 6", dc)
	}
	top, err := low.Response(8000)
	if err != nil {
		t.Fatalf("low shelf response at 8 kHz: %v", err)
	}
	if math.Abs(top) > 0.5 {
		t.Fatalf("low shelf at 8 kHz = %v dB, want near 0", top)
	}
	hp := impl.NewParametricEqDesigner(eqLimits())
	if _, err := hp.AddBand(impl.BandSpec{Type: impl.BandTypeHighPass, FreqHz: 500, Q: 1, GainDB: 0}); err != nil {
		t.Fatalf("add high pass: %v", err)
	}
	pass, err := hp.Response(100)
	if err != nil {
		t.Fatalf("high pass at 100 Hz: %v", err)
	}
	if pass < -1 {
		t.Fatalf("high pass still attenuates its pass band: %v dB", pass)
	}
	stop, err := hp.Response(8000)
	if err != nil {
		t.Fatalf("high pass at 8 kHz: %v", err)
	}
	if stop > -30 {
		t.Fatalf("high pass stop band = %v dB, want a deep cut", stop)
	}
}

func TestResponseRejectsBadProbeFreq(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	if _, err := d.AddBand(peakBand(1000, 1, 3)); err != nil {
		t.Fatalf("add band: %v", err)
	}
	for _, f := range []float64{0, -100, 24000} {
		if _, err := d.Response(f); !errors.Is(err, impl.ErrBadProbeFreq) {
			t.Fatalf("probe %v Hz must be rejected, got %v", f, err)
		}
	}
	if _, err := d.Response(23999); err != nil {
		t.Fatalf("probe just under nyquist must work, got %v", err)
	}
	if err := d.SetSampleRate(22050); err != nil {
		t.Fatalf("rate switch: %v", err)
	}
	if _, err := d.Response(11024); err != nil {
		t.Fatalf("probe under the new nyquist must work, got %v", err)
	}
}

func TestGlobalBypassAndOutputGain(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	if _, err := d.AddBand(peakBand(1000, 1, 6)); err != nil {
		t.Fatalf("add band: %v", err)
	}
	if err := d.SetOutputGain(-4); err != nil {
		t.Fatalf("set output gain: %v", err)
	}
	if got := d.OutputGain(); got != -4 {
		t.Fatalf("output gain = %v, want -4", got)
	}
	withBand, err := d.Response(1000)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	if !nearly(withBand, 2, 1e-6) {
		t.Fatalf("band plus trim = %v dB, want 6-4", withBand)
	}
	if err := d.SetBypass(true); err != nil {
		t.Fatalf("set bypass: %v", err)
	}
	if !d.Bypassed() {
		t.Fatalf("bypass flag was not stored")
	}
	flat, err := d.Response(1000)
	if err != nil {
		t.Fatalf("response while bypassed: %v", err)
	}
	if !nearly(flat, -4, 1e-9) {
		t.Fatalf("bypassed response = %v dB, want the bare output trim -4", flat)
	}
	if err := d.SetBypass(false); err != nil {
		t.Fatalf("clear bypass: %v", err)
	}
	back, err := d.Response(1000)
	if err != nil {
		t.Fatalf("response after bypass: %v", err)
	}
	if !nearly(back, 2, 1e-6) {
		t.Fatalf("response after clearing the bypass = %v dB, want 2", back)
	}
	if err := d.SetOutputGain(25); !errors.Is(err, impl.ErrBadOutputGain) {
		t.Fatalf("trim above the headroom must fail, got %v", err)
	}
	if err := d.SetOutputGain(-25); !errors.Is(err, impl.ErrBadOutputGain) {
		t.Fatalf("trim below the headroom must fail, got %v", err)
	}
}

func TestConcurrentUpdateAndResponse(t *testing.T) {
	d := impl.NewParametricEqDesigner(eqLimits())
	id, err := d.AddBand(peakBand(1000, 1, 6))
	if err != nil {
		t.Fatalf("add band: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				freq := 500.0
				if (w+i)%2 == 0 {
					freq = 3000
				}
				if err := d.UpdateBand(id, peakBand(freq, 1, 6)); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				value, err := d.Response(1000)
				if err != nil {
					errs <- err
					return
				}
				if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 60 {
					errs <- errors.New("response out of range while bands change")
					return
				}
				if _, err := d.Coefficients(id); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access failed: %v", err)
	}
	if got := d.Revision(); got != 161 {
		t.Fatalf("revision after 160 updates = %d, want 161", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	limits := eqLimits()
	d := impl.NewParametricEqDesigner(limits)
	if err := d.SetSampleRate(44100); err != nil {
		t.Fatalf("rate switch: %v", err)
	}
	first, err := d.AddBand(peakBand(250, 0.8, 5))
	if err != nil {
		t.Fatalf("add first band: %v", err)
	}
	second, err := d.AddBand(impl.BandSpec{Type: impl.BandTypeHighShelf, FreqHz: 6000, Q: 1, GainDB: -4})
	if err != nil {
		t.Fatalf("add second band: %v", err)
	}
	if err := d.SetOutputGain(-2.5); err != nil {
		t.Fatalf("output gain: %v", err)
	}
	revision := d.Revision()
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewParametricEqDesigner(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.SampleRate(); got != 44100 {
		t.Fatalf("restored rate = %d, want 44100", got)
	}
	if got := len(restored.Bands()); got != 2 {
		t.Fatalf("restored band count = %d, want 2", got)
	}
	if got := restored.OutputGain(); got != -2.5 {
		t.Fatalf("restored output gain = %v, want -2.5", got)
	}
	if got := restored.Revision(); got != revision {
		t.Fatalf("restored revision = %d, want %d", got, revision)
	}
	order := restored.Bands()
	if order[0] != first || order[1] != second {
		t.Fatalf("restored cascade order = %v, want %d then %d", order, first, second)
	}
	want, err := d.Coefficients(second)
	if err != nil {
		t.Fatalf("source coefficients: %v", err)
	}
	got2, err := restored.Coefficients(second)
	if err != nil {
		t.Fatalf("restored coefficients: %v", err)
	}
	if !nearly(got2.B0, want.B0, 1e-12) || !nearly(got2.A2, want.A2, 1e-12) {
		t.Fatalf("restored coefficients %+v differ from %+v", got2, want)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(restored.Bands()); got != 2 {
		t.Fatalf("failed restore dropped bands: %d left", got)
	}
	if got := restored.Revision(); got != revision {
		t.Fatalf("failed restore changed the revision to %d", got)
	}
	fresh, err := restored.AddBand(peakBand(12000, 1, 2))
	if err != nil {
		t.Fatalf("add band after restore: %v", err)
	}
	if fresh <= second {
		t.Fatalf("band id after restore = %d, must continue past %d", fresh, second)
	}
	if got := restored.Revision(); got != revision+1 {
		t.Fatalf("revision after the new band = %d, want %d", got, revision+1)
	}
}
