package task

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-01"
)

func guardOptions() impl.Options {
	return impl.Options{
		TenantSocketLimit: 4,
		TenantConnLimit:   2,
		FDMaxPerConn:      3,
		FDBytesPerConn:    8192,
		LeaseTTL:          30 * time.Second,
		PeerReader: func(tenantID, name string) impl.PeerCred {
			return impl.PeerCred{PID: 4242, UID: 1000, GID: 1000, Cgroup: "gsb/" + tenantID + "/job"}
		},
	}
}

func testPolicy() impl.PeerPolicy {
	return impl.PeerPolicy{
		RequireSameCgroup: true,
		AllowedKinds:      []string{impl.FDSealedMemfd, impl.FDPidfd},
		MaxFDs:            3,
		MaxFDBytes:        8192,
	}
}

func TestListenEncodesTenantAndJobIntoName(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	ep, err := g.Listen("t-1", "job-7", "control")
	if err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	if ep.Name != "@gsb/t-1/job-7/control" {
		t.Fatalf("encoded name = %q, want %q", ep.Name, "@gsb/t-1/job-7/control")
	}
	if ep.Kind != impl.NamespaceAbstract {
		t.Fatalf("namespace kind = %q, want %q", ep.Kind, impl.NamespaceAbstract)
	}
	if ep.Path != "" {
		t.Fatalf("abstract endpoint must not carry a filesystem path, got %q", ep.Path)
	}
	if ep.TenantID != "t-1" || ep.JobID != "job-7" {
		t.Fatalf("endpoint identity = %q/%q, want t-1/job-7", ep.TenantID, ep.JobID)
	}
	if !ep.LeaseExpires.After(time.Now()) {
		t.Fatalf("lease must expire in the future, got %v", ep.LeaseExpires)
	}
}

func TestListenRejectsMalformedNames(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	bad := []struct{ tenant, job, name string }{
		{"", "job-1", "control"},
		{"t-1", "  ", "control"},
		{"t-1", "job-1", ""},
		{"t-1", "job-1", "a/b"},
		{"t-1", "job-1", strings.Repeat("x", 130)},
	}
	for _, c := range bad {
		if _, err := g.Listen(c.tenant, c.job, c.name); !errors.Is(err, impl.ErrBadName) {
			t.Fatalf("listen(%q,%q,%q) err = %v, want ErrBadName", c.tenant, c.job, c.name, err)
		}
	}
	if got := len(g.Audit()); got != 0 {
		t.Fatalf("rejected names must not create bindings, got %d", got)
	}
}

func TestListenSameNameOnlyOneBindingWins(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, taken := 0, 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.Listen("t-1", "job-1", "control")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, impl.ErrNameTaken):
				taken++
			}
		}()
	}
	wg.Wait()
	if ok != 1 || taken != racers-1 {
		t.Fatalf("concurrent listen: ok=%d taken=%d, want ok=1 taken=%d", ok, taken, racers-1)
	}
	if got := len(g.Audit()); got != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", got)
	}
}

func TestListenEnforcesTenantSocketLimit(t *testing.T) {
	opts := guardOptions()
	opts.TenantSocketLimit = 2
	g := impl.NewUnixSocketNamespaceGuard(opts)
	for _, name := range []string{"a", "b"} {
		if _, err := g.Listen("t-1", "job-1", name); err != nil {
			t.Fatalf("listen %s must succeed, got %v", name, err)
		}
	}
	if _, err := g.Listen("t-1", "job-1", "c"); !errors.Is(err, impl.ErrSocketLimit) {
		t.Fatalf("third listen err = %v, want ErrSocketLimit", err)
	}
	if _, err := g.Listen("t-2", "job-9", "a"); err != nil {
		t.Fatalf("other tenant must keep its own quota, got %v", err)
	}
	if got := len(g.BindingsFor("t-1")); got != 2 {
		t.Fatalf("tenant t-1 bindings = %d, want 2", got)
	}
}

func TestDialRequiresSameTenant(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	if _, err := g.Dial("t-2", "@gsb/t-1/job-1/control", testPolicy()); !errors.Is(err, impl.ErrTenantMismatch) {
		t.Fatalf("cross tenant dial err = %v, want ErrTenantMismatch", err)
	}
	conn, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy())
	if err != nil {
		t.Fatalf("owner tenant dial must succeed, got %v", err)
	}
	if conn.ID == "" || conn.TenantID != "t-1" {
		t.Fatalf("conn = %+v, want non empty id and tenant t-1", conn)
	}
}

