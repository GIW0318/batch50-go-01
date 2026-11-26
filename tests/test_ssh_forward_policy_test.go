package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-01"
)

func policyConfig() impl.PolicyConfig {
	return impl.PolicyConfig{
		MaxSessionsPerTenant:  2,
		MaxChannelsPerSession: 3,
		MaxBytesPerSession:    4096,
		PortRangeLow:          20000,
		PortRangeHigh:         20010,
		LeaseTTL:              10 * time.Minute,
		LeaseQueueDepth:       2,
		Rules: map[string]impl.TenantRule{
			"tenant-a": {
				Hosts:         []string{"10.20.30.40", "db.internal", "cache.internal"},
				Ports:         []int{5432, 6379},
				ForbiddenNets: []string{"10.99.0.0/16"},
			},
		},
		Resolver: func(host string) []string {
			switch host {
			case "db.internal":
				return []string{"10.20.30.40"}
			case "cache.internal":
				return []string{"10.60.0.9"}
			}
			return nil
		},
	}
}

func mustRegister(t *testing.T, p *impl.SshForwardPolicy, id, tenant string) impl.Session {
	t.Helper()
	s, err := p.RegisterSession(id, tenant, 1)
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	return s
}

func TestRegisterSessionCreatesActiveSession(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	s, err := p.RegisterSession("sess-1", "tenant-a", 5)
	if err != nil {
		t.Fatalf("register session: %v", err)
	}
	if s.ID != "sess-1" || s.Tenant != "tenant-a" || s.Priority != 5 {
		t.Fatalf("session identity mismatch: %+v", s)
	}
	if s.State != impl.SessionActive {
		t.Fatalf("state = %q, want %q", s.State, impl.SessionActive)
	}
	if s.Channels != 0 || s.Bytes != 0 || s.RemotePort != 0 {
		t.Fatalf("fresh session must start empty, got %+v", s)
	}
	stored, err := p.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if stored.Tenant != "tenant-a" || stored.Priority != 5 {
		t.Fatalf("stored session mismatch: %+v", stored)
	}
	if got := p.SessionCount("tenant-a"); got != 1 {
		t.Fatalf("tenant session count = %d, want 1", got)
	}
}

func TestRegisterSessionRejectsBlankAndDuplicate(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	if _, err := p.RegisterSession("   ", "tenant-a", 1); !errors.Is(err, impl.ErrBlankSession) {
		t.Fatalf("blank session id: err = %v, want ErrBlankSession", err)
	}
	if _, err := p.RegisterSession("sess-2", "", 1); !errors.Is(err, impl.ErrBlankSession) {
		t.Fatalf("blank tenant: err = %v, want ErrBlankSession", err)
	}
	if got := p.SessionCount("tenant-a"); got != 0 {
		t.Fatalf("rejected registrations left %d sessions", got)
	}
	mustRegister(t, p, "sess-2", "tenant-a")
	if _, err := p.RegisterSession("sess-2", "tenant-b", 1); !errors.Is(err, impl.ErrSessionExists) {
		t.Fatalf("duplicate session: err = %v, want ErrSessionExists", err)
	}
	if got := p.SessionCount("tenant-a"); got != 1 {
		t.Fatalf("tenant session count = %d, want 1", got)
	}
}

func TestRegisterSessionEnforcesTenantLimit(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-a1", "tenant-a")
	mustRegister(t, p, "sess-a2", "tenant-a")
	if _, err := p.RegisterSession("sess-a3", "tenant-a", 1); !errors.Is(err, impl.ErrTenantSessionLimit) {
		t.Fatalf("third session: err = %v, want ErrTenantSessionLimit", err)
	}
	if got := p.SessionCount("tenant-a"); got != 2 {
		t.Fatalf("tenant session count = %d, want 2", got)
	}
	if _, err := p.RegisterSession("sess-b1", "tenant-b", 1); err != nil {
		t.Fatalf("other tenant must stay unaffected, got %v", err)
	}
	if _, err := p.SessionState("sess-a3"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("rejected session must not exist, err = %v", err)
	}
}

