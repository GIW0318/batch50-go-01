// Package task runs the login admission queue of a game: per region pools,
// ordered advancement, heartbeat based expiry, one shot grants and priority
// lanes that must not starve the normal queue.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

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

const (
	snapshotVersion    = 1
	minRetentionWindow = 24 * time.Hour
	maxRetainedTickets = 65536
	maxRetainedGrants  = 131072
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
	TicketID  string
	AccountID string
	Region    string
	Status    string
	Position  int
}

type ticketRecord struct {
	Ticket
	LastHeartbeat time.Time
	TerminalAt    time.Time
	ActiveGrant   string
}

type grantRecord struct {
	Grant
	Used      bool
	Revoked   bool
	UsedAt    time.Time
	RevokedAt time.Time
	Ord       uint64
}

type regionPool struct {
	capacity   int
	nextSeq    int64
	waiting    []string
	starvation int
}

type accountRegion struct {
	accountID string
	region    string
}

// LoginAdmissionQueue owns the per region pools, the tickets, the outstanding
// grants and the priority whitelist.
type LoginAdmissionQueue struct {
	mu sync.Mutex

	cfg      Config
	pools    map[string]*regionPool
	tickets  map[string]*ticketRecord
	grants   map[string]*grantRecord
	byTicket map[string][]string

	activeGrants map[string]map[string]struct{}
	accounts     map[accountRegion]string
	priority     map[string]bool
	occupied     map[string]int

	nextTicketID uint64
	nextNonce    uint64
	nextGrantOrd uint64
	lastNow      time.Time
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotState struct {
	Version      int              `json:"version"`
	Pools        []snapshotPool   `json:"pools"`
	Tickets      []snapshotTicket `json:"tickets"`
	Grants       []snapshotGrant  `json:"grants"`
	Priority     []string         `json:"priority"`
	Occupied     map[string]int   `json:"occupied"`
	NextTicketID uint64           `json:"next_ticket_id"`
	NextNonce    uint64           `json:"next_nonce"`
	NextGrantOrd uint64           `json:"next_grant_ord"`
	LastNow      time.Time        `json:"last_now"`
}

type snapshotPool struct {
	Region     string `json:"region"`
	Capacity   int    `json:"capacity"`
	NextSeq    int64  `json:"next_seq"`
	Starvation int    `json:"starvation"`
}

type snapshotTicket struct {
	Ticket        Ticket    `json:"ticket"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	TerminalAt    time.Time `json:"terminal_at"`
	ActiveGrant   string    `json:"active_grant"`
}

type snapshotGrant struct {
	Grant     Grant     `json:"grant"`
	Used      bool      `json:"used"`
	Revoked   bool      `json:"revoked"`
	UsedAt    time.Time `json:"used_at"`
	RevokedAt time.Time `json:"revoked_at"`
	Ord       uint64    `json:"ord"`
}

// NewLoginAdmissionQueue builds a queue with the given configuration.
func NewLoginAdmissionQueue(cfg Config) *LoginAdmissionQueue {
	cfg = copyConfig(cfg)
	q := &LoginAdmissionQueue{
		cfg:          cfg,
		pools:        make(map[string]*regionPool),
		tickets:      make(map[string]*ticketRecord),
		grants:       make(map[string]*grantRecord),
		byTicket:     make(map[string][]string),
		activeGrants: make(map[string]map[string]struct{}),
		accounts:     make(map[accountRegion]string),
		priority:     make(map[string]bool),
		occupied:     make(map[string]int),
	}
	for region, capacity := range cfg.Capacity {
		q.pools[region] = &regionPool{capacity: capacity}
	}
	return q
}

func copyConfig(cfg Config) Config {
	capacity := make(map[string]int, len(cfg.Capacity))
	for region, value := range cfg.Capacity {
		capacity[region] = value
	}
	cfg.Capacity = capacity
	return cfg
}

// SetCapacity sets the seat capacity of one region.
func (q *LoginAdmissionQueue) SetCapacity(region string, capacity int) error {
	if region == "" {
		return ErrBadArgument
	}
	if capacity < 0 {
		return ErrInvalidCapacity
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	pool, ok := q.pools[region]
	if !ok {
		pool = &regionPool{}
		q.pools[region] = pool
	}
	oldCapacity := pool.capacity
	pool.capacity = capacity
	q.cfg.Capacity[region] = capacity

	if capacity >= oldCapacity {
		return nil
	}

	now := q.lastNow
	q.expireGrantsLocked(now)
	live := q.liveGrantCountLocked(region, now)
	excess := q.occupied[region] + live - capacity
	if excess <= 0 {
		return nil
	}

	grantIDs := make([]string, 0, len(q.activeGrants[region]))
	for nonce := range q.activeGrants[region] {
		grantIDs = append(grantIDs, nonce)
	}
	sort.Slice(grantIDs, func(i, j int) bool {
		left := q.grants[grantIDs[i]]
		right := q.grants[grantIDs[j]]
		if left == nil || right == nil {
			return grantIDs[i] > grantIDs[j]
		}
		leftTicket := q.tickets[left.TicketID]
		rightTicket := q.tickets[right.TicketID]
		if leftTicket != nil && rightTicket != nil && leftTicket.Seq != rightTicket.Seq {
			return leftTicket.Seq > rightTicket.Seq
		}
		return left.Ord > right.Ord
	})
	for _, nonce := range grantIDs {
		if excess <= 0 {
			break
		}
		rec := q.grants[nonce]
		if rec == nil || rec.Used || rec.Revoked || now.After(rec.ExpiresAt) {
			continue
		}
		q.revokeGrantLocked(rec, now)
		excess--
	}
	q.cleanupRetentionLocked(now)
	return nil
}

// MarkPriority adds one account to the priority lane.
func (q *LoginAdmissionQueue) MarkPriority(accountID string) error {
	if accountID == "" {
		return ErrBadArgument
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.priority[accountID] = true
	for _, rec := range q.tickets {
		if rec.AccountID == accountID &&
			(rec.Status == TicketWaiting || rec.Status == TicketGranted || rec.Status == TicketAdmitted) {
			rec.Priority = true
		}
	}
	return nil
}

// Enqueue puts one account into the pool of its region.
func (q *LoginAdmissionQueue) Enqueue(accountID, region string, at time.Time) (Ticket, error) {
	if accountID == "" || region == "" {
		return Ticket{}, ErrBadArgument
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	pool, ok := q.pools[region]
	if !ok {
		return Ticket{}, ErrUnknownRegion
	}
	q.advanceNowLocked(at)
	now := q.lastNow

	if existing := q.liveTicketLocked(accountID, region); existing != nil {
		if existing.Status == TicketGranted && existing.ActiveGrant != "" {
			rec := q.grants[existing.ActiveGrant]
			if rec != nil && now.After(rec.ExpiresAt) {
				q.expireGrantLocked(rec, now)
			}
		}
		if existing.Status == TicketWaiting && q.ticketExpiredLocked(existing, now) {
			q.terminateTicketLocked(existing, TicketExpired, now)
		}
		if existing.Status == TicketWaiting || existing.Status == TicketGranted || existing.Status == TicketAdmitted {
			if existing.Status != TicketAdmitted && now.After(existing.LastHeartbeat) {
				existing.LastHeartbeat = now
			}
			existing.Priority = q.priority[accountID]
			return existing.Ticket, nil
		}
	}

	pool.nextSeq++
	ticketID := q.newTicketIDLocked()
	rec := &ticketRecord{
		Ticket: Ticket{
			TicketID:   ticketID,
			AccountID:  accountID,
			Region:     region,
			Seq:        pool.nextSeq,
			EnqueuedAt: at,
			Priority:   q.priority[accountID],
			Status:     TicketWaiting,
		},
		LastHeartbeat: at,
	}
	q.tickets[ticketID] = rec
	q.accounts[accountRegion{accountID: accountID, region: region}] = ticketID
	pool.waiting = append(pool.waiting, ticketID)
	q.cleanupRetentionLocked(now)
	return rec.Ticket, nil
}

// Heartbeat refreshes one ticket and reports its position.
func (q *LoginAdmissionQueue) Heartbeat(ticketID string, at time.Time) (Position, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	rec, ok := q.tickets[ticketID]
	if !ok {
		return Position{}, ErrTicketNotFound
	}
	q.advanceNowLocked(at)
	now := q.lastNow
	q.expireGrantsLocked(now)

	switch rec.Status {
	case TicketExpired:
		return Position{}, ErrTicketExpired
	case TicketLeft, TicketAdmitted:
		return Position{}, ErrTicketClosed
	}
	if q.ticketExpiredLocked(rec, now) {
		q.terminateTicketLocked(rec, TicketExpired, now)
		return Position{}, ErrTicketExpired
	}
	if now.After(rec.LastHeartbeat) {
		rec.LastHeartbeat = now
	}
	rec.Priority = q.priority[rec.AccountID]
	return Position{
		TicketID: rec.TicketID,
		Region:   rec.Region,
		Seq:      rec.Seq,
		Ahead:    q.positionLocked(rec),
	}, nil
}

// Admit issues one batch of single use grants for every region with free seats.
func (q *LoginAdmissionQueue) Admit(now time.Time) ([]Grant, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.advanceNowLocked(now)
	now = q.lastNow
	q.expireGrantsLocked(now)
	q.expireWaitingLocked(now)

	grants := make([]Grant, 0)
	batchSize := q.cfg.BatchSize
	if batchSize <= 0 {
		q.cleanupRetentionLocked(now)
		return grants, nil
	}
	remaining := batchSize
	priorityBudget := q.cfg.PriorityQuota
	if priorityBudget < 0 {
		priorityBudget = 0
	}

	regions := make([]string, 0, len(q.pools))
	for region := range q.pools {
		regions = append(regions, region)
	}
	sort.Strings(regions)

	for _, region := range regions {
		if remaining <= 0 {
			break
		}
		pool := q.pools[region]
		slots := pool.capacity - q.occupied[region] - q.liveGrantCountLocked(region, now)
		if slots <= 0 {
			continue
		}
		take := slots
		if take > remaining {
			take = remaining
		}
		if take <= 0 {
			continue
		}
		picked, priorityUsed := q.pickLocked(pool, take, priorityBudget)
		priorityBudget -= priorityUsed
		for _, rec := range picked {
			grants = append(grants, q.issueGrantLocked(rec, now))
			remaining--
			if remaining == 0 {
				break
			}
		}
	}
	q.cleanupRetentionLocked(now)
	return grants, nil
}

// Redeem consumes a grant and occupies one seat.
func (q *LoginAdmissionQueue) Redeem(ticketID, nonce string, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	rec, ok := q.tickets[ticketID]
	if !ok {
		return ErrTicketNotFound
	}
	q.advanceNowLocked(at)
	now := q.lastNow
	q.expireGrantsLocked(now)

	var grant *grantRecord
	for _, candidate := range q.byTicket[ticketID] {
		record := q.grants[candidate]
		if record != nil && record.Nonce == nonce {
			grant = record
			break
		}
	}
	if grant == nil {
		return ErrGrantNotFound
	}
	if grant.Used {
		return ErrGrantUsed
	}
	if grant.Revoked {
		return ErrGrantNotFound
	}
	if now.After(grant.ExpiresAt) {
		return ErrGrantExpired
	}
	if rec.Status != TicketGranted || rec.ActiveGrant != grant.Nonce {
		return ErrTicketClosed
	}
	pool := q.pools[rec.Region]
	if pool == nil || q.occupied[rec.Region] >= pool.capacity {
		return ErrTicketClosed
	}

	grant.Used = true
	grant.UsedAt = now
	q.removeActiveGrantLocked(grant)
	rec.ActiveGrant = ""
	rec.Status = TicketAdmitted
	rec.TerminalAt = now
	q.occupied[rec.Region]++
	q.cleanupRetentionLocked(now)
	return nil
}

// Leave gives up a waiting or granted ticket.
func (q *LoginAdmissionQueue) Leave(ticketID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	rec, ok := q.tickets[ticketID]
	if !ok {
		return ErrTicketNotFound
	}
	switch rec.Status {
	case TicketWaiting:
		q.terminateTicketLocked(rec, TicketLeft, q.lastNow)
		return nil
	case TicketGranted:
		if rec.ActiveGrant != "" {
			if grant := q.grants[rec.ActiveGrant]; grant != nil {
				q.revokeGrantLocked(grant, q.lastNow)
			}
		}
		q.terminateTicketLocked(rec, TicketLeft, q.lastNow)
		return nil
	default:
		return ErrTicketClosed
	}
}

// Status reports the state of one ticket.
func (q *LoginAdmissionQueue) Status(ticketID string) (TicketState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	rec, ok := q.tickets[ticketID]
	if !ok {
		return TicketState{}, ErrTicketNotFound
	}
	return TicketState{
		TicketID:  rec.TicketID,
		AccountID: rec.AccountID,
		Region:    rec.Region,
		Status:    rec.Status,
		Position:  q.positionLocked(rec),
	}, nil
}

// FindTicket looks up the live ticket of one account inside one region.
func (q *LoginAdmissionQueue) FindTicket(accountID, region string) (Ticket, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	rec := q.liveTicketLocked(accountID, region)
	if rec == nil {
		return Ticket{}, false
	}
	ticket := rec.Ticket
	ticket.Priority = q.priority[accountID]
	return ticket, true
}

// Grants lists the outstanding grants of one region.
func (q *LoginAdmissionQueue) Grants(region string) []Grant {
	q.mu.Lock()
	defer q.mu.Unlock()

	active := q.activeGrants[region]
	records := make([]*grantRecord, 0, len(active))
	for nonce := range active {
		rec := q.grants[nonce]
		if rec == nil || rec.Used || rec.Revoked || q.lastNow.After(rec.ExpiresAt) {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Ord < records[j].Ord })
	result := make([]Grant, 0, len(records))
	for _, rec := range records {
		result = append(result, rec.Grant)
	}
	return result
}

// EvictExpired closes every waiting ticket whose heartbeat went stale.
func (q *LoginAdmissionQueue) EvictExpired(now time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.advanceNowLocked(now)
	now = q.lastNow
	q.expireGrantsLocked(now)
	count := q.expireWaitingLocked(now)
	q.cleanupRetentionLocked(now)
	return count, nil
}

// Occupied counts the admitted seats of one region.
func (q *LoginAdmissionQueue) Occupied(region string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.occupied[region]
}

// Snapshot serialises pools, tickets, grants and the priority whitelist.
func (q *LoginAdmissionQueue) Snapshot() ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	state := snapshotState{
		Version:      snapshotVersion,
		Occupied:     make(map[string]int, len(q.occupied)),
		NextTicketID: q.nextTicketID,
		NextNonce:    q.nextNonce,
		NextGrantOrd: q.nextGrantOrd,
		LastNow:      q.lastNow,
	}
	for region, pool := range q.pools {
		state.Pools = append(state.Pools, snapshotPool{
			Region:     region,
			Capacity:   pool.capacity,
			NextSeq:    pool.nextSeq,
			Starvation: pool.starvation,
		})
	}
	sort.Slice(state.Pools, func(i, j int) bool { return state.Pools[i].Region < state.Pools[j].Region })

	for _, rec := range q.tickets {
		state.Tickets = append(state.Tickets, snapshotTicket{
			Ticket:        rec.Ticket,
			LastHeartbeat: rec.LastHeartbeat,
			TerminalAt:    rec.TerminalAt,
			ActiveGrant:   rec.ActiveGrant,
		})
	}
	sort.Slice(state.Tickets, func(i, j int) bool {
		if state.Tickets[i].Ticket.Region != state.Tickets[j].Ticket.Region {
			return state.Tickets[i].Ticket.Region < state.Tickets[j].Ticket.Region
		}
		if state.Tickets[i].Ticket.Seq != state.Tickets[j].Ticket.Seq {
			return state.Tickets[i].Ticket.Seq < state.Tickets[j].Ticket.Seq
		}
		return state.Tickets[i].Ticket.TicketID < state.Tickets[j].Ticket.TicketID
	})

	for _, rec := range q.grants {
		state.Grants = append(state.Grants, snapshotGrant{
			Grant:     rec.Grant,
			Used:      rec.Used,
			Revoked:   rec.Revoked,
			UsedAt:    rec.UsedAt,
			RevokedAt: rec.RevokedAt,
			Ord:       rec.Ord,
		})
	}
	sort.Slice(state.Grants, func(i, j int) bool { return state.Grants[i].Ord < state.Grants[j].Ord })

	state.Priority = make([]string, 0, len(q.priority))
	for accountID := range q.priority {
		state.Priority = append(state.Priority, accountID)
	}
	sort.Strings(state.Priority)
	for region, count := range q.occupied {
		if count != 0 {
			state.Occupied[region] = count
		}
	}

	rawState, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(rawState)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(digest[:]),
		State:    rawState,
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (q *LoginAdmissionQueue) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return ErrBadSnapshot
	}
	digest := sha256.Sum256(envelope.State)
	if hex.EncodeToString(digest[:]) != envelope.Checksum {
		return ErrBadSnapshot
	}
	var state snapshotState
	if err := json.Unmarshal(envelope.State, &state); err != nil || state.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	pools := make(map[string]*regionPool, len(state.Pools))
	for _, saved := range state.Pools {
		if saved.Region == "" || saved.Capacity < 0 || saved.NextSeq < 0 {
			return ErrBadSnapshot
		}
		if _, exists := pools[saved.Region]; exists {
			return ErrBadSnapshot
		}
		pools[saved.Region] = &regionPool{
			capacity:   saved.Capacity,
			nextSeq:    saved.NextSeq,
			starvation: saved.Starvation,
		}
	}

	priority := make(map[string]bool, len(state.Priority))
	for _, accountID := range state.Priority {
		if accountID == "" {
			return ErrBadSnapshot
		}
		priority[accountID] = true
	}

	tickets := make(map[string]*ticketRecord, len(state.Tickets))
	byTicket := make(map[string][]string)
	accounts := make(map[accountRegion]string)
	seenSeq := make(map[string]map[int64]bool)
	for _, saved := range state.Tickets {
		ticket := saved.Ticket
		if ticket.TicketID == "" || ticket.AccountID == "" || ticket.Region == "" || ticket.Seq <= 0 {
			return ErrBadSnapshot
		}
		if !validStatus(ticket.Status) {
			return ErrBadSnapshot
		}
		pool := pools[ticket.Region]
		if pool == nil {
			return ErrBadSnapshot
		}
		if _, exists := tickets[ticket.TicketID]; exists {
			return ErrBadSnapshot
		}
		if seenSeq[ticket.Region] == nil {
			seenSeq[ticket.Region] = make(map[int64]bool)
		}
		if seenSeq[ticket.Region][ticket.Seq] {
			return ErrBadSnapshot
		}
		seenSeq[ticket.Region][ticket.Seq] = true
		if ticket.Priority && !priority[ticket.AccountID] {
			return ErrBadSnapshot
		}
		rec := &ticketRecord{
			Ticket:        ticket,
			LastHeartbeat: saved.LastHeartbeat,
			TerminalAt:    saved.TerminalAt,
			ActiveGrant:   saved.ActiveGrant,
		}
		tickets[ticket.TicketID] = rec
		if liveStatus(ticket.Status) {
			key := accountRegion{accountID: ticket.AccountID, region: ticket.Region}
			if _, exists := accounts[key]; exists {
				return ErrBadSnapshot
			}
			accounts[key] = ticket.TicketID
		}
	}

	grants := make(map[string]*grantRecord, len(state.Grants))
	activeGrants := make(map[string]map[string]struct{})
	for _, saved := range state.Grants {
		grant := saved.Grant
		if grant.Nonce == "" || grant.TicketID == "" || grant.Region == "" {
			return ErrBadSnapshot
		}
		ticket := tickets[grant.TicketID]
		if ticket == nil || ticket.Region != grant.Region || ticket.AccountID != grant.AccountID {
			return ErrBadSnapshot
		}
		if _, exists := grants[grant.Nonce]; exists {
			return ErrBadSnapshot
		}
		if saved.Used && saved.Revoked {
			return ErrBadSnapshot
		}
		rec := &grantRecord{
			Grant:     grant,
			Used:      saved.Used,
			Revoked:   saved.Revoked,
			UsedAt:    saved.UsedAt,
			RevokedAt: saved.RevokedAt,
			Ord:       saved.Ord,
		}
		grants[grant.Nonce] = rec
		byTicket[grant.TicketID] = append(byTicket[grant.TicketID], grant.Nonce)
	}

	for _, pool := range pools {
		pool.waiting = pool.waiting[:0]
	}
	for _, rec := range tickets {
		if rec.Status == TicketWaiting {
			pools[rec.Region].waiting = append(pools[rec.Region].waiting, rec.TicketID)
		}
		if rec.Status == TicketGranted {
			if rec.ActiveGrant == "" {
				return ErrBadSnapshot
			}
			grant := grants[rec.ActiveGrant]
			if grant == nil || grant.TicketID != rec.TicketID || grant.Used || grant.Revoked {
				return ErrBadSnapshot
			}
			if activeGrants[rec.Region] == nil {
				activeGrants[rec.Region] = make(map[string]struct{})
			}
			activeGrants[rec.Region][rec.ActiveGrant] = struct{}{}
		} else if rec.ActiveGrant != "" {
			return ErrBadSnapshot
		}
	}
	for _, pool := range pools {
		sort.Slice(pool.waiting, func(i, j int) bool {
			return tickets[pool.waiting[i]].Seq < tickets[pool.waiting[j]].Seq
		})
	}

	occupied := make(map[string]int, len(state.Occupied))
	for region, count := range state.Occupied {
		if count < 0 || pools[region] == nil {
			return ErrBadSnapshot
		}
		occupied[region] = count
	}
	for region := range occupied {
		if pools[region] == nil {
			return ErrBadSnapshot
		}
	}
	for region, pool := range pools {
		maxSeq := int64(0)
		for seq := range seenSeq[region] {
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		if pool.nextSeq < maxSeq {
			pool.nextSeq = maxSeq
		}
	}

	capacity := make(map[string]int, len(pools))
	for region, pool := range pools {
		capacity[region] = pool.capacity
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	q.pools = pools
	q.tickets = tickets
	q.grants = grants
	q.byTicket = byTicket
	q.activeGrants = activeGrants
	q.accounts = accounts
	q.priority = priority
	q.occupied = occupied
	q.nextTicketID = state.NextTicketID
	q.nextNonce = state.NextNonce
	q.nextGrantOrd = state.NextGrantOrd
	q.lastNow = state.LastNow
	q.cfg.Capacity = capacity
	return nil
}

func validStatus(status string) bool {
	switch status {
	case TicketWaiting, TicketGranted, TicketAdmitted, TicketExpired, TicketLeft:
		return true
	default:
		return false
	}
}

func liveStatus(status string) bool {
	return status == TicketWaiting || status == TicketGranted || status == TicketAdmitted
}

func (q *LoginAdmissionQueue) advanceNowLocked(now time.Time) {
	if now.After(q.lastNow) {
		q.lastNow = now
	}
}

func (q *LoginAdmissionQueue) ticketExpiredLocked(rec *ticketRecord, now time.Time) bool {
	return now.Sub(rec.LastHeartbeat) > q.cfg.TicketTTL
}

func (q *LoginAdmissionQueue) positionLocked(rec *ticketRecord) int {
	if rec.Status != TicketWaiting {
		return 0
	}
	pool := q.pools[rec.Region]
	if pool == nil {
		return 0
	}
	ahead := 0
	for _, ticketID := range pool.waiting {
		other := q.tickets[ticketID]
		if other != nil && other.Status == TicketWaiting && other.Seq < rec.Seq {
			ahead++
		}
	}
	return ahead
}

func (q *LoginAdmissionQueue) liveTicketLocked(accountID, region string) *ticketRecord {
	key := accountRegion{accountID: accountID, region: region}
	ticketID, ok := q.accounts[key]
	if !ok {
		return nil
	}
	rec := q.tickets[ticketID]
	if rec == nil || !liveStatus(rec.Status) {
		delete(q.accounts, key)
		return nil
	}
	return rec
}

func (q *LoginAdmissionQueue) newTicketIDLocked() string {
	for {
		q.nextTicketID++
		ticketID := "tk-" + strconv.FormatUint(q.nextTicketID, 16)
		if _, exists := q.tickets[ticketID]; !exists {
			return ticketID
		}
	}
}

func (q *LoginAdmissionQueue) newNonceLocked() string {
	for {
		q.nextNonce++
		nonce := "grant-" + strconv.FormatUint(q.nextNonce, 16)
		if _, exists := q.grants[nonce]; !exists {
			return nonce
		}
	}
}

func (q *LoginAdmissionQueue) removeFromWaitingLocked(pool *regionPool, ticketID string) {
	if pool == nil {
		return
	}
	for index, candidate := range pool.waiting {
		if candidate == ticketID {
			copy(pool.waiting[index:], pool.waiting[index+1:])
			pool.waiting = pool.waiting[:len(pool.waiting)-1]
			return
		}
	}
}

func (q *LoginAdmissionQueue) insertWaitingLocked(pool *regionPool, ticketID string) {
	if pool == nil {
		return
	}
	rec := q.tickets[ticketID]
	if rec == nil || rec.Status != TicketWaiting {
		return
	}
	for _, candidate := range pool.waiting {
		if candidate == ticketID {
			return
		}
	}
	index := sort.Search(len(pool.waiting), func(i int) bool {
		other := q.tickets[pool.waiting[i]]
		return other != nil && other.Seq >= rec.Seq
	})
	pool.waiting = append(pool.waiting, "")
	copy(pool.waiting[index+1:], pool.waiting[index:])
	pool.waiting[index] = ticketID
}

func (q *LoginAdmissionQueue) terminateTicketLocked(rec *ticketRecord, status string, at time.Time) {
	if rec == nil {
		return
	}
	if rec.ActiveGrant != "" {
		if grant := q.grants[rec.ActiveGrant]; grant != nil && !grant.Used && !grant.Revoked {
			grant.Revoked = true
			grant.RevokedAt = at
			q.removeActiveGrantLocked(grant)
		}
		rec.ActiveGrant = ""
	}
	rec.Status = status
	rec.TerminalAt = at
	q.removeFromWaitingLocked(q.pools[rec.Region], rec.TicketID)
	key := accountRegion{accountID: rec.AccountID, region: rec.Region}
	if ticketID, ok := q.accounts[key]; ok && ticketID == rec.TicketID {
		delete(q.accounts, key)
	}
}

func (q *LoginAdmissionQueue) addActiveGrantLocked(rec *grantRecord) {
	if q.activeGrants[rec.Region] == nil {
		q.activeGrants[rec.Region] = make(map[string]struct{})
	}
	q.activeGrants[rec.Region][rec.Nonce] = struct{}{}
}

func (q *LoginAdmissionQueue) removeActiveGrantLocked(rec *grantRecord) {
	active := q.activeGrants[rec.Region]
	if active == nil {
		return
	}
	delete(active, rec.Nonce)
	if len(active) == 0 {
		delete(q.activeGrants, rec.Region)
	}
}

func (q *LoginAdmissionQueue) revokeGrantLocked(rec *grantRecord, at time.Time) {
	if rec == nil || rec.Used || rec.Revoked {
		return
	}
	rec.Revoked = true
	rec.RevokedAt = at
	q.removeActiveGrantLocked(rec)
	ticket := q.tickets[rec.TicketID]
	if ticket != nil && ticket.Status == TicketGranted && ticket.ActiveGrant == rec.Nonce {
		ticket.ActiveGrant = ""
		ticket.Status = TicketWaiting
		q.insertWaitingLocked(q.pools[ticket.Region], ticket.TicketID)
	}
}

func (q *LoginAdmissionQueue) expireGrantLocked(rec *grantRecord, now time.Time) {
	if rec == nil || rec.Used || rec.Revoked || !now.After(rec.ExpiresAt) {
		return
	}
	q.removeActiveGrantLocked(rec)
	ticket := q.tickets[rec.TicketID]
	if ticket != nil && ticket.Status == TicketGranted && ticket.ActiveGrant == rec.Nonce {
		ticket.ActiveGrant = ""
		ticket.Status = TicketWaiting
		q.insertWaitingLocked(q.pools[ticket.Region], ticket.TicketID)
	}
}

func (q *LoginAdmissionQueue) expireGrantsLocked(now time.Time) {
	for region, active := range q.activeGrants {
		for nonce := range active {
			rec := q.grants[nonce]
			if rec == nil {
				delete(active, nonce)
				continue
			}
			if now.After(rec.ExpiresAt) {
				q.expireGrantLocked(rec, now)
			}
		}
		if len(active) == 0 {
			delete(q.activeGrants, region)
		}
	}
}

func (q *LoginAdmissionQueue) expireWaitingLocked(now time.Time) int {
	expired := 0
	for _, pool := range q.pools {
		current := append([]string(nil), pool.waiting...)
		for _, ticketID := range current {
			rec := q.tickets[ticketID]
			if rec == nil || rec.Status != TicketWaiting {
				continue
			}
			if q.ticketExpiredLocked(rec, now) {
				q.terminateTicketLocked(rec, TicketExpired, now)
				expired++
			}
		}
	}
	return expired
}

func (q *LoginAdmissionQueue) liveGrantCountLocked(region string, now time.Time) int {
	count := 0
	for nonce := range q.activeGrants[region] {
		rec := q.grants[nonce]
		if rec == nil || rec.Used || rec.Revoked || now.After(rec.ExpiresAt) {
			continue
		}
		count++
	}
	return count
}

func (q *LoginAdmissionQueue) pickLocked(pool *regionPool, limit, priorityBudget int) ([]*ticketRecord, int) {
	if limit <= 0 || pool == nil {
		return nil, 0
	}
	priorityTickets := make([]*ticketRecord, 0)
	normalTickets := make([]*ticketRecord, 0)
	for _, ticketID := range pool.waiting {
		rec := q.tickets[ticketID]
		if rec == nil || rec.Status != TicketWaiting {
			continue
		}
		if q.priority[rec.AccountID] {
			priorityTickets = append(priorityTickets, rec)
		} else {
			normalTickets = append(normalTickets, rec)
		}
	}
	if len(priorityTickets) == 0 && len(normalTickets) == 0 {
		return nil, 0
	}

	quota := priorityBudget
	if configured := q.cfg.PriorityQuota; configured < quota {
		quota = configured
	}
	if quota < 0 {
		quota = 0
	}
	forceNormal := false
	if len(normalTickets) > 0 && q.cfg.StarvationLimit > 0 {
		threshold := q.cfg.StarvationLimit - 1
		if threshold < 0 {
			threshold = 0
		}
		forceNormal = pool.starvation >= threshold
	}
	picked := make([]*ticketRecord, 0, limit)
	priorityAt, normalAt := 0, 0
	priorityUsed, normalUsed := 0, 0

	if forceNormal {
		picked = append(picked, normalTickets[normalAt])
		normalAt++
		normalUsed++
	}
	for len(picked) < limit {
		if priorityAt < len(priorityTickets) && priorityUsed < quota {
			picked = append(picked, priorityTickets[priorityAt])
			priorityAt++
			priorityUsed++
			continue
		}
		if normalAt < len(normalTickets) {
			picked = append(picked, normalTickets[normalAt])
			normalAt++
			normalUsed++
			continue
		}
		break
	}

	if normalUsed > 0 {
		pool.starvation = 0
	} else if priorityUsed > 0 && len(normalTickets) > 0 {
		pool.starvation++
	}
	return picked, priorityUsed
}

func (q *LoginAdmissionQueue) issueGrantLocked(ticket *ticketRecord, now time.Time) Grant {
	q.nextGrantOrd++
	grant := &grantRecord{
		Grant: Grant{
			TicketID:  ticket.TicketID,
			AccountID: ticket.AccountID,
			Region:    ticket.Region,
			Nonce:     q.newNonceLocked(),
			GrantedAt: now,
			ExpiresAt: now.Add(q.cfg.GrantTTL),
		},
		Ord: q.nextGrantOrd,
	}
	q.grants[grant.Nonce] = grant
	q.byTicket[grant.TicketID] = append(q.byTicket[grant.TicketID], grant.Nonce)
	q.addActiveGrantLocked(grant)
	q.removeFromWaitingLocked(q.pools[ticket.Region], ticket.TicketID)
	ticket.Status = TicketGranted
	ticket.ActiveGrant = grant.Nonce
	ticket.Priority = q.priority[ticket.AccountID]
	return grant.Grant
}

func (q *LoginAdmissionQueue) retentionWindowLocked() time.Duration {
	window := minRetentionWindow
	if q.cfg.TicketTTL > 0 && q.cfg.TicketTTL*2 > window {
		window = q.cfg.TicketTTL * 2
	}
	if q.cfg.GrantTTL > 0 && q.cfg.GrantTTL*2 > window {
		window = q.cfg.GrantTTL * 2
	}
	return window
}

func (q *LoginAdmissionQueue) cleanupRetentionLocked(now time.Time) {
	window := q.retentionWindowLocked()
	for ticketID, rec := range q.tickets {
		if !terminalStatus(rec.Status) || rec.TerminalAt.IsZero() {
			continue
		}
		if now.Sub(rec.TerminalAt) > window {
			q.deleteTicketLocked(ticketID)
		}
	}
	q.trimTicketsLocked()

	for nonce, rec := range q.grants {
		if rec.Used || rec.Revoked || now.After(rec.ExpiresAt) {
			retainedAt := rec.ExpiresAt
			if rec.Used {
				retainedAt = rec.UsedAt
			}
			if rec.Revoked {
				retainedAt = rec.RevokedAt
			}
			if !retainedAt.IsZero() && now.Sub(retainedAt) > window {
				q.deleteGrantLocked(nonce)
			}
		}
	}
	q.trimGrantsLocked(now)
}

func (q *LoginAdmissionQueue) trimTicketsLocked() {
	if len(q.tickets) <= maxRetainedTickets {
		return
	}
	type candidate struct {
		id string
		at time.Time
	}
	candidates := make([]candidate, 0, len(q.tickets)-maxRetainedTickets)
	for ticketID, rec := range q.tickets {
		if terminalStatus(rec.Status) {
			candidates = append(candidates, candidate{id: ticketID, at: rec.TerminalAt})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].at.Before(candidates[j].at) })
	for _, item := range candidates {
		if len(q.tickets) <= maxRetainedTickets {
			return
		}
		q.deleteTicketLocked(item.id)
	}
}

func (q *LoginAdmissionQueue) trimGrantsLocked(now time.Time) {
	if len(q.grants) <= maxRetainedGrants {
		return
	}
	type candidate struct {
		nonce string
		at    time.Time
	}
	candidates := make([]candidate, 0, len(q.grants)-maxRetainedGrants)
	for nonce, rec := range q.grants {
		if !rec.Used && !rec.Revoked && !now.After(rec.ExpiresAt) {
			continue
		}
		at := rec.ExpiresAt
		if rec.Used {
			at = rec.UsedAt
		}
		if rec.Revoked {
			at = rec.RevokedAt
		}
		candidates = append(candidates, candidate{nonce: nonce, at: at})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].at.Before(candidates[j].at) })
	for _, item := range candidates {
		if len(q.grants) <= maxRetainedGrants {
			return
		}
		q.deleteGrantLocked(item.nonce)
	}
}

func terminalStatus(status string) bool {
	return status == TicketAdmitted || status == TicketExpired || status == TicketLeft
}

func (q *LoginAdmissionQueue) deleteTicketLocked(ticketID string) {
	rec := q.tickets[ticketID]
	if rec == nil {
		return
	}
	for _, nonce := range append([]string(nil), q.byTicket[ticketID]...) {
		q.deleteGrantLocked(nonce)
	}
	delete(q.byTicket, ticketID)
	delete(q.tickets, ticketID)
	key := accountRegion{accountID: rec.AccountID, region: rec.Region}
	if current, ok := q.accounts[key]; ok && current == ticketID {
		delete(q.accounts, key)
	}
	q.removeFromWaitingLocked(q.pools[rec.Region], ticketID)
}

func (q *LoginAdmissionQueue) deleteGrantLocked(nonce string) {
	rec := q.grants[nonce]
	if rec == nil {
		return
	}
	q.removeActiveGrantLocked(rec)
	delete(q.grants, nonce)
	ids := q.byTicket[rec.TicketID]
	for index, candidate := range ids {
		if candidate == nonce {
			ids = append(ids[:index], ids[index+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(q.byTicket, rec.TicketID)
	} else {
		q.byTicket[rec.TicketID] = ids
	}
}
