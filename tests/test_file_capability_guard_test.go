package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-d-07"
)

func hostPolicy() impl.CapabilityPolicy {
	return impl.CapabilityPolicy{
		Allowed:        []string{"cap_chown", "cap_net_admin", "cap_net_raw"},
		InheritAllowed: []string{"cap_net_raw"},
		MaxPerFile:     3,
		MaxFiles:       4,
	}
}

func TestRegisterFileStoresSortedCaps(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/usr/bin/ping", []string{"cap_net_raw", "cap_net_admin"}, impl.FileFlags{SetGID: true}); err != nil {
		t.Fatalf("register file: %v", err)
	}
	rec, ok := g.File("/usr/bin/ping")
	if !ok {
		t.Fatalf("registered file must be readable")
	}
	if rec.Gen != 1 {
		t.Fatalf("generation of a fresh row = %d, want 1", rec.Gen)
	}
	if len(rec.Caps) != 2 || rec.Caps[0] != "cap_net_admin" || rec.Caps[1] != "cap_net_raw" {
		t.Fatalf("caps = %v, want sorted [cap_net_admin cap_net_raw]", rec.Caps)
	}
	if !rec.Flags.SetGID || rec.Flags.SetUID || rec.Flags.Inherit {
		t.Fatalf("flags = %+v, want only SetGID", rec.Flags)
	}
	files := g.Files()
	if len(files) != 1 || files[0].Path != "/usr/bin/ping" {
		t.Fatalf("files = %+v, want exactly /usr/bin/ping", files)
	}
}

func TestRegisterFileRejectsBlankAndDuplicate(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("   ", nil, impl.FileFlags{}); !errors.Is(err, impl.ErrBlankPath) {
		t.Fatalf("blank path must be rejected with ErrBlankPath, got %v", err)
	}
	if err := g.RegisterFile("/srv/agent", nil, impl.FileFlags{}); err != nil {
		t.Fatalf("first registration of /srv/agent: %v", err)
	}
	if err := g.RegisterFile("/srv/agent", nil, impl.FileFlags{}); !errors.Is(err, impl.ErrDuplicateFile) {
		t.Fatalf("duplicate path must be rejected with ErrDuplicateFile, got %v", err)
	}
	if got := len(g.Files()); got != 1 {
		t.Fatalf("files after rejected duplicates = %d, want 1", got)
	}
}

func TestRegisterFileRejectsCapOutsideWhitelist(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/srv/helper", []string{"cap_sys_admin"}, impl.FileFlags{}); !errors.Is(err, impl.ErrCapNotAllowed) {
		t.Fatalf("unlisted capability must be rejected with ErrCapNotAllowed, got %v", err)
	}
	if got := len(g.Files()); got != 0 {
		t.Fatalf("rejected registration must not create rows, got %d", got)
	}
	if err := g.RegisterFile("/srv/helper", []string{"cap_chown"}, impl.FileFlags{}); err != nil {
		t.Fatalf("whitelisted capability must be accepted, got %v", err)
	}
}

func TestRegisterFileEnforcesCapLimit(t *testing.T) {
	g := impl.NewFileCapabilityGuard(impl.CapabilityPolicy{
		Allowed:        []string{"cap_chown", "cap_net_admin", "cap_net_raw"},
		InheritAllowed: []string{"cap_chown"},
		MaxPerFile:     2,
		MaxFiles:       2,
	})
	if err := g.RegisterFile("/srv/wide", []string{"cap_chown", "cap_net_admin", "cap_net_raw"}, impl.FileFlags{}); !errors.Is(err, impl.ErrCapLimit) {
		t.Fatalf("three capabilities over a limit of two must be rejected with ErrCapLimit, got %v", err)
	}
	if err := g.RegisterFile("/srv/ping", []string{"cap_net_raw", "cap_net_raw"}, impl.FileFlags{}); err != nil {
		t.Fatalf("repeated capability should collapse into one entry, got %v", err)
	}
	rec, ok := g.File("/srv/ping")
	if !ok || len(rec.Caps) != 1 || rec.Caps[0] != "cap_net_raw" {
		t.Fatalf("deduplicated caps = %+v, want exactly cap_net_raw", rec.Caps)
	}
}

