package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-b-05"
)

func stackLimits() impl.Limits {
	return impl.Limits{MaxOverlays: 4, MaxSteps: 8}
}

func stackViewport() impl.Rect {
	return impl.Rect{X: 0, Y: 0, W: 1280, H: 720}
}

func baseCamera() impl.CameraSpec {
	return impl.CameraSpec{
		ID:           "base",
		ClearColor:   true,
		ClearDepth:   true,
		DepthEnabled: true,
	}
}

func overlayCamera(id string, order int) impl.CameraSpec {
	return impl.CameraSpec{
		ID:           id,
		Order:        order,
		ClearColor:   false,
		ClearDepth:   true,
		DepthEnabled: true,
	}
}

func pushBase(t *testing.T, c *impl.CameraStackCompositor) {
	t.Helper()
	if err := c.Push(baseCamera()); err != nil {
		t.Fatalf("push base camera: %v", err)
	}
	if err := c.SetViewport(stackViewport()); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
}

func stepIDs(steps []impl.RenderStep) []string {
	ids := make([]string, 0, len(steps))
	for _, s := range steps {
		ids = append(ids, s.CameraID)
	}
	return ids
}

func findStep(steps []impl.RenderStep, id string) (impl.RenderStep, bool) {
	for _, s := range steps {
		if s.CameraID == id {
			return s, true
		}
	}
	return impl.RenderStep{}, false
}

func TestPushAndComposeBaseOnly(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("step count = %d, want 1", len(steps))
	}
	if steps[0].CameraID != "base" {
		t.Fatalf("first step = %q, want the base camera", steps[0].CameraID)
	}
	if steps[0].Seq != 1 {
		t.Fatalf("sequence = %d, want 1", steps[0].Seq)
	}
	if steps[0].Rect != stackViewport() {
		t.Fatalf("step rect = %+v, want the stack viewport", steps[0].Rect)
	}
	if st := c.Stats(); st.Composes != 1 || st.Registered != 1 {
		t.Fatalf("stats composes=%d registered=%d, want 1 and 1", st.Composes, st.Registered)
	}
}

func TestPushRejectsBadAndDuplicate(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	if err := c.Push(impl.CameraSpec{ID: "   "}); !errors.Is(err, impl.ErrBadCamera) {
		t.Fatalf("blank base id must fail with ErrBadCamera, got %v", err)
	}
	if _, ok := c.Base(); ok {
		t.Fatalf("a rejected base must not be registered")
	}
	if err := c.Push(baseCamera()); err != nil {
		t.Fatalf("push base: %v", err)
	}
	if err := c.Push(overlayCamera("second", 1)); !errors.Is(err, impl.ErrBaseExists) {
		t.Fatalf("a second base must fail with ErrBaseExists, got %v", err)
	}
	base, ok := c.Base()
	if !ok || base.ID != "base" {
		t.Fatalf("base camera = %+v ok=%v, want the first one", base, ok)
	}
}

func TestOverlayRequiresBase(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	if err := c.Overlay(overlayCamera("o1", 10), 10); !errors.Is(err, impl.ErrNoBase) {
		t.Fatalf("an overlay before the base must fail with ErrNoBase, got %v", err)
	}
	if _, err := c.Compose(); !errors.Is(err, impl.ErrNoBase) {
		t.Fatalf("composing without a base must fail with ErrNoBase, got %v", err)
	}
}

