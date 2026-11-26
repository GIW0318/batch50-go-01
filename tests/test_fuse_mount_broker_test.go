package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-d-04"
)

func brokerLimits() impl.Limits {
	return impl.Limits{MaxMounts: 4, MaxQueueDepth: 2, MaxMapping: 2}
}

func addMount(t *testing.T, b *impl.FuseMountBroker, point string, owner int, allowOther bool) impl.Mount {
	t.Helper()
	m, err := b.Mount(impl.MountSpec{Mountpoint: point, OwnerUID: owner, AllowOther: allowOther, Perm: "rw", Backend: "backend-a"})
	if err != nil {
		t.Fatalf("mount %s: %v", point, err)
	}
	return m
}

func ownerRequest(id string, uid int) impl.Request {
	return impl.Request{ID: id, CallerUID: uid, Path: "/payload.bin", Op: "read", Idempotent: true}
}

func TestMountRegistersAndLists(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/data", OwnerUID: 1000, Perm: "rw", Backend: "srv-backend"})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if m.ID == "" || m.Mountpoint != "/srv/data" {
		t.Fatalf("mount id=%q point=%q, want a named mount", m.ID, m.Mountpoint)
	}
	if m.State != impl.MountActive || m.Gen <= 0 || m.Queued != 0 {
		t.Fatalf("mount state=%q gen=%d queued=%d, want an active fresh mount", m.State, m.Gen, m.Queued)
	}
	list := b.Mounts()
	if len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("mounts = %+v, want exactly %q", list, m.ID)
	}
	depth, err := b.Depth(m.ID)
	if err != nil || depth != 0 {
		t.Fatalf("depth = %d err=%v, want 0", depth, err)
	}
	st, err := b.Stats(m.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Requests != 0 || st.Denied != 0 || st.Queued != 0 || st.Dropped != 0 || st.Gen <= 0 {
		t.Fatalf("fresh stats = %+v, want zero counters and a positive gen", st)
	}
}

func TestMountRejectsBadMountpoint(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	for _, point := range []string{"tmp/x", "", "/", "/a/../b", "/srv//data"} {
		if _, err := b.Mount(impl.MountSpec{Mountpoint: point, OwnerUID: 0, Perm: "rw"}); !errors.Is(err, impl.ErrBadMountpoint) {
			t.Fatalf("mountpoint %q: got %v, want ErrBadMountpoint", point, err)
		}
	}
	if got := len(b.Mounts()); got != 0 {
		t.Fatalf("rejected mounts left %d entries behind", got)
	}
}

func TestMountRejectsBadPermAndOwner(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/a", OwnerUID: 1000, Perm: "rx"}); !errors.Is(err, impl.ErrBadPerm) {
		t.Fatalf("an unknown permission must fail with ErrBadPerm, got %v", err)
	}
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/a", OwnerUID: -1, Perm: "rw"}); !errors.Is(err, impl.ErrBadOwner) {
		t.Fatalf("a negative owner must fail with ErrBadOwner, got %v", err)
	}
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/a", OwnerUID: 1000, Perm: "rw", DowngradeTo: -1}); !errors.Is(err, impl.ErrBadOwner) {
		t.Fatalf("a negative downgrade identity must fail with ErrBadOwner, got %v", err)
	}
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/a", OwnerUID: 1000, Perm: "ro"}); err != nil {
		t.Fatalf("a read only mount must be accepted, got %v", err)
	}
}

func TestMountRejectsDuplicateMountpoint(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/a", 1000, false)
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/a", OwnerUID: 1000, Perm: "rw"}); !errors.Is(err, impl.ErrMountExists) {
		t.Fatalf("a duplicate mountpoint must fail with ErrMountExists, got %v", err)
	}
	if err := b.Unmount(m.ID); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/a", OwnerUID: 1000, Perm: "rw"}); err != nil {
		t.Fatalf("the mountpoint must be reusable after unmount, got %v", err)
	}
}

func TestMountEnforcesLimit(t *testing.T) {
	limits := brokerLimits()
	limits.MaxMounts = 2
	b := impl.NewFuseMountBroker(limits)
	first := addMount(t, b, "/srv/a", 1000, false)
	addMount(t, b, "/srv/b", 1001, false)
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/c", OwnerUID: 1002, Perm: "rw"}); !errors.Is(err, impl.ErrMountLimit) {
		t.Fatalf("the third mount must hit the cap, got %v", err)
	}
	if got := len(b.Mounts()); got != 2 {
		t.Fatalf("mounts = %d, want the cap 2", got)
	}
	if err := b.Unmount(first.ID); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	if _, err := b.Mount(impl.MountSpec{Mountpoint: "/srv/c", OwnerUID: 1002, Perm: "rw"}); err != nil {
		t.Fatalf("the freed slot must accept a new mount, got %v", err)
	}
}

