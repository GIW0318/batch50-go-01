// Package task enforces SSH server side port forwarding policy and leases
// reverse forwarding ports to live sessions.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Decision reason codes.
const (
	ReasonAllow            = "forward_allow"
	ReasonDeniedNotListed  = "forward_denied_not_listed"
	ReasonDeniedPort       = "forward_denied_port"
	ReasonDeniedCIDR       = "forward_denied_cidr"
	ReasonDeniedLoopback   = "forward_denied_loopback"
	ReasonDeniedMetadata   = "forward_denied_metadata"
	ReasonDeniedChannelCap = "forward_denied_channel_cap"
	ReasonDeniedByteCap    = "forward_denied_byte_cap"
)

// Session lifecycle states.
const (
	SessionActive = "active"
	SessionClosed = "closed"
)

// Errors surfaced by the policy.
var (
	ErrBlankSession       = errors.New("session id and tenant are required")
	ErrSessionExists      = errors.New("session already registered")
	ErrSessionNotFound    = errors.New("session not found")
	ErrTenantSessionLimit = errors.New("tenant session limit reached")
	ErrPortRange          = errors.New("requested port outside the configured range")
	ErrPortBusy           = errors.New("requested port already leased")
	ErrByteQuota          = errors.New("session byte quota exceeded")
	ErrBadHost            = errors.New("host cannot be normalized")
	ErrBadSnapshot        = errors.New("snapshot payload invalid")
)

// HostPort is one direct-tcpip destination as requested by a client.
type HostPort struct {
	Host string
	Port int
}

// Decision is the answer for one direct-tcpip request.
type Decision struct {
	Allow  bool
	Reason string
	Target string
}

// RemoteForward is one -R style request from a session.
type RemoteForward struct {
	WantPort int
	Tenant   string
}

// BoundPort is one granted reverse forwarding lease.
type BoundPort struct {
	Port      int
	LeaseID   string
	SessionID string
	ExpiresAt time.Time
}

// Session is the live record of one SSH session.
type Session struct {
	ID         string
	Tenant     string
	Priority   int
	State      string
	Channels   int
	RemotePort int
	Bytes      int64
}

// TenantRule is the allow list and the denied networks of one tenant.
type TenantRule struct {
	Hosts         []string
	Ports         []int
	ForbiddenNets []string
}

// PolicyConfig bounds the live tables of the policy.
type PolicyConfig struct {
	MaxSessionsPerTenant  int
	MaxChannelsPerSession int
	MaxBytesPerSession    int64
	PortRangeLow          int
	PortRangeHigh         int
	LeaseTTL              time.Duration
	LeaseQueueDepth       int
	Rules                 map[string]TenantRule
	Resolver              func(host string) []string
}

// SshForwardPolicy owns sessions, the direct-tcpip rules and the port pool.
type SshForwardPolicy struct {
	cfg PolicyConfig
}

// NewSshForwardPolicy builds a policy with the given caps.
func NewSshForwardPolicy(cfg PolicyConfig) *SshForwardPolicy {
	return &SshForwardPolicy{cfg: cfg}
}

// RegisterSession records one authenticated session.
func (p *SshForwardPolicy) RegisterSession(sessionID, tenant string, priority int) (Session, error) {
	return Session{}, errNotImplemented
}

// CheckDirect admits or denies one direct-tcpip channel.
func (p *SshForwardPolicy) CheckDirect(sessionID string, dest HostPort) (Decision, error) {
	return Decision{}, errNotImplemented
}

// AllocateRemote leases one reverse forwarding port to a session.
func (p *SshForwardPolicy) AllocateRemote(sessionID string, req RemoteForward) (BoundPort, error) {
	return BoundPort{}, errNotImplemented
}

// Release drops a session and every port lease it holds.
func (p *SshForwardPolicy) Release(sessionID string) error {
	return errNotImplemented
}

// SessionState reads one session record.
func (p *SshForwardPolicy) SessionState(sessionID string) (Session, error) {
	return Session{}, errNotImplemented
}

// NormalizeHost canonicalises an address literal or a domain name.
func (p *SshForwardPolicy) NormalizeHost(host string) (string, error) {
	return "", errNotImplemented
}

// ChargeBytes accounts forwarded payload against the session quota.
func (p *SshForwardPolicy) ChargeBytes(sessionID string, n int64) error {
	return errNotImplemented
}

// LeasePool returns a copy of every live port lease.
func (p *SshForwardPolicy) LeasePool() []BoundPort {
	return nil
}

// ReclaimExpired frees leases whose TTL elapsed and reports the count.
func (p *SshForwardPolicy) ReclaimExpired(now time.Time) int {
	return 0
}

// SessionCount reports how many live sessions a tenant holds.
func (p *SshForwardPolicy) SessionCount(tenant string) int {
	return 0
}

// Snapshot serialises sessions, leases and the lease counter.
func (p *SshForwardPolicy) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *SshForwardPolicy) Restore(data []byte) error {
	return errNotImplemented
}
