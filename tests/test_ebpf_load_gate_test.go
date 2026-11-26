package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-d-03"
)

func gatePolicy() impl.GatePolicy {
	return impl.GatePolicy{
		AllowedOps:       []string{"mov", "ldMap", "call", "exit"},
		AllowedProgTypes: []string{"socket_filter", "xdp", "tracepoint"},
		MaxPrograms:      2,
		MaxInsns:         4,
		MaxMapsPerProg:   2,
		MaxAttachTargets: 1,
		RequiredLicense:  "GPL",
	}
}

func validProgram(name string) impl.Program {
	return impl.Program{
		Name:    name,
		Type:    "socket_filter",
		Insns:   []impl.Instruction{{Op: "mov", Imm: 1}, {Op: "ldMap", Map: "conn_stats"}, {Op: "exit"}},
		Maps:    []string{"conn_stats"},
		License: "GPL",
	}
}

func mustRegister(t *testing.T, g *impl.EbpfLoadGate, spec impl.MapSpec) {
	t.Helper()
	if err := g.RegisterMap(spec); err != nil {
		t.Fatalf("register map %q: %v", spec.Name, err)
	}
}

func TestRegisterMapValidatesSpec(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	if err := g.RegisterMap(impl.MapSpec{Tenant: "t1", MaxEntries: 8}); !errors.Is(err, impl.ErrBlankMapName) {
		t.Fatalf("blank map name error = %v, want ErrBlankMapName", err)
	}
	if err := g.RegisterMap(impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 0}); !errors.Is(err, impl.ErrBadMapSpec) {
		t.Fatalf("zero max entries error = %v, want ErrBadMapSpec", err)
	}
	if err := g.RegisterMap(impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 0}); !errors.Is(err, impl.ErrBadMapSpec) {
		t.Fatalf("repeated bad spec error = %v, want ErrBadMapSpec", err)
	}
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	if err := g.CheckMapAccess("t1", "conn_stats", false); err != nil {
		t.Fatalf("registered map must be readable by its tenant, got %v", err)
	}
}

func TestCheckMapAccessEnforcesOwnership(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	mustRegister(t, g, impl.MapSpec{Name: "counters", Tenant: "t1", MaxEntries: 256})

	if err := g.CheckMapAccess("t1", "conn_stats", true); err != nil {
		t.Fatalf("writable map write access: %v", err)
	}
	if err := g.CheckMapAccess("t1", "counters", false); err != nil {
		t.Fatalf("read only map read access: %v", err)
	}
	if err := g.CheckMapAccess("t1", "counters", true); !errors.Is(err, impl.ErrMapReadOnly) {
		t.Fatalf("write to a read only map error = %v, want ErrMapReadOnly", err)
	}
	if err := g.CheckMapAccess("t2", "conn_stats", false); !errors.Is(err, impl.ErrMapTenantMismatch) {
		t.Fatalf("cross tenant map error = %v, want ErrMapTenantMismatch", err)
	}
	if err := g.CheckMapAccess("t1", "ghost", false); !errors.Is(err, impl.ErrUnknownMap) {
		t.Fatalf("unknown map error = %v, want ErrUnknownMap", err)
	}
}

func TestCheckInstructionsRejectsUnknownOp(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	prog := validProgram("guard-1")
	if err := g.CheckInstructions(prog); err != nil {
		t.Fatalf("whitelisted program must pass: %v", err)
	}
	prog.Insns = []impl.Instruction{{Op: "mov"}, {Op: "bpf_syscall"}, {Op: "exit"}}
	if err := g.CheckInstructions(prog); !errors.Is(err, impl.ErrOpNotAllowed) {
		t.Fatalf("unlisted op error = %v, want ErrOpNotAllowed", err)
	}
	blank := validProgram("")
	if err := g.CheckInstructions(blank); !errors.Is(err, impl.ErrBlankProgramName) {
		t.Fatalf("blank program name error = %v, want ErrBlankProgramName", err)
	}
	empty := validProgram("guard-2")
	empty.Insns = nil
	if err := g.CheckInstructions(empty); !errors.Is(err, impl.ErrEmptyProgram) {
		t.Fatalf("empty body error = %v, want ErrEmptyProgram", err)
	}
}

func TestCheckInstructionsRejectsOversizedProgram(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	prog := validProgram("guard-1")
	prog.Insns = []impl.Instruction{{Op: "mov"}, {Op: "mov"}, {Op: "mov"}, {Op: "mov"}, {Op: "exit"}}
	if err := g.CheckInstructions(prog); !errors.Is(err, impl.ErrProgramTooLarge) {
		t.Fatalf("oversized program error = %v, want ErrProgramTooLarge", err)
	}
}