func TestForwardOwnerAllowed(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	if _, err := b.Forward("mnt-404", ownerRequest("r0", 1000)); !errors.Is(err, impl.ErrUnknownMount) {
		t.Fatalf("forwarding to an unknown mount must fail with ErrUnknownMount, got %v", err)
	}
	if _, err := b.Forward(m.ID, impl.Request{CallerUID: 1000, Path: "/f"}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("a request without an id must fail with ErrBadRequest, got %v", err)
	}
	if _, err := b.Forward(m.ID, impl.Request{ID: "r2", CallerUID: 1000}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("a request without a path must fail with ErrBadRequest, got %v", err)
	}
	r, err := b.Forward(m.ID, ownerRequest("r3", 1000))
	if err != nil {
		t.Fatalf("forward the owner request: %v", err)
	}
	if r.Code != 200 || r.MappedUID != 0 || r.Queued || r.Retry {
		t.Fatalf("reply = %+v, want an immediate 200 for the owner mapped to container root", r)
	}
	st, err := b.Stats(m.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Requests != 1 || st.Denied != 0 || st.Queued != 0 {
		t.Fatalf("stats = %+v, want one served request", st)
	}
}

func TestForwardNonOwnerDeniedWithoutAllowOther(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	if _, err := b.Forward(m.ID, ownerRequest("r1", 1001)); !errors.Is(err, impl.ErrAccessDenied) {
		t.Fatalf("a non owner without cross user access must fail with ErrAccessDenied, got %v", err)
	}
	st, err := b.Stats(m.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Denied != 1 || st.Requests != 0 {
		t.Fatalf("stats = %+v, want one denial and no served request", st)
	}
}

func TestForwardAllowOtherRequiresMapping(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/shared", 1000, true)
	if err := b.SetMapping(m.ID, []impl.IDMap{{HostID: 1001, ContainerID: 0}}); err != nil {
		t.Fatalf("set mapping: %v", err)
	}
	r, err := b.Forward(m.ID, ownerRequest("r1", 1001))
	if err != nil {
		t.Fatalf("a mapped caller must be served: %v", err)
	}
	if r.Code != 200 || r.MappedUID != 0 {
		t.Fatalf("reply = %+v, want 200 with the mapped container identity 0", r)
	}
	if _, err := b.Forward(m.ID, ownerRequest("r2", 1002)); !errors.Is(err, impl.ErrAccessDenied) {
		t.Fatalf("an unmapped caller must fail with ErrAccessDenied, got %v", err)
	}
	st, err := b.Stats(m.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Requests != 1 || st.Denied != 1 {
		t.Fatalf("stats = %+v, want one served and one denied request", st)
	}
}

func TestSetMappingValidatesAndReplaces(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/shared", 1000, true)
	if err := b.SetMapping("mnt-404", []impl.IDMap{{HostID: 1, ContainerID: 1}}); !errors.Is(err, impl.ErrUnknownMount) {
		t.Fatalf("an unknown mount must fail with ErrUnknownMount, got %v", err)
	}
	for _, bad := range [][]impl.IDMap{
		{{HostID: 1, ContainerID: 0}, {HostID: 2, ContainerID: 1}, {HostID: 3, ContainerID: 2}},
		{{HostID: 1, ContainerID: 0}, {HostID: 1, ContainerID: 1}},
		{{HostID: 1, ContainerID: 0}, {HostID: 2, ContainerID: 0}},
		{{HostID: -1, ContainerID: 0}},
	} {
		if err := b.SetMapping(m.ID, bad); !errors.Is(err, impl.ErrBadMapping) {
			t.Fatalf("mapping %v: got %v, want ErrBadMapping", bad, err)
		}
	}
	if err := b.SetMapping(m.ID, []impl.IDMap{{HostID: 1001, ContainerID: 0}, {HostID: 1002, ContainerID: 1}}); err != nil {
		t.Fatalf("a valid mapping must be accepted, got %v", err)
	}
	for host, want := range map[int]int{1001: 0, 1002: 1} {
		r, err := b.Forward(m.ID, ownerRequest(fmt.Sprintf("r-%d", host), host))
		if err != nil {
			t.Fatalf("forward for host %d: %v", host, err)
		}
		if r.MappedUID != want {
			t.Fatalf("host %d mapped to %d, want %d", host, r.MappedUID, want)
		}
	}
	if err := b.SetMapping(m.ID, []impl.IDMap{{HostID: 1001, ContainerID: 5}}); err != nil {
		t.Fatalf("replace the mapping: %v", err)
	}
	r, err := b.Forward(m.ID, ownerRequest("r-again", 1001))
	if err != nil || r.MappedUID != 5 {
		t.Fatalf("forward after the replacement = %+v err=%v, want the new mapping 5", r, err)
	}
	if _, err := b.Forward(m.ID, ownerRequest("r-gone", 1002)); !errors.Is(err, impl.ErrAccessDenied) {
		t.Fatalf("a dropped mapping must deny the caller, got %v", err)
	}
	if err := b.SetMapping(m.ID, nil); err != nil {
		t.Fatalf("clearing the mapping must be accepted, got %v", err)
	}
	if _, err := b.Forward(m.ID, ownerRequest("r-empty", 1001)); !errors.Is(err, impl.ErrAccessDenied) {
		t.Fatalf("an empty mapping must deny every non owner caller, got %v", err)
	}
}

func TestQueueBackpressureReturnsRetryable(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	if err := b.SetBackend(m.ID, false); err != nil {
		t.Fatalf("take the backend offline: %v", err)
	}
	for i := 0; i < 2; i++ {
		r, err := b.Forward(m.ID, ownerRequest(fmt.Sprintf("q%d", i), 1000))
		if err != nil {
			t.Fatalf("queued forward %d: %v", i, err)
		}
		if !r.Queued || !r.Retry {
			t.Fatalf("queued forward %d reply = %+v, want queued and retryable", i, r)
		}
	}
	depth, err := b.Depth(m.ID)
	if err != nil || depth != 2 {
		t.Fatalf("depth = %d err=%v, want 2", depth, err)
	}
	r, err := b.Forward(m.ID, ownerRequest("q-overflow", 1000))
	if !errors.Is(err, impl.ErrQueueFull) {
		t.Fatalf("a forward past the queue cap must fail with ErrQueueFull, got %v", err)
	}
	if !r.Retry {
		t.Fatalf("the overflow reply = %+v, want it marked retryable", r)
	}
	depth, _ = b.Depth(m.ID)
	if depth != 2 {
		t.Fatalf("depth after the overflow = %d, want it capped at 2", depth)
	}
	st, _ := b.Stats(m.ID)
	if st.Requests != 2 || st.Queued != 2 || st.Dropped != 1 {
		t.Fatalf("stats = %+v, want 2 accepted, 2 queued and 1 dropped", st)
	}
}

func TestFlushDrainsQueue(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	if err := b.SetBackend(m.ID, false); err != nil {
		t.Fatalf("take the backend offline: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := b.Forward(m.ID, ownerRequest(fmt.Sprintf("q%d", i), 1000)); err != nil {
			t.Fatalf("queue forward %d: %v", i, err)
		}
	}
	if err := b.SetBackend(m.ID, true); err != nil {
		t.Fatalf("bring the backend online: %v", err)
	}
	n, err := b.Flush(m.ID)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 2 {
		t.Fatalf("flush drained %d requests, want 2", n)
	}
	depth, _ := b.Depth(m.ID)
	if depth != 0 {
		t.Fatalf("depth after flush = %d, want 0", depth)
	}
	st, _ := b.Stats(m.ID)
	if st.Queued != 0 || st.Requests != 2 {
		t.Fatalf("stats after flush = %+v, want an empty queue and two accepted requests", st)
	}
	if n, err := b.Flush(m.ID); err != nil || n != 0 {
		t.Fatalf("a second flush drained %d requests err=%v, want 0", n, err)
	}
}

func TestFlushRefusedWhileBackendDown(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	if err := b.SetBackend(m.ID, false); err != nil {
		t.Fatalf("take the backend offline: %v", err)
	}
	if _, err := b.Forward(m.ID, ownerRequest("q1", 1000)); err != nil {
		t.Fatalf("queue one request: %v", err)
	}
	if _, err := b.Flush(m.ID); !errors.Is(err, impl.ErrBackendDown) {
		t.Fatalf("flushing a disconnected backend must fail with ErrBackendDown, got %v", err)
	}
	if depth, _ := b.Depth(m.ID); depth != 1 {
		t.Fatalf("depth after the refused flush = %d, want the request kept", depth)
	}
}

func TestUnmountRequiresDrain(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	if err := b.SetBackend(m.ID, false); err != nil {
		t.Fatalf("take the backend offline: %v", err)
	}
	if _, err := b.Forward(m.ID, ownerRequest("q1", 1000)); err != nil {
		t.Fatalf("queue one request: %v", err)
	}
	if err := b.Unmount(m.ID); !errors.Is(err, impl.ErrQueueBusy) {
		t.Fatalf("unmounting with a queued request must fail with ErrQueueBusy, got %v", err)
	}
	if got := len(b.Mounts()); got != 1 {
		t.Fatalf("mounts after the refused unmount = %d, want 1", got)
	}
	if err := b.SetBackend(m.ID, true); err != nil {
		t.Fatalf("bring the backend online: %v", err)
	}
	if _, err := b.Flush(m.ID); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := b.Unmount(m.ID); err != nil {
		t.Fatalf("unmount after draining: %v", err)
	}
	if got := len(b.Mounts()); got != 0 {
		t.Fatalf("mounts = %d, want 0", got)
	}
	if _, err := b.Stats(m.ID); !errors.Is(err, impl.ErrUnknownMount) {
		t.Fatalf("stats of a removed mount must fail with ErrUnknownMount, got %v", err)
	}
	if _, err := b.Depth(m.ID); !errors.Is(err, impl.ErrUnknownMount) {
		t.Fatalf("depth of a removed mount must fail with ErrUnknownMount, got %v", err)
	}
	if err := b.Unmount(m.ID); !errors.Is(err, impl.ErrUnknownMount) {
		t.Fatalf("unmounting twice must fail with ErrUnknownMount, got %v", err)
	}
}

func TestConcurrentForwardCounts(t *testing.T) {
	b := impl.NewFuseMountBroker(brokerLimits())
	m := addMount(t, b, "/srv/data", 1000, false)
	const workers = 8
	const perWorker = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				if _, err := b.Forward(m.ID, ownerRequest(fmt.Sprintf("w%d-%d", i, j), 1000)); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent forward: %v", err)
	}
	st, err := b.Stats(m.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Requests != workers*perWorker {
		t.Fatalf("served requests = %d, want %d", st.Requests, workers*perWorker)
	}
	if st.Denied != 0 || st.Queued != 0 || st.Dropped != 0 {
		t.Fatalf("stats = %+v, want no denials or drops", st)
	}
}

func TestSnapshotRestoreKeepsQueueAndMapping(t *testing.T) {
	limits := brokerLimits()
	b := impl.NewFuseMountBroker(limits)
	m := addMount(t, b, "/srv/shared", 1000, true)
	if err := b.SetMapping(m.ID, []impl.IDMap{{HostID: 1001, ContainerID: 0}}); err != nil {
		t.Fatalf("set mapping: %v", err)
	}
	if err := b.SetBackend(m.ID, false); err != nil {
		t.Fatalf("take the backend offline: %v", err)
	}
	if _, err := b.Forward(m.ID, ownerRequest("q1", 1001)); err != nil {
		t.Fatalf("queue one request: %v", err)
	}
	data, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewFuseMountBroker(limits)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	mounts := r.Mounts()
	if len(mounts) != 1 || mounts[0].ID != m.ID || mounts[0].Mountpoint != "/srv/shared" {
		t.Fatalf("restored mounts = %+v, want the original mount", mounts)
	}
	if depth, err := r.Depth(m.ID); err != nil || depth != 1 {
		t.Fatalf("restored depth = %d err=%v, want the queued request kept", depth, err)
	}
	if st, err := r.Stats(m.ID); err != nil || st.Queued != 1 || st.Requests != 1 {
		t.Fatalf("restored stats = %+v err=%v, want one queued request", st, err)
	}
	if err := r.SetBackend(m.ID, true); err != nil {
		t.Fatalf("bring the restored backend online: %v", err)
	}
	if n, err := r.Flush(m.ID); err != nil || n != 1 {
		t.Fatalf("flush after restore drained %d err=%v, want 1", n, err)
	}
	reply, err := r.Forward(m.ID, ownerRequest("r2", 1001))
	if err != nil || reply.Code != 200 || reply.MappedUID != 0 {
		t.Fatalf("the restored mapping must still translate the caller, reply=%+v err=%v", reply, err)
	}
	if err := r.Restore([]byte("broken-broker-state")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a broken snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(r.Mounts()); got != 1 {
		t.Fatalf("mounts after the rejected restore = %d, want the live 1", got)
	}
}
