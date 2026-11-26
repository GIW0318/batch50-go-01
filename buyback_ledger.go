// Package task keeps the buyback ledger of an item shop: sold batches, their
// price snapshot, per player slot capacity, timed buyback and snapshots.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

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

const snapshotVersion = 1

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
	mu     sync.RWMutex
	limits Limits
	state  *ledgerState
}

type ledgerState struct {
	Prices  map[string]int64        `json:"prices"`
	Players map[string]*playerState `json:"players"`
}

type playerState struct {
	Wallet  int64          `json:"wallet"`
	BagFree int            `json:"bag_free"`
	Spent   int64          `json:"spent"`
	NextSeq uint64         `json:"next_seq"`
	Slots   []*storedBatch `json:"-"`
}

type storedBatch struct {
	Entry BuybackEntry
	Seq   uint64
}

type snapshotData struct {
	Version int                       `json:"version"`
	Prices  map[string]int64          `json:"prices"`
	Players map[string]snapshotPlayer `json:"players"`
}

type snapshotPlayer struct {
	Wallet  int64           `json:"wallet"`
	BagFree int             `json:"bag_free"`
	Spent   int64           `json:"spent"`
	NextSeq uint64          `json:"next_seq"`
	Entries []snapshotEntry `json:"entries"`
}

