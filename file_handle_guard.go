// Package task governs opens that go through a file handle instead of a path:
// the handle carries mount, inode and generation, the guard re-resolves the
// path before admitting the open and keeps an authorisation ledger.
package task

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

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

type mountRecord struct {
	Mount
	epoch uint64
}

type handleRecord struct {
	Handle
	mountEpoch uint64
	real       bool
	lastUsed   uint64
}

type ownerRecord struct {
	Owner
	links     map[string]uint32
	linkOrder []string
}

type grantRecord struct {
	Grant
	seq uint64
}

type openRecord struct {
	File
	seq uint64
}

type resolvedHandle struct {
	Handle
	real       bool
	mountEpoch uint64
}

// FileHandleGuard owns the mount table, the handle cache, the owner table, the
// grant ledger and the open counters.
type FileHandleGuard struct {
	mu sync.Mutex

	limits  Limits
	allowed map[uint64]struct{}

	mounts map[uint64]*mountRecord

	records map[string]*handleRecord
	cache   map[string]string

	owners  map[string]*ownerRecord
	revoked map[string]bool

	grants []grantRecord
	opens  []openRecord

	mountSeq uint64
	touchSeq uint64
	grantSeq uint64
	openSeq  uint64
}

// NewFileHandleGuard builds a guard with the given caps.
func NewFileHandleGuard(limits Limits) *FileHandleGuard {
	limits.AllowedMounts = append([]uint64(nil), limits.AllowedMounts...)
	g := &FileHandleGuard{
		limits:  limits,
		allowed: make(map[uint64]struct{}, len(limits.AllowedMounts)),
		mounts:  make(map[uint64]*mountRecord),
		records: make(map[string]*handleRecord),
		cache:   make(map[string]string),
		owners:  make(map[string]*ownerRecord),
		revoked: make(map[string]bool),
	}
	for _, id := range limits.AllowedMounts {
		g.allowed[id] = struct{}{}
	}
	return g
}

