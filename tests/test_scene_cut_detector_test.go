package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-04"
)

func cutConfig() impl.CutConfig {
	return impl.DefaultCutConfig()
}

func histPeak(bins, pos int) []float64 {
	out := make([]float64, bins)
	out[pos] = 1
	return out
}

func flatHist(bins int) []float64 {
	out := make([]float64, bins)
	for i := range out {
		out[i] = 1 / float64(bins)
	}
	return out
}

func TestNewDetectorState(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	if err := detector.Validate(); err != nil {
		t.Fatalf("the reference configuration must validate, got %v", err)
	}
	if got := detector.FrameCount(); got != 0 {
		t.Fatalf("fresh frame count = %d, want 0", got)
	}
	high, low := detector.Thresholds()
	if high != impl.DefaultHighThreshold || low != impl.DefaultLowThreshold {
		t.Fatalf("thresholds = %v/%v, want %v/%v", high, low, impl.DefaultHighThreshold, impl.DefaultLowThreshold)
	}
	if got := detector.MinInterval(); got != impl.DefaultMinIntervalMS {
		t.Fatalf("minimum interval = %d, want %d", got, impl.DefaultMinIntervalMS)
	}
	if got := len(detector.Cuts()); got != 0 {
		t.Fatalf("fresh cut list has %d entries", got)
	}
}

func TestFirstFrameHasZeroScore(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	verdict, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 0), Edges: 0.5}, 1000)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if verdict.Index != 0 {
		t.Fatalf("first verdict index = %d, want 0", verdict.Index)
	}
	if verdict.Score != 0 {
		t.Fatalf("first verdict score = %v, want 0", verdict.Score)
	}
	if verdict.Cut {
		t.Fatalf("the first frame must never be a cut")
	}
	if verdict.PTS != 1000 {
		t.Fatalf("verdict pts = %d, want the pushed 1000", verdict.PTS)
	}
	if got := detector.FrameCount(); got != 1 {
		t.Fatalf("frame count = %d, want 1", got)
	}
}

func TestPushFrameRejectsBadEvidence(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	bad := []struct {
		name  string
		frame impl.FrameStats
		want  error
	}{
		{"short histogram", impl.FrameStats{Luma: []float64{1}, Edges: 0.5}, impl.ErrBadHistogram},
		{"negative bin", impl.FrameStats{Luma: []float64{-1, 1, 0, 0, 0, 0, 0, 1}, Edges: 0.5}, impl.ErrBadHistogram},
		{"unnormalised", impl.FrameStats{Luma: []float64{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1}, Edges: 0.5}, impl.ErrBadHistogram},
		{"not a number", impl.FrameStats{Luma: []float64{math.NaN(), 1, 0, 0, 0, 0, 0, 0}, Edges: 0.5}, impl.ErrBadHistogram},
		{"edges above one", impl.FrameStats{Luma: histPeak(8, 0), Edges: 1.5}, impl.ErrBadEdges},
		{"edges not a number", impl.FrameStats{Luma: histPeak(8, 0), Edges: math.NaN()}, impl.ErrBadEdges},
	}
	for _, tc := range bad {
		if _, err := detector.PushFrame(tc.frame, 0); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := detector.FrameCount(); got != 0 {
		t.Fatalf("rejected frames advanced the count to %d", got)
	}
	if _, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 0), Edges: 0.5}, 0); err != nil {
		t.Fatalf("well formed frame: %v", err)
	}
	if got := detector.FrameCount(); got != 1 {
		t.Fatalf("frame count after one accepted frame = %d, want 1", got)
	}
}

func TestHistogramScoreMatchesWeights(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	if _, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 0), Edges: 0.5}, 0); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	verdict, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 7), Edges: 0.5}, 40)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if math.Abs(verdict.Score-0.7) > 1e-9 {
		t.Fatalf("score = %v, want the histogram weight 0.7 for a full histogram change", verdict.Score)
	}
	stored, err := detector.Score(1)
	if err != nil {
		t.Fatalf("read score: %v", err)
	}
	if math.Abs(stored-0.7) > 1e-9 {
		t.Fatalf("stored score = %v, want 0.7", stored)
	}
}

