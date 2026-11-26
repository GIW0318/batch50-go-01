// Package task assigns players of one scene map to channel lines: soft and hard
// capacity limits, whole party reservations, switch cooldown, rebalancing and
// snapshot based restarts.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Line status values reported by LineState.
const (
	LineOpen   = "open"
	LineClosed = "closed"
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
	limits Limits
}

// NewChannelLineAssigner builds an assigner with the given limits.
func NewChannelLineAssigner(limits Limits) *ChannelLineAssigner {
	return &ChannelLineAssigner{limits: limits}
}

// OpenLine adds one channel line to a map and returns its public state.
func (a *ChannelLineAssigner) OpenLine(mapID string, capacity int) (LineState, error) {
	return LineState{}, errNotImplemented
}

// CloseLine closes one line and returns the players that were pushed out.
func (a *ChannelLineAssigner) CloseLine(mapID, lineID string, at time.Time) ([]string, error) {
	return nil, errNotImplemented
}

// ReserveParty books whole party seats on one single line.
func (a *ChannelLineAssigner) ReserveParty(mapID, partyID string, members int) (string, error) {
	return "", errNotImplemented
}

// Enter seats one player, following the party binding when there is one.
func (a *ChannelLineAssigner) Enter(mapID, playerID, partyID string, at time.Time) (Assignment, error) {
	return Assignment{}, errNotImplemented
}

// Leave releases the seat of one player.
func (a *ChannelLineAssigner) Leave(mapID, playerID string) error {
	return errNotImplemented
}

// Switch moves one player to another line of the same map.
func (a *ChannelLineAssigner) Switch(mapID, playerID, targetLine string, at time.Time) (Assignment, error) {
	return Assignment{}, errNotImplemented
}

// Rebalance evens the fill level of the lines of one map.
func (a *ChannelLineAssigner) Rebalance(mapID string, at time.Time) ([]Move, error) {
	return nil, errNotImplemented
}

// LockSwitching freezes or unfreezes line switching for a whole map.
func (a *ChannelLineAssigner) LockSwitching(mapID string, locked bool) error {
	return errNotImplemented
}

// IsLocked reports whether line switching is frozen on the map.
func (a *ChannelLineAssigner) IsLocked(mapID string) (bool, error) {
	return false, errNotImplemented
}

// Locate reports where one player currently sits.
func (a *ChannelLineAssigner) Locate(mapID, playerID string) (Assignment, bool) {
	return Assignment{}, false
}

// Lines lists the line states of one map in line order.
func (a *ChannelLineAssigner) Lines(mapID string) ([]LineState, error) {
	return nil, errNotImplemented
}

// Snapshot serialises lines, seating, party bindings and cooldowns.
func (a *ChannelLineAssigner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *ChannelLineAssigner) Restore(data []byte) error {
	return errNotImplemented
}
