package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-08"
)

func gateConfig() impl.GateConfig {
	return impl.GateConfig{
		TrustedRoots: []string{"root-01"},
		MaxModules:   8,
	}
}

func validChain() []impl.CertLink {
	return []impl.CertLink{
		{Subject: "leaf-nvme", Issuer: "corp-root", Fingerprint: "leaf-01", NotAfter: 5000},
		{Subject: "corp-root", Issuer: "corp-root", Fingerprint: "root-01", NotAfter: 5000},
	}
}

func signedRequest(name, hash string, at int64) impl.ModuleRequest {
	return impl.ModuleRequest{
		Name:      name,
		Version:   "1.2.0",
		Hash:      hash,
		SignerKey: "leaf-01",
		Signature: impl.ModuleSignature{Sig: "sig-bytes", Chain: validChain()},
		At:        at,
	}
}

func unsignedRequest(name, hash string, at int64) impl.ModuleRequest {
	return impl.ModuleRequest{
		Name:     name,
		Version:  "0.9.0",
		Hash:     hash,
		Unsigned: true,
		At:       at,
	}
}

func TestLoadSignedModuleAccepted(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	res, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000))
	if err != nil {
		t.Fatalf("load a module with a valid chain: %v", err)
	}
	if !res.Loaded || !res.Signed {
		t.Fatalf("result = %+v, want loaded and signed", res)
	}
	if res.Taint != 0 {
		t.Fatalf("taint raised by a signed load = %d, want 0", res.Taint)
	}
	modules := g.Modules()
	if len(modules) != 1 {
		t.Fatalf("modules = %d, want 1", len(modules))
	}
	if modules[0].Name != "nvme-rdma" || !modules[0].Signed {
		t.Fatalf("module row = %+v, want a signed nvme-rdma", modules[0])
	}
	if modules[0].Seq <= 0 {
		t.Fatalf("module seq = %d, want a positive number", modules[0].Seq)
	}
	if got := g.Taint().Bits; got != 0 {
		t.Fatalf("taint bits = %d, want 0 for a signed load", got)
	}
}

func TestLoadRejectsBlankModuleIdentity(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	for _, req := range []impl.ModuleRequest{
		{Version: "1.0", Hash: "sha256:aa", Unsigned: true, At: 1000},
		{Name: "zfs", Hash: "sha256:aa", Unsigned: true, At: 1000},
		{Name: "zfs", Version: "1.0", Unsigned: true, At: 1000},
		{Name: "../etc/passwd", Version: "1.0", Hash: "sha256:aa", Unsigned: true, At: 1000},
	} {
		if _, err := g.Load(req); !errors.Is(err, impl.ErrBadModule) {
			t.Fatalf("invalid module identity %+v must fail with ErrBadModule, got %v", req, err)
		}
	}
	if got := len(g.Modules()); got != 0 {
		t.Fatalf("rejected loads must not populate the table, got %d modules", got)
	}
}

func TestLoadRejectsBrokenChain(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	req := signedRequest("nvme-rdma", "sha256:aa", 1000)
	req.Signature.Chain = []impl.CertLink{
		{Subject: "leaf-nvme", Issuer: "mid-ca", Fingerprint: "leaf-01", NotAfter: 5000},
		{Subject: "corp-root", Issuer: "corp-root", Fingerprint: "root-01", NotAfter: 5000},
	}
	if _, err := g.Load(req); !errors.Is(err, impl.ErrChainBroken) {
		t.Fatalf("chain with a missing intermediate must fail with ErrChainBroken, got %v", err)
	}
	empty := signedRequest("nvme-rdma", "sha256:aa", 1000)
	empty.Signature.Chain = nil
	if _, err := g.Load(empty); !errors.Is(err, impl.ErrChainBroken) {
		t.Fatalf("empty chain must fail with ErrChainBroken, got %v", err)
	}
	if got := len(g.Modules()); got != 0 {
		t.Fatalf("a broken chain must not load, got %d modules", got)
	}
}

func TestLoadRejectsUntrustedRoot(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	req := signedRequest("nvme-rdma", "sha256:aa", 1000)
	req.Signature.Chain[1].Fingerprint = "root-99"
	if _, err := g.Load(req); !errors.Is(err, impl.ErrUntrustedRoot) {
		t.Fatalf("chain rooted outside the trust set must fail with ErrUntrustedRoot, got %v", err)
	}
	if got := len(g.Modules()); got != 0 {
		t.Fatalf("an untrusted chain must not load, got %d modules", got)
	}
}

func TestLoadRejectsExpiredCertificate(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	req := signedRequest("nvme-rdma", "sha256:aa", 1000)
	req.Signature.Chain[0].NotAfter = 900
	if _, err := g.Load(req); !errors.Is(err, impl.ErrCertExpired) {
		t.Fatalf("expired leaf certificate must fail with ErrCertExpired, got %v", err)
	}
	if got := len(g.Modules()); got != 0 {
		t.Fatalf("an expired chain must not load, got %d modules", got)
	}
}

