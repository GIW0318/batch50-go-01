package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-09"
)

func sequencerConfig() impl.SampleConfig {
	return impl.SampleConfig{
		Width:              8,
		Height:             8,
		FrameSamples:       64,
		MaxSamplesPerPixel: 16,
		ConvergeSamples:    4,
	}
}

func fillSample(pixel, index int) impl.Sample {
	return impl.Sample{Pixel: pixel, Index: index, U: 0.5, V: 0.5}
}

func TestNewNormalisesConfig(t *testing.T) {
	p := impl.NewPathSampleSequencer(impl.SampleConfig{})
	cfg := p.Config()
	if cfg.Width != impl.DefaultWidth || cfg.Height != impl.DefaultHeight {
		t.Fatalf("default size = %dx%d, want %dx%d", cfg.Width, cfg.Height, impl.DefaultWidth, impl.DefaultHeight)
	}
	if cfg.FrameSamples != impl.DefaultFrameSamples {
		t.Fatalf("default frame budget = %d, want %d", cfg.FrameSamples, impl.DefaultFrameSamples)
	}
	if cfg.MaxSamplesPerPixel != impl.DefaultMaxSamplesPerPixel {
		t.Fatalf("default pixel cap = %d, want %d", cfg.MaxSamplesPerPixel, impl.DefaultMaxSamplesPerPixel)
	}
	if cfg.ConvergeSamples != impl.DefaultConvergeSamples {
		t.Fatalf("default convergence target = %d, want %d", cfg.ConvergeSamples, impl.DefaultConvergeSamples)
	}
	if stats := p.Stats(); stats.Pixels != cfg.Width*cfg.Height {
		t.Fatalf("pixel count = %d, want %d", stats.Pixels, cfg.Width*cfg.Height)
	}
	partial := impl.NewPathSampleSequencer(impl.SampleConfig{Width: 4, Height: 3, FrameSamples: -5, ConvergeSamples: 2})
	got := partial.Config()
	if got.Width != 4 || got.Height != 3 || got.ConvergeSamples != 2 {
		t.Fatalf("explicit config fields lost: %+v", got)
	}
	if got.FrameSamples != impl.DefaultFrameSamples {
		t.Fatalf("negative budget must fall back to %d, got %d", impl.DefaultFrameSamples, got.FrameSamples)
	}
	if got2 := partial.Stats().Pixels; got2 != 12 {
		t.Fatalf("pixel count = %d, want 12", got2)
	}
}

func TestBeginFrameRejectsNonIncreasing(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(4); err != nil {
		t.Fatalf("begin frame 4: %v", err)
	}
	if err := p.BeginFrame(4); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("repeating frame 4 must fail with ErrBadFrame, got %v", err)
	}
	if err := p.BeginFrame(3); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("going back to frame 3 must fail with ErrBadFrame, got %v", err)
	}
	if got := p.Frame(); got != 4 {
		t.Fatalf("frame after rejected begin = %d, want 4", got)
	}
}

func TestSamplesForRequiresFrame(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if _, err := p.SamplesFor(0, 1); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("sampling before BeginFrame must fail with ErrNoFrame, got %v", err)
	}
	if _, err := p.NextSample(0); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("NextSample before BeginFrame must fail with ErrNoFrame, got %v", err)
	}
	if err := p.Accumulate(0, fillSample(0, 1), impl.Color{R: 1}); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("accumulating before BeginFrame must fail with ErrNoFrame, got %v", err)
	}
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	if _, err := p.SamplesFor(0, 1); err != nil {
		t.Fatalf("sampling after BeginFrame: %v", err)
	}
}

func TestSamplesForRejectsBadPixelAndCount(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	if _, err := p.SamplesFor(-1, 1); !errors.Is(err, impl.ErrBadPixel) {
		t.Fatalf("negative pixel must fail with ErrBadPixel, got %v", err)
	}
	if _, err := p.SamplesFor(64, 1); !errors.Is(err, impl.ErrBadPixel) {
		t.Fatalf("pixel past the last one must fail with ErrBadPixel, got %v", err)
	}
	if _, err := p.NextSample(64); !errors.Is(err, impl.ErrBadPixel) {
		t.Fatalf("NextSample past the last pixel must fail with ErrBadPixel, got %v", err)
	}
	if _, err := p.SamplesFor(0, 0); !errors.Is(err, impl.ErrBadCount) {
		t.Fatalf("zero count must fail with ErrBadCount, got %v", err)
	}
	if _, err := p.SamplesFor(0, -3); !errors.Is(err, impl.ErrBadCount) {
		t.Fatalf("negative count must fail with ErrBadCount, got %v", err)
	}
	if got := p.BudgetRemaining(); got != 64 {
		t.Fatalf("rejected calls consumed budget, remaining = %d, want 64", got)
	}
}

