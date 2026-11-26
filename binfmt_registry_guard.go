// Package task keeps the binary format handler registry used by the sandbox
// host and by tenant namespaces. A registration binds a magic pattern and a
// mask to an interpreter path, and resolution walks the overlapping
// registrations by priority, mask specificity and registration order.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
)

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
	mu       sync.RWMutex
	limits   Limits
	next     int64
	host     []Entry
	overlays map[string][]Entry
}

// NewBinfmtRegistryGuard builds a registry with the given caps.
func NewBinfmtRegistryGuard(limits Limits) *BinfmtRegistryGuard {
	return &BinfmtRegistryGuard{limits: limits, overlays: make(map[string][]Entry)}
}

// Register validates one registration, assigns its id and stores it.
func (g *BinfmtRegistryGuard) Register(e Entry) (string, error) {
	if err := g.validatePattern(e.Magic, e.Mask); err != nil {
		return "", err
	}
	if err := g.validateInterpreter(e.Interpreter); err != nil {
		return "", err
	}
	if len(e.InterpreterHeader) > 0 && patternMatches(e.InterpreterHeader, e.Magic, e.Mask) {
		return "", ErrInterpreterLoop
	}
	if err := validateFlags(e.Flags); err != nil {
		return "", err
	}
	ns := e.Namespace
	if ns == "" || ns == HostNamespace {
		ns = HostNamespace
	} else if !validNamespace(ns) {
		return "", ErrBadNamespace
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.totalLocked() >= g.limits.MaxEntries {
		return "", ErrEntryLimit
	}
	for _, ex := range g.scopeLocked(ns) {
		if ex.Priority == e.Priority && samePattern(ex, e) {
			return "", ErrMaskConflict
		}
	}
	g.next++
	stored := cloneEntry(e)
	stored.ID = fmt.Sprintf("bf-%d", g.next)
	stored.Seq = g.next
	stored.Enabled = true
	stored.Namespace = ns
	if ns == HostNamespace {
		g.host = append(g.host, stored)
	} else {
		g.overlays[ns] = append(g.overlays[ns], stored)
	}
	return stored.ID, nil
}

// Unregister drops one host registration.
func (g *BinfmtRegistryGuard) Unregister(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.host {
		if g.host[i].ID == id {
			g.host = append(g.host[:i], g.host[i+1:]...)
			return nil
		}
	}
	return ErrUnknownEntry
}

// UnregisterIn drops one namespace overlay registration.
func (g *BinfmtRegistryGuard) UnregisterIn(ns, id string) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entries := g.overlays[ns]
	for i := range entries {
		if entries[i].ID == id {
			g.overlays[ns] = append(entries[:i], entries[i+1:]...)
			return nil
		}
	}
	return ErrUnknownEntry
}

// Resolve picks the handler for a magic prefix in the host view.
func (g *BinfmtRegistryGuard) Resolve(magic []byte) (Entry, bool, error) {
	if err := g.validateInput(magic); err != nil {
		return Entry{}, false, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return pickBest(g.host, magic)
}

// ResolveIn picks the handler inside one namespace, falling back to the host.
func (g *BinfmtRegistryGuard) ResolveIn(ns string, magic []byte) (Entry, bool, error) {
	if err := g.validateInput(magic); err != nil {
		return Entry{}, false, err
	}
	if ns == "" || ns == HostNamespace {
		return g.Resolve(magic)
	}
	if !validNamespace(ns) {
		return Entry{}, false, ErrBadNamespace
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if entries, ok := g.overlays[ns]; ok {
		if e, found, _ := pickBest(entries, magic); found {
			return e, true, nil
		}
	}
	return pickBest(g.host, magic)
}

// SetEnabled flips the enable flag of a host registration.
func (g *BinfmtRegistryGuard) SetEnabled(id string, on bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.host {
		if g.host[i].ID == id {
			g.host[i].Enabled = on
			return nil
		}
	}
	return ErrUnknownEntry
}

// SetEnabledIn flips the enable flag inside one namespace overlay only.
func (g *BinfmtRegistryGuard) SetEnabledIn(ns, id string, on bool) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entries := g.overlays[ns]
	for i := range entries {
		if entries[i].ID == id {
			entries[i].Enabled = on
			g.overlays[ns] = entries
			return nil
		}
	}
	return ErrUnknownEntry
}

// SetPriority re-ranks one host registration.
func (g *BinfmtRegistryGuard) SetPriority(id string, priority int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.host {
		if g.host[i].ID != id {
			continue
		}
		for j := range g.host {
			if j != i && g.host[j].Priority == priority && samePattern(g.host[j], g.host[i]) {
				return ErrMaskConflict
			}
		}
		g.host[i].Priority = priority
		return nil
	}
	return ErrUnknownEntry
}

// Entry reads one host registration.
func (g *BinfmtRegistryGuard) Entry(id string) (Entry, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, e := range g.host {
		if e.ID == id {
			return cloneEntry(e), true
		}
	}
	return Entry{}, false
}

// Entries lists the host registrations in registration order.
func (g *BinfmtRegistryGuard) Entries() []Entry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return cloneEntries(g.host)
}

