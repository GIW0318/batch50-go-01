package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-18"
)

func sentinelConfig() impl.Config {
	return impl.Config{MaxEventsPerTarget: 32, MaxRules: 4, MaxChainSteps: 4, Window: 1000}
}

func attachEvent(ts int64) impl.Event {
	return impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindAttach, TS: ts, ParentPID: 1, TargetStart: 1700000000000}
}

func TestIngestValidatesEvent(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	bad := []impl.Event{
		{PID: 0, TargetPID: 4242, Kind: impl.KindExec, TS: 10},
		{PID: 900, TargetPID: 0, Kind: impl.KindExec, TS: 10},
		{PID: 900, TargetPID: 4242, Kind: "ptrace_poke", TS: 10},
		{PID: 900, TargetPID: 4242, Kind: impl.KindExec, TS: 0},
	}
	for _, ev := range bad {
		if _, err := s.Ingest(ev); !errors.Is(err, impl.ErrBadEvent) {
			t.Fatalf("ingest %+v = %v, want ErrBadEvent", ev, err)
		}
	}
	if _, err := s.Explain(4242); !errors.Is(err, impl.ErrNoTrace) {
		t.Fatalf("rejected events must not create a trace, got %v", err)
	}
	v, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindExec, TS: 10, TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("valid exec event: %v", err)
	}
	if v.Blocked {
		t.Fatalf("a lone exec event must not block the target: %+v", v)
	}
}

func TestIngestSingleAttachAlerts(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	v, err := s.Ingest(attachEvent(100))
	if err != nil {
		t.Fatalf("ingest attach: %v", err)
	}
	if v.Action != impl.ActionAlert || v.Reason != impl.ReasonChainAlert {
		t.Fatalf("attach verdict = %+v, want alert/chain_alert", v)
	}
	if v.Score != impl.ScoreAttach {
		t.Fatalf("attach score = %d, want %d", v.Score, impl.ScoreAttach)
	}
	if len(v.Chain) != 1 || v.Chain[0] != impl.KindAttach {
		t.Fatalf("attach chain = %v, want [%s]", v.Chain, impl.KindAttach)
	}
	if v.Blocked {
		t.Fatalf("a single attach must only alert")
	}
	if v.Seq != 1 {
		t.Fatalf("first verdict seq = %d, want 1", v.Seq)
	}
	if got := len(s.Blocked()); got != 0 {
		t.Fatalf("blocked list has %d entries after an alert, want 0", got)
	}
	tr, err := s.Explain(4242)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(tr.Events) != 1 || len(tr.Verdicts) != 1 {
		t.Fatalf("trace after one event = %+v, want one event and one verdict", tr)
	}
}

func TestIngestFullChainBlocksTarget(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	if _, err := s.Ingest(attachEvent(100)); err != nil {
		t.Fatalf("ingest attach: %v", err)
	}
	v, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindWriteVM, TS: 200, TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("ingest remote write: %v", err)
	}
	if v.Action != impl.ActionBlock || v.Reason != impl.ReasonChainBlock {
		t.Fatalf("chain verdict = %+v, want block/chain_block", v)
	}
	if v.Score != impl.ScoreAttach+impl.ScoreWriteVM {
		t.Fatalf("chain score = %d, want %d", v.Score, impl.ScoreAttach+impl.ScoreWriteVM)
	}
	if len(v.Chain) != 2 || v.Chain[0] != impl.KindAttach || v.Chain[1] != impl.KindWriteVM {
		t.Fatalf("chain = %v, want [attach write]", v.Chain)
	}
	if !v.Blocked {
		t.Fatalf("a chain over the block threshold must report Blocked")
	}
	if v.Seq != 2 {
		t.Fatalf("second verdict seq = %d, want 2", v.Seq)
	}
	blocked := s.Blocked()
	if len(blocked) != 1 || blocked[0] != 4242 {
		t.Fatalf("blocked list = %v, want [4242]", blocked)
	}
	late, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindRemoteThread, TS: 300, TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("ingest after the block: %v", err)
	}
	if late.Action != impl.ActionBlock || late.Reason != impl.ReasonPriorBlock {
		t.Fatalf("event on a blocked target = %+v, want block/prior_block", late)
	}
	if !late.Blocked {
		t.Fatalf("event on a blocked target must stay blocked")
	}
}

