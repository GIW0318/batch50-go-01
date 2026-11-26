// Package task hands out memory protection keys to isolation domains, keeps the
// key to domain mapping, the per key access rights and the thread pins that a
// goroutine needs before it may change those rights.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

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

const snapshotVersion = 1

type snapshotFile struct {
	Version      int         `json:"version"`
	DomainSeq    int64       `json:"domain_seq"`
	PinSeq       int64       `json:"pin_seq"`
	KeyWatermark int         `json:"key_watermark"`
	Domains      []Domain    `json:"domains"`
	Keys         []PkeyState `json:"keys"`
	Pins         []Pin       `json:"pins"`
}

type domainRecord struct {
	Domain
}

type keyRecord struct {
	pkey     Pkey
	domainID string
	rights   Rights
	version  int64
}

type pinRecord struct {
	Pin
	local bool
	tid   int
}

type threadState struct {
	pins   map[string]struct{}
	refs   map[Pkey]int
	rights map[Pkey]Rights
}

type allocatorState struct {
	domainSeq    int64
	pinSeq       int64
	keyWatermark int
	domains      map[string]*domainRecord
	order        []string
	names        map[string]string
	keys         map[Pkey]*keyRecord
	pins         map[string]*pinRecord
}

// PkeyDomainAllocator owns the domain table, the key slots and the pin table.
type PkeyDomainAllocator struct {
	cfg     Config
	mu      sync.RWMutex
	state   *allocatorState
	threads map[int]*threadState
}

// NewPkeyDomainAllocator builds an allocator with the given caps.
func NewPkeyDomainAllocator(cfg Config) *PkeyDomainAllocator {
	cfg = normalizeConfig(cfg)
	return &PkeyDomainAllocator{
		cfg:     cfg,
		state:   newAllocatorState(),
		threads: make(map[int]*threadState),
	}
}

func normalizeConfig(cfg Config) Config {
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = 15
	}
	if cfg.MaxRegionsPerDomain <= 0 {
		cfg.MaxRegionsPerDomain = 4
	}
	if cfg.MaxPinsPerDomain <= 0 {
		cfg.MaxPinsPerDomain = 8
	}
	return cfg
}

func newAllocatorState() *allocatorState {
	return &allocatorState{
		domains: make(map[string]*domainRecord),
		order:   make([]string, 0),
		names:   make(map[string]string),
		keys:    make(map[Pkey]*keyRecord),
		pins:    make(map[string]*pinRecord),
	}
}

func cloneDomain(domain Domain) Domain {
	domain.Keys = append(make([]Pkey, 0, len(domain.Keys)), domain.Keys...)
	domain.Regions = append(make([]Region, 0, len(domain.Regions)), domain.Regions...)
	return domain
}

func (r *domainRecord) view() Domain {
	return cloneDomain(r.Domain)
}

func validMode(mode PkeyMode) bool {
	return mode == ModeExclusive || mode == ModeShared
}

func validRights(rights Rights) bool {
	return rights != 0 &&
		rights&^RightsFull == 0 &&
		(rights&RightsWrite == 0 || rights&RightsRead != 0)
}

func validRegion(region Region) bool {
	return region.Len > 0 && region.Start <= ^uint64(0)-region.Len
}

func regionEnd(region Region) uint64 {
	return region.Start + region.Len
}

func regionsOverlap(left, right Region) bool {
	return validRegion(left) && validRegion(right) &&
		left.Start < regionEnd(right) && right.Start < regionEnd(left)
}

