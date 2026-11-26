package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-b-17"
)

func screenBounds() impl.Rect {
	return impl.Rect{X: 0, Y: 0, W: 1920, H: 1080}
}

func newStack(maxDepth, featherBudget int) *impl.MaskStack {
	return impl.NewMaskStack(screenBounds(), maxDepth, featherBudget)
}

func box(x, y, w, h float64) impl.Rect {
	return impl.Rect{X: x, Y: y, W: w, H: h}
}

func sameRect(got, want impl.Rect) bool {
	return got.X == want.X && got.Y == want.Y && got.W == want.W && got.H == want.H
}

func TestNewStackStartsAtBounds(t *testing.T) {
	s := newStack(8, 64)
	if got := s.Depth(); got != 0 {
		t.Fatalf("depth of a fresh stack = %d, want 0", got)
	}
	if got := s.Clip(); !sameRect(got, screenBounds()) {
		t.Fatalf("fresh clip = %+v, want the base bounds %+v", got, screenBounds())
	}
	if got := s.FeatherUsed(); got != 0 {
		t.Fatalf("fresh feather usage = %d, want 0", got)
	}
	if _, ok := s.MaskAt(0); ok {
		t.Fatalf("fresh stack must not report a mask at depth 0")
	}
}

func TestPushRectIntersectsCurrentClip(t *testing.T) {
	s := newStack(8, 64)
	outer, err := s.PushRect(box(100, 100, 500, 400), 0)
	if err != nil {
		t.Fatalf("push outer rect: %v", err)
	}
	if outer == "" {
		t.Fatalf("push must return a non empty token")
	}
	if got := s.Clip(); !sameRect(got, box(100, 100, 500, 400)) {
		t.Fatalf("clip after outer rect = %+v, want {100 100 500 400}", got)
	}
	inner, err := s.PushRect(box(200, 150, 100, 100), 0)
	if err != nil {
		t.Fatalf("push inner rect: %v", err)
	}
	if inner == outer {
		t.Fatalf("two entries must not share the token %q", inner)
	}
	if got := s.Clip(); !sameRect(got, box(200, 150, 100, 100)) {
		t.Fatalf("clip after inner rect = %+v, want the intersected {200 150 100 100}", got)
	}
	stats := s.Stats()
	if stats.Depth != 2 || stats.Pushes != 2 || stats.Pops != 0 {
		t.Fatalf("stats after two pushes = %+v, want depth 2 pushes 2 pops 0", stats)
	}
}

func TestPushRectRejectsBadRect(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushRect(box(10, 10, 0, 50), 0); !errors.Is(err, impl.ErrBadRect) {
		t.Fatalf("zero width rect = %v, want ErrBadRect", err)
	}
	if _, err := s.PushRect(box(10, 10, 50, -1), 0); !errors.Is(err, impl.ErrBadRect) {
		t.Fatalf("negative height rect = %v, want ErrBadRect", err)
	}
	if _, err := s.PushRect(box(10, 10, 50, 50), -0.5); !errors.Is(err, impl.ErrBadSoftness) {
		t.Fatalf("negative softness = %v, want ErrBadSoftness", err)
	}
	if got := s.Depth(); got != 0 {
		t.Fatalf("rejected pushes changed the depth to %d, want 0", got)
	}
	if got := s.Clip(); !sameRect(got, screenBounds()) {
		t.Fatalf("rejected pushes changed the clip to %+v", got)
	}
}

func TestPushRejectsBeyondDepthLimit(t *testing.T) {
	s := newStack(2, 64)
	if _, err := s.PushRect(box(0, 0, 1000, 800), 0); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "m-1", Rect: box(100, 100, 400, 300)}); err != nil {
		t.Fatalf("second push: %v", err)
	}
	if _, err := s.PushRect(box(0, 0, 10, 10), 0); !errors.Is(err, impl.ErrDepthLimit) {
		t.Fatalf("third push over a cap of two = %v, want ErrDepthLimit", err)
	}
	if got := s.Depth(); got != 2 {
		t.Fatalf("depth after a rejected push = %d, want 2", got)
	}
	if got := s.Clip(); !sameRect(got, box(100, 100, 400, 300)) {
		t.Fatalf("clip after a rejected push = %+v, want the previous intersection", got)
	}
}

