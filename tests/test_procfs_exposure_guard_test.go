package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-d-09"
)

type testClock struct {
	at time.Time
}

func (c *testClock) now() time.Time          { return c.at }
func (c *testClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newExposureGuard() (*impl.ProcfsExposureGuard, *testClock) {
	c := &testClock{at: time.Unix(1700000000, 0)}
	return impl.NewProcfsExposureGuard(impl.Options{Now: c.now}), c
}

func appliedLevel(t *testing.T, g *impl.ProcfsExposureGuard, ns string, level int) {
	t.Helper()
	if err := g.SetPolicy(ns, impl.Policy{HideLevel: level}); err != nil {
		t.Fatalf("set policy %d on %s: %v", level, ns, err)
	}
	if err := g.Apply(ns); err != nil {
		t.Fatalf("apply %s: %v", ns, err)
	}
}

func exporterProcess() impl.Process {
	return impl.Process{PID: 4242, Owner: "2000", Exe: "/opt/node_exporter", Digest: "sha256:aa11"}
}

func TestApplyRequiresPolicy(t *testing.T) {
	g, _ := newExposureGuard()
	if err := g.Apply("prod"); !errors.Is(err, impl.ErrNoPolicy) {
		t.Fatalf("apply without a policy must fail with ErrNoPolicy, got %v", err)
	}
	if _, ok := g.MountedLevel("prod"); ok {
		t.Fatalf("nothing may be mounted without a policy")
	}
	if got := g.Namespaces(); len(got) != 0 {
		t.Fatalf("namespaces = %v, want none after a rejected apply", got)
	}
}

func TestApplyRecordsHideLevel(t *testing.T) {
	g, _ := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideOtherUsers)
	level, ok := g.MountedLevel("prod")
	if !ok || level != impl.HideOtherUsers {
		t.Fatalf("mounted level = %d ok=%v, want %d", level, ok, impl.HideOtherUsers)
	}
	policy, ok := g.Policy("prod")
	if !ok || policy.HideLevel != impl.HideOtherUsers {
		t.Fatalf("policy = %+v ok=%v, want hide level %d", policy, ok, impl.HideOtherUsers)
	}
	if got := g.Namespaces(); len(got) != 1 || got[0] != "prod" {
		t.Fatalf("namespaces = %v, want [prod]", got)
	}
}

func TestVisibleOpenPolicySeesEveryone(t *testing.T) {
	g, _ := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideNone)
	visible, err := g.Visible("prod", "1000", exporterProcess())
	if err != nil {
		t.Fatalf("visible: %v", err)
	}
	if !visible {
		t.Fatalf("hide level 0 must show every process")
	}
}

func TestVisibleHidesOtherOwnersAtLevelOne(t *testing.T) {
	g, _ := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideOtherUsers)
	other := exporterProcess()
	visible, err := g.Visible("prod", "1000", other)
	if err != nil {
		t.Fatalf("visible for another owner: %v", err)
	}
	if visible {
		t.Fatalf("hide level 1 must hide processes of another owner")
	}
	own := exporterProcess()
	own.Owner = "1000"
	visible, err = g.Visible("prod", "1000", own)
	if err != nil {
		t.Fatalf("visible for the same owner: %v", err)
	}
	if !visible {
		t.Fatalf("hide level 1 must keep the viewer's own processes visible")
	}
}

