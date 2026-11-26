// Package task hands cache masks and memory bandwidth budgets to tenants,
// moves tasks between resource groups, and keeps monitor identifiers reusable.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"sync"
)

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

const snapshotVersion = 1

// Errors returned by the guard.
var (
	ErrUnknownNode   = errors.New("node is not registered")
	ErrUnknownGroup  = errors.New("resource group is not known")
	ErrBadTenant     = errors.New("tenant id is required")
	ErrBadSpec       = errors.New("resource specification is not valid")
	ErrMaskOverlap   = errors.New("cache mask overlaps another group")
	ErrMaskExhausted = errors.New("no contiguous cache mask is left")
	ErrGroupLimit    = errors.New("group limit reached for this node")
	ErrGroupNotEmpty = errors.New("group still holds tasks")
	ErrBadPID        = errors.New("pid is not valid")
	ErrNoMonitor     = errors.New("no monitor identifier is available")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// GuardLimits bounds one node.
type GuardLimits struct {
	MaxGroups     int `json:"max_groups"`
	MaxMonitorIDs int `json:"max_monitor_ids"`
	MaskBits      int `json:"mask_bits"`
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
	ID       string   `json:"id"`
	NodeID   string   `json:"node_id"`
	TenantID string   `json:"tenant_id"`
	Tenants  []string `json:"tenants"`
	Mask     uint64   `json:"mask"`
	MaskBits int      `json:"mask_bits"`
	Weight   int      `json:"weight"`
	*BandwidthState
	Class      string `json:"class"`
	MonitorID  int    `json:"monitor_id"`
	PIDs       []int  `json:"pids"`
	Generation int    `json:"generation"`
	State      string `json:"state"`
}

// BandwidthState is the mutable budget handle observed by retained Group values.
type BandwidthState struct {
	MBps int `json:"mbps"`
}

// Counters is the occupancy reported for one group.
type Counters struct {
	LLC        uint64 `json:"llc"`
	MBps       uint64 `json:"mbps"`
	MBpsLimit  int    `json:"mbps_limit"`
	MonitorID  int    `json:"monitor_id"`
	Generation int    `json:"generation"`
}

type nodeState struct {
	Info          NodeInfo `json:"info"`
	FullMask      uint64   `json:"full_mask"`
	AllowedMask   uint64   `json:"allowed_mask"`
	MonitorCursor int      `json:"monitor_cursor"`
}

type sampleState struct {
	Have      bool   `json:"have"`
	LastLLC   uint64 `json:"last_llc"`
	LastMBps  uint64 `json:"last_mbps"`
	TotalLLC  uint64 `json:"total_llc"`
	TotalMBps uint64 `json:"total_mbps"`
}

type groupState struct {
	Info   Group       `json:"info"`
	Seq    uint64      `json:"seq"`
	Sample sampleState `json:"sample"`
}

type guardState struct {
	Nodes  map[string]*nodeState  `json:"nodes"`
	Groups map[string]*groupState `json:"groups"`
	PIDs   map[int]string         `json:"pids"`
	Clock  uint64                 `json:"clock"`
}

type snapshotDocument struct {
	Version int         `json:"version"`
	Limits  GuardLimits `json:"limits"`
	State   *guardState `json:"state"`
}

// ResctrlTenantGuard owns nodes, resource groups and the monitor pool.
type ResctrlTenantGuard struct {
	limits GuardLimits
	mu     sync.Mutex
	state  *guardState
}

// NewResctrlTenantGuard builds a guard with the given caps.
func NewResctrlTenantGuard(limits GuardLimits) *ResctrlTenantGuard {
	return &ResctrlTenantGuard{limits: limits, state: newGuardState()}
}

func newGuardState() *guardState {
	return &guardState{
		Nodes:  make(map[string]*nodeState),
		Groups: make(map[string]*groupState),
		PIDs:   make(map[int]string),
	}
}

func (g *ResctrlTenantGuard) stateLocked() *guardState {
	if g.state == nil {
		g.state = newGuardState()
	}
	return g.state
}

// RegisterNode registers a node with its cache domains and bandwidth capacity.
func (g *ResctrlTenantGuard) RegisterNode(nodeID string, domains []Domain, mbpsCap int) error {
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("%w: node id is required", ErrBadSpec)
	}
	if mbpsCap <= 0 {
		return fmt.Errorf("%w: bandwidth capacity must be positive", ErrBadSpec)
	}
	if len(domains) == 0 {
		return fmt.Errorf("%w: at least one cache domain is required", ErrBadSpec)
	}

	copiedDomains := make([]Domain, len(domains))
	for i, domain := range domains {
		if len(domain.CPUs) == 0 {
			return fmt.Errorf("%w: domain %d has no CPUs", ErrBadSpec, domain.ID)
		}
		if domain.Ways < 0 {
			return fmt.Errorf("%w: domain %d has negative ways", ErrBadSpec, domain.ID)
		}
		domain.CPUs = sortedInts(domain.CPUs)
		copiedDomains[i] = domain
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.stateLocked()
	if _, exists := state.Nodes[nodeID]; exists {
		return fmt.Errorf("%w: node %q is already registered", ErrBadSpec, nodeID)
	}

	width := g.nodeMaskWidth(copiedDomains)
	fullMask := lowMask(width)
	info := NodeInfo{
		ID:       nodeID,
		Domains:  copiedDomains,
		MBpsCap:  mbpsCap,
		FreeMask: fullMask,
		Offline:  []int{},
		Version:  0,
	}
	state.Nodes[nodeID] = &nodeState{
		Info:        info,
		FullMask:    fullMask,
		AllowedMask: fullMask,
	}
	return nil
}

// Node reads the topology of one node.
func (g *ResctrlTenantGuard) Node(nodeID string) (NodeInfo, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return NodeInfo{}, false
	}
	node, ok := g.state.Nodes[nodeID]
	if !ok {
		return NodeInfo{}, false
	}
	return cloneNodeInfo(node.Info), true
}

