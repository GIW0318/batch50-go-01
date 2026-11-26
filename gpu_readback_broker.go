// Package task brokers GPU readback requests through per purpose staging
// rings and delivers the finished buffers in submission order.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Defaults applied when the matching BrokerConfig field is left unset.
const (
	DefaultMaxSlotsPerPurpose = 8
	DefaultPoolSlots          = 4
	DefaultMaxBytes           = 1 << 20
	DefaultMaxLatencyTicks    = 3
)

// Errors reported by the broker.
var (
	ErrBadPurpose        = errors.New("readback purpose is invalid")
	ErrBadSize           = errors.New("readback size is invalid")
	ErrBadPool           = errors.New("staging pool size is invalid")
	ErrPoolBusy          = errors.New("staging pool is busy")
	ErrPoolFull          = errors.New("staging pool is full")
	ErrBytesLimit        = errors.New("staging byte limit would be exceeded")
	ErrUnknownTicket     = errors.New("ticket is unknown")
	ErrTicketInvalidated = errors.New("ticket was invalidated by a restore")
	ErrTicketCancelled   = errors.New("ticket was cancelled")
	ErrTicketConsumed    = errors.New("ticket result was already taken")
	ErrBadState          = errors.New("ticket state does not allow this call")
	ErrSizeMismatch      = errors.New("payload size does not match the request")
	ErrBadClock          = errors.New("logical clock must not move backwards")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Ticket identifies one readback request.
type Ticket uint64

// BrokerConfig bounds the staging rings and the logical latency budget.
type BrokerConfig struct {
	MaxSlotsPerPurpose int
	DefaultPoolSlots   int
	MaxBytes           int64
	MaxLatencyTicks    int64
}

// Result is one finished readback buffer.
type Result struct {
	Ticket   Ticket `json:"ticket"`
	Purpose  string `json:"purpose"`
	Seq      int64  `json:"seq"`
	Data     []byte `json:"data"`
	Degraded bool   `json:"degraded"`
	At       int64  `json:"at"`
}

// Stats is the counter block of one purpose.
type Stats struct {
	Requests        int   `json:"requests"`
	Delivered       int   `json:"delivered"`
	Degraded        int   `json:"degraded"`
	Cancelled       int   `json:"cancelled"`
	InFlight        int   `json:"in_flight"`
	BytesInFlight   int64 `json:"bytes_in_flight"`
	MaxLatencyTicks int64 `json:"max_latency_ticks"`
}

// ReadbackBroker owns the staging rings, the tickets and the logical clock.
type ReadbackBroker struct {
	cfg BrokerConfig
}

// NewReadbackBroker builds a broker with the given caps.
func NewReadbackBroker(cfg BrokerConfig) *ReadbackBroker {
	return &ReadbackBroker{cfg: cfg}
}

// SetClock moves the logical clock forward.
func (b *ReadbackBroker) SetClock(tick int64) error {
	return errNotImplemented
}

// Clock reads the logical clock.
func (b *ReadbackBroker) Clock() int64 {
	return 0
}

// SetPoolSize resizes the staging ring of one purpose.
func (b *ReadbackBroker) SetPoolSize(purpose string, slots int) error {
	return errNotImplemented
}

// Request reserves one staging slot and returns its ticket.
func (b *ReadbackBroker) Request(purpose string, size int) (Ticket, error) {
	return 0, errNotImplemented
}

// Submit hands the GPU written bytes of one ticket to the broker.
func (b *ReadbackBroker) Submit(t Ticket, data []byte) error {
	return errNotImplemented
}

// CompleteFence marks the GPU write of one ticket as finished.
func (b *ReadbackBroker) CompleteFence(t Ticket) error {
	return errNotImplemented
}

// Poll tries to take the result of one ticket.
func (b *ReadbackBroker) Poll(t Ticket) (Result, bool, error) {
	return Result{}, false, errNotImplemented
}

// Cancel drops a ticket that has not produced a result yet.
func (b *ReadbackBroker) Cancel(t Ticket) error {
	return errNotImplemented
}

// Latency returns the counter block of one purpose.
func (b *ReadbackBroker) Latency(purpose string) Stats {
	return Stats{}
}

// Purposes lists the purposes with a staging ring in ascending order.
func (b *ReadbackBroker) Purposes() []string {
	return nil
}

// Pending counts the tickets of one purpose that still wait for data.
func (b *ReadbackBroker) Pending(purpose string) int {
	return 0
}

// Snapshot serialises the pools, the tickets and the logical clock.
func (b *ReadbackBroker) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot payload.
func (b *ReadbackBroker) Restore(data []byte) error {
	return errNotImplemented
}
