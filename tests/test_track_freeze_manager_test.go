package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-08"
)

func freezeManager() *impl.TrackFreezeManager {
	return impl.NewTrackFreezeManager(impl.Limits{CacheBytes: 1000, EntryBytes: 400})
}

func registerTrack(t *testing.T, m *impl.TrackFreezeManager, track string, parts ...string) {
	t.Helper()
	if err := m.SetCacheKey(track, parts); err != nil {
		t.Fatalf("register %s: %v", track, err)
	}
}

func freezeAndCommit(t *testing.T, m *impl.TrackFreezeManager, track string) impl.FreezeID {
	t.Helper()
	id, err := m.Freeze(track)
	if err != nil {
		t.Fatalf("freeze %s: %v", track, err)
	}
	if _, err := m.Advance(1); err != nil {
		t.Fatalf("advance after freeze %s: %v", track, err)
	}
	return id
}

func TestSetCacheKeyBuildsCanonicalKey(t *testing.T) {
	m := freezeManager()
	if err := m.SetCacheKey("tone-a", []string{"osc", "filter", "gain"}); err != nil {
		t.Fatalf("set cache key: %v", err)
	}
	st, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != impl.ModeLive || st.Pending {
		t.Fatalf("fresh track state = %+v, want a live track without a pending change", st)
	}
	if st.Key != "filter/gain/osc" {
		t.Fatalf("canonical key = %q, want filter/gain/osc", st.Key)
	}
	if st.Block != 0 || st.Bytes != 0 {
		t.Fatalf("fresh track block=%d bytes=%d, want 0 and 0", st.Block, st.Bytes)
	}
	if err := m.SetCacheKey("tone-a", []string{"gain", "filter", " gain "}); !errors.Is(err, impl.ErrBadKey) {
		t.Fatalf("duplicate key parts must fail with ErrBadKey, got %v", err)
	}
	after, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status after rejected key: %v", err)
	}
	if after.Key != "filter/gain/osc" {
		t.Fatalf("rejected key change altered the key to %q", after.Key)
	}
}

func TestSetCacheKeyRejectsInvalidParts(t *testing.T) {
	m := freezeManager()
	if err := m.SetCacheKey("   ", []string{"osc"}); !errors.Is(err, impl.ErrBadTrack) {
		t.Fatalf("blank track must fail with ErrBadTrack, got %v", err)
	}
	for _, parts := range [][]string{nil, {}, {"osc", "  "}, {" "}} {
		if err := m.SetCacheKey("tone-b", parts); !errors.Is(err, impl.ErrBadKey) {
			t.Fatalf("parts %v must fail with ErrBadKey, got %v", parts, err)
		}
	}
	if _, err := m.Status("tone-b"); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("rejected registrations must not create a track, got %v", err)
	}
}

func TestFreezeWaitsForBlockBoundary(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc", "filter")
	id, err := m.Freeze("tone-a")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if id == "" {
		t.Fatalf("freeze must return a non empty id")
	}
	st, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != impl.ModeLive || !st.Pending {
		t.Fatalf("pending freeze state = %+v, want a live track with a pending change", st)
	}
	if st.FreezeID != id {
		t.Fatalf("pending freeze id = %q, want %q", st.FreezeID, id)
	}
	if st.Key != "filter/osc" {
		t.Fatalf("key = %q, want filter/osc", st.Key)
	}
	if st.Bytes != 0 {
		t.Fatalf("pending freeze already reports %d cache bytes, want 0", st.Bytes)
	}
	block, err := m.Advance(1)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if block != 1 {
		t.Fatalf("block after one advance = %d, want 1", block)
	}
	st, err = m.Status("tone-a")
	if err != nil {
		t.Fatalf("status after boundary: %v", err)
	}
	if st.Mode != impl.ModeFrozen || st.Pending {
		t.Fatalf("state after boundary = %+v, want a committed frozen track", st)
	}
	if st.Bytes != 400 {
		t.Fatalf("frozen bytes = %d, want the 400 byte entry cost", st.Bytes)
	}
	if st.Block != 1 {
		t.Fatalf("frozen block = %d, want 1", st.Block)
	}
	if st.FreezeID != id {
		t.Fatalf("frozen id = %q, want the original %q", st.FreezeID, id)
	}
}

func TestFreezeUnknownTrack(t *testing.T) {
	m := freezeManager()
	if _, err := m.Freeze("ghost"); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("freezing an unknown track must fail with ErrUnknownTrack, got %v", err)
	}
	if _, err := m.Freeze("   "); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("freezing a blank track must fail with ErrUnknownTrack, got %v", err)
	}
}