// Create allocates a resource group for a tenant and returns its id.
func (g *ResctrlTenantGuard) Create(tenantID string, spec ResctrlSpec) (string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return "", ErrBadTenant
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.stateLocked()
	node, ok := state.Nodes[spec.NodeID]
	if !ok {
		return "", ErrUnknownNode
	}

	class := spec.Class
	if class == "" {
		class = ClassDedicated
	}
	if class != ClassDedicated && class != ClassShared {
		return "", fmt.Errorf("%w: unknown class %q", ErrBadSpec, spec.Class)
	}

	groupID := spec.NodeID + "/" + tenantID
	if class == ClassShared {
		groupID = spec.NodeID + "/shared"
	}

	if existing, exists := state.Groups[groupID]; exists {
		if class == ClassShared {
			existing.Info.Tenants = appendStringUnique(existing.Info.Tenants, tenantID)
		}
		return groupID, nil
	}

	requestedMask, maskBits, err := validateMaskSpec(node, spec)
	if err != nil {
		return "", err
	}

	maxGroups := g.limits.MaxGroups
	if maxGroups < 0 {
		maxGroups = 0
	}
	if len(state.groupsForNode(spec.NodeID)) >= maxGroups {
		victim := state.oldestIdleGroup(spec.NodeID)
		if victim == nil {
			return "", ErrGroupLimit
		}
		state.deleteGroupLocked(victim.Info.ID)
	}

	usedMask := state.maskUsedByNode(spec.NodeID)
	var groupMask uint64
	if spec.Mask != 0 {
		if requestedMask&usedMask != 0 {
			return "", fmt.Errorf("%w: mask %#x overlaps used mask %#x", ErrMaskOverlap, requestedMask, usedMask)
		}
		groupMask = requestedMask
	} else {
		groupMask, err = allocateContiguousMask(node.AllowedMask&^usedMask, maskBits)
		if err != nil {
			return "", err
		}
	}

	monitorID, err := state.allocateMonitorID(node, g.limits.MaxMonitorIDs)
	if err != nil {
		return "", err
	}

	state.Clock++
	weight := spec.Weight
	if weight <= 0 {
		weight = 1
	}
	group := Group{
		ID:             groupID,
		NodeID:         spec.NodeID,
		TenantID:       tenantID,
		Tenants:        []string{tenantID},
		Mask:           groupMask,
		MaskBits:       maskBits,
		Weight:         weight,
		BandwidthState: &BandwidthState{},
		Class:          class,
		MonitorID:      monitorID,
		PIDs:           []int{},
		Generation:     int(state.Clock),
		State:          StateActive,
	}
	state.Groups[groupID] = &groupState{Info: group, Seq: state.Clock}
	state.recalculateBandwidthLocked(node)
	state.recomputeFreeMaskLocked(node)
	return groupID, nil
}