func TestDialUnknownNameReturnsErrNoBinding(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	if _, err := g.Dial("t-1", "@gsb/t-1/job-1/gone", testPolicy()); !errors.Is(err, impl.ErrNoBinding) {
		t.Fatalf("dial on unknown name err = %v, want ErrNoBinding", err)
	}
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	if _, err := g.Dial("t-1", "control", testPolicy()); !errors.Is(err, impl.ErrNoBinding) {
		t.Fatalf("dial with the bare job local name err = %v, want ErrNoBinding", err)
	}
}

func TestDialEnforcesTenantConnLimit(t *testing.T) {
	opts := guardOptions()
	opts.TenantConnLimit = 2
	g := impl.NewUnixSocketNamespaceGuard(opts)
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy()); err != nil {
			t.Fatalf("dial %d must succeed, got %v", i, err)
		}
	}
	if _, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy()); !errors.Is(err, impl.ErrConnLimit) {
		t.Fatalf("third dial err = %v, want ErrConnLimit", err)
	}
}

func TestDialRejectsForeignCgroup(t *testing.T) {
	opts := guardOptions()
	opts.PeerReader = func(tenantID, name string) impl.PeerCred {
		return impl.PeerCred{PID: 5150, UID: 1000, GID: 1000, Cgroup: "gsb/t-2/job-9"}
	}
	g := impl.NewUnixSocketNamespaceGuard(opts)
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	if _, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy()); !errors.Is(err, impl.ErrPeerUntrusted) {
		t.Fatalf("peer in another cgroup err = %v, want ErrPeerUntrusted", err)
	}
	rows := g.BindingsFor("t-1")
	if len(rows) != 1 || rows[0].Conns != 0 {
		t.Fatalf("rejected peer must not be counted, rows = %+v", rows)
	}
}

func TestAdmitFDAllowsOnlyWhitelistedKinds(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	conn, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy())
	if err != nil {
		t.Fatalf("dial must succeed, got %v", err)
	}
	if err := g.AdmitFD(conn.ID, impl.FDSealedMemfd, 4096, time.Now()); err != nil {
		t.Fatalf("whitelisted memfd must be admitted, got %v", err)
	}
	if err := g.AdmitFD(conn.ID, impl.FDRegularFile, 16, time.Now()); !errors.Is(err, impl.ErrFDKindDenied) {
		t.Fatalf("regular file fd err = %v, want ErrFDKindDenied", err)
	}
	if left := g.FDBudgetLeft(conn.ID); left != 8192-4096 {
		t.Fatalf("remaining budget = %d, want %d", left, 8192-4096)
	}
}

func TestAdmitFDEnforcesPerConnCount(t *testing.T) {
	opts := guardOptions()
	opts.FDMaxPerConn = 2
	g := impl.NewUnixSocketNamespaceGuard(opts)
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	conn, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy())
	if err != nil {
		t.Fatalf("dial must succeed, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := g.AdmitFD(conn.ID, impl.FDPidfd, 64, time.Now()); err != nil {
			t.Fatalf("admit %d must succeed, got %v", i, err)
		}
	}
	if err := g.AdmitFD(conn.ID, impl.FDPidfd, 64, time.Now()); !errors.Is(err, impl.ErrFDBudget) {
		t.Fatalf("third admit err = %v, want ErrFDBudget", err)
	}
}

func TestAdmitFDEnforcesByteBudget(t *testing.T) {
	opts := guardOptions()
	opts.FDBytesPerConn = 8192
	g := impl.NewUnixSocketNamespaceGuard(opts)
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	conn, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy())
	if err != nil {
		t.Fatalf("dial must succeed, got %v", err)
	}
	if err := g.AdmitFD(conn.ID, impl.FDSealedMemfd, 6000, time.Now()); err != nil {
		t.Fatalf("first admit must succeed, got %v", err)
	}
	if err := g.AdmitFD(conn.ID, impl.FDSealedMemfd, 4000, time.Now()); !errors.Is(err, impl.ErrFDBudget) {
		t.Fatalf("over budget admit err = %v, want ErrFDBudget", err)
	}
	if left := g.FDBudgetLeft(conn.ID); left != 2192 {
		t.Fatalf("remaining budget = %d, want 2192", left)
	}
}

