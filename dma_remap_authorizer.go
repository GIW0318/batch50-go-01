// Package task authorises DMA remapping for passthrough devices, keeping whole
// IOMMU groups together and bounding the temporary authorisation windows.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

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

type deviceState struct {
	id        DeviceID
	group     string
	functions []Function
	detached  bool
	bound     bool
}

type groupState struct {
	mu      sync.Mutex
	name    string
	devices map[DeviceID]*deviceState
	order   []DeviceID
	domain  string
	bound   bool
	revoked bool
	reason  string
	grantID string
}

type grantState struct {
	grant  Grant
	group  string
	active bool
}

type leaseState struct {
	lease Lease
	group string
}

// DmaRemapAuthorizer owns the device table, the group table and the leases.
type DmaRemapAuthorizer struct {
	limits Limits

	mu        sync.RWMutex
	epoch     uint64
	devices   map[DeviceID]*deviceState
	groups    map[string]*groupState
	grants    map[string]*grantState
	leases    map[string]*leaseState
	nextGrant uint64
	nextLease int64
}

// NewDmaRemapAuthorizer builds an authoriser with the given caps.
func NewDmaRemapAuthorizer(limits Limits) *DmaRemapAuthorizer {
	return &DmaRemapAuthorizer{
		limits:  limits,
		devices: make(map[DeviceID]*deviceState),
		groups:  make(map[string]*groupState),
		grants:  make(map[string]*grantState),
		leases:  make(map[string]*leaseState),
	}
}

func (a *DmaRemapAuthorizer) now() time.Time {
	if a.limits.Now != nil {
		return a.limits.Now()
	}
	return time.Now()
}

func newGroupState(name string) *groupState {
	return &groupState{
		name:    name,
		devices: make(map[DeviceID]*deviceState),
	}
}

func copyFunctions(in []Function) []Function {
	out := make([]Function, len(in))
	copy(out, in)
	return out
}

func validateRegistration(group string, functions []Function) error {
	if strings.TrimSpace(group) == "" {
		return ErrBlankGroup
	}
	if len(functions) == 0 {
		return ErrNoFunctions
	}
	seen := make(map[string]struct{}, len(functions))
	for _, fn := range functions {
		if _, ok := seen[fn.Name]; ok {
			return ErrDuplicateFunction
		}
		seen[fn.Name] = struct{}{}
	}
	return nil
}

func groupExclusionReason(g *groupState) string {
	for _, id := range g.order {
		d := g.devices[id]
		if d == nil {
			continue
		}
		for _, fn := range d.functions {
			if fn.Excluded {
				name := fn.Name
				if name == "" {
					name = "<unnamed>"
				}
				return fmt.Sprintf("function %q on device %s is excluded", name, id.Path)
			}
		}
	}
	return ""
}

func attachedExclusionReason(g *groupState) string {
	for _, id := range g.order {
		d := g.devices[id]
		if d == nil || d.detached {
			continue
		}
		for _, fn := range d.functions {
			if fn.Excluded {
				name := fn.Name
				if name == "" {
					name = "<unnamed>"
				}
				return fmt.Sprintf("function %q on device %s is excluded", name, id.Path)
			}
		}
	}
	return ""
}

func removeDeviceID(in []DeviceID, target DeviceID) []DeviceID {
	for i, id := range in {
		if id == target {
			copy(in[i:], in[i+1:])
			return in[:len(in)-1]
		}
	}
	return in
}

func (a *DmaRemapAuthorizer) unbindGroupLocked(g *groupState) {
	if g.grantID != "" {
		if gs := a.grants[g.grantID]; gs != nil {
			gs.active = false
		}
	}
	for _, d := range g.devices {
		d.bound = false
	}
	for id, ls := range a.leases {
		if ls.group == g.name {
			delete(a.leases, id)
		}
	}
	g.domain = ""
	g.bound = false
	g.grantID = ""
}

