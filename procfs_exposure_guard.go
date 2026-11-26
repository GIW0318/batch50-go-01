// Package task governs how much of the process filesystem a namespace
// exposes, keeps monitor exemptions bound to executable digests and verifies
// that the mounted policy is the one that was asked for.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hide levels understood by the guard.
const (
	HideNone       = 0
	HideOtherUsers = 1
	HideAll        = 2
)

const snapshotVersion = 1

// Errors returned by the guard.
var (
	ErrBadNamespace  = errors.New("namespace is not usable")
	ErrBadLevel      = errors.New("hide level is not supported")
	ErrRelaxRequired = errors.New("loosening the hide level needs an explicit flag")
	ErrNoPolicy      = errors.New("namespace has no policy")
	ErrNotApplied    = errors.New("policy has not been applied")
	ErrBadExemption  = errors.New("exemption entry is not usable")
	ErrBadSnapshot   = errors.New("exposure snapshot is not usable")
)

// Options tunes the guard.
type Options struct {
	// Now is the clock used for exemption expiry. Nil means time.Now.
	Now func() time.Time
}

// Policy is the exposure policy of one namespace.
type Policy struct {
	HideLevel int  `json:"hide_level"`
	Relax     bool `json:"relax"`
}

// Process is one process as seen from a viewer.
type Process struct {
	PID    int    `json:"pid"`
	Owner  string `json:"owner"`
	Exe    string `json:"exe"`
	Digest string `json:"digest"`
}

