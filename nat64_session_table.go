// Package task keeps the NAT64 data plane ledger of the border gateway: it
// hands out per tenant IPv4 port blocks, translates internal IPv6 endpoints,
// ages sessions per protocol and answers DNS64 synthesis questions.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// errNotImplemented is returned by behaviour that is not wired up yet.
var errNotImplemented = errors.New("not implemented")

// IP protocol numbers understood by the session table.
const (
	ProtoICMP uint8 = 1
	ProtoTCP  uint8 = 6
	ProtoUDP  uint8 = 17
)

// FirstBlockPort is the first port handed out inside every reserved block.
const FirstBlockPort uint16 = 1024

const (
	snapshotVersion = 1
	maxBlockPorts   = 65536 - int(FirstBlockPort)
)

// Errors reported by the table.
var (
	ErrBadEndpoint        = errors.New("endpoint address or port is invalid")
	ErrOutsidePrefix      = errors.New("destination address is outside the nat64 prefix")
	ErrBadTenant          = errors.New("tenant id or internal prefix is invalid")
	ErrTenantExists       = errors.New("tenant is already registered with another prefix")
	ErrUnknownTenant      = errors.New("tenant is not registered")
	ErrBadBlockSize       = errors.New("requested block size is outside the allowed range")
	ErrBlockLimit         = errors.New("per tenant port block limit reached")
	ErrPortExhausted      = errors.New("no free port is available for this tenant")
	ErrSessionLimit       = errors.New("session table is full")
	ErrUnknownMapping     = errors.New("mapping is unknown or already released")
	ErrNoData             = errors.New("name has neither aaaa nor a records")
	ErrSynthAuthoritative = errors.New("synthesized answer cannot enter the authoritative zone")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// Endpoint is one side of a flow. Addr is an IPv6 literal on the internal
// side and an IPv4 literal for embedded destinations and external mappings.
type Endpoint struct {
	Addr string
	Port uint16
}

// Tenant ties an internal IPv6 prefix to one customer.
type Tenant struct {
	ID             string
	InternalPrefix string
}

// Block is one reserved slice of a pool address and its port range.
type Block struct {
	TenantID  string
	IPv4      string
	PortStart uint16
	PortEnd   uint16
}

// Mapped is one translated session.
type Mapped struct {
	SessionID   string
	TenantID    string
	Proto       uint8
	Internal    Endpoint
	Destination Endpoint
	External    Endpoint
	Refs        int
	LastSeen    time.Time
}

// PortStats separates the two ways a translation can fail to get a port.
type PortStats struct {
	Exhausted uint64
	Queued    uint64
}

// TenantUsage reports the live footprint of one tenant.
type TenantUsage struct {
	Blocks   int
	Sessions int
}

// DNSAnswer is the result of one DNS64 question.
type DNSAnswer struct {
	Name          string
	Records       []string
	Synth         bool
	Authoritative bool
}

// Config holds the hard limits of the gateway.
type Config struct {
	Prefix             string
	Pool               []string
	MaxBlocksPerTenant int
	BlockSize          int
	MaxSessions        int
	ReclaimDelay       time.Duration
	QueueTimeout       time.Duration
	TCPIdle            time.Duration
	UDPIdle            time.Duration
	ICMPIdle           time.Duration
}

type portState struct {
	sessionID    string
	reclaimUntil time.Time
}

type blockState struct {
	Block
	addr  netip.Addr
	ports map[uint16]*portState
}

type sessionKey struct {
	proto   uint8
	src     netip.Addr
	srcPort uint16
	dst     netip.Addr
	dstPort uint16
}

type externalKey struct {
	addr  netip.Addr
	port  uint16
	proto uint8
}

type sessionState struct {
	Mapped
	key    sessionKey
	extKey externalKey
	block  *blockState
}

// Nat64SessionTable owns tenants, port blocks, sessions and DNS64 state.
type Nat64SessionTable struct {
	mu sync.Mutex

	cfg      Config
	prefix   netip.Prefix
	prefixOK bool
	pool     []netip.Addr

	tenants        map[string]Tenant
	tenantOrder    []string
	tenantPrefixes map[string]netip.Prefix

	blocks   map[string][]*blockState
	usedPool map[netip.Addr]bool

	sessions   map[string]*sessionState
	byKey      map[sessionKey]string
	byExternal map[externalKey]string

	stats         map[string]PortStats
	nextSessionID uint64
	now           time.Time
	notify        chan struct{}
	zone          map[string]DNSAnswer
}

// NewNat64SessionTable builds a table with the given limits.
func NewNat64SessionTable(cfg Config) *Nat64SessionTable {
	t := &Nat64SessionTable{
		cfg:            cfg,
		tenants:        make(map[string]Tenant),
		tenantPrefixes: make(map[string]netip.Prefix),
		blocks:         make(map[string][]*blockState),
		usedPool:       make(map[netip.Addr]bool),
		sessions:       make(map[string]*sessionState),
		byKey:          make(map[sessionKey]string),
		byExternal:     make(map[externalKey]string),
		stats:          make(map[string]PortStats),
		now:            time.Now(),
		notify:         make(chan struct{}),
		zone:           make(map[string]DNSAnswer),
	}
	if p, err := netip.ParsePrefix(cfg.Prefix); err == nil && p.Addr().Is6() {
		t.prefix = p.Masked()
		t.prefixOK = true
	}
	t.pool = parsePool(cfg.Pool)
	return t
}

func parsePool(values []string) []netip.Addr {
	seen := make(map[netip.Addr]bool)
	pool := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if !addr.Is4() || addr.Zone() != "" || seen[addr] {
			continue
		}
		seen[addr] = true
		pool = append(pool, addr)
	}
	return pool
}

