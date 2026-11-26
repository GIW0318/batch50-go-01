// Package task inspects the memory mappings of managed processes, grades the
// ones that are writable and executable at the same time, and hands out JIT
// exemption leases for the mappings that are expected to behave that way.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

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

type regionState struct {
	Region       Region
	Key          string
	CoverageSize uint64
}

type processState struct {
	Handle           Handle
	Profile          Profile
	Maps             []Mapping
	Regions          map[string]*regionState
	RegionKeys       map[string]string
	CurrentRegionIDs []string
	NextRegionSeq    uint64
	GraceSeen        map[string]bool
	LeaseIDs         []string
}

type leaseRecord struct {
	Lease    Lease
	Process  Handle
	Released bool
}

// WxRegionWarden owns the process table, the lease table and the region table.
type WxRegionWarden struct {
	mu sync.Mutex

	limits       Limits
	nextSeq      uint64
	nextLeaseSeq uint64
	processes    map[Handle]*processState
	pidIndex     map[int]Handle
	leases       map[string]*leaseRecord
	leaseOrder   []string
}

// NewWxRegionWarden builds a warden with the given caps.
func NewWxRegionWarden(limits Limits) *WxRegionWarden {
	return &WxRegionWarden{
		limits:    limits,
		processes: make(map[Handle]*processState),
		pidIndex:  make(map[int]Handle),
		leases:    make(map[string]*leaseRecord),
	}
}

// Register adds one managed process.
func (w *WxRegionWarden) Register(pid int, profile Profile) (Handle, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if profile.Tenant == "" || profile.Name == "" {
		return Handle{}, ErrBlankProfile
	}
	if _, exists := w.pidIndex[pid]; exists {
		return Handle{}, ErrProcessExists
	}
	if w.limits.MaxProcessesPerTenant > 0 {
		count := 0
		for _, p := range w.processes {
			if p.Profile.Tenant == profile.Tenant {
				count++
			}
		}
		if count >= w.limits.MaxProcessesPerTenant {
			return Handle{}, ErrTenantProcessLimit
		}
	}

	w.nextSeq++
	h := Handle{PID: pid, Tenant: profile.Tenant, Seq: w.nextSeq}
	p := &processState{
		Handle:           h,
		Profile:          profile,
		Regions:          make(map[string]*regionState),
		RegionKeys:       make(map[string]string),
		GraceSeen:        make(map[string]bool),
		LeaseIDs:         make([]string, 0),
		CurrentRegionIDs: make([]string, 0),
	}
	w.processes[h] = p
	w.pidIndex[pid] = h
	return h, nil
}

// Unregister removes one managed process and its leases.
func (w *WxRegionWarden) Unregister(h Handle) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return ErrUnknownHandle
	}
	for _, id := range p.LeaseIDs {
		delete(w.leases, id)
		for i, leaseID := range w.leaseOrder {
			if leaseID == id {
				w.leaseOrder = append(w.leaseOrder[:i], w.leaseOrder[i+1:]...)
				break
			}
		}
	}
	delete(w.processes, p.Handle)
	delete(w.pidIndex, p.Handle.PID)
	return nil
}

// Feed replaces the mapping table of one process.
func (w *WxRegionWarden) Feed(h Handle, maps []Mapping) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return 0, ErrUnknownHandle
	}
	if w.limits.MaxRegionsPerProcess > 0 && len(maps) > w.limits.MaxRegionsPerProcess {
		return 0, ErrBadMapping
	}
	for _, m := range maps {
		if m.Start >= m.End {
			return 0, ErrBadMapping
		}
	}

	copied := append([]Mapping(nil), maps...)
	p.Maps = copied
	return len(copied), nil
}

