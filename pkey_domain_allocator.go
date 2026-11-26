// Package task hands out memory protection keys to isolation domains, keeps the
// key to domain mapping, the per key access rights and the thread pins that a
// goroutine needs before it may change those rights.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Pkey is a protection key slot. Slot 0 is reserved and never handed out.
type Pkey int

// Rights is the access mask stored for one protection key.
type Rights uint8

// Access bits. Write without read is never a valid mask.
const (
	RightsRead  Rights = 1
	RightsWrite Rights = 2
	RightsFull  Rights = RightsRead | RightsWrite
)

// PkeyMode says how many memory regions a domain may bind.
type PkeyMode string

const (
	// ModeExclusive domains bind exactly one region.
	ModeExclusive PkeyMode = "exclusive"
	// ModeShared domains bind up to Config.MaxRegionsPerDomain regions.
	ModeShared PkeyMode = "shared"
)

// Region is a half open address interval [Start, Start+Len).
type Region struct {
	Start uint64
	Len   uint64
}

// Config bounds the key slots, the regions per domain and the thread pins per
// domain. Non positive values fall back to the documented defaults.
type Config struct {
	MaxKeys             int
	MaxRegionsPerDomain int
	MaxPinsPerDomain    int
}

// Domain is the isolation domain record handed back to callers.
type Domain struct {
	ID      string
	Name    string
	Mode    PkeyMode
	Keys    []Pkey
	Regions []Region
	Seq     int64
}

// PkeyState is the per key bookkeeping visible to callers.
type PkeyState struct {
	Pkey     Pkey
	DomainID string
	Rights   Rights
	Version  int64
}

// Pin records that one goroutine locked its OS thread and took the key of a
// domain for the duration of a rights switch.
type Pin struct {
	ID       string
	DomainID string
	Pkey     Pkey
}

// Errors returned by the allocator.
var (
	ErrInvalidDomain   = errors.New("domain name and mode are required")
	ErrDomainExists    = errors.New("domain name already registered")
	ErrUnknownDomain   = errors.New("unknown domain")
	ErrPkeyExhausted   = errors.New("no protection key left")
	ErrPkeyUnknown     = errors.New("unknown protection key")
	ErrBadRights       = errors.New("invalid rights mask")
	ErrBadRegion       = errors.New("invalid memory region")
	ErrRegionOverlap   = errors.New("region overlaps another region of the same domain")
	ErrDomainViolation = errors.New("region is owned by another domain")
	ErrRegionLimit     = errors.New("region limit reached for the domain")
	ErrPinLimit        = errors.New("thread pin limit reached for the domain")
	ErrPinsActive      = errors.New("thread pins still reference the domain")
	ErrUnknownPin      = errors.New("unknown thread pin")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// PkeyDomainAllocator owns the domain table, the key slots and the pin table.
type PkeyDomainAllocator struct {
	cfg Config
}

// NewPkeyDomainAllocator builds an allocator with the given caps.
func NewPkeyDomainAllocator(cfg Config) *PkeyDomainAllocator {
	return &PkeyDomainAllocator{cfg: cfg}
}

// NewDomain registers one isolation domain and returns its record.
func (a *PkeyDomainAllocator) NewDomain(name string, mode PkeyMode) (Domain, error) {
	return Domain{}, errNotImplemented
}

// AllocKey hands the lowest free key slot to the domain.
func (a *PkeyDomainAllocator) AllocKey(domainID string) (Pkey, error) {
	return 0, errNotImplemented
}

// SetRights replaces the access mask of one key and bumps its version.
func (a *PkeyDomainAllocator) SetRights(pkey Pkey, rights Rights) error {
	return errNotImplemented
}

// Bind attaches one memory region to a domain.
func (a *PkeyDomainAllocator) Bind(domainID string, r Region) error {
	return errNotImplemented
}

// Check reports whether a region is fully owned by the given domain.
func (a *PkeyDomainAllocator) Check(domainID string, r Region) error {
	return errNotImplemented
}

// PinThread records a thread pin for the domain on the calling goroutine.
func (a *PkeyDomainAllocator) PinThread(domainID string) (Pin, error) {
	return Pin{}, errNotImplemented
}

// UnpinThread releases one thread pin and resets the thread local key state.
func (a *PkeyDomainAllocator) UnpinThread(pin Pin) error {
	return errNotImplemented
}

// ReleaseDomain drops a domain, frees its keys and forgets its regions.
func (a *PkeyDomainAllocator) ReleaseDomain(domainID string) error {
	return errNotImplemented
}

// Domain reads one domain record.
func (a *PkeyDomainAllocator) Domain(domainID string) (Domain, bool) {
	return Domain{}, false
}

// PkeyState reads the bookkeeping of one key slot.
func (a *PkeyDomainAllocator) PkeyState(pkey Pkey) (PkeyState, bool) {
	return PkeyState{}, false
}

// Domains lists the live domains in creation order.
func (a *PkeyDomainAllocator) Domains() []Domain {
	return nil
}

// Snapshot serialises the domains, the key slots and the thread pins.
func (a *PkeyDomainAllocator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *PkeyDomainAllocator) Restore(data []byte) error {
	return errNotImplemented
}
