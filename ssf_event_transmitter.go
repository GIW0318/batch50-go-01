// Package task signs security event tokens, fans them out to the receivers
// whose subscription matches the event, and keeps a delivery ledger with a
// bounded retry plan.
package task

import (
	"errors"
	"sync"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Event types understood by the transmitter.
const (
	EventSessionRevoked    = "session-revoked"
	EventAccountDisabled   = "account-disabled"
	EventCredentialChanged = "credential-changed"
)

// Delivery states recorded in the ledger.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	StatusGone      = "gone"
)

// Errors returned by the transmitter.
var (
	ErrBlankTenant      = errors.New("tenant id is required")
	ErrUnknownEventType = errors.New("event type is not recognised")
	ErrEmptySubject     = errors.New("event subject range is empty")
	ErrBlankReceiver    = errors.New("receiver id and endpoint are required")
	ErrNoEventTypes     = errors.New("receiver must subscribe to at least one event type")
	ErrNoSigningKey     = errors.New("no signing key for this tenant")
	ErrUnknownEvent     = errors.New("unknown event id")
	ErrUnknownReceiver  = errors.New("receiver is not part of this event")
	ErrAlreadyDelivered = errors.New("event already delivered to this receiver")
	ErrDeliveryExpired  = errors.New("delivery attempts exhausted")
	ErrQueueFull        = errors.New("in flight queue is full")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// EventID identifies one published security event.
type EventID string

// SecurityEvent is one signal pushed to the subscribers.
type SecurityEvent struct {
	TenantID string   `json:"tenant_id"`
	Type     string   `json:"type"`
	Subject  string   `json:"subject"`
	Scope    []string `json:"scope"`
	IssuedAt int64    `json:"issued_at"`
}

// ReceiverSpec is one subscription. An empty Subjects list matches any
// subject of the event scope.
type ReceiverSpec struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	Endpoint    string   `json:"endpoint"`
	EventTypes  []string `json:"event_types"`
	Subjects    []string `json:"subjects"`
	MaxAttempts int      `json:"max_attempts"`
}

// DeliveryState is one ledger row: the delivery of one event to one receiver.
// NextIn is the backoff in seconds before the next attempt, computed as two to
// the power of attempts minus one and capped at 300.
type DeliveryState struct {
	ReceiverID string `json:"receiver_id"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	LastCode   int    `json:"last_code"`
	NextIn     int64  `json:"next_in"`
}

// Sender pushes one signed token to one receiver endpoint and returns the HTTP
// status code the receiver answered with.
type Sender interface {
	Deliver(receiverID, endpoint, token string) (int, error)
}

// Limits bounds the ledger and the in flight queue.
type Limits struct {
	MaxEvents   int
	MaxInFlight int
}

// SetEventTransmitter owns subscriptions, the signing keys and the ledger.
type SetEventTransmitter struct {
	mu     sync.Mutex
	sender Sender
	limits Limits
}

// NewSetEventTransmitter builds a transmitter on top of a sender.
func NewSetEventTransmitter(sender Sender, limits Limits) *SetEventTransmitter {
	return &SetEventTransmitter{sender: sender, limits: limits}
}

// SetKey installs the signing key of one tenant.
func (t *SetEventTransmitter) SetKey(tenantID string, key []byte) error {
	return errNotImplemented
}

// Subscribe creates or replaces one subscription. Only events published after
// the change are matched against the new spec.
func (t *SetEventTransmitter) Subscribe(spec ReceiverSpec) error {
	return errNotImplemented
}

// Publish signs and delivers one event to every matching receiver and returns
// its ledger id.
func (t *SetEventTransmitter) Publish(evt SecurityEvent) (EventID, error) {
	return "", errNotImplemented
}

// Delivery reports the ledger rows of one event.
func (t *SetEventTransmitter) Delivery(eventID string) ([]DeliveryState, error) {
	return nil, errNotImplemented
}

// Retry re-attempts one delivery with the token it was first signed with.
func (t *SetEventTransmitter) Retry(eventID, receiverID string) error {
	return errNotImplemented
}

// Subscribers lists the current subscriptions of one tenant.
func (t *SetEventTransmitter) Subscribers(tenantID string) []ReceiverSpec {
	return nil
}

// Pending counts the deliveries that are still waiting for a retry.
func (t *SetEventTransmitter) Pending() int {
	return 0
}

// Snapshot serialises subscriptions, keys, the ledger and the retry plan.
func (t *SetEventTransmitter) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (t *SetEventTransmitter) Restore(data []byte) error {
	return errNotImplemented
}
