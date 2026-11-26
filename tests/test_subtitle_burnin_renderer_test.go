package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-14"
)

func subMS(n int) time.Duration {
	return time.Duration(n) * time.Millisecond
}

func subRenderer() *impl.SubtitleBurninRenderer {
	return impl.NewSubtitleBurninRenderer(impl.Config{MaxLayers: 4, GlyphBudget: 40, Fallback: []string{"Source Han"}})
}

func subSheet() impl.StyleSheet {
	return impl.StyleSheet{
		Global: impl.Style{Font: "Source Han", Size: 24, Color: "white", Align: "center"},
		Tracks: map[string]impl.Style{"main": {Size: 36}},
		Cues:   map[string]impl.Style{"sub-1": {Color: "red"}},
	}
}

func subCue(id string, layer, priority, start, end int, text string) impl.Cue {
	return impl.Cue{
		ID:       id,
		Track:    "main",
		Layer:    layer,
		Priority: priority,
		Start:    subMS(start),
		End:      subMS(end),
		Text:     text,
	}
}

func subLoaded(t *testing.T) *impl.SubtitleBurninRenderer {
	t.Helper()
	r := subRenderer()
	if err := r.RegisterFont("Source Han", impl.FontRef{Family: "Source Han", Source: "local"}); err != nil {
		t.Fatalf("register font: %v", err)
	}
	if err := r.Load(subSheet()); err != nil {
		t.Fatalf("load style sheet: %v", err)
	}
	return r
}

func TestAddCueValidatesFields(t *testing.T) {
	r := subLoaded(t)
	bad := []impl.Cue{
		{ID: "", Track: "main", Start: 0, End: subMS(1000), Text: "x"},
		{ID: "c1", Track: "main", Start: subMS(1000), End: subMS(1000), Text: "x"},
		{ID: "c2", Track: "main", Start: 0, End: subMS(1000), Text: "   "},
		{ID: "c3", Track: "main", Layer: -1, Start: 0, End: subMS(1000), Text: "x"},
	}
	for i, cue := range bad {
		if err := r.AddCue(cue); !errors.Is(err, impl.ErrBadCue) {
			t.Fatalf("bad cue %d must report ErrBadCue, got %v", i, err)
		}
	}
	if got := len(r.Cues(0, subMS(10000))); got != 0 {
		t.Fatalf("rejected cues must not enter the table, got %d", got)
	}
	if err := r.AddCue(subCue("good", 0, 1, 0, 1000, "hello")); err != nil {
		t.Fatalf("valid cue must be accepted, got %v", err)
	}
	if got := len(r.Cues(0, subMS(10000))); got != 1 {
		t.Fatalf("cue table size = %d, want 1", got)
	}
}

func TestCuesFiltersByRangeInStartOrder(t *testing.T) {
	r := subLoaded(t)
	for _, cue := range []impl.Cue{
		subCue("c1", 0, 1, 0, 1000, "one"),
		subCue("c2", 0, 1, 1000, 3000, "two"),
		subCue("c3", 0, 1, 4000, 5000, "three"),
	} {
		if err := r.AddCue(cue); err != nil {
			t.Fatalf("add %s: %v", cue.ID, err)
		}
	}
	got := r.Cues(0, subMS(2000))
	if len(got) != 2 {
		t.Fatalf("cues in 0-2000ms = %d, want 2", len(got))
	}
	if got[0].ID != "c1" || got[1].ID != "c2" {
		t.Fatalf("cues in start order = %q,%q, want c1,c2", got[0].ID, got[1].ID)
	}
	if got := r.Cues(subMS(3000), subMS(4500)); len(got) != 1 || got[0].ID != "c3" {
		t.Fatalf("cues in 3000-4500ms = %+v, want only c3", got)
	}
}

