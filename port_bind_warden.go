// Package task approves and accounts host port bindings, keeps the reservation
// pool with its leases and manages the members of shared port groups.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Port classes reported by Explain.
const (
	ClassPrivileged = "privileged"
	ClassReserved   = "reserved"
	ClassDynamic    = "dynamic"
)

// Port range anchors. Ports up to PrivilegedPortMax need an allow listed owner,
// ports up to ReservedPortMax belong to the reserved pool.
const (
	PrivilegedPortMax = 1023
	ReservedPortMax   = 32767
	MaxPort           = 65535
)

// Lease is a reservation of one port for one owner.
type Lease struct {
	ID        string
	Port      int
	Owner     string
	ExpiresAt int64
	Seq       int64
}

// BindOpts carries the bind request details.
type BindOpts struct {
	Address    string
	Privileged bool
}

// Binding is one live bind of a leased port.
type Binding struct {
	ID      string
	Port    int
	Owner   string
	LeaseID string
	Group   string
	Address string
	Seq     int64
}

// PortState is the explainable view of one port.
type PortState struct {
	Port     int
	Class    string
	Lease    *Lease
	Bindings []Binding
	Group    string
}

// ReclaimReport lists what a checkpoint reclaimed and what it only graced.
type ReclaimReport struct {
	Reclaimed []string
	Graced    []string
}

// Config bounds the pool and injects the clock. Now may be nil, then the wall
// clock is used.
type Config struct {
	PrivilegedOwners []string
	ExternallyBusy   []int
	MaxLeases        int
	MaxPerOwner      int
	MaxGroupMembers  int
	Now              func() time.Time
}

// Errors returned by the warden.
var (
	ErrBadPort         = errors.New("port is outside the valid range")
	ErrBadOwner        = errors.New("owner is required")
	ErrBadTTL          = errors.New("lease ttl must be positive")
	ErrPortNotAllowed  = errors.New("owner may not reserve this port")
	ErrPortInUse       = errors.New("port is already reserved by another owner")
	ErrLeaseNotFound   = errors.New("unknown lease")
	ErrLeaseExpired    = errors.New("lease has expired")
	ErrLeaseLimit      = errors.New("lease pool limit reached")
	ErrOwnerLimit      = errors.New("per owner lease limit reached")
	ErrUnknownBinding  = errors.New("unknown binding")
	ErrBadBinding      = errors.New("bind request is invalid")
	ErrBadGroup        = errors.New("group name is required")
	ErrGroupLimit      = errors.New("group member limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// PortBindWarden owns the lease table, the binding table and the group table.
type PortBindWarden struct {
	cfg Config
}

// NewPortBindWarden builds a warden with the given caps.
func NewPortBindWarden(cfg Config) *PortBindWarden {
	return &PortBindWarden{cfg: cfg}
}

// Reserve takes a lease on one port for one owner.
func (w *PortBindWarden) Reserve(port int, owner string, ttl time.Duration) (Lease, error) {
	return Lease{}, errNotImplemented
}

// Bind turns one lease into a live binding.
func (w *PortBindWarden) Bind(leaseID string, opts BindOpts) (Binding, error) {
	return Binding{}, errNotImplemented
}

// JoinGroup adds one binding to a shared port group.
func (w *PortBindWarden) JoinGroup(group string, bindingID string) error {
	return errNotImplemented
}

// Release drops one binding and its group membership.
func (w *PortBindWarden) Release(bindingID string) error {
	return errNotImplemented
}

// Explain reports the state of one port.
func (w *PortBindWarden) Explain(port int) (PortState, error) {
	return PortState{}, errNotImplemented
}

// Checkpoint sweeps expired leases and reports what it handled.
func (w *PortBindWarden) Checkpoint() (ReclaimReport, error) {
	return ReclaimReport{}, errNotImplemented
}

// Lease reads one lease record.
func (w *PortBindWarden) Lease(leaseID string) (Lease, bool) {
	return Lease{}, false
}

// Binding reads one binding record.
func (w *PortBindWarden) Binding(bindingID string) (Binding, bool) {
	return Binding{}, false
}

// Snapshot serialises the leases, the bindings and the group table.
func (w *PortBindWarden) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (w *PortBindWarden) Restore(data []byte) error {
	return errNotImplemented
}
