package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-09"
)

func plainJob(project string) impl.BounceJob {
	return impl.BounceJob{
		Project:    project,
		SampleRate: 48000,
		BlockSize:  480,
		Blocks:     10,
		TrackRings: []int{10, 10},
	}
}

func startJob(t *testing.T, r *impl.OfflineBounceRenderer, job impl.BounceJob) impl.JobID {
	t.Helper()
	id, err := r.Start(job)
	if err != nil {
		t.Fatalf("start job %s: %v", job.Project, err)
	}
	if id == 0 {
		t.Fatalf("start returned the zero job id")
	}
	return id
}

func TestStartValidatesJob(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	bad := []impl.BounceJob{
		{Project: "   ", SampleRate: 48000, BlockSize: 480, Blocks: 10, TrackRings: []int{1}},
		{Project: "mix", SampleRate: 0, BlockSize: 480, Blocks: 10, TrackRings: []int{1}},
		{Project: "mix", SampleRate: 48000, BlockSize: 0, Blocks: 10, TrackRings: []int{1}},
		{Project: "mix", SampleRate: 48000, BlockSize: 480, Blocks: 0, TrackRings: []int{1}},
		{Project: "mix", SampleRate: 48000, BlockSize: 480, Blocks: 10, TrackRings: nil},
		{Project: "mix", SampleRate: 48000, BlockSize: 480, Blocks: 10, TrackRings: []int{-1}},
	}
	for i, job := range bad {
		if _, err := r.Start(job); !errors.Is(err, impl.ErrBadJob) {
			t.Fatalf("job %d (%+v) must fail with ErrBadJob, got %v", i, job, err)
		}
	}
	if got := len(r.Jobs()); got != 0 {
		t.Fatalf("rejected jobs must not be registered, got %d jobs", got)
	}
}

func TestStartAssignsMonotonicIDs(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	first := startJob(t, r, plainJob("mix-a"))
	second := startJob(t, r, plainJob("mix-b"))
	if first != 1 {
		t.Fatalf("first job id = %d, want 1", first)
	}
	if second <= first {
		t.Fatalf("second job id = %d, must continue past %d", second, first)
	}
	jobs := r.Jobs()
	if len(jobs) != 2 || jobs[0] != first || jobs[1] != second {
		t.Fatalf("jobs = %v, want [%d %d] in ascending order", jobs, first, second)
	}
}

func TestStartEnforcesLimitsAndProjectUniqueness(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{MaxJobs: 2})
	startJob(t, r, plainJob("mix-a"))
	startJob(t, r, plainJob("mix-b"))
	if _, err := r.Start(plainJob("mix-a")); !errors.Is(err, impl.ErrJobActive) {
		t.Fatalf("resubmitting an active project must fail with ErrJobActive, got %v", err)
	}
	if _, err := r.Start(plainJob("mix-c")); !errors.Is(err, impl.ErrJobLimit) {
		t.Fatalf("a third concurrent job must fail with ErrJobLimit, got %v", err)
	}
	if got := len(r.Jobs()); got != 2 {
		t.Fatalf("job count = %d, want the configured cap 2", got)
	}
}

