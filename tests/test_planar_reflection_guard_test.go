package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-08"
)

func guardLimits() impl.Limits {
	return impl.Limits{MaxPlanes: 4, AtlasWidth: 256, AtlasHeight: 256}
}

func flatPlane(width, height int) impl.PlaneSpec {
	return impl.PlaneSpec{Point: impl.Vec3{}, Normal: impl.Vec3{Y: 1}, Priority: 1, Width: width, Height: height}
}

func playerCamera() impl.CameraParams {
	return impl.CameraParams{
		Position: impl.Vec3{X: 2, Y: 3, Z: -4},
		Forward:  impl.Vec3{X: 0, Y: -0.6, Z: 0.8},
		Up:       impl.Vec3{X: 0, Y: 0, Z: 1},
		FOV:      60,
		Near:     0.1,
		Far:      100,
	}
}

func TestRegisterPlaneStoresSinglePlane(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(64, 64)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	planes := g.Planes()
	if len(planes) != 1 || planes[0] != "water" {
		t.Fatalf("planes = %v, want [water]", planes)
	}
	st, ok := g.State("water")
	if !ok {
		t.Fatalf("state of a registered plane must be readable")
	}
	if st.EveryN != 1 {
		t.Fatalf("default update rate = %d, want 1", st.EveryN)
	}
	if st.Generation != 0 {
		t.Fatalf("fresh plane generation = %d, want 0", st.Generation)
	}
	if st.AtlasW != 64 || st.AtlasH != 64 {
		t.Fatalf("atlas viewport = %dx%d, want 64x64", st.AtlasW, st.AtlasH)
	}
	if st.AtlasX != 0 || st.AtlasY != 0 {
		t.Fatalf("first plane viewport origin = (%d,%d), want (0,0)", st.AtlasX, st.AtlasY)
	}
	if !st.Enabled {
		t.Fatalf("a fresh plane must start enabled")
	}
	layout := g.Atlas()
	if layout.Width != 256 || layout.Height != 256 {
		t.Fatalf("atlas = %dx%d, want 256x256", layout.Width, layout.Height)
	}
	if len(layout.Entries) != 1 || layout.Entries[0].PlaneID != "water" || layout.Entries[0].W != 64 {
		t.Fatalf("atlas entries = %+v, want the water viewport", layout.Entries)
	}
	if stats := g.Stats(); stats.Planes != 1 || stats.Updates != 0 {
		t.Fatalf("stats = %+v, want one plane and no commits", stats)
	}
	if _, ok := g.State("mirror"); ok {
		t.Fatalf("an unregistered plane must not report state")
	}
	if err := g.RegisterPlane("mirror", flatPlane(32, 32)); err != nil {
		t.Fatalf("register mirror: %v", err)
	}
	if planes := g.Planes(); len(planes) != 2 || planes[0] != "mirror" || planes[1] != "water" {
		t.Fatalf("planes = %v, want ascending order", planes)
	}
}

func TestRegisterPlaneRejectsBadSpec(t *testing.T) {
	cases := []struct {
		id    string
		plane impl.PlaneSpec
	}{
		{"  ", flatPlane(16, 16)},
		{"water", impl.PlaneSpec{Point: impl.Vec3{}, Normal: impl.Vec3{}, Width: 16, Height: 16}},
		{"water", impl.PlaneSpec{Point: impl.Vec3{}, Normal: impl.Vec3{X: math.NaN(), Y: 1}, Width: 16, Height: 16}},
		{"water", impl.PlaneSpec{Point: impl.Vec3{}, Normal: impl.Vec3{Y: 1}, Width: 0, Height: 16}},
		{"water", impl.PlaneSpec{Point: impl.Vec3{}, Normal: impl.Vec3{Y: 1}, Width: 16, Height: -4}},
	}
	g := impl.NewPlanarReflectionGuard(guardLimits())
	for i, c := range cases {
		if err := g.RegisterPlane(c.id, c.plane); !errors.Is(err, impl.ErrBadPlane) {
			t.Fatalf("case %d must fail with ErrBadPlane, got %v", i, err)
		}
	}
	if planes := g.Planes(); len(planes) != 0 {
		t.Fatalf("rejected planes must not enter the table, got %v", planes)
	}
}

func TestRegisterPlaneRejectsDuplicate(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(16, 16)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if err := g.RegisterPlane("water", flatPlane(8, 8)); !errors.Is(err, impl.ErrDuplicatePlane) {
		t.Fatalf("duplicate plane must fail with ErrDuplicatePlane, got %v", err)
	}
	if planes := g.Planes(); len(planes) != 1 {
		t.Fatalf("plane count = %d, want 1", len(planes))
	}
}

