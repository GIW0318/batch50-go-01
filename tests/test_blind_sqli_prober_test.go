package task

import (
	"errors"
	"reflect"
	"testing"

	impl "gsb/batch50-go-safety-c-23"
)

func baseProberOptions() impl.Options {
	return impl.Options{
		MaxRequestsPerPoint:   40,
		MaxConcurrentPoints:   4,
		MinBaselineSamples:    3,
		MaxPositions:          6,
		Alphabet:              "abcdef",
		TotalMillisBudget:     100000,
		ChangeSimilarityFloor: 90,
	}
}

func testParamPoint(id string) impl.ParamPoint {
	return impl.ParamPoint{ID: id, Name: "q", BaseURL: "https://shop.example/search", Method: "GET"}
}

func newScriptedProber(opts impl.Options, samples []impl.Sample) (*impl.BlindSqliProber, *int) {
	count := 0
	opts.Fetch = func(impl.ParamPoint, impl.Payload) (impl.Sample, error) {
		index := count
		count++
		if index >= len(samples) {
			return impl.Sample{}, errors.New("sample script exhausted")
		}
		return samples[index], nil
	}
	return impl.NewBlindSqliProber(opts), &count
}

func stableSamples(body string, millis int, n int) []impl.Sample {
	out := make([]impl.Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, impl.Sample{Body: body, ElapsedMillis: millis})
	}
	return out
}

func TestBaselineSamplesAndSigns(t *testing.T) {
	samples := stableSamples("Welcome", 200, 3)
	r, count := newScriptedProber(baseProberOptions(), samples)
	base, err := r.Baseline(testParamPoint("p-base"), 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if *count != 3 {
		t.Fatalf("fetch calls = %d, want exactly the requested three", *count)
	}
	if base.CaseID == "" || base.PointID != "p-base" {
		t.Fatalf("baseline identity mismatch: %+v", base)
	}
	if base.Signature != "Welcome" {
		t.Fatalf("signature = %q, want the stabilised body", base.Signature)
	}
	if !base.Stable || base.Samples != 3 {
		t.Fatalf("baseline stable=%v samples=%d, want stable with three samples", base.Stable, base.Samples)
	}
	if base.MeanMillis != 200 || base.JitterMillis != 0 {
		t.Fatalf("baseline timing = mean %d jitter %d, want 200 and 0", base.MeanMillis, base.JitterMillis)
	}
	points := r.Points()
	if len(points) != 1 || points[0] != "p-base" {
		t.Fatalf("points = %v, want [p-base]", points)
	}
}

func TestBaselineRejectsTooFewSamples(t *testing.T) {
	r, count := newScriptedProber(baseProberOptions(), stableSamples("Welcome", 200, 3))
	if _, err := r.Baseline(testParamPoint("p-few"), 2); !errors.Is(err, impl.ErrBadSampleCount) {
		t.Fatalf("two samples under a minimum of three must fail with ErrBadSampleCount, got %v", err)
	}
	if *count != 0 {
		t.Fatalf("rejected baseline must not sample, got %d fetch calls", *count)
	}
}

func TestBaselineRejectsUnstableBodies(t *testing.T) {
	samples := []impl.Sample{
		{Body: "Welcome to portal", ElapsedMillis: 200},
		{Body: "Error 500 page", ElapsedMillis: 210},
		{Body: "Welcome to portal", ElapsedMillis: 190},
	}
	r, count := newScriptedProber(baseProberOptions(), samples)
	if _, err := r.Baseline(testParamPoint("p-unstable"), 3); !errors.Is(err, impl.ErrBaselineUnstable) {
		t.Fatalf("disagreeing baseline bodies must fail with ErrBaselineUnstable, got %v", err)
	}
	if *count != 3 {
		t.Fatalf("fetch calls = %d, want 3 before stabilising fails", *count)
	}
	if got := len(r.Points()); got != 0 {
		t.Fatalf("unstable baseline must not register a case, got %d points", got)
	}
}

func TestProbeRequiresStableBaseline(t *testing.T) {
	r, count := newScriptedProber(baseProberOptions(), stableSamples("Welcome", 200, 3))
	payload := impl.Payload{Kind: impl.PayloadBoolean, Expression: "1=1", ProbeChar: "a"}
	if _, err := r.Probe(testParamPoint("p-nobase"), payload); !errors.Is(err, impl.ErrNoBaseline) {
		t.Fatalf("probing without a baseline must fail with ErrNoBaseline, got %v", err)
	}
	if *count != 0 {
		t.Fatalf("rejected probe must not sample, got %d fetch calls", *count)
	}
}

func TestProbeUnchangedBodyStaysFullSimilarity(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome", ElapsedMillis: 210})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-same")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	diff, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "1=1", ProbeChar: "a"})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if diff.CaseID != base.CaseID {
		t.Fatalf("diff case = %q, want %q", diff.CaseID, base.CaseID)
	}
	if diff.Changed {
		t.Fatalf("an unchanged body must not be reported as changed")
	}
	if diff.Similarity != 100 {
		t.Fatalf("similarity = %d, want 100", diff.Similarity)
	}
	if diff.Position != 1 {
		t.Fatalf("diff position = %d, want the first position", diff.Position)
	}
	if diff.MeanMillis != 200 || diff.DeltaMillis != 10 || diff.ThresholdMillis != 100 {
		t.Fatalf("timing = mean %d delta %d threshold %d, want 200/10/100", diff.MeanMillis, diff.DeltaMillis, diff.ThresholdMillis)
	}
	if diff.LatencyHit {
		t.Fatalf("a ten millisecond delta must not hit the latency channel")
	}
}

