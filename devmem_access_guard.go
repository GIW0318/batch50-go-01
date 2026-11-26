// Package task authorises raw memory and IO device opens by address range,
// records every grant and open in an audit trail, and reclaims descriptors
// when an owner is revoked.
package task

import (
	"errors"
	"sync"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Device kinds.
const (
	KindMem  = "mem"
	KindPort = "port"
	KindMSR  = "msr"
)

// Access modes used by grants and files.
const (
	ReadOnly  = 0
	WriteOnly = 1
	ReadWrite = 2
)

// Capabilities a caller has to hold for a device kind.
const (
	CapRawIO  = "cap_rawio"
	CapPortIO = "cap_portio"
	CapMSR    = "cap_msr"
)

// Audit phases.
const (
	PhaseGranted   = "granted"
	PhaseOpened    = "opened"
	PhaseDenied    = "denied"
	PhaseRevoked   = "revoked"
	PhaseReclaimed = "reclaimed"
)

// File states.
const (
	FileOpen        = "open"
	FileQuarantined = "quarantined"
	FileReclaimed   = "reclaimed"
	FileClosed      = "closed"
)

// Errors returned by the guard.
var (
	ErrBadRequest    = errors.New("request is not valid")
	ErrUnknownDevice = errors.New("device is not registered")
	ErrUnknownGrant  = errors.New("grant is not known")
	ErrUnknownOwner  = errors.New("owner holds no grant or file")
	ErrUnknownFile   = errors.New("file is not known")
	ErrNoCapability  = errors.New("caller lacks the capability for this device")
	ErrNoTicket      = errors.New("work order ticket is required")
	ErrWindowClosed  = errors.New("maintenance window is closed")
	ErrOutOfRange    = errors.New("requested range is outside the device")
	ErrRangeTooLong  = errors.New("requested range exceeds the length cap")
	ErrGrantLimit    = errors.New("grant limit reached for this owner")
	ErrAuditFull     = errors.New("audit buffer is full")
	ErrGrantExpired  = errors.New("grant has expired")
	ErrGrantConsumed = errors.New("grant was already opened")
	ErrGrantRevoked  = errors.New("grant was revoked")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// GuardLimits bounds the live tables of the guard.
type GuardLimits struct {
	MaxGrantsPerOwner int
	MaxRangeLen       int64
	MaxAudit          int
}

// Caller is the identity asking for a device range.
type Caller struct {
	Owner  string   `json:"owner"`
	Group  string   `json:"group"`
	Ticket string   `json:"ticket"`
	Caps   []string `json:"caps"`
}

// Device is one raw memory or IO node with its strict address range. The
// registry only keeps Node, Kind, Base, Size and HostInode; a request reuses
// the same record and fills Offset and Length with the wanted slice, where
// Offset is relative to Base.
type Device struct {
	Node      string `json:"node"`
	Kind      string `json:"kind"`
	Base      int64  `json:"base"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	HostInode uint64 `json:"host_inode"`
}

// Grant is one authorised range with its expiry and generation.
type Grant struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Group      string `json:"group"`
	Ticket     string `json:"ticket"`
	Device     string `json:"device"`
	Offset     int64  `json:"offset"`
	Length     int64  `json:"length"`
	RW         int    `json:"rw"`
	ExpiresAt  int64  `json:"expires_at"`
	Generation int64  `json:"generation"`
	Consumed   bool   `json:"consumed"`
}

// File is one open descriptor handed to a caller.
type File struct {
	FD         int    `json:"fd"`
	GrantID    string `json:"grant_id"`
	Owner      string `json:"owner"`
	Device     string `json:"device"`
	Offset     int64  `json:"offset"`
	Length     int64  `json:"length"`
	RW         int    `json:"rw"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

// Access is one audit record.
type Access struct {
	Seq    int64  `json:"seq"`
	Phase  string `json:"phase"`
	Owner  string `json:"owner"`
	Group  string `json:"group"`
	Device string `json:"device"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	RW     int    `json:"rw"`
	Ticket string `json:"ticket"`
	Reason string `json:"reason"`
}

// DevmemAccessGuard owns devices, grants, files and the audit buffer.
type DevmemAccessGuard struct {
	limits GuardLimits
	mu     sync.Mutex
}

// NewDevmemAccessGuard builds a guard with the given caps.
func NewDevmemAccessGuard(limits GuardLimits) *DevmemAccessGuard {
	return &DevmemAccessGuard{limits: limits}
}

// AddDevice registers one device node and its strict range.
func (g *DevmemAccessGuard) AddDevice(dev Device) error {
	return errNotImplemented
}

// Devices lists the registered devices in ascending node order.
func (g *DevmemAccessGuard) Devices() []Device {
	return nil
}

// SetNow moves the logical clock used for maintenance windows.
func (g *DevmemAccessGuard) SetNow(tick int64) error {
	return errNotImplemented
}

// SetWindow opens a maintenance window for one owner.
func (g *DevmemAccessGuard) SetWindow(owner string, open, close int64) error {
	return errNotImplemented
}

// Authorize checks capability, window and range and returns a grant.
func (g *DevmemAccessGuard) Authorize(caller Caller, dev Device, rw int) (Grant, error) {
	return Grant{}, errNotImplemented
}

// Open turns a grant into one open descriptor.
func (g *DevmemAccessGuard) Open(grantID string) (File, error) {
	return File{}, errNotImplemented
}

// Close releases one open descriptor.
func (g *DevmemAccessGuard) Close(fd int) error {
	return errNotImplemented
}

// Revoke invalidates every grant of an owner and quarantines its files.
func (g *DevmemAccessGuard) Revoke(owner string) error {
	return errNotImplemented
}

// Reclaim removes one quarantined descriptor.
func (g *DevmemAccessGuard) Reclaim(fd int) error {
	return errNotImplemented
}

// Audit returns a copy of the audit buffer, oldest first.
func (g *DevmemAccessGuard) Audit() []Access {
	return nil
}

// Grants returns the grants of one owner, oldest first.
func (g *DevmemAccessGuard) Grants(owner string) []Grant {
	return nil
}

// Files returns the descriptors of one owner in ascending fd order.
func (g *DevmemAccessGuard) Files(owner string) []File {
	return nil
}

// Quarantined returns the descriptors waiting for reclaim, ascending by fd.
func (g *DevmemAccessGuard) Quarantined() []File {
	return nil
}

// Snapshot serialises devices, grants, files and the audit buffer.
func (g *DevmemAccessGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *DevmemAccessGuard) Restore(data []byte) error {
	return errNotImplemented
}
