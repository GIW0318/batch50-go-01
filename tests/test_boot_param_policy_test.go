package task

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-06"
)

func policyLimits() impl.PolicyLimits {
	return impl.PolicyLimits{MaxParams: 12, MaxValueLen: 32, MaxGenerations: 3, BatchSize: 2}
}

func newPolicy(t *testing.T) *impl.BootParamPolicy {
	t.Helper()
	return impl.NewBootParamPolicy(policyLimits())
}

func register(t *testing.T, p *impl.BootParamPolicy, nodeID, role string) {
	t.Helper()
	if err := p.RegisterNode(nodeID, role); err != nil {
		t.Fatalf("register %s as %s: %v", nodeID, role, err)
	}
}

func baseSet(t *testing.T, p *impl.BootParamPolicy, nodeID string) impl.ParamSet {
	t.Helper()
	set, err := p.Baseline(nodeID)
	if err != nil {
		t.Fatalf("baseline for %s: %v", nodeID, err)
	}
	return set
}

func setOf(pairs ...string) impl.ParamSet {
	if len(pairs)%2 != 0 {
		panic("setOf needs key value pairs")
	}
	entries := make([]impl.Param, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		entries = append(entries, impl.Param{Key: pairs[i], Value: pairs[i+1]})
	}
	return impl.ParamSet{Entries: entries}
}

func valueOf(t *testing.T, set impl.ParamSet, key string) string {
	t.Helper()
	v, ok := set.Get(key)
	if !ok {
		t.Fatalf("parameter %q missing from %+v", key, set.Entries)
	}
	return v
}

func computeTarget(level, panicValue, hugepage string) impl.ParamSet {
	return setOf(
		impl.ParamAudit, "on",
		impl.ParamConsoleLevel, level,
		impl.ParamIOMMU, "strict",
		impl.ParamMitigations, "auto",
		impl.ParamNUMABalancing, "1",
		impl.ParamPanic, panicValue,
		impl.ParamRescueShell, "0",
		impl.ParamTHP, hugepage,
	)
}

func TestBaselineUsesRoleAllowList(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-compute-1", impl.RoleCompute)
	set := baseSet(t, p, "node-compute-1")
	if len(set.Entries) != 8 {
		t.Fatalf("baseline entries = %d, want the 8 role parameters", len(set.Entries))
	}
	want := map[string]string{
		impl.ParamAudit:         "on",
		impl.ParamConsoleLevel:  "3",
		impl.ParamIOMMU:         "strict",
		impl.ParamMitigations:   "auto",
		impl.ParamNUMABalancing: "1",
		impl.ParamPanic:         "10",
		impl.ParamRescueShell:   "0",
		impl.ParamTHP:           "madvise",
	}
	for key, expected := range want {
		if got := valueOf(t, set, key); got != expected {
			t.Fatalf("%s = %q, want %q", key, got, expected)
		}
	}
	keys := set.Keys()
	if len(keys) != len(set.Entries) {
		t.Fatalf("Keys() returned %d names for %d entries", len(keys), len(set.Entries))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("baseline keys must be sorted ascending, got %v", keys)
		}
	}
}

func TestBaselineUnknownNodeAndBadRoleFail(t *testing.T) {
	p := newPolicy(t)
	if _, err := p.Baseline("node-missing"); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("baseline of an unregistered node must fail with ErrUnknownNode, got %v", err)
	}
	if err := p.RegisterNode("node-x", "edge"); !errors.Is(err, impl.ErrBadRole) {
		t.Fatalf("unknown role must fail with ErrBadRole, got %v", err)
	}
	if err := p.RegisterNode("node-x", impl.RoleControl); err != nil {
		t.Fatalf("valid role must register: %v", err)
	}
	if _, err := p.Baseline("node-x"); err != nil {
		t.Fatalf("registered node must expose a baseline: %v", err)
	}
}