func TestAdvanceReportsProgress(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := r.SetTail(20 * time.Millisecond); err != nil {
		t.Fatalf("set tail: %v", err)
	}
	id := startJob(t, r, plainJob("mix-a"))
	p, err := r.Advance(id, 5)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if p.Job != id || p.Project != "mix-a" {
		t.Fatalf("progress identity = %+v, want job %d of mix-a", p, id)
	}
	if p.Content != 10 || p.Tail != 2 {
		t.Fatalf("content=%d tail=%d, want 10 and 2", p.Content, p.Tail)
	}
	if p.Done != 5 || p.Samples != 2400 {
		t.Fatalf("done=%d samples=%d, want 5 blocks and 2400 samples", p.Done, p.Samples)
	}
	if p.State != impl.JobRunning || p.Silent {
		t.Fatalf("state=%q silent=%v, want a running job without silence", p.State, p.Silent)
	}
	st, err := r.Status(id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st != p {
		t.Fatalf("status = %+v, want the same progress %+v", st, p)
	}
}

func TestAdvanceClampsAtTotalBlocks(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := r.SetTail(20 * time.Millisecond); err != nil {
		t.Fatalf("set tail: %v", err)
	}
	id := startJob(t, r, plainJob("mix-a"))
	p, err := r.Advance(id, 64)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if p.Done != 12 || p.Content != 10 || p.Tail != 2 {
		t.Fatalf("progress = %+v, want 12 of 10+2 blocks", p)
	}
	if p.State != impl.JobDone || !p.Silent {
		t.Fatalf("state=%q silent=%v, want a finished silent job", p.State, p.Silent)
	}
	if p.Samples != 5760 {
		t.Fatalf("samples = %d, want 12*480 = 5760", p.Samples)
	}
	if _, err := r.Advance(id, 1); !errors.Is(err, impl.ErrJobDone) {
		t.Fatalf("advancing a finished job must fail with ErrJobDone, got %v", err)
	}
}

func TestAdvanceRejectsOutOfRangeBlocks(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := r.SetTail(20 * time.Millisecond); err != nil {
		t.Fatalf("set tail: %v", err)
	}
	id := startJob(t, r, plainJob("mix-a"))
	for _, blocks := range []int{0, -1, 65} {
		if _, err := r.Advance(id, blocks); !errors.Is(err, impl.ErrBadBlocks) {
			t.Fatalf("advance %d must fail with ErrBadBlocks, got %v", blocks, err)
		}
	}
	st, err := r.Status(id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Done != 0 {
		t.Fatalf("done = %d, want 0 after rejected advances", st.Done)
	}
}

func TestZeroTailFinishesWithContent(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := r.SetTail(0); err != nil {
		t.Fatalf("set zero tail: %v", err)
	}
	job := plainJob("dry-mix")
	job.TrackRings = []int{10}
	id := startJob(t, r, job)
	st, err := r.Status(id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Tail != 0 || st.Content != 10 {
		t.Fatalf("content=%d tail=%d, want 10 content blocks and no tail", st.Content, st.Tail)
	}
	p, err := r.Advance(id, 64)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if p.Done != 10 || p.State != impl.JobDone || !p.Silent {
		t.Fatalf("progress = %+v, want 10 finished silent blocks", p)
	}
}

func TestSetTailValidation(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if got := r.Tail(); got != 2*time.Second {
		t.Fatalf("default tail = %v, want 2s", got)
	}
	if err := r.SetTail(-time.Millisecond); !errors.Is(err, impl.ErrBadTail) {
		t.Fatalf("negative tail must fail with ErrBadTail, got %v", err)
	}
	if err := r.SetTail(11 * time.Second); !errors.Is(err, impl.ErrBadTail) {
		t.Fatalf("tail beyond the ten second cap must fail with ErrBadTail, got %v", err)
	}
	if got := r.Tail(); got != 2*time.Second {
		t.Fatalf("rejected tails must not change the active one, got %v", got)
	}
	if err := r.SetTail(30 * time.Millisecond); err != nil {
		t.Fatalf("set valid tail: %v", err)
	}
	if got := r.Tail(); got != 30*time.Millisecond {
		t.Fatalf("tail = %v, want 30ms", got)
	}
}

func TestSetLatencyValidation(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if got := r.LatencyComp(); got != 0 {
		t.Fatalf("default latency compensation = %v, want 0", got)
	}
	if err := r.SetLatencyComp(-1); !errors.Is(err, impl.ErrBadLatency) {
		t.Fatalf("negative compensation must fail with ErrBadLatency, got %v", err)
	}
	if err := r.SetLatencyComp(501); !errors.Is(err, impl.ErrBadLatency) {
		t.Fatalf("compensation beyond 500ms must fail with ErrBadLatency, got %v", err)
	}
	if got := r.LatencyComp(); got != 0 {
		t.Fatalf("rejected compensation must not change the active one, got %v", got)
	}
	if err := r.SetLatencyComp(12.5); err != nil {
		t.Fatalf("set valid compensation: %v", err)
	}
	if got := r.LatencyComp(); got != 12.5 {
		t.Fatalf("latency compensation = %v, want 12.5", got)
	}
}

func TestStatusRejectsUnknownJob(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	for _, id := range []impl.JobID{0, -1, 99} {
		if _, err := r.Status(id); !errors.Is(err, impl.ErrUnknownJob) {
			t.Fatalf("status of job %d must fail with ErrUnknownJob, got %v", id, err)
		}
	}
}

func TestCancelKeepsFinishedBlocks(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := r.SetTail(20 * time.Millisecond); err != nil {
		t.Fatalf("set tail: %v", err)
	}
	id := startJob(t, r, plainJob("mix-a"))
	if _, err := r.Advance(id, 3); err != nil {
		t.Fatalf("advance before cancel: %v", err)
	}
	if err := r.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, err := r.Status(id)
	if err != nil {
		t.Fatalf("status after cancel: %v", err)
	}
	if st.State != impl.JobCancelled || st.Done != 3 {
		t.Fatalf("cancelled progress = %+v, want cancelled at 3 blocks", st)
	}
	if _, err := r.Advance(id, 1); !errors.Is(err, impl.ErrJobDone) {
		t.Fatalf("advancing a cancelled job must fail with ErrJobDone, got %v", err)
	}
	if err := r.Cancel(id); !errors.Is(err, impl.ErrJobDone) {
		t.Fatalf("cancelling twice must fail with ErrJobDone, got %v", err)
	}
}

func TestRenderIsDeterministicAcrossBatches(t *testing.T) {
	ra := impl.NewOfflineBounceRenderer(impl.Options{})
	rb := impl.NewOfflineBounceRenderer(impl.Options{})
	jobA := plainJob("det-mix")
	jobB := plainJob("det-mix")
	idA := startJob(t, ra, jobA)
	idB := startJob(t, rb, jobB)
	if _, err := ra.Advance(idA, 3); err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	pa, err := ra.Advance(idA, 5)
	if err != nil {
		t.Fatalf("advance 5: %v", err)
	}
	pb, err := rb.Advance(idB, 8)
	if err != nil {
		t.Fatalf("advance 8: %v", err)
	}
	if pa.Done != 8 || pb.Done != 8 {
		t.Fatalf("done = %d and %d, want 8 blocks for both", pa.Done, pb.Done)
	}
	if pa.Checksum != pb.Checksum || pa.Checksum == 0 {
		t.Fatalf("checksum after 8 blocks = %d and %d, want the same non zero value", pa.Checksum, pb.Checksum)
	}
	fa, err := ra.Advance(idA, 64)
	if err != nil {
		t.Fatalf("finish job a: %v", err)
	}
	fb, err := rb.Advance(idB, 64)
	if err != nil {
		t.Fatalf("finish job b: %v", err)
	}
	if fa.State != impl.JobDone || fb.State != impl.JobDone {
		t.Fatalf("final states = %q and %q, want done", fa.State, fb.State)
	}
	if fa.Done != fb.Done || fa.Checksum != fb.Checksum {
		t.Fatalf("final progress differs: %+v vs %+v", fa, fb)
	}
}

func TestJobsAreIsolated(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	idA := startJob(t, r, plainJob("mix-a"))
	idB := startJob(t, r, plainJob("mix-b"))
	if _, err := r.Advance(idA, 4); err != nil {
		t.Fatalf("advance mix-a: %v", err)
	}
	pa, err := r.Status(idA)
	if err != nil {
		t.Fatalf("status mix-a: %v", err)
	}
	pb, err := r.Status(idB)
	if err != nil {
		t.Fatalf("status mix-b: %v", err)
	}
	if pa.Done != 4 {
		t.Fatalf("mix-a done = %d, want 4", pa.Done)
	}
	if pb.Done != 0 || pb.Project != "mix-b" {
		t.Fatalf("mix-b progress = %+v, want an untouched job", pb)
	}
}

func TestConcurrentStartsRespectLimit(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{MaxJobs: 3})
	var wg sync.WaitGroup
	var mu sync.Mutex
	started, limited := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.Start(plainJob(fmt.Sprintf("race-%02d", i)))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				started++
			case errors.Is(err, impl.ErrJobLimit):
				limited++
			}
		}(i)
	}
	wg.Wait()
	if started != 3 {
		t.Fatalf("concurrent starts accepted %d jobs, want exactly the cap 3", started)
	}
	if limited != 9 {
		t.Fatalf("concurrent starts reported %d limit errors, want 9", limited)
	}
	if got := len(r.Jobs()); got != 3 {
		t.Fatalf("job count after the race = %d, want 3", got)
	}
}

