// Package task hands cache masks and memory bandwidth budgets to tenants,
// moves tasks between resource groups, and keeps monitor identifiers reusable.
package task

import (
	"errors"
	"sort"
	"sync"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Group classes.
const (
	ClassDedicated = "dedicated"
	ClassShared    = "shared"
)

// Group states.
const (
	StateActive    = "active"
	StateReclaimed = "reclaimed"
)

// Errors returned by the guard.
var (
	ErrUnknownNode  = errors.New("node is not registered")
	ErrUnknownGroup = errors.New("resource group is not known")
	ErrBadTenant    = errors.New("tenant id is required")
	ErrBadSpec      = errors.New("resource specification is not valid")
	ErrMaskOverlap  = errors.New("cache mask overlaps another group")
	ErrMaskExhausted = errors.New("no contiguous cache mask is left")
	ErrGroupLimit   = errors.New("group limit reached for this node")
	ErrGroupNotEmpty = errors.New("group still holds tasks")
	ErrBadPID       = errors.New("pid is not valid")
	ErrNoMonitor    = errors.New("no monitor identifier is available")
	ErrBadSnapshot  = errors.New("snapshot payload is invalid")
)

// GuardLimits bounds one node.
type GuardLimits struct {
	MaxGroups     int
	MaxMonitorIDs int
	MaskBits      int
}

// Domain is one cache domain of a node.
type Domain struct {
	ID   int   `json:"id"`
	CPUs []int `json:"cpus"`
	Ways int   `json:"ways"`
}

// NodeInfo is the cache topology and bandwidth capacity of one node.
type NodeInfo struct {
	ID       string   `json:"id"`
	Domains  []Domain `json:"domains"`
	MBpsCap  int      `json:"mbps_cap"`
	FreeMask uint64   `json:"free_mask"`
	Offline  []int    `json:"offline"`
	Version  int      `json:"version"`
}

// ResctrlSpec asks for one resource group.
type ResctrlSpec struct {
	NodeID   string `json:"node_id"`
	Class    string `json:"class"`
	Mask     uint64 `json:"mask"`
	MaskBits int    `json:"mask_bits"`
	Weight   int    `json:"weight"`
}

// Group is one cache and bandwidth resource group.
type Group struct {
	ID         string   `json:"id"`
	NodeID     string   `json:"node_id"`
	TenantID   string   `json:"tenant_id"`
	Tenants    []string `json:"tenants"`
	Mask       uint64   `json:"mask"`
	MaskBits   int      `json:"mask_bits"`
	Weight     int      `json:"weight"`
	MBps       int      `json:"mbps"`
	Class      string   `json:"class"`
	MonitorID  int      `json:"monitor_id"`
	PIDs       []int    `json:"pids"`
	Generation int      `json:"generation"`
	State      string   `json:"state"`
}

// Counters is the occupancy reported for one group.
type Counters struct {
	LLC        uint64 `json:"llc"`
	MBps       uint64 `json:"mbps"`
	MBpsLimit  int    `json:"mbps_limit"`
	MonitorID  int    `json:"monitor_id"`
	Generation int    `json:"generation"`
}

// ResctrlTenantGuard owns nodes, resource groups and the monitor pool.
type ResctrlTenantGuard struct {
	limits GuardLimits
	mu     sync.Mutex
}

// NewResctrlTenantGuard builds a guard with the given caps.
func NewResctrlTenantGuard(limits GuardLimits) *ResctrlTenantGuard {
	return &ResctrlTenantGuard{limits: limits}
}

// RegisterNode registers a node with its cache domains and bandwidth capacity.
func (g *ResctrlTenantGuard) RegisterNode(nodeID string, domains []Domain, mbpsCap int) error {
	return errNotImplemented
}

// Node reads the topology of one node.
func (g *ResctrlTenantGuard) Node(nodeID string) (NodeInfo, bool) {
	return NodeInfo{}, false
}

// Create allocates a resource group for a tenant and returns its id.
func (g *ResctrlTenantGuard) Create(tenantID string, spec ResctrlSpec) (string, error) {
	return "", errNotImplemented
}

// Delete removes an empty group and returns its mask and monitor identifier.
func (g *ResctrlTenantGuard) Delete(groupID string) error {
	return errNotImplemented
}

// Assign moves one task into a group.
func (g *ResctrlTenantGuard) Assign(pid int, groupID string) error {
	return errNotImplemented
}

// Detach forgets the group of one task.
func (g *ResctrlTenantGuard) Detach(pid int) error {
	return errNotImplemented
}

// Rebalance reclaims idle groups of a node and returns how many were freed.
func (g *ResctrlTenantGuard) Rebalance(nodeID string) (int, error) {
	return 0, errNotImplemented
}

// Stats returns the occupancy of one group.
func (g *ResctrlTenantGuard) Stats(groupID string) (Counters, error) {
	return Counters{}, errNotImplemented
}

// Group reads one resource group.
func (g *ResctrlTenantGuard) Group(groupID string) (Group, bool) {
	return Group{}, false
}

// Groups lists the groups of a node, ordered by id.
func (g *ResctrlTenantGuard) Groups(nodeID string) []Group {
	return nil
}

// SetOfflineCPUs marks CPUs offline and recomputes the usable cache mask.
func (g *ResctrlTenantGuard) SetOfflineCPUs(nodeID string, cpus []int) error {
	return errNotImplemented
}

// Sample folds one raw counter reading into the running total of a group.
func (g *ResctrlTenantGuard) Sample(groupID string, raw Counters) (Counters, error) {
	return Counters{}, errNotImplemented
}

// Snapshot serialises nodes, groups and tasks.
func (g *ResctrlTenantGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ResctrlTenantGuard) Restore(data []byte) error {
	return errNotImplemented
}

// sortedInts returns a sorted copy of in without duplicates.
func sortedInts(in []int) []int {
	out := make([]int, len(in))
	copy(out, in)
	sort.Ints(out)
	return out
}
