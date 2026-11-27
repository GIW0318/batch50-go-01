// Package task hands out compute capacity for security domains while keeping
// the two hyperthread siblings of one physical core inside the same domain.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

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

const (
	maxTopologyCPUs = 4096
	maxAllocations  = 65536
	maxDomains      = 65536
	maxSnapshotSize = 32 << 20
	snapshotVersion = 1
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

type coreInfo struct {
	Core
	usable bool
}

type domainInfo struct {
	load       int
	lastReason string
}

// SmtSiblingIsolator owns the topology, the core ownership table and the
// monotonic counters.
type SmtSiblingIsolator struct {
	mu sync.Mutex

	limits Limits

	topology []Core
	cpus     []CPU
	cpuByID  map[int]CPU
	cores    []coreInfo

	allocations map[string]Allocation
	cpuOwners   map[int]string
	coreOwners  map[int]string
	domains     map[string]*domainInfo
	nextGen     int64
}

// NewSmtSiblingIsolator builds an isolator with the given policy.
func NewSmtSiblingIsolator(limits Limits) *SmtSiblingIsolator {
	return &SmtSiblingIsolator{
		limits:      limits,
		cpuByID:     make(map[int]CPU),
		cpuOwners:   make(map[int]string),
		coreOwners:  make(map[int]string),
		allocations: make(map[string]Allocation),
		domains:     make(map[string]*domainInfo),
		nextGen:     1,
	}
}

// SetTopology installs the processor topology and rebuilds the core groups.
func (s *SmtSiblingIsolator) SetTopology(cpus []CPU) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.allocations) != 0 {
		return ErrBusy
	}

	topology, normalized, cores, err := buildTopology(cpus)
	if err != nil {
		return ErrBadTopology
	}

	s.topology = topology
	s.cpus = normalized
	s.cpuByID = make(map[int]CPU, len(normalized))
	for _, cpu := range normalized {
		s.cpuByID[cpu.ID] = cpu
	}
	s.cores = cores
	s.allocations = make(map[string]Allocation)
	s.cpuOwners = make(map[int]string)
	s.coreOwners = make(map[int]string)
	s.domains = make(map[string]*domainInfo)
	s.nextGen = 1
	return nil
}

// Topology returns a read only copy of the core groups, ordered by core id.
func (s *SmtSiblingIsolator) Topology() []Core {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyCores(s.topology)
}

// Reserve hands out threads to one domain without mixing domains on a core.
func (s *SmtSiblingIsolator) Reserve(domain string, req Resource) (Allocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(domain) == "" {
		return Allocation{}, ErrBadDomain
	}
	if req.Threads <= 0 {
		return Allocation{}, ErrBadRequest
	}

	if len(s.cores) == 0 || len(s.allocations) >= maxAllocations {
		s.noteFailure(domain, ReasonCapacityExhausted)
		return Allocation{}, ErrCapacity
	}
	if req.Threads > maxTopologyCPUs {
		s.noteFailure(domain, ReasonCapacityExhausted)
		return Allocation{}, ErrCapacity
	}
	if _, exists := s.domains[domain]; !exists && len(s.domains) >= maxDomains {
		return Allocation{}, ErrCapacity
	}

	selectedCPUs, selectedCores := s.plan(domain, req)
	if len(selectedCPUs) < req.Threads {
		usable, blocked := s.freeCapacity(domain)
		reason := ReasonCapacityExhausted
		if usable < req.Threads && usable+blocked >= req.Threads {
			reason = ReasonSiblingBlocked
		}
		s.noteFailure(domain, reason)
		return Allocation{}, ErrCapacity
	}

	// Re-check every processor on the selected cores, including siblings that
	// this allocation did not touch. A damaged ownership table must fail closed
	// instead of ever letting one core belong to two domains.
	for _, coreID := range selectedCores {
		core, ok := s.coreByID(coreID)
		if !ok {
			s.noteFailure(domain, ReasonCapacityExhausted)
			return Allocation{}, ErrCapacity
		}
		owner, owned, conflict := s.coreOwnership(core)
		if conflict || (owned && owner != domain) {
			s.noteFailure(domain, ReasonSiblingBlocked)
			return Allocation{}, ErrCapacity
		}
		for _, cpuID := range core.CPUs {
			if cpuOwner, ok := s.cpuOwners[cpuID]; ok && cpuOwner != domain {
				s.noteFailure(domain, ReasonSiblingBlocked)
				return Allocation{}, ErrCapacity
			}
		}
	}

	gen, ok := s.nextGeneration()
	if !ok {
		s.noteFailure(domain, ReasonCapacityExhausted)
		return Allocation{}, ErrCapacity
	}
	alloc := Allocation{
		ID:      allocationID(gen),
		Domain:  domain,
		CPUs:    append([]int(nil), selectedCPUs...),
		Cores:   append([]int(nil), selectedCores...),
		Threads: len(selectedCPUs),
		Gen:     gen,
	}
	if req.Exclusive && alloc.Threads > req.Threads {
		alloc.Degraded = true
		alloc.Reason = ReasonWholeCore
	}

	for _, cpuID := range alloc.CPUs {
		s.cpuOwners[cpuID] = domain
	}
	for _, coreID := range alloc.Cores {
		s.coreOwners[coreID] = domain
	}
	s.allocations[alloc.ID] = alloc
	state := s.domainInfo(domain)
	if alloc.Degraded {
		state.lastReason = alloc.Reason
	} else if state.lastReason != ReasonLoadBelowThreshold {
		state.lastReason = ""
	}
	return copyAllocation(alloc), nil
}

