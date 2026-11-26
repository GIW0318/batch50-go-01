package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-12"
)

func brokerLimits() impl.Limits {
	return impl.Limits{
		MaxRecords: 64,
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func regularQuota() impl.Quota {
	return impl.Quota{MaxInFlight: 8, MaxPerKind: 8, Allowed: []string{impl.KindRegular, impl.KindDirectory}}
}

func newPeer(t *testing.T, b *impl.ScmRightsBroker, id, tenant string, uid uint32) impl.Peer {
	t.Helper()
	p, err := b.RegisterPeer(id, tenant, uid)
	if err != nil {
		t.Fatalf("register peer %q must succeed, got %v", id, err)
	}
	return p
}

func regularDesc(target string) impl.Descriptor {
	return impl.Descriptor{Kind: impl.KindRegular, Target: target, Size: 1024, ObjectKey: "obj:" + target}
}

func TestAcceptConnRejectsMissingConnection(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if _, err := b.AcceptConn(nil); !errors.Is(err, impl.ErrBadConn) {
		t.Fatalf("accept nil connection = %v, want ErrBadConn", err)
	}
}

func TestRegisterPeerRejectsBlankTenant(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if _, err := b.RegisterPeer("peer-x", "", 1000); !errors.Is(err, impl.ErrBlankPeer) {
		t.Fatalf("register peer with blank tenant = %v, want ErrBlankPeer", err)
	}
}

func TestRegisterPeerRejectsDuplicateID(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	newPeer(t, b, "peer-1", "tenant-a", 1000)
	if _, err := b.RegisterPeer("peer-1", "tenant-b", 1001); !errors.Is(err, impl.ErrPeerExists) {
		t.Fatalf("duplicate peer id = %v, want ErrPeerExists", err)
	}
}

func TestReceiveReturnsGrantsInArrivalOrder(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.SetQuota("tenant-a", regularQuota()); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	staged := []impl.Descriptor{
		regularDesc("/srv/data/a.bin"),
		{Kind: impl.KindDirectory, Target: "/srv/data", ObjectKey: "obj:/srv/data"},
		regularDesc("/srv/data/b.bin"),
	}
	if err := b.Stage(p, staged); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	grants, err := b.Receive(p, 3)
	if err != nil {
		t.Fatalf("receive must succeed, got %v", err)
	}
	if len(grants) != 3 {
		t.Fatalf("grants = %d, want 3", len(grants))
	}
	wantKinds := []string{impl.KindRegular, impl.KindDirectory, impl.KindRegular}
	for i, g := range grants {
		if g.Kind != wantKinds[i] {
			t.Fatalf("grant %d kind = %q, want %q", i, g.Kind, wantKinds[i])
		}
		if g.Target != staged[i].Target {
			t.Fatalf("grant %d target = %q, want %q", i, g.Target, staged[i].Target)
		}
		if g.Tenant != "tenant-a" || g.PeerID != "peer-1" {
			t.Fatalf("grant %d owner = %q/%q, want tenant-a/peer-1", i, g.Tenant, g.PeerID)
		}
		if g.ID == "" {
			t.Fatalf("grant %d has an empty id", i)
		}
	}
	if grants[0].Seq >= grants[1].Seq || grants[1].Seq >= grants[2].Seq {
		t.Fatalf("grant sequence is not ascending: %d %d %d", grants[0].Seq, grants[1].Seq, grants[2].Seq)
	}
}

func TestReceiveRejectsDisallowedKind(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.SetQuota("tenant-a", regularQuota()); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	if err := b.Stage(p, []impl.Descriptor{{Kind: impl.KindMemfd, Target: "memfd:secret", Size: 4096, ObjectKey: "obj:memfd:secret"}}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 4); !errors.Is(err, impl.ErrFDTypeDenied) {
		t.Fatalf("memfd admission = %v, want ErrFDTypeDenied", err)
	}
	if stats := b.Stats("tenant-a"); stats.InFlight != 0 || stats.Granted != 0 {
		t.Fatalf("stats after denial = %+v, want nothing granted", stats)
	}
}

func TestReceiveEnforcesMaxFDsPerCall(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.SetQuota("tenant-a", regularQuota()); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	staged := []impl.Descriptor{
		regularDesc("/srv/a"), regularDesc("/srv/b"), regularDesc("/srv/c"), regularDesc("/srv/d"),
	}
	if err := b.Stage(p, staged); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 2); !errors.Is(err, impl.ErrQuotaExceeded) {
		t.Fatalf("oversized message = %v, want ErrQuotaExceeded", err)
	}
	grants, err := b.Receive(p, 4)
	if err != nil {
		t.Fatalf("full message must be accepted after the refusal, got %v", err)
	}
	if len(grants) != 4 {
		t.Fatalf("grants = %d, want 4", len(grants))
	}
}

func TestReceiveEnforcesTenantInFlightLimit(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	q := regularQuota()
	q.MaxInFlight = 2
	if err := b.SetQuota("tenant-a", q); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	if err := b.Stage(p, []impl.Descriptor{regularDesc("/srv/a"), regularDesc("/srv/b")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	grants, err := b.Receive(p, 2)
	if err != nil {
		t.Fatalf("receive up to the limit must succeed, got %v", err)
	}
	if err := b.Stage(p, []impl.Descriptor{regularDesc("/srv/c")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 1); !errors.Is(err, impl.ErrQuotaExceeded) {
		t.Fatalf("receive above the in flight limit = %v, want ErrQuotaExceeded", err)
	}
	if err := b.Release(grants[0].ID); err != nil {
		t.Fatalf("release must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 1); err != nil {
		t.Fatalf("receive after a release must succeed, got %v", err)
	}
}

func TestReceiveEnforcesPerKindLimit(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	q := regularQuota()
	q.MaxPerKind = 1
	if err := b.SetQuota("tenant-a", q); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	if err := b.Stage(p, []impl.Descriptor{regularDesc("/srv/a"), regularDesc("/srv/b")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 2); !errors.Is(err, impl.ErrQuotaExceeded) {
		t.Fatalf("two descriptors of one kind = %v, want ErrQuotaExceeded", err)
	}
	mixed := []impl.Descriptor{
		regularDesc("/srv/a"),
		{Kind: impl.KindDirectory, Target: "/srv", ObjectKey: "obj:/srv"},
	}
	if err := b.Stage(p, mixed); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 2); err != nil {
		t.Fatalf("mixed kinds within the per kind limit must succeed, got %v", err)
	}
}

func TestReleaseRejectsUnknownGrant(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.Release("grant-does-not-exist"); !errors.Is(err, impl.ErrUnknownGrant) {
		t.Fatalf("release unknown grant = %v, want ErrUnknownGrant", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.SetQuota("tenant-a", regularQuota()); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	if err := b.Stage(p, []impl.Descriptor{regularDesc("/srv/a")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	grants, err := b.Receive(p, 1)
	if err != nil {
		t.Fatalf("receive must succeed, got %v", err)
	}
	if err := b.Release(grants[0].ID); err != nil {
		t.Fatalf("first release must succeed, got %v", err)
	}
	if err := b.Release(grants[0].ID); err != nil {
		t.Fatalf("second release must be a no-op success, got %v", err)
	}
	stats := b.Stats("tenant-a")
	if stats.InFlight != 0 {
		t.Fatalf("in flight = %d, want 0", stats.InFlight)
	}
	if stats.Released != 1 {
		t.Fatalf("released = %d, want 1 closed grant", stats.Released)
	}
}

func TestAuditSequenceIsMonotonic(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.SetQuota("tenant-a", regularQuota()); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	if err := b.Stage(p, []impl.Descriptor{regularDesc("/srv/a"), regularDesc("/srv/b")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	grants, err := b.Receive(p, 2)
	if err != nil {
		t.Fatalf("receive must succeed, got %v", err)
	}
	if err := b.Release(grants[1].ID); err != nil {
		t.Fatalf("release must succeed, got %v", err)
	}
	log := b.Audit()
	if len(log) < 3 {
		t.Fatalf("audit records = %d, want at least 3", len(log))
	}
	for i := 1; i < len(log); i++ {
		if log[i].Seq <= log[i-1].Seq {
			t.Fatalf("audit sequence not monotonic at %d: %d then %d", i, log[i-1].Seq, log[i].Seq)
		}
	}
	if log[0].Event != impl.EventGranted {
		t.Fatalf("first audit event = %q, want %q", log[0].Event, impl.EventGranted)
	}
	if log[len(log)-1].Event != impl.EventReleased {
		t.Fatalf("last audit event = %q, want %q", log[len(log)-1].Event, impl.EventReleased)
	}
}

func TestSnapshotRestoreReclaimsOrphans(t *testing.T) {
	limits := brokerLimits()
	b := impl.NewScmRightsBroker(limits)
	if err := b.SetQuota("tenant-a", regularQuota()); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	if err := b.Stage(p, []impl.Descriptor{regularDesc("/srv/a"), regularDesc("/srv/b")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 2); err != nil {
		t.Fatalf("receive must succeed, got %v", err)
	}
	data, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewScmRightsBroker(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	if stats := restored.Stats("tenant-a"); stats.InFlight != 0 {
		t.Fatalf("in flight after restore = %d, want 0", stats.InFlight)
	}
	orphans := 0
	for _, rec := range restored.Audit() {
		if rec.Event == impl.EventOrphanReclaimed {
			orphans++
		}
	}
	if orphans != 2 {
		t.Fatalf("orphan records = %d, want 2", orphans)
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	if err := b.Restore([]byte("not a snapshot at all")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("restore garbage = %v, want ErrBadSnapshot", err)
	}
}

func TestAuditTrimsOldestRecords(t *testing.T) {
	limits := brokerLimits()
	limits.MaxRecords = 4
	b := impl.NewScmRightsBroker(limits)
	q := regularQuota()
	q.MaxInFlight = 8
	if err := b.SetQuota("tenant-a", q); err != nil {
		t.Fatalf("set quota must succeed, got %v", err)
	}
	p := newPeer(t, b, "peer-1", "tenant-a", 1000)
	staged := []impl.Descriptor{
		regularDesc("/srv/1"), regularDesc("/srv/2"), regularDesc("/srv/3"),
		regularDesc("/srv/4"), regularDesc("/srv/5"), regularDesc("/srv/6"),
	}
	if err := b.Stage(p, staged); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(p, 6); err != nil {
		t.Fatalf("receive must succeed, got %v", err)
	}
	log := b.Audit()
	if len(log) != 4 {
		t.Fatalf("audit records = %d, want 4", len(log))
	}
	if log[0].Seq != 3 || log[3].Seq != 6 {
		t.Fatalf("kept sequence window = %d..%d, want 3..6", log[0].Seq, log[3].Seq)
	}
}

func TestQuotaIsScopedPerTenant(t *testing.T) {
	b := impl.NewScmRightsBroker(brokerLimits())
	q := regularQuota()
	q.MaxInFlight = 2
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := b.SetQuota(tenant, q); err != nil {
			t.Fatalf("set quota for %s must succeed, got %v", tenant, err)
		}
	}
	pa := newPeer(t, b, "peer-a", "tenant-a", 1000)
	pb := newPeer(t, b, "peer-b", "tenant-b", 1001)
	if err := b.Stage(pa, []impl.Descriptor{regularDesc("/srv/a1"), regularDesc("/srv/a2")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(pa, 2); err != nil {
		t.Fatalf("tenant-a receive must succeed, got %v", err)
	}
	if err := b.Stage(pb, []impl.Descriptor{regularDesc("/srv/b1"), regularDesc("/srv/b2")}); err != nil {
		t.Fatalf("stage must succeed, got %v", err)
	}
	if _, err := b.Receive(pb, 2); err != nil {
		t.Fatalf("tenant-b receive must succeed despite tenant-a usage, got %v", err)
	}
	if stats := b.Stats("tenant-b"); stats.InFlight != 2 {
		t.Fatalf("tenant-b in flight = %d, want 2", stats.InFlight)
	}
}
