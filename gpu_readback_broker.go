// Package task brokers GPU readback requests through per purpose staging
// rings and delivers the finished buffers in submission order.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Defaults applied when the matching BrokerConfig field is left unset.
const (
	DefaultMaxSlotsPerPurpose = 8
	DefaultPoolSlots          = 4
	DefaultMaxBytes           = 1 << 20
	DefaultMaxLatencyTicks    = 3

	snapshotVersion = 1
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

type ticketStatus uint8

const (
	ticketWaiting ticketStatus = iota
	ticketDelivered
	ticketDegraded
	ticketCancelled
	ticketInvalid
)

// ReadbackBroker owns the staging rings, the tickets and the logical clock.
type ReadbackBroker struct {
	mu            sync.Mutex
	cfg           BrokerConfig
	maxSlots      int
	defaultSlots  int
	maxBytes      int64
	latencyBudget int64

	clock      int64
	watermark  Ticket
	totalBytes int64
	purposes   map[string]*purposeState
	tickets    map[Ticket]*ticketState
}

type purposeState struct {
	name     string
	poolSize int
	nextSlot int
	used     int
	nextSeq  int64
	head     int
	order    []Ticket
	slots    []Ticket
	stats    Stats
}

type ticketState struct {
	id          Ticket
	purpose     string
	seq         int64
	size        int
	requestedAt int64
	status      ticketStatus
	submitted   bool
	fenceDone   bool
	slot        int
	data        []byte
	degraded    Result
}

type snapshotFile struct {
	Version           int               `json:"version"`
	TicketWatermark   Ticket            `json:"ticket_watermark"`
	Clock             int64             `json:"clock"`
	Purposes          []snapshotPurpose `json:"purposes"`
	Tickets           []snapshotTicket  `json:"tickets"`
	UnfinishedTickets []Ticket          `json:"unfinished_tickets"`
}

type snapshotPurpose struct {
	Name     string   `json:"name"`
	PoolSize int      `json:"pool_size"`
	NextSeq  int64    `json:"next_seq"`
	Head     int      `json:"head"`
	Order    []Ticket `json:"order"`
	Stats    Stats    `json:"stats"`
}

type snapshotTicket struct {
	ID          Ticket       `json:"id"`
	Purpose     string       `json:"purpose"`
	Seq         int64        `json:"seq"`
	Size        int          `json:"size"`
	RequestedAt int64        `json:"requested_at"`
	Status      ticketStatus `json:"status"`
	Submitted   bool         `json:"submitted"`
	FenceDone   bool         `json:"fence_done"`
	Degraded    *Result      `json:"degraded,omitempty"`
}

// NewReadbackBroker builds a broker with the given caps.
func NewReadbackBroker(cfg BrokerConfig) *ReadbackBroker {
	maxSlots := cfg.MaxSlotsPerPurpose
	if maxSlots <= 0 {
		maxSlots = DefaultMaxSlotsPerPurpose
	}

	defaultSlots := cfg.DefaultPoolSlots
	if defaultSlots <= 0 {
		defaultSlots = DefaultPoolSlots
	}

	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	latencyBudget := cfg.MaxLatencyTicks
	if latencyBudget <= 0 {
		latencyBudget = DefaultMaxLatencyTicks
	}

	return &ReadbackBroker{
		cfg:           cfg,
		maxSlots:      maxSlots,
		defaultSlots:  defaultSlots,
		maxBytes:      maxBytes,
		latencyBudget: latencyBudget,
		purposes:      make(map[string]*purposeState),
		tickets:       make(map[Ticket]*ticketState),
	}
}

// SetClock moves the logical clock forward.
func (b *ReadbackBroker) SetClock(tick int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if tick < 0 || tick < b.clock {
		return ErrBadClock
	}
	b.clock = tick
	return nil
}

// Clock reads the logical clock.
func (b *ReadbackBroker) Clock() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock
}