func TestRegisterFileEnforcesFileLimit(t *testing.T) {
	g := impl.NewFileCapabilityGuard(impl.CapabilityPolicy{
		Allowed:    []string{"cap_chown"},
		MaxPerFile: 2,
		MaxFiles:   2,
	})
	if err := g.RegisterFile("/one", nil, impl.FileFlags{}); err != nil {
		t.Fatalf("register /one: %v", err)
	}
	if err := g.RegisterFile("/two", nil, impl.FileFlags{}); err != nil {
		t.Fatalf("register /two: %v", err)
	}
	if err := g.RegisterFile("/three", nil, impl.FileFlags{}); !errors.Is(err, impl.ErrFileLimit) {
		t.Fatalf("third path over a limit of two must be rejected with ErrFileLimit, got %v", err)
	}
	if got := len(g.Files()); got != 2 {
		t.Fatalf("files after rejected overflow = %d, want 2", got)
	}
}

func TestGrantIsIdempotentAndValidated(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/bin/tool", []string{"cap_net_raw"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /bin/tool: %v", err)
	}
	if err := g.Grant("/bin/tool", "cap_net_admin"); err != nil {
		t.Fatalf("grant cap_net_admin: %v", err)
	}
	rec, _ := g.File("/bin/tool")
	if rec.Gen != 2 {
		t.Fatalf("generation after one real change = %d, want 2", rec.Gen)
	}
	if len(rec.Caps) != 2 || rec.Caps[0] != "cap_net_admin" || rec.Caps[1] != "cap_net_raw" {
		t.Fatalf("caps after grant = %v, want sorted pair", rec.Caps)
	}
	if err := g.Grant("/bin/tool", "cap_net_admin"); err != nil {
		t.Fatalf("granting the same capability twice must stay successful, got %v", err)
	}
	rec, _ = g.File("/bin/tool")
	if rec.Gen != 2 || len(rec.Caps) != 2 {
		t.Fatalf("idempotent grant changed the row: gen=%d caps=%v", rec.Gen, rec.Caps)
	}
	if err := g.Grant("/bin/tool", "cap_sys_admin"); !errors.Is(err, impl.ErrCapNotAllowed) {
		t.Fatalf("grant of an unlisted capability must fail with ErrCapNotAllowed, got %v", err)
	}
	if err := g.Grant("/bin/ghost", "cap_chown"); !errors.Is(err, impl.ErrUnknownFile) {
		t.Fatalf("grant on an unknown path must fail with ErrUnknownFile, got %v", err)
	}
}

func TestGrantEnforcesCapLimit(t *testing.T) {
	g := impl.NewFileCapabilityGuard(impl.CapabilityPolicy{
		Allowed:        []string{"cap_chown", "cap_net_admin", "cap_net_raw"},
		InheritAllowed: []string{"cap_chown"},
		MaxPerFile:     2,
		MaxFiles:       2,
	})
	if err := g.RegisterFile("/bin/tool", []string{"cap_chown", "cap_net_raw"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /bin/tool: %v", err)
	}
	if err := g.Grant("/bin/tool", "cap_net_admin"); !errors.Is(err, impl.ErrCapLimit) {
		t.Fatalf("grant over the per file limit must fail with ErrCapLimit, got %v", err)
	}
	rec, _ := g.File("/bin/tool")
	if len(rec.Caps) != 2 || rec.Gen != 1 {
		t.Fatalf("rejected grant mutated the row: caps=%v gen=%d", rec.Caps, rec.Gen)
	}
}

func TestRevokeRequiresExistingGrant(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/bin/tool", []string{"cap_chown", "cap_net_raw"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /bin/tool: %v", err)
	}
	if err := g.Revoke("/bin/tool", "cap_chown"); err != nil {
		t.Fatalf("revoke cap_chown: %v", err)
	}
	rec, _ := g.File("/bin/tool")
	if len(rec.Caps) != 1 || rec.Caps[0] != "cap_net_raw" || rec.Gen != 2 {
		t.Fatalf("row after revoke = %+v, want only cap_net_raw at generation 2", rec)
	}
	if err := g.Revoke("/bin/tool", "cap_chown"); !errors.Is(err, impl.ErrCapAbsent) {
		t.Fatalf("revoking an absent capability must fail with ErrCapAbsent, got %v", err)
	}
	if err := g.Revoke("/bin/ghost", "cap_chown"); !errors.Is(err, impl.ErrUnknownFile) {
		t.Fatalf("revoke on an unknown path must fail with ErrUnknownFile, got %v", err)
	}
}