// AddMount registers one mount prefix that Lookup resolves against.
func (g *FileHandleGuard) AddMount(id uint64, prefix, namespace string, generationSupported bool) error {
	clean, ok := canonicalPath(prefix)
	if id == 0 || !ok {
		return ErrBadMount
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	old, found := g.mounts[id]
	if !found {
		g.mountSeq++
		g.mounts[id] = &mountRecord{
			Mount: Mount{
				ID:                  id,
				Prefix:              clean,
				Namespace:           namespace,
				GenerationSupported: generationSupported,
			},
			epoch: g.mountSeq,
		}
		return nil
	}

	// A namespace-only change models mount propagation.  The file identity is
	// unchanged, so the old handle remains meaningful but the target namespace
	// moves.  Anything else is a new mount incarnation.
	if old.Prefix == clean && old.GenerationSupported == generationSupported && old.Namespace != namespace {
		old.Namespace = namespace
		return nil
	}

	g.mountSeq++
	old.Mount = Mount{
		ID:                  id,
		Prefix:              clean,
		Namespace:           namespace,
		GenerationSupported: generationSupported,
	}
	old.epoch = g.mountSeq
	return nil
}

// Lookup resolves a path into a handle.
func (g *FileHandleGuard) Lookup(path string) (Handle, error) {
	clean, ok := canonicalPath(path)
	if !ok {
		return Handle{}, ErrBadPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	m := g.matchMountLocked(clean)
	if m == nil {
		return Handle{}, ErrMountDenied
	}
	if _, ok := g.allowed[m.ID]; !ok {
		return Handle{}, ErrMountDenied
	}

	cur := g.resolveLocked(clean, m)
	key := handleKey(cur.Handle)

	if cachedKey := g.cache[clean]; cachedKey != "" {
		if rec := g.records[cachedKey]; rec != nil && handleKey(rec.Handle) == key {
			rec.mountEpoch = cur.mountEpoch
			rec.real = cur.real
			g.touchLocked(rec)
			g.rememberAliasLocked(cur.Handle)
			return rec.Handle, nil
		}
	}

	rec := g.records[key]
	if rec == nil {
		rec = &handleRecord{Handle: cur.Handle}
		g.records[key] = rec
	}
	rec.mountEpoch = cur.mountEpoch
	rec.real = cur.real
	g.touchLocked(rec)
	g.cache[clean] = key
	g.rememberAliasLocked(cur.Handle)
	g.evictCacheLocked()
	return rec.Handle, nil
}

// Register records the owner of one handle.
func (g *FileHandleGuard) Register(h Handle, owner Owner) error {
	if owner.Tenant == "" || owner.Mode == 0 {
		return ErrBadOwner
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	id := identityKey(h)
	rec := g.records[handleKey(h)]
	if existing := g.owners[id]; existing != nil {
		if existing.Tenant != owner.Tenant || existing.UID != owner.UID || existing.GID != owner.GID {
			return ErrAlreadyOwned
		}
		oldMode, alreadyLinked := existing.links[h.Path]
		if rec == nil && !alreadyLinked {
			return ErrNoHandle
		}
		if alreadyLinked {
			// Re-registration never loosens an existing link.
			existing.links[h.Path] = oldMode & owner.Mode
		} else {
			existing.links[h.Path] = owner.Mode
			existing.linkOrder = append(existing.linkOrder, h.Path)
		}
		if rec != nil {
			g.touchLocked(rec)
		}
		return nil
	}

	if rec == nil {
		return ErrNoHandle
	}

	g.owners[id] = &ownerRecord{
		Owner:     owner,
		links:     map[string]uint32{h.Path: owner.Mode},
		linkOrder: []string{h.Path},
	}
	if rec := g.records[handleKey(h)]; rec != nil {
		g.touchLocked(rec)
	}
	return nil
}

// Open admits or refuses one handle based open.
func (g *FileHandleGuard) Open(h Handle, flags int, caller Caller) (File, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := identityKey(h)
	if g.revoked[id] {
		return File{}, ErrRevoked
	}

	exact := g.records[handleKey(h)]
	pathRec := g.pathRecordLocked(h.Path)
	owner := g.owners[id]
	known := exact != nil || pathRec != nil || owner != nil
	if !known {
		// A handle whose cache entry was evicted is still usable if its
		// identity resolves correctly; a forged handle is not.
		if _, err := g.validateLocked(h); err != nil {
			return File{}, ErrNoHandle
		}
	}

	cur, err := g.validateLocked(h)
	if err != nil {
		return File{}, err
	}
	if exact != nil && exact.mountEpoch != cur.mountEpoch {
		return File{}, ErrStaleHandle
	}
	if g.revoked[identityKey(cur.Handle)] {
		return File{}, ErrRevoked
	}

	owner = g.owners[identityKey(cur.Handle)]
	if owner == nil {
		return File{}, ErrNotRegistered
	}
	if caller.Tenant != owner.Tenant {
		return File{}, ErrOwnerMismatch
	}

	needRead, needWrite, accessOK := accessForFlags(flags)
	if !accessOK {
		return File{}, ErrPermissionDenied
	}
	readOK, writeOK := g.effectiveAccessLocked(owner, identityKey(cur.Handle), cur.Handle)
	if needRead && !readOK {
		return File{}, ErrPermissionDenied
	}
	if needWrite && !writeOK {
		return File{}, ErrPermissionDenied
	}

	if cur.Namespace != caller.Namespace {
		if err := g.checkGrantLocked(identityKey(cur.Handle), caller.Tenant, time.Now()); err != nil {
			return File{}, err
		}
	}

	if max := g.limits.MaxOpensPerTenant; max > 0 && g.openCountLocked(caller.Tenant) >= max {
		return File{}, ErrOpenLimit
	}

	// Re-resolve immediately before admission, then pin the inode with a real
	// open where the path exists, and re-check after that open.  A rename or
	// remount that changes the target rolls the admission back.
	final, err := g.validateLocked(h)
	if err != nil {
		return File{}, err
	}
	if identityKey(final.Handle) != identityKey(cur.Handle) || final.mountEpoch != cur.mountEpoch {
		return File{}, ErrStaleHandle
	}
	if exact != nil && exact.mountEpoch != final.mountEpoch {
		return File{}, ErrStaleHandle
	}
	if err := settleResolved(final, needRead, needWrite); err != nil {
		return File{}, err
	}
	post, err := g.validateLocked(h)
	if err != nil {
		return File{}, err
	}
	if identityKey(post.Handle) != identityKey(final.Handle) || post.mountEpoch != final.mountEpoch {
		return File{}, ErrStaleHandle
	}
	if g.revoked[identityKey(post.Handle)] {
		return File{}, ErrRevoked
	}

	g.openSeq++
	f := File{
		Handle:   post.Handle,
		Tenant:   caller.Tenant,
		Flags:    flags,
		OpenedAt: time.Now(),
	}
	g.opens = append(g.opens, openRecord{File: f, seq: g.openSeq})
	if rec := g.records[handleKey(post.Handle)]; rec != nil {
		g.touchLocked(rec)
	}
	return f, nil
}

// Revoke fences a handle off and keeps the already opened files listed.
func (g *FileHandleGuard) Revoke(h Handle) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := identityKey(h)
	if g.revoked[id] {
		return nil
	}
	exact := g.records[handleKey(h)]
	if exact == nil && g.owners[id] == nil {
		return ErrNoHandle
	}
	g.revoked[id] = true
	if exact != nil {
		g.touchLocked(exact)
	}
	return nil
}

// Verify re-resolves the handle path and reports whether it still matches.
func (g *FileHandleGuard) Verify(h Handle) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := identityKey(h)
	if g.revoked[id] {
		return ErrRevoked
	}
	exact := g.records[handleKey(h)]
	boolKnown := exact != nil || g.pathRecordLocked(h.Path) != nil || g.owners[id] != nil
	if !boolKnown {
		if _, err := g.validateLocked(h); err != nil {
			return ErrNoHandle
		}
	}

	cur, err := g.validateLocked(h)
	if err != nil {
		return err
	}
	if exact != nil && exact.mountEpoch != cur.mountEpoch {
		return ErrStaleHandle
	}
	if g.revoked[identityKey(cur.Handle)] {
		return ErrRevoked
	}
	if exact != nil {
		g.touchLocked(exact)
	}
	return nil
}

// Grant writes one authorisation line for a foreign tenant or namespace.
func (g *FileHandleGuard) Grant(h Handle, tenant string, expires time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := identityKey(h)
	exact := g.records[handleKey(h)]
	if exact == nil && g.owners[id] == nil && !g.revoked[id] {
		return ErrNoHandle
	}
	if expires.IsZero() {
		expires = time.Now().Add(g.limits.GrantTTL)
	}

	g.grantSeq++
	g.grants = append(g.grants, grantRecord{
		Grant: Grant{
			Handle:  h,
			Tenant:  tenant,
			CrossNS: true,
			Expires: expires,
		},
		seq: g.grantSeq,
	})
	if exact != nil {
		g.touchLocked(exact)
	}
	return nil
}

// Grants lists the ledger lines of one tenant, oldest first.
func (g *FileHandleGuard) Grants(tenant string) []Grant {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]Grant, 0)
	for _, rec := range g.grants {
		if rec.Tenant == tenant {
			out = append(out, rec.Grant)
		}
	}
	return out
}

