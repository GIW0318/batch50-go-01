package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-45"
)

type fakeChain struct {
	mu        sync.Mutex
	values    map[string]string
	reject    map[string]bool
	rewritten map[string]bool
	calls     int
	varying   bool
	sent      []string
}

func newFakeChain() *fakeChain {
	return &fakeChain{values: map[string]string{}, reject: map[string]bool{}, rewritten: map[string]bool{}}
}

func (f *fakeChain) Send(layer, body string) (impl.LayerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sent = append(f.sent, layer)
	value := f.values[layer]
	if f.varying {
		value = fmt.Sprintf("%s-%d", value, f.calls)
	}
	return impl.LayerResult{Accepted: !f.reject[layer], Value: value, Rewritten: f.rewritten[layer]}, nil
}

func uniformChain() *fakeChain {
	f := newFakeChain()
	f.values["gateway"] = "role=user"
	f.values["auth"] = "role=user"
	f.values["service"] = "role=user"
	return f
}

func caseSet() []impl.Case {
	return []impl.Case{
		{CaseID: "esc-slash", Name: "escaped slash", Dimension: impl.DimensionEscape, Impact: impl.ImpactWrite, Body: `{"path":"a\/b"}`},
		{CaseID: "dup-key", Name: "duplicate key", Dimension: impl.DimensionDuplicateKey, Impact: impl.ImpactAuth, Body: `{"role":"user","role":"admin"}`},
		{CaseID: "big-int", Name: "big integer", Dimension: impl.DimensionNumberPrecision, Impact: impl.ImpactDisplay, Body: `{"id":9007199254740993}`},
	}
}

func differentialConfig() impl.Config {
	return impl.Config{MaxProbesPerTarget: 8, MaxObservations: 64, Layers: []string{"gateway", "auth", "service"}}
}

func newDifferential(t *testing.T, chain impl.Chain) (*impl.JsonParseDifferential, impl.Target) {
	t.Helper()
	d := impl.NewJsonParseDifferential(differentialConfig(), caseSet())
	if chain != nil {
		if err := d.SetChain(chain); err != nil {
			t.Fatalf("set chain: %v", err)
		}
	}
	target := impl.Target{TargetID: "api"}
	if err := d.AddTarget(target); err != nil {
		t.Fatalf("add target: %v", err)
	}
	return d, target
}

func escapeCase() impl.Case {
	for _, c := range caseSet() {
		if c.CaseID == "esc-slash" {
			return c
		}
	}
	panic("missing esc-slash case")
}

func TestCaseReturnsRegisteredPayload(t *testing.T) {
	d := impl.NewJsonParseDifferential(differentialConfig(), caseSet())
	payload, err := d.Case("esc-slash")
	if err != nil {
		t.Fatalf("case lookup: %v", err)
	}
	if payload.Dimension != impl.DimensionEscape {
		t.Fatalf("dimension = %q, want %q", payload.Dimension, impl.DimensionEscape)
	}
	if payload.Impact != impl.ImpactWrite {
		t.Fatalf("impact = %q, want %q", payload.Impact, impl.ImpactWrite)
	}
	if !strings.Contains(payload.Body, `\/`) {
		t.Fatalf("body = %q, want the escaped slash payload", payload.Body)
	}
	sum := sha256.Sum256([]byte(payload.Body))
	if payload.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want the SHA-256 of the body", payload.Digest)
	}
	if _, err := d.Case("no-such-case"); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("unknown case must fail with ErrUnknownCase, got %v", err)
	}
}