// Delete removes an empty group and returns its mask and monitor identifier.
func (g *ResctrlTenantGuard) Delete(groupID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return ErrUnknownGroup
	}
	group, ok := g.state.Groups[groupID]
	if !ok {
		return ErrUnknownGroup
	}
	if len(group.Info.PIDs) != 0 {
		return ErrGroupNotEmpty
	}
	g.state.deleteGroupLocked(groupID)
	return nil
}

// Assign moves one task into a group.
func (g *ResctrlTenantGuard) Assign(pid int, groupID string) error {
	if pid <= 0 {
		return ErrBadPID
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return ErrUnknownGroup
	}
	target, ok := g.state.Groups[groupID]
	if !ok {
		return ErrUnknownGroup
	}

	if oldGroupID, assigned := g.state.PIDs[pid]; assigned && oldGroupID != groupID {
		if oldGroup := g.state.Groups[oldGroupID]; oldGroup != nil {
			oldGroup.Info.PIDs = removePID(oldGroup.Info.PIDs, pid)
		}
	}

	target.Info.PIDs = append(target.Info.PIDs, pid)
	sort.Ints(target.Info.PIDs)
	target.Info.PIDs = compactInts(target.Info.PIDs)
	g.state.PIDs[pid] = groupID
	return nil
}

// Detach forgets the group of one task.
func (g *ResctrlTenantGuard) Detach(pid int) error {
	if pid <= 0 {
		return ErrBadPID
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return nil
	}
	groupID, ok := g.state.PIDs[pid]
	if !ok {
		return nil
	}
	if group, exists := g.state.Groups[groupID]; exists {
		group.Info.PIDs = removePID(group.Info.PIDs, pid)
	}
	delete(g.state.PIDs, pid)
	return nil
}

// Rebalance reclaims idle groups of a node and returns how many were freed.
func (g *ResctrlTenantGuard) Rebalance(nodeID string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return 0, ErrUnknownNode
	}
	node, ok := g.state.Nodes[nodeID]
	if !ok {
		return 0, ErrUnknownNode
	}

	var idleIDs []string
	for _, group := range g.state.Groups {
		if group.Info.NodeID == nodeID && len(group.Info.PIDs) == 0 {
			idleIDs = append(idleIDs, group.Info.ID)
		}
	}
	sort.Strings(idleIDs)
	for _, id := range idleIDs {
		g.state.deleteGroupLocked(id)
	}
	if len(idleIDs) > 0 {
		g.state.recalculateBandwidthLocked(node)
		g.state.recomputeFreeMaskLocked(node)
	}
	return len(idleIDs), nil
}

// Stats returns the occupancy of one group.
func (g *ResctrlTenantGuard) Stats(groupID string) (Counters, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return Counters{}, ErrUnknownGroup
	}
	group, ok := g.state.Groups[groupID]
	if !ok {
		return Counters{}, ErrUnknownGroup
	}
	return group.statsLocked(), nil
}

// Group reads one resource group.
func (g *ResctrlTenantGuard) Group(groupID string) (Group, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return Group{}, false
	}
	group, ok := g.state.Groups[groupID]
	if !ok {
		return Group{}, false
	}
	return cloneGroup(group.Info), true
}

