package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-12"
)

const (
	letterboxW = 40
	letterboxH = 24
	barLuma    = uint8(16)
	bodyLuma   = uint8(200)
)

func letterboxFrame(top, bottom, left, right int, bar, body uint8) impl.Frame {
	luma := make([]uint8, letterboxW*letterboxH)
	for y := 0; y < letterboxH; y++ {
		for x := 0; x < letterboxW; x++ {
			v := body
			if y < top || y >= letterboxH-bottom || x < left || x >= letterboxW-right {
				v = bar
			}
			luma[y*letterboxW+x] = v
		}
	}
	return impl.Frame{Width: letterboxW, Height: letterboxH, Luma: luma}
}

func letterboxNew(window int) *impl.LetterboxDetector {
	return impl.NewLetterboxDetector(impl.Config{Threshold: 40, StableWindow: window})
}

func fullRect() impl.CropRect {
	return impl.CropRect{X: 0, Y: 0, W: letterboxW, H: letterboxH}
}

func TestFeedNeedsTheWholeStableWindow(t *testing.T) {
	d := letterboxNew(3)
	bars := letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)

	first, err := d.Feed(bars)
	if err != nil {
		t.Fatalf("first feed: %v", err)
	}
	if first.Streak != 1 {
		t.Fatalf("streak after one frame = %d, want 1", first.Streak)
	}
	if first.Stable {
		t.Fatalf("one frame must not be stable with a window of 3")
	}
	if first.Top != 4 || first.Bottom != 2 {
		t.Fatalf("matte = top %d bottom %d, want top 4 bottom 2", first.Top, first.Bottom)
	}
	if got := d.Rect(); got != fullRect() {
		t.Fatalf("published rect before stability = %+v, want the full frame", got)
	}

	second, err := d.Feed(bars)
	if err != nil {
		t.Fatalf("second feed: %v", err)
	}
	if second.Streak != 2 || second.Stable {
		t.Fatalf("second frame streak=%d stable=%v, want streak 2 and not stable", second.Streak, second.Stable)
	}
	if got := d.Rect(); got != fullRect() {
		t.Fatalf("published rect after two frames = %+v, want the full frame", got)
	}

	third, err := d.Feed(bars)
	if err != nil {
		t.Fatalf("third feed: %v", err)
	}
	if !third.Stable {
		t.Fatalf("third identical frame must complete the window of 3")
	}
	want := impl.CropRect{X: 0, Y: 4, W: 40, H: 18}
	if got := d.Rect(); got != want {
		t.Fatalf("published rect after the window = %+v, want %+v", got, want)
	}
}

func TestFeedDetectsTopAndBottomMatte(t *testing.T) {
	d := letterboxNew(1)
	got, err := d.Feed(letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma))
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if got.Top != 4 || got.Bottom != 2 {
		t.Fatalf("matte = top %d bottom %d, want top 4 bottom 2", got.Top, got.Bottom)
	}
	if got.Left != 0 || got.Right != 0 {
		t.Fatalf("matte = left %d right %d, want no side matte", got.Left, got.Right)
	}
	if !got.Stable {
		t.Fatalf("a window of 1 must publish on the first frame")
	}
	want := impl.CropRect{X: 0, Y: 4, W: 40, H: 18}
	if rect := d.Rect(); rect != want {
		t.Fatalf("published rect = %+v, want %+v", rect, want)
	}
}

func TestFeedDetectsSideMatte(t *testing.T) {
	d := letterboxNew(1)
	got, err := d.Feed(letterboxFrame(0, 0, 4, 6, barLuma, bodyLuma))
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if got.Left != 4 || got.Right != 6 {
		t.Fatalf("matte = left %d right %d, want left 4 right 6", got.Left, got.Right)
	}
	if got.Top != 0 || got.Bottom != 0 {
		t.Fatalf("matte = top %d bottom %d, want no vertical matte", got.Top, got.Bottom)
	}
	want := impl.CropRect{X: 4, Y: 0, W: 30, H: 24}
	if rect := d.Rect(); rect != want {
		t.Fatalf("published rect = %+v, want %+v", rect, want)
	}
}

func TestFeedDetectsBothAxesWithoutSwallowing(t *testing.T) {
	d := letterboxNew(1)
	got, err := d.Feed(letterboxFrame(4, 2, 4, 6, barLuma, bodyLuma))
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if got.Top != 4 || got.Bottom != 2 {
		t.Fatalf("vertical matte = top %d bottom %d, want top 4 bottom 2", got.Top, got.Bottom)
	}
	if got.Left != 4 || got.Right != 6 {
		t.Fatalf("horizontal matte = left %d right %d, want left 4 right 6", got.Left, got.Right)
	}
	want := impl.CropRect{X: 4, Y: 4, W: 30, H: 18}
	if rect := d.Rect(); rect != want {
		t.Fatalf("published rect = %+v, want %+v", rect, want)
	}
}

