// Package task owns the equipment salvage ledger: tier tables, per item
// yields, batch settlement, bag accounting and restart recovery.
package task

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Errors reported by the salvage ledger.
var (
	ErrBadPlayer     = errors.New("player id is required")
	ErrBadItem       = errors.New("item id is required")
	ErrUnknownTier   = errors.New("no yield table covers this item")
	ErrBadTable      = errors.New("yield table definition is invalid")
	ErrBoundItem     = errors.New("bound equipment cannot be salvaged")
	ErrQuestItem     = errors.New("quest item cannot be salvaged")
	ErrEmptyBatch    = errors.New("salvage batch is empty")
	ErrBatchTooLarge = errors.New("salvage batch exceeds the batch limit")
	ErrBadLimit      = errors.New("limit must be positive")
	ErrBagFull       = errors.New("bag cannot hold the whole batch")
	ErrLimit         = errors.New("salvage table limit reached")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// ItemRef identifies one equipment instance handed to the salvager.
type ItemRef struct {
	ID    string
	Tier  string
	Plus  int
	Bound bool
	Quest bool
}

// Yield is one material stack produced by a salvage step.
type Yield struct {
	Material string
	Count    int
}

// Table is the yield definition of one tier at one enhancement level.
type Table struct {
	Plus      int
	Materials []Yield
}

// BatchResult is the outcome of one accepted batch.
type BatchResult struct {
	Player string
	Seq    int64
	Items  int
	Yields []Yield
}

// BatchRecord is the history entry of one accepted batch.
type BatchRecord struct {
	Seq    int64
	Player string
	Items  []string
	Yields []Yield
}

// Limits bounds the ledger tables.
type Limits struct {
	MaxPlayers int `json:"max_players"`
	MaxBatch   int `json:"max_batch"`
	MaxHistory int `json:"max_history"`
}

// SalvageYieldLedger owns yield tables, salvage history, bags and the
// already salvaged set.
type SalvageYieldLedger struct {
	mu sync.Mutex

	limits      Limits
	batchLimit  int
	bagCapacity int

	tables   map[string]map[int]Table
	history  map[string][]BatchRecord
	bags     map[string]map[string]int
	salvaged map[string]struct{}
	nextSeq  int64
}

const snapshotVersion = 1

type snapshotTable struct {
	Tier  string `json:"tier"`
	Table Table  `json:"table"`
}

type ledgerSnapshot struct {
	Version     int                       `json:"version"`
	Limits      Limits                    `json:"limits"`
	BatchLimit  int                       `json:"batch_limit"`
	BagCapacity int                       `json:"bag_capacity"`
	Seq         int64                     `json:"seq"`
	Tables      []snapshotTable           `json:"tables"`
	History     map[string][]BatchRecord  `json:"history"`
	Bags        map[string]map[string]int `json:"bags"`
	Salvaged    []string                  `json:"salvaged"`
}

// NewSalvageYieldLedger builds a ledger with the given caps.
func NewSalvageYieldLedger(limits Limits) *SalvageYieldLedger {
	ledger := &SalvageYieldLedger{
		limits:   limits,
		tables:   make(map[string]map[int]Table),
		history:  make(map[string][]BatchRecord),
		bags:     make(map[string]map[string]int),
		salvaged: make(map[string]struct{}),
	}
	if limits.MaxBatch > 0 {
		ledger.batchLimit = limits.MaxBatch
	}
	return ledger
}

// SetTable registers the yield table of one tier at one enhancement level.
func (l *SalvageYieldLedger) SetTable(tier string, table Table) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if strings.TrimSpace(tier) == "" {
		return ErrBadTable
	}
	normalized, err := normalizeTable(table)
	if err != nil {
		return err
	}
	if levels := l.tables[tier]; levels != nil {
		if _, exists := levels[normalized.Plus]; exists {
			return ErrBadTable
		}
	} else {
		levels = make(map[int]Table)
		l.tables[tier] = levels
	}
	l.tables[tier][normalized.Plus] = normalized
	return nil
}

