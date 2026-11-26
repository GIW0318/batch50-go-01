package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-27"
)

func testLimits() impl.Limits {
	return impl.Limits{MaxCasesPerTarget: 16, MaxTargets: 4}
}

func sampleToken(target string) impl.Token {
	return impl.Token{
		Target:    target,
		Alg:       "rs256",
		Header:    "eyJhbGciOiJSUzI1NiJ9",
		Payload:   "eyJzdWIiOiJ1LTEifQ",
		Signature: "c2lnbmF0dXJl",
		Claims:    []string{"exp", "iss", "aud"},
	}
}

func newMatrix(t *testing.T, p *impl.JwtConfusionProber, target string) impl.Matrix {
	t.Helper()
	m, err := p.Enumerate(sampleToken(target))
	if err != nil {
		t.Fatalf("enumerate %s: %v", target, err)
	}
	return m
}

func TestEnumerateRejectsBlankSample(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	if _, err := p.Enumerate(impl.Token{Alg: "hs256"}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("sample without a target must fail with ErrBadSample, got %v", err)
	}
	if _, err := p.Enumerate(impl.Token{Target: "t1"}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("sample without an algorithm must fail with ErrBadSample, got %v", err)
	}
	if got := len(p.Cases("t1")); got != 0 {
		t.Fatalf("a rejected sample must not build a matrix, got %d cases", got)
	}
}

func TestEnumerateBuildsStableCaseIDs(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	m := newMatrix(t, p, "t1")
	if m.Target != "t1" {
		t.Fatalf("matrix target = %q, want t1", m.Target)
	}
	want := []string{
		"t1/alg/none",
		"t1/alg/hs256",
		"t1/alg/rs256",
		"t1/claim/exp",
		"t1/claim/iss",
		"t1/claim/aud",
		"t1/sig/empty",
	}
	if len(m.Cases) != len(want) {
		t.Fatalf("matrix size = %d, want %d (%v)", len(m.Cases), len(want), want)
	}
	for i, id := range want {
		if m.Cases[i].ID != id {
			t.Fatalf("case %d id = %q, want %q", i, m.Cases[i].ID, id)
		}
	}
	if got := len(p.Cases("t1")); got != len(want) {
		t.Fatalf("stored case count = %d, want %d", got, len(want))
	}
}

func TestEnumerateEnforcesCaseLimit(t *testing.T) {
	p := impl.NewJwtConfusionProber(impl.Limits{MaxCasesPerTarget: 5, MaxTargets: 4})
	if _, err := p.Enumerate(sampleToken("t1")); !errors.Is(err, impl.ErrCaseLimit) {
		t.Fatalf("seven cases against a cap of five must fail with ErrCaseLimit, got %v", err)
	}
	if got := len(p.Cases("t1")); got != 0 {
		t.Fatalf("an over cap matrix must not be stored partially, got %d cases", got)
	}
	if done, total := p.Progress("t1"); done != 0 || total != 0 {
		t.Fatalf("progress on a target without cases = %d/%d, want 0/0", done, total)
	}
}

func TestEnumerateRejectsSecondBatch(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if _, err := p.Enumerate(sampleToken("t1")); !errors.Is(err, impl.ErrBatchInFlight) {
		t.Fatalf("a second batch on the same target must fail with ErrBatchInFlight, got %v", err)
	}
	if _, err := p.CleanupAll("t1"); err != nil {
		t.Fatalf("cleanup all: %v", err)
	}
	if _, err := p.Enumerate(sampleToken("t1")); err != nil {
		t.Fatalf("a fresh batch must be accepted after the old one is closed, got %v", err)
	}
}

func TestEnumerateEnforcesTargetLimit(t *testing.T) {
	p := impl.NewJwtConfusionProber(impl.Limits{MaxCasesPerTarget: 16, MaxTargets: 1})
	newMatrix(t, p, "t1")
	if _, err := p.Enumerate(sampleToken("t2")); !errors.Is(err, impl.ErrTargetLimit) {
		t.Fatalf("second tracked target must fail with ErrTargetLimit, got %v", err)
	}
	if err := p.Close("t1"); err != nil {
		t.Fatalf("close t1: %v", err)
	}
	if _, err := p.Enumerate(sampleToken("t2")); err != nil {
		t.Fatalf("a closed target must free its slot, got %v", err)
	}
	if err := p.Close("t1"); !errors.Is(err, impl.ErrUnknownTarget) {
		t.Fatalf("closing an untracked target must fail with ErrUnknownTarget, got %v", err)
	}
}

func TestCraftUnknownCaseFails(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if _, err := p.Craft("t1/alg/md5"); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("unknown case must fail with ErrUnknownCase, got %v", err)
	}
	if _, err := p.Craft(""); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("blank case id must fail with ErrUnknownCase, got %v", err)
	}
}

func TestCraftNoneCaseHasEmptySignature(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	none, err := p.Craft("t1/alg/none")
	if err != nil {
		t.Fatalf("craft none case: %v", err)
	}
	if none.Alg != "none" {
		t.Fatalf("none case algorithm = %q, want none", none.Alg)
	}
	if none.Signature != "" {
		t.Fatalf("none case must carry an empty signature, got %q", none.Signature)
	}
	forged, err := p.Craft("t1/alg/hs256")
	if err != nil {
		t.Fatalf("craft hs256 case: %v", err)
	}
	if forged.Alg != "hs256" {
		t.Fatalf("hs256 case algorithm = %q, want hs256", forged.Alg)
	}
	if forged.Signature == "" {
		t.Fatalf("hs256 case must carry a forged signature")
	}
	again, err := p.Craft("t1/alg/hs256")
	if err != nil {
		t.Fatalf("craft hs256 case twice: %v", err)
	}
	if again.Signature != forged.Signature || again.Alg != forged.Alg {
		t.Fatalf("crafting the same case twice must return the same token, got %+v then %+v", forged, again)
	}
}