func TestRegisterPlaneEnforcesLimit(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(impl.Limits{MaxPlanes: 2, AtlasWidth: 256, AtlasHeight: 256})
	if err := g.RegisterPlane("a", flatPlane(16, 16)); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := g.RegisterPlane("b", flatPlane(16, 16)); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if err := g.RegisterPlane("c", flatPlane(16, 16)); !errors.Is(err, impl.ErrPlaneLimit) {
		t.Fatalf("third plane over the cap must fail with ErrPlaneLimit, got %v", err)
	}
	if planes := g.Planes(); len(planes) != 2 {
		t.Fatalf("plane count = %d, want the cap 2", len(planes))
	}
}

func TestBuildMirrorCameraMirrorsPosition(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	mc, err := g.BuildMirrorCamera("water", playerCamera())
	if err != nil {
		t.Fatalf("build mirror camera: %v", err)
	}
	if mc.PlaneID != "water" {
		t.Fatalf("mirror camera plane = %q, want water", mc.PlaneID)
	}
	if mc.Position.X != 2 || mc.Position.Y != -3 || mc.Position.Z != -4 {
		t.Fatalf("mirror position = %+v, want (2,-3,-4)", mc.Position)
	}
	if math.Abs(mc.Forward.Y-0.6) > 1e-9 || math.Abs(mc.Forward.Z-0.8) > 1e-9 || mc.Forward.X != 0 {
		t.Fatalf("mirror forward = %+v, want (0,0.6,0.8)", mc.Forward)
	}
	if mc.Near != 0.1 || mc.Far != 100 {
		t.Fatalf("mirror near/far = %v/%v, want the camera values", mc.Near, mc.Far)
	}
	if !mc.Enabled {
		t.Fatalf("a plane above the camera must stay enabled")
	}
}

func TestBuildMirrorCameraMirrorsUpAndClipPlane(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	cam := impl.CameraParams{
		Position: impl.Vec3{X: 1, Y: 5, Z: 1},
		Forward:  impl.Vec3{X: 0, Y: -0.70710678, Z: 0.70710678},
		Up:       impl.Vec3{X: 0, Y: 0, Z: 1},
		FOV:      70,
		Near:     0.05,
		Far:      500,
	}
	mc, err := g.BuildMirrorCamera("water", cam)
	if err != nil {
		t.Fatalf("build mirror camera: %v", err)
	}
	if mc.Up.X != 0 || mc.Up.Y != 0 || mc.Up.Z != 1 {
		t.Fatalf("mirror up = %+v, want (0,0,1) for a plane with normal +Y", mc.Up)
	}
	if mc.ClipPlane.X != 0 || mc.ClipPlane.Y != 1 || mc.ClipPlane.Z != 0 || mc.ClipPlane.W != 0 {
		t.Fatalf("clip plane = %+v, want (0,1,0,0)", mc.ClipPlane)
	}
}

func TestBuildMirrorCameraRejectsBadInput(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if _, err := g.BuildMirrorCamera("mirror", playerCamera()); !errors.Is(err, impl.ErrUnknownPlane) {
		t.Fatalf("unknown plane must fail with ErrUnknownPlane, got %v", err)
	}
	bad := []impl.CameraParams{
		{Position: impl.Vec3{}, Forward: impl.Vec3{}, Up: impl.Vec3{Z: 1}, FOV: 60, Near: 0.1, Far: 100},
		{Position: impl.Vec3{}, Forward: impl.Vec3{Z: 1}, Up: impl.Vec3{}, FOV: 60, Near: 0.1, Far: 100},
		{Position: impl.Vec3{}, Forward: impl.Vec3{Z: 1}, Up: impl.Vec3{Y: 1}, FOV: 0, Near: 0.1, Far: 100},
		{Position: impl.Vec3{}, Forward: impl.Vec3{Z: 1}, Up: impl.Vec3{Y: 1}, FOV: 60, Near: 0, Far: 100},
		{Position: impl.Vec3{}, Forward: impl.Vec3{Z: 1}, Up: impl.Vec3{Y: 1}, FOV: 60, Near: 10, Far: 1},
	}
	for i, cam := range bad {
		if _, err := g.BuildMirrorCamera("water", cam); !errors.Is(err, impl.ErrBadCamera) {
			t.Fatalf("camera case %d must fail with ErrBadCamera, got %v", i, err)
		}
	}
}