func TestEdgeScoreMatchesWeights(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	if _, err := detector.PushFrame(impl.FrameStats{Luma: flatHist(8), Edges: 0.2}, 0); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	verdict, err := detector.PushFrame(impl.FrameStats{Luma: flatHist(8), Edges: 0.8}, 40)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if math.Abs(verdict.Score-0.225) > 1e-9 {
		t.Fatalf("score = %v, want 0.3 * (0.6/0.8) = 0.225", verdict.Score)
	}
	if verdict.Cut {
		t.Fatalf("a score of 0.225 sits below the low threshold and must not be a cut")
	}
}

func TestHardCutReportedOnce(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	steps := []struct {
		hist  []float64
		pts   int64
		score float64
	}{
		{histPeak(8, 0), 0, 0},
		{histPeak(8, 7), 500, 0.7},
		{histPeak(8, 7), 1000, 0},
		{histPeak(8, 7), 1500, 0},
	}
	for i, step := range steps {
		verdict, err := detector.PushFrame(impl.FrameStats{Luma: step.hist, Edges: 0.5}, step.pts)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if math.Abs(verdict.Score-step.score) > 1e-9 {
			t.Fatalf("frame %d score = %v, want %v", i, verdict.Score, step.score)
		}
		if step.score >= impl.DefaultHighThreshold && !verdict.Cut {
			t.Fatalf("frame %d must be reported as a cut", i)
		}
		if step.score == 0 && verdict.Cut {
			t.Fatalf("frame %d repeats the previous picture and must not be a cut", i)
		}
	}
	cuts := detector.Cuts()
	if len(cuts) != 1 {
		t.Fatalf("cut list = %+v, want exactly one cut", cuts)
	}
	if cuts[0].Index != 1 || cuts[0].PTS != 500 {
		t.Fatalf("cut = %+v, want index 1 at pts 500", cuts[0])
	}
	if math.Abs(cuts[0].Score-0.7) > 1e-9 {
		t.Fatalf("cut score = %v, want 0.7", cuts[0].Score)
	}
}

func TestIdenticalFramesProduceNoCut(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	for i := 0; i < 5; i++ {
		verdict, err := detector.PushFrame(impl.FrameStats{Luma: flatHist(8), Edges: 0.42}, int64(i)*500)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if verdict.Score != 0 {
			t.Fatalf("frame %d reports score %v for an unchanged picture", i, verdict.Score)
		}
		if verdict.Cut {
			t.Fatalf("frame %d was reported as a cut", i)
		}
	}
	if got := len(detector.Cuts()); got != 0 {
		t.Fatalf("unchanged clip produced %d cuts", got)
	}
}

func TestSetThresholdsValidates(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	if err := detector.SetThresholds(0.3, 0.6); !errors.Is(err, impl.ErrBadThreshold) {
		t.Fatalf("high below low must fail, got %v", err)
	}
	if err := detector.SetThresholds(0.55, 0); !errors.Is(err, impl.ErrBadThreshold) {
		t.Fatalf("zero low threshold must fail, got %v", err)
	}
	if err := detector.SetThresholds(1.2, 0.3); !errors.Is(err, impl.ErrBadThreshold) {
		t.Fatalf("high above one must fail, got %v", err)
	}
	if err := detector.SetThresholds(math.NaN(), 0.3); !errors.Is(err, impl.ErrBadThreshold) {
		t.Fatalf("NaN high threshold must fail, got %v", err)
	}
	high, low := detector.Thresholds()
	if high != impl.DefaultHighThreshold || low != impl.DefaultLowThreshold {
		t.Fatalf("rejected sets changed the thresholds to %v/%v", high, low)
	}
	if err := detector.SetThresholds(0.6, 0.35); err != nil {
		t.Fatalf("valid thresholds: %v", err)
	}
	high, low = detector.Thresholds()
	if high != 0.6 || low != 0.35 {
		t.Fatalf("thresholds = %v/%v, want 0.6/0.35", high, low)
	}
}

