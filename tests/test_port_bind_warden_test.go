package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-20"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Unix(1700000000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func wardenConfig(clock *fakeClock) impl.Config {
	return impl.Config{
		PrivilegedOwners: []string{"root-svc"},
		MaxLeases:        8,
		MaxPerOwner:      4,
		MaxGroupMembers:  2,
		Now:              clock.now,
	}
}

func mustReserve(t *testing.T, w *impl.PortBindWarden, port int, owner string, ttl time.Duration) impl.Lease {
	t.Helper()
	lease, err := w.Reserve(port, owner, ttl)
	if err != nil {
		t.Fatalf("reserve %d for %s: %v", port, owner, err)
	}
	return lease
}

func mustBind(t *testing.T, w *impl.PortBindWarden, leaseID string) impl.Binding {
	t.Helper()
	bind, err := w.Bind(leaseID, impl.BindOpts{Address: "0.0.0.0"})
	if err != nil {
		t.Fatalf("bind lease %s: %v", leaseID, err)
	}
	return bind
}

func TestReserveCreatesDynamicLease(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	lease := mustReserve(t, w, 40000, "svc-a", time.Minute)
	if lease.ID != "lease-1" {
		t.Fatalf("lease id = %q, want lease-1", lease.ID)
	}
	if lease.Port != 40000 || lease.Owner != "svc-a" {
		t.Fatalf("lease record = %+v, want port 40000 owned by svc-a", lease)
	}
	if lease.Seq != 1 {
		t.Fatalf("lease seq = %d, want 1", lease.Seq)
	}
	if want := clock.now().Add(time.Minute).UnixNano(); lease.ExpiresAt != want {
		t.Fatalf("lease expiry = %d, want %d", lease.ExpiresAt, want)
	}
	view, err := w.Explain(40000)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if view.Class != impl.ClassDynamic {
		t.Fatalf("class = %q, want dynamic", view.Class)
	}
	if view.Lease == nil || view.Lease.ID != lease.ID {
		t.Fatalf("explained lease = %+v, want lease-1", view.Lease)
	}
	if len(view.Bindings) != 0 || view.Group != "" {
		t.Fatalf("fresh port state = %+v, want no bindings and no group", view)
	}
}

func TestReserveRejectsInvalidPortAndTTL(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	for _, port := range []int{0, -1, 70000} {
		if _, err := w.Reserve(port, "svc-a", time.Minute); !errors.Is(err, impl.ErrBadPort) {
			t.Fatalf("reserve port %d = %v, want ErrBadPort", port, err)
		}
	}
	if _, err := w.Reserve(40000, "   ", time.Minute); !errors.Is(err, impl.ErrBadOwner) {
		t.Fatalf("blank owner = %v, want ErrBadOwner", err)
	}
	for _, ttl := range []time.Duration{0, -time.Minute} {
		if _, err := w.Reserve(40000, "svc-a", ttl); !errors.Is(err, impl.ErrBadTTL) {
			t.Fatalf("ttl %v = %v, want ErrBadTTL", ttl, err)
		}
	}
	if _, err := w.Explain(70000); !errors.Is(err, impl.ErrBadPort) {
		t.Fatalf("explain of an invalid port = %v, want ErrBadPort", err)
	}
}

func TestReservePrivilegedRequiresPermission(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	if _, err := w.Reserve(80, "svc-a", time.Minute); !errors.Is(err, impl.ErrPortNotAllowed) {
		t.Fatalf("privileged reserve by an ordinary owner = %v, want ErrPortNotAllowed", err)
	}
	lease := mustReserve(t, w, 80, "root-svc", time.Minute)
	if lease.Port != 80 {
		t.Fatalf("privileged lease = %+v, want port 80", lease)
	}
	view, err := w.Explain(80)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if view.Class != impl.ClassPrivileged {
		t.Fatalf("class = %q, want privileged", view.Class)
	}
	reserved, err := w.Explain(20000)
	if err != nil {
		t.Fatalf("explain reserved port: %v", err)
	}
	if reserved.Class != impl.ClassReserved || reserved.Lease != nil {
		t.Fatalf("reserved port state = %+v, want the reserved class and no lease", reserved)
	}
}

