package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-15"
)

func seekMS(n int) time.Duration {
	return time.Duration(n) * time.Millisecond
}

func seekGOP(keyIndex int64, keyPTS time.Duration, frameDur time.Duration, frames, bytes int) impl.GOPInfo {
	return impl.GOPInfo{KeyIndex: keyIndex, KeyPTS: keyPTS, FrameDur: frameDur, Frames: frames, Bytes: bytes}
}

func seekCoordinator(budget, cacheBytes int) *impl.SeekCoordinator {
	return impl.NewSeekCoordinator(impl.Limits{Budget: budget, CacheBytes: cacheBytes})
}

func TestSeekRejectsNegativeTarget(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 100, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	if _, err := c.Seek(-1 * time.Millisecond); !errors.Is(err, impl.ErrBadTarget) {
		t.Fatalf("negative target must report ErrBadTarget, got %v", err)
	}
	if got := c.Position(); got != 0 {
		t.Fatalf("position after a rejected seek = %v, want 0", got)
	}
}

func TestSeekNeedsACoveringGOP(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if _, err := c.Seek(seekMS(1000)); !errors.Is(err, impl.ErrNotSeekable) {
		t.Fatalf("seek without a cached range must report ErrNotSeekable, got %v", err)
	}
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 10, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	if _, err := c.Seek(seekMS(5000)); !errors.Is(err, impl.ErrNotSeekable) {
		t.Fatalf("seek past the cached range must report ErrNotSeekable, got %v", err)
	}
}

func TestAwaitLandsOnTheTargetFrame(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(10, seekMS(1000), seekMS(100), 50, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	id, err := c.Seek(seekMS(1350))
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	frame, err := c.Await(id)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if frame.Index != 13 {
		t.Fatalf("landed frame index = %d, want keyframe 10 plus 3 frames", frame.Index)
	}
	if frame.PTS != seekMS(1300) {
		t.Fatalf("landed pts = %v, want 1300ms", frame.PTS)
	}
	if frame.AnchorIndex != 10 {
		t.Fatalf("anchor = %d, want the keyframe index 10", frame.AnchorIndex)
	}
	if frame.Decoded != 3 {
		t.Fatalf("decoded frames = %d, want 3", frame.Decoded)
	}
	if !frame.Exact {
		t.Fatalf("a seek inside the decode budget must be exact")
	}
	if frame.Request != id {
		t.Fatalf("frame request = %d, want %d", frame.Request, id)
	}
}

func TestPositionFollowsTheDeliveredFrame(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(10, seekMS(1000), seekMS(100), 50, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	id, err := c.Seek(seekMS(1350))
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if got := c.Position(); got != 0 {
		t.Fatalf("position before the frame is delivered = %v, want 0", got)
	}
	if _, err := c.Await(id); err != nil {
		t.Fatalf("await: %v", err)
	}
	if got := c.Position(); got != seekMS(1300) {
		t.Fatalf("position after delivery = %v, want the landed frame 1300ms", got)
	}
}

func TestAwaitRejectsUnknownRequest(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if _, err := c.Await(impl.RequestID(999)); !errors.Is(err, impl.ErrUnknownRequest) {
		t.Fatalf("unknown request must report ErrUnknownRequest, got %v", err)
	}
}

func TestCancelClosesPendingRequest(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 100, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	id, err := c.Seek(seekMS(2000))
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if err := c.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := c.Stats().Cancelled; got != 1 {
		t.Fatalf("cancelled counter = %d, want 1", got)
	}
	if err := c.Cancel(id); !errors.Is(err, impl.ErrRequestClosed) {
		t.Fatalf("second cancel must report ErrRequestClosed, got %v", err)
	}
	if _, err := c.Await(id); !errors.Is(err, impl.ErrRequestClosed) {
		t.Fatalf("await on a cancelled request must report ErrRequestClosed, got %v", err)
	}
	if got := c.Position(); got != 0 {
		t.Fatalf("a cancelled request must not move the position, got %v", got)
	}
}

func TestSetBudgetValidatesRange(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.SetBudget(0); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("budget 0 must report ErrBadBudget, got %v", err)
	}
	if err := c.SetBudget(-5); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("negative budget must report ErrBadBudget, got %v", err)
	}
	if err := c.SetBudget(5000); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("budget above the maximum must report ErrBadBudget, got %v", err)
	}
	if err := c.SetBudget(32); err != nil {
		t.Fatalf("budget 32 must be accepted, got %v", err)
	}
}

func TestBudgetCapsTheDecodedFrames(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.SetBudget(20); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(10), 1000, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	id, err := c.Seek(seekMS(5000))
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	frame, err := c.Await(id)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if frame.Exact {
		t.Fatalf("a target 500 frames past the keyframe cannot be exact with a budget of 20")
	}
	if frame.Decoded != 20 {
		t.Fatalf("decoded frames = %d, want the budget 20", frame.Decoded)
	}
	if frame.Index != 20 {
		t.Fatalf("landed index = %d, want the last reachable frame 20", frame.Index)
	}
	if frame.PTS != seekMS(200) {
		t.Fatalf("landed pts = %v, want 200ms", frame.PTS)
	}
}

func TestCacheGOPValidatesFields(t *testing.T) {
	c := seekCoordinator(64, 4096)
	bad := []impl.GOPInfo{
		{KeyIndex: 0, KeyPTS: 0, FrameDur: 0, Frames: 10, Bytes: 100},
		{KeyIndex: 0, KeyPTS: 0, FrameDur: seekMS(40), Frames: 0, Bytes: 100},
		{KeyIndex: 0, KeyPTS: 0, FrameDur: seekMS(40), Frames: 10, Bytes: 0},
		{KeyIndex: -1, KeyPTS: 0, FrameDur: seekMS(40), Frames: 10, Bytes: 100},
	}
	for i, gop := range bad {
		if err := c.CacheGOP(gop); !errors.Is(err, impl.ErrBadGOP) {
			t.Fatalf("bad gop %d must report ErrBadGOP, got %v", i, err)
		}
	}
	if got := c.CacheSize(); got != 0 {
		t.Fatalf("cache size after rejected gops = %d, want 0", got)
	}
}