// OpenedFiles lists the admitted opens of one tenant, oldest first.
func (g *FileHandleGuard) OpenedFiles(tenant string) []File {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]File, 0)
	for _, rec := range g.opens {
		if rec.Tenant == tenant {
			out = append(out, rec.File)
		}
	}
	return out
}

// OpenCount reports how many opens one tenant holds.
func (g *FileHandleGuard) OpenCount(tenant string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.openCountLocked(tenant)
}

func (g *FileHandleGuard) openCountLocked(tenant string) int {
	n := 0
	for _, rec := range g.opens {
		if rec.Tenant == tenant {
			n++
		}
	}
	return n
}

type exportMount struct {
	Mount
	Epoch uint64 `json:"epoch"`
}

type exportHandle struct {
	Handle
	MountEpoch uint64 `json:"mount_epoch"`
	LastUsed   uint64 `json:"last_used"`
	Real       bool   `json:"real,omitempty"`
}

type exportCachePath struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

type exportOwner struct {
	Identity string `json:"identity"`
	Owner
	Links []exportOwnerLink `json:"links"`
}

type exportOwnerLink struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type exportGrant struct {
	Grant
	Seq uint64 `json:"seq"`
}

type exportOpen struct {
	File
	Seq uint64 `json:"seq"`
}

