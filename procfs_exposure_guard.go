// Package task governs how much of the process filesystem a namespace
// exposes, keeps monitor exemptions bound to executable digests and verifies
// that the mounted policy is the one that was asked for.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks every behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Hide levels understood by the guard.
const (
	HideNone       = 0
	HideOtherUsers = 1
	HideAll        = 2
)

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

// ProcfsExposureGuard owns policies, mounted levels and exemptions.
type ProcfsExposureGuard struct {
	opts Options
}

// NewProcfsExposureGuard builds a guard with no namespaces.
func NewProcfsExposureGuard(opts Options) *ProcfsExposureGuard {
	return &ProcfsExposureGuard{opts: opts}
}

// SetPolicy stores the wanted policy of one namespace.
func (g *ProcfsExposureGuard) SetPolicy(ns string, policy Policy) error {
	return errNotImplemented
}

// Policy reads the wanted policy of one namespace.
func (g *ProcfsExposureGuard) Policy(ns string) (Policy, bool) {
	return Policy{}, false
}

// Apply mounts the process filesystem for one namespace and reads it back.
func (g *ProcfsExposureGuard) Apply(ns string) error {
	return errNotImplemented
}

// MountedLevel reports the hide level that is actually mounted.
func (g *ProcfsExposureGuard) MountedLevel(ns string) (int, bool) {
	return 0, false
}

// Visible answers whether one viewer may see one process.
func (g *ProcfsExposureGuard) Visible(ns, viewer string, target Process) (bool, error) {
	return false, errNotImplemented
}

// Exempt registers one time limited monitor exemption.
func (g *ProcfsExposureGuard) Exempt(ns string, entry Exemption) error {
	return errNotImplemented
}

// Exemptions lists the live exemptions of one namespace.
func (g *ProcfsExposureGuard) Exemptions(ns string) []Exemption {
	return nil
}

// Exit drops the exemptions bound to a process that has exited.
func (g *ProcfsExposureGuard) Exit(ns string, pid int) error {
	return errNotImplemented
}

// Verify compares the wanted policy with the mounted one.
func (g *ProcfsExposureGuard) Verify(ns string) ([]Diff, error) {
	return nil, errNotImplemented
}

// Namespaces lists the namespaces known to the guard.
func (g *ProcfsExposureGuard) Namespaces() []string {
	return nil
}

// Snapshot serialises policies, mounted levels and exemptions.
func (g *ProcfsExposureGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ProcfsExposureGuard) Restore(data []byte) error {
	return errNotImplemented
}