func TestSetRoleReevaluatesBaseline(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-store-1", impl.RoleStorage)
	if got := valueOf(t, baseSet(t, p, "node-store-1"), impl.ParamIOMMU); got != "on" {
		t.Fatalf("storage baseline iommu = %q, want on", got)
	}
	if err := p.SetRole("node-store-1", impl.RoleCompute); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if got := valueOf(t, baseSet(t, p, "node-store-1"), impl.ParamIOMMU); got != "strict" {
		t.Fatalf("compute baseline iommu = %q, want strict", got)
	}
	if err := p.SetRole("node-store-1", "edge"); !errors.Is(err, impl.ErrBadRole) {
		t.Fatalf("unknown role on SetRole must fail with ErrBadRole, got %v", err)
	}
	if got := valueOf(t, baseSet(t, p, "node-store-1"), impl.ParamIOMMU); got != "strict" {
		t.Fatalf("failed SetRole must keep the previous role, iommu = %q", got)
	}
	if err := p.SetRole("node-missing", impl.RoleCompute); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("SetRole on an unregistered node must fail with ErrUnknownNode, got %v", err)
	}
}

func TestPlanReportsAddAndRemove(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c1", impl.RoleCompute)
	target := setOf(
		impl.ParamAudit, "on",
		impl.ParamConsoleLevel, "4",
		impl.ParamIOMMU, "strict",
		impl.ParamMitigations, "auto",
		impl.ParamNUMABalancing, "1",
		impl.ParamPanic, "5",
		impl.ParamRescueShell, "0",
	)
	plan, err := p.Plan("node-c1", target)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.NodeID != "node-c1" {
		t.Fatalf("plan node = %q, want node-c1", plan.NodeID)
	}
	if plan.Generation != 0 {
		t.Fatalf("plan generation = %d, want the current generation 0", plan.Generation)
	}
	if plan.Fingerprint == "" {
		t.Fatalf("plan must carry a fingerprint of the resulting set")
	}
	if len(plan.Add) != 2 {
		t.Fatalf("plan add = %+v, want console_loglevel and panic", plan.Add)
	}
	if plan.Add[0].Key != impl.ParamConsoleLevel || plan.Add[0].Value != "4" {
		t.Fatalf("first add = %+v, want console_loglevel=4", plan.Add[0])
	}
	if plan.Add[1].Key != impl.ParamPanic || plan.Add[1].Value != "5" {
		t.Fatalf("second add = %+v, want panic=5", plan.Add[1])
	}
	if len(plan.Remove) != 1 || plan.Remove[0] != impl.ParamTHP {
		t.Fatalf("plan remove = %v, want [transparent_hugepage]", plan.Remove)
	}
	if plan.Role != impl.RoleCompute {
		t.Fatalf("plan role = %q, want %q", plan.Role, impl.RoleCompute)
	}
}

func TestPlanRejectsForbiddenParams(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c2", impl.RoleCompute)
	if _, err := p.Plan("node-c2", baseSet(t, p, "node-c2")); err != nil {
		t.Fatalf("the role baseline itself must be plannable, got %v", err)
	}
	cases := []struct {
		name string
		set  impl.ParamSet
	}{
		{"rescue shell on", setOf(impl.ParamRescueShell, "1")},
		{"iommu off", setOf(impl.ParamIOMMU, "off")},
		{"audit off", setOf(impl.ParamAudit, "0")},
		{"mitigations off", setOf(impl.ParamMitigations, "off")},
		{"blank blacklist", setOf(impl.ParamModuleBlacklist, "  ")},
		{"blacklist storage driver", setOf(impl.ParamModuleBlacklist, "dm_mod nvme")},
		{"verbose console", setOf(impl.ParamConsoleLevel, "9")},
	}
	for _, tc := range cases {
		if _, err := p.Plan("node-c2", tc.set); !errors.Is(err, impl.ErrForbiddenParam) {
			t.Fatalf("%s: want ErrForbiddenParam, got %v", tc.name, err)
		}
	}
}

func TestPlanEnforcesParamLimits(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c3", impl.RoleCompute)
	longValue := strings.Repeat("x", 33)
	if _, err := p.Plan("node-c3", setOf(impl.ParamModuleBlacklist, longValue)); !errors.Is(err, impl.ErrParamLimit) {
		t.Fatalf("33 byte value with a 32 byte cap must fail with ErrParamLimit, got %v", err)
	}
	pairs := make([]string, 0, 26)
	for i := 0; i < 13; i++ {
		pairs = append(pairs, fmt.Sprintf("extra_%02d", i), "1")
	}
	if _, err := p.Plan("node-c3", setOf(pairs...)); !errors.Is(err, impl.ErrParamLimit) {
		t.Fatalf("13 entries with a 12 entry cap must fail with ErrParamLimit, got %v", err)
	}
	if _, err := p.Plan("node-c3", setOf(impl.ParamModuleBlacklist, strings.Repeat("y", 32))); err != nil {
		t.Fatalf("value of exactly 32 bytes must be accepted, got %v", err)
	}
}

