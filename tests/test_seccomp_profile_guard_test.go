package task

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	impl "gsb/batch50-go-safety-d-06"
)

func baseProfile() impl.Profile {
	return impl.Profile{
		Name:          "container",
		DefaultAction: impl.ActionKill,
		Rules:         []impl.SyscallRule{{Name: "read", Action: impl.ActionAllow}},
		NoNewPrivs:    true,
		AllThreads:    true,
		ThreadCount:   1,
	}
}

func ruleNames(rules []impl.SyscallRule) []string {
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Name)
	}
	return names
}

func TestNewGuardAppliesDefaults(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	pol := g.Policy()
	if pol.MaxRules != impl.DefaultMaxRules {
		t.Fatalf("default rule budget = %d, want %d", pol.MaxRules, impl.DefaultMaxRules)
	}
	if pol.MaxThreads != impl.DefaultMaxThreads {
		t.Fatalf("default thread budget = %d, want %d", pol.MaxThreads, impl.DefaultMaxThreads)
	}
	if pol.AllowMissingNoNewPrivs || pol.AllowPartialThreads {
		t.Fatalf("default policy = %+v, want both relaxations off", pol)
	}
	if len(pol.ProtectedSyscalls) < 4 {
		t.Fatalf("default protected list = %v, want at least four entries", pol.ProtectedSyscalls)
	}
	if !sort.StringsAreSorted(pol.ProtectedSyscalls) {
		t.Fatalf("default protected list = %v, want ascending order", pol.ProtectedSyscalls)
	}
	found := false
	for _, name := range pol.ProtectedSyscalls {
		if name == "ptrace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("default protected list = %v, want ptrace inside", pol.ProtectedSyscalls)
	}
}

func TestValidateRejectsPermissiveDefault(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	permissive := baseProfile()
	permissive.DefaultAction = impl.ActionAllow
	if err := g.Validate(permissive); !errors.Is(err, impl.ErrPermissiveDefault) {
		t.Fatalf("allow default error = %v, want ErrPermissiveDefault", err)
	}
	clean := baseProfile()
	if err := g.Validate(clean); err != nil {
		t.Fatalf("kill default must validate: %v", err)
	}
}

func TestValidateRejectsUnknownAction(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	bad := baseProfile()
	bad.Rules = []impl.SyscallRule{{Name: "read", Action: "bogus"}}
	if err := g.Validate(bad); !errors.Is(err, impl.ErrBadAction) {
		t.Fatalf("unknown action error = %v, want ErrBadAction", err)
	}
	blank := baseProfile()
	blank.Rules = []impl.SyscallRule{{Name: "  ", Action: impl.ActionErrno, Errno: 1}}
	if err := g.Validate(blank); !errors.Is(err, impl.ErrBlankSyscall) {
		t.Fatalf("blank syscall error = %v, want ErrBlankSyscall", err)
	}
	noName := baseProfile()
	noName.Name = ""
	if err := g.Validate(noName); !errors.Is(err, impl.ErrBlankProfile) {
		t.Fatalf("blank profile name error = %v, want ErrBlankProfile", err)
	}
}

func TestValidateRejectsProtectedSyscallAllowed(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	leaky := baseProfile()
	leaky.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "ptrace", Action: impl.ActionAllow},
	}
	if err := g.Validate(leaky); !errors.Is(err, impl.ErrProtectedSyscall) {
		t.Fatalf("allowed ptrace error = %v, want ErrProtectedSyscall", err)
	}
	denied := baseProfile()
	denied.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "ptrace", Action: impl.ActionErrno, Errno: 1},
	}
	if err := g.Validate(denied); err != nil {
		t.Fatalf("explicitly denied ptrace must validate: %v", err)
	}
}

func TestValidateRejectsDuplicateAndExcessRules(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	dup := baseProfile()
	dup.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "read", Action: impl.ActionErrno, Errno: 5},
	}
	if err := g.Validate(dup); !errors.Is(err, impl.ErrDuplicateRule) {
		t.Fatalf("duplicate rule error = %v, want ErrDuplicateRule", err)
	}

	tight := impl.NewSeccompProfileGuard(impl.GuardPolicy{MaxRules: 2, MaxThreads: 4})
	many := baseProfile()
	many.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "write", Action: impl.ActionAllow},
		{Name: "openat", Action: impl.ActionAllow},
	}
	if err := tight.Validate(many); !errors.Is(err, impl.ErrRuleLimit) {
		t.Fatalf("over budget rules error = %v, want ErrRuleLimit", err)
	}
}

