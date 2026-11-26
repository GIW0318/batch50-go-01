// Package task plans client side patch downloads: it diffs two versions file
// by file, slices the transferred files into fixed size chunks and tracks the
// confirmed chunks of every client across restarts.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

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

type fileProgress struct {
	confirmed []bool
	count     int
	seq       int64
}

type clientState struct {
	manifestID string
	diskBytes  int64
	hasDisk    bool
	paused     bool
	files      map[string]*fileProgress
}

// PatchManifestPlanner owns the manifests and the per client chunk bitmaps.
type PatchManifestPlanner struct {
	mu              sync.Mutex
	limits          Limits
	manifests       map[string]Manifest
	clients         map[string]*clientState
	chunkCache      map[string][]ChunkRef
	nextManifestSeq int64
	nextOpSeq       int64
}

// NewPatchManifestPlanner builds a planner with the given caps.
func NewPatchManifestPlanner(limits Limits) *PatchManifestPlanner {
	return &PatchManifestPlanner{
		limits:     limits,
		manifests:  make(map[string]Manifest),
		clients:    make(map[string]*clientState),
		chunkCache: make(map[string][]ChunkRef),
	}
}

// Build diffs two versions and stores the resulting manifest.
func (p *PatchManifestPlanner) Build(fromVersion, toVersion string, files []FileEntry) (Manifest, error) {
	if strings.TrimSpace(fromVersion) == "" || strings.TrimSpace(toVersion) == "" || fromVersion == toVersion {
		return Manifest{}, ErrBadVersion
	}

	seenPaths := make(map[string]struct{}, len(files))
	for _, file := range files {
		if strings.TrimSpace(file.Path) == "" || file.Size < 0 {
			return Manifest{}, ErrBadFile
		}
		if _, exists := seenPaths[file.Path]; exists {
			return Manifest{}, ErrBadFile
		}
		seenPaths[file.Path] = struct{}{}
	}
	if len(files) > p.limits.MaxFiles {
		return Manifest{}, ErrFileLimit
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	id := stableManifestID(fromVersion, toVersion)
	if existing, ok := p.manifests[id]; ok {
		return cloneManifest(existing), nil
	}
	if len(p.manifests) >= p.limits.MaxManifests {
		return Manifest{}, ErrManifestLimit
	}

	baseline := p.latestManifestForVersionLocked(fromVersion)
	baselinePaths := make(map[string]FilePlan)
	if baseline != nil {
		for _, plan := range baseline.Files {
			switch plan.Action {
			case ActionKeep, ActionAdd, ActionReplace:
				baselinePaths[plan.Path] = plan
			}
		}
	}

	target := append([]FileEntry(nil), files...)
	sort.Slice(target, func(i, j int) bool { return target[i].Path < target[j].Path })

	plans := make([]FilePlan, 0, len(target)+len(baselinePaths))
	targetPaths := make(map[string]struct{}, len(target))
	var totalBytes int64
	var totalChunks int

	for _, file := range target {
		targetPaths[file.Path] = struct{}{}
		oldPlan, existed := baselinePaths[file.Path]

		plan := FilePlan{Path: file.Path, Size: file.Size, Hash: file.Hash}
		switch {
		case existed && oldPlan.Hash == file.Hash:
			plan.Action = ActionKeep
		case existed:
			plan.Action = ActionReplace
			plan.Chunks = p.chunksForLocked(file.Path, file.Hash, file.Size)
		default:
			plan.Action = ActionAdd
			plan.Chunks = p.chunksForLocked(file.Path, file.Hash, file.Size)
		}
		if plan.Action == ActionAdd || plan.Action == ActionReplace {
			totalBytes += plan.Size
			totalChunks += len(plan.Chunks)
		}
		plans = append(plans, plan)
	}

	for path, oldPlan := range baselinePaths {
		if _, stillPresent := targetPaths[path]; stillPresent {
			continue
		}
		plans = append(plans, FilePlan{
			Path:   path,
			Action: ActionRemove,
			Size:   oldPlan.Size,
			Hash:   oldPlan.Hash,
		})
	}

	sort.Slice(plans, func(i, j int) bool { return plans[i].Path < plans[j].Path })

	p.nextManifestSeq++
	manifest := Manifest{
		ID:          id,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Files:       plans,
		Chunks:      totalChunks,
		TotalBytes:  totalBytes,
		Seq:         p.nextManifestSeq,
	}
	p.manifests[id] = cloneManifest(manifest)
	return manifest, nil
}

// ManifestOf reads one published manifest.
func (p *PatchManifestPlanner) ManifestOf(manifestID string) (Manifest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	manifest, ok := p.manifests[manifestID]
	if !ok {
		return Manifest{}, false
	}
	return cloneManifest(manifest), true
}

// Begin pins a manifest to a client and returns its download plan.
func (p *PatchManifestPlanner) Begin(manifestID, clientID string) (DownloadPlan, error) {
	if strings.TrimSpace(clientID) == "" {
		return DownloadPlan{}, ErrBadClient
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	manifest, ok := p.manifests[manifestID]
	if !ok {
		return DownloadPlan{}, ErrManifestNotFound
	}
	state := p.clients[clientID]
	if state != nil && state.manifestID != "" && state.manifestID != manifestID {
		return DownloadPlan{}, ErrVersionPinned
	}
	if state != nil && state.hasDisk && state.diskBytes < manifest.TotalBytes {
		return DownloadPlan{}, ErrDiskFull
	}
	if state != nil && state.manifestID != "" {
		p.nextOpSeq++
		return missingPlanLocked(manifest, clientID, state, state.paused, p.nextOpSeq), nil
	}

	p.nextOpSeq++
	if state == nil {
		state = &clientState{}
		p.clients[clientID] = state
	}
	state.manifestID = manifestID
	state.paused = false
	state.files = make(map[string]*fileProgress, len(manifest.Files))
	for _, plan := range manifest.Files {
		state.files[plan.Path] = &fileProgress{
			confirmed: make([]bool, len(plan.Chunks)),
			seq:       p.nextOpSeq,
		}
	}
	return missingPlanLocked(manifest, clientID, state, false, p.nextOpSeq), nil
}

// AckChunk records one confirmed chunk of one file.
func (p *PatchManifestPlanner) AckChunk(manifestID, clientID, filePath string, chunk int) (Progress, error) {
	if strings.TrimSpace(clientID) == "" {
		return Progress{}, ErrBadClient
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	manifest, ok := p.manifests[manifestID]
	if !ok {
		return Progress{}, ErrManifestNotFound
	}
	state := p.clients[clientID]
	if state == nil || state.manifestID == "" {
		return Progress{}, ErrNoPlan
	}
	if state.manifestID != manifestID {
		return Progress{}, ErrVersionPinned
	}

	plan, planOK := findFilePlan(manifest, filePath)
	if !planOK || chunk < 0 || chunk >= len(plan.Chunks) {
		return Progress{}, ErrBadChunk
	}
	progress := state.files[filePath]
	if progress == nil || len(progress.confirmed) != len(plan.Chunks) {
		return Progress{}, ErrBadChunk
	}

	if !progress.confirmed[chunk] {
		progress.confirmed[chunk] = true
		progress.count++
		p.nextOpSeq++
		progress.seq = p.nextOpSeq
	}
	return progressValue(manifestID, clientID, plan.Path, progress), nil
}

// Verify reports which files of a manifest are fully confirmed.
func (p *PatchManifestPlanner) Verify(manifestID, clientID string) (VerifyResult, error) {
	if strings.TrimSpace(clientID) == "" {
		return VerifyResult{}, ErrBadClient
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	manifest, ok := p.manifests[manifestID]
	if !ok {
		return VerifyResult{}, ErrManifestNotFound
	}
	state := p.clients[clientID]
	if state == nil || state.manifestID == "" {
		return VerifyResult{}, ErrNoPlan
	}
	if state.manifestID != manifestID {
		return VerifyResult{}, ErrVersionPinned
	}

	result := VerifyResult{
		ManifestID: manifestID,
		ClientID:   clientID,
		FilesTotal: len(manifest.Files),
	}
	for _, plan := range manifest.Files {
		complete := plan.Action == ActionKeep || plan.Action == ActionRemove || len(plan.Chunks) == 0
		if !complete {
			progress := state.files[plan.Path]
			complete = progress != nil && progress.count == len(plan.Chunks)
		}
		if complete {
			result.FilesDone++
		} else {
			result.Missing = append(result.Missing, plan.Path)
		}
	}
	sort.Strings(result.Missing)
	result.OK = result.FilesDone == result.FilesTotal
	return result, nil
}

// Pause freezes the download of one client.
func (p *PatchManifestPlanner) Pause(manifestID, clientID string) error {
	if strings.TrimSpace(clientID) == "" {
		return ErrBadClient
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.manifests[manifestID]; !ok {
		return ErrManifestNotFound
	}
	state := p.clients[clientID]
	if state == nil || state.manifestID == "" {
		return ErrNoPlan
	}
	if state.manifestID != manifestID {
		return ErrVersionPinned
	}
	if state.paused {
		return ErrAlreadyPaused
	}
	state.paused = true
	p.nextOpSeq++
	return nil
}

// Resume returns the chunks still missing for one client.
func (p *PatchManifestPlanner) Resume(manifestID, clientID string) (DownloadPlan, error) {
	if strings.TrimSpace(clientID) == "" {
		return DownloadPlan{}, ErrBadClient
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	manifest, ok := p.manifests[manifestID]
	if !ok {
		return DownloadPlan{}, ErrManifestNotFound
	}
	state := p.clients[clientID]
	if state == nil || state.manifestID == "" {
		return DownloadPlan{}, ErrNoPlan
	}
	if state.manifestID != manifestID {
		return DownloadPlan{}, ErrVersionPinned
	}
	if !state.paused {
		return DownloadPlan{}, ErrNotPaused
	}
	state.paused = false
	p.nextOpSeq++
	return missingPlanLocked(manifest, clientID, state, true, p.nextOpSeq), nil
}

// ProgressOf reads the chunk progress of one file.
func (p *PatchManifestPlanner) ProgressOf(manifestID, clientID, filePath string) (Progress, bool) {
	if strings.TrimSpace(clientID) == "" {
		return Progress{}, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	manifest, ok := p.manifests[manifestID]
	if !ok {
		return Progress{}, false
	}
	state := p.clients[clientID]
	if state == nil || state.manifestID != manifestID {
		return Progress{}, false
	}
	plan, ok := findFilePlan(manifest, filePath)
	if !ok {
		return Progress{}, false
	}
	progress := state.files[filePath]
	if progress == nil {
		return Progress{}, false
	}
	return progressValue(manifestID, clientID, plan.Path, progress), true
}

// SetClientDisk records the free disk space of one client in bytes.
func (p *PatchManifestPlanner) SetClientDisk(clientID string, freeBytes int64) error {
	if strings.TrimSpace(clientID) == "" {
		return ErrBadClient
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.clients[clientID]
	if state == nil {
		state = &clientState{}
		p.clients[clientID] = state
	}
	state.hasDisk = true
	state.diskBytes = freeBytes
	return nil
}

// PinnedManifest reads the manifest a client is pinned to.
func (p *PatchManifestPlanner) PinnedManifest(clientID string) (string, bool) {
	if strings.TrimSpace(clientID) == "" {
		return "", false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.clients[clientID]
	if state == nil || state.manifestID == "" {
		return "", false
	}
	if _, ok := p.manifests[state.manifestID]; !ok {
		return "", false
	}
	return state.manifestID, true
}

type snapshotFileProgress struct {
	Confirmed []bool `json:"confirmed"`
	Seq       int64  `json:"seq"`
}

type snapshotClient struct {
	ManifestID string                          `json:"manifest_id,omitempty"`
	DiskBytes  int64                           `json:"disk_bytes,omitempty"`
	HasDisk    bool                            `json:"has_disk,omitempty"`
	Paused     bool                            `json:"paused,omitempty"`
	Files      map[string]snapshotFileProgress `json:"files,omitempty"`
}

type snapshotDocument struct {
	Version         int                       `json:"version"`
	Manifests       []Manifest                `json:"manifests"`
	Clients         map[string]snapshotClient `json:"clients"`
	NextManifestSeq int64                     `json:"next_manifest_seq"`
	NextOpSeq       int64                     `json:"next_op_seq"`
	Checksum        string                    `json:"checksum"`
}

// Snapshot serialises manifests, pins and chunk bitmaps.
func (p *PatchManifestPlanner) Snapshot() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	document := snapshotDocument{
		Version:         1,
		Manifests:       make([]Manifest, 0, len(p.manifests)),
		Clients:         make(map[string]snapshotClient, len(p.clients)),
		NextManifestSeq: p.nextManifestSeq,
		NextOpSeq:       p.nextOpSeq,
	}
	for _, manifest := range p.manifests {
		document.Manifests = append(document.Manifests, cloneManifest(manifest))
	}
	sort.Slice(document.Manifests, func(i, j int) bool {
		return document.Manifests[i].Seq < document.Manifests[j].Seq
	})

	for clientID, state := range p.clients {
		client := snapshotClient{
			ManifestID: state.manifestID,
			DiskBytes:  state.diskBytes,
			HasDisk:    state.hasDisk,
			Paused:     state.paused,
		}
		if state.manifestID != "" {
			client.Files = make(map[string]snapshotFileProgress, len(state.files))
			for path, progress := range state.files {
				client.Files[path] = snapshotFileProgress{
					Confirmed: append([]bool(nil), progress.confirmed...),
					Seq:       progress.seq,
				}
			}
		}
		document.Clients[clientID] = client
	}

	checksum, err := snapshotChecksum(document)
	if err != nil {
		return nil, err
	}
	document.Checksum = checksum
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *PatchManifestPlanner) Restore(data []byte) error {
	var document snapshotDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return ErrBadSnapshot
	}
	if !validSnapshotChecksum(document) || !validSnapshot(document) {
		return ErrBadSnapshot
	}

	manifests := make(map[string]Manifest, len(document.Manifests))
	for _, manifest := range document.Manifests {
		manifests[manifest.ID] = cloneManifest(manifest)
	}
	clients := make(map[string]*clientState, len(document.Clients))
	for clientID, snapshot := range document.Clients {
		state := &clientState{
			manifestID: snapshot.ManifestID,
			diskBytes:  snapshot.DiskBytes,
			hasDisk:    snapshot.HasDisk,
			paused:     snapshot.Paused,
		}
		if snapshot.ManifestID != "" {
			state.files = make(map[string]*fileProgress, len(snapshot.Files))
			for path, progress := range snapshot.Files {
				confirmed := append([]bool(nil), progress.Confirmed...)
				count := 0
				for _, bit := range confirmed {
					if bit {
						count++
					}
				}
				state.files[path] = &fileProgress{
					confirmed: confirmed,
					count:     count,
					seq:       progress.Seq,
				}
			}
		}
		clients[clientID] = state
	}

	p.mu.Lock()
	p.manifests = manifests
	p.clients = clients
	p.chunkCache = make(map[string][]ChunkRef)
	p.nextManifestSeq = document.NextManifestSeq
	p.nextOpSeq = document.NextOpSeq
	p.mu.Unlock()
	return nil
}

func (p *PatchManifestPlanner) latestManifestForVersionLocked(version string) *Manifest {
	var latest *Manifest
	for _, manifest := range p.manifests {
		if manifest.ToVersion != version {
			continue
		}
		if latest == nil || manifest.Seq > latest.Seq {
			manifestCopy := cloneManifest(manifest)
			latest = &manifestCopy
		}
	}
	return latest
}

func (p *PatchManifestPlanner) chunksForLocked(path, fileHash string, size int64) []ChunkRef {
	key := fileHash + "\x00" + fmt.Sprintf("%d", size)
	template, ok := p.chunkCache[key]
	if !ok {
		chunkSize := p.limits.ChunkSize
		if chunkSize <= 0 {
			chunkSize = 1
		}
		step := int64(chunkSize)
		var chunks []ChunkRef
		var offset int64
		for index := 0; offset < size; index++ {
			length := chunkSize
			if remaining := size - offset; remaining < step {
				length = int(remaining)
			}
			chunks = append(chunks, ChunkRef{
				Index:  index,
				Offset: offset,
				Length: length,
				Hash:   contentChunkHash(fileHash, index, offset, length),
			})
			offset += int64(length)
		}
		template = chunks
		p.chunkCache[key] = chunks
	}

	chunks := make([]ChunkRef, len(template))
	for i, chunk := range template {
		chunk.Path = path
		chunks[i] = chunk
	}
	return chunks
}

func missingPlanLocked(manifest Manifest, clientID string, state *clientState, resume bool, seq int64) DownloadPlan {
	plan := DownloadPlan{
		ManifestID: manifest.ID,
		ClientID:   clientID,
		Resume:     resume,
		Seq:        seq,
	}
	for _, file := range manifest.Files {
		progress := state.files[file.Path]
		for _, chunk := range file.Chunks {
			if progress == nil || chunk.Index < 0 || chunk.Index >= len(progress.confirmed) || !progress.confirmed[chunk.Index] {
				plan.Chunks = append(plan.Chunks, chunk)
			}
		}
	}
	return plan
}

func progressValue(manifestID, clientID, path string, progress *fileProgress) Progress {
	total := len(progress.confirmed)
	return Progress{
		ManifestID: manifestID,
		ClientID:   clientID,
		Path:       path,
		Confirmed:  progress.count,
		Total:      total,
		Done:       progress.count == total,
		Seq:        progress.seq,
	}
}

func findFilePlan(manifest Manifest, path string) (FilePlan, bool) {
	for _, plan := range manifest.Files {
		if plan.Path == path {
			return plan, true
		}
	}
	return FilePlan{}, false
}

func stableManifestID(fromVersion, toVersion string) string {
	sum := sha256.Sum256([]byte(fromVersion + "\x00" + toVersion))
	return "manifest-" + hex.EncodeToString(sum[:])
}

func contentChunkHash(fileHash string, index int, offset int64, length int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d", fileHash, index, offset, length)))
	return hex.EncodeToString(sum[:])
}

func snapshotChecksum(document snapshotDocument) (string, error) {
	document.Checksum = ""
	data, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validSnapshotChecksum(document snapshotDocument) bool {
	if document.Checksum == "" {
		return false
	}
	expected, err := snapshotChecksum(document)
	return err == nil && document.Checksum == expected
}

func cloneChunks(chunks []ChunkRef) []ChunkRef {
	if chunks == nil {
		return nil
	}
	out := make([]ChunkRef, len(chunks))
	copy(out, chunks)
	return out
}

func cloneManifest(manifest Manifest) Manifest {
	out := manifest
	if manifest.Files == nil {
		return out
	}
	out.Files = make([]FilePlan, len(manifest.Files))
	for i, plan := range manifest.Files {
		out.Files[i] = plan
		out.Files[i].Chunks = cloneChunks(plan.Chunks)
	}
	return out
}

func validSnapshot(document snapshotDocument) bool {
	if document.Version != 1 || document.NextManifestSeq < 0 || document.NextOpSeq < 0 {
		return false
	}

	seenIDs := make(map[string]struct{}, len(document.Manifests))
	seenSequences := make(map[int64]struct{}, len(document.Manifests))
	var maxSequence int64
	for _, manifest := range document.Manifests {
		if strings.TrimSpace(manifest.FromVersion) == "" || strings.TrimSpace(manifest.ToVersion) == "" || manifest.FromVersion == manifest.ToVersion {
			return false
		}
		if manifest.ID == "" || manifest.ID != stableManifestID(manifest.FromVersion, manifest.ToVersion) {
			return false
		}
		if manifest.Seq <= 0 {
			return false
		}
		if _, duplicate := seenIDs[manifest.ID]; duplicate {
			return false
		}
		if _, duplicate := seenSequences[manifest.Seq]; duplicate {
			return false
		}
		seenIDs[manifest.ID] = struct{}{}
		seenSequences[manifest.Seq] = struct{}{}
		if manifest.Seq > maxSequence {
			maxSequence = manifest.Seq
		}
		if manifest.Chunks < 0 || manifest.TotalBytes < 0 {
			return false
		}

		seenPaths := make(map[string]struct{}, len(manifest.Files))
		var chunks int64
		var bytes int64
		for _, plan := range manifest.Files {
			if strings.TrimSpace(plan.Path) == "" || plan.Size < 0 {
				return false
			}
			if _, duplicate := seenPaths[plan.Path]; duplicate {
				return false
			}
			seenPaths[plan.Path] = struct{}{}
			switch plan.Action {
			case ActionKeep, ActionRemove:
				if len(plan.Chunks) != 0 {
					return false
				}
			case ActionAdd, ActionReplace:
				if !validFileChunks(plan) {
					return false
				}
				chunks += int64(len(plan.Chunks))
				bytes += plan.Size
			default:
				return false
			}
		}
		if int64(manifest.Chunks) != chunks || manifest.TotalBytes != bytes {
			return false
		}
	}
	if document.NextManifestSeq != maxSequence {
		return false
	}

	manifestByID := make(map[string]Manifest, len(document.Manifests))
	for _, manifest := range document.Manifests {
		manifestByID[manifest.ID] = manifest
	}
	var maxProgressSeq int64
	for clientID, snapshot := range document.Clients {
		if strings.TrimSpace(clientID) == "" {
			return false
		}
		if snapshot.ManifestID == "" {
			if snapshot.Paused || len(snapshot.Files) != 0 {
				return false
			}
			continue
		}
		manifest, ok := manifestByID[snapshot.ManifestID]
		if !ok || len(snapshot.Files) != len(manifest.Files) {
			return false
		}
		for _, plan := range manifest.Files {
			progress, ok := snapshot.Files[plan.Path]
			if !ok || len(progress.Confirmed) != len(plan.Chunks) || progress.Seq < 0 {
				return false
			}
			if progress.Seq > maxProgressSeq {
				maxProgressSeq = progress.Seq
			}
		}
	}
	if maxProgressSeq > document.NextOpSeq {
		return false
	}
	return true
}

func validFileChunks(plan FilePlan) bool {
	if len(plan.Chunks) == 0 {
		return plan.Size == 0
	}
	var nextOffset int64
	firstLength := 0
	for index, chunk := range plan.Chunks {
		if chunk.Path != plan.Path || chunk.Index != index || chunk.Offset != nextOffset || chunk.Length <= 0 {
			return false
		}
		if index == 0 {
			firstLength = chunk.Length
		} else if index < len(plan.Chunks)-1 && chunk.Length != firstLength {
			return false
		}
		if index == len(plan.Chunks)-1 && firstLength > 0 && chunk.Length > firstLength {
			return false
		}
		if chunk.Hash != contentChunkHash(plan.Hash, index, chunk.Offset, chunk.Length) {
			return false
		}
		nextOffset += int64(chunk.Length)
	}
	return nextOffset == plan.Size
}