func sortedGroupLocks(first, second *groupState) (*groupState, *groupState) {
	if first.name <= second.name {
		return first, second
	}
	return second, first
}

func unlockGroupPair(first, second *groupState) {
	second.mu.Unlock()
	first.mu.Unlock()
}

// RegisterDevice records one device and the IOMMU group it belongs to.
func (a *DmaRemapAuthorizer) RegisterDevice(dev DeviceID, group string, functions []Function) error {
	if err := validateRegistration(group, functions); err != nil {
		return err
	}
	functionCopy := copyFunctions(functions)

	for {
		a.mu.RLock()
		epoch := a.epoch
		_, exists := a.devices[dev]
		g := a.groups[group]
		a.mu.RUnlock()
		if exists {
			return ErrDeviceExists
		}

		if g == nil {
			a.mu.Lock()
			if a.epoch != epoch {
				a.mu.Unlock()
				continue
			}
			if _, exists = a.devices[dev]; exists {
				a.mu.Unlock()
				return ErrDeviceExists
			}
			g = a.groups[group]
			if g == nil {
				g = newGroupState(group)
				a.groups[group] = g
			}
			a.mu.Unlock()
		}

		g.mu.Lock()
		err, retry := func() (error, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.epoch != epoch {
				return nil, true
			}
			if _, exists := a.devices[dev]; exists {
				return ErrDeviceExists, false
			}
			d := &deviceState{
				id:        dev,
				group:     group,
				functions: functionCopy,
			}
			if g.bound {
				a.unbindGroupLocked(g)
			}
			d.bound = false
			a.devices[dev] = d
			g.devices[dev] = d
			g.order = append(g.order, dev)
			g.revoked = false
			g.reason = groupExclusionReason(g)
			return nil, false
		}()
		g.mu.Unlock()
		if retry {
			continue
		}
		return err
	}
}

// Detach marks one device as physically removed.
func (a *DmaRemapAuthorizer) Detach(dev DeviceID) error {
	for {
		a.mu.RLock()
		epoch := a.epoch
		d := a.devices[dev]
		var g *groupState
		if d != nil {
			g = a.groups[d.group]
		}
		a.mu.RUnlock()
		if d == nil {
			return ErrUnknownDevice
		}
		if g == nil {
			return ErrUnknownDevice
		}

		g.mu.Lock()
		err, retry := func() (error, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.epoch != epoch {
				return nil, true
			}
			d = a.devices[dev]
			if d == nil {
				return ErrUnknownDevice, false
			}
			if !d.detached && g.bound {
				a.unbindGroupLocked(g)
			}
			d.detached = true
			d.bound = false
			return nil, false
		}()
		g.mu.Unlock()
		if retry {
			continue
		}
		return err
	}
}