func TestApplyWritesGenerationAndDefersColdParams(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c4", impl.RoleCompute)
	plan, err := p.Plan("node-c4", computeTarget("4", "7", "never"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := p.Apply("node-c4", plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	current, err := p.Current("node-c4")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if got := valueOf(t, current, impl.ParamConsoleLevel); got != "4" {
		t.Fatalf("console_loglevel = %q, want the hot value 4", got)
	}
	if got := valueOf(t, current, impl.ParamPanic); got != "7" {
		t.Fatalf("panic = %q, want the hot value 7", got)
	}
	if got := valueOf(t, current, impl.ParamTHP); got != "madvise" {
		t.Fatalf("transparent_hugepage = %q, want the old value madvise until reboot", got)
	}
	gens, err := p.Generations("node-c4")
	if err != nil {
		t.Fatalf("generations: %v", err)
	}
	if len(gens) != 2 {
		t.Fatalf("generations = %d, want 2 after one apply", len(gens))
	}
	if gens[0].Index != 0 || !gens[0].Live {
		t.Fatalf("generation zero = %+v, want index 0 and live", gens[0])
	}
	if gens[1].Index != 1 || gens[1].Live {
		t.Fatalf("new generation = %+v, want index 1 and not live before the restart", gens[1])
	}
	if !strings.Contains(gens[1].Rendered, "transparent_hugepage=never") {
		t.Fatalf("written entry must carry the cold value, got %q", gens[1].Rendered)
	}
	drift, err := p.Verify("node-c4")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(drift.Pending) != 1 || drift.Pending[0] != impl.ParamTHP {
		t.Fatalf("pending = %v, want [transparent_hugepage]", drift.Pending)
	}
	if len(drift.Missing) != 0 || len(drift.Differs) != 0 {
		t.Fatalf("hot values must not show up as drift, got missing=%v differs=%v", drift.Missing, drift.Differs)
	}
	if drift.Clean() {
		t.Fatalf("a node with a pending parameter is not clean")
	}
}

func TestApplyRejectsForeignPlan(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c5", impl.RoleCompute)
	register(t, p, "node-c6", impl.RoleCompute)
	plan, err := p.Plan("node-c5", computeTarget("4", "10", "madvise"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := p.Apply("node-c6", plan); !errors.Is(err, impl.ErrBadPlan) {
		t.Fatalf("applying a plan of another node must fail with ErrBadPlan, got %v", err)
	}
	if err := p.Apply("node-missing", plan); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("applying to an unregistered node must fail with ErrUnknownNode, got %v", err)
	}
	gens, err := p.Generations("node-c6")
	if err != nil {
		t.Fatalf("generations: %v", err)
	}
	if len(gens) != 1 {
		t.Fatalf("a rejected apply must not write a generation, got %d", len(gens))
	}
}

func TestStalePlanIsRejected(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c7", impl.RoleCompute)
	first, err := p.Plan("node-c7", computeTarget("4", "10", "madvise"))
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	second, err := p.Plan("node-c7", computeTarget("2", "10", "madvise"))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if err := p.Apply("node-c7", first); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := p.Apply("node-c7", second); !errors.Is(err, impl.ErrStalePlan) {
		t.Fatalf("a plan built on generation 0 must be stale after the first apply, got %v", err)
	}
	current, err := p.Current("node-c7")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if got := valueOf(t, current, impl.ParamConsoleLevel); got != "4" {
		t.Fatalf("console_loglevel = %q, want the first plan value 4", got)
	}
}

func TestConcurrentApplyOnlyOnePlanWins(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c8", impl.RoleCompute)
	plans := make([]impl.ChangePlan, 0, 8)
	for i := 0; i < 8; i++ {
		plan, err := p.Plan("node-c8", computeTarget(fmt.Sprintf("%d", 1+i%4), "10", "madvise"))
		if err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
		plans = append(plans, plan)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	committed, stale, other := 0, 0, 0
	for _, plan := range plans {
		wg.Add(1)
		go func(plan impl.ChangePlan) {
			defer wg.Done()
			err := p.Apply("node-c8", plan)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				committed++
			case errors.Is(err, impl.ErrStalePlan):
				stale++
			default:
				other++
			}
		}(plan)
	}
	wg.Wait()
	if committed != 1 || stale != 7 || other != 0 {
		t.Fatalf("concurrent applies got committed=%d stale=%d other=%d, want 1/7/0", committed, stale, other)
	}
	gens, err := p.Generations("node-c8")
	if err != nil {
		t.Fatalf("generations: %v", err)
	}
	if len(gens) != 2 {
		t.Fatalf("generations = %d, want exactly one new generation", len(gens))
	}
}

func TestVerifyReportsDriftAfterRestart(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c9", impl.RoleCompute)
	plan, err := p.Plan("node-c9", computeTarget("4", "10", "madvise"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := p.Apply("node-c9", plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cmdline := "audit=on console_loglevel=3 iommu=strict mitigations=auto panic=10 rescue_shell=0 transparent_hugepage=madvise unknown_toggle=1"
	if err := p.ReportRunning("node-c9", cmdline); err != nil {
		t.Fatalf("report running: %v", err)
	}
	drift, err := p.Verify("node-c9")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(drift.Differs) != 1 {
		t.Fatalf("differs = %+v, want only console_loglevel", drift.Differs)
	}
	if drift.Differs[0].Key != impl.ParamConsoleLevel {
		t.Fatalf("differs key = %q, want console_loglevel", drift.Differs[0].Key)
	}
	if drift.Differs[0].Want != "4" || drift.Differs[0].Got != "3" {
		t.Fatalf("differs value = %+v, want want=4 got=3", drift.Differs[0])
	}
	if len(drift.Missing) != 1 || drift.Missing[0].Key != impl.ParamNUMABalancing {
		t.Fatalf("missing = %+v, want the absent numa_balancing", drift.Missing)
	}
	if len(drift.Extra) != 1 || drift.Extra[0].Key != "unknown_toggle" {
		t.Fatalf("extra = %+v, want unknown_toggle", drift.Extra)
	}
	if len(drift.Pending) != 0 {
		t.Fatalf("pending = %v, want none after the restart report", drift.Pending)
	}
	if drift.Clean() {
		t.Fatalf("a node with a differing parameter is not clean")
	}
}

func TestRenderEntryQuotesValuesWithSpaces(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c10", impl.RoleCompute)
	target := computeTarget("3", "10", "madvise")
	target.Entries = append(target.Entries, impl.Param{Key: impl.ParamModuleBlacklist, Value: "dm_mod raid1"})
	plan, err := p.Plan("node-c10", target)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := p.Apply("node-c10", plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rendered, err := p.Render("node-c10", 1)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rendered, `module_blacklist="dm_mod raid1"`) {
		t.Fatalf("rendered entry must quote the value with spaces, got %q", rendered)
	}
	if strings.Contains(rendered, "module_blacklist=dm_mod raid1") {
		t.Fatalf("unquoted value must not appear, got %q", rendered)
	}
	lines := strings.Split(strings.TrimSpace(rendered), "\n")
	if len(lines) != 9 {
		t.Fatalf("rendered lines = %d, want 9 parameters", len(lines))
	}
	auditAt, blacklistAt := -1, -1
	for i, line := range lines {
		if strings.HasPrefix(line, impl.ParamAudit+"=") {
			auditAt = i
		}
		if strings.HasPrefix(line, impl.ParamModuleBlacklist+"=") {
			blacklistAt = i
		}
	}
	if auditAt < 0 || blacklistAt < 0 || auditAt > blacklistAt {
		t.Fatalf("rendered lines must be sorted by parameter name, got %q", rendered)
	}
	again, err := p.Render("node-c10", 1)
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if again != rendered {
		t.Fatalf("rendering the same generation twice must be identical")
	}
	if _, err := p.Render("node-c10", 9); err == nil {
		t.Fatalf("rendering an unknown generation must fail")
	}
}

func TestRollbackRestoresRetainedGeneration(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c11", impl.RoleCompute)
	plan, err := p.Plan("node-c11", computeTarget("4", "10", "madvise"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := p.Apply("node-c11", plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := p.Rollback("node-c11"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	current, err := p.Current("node-c11")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if got := valueOf(t, current, impl.ParamConsoleLevel); got != "3" {
		t.Fatalf("console_loglevel after rollback = %q, want the generation zero value 3", got)
	}
	gens, err := p.Generations("node-c11")
	if err != nil {
		t.Fatalf("generations: %v", err)
	}
	last := gens[len(gens)-1]
	if last.Index != 2 {
		t.Fatalf("rollback generation index = %d, want 2", last.Index)
	}
	if got, ok := last.Params.Get(impl.ParamConsoleLevel); !ok || got != "3" {
		t.Fatalf("rollback generation holds console_loglevel=%q, want 3", got)
	}
	fresh := newPolicy(t)
	register(t, fresh, "node-c12", impl.RoleControl)
	if err := fresh.Rollback("node-c12"); !errors.Is(err, impl.ErrNoGeneration) {
		t.Fatalf("rollback with a single generation must fail with ErrNoGeneration, got %v", err)
	}
}

func TestGenerationHistoryCapped(t *testing.T) {
	p := newPolicy(t)
	register(t, p, "node-c13", impl.RoleCompute)
	for i := 0; i < 3; i++ {
		plan, err := p.Plan("node-c13", computeTarget(fmt.Sprintf("%d", 1+i), "10", "madvise"))
		if err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
		if err := p.Apply("node-c13", plan); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	gens, err := p.Generations("node-c13")
	if err != nil {
		t.Fatalf("generations: %v", err)
	}
	if len(gens) != 3 {
		t.Fatalf("retained generations = %d, want the cap 3", len(gens))
	}
	if gens[0].Index != 1 || gens[2].Index != 3 {
		t.Fatalf("retained window = [%d..%d], want [1..3] with generation zero evicted", gens[0].Index, gens[2].Index)
	}
}

func TestBatchApplyStopsAtFailedBatch(t *testing.T) {
	p := impl.NewBootParamPolicy(impl.PolicyLimits{MaxParams: 12, MaxValueLen: 32, MaxGenerations: 3, BatchSize: 2})
	nodes := []string{"node-b1", "node-b2", "node-b3", "node-b4"}
	plans := make(map[string]impl.ChangePlan, len(nodes))
	for _, node := range nodes {
		if err := p.RegisterNode(node, impl.RoleCompute); err != nil {
			t.Fatalf("register %s: %v", node, err)
		}
		plan, err := p.Plan(node, computeTarget("4", "10", "madvise"))
		if err != nil {
			t.Fatalf("plan for %s: %v", node, err)
		}
		plans[node] = plan
	}
	plans["node-b3"] = impl.ChangePlan{NodeID: "node-b3", Generation: 9}
	result, err := p.BatchApply([]string{"node-b4", "node-b3", "node-b2", "node-b1"}, plans)
	if !errors.Is(err, impl.ErrBatchAborted) {
		t.Fatalf("a failing batch must fail with ErrBatchAborted, got %v", err)
	}
	if len(result.Applied) != 2 || result.Applied[0] != "node-b1" || result.Applied[1] != "node-b2" {
		t.Fatalf("applied = %v, want the first batch node-b1 node-b2 in ascending order", result.Applied)
	}
	if result.Failed != "node-b3" {
		t.Fatalf("failed node = %q, want node-b3", result.Failed)
	}
	gens, err := p.Generations("node-b4")
	if err != nil {
		t.Fatalf("generations for node-b4: %v", err)
	}
	if len(gens) != 1 {
		t.Fatalf("node-b4 generations = %d, want it untouched after the aborted batch", len(gens))
	}
	gens, err = p.Generations("node-b2")
	if err != nil {
		t.Fatalf("generations for node-b2: %v", err)
	}
	if len(gens) != 2 {
		t.Fatalf("node-b2 generations = %d, want the committed batch", len(gens))
	}
}
