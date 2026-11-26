package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-b-18"
)

func newUnwrapper(t *testing.T) *impl.LightmapUnwrapper {
	t.Helper()
	u, err := impl.NewLightmapUnwrapper(8, 1024)
	if err != nil {
		t.Fatalf("new unwrapper: %v", err)
	}
	if u == nil {
		t.Fatalf("new unwrapper returned nil without an error")
	}
	return u
}

// foldMesh is two triangles sharing the edge 0-2 with a 90 degree dihedral.
func foldMesh() impl.Mesh {
	return impl.Mesh{
		ID: "fold",
		Verts: []impl.Vec3{
			{X: 0, Y: 0, Z: 0},
			{X: 1, Y: 0, Z: 0},
			{X: 0, Y: 1, Z: 0},
			{X: 0, Y: 0, Z: 1},
		},
		Tris: []impl.Tri{
			{A: 0, B: 1, C: 2},
			{A: 0, B: 2, C: 3},
		},
	}
}

// looseMesh is four triangles without any shared vertex.
func looseMesh() impl.Mesh {
	m := impl.Mesh{ID: "loose"}
	for i := 0; i < 4; i++ {
		base := float64(i) * 10
		m.Verts = append(m.Verts,
			impl.Vec3{X: base, Y: 0, Z: 0},
			impl.Vec3{X: base + 1, Y: 0, Z: 0},
			impl.Vec3{X: base, Y: 1, Z: 0},
		)
		m.Tris = append(m.Tris, impl.Tri{A: i * 3, B: i*3 + 1, C: i*3 + 2})
	}
	return m
}

func TestNewUnwrapperRejectsBadLimits(t *testing.T) {
	if _, err := impl.NewLightmapUnwrapper(0, 1024); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("zero chart limit = %v, want ErrBadLimit", err)
	}
	if _, err := impl.NewLightmapUnwrapper(4, 0); !errors.Is(err, impl.ErrBadAtlasSize) {
		t.Fatalf("zero atlas size = %v, want ErrBadAtlasSize", err)
	}
	u, err := impl.NewLightmapUnwrapper(4, 2048)
	if err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
	if u == nil {
		t.Fatalf("valid limits produced a nil unwrapper")
	}
}

func TestSetPaddingRejectsNegative(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetPadding(-1); !errors.Is(err, impl.ErrBadPadding) {
		t.Fatalf("negative padding = %v, want ErrBadPadding", err)
	}
	if got := u.Padding(); got != 0 {
		t.Fatalf("padding after a rejected value = %d, want 0", got)
	}
}

func TestSetPaddingReportedByPadding(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetPadding(4); err != nil {
		t.Fatalf("set padding 4: %v", err)
	}
	if got := u.Padding(); got != 4 {
		t.Fatalf("padding = %d, want 4", got)
	}
	if err := u.SetPadding(0); err != nil {
		t.Fatalf("set padding 0: %v", err)
	}
	if got := u.Padding(); got != 0 {
		t.Fatalf("padding = %d, want 0", got)
	}
}

func TestSetChartLimitRejectsNonPositive(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetChartLimit(0); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("zero chart limit = %v, want ErrBadLimit", err)
	}
	if err := u.SetChartLimit(-3); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("negative chart limit = %v, want ErrBadLimit", err)
	}
	if err := u.SetChartLimit(99); !errors.Is(err, impl.ErrBadLimit) {
		t.Fatalf("chart limit above the configured cap = %v, want ErrBadLimit", err)
	}
	if err := u.SetChartLimit(3); err != nil {
		t.Fatalf("chart limit within the cap: %v", err)
	}
}

func TestUnwrapRejectsDegenerateTexels(t *testing.T) {
	u := newUnwrapper(t)
	if _, err := u.Unwrap(foldMesh(), 0); !errors.Is(err, impl.ErrBadDensity) {
		t.Fatalf("zero texels per unit = %v, want ErrBadDensity", err)
	}
	if _, err := u.Unwrap(foldMesh(), -2); !errors.Is(err, impl.ErrBadDensity) {
		t.Fatalf("negative texels per unit = %v, want ErrBadDensity", err)
	}
	if got := len(u.Charts()); got != 0 {
		t.Fatalf("rejected unwraps produced %d charts, want 0", got)
	}
}

func TestUnwrapRejectsEmptyMesh(t *testing.T) {
	u := newUnwrapper(t)
	if _, err := u.Unwrap(impl.Mesh{ID: "blank"}, 4); !errors.Is(err, impl.ErrEmptyMesh) {
		t.Fatalf("mesh without vertices = %v, want ErrEmptyMesh", err)
	}
	vertsOnly := impl.Mesh{ID: "verts", Verts: foldMesh().Verts}
	if _, err := u.Unwrap(vertsOnly, 4); !errors.Is(err, impl.ErrEmptyMesh) {
		t.Fatalf("mesh without triangles = %v, want ErrEmptyMesh", err)
	}
}