func TestIngestDuplicateEventIsIdempotent(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	first, err := s.Ingest(attachEvent(100))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := s.Ingest(attachEvent(100))
	if err != nil {
		t.Fatalf("repeated ingest: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("repeated event must be marked duplicate: %+v", second)
	}
	if second.Action != first.Action || second.Score != first.Score {
		t.Fatalf("duplicate verdict = %+v, want the original action and score %+v", second, first)
	}
	tr, err := s.Explain(4242)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(tr.Events) != 1 {
		t.Fatalf("duplicate event was recorded again, trace holds %d events", len(tr.Events))
	}
	third, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindExec, TS: 150, TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("distinct ingest: %v", err)
	}
	if third.Seq != 2 {
		t.Fatalf("duplicate consumed a verdict sequence, next seq = %d, want 2", third.Seq)
	}
}

func TestIngestExecBelowThresholdAllows(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	v, err := s.Ingest(impl.Event{PID: 900, TargetPID: 5151, Kind: impl.KindExec, TS: 100, TargetStart: 1700000000001})
	if err != nil {
		t.Fatalf("ingest exec: %v", err)
	}
	if v.Action != impl.ActionAllow || v.Reason != impl.ReasonScoreLow {
		t.Fatalf("exec verdict = %+v, want allow/score_low", v)
	}
	if v.Score != impl.ScoreExec {
		t.Fatalf("exec score = %d, want %d", v.Score, impl.ScoreExec)
	}
	if v.Blocked || len(v.Chain) != 1 || v.Chain[0] != impl.KindExec {
		t.Fatalf("exec verdict = %+v, want an unblocked single step chain", v)
	}
}

func TestAllowRuleGrantsException(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	id, err := s.Allow(impl.AllowRule{Name: "vendor debugger", ParentPID: 42, Signer: "vendor-x"})
	if err != nil {
		t.Fatalf("allow rule: %v", err)
	}
	if id != impl.RuleID("rule-1") {
		t.Fatalf("rule id = %q, want rule-1", id)
	}
	rule, ok := s.Rule("rule-1")
	if !ok || rule.Name != "vendor debugger" || rule.ParentPID != 42 || rule.Signer != "vendor-x" {
		t.Fatalf("stored rule = %+v ok=%v", rule, ok)
	}
	if got := s.Rules(); len(got) != 1 || got[0].ID != "rule-1" {
		t.Fatalf("rules = %+v, want rule-1", got)
	}
	v, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindAttach, TS: 100, ParentPID: 42, Signer: "vendor-x", TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("ingest exempted attach: %v", err)
	}
	if v.Action != impl.ActionAllow || v.Reason != impl.ReasonAllowRule {
		t.Fatalf("exempted attach = %+v, want allow/allow_rule", v)
	}
	if v.Blocked || v.Score != 0 {
		t.Fatalf("exempted attach = %+v, want unblocked with no score", v)
	}
	other, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4243, Kind: impl.KindAttach, TS: 100, ParentPID: 42, Signer: "vendor-y", TargetStart: 1700000000002})
	if err != nil {
		t.Fatalf("ingest other signer: %v", err)
	}
	if other.Action != impl.ActionAlert {
		t.Fatalf("attach from another signer = %+v, want a plain alert", other)
	}
}

func TestAllowValidationAndLimit(t *testing.T) {
	s := impl.NewInjectionSentinel(impl.Config{MaxEventsPerTarget: 8, MaxRules: 1, MaxChainSteps: 4, Window: 1000})
	bad := []impl.AllowRule{
		{Name: "  ", ParentPID: 42, Signer: "vendor-x"},
		{Name: "no parent", ParentPID: 0, Signer: "vendor-x"},
		{Name: "no signer", ParentPID: 42, Signer: ""},
		{Name: "negative expiry", ParentPID: 42, Signer: "vendor-x", ExpiresAt: -5},
	}
	for _, rule := range bad {
		if _, err := s.Allow(rule); !errors.Is(err, impl.ErrBadRule) {
			t.Fatalf("allow %+v = %v, want ErrBadRule", rule, err)
		}
	}
	if _, err := s.Allow(impl.AllowRule{Name: "first", ParentPID: 42, Signer: "vendor-x"}); err != nil {
		t.Fatalf("first rule: %v", err)
	}
	if _, err := s.Allow(impl.AllowRule{Name: "second", ParentPID: 43, Signer: "vendor-y"}); !errors.Is(err, impl.ErrRuleLimit) {
		t.Fatalf("second rule must fail with ErrRuleLimit, got %v", err)
	}
	if _, ok := s.Rule("rule-404"); ok {
		t.Fatalf("unknown rule id must not resolve")
	}
}