func TestReleaseDropsBindingAndFreesQuota(t *testing.T) {
	opts := guardOptions()
	opts.TenantSocketLimit = 1
	opts.TenantConnLimit = 1
	g := impl.NewUnixSocketNamespaceGuard(opts)
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	conn, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy())
	if err != nil {
		t.Fatalf("dial must succeed, got %v", err)
	}
	if err := g.AdmitFD(conn.ID, impl.FDSealedMemfd, 1024, time.Now()); err != nil {
		t.Fatalf("admit must succeed, got %v", err)
	}
	if err := g.Release("job-1"); err != nil {
		t.Fatalf("release must succeed, got %v", err)
	}
	if got := len(g.Audit()); got != 0 {
		t.Fatalf("audit rows after release = %d, want 0", got)
	}
	if _, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy()); !errors.Is(err, impl.ErrNoBinding) {
		t.Fatalf("dial after release err = %v, want ErrNoBinding", err)
	}
	if _, err := g.Listen("t-1", "job-2", "control"); err != nil {
		t.Fatalf("socket quota must be freed, got %v", err)
	}
	if _, err := g.Dial("t-1", "@gsb/t-1/job-2/control", testPolicy()); err != nil {
		t.Fatalf("connection quota must be freed, got %v", err)
	}
}

func TestReleaseUnknownJobIsIdempotent(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	if err := g.Release("job-does-not-exist"); err != nil {
		t.Fatalf("release of an unknown job must be a no op, got %v", err)
	}
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	if err := g.Release("job-1"); err != nil {
		t.Fatalf("first release must succeed, got %v", err)
	}
	if err := g.Release("job-1"); err != nil {
		t.Fatalf("repeated release must stay a no op, got %v", err)
	}
}

func TestAuditOrdersBindingsBySequence(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	for _, spec := range []struct{ tenant, job, name string }{
		{"t-1", "job-1", "a"},
		{"t-1", "job-2", "b"},
		{"t-2", "job-3", "c"},
	} {
		if _, err := g.Listen(spec.tenant, spec.job, spec.name); err != nil {
			t.Fatalf("listen %s/%s must succeed, got %v", spec.tenant, spec.name, err)
		}
	}
	rows := g.Audit()
	if len(rows) != 3 {
		t.Fatalf("audit rows = %d, want 3", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Seq <= rows[i-1].Seq {
			t.Fatalf("audit sequence must grow: rows[%d]=%d rows[%d]=%d", i-1, rows[i-1].Seq, i, rows[i].Seq)
		}
	}
	for _, row := range rows {
		if row.State != impl.BindingBound {
			t.Fatalf("row %+v must be bound", row)
		}
		if row.Kind != impl.NamespaceAbstract {
			t.Fatalf("row %+v must be an abstract binding", row)
		}
	}
	if got := len(g.BindingsFor("t-1")); got != 2 {
		t.Fatalf("bindings for t-1 = %d, want 2", got)
	}
}

func TestPeerIdentityReportsRecordedCredentials(t *testing.T) {
	g := impl.NewUnixSocketNamespaceGuard(guardOptions())
	if _, err := g.Listen("t-1", "job-1", "control"); err != nil {
		t.Fatalf("listen must succeed, got %v", err)
	}
	conn, err := g.Dial("t-1", "@gsb/t-1/job-1/control", testPolicy())
	if err != nil {
		t.Fatalf("dial must succeed, got %v", err)
	}
	cred, err := g.PeerIdentity(conn.ID)
	if err != nil {
		t.Fatalf("peer identity must be readable, got %v", err)
	}
	if cred.PID != 4242 || cred.UID != 1000 || cred.GID != 1000 {
		t.Fatalf("recorded credentials = %+v, want pid 4242 uid 1000 gid 1000", cred)
	}
	if cred.Tenant != "t-1" {
		t.Fatalf("recorded tenant = %q, want t-1", cred.Tenant)
	}
	if _, err := g.PeerIdentity("conn-missing"); !errors.Is(err, impl.ErrNoBinding) {
		t.Fatalf("unknown connection err = %v, want ErrNoBinding", err)
	}
}
