// Package task seals a hibernate memory image chunk by chunk, verifies the
// chunk table when the machine wakes up and keeps sensitive pages accounted for.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

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

type sensitivePage struct {
	Page    string
	Cleared bool
}

type imageRecord struct {
	image     SealedImage
	committed bool
	sensitive []sensitivePage
}

type snapshotImage struct {
	Image     SealedImage     `json:"image"`
	Committed bool            `json:"committed"`
	Sensitive []sensitivePage `json:"sensitive,omitempty"`
}

type snapshotState struct {
	Version int             `json:"version"`
	Images  []snapshotImage `json:"images"`
}

const (
	snapshotVersion = 1
	snapshotMagic   = "HIBIMG1\n"
)

// HibernateImageGuard owns the image table and the sensitive page ledger.
type HibernateImageGuard struct {
	mu        sync.Mutex
	limits    Limits
	images    map[string]*imageRecord
	usedBytes int64
}

// NewHibernateImageGuard builds a guard with the given caps.
func NewHibernateImageGuard(limits Limits) *HibernateImageGuard {
	return &HibernateImageGuard{
		limits: limits,
		images: make(map[string]*imageRecord),
	}
}

func (g *HibernateImageGuard) now() time.Time {
	if g.limits.Now != nil {
		return g.limits.Now()
	}
	return time.Now()
}

func copyImage(img SealedImage) SealedImage {
	out := img
	out.Chunks = append([]ChunkState(nil), img.Chunks...)
	return out
}

func copySensitive(pages []sensitivePage) []sensitivePage {
	return append([]sensitivePage(nil), pages...)
}

func (g *HibernateImageGuard) expiryLocked(rec *imageRecord) time.Time {
	return rec.image.SealedAt.Add(rec.image.Policy.Retention).Add(g.limits.RestoreGrace)
}

func (g *HibernateImageGuard) expiredLocked(rec *imageRecord, now time.Time) bool {
	return now.After(g.expiryLocked(rec))
}

func (g *HibernateImageGuard) recomputeUsageLocked() {
	var used int64
	for _, rec := range g.images {
		for _, chunk := range rec.image.Chunks {
			if chunk.Size > 0 {
				used += int64(chunk.Size)
			}
		}
	}
	g.usedBytes = used
}

func (g *HibernateImageGuard) expireLocked(now time.Time) {
	changed := false
	for id, rec := range g.images {
		if g.expiredLocked(rec, now) {
			delete(g.images, id)
			changed = true
		}
	}
	if changed {
		g.recomputeUsageLocked()
	}
}

func (g *HibernateImageGuard) imageLocked(id string) (*imageRecord, error) {
	if g.images == nil {
		g.images = make(map[string]*imageRecord)
	}
	rec, ok := g.images[id]
	if !ok {
		return nil, ErrUnknownImage
	}
	return rec, nil
}

func (g *HibernateImageGuard) pendingSensitiveLocked(rec *imageRecord) int {
	pending := 0
	for _, page := range rec.sensitive {
		if !page.Cleared {
			pending++
		}
	}
	return pending
}

// Seal registers a new image that is going to be written chunk by chunk.
func (g *HibernateImageGuard) Seal(image ImageRef, policy SealPolicy) (SealedImage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	if image.ID == "" || image.Pages <= 0 || image.Bytes < 0 {
		return SealedImage{}, ErrBadImage
	}
	if policy.Algorithm == "" {
		return SealedImage{}, ErrPolicyMismatch
	}
	if !policy.Sealed || policy.Retention < 0 {
		return SealedImage{}, ErrBadPolicy
	}
	if _, exists := g.images[image.ID]; exists {
		return SealedImage{}, ErrImageExists
	}
	if g.limits.MaxImages > 0 && len(g.images) >= g.limits.MaxImages {
		return SealedImage{}, ErrImageLimit
	}

	sealed := SealedImage{
		ID:        image.ID,
		Algorithm: policy.Algorithm,
		Pages:     image.Pages,
		Bytes:     image.Bytes,
		Chunks:    []ChunkState{},
		Watermark: 0,
		Policy:    policy,
		SealedAt:  g.now(),
	}
	rec := &imageRecord{image: sealed}
	g.images[image.ID] = rec
	return copyImage(sealed), nil
}

