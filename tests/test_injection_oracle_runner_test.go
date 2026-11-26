package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-22"
)

func newRunner(cap int) *impl.InjectionOracleRunner {
	return impl.NewInjectionOracleRunner(impl.Options{
		MaxAttemptsPerPoint:    cap,
		MaxConcurrentPoints:    4,
		TotalDelayBudgetMillis: 60000,
	})
}

func echoPayload() impl.Payload {
	return impl.Payload{Name: "echo-cmd", Value: "id;echo GSB-XYZ", Marker: "GSB-XYZ", DelayMillis: 600}
}

func templatePayload() impl.Payload {
	return impl.Payload{Name: "tpl-expr", Value: "{{7*7}}", Marker: "GSB-TPL", DelayMillis: 400}
}

func pointWith(key string, payloads ...impl.Payload) impl.InjectionPoint {
	return impl.InjectionPoint{Key: key, Param: "q", BaseRequest: "GET /search?q=1", SinkKind: impl.SinkCommand, Payloads: payloads}
}

func planOne(t *testing.T, r *impl.InjectionOracleRunner, key string) impl.CaseSet {
	t.Helper()
	set, err := r.Plan(pointWith(key, echoPayload()))
	if err != nil {
		t.Fatalf("plan %s: %v", key, err)
	}
	return set
}

func runAndSubmit(t *testing.T, r *impl.InjectionOracleRunner, caseID, channel, body string, elapsed int) {
	t.Helper()
	att, err := r.Run(caseID)
	if err != nil {
		t.Fatalf("run %s channel: %v", channel, err)
	}
	if att.Channel != channel {
		t.Fatalf("attempt channel = %q, want %q", att.Channel, channel)
	}
	if err := r.Submit(att.ID, impl.Response{AttemptID: att.ID, StatusCode: 200, Body: body, ElapsedMillis: elapsed}); err != nil {
		t.Fatalf("submit %s channel: %v", channel, err)
	}
}

func TestPlanBuildsSerialCases(t *testing.T) {
	r := newRunner(12)
	set, err := r.Plan(pointWith("p-plan", echoPayload(), templatePayload()))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if set.ID == "" || set.PointKey != "p-plan" {
		t.Fatalf("case set identity mismatch: %+v", set)
	}
	if len(set.Cases) != 2 {
		t.Fatalf("planned cases = %d, want one per payload", len(set.Cases))
	}
	if set.Cases[0].PayloadName != "echo-cmd" || set.Cases[1].PayloadName != "tpl-expr" {
		t.Fatalf("case order = %q,%q, want echo-cmd then tpl-expr", set.Cases[0].PayloadName, set.Cases[1].PayloadName)
	}
	if set.AttemptsPerCase != 3 {
		t.Fatalf("attempts per case = %d, want 3 channels", set.AttemptsPerCase)
	}
	cases := r.Cases("p-plan")
	if len(cases) != 2 || cases[0].ID != set.Cases[0].ID {
		t.Fatalf("cases lookup = %+v, want the planned order", cases)
	}
	one, ok := r.Case(set.Cases[0].ID)
	if !ok || one.State != impl.CasePlanned {
		t.Fatalf("case state = %q ok=%v, want %q", one.State, ok, impl.CasePlanned)
	}
}

func TestPlanRejectsBlankPoint(t *testing.T) {
	r := newRunner(12)
	if _, err := r.Plan(pointWith("", echoPayload())); !errors.Is(err, impl.ErrBlankPoint) {
		t.Fatalf("blank key must fail with ErrBlankPoint, got %v", err)
	}
	blankParam := pointWith("p-blank", echoPayload())
	blankParam.Param = "  "
	if _, err := r.Plan(blankParam); !errors.Is(err, impl.ErrBlankPoint) {
		t.Fatalf("blank parameter must fail with ErrBlankPoint, got %v", err)
	}
	if got := len(r.Cases("p-blank")); got != 0 {
		t.Fatalf("rejected points must not plan cases, got %d", got)
	}
}

