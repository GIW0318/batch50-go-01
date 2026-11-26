package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-15"
)

func livepatchLimits() impl.Limits {
	return impl.Limits{
		MaxStackDepth: 8,
		MaxHosts:      8,
		MaxPatches:    32,
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func livePatch(id string, prereqs ...string) impl.Patch {
	return impl.Patch{
		ID:        id,
		Prereqs:   prereqs,
		MinKernel: "5.15",
		MaxKernel: "6.4",
		BuildID:   "build-" + id,
	}
}

func registerHost(t *testing.T, g *impl.LivepatchConsistencyGuard, host, kernel string) {
	t.Helper()
	if err := g.RegisterHost(host, kernel, "build-"+host); err != nil {
		t.Fatalf("register host %q must succeed, got %v", host, err)
	}
}

func stackIDs(state impl.HostState) []string {
	out := make([]string, 0, len(state.Stack))
	for _, id := range state.Stack {
		out = append(out, string(id))
	}
	return out
}

func TestRegisterHostRejectsBlankKernel(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	if err := g.RegisterHost("node-1", "", "build-node-1"); !errors.Is(err, impl.ErrBlankHost) {
		t.Fatalf("register host without a kernel = %v, want ErrBlankHost", err)
	}
}

func TestRegisterRejectsBlankPatchID(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	if _, err := g.Register(impl.Patch{MinKernel: "5.15", MaxKernel: "6.4"}); !errors.Is(err, impl.ErrBlankPatch) {
		t.Fatalf("register patch without an id = %v, want ErrBlankPatch", err)
	}
}

func TestRegisterRejectsDuplicatePatch(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	if _, err := g.Register(livePatch("dup")); err != nil {
		t.Fatalf("first register must succeed, got %v", err)
	}
	if _, err := g.Register(livePatch("dup")); !errors.Is(err, impl.ErrPatchExists) {
		t.Fatalf("duplicate patch = %v, want ErrPatchExists", err)
	}
}

func TestRegisterRejectsSelfPrerequisite(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	if _, err := g.Register(livePatch("self", "self")); !errors.Is(err, impl.ErrCyclicPatch) {
		t.Fatalf("patch that depends on itself = %v, want ErrCyclicPatch", err)
	}
}

func TestPlanOrdersPrerequisiteChain(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	if _, err := g.Register(livePatch("gamma", "beta")); err != nil {
		t.Fatalf("register gamma must succeed, got %v", err)
	}
	if _, err := g.Register(livePatch("beta", "alpha")); err != nil {
		t.Fatalf("register beta must succeed, got %v", err)
	}
	if _, err := g.Register(livePatch("alpha")); err != nil {
		t.Fatalf("register alpha must succeed, got %v", err)
	}
	plan, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	if plan.ID == "" {
		t.Fatalf("plan id is empty")
	}
	if plan.Host != "node-1" {
		t.Fatalf("plan host = %q, want node-1", plan.Host)
	}
	if len(plan.Order) != 3 {
		t.Fatalf("plan order = %v, want three patches", plan.Order)
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, id := range plan.Order {
		if string(id) != want[i] {
			t.Fatalf("plan order = %v, want %v", plan.Order, want)
		}
	}
}

func TestPlanRejectsMissingPrerequisite(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	if _, err := g.Register(livePatch("orphan", "ghost")); err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if _, err := g.Plan("node-1"); !errors.Is(err, impl.ErrPrereqMissing) {
		t.Fatalf("plan with a missing prerequisite = %v, want ErrPrereqMissing", err)
	}
}

func TestPlanRejectsKernelOutOfRange(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	p := livePatch("narrow")
	p.MinKernel = "5.15"
	p.MaxKernel = "6.0"
	if _, err := g.Register(p); err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if _, err := g.Plan("node-1"); !errors.Is(err, impl.ErrKernelMismatch) {
		t.Fatalf("plan on an unsupported kernel = %v, want ErrKernelMismatch", err)
	}
}

func TestPlanRejectsConflictingPatches(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	first := livePatch("first")
	first.Conflicts = []string{"second"}
	second := livePatch("second")
	second.Conflicts = []string{"first"}
	if _, err := g.Register(first); err != nil {
		t.Fatalf("register first must succeed, got %v", err)
	}
	if _, err := g.Register(second); err != nil {
		t.Fatalf("register second must succeed, got %v", err)
	}
	if _, err := g.Plan("node-1"); !errors.Is(err, impl.ErrPatchConflict) {
		t.Fatalf("plan with conflicting patches = %v, want ErrPatchConflict", err)
	}
}

func TestApplyCommitsStack(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	for _, id := range []string{"alpha", "beta", "gamma"} {
		if _, err := g.Register(livePatch(id)); err != nil {
			t.Fatalf("register %q must succeed, got %v", id, err)
		}
	}
	plan, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	result, err := g.Apply(plan.ID)
	if err != nil {
		t.Fatalf("apply must succeed, got %v", err)
	}
	if result.PlanID != plan.ID || result.Host != "node-1" {
		t.Fatalf("result = %+v, want plan %s on node-1", result, plan.ID)
	}
	if len(result.Applied) != 3 || result.Depth != 3 {
		t.Fatalf("applied = %v depth = %d, want 3 and 3", result.Applied, result.Depth)
	}
	state, err := g.Status("node-1")
	if err != nil {
		t.Fatalf("status must be readable, got %v", err)
	}
	if got := stackIDs(state); len(got) != 3 {
		t.Fatalf("stack = %v, want three patches", got)
	}
	if state.Busy {
		t.Fatalf("host is still marked busy after apply")
	}
	hosts := g.Hosts()
	if len(hosts) != 1 || hosts[0] != "node-1" {
		t.Fatalf("hosts = %v, want [node-1]", hosts)
	}
}

func TestApplyRejectsConsumedPlan(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	if _, err := g.Register(livePatch("alpha")); err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	plan, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	if _, err := g.Apply(plan.ID); err != nil {
		t.Fatalf("first apply must succeed, got %v", err)
	}
	if _, err := g.Apply(plan.ID); !errors.Is(err, impl.ErrPlanConsumed) {
		t.Fatalf("second apply = %v, want ErrPlanConsumed", err)
	}
}

func TestApplyEnforcesStackDepth(t *testing.T) {
	limits := livepatchLimits()
	limits.MaxStackDepth = 2
	g := impl.NewLivepatchConsistencyGuard(limits)
	registerHost(t, g, "node-1", "6.1.0")
	for _, id := range []string{"one", "two", "three"} {
		if _, err := g.Register(livePatch(id)); err != nil {
			t.Fatalf("register %q must succeed, got %v", id, err)
		}
	}
	plan, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	if _, err := g.Apply(plan.ID); !errors.Is(err, impl.ErrStackLimit) {
		t.Fatalf("apply above the stack depth = %v, want ErrStackLimit", err)
	}
	state, err := g.Status("node-1")
	if err != nil {
		t.Fatalf("status must be readable, got %v", err)
	}
	if len(state.Stack) != 0 {
		t.Fatalf("stack after a refused apply = %v, want empty", stackIDs(state))
	}
}

func TestRollbackRestoresPreviousStack(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	registerHost(t, g, "node-1", "6.1.0")
	if _, err := g.Register(livePatch("alpha")); err != nil {
		t.Fatalf("register alpha must succeed, got %v", err)
	}
	first, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("first plan must succeed, got %v", err)
	}
	if _, err := g.Apply(first.ID); err != nil {
		t.Fatalf("first apply must succeed, got %v", err)
	}
	if _, err := g.Register(livePatch("beta")); err != nil {
		t.Fatalf("register beta must succeed, got %v", err)
	}
	second, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("second plan must succeed, got %v", err)
	}
	if _, err := g.Apply(second.ID); err != nil {
		t.Fatalf("second apply must succeed, got %v", err)
	}
	if err := g.Rollback(second.ID, "bad build"); err != nil {
		t.Fatalf("rollback must succeed, got %v", err)
	}
	state, err := g.Status("node-1")
	if err != nil {
		t.Fatalf("status must be readable, got %v", err)
	}
	got := stackIDs(state)
	if len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("stack after rollback = %v, want [alpha]", got)
	}
	if state.LastRollback != "bad build" {
		t.Fatalf("last rollback = %q, want bad build", state.LastRollback)
	}
}