// Scan grades the current mapping table of one process and stores the result.
func (w *WxRegionWarden) Scan(h Handle) (Report, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return Report{}, ErrUnknownHandle
	}

	now := w.nowLocked()

	groups := make(map[string]*Mapping)
	coverage := make(map[string]uint64)
	order := make([]string, 0, len(p.Maps))
	for _, m := range p.Maps {
		key := mappingKey(m)
		if span := m.End - m.Start; span > coverage[key] {
			coverage[key] = span
		}
		group, ok := groups[key]
		if !ok {
			cp := m
			groups[key] = &cp
			order = append(order, key)
			continue
		}
		if m.Start < group.Start {
			group.Start = m.Start
		}
		if m.End > group.End {
			group.End = m.End
		}
		group.Writable = group.Writable || m.Writable
		group.Executable = group.Executable || m.Executable
		group.Anonymous = group.Anonymous || m.Anonymous
		if group.Backing == "" {
			group.Backing = m.Backing
		}
		if group.Path == "" {
			group.Path = m.Path
		}
	}

	report := Report{
		PID:      p.Handle.PID,
		Regions:  make([]Region, 0),
		Verdicts: make([]Verdict, 0),
	}
	currentIDs := make([]string, 0)

	for _, key := range order {
		m := groups[key]
		if !m.Writable || !m.Executable {
			continue
		}
		coverageSize := coverage[key]
		protection, protectingLease := w.leaseProtectionLocked(p, now, coverageSize)

		regionID, ok := p.RegionKeys[key]
		if !ok {
			for id, prior := range p.Regions {
				if prior.Region.Start == m.Start && prior.Region.End == m.End && !containsString(currentIDs, id) {
					regionID = id
					ok = true
					break
				}
			}
		}
		if !ok {
			p.NextRegionSeq++
			regionID = fmt.Sprintf("r-%d-%d", p.Handle.Seq, p.NextRegionSeq)
		}
		p.RegionKeys[key] = regionID

		desiredLevel := LevelObserve
		desiredReason := ""
		jit := false
		if m.Anonymous {
			switch protection {
			case LeaseActive:
				desiredReason = ReasonJitLeaseActive
				jit = true
			case LeaseGrace:
				desiredReason = ReasonLeaseGrace
				jit = true
			default:
				desiredLevel = LevelAlert
				desiredReason = ReasonAnonWX
			}
		}

		state := p.Regions[regionID]
		if state == nil {
			if desiredReason == ReasonLeaseGrace {
				p.GraceSeen[protectingLease] = true
			}
			state = &regionState{
				Region: Region{
					ID:         regionID,
					PID:        p.Handle.PID,
					Start:      m.Start,
					End:        m.End,
					Writable:   m.Writable,
					Executable: m.Executable,
					Anonymous:  m.Anonymous,
					Backing:    m.Backing,
					Jit:        jit,
					Level:      desiredLevel,
					Reason:     desiredReason,
				},
				Key:          key,
				CoverageSize: coverageSize,
			}
			p.Regions[regionID] = state
		} else {
			level := state.Region.Level
			reason := state.Region.Reason

			if desiredLevel > level {
				level = desiredLevel
				reason = desiredReason
			} else if desiredLevel == level && level == LevelObserve {
				if desiredReason == ReasonLeaseGrace {
					if !p.GraceSeen[protectingLease] && reason != ReasonLeaseGrace {
						reason = ReasonLeaseGrace
						p.GraceSeen[protectingLease] = true
					}
				} else if desiredReason != "" {
					reason = desiredReason
				}
			}

			state.Region.Start = m.Start
			state.Region.End = m.End
			state.Region.Writable = m.Writable
			state.Region.Executable = m.Executable
			state.Region.Anonymous = m.Anonymous
			state.Region.Backing = m.Backing
			state.Region.Jit = jit
			state.Region.Level = level
			state.Region.Reason = reason
			state.CoverageSize = coverageSize
		}

		currentIDs = append(currentIDs, regionID)
		report.Regions = append(report.Regions, state.Region)
		report.Verdicts = append(report.Verdicts, Verdict{
			RegionID: regionID,
			Action:   actionForLevel(state.Region.Level),
			Level:    state.Region.Level,
			Reason:   state.Region.Reason,
		})
		switch state.Region.Level {
		case LevelObserve:
			report.Observed++
		case LevelAlert:
			report.Alerts++
		case LevelRestrict:
			report.Restricted++
		}
	}
	p.CurrentRegionIDs = currentIDs
	return report, nil
}

// Classify grades a single mapping outside of any process context.
func (w *WxRegionWarden) Classify(m Mapping) (Region, error) {
	if m.Start >= m.End {
		return Region{}, ErrBadMapping
	}

	r := Region{
		ID:         fmt.Sprintf("m-%x-%x", m.Start, m.End),
		Start:      m.Start,
		End:        m.End,
		Writable:   m.Writable,
		Executable: m.Executable,
		Anonymous:  m.Anonymous,
		Backing:    m.Backing,
		Level:      LevelObserve,
	}
	if m.Writable && m.Executable && m.Anonymous {
		r.Level = LevelAlert
		r.Reason = ReasonAnonWX
	}
	return r, nil
}