func TestSortRulesAscendingWithoutMutation(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	input := []impl.SyscallRule{
		{Name: "write", Action: impl.ActionAllow},
		{Name: "openat", Action: impl.ActionAllow},
		{Name: "read", Action: impl.ActionAllow},
	}
	out := g.SortRules(input)
	if got, want := ruleNames(out), []string{"openat", "read", "write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted names = %v, want %v", got, want)
	}
	if got, want := ruleNames(input), []string{"write", "openat", "read"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("input order = %v, want the untouched %v", got, want)
	}
	repeat := g.SortRules(input)
	if !reflect.DeepEqual(out, repeat) {
		t.Fatalf("repeat sort = %v, want the same order %v", repeat, out)
	}
}

func TestRuleForFindsRule(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	profile := baseProfile()
	profile.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "openat", Action: impl.ActionErrno, Errno: 13},
	}
	rule, ok := g.RuleFor(profile, "openat")
	if !ok || rule.Action != impl.ActionErrno || rule.Errno != 13 {
		t.Fatalf("openat rule = %+v ok=%v, want errno 13", rule, ok)
	}
	if _, ok := g.RuleFor(profile, "mount"); ok {
		t.Fatalf("syscall without a rule must not be found")
	}
}

func TestMergeKeepsTightenedSubset(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	base := baseProfile()
	base.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "write", Action: impl.ActionAllow},
		{Name: "openat", Action: impl.ActionAllow},
	}
	next := baseProfile()
	next.Rules = []impl.SyscallRule{{Name: "read", Action: impl.ActionAllow}}

	res, err := g.Merge(base, next)
	if err != nil {
		t.Fatalf("tightening merge: %v", err)
	}
	if got, want := ruleNames(res.Profile.Rules), []string{"read"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged rules = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(res.Removed, []string{"openat", "write"}) {
		t.Fatalf("removed = %v, want [openat write]", res.Removed)
	}
	if len(res.Added) != 0 || len(res.Widened) != 0 {
		t.Fatalf("added = %v widened = %v, want both empty", res.Added, res.Widened)
	}
	if res.Profile.DefaultAction != impl.ActionKill {
		t.Fatalf("merged default action = %q, want %q", res.Profile.DefaultAction, impl.ActionKill)
	}
	if rule, ok := g.RuleFor(res.Profile, "write"); ok {
		t.Fatalf("merged profile still carries write: %+v", rule)
	}
}

func TestMergeRecordsAddedDenials(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	base := baseProfile()
	base.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "write", Action: impl.ActionAllow},
	}
	next := baseProfile()
	next.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "openat", Action: impl.ActionErrno, Errno: 5},
	}
	res, err := g.Merge(base, next)
	if err != nil {
		t.Fatalf("merge with a new denial: %v", err)
	}
	if !reflect.DeepEqual(res.Added, []string{"openat"}) {
		t.Fatalf("added = %v, want [openat]", res.Added)
	}
	if !reflect.DeepEqual(res.Removed, []string{"write"}) {
		t.Fatalf("removed = %v, want [write]", res.Removed)
	}
	rule, ok := g.RuleFor(res.Profile, "openat")
	if !ok || rule.Action != impl.ActionErrno || rule.Errno != 5 {
		t.Fatalf("merged openat rule = %+v ok=%v, want errno 5", rule, ok)
	}
}

func TestTightenReplacesActionWithoutMutatingInput(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	profile := baseProfile()
	out, err := g.Tighten(profile, []impl.SyscallRule{{Name: "read", Action: impl.ActionErrno, Errno: 13}})
	if err != nil {
		t.Fatalf("tighten read: %v", err)
	}
	rule, ok := g.RuleFor(out, "read")
	if !ok || rule.Action != impl.ActionErrno || rule.Errno != 13 {
		t.Fatalf("tightened read rule = %+v ok=%v, want errno 13", rule, ok)
	}
	original, ok := g.RuleFor(profile, "read")
	if !ok || original.Action != impl.ActionAllow {
		t.Fatalf("input profile rule = %+v ok=%v, want the untouched allow", original, ok)
	}
	if _, err := g.Tighten(profile, []impl.SyscallRule{{Name: "ptrace", Action: impl.ActionAllow}}); !errors.Is(err, impl.ErrProtectedSyscall) {
		t.Fatalf("tighten with an allowed protected syscall error = %v, want ErrProtectedSyscall", err)
	}
	if _, err := g.Tighten(profile, []impl.SyscallRule{{Name: "", Action: impl.ActionKill}}); !errors.Is(err, impl.ErrBlankSyscall) {
		t.Fatalf("tighten with a blank syscall error = %v, want ErrBlankSyscall", err)
	}
}

