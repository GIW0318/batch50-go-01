package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-17"
)

var dispatchBase = time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)

func coaConfig(clock *time.Time) impl.DispatcherConfig {
	return impl.DispatcherConfig{
		BaseBackoff: 2 * time.Second,
		MaxBackoff:  8 * time.Second,
		MaxAttempts: 3,
		TotalBudget: 20 * time.Second,
		MaxPending:  32,
		Now:         func() time.Time { return *clock },
	}
}

func coaRequest(kind, session string) impl.CoaRequest {
	return impl.CoaRequest{
		Kind:      kind,
		SessionID: session,
		NasIP:     "10.9.0.1",
		UserName:  "acct-" + session,
		Attrs:     []impl.Attribute{{Name: "Framed-Pool", Value: "gold"}},
	}
}

func TestSubmitReturnsQueuedJob(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if job.ID == "" {
		t.Fatalf("job id must not be empty")
	}
	if job.Kind != impl.KindCoA || job.SessionID != "sess-1" {
		t.Fatalf("job identity = %+v", job)
	}
	if job.State != impl.StateQueued {
		t.Fatalf("state = %q, want %q", job.State, impl.StateQueued)
	}
	if job.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 before the first dispatch", job.Attempts)
	}
	if !job.NextAt.Equal(clock) {
		t.Fatalf("next dispatch = %v, want %v", job.NextAt, clock)
	}
	pending := d.Pending()
	if len(pending) != 1 || pending[0].ID != job.ID {
		t.Fatalf("pending = %+v, want the submitted job", pending)
	}
	if got := d.Stats().Submitted; got != 1 {
		t.Fatalf("submitted counter = %d, want 1", got)
	}
}

func TestSubmitRejectsBlankAndUnsupportedKind(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	if _, err := d.Submit(impl.CoaRequest{Kind: "", SessionID: "sess-1"}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("blank kind: err = %v, want ErrBadRequest", err)
	}
	if _, err := d.Submit(impl.CoaRequest{Kind: impl.KindCoA, SessionID: "  "}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("blank session: err = %v, want ErrBadRequest", err)
	}
	if _, err := d.Submit(coaRequest("reauth", "sess-1")); !errors.Is(err, impl.ErrUnsupportedKind) {
		t.Fatalf("unsupported kind: err = %v, want ErrUnsupportedKind", err)
	}
	if got := len(d.Pending()); got != 0 {
		t.Fatalf("rejected submissions left %d pending jobs", got)
	}
}

func TestSubmitIsIdempotentWhileJobLive(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	first, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate submit returned %q, want %q", second.ID, first.ID)
	}
	if got := len(d.Pending()); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	if got := d.Stats().Submitted; got != 1 {
		t.Fatalf("submitted counter = %d, want 1", got)
	}
}

func TestSubmitAllowsNewJobAfterConclusion(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	first, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if got := d.Tick(clock); len(got) != 1 {
		t.Fatalf("tick dispatched %d jobs, want 1", len(got))
	}
	if _, err := d.Complete(first.ID, impl.Reply{Code: impl.CodeCoAAck}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	second, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit after conclusion: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("concluded job id %q was reused", first.ID)
	}
	if got := len(d.Pending()); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
}

func TestSubmitEnforcesPendingCap(t *testing.T) {
	clock := dispatchBase
	cfg := coaConfig(&clock)
	cfg.MaxPending = 1
	d := impl.NewRadiusCoaDispatcher(cfg)
	if _, err := d.Submit(coaRequest(impl.KindCoA, "sess-1")); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := d.Submit(coaRequest(impl.KindDisconnect, "sess-2")); !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("over cap: err = %v, want ErrQueueFull", err)
	}
	if got := len(d.Pending()); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
}

func TestTickAppliesExponentialBackoff(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	dispatched := d.Tick(clock)
	if len(dispatched) != 1 {
		t.Fatalf("first tick dispatched %d jobs, want 1", len(dispatched))
	}
	if dispatched[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", dispatched[0].Attempts)
	}
	if !dispatched[0].NextAt.Equal(clock.Add(2 * time.Second)) {
		t.Fatalf("next dispatch = %v, want %v", dispatched[0].NextAt, clock.Add(2*time.Second))
	}
	clock = clock.Add(2 * time.Second)
	dispatched = d.Tick(clock)
	if len(dispatched) != 1 || dispatched[0].Attempts != 2 {
		t.Fatalf("second tick = %+v, want attempt 2", dispatched)
	}
	if !dispatched[0].NextAt.Equal(clock.Add(4 * time.Second)) {
		t.Fatalf("next dispatch = %v, want %v", dispatched[0].NextAt, clock.Add(4*time.Second))
	}
	clock = clock.Add(4 * time.Second)
	dispatched = d.Tick(clock)
	if len(dispatched) != 1 || dispatched[0].Attempts != 3 {
		t.Fatalf("third tick = %+v, want attempt 3", dispatched)
	}
	if !dispatched[0].NextAt.Equal(clock.Add(8 * time.Second)) {
		t.Fatalf("backoff must cap at 8s, next = %v", dispatched[0].NextAt)
	}
	if _, err := d.Outcome(job.ID); err != nil {
		t.Fatalf("outcome: %v", err)
	}
}