// Exemption lets one viewer keep seeing processes of one executable image.
type Exemption struct {
	Viewer    string    `json:"viewer"`
	Exe       string    `json:"exe"`
	Digest    string    `json:"digest"`
	PID       int       `json:"pid"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Diff is one deviation between the wanted and the mounted policy.
type Diff struct {
	Field string `json:"field"`
	Want  string `json:"want"`
	Got   string `json:"got"`
}

type namespaceState struct {
	Policy       *Policy     `json:"policy,omitempty"`
	MountedLevel *int        `json:"mounted_level,omitempty"`
	Exemptions   []Exemption `json:"exemptions,omitempty"`
}

type guardSnapshot struct {
	Version    int                        `json:"version"`
	Namespaces map[string]*namespaceState `json:"namespaces"`
}

func newGuardSnapshot() *guardSnapshot {
	return &guardSnapshot{
		Version:    snapshotVersion,
		Namespaces: make(map[string]*namespaceState),
	}
}

func cloneGuardSnapshot(in *guardSnapshot) *guardSnapshot {
	out := newGuardSnapshot()
	if in == nil {
		return out
	}
	out.Version = in.Version
	for namespace, state := range in.Namespaces {
		if state == nil {
			out.Namespaces[namespace] = nil
			continue
		}
		copied := &namespaceState{}
		if state.Policy != nil {
			policy := *state.Policy
			copied.Policy = &policy
		}
		if state.MountedLevel != nil {
			level := *state.MountedLevel
			copied.MountedLevel = &level
		}
		if state.Exemptions != nil {
			copied.Exemptions = append([]Exemption(nil), state.Exemptions...)
		}
		out.Namespaces[namespace] = copied
	}
	return out
}

func (s *guardSnapshot) namespace(namespace string) *namespaceState {
	state := s.Namespaces[namespace]
	if state == nil {
		state = &namespaceState{}
		s.Namespaces[namespace] = state
	}
	return state
}

func parentNamespace(namespace string) string {
	slash := strings.LastIndexByte(namespace, '/')
	dot := strings.LastIndexByte(namespace, '.')
	separator := slash
	if dot > separator {
		separator = dot
	}
	if separator <= 0 {
		return ""
	}
	return namespace[:separator]
}

func (s *guardSnapshot) effectivePolicy(namespace string) (Policy, bool) {
	current := namespace
	for {
		if state := s.Namespaces[current]; state != nil && state.Policy != nil {
			return *state.Policy, true
		}
		parent := parentNamespace(current)
		if parent == "" || parent == current {
			return Policy{}, false
		}
		current = parent
	}
}

// ProcfsExposureGuard owns policies, mounted levels and exemptions.
type ProcfsExposureGuard struct {
	opts     Options
	writeMu  sync.Mutex
	snapshot atomic.Pointer[guardSnapshot]
}

// NewProcfsExposureGuard builds a guard with no namespaces.
func NewProcfsExposureGuard(opts Options) *ProcfsExposureGuard {
	guard := &ProcfsExposureGuard{opts: opts}
	guard.snapshot.Store(newGuardSnapshot())
	return guard
}

func (g *ProcfsExposureGuard) currentSnapshot() *guardSnapshot {
	if current := g.snapshot.Load(); current != nil {
		return current
	}
	fallback := newGuardSnapshot()
	g.snapshot.CompareAndSwap(nil, fallback)
	return g.snapshot.Load()
}

func (g *ProcfsExposureGuard) now() time.Time {
	if g.opts.Now != nil {
		return g.opts.Now()
	}
	return time.Now()
}

func blankNamespace(namespace string) bool {
	return strings.TrimSpace(namespace) == ""
}

func validHideLevel(level int) bool {
	return level == HideNone || level == HideOtherUsers || level == HideAll
}

func blankValue(value string) bool {
	return strings.TrimSpace(value) == ""
}

func validNewExemption(entry Exemption, now time.Time) bool {
	return !blankValue(entry.Viewer) &&
		!blankValue(entry.Exe) &&
		!blankValue(entry.Digest) &&
		entry.ExpiresAt.After(now)
}

func exemptionAllows(exemptions []Exemption, viewer string, target Process, now time.Time) bool {
	for _, entry := range exemptions {
		if entry.Viewer != viewer || entry.Digest != target.Digest {
			continue
		}
		if !entry.ExpiresAt.After(now) {
			continue
		}
		return true
	}
	return false
}

// SetPolicy stores the wanted policy of one namespace.
func (g *ProcfsExposureGuard) SetPolicy(ns string, policy Policy) error {
	if blankNamespace(ns) {
		return ErrBadNamespace
	}
	if !validHideLevel(policy.HideLevel) {
		return ErrBadLevel
	}

	g.writeMu.Lock()
	defer g.writeMu.Unlock()

	current := g.currentSnapshot()
	if previous, ok := current.effectivePolicy(ns); ok &&
		policy.HideLevel < previous.HideLevel && !policy.Relax {
		return ErrRelaxRequired
	}

	next := cloneGuardSnapshot(current)
	state := next.namespace(ns)
	storedPolicy := policy
	state.Policy = &storedPolicy
	g.snapshot.Store(next)
	return nil
}

// Policy reads the wanted policy of one namespace.
func (g *ProcfsExposureGuard) Policy(ns string) (Policy, bool) {
	if blankNamespace(ns) {
		return Policy{}, false
	}
	policy, ok := g.currentSnapshot().effectivePolicy(ns)
	if !ok {
		return Policy{}, false
	}
	return policy, true
}

// Apply mounts the process filesystem for one namespace and reads it back.
func (g *ProcfsExposureGuard) Apply(ns string) error {
	if blankNamespace(ns) {
		return ErrBadNamespace
	}

	g.writeMu.Lock()
	defer g.writeMu.Unlock()

	current := g.currentSnapshot()
	policy, ok := current.effectivePolicy(ns)
	if !ok {
		return ErrNoPolicy
	}

	next := cloneGuardSnapshot(current)
	level := policy.HideLevel
	next.namespace(ns).MountedLevel = &level
	g.snapshot.Store(next)
	return nil
}

// MountedLevel reports the hide level that is actually mounted.
func (g *ProcfsExposureGuard) MountedLevel(ns string) (int, bool) {
	if blankNamespace(ns) {
		return 0, false
	}
	state := g.currentSnapshot().Namespaces[ns]
	if state == nil || state.MountedLevel == nil {
		return 0, false
	}
	return *state.MountedLevel, true
}

// Visible answers whether one viewer may see one process.
func (g *ProcfsExposureGuard) Visible(ns, viewer string, target Process) (bool, error) {
	if blankNamespace(ns) {
		return false, ErrBadNamespace
	}

	current := g.currentSnapshot()
	if _, ok := current.effectivePolicy(ns); !ok {
		return false, ErrNoPolicy
	}
	state := current.Namespaces[ns]
	if state == nil || state.MountedLevel == nil {
		return false, ErrNotApplied
	}

	if target.Owner == viewer {
		return true, nil
	}

	level := *state.MountedLevel
	if level == HideNone {
		return true, nil
	}
	if exemptionAllows(state.Exemptions, viewer, target, g.now()) {
		return true, nil
	}
	if level == HideOtherUsers || level == HideAll {
		return false, nil
	}
	return false, ErrBadLevel
}

// Exempt registers one time limited monitor exemption.
func (g *ProcfsExposureGuard) Exempt(ns string, entry Exemption) error {
	if blankNamespace(ns) {
		return ErrBadNamespace
	}
	if !validNewExemption(entry, g.now()) {
		return ErrBadExemption
	}

	g.writeMu.Lock()
	defer g.writeMu.Unlock()

	current := g.currentSnapshot()
	if _, ok := current.effectivePolicy(ns); !ok {
		return ErrNoPolicy
	}

	next := cloneGuardSnapshot(current)
	state := next.namespace(ns)
	state.Exemptions = append(state.Exemptions, entry)
	g.snapshot.Store(next)
	return nil
}

// Exemptions lists the live exemptions of one namespace.
func (g *ProcfsExposureGuard) Exemptions(ns string) []Exemption {
	if blankNamespace(ns) {
		return nil
	}
	state := g.currentSnapshot().Namespaces[ns]
	if state == nil {
		return nil
	}

	now := g.now()
	live := make([]Exemption, 0, len(state.Exemptions))
	for _, entry := range state.Exemptions {
		if entry.ExpiresAt.After(now) {
			live = append(live, entry)
		}
	}
	return live
}

// Exit drops the exemptions bound to a process that has exited.
func (g *ProcfsExposureGuard) Exit(ns string, pid int) error {
	if blankNamespace(ns) {
		return ErrBadNamespace
	}

	g.writeMu.Lock()
	defer g.writeMu.Unlock()

	current := g.currentSnapshot()
	state := current.Namespaces[ns]
	if state == nil || len(state.Exemptions) == 0 {
		return nil
	}

	next := cloneGuardSnapshot(current)
	nextState := next.Namespaces[ns]
	kept := make([]Exemption, 0, len(nextState.Exemptions))
	for _, entry := range nextState.Exemptions {
		if entry.PID != pid {
			kept = append(kept, entry)
		}
	}
	nextState.Exemptions = kept
	if nextState.Policy == nil && nextState.MountedLevel == nil && len(kept) == 0 {
		delete(next.Namespaces, ns)
	}
	g.snapshot.Store(next)
	return nil
}

// Verify compares the wanted policy with the mounted one.
func (g *ProcfsExposureGuard) Verify(ns string) ([]Diff, error) {
	if blankNamespace(ns) {
		return nil, ErrBadNamespace
	}

	current := g.currentSnapshot()
	policy, ok := current.effectivePolicy(ns)
	if !ok {
		return nil, ErrNoPolicy
	}
	state := current.Namespaces[ns]
	if state == nil || state.MountedLevel == nil {
		return nil, ErrNotApplied
	}

	if policy.HideLevel == *state.MountedLevel {
		return nil, nil
	}
	return []Diff{{
		Field: "hide_level",
		Want:  strconv.Itoa(policy.HideLevel),
		Got:   strconv.Itoa(*state.MountedLevel),
	}}, nil
}

// Namespaces lists the namespaces known to the guard.
func (g *ProcfsExposureGuard) Namespaces() []string {
	current := g.currentSnapshot()
	namespaces := make([]string, 0, len(current.Namespaces))
	for namespace := range current.Namespaces {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	return namespaces
}

// Snapshot serialises policies, mounted levels and exemptions.
func (g *ProcfsExposureGuard) Snapshot() ([]byte, error) {
	current := g.currentSnapshot()
	data, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("serialise exposure snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ProcfsExposureGuard) Restore(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: snapshot must be a JSON object", ErrBadSnapshot)
	}

	restored := newGuardSnapshot()
	if err := json.Unmarshal(trimmed, restored); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateRestoredSnapshot(restored); err != nil {
		return err
	}
	restored.Version = snapshotVersion

	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	g.snapshot.Store(restored)
	return nil
}

func validateRestoredSnapshot(snapshot *guardSnapshot) error {
	if snapshot.Version != 0 && snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported snapshot version %d", ErrBadSnapshot, snapshot.Version)
	}
	if snapshot.Namespaces == nil {
		snapshot.Namespaces = make(map[string]*namespaceState)
	}

	for namespace, state := range snapshot.Namespaces {
		if blankNamespace(namespace) || state == nil {
			return fmt.Errorf("%w: invalid namespace state", ErrBadSnapshot)
		}
		if state.Policy != nil && !validHideLevel(state.Policy.HideLevel) {
			return fmt.Errorf("%w: invalid policy hide level", ErrBadSnapshot)
		}
		if state.MountedLevel != nil && !validHideLevel(*state.MountedLevel) {
			return fmt.Errorf("%w: invalid mounted hide level", ErrBadSnapshot)
		}
		if state.Policy == nil && state.MountedLevel == nil && len(state.Exemptions) == 0 {
			return fmt.Errorf("%w: empty namespace state", ErrBadSnapshot)
		}
		for _, entry := range state.Exemptions {
			if blankValue(entry.Viewer) || blankValue(entry.Exe) || blankValue(entry.Digest) {
				return fmt.Errorf("%w: incomplete exemption", ErrBadSnapshot)
			}
			if entry.ExpiresAt.IsZero() {
				return fmt.Errorf("%w: exemption has no expiry", ErrBadSnapshot)
			}
		}
	}

	for namespace := range snapshot.Namespaces {
		if _, ok := snapshot.effectivePolicy(namespace); !ok {
			return fmt.Errorf("%w: namespace %q has no policy", ErrBadSnapshot, namespace)
		}
	}
	return nil
}