func TestPopRestoresPreviousClip(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushRect(box(100, 100, 500, 400), 0); err != nil {
		t.Fatalf("push outer rect: %v", err)
	}
	top, err := s.PushRect(box(200, 150, 100, 100), 0)
	if err != nil {
		t.Fatalf("push inner rect: %v", err)
	}
	if err := s.Pop(top); err != nil {
		t.Fatalf("pop top entry: %v", err)
	}
	if got := s.Depth(); got != 1 {
		t.Fatalf("depth after pop = %d, want 1", got)
	}
	if got := s.Clip(); !sameRect(got, box(100, 100, 500, 400)) {
		t.Fatalf("clip after pop = %+v, want the outer rect {100 100 500 400}", got)
	}
	if got := s.Stats().Pops; got != 1 {
		t.Fatalf("pop counter = %d, want 1", got)
	}
}

func TestPopRequiresTopToken(t *testing.T) {
	s := newStack(8, 64)
	bottom, err := s.PushRect(box(100, 100, 500, 400), 0)
	if err != nil {
		t.Fatalf("push bottom rect: %v", err)
	}
	top, err := s.PushRect(box(200, 150, 100, 100), 0)
	if err != nil {
		t.Fatalf("push top rect: %v", err)
	}
	if err := s.Pop(bottom); !errors.Is(err, impl.ErrTokenMismatch) {
		t.Fatalf("pop with a non top token = %v, want ErrTokenMismatch", err)
	}
	if got := s.Depth(); got != 2 {
		t.Fatalf("depth after a rejected pop = %d, want 2", got)
	}
	if got := s.Clip(); !sameRect(got, box(200, 150, 100, 100)) {
		t.Fatalf("clip after a rejected pop = %+v, want the top intersection", got)
	}
	if err := s.Pop(top); err != nil {
		t.Fatalf("pop the real top token: %v", err)
	}
	if err := s.Pop(bottom); err != nil {
		t.Fatalf("pop the bottom token once it is on top: %v", err)
	}
	if got := s.Depth(); got != 0 {
		t.Fatalf("depth after popping everything = %d, want 0", got)
	}
}

func TestPopEmptyStackReturnsError(t *testing.T) {
	s := newStack(8, 64)
	if err := s.Pop("token-1"); !errors.Is(err, impl.ErrStackEmpty) {
		t.Fatalf("pop on an empty stack = %v, want ErrStackEmpty", err)
	}
	if got := s.Depth(); got != 0 {
		t.Fatalf("depth after the rejected pop = %d, want 0", got)
	}
}

func TestMaskAtReturnsOutermostFirst(t *testing.T) {
	s := newStack(8, 64)
	outer := impl.MaskRef{ID: "mask-outer", Rect: box(0, 0, 1600, 900), Feather: 0}
	inner := impl.MaskRef{ID: "mask-inner", Rect: box(100, 100, 800, 600), Feather: 0}
	if _, err := s.PushMask(outer); err != nil {
		t.Fatalf("push outer mask: %v", err)
	}
	if _, err := s.PushMask(inner); err != nil {
		t.Fatalf("push inner mask: %v", err)
	}
	got, ok := s.MaskAt(0)
	if !ok || got.ID != "mask-outer" {
		t.Fatalf("mask at depth 0 = %+v ok=%v, want mask-outer", got, ok)
	}
	got, ok = s.MaskAt(1)
	if !ok || got.ID != "mask-inner" {
		t.Fatalf("mask at depth 1 = %+v ok=%v, want mask-inner", got, ok)
	}
	if _, ok := s.MaskAt(2); ok {
		t.Fatalf("mask at depth 2 must not be reported")
	}
	if _, ok := s.MaskAt(-1); ok {
		t.Fatalf("negative depth must not be reported")
	}
	if got := s.Stats().Masks; got != 2 {
		t.Fatalf("mask counter = %d, want 2", got)
	}
}

func TestPushMaskRejectsBlankID(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushMask(impl.MaskRef{ID: "  ", Rect: box(0, 0, 100, 100)}); !errors.Is(err, impl.ErrBadMask) {
		t.Fatalf("blank mask id = %v, want ErrBadMask", err)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "mask-1", Rect: box(0, 0, 0, 100)}); !errors.Is(err, impl.ErrBadMask) {
		t.Fatalf("degenerate mask rect = %v, want ErrBadMask", err)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "mask-2", Rect: box(0, 0, 100, 100), Feather: -2}); !errors.Is(err, impl.ErrBadMask) {
		t.Fatalf("negative mask feather = %v, want ErrBadMask", err)
	}
	if got := s.Depth(); got != 0 {
		t.Fatalf("rejected masks changed the depth to %d, want 0", got)
	}
}

func TestMaskContributesClipIntersection(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushRect(box(0, 0, 800, 600), 0); err != nil {
		t.Fatalf("push rect: %v", err)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "mask-1", Rect: box(100, 50, 400, 300)}); err != nil {
		t.Fatalf("push mask: %v", err)
	}
	if got := s.Clip(); !sameRect(got, box(100, 50, 400, 300)) {
		t.Fatalf("clip after the mask = %+v, want the intersected mask rect {100 50 400 300}", got)
	}
}

