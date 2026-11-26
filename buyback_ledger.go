// Package task keeps the buyback ledger of an item shop: sold batches, their
// price snapshot, per player slot capacity, timed buyback and snapshots.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Errors reported by the ledger.
var (
	ErrEmptySell        = errors.New("sell request carries no items")
	ErrInvalidCount     = errors.New("item count must be positive")
	ErrTooManyItems     = errors.New("sell request exceeds the batch limit")
	ErrBoundItem        = errors.New("bound items cannot be sold")
	ErrNoPrice          = errors.New("item has no market price")
	ErrInvalidPrice     = errors.New("market price must be positive")
	ErrInvalidSlot      = errors.New("slot index is out of range")
	ErrSlotEmpty        = errors.New("slot holds no buyback batch")
	ErrExpired          = errors.New("buyback window has closed")
	ErrBagFull          = errors.New("bag has no room for the batch")
	ErrInsufficientGold = errors.New("player cannot pay the buyback price")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Limits bounds the buyback slots of one player, the items accepted by one sell
// call, the length of the buyback window and the accepted unit price.
type Limits struct {
	SlotsPerPlayer  int
	MaxItemsPerSell int
	BuybackWindow   time.Duration
	MaxUnitPrice    int64
}

// ItemRef is one stack of items. Enhance carries the upgrade level so that two
// batches of the same item stay distinguishable after a buyback.
type ItemRef struct {
	ItemID  string
	Count   int
	Enhance int
	Bound   bool
}

// SellReceipt describes one accepted sell call.
type SellReceipt struct {
	Slots    []int
	Total    int64
	Replaced []BuybackEntry
}

// BuybackQuote is the priced view of one occupied slot.
type BuybackQuote struct {
	Slot      int
	Item      ItemRef
	Price     int64
	SoldAt    time.Time
	ExpiresAt time.Time
	Valid     bool
}

// BuybackEntry is one stored batch inside a player slot.
type BuybackEntry struct {
	Slot      int
	Item      ItemRef
	Price     int64
	SoldAt    time.Time
	ExpiresAt time.Time
}

// BuybackLedger owns the market price book, the per player slots, wallets, bag
// room and the buyback windows.
type BuybackLedger struct {
	limits Limits
}

// NewBuybackLedger builds a ledger with the given limits.
func NewBuybackLedger(limits Limits) *BuybackLedger {
	return &BuybackLedger{limits: limits}
}

// SetMarketPrice records the current unit price of one item.
func (l *BuybackLedger) SetMarketPrice(itemID string, unit int64) error {
	return errNotImplemented
}

// SetBagFree records how many bag slots a player still has.
func (l *BuybackLedger) SetBagFree(playerID string, slots int) error {
	return errNotImplemented
}

// SetWallet records the gold a player owns.
func (l *BuybackLedger) SetWallet(playerID string, gold int64) error {
	return errNotImplemented
}

// Wallet reports the gold a player owns.
func (l *BuybackLedger) Wallet(playerID string) int64 {
	return 0
}

// BagFree reports how many bag slots a player still has.
func (l *BuybackLedger) BagFree(playerID string) int {
	return 0
}

// Sell accepts one sell call and stores every item as its own slot batch.
func (l *BuybackLedger) Sell(playerID string, items []ItemRef, at time.Time) (SellReceipt, error) {
	return SellReceipt{}, errNotImplemented
}

// Quote prices one occupied slot.
func (l *BuybackLedger) Quote(playerID string, slot int) (BuybackQuote, error) {
	return BuybackQuote{}, errNotImplemented
}

// BuyBack charges the player and returns one batch to the bag.
func (l *BuybackLedger) BuyBack(playerID string, slot int, at time.Time) (ItemRef, error) {
	return ItemRef{}, errNotImplemented
}

// Expire drops every batch whose window closed at or before now.
func (l *BuybackLedger) Expire(now time.Time) (int, error) {
	return 0, errNotImplemented
}

// List returns the occupied slots of one player in slot order.
func (l *BuybackLedger) List(playerID string) ([]BuybackEntry, error) {
	return nil, errNotImplemented
}

// Spent reports how much gold a player paid for buybacks.
func (l *BuybackLedger) Spent(playerID string) int64 {
	return 0
}

// SlotsUsed counts the occupied slots of one player.
func (l *BuybackLedger) SlotsUsed(playerID string) (int, error) {
	return 0, errNotImplemented
}

// Snapshot serialises prices, wallets, bag room and every stored batch.
func (l *BuybackLedger) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *BuybackLedger) Restore(data []byte) error {
	return errNotImplemented
}