func TestSnapshotRestoreKeepsProgress(t *testing.T) {
	r := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := r.SetTail(20 * time.Millisecond); err != nil {
		t.Fatalf("set tail: %v", err)
	}
	id := startJob(t, r, plainJob("resume-mix"))
	if _, err := r.Advance(id, 4); err != nil {
		t.Fatalf("advance before snapshot: %v", err)
	}
	before, err := r.Status(id)
	if err != nil {
		t.Fatalf("status before snapshot: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewOfflineBounceRenderer(impl.Options{})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, err := restored.Status(id)
	if err != nil {
		t.Fatalf("status after restore: %v", err)
	}
	if after != before {
		t.Fatalf("restored progress = %+v, want %+v", after, before)
	}
	final, err := restored.Advance(id, 64)
	if err != nil {
		t.Fatalf("advance after restore: %v", err)
	}
	if final.State != impl.JobDone || final.Done != 12 {
		t.Fatalf("final progress = %+v, want all 12 blocks done", final)
	}
	if err := restored.Restore([]byte("{")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	kept, err := restored.Status(id)
	if err != nil {
		t.Fatalf("status after rejected restore: %v", err)
	}
	if kept != final {
		t.Fatalf("rejected restore changed progress to %+v, want %+v", kept, final)
	}
}