func TestOverlayRejectsDuplicateAndLimit(t *testing.T) {
	c := impl.NewCameraStackCompositor(impl.Limits{MaxOverlays: 2, MaxSteps: 8})
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("o1", 10), 10); err != nil {
		t.Fatalf("overlay o1: %v", err)
	}
	if err := c.Overlay(overlayCamera("o2", 20), 20); err != nil {
		t.Fatalf("overlay o2: %v", err)
	}
	if err := c.Overlay(overlayCamera("o3", 30), 30); !errors.Is(err, impl.ErrOverlayLimit) {
		t.Fatalf("a third overlay under a cap of two must fail with ErrOverlayLimit, got %v", err)
	}
	if err := c.Overlay(overlayCamera("o1", 40), 40); !errors.Is(err, impl.ErrDuplicateCamera) {
		t.Fatalf("a duplicate overlay id must fail with ErrDuplicateCamera, got %v", err)
	}
	if err := c.Overlay(overlayCamera("base", 50), 50); !errors.Is(err, impl.ErrDuplicateCamera) {
		t.Fatalf("an overlay reusing the base id must fail with ErrDuplicateCamera, got %v", err)
	}
	if got := len(c.Overlays()); got != 2 {
		t.Fatalf("overlay count = %d, want 2", got)
	}
}

func TestComposeOrdersOverlaysByOrder(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	for _, spec := range []struct {
		id    string
		order int
	}{{"late", 30}, {"early", 10}, {"middle", 20}} {
		if err := c.Overlay(overlayCamera(spec.id, spec.order), spec.order); err != nil {
			t.Fatalf("overlay %s: %v", spec.id, err)
		}
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	want := []string{"base", "early", "middle", "late"}
	if got := stepIDs(steps); len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("steps = %v, want %v", got, want)
			}
		}
	}
	for i, s := range steps {
		if s.Seq != i+1 {
			t.Fatalf("step %d sequence = %d, want %d", i, s.Seq, i+1)
		}
	}
}

func TestComposePlacesUICameraLast(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	ui := impl.CameraSpec{ID: "hud", Order: 0, IsUI: true, DepthEnabled: true}
	if err := c.Overlay(ui, 0); err != nil {
		t.Fatalf("overlay hud: %v", err)
	}
	if err := c.Overlay(overlayCamera("scene", 5), 5); err != nil {
		t.Fatalf("overlay scene: %v", err)
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	want := []string{"base", "scene", "hud"}
	for i, id := range want {
		if steps[i].CameraID != id {
			t.Fatalf("step %d = %q, want %q (UI camera must come last)", i, steps[i].CameraID, id)
		}
	}
	hud, _ := findStep(steps, "hud")
	if hud.DepthEnabled {
		t.Fatalf("the UI camera must not take part in depth")
	}
}

func TestComposeEnforcesStepLimit(t *testing.T) {
	c := impl.NewCameraStackCompositor(impl.Limits{MaxOverlays: 4, MaxSteps: 2})
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("o1", 10), 10); err != nil {
		t.Fatalf("overlay o1: %v", err)
	}
	if err := c.Overlay(overlayCamera("o2", 20), 20); err != nil {
		t.Fatalf("overlay o2: %v", err)
	}
	if _, err := c.Compose(); !errors.Is(err, impl.ErrStepLimit) {
		t.Fatalf("three steps under a cap of two must fail with ErrStepLimit, got %v", err)
	}
	wide := impl.NewCameraStackCompositor(impl.Limits{MaxOverlays: 0, MaxSteps: 0})
	if got := wide.Stats(); got.Composes != 0 {
		t.Fatalf("a fresh compositor must not report composes")
	}
	pushBase(t, wide)
	steps, err := wide.Compose()
	if err != nil {
		t.Fatalf("compose with the default caps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("default capped compose returned %d steps, want 1", len(steps))
	}
}

func TestRemoveRejectsBaseAndUnknown(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("o1", 10), 10); err != nil {
		t.Fatalf("overlay o1: %v", err)
	}
	if err := c.Remove("base"); !errors.Is(err, impl.ErrBaseRequired) {
		t.Fatalf("removing the base camera must fail with ErrBaseRequired, got %v", err)
	}
	if err := c.Remove("nope"); !errors.Is(err, impl.ErrNotFound) {
		t.Fatalf("removing an unknown camera must fail with ErrNotFound, got %v", err)
	}
	if err := c.Remove("o1"); err != nil {
		t.Fatalf("remove o1: %v", err)
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose after the removal: %v", err)
	}
	if len(steps) != 1 || steps[0].CameraID != "base" {
		t.Fatalf("steps after the removal = %+v, want only the base", steps)
	}
	if st := c.Stats(); st.Removed != 1 {
		t.Fatalf("remove counter = %d, want 1", st.Removed)
	}
}