// AppendChunk appends the next chunk and returns its index.
func (g *HibernateImageGuard) AppendChunk(imgID string, data []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return 0, err
	}
	if rec.committed {
		return 0, ErrImageSealed
	}

	img := &rec.image
	if img.Watermark >= img.Pages ||
		(g.limits.MaxChunksPerImage > 0 && img.Watermark >= g.limits.MaxChunksPerImage) {
		return 0, ErrChunkLimit
	}

	chunkSize := int64(len(data))
	if chunkSize < 0 {
		return 0, ErrDiskBudget
	}
	if g.limits.MaxImageBytes > 0 {
		if g.usedBytes > g.limits.MaxImageBytes || chunkSize > g.limits.MaxImageBytes-g.usedBytes {
			return 0, ErrDiskBudget
		}
	}

	sum := sha256.Sum256(data)
	idx := img.Watermark
	chunk := ChunkState{
		Index: idx,
		Size:  len(data),
		Hash:  hex.EncodeToString(sum[:]),
		State: ChunkWritten,
	}
	img.Chunks = append(img.Chunks, chunk)
	img.Watermark++
	g.usedBytes += chunkSize
	return idx, nil
}

// Commit closes the image once every page has been written.
func (g *HibernateImageGuard) Commit(imgID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return err
	}
	if rec.committed {
		return ErrAlreadySealed
	}
	if rec.image.Watermark != rec.image.Pages || len(rec.image.Chunks) != rec.image.Pages {
		return ErrIncomplete
	}
	rec.committed = true
	return nil
}

// Open verifies the key against the header and marks the image open.
func (g *HibernateImageGuard) Open(img SealedImage, key KeyRef) (ImageRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(img.ID)
	if err != nil {
		return ImageRef{}, err
	}
	canonical := &rec.image
	if key.Algorithm != canonical.Algorithm ||
		(img.Algorithm != "" && img.Algorithm != canonical.Algorithm) {
		return ImageRef{}, ErrPolicyMismatch
	}
	if !rec.committed {
		return ImageRef{}, ErrNotSealed
	}
	if g.pendingSensitiveLocked(rec) > 0 {
		return ImageRef{}, ErrSensitivePending
	}
	if canonical.Opened {
		return ImageRef{}, ErrImageBusy
	}
	if canonical.Watermark != canonical.Pages || len(canonical.Chunks) != canonical.Pages {
		return ImageRef{}, ErrNotSealed
	}
	for _, chunk := range canonical.Chunks {
		if chunk.State == ChunkCorrupt {
			return ImageRef{}, ErrChunkCorrupt
		}
		if chunk.State == ChunkPending || chunk.Hash == "" {
			return ImageRef{}, ErrNotSealed
		}
	}

	canonical.Opened = true
	canonical.Completed = true
	return ImageRef{
		ID:    canonical.ID,
		Pages: canonical.Pages,
		Bytes: canonical.Bytes,
	}, nil
}

// Close releases the open marker of one image.
func (g *HibernateImageGuard) Close(imgID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return err
	}
	if !rec.image.Opened {
		return ErrNotOpened
	}
	rec.image.Opened = false
	return nil
}