func TestUnwrapSplitsChartAtHardEdge(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetSmoothAngle(30); err != nil {
		t.Fatalf("set smooth angle 30: %v", err)
	}
	atlas, err := u.Unwrap(foldMesh(), 4)
	if err != nil {
		t.Fatalf("unwrap fold mesh: %v", err)
	}
	if len(atlas.Charts) != 2 {
		t.Fatalf("a 90 degree fold with a 30 degree threshold produced %d charts, want 2", len(atlas.Charts))
	}
	for _, c := range atlas.Charts {
		if len(c.Tris) != 1 {
			t.Fatalf("chart %q holds %d triangles, want 1", c.ID, len(c.Tris))
		}
	}
	stats := u.Stats()
	if stats.Charts != 2 || stats.Tris != 2 {
		t.Fatalf("stats after the fold = %+v, want 2 charts and 2 triangles", stats)
	}
}

func TestUnwrapKeepsOneChartBelowSmoothAngle(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetSmoothAngle(120); err != nil {
		t.Fatalf("set smooth angle 120: %v", err)
	}
	atlas, err := u.Unwrap(foldMesh(), 4)
	if err != nil {
		t.Fatalf("unwrap fold mesh: %v", err)
	}
	if len(atlas.Charts) != 1 {
		t.Fatalf("a 90 degree fold below a 120 degree threshold produced %d charts, want 1", len(atlas.Charts))
	}
	if got := len(atlas.Charts[0].Tris); got != 2 {
		t.Fatalf("the merged chart holds %d triangles, want 2", got)
	}
	if err := u.SetSmoothAngle(-5); !errors.Is(err, impl.ErrBadSmoothAngle) {
		t.Fatalf("negative smooth angle = %v, want ErrBadSmoothAngle", err)
	}
	if err := u.SetSmoothAngle(181); !errors.Is(err, impl.ErrBadSmoothAngle) {
		t.Fatalf("smooth angle above 180 = %v, want ErrBadSmoothAngle", err)
	}
}

func TestUnwrapEnforcesChartLimit(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetChartLimit(2); err != nil {
		t.Fatalf("set chart limit 2: %v", err)
	}
	if _, err := u.Unwrap(looseMesh(), 4); !errors.Is(err, impl.ErrChartLimit) {
		t.Fatalf("four charts against a limit of two = %v, want ErrChartLimit", err)
	}
	if got := len(u.Charts()); got != 0 {
		t.Fatalf("a rejected unwrap left %d charts behind, want 0", got)
	}
	if err := u.SetChartLimit(4); err != nil {
		t.Fatalf("set chart limit 4: %v", err)
	}
	atlas, err := u.Unwrap(looseMesh(), 4)
	if err != nil {
		t.Fatalf("unwrap within the raised limit: %v", err)
	}
	if len(atlas.Charts) != 4 {
		t.Fatalf("four loose triangles produced %d charts, want 4", len(atlas.Charts))
	}
}

func TestUnwrapAssignsRequestedDensity(t *testing.T) {
	u := newUnwrapper(t)
	if _, err := u.Unwrap(foldMesh(), 8); err != nil {
		t.Fatalf("unwrap fold mesh: %v", err)
	}
	charts := u.Charts()
	if len(charts) != 2 {
		t.Fatalf("chart count = %d, want 2", len(charts))
	}
	for _, c := range charts {
		got, err := u.ChartDensity(c.ID)
		if err != nil {
			t.Fatalf("density of %q: %v", c.ID, err)
		}
		if got != 8 {
			t.Fatalf("chart %q density = %v, want the requested 8 texels per unit", c.ID, got)
		}
	}
	if _, err := u.ChartDensity("chart-missing"); !errors.Is(err, impl.ErrUnknownChart) {
		t.Fatalf("density of an unknown chart = %v, want ErrUnknownChart", err)
	}
	stats := u.Stats()
	if stats.AtlasWidth <= 0 || stats.AtlasHeight <= 0 {
		t.Fatalf("atlas size = %dx%d, want positive dimensions", stats.AtlasWidth, stats.AtlasHeight)
	}
	if stats.AtlasWidth > 1024 || stats.AtlasHeight > 1024 {
		t.Fatalf("atlas size = %dx%d, must stay within the configured 1024", stats.AtlasWidth, stats.AtlasHeight)
	}
}