func TestCacheEvictsOldestGOPByBytes(t *testing.T) {
	c := seekCoordinator(64, 1000)
	for i := 0; i < 3; i++ {
		gop := seekGOP(int64(i*100), seekMS(i*100*40), seekMS(40), 100, 400)
		if err := c.CacheGOP(gop); err != nil {
			t.Fatalf("cache gop %d: %v", i, err)
		}
	}
	if got := c.CacheSize(); got != 800 {
		t.Fatalf("cache size = %d, want the 800 bytes that fit", got)
	}
	if got := c.Stats().Evicted; got != 1 {
		t.Fatalf("evicted gops = %d, want 1", got)
	}
	if _, err := c.Seek(seekMS(4050)); err != nil {
		t.Fatalf("seek inside the second gop: %v", err)
	}
	if _, err := c.Seek(seekMS(8050)); err != nil {
		t.Fatalf("seek inside the newest gop: %v", err)
	}
	if _, err := c.Seek(seekMS(50)); !errors.Is(err, impl.ErrNotSeekable) {
		t.Fatalf("the evicted gop must no longer be seekable, got %v", err)
	}
}

func TestCacheRejectsOversizedGOP(t *testing.T) {
	c := seekCoordinator(64, 1000)
	big := seekGOP(0, 0, seekMS(40), 100, 2000)
	if err := c.CacheGOP(big); !errors.Is(err, impl.ErrGOPTooLarge) {
		t.Fatalf("gop larger than the cache must report ErrGOPTooLarge, got %v", err)
	}
	if got := c.CacheSize(); got != 0 {
		t.Fatalf("cache size after an oversized gop = %d, want 0", got)
	}
	if _, err := c.Seek(seekMS(100)); !errors.Is(err, impl.ErrNotSeekable) {
		t.Fatalf("a rejected gop must not become seekable, got %v", err)
	}
}

func TestStatsCountsRequestsAndDeliveries(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 100, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	if _, err := c.Seek(seekMS(350)); err != nil {
		t.Fatalf("first seek: %v", err)
	}
	second, err := c.Seek(seekMS(550))
	if err != nil {
		t.Fatalf("second seek: %v", err)
	}
	frame, err := c.Await(second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if frame.Index != 5 {
		t.Fatalf("landed index = %d, want 5", frame.Index)
	}
	stats := c.Stats()
	if stats.Requests != 2 {
		t.Fatalf("request counter = %d, want 2", stats.Requests)
	}
	if stats.Delivered != 1 {
		t.Fatalf("delivered counter = %d, want 1", stats.Delivered)
	}
	if stats.Decoded != 5 {
		t.Fatalf("decoded frame counter = %d, want 5", stats.Decoded)
	}
	if stats.Superseded != 1 {
		t.Fatalf("superseded counter = %d, want 1 for the request replaced by the second seek", stats.Superseded)
	}
}

func TestSnapshotRestoreKeepsPositionAndCache(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 100, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	id, err := c.Seek(seekMS(550))
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := c.Await(id); err != nil {
		t.Fatalf("await: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := seekCoordinator(64, 4096)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Position(); got != seekMS(500) {
		t.Fatalf("restored position = %v, want 500ms", got)
	}
	if got := restored.CacheSize(); got != 512 {
		t.Fatalf("restored cache size = %d, want 512", got)
	}
	next, err := restored.Seek(seekMS(1050))
	if err != nil {
		t.Fatalf("seek after restore: %v", err)
	}
	if next <= id {
		t.Fatalf("request id after restore = %d, must continue past %d", next, id)
	}
	if _, err := restored.Await(next); err != nil {
		t.Fatalf("await after restore: %v", err)
	}
	if got := restored.Position(); got != seekMS(1000) {
		t.Fatalf("position after the restored seek = %v, want 1000ms", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 100, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	id, err := c.Seek(seekMS(250))
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := c.Await(id); err != nil {
		t.Fatalf("await: %v", err)
	}
	if err := c.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must report ErrBadSnapshot, got %v", err)
	}
	if got := c.Position(); got != seekMS(200) {
		t.Fatalf("position after a rejected restore = %v, want 200ms", got)
	}
	if got := c.CacheSize(); got != 512 {
		t.Fatalf("cache size after a rejected restore = %d, want 512", got)
	}
	if _, err := c.Seek(seekMS(300)); err != nil {
		t.Fatalf("seek after a rejected restore: %v", err)
	}
}

func TestConcurrentSeekAssignsUniqueIDs(t *testing.T) {
	c := seekCoordinator(64, 4096)
	if err := c.CacheGOP(seekGOP(0, 0, seekMS(100), 100, 512)); err != nil {
		t.Fatalf("cache gop: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[impl.RequestID]bool)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := c.Seek(seekMS(100 * (i % 10)))
			if err != nil {
				t.Errorf("concurrent seek: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[id] {
				t.Errorf("request id %d was handed out twice", id)
			}
			seen[id] = true
		}(i)
	}
	wg.Wait()
	if len(seen) != 64 {
		t.Fatalf("unique request ids = %d, want 64", len(seen))
	}
	if got := c.Stats().Requests; got != 64 {
		t.Fatalf("request counter = %d, want 64", got)
	}
}