type exportSnapshot struct {
	Version  int               `json:"version"`
	Mounts   []exportMount     `json:"mounts"`
	Handles  []exportHandle    `json:"handles"`
	Cache    []exportCachePath `json:"cache"`
	Owners   []exportOwner     `json:"owners"`
	Revoked  []string          `json:"revoked"`
	Grants   []exportGrant     `json:"grants"`
	Opens    []exportOpen      `json:"opens"`
	MountSeq uint64            `json:"mount_seq"`
	TouchSeq uint64            `json:"touch_seq"`
	GrantSeq uint64            `json:"grant_seq"`
	OpenSeq  uint64            `json:"open_seq"`
}

// Export serialises the mount table, owners, grants, revocations and opens.
func (g *FileHandleGuard) Export() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	snap := exportSnapshot{
		Version:  1,
		MountSeq: g.mountSeq,
		TouchSeq: g.touchSeq,
		GrantSeq: g.grantSeq,
		OpenSeq:  g.openSeq,
	}

	mountIDs := make([]uint64, 0, len(g.mounts))
	for id := range g.mounts {
		mountIDs = append(mountIDs, id)
	}
	sort.Slice(mountIDs, func(i, j int) bool { return mountIDs[i] < mountIDs[j] })
	for _, id := range mountIDs {
		m := g.mounts[id]
		snap.Mounts = append(snap.Mounts, exportMount{Mount: m.Mount, Epoch: m.epoch})
	}

	handleKeys := make([]string, 0, len(g.records))
	for key := range g.records {
		handleKeys = append(handleKeys, key)
	}
	sort.Strings(handleKeys)
	for _, key := range handleKeys {
		rec := g.records[key]
		snap.Handles = append(snap.Handles, exportHandle{
			Handle:     rec.Handle,
			MountEpoch: rec.mountEpoch,
			LastUsed:   rec.lastUsed,
			Real:       rec.real,
		})
	}

	cachePaths := make([]string, 0, len(g.cache))
	for p := range g.cache {
		cachePaths = append(cachePaths, p)
	}
	sort.Strings(cachePaths)
	for _, p := range cachePaths {
		snap.Cache = append(snap.Cache, exportCachePath{Path: p, Key: g.cache[p]})
	}

	ownerIDs := make([]string, 0, len(g.owners))
	for id := range g.owners {
		ownerIDs = append(ownerIDs, id)
	}
	sort.Strings(ownerIDs)
	for _, id := range ownerIDs {
		rec := g.owners[id]
		links := make([]exportOwnerLink, 0, len(rec.links))
		for p, mode := range rec.links {
			links = append(links, exportOwnerLink{Path: p, Mode: mode})
		}
		sort.Slice(links, func(i, j int) bool { return links[i].Path < links[j].Path })
		snap.Owners = append(snap.Owners, exportOwner{
			Identity: id,
			Owner:    rec.Owner,
			Links:    links,
		})
	}

	for id := range g.revoked {
		snap.Revoked = append(snap.Revoked, id)
	}
	sort.Strings(snap.Revoked)

	for _, rec := range g.grants {
		snap.Grants = append(snap.Grants, exportGrant{Grant: rec.Grant, Seq: rec.seq})
	}
	for _, rec := range g.opens {
		snap.Opens = append(snap.Opens, exportOpen{File: rec.File, Seq: rec.seq})
	}

	return json.Marshal(snap)
}

