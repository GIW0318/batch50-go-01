// Package task runs the guest network admission gate: unauthorised clients are
// diverted to the portal, only the walled garden stays reachable before an
// authorisation lease is issued and bound to a device fingerprint.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Client states tracked by the gate.
const (
	ClientUnknown    = "unknown"
	ClientPending    = "pending"
	ClientAuthorized = "authorized"
	ClientExpired    = "expired"
	ClientBlocked    = "blocked"
)

// Lease states tracked by the gate.
const (
	LeaseActive  = "active"
	LeaseExpired = "expired"
	LeaseRevoked = "revoked"
)

// Request phases at which the gate is consulted.
const (
	PhaseResolve = "resolve"
	PhaseConnect = "connect"
)

// Verdict actions.
const (
	ActionPortal = "portal"
	ActionAllow  = "allow"
	ActionDeny   = "deny"
)

// Reason codes attached to a verdict.
const (
	ReasonPortalIntercept = "reason_portal_intercept"
	ReasonWalledGarden    = "reason_walled_garden"
	ReasonAuthorized      = "reason_authorized"
	ReasonPortalAsset     = "reason_portal_asset"
	ReasonLeaseExpired    = "reason_lease_expired"
	ReasonBlocked         = "reason_blocked"
)

// Walled garden rule kinds.
const (
	RuleSuffix = "suffix"
	RuleCIDR   = "cidr"
)

// Errors returned by the gate.
var (
	ErrBlankClient    = errors.New("client id is required")
	ErrUnknownClient  = errors.New("client is not known to the gate")
	ErrDeviceMismatch = errors.New("device fingerprint does not match the client")
	ErrBadTTL         = errors.New("lease ttl must be positive")
	ErrBadRule        = errors.New("walled garden rule is invalid")
	ErrLeaseNotFound  = errors.New("lease not found")
	ErrLeaseNotActive = errors.New("lease is not active")
	ErrLeaseExpired   = errors.New("lease already expired")
	ErrClientBlocked  = errors.New("client is blocked")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
	ErrLeaseLimit     = errors.New("lease table is full")
	ErrRuleLimit      = errors.New("walled garden rule table is full")
)

// Limits bounds the gate tables and lease lifetime.
type Limits struct {
	LeaseTTL  time.Duration
	MaxTTL    time.Duration
	MaxRules  int
	MaxLeases int
}

// Rule is one walled garden entry: either a domain suffix or an address block.
type Rule struct {
	Kind  string
	Value string
}

// Client is one guest device tracked by the gate.
type Client struct {
	ID          string
	Fingerprint string
	Addr        string
	State       string
	LeaseID     string
	Attempts    int
	FirstSeen   time.Time
}

// Request is one intercepted name resolution or connection attempt.
type Request struct {
	Host  string
	Addr  string
	Path  string
	Phase string
	Now   time.Time
}

// Verdict is the decision returned for one intercepted request.
type Verdict struct {
	ClientID string
	Action   string
	Reason   string
	LeaseID  string
	Host     string
	Addr     string
	Phase    string
}

// Grant is one authorisation request.
type Grant struct {
	Fingerprint string
	TTL         time.Duration
	Now         time.Time
}

// Lease is one authorisation lease bound to a device fingerprint.
type Lease struct {
	ID          string
	ClientID    string
	Fingerprint string
	Issued      time.Time
	Expires     time.Time
	State       string
}

// CaptivePortalGate owns the client table, the lease table and the walled
// garden rules.
type CaptivePortalGate struct {
	limits  Limits
	rules   []Rule
	clients map[string]*Client
	leases  map[string]*Lease
	seq     int64
}

// NewCaptivePortalGate builds a gate with the given tables and rules.
func NewCaptivePortalGate(limits Limits, rules []Rule) *CaptivePortalGate {
	return &CaptivePortalGate{limits: limits, rules: append([]Rule(nil), rules...), clients: map[string]*Client{}, leases: map[string]*Lease{}}
}

// Intercept decides what to do with one request from one client.
func (g *CaptivePortalGate) Intercept(client Client, req Request) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Authorize issues a lease bound to the client device fingerprint.
func (g *CaptivePortalGate) Authorize(clientID string, grant Grant) (Lease, error) {
	return Lease{}, errNotImplemented
}

// WalledGarden returns a copy of the configured rules in configuration order.
func (g *CaptivePortalGate) WalledGarden() []Rule {
	return nil
}

// Revoke kills one lease and blocks the client behind it.
func (g *CaptivePortalGate) Revoke(leaseID string) error {
	return errNotImplemented
}

// Expire closes every lease whose deadline has passed and reports how many.
func (g *CaptivePortalGate) Expire(now time.Time) int {
	return 0
}

// Validate re-checks a lease for an already established connection.
func (g *CaptivePortalGate) Validate(leaseID string, now time.Time) error {
	return errNotImplemented
}

// ClientOf reads one client record.
func (g *CaptivePortalGate) ClientOf(clientID string) (Client, bool) {
	return Client{}, false
}

// LeaseOf reads one lease record.
func (g *CaptivePortalGate) LeaseOf(leaseID string) (Lease, bool) {
	return Lease{}, false
}

// Leases returns every lease record, ordered by lease id.
func (g *CaptivePortalGate) Leases() []Lease {
	return nil
}

// Snapshot serialises clients, leases, rules and the sequence counter.
func (g *CaptivePortalGate) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *CaptivePortalGate) Restore(data []byte) error {
	return errNotImplemented
}