// NamespaceEntries lists the overlay registrations of one namespace.
func (g *BinfmtRegistryGuard) NamespaceEntries(ns string) []Entry {
	if !validNamespace(ns) {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return cloneEntries(g.overlays[ns])
}

// Purge drops every overlay registration of one namespace and reports how
// many were removed.
func (g *BinfmtRegistryGuard) Purge(ns string) int {
	if !validNamespace(ns) {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := len(g.overlays[ns])
	delete(g.overlays, ns)
	return n
}

// snapshotPayload is the serialised registry state.
type snapshotPayload struct {
	Next     int64              `json:"next"`
	Host     []Entry            `json:"host"`
	Overlays map[string][]Entry `json:"overlays"`
}

// Snapshot serialises the host table, the overlays and the counters.
func (g *BinfmtRegistryGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	payload := snapshotPayload{
		Next:     g.next,
		Host:     cloneEntries(g.host),
		Overlays: make(map[string][]Entry, len(g.overlays)),
	}
	for ns, entries := range g.overlays {
		payload.Overlays[ns] = cloneEntries(entries)
	}
	return json.Marshal(payload)
}

// Restore replaces the whole registry state with a snapshot, all or nothing.
func (g *BinfmtRegistryGuard) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Next < 0 {
		return ErrBadSnapshot
	}
	total := len(payload.Host)
	seen := make(map[string]bool, total)
	maxSeq := int64(0)
	check := func(e Entry, ns string) error {
		if e.ID == "" || e.Seq <= 0 || seen[e.ID] {
			return ErrBadSnapshot
		}
		if err := g.validatePattern(e.Magic, e.Mask); err != nil {
			return ErrBadSnapshot
		}
		if err := g.validateInterpreter(e.Interpreter); err != nil {
			return ErrBadSnapshot
		}
		if err := validateFlags(e.Flags); err != nil {
			return ErrBadSnapshot
		}
		if e.Namespace != ns {
			return ErrBadSnapshot
		}
		seen[e.ID] = true
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		return nil
	}
	for _, e := range payload.Host {
		if err := check(e, HostNamespace); err != nil {
			return err
		}
	}
	for ns, entries := range payload.Overlays {
		if !validNamespace(ns) {
			return ErrBadSnapshot
		}
		total += len(entries)
		for _, e := range entries {
			if err := check(e, ns); err != nil {
				return err
			}
		}
	}
	if total > g.limits.MaxEntries {
		return ErrBadSnapshot
	}
	next := payload.Next
	if maxSeq > next {
		next = maxSeq
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.host = cloneEntries(payload.Host)
	g.overlays = make(map[string][]Entry, len(payload.Overlays))
	for ns, entries := range payload.Overlays {
		g.overlays[ns] = cloneEntries(entries)
	}
	g.next = next
	return nil
}

// validatePattern enforces the magic/mask shape rules.
func (g *BinfmtRegistryGuard) validatePattern(magic, mask []byte) error {
	n := len(magic)
	if n == 0 || n != len(mask) || n > g.limits.MaxMagicLen {
		return ErrBadEntry
	}
	for i := range magic {
		if magic[i]&^mask[i] != 0 {
			return ErrBadEntry
		}
	}
	return nil
}

// validateInput bounds a resolve query.
func (g *BinfmtRegistryGuard) validateInput(magic []byte) error {
	if len(magic) == 0 || len(magic) > g.limits.MaxMagicLen {
		return ErrBadEntry
	}
	return nil
}

// validateInterpreter enforces the interpreter path rules.
func (g *BinfmtRegistryGuard) validateInterpreter(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return ErrBadInterpreter
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return ErrBadInterpreter
		}
	}
	clean := path.Clean(p)
	for _, dir := range g.limits.AllowedDirs {
		d := path.Clean(dir)
		if clean == d || strings.HasPrefix(clean, d+"/") {
			return nil
		}
	}
	return ErrBadInterpreter
}

