// Package task owns the gem sockets of player equipment: colour matching
// against the socket, extraction per tier policy, set counting recomputed from
// the sockets and a stats summary committed in one step.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// Socket colours understood by the guard.
const (
	ColorRed    = "red"
	ColorBlue   = "blue"
	ColorGreen  = "green"
	ColorYellow = "yellow"
)

// Extraction policies of one equipment tier.
const (
	ExtractReturn  = "return"
	ExtractDestroy = "destroy"
)

// Actions recorded in the per item history.
const (
	ActionInlay   = "inlay"
	ActionExtract = "extract"
)

// Bonus granted per active gem set.
const (
	SetBonusAttack  = 10
	SetBonusDefense = 5
	SetBonusCrit    = 5
)

// Errors returned by the guard.
var (
	ErrBlankPlayer    = errors.New("player id is required")
	ErrBadItem        = errors.New("item spec is invalid")
	ErrItemExists     = errors.New("item is already registered")
	ErrItemLimit      = errors.New("item registry limit reached")
	ErrUnknownItem    = errors.New("unknown item")
	ErrNotOwner       = errors.New("item belongs to another player")
	ErrSocketLimit    = errors.New("socket count exceeds the tier limit")
	ErrBadSocketRule  = errors.New("socket rule is invalid")
	ErrSocketIndex    = errors.New("socket index is out of range")
	ErrSocketLocked   = errors.New("socket is not open for this tier")
	ErrSocketOccupied = errors.New("socket already holds a gem")
	ErrSocketEmpty    = errors.New("socket holds no gem")
	ErrBadGem         = errors.New("gem spec is invalid")
	ErrGemExists      = errors.New("gem is already registered")
	ErrUnknownGem     = errors.New("unknown gem")
	ErrGemNotFound    = errors.New("player has no such gem")
	ErrColorMismatch  = errors.New("gem colour does not match the socket")
	ErrBadAmount      = errors.New("gem amount must be positive")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// GemSpec is the static description of one gem.
type GemSpec struct {
	ID           string `json:"id"`
	Color        string `json:"color"`
	SetID        string `json:"set_id"`
	Attack       int    `json:"attack"`
	Defense      int    `json:"defense"`
	Crit         int    `json:"crit"`
	SetThreshold int    `json:"set_threshold"`
}

// ItemSpec is the static description of one piece of equipment.
type ItemSpec struct {
	ID           string   `json:"id"`
	Owner        string   `json:"owner"`
	Tier         string   `json:"tier"`
	SocketColors []string `json:"socket_colors"`
}

// SocketRule is the per tier opening and extraction policy.
type SocketRule struct {
	Tier          string `json:"tier"`
	MaxSockets    int    `json:"max_sockets"`
	ExtractPolicy string `json:"extract_policy"`
}

// Socket is the projection of one socket.
type Socket struct {
	Index int    `json:"index"`
	Color string `json:"color"`
	Gem   string `json:"gem"`
}

// Stats is the attribute summary of one player.
type Stats struct {
	Attack     int `json:"attack"`
	Defense    int `json:"defense"`
	Crit       int `json:"crit"`
	ActiveSets int `json:"active_sets"`
}

// Result is the outcome of one successful inlay.
type Result struct {
	ItemID    string `json:"item_id"`
	Socket    int    `json:"socket"`
	Gem       string `json:"gem"`
	SetID     string `json:"set_id"`
	SetCount  int    `json:"set_count"`
	SetActive bool   `json:"set_active"`
	Stats     Stats  `json:"stats"`
	Seq       int64  `json:"seq"`
}

// Gem is the outcome of one extraction.
type Gem struct {
	ID        string `json:"id"`
	Color     string `json:"color"`
	SetID     string `json:"set_id"`
	Destroyed bool   `json:"destroyed"`
}

// InlayRecord is one entry of the per item history.
type InlayRecord struct {
	Seq       int64  `json:"seq"`
	Player    string `json:"player"`
	ItemID    string `json:"item_id"`
	Socket    int    `json:"socket"`
	Action    string `json:"action"`
	GemID     string `json:"gem_id"`
	SetID     string `json:"set_id"`
	Destroyed bool   `json:"destroyed"`
}

// Limits bounds the registry sizes and the per item history length.
type Limits struct {
	MaxItems   int `json:"max_items"`
	MaxHistory int `json:"max_history"`
}

// itemRecord is the mutable state of one registered item.
type itemRecord struct {
	spec    ItemSpec
	sockets []Socket
}

// SocketInlayGuard owns the item registry, the gem bag and the socket state.
// A single lock serialises every mutation, so a reader only ever observes the
// state before or after a whole inlay/extract transaction.
type SocketInlayGuard struct {
	mu      sync.RWMutex
	limits  Limits
	items   map[string]*itemRecord
	gems    map[string]GemSpec
	rules   map[string]SocketRule
	bags    map[string]map[string]int
	history map[string][]InlayRecord
	seq     int64
}

// NewSocketInlayGuard builds a guard with the given caps.
func NewSocketInlayGuard(limits Limits) *SocketInlayGuard {
	return &SocketInlayGuard{
		limits:  limits,
		items:   make(map[string]*itemRecord),
		gems:    make(map[string]GemSpec),
		rules:   make(map[string]SocketRule),
		bags:    make(map[string]map[string]int),
		history: make(map[string][]InlayRecord),
	}
}

func blank(s string) bool {
	return strings.TrimSpace(s) == ""
}

func cloneItemSpec(spec ItemSpec) ItemSpec {
	spec.SocketColors = append([]string(nil), spec.SocketColors...)
	return spec
}

// RegisterItem records one piece of equipment and its socket colours.
func (g *SocketInlayGuard) RegisterItem(spec ItemSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if blank(spec.ID) || blank(spec.Owner) || len(spec.SocketColors) == 0 {
		return ErrBadItem
	}
	for _, colour := range spec.SocketColors {
		if blank(colour) {
			return ErrBadItem
		}
	}
	if _, exists := g.items[spec.ID]; exists {
		return ErrItemExists
	}
	if len(g.items) >= g.limits.MaxItems {
		return ErrItemLimit
	}
	if rule, exists := g.rules[spec.Tier]; exists && len(spec.SocketColors) > rule.MaxSockets {
		return ErrSocketLimit
	}

	colours := append([]string(nil), spec.SocketColors...)
	sockets := make([]Socket, len(colours))
	for i, colour := range colours {
		sockets[i] = Socket{Index: i, Color: colour}
	}
	spec.SocketColors = colours
	g.items[spec.ID] = &itemRecord{spec: spec, sockets: sockets}
	return nil
}

// RegisterGem records one gem template.
func (g *SocketInlayGuard) RegisterGem(spec GemSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if blank(spec.ID) || blank(spec.Color) || spec.SetThreshold <= 0 {
		return ErrBadGem
	}
	if _, exists := g.gems[spec.ID]; exists {
		return ErrGemExists
	}
	g.gems[spec.ID] = spec
	return nil
}

// SetSocketRule records the opening and extraction policy of one tier.
func (g *SocketInlayGuard) SetSocketRule(tier string, rule SocketRule) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if blank(tier) {
		return ErrBadSocketRule
	}
	if rule.Tier != "" && rule.Tier != tier {
		return ErrBadSocketRule
	}
	if rule.MaxSockets < 0 {
		return ErrBadSocketRule
	}
	if rule.ExtractPolicy != ExtractReturn && rule.ExtractPolicy != ExtractDestroy {
		return ErrBadSocketRule
	}
	rule.Tier = tier
	g.rules[tier] = rule
	return nil
}