func TestSetFlagsRejectsSetUIDOnCapabilityFiles(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/srv/worker", nil, impl.FileFlags{}); err != nil {
		t.Fatalf("register /srv/worker: %v", err)
	}
	if err := g.SetFlags("/srv/worker", impl.FileFlags{SetUID: true}); err != nil {
		t.Fatalf("setuid on a capability free file must be allowed, got %v", err)
	}
	rec, _ := g.File("/srv/worker")
	if !rec.Flags.SetUID || rec.Gen != 2 {
		t.Fatalf("row after setuid = %+v, want SetUID at generation 2", rec)
	}
	if err := g.SetFlags("/srv/worker", impl.FileFlags{SetUID: true}); err != nil {
		t.Fatalf("rewriting identical flags must stay successful, got %v", err)
	}
	if rec, _ = g.File("/srv/worker"); rec.Gen != 2 {
		t.Fatalf("identical flags bumped the generation to %d, want 2", rec.Gen)
	}
	if err := g.RegisterFile("/srv/helper", []string{"cap_chown"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /srv/helper: %v", err)
	}
	if err := g.SetFlags("/srv/helper", impl.FileFlags{SetUID: true}); !errors.Is(err, impl.ErrSetUIDWithCaps) {
		t.Fatalf("setuid next to capabilities must fail with ErrSetUIDWithCaps, got %v", err)
	}
	if rec, _ = g.File("/srv/helper"); rec.Flags.SetUID {
		t.Fatalf("rejected SetFlags still flipped the setuid bit: %+v", rec)
	}
	if err := g.SetFlags("/srv/ghost", impl.FileFlags{}); !errors.Is(err, impl.ErrUnknownFile) {
		t.Fatalf("flags on an unknown path must fail with ErrUnknownFile, got %v", err)
	}
}

func TestEffectiveCapsFollowWhitelist(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/bin/tool", []string{"cap_net_raw", "cap_chown"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /bin/tool: %v", err)
	}
	caps := g.EffectiveCaps("/bin/tool")
	if len(caps) != 2 || caps[0] != "cap_chown" || caps[1] != "cap_net_raw" {
		t.Fatalf("effective caps = %v, want sorted [cap_chown cap_net_raw]", caps)
	}
	if caps := g.EffectiveCaps("/bin/ghost"); len(caps) != 0 {
		t.Fatalf("effective caps of an unknown path = %v, want empty", caps)
	}
}

