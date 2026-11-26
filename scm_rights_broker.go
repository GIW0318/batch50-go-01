// Package task brokers file descriptors received over SCM_RIGHTS on unix
// domain sockets, attributes them to tenants and enforces per tenant quotas.
package task

import (
	"errors"
	"net"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Descriptor kinds understood by the broker.
const (
	KindRegular   = "regular"
	KindDirectory = "dir"
	KindMemfd     = "memfd"
	KindPidfd     = "pidfd"
	KindSocket    = "socket"
)

// Audit events.
const (
	EventGranted         = "granted"
	EventReleased        = "released"
	EventDenied          = "denied"
	EventOrphanReclaimed = "orphan_reclaimed"
)

// Errors returned by the broker.
var (
	ErrBadConn       = errors.New("unix domain connection is required")
	ErrBlankPeer     = errors.New("peer id and tenant are required")
	ErrPeerExists    = errors.New("peer already registered")
	ErrUnknownPeer   = errors.New("unknown peer")
	ErrNoDescriptors = errors.New("no staged descriptors")
	ErrBadLimit      = errors.New("descriptor limit must be positive")
	ErrFDTypeDenied  = errors.New("descriptor type is not allowed")
	ErrQuotaExceeded = errors.New("tenant descriptor quota exceeded")
	ErrUnknownGrant  = errors.New("unknown descriptor grant")
	ErrBadQuota      = errors.New("quota values must not be negative")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Limits bounds the audit log.
type Limits struct {
	MaxRecords int
	Now        func() time.Time
}

// Quota is the per tenant descriptor budget.
type Quota struct {
	MaxInFlight int
	MaxPerKind  int
	Allowed     []string
}

// Descriptor is one staged file descriptor waiting for admission.
type Descriptor struct {
	Kind      string
	Target    string
	Size      int64
	ObjectKey string
	ReadOnly  bool
}

// Peer is one authenticated unix socket peer.
type Peer struct {
	ID     string
	Tenant string
	UID    uint32
	PID    int
	Seq    uint64
}

// FDGrant is one admitted descriptor.
type FDGrant struct {
	ID        string
	PeerID    string
	Tenant    string
	Kind      string
	Target    string
	Size      int64
	ObjectKey string
	Seq       int64
}

// GrantRecord is one line of the audit log.
type GrantRecord struct {
	GrantID   string
	Tenant    string
	Kind      string
	ObjectKey string
	Event     string
	Reason    string
	Seq       int64
}

// BrokerStats aggregates the live counters of one tenant.
type BrokerStats struct {
	Tenant   string
	InFlight int
	Granted  int
	Released int
}

// ScmRightsBroker owns the peer table, the quota table and the audit log.
type ScmRightsBroker struct {
	limits Limits
}

// NewScmRightsBroker builds a broker with the given log bound.
func NewScmRightsBroker(limits Limits) *ScmRightsBroker {
	return &ScmRightsBroker{limits: limits}
}

// AcceptConn authenticates one accepted unix connection through peer credentials.
func (b *ScmRightsBroker) AcceptConn(conn *net.UnixConn) (Peer, error) {
	return Peer{}, errNotImplemented
}

// RegisterPeer records one peer that has already been authenticated.
func (b *ScmRightsBroker) RegisterPeer(peerID, tenant string, uid uint32) (Peer, error) {
	return Peer{}, errNotImplemented
}

// SetQuota installs the descriptor budget of one tenant.
func (b *ScmRightsBroker) SetQuota(tenant string, q Quota) error {
	return errNotImplemented
}

// Stage queues descriptors as if they had arrived with a single message.
func (b *ScmRightsBroker) Stage(peer Peer, in []Descriptor) error {
	return errNotImplemented
}

// Receive admits the staged descriptors of one peer in arrival order.
func (b *ScmRightsBroker) Receive(peer Peer, maxFDs int) ([]FDGrant, error) {
	return nil, errNotImplemented
}

// Release closes one admitted descriptor grant.
func (b *ScmRightsBroker) Release(grantID string) error {
	return errNotImplemented
}

// ReleasePeer closes every grant of one peer after it exits.
func (b *ScmRightsBroker) ReleasePeer(peerID string) (int, error) {
	return 0, errNotImplemented
}

// Sweep reclaims grants of peers that went away before releasing them.
func (b *ScmRightsBroker) Sweep() (int, error) {
	return 0, errNotImplemented
}

// Audit returns the audit log, oldest first.
func (b *ScmRightsBroker) Audit() []GrantRecord {
	return nil
}

// Stats reads the live counters of one tenant.
func (b *ScmRightsBroker) Stats(tenant string) BrokerStats {
	return BrokerStats{}
}

// Snapshot serialises peers, quotas, grants and the audit log.
func (b *ScmRightsBroker) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (b *ScmRightsBroker) Restore(data []byte) error {
	return errNotImplemented
}