func TestThresholdDecidesWhatCountsAsMatte(t *testing.T) {
	grey := letterboxFrame(4, 2, 0, 0, 50, bodyLuma)

	strict := letterboxNew(1)
	if err := strict.SetThreshold(100); err != nil {
		t.Fatalf("set threshold 100: %v", err)
	}
	got, err := strict.Feed(grey)
	if err != nil {
		t.Fatalf("feed with a high threshold: %v", err)
	}
	if got.Top != 4 || got.Bottom != 2 {
		t.Fatalf("grey bars above the threshold must be matte, got top %d bottom %d", got.Top, got.Bottom)
	}

	loose := letterboxNew(1)
	if err := loose.SetThreshold(20); err != nil {
		t.Fatalf("set threshold 20: %v", err)
	}
	got, err = loose.Feed(grey)
	if err != nil {
		t.Fatalf("feed with a low threshold: %v", err)
	}
	if got.Top != 0 || got.Bottom != 0 {
		t.Fatalf("grey bars below the threshold must stay content, got top %d bottom %d", got.Top, got.Bottom)
	}
}

func TestSetThresholdRejectsOutOfRange(t *testing.T) {
	d := letterboxNew(3)
	if err := d.SetThreshold(0); !errors.Is(err, impl.ErrBadThreshold) {
		t.Fatalf("threshold 0 must report ErrBadThreshold, got %v", err)
	}
	if err := d.SetThreshold(256); !errors.Is(err, impl.ErrBadThreshold) {
		t.Fatalf("threshold 256 must report ErrBadThreshold, got %v", err)
	}
	if err := d.SetThreshold(40); err != nil {
		t.Fatalf("threshold 40 must be accepted, got %v", err)
	}
}

func TestSetStableWindowRejectsOutOfRange(t *testing.T) {
	d := letterboxNew(3)
	if err := d.SetStableWindow(0); !errors.Is(err, impl.ErrBadWindow) {
		t.Fatalf("window 0 must report ErrBadWindow, got %v", err)
	}
	if err := d.SetStableWindow(121); !errors.Is(err, impl.ErrBadWindow) {
		t.Fatalf("window 121 must report ErrBadWindow, got %v", err)
	}
	if err := d.SetStableWindow(5); err != nil {
		t.Fatalf("window 5 must be accepted, got %v", err)
	}
}

func TestFeedRejectsMalformedFrame(t *testing.T) {
	d := letterboxNew(3)
	short := impl.Frame{Width: 10, Height: 10, Luma: make([]uint8, 50)}
	if _, err := d.Feed(short); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("short luma plane must report ErrBadFrame, got %v", err)
	}
	empty := impl.Frame{Width: 0, Height: 10, Luma: make([]uint8, 10)}
	if _, err := d.Feed(empty); !errors.Is(err, impl.ErrBadFrame) {
		t.Fatalf("zero width must report ErrBadFrame, got %v", err)
	}
	stats := d.Stats()
	if stats.Frames != 0 {
		t.Fatalf("rejected frames must not count as scanned frames, got %d", stats.Frames)
	}
	if stats.Rejected != 2 {
		t.Fatalf("rejected counter = %d, want 2", stats.Rejected)
	}
}

func TestCommitValidatesAndInstallsRect(t *testing.T) {
	d := letterboxNew(1)
	if err := d.Commit(impl.CropRect{X: 0, Y: 0, W: 40, H: 24}); !errors.Is(err, impl.ErrNoFrame) {
		t.Fatalf("commit before any frame must report ErrNoFrame, got %v", err)
	}
	if _, err := d.Feed(letterboxFrame(0, 0, 0, 0, barLuma, bodyLuma)); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if err := d.Commit(impl.CropRect{X: 0, Y: 0, W: 0, H: 12}); !errors.Is(err, impl.ErrBadRect) {
		t.Fatalf("zero width rect must report ErrBadRect, got %v", err)
	}
	if err := d.Commit(impl.CropRect{X: 0, Y: 0, W: 41, H: 24}); !errors.Is(err, impl.ErrBadRect) {
		t.Fatalf("rect past the right edge must report ErrBadRect, got %v", err)
	}
	want := impl.CropRect{X: 8, Y: 6, W: 20, H: 10}
	if err := d.Commit(want); err != nil {
		t.Fatalf("manual commit: %v", err)
	}
	if got := d.Rect(); got != want {
		t.Fatalf("published rect after manual commit = %+v, want %+v", got, want)
	}
	if got := d.Stats().Manual; got != 1 {
		t.Fatalf("manual commit counter = %d, want 1", got)
	}
}