func TestTickSkipsJobsBeforeNextAt(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := d.Tick(clock); len(got) != 1 {
		t.Fatalf("first tick dispatched %d jobs, want 1", len(got))
	}
	clock = clock.Add(1 * time.Second)
	if got := d.Tick(clock); len(got) != 0 {
		t.Fatalf("early tick dispatched %+v, want nothing", got)
	}
	result, err := d.Outcome(job.ID)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if result.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after the early tick", result.Attempts)
	}
}

func TestTickStopsAtMaxAttempts(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindDisconnect, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	for i := 0; i < 3; i++ {
		if got := d.Tick(clock); len(got) != 1 {
			t.Fatalf("tick %d dispatched %d jobs, want 1", i, len(got))
		}
		clock = clock.Add(time.Duration(1<<uint(i)) * 2 * time.Second)
	}
	if got := d.Tick(clock); len(got) != 0 {
		t.Fatalf("tick past the attempt cap dispatched %+v, want nothing", got)
	}
	result, err := d.Outcome(job.ID)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if result.State != impl.StateTimeout {
		t.Fatalf("state = %q, want %q", result.State, impl.StateTimeout)
	}
	if result.Retryable {
		t.Fatalf("attempt cap timeout must not stay retryable")
	}
}

func TestCompleteAckRecordsResult(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := d.Tick(clock); len(got) != 1 {
		t.Fatalf("tick dispatched %d jobs, want 1", len(got))
	}
	clock = clock.Add(3 * time.Second)
	result, err := d.Complete(job.ID, impl.Reply{Code: impl.CodeCoAAck})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if result.JobID != job.ID || result.State != impl.StateAcked {
		t.Fatalf("result = %+v, want acked for %s", result, job.ID)
	}
	if result.Code != impl.CodeCoAAck || result.Retryable {
		t.Fatalf("result code/retryable = %d/%v, want 41/false", result.Code, result.Retryable)
	}
	if result.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", result.Attempts)
	}
	if result.Elapsed != 3*time.Second {
		t.Fatalf("elapsed = %v, want 3s", result.Elapsed)
	}
	if got := len(d.Pending()); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
	if _, err := d.Complete(job.ID, impl.Reply{Code: impl.CodeCoAAck}); !errors.Is(err, impl.ErrJobClosed) {
		t.Fatalf("second complete: err = %v, want ErrJobClosed", err)
	}
	if got := d.Stats().Acked; got != 1 {
		t.Fatalf("acked counter = %d, want 1", got)
	}
}

func TestCompleteNakPermanentCause(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	d.Tick(clock)
	result, err := d.Complete(job.ID, impl.Reply{Code: impl.CodeCoANak, Cause: impl.CauseUnsupportedAttribute})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if result.State != impl.StateNacked {
		t.Fatalf("state = %q, want %q", result.State, impl.StateNacked)
	}
	if result.Cause != impl.CauseUnsupportedAttribute {
		t.Fatalf("cause = %d, want %d", result.Cause, impl.CauseUnsupportedAttribute)
	}
	if result.Retryable {
		t.Fatalf("unsupported attribute must not be retryable")
	}
	if got := len(d.Pending()); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
}

func TestCompleteNakResourcesRequeues(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	d.Tick(clock)
	result, err := d.Complete(job.ID, impl.Reply{Code: impl.CodeCoANak, Cause: impl.CauseResourcesUnavailable})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !result.Retryable {
		t.Fatalf("resources unavailable must stay retryable")
	}
	if result.State != impl.StateQueued {
		t.Fatalf("state = %q, want %q", result.State, impl.StateQueued)
	}
	pending := d.Pending()
	if len(pending) != 1 || pending[0].ID != job.ID {
		t.Fatalf("pending = %+v, want the requeued job", pending)
	}
	if pending[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 kept across the requeue", pending[0].Attempts)
	}
}