// NewDomain registers one isolation domain and returns its record.
func (a *PkeyDomainAllocator) NewDomain(name string, mode PkeyMode) (Domain, error) {
	if strings.TrimSpace(name) == "" || !validMode(mode) {
		return Domain{}, ErrInvalidDomain
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.state.names[name]; exists {
		return Domain{}, ErrDomainExists
	}

	a.state.domainSeq++
	domain := Domain{
		ID:      fmt.Sprintf("dom-%d", a.state.domainSeq),
		Name:    name,
		Mode:    mode,
		Keys:    make([]Pkey, 0),
		Regions: make([]Region, 0),
		Seq:     a.state.domainSeq,
	}
	a.state.domains[domain.ID] = &domainRecord{Domain: domain}
	a.state.names[name] = domain.ID
	a.state.order = append(a.state.order, domain.ID)
	return cloneDomain(domain), nil
}

// AllocKey hands the lowest free key slot to the domain.
func (a *PkeyDomainAllocator) AllocKey(domainID string) (Pkey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	domain, ok := a.state.domains[domainID]
	if !ok {
		return 0, ErrUnknownDomain
	}

	for candidate := Pkey(1); candidate <= Pkey(a.cfg.MaxKeys); candidate++ {
		if _, allocated := a.state.keys[candidate]; allocated {
			continue
		}
		if a.threadKeyBusyLocked(candidate) {
			continue
		}

		a.state.keys[candidate] = &keyRecord{
			pkey:     candidate,
			domainID: domainID,
			rights:   RightsRead,
			version:  1,
		}
		domain.Keys = append(domain.Keys, candidate)
		sort.Slice(domain.Keys, func(i, j int) bool {
			return domain.Keys[i] < domain.Keys[j]
		})
		if int(candidate) > a.state.keyWatermark {
			a.state.keyWatermark = int(candidate)
		}
		return candidate, nil
	}

	return 0, ErrPkeyExhausted
}

// SetRights replaces the access mask of one key and bumps its version.
func (a *PkeyDomainAllocator) SetRights(pkey Pkey, rights Rights) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tid := syscall.Gettid()

	a.mu.Lock()
	defer a.mu.Unlock()

	if !validRights(rights) {
		return ErrBadRights
	}
	key, ok := a.state.keys[pkey]
	if !ok {
		return ErrPkeyUnknown
	}

	key.rights = rights
	key.version++

	thread := a.threadStateLocked(tid)
	thread.rights[pkey] = rights
	if thread.refs[pkey] == 0 {
		delete(thread.rights, pkey)
		a.cleanThreadLocked(tid)
	}
	return nil
}

