package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-b-07"
)

func testLimits() impl.Limits {
	return impl.Limits{MaxResources: 8, MaxAttempts: 2, MaxLevel: 3, RebuildBudget: time.Minute}
}

func registerSpec(t *testing.T, d *impl.DeviceLostRecovery, spec impl.ResourceSpec) impl.Handle {
	t.Helper()
	h, err := d.Register(spec)
	if err != nil {
		t.Fatalf("register %s: %v", spec.Name, err)
	}
	return h
}

func TestRegisterAllocatesMonotonicHandles(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	h1 := registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 4096})
	h2 := registerSpec(t, d, impl.ResourceSpec{Name: "main.ubo", Kind: impl.KindBuffer, Bytes: 256, DependsOn: []string{"albedo"}})
	h3 := registerSpec(t, d, impl.ResourceSpec{Name: "hdr", Kind: impl.KindTarget, Bytes: 8192})
	if h1 <= 0 || h2 <= h1 || h3 <= h2 {
		t.Fatalf("handles must be positive and increasing, got %d %d %d", h1, h2, h3)
	}
	st, err := d.Resolve(h3)
	if err != nil {
		t.Fatalf("resolve hdr: %v", err)
	}
	if st.Name != "hdr" || st.Kind != impl.KindTarget || st.Bytes != 8192 {
		t.Fatalf("resolved state = %+v, want the hdr target description", st)
	}
	if !st.Valid || st.Generation != 1 {
		t.Fatalf("fresh resource valid=%v generation=%d, want true and 1", st.Valid, st.Generation)
	}
	if got := d.Generation(); got != 1 {
		t.Fatalf("initial generation = %d, want 1", got)
	}
	if got := d.Status(); got != impl.StatusReady {
		t.Fatalf("status = %q, want %q", got, impl.StatusReady)
	}
	res := d.Resources()
	if len(res) != 3 || res[0] != h1 || res[1] != h2 || res[2] != h3 {
		t.Fatalf("resources = %v, want [%d %d %d] ascending", res, h1, h2, h3)
	}
	if _, err := d.Resolve(impl.Handle(0)); !errors.Is(err, impl.ErrUnknownHandle) {
		t.Fatalf("zero handle must fail with ErrUnknownHandle, got %v", err)
	}
	if _, err := d.Resolve(impl.Handle(999)); !errors.Is(err, impl.ErrUnknownHandle) {
		t.Fatalf("unknown handle must fail with ErrUnknownHandle, got %v", err)
	}
}

func TestRegisterRejectsBadSpec(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	cases := []impl.ResourceSpec{
		{Name: "  ", Kind: impl.KindTexture, Bytes: 16},
		{Name: "no.kind", Kind: "shader", Bytes: 16},
		{Name: "zero.bytes", Kind: impl.KindBuffer, Bytes: 0},
		{Name: "negative.bytes", Kind: impl.KindBuffer, Bytes: -8},
	}
	for i, spec := range cases {
		if _, err := d.Register(spec); !errors.Is(err, impl.ErrBadResource) {
			t.Fatalf("case %d (%+v) must fail with ErrBadResource, got %v", i, spec, err)
		}
	}
	if _, err := d.Register(impl.ResourceSpec{Name: "self", Kind: impl.KindBuffer, Bytes: 16, DependsOn: []string{"self"}}); !errors.Is(err, impl.ErrBadResource) {
		t.Fatalf("a resource depending on itself must fail with ErrBadResource, got %v", err)
	}
	if got := len(d.Resources()); got != 0 {
		t.Fatalf("rejected registrations must not enter the table, got %d resources", got)
	}
}

func TestRegisterRejectsDuplicateName(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 32})
	if _, err := d.Register(impl.ResourceSpec{Name: "albedo", Kind: impl.KindBuffer, Bytes: 32}); !errors.Is(err, impl.ErrDuplicateResource) {
		t.Fatalf("duplicate name must fail with ErrDuplicateResource, got %v", err)
	}
	if got := len(d.Resources()); got != 1 {
		t.Fatalf("resource count = %d, want 1", got)
	}
}