// Release returns one allocation and restores the affinity of its cores.
func (s *SmtSiblingIsolator) Release(allocID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	alloc, ok := s.allocations[allocID]
	if !ok {
		return ErrUnknownAlloc
	}

	delete(s.allocations, allocID)
	for _, cpuID := range alloc.CPUs {
		delete(s.cpuOwners, cpuID)
	}
	for _, coreID := range alloc.Cores {
		if !s.coreHasOwnedCPU(coreID) {
			delete(s.coreOwners, coreID)
			continue
		}
		// All surviving allocations on a core are necessarily from the same
		// domain; repair the owner from the live allocation table if needed.
		if owner, ok := s.coreOwners[coreID]; !ok || owner != alloc.Domain {
			s.coreOwners[coreID] = s.liveCoreOwner(coreID)
		}
	}
	if s.domainAllocationCountLocked(alloc.Domain) == 0 {
		if state, ok := s.domains[alloc.Domain]; ok {
			state.lastReason = ""
		}
	}
	return nil
}

// Rebalance reclaims idle domain state and re-evaluates yielding.
func (s *SmtSiblingIsolator) Rebalance() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for domain, state := range s.domains {
		if s.domainAllocationCountLocked(domain) == 0 {
			delete(s.domains, domain)
			continue
		}
		if state.load < s.limits.YieldReleaseLoad {
			// The sibling slots are already exclusively available to this
			// domain. Ending the low-load yield state makes them part of the
			// domain's usable pool again and records the transition.
			state.lastReason = ReasonLoadBelowThreshold
		}
	}
	s.repairOwnership()
	return nil
}

// Allocation reads one allocation record.
func (s *SmtSiblingIsolator) Allocation(allocID string) (Allocation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	alloc, ok := s.allocations[allocID]
	if !ok {
		return Allocation{}, false
	}
	return copyAllocation(alloc), true
}

