// Package task approves and accounts host port bindings, keeps the reservation
// pool with its leases and manages the members of shared port groups.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	ErrBadPort        = errors.New("port is outside the valid range")
	ErrBadOwner       = errors.New("owner is required")
	ErrBadTTL         = errors.New("lease ttl must be positive")
	ErrPortNotAllowed = errors.New("owner may not reserve this port")
	ErrPortInUse      = errors.New("port is already reserved by another owner")
	ErrLeaseNotFound  = errors.New("unknown lease")
	ErrLeaseExpired   = errors.New("lease has expired")
	ErrLeaseLimit     = errors.New("lease pool limit reached")
	ErrOwnerLimit     = errors.New("per owner lease limit reached")
	ErrUnknownBinding = errors.New("unknown binding")
	ErrBadBinding     = errors.New("bind request is invalid")
	ErrBadGroup       = errors.New("group name is required")
	ErrGroupLimit     = errors.New("group member limit reached")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")

	errBindingInGroup = errors.New("binding already belongs to another group")
)

const snapshotVersion = 1

type snapshotState struct {
	Version      int                 `json:"version"`
	Leases       []Lease             `json:"leases"`
	Bindings     []Binding           `json:"bindings"`
	Groups       map[string][]string `json:"groups"`
	Graced       []string            `json:"graced,omitempty"`
	NextLeaseSeq int64               `json:"next_lease_seq"`
	NextBindSeq  int64               `json:"next_bind_seq"`
}

// PortBindWarden owns the lease table, the binding table and the group table.
type PortBindWarden struct {
	cfg Config
	now func() time.Time

	privileged map[string]struct{}
	external   map[int]struct{}

	mu sync.RWMutex

	leases          map[string]Lease
	leaseByPort     map[int]string
	bindings        map[string]Binding
	bindingsByLease map[string]map[string]struct{}
	bindingsByPort  map[int]map[string]struct{}
	groups          map[string]map[string]struct{}
	graced          map[string]struct{}

	nextLeaseSeq int64
	nextBindSeq  int64
}

// NewPortBindWarden builds a warden with the given caps.
func NewPortBindWarden(cfg Config) *PortBindWarden {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	w := &PortBindWarden{
		cfg:             cfg,
		now:             cfg.Now,
		privileged:      make(map[string]struct{}, len(cfg.PrivilegedOwners)),
		external:        make(map[int]struct{}, len(cfg.ExternallyBusy)),
		leases:          make(map[string]Lease),
		leaseByPort:     make(map[int]string),
		bindings:        make(map[string]Binding),
		bindingsByLease: make(map[string]map[string]struct{}),
		bindingsByPort:  make(map[int]map[string]struct{}),
		groups:          make(map[string]map[string]struct{}),
		graced:          make(map[string]struct{}),
		nextLeaseSeq:    1,
		nextBindSeq:     1,
	}
	for _, owner := range cfg.PrivilegedOwners {
		owner = strings.TrimSpace(owner)
		if owner != "" {
			w.privileged[owner] = struct{}{}
		}
	}
	for _, port := range cfg.ExternallyBusy {
		w.external[port] = struct{}{}
	}
	return w
}