func TestCheckEscapeListsProtectedAllowed(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	leaky := baseProfile()
	leaky.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "ptrace", Action: impl.ActionAllow},
		{Name: "bpf", Action: impl.ActionAllow},
		{Name: "userfaultfd", Action: impl.ActionAllow},
	}
	escapes := g.CheckEscape(leaky)
	if got, want := escapes, []string{"bpf", "ptrace", "userfaultfd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("escapes = %v, want %v", got, want)
	}
	clean := baseProfile()
	if escapes := g.CheckEscape(clean); len(escapes) != 0 {
		t.Fatalf("clean profile escapes = %v, want none", escapes)
	}
}

func TestApplyRequiresNoNewPrivs(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	profile := baseProfile()
	profile.NoNewPrivs = false
	if _, err := g.Apply(profile); !errors.Is(err, impl.ErrNoNewPrivs) {
		t.Fatalf("missing no_new_privs error = %v, want ErrNoNewPrivs", err)
	}
	if _, ok := g.Applied(); ok {
		t.Fatalf("a rejected profile must not be installed")
	}
	if st := g.Stats(); st.Applied != 0 {
		t.Fatalf("applied counter = %d, want 0", st.Applied)
	}
}

func TestApplyRejectsBadThreadCount(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	zero := baseProfile()
	zero.ThreadCount = 0
	if _, err := g.Apply(zero); !errors.Is(err, impl.ErrBadThreadCount) {
		t.Fatalf("zero threads error = %v, want ErrBadThreadCount", err)
	}
	huge := baseProfile()
	huge.ThreadCount = impl.DefaultMaxThreads + 1
	if _, err := g.Apply(huge); !errors.Is(err, impl.ErrThreadLimit) {
		t.Fatalf("thread budget error = %v, want ErrThreadLimit", err)
	}
}

func TestApplyRecordsReportAndIsIdempotent(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	profile := baseProfile()
	profile.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "write", Action: impl.ActionAllow},
		{Name: "ptrace", Action: impl.ActionErrno, Errno: 1},
	}
	report, err := g.Apply(profile)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Profile != "container" || report.Threads != 1 {
		t.Fatalf("report = %+v, want container on 1 thread", report)
	}
	if report.Blocked != 1 {
		t.Fatalf("blocked counter = %d, want 1 (the ptrace errno rule)", report.Blocked)
	}
	if len(report.Escapes) != 0 {
		t.Fatalf("escapes = %v, want none", report.Escapes)
	}
	applied, ok := g.Applied()
	if !ok || applied.Name != "container" || len(applied.Rules) != 3 {
		t.Fatalf("applied profile = %+v ok=%v, want the three rule container profile", applied, ok)
	}
	again, err := g.Apply(profile)
	if err != nil {
		t.Fatalf("repeated apply: %v", err)
	}
	if again.Profile != report.Profile || again.Blocked != report.Blocked {
		t.Fatalf("repeated report = %+v, want the first one %+v", again, report)
	}
	if st := g.Stats(); st.Applied != 1 {
		t.Fatalf("applied counter after two identical applies = %d, want 1", st.Applied)
	}
}

func TestSnapshotRestoreKeepsAppliedProfile(t *testing.T) {
	g := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	profile := baseProfile()
	profile.Rules = []impl.SyscallRule{
		{Name: "read", Action: impl.ActionAllow},
		{Name: "ptrace", Action: impl.ActionErrno, Errno: 1},
	}
	if _, err := g.Apply(profile); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewSeccompProfileGuard(impl.GuardPolicy{})
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	applied, ok := restored.Applied()
	if !ok || applied.Name != "container" || !applied.NoNewPrivs || applied.ThreadCount != 1 {
		t.Fatalf("restored profile = %+v ok=%v, want the applied container profile", applied, ok)
	}
	rule, ok := restored.RuleFor(applied, "ptrace")
	if !ok || rule.Action != impl.ActionErrno || rule.Errno != 1 {
		t.Fatalf("restored ptrace rule = %+v ok=%v, want errno 1", rule, ok)
	}
	if before, after := g.Stats(), restored.Stats(); before != after {
		t.Fatalf("restored stats = %+v, want %+v", after, before)
	}
	if err := restored.Restore([]byte("not a snapshot payload")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("garbage snapshot error = %v, want ErrBadSnapshot", err)
	}
	if _, ok := restored.Applied(); !ok {
		t.Fatalf("failed restore must leave the installed profile untouched")
	}
}