func (t *Nat64SessionTable) initLocked() {
	if t.tenants == nil {
		t.tenants = make(map[string]Tenant)
	}
	if t.tenantPrefixes == nil {
		t.tenantPrefixes = make(map[string]netip.Prefix)
	}
	if t.blocks == nil {
		t.blocks = make(map[string][]*blockState)
	}
	if t.usedPool == nil {
		t.usedPool = make(map[netip.Addr]bool)
	}
	if t.sessions == nil {
		t.sessions = make(map[string]*sessionState)
	}
	if t.byKey == nil {
		t.byKey = make(map[sessionKey]string)
	}
	if t.byExternal == nil {
		t.byExternal = make(map[externalKey]string)
	}
	if t.stats == nil {
		t.stats = make(map[string]PortStats)
	}
	if t.zone == nil {
		t.zone = make(map[string]DNSAnswer)
	}
	if t.notify == nil {
		t.notify = make(chan struct{})
	}
	if t.now.IsZero() {
		t.now = time.Now()
	}
}

func (t *Nat64SessionTable) signalLocked() {
	if t.notify == nil {
		t.notify = make(chan struct{})
		return
	}
	close(t.notify)
	t.notify = make(chan struct{})
}

func (t *Nat64SessionTable) advanceClockLocked(now time.Time) time.Time {
	if t.now.IsZero() || now.After(t.now) {
		t.now = now
	}
	return t.now
}

// RegisterTenant maps an internal IPv6 prefix to a tenant id.
func (t *Nat64SessionTable) RegisterTenant(tenantID, internalPrefix string) error {
	if strings.TrimSpace(tenantID) == "" {
		return ErrBadTenant
	}
	prefix, err := netip.ParsePrefix(internalPrefix)
	if err != nil || !prefix.Addr().Is6() {
		return ErrBadTenant
	}
	prefix = prefix.Masked()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	if existing, ok := t.tenants[tenantID]; ok {
		if existing.InternalPrefix == prefix.String() {
			return nil
		}
		return ErrTenantExists
	}
	t.tenants[tenantID] = Tenant{ID: tenantID, InternalPrefix: prefix.String()}
	t.tenantPrefixes[tenantID] = prefix
	t.tenantOrder = append(t.tenantOrder, tenantID)
	return nil
}

// Reserve allocates a port block of n ports for one tenant.
func (t *Nat64SessionTable) Reserve(tenantID string, n int) (Block, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()
	return t.addBlockLocked(tenantID, n)
}