func TestSetUpdateRateValidates(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if err := g.SetUpdateRate("water", 0); !errors.Is(err, impl.ErrBadUpdateRate) {
		t.Fatalf("zero rate must fail with ErrBadUpdateRate, got %v", err)
	}
	if err := g.SetUpdateRate("water", -3); !errors.Is(err, impl.ErrBadUpdateRate) {
		t.Fatalf("negative rate must fail with ErrBadUpdateRate, got %v", err)
	}
	if err := g.SetUpdateRate("mirror", 2); !errors.Is(err, impl.ErrUnknownPlane) {
		t.Fatalf("unknown plane must fail with ErrUnknownPlane, got %v", err)
	}
	if err := g.SetUpdateRate("water", 5); err != nil {
		t.Fatalf("set rate 5: %v", err)
	}
	st, _ := g.State("water")
	if st.EveryN != 5 {
		t.Fatalf("update rate = %d, want 5", st.EveryN)
	}
}

func TestCommitRequiresOpenFrame(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if _, err := g.Commit("water"); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("commit before BeginFrame must fail with ErrNoFrame, got %v", err)
	}
	if err := g.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	if _, err := g.Commit("mirror"); !errors.Is(err, impl.ErrUnknownPlane) {
		t.Fatalf("commit on an unknown plane must fail with ErrUnknownPlane, got %v", err)
	}
}

func TestBeginFrameRejectsNonIncreasing(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.BeginFrame(2); err != nil {
		t.Fatalf("begin frame 2: %v", err)
	}
	if err := g.BeginFrame(2); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("repeating frame 2 must fail with ErrBadFrame, got %v", err)
	}
	if err := g.BeginFrame(1); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("going back to frame 1 must fail with ErrBadFrame, got %v", err)
	}
	if got := g.Frame(); got != 2 {
		t.Fatalf("frame after rejected begin = %d, want 2", got)
	}
}

func TestCommitHonorsUpdateRate(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if err := g.SetUpdateRate("water", 3); err != nil {
		t.Fatalf("set rate: %v", err)
	}
	if err := g.BeginFrame(10); err != nil {
		t.Fatalf("begin frame 10: %v", err)
	}
	first, err := g.Commit("water")
	if err != nil {
		t.Fatalf("commit frame 10: %v", err)
	}
	if !first.Updated || first.Generation != 1 || first.Frame != 10 {
		t.Fatalf("first commit = %+v, want an update in frame 10 with generation 1", first)
	}
	for _, frame := range []int{11, 12} {
		if err := g.BeginFrame(frame); err != nil {
			t.Fatalf("begin frame %d: %v", frame, err)
		}
		st, err := g.Commit("water")
		if err != nil {
			t.Fatalf("commit frame %d: %v", frame, err)
		}
		if st.Updated || st.Generation != 1 || st.Frame != 10 {
			t.Fatalf("commit in frame %d = %+v, want a skip that keeps generation 1 and frame 10", frame, st)
		}
	}
	if err := g.BeginFrame(13); err != nil {
		t.Fatalf("begin frame 13: %v", err)
	}
	next, err := g.Commit("water")
	if err != nil {
		t.Fatalf("commit frame 13: %v", err)
	}
	if !next.Updated || next.Generation != 2 || next.Frame != 13 {
		t.Fatalf("commit in frame 13 = %+v, want generation 2 at frame 13", next)
	}
	stats := g.Stats()
	if stats.Updates != 2 || stats.Skipped != 2 {
		t.Fatalf("stats = %+v, want 2 updates and 2 skips", stats)
	}
}

func TestInvalidateForcesNextCommit(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if err := g.SetUpdateRate("water", 10); err != nil {
		t.Fatalf("set rate: %v", err)
	}
	if err := g.BeginFrame(1); err != nil {
		t.Fatalf("begin frame 1: %v", err)
	}
	if st, err := g.Commit("water"); err != nil || !st.Updated {
		t.Fatalf("first commit must render, got %+v err=%v", st, err)
	}
	if err := g.BeginFrame(2); err != nil {
		t.Fatalf("begin frame 2: %v", err)
	}
	if st, err := g.Commit("water"); err != nil || st.Updated {
		t.Fatalf("second commit must be skipped by the rate, got %+v err=%v", st, err)
	}
	if err := g.Invalidate("water"); err != nil {
		t.Fatalf("invalidate water: %v", err)
	}
	st, err := g.Commit("water")
	if err != nil {
		t.Fatalf("commit after invalidate: %v", err)
	}
	if !st.Updated || st.Generation != 2 || st.Frame != 2 {
		t.Fatalf("commit after invalidate = %+v, want generation 2 at frame 2", st)
	}
	if err := g.Invalidate("mirror"); !errors.Is(err, impl.ErrUnknownPlane) {
		t.Fatalf("invalidating an unknown plane must fail with ErrUnknownPlane, got %v", err)
	}
}