// Groups lists the groups of a node, ordered by id.
func (g *ResctrlTenantGuard) Groups(nodeID string) []Group {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return []Group{}
	}
	groups := g.state.groupsForNode(nodeID)
	out := make([]Group, 0, len(groups))
	for _, group := range groups {
		out = append(out, cloneGroup(group.Info))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SetOfflineCPUs marks CPUs offline and recomputes the usable cache mask.
func (g *ResctrlTenantGuard) SetOfflineCPUs(nodeID string, cpus []int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return ErrUnknownNode
	}
	node, ok := g.state.Nodes[nodeID]
	if !ok {
		return ErrUnknownNode
	}
	for _, cpu := range cpus {
		if cpu < 0 {
			return fmt.Errorf("%w: offline cpu must not be negative", ErrBadSpec)
		}
	}

	offline := sortedInts(cpus)
	changed := !equalInts(node.Info.Offline, offline)
	node.Info.Offline = offline

	var offlineMask uint64
	for _, cpu := range offline {
		if cpu >= 0 && cpu < 64 {
			offlineMask |= uint64(1) << uint(cpu)
		}
	}
	node.AllowedMask = node.FullMask &^ offlineMask
	if changed {
		node.Info.Version++
	}
	g.state.recomputeFreeMaskLocked(node)
	return nil
}

// Sample folds one raw counter reading into the running total of a group.
func (g *ResctrlTenantGuard) Sample(groupID string, raw Counters) (Counters, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		return Counters{}, ErrUnknownGroup
	}
	group, ok := g.state.Groups[groupID]
	if !ok {
		return Counters{}, ErrUnknownGroup
	}

	if !group.Sample.Have {
		group.Sample.Have = true
		group.Sample.LastLLC = raw.LLC
		group.Sample.LastMBps = raw.MBps
	} else {
		group.Sample.TotalLLC += counterDelta(group.Sample.LastLLC, raw.LLC)
		group.Sample.TotalMBps += counterDelta(group.Sample.LastMBps, raw.MBps)
		group.Sample.LastLLC = raw.LLC
		group.Sample.LastMBps = raw.MBps
	}
	return group.statsLocked(), nil
}