func (t *Nat64SessionTable) addBlockLocked(tenantID string, n int) (Block, error) {
	if n < 1 || n > t.cfg.BlockSize || n > maxBlockPorts {
		return Block{}, ErrBadBlockSize
	}
	if _, ok := t.tenants[tenantID]; !ok {
		return Block{}, ErrUnknownTenant
	}
	if t.cfg.MaxBlocksPerTenant <= 0 || len(t.blocks[tenantID]) >= t.cfg.MaxBlocksPerTenant {
		return Block{}, ErrBlockLimit
	}
	addr, ok := t.nextPoolAddrLocked()
	if !ok {
		return Block{}, ErrPortExhausted
	}
	block := &blockState{
		Block: Block{
			TenantID:  tenantID,
			IPv4:      addr.String(),
			PortStart: FirstBlockPort,
			PortEnd:   FirstBlockPort + uint16(n-1),
		},
		addr:  addr,
		ports: make(map[uint16]*portState),
	}
	t.blocks[tenantID] = append(t.blocks[tenantID], block)
	t.usedPool[addr] = true
	t.signalLocked()
	return block.Block, nil
}

func (t *Nat64SessionTable) nextPoolAddrLocked() (netip.Addr, bool) {
	for _, addr := range t.pool {
		if !t.usedPool[addr] {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

func (t *Nat64SessionTable) canAddBlockLocked(tenantID string) bool {
	if t.cfg.BlockSize < 1 || t.cfg.BlockSize > maxBlockPorts {
		return false
	}
	if t.cfg.MaxBlocksPerTenant <= 0 || len(t.blocks[tenantID]) >= t.cfg.MaxBlocksPerTenant {
		return false
	}
	_, ok := t.nextPoolAddrLocked()
	return ok
}

// Translate creates or shares the mapping for one internal endpoint and
// destination pair, returning the external endpoint the peer sees.
func (t *Nat64SessionTable) Translate(src, dst Endpoint, proto uint8) (Mapped, error) {
	if !validProto(proto) || src.Port == 0 || dst.Port == 0 {
		return Mapped{}, ErrBadEndpoint
	}
	srcAddr, err := parseEndpointAddress(src.Addr)
	if err != nil {
		if strings.Contains(src.Addr, ":") {
			return Mapped{}, ErrUnknownTenant
		}
		return Mapped{}, err
	}
	dstAddr, err := parseEndpointAddress(dst.Addr)
	if err != nil {
		return Mapped{}, err
	}
	if !t.prefixOK || !dstAddr.Is6() || !t.prefix.Contains(dstAddr) {
		return Mapped{}, ErrOutsidePrefix
	}
	dstV4, ok := embeddedIPv4(dstAddr)
	if !ok {
		return Mapped{}, ErrOutsidePrefix
	}

	key := sessionKey{
		proto:   proto,
		src:     srcAddr,
		srcPort: src.Port,
		dst:     dstV4,
		dstPort: dst.Port,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	var deadline time.Time
	queued := false
	for {
		now := t.advanceClockLocked(time.Now())

		if id, ok := t.byKey[key]; ok {
			session := t.sessions[id]
			if session != nil {
				session.Refs++
				session.LastSeen = now
				return session.Mapped, nil
			}
			delete(t.byKey, key)
		}

		tenantID, ok := t.tenantForAddrLocked(srcAddr)
		if !ok {
			return Mapped{}, ErrUnknownTenant
		}
		if t.cfg.MaxSessions >= 0 && len(t.sessions) >= t.cfg.MaxSessions {
			return Mapped{}, ErrSessionLimit
		}

		block, port, ok := t.findFreePortLocked(tenantID, now)
		if !ok && t.canAddBlockLocked(tenantID) {
			if _, addErr := t.addBlockLocked(tenantID, t.cfg.BlockSize); addErr == nil {
				block, port, ok = t.findFreePortLocked(tenantID, now)
			} else if addErr != ErrPortExhausted && addErr != ErrBlockLimit && addErr != ErrBadBlockSize {
				return Mapped{}, addErr
			}
		}
		if ok {
			sessionID := t.newSessionIDLocked()
			block.ports[port] = &portState{sessionID: sessionID}
			extKey := externalKey{addr: block.addr, port: port, proto: proto}
			session := &sessionState{
				Mapped: Mapped{
					SessionID:   sessionID,
					TenantID:    tenantID,
					Proto:       proto,
					Internal:    src,
					Destination: Endpoint{Addr: dstV4.String(), Port: dst.Port},
					External:    Endpoint{Addr: block.IPv4, Port: port},
					Refs:        1,
					LastSeen:    now,
				},
				key:    key,
				extKey: extKey,
				block:  block,
			}
			t.sessions[sessionID] = session
			t.byKey[key] = sessionID
			t.byExternal[extKey] = sessionID
			return session.Mapped, nil
		}

		if !queued {
			if t.cfg.QueueTimeout <= 0 {
				t.incrementExhaustedLocked(tenantID)
				return Mapped{}, ErrPortExhausted
			}
			queued = true
			stats := t.stats[tenantID]
			stats.Queued++
			t.stats[tenantID] = stats
			deadline = time.Now().Add(t.cfg.QueueTimeout)
		}

		waitUntil := deadline
		if next, ok := t.nextReclaimLocked(tenantID, now); ok && next.Before(waitUntil) {
			waitUntil = next
		}
		if !time.Now().Before(waitUntil) {
			return Mapped{}, ErrPortExhausted
		}
		notify := t.notify
		wait := time.Until(waitUntil)
		t.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-notify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		t.mu.Lock()
		t.initLocked()
	}
}

func (t *Nat64SessionTable) incrementExhaustedLocked(tenantID string) {
	stats := t.stats[tenantID]
	stats.Exhausted++
	t.stats[tenantID] = stats
}

func (t *Nat64SessionTable) tenantForAddrLocked(addr netip.Addr) (string, bool) {
	best := ""
	bestBits := -1
	for _, tenantID := range t.tenantOrder {
		prefix, ok := t.tenantPrefixes[tenantID]
		if !ok {
			continue
		}
		if prefix.Contains(addr) && prefix.Bits() > bestBits {
			best = tenantID
			bestBits = prefix.Bits()
		}
	}
	return best, best != ""
}

func (t *Nat64SessionTable) findFreePortLocked(tenantID string, now time.Time) (*blockState, uint16, bool) {
	for _, block := range t.blocks[tenantID] {
		for port := int(block.PortStart); port <= int(block.PortEnd); port++ {
			p := uint16(port)
			state, ok := block.ports[p]
			if !ok {
				return block, p, true
			}
			if state.sessionID != "" {
				continue
			}
			if !now.Before(state.reclaimUntil) {
				delete(block.ports, p)
				return block, p, true
			}
		}
	}
	return nil, 0, false
}

func (t *Nat64SessionTable) nextReclaimLocked(tenantID string, now time.Time) (time.Time, bool) {
	var soonest time.Time
	found := false
	for _, block := range t.blocks[tenantID] {
		for _, state := range block.ports {
			if state.sessionID != "" || !state.reclaimUntil.After(now) {
				continue
			}
			if !found || state.reclaimUntil.Before(soonest) {
				soonest = state.reclaimUntil
				found = true
			}
		}
	}
	return soonest, found
}

func (t *Nat64SessionTable) newSessionIDLocked() string {
	for {
		t.nextSessionID++
		id := fmt.Sprintf("sess-%d", t.nextSessionID)
		if _, exists := t.sessions[id]; !exists {
			return id
		}
	}
}

// Release drops one reference of a mapping and closes it at zero.
func (t *Nat64SessionTable) Release(mapped Mapped) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	if mapped.SessionID == "" {
		return ErrUnknownMapping
	}
	session, ok := t.sessions[mapped.SessionID]
	if !ok {
		return ErrUnknownMapping
	}
	session.Refs--
	if session.Refs <= 0 {
		now := t.advanceClockLocked(time.Now())
		t.closeSessionLocked(session, now)
		t.signalLocked()
	}
	return nil
}

func (t *Nat64SessionTable) closeSessionLocked(session *sessionState, now time.Time) {
	delete(t.sessions, session.SessionID)
	if t.byKey[session.key] == session.SessionID {
		delete(t.byKey, session.key)
	}
	if t.byExternal[session.extKey] == session.SessionID {
		delete(t.byExternal, session.extKey)
	}
	if session.block == nil {
		return
	}
	port := session.Mapped.External.Port
	if t.cfg.ReclaimDelay > 0 {
		session.block.ports[port] = &portState{reclaimUntil: now.Add(t.cfg.ReclaimDelay)}
	} else {
		delete(session.block.ports, port)
	}
}

// Lookup resolves an external endpoint back to its mapping.
func (t *Nat64SessionTable) Lookup(external Endpoint, proto uint8) (Mapped, bool) {
	addr, err := parseEndpointAddress(external.Addr)
	if err != nil {
		return Mapped{}, false
	}
	addr = addr.Unmap()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	id, ok := t.byExternal[externalKey{addr: addr, port: external.Port, proto: proto}]
	if !ok {
		return Mapped{}, false
	}
	session, ok := t.sessions[id]
	if !ok {
		return Mapped{}, false
	}
	return session.Mapped, true
}

// Sweep expires idle sessions and reclaims ports whose hold time is over.
func (t *Nat64SessionTable) Sweep(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	current := t.advanceClockLocked(now)
	expired := 0
	for _, session := range t.sessions {
		idle := t.idleForProto(session.Proto)
		if idle > 0 && current.Sub(session.LastSeen) >= idle {
			t.closeSessionLocked(session, current)
			expired++
		}
	}

	freed := false
	for _, tenantBlocks := range t.blocks {
		for _, block := range tenantBlocks {
			for port, state := range block.ports {
				if state.sessionID == "" && !current.Before(state.reclaimUntil) {
					delete(block.ports, port)
					freed = true
				}
			}
		}
	}
	if expired > 0 || freed {
		t.signalLocked()
	}
	return expired
}

func (t *Nat64SessionTable) idleForProto(proto uint8) time.Duration {
	switch proto {
	case ProtoTCP:
		return t.cfg.TCPIdle
	case ProtoUDP:
		return t.cfg.UDPIdle
	case ProtoICMP:
		return t.cfg.ICMPIdle
	default:
		return 0
	}
}

// PortStats reports exhaustion and queueing counters of one tenant.
func (t *Nat64SessionTable) PortStats(tenantID string) PortStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()
	return t.stats[tenantID]
}

// TenantUsage reports blocks and sessions held by one tenant.
func (t *Nat64SessionTable) TenantUsage(tenantID string) TenantUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	usage := TenantUsage{Blocks: len(t.blocks[tenantID])}
	for _, session := range t.sessions {
		if session.TenantID == tenantID {
			usage.Sessions++
		}
	}
	return usage
}

// ReverseICMPError maps a quoted external endpoint back to the session it
// belongs to, so an ICMP error can be rewritten for the internal host.
func (t *Nat64SessionTable) ReverseICMPError(external Endpoint, proto uint8) (Mapped, error) {
	addr, err := parseEndpointAddress(external.Addr)
	if err != nil {
		return Mapped{}, err
	}
	addr = addr.Unmap()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	id, ok := t.byExternal[externalKey{addr: addr, port: external.Port, proto: proto}]
	if !ok {
		return Mapped{}, ErrUnknownMapping
	}
	session, ok := t.sessions[id]
	if !ok {
		return Mapped{}, ErrUnknownMapping
	}
	return session.Mapped, nil
}

// SynthAAAA answers a DNS64 question from A records and real AAAA records.
func (t *Nat64SessionTable) SynthAAAA(name string, aRecords, realAAAA []string) (DNSAnswer, error) {
	if len(realAAAA) > 0 {
		records := make([]string, 0, len(realAAAA))
		for _, record := range realAAAA {
			addr, err := parseEndpointAddress(record)
			if err != nil || !addr.Is6() {
				return DNSAnswer{}, ErrBadEndpoint
			}
			records = append(records, record)
		}
		return DNSAnswer{Name: name, Records: records, Synth: false, Authoritative: false}, nil
	}
	if len(aRecords) == 0 {
		return DNSAnswer{}, ErrNoData
	}
	if !t.prefixOK {
		return DNSAnswer{}, ErrBadEndpoint
	}

	records := make([]string, 0, len(aRecords))
	for _, record := range aRecords {
		addr, err := parseEndpointAddress(record)
		if err != nil || !addr.Is4() {
			return DNSAnswer{}, ErrBadEndpoint
		}
		synth, ok := t.synthesizeIPv4(addr.Unmap())
		if !ok {
			return DNSAnswer{}, ErrBadEndpoint
		}
		records = append(records, synth.String())
	}
	return DNSAnswer{Name: name, Records: records, Synth: true, Authoritative: false}, nil
}

func (t *Nat64SessionTable) synthesizeIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !t.prefixOK || !addr.Is4() {
		return netip.Addr{}, false
	}
	base := t.prefix.Masked().Addr().As16()
	v4 := addr.As4()
	copy(base[12:], v4[:])
	return netip.AddrFrom16(base), true
}