// YieldOf resolves the materials of one equipment instance.
func (l *SalvageYieldLedger) YieldOf(item ItemRef) ([]Yield, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.yieldOfLocked(item)
}

// Preview resolves the summed materials of a batch without recording it.
func (l *SalvageYieldLedger) Preview(items []ItemRef) ([]Yield, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(items) == 0 {
		return nil, ErrEmptyBatch
	}
	if l.batchLimit > 0 && len(items) > l.batchLimit {
		return nil, ErrBatchTooLarge
	}
	_, yields, err := l.resolveBatchLocked(items)
	if err != nil {
		return nil, err
	}
	return yields, nil
}

// Salvage consumes a batch of equipment and records the outcome.
func (l *SalvageYieldLedger) Salvage(player string, items []ItemRef) (BatchResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if strings.TrimSpace(player) == "" {
		return BatchResult{}, ErrBadPlayer
	}
	if len(items) == 0 {
		return BatchResult{}, ErrEmptyBatch
	}
	if l.batchLimit > 0 && len(items) > l.batchLimit {
		return BatchResult{}, ErrBatchTooLarge
	}

	unique, yields, err := l.resolveBatchLocked(items)
	if err != nil {
		return BatchResult{}, err
	}
	if len(unique) == 0 {
		return BatchResult{Player: player, Yields: emptyYields()}, nil
	}

	if l.limits.MaxPlayers > 0 && !l.playerKnownLocked(player) && l.playerCountLocked() >= l.limits.MaxPlayers {
		return BatchResult{}, ErrLimit
	}
	if l.limits.MaxHistory > 0 && len(l.history[player]) >= l.limits.MaxHistory {
		return BatchResult{}, ErrLimit
	}

	bag := cloneBag(l.bags[player])
	for _, yield := range yields {
		bag[yield.Material] += yield.Count
	}
	if l.bagCapacity > 0 && bagUnits(bag) > l.bagCapacity {
		return BatchResult{}, ErrBagFull
	}

	seq := l.nextSeq + 1
	recordItems := make([]string, 0, len(unique))
	for _, item := range unique {
		recordItems = append(recordItems, item.ID)
	}

	l.nextSeq = seq
	l.bags[player] = bag
	l.history[player] = append(l.history[player], BatchRecord{
		Seq:    seq,
		Player: player,
		Items:  recordItems,
		Yields: cloneYields(yields),
	})
	for _, item := range unique {
		l.salvaged[item.ID] = struct{}{}
	}

	return BatchResult{
		Player: player,
		Seq:    seq,
		Items:  len(unique),
		Yields: cloneYields(yields),
	}, nil
}

// History returns the accepted batches of one player, oldest first.
func (l *SalvageYieldLedger) History(player string) []BatchRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	records := l.history[player]
	out := make([]BatchRecord, 0, len(records))
	for _, record := range records {
		out = append(out, BatchRecord{
			Seq:    record.Seq,
			Player: record.Player,
			Items:  append([]string(nil), record.Items...),
			Yields: cloneYields(record.Yields),
		})
	}
	return out
}