// Bind attaches one memory region to a domain.
func (a *PkeyDomainAllocator) Bind(domainID string, region Region) error {
	if !validRegion(region) {
		return ErrBadRegion
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	domain, ok := a.state.domains[domainID]
	if !ok {
		return ErrUnknownDomain
	}

	for _, existing := range domain.Regions {
		if regionsOverlap(existing, region) {
			return ErrRegionOverlap
		}
	}
	for _, other := range a.state.domains {
		if other.ID == domainID {
			continue
		}
		for _, existing := range other.Regions {
			if regionsOverlap(existing, region) {
				return ErrDomainViolation
			}
		}
	}

	limit := a.cfg.MaxRegionsPerDomain
	if domain.Mode == ModeExclusive {
		limit = 1
	}
	if len(domain.Regions) >= limit {
		return ErrRegionLimit
	}

	domain.Regions = append(domain.Regions, region)
	return nil
}

// Check reports whether a region is fully owned by the given domain.
func (a *PkeyDomainAllocator) Check(domainID string, region Region) error {
	if !validRegion(region) {
		return ErrBadRegion
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	domain, ok := a.state.domains[domainID]
	if !ok {
		return ErrUnknownDomain
	}

	ownedRegions := append(make([]Region, 0, len(domain.Regions)), domain.Regions...)
	sort.Slice(ownedRegions, func(i, j int) bool {
		return ownedRegions[i].Start < ownedRegions[j].Start
	})

	checkStart := region.Start
	checkEnd := regionEnd(region)
	for _, owned := range ownedRegions {
		ownedEnd := regionEnd(owned)
		if checkEnd <= owned.Start {
			break
		}
		if checkStart < owned.Start {
			return ErrDomainViolation
		}
		if checkStart < ownedEnd {
			checkStart = ownedEnd
		}
		if checkStart >= checkEnd {
			return nil
		}
	}

	return ErrDomainViolation
}

// PinThread records a thread pin for the domain on the calling goroutine.
func (a *PkeyDomainAllocator) PinThread(domainID string) (Pin, error) {
	runtime.LockOSThread()
	osThreadLocked := true
	defer func() {
		if osThreadLocked {
			runtime.UnlockOSThread()
		}
	}()
	tid := syscall.Gettid()

	a.mu.Lock()
	defer a.mu.Unlock()

	domain, ok := a.state.domains[domainID]
	if !ok {
		return Pin{}, ErrUnknownDomain
	}
	if len(domain.Keys) == 0 {
		return Pin{}, ErrPkeyUnknown
	}

	activePins := 0
	for _, pin := range a.state.pins {
		if pin.DomainID == domainID {
			activePins++
		}
	}
	if activePins >= a.cfg.MaxPinsPerDomain {
		return Pin{}, ErrPinLimit
	}

	pkey := domain.Keys[0]
	for _, candidate := range domain.Keys[1:] {
		if candidate < pkey {
			pkey = candidate
		}
	}

	a.state.pinSeq++
	pin := Pin{
		ID:       fmt.Sprintf("pin-%d", a.state.pinSeq),
		DomainID: domainID,
		Pkey:     pkey,
	}
	a.state.pins[pin.ID] = &pinRecord{Pin: pin, local: true, tid: tid}

	thread := a.threadStateLocked(tid)
	thread.pins[pin.ID] = struct{}{}
	thread.refs[pkey]++
	if key, ok := a.state.keys[pkey]; ok {
		thread.rights[pkey] = key.rights
	}

	osThreadLocked = false
	return pin, nil
}

// UnpinThread releases one thread pin and resets the thread local key state.
func (a *PkeyDomainAllocator) UnpinThread(pin Pin) error {
	a.mu.Lock()
	record, ok := a.state.pins[pin.ID]
	if !ok {
		a.mu.Unlock()
		return ErrUnknownPin
	}
	if record.local && record.tid != syscall.Gettid() {
		a.mu.Unlock()
		return ErrUnknownPin
	}

	delete(a.state.pins, record.ID)
	if record.local {
		if thread, exists := a.threads[record.tid]; exists {
			delete(thread.pins, record.ID)
			if thread.refs[record.Pkey] > 0 {
				thread.refs[record.Pkey]--
				if thread.refs[record.Pkey] == 0 {
					delete(thread.refs, record.Pkey)
					delete(thread.rights, record.Pkey)
				}
			}
			a.cleanThreadLocked(record.tid)
		}
	}
	local := record.local
	a.mu.Unlock()

	if local {
		runtime.UnlockOSThread()
	}
	return nil
}

// ReleaseDomain drops a domain, frees its keys and forgets its regions.
func (a *PkeyDomainAllocator) ReleaseDomain(domainID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	domain, ok := a.state.domains[domainID]
	if !ok {
		return ErrUnknownDomain
	}
	for _, pin := range a.state.pins {
		if pin.DomainID == domainID {
			return ErrPinsActive
		}
	}

	delete(a.state.names, domain.Name)
	delete(a.state.domains, domainID)
	for index, id := range a.state.order {
		if id == domainID {
			a.state.order = append(a.state.order[:index], a.state.order[index+1:]...)
			break
		}
	}
	for _, pkey := range domain.Keys {
		delete(a.state.keys, pkey)
	}
	return nil
}

// Domain reads one domain record.
func (a *PkeyDomainAllocator) Domain(domainID string) (Domain, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	domain, ok := a.state.domains[domainID]
	if !ok {
		return Domain{}, false
	}
	return domain.view(), true
}

// PkeyState reads the bookkeeping of one key slot.
func (a *PkeyDomainAllocator) PkeyState(pkey Pkey) (PkeyState, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	key, ok := a.state.keys[pkey]
	if !ok {
		return PkeyState{}, false
	}
	return PkeyState{
		Pkey:     key.pkey,
		DomainID: key.domainID,
		Rights:   key.rights,
		Version:  key.version,
	}, true
}

// Domains lists the live domains in creation order.
func (a *PkeyDomainAllocator) Domains() []Domain {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]Domain, 0, len(a.state.order))
	for _, id := range a.state.order {
		if domain, ok := a.state.domains[id]; ok {
			result = append(result, domain.view())
		}
	}
	return result
}

