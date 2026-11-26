// Package task owns the reroll of equipment extra attributes: locked entries
// that raise the price, a weighted pool per quality tier, a preview with a
// validity window and a commit that charges and replaces in one step.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Pricing anchors of one reroll.
const (
	BaseRollCost  = 10
	LockSurcharge = 15
)

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

// AttributeRerollGuard owns the pools, the items, the locks and the history.
type AttributeRerollGuard struct {
	limits Limits
	now    time.Time
	seed   int64
}

// NewAttributeRerollGuard builds a guard with the given caps, starting clock
// and pseudo random seed.
func NewAttributeRerollGuard(limits Limits, now time.Time, seed int64) *AttributeRerollGuard {
	return &AttributeRerollGuard{limits: limits, now: now, seed: seed}
}

// SetPool records the weighted pool of one quality tier.
func (g *AttributeRerollGuard) SetPool(pool Pool) error {
	return errNotImplemented
}

// RegisterItem records one piece of equipment and its current attributes.
func (g *AttributeRerollGuard) RegisterItem(spec ItemSpec) error {
	return errNotImplemented
}

// GrantShards adds reroll shards to one item.
func (g *AttributeRerollGuard) GrantShards(itemID string, n int) error {
	return errNotImplemented
}

// Shards reads the shard balance of one item.
func (g *AttributeRerollGuard) Shards(itemID string) int {
	return 0
}

// Entries reads the current attributes of one item.
func (g *AttributeRerollGuard) Entries(itemID string) ([]Entry, error) {
	return nil, errNotImplemented
}

// Lock adds one index to the locked set of an item.
func (g *AttributeRerollGuard) Lock(itemID string, idx int) (Cost, error) {
	return Cost{}, errNotImplemented
}

// Unlock removes one index from the locked set of an item.
func (g *AttributeRerollGuard) Unlock(itemID string, idx int) error {
	return errNotImplemented
}

// UnlockAll clears the locked set of an item.
func (g *AttributeRerollGuard) UnlockAll(itemID string) error {
	return errNotImplemented
}

// Locked returns a copy of the locked indices of one item, ascending.
func (g *AttributeRerollGuard) Locked(itemID string) []int {
	return nil
}

// Preview builds a reroll proposal without touching the item.
func (g *AttributeRerollGuard) Preview(itemID string, locked []int) (Roll, error) {
	return Roll{}, errNotImplemented
}

// Apply commits one previewed reroll.
func (g *AttributeRerollGuard) Apply(itemID string, roll Roll) (Result, error) {
	return Result{}, errNotImplemented
}

// History returns a copy of the per item reroll history, oldest first.
func (g *AttributeRerollGuard) History(itemID string) []RollRecord {
	return nil
}

// AdvanceClock moves the guard clock forward.
func (g *AttributeRerollGuard) AdvanceClock(now time.Time) error {
	return errNotImplemented
}

// Snapshot serialises the whole guard state.
func (g *AttributeRerollGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *AttributeRerollGuard) Restore(data []byte) error {
	return errNotImplemented
}