func TestPlanEnforcesAttemptCap(t *testing.T) {
	r := newRunner(5)
	if _, err := r.Plan(pointWith("p-cap", echoPayload(), templatePayload())); !errors.Is(err, impl.ErrAttemptLimit) {
		t.Fatalf("six needed attempts under a cap of five must fail with ErrAttemptLimit, got %v", err)
	}
	if got := len(r.Cases("p-cap")); got != 0 {
		t.Fatalf("rejected plan must not register cases, got %d", got)
	}
}

func TestPlanRejectsDuplicatePoint(t *testing.T) {
	r := newRunner(12)
	if _, err := r.Plan(pointWith("p-dup", echoPayload())); err != nil {
		t.Fatalf("first plan: %v", err)
	}
	if _, err := r.Plan(pointWith("p-dup", templatePayload())); !errors.Is(err, impl.ErrPointExists) {
		t.Fatalf("second plan of the same point must fail with ErrPointExists, got %v", err)
	}
}

func TestPlanRejectsEmptyPayloadList(t *testing.T) {
	r := newRunner(12)
	if _, err := r.Plan(pointWith("p-empty")); !errors.Is(err, impl.ErrBadPayload) {
		t.Fatalf("a point without payloads must fail with ErrBadPayload, got %v", err)
	}
}

func TestRunReturnsAttemptsInChannelOrder(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-order")
	caseID := set.Cases[0].ID
	want := []string{impl.ChannelBaseline, impl.ChannelEcho, impl.ChannelTime}
	for i, channel := range want {
		att, err := r.Run(caseID)
		if err != nil {
			t.Fatalf("run %s: %v", channel, err)
		}
		if att.Channel != channel {
			t.Fatalf("attempt %d channel = %q, want %q", i, att.Channel, channel)
		}
		if att.Ordinal != i+1 {
			t.Fatalf("attempt %d ordinal = %d, want %d", i, att.Ordinal, i+1)
		}
		if att.CaseID != caseID || att.ID == "" {
			t.Fatalf("attempt identity mismatch: %+v", att)
		}
		if err := r.Submit(att.ID, impl.Response{AttemptID: att.ID, StatusCode: 200, Body: "hello world", ElapsedMillis: 120}); err != nil {
			t.Fatalf("submit %s: %v", channel, err)
		}
	}
	if _, err := r.Run(caseID); !errors.Is(err, impl.ErrCaseClosed) {
		t.Fatalf("a fully submitted case must stop handing out attempts, got %v", err)
	}
}