func TestSetMinIntervalValidates(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	if err := detector.SetMinInterval(-1); !errors.Is(err, impl.ErrBadMinInterval) {
		t.Fatalf("negative interval must fail, got %v", err)
	}
	if got := detector.MinInterval(); got != impl.DefaultMinIntervalMS {
		t.Fatalf("rejected interval changed the value to %d", got)
	}
	if err := detector.SetMinInterval(0); err != nil {
		t.Fatalf("zero interval must be accepted: %v", err)
	}
	if err := detector.SetMinInterval(250); err != nil {
		t.Fatalf("valid interval: %v", err)
	}
	if got := detector.MinInterval(); got != 250 {
		t.Fatalf("interval = %d, want 250", got)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		edit func(*impl.CutConfig)
		want error
	}{
		{"weights do not add up", func(c *impl.CutConfig) { c.HistWeight = 0.5 }, impl.ErrBadWeights},
		{"single bin", func(c *impl.CutConfig) { c.HistBins = 1 }, impl.ErrBadBins},
		{"zero frame duration", func(c *impl.CutConfig) { c.NominalFrameMS = 0 }, impl.ErrBadNominal},
		{"zero history", func(c *impl.CutConfig) { c.MaxHistory = 0 }, impl.ErrBadHistory},
		{"zero high threshold", func(c *impl.CutConfig) { c.HighThreshold = 0 }, impl.ErrBadThreshold},
		{"low above high", func(c *impl.CutConfig) { c.LowThreshold = 0.9 }, impl.ErrBadThreshold},
	}
	for _, tc := range cases {
		cfg := impl.DefaultCutConfig()
		tc.edit(&cfg)
		if err := impl.NewSceneCutDetector(cfg).Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	bad := impl.DefaultCutConfig()
	bad.EdgeWeight = 0.9
	if _, err := impl.NewSceneCutDetector(bad).PushFrame(impl.FrameStats{Luma: flatHist(8), Edges: 0.5}, 0); !errors.Is(err, impl.ErrBadConfig) {
		t.Fatalf("pushing with an invalid configuration must fail with ErrBadConfig, got %v", err)
	}
}

func TestFlushReturnsStableCutList(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	frames := []impl.FrameStats{
		{Luma: histPeak(8, 0), Edges: 0.5},
		{Luma: histPeak(8, 7), Edges: 0.5},
		{Luma: histPeak(8, 7), Edges: 0.5},
	}
	for i, frame := range frames {
		if _, err := detector.PushFrame(frame, int64(i)*500); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	first, err := detector.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(first) != 1 || first[0].Index != 1 {
		t.Fatalf("flush returned %+v, want the single cut at index 1", first)
	}
	second, err := detector.Flush()
	if err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("second flush returned %d cuts, the first returned %d", len(second), len(first))
	}
	if _, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 7), Edges: 0.5}, 2000); err != nil {
		t.Fatalf("push after flush: %v", err)
	}
	if got := detector.FrameCount(); got != 4 {
		t.Fatalf("frame count after the post flush frame = %d, want 4", got)
	}
}

func TestResetRebuildsClip(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	if _, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 0), Edges: 0.5}, 0); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if _, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 7), Edges: 0.5}, 500); err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if got := len(detector.Cuts()); got != 1 {
		t.Fatalf("cut count before reset = %d, want 1", got)
	}
	if err := detector.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := detector.FrameCount(); got != 0 {
		t.Fatalf("frame count after reset = %d, want 0", got)
	}
	if got := len(detector.Cuts()); got != 0 {
		t.Fatalf("cut list after reset has %d entries", got)
	}
	if _, err := detector.Score(0); !errors.Is(err, impl.ErrIndexOutOfRange) {
		t.Fatalf("score after reset must be unavailable, got %v", err)
	}
	verdict, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 0), Edges: 0.5}, 5000)
	if err != nil {
		t.Fatalf("push after reset: %v", err)
	}
	if verdict.Index != 0 || verdict.Score != 0 {
		t.Fatalf("first frame after reset = %+v, want index 0 and score 0", verdict)
	}
	cut, err := detector.PushFrame(impl.FrameStats{Luma: histPeak(8, 7), Edges: 0.5}, 5500)
	if err != nil {
		t.Fatalf("second frame after reset: %v", err)
	}
	if !cut.Cut {
		t.Fatalf("a hard change after reset must still be reported")
	}
}

