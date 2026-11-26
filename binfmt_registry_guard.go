// Package task keeps the binary format handler registry used by the sandbox
// host and by tenant namespaces. A registration binds a magic pattern and a
// mask to an interpreter path, and resolution walks the overlapping
// registrations by priority, mask specificity and registration order.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Recognised registration flags.
const (
	FlagPreserveArgv = "P"
	FlagOpenBinary   = "O"
	FlagCredentials  = "C"
	FlagFixBinary    = "F"
)

// HostNamespace labels the host view. Registrations carrying another
// namespace only live inside that namespace overlay.
const HostNamespace = "host"

// Errors returned by the registry guard.
var (
	ErrBadEntry        = errors.New("registration entry is invalid")
	ErrBadNamespace    = errors.New("namespace name is invalid")
	ErrBadInterpreter  = errors.New("interpreter path is invalid")
	ErrBadFlags        = errors.New("registration flags are invalid")
	ErrInterpreterLoop = errors.New("interpreter resolves through its own registration")
	ErrMaskConflict    = errors.New("an entry with the same pattern and priority already exists")
	ErrUnknownEntry    = errors.New("registration not found")
	ErrEntryLimit      = errors.New("registry entry limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Entry is one binary format registration. ID and Seq are assigned by the
// registry, the caller only fills the matching and interpreter fields.
type Entry struct {
	ID                string   `json:"id"`
	Namespace         string   `json:"namespace"`
	Magic             []byte   `json:"magic"`
	Mask              []byte   `json:"mask"`
	Interpreter       string   `json:"interpreter"`
	InterpreterHeader []byte   `json:"interpreter_header"`
	Flags             []string `json:"flags"`
	Priority          int      `json:"priority"`
	Enabled           bool     `json:"enabled"`
	Seq               int64    `json:"seq"`
}

// Limits bounds the registry tables.
type Limits struct {
	MaxEntries  int
	MaxMagicLen int
	AllowedDirs []string
}

// BinfmtRegistryGuard owns the host table, the namespace overlays and the
// monotonic id counter.
type BinfmtRegistryGuard struct {
	limits Limits
}

// NewBinfmtRegistryGuard builds a registry with the given caps.
func NewBinfmtRegistryGuard(limits Limits) *BinfmtRegistryGuard {
	return &BinfmtRegistryGuard{limits: limits}
}

// Register validates one registration, assigns its id and stores it.
func (g *BinfmtRegistryGuard) Register(e Entry) (string, error) {
	return "", errNotImplemented
}

// Unregister drops one host registration.
func (g *BinfmtRegistryGuard) Unregister(id string) error {
	return errNotImplemented
}

// UnregisterIn drops one namespace overlay registration.
func (g *BinfmtRegistryGuard) UnregisterIn(ns, id string) error {
	return errNotImplemented
}

// Resolve picks the handler for a magic prefix in the host view.
func (g *BinfmtRegistryGuard) Resolve(magic []byte) (Entry, bool, error) {
	return Entry{}, false, errNotImplemented
}

// ResolveIn picks the handler inside one namespace, falling back to the host.
func (g *BinfmtRegistryGuard) ResolveIn(ns string, magic []byte) (Entry, bool, error) {
	return Entry{}, false, errNotImplemented
}

// SetEnabled flips the enable flag of a host registration.
func (g *BinfmtRegistryGuard) SetEnabled(id string, on bool) error {
	return errNotImplemented
}

// SetEnabledIn flips the enable flag inside one namespace overlay only.
func (g *BinfmtRegistryGuard) SetEnabledIn(ns, id string, on bool) error {
	return errNotImplemented
}

// SetPriority re-ranks one host registration.
func (g *BinfmtRegistryGuard) SetPriority(id string, priority int) error {
	return errNotImplemented
}

// Entry reads one host registration.
func (g *BinfmtRegistryGuard) Entry(id string) (Entry, bool) {
	return Entry{}, false
}

// Entries lists the host registrations in registration order.
func (g *BinfmtRegistryGuard) Entries() []Entry {
	return nil
}

// NamespaceEntries lists the overlay registrations of one namespace.
func (g *BinfmtRegistryGuard) NamespaceEntries(ns string) []Entry {
	return nil
}

// Purge drops every overlay registration of one namespace and reports how
// many were removed.
func (g *BinfmtRegistryGuard) Purge(ns string) int {
	return 0
}

// Snapshot serialises the host table, the overlays and the counters.
func (g *BinfmtRegistryGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole registry state with a snapshot, all or nothing.
func (g *BinfmtRegistryGuard) Restore(data []byte) error {
	return errNotImplemented
}