func TestValidateReportsOverlappingCharts(t *testing.T) {
	u := newUnwrapper(t)
	atlas := impl.Atlas{
		Width:  512,
		Height: 512,
		Charts: []impl.Chart{
			{ID: "chart-a", Rect: impl.Rect{X: 0, Y: 0, W: 100, H: 100}, Density: 8},
			{ID: "chart-b", Rect: impl.Rect{X: 50, Y: 50, W: 100, H: 100}, Density: 8},
		},
	}
	issues := u.Validate(atlas)
	overlaps := 0
	for _, issue := range issues {
		if issue.Kind == impl.IssueOverlap {
			overlaps++
		}
	}
	if overlaps == 0 {
		t.Fatalf("overlapping charts produced no overlap issue, got %+v", issues)
	}
}

func TestValidateReportsOutOfBoundsChart(t *testing.T) {
	u := newUnwrapper(t)
	atlas := impl.Atlas{
		Width:  512,
		Height: 512,
		Charts: []impl.Chart{
			{ID: "chart-a", Rect: impl.Rect{X: 460, Y: 0, W: 100, H: 100}, Density: 8},
		},
	}
	issues := u.Validate(atlas)
	found := false
	for _, issue := range issues {
		if issue.Kind == impl.IssueOutOfBounds {
			found = true
		}
	}
	if !found {
		t.Fatalf("a chart crossing the atlas border produced no out of bounds issue, got %+v", issues)
	}
}

func TestValidateCleanAtlasIsEmpty(t *testing.T) {
	u := newUnwrapper(t)
	atlas := impl.Atlas{
		Width:  512,
		Height: 512,
		Charts: []impl.Chart{
			{ID: "chart-a", Rect: impl.Rect{X: 0, Y: 0, W: 100, H: 100}, Density: 8},
			{ID: "chart-b", Rect: impl.Rect{X: 200, Y: 0, W: 100, H: 100}, Density: 8},
		},
	}
	if issues := u.Validate(atlas); len(issues) != 0 {
		t.Fatalf("a clean atlas produced %+v, want no issues", issues)
	}
}

func TestRebakeKeepsOtherChartOffsets(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetSmoothAngle(30); err != nil {
		t.Fatalf("set smooth angle 30: %v", err)
	}
	if _, err := u.Unwrap(foldMesh(), 4); err != nil {
		t.Fatalf("unwrap fold mesh: %v", err)
	}
	charts := u.Charts()
	if len(charts) != 2 {
		t.Fatalf("chart count = %d, want 2", len(charts))
	}
	firstID := charts[0].ID
	firstOffset := charts[0].Offset
	if err := u.Rebake(charts[1].ID); err != nil {
		t.Fatalf("rebake second chart: %v", err)
	}
	after := u.Charts()
	if len(after) != 2 {
		t.Fatalf("chart count after rebake = %d, want 2", len(after))
	}
	var kept impl.Chart
	for _, c := range after {
		if c.ID == firstID {
			kept = c
		}
	}
	if kept.Offset != firstOffset {
		t.Fatalf("rebaking chart %q moved chart %q from %+v to %+v", charts[1].ID, firstID, firstOffset, kept.Offset)
	}
	if got := u.Stats().Rebakes; got != 1 {
		t.Fatalf("rebake counter = %d, want 1", got)
	}
	if err := u.Rebake("chart-missing"); !errors.Is(err, impl.ErrUnknownChart) {
		t.Fatalf("rebake of an unknown chart = %v, want ErrUnknownChart", err)
	}
}

func TestSnapshotRestoreKeepsAtlas(t *testing.T) {
	u := newUnwrapper(t)
	if err := u.SetPadding(3); err != nil {
		t.Fatalf("set padding: %v", err)
	}
	if _, err := u.Unwrap(foldMesh(), 8); err != nil {
		t.Fatalf("unwrap fold mesh: %v", err)
	}
	payload, err := u.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := newUnwrapper(t)
	if err := restored.Restore(payload); err != nil {
		t.Fatalf("restore: %v", err)
	}
	charts := restored.Charts()
	if len(charts) != 2 {
		t.Fatalf("chart count after restore = %d, want 2", len(charts))
	}
	if got := restored.Padding(); got != 3 {
		t.Fatalf("padding after restore = %d, want 3", got)
	}
	if got := restored.Stats().Charts; got != 2 {
		t.Fatalf("stats chart count after restore = %d, want 2", got)
	}
	if err := restored.Rebake(charts[0].ID); err != nil {
		t.Fatalf("rebake a restored chart: %v", err)
	}
	if err := restored.Restore([]byte("not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload = %v, want ErrBadSnapshot", err)
	}
	if got := len(restored.Charts()); got != 2 {
		t.Fatalf("a rejected restore dropped the atlas to %d charts, want 2", got)
	}
}
