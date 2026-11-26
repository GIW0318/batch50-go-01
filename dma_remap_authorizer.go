// Package task authorises DMA remapping for passthrough devices, keeping whole
// IOMMU groups together and bounding the temporary authorisation windows.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Errors returned by the authoriser.
var (
	ErrBlankGroup        = errors.New("iommu group is required")
	ErrNoFunctions       = errors.New("device must expose at least one function")
	ErrDuplicateFunction = errors.New("device function is declared twice")
	ErrDeviceExists      = errors.New("device already registered")
	ErrUnknownDevice     = errors.New("unknown device")
	ErrDeviceDetached    = errors.New("device is detached")
	ErrGroupIncomplete   = errors.New("iommu group contains an excluded function")
	ErrGroupBound        = errors.New("iommu group is already bound")
	ErrGroupConflict     = errors.New("device in the group is bound to another domain")
	ErrGroupRevoked      = errors.New("iommu group is revoked")
	ErrDomainLimit       = errors.New("domain device limit reached")
	ErrUnknownGroup      = errors.New("unknown iommu group")
	ErrUnknownGrant      = errors.New("unknown dma grant")
	ErrBadWindow         = errors.New("authorisation window must be positive")
	ErrWindowTooLong     = errors.New("authorisation window exceeds the per window cap")
	ErrWindowBudget      = errors.New("authorisation window budget exhausted")
	ErrBadRevokeReason   = errors.New("revoke reason is required")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// DeviceID identifies one device by vendor, product and topology path.
type DeviceID struct {
	Vendor string
	Device string
	Path   string
}

// Function is one function exposed by a device.
type Function struct {
	Name     string
	Excluded bool
}

// Limits bounds the live tables of the authoriser.
type Limits struct {
	MaxDevicesPerDomain int
	MaxWindow           time.Duration
	MaxWindowTotal      time.Duration
	MaxGroups           int
	RestoreGrace        time.Duration
	Now                 func() time.Time
}

// Grant is one whole group bound to a domain.
type Grant struct {
	ID      string
	Domain  string
	Group   string
	Devices []DeviceID
	Seq     uint64
}

// Lease is one temporary authorisation window.
type Lease struct {
	ID        string
	GrantID   string
	Domain    string
	Window    time.Duration
	ExpiresAt time.Time
	Seq       int64
}

// DeviceView is the per device projection of a group.
type DeviceView struct {
	ID        DeviceID
	Functions int
	Excluded  int
	Bound     bool
	Detached  bool
}

// GroupView is the state of one IOMMU group.
type GroupView struct {
	Group        string
	Domain       string
	Bound        bool
	Revoked      bool
	Reason       string
	Devices      []DeviceView
	ActiveLeases int
}

// Usage aggregates the live counters of one domain.
type Usage struct {
	Domain      string
	Devices     int
	Leases      int
	WindowTotal time.Duration
}

// DmaRemapAuthorizer owns the device table, the group table and the leases.
type DmaRemapAuthorizer struct {
	limits Limits
}

// NewDmaRemapAuthorizer builds an authoriser with the given caps.
func NewDmaRemapAuthorizer(limits Limits) *DmaRemapAuthorizer {
	return &DmaRemapAuthorizer{limits: limits}
}

// RegisterDevice records one device and the IOMMU group it belongs to.
func (a *DmaRemapAuthorizer) RegisterDevice(dev DeviceID, group string, functions []Function) error {
	return errNotImplemented
}

// Detach marks one device as physically removed.
func (a *DmaRemapAuthorizer) Detach(dev DeviceID) error {
	return errNotImplemented
}

// Attach brings a detached device back and re-reads its group.
func (a *DmaRemapAuthorizer) Attach(dev DeviceID, group string) error {
	return errNotImplemented
}

// Enroll binds the whole IOMMU group of the device to one domain.
func (a *DmaRemapAuthorizer) Enroll(dev DeviceID, domain string) (Grant, error) {
	return Grant{}, errNotImplemented
}

// Authorize opens one temporary window for a bound grant.
func (a *DmaRemapAuthorizer) Authorize(grantID string, window time.Duration) (Lease, error) {
	return Lease{}, errNotImplemented
}

// Revoke unbinds the whole group of the device and records the reason.
func (a *DmaRemapAuthorizer) Revoke(dev DeviceID, reason string) error {
	return errNotImplemented
}

// GroupStatus reads the state of one IOMMU group.
func (a *DmaRemapAuthorizer) GroupStatus(group string) (GroupView, error) {
	return GroupView{}, errNotImplemented
}

// DomainUsage reads the live counters of one domain.
func (a *DmaRemapAuthorizer) DomainUsage(domain string) Usage {
	return Usage{}
}

// ActiveLeases lists the live windows of one domain, oldest first.
func (a *DmaRemapAuthorizer) ActiveLeases(domain string) []Lease {
	return nil
}

// Snapshot serialises devices, bindings and windows.
func (a *DmaRemapAuthorizer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *DmaRemapAuthorizer) Restore(data []byte) error {
	return errNotImplemented
}