func TestCompleteRejectsMismatchedCode(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindDisconnect, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	d.Tick(clock)
	if _, err := d.Complete(job.ID, impl.Reply{Code: impl.CodeCoAAck}); !errors.Is(err, impl.ErrProtocolMismatch) {
		t.Fatalf("coa ack on a disconnect job: err = %v, want ErrProtocolMismatch", err)
	}
	result, err := d.Outcome(job.ID)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if result.State != impl.StateInflight {
		t.Fatalf("state = %q, want the job untouched in %q", result.State, impl.StateInflight)
	}
	if got := len(d.Pending()); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	if _, err := d.Complete("job-ghost", impl.Reply{Code: impl.CodeDisconnectAck}); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("unknown job: err = %v, want ErrUnknownJob", err)
	}
}

func TestCancelRemovesFromPending(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	job, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	d.Tick(clock)
	if err := d.Cancel(job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := len(d.Pending()); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
	result, err := d.Outcome(job.ID)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if result.State != impl.StateCanceled {
		t.Fatalf("state = %q, want %q", result.State, impl.StateCanceled)
	}
	if err := d.Cancel(job.ID); !errors.Is(err, impl.ErrJobClosed) {
		t.Fatalf("second cancel: err = %v, want ErrJobClosed", err)
	}
	if err := d.Cancel("job-ghost"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("unknown cancel: err = %v, want ErrUnknownJob", err)
	}
	if got := d.Stats().Canceled; got != 1 {
		t.Fatalf("canceled counter = %d, want 1", got)
	}
}

func TestRetryRules(t *testing.T) {
	clock := dispatchBase
	d := impl.NewRadiusCoaDispatcher(coaConfig(&clock))
	timedOut, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	for i := 0; i < 3; i++ {
		d.Tick(clock)
		clock = clock.Add(time.Duration(1<<uint(i)) * 2 * time.Second)
	}
	d.Tick(clock)
	if result, _ := d.Outcome(timedOut.ID); result.State != impl.StateTimeout {
		t.Fatalf("state = %q, want timeout before retry", result.State)
	}
	if err := d.Retry(timedOut.ID); err != nil {
		t.Fatalf("retry a timed out job: %v", err)
	}
	pending := d.Pending()
	if len(pending) != 1 || pending[0].ID != timedOut.ID {
		t.Fatalf("pending after retry = %+v, want the requeued job", pending)
	}
	acked, err := d.Submit(coaRequest(impl.KindCoA, "sess-2"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	d.Tick(clock)
	if _, err := d.Complete(acked.ID, impl.Reply{Code: impl.CodeCoAAck}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := d.Retry(acked.ID); !errors.Is(err, impl.ErrNotRetryable) {
		t.Fatalf("retry an acked job: err = %v, want ErrNotRetryable", err)
	}
	if err := d.Retry("job-ghost"); !errors.Is(err, impl.ErrUnknownJob) {
		t.Fatalf("retry unknown job: err = %v, want ErrUnknownJob", err)
	}
}

func TestSnapshotRestoreKeepsJobsAndSequence(t *testing.T) {
	clock := dispatchBase
	cfg := coaConfig(&clock)
	d := impl.NewRadiusCoaDispatcher(cfg)
	acked, err := d.Submit(coaRequest(impl.KindCoA, "sess-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	d.Tick(clock)
	if _, err := d.Complete(acked.ID, impl.Reply{Code: impl.CodeCoAAck}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	queued, err := d.Submit(coaRequest(impl.KindDisconnect, "sess-2"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewRadiusCoaDispatcher(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	pending := restored.Pending()
	if len(pending) != 1 || pending[0].ID != queued.ID {
		t.Fatalf("pending after restore = %+v, want %s", pending, queued.ID)
	}
	result, err := restored.Outcome(acked.ID)
	if err != nil {
		t.Fatalf("outcome after restore: %v", err)
	}
	if result.State != impl.StateAcked || result.Code != impl.CodeCoAAck {
		t.Fatalf("acked result after restore = %+v", result)
	}
	stats := restored.Stats()
	if stats.Submitted != 2 || stats.Acked != 1 {
		t.Fatalf("stats after restore = %+v, want 2 submitted and 1 acked", stats)
	}
	if err := restored.Restore([]byte("not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt restore: err = %v, want ErrBadSnapshot", err)
	}
	fresh, err := restored.Submit(coaRequest(impl.KindCoA, "sess-3"))
	if err != nil {
		t.Fatalf("submit after restore: %v", err)
	}
	if fresh.ID == queued.ID || fresh.ID == acked.ID {
		t.Fatalf("job id %q collides with a restored job", fresh.ID)
	}
	if got := len(restored.Pending()); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}
