// Package task arbitrates DNS-SD instance name conflicts: registrations go
// through a probing phase, a conflicting answer either forces a rename or is
// conceded by the peer, and cache-flush records are replaced per interface.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registration phases.
const (
	PhaseProbing     = "probing"
	PhaseAnnouncing  = "announcing"
	PhaseEstablished = "established"
	PhaseConflict    = "conflict"
	PhaseRenamed     = "renamed"
)

// Event kinds produced by Observe.
const (
	EventRenamed   = "renamed"
	EventKept      = "kept"
	EventCached    = "cached"
	EventIgnored   = "ignored"
	EventExhausted = "exhausted"
)

// Errors returned by the resolver.
var (
	ErrBadService        = errors.New("instance and service type are required")
	ErrDuplicateInstance = errors.New("instance is already registered")
	ErrUnknownInstance   = errors.New("instance is not registered")
	ErrBadPhase          = errors.New("phase transition is not allowed")
	ErrRenameExhausted   = errors.New("rename budget exhausted")
	ErrBadMessage        = errors.New("message has no instance name")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

const snapshotVersion = 1

// Config bounds the resolver tables and the rename budget.
type Config struct {
	MaxRenames int
	MaxRecords int
}

// Service is one DNS-SD registration request.
type Service struct {
	Instance string
	Type     string
	Host     string
	Port     int
	Addr     string
}

// State is the registration record kept per instance.
type State struct {
	Instance string
	Current  string
	Type     string
	Host     string
	Port     int
	Addr     string
	Phase    string
	Probes   int
	Renames  int
	Seq      int64
	Conceded []string
}

// Record is one resource record observed on the wire.
type Record struct {
	Interface string
	Name      string
	Value     string
}

// Message is one observed multicast answer.
type Message struct {
	Instance   string
	Type       string
	Self       bool
	CacheFlush bool
	Records    []Record
}

// Event is one arbitration decision.
type Event struct {
	Instance  string
	Kind      string
	Peer      string
	Candidate string
	Seq       int64
}

// snapshotPayload is the on-disk representation of the resolver tables.
type snapshotPayload struct {
	Version int                 `json:"version"`
	Seq     int64               `json:"seq"`
	States  map[string]*State   `json:"states"`
	Events  map[string][]Event  `json:"events"`
	Cache   map[string][]Record `json:"cache"`
}

// MdnsServiceConflictResolver owns the registration table, the event log and
// the per interface record cache.
type MdnsServiceConflictResolver struct {
	mu     sync.Mutex
	cfg    Config
	states map[string]*State
	events map[string][]Event
	cache  map[string][]Record
	seq    int64
}

// NewMdnsServiceConflictResolver builds a resolver with the given caps.
func NewMdnsServiceConflictResolver(cfg Config) *MdnsServiceConflictResolver {
	return &MdnsServiceConflictResolver{
		cfg:    cfg,
		states: map[string]*State{},
		events: map[string][]Event{},
		cache:  map[string][]Record{},
	}
}

// Register admits one service and puts it into probing.
func (r *MdnsServiceConflictResolver) Register(svc Service) (State, error) {
	instance := strings.TrimSpace(svc.Instance)
	serviceType := strings.TrimSpace(svc.Type)
	if instance == "" || serviceType == "" {
		return State{}, ErrBadService
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.states[instance]; exists {
		return State{}, ErrDuplicateInstance
	}

	r.seq++
	state := &State{
		Instance: instance,
		Current:  instance,
		Type:     serviceType,
		Host:     svc.Host,
		Port:     svc.Port,
		Addr:     svc.Addr,
		Phase:    PhaseProbing,
		Probes:   0,
		Renames:  0,
		Seq:      r.seq,
		Conceded: []string{},
	}
	r.states[instance] = state
	return cloneState(state), nil
}

// Observe folds one multicast answer into the tables and reports the decision.
func (r *MdnsServiceConflictResolver) Observe(msg Message) (Event, error) {
	if strings.TrimSpace(msg.Instance) == "" {
		return Event{}, ErrBadMessage
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Our own multicast traffic is never a conflict: it neither consumes the
	// rename budget nor advances the global sequence.
	if msg.Self {
		return Event{}, nil
	}

	r.seq++
	seq := r.seq

	state := r.findRegistration(msg.Type)

	if state == nil || !r.isContender(state, msg) {
		r.applyCache(msg)
		instance := ""
		if state != nil {
			instance = state.Instance
		}
		return Event{Instance: instance, Kind: EventCached, Peer: msg.Instance, Seq: seq}, nil
	}

	event := Event{Instance: state.Instance, Peer: msg.Instance, Seq: seq}

	if containsString(state.Conceded, msg.Instance) {
		event.Kind = EventIgnored
		r.events[state.Instance] = append(r.events[state.Instance], event)
		return event, nil
	}

	if msg.Instance >= state.Current {
		event.Kind = EventKept
		r.events[state.Instance] = append(r.events[state.Instance], event)
		return event, nil
	}

	if state.Renames >= r.cfg.MaxRenames {
		state.Phase = PhaseConflict
		event.Kind = EventExhausted
		r.events[state.Instance] = append(r.events[state.Instance], event)
		return event, ErrRenameExhausted
	}

	state.Renames++
	state.Current = renameCandidate(state.Instance, state.Renames)
	state.Phase = PhaseRenamed
	state.Conceded = append(state.Conceded, msg.Instance)
	event.Kind = EventRenamed
	event.Candidate = state.Current
	r.events[state.Instance] = append(r.events[state.Instance], event)
	return event, nil
}

// Commit advances one registration one phase forward.
func (r *MdnsServiceConflictResolver) Commit(instance string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[instance]
	if !ok {
		return ErrUnknownInstance
	}

	switch state.Phase {
	case PhaseProbing, PhaseAnnouncing, PhaseRenamed:
		if state.Phase == PhaseAnnouncing {
			state.Phase = PhaseEstablished
		} else {
			state.Phase = PhaseAnnouncing
		}
		return nil
	default:
		return ErrBadPhase
	}
}

// Withdraw removes one registration and its event log.
func (r *MdnsServiceConflictResolver) Withdraw(instance string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.states[instance]; !ok {
		return ErrUnknownInstance
	}
	delete(r.states, instance)
	delete(r.events, instance)
	return nil
}

// StateOf reads one registration by its registered instance name.
func (r *MdnsServiceConflictResolver) StateOf(instance string) (State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[instance]
	if !ok {
		return State{}, false
	}
	return cloneState(state), true
}

// States returns every registration, ordered by instance name.
func (r *MdnsServiceConflictResolver) States() []State {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(r.states))
	for name := range r.states {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]State, 0, len(names))
	for _, name := range names {
		result = append(result, cloneState(r.states[name]))
	}
	return result
}

// Events returns the arbitration log of one instance, oldest first.
func (r *MdnsServiceConflictResolver) Events(instance string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	log := r.events[instance]
	if log == nil {
		return []Event{}
	}
	return append([]Event(nil), log...)
}

// Cache returns the observed records grouped by interface.
func (r *MdnsServiceConflictResolver) Cache() map[string][]Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make(map[string][]Record, len(r.cache))
	for iface, records := range r.cache {
		result[iface] = append([]Record(nil), records...)
	}
	return result
}