func TestReserveSamePortOwnerAndConflict(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	first := mustReserve(t, w, 40001, "svc-a", time.Minute)
	if _, err := w.Reserve(40001, "svc-b", time.Minute); !errors.Is(err, impl.ErrPortInUse) {
		t.Fatalf("second owner on the same port = %v, want ErrPortInUse", err)
	}
	clock.advance(30 * time.Second)
	renewed, err := w.Reserve(40001, "svc-a", 2*time.Minute)
	if err != nil {
		t.Fatalf("owner renewal: %v", err)
	}
	if renewed.ID != first.ID {
		t.Fatalf("renewal changed the lease id to %q, want %q", renewed.ID, first.ID)
	}
	if want := clock.now().Add(2 * time.Minute).UnixNano(); renewed.ExpiresAt != want {
		t.Fatalf("renewed expiry = %d, want %d", renewed.ExpiresAt, want)
	}
	view, _ := w.Explain(40001)
	if view.Lease == nil || view.Lease.Owner != "svc-a" {
		t.Fatalf("port state after renewal = %+v, want the svc-a lease", view)
	}
}

func TestReserveOwnerLimit(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(impl.Config{PrivilegedOwners: []string{"root-svc"}, MaxLeases: 8, MaxPerOwner: 2, MaxGroupMembers: 2, Now: clock.now})
	mustReserve(t, w, 40010, "svc-a", time.Minute)
	mustReserve(t, w, 40011, "svc-a", time.Minute)
	if _, err := w.Reserve(40012, "svc-a", time.Minute); !errors.Is(err, impl.ErrOwnerLimit) {
		t.Fatalf("third lease for one owner = %v, want ErrOwnerLimit", err)
	}
	if _, err := w.Reserve(40012, "svc-b", time.Minute); err != nil {
		t.Fatalf("another owner must still be able to reserve, got %v", err)
	}
	view, _ := w.Explain(40012)
	if view.Lease == nil || view.Lease.Owner != "svc-b" {
		t.Fatalf("port 40012 state = %+v, want the svc-b lease", view)
	}
}

func TestReserveLeaseLimit(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(impl.Config{PrivilegedOwners: []string{"root-svc"}, MaxLeases: 2, MaxPerOwner: 4, MaxGroupMembers: 2, Now: clock.now})
	mustReserve(t, w, 40020, "svc-a", time.Minute)
	mustReserve(t, w, 40021, "svc-b", time.Minute)
	if _, err := w.Reserve(40022, "svc-c", time.Minute); !errors.Is(err, impl.ErrLeaseLimit) {
		t.Fatalf("third lease in the pool = %v, want ErrLeaseLimit", err)
	}
	if _, ok := w.Lease("lease-3"); ok {
		t.Fatalf("rejected lease must not be recorded")
	}
}

func TestBindRejectsExpiredLease(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	lease := mustReserve(t, w, 40030, "svc-a", time.Minute)
	if _, err := w.Bind("lease-404", impl.BindOpts{Address: "0.0.0.0"}); !errors.Is(err, impl.ErrLeaseNotFound) {
		t.Fatalf("bind on an unknown lease = %v, want ErrLeaseNotFound", err)
	}
	if _, err := w.Bind(lease.ID, impl.BindOpts{Address: "  "}); !errors.Is(err, impl.ErrBadBinding) {
		t.Fatalf("bind without an address = %v, want ErrBadBinding", err)
	}
	clock.advance(2 * time.Minute)
	if _, err := w.Bind(lease.ID, impl.BindOpts{Address: "0.0.0.0"}); !errors.Is(err, impl.ErrLeaseExpired) {
		t.Fatalf("bind on an expired lease = %v, want ErrLeaseExpired", err)
	}
	view, _ := w.Explain(40030)
	if len(view.Bindings) != 0 {
		t.Fatalf("rejected bind left %d bindings behind", len(view.Bindings))
	}
}

func TestBindChecksPrivilegeClaim(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	lease := mustReserve(t, w, 40040, "svc-a", time.Minute)
	if _, err := w.Bind(lease.ID, impl.BindOpts{Address: "0.0.0.0", Privileged: true}); !errors.Is(err, impl.ErrPortNotAllowed) {
		t.Fatalf("privileged claim by an ordinary owner = %v, want ErrPortNotAllowed", err)
	}
	bind := mustBind(t, w, lease.ID)
	if bind.ID != "bind-1" || bind.Port != 40040 || bind.Owner != "svc-a" || bind.LeaseID != lease.ID {
		t.Fatalf("binding record = %+v, want bind-1 on port 40040", bind)
	}
	if bind.Address != "0.0.0.0" || bind.Group != "" {
		t.Fatalf("binding record = %+v, want the requested address and no group", bind)
	}
	view, _ := w.Explain(40040)
	if len(view.Bindings) != 1 || view.Bindings[0].ID != bind.ID {
		t.Fatalf("port bindings = %+v, want only bind-1", view.Bindings)
	}
}