// GrantJitLease opens an exemption of the given size for the given duration.
func (w *WxRegionWarden) GrantJitLease(h Handle, size uint64, ttl time.Duration) (Lease, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return Lease{}, ErrUnknownHandle
	}
	if !p.Profile.AllowJit {
		return Lease{}, fmt.Errorf("%w: %s", ErrJitNotAllowed, ReasonJitDenied)
	}
	if w.limits.MaxLeaseBytes > 0 && size > w.limits.MaxLeaseBytes {
		return Lease{}, ErrLeaseTooLarge
	}
	if w.limits.MaxLeasesPerProcess > 0 && len(p.LeaseIDs) >= w.limits.MaxLeasesPerProcess {
		return Lease{}, ErrLeaseLimit
	}

	var id string
	for {
		w.nextLeaseSeq++
		id = fmt.Sprintf("lease-%d", w.nextLeaseSeq)
		if _, exists := w.leases[id]; !exists {
			break
		}
	}

	now := w.nowLocked()
	rec := &leaseRecord{
		Lease: Lease{
			ID:        id,
			PID:       p.Handle.PID,
			Size:      size,
			ExpiresAt: now.Add(ttl),
			State:     LeaseActive,
		},
		Process: p.Handle,
	}
	w.leases[id] = rec
	w.leaseOrder = append(w.leaseOrder, id)
	p.LeaseIDs = append(p.LeaseIDs, id)
	return w.leaseViewLocked(rec, now), nil
}

// ReleaseLease closes one lease.
func (w *WxRegionWarden) ReleaseLease(leaseID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	rec := w.leases[leaseID]
	if rec == nil {
		return ErrUnknownLease
	}
	if rec.Released {
		return ErrLeaseReleased
	}
	rec.Released = true
	rec.Lease.State = LeaseRevoked
	return nil
}

// LeaseState reads one lease and refreshes its state against the clock.
func (w *WxRegionWarden) LeaseState(leaseID string) (Lease, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	rec := w.leases[leaseID]
	if rec == nil {
		return Lease{}, ErrUnknownLease
	}
	return w.leaseViewLocked(rec, w.nowLocked()), nil
}

// Disposition reads the stored action and reason of one region.
func (w *WxRegionWarden) Disposition(h Handle, regionID string) (Action string, reason string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return "", "", ErrUnknownHandle
	}
	if state := p.Regions[regionID]; state != nil {
		return actionForLevel(state.Region.Level), state.Region.Reason, nil
	}
	if w.regionOwnerLocked(regionID) != nil {
		return "", ReasonRegionNotOwned, nil
	}
	return "", "", ErrUnknownRegion
}

// Restrict lowers the permissions of one region, or explains why it refused.
func (w *WxRegionWarden) Restrict(h Handle, regionID string) (Action string, reason string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return "", "", ErrUnknownHandle
	}
	state := p.Regions[regionID]
	if state == nil {
		if w.regionOwnerLocked(regionID) != nil {
			return "", ReasonRegionNotOwned, nil
		}
		return "", "", ErrUnknownRegion
	}

	now := w.nowLocked()
	regionSize := state.CoverageSize
	if regionSize == 0 {
		regionSize = state.Region.End - state.Region.Start
	}
	protection, protectingLease := w.leaseProtectionLocked(p, now, regionSize)
	switch protection {
	case LeaseActive:
		return actionForLevel(state.Region.Level), ReasonRestrictDenied, nil
	case LeaseGrace:
		if !p.GraceSeen[protectingLease] {
			p.GraceSeen[protectingLease] = true
			if state.Region.Level == LevelObserve {
				state.Region.Reason = ReasonLeaseGrace
			}
		}
		return actionForLevel(state.Region.Level), ReasonLeaseGrace, nil
	}

	state.Region.Level = LevelRestrict
	state.Region.Reason = ReasonRestrictApplied
	return ActionRestrict, ReasonRestrictApplied, nil
}

// Regions returns the graded regions of one process.
func (w *WxRegionWarden) Regions(h Handle) []Region {
	w.mu.Lock()
	defer w.mu.Unlock()

	p := w.findProcessLocked(h)
	if p == nil {
		return nil
	}
	regions := make([]Region, 0, len(p.CurrentRegionIDs))
	for _, id := range p.CurrentRegionIDs {
		if state := p.Regions[id]; state != nil {
			regions = append(regions, state.Region)
		}
	}
	return regions
}

