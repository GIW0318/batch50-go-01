package task

import (
	"errors"
	"math"
	"testing"

	impl "gsb/batch50-go-game-b-13"
)

func splitConfig() impl.SplitConfig {
	return impl.SplitConfig{
		MaxSlots:      4,
		MinCellW:      320,
		MinCellH:      180,
		TotalBudgetUs: 20000,
		MinBudgetUs:   2000,
	}
}

func newAllocator(t *testing.T, cfg impl.SplitConfig, w, h int) *impl.SplitViewportAllocator {
	t.Helper()
	a := impl.NewSplitViewportAllocator(cfg)
	if err := a.SetWindow(w, h); err != nil {
		t.Fatalf("set window %dx%d: %v", w, h, err)
	}
	plan, ok := a.Pending()
	if !ok {
		t.Fatalf("set window must queue a layout version")
	}
	if err := a.Commit(plan); err != nil {
		t.Fatalf("commit window layout: %v", err)
	}
	return a
}

func joinAndCommit(t *testing.T, a *impl.SplitViewportAllocator, slot string, aspect float64) impl.LayoutPlan {
	t.Helper()
	plan, err := a.Join(slot, aspect)
	if err != nil {
		t.Fatalf("join %q: %v", slot, err)
	}
	if err := a.Commit(plan); err != nil {
		t.Fatalf("commit join %q: %v", slot, err)
	}
	return plan
}

func cellByName(t *testing.T, plan impl.LayoutPlan, viewport string) impl.ViewportCell {
	t.Helper()
	for _, cell := range plan.Cells {
		if cell.Viewport == viewport {
			return cell
		}
	}
	t.Fatalf("viewport %q missing from plan %+v", viewport, plan.Cells)
	return impl.ViewportCell{}
}

func TestJoinRejectsBadSlotAndAspect(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	if _, err := a.Join("   ", 16.0/9.0); !errors.Is(err, impl.ErrBadSlot) {
		t.Fatalf("blank slot must fail with ErrBadSlot, got %v", err)
	}
	if _, err := a.Join("p1", 0); !errors.Is(err, impl.ErrBadAspect) {
		t.Fatalf("zero aspect must fail with ErrBadAspect, got %v", err)
	}
	if _, err := a.Join("p1", math.NaN()); !errors.Is(err, impl.ErrBadAspect) {
		t.Fatalf("NaN aspect must fail with ErrBadAspect, got %v", err)
	}
	if _, ok := a.Pending(); ok {
		t.Fatalf("rejected joins must not queue a layout version")
	}
	joinAndCommit(t, a, "p1", 16.0/9.0)
	if _, err := a.Join("p1", 16.0/9.0); !errors.Is(err, impl.ErrSlotExists) {
		t.Fatalf("duplicate slot must fail with ErrSlotExists, got %v", err)
	}
	slots := a.Slots()
	if len(slots) != 1 || slots[0] != "p1" {
		t.Fatalf("slots = %v, want [p1]", slots)
	}
}

func TestJoinRespectsSlotLimit(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	for _, slot := range []string{"p1", "p2", "p3", "p4"} {
		joinAndCommit(t, a, slot, 16.0/9.0)
	}
	if _, err := a.Join("p5", 16.0/9.0); !errors.Is(err, impl.ErrSlotLimit) {
		t.Fatalf("fifth slot must fail with ErrSlotLimit, got %v", err)
	}
	if got := len(a.Slots()); got != 4 {
		t.Fatalf("slot count = %d, want the cap 4", got)
	}
}

