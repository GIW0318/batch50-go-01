package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-31"
)

func baseTime() time.Time {
	return time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
}

func testDictionary() []impl.DictionaryEntry {
	return []impl.DictionaryEntry{
		{Sandbox: "sb-a", API: "NtWriteFile", Topic: "file_write"},
		{Sandbox: "sb-b", API: "WriteFile", Topic: "file_write"},
		{Sandbox: "*", API: "open", Topic: "file_read"},
	}
}

func testLimits() impl.Limits {
	return impl.Limits{
		MaxReportsPerSample:   6,
		MaxBehaviorsPerReport: 8,
		MaxSamples:            4,
		MergeWindow:           30 * time.Second,
	}
}

func newNormalizer() *impl.SandboxReportNormalizer {
	return impl.NewSandboxReportNormalizer(testLimits(), testDictionary())
}

func newReport(id, sample, sandbox, verdict string, behaviors ...impl.Behavior) impl.Report {
	return impl.Report{
		ReportID:   id,
		SampleID:   sample,
		Sandbox:    sandbox,
		Verdict:    verdict,
		ReceivedAt: baseTime(),
		Behaviors:  behaviors,
	}
}

func TestIngestNormalizesReport(t *testing.T) {
	n := newNormalizer()
	got, err := n.Ingest(newReport("r-1", "sample-1", "sb-a", impl.VerdictMalicious,
		impl.Behavior{API: "NtWriteFile", Count: 2},
		impl.Behavior{API: "open", Count: 1},
	))
	if err != nil {
		t.Fatalf("ingest must accept a well formed report, got %v", err)
	}
	if got.SampleID != "sample-1" || got.ReportID != "r-1" || got.Sandbox != "sb-a" {
		t.Fatalf("normalised identity mismatch: %+v", got)
	}
	if len(got.Topics) != 2 {
		t.Fatalf("topic count = %d, want 2 (%+v)", len(got.Topics), got.Topics)
	}
	if got.Topics[0].Topic != "file_read" || got.Topics[0].Count != 1 || got.Topics[0].Sandboxes != 1 {
		t.Fatalf("first topic = %+v, want file_read count 1 from one sandbox", got.Topics[0])
	}
	if got.Topics[1].Topic != "file_write" || got.Topics[1].Count != 2 {
		t.Fatalf("second topic = %+v, want file_write count 2", got.Topics[1])
	}
	if pending := n.Pending("sample-1"); pending != 1 {
		t.Fatalf("pending reports = %d, want 1", pending)
	}
}

func TestIngestRejectsMalformedReports(t *testing.T) {
	n := newNormalizer()
	if _, err := n.Ingest(newReport("", "sample-2", "sb-a", impl.VerdictClean)); !errors.Is(err, impl.ErrEmptyReport) {
		t.Fatalf("blank report id must be rejected with ErrEmptyReport, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-2", "", "sb-a", impl.VerdictClean)); !errors.Is(err, impl.ErrEmptyReport) {
		t.Fatalf("blank sample id must be rejected with ErrEmptyReport, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-3", "sample-2", "", impl.VerdictClean)); !errors.Is(err, impl.ErrEmptyReport) {
		t.Fatalf("blank sandbox must be rejected with ErrEmptyReport, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-4", "sample-2", "sb-a", "maybe")); !errors.Is(err, impl.ErrBadVerdict) {
		t.Fatalf("unknown verdict must be rejected with ErrBadVerdict, got %v", err)
	}
	if pending := n.Pending("sample-2"); pending != 0 {
		t.Fatalf("rejected reports must not be held, pending = %d", pending)
	}
}

func TestIngestRejectsDuplicateReport(t *testing.T) {
	n := newNormalizer()
	if _, err := n.Ingest(newReport("r-dup", "sample-3", "sb-a", impl.VerdictMalicious)); err != nil {
		t.Fatalf("first ingest must succeed, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-dup", "sample-3", "sb-a", impl.VerdictMalicious)); !errors.Is(err, impl.ErrDuplicateReport) {
		t.Fatalf("duplicate report id must be rejected with ErrDuplicateReport, got %v", err)
	}
	if pending := n.Pending("sample-3"); pending != 1 {
		t.Fatalf("duplicate must not grow the in-flight set, pending = %d", pending)
	}
}

func TestIngestEnforcesReportLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxReportsPerSample = 2
	n := impl.NewSandboxReportNormalizer(limits, testDictionary())
	for _, id := range []string{"r-1", "r-2"} {
		if _, err := n.Ingest(newReport(id, "sample-4", "sb-a", impl.VerdictMalicious)); err != nil {
			t.Fatalf("ingest %s must succeed, got %v", id, err)
		}
	}
	if _, err := n.Ingest(newReport("r-3", "sample-4", "sb-b", impl.VerdictMalicious)); !errors.Is(err, impl.ErrReportLimit) {
		t.Fatalf("third report must hit the per sample limit, got %v", err)
	}
	if pending := n.Pending("sample-4"); pending != 2 {
		t.Fatalf("pending reports = %d, want the configured cap 2", pending)
	}
}

func TestIngestEnforcesBehaviorLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxBehaviorsPerReport = 3
	n := impl.NewSandboxReportNormalizer(limits, testDictionary())
	behaviors := []impl.Behavior{
		{API: "open", Count: 1},
		{API: "NtWriteFile", Count: 1},
		{API: "unknown_api", Count: 1},
		{API: "open", Count: 2},
	}
	if _, err := n.Ingest(newReport("r-1", "sample-5", "sb-a", impl.VerdictMalicious, behaviors...)); !errors.Is(err, impl.ErrBehaviorLimit) {
		t.Fatalf("oversized behaviour list must be rejected with ErrBehaviorLimit, got %v", err)
	}
	if pending := n.Pending("sample-5"); pending != 0 {
		t.Fatalf("rejected report must not be held, pending = %d", pending)
	}
	if samples := n.Samples(); len(samples) != 0 {
		t.Fatalf("rejected report must not create a sample, got %v", samples)
	}
}

func TestIngestEnforcesSampleLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxSamples = 1
	n := impl.NewSandboxReportNormalizer(limits, testDictionary())
	if _, err := n.Ingest(newReport("r-1", "sample-a", "sb-a", impl.VerdictClean)); err != nil {
		t.Fatalf("first sample must be accepted, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-2", "sample-b", "sb-a", impl.VerdictClean)); !errors.Is(err, impl.ErrSampleLimit) {
		t.Fatalf("second sample must hit the sample limit, got %v", err)
	}
	if samples := n.Samples(); len(samples) != 1 || samples[0] != "sample-a" {
		t.Fatalf("tracked samples = %v, want exactly sample-a", samples)
	}
}

func TestMergeWaitsForExpectedReports(t *testing.T) {
	n := newNormalizer()
	if err := n.Expect("sample-7", []string{"sb-a", "sb-b"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-1", "sample-7", "sb-a", impl.VerdictMalicious,
		impl.Behavior{API: "NtWriteFile", Count: 1},
	)); err != nil {
		t.Fatalf("ingest must succeed, got %v", err)
	}
	if _, err := n.Merge("sample-7"); !errors.Is(err, impl.ErrPendingConsensus) {
		t.Fatalf("merge with one sandbox missing must stay pending, got %v", err)
	}
	if _, err := n.Resolve("sample-7", "file_write"); !errors.Is(err, impl.ErrNoConsensus) {
		t.Fatalf("resolve before the consensus is published must return ErrNoConsensus, got %v", err)
	}
	state, ok := n.State("sample-7")
	if !ok {
		t.Fatalf("expected sample state to exist")
	}
	if state.Expected != 2 || state.Reports != 1 || state.Published {
		t.Fatalf("state = %+v, want 2 expected, 1 reported and not published", state)
	}
}

func TestMergeAggregatesTopicCounts(t *testing.T) {
	n := newNormalizer()
	if err := n.Expect("sample-8", []string{"sb-b", "sb-a"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-1", "sample-8", "sb-a", impl.VerdictMalicious,
		impl.Behavior{API: "NtWriteFile", Count: 2},
	)); err != nil {
		t.Fatalf("ingest sb-a: %v", err)
	}
	if _, err := n.Ingest(newReport("r-2", "sample-8", "sb-b", impl.VerdictMalicious,
		impl.Behavior{API: "WriteFile", Count: 3},
	)); err != nil {
		t.Fatalf("ingest sb-b: %v", err)
	}
	consensus, err := n.Merge("sample-8")
	if err != nil {
		t.Fatalf("merge with every expected sandbox present must publish, got %v", err)
	}
	if consensus.SampleID != "sample-8" || consensus.ReportCount != 2 {
		t.Fatalf("consensus identity = %+v, want sample-8 with 2 reports", consensus)
	}
	if len(consensus.Sandboxes) != 2 || consensus.Sandboxes[0] != "sb-a" || consensus.Sandboxes[1] != "sb-b" {
		t.Fatalf("sandboxes = %v, want ascending sb-a, sb-b", consensus.Sandboxes)
	}
	if len(consensus.Topics) != 1 {
		t.Fatalf("topics = %+v, want one merged bucket", consensus.Topics)
	}
	if consensus.Topics[0].Topic != "file_write" || consensus.Topics[0].Count != 5 || consensus.Topics[0].Sandboxes != 2 {
		t.Fatalf("merged topic = %+v, want file_write count 5 from two sandboxes", consensus.Topics[0])
	}
}

