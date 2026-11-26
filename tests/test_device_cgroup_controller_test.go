package task

import (
	"errors"
	"sort"
	"strings"
	"testing"

	impl "gsb/batch50-go-safety-c-03"
)

func testLimits() impl.Limits {
	return impl.Limits{MaxRulesPerContainer: 16, MaxContainers: 4, MaxCacheEntries: 8}
}

func basePolicy() impl.DevicePolicy {
	return impl.DevicePolicy{
		Revision: 7,
		Rules: []impl.Rule{
			{Type: impl.DeviceChar, Major: 1, Minor: 3, Allow: true, Priority: 20},
			{Type: impl.DeviceBlock, Major: 8, Minor: impl.Wildcard, Allow: true, Priority: 10},
			{Type: impl.DeviceChar, Major: 10, Minor: 200, Allow: false, Priority: 90},
		},
	}
}

func mustCompile(t *testing.T, c *impl.DeviceCgroupController, policy impl.DevicePolicy) impl.Program {
	t.Helper()
	prog, err := c.Compile(policy)
	if err != nil {
		t.Fatalf("compile must succeed, got %v", err)
	}
	return prog
}

func mustAttach(t *testing.T, c *impl.DeviceCgroupController, container string, policy impl.DevicePolicy) impl.Program {
	t.Helper()
	prog := mustCompile(t, c, policy)
	if err := c.Attach(container, prog); err != nil {
		t.Fatalf("attach %s must succeed, got %v", container, err)
	}
	return prog
}

func TestCompileProducesProgramAndDigest(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	prog := mustCompile(t, c, basePolicy())
	if prog.ID == "" || !strings.HasPrefix(prog.ID, "prog-") {
		t.Fatalf("program id = %q, want a prog- prefixed id", prog.ID)
	}
	if prog.Revision != 7 {
		t.Fatalf("program revision = %d, want 7", prog.Revision)
	}
	if prog.Rules != 3 {
		t.Fatalf("program rule count = %d, want 3", prog.Rules)
	}
	if prog.Digest == "" {
		t.Fatalf("program digest must not be empty")
	}
	if len(prog.Instructions) != 3 {
		t.Fatalf("instructions = %d, want 3", len(prog.Instructions))
	}
	if prog.Instructions[0].Op != impl.OpDeny {
		t.Fatalf("first instruction op = %q, want deny to be emitted first", prog.Instructions[0].Op)
	}
}

func TestCompileIsOrderIndependent(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	p1 := basePolicy()
	p2 := basePolicy()
	for i, j := 0, len(p2.Rules)-1; i < j; i, j = i+1, j-1 {
		p2.Rules[i], p2.Rules[j] = p2.Rules[j], p2.Rules[i]
	}
	prog1 := mustCompile(t, c, p1)
	prog2 := mustCompile(t, c, p2)
	if prog1.Digest != prog2.Digest {
		t.Fatalf("digests differ: %q vs %q", prog1.Digest, prog2.Digest)
	}
	if prog1.ID != prog2.ID {
		t.Fatalf("program ids differ: %q vs %q", prog1.ID, prog2.ID)
	}
	ops := make([]string, 0, len(prog1.Instructions))
	for _, ins := range prog1.Instructions {
		ops = append(ops, ins.Op)
	}
	if strings.Join(ops, ",") != "deny,allow,allow" {
		t.Fatalf("instruction order = %v, want deny before allow", ops)
	}
}

func TestCompileRejectsUnknownDeviceType(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	policy := impl.DevicePolicy{
		Revision: 1,
		Rules:    []impl.Rule{{Type: "z", Major: 1, Minor: 1, Allow: true}},
	}
	if _, err := c.Compile(policy); !errors.Is(err, impl.ErrBadDeviceType) {
		t.Fatalf("unknown class err = %v, want ErrBadDeviceType", err)
	}
}

func TestCompileRejectsRuleLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxRulesPerContainer = 2
	c := impl.NewDeviceCgroupController(limits)
	if _, err := c.Compile(basePolicy()); !errors.Is(err, impl.ErrRuleLimit) {
		t.Fatalf("three rules over a limit of two err = %v, want ErrRuleLimit", err)
	}
}

func TestCompileRejectsEmptyPolicy(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	if _, err := c.Compile(impl.DevicePolicy{Revision: 1}); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("policy without rules err = %v, want ErrBadPolicy", err)
	}
	if _, err := c.Compile(impl.DevicePolicy{Rules: basePolicy().Rules}); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("policy without revision err = %v, want ErrBadPolicy", err)
	}
}

func TestAttachRequiresCompiledProgram(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	if err := c.Attach("c-1", impl.Program{}); !errors.Is(err, impl.ErrBadProgram) {
		t.Fatalf("zero program err = %v, want ErrBadProgram", err)
	}
	prog := mustAttach(t, c, "c-1", basePolicy())
	got, ok := c.Program("c-1")
	if !ok {
		t.Fatalf("container c-1 must be attached")
	}
	if got.Digest != prog.Digest || got.Revision != 7 {
		t.Fatalf("active program = %+v, want digest %q revision 7", got, prog.Digest)
	}
	if v := c.CurrentVersion("c-1"); v != impl.Version(7) {
		t.Fatalf("current version = %d, want 7", v)
	}
}

func TestAttachEnforcesContainerLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxContainers = 1
	c := impl.NewDeviceCgroupController(limits)
	mustAttach(t, c, "c-1", basePolicy())
	if err := c.Attach("c-2", mustCompile(t, c, basePolicy())); !errors.Is(err, impl.ErrContainerLimit) {
		t.Fatalf("second container err = %v, want ErrContainerLimit", err)
	}
	got := c.Containers()
	if len(got) != 1 || got[0] != "c-1" {
		t.Fatalf("containers = %v, want exactly c-1", got)
	}
}

func TestExplainDefaultDeny(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	prog := mustAttach(t, c, "c-1", basePolicy())
	verdict, reason := c.Explain("c-1", impl.Device{Type: impl.DeviceChar, Major: 9, Minor: 0, Path: "/dev/tty9"})
	if verdict.Allowed {
		t.Fatalf("unmatched device must be denied, got %+v", verdict)
	}
	if reason.Code != impl.ReasonDefaultDeny {
		t.Fatalf("reason code = %q, want %q", reason.Code, impl.ReasonDefaultDeny)
	}
	if verdict.Program != prog.ID {
		t.Fatalf("verdict program = %q, want %q", verdict.Program, prog.ID)
	}
	if verdict.Generation != impl.Version(7) {
		t.Fatalf("verdict generation = %d, want 7", verdict.Generation)
	}
}

func TestExplainDenyOutranksAllow(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	policy := impl.DevicePolicy{
		Revision: 3,
		Rules: []impl.Rule{
			{Type: impl.DeviceChar, Major: 1, Minor: 3, Allow: true, Priority: 99},
			{Type: impl.DeviceChar, Major: 1, Minor: 3, Allow: false, Priority: 1},
		},
	}
	mustAttach(t, c, "c-1", policy)
	verdict, reason := c.Explain("c-1", impl.Device{Type: impl.DeviceChar, Major: 1, Minor: 3})
	if verdict.Allowed {
		t.Fatalf("deny rule must outrank the allow rule, got %+v", verdict)
	}
	if reason.Code != impl.ReasonExactDeny {
		t.Fatalf("reason code = %q, want %q", reason.Code, impl.ReasonExactDeny)
	}
}

func TestExplainWildcardAllow(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	mustAttach(t, c, "c-1", basePolicy())
	verdict, reason := c.Explain("c-1", impl.Device{Type: impl.DeviceBlock, Major: 8, Minor: 17, Path: "/dev/sdb1"})
	if !verdict.Allowed {
		t.Fatalf("block device 8:17 must be allowed by the wildcard rule, reason %+v", reason)
	}
	if reason.Code != impl.ReasonWildcardAllow {
		t.Fatalf("reason code = %q, want %q", reason.Code, impl.ReasonWildcardAllow)
	}
	blocked, denyReason := c.Explain("c-1", impl.Device{Type: impl.DeviceChar, Major: 10, Minor: 200})
	if blocked.Allowed {
		t.Fatalf("char device 10:200 must stay denied")
	}
	if denyReason.Code != impl.ReasonExactDeny {
		t.Fatalf("reason code = %q, want %q", denyReason.Code, impl.ReasonExactDeny)
	}
}

func TestUpdateBumpsVersionAndAppliesNewRules(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	mustAttach(t, c, "c-1", basePolicy())
	next := impl.DevicePolicy{
		Revision: 8,
		Rules:    []impl.Rule{{Type: impl.DeviceChar, Major: 9, Minor: 0, Allow: true, Priority: 5}},
	}
	ver, err := c.Update("c-1", next)
	if err != nil {
		t.Fatalf("update must succeed, got %v", err)
	}
	if ver != impl.Version(8) {
		t.Fatalf("update returned version %d, want 8", ver)
	}
	if got := c.CurrentVersion("c-1"); got != impl.Version(8) {
		t.Fatalf("current version = %d, want 8", got)
	}
	verdict, reason := c.Explain("c-1", impl.Device{Type: impl.DeviceChar, Major: 9, Minor: 0})
	if !verdict.Allowed || reason.Code != impl.ReasonExactAllow {
		t.Fatalf("new generation must allow 9:0, verdict=%+v reason=%+v", verdict, reason)
	}
	if verdict.Generation != impl.Version(8) {
		t.Fatalf("verdict generation = %d, want 8", verdict.Generation)
	}
}