func TestProbeChangedBodyDetected(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome administrator", ElapsedMillis: 210})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-diff")
	if _, err := r.Baseline(point, 3); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	diff, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "1=2", ProbeChar: "b"})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !diff.Changed {
		t.Fatalf("a diverging body must be reported as changed")
	}
	if diff.Similarity >= 100 || diff.Similarity < 0 {
		t.Fatalf("similarity = %d, want a percentage below 100", diff.Similarity)
	}
}

func TestProbeTimeChannelHit(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome", ElapsedMillis: 900})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-time")
	if _, err := r.Baseline(point, 3); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	diff, err := r.Probe(point, impl.Payload{Kind: impl.PayloadTime, Expression: "dbms_pipe.receive_message", ProbeChar: "c", DelayMillis: 600})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !diff.LatencyHit {
		t.Fatalf("a seven hundred millisecond delta must hit the latency channel")
	}
	if diff.DeltaMillis != 700 {
		t.Fatalf("delta = %d, want 700", diff.DeltaMillis)
	}
	if diff.ThresholdMillis != 300 {
		t.Fatalf("threshold = %d, want half the declared delay", diff.ThresholdMillis)
	}
}

func TestProbeTimeChannelMiss(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome", ElapsedMillis: 240})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-slow")
	if _, err := r.Baseline(point, 3); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	diff, err := r.Probe(point, impl.Payload{Kind: impl.PayloadTime, Expression: "sleep", ProbeChar: "d", DelayMillis: 600})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if diff.LatencyHit {
		t.Fatalf("a forty millisecond delta must not hit the latency channel")
	}
	if diff.DeltaMillis != 40 || diff.ThresholdMillis != 300 {
		t.Fatalf("delta=%d threshold=%d, want 40 and 300", diff.DeltaMillis, diff.ThresholdMillis)
	}
}

func TestProbeRejectsBlankExpression(t *testing.T) {
	r, count := newScriptedProber(baseProberOptions(), stableSamples("Welcome", 200, 3))
	point := testParamPoint("p-blank")
	if _, err := r.Baseline(point, 3); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	before := *count
	if _, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "   ", ProbeChar: "a"}); !errors.Is(err, impl.ErrBadPayload) {
		t.Fatalf("blank expression must fail with ErrBadPayload, got %v", err)
	}
	if *count != before {
		t.Fatalf("rejected payload must not sample, got %d extra calls", *count-before)
	}
}

func TestProbeEnforcesRequestBudget(t *testing.T) {
	opts := baseProberOptions()
	opts.MaxRequestsPerPoint = 2
	samples := append(stableSamples("Welcome", 200, 3),
		impl.Sample{Body: "Welcome", ElapsedMillis: 205},
		impl.Sample{Body: "Welcome", ElapsedMillis: 206},
		impl.Sample{Body: "Welcome", ElapsedMillis: 207},
	)
	r, count := newScriptedProber(opts, samples)
	point := testParamPoint("p-budget")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	payload := impl.Payload{Kind: impl.PayloadBoolean, Expression: "1=1", ProbeChar: "a"}
	for i := 0; i < 2; i++ {
		if _, err := r.Probe(point, payload); err != nil {
			t.Fatalf("probe %d must stay inside the budget, got %v", i, err)
		}
	}
	if _, err := r.Probe(point, payload); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("third probe must fail with ErrBudgetExhausted, got %v", err)
	}
	if *count != 5 {
		t.Fatalf("fetch calls = %d, want 3 baseline plus 2 probes", *count)
	}
	budget, err := r.Budget(base.CaseID)
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if budget.RequestsLeft != 0 {
		t.Fatalf("requests left = %d, want 0", budget.RequestsLeft)
	}
}