func TestHistoryCapEvictsOldScores(t *testing.T) {
	cfg := cutConfig()
	cfg.MaxHistory = 4
	detector := impl.NewSceneCutDetector(cfg)
	for i := 0; i < 10; i++ {
		hist := histPeak(8, 0)
		if i%2 == 1 {
			hist = histPeak(8, 7)
		}
		if _, err := detector.PushFrame(impl.FrameStats{Luma: hist, Edges: 0.5}, int64(i)*100); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if _, err := detector.Score(5); !errors.Is(err, impl.ErrIndexOutOfRange) {
		t.Fatalf("evicted score must be unavailable, got %v", err)
	}
	for _, index := range []int{6, 9} {
		score, err := detector.Score(index)
		if err != nil {
			t.Fatalf("score %d: %v", index, err)
		}
		if math.Abs(score-0.7) > 1e-9 {
			t.Fatalf("score %d = %v, want 0.7", index, score)
		}
	}
	if _, err := detector.Score(-1); !errors.Is(err, impl.ErrIndexOutOfRange) {
		t.Fatalf("negative index must fail, got %v", err)
	}
	if _, err := detector.Score(10); !errors.Is(err, impl.ErrIndexOutOfRange) {
		t.Fatalf("future index must fail, got %v", err)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	cfg := cutConfig()
	detector := impl.NewSceneCutDetector(cfg)
	frames := []struct {
		hist []float64
		pts  int64
	}{
		{histPeak(8, 0), 0},
		{histPeak(8, 7), 500},
		{histPeak(8, 7), 1000},
	}
	for i, frame := range frames {
		if _, err := detector.PushFrame(impl.FrameStats{Luma: frame.hist, Edges: 0.5}, frame.pts); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := detector.SetThresholds(0.6, 0.35); err != nil {
		t.Fatalf("thresholds: %v", err)
	}
	if err := detector.SetMinInterval(250); err != nil {
		t.Fatalf("interval: %v", err)
	}
	data, err := detector.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewSceneCutDetector(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.FrameCount(); got != 3 {
		t.Fatalf("restored frame count = %d, want 3", got)
	}
	high, low := restored.Thresholds()
	if high != 0.6 || low != 0.35 {
		t.Fatalf("restored thresholds = %v/%v, want 0.6/0.35", high, low)
	}
	if got := restored.MinInterval(); got != 250 {
		t.Fatalf("restored interval = %d, want 250", got)
	}
	cuts := restored.Cuts()
	if len(cuts) != 1 || cuts[0].Index != 1 || cuts[0].PTS != 500 {
		t.Fatalf("restored cuts = %+v, want the single cut at index 1", cuts)
	}
	score, err := restored.Score(2)
	if err != nil {
		t.Fatalf("restored score: %v", err)
	}
	if score != 0 {
		t.Fatalf("restored score of the repeated picture = %v, want 0", score)
	}
	if err := restored.Restore([]byte("}{")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.FrameCount(); got != 3 {
		t.Fatalf("failed restore changed the frame count to %d", got)
	}
	verdict, err := restored.PushFrame(impl.FrameStats{Luma: histPeak(8, 7), Edges: 0.5}, 1500)
	if err != nil {
		t.Fatalf("push after restore: %v", err)
	}
	if verdict.Index != 3 {
		t.Fatalf("index after restore = %d, want 3", verdict.Index)
	}
}

func TestConcurrentPushKeepsCount(t *testing.T) {
	detector := impl.NewSceneCutDetector(cutConfig())
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				verdict, err := detector.PushFrame(impl.FrameStats{Luma: flatHist(8), Edges: 0.5}, 0)
				if err != nil {
					errs <- err
					return
				}
				if verdict.Score < 0 || verdict.Score > 1 {
					errs <- errors.New("score outside the unit interval")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent push failed: %v", err)
	}
	if got := detector.FrameCount(); got != 60 {
		t.Fatalf("frame count after 60 concurrent pushes = %d", got)
	}
	if got := len(detector.Cuts()); got != 0 {
		t.Fatalf("identical frames produced %d cuts", got)
	}
}
