// Package task assigns players of one scene map to channel lines: soft and hard
// capacity limits, whole party reservations, switch cooldown, rebalancing and
// snapshot based restarts.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Line status values reported by LineState.
const (
	LineOpen   = "open"
	LineClosed = "closed"

	snapshotVersion = 1
)

// Errors reported by the assigner.
var (
	ErrInvalidCapacity  = errors.New("line capacity must be positive")
	ErrLineLimitReached = errors.New("map has reached its line budget")
	ErrLineNotFound     = errors.New("line does not belong to this map")
	ErrLineFull         = errors.New("line is at its hard capacity")
	ErrPartyTooLarge    = errors.New("party is larger than the configured maximum")
	ErrPartyNoRoom      = errors.New("no single line can seat the whole party")
	ErrPartyNotReserved = errors.New("party has no reservation on this map")
	ErrSwitchLocked     = errors.New("line switching is locked on this map")
	ErrSwitchCooling    = errors.New("player switched lines too recently")
	ErrSwitchSameLine   = errors.New("player already sits on the target line")
	ErrPlayerNotOnMap   = errors.New("player is not on this map")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Limits bounds the number of lines, the party size, the moves produced by one
// rebalance pass and the switch cooldown. SoftRatio is the fraction of the hard
// capacity that new solo entrants treat as the comfortable fill level.
type Limits struct {
	MaxLinesPerMap       int
	MaxPartySize         int
	MaxMovesPerRebalance int
	SwitchCooldown       time.Duration
	SoftRatio            float64
}

// LineState is the public view of one channel line.
type LineState struct {
	MapID     string
	LineID    string
	Capacity  int
	SoftCap   int
	Occupancy int
	Reserved  int
	Status    string
}

// Assignment records where one player sits.
type Assignment struct {
	MapID         string
	PlayerID      string
	PartyID       string
	LineID        string
	HomeLine      string
	Reserved      bool
	CooldownUntil time.Time
}

// Move is one player movement produced by Rebalance.
type Move struct {
	MapID    string
	PlayerID string
	FromLine string
	ToLine   string
	PartyID  string
}

// ChannelLineAssigner owns the line table, the seating of every player, the
// party bindings and the switch cooldown book of all maps.
type ChannelLineAssigner struct {
	mu     sync.RWMutex
	limits Limits
	maps   map[string]*mapState
}

type mapState struct {
	id       string
	nextLine int
	lines    []*lineState
	lineByID map[string]*lineState
	players  map[string]*playerState
	parties  map[string]*partyState
	locked   bool
}

type lineState struct {
	number    int
	mapID     string
	lineID    string
	capacity  int
	softCap   int
	occupancy int
	reserved  int
	status    string
}

func (l *lineState) publicState() LineState {
	return LineState{
		MapID:     l.mapID,
		LineID:    l.lineID,
		Capacity:  l.capacity,
		SoftCap:   l.softCap,
		Occupancy: l.occupancy,
		Reserved:  l.reserved,
		Status:    l.status,
	}
}

func (l *lineState) hardFree() int {
	return l.capacity - l.occupancy - l.reserved
}

type playerState struct {
	playerID      string
	partyID       string
	lineID        string
	homeLine      string
	usesReserved  bool
	cooldownUntil time.Time
}

func (p *playerState) assignment(mapID string) Assignment {
	return Assignment{
		MapID:         mapID,
		PlayerID:      p.playerID,
		PartyID:       p.partyID,
		LineID:        p.lineID,
		HomeLine:      p.homeLine,
		Reserved:      p.usesReserved,
		CooldownUntil: p.cooldownUntil,
	}
}

type partyState struct {
	partyID           string
	lineID            string
	reservationLineID string
	leaderID          string
	reservation       bool
	size              int
	remaining         int
}

func newMapState(mapID string) *mapState {
	return &mapState{
		id:       mapID,
		lineByID: make(map[string]*lineState),
		players:  make(map[string]*playerState),
		parties:  make(map[string]*partyState),
	}
}

// NewChannelLineAssigner builds an assigner with the given limits.
func NewChannelLineAssigner(limits Limits) *ChannelLineAssigner {
	return &ChannelLineAssigner{
		limits: limits,
		maps:   make(map[string]*mapState),
	}
}

func (a *ChannelLineAssigner) ensureMapLocked(mapID string) *mapState {
	if a.maps == nil {
		a.maps = make(map[string]*mapState)
	}
	m := a.maps[mapID]
	if m == nil {
		m = newMapState(mapID)
		a.maps[mapID] = m
	}
	return m
}

func (a *ChannelLineAssigner) softCap(capacity int) int {
	soft := int(float64(capacity) * a.limits.SoftRatio)
	if soft < 1 {
		return 1
	}
	return soft
}

func lessOccupied(candidate, current *lineState) bool {
	if current == nil {
		return true
	}
	if candidate.occupancy != current.occupancy {
		return candidate.occupancy < current.occupancy
	}
	return candidate.number < current.number
}

// pickSoloLine implements the soft-cap spreading rule. Callers must hold the
// assigner write lock.
func pickSoloLine(m *mapState) *lineState {
	var softLine, overflowLine *lineState
	for _, line := range m.lines {
		if line.status != LineOpen || line.hardFree() <= 0 {
			continue
		}
		if line.occupancy <= line.softCap {
			if lessOccupied(line, softLine) {
				softLine = line
			}
		} else if lessOccupied(line, overflowLine) {
			overflowLine = line
		}
	}
	if softLine != nil {
		return softLine
	}
	return overflowLine
}

func pickRebalanceTarget(m *mapState, fromLine string) *lineState {
	var target *lineState
	for _, line := range m.lines {
		if line.lineID == fromLine || line.status != LineOpen {
			continue
		}
		if line.occupancy >= line.softCap || line.hardFree() <= 0 {
			continue
		}
		if target == nil || line.occupancy < target.occupancy ||
			(line.occupancy == target.occupancy && line.number < target.number) {
			target = line
		}
	}
	return target
}

func (m *mapState) partyMemberCount(partyID string) int {
	count := 0
	for _, player := range m.players {
		if player.partyID == partyID {
			count++
		}
	}
	return count
}

func (m *mapState) reconcilePartyLocked(partyID string) {
	party, ok := m.parties[partyID]
	if !ok {
		return
	}
	if leader, exists := m.players[party.leaderID]; exists && leader.partyID == partyID {
		party.lineID = leader.lineID
		return
	}

	var nextLeader *playerState
	for _, player := range m.players {
		if player.partyID != partyID {
			continue
		}
		if nextLeader == nil || player.playerID < nextLeader.playerID {
			nextLeader = player
		}
	}
	if nextLeader != nil {
		party.leaderID = nextLeader.playerID
		party.lineID = nextLeader.lineID
		return
	}

	if party.reservation {
		if line, ok := m.lineByID[party.reservationLineID]; ok && line.status == LineOpen {
			party.lineID = party.reservationLineID
			return
		}
		if line, ok := m.lineByID[party.lineID]; ok && line.status == LineOpen {
			return
		}
	}
	delete(m.parties, partyID)
}

// OpenLine adds one channel line to a map and returns its public state.
func (a *ChannelLineAssigner) OpenLine(mapID string, capacity int) (LineState, error) {
	if capacity <= 0 {
		return LineState{}, ErrInvalidCapacity
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.ensureMapLocked(mapID)
	if len(m.lines) >= a.limits.MaxLinesPerMap {
		return LineState{}, ErrLineLimitReached
	}

	m.nextLine++
	line := &lineState{
		number:   m.nextLine,
		mapID:    mapID,
		lineID:   fmt.Sprintf("line-%d", m.nextLine),
		capacity: capacity,
		softCap:  a.softCap(capacity),
		status:   LineOpen,
	}
	m.lines = append(m.lines, line)
	m.lineByID[line.lineID] = line
	return line.publicState(), nil
}

// CloseLine closes one line and returns the players that were pushed out.
func (a *ChannelLineAssigner) CloseLine(mapID, lineID string, at time.Time) ([]string, error) {
	_ = at
	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.maps[mapID]
	if m == nil {
		return nil, ErrLineNotFound
	}
	line, ok := m.lineByID[lineID]
	if !ok {
		return nil, ErrLineNotFound
	}
	if line.status == LineClosed {
		return []string{}, nil
	}

	evicted := make([]string, 0)
	for playerID, player := range m.players {
		if player.lineID != lineID {
			continue
		}
		evicted = append(evicted, playerID)
	}
	sort.Strings(evicted)

	for _, playerID := range evicted {
		delete(m.players, playerID)
	}
	line.occupancy = 0
	line.reserved = 0
	line.status = LineClosed

	for _, party := range m.parties {
		if party.reservation && party.reservationLineID == lineID {
			party.reservation = false
			party.remaining = 0
			party.reservationLineID = ""
		}
	}
	for partyID := range m.parties {
		m.reconcilePartyLocked(partyID)
	}

	return evicted, nil
}

// ReserveParty books whole party seats on one single line.
func (a *ChannelLineAssigner) ReserveParty(mapID, partyID string, members int) (string, error) {
	if members < 0 || members > a.limits.MaxPartySize {
		return "", ErrPartyTooLarge
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.ensureMapLocked(mapID)
	if party, ok := m.parties[partyID]; ok {
		if party.reservation && party.reservationLineID != "" {
			return party.reservationLineID, nil
		}
		return party.lineID, nil
	}

	for _, line := range m.lines {
		if line.status != LineOpen || line.hardFree() < members {
			continue
		}
		line.reserved += members
		m.parties[partyID] = &partyState{
			partyID:           partyID,
			lineID:            line.lineID,
			reservationLineID: line.lineID,
			reservation:       true,
			size:              members,
			remaining:         members,
		}
		return line.lineID, nil
	}
	return "", ErrPartyNoRoom
}

// Enter seats one player, following the party binding when there is one.
func (a *ChannelLineAssigner) Enter(mapID, playerID, partyID string, at time.Time) (Assignment, error) {
	_ = at
	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.ensureMapLocked(mapID)
	if player, ok := m.players[playerID]; ok {
		return player.assignment(mapID), nil
	}

	if partyID != "" && m.partyMemberCount(partyID) >= a.limits.MaxPartySize {
		return Assignment{}, ErrPartyTooLarge
	}

	var party *partyState
	if partyID != "" {
		party = m.parties[partyID]
	}

	var target *lineState
	consumeReservation := false
	preferredLine := ""

	if party != nil {
		if party.reservation && party.remaining > 0 {
			if line, ok := m.lineByID[party.reservationLineID]; ok &&
				line.status == LineOpen && line.reserved > 0 {
				target = line
				consumeReservation = true
				preferredLine = line.lineID
			}
		}

		if target == nil {
			leaderPresent := party.leaderID != ""
			if leaderPresent {
				_, leaderPresent = m.players[party.leaderID]
			}
			if party.reservation || leaderPresent {
				preferredLine = party.lineID
				if line, ok := m.lineByID[party.lineID]; ok && line.status == LineOpen && line.hardFree() > 0 {
					target = line
				}
			}
		}
	}

	if target == nil {
		target = pickSoloLine(m)
	}
	if target == nil {
		return Assignment{}, ErrLineFull
	}
	if !consumeReservation && target.hardFree() <= 0 {
		return Assignment{}, ErrLineFull
	}

	if consumeReservation {
		target.reserved--
		party.remaining--
	}
	target.occupancy++

	homeLine := preferredLine
	if homeLine == "" {
		homeLine = target.lineID
	}
	player := &playerState{
		playerID:     playerID,
		partyID:      partyID,
		lineID:       target.lineID,
		homeLine:     homeLine,
		usesReserved: consumeReservation,
	}
	m.players[playerID] = player

	if partyID != "" {
		if party == nil {
			party = &partyState{
				partyID:  partyID,
				lineID:   target.lineID,
				leaderID: playerID,
			}
			m.parties[partyID] = party
			player.homeLine = target.lineID
		} else if party.leaderID == "" {
			party.leaderID = playerID
			party.lineID = target.lineID
			player.homeLine = target.lineID
		} else if _, leaderExists := m.players[party.leaderID]; !leaderExists {
			party.leaderID = playerID
			party.lineID = target.lineID
			player.homeLine = target.lineID
		}
	}

	return player.assignment(mapID), nil
}

// Leave releases the seat of one player.
func (a *ChannelLineAssigner) Leave(mapID, playerID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.maps[mapID]
	if m == nil {
		return ErrPlayerNotOnMap
	}
	player, ok := m.players[playerID]
	if !ok {
		return ErrPlayerNotOnMap
	}

	if line, exists := m.lineByID[player.lineID]; exists {
		line.occupancy--
		if line.occupancy < 0 {
			line.occupancy = 0
		}
	}
	partyID := player.partyID
	delete(m.players, playerID)
	if partyID != "" {
		m.reconcilePartyLocked(partyID)
	}
	return nil
}

// Switch moves one player to another line of the same map.
func (a *ChannelLineAssigner) Switch(mapID, playerID, targetLine string, at time.Time) (Assignment, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.maps[mapID]
	if m == nil {
		return Assignment{}, ErrPlayerNotOnMap
	}
	if m.locked {
		return Assignment{}, ErrSwitchLocked
	}
	player, ok := m.players[playerID]
	if !ok {
		return Assignment{}, ErrPlayerNotOnMap
	}
	if at.Before(player.cooldownUntil) {
		return player.assignment(mapID), ErrSwitchCooling
	}

	target, ok := m.lineByID[targetLine]
	if !ok || target.status != LineOpen {
		// A line closed while the switch was being processed cannot accept the
		// player. The locked critical section leaves the player on the source
		// line and no cooldown is started by this failed attempt.
		return player.assignment(mapID), ErrLineNotFound
	}
	if target.lineID == player.lineID {
		return player.assignment(mapID), ErrSwitchSameLine
	}

	source := m.lineByID[player.lineID]
	party := m.parties[player.partyID]
	consumePartyReservation := party != nil && party.reservation &&
		party.reservationLineID == target.lineID && party.remaining > 0 && target.reserved > 0

	if !consumePartyReservation && target.hardFree() <= 0 {
		return player.assignment(mapID), ErrLineFull
	}

	if source != nil {
		source.occupancy--
		if source.occupancy < 0 {
			source.occupancy = 0
		}
	}
	if consumePartyReservation {
		target.reserved--
		party.remaining--
		player.usesReserved = true
	}
	target.occupancy++

	player.lineID = target.lineID
	player.homeLine = target.lineID
	player.cooldownUntil = at.Add(a.limits.SwitchCooldown)
	if party != nil && party.leaderID == playerID {
		party.lineID = target.lineID
	}

	return player.assignment(mapID), nil
}

// Rebalance evens the fill level of the lines of one map.
func (a *ChannelLineAssigner) Rebalance(mapID string, at time.Time) ([]Move, error) {
	_ = at
	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.maps[mapID]
	if m == nil {
		return []Move{}, nil
	}

	type candidate struct {
		player *playerState
		line   *lineState
	}
	candidates := make([]candidate, 0)
	for _, player := range m.players {
		if player.partyID != "" {
			continue
		}
		line := m.lineByID[player.lineID]
		if line == nil || line.status != LineOpen || line.occupancy <= line.softCap {
			continue
		}
		candidates = append(candidates, candidate{player: player, line: line})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].player.playerID < candidates[j].player.playerID
	})

	budget := a.limits.MaxMovesPerRebalance
	if budget <= 0 {
		return []Move{}, nil
	}
	moves := make([]Move, 0, minInt(budget, len(candidates)))
	for _, item := range candidates {
		if len(moves) >= budget {
			break
		}
		player := item.player
		source := m.lineByID[player.lineID]
		if source == nil || source.status != LineOpen || source.occupancy <= source.softCap {
			continue
		}
		target := pickRebalanceTarget(m, source.lineID)
		if target == nil {
			continue
		}

		source.occupancy--
		target.occupancy++
		player.lineID = target.lineID
		player.homeLine = target.lineID
		moves = append(moves, Move{
			MapID:    mapID,
			PlayerID: player.playerID,
			FromLine: source.lineID,
			ToLine:   target.lineID,
			PartyID:  player.partyID,
		})
	}

	sort.Slice(moves, func(i, j int) bool {
		return moves[i].PlayerID < moves[j].PlayerID
	})
	return moves, nil
}

func minInt(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// LockSwitching freezes or unfreezes line switching for a whole map.
func (a *ChannelLineAssigner) LockSwitching(mapID string, locked bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.ensureMapLocked(mapID)
	m.locked = locked
	return nil
}

// IsLocked reports whether line switching is frozen on the map.
func (a *ChannelLineAssigner) IsLocked(mapID string) (bool, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	m := a.maps[mapID]
	return m != nil && m.locked, nil
}

// Locate reports where one player currently sits.
func (a *ChannelLineAssigner) Locate(mapID, playerID string) (Assignment, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	m := a.maps[mapID]
	if m == nil {
		return Assignment{}, false
	}
	player, ok := m.players[playerID]
	if !ok {
		return Assignment{}, false
	}
	return player.assignment(mapID), true
}

// Lines lists the line states of one map in line order.
func (a *ChannelLineAssigner) Lines(mapID string) ([]LineState, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	m := a.maps[mapID]
	if m == nil {
		return []LineState{}, nil
	}
	out := make([]LineState, 0, len(m.lines))
	for _, line := range m.lines {
		out = append(out, line.publicState())
	}
	return out, nil
}

type snapshotFile struct {
	Version int           `json:"version"`
	Maps    []snapshotMap `json:"maps"`
}

type snapshotMap struct {
	MapID    string           `json:"map_id"`
	Locked   bool             `json:"locked"`
	NextLine int              `json:"next_line"`
	Lines    []snapshotLine   `json:"lines"`
	Players  []snapshotPlayer `json:"players"`
	Parties  []snapshotParty  `json:"parties"`
}

type snapshotLine struct {
	Number    int    `json:"number"`
	LineID    string `json:"line_id"`
	Capacity  int    `json:"capacity"`
	SoftCap   int    `json:"soft_cap"`
	Occupancy int    `json:"occupancy"`
	Reserved  int    `json:"reserved"`
	Status    string `json:"status"`
}

type snapshotPlayer struct {
	PlayerID      string    `json:"player_id"`
	PartyID       string    `json:"party_id"`
	LineID        string    `json:"line_id"`
	HomeLine      string    `json:"home_line"`
	UsesReserved  bool      `json:"uses_reserved"`
	CooldownUntil time.Time `json:"cooldown_until"`
}

type snapshotParty struct {
	PartyID           string `json:"party_id"`
	LineID            string `json:"line_id"`
	ReservationLineID string `json:"reservation_line_id"`
	LeaderID          string `json:"leader_id"`
	Reservation       bool   `json:"reservation"`
	Size              int    `json:"size"`
	Remaining         int    `json:"remaining"`
}

// Snapshot serialises lines, seating, party bindings and cooldowns.
func (a *ChannelLineAssigner) Snapshot() ([]byte, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	mapIDs := make([]string, 0, len(a.maps))
	for mapID := range a.maps {
		mapIDs = append(mapIDs, mapID)
	}
	sort.Strings(mapIDs)

	file := snapshotFile{Version: snapshotVersion, Maps: make([]snapshotMap, 0, len(mapIDs))}
	for _, mapID := range mapIDs {
		m := a.maps[mapID]
		sm := snapshotMap{
			MapID:    mapID,
			Locked:   m.locked,
			NextLine: m.nextLine,
			Lines:    make([]snapshotLine, 0, len(m.lines)),
			Players:  make([]snapshotPlayer, 0, len(m.players)),
			Parties:  make([]snapshotParty, 0, len(m.parties)),
		}
		for _, line := range m.lines {
			sm.Lines = append(sm.Lines, snapshotLine{
				Number:    line.number,
				LineID:    line.lineID,
				Capacity:  line.capacity,
				SoftCap:   line.softCap,
				Occupancy: line.occupancy,
				Reserved:  line.reserved,
				Status:    line.status,
			})
		}

		playerIDs := make([]string, 0, len(m.players))
		for playerID := range m.players {
			playerIDs = append(playerIDs, playerID)
		}
		sort.Strings(playerIDs)
		for _, playerID := range playerIDs {
			player := m.players[playerID]
			sm.Players = append(sm.Players, snapshotPlayer{
				PlayerID:      player.playerID,
				PartyID:       player.partyID,
				LineID:        player.lineID,
				HomeLine:      player.homeLine,
				UsesReserved:  player.usesReserved,
				CooldownUntil: player.cooldownUntil,
			})
		}

		partyIDs := make([]string, 0, len(m.parties))
		for partyID := range m.parties {
			partyIDs = append(partyIDs, partyID)
		}
		sort.Strings(partyIDs)
		for _, partyID := range partyIDs {
			party := m.parties[partyID]
			sm.Parties = append(sm.Parties, snapshotParty{
				PartyID:           party.partyID,
				LineID:            party.lineID,
				ReservationLineID: party.reservationLineID,
				LeaderID:          party.leaderID,
				Reservation:       party.reservation,
				Size:              party.size,
				Remaining:         party.remaining,
			})
		}
		file.Maps = append(file.Maps, sm)
	}

	data, err := json.Marshal(file)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *ChannelLineAssigner) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return ErrBadSnapshot
	}

	var file snapshotFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	nextMaps, err := file.buildStates()
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.maps = nextMaps
	a.mu.Unlock()
	return nil
}