// Snapshot serialises registrations, events, the record cache and the counter.
func (r *MdnsServiceConflictResolver) Snapshot() ([]byte, error) {
	r.mu.Lock()
	payload := snapshotPayload{
		Version: snapshotVersion,
		Seq:     r.seq,
		States:  make(map[string]*State, len(r.states)),
		Events:  make(map[string][]Event, len(r.events)),
		Cache:   make(map[string][]Record, len(r.cache)),
	}
	for name, state := range r.states {
		payload.States[name] = cloneStatePtr(state)
	}
	for name, log := range r.events {
		payload.Events[name] = append([]Event(nil), log...)
	}
	for iface, records := range r.cache {
		payload.Cache[iface] = append([]Record(nil), records...)
	}
	r.mu.Unlock()

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.Join(ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *MdnsServiceConflictResolver) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return errors.Join(ErrBadSnapshot, err)
	}
	if payload.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	states := make(map[string]*State, len(payload.States))
	for name, state := range payload.States {
		if state == nil || strings.TrimSpace(state.Instance) == "" || strings.TrimSpace(state.Type) == "" {
			return ErrBadSnapshot
		}
		if state.Instance != name {
			return ErrBadSnapshot
		}
		cloned := cloneStatePtr(state)
		if cloned.Conceded == nil {
			cloned.Conceded = []string{}
		}
		states[name] = cloned
	}

	events := make(map[string][]Event, len(payload.Events))
	for name, log := range payload.Events {
		if _, ok := states[name]; !ok {
			return ErrBadSnapshot
		}
		events[name] = append([]Event(nil), log...)
	}

	cache := make(map[string][]Record, len(payload.Cache))
	for iface, records := range payload.Cache {
		cache[iface] = append([]Record(nil), records...)
	}

	r.mu.Lock()
	r.states = states
	r.events = events
	r.cache = cache
	r.seq = payload.Seq
	r.mu.Unlock()
	return nil
}