// GrantGem adds gems to the bag of one player.
func (g *SocketInlayGuard) GrantGem(player, gem string, n int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if n <= 0 {
		return ErrBadAmount
	}
	if blank(player) {
		return ErrBlankPlayer
	}
	if _, exists := g.gems[gem]; !exists {
		return ErrUnknownGem
	}
	bag := g.bags[player]
	if bag == nil {
		bag = make(map[string]int)
		g.bags[player] = bag
	}
	bag[gem] += n
	return nil
}

// GemCount reads how many copies of one gem the player still holds.
func (g *SocketInlayGuard) GemCount(player, gem string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.bags[player][gem]
}

// Inlay puts one gem of the bag into one socket.
func (g *SocketInlayGuard) Inlay(player, itemID string, socket int, gem string) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if blank(player) {
		return Result{}, ErrBlankPlayer
	}
	item, exists := g.items[itemID]
	if !exists {
		return Result{}, ErrUnknownItem
	}
	if item.spec.Owner != player {
		return Result{}, ErrNotOwner
	}
	if socket < 0 || socket >= len(item.sockets) {
		return Result{}, ErrSocketIndex
	}
	gemSpec, exists := g.gems[gem]
	if !exists {
		return Result{}, ErrUnknownGem
	}
	if rule, exists := g.rules[item.spec.Tier]; exists && socket >= rule.MaxSockets {
		return Result{}, ErrSocketLocked
	}
	if item.sockets[socket].Color != gemSpec.Color {
		return Result{}, ErrColorMismatch
	}
	if item.sockets[socket].Gem != "" {
		return Result{}, ErrSocketOccupied
	}
	if g.bags[player][gem] <= 0 {
		return Result{}, ErrGemNotFound
	}

	// All checks passed: commit the bag debit and the socket fill together.
	bag := g.bags[player]
	if bag[gem] == 1 {
		delete(bag, gem)
	} else {
		bag[gem]--
	}
	item.sockets[socket].Gem = gem
	g.seq++
	seq := g.seq

	stats, counts, thresholds := g.statsLocked(player)
	setCount := 0
	setActive := false
	if gemSpec.SetID != "" {
		setCount = counts[gemSpec.SetID]
		setActive = setCount >= thresholds[gemSpec.SetID]
	}
	g.recordLocked(InlayRecord{
		Seq:    seq,
		Player: player,
		ItemID: itemID,
		Socket: socket,
		Action: ActionInlay,
		GemID:  gem,
		SetID:  gemSpec.SetID,
	})
	return Result{
		ItemID:    itemID,
		Socket:    socket,
		Gem:       gem,
		SetID:     gemSpec.SetID,
		SetCount:  setCount,
		SetActive: setActive,
		Stats:     stats,
		Seq:       seq,
	}, nil
}