// Summary aggregates the live state of one tenant.
func (w *WxRegionWarden) Summary(tenant string) Summary {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.nowLocked()
	summary := Summary{Tenant: tenant}
	for _, p := range w.processes {
		if p.Profile.Tenant != tenant {
			continue
		}
		summary.Processes++
		for _, id := range p.LeaseIDs {
			rec := w.leases[id]
			if rec != nil && !rec.Released && now.Before(rec.Lease.ExpiresAt) {
				summary.Leases++
			}
		}
		for _, id := range p.CurrentRegionIDs {
			if state := p.Regions[id]; state != nil && state.Region.Level == LevelRestrict {
				summary.Restricted++
			}
		}
	}
	return summary
}

type storedRegion struct {
	Key          string `json:"key"`
	Region       Region `json:"region"`
	CoverageSize uint64 `json:"coverage_size"`
}

type storedProcess struct {
	Handle           Handle                  `json:"handle"`
	Profile          Profile                 `json:"profile"`
	Maps             []Mapping               `json:"maps"`
	Regions          map[string]storedRegion `json:"regions"`
	RegionKeys       map[string]string       `json:"region_keys"`
	CurrentRegionIDs []string                `json:"current_region_ids"`
	NextRegionSeq    uint64                  `json:"next_region_seq"`
	GraceSeen        map[string]bool         `json:"grace_seen"`
	LeaseIDs         []string                `json:"lease_ids"`
}

type storedLease struct {
	Lease    Lease  `json:"lease"`
	Process  Handle `json:"process"`
	Released bool   `json:"released"`
}

type snapshotPayload struct {
	Version      int             `json:"version"`
	NextSeq      uint64          `json:"next_seq"`
	NextLeaseSeq uint64          `json:"next_lease_seq"`
	Processes    []storedProcess `json:"processes"`
	Leases       []storedLease   `json:"leases"`
}

// Snapshot serialises processes, leases and graded regions.
func (w *WxRegionWarden) Snapshot() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.nowLocked()
	payload := snapshotPayload{
		Version:      1,
		NextSeq:      w.nextSeq,
		NextLeaseSeq: w.nextLeaseSeq,
		Processes:    make([]storedProcess, 0, len(w.processes)),
		Leases:       make([]storedLease, 0, len(w.leases)),
	}

	for _, p := range w.processes {
		sp := storedProcess{
			Handle:           p.Handle,
			Profile:          p.Profile,
			Maps:             append([]Mapping(nil), p.Maps...),
			Regions:          make(map[string]storedRegion, len(p.Regions)),
			RegionKeys:       cloneStringMap(p.RegionKeys),
			CurrentRegionIDs: append([]string(nil), p.CurrentRegionIDs...),
			NextRegionSeq:    p.NextRegionSeq,
			GraceSeen:        cloneBoolMap(p.GraceSeen),
			LeaseIDs:         append([]string(nil), p.LeaseIDs...),
		}
		for id, state := range p.Regions {
			sp.Regions[id] = storedRegion{Key: state.Key, Region: state.Region, CoverageSize: state.CoverageSize}
		}
		payload.Processes = append(payload.Processes, sp)
	}
	for _, id := range w.leaseOrder {
		rec := w.leases[id]
		if rec == nil {
			continue
		}
		lease := w.leaseViewLocked(rec, now)
		payload.Leases = append(payload.Leases, storedLease{
			Lease:    lease,
			Process:  rec.Process,
			Released: rec.Released,
		})
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (w *WxRegionWarden) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	restored, err := w.prepareRestore(payload)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextSeq = restored.nextSeq
	w.nextLeaseSeq = restored.nextLeaseSeq
	w.processes = restored.processes
	w.pidIndex = restored.pidIndex
	w.leases = restored.leases
	w.leaseOrder = restored.leaseOrder
	return nil
}

type restoredState struct {
	nextSeq      uint64
	nextLeaseSeq uint64
	processes    map[Handle]*processState
	pidIndex     map[int]Handle
	leases       map[string]*leaseRecord
	leaseOrder   []string
}