// findRegistration locates the registration that an answer may arbitrate. An
// answer of another service type belongs to a different table and can never
// conflict with a registration. The key is always the original instance name,
// while Current tracks renames.
func (r *MdnsServiceConflictResolver) findRegistration(serviceType string) *State {
	for _, state := range r.states {
		if state.Type == serviceType {
			return state
		}
	}
	return nil
}

// isContender tells whether an answer comes from the peer box paired with the
// registration on the same point-to-point link, rather than an unrelated
// device that merely happens to run the same service type. The two
// experiment boxes occupy one /30 transit subnet; every other address is a
// foreign service whose records are only cached.
func (r *MdnsServiceConflictResolver) isContender(state *State, msg Message) bool {
	local := net.ParseIP(strings.TrimSpace(state.Addr))
	if local == nil {
		return false
	}
	for _, record := range msg.Records {
		peer := net.ParseIP(strings.TrimSpace(record.Value))
		if peer != nil && sameLinkSubnet(local, peer) {
			return true
		}
	}
	return false
}

// applyCache folds the message records into the per interface cache. A
// cache-flush answer replaces only the interface buckets it touches; a plain
// answer appends to the existing bucket.
func (r *MdnsServiceConflictResolver) applyCache(msg Message) {
	flushIfaces := map[string]struct{}{}
	if msg.CacheFlush {
		for _, record := range msg.Records {
			flushIfaces[record.Interface] = struct{}{}
		}
		for iface := range flushIfaces {
			r.cache[iface] = []Record{}
		}
	}
	for _, record := range msg.Records {
		if r.cfg.MaxRecords > 0 {
			if total := r.totalRecordsLocked(); total >= r.cfg.MaxRecords {
				continue
			}
		}
		r.cache[record.Interface] = append(r.cache[record.Interface], record)
	}
	for iface, records := range r.cache {
		if len(records) == 0 {
			delete(r.cache, iface)
		}
	}
}

func (r *MdnsServiceConflictResolver) totalRecordsLocked() int {
	total := 0
	for _, records := range r.cache {
		total += len(records)
	}
	return total
}

func renameCandidate(instance string, renames int) string {
	return instance + "-" + strconv.Itoa(renames+1)
}

func sameLinkSubnet(local, peer net.IP) bool {
	local4 := local.To4()
	peer4 := peer.To4()
	if local4 == nil || peer4 == nil {
		return false
	}
	mask := net.CIDRMask(30, 32)
	return bytes.Equal(local4.Mask(mask), peer4.Mask(mask))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneState(state *State) State {
	cloned := *state
	cloned.Conceded = append([]string(nil), state.Conceded...)
	return cloned
}

func cloneStatePtr(state *State) *State {
	cloned := cloneState(state)
	return &cloned
}
