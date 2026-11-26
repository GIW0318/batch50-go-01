// Package task hands out compute capacity for security domains while keeping
// the two hyperthread siblings of one physical core inside the same domain.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Reasons attached to a degraded allocation or to a domain usage record.
const (
	ReasonWholeCore          = "reason_whole_core"
	ReasonSiblingBlocked     = "reason_sibling_blocked"
	ReasonCapacityExhausted  = "reason_capacity_exhausted"
	ReasonLoadBelowThreshold = "reason_load_below_threshold"
)

// Errors returned by the isolator.
var (
	ErrBadTopology  = errors.New("cpu topology is invalid")
	ErrBadDomain    = errors.New("domain name is required")
	ErrBadRequest   = errors.New("reservation request is invalid")
	ErrCapacity     = errors.New("not enough isolated capacity")
	ErrBusy         = errors.New("topology cannot change while allocations exist")
	ErrUnknownAlloc = errors.New("allocation not found")
	ErrBadSnapshot  = errors.New("snapshot payload is invalid")
)

// CPU is one logical processor as reported by the host.
type CPU struct {
	ID       int  `json:"id"`
	Core     int  `json:"core"`
	Thread   int  `json:"thread"`
	Online   bool `json:"online"`
	Reserved bool `json:"reserved"`
}

// Core groups the logical processors that share one physical core.
type Core struct {
	ID     int   `json:"id"`
	CPUs   []int `json:"cpus"`
	Online bool  `json:"online"`
}

// Resource is one reservation request.
type Resource struct {
	Threads   int  `json:"threads"`
	Exclusive bool `json:"exclusive"`
}

// Allocation is the capacity handed to one domain.
type Allocation struct {
	ID       string `json:"id"`
	Domain   string `json:"domain"`
	CPUs     []int  `json:"cpus"`
	Cores    []int  `json:"cores"`
	Threads  int    `json:"threads"`
	Degraded bool   `json:"degraded"`
	Reason   string `json:"reason"`
	Gen      int64  `json:"gen"`
}

// Usage is the aggregated footprint of one domain.
type Usage struct {
	Domain      string `json:"domain"`
	CPUs        []int  `json:"cpus"`
	Cores       []int  `json:"cores"`
	Threads     int    `json:"threads"`
	Allocations int    `json:"allocations"`
	Load        int    `json:"load"`
	LastReason  string `json:"last_reason"`
}

// Limits carries the yielding policy knobs.
type Limits struct {
	YieldReleaseLoad int
}

// SmtSiblingIsolator owns the topology, the core ownership table and the
// monotonic counters.
type SmtSiblingIsolator struct {
	limits Limits
}

// NewSmtSiblingIsolator builds an isolator with the given policy.
func NewSmtSiblingIsolator(limits Limits) *SmtSiblingIsolator {
	return &SmtSiblingIsolator{limits: limits}
}

// SetTopology installs the processor topology and rebuilds the core groups.
func (s *SmtSiblingIsolator) SetTopology(cpus []CPU) error {
	return errNotImplemented
}

// Topology returns a read only copy of the core groups, ordered by core id.
func (s *SmtSiblingIsolator) Topology() []Core {
	return nil
}

// Reserve hands out threads to one domain without mixing domains on a core.
func (s *SmtSiblingIsolator) Reserve(domain string, req Resource) (Allocation, error) {
	return Allocation{}, errNotImplemented
}

// Release returns one allocation and restores the affinity of its cores.
func (s *SmtSiblingIsolator) Release(allocID string) error {
	return errNotImplemented
}

// Rebalance reclaims idle domain state and re-evaluates yielding.
func (s *SmtSiblingIsolator) Rebalance() error {
	return errNotImplemented
}

// Allocation reads one allocation record.
func (s *SmtSiblingIsolator) Allocation(allocID string) (Allocation, bool) {
	return Allocation{}, false
}

// Allocations lists every live allocation ordered by allocation id.
func (s *SmtSiblingIsolator) Allocations() []Allocation {
	return nil
}

// DomainUsage reports the aggregated footprint of one domain.
func (s *SmtSiblingIsolator) DomainUsage(domain string) Usage {
	return Usage{}
}

// SetLoad records the current load of one domain.
func (s *SmtSiblingIsolator) SetLoad(domain string, load int) error {
	return errNotImplemented
}

// Domains lists the domains that still hold capacity, ordered by name.
func (s *SmtSiblingIsolator) Domains() []string {
	return nil
}

// Snapshot serialises the topology, the allocations and the counters.
func (s *SmtSiblingIsolator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole isolator state with a snapshot, all or nothing.
func (s *SmtSiblingIsolator) Restore(data []byte) error {
	return errNotImplemented
}
