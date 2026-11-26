package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-07"
)

func batchConfig() impl.BatchConfig {
	return impl.BatchConfig{MaxJobs: 4, MaxInFrames: 8, MaxChannels: 2, ClipCeiling: 32767, DitherBudget: 0}
}

func submitMono(t *testing.T, b *impl.AudioResampleBatch, id string, pcm []int16, rateIn, rateOut int) impl.JobSpec {
	t.Helper()
	spec, err := b.Submit(id, pcm, 1, rateIn, rateOut)
	if err != nil {
		t.Fatalf("submit %s: %v", id, err)
	}
	return spec
}

func TestSubmitValidatesFields(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	if _, err := b.Submit("   ", []int16{1, 2}, 1, 48000, 24000); !errors.Is(err, impl.ErrBlankJob) {
		t.Fatalf("blank id must fail with ErrBlankJob, got %v", err)
	}
	if _, err := b.Submit("j1", []int16{1, 2}, 3, 48000, 24000); !errors.Is(err, impl.ErrBadChannels) {
		t.Fatalf("three channels over a limit of two must fail with ErrBadChannels, got %v", err)
	}
	if _, err := b.Submit("j1", []int16{1, 2}, 2, 12345, 24000); !errors.Is(err, impl.ErrUnsupportedRate) {
		t.Fatalf("unknown input rate must fail with ErrUnsupportedRate, got %v", err)
	}
	if _, err := b.Submit("j1", []int16{1, 2}, 1, 48000, 48000); !errors.Is(err, impl.ErrSameRate) {
		t.Fatalf("identical rates must fail with ErrSameRate, got %v", err)
	}
	if _, err := b.Submit("j1", []int16{1, 2, 3}, 2, 48000, 24000); !errors.Is(err, impl.ErrBadFrames) {
		t.Fatalf("odd sample count in stereo must fail with ErrBadFrames, got %v", err)
	}
	if _, err := b.Submit("j2", make([]int16, 10), 1, 48000, 24000); !errors.Is(err, impl.ErrFrameLimit) {
		t.Fatalf("ten frames over a limit of eight must fail with ErrFrameLimit, got %v", err)
	}
	spec := submitMono(t, b, "j3", []int16{1, 2, 3, 4}, 48000, 24000)
	if spec.ID != "j3" || spec.State != impl.JobPending || spec.InFrames != 4 {
		t.Fatalf("accepted spec = %+v, want j3 pending with four frames", spec)
	}
	if got := b.Pending(); got != 1 {
		t.Fatalf("pending jobs = %d, want 1", got)
	}
	queue := b.Queue()
	if len(queue) != 1 || queue[0] != "j3" {
		t.Fatalf("queue = %v, want [j3]", queue)
	}
}

func TestSubmitRejectsDuplicateAndQueueFull(t *testing.T) {
	b := impl.NewAudioResampleBatch(impl.BatchConfig{MaxJobs: 2, MaxInFrames: 8, MaxChannels: 2, ClipCeiling: 32767, DitherBudget: 0})
	submitMono(t, b, "j1", []int16{1, 2}, 48000, 24000)
	if _, err := b.Submit("j1", []int16{1, 2}, 1, 48000, 24000); !errors.Is(err, impl.ErrJobExists) {
		t.Fatalf("duplicate id must fail with ErrJobExists, got %v", err)
	}
	submitMono(t, b, "j2", []int16{1, 2}, 48000, 24000)
	if _, err := b.Submit("j3", []int16{1, 2}, 1, 48000, 24000); !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("third pending job over a limit of two must fail with ErrQueueFull, got %v", err)
	}
	if got := b.Pending(); got != 2 {
		t.Fatalf("pending jobs = %d, want 2", got)
	}
	queue := b.Queue()
	if len(queue) != 2 || queue[0] != "j1" || queue[1] != "j2" {
		t.Fatalf("queue = %v, want [j1 j2]", queue)
	}
}