// Allocations lists every live allocation ordered by allocation id.
func (s *SmtSiblingIsolator) Allocations() []Allocation {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Allocation, 0, len(s.allocations))
	for _, alloc := range s.allocations {
		out = append(out, copyAllocation(alloc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DomainUsage reports the aggregated footprint of one domain.
func (s *SmtSiblingIsolator) DomainUsage(domain string) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()

	usage := Usage{Domain: domain}
	coreSet := make(map[int]struct{})
	for _, alloc := range s.allocations {
		if alloc.Domain != domain {
			continue
		}
		usage.Allocations++
		usage.Threads += alloc.Threads
		usage.CPUs = append(usage.CPUs, alloc.CPUs...)
		for _, coreID := range alloc.Cores {
			coreSet[coreID] = struct{}{}
		}
	}
	sort.Ints(usage.CPUs)
	usage.CPUs = uniqueInts(usage.CPUs)
	for coreID := range coreSet {
		usage.Cores = append(usage.Cores, coreID)
	}
	sort.Ints(usage.Cores)
	if state, ok := s.domains[domain]; ok {
		usage.Load = state.load
		usage.LastReason = state.lastReason
	}
	return usage
}

// SetLoad records the current load of one domain.
func (s *SmtSiblingIsolator) SetLoad(domain string, load int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(domain) == "" {
		return ErrBadDomain
	}
	if load < 0 {
		return ErrBadRequest
	}
	if _, ok := s.domains[domain]; !ok && len(s.domains) >= maxDomains {
		return ErrCapacity
	}
	state := s.domainInfo(domain)
	state.load = load
	if load < s.limits.YieldReleaseLoad {
		// The yielding state has ended and the domain's sibling slots are back
		// in its own isolated pool.
		state.lastReason = ReasonLoadBelowThreshold
	} else if state.lastReason == ReasonLoadBelowThreshold {
		state.lastReason = ""
	}
	return nil
}

// Domains lists the domains that still hold capacity, ordered by name.
func (s *SmtSiblingIsolator) Domains() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	set := make(map[string]struct{})
	for _, alloc := range s.allocations {
		set[alloc.Domain] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for domain := range set {
		out = append(out, domain)
	}
	sort.Strings(out)
	return out
}

type snapshotDocument struct {
	Version     int              `json:"version"`
	Topology    []CPU            `json:"topology"`
	Allocations []Allocation     `json:"allocations"`
	Domains     []domainSnapshot `json:"domains,omitempty"`
	NextGen     int64            `json:"next_gen"`
}

type domainSnapshot struct {
	Domain     string `json:"domain"`
	Load       int    `json:"load"`
	LastReason string `json:"last_reason"`
}

// Snapshot serialises the topology, the allocations and the counters.
func (s *SmtSiblingIsolator) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := snapshotDocument{
		Version:     snapshotVersion,
		Topology:    copyCPUs(s.cpus),
		Allocations: make([]Allocation, 0, len(s.allocations)),
		Domains:     make([]domainSnapshot, 0, len(s.domains)),
		NextGen:     s.nextGen,
	}
	for _, alloc := range s.allocations {
		doc.Allocations = append(doc.Allocations, copyAllocation(alloc))
	}
	sort.Slice(doc.Allocations, func(i, j int) bool {
		return doc.Allocations[i].ID < doc.Allocations[j].ID
	})
	for domain, state := range s.domains {
		doc.Domains = append(doc.Domains, domainSnapshot{
			Domain:     domain,
			Load:       state.load,
			LastReason: state.lastReason,
		})
	}
	sort.Slice(doc.Domains, func(i, j int) bool {
		return doc.Domains[i].Domain < doc.Domains[j].Domain
	})

	data, err := json.Marshal(doc)
	if err != nil {
		return nil, ErrBadSnapshot
	}
	if len(data) > maxSnapshotSize {
		return nil, ErrBadSnapshot
	}
	return data, nil
}

// Restore replaces the whole isolator state with a snapshot, all or nothing.
func (s *SmtSiblingIsolator) Restore(data []byte) error {
	if len(data) == 0 || len(data) > maxSnapshotSize {
		return ErrBadSnapshot
	}

	var doc snapshotDocument
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return ErrBadSnapshot
	}
	if err := ensureEOF(dec); err != nil || doc.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	var topology []Core
	var normalized []CPU
	var cores []coreInfo
	if len(doc.Topology) == 0 {
		if len(doc.Allocations) != 0 {
			return ErrBadSnapshot
		}
	} else {
		var err error
		topology, normalized, cores, err = buildTopology(doc.Topology)
		if err != nil {
			return ErrBadSnapshot
		}
	}
	if len(doc.Allocations) > maxAllocations || len(doc.Domains) > maxDomains {
		return ErrBadSnapshot
	}

	cpuByID := make(map[int]CPU, len(normalized))
	for _, cpu := range normalized {
		cpuByID[cpu.ID] = cpu
	}
	usableCores := make(map[int]bool, len(cores))
	for _, core := range cores {
		usableCores[core.ID] = core.usable
	}
	allocations := make(map[string]Allocation, len(doc.Allocations))
	cpuOwners := make(map[int]string)
	coreOwners := make(map[int]string)
	seenGens := make(map[int64]struct{}, len(doc.Allocations))
	var maxGen int64
	for _, input := range doc.Allocations {
		if !validAllocationInput(input) {
			return ErrBadSnapshot
		}
		if _, exists := allocations[input.ID]; exists {
			return ErrBadSnapshot
		}
		if _, exists := seenGens[input.Gen]; exists {
			return ErrBadSnapshot
		}
		seenGens[input.Gen] = struct{}{}
		alloc := copyAllocation(input)
		sort.Ints(alloc.CPUs)
		alloc.CPUs = uniqueInts(alloc.CPUs)
		if len(alloc.CPUs) != alloc.Threads {
			return ErrBadSnapshot
		}
		coreSet := make(map[int]struct{})
		for _, cpuID := range alloc.CPUs {
			cpu, ok := cpuByID[cpuID]
			if !ok || !cpu.Online || cpu.Reserved || !usableCores[cpu.Core] {
				return ErrBadSnapshot
			}
			coreSet[cpu.Core] = struct{}{}
			if _, exists := cpuOwners[cpuID]; exists {
				return ErrBadSnapshot
			}
			cpuOwners[cpuID] = alloc.Domain
			if owner, exists := coreOwners[cpu.Core]; exists && owner != alloc.Domain {
				return ErrBadSnapshot
			}
			coreOwners[cpu.Core] = alloc.Domain
		}
		actualCores := make([]int, 0, len(coreSet))
		for coreID := range coreSet {
			actualCores = append(actualCores, coreID)
		}
		sort.Ints(actualCores)
		if !equalInts(actualCores, alloc.Cores) {
			return ErrBadSnapshot
		}
		if alloc.Gen > maxGen {
			maxGen = alloc.Gen
		}
		allocations[alloc.ID] = alloc
	}

	if doc.NextGen <= maxGen || doc.NextGen <= 0 || doc.NextGen >= math.MaxInt64 {
		return ErrBadSnapshot
	}
	domains := make(map[string]*domainInfo, len(doc.Domains))
	for _, input := range doc.Domains {
		if strings.TrimSpace(input.Domain) == "" || input.Load < 0 {
			return ErrBadSnapshot
		}
		if _, exists := domains[input.Domain]; exists {
			return ErrBadSnapshot
		}
		domains[input.Domain] = &domainInfo{
			load:       input.Load,
			lastReason: input.LastReason,
		}
	}
	for domain := range allocations {
		if _, ok := domains[domain]; !ok {
			domains[domain] = &domainInfo{}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.topology = topology
	s.cpus = normalized
	s.cpuByID = cpuByID
	s.cores = cores
	s.allocations = allocations
	s.cpuOwners = cpuOwners
	s.coreOwners = coreOwners
	s.domains = domains
	s.nextGen = doc.NextGen
	return nil
}

func buildTopology(input []CPU) ([]Core, []CPU, []coreInfo, error) {
	if len(input) == 0 || len(input) > maxTopologyCPUs {
		return nil, nil, nil, ErrBadTopology
	}

	seen := make(map[int]struct{}, len(input))
	grouped := make(map[int][]CPU)
	for _, cpu := range input {
		if cpu.ID < 0 || cpu.Core < 0 || cpu.Thread < 0 {
			return nil, nil, nil, ErrBadTopology
		}
		if _, exists := seen[cpu.ID]; exists {
			return nil, nil, nil, ErrBadTopology
		}
		seen[cpu.ID] = struct{}{}
		grouped[cpu.Core] = append(grouped[cpu.Core], cpu)
	}

	coreIDs := make([]int, 0, len(grouped))
	for coreID := range grouped {
		coreIDs = append(coreIDs, coreID)
	}
	sort.Ints(coreIDs)

	topology := make([]Core, 0, len(coreIDs))
	cores := make([]coreInfo, 0, len(coreIDs))
	normalized := make([]CPU, 0, len(input))
	for _, coreID := range coreIDs {
		members := grouped[coreID]
		if len(members) == 0 || len(members) > 2 {
			return nil, nil, nil, ErrBadTopology
		}
		sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
		online := true
		usable := true
		cpuIDs := make([]int, 0, len(members))
		for _, cpu := range members {
			cpuIDs = append(cpuIDs, cpu.ID)
			normalized = append(normalized, cpu)
			if !cpu.Online {
				online = false
				usable = false
			}
			if cpu.Reserved {
				usable = false
			}
		}
		publicCore := Core{
			ID:     coreID,
			CPUs:   append([]int(nil), cpuIDs...),
			Online: online,
		}
		topology = append(topology, publicCore)
		cores = append(cores, coreInfo{Core: publicCore, usable: usable})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	return topology, normalized, cores, nil
}

func (s *SmtSiblingIsolator) plan(domain string, req Resource) ([]int, []int) {
	var sameDomain, freeCores []coreInfo
	for _, core := range s.cores {
		if !coreUsable(core) {
			continue
		}
		owner, owned, conflict := s.coreOwnership(core)
		if conflict {
			continue
		}
		switch {
		case owned && owner == domain:
			if s.coreFreeCPUs(core) > 0 {
				sameDomain = append(sameDomain, core)
			}
		case !owned:
			freeCores = append(freeCores, core)
		default:
			// A core owned by another domain is never a candidate. Free
			// siblings on it are reported as blocked capacity, not split off.
		}
	}

	selectedCPUs := make([]int, 0, min(req.Threads, maxTopologyCPUs))
	selectedCoreSet := make(map[int]struct{})
	choose := func(core coreInfo, wholeCore bool) bool {
		free := s.coreFreeCPUs(core)
		if free == 0 {
			return false
		}
		cpus := s.coreFreeCPUList(core)
		if wholeCore {
			selectedCPUs = append(selectedCPUs, cpus...)
		} else {
			need := req.Threads - len(selectedCPUs)
			if need > len(cpus) {
				need = len(cpus)
			}
			selectedCPUs = append(selectedCPUs, cpus[:need]...)
		}
		selectedCoreSet[core.ID] = struct{}{}
		return len(selectedCPUs) >= req.Threads
	}

	for _, core := range sameDomain {
		if choose(core, req.Exclusive) {
			break
		}
	}
	if len(selectedCPUs) < req.Threads {
		for _, core := range freeCores {
			if choose(core, req.Exclusive) {
				break
			}
		}
	}

	cores := make([]int, 0, len(selectedCoreSet))
	for coreID := range selectedCoreSet {
		cores = append(cores, coreID)
	}
	sort.Ints(cores)
	sort.Ints(selectedCPUs)
	return selectedCPUs, cores
}

func (s *SmtSiblingIsolator) coreFreeCPUList(core coreInfo) []int {
	out := make([]int, 0, len(core.CPUs))
	for _, cpuID := range core.CPUs {
		if _, owned := s.cpuOwners[cpuID]; !owned {
			out = append(out, cpuID)
		}
	}
	return out
}

func (s *SmtSiblingIsolator) coreFreeCPUs(core coreInfo) int {
	count := 0
	for _, cpuID := range core.CPUs {
		if _, owned := s.cpuOwners[cpuID]; !owned {
			count++
		}
	}
	return count
}

func (s *SmtSiblingIsolator) freeCapacity(domain string) (int, int) {
	usable, blocked := 0, 0
	for _, core := range s.cores {
		if !coreUsable(core) {
			continue
		}
		free := s.coreFreeCPUs(core)
		if free == 0 {
			continue
		}
		owner, owned, conflict := s.coreOwnership(core)
		if conflict || (owned && owner != domain) {
			blocked += free
		} else {
			usable += free
		}
	}
	return usable, blocked
}

func coreUsable(core coreInfo) bool {
	return core.usable
}

func (s *SmtSiblingIsolator) domainInfo(domain string) *domainInfo {
	if state, ok := s.domains[domain]; ok {
		return state
	}
	state := &domainInfo{}
	s.domains[domain] = state
	return state
}

func (s *SmtSiblingIsolator) noteFailure(domain, reason string) {
	if _, ok := s.domains[domain]; !ok && len(s.domains) >= maxDomains {
		return
	}
	s.domainInfo(domain).lastReason = reason
}

func (s *SmtSiblingIsolator) nextGeneration() (int64, bool) {
	start := s.nextGen
	if start <= 0 {
		start = 1
	}
	for attempts := 0; attempts <= maxAllocations && start > 0 && start < math.MaxInt64; attempts++ {
		gen := start
		start++
		if _, exists := s.allocations[allocationID(gen)]; !exists {
			s.nextGen = start
			return gen, true
		}
	}
	return 0, false
}

func allocationID(gen int64) string {
	return fmt.Sprintf("alloc-%016d", gen)
}

func (s *SmtSiblingIsolator) domainAllocationCountLocked(domain string) int {
	count := 0
	for _, alloc := range s.allocations {
		if alloc.Domain == domain {
			count++
		}
	}
	return count
}

func (s *SmtSiblingIsolator) coreOwnership(core coreInfo) (string, bool, bool) {
	owner := ""
	owned := false
	conflict := false
	for _, cpuID := range core.CPUs {
		cpuOwner, ok := s.cpuOwners[cpuID]
		if !ok {
			continue
		}
		if !owned {
			owner = cpuOwner
			owned = true
			continue
		}
		if cpuOwner != owner {
			conflict = true
		}
	}
	if coreOwner, ok := s.coreOwners[core.ID]; ok {
		if !owned {
			owner = coreOwner
			owned = true
		} else if coreOwner != owner {
			conflict = true
		}
	}
	return owner, owned, conflict
}

func (s *SmtSiblingIsolator) coreHasOwnedCPU(coreID int) bool {
	for _, cpuID := range s.coreCPUIDs(coreID) {
		if _, ok := s.cpuOwners[cpuID]; ok {
			return true
		}
	}
	return false
}

func (s *SmtSiblingIsolator) coreCPUIDs(coreID int) []int {
	for _, core := range s.cores {
		if core.ID == coreID {
			return core.CPUs
		}
	}
	return nil
}

func (s *SmtSiblingIsolator) coreByID(coreID int) (coreInfo, bool) {
	for _, core := range s.cores {
		if core.ID == coreID {
			return core, true
		}
	}
	return coreInfo{}, false
}

func (s *SmtSiblingIsolator) liveCoreOwner(coreID int) string {
	for _, cpuID := range s.coreCPUIDs(coreID) {
		if owner, ok := s.cpuOwners[cpuID]; ok {
			return owner
		}
	}
	return ""
}

func (s *SmtSiblingIsolator) repairOwnership() {
	cpuOwners := make(map[int]string)
	coreOwners := make(map[int]string)
	for id, alloc := range s.allocations {
		for _, cpuID := range alloc.CPUs {
			cpu, ok := s.cpuByID[cpuID]
			if !ok {
				delete(s.allocations, id)
				break
			}
			cpuOwners[cpuID] = alloc.Domain
			coreOwners[cpu.Core] = alloc.Domain
		}
	}
	s.cpuOwners = cpuOwners
	s.coreOwners = coreOwners
}

func validAllocationInput(alloc Allocation) bool {
	return strings.TrimSpace(alloc.ID) != "" &&
		strings.TrimSpace(alloc.Domain) != "" &&
		alloc.Threads > 0 &&
		len(alloc.CPUs) > 0 &&
		len(alloc.Cores) > 0 &&
		alloc.Gen > 0 &&
		alloc.Gen < math.MaxInt64
}

func copyAllocation(alloc Allocation) Allocation {
	alloc.CPUs = append([]int(nil), alloc.CPUs...)
	alloc.Cores = append([]int(nil), alloc.Cores...)
	return alloc
}

func copyCPUs(cpus []CPU) []CPU {
	return append([]CPU(nil), cpus...)
}

func copyCores(cores []Core) []Core {
	out := make([]Core, len(cores))
	for i, core := range cores {
		out[i] = Core{ID: core.ID, Online: core.Online, CPUs: append([]int(nil), core.CPUs...)}
	}
	return out
}

func uniqueInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON value")
	}
	return err
}