func TestRegisterEnforcesMaxResources(t *testing.T) {
	d := impl.NewDeviceLostRecovery(impl.Limits{MaxResources: 2, MaxAttempts: 2, MaxLevel: 3})
	registerSpec(t, d, impl.ResourceSpec{Name: "a", Kind: impl.KindTexture, Bytes: 8})
	registerSpec(t, d, impl.ResourceSpec{Name: "b", Kind: impl.KindBuffer, Bytes: 8})
	if _, err := d.Register(impl.ResourceSpec{Name: "c", Kind: impl.KindBuffer, Bytes: 8}); !errors.Is(err, impl.ErrResourceLimit) {
		t.Fatalf("third resource over the cap must fail with ErrResourceLimit, got %v", err)
	}
	if got := len(d.Resources()); got != 2 {
		t.Fatalf("resource count = %d, want the cap 2", got)
	}
}

func TestRegisterRejectsUnknownDependency(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	if _, err := d.Register(impl.ResourceSpec{Name: "ssao", Kind: impl.KindPipeline, Bytes: 64, DependsOn: []string{"depth"}}); !errors.Is(err, impl.ErrMissingDependency) {
		t.Fatalf("unknown dependency must fail with ErrMissingDependency, got %v", err)
	}
	if got := len(d.Resources()); got != 0 {
		t.Fatalf("rejected resource must not be stored, got %d", got)
	}
	registerSpec(t, d, impl.ResourceSpec{Name: "depth", Kind: impl.KindTexture, Bytes: 1024})
	registerSpec(t, d, impl.ResourceSpec{Name: "ssao", Kind: impl.KindPipeline, Bytes: 64, DependsOn: []string{"depth"}})
	if got := len(d.Resources()); got != 2 {
		t.Fatalf("resource count = %d, want 2", got)
	}
}

func TestRegisterRejectedWhileLost(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 16})
	if err := d.OnLost("gpu hang"); err != nil {
		t.Fatalf("mark device lost: %v", err)
	}
	if _, err := d.Register(impl.ResourceSpec{Name: "late", Kind: impl.KindBuffer, Bytes: 16}); !errors.Is(err, impl.ErrDeviceLost) {
		t.Fatalf("registering while the device is lost must fail with ErrDeviceLost, got %v", err)
	}
}

func TestOnLostRequiresReason(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	h := registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 16})
	if err := d.OnLost("   "); !errors.Is(err, impl.ErrInvalidReason) {
		t.Fatalf("blank loss reason must fail with ErrInvalidReason, got %v", err)
	}
	if got := d.Status(); got != impl.StatusReady {
		t.Fatalf("status after a rejected loss = %q, want ready", got)
	}
	if st, err := d.Resolve(h); err != nil || !st.Valid {
		t.Fatalf("live handle must stay readable, got state=%+v err=%v", st, err)
	}
}

func TestOnLostTwiceFails(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 16})
	if err := d.OnLost("reset"); err != nil {
		t.Fatalf("first loss: %v", err)
	}
	if err := d.OnLost("reset again"); !errors.Is(err, impl.ErrAlreadyLost) {
		t.Fatalf("second loss must fail with ErrAlreadyLost, got %v", err)
	}
	if got := d.Status(); got != impl.StatusLost {
		t.Fatalf("status = %q, want lost", got)
	}
}

func TestHandleInvalidAfterLoss(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	h := registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 16})
	if err := d.OnLost("tiled memory corruption"); err != nil {
		t.Fatalf("mark device lost: %v", err)
	}
	if _, err := d.Resolve(h); !errors.Is(err, impl.ErrStaleHandle) {
		t.Fatalf("handle of a lost device must fail with ErrStaleHandle, got %v", err)
	}
	if got := len(d.Resources()); got != 0 {
		t.Fatalf("no resource is live while the device is lost, got %d", got)
	}
	if got := d.Generation(); got != 1 {
		t.Fatalf("generation before the rebuild = %d, want 1", got)
	}
}

func TestRecreateWithoutLossFails(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 16})
	if _, err := d.Recreate(time.Unix(100, 0)); !errors.Is(err, impl.ErrNotLost) {
		t.Fatalf("rebuild on a healthy device must fail with ErrNotLost, got %v", err)
	}
}