func TestPlanComputesOutputFrames(t *testing.T) {
	b := impl.NewAudioResampleBatch(impl.BatchConfig{MaxJobs: 2, MaxInFrames: 5000, MaxChannels: 2, ClipCeiling: 32767, DitherBudget: 0})
	submitMono(t, b, "j1", make([]int16, 4410), 44100, 48000)
	submitMono(t, b, "j2", make([]int16, 1000), 48000, 24000)
	plan, err := b.Plan("j1")
	if err != nil {
		t.Fatalf("plan j1: %v", err)
	}
	if plan.InFrames != 4410 || plan.OutFrames != 4800 {
		t.Fatalf("plan j1 frames = %d -> %d, want 4410 -> 4800", plan.InFrames, plan.OutFrames)
	}
	if plan.RateIn != 44100 || plan.RateOut != 48000 || plan.DitherBudget != 0 {
		t.Fatalf("plan j1 rates = %+v, want 44100 -> 48000 with a zero budget", plan)
	}
	plan, err = b.Plan("j2")
	if err != nil {
		t.Fatalf("plan j2: %v", err)
	}
	if plan.OutFrames != 500 {
		t.Fatalf("plan j2 output frames = %d, want 500", plan.OutFrames)
	}
	if _, err := b.Plan("ghost"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("plan of an unknown job must fail with ErrUnknownJob, got %v", err)
	}
}

func TestRunDownsampleKeepsExactSamples(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	submitMono(t, b, "j1", []int16{1000, -2000, 3000, -4000}, 48000, 24000)
	result, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	if result.OutFrames != 2 {
		t.Fatalf("output frames = %d, want 2", result.OutFrames)
	}
	if len(result.PCM) != 2 || result.PCM[0] != 1000 || result.PCM[1] != 3000 {
		t.Fatalf("pcm = %v, want [1000 3000]", result.PCM)
	}
	if result.Channels != 1 || result.RateIn != 48000 || result.RateOut != 24000 {
		t.Fatalf("result header = %+v, want mono 48000 -> 24000", result)
	}
	if result.ClippedFrames != 0 || result.DitherUsed != 0 || result.Seq <= 0 {
		t.Fatalf("counters = clipped %d dither %d seq %d, want zeros and a positive sequence", result.ClippedFrames, result.DitherUsed, result.Seq)
	}
	if got := b.Pending(); got != 0 {
		t.Fatalf("pending jobs after run = %d, want 0", got)
	}
	job, ok := b.Result("j1")
	if !ok || job.OutFrames != 2 {
		t.Fatalf("stored result = %+v (ok=%v), want the finished job", job, ok)
	}
}

func TestRunUpsampleInterpolatesMidpoints(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	submitMono(t, b, "j1", []int16{10, 20, 30}, 24000, 48000)
	result, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	if result.OutFrames != 6 || len(result.PCM) != 6 {
		t.Fatalf("output = %d frames / %d samples, want six", result.OutFrames, len(result.PCM))
	}
	want := []int16{10, 15, 20, 25, 30, 30}
	for i, sample := range want {
		if result.PCM[i] != sample {
			t.Fatalf("pcm[%d] = %d, want %d (full output %v)", i, result.PCM[i], sample, result.PCM)
		}
	}
}

func TestRunStereoLayoutPreserved(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	if _, err := b.Submit("j1", []int16{10, 10, 20, 20, 30, 30}, 2, 24000, 48000); err != nil {
		t.Fatalf("submit stereo job: %v", err)
	}
	result, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	if result.Channels != 2 || result.OutFrames != 6 {
		t.Fatalf("result header = %+v, want six stereo frames", result)
	}
	if len(result.PCM) != 12 {
		t.Fatalf("interleaved sample count = %d, want 12", len(result.PCM))
	}
	for frame := 0; frame < result.OutFrames; frame++ {
		left := result.PCM[frame*2]
		right := result.PCM[frame*2+1]
		if left != right {
			t.Fatalf("frame %d channels diverged: %d vs %d", frame, left, right)
		}
	}
	if result.PCM[2] != 15 || result.PCM[4] != 20 || result.PCM[10] != 30 {
		t.Fatalf("interleaved pcm = %v, want midpoints and held tail", result.PCM)
	}
}