func TestSamplesForContinuesIndices(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	first, err := p.SamplesFor(9, 4)
	if err != nil {
		t.Fatalf("first SamplesFor: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("first call returned %d samples, want 4", len(first))
	}
	for i, s := range first {
		if s.Pixel != 9 {
			t.Fatalf("sample %d pixel = %d, want 9", i, s.Pixel)
		}
		if s.Index != i+1 {
			t.Fatalf("sample %d index = %d, want %d", i, s.Index, i+1)
		}
	}
	second, err := p.SamplesFor(9, 2)
	if err != nil {
		t.Fatalf("second SamplesFor: %v", err)
	}
	if len(second) != 2 || second[0].Index != 5 || second[1].Index != 6 {
		t.Fatalf("second call indices = %+v, want 5 and 6", second)
	}
	other, err := p.SamplesFor(10, 2)
	if err != nil {
		t.Fatalf("SamplesFor on another pixel: %v", err)
	}
	if other[0].Index != 1 || other[1].Index != 2 {
		t.Fatalf("another pixel must start at index 1, got %+v", other)
	}
	next, err := p.NextSample(9)
	if err != nil {
		t.Fatalf("NextSample: %v", err)
	}
	if next.Index != 7 || next.Pixel != 9 {
		t.Fatalf("NextSample = %+v, want pixel 9 index 7", next)
	}
}

func TestSamplesForIsDeterministicAndInUnitSquare(t *testing.T) {
	cfg := sequencerConfig()
	a := impl.NewPathSampleSequencer(cfg)
	b := impl.NewPathSampleSequencer(cfg)
	for _, p := range []*impl.PathSampleSequencer{a, b} {
		if err := p.BeginFrame(1); err != nil {
			t.Fatalf("begin frame 1: %v", err)
		}
	}
	sa, err := a.SamplesFor(37, 6)
	if err != nil {
		t.Fatalf("samples from a: %v", err)
	}
	sb, err := b.SamplesFor(37, 6)
	if err != nil {
		t.Fatalf("samples from b: %v", err)
	}
	for i := range sa {
		if sa[i].Index != sb[i].Index || sa[i].U != sb[i].U || sa[i].V != sb[i].V {
			t.Fatalf("sample %d differs between equal sequencers: %+v vs %+v", i, sa[i], sb[i])
		}
		if math.IsNaN(sa[i].U) || math.IsNaN(sa[i].V) {
			t.Fatalf("sample %d coordinates are NaN", i)
		}
		if sa[i].U < 0 || sa[i].U >= 1 || sa[i].V < 0 || sa[i].V >= 1 {
			t.Fatalf("sample %d = (%v,%v), want both inside [0,1)", i, sa[i].U, sa[i].V)
		}
	}
}

func TestSamplesForScramblesPerPixel(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	left, err := p.SamplesFor(1, 4)
	if err != nil {
		t.Fatalf("samples for pixel 1: %v", err)
	}
	right, err := p.SamplesFor(2, 4)
	if err != nil {
		t.Fatalf("samples for pixel 2: %v", err)
	}
	identical := true
	for i := range left {
		if left[i].U != right[i].U || left[i].V != right[i].V {
			identical = false
			break
		}
	}
	if identical {
		t.Fatalf("two pixels got the same offsets, per pixel decorrelation is missing")
	}
}

func TestFrameBudgetIsExact(t *testing.T) {
	p := impl.NewPathSampleSequencer(impl.SampleConfig{Width: 8, Height: 8, FrameSamples: 10, MaxSamplesPerPixel: 16, ConvergeSamples: 4})
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	if got := p.BudgetRemaining(); got != 10 {
		t.Fatalf("budget at frame start = %d, want 10", got)
	}
	if _, err := p.SamplesFor(0, 4); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if _, err := p.SamplesFor(1, 6); err != nil {
		t.Fatalf("batch that exactly fills the budget: %v", err)
	}
	if got := p.BudgetRemaining(); got != 0 {
		t.Fatalf("budget after the exact fill = %d, want 0", got)
	}
	if _, err := p.SamplesFor(2, 1); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("sampling past the budget must fail with ErrBudgetExhausted, got %v", err)
	}
	if got := p.BudgetRemaining(); got != 0 {
		t.Fatalf("rejected batch must not consume budget, remaining = %d", got)
	}
	if _, err := p.SamplesFor(2, 4); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("an oversized batch must fail as a whole, got %v", err)
	}
	if err := p.BeginFrame(2); err != nil {
		t.Fatalf("begin frame 2: %v", err)
	}
	if got := p.BudgetRemaining(); got != 10 {
		t.Fatalf("budget in the next frame = %d, want 10", got)
	}
	if stats := p.Stats(); stats.Budget != 10 || stats.Frame != 2 {
		t.Fatalf("stats = %+v, want budget 10 in frame 2", stats)
	}
}