func TestSetViewportValidates(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	if err := c.SetViewport(impl.Rect{X: 0, Y: 0, W: 0, H: 720}); !errors.Is(err, impl.ErrBadViewport) {
		t.Fatalf("a zero width viewport must fail with ErrBadViewport, got %v", err)
	}
	if err := c.SetViewport(impl.Rect{X: 0, Y: 0, W: 1280, H: -1}); !errors.Is(err, impl.ErrBadViewport) {
		t.Fatalf("a negative height must fail with ErrBadViewport, got %v", err)
	}
	if got := c.Viewport(); got != stackViewport() {
		t.Fatalf("viewport after the rejects = %+v, want the previous one", got)
	}
	inset := impl.Rect{X: 10, Y: 20, W: 640, H: 360}
	if err := c.Overlay(impl.CameraSpec{ID: "inset", Order: 5, Viewport: inset, DepthEnabled: true}, 5); err != nil {
		t.Fatalf("overlay inset: %v", err)
	}
	if err := c.Overlay(overlayCamera("full", 10), 10); err != nil {
		t.Fatalf("overlay full: %v", err)
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if s, _ := findStep(steps, "inset"); s.Rect != inset {
		t.Fatalf("inset rect = %+v, want its own %+v", s.Rect, inset)
	}
	if s, _ := findStep(steps, "full"); s.Rect != stackViewport() {
		t.Fatalf("full rect = %+v, want the stack viewport", s.Rect)
	}
}

func TestCamerasReturnsComposeOrder(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("z-last", 30), 30); err != nil {
		t.Fatalf("overlay z-last: %v", err)
	}
	if err := c.Overlay(overlayCamera("a-first", 10), 10); err != nil {
		t.Fatalf("overlay a-first: %v", err)
	}
	overlays := c.Overlays()
	if len(overlays) != 2 || overlays[0].ID != "a-first" || overlays[1].ID != "z-last" {
		t.Fatalf("overlays = %+v, want a-first then z-last", overlays)
	}
	all := c.Cameras()
	if len(all) != 3 || all[0].ID != "base" {
		t.Fatalf("cameras = %+v, want the base first", all)
	}
	if all[1].ID != "a-first" || all[2].ID != "z-last" {
		t.Fatalf("camera order = %v, want the compose order", []string{all[1].ID, all[2].ID})
	}
}

func TestSetOrderReorders(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("o1", 10), 10); err != nil {
		t.Fatalf("overlay o1: %v", err)
	}
	if err := c.Overlay(overlayCamera("o2", 20), 20); err != nil {
		t.Fatalf("overlay o2: %v", err)
	}
	if err := c.SetOrder("nope", 1); !errors.Is(err, impl.ErrNotFound) {
		t.Fatalf("reordering an unknown camera must fail with ErrNotFound, got %v", err)
	}
	if err := c.SetOrder("base", 1); !errors.Is(err, impl.ErrBaseRequired) {
		t.Fatalf("reordering the base camera must fail with ErrBaseRequired, got %v", err)
	}
	if err := c.SetOrder("o1", 30); err != nil {
		t.Fatalf("reorder o1: %v", err)
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	want := []string{"base", "o2", "o1"}
	for i, id := range want {
		if steps[i].CameraID != id {
			t.Fatalf("step %d = %q, want %q", i, steps[i].CameraID, id)
		}
	}
	if steps[2].Order != 30 {
		t.Fatalf("o1 order = %d, want 30", steps[2].Order)
	}
}

