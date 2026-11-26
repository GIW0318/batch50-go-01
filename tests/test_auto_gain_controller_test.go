package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-03"
)

func agcConfig() impl.AgcConfig {
	return impl.DefaultAgcConfig()
}

func steadyFrame(n int, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp)
	}
	return out
}

func peakOfFrame(frame []float32) float64 {
	peak := 0.0
	for _, v := range frame {
		if math.Abs(float64(v)) > peak {
			peak = math.Abs(float64(v))
		}
	}
	if peak < 1e-6 {
		peak = 1e-6
	}
	return 20 * math.Log10(peak)
}

func TestDefaultConfigAndFreshState(t *testing.T) {
	cfg := impl.DefaultAgcConfig()
	controller := impl.NewAutoGainController(cfg)
	if err := controller.Validate(); err != nil {
		t.Fatalf("the reference configuration must validate, got %v", err)
	}
	if got := controller.Config().TargetDBFS; got != -12 {
		t.Fatalf("target = %v dBFS, want -12", got)
	}
	if got := controller.GainDB(); got != 0 {
		t.Fatalf("fresh gain = %v dB, want 0", got)
	}
	if got := controller.Frames(); got != 0 {
		t.Fatalf("fresh frame counter = %d, want 0", got)
	}
	if controller.GateState().Gated {
		t.Fatalf("a fresh controller must not report a closed gate")
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		edit func(*impl.AgcConfig)
		want error
	}{
		{"slow rate", func(c *impl.AgcConfig) { c.SampleRate = 4000 }, impl.ErrBadRate},
		{"target too low", func(c *impl.AgcConfig) { c.TargetDBFS = -70 }, impl.ErrBadTarget},
		{"min above max", func(c *impl.AgcConfig) { c.MinGainDB = 5 }, impl.ErrBadGainRange},
		{"max too high", func(c *impl.AgcConfig) { c.MaxGainDB = 50 }, impl.ErrBadGainRange},
		{"gate too high", func(c *impl.AgcConfig) { c.GateThresholdDBFS = -10 }, impl.ErrBadGate},
		{"zero attack", func(c *impl.AgcConfig) { c.AttackMS = 0 }, impl.ErrBadTiming},
		{"negative release", func(c *impl.AgcConfig) { c.ReleaseMS = -1 }, impl.ErrBadTiming},
		{"limiter above zero", func(c *impl.AgcConfig) { c.LimiterDBFS = 3 }, impl.ErrBadLimiter},
	}
	for _, tc := range cases {
		cfg := impl.DefaultAgcConfig()
		tc.edit(&cfg)
		if err := impl.NewAutoGainController(cfg).Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	bad := impl.DefaultAgcConfig()
	bad.AttackMS = 0
	if _, err := impl.NewAutoGainController(bad).Process(steadyFrame(480, 0.1)); !errors.Is(err, impl.ErrBadConfig) {
		t.Fatalf("processing with an invalid configuration must fail with ErrBadConfig, got %v", err)
	}
}

func TestProcessRejectsEmptyFrame(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	if _, err := controller.Process(nil); !errors.Is(err, impl.ErrEmptyFrame) {
		t.Fatalf("nil frame must be rejected, got %v", err)
	}
	if _, err := controller.Process([]float32{}); !errors.Is(err, impl.ErrEmptyFrame) {
		t.Fatalf("empty frame must be rejected, got %v", err)
	}
	if got := controller.Frames(); got != 0 {
		t.Fatalf("rejected frames advanced the counter to %d", got)
	}
}

func TestQuietFramesReachMaxGain(t *testing.T) {
	cfg := agcConfig()
	controller := impl.NewAutoGainController(cfg)
	var state impl.GainState
	for i := 0; i < 200; i++ {
		var err error
		state, err = controller.Process(steadyFrame(480, 0.01))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if math.Abs(state.GainDB-cfg.MaxGainDB) > 1 {
		t.Fatalf("settled gain = %v dB, want the configured maximum %v dB", state.GainDB, cfg.MaxGainDB)
	}
	if state.GainDB > cfg.MaxGainDB+1e-9 {
		t.Fatalf("gain %v exceeded the maximum %v", state.GainDB, cfg.MaxGainDB)
	}
	if math.Abs(state.OutputDBFS-(peakOfFrame(steadyFrame(480, 0.01))+state.GainDB)) > 1e-6 {
		t.Fatalf("output level = %v, want peak plus gain", state.OutputDBFS)
	}
}

func TestLoudFrameCutsGainFast(t *testing.T) {
	cfg := agcConfig()
	controller := impl.NewAutoGainController(cfg)
	state, err := controller.Process(steadyFrame(480, 0.8))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if state.Gated {
		t.Fatalf("a frame at %v dBFS must not be gated", state.PeakDBFS)
	}
	if state.GainDB > -5 {
		t.Fatalf("gain after one loud frame = %v dB, want the fast attack to cut at least 5 dB", state.GainDB)
	}
	if got := controller.Frames(); got != 1 {
		t.Fatalf("frame counter = %d, want 1", got)
	}
	if len(state.ChannelGains) != 1 || state.ChannelGains[0] != state.GainDB {
		t.Fatalf("mono channel gains = %v, want a single entry equal to %v", state.ChannelGains, state.GainDB)
	}
}

func TestAttackFasterThanRelease(t *testing.T) {
	fast := impl.NewAutoGainController(agcConfig())
	down, err := fast.Process(steadyFrame(480, 0.63))
	if err != nil {
		t.Fatalf("downward frame: %v", err)
	}
	slow := impl.NewAutoGainController(agcConfig())
	up, err := slow.Process(steadyFrame(480, 0.01))
	if err != nil {
		t.Fatalf("upward frame: %v", err)
	}
	downStep := -down.GainDB
	upStep := up.GainDB
	if downStep < 5 {
		t.Fatalf("one frame moved the gain down by %v dB, want at least 5", downStep)
	}
	if upStep > 2 {
		t.Fatalf("one frame moved the gain up by %v dB, want a slow release", upStep)
	}
	if downStep < 3*upStep {
		t.Fatalf("attack step %v and release step %v are not asymmetric enough", downStep, upStep)
	}
}

func TestGateFreezesGain(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	if _, err := controller.Process(steadyFrame(480, 0.5)); err != nil {
		t.Fatalf("warm up frame: %v", err)
	}
	before := controller.GainDB()
	if before >= 0 {
		t.Fatalf("warm up must leave a negative gain, got %v", before)
	}
	for i := 0; i < 5; i++ {
		state, err := controller.Process(steadyFrame(480, 0))
		if err != nil {
			t.Fatalf("silent frame %d: %v", i, err)
		}
		if !state.Gated {
			t.Fatalf("silent frame %d was not gated", i)
		}
		if state.GainDB != before {
			t.Fatalf("gain moved to %v while gated, want it frozen at %v", state.GainDB, before)
		}
	}
	if got := controller.GateState(); !got.Gated || got.Frames != 5 {
		t.Fatalf("gate state = %+v, want it closed for 5 frames", got)
	}
}

func TestGateReopensOnSignal(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	for i := 0; i < 3; i++ {
		if _, err := controller.Process(steadyFrame(480, 0)); err != nil {
			t.Fatalf("silent frame %d: %v", i, err)
		}
	}
	if got := controller.GateState(); !got.Gated || got.Frames != 3 {
		t.Fatalf("gate state after silence = %+v, want 3 frozen frames", got)
	}
	state, err := controller.Process(steadyFrame(480, 0.4))
	if err != nil {
		t.Fatalf("loud frame: %v", err)
	}
	if state.Gated {
		t.Fatalf("a frame at %v dBFS must reopen the gate", state.PeakDBFS)
	}
	if got := controller.GateState(); got.Gated || got.Frames != 0 {
		t.Fatalf("gate state after the loud frame = %+v, want an open gate and a zero counter", got)
	}
}

func TestSetTargetValidates(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	if err := controller.SetTargetDBFS(-70); !errors.Is(err, impl.ErrBadTarget) {
		t.Fatalf("target below the range must fail, got %v", err)
	}
	if err := controller.SetTargetDBFS(math.NaN()); !errors.Is(err, impl.ErrBadTarget) {
		t.Fatalf("NaN target must fail, got %v", err)
	}
	if err := controller.SetTargetDBFS(-6); err != nil {
		t.Fatalf("valid target: %v", err)
	}
	if got := controller.Config().TargetDBFS; got != -6 {
		t.Fatalf("target after the change = %v, want -6", got)
	}
}

func TestSetMaxGainClampsCurrentGain(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	for i := 0; i < 15; i++ {
		if _, err := controller.Process(steadyFrame(480, 0.01)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if got := controller.GainDB(); got <= 5 {
		t.Fatalf("gain after 15 quiet frames = %v dB, want it well above 5", got)
	}
	if err := controller.SetMaxGain(3); err != nil {
		t.Fatalf("lower the maximum gain: %v", err)
	}
	if got := controller.GainDB(); got != 3 {
		t.Fatalf("gain after lowering the maximum = %v, want it clamped to 3 immediately", got)
	}
	if err := controller.SetMaxGain(41); !errors.Is(err, impl.ErrBadMaxGain) {
		t.Fatalf("maximum above the range must fail, got %v", err)
	}
	if err := controller.SetMaxGain(-1); !errors.Is(err, impl.ErrBadMaxGain) {
		t.Fatalf("negative maximum must fail, got %v", err)
	}
	if err := controller.SetMaxGain(math.NaN()); !errors.Is(err, impl.ErrBadMaxGain) {
		t.Fatalf("NaN maximum must fail, got %v", err)
	}
	if got := controller.Config().MaxGainDB; got != 3 {
		t.Fatalf("maximum gain after the rejected sets = %v, want 3", got)
	}
}

func TestSetGateThresholdValidates(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	if err := controller.SetGateThreshold(-95); !errors.Is(err, impl.ErrBadGate) {
		t.Fatalf("gate below the range must fail, got %v", err)
	}
	if err := controller.SetGateThreshold(-10); !errors.Is(err, impl.ErrBadGate) {
		t.Fatalf("gate above the range must fail, got %v", err)
	}
	if err := controller.SetGateThreshold(math.NaN()); !errors.Is(err, impl.ErrBadGate) {
		t.Fatalf("NaN gate must fail, got %v", err)
	}
	if err := controller.SetGateThreshold(-60); err != nil {
		t.Fatalf("valid gate: %v", err)
	}
	state, err := controller.Process(steadyFrame(480, 0.0005))
	if err != nil {
		t.Fatalf("quiet frame: %v", err)
	}
	if !state.Gated {
		t.Fatalf("frame at %v dBFS must sit below the new gate -60", state.PeakDBFS)
	}
}

func TestLimiterPreventsClipping(t *testing.T) {
	cfg := agcConfig()
	cfg.AttackMS = 200
	controller := impl.NewAutoGainController(cfg)
	for i := 0; i < 20; i++ {
		if _, err := controller.Process(steadyFrame(480, 0.01)); err != nil {
			t.Fatalf("warm up frame %d: %v", i, err)
		}
	}
	if got := controller.GainDB(); got < 5 {
		t.Fatalf("warm up gain = %v dB, want it high enough to expose the limiter", got)
	}
	frame := steadyFrame(480, 0.95)
	state, err := controller.Process(frame)
	if err != nil {
		t.Fatalf("hot frame: %v", err)
	}
	if !state.Limited {
		t.Fatalf("a frame that would leave the ceiling must be limited")
	}
	if state.OutputDBFS > cfg.LimiterDBFS+1e-6 {
		t.Fatalf("output = %v dBFS, want it held at the ceiling %v", state.OutputDBFS, cfg.LimiterDBFS)
	}
	if math.Abs(state.GainDB-(cfg.LimiterDBFS-state.PeakDBFS)) > 1e-6 {
		t.Fatalf("limiter gain = %v, want the ceiling minus the peak %v", state.GainDB, cfg.LimiterDBFS-state.PeakDBFS)
	}
	if math.Abs(state.PeakDBFS-peakOfFrame(frame)) > 1e-6 {
		t.Fatalf("reported peak = %v, want %v", state.PeakDBFS, peakOfFrame(frame))
	}
}

func TestProcessStereoLinksChannels(t *testing.T) {
	controller := impl.NewAutoGainController(agcConfig())
	left := steadyFrame(480, 0.4)
	right := steadyFrame(480, 0.04)
	state, err := controller.ProcessStereo(left, right)
	if err != nil {
		t.Fatalf("stereo frame: %v", err)
	}
	if len(state.ChannelGains) != 2 {
		t.Fatalf("channel gains = %v, want two entries", state.ChannelGains)
	}
	if state.ChannelGains[0] != state.ChannelGains[1] {
		t.Fatalf("linked channels drifted apart: %v", state.ChannelGains)
	}
	if state.ChannelGains[0] != state.GainDB {
		t.Fatalf("applied gain %v differs from the reported gain %v", state.ChannelGains[0], state.GainDB)
	}
	if _, err := controller.ProcessStereo(steadyFrame(480, 0.1), steadyFrame(240, 0.1)); !errors.Is(err, impl.ErrLengthMismatch) {
		t.Fatalf("mismatched channel lengths must fail, got %v", err)
	}
	if _, err := controller.ProcessStereo(nil, nil); !errors.Is(err, impl.ErrEmptyFrame) {
		t.Fatalf("empty stereo frame must fail, got %v", err)
	}
	if got := controller.Frames(); got != 1 {
		t.Fatalf("rejected stereo frames advanced the counter to %d", got)
	}
}

func TestSnapshotRestoreContinuesEnvelope(t *testing.T) {
	cfg := agcConfig()
	controller := impl.NewAutoGainController(cfg)
	for i := 0; i < 10; i++ {
		if _, err := controller.Process(steadyFrame(480, 0.01)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	before := controller.GainDB()
	if before <= 0 {
		t.Fatalf("warm up gain = %v dB, want a positive gain", before)
	}
	if err := controller.Restore([]byte("not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if got := controller.GainDB(); got != before {
		t.Fatalf("failed restore moved the gain from %v to %v", before, got)
	}
	data, err := controller.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewAutoGainController(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.GainDB(); got != before {
		t.Fatalf("restored gain = %v, want %v", got, before)
	}
	if got := restored.Frames(); got != 10 {
		t.Fatalf("restored frame counter = %d, want 10", got)
	}
	state, err := restored.Process(steadyFrame(480, 0.03))
	if err != nil {
		t.Fatalf("frame after restore: %v", err)
	}
	if state.GainDB <= before || state.GainDB > before+2 {
		t.Fatalf("gain after restore = %v, want one small release step above %v", state.GainDB, before)
	}
}

func TestConcurrentProcessStaysInWindow(t *testing.T) {
	cfg := agcConfig()
	controller := impl.NewAutoGainController(cfg)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				state, err := controller.Process(steadyFrame(480, 0.1))
				if err != nil {
					errs <- err
					return
				}
				if state.GainDB > cfg.MaxGainDB+1e-9 || state.GainDB < cfg.MinGainDB-1e-9 {
					errs <- errors.New("gain escaped the configured window")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent processing failed: %v", err)
	}
	if got := controller.Frames(); got != 60 {
		t.Fatalf("frame counter after 60 concurrent frames = %d", got)
	}
	if got := controller.GainDB(); got > cfg.MaxGainDB+1e-9 || got < cfg.MinGainDB-1e-9 {
		t.Fatalf("final gain %v escaped the window [%v, %v]", got, cfg.MinGainDB, cfg.MaxGainDB)
	}
}
