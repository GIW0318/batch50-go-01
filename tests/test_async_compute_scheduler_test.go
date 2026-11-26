package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-06"
)

func computeJob(name string, cost int) impl.JobSpec {
	return impl.JobSpec{Name: name, Cost: cost, Writes: []string{name + ".out"}}
}

func graphicsJob(name string) impl.JobSpec {
	return impl.JobSpec{Name: name, Cost: 1, Reads: []string{"depth"}, Writes: []string{"color"}}
}

func openScheduler(t *testing.T, frame int, limits impl.Limits) *impl.AsyncComputeScheduler {
	t.Helper()
	s := impl.NewAsyncComputeScheduler(limits)
	if err := s.BeginFrame(frame); err != nil {
		t.Fatalf("begin frame %d: %v", frame, err)
	}
	return s
}

func TestSubmitRequiresOpenFrame(t *testing.T) {
	s := impl.NewAsyncComputeScheduler(impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	if _, err := s.Submit(impl.QueueGraphics, graphicsJob("gbuffer")); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("submit before BeginFrame must fail with ErrNoFrame, got %v", err)
	}
	if _, err := s.EndFrame(); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("EndFrame before BeginFrame must fail with ErrNoFrame, got %v", err)
	}
}

func TestSubmitRejectsBadQueueAndBadJob(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	if _, err := s.Submit(impl.QueueKind(0), graphicsJob("a")); !errors.Is(err, impl.ErrBadQueue) {
		t.Fatalf("zero queue must fail with ErrBadQueue, got %v", err)
	}
	if _, err := s.Submit(impl.QueueKind(7), graphicsJob("b")); !errors.Is(err, impl.ErrBadQueue) {
		t.Fatalf("unknown queue must fail with ErrBadQueue, got %v", err)
	}
	cases := []impl.JobSpec{
		{Name: "   ", Cost: 3},
		{Name: "c", Cost: 0},
		{Name: "d", Cost: -4},
		{Name: "e", Cost: 4, Writes: []string{""}},
		{Name: "f", Cost: 4, Reads: []string{"x", "x"}},
	}
	for i, spec := range cases {
		if _, err := s.Submit(impl.QueueCompute, spec); !errors.Is(err, impl.ErrBadJob) {
			t.Fatalf("case %d (%+v) must fail with ErrBadJob, got %v", i, spec, err)
		}
	}
	if got := len(s.Pending(impl.QueueGraphics)) + len(s.Pending(impl.QueueCompute)); got != 0 {
		t.Fatalf("rejected submissions must not enter the job table, got %d pending", got)
	}
}

func TestSubmitAllocatesMonotonicHandles(t *testing.T) {
	s := openScheduler(t, 2, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	h1, err := s.Submit(impl.QueueGraphics, graphicsJob("first"))
	if err != nil {
		t.Fatalf("submit first: %v", err)
	}
	h2, err := s.Submit(impl.QueueGraphics, graphicsJob("second"))
	if err != nil {
		t.Fatalf("submit second: %v", err)
	}
	if h1 <= 0 || h2 <= h1 {
		t.Fatalf("handles must be positive and increasing, got %d then %d", h1, h2)
	}
	st, ok := s.Job(h2)
	if !ok {
		t.Fatalf("submitted job must be readable")
	}
	if st.Name != "second" || st.Queue != impl.QueueGraphics || st.Frame != 2 {
		t.Fatalf("job record = %+v, want name second on the graphics queue in frame 2", st)
	}
	if st.Status != impl.StatusPending {
		t.Fatalf("fresh job status = %q, want %q", st.Status, impl.StatusPending)
	}
	if len(st.Writes) != 1 || st.Writes[0] != "color" {
		t.Fatalf("job writes = %v, want the declared write set", st.Writes)
	}
	pending := s.Pending(impl.QueueGraphics)
	if len(pending) != 2 || pending[0] != h1 || pending[1] != h2 {
		t.Fatalf("pending graphics = %v, want [%d %d] in submission order", pending, h1, h2)
	}
	if _, ok := s.Job(impl.JobHandle(4242)); ok {
		t.Fatalf("unknown handle must not resolve")
	}
}

func TestSubmitEnforcesJobLimitAndDefaults(t *testing.T) {
	zero := impl.NewAsyncComputeScheduler(impl.Limits{})
	if got := zero.Limits(); got.MaxJobs != impl.DefaultMaxJobs || got.ComputeBudget != impl.DefaultComputeBudget {
		t.Fatalf("zero limits normalised to %+v, want %d/%d", got, impl.DefaultMaxJobs, impl.DefaultComputeBudget)
	}
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 2, ComputeBudget: 100})
	if _, err := s.Submit(impl.QueueCompute, computeJob("a", 5)); err != nil {
		t.Fatalf("submit a: %v", err)
	}
	if _, err := s.Submit(impl.QueueCompute, computeJob("b", 5)); err != nil {
		t.Fatalf("submit b: %v", err)
	}
	if _, err := s.Submit(impl.QueueCompute, computeJob("c", 5)); !errors.Is(err, impl.ErrJobLimit) {
		t.Fatalf("third job over MaxJobs=2 must fail with ErrJobLimit, got %v", err)
	}
}

