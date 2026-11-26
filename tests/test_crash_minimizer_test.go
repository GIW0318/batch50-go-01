package task

import (
	"bytes"
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-29"
)

func testLimits() impl.Limits {
	return impl.Limits{MaxReplays: 64, MaxJobs: 4}
}

func startJob(t *testing.T, m *impl.CrashMinimizer, data string, cluster impl.ClusterID) impl.Job {
	t.Helper()
	job, err := m.Start(impl.Sample{ID: "sample-1", Data: []byte(data)}, cluster)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	return job
}

func TestStartRejectsEmptySample(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	if _, err := m.Start(impl.Sample{ID: "empty"}, "heap-overflow"); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("sample without data must fail with ErrBadSample, got %v", err)
	}
	if _, err := m.Start(impl.Sample{ID: "", Data: []byte("x")}, "heap-overflow"); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("sample without an id must fail with ErrBadSample, got %v", err)
	}
	if got := len(m.Jobs()); got != 0 {
		t.Fatalf("rejected samples must not open jobs, got %d", got)
	}
}

func TestStartEnforcesJobLimit(t *testing.T) {
	m := impl.NewCrashMinimizer(impl.Limits{MaxReplays: 64, MaxJobs: 1})
	startJob(t, m, "AAAABBBB", "use-after-free")
	if _, err := m.Start(impl.Sample{ID: "sample-2", Data: []byte("CCCCDDDD")}, "use-after-free"); !errors.Is(err, impl.ErrJobLimit) {
		t.Fatalf("second job must fail with ErrJobLimit, got %v", err)
	}
	if got := len(m.Jobs()); got != 1 {
		t.Fatalf("job table holds %d entries, want 1", got)
	}
}

func TestStartReturnsRunningJob(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "AAAABBBBCCCCCRASH", "heap-overflow")
	if job.ID == "" {
		t.Fatalf("job id must not be empty")
	}
	if job.Cluster != "heap-overflow" {
		t.Fatalf("job cluster = %q, want heap-overflow", job.Cluster)
	}
	if job.Status != impl.StatusRunning {
		t.Fatalf("job status = %q, want %q", job.Status, impl.StatusRunning)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best: %v", err)
	}
	if string(best.Data) != "AAAABBBBCCCCCRASH" {
		t.Fatalf("best input before any replay = %q, want the untouched sample", best.Data)
	}
	if got := m.Jobs(); len(got) != 1 || got[0] != job.ID {
		t.Fatalf("jobs = %v, want only %q", got, job.ID)
	}
}

func TestCandidateUnknownJobFails(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	if _, err := m.Candidate("job-404"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("unknown job must fail with ErrUnknownJob, got %v", err)
	}
	if _, err := m.Candidate(""); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("blank job id must fail with ErrUnknownJob, got %v", err)
	}
}

func TestResultBeforeCandidateFails(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "AAAABBBB", "heap-overflow")
	if err := m.Result(job.ID, true, "heap-overflow"); !errors.Is(err, impl.ErrNoCandidate) {
		t.Fatalf("a result without an outstanding candidate must fail with ErrNoCandidate, got %v", err)
	}
}

func TestRejectedCandidateKeepsBest(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	sample := "PADDING-ONLY-NO-CRASH"
	job := startJob(t, m, sample, "heap-overflow")
	cand, err := m.Candidate(job.ID)
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if bytes.Equal(cand.Data, []byte(sample)) {
		t.Fatalf("the first candidate must already be shorter than the sample")
	}
	if err := m.Result(job.ID, false, "heap-overflow"); err != nil {
		t.Fatalf("result: %v", err)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best: %v", err)
	}
	if string(best.Data) != sample {
		t.Fatalf("a replay that does not crash must not replace the best input, got %q", best.Data)
	}
	pr, err := m.Progress(job.ID)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if pr.Replays != 1 || pr.Rejected != 1 || pr.Accepted != 0 {
		t.Fatalf("progress = %+v, want 1 replay and 1 rejection", pr)
	}
}