// Import replaces the whole state with an exported payload, all or nothing.
func (g *FileHandleGuard) Import(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}

	var snap exportSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}
	if snap.Version != 1 {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	mounts := make(map[uint64]*mountRecord, len(snap.Mounts))
	for _, em := range snap.Mounts {
		clean, ok := canonicalPath(em.Prefix)
		if em.ID == 0 || !ok || clean != em.Prefix || em.Epoch == 0 {
			return ErrBadSnapshot
		}
		if _, dup := mounts[em.ID]; dup {
			return ErrBadSnapshot
		}
		mounts[em.ID] = &mountRecord{
			Mount: Mount{
				ID:                  em.ID,
				Prefix:              clean,
				Namespace:           em.Namespace,
				GenerationSupported: em.GenerationSupported,
			},
			epoch: em.Epoch,
		}
	}

	records := make(map[string]*handleRecord, len(snap.Handles))
	cache := make(map[string]string, len(snap.Cache))
	for _, eh := range snap.Handles {
		h := eh.Handle
		clean, ok := canonicalPath(h.Path)
		if !ok || clean != h.Path || h.Inode == 0 {
			return ErrBadSnapshot
		}
		m, ok := mounts[h.MountID]
		if !ok {
			return ErrBadSnapshot
		}
		key := handleKey(h)
		if _, dup := records[key]; dup {
			return ErrBadSnapshot
		}
		epoch := eh.MountEpoch
		if epoch == 0 {
			epoch = m.epoch
		}
		records[key] = &handleRecord{
			Handle:     h,
			mountEpoch: epoch,
			real:       eh.Real,
			lastUsed:   eh.LastUsed,
		}
	}

	for _, ec := range snap.Cache {
		clean, ok := canonicalPath(ec.Path)
		if !ok || clean != ec.Path {
			return ErrBadSnapshot
		}
		if _, ok := records[ec.Key]; !ok {
			return ErrBadSnapshot
		}
		cache[clean] = ec.Key
	}
	// Fill in any cache entries the exporter could not name explicitly, and
	// prefer the most recently used record when both forms are present.
	for key, rec := range records {
		existing, ok := cache[rec.Path]
		if ok {
			old := records[existing]
			if old != nil && old.lastUsed >= rec.lastUsed {
				continue
			}
		}
		cache[rec.Path] = key
	}

	owners := make(map[string]*ownerRecord, len(snap.Owners))
	for _, eo := range snap.Owners {
		if eo.Identity == "" || eo.Tenant == "" || eo.Mode == 0 {
			return ErrBadSnapshot
		}
		if _, dup := owners[eo.Identity]; dup {
			return ErrBadSnapshot
		}
		links := make(map[string]uint32, len(eo.Links))
		order := make([]string, 0, len(eo.Links))
		for _, l := range eo.Links {
			clean, ok := canonicalPath(l.Path)
			if !ok || clean != l.Path {
				return ErrBadSnapshot
			}
			links[clean] = l.Mode
			order = append(order, clean)
		}
		owners[eo.Identity] = &ownerRecord{
			Owner:     eo.Owner,
			links:     links,
			linkOrder: order,
		}
	}

	revoked := make(map[string]bool, len(snap.Revoked))
	for _, id := range snap.Revoked {
		if id == "" {
			return ErrBadSnapshot
		}
		revoked[id] = true
	}

	grants := make([]grantRecord, 0, len(snap.Grants))
	grantSeq := snap.GrantSeq
	for _, eg := range snap.Grants {
		h := eg.Handle
		if h.Path != "" {
			clean, ok := canonicalPath(h.Path)
			if !ok || clean != h.Path {
				return ErrBadSnapshot
			}
		}
		seq := eg.Seq
		if seq == 0 {
			grantSeq++
			seq = grantSeq
		}
		if seq > grantSeq {
			grantSeq = seq
		}
		grants = append(grants, grantRecord{Grant: eg.Grant, seq: seq})
	}
	sort.SliceStable(grants, func(i, j int) bool { return grants[i].seq < grants[j].seq })

	opens := make([]openRecord, 0, len(snap.Opens))
	openSeq := snap.OpenSeq
	for _, eo := range snap.Opens {
		h := eo.Handle
		if h.Path != "" {
			clean, ok := canonicalPath(h.Path)
			if !ok || clean != h.Path {
				return ErrBadSnapshot
			}
		}
		seq := eo.Seq
		if seq == 0 {
			openSeq++
			seq = openSeq
		}
		if seq > openSeq {
			openSeq = seq
		}
		opens = append(opens, openRecord{File: eo.File, seq: seq})
	}
	sort.SliceStable(opens, func(i, j int) bool { return opens[i].seq < opens[j].seq })

	touchSeq := snap.TouchSeq
	for _, rec := range records {
		if rec.lastUsed > touchSeq {
			touchSeq = rec.lastUsed
		}
	}
	mountSeq := snap.MountSeq
	for _, m := range mounts {
		if m.epoch > mountSeq {
			mountSeq = m.epoch
		}
	}

	// Honour caps even when importing a payload built under wider limits.
	if g.limits.MaxOpensPerTenant > 0 {
		for {
			over := false
			counts := make(map[string]int)
			for _, rec := range opens {
				counts[rec.Tenant]++
				if counts[rec.Tenant] > g.limits.MaxOpensPerTenant {
					over = true
					break
				}
			}
			if !over {
				break
			}
			// Drop the oldest open of the first tenant that is over its cap.
			removeAt := -1
			for i, rec := range opens {
				if counts[rec.Tenant] > g.limits.MaxOpensPerTenant {
					if removeAt == -1 || opens[removeAt].seq > rec.seq {
						removeAt = i
					}
				}
			}
			if removeAt < 0 {
				break
			}
			opens = append(opens[:removeAt], opens[removeAt+1:]...)
			counts = nil
		}
	}
	if g.limits.MaxCache > 0 {
		for len(records) > g.limits.MaxCache {
			oldestKey := ""
			var oldest uint64
			first := true
			for key, rec := range records {
				if first || rec.lastUsed < oldest || (rec.lastUsed == oldest && key < oldestKey) {
					oldestKey = key
					oldest = rec.lastUsed
					first = false
				}
			}
			delete(records, oldestKey)
			for p, key := range cache {
				if key == oldestKey {
					delete(cache, p)
				}
			}
		}
	}

	g.mounts = mounts
	g.records = records
	g.cache = cache
	g.owners = owners
	g.revoked = revoked
	g.grants = grants
	g.opens = opens
	g.mountSeq = mountSeq
	g.touchSeq = touchSeq
	g.grantSeq = grantSeq
	g.openSeq = openSeq
	return nil
}