func TestAtRendersCueInsideItsWindow(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("sub-1", 0, 7, 1000, 2000, "第一行")); err != nil {
		t.Fatalf("add cue: %v", err)
	}
	items, err := r.At(impl.Frame{PTS: subMS(1500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render frame: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items on the frame = %d, want 1", len(items))
	}
	if items[0].Cue != "sub-1" || items[0].Text != "第一行" {
		t.Fatalf("item = %+v, want the cue sub-1", items[0])
	}
	if items[0].Layer != 0 {
		t.Fatalf("item layer = %d, want 0", items[0].Layer)
	}
	if items[0].Font.Family != "Source Han" {
		t.Fatalf("item font = %q, want Source Han", items[0].Font.Family)
	}
}

func TestAtUsesHalfOpenFrameWindow(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("sub-1", 0, 7, 1000, 2000, "window")); err != nil {
		t.Fatalf("add cue: %v", err)
	}
	start, err := r.At(impl.Frame{PTS: subMS(1000), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render the first frame: %v", err)
	}
	if len(start) != 1 {
		t.Fatalf("items on the first frame = %d, want 1 because the window is closed on the left", len(start))
	}
	end, err := r.At(impl.Frame{PTS: subMS(2000), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render the end frame: %v", err)
	}
	if len(end) != 0 {
		t.Fatalf("items on the end frame = %d, want 0 because the window is open on the right", len(end))
	}
}

func TestStyleInheritsGlobalTrackAndCue(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("sub-1", 0, 7, 0, 2000, "styled")); err != nil {
		t.Fatalf("add cue: %v", err)
	}
	if err := r.AddCue(impl.Cue{ID: "plain", Track: "extra", Layer: 1, Priority: 1, Start: 0, End: subMS(2000), Text: "plain"}); err != nil {
		t.Fatalf("add plain cue: %v", err)
	}
	items, err := r.At(impl.Frame{PTS: subMS(500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render frame: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 across two layers", len(items))
	}
	main := items[0]
	if main.Cue != "sub-1" {
		t.Fatalf("first item = %q, want sub-1 on the lower layer", main.Cue)
	}
	if main.Style.Size != 36 {
		t.Fatalf("cue size = %d, want the track override 36", main.Style.Size)
	}
	if main.Style.Color != "red" {
		t.Fatalf("cue colour = %q, want the cue override red", main.Style.Color)
	}
	if main.Style.Align != "center" {
		t.Fatalf("cue align = %q, want the global centre alignment", main.Style.Align)
	}
	if main.Style.Font != "Source Han" {
		t.Fatalf("cue font = %q, want the global font", main.Style.Font)
	}
	plain := items[1]
	if plain.Cue != "plain" {
		t.Fatalf("second item = %q, want plain", plain.Cue)
	}
	if plain.Style.Size != 24 {
		t.Fatalf("track without overrides must keep the global size 24, got %d", plain.Style.Size)
	}
	if plain.Style.Color != "white" {
		t.Fatalf("track without overrides must keep the global colour, got %q", plain.Style.Color)
	}
}

func TestSameLayerKeepsHighestPriority(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("low", 0, 5, 0, 2000, "low")); err != nil {
		t.Fatalf("add low: %v", err)
	}
	if err := r.AddCue(subCue("high", 0, 9, 0, 2000, "high")); err != nil {
		t.Fatalf("add high: %v", err)
	}
	items, err := r.At(impl.Frame{PTS: subMS(500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render frame: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items on one layer = %d, want only the winner", len(items))
	}
	if items[0].Cue != "high" {
		t.Fatalf("winner = %q, want the higher priority cue", items[0].Cue)
	}
	if got := r.Stats().Suppressed; got != 1 {
		t.Fatalf("suppressed counter = %d, want 1", got)
	}
}

func TestCrossLayerShowsBothInLayerOrder(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("upper", 2, 1, 0, 2000, "upper")); err != nil {
		t.Fatalf("add upper: %v", err)
	}
	if err := r.AddCue(subCue("base", 0, 1, 0, 2000, "base")); err != nil {
		t.Fatalf("add base: %v", err)
	}
	items, err := r.At(impl.Frame{PTS: subMS(500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render frame: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want both layers", len(items))
	}
	if items[0].Layer != 0 || items[1].Layer != 2 {
		t.Fatalf("layer order = %d,%d, want ascending 0,2", items[0].Layer, items[1].Layer)
	}
	if items[0].Cue != "base" || items[1].Cue != "upper" {
		t.Fatalf("item order = %q,%q, want base then upper", items[0].Cue, items[1].Cue)
	}
}

func TestLayerLimitCapsVisibleLayers(t *testing.T) {
	r := impl.NewSubtitleBurninRenderer(impl.Config{MaxLayers: 2, GlyphBudget: 40})
	if err := r.Load(subSheet()); err != nil {
		t.Fatalf("load style sheet: %v", err)
	}
	for _, cue := range []impl.Cue{
		subCue("l0", 0, 1, 0, 2000, "l0"),
		subCue("l1", 1, 1, 0, 2000, "l1"),
		subCue("l2", 2, 1, 0, 2000, "l2"),
		subCue("l3", 3, 1, 0, 2000, "l3"),
	} {
		if err := r.AddCue(cue); err != nil {
			t.Fatalf("add %s: %v", cue.ID, err)
		}
	}
	items, err := r.At(impl.Frame{PTS: subMS(500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render frame: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want the layer cap 2", len(items))
	}
	if items[0].Layer != 0 || items[1].Layer != 1 {
		t.Fatalf("kept layers = %d,%d, want the two lowest layer numbers", items[0].Layer, items[1].Layer)
	}
}

func TestBudgetReportsGlyphsOfTheFrame(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("ascii", 0, 1, 0, 2000, "abc")); err != nil {
		t.Fatalf("add ascii: %v", err)
	}
	if err := r.AddCue(subCue("cjk", 1, 1, 0, 2000, "字幕组")); err != nil {
		t.Fatalf("add cjk: %v", err)
	}
	if _, err := r.At(impl.Frame{PTS: subMS(500), Width: 1920, Height: 1080}); err != nil {
		t.Fatalf("render frame: %v", err)
	}
	glyphs, limit := r.Budget()
	if glyphs != 6 {
		t.Fatalf("glyphs on the frame = %d, want 3+3=6", glyphs)
	}
	if limit != 40 {
		t.Fatalf("glyph limit = %d, want the configured 40", limit)
	}
}

func TestSetSafeAreaValidatesRectangle(t *testing.T) {
	r := subLoaded(t)
	if err := r.SetSafeArea(impl.Rect{X: 0, Y: 0, W: 0, H: 100}); !errors.Is(err, impl.ErrBadArea) {
		t.Fatalf("zero width safe area must report ErrBadArea, got %v", err)
	}
	if err := r.SetSafeArea(impl.Rect{X: -1, Y: 0, W: 100, H: 100}); !errors.Is(err, impl.ErrBadArea) {
		t.Fatalf("negative origin safe area must report ErrBadArea, got %v", err)
	}
	if err := r.SetSafeArea(impl.Rect{X: 80, Y: 60, W: 1760, H: 960}); err != nil {
		t.Fatalf("valid safe area must be accepted, got %v", err)
	}
}

func TestResolveFontWalksTheFallbackChain(t *testing.T) {
	r := subRenderer()
	if err := r.RegisterFont("Source Han", impl.FontRef{Family: "Source Han", Source: "local"}); err != nil {
		t.Fatalf("register font: %v", err)
	}
	got, err := r.ResolveFont("Noto Sans SC")
	if err != nil {
		t.Fatalf("missing family must resolve through the fallback chain, got %v", err)
	}
	if got.Family != "Source Han" {
		t.Fatalf("resolved family = %q, want the fallback Source Han", got.Family)
	}
	if !got.Fallback {
		t.Fatalf("a fallback resolution must be marked as such")
	}
	direct, err := r.ResolveFont("Source Han")
	if err != nil {
		t.Fatalf("installed family must resolve: %v", err)
	}
	if direct.Fallback {
		t.Fatalf("a direct hit must not be marked as a fallback")
	}
}

func TestResolveFontReportsMissingFamily(t *testing.T) {
	r := impl.NewSubtitleBurninRenderer(impl.Config{MaxLayers: 2, GlyphBudget: 40})
	if _, err := r.ResolveFont("Absent Family"); !errors.Is(err, impl.ErrFontMissing) {
		t.Fatalf("unresolvable family must report ErrFontMissing, got %v", err)
	}
}

func TestSnapshotRestoreKeepsCuesAndStyles(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("sub-1", 0, 7, 1000, 2000, "保真")); err != nil {
		t.Fatalf("add cue: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := subRenderer()
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(restored.Cues(0, subMS(5000))); got != 1 {
		t.Fatalf("cues after restore = %d, want 1", got)
	}
	items, err := restored.At(impl.Frame{PTS: subMS(1500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render after restore: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items after restore = %d, want 1", len(items))
	}
	if items[0].Style.Size != 36 || items[0].Style.Color != "red" {
		t.Fatalf("restored style = size %d colour %q, want the track size 36 and the cue colour red", items[0].Style.Size, items[0].Style.Color)
	}
	if items[0].Font.Family != "Source Han" {
		t.Fatalf("restored font = %q, want Source Han", items[0].Font.Family)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("sub-1", 0, 7, 0, 2000, "live")); err != nil {
		t.Fatalf("add cue: %v", err)
	}
	if err := r.Restore([]byte("<snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must report ErrBadSnapshot, got %v", err)
	}
	if got := len(r.Cues(0, subMS(5000))); got != 1 {
		t.Fatalf("cues after a rejected restore = %d, want 1", got)
	}
	items, err := r.At(impl.Frame{PTS: subMS(500), Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("render after a rejected restore: %v", err)
	}
	if len(items) != 1 || items[0].Text != "live" {
		t.Fatalf("live cue after a rejected restore = %+v, want the existing cue", items)
	}
}

func TestConcurrentAtCountsEveryFrame(t *testing.T) {
	r := subLoaded(t)
	if err := r.AddCue(subCue("sub-1", 0, 7, 0, 5000, "并发")); err != nil {
		t.Fatalf("add cue: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := r.At(impl.Frame{PTS: subMS(1000), Width: 1920, Height: 1080})
			if err != nil {
				t.Errorf("concurrent render: %v", err)
				return
			}
			if len(items) != 1 {
				t.Errorf("concurrent render items = %d, want 1", len(items))
			}
		}()
	}
	wg.Wait()
	if got := r.Stats().Frames; got != 32 {
		t.Fatalf("rendered frames after 32 concurrent calls = %d, want 32", got)
	}
}