// EnterAuthoritativeZone publishes a synthesized answer into the zone.
func (t *Nat64SessionTable) EnterAuthoritativeZone(name string, answer DNSAnswer) error {
	if answer.Synth {
		return ErrSynthAuthoritative
	}
	if len(answer.Records) == 0 {
		return ErrNoData
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	published := cloneAnswer(answer)
	published.Name = name
	published.Authoritative = true
	t.zone[name] = published
	return nil
}

func cloneAnswer(answer DNSAnswer) DNSAnswer {
	answer.Records = append([]string(nil), answer.Records...)
	return answer
}

type snapshotData struct {
	Version       int                  `json:"version"`
	Clock         time.Time            `json:"clock"`
	Tenants       []Tenant             `json:"tenants"`
	Blocks        []snapshotBlock      `json:"blocks"`
	Sessions      []snapshotSession    `json:"sessions"`
	Counters      snapshotCounters     `json:"counters"`
	Authoritative map[string]DNSAnswer `json:"authoritative,omitempty"`
}

type snapshotBlock struct {
	TenantID  string         `json:"tenant_id"`
	IPv4      string         `json:"ipv4"`
	PortStart uint16         `json:"port_start"`
	PortEnd   uint16         `json:"port_end"`
	Ports     []snapshotPort `json:"ports,omitempty"`
}

type snapshotPort struct {
	Port         uint16    `json:"port"`
	SessionID    string    `json:"session_id,omitempty"`
	ReclaimUntil time.Time `json:"reclaim_until,omitempty"`
}

type snapshotSession struct {
	SessionID   string    `json:"session_id"`
	TenantID    string    `json:"tenant_id"`
	Proto       uint8     `json:"proto"`
	Internal    Endpoint  `json:"internal"`
	Destination Endpoint  `json:"destination"`
	External    Endpoint  `json:"external"`
	Refs        int       `json:"refs"`
	LastSeen    time.Time `json:"last_seen"`
}

type snapshotCounters struct {
	NextSessionID uint64               `json:"next_session_id"`
	PortStats     map[string]PortStats `json:"port_stats,omitempty"`
}

// Snapshot serialises tenants, blocks, sessions and counters.
func (t *Nat64SessionTable) Snapshot() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	clock := t.advanceClockLocked(time.Now())
	snap := snapshotData{
		Version:       snapshotVersion,
		Clock:         clock,
		Counters:      snapshotCounters{NextSessionID: t.nextSessionID, PortStats: make(map[string]PortStats, len(t.stats))},
		Authoritative: make(map[string]DNSAnswer, len(t.zone)),
	}
	for _, tenantID := range t.tenantOrder {
		if tenant, ok := t.tenants[tenantID]; ok {
			snap.Tenants = append(snap.Tenants, tenant)
		}
	}
	for _, tenantID := range t.tenantOrder {
		for _, block := range t.blocks[tenantID] {
			sb := snapshotBlock{
				TenantID:  block.TenantID,
				IPv4:      block.IPv4,
				PortStart: block.PortStart,
				PortEnd:   block.PortEnd,
			}
			ports := make([]uint16, 0, len(block.ports))
			for port := range block.ports {
				ports = append(ports, port)
			}
			sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
			for _, port := range ports {
				state := block.ports[port]
				sb.Ports = append(sb.Ports, snapshotPort{
					Port:         port,
					SessionID:    state.sessionID,
					ReclaimUntil: state.reclaimUntil,
				})
			}
			snap.Blocks = append(snap.Blocks, sb)
		}
	}
	sessionIDs := make([]string, 0, len(t.sessions))
	for sessionID := range t.sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		session := t.sessions[sessionID]
		snap.Sessions = append(snap.Sessions, snapshotSession{
			SessionID:   session.SessionID,
			TenantID:    session.TenantID,
			Proto:       session.Proto,
			Internal:    session.Internal,
			Destination: session.Destination,
			External:    session.External,
			Refs:        session.Refs,
			LastSeen:    session.LastSeen,
		})
	}
	for tenantID, stats := range t.stats {
		snap.Counters.PortStats[tenantID] = stats
	}
	for name, answer := range t.zone {
		snap.Authoritative[name] = cloneAnswer(answer)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole table state, all or nothing.
func (t *Nat64SessionTable) Restore(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()

	var snap snapshotData
	if len(data) == 0 || json.Unmarshal(data, &snap) != nil || snap.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	tenants := make(map[string]Tenant)
	tenantPrefixes := make(map[string]netip.Prefix)
	tenantOrder := make([]string, 0, len(snap.Tenants))
	for _, tenant := range snap.Tenants {
		if strings.TrimSpace(tenant.ID) == "" {
			return ErrBadSnapshot
		}
		if _, duplicate := tenants[tenant.ID]; duplicate {
			return ErrBadSnapshot
		}
		prefix, err := netip.ParsePrefix(tenant.InternalPrefix)
		if err != nil || !prefix.Addr().Is6() {
			return ErrBadSnapshot
		}
		prefix = prefix.Masked()
		tenants[tenant.ID] = Tenant{ID: tenant.ID, InternalPrefix: prefix.String()}
		tenantPrefixes[tenant.ID] = prefix
		tenantOrder = append(tenantOrder, tenant.ID)
	}

	blocks := make(map[string][]*blockState)
	blockByAddr := make(map[netip.Addr]*blockState)
	usedPool := make(map[netip.Addr]bool)
	for _, sb := range snap.Blocks {
		if _, ok := tenants[sb.TenantID]; !ok {
			return ErrBadSnapshot
		}
		addr, err := netip.ParseAddr(sb.IPv4)
		if err != nil {
			return ErrBadSnapshot
		}
		addr = addr.Unmap()
		if !addr.Is4() || addr.Zone() != "" || !t.poolContains(addr) || usedPool[addr] {
			return ErrBadSnapshot
		}
		if sb.PortStart != FirstBlockPort || sb.PortEnd < sb.PortStart {
			return ErrBadSnapshot
		}
		size := int(sb.PortEnd) - int(sb.PortStart) + 1
		if size < 1 || size > t.cfg.BlockSize || size > maxBlockPorts {
			return ErrBadSnapshot
		}
		if t.cfg.MaxBlocksPerTenant <= 0 || len(blocks[sb.TenantID]) >= t.cfg.MaxBlocksPerTenant {
			return ErrBadSnapshot
		}
		block := &blockState{
			Block: Block{
				TenantID:  sb.TenantID,
				IPv4:      addr.String(),
				PortStart: sb.PortStart,
				PortEnd:   sb.PortEnd,
			},
			addr:  addr,
			ports: make(map[uint16]*portState),
		}
		seenPorts := make(map[uint16]bool)
		for _, sp := range sb.Ports {
			if sp.Port < sb.PortStart || sp.Port > sb.PortEnd || seenPorts[sp.Port] {
				return ErrBadSnapshot
			}
			seenPorts[sp.Port] = true
			if sp.SessionID == "" {
				if !sp.ReclaimUntil.IsZero() {
					block.ports[sp.Port] = &portState{reclaimUntil: sp.ReclaimUntil}
				}
				continue
			}
			if !sp.ReclaimUntil.IsZero() {
				return ErrBadSnapshot
			}
			block.ports[sp.Port] = &portState{sessionID: sp.SessionID}
		}
		blocks[sb.TenantID] = append(blocks[sb.TenantID], block)
		blockByAddr[addr] = block
		usedPool[addr] = true
	}

	sessions := make(map[string]*sessionState)
	byKey := make(map[sessionKey]string)
	byExternal := make(map[externalKey]string)
	for _, ss := range snap.Sessions {
		if ss.SessionID == "" || ss.Refs < 1 || !validProto(ss.Proto) {
			return ErrBadSnapshot
		}
		if _, duplicate := sessions[ss.SessionID]; duplicate {
			return ErrBadSnapshot
		}
		if t.cfg.MaxSessions >= 0 && len(sessions) >= t.cfg.MaxSessions {
			return ErrBadSnapshot
		}
		prefix, ok := tenantPrefixes[ss.TenantID]
		if !ok {
			return ErrBadSnapshot
		}
		srcAddr, err := parseEndpointAddress(ss.Internal.Addr)
		if err != nil || !srcAddr.Is6() || !prefix.Contains(srcAddr) || ss.Internal.Port == 0 {
			return ErrBadSnapshot
		}
		dstAddr, err := parseEndpointAddress(ss.Destination.Addr)
		if err != nil {
			return ErrBadSnapshot
		}
		dstAddr = dstAddr.Unmap()
		if !dstAddr.Is4() || ss.Destination.Port == 0 {
			return ErrBadSnapshot
		}
		extAddr, err := parseEndpointAddress(ss.External.Addr)
		if err != nil {
			return ErrBadSnapshot
		}
		extAddr = extAddr.Unmap()
		if !extAddr.Is4() {
			return ErrBadSnapshot
		}
		block := blockByAddr[extAddr]
		if block == nil || block.TenantID != ss.TenantID || ss.External.Port < block.PortStart || ss.External.Port > block.PortEnd {
			return ErrBadSnapshot
		}
		slot := block.ports[ss.External.Port]
		if slot == nil || slot.sessionID != ss.SessionID {
			return ErrBadSnapshot
		}
		key := sessionKey{
			proto:   ss.Proto,
			src:     srcAddr,
			srcPort: ss.Internal.Port,
			dst:     dstAddr,
			dstPort: ss.Destination.Port,
		}
		extKey := externalKey{addr: extAddr, port: ss.External.Port, proto: ss.Proto}
		if _, duplicate := byKey[key]; duplicate {
			return ErrBadSnapshot
		}
		if _, duplicate := byExternal[extKey]; duplicate {
			return ErrBadSnapshot
		}
		session := &sessionState{
			Mapped: Mapped{
				SessionID:   ss.SessionID,
				TenantID:    ss.TenantID,
				Proto:       ss.Proto,
				Internal:    ss.Internal,
				Destination: Endpoint{Addr: dstAddr.String(), Port: ss.Destination.Port},
				External:    Endpoint{Addr: block.IPv4, Port: ss.External.Port},
				Refs:        ss.Refs,
				LastSeen:    ss.LastSeen,
			},
			key:    key,
			extKey: extKey,
			block:  block,
		}
		sessions[ss.SessionID] = session
		byKey[key] = ss.SessionID
		byExternal[extKey] = ss.SessionID
	}

	slotOwners := make(map[string]bool)
	for _, tenantID := range tenantOrder {
		for _, block := range blocks[tenantID] {
			for port, state := range block.ports {
				if state.sessionID == "" {
					continue
				}
				session, ok := sessions[state.sessionID]
				if !ok || slotOwners[state.sessionID] {
					return ErrBadSnapshot
				}
				if session.block != block || session.External.Port != port || session.TenantID != block.TenantID {
					return ErrBadSnapshot
				}
				slotOwners[state.sessionID] = true
			}
		}
	}
	for sessionID := range sessions {
		if !slotOwners[sessionID] {
			return ErrBadSnapshot
		}
	}

	stats := make(map[string]PortStats, len(snap.Counters.PortStats))
	for tenantID, value := range snap.Counters.PortStats {
		if _, ok := tenants[tenantID]; !ok {
			return ErrBadSnapshot
		}
		stats[tenantID] = value
	}
	zone := make(map[string]DNSAnswer, len(snap.Authoritative))
	for name, answer := range snap.Authoritative {
		if answer.Synth {
			return ErrBadSnapshot
		}
		zone[name] = cloneAnswer(answer)
	}

	t.tenants = tenants
	t.tenantOrder = tenantOrder
	t.tenantPrefixes = tenantPrefixes
	t.blocks = blocks
	t.usedPool = usedPool
	t.sessions = sessions
	t.byKey = byKey
	t.byExternal = byExternal
	t.stats = stats
	t.nextSessionID = snap.Counters.NextSessionID
	t.zone = zone
	t.now = t.advanceClockLocked(time.Now())
	if snap.Clock.After(t.now) {
		t.now = snap.Clock
	}
	t.signalLocked()
	return nil
}

func (t *Nat64SessionTable) poolContains(addr netip.Addr) bool {
	for _, candidate := range t.pool {
		if candidate == addr {
			return true
		}
	}
	return false
}

func parseEndpointAddress(value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(value)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, ErrBadEndpoint
	}
	return addr, nil
}

func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	bytes := addr.As16()
	var v4 [4]byte
	copy(v4[:], bytes[12:])
	return netip.AddrFrom4(v4), true
}

func validProto(proto uint8) bool {
	return proto == ProtoTCP || proto == ProtoUDP || proto == ProtoICMP
}