// Snapshot serialises the domains, the key slots and the thread pins.
func (a *PkeyDomainAllocator) Snapshot() ([]byte, error) {
	a.mu.RLock()
	file := snapshotFile{
		Version:      snapshotVersion,
		DomainSeq:    a.state.domainSeq,
		PinSeq:       a.state.pinSeq,
		KeyWatermark: a.state.keyWatermark,
		Domains:      make([]Domain, 0, len(a.state.order)),
		Keys:         make([]PkeyState, 0, len(a.state.keys)),
		Pins:         make([]Pin, 0, len(a.state.pins)),
	}
	for _, id := range a.state.order {
		file.Domains = append(file.Domains, a.state.domains[id].view())
	}
	for _, key := range a.state.keys {
		file.Keys = append(file.Keys, PkeyState{
			Pkey:     key.pkey,
			DomainID: key.domainID,
			Rights:   key.rights,
			Version:  key.version,
		})
	}
	sort.Slice(file.Keys, func(i, j int) bool {
		return file.Keys[i].Pkey < file.Keys[j].Pkey
	})
	for _, pin := range a.state.pins {
		file.Pins = append(file.Pins, pin.Pin)
	}
	sort.Slice(file.Pins, func(i, j int) bool {
		return pinSequenceForID(file.Pins[i].ID) < pinSequenceForID(file.Pins[j].ID)
	})
	a.mu.RUnlock()

	data, err := json.Marshal(&file)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *PkeyDomainAllocator) Restore(data []byte) error {
	var file snapshotFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&file); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if err := expectSnapshotEOF(decoder); err != nil {
		return err
	}

	next, err := stateFromSnapshot(&file, a.cfg)
	if err != nil {
		return err
	}

	a.mu.Lock()
	if err := a.transferLocalPinsLocked(next); err != nil {
		a.mu.Unlock()
		return err
	}
	a.state = next
	a.mu.Unlock()
	return nil
}

func expectSnapshotEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: trailing snapshot data: %v", ErrBadSnapshot, err)
	}
	return fmt.Errorf("%w: trailing snapshot value", ErrBadSnapshot)
}

func (a *PkeyDomainAllocator) transferLocalPinsLocked(next *allocatorState) error {
	for _, pin := range a.state.pins {
		if !pin.local {
			continue
		}
		restored, ok := next.pins[pin.ID]
		if !ok || restored.Pin != pin.Pin {
			return ErrPinsActive
		}
		restored.local = true
		restored.tid = pin.tid
	}
	return nil
}

func (a *PkeyDomainAllocator) threadStateLocked(tid int) *threadState {
	if a.threads == nil {
		a.threads = make(map[int]*threadState)
	}
	thread, ok := a.threads[tid]
	if !ok {
		thread = &threadState{
			pins:   make(map[string]struct{}),
			refs:   make(map[Pkey]int),
			rights: make(map[Pkey]Rights),
		}
		a.threads[tid] = thread
	}
	return thread
}

func (a *PkeyDomainAllocator) cleanThreadLocked(tid int) {
	thread, ok := a.threads[tid]
	if !ok {
		return
	}
	if len(thread.pins) == 0 && len(thread.refs) == 0 && len(thread.rights) == 0 {
		delete(a.threads, tid)
	}
}