// Attach brings a detached device back and re-reads its group.
func (a *DmaRemapAuthorizer) Attach(dev DeviceID, group string) error {
	if strings.TrimSpace(group) == "" {
		return ErrBlankGroup
	}

	for {
		a.mu.RLock()
		epoch := a.epoch
		d := a.devices[dev]
		var oldGroup, newGroup *groupState
		if d != nil {
			oldGroup = a.groups[d.group]
		}
		newGroup = a.groups[group]
		a.mu.RUnlock()
		if d == nil {
			return ErrUnknownDevice
		}
		if oldGroup == nil {
			return ErrUnknownDevice
		}

		if newGroup == nil {
			a.mu.Lock()
			if a.epoch != epoch {
				a.mu.Unlock()
				continue
			}
			if d = a.devices[dev]; d == nil {
				a.mu.Unlock()
				return ErrUnknownDevice
			}
			newGroup = a.groups[group]
			if newGroup == nil {
				newGroup = newGroupState(group)
				a.groups[group] = newGroup
			}
			a.mu.Unlock()
		}

		if oldGroup == newGroup {
			oldGroup.mu.Lock()
			err, retry := func() (error, bool) {
				a.mu.Lock()
				defer a.mu.Unlock()
				if a.epoch != epoch {
					return nil, true
				}
				d = a.devices[dev]
				if d == nil {
					return ErrUnknownDevice, false
				}
				if a.groups[d.group] != oldGroup {
					return nil, true
				}
				wasDetached := d.detached
				d.detached = false
				if wasDetached && oldGroup.bound {
					a.unbindGroupLocked(oldGroup)
					d.bound = false
				}
				if !oldGroup.revoked {
					oldGroup.reason = groupExclusionReason(oldGroup)
				}
				return nil, false
			}()
			oldGroup.mu.Unlock()
			if retry {
				continue
			}
			return err
		}

		first, second := sortedGroupLocks(oldGroup, newGroup)
		first.mu.Lock()
		second.mu.Lock()
		err, retry := func() (error, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.epoch != epoch {
				return nil, true
			}
			d = a.devices[dev]
			if d == nil {
				return ErrUnknownDevice, false
			}
			if a.groups[d.group] != oldGroup || a.groups[group] != newGroup {
				return nil, true
			}
			if oldGroup.bound {
				a.unbindGroupLocked(oldGroup)
			}
			delete(oldGroup.devices, dev)
			oldGroup.order = removeDeviceID(oldGroup.order, dev)
			if !oldGroup.revoked {
				oldGroup.reason = groupExclusionReason(oldGroup)
			}
			if newGroup.bound {
				a.unbindGroupLocked(newGroup)
			}
			d.group = group
			d.detached = false
			d.bound = false
			newGroup.devices[dev] = d
			newGroup.order = append(newGroup.order, dev)
			if !newGroup.revoked {
				newGroup.reason = groupExclusionReason(newGroup)
			}
			return nil, false
		}()
		unlockGroupPair(first, second)
		if retry {
			continue
		}
		return err
	}
}

// Enroll binds the whole IOMMU group of the device to one domain.
func (a *DmaRemapAuthorizer) Enroll(dev DeviceID, domain string) (Grant, error) {
	for {
		a.mu.RLock()
		epoch := a.epoch
		d := a.devices[dev]
		var g *groupState
		if d != nil {
			g = a.groups[d.group]
		}
		a.mu.RUnlock()
		if d == nil {
			return Grant{}, ErrUnknownDevice
		}
		if g == nil {
			return Grant{}, ErrUnknownGroup
		}

		g.mu.Lock()
		grant, err, retry := func() (Grant, error, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.epoch != epoch {
				return Grant{}, nil, true
			}
			d = a.devices[dev]
			if d == nil {
				return Grant{}, ErrUnknownDevice, false
			}
			g = a.groups[d.group]
			if g == nil {
				return Grant{}, ErrUnknownGroup, false
			}
			if d.detached {
				return Grant{}, ErrDeviceDetached, false
			}
			if g.revoked {
				return Grant{}, ErrGroupRevoked, false
			}
			if reason := attachedExclusionReason(g); reason != "" {
				g.reason = reason
				if g.bound {
					a.unbindGroupLocked(g)
				}
				return Grant{}, ErrGroupIncomplete, false
			}
			if g.bound {
				if g.domain == domain {
					return Grant{}, ErrGroupBound, false
				}
				return Grant{}, ErrGroupConflict, false
			}
			for _, id := range g.order {
				if member := g.devices[id]; member != nil && member.bound {
					if g.domain != "" && g.domain != domain {
						return Grant{}, ErrGroupConflict, false
					}
					return Grant{}, ErrGroupConflict, false
				}
			}
			if limit := a.limits.MaxDevicesPerDomain; limit > 0 {
				used := 0
				for _, other := range a.groups {
					if other.bound && other.domain == domain {
						for _, member := range other.devices {
							if member.bound {
								used++
							}
						}
					}
				}
				bindCount := 0
				for _, member := range g.devices {
					if !member.detached {
						bindCount++
					}
				}
				if used+bindCount > limit {
					return Grant{}, ErrDomainLimit, false
				}
			}

			a.nextGrant++
			seq := a.nextGrant
			newGrant := Grant{
				ID:      fmt.Sprintf("grant-%d", seq),
				Domain:  domain,
				Group:   g.name,
				Devices: make([]DeviceID, 0, len(g.order)),
				Seq:     seq,
			}
			for _, id := range g.order {
				if member := g.devices[id]; member != nil && !member.detached {
					newGrant.Devices = append(newGrant.Devices, id)
				}
			}
			a.grants[newGrant.ID] = &grantState{
				grant:  newGrant,
				group:  g.name,
				active: true,
			}
			g.bound = true
			g.domain = domain
			g.grantID = newGrant.ID
			g.reason = ""
			for _, id := range g.order {
				if member := g.devices[id]; member != nil {
					member.bound = !member.detached
				}
			}
			return newGrant, nil, false
		}()
		g.mu.Unlock()
		if retry {
			continue
		}
		return grant, err
	}
}

