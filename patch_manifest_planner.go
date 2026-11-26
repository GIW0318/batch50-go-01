// Package task plans client side patch downloads: it diffs two versions file
// by file, slices the transferred files into fixed size chunks and tracks the
// confirmed chunks of every client across restarts.
package task

import "errors"

// errNotImplemented marks the behaviours that still have to be written.
var errNotImplemented = errors.New("not implemented")

// File actions inside one manifest.
const (
	ActionKeep    = "keep"
	ActionAdd     = "add"
	ActionReplace = "replace"
	ActionRemove  = "remove"
)

// Errors returned by the planner.
var (
	ErrBadVersion       = errors.New("version pair is invalid")
	ErrBadFile          = errors.New("file entry is invalid")
	ErrFileLimit        = errors.New("file limit exceeded")
	ErrManifestLimit    = errors.New("manifest limit reached")
	ErrManifestNotFound = errors.New("manifest not found")
	ErrBadClient        = errors.New("client id is required")
	ErrVersionPinned    = errors.New("client is pinned to another manifest")
	ErrDiskFull         = errors.New("client disk space is short")
	ErrBadChunk         = errors.New("chunk is not part of this file plan")
	ErrNoPlan           = errors.New("client has no download plan")
	ErrAlreadyPaused    = errors.New("download is already paused")
	ErrNotPaused        = errors.New("download is not paused")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Limits bounds one planner.
type Limits struct {
	MaxFiles     int
	ChunkSize    int
	MaxManifests int
	MaxClients   int
}

// FileEntry is one file of a version.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}

// ChunkRef is one fixed size slice of a transferred file.
type ChunkRef struct {
	Path   string `json:"path"`
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	Hash   string `json:"hash"`
}

// FilePlan is the decision taken for one path.
type FilePlan struct {
	Path   string     `json:"path"`
	Action string     `json:"action"`
	Size   int64      `json:"size"`
	Hash   string     `json:"hash"`
	Chunks []ChunkRef `json:"chunks"`
}

// Manifest is the published patch list of one version pair.
type Manifest struct {
	ID          string     `json:"id"`
	FromVersion string     `json:"from_version"`
	ToVersion   string     `json:"to_version"`
	Files       []FilePlan `json:"files"`
	Chunks      int        `json:"chunks"`
	TotalBytes  int64      `json:"total_bytes"`
	Seq         int64      `json:"seq"`
}

// DownloadPlan is the set of chunks one client still has to fetch.
type DownloadPlan struct {
	ManifestID string     `json:"manifest_id"`
	ClientID   string     `json:"client_id"`
	Chunks     []ChunkRef `json:"chunks"`
	Resume     bool       `json:"resume"`
	Seq        int64      `json:"seq"`
}

// Progress is the confirmed chunk count of one file for one client.
type Progress struct {
	ManifestID string `json:"manifest_id"`
	ClientID   string `json:"client_id"`
	Path       string `json:"path"`
	Confirmed  int    `json:"confirmed"`
	Total      int    `json:"total"`
	Done       bool   `json:"done"`
	Seq        int64  `json:"seq"`
}

// VerifyResult is the outcome of a client side integrity check.
type VerifyResult struct {
	ManifestID string   `json:"manifest_id"`
	ClientID   string   `json:"client_id"`
	OK         bool     `json:"ok"`
	FilesDone  int      `json:"files_done"`
	FilesTotal int      `json:"files_total"`
	Missing    []string `json:"missing"`
}

// PatchManifestPlanner owns the manifests and the per client chunk bitmaps.
type PatchManifestPlanner struct {
	limits Limits
}

// NewPatchManifestPlanner builds a planner with the given caps.
func NewPatchManifestPlanner(limits Limits) *PatchManifestPlanner {
	return &PatchManifestPlanner{limits: limits}
}

// Build diffs two versions and stores the resulting manifest.
func (p *PatchManifestPlanner) Build(fromVersion, toVersion string, files []FileEntry) (Manifest, error) {
	return Manifest{}, errNotImplemented
}

// ManifestOf reads one published manifest.
func (p *PatchManifestPlanner) ManifestOf(manifestID string) (Manifest, bool) {
	return Manifest{}, false
}

// Begin pins a manifest to a client and returns its download plan.
func (p *PatchManifestPlanner) Begin(manifestID, clientID string) (DownloadPlan, error) {
	return DownloadPlan{}, errNotImplemented
}

// AckChunk records one confirmed chunk of one file.
func (p *PatchManifestPlanner) AckChunk(manifestID, clientID, filePath string, chunk int) (Progress, error) {
	return Progress{}, errNotImplemented
}

// Verify reports which files of a manifest are fully confirmed.
func (p *PatchManifestPlanner) Verify(manifestID, clientID string) (VerifyResult, error) {
	return VerifyResult{}, errNotImplemented
}

// Pause freezes the download of one client.
func (p *PatchManifestPlanner) Pause(manifestID, clientID string) error {
	return errNotImplemented
}

// Resume returns the chunks still missing for one client.
func (p *PatchManifestPlanner) Resume(manifestID, clientID string) (DownloadPlan, error) {
	return DownloadPlan{}, errNotImplemented
}

// ProgressOf reads the chunk progress of one file.
func (p *PatchManifestPlanner) ProgressOf(manifestID, clientID, filePath string) (Progress, bool) {
	return Progress{}, false
}

// SetClientDisk records the free disk space of one client in bytes.
func (p *PatchManifestPlanner) SetClientDisk(clientID string, freeBytes int64) error {
	return errNotImplemented
}

// PinnedManifest reads the manifest a client is pinned to.
func (p *PatchManifestPlanner) PinnedManifest(clientID string) (string, bool) {
	return "", false
}

// Snapshot serialises manifests, pins and chunk bitmaps.
func (p *PatchManifestPlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *PatchManifestPlanner) Restore(data []byte) error {
	return errNotImplemented
}