func TestSinglePlayerFillsWindow(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	before := a.Layout()
	if before.Version != 2 {
		t.Fatalf("active version after the window commit = %d, want 2", before.Version)
	}
	if len(before.Cells) != 0 {
		t.Fatalf("layout before the join has %d cells, want none", len(before.Cells))
	}
	plan, err := a.Join("p1", 16.0/9.0)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if plan.Version != 3 {
		t.Fatalf("queued version = %d, want 3", plan.Version)
	}
	if got := a.Layout().Version; got != 2 {
		t.Fatalf("active version before the commit = %d, want the old 2", got)
	}
	if err := a.Commit(plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	layout := a.Layout()
	if layout.Version != 3 || layout.WindowW != 1280 || layout.WindowH != 720 {
		t.Fatalf("layout = %+v, want version 3 on a 1280x720 window", layout)
	}
	if len(layout.Cells) != 1 {
		t.Fatalf("cell count = %d, want 1", len(layout.Cells))
	}
	cell := layout.Cells[0]
	if cell.Viewport != "p1" || cell.X != 0 || cell.Y != 0 || cell.W != 1280 || cell.H != 720 {
		t.Fatalf("cell = %+v, want the full window", cell)
	}
	if cell.ContentX != 0 || cell.ContentY != 0 || cell.ContentW != 1280 || cell.ContentH != 720 {
		t.Fatalf("content = %+v, want it to fill the cell", cell)
	}
}

func TestTwoPlayerSplitColumns(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	joinAndCommit(t, a, "p2", 16.0/9.0)
	layout := a.Layout()
	if len(layout.Cells) != 2 {
		t.Fatalf("cell count = %d, want 2", len(layout.Cells))
	}
	left := layout.Cells[0]
	if left.Viewport != "p1" || left.X != 0 || left.Y != 0 || left.W != 640 || left.H != 720 {
		t.Fatalf("left cell = %+v, want 0,0 640x720", left)
	}
	if left.ContentX != 0 || left.ContentY != 180 || left.ContentW != 640 || left.ContentH != 360 {
		t.Fatalf("left content = %+v, want 640x360 letterboxed at y 180", left)
	}
	right := layout.Cells[1]
	if right.Viewport != "p2" || right.X != 640 || right.Y != 0 || right.W != 640 || right.H != 720 {
		t.Fatalf("right cell = %+v, want 640,0 640x720", right)
	}
	if right.ContentX != 640 || right.ContentY != 180 || right.ContentW != 640 || right.ContentH != 360 {
		t.Fatalf("right content = %+v, want 640x360 letterboxed at y 180", right)
	}
}

func TestThreePlayerLayout(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 4.0/3.0)
	joinAndCommit(t, a, "p2", 4.0/3.0)
	joinAndCommit(t, a, "p3", 4.0/3.0)
	layout := a.Layout()
	if len(layout.Cells) != 3 {
		t.Fatalf("cell count = %d, want 3", len(layout.Cells))
	}
	first := layout.Cells[0]
	if first.X != 0 || first.Y != 0 || first.W != 640 || first.H != 720 {
		t.Fatalf("first cell = %+v, want the left half as one tall cell", first)
	}
	if first.ContentX != 0 || first.ContentY != 120 || first.ContentW != 640 || first.ContentH != 480 {
		t.Fatalf("first content = %+v, want 640x480 centred at y 120", first)
	}
	second := layout.Cells[1]
	if second.X != 640 || second.Y != 0 || second.W != 640 || second.H != 360 {
		t.Fatalf("second cell = %+v, want the right top quarter", second)
	}
	if second.ContentX != 720 || second.ContentY != 0 || second.ContentW != 480 || second.ContentH != 360 {
		t.Fatalf("second content = %+v, want 480x360 centred at x 720", second)
	}
	third := layout.Cells[2]
	if third.X != 640 || third.Y != 360 || third.W != 640 || third.H != 360 {
		t.Fatalf("third cell = %+v, want the right bottom quarter", third)
	}
	if third.ContentX != 720 || third.ContentY != 360 || third.ContentW != 480 || third.ContentH != 360 {
		t.Fatalf("third content = %+v, want 480x360 at y 360", third)
	}
}

func TestFourPlayerQuadrants(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	for _, slot := range []string{"p1", "p2", "p3", "p4"} {
		joinAndCommit(t, a, slot, 16.0/9.0)
	}
	layout := a.Layout()
	if len(layout.Cells) != 4 {
		t.Fatalf("cell count = %d, want 4", len(layout.Cells))
	}
	want := [][4]int{
		{0, 0, 640, 360},
		{640, 0, 640, 360},
		{0, 360, 640, 360},
		{640, 360, 640, 360},
	}
	for i, box := range want {
		cell := layout.Cells[i]
		if cell.X != box[0] || cell.Y != box[1] || cell.W != box[2] || cell.H != box[3] {
			t.Fatalf("cell %d = %+v, want %v", i, cell, box)
		}
		if cell.ContentX != box[0] || cell.ContentY != box[1] || cell.ContentW != 640 || cell.ContentH != 360 {
			t.Fatalf("cell %d content = %+v, want it to fill the quadrant", i, cell)
		}
	}
	if layout.Cells[0].Viewport != "p1" || layout.Cells[3].Viewport != "p4" {
		t.Fatalf("cell order = %v, want the join order", layout.Cells)
	}
}