func TestRecreateRebuildsInKindOrder(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	registerSpec(t, d, impl.ResourceSpec{Name: "t1", Kind: impl.KindTarget, Bytes: 4096})
	registerSpec(t, d, impl.ResourceSpec{Name: "b1", Kind: impl.KindBuffer, Bytes: 512})
	registerSpec(t, d, impl.ResourceSpec{Name: "tx1", Kind: impl.KindTexture, Bytes: 2048, DependsOn: []string{"b1"}})
	registerSpec(t, d, impl.ResourceSpec{Name: "p1", Kind: impl.KindPipeline, Bytes: 128})
	registerSpec(t, d, impl.ResourceSpec{Name: "tx2", Kind: impl.KindTexture, Bytes: 2048})
	if err := d.OnLost("gpu hang"); err != nil {
		t.Fatalf("mark device lost: %v", err)
	}
	report, err := d.Recreate(time.Unix(200, 0))
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if report.Generation != 2 || report.Attempts != 1 || report.Level != 0 {
		t.Fatalf("report = %+v, want generation 2, attempt 1, level 0", report)
	}
	if report.Reason != "gpu hang" {
		t.Fatalf("report reason = %q, want the loss reason", report.Reason)
	}
	wantNames := []string{"p1", "tx1", "tx2", "b1", "t1"}
	if len(report.Rebuilt) != len(wantNames) {
		t.Fatalf("rebuilt %d resources, want %d", len(report.Rebuilt), len(wantNames))
	}
	for i, want := range wantNames {
		st, err := d.Resolve(report.Rebuilt[i])
		if err != nil {
			t.Fatalf("resolve rebuilt handle %d: %v", i, err)
		}
		if st.Name != want {
			t.Fatalf("rebuild order = %v, want pipelines then textures then buffers then targets", rebuildNames(t, d, report.Rebuilt))
		}
		if st.Generation != 2 {
			t.Fatalf("rebuilt resource %s generation = %d, want 2", st.Name, st.Generation)
		}
	}
	if got := d.Status(); got != impl.StatusReady {
		t.Fatalf("status after rebuild = %q, want ready", got)
	}
	if got := d.Generation(); got != 2 {
		t.Fatalf("generation after rebuild = %d, want 2", got)
	}
	if got := d.Resources(); len(got) != 5 || got[0] != report.Rebuilt[0] {
		t.Fatalf("live resources = %v, want the rebuilt handle set", got)
	}
}

func rebuildNames(t *testing.T, d *impl.DeviceLostRecovery, handles []impl.Handle) []string {
	t.Helper()
	names := make([]string, 0, len(handles))
	for _, h := range handles {
		st, err := d.Resolve(h)
		if err != nil {
			return []string{fmt.Sprintf("<unresolved %d>", h)}
		}
		names = append(names, st.Name)
	}
	return names
}

func TestRecreateAttemptBudget(t *testing.T) {
	d := impl.NewDeviceLostRecovery(impl.Limits{MaxResources: 8, MaxAttempts: 2, MaxLevel: 3})
	registerSpec(t, d, impl.ResourceSpec{Name: "a", Kind: impl.KindBuffer, Bytes: 16, DependsOn: []string{"b"}})
	registerSpec(t, d, impl.ResourceSpec{Name: "b", Kind: impl.KindBuffer, Bytes: 16, DependsOn: []string{"a"}})
	if err := d.OnLost("page fault"); err != nil {
		t.Fatalf("mark device lost: %v", err)
	}
	if _, err := d.Recreate(time.Unix(300, 0)); !errors.Is(err, impl.ErrDependencyCycle) {
		t.Fatalf("first rebuild over a dependency cycle must fail with ErrDependencyCycle, got %v", err)
	}
	if _, err := d.Recreate(time.Unix(301, 0)); !errors.Is(err, impl.ErrDependencyCycle) {
		t.Fatalf("second rebuild over a dependency cycle must fail with ErrDependencyCycle, got %v", err)
	}
	if _, err := d.Recreate(time.Unix(302, 0)); !errors.Is(err, impl.ErrRecoveryBudget) {
		t.Fatalf("a third rebuild with MaxAttempts=2 must fail with ErrRecoveryBudget, got %v", err)
	}
	if got := d.Status(); got != impl.StatusLost {
		t.Fatalf("status after the exhausted budget = %q, want lost", got)
	}
	if got := len(d.Resources()); got != 0 {
		t.Fatalf("failed rebuilds must not publish handles, got %d", got)
	}
}

