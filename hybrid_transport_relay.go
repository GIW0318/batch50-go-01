// Package task pairs a desktop client with a phone authenticator over a short
// lived relay, forwards one login challenge through the paired channel and
// keeps only the assertion digest after the relay finishes.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Relay states.
const (
	RelayPending   = "pending"
	RelayAttached  = "attached"
	RelayForwarded = "forwarded"
	RelayCompleted = "completed"
	RelayCanceled  = "canceled"
	RelayExpired   = "expired"
)

// Channel state once the phone finished attaching.
const (
	ChannelOpen = "open"
)

// Audit event kinds.
const (
	EventStarted   = "started"
	EventAttached  = "attached"
	EventForwarded = "forwarded"
	EventCompleted = "completed"
	EventCanceled  = "canceled"
	EventExpired   = "expired"
)

// Audit actors and reason codes.
const (
	ActorDesktop = "desktop"
	ActorPhone   = "phone"

	ReasonPairTokenMismatch = "reason_pair_token_mismatch"
	ReasonCanceledByDesktop = "reason_canceled_by_desktop"
	ReasonCanceledByPhone   = "reason_canceled_by_phone"
	ReasonRelayTimeout      = "reason_relay_timeout"
	ReasonOrderConflict     = "reason_order_conflict"
	ReasonLimitReached      = "reason_limit_reached"
)

// Errors returned by the relay.
var (
	ErrEmptyIdentity     = errors.New("desktop id and phone id are required")
	ErrRelayNotFound     = errors.New("relay not found")
	ErrRelayLimit        = errors.New("pending relay limit reached for this desktop")
	ErrBadLimit          = errors.New("relay limits must not be negative")
	ErrAttachDenied      = errors.New("relay attachment denied")
	ErrRelayExpired      = errors.New("relay expired")
	ErrRelayClosed       = errors.New("relay already finished")
	ErrChannelNotFound   = errors.New("channel not found")
	ErrChallengeTooLarge = errors.New("challenge exceeds the relay byte budget")
	ErrEmptyChallenge    = errors.New("challenge is empty")
	ErrEmptyAssertion    = errors.New("assertion is empty")
	ErrBadActor          = errors.New("cancel must name the desktop or the phone")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Limits bounds relay lifetime, concurrency and audit retention.
type Limits struct {
	MaxPendingPerDesktop int
	DefaultTTL           time.Duration
	MaxTTL               time.Duration
	MaxChallengeBytes    int
	MaxAudit             int
	Now                  func() time.Time
}

// RelayOpts narrows the lifetime and the byte budget of one relay.
type RelayOpts struct {
	TTL      time.Duration
	MaxBytes int
}

// Proof is the near field evidence presented by the phone when attaching.
type Proof struct {
	Token    string
	PhoneKey string
	Nonce    []byte
}

// Relay is one short lived pairing attempt between a desktop and a phone.
type Relay struct {
	ID        string
	DesktopID string
	State     string
	PairToken string
	CreatedAt time.Time
	ExpiresAt time.Time
	MaxBytes  int
	Seq       int64
}

// Channel is the paired transport that carries the challenge.
type Channel struct {
	ID         string
	RelayID    string
	DesktopID  string
	PhoneID    string
	State      string
	Credential string
	Forwards   int
	Seq        int64
}

// Result is the terminal outcome of a relay.
type Result struct {
	RelayID     string
	State       string
	Digest      string
	Bytes       int
	CompletedAt time.Time
	Seq         int64
}

// Event is one audit entry. A completed relay keeps the digest only, never
// the assertion bytes.
type Event struct {
	Kind   string
	Actor  string
	Reason string
	Digest string
	At     time.Time
	Seq    int64
}

// HybridTransportRelay owns the relay table, the channel table, the counters
// and the audit trail.
type HybridTransportRelay struct {
	limits Limits
}

// NewHybridTransportRelay builds a relay service with the given caps.
func NewHybridTransportRelay(limits Limits) *HybridTransportRelay {
	return &HybridTransportRelay{limits: limits}
}

// Start opens a pending relay for one desktop.
func (r *HybridTransportRelay) Start(desktopID string, opts RelayOpts) (Relay, error) {
	return Relay{}, errNotImplemented
}

// Attach binds a phone to a pending relay through the near field proof.
func (r *HybridTransportRelay) Attach(relayID string, phoneID string, proof Proof) (Channel, error) {
	return Channel{}, errNotImplemented
}

// Forward sends one challenge through the channel and returns the envelope.
func (r *HybridTransportRelay) Forward(channelID string, challenge []byte) ([]byte, error) {
	return nil, errNotImplemented
}

// Complete records the assertion returned by the phone.
func (r *HybridTransportRelay) Complete(relayID string, assertion []byte) (Result, error) {
	return Result{}, errNotImplemented
}

// Cancel closes a relay from one of its two ends.
func (r *HybridTransportRelay) Cancel(relayID string, by string) error {
	return errNotImplemented
}

// Rebind rebuilds the transport of a channel after the phone changed network.
func (r *HybridTransportRelay) Rebind(channelID string, proof Proof) (Channel, error) {
	return Channel{}, errNotImplemented
}

// RelayState returns the state of one relay, or an empty string when unknown.
func (r *HybridTransportRelay) RelayState(relayID string) string {
	return ""
}

// ChannelInfo reads one channel.
func (r *HybridTransportRelay) ChannelInfo(channelID string) (Channel, bool) {
	return Channel{}, false
}

// Pending counts the relays of one desktop that have not finished yet.
func (r *HybridTransportRelay) Pending(desktopID string) int {
	return 0
}

// Audit returns the per relay audit trail, oldest first.
func (r *HybridTransportRelay) Audit(relayID string) []Event {
	return nil
}

// Sweep expires every relay whose deadline passed and returns the count.
func (r *HybridTransportRelay) Sweep() int {
	return 0
}

// Snapshot serialises relays, channels, counters and the audit trail.
func (r *HybridTransportRelay) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *HybridTransportRelay) Restore(data []byte) error {
	return errNotImplemented
}
