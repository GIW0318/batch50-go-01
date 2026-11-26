// Package task coordinates hero swaps between players after the pick phase:
// one pending request per player, a two way atomic exchange and a roster that
// closes when the match countdown ends.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Swap request states.
const (
	SwapPending   = "pending"
	SwapAccepted  = "accepted"
	SwapDeclined  = "declined"
	SwapCancelled = "cancelled"
	SwapExpired   = "expired"
)

// Roster states.
const (
	RosterOpen   = "open"
	RosterClosed = "closed"
)

// Errors returned by the coordinator.
var (
	ErrBlankMatch        = errors.New("match id is required")
	ErrEmptyRoster       = errors.New("roster needs players with heroes")
	ErrDuplicateHero     = errors.New("two players picked the same hero")
	ErrUnknownMatch      = errors.New("unknown match")
	ErrRosterLocked      = errors.New("roster of this match is already locked")
	ErrRosterClosed      = errors.New("roster is closed, swaps are over")
	ErrUnknownPlayer     = errors.New("player is not in the roster")
	ErrSelfSwap          = errors.New("a player cannot swap with themself")
	ErrPendingExists     = errors.New("player already has a pending swap")
	ErrRequestLimit      = errors.New("swap request quota reached for this match")
	ErrUnknownRequest    = errors.New("unknown swap request")
	ErrRequestNotPending = errors.New("swap request is not pending")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// SwapRequest is one player asking another for the hero.
type SwapRequest struct {
	RequestID  string    `json:"request_id"`
	MatchID    string    `json:"match_id"`
	FromPlayer string    `json:"from_player"`
	ToPlayer   string    `json:"to_player"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// SwapPair records one completed exchange.
type SwapPair struct {
	PlayerA string `json:"player_a"`
	PlayerB string `json:"player_b"`
	HeroA   string `json:"hero_a"`
	HeroB   string `json:"hero_b"`
}

// SwapResult is what an accepted request produced.
type SwapResult struct {
	RequestID string     `json:"request_id"`
	MatchID   string     `json:"match_id"`
	State     string     `json:"state"`
	Pairs     []SwapPair `json:"pairs"`
	Seq       int64      `json:"seq"`
}

// Limits bounds the live requests and the quota of one match.
type Limits struct {
	MaxPendingPerMatch  int
	MaxRequestsPerMatch int
	RequestTTL          time.Duration
}

// HeroSwapCoordinator owns the rosters, the pending requests and the sequence.
type HeroSwapCoordinator struct {
	limits Limits
}

// NewHeroSwapCoordinator builds a coordinator with the given caps.
func NewHeroSwapCoordinator(limits Limits) *HeroSwapCoordinator {
	return &HeroSwapCoordinator{limits: limits}
}

// LockRoster stores the final picks of one match before the countdown.
func (c *HeroSwapCoordinator) LockRoster(matchID string, picks map[string]string) error {
	return errNotImplemented
}

// Request asks another player for a swap.
func (c *HeroSwapCoordinator) Request(matchID, fromPlayer, toPlayer string) (SwapRequest, error) {
	return SwapRequest{}, errNotImplemented
}

// Accept commits the exchange for one pending request.
func (c *HeroSwapCoordinator) Accept(matchID, requestID string) (SwapResult, error) {
	return SwapResult{}, errNotImplemented
}

// Decline turns a pending request down and leaves the heroes alone.
func (c *HeroSwapCoordinator) Decline(matchID, requestID string) error {
	return errNotImplemented
}

// Cancel withdraws a pending request.
func (c *HeroSwapCoordinator) Cancel(matchID, requestID string) error {
	return errNotImplemented
}

// Expire folds the pending requests whose lifetime ran out.
func (c *HeroSwapCoordinator) Expire(now time.Time) (int, error) {
	return 0, errNotImplemented
}

// CloseRoster seals a match when the countdown reaches its last stage.
func (c *HeroSwapCoordinator) CloseRoster(matchID string) error {
	return errNotImplemented
}

// Roster reads the current hero of every player of one match.
func (c *HeroSwapCoordinator) Roster(matchID string) (map[string]string, error) {
	return nil, errNotImplemented
}

// Pending lists the pending requests of one match, oldest first.
func (c *HeroSwapCoordinator) Pending(matchID string) []SwapRequest {
	return nil
}

// Snapshot serialises the rosters, the requests and the sequence.
func (c *HeroSwapCoordinator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *HeroSwapCoordinator) Restore(data []byte) error {
	return errNotImplemented
}
