// Package task governs opens that go through a file handle instead of a path:
// the handle carries mount, inode and generation, the guard re-resolves the
// path before admitting the open and keeps an authorisation ledger.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks every behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Permission bits used by Owner.Mode.
const (
	PermRead  uint32 = 0o444
	PermWrite uint32 = 0o222
)

// Requested access bits handed to Open in flags.
const (
	AccessRead  = 0
	AccessWrite = 1
	AccessBoth  = 2
)

// Errors returned by the guard.
var (
	ErrBadPath          = errors.New("path is not acceptable")
	ErrBadMount         = errors.New("mount entry is not acceptable")
	ErrMountDenied      = errors.New("mount is not in the allowed set")
	ErrNoHandle         = errors.New("file handle is not known to the guard")
	ErrNotRegistered    = errors.New("handle has no registered owner")
	ErrAlreadyOwned     = errors.New("handle already belongs to another owner")
	ErrBadOwner         = errors.New("owner record is not acceptable")
	ErrOwnerMismatch    = errors.New("caller tenant does not own the file")
	ErrNamespaceDenied  = errors.New("cross namespace open needs a grant")
	ErrPermissionDenied = errors.New("requested access exceeds the owner mode")
	ErrRevoked          = errors.New("handle has been revoked")
	ErrStaleHandle      = errors.New("handle no longer matches the file")
	ErrOpenLimit        = errors.New("per tenant open limit reached")
	ErrGrantExpired     = errors.New("authorisation grant has expired")
	ErrCacheLimit       = errors.New("handle cache is full")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Handle identifies one file without walking the path again.
type Handle struct {
	MountID    uint64 `json:"mount_id"`
	Inode      uint64 `json:"inode"`
	Generation uint64 `json:"generation"`
	Path       string `json:"path"`
	Namespace  string `json:"namespace"`
	Digest     string `json:"digest"`
}

// Caller is the process that asks for the open.
type Caller struct {
	Tenant    string `json:"tenant"`
	PID       int    `json:"pid"`
	Namespace string `json:"namespace"`
}

// Owner is the account the file belongs to.
type Owner struct {
	Tenant string `json:"tenant"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Mode   uint32 `json:"mode"`
}

// File is one admitted open.
type File struct {
	Handle   Handle    `json:"handle"`
	Tenant   string    `json:"tenant"`
	Flags    int       `json:"flags"`
	OpenedAt time.Time `json:"opened_at"`
}

// Grant is one authorisation line of the ledger.
type Grant struct {
	Handle  Handle    `json:"handle"`
	Tenant  string    `json:"tenant"`
	CrossNS bool      `json:"cross_ns"`
	Expires time.Time `json:"expires"`
}

// Limits bounds the guard tables.
type Limits struct {
	AllowedMounts     []uint64
	MaxCache          int
	MaxOpensPerTenant int
	GrantTTL          time.Duration
}

// Mount is one entry of the mount table the guard resolves against.
type Mount struct {
	ID                  uint64 `json:"id"`
	Prefix              string `json:"prefix"`
	Namespace           string `json:"namespace"`
	GenerationSupported bool   `json:"generation_supported"`
}

// FileHandleGuard owns the mount table, the handle cache, the owner table, the
// grant ledger and the open counters.
type FileHandleGuard struct {
	limits Limits
}

// NewFileHandleGuard builds a guard with the given caps.
func NewFileHandleGuard(limits Limits) *FileHandleGuard {
	return &FileHandleGuard{limits: limits}
}

// AddMount registers one mount prefix that Lookup resolves against.
func (g *FileHandleGuard) AddMount(id uint64, prefix, namespace string, generationSupported bool) error {
	return errNotImplemented
}

// Lookup resolves a path into a handle.
func (g *FileHandleGuard) Lookup(path string) (Handle, error) {
	return Handle{}, errNotImplemented
}

// Register records the owner of one handle.
func (g *FileHandleGuard) Register(h Handle, owner Owner) error {
	return errNotImplemented
}

// Open admits or refuses one handle based open.
func (g *FileHandleGuard) Open(h Handle, flags int, caller Caller) (File, error) {
	return File{}, errNotImplemented
}

// Revoke fences a handle off and keeps the already opened files listed.
func (g *FileHandleGuard) Revoke(h Handle) error {
	return errNotImplemented
}

// Verify re-resolves the handle path and reports whether it still matches.
func (g *FileHandleGuard) Verify(h Handle) error {
	return errNotImplemented
}

// Grant writes one authorisation line for a foreign tenant or namespace.
func (g *FileHandleGuard) Grant(h Handle, tenant string, expires time.Time) error {
	return errNotImplemented
}

// Grants lists the ledger lines of one tenant, oldest first.
func (g *FileHandleGuard) Grants(tenant string) []Grant {
	return nil
}

// OpenedFiles lists the admitted opens of one tenant, oldest first.
func (g *FileHandleGuard) OpenedFiles(tenant string) []File {
	return nil
}

// OpenCount reports how many opens one tenant holds.
func (g *FileHandleGuard) OpenCount(tenant string) int {
	return 0
}

// Export serialises the mount table, owners, grants, revocations and opens.
func (g *FileHandleGuard) Export() ([]byte, error) {
	return nil, errNotImplemented
}

// Import replaces the whole state with an exported payload, all or nothing.
func (g *FileHandleGuard) Import(data []byte) error {
	return errNotImplemented
}