// SetPoolSize resizes the staging ring of one purpose.
func (b *ReadbackBroker) SetPoolSize(purpose string, slots int) error {
	if !validPurpose(purpose) {
		return ErrBadPurpose
	}
	if slots <= 0 || slots > b.maxSlots {
		return ErrBadPool
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	p := b.purposes[purpose]
	if p == nil {
		b.newPurposeLocked(purpose, slots)
		return nil
	}
	if p.used > slots {
		return ErrPoolBusy
	}

	p.resizeLocked(slots, b.tickets)
	return nil
}

// Request reserves one staging slot and returns its ticket.
func (b *ReadbackBroker) Request(purpose string, size int) (Ticket, error) {
	if !validPurpose(purpose) {
		return 0, ErrBadPurpose
	}
	if size <= 0 || int64(size) > b.maxBytes {
		return 0, ErrBadSize
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	p := b.purposes[purpose]
	poolSize := b.defaultSlots
	used := 0
	if p != nil {
		poolSize = p.poolSize
		used = p.used
	}
	if used >= poolSize {
		return 0, ErrPoolFull
	}
	if int64(size) > b.maxBytes-b.totalBytes {
		return 0, ErrBytesLimit
	}

	if p == nil {
		p = b.newPurposeLocked(purpose, b.defaultSlots)
	}

	b.watermark++
	id := b.watermark
	p.nextSeq++

	ticket := &ticketState{
		id:          id,
		purpose:     purpose,
		seq:         p.nextSeq,
		size:        size,
		requestedAt: b.clock,
		status:      ticketWaiting,
		slot:        -1,
	}
	ticket.slot = p.allocateLocked(id)

	b.tickets[id] = ticket
	p.order = append(p.order, id)
	b.totalBytes += int64(size)
	p.stats.Requests++
	p.stats.InFlight++
	p.stats.BytesInFlight += int64(size)
	p.stats.MaxLatencyTicks = b.latencyBudget

	return id, nil
}

// Submit hands the GPU written bytes of one ticket to the broker.
func (b *ReadbackBroker) Submit(t Ticket, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ticket, ok := b.tickets[t]
	if !ok {
		return ErrUnknownTicket
	}
	if ticket.status == ticketInvalid {
		return ErrTicketInvalidated
	}
	if ticket.status == ticketCancelled {
		return ErrTicketCancelled
	}
	if ticket.submitted {
		return ErrBadState
	}
	if len(data) != ticket.size {
		return ErrSizeMismatch
	}

	ticket.data = append(make([]byte, 0, ticket.size), data...)
	ticket.submitted = true
	return nil
}

// CompleteFence marks the GPU write of one ticket as finished.
func (b *ReadbackBroker) CompleteFence(t Ticket) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ticket, ok := b.tickets[t]
	if !ok {
		return ErrUnknownTicket
	}
	if ticket.status == ticketInvalid {
		return ErrTicketInvalidated
	}

	ticket.fenceDone = true
	b.releaseSlotLocked(ticket)
	return nil
}

// Poll tries to take the result of one ticket.
func (b *ReadbackBroker) Poll(t Ticket) (Result, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ticket, ok := b.tickets[t]
	if !ok {
		return Result{}, false, ErrUnknownTicket
	}

	switch ticket.status {
	case ticketInvalid:
		return Result{}, false, ErrTicketInvalidated
	case ticketCancelled:
		return Result{}, false, ErrTicketCancelled
	case ticketDelivered:
		return Result{}, false, ErrTicketConsumed
	case ticketDegraded:
		return ticket.degraded, true, nil
	}

	if b.clock-ticket.requestedAt > b.latencyBudget {
		result := Result{
			Ticket:   ticket.id,
			Purpose:  ticket.purpose,
			Seq:      ticket.seq,
			Degraded: true,
			At:       b.clock,
		}
		ticket.status = ticketDegraded
		ticket.degraded = result

		if p := b.purposes[ticket.purpose]; p != nil {
			p.stats.Degraded++
			p.stats.InFlight--
			b.advanceOrderLocked(p)
		}
		b.releaseSlotLocked(ticket)
		return result, true, nil
	}

	if !ticket.submitted || !ticket.fenceDone {
		return Result{}, false, nil
	}

	p := b.purposes[ticket.purpose]
	if p == nil {
		return Result{}, false, ErrUnknownTicket
	}

	b.advanceOrderLocked(p)
	if p.head >= len(p.order) || p.order[p.head] != ticket.id {
		return Result{}, false, nil
	}

	result := Result{
		Ticket:  ticket.id,
		Purpose: ticket.purpose,
		Seq:     ticket.seq,
		Data:    ticket.data,
		At:      b.clock,
	}
	ticket.data = nil
	ticket.status = ticketDelivered
	p.stats.Delivered++
	p.stats.InFlight--
	b.advanceOrderLocked(p)
	b.releaseSlotLocked(ticket)

	return result, true, nil
}

// Cancel drops a ticket that has not produced a result yet.
func (b *ReadbackBroker) Cancel(t Ticket) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ticket, ok := b.tickets[t]
	if !ok {
		return ErrUnknownTicket
	}

	switch ticket.status {
	case ticketInvalid:
		return ErrTicketInvalidated
	case ticketCancelled:
		return nil
	case ticketDelivered, ticketDegraded:
		return ErrBadState
	}

	ticket.status = ticketCancelled
	if p := b.purposes[ticket.purpose]; p != nil {
		p.stats.Cancelled++
		p.stats.InFlight--
		b.advanceOrderLocked(p)
	}
	b.releaseSlotLocked(ticket)
	return nil
}

