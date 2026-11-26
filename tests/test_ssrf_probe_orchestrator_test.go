package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-21"
)

func publicLookup(host string) ([]string, error) {
	switch host {
	case "cdn.example.net":
		return []string{"203.0.113.9"}, nil
	case "second.example.net":
		return []string{"203.0.113.10"}, nil
	default:
		return []string{"93.184.216.34"}, nil
	}
}

func loopbackLookup(host string) ([]string, error) {
	if host == "example.com" {
		return []string{"93.184.216.34"}, nil
	}
	return []string{"127.0.0.1"}, nil
}

func baseOptions() impl.Options {
	return impl.Options{MaxConcurrentProbes: 4, MaxHops: 4, TotalMillis: 4000, MaxRequestsPerProbe: 8, Lookup: publicLookup}
}

func sampleTarget(host, param string) impl.Target {
	return impl.Target{Scheme: impl.SchemeHTTP, Host: host, Port: 8080, Method: "GET", Param: param}
}

func TestSubmitQueuesProbe(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit must succeed, got %v", err)
	}
	if id == "" {
		t.Fatalf("probe id must not be empty")
	}
	state, ok := o.Probe(id)
	if !ok {
		t.Fatalf("submitted probe must be readable")
	}
	if state.State != impl.StateQueued {
		t.Fatalf("probe state = %q, want %q", state.State, impl.StateQueued)
	}
	if state.Target.Host != "example.com" || state.Target.Param != "url" {
		t.Fatalf("probe target mismatch: %+v", state.Target)
	}
	pending := o.Pending()
	if len(pending) != 1 || pending[0] != id {
		t.Fatalf("pending = %v, want exactly [%s]", pending, id)
	}
}

func TestSubmitRejectsBlankTarget(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	blank := impl.Target{Scheme: impl.SchemeHTTP, Host: "example.com", Port: 8080, Method: "GET", Param: "   "}
	if _, err := o.Submit(blank); !errors.Is(err, impl.ErrBlankTarget) {
		t.Fatalf("blank parameter must be rejected with ErrBlankTarget, got %v", err)
	}
	empty := impl.Target{Scheme: impl.SchemeHTTP, Host: "", Port: 8080, Method: "GET", Param: "url"}
	if _, err := o.Submit(empty); !errors.Is(err, impl.ErrBlankTarget) {
		t.Fatalf("blank host must be rejected with ErrBlankTarget, got %v", err)
	}
	if got := len(o.Pending()); got != 0 {
		t.Fatalf("rejected targets must not register probes, got %d pending", got)
	}
}

func TestSubmitEnforcesConcurrencyLimit(t *testing.T) {
	opts := baseOptions()
	opts.MaxConcurrentProbes = 2
	o := impl.NewSsrfProbeOrchestrator(opts)
	for _, host := range []string{"a.example", "b.example"} {
		if _, err := o.Submit(sampleTarget(host, "url")); err != nil {
			t.Fatalf("submit %s: %v", host, err)
		}
	}
	if _, err := o.Submit(sampleTarget("c.example", "url")); !errors.Is(err, impl.ErrProbeLimit) {
		t.Fatalf("third probe must hit the concurrency cap, got %v", err)
	}
	if got := len(o.Pending()); got != 2 {
		t.Fatalf("pending probes = %d, want the cap 2", got)
	}
}

