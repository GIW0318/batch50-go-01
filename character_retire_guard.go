// Package task runs the character retire flow of one game account: retire
// requests, the cool down window, cancellation while the window is open, the
// final purge, name retention and tombstone based restores.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	limits Limits
}

// NewCharacterRetireGuard builds a guard with the given limits.
func NewCharacterRetireGuard(limits Limits) *CharacterRetireGuard {
	return &CharacterRetireGuard{limits: limits}
}

// Register adds one active character and claims its name.
func (g *CharacterRetireGuard) Register(accountID, characterID, name string) (CharacterState, error) {
	return CharacterState{}, errNotImplemented
}

// Request opens a retire ticket for one character.
func (g *CharacterRetireGuard) Request(accountID, characterID string, at time.Time) (RetireTicket, error) {
	return RetireTicket{}, errNotImplemented
}

// Cancel withdraws one retire ticket while the cool down is still running.
func (g *CharacterRetireGuard) Cancel(accountID, ticketID string) (CharacterState, error) {
	return CharacterState{}, errNotImplemented
}

// Finalize purges the character once the cool down has elapsed.
func (g *CharacterRetireGuard) Finalize(ticketID string, at time.Time) (RetireResult, error) {
	return RetireResult{}, errNotImplemented
}

// RestoreCharacter brings a purged character back from its tombstone.
func (g *CharacterRetireGuard) RestoreCharacter(accountID, characterID string) (CharacterState, error) {
	return CharacterState{}, errNotImplemented
}

// Login opens a play session; a pending retire is dropped by the login itself.
func (g *CharacterRetireGuard) Login(accountID, characterID string, at time.Time) (CharacterState, error) {
	return CharacterState{}, errNotImplemented
}

// EnterMatch rejects characters that are pending or already purged.
func (g *CharacterRetireGuard) EnterMatch(characterID string) error {
	return errNotImplemented
}

// StateOf reports the state of one character.
func (g *CharacterRetireGuard) StateOf(characterID string) (CharacterState, error) {
	return CharacterState{}, errNotImplemented
}

// NameStatus reports who holds one character name.
func (g *CharacterRetireGuard) NameStatus(name string) (NameState, error) {
	return NameState{}, errNotImplemented
}

// Pending lists the open retire tickets of one account.
func (g *CharacterRetireGuard) Pending(accountID string) ([]RetireTicket, error) {
	return nil, errNotImplemented
}

// Snapshot serialises characters, tickets, names and tombstones.
func (g *CharacterRetireGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// RestoreSnapshot replaces the whole state with a snapshot, all or nothing.
func (g *CharacterRetireGuard) RestoreSnapshot(data []byte) error {
	return errNotImplemented
}