// Latency returns the counter block of one purpose.
func (b *ReadbackBroker) Latency(purpose string) Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.purposes[purpose]
	if !ok {
		return Stats{MaxLatencyTicks: b.latencyBudget}
	}
	stats := p.stats
	stats.MaxLatencyTicks = b.latencyBudget
	return stats
}

// Purposes lists the purposes with a staging ring in ascending order.
func (b *ReadbackBroker) Purposes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	names := make([]string, 0, len(b.purposes))
	for name := range b.purposes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Pending counts the tickets of one purpose that still wait for data.
func (b *ReadbackBroker) Pending(purpose string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.purposes[purpose]
	if !ok {
		return 0
	}
	return p.stats.InFlight
}

// Snapshot serialises the pools, the tickets and the logical clock.
func (b *ReadbackBroker) Snapshot() ([]byte, error) {
	b.mu.Lock()
	snapshot := snapshotFile{
		Version:         snapshotVersion,
		TicketWatermark: b.watermark,
		Clock:           b.clock,
	}

	names := make([]string, 0, len(b.purposes))
	for name := range b.purposes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		p := b.purposes[name]
		purposeSnapshot := snapshotPurpose{
			Name:     p.name,
			PoolSize: p.poolSize,
			NextSeq:  p.nextSeq,
			Head:     p.head,
			Order:    append([]Ticket(nil), p.order...),
			Stats:    p.stats,
		}

		for _, id := range p.order {
			ticket := b.tickets[id]
			if ticket == nil {
				continue
			}

			ticketSnapshot := snapshotTicket{
				ID:          ticket.id,
				Purpose:     ticket.purpose,
				Seq:         ticket.seq,
				Size:        ticket.size,
				RequestedAt: ticket.requestedAt,
				Status:      ticket.status,
				Submitted:   ticket.submitted,
				FenceDone:   ticket.fenceDone,
			}
			if ticket.status == ticketDegraded {
				result := ticket.degraded
				ticketSnapshot.Degraded = &result
			}
			snapshot.Tickets = append(snapshot.Tickets, ticketSnapshot)

			if ticket.status == ticketWaiting {
				snapshot.UnfinishedTickets = append(snapshot.UnfinishedTickets, ticket.id)
			}
		}
		snapshot.Purposes = append(snapshot.Purposes, purposeSnapshot)
	}
	b.mu.Unlock()

	data, err := json.Marshal(&snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal readback snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot payload.
func (b *ReadbackBroker) Restore(data []byte) error {
	var snapshot snapshotFile
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot, b); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.clock = snapshot.Clock
	b.watermark = snapshot.TicketWatermark
	b.totalBytes = 0
	b.purposes = make(map[string]*purposeState, len(snapshot.Purposes))
	b.tickets = make(map[Ticket]*ticketState, len(snapshot.Tickets))

	for _, purposeSnapshot := range snapshot.Purposes {
		stats := purposeSnapshot.Stats
		stats.InFlight = 0
		stats.BytesInFlight = 0
		stats.MaxLatencyTicks = b.latencyBudget

		p := &purposeState{
			name:     purposeSnapshot.Name,
			poolSize: purposeSnapshot.PoolSize,
			nextSeq:  purposeSnapshot.NextSeq,
			head:     purposeSnapshot.Head,
			order:    append([]Ticket(nil), purposeSnapshot.Order...),
			slots:    make([]Ticket, purposeSnapshot.PoolSize),
			stats:    stats,
		}
		b.purposes[p.name] = p
	}

	for _, ticketSnapshot := range snapshot.Tickets {
		status := ticketSnapshot.Status
		submitted := ticketSnapshot.Submitted
		fenceDone := ticketSnapshot.FenceDone
		var degraded Result

		if status == ticketWaiting {
			status = ticketInvalid
			submitted = false
			fenceDone = false
		}
		if status == ticketDegraded && ticketSnapshot.Degraded != nil {
			degraded = *ticketSnapshot.Degraded
		}

		ticket := &ticketState{
			id:          ticketSnapshot.ID,
			purpose:     ticketSnapshot.Purpose,
			seq:         ticketSnapshot.Seq,
			size:        ticketSnapshot.Size,
			requestedAt: ticketSnapshot.RequestedAt,
			status:      status,
			submitted:   submitted,
			fenceDone:   fenceDone,
			slot:        -1,
			degraded:    degraded,
		}
		b.tickets[ticket.id] = ticket
	}

	return nil
}