func TestWhitelistSkipsSignatureButChecksHash(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	if err := g.AllowModule(impl.Policy{Name: "zfs", Hash: "sha256:zz", Allow: true}); err != nil {
		t.Fatalf("register whitelist: %v", err)
	}
	res, err := g.Load(unsignedRequest("zfs", "sha256:zz", 1000))
	if err != nil {
		t.Fatalf("whitelisted module with the recorded hash: %v", err)
	}
	if !res.PolicyExempt {
		t.Fatalf("result = %+v, want a policy exempt load", res)
	}
	if res.Signed {
		t.Fatalf("a whitelisted unsigned module must not be reported as signed")
	}
	if got := g.Taint().Bits; got != 0 {
		t.Fatalf("taint bits = %d, want 0 for a whitelisted module", got)
	}
	if _, err := g.Load(unsignedRequest("zfs", "sha256:other", 1100)); !errors.Is(err, impl.ErrPolicyDenied) {
		t.Fatalf("whitelisted name with another hash must fail with ErrPolicyDenied, got %v", err)
	}
	if got := len(g.Modules()); got != 1 {
		t.Fatalf("modules = %d, want 1 after the denied load", got)
	}
}

func TestUnsignedLoadTaintsKernel(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	res, err := g.Load(unsignedRequest("vendor-accel", "sha256:bb", 1200))
	if err != nil {
		t.Fatalf("unsigned load in permissive mode: %v", err)
	}
	if res.Signed {
		t.Fatalf("an unsigned module must not be reported as signed")
	}
	state := g.Taint()
	if state.Bits != impl.TaintUnsignedModule {
		t.Fatalf("taint bits = %d, want %d", state.Bits, impl.TaintUnsignedModule)
	}
	if state.Since != 1200 {
		t.Fatalf("taint since = %d, want the load time 1200", state.Since)
	}
	if res.Taint != impl.TaintUnsignedModule {
		t.Fatalf("result taint = %d, want %d", res.Taint, impl.TaintUnsignedModule)
	}
}

func TestDuplicateLoadIsIdempotent(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	first, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000))
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.AlreadyLoaded {
		t.Fatalf("first load must not report an existing module")
	}
	second, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1100))
	if err != nil {
		t.Fatalf("repeated load of the same module: %v", err)
	}
	if !second.AlreadyLoaded {
		t.Fatalf("repeated load must report AlreadyLoaded, got %+v", second)
	}
	if got := len(g.Modules()); got != 1 {
		t.Fatalf("modules = %d, want 1 after a repeated load", got)
	}
	other := signedRequest("nvme-rdma", "sha256:cc", 1200)
	if _, err := g.Load(other); !errors.Is(err, impl.ErrHashMismatch) {
		t.Fatalf("same name with a different hash must fail with ErrHashMismatch, got %v", err)
	}
	if got := len(g.Modules()); got != 1 {
		t.Fatalf("hash mismatch must not add a row, got %d modules", got)
	}
}

func TestModuleTableCapEnforced(t *testing.T) {
	cfg := gateConfig()
	cfg.MaxModules = 1
	g := impl.NewModuleSignGate(cfg)
	if _, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000)); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, err := g.Load(signedRequest("zfs", "sha256:zz", 1000)); !errors.Is(err, impl.ErrModuleLimit) {
		t.Fatalf("second module must hit ErrModuleLimit, got %v", err)
	}
	modules := g.Modules()
	if len(modules) != 1 || modules[0].Name != "nvme-rdma" {
		t.Fatalf("modules = %+v, want only nvme-rdma", modules)
	}
}

func TestUnloadUnknownIsIdempotent(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	removed, err := g.Unload("ghost")
	if err != nil {
		t.Fatalf("unloading an unknown module must not fail, got %v", err)
	}
	if removed {
		t.Fatalf("unloading an unknown module must report false")
	}
	if _, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000)); err != nil {
		t.Fatalf("load: %v", err)
	}
	removed, err = g.Unload("nvme-rdma")
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if !removed {
		t.Fatalf("unloading a resident module must report true")
	}
	if got := len(g.Modules()); got != 0 {
		t.Fatalf("modules after unload = %d, want 0", got)
	}
}

func TestEnforceModeRejectsUnsignedLoad(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	if err := g.SetEnforce(true, 500); err != nil {
		t.Fatalf("enable enforcement: %v", err)
	}
	if _, err := g.Load(unsignedRequest("vendor-accel", "sha256:bb", 1000)); !errors.Is(err, impl.ErrEnforceDenied) {
		t.Fatalf("unsigned load under enforcement must fail with ErrEnforceDenied, got %v", err)
	}
	if _, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000)); err != nil {
		t.Fatalf("signed load under enforcement must succeed, got %v", err)
	}
	modules := g.Modules()
	if len(modules) != 1 || !modules[0].Signed {
		t.Fatalf("modules = %+v, want one signed module", modules)
	}
}