// Authorize opens one temporary window for a bound grant.
func (a *DmaRemapAuthorizer) Authorize(grantID string, window time.Duration) (Lease, error) {
	if window <= 0 {
		return Lease{}, ErrBadWindow
	}
	if limit := a.limits.MaxWindow; limit > 0 && window > limit {
		return Lease{}, ErrWindowTooLong
	}

	for {
		a.mu.RLock()
		epoch := a.epoch
		gs := a.grants[grantID]
		var g *groupState
		if gs != nil {
			g = a.groups[gs.group]
		}
		a.mu.RUnlock()
		if gs == nil || g == nil {
			return Lease{}, ErrUnknownGrant
		}

		g.mu.Lock()
		now := a.now()
		lease, err, retry := func() (Lease, error, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.epoch != epoch {
				return Lease{}, nil, true
			}
			gs = a.grants[grantID]
			if gs == nil {
				return Lease{}, ErrUnknownGrant, false
			}
			g = a.groups[gs.group]
			if g == nil {
				return Lease{}, ErrUnknownGrant, false
			}
			if g.revoked {
				return Lease{}, ErrGroupRevoked, false
			}
			if !gs.active || !g.bound || g.grantID != grantID {
				return Lease{}, ErrUnknownGrant, false
			}

			used := time.Duration(0)
			for id, ls := range a.leases {
				if !now.Before(ls.lease.ExpiresAt) {
					delete(a.leases, id)
					continue
				}
				if ls.lease.Domain == gs.grant.Domain {
					used += ls.lease.Window
				}
			}
			if limit := a.limits.MaxWindowTotal; limit > 0 && used+window > limit {
				return Lease{}, ErrWindowBudget, false
			}

			a.nextLease++
			seq := a.nextLease
			newLease := Lease{
				ID:        fmt.Sprintf("lease-%d", seq),
				GrantID:   grantID,
				Domain:    gs.grant.Domain,
				Window:    window,
				ExpiresAt: now.Add(window),
				Seq:       seq,
			}
			a.leases[newLease.ID] = &leaseState{
				lease: newLease,
				group: g.name,
			}
			return newLease, nil, false
		}()
		g.mu.Unlock()
		if retry {
			continue
		}
		return lease, err
	}
}

// Revoke unbinds the whole group of the device and records the reason.
func (a *DmaRemapAuthorizer) Revoke(dev DeviceID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrBadRevokeReason
	}

	for {
		a.mu.RLock()
		epoch := a.epoch
		d := a.devices[dev]
		var g *groupState
		if d != nil {
			g = a.groups[d.group]
		}
		a.mu.RUnlock()
		if d == nil {
			return ErrUnknownDevice
		}
		if g == nil {
			return ErrUnknownDevice
		}

		g.mu.Lock()
		err, retry := func() (error, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.epoch != epoch {
				return nil, true
			}
			d = a.devices[dev]
			if d == nil {
				return ErrUnknownDevice, false
			}
			if g.bound || g.grantID != "" {
				a.unbindGroupLocked(g)
			}
			for _, member := range g.devices {
				member.bound = false
			}
			g.bound = false
			g.domain = ""
			g.grantID = ""
			g.revoked = true
			g.reason = reason
			return nil, false
		}()
		g.mu.Unlock()
		if retry {
			continue
		}
		return err
	}
}