func TestUpdateRejectsStaleRevision(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	mustAttach(t, c, "c-1", basePolicy())
	next := impl.DevicePolicy{
		Revision: 8,
		Rules:    []impl.Rule{{Type: impl.DeviceChar, Major: 9, Minor: 0, Allow: true, Priority: 5}},
	}
	if _, err := c.Update("c-1", next); err != nil {
		t.Fatalf("first update must succeed, got %v", err)
	}
	stale := impl.DevicePolicy{
		Revision: 8,
		Rules:    []impl.Rule{{Type: impl.DeviceChar, Major: 11, Minor: 0, Allow: true, Priority: 5}},
	}
	if _, err := c.Update("c-1", stale); !errors.Is(err, impl.ErrStaleRevision) {
		t.Fatalf("same revision err = %v, want ErrStaleRevision", err)
	}
	if got := c.CurrentVersion("c-1"); got != impl.Version(8) {
		t.Fatalf("stale update must not move the version, got %d", got)
	}
	verdict, _ := c.Explain("c-1", impl.Device{Type: impl.DeviceChar, Major: 11, Minor: 0})
	if verdict.Allowed {
		t.Fatalf("stale rules must not be installed")
	}
}

func TestUpdateFailureKeepsPreviousProgram(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	mustAttach(t, c, "c-1", basePolicy())
	before, ok := c.Program("c-1")
	if !ok {
		t.Fatalf("container must be attached before the failed update")
	}
	broken := impl.DevicePolicy{
		Revision: 9,
		Rules:    []impl.Rule{{Type: "q", Major: 1, Minor: 1, Allow: true}},
	}
	if _, err := c.Update("c-1", broken); !errors.Is(err, impl.ErrBadDeviceType) {
		t.Fatalf("broken policy err = %v, want ErrBadDeviceType", err)
	}
	after, ok := c.Program("c-1")
	if !ok {
		t.Fatalf("failed update must keep the container attached")
	}
	if after.Digest != before.Digest || after.Revision != before.Revision {
		t.Fatalf("active program changed after a failed update: %+v -> %+v", before, after)
	}
	if got := c.CurrentVersion("c-1"); got != impl.Version(7) {
		t.Fatalf("version after failed update = %d, want 7", got)
	}
	if _, err := c.Update("c-ghost", impl.DevicePolicy{Revision: 2, Rules: basePolicy().Rules}); !errors.Is(err, impl.ErrNoContainer) {
		t.Fatalf("update on unknown container err = %v, want ErrNoContainer", err)
	}
}

func TestDetachRemovesContainer(t *testing.T) {
	c := impl.NewDeviceCgroupController(testLimits())
	mustAttach(t, c, "c-1", basePolicy())
	mustAttach(t, c, "c-2", basePolicy())
	if err := c.Detach("c-1"); err != nil {
		t.Fatalf("detach must succeed, got %v", err)
	}
	if _, ok := c.Program("c-1"); ok {
		t.Fatalf("detached container must not report a program")
	}
	verdict, reason := c.Explain("c-1", impl.Device{Type: impl.DeviceChar, Major: 1, Minor: 3})
	if verdict.Allowed {
		t.Fatalf("detached container must never be allowed")
	}
	if reason.Code != impl.ReasonNoContainer {
		t.Fatalf("reason code = %q, want %q", reason.Code, impl.ReasonNoContainer)
	}
	containers := c.Containers()
	sort.Strings(containers)
	if len(containers) != 1 || containers[0] != "c-2" {
		t.Fatalf("containers = %v, want exactly c-2", containers)
	}
	if err := c.Detach("c-1"); !errors.Is(err, impl.ErrNoContainer) {
		t.Fatalf("second detach err = %v, want ErrNoContainer", err)
	}
}

func TestExportImportKeepsPrograms(t *testing.T) {
	source := impl.NewDeviceCgroupController(testLimits())
	prog := mustAttach(t, source, "c-1", basePolicy())
	mustAttach(t, source, "c-2", impl.DevicePolicy{
		Revision: 2,
		Rules:    []impl.Rule{{Type: impl.DeviceAll, Major: impl.Wildcard, Minor: impl.Wildcard, Allow: true}},
	})
	payload, err := source.Export()
	if err != nil {
		t.Fatalf("export must succeed, got %v", err)
	}
	target := impl.NewDeviceCgroupController(testLimits())
	if err := target.Import(payload); err != nil {
		t.Fatalf("import must succeed, got %v", err)
	}
	restored, ok := target.Program("c-1")
	if !ok {
		t.Fatalf("imported controller must know c-1")
	}
	if restored.Digest != prog.Digest || target.CurrentVersion("c-1") != impl.Version(7) {
		t.Fatalf("restored program = %+v, want digest %q version 7", restored, prog.Digest)
	}
	verdict, reason := target.Explain("c-2", impl.Device{Type: impl.DeviceChar, Major: 10, Minor: 200})
	if !verdict.Allowed || reason.Code != impl.ReasonWildcardAllow {
		t.Fatalf("restored all-device policy must allow 10:200, verdict=%+v reason=%+v", verdict, reason)
	}
	if got := len(target.Containers()); got != 2 {
		t.Fatalf("restored containers = %d, want 2", got)
	}
}