func TestAcceptedCandidateReplacesBest(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "PADDING-CRASH", "heap-overflow")
	first, err := m.Candidate(job.ID)
	if err != nil {
		t.Fatalf("first candidate: %v", err)
	}
	if bytes.Contains(first.Data, []byte("CRASH")) {
		t.Fatalf("the first cut drops the tail half and must not contain the crash marker, got %q", first.Data)
	}
	if err := m.Result(job.ID, false, "heap-overflow"); err != nil {
		t.Fatalf("first result: %v", err)
	}
	second, err := m.Candidate(job.ID)
	if err != nil {
		t.Fatalf("second candidate: %v", err)
	}
	if !bytes.Contains(second.Data, []byte("CRASH")) {
		t.Fatalf("the second cut drops the head half and must keep the crash marker, got %q", second.Data)
	}
	if err := m.Result(job.ID, true, "heap-overflow"); err != nil {
		t.Fatalf("second result: %v", err)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best: %v", err)
	}
	if !bytes.Equal(best.Data, second.Data) {
		t.Fatalf("an accepted candidate must become the best input, got %q want %q", best.Data, second.Data)
	}
	if len(best.Data) >= len("PADDING-CRASH") {
		t.Fatalf("the best input did not shrink: %d bytes", len(best.Data))
	}
}

func TestMinimizerConvergesToCrashingInput(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	sample := "ZZZZZZZZZZCRASH"
	job := startJob(t, m, sample, "heap-overflow")
	steps := 0
	for {
		cand, err := m.Candidate(job.ID)
		if errors.Is(err, impl.ErrConverged) {
			break
		}
		if err != nil {
			t.Fatalf("candidate: %v", err)
		}
		steps++
		if steps > 32 {
			t.Fatalf("minimisation did not converge within 32 steps")
		}
		crashed := bytes.Contains(cand.Data, []byte("CRASH"))
		if err := m.Result(job.ID, crashed, "heap-overflow"); err != nil {
			t.Fatalf("result: %v", err)
		}
	}
	if steps == 0 {
		t.Fatalf("minimisation must replay at least one candidate")
	}
	status, err := m.Status(job.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != impl.StatusConverged {
		t.Fatalf("status = %q, want %q after both halves were rejected", status, impl.StatusConverged)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best: %v", err)
	}
	if string(best.Data) != "CRASH" {
		t.Fatalf("best input = %q, want the trimmed CRASH marker", best.Data)
	}
	if len(best.Data) >= len(sample) {
		t.Fatalf("best input did not shrink below the sample")
	}
}

func TestReplayBudgetExhausted(t *testing.T) {
	m := impl.NewCrashMinimizer(impl.Limits{MaxReplays: 1, MaxJobs: 4})
	job := startJob(t, m, "PADDING-ONLY", "heap-overflow")
	if _, err := m.Candidate(job.ID); err != nil {
		t.Fatalf("first candidate: %v", err)
	}
	if err := m.Result(job.ID, false, "heap-overflow"); err != nil {
		t.Fatalf("first result: %v", err)
	}
	if _, err := m.Candidate(job.ID); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("the second candidate must hit the replay budget with ErrBudgetExhausted, got %v", err)
	}
	status, err := m.Status(job.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != impl.StatusBudget {
		t.Fatalf("status = %q, want %q", status, impl.StatusBudget)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best: %v", err)
	}
	if string(best.Data) != "PADDING-ONLY" {
		t.Fatalf("an exhausted job must keep its best input, got %q", best.Data)
	}
}

func TestBestBeforeAcceptReturnsSample(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "AAAA-BBBB", "stack-overflow")
	if _, err := m.Candidate(job.ID); err != nil {
		t.Fatalf("candidate: %v", err)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best: %v", err)
	}
	if string(best.Data) != "AAAA-BBBB" {
		t.Fatalf("best while a candidate is outstanding = %q, want the sample", best.Data)
	}
}

