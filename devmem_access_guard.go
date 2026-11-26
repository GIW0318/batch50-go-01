// Package task authorises raw memory and IO device opens by address range,
// records every grant and open in an audit trail, and reclaims descriptors
// when an owner is revoked.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
)

// Device kinds.
const (
	KindMem  = "mem"
	KindPort = "port"
	KindMSR  = "msr"
)

// Access modes used by grants and files.
const (
	ReadOnly  = 0
	WriteOnly = 1
	ReadWrite = 2
)

// Capabilities a caller has to hold for a device kind.
const (
	CapRawIO  = "cap_rawio"
	CapPortIO = "cap_portio"
	CapMSR    = "cap_msr"
)

// Audit phases.
const (
	PhaseGranted   = "granted"
	PhaseOpened    = "opened"
	PhaseDenied    = "denied"
	PhaseRevoked   = "revoked"
	PhaseReclaimed = "reclaimed"
)

// File states.
const (
	FileOpen        = "open"
	FileQuarantined = "quarantined"
	FileReclaimed   = "reclaimed"
	FileClosed      = "closed"
)

const snapshotVersion = 1

// Errors returned by the guard.
var (
	ErrBadRequest    = errors.New("request is not valid")
	ErrUnknownDevice = errors.New("device is not registered")
	ErrUnknownGrant  = errors.New("grant is not known")
	ErrUnknownOwner  = errors.New("owner holds no grant or file")
	ErrUnknownFile   = errors.New("file is not known")
	ErrNoCapability  = errors.New("caller lacks the capability for this device")
	ErrNoTicket      = errors.New("work order ticket is required")
	ErrWindowClosed  = errors.New("maintenance window is closed")
	ErrOutOfRange    = errors.New("requested range is outside the device")
	ErrRangeTooLong  = errors.New("requested range exceeds the length cap")
	ErrGrantLimit    = errors.New("grant limit reached for this owner")
	ErrAuditFull     = errors.New("audit buffer is full")
	ErrGrantExpired  = errors.New("grant has expired")
	ErrGrantConsumed = errors.New("grant was already opened")
	ErrGrantRevoked  = errors.New("grant was revoked")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// GuardLimits bounds the live tables of the guard.
type GuardLimits struct {
	MaxGrantsPerOwner int
	MaxRangeLen       int64
	MaxAudit          int
}

// Caller is the identity asking for a device range.
type Caller struct {
	Owner  string   `json:"owner"`
	Group  string   `json:"group"`
	Ticket string   `json:"ticket"`
	Caps   []string `json:"caps"`
}

// Device is one raw memory or IO node with its strict address range. The
// registry only keeps Node, Kind, Base, Size and HostInode; a request reuses
// the same record and fills Offset and Length with the wanted slice, where
// Offset is relative to Base.
type Device struct {
	Node      string `json:"node"`
	Kind      string `json:"kind"`
	Base      int64  `json:"base"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	HostInode uint64 `json:"host_inode"`
}

// Grant is one authorised range with its expiry and generation.
type Grant struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Group      string `json:"group"`
	Ticket     string `json:"ticket"`
	Device     string `json:"device"`
	Offset     int64  `json:"offset"`
	Length     int64  `json:"length"`
	RW         int    `json:"rw"`
	ExpiresAt  int64  `json:"expires_at"`
	Generation int64  `json:"generation"`
	Consumed   bool   `json:"consumed"`
}

// File is one open descriptor handed to a caller.
type File struct {
	FD         int    `json:"fd"`
	GrantID    string `json:"grant_id"`
	Owner      string `json:"owner"`
	Device     string `json:"device"`
	Offset     int64  `json:"offset"`
	Length     int64  `json:"length"`
	RW         int    `json:"rw"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

// Access is one audit record.
type Access struct {
	Seq    int64  `json:"seq"`
	Phase  string `json:"phase"`
	Owner  string `json:"owner"`
	Group  string `json:"group"`
	Device string `json:"device"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	RW     int    `json:"rw"`
	Ticket string `json:"ticket"`
	Reason string `json:"reason"`
}

type maintenanceWindow struct {
	Open  int64 `json:"open"`
	Close int64 `json:"close"`
}

type grantRecord struct {
	Grant
	Revoked bool `json:"revoked"`
}

type snapshotState struct {
	Version         int                          `json:"version"`
	Limits          GuardLimits                  `json:"limits"`
	Now             int64                        `json:"now"`
	NextGrantNumber uint64                       `json:"next_grant_number"`
	NextFD          int                          `json:"next_fd"`
	NextAuditSeq    int64                        `json:"next_audit_seq"`
	Generations     map[string]int64             `json:"generations"`
	Windows         map[string]maintenanceWindow `json:"windows"`
	Devices         []Device                     `json:"devices"`
	Grants          []grantRecord                `json:"grants"`
	Files           []File                       `json:"files"`
	Audit           []Access                     `json:"audit"`
}

// DevmemAccessGuard owns devices, grants, files and the audit buffer.
type DevmemAccessGuard struct {
	limits GuardLimits
	mu     sync.Mutex

	now             int64
	devicesByNode   map[string]Device
	devicesByInode  map[uint64]Device
	windows         map[string]maintenanceWindow
	generations     map[string]int64
	grants          map[string]*grantRecord
	grantOrder      []string
	files           map[int]*File
	audit           []Access
	nextGrantNumber uint64
	nextFD          int
	nextAuditSeq    int64
}

// NewDevmemAccessGuard builds a guard with the given caps.
func NewDevmemAccessGuard(limits GuardLimits) *DevmemAccessGuard {
	return &DevmemAccessGuard{
		limits:          limits,
		devicesByNode:   make(map[string]Device),
		devicesByInode:  make(map[uint64]Device),
		windows:         make(map[string]maintenanceWindow),
		generations:     make(map[string]int64),
		grants:          make(map[string]*grantRecord),
		files:           make(map[int]*File),
		audit:           make([]Access, 0),
		nextGrantNumber: 1,
		nextFD:          3,
		nextAuditSeq:    1,
	}
}

func (g *DevmemAccessGuard) ensureStateLocked() {
	if g.devicesByNode == nil {
		g.devicesByNode = make(map[string]Device)
	}
	if g.devicesByInode == nil {
		g.devicesByInode = make(map[uint64]Device)
	}
	if g.windows == nil {
		g.windows = make(map[string]maintenanceWindow)
	}
	if g.generations == nil {
		g.generations = make(map[string]int64)
	}
	if g.grants == nil {
		g.grants = make(map[string]*grantRecord)
	}
	if g.files == nil {
		g.files = make(map[int]*File)
	}
	if g.audit == nil {
		g.audit = make([]Access, 0)
	}
	if g.nextGrantNumber == 0 {
		g.nextGrantNumber = 1
	}
	if g.nextFD < 3 {
		g.nextFD = 3
	}
	if g.nextAuditSeq <= 0 {
		g.nextAuditSeq = int64(len(g.audit)) + 1
	}
}

// AddDevice registers one device node and its strict range.
func (g *DevmemAccessGuard) AddDevice(dev Device) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	dev.Node = strings.TrimSpace(dev.Node)
	if dev.Node == "" || !validDeviceKind(dev.Kind) || dev.Size <= 0 {
		return ErrBadRequest
	}
	if _, exists := g.devicesByNode[dev.Node]; exists {
		return ErrBadRequest
	}
	if dev.HostInode != 0 {
		if _, exists := g.devicesByInode[dev.HostInode]; exists {
			return ErrBadRequest
		}
	}

	g.devicesByNode[dev.Node] = dev
	if dev.HostInode != 0 {
		g.devicesByInode[dev.HostInode] = dev
	}
	return nil
}

// Devices lists the registered devices in ascending node order.
func (g *DevmemAccessGuard) Devices() []Device {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sortedDevicesLocked()
}

func (g *DevmemAccessGuard) sortedDevicesLocked() []Device {
	nodes := make([]string, 0, len(g.devicesByNode))
	for node := range g.devicesByNode {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	out := make([]Device, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, g.devicesByNode[node])
	}
	return out
}

// SetNow moves the logical clock used for maintenance windows.
func (g *DevmemAccessGuard) SetNow(tick int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()
	g.now = tick
	return nil
}

// SetWindow opens a maintenance window for one owner.
func (g *DevmemAccessGuard) SetWindow(owner string, open, close int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	owner = strings.TrimSpace(owner)
	if owner == "" || open >= close {
		return ErrBadRequest
	}
	g.windows[owner] = maintenanceWindow{Open: open, Close: close}
	return nil
}

// Authorize checks capability, window and range and returns a grant.
func (g *DevmemAccessGuard) Authorize(caller Caller, requested Device, rw int) (Grant, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	caller.Owner = strings.TrimSpace(caller.Owner)
	caller.Group = strings.TrimSpace(caller.Group)
	caller.Ticket = strings.TrimSpace(caller.Ticket)

	if caller.Ticket == "" {
		g.denyAuthorizationLocked(caller, requested, rw, ErrNoTicket)
		return Grant{}, ErrNoTicket
	}
	if !validAccessMode(rw) {
		err := fmt.Errorf("%w: invalid access mode %d", ErrBadRequest, rw)
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}

	device, err := g.findDeviceLocked(requested)
	if err != nil {
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}
	requested.Node = device.Node

	capability := capabilityForKind(device.Kind)
	if !callerHasCapability(caller.Caps, capability) {
		err := ErrNoCapability
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}

	window, windowOK := g.windows[caller.Owner]
	if !windowOK || g.now < window.Open || g.now >= window.Close {
		err := ErrWindowClosed
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}

	if deviceHasNonRangeOffset(device) {
		err := fmt.Errorf("%w: %s has no interval offset semantics", ErrOutOfRange, device.Node)
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}

	if requested.Offset < 0 || requested.Length <= 0 {
		err := ErrOutOfRange
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}
	if requested.Offset > device.Size || requested.Length > device.Size-requested.Offset {
		err := ErrOutOfRange
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}
	if requested.Length > g.limits.MaxRangeLen {
		err := ErrRangeTooLong
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}
	if g.outstandingGrantCountLocked(caller.Owner) >= g.limits.MaxGrantsPerOwner {
		err := ErrGrantLimit
		g.denyAuthorizationLocked(caller, requested, rw, err)
		return Grant{}, err
	}
	if g.auditFullLocked() {
		return Grant{}, ErrAuditFull
	}

	grant := Grant{
		ID:         fmt.Sprintf("grant-%d", g.nextGrantNumber),
		Owner:      caller.Owner,
		Group:      caller.Group,
		Ticket:     caller.Ticket,
		Device:     device.Node,
		Offset:     requested.Offset,
		Length:     requested.Length,
		RW:         rw,
		ExpiresAt:  window.Close,
		Generation: g.generations[caller.Owner],
		Consumed:   false,
	}
	g.nextGrantNumber++
	g.grants[grant.ID] = &grantRecord{Grant: grant}
	g.grantOrder = append(g.grantOrder, grant.ID)

	_ = g.appendAuditLocked(Access{
		Phase:  PhaseGranted,
		Owner:  grant.Owner,
		Group:  grant.Group,
		Device: grant.Device,
		Offset: grant.Offset,
		Length: grant.Length,
		RW:     grant.RW,
		Ticket: grant.Ticket,
	})

	return grant, nil
}

// Open turns a grant into one open descriptor.
func (g *DevmemAccessGuard) Open(grantID string) (File, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	record, ok := g.grants[grantID]
	if !ok {
		err := ErrUnknownGrant
		_ = g.appendAuditLocked(Access{Phase: PhaseDenied, Reason: err.Error()})
		return File{}, err
	}

	grant := record.Grant
	if record.Revoked || grant.Generation < g.generations[grant.Owner] {
		err := ErrGrantRevoked
		g.denyGrantLocked(grant, err)
		return File{}, err
	}
	if grant.Consumed {
		err := ErrGrantConsumed
		g.denyGrantLocked(grant, err)
		return File{}, err
	}
	if g.now >= grant.ExpiresAt {
		err := ErrGrantExpired
		g.denyGrantLocked(grant, err)
		return File{}, err
	}
	if g.auditFullLocked() {
		return File{}, ErrAuditFull
	}

	fd := g.nextFD
	g.nextFD++
	file := File{
		FD:         fd,
		GrantID:    grant.ID,
		Owner:      grant.Owner,
		Device:     grant.Device,
		Offset:     grant.Offset,
		Length:     grant.Length,
		RW:         grant.RW,
		Generation: grant.Generation,
		State:      FileOpen,
	}
	fileCopy := file
	g.files[fd] = &fileCopy
	record.Consumed = true

	_ = g.appendAuditLocked(Access{
		Phase:  PhaseOpened,
		Owner:  grant.Owner,
		Group:  grant.Group,
		Device: grant.Device,
		Offset: grant.Offset,
		Length: grant.Length,
		RW:     grant.RW,
		Ticket: grant.Ticket,
	})

	return file, nil
}

// Close releases one open descriptor.
func (g *DevmemAccessGuard) Close(fd int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	file, ok := g.files[fd]
	if !ok {
		_ = g.appendAuditLocked(Access{Phase: PhaseDenied, Reason: ErrUnknownFile.Error()})
		return ErrUnknownFile
	}
	if file.State != FileOpen {
		_ = g.appendAuditLocked(Access{
			Phase:  PhaseDenied,
			Owner:  file.Owner,
			Device: file.Device,
			Offset: file.Offset,
			Length: file.Length,
			RW:     file.RW,
			Reason: ErrUnknownFile.Error(),
		})
		return ErrUnknownFile
	}
	delete(g.files, fd)
	return nil
}

// Revoke invalidates every grant of an owner and quarantines its files.
func (g *DevmemAccessGuard) Revoke(owner string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	owner = strings.TrimSpace(owner)
	hasGrant := false
	for _, id := range g.grantOrder {
		if g.grants[id].Owner == owner {
			hasGrant = true
			break
		}
	}
	hasFile := false
	for _, file := range g.files {
		if file.Owner == owner {
			hasFile = true
			break
		}
	}
	if !hasGrant && !hasFile {
		_ = g.appendAuditLocked(Access{
			Phase:  PhaseDenied,
			Owner:  owner,
			Reason: ErrUnknownOwner.Error(),
		})
		return ErrUnknownOwner
	}

	records := g.revocationAuditRecordsLocked(owner)
	if g.limits.MaxAudit <= 0 || len(g.audit)+len(records) > g.limits.MaxAudit {
		return ErrAuditFull
	}

	g.generations[owner]++
	for _, id := range g.grantOrder {
		record := g.grants[id]
		if record.Owner == owner {
			record.Revoked = true
		}
	}
	for _, file := range g.files {
		if file.Owner == owner {
			file.State = FileQuarantined
		}
	}
	for _, record := range records {
		_ = g.appendAuditLocked(record)
	}
	return nil
}

// Reclaim removes one quarantined descriptor.
func (g *DevmemAccessGuard) Reclaim(fd int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	file, ok := g.files[fd]
	if !ok {
		_ = g.appendAuditLocked(Access{Phase: PhaseDenied, Reason: ErrUnknownFile.Error()})
		return ErrUnknownFile
	}
	if file.State != FileQuarantined {
		_ = g.appendAuditLocked(Access{
			Phase:  PhaseDenied,
			Owner:  file.Owner,
			Device: file.Device,
			Offset: file.Offset,
			Length: file.Length,
			RW:     file.RW,
			Reason: ErrUnknownFile.Error(),
		})
		return ErrUnknownFile
	}
	if g.auditFullLocked() {
		return ErrAuditFull
	}

	record := Access{
		Phase:  PhaseReclaimed,
		Owner:  file.Owner,
		Device: file.Device,
		Offset: file.Offset,
		Length: file.Length,
		RW:     file.RW,
	}
	if grant, ok := g.grants[file.GrantID]; ok {
		record.Group = grant.Group
		record.Ticket = grant.Ticket
	}

	delete(g.files, fd)
	_ = g.appendAuditLocked(record)
	return nil
}

// Audit returns a copy of the audit buffer, oldest first.
func (g *DevmemAccessGuard) Audit() []Access {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]Access, len(g.audit))
	copy(out, g.audit)
	return out
}

// Grants returns the grants of one owner, oldest first.
func (g *DevmemAccessGuard) Grants(owner string) []Grant {
	g.mu.Lock()
	defer g.mu.Unlock()

	owner = strings.TrimSpace(owner)
	out := make([]Grant, 0)
	for _, id := range g.grantOrder {
		record := g.grants[id]
		if record.Owner == owner {
			out = append(out, record.Grant)
		}
	}
	return out
}

// Files returns the descriptors of one owner in ascending fd order.
func (g *DevmemAccessGuard) Files(owner string) []File {
	g.mu.Lock()
	defer g.mu.Unlock()

	owner = strings.TrimSpace(owner)
	fds := make([]int, 0)
	for fd, file := range g.files {
		if file.Owner == owner {
			fds = append(fds, fd)
		}
	}
	sort.Ints(fds)

	out := make([]File, 0, len(fds))
	for _, fd := range fds {
		out = append(out, *g.files[fd])
	}
	return out
}

// Quarantined returns the descriptors waiting for reclaim, ascending by fd.
func (g *DevmemAccessGuard) Quarantined() []File {
	g.mu.Lock()
	defer g.mu.Unlock()

	fds := make([]int, 0)
	for fd, file := range g.files {
		if file.State == FileQuarantined {
			fds = append(fds, fd)
		}
	}
	sort.Ints(fds)

	out := make([]File, 0, len(fds))
	for _, fd := range fds {
		out = append(out, *g.files[fd])
	}
	return out
}

// Snapshot serialises devices, grants, files and the audit buffer.
func (g *DevmemAccessGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	grants := make([]grantRecord, 0, len(g.grantOrder))
	for _, id := range g.grantOrder {
		grants = append(grants, *g.grants[id])
	}

	files := make([]File, 0, len(g.files))
	for _, file := range g.files {
		files = append(files, *file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].FD < files[j].FD })

	snapshot := snapshotState{
		Version:         snapshotVersion,
		Limits:          g.limits,
		Now:             g.now,
		NextGrantNumber: g.nextGrantNumber,
		NextFD:          g.nextFD,
		NextAuditSeq:    g.nextAuditSeq,
		Generations:     g.generations,
		Windows:         g.windows,
		Devices:         g.sortedDevicesLocked(),
		Grants:          grants,
		Files:           files,
		Audit:           append([]Access{}, g.audit...),
	}
	return json.Marshal(snapshot)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *DevmemAccessGuard) Restore(data []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureStateLocked()

	var snapshot snapshotState
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}

	replacement, err := guardFromSnapshot(&snapshot, g.limits)
	if err != nil {
		return err
	}

	g.now = replacement.now
	g.devicesByNode = replacement.devicesByNode
	g.devicesByInode = replacement.devicesByInode
	g.windows = replacement.windows
	g.generations = replacement.generations
	g.grants = replacement.grants
	g.grantOrder = replacement.grantOrder
	g.files = replacement.files
	g.audit = replacement.audit
	g.nextGrantNumber = replacement.nextGrantNumber
	g.nextFD = replacement.nextFD
	g.nextAuditSeq = replacement.nextAuditSeq
	return nil
}

func guardFromSnapshot(snapshot *snapshotState, limits GuardLimits) (*DevmemAccessGuard, error) {
	if snapshot.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}

	replacement := NewDevmemAccessGuard(limits)
	replacement.now = snapshot.Now
	replacement.nextGrantNumber = snapshot.NextGrantNumber
	replacement.nextFD = snapshot.NextFD
	replacement.nextAuditSeq = snapshot.NextAuditSeq

	for _, device := range snapshot.Devices {
		if err := replacement.AddDevice(device); err != nil {
			return nil, fmt.Errorf("%w: device: %v", ErrBadSnapshot, err)
		}
	}

	for owner, window := range snapshot.Windows {
		owner = strings.TrimSpace(owner)
		if owner == "" || window.Open >= window.Close {
			return nil, fmt.Errorf("%w: invalid maintenance window", ErrBadSnapshot)
		}
		replacement.windows[owner] = window
	}

	for owner, generation := range snapshot.Generations {
		if generation < 0 {
			return nil, fmt.Errorf("%w: negative generation", ErrBadSnapshot)
		}
		replacement.generations[strings.TrimSpace(owner)] = generation
	}

	seenGrants := make(map[string]bool)
	for i := range snapshot.Grants {
		record := snapshot.Grants[i]
		grant := record.Grant
		if grant.ID == "" || seenGrants[grant.ID] {
			return nil, fmt.Errorf("%w: duplicate or empty grant id", ErrBadSnapshot)
		}
		if grant.Generation < 0 || !validAccessMode(grant.RW) {
			return nil, fmt.Errorf("%w: invalid grant identity", ErrBadSnapshot)
		}
		device, ok := replacement.devicesByNode[grant.Device]
		if !ok {
			return nil, fmt.Errorf("%w: grant references unknown device", ErrBadSnapshot)
		}
		if grant.Offset < 0 || grant.Length <= 0 ||
			grant.Offset > device.Size || grant.Length > device.Size-grant.Offset {
			return nil, fmt.Errorf("%w: grant range is invalid", ErrBadSnapshot)
		}
		seenGrants[grant.ID] = true
		replacement.grants[grant.ID] = &grantRecord{Grant: grant, Revoked: record.Revoked}
		replacement.grantOrder = append(replacement.grantOrder, grant.ID)
	}

	seenFDs := make(map[int]bool)
	for _, file := range snapshot.Files {
		if file.FD < 3 || seenFDs[file.FD] {
			return nil, fmt.Errorf("%w: duplicate or invalid fd", ErrBadSnapshot)
		}
		if file.State != FileOpen && file.State != FileQuarantined {
			return nil, fmt.Errorf("%w: invalid file state", ErrBadSnapshot)
		}
		grant, ok := replacement.grants[file.GrantID]
		if !ok {
			return nil, fmt.Errorf("%w: file references unknown grant", ErrBadSnapshot)
		}
		if file.Owner != grant.Owner || file.Device != grant.Device ||
			file.Offset != grant.Offset || file.Length != grant.Length ||
			file.RW != grant.RW || file.Generation != grant.Generation {
			return nil, fmt.Errorf("%w: file does not match its grant", ErrBadSnapshot)
		}
		revoked := grant.Revoked || grant.Generation < replacement.generations[grant.Owner]
		if revoked && file.State != FileQuarantined {
			return nil, fmt.Errorf("%w: revoked file is not quarantined", ErrBadSnapshot)
		}
		if !revoked && file.State != FileOpen {
			return nil, fmt.Errorf("%w: active grant file is not open", ErrBadSnapshot)
		}
		seenFDs[file.FD] = true
		fileCopy := file
		replacement.files[file.FD] = &fileCopy
	}

	for i, record := range snapshot.Audit {
		if record.Seq != int64(i+1) || !validAuditPhase(record.Phase) {
			return nil, fmt.Errorf("%w: audit sequence or phase is invalid", ErrBadSnapshot)
		}
	}
	replacement.audit = append([]Access{}, snapshot.Audit...)

	if replacement.nextGrantNumber == 0 {
		return nil, fmt.Errorf("%w: invalid next grant id", ErrBadSnapshot)
	}
	futureGrantID := fmt.Sprintf("grant-%d", replacement.nextGrantNumber)
	if _, collision := replacement.grants[futureGrantID]; collision {
		return nil, fmt.Errorf("%w: next grant id collides", ErrBadSnapshot)
	}
	if replacement.nextFD < 3 {
		return nil, fmt.Errorf("%w: invalid next fd", ErrBadSnapshot)
	}
	for fd := range replacement.files {
		if fd >= replacement.nextFD {
			return nil, fmt.Errorf("%w: next fd is not ahead of files", ErrBadSnapshot)
		}
	}
	if replacement.nextAuditSeq != int64(len(replacement.audit))+1 {
		return nil, fmt.Errorf("%w: invalid next audit sequence", ErrBadSnapshot)
	}

	return replacement, nil
}

func validDeviceKind(kind string) bool {
	switch kind {
	case KindMem, KindPort, KindMSR:
		return true
	default:
		return false
	}
}

func validAccessMode(rw int) bool {
	return rw >= ReadOnly && rw <= ReadWrite
}

func validAuditPhase(phase string) bool {
	switch phase {
	case PhaseGranted, PhaseOpened, PhaseDenied, PhaseRevoked, PhaseReclaimed:
		return true
	default:
		return false
	}
}

func capabilityForKind(kind string) string {
	switch kind {
	case KindPort:
		return CapPortIO
	case KindMSR:
		return CapMSR
	default:
		return CapRawIO
	}
}

func callerHasCapability(caps []string, wanted string) bool {
	for _, capability := range caps {
		if strings.TrimSpace(capability) == wanted {
			return true
		}
	}
	return false
}

func (g *DevmemAccessGuard) findDeviceLocked(requested Device) (Device, error) {
	if requested.HostInode != 0 {
		if device, ok := g.devicesByInode[requested.HostInode]; ok {
			return device, nil
		}
	}

	node := strings.TrimSpace(requested.Node)
	if node == "" {
		return Device{}, ErrUnknownDevice
	}
	device, ok := g.devicesByNode[node]
	if !ok {
		return Device{}, ErrUnknownDevice
	}
	if requested.HostInode != 0 && device.HostInode != 0 && requested.HostInode != device.HostInode {
		return Device{}, ErrUnknownDevice
	}
	return device, nil
}

func deviceHasNonRangeOffset(device Device) bool {
	return path.Base(strings.ToLower(device.Node)) == "kmem"
}

func (g *DevmemAccessGuard) auditFullLocked() bool {
	return g.limits.MaxAudit <= 0 || len(g.audit) >= g.limits.MaxAudit
}

func (g *DevmemAccessGuard) appendAuditLocked(record Access) error {
	if g.auditFullLocked() {
		return ErrAuditFull
	}
	record.Seq = g.nextAuditSeq
	g.nextAuditSeq++
	g.audit = append(g.audit, record)
	return nil
}

func (g *DevmemAccessGuard) denyAuthorizationLocked(caller Caller, device Device, rw int, cause error) {
	_ = g.appendAuditLocked(Access{
		Phase:  PhaseDenied,
		Owner:  caller.Owner,
		Group:  caller.Group,
		Device: device.Node,
		Offset: device.Offset,
		Length: device.Length,
		RW:     rw,
		Ticket: caller.Ticket,
		Reason: cause.Error(),
	})
}

func (g *DevmemAccessGuard) denyGrantLocked(grant Grant, cause error) {
	_ = g.appendAuditLocked(Access{
		Phase:  PhaseDenied,
		Owner:  grant.Owner,
		Group:  grant.Group,
		Device: grant.Device,
		Offset: grant.Offset,
		Length: grant.Length,
		RW:     grant.RW,
		Ticket: grant.Ticket,
		Reason: cause.Error(),
	})
}

func (g *DevmemAccessGuard) fileForGrantLocked(grantID string) (*File, bool) {
	for _, file := range g.files {
		if file.GrantID == grantID {
			return file, true
		}
	}
	return nil, false
}

func (g *DevmemAccessGuard) outstandingGrantCountLocked(owner string) int {
	count := 0
	for _, grantID := range g.grantOrder {
		record := g.grants[grantID]
		if record.Owner != owner || record.Revoked {
			continue
		}
		if !record.Consumed {
			count++
			continue
		}
		if _, exists := g.fileForGrantLocked(grantID); exists {
			count++
		}
	}
	return count
}

func (g *DevmemAccessGuard) revocationAuditRecordsLocked(owner string) []Access {
	records := make([]Access, 0)
	for _, grantID := range g.grantOrder {
		grant := g.grants[grantID].Grant
		if grant.Owner != owner {
			continue
		}
		records = append(records, Access{
			Phase:  PhaseRevoked,
			Owner:  grant.Owner,
			Group:  grant.Group,
			Device: grant.Device,
			Offset: grant.Offset,
			Length: grant.Length,
			RW:     grant.RW,
			Ticket: grant.Ticket,
		})
	}

	orphanFDs := make([]int, 0)
	for fd, file := range g.files {
		if file.Owner != owner {
			continue
		}
		if _, ok := g.grants[file.GrantID]; !ok {
			orphanFDs = append(orphanFDs, fd)
		}
	}
	sort.Ints(orphanFDs)
	for _, fd := range orphanFDs {
		file := g.files[fd]
		records = append(records, Access{
			Phase:  PhaseRevoked,
			Owner:  file.Owner,
			Device: file.Device,
			Offset: file.Offset,
			Length: file.Length,
			RW:     file.RW,
		})
	}
	return records
}