func validPurpose(purpose string) bool {
	return strings.TrimSpace(purpose) != ""
}

func (b *ReadbackBroker) newPurposeLocked(name string, slots int) *purposeState {
	p := &purposeState{
		name:     name,
		poolSize: slots,
		slots:    make([]Ticket, slots),
		stats:    Stats{MaxLatencyTicks: b.latencyBudget},
	}
	b.purposes[name] = p
	return p
}

func (p *purposeState) allocateLocked(id Ticket) int {
	for i := 0; i < p.poolSize; i++ {
		index := (p.nextSlot + i) % p.poolSize
		if p.slots[index] == 0 {
			p.slots[index] = id
			p.used++
			p.nextSlot = (index + 1) % p.poolSize
			return index
		}
	}
	return -1
}

func (p *purposeState) resizeLocked(size int, tickets map[Ticket]*ticketState) {
	active := make([]Ticket, 0, p.used)
	for _, id := range p.slots {
		if id != 0 {
			active = append(active, id)
		}
	}

	p.slots = make([]Ticket, size)
	for index, id := range active {
		p.slots[index] = id
		if ticket := tickets[id]; ticket != nil {
			ticket.slot = index
		}
	}
	p.poolSize = size
	p.used = len(active)
	p.nextSlot = p.used
	if p.nextSlot >= p.poolSize {
		p.nextSlot = 0
	}
}

func (b *ReadbackBroker) advanceOrderLocked(p *purposeState) {
	for p.head < len(p.order) {
		ticket := b.tickets[p.order[p.head]]
		if ticket != nil && ticket.status == ticketWaiting {
			return
		}
		p.head++
	}
}

func (b *ReadbackBroker) releaseSlotLocked(ticket *ticketState) {
	if ticket.slot < 0 || !ticket.fenceDone {
		return
	}
	if ticket.status != ticketDelivered &&
		ticket.status != ticketDegraded &&
		ticket.status != ticketCancelled {
		return
	}

	if p := b.purposes[ticket.purpose]; p != nil {
		if ticket.slot < len(p.slots) && p.slots[ticket.slot] == ticket.id {
			p.slots[ticket.slot] = 0
			p.used--
			p.stats.BytesInFlight -= int64(ticket.size)
			b.totalBytes -= int64(ticket.size)
		}
	}
	ticket.slot = -1
	ticket.data = nil
}