func TestBeginFrameRequiresIncreasingNumbers(t *testing.T) {
	s := impl.NewAsyncComputeScheduler(impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	if err := s.BeginFrame(5); err != nil {
		t.Fatalf("begin frame 5: %v", err)
	}
	if err := s.BeginFrame(5); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("repeating frame 5 must fail with ErrBadFrame, got %v", err)
	}
	if err := s.BeginFrame(4); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("going back to frame 4 must fail with ErrBadFrame, got %v", err)
	}
	if got := s.Frame(); got != 5 {
		t.Fatalf("frame after rejected begin = %d, want 5", got)
	}
}

func TestEndFrameRunsGraphicsInSubmissionOrder(t *testing.T) {
	s := openScheduler(t, 7, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	var handles []impl.JobHandle
	for _, name := range []string{"shadow", "gbuffer", "lighting"} {
		h, err := s.Submit(impl.QueueGraphics, graphicsJob(name))
		if err != nil {
			t.Fatalf("submit %s: %v", name, err)
		}
		handles = append(handles, h)
	}
	report, err := s.EndFrame()
	if err != nil {
		t.Fatalf("end frame: %v", err)
	}
	if report.Frame != 7 {
		t.Fatalf("report frame = %d, want 7", report.Frame)
	}
	if len(report.Graphics) != 3 {
		t.Fatalf("graphics timeline = %v, want three jobs", report.Graphics)
	}
	for i, want := range handles {
		if report.Graphics[i] != want {
			t.Fatalf("graphics timeline = %v, want submission order %v", report.Graphics, handles)
		}
	}
	for _, h := range handles {
		st, ok := s.Job(h)
		if !ok || st.Status != impl.StatusDone {
			t.Fatalf("job %d status=%q ok=%v, want done", h, st.Status, ok)
		}
	}
	if len(report.Compute) != 0 {
		t.Fatalf("compute timeline = %v, want empty", report.Compute)
	}
}

func TestEndFrameDefersComputeOverBudget(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	first, err := s.Submit(impl.QueueCompute, computeJob("probe", 60))
	if err != nil {
		t.Fatalf("submit probe: %v", err)
	}
	second, err := s.Submit(impl.QueueCompute, computeJob("fog", 60))
	if err != nil {
		t.Fatalf("submit fog: %v", err)
	}
	third, err := s.Submit(impl.QueueCompute, computeJob("bloom", 30))
	if err != nil {
		t.Fatalf("submit bloom: %v", err)
	}
	report, err := s.EndFrame()
	if err != nil {
		t.Fatalf("end frame: %v", err)
	}
	if len(report.Compute) != 2 || report.Compute[0] != first || report.Compute[1] != third {
		t.Fatalf("compute timeline = %v, want [%d %d] skipping the job that does not fit", report.Compute, first, third)
	}
	if len(report.Deferred) != 1 || report.Deferred[0] != second {
		t.Fatalf("deferred = %v, want only the fog job %d", report.Deferred, second)
	}
	if report.ComputeUse != 90 {
		t.Fatalf("compute use = %d, want 60+30=90", report.ComputeUse)
	}
	if st, _ := s.Job(second); st.Status != impl.StatusDeferred {
		t.Fatalf("over budget job status = %q, want %q", st.Status, impl.StatusDeferred)
	}
	if st, _ := s.Job(third); st.Status != impl.StatusDone {
		t.Fatalf("late submitted job that fits must run, got status %q", st.Status)
	}
}

func TestBarrierRejectsSameQueueAndBadWaits(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	g1, err := s.Submit(impl.QueueGraphics, graphicsJob("g1"))
	if err != nil {
		t.Fatalf("submit g1: %v", err)
	}
	g2, err := s.Submit(impl.QueueGraphics, graphicsJob("g2"))
	if err != nil {
		t.Fatalf("submit g2: %v", err)
	}
	if err := s.Barrier(g2, []impl.JobHandle{g1}); !errors.Is(err, impl.ErrBarrierSameQueue) {
		t.Fatalf("same queue barrier must fail with ErrBarrierSameQueue, got %v", err)
	}
	if err := s.Barrier(g2, nil); !errors.Is(err, impl.ErrBadBarrier) {
		t.Fatalf("empty wait list must fail with ErrBadBarrier, got %v", err)
	}
	if err := s.Barrier(g2, []impl.JobHandle{g2}); !errors.Is(err, impl.ErrBadBarrier) {
		t.Fatalf("self wait must fail with ErrBadBarrier, got %v", err)
	}
	if err := s.Barrier(g2, []impl.JobHandle{impl.JobHandle(9001)}); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("unknown wait handle must fail with ErrUnknownJob, got %v", err)
	}
	if err := s.Barrier(impl.JobHandle(9002), []impl.JobHandle{g1}); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("unknown barrier owner must fail with ErrUnknownJob, got %v", err)
	}
}

