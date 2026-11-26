// Package task models the send side of a one way import link: monotonic
// sequence allocation, a bounded retransmission window and generation tagged
// keys, with out of band acknowledgements.
package task

import "errors"

// errNotImplemented is returned by behaviour that is not wired up yet.
var errNotImplemented = errors.New("not implemented")

// Acknowledgement statuses understood by the bridge.
const (
	AckOK = "ok"
)

// Errors reported by the bridge.
var (
	ErrBadMeta         = errors.New("message metadata is incomplete")
	ErrEmptyPayload    = errors.New("payload must not be empty")
	ErrPayloadTooLarge = errors.New("payload is larger than the configured limit")
	ErrWindowFull      = errors.New("send window is full")
	ErrQueueFull       = errors.New("send queue capacity is exhausted")
	ErrUnknownSeq      = errors.New("sequence number is unknown or already pruned")
	ErrNoAckPending    = errors.New("no acknowledgement is pending")
	ErrBadAck          = errors.New("acknowledgement is malformed or belongs to another generation")
	ErrKeyGenExhausted = errors.New("key generation limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Seq is the strictly monotonic message number.
type Seq uint64

// Meta describes the payload without opening it.
type Meta struct {
	Kind        string
	SourceID    string
	ContentType string
	Correlation string
}

// Message is one retained outbound message.
type Message struct {
	Seq        Seq
	Digest     string
	Generation int
	Size       int
	Payload    []byte
	Acked      bool
}

// Ack is one out of band confirmation.
type Ack struct {
	Seq        Seq
	Generation int
	Status     string
	Note       string
}

// Stats reports the queue, window and key state.
type Stats struct {
	Queued        int
	InFlight      int
	Acked         uint64
	BytesInFlight int64
	Watermark     Seq
	Generation    int
	Blocked       bool
}

// Config holds the hard limits of the bridge.
type Config struct {
	WindowMessages   int
	WindowBytes      int64
	MaxQueueMessages int
	MaxPayloadBytes  int
	MaxGeneration    int
	MaxPollBacklog   int
}

// DiodeBridge owns the send queue, the window and the key generation.
type DiodeBridge struct {
	cfg Config
}

// NewDiodeBridge builds a bridge with the given limits.
func NewDiodeBridge(cfg Config) *DiodeBridge {
	return &DiodeBridge{cfg: cfg}
}

// Submit queues one payload and assigns the next sequence number.
func (b *DiodeBridge) Submit(payload []byte, meta Meta) (Seq, error) {
	return 0, errNotImplemented
}

// AcceptAck processes one acknowledgement from the out of band channel.
func (b *DiodeBridge) AcceptAck(ack Ack) error {
	return errNotImplemented
}

// PollAck returns the next unread acknowledgement, oldest first.
func (b *DiodeBridge) PollAck() (Ack, error) {
	return Ack{}, errNotImplemented
}

// Stats reports queue, window and key counters.
func (b *DiodeBridge) Stats() Stats {
	return Stats{}
}

// RotateKey advances the key generation for new messages.
func (b *DiodeBridge) RotateKey() error {
	return errNotImplemented
}

// Generation returns the current key generation.
func (b *DiodeBridge) Generation() int {
	return 0
}

// Lookup reads one retained message.
func (b *DiodeBridge) Lookup(seq Seq) (Message, bool) {
	return Message{}, false
}

// InFlight lists unacknowledged messages in sequence order.
func (b *DiodeBridge) InFlight() []Message {
	return nil
}

// Resend requeues a retained message without changing its sequence number.
func (b *DiodeBridge) Resend(seq Seq) error {
	return errNotImplemented
}

// Snapshot serialises the queue, the watermark and the key generation.
func (b *DiodeBridge) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the bridge state, all or nothing.
func (b *DiodeBridge) Restore(data []byte) error {
	return errNotImplemented
}
