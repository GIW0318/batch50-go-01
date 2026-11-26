// Package task runs the character retire flow of one game account: retire
// requests, the cool down window, cancellation while the window is open, the
// final purge, name retention and tombstone based restores.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Character status values reported by CharacterState.
const (
	StatusActive  = "active"
	StatusPending = "pending"
	StatusPurged  = "purged"
)

// Name status values reported by NameState.
const (
	NameFree  = "free"
	NameHeld  = "held"
	NameTaken = "taken"
)

// Errors reported by the guard.
var (
	ErrBadArgument       = errors.New("account, character and name are required")
	ErrCharacterExists   = errors.New("character id is already registered")
	ErrNameTaken         = errors.New("character name is already taken")
	ErrCharacterNotFound = errors.New("character is not registered")
	ErrAccountMismatch   = errors.New("character or ticket belongs to another account")
	ErrPendingLimit      = errors.New("account reached its pending retire limit")
	ErrTicketNotFound    = errors.New("retire ticket is not known")
	ErrTicketClosed      = errors.New("retire ticket is already closed")
	ErrTooEarly          = errors.New("retire cool down has not elapsed")
	ErrCharacterRetired  = errors.New("character is retired")
	ErrNotRestorable     = errors.New("character cannot be restored")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

const (
	snapshotVersion = 1

	ticketOpen     = "open"
	ticketCanceled = "canceled"
	ticketClosed   = "closed"
)

// Limits bounds the pending retire tickets of one account, the cool down before
// the purge, the nickname retention window and the tombstone book.
type Limits struct {
	MaxPendingPerAccount int
	RetireCoolDown       time.Duration
	NameHoldDuration     time.Duration
	MaxTombstones        int
}

// CharacterState is the public view of one character.
type CharacterState struct {
	CharacterID string
	AccountID   string
	Name        string
	Status      string
}

// RetireTicket is one pending retire request.
type RetireTicket struct {
	TicketID    string
	AccountID   string
	CharacterID string
	RequestedAt time.Time
	PurgeAt     time.Time
}

// RetireResult reports the outcome of one finalize call.
type RetireResult struct {
	TicketID    string
	CharacterID string
	Name        string
	Purged      bool
	TombstoneID string
}

// NameState reports who holds one character name.
type NameState struct {
	Name   string
	Status string
	HeldBy string
	Until  time.Time
}

// CharacterRetireGuard owns the character book, the retire tickets, the name
// registry and the tombstones.
type CharacterRetireGuard struct {
	mu     sync.RWMutex
	limits Limits
	state  *guardState
}

type guardState struct {
	Seq        uint64                      `json:"seq"`
	Now        time.Time                   `json:"now"`
	Characters map[string]*characterRecord `json:"characters"`
	Tickets    map[string]*ticketRecord    `json:"tickets"`
	Names      map[string]*nameRecord      `json:"names"`
	Tombstones map[string]*tombstoneRecord `json:"tombstones"`
}

type characterRecord struct {
	CharacterID     string `json:"character_id"`
	AccountID       string `json:"account_id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	CurrentTicketID string `json:"current_ticket_id,omitempty"`
	TombstoneID     string `json:"tombstone_id,omitempty"`
}

func (c *characterRecord) publicState() CharacterState {
	return CharacterState{
		CharacterID: c.CharacterID,
		AccountID:   c.AccountID,
		Name:        c.Name,
		Status:      c.Status,
	}
}

type ticketRecord struct {
	Seq         uint64    `json:"seq"`
	TicketID    string    `json:"ticket_id"`
	AccountID   string    `json:"account_id"`
	CharacterID string    `json:"character_id"`
	RequestedAt time.Time `json:"requested_at"`
	PurgeAt     time.Time `json:"purge_at"`
	Status      string    `json:"status"`
	ClosedAt    time.Time `json:"closed_at"`
}

func (t *ticketRecord) publicTicket() RetireTicket {
	return RetireTicket{
		TicketID:    t.TicketID,
		AccountID:   t.AccountID,
		CharacterID: t.CharacterID,
		RequestedAt: t.RequestedAt,
		PurgeAt:     t.PurgeAt,
	}
}

type nameRecord struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	HeldBy      string    `json:"held_by"`
	AccountID   string    `json:"account_id,omitempty"`
	CharacterID string    `json:"character_id,omitempty"`
	TombstoneID string    `json:"tombstone_id,omitempty"`
	Until       time.Time `json:"until"`
}

type tombstoneRecord struct {
	Seq         uint64    `json:"seq"`
	TombstoneID string    `json:"tombstone_id"`
	TicketID    string    `json:"ticket_id"`
	CharacterID string    `json:"character_id"`
	AccountID   string    `json:"account_id"`
	Name        string    `json:"name"`
	PurgedAt    time.Time `json:"purged_at"`
	HoldUntil   time.Time `json:"hold_until"`
}

type snapshotEnvelope struct {
	Version int         `json:"version"`
	State   *guardState `json:"state"`
}

// NewCharacterRetireGuard builds a guard with the given limits.
func NewCharacterRetireGuard(limits Limits) *CharacterRetireGuard {
	return &CharacterRetireGuard{
		limits: limits,
		state:  newGuardState(),
	}
}

func newGuardState() *guardState {
	return &guardState{
		Characters: make(map[string]*characterRecord),
		Tickets:    make(map[string]*ticketRecord),
		Names:      make(map[string]*nameRecord),
		Tombstones: make(map[string]*tombstoneRecord),
	}
}

func required(value string) bool {
	return strings.TrimSpace(value) != ""
}

func (s *guardState) nextID(prefix string) string {
	s.Seq++
	return fmt.Sprintf("%s-%d", prefix, s.Seq)
}

func (s *guardState) observe(at time.Time) {
	if !at.IsZero() && at.After(s.Now) {
		s.Now = at
	}
}

func (s *guardState) nameAvailable(name *nameRecord) bool {
	if name == nil || name.Status == NameFree {
		return true
	}
	if name.Status != NameHeld {
		return false
	}
	return !s.Now.Before(name.Until)
}

func (s *guardState) openTicketForCharacter(characterID string) *ticketRecord {
	character := s.Characters[characterID]
	if character != nil && character.CurrentTicketID != "" {
		if ticket := s.Tickets[character.CurrentTicketID]; ticket != nil && ticket.Status == ticketOpen {
			return ticket
		}
	}

	var fallback *ticketRecord
	for _, ticket := range s.Tickets {
		if ticket.CharacterID != characterID || ticket.Status != ticketOpen {
			continue
		}
		if fallback == nil || ticket.Seq > fallback.Seq {
			fallback = ticket
		}
	}
	return fallback
}

func (s *guardState) cancelTicket(ticket *ticketRecord) (CharacterState, error) {
	character := s.Characters[ticket.CharacterID]
	if character == nil {
		return CharacterState{}, ErrCharacterNotFound
	}

	switch ticket.Status {
	case ticketCanceled:
		return character.publicState(), nil
	case ticketClosed:
		return CharacterState{}, ErrTicketClosed
	}

	ticket.Status = ticketCanceled
	if character.CurrentTicketID == ticket.TicketID {
		character.Status = StatusActive
		character.CurrentTicketID = ""
	}
	return character.publicState(), nil
}

func (s *guardState) evictOldestTombstones(limit int) {
	if limit < 0 {
		limit = 0
	}

	for len(s.Tombstones) > limit {
		var oldest *tombstoneRecord
		for _, tombstone := range s.Tombstones {
			if oldest == nil ||
				tombstone.PurgedAt.Before(oldest.PurgedAt) ||
				(tombstone.PurgedAt.Equal(oldest.PurgedAt) && tombstone.Seq < oldest.Seq) {
				oldest = tombstone
			}
		}
		if oldest == nil {
			return
		}

		delete(s.Tombstones, oldest.TombstoneID)
		if character := s.Characters[oldest.CharacterID]; character != nil &&
			character.TombstoneID == oldest.TombstoneID {
			character.TombstoneID = ""
		}
		if name := s.Names[oldest.Name]; name != nil && name.TombstoneID == oldest.TombstoneID {
			name.TombstoneID = ""
		}
	}
}

// Register adds one active character and claims its name.
func (g *CharacterRetireGuard) Register(accountID, characterID, name string) (CharacterState, error) {
	if !required(accountID) || !required(characterID) || !required(name) {
		return CharacterState{}, ErrBadArgument
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state

	if _, exists := s.Characters[characterID]; exists {
		return CharacterState{}, ErrCharacterExists
	}
	if existing := s.Names[name]; existing != nil && !s.nameAvailable(existing) {
		return CharacterState{}, ErrNameTaken
	}

	delete(s.Names, name)
	character := &characterRecord{
		CharacterID: characterID,
		AccountID:   accountID,
		Name:        name,
		Status:      StatusActive,
	}
	s.Characters[characterID] = character
	s.Names[name] = &nameRecord{
		Name:        name,
		Status:      NameTaken,
		HeldBy:      characterID,
		AccountID:   accountID,
		CharacterID: characterID,
	}
	return character.publicState(), nil
}

// Request opens a retire ticket for one character.
func (g *CharacterRetireGuard) Request(accountID, characterID string, at time.Time) (RetireTicket, error) {
	if !required(accountID) || !required(characterID) {
		return RetireTicket{}, ErrBadArgument
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state

	character, ok := s.Characters[characterID]
	if !ok {
		return RetireTicket{}, ErrCharacterNotFound
	}
	if character.AccountID != accountID {
		return RetireTicket{}, ErrAccountMismatch
	}

	if character.Status == StatusPending {
		if ticket := s.openTicketForCharacter(characterID); ticket != nil {
			character.CurrentTicketID = ticket.TicketID
			return ticket.publicTicket(), nil
		}
		character.Status = StatusActive
		character.CurrentTicketID = ""
	}
	if character.Status == StatusPurged {
		return RetireTicket{}, ErrCharacterRetired
	}

	pendingLimit := g.limits.MaxPendingPerAccount
	if pendingLimit < 0 {
		pendingLimit = 0
	}
	pendingCount := 0
	for _, ticket := range s.Tickets {
		if ticket.AccountID == accountID && ticket.Status == ticketOpen {
			pendingCount++
		}
	}
	if pendingCount >= pendingLimit {
		return RetireTicket{}, ErrPendingLimit
	}

	s.observe(at)
	ticketID := s.nextID("rt")
	ticket := &ticketRecord{
		Seq:         s.Seq,
		TicketID:    ticketID,
		AccountID:   accountID,
		CharacterID: characterID,
		RequestedAt: at,
		PurgeAt:     at.Add(g.limits.RetireCoolDown),
		Status:      ticketOpen,
	}
	s.Tickets[ticketID] = ticket
	character.Status = StatusPending
	character.CurrentTicketID = ticketID
	return ticket.publicTicket(), nil
}

// Cancel withdraws one retire ticket while the cool down is still running.
func (g *CharacterRetireGuard) Cancel(accountID, ticketID string) (CharacterState, error) {
	if !required(accountID) {
		return CharacterState{}, ErrBadArgument
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	ticket, ok := g.state.Tickets[ticketID]
	if !ok {
		return CharacterState{}, ErrTicketNotFound
	}
	if ticket.AccountID != accountID {
		return CharacterState{}, ErrAccountMismatch
	}
	return g.state.cancelTicket(ticket)
}

// Finalize purges the character once the cool down has elapsed.
func (g *CharacterRetireGuard) Finalize(ticketID string, at time.Time) (RetireResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state

	ticket, ok := s.Tickets[ticketID]
	if !ok {
		return RetireResult{}, ErrTicketNotFound
	}
	if ticket.Status != ticketOpen {
		return RetireResult{}, ErrTicketClosed
	}

	if at.Before(ticket.PurgeAt) {
		return RetireResult{}, ErrTooEarly
	}
	s.observe(at)

	character, ok := s.Characters[ticket.CharacterID]
	if !ok {
		return RetireResult{}, ErrCharacterNotFound
	}

	tombstoneID := s.nextID("tb")
	tombstone := &tombstoneRecord{
		Seq:         s.Seq,
		TombstoneID: tombstoneID,
		TicketID:    ticket.TicketID,
		CharacterID: character.CharacterID,
		AccountID:   character.AccountID,
		Name:        character.Name,
		PurgedAt:    at,
		HoldUntil:   at.Add(g.limits.NameHoldDuration),
	}
	s.Tombstones[tombstoneID] = tombstone

	ticket.Status = ticketClosed
	ticket.ClosedAt = at
	character.Status = StatusPurged
	character.CurrentTicketID = ""
	character.TombstoneID = tombstoneID
	s.Names[character.Name] = &nameRecord{
		Name:        character.Name,
		Status:      NameHeld,
		HeldBy:      tombstoneID,
		AccountID:   character.AccountID,
		CharacterID: character.CharacterID,
		TombstoneID: tombstoneID,
		Until:       tombstone.HoldUntil,
	}

	g.state.evictOldestTombstones(g.limits.MaxTombstones)

	return RetireResult{
		TicketID:    ticket.TicketID,
		CharacterID: character.CharacterID,
		Name:        character.Name,
		Purged:      true,
		TombstoneID: tombstoneID,
	}, nil
}

// RestoreCharacter brings a purged character back from its tombstone.
func (g *CharacterRetireGuard) RestoreCharacter(accountID, characterID string) (CharacterState, error) {
	if !required(accountID) || !required(characterID) {
		return CharacterState{}, ErrBadArgument
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state

	character, ok := s.Characters[characterID]
	if !ok {
		return CharacterState{}, ErrCharacterNotFound
	}
	if character.AccountID != accountID {
		return CharacterState{}, ErrAccountMismatch
	}
	if character.Status != StatusPurged {
		return character.publicState(), ErrNotRestorable
	}

	tombstone, ok := s.Tombstones[character.TombstoneID]
	if !ok || tombstone == nil || tombstone.CharacterID != characterID {
		return character.publicState(), ErrNotRestorable
	}

	if name := s.Names[character.Name]; name != nil && !s.nameAvailable(name) {
		sameTombstone := name.TombstoneID == tombstone.TombstoneID || name.HeldBy == tombstone.TombstoneID
		sameOwnerHold := name.Status == NameHeld && name.AccountID == accountID
		sameCharacter := name.CharacterID == characterID && name.Status == NameTaken
		if !sameTombstone && !sameOwnerHold && !sameCharacter {
			return character.publicState(),
				fmt.Errorf("%w: name %q is occupied: %w", ErrNotRestorable, character.Name, ErrNameTaken)
		}
	}

	character.Status = StatusActive
	character.CurrentTicketID = ""
	character.TombstoneID = ""
	s.Names[character.Name] = &nameRecord{
		Name:        character.Name,
		Status:      NameTaken,
		HeldBy:      characterID,
		AccountID:   accountID,
		CharacterID: characterID,
	}
	delete(s.Tombstones, tombstone.TombstoneID)
	return character.publicState(), nil
}

// Login opens a play session; a pending retire is dropped by the login itself.
func (g *CharacterRetireGuard) Login(accountID, characterID string, at time.Time) (CharacterState, error) {
	if !required(accountID) || !required(characterID) {
		return CharacterState{}, ErrBadArgument
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state

	character, ok := s.Characters[characterID]
	if !ok {
		return CharacterState{}, ErrCharacterNotFound
	}
	if character.AccountID != accountID {
		return CharacterState{}, ErrAccountMismatch
	}

	switch character.Status {
	case StatusActive:
		s.observe(at)
		return character.publicState(), nil
	case StatusPending:
		s.observe(at)
		ticket := s.openTicketForCharacter(characterID)
		if ticket == nil {
			character.Status = StatusActive
			character.CurrentTicketID = ""
			return character.publicState(), nil
		}
		return s.cancelTicket(ticket)
	case StatusPurged:
		return character.publicState(), ErrCharacterRetired
	default:
		return CharacterState{}, ErrCharacterNotFound
	}
}

// EnterMatch rejects characters that are pending or already purged.
func (g *CharacterRetireGuard) EnterMatch(characterID string) error {
	if !required(characterID) {
		return ErrBadArgument
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	character, ok := g.state.Characters[characterID]
	if !ok {
		return ErrCharacterNotFound
	}
	if character.Status != StatusActive {
		return ErrCharacterRetired
	}
	return nil
}

// StateOf reports the state of one character.
func (g *CharacterRetireGuard) StateOf(characterID string) (CharacterState, error) {
	if !required(characterID) {
		return CharacterState{}, ErrBadArgument
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	character, ok := g.state.Characters[characterID]
	if !ok {
		return CharacterState{}, ErrCharacterNotFound
	}
	return character.publicState(), nil
}

// NameStatus reports who holds one character name.
func (g *CharacterRetireGuard) NameStatus(name string) (NameState, error) {
	if !required(name) {
		return NameState{}, ErrBadArgument
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	s := g.state

	record, ok := s.Names[name]
	if !ok || s.nameAvailable(record) {
		return NameState{Name: name, Status: NameFree}, nil
	}
	return NameState{
		Name:   name,
		Status: record.Status,
		HeldBy: record.HeldBy,
		Until:  record.Until,
	}, nil
}

// Pending lists the open retire tickets of one account.
func (g *CharacterRetireGuard) Pending(accountID string) ([]RetireTicket, error) {
	if !required(accountID) {
		return nil, ErrBadArgument
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	tickets := make([]*ticketRecord, 0)
	for _, ticket := range g.state.Tickets {
		if ticket.AccountID == accountID && ticket.Status == ticketOpen {
			tickets = append(tickets, ticket)
		}
	}
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].Seq != tickets[j].Seq {
			return tickets[i].Seq < tickets[j].Seq
		}
		return tickets[i].TicketID < tickets[j].TicketID
	})

	result := make([]RetireTicket, 0, len(tickets))
	for _, ticket := range tickets {
		result = append(result, ticket.publicTicket())
	}
	return result, nil
}

// Snapshot serialises characters, tickets, names and tombstones.
func (g *CharacterRetireGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	payload := snapshotEnvelope{Version: snapshotVersion, State: g.state}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// RestoreSnapshot replaces the whole state with a snapshot, all or nothing.
func (g *CharacterRetireGuard) RestoreSnapshot(data []byte) error {
	next, err := stateFromSnapshot(data, g.limits)
	if err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	g.state = next
	g.mu.Unlock()
	return nil
}

func stateFromSnapshot(data []byte, limits Limits) (*guardState, error) {
	if len(data) == 0 {
		return nil, ErrBadSnapshot
	}

	var payload snapshotEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing snapshot data", ErrBadSnapshot)
	}
	if payload.Version != snapshotVersion || payload.State == nil {
		return nil, fmt.Errorf("%w: unsupported snapshot version", ErrBadSnapshot)
	}

	s := payload.State
	if s.Characters == nil {
		s.Characters = make(map[string]*characterRecord)
	}
	if s.Tickets == nil {
		s.Tickets = make(map[string]*ticketRecord)
	}
	if s.Names == nil {
		s.Names = make(map[string]*nameRecord)
	}
	if s.Tombstones == nil {
		s.Tombstones = make(map[string]*tombstoneRecord)
	}

	maxSeq := s.Seq
	for id, character := range s.Characters {
		if character == nil || character.CharacterID != id ||
			!required(character.AccountID) || !required(character.CharacterID) || !required(character.Name) {
			return nil, ErrBadSnapshot
		}
		if character.Status != StatusActive && character.Status != StatusPending &&
			character.Status != StatusPurged {
			return nil, ErrBadSnapshot
		}
	}

	openByAccount := make(map[string]int)
	for id, ticket := range s.Tickets {
		if ticket == nil || ticket.TicketID != id ||
			!required(ticket.AccountID) || !required(ticket.CharacterID) {
			return nil, ErrBadSnapshot
		}
		if ticket.Status != ticketOpen && ticket.Status != ticketCanceled && ticket.Status != ticketClosed {
			return nil, ErrBadSnapshot
		}
		character, ok := s.Characters[ticket.CharacterID]
		if !ok || character.AccountID != ticket.AccountID {
			return nil, ErrBadSnapshot
		}
		if ticket.Status == ticketOpen {
			openByAccount[ticket.AccountID]++
			if character.Status != StatusPending || character.CurrentTicketID != ticket.TicketID {
				return nil, ErrBadSnapshot
			}
		}
		if ticket.Seq > maxSeq {
			maxSeq = ticket.Seq
		}
	}

	for _, character := range s.Characters {
		if character.Status == StatusPending {
			ticket, ok := s.Tickets[character.CurrentTicketID]
			if !ok || ticket == nil || ticket.Status != ticketOpen ||
				ticket.CharacterID != character.CharacterID {
				return nil, ErrBadSnapshot
			}
		}
		if character.TombstoneID != "" {
			tombstone, ok := s.Tombstones[character.TombstoneID]
			if !ok || tombstone == nil || tombstone.CharacterID != character.CharacterID {
				return nil, ErrBadSnapshot
			}
		}
	}

	for name, record := range s.Names {
		if record == nil || record.Name != name || !required(name) {
			return nil, ErrBadSnapshot
		}
		switch record.Status {
		case NameFree, NameHeld:
		case NameTaken:
			character, ok := s.Characters[record.CharacterID]
			if !ok || character.Name != name {
				return nil, ErrBadSnapshot
			}
		default:
			return nil, ErrBadSnapshot
		}
	}

	for id, tombstone := range s.Tombstones {
		if tombstone == nil || tombstone.TombstoneID != id ||
			!required(tombstone.AccountID) || !required(tombstone.CharacterID) || !required(tombstone.Name) {
			return nil, ErrBadSnapshot
		}
		character, ok := s.Characters[tombstone.CharacterID]
		if !ok || character.AccountID != tombstone.AccountID || character.Name != tombstone.Name {
			return nil, ErrBadSnapshot
		}
		if tombstone.Seq > maxSeq {
			maxSeq = tombstone.Seq
		}
	}
	s.Seq = maxSeq

	pendingLimit := limits.MaxPendingPerAccount
	if pendingLimit < 0 {
		pendingLimit = 0
	}
	for _, count := range openByAccount {
		if count > pendingLimit {
			return nil, ErrBadSnapshot
		}
	}
	tombstoneLimit := limits.MaxTombstones
	if tombstoneLimit < 0 {
		tombstoneLimit = 0
	}
	if len(s.Tombstones) > tombstoneLimit {
		return nil, ErrBadSnapshot
	}

	return s, nil
}