// VerifyChunk checks one chunk against the chunk table.
func (g *HibernateImageGuard) VerifyChunk(img SealedImage, idx int) (ChunkState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(img.ID)
	if err != nil {
		return ChunkState{}, err
	}
	canonical := &rec.image
	if idx < 0 || idx >= canonical.Watermark || idx >= len(canonical.Chunks) || idx >= len(img.Chunks) {
		return ChunkState{}, ErrChunkRange
	}

	expected := canonical.Chunks[idx]
	if expected.State == ChunkCorrupt {
		return expected, ErrChunkCorrupt
	}
	presented := img.Chunks[idx]
	if presented.Index != expected.Index || presented.Size != expected.Size || presented.Hash != expected.Hash {
		canonical.Chunks[idx].State = ChunkCorrupt
		bad := canonical.Chunks[idx]
		return bad, ErrChunkCorrupt
	}

	canonical.Chunks[idx].State = ChunkVerified
	return canonical.Chunks[idx], nil
}

// Discard drops one image and its chunk table.
func (g *HibernateImageGuard) Discard(imgID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	if _, err := g.imageLocked(imgID); err != nil {
		return err
	}
	delete(g.images, imgID)
	g.recomputeUsageLocked()
	return nil
}

// Chunks returns a copy of the chunk table of one image.
func (g *HibernateImageGuard) Chunks(imgID string) []ChunkState {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return nil
	}
	return append([]ChunkState(nil), rec.image.Chunks...)
}

// Images returns the image table ordered by image id.
func (g *HibernateImageGuard) Images() []SealedImage {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	ids := make([]string, 0, len(g.images))
	for id := range g.images {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]SealedImage, 0, len(ids))
	for _, id := range ids {
		out = append(out, copyImage(g.images[id].image))
	}
	return out
}

// RegisterSensitivePage records a page that must be wiped before wake up ends.
func (g *HibernateImageGuard) RegisterSensitivePage(imgID, page string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return err
	}
	for _, registered := range rec.sensitive {
		if registered.Page == page {
			return ErrDuplicatePage
		}
	}
	rec.sensitive = append(rec.sensitive, sensitivePage{Page: page})
	rec.image.Completed = false
	return nil
}

// ClearSensitivePage records that one sensitive page has been wiped.
func (g *HibernateImageGuard) ClearSensitivePage(imgID, page string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return err
	}
	for i := range rec.sensitive {
		if rec.sensitive[i].Page == page && !rec.sensitive[i].Cleared {
			rec.sensitive[i].Cleared = true
			if rec.image.Opened && g.pendingSensitiveLocked(rec) == 0 {
				rec.image.Completed = true
			}
			return nil
		}
	}
	return ErrUnknownPage
}

// PendingSensitive lists the sensitive pages that are still resident.
func (g *HibernateImageGuard) PendingSensitive(imgID string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	rec, err := g.imageLocked(imgID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rec.sensitive))
	for _, page := range rec.sensitive {
		if !page.Cleared {
			out = append(out, page.Page)
		}
	}
	return out
}

func encodeSnapshot(state snapshotState) ([]byte, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	out := make([]byte, 0, len(snapshotMagic)+hex.EncodedLen(len(sum))+1+len(payload))
	out = append(out, snapshotMagic...)
	out = append(out, []byte(hex.EncodeToString(sum[:]))...)
	out = append(out, '\n')
	out = append(out, payload...)
	return out, nil
}

func decodeSnapshot(data []byte) (snapshotState, error) {
	var state snapshotState
	if !bytes.HasPrefix(data, []byte(snapshotMagic)) {
		return state, ErrBadSnapshot
	}
	rest := data[len(snapshotMagic):]
	newline := bytes.IndexByte(rest, '\n')
	if newline < 0 {
		return state, ErrBadSnapshot
	}
	checksum, err := hex.DecodeString(string(rest[:newline]))
	if err != nil || len(checksum) != sha256.Size {
		return state, ErrBadSnapshot
	}
	payload := rest[newline+1:]
	actual := sha256.Sum256(payload)
	if !bytes.Equal(actual[:], checksum) {
		return state, ErrBadSnapshot
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return snapshotState{}, ErrBadSnapshot
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return snapshotState{}, ErrBadSnapshot
	}
	return state, nil
}