func TestResolutionChangeRejectsBadAndOverflow(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(impl.Limits{MaxPlanes: 4, AtlasWidth: 64, AtlasHeight: 64})
	if err := g.RegisterPlane("too.big", flatPlane(128, 128)); !errors.Is(err, impl.ErrAtlasBudget) {
		t.Fatalf("a plane bigger than the atlas must fail with ErrAtlasBudget, got %v", err)
	}
	if err := g.RegisterPlane("lake", flatPlane(32, 32)); err != nil {
		t.Fatalf("register lake: %v", err)
	}
	if planes := g.Planes(); len(planes) != 1 {
		t.Fatalf("plane count = %d, want only lake", len(planes))
	}
	if err := g.SetResolution("lake", 0, 32); !errors.Is(err, impl.ErrBadResolution) {
		t.Fatalf("zero width must fail with ErrBadResolution, got %v", err)
	}
	if err := g.SetResolution("lake", 100, 100); !errors.Is(err, impl.ErrAtlasBudget) {
		t.Fatalf("resolution over the atlas must fail with ErrAtlasBudget, got %v", err)
	}
	if err := g.SetResolution("lake", 48, 48); err != nil {
		t.Fatalf("set resolution 48x48: %v", err)
	}
	st, ok := g.State("lake")
	if !ok || st.AtlasW != 48 || st.AtlasH != 48 {
		t.Fatalf("viewport after resize = %+v ok=%v, want 48x48", st, ok)
	}
	if err := g.SetResolution("mirror", 4, 4); !errors.Is(err, impl.ErrUnknownPlane) {
		t.Fatalf("resizing an unknown plane must fail with ErrUnknownPlane, got %v", err)
	}
}

func TestSnapshotRestoreKeepsRateAndGeneration(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if err := g.RegisterPlane("mirror", flatPlane(16, 16)); err != nil {
		t.Fatalf("register mirror: %v", err)
	}
	if err := g.SetUpdateRate("water", 4); err != nil {
		t.Fatalf("set water rate: %v", err)
	}
	if err := g.BeginFrame(3); err != nil {
		t.Fatalf("begin frame 3: %v", err)
	}
	if _, err := g.Commit("water"); err != nil {
		t.Fatalf("commit water: %v", err)
	}
	if _, err := g.Commit("mirror"); err != nil {
		t.Fatalf("commit mirror: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewPlanarReflectionGuard(guardLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Frame(); got != 3 {
		t.Fatalf("restored frame = %d, want 3", got)
	}
	st, ok := restored.State("water")
	if !ok || st.EveryN != 4 || st.Generation != 1 || st.AtlasW != 32 {
		t.Fatalf("restored water state = %+v ok=%v, want rate 4, generation 1 and a 32 wide viewport", st, ok)
	}
	if planes := restored.Planes(); len(planes) != 2 {
		t.Fatalf("restored planes = %v, want two", planes)
	}
	if err := restored.BeginFrame(3); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("frame 3 must already be spent after restore, got %v", err)
	}
	if err := restored.Restore([]byte("garbage")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Frame(); got != 3 {
		t.Fatalf("rejected restore must not touch the live frame, got %d", got)
	}
}

func TestConcurrentCommitUpdatesOncePerFrame(t *testing.T) {
	g := impl.NewPlanarReflectionGuard(guardLimits())
	if err := g.RegisterPlane("water", flatPlane(32, 32)); err != nil {
		t.Fatalf("register water: %v", err)
	}
	if err := g.BeginFrame(5); err != nil {
		t.Fatalf("begin frame 5: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	updated := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := g.Commit("water")
			if err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			if st.Updated {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if updated != 1 {
		t.Fatalf("updated commits in one frame = %d, want exactly 1", updated)
	}
	st, _ := g.State("water")
	if st.Generation != 1 {
		t.Fatalf("generation after the race = %d, want 1", st.Generation)
	}
	if stats := g.Stats(); stats.Updates != 1 {
		t.Fatalf("update counter = %d, want 1", stats.Updates)
	}
}