func TestLetterboxAddsHorizontalBars(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1000, 600)
	joinAndCommit(t, a, "p1", 4.0/3.0)
	layout := a.Layout()
	if len(layout.Cells) != 1 {
		t.Fatalf("cell count = %d, want 1", len(layout.Cells))
	}
	cell := layout.Cells[0]
	if cell.W != 1000 || cell.H != 600 {
		t.Fatalf("cell = %+v, want 1000x600", cell)
	}
	if cell.ContentX != 100 || cell.ContentY != 0 || cell.ContentW != 800 || cell.ContentH != 600 {
		t.Fatalf("content = %+v, want 800x600 centred at x 100", cell)
	}
}

func TestJoinRejectsLayoutBelowMinCell(t *testing.T) {
	a := newAllocator(t, splitConfig(), 500, 300)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	if _, err := a.Join("p2", 16.0/9.0); !errors.Is(err, impl.ErrLayoutTooSmall) {
		t.Fatalf("a two way split below the minimum cell width must fail with ErrLayoutTooSmall, got %v", err)
	}
	slots := a.Slots()
	if len(slots) != 1 || slots[0] != "p1" {
		t.Fatalf("slots after the rejected join = %v, want [p1]", slots)
	}
	if _, ok := a.Pending(); ok {
		t.Fatalf("the rejected join must not queue a layout version")
	}
}

func TestLeaveReturnsPlanAndDropsSlot(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	joinAndCommit(t, a, "p2", 16.0/9.0)
	joinAndCommit(t, a, "p3", 16.0/9.0)
	plan, err := a.Leave("p2")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if len(plan.Cells) != 2 {
		t.Fatalf("queued plan has %d cells, want 2", len(plan.Cells))
	}
	if err := a.Commit(plan); err != nil {
		t.Fatalf("commit leave: %v", err)
	}
	layout := a.Layout()
	if len(layout.Cells) != 2 {
		t.Fatalf("active cells = %d, want 2", len(layout.Cells))
	}
	if layout.Cells[0].Viewport != "p1" || layout.Cells[0].W != 640 {
		t.Fatalf("first cell = %+v, want p1 on the left half", layout.Cells[0])
	}
	if layout.Cells[1].Viewport != "p3" || layout.Cells[1].X != 640 {
		t.Fatalf("second cell = %+v, want p3 on the right half", layout.Cells[1])
	}
	if _, err := a.Leave("ghost"); !errors.Is(err, impl.ErrUnknownSlot) {
		t.Fatalf("leaving an unknown slot must fail with ErrUnknownSlot, got %v", err)
	}
	if got := len(a.Slots()); got != 2 {
		t.Fatalf("slot count = %d, want 2", got)
	}
}

func TestSetAspectValidatesAndRebuilds(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	active := a.Layout().Version
	if err := a.SetAspect("ghost", 4.0/3.0); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
	if err := a.SetAspect("p1", -2); !errors.Is(err, impl.ErrBadAspect) {
		t.Fatalf("negative aspect must fail with ErrBadAspect, got %v", err)
	}
	if err := a.SetAspect("p1", 4.0/3.0); err != nil {
		t.Fatalf("set aspect: %v", err)
	}
	plan, ok := a.Pending()
	if !ok {
		t.Fatalf("a valid aspect change must queue a layout version")
	}
	if plan.Version <= active {
		t.Fatalf("queued version %d must advance past the active %d", plan.Version, active)
	}
	if err := a.Commit(plan); err != nil {
		t.Fatalf("commit aspect: %v", err)
	}
	cell := a.Layout().Cells[0]
	if cell.ContentX != 160 || cell.ContentY != 0 || cell.ContentW != 960 || cell.ContentH != 720 {
		t.Fatalf("content = %+v, want 960x720 centred at x 160", cell)
	}
}

func TestSetBudgetValidates(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	joinAndCommit(t, a, "p2", 16.0/9.0)
	budgets := a.Budgets()
	if budgets["p1"] != 2000 || budgets["p2"] != 2000 {
		t.Fatalf("default budgets = %v, want the minimum 2000 for both", budgets)
	}
	if err := a.SetBudget("ghost", 4000); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
	if err := a.SetBudget("p1", 0); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("zero budget must fail with ErrBadBudget, got %v", err)
	}
	if err := a.SetBudget("p1", 1999); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("budget below the minimum must fail with ErrBadBudget, got %v", err)
	}
	if err := a.SetBudget("p1", 9000); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if got := a.Budgets()["p1"]; got != 9000 {
		t.Fatalf("p1 budget = %d, want 9000", got)
	}
	if err := a.SetBudget("p2", 18000); !errors.Is(err, impl.ErrBudgetExceeded) {
		t.Fatalf("budget above the total must fail with ErrBudgetExceeded, got %v", err)
	}
	if got := a.Budgets()["p2"]; got != 2000 {
		t.Fatalf("p2 budget = %d after the rejected change, want 2000", got)
	}
}

