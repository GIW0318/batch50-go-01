// Package task freezes expensive audio tracks into rendered cache entries and
// switches between the frozen and the live path only on audio block boundaries.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	defaultEntryBytes int64 = 64 << 10
	defaultCacheBytes int64 = 1 << 20
	snapshotVersion         = 1
)

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

type pendingChange uint8

const (
	changeNone pendingChange = iota
	changeFreeze
	changeUnfreeze
	changeInvalidate
)

type freezeTrack struct {
	state     State
	change    pendingChange
	activeKey string
	order     uint64
}

type cachedItem struct {
	entry CacheEntry
	order uint64
}

// TrackFreezeManager owns the track table, the pending mode changes and the
// render cache.
type TrackFreezeManager struct {
	mu         sync.Mutex
	limits     Limits
	block      int64
	nextFreeze uint64
	clock      uint64
	tracks     map[string]*freezeTrack
	entries    map[string]*cachedItem
	usedBytes  int64
}

// NewTrackFreezeManager builds a manager with the given cache limits.
func NewTrackFreezeManager(limits Limits) *TrackFreezeManager {
	if limits.EntryBytes <= 0 {
		limits.EntryBytes = defaultEntryBytes
	}
	if limits.CacheBytes <= 0 {
		limits.CacheBytes = defaultCacheBytes
	}

	return &TrackFreezeManager{
		limits:  limits,
		tracks:  make(map[string]*freezeTrack),
		entries: make(map[string]*cachedItem),
	}
}

func canonicalCacheKey(parts []string) (string, error) {
	if len(parts) == 0 {
		return "", ErrBadKey
	}

	seen := make(map[string]struct{}, len(parts))
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			return "", ErrBadKey
		}
		if _, exists := seen[item]; exists {
			return "", ErrBadKey
		}
		seen[item] = struct{}{}
		normalized = append(normalized, item)
	}

	sort.Strings(normalized)
	return strings.Join(normalized, "/"), nil
}

// SetCacheKey registers a track or replaces its cache key parts.
func (m *TrackFreezeManager) SetCacheKey(track string, parts []string) error {
	if strings.TrimSpace(track) == "" {
		return ErrBadTrack
	}

	key, err := canonicalCacheKey(parts)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	current, exists := m.tracks[track]
	if exists {
		if current.state.Mode == ModeFrozen || current.state.Pending {
			return ErrFrozen
		}
		current.state.Key = key
		current.activeKey = key
		return nil
	}

	m.tracks[track] = &freezeTrack{
		state: State{
			Track: track,
			Mode:  ModeLive,
			Key:   key,
		},
		activeKey: key,
	}
	return nil
}

// Freeze requests the frozen path from the next audio block onward.
func (m *TrackFreezeManager) Freeze(track string) (FreezeID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, exists := m.tracks[track]
	if !exists {
		return "", ErrUnknownTrack
	}
	if current.state.Mode == ModeFrozen || current.state.Pending {
		return "", ErrAlreadyFrozen
	}
	if current.state.Key == "" {
		return "", ErrBadKey
	}
	if !m.freezeBudgetFitsLocked(current.state.Key) {
		return "", ErrBudget
	}

	m.nextFreeze++
	id := FreezeID(fmt.Sprintf("freeze-%d", m.nextFreeze))
	m.clock++

	current.state.FreezeID = id
	current.state.Pending = true
	current.change = changeFreeze
	current.order = m.clock

	return id, nil
}

func (m *TrackFreezeManager) freezeBudgetFitsLocked(extraKey string) bool {
	missing := make(map[string]struct{})
	projected := m.usedBytes

	addKey := func(key string) {
		if key == "" {
			return
		}
		if _, cached := m.entries[key]; cached {
			return
		}
		if _, reserved := missing[key]; reserved {
			return
		}
		missing[key] = struct{}{}
		projected += m.limits.EntryBytes
	}

	for _, track := range m.tracks {
		if track.change == changeFreeze {
			addKey(track.state.Key)
		}
	}
	addKey(extraKey)

	return projected <= m.limits.CacheBytes
}