// GroupStatus reads the state of one IOMMU group.
func (a *DmaRemapAuthorizer) GroupStatus(group string) (GroupView, error) {
	now := a.now()
	a.mu.RLock()
	defer a.mu.RUnlock()
	g := a.groups[group]
	if g == nil {
		return GroupView{}, ErrUnknownGroup
	}
	view := GroupView{
		Group:   g.name,
		Domain:  g.domain,
		Bound:   g.bound,
		Revoked: g.revoked,
		Reason:  g.reason,
		Devices: make([]DeviceView, 0, len(g.order)),
	}
	for _, id := range g.order {
		d := g.devices[id]
		if d == nil {
			continue
		}
		excluded := 0
		for _, fn := range d.functions {
			if fn.Excluded {
				excluded++
			}
		}
		view.Devices = append(view.Devices, DeviceView{
			ID:        d.id,
			Functions: len(d.functions),
			Excluded:  excluded,
			Bound:     d.bound,
			Detached:  d.detached,
		})
	}
	for _, ls := range a.leases {
		if ls.group == group && now.Before(ls.lease.ExpiresAt) {
			view.ActiveLeases++
		}
	}
	return view, nil
}

// DomainUsage reads the live counters of one domain.
func (a *DmaRemapAuthorizer) DomainUsage(domain string) Usage {
	now := a.now()
	usage := Usage{Domain: domain}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, g := range a.groups {
		if !g.bound || g.domain != domain {
			continue
		}
		for _, d := range g.devices {
			if d.bound {
				usage.Devices++
			}
		}
	}
	for _, ls := range a.leases {
		if ls.lease.Domain != domain || !now.Before(ls.lease.ExpiresAt) {
			continue
		}
		usage.Leases++
		usage.WindowTotal += ls.lease.Window
	}
	return usage
}

