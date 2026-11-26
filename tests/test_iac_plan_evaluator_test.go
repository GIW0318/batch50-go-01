package task

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-36"
)

func newEval() *impl.IacPlanEvaluator {
	return impl.NewIacPlanEvaluator(impl.Limits{MaxResources: 32, MaxFindings: 64})
}

func mustLoad(t *testing.T, e *impl.IacPlanEvaluator, plan string) impl.Snapshot {
	t.Helper()
	snap, err := e.Load([]byte(plan))
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	return snap
}

const modulePlan = `{
  "resources": [
    {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "create", "after": {"acl": "private"}}
  ],
  "modules": [
    {"name": "app", "count": 2, "resources": [
      {"address": "aws_ebs_volume.data", "type": "aws_ebs_volume", "action": "create", "after": {"encrypted": "true"}}
    ]}
  ]
}`

const violationPlan = `{
  "resources": [
    {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "update", "before": {"acl": "private"}, "after": {"acl": "public-read"}},
    {"address": "aws_security_group.edge", "type": "aws_security_group", "action": "create", "after": {"ingress": "0.0.0.0/0:22"}},
    {"address": "aws_db_instance.orders", "type": "aws_db_instance", "action": "update", "after": {"publicly_accessible": "true"}}
  ]
}`

func TestLoadRejectsMalformedDocuments(t *testing.T) {
	e := newEval()
	docs := []string{
		`{"resources": [`,
		`{}`,
		`{"resources": [{"address": "aws_s3_bucket.logs", "action": "create", "after": {"acl": "private"}}]}`,
		`{"resources": [{"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "explode"}]}`,
	}
	for _, doc := range docs {
		if _, err := e.Load([]byte(doc)); !errors.Is(err, impl.ErrBadPlan) {
			t.Fatalf("plan %q must fail with ErrBadPlan, got %v", doc, err)
		}
	}
	if got := e.Snapshots(); len(got) != 0 {
		t.Fatalf("a rejected plan must not be registered, got %v", got)
	}
}

func TestLoadExpandsModuleAddresses(t *testing.T) {
	e := newEval()
	snap := mustLoad(t, e, modulePlan)
	want := []string{
		"aws_s3_bucket.logs",
		"module.app[0].aws_ebs_volume.data",
		"module.app[1].aws_ebs_volume.data",
	}
	if len(snap.Resources) != len(want) {
		t.Fatalf("expanded resources = %d, want %d", len(snap.Resources), len(want))
	}
	for i, addr := range want {
		if snap.Resources[i].Address != addr {
			t.Fatalf("resource %d address = %q, want %q", i, snap.Resources[i].Address, addr)
		}
	}
	if len(snap.Digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex characters", snap.Digest)
	}
	if _, err := hex.DecodeString(snap.Digest); err != nil {
		t.Fatalf("digest %q must be hex: %v", snap.Digest, err)
	}
	if snap.ID == "" {
		t.Fatalf("a loaded plan must carry an identifier")
	}
	if got := e.Snapshots(); len(got) != 1 || got[0] != snap.ID {
		t.Fatalf("registered snapshots = %v, want [%s]", got, snap.ID)
	}
}

func TestLoadRejectsDuplicateAddress(t *testing.T) {
	e := newEval()
	plan := `{"resources": [
	  {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "create"},
	  {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "update"}
	]}`
	if _, err := e.Load([]byte(plan)); !errors.Is(err, impl.ErrBadPlan) {
		t.Fatalf("two resources on one address must fail with ErrBadPlan, got %v", err)
	}
}

func TestLoadEnforcesResourceCap(t *testing.T) {
	e := impl.NewIacPlanEvaluator(impl.Limits{MaxResources: 2, MaxFindings: 16})
	if _, err := e.Load([]byte(modulePlan)); !errors.Is(err, impl.ErrTooManyResources) {
		t.Fatalf("three expanded resources under a cap of two must fail with ErrTooManyResources, got %v", err)
	}
	if got := e.Snapshots(); len(got) != 0 {
		t.Fatalf("an oversized plan must not be registered, got %v", got)
	}
}