func TestMergePublishesOnWindowExpiry(t *testing.T) {
	limits := testLimits()
	limits.MergeWindow = 30 * time.Second
	n := impl.NewSandboxReportNormalizer(limits, testDictionary())
	now := baseTime()
	n.SetClock(func() time.Time { return now })
	if err := n.Expect("sample-9", []string{"sb-a", "sb-b"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-1", "sample-9", "sb-a", impl.VerdictClean,
		impl.Behavior{API: "open", Count: 1},
	)); err != nil {
		t.Fatalf("ingest must succeed, got %v", err)
	}
	now = baseTime().Add(31 * time.Second)
	consensus, err := n.Merge("sample-9")
	if err != nil {
		t.Fatalf("merge after the window must publish with the reports at hand, got %v", err)
	}
	if consensus.ReportCount != 1 || len(consensus.Sandboxes) != 1 || consensus.Sandboxes[0] != "sb-a" {
		t.Fatalf("partial consensus = %+v, want only sb-a", consensus)
	}
}

func TestUnknownSampleErrors(t *testing.T) {
	n := newNormalizer()
	if _, err := n.Merge("ghost"); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("merge of an unknown sample must return ErrUnknownSample, got %v", err)
	}
	if _, err := n.Resolve("ghost", "file_write"); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("resolve of an unknown sample must return ErrUnknownSample, got %v", err)
	}
	if _, err := n.Explain("ghost", "file_write"); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("explain of an unknown sample must return ErrUnknownSample, got %v", err)
	}
	if pending := n.Pending("ghost"); pending != 0 {
		t.Fatalf("unknown sample pending = %d, want 0", pending)
	}
}