func TestBarrierMergesDuplicateWaits(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	g1, err := s.Submit(impl.QueueGraphics, graphicsJob("g1"))
	if err != nil {
		t.Fatalf("submit g1: %v", err)
	}
	g2, err := s.Submit(impl.QueueGraphics, graphicsJob("g2"))
	if err != nil {
		t.Fatalf("submit g2: %v", err)
	}
	c1, err := s.Submit(impl.QueueCompute, computeJob("c1", 10))
	if err != nil {
		t.Fatalf("submit c1: %v", err)
	}
	if err := s.Barrier(c1, []impl.JobHandle{g2, g1, g2}); err != nil {
		t.Fatalf("declare barrier: %v", err)
	}
	waits := s.Depends(c1)
	if len(waits) != 2 || waits[0] != g2 || waits[1] != g1 {
		t.Fatalf("barrier waits = %v, want [%d %d] with the duplicate removed", waits, g2, g1)
	}
	if got := s.Depends(g1); len(got) != 0 {
		t.Fatalf("a job without barriers must report no waits, got %v", got)
	}
}

func TestEndFrameSerialisesOnCrossQueueBarrier(t *testing.T) {
	s := openScheduler(t, 3, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	g, err := s.Submit(impl.QueueGraphics, graphicsJob("depth"))
	if err != nil {
		t.Fatalf("submit graphics: %v", err)
	}
	c, err := s.Submit(impl.QueueCompute, computeJob("ssao", 20))
	if err != nil {
		t.Fatalf("submit compute: %v", err)
	}
	if err := s.Barrier(c, []impl.JobHandle{g}); err != nil {
		t.Fatalf("declare cross queue barrier: %v", err)
	}
	report, err := s.EndFrame()
	if err != nil {
		t.Fatalf("end frame: %v", err)
	}
	if !report.Serialized {
		t.Fatalf("a frame with a cross queue barrier must report Serialized=true")
	}
	plain := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	if _, err := plain.Submit(impl.QueueGraphics, graphicsJob("depth")); err != nil {
		t.Fatalf("submit plain graphics: %v", err)
	}
	if _, err := plain.Submit(impl.QueueCompute, computeJob("ssao", 20)); err != nil {
		t.Fatalf("submit plain compute: %v", err)
	}
	overlapped, err := plain.EndFrame()
	if err != nil {
		t.Fatalf("end plain frame: %v", err)
	}
	if overlapped.Serialized {
		t.Fatalf("a frame without cross queue barriers must report Serialized=false")
	}
}

func TestCancelDropsPendingJob(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	h, err := s.Submit(impl.QueueCompute, computeJob("fog", 10))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := s.Cancel(h); err != nil {
		t.Fatalf("cancel pending job: %v", err)
	}
	st, ok := s.Job(h)
	if !ok || st.Status != impl.StatusCancelled {
		t.Fatalf("cancelled job status=%q ok=%v, want %q", st.Status, ok, impl.StatusCancelled)
	}
	if got := s.Pending(impl.QueueCompute); len(got) != 0 {
		t.Fatalf("cancelled job must leave the pending list, got %v", got)
	}
	if err := s.Cancel(h); err != nil {
		t.Fatalf("cancelling twice must stay quiet, got %v", err)
	}
	if err := s.Cancel(impl.JobHandle(777)); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("cancelling an unknown handle must fail with ErrUnknownJob, got %v", err)
	}
}