func TestRunRejectsSecondAttemptWhileOutstanding(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-pending")
	if _, err := r.Run(set.Cases[0].ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := r.Run(set.Cases[0].ID); !errors.Is(err, impl.ErrAttemptPending) {
		t.Fatalf("second run before submit must fail with ErrAttemptPending, got %v", err)
	}
}

func TestRunRejectsOtherCaseOfSamePoint(t *testing.T) {
	r := newRunner(12)
	set, err := r.Plan(pointWith("p-serial", echoPayload(), templatePayload()))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := r.Run(set.Cases[0].ID); err != nil {
		t.Fatalf("run first case: %v", err)
	}
	if _, err := r.Run(set.Cases[1].ID); !errors.Is(err, impl.ErrCasePending) {
		t.Fatalf("a second case of the same point must wait, got %v", err)
	}
}

func TestSubmitRejectsUnknownAndDuplicateAttempt(t *testing.T) {
	r := newRunner(12)
	if err := r.Submit("attempt-404", impl.Response{AttemptID: "attempt-404", StatusCode: 200}); !errors.Is(err, impl.ErrUnknownAttempt) {
		t.Fatalf("unknown attempt must fail with ErrUnknownAttempt, got %v", err)
	}
	set := planOne(t, r, "p-submit")
	att, err := r.Run(set.Cases[0].ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := r.Submit(att.ID, impl.Response{AttemptID: att.ID, StatusCode: 200, Body: "hello world", ElapsedMillis: 120}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := r.Submit(att.ID, impl.Response{AttemptID: att.ID, StatusCode: 200, Body: "hello world", ElapsedMillis: 120}); !errors.Is(err, impl.ErrAttemptClosed) {
		t.Fatalf("repeated submit must fail with ErrAttemptClosed, got %v", err)
	}
}

func TestJudgeBeforeChannelsComplete(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-early")
	caseID := set.Cases[0].ID
	runAndSubmit(t, r, caseID, impl.ChannelBaseline, "hello world", 120)
	runAndSubmit(t, r, caseID, impl.ChannelEcho, "hello GSB-XYZ world", 130)
	if _, err := r.Judge(caseID); !errors.Is(err, impl.ErrChannelsPending) {
		t.Fatalf("judging before the latency channel must fail with ErrChannelsPending, got %v", err)
	}
}

func TestJudgeEchoHit(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-echo")
	caseID := set.Cases[0].ID
	runAndSubmit(t, r, caseID, impl.ChannelBaseline, "hello world", 120)
	runAndSubmit(t, r, caseID, impl.ChannelEcho, "hello GSB-XYZ world", 130)
	runAndSubmit(t, r, caseID, impl.ChannelTime, "hello world", 140)
	out, err := r.Judge(caseID)
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if !out.Hit || out.Channel != impl.ChannelEcho || out.Confidence != impl.ConfidenceMedium {
		t.Fatalf("echo hit got hit=%v channel=%q confidence=%q", out.Hit, out.Channel, out.Confidence)
	}
	if out.Reason != impl.ReasonEchoHit {
		t.Fatalf("outcome reason = %q, want %q", out.Reason, impl.ReasonEchoHit)
	}
	if !out.EchoMatched {
		t.Fatalf("echo channel must be marked as matched")
	}
	if out.BaselineMillis != 120 || out.ObservedMillis != 140 {
		t.Fatalf("latency bookkeeping = baseline %d observed %d, want 120 and 140", out.BaselineMillis, out.ObservedMillis)
	}
	if out.DeltaMillis != 20 || out.ThresholdMillis != 300 {
		t.Fatalf("latency diff = delta %d threshold %d, want 20 and 300", out.DeltaMillis, out.ThresholdMillis)
	}
	if c, ok := r.Case(caseID); !ok || c.State != impl.CaseJudged {
		t.Fatalf("case state = %q ok=%v, want %q", c.State, ok, impl.CaseJudged)
	}
}

func TestJudgeTimeHit(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-time")
	caseID := set.Cases[0].ID
	runAndSubmit(t, r, caseID, impl.ChannelBaseline, "hello world", 120)
	runAndSubmit(t, r, caseID, impl.ChannelEcho, "hello world", 135)
	runAndSubmit(t, r, caseID, impl.ChannelTime, "hello world", 900)
	out, err := r.Judge(caseID)
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if !out.Hit || out.Channel != impl.ChannelTime {
		t.Fatalf("time hit got hit=%v channel=%q", out.Hit, out.Channel)
	}
	if out.Reason != impl.ReasonTimeHit || out.Confidence != impl.ConfidenceMedium {
		t.Fatalf("time hit reason=%q confidence=%q", out.Reason, out.Confidence)
	}
	if out.DeltaMillis != 780 || out.ThresholdMillis != 300 {
		t.Fatalf("latency diff = delta %d threshold %d, want 780 and 300", out.DeltaMillis, out.ThresholdMillis)
	}
	if out.EchoMatched {
		t.Fatalf("echo channel must not be marked as matched")
	}
}

func TestJudgeNoChannelHit(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-miss")
	caseID := set.Cases[0].ID
	runAndSubmit(t, r, caseID, impl.ChannelBaseline, "hello world", 120)
	runAndSubmit(t, r, caseID, impl.ChannelEcho, "hello world", 125)
	runAndSubmit(t, r, caseID, impl.ChannelTime, "hello world", 150)
	out, err := r.Judge(caseID)
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if out.Hit {
		t.Fatalf("a body without the marker and a flat latency must not hit")
	}
	if out.Reason != impl.ReasonNoHit {
		t.Fatalf("miss reason = %q, want %q", out.Reason, impl.ReasonNoHit)
	}
	if out.Channel != "" || out.Confidence != "" {
		t.Fatalf("miss must not report a channel or a confidence, got %q/%q", out.Channel, out.Confidence)
	}
	if out.DeltaMillis != 30 || out.ThresholdMillis != 300 {
		t.Fatalf("latency diff = delta %d threshold %d, want 30 and 300", out.DeltaMillis, out.ThresholdMillis)
	}
}

func TestAbortClosesCase(t *testing.T) {
	r := newRunner(12)
	set := planOne(t, r, "p-abort")
	caseID := set.Cases[0].ID
	if _, err := r.Run(caseID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := r.Abort(caseID, "   "); !errors.Is(err, impl.ErrAbortReason) {
		t.Fatalf("blank abort reason must fail with ErrAbortReason, got %v", err)
	}
	if err := r.Abort(caseID, "target became unstable"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := r.Abort(caseID, "second call"); err != nil {
		t.Fatalf("abort must be idempotent, got %v", err)
	}
	if _, err := r.Run(caseID); !errors.Is(err, impl.ErrCaseAborted) {
		t.Fatalf("run on an aborted case must fail with ErrCaseAborted, got %v", err)
	}
	if _, err := r.Judge(caseID); !errors.Is(err, impl.ErrCaseAborted) {
		t.Fatalf("judge on an aborted case must fail with ErrCaseAborted, got %v", err)
	}
	c, ok := r.Case(caseID)
	if !ok || c.State != impl.CaseAborted {
		t.Fatalf("case state = %q ok=%v, want %q", c.State, ok, impl.CaseAborted)
	}
}

func TestSnapshotRestoreKeepsProgress(t *testing.T) {
	opts := impl.Options{MaxAttemptsPerPoint: 6, MaxConcurrentPoints: 4, TotalDelayBudgetMillis: 60000}
	r := impl.NewInjectionOracleRunner(opts)
	set, err := r.Plan(pointWith("p-snap", echoPayload(), templatePayload()))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	judgedID := set.Cases[0].ID
	runAndSubmit(t, r, judgedID, impl.ChannelBaseline, "hello world", 120)
	runAndSubmit(t, r, judgedID, impl.ChannelEcho, "hello GSB-XYZ world", 130)
	runAndSubmit(t, r, judgedID, impl.ChannelTime, "hello world", 140)
	before, err := r.Judge(judgedID)
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	openID := set.Cases[1].ID
	runAndSubmit(t, r, openID, impl.ChannelBaseline, "hello world", 120)
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewInjectionOracleRunner(opts)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, err := restored.Judge(judgedID)
	if err != nil {
		t.Fatalf("judge after restore: %v", err)
	}
	if after.Hit != before.Hit || after.Channel != before.Channel || after.Reason != before.Reason {
		t.Fatalf("restored outcome = %+v, want %+v", after, before)
	}
	att, err := restored.Run(openID)
	if err != nil {
		t.Fatalf("run after restore: %v", err)
	}
	if att.Channel != impl.ChannelEcho || att.Ordinal != 2 {
		t.Fatalf("restored chain resumed at %q ordinal %d, want echo ordinal 2", att.Channel, att.Ordinal)
	}
	budget, err := restored.Budget(openID)
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if budget.AttemptsLeft != 2 {
		t.Fatalf("attempts left = %d, want 2 after four submitted attempts", budget.AttemptsLeft)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, err := restored.Run(openID); !errors.Is(err, impl.ErrAttemptPending) {
		t.Fatalf("failed restore must not touch live state, got %v", err)
	}
}