func TestReloadPolicyKeepsRowsAndReportsDrift(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/bin/tool", []string{"cap_net_raw", "cap_net_admin"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /bin/tool: %v", err)
	}
	if got := len(g.Reconcile()); got != 0 {
		t.Fatalf("fresh rows against their own policy must be clean, got %d findings", got)
	}
	bad := []impl.CapabilityPolicy{
		{Allowed: nil, MaxPerFile: 2, MaxFiles: 2},
		{Allowed: []string{"cap_chown"}, MaxPerFile: 0, MaxFiles: 2},
		{Allowed: []string{"cap_chown"}, MaxPerFile: 2, MaxFiles: 0},
		{Allowed: []string{"cap_chown"}, InheritAllowed: []string{"cap_net_raw"}, MaxPerFile: 2, MaxFiles: 2},
	}
	for i, policy := range bad {
		if err := g.ReloadPolicy(policy); !errors.Is(err, impl.ErrBadPolicy) {
			t.Fatalf("invalid policy %d must fail with ErrBadPolicy, got %v", i, err)
		}
	}
	if caps := g.EffectiveCaps("/bin/tool"); len(caps) != 2 {
		t.Fatalf("a rejected policy swap changed the effective set to %v", caps)
	}
	narrow := impl.CapabilityPolicy{
		Allowed:        []string{"cap_chown", "cap_net_raw"},
		InheritAllowed: []string{"cap_chown"},
		MaxPerFile:     2,
		MaxFiles:       3,
	}
	if err := g.ReloadPolicy(narrow); err != nil {
		t.Fatalf("valid policy swap: %v", err)
	}
	rec, _ := g.File("/bin/tool")
	if len(rec.Caps) != 2 || rec.Gen != 1 {
		t.Fatalf("policy swap rewrote the row: caps=%v gen=%d", rec.Caps, rec.Gen)
	}
	if caps := g.EffectiveCaps("/bin/tool"); len(caps) != 1 || caps[0] != "cap_net_raw" {
		t.Fatalf("effective caps after narrowing = %v, want [cap_net_raw]", caps)
	}
	findings := g.Reconcile()
	if len(findings) != 1 {
		t.Fatalf("drift findings = %+v, want exactly one unlisted capability", findings)
	}
	if findings[0].Path != "/bin/tool" || findings[0].Kind != impl.ViolationUnlistedCap || findings[0].Cap != "cap_net_admin" {
		t.Fatalf("drift finding = %+v, want /bin/tool unlisted_cap cap_net_admin", findings[0])
	}
}

func TestRepairConvergesRowOntoPolicy(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/bin/tool", []string{"cap_net_raw", "cap_net_admin"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /bin/tool: %v", err)
	}
	narrow := impl.CapabilityPolicy{
		Allowed:        []string{"cap_net_raw"},
		InheritAllowed: []string{"cap_net_raw"},
		MaxPerFile:     1,
		MaxFiles:       2,
	}
	if err := g.ReloadPolicy(narrow); err != nil {
		t.Fatalf("narrow policy: %v", err)
	}
	report, err := g.Repair("/bin/tool")
	if err != nil {
		t.Fatalf("repair /bin/tool: %v", err)
	}
	if len(report.RemovedCaps) != 1 || report.RemovedCaps[0] != "cap_net_admin" {
		t.Fatalf("repair removed %v, want [cap_net_admin]", report.RemovedCaps)
	}
	rec, _ := g.File("/bin/tool")
	if len(rec.Caps) != 1 || rec.Caps[0] != "cap_net_raw" || rec.Gen != 2 {
		t.Fatalf("row after repair = %+v, want only cap_net_raw at generation 2", rec)
	}
	if got := len(g.Reconcile()); got != 0 {
		t.Fatalf("findings after repair = %d, want 0", got)
	}
	clean, err := g.Repair("/bin/tool")
	if err != nil {
		t.Fatalf("repair of a clean row: %v", err)
	}
	if len(clean.RemovedCaps) != 0 {
		t.Fatalf("clean repair removed %v, want nothing", clean.RemovedCaps)
	}
	if _, err := g.Repair("/bin/ghost"); !errors.Is(err, impl.ErrUnknownFile) {
		t.Fatalf("repair on an unknown path must fail with ErrUnknownFile, got %v", err)
	}
}