func TestSuppressDowngradesAlert(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	id, err := s.Allow(impl.AllowRule{Name: "vendor debugger", ParentPID: 42, Signer: "vendor-x"})
	if err != nil {
		t.Fatalf("allow rule: %v", err)
	}
	if err := s.Suppress(string(id), time.Hour); err != nil {
		t.Fatalf("suppress rule: %v", err)
	}
	v, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindAttach, TS: 100, ParentPID: 42, Signer: "vendor-x", TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("ingest suppressed attach: %v", err)
	}
	if v.Action != impl.ActionAllow || v.Reason != impl.ReasonSuppressed {
		t.Fatalf("suppressed verdict = %+v, want allow/suppressed", v)
	}
	if !v.Suppressed {
		t.Fatalf("suppressed verdict must carry the Suppressed mark")
	}
	if v.Score != impl.ScoreAttach || len(v.Chain) != 1 {
		t.Fatalf("suppressed verdict = %+v, want the event still scored", v)
	}
	if v.Blocked {
		t.Fatalf("suppressed alert must not block")
	}
}

func TestSuppressValidation(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	if err := s.Suppress("rule-404", time.Hour); !errors.Is(err, impl.ErrUnknownRule) {
		t.Fatalf("suppress of an unknown rule = %v, want ErrUnknownRule", err)
	}
	if _, err := s.Allow(impl.AllowRule{Name: "vendor debugger", ParentPID: 42, Signer: "vendor-x"}); err != nil {
		t.Fatalf("allow rule: %v", err)
	}
	if err := s.Suppress("rule-1", 0); !errors.Is(err, impl.ErrBadSuppression) {
		t.Fatalf("zero window = %v, want ErrBadSuppression", err)
	}
	if err := s.Suppress("rule-1", -time.Second); !errors.Is(err, impl.ErrBadSuppression) {
		t.Fatalf("negative window = %v, want ErrBadSuppression", err)
	}
}

func TestBufferKeepsNewestEvents(t *testing.T) {
	s := impl.NewInjectionSentinel(impl.Config{MaxEventsPerTarget: 3, MaxRules: 2, MaxChainSteps: 4, Window: 1000})
	kinds := []string{impl.KindExec, impl.KindAttach, impl.KindExec, impl.KindExec, impl.KindExec}
	for i, kind := range kinds {
		if _, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: kind, TS: int64(10 * (i + 1)), TargetStart: 1700000000000}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	tr, err := s.Explain(4242)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(tr.Events) != 3 {
		t.Fatalf("retained events = %d, want the cap 3", len(tr.Events))
	}
	wantTS := []int64{30, 40, 50}
	for i, want := range wantTS {
		if tr.Events[i].TS != want {
			t.Fatalf("retained event %d has ts %d, want the newest window %v", i, tr.Events[i].TS, wantTS)
		}
	}
	if len(tr.Verdicts) != 5 {
		t.Fatalf("verdict history = %d, want all 5 verdicts kept", len(tr.Verdicts))
	}
}

func TestExplainUnknownTarget(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	if _, err := s.Explain(7777); !errors.Is(err, impl.ErrNoTrace) {
		t.Fatalf("explain of an unseen target = %v, want ErrNoTrace", err)
	}
	if _, err := s.Ingest(attachEvent(100)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := s.Explain(7777); !errors.Is(err, impl.ErrNoTrace) {
		t.Fatalf("unrelated target must still be unknown, got %v", err)
	}
}

func TestVerdictHistoryGrowsInOrder(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	for i := 0; i < 3; i++ {
		if _, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindExec, TS: int64(10 * (i + 1)), TargetStart: 1700000000000}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	tr, err := s.Explain(4242)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(tr.Verdicts) != 3 {
		t.Fatalf("verdict history = %d, want 3", len(tr.Verdicts))
	}
	for i, v := range tr.Verdicts {
		if v.Seq != int64(i+1) {
			t.Fatalf("verdict %d seq = %d, want %d", i, v.Seq, i+1)
		}
		if v.Action != impl.ActionAllow {
			t.Fatalf("verdict %d action = %q, want allow", i, v.Action)
		}
	}
}

func TestConcurrentIngestBlocksOnce(t *testing.T) {
	s := impl.NewInjectionSentinel(impl.Config{MaxEventsPerTarget: 32, MaxRules: 2, MaxChainSteps: 4, Window: 1000})
	if _, err := s.Ingest(attachEvent(10)); err != nil {
		t.Fatalf("seed attach: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	blocks, priors := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindWriteVM, TS: int64(100 + i), TargetStart: 1700000000000})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("concurrent ingest %d: %v", i, err)
				return
			}
			switch v.Reason {
			case impl.ReasonChainBlock:
				blocks++
			case impl.ReasonPriorBlock:
				priors++
			default:
				t.Errorf("unexpected reason %q for concurrent ingest %d", v.Reason, i)
			}
		}(i)
	}
	wg.Wait()
	if blocks != 1 {
		t.Fatalf("concurrent chain steps produced %d block verdicts, want exactly 1", blocks)
	}
	if priors != 7 {
		t.Fatalf("concurrent chain steps produced %d prior block verdicts, want 7", priors)
	}
	if got := s.Blocked(); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("blocked list = %v, want [4242]", got)
	}
}