func canonicalPath(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) == "" || !strings.HasPrefix(raw, "/") {
		return "", false
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return "", false
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", false
		}
	}
	clean := pathpkg.Clean(raw)
	if !strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

func (g *FileHandleGuard) matchMountLocked(p string) *mountRecord {
	var best *mountRecord
	for _, m := range g.mounts {
		prefix := m.Prefix
		if prefix != "/" && p != prefix && !strings.HasPrefix(p, prefix+"/") {
			continue
		}
		if best == nil || len(prefix) > len(best.Prefix) ||
			(len(prefix) == len(best.Prefix) && m.epoch > best.epoch) {
			best = m
		}
	}
	return best
}

func (g *FileHandleGuard) resolveLocked(p string, m *mountRecord) resolvedHandle {
	var (
		inode uint64
		info  os.FileInfo
		real  bool
	)

	if fi, err := os.Stat(p); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Ino != 0 {
			inode = uint64(st.Ino)
			info = fi
			real = true
		}
	}
	if inode == 0 {
		inode = syntheticInode(m, p)
	}

	var generation uint64
	if m.GenerationSupported {
		if real {
			generation = realGeneration(m.epoch, inode, info)
		} else {
			generation = syntheticGeneration(m, p, inode)
		}
	}

	var digest string
	if real {
		digest = realDigest(p, info, m.ID, inode)
	} else {
		digest = syntheticDigest(m.ID, inode)
	}

	return resolvedHandle{
		Handle: Handle{
			MountID:    m.ID,
			Inode:      inode,
			Generation: generation,
			Path:       p,
			Namespace:  m.Namespace,
			Digest:     digest,
		},
		real:       real,
		mountEpoch: m.epoch,
	}
}

