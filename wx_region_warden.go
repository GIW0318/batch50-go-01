// Package task inspects the memory mappings of managed processes, grades the
// ones that are writable and executable at the same time, and hands out JIT
// exemption leases for the mappings that are expected to behave that way.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Disposition actions reported for a graded region.
const (
	ActionObserve  = "observe"
	ActionAlert    = "alert"
	ActionRestrict = "restrict"
)

// Disposition levels. The stored level only ever moves towards the stricter end.
const (
	LevelObserve  = 0
	LevelAlert    = 1
	LevelRestrict = 2
)

// Explainable reason codes.
const (
	ReasonAnonWX          = "reason_anon_wx"
	ReasonJitLeaseActive  = "reason_jit_lease_active"
	ReasonJitDenied       = "reason_jit_denied"
	ReasonLeaseExpired    = "reason_lease_expired"
	ReasonLeaseGrace      = "reason_lease_grace"
	ReasonRestrictDenied  = "reason_restrict_denied"
	ReasonRestrictApplied = "reason_restrict_applied"
	ReasonRegionNotOwned  = "reason_region_not_owned"
)

// Lease states.
const (
	LeaseActive  = "active"
	LeaseGrace   = "grace"
	LeaseExpired = "expired"
	LeaseRevoked = "revoked"
)

// Errors returned by the warden.
var (
	ErrBlankProfile       = errors.New("tenant and profile name are required")
	ErrProcessExists      = errors.New("process already registered")
	ErrTenantProcessLimit = errors.New("tenant process limit reached")
	ErrUnknownHandle      = errors.New("unknown process handle")
	ErrBadMapping         = errors.New("mapping range is invalid")
	ErrJitNotAllowed      = errors.New("profile does not allow jit mappings")
	ErrLeaseLimit         = errors.New("per process lease limit reached")
	ErrLeaseTooLarge      = errors.New("lease size exceeds the per lease cap")
	ErrUnknownLease       = errors.New("unknown lease")
	ErrLeaseReleased      = errors.New("lease already released")
	ErrUnknownRegion      = errors.New("unknown region")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// Mapping is one line of the mapping table fed into the warden.
type Mapping struct {
	Start      uint64
	End        uint64
	Writable   bool
	Executable bool
	Anonymous  bool
	Backing    string
	Path       string
}

// Profile describes what a managed process is allowed to do.
type Profile struct {
	Tenant   string
	Name     string
	AllowJit bool
}

// Handle identifies one registered process.
type Handle struct {
	PID    int
	Tenant string
	Seq    uint64
}

// Limits bounds the live tables of the warden.
type Limits struct {
	MaxProcessesPerTenant int
	MaxLeasesPerProcess   int
	MaxLeaseBytes         uint64
	MaxRegionsPerProcess  int
	LeaseGrace            time.Duration
	Now                   func() time.Time
}

// Region is one graded mapping region.
type Region struct {
	ID         string
	PID        int
	Start      uint64
	End        uint64
	Writable   bool
	Executable bool
	Anonymous  bool
	Backing    string
	Jit        bool
	Level      int
	Reason     string
}

// Verdict is the graded outcome for a single region.
type Verdict struct {
	RegionID string
	Action   string
	Level    int
	Reason   string
}

// Report is the result of one scan.
type Report struct {
	PID        int
	Regions    []Region
	Verdicts   []Verdict
	Observed   int
	Alerts     int
	Restricted int
}

// Lease is a JIT exemption handed to one process.
type Lease struct {
	ID        string
	PID       int
	Size      uint64
	Backing   string
	ExpiresAt time.Time
	State     string
}

// Summary aggregates the live state of one tenant.
type Summary struct {
	Tenant     string
	Processes  int
	Leases     int
	Restricted int
}

// WxRegionWarden owns the process table, the lease table and the region table.
type WxRegionWarden struct {
	limits Limits
}

// NewWxRegionWarden builds a warden with the given caps.
func NewWxRegionWarden(limits Limits) *WxRegionWarden {
	return &WxRegionWarden{limits: limits}
}

// Register adds one managed process.
func (w *WxRegionWarden) Register(pid int, profile Profile) (Handle, error) {
	return Handle{}, errNotImplemented
}

// Unregister removes one managed process and its leases.
func (w *WxRegionWarden) Unregister(h Handle) error {
	return errNotImplemented
}

// Feed replaces the mapping table of one process.
func (w *WxRegionWarden) Feed(h Handle, maps []Mapping) (int, error) {
	return 0, errNotImplemented
}

// Scan grades the current mapping table of one process and stores the result.
func (w *WxRegionWarden) Scan(h Handle) (Report, error) {
	return Report{}, errNotImplemented
}

// Classify grades a single mapping outside of any process context.
func (w *WxRegionWarden) Classify(m Mapping) (Region, error) {
	return Region{}, errNotImplemented
}

// GrantJitLease opens an exemption of the given size for the given duration.
func (w *WxRegionWarden) GrantJitLease(h Handle, size uint64, ttl time.Duration) (Lease, error) {
	return Lease{}, errNotImplemented
}

// ReleaseLease closes one lease.
func (w *WxRegionWarden) ReleaseLease(leaseID string) error {
	return errNotImplemented
}

// LeaseState reads one lease and refreshes its state against the clock.
func (w *WxRegionWarden) LeaseState(leaseID string) (Lease, error) {
	return Lease{}, errNotImplemented
}

// Disposition reads the stored action and reason of one region.
func (w *WxRegionWarden) Disposition(h Handle, regionID string) (Action string, reason string, err error) {
	return "", "", errNotImplemented
}

// Restrict lowers the permissions of one region, or explains why it refused.
func (w *WxRegionWarden) Restrict(h Handle, regionID string) (Action string, reason string, err error) {
	return "", "", errNotImplemented
}

// Regions returns the graded regions of one process.
func (w *WxRegionWarden) Regions(h Handle) []Region {
	return nil
}

// Summary aggregates the live state of one tenant.
func (w *WxRegionWarden) Summary(tenant string) Summary {
	return Summary{}
}

// Snapshot serialises processes, leases and graded regions.
func (w *WxRegionWarden) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (w *WxRegionWarden) Restore(data []byte) error {
	return errNotImplemented
}