func TestVisibleRejectsUnknownNamespace(t *testing.T) {
	g, _ := newExposureGuard()
	if _, err := g.Visible("nope", "1000", exporterProcess()); !errors.Is(err, impl.ErrNoPolicy) {
		t.Fatalf("unknown namespace must fail with ErrNoPolicy, got %v", err)
	}
	if err := g.SetPolicy("staged", impl.Policy{HideLevel: impl.HideAll}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	if _, err := g.Visible("staged", "1000", exporterProcess()); !errors.Is(err, impl.ErrNotApplied) {
		t.Fatalf("a policy that was never applied must fail with ErrNotApplied, got %v", err)
	}
}

func TestSetPolicyRelaxNeedsFlag(t *testing.T) {
	g, _ := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	if err := g.SetPolicy("prod", impl.Policy{HideLevel: impl.HideOtherUsers}); !errors.Is(err, impl.ErrRelaxRequired) {
		t.Fatalf("loosening 2 to 1 must fail with ErrRelaxRequired, got %v", err)
	}
	policy, ok := g.Policy("prod")
	if !ok || policy.HideLevel != impl.HideAll {
		t.Fatalf("policy after the refused relax = %+v ok=%v, want hide level 2", policy, ok)
	}
	if err := g.SetPolicy("prod", impl.Policy{HideLevel: impl.HideOtherUsers, Relax: true}); err != nil {
		t.Fatalf("explicit relax must be accepted, got %v", err)
	}
	if err := g.Apply("prod"); err != nil {
		t.Fatalf("apply after relax: %v", err)
	}
	if level, ok := g.MountedLevel("prod"); !ok || level != impl.HideOtherUsers {
		t.Fatalf("mounted level = %d ok=%v, want 1 after the explicit relax", level, ok)
	}
}

func TestSetPolicyRejectsBadLevel(t *testing.T) {
	g, _ := newExposureGuard()
	for _, level := range []int{3, -1} {
		if err := g.SetPolicy("prod", impl.Policy{HideLevel: level}); !errors.Is(err, impl.ErrBadLevel) {
			t.Fatalf("hide level %d must fail with ErrBadLevel, got %v", level, err)
		}
	}
	if _, ok := g.Policy("prod"); ok {
		t.Fatalf("rejected policies must not be stored")
	}
	if err := g.SetPolicy("  ", impl.Policy{HideLevel: impl.HideNone}); !errors.Is(err, impl.ErrBadNamespace) {
		t.Fatalf("blank namespace must fail with ErrBadNamespace, got %v", err)
	}
}

func TestExemptGrantsVisibilityByDigest(t *testing.T) {
	g, clock := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	entry := impl.Exemption{
		Viewer:    "1000",
		Exe:       "/opt/node_exporter",
		Digest:    "sha256:aa11",
		PID:       700,
		ExpiresAt: clock.at.Add(time.Hour),
	}
	if err := g.Exempt("prod", entry); err != nil {
		t.Fatalf("exempt: %v", err)
	}
	visible, err := g.Visible("prod", "1000", exporterProcess())
	if err != nil {
		t.Fatalf("visible with an exemption: %v", err)
	}
	if !visible {
		t.Fatalf("an exemption bound to the executable digest must expose the process")
	}
	if got := g.Exemptions("prod"); len(got) != 1 || got[0].Digest != "sha256:aa11" {
		t.Fatalf("exemptions = %+v, want the registered digest", got)
	}
}

func TestExemptionExpires(t *testing.T) {
	g, clock := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	entry := impl.Exemption{
		Viewer:    "1000",
		Exe:       "/opt/node_exporter",
		Digest:    "sha256:aa11",
		PID:       700,
		ExpiresAt: clock.at.Add(time.Hour),
	}
	if err := g.Exempt("prod", entry); err != nil {
		t.Fatalf("exempt: %v", err)
	}
	clock.advance(2 * time.Hour)
	visible, err := g.Visible("prod", "1000", exporterProcess())
	if err != nil {
		t.Fatalf("visible after expiry: %v", err)
	}
	if visible {
		t.Fatalf("an expired exemption must stop exposing the process")
	}
}

func TestExemptionIsNamespaceScoped(t *testing.T) {
	g, clock := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	appliedLevel(t, g, "dev", impl.HideAll)
	entry := impl.Exemption{
		Viewer:    "1000",
		Exe:       "/opt/node_exporter",
		Digest:    "sha256:aa11",
		PID:       700,
		ExpiresAt: clock.at.Add(time.Hour),
	}
	if err := g.Exempt("prod", entry); err != nil {
		t.Fatalf("exempt in prod: %v", err)
	}
	visible, err := g.Visible("dev", "1000", exporterProcess())
	if err != nil {
		t.Fatalf("visible in dev: %v", err)
	}
	if visible {
		t.Fatalf("an exemption of prod must not leak into dev")
	}
}

func TestExemptionDroppedOnProcessExit(t *testing.T) {
	g, clock := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	entry := impl.Exemption{
		Viewer:    "1000",
		Exe:       "/opt/node_exporter",
		Digest:    "sha256:aa11",
		PID:       700,
		ExpiresAt: clock.at.Add(time.Hour),
	}
	if err := g.Exempt("prod", entry); err != nil {
		t.Fatalf("exempt: %v", err)
	}
	if err := g.Exit("prod", 700); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if visible, err := g.Visible("prod", "1000", exporterProcess()); err != nil || visible {
		t.Fatalf("exemption must die with its process, visible=%v err=%v", visible, err)
	}
	if err := g.Exit("prod", 700); err != nil {
		t.Fatalf("repeated exit must be idempotent, got %v", err)
	}
	if got := g.Exemptions("prod"); len(got) != 0 {
		t.Fatalf("live exemptions = %+v, want none after the exit", got)
	}
}

func TestExemptRejectsBadEntries(t *testing.T) {
	g, clock := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	good := impl.Exemption{
		Viewer:    "1000",
		Exe:       "/opt/node_exporter",
		Digest:    "sha256:aa11",
		PID:       700,
		ExpiresAt: clock.at.Add(time.Hour),
	}
	blankViewer := good
	blankViewer.Viewer = "  "
	blankDigest := good
	blankDigest.Digest = ""
	expired := good
	expired.ExpiresAt = clock.at
	cases := []struct {
		name  string
		entry impl.Exemption
	}{
		{"blank viewer", blankViewer},
		{"blank digest", blankDigest},
		{"past deadline", expired},
	}
	for _, c := range cases {
		if err := g.Exempt("prod", c.entry); !errors.Is(err, impl.ErrBadExemption) {
			t.Fatalf("%s must fail with ErrBadExemption, got %v", c.name, err)
		}
	}
	if err := g.Exempt("absent", good); !errors.Is(err, impl.ErrNoPolicy) {
		t.Fatalf("exempting an unknown namespace must fail with ErrNoPolicy, got %v", err)
	}
	if got := g.Exemptions("prod"); len(got) != 0 {
		t.Fatalf("rejected exemptions must not be stored, got %+v", got)
	}
}

func TestVerifyReportsUnappliedPolicyChange(t *testing.T) {
	g, _ := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideOtherUsers)
	diffs, err := g.Verify("prod")
	if err != nil {
		t.Fatalf("verify right after apply: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("diffs after apply = %+v, want none", diffs)
	}
	if err := g.SetPolicy("prod", impl.Policy{HideLevel: impl.HideAll}); err != nil {
		t.Fatalf("tighten policy: %v", err)
	}
	diffs, err = g.Verify("prod")
	if err != nil {
		t.Fatalf("verify after tightening: %v", err)
	}
	want := []impl.Diff{{Field: "hide_level", Want: "2", Got: "1"}}
	if len(diffs) != 1 || diffs[0] != want[0] {
		t.Fatalf("diffs = %+v, want %+v", diffs, want)
	}
	if err := g.Apply("prod"); err != nil {
		t.Fatalf("apply tightened policy: %v", err)
	}
	if diffs, err = g.Verify("prod"); err != nil || len(diffs) != 0 {
		t.Fatalf("diffs after re-apply = %+v err=%v, want none", diffs, err)
	}
}

func TestSnapshotRestoreKeepsState(t *testing.T) {
	g, clock := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideAll)
	entry := impl.Exemption{
		Viewer:    "1000",
		Exe:       "/opt/node_exporter",
		Digest:    "sha256:aa11",
		PID:       700,
		ExpiresAt: clock.at.Add(time.Hour),
	}
	if err := g.Exempt("prod", entry); err != nil {
		t.Fatalf("exempt: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewProcfsExposureGuard(impl.Options{Now: clock.now})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	policy, ok := restored.Policy("prod")
	if !ok || policy.HideLevel != impl.HideAll {
		t.Fatalf("restored policy = %+v ok=%v, want hide level 2", policy, ok)
	}
	if level, ok := restored.MountedLevel("prod"); !ok || level != impl.HideAll {
		t.Fatalf("restored mounted level = %d ok=%v, want 2", level, ok)
	}
	if got := restored.Exemptions("prod"); len(got) != 1 {
		t.Fatalf("restored exemptions = %+v, want one", got)
	}
	visible, err := restored.Visible("prod", "1000", exporterProcess())
	if err != nil || !visible {
		t.Fatalf("restored exemption must still expose the process, visible=%v err=%v", visible, err)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	g, _ := newExposureGuard()
	appliedLevel(t, g, "prod", impl.HideOtherUsers)
	if err := g.Restore([]byte("}{ not json")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if level, ok := g.MountedLevel("prod"); !ok || level != impl.HideOtherUsers {
		t.Fatalf("failed restore must not touch live state, level=%d ok=%v", level, ok)
	}
	visible, err := g.Visible("prod", "1000", exporterProcess())
	if err != nil {
		t.Fatalf("visible after a rejected restore: %v", err)
	}
	if visible {
		t.Fatalf("live policy must keep hiding another owner after a rejected restore")
	}
}