// SetBatchLimit caps how many items one batch may carry.
func (l *SalvageYieldLedger) SetBatchLimit(n int) error {
	if n <= 0 {
		return ErrBadLimit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batchLimit = n
	return nil
}

// SetBagCapacity caps how many material units one bag may hold.
func (l *SalvageYieldLedger) SetBagCapacity(n int) error {
	if n <= 0 {
		return ErrBadLimit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bagCapacity = n
	return nil
}

// Bag returns a copy of the material counts of one player.
func (l *SalvageYieldLedger) Bag(player string) map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return cloneBag(l.bags[player])
}

// AlreadySalvaged tells whether an equipment instance was consumed before.
func (l *SalvageYieldLedger) AlreadySalvaged(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	_, exists := l.salvaged[id]
	return exists
}

// Snapshot serialises tables, history, bags and the salvaged set.
func (l *SalvageYieldLedger) Snapshot() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	tiers := make([]string, 0, len(l.tables))
	for tier := range l.tables {
		tiers = append(tiers, tier)
	}
	sort.Strings(tiers)

	tables := make([]snapshotTable, 0)
	for _, tier := range tiers {
		levels := make([]int, 0, len(l.tables[tier]))
		for plus := range l.tables[tier] {
			levels = append(levels, plus)
		}
		sort.Ints(levels)
		for _, plus := range levels {
			table := l.tables[tier][plus]
			tables = append(tables, snapshotTable{
				Tier: tier,
				Table: Table{
					Plus:      table.Plus,
					Materials: cloneYields(table.Materials),
				},
			})
		}
	}

	history := make(map[string][]BatchRecord, len(l.history))
	for player, records := range l.history {
		history[player] = make([]BatchRecord, 0, len(records))
		for _, record := range records {
			history[player] = append(history[player], BatchRecord{
				Seq:    record.Seq,
				Player: record.Player,
				Items:  append([]string(nil), record.Items...),
				Yields: cloneYields(record.Yields),
			})
		}
	}

	bags := make(map[string]map[string]int, len(l.bags))
	for player, bag := range l.bags {
		bags[player] = cloneBag(bag)
	}

	salvaged := make([]string, 0, len(l.salvaged))
	for id := range l.salvaged {
		salvaged = append(salvaged, id)
	}
	sort.Strings(salvaged)

	payload, err := json.Marshal(ledgerSnapshot{
		Version:     snapshotVersion,
		Limits:      l.limits,
		BatchLimit:  l.batchLimit,
		BagCapacity: l.bagCapacity,
		Seq:         l.nextSeq,
		Tables:      tables,
		History:     history,
		Bags:        bags,
		Salvaged:    salvaged,
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *SalvageYieldLedger) Restore(data []byte) error {
	var snapshot ledgerSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ErrBadSnapshot
	}
	tables, err := validateSnapshot(&snapshot)
	if err != nil {
		return ErrBadSnapshot
	}

	history := make(map[string][]BatchRecord, len(snapshot.History))
	for player, records := range snapshot.History {
		history[player] = make([]BatchRecord, 0, len(records))
		for _, record := range records {
			history[player] = append(history[player], BatchRecord{
				Seq:    record.Seq,
				Player: record.Player,
				Items:  append([]string(nil), record.Items...),
				Yields: cloneYields(record.Yields),
			})
		}
	}
	bags := make(map[string]map[string]int, len(snapshot.Bags))
	for player, bag := range snapshot.Bags {
		bags[player] = cloneBag(bag)
	}
	salvaged := make(map[string]struct{}, len(snapshot.Salvaged))
	for _, id := range snapshot.Salvaged {
		salvaged[id] = struct{}{}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = snapshot.Limits
	l.batchLimit = snapshot.BatchLimit
	l.bagCapacity = snapshot.BagCapacity
	l.tables = tables
	l.history = history
	l.bags = bags
	l.salvaged = salvaged
	l.nextSeq = snapshot.Seq
	return nil
}

func (l *SalvageYieldLedger) yieldOfLocked(item ItemRef) ([]Yield, error) {
	if strings.TrimSpace(item.ID) == "" {
		return nil, ErrBadItem
	}
	if item.Bound {
		return nil, ErrBoundItem
	}
	if item.Quest {
		return nil, ErrQuestItem
	}

	levels, exists := l.tables[item.Tier]
	if !exists || len(levels) == 0 {
		return nil, ErrUnknownTier
	}
	bestPlus := -1
	for plus := range levels {
		if plus <= item.Plus && plus > bestPlus {
			bestPlus = plus
		}
	}
	if bestPlus < 0 {
		return nil, ErrUnknownTier
	}
	return cloneYields(levels[bestPlus].Materials), nil
}

func (l *SalvageYieldLedger) resolveBatchLocked(items []ItemRef) ([]ItemRef, []Yield, error) {
	seen := make(map[string]struct{}, len(items))
	unique := make([]ItemRef, 0, len(items))
	sums := make(map[string]int)

	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			return nil, nil, ErrBadItem
		}
		if _, exists := l.salvaged[item.ID]; exists {
			continue
		}
		itemYields, err := l.yieldOfLocked(item)
		if err != nil {
			return nil, nil, err
		}
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		unique = append(unique, item)
		for _, yield := range itemYields {
			sums[yield.Material] += yield.Count
		}
	}

	return unique, sortedYields(sums), nil
}

