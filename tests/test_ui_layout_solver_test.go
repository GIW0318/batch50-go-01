package task

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-02"
)

func solverConfig() impl.LayoutConfig {
	return impl.LayoutConfig{MaxNodes: 32, MaxDepth: 8}
}

func fixed(v float64) impl.SizeSpec {
	return impl.SizeSpec{Mode: impl.ModeFixed, Value: v}
}

func percent(v float64) impl.SizeSpec {
	return impl.SizeSpec{Mode: impl.ModePercent, Value: v}
}

func flex(weight int) impl.SizeSpec {
	return impl.SizeSpec{Mode: impl.ModeFlex, Weight: weight}
}

func emptyAxis() impl.SizeSpec {
	return impl.SizeSpec{}
}

// rowTree is a row root with two fixed width children.
func rowTree() impl.NodeSpec {
	return impl.NodeSpec{
		ID:      "root",
		Dir:     impl.DirRow,
		Padding: 10,
		Gap:     5,
		Children: []impl.NodeSpec{
			{ID: "c1", Dir: impl.DirColumn, Width: fixed(60), Height: emptyAxis()},
			{ID: "c2", Dir: impl.DirColumn, Width: fixed(40), Height: emptyAxis()},
		},
	}
}

func TestBuildReturnsPreOrderTree(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	tree, err := s.Build(rowTree())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tree.RootID != "root" {
		t.Fatalf("root id = %q, want root", tree.RootID)
	}
	if len(tree.Nodes) != 3 {
		t.Fatalf("node count = %d, want 3", len(tree.Nodes))
	}
	wantIDs := []string{"root", "c1", "c2"}
	for i, id := range wantIDs {
		if tree.Nodes[i].ID != id {
			t.Fatalf("node[%d] = %q, want %q in pre-order", i, tree.Nodes[i].ID, id)
		}
	}
	if tree.Nodes[0].Depth != 0 || tree.Nodes[0].ParentID != "" {
		t.Fatalf("root depth=%d parent=%q, want 0 and empty", tree.Nodes[0].Depth, tree.Nodes[0].ParentID)
	}
	if tree.Nodes[1].Depth != 1 || tree.Nodes[1].ParentID != "root" {
		t.Fatalf("child depth=%d parent=%q, want 1 and root", tree.Nodes[1].Depth, tree.Nodes[1].ParentID)
	}
	if st := s.Stats(); st.Builds != 1 || st.Solves != 0 {
		t.Fatalf("stats builds=%d solves=%d, want 1 and 0", st.Builds, st.Solves)
	}
}

func TestBuildRejectsBadNodes(t *testing.T) {
	cases := []struct {
		name string
		spec impl.NodeSpec
		want error
	}{
		{
			"blank root id",
			impl.NodeSpec{ID: "  ", Dir: impl.DirRow},
			impl.ErrBadNode,
		},
		{
			"blank child id",
			impl.NodeSpec{ID: "root", Dir: impl.DirRow, Children: []impl.NodeSpec{{ID: ""}}},
			impl.ErrBadNode,
		},
		{
			"unknown direction",
			impl.NodeSpec{ID: "root", Dir: "grid"},
			impl.ErrBadNode,
		},
		{
			"unknown width mode",
			impl.NodeSpec{ID: "root", Dir: impl.DirRow, Width: impl.SizeSpec{Mode: "auto"}},
			impl.ErrBadNode,
		},
		{
			"percent above hundred",
			impl.NodeSpec{ID: "root", Dir: impl.DirRow, Width: percent(120)},
			impl.ErrBadNode,
		},
		{
			"negative padding",
			impl.NodeSpec{ID: "root", Dir: impl.DirRow, Padding: -1},
			impl.ErrBadNode,
		},
		{
			"duplicate child id",
			impl.NodeSpec{ID: "root", Dir: impl.DirRow, Children: []impl.NodeSpec{{ID: "dup"}, {ID: "dup"}}},
			impl.ErrDuplicateNode,
		},
	}
	for _, c := range cases {
		s := impl.NewUiLayoutSolver(solverConfig())
		if _, err := s.Build(c.spec); !errors.Is(err, c.want) {
			t.Fatalf("%s must fail with %v, got %v", c.name, c.want, err)
		}
	}
}