func TestFreezeAlreadyFrozenTrack(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc")
	first := freezeAndCommit(t, m, "tone-a")
	if _, err := m.Freeze("tone-a"); !errors.Is(err, impl.ErrAlreadyFrozen) {
		t.Fatalf("freezing a frozen track must fail with ErrAlreadyFrozen, got %v", err)
	}
	st, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != impl.ModeFrozen || st.Pending || st.FreezeID != first {
		t.Fatalf("rejected freeze changed the state to %+v", st)
	}
}

func TestUnfreezeRejectsLiveTrack(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc")
	if err := m.Unfreeze("tone-a"); !errors.Is(err, impl.ErrNotFrozen) {
		t.Fatalf("unfreezing a live track must fail with ErrNotFrozen, got %v", err)
	}
	if err := m.Invalidate("tone-a"); !errors.Is(err, impl.ErrNotFrozen) {
		t.Fatalf("invalidating a live track must fail with ErrNotFrozen, got %v", err)
	}
	if err := m.Invalidate("ghost"); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("invalidating an unknown track must fail with ErrUnknownTrack, got %v", err)
	}
}

func TestUnfreezeReturnsToLiveAtBoundary(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc")
	freezeAndCommit(t, m, "tone-a")
	if err := m.Unfreeze("tone-a"); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	st, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status while unfreezing: %v", err)
	}
	if st.Mode != impl.ModeFrozen || !st.Pending {
		t.Fatalf("pending unfreeze state = %+v, want a frozen track with a pending change", st)
	}
	block, err := m.Advance(1)
	if err != nil {
		t.Fatalf("advance after unfreeze: %v", err)
	}
	if block != 2 {
		t.Fatalf("block = %d, want 2", block)
	}
	st, err = m.Status("tone-a")
	if err != nil {
		t.Fatalf("status after unfreeze boundary: %v", err)
	}
	if st.Mode != impl.ModeLive || st.Pending || st.Bytes != 0 {
		t.Fatalf("live state = %+v, want live without pending bytes", st)
	}
	if err := m.Unfreeze("tone-a"); !errors.Is(err, impl.ErrNotFrozen) {
		t.Fatalf("unfreezing twice must fail with ErrNotFrozen, got %v", err)
	}
}

func TestAdvanceRejectsNonPositive(t *testing.T) {
	m := freezeManager()
	for _, blocks := range []int{0, -1, -8} {
		if _, err := m.Advance(blocks); !errors.Is(err, impl.ErrBadBlocks) {
			t.Fatalf("advance %d must fail with ErrBadBlocks, got %v", blocks, err)
		}
	}
	if got := m.Block(); got != 0 {
		t.Fatalf("block = %d, want 0 after rejected advances", got)
	}
}

func TestBlockAdvancesMonotonically(t *testing.T) {
	m := freezeManager()
	first, err := m.Advance(3)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if first != 3 {
		t.Fatalf("block after 3 = %d, want 3", first)
	}
	second, err := m.Advance(2)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if second != 5 {
		t.Fatalf("block after another 2 = %d, want 5", second)
	}
	if got := m.Block(); got != 5 {
		t.Fatalf("block = %d, want 5", got)
	}
}

func TestBudgetRejectsFreezeBeyondLimit(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "a")
	registerTrack(t, m, "tone-b", "b")
	registerTrack(t, m, "tone-c", "c")
	freezeAndCommit(t, m, "tone-a")
	freezeAndCommit(t, m, "tone-b")
	if _, err := m.Freeze("tone-c"); !errors.Is(err, impl.ErrBudget) {
		t.Fatalf("freezing beyond the cache budget must fail with ErrBudget, got %v", err)
	}
	st, err := m.Status("tone-c")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != impl.ModeLive || st.Pending || st.Bytes != 0 {
		t.Fatalf("rejected track state = %+v, want a live track without a pending change", st)
	}
	bytes, limit := m.Budget()
	if bytes != 800 || limit != 1000 {
		t.Fatalf("budget = %d/%d, want 800/1000", bytes, limit)
	}
}