func TestSnapshotRestoreKeepsRulesBlocksAndSuppression(t *testing.T) {
	cfg := sentinelConfig()
	s := impl.NewInjectionSentinel(cfg)
	id, err := s.Allow(impl.AllowRule{Name: "vendor debugger", ParentPID: 42, Signer: "vendor-x"})
	if err != nil {
		t.Fatalf("allow rule: %v", err)
	}
	if err := s.Suppress(string(id), time.Hour); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if _, err := s.Ingest(attachEvent(100)); err != nil {
		t.Fatalf("seed attach: %v", err)
	}
	if _, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindWriteVM, TS: 200, TargetStart: 1700000000000}); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewInjectionSentinel(cfg)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := r.Rules(); len(got) != 1 || got[0].ID != "rule-1" {
		t.Fatalf("restored rules = %+v, want rule-1", got)
	}
	if blocked := r.Blocked(); len(blocked) != 1 || blocked[0] != 4242 {
		t.Fatalf("restored block list = %v, want [4242]", blocked)
	}
	suppressed, err := r.Ingest(impl.Event{PID: 901, TargetPID: 5151, Kind: impl.KindAttach, TS: 300, ParentPID: 42, Signer: "vendor-x", TargetStart: 1700000000003})
	if err != nil {
		t.Fatalf("ingest after restore: %v", err)
	}
	if !suppressed.Suppressed || suppressed.Action != impl.ActionAllow {
		t.Fatalf("restored suppression did not survive: %+v", suppressed)
	}
	prior, err := r.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindRemoteThread, TS: 400, TargetStart: 1700000000000})
	if err != nil {
		t.Fatalf("ingest blocked target after restore: %v", err)
	}
	if prior.Reason != impl.ReasonPriorBlock {
		t.Fatalf("restored block list did not survive: %+v", prior)
	}
	next, err := r.Allow(impl.AllowRule{Name: "second", ParentPID: 43, Signer: "vendor-y"})
	if err != nil {
		t.Fatalf("allow after restore: %v", err)
	}
	if next != impl.RuleID("rule-2") {
		t.Fatalf("rule id after restore = %q, want rule-2", next)
	}
}

func TestRestoreRejectsCorruptSnapshotKeepsState(t *testing.T) {
	s := impl.NewInjectionSentinel(sentinelConfig())
	if _, err := s.Allow(impl.AllowRule{Name: "vendor debugger", ParentPID: 42, Signer: "vendor-x"}); err != nil {
		t.Fatalf("allow rule: %v", err)
	}
	if _, err := s.Ingest(attachEvent(100)); err != nil {
		t.Fatalf("seed attach: %v", err)
	}
	if _, err := s.Ingest(impl.Event{PID: 900, TargetPID: 4242, Kind: impl.KindWriteVM, TS: 200, TargetStart: 1700000000000}); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := s.Restore([]byte("not a snapshot at all")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot = %v, want ErrBadSnapshot", err)
	}
	if got := s.Rules(); len(got) != 1 {
		t.Fatalf("failed restore changed the rule table, count = %d", len(got))
	}
	if got := s.Blocked(); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("failed restore changed the block list: %v", got)
	}
	tr, err := s.Explain(4242)
	if err != nil {
		t.Fatalf("trace must survive a rejected restore: %v", err)
	}
	if len(tr.Events) != 2 {
		t.Fatalf("trace holds %d events after the rejected restore, want 2", len(tr.Events))
	}
}