func validateSnapshot(snapshot *snapshotFile, broker *ReadbackBroker) error {
	badSnapshot := func(message string) error {
		return fmt.Errorf("%w: %s", ErrBadSnapshot, message)
	}

	if snapshot.Version != snapshotVersion {
		return badSnapshot("unsupported version")
	}
	purposes := make(map[string]snapshotPurpose, len(snapshot.Purposes))
	for _, p := range snapshot.Purposes {
		if !validPurpose(p.Name) {
			return badSnapshot("invalid purpose name")
		}
		if _, exists := purposes[p.Name]; exists {
			return badSnapshot("duplicate purpose")
		}
		if p.PoolSize <= 0 || p.PoolSize > broker.maxSlots {
			return badSnapshot("invalid pool size")
		}
		if p.NextSeq < 0 || int64(len(p.Order)) != p.NextSeq {
			return badSnapshot("invalid sequence watermark")
		}
		if p.Head < 0 || p.Head > len(p.Order) {
			return badSnapshot("invalid order head")
		}
		if p.Stats.Requests < 0 || p.Stats.Delivered < 0 || p.Stats.Degraded < 0 ||
			p.Stats.Cancelled < 0 || p.Stats.InFlight < 0 || p.Stats.BytesInFlight < 0 {
			return badSnapshot("negative stats")
		}
		purposes[p.Name] = p
	}

	tickets := make(map[Ticket]snapshotTicket, len(snapshot.Tickets))
	waitingByPurpose := make(map[string]int)
	for _, ticket := range snapshot.Tickets {
		if ticket.ID == 0 || ticket.ID > snapshot.TicketWatermark {
			return badSnapshot("invalid ticket id")
		}
		if _, exists := tickets[ticket.ID]; exists {
			return badSnapshot("duplicate ticket")
		}
		p, exists := purposes[ticket.Purpose]
		if !exists {
			return badSnapshot("unknown ticket purpose")
		}
		if ticket.Seq <= 0 || ticket.Seq > p.NextSeq {
			return badSnapshot("invalid ticket sequence")
		}
		if ticket.Size <= 0 || int64(ticket.Size) > broker.maxBytes {
			return badSnapshot("invalid ticket size")
		}
		if ticket.RequestedAt < 0 || ticket.RequestedAt > snapshot.Clock {
			return badSnapshot("invalid request clock")
		}
		if ticket.Status > ticketInvalid {
			return badSnapshot("invalid ticket status")
		}
		if ticket.Status == ticketWaiting {
			waitingByPurpose[ticket.Purpose]++
		}
		if ticket.Status == ticketDegraded {
			if ticket.Degraded == nil || !ticket.Degraded.Degraded ||
				ticket.Degraded.Ticket != ticket.ID || ticket.Degraded.At < 0 {
				return badSnapshot("invalid degraded result")
			}
		}
		tickets[ticket.ID] = ticket
	}

	for name, waiting := range waitingByPurpose {
		if waiting > purposes[name].PoolSize {
			return badSnapshot("too many unfinished tickets")
		}
		if waiting != purposes[name].Stats.InFlight {
			return badSnapshot("inconsistent pending count")
		}
	}

	unfinished := make(map[Ticket]bool, len(snapshot.UnfinishedTickets))
	for _, id := range snapshot.UnfinishedTickets {
		ticket, exists := tickets[id]
		if !exists || ticket.Status != ticketWaiting {
			return badSnapshot("invalid unfinished ticket")
		}
		if unfinished[id] {
			return badSnapshot("duplicate unfinished ticket")
		}
		unfinished[id] = true
	}

	for _, p := range snapshot.Purposes {
		seen := make(map[Ticket]bool, len(p.Order))
		for _, id := range p.Order {
			ticket, exists := tickets[id]
			if !exists || ticket.Purpose != p.Name || seen[id] {
				return badSnapshot("invalid purpose order")
			}
			seen[id] = true
		}
		for _, ticket := range tickets {
			if ticket.Purpose == p.Name && !seen[ticket.ID] {
				return badSnapshot("ticket missing from order")
			}
		}
	}

	return nil
}