// Snapshot serialises images, chunk tables and the sensitive page ledger.
func (g *HibernateImageGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.expireLocked(g.now())

	ids := make([]string, 0, len(g.images))
	for id := range g.images {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	state := snapshotState{
		Version: snapshotVersion,
		Images:  make([]snapshotImage, 0, len(ids)),
	}
	for _, id := range ids {
		rec := g.images[id]
		state.Images = append(state.Images, snapshotImage{
			Image:     copyImage(rec.image),
			Committed: rec.committed,
			Sensitive: copySensitive(rec.sensitive),
		})
	}
	return encodeSnapshot(state)
}

func validChunkState(state string) bool {
	switch state {
	case ChunkWritten, ChunkVerified, ChunkCorrupt:
		return true
	default:
		return false
	}
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *HibernateImageGuard) Restore(data []byte) error {
	state, err := decodeSnapshot(data)
	if err != nil || state.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	now := g.now()
	next := make(map[string]*imageRecord, len(state.Images))
	var used int64

	for _, saved := range state.Images {
		img := saved.Image
		if img.ID == "" || img.Pages <= 0 || img.Bytes < 0 ||
			img.Algorithm == "" || img.Policy.Algorithm != img.Algorithm ||
			!img.Policy.Sealed || img.Policy.Retention < 0 || img.SealedAt.IsZero() {
			return ErrBadSnapshot
		}
		if _, exists := next[img.ID]; exists {
			return ErrBadSnapshot
		}
		if img.Watermark < 0 || img.Watermark > img.Pages {
			return ErrBadSnapshot
		}
		if len(img.Chunks) > img.Pages {
			return ErrBadSnapshot
		}

		// The watermark is authoritative. Anything beyond it was not committed
		// to the table. Within the watermark, stop at the first incomplete slot
		// so a partially written chunk can never be mistaken for a full one.
		watermark := img.Watermark
		if watermark > len(img.Chunks) {
			watermark = len(img.Chunks)
		}
		for i := 0; i < watermark; i++ {
			chunk := img.Chunks[i]
			if chunk.Index != i || chunk.Size < 0 || chunk.Hash == "" || !validChunkState(chunk.State) {
				watermark = i
				break
			}
		}
		img.Chunks = append([]ChunkState(nil), img.Chunks[:watermark]...)
		img.Watermark = watermark

		// A committed image cannot be recovered from a short chunk table. It is
		// safer to invalidate the whole image than to decrypt a half image.
		if saved.Committed && (img.Watermark != img.Pages || len(img.Chunks) != img.Pages) {
			continue
		}

		img.Opened = false
		if !saved.Committed {
			img.Completed = false
		}

		pages := copySensitive(saved.Sensitive)
		seenPages := make(map[string]struct{}, len(pages))
		for _, page := range pages {
			if _, seen := seenPages[page.Page]; seen {
				return ErrBadSnapshot
			}
			seenPages[page.Page] = struct{}{}
		}
		rec := &imageRecord{
			image:     img,
			committed: saved.Committed,
			sensitive: pages,
		}
		if g.pendingSensitiveLocked(rec) > 0 {
			rec.image.Completed = false
		}
		if g.limits.MaxChunksPerImage > 0 && rec.image.Watermark > g.limits.MaxChunksPerImage {
			return ErrBadSnapshot
		}
		if g.expiredLocked(rec, now) {
			continue
		}
		for _, chunk := range rec.image.Chunks {
			if chunk.Size > 0 {
				used += int64(chunk.Size)
			}
		}
		next[img.ID] = rec
	}

	if g.limits.MaxImages > 0 && len(next) > g.limits.MaxImages {
		return ErrBadSnapshot
	}
	if g.limits.MaxImageBytes > 0 && used > g.limits.MaxImageBytes {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.images = next
	g.usedBytes = used
	return nil
}