func TestNextBlocksLoopbackTarget(t *testing.T) {
	opts := baseOptions()
	opts.Lookup = loopbackLookup
	o := impl.NewSsrfProbeOrchestrator(opts)
	id, err := o.Submit(sampleTarget("internal.example", "next"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := o.Next(id); !errors.Is(err, impl.ErrBlockedTarget) {
		t.Fatalf("loopback resolution must be rejected with ErrBlockedTarget, got %v", err)
	}
	verdict, err := o.Verdict(id)
	if err != nil {
		t.Fatalf("blocked probe must still produce a verdict, got %v", err)
	}
	if !verdict.Blocked {
		t.Fatalf("verdict must be marked blocked")
	}
	if verdict.Reason != impl.ReasonBlockedLoopback {
		t.Fatalf("verdict reason = %q, want %q", verdict.Reason, impl.ReasonBlockedLoopback)
	}
	if verdict.Hop != 1 {
		t.Fatalf("blocked hop = %d, want the first hop", verdict.Hop)
	}
	if len(verdict.Evidence) == 0 {
		t.Fatalf("blocked verdict must carry evidence")
	}
	state, ok := o.Probe(id)
	if !ok || state.State != impl.StateBlocked {
		t.Fatalf("probe state = %q ok=%v, want %q", state.State, ok, impl.StateBlocked)
	}
}

func TestNextReturnsPrimaryStep(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	step, err := o.Next(id)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if step.ID == "" || step.ProbeID != id {
		t.Fatalf("step identity mismatch: %+v", step)
	}
	if step.Kind != impl.KindPrimary || step.Hop != 1 {
		t.Fatalf("step kind=%q hop=%d, want primary hop 1", step.Kind, step.Hop)
	}
	if step.URL != "http://example.com:8080/url" {
		t.Fatalf("primary url = %q, want http://example.com:8080/url", step.URL)
	}
	if step.Budget.HopsLeft != 4 || step.Budget.MillisLeft != 4000 || step.Budget.Requests != 1 {
		t.Fatalf("budget = %+v, want hops 4 millis 4000 requests 1", step.Budget)
	}
}

func TestNextRequiresPreviousStepReported(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := o.Next(id); err != nil {
		t.Fatalf("first next: %v", err)
	}
	if _, err := o.Next(id); !errors.Is(err, impl.ErrStepPending) {
		t.Fatalf("second next without a report must fail with ErrStepPending, got %v", err)
	}
}

func TestRedirectAdvancesHop(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	first, err := o.Next(id)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	obs := impl.Observation{StepID: first.ID, StatusCode: 302, Location: "https://cdn.example.net/a", ElapsedMillis: 120}
	if err := o.Report(first.ID, obs); err != nil {
		t.Fatalf("report redirect: %v", err)
	}
	second, err := o.Next(id)
	if err != nil {
		t.Fatalf("next after redirect: %v", err)
	}
	if second.Kind != impl.KindRedirect || second.Hop != 2 {
		t.Fatalf("second step kind=%q hop=%d, want redirect hop 2", second.Kind, second.Hop)
	}
	if second.URL != "https://cdn.example.net/a" {
		t.Fatalf("redirect url = %q, want the reported location", second.URL)
	}
	if second.Budget.Requests != 2 || second.Budget.HopsLeft != 3 {
		t.Fatalf("budget after redirect = %+v, want requests 2 hops 3", second.Budget)
	}
}

func TestRedirectToBlockedTargetStops(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(impl.Options{MaxConcurrentProbes: 4, MaxHops: 4, TotalMillis: 4000, Lookup: loopbackLookup})
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	first, err := o.Next(id)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	obs := impl.Observation{StepID: first.ID, StatusCode: 307, Location: "http://127.0.0.1/admin", ElapsedMillis: 90}
	if err := o.Report(first.ID, obs); err != nil {
		t.Fatalf("report redirect: %v", err)
	}
	if _, err := o.Next(id); !errors.Is(err, impl.ErrBlockedTarget) {
		t.Fatalf("redirect into loopback must be rejected with ErrBlockedTarget, got %v", err)
	}
	verdict, err := o.Verdict(id)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if !verdict.Blocked || verdict.Hop != 2 {
		t.Fatalf("verdict blocked=%v hop=%d, want blocked at hop 2", verdict.Blocked, verdict.Hop)
	}
	if verdict.Reason != impl.ReasonBlockedLoopback {
		t.Fatalf("verdict reason = %q, want %q", verdict.Reason, impl.ReasonBlockedLoopback)
	}
}

func TestHopBudgetExhausted(t *testing.T) {
	opts := baseOptions()
	opts.MaxHops = 2
	o := impl.NewSsrfProbeOrchestrator(opts)
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	first, err := o.Next(id)
	if err != nil {
		t.Fatalf("first hop: %v", err)
	}
	if err := o.Report(first.ID, impl.Observation{StepID: first.ID, StatusCode: 302, Location: "https://cdn.example.net/a", ElapsedMillis: 50}); err != nil {
		t.Fatalf("report hop 1: %v", err)
	}
	second, err := o.Next(id)
	if err != nil {
		t.Fatalf("second hop: %v", err)
	}
	if err := o.Report(second.ID, impl.Observation{StepID: second.ID, StatusCode: 302, Location: "https://second.example.net/b", ElapsedMillis: 50}); err != nil {
		t.Fatalf("report hop 2: %v", err)
	}
	if _, err := o.Next(id); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("third hop must exhaust the hop budget, got %v", err)
	}
	verdict, err := o.Verdict(id)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if verdict.Reason != impl.ReasonHopBudget {
		t.Fatalf("verdict reason = %q, want %q", verdict.Reason, impl.ReasonHopBudget)
	}
	state, ok := o.Probe(id)
	if !ok || state.State != impl.StateExhausted {
		t.Fatalf("probe state = %q ok=%v, want %q", state.State, ok, impl.StateExhausted)
	}
}

func TestTimeBudgetExhausted(t *testing.T) {
	opts := baseOptions()
	opts.TotalMillis = 500
	o := impl.NewSsrfProbeOrchestrator(opts)
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	first, err := o.Next(id)
	if err != nil {
		t.Fatalf("first hop: %v", err)
	}
	if err := o.Report(first.ID, impl.Observation{StepID: first.ID, StatusCode: 302, Location: "https://cdn.example.net/a", ElapsedMillis: 700}); err != nil {
		t.Fatalf("report slow hop: %v", err)
	}
	if _, err := o.Next(id); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("overdrawn time budget must stop the chain, got %v", err)
	}
	verdict, err := o.Verdict(id)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if verdict.Reason != impl.ReasonTimeBudget {
		t.Fatalf("verdict reason = %q, want %q", verdict.Reason, impl.ReasonTimeBudget)
	}
}