// Extract takes the gem out of one socket following the tier policy.
func (g *SocketInlayGuard) Extract(player, itemID string, socket int) (Gem, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if blank(player) {
		return Gem{}, ErrBlankPlayer
	}
	item, exists := g.items[itemID]
	if !exists {
		return Gem{}, ErrUnknownItem
	}
	if item.spec.Owner != player {
		return Gem{}, ErrNotOwner
	}
	if socket < 0 || socket >= len(item.sockets) {
		return Gem{}, ErrSocketIndex
	}
	gemID := item.sockets[socket].Gem
	if gemID == "" {
		return Gem{}, ErrSocketEmpty
	}
	gemSpec, exists := g.gems[gemID]
	if !exists {
		return Gem{}, ErrUnknownGem
	}

	destroyed := false
	if rule, exists := g.rules[item.spec.Tier]; exists && rule.ExtractPolicy == ExtractDestroy {
		destroyed = true
	}
	item.sockets[socket].Gem = ""
	if !destroyed {
		bag := g.bags[player]
		if bag == nil {
			bag = make(map[string]int)
			g.bags[player] = bag
		}
		bag[gemID]++
	}

	g.seq++
	seq := g.seq
	g.recordLocked(InlayRecord{
		Seq:       seq,
		Player:    player,
		ItemID:    itemID,
		Socket:    socket,
		Action:    ActionExtract,
		GemID:     gemID,
		SetID:     gemSpec.SetID,
		Destroyed: destroyed,
	})
	return Gem{
		ID:        gemID,
		Color:     gemSpec.Color,
		SetID:     gemSpec.SetID,
		Destroyed: destroyed,
	}, nil
}

// Sockets lists the sockets of one item, lowest index first.
func (g *SocketInlayGuard) Sockets(itemID string) ([]Socket, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	item, exists := g.items[itemID]
	if !exists {
		return nil, ErrUnknownItem
	}
	return append([]Socket(nil), item.sockets...), nil
}

// Summary recomputes the attribute summary of one player from its sockets.
func (g *SocketInlayGuard) Summary(player string) (Stats, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	stats, _, _ := g.statsLocked(player)
	return stats, nil
}

// History returns a copy of the per item inlay history, oldest first.
func (g *SocketInlayGuard) History(itemID string) []InlayRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]InlayRecord(nil), g.history[itemID]...)
}