func (a *PkeyDomainAllocator) threadKeyBusyLocked(pkey Pkey) bool {
	for _, thread := range a.threads {
		if thread.refs[pkey] > 0 {
			return true
		}
		if _, ok := thread.rights[pkey]; ok {
			return true
		}
	}
	return false
}

func stateFromSnapshot(file *snapshotFile, cfg Config) (*allocatorState, error) {
	if file.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, file.Version)
	}
	if file.DomainSeq < 0 || file.PinSeq < 0 || file.KeyWatermark < 0 {
		return nil, fmt.Errorf("%w: negative watermark", ErrBadSnapshot)
	}
	if file.KeyWatermark > cfg.MaxKeys {
		return nil, fmt.Errorf("%w: key watermark exceeds configuration", ErrBadSnapshot)
	}

	state := newAllocatorState()
	state.domainSeq = file.DomainSeq
	state.pinSeq = file.PinSeq
	state.keyWatermark = file.KeyWatermark

	previousSeq := int64(0)
	for i := range file.Domains {
		domain := cloneDomain(file.Domains[i])
		if domain.Seq <= 0 || domain.Seq <= previousSeq {
			return nil, fmt.Errorf("%w: domain sequence is not monotonic", ErrBadSnapshot)
		}
		previousSeq = domain.Seq
		if domain.Seq > state.domainSeq {
			return nil, fmt.Errorf("%w: domain sequence exceeds watermark", ErrBadSnapshot)
		}
		expectedID := fmt.Sprintf("dom-%d", domain.Seq)
		if domain.ID != expectedID {
			return nil, fmt.Errorf("%w: domain id does not match sequence", ErrBadSnapshot)
		}
		if strings.TrimSpace(domain.Name) == "" || !validMode(domain.Mode) {
			return nil, fmt.Errorf("%w: invalid domain", ErrBadSnapshot)
		}
		if _, duplicateID := state.domains[domain.ID]; duplicateID {
			return nil, fmt.Errorf("%w: duplicate domain id", ErrBadSnapshot)
		}
		if _, duplicateName := state.names[domain.Name]; duplicateName {
			return nil, fmt.Errorf("%w: duplicate domain name", ErrBadSnapshot)
		}
		if domain.Keys == nil {
			domain.Keys = make([]Pkey, 0)
		}
		if domain.Regions == nil {
			domain.Regions = make([]Region, 0)
		}

		limit := cfg.MaxRegionsPerDomain
		if domain.Mode == ModeExclusive {
			limit = 1
		}
		if len(domain.Regions) > limit {
			return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrRegionLimit)
		}
		for _, region := range domain.Regions {
			if !validRegion(region) {
				return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrBadRegion)
			}
		}
		for left := 0; left < len(domain.Regions); left++ {
			for right := left + 1; right < len(domain.Regions); right++ {
				if regionsOverlap(domain.Regions[left], domain.Regions[right]) {
					return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrRegionOverlap)
				}
			}
		}

		state.domains[domain.ID] = &domainRecord{Domain: domain}
		state.names[domain.Name] = domain.ID
		state.order = append(state.order, domain.ID)
	}

	maxAssignedKey := 0
	for _, key := range file.Keys {
		if key.Pkey <= 0 || int(key.Pkey) > cfg.MaxKeys {
			return nil, fmt.Errorf("%w: key outside budget", ErrBadSnapshot)
		}
		if key.Version < 1 || !validRights(key.Rights) {
			return nil, fmt.Errorf("%w: invalid key state", ErrBadSnapshot)
		}
		if _, ok := state.domains[key.DomainID]; !ok {
			return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrUnknownDomain)
		}
		if _, duplicate := state.keys[key.Pkey]; duplicate {
			return nil, fmt.Errorf("%w: duplicate key", ErrBadSnapshot)
		}
		state.keys[key.Pkey] = &keyRecord{
			pkey:     key.Pkey,
			domainID: key.DomainID,
			rights:   key.Rights,
			version:  key.Version,
		}
		if int(key.Pkey) > maxAssignedKey {
			maxAssignedKey = int(key.Pkey)
		}
	}
	if file.KeyWatermark < maxAssignedKey {
		return nil, fmt.Errorf("%w: key watermark is below an allocated key", ErrBadSnapshot)
	}

	for _, id := range state.order {
		domain := state.domains[id]
		ownedKeys := make(map[Pkey]struct{}, len(domain.Keys))
		for _, pkey := range domain.Keys {
			key, ok := state.keys[pkey]
			if !ok || key.domainID != id {
				return nil, fmt.Errorf("%w: domain key list references invalid key", ErrBadSnapshot)
			}
			if _, duplicate := ownedKeys[pkey]; duplicate {
				return nil, fmt.Errorf("%w: duplicate domain key", ErrBadSnapshot)
			}
			ownedKeys[pkey] = struct{}{}
		}
		for pkey, key := range state.keys {
			if key.domainID == id {
				if _, listed := ownedKeys[pkey]; !listed {
					return nil, fmt.Errorf("%w: key table is missing from domain list", ErrBadSnapshot)
				}
			}
		}
		sortedKeys := append([]Pkey(nil), domain.Keys...)
		sort.Slice(sortedKeys, func(i, j int) bool { return sortedKeys[i] < sortedKeys[j] })
		for i := range sortedKeys {
			if domain.Keys[i] != sortedKeys[i] {
				return nil, fmt.Errorf("%w: domain key list is not ordered", ErrBadSnapshot)
			}
		}
	}

	for leftIndex := 0; leftIndex < len(state.order); leftIndex++ {
		left := state.domains[state.order[leftIndex]]
		for rightIndex := leftIndex + 1; rightIndex < len(state.order); rightIndex++ {
			right := state.domains[state.order[rightIndex]]
			for _, leftRegion := range left.Regions {
				for _, rightRegion := range right.Regions {
					if regionsOverlap(leftRegion, rightRegion) {
						return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrDomainViolation)
					}
				}
			}
		}
	}

	for _, pin := range file.Pins {
		if pin.ID == "" || pin.DomainID == "" {
			return nil, fmt.Errorf("%w: invalid pin", ErrBadSnapshot)
		}
		sequence := pinSequenceForID(pin.ID)
		if sequence <= 0 || sequence > state.pinSeq {
			return nil, fmt.Errorf("%w: pin sequence exceeds watermark", ErrBadSnapshot)
		}
		if _, duplicate := state.pins[pin.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate pin", ErrBadSnapshot)
		}
		domain, ok := state.domains[pin.DomainID]
		if !ok {
			return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrUnknownDomain)
		}
		key, ok := state.keys[pin.Pkey]
		if !ok || key.domainID != pin.DomainID || !containsPkey(domain.Keys, pin.Pkey) {
			return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrPkeyUnknown)
		}
		activePins := 0
		for _, existing := range state.pins {
			if existing.DomainID == pin.DomainID {
				activePins++
			}
		}
		if activePins >= cfg.MaxPinsPerDomain {
			return nil, fmt.Errorf("%w: %w", ErrBadSnapshot, ErrPinLimit)
		}
		state.pins[pin.ID] = &pinRecord{Pin: pin}
	}

	return state, nil
}

func containsPkey(keys []Pkey, target Pkey) bool {
	for _, key := range keys {
		if key == target {
			return true
		}
	}
	return false
}

func pinSequenceForID(id string) int64 {
	if !strings.HasPrefix(id, "pin-") {
		return 0
	}
	sequence, err := strconv.ParseInt(strings.TrimPrefix(id, "pin-"), 10, 64)
	if err != nil || sequence <= 0 {
		return 0
	}
	return sequence
}
