// Package task keeps the file capability table of a fleet of edge hosts:
// it converges grants to a whitelist, tracks inherit bits that survive exec,
// reports policy drift and repairs it without widening the approved set.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Violation kinds reported by Reconcile.
const (
	ViolationUnlistedCap      = "unlisted_cap"
	ViolationUnboundedInherit = "unbounded_inherit"
	ViolationPrivilegeBit     = "privilege_bit"
)

// Errors returned by the guard.
var (
	ErrBadPolicy      = errors.New("capability policy is invalid")
	ErrBlankPath      = errors.New("file path is required")
	ErrDuplicateFile  = errors.New("file is already registered")
	ErrFileLimit      = errors.New("registered file limit reached")
	ErrUnknownFile    = errors.New("file is not registered")
	ErrCapNotAllowed  = errors.New("capability is not whitelisted")
	ErrCapLimit       = errors.New("capability limit reached for this file")
	ErrCapAbsent      = errors.New("capability is not granted on this file")
	ErrSetUIDWithCaps = errors.New("setuid bit cannot coexist with capabilities")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// CapabilityPolicy is the approved capability surface of one host class.
type CapabilityPolicy struct {
	Allowed        []string `json:"allowed"`
	InheritAllowed []string `json:"inherit_allowed"`
	MaxPerFile     int      `json:"max_per_file"`
	MaxFiles       int      `json:"max_files"`
}

// FileFlags carries the privilege bits stored next to the capability set.
type FileFlags struct {
	SetUID  bool `json:"set_uid"`
	SetGID  bool `json:"set_gid"`
	Inherit bool `json:"inherit"`
}

// FileRecord is the per path row kept by the guard.
type FileRecord struct {
	Path  string    `json:"path"`
	Caps  []string  `json:"caps"`
	Flags FileFlags `json:"flags"`
	Gen   int64     `json:"gen"`
}

// Violation is one drift finding produced by Reconcile.
type Violation struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Cap  string `json:"cap"`
}

// RepairReport describes what Repair changed on one path.
type RepairReport struct {
	RemovedCaps    []string `json:"removed_caps"`
	ClearedInherit bool     `json:"cleared_inherit"`
	ClearedSetUID  bool     `json:"cleared_set_uid"`
	Gen            int64    `json:"gen"`
}

// FileCapabilityGuard owns the file table and the active policy.
type FileCapabilityGuard struct {
	policy CapabilityPolicy
}

// NewFileCapabilityGuard builds a guard around an approved policy.
func NewFileCapabilityGuard(policy CapabilityPolicy) *FileCapabilityGuard {
	return &FileCapabilityGuard{policy: policy}
}

// RegisterFile adds a path with its approved capability set and bits.
func (g *FileCapabilityGuard) RegisterFile(path string, caps []string, flags FileFlags) error {
	return errNotImplemented
}

// RemoveFile drops one path from the table.
func (g *FileCapabilityGuard) RemoveFile(path string) error {
	return errNotImplemented
}

// Grant adds one capability to an already registered path.
func (g *FileCapabilityGuard) Grant(path, capability string) error {
	return errNotImplemented
}

// Revoke removes one capability from an already registered path.
func (g *FileCapabilityGuard) Revoke(path, capability string) error {
	return errNotImplemented
}

// SetFlags replaces the privilege bits of one path.
func (g *FileCapabilityGuard) SetFlags(path string, flags FileFlags) error {
	return errNotImplemented
}

// File reads one row.
func (g *FileCapabilityGuard) File(path string) (FileRecord, bool) {
	return FileRecord{}, false
}

// Files returns every row ordered by path.
func (g *FileCapabilityGuard) Files() []FileRecord {
	return nil
}

// EffectiveCaps is the capability set that actually reaches exec.
func (g *FileCapabilityGuard) EffectiveCaps(path string) []string {
	return nil
}

// ReloadPolicy swaps the whitelist without touching the stored rows.
func (g *FileCapabilityGuard) ReloadPolicy(policy CapabilityPolicy) error {
	return errNotImplemented
}

// Reconcile lists every row that drifted away from the active policy.
func (g *FileCapabilityGuard) Reconcile() []Violation {
	return nil
}

// Repair converges one row onto the active policy.
func (g *FileCapabilityGuard) Repair(path string) (RepairReport, error) {
	return RepairReport{}, errNotImplemented
}

// Snapshot serialises the policy and every row.
func (g *FileCapabilityGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole table and policy, all or nothing.
func (g *FileCapabilityGuard) Restore(data []byte) error {
	return errNotImplemented
}
