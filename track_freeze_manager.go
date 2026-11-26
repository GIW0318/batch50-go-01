// Package task freezes expensive audio tracks into rendered cache entries and
// switches between the frozen and the live path only on audio block boundaries.
package task

import "errors"

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Track modes reported by Status.
const (
	ModeLive   = "live"
	ModeFrozen = "frozen"
)

// FreezeID identifies one pending or committed freeze request.
type FreezeID string

// Limits bounds the render cache.
type Limits struct {
	CacheBytes int64
	EntryBytes int64
}

// State is the per track view of the freeze manager.
type State struct {
	Track    string
	Mode     string
	Pending  bool
	FreezeID FreezeID
	Key      string
	Bytes    int64
	Block    int64
	Hits     int
}

// CacheEntry is one rendered track image kept for reuse.
type CacheEntry struct {
	Key         string
	Bytes       int64
	Reclaimable bool
	Refs        int
	LastBlock   int64
	Hits        int
}

// Errors reported by the manager.
var (
	ErrBadTrack      = errors.New("track name is required")
	ErrBadKey        = errors.New("cache key parts are invalid")
	ErrUnknownTrack  = errors.New("track is not registered")
	ErrAlreadyFrozen = errors.New("track is already frozen")
	ErrNotFrozen     = errors.New("track is not frozen")
	ErrFrozen        = errors.New("track is frozen")
	ErrBadBlocks     = errors.New("block count must be positive")
	ErrBudget        = errors.New("render cache budget exceeded")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// TrackFreezeManager owns the track table, the pending mode changes and the
// render cache.
type TrackFreezeManager struct {
	limits Limits
}

// NewTrackFreezeManager builds a manager with the given cache limits.
func NewTrackFreezeManager(limits Limits) *TrackFreezeManager {
	return &TrackFreezeManager{limits: limits}
}

// SetCacheKey registers a track or replaces its cache key parts.
func (m *TrackFreezeManager) SetCacheKey(track string, parts []string) error {
	return errNotImplemented
}

// Freeze requests the frozen path from the next audio block onward.
func (m *TrackFreezeManager) Freeze(track string) (FreezeID, error) {
	return "", errNotImplemented
}

// Unfreeze requests the live path from the next audio block onward.
func (m *TrackFreezeManager) Unfreeze(track string) error {
	return errNotImplemented
}

// Invalidate drops the cache key of a frozen track.
func (m *TrackFreezeManager) Invalidate(track string) error {
	return errNotImplemented
}

// Status reads one track state.
func (m *TrackFreezeManager) Status(track string) (State, error) {
	return State{}, errNotImplemented
}

// Cached reads one cache entry.
func (m *TrackFreezeManager) Cached(key string) (CacheEntry, bool) {
	return CacheEntry{}, false
}

// Collect reclaims every reclaimable cache entry, least recently used first.
func (m *TrackFreezeManager) Collect() []string {
	return nil
}

// Budget reports the used and the configured cache bytes.
func (m *TrackFreezeManager) Budget() (bytes int64, limit int64) {
	return 0, 0
}

// Block reports the current audio block index.
func (m *TrackFreezeManager) Block() int64 {
	return 0
}

// Advance moves the block clock and commits pending mode changes.
func (m *TrackFreezeManager) Advance(blocks int) (int64, error) {
	return 0, errNotImplemented
}

// Snapshot serialises the track table, the cache and the block clock.
func (m *TrackFreezeManager) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (m *TrackFreezeManager) Restore(data []byte) error {
	return errNotImplemented
}