func TestLoadRejectsDeniedProgramType(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	prog := validProgram("guard-1")
	prog.Type = "raw_tracepoint"
	if _, err := g.Load("t1", prog); !errors.Is(err, impl.ErrProgTypeDenied) {
		t.Fatalf("denied program type error = %v, want ErrProgTypeDenied", err)
	}
	if used, _ := g.Quota("t1"); used != 0 {
		t.Fatalf("rejected program must not consume quota, used=%d", used)
	}
	if st := g.Stats(); st.Rejected != 1 || st.Loaded != 0 {
		t.Fatalf("stats = %+v, want rejected 1 loaded 0", st)
	}
}

func TestLoadRequiresGplLicense(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	prog := validProgram("guard-1")
	prog.License = "MIT"
	if _, err := g.Load("t1", prog); !errors.Is(err, impl.ErrLicenseRequired) {
		t.Fatalf("non gpl license error = %v, want ErrLicenseRequired", err)
	}
	prog.License = "GPL"
	if _, err := g.Load("t1", prog); err != nil {
		t.Fatalf("gpl program must load: %v", err)
	}
}

func TestLoadReturnsHandleAndConsumesQuota(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	first, err := g.Load("t1", validProgram("guard-1"))
	if err != nil {
		t.Fatalf("load guard-1: %v", err)
	}
	second, err := g.Load("t1", validProgram("guard-2"))
	if err != nil {
		t.Fatalf("load guard-2: %v", err)
	}
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("handle ids = %q and %q, want two distinct non empty ids", first.ID, second.ID)
	}
	if first.Fingerprint == "" || second.Fingerprint == "" || first.Fingerprint == second.Fingerprint {
		t.Fatalf("fingerprints = %q and %q, want two distinct non empty values", first.Fingerprint, second.Fingerprint)
	}
	if first.State != impl.ProgLoaded || second.State != impl.ProgLoaded {
		t.Fatalf("fresh handles = %q/%q, want %q", first.State, second.State, impl.ProgLoaded)
	}
	used, limit := g.Quota("t1")
	if used != 2 || limit != 2 {
		t.Fatalf("quota = %d/%d, want 2/2", used, limit)
	}
	if st := g.Stats(); st.Loaded != 2 || st.Rejected != 0 {
		t.Fatalf("stats = %+v, want loaded 2 rejected 0", st)
	}
}

func TestLoadRejectsBeyondQuota(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	for _, name := range []string{"guard-1", "guard-2"} {
		if _, err := g.Load("t1", validProgram(name)); err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
	}
	if _, err := g.Load("t1", validProgram("guard-3")); !errors.Is(err, impl.ErrQuotaExceeded) {
		t.Fatalf("third load error = %v, want ErrQuotaExceeded", err)
	}
	if used, _ := g.Quota("t1"); used != 2 {
		t.Fatalf("quota after rejection = %d, want the untouched 2", used)
	}
}

func TestLoadRejectsUndeclaredMap(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	prog := validProgram("guard-1")
	prog.Maps = []string{"ghost_map"}
	prog.Insns = []impl.Instruction{{Op: "mov"}, {Op: "exit"}}
	if _, err := g.Load("t1", prog); !errors.Is(err, impl.ErrUnknownMap) {
		t.Fatalf("undeclared map error = %v, want ErrUnknownMap", err)
	}
	prog.Maps = []string{"conn_stats", "conn_stats", "conn_stats"}
	if _, err := g.Load("t1", prog); !errors.Is(err, impl.ErrTooManyMaps) {
		t.Fatalf("too many maps error = %v, want ErrTooManyMaps", err)
	}
	if used, _ := g.Quota("t1"); used != 0 {
		t.Fatalf("rejected loads must not consume quota, used=%d", used)
	}
}