func (l *SalvageYieldLedger) playerKnownLocked(player string) bool {
	if _, exists := l.history[player]; exists {
		return true
	}
	_, exists := l.bags[player]
	return exists
}

func (l *SalvageYieldLedger) playerCountLocked() int {
	players := make(map[string]struct{}, len(l.history)+len(l.bags))
	for player := range l.history {
		players[player] = struct{}{}
	}
	for player := range l.bags {
		players[player] = struct{}{}
	}
	return len(players)
}

func normalizeTable(table Table) (Table, error) {
	if table.Plus < 0 || len(table.Materials) == 0 {
		return Table{}, ErrBadTable
	}
	sums := make(map[string]int, len(table.Materials))
	for _, material := range table.Materials {
		if strings.TrimSpace(material.Material) == "" || material.Count <= 0 {
			return Table{}, ErrBadTable
		}
		if sums[material.Material] > maxInt()-material.Count {
			return Table{}, ErrBadTable
		}
		sums[material.Material] += material.Count
	}
	return Table{Plus: table.Plus, Materials: sortedYields(sums)}, nil
}

func sortedYields(sums map[string]int) []Yield {
	materials := make([]string, 0, len(sums))
	for material, count := range sums {
		if count > 0 {
			materials = append(materials, material)
		}
	}
	sort.Strings(materials)
	yields := make([]Yield, 0, len(materials))
	for _, material := range materials {
		yields = append(yields, Yield{Material: material, Count: sums[material]})
	}
	return yields
}

func cloneYields(yields []Yield) []Yield {
	if len(yields) == 0 {
		return emptyYields()
	}
	return append([]Yield(nil), yields...)
}

func emptyYields() []Yield {
	return make([]Yield, 0)
}

func cloneBag(bag map[string]int) map[string]int {
	out := make(map[string]int, len(bag))
	for material, count := range bag {
		out[material] = count
	}
	return out
}