// statsLocked recomputes gem contributions and set bonuses from live sockets.
func (g *SocketInlayGuard) statsLocked(player string) (Stats, map[string]int, map[string]int) {
	counts := make(map[string]int)
	thresholds := make(map[string]int)
	var stats Stats
	for _, item := range g.items {
		if item.spec.Owner != player {
			continue
		}
		for _, socket := range item.sockets {
			if socket.Gem == "" {
				continue
			}
			gem, exists := g.gems[socket.Gem]
			if !exists {
				continue
			}
			stats.Attack += gem.Attack
			stats.Defense += gem.Defense
			stats.Crit += gem.Crit
			if gem.SetID == "" {
				continue
			}
			counts[gem.SetID]++
			if threshold, exists := thresholds[gem.SetID]; !exists || gem.SetThreshold < threshold {
				thresholds[gem.SetID] = gem.SetThreshold
			}
		}
	}
	for setID, count := range counts {
		if count < thresholds[setID] {
			continue
		}
		stats.Attack += SetBonusAttack
		stats.Defense += SetBonusDefense
		stats.Crit += SetBonusCrit
		stats.ActiveSets++
	}
	return stats, counts, thresholds
}

// recordLocked appends one history entry and trims the oldest entries.
func (g *SocketInlayGuard) recordLocked(record InlayRecord) {
	max := g.limits.MaxHistory
	if max <= 0 {
		delete(g.history, record.ItemID)
		return
	}
	history := append(g.history[record.ItemID], record)
	if len(history) > max {
		history = history[len(history)-max:]
	}
	g.history[record.ItemID] = history
}

const (
	snapshotFormat  = "gsb.socket_inlay_guard.v1"
	snapshotVersion = 1
)

type snapshotItem struct {
	Spec    ItemSpec `json:"spec"`
	Sockets []Socket `json:"sockets"`
}

type snapshotPayload struct {
	Version int                       `json:"version"`
	Limits  Limits                    `json:"limits"`
	Seq     int64                     `json:"seq"`
	Items   map[string]snapshotItem   `json:"items"`
	Gems    map[string]GemSpec        `json:"gems"`
	Rules   map[string]SocketRule     `json:"rules"`
	Bags    map[string]map[string]int `json:"bags"`
	History map[string][]InlayRecord  `json:"history"`
}

type snapshotEnvelope struct {
	Format   string          `json:"format"`
	Checksum string          `json:"checksum"`
	Payload  json.RawMessage `json:"payload"`
}

// Snapshot serialises the whole guard state.
func (g *SocketInlayGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	payload := snapshotPayload{
		Version: snapshotVersion,
		Limits:  g.limits,
		Seq:     g.seq,
		Items:   make(map[string]snapshotItem, len(g.items)),
		Gems:    make(map[string]GemSpec, len(g.gems)),
		Rules:   make(map[string]SocketRule, len(g.rules)),
		Bags:    make(map[string]map[string]int, len(g.bags)),
		History: make(map[string][]InlayRecord, len(g.history)),
	}
	for id, item := range g.items {
		payload.Items[id] = snapshotItem{
			Spec:    cloneItemSpec(item.spec),
			Sockets: append([]Socket(nil), item.sockets...),
		}
	}
	for id, gem := range g.gems {
		payload.Gems[id] = gem
	}
	for tier, rule := range g.rules {
		payload.Rules[tier] = rule
	}
	for player, bag := range g.bags {
		copied := make(map[string]int, len(bag))
		for gem, count := range bag {
			copied[gem] = count
		}
		payload.Bags[player] = copied
	}
	for itemID, history := range g.history {
		payload.History[itemID] = append([]InlayRecord(nil), history...)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	envelope := snapshotEnvelope{
		Format:   snapshotFormat,
		Checksum: hex.EncodeToString(sum[:]),
		Payload:  data,
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SocketInlayGuard) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Format != snapshotFormat || len(envelope.Payload) == 0 {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Payload)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), envelope.Checksum) {
		return ErrBadSnapshot
	}
	var payload snapshotPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	restored, err := decodeSnapshot(payload)
	if err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = restored.limits
	g.items = restored.items
	g.gems = restored.gems
	g.rules = restored.rules
	g.bags = restored.bags
	g.history = restored.history
	g.seq = restored.seq
	return nil
}