func (w *WxRegionWarden) prepareRestore(payload snapshotPayload) (*restoredState, error) {
	if payload.Version != 1 {
		return nil, fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}

	rs := &restoredState{
		nextSeq:      payload.NextSeq,
		nextLeaseSeq: payload.NextLeaseSeq,
		processes:    make(map[Handle]*processState, len(payload.Processes)),
		pidIndex:     make(map[int]Handle, len(payload.Processes)),
		leases:       make(map[string]*leaseRecord, len(payload.Leases)),
		leaseOrder:   make([]string, 0, len(payload.Leases)),
	}

	tenantCounts := make(map[string]int)
	for _, sp := range payload.Processes {
		if sp.Profile.Tenant == "" || sp.Profile.Name == "" || sp.Handle.Seq == 0 {
			return nil, fmt.Errorf("%w: invalid process identity", ErrBadSnapshot)
		}
		if sp.Handle.Tenant != sp.Profile.Tenant {
			return nil, fmt.Errorf("%w: process tenant mismatch", ErrBadSnapshot)
		}
		if _, exists := rs.processes[sp.Handle]; exists {
			return nil, fmt.Errorf("%w: duplicate process handle", ErrBadSnapshot)
		}
		if _, exists := rs.pidIndex[sp.Handle.PID]; exists {
			return nil, fmt.Errorf("%w: duplicate process pid", ErrBadSnapshot)
		}
		if w.limits.MaxRegionsPerProcess > 0 && len(sp.Maps) > w.limits.MaxRegionsPerProcess {
			return nil, fmt.Errorf("%w: too many mappings", ErrBadSnapshot)
		}
		for _, m := range sp.Maps {
			if m.Start >= m.End {
				return nil, fmt.Errorf("%w: invalid mapping", ErrBadSnapshot)
			}
		}
		tenantCounts[sp.Profile.Tenant]++
		if w.limits.MaxProcessesPerTenant > 0 && tenantCounts[sp.Profile.Tenant] > w.limits.MaxProcessesPerTenant {
			return nil, fmt.Errorf("%w: tenant process limit exceeded", ErrBadSnapshot)
		}
		if sp.Handle.Seq > rs.nextSeq {
			return nil, fmt.Errorf("%w: next process sequence is stale", ErrBadSnapshot)
		}
		if sp.NextRegionSeq == 0 && len(sp.RegionKeys) > 0 {
			return nil, fmt.Errorf("%w: invalid region sequence", ErrBadSnapshot)
		}

		p := &processState{
			Handle:           sp.Handle,
			Profile:          sp.Profile,
			Maps:             append([]Mapping(nil), sp.Maps...),
			Regions:          make(map[string]*regionState, len(sp.Regions)),
			RegionKeys:       cloneStringMap(sp.RegionKeys),
			CurrentRegionIDs: append([]string(nil), sp.CurrentRegionIDs...),
			NextRegionSeq:    sp.NextRegionSeq,
			GraceSeen:        cloneBoolMap(sp.GraceSeen),
			LeaseIDs:         append([]string(nil), sp.LeaseIDs...),
		}

		for id, sr := range sp.Regions {
			if id == "" || sr.Key == "" || sr.Region.ID != id {
				return nil, fmt.Errorf("%w: invalid region identity", ErrBadSnapshot)
			}
			if sr.Region.PID != sp.Handle.PID || sr.Region.Start >= sr.Region.End {
				return nil, fmt.Errorf("%w: invalid region", ErrBadSnapshot)
			}
			if sr.Region.Level < LevelObserve || sr.Region.Level > LevelRestrict {
				return nil, fmt.Errorf("%w: invalid region level", ErrBadSnapshot)
			}
			if sr.CoverageSize > sr.Region.End-sr.Region.Start {
				return nil, fmt.Errorf("%w: invalid region coverage", ErrBadSnapshot)
			}
			p.Regions[id] = &regionState{Region: sr.Region, Key: sr.Key, CoverageSize: sr.CoverageSize}
		}
		for _, id := range p.RegionKeys {
			state := p.Regions[id]
			if state == nil {
				return nil, fmt.Errorf("%w: inconsistent region index", ErrBadSnapshot)
			}
		}
		for _, id := range p.CurrentRegionIDs {
			if p.Regions[id] == nil {
				return nil, fmt.Errorf("%w: missing current region", ErrBadSnapshot)
			}
		}

		rs.processes[sp.Handle] = p
		rs.pidIndex[sp.Handle.PID] = sp.Handle
	}

	processLeaseCount := make(map[Handle]int)
	for _, sl := range payload.Leases {
		if sl.Lease.ID == "" {
			return nil, fmt.Errorf("%w: invalid lease identity", ErrBadSnapshot)
		}
		if _, exists := rs.leases[sl.Lease.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate lease", ErrBadSnapshot)
		}
		p := rs.processes[sl.Process]
		if p == nil || sl.Lease.PID != p.Handle.PID {
			return nil, fmt.Errorf("%w: lease process mismatch", ErrBadSnapshot)
		}
		if w.limits.MaxLeaseBytes > 0 && sl.Lease.Size > w.limits.MaxLeaseBytes {
			return nil, fmt.Errorf("%w: lease too large", ErrBadSnapshot)
		}
		if sl.Lease.State != "" && sl.Lease.State != LeaseActive &&
			sl.Lease.State != LeaseGrace && sl.Lease.State != LeaseExpired &&
			sl.Lease.State != LeaseRevoked {
			return nil, fmt.Errorf("%w: invalid lease state", ErrBadSnapshot)
		}
		processLeaseCount[sl.Process]++
		if w.limits.MaxLeasesPerProcess > 0 && processLeaseCount[sl.Process] > w.limits.MaxLeasesPerProcess {
			return nil, fmt.Errorf("%w: too many leases", ErrBadSnapshot)
		}
		if !containsString(p.LeaseIDs, sl.Lease.ID) {
			return nil, fmt.Errorf("%w: unindexed lease", ErrBadSnapshot)
		}
		rs.leases[sl.Lease.ID] = &leaseRecord{
			Lease:    sl.Lease,
			Process:  sl.Process,
			Released: sl.Released,
		}
		rs.leaseOrder = append(rs.leaseOrder, sl.Lease.ID)
	}
	for _, p := range rs.processes {
		if len(p.LeaseIDs) != processLeaseCount[p.Handle] {
			return nil, fmt.Errorf("%w: lease index mismatch", ErrBadSnapshot)
		}
		for _, id := range p.LeaseIDs {
			if rs.leases[id] == nil {
				return nil, fmt.Errorf("%w: missing lease", ErrBadSnapshot)
			}
		}
	}

	return rs, nil
}