func bagUnits(bag map[string]int) int {
	total := 0
	for _, count := range bag {
		if count > 0 && total > maxInt()-count {
			return maxInt()
		}
		total += count
	}
	return total
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func validateSnapshot(snapshot *ledgerSnapshot) (map[string]map[int]Table, error) {
	if snapshot.Version != snapshotVersion {
		return nil, ErrBadSnapshot
	}
	if snapshot.Limits.MaxPlayers < 0 || snapshot.Limits.MaxBatch < 0 || snapshot.Limits.MaxHistory < 0 ||
		snapshot.BatchLimit < 0 || snapshot.BagCapacity < 0 || snapshot.Seq < 0 {
		return nil, ErrBadSnapshot
	}

	tables := make(map[string]map[int]Table)
	levelKeys := make(map[string]map[int]struct{})
	for _, entry := range snapshot.Tables {
		if strings.TrimSpace(entry.Tier) == "" {
			return nil, ErrBadSnapshot
		}
		normalized, err := normalizeTable(entry.Table)
		if err != nil {
			return nil, ErrBadSnapshot
		}
		if levelKeys[entry.Tier] == nil {
			levelKeys[entry.Tier] = make(map[int]struct{})
			tables[entry.Tier] = make(map[int]Table)
		}
		if _, exists := levelKeys[entry.Tier][normalized.Plus]; exists {
			return nil, ErrBadSnapshot
		}
		levelKeys[entry.Tier][normalized.Plus] = struct{}{}
		tables[entry.Tier][normalized.Plus] = normalized
	}

	salvaged := make(map[string]struct{}, len(snapshot.Salvaged))
	for _, id := range snapshot.Salvaged {
		if strings.TrimSpace(id) == "" {
			return nil, ErrBadSnapshot
		}
		if _, exists := salvaged[id]; exists {
			return nil, ErrBadSnapshot
		}
		salvaged[id] = struct{}{}
	}

	expectedBags := make(map[string]map[string]int)
	players := make(map[string]struct{})
	sequences := make(map[int64]struct{})
	historyItems := make(map[string]struct{})
	var maxSequence int64
	for player, records := range snapshot.History {
		if strings.TrimSpace(player) == "" {
			return nil, ErrBadSnapshot
		}
		if len(records) == 0 {
			return nil, ErrBadSnapshot
		}
		players[player] = struct{}{}
		if snapshot.Limits.MaxHistory > 0 && len(records) > snapshot.Limits.MaxHistory {
			return nil, ErrBadSnapshot
		}
		var previous int64
		for _, record := range records {
			if record.Seq <= 0 || record.Seq <= previous || record.Player != player {
				return nil, ErrBadSnapshot
			}
			if _, duplicate := sequences[record.Seq]; duplicate {
				return nil, ErrBadSnapshot
			}
			sequences[record.Seq] = struct{}{}
			if record.Seq > maxSequence {
				maxSequence = record.Seq
			}
			previous = record.Seq

			if len(record.Items) == 0 {
				return nil, ErrBadSnapshot
			}
			items := make(map[string]struct{}, len(record.Items))
			for _, id := range record.Items {
				if strings.TrimSpace(id) == "" {
					return nil, ErrBadSnapshot
				}
				if _, duplicate := items[id]; duplicate {
					return nil, ErrBadSnapshot
				}
				items[id] = struct{}{}
				if _, duplicate := historyItems[id]; duplicate {
					return nil, ErrBadSnapshot
				}
				historyItems[id] = struct{}{}
				if _, exists := salvaged[id]; !exists {
					return nil, ErrBadSnapshot
				}
			}
			if err := validateYields(record.Yields); err != nil {
				return nil, ErrBadSnapshot
			}
			if expectedBags[player] == nil {
				expectedBags[player] = make(map[string]int)
			}
			for _, yield := range record.Yields {
				expectedBags[player][yield.Material] += yield.Count
			}
		}
	}
	if len(historyItems) != len(salvaged) {
		return nil, ErrBadSnapshot
	}
	if snapshot.Seq != maxSequence {
		return nil, ErrBadSnapshot
	}

	for player, bag := range snapshot.Bags {
		if strings.TrimSpace(player) == "" {
			return nil, ErrBadSnapshot
		}
		if _, exists := expectedBags[player]; !exists {
			return nil, ErrBadSnapshot
		}
		players[player] = struct{}{}
		for material, count := range bag {
			if strings.TrimSpace(material) == "" || count <= 0 {
				return nil, ErrBadSnapshot
			}
		}
		if !equalCounts(bag, expectedBags[player]) {
			return nil, ErrBadSnapshot
		}
	}
	for player, expected := range expectedBags {
		if len(expected) == 0 {
			continue
		}
		if _, exists := snapshot.Bags[player]; !exists {
			return nil, ErrBadSnapshot
		}
	}
	if snapshot.Limits.MaxPlayers > 0 && len(players) > snapshot.Limits.MaxPlayers {
		return nil, ErrBadSnapshot
	}

	return tables, nil
}

func validateYields(yields []Yield) error {
	if len(yields) == 0 {
		return ErrBadSnapshot
	}
	previous := ""
	for _, yield := range yields {
		if strings.TrimSpace(yield.Material) == "" || yield.Count <= 0 || yield.Material <= previous {
			return ErrBadSnapshot
		}
		previous = yield.Material
	}
	return nil
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for material, count := range a {
		if b[material] != count {
			return false
		}
	}
	return true
}