func TestDegradeValidatesLevel(t *testing.T) {
	d := impl.NewDeviceLostRecovery(impl.Limits{MaxResources: 8, MaxAttempts: 2, MaxLevel: 3})
	if err := d.Degrade(-1); !errors.Is(err, impl.ErrBadDegradeLevel) {
		t.Fatalf("negative level must fail with ErrBadDegradeLevel, got %v", err)
	}
	if err := d.Degrade(4); !errors.Is(err, impl.ErrBadDegradeLevel) {
		t.Fatalf("level above MaxLevel must fail with ErrBadDegradeLevel, got %v", err)
	}
	if got := d.DegradeLevel(); got != 0 {
		t.Fatalf("level after rejected calls = %d, want 0", got)
	}
	if err := d.Degrade(2); err != nil {
		t.Fatalf("degrade to level 2: %v", err)
	}
	if got := d.DegradeLevel(); got != 2 {
		t.Fatalf("level = %d, want 2", got)
	}
	if err := d.Degrade(2); err != nil {
		t.Fatalf("repeating the same level must stay quiet, got %v", err)
	}
	if err := d.Degrade(3); err != nil {
		t.Fatalf("degrade further to level 3: %v", err)
	}
	if got := d.DegradeLevel(); got != 3 {
		t.Fatalf("level = %d, want 3", got)
	}
}

func TestConcurrentRegisterRespectsLimit(t *testing.T) {
	d := impl.NewDeviceLostRecovery(impl.Limits{MaxResources: 5, MaxAttempts: 2, MaxLevel: 3})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = d.Register(impl.ResourceSpec{Name: fmt.Sprintf("res-%02d", i), Kind: impl.KindBuffer, Bytes: 16})
		}(i)
	}
	wg.Wait()
	if got := len(d.Resources()); got != 5 {
		t.Fatalf("resources after the race = %d, want the cap 5", got)
	}
	if _, err := d.Register(impl.ResourceSpec{Name: "overflow", Kind: impl.KindBuffer, Bytes: 16}); !errors.Is(err, impl.ErrResourceLimit) {
		t.Fatalf("the cap must hold after the race, got %v", err)
	}
}

func TestSnapshotRestoreKeepsGenerationAndLevel(t *testing.T) {
	d := impl.NewDeviceLostRecovery(testLimits())
	registerSpec(t, d, impl.ResourceSpec{Name: "albedo", Kind: impl.KindTexture, Bytes: 1024})
	h2 := registerSpec(t, d, impl.ResourceSpec{Name: "main.ubo", Kind: impl.KindBuffer, Bytes: 256})
	if err := d.OnLost("xid 79"); err != nil {
		t.Fatalf("mark device lost: %v", err)
	}
	report, err := d.Recreate(time.Unix(400, 0))
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if err := d.Degrade(1); err != nil {
		t.Fatalf("degrade: %v", err)
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewDeviceLostRecovery(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Generation(); got != 2 {
		t.Fatalf("restored generation = %d, want 2", got)
	}
	if got := restored.DegradeLevel(); got != 1 {
		t.Fatalf("restored level = %d, want 1", got)
	}
	if got := restored.Status(); got != impl.StatusReady {
		t.Fatalf("restored status = %q, want ready", got)
	}
	res := restored.Resources()
	if len(res) != 2 {
		t.Fatalf("restored resources = %v, want two handles", res)
	}
	live := res[len(res)-1]
	if st, err := restored.Resolve(live); err != nil || st.Name != "main.ubo" {
		t.Fatalf("restored handle must resolve to main.ubo, got %+v err=%v", st, err)
	}
	if _, err := restored.Resolve(h2); !errors.Is(err, impl.ErrStaleHandle) {
		t.Fatalf("the pre-loss handle must stay stale after restore, got %v", err)
	}
	if len(report.Rebuilt) != 2 {
		t.Fatalf("rebuild report listed %d handles, want 2", len(report.Rebuilt))
	}
	if err := restored.Restore([]byte("not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Generation(); got != 2 {
		t.Fatalf("rejected restore must not touch the live generation, got %d", got)
	}
}