func TestRollbackRejectsUnknownPlan(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	if err := g.Rollback("plan-never-made", "cleanup"); !errors.Is(err, impl.ErrUnknownPlan) {
		t.Fatalf("rollback unknown plan = %v, want ErrUnknownPlan", err)
	}
}

func TestSnapshotRestoreKeepsStack(t *testing.T) {
	limits := livepatchLimits()
	g := impl.NewLivepatchConsistencyGuard(limits)
	registerHost(t, g, "node-1", "6.1.0")
	if _, err := g.Register(livePatch("alpha", "beta")); err != nil {
		t.Fatalf("register alpha must succeed, got %v", err)
	}
	if _, err := g.Register(livePatch("beta")); err != nil {
		t.Fatalf("register beta must succeed, got %v", err)
	}
	plan, err := g.Plan("node-1")
	if err != nil {
		t.Fatalf("plan must succeed, got %v", err)
	}
	if _, err := g.Apply(plan.ID); err != nil {
		t.Fatalf("apply must succeed, got %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewLivepatchConsistencyGuard(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	state, err := restored.Status("node-1")
	if err != nil {
		t.Fatalf("restored status must be readable, got %v", err)
	}
	got := stackIDs(state)
	if len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
		t.Fatalf("restored stack = %v, want [beta alpha]", got)
	}
	if state.Kernel != "6.1.0" {
		t.Fatalf("restored kernel = %q, want 6.1.0", state.Kernel)
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	g := impl.NewLivepatchConsistencyGuard(livepatchLimits())
	if err := g.Restore([]byte("not a livepatch snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("restore garbage = %v, want ErrBadSnapshot", err)
	}
}