// Snapshot serialises nodes, groups and tasks.
func (g *ResctrlTenantGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.stateLocked()
	doc := snapshotDocument{
		Version: snapshotVersion,
		Limits:  g.limits,
		State:   state,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ResctrlTenantGuard) Restore(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	var doc snapshotDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if doc.Version != snapshotVersion || doc.State == nil {
		return fmt.Errorf("%w: unsupported snapshot document", ErrBadSnapshot)
	}

	limits := g.limits
	if limits == (GuardLimits{}) && doc.Limits != (GuardLimits{}) {
		limits = doc.Limits
	}
	restored, err := buildRestoredState(doc.State, limits)
	if err != nil {
		return err
	}

	g.limits = limits
	g.state = restored
	return nil
}

func (g *ResctrlTenantGuard) nodeMaskWidth(domains []Domain) int {
	width := g.limits.MaskBits
	if width <= 0 {
		return 0
	}
	if width > 64 {
		width = 64
	}
	for _, domain := range domains {
		if domain.Ways > 0 && domain.Ways < width {
			width = domain.Ways
		}
	}
	return width
}

func (s *guardState) groupsForNode(nodeID string) []*groupState {
	var out []*groupState
	for _, group := range s.Groups {
		if group.Info.NodeID == nodeID {
			out = append(out, group)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.ID < out[j].Info.ID })
	return out
}

func (s *guardState) oldestIdleGroup(nodeID string) *groupState {
	var victim *groupState
	for _, group := range s.Groups {
		if group.Info.NodeID != nodeID || len(group.Info.PIDs) != 0 {
			continue
		}
		if victim == nil || group.Seq < victim.Seq {
			victim = group
		}
	}
	return victim
}

func (s *guardState) deleteGroupLocked(groupID string) {
	group := s.Groups[groupID]
	if group == nil {
		return
	}
	for _, pid := range group.Info.PIDs {
		if owner := s.PIDs[pid]; owner == groupID {
			delete(s.PIDs, pid)
		}
	}
	nodeID := group.Info.NodeID
	delete(s.Groups, groupID)
	if node, ok := s.Nodes[nodeID]; ok {
		s.recalculateBandwidthLocked(node)
		s.recomputeFreeMaskLocked(node)
	}
}

func (s *guardState) maskUsedByNode(nodeID string) uint64 {
	var used uint64
	for _, group := range s.Groups {
		if group.Info.NodeID == nodeID {
			used |= group.Info.Mask
		}
	}
	return used
}

func (s *guardState) recomputeFreeMaskLocked(node *nodeState) {
	node.Info.FreeMask = node.AllowedMask &^ s.maskUsedByNode(node.Info.ID)
}

func (s *guardState) recalculateBandwidthLocked(node *nodeState) {
	groups := s.groupsForNode(node.Info.ID)
	totalWeight := 0
	for _, group := range groups {
		if group.Info.Weight <= 0 {
			group.Info.Weight = 1
		}
		totalWeight += group.Info.Weight
	}
	for _, group := range groups {
		if totalWeight == 0 {
			group.Info.MBps = 0
			continue
		}
		group.Info.MBps = int(int64(group.Info.Weight) * int64(node.Info.MBpsCap) / int64(totalWeight))
	}
}

func (s *guardState) allocateMonitorID(node *nodeState, limit int) (int, error) {
	if limit <= 0 {
		return 0, ErrNoMonitor
	}
	used := make(map[int]int)
	for _, group := range s.Groups {
		if group.Info.NodeID != node.Info.ID {
			continue
		}
		if group.Info.MonitorID >= 0 && group.Info.MonitorID < limit {
			used[group.Info.MonitorID]++
		}
	}
	for id := 0; id < limit; id++ {
		if used[id] == 0 {
			return id, nil
		}
	}
	id := node.MonitorCursor % limit
	node.MonitorCursor++
	return id, nil
}

func (g *groupState) statsLocked() Counters {
	return Counters{
		LLC:        g.Sample.TotalLLC,
		MBps:       g.Sample.TotalMBps,
		MBpsLimit:  g.Info.MBps,
		MonitorID:  g.Info.MonitorID,
		Generation: g.Info.Generation,
	}
}

func validateMaskSpec(node *nodeState, spec ResctrlSpec) (uint64, int, error) {
	if spec.Mask != 0 {
		mask := spec.Mask
		if mask&^node.FullMask != 0 {
			return 0, 0, fmt.Errorf("%w: mask %#x is wider than node mask %#x", ErrBadSpec, mask, node.FullMask)
		}
		if mask&^node.AllowedMask != 0 {
			return 0, 0, fmt.Errorf("%w: mask %#x uses offline bits", ErrBadSpec, mask)
		}
		return mask, bits.OnesCount64(mask), nil
	}

	width := spec.MaskBits
	if width <= 0 {
		return 0, 0, fmt.Errorf("%w: mask or positive mask width is required", ErrBadSpec)
	}
	if width > 64 || lowMask(width)&^node.FullMask != 0 {
		return 0, 0, fmt.Errorf("%w: mask width %d exceeds node width", ErrBadSpec, width)
	}
	return 0, width, nil
}

func allocateContiguousMask(free uint64, width int) (uint64, error) {
	if width <= 0 || width > 64 {
		return 0, ErrBadSpec
	}
	window := lowMask(width)
	for start := 0; start+width <= 64; start++ {
		candidate := window << uint(start)
		if candidate&free == candidate {
			return candidate, nil
		}
	}
	return 0, ErrMaskExhausted
}

func buildRestoredState(doc *guardState, limits GuardLimits) (*guardState, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: empty state", ErrBadSnapshot)
	}
	if doc.Nodes == nil {
		doc.Nodes = make(map[string]*nodeState)
	}
	if doc.Groups == nil {
		doc.Groups = make(map[string]*groupState)
	}
	if doc.PIDs == nil {
		doc.PIDs = make(map[int]string)
	}

	maskLimit := limits.MaskBits
	if maskLimit > 64 {
		maskLimit = 64
	}
	maxGroups := limits.MaxGroups
	if maxGroups < 0 {
		maxGroups = 0
	}
	maxMonitors := limits.MaxMonitorIDs
	if maxMonitors < 0 {
		maxMonitors = 0
	}

	for nodeID, node := range doc.Nodes {
		if node == nil {
			return nil, fmt.Errorf("%w: nil node", ErrBadSnapshot)
		}
		if node.Info.ID == "" {
			node.Info.ID = nodeID
		}
		if node.Info.ID != nodeID || node.Info.MBpsCap <= 0 || len(node.Info.Domains) == 0 {
			return nil, fmt.Errorf("%w: invalid node %q", ErrBadSnapshot, nodeID)
		}
		for _, domain := range node.Info.Domains {
			if len(domain.CPUs) == 0 || domain.Ways < 0 {
				return nil, fmt.Errorf("%w: invalid domain for node %q", ErrBadSnapshot, nodeID)
			}
		}
		if node.MonitorCursor < 0 {
			return nil, fmt.Errorf("%w: negative monitor cursor", ErrBadSnapshot)
		}

		width := maskLimit
		if width > 0 {
			for _, domain := range node.Info.Domains {
				if domain.Ways > 0 && domain.Ways < width {
					width = domain.Ways
				}
			}
		}
		node.FullMask = lowMask(width)
		node.Info.Offline = sortedInts(node.Info.Offline)
		var offlineMask uint64
		for _, cpu := range node.Info.Offline {
			if cpu < 0 {
				return nil, fmt.Errorf("%w: negative offline cpu", ErrBadSnapshot)
			}
			if cpu < 64 {
				offlineMask |= uint64(1) << uint(cpu)
			}
		}
		node.AllowedMask = node.FullMask &^ offlineMask
		node.Info.Domains = cloneDomains(node.Info.Domains)
	}

	pidOwners := make(map[int]string)
	maxSeq := doc.Clock
	for groupID, group := range doc.Groups {
		if group == nil {
			return nil, fmt.Errorf("%w: nil group", ErrBadSnapshot)
		}
		info := &group.Info
		if info.ID == "" {
			info.ID = groupID
		}
		if info.ID != groupID {
			return nil, fmt.Errorf("%w: group id mismatch", ErrBadSnapshot)
		}
		node, ok := doc.Nodes[info.NodeID]
		if !ok {
			return nil, fmt.Errorf("%w: group %q has unknown node", ErrBadSnapshot, groupID)
		}
		if info.Class == "" {
			info.Class = ClassDedicated
		}
		if info.Class != ClassDedicated && info.Class != ClassShared {
			return nil, fmt.Errorf("%w: group %q has invalid class", ErrBadSnapshot, groupID)
		}
		if strings.TrimSpace(info.TenantID) == "" || len(info.Tenants) == 0 {
			return nil, fmt.Errorf("%w: group %q has no tenant", ErrBadSnapshot, groupID)
		}
		if info.BandwidthState == nil {
			info.BandwidthState = &BandwidthState{}
		}
		if info.Weight <= 0 {
			info.Weight = 1
		}
		if info.State == "" {
			info.State = StateActive
		}
		if info.State != StateActive && info.State != StateReclaimed {
			return nil, fmt.Errorf("%w: group %q has invalid state", ErrBadSnapshot, groupID)
		}
		if info.Mask == 0 || info.Mask&^node.FullMask != 0 {
			return nil, fmt.Errorf("%w: group %q has invalid mask", ErrBadSnapshot, groupID)
		}
		if info.MaskBits != bits.OnesCount64(info.Mask) || info.MaskBits <= 0 {
			return nil, fmt.Errorf("%w: group %q has invalid mask width", ErrBadSnapshot, groupID)
		}
		if maxMonitors <= 0 || info.MonitorID < 0 || info.MonitorID >= maxMonitors {
			return nil, fmt.Errorf("%w: group %q has invalid monitor id", ErrBadSnapshot, groupID)
		}
		if group.Seq == 0 {
			group.Seq = uint64(info.Generation)
		}
		if group.Seq == 0 {
			return nil, fmt.Errorf("%w: group %q has no generation", ErrBadSnapshot, groupID)
		}
		if group.Seq > maxSeq {
			maxSeq = group.Seq
		}
		if info.Generation <= 0 {
			info.Generation = int(group.Seq)
		}

		seenPID := make(map[int]bool)
		for _, pid := range info.PIDs {
			if pid <= 0 || seenPID[pid] {
				return nil, fmt.Errorf("%w: group %q has invalid pid list", ErrBadSnapshot, groupID)
			}
			seenPID[pid] = true
			if owner, exists := pidOwners[pid]; exists && owner != groupID {
				return nil, fmt.Errorf("%w: pid %d has multiple owners", ErrBadSnapshot, pid)
			}
			pidOwners[pid] = groupID
		}
		info.PIDs = sortedInts(info.PIDs)

		usedMask := uint64(0)
		for otherID, other := range doc.Groups {
			if other == nil || other.Info.NodeID != info.NodeID || otherID == groupID {
				continue
			}
			usedMask |= other.Info.Mask
		}
		if info.Mask&usedMask != 0 {
			return nil, fmt.Errorf("%w: group %q overlaps another mask", ErrBadSnapshot, groupID)
		}
	}

	for nodeID, node := range doc.Nodes {
		count := 0
		for _, group := range doc.Groups {
			if group.Info.NodeID == nodeID {
				count++
			}
		}
		if count > maxGroups {
			return nil, fmt.Errorf("%w: node %q exceeds group limit", ErrBadSnapshot, nodeID)
		}
		doc.recalculateBandwidthLocked(node)
		doc.recomputeFreeMaskLocked(node)
	}

	if len(doc.PIDs) != len(pidOwners) {
		return nil, fmt.Errorf("%w: task ownership is inconsistent", ErrBadSnapshot)
	}
	for pid, groupID := range pidOwners {
		if doc.PIDs[pid] != groupID {
			return nil, fmt.Errorf("%w: task ownership is inconsistent", ErrBadSnapshot)
		}
	}
	for pid, groupID := range doc.PIDs {
		if pid <= 0 || pidOwners[pid] != groupID {
			return nil, fmt.Errorf("%w: task ownership is inconsistent", ErrBadSnapshot)
		}
	}
	doc.Clock = maxSeq
	return doc, nil
}

