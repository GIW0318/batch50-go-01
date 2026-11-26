package task

import (
	"errors"
	"math"
	"testing"

	impl "gsb/batch50-go-game-b-19"
)

func newScheduler(t *testing.T) *impl.NormalRebuildScheduler {
	t.Helper()
	s := impl.NewNormalRebuildScheduler()
	if s == nil {
		t.Fatalf("new scheduler returned nil")
	}
	return s
}

// singleTri is one triangle in the XY plane, its normal is +Z.
func singleTri() impl.RebuildMesh {
	return impl.RebuildMesh{
		ID: "single",
		Verts: []impl.Vec3{
			{X: 0, Y: 0, Z: 0},
			{X: 1, Y: 0, Z: 0},
			{X: 0, Y: 1, Z: 0},
		},
		Tris: []impl.Tri{{A: 0, B: 1, C: 2}},
	}
}

// foldMesh is two triangles sharing the edge 0-1 with a 90 degree dihedral.
// The first triangle normal is +Z, the second is -Y.
func foldMesh() impl.RebuildMesh {
	return impl.RebuildMesh{
		ID: "fold",
		Verts: []impl.Vec3{
			{X: 0, Y: 0, Z: 0},
			{X: 1, Y: 0, Z: 0},
			{X: 0, Y: 1, Z: 0},
			{X: 0, Y: 0, Z: 1},
		},
		Tris: []impl.Tri{
			{A: 0, B: 1, C: 2},
			{A: 0, B: 1, C: 3},
		},
	}
}

// twoIslands is two disconnected triangles with six vertices.
func twoIslands() impl.RebuildMesh {
	return impl.RebuildMesh{
		ID: "islands",
		Verts: []impl.Vec3{
			{X: 0, Y: 0, Z: 0},
			{X: 1, Y: 0, Z: 0},
			{X: 0, Y: 1, Z: 0},
			{X: 5, Y: 0, Z: 0},
			{X: 6, Y: 0, Z: 0},
			{X: 5, Y: 1, Z: 0},
		},
		Tris: []impl.Tri{
			{A: 0, B: 1, C: 2},
			{A: 3, B: 4, C: 5},
		},
	}
}

func vecNear(a, b impl.Vec3) bool {
	return math.Abs(a.X-b.X) < 1e-6 && math.Abs(a.Y-b.Y) < 1e-6 && math.Abs(a.Z-b.Z) < 1e-6
}

func vecLen(v impl.Vec3) float64 {
	return math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
}

func TestRegisterMeshRejectsDuplicateAndBlank(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("   ", singleTri()); !errors.Is(err, impl.ErrBadName) {
		t.Fatalf("blank mesh name = %v, want ErrBadName", err)
	}
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.RegisterMesh("mesh-a", singleTri()); !errors.Is(err, impl.ErrMeshExists) {
		t.Fatalf("duplicate mesh = %v, want ErrMeshExists", err)
	}
	if got := s.Stats().Meshes; got != 1 {
		t.Fatalf("mesh count = %d, want 1", got)
	}
}

func TestRegisterMeshRejectsEmptyMesh(t *testing.T) {
	s := newScheduler(t)
	empty := impl.RebuildMesh{ID: "empty", Verts: singleTri().Verts}
	if err := s.RegisterMesh("mesh-empty", empty); !errors.Is(err, impl.ErrBadMesh) {
		t.Fatalf("mesh without triangles = %v, want ErrBadMesh", err)
	}
	broken := singleTri()
	broken.Tris = []impl.Tri{{A: 0, B: 1, C: 9}}
	if err := s.RegisterMesh("mesh-broken", broken); !errors.Is(err, impl.ErrBadMesh) {
		t.Fatalf("triangle with an out of range index = %v, want ErrBadMesh", err)
	}
	if got := s.Stats().Meshes; got != 0 {
		t.Fatalf("rejected meshes were registered, count = %d, want 0", got)
	}
}

func TestMarkDirtyRejectsUnknownMesh(t *testing.T) {
	s := newScheduler(t)
	if err := s.MarkDirty("ghost", []int{0}); !errors.Is(err, impl.ErrUnknownMesh) {
		t.Fatalf("dirty on unknown mesh = %v, want ErrUnknownMesh", err)
	}
	if _, err := s.Rebuild("ghost", 4); !errors.Is(err, impl.ErrUnknownMesh) {
		t.Fatalf("rebuild of unknown mesh = %v, want ErrUnknownMesh", err)
	}
	if _, err := s.Normals("ghost"); !errors.Is(err, impl.ErrUnknownMesh) {
		t.Fatalf("normals of unknown mesh = %v, want ErrUnknownMesh", err)
	}
}

func TestMarkDirtyRejectsOutOfRangeVertex(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{3}); !errors.Is(err, impl.ErrBadVertex) {
		t.Fatalf("vertex index 3 of a three vertex mesh = %v, want ErrBadVertex", err)
	}
	if err := s.MarkDirty("mesh-a", []int{-1}); !errors.Is(err, impl.ErrBadVertex) {
		t.Fatalf("negative vertex index = %v, want ErrBadVertex", err)
	}
	if got := s.DirtyCount("mesh-a"); got != 0 {
		t.Fatalf("dirty count after rejects = %d, want 0", got)
	}
}

