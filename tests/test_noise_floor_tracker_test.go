package task

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-02"
)

func noiseConfig() impl.NoiseConfig {
	cfg := impl.DefaultNoiseConfig()
	cfg.Bands = 2
	cfg.FrameSize = 256
	cfg.SampleRate = 48000
	return cfg
}

func constFrame(n int, amp float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = amp
	}
	return out
}

func sineFrame(n int, amp, freq float64, fs int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(fs)))
	}
	return out
}

func bandLevelDB(frame []float32, bands, band int) float64 {
	width := len(frame) / bands
	start := band * width
	sum := 0.0
	for i := start; i < start+width; i++ {
		v := float64(frame[i])
		sum += v * v
	}
	mean := sum / float64(width)
	if mean < 1e-20 {
		mean = 1e-20
	}
	return 10 * math.Log10(mean)
}

func TestDefaultConfigAndFreshState(t *testing.T) {
	cfg := impl.DefaultNoiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	if err := tracker.Validate(); err != nil {
		t.Fatalf("the reference configuration must validate, got %v", err)
	}
	if got := tracker.Bands(); got != 4 {
		t.Fatalf("band count = %d, want 4", got)
	}
	active := tracker.Config()
	if active.FrameSize != 480 || active.SampleRate != 48000 {
		t.Fatalf("active config = %+v, want the stored defaults", active)
	}
	if got := tracker.Frame(); got != 0 {
		t.Fatalf("fresh frame counter = %d, want 0", got)
	}
	decision, err := tracker.PushFrame(constFrame(480, 0.1), time.Time{})
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if decision.Frame != 1 {
		t.Fatalf("first decision frame = %d, want 1", decision.Frame)
	}
	if len(decision.Floors) != 4 || len(decision.Gains) != 4 {
		t.Fatalf("decision vectors = %d floors %d gains, want 4 and 4", len(decision.Floors), len(decision.Gains))
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		edit func(*impl.NoiseConfig)
		want error
	}{
		{"no bands", func(c *impl.NoiseConfig) { c.Bands = 0 }, impl.ErrBadBands},
		{"frame not divisible", func(c *impl.NoiseConfig) { c.Bands = 3; c.FrameSize = 100 }, impl.ErrBadFrameSize},
		{"slow sample rate", func(c *impl.NoiseConfig) { c.SampleRate = 4000 }, impl.ErrBadSampleRate},
		{"empty window", func(c *impl.NoiseConfig) { c.Window = 0 }, impl.ErrBadWindow},
		{"zero delay", func(c *impl.NoiseConfig) { c.DelayMS = 0 }, impl.ErrBadTiming},
		{"negative bias", func(c *impl.NoiseConfig) { c.BiasDB = -1 }, impl.ErrBadBias},
		{"negative creep", func(c *impl.NoiseConfig) { c.CreepDBPerSec = -1 }, impl.ErrBadRate},
		{"floor too deep", func(c *impl.NoiseConfig) { c.GainFloorDB = -80 }, impl.ErrBadGainFloor},
		{"initial floor too high", func(c *impl.NoiseConfig) { c.InitialFloorDB = -5 }, impl.ErrBadInitial},
	}
	for _, tc := range cases {
		cfg := impl.DefaultNoiseConfig()
		tc.edit(&cfg)
		if err := impl.NewNoiseFloorTracker(cfg).Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	bad := impl.DefaultNoiseConfig()
	bad.Window = 0
	if _, err := impl.NewNoiseFloorTracker(bad).PushFrame(constFrame(480, 0.1), time.Time{}); !errors.Is(err, impl.ErrBadConfig) {
		t.Fatalf("push with an invalid configuration must fail with ErrBadConfig, got %v", err)
	}
}

func TestPushFrameRejectsWrongLength(t *testing.T) {
	tracker := impl.NewNoiseFloorTracker(noiseConfig())
	for _, frame := range [][]float32{nil, make([]float32, 200), make([]float32, 512)} {
		if _, err := tracker.PushFrame(frame, time.Time{}); !errors.Is(err, impl.ErrFrameSize) {
			t.Fatalf("frame of %d samples must be rejected, got %v", len(frame), err)
		}
	}
	if got := tracker.Frame(); got != 0 {
		t.Fatalf("rejected frames advanced the counter to %d", got)
	}
	if _, err := tracker.PushFrame(make([]float32, 256), time.Time{}); err != nil {
		t.Fatalf("well sized frame: %v", err)
	}
	if got := tracker.Frame(); got != 1 {
		t.Fatalf("frame counter = %d after one accepted frame, want 1", got)
	}
}

func TestFirstFrameKeepsConfiguredFloor(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	frame := constFrame(cfg.FrameSize, 0.5)
	decision, err := tracker.PushFrame(frame, time.Time{})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	level := bandLevelDB(frame, cfg.Bands, 0)
	floor := decision.Floors[0]
	if floor > cfg.InitialFloorDB+2 || floor < cfg.InitialFloorDB {
		t.Fatalf("floor after the first frame = %v dB, want it to stay near the configured %v dB", floor, cfg.InitialFloorDB)
	}
	if floor > level+cfg.BiasDB-20 {
		t.Fatalf("floor jumped straight to the frame level (%v dB) instead of creeping up", floor)
	}
	stored, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if stored != floor {
		t.Fatalf("decision floor %v differs from the stored floor %v", floor, stored)
	}
	if decision.Gains[0] != 0 || decision.Gains[1] != 0 {
		t.Fatalf("a loud first frame must not be attenuated, got %v", decision.Gains)
	}
}

func TestFrameCounterAndTimestamp(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	base := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	stamps := []time.Time{base, base.Add(10 * time.Millisecond), base.Add(20 * time.Millisecond)}
	for i, at := range stamps {
		decision, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.05), at)
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		if decision.Frame != int64(i+1) {
			t.Fatalf("decision %d reports frame %d, want %d", i, decision.Frame, i+1)
		}
		if !decision.At.Equal(at) {
			t.Fatalf("decision %d timestamp = %v, want %v", i, decision.At, at)
		}
	}
	if got := tracker.Frame(); got != 3 {
		t.Fatalf("frame counter = %d, want 3", got)
	}
}