func TestEvaluateOrdersFindingsByRule(t *testing.T) {
	e := newEval()
	snap := mustLoad(t, e, violationPlan)
	findings, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	wantRules := []string{"R01", "R04", "R05"}
	wantPaths := []string{"acl", "ingress", "publicly_accessible"}
	if len(findings) != len(wantRules) {
		t.Fatalf("findings = %d, want %d: %+v", len(findings), len(wantRules), findings)
	}
	for i := range wantRules {
		if findings[i].RuleID != wantRules[i] {
			t.Fatalf("finding %d rule = %q, want %q", i, findings[i].RuleID, wantRules[i])
		}
		if findings[i].Path != wantPaths[i] {
			t.Fatalf("finding %d path = %q, want %q", i, findings[i].Path, wantPaths[i])
		}
		if findings[i].Verdict != impl.VerdictViolation {
			t.Fatalf("finding %d verdict = %q, want %q", i, findings[i].Verdict, impl.VerdictViolation)
		}
		if findings[i].ID == "" || findings[i].Address == "" {
			t.Fatalf("finding %d must carry an id and an address: %+v", i, findings[i])
		}
	}
	if findings[0].Severity != impl.SeverityCritical {
		t.Fatalf("a public bucket must be critical, got %q", findings[0].Severity)
	}
	again, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("second evaluate: %v", err)
	}
	if len(again) != len(findings) {
		t.Fatalf("a repeated evaluation must be idempotent, got %d then %d findings", len(findings), len(again))
	}
	if again[0].ID != findings[0].ID {
		t.Fatalf("a repeated evaluation must reuse the finding id %q, got %q", findings[0].ID, again[0].ID)
	}
}

func TestEvaluateMarksUnknownAttributeUncertain(t *testing.T) {
	e := newEval()
	plan := `{"resources": [{"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "update",
	  "before": {"acl": "private"}, "after": {"acl": "public-read"}, "unknown": ["acl"]}]}`
	snap := mustLoad(t, e, plan)
	findings, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Verdict != impl.VerdictUncertain {
		t.Fatalf("an unknown attribute must be uncertain, got %q", findings[0].Verdict)
	}
	if findings[0].RuleID != "R01" || findings[0].Path != "acl" {
		t.Fatalf("uncertain finding = %q on %q, want R01 on acl", findings[0].RuleID, findings[0].Path)
	}
}

func TestEvaluateIsolatesRuleErrors(t *testing.T) {
	e := newEval()
	plan := `{"resources": [
	  {"address": "aws_ebs_volume.data", "type": "aws_ebs_volume", "action": "create", "after": {"encrypted": "maybe"}},
	  {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "update", "before": {"acl": "private"}, "after": {"acl": "public-read"}}
	]}`
	snap := mustLoad(t, e, plan)
	findings, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(findings), findings)
	}
	if findings[0].RuleID != "R01" || findings[0].Verdict != impl.VerdictViolation {
		t.Fatalf("the bucket finding must stay a violation, got %q/%q", findings[0].RuleID, findings[0].Verdict)
	}
	if findings[1].RuleID != "R03" || findings[1].Verdict != impl.VerdictError {
		t.Fatalf("an unreadable boolean must be a rule error on R03, got %q/%q", findings[1].RuleID, findings[1].Verdict)
	}
}

func TestEvaluateRejectsUnregisteredSnapshot(t *testing.T) {
	e := newEval()
	if _, err := e.Evaluate(impl.Snapshot{ID: "snap-404"}); !errors.Is(err, impl.ErrNoSnapshot) {
		t.Fatalf("evaluating an unknown snapshot must fail with ErrNoSnapshot, got %v", err)
	}
}

func TestEvaluateEnforcesFindingCap(t *testing.T) {
	e := impl.NewIacPlanEvaluator(impl.Limits{MaxResources: 32, MaxFindings: 1})
	snap := mustLoad(t, e, violationPlan)
	if _, err := e.Evaluate(snap); !errors.Is(err, impl.ErrTooManyFindings) {
		t.Fatalf("three findings under a cap of one must fail with ErrTooManyFindings, got %v", err)
	}
}