func TestCommitRestartsTheStableWindow(t *testing.T) {
	d := letterboxNew(3)
	bars := letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)
	for i := 0; i < 2; i++ {
		if _, err := d.Feed(bars); err != nil {
			t.Fatalf("feed %d: %v", i, err)
		}
	}
	if err := d.Commit(impl.CropRect{X: 0, Y: 0, W: 40, H: 24}); err != nil {
		t.Fatalf("manual commit: %v", err)
	}
	got, err := d.Feed(bars)
	if err != nil {
		t.Fatalf("feed after commit: %v", err)
	}
	if got.Streak != 1 {
		t.Fatalf("streak right after a manual commit = %d, want the window restarted at 1", got.Streak)
	}
	if got.Stable {
		t.Fatalf("a manual commit must not leave the next frame stable")
	}
}

func TestStatsCountsScansAndPublishes(t *testing.T) {
	d := letterboxNew(3)
	bars := letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)
	for i := 0; i < 6; i++ {
		if _, err := d.Feed(bars); err != nil {
			t.Fatalf("feed %d: %v", i, err)
		}
	}
	stats := d.Stats()
	if stats.Frames != 6 {
		t.Fatalf("scanned frames = %d, want 6", stats.Frames)
	}
	if stats.Stable != 2 {
		t.Fatalf("stable publishes = %d, want 2 windows of 3", stats.Stable)
	}
	if stats.MaxStreak != 3 {
		t.Fatalf("longest streak = %d, want the window length 3", stats.MaxStreak)
	}
	if stats.Rejected != 0 {
		t.Fatalf("rejected frames = %d, want 0", stats.Rejected)
	}
}

func TestResetClearsStateButKeepsGeometry(t *testing.T) {
	d := letterboxNew(3)
	bars := letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)
	for i := 0; i < 3; i++ {
		if _, err := d.Feed(bars); err != nil {
			t.Fatalf("feed %d: %v", i, err)
		}
	}
	if err := d.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	stats := d.Stats()
	if stats.Frames != 0 || stats.Stable != 0 || stats.MaxStreak != 0 {
		t.Fatalf("counters after reset = %+v, want all zero", stats)
	}
	if got := d.Rect(); got != fullRect() {
		t.Fatalf("rect after reset = %+v, want the full frame", got)
	}
	w, h := d.FrameSize()
	if w != letterboxW || h != letterboxH {
		t.Fatalf("frame size after reset = %dx%d, want %dx%d", w, h, letterboxW, letterboxH)
	}
}

func TestSnapshotRestoreKeepsStreakAndRect(t *testing.T) {
	bars := letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)
	d := letterboxNew(3)
	for i := 0; i < 2; i++ {
		if _, err := d.Feed(bars); err != nil {
			t.Fatalf("feed %d: %v", i, err)
		}
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := letterboxNew(3)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := restored.Feed(bars)
	if err != nil {
		t.Fatalf("feed after restore: %v", err)
	}
	if !got.Stable {
		t.Fatalf("the restored streak must complete the window of 3, got streak %d", got.Streak)
	}
	want := impl.CropRect{X: 0, Y: 4, W: 40, H: 18}
	if rect := restored.Rect(); rect != want {
		t.Fatalf("restored rect = %+v, want %+v", rect, want)
	}
	if got := restored.Stats().Frames; got != 3 {
		t.Fatalf("frames after restore and one feed = %d, want 3", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	d := letterboxNew(3)
	if _, err := d.Feed(letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)); err != nil {
		t.Fatalf("feed: %v", err)
	}
	before := d.Rect()
	if err := d.Restore([]byte("}{ broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must report ErrBadSnapshot, got %v", err)
	}
	if got := d.Rect(); got != before {
		t.Fatalf("failed restore changed the published rect to %+v", got)
	}
	if got := d.Stats().Frames; got != 1 {
		t.Fatalf("frames after a rejected restore = %d, want 1", got)
	}
}

func TestConcurrentFeedsCountEveryFrame(t *testing.T) {
	d := letterboxNew(3)
	bars := letterboxFrame(4, 2, 0, 0, barLuma, bodyLuma)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Feed(bars); err != nil {
				t.Errorf("concurrent feed: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := d.Stats().Frames; got != 40 {
		t.Fatalf("scanned frames after 40 concurrent feeds = %d, want 40", got)
	}
	if got := d.Rect(); got != (impl.CropRect{X: 0, Y: 4, W: 40, H: 18}) {
		t.Fatalf("published rect = %+v, want the stable crop", got)
	}
}