func TestRunRejectsOutOfOrderAndRerun(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	submitMono(t, b, "j1", []int16{1, 2}, 48000, 24000)
	submitMono(t, b, "j2", []int16{3, 4}, 48000, 24000)
	if _, err := b.Run("j2"); !errors.Is(err, impl.ErrJobOrder) {
		t.Fatalf("running out of order must fail with ErrJobOrder, got %v", err)
	}
	if _, err := b.Run("j1"); err != nil {
		t.Fatalf("run j1: %v", err)
	}
	if _, err := b.Run("j1"); !errors.Is(err, impl.ErrJobDone) {
		t.Fatalf("rerunning a finished job must fail with ErrJobDone, got %v", err)
	}
	queue := b.Queue()
	if len(queue) != 1 || queue[0] != "j2" {
		t.Fatalf("queue after finishing j1 = %v, want [j2]", queue)
	}
	if _, err := b.Run("j2"); err != nil {
		t.Fatalf("run j2 after j1: %v", err)
	}
}

func TestRunUnknownJob(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	if _, err := b.Run("ghost"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("running an unknown job must fail with ErrUnknownJob, got %v", err)
	}
	if _, ok := b.Result("ghost"); ok {
		t.Fatalf("unknown job must not have a result")
	}
}

func TestCancelPendingAndUnknown(t *testing.T) {
	b := impl.NewAudioResampleBatch(impl.BatchConfig{MaxJobs: 2, MaxInFrames: 8, MaxChannels: 2, ClipCeiling: 32767, DitherBudget: 0})
	submitMono(t, b, "j1", []int16{1, 2}, 48000, 24000)
	submitMono(t, b, "j2", []int16{3, 4}, 48000, 24000)
	if err := b.Cancel("j1"); err != nil {
		t.Fatalf("cancel j1: %v", err)
	}
	if queue := b.Queue(); len(queue) != 1 || queue[0] != "j2" {
		t.Fatalf("queue after cancel = %v, want [j2]", queue)
	}
	if _, err := b.Run("j1"); !errors.Is(err, impl.ErrJobCancelled) {
		t.Fatalf("running a cancelled job must fail with ErrJobCancelled, got %v", err)
	}
	if err := b.Cancel("j1"); err != nil {
		t.Fatalf("cancelling twice must stay successful, got %v", err)
	}
	if err := b.Cancel("j2"); err != nil {
		t.Fatalf("cancel j2: %v", err)
	}
	if _, err := b.Run("j2"); !errors.Is(err, impl.ErrJobCancelled) {
		t.Fatalf("running a cancelled head must fail with ErrJobCancelled, got %v", err)
	}
	submitMono(t, b, "j3", []int16{5, 6}, 48000, 24000)
	if _, err := b.Run("j3"); err != nil {
		t.Fatalf("run j3: %v", err)
	}
	if err := b.Cancel("j3"); !errors.Is(err, impl.ErrJobDone) {
		t.Fatalf("cancelling a finished job must fail with ErrJobDone, got %v", err)
	}
	if err := b.Cancel("ghost"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("cancelling an unknown job must fail with ErrUnknownJob, got %v", err)
	}
}

func TestClippingProtectionCountsFrames(t *testing.T) {
	b := impl.NewAudioResampleBatch(impl.BatchConfig{MaxJobs: 2, MaxInFrames: 8, MaxChannels: 2, ClipCeiling: 20000, DitherBudget: 0})
	submitMono(t, b, "j1", []int16{30000, -30000, -25000, 25000}, 48000, 24000)
	result, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	if result.OutFrames != 2 || len(result.PCM) != 2 {
		t.Fatalf("output = %d frames / %d samples, want two", result.OutFrames, len(result.PCM))
	}
	if result.ClippedFrames != 2 {
		t.Fatalf("clipped frames = %d, want every frame counted once", result.ClippedFrames)
	}
	for i, sample := range result.PCM {
		if sample > 20000 || sample < -20000 {
			t.Fatalf("pcm[%d] = %d, want the sample guarded at the ceiling", i, sample)
		}
	}
	if result.PCM[0] != 20000 || result.PCM[1] != -20000 {
		t.Fatalf("pcm = %v, want the peak pinned to the ceiling", result.PCM)
	}
}