type snapshotEntry struct {
	Slot      int       `json:"slot"`
	Seq       uint64    `json:"seq"`
	Item      ItemRef   `json:"item"`
	Price     int64     `json:"price"`
	SoldAt    time.Time `json:"sold_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewBuybackLedger builds a ledger with the given limits.
func NewBuybackLedger(limits Limits) *BuybackLedger {
	if limits.SlotsPerPlayer < 0 {
		limits.SlotsPerPlayer = 0
	}
	return &BuybackLedger{
		limits: limits,
		state: &ledgerState{
			Prices:  make(map[string]int64),
			Players: make(map[string]*playerState),
		},
	}
}

func (l *BuybackLedger) slotCount() int {
	return max(l.limits.SlotsPerPlayer, 0)
}

func (s *ledgerState) player(playerID string, slots int) *playerState {
	p := s.Players[playerID]
	if p == nil {
		p = &playerState{Slots: make([]*storedBatch, slots)}
		s.Players[playerID] = p
	}
	return p
}

// SetMarketPrice records the current unit price of one item.
func (l *BuybackLedger) SetMarketPrice(itemID string, unit int64) error {
	if unit <= 0 {
		return ErrInvalidPrice
	}
	if unit > l.limits.MaxUnitPrice {
		return ErrInvalidPrice
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.state.Prices[itemID] = unit
	return nil
}

// SetBagFree records how many bag slots a player still has.
func (l *BuybackLedger) SetBagFree(playerID string, slots int) error {
	if slots < 0 {
		return fmt.Errorf("bag free slots must be non-negative, got %d", slots)
	}

	count := l.slotCount()
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.state.player(playerID, count)
	p.BagFree = slots
	return nil
}

// SetWallet records the gold a player owns.
func (l *BuybackLedger) SetWallet(playerID string, gold int64) error {
	count := l.slotCount()
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.state.player(playerID, count)
	p.Wallet = gold
	return nil
}

// Wallet reports the gold a player owns.
func (l *BuybackLedger) Wallet(playerID string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if p := l.state.Players[playerID]; p != nil {
		return p.Wallet
	}
	return 0
}

// BagFree reports how many bag slots a player still has.
func (l *BuybackLedger) BagFree(playerID string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if p := l.state.Players[playerID]; p != nil {
		return p.BagFree
	}
	return 0
}

// Sell accepts one sell call and stores every item as its own slot batch.
func (l *BuybackLedger) Sell(playerID string, items []ItemRef, at time.Time) (SellReceipt, error) {
	if len(items) == 0 {
		return SellReceipt{}, ErrEmptySell
	}
	if len(items) > l.limits.MaxItemsPerSell {
		return SellReceipt{}, ErrTooManyItems
	}

	planned := make([]BuybackEntry, len(items))
	var total int64

	l.mu.Lock()
	defer l.mu.Unlock()

	for i, item := range items {
		if item.Count <= 0 {
			return SellReceipt{}, ErrInvalidCount
		}
		if item.Bound {
			return SellReceipt{}, ErrBoundItem
		}

		unit, ok := l.state.Prices[item.ItemID]
		if !ok || unit <= 0 || unit > l.limits.MaxUnitPrice {
			return SellReceipt{}, ErrNoPrice
		}

		price, err := multiplyPrice(unit, item.Count)
		if err != nil {
			return SellReceipt{}, err
		}
		total, err = addPrice(total, price)
		if err != nil {
			return SellReceipt{}, err
		}

		planned[i] = BuybackEntry{
			Item:      item,
			Price:     price,
			SoldAt:    at,
			ExpiresAt: at.Add(l.limits.BuybackWindow),
		}
	}

	slotCount := l.slotCount()
	p := l.state.player(playerID, slotCount)
	receipt := SellReceipt{Total: total}

	for i := range planned {
		slot := firstFreeSlot(p.Slots)
		if slot < 0 {
			if len(p.Slots) == 0 {
				return SellReceipt{}, ErrInvalidSlot
			}

			oldest := oldestSlot(p.Slots)
			evicted := p.Slots[oldest].Entry
			receipt.Replaced = append(receipt.Replaced, evicted)
			slot = oldest
		}

		p.NextSeq++
		planned[i].Slot = slot
		p.Slots[slot] = &storedBatch{Entry: planned[i], Seq: p.NextSeq}
		receipt.Slots = append(receipt.Slots, slot)
	}

	return receipt, nil
}

func multiplyPrice(unit int64, count int) (int64, error) {
	if count <= 0 {
		return 0, ErrInvalidCount
	}
	if unit <= 0 {
		return 0, ErrNoPrice
	}

	stacks := int64(count)
	if stacks > math.MaxInt64/unit {
		return 0, fmt.Errorf("buyback price overflow: %d x %d", unit, count)
	}
	return unit * stacks, nil
}

func addPrice(left, right int64) (int64, error) {
	if right > math.MaxInt64-left {
		return 0, fmt.Errorf("buyback total overflow: %d + %d", left, right)
	}
	return left + right, nil
}

func firstFreeSlot(slots []*storedBatch) int {
	for i, batch := range slots {
		if batch == nil {
			return i
		}
	}
	return -1
}

func oldestSlot(slots []*storedBatch) int {
	oldest := 0
	for i := 1; i < len(slots); i++ {
		if isOlder(slots[i], slots[oldest]) {
			oldest = i
		}
	}
	return oldest
}

func isOlder(candidate, current *storedBatch) bool {
	if candidate.Entry.SoldAt.Before(current.Entry.SoldAt) {
		return true
	}
	if current.Entry.SoldAt.Before(candidate.Entry.SoldAt) {
		return false
	}
	if candidate.Seq != current.Seq {
		return candidate.Seq < current.Seq
	}
	return candidate.Entry.Slot < current.Entry.Slot
}

// Quote prices one occupied slot.
func (l *BuybackLedger) Quote(playerID string, slot int) (BuybackQuote, error) {
	if slot < 0 || slot >= l.slotCount() {
		return BuybackQuote{}, ErrInvalidSlot
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	p := l.state.Players[playerID]
	if p == nil || slot >= len(p.Slots) || p.Slots[slot] == nil {
		return BuybackQuote{}, ErrSlotEmpty
	}

	entry := p.Slots[slot].Entry
	return BuybackQuote{
		Slot:      entry.Slot,
		Item:      entry.Item,
		Price:     entry.Price,
		SoldAt:    entry.SoldAt,
		ExpiresAt: entry.ExpiresAt,
		Valid:     true,
	}, nil
}

// BuyBack charges the player and returns one batch to the bag.
func (l *BuybackLedger) BuyBack(playerID string, slot int, at time.Time) (ItemRef, error) {
	if slot < 0 || slot >= l.slotCount() {
		return ItemRef{}, ErrInvalidSlot
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	p := l.state.Players[playerID]
	if p == nil || slot >= len(p.Slots) || p.Slots[slot] == nil {
		return ItemRef{}, ErrSlotEmpty
	}

	batch := p.Slots[slot]
	entry := batch.Entry

	if !at.Before(entry.ExpiresAt) {
		return ItemRef{}, ErrExpired
	}
	if p.Wallet < entry.Price {
		return ItemRef{}, ErrInsufficientGold
	}
	if p.BagFree <= 0 {
		return ItemRef{}, ErrBagFull
	}

	newSpent, err := addPrice(p.Spent, entry.Price)
	if err != nil {
		return ItemRef{}, err
	}

	p.Wallet -= entry.Price
	p.Spent = newSpent
	p.BagFree--
	p.Slots[slot] = nil
	return entry.Item, nil
}

// Expire drops every batch whose window closed at or before now.
func (l *BuybackLedger) Expire(now time.Time) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	purged := 0
	for _, p := range l.state.Players {
		for slot, batch := range p.Slots {
			if batch != nil && !now.Before(batch.Entry.ExpiresAt) {
				p.Slots[slot] = nil
				purged++
			}
		}
	}
	return purged, nil
}

// List returns the occupied slots of one player in slot order.
func (l *BuybackLedger) List(playerID string) ([]BuybackEntry, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	entries := make([]BuybackEntry, 0, l.slotCount())
	p := l.state.Players[playerID]
	if p == nil {
		return entries, nil
	}

	for _, batch := range p.Slots {
		if batch != nil {
			entries = append(entries, batch.Entry)
		}
	}
	return entries, nil
}

// Spent reports how much gold a player paid for buybacks.
func (l *BuybackLedger) Spent(playerID string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if p := l.state.Players[playerID]; p != nil {
		return p.Spent
	}
	return 0
}

// SlotsUsed counts the occupied slots of one player.
func (l *BuybackLedger) SlotsUsed(playerID string) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	used := 0
	if p := l.state.Players[playerID]; p != nil {
		for _, batch := range p.Slots {
			if batch != nil {
				used++
			}
		}
	}
	return used, nil
}

// Snapshot serialises prices, wallets, bag room and every stored batch.
func (l *BuybackLedger) Snapshot() ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	data := snapshotData{
		Version: snapshotVersion,
		Prices:  make(map[string]int64, len(l.state.Prices)),
		Players: make(map[string]snapshotPlayer, len(l.state.Players)),
	}

	for itemID, price := range l.state.Prices {
		data.Prices[itemID] = price
	}

	for playerID, p := range l.state.Players {
		playerSnapshot := snapshotPlayer{
			Wallet:  p.Wallet,
			BagFree: p.BagFree,
			Spent:   p.Spent,
			NextSeq: p.NextSeq,
			Entries: make([]snapshotEntry, 0, len(p.Slots)),
		}

		for _, batch := range p.Slots {
			if batch == nil {
				continue
			}
			playerSnapshot.Entries = append(playerSnapshot.Entries, snapshotEntry{
				Slot:      batch.Entry.Slot,
				Seq:       batch.Seq,
				Item:      batch.Entry.Item,
				Price:     batch.Entry.Price,
				SoldAt:    batch.Entry.SoldAt,
				ExpiresAt: batch.Entry.ExpiresAt,
			})
		}

		data.Players[playerID] = playerSnapshot
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *BuybackLedger) Restore(data []byte) error {
	var payload snapshotData
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&payload); err != nil {
		return ErrBadSnapshot
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrBadSnapshot
	}

	next, err := stateFromSnapshot(&payload, l.limits)
	if err != nil {
		return err
	}

	l.mu.Lock()
	l.state = next
	l.mu.Unlock()
	return nil
}

func stateFromSnapshot(payload *snapshotData, limits Limits) (*ledgerState, error) {
	if payload.Version != snapshotVersion {
		return nil, ErrBadSnapshot
	}

	next := &ledgerState{
		Prices:  make(map[string]int64, len(payload.Prices)),
		Players: make(map[string]*playerState, len(payload.Players)),
	}

	for itemID, price := range payload.Prices {
		if price <= 0 || price > limits.MaxUnitPrice {
			return nil, ErrBadSnapshot
		}
		next.Prices[itemID] = price
	}

	slotCount := max(limits.SlotsPerPlayer, 0)

	for playerID, playerData := range payload.Players {
		if playerData.BagFree < 0 || playerData.Spent < 0 {
			return nil, ErrBadSnapshot
		}

		p := &playerState{
			Wallet:  playerData.Wallet,
			BagFree: playerData.BagFree,
			Spent:   playerData.Spent,
			NextSeq: playerData.NextSeq,
			Slots:   make([]*storedBatch, slotCount),
		}

		seenSlots := make(map[int]struct{}, len(playerData.Entries))
		maxSeq := playerData.NextSeq
		for _, entryData := range playerData.Entries {
			if entryData.Slot < 0 || entryData.Slot >= slotCount {
				return nil, ErrBadSnapshot
			}
			if _, exists := seenSlots[entryData.Slot]; exists {
				return nil, ErrBadSnapshot
			}
			if entryData.Item.Count <= 0 || entryData.Price <= 0 {
				return nil, ErrBadSnapshot
			}

			seenSlots[entryData.Slot] = struct{}{}
			entry := BuybackEntry{
				Slot:      entryData.Slot,
				Item:      entryData.Item,
				Price:     entryData.Price,
				SoldAt:    entryData.SoldAt,
				ExpiresAt: entryData.ExpiresAt,
			}
			p.Slots[entryData.Slot] = &storedBatch{Entry: entry, Seq: entryData.Seq}
			maxSeq = max(maxSeq, entryData.Seq)
		}
		p.NextSeq = maxSeq
		next.Players[playerID] = p
	}

	return next, nil
}