func TestBuildEnforcesNodeAndDepthLimits(t *testing.T) {
	two := impl.NewUiLayoutSolver(impl.LayoutConfig{MaxNodes: 2, MaxDepth: 8})
	wide := impl.NodeSpec{
		ID:  "root",
		Dir: impl.DirRow,
		Children: []impl.NodeSpec{
			{ID: "c1"},
			{ID: "c2"},
		},
	}
	if _, err := two.Build(wide); !errors.Is(err, impl.ErrNodeLimit) {
		t.Fatalf("three nodes under a cap of two must fail with ErrNodeLimit, got %v", err)
	}
	shallow := impl.NewUiLayoutSolver(impl.LayoutConfig{MaxNodes: 8, MaxDepth: 1})
	deep := impl.NodeSpec{
		ID:  "root",
		Dir: impl.DirRow,
		Children: []impl.NodeSpec{
			{ID: "c1", Dir: impl.DirColumn, Children: []impl.NodeSpec{{ID: "gc1"}}},
		},
	}
	if _, err := shallow.Build(deep); !errors.Is(err, impl.ErrDepthLimit) {
		t.Fatalf("a grandchild under MaxDepth 1 must fail with ErrDepthLimit, got %v", err)
	}
}

func TestSolveRootFillsViewport(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(impl.NodeSpec{ID: "root", Dir: impl.DirRow}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	res, err := s.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("solved nodes = %d, want 1", len(res.Nodes))
	}
	want := impl.Rect{X: 0, Y: 0, W: 320, H: 200}
	if res.Nodes[0].Box != want {
		t.Fatalf("root box = %+v, want %+v", res.Nodes[0].Box, want)
	}
	if res.ViewportW != 320 || res.ViewportH != 200 {
		t.Fatalf("result viewport = %vx%v, want 320x200", res.ViewportW, res.ViewportH)
	}
	if res.Solve != 1 {
		t.Fatalf("solve sequence = %d, want 1", res.Solve)
	}
}

func TestSolveFixedRowWithPaddingAndGap(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	res, err := s.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	byID := map[string]impl.Rect{}
	for _, n := range res.Nodes {
		byID[n.ID] = n.Box
	}
	wantC1 := impl.Rect{X: 10, Y: 10, W: 60, H: 180}
	wantC2 := impl.Rect{X: 75, Y: 10, W: 40, H: 180}
	if byID["c1"] != wantC1 {
		t.Fatalf("c1 box = %+v, want %+v", byID["c1"], wantC1)
	}
	if byID["c2"] != wantC2 {
		t.Fatalf("c2 box = %+v, want %+v", byID["c2"], wantC2)
	}
}

func TestSolvePercentChildren(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	spec := impl.NodeSpec{
		ID:      "root",
		Dir:     impl.DirRow,
		Padding: 10,
		Children: []impl.NodeSpec{
			{ID: "left", Width: percent(50)},
			{ID: "right", Width: percent(50)},
		},
	}
	if _, err := s.Build(spec); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(120, 100); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	res, err := s.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	byID := map[string]impl.Rect{}
	for _, n := range res.Nodes {
		byID[n.ID] = n.Box
	}
	if got := byID["left"]; got != (impl.Rect{X: 10, Y: 10, W: 50, H: 80}) {
		t.Fatalf("left box = %+v, want 10/10/50/80", got)
	}
	if got := byID["right"]; got != (impl.Rect{X: 60, Y: 10, W: 50, H: 80}) {
		t.Fatalf("right box = %+v, want 60/10/50/80", got)
	}
}

func TestSolveFlexSplitsRemainder(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	spec := impl.NodeSpec{
		ID:  "root",
		Dir: impl.DirRow,
		Children: []impl.NodeSpec{
			{ID: "f1", Width: flex(1)},
			{ID: "f2", Width: flex(1)},
			{ID: "f3", Width: flex(1)},
		},
	}
	if _, err := s.Build(spec); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 100); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	res, err := s.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	byID := map[string]impl.Rect{}
	for _, n := range res.Nodes {
		byID[n.ID] = n.Box
	}
	total := 0.0
	for _, id := range []string{"f1", "f2", "f3"} {
		box := byID[id]
		if box.W < 106 || box.W > 107 {
			t.Fatalf("%s width = %v, want 106 or 107", id, box.W)
		}
		if math.Trunc(box.W) != box.W {
			t.Fatalf("%s width = %v, want a whole pixel share", id, box.W)
		}
		total += box.W
	}
	if total != 320 {
		t.Fatalf("flex widths add up to %v, want the full 320", total)
	}
	if byID["f2"].X != byID["f1"].W {
		t.Fatalf("f2 starts at %v, want it right after f1 at %v", byID["f2"].X, byID["f1"].W)
	}
	if got := byID["f2"].X + byID["f2"].W; got != byID["f3"].X {
		t.Fatalf("f3 starts at %v, want %v", byID["f3"].X, got)
	}
}