func TestSubmitUnknownCaseFails(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Submit("t1/claim/nbf", impl.Response{Status: 200}); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("submitting an unknown case must fail with ErrUnknownCase, got %v", err)
	}
}

func TestSubmitTwiceRejected(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Submit("t1/claim/exp", impl.Response{Status: 200}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := p.Submit("t1/claim/exp", impl.Response{Status: 500}); !errors.Is(err, impl.ErrAlreadySubmitted) {
		t.Fatalf("second submit must fail with ErrAlreadySubmitted, got %v", err)
	}
}

func TestJudgeBeforeSubmitFails(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if _, err := p.Judge("t1/alg/none"); !errors.Is(err, impl.ErrNotSubmitted) {
		t.Fatalf("judging a case without a response must fail with ErrNotSubmitted, got %v", err)
	}
	if _, err := p.Judge("t1/alg/md5"); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("judging an unknown case must fail with ErrUnknownCase, got %v", err)
	}
}

func TestJudgeReportsHitWithEvidence(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Submit("t1/alg/none", impl.Response{Status: 200, Body: "account-list", Protected: true}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	out, err := p.Judge("t1/alg/none")
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if !out.Hit {
		t.Fatalf("a response from the protected path must count as a hit")
	}
	if out.CaseID != "t1/alg/none" {
		t.Fatalf("outcome case = %q, want t1/alg/none", out.CaseID)
	}
	if out.Evidence == "" {
		t.Fatalf("a hit must carry a non empty evidence type")
	}
	again, err := p.Judge("t1/alg/none")
	if err != nil {
		t.Fatalf("judge twice: %v", err)
	}
	if again.Hit != out.Hit || again.Evidence != out.Evidence {
		t.Fatalf("judging twice must be stable, got %+v then %+v", out, again)
	}
}

func TestJudgeReportsMissWithReason(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Submit("t1/claim/exp", impl.Response{Status: 401, Body: "unauthorized"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	out, err := p.Judge("t1/claim/exp")
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if out.Hit {
		t.Fatalf("a rejected response must not count as a hit")
	}
	if out.Reason == "" {
		t.Fatalf("a miss must carry a non empty reason")
	}
	if done, total := p.Progress("t1"); done != 1 || total != 7 {
		t.Fatalf("progress = %d/%d, want 1/7", done, total)
	}
}

func TestCleanupRequiresJudgement(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Cleanup("t1/alg/none"); !errors.Is(err, impl.ErrNotJudged) {
		t.Fatalf("cleaning an unjudged case must fail with ErrNotJudged, got %v", err)
	}
	if err := p.Submit("t1/alg/none", impl.Response{Status: 401}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := p.Judge("t1/alg/none"); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if err := p.Cleanup("t1/alg/none"); err != nil {
		t.Fatalf("cleanup a judged case: %v", err)
	}
	if err := p.Cleanup("t1/alg/none"); err != nil {
		t.Fatalf("cleanup must be idempotent, got %v", err)
	}
	if _, err := p.Craft("t1/alg/none"); !errors.Is(err, impl.ErrCaseClosed) {
		t.Fatalf("a cleaned case token must be closed, got %v", err)
	}
	if err := p.Cleanup("t1/alg/md5"); !errors.Is(err, impl.ErrUnknownCase) {
		t.Fatalf("cleaning an unknown case must fail with ErrUnknownCase, got %v", err)
	}
}

func TestSnapshotRestoreKeepsProgressAndVoidsTokens(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Submit("t1/claim/exp", impl.Response{Status: 200, Body: "account-list", Protected: true}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	want, err := p.Judge("t1/claim/exp")
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewJwtConfusionProber(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if done, total := restored.Progress("t1"); done != 1 || total != 7 {
		t.Fatalf("restored progress = %d/%d, want 1/7", done, total)
	}
	got, err := restored.Judge("t1/claim/exp")
	if err != nil {
		t.Fatalf("judge after restore: %v", err)
	}
	if got.Hit != want.Hit || got.Evidence != want.Evidence {
		t.Fatalf("restored outcome = %+v, want %+v", got, want)
	}
	if _, err := restored.Craft("t1/claim/iss"); !errors.Is(err, impl.ErrTokenVoid) {
		t.Fatalf("an unsubmitted token must be void after restore, got %v", err)
	}
	if got := len(restored.Cases("t1")); got != 7 {
		t.Fatalf("restored case table has %d entries, want 7", got)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	p := impl.NewJwtConfusionProber(testLimits())
	newMatrix(t, p, "t1")
	if err := p.Restore([]byte("not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(p.Cases("t1")); got != 7 {
		t.Fatalf("a rejected restore must not touch live cases, got %d", got)
	}
	if _, err := p.Craft("t1/sig/empty"); err != nil {
		t.Fatalf("live tokens must keep working after a rejected restore, got %v", err)
	}
}
