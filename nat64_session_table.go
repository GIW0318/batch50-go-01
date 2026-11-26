// Package task keeps the NAT64 data plane ledger of the border gateway: it
// hands out per tenant IPv4 port blocks, translates internal IPv6 endpoints,
// ages sessions per protocol and answers DNS64 synthesis questions.
package task

import (
	"errors"
	"net/netip"
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

// Nat64SessionTable owns tenants, port blocks, sessions and DNS64 state.
type Nat64SessionTable struct {
	cfg      Config
	prefix   netip.Prefix
	tenants  map[string]Tenant
	blocks   map[string][]Block
	mappings map[string]Mapped
}

// NewNat64SessionTable builds a table with the given limits.
func NewNat64SessionTable(cfg Config) *Nat64SessionTable {
	return &Nat64SessionTable{cfg: cfg}
}

// RegisterTenant maps an internal IPv6 prefix to a tenant id.
func (t *Nat64SessionTable) RegisterTenant(tenantID, internalPrefix string) error {
	return errNotImplemented
}

// Reserve allocates a port block of n ports for one tenant.
func (t *Nat64SessionTable) Reserve(tenantID string, n int) (Block, error) {
	return Block{}, errNotImplemented
}

// Translate creates or shares the mapping for one internal endpoint and
// destination pair, returning the external endpoint the peer sees.
func (t *Nat64SessionTable) Translate(src, dst Endpoint, proto uint8) (Mapped, error) {
	return Mapped{}, errNotImplemented
}

// Release drops one reference of a mapping and closes it at zero.
func (t *Nat64SessionTable) Release(mapped Mapped) error {
	return errNotImplemented
}

// Lookup resolves an external endpoint back to its mapping.
func (t *Nat64SessionTable) Lookup(external Endpoint, proto uint8) (Mapped, bool) {
	return Mapped{}, false
}

// Sweep expires idle sessions and reclaims ports whose hold time is over.
func (t *Nat64SessionTable) Sweep(now time.Time) int {
	return 0
}

// PortStats reports exhaustion and queueing counters of one tenant.
func (t *Nat64SessionTable) PortStats(tenantID string) PortStats {
	return PortStats{}
}

// TenantUsage reports blocks and sessions held by one tenant.
func (t *Nat64SessionTable) TenantUsage(tenantID string) TenantUsage {
	return TenantUsage{}
}

// ReverseICMPError maps a quoted external endpoint back to the session it
// belongs to, so an ICMP error can be rewritten for the internal host.
func (t *Nat64SessionTable) ReverseICMPError(external Endpoint, proto uint8) (Mapped, error) {
	return Mapped{}, errNotImplemented
}

// SynthAAAA answers a DNS64 question from A records and real AAAA records.
func (t *Nat64SessionTable) SynthAAAA(name string, aRecords, realAAAA []string) (DNSAnswer, error) {
	return DNSAnswer{}, errNotImplemented
}

// EnterAuthoritativeZone publishes a synthesized answer into the zone.
func (t *Nat64SessionTable) EnterAuthoritativeZone(name string, answer DNSAnswer) error {
	return errNotImplemented
}

// Snapshot serialises tenants, blocks, sessions and counters.
func (t *Nat64SessionTable) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole table state, all or nothing.
func (t *Nat64SessionTable) Restore(data []byte) error {
	return errNotImplemented
}