func TestDitherBudgetZeroIsPureRounding(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	submitMono(t, b, "j1", []int16{0, 1000, 2000}, 44100, 48000)
	result, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	want := []int16{0, 919, 1838, 2000}
	if result.OutFrames != len(want) || len(result.PCM) != len(want) {
		t.Fatalf("output = %d frames / %d samples, want %d", result.OutFrames, len(result.PCM), len(want))
	}
	for i, sample := range want {
		if result.PCM[i] != sample {
			t.Fatalf("pcm[%d] = %d, want %d (full output %v)", i, result.PCM[i], sample, result.PCM)
		}
	}
	if result.DitherUsed != 0 {
		t.Fatalf("dither used without a budget = %d, want 0", result.DitherUsed)
	}
}

func TestDitherBudgetAllocatedPerFrame(t *testing.T) {
	b := impl.NewAudioResampleBatch(impl.BatchConfig{MaxJobs: 2, MaxInFrames: 8, MaxChannels: 2, ClipCeiling: 32767, DitherBudget: 3})
	submitMono(t, b, "j1", []int16{100, 200, 300, 400, 500, 600, 700, 800}, 48000, 24000)
	result, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	if result.OutFrames != 4 {
		t.Fatalf("output frames = %d, want 4", result.OutFrames)
	}
	if result.DitherUsed != 3 {
		t.Fatalf("dither used = %d, want the three units of the budget", result.DitherUsed)
	}
	if result.ClippedFrames != 0 {
		t.Fatalf("quiet input produced %d clipped frames, want 0", result.ClippedFrames)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	submitMono(t, b, "j1", []int16{1000, -2000, 3000, -4000}, 48000, 24000)
	first, err := b.Run("j1")
	if err != nil {
		t.Fatalf("run j1: %v", err)
	}
	submitMono(t, b, "j2", []int16{10, 20, 30}, 24000, 48000)
	snap, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	submitMono(t, b, "j3", []int16{1, 2}, 48000, 24000)
	if err := b.Cancel("j2"); err != nil {
		t.Fatalf("cancel j2: %v", err)
	}
	if err := b.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(b.Jobs()); got != 2 {
		t.Fatalf("jobs after restore = %d, want 2", got)
	}
	if got := b.Pending(); got != 1 {
		t.Fatalf("pending after restore = %d, want 1", got)
	}
	if queue := b.Queue(); len(queue) != 1 || queue[0] != "j2" {
		t.Fatalf("queue after restore = %v, want [j2]", queue)
	}
	stored, ok := b.Result("j1")
	if !ok || len(stored.PCM) != len(first.PCM) {
		t.Fatalf("result j1 after restore = %+v (ok=%v), want the two sample output", stored, ok)
	}
	for i, sample := range first.PCM {
		if stored.PCM[i] != sample {
			t.Fatalf("restored pcm[%d] = %d, want %d", i, stored.PCM[i], sample)
		}
	}
	if _, err := b.Run("j2"); err != nil {
		t.Fatalf("run j2 after restore: %v", err)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	submitMono(t, b, "j1", []int16{1, 2}, 48000, 24000)
	if err := b.Restore([]byte("{\"jobs\": [ {\"id\":")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("truncated payload must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(b.Jobs()); got != 1 {
		t.Fatalf("failed restore changed the live jobs to %d", got)
	}
	if got := b.Pending(); got != 1 {
		t.Fatalf("failed restore changed the queue to %d pending", got)
	}
	if _, err := b.Run("j1"); err != nil {
		t.Fatalf("live job unusable after a failed restore: %v", err)
	}
}

func TestConcurrentSubmitSameID(t *testing.T) {
	b := impl.NewAudioResampleBatch(batchConfig())
	const workers = 24
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Submit("dup", []int16{1, 2}, 1, 48000, 24000)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	accepted := 0
	duplicated := 0
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, impl.ErrJobExists):
			duplicated++
		default:
			t.Fatalf("concurrent submit returned %v", err)
		}
	}
	if accepted != 1 || duplicated != workers-1 {
		t.Fatalf("concurrent submits accepted %d and rejected %d, want 1 and %d", accepted, duplicated, workers-1)
	}
	if got := len(b.Jobs()); got != 1 {
		t.Fatalf("jobs after the race = %d, want 1", got)
	}
	if queue := b.Queue(); len(queue) != 1 || queue[0] != "dup" {
		t.Fatalf("queue after the race = %v, want [dup]", queue)
	}
}