func TestSnapshotRestoreKeepsRowsAndPolicy(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/a", []string{"cap_net_raw", "cap_net_admin"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /a: %v", err)
	}
	if err := g.RegisterFile("/b", []string{"cap_chown"}, impl.FileFlags{SetGID: true}); err != nil {
		t.Fatalf("register /b: %v", err)
	}
	if err := g.Grant("/b", "cap_net_raw"); err != nil {
		t.Fatalf("grant on /b: %v", err)
	}
	narrow := impl.CapabilityPolicy{
		Allowed:        []string{"cap_chown", "cap_net_raw"},
		InheritAllowed: []string{"cap_chown"},
		MaxPerFile:     2,
		MaxFiles:       3,
	}
	if err := g.ReloadPolicy(narrow); err != nil {
		t.Fatalf("narrow policy: %v", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := g.RemoveFile("/a"); err != nil {
		t.Fatalf("remove /a: %v", err)
	}
	if err := g.RegisterFile("/zzz", nil, impl.FileFlags{}); err != nil {
		t.Fatalf("register /zzz: %v", err)
	}
	if err := g.ReloadPolicy(hostPolicy()); err != nil {
		t.Fatalf("restore wide policy before restore: %v", err)
	}
	if err := g.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	files := g.Files()
	if len(files) != 2 || files[0].Path != "/a" || files[1].Path != "/b" {
		t.Fatalf("files after restore = %+v, want [/a /b]", files)
	}
	if len(files[0].Caps) != 2 || files[0].Gen != 1 {
		t.Fatalf("restored /a row = %+v, want two caps at generation 1", files[0])
	}
	if len(files[1].Caps) != 2 || files[1].Gen != 2 || !files[1].Flags.SetGID {
		t.Fatalf("restored /b row = %+v, want two caps at generation 2 with SetGID", files[1])
	}
	findings := g.Reconcile()
	if len(findings) != 1 || findings[0].Cap != "cap_net_admin" {
		t.Fatalf("restored policy does not match the snapshot: findings=%+v", findings)
	}
	if caps := g.EffectiveCaps("/a"); len(caps) != 1 || caps[0] != "cap_net_raw" {
		t.Fatalf("effective caps of /a after restore = %v, want [cap_net_raw]", caps)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	g := impl.NewFileCapabilityGuard(hostPolicy())
	if err := g.RegisterFile("/a", []string{"cap_chown"}, impl.FileFlags{}); err != nil {
		t.Fatalf("register /a: %v", err)
	}
	if _, err := g.Snapshot(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := g.Restore([]byte("{\"files\": [ {\"path\":")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("truncated payload must fail with ErrBadSnapshot, got %v", err)
	}
	files := g.Files()
	if len(files) != 1 || files[0].Path != "/a" || len(files[0].Caps) != 1 || files[0].Caps[0] != "cap_chown" {
		t.Fatalf("failed restore damaged the live table: %+v", files)
	}
}

func TestConcurrentGrantsKeepSingleSortedSet(t *testing.T) {
	allowed := make([]string, 8)
	for i := range allowed {
		allowed[i] = fmt.Sprintf("cap_%02d", i)
	}
	g := impl.NewFileCapabilityGuard(impl.CapabilityPolicy{
		Allowed:        allowed,
		InheritAllowed: allowed[:1],
		MaxPerFile:     8,
		MaxFiles:       2,
	})
	if err := g.RegisterFile("/opt/worker", nil, impl.FileFlags{}); err != nil {
		t.Fatalf("register /opt/worker: %v", err)
	}
	const workers = 24
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- g.Grant("/opt/worker", allowed[n%8])
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent grant returned %v", err)
		}
	}
	rec, ok := g.File("/opt/worker")
	if !ok {
		t.Fatalf("row disappeared during concurrent grants")
	}
	if len(rec.Caps) != 8 {
		t.Fatalf("capability count after concurrent grants = %d, want 8", len(rec.Caps))
	}
	for i, cap := range rec.Caps {
		if cap != allowed[i] {
			t.Fatalf("caps[%d] = %q, want %q (sorted, no duplicates)", i, cap, allowed[i])
		}
	}
	if caps := g.EffectiveCaps("/opt/worker"); len(caps) != 8 {
		t.Fatalf("effective caps after concurrent grants = %d, want 8", len(caps))
	}
}