// Unfreeze requests the live path from the next audio block onward.
func (m *TrackFreezeManager) Unfreeze(track string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, exists := m.tracks[track]
	if !exists {
		return ErrUnknownTrack
	}
	if current.state.Mode != ModeFrozen {
		return ErrNotFrozen
	}

	if current.change == changeNone {
		m.clock++
		current.change = changeUnfreeze
		current.order = m.clock
		current.state.Pending = true
	}

	return nil
}

// Invalidate drops the cache key of a frozen track and queues the live path.
func (m *TrackFreezeManager) Invalidate(track string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, exists := m.tracks[track]
	if !exists {
		return ErrUnknownTrack
	}
	if current.state.Mode != ModeFrozen {
		return ErrNotFrozen
	}

	switch current.change {
	case changeNone:
		m.clock++
		current.change = changeInvalidate
		current.order = m.clock
		current.state.Pending = true
	case changeUnfreeze:
		current.change = changeInvalidate
	}
	current.state.Key = ""

	return nil
}

// Status reads one track state.
func (m *TrackFreezeManager) Status(track string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, exists := m.tracks[track]
	if !exists {
		return State{}, ErrUnknownTrack
	}
	return current.state, nil
}

// Cached reads one cache entry.
func (m *TrackFreezeManager) Cached(key string) (CacheEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	item, exists := m.entries[key]
	if !exists {
		return CacheEntry{}, false
	}
	return item.entry, true
}

// Collect reclaims every reclaimable cache entry, least recently used first.
func (m *TrackFreezeManager) Collect() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	reclaimable := make([]*cachedItem, 0)
	for _, item := range m.entries {
		if item.entry.Reclaimable && item.entry.Refs == 0 {
			reclaimable = append(reclaimable, item)
		}
	}

	sort.Slice(reclaimable, func(i, j int) bool {
		if reclaimable[i].order != reclaimable[j].order {
			return reclaimable[i].order < reclaimable[j].order
		}
		return reclaimable[i].entry.Key < reclaimable[j].entry.Key
	})

	keys := make([]string, 0, len(reclaimable))
	for _, item := range reclaimable {
		key := item.entry.Key
		keys = append(keys, key)
		m.usedBytes -= item.entry.Bytes
		delete(m.entries, key)
	}

	return keys
}

// Budget reports the used and the configured cache bytes.
func (m *TrackFreezeManager) Budget() (bytes int64, limit int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.usedBytes, m.limits.CacheBytes
}

// Block reports the current audio block index.
func (m *TrackFreezeManager) Block() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.block
}

// Advance moves the block clock and commits pending mode changes.
func (m *TrackFreezeManager) Advance(blocks int) (int64, error) {
	if blocks <= 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.block, ErrBadBlocks
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	pending := make([]*freezeTrack, 0)
	for _, track := range m.tracks {
		if track.change != changeNone {
			pending = append(pending, track)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].order != pending[j].order {
			return pending[i].order < pending[j].order
		}
		return pending[i].state.Track < pending[j].state.Track
	})

	missing := make(map[string]struct{})
	projected := m.usedBytes
	for _, track := range pending {
		if track.change != changeFreeze {
			continue
		}
		key := track.activeKey
		if key == "" {
			key = track.state.Key
		}
		if _, exists := m.entries[key]; exists {
			continue
		}
		if _, reserved := missing[key]; reserved {
			continue
		}
		missing[key] = struct{}{}
		projected += m.limits.EntryBytes
	}
	if projected > m.limits.CacheBytes {
		return m.block, ErrBudget
	}

	m.block += int64(blocks)
	for _, track := range pending {
		switch track.change {
		case changeFreeze:
			m.commitFreezeLocked(track)
		case changeUnfreeze:
			m.commitLiveLocked(track, false)
		case changeInvalidate:
			m.commitLiveLocked(track, true)
		}
		track.change = changeNone
		track.order = 0
	}

	return m.block, nil
}