func TestCancelFinishedJobFails(t *testing.T) {
	s := openScheduler(t, 2, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	h, err := s.Submit(impl.QueueGraphics, graphicsJob("ui"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.EndFrame(); err != nil {
		t.Fatalf("end frame: %v", err)
	}
	if err := s.Cancel(h); !errors.Is(err, impl.ErrJobDone) {
		t.Fatalf("cancelling a finished job must fail with ErrJobDone, got %v", err)
	}
	if st, _ := s.Job(h); st.Status != impl.StatusDone {
		t.Fatalf("failed cancel must leave the job done, got %q", st.Status)
	}
}

func TestSnapshotRestoreKeepsFrameAndJobs(t *testing.T) {
	s := openScheduler(t, 3, impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	h, err := s.Submit(impl.QueueGraphics, graphicsJob("hud"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.EndFrame(); err != nil {
		t.Fatalf("end frame: %v", err)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewAsyncComputeScheduler(impl.Limits{MaxJobs: 8, ComputeBudget: 100})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Frame(); got != 3 {
		t.Fatalf("restored frame = %d, want 3", got)
	}
	st, ok := restored.Job(h)
	if !ok || st.Status != impl.StatusDone || st.Name != "hud" {
		t.Fatalf("restored job = %+v ok=%v, want the finished hud job", st, ok)
	}
	if err := restored.BeginFrame(3); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("frame 3 must already be spent after restore, got %v", err)
	}
	if err := restored.BeginFrame(4); err != nil {
		t.Fatalf("frame 4 must be usable after restore: %v", err)
	}
	next, err := restored.Submit(impl.QueueGraphics, graphicsJob("next"))
	if err != nil {
		t.Fatalf("submit after restore: %v", err)
	}
	if next <= h {
		t.Fatalf("handle watermark must continue past %d, got %d", h, next)
	}
	if err := restored.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Frame(); got != 4 {
		t.Fatalf("rejected restore must not touch the live frame, got %d", got)
	}
}

func TestCancelDeferredJobDropsItFromNextFrame(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 8, ComputeBudget: 50})
	running, err := s.Submit(impl.QueueCompute, computeJob("probe", 50))
	if err != nil {
		t.Fatalf("submit probe: %v", err)
	}
	deferred, err := s.Submit(impl.QueueCompute, computeJob("fog", 50))
	if err != nil {
		t.Fatalf("submit fog: %v", err)
	}
	first, err := s.EndFrame()
	if err != nil {
		t.Fatalf("end first frame: %v", err)
	}
	if len(first.Deferred) != 1 || first.Deferred[0] != deferred {
		t.Fatalf("deferred = %v, want [%d]", first.Deferred, deferred)
	}
	if err := s.Cancel(deferred); err != nil {
		t.Fatalf("cancel deferred job: %v", err)
	}
	if err := s.BeginFrame(2); err != nil {
		t.Fatalf("begin second frame: %v", err)
	}
	second, err := s.EndFrame()
	if err != nil {
		t.Fatalf("end second frame: %v", err)
	}
	if len(second.Compute) != 0 || len(second.Deferred) != 0 {
		t.Fatalf("cancelled carry over must not run, got compute=%v deferred=%v", second.Compute, second.Deferred)
	}
	if st, _ := s.Job(deferred); st.Status != impl.StatusCancelled {
		t.Fatalf("cancelled deferred job status = %q, want %q", st.Status, impl.StatusCancelled)
	}
	if st, _ := s.Job(running); st.Status != impl.StatusDone {
		t.Fatalf("job that ran in frame 1 status = %q, want done", st.Status)
	}
}

func TestConcurrentSubmitRespectsJobLimit(t *testing.T) {
	s := openScheduler(t, 1, impl.Limits{MaxJobs: 4, ComputeBudget: 100})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.Submit(impl.QueueCompute, computeJob("job", 1))
		}(i)
	}
	wg.Wait()
	if got := len(s.Pending(impl.QueueCompute)); got != 4 {
		t.Fatalf("pending jobs after the race = %d, want the cap 4", got)
	}
	if _, err := s.Submit(impl.QueueCompute, computeJob("job", 1)); !errors.Is(err, impl.ErrJobLimit) {
		t.Fatalf("the cap must stay enforced after the race, got %v", err)
	}
}