// Reserve takes a lease on one port for one owner.
func (w *PortBindWarden) Reserve(port int, owner string, ttl time.Duration) (Lease, error) {
	if !validPort(port) {
		return Lease{}, ErrBadPort
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Lease{}, ErrBadOwner
	}
	if ttl <= 0 {
		return Lease{}, ErrBadTTL
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()

	if port <= PrivilegedPortMax && !w.isPrivilegedOwner(owner) {
		return Lease{}, ErrPortNotAllowed
	}

	if leaseID, ok := w.leaseByPort[port]; ok {
		current := w.leases[leaseID]
		if current.Owner != owner {
			return Lease{}, ErrPortInUse
		}
		current.ExpiresAt = now.Add(ttl).UnixNano()
		w.leases[leaseID] = current
		delete(w.graced, leaseID)
		return current, nil
	}

	if w.ownerLeaseCount(owner) >= w.cfg.MaxPerOwner {
		return Lease{}, ErrOwnerLimit
	}
	if len(w.leases) >= w.cfg.MaxLeases {
		return Lease{}, ErrLeaseLimit
	}

	seq := w.nextLeaseSeq
	w.nextLeaseSeq++
	lease := Lease{
		ID:        leaseIDFor(seq),
		Port:      port,
		Owner:     owner,
		ExpiresAt: now.Add(ttl).UnixNano(),
		Seq:       seq,
	}
	w.leases[lease.ID] = lease
	w.leaseByPort[port] = lease.ID
	return lease, nil
}

// Bind turns one lease into a live binding.
func (w *PortBindWarden) Bind(leaseID string, opts BindOpts) (Binding, error) {
	address := strings.TrimSpace(opts.Address)

	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()

	lease, ok := w.leases[leaseID]
	if !ok {
		return Binding{}, ErrLeaseNotFound
	}
	if now.UnixNano() >= lease.ExpiresAt {
		return Binding{}, ErrLeaseExpired
	}
	if address == "" {
		return Binding{}, ErrBadBinding
	}
	if (opts.Privileged || lease.Port <= PrivilegedPortMax) && !w.isPrivilegedOwner(lease.Owner) {
		return Binding{}, ErrPortNotAllowed
	}
	if _, busy := w.external[lease.Port]; busy {
		return Binding{}, ErrPortInUse
	}

	seq := w.nextBindSeq
	w.nextBindSeq++
	binding := Binding{
		ID:      bindingIDFor(seq),
		Port:    lease.Port,
		Owner:   lease.Owner,
		LeaseID: lease.ID,
		Address: address,
		Seq:     seq,
	}
	w.bindings[binding.ID] = binding
	addBindingRefByLease(w.bindingsByLease, lease.ID, binding.ID)
	addBindingRefByPort(w.bindingsByPort, lease.Port, binding.ID)
	return binding, nil
}

// JoinGroup adds one binding to a shared port group.
func (w *PortBindWarden) JoinGroup(group string, bindingID string) error {
	group = strings.TrimSpace(group)
	if group == "" {
		return ErrBadGroup
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	binding, ok := w.bindings[bindingID]
	if !ok {
		return ErrUnknownBinding
	}
	if binding.Group == group {
		return nil
	}
	if binding.Group != "" {
		return errBindingInGroup
	}
	members := w.groups[group]
	if len(members) >= w.cfg.MaxGroupMembers {
		return ErrGroupLimit
	}
	if members == nil {
		members = make(map[string]struct{})
		w.groups[group] = members
	}
	members[bindingID] = struct{}{}
	binding.Group = group
	w.bindings[bindingID] = binding
	return nil
}

// Release drops one binding and its group membership.
func (w *PortBindWarden) Release(bindingID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	binding, ok := w.bindings[bindingID]
	if !ok {
		return ErrUnknownBinding
	}
	w.removeBindingLocked(binding)
	return nil
}

// Explain reports the state of one port.
func (w *PortBindWarden) Explain(port int) (PortState, error) {
	if !validPort(port) {
		return PortState{}, ErrBadPort
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	state := PortState{
		Port:     port,
		Class:    portClass(port),
		Bindings: make([]Binding, 0),
	}
	if leaseID, ok := w.leaseByPort[port]; ok {
		lease := w.leases[leaseID]
		state.Lease = &lease
	}
	for bindingID := range w.bindingsByPort[port] {
		binding, ok := w.bindings[bindingID]
		if ok {
			state.Bindings = append(state.Bindings, binding)
		}
	}
	sort.Slice(state.Bindings, func(i, j int) bool {
		if state.Bindings[i].Seq != state.Bindings[j].Seq {
			return state.Bindings[i].Seq < state.Bindings[j].Seq
		}
		return state.Bindings[i].ID < state.Bindings[j].ID
	})
	for _, binding := range state.Bindings {
		if binding.Group != "" {
			state.Group = binding.Group
			break
		}
	}
	return state, nil
}

// Checkpoint sweeps expired leases and reports what it handled.
func (w *PortBindWarden) Checkpoint() (ReclaimReport, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()

	type expiredLease struct {
		id          string
		seq         int64
		hasBindings bool
	}
	expired := make([]expiredLease, 0)
	for id, lease := range w.leases {
		if now.UnixNano() >= lease.ExpiresAt {
			expired = append(expired, expiredLease{
				id:          id,
				seq:         lease.Seq,
				hasBindings: len(w.bindingsByLease[id]) > 0,
			})
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].seq != expired[j].seq {
			return expired[i].seq < expired[j].seq
		}
		return expired[i].id < expired[j].id
	})

	report := ReclaimReport{
		Reclaimed: make([]string, 0),
		Graced:    make([]string, 0),
	}
	for _, candidate := range expired {
		if candidate.hasBindings {
			if _, alreadyGraced := w.graced[candidate.id]; !alreadyGraced {
				w.graced[candidate.id] = struct{}{}
				report.Graced = append(report.Graced, candidate.id)
				continue
			}
		}
		w.reclaimLeaseLocked(candidate.id)
		report.Reclaimed = append(report.Reclaimed, candidate.id)
	}
	return report, nil
}

// Lease reads one lease record.
func (w *PortBindWarden) Lease(leaseID string) (Lease, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	lease, ok := w.leases[leaseID]
	return lease, ok
}

// Binding reads one binding record.
func (w *PortBindWarden) Binding(bindingID string) (Binding, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	binding, ok := w.bindings[bindingID]
	return binding, ok
}

// Snapshot serialises the leases, the bindings and the group table.
func (w *PortBindWarden) Snapshot() ([]byte, error) {
	w.mu.RLock()
	state := snapshotState{
		Version:      snapshotVersion,
		Leases:       make([]Lease, 0, len(w.leases)),
		Bindings:     make([]Binding, 0, len(w.bindings)),
		Groups:       make(map[string][]string, len(w.groups)),
		Graced:       make([]string, 0, len(w.graced)),
		NextLeaseSeq: w.nextLeaseSeq,
		NextBindSeq:  w.nextBindSeq,
	}
	for _, lease := range w.leases {
		state.Leases = append(state.Leases, lease)
	}
	for _, binding := range w.bindings {
		state.Bindings = append(state.Bindings, binding)
	}
	for group, memberSet := range w.groups {
		members := make([]string, 0, len(memberSet))
		for bindingID := range memberSet {
			members = append(members, bindingID)
		}
		sort.Strings(members)
		state.Groups[group] = members
	}
	for leaseID := range w.graced {
		state.Graced = append(state.Graced, leaseID)
	}
	w.mu.RUnlock()

	sort.Slice(state.Leases, func(i, j int) bool {
		if state.Leases[i].Seq != state.Leases[j].Seq {
			return state.Leases[i].Seq < state.Leases[j].Seq
		}
		return state.Leases[i].ID < state.Leases[j].ID
	})
	sort.Slice(state.Bindings, func(i, j int) bool {
		if state.Bindings[i].Seq != state.Bindings[j].Seq {
			return state.Bindings[i].Seq < state.Bindings[j].Seq
		}
		return state.Bindings[i].ID < state.Bindings[j].ID
	})
	leaseSeqs := make(map[string]int64, len(state.Leases))
	for _, lease := range state.Leases {
		leaseSeqs[lease.ID] = lease.Seq
	}
	sort.Slice(state.Graced, func(i, j int) bool {
		left, leftOK := leaseSeqs[state.Graced[i]]
		right, rightOK := leaseSeqs[state.Graced[j]]
		if leftOK && rightOK && left != right {
			return left < right
		}
		return state.Graced[i] < state.Graced[j]
	})

	return json.Marshal(state)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (w *PortBindWarden) Restore(data []byte) error {
	state, ok := decodeSnapshot(data)
	if !ok {
		return ErrBadSnapshot
	}
	restored, ok := w.buildRestoredState(state)
	if !ok {
		return ErrBadSnapshot
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.leases = restored.leases
	w.leaseByPort = restored.leaseByPort
	w.bindings = restored.bindings
	w.bindingsByLease = restored.bindingsByLease
	w.bindingsByPort = restored.bindingsByPort
	w.groups = restored.groups
	w.graced = restored.graced
	w.nextLeaseSeq = state.NextLeaseSeq
	w.nextBindSeq = state.NextBindSeq
	return nil
}

type restoredState struct {
	leases          map[string]Lease
	leaseByPort     map[int]string
	bindings        map[string]Binding
	bindingsByLease map[string]map[string]struct{}
	bindingsByPort  map[int]map[string]struct{}
	groups          map[string]map[string]struct{}
	graced          map[string]struct{}
}

func (w *PortBindWarden) buildRestoredState(state snapshotState) (restoredState, bool) {
	if state.Version != snapshotVersion || state.NextLeaseSeq <= 0 || state.NextBindSeq <= 0 {
		return restoredState{}, false
	}
	if len(state.Leases) > w.cfg.MaxLeases {
		return restoredState{}, false
	}

	result := restoredState{
		leases:          make(map[string]Lease, len(state.Leases)),
		leaseByPort:     make(map[int]string, len(state.Leases)),
		bindings:        make(map[string]Binding, len(state.Bindings)),
		bindingsByLease: make(map[string]map[string]struct{}),
		bindingsByPort:  make(map[int]map[string]struct{}),
		groups:          make(map[string]map[string]struct{}, len(state.Groups)),
		graced:          make(map[string]struct{}, len(state.Graced)),
	}

	leaseSeqs := make(map[int64]struct{}, len(state.Leases))
	ownerCounts := make(map[string]int, len(state.Leases))
	for _, lease := range state.Leases {
		if lease.ID == "" || !validPort(lease.Port) || strings.TrimSpace(lease.Owner) == "" {
			return restoredState{}, false
		}
		if lease.Seq <= 0 || lease.Seq >= state.NextLeaseSeq {
			return restoredState{}, false
		}
		if _, duplicate := leaseSeqs[lease.Seq]; duplicate {
			return restoredState{}, false
		}
		if _, duplicate := result.leases[lease.ID]; duplicate {
			return restoredState{}, false
		}
		if _, duplicate := result.leaseByPort[lease.Port]; duplicate {
			return restoredState{}, false
		}
		if lease.Port <= PrivilegedPortMax && !w.isPrivilegedOwner(lease.Owner) {
			return restoredState{}, false
		}
		leaseSeqs[lease.Seq] = struct{}{}
		ownerCounts[lease.Owner]++
		if ownerCounts[lease.Owner] > w.cfg.MaxPerOwner {
			return restoredState{}, false
		}
		result.leases[lease.ID] = lease
		result.leaseByPort[lease.Port] = lease.ID
	}

	bindingSeqs := make(map[int64]struct{}, len(state.Bindings))
	membershipByBinding := make(map[string]string, len(state.Bindings))
	for _, binding := range state.Bindings {
		if binding.ID == "" || binding.LeaseID == "" || strings.TrimSpace(binding.Address) == "" {
			return restoredState{}, false
		}
		if binding.Seq <= 0 || binding.Seq >= state.NextBindSeq {
			return restoredState{}, false
		}
		if _, duplicate := bindingSeqs[binding.Seq]; duplicate {
			return restoredState{}, false
		}
		if _, duplicate := result.bindings[binding.ID]; duplicate {
			return restoredState{}, false
		}
		lease, found := result.leases[binding.LeaseID]
		if !found || lease.Port != binding.Port || lease.Owner != binding.Owner {
			return restoredState{}, false
		}
		if binding.Group != "" && strings.TrimSpace(binding.Group) == "" {
			return restoredState{}, false
		}
		bindingSeqs[binding.Seq] = struct{}{}
		result.bindings[binding.ID] = binding
		addBindingRefByLease(result.bindingsByLease, binding.LeaseID, binding.ID)
		addBindingRefByPort(result.bindingsByPort, binding.Port, binding.ID)
	}

	for group, members := range state.Groups {
		group = strings.TrimSpace(group)
		if group == "" || len(members) == 0 || len(members) > w.cfg.MaxGroupMembers {
			return restoredState{}, false
		}
		if _, duplicate := result.groups[group]; duplicate {
			return restoredState{}, false
		}
		memberSet := make(map[string]struct{}, len(members))
		for _, bindingID := range members {
			if _, duplicate := memberSet[bindingID]; duplicate {
				return restoredState{}, false
			}
			binding, found := result.bindings[bindingID]
			if !found || binding.Group != group {
				return restoredState{}, false
			}
			if _, alreadyGrouped := membershipByBinding[bindingID]; alreadyGrouped {
				return restoredState{}, false
			}
			memberSet[bindingID] = struct{}{}
			membershipByBinding[bindingID] = group
		}
		result.groups[group] = memberSet
	}
	for _, binding := range result.bindings {
		if binding.Group == "" {
			if _, grouped := membershipByBinding[binding.ID]; grouped {
				return restoredState{}, false
			}
			continue
		}
		group, found := membershipByBinding[binding.ID]
		if !found || group != binding.Group {
			return restoredState{}, false
		}
	}

	for _, leaseID := range state.Graced {
		if leaseID == "" {
			return restoredState{}, false
		}
		if _, duplicate := result.graced[leaseID]; duplicate {
			return restoredState{}, false
		}
		if _, found := result.leases[leaseID]; !found {
			return restoredState{}, false
		}
		if len(result.bindingsByLease[leaseID]) == 0 {
			return restoredState{}, false
		}
		result.graced[leaseID] = struct{}{}
	}
	return result, true
}

func decodeSnapshot(data []byte) (snapshotState, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return snapshotState{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state snapshotState
	if err := decoder.Decode(&state); err != nil {
		return snapshotState{}, false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return snapshotState{}, false
	}
	return state, true
}

func (w *PortBindWarden) isPrivilegedOwner(owner string) bool {
	_, ok := w.privileged[owner]
	return ok
}

func (w *PortBindWarden) ownerLeaseCount(owner string) int {
	count := 0
	for _, lease := range w.leases {
		if lease.Owner == owner {
			count++
		}
	}
	return count
}

func (w *PortBindWarden) removeBindingLocked(binding Binding) {
	delete(w.bindings, binding.ID)
	removeBindingRefByLease(w.bindingsByLease, binding.LeaseID, binding.ID)
	removeBindingRefByPort(w.bindingsByPort, binding.Port, binding.ID)
	if binding.Group != "" {
		members := w.groups[binding.Group]
		delete(members, binding.ID)
		if len(members) == 0 {
			delete(w.groups, binding.Group)
		}
	}
	if len(w.bindingsByLease[binding.LeaseID]) == 0 {
		delete(w.graced, binding.LeaseID)
	}
}

func (w *PortBindWarden) reclaimLeaseLocked(leaseID string) {
	memberIDs := make([]string, 0, len(w.bindingsByLease[leaseID]))
	for bindingID := range w.bindingsByLease[leaseID] {
		memberIDs = append(memberIDs, bindingID)
	}
	for _, bindingID := range memberIDs {
		if binding, ok := w.bindings[bindingID]; ok {
			w.removeBindingLocked(binding)
		}
	}
	if lease, ok := w.leases[leaseID]; ok {
		delete(w.leaseByPort, lease.Port)
		delete(w.leases, leaseID)
	}
	delete(w.bindingsByLease, leaseID)
	delete(w.graced, leaseID)
}

func addBindingRefByLease(index map[string]map[string]struct{}, key string, bindingID string) {
	bindings := index[key]
	if bindings == nil {
		bindings = make(map[string]struct{})
		index[key] = bindings
	}
	bindings[bindingID] = struct{}{}
}

func addBindingRefByPort(index map[int]map[string]struct{}, key int, bindingID string) {
	bindings := index[key]
	if bindings == nil {
		bindings = make(map[string]struct{})
		index[key] = bindings
	}
	bindings[bindingID] = struct{}{}
}

func removeBindingRefByLease(index map[string]map[string]struct{}, key string, bindingID string) {
	bindings := index[key]
	delete(bindings, bindingID)
	if len(bindings) == 0 {
		delete(index, key)
	}
}

func removeBindingRefByPort(index map[int]map[string]struct{}, key int, bindingID string) {
	bindings := index[key]
	delete(bindings, bindingID)
	if len(bindings) == 0 {
		delete(index, key)
	}
}

func validPort(port int) bool {
	return port >= 1 && port <= MaxPort
}

func portClass(port int) string {
	switch {
	case port <= PrivilegedPortMax:
		return ClassPrivileged
	case port <= ReservedPortMax:
		return ClassReserved
	default:
		return ClassDynamic
	}
}

func leaseIDFor(seq int64) string {
	return "lease-" + strconv.FormatInt(seq, 10)
}

func bindingIDFor(seq int64) string {
	return "bind-" + strconv.FormatInt(seq, 10)
}