func TestCancelStopsCandidates(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "AAAABBBB", "heap-overflow")
	if _, err := m.Candidate(job.ID); err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if err := m.Cancel(job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := m.Candidate(job.ID); !errors.Is(err, impl.ErrJobCancelled) {
		t.Fatalf("candidate on a cancelled job must fail with ErrJobCancelled, got %v", err)
	}
	if err := m.Result(job.ID, true, "heap-overflow"); !errors.Is(err, impl.ErrJobCancelled) {
		t.Fatalf("result on a cancelled job must fail with ErrJobCancelled, got %v", err)
	}
	if err := m.Cancel(job.ID); err != nil {
		t.Fatalf("cancel must be idempotent, got %v", err)
	}
	best, err := m.Best(job.ID)
	if err != nil {
		t.Fatalf("best of a cancelled job must stay readable, got %v", err)
	}
	if string(best.Data) != "AAAABBBB" {
		t.Fatalf("cancelled job best = %q, want the sample", best.Data)
	}
	if err := m.Cancel("job-404"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("cancelling an unknown job must fail with ErrUnknownJob, got %v", err)
	}
}

func TestProgressCountsAcceptedAndRejected(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "PADDING-CRASH", "heap-overflow")
	if _, err := m.Candidate(job.ID); err != nil {
		t.Fatalf("first candidate: %v", err)
	}
	if err := m.Result(job.ID, false, "heap-overflow"); err != nil {
		t.Fatalf("first result: %v", err)
	}
	second, err := m.Candidate(job.ID)
	if err != nil {
		t.Fatalf("second candidate: %v", err)
	}
	if err := m.Result(job.ID, true, "heap-overflow"); err != nil {
		t.Fatalf("second result: %v", err)
	}
	pr, err := m.Progress(job.ID)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if pr.Replays != 2 || pr.Rejected != 1 || pr.Accepted != 1 {
		t.Fatalf("progress = %+v, want 2 replays split into 1 rejection and 1 acceptance", pr)
	}
	if pr.BestLen != len(second.Data) {
		t.Fatalf("progress best length = %d, want %d", pr.BestLen, len(second.Data))
	}
	if pr.Status != impl.StatusRunning {
		t.Fatalf("progress status = %q, want %q", pr.Status, impl.StatusRunning)
	}
}

func TestSnapshotRestoreKeepsBestAndReplays(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "ZZZZZZZZZZCRASH", "heap-overflow")
	if _, err := m.Candidate(job.ID); err != nil {
		t.Fatalf("first candidate: %v", err)
	}
	if err := m.Result(job.ID, false, "heap-overflow"); err != nil {
		t.Fatalf("first result: %v", err)
	}
	second, err := m.Candidate(job.ID)
	if err != nil {
		t.Fatalf("second candidate: %v", err)
	}
	if err := m.Result(job.ID, true, "heap-overflow"); err != nil {
		t.Fatalf("second result: %v", err)
	}
	before, err := m.Progress(job.ID)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	data, err := m.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewCrashMinimizer(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	best, err := restored.Best(job.ID)
	if err != nil {
		t.Fatalf("best after restore: %v", err)
	}
	if !bytes.Equal(best.Data, second.Data) {
		t.Fatalf("restored best = %q, want %q", best.Data, second.Data)
	}
	after, err := restored.Progress(job.ID)
	if err != nil {
		t.Fatalf("progress after restore: %v", err)
	}
	if after.Replays != before.Replays || after.Accepted != before.Accepted || after.Rejected != before.Rejected {
		t.Fatalf("restored counters = %+v, want %+v", after, before)
	}
	if _, err := restored.Candidate(job.ID); err != nil {
		t.Fatalf("minimisation must continue after a restore, got %v", err)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	job := startJob(t, m, "AAAABBBB", "heap-overflow")
	if err := m.Restore([]byte("<<corrupt>>")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, err := m.Best(job.ID); err != nil {
		t.Fatalf("a rejected restore must not drop live jobs, got %v", err)
	}
	if _, err := m.Candidate(job.ID); err != nil {
		t.Fatalf("live jobs must keep working after a rejected restore, got %v", err)
	}
}

func TestResultUnknownJobFails(t *testing.T) {
	m := impl.NewCrashMinimizer(testLimits())
	if err := m.Result("job-404", true, "heap-overflow"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("a result for an unknown job must fail with ErrUnknownJob, got %v", err)
	}
	if _, err := m.Best("job-404"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("best for an unknown job must fail with ErrUnknownJob, got %v", err)
	}
	if _, err := m.Progress("job-404"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("progress for an unknown job must fail with ErrUnknownJob, got %v", err)
	}
}