func TestProbeRejectsUnknownTargetAndCase(t *testing.T) {
	d := impl.NewJsonParseDifferential(differentialConfig(), caseSet())
	if err := d.SetChain(uniformChain()); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	if _, err := d.Probe(impl.Target{TargetID: "api"}, escapeCase()); !errors.Is(err, impl.ErrUnknownTarget) {
		t.Fatalf("probing an unregistered target must fail with ErrUnknownTarget, got %v", err)
	}
	if err := d.AddTarget(impl.Target{TargetID: "api"}); err != nil {
		t.Fatalf("add target: %v", err)
	}
	if _, err := d.Probe(impl.Target{TargetID: "api"}, impl.Case{CaseID: "ghost"}); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("probing an unregistered case must fail with ErrUnknownCase, got %v", err)
	}
	if _, err := d.Probe(impl.Target{TargetID: "api"}, impl.Case{}); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("probing an empty case must fail with ErrUnknownCase, got %v", err)
	}
}

func TestProbeRequiresChain(t *testing.T) {
	d, target := newDifferential(t, nil)
	if _, err := d.Probe(target, escapeCase()); !errors.Is(err, impl.ErrNoChain) {
		t.Fatalf("probing without a chain must fail with ErrNoChain, got %v", err)
	}
	if err := d.SetChain(nil); !errors.Is(err, impl.ErrNoChain) {
		t.Fatalf("attaching a nil chain must fail with ErrNoChain, got %v", err)
	}
	if got := len(d.Observations()); got != 0 {
		t.Fatalf("a rejected probe recorded %d observations", got)
	}
}

func TestProbeCollectsOneViewPerLayer(t *testing.T) {
	f := uniformChain()
	d, target := newDifferential(t, f)
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if obs.ObsID != "obs-1" {
		t.Fatalf("observation id = %q, want obs-1", obs.ObsID)
	}
	if len(obs.Views) != 3 {
		t.Fatalf("views = %d, want one per layer", len(obs.Views))
	}
	for i, want := range []string{"gateway", "auth", "service"} {
		if obs.Views[i].Layer != want {
			t.Fatalf("view %d layer = %q, want %q", i, obs.Views[i].Layer, want)
		}
	}
	if len(f.sent) != 3 || f.sent[0] != "gateway" || f.sent[2] != "service" {
		t.Fatalf("chain call order = %v, want gateway, auth, service", f.sent)
	}
	if got := len(d.Observations()); got != 1 {
		t.Fatalf("recorded observations = %d, want 1", got)
	}
}

func TestProbeEnforcesBudget(t *testing.T) {
	cfg := impl.Config{MaxProbesPerTarget: 2, MaxObservations: 16, Layers: []string{"gateway", "auth", "service"}}
	d := impl.NewJsonParseDifferential(cfg, caseSet())
	if err := d.SetChain(uniformChain()); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	target := impl.Target{TargetID: "api"}
	if err := d.AddTarget(target); err != nil {
		t.Fatalf("add target: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := d.Probe(target, escapeCase()); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	if got := d.BudgetLeft("api"); got != 0 {
		t.Fatalf("budget left = %d, want 0", got)
	}
	if _, err := d.Probe(target, escapeCase()); !errors.Is(err, impl.ErrBudgetExhausted) {
		t.Fatalf("a probe above the budget must fail with ErrBudgetExhausted, got %v", err)
	}
}

func TestCompareConsistentWhenLayersAgree(t *testing.T) {
	d, target := newDifferential(t, uniformChain())
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	verdict, err := d.Compare([]impl.Observation{obs})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if verdict.Level != impl.LevelConsistent {
		t.Fatalf("level = %q, want %q", verdict.Level, impl.LevelConsistent)
	}
	if verdict.Reason != impl.ReasonAgreement {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonAgreement)
	}
	if verdict.VerdictID != "verdict-1" {
		t.Fatalf("verdict id = %q, want verdict-1", verdict.VerdictID)
	}
	if verdict.CaseID != "esc-slash" {
		t.Fatalf("verdict case = %q, want esc-slash", verdict.CaseID)
	}
	if len(verdict.Diffs) != 0 {
		t.Fatalf("agreeing layers produced %d diffs, want none", len(verdict.Diffs))
	}
}

