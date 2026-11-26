// Package task seals a hibernate memory image chunk by chunk, verifies the
// chunk table when the machine wakes up and keeps sensitive pages accounted for.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Chunk states.
const (
	ChunkPending  = "pending"
	ChunkWritten  = "written"
	ChunkVerified = "verified"
	ChunkCorrupt  = "corrupt"
)

// Errors returned by the guard.
var (
	ErrBadImage         = errors.New("image reference is invalid")
	ErrImageExists      = errors.New("image already sealed")
	ErrImageLimit       = errors.New("image table is full")
	ErrPolicyMismatch   = errors.New("seal policy does not match the image header")
	ErrBadPolicy        = errors.New("seal policy is incomplete")
	ErrUnknownImage     = errors.New("unknown image")
	ErrChunkLimit       = errors.New("chunk count exceeds the per image cap")
	ErrDiskBudget       = errors.New("disk reservation is exhausted")
	ErrImageSealed      = errors.New("image is closed for writing")
	ErrIncomplete       = errors.New("image does not have all pages yet")
	ErrAlreadySealed    = errors.New("image is already committed")
	ErrNotSealed        = errors.New("image is not committed")
	ErrImageBusy        = errors.New("image is already open")
	ErrNotOpened        = errors.New("image is not open")
	ErrChunkRange       = errors.New("chunk index is out of range")
	ErrChunkCorrupt     = errors.New("chunk content does not match its hash")
	ErrSensitivePending = errors.New("sensitive pages are still resident")
	ErrDuplicatePage    = errors.New("sensitive page already registered")
	ErrUnknownPage      = errors.New("unknown sensitive page")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// ImageRef is the in memory image handed over for sealing.
type ImageRef struct {
	ID    string
	Pages int
	Bytes int64
}

// SealPolicy is written into the image header.
type SealPolicy struct {
	Algorithm string
	Retention time.Duration
	Sealed    bool
}

// KeyRef is the key material offered when the image is opened again.
type KeyRef struct {
	ID        string
	Algorithm string
}

// ChunkState is one entry of the chunk table.
type ChunkState struct {
	Index int
	Size  int
	Hash  string
	State string
}

// SealedImage is the image header plus the chunk table.
type SealedImage struct {
	ID        string
	Algorithm string
	Pages     int
	Bytes     int64
	Chunks    []ChunkState
	Watermark int
	Opened    bool
	Completed bool
	Policy    SealPolicy
	SealedAt  time.Time
}

// Limits bounds the image table, the chunk table and the disk reservation.
type Limits struct {
	MaxChunksPerImage int
	MaxImageBytes     int64
	MaxImages         int
	RestoreGrace      time.Duration
	Now               func() time.Time
}

// HibernateImageGuard owns the image table and the sensitive page ledger.
type HibernateImageGuard struct {
	limits Limits
}

// NewHibernateImageGuard builds a guard with the given caps.
func NewHibernateImageGuard(limits Limits) *HibernateImageGuard {
	return &HibernateImageGuard{limits: limits}
}

// Seal registers a new image that is going to be written chunk by chunk.
func (g *HibernateImageGuard) Seal(image ImageRef, policy SealPolicy) (SealedImage, error) {
	return SealedImage{}, errNotImplemented
}

// AppendChunk appends the next chunk and returns its index.
func (g *HibernateImageGuard) AppendChunk(imgID string, data []byte) (int, error) {
	return 0, errNotImplemented
}

// Commit closes the image once every page has been written.
func (g *HibernateImageGuard) Commit(imgID string) error {
	return errNotImplemented
}

// Open verifies the key against the header and marks the image open.
func (g *HibernateImageGuard) Open(img SealedImage, key KeyRef) (ImageRef, error) {
	return ImageRef{}, errNotImplemented
}

// Close releases the open marker of one image.
func (g *HibernateImageGuard) Close(imgID string) error {
	return errNotImplemented
}

// VerifyChunk checks one chunk against the chunk table.
func (g *HibernateImageGuard) VerifyChunk(img SealedImage, idx int) (ChunkState, error) {
	return ChunkState{}, errNotImplemented
}

// Discard drops one image and its chunk table.
func (g *HibernateImageGuard) Discard(imgID string) error {
	return errNotImplemented
}

// Chunks returns a copy of the chunk table of one image.
func (g *HibernateImageGuard) Chunks(imgID string) []ChunkState {
	return nil
}

// Images returns the image table ordered by image id.
func (g *HibernateImageGuard) Images() []SealedImage {
	return nil
}

// RegisterSensitivePage records a page that must be wiped before wake up ends.
func (g *HibernateImageGuard) RegisterSensitivePage(imgID, page string) error {
	return errNotImplemented
}

// ClearSensitivePage records that one sensitive page has been wiped.
func (g *HibernateImageGuard) ClearSensitivePage(imgID, page string) error {
	return errNotImplemented
}

// PendingSensitive lists the sensitive pages that are still resident.
func (g *HibernateImageGuard) PendingSensitive(imgID string) []string {
	return nil
}

// Snapshot serialises images, chunk tables and the sensitive page ledger.
func (g *HibernateImageGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *HibernateImageGuard) Restore(data []byte) error {
	return errNotImplemented
}