func TestCheckpointReclaimsIdleExpiredLeases(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	idle := mustReserve(t, w, 40050, "svc-a", time.Minute)
	live := mustReserve(t, w, 40051, "svc-a", time.Hour)
	clock.advance(2 * time.Minute)
	report, err := w.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(report.Reclaimed) != 1 || report.Reclaimed[0] != idle.ID {
		t.Fatalf("reclaimed = %v, want [%s]", report.Reclaimed, idle.ID)
	}
	if len(report.Graced) != 0 {
		t.Fatalf("graced = %v, want none", report.Graced)
	}
	view, _ := w.Explain(40050)
	if view.Lease != nil {
		t.Fatalf("reclaimed port still reports a lease: %+v", view.Lease)
	}
	if _, ok := w.Lease(idle.ID); ok {
		t.Fatalf("reclaimed lease is still readable")
	}
	if _, ok := w.Lease(live.ID); !ok {
		t.Fatalf("unexpired lease %s was reclaimed", live.ID)
	}
	next := mustReserve(t, w, 40050, "svc-b", time.Minute)
	if next.ID == idle.ID {
		t.Fatalf("lease id was reused after a reclaim: %q", next.ID)
	}
}

func TestCheckpointKeepsUnexpiredLeases(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	lease := mustReserve(t, w, 40060, "svc-a", time.Hour)
	bind := mustBind(t, w, lease.ID)
	clock.advance(time.Minute)
	report, err := w.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(report.Reclaimed) != 0 || len(report.Graced) != 0 {
		t.Fatalf("checkpoint touched unexpired leases: %+v", report)
	}
	if _, ok := w.Lease(lease.ID); !ok {
		t.Fatalf("unexpired lease disappeared")
	}
	if _, ok := w.Binding(bind.ID); !ok {
		t.Fatalf("binding disappeared")
	}
	view, _ := w.Explain(40060)
	if view.Lease == nil || len(view.Bindings) != 1 {
		t.Fatalf("port state after the checkpoint = %+v, want the lease and its binding", view)
	}
}

func TestJoinGroupAtomicAndLimit(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(impl.Config{PrivilegedOwners: []string{"root-svc"}, MaxLeases: 8, MaxPerOwner: 4, MaxGroupMembers: 2, Now: clock.now})
	first := mustBind(t, w, mustReserve(t, w, 40070, "svc-a", time.Minute).ID)
	second := mustBind(t, w, mustReserve(t, w, 40071, "svc-a", time.Minute).ID)
	if err := w.JoinGroup("  ", first.ID); !errors.Is(err, impl.ErrBadGroup) {
		t.Fatalf("blank group name = %v, want ErrBadGroup", err)
	}
	if err := w.JoinGroup("edge", "bind-404"); !errors.Is(err, impl.ErrUnknownBinding) {
		t.Fatalf("unknown binding = %v, want ErrUnknownBinding", err)
	}
	if err := w.JoinGroup("edge", first.ID); err != nil {
		t.Fatalf("first member: %v", err)
	}
	if err := w.JoinGroup("edge", first.ID); err != nil {
		t.Fatalf("repeated join must be idempotent, got %v", err)
	}
	if err := w.JoinGroup("edge", second.ID); err != nil {
		t.Fatalf("second member: %v", err)
	}
	third := mustBind(t, w, mustReserve(t, w, 40072, "svc-a", time.Minute).ID)
	if err := w.JoinGroup("edge", third.ID); !errors.Is(err, impl.ErrGroupLimit) {
		t.Fatalf("third member = %v, want ErrGroupLimit", err)
	}
	view, _ := w.Explain(40070)
	if view.Group != "edge" {
		t.Fatalf("port group = %q, want edge", view.Group)
	}
	if len(view.Bindings) != 1 || view.Bindings[0].Group != "edge" {
		t.Fatalf("binding group = %+v, want edge", view.Bindings)
	}
	rejected, _ := w.Explain(40072)
	if rejected.Group != "" {
		t.Fatalf("binding outside the group reports group %q", rejected.Group)
	}
}