func TestSolveNestedColumnUsesParentContent(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	spec := impl.NodeSpec{
		ID:  "root",
		Dir: impl.DirRow,
		Children: []impl.NodeSpec{
			{
				ID:      "panel",
				Dir:     impl.DirColumn,
				Width:   fixed(100),
				Padding: 5,
				Children: []impl.NodeSpec{
					{ID: "label", Height: fixed(20)},
				},
			},
		},
	}
	if _, err := s.Build(spec); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(200, 100); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	res, err := s.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	byID := map[string]impl.Rect{}
	for _, n := range res.Nodes {
		byID[n.ID] = n.Box
	}
	if got := byID["panel"]; got != (impl.Rect{X: 0, Y: 0, W: 100, H: 100}) {
		t.Fatalf("panel box = %+v, want 0/0/100/100", got)
	}
	if got := byID["label"]; got != (impl.Rect{X: 5, Y: 5, W: 90, H: 20}) {
		t.Fatalf("label box = %+v, want 5/5/90/20", got)
	}
}

func TestHitTestReturnsDeepestChildAndOutside(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if _, err := s.Solve(); err != nil {
		t.Fatalf("solve: %v", err)
	}
	cases := []struct {
		x, y float64
		want string
		ok   bool
	}{
		{20, 20, "c1", true},
		{80, 20, "c2", true},
		{5, 5, "root", true},
		{319, 199, "root", true},
		{400, 20, "", false},
		{20, -3, "", false},
	}
	for _, c := range cases {
		got, ok := s.HitTest(c.x, c.y)
		if ok != c.ok || got != c.want {
			t.Fatalf("hit test at %v,%v = %q,%v want %q,%v", c.x, c.y, got, ok, c.want, c.ok)
		}
	}
	if n, ok := s.Node("c1"); !ok || n.Box.W != 60 || n.ParentID != "root" {
		t.Fatalf("node c1 = %+v ok=%v, want the solved child", n, ok)
	}
	if _, ok := s.Node("nope"); ok {
		t.Fatalf("unknown node must not be readable")
	}
}

func TestInvalidateMarksDirtyAndRejectsUnknown(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if _, err := s.Solve(); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if got := s.Dirty(); len(got) != 0 {
		t.Fatalf("dirty set after a solve = %v, want empty", got)
	}
	if err := s.Invalidate("nope"); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("invalidating an unknown node must fail with ErrUnknownNode, got %v", err)
	}
	if err := s.Invalidate("c2"); err != nil {
		t.Fatalf("invalidate c2: %v", err)
	}
	if err := s.Invalidate("c1"); err != nil {
		t.Fatalf("invalidate c1: %v", err)
	}
	want := []string{"c1", "c2"}
	if got := s.Dirty(); !reflect.DeepEqual(got, want) {
		t.Fatalf("dirty set = %v, want the ascending list %v", got, want)
	}
	if _, err := s.Solve(); err != nil {
		t.Fatalf("solve after invalidate: %v", err)
	}
	if got := s.Dirty(); len(got) != 0 {
		t.Fatalf("dirty set after the reflow = %v, want empty", got)
	}
	if st := s.Stats(); st.Invalidations != 2 {
		t.Fatalf("invalidation counter = %d, want 2", st.Invalidations)
	}
}