func (w *WxRegionWarden) findProcessLocked(h Handle) *processState {
	if h.Seq != 0 {
		return w.processes[h]
	}
	pidHandle, ok := w.pidIndex[h.PID]
	if !ok || pidHandle.Tenant != h.Tenant {
		return nil
	}
	return w.processes[pidHandle]
}

func (w *WxRegionWarden) regionOwnerLocked(regionID string) *regionState {
	for _, p := range w.processes {
		if state := p.Regions[regionID]; state != nil {
			return state
		}
	}
	return nil
}

func (w *WxRegionWarden) nowLocked() time.Time {
	if w.limits.Now != nil {
		return w.limits.Now()
	}
	return time.Now()
}

func (w *WxRegionWarden) leaseProtectionLocked(p *processState, now time.Time, regionSize uint64) (string, string) {
	var activeTotal uint64
	var activeLease string
	var graceTotal uint64
	var graceLease string
	for _, id := range p.LeaseIDs {
		rec := w.leases[id]
		if rec == nil || rec.Released {
			continue
		}
		if now.Before(rec.Lease.ExpiresAt) {
			activeTotal = saturatingAdd(activeTotal, rec.Lease.Size)
			if activeLease == "" {
				activeLease = id
			}
		} else if w.limits.LeaseGrace > 0 && now.Before(rec.Lease.ExpiresAt.Add(w.limits.LeaseGrace)) {
			graceTotal = saturatingAdd(graceTotal, rec.Lease.Size)
			if graceLease == "" {
				graceLease = id
			}
		}
	}
	if activeLease != "" && activeTotal >= regionSize {
		return LeaseActive, activeLease
	}
	if graceLease != "" && graceTotal >= regionSize {
		return LeaseGrace, graceLease
	}
	return "", ""
}

func (w *WxRegionWarden) leaseViewLocked(rec *leaseRecord, now time.Time) Lease {
	lease := rec.Lease
	switch {
	case rec.Released:
		lease.State = LeaseRevoked
	case !now.Before(lease.ExpiresAt):
		lease.State = LeaseExpired
	default:
		lease.State = LeaseActive
	}
	return lease
}

func mappingKey(m Mapping) string {
	if m.Path != "" {
		return "path:" + m.Path
	}
	if m.Backing != "" {
		return "backing:" + m.Backing
	}
	return fmt.Sprintf("range:%x-%x", m.Start, m.End)
}

func actionForLevel(level int) string {
	switch level {
	case LevelRestrict:
		return ActionRestrict
	case LevelAlert:
		return ActionAlert
	default:
		return ActionObserve
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneBoolMap(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func saturatingAdd(a, b uint64) uint64 {
	if a > ^uint64(0)-b {
		return ^uint64(0)
	}
	return a + b
}