func TestReleaseRemovesBindingAndGroup(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	bind := mustBind(t, w, mustReserve(t, w, 40080, "svc-a", time.Minute).ID)
	if err := w.JoinGroup("edge", bind.ID); err != nil {
		t.Fatalf("join group: %v", err)
	}
	if err := w.Release(bind.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok := w.Binding(bind.ID); ok {
		t.Fatalf("released binding is still readable")
	}
	view, err := w.Explain(40080)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(view.Bindings) != 0 || view.Group != "" {
		t.Fatalf("port state after release = %+v, want no bindings and no group", view)
	}
	if view.Lease == nil {
		t.Fatalf("release must not drop the lease")
	}
	if err := w.Release(bind.ID); !errors.Is(err, impl.ErrUnknownBinding) {
		t.Fatalf("double release = %v, want ErrUnknownBinding", err)
	}
}

func TestConcurrentReserveSingleWinner(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := []string{}
	conflicts := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "svc-" + string(rune('a'+i))
			lease, err := w.Reserve(40090, owner, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, lease.Owner)
			case errors.Is(err, impl.ErrPortInUse):
				conflicts++
			default:
				t.Errorf("unexpected reserve error for %s: %v", owner, err)
			}
		}(i)
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("concurrent reserve produced %d winners %v, want exactly 1", len(winners), winners)
	}
	if conflicts != 15 {
		t.Fatalf("concurrent reserve produced %d conflicts, want 15", conflicts)
	}
	view, _ := w.Explain(40090)
	if view.Lease == nil || view.Lease.Owner != winners[0] {
		t.Fatalf("port state after the race = %+v, want the winning lease %v", view, winners)
	}
}

func TestSnapshotRestoreKeepsLeasesBindingsAndGroups(t *testing.T) {
	clock := newClock()
	cfg := wardenConfig(clock)
	w := impl.NewPortBindWarden(cfg)
	lease := mustReserve(t, w, 40100, "svc-a", time.Hour)
	bind := mustBind(t, w, lease.ID)
	if err := w.JoinGroup("edge", bind.ID); err != nil {
		t.Fatalf("join group: %v", err)
	}
	data, err := w.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewPortBindWarden(cfg)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	view, err := r.Explain(40100)
	if err != nil {
		t.Fatalf("explain after restore: %v", err)
	}
	if view.Lease == nil || view.Lease.ID != lease.ID || view.Lease.Owner != "svc-a" {
		t.Fatalf("restored lease = %+v, want %s", view.Lease, lease.ID)
	}
	if len(view.Bindings) != 1 || view.Bindings[0].ID != bind.ID {
		t.Fatalf("restored bindings = %+v, want %s", view.Bindings, bind.ID)
	}
	if view.Group != "edge" {
		t.Fatalf("restored group = %q, want edge", view.Group)
	}
	if got, ok := r.Binding(bind.ID); !ok || got.Group != "edge" {
		t.Fatalf("binding lookup after restore = %+v ok=%v", got, ok)
	}
	next := mustReserve(t, r, 40101, "svc-a", time.Minute)
	if next.ID == lease.ID {
		t.Fatalf("lease id was reused after restore: %q", next.ID)
	}
	if next.Seq <= lease.Seq {
		t.Fatalf("lease seq after restore = %d, must move past %d", next.Seq, lease.Seq)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	clock := newClock()
	w := impl.NewPortBindWarden(wardenConfig(clock))
	lease := mustReserve(t, w, 40110, "svc-a", time.Minute)
	bind := mustBind(t, w, lease.ID)
	if err := w.Restore([]byte("\x00\x01 not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot = %v, want ErrBadSnapshot", err)
	}
	view, err := w.Explain(40110)
	if err != nil {
		t.Fatalf("explain after the rejected restore: %v", err)
	}
	if view.Lease == nil || view.Lease.ID != lease.ID {
		t.Fatalf("failed restore changed the lease table: %+v", view.Lease)
	}
	if len(view.Bindings) != 1 || view.Bindings[0].ID != bind.ID {
		t.Fatalf("failed restore changed the bindings: %+v", view.Bindings)
	}
	report, err := w.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint after the rejected restore: %v", err)
	}
	if len(report.Reclaimed) != 0 {
		t.Fatalf("checkpoint reclaimed %v from the live state", report.Reclaimed)
	}
}
