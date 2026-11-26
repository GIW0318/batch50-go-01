// Package task owns the offline earnings ledger: it converts offline time into
// pending resources, keeps boosts bounded per kind and settles claims once.
package task

import (
	"errors"
	"time"
)

var errNotImplemented = errors.New("not implemented")

// Errors reported by the earnings ledger.
var (
	ErrBadPlayer      = errors.New("player id is required")
	ErrUnknownPlayer  = errors.New("unknown player")
	ErrBadTime        = errors.New("settle time is before the last online moment")
	ErrBadCap         = errors.New("offline cap must be positive")
	ErrBadRate        = errors.New("offline rate is invalid")
	ErrBadBagCapacity = errors.New("bag capacity must not be negative")
	ErrLimit          = errors.New("earnings table limit reached")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Bonus is one multiplicative boost. Bonuses sharing a Kind are folded by
// taking the highest percent; Until marks the moment the boost expires.
type Bonus struct {
	Kind    string
	Percent int
	Until   time.Time
}

// Rate is the production rate of one player. Base is in units per hour.
type Rate struct {
	Base    int64
	Bonuses []Bonus
}

// Reward is one settled amount that is either still pending or already banked.
type Reward struct {
	Player  string
	Amount  int64
	Seconds int64
	Capped  bool
}

// OfflineReport describes one settlement window.
type OfflineReport struct {
	Player  string
	From    time.Time
	To      time.Time
	Seconds int64
	Amount  int64
	Capped  bool
}

// Limits bounds the ledger tables.
type Limits struct {
	MaxPlayers int
	MaxBonuses int
}

// OfflineEarningsLedger owns baselines, pending entries and player balances.
type OfflineEarningsLedger struct {
	limits Limits
}

// NewOfflineEarningsLedger builds a ledger with the given caps.
func NewOfflineEarningsLedger(limits Limits) *OfflineEarningsLedger {
	return &OfflineEarningsLedger{limits: limits}
}

// SetCap sets the maximum offline span that may be settled in one go.
func (l *OfflineEarningsLedger) SetCap(d time.Duration) error {
	return errNotImplemented
}

// SetRate replaces the production rate of one player.
func (l *OfflineEarningsLedger) SetRate(player string, rate Rate) error {
	return errNotImplemented
}

// Record stores the last online moment of one player.
func (l *OfflineEarningsLedger) Record(player string, at time.Time) error {
	return errNotImplemented
}

// Settle turns the offline span into a pending entry.
func (l *OfflineEarningsLedger) Settle(player string, now time.Time) (OfflineReport, error) {
	return OfflineReport{}, errNotImplemented
}

// Preview computes the reward of a settlement without touching the ledger.
func (l *OfflineEarningsLedger) Preview(player string, now time.Time) (Reward, error) {
	return Reward{}, errNotImplemented
}

// Claim moves the pending entry into the player balance.
func (l *OfflineEarningsLedger) Claim(player string) (Reward, error) {
	return Reward{}, errNotImplemented
}

// Pending reads the entry that waits for a claim.
func (l *OfflineEarningsLedger) Pending(player string) (Reward, bool) {
	return Reward{}, false
}

// Bank reads the amount already claimed by one player.
func (l *OfflineEarningsLedger) Bank(player string) int64 {
	return 0
}

// SetBagCapacity limits how many units a claim can move into the bag.
func (l *OfflineEarningsLedger) SetBagCapacity(n int) error {
	return errNotImplemented
}

// Snapshot serialises baselines, rates, pending entries and balances.
func (l *OfflineEarningsLedger) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *OfflineEarningsLedger) Restore(data []byte) error {
	return errNotImplemented
}