func (g *FileHandleGuard) validateLocked(h Handle) (resolvedHandle, error) {
	clean, ok := canonicalPath(h.Path)
	if !ok || clean != h.Path {
		return resolvedHandle{}, ErrStaleHandle
	}
	m := g.matchMountLocked(clean)
	if m == nil {
		return resolvedHandle{}, ErrStaleHandle
	}
	if _, ok := g.allowed[m.ID]; !ok {
		return resolvedHandle{}, ErrStaleHandle
	}

	cur := g.resolveLocked(clean, m)
	if h.MountID != cur.MountID ||
		h.Inode != cur.Inode ||
		h.Generation != cur.Generation ||
		h.Digest != cur.Digest {
		return resolvedHandle{}, ErrStaleHandle
	}
	return cur, nil
}

func (g *FileHandleGuard) pathRecordLocked(raw string) *handleRecord {
	clean, ok := canonicalPath(raw)
	if !ok {
		return nil
	}
	if key := g.cache[clean]; key != "" {
		if rec := g.records[key]; rec != nil {
			return rec
		}
	}
	var best *handleRecord
	for _, rec := range g.records {
		if rec.Path != clean {
			continue
		}
		if best == nil || rec.lastUsed > best.lastUsed {
			best = rec
		}
	}
	return best
}

func (g *FileHandleGuard) touchLocked(rec *handleRecord) {
	g.touchSeq++
	rec.lastUsed = g.touchSeq
}

func (g *FileHandleGuard) rememberAliasLocked(h Handle) {
	owner := g.owners[identityKey(h)]
	if owner == nil {
		return
	}
	if _, ok := owner.links[h.Path]; !ok {
		owner.links[h.Path] = owner.Mode
		owner.linkOrder = append(owner.linkOrder, h.Path)
	}
}

func (g *FileHandleGuard) evictCacheLocked() {
	if g.limits.MaxCache <= 0 {
		return
	}
	for len(g.records) > g.limits.MaxCache {
		oldestKey := ""
		var oldest uint64
		first := true
		for key, rec := range g.records {
			if first || rec.lastUsed < oldest || (rec.lastUsed == oldest && key < oldestKey) {
				oldestKey = key
				oldest = rec.lastUsed
				first = false
			}
		}
		delete(g.records, oldestKey)
		for p, key := range g.cache {
			if key == oldestKey {
				delete(g.cache, p)
			}
		}
	}
}

func (g *FileHandleGuard) effectiveAccessLocked(owner *ownerRecord, id string, current Handle) (bool, bool) {
	paths := make(map[string]uint32, len(owner.links))
	for p, mode := range owner.links {
		paths[p] = mode
	}
	for _, rec := range g.records {
		if identityKey(rec.Handle) != id {
			continue
		}
		if _, ok := paths[rec.Path]; !ok {
			paths[rec.Path] = owner.Mode
		}
	}
	if len(paths) == 0 {
		paths[current.Path] = owner.Mode
	}

	readOK, writeOK := true, true
	for p, mode := range paths {
		if mode&PermRead == 0 {
			readOK = false
		}
		if mode&PermWrite == 0 {
			writeOK = false
		}
		dirRead, dirWrite := directoryAccess(p)
		if !dirRead {
			readOK = false
		}
		if !dirWrite {
			writeOK = false
		}
	}
	return readOK, writeOK
}