// validateFlags enforces the known flag set and the O/F exclusion.
func validateFlags(flags []string) error {
	seen := make(map[string]bool, len(flags))
	for _, f := range flags {
		switch f {
		case FlagPreserveArgv, FlagOpenBinary, FlagCredentials, FlagFixBinary:
		default:
			return ErrBadFlags
		}
		if seen[f] {
			return ErrBadFlags
		}
		seen[f] = true
	}
	if seen[FlagOpenBinary] && seen[FlagFixBinary] {
		return ErrBadFlags
	}
	return nil
}

// validNamespace reports whether ns may label an overlay.
func validNamespace(ns string) bool {
	if ns == "" || ns == HostNamespace || len(ns) > 64 {
		return false
	}
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// patternMatches reports whether data starts with the masked magic.
func patternMatches(data, magic, mask []byte) bool {
	if len(data) < len(magic) {
		return false
	}
	for i := range magic {
		if data[i]&mask[i] != magic[i]&mask[i] {
			return false
		}
	}
	return true
}

// samePattern reports whether two entries carry identical magic and mask.
func samePattern(a, b Entry) bool {
	return bytes.Equal(a.Magic, b.Magic) && bytes.Equal(a.Mask, b.Mask)
}

// maskBits counts the effective mask bits used as the specificity tiebreak.
func maskBits(mask []byte) int {
	bits := 0
	for _, b := range mask {
		for b != 0 {
			bits += int(b & 1)
			b >>= 1
		}
	}
	return bits
}

// pickBest selects the winning entry for a magic prefix. All-zero mask
// wildcards only serve as a fallback and always rank behind a more specific
// match; inside each class the order is highest priority, then most mask
// bits, then earliest registration.
func pickBest(entries []Entry, magic []byte) (Entry, bool, error) {
	best := -1
	bestBits := 0
	for i := range entries {
		e := &entries[i]
		if !e.Enabled || !patternMatches(magic, e.Magic, e.Mask) {
			continue
		}
		bits := maskBits(e.Mask)
		if best == -1 || better(*e, bits, entries[best], bestBits) {
			best = i
			bestBits = bits
		}
	}
	if best == -1 {
		return Entry{}, false, nil
	}
	return cloneEntry(entries[best]), true, nil
}

// better reports whether candidate a outranks the current best b.
func better(a Entry, aBits int, b Entry, bBits int) bool {
	aWild, bWild := aBits == 0, bBits == 0
	if aWild != bWild {
		return bWild
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return aBits > bBits
}

// totalLocked counts every registration; the caller must hold the lock.
func (g *BinfmtRegistryGuard) totalLocked() int {
	total := len(g.host)
	for _, entries := range g.overlays {
		total += len(entries)
	}
	return total
}

// scopeLocked returns the live entry slice of one scope.
func (g *BinfmtRegistryGuard) scopeLocked(ns string) []Entry {
	if ns == HostNamespace {
		return g.host
	}
	return g.overlays[ns]
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

func cloneEntry(e Entry) Entry {
	e.Magic = cloneBytes(e.Magic)
	e.Mask = cloneBytes(e.Mask)
	e.InterpreterHeader = cloneBytes(e.InterpreterHeader)
	e.Flags = append([]string(nil), e.Flags...)
	return e
}

func cloneEntries(entries []Entry) []Entry {
	if entries == nil {
		return nil
	}
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = cloneEntry(e)
	}
	return out
}