func TestTaintBitsAccumulateAndAckClearsOne(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	if _, err := g.Load(unsignedRequest("vendor-accel", "sha256:bb", 1000)); err != nil {
		t.Fatalf("unsigned load: %v", err)
	}
	outOfTree := signedRequest("vendor-net", "sha256:cc", 1100)
	outOfTree.OutOfTree = true
	if _, err := g.Load(outOfTree); err != nil {
		t.Fatalf("out of tree load: %v", err)
	}
	if got := g.Taint().Bits; got != impl.TaintUnsignedModule|impl.TaintOutOfTree {
		t.Fatalf("taint bits = %d, want %d", got, impl.TaintUnsignedModule|impl.TaintOutOfTree)
	}
	if err := g.AckTaint(impl.TaintOutOfTree, 1200); err != nil {
		t.Fatalf("ack the out of tree bit: %v", err)
	}
	if got := g.Taint().Bits; got != impl.TaintUnsignedModule {
		t.Fatalf("taint bits after ack = %d, want only %d", got, impl.TaintUnsignedModule)
	}
	if err := g.AckTaint(0, 1300); !errors.Is(err, impl.ErrBadTaint) {
		t.Fatalf("acking zero bits must fail with ErrBadTaint, got %v", err)
	}
	if got := g.Taint().Bits; got != impl.TaintUnsignedModule {
		t.Fatalf("a rejected ack must not clear bits, got %d", got)
	}
}

func TestSnapshotRestoreKeepsTaintAndEnforce(t *testing.T) {
	cfg := gateConfig()
	g := impl.NewModuleSignGate(cfg)
	if _, err := g.Load(unsignedRequest("vendor-accel", "sha256:bb", 1000)); err != nil {
		t.Fatalf("unsigned load: %v", err)
	}
	if _, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1100)); err != nil {
		t.Fatalf("signed load: %v", err)
	}
	if err := g.SetEnforce(true, 1200); err != nil {
		t.Fatalf("enable enforcement: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewModuleSignGate(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(restored.Modules()); got != 2 {
		t.Fatalf("restored modules = %d, want 2", got)
	}
	if got := restored.Taint().Bits; got != impl.TaintUnsignedModule {
		t.Fatalf("restored taint bits = %d, want %d", got, impl.TaintUnsignedModule)
	}
	if _, err := restored.Load(unsignedRequest("vendor-extra", "sha256:dd", 1300)); !errors.Is(err, impl.ErrEnforceDenied) {
		t.Fatalf("restore must keep enforcement on, got %v", err)
	}
	res, err := restored.Load(signedRequest("zfs", "sha256:zz", 1400))
	if err != nil {
		t.Fatalf("signed load after restore: %v", err)
	}
	if res.Loaded == false || res.AlreadyLoaded {
		t.Fatalf("unexpected result after restore: %+v", res)
	}
	modules := restored.Modules()
	if modules[len(modules)-1].Seq <= 0 {
		t.Fatalf("module sequence after restore must stay positive")
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	if _, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := g.Restore([]byte("not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(g.Modules()); got != 1 {
		t.Fatalf("failed restore must not touch live modules, got %d", got)
	}
	if got := g.Taint().Bits; got != 0 {
		t.Fatalf("failed restore must not touch live taint, got %d", got)
	}
}

func TestAuditRecordsEveryAttempt(t *testing.T) {
	g := impl.NewModuleSignGate(gateConfig())
	if _, err := g.Load(signedRequest("nvme-rdma", "sha256:aa", 1000)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := g.Load(impl.ModuleRequest{Name: "", Version: "1.0", Hash: "h", Unsigned: true, At: 1100}); !errors.Is(err, impl.ErrBadModule) {
		t.Fatalf("blank module must be rejected, got %v", err)
	}
	if _, err := g.Unload("nvme-rdma"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	audit := g.Audit()
	if len(audit) != 3 {
		t.Fatalf("audit entries = %d, want 3", len(audit))
	}
	wantOutcomes := []string{impl.OutcomeLoaded, impl.OutcomeRejected, impl.OutcomeUnloaded}
	for i, want := range wantOutcomes {
		if audit[i].Outcome != want {
			t.Fatalf("audit[%d] outcome = %q, want %q", i, audit[i].Outcome, want)
		}
	}
	for i := 1; i < len(audit); i++ {
		if audit[i].Seq <= audit[i-1].Seq {
			t.Fatalf("audit sequence must be strictly increasing, got %d then %d", audit[i-1].Seq, audit[i].Seq)
		}
	}
	if audit[0].Name != "nvme-rdma" || audit[2].Name != "nvme-rdma" {
		t.Fatalf("audit rows must carry the module name: %+v", audit)
	}
}