func (g *FileHandleGuard) checkGrantLocked(id, tenant string, now time.Time) error {
	found, valid := false, false
	for _, rec := range g.grants {
		if rec.Tenant != tenant || identityKey(rec.Handle) != id {
			continue
		}
		found = true
		if rec.Expires.After(now) {
			valid = true
		}
	}
	if valid {
		return nil
	}
	if found {
		return ErrGrantExpired
	}
	return ErrNamespaceDenied
}

func accessForFlags(flags int) (needRead, needWrite, ok bool) {
	switch flags {
	case AccessRead:
		return true, false, true
	case AccessWrite:
		return false, true, true
	case AccessBoth:
		return true, true, true
	default:
		return false, false, false
	}
}

func directoryAccess(p string) (readOK, writeOK bool) {
	readOK, writeOK = true, true
	dir := pathpkg.Dir(p)
	if dir == p {
		dir = "/"
	}
	for {
		if info, err := os.Stat(dir); err == nil {
			if info.IsDir() {
				perm := info.Mode().Perm()
				if perm&0o444 == 0 || perm&0o111 == 0 {
					readOK = false
				}
				if perm&0o222 == 0 || perm&0o111 == 0 {
					writeOK = false
				}
			}
		}
		if dir == "/" {
			break
		}
		next := pathpkg.Dir(dir)
		if next == dir || next == "" || next == "." {
			break
		}
		dir = next
	}
	return readOK, writeOK
}

func settleResolved(r resolvedHandle, needRead, needWrite bool) error {
	if !r.real {
		return nil
	}
	flags := os.O_RDONLY
	if needRead && needWrite {
		flags = os.O_RDWR
	} else if needWrite {
		flags = os.O_WRONLY
	}
	f, err := os.OpenFile(r.Path, flags, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrStaleHandle
		}
		if errors.Is(err, os.ErrPermission) {
			return ErrPermissionDenied
		}
		return ErrStaleHandle
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ErrStaleHandle
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(st.Ino) != r.Inode {
		return ErrStaleHandle
	}
	return nil
}

func handleKey(h Handle) string {
	return fmt.Sprintf("%d|%d|%d|%q|%q|%q", h.MountID, h.Inode, h.Generation, h.Path, h.Namespace, h.Digest)
}

func identityKey(h Handle) string {
	return fmt.Sprintf("%d|%d|%d|%q", h.MountID, h.Inode, h.Generation, h.Digest)
}

func syntheticInode(m *mountRecord, p string) uint64 {
	v := hash64(fmt.Sprintf("inode|%d|%d|%q", m.ID, m.epoch, p))
	return v | (uint64(1) << 63)
}

func syntheticGeneration(m *mountRecord, p string, inode uint64) uint64 {
	return hash64(fmt.Sprintf("generation|%d|%d|%d|%q", m.ID, m.epoch, inode, p))
}

func syntheticDigest(mountID, inode uint64) string {
	return sha256String(fmt.Sprintf("content|%d|%d", mountID, inode))
}

func realGeneration(epoch, inode uint64, info os.FileInfo) uint64 {
	seed := fmt.Sprintf("generation|%d|%d|%d|%d|%d",
		epoch, inode, info.Size(), info.ModTime().UnixNano(), info.Mode())
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		seed += fmt.Sprintf("|%d|%d|%d|%d", st.Ctim.Sec, st.Ctim.Nsec, st.Dev, st.Mode)
	}
	return hash64(seed)
}

func realDigest(p string, info os.FileInfo, mountID, inode uint64) string {
	if info.Mode().IsRegular() {
		if f, err := os.Open(p); err == nil {
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			closeErr := f.Close()
			if copyErr == nil && closeErr == nil {
				return hex.EncodeToString(h.Sum(nil))
			}
		}
	}
	return sha256String(fmt.Sprintf("metadata|%d|%d|%d|%d|%d",
		mountID, inode, info.Size(), info.Mode(), info.ModTime().UnixNano()))
}

func hash64(s string) uint64 {
	sum := sha256.Sum256([]byte(s))
	v := binary.BigEndian.Uint64(sum[:8])
	if v == 0 {
		return 1
	}
	return v
}

func sha256String(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
