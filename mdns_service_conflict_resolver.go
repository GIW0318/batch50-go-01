// Package task arbitrates DNS-SD instance name conflicts: registrations go
// through a probing phase, a conflicting answer either forces a rename or is
// conceded by the peer, and cache-flush records are replaced per interface.
package task

import (
	"errors"
)

// errNotImplemented is returned by behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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

// MdnsServiceConflictResolver owns the registration table, the event log and
// the per interface record cache.
type MdnsServiceConflictResolver struct {
	cfg    Config
	states map[string]*State
	events map[string][]Event
	cache  map[string][]Record
	seq    int64
}

// NewMdnsServiceConflictResolver builds a resolver with the given caps.
func NewMdnsServiceConflictResolver(cfg Config) *MdnsServiceConflictResolver {
	return &MdnsServiceConflictResolver{cfg: cfg, states: map[string]*State{}, events: map[string][]Event{}, cache: map[string][]Record{}}
}

// Register admits one service and puts it into probing.
func (r *MdnsServiceConflictResolver) Register(svc Service) (State, error) {
	return State{}, errNotImplemented
}

// Observe folds one multicast answer into the tables and reports the decision.
func (r *MdnsServiceConflictResolver) Observe(msg Message) (Event, error) {
	return Event{}, errNotImplemented
}

// Commit advances one registration one phase forward.
func (r *MdnsServiceConflictResolver) Commit(instance string) error {
	return errNotImplemented
}

// Withdraw removes one registration and its record cache.
func (r *MdnsServiceConflictResolver) Withdraw(instance string) error {
	return errNotImplemented
}

// StateOf reads one registration by its registered instance name.
func (r *MdnsServiceConflictResolver) StateOf(instance string) (State, bool) {
	return State{}, false
}

// States returns every registration, ordered by instance name.
func (r *MdnsServiceConflictResolver) States() []State {
	return nil
}

// Events returns the arbitration log of one instance, oldest first.
func (r *MdnsServiceConflictResolver) Events(instance string) []Event {
	return nil
}

// Cache returns the observed records grouped by interface.
func (r *MdnsServiceConflictResolver) Cache() map[string][]Record {
	return nil
}

// Snapshot serialises registrations, events, the record cache and the counter.
func (r *MdnsServiceConflictResolver) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *MdnsServiceConflictResolver) Restore(data []byte) error {
	return errNotImplemented
}