func TestReportRejectsClosedAndUnknownStep(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	step, err := o.Next(id)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if err := o.Report("step-404", impl.Observation{StepID: "step-404", StatusCode: 200}); !errors.Is(err, impl.ErrUnknownStep) {
		t.Fatalf("unknown step must fail with ErrUnknownStep, got %v", err)
	}
	if err := o.Report(step.ID, impl.Observation{StepID: step.ID, StatusCode: 200}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	if err := o.Report(step.ID, impl.Observation{StepID: step.ID, StatusCode: 200}); !errors.Is(err, impl.ErrStepClosed) {
		t.Fatalf("repeated report must fail with ErrStepClosed, got %v", err)
	}
	verdict, err := o.Verdict(id)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if !verdict.Done || verdict.Reason != impl.ReasonCompleted {
		t.Fatalf("final response verdict done=%v reason=%q, want done/completed", verdict.Done, verdict.Reason)
	}
}

func TestCancelClosesProbeAndFreesSlot(t *testing.T) {
	opts := baseOptions()
	opts.MaxConcurrentProbes = 1
	o := impl.NewSsrfProbeOrchestrator(opts)
	first, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit first: %v", err)
	}
	if err := o.Cancel(first); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := o.Cancel(first); err != nil {
		t.Fatalf("cancel must be idempotent, got %v", err)
	}
	if _, err := o.Next(first); !errors.Is(err, impl.ErrProbeCancelled) {
		t.Fatalf("next on a cancelled probe must fail with ErrProbeCancelled, got %v", err)
	}
	verdict, err := o.Verdict(first)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if !verdict.Cancelled || verdict.Reason != impl.ReasonCancelled {
		t.Fatalf("cancel verdict cancelled=%v reason=%q", verdict.Cancelled, verdict.Reason)
	}
	if _, err := o.Submit(sampleTarget("second.example", "url")); err != nil {
		t.Fatalf("cancel must release the concurrency slot, got %v", err)
	}
}