func TestSetSizeAppliesAndValidates(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if _, err := s.Solve(); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if err := s.SetSize("nope", fixed(10), fixed(10)); !errors.Is(err, impl.ErrUnknownNode) {
		t.Fatalf("resizing an unknown node must fail with ErrUnknownNode, got %v", err)
	}
	if err := s.SetSize("c1", fixed(-5), fixed(10)); !errors.Is(err, impl.ErrBadSize) {
		t.Fatalf("negative size must fail with ErrBadSize, got %v", err)
	}
	if err := s.SetSize("c1", flex(-1), fixed(10)); !errors.Is(err, impl.ErrBadSize) {
		t.Fatalf("negative flex weight must fail with ErrBadSize, got %v", err)
	}
	if err := s.SetSize("c1", fixed(90), fixed(30)); err != nil {
		t.Fatalf("set size: %v", err)
	}
	res, err := s.Solve()
	if err != nil {
		t.Fatalf("solve after resize: %v", err)
	}
	var box impl.Rect
	for _, n := range res.Nodes {
		if n.ID == "c1" {
			box = n.Box
		}
	}
	if box != (impl.Rect{X: 10, Y: 10, W: 90, H: 30}) {
		t.Fatalf("c1 box after resize = %+v, want 10/10/90/30", box)
	}
}

func TestSetViewportRejectsNonFinite(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(math.NaN(), 100); !errors.Is(err, impl.ErrBadViewport) {
		t.Fatalf("nan width must fail with ErrBadViewport, got %v", err)
	}
	if err := s.SetViewport(100, math.Inf(1)); !errors.Is(err, impl.ErrBadViewport) {
		t.Fatalf("infinite height must fail with ErrBadViewport, got %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("a finite viewport must be accepted, got %v", err)
	}
}

func TestSolveAfterViewportChangeReflows(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	spec := impl.NodeSpec{
		ID:      "root",
		Dir:     impl.DirRow,
		Padding: 10,
		Children: []impl.NodeSpec{
			{ID: "half", Width: percent(50)},
		},
	}
	if _, err := s.Build(spec); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(120, 100); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	first, err := s.Solve()
	if err != nil {
		t.Fatalf("first solve: %v", err)
	}
	if got := first.Nodes[0].Box.W; got != 120 {
		t.Fatalf("root width = %v, want 120", got)
	}
	if err := s.SetViewport(420, 300); err != nil {
		t.Fatalf("set wider viewport: %v", err)
	}
	second, err := s.Solve()
	if err != nil {
		t.Fatalf("second solve: %v", err)
	}
	if got := second.Nodes[0].Box; got != (impl.Rect{X: 0, Y: 0, W: 420, H: 300}) {
		t.Fatalf("root box after the resize = %+v, want 0/0/420/300", got)
	}
	if got := second.Nodes[1].Box.W; got != 200 {
		t.Fatalf("half width after the resize = %v, want 200", got)
	}
	if st := s.Stats(); st.Solves != 2 {
		t.Fatalf("solve counter = %d, want 2", st.Solves)
	}
}

func TestSnapshotRestoreKeepsLayout(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if _, err := s.Solve(); err != nil {
		t.Fatalf("solve: %v", err)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewUiLayoutSolver(solverConfig())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	node, ok := restored.Node("c1")
	if !ok {
		t.Fatalf("restored solver lost the layout tree")
	}
	if node.Box != (impl.Rect{X: 10, Y: 10, W: 60, H: 180}) {
		t.Fatalf("restored c1 box = %+v, want 10/10/60/180", node.Box)
	}
	res, err := restored.Solve()
	if err != nil {
		t.Fatalf("solve after restore: %v", err)
	}
	if res.Nodes[0].Box.W != 320 {
		t.Fatalf("restored viewport width = %v, want 320", res.Nodes[0].Box.W)
	}
	if err := restored.Restore([]byte("not json at all")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := restored.Node("c1"); !ok {
		t.Fatalf("a rejected restore must not drop the live tree")
	}
}

func TestConcurrentSolveSerialized(t *testing.T) {
	s := impl.NewUiLayoutSolver(solverConfig())
	if _, err := s.Build(rowTree()); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := s.SetViewport(320, 200); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	var wg sync.WaitGroup
	results := make([]impl.LayoutResult, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.Solve()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent solve %d: %v", i, err)
		}
	}
	for i, res := range results {
		if len(res.Nodes) != 3 {
			t.Fatalf("solve %d returned %d nodes, want 3", i, len(res.Nodes))
		}
		if res.Nodes[0].Box != (impl.Rect{X: 0, Y: 0, W: 320, H: 200}) {
			t.Fatalf("solve %d root box = %+v, want the viewport", i, res.Nodes[0].Box)
		}
	}
	if st := s.Stats(); st.Solves != 8 {
		t.Fatalf("solve counter = %d, want 8", st.Solves)
	}
}
