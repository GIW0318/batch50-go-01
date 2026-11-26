// Package task owns the reroll of equipment extra attributes: locked entries
// that raise the price, a weighted pool per quality tier, a preview with a
// validity window and a commit that charges and replaces in one step.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pricing anchors of one reroll.
const (
	BaseRollCost  = 10
	LockSurcharge = 15
)

const snapshotVersion = 1

// Errors returned by the guard.
var (
	ErrBadItem            = errors.New("item spec is invalid")
	ErrItemExists         = errors.New("item is already registered")
	ErrUnknownItem        = errors.New("unknown item")
	ErrBadPool            = errors.New("pool is invalid")
	ErrUnknownPool        = errors.New("no pool for this tier")
	ErrBadLock            = errors.New("locked index is invalid")
	ErrLockLimit          = errors.New("lock cap reached")
	ErrNotLocked          = errors.New("index is not locked")
	ErrStaleRoll          = errors.New("roll was not produced by the latest preview")
	ErrRollExpired        = errors.New("preview already expired")
	ErrInsufficientShards = errors.New("not enough shards for this reroll")
	ErrBadShards          = errors.New("shard amount must be positive")
	ErrTimeWentBackwards  = errors.New("clock moved backwards")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// Entry is one extra attribute of a piece of equipment.
type Entry struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// PoolEntry is one weighted candidate of a quality tier.
type PoolEntry struct {
	Name     string `json:"name"`
	Weight   int    `json:"weight"`
	MinValue int    `json:"min_value"`
	MaxValue int    `json:"max_value"`
}

// Pool is the weighted candidate table of one quality tier.
type Pool struct {
	Tier    string      `json:"tier"`
	Entries []PoolEntry `json:"entries"`
}

// ItemSpec is the static description of one piece of equipment.
type ItemSpec struct {
	ID      string  `json:"id"`
	Tier    string  `json:"tier"`
	Entries []Entry `json:"entries"`
}

// Roll is one previewed reroll result.
type Roll struct {
	ItemID    string  `json:"item_id"`
	Entries   []Entry `json:"entries"`
	Locked    []int   `json:"locked"`
	Cost      int     `json:"cost"`
	Seq       int64   `json:"seq"`
	ExpiresAt int64   `json:"expires_at"`
}

// Cost is the projected price of the next reroll.
type Cost struct {
	ItemID string `json:"item_id"`
	Locked int    `json:"locked"`
	Amount int    `json:"amount"`
}

// Result is the outcome of one committed reroll.
type Result struct {
	ItemID  string  `json:"item_id"`
	Entries []Entry `json:"entries"`
	Cost    int     `json:"cost"`
	Shards  int     `json:"shards"`
	Seq     int64   `json:"seq"`
}

// RollRecord is one entry of the per item reroll history.
type RollRecord struct {
	ItemID  string  `json:"item_id"`
	Entries []Entry `json:"entries"`
	Cost    int     `json:"cost"`
	At      int64   `json:"at"`
	Seq     int64   `json:"seq"`
}

// Limits bounds the registries, the lock cap, the history and the preview TTL.
type Limits struct {
	MaxItems         int   `json:"max_items"`
	MaxLocks         int   `json:"max_locks"`
	MaxHistory       int   `json:"max_history"`
	PreviewTTLMillis int64 `json:"preview_ttl_millis"`
}

type itemState struct {
	ID      string
	Tier    string
	Entries []Entry
	Shards  int
	Locks   []int
	History []RollRecord
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Data     json.RawMessage `json:"data"`
}

type snapshotData struct {
	Limits    Limits                  `json:"limits"`
	NowMillis int64                   `json:"now_millis"`
	Seq       int64                   `json:"seq"`
	Seed      int64                   `json:"seed"`
	Pools     map[string]Pool         `json:"pools"`
	Items     map[string]snapshotItem `json:"items"`
}

type snapshotItem struct {
	ID      string       `json:"id"`
	Tier    string       `json:"tier"`
	Entries []Entry      `json:"entries"`
	Shards  int          `json:"shards"`
	Locks   []int        `json:"locks"`
	History []RollRecord `json:"history"`
}

// AttributeRerollGuard owns the pools, the items, the locks and the history.
type AttributeRerollGuard struct {
	mu       sync.RWMutex
	limits   Limits
	now      time.Time
	seed     int64
	rng      *rand.Rand
	seq      int64
	pools    map[string]Pool
	items    map[string]*itemState
	previews map[string]Roll
}

// NewAttributeRerollGuard builds a guard with the given caps, starting clock
// and pseudo random seed.
func NewAttributeRerollGuard(limits Limits, now time.Time, seed int64) *AttributeRerollGuard {
	return &AttributeRerollGuard{
		limits:   limits,
		now:      now,
		seed:     seed,
		rng:      rand.New(rand.NewSource(seed)),
		pools:    make(map[string]Pool),
		items:    make(map[string]*itemState),
		previews: make(map[string]Roll),
	}
}

// SetPool records the weighted pool of one quality tier.
func (g *AttributeRerollGuard) SetPool(pool Pool) error {
	if err := validatePool(pool); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.pools[pool.Tier] = clonePool(pool)
	return nil
}

// RegisterItem records one piece of equipment and its current attributes.
func (g *AttributeRerollGuard) RegisterItem(spec ItemSpec) error {
	if strings.TrimSpace(spec.ID) == "" || len(spec.Entries) == 0 {
		return ErrBadItem
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.pools[spec.Tier]; !ok {
		return ErrUnknownPool
	}
	if _, exists := g.items[spec.ID]; exists {
		return ErrItemExists
	}
	if g.limits.MaxItems > 0 && len(g.items) >= g.limits.MaxItems {
		return ErrBadItem
	}

	g.items[spec.ID] = &itemState{
		ID:      spec.ID,
		Tier:    spec.Tier,
		Entries: cloneEntries(spec.Entries),
		Locks:   []int{},
		History: []RollRecord{},
	}
	return nil
}

// GrantShards adds reroll shards to one item.
func (g *AttributeRerollGuard) GrantShards(itemID string, n int) error {
	if n <= 0 {
		return ErrBadShards
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	item := g.items[itemID]
	if item == nil {
		return ErrUnknownItem
	}
	item.Shards += n
	return nil
}

// Shards reads the shard balance of one item.
func (g *AttributeRerollGuard) Shards(itemID string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	item := g.items[itemID]
	if item == nil {
		return 0
	}
	return item.Shards
}

// Entries reads the current attributes of one item.
func (g *AttributeRerollGuard) Entries(itemID string) ([]Entry, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	item := g.items[itemID]
	if item == nil {
		return nil, ErrUnknownItem
	}
	return cloneEntries(item.Entries), nil
}

// Lock adds one index to the locked set of an item.
func (g *AttributeRerollGuard) Lock(itemID string, idx int) (Cost, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	item := g.items[itemID]
	if item == nil {
		return Cost{}, ErrUnknownItem
	}
	if idx < 0 || idx >= len(item.Entries) {
		return Cost{}, ErrBadLock
	}
	for _, lockedIdx := range item.Locks {
		if lockedIdx == idx {
			return Cost{}, ErrBadLock
		}
	}
	if len(item.Locks) >= g.limits.MaxLocks {
		return Cost{}, ErrLockLimit
	}

	item.Locks = append(item.Locks, idx)
	sort.Ints(item.Locks)
	delete(g.previews, itemID)
	return Cost{
		ItemID: itemID,
		Locked: len(item.Locks),
		Amount: rerollCost(len(item.Locks)),
	}, nil
}

// Unlock removes one index from the locked set of an item.
func (g *AttributeRerollGuard) Unlock(itemID string, idx int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	item := g.items[itemID]
	if item == nil {
		return ErrUnknownItem
	}

	for i, lockedIdx := range item.Locks {
		if lockedIdx != idx {
			continue
		}
		item.Locks = append(item.Locks[:i], item.Locks[i+1:]...)
		delete(g.previews, itemID)
		return nil
	}
	return ErrNotLocked
}

// UnlockAll clears the locked set of an item.
func (g *AttributeRerollGuard) UnlockAll(itemID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	item := g.items[itemID]
	if item == nil {
		return ErrUnknownItem
	}
	if len(item.Locks) > 0 {
		item.Locks = []int{}
		delete(g.previews, itemID)
	}
	return nil
}

// Locked returns a copy of the locked indices of one item, ascending.
func (g *AttributeRerollGuard) Locked(itemID string) []int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	item := g.items[itemID]
	if item == nil {
		return nil
	}
	return cloneInts(item.Locks)
}

// Preview builds a reroll proposal without touching the item.
func (g *AttributeRerollGuard) Preview(itemID string, locked []int) (Roll, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	item := g.items[itemID]
	if item == nil {
		return Roll{}, ErrUnknownItem
	}
	requestedLocks, err := normalizeLockIndices(item, locked)
	if err != nil {
		return Roll{}, err
	}
	lockIndices := mergeLockIndices(item.Locks, requestedLocks)
	if len(lockIndices) > g.limits.MaxLocks {
		return Roll{}, ErrLockLimit
	}
	pool, ok := g.pools[item.Tier]
	if !ok {
		return Roll{}, ErrUnknownPool
	}
	entries, err := g.buildRollEntries(item, pool, lockIndices)
	if err != nil {
		return Roll{}, err
	}

	g.seq++
	roll := Roll{
		ItemID:    itemID,
		Entries:   entries,
		Locked:    cloneInts(lockIndices),
		Cost:      rerollCost(len(lockIndices)),
		Seq:       g.seq,
		ExpiresAt: g.now.UnixMilli() + g.limits.PreviewTTLMillis,
	}
	g.previews[itemID] = cloneRoll(roll)
	return cloneRoll(roll), nil
}

// Apply commits one previewed reroll.
func (g *AttributeRerollGuard) Apply(itemID string, roll Roll) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	item := g.items[itemID]
	if item == nil {
		return Result{}, ErrUnknownItem
	}
	pending, ok := g.previews[itemID]
	if !ok || pending.Seq != roll.Seq || (roll.ItemID != "" && roll.ItemID != itemID) {
		return Result{}, ErrStaleRoll
	}
	if g.now.UnixMilli() >= pending.ExpiresAt {
		return Result{}, ErrRollExpired
	}
	if item.Shards < pending.Cost {
		return Result{}, ErrInsufficientShards
	}

	committedEntries := cloneEntries(pending.Entries)
	item.Shards -= pending.Cost
	item.Entries = committedEntries
	item.History = append(item.History, RollRecord{
		ItemID:  itemID,
		Entries: cloneEntries(committedEntries),
		Cost:    pending.Cost,
		At:      g.now.UnixMilli(),
		Seq:     pending.Seq,
	})
	trimHistory(item, g.limits.MaxHistory)
	delete(g.previews, itemID)

	return Result{
		ItemID:  itemID,
		Entries: cloneEntries(committedEntries),
		Cost:    pending.Cost,
		Shards:  item.Shards,
		Seq:     pending.Seq,
	}, nil
}

// History returns a copy of the per item reroll history, oldest first.
func (g *AttributeRerollGuard) History(itemID string) []RollRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()

	item := g.items[itemID]
	if item == nil {
		return nil
	}
	return cloneHistory(item.History)
}