func TestSnapshotRestoreResumesChain(t *testing.T) {
	opts := baseOptions()
	o := impl.NewSsrfProbeOrchestrator(opts)
	doneID, err := o.Submit(sampleTarget("second.example", "url"))
	if err != nil {
		t.Fatalf("submit completed probe: %v", err)
	}
	first, err := o.Next(doneID)
	if err != nil {
		t.Fatalf("next completed probe: %v", err)
	}
	if err := o.Report(first.ID, impl.Observation{StepID: first.ID, StatusCode: 200, ElapsedMillis: 40}); err != nil {
		t.Fatalf("report completed probe: %v", err)
	}
	openID, err := o.Submit(sampleTarget("example.com", "next"))
	if err != nil {
		t.Fatalf("submit open probe: %v", err)
	}
	openStep, err := o.Next(openID)
	if err != nil {
		t.Fatalf("next open probe: %v", err)
	}
	if err := o.Report(openStep.ID, impl.Observation{StepID: openStep.ID, StatusCode: 301, Location: "https://cdn.example.net/a", ElapsedMillis: 300}); err != nil {
		t.Fatalf("report open probe: %v", err)
	}
	data, err := o.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewSsrfProbeOrchestrator(opts)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	state, ok := restored.Probe(openID)
	if !ok {
		t.Fatalf("restored orchestrator lost the open probe")
	}
	if state.Requests != 1 || state.ElapsedMillis != 300 {
		t.Fatalf("restored requests=%d elapsed=%d, want 1 and 300", state.Requests, state.ElapsedMillis)
	}
	next, err := restored.Next(openID)
	if err != nil {
		t.Fatalf("next after restore: %v", err)
	}
	if next.Hop != 2 || next.Kind != impl.KindRedirect || next.URL != "https://cdn.example.net/a" {
		t.Fatalf("restored chain resumed at %+v, want redirect hop 2", next)
	}
	if next.Budget.Requests != 2 || next.Budget.MillisLeft != 3700 {
		t.Fatalf("restored budget = %+v, want requests 2 millis 3700", next.Budget)
	}
	verdict, err := restored.Verdict(doneID)
	if err != nil {
		t.Fatalf("restored verdict: %v", err)
	}
	if !verdict.Done || verdict.Reason != impl.ReasonCompleted {
		t.Fatalf("completed verdict lost in snapshot: done=%v reason=%q", verdict.Done, verdict.Reason)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	id, err := o.Submit(sampleTarget("example.com", "url"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	step, err := o.Next(id)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if err := o.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	state, ok := o.Probe(id)
	if !ok || state.State != impl.StateActive {
		t.Fatalf("failed restore must not touch live state, got state=%q ok=%v", state.State, ok)
	}
	if err := o.Report(step.ID, impl.Observation{StepID: step.ID, StatusCode: 200}); err != nil {
		t.Fatalf("live step must keep working after a rejected restore, got %v", err)
	}
	verdict, err := o.Verdict(id)
	if err != nil || !verdict.Done {
		t.Fatalf("probe must complete normally, verdict=%+v err=%v", verdict, err)
	}
}

func TestClassifyAddressFamilies(t *testing.T) {
	o := impl.NewSsrfProbeOrchestrator(baseOptions())
	cases := []struct {
		addr string
		want string
	}{
		{"127.0.0.1", impl.FamilyLoopback},
		{"10.1.2.3", impl.FamilyPrivate},
		{"169.254.169.254", impl.FamilyLinkLocal},
		{"93.184.216.34", impl.FamilyPublic},
		{"::1", impl.FamilyLoopback},
		{"not-an-address", impl.FamilyInvalid},
	}
	for _, c := range cases {
		if got := o.ClassifyAddress(c.addr); got != c.want {
			t.Fatalf("ClassifyAddress(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