func TestExplainDescribesFinding(t *testing.T) {
	e := newEval()
	snap := mustLoad(t, e, violationPlan)
	findings, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	tr, err := e.Explain(findings[1].ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if tr.FindingID != findings[1].ID || tr.RuleID != findings[1].RuleID {
		t.Fatalf("trace = %q/%q, want %q/%q", tr.FindingID, tr.RuleID, findings[1].ID, findings[1].RuleID)
	}
	if tr.Address != findings[1].Address || tr.Path != findings[1].Path {
		t.Fatalf("trace address/path = %q/%q, want %q/%q", tr.Address, tr.Path, findings[1].Address, findings[1].Path)
	}
	if len(tr.Steps) < 2 {
		t.Fatalf("trace steps = %v, want at least two recorded steps", tr.Steps)
	}
	if _, err := e.Explain("R01:nowhere#acl"); !errors.Is(err, impl.ErrNoFinding) {
		t.Fatalf("explaining an unknown finding must fail with ErrNoFinding, got %v", err)
	}
}

func TestDiffReportsAddedRemovedAndChangedPaths(t *testing.T) {
	e := newEval()
	prev := mustLoad(t, e, `{"resources": [
	  {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "create", "after": {"acl": "private"}},
	  {"address": "aws_ebs_volume.data", "type": "aws_ebs_volume", "action": "create", "after": {"encrypted": "true"}}
	]}`)
	next := mustLoad(t, e, `{"resources": [
	  {"address": "aws_s3_bucket.logs", "type": "aws_s3_bucket", "action": "update", "before": {"acl": "private"}, "after": {"acl": "public-read"}},
	  {"address": "aws_kms_key.cmk", "type": "aws_kms_key", "action": "create", "after": {"rotation": "false"}}
	]}`)
	deltas, err := e.Diff(prev, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	wantKind := []string{impl.DeltaRemoved, impl.DeltaAdded, impl.DeltaChanged}
	wantAddr := []string{"aws_ebs_volume.data", "aws_kms_key.cmk", "aws_s3_bucket.logs"}
	if len(deltas) != len(wantAddr) {
		t.Fatalf("deltas = %d, want %d: %+v", len(deltas), len(wantAddr), deltas)
	}
	for i := range wantAddr {
		if deltas[i].Address != wantAddr[i] || deltas[i].Kind != wantKind[i] {
			t.Fatalf("delta %d = %s/%s, want %s/%s", i, deltas[i].Address, deltas[i].Kind, wantAddr[i], wantKind[i])
		}
	}
	changed := deltas[2]
	if len(changed.Paths) != 1 || changed.Paths[0] != "acl" {
		t.Fatalf("changed paths = %v, want [acl]", changed.Paths)
	}
}

func TestDiffOfIdenticalSnapshotsIsEmpty(t *testing.T) {
	e := newEval()
	first := mustLoad(t, e, violationPlan)
	second := mustLoad(t, e, violationPlan)
	deltas, err := e.Diff(first, second)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(deltas) != 0 {
		t.Fatalf("deltas = %+v, want none", deltas)
	}
}

func TestSetRulesNarrowsEvaluation(t *testing.T) {
	e := newEval()
	if err := e.SetRules([]string{"R05"}); err != nil {
		t.Fatalf("set rules: %v", err)
	}
	active := e.Rules()
	if len(active) != 1 || active[0].ID != "R05" {
		t.Fatalf("active rules = %+v, want just R05", active)
	}
	snap := mustLoad(t, e, violationPlan)
	findings, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 1 || findings[0].RuleID != "R05" {
		t.Fatalf("findings = %+v, want just the R05 finding", findings)
	}
	if err := e.SetRules([]string{"R99"}); !errors.Is(err, impl.ErrUnknownRule) {
		t.Fatalf("an unknown rule id must fail with ErrUnknownRule, got %v", err)
	}
	if got := e.Rules(); len(got) != 1 || got[0].ID != "R05" {
		t.Fatalf("a rejected rule set must not change the active rules, got %+v", got)
	}
}

func TestSnapshotRestoreKeepsFindings(t *testing.T) {
	e := newEval()
	snap := mustLoad(t, e, violationPlan)
	other := mustLoad(t, e, modulePlan)
	findings, err := e.Evaluate(snap)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	data, err := e.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newEval()
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Snapshots(); len(got) != 2 || got[0] != snap.ID || got[1] != other.ID {
		t.Fatalf("restored snapshots = %v, want [%s %s]", got, snap.ID, other.ID)
	}
	stored, ok := restored.Findings(snap.ID)
	if !ok || len(stored) != len(findings) {
		t.Fatalf("restored findings = %+v, want %d entries", stored, len(findings))
	}
	if stored[0].ID != findings[0].ID {
		t.Fatalf("restored finding id = %q, want %q", stored[0].ID, findings[0].ID)
	}
	if _, err := restored.Explain(findings[0].ID); err != nil {
		t.Fatalf("restored trace must be readable: %v", err)
	}
	if err := restored.Restore([]byte("###not-a-snapshot###")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Snapshots(); len(got) != 2 {
		t.Fatalf("a rejected restore must not touch the live snapshots, got %v", got)
	}
}

func TestConcurrentEvaluationIsConsistent(t *testing.T) {
	e := newEval()
	snap := mustLoad(t, e, violationPlan)
	const workers = 8
	var wg sync.WaitGroup
	counts := make([]int, workers)
	firstIDs := make([]string, workers)
	failures := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			findings, err := e.Evaluate(snap)
			if err != nil {
				failures[idx] = err
				return
			}
			counts[idx] = len(findings)
			firstIDs[idx] = findings[0].ID
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if failures[i] != nil {
			t.Fatalf("worker %d failed: %v", i, failures[i])
		}
		if counts[i] != 3 {
			t.Fatalf("worker %d saw %d findings, want 3", i, counts[i])
		}
		if firstIDs[i] != firstIDs[0] {
			t.Fatalf("worker %d reported id %q, want the stable id %q", i, firstIDs[i], firstIDs[0])
		}
	}
}