func TestAccumulateAveragesSamples(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	batched, err := p.SamplesFor(5, 2)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	if err := p.Accumulate(5, batched[0], impl.Color{R: 1, G: 0, B: 0}); err != nil {
		t.Fatalf("first accumulate: %v", err)
	}
	if err := p.Accumulate(5, batched[1], impl.Color{R: 0, G: 3, B: 0}); err != nil {
		t.Fatalf("second accumulate: %v", err)
	}
	state, err := p.Convergence(5)
	if err != nil {
		t.Fatalf("convergence: %v", err)
	}
	if state.Samples != 2 {
		t.Fatalf("sample count = %d, want 2", state.Samples)
	}
	if math.Abs(state.Mean.R-0.5) > 1e-9 || math.Abs(state.Mean.G-1.5) > 1e-9 || state.Mean.B != 0 {
		t.Fatalf("mean = %+v, want (0.5,1.5,0)", state.Mean)
	}
	if got := p.SampleCount(5); got != 2 {
		t.Fatalf("SampleCount = %d, want 2", got)
	}
	if got := p.SampleCount(6); got != 0 {
		t.Fatalf("SampleCount of an untouched pixel = %d, want 0", got)
	}
}

func TestAccumulateRejectsBadColor(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	bad := []impl.Color{
		{R: math.NaN()},
		{G: math.Inf(1)},
		{B: -1},
	}
	for i, value := range bad {
		if err := p.Accumulate(1, fillSample(1, 1), value); !errors.Is(err, impl.ErrBadColor) {
			t.Fatalf("color case %d must fail with ErrBadColor, got %v", i, err)
		}
	}
	state, err := p.Convergence(1)
	if err != nil {
		t.Fatalf("convergence: %v", err)
	}
	if state.Samples != 0 {
		t.Fatalf("rejected values must not accumulate, sample count = %d", state.Samples)
	}
}

func TestAccumulateRejectsMismatchedSample(t *testing.T) {
	p := impl.NewPathSampleSequencer(sequencerConfig())
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	if err := p.Accumulate(1, fillSample(2, 1), impl.Color{R: 1}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("sample of another pixel must fail with ErrBadSample, got %v", err)
	}
	if err := p.Accumulate(1, fillSample(1, 0), impl.Color{R: 1}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("zero sample index must fail with ErrBadSample, got %v", err)
	}
	if err := p.Accumulate(1, fillSample(1, -2), impl.Color{R: 1}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("negative sample index must fail with ErrBadSample, got %v", err)
	}
	if err := p.Accumulate(1, fillSample(1, 1), impl.Color{R: 1}); err != nil {
		t.Fatalf("a well formed sample must accumulate, got %v", err)
	}
	if got := p.SampleCount(1); got != 1 {
		t.Fatalf("sample count = %d, want 1", got)
	}
}

func TestAccumulateEnforcesPixelCap(t *testing.T) {
	p := impl.NewPathSampleSequencer(impl.SampleConfig{Width: 4, Height: 4, FrameSamples: 32, MaxSamplesPerPixel: 2, ConvergeSamples: 2})
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	for index := 1; index <= 2; index++ {
		if err := p.Accumulate(3, fillSample(3, index), impl.Color{R: 1}); err != nil {
			t.Fatalf("accumulate sample %d: %v", index, err)
		}
	}
	if err := p.Accumulate(3, fillSample(3, 3), impl.Color{R: 1}); !errors.Is(err, impl.ErrSampleCap) {
		t.Fatalf("third accumulation over a cap of two must fail with ErrSampleCap, got %v", err)
	}
	if got := p.SampleCount(3); got != 2 {
		t.Fatalf("sample count after the cap = %d, want 2", got)
	}
}