func TestAttachRequiresLoadedProgram(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	if err := g.Attach("h-missing", impl.AttachTarget{Kind: "socket_filter", Name: "eth0"}); !errors.Is(err, impl.ErrUnknownHandle) {
		t.Fatalf("attach to unknown handle error = %v, want ErrUnknownHandle", err)
	}
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	h, err := g.Load("t1", validProgram("guard-1"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := g.Attach(h.ID, impl.AttachTarget{Kind: "socket_filter"}); !errors.Is(err, impl.ErrBadAttachTarget) {
		t.Fatalf("blank target name error = %v, want ErrBadAttachTarget", err)
	}
	if err := g.Attach(h.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth0", IfIndex: 2}); err != nil {
		t.Fatalf("attach to eth0: %v", err)
	}
}

func TestAttachLimitEnforced(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	h, err := g.Load("t1", validProgram("guard-1"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := g.Attach(h.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth0"}); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if err := g.Attach(h.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth1"}); !errors.Is(err, impl.ErrAttachLimit) {
		t.Fatalf("second attach error = %v, want ErrAttachLimit", err)
	}
	got, ok := g.Handle(h.ID)
	if !ok || len(got.Targets) != 1 || got.Targets[0] != "eth0" {
		t.Fatalf("handle targets = %v ok=%v, want only eth0", got.Targets, ok)
	}
}

func TestUnloadRefusesAttachedProgram(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	h, err := g.Load("t1", validProgram("guard-1"))
	if err != nil {
		t.Fatalf("load guard-1: %v", err)
	}
	if _, err := g.Load("t1", validProgram("guard-2")); err != nil {
		t.Fatalf("load guard-2: %v", err)
	}
	if err := g.Attach(h.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth0"}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := g.Unload(h.ID); !errors.Is(err, impl.ErrStillAttached) {
		t.Fatalf("unload while attached error = %v, want ErrStillAttached", err)
	}
	if err := g.Detach(h.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got, ok := g.Handle(h.ID)
	if !ok || got.State != impl.ProgLoaded || len(got.Targets) != 0 {
		t.Fatalf("handle after detach = %+v ok=%v, want loaded without targets", got, ok)
	}
	if err := g.Unload(h.ID); err != nil {
		t.Fatalf("unload after detach: %v", err)
	}
	if _, ok := g.Handle(h.ID); ok {
		t.Fatalf("unloaded handle must leave the table")
	}
	if err := g.Unload(h.ID); !errors.Is(err, impl.ErrUnknownHandle) {
		t.Fatalf("repeated unload error = %v, want ErrUnknownHandle", err)
	}
	if used, _ := g.Quota("t1"); used != 1 {
		t.Fatalf("quota after unload = %d, want the slot back (1)", used)
	}
	if err := g.Detach(h.ID); !errors.Is(err, impl.ErrUnknownHandle) {
		t.Fatalf("detach on a removed handle error = %v, want ErrUnknownHandle", err)
	}
}

func TestAttachedListPerTenant(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	first, err := g.Load("t1", validProgram("guard-1"))
	if err != nil {
		t.Fatalf("load guard-1: %v", err)
	}
	if _, err := g.Load("t1", validProgram("guard-2")); err != nil {
		t.Fatalf("load guard-2: %v", err)
	}
	if err := g.Attach(first.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth0"}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	attached := g.Attached("t1")
	if len(attached) != 1 || attached[0].ID != first.ID {
		t.Fatalf("attached list = %+v, want only %q", attached, first.ID)
	}
	if other := g.Attached("t2"); len(other) != 0 {
		t.Fatalf("other tenant attached list = %+v, want empty", other)
	}
}

func TestSnapshotRestoreKeepsProgramsAndQuota(t *testing.T) {
	g := impl.NewEbpfLoadGate(gatePolicy())
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})
	first, err := g.Load("t1", validProgram("guard-1"))
	if err != nil {
		t.Fatalf("load guard-1: %v", err)
	}
	if _, err := g.Load("t1", validProgram("guard-2")); err != nil {
		t.Fatalf("load guard-2: %v", err)
	}
	if err := g.Attach(first.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth0"}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewEbpfLoadGate(gatePolicy())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if used, limit := restored.Quota("t1"); used != 2 || limit != 2 {
		t.Fatalf("restored quota = %d/%d, want 2/2", used, limit)
	}
	got, ok := restored.Handle(first.ID)
	if !ok || got.State != impl.ProgAttached || len(got.Targets) != 1 || got.Targets[0] != "eth0" {
		t.Fatalf("restored handle = %+v ok=%v, want attached to eth0", got, ok)
	}
	if err := restored.Attach(first.ID, impl.AttachTarget{Kind: "socket_filter", Name: "eth1"}); !errors.Is(err, impl.ErrAttachLimit) {
		t.Fatalf("restored attach budget error = %v, want ErrAttachLimit", err)
	}
	if before, after := g.Stats(), restored.Stats(); before != after {
		t.Fatalf("restored stats = %+v, want %+v", after, before)
	}
	if err := restored.Restore([]byte("not a snapshot payload")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("garbage snapshot error = %v, want ErrBadSnapshot", err)
	}
	if used, _ := restored.Quota("t1"); used != 2 {
		t.Fatalf("failed restore must leave the quota untouched, used=%d", used)
	}
}

func TestConcurrentLoadRespectsQuota(t *testing.T) {
	policy := gatePolicy()
	policy.MaxPrograms = 3
	g := impl.NewEbpfLoadGate(policy)
	mustRegister(t, g, impl.MapSpec{Name: "conn_stats", Tenant: "t1", MaxEntries: 1024, Writable: true})

	var (
		mu        sync.Mutex
		ok        int
		exhausted int
		other     []error
		wg        sync.WaitGroup
	)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			prog := validProgram("racer")
			prog.Insns = []impl.Instruction{{Op: "mov", Imm: int64(n)}, {Op: "exit"}}
			_, err := g.Load("t1", prog)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, impl.ErrQuotaExceeded):
				exhausted++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()
	if len(other) != 0 {
		t.Fatalf("unexpected load errors: %v", other)
	}
	if ok != 3 || exhausted != 9 {
		t.Fatalf("concurrent loads ok=%d exhausted=%d, want 3 and 9", ok, exhausted)
	}
	if used, _ := g.Quota("t1"); used != 3 {
		t.Fatalf("quota after the race = %d, want 3", used)
	}
}