func TestResolvePicksMajority(t *testing.T) {
	n := newNormalizer()
	if err := n.Expect("sample-11", []string{"sb-a", "sb-b", "sb-c", "sb-d"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	verdicts := map[string]string{
		"sb-a": impl.VerdictMalicious,
		"sb-b": impl.VerdictMalicious,
		"sb-c": impl.VerdictMalicious,
		"sb-d": impl.VerdictClean,
	}
	index := 0
	for _, sandbox := range []string{"sb-a", "sb-b", "sb-c", "sb-d"} {
		index++
		id := "r-" + sandbox
		if _, err := n.Ingest(newReport(id, "sample-11", sandbox, verdicts[sandbox],
			impl.Behavior{API: "NtWriteFile", Count: index},
		)); err != nil {
			t.Fatalf("ingest %s: %v", sandbox, err)
		}
	}
	if _, err := n.Merge("sample-11"); err != nil {
		t.Fatalf("merge must publish once all sandboxes reported, got %v", err)
	}
	decision, err := n.Resolve("sample-11", "file_write")
	if err != nil {
		t.Fatalf("resolve with a clear majority must succeed, got %v", err)
	}
	if decision.Outcome != impl.OutcomeMalicious {
		t.Fatalf("outcome = %q, want %q", decision.Outcome, impl.OutcomeMalicious)
	}
	if decision.Reason != impl.ReasonMajority {
		t.Fatalf("reason = %q, want %q", decision.Reason, impl.ReasonMajority)
	}
	if decision.Votes != 4 {
		t.Fatalf("votes = %d, want all four observing sandboxes", decision.Votes)
	}
	if decision.DecisionID == "" || decision.SampleID != "sample-11" || decision.Topic != "file_write" {
		t.Fatalf("decision identity = %+v", decision)
	}
}

func TestResolveTieFailsUnresolved(t *testing.T) {
	n := newNormalizer()
	if err := n.Expect("sample-12", []string{"sb-a", "sb-b"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-1", "sample-12", "sb-a", impl.VerdictMalicious,
		impl.Behavior{API: "NtWriteFile", Count: 1},
	)); err != nil {
		t.Fatalf("ingest sb-a: %v", err)
	}
	if _, err := n.Ingest(newReport("r-2", "sample-12", "sb-b", impl.VerdictClean,
		impl.Behavior{API: "WriteFile", Count: 1},
	)); err != nil {
		t.Fatalf("ingest sb-b: %v", err)
	}
	if _, err := n.Merge("sample-12"); err != nil {
		t.Fatalf("merge must publish, got %v", err)
	}
	if _, err := n.Resolve("sample-12", "file_write"); !errors.Is(err, impl.ErrUnresolved) {
		t.Fatalf("a one to one split must stay unresolved, got %v", err)
	}
}

func TestExplainReturnsSourcesSorted(t *testing.T) {
	n := newNormalizer()
	if err := n.Expect("sample-14", []string{"sb-c", "sb-a", "sb-b"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	for _, pair := range [][2]string{{"r-3", "sb-c"}, {"r-1", "sb-a"}, {"r-2", "sb-b"}} {
		if _, err := n.Ingest(newReport(pair[0], "sample-14", pair[1], impl.VerdictMalicious,
			impl.Behavior{API: "NtWriteFile", Count: 1},
		)); err != nil {
			t.Fatalf("ingest %s: %v", pair[0], err)
		}
	}
	if _, err := n.Merge("sample-14"); err != nil {
		t.Fatalf("merge must publish, got %v", err)
	}
	evidence, err := n.Explain("sample-14", "file_write")
	if err != nil {
		t.Fatalf("explain must succeed for a published topic, got %v", err)
	}
	if len(evidence.ReportIDs) != 3 || evidence.ReportIDs[0] != "r-1" || evidence.ReportIDs[1] != "r-2" || evidence.ReportIDs[2] != "r-3" {
		t.Fatalf("report ids = %v, want ascending r-1 r-2 r-3", evidence.ReportIDs)
	}
	if len(evidence.Sandboxes) != 3 || evidence.Sandboxes[0] != "sb-a" || evidence.Sandboxes[2] != "sb-c" {
		t.Fatalf("sandboxes = %v, want ascending sb-a..sb-c", evidence.Sandboxes)
	}
	if evidence.DecisionID == "" || evidence.SampleID != "sample-14" || evidence.Topic != "file_write" {
		t.Fatalf("evidence identity = %+v", evidence)
	}
}

func TestSnapshotRestorePendingMerge(t *testing.T) {
	n := newNormalizer()
	if err := n.Expect("sample-15", []string{"sb-a", "sb-b"}); err != nil {
		t.Fatalf("expect must be accepted, got %v", err)
	}
	if _, err := n.Ingest(newReport("r-1", "sample-15", "sb-a", impl.VerdictMalicious,
		impl.Behavior{API: "NtWriteFile", Count: 4},
	)); err != nil {
		t.Fatalf("ingest must succeed, got %v", err)
	}
	blob, err := n.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewSandboxReportNormalizer(testLimits(), testDictionary())
	if err := restored.Restore(blob); err != nil {
		t.Fatalf("restore must accept a snapshot it produced, got %v", err)
	}
	if pending := restored.Pending("sample-15"); pending != 1 {
		t.Fatalf("restored pending = %d, want 1", pending)
	}
	state, ok := restored.State("sample-15")
	if !ok || state.Expected != 2 || state.Published {
		t.Fatalf("restored state = %+v ok=%v, want unpublished with 2 expected", state, ok)
	}
	if _, err := restored.Merge("sample-15"); !errors.Is(err, impl.ErrPendingConsensus) {
		t.Fatalf("restored merge must still wait for the missing sandbox, got %v", err)
	}
	if err := restored.Restore([]byte("not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if pending := restored.Pending("sample-15"); pending != 1 {
		t.Fatalf("failed restore must keep the previous state, pending = %d", pending)
	}
}

func TestConcurrentIngestRespectsReportLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxReportsPerSample = 5
	limits.MaxSamples = 10
	n := impl.NewSandboxReportNormalizer(limits, testDictionary())
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, limited := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "r-" + string(rune('a'+i))
			_, err := n.Ingest(newReport(id, "sample-16", "sb-a", impl.VerdictMalicious,
				impl.Behavior{API: "NtWriteFile", Count: 1},
			))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, impl.ErrReportLimit):
				limited++
			}
		}(i)
	}
	wg.Wait()
	if accepted != 5 {
		t.Fatalf("accepted reports = %d, want exactly the configured cap 5", accepted)
	}
	if limited != 15 {
		t.Fatalf("limited reports = %d, want 15", limited)
	}
	if pending := n.Pending("sample-16"); pending != 5 {
		t.Fatalf("pending reports = %d, want 5", pending)
	}
}