func TestCompareFlagsAcceptanceSplit(t *testing.T) {
	f := uniformChain()
	f.reject["gateway"] = true
	d, target := newDifferential(t, f)
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	verdict, err := d.Compare([]impl.Observation{obs})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if verdict.Level != impl.LevelHigh {
		t.Fatalf("level = %q, want %q when one layer accepts what another rejects", verdict.Level, impl.LevelHigh)
	}
	if verdict.Reason != impl.ReasonAcceptanceSplit {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonAcceptanceSplit)
	}
	if len(verdict.Diffs) == 0 {
		t.Fatalf("an acceptance split must carry at least one diff")
	}
	if verdict.Diffs[0].Field != "accepted" {
		t.Fatalf("first diff field = %q, want accepted", verdict.Diffs[0].Field)
	}
}

func TestCompareFlagsWriteImpactValueSplit(t *testing.T) {
	f := uniformChain()
	f.values["service"] = "role=admin"
	d, target := newDifferential(t, f)
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	verdict, err := d.Compare([]impl.Observation{obs})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if verdict.Level != impl.LevelHigh {
		t.Fatalf("level = %q, want %q for a value split on a write path", verdict.Level, impl.LevelHigh)
	}
	if verdict.Reason != impl.ReasonValueSplit {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonValueSplit)
	}
	if len(verdict.Diffs) == 0 || verdict.Diffs[0].Field != "value" {
		t.Fatalf("diffs = %+v, want a value diff", verdict.Diffs)
	}
	if verdict.Diffs[0].ValueA == verdict.Diffs[0].ValueB {
		t.Fatalf("value diff carries identical values: %+v", verdict.Diffs[0])
	}
}

func TestCompareMarksUnstableOnVaryingResults(t *testing.T) {
	f := uniformChain()
	f.varying = true
	d, target := newDifferential(t, f)
	first, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	second, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	verdict, err := d.Compare([]impl.Observation{first, second})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if verdict.Level != impl.LevelUncertain {
		t.Fatalf("level = %q, want %q when repeated probes disagree", verdict.Level, impl.LevelUncertain)
	}
	if verdict.Reason != impl.ReasonUnstable {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonUnstable)
	}
}

func TestCompareRejectsEmptyInput(t *testing.T) {
	d := impl.NewJsonParseDifferential(differentialConfig(), caseSet())
	if _, err := d.Compare(nil); !errors.Is(err, impl.ErrNoObservations) {
		t.Fatalf("an empty comparison must fail with ErrNoObservations, got %v", err)
	}
	chain := uniformChain()
	if err := d.SetChain(chain); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	target := impl.Target{TargetID: "api"}
	if err := d.AddTarget(target); err != nil {
		t.Fatalf("add target: %v", err)
	}
	esc, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe escape: %v", err)
	}
	bigInt := impl.Case{CaseID: "big-int", Name: "big integer", Dimension: impl.DimensionNumberPrecision, Impact: impl.ImpactDisplay, Body: `{"id":9007199254740993}`}
	big, err := d.Probe(target, bigInt)
	if err != nil {
		t.Fatalf("probe big int: %v", err)
	}
	if _, err := d.Compare([]impl.Observation{esc, big}); !errors.Is(err, impl.ErrMixedCases) {
		t.Fatalf("a comparison across cases must fail with ErrMixedCases, got %v", err)
	}
}