func TestCheckDirectAllowsListedHostAndPort(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	d, err := p.CheckDirect("sess-1", impl.HostPort{Host: "10.20.30.40", Port: 5432})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if !d.Allow {
		t.Fatalf("listed destination denied with reason %q", d.Reason)
	}
	if d.Reason != impl.ReasonAllow {
		t.Fatalf("reason = %q, want %q", d.Reason, impl.ReasonAllow)
	}
	if d.Target != "10.20.30.40:5432" {
		t.Fatalf("target = %q, want 10.20.30.40:5432", d.Target)
	}
	s, err := p.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.Channels != 1 {
		t.Fatalf("channel count = %d, want 1", s.Channels)
	}
}

func TestCheckDirectRejectsHostOrPortOutsideRule(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	d, err := p.CheckDirect("sess-1", impl.HostPort{Host: "10.20.30.41", Port: 5432})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedNotListed {
		t.Fatalf("unknown host: allow=%v reason=%q", d.Allow, d.Reason)
	}
	d, err = p.CheckDirect("sess-1", impl.HostPort{Host: "cache.internal", Port: 22})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedPort {
		t.Fatalf("unknown port: allow=%v reason=%q", d.Allow, d.Reason)
	}
}

func TestCheckDirectRejectsLoopbackAndMetadata(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	d, err := p.CheckDirect("sess-1", impl.HostPort{Host: "127.0.0.1", Port: 5432})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedLoopback {
		t.Fatalf("loopback: allow=%v reason=%q", d.Allow, d.Reason)
	}
	d, err = p.CheckDirect("sess-1", impl.HostPort{Host: "169.254.169.254", Port: 80})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedMetadata {
		t.Fatalf("metadata: allow=%v reason=%q", d.Allow, d.Reason)
	}
	d, err = p.CheckDirect("sess-1", impl.HostPort{Host: "::1", Port: 6379})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedLoopback {
		t.Fatalf("ipv6 loopback: allow=%v reason=%q", d.Allow, d.Reason)
	}
}

func TestCheckDirectRejectsForbiddenNetwork(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	d, err := p.CheckDirect("sess-1", impl.HostPort{Host: "10.99.7.7", Port: 5432})
	if err != nil {
		t.Fatalf("check direct: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedCIDR {
		t.Fatalf("forbidden network: allow=%v reason=%q", d.Allow, d.Reason)
	}
}

func TestCheckDirectEnforcesChannelQuota(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	for i := 0; i < 3; i++ {
		d, err := p.CheckDirect("sess-1", impl.HostPort{Host: "10.20.30.40", Port: 5432})
		if err != nil {
			t.Fatalf("check direct %d: %v", i, err)
		}
		if !d.Allow {
			t.Fatalf("check direct %d denied with %q", i, d.Reason)
		}
	}
	d, err := p.CheckDirect("sess-1", impl.HostPort{Host: "10.20.30.40", Port: 5432})
	if err != nil {
		t.Fatalf("check direct over quota: %v", err)
	}
	if d.Allow || d.Reason != impl.ReasonDeniedChannelCap {
		t.Fatalf("over quota: allow=%v reason=%q", d.Allow, d.Reason)
	}
	s, err := p.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.Channels != 3 {
		t.Fatalf("channel count = %d, want 3", s.Channels)
	}
}

func TestAllocateRemoteBindsPortInsideRange(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	b, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 20005})
	if err != nil {
		t.Fatalf("allocate remote: %v", err)
	}
	if b.Port != 20005 {
		t.Fatalf("bound port = %d, want 20005", b.Port)
	}
	if b.LeaseID == "" {
		t.Fatalf("lease id must not be empty")
	}
	if b.SessionID != "sess-1" {
		t.Fatalf("lease session = %q, want sess-1", b.SessionID)
	}
	s, err := p.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.RemotePort != 20005 {
		t.Fatalf("session remote port = %d, want 20005", s.RemotePort)
	}
	if pool := p.LeasePool(); len(pool) != 1 || pool[0].Port != 20005 {
		t.Fatalf("lease pool = %+v, want one lease on 20005", pool)
	}
}