func (m *TrackFreezeManager) commitFreezeLocked(track *freezeTrack) {
	key := track.activeKey
	if key == "" {
		key = track.state.Key
		track.activeKey = key
	}

	track.state.Mode = ModeFrozen
	track.state.Pending = false
	track.state.Block = m.block
	track.state.Bytes = m.limits.EntryBytes

	item, exists := m.entries[key]
	if !exists {
		m.clock++
		m.entries[key] = &cachedItem{
			entry: CacheEntry{
				Key:       key,
				Bytes:     m.limits.EntryBytes,
				Refs:      1,
				LastBlock: m.block,
			},
			order: m.clock,
		}
		m.usedBytes += m.limits.EntryBytes
		return
	}

	if item.entry.Refs == 0 {
		track.state.Hits++
	}
	item.entry.Hits++
	item.entry.Refs++
	item.entry.Reclaimable = false
	item.entry.LastBlock = m.block
	m.clock++
	item.order = m.clock
}

func (m *TrackFreezeManager) commitLiveLocked(track *freezeTrack, invalidate bool) {
	key := track.activeKey
	if key == "" {
		key = track.state.Key
	}

	track.state.Mode = ModeLive
	track.state.Pending = false
	track.state.Block = m.block
	track.state.Bytes = 0

	item, exists := m.entries[key]
	if exists {
		if item.entry.Refs > 0 {
			item.entry.Refs--
		}

		switch {
		case invalidate && item.entry.Refs == 0:
			m.usedBytes -= item.entry.Bytes
			delete(m.entries, key)
		case item.entry.Refs == 0:
			item.entry.Reclaimable = true
			item.entry.LastBlock = m.block
			m.clock++
			item.order = m.clock
		}
	}

	if invalidate {
		track.state.Key = ""
		track.activeKey = ""
	}
}

type snapshotData struct {
	Version    int             `json:"version"`
	Block      int64           `json:"block"`
	NextFreeze uint64          `json:"next_freeze"`
	Clock      uint64          `json:"clock"`
	Limits     Limits          `json:"limits"`
	UsedBytes  int64           `json:"used_bytes"`
	Tracks     []trackSnapshot `json:"tracks"`
	Entries    []entrySnapshot `json:"entries"`
}

type trackSnapshot struct {
	State
	Change    pendingChange `json:"change"`
	ActiveKey string        `json:"active_key"`
	Order     uint64        `json:"order"`
}

type entrySnapshot struct {
	CacheEntry
	Order uint64 `json:"order"`
}

// Snapshot serialises the track table, the cache and the block clock.
func (m *TrackFreezeManager) Snapshot() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tracks := make([]*freezeTrack, 0, len(m.tracks))
	for _, track := range m.tracks {
		tracks = append(tracks, track)
	}
	sort.Slice(tracks, func(i, j int) bool {
		return tracks[i].state.Track < tracks[j].state.Track
	})

	entries := make([]*cachedItem, 0, len(m.entries))
	for _, entry := range m.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].entry.Key < entries[j].entry.Key
	})

	data := snapshotData{
		Version:    snapshotVersion,
		Block:      m.block,
		NextFreeze: m.nextFreeze,
		Clock:      m.clock,
		Limits:     m.limits,
		UsedBytes:  m.usedBytes,
		Tracks:     make([]trackSnapshot, 0, len(tracks)),
		Entries:    make([]entrySnapshot, 0, len(entries)),
	}
	for _, track := range tracks {
		data.Tracks = append(data.Tracks, trackSnapshot{
			State:     track.state,
			Change:    track.change,
			ActiveKey: track.activeKey,
			Order:     track.order,
		})
	}
	for _, entry := range entries {
		data.Entries = append(data.Entries, entrySnapshot{
			CacheEntry: entry.entry,
			Order:      entry.order,
		})
	}

	return json.Marshal(data)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (m *TrackFreezeManager) Restore(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}

	tracks, entries, usedBytes, clock, nextFreeze, err := buildSnapshotState(&snapshot, m.limits)
	if err != nil {
		return err
	}

	m.block = snapshot.Block
	m.nextFreeze = nextFreeze
	m.clock = clock
	m.tracks = tracks
	m.entries = entries
	m.usedBytes = usedBytes
	return nil
}