func TestExplainReturnsDiffs(t *testing.T) {
	f := uniformChain()
	f.values["auth"] = "role=admin"
	d, target := newDifferential(t, f)
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	verdict, err := d.Compare([]impl.Observation{obs})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	diffs, err := d.Explain(verdict.VerdictID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(diffs) != len(verdict.Diffs) {
		t.Fatalf("explained %d diffs, verdict carries %d", len(diffs), len(verdict.Diffs))
	}
	if len(diffs) == 0 {
		t.Fatalf("a value split must be explainable")
	}
	for i, diff := range diffs {
		if diff.LayerA == "" || diff.LayerB == "" {
			t.Fatalf("diff %d does not name both layers: %+v", i, diff)
		}
	}
	if _, err := d.Explain("verdict-404"); !errors.Is(err, impl.ErrUnknownVerdict) {
		t.Fatalf("unknown verdict must fail with ErrUnknownVerdict, got %v", err)
	}
}

func TestObservationAndVerdictLists(t *testing.T) {
	d, target := newDifferential(t, uniformChain())
	first, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := len(d.Observations()); got != 1 {
		t.Fatalf("observations = %d, want 1 after the first probe", got)
	}
	if _, err := d.Probe(target, escapeCase()); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if got := len(d.Observations()); got != 2 {
		t.Fatalf("observations = %d, want 2 after the second probe", got)
	}
	verdict, err := d.Compare([]impl.Observation{first})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	ids := d.Verdicts()
	if len(ids) != 1 || ids[0] != verdict.VerdictID {
		t.Fatalf("verdict list = %v, want [%s]", ids, verdict.VerdictID)
	}
	if got := len(d.Targets()); got != 1 || d.Targets()[0] != "api" {
		t.Fatalf("target list = %v, want [api]", d.Targets())
	}
}

func TestSnapshotRestoreKeepsVerdicts(t *testing.T) {
	f := uniformChain()
	f.values["service"] = "role=admin"
	d, target := newDifferential(t, f)
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	verdict, err := d.Compare([]impl.Observation{obs})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewJsonParseDifferential(differentialConfig(), caseSet())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ids := restored.Verdicts()
	if len(ids) != 1 || ids[0] != verdict.VerdictID {
		t.Fatalf("restored verdicts = %v, want [%s]", ids, verdict.VerdictID)
	}
	diffs, err := restored.Explain(verdict.VerdictID)
	if err != nil {
		t.Fatalf("explain after restore: %v", err)
	}
	if len(diffs) != len(verdict.Diffs) {
		t.Fatalf("restored diffs = %d, want %d", len(diffs), len(verdict.Diffs))
	}
	if got := restored.BudgetLeft("api"); got != d.BudgetLeft("api") {
		t.Fatalf("restored budget left = %d, want %d", got, d.BudgetLeft("api"))
	}
	if got := len(restored.Observations()); got != 1 {
		t.Fatalf("restored observations = %d, want 1", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	d, target := newDifferential(t, uniformChain())
	obs, err := d.Probe(target, escapeCase())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := d.Compare([]impl.Observation{obs}); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if err := d.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(d.Verdicts()); got != 1 {
		t.Fatalf("a rejected restore must keep the live verdicts, got %d", got)
	}
	if got := d.BudgetLeft("api"); got != 7 {
		t.Fatalf("probe budget after one probe = %d, want 7 out of the configured 8", got)
	}
}

func TestConcurrentProbesOnDistinctTargets(t *testing.T) {
	cfg := impl.Config{MaxProbesPerTarget: 4, MaxObservations: 32, Layers: []string{"gateway", "auth", "service"}}
	d := impl.NewJsonParseDifferential(cfg, caseSet())
	if err := d.SetChain(uniformChain()); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	targets := []impl.Target{{TargetID: "api-a"}, {TargetID: "api-b"}}
	for _, target := range targets {
		if err := d.AddTarget(target); err != nil {
			t.Fatalf("add target: %v", err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for _, target := range targets {
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(target impl.Target) {
				defer wg.Done()
				if _, err := d.Probe(target, escapeCase()); err != nil {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}(target)
		}
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent probes failed", failures)
	}
	if got := len(d.Observations()); got != 8 {
		t.Fatalf("recorded observations = %d, want 8", got)
	}
	for _, target := range targets {
		if got := d.BudgetLeft(target.TargetID); got != 0 {
			t.Fatalf("budget left for %s = %d, want 0", target.TargetID, got)
		}
	}
}