func TestCommitVersionRules(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	first := joinAndCommit(t, a, "p1", 16.0/9.0)
	secondPlan, err := a.Join("p2", 16.0/9.0)
	if err != nil {
		t.Fatalf("join p2: %v", err)
	}
	if err := a.Commit(first); !errors.Is(err, impl.ErrStalePlan) {
		t.Fatalf("committing the previous version must fail with ErrStalePlan, got %v", err)
	}
	if got := a.Layout().Version; got != first.Version {
		t.Fatalf("stale commit moved the active version to %d, want %d", got, first.Version)
	}
	if err := a.Commit(secondPlan); err != nil {
		t.Fatalf("commit current plan: %v", err)
	}
	if err := a.Commit(secondPlan); err != nil {
		t.Fatalf("committing the active version again must be idempotent, got %v", err)
	}
	if got := a.Layout().Version; got != secondPlan.Version {
		t.Fatalf("active version = %d, want %d", got, secondPlan.Version)
	}
}

func TestSetWindowValidates(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	joinAndCommit(t, a, "p2", 16.0/9.0)
	if err := a.SetWindow(0, 720); !errors.Is(err, impl.ErrBadWindow) {
		t.Fatalf("zero width must fail with ErrBadWindow, got %v", err)
	}
	if err := a.SetWindow(-5, 720); !errors.Is(err, impl.ErrBadWindow) {
		t.Fatalf("negative width must fail with ErrBadWindow, got %v", err)
	}
	if err := a.SetWindow(400, 200); !errors.Is(err, impl.ErrLayoutTooSmall) {
		t.Fatalf("a window too small for the split must fail with ErrLayoutTooSmall, got %v", err)
	}
	if w, h := a.Window(); w != 1280 || h != 720 {
		t.Fatalf("window = %dx%d after rejected changes, want 1280x720", w, h)
	}
	if err := a.SetWindow(1600, 900); err != nil {
		t.Fatalf("set window: %v", err)
	}
	if w, h := a.Window(); w != 1600 || h != 900 {
		t.Fatalf("window = %dx%d, want 1600x900", w, h)
	}
	plan, ok := a.Pending()
	if !ok {
		t.Fatalf("a window change must queue a layout version")
	}
	if err := a.Commit(plan); err != nil {
		t.Fatalf("commit window change: %v", err)
	}
	if cell := a.Layout().Cells[0]; cell.W != 800 || cell.H != 900 {
		t.Fatalf("first cell after the resize = %+v, want 800x900", cell)
	}
}

func TestSnapshotRestoreKeepsMembers(t *testing.T) {
	cfg := splitConfig()
	a := newAllocator(t, cfg, 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	joinAndCommit(t, a, "p2", 4.0/3.0)
	if err := a.SetBudget("p1", 5000); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	version := a.Layout().Version
	data, err := a.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewSplitViewportAllocator(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	slots := restored.Slots()
	if len(slots) != 2 || slots[0] != "p1" || slots[1] != "p2" {
		t.Fatalf("restored slots = %v, want [p1 p2]", slots)
	}
	if w, h := restored.Window(); w != 1280 || h != 720 {
		t.Fatalf("restored window = %dx%d, want 1280x720", w, h)
	}
	layout := restored.Layout()
	if layout.Version != version {
		t.Fatalf("restored version = %d, want %d", layout.Version, version)
	}
	if len(layout.Cells) != 2 {
		t.Fatalf("restored cells = %d, want 2", len(layout.Cells))
	}
	second := cellByName(t, layout, "p2")
	if second.X != 640 || second.ContentX != 640 || second.ContentY != 120 || second.ContentW != 640 || second.ContentH != 480 {
		t.Fatalf("restored p2 cell = %+v, want the 4:3 letterbox inside the right half", second)
	}
	if got := restored.Budgets()["p1"]; got != 5000 {
		t.Fatalf("restored p1 budget = %d, want 5000", got)
	}
	plan, err := restored.Leave("p2")
	if err != nil {
		t.Fatalf("leave after restore: %v", err)
	}
	if plan.Version <= version {
		t.Fatalf("leave after restore queued version %d, want it past %d", plan.Version, version)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	a := newAllocator(t, splitConfig(), 1280, 720)
	joinAndCommit(t, a, "p1", 16.0/9.0)
	if err := a.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	slots := a.Slots()
	if len(slots) != 1 || slots[0] != "p1" {
		t.Fatalf("failed restore touched the slots, got %v", slots)
	}
	if w, h := a.Window(); w != 1280 || h != 720 {
		t.Fatalf("failed restore touched the window, got %dx%d", w, h)
	}
}