func TestQuietFramesReachGainFloor(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	var last impl.FrameDecision
	for i := 0; i < 60; i++ {
		decision, err := tracker.PushFrame(constFrame(cfg.FrameSize, 1e-5), time.Time{})
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		last = decision
	}
	gain, err := tracker.Gain(0)
	if err != nil {
		t.Fatalf("read gain: %v", err)
	}
	if math.Abs(gain-cfg.GainFloorDB) > 0.5 {
		t.Fatalf("gain after 60 quiet frames = %v dB, want the floor %v dB", gain, cfg.GainFloorDB)
	}
	if last.Speech {
		t.Fatalf("a frame 90 dB under the floor must not be flagged as speech")
	}
}

func TestLoudFramesKeepUnityGain(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	var decision impl.FrameDecision
	for i := 0; i < 10; i++ {
		var err error
		decision, err = tracker.PushFrame(sineFrame(cfg.FrameSize, 0.5, 440, cfg.SampleRate), time.Time{})
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if decision.Gains[0] != 0 || decision.Gains[1] != 0 {
		t.Fatalf("speech frames must pass untouched, got gains %v", decision.Gains)
	}
	prob, err := tracker.SpeechProbability(0)
	if err != nil {
		t.Fatalf("speech probability: %v", err)
	}
	if prob < 0.9 {
		t.Fatalf("speech probability of a loud tone = %v, want near 1", prob)
	}
	if !decision.Speech {
		t.Fatalf("the frame level must be reported as speech")
	}
}

func TestSpeechProbabilityAndIndexBounds(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	for i := 0; i < 5; i++ {
		if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.2), time.Time{}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	for band := 0; band < cfg.Bands; band++ {
		prob, err := tracker.SpeechProbability(band)
		if err != nil {
			t.Fatalf("probability of band %d: %v", band, err)
		}
		if prob < 0 || prob > 1 {
			t.Fatalf("probability of band %d = %v, want it inside [0,1]", band, prob)
		}
	}
	if _, err := tracker.SpeechProbability(-1); !errors.Is(err, impl.ErrBandIndex) {
		t.Fatalf("negative band index must fail, got %v", err)
	}
	if _, err := tracker.NoiseFloor(cfg.Bands); !errors.Is(err, impl.ErrBandIndex) {
		t.Fatalf("band index past the end must fail, got %v", err)
	}
	if _, err := tracker.Gain(-3); !errors.Is(err, impl.ErrBandIndex) {
		t.Fatalf("negative gain index must fail, got %v", err)
	}
}

func TestSetGainFloorValidates(t *testing.T) {
	tracker := impl.NewNoiseFloorTracker(noiseConfig())
	if err := tracker.SetGainFloor(-61); !errors.Is(err, impl.ErrBadGainFloor) {
		t.Fatalf("floor below the range must fail, got %v", err)
	}
	if err := tracker.SetGainFloor(1); !errors.Is(err, impl.ErrBadGainFloor) {
		t.Fatalf("positive floor must fail, got %v", err)
	}
	if err := tracker.SetGainFloor(math.NaN()); !errors.Is(err, impl.ErrBadGainFloor) {
		t.Fatalf("NaN floor must fail, got %v", err)
	}
	if err := tracker.SetGainFloor(-20); err != nil {
		t.Fatalf("valid floor: %v", err)
	}
	if got := tracker.GainFloor(); got != -20 {
		t.Fatalf("gain floor = %v, want -20", got)
	}
}

func TestGainNeverBelowFloor(t *testing.T) {
	cfg := noiseConfig()
	cfg.GainFloorDB = -6
	tracker := impl.NewNoiseFloorTracker(cfg)
	if err := tracker.SetGainFloor(-6); err != nil {
		t.Fatalf("set floor: %v", err)
	}
	var final float64
	for i := 0; i < 80; i++ {
		decision, err := tracker.PushFrame(constFrame(cfg.FrameSize, 1e-5), time.Time{})
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		for band, gain := range decision.Gains {
			if gain < -6-1e-9 {
				t.Fatalf("frame %d band %d gain = %v, below the configured floor -6", i, band, gain)
			}
		}
		final = decision.Gains[0]
	}
	if math.Abs(final-(-6)) > 0.5 {
		t.Fatalf("settled gain = %v dB, want the floor -6", final)
	}
}

func TestResetRequiresReasonAndRebuilds(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	for i := 0; i < 3; i++ {
		if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.3), time.Time{}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	before, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if err := tracker.Reset("   "); !errors.Is(err, impl.ErrBadReset) {
		t.Fatalf("blank reset reason must fail, got %v", err)
	}
	if got := tracker.Frame(); got != 3 {
		t.Fatalf("rejected reset changed the frame counter to %d", got)
	}
	after, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor after the rejected reset: %v", err)
	}
	if after != before {
		t.Fatalf("rejected reset moved the floor from %v to %v", before, after)
	}
	if err := tracker.Reset("operator cleared the room tone"); err != nil {
		t.Fatalf("valid reset: %v", err)
	}
	if got := tracker.Frame(); got != 0 {
		t.Fatalf("frame counter after reset = %d, want 0", got)
	}
	floor, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor after reset: %v", err)
	}
	if floor != cfg.InitialFloorDB {
		t.Fatalf("floor after reset = %v, want the initial %v", floor, cfg.InitialFloorDB)
	}
	gain, err := tracker.Gain(0)
	if err != nil {
		t.Fatalf("read gain after reset: %v", err)
	}
	if gain != 0 {
		t.Fatalf("gain after reset = %v, want unity", gain)
	}
	if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.1), time.Time{}); err != nil {
		t.Fatalf("push after reset: %v", err)
	}
	if got := tracker.Frame(); got != 1 {
		t.Fatalf("frame counter after the first post reset frame = %d, want 1", got)
	}
}