func TestSnapshotRestoreKeepsStack(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("o1", 10), 10); err != nil {
		t.Fatalf("overlay o1: %v", err)
	}
	if err := c.Overlay(overlayCamera("o2", 20), 20); err != nil {
		t.Fatalf("overlay o2: %v", err)
	}
	if err := c.SetOrder("o2", 5); err != nil {
		t.Fatalf("reorder o2: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewCameraStackCompositor(stackLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if base, ok := restored.Base(); !ok || base.ID != "base" {
		t.Fatalf("restored base = %+v ok=%v, want base", base, ok)
	}
	if got := restored.Viewport(); got != stackViewport() {
		t.Fatalf("restored viewport = %+v, want the stack viewport", got)
	}
	steps, err := restored.Compose()
	if err != nil {
		t.Fatalf("compose after restore: %v", err)
	}
	want := []string{"base", "o2", "o1"}
	for i, id := range want {
		if steps[i].CameraID != id {
			t.Fatalf("restored step %d = %q, want %q", i, steps[i].CameraID, id)
		}
	}
	if err := restored.Restore([]byte("[]")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, err := restored.Compose(); err != nil {
		t.Fatalf("a rejected restore must not break composing, got %v", err)
	}
}

func TestConcurrentOverlaySerialized(t *testing.T) {
	c := impl.NewCameraStackCompositor(impl.Limits{MaxOverlays: 8, MaxSteps: 16})
	pushBase(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := overlayCamera("o-"+string(rune('a'+i)), i)
			_ = c.Overlay(spec, i)
		}(i)
	}
	wg.Wait()
	if got := len(c.Overlays()); got != 8 {
		t.Fatalf("overlay count after the race = %d, want 8", got)
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose after the race: %v", err)
	}
	if len(steps) != 9 {
		t.Fatalf("step count = %d, want 9 (base plus eight overlays)", len(steps))
	}
	if st := c.Stats(); st.Registered != 9 {
		t.Fatalf("registered counter = %d, want 9", st.Registered)
	}
}

func TestComposeKeepsDeclaredClearFlags(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	base := baseCamera()
	base.ClearDepth = false
	if err := c.Push(base); err != nil {
		t.Fatalf("push base camera: %v", err)
	}
	if err := c.SetViewport(stackViewport()); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	full := overlayCamera("full", 10)
	full.ClearColor = true
	full.ClearDepth = true
	if err := c.Overlay(full, 10); err != nil {
		t.Fatalf("overlay full: %v", err)
	}
	steps, err := c.Compose()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	baseStep, _ := findStep(steps, "base")
	if !baseStep.ClearColor || baseStep.ClearDepth {
		t.Fatalf("base step clears color=%v depth=%v, want the declared color only", baseStep.ClearColor, baseStep.ClearDepth)
	}
	fullStep, _ := findStep(steps, "full")
	if !fullStep.ClearColor || !fullStep.ClearDepth {
		t.Fatalf("overlay step clears color=%v depth=%v, want both buffers", fullStep.ClearColor, fullStep.ClearDepth)
	}
}

func TestStatsCountsComposesAndRejects(t *testing.T) {
	c := impl.NewCameraStackCompositor(stackLimits())
	pushBase(t, c)
	if err := c.Overlay(overlayCamera("o1", 10), 10); err != nil {
		t.Fatalf("overlay o1: %v", err)
	}
	if _, err := c.Compose(); err != nil {
		t.Fatalf("first compose: %v", err)
	}
	if err := c.Overlay(overlayCamera("o1", 20), 20); !errors.Is(err, impl.ErrDuplicateCamera) {
		t.Fatalf("duplicate overlay must fail with ErrDuplicateCamera, got %v", err)
	}
	if _, err := c.Compose(); err != nil {
		t.Fatalf("second compose: %v", err)
	}
	st := c.Stats()
	if st.Composes != 2 {
		t.Fatalf("compose counter = %d, want 2", st.Composes)
	}
	if st.Rejects != 1 {
		t.Fatalf("reject counter = %d, want 1", st.Rejects)
	}
	if st.Removed != 0 {
		t.Fatalf("remove counter = %d, want 0", st.Removed)
	}
}