func TestMarkDirtyDeduplicatesIndices(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{1, 1, 2, 2, 1}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if got := s.DirtyCount("mesh-a"); got != 2 {
		t.Fatalf("dirty count for {1,1,2,2,1} = %d, want 2", got)
	}
	if got := s.Stats().Dirty; got != 2 {
		t.Fatalf("stats dirty count = %d, want 2", got)
	}
}

func TestRebuildRejectsBadBudget(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{0}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if _, err := s.Rebuild("mesh-a", 0); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("zero budget = %v, want ErrBadBudget", err)
	}
	if _, err := s.Rebuild("mesh-a", -3); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("negative budget = %v, want ErrBadBudget", err)
	}
	if got := s.DirtyCount("mesh-a"); got != 1 {
		t.Fatalf("dirty count after rejected budgets = %d, want 1", got)
	}
}

func TestRebuildComputesFaceNormal(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{0, 1, 2}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	processed, err := s.Rebuild("mesh-a", 8)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if processed != 3 {
		t.Fatalf("rebuilt %d vertices, want 3", processed)
	}
	if got := s.DirtyCount("mesh-a"); got != 0 {
		t.Fatalf("dirty count after a full rebuild = %d, want 0", got)
	}
	normals, err := s.Normals("mesh-a")
	if err != nil {
		t.Fatalf("normals: %v", err)
	}
	if len(normals) != 3 {
		t.Fatalf("normal count = %d, want 3", len(normals))
	}
	want := impl.Vec3{X: 0, Y: 0, Z: 1}
	for i, n := range normals {
		if !vecNear(n, want) {
			t.Fatalf("normal %d = %+v, want the unit face normal %+v", i, n, want)
		}
	}
}

func TestRebuildRespectsBudget(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{0, 1, 2}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	processed, err := s.Rebuild("mesh-a", 1)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if processed != 1 {
		t.Fatalf("rebuilt %d vertices under a budget of 1, want exactly 1", processed)
	}
	if got := s.DirtyCount("mesh-a"); got != 2 {
		t.Fatalf("dirty count after the capped rebuild = %d, want the 2 deferred vertices", got)
	}
	processed, err = s.Rebuild("mesh-a", 8)
	if err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if processed < 2 || processed > 8 {
		t.Fatalf("second rebuild processed %d vertices, want the 2 deferred ones within budget 8", processed)
	}
	if got := s.DirtyCount("mesh-a"); got != 0 {
		t.Fatalf("dirty count after the deferred rebuild = %d, want 0", got)
	}
}

func TestRebuildKeepsUndirtyVertices(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", twoIslands()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{0, 1, 2}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if _, err := s.Rebuild("mesh-a", 8); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	normals, err := s.Normals("mesh-a")
	if err != nil {
		t.Fatalf("normals: %v", err)
	}
	if len(normals) != 6 {
		t.Fatalf("normal count = %d, want 6", len(normals))
	}
	for i := 0; i < 3; i++ {
		if !vecNear(normals[i], impl.Vec3{X: 0, Y: 0, Z: 1}) {
			t.Fatalf("dirty island vertex %d normal = %+v, want +Z", i, normals[i])
		}
	}
	for i := 3; i < 6; i++ {
		if normals[i] != (impl.Vec3{}) {
			t.Fatalf("untouched island vertex %d normal = %+v, want the zero vector", i, normals[i])
		}
	}
}

func TestFreshMeshNormalsAreZero(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	normals, err := s.Normals("mesh-a")
	if err != nil {
		t.Fatalf("normals of a fresh mesh: %v", err)
	}
	if len(normals) != 3 {
		t.Fatalf("fresh normal count = %d, want 3", len(normals))
	}
	for i, n := range normals {
		if n != (impl.Vec3{}) {
			t.Fatalf("fresh vertex %d normal = %+v, want the zero vector before any rebuild", i, n)
		}
	}
	if got := s.DirtyCount("mesh-a"); got != 0 {
		t.Fatalf("fresh mesh dirty count = %d, want 0", got)
	}
	if got := s.Stats().Dirty; got != 0 {
		t.Fatalf("fresh stats dirty count = %d, want 0", got)
	}
}