func TestFloorDescentIsRateLimited(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.5), time.Time{}); err != nil {
		t.Fatalf("loud frame: %v", err)
	}
	first, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	quiet := constFrame(cfg.FrameSize, 1e-4)
	level := bandLevelDB(quiet, cfg.Bands, 0)
	if _, err := tracker.PushFrame(quiet, time.Time{}); err != nil {
		t.Fatalf("quiet frame: %v", err)
	}
	second, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	dt := float64(cfg.FrameSize) / float64(cfg.SampleRate)
	if first-second > cfg.DecayDBPerSec*dt+0.01 {
		t.Fatalf("floor fell %v dB in one frame, the rate limit allows %v", first-second, cfg.DecayDBPerSec*dt)
	}
	if second > first+1e-9 {
		t.Fatalf("a quieter frame must not raise the floor: %v then %v", first, second)
	}
	if second <= level+cfg.BiasDB {
		t.Fatalf("floor %v dB snapped to the new minimum %v dB instead of descending slowly", second, level+cfg.BiasDB)
	}
}

func TestSnapshotRestoreContinuesStream(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	for i := 0; i < 5; i++ {
		if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.2), time.Time{}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	data, err := tracker.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewNoiseFloorTracker(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Frame(); got != 5 {
		t.Fatalf("restored frame counter = %d, want 5", got)
	}
	for band := 0; band < cfg.Bands; band++ {
		wantFloor, err := tracker.NoiseFloor(band)
		if err != nil {
			t.Fatalf("source floor: %v", err)
		}
		gotFloor, err := restored.NoiseFloor(band)
		if err != nil {
			t.Fatalf("restored floor: %v", err)
		}
		if math.Abs(gotFloor-wantFloor) > 1e-9 {
			t.Fatalf("restored floor of band %d = %v, want %v", band, gotFloor, wantFloor)
		}
		wantGain, err := tracker.Gain(band)
		if err != nil {
			t.Fatalf("source gain: %v", err)
		}
		gotGain, err := restored.Gain(band)
		if err != nil {
			t.Fatalf("restored gain: %v", err)
		}
		if math.Abs(gotGain-wantGain) > 1e-9 {
			t.Fatalf("restored gain of band %d = %v, want %v", band, gotGain, wantGain)
		}
	}
	decision, err := restored.PushFrame(constFrame(cfg.FrameSize, 0.2), time.Time{})
	if err != nil {
		t.Fatalf("push after restore: %v", err)
	}
	if decision.Frame != 6 {
		t.Fatalf("frame after restore = %d, want 6", decision.Frame)
	}
}

func TestRestoreRejectsBadPayload(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	for i := 0; i < 4; i++ {
		if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.25), time.Time{}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	floor, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if err := tracker.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	other := impl.DefaultNoiseConfig()
	other.Bands = 3
	other.FrameSize = 480
	foreign, err := impl.NewNoiseFloorTracker(other).Snapshot()
	if err != nil {
		t.Fatalf("foreign snapshot: %v", err)
	}
	if err := tracker.Restore(foreign); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a snapshot with another band layout must fail, got %v", err)
	}
	if got := tracker.Frame(); got != 4 {
		t.Fatalf("failed restore changed the frame counter to %d", got)
	}
	after, err := tracker.NoiseFloor(0)
	if err != nil {
		t.Fatalf("read floor after the failed restores: %v", err)
	}
	if after != floor {
		t.Fatalf("failed restores moved the floor from %v to %v", floor, after)
	}
}

func TestConcurrentPushKeepsCount(t *testing.T) {
	cfg := noiseConfig()
	tracker := impl.NewNoiseFloorTracker(cfg)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if _, err := tracker.PushFrame(constFrame(cfg.FrameSize, 0.02), time.Time{}); err != nil {
					errs <- err
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
	if got := tracker.Frame(); got != 90 {
		t.Fatalf("frame counter after 90 concurrent pushes = %d", got)
	}
	gain, err := tracker.Gain(0)
	if err != nil {
		t.Fatalf("read gain: %v", err)
	}
	if gain > 0 || gain < cfg.GainFloorDB-1e-9 {
		t.Fatalf("gain after the concurrent run = %v, outside [%v, 0]", gain, cfg.GainFloorDB)
	}
}