func counterDelta(last, current uint64) uint64 {
	if current >= last {
		return current - last
	}
	wrapped := current - last
	if wrapped > ^uint64(0)>>1 {
		return current
	}
	return wrapped
}

func lowMask(width int) uint64 {
	if width <= 0 {
		return 0
	}
	if width >= 64 {
		return ^uint64(0)
	}
	return (uint64(1) << uint(width)) - 1
}

func cloneNodeInfo(in NodeInfo) NodeInfo {
	out := in
	out.Domains = cloneDomains(in.Domains)
	out.Offline = append([]int(nil), in.Offline...)
	return out
}

func cloneDomains(in []Domain) []Domain {
	out := make([]Domain, len(in))
	for i, domain := range in {
		domain.CPUs = append([]int(nil), domain.CPUs...)
		out[i] = domain
	}
	return out
}

func cloneGroup(in Group) Group {
	out := in
	out.Tenants = append([]string(nil), in.Tenants...)
	out.PIDs = append([]int(nil), in.PIDs...)
	return out
}

func appendStringUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

func removePID(pids []int, pid int) []int {
	out := make([]int, 0, len(pids))
	for _, candidate := range pids {
		if candidate != pid {
			out = append(out, candidate)
		}
	}
	return out
}

func compactInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	write := 1
	for read := 1; read < len(in); read++ {
		if in[read] != in[write-1] {
			in[write] = in[read]
			write++
		}
	}
	return in[:write]
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortedInts returns a sorted copy of in without duplicates.
func sortedInts(in []int) []int {
	if len(in) == 0 {
		return []int{}
	}
	out := append([]int(nil), in...)
	sort.Ints(out)
	return compactInts(out)
}