// decodeSnapshot validates a payload and builds a detached guard state.
func decodeSnapshot(payload snapshotPayload) (*SocketInlayGuard, error) {
	restored := &SocketInlayGuard{
		limits:  payload.Limits,
		items:   make(map[string]*itemRecord, len(payload.Items)),
		gems:    make(map[string]GemSpec, len(payload.Gems)),
		rules:   make(map[string]SocketRule, len(payload.Rules)),
		bags:    make(map[string]map[string]int, len(payload.Bags)),
		history: make(map[string][]InlayRecord, len(payload.History)),
		seq:     payload.Seq,
	}

	for id, gem := range payload.Gems {
		if blank(id) || gem.ID != id || blank(gem.Color) || gem.SetThreshold <= 0 {
			return nil, ErrBadSnapshot
		}
		restored.gems[id] = gem
	}
	for tier, rule := range payload.Rules {
		if blank(tier) || rule.Tier != tier || rule.MaxSockets < 0 {
			return nil, ErrBadSnapshot
		}
		if rule.ExtractPolicy != ExtractReturn && rule.ExtractPolicy != ExtractDestroy {
			return nil, ErrBadSnapshot
		}
		restored.rules[tier] = rule
	}
	for id, item := range payload.Items {
		if blank(id) || item.Spec.ID != id || blank(item.Spec.Owner) || len(item.Spec.SocketColors) == 0 {
			return nil, ErrBadSnapshot
		}
		for _, colour := range item.Spec.SocketColors {
			if blank(colour) {
				return nil, ErrBadSnapshot
			}
		}
		if len(item.Sockets) != len(item.Spec.SocketColors) {
			return nil, ErrBadSnapshot
		}
		sockets := make([]Socket, len(item.Sockets))
		for i, socket := range item.Sockets {
			if socket.Index != i || socket.Color != item.Spec.SocketColors[i] {
				return nil, ErrBadSnapshot
			}
			if socket.Gem != "" {
				gem, exists := restored.gems[socket.Gem]
				if !exists || gem.Color != socket.Color {
					return nil, ErrBadSnapshot
				}
			}
			sockets[i] = Socket{Index: i, Color: socket.Color, Gem: socket.Gem}
		}
		restored.items[id] = &itemRecord{spec: cloneItemSpec(item.Spec), sockets: sockets}
	}
	for player, bag := range payload.Bags {
		if blank(player) {
			return nil, ErrBadSnapshot
		}
		copied := make(map[string]int, len(bag))
		for gemID, count := range bag {
			if _, exists := restored.gems[gemID]; !exists || count < 0 {
				return nil, ErrBadSnapshot
			}
			if count > 0 {
				copied[gemID] = count
			}
		}
		restored.bags[player] = copied
	}

	var maxSeq int64
	for itemID, history := range payload.History {
		item, exists := restored.items[itemID]
		if !exists {
			return nil, ErrBadSnapshot
		}
		if payload.Limits.MaxHistory >= 0 && len(history) > payload.Limits.MaxHistory {
			return nil, ErrBadSnapshot
		}
		var previous int64
		copied := make([]InlayRecord, 0, len(history))
		for _, record := range history {
			if record.Seq <= 0 || record.Seq <= previous {
				return nil, ErrBadSnapshot
			}
			if blank(record.Player) || record.Player != item.spec.Owner || record.ItemID != itemID {
				return nil, ErrBadSnapshot
			}
			if record.Socket < 0 || record.Socket >= len(item.sockets) {
				return nil, ErrBadSnapshot
			}
			if record.Action != ActionInlay && record.Action != ActionExtract {
				return nil, ErrBadSnapshot
			}
			gem, exists := restored.gems[record.GemID]
			if !exists || record.SetID != gem.SetID {
				return nil, ErrBadSnapshot
			}
			if record.Action == ActionInlay && record.Destroyed {
				return nil, ErrBadSnapshot
			}
			previous = record.Seq
			if record.Seq > maxSeq {
				maxSeq = record.Seq
			}
			copied = append(copied, record)
		}
		restored.history[itemID] = copied
	}
	if restored.seq < 0 {
		return nil, ErrBadSnapshot
	}
	if restored.seq < maxSeq {
		restored.seq = maxSeq
	}
	return restored, nil
}