func TestConvergenceReportsSamples(t *testing.T) {
	p := impl.NewPathSampleSequencer(impl.SampleConfig{Width: 4, Height: 4, FrameSamples: 32, MaxSamplesPerPixel: 8, ConvergeSamples: 3})
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	empty, err := p.Convergence(0)
	if err != nil {
		t.Fatalf("convergence of an untouched pixel: %v", err)
	}
	if empty.Samples != 0 || empty.Converged {
		t.Fatalf("untouched pixel = %+v, want zero samples and not converged", empty)
	}
	for index := 1; index <= 3; index++ {
		if err := p.Accumulate(0, fillSample(0, index), impl.Color{R: 2, G: 2, B: 2}); err != nil {
			t.Fatalf("accumulate sample %d: %v", index, err)
		}
	}
	state, err := p.Convergence(0)
	if err != nil {
		t.Fatalf("convergence: %v", err)
	}
	if state.Samples != 3 || !state.Converged {
		t.Fatalf("state = %+v, want 3 samples and converged", state)
	}
	if state.Mean.R != 2 || state.Mean.G != 2 || state.Mean.B != 2 {
		t.Fatalf("mean = %+v, want (2,2,2)", state.Mean)
	}
	if _, err := p.Convergence(16); !errors.Is(err, impl.ErrBadPixel) {
		t.Fatalf("pixel past the last one must fail with ErrBadPixel, got %v", err)
	}
}

func TestSnapshotRestoreKeepsAccumulation(t *testing.T) {
	cfg := impl.SampleConfig{Width: 8, Height: 8, FrameSamples: 100, MaxSamplesPerPixel: 64, ConvergeSamples: 3}
	p := impl.NewPathSampleSequencer(cfg)
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	samples, err := p.SamplesFor(9, 2)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	if err := p.Accumulate(9, samples[0], impl.Color{R: 1, G: 2, B: 3}); err != nil {
		t.Fatalf("first accumulate: %v", err)
	}
	if err := p.Accumulate(9, samples[1], impl.Color{R: 3, G: 4, B: 5}); err != nil {
		t.Fatalf("second accumulate: %v", err)
	}
	if err := p.Reset("   "); !errors.Is(err, impl.ErrBadReason) {
		t.Fatalf("blank reset reason must fail with ErrBadReason, got %v", err)
	}
	if got := p.SampleCount(9); got != 2 {
		t.Fatalf("a rejected reset must keep the buffer, sample count = %d", got)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewPathSampleSequencer(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Frame(); got != 1 {
		t.Fatalf("restored frame = %d, want 1", got)
	}
	state, err := restored.Convergence(9)
	if err != nil {
		t.Fatalf("restored convergence: %v", err)
	}
	if state.Samples != 2 {
		t.Fatalf("restored sample count = %d, want 2", state.Samples)
	}
	if math.Abs(state.Mean.R-2) > 1e-9 || math.Abs(state.Mean.G-3) > 1e-9 || math.Abs(state.Mean.B-4) > 1e-9 {
		t.Fatalf("restored mean = %+v, want (2,3,4)", state.Mean)
	}
	if got := restored.BudgetRemaining(); got != 98 {
		t.Fatalf("restored budget = %d, want 98", got)
	}
	if err := restored.BeginFrame(1); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("frame 1 must already be spent after restore, got %v", err)
	}
	if err := restored.Restore([]byte("nope")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.SampleCount(9); got != 2 {
		t.Fatalf("rejected restore must not touch the buffer, sample count = %d", got)
	}
}

func TestConcurrentAccumulateKeepsTotals(t *testing.T) {
	p := impl.NewPathSampleSequencer(impl.SampleConfig{Width: 4, Height: 4, FrameSamples: 64, MaxSamplesPerPixel: 32, ConvergeSamples: 4})
	if err := p.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := p.Accumulate(3, fillSample(3, i+1), impl.Color{R: float64(i), G: 1, B: 0}); err != nil {
				t.Errorf("accumulate %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	state, err := p.Convergence(3)
	if err != nil {
		t.Fatalf("convergence: %v", err)
	}
	if state.Samples != 8 {
		t.Fatalf("sample count after the race = %d, want 8", state.Samples)
	}
	if math.Abs(state.Mean.R-3.5) > 1e-9 {
		t.Fatalf("mean red after the race = %v, want 3.5", state.Mean.R)
	}
	if math.Abs(state.Mean.G-1) > 1e-9 {
		t.Fatalf("mean green after the race = %v, want 1", state.Mean.G)
	}
}