func TestInferConfirmsCharacter(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome administrator", ElapsedMillis: 210})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-confirm")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "substr=1", ProbeChar: "a"}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	inference, err := r.Infer(base.CaseID)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if inference.Confirmed != "a" {
		t.Fatalf("confirmed = %q, want a", inference.Confirmed)
	}
	if inference.Position != 2 || inference.Extracted != "a" {
		t.Fatalf("position=%d extracted=%q, want 2 and a", inference.Position, inference.Extracted)
	}
	if len(inference.Candidates) != 6 {
		t.Fatalf("candidates after confirmation = %v, want the full alphabet for the next position", inference.Candidates)
	}
	if inference.Complete {
		t.Fatalf("a confirmed first character must not complete the case")
	}
}

func TestInferEliminatesCharacter(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome", ElapsedMillis: 200})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-elim")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "substr=1", ProbeChar: "b"}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	inference, err := r.Infer(base.CaseID)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if inference.Confirmed != "" || inference.Position != 1 || inference.Extracted != "" {
		t.Fatalf("a negative probe must not confirm anything, got %+v", inference)
	}
	want := []string{"a", "c", "d", "e", "f"}
	if !reflect.DeepEqual(inference.Candidates, want) {
		t.Fatalf("candidates = %v, want %v in alphabet order", inference.Candidates, want)
	}
}

func TestInferTimeHitConfirmsCharacter(t *testing.T) {
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome", ElapsedMillis: 900})
	r, _ := newScriptedProber(baseProberOptions(), samples)
	point := testParamPoint("p-timeinfer")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := r.Probe(point, impl.Payload{Kind: impl.PayloadTime, Expression: "sleep", ProbeChar: "c", DelayMillis: 600}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	inference, err := r.Infer(base.CaseID)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if inference.Confirmed != "c" || inference.Position != 2 || inference.Extracted != "c" {
		t.Fatalf("time channel confirmation = %+v, want c at position 2", inference)
	}
}

func TestStopFreezesCase(t *testing.T) {
	opts := baseProberOptions()
	r, _ := newScriptedProber(opts, append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome", ElapsedMillis: 200}))
	point := testParamPoint("p-stop")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if err := r.Stop(base.CaseID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := r.Stop(base.CaseID); err != nil {
		t.Fatalf("stop must be idempotent, got %v", err)
	}
	if _, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "1=1", ProbeChar: "a"}); !errors.Is(err, impl.ErrCaseStopped) {
		t.Fatalf("probe after stop must fail with ErrCaseStopped, got %v", err)
	}
	if _, err := r.Infer(base.CaseID); !errors.Is(err, impl.ErrCaseStopped) {
		t.Fatalf("infer after stop must fail with ErrCaseStopped, got %v", err)
	}
	budget, err := r.Budget(base.CaseID)
	if err != nil {
		t.Fatalf("budget after stop: %v", err)
	}
	if budget.RequestsLeft != opts.MaxRequestsPerPoint {
		t.Fatalf("requests left = %d, want the untouched cap %d", budget.RequestsLeft, opts.MaxRequestsPerPoint)
	}
	view, ok := r.Case(base.CaseID)
	if !ok || view.State != impl.StateStopped {
		t.Fatalf("case state = %q ok=%v, want %q", view.State, ok, impl.StateStopped)
	}
}

func TestSnapshotRestoreKeepsInference(t *testing.T) {
	opts := baseProberOptions()
	samples := append(stableSamples("Welcome", 200, 3), impl.Sample{Body: "Welcome administrator", ElapsedMillis: 210})
	r, _ := newScriptedProber(opts, samples)
	point := testParamPoint("p-snap")
	base, err := r.Baseline(point, 3)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := r.Probe(point, impl.Payload{Kind: impl.PayloadBoolean, Expression: "substr=1", ProbeChar: "a"}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := r.Infer(base.CaseID); err != nil {
		t.Fatalf("infer: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewBlindSqliProber(opts)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	inference, err := restored.Infer(base.CaseID)
	if err != nil {
		t.Fatalf("infer after restore: %v", err)
	}
	if inference.Confirmed != "a" || inference.Extracted != "a" || inference.Position != 2 {
		t.Fatalf("restored inference = %+v, want a confirmed at position 2", inference)
	}
	budget, err := restored.Budget(base.CaseID)
	if err != nil {
		t.Fatalf("budget after restore: %v", err)
	}
	if budget.RequestsLeft != opts.MaxRequestsPerPoint-1 {
		t.Fatalf("requests left = %d, want %d", budget.RequestsLeft, opts.MaxRequestsPerPoint-1)
	}
	if points := restored.Points(); len(points) != 1 || points[0] != "p-snap" {
		t.Fatalf("restored points = %v, want [p-snap]", points)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, err := restored.Infer(base.CaseID); err != nil {
		t.Fatalf("failed restore must not touch live state, got %v", err)
	}
}