func TestAllocateRemoteRejectsPortOutsideRange(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	if _, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 19999}); !errors.Is(err, impl.ErrPortRange) {
		t.Fatalf("low port: err = %v, want ErrPortRange", err)
	}
	if _, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 20011}); !errors.Is(err, impl.ErrPortRange) {
		t.Fatalf("high port: err = %v, want ErrPortRange", err)
	}
	if _, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 20000}); err != nil {
		t.Fatalf("boundary port must be accepted, got %v", err)
	}
}

func TestAllocateRemoteRejectsSecondHolder(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	mustRegister(t, p, "sess-2", "tenant-b")
	if _, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 20003}); err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := p.AllocateRemote("sess-2", impl.RemoteForward{WantPort: 20003}); !errors.Is(err, impl.ErrPortBusy) {
		t.Fatalf("second lease: err = %v, want ErrPortBusy", err)
	}
	s, err := p.SessionState("sess-2")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.RemotePort != 0 {
		t.Fatalf("rejected session holds port %d, want none", s.RemotePort)
	}
	pool := p.LeasePool()
	if len(pool) != 1 || pool[0].SessionID != "sess-1" {
		t.Fatalf("lease pool altered by rejected request: %+v", pool)
	}
}

func TestReleaseReclaimsRemotePort(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	mustRegister(t, p, "sess-2", "tenant-b")
	if _, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 20007}); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := p.Release("sess-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := p.SessionState("sess-1"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("released session still readable, err = %v", err)
	}
	if got := p.LeasePool(); len(got) != 0 {
		t.Fatalf("lease pool after release = %+v, want empty", got)
	}
	if _, err := p.AllocateRemote("sess-2", impl.RemoteForward{WantPort: 20007}); err != nil {
		t.Fatalf("released port must be reusable, got %v", err)
	}
}

func TestReleaseUnknownSessionIsIdempotent(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	if err := p.Release("ghost"); err != nil {
		t.Fatalf("release unknown session must succeed, got %v", err)
	}
	if _, err := p.SessionState("ghost"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("ghost session state err = %v, want ErrSessionNotFound", err)
	}
	if got := p.SessionCount("tenant-a"); got != 1 {
		t.Fatalf("tenant session count = %d, want 1", got)
	}
}

func TestChargeBytesEnforcesQuota(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	if err := p.ChargeBytes("sess-1", 4000); err != nil {
		t.Fatalf("charge under quota: %v", err)
	}
	s, err := p.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.Bytes != 4000 {
		t.Fatalf("bytes = %d, want 4000", s.Bytes)
	}
	if err := p.ChargeBytes("sess-1", 200); !errors.Is(err, impl.ErrByteQuota) {
		t.Fatalf("over quota: err = %v, want ErrByteQuota", err)
	}
	s, err = p.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.Bytes != 4000 {
		t.Fatalf("rejected charge was accounted, bytes = %d", s.Bytes)
	}
}

func TestSnapshotRestoreKeepsLeasesAndTenantCount(t *testing.T) {
	p := impl.NewSshForwardPolicy(policyConfig())
	mustRegister(t, p, "sess-1", "tenant-a")
	if _, err := p.AllocateRemote("sess-1", impl.RemoteForward{WantPort: 20002}); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := p.ChargeBytes("sess-1", 128); err != nil {
		t.Fatalf("charge: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewSshForwardPolicy(policyConfig())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	s, err := restored.SessionState("sess-1")
	if err != nil {
		t.Fatalf("session after restore: %v", err)
	}
	if s.RemotePort != 20002 || s.Bytes != 128 {
		t.Fatalf("session after restore = %+v", s)
	}
	if got := restored.SessionCount("tenant-a"); got != 1 {
		t.Fatalf("tenant count after restore = %d, want 1", got)
	}
	if _, err := restored.RegisterSession("sess-9", "tenant-b", 1); err != nil {
		t.Fatalf("register after restore: %v", err)
	}
	if _, err := restored.AllocateRemote("sess-9", impl.RemoteForward{WantPort: 20002}); !errors.Is(err, impl.ErrPortBusy) {
		t.Fatalf("restored lease must keep port busy, err = %v", err)
	}
}