func TestNormalsReturnsDefensiveCopy(t *testing.T) {
	s := newScheduler(t)
	if err := s.RegisterMesh("mesh-a", singleTri()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-a", []int{0, 1, 2}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if _, err := s.Rebuild("mesh-a", 8); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	first, err := s.Normals("mesh-a")
	if err != nil {
		t.Fatalf("normals: %v", err)
	}
	first[0] = impl.Vec3{X: 42, Y: 42, Z: 42}
	second, err := s.Normals("mesh-a")
	if err != nil {
		t.Fatalf("second normals read: %v", err)
	}
	if second[0] == (impl.Vec3{X: 42, Y: 42, Z: 42}) {
		t.Fatalf("mutating the returned slice changed the stored normals")
	}
	if !vecNear(second[0], impl.Vec3{X: 0, Y: 0, Z: 1}) {
		t.Fatalf("stored normal = %+v, want the face normal +Z", second[0])
	}
}

func TestSetSmoothAngleRejectsOutOfRange(t *testing.T) {
	s := newScheduler(t)
	if err := s.SetSmoothAngle(-1); !errors.Is(err, impl.ErrBadAngle) {
		t.Fatalf("negative smooth angle = %v, want ErrBadAngle", err)
	}
	if err := s.SetSmoothAngle(180.5); !errors.Is(err, impl.ErrBadAngle) {
		t.Fatalf("smooth angle above 180 = %v, want ErrBadAngle", err)
	}
	if got := s.SmoothAngle(); got != 60 {
		t.Fatalf("smooth angle after rejects = %v, want the default 60", got)
	}
	if err := s.SetSmoothAngle(45); err != nil {
		t.Fatalf("set smooth angle 45: %v", err)
	}
	if got := s.SmoothAngle(); got != 45 {
		t.Fatalf("smooth angle = %v, want 45", got)
	}
}

func TestSmoothEdgeAveragesNormals(t *testing.T) {
	s := newScheduler(t)
	if err := s.SetSmoothAngle(180); err != nil {
		t.Fatalf("set smooth angle: %v", err)
	}
	if err := s.RegisterMesh("mesh-fold", foldMesh()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-fold", []int{0, 1, 2, 3}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if _, err := s.Rebuild("mesh-fold", 16); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	normals, err := s.Normals("mesh-fold")
	if err != nil {
		t.Fatalf("normals: %v", err)
	}
	if len(normals) != 4 {
		t.Fatalf("normal count = %d, want 4", len(normals))
	}
	if !vecNear(normals[0], normals[1]) {
		t.Fatalf("shared vertices got different normals %+v and %+v, want one averaged normal", normals[0], normals[1])
	}
	if got := vecLen(normals[0]); math.Abs(got-1) > 1e-6 {
		t.Fatalf("averaged normal length = %v, want 1", got)
	}
	dot := math.Abs(normals[0].Z)
	if dot > 0.99 {
		t.Fatalf("averaged normal %+v equals the first face normal, the smooth edge was not averaged", normals[0])
	}
	if !vecNear(normals[2], impl.Vec3{X: 0, Y: 0, Z: 1}) {
		t.Fatalf("vertex 2 normal = %+v, want the +Z face normal", normals[2])
	}
}

func TestHardEdgeKeepsPerFaceNormals(t *testing.T) {
	s := newScheduler(t)
	if err := s.SetSmoothAngle(30); err != nil {
		t.Fatalf("set smooth angle: %v", err)
	}
	if err := s.RegisterMesh("mesh-fold", foldMesh()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-fold", []int{0, 1, 2, 3}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if _, err := s.Rebuild("mesh-fold", 16); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	normals, err := s.Normals("mesh-fold")
	if err != nil {
		t.Fatalf("normals: %v", err)
	}
	wantPlusZ := impl.Vec3{X: 0, Y: 0, Z: 1}
	if !vecNear(normals[0], wantPlusZ) {
		t.Fatalf("vertex 0 normal = %+v, want %+v: a 90 degree fold over a 30 degree threshold is a hard edge", normals[0], wantPlusZ)
	}
	if !vecNear(normals[1], wantPlusZ) {
		t.Fatalf("vertex 1 normal = %+v, want %+v", normals[1], wantPlusZ)
	}
	if !vecNear(normals[3], impl.Vec3{X: 0, Y: -1, Z: 0}) {
		t.Fatalf("vertex 3 normal = %+v, want the second face normal (0,-1,0)", normals[3])
	}
}

func TestSnapshotRestoreKeepsDirtySet(t *testing.T) {
	s := newScheduler(t)
	if err := s.SetSmoothAngle(45); err != nil {
		t.Fatalf("set smooth angle: %v", err)
	}
	if err := s.RegisterMesh("mesh-fold", foldMesh()); err != nil {
		t.Fatalf("register mesh: %v", err)
	}
	if err := s.MarkDirty("mesh-fold", []int{0, 2}); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	payload, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := newScheduler(t)
	if err := restored.Restore(payload); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.SmoothAngle(); got != 45 {
		t.Fatalf("smooth angle after restore = %v, want 45", got)
	}
	if got := restored.DirtyCount("mesh-fold"); got != 2 {
		t.Fatalf("dirty count after restore = %d, want 2", got)
	}
	processed, err := restored.Rebuild("mesh-fold", 8)
	if err != nil {
		t.Fatalf("rebuild after restore: %v", err)
	}
	if processed < 2 || processed > 8 {
		t.Fatalf("rebuild after restore processed %d vertices, want at least the 2 restored dirty ones", processed)
	}
	if got := restored.DirtyCount("mesh-fold"); got != 0 {
		t.Fatalf("dirty count after the restored rebuild = %d, want 0", got)
	}
	normals, err := restored.Normals("mesh-fold")
	if err != nil {
		t.Fatalf("normals after restore: %v", err)
	}
	if len(normals) != 4 {
		t.Fatalf("normal count after restore = %d, want 4", len(normals))
	}
}