func badSnapshot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadSnapshot, fmt.Sprintf(format, args...))
}

func buildSnapshotState(snapshot *snapshotData, limits Limits) (map[string]*freezeTrack, map[string]*cachedItem, int64, uint64, uint64, error) {
	if snapshot.Version != snapshotVersion {
		return nil, nil, 0, 0, 0, badSnapshot("unsupported version %d", snapshot.Version)
	}
	if snapshot.Block < 0 {
		return nil, nil, 0, 0, 0, badSnapshot("negative block")
	}
	if snapshot.UsedBytes < 0 {
		return nil, nil, 0, 0, 0, badSnapshot("negative used bytes")
	}

	tracks := make(map[string]*freezeTrack, len(snapshot.Tracks))
	clock := snapshot.Clock
	nextFreeze := snapshot.NextFreeze

	for index := range snapshot.Tracks {
		item := snapshot.Tracks[index]
		state := item.State

		if strings.TrimSpace(state.Track) == "" {
			return nil, nil, 0, 0, 0, badSnapshot("track %d has no name", index)
		}
		if _, duplicate := tracks[state.Track]; duplicate {
			return nil, nil, 0, 0, 0, badSnapshot("duplicate track %q", state.Track)
		}
		if state.Mode != ModeLive && state.Mode != ModeFrozen {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid mode %q", state.Track, state.Mode)
		}
		if item.Change < changeNone || item.Change > changeInvalidate {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid change", state.Track)
		}
		if state.Pending != (item.Change != changeNone) {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has inconsistent pending marker", state.Track)
		}
		if item.Change == changeFreeze && state.Mode != ModeLive {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid pending freeze", state.Track)
		}
		if (item.Change == changeUnfreeze || item.Change == changeInvalidate) && state.Mode != ModeFrozen {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid pending live switch", state.Track)
		}
		if state.Block < 0 || state.Block > snapshot.Block {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid block", state.Track)
		}
		if state.Bytes < 0 || state.Hits < 0 {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has negative counters", state.Track)
		}
		if state.Mode == ModeFrozen && state.Bytes != limits.EntryBytes {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid frozen bytes", state.Track)
		}
		if state.Mode == ModeLive && state.Bytes != 0 {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has invalid live bytes", state.Track)
		}

		activeKey := item.ActiveKey
		if activeKey == "" {
			activeKey = state.Key
		}
		if (state.Mode == ModeFrozen || item.Change == changeFreeze) && activeKey == "" {
			return nil, nil, 0, 0, 0, badSnapshot("track %q has no cache key", state.Track)
		}
		if state.Key != "" && strings.TrimSpace(state.Key) != state.Key {
			return nil, nil, 0, 0, 0, badSnapshot("track %q key is not canonical", state.Track)
		}
		if activeKey != "" && strings.TrimSpace(activeKey) != activeKey {
			return nil, nil, 0, 0, 0, badSnapshot("track %q active key is not canonical", state.Track)
		}
		if state.FreezeID == "" && (state.Mode == ModeFrozen || item.Change != changeNone) {
			return nil, nil, 0, 0, 0, badSnapshot("track %q is missing freeze id", state.Track)
		}
		if item.Order > clock {
			clock = item.Order
		}
		if state.FreezeID != "" {
			var number uint64
			if _, scanErr := fmt.Sscanf(string(state.FreezeID), "freeze-%d", &number); scanErr == nil && number > nextFreeze {
				nextFreeze = number
			}
		}

		tracks[state.Track] = &freezeTrack{
			state:     state,
			change:    item.Change,
			activeKey: activeKey,
			order:     item.Order,
		}
	}

	entries := make(map[string]*cachedItem, len(snapshot.Entries))
	var usedBytes int64
	for index := range snapshot.Entries {
		item := snapshot.Entries[index]
		entry := item.CacheEntry

		if strings.TrimSpace(entry.Key) == "" {
			return nil, nil, 0, 0, 0, badSnapshot("entry %d has no key", index)
		}
		if _, duplicate := entries[entry.Key]; duplicate {
			return nil, nil, 0, 0, 0, badSnapshot("duplicate cache key %q", entry.Key)
		}
		if entry.Bytes != limits.EntryBytes {
			return nil, nil, 0, 0, 0, badSnapshot("entry %q has invalid byte size", entry.Key)
		}
		if entry.Refs < 0 || entry.Hits < 0 {
			return nil, nil, 0, 0, 0, badSnapshot("entry %q has negative counters", entry.Key)
		}
		if entry.LastBlock < 0 || entry.LastBlock > snapshot.Block {
			return nil, nil, 0, 0, 0, badSnapshot("entry %q has invalid block", entry.Key)
		}
		if entry.Refs == 0 && !entry.Reclaimable {
			return nil, nil, 0, 0, 0, badSnapshot("entry %q is unused but not reclaimable", entry.Key)
		}
		if entry.Refs > 0 && entry.Reclaimable {
			return nil, nil, 0, 0, 0, badSnapshot("entry %q is reclaimable while referenced", entry.Key)
		}
		if item.Order > clock {
			clock = item.Order
		}

		usedBytes += entry.Bytes
		entries[entry.Key] = &cachedItem{entry: entry, order: item.Order}
	}

	if usedBytes > limits.CacheBytes {
		return nil, nil, 0, 0, 0, badSnapshot("cache budget exceeds limit")
	}
	if snapshot.UsedBytes != usedBytes {
		return nil, nil, 0, 0, 0, badSnapshot("used byte count does not match entries")
	}

	referenced := make(map[string]int)
	for _, track := range tracks {
		if track.state.Mode != ModeFrozen {
			continue
		}
		key := track.activeKey
		entry, exists := entries[key]
		if !exists {
			return nil, nil, 0, 0, 0, badSnapshot("frozen track %q misses cache entry", track.state.Track)
		}
		referenced[key]++
		if entry.entry.Refs < referenced[key] {
			return nil, nil, 0, 0, 0, badSnapshot("cache entry %q has inconsistent refs", key)
		}
	}
	for key, count := range referenced {
		if entries[key].entry.Refs != count {
			return nil, nil, 0, 0, 0, badSnapshot("cache entry %q refs do not match tracks", key)
		}
	}
	for key, entry := range entries {
		if entry.entry.Refs != referenced[key] {
			return nil, nil, 0, 0, 0, badSnapshot("cache entry %q has unexpected refs", key)
		}
	}

	projectedBytes := usedBytes
	reserved := make(map[string]struct{})
	for _, track := range tracks {
		if track.change != changeFreeze {
			continue
		}
		key := track.activeKey
		if key == "" {
			key = track.state.Key
		}
		if _, exists := entries[key]; exists {
			continue
		}
		if _, duplicate := reserved[key]; duplicate {
			continue
		}
		reserved[key] = struct{}{}
		projectedBytes += limits.EntryBytes
	}
	if projectedBytes > limits.CacheBytes {
		return nil, nil, 0, 0, 0, badSnapshot("pending freezes exceed cache budget")
	}

	return tracks, entries, usedBytes, clock, nextFreeze, nil
}