func (f snapshotFile) buildStates() (map[string]*mapState, error) {
	if f.Version != snapshotVersion {
		return nil, ErrBadSnapshot
	}

	result := make(map[string]*mapState, len(f.Maps))
	for _, sm := range f.Maps {
		if strings.TrimSpace(sm.MapID) == "" {
			return nil, ErrBadSnapshot
		}
		if _, duplicated := result[sm.MapID]; duplicated {
			return nil, ErrBadSnapshot
		}

		m := newMapState(sm.MapID)
		m.locked = sm.Locked
		m.nextLine = sm.NextLine

		seenLines := make(map[string]struct{}, len(sm.Lines))
		occupancyByLine := make(map[string]int, len(sm.Lines))
		for _, sl := range sm.Lines {
			expectedLineID := fmt.Sprintf("line-%d", sl.Number)
			if sl.Number <= 0 || sl.LineID != expectedLineID {
				return nil, ErrBadSnapshot
			}
			if _, duplicated := seenLines[sl.LineID]; duplicated {
				return nil, ErrBadSnapshot
			}
			if sl.Capacity <= 0 || sl.SoftCap < 1 {
				return nil, ErrBadSnapshot
			}
			if sl.Occupancy < 0 || sl.Reserved < 0 || sl.Occupancy+sl.Reserved > sl.Capacity {
				return nil, ErrBadSnapshot
			}
			if sl.Status != LineOpen && sl.Status != LineClosed {
				return nil, ErrBadSnapshot
			}
			if sl.Status == LineClosed && (sl.Occupancy != 0 || sl.Reserved != 0) {
				return nil, ErrBadSnapshot
			}

			line := &lineState{
				number:    sl.Number,
				mapID:     sm.MapID,
				lineID:    sl.LineID,
				capacity:  sl.Capacity,
				softCap:   sl.SoftCap,
				occupancy: sl.Occupancy,
				reserved:  sl.Reserved,
				status:    sl.Status,
			}
			m.lines = append(m.lines, line)
			m.lineByID[line.lineID] = line
			seenLines[line.lineID] = struct{}{}
			occupancyByLine[line.lineID] = 0
			if sl.Number > m.nextLine {
				m.nextLine = sl.Number
			}
		}
		sort.Slice(m.lines, func(i, j int) bool {
			return m.lines[i].number < m.lines[j].number
		})

		seenPlayers := make(map[string]struct{}, len(sm.Players))
		for _, sp := range sm.Players {
			if strings.TrimSpace(sp.PlayerID) == "" {
				return nil, ErrBadSnapshot
			}
			if _, duplicated := seenPlayers[sp.PlayerID]; duplicated {
				return nil, ErrBadSnapshot
			}
			line, ok := m.lineByID[sp.LineID]
			if !ok || line.status != LineOpen {
				return nil, ErrBadSnapshot
			}
			if sp.HomeLine == "" {
				sp.HomeLine = sp.LineID
			} else if _, ok := m.lineByID[sp.HomeLine]; !ok {
				return nil, ErrBadSnapshot
			}

			player := &playerState{
				playerID:      sp.PlayerID,
				partyID:       sp.PartyID,
				lineID:        sp.LineID,
				homeLine:      sp.HomeLine,
				usesReserved:  sp.UsesReserved,
				cooldownUntil: sp.CooldownUntil,
			}
			m.players[player.playerID] = player
			seenPlayers[player.playerID] = struct{}{}
			occupancyByLine[player.lineID]++
		}

		for _, line := range m.lines {
			if occupancyByLine[line.lineID] != line.occupancy {
				return nil, ErrBadSnapshot
			}
		}

		remainingByLine := make(map[string]int, len(sm.Parties))
		seenParties := make(map[string]struct{}, len(sm.Parties))
		for _, sp := range sm.Parties {
			if strings.TrimSpace(sp.PartyID) == "" {
				return nil, ErrBadSnapshot
			}
			if _, duplicated := seenParties[sp.PartyID]; duplicated {
				return nil, ErrBadSnapshot
			}
			line, ok := m.lineByID[sp.LineID]
			if !ok || line.status != LineOpen {
				return nil, ErrBadSnapshot
			}
			reservationLineID := sp.ReservationLineID
			if sp.Reservation && reservationLineID == "" {
				reservationLineID = sp.LineID
			}
			if sp.Reservation {
				reservedLine, ok := m.lineByID[reservationLineID]
				if !ok || reservedLine.status != LineOpen {
					return nil, ErrBadSnapshot
				}
			}
			if sp.Size < 0 || sp.Remaining < 0 || sp.Remaining > sp.Size {
				return nil, ErrBadSnapshot
			}
			if sp.LeaderID != "" {
				leader, ok := m.players[sp.LeaderID]
				if !ok || leader.partyID != sp.PartyID {
					return nil, ErrBadSnapshot
				}
			}

			party := &partyState{
				partyID:           sp.PartyID,
				lineID:            sp.LineID,
				reservationLineID: reservationLineID,
				leaderID:          sp.LeaderID,
				reservation:       sp.Reservation,
				size:              sp.Size,
				remaining:         sp.Remaining,
			}
			m.parties[party.partyID] = party
			seenParties[party.partyID] = struct{}{}
			if party.reservation {
				remainingByLine[party.reservationLineID] += party.remaining
			}
		}

		for _, line := range m.lines {
			if line.reserved != remainingByLine[line.lineID] {
				return nil, ErrBadSnapshot
			}
		}
		for _, player := range m.players {
			if player.partyID == "" {
				continue
			}
			party, ok := m.parties[player.partyID]
			if !ok {
				return nil, ErrBadSnapshot
			}
			if party.leaderID == "" {
				party.leaderID = player.playerID
			}
		}

		result[sm.MapID] = m
	}
	return result, nil
}
