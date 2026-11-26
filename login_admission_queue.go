// Package task runs the login admission queue of a game: per region pools,
// ordered advancement, heartbeat based expiry, one shot grants and priority
// lanes that must not starve the normal queue.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Ticket status values reported by TicketState.
const (
	TicketWaiting  = "waiting"
	TicketGranted  = "granted"
	TicketAdmitted = "admitted"
	TicketExpired  = "expired"
	TicketLeft     = "left"
)

// Errors reported by the queue.
var (
	ErrBadArgument     = errors.New("account and region are required")
	ErrUnknownRegion   = errors.New("region has no capacity configured")
	ErrInvalidCapacity = errors.New("capacity must not be negative")
	ErrTicketNotFound  = errors.New("ticket is not known")
	ErrTicketExpired   = errors.New("ticket expired after missing heartbeats")
	ErrTicketClosed    = errors.New("ticket is no longer waiting")
	ErrGrantNotFound   = errors.New("grant is not known")
	ErrGrantUsed       = errors.New("grant was already redeemed")
	ErrGrantExpired    = errors.New("grant expired before it was redeemed")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Config holds the pool capacities and the timing budgets of the queue.
type Config struct {
	Capacity        map[string]int
	TicketTTL       time.Duration
	GrantTTL        time.Duration
	BatchSize       int
	PriorityQuota   int
	StarvationLimit int
}

// Ticket is one place in the login queue.
type Ticket struct {
	TicketID   string
	AccountID  string
	Region     string
	Seq        int64
	EnqueuedAt time.Time
	Priority   bool
	Status     string
}

// Position tells a waiting player how many tickets are ahead of it.
type Position struct {
	TicketID string
	Region   string
	Seq      int64
	Ahead    int
}

// Grant is one single use admission voucher.
type Grant struct {
	TicketID  string
	AccountID string
	Region    string
	Nonce     string
	GrantedAt time.Time
	ExpiresAt time.Time
}

// TicketState is the public view of one ticket.
type TicketState struct {
	TicketID string
	AccountID string
	Region   string
	Status   string
	Position int
}

// LoginAdmissionQueue owns the per region pools, the tickets, the outstanding
// grants and the priority whitelist.
type LoginAdmissionQueue struct {
	cfg Config
}

// NewLoginAdmissionQueue builds a queue with the given configuration.
func NewLoginAdmissionQueue(cfg Config) *LoginAdmissionQueue {
	return &LoginAdmissionQueue{cfg: cfg}
}

// SetCapacity sets the seat capacity of one region.
func (q *LoginAdmissionQueue) SetCapacity(region string, capacity int) error {
	return errNotImplemented
}

// MarkPriority adds one account to the priority lane.
func (q *LoginAdmissionQueue) MarkPriority(accountID string) error {
	return errNotImplemented
}

// Enqueue puts one account into the pool of its region.
func (q *LoginAdmissionQueue) Enqueue(accountID, region string, at time.Time) (Ticket, error) {
	return Ticket{}, errNotImplemented
}

// Heartbeat refreshes one ticket and reports its position.
func (q *LoginAdmissionQueue) Heartbeat(ticketID string, at time.Time) (Position, error) {
	return Position{}, errNotImplemented
}

// Admit issues one batch of single use grants for every region with free seats.
func (q *LoginAdmissionQueue) Admit(now time.Time) ([]Grant, error) {
	return nil, errNotImplemented
}

// Redeem consumes a grant and occupies one seat.
func (q *LoginAdmissionQueue) Redeem(ticketID, nonce string, at time.Time) error {
	return errNotImplemented
}

// Leave gives up a waiting or granted ticket.
func (q *LoginAdmissionQueue) Leave(ticketID string) error {
	return errNotImplemented
}

// Status reports the state of one ticket.
func (q *LoginAdmissionQueue) Status(ticketID string) (TicketState, error) {
	return TicketState{}, errNotImplemented
}

// FindTicket looks up the live ticket of one account inside one region.
func (q *LoginAdmissionQueue) FindTicket(accountID, region string) (Ticket, bool) {
	return Ticket{}, false
}

// Grants lists the outstanding grants of one region.
func (q *LoginAdmissionQueue) Grants(region string) []Grant {
	return nil
}

// EvictExpired closes every waiting ticket whose heartbeat went stale.
func (q *LoginAdmissionQueue) EvictExpired(now time.Time) (int, error) {
	return 0, errNotImplemented
}

// Occupied counts the admitted seats of one region.
func (q *LoginAdmissionQueue) Occupied(region string) int {
	return 0
}

// Snapshot serialises pools, tickets, grants and the priority whitelist.
func (q *LoginAdmissionQueue) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (q *LoginAdmissionQueue) Restore(data []byte) error {
	return errNotImplemented
}