// AdvanceClock moves the guard clock forward.
func (g *AttributeRerollGuard) AdvanceClock(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if now.Before(g.now) {
		return ErrTimeWentBackwards
	}
	g.now = now
	return nil
}

// Snapshot serialises the whole guard state.
func (g *AttributeRerollGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	pools := make(map[string]Pool, len(g.pools))
	for tier, pool := range g.pools {
		pools[tier] = clonePool(pool)
	}

	items := make(map[string]snapshotItem, len(g.items))
	for id, item := range g.items {
		items[id] = snapshotItem{
			ID:      item.ID,
			Tier:    item.Tier,
			Entries: cloneEntries(item.Entries),
			Shards:  item.Shards,
			Locks:   cloneInts(item.Locks),
			History: cloneHistory(item.History),
		}
	}

	payload, err := json.Marshal(snapshotData{
		Limits:    g.limits,
		NowMillis: g.now.UnixMilli(),
		Seq:       g.seq,
		Seed:      g.seed,
		Pools:     pools,
		Items:     items,
	})
	if err != nil {
		return nil, err
	}

	checksum := sha256.Sum256(payload)
	return json.Marshal(snapshotEnvelope{
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(checksum[:]),
		Data:     payload,
	})
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *AttributeRerollGuard) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || envelope.Checksum == "" || len(envelope.Data) == 0 {
		return ErrBadSnapshot
	}

	checksum := sha256.Sum256(envelope.Data)
	if hex.EncodeToString(checksum[:]) != envelope.Checksum {
		return ErrBadSnapshot
	}

	var state snapshotData
	if err := json.Unmarshal(envelope.Data, &state); err != nil {
		return ErrBadSnapshot
	}
	if err := validateSnapshot(&state); err != nil {
		return err
	}

	pools := make(map[string]Pool, len(state.Pools))
	for tier, pool := range state.Pools {
		pools[tier] = clonePool(pool)
	}

	items := make(map[string]*itemState, len(state.Items))
	nextSeq := state.Seq
	for id, snapshotItem := range state.Items {
		locks := cloneInts(snapshotItem.Locks)
		sort.Ints(locks)
		history := cloneHistory(snapshotItem.History)
		for _, record := range history {
			if record.Seq > nextSeq {
				nextSeq = record.Seq
			}
		}
		items[id] = &itemState{
			ID:      id,
			Tier:    snapshotItem.Tier,
			Entries: cloneEntries(snapshotItem.Entries),
			Shards:  snapshotItem.Shards,
			Locks:   locks,
			History: history,
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.limits = state.Limits
	g.now = time.UnixMilli(state.NowMillis)
	g.seed = state.Seed
	g.rng = rand.New(rand.NewSource(state.Seed))
	g.seq = nextSeq
	g.pools = pools
	g.items = items
	g.previews = make(map[string]Roll)
	return nil
}

func validatePool(pool Pool) error {
	if strings.TrimSpace(pool.Tier) == "" || len(pool.Entries) == 0 {
		return ErrBadPool
	}
	for _, candidate := range pool.Entries {
		if strings.TrimSpace(candidate.Name) == "" || candidate.Weight <= 0 || candidate.MinValue > candidate.MaxValue {
			return ErrBadPool
		}
	}
	return nil
}

func normalizeLockIndices(item *itemState, requested []int) ([]int, error) {
	locks := make([]int, 0, len(requested))
	seen := make(map[int]struct{}, len(requested))
	for _, idx := range requested {
		if idx < 0 || idx >= len(item.Entries) {
			return nil, ErrBadLock
		}
		if _, repeated := seen[idx]; repeated {
			return nil, ErrBadLock
		}
		seen[idx] = struct{}{}
		locks = append(locks, idx)
	}
	sort.Ints(locks)
	return locks, nil
}

func mergeLockIndices(persistent []int, requested []int) []int {
	merged := make(map[int]struct{}, len(persistent)+len(requested))
	for _, idx := range persistent {
		merged[idx] = struct{}{}
	}
	for _, idx := range requested {
		merged[idx] = struct{}{}
	}
	locks := make([]int, 0, len(merged))
	for idx := range merged {
		locks = append(locks, idx)
	}
	sort.Ints(locks)
	return locks
}

func (g *AttributeRerollGuard) buildRollEntries(item *itemState, pool Pool, locks []int) ([]Entry, error) {
	locked := make(map[int]struct{}, len(locks))
	for _, idx := range locks {
		locked[idx] = struct{}{}
	}

	usedNames := make(map[string]struct{}, len(item.Entries))
	entries := make([]Entry, len(item.Entries))
	for idx, current := range item.Entries {
		if _, isLocked := locked[idx]; !isLocked {
			continue
		}
		entries[idx] = current
		usedNames[current.Name] = struct{}{}
	}

	for idx := range item.Entries {
		if _, isLocked := locked[idx]; isLocked {
			continue
		}
		candidate, err := g.drawCandidate(pool, usedNames)
		if err != nil {
			return nil, err
		}
		value := candidate.MinValue
		span := candidate.MaxValue - candidate.MinValue + 1
		if span > 1 {
			value += g.rng.Intn(span)
		}
		entries[idx] = Entry{Name: candidate.Name, Value: value}
		usedNames[candidate.Name] = struct{}{}
	}
	return entries, nil
}

func (g *AttributeRerollGuard) drawCandidate(pool Pool, usedNames map[string]struct{}) (PoolEntry, error) {
	totalWeight := 0
	for _, candidate := range pool.Entries {
		if _, used := usedNames[candidate.Name]; !used {
			totalWeight += candidate.Weight
		}
	}
	if totalWeight <= 0 {
		return PoolEntry{}, ErrBadPool
	}

	pick := g.rng.Intn(totalWeight)
	for _, candidate := range pool.Entries {
		if _, used := usedNames[candidate.Name]; used {
			continue
		}
		if pick < candidate.Weight {
			return candidate, nil
		}
		pick -= candidate.Weight
	}
	return PoolEntry{}, ErrBadPool
}

func rerollCost(lockCount int) int {
	return BaseRollCost + lockCount*LockSurcharge
}

func trimHistory(item *itemState, maxHistory int) {
	if maxHistory <= 0 {
		item.History = nil
		return
	}
	if len(item.History) <= maxHistory {
		return
	}

	trimmed := make([]RollRecord, maxHistory)
	copy(trimmed, item.History[len(item.History)-maxHistory:])
	item.History = trimmed
}

func validateSnapshot(state *snapshotData) error {
	if state.Pools == nil || state.Items == nil {
		return ErrBadSnapshot
	}
	if state.Limits.MaxItems < 0 || state.Limits.MaxLocks < 0 || state.Limits.MaxHistory < 0 || state.Limits.PreviewTTLMillis < 0 {
		return ErrBadSnapshot
	}
	if state.Seq < 0 {
		return ErrBadSnapshot
	}

	for tier, pool := range state.Pools {
		if pool.Tier != tier {
			return ErrBadSnapshot
		}
		if err := validatePool(pool); err != nil {
			return ErrBadSnapshot
		}
	}

	for id, item := range state.Items {
		if strings.TrimSpace(id) == "" || item.ID != id || strings.TrimSpace(item.ID) == "" || len(item.Entries) == 0 || item.Shards < 0 {
			return ErrBadSnapshot
		}
		if _, ok := state.Pools[item.Tier]; !ok {
			return ErrBadSnapshot
		}

		seenLocks := make(map[int]struct{}, len(item.Locks))
		for _, idx := range item.Locks {
			if idx < 0 || idx >= len(item.Entries) {
				return ErrBadSnapshot
			}
			if _, repeated := seenLocks[idx]; repeated {
				return ErrBadSnapshot
			}
			seenLocks[idx] = struct{}{}
		}
		if len(item.Locks) > state.Limits.MaxLocks {
			return ErrBadSnapshot
		}
		if state.Limits.MaxHistory == 0 && len(item.History) != 0 {
			return ErrBadSnapshot
		}
		if state.Limits.MaxHistory > 0 && len(item.History) > state.Limits.MaxHistory {
			return ErrBadSnapshot
		}

		var lastSeq int64
		for _, record := range item.History {
			if record.Seq <= lastSeq || record.Seq <= 0 {
				return ErrBadSnapshot
			}
			if len(record.Entries) != len(item.Entries) || record.Cost < 0 {
				return ErrBadSnapshot
			}
			lastSeq = record.Seq
		}
	}
	return nil
}

func clonePool(pool Pool) Pool {
	pool.Entries = append([]PoolEntry(nil), pool.Entries...)
	return pool
}

func cloneEntries(entries []Entry) []Entry {
	if entries == nil {
		return nil
	}
	cloned := make([]Entry, len(entries))
	copy(cloned, entries)
	return cloned
}

func cloneInts(values []int) []int {
	if values == nil {
		return nil
	}
	cloned := make([]int, len(values))
	copy(cloned, values)
	return cloned
}

func cloneRoll(roll Roll) Roll {
	roll.Entries = cloneEntries(roll.Entries)
	roll.Locked = cloneInts(roll.Locked)
	return roll
}

func cloneHistory(history []RollRecord) []RollRecord {
	cloned := make([]RollRecord, len(history))
	for i, record := range history {
		cloned[i] = record
		cloned[i].Entries = cloneEntries(record.Entries)
	}
	return cloned
}