func TestCollectReclaimsLeastRecentlyUsed(t *testing.T) {
	m := impl.NewTrackFreezeManager(impl.Limits{CacheBytes: 1000, EntryBytes: 200})
	registerTrack(t, m, "tone-a", "a")
	registerTrack(t, m, "tone-b", "b")
	registerTrack(t, m, "tone-c", "c")
	freezeAndCommit(t, m, "tone-a")
	freezeAndCommit(t, m, "tone-b")
	freezeAndCommit(t, m, "tone-c")
	for _, track := range []string{"tone-a", "tone-b", "tone-c"} {
		if err := m.Unfreeze(track); err != nil {
			t.Fatalf("unfreeze %s: %v", track, err)
		}
		if _, err := m.Advance(1); err != nil {
			t.Fatalf("advance after unfreezing %s: %v", track, err)
		}
	}
	evicted := m.Collect()
	if len(evicted) != 3 {
		t.Fatalf("collect evicted %v, want the three reclaimable keys", evicted)
	}
	if evicted[0] != "a" || evicted[1] != "b" || evicted[2] != "c" {
		t.Fatalf("collect order = %v, want a, b, c by least recent use", evicted)
	}
	for _, key := range evicted {
		if _, ok := m.Cached(key); ok {
			t.Fatalf("evicted key %s must not stay cached", key)
		}
	}
	if bytes, _ := m.Budget(); bytes != 0 {
		t.Fatalf("budget after collect = %d, want 0", bytes)
	}
	if got := m.Collect(); len(got) != 0 {
		t.Fatalf("second collect evicted %v, want nothing", got)
	}
}

func TestCachedEntryLifecycle(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc", "filter")
	if _, ok := m.Cached("filter/osc"); ok {
		t.Fatalf("a track that was never frozen must not produce a cache entry")
	}
	freezeAndCommit(t, m, "tone-a")
	entry, ok := m.Cached("filter/osc")
	if !ok {
		t.Fatalf("expected a cache entry after the freeze boundary")
	}
	if entry.Key != "filter/osc" || entry.Bytes != 400 {
		t.Fatalf("entry = %+v, want key filter/osc with 400 bytes", entry)
	}
	if entry.Refs != 1 || entry.Reclaimable {
		t.Fatalf("entry refs=%d reclaimable=%v, want one reference and not reclaimable", entry.Refs, entry.Reclaimable)
	}
	if _, ok := m.Cached("nothing"); ok {
		t.Fatalf("unknown cache keys must report false")
	}
}

func TestConcurrentFreezeHasSingleWinner(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc")
	var wg sync.WaitGroup
	var mu sync.Mutex
	frozen, rejected := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Freeze("tone-a")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				frozen++
			case errors.Is(err, impl.ErrAlreadyFrozen):
				rejected++
			}
		}()
	}
	wg.Wait()
	if frozen != 1 {
		t.Fatalf("concurrent freezes accepted %d requests, want exactly 1", frozen)
	}
	if rejected != 7 {
		t.Fatalf("concurrent freezes rejected %d requests, want 7", rejected)
	}
	if _, err := m.Advance(1); err != nil {
		t.Fatalf("advance: %v", err)
	}
	st, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != impl.ModeFrozen || st.Pending {
		t.Fatalf("state after the race = %+v, want a committed frozen track", st)
	}
}

func TestSnapshotRestoreKeepsFrozenState(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc", "filter")
	id := freezeAndCommit(t, m, "tone-a")
	data, err := m.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := freezeManager()
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := r.Status("tone-a")
	if err != nil {
		t.Fatalf("status after restore: %v", err)
	}
	if st.Mode != impl.ModeFrozen || st.Pending || st.Bytes != 400 {
		t.Fatalf("restored state = %+v, want a frozen track with 400 bytes", st)
	}
	if st.Key != "filter/osc" || st.FreezeID != id || st.Block != 1 {
		t.Fatalf("restored fields = %+v, want key filter/osc id %q block 1", st, id)
	}
	if bytes, limit := r.Budget(); bytes != 400 || limit != 1000 {
		t.Fatalf("restored budget = %d/%d, want 400/1000", bytes, limit)
	}
	if err := r.Unfreeze("tone-a"); err != nil {
		t.Fatalf("unfreeze after restore: %v", err)
	}
	if _, err := r.Advance(1); err != nil {
		t.Fatalf("advance after restore: %v", err)
	}
	st, err = r.Status("tone-a")
	if err != nil {
		t.Fatalf("status after unfreeze: %v", err)
	}
	if st.Mode != impl.ModeLive || st.Block != 2 {
		t.Fatalf("state after unfreeze = %+v, want live at block 2", st)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	m := freezeManager()
	registerTrack(t, m, "tone-a", "osc")
	freezeAndCommit(t, m, "tone-a")
	if err := m.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	st, err := m.Status("tone-a")
	if err != nil {
		t.Fatalf("status after rejected restore: %v", err)
	}
	if st.Mode != impl.ModeFrozen || st.Bytes != 400 {
		t.Fatalf("rejected restore changed the live state to %+v", st)
	}
	if got := m.Block(); got != 1 {
		t.Fatalf("block = %d, want 1 after a rejected restore", got)
	}
	if block, err := m.Advance(1); err != nil || block != 2 {
		t.Fatalf("advance after rejected restore = %d (err %v), want 2", block, err)
	}
}