// ActiveLeases lists the live windows of one domain, oldest first.
func (a *DmaRemapAuthorizer) ActiveLeases(domain string) []Lease {
	now := a.now()
	a.mu.RLock()
	out := make([]Lease, 0)
	for _, ls := range a.leases {
		if ls.lease.Domain == domain && now.Before(ls.lease.ExpiresAt) {
			out = append(out, ls.lease)
		}
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out
}

type snapshotDevice struct {
	ID        DeviceID   `json:"id"`
	Group     string     `json:"group"`
	Functions []Function `json:"functions"`
	Detached  bool       `json:"detached"`
}

type snapshotGroup struct {
	Name    string     `json:"name"`
	Domain  string     `json:"domain"`
	Bound   bool       `json:"bound"`
	Revoked bool       `json:"revoked"`
	Reason  string     `json:"reason"`
	Devices []DeviceID `json:"devices"`
	GrantID string     `json:"grant_id"`
}

type snapshotGrant struct {
	Grant  Grant `json:"grant"`
	Active bool  `json:"active"`
}

type snapshotLease struct {
	Lease Lease  `json:"lease"`
	Group string `json:"group"`
}

type snapshotPayload struct {
	Version      int              `json:"version"`
	SavedAt      int64            `json:"saved_at"`
	NextGrantSeq uint64           `json:"next_grant_seq"`
	NextLeaseSeq int64            `json:"next_lease_seq"`
	Devices      []snapshotDevice `json:"devices"`
	Groups       []snapshotGroup  `json:"groups"`
	Grants       []snapshotGrant  `json:"grants"`
	Leases       []snapshotLease  `json:"leases"`
}

// Snapshot serialises devices, bindings and windows.
func (a *DmaRemapAuthorizer) Snapshot() ([]byte, error) {
	now := a.now()
	a.mu.RLock()

	payload := snapshotPayload{
		Version:      1,
		SavedAt:      now.UnixNano(),
		NextGrantSeq: a.nextGrant,
		NextLeaseSeq: a.nextLease,
	}

	deviceIDs := make([]DeviceID, 0, len(a.devices))
	for id := range a.devices {
		deviceIDs = append(deviceIDs, id)
	}
	sort.Slice(deviceIDs, func(i, j int) bool {
		if deviceIDs[i].Vendor != deviceIDs[j].Vendor {
			return deviceIDs[i].Vendor < deviceIDs[j].Vendor
		}
		if deviceIDs[i].Device != deviceIDs[j].Device {
			return deviceIDs[i].Device < deviceIDs[j].Device
		}
		return deviceIDs[i].Path < deviceIDs[j].Path
	})
	for _, id := range deviceIDs {
		d := a.devices[id]
		payload.Devices = append(payload.Devices, snapshotDevice{
			ID:        d.id,
			Group:     d.group,
			Functions: copyFunctions(d.functions),
			Detached:  d.detached,
		})
	}

	groupNames := make([]string, 0, len(a.groups))
	for name := range a.groups {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		g := a.groups[name]
		payload.Groups = append(payload.Groups, snapshotGroup{
			Name:    g.name,
			Domain:  g.domain,
			Bound:   g.bound,
			Revoked: g.revoked,
			Reason:  g.reason,
			Devices: append([]DeviceID(nil), g.order...),
			GrantID: g.grantID,
		})
	}

	grantIDs := make([]string, 0, len(a.grants))
	for id, gs := range a.grants {
		if gs.active {
			grantIDs = append(grantIDs, id)
		}
	}
	sort.Strings(grantIDs)
	for _, id := range grantIDs {
		gs := a.grants[id]
		payload.Grants = append(payload.Grants, snapshotGrant{
			Grant:  gs.grant,
			Active: gs.active,
		})
	}

	leaseIDs := make([]string, 0, len(a.leases))
	for id := range a.leases {
		leaseIDs = append(leaseIDs, id)
	}
	sort.Strings(leaseIDs)
	for _, id := range leaseIDs {
		ls := a.leases[id]
		payload.Leases = append(payload.Leases, snapshotLease{
			Lease: ls.lease,
			Group: ls.group,
		})
	}
	a.mu.RUnlock()

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *DmaRemapAuthorizer) Restore(data []byte) error {
	now := a.now()
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Version != 1 {
		return ErrBadSnapshot
	}

	devices := make(map[DeviceID]*deviceState)
	for _, sd := range payload.Devices {
		if _, exists := devices[sd.ID]; exists {
			return ErrBadSnapshot
		}
		if strings.TrimSpace(sd.Group) == "" || len(sd.Functions) == 0 {
			return ErrBadSnapshot
		}
		seen := make(map[string]struct{}, len(sd.Functions))
		for _, fn := range sd.Functions {
			if _, exists := seen[fn.Name]; exists {
				return ErrBadSnapshot
			}
			seen[fn.Name] = struct{}{}
		}
		devices[sd.ID] = &deviceState{
			id:        sd.ID,
			group:     sd.Group,
			functions: copyFunctions(sd.Functions),
			detached:  sd.Detached,
		}
	}

	groups := make(map[string]*groupState)
	members := make(map[string]map[DeviceID]struct{})
	for _, sg := range payload.Groups {
		if strings.TrimSpace(sg.Name) == "" {
			return ErrBadSnapshot
		}
		if _, exists := groups[sg.Name]; exists {
			return ErrBadSnapshot
		}
		g := newGroupState(sg.Name)
		g.domain = sg.Domain
		g.bound = sg.Bound
		g.revoked = sg.Revoked
		g.reason = sg.Reason
		g.grantID = sg.GrantID
		groups[sg.Name] = g
		members[sg.Name] = make(map[DeviceID]struct{})
		for _, id := range sg.Devices {
			if _, exists := members[sg.Name][id]; exists {
				return ErrBadSnapshot
			}
			d := devices[id]
			if d == nil || d.group != sg.Name {
				return ErrBadSnapshot
			}
			members[sg.Name][id] = struct{}{}
			g.devices[id] = d
			g.order = append(g.order, id)
		}
	}
	for id, d := range devices {
		g := groups[d.group]
		if g == nil {
			return ErrBadSnapshot
		}
		if _, ok := members[d.group][id]; !ok {
			return ErrBadSnapshot
		}
	}

	grants := make(map[string]*grantState)
	maxGrantSeq := uint64(0)
	for _, sgr := range payload.Grants {
		grant := sgr.Grant
		if grant.ID == "" {
			return ErrBadSnapshot
		}
		if _, exists := grants[grant.ID]; exists {
			return ErrBadSnapshot
		}
		g := groups[grant.Group]
		if g == nil || !sgr.Active {
			return ErrBadSnapshot
		}
		if !g.bound || g.domain != grant.Domain || g.grantID != grant.ID {
			return ErrBadSnapshot
		}
		attached := make(map[DeviceID]struct{})
		for _, id := range g.order {
			if d := g.devices[id]; d != nil && !d.detached {
				attached[id] = struct{}{}
			}
		}
		if len(attached) == 0 || len(grant.Devices) != len(attached) {
			return ErrBadSnapshot
		}
		seen := make(map[DeviceID]struct{}, len(grant.Devices))
		for _, id := range grant.Devices {
			if _, ok := attached[id]; !ok {
				return ErrBadSnapshot
			}
			if _, exists := seen[id]; exists {
				return ErrBadSnapshot
			}
			seen[id] = struct{}{}
		}
		grants[grant.ID] = &grantState{grant: grant, group: grant.Group, active: true}
		if grant.Seq > maxGrantSeq {
			maxGrantSeq = grant.Seq
		}
	}

	for _, sg := range payload.Groups {
		g := groups[sg.Name]
		if g.bound {
			if g.grantID == "" || g.revoked {
				return ErrBadSnapshot
			}
			if grants[g.grantID] == nil {
				return ErrBadSnapshot
			}
			for _, d := range g.devices {
				d.bound = !d.detached
			}
		} else {
			if g.grantID != "" {
				return ErrBadSnapshot
			}
			for _, d := range g.devices {
				d.bound = false
			}
		}
	}

	stale := false
	if elapsed := now.Sub(time.Unix(0, payload.SavedAt)); elapsed > a.limits.RestoreGrace {
		stale = true
	}

	leases := make(map[string]*leaseState)
	maxLeaseSeq := int64(0)
	for _, sl := range payload.Leases {
		lease := sl.Lease
		if lease.ID == "" || lease.GrantID == "" || lease.Window <= 0 {
			return ErrBadSnapshot
		}
		if _, exists := leases[lease.ID]; exists {
			return ErrBadSnapshot
		}
		gs := grants[lease.GrantID]
		if gs == nil || lease.Domain != gs.grant.Domain {
			return ErrBadSnapshot
		}
		if sl.Group != gs.group {
			return ErrBadSnapshot
		}
		if lease.Seq > maxLeaseSeq {
			maxLeaseSeq = lease.Seq
		}
		if stale || !now.Before(lease.ExpiresAt) {
			continue
		}
		leases[lease.ID] = &leaseState{lease: lease, group: sl.Group}
	}

	nextGrant := payload.NextGrantSeq
	if nextGrant < maxGrantSeq {
		nextGrant = maxGrantSeq
	}
	nextLease := payload.NextLeaseSeq
	if nextLease < maxLeaseSeq {
		nextLease = maxLeaseSeq
	}

	a.mu.Lock()
	a.epoch++
	a.devices = devices
	a.groups = groups
	a.grants = grants
	a.leases = leases
	a.nextGrant = nextGrant
	a.nextLease = nextLease
	a.mu.Unlock()
	return nil
}