func TestFeatherCostAccumulates(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushRect(box(0, 0, 500, 500), 3.5); err != nil {
		t.Fatalf("push soft rect: %v", err)
	}
	if got := s.FeatherUsed(); got != 4 {
		t.Fatalf("feather after softness 3.5 = %d, want the ceiling 4", got)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "mask-1", Rect: box(0, 0, 400, 400), Feather: 2}); err != nil {
		t.Fatalf("push soft mask: %v", err)
	}
	if got := s.FeatherUsed(); got != 6 {
		t.Fatalf("feather after adding 2 = %d, want 6", got)
	}
	if got := s.Stats().FeatherUsed; got != 6 {
		t.Fatalf("stats feather usage = %d, want 6", got)
	}
}

func TestFeatherBudgetRejectsWithoutStateChange(t *testing.T) {
	s := newStack(8, 5)
	if _, err := s.PushRect(box(0, 0, 500, 500), 3); err != nil {
		t.Fatalf("push within budget: %v", err)
	}
	if _, err := s.PushRect(box(0, 0, 400, 400), 3); !errors.Is(err, impl.ErrFeatherBudget) {
		t.Fatalf("push over the feather budget = %v, want ErrFeatherBudget", err)
	}
	if got := s.Depth(); got != 1 {
		t.Fatalf("depth after a rejected soft push = %d, want 1", got)
	}
	if got := s.FeatherUsed(); got != 3 {
		t.Fatalf("feather after a rejected soft push = %d, want 3", got)
	}
	if got := s.Clip(); !sameRect(got, box(0, 0, 500, 500)) {
		t.Fatalf("clip after a rejected soft push = %+v, want the first rect", got)
	}
}

func TestZeroSoftnessCostsNothing(t *testing.T) {
	s := newStack(8, 5)
	if _, err := s.PushRect(box(0, 0, 500, 500), 0); err != nil {
		t.Fatalf("push hard rect: %v", err)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "mask-hard", Rect: box(0, 0, 500, 500)}); err != nil {
		t.Fatalf("push hard mask: %v", err)
	}
	if got := s.FeatherUsed(); got != 0 {
		t.Fatalf("feather for hard edges = %d, want 0", got)
	}
	if got := s.Depth(); got != 2 {
		t.Fatalf("depth = %d, want 2", got)
	}
}

func TestResetClearsStackAndFeather(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushRect(box(10, 10, 500, 400), 2); err != nil {
		t.Fatalf("push rect: %v", err)
	}
	if _, err := s.PushMask(impl.MaskRef{ID: "mask-1", Rect: box(20, 20, 300, 200), Feather: 3}); err != nil {
		t.Fatalf("push mask: %v", err)
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := s.Depth(); got != 0 {
		t.Fatalf("depth after reset = %d, want 0", got)
	}
	if got := s.Clip(); !sameRect(got, screenBounds()) {
		t.Fatalf("clip after reset = %+v, want the base bounds", got)
	}
	if got := s.FeatherUsed(); got != 0 {
		t.Fatalf("feather after reset = %d, want 0", got)
	}
	if _, ok := s.MaskAt(0); ok {
		t.Fatalf("mask after reset must not be reported")
	}
}

func TestSnapshotRestoreKeepsStack(t *testing.T) {
	s := newStack(8, 64)
	if _, err := s.PushRect(box(0, 0, 800, 600), 2); err != nil {
		t.Fatalf("push rect: %v", err)
	}
	top, err := s.PushMask(impl.MaskRef{ID: "mask-1", Rect: box(100, 50, 400, 300), Feather: 4})
	if err != nil {
		t.Fatalf("push mask: %v", err)
	}
	payload, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := newStack(8, 64)
	if err := restored.Restore(payload); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Depth(); got != 2 {
		t.Fatalf("depth after restore = %d, want 2", got)
	}
	if got := restored.Clip(); !sameRect(got, box(100, 50, 400, 300)) {
		t.Fatalf("clip after restore = %+v, want the stored intersection", got)
	}
	mask, ok := restored.MaskAt(1)
	if !ok || mask.ID != "mask-1" {
		t.Fatalf("mask after restore = %+v ok=%v, want mask-1", mask, ok)
	}
	if got := restored.FeatherUsed(); got != 6 {
		t.Fatalf("feather after restore = %d, want 6", got)
	}
	if err := restored.Pop(top); err != nil {
		t.Fatalf("pop the restored top token: %v", err)
	}
	if got := restored.Depth(); got != 1 {
		t.Fatalf("depth after popping the restored token = %d, want 1", got)
	}
}
