// Package task unpacks firmware images: it reads the partition table, walks
// nested filesystems, and reports every file it extracted together with the
// path it came from, all under hard depth, per-file and total byte budgets.
package task

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	firmwareHeaderSize = 32
	firmwareEntrySize  = 32
	maxPartitions      = 32
	snapshotVersion    = 1
)

const (
	firmwareMagic = "FWIM"
	fwfsMagic     = "FWFS"
)

// Partition kinds.
const (
	KindRaw        uint32 = 1
	KindCompressed uint32 = 2
	KindNested     uint32 = 3
)

// Errors reported by the unpacker.
var (
	ErrBadImage      = errors.New("firmware image header or table is invalid")
	ErrOverlap       = errors.New("partition ranges overlap")
	ErrOutOfBounds   = errors.New("partition lies outside the image")
	ErrBudget        = errors.New("extraction budget is exhausted")
	ErrNotOpen       = errors.New("image is not open")
	ErrBusy          = errors.New("an extraction of this image is already running")
	ErrTooManyImages = errors.New("open image count exceeds the cap")
	ErrBadSelector   = errors.New("selector names an unknown partition")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

var errTotalLimit = errors.New("total budget exhausted")

// Image is one opened firmware blob.
type Image struct {
	ID     string
	Digest string
	Size   int
}

// Partition is one entry of the partition table.
type Partition struct {
	Name   string
	Offset uint32
	Size   uint32
	Kind   uint32
	Flags  uint32
}

// Selector narrows an extraction to one partition and one path prefix.
type Selector struct {
	Partition string
	Prefix    string
}

// Entry is one extracted file.
type Entry struct {
	Path     string
	Size     int
	SHA256   string
	Complete bool
}

// ManifestFile is one content digest in the manifest, with every path it was
// found at.
type ManifestFile struct {
	SHA256 string   `json:"sha256"`
	Size   int      `json:"size"`
	Paths  []string `json:"paths"`
}

// ManifestReport is the whole-image inventory produced after an extraction.
type ManifestReport struct {
	ImageID string         `json:"imageId"`
	Files   []ManifestFile `json:"files"`
	Missing []string       `json:"missing"`
}

// Stats counts the extraction work done so far.
type Stats struct {
	Images int
	Files  int
	Bytes  int
}

// Limits bounds nesting, file size, total bytes and open images.
type Limits struct {
	MaxDepth      int
	MaxFileBytes  int
	MaxTotalBytes int
	MaxImages     int
}

type imageState struct {
	mu           sync.Mutex
	closed       bool
	info         Image
	raw          []byte
	partitions   []Partition
	last         []Entry
	remaining    int
	inventory    []Entry
	hasInventory bool
}

// FirmwareImageUnpacker owns the open images and their extraction state.
type FirmwareImageUnpacker struct {
	mu     sync.Mutex
	limits Limits
	images map[string]*imageState
	order  []string
	seq    int
}

// NewFirmwareImageUnpacker builds an unpacker with the given budgets.
func NewFirmwareImageUnpacker(limits Limits) *FirmwareImageUnpacker {
	limits = normalizeLimits(limits)
	return &FirmwareImageUnpacker{
		limits: limits,
		images: make(map[string]*imageState),
	}
}

func normalizeLimits(limits Limits) Limits {
	if limits.MaxDepth < 0 {
		limits.MaxDepth = 0
	}
	if limits.MaxFileBytes < 0 {
		limits.MaxFileBytes = 0
	}
	if limits.MaxTotalBytes < 0 {
		limits.MaxTotalBytes = 0
	}
	if limits.MaxImages < 0 {
		limits.MaxImages = 0
	}
	return limits
}

// Open parses the image header and partition table and registers the image.
func (u *FirmwareImageUnpacker) Open(img []byte) (Image, error) {
	raw := cloneBytes(img)
	partitions, err := parseFirmware(raw)
	if err != nil {
		return Image{}, err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.images == nil {
		u.images = make(map[string]*imageState)
	}
	if len(u.images) >= u.limits.MaxImages {
		return Image{}, ErrTooManyImages
	}
	var id string
	for {
		u.seq++
		id = "img-" + strconv.Itoa(u.seq)
		if _, exists := u.images[id]; !exists {
			break
		}
	}
	info := Image{ID: id, Digest: digest, Size: len(raw)}
	state := &imageState{
		info:       info,
		raw:        raw,
		partitions: partitions,
		remaining:  u.limits.MaxTotalBytes,
	}
	u.images[id] = state
	u.order = append(u.order, id)
	return info, nil
}

// Partitions lists the partition table in offset order and reports overlaps.
func (u *FirmwareImageUnpacker) Partitions(img Image) ([]Partition, error) {
	state := u.imageState(img.ID)
	if state == nil {
		return nil, ErrNotOpen
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrNotOpen
	}
	partitions, err := validatePartitions(state.partitions, len(state.raw))
	if err != nil {
		return nil, err
	}
	return clonePartitions(partitions), nil
}

// Extract expands the selected partitions and returns the files found.
func (u *FirmwareImageUnpacker) Extract(img Image, sel Selector) ([]Entry, error) {
	state := u.imageState(img.ID)
	if state == nil {
		return nil, ErrNotOpen
	}
	if !state.mu.TryLock() {
		return nil, ErrBusy
	}
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrNotOpen
	}

	partitions, err := validatePartitions(state.partitions, len(state.raw))
	if err != nil {
		return nil, err
	}
	sel.Prefix = normalizePrefix(sel.Prefix)
	selected, err := selectPartitions(partitions, sel.Partition)
	if err != nil {
		return nil, err
	}

	ex := newExtractor(u.limits)
	var walkErr error
	for _, partition := range selected {
		if walkErr = ex.walkPartition(state.raw, partition, "", 0, sel); walkErr != nil {
			break
		}
	}

	entries := ex.sortedEntries()
	if walkErr != nil {
		if errors.Is(walkErr, errTotalLimit) || errors.Is(walkErr, ErrBudget) {
			walkErr = ErrBudget
		} else {
			return nil, walkErr
		}
	}
	if walkErr == nil && ex.fileLimit {
		walkErr = ErrBudget
	}

	state.last = cloneEntries(entries)
	state.remaining = ex.remaining
	if walkErr == nil && sel.Partition == "" && sel.Prefix == "" {
		state.inventory = cloneEntries(entries)
		state.hasInventory = true
	}
	return entries, walkErr
}

// Manifest returns the whole-image inventory of the last extraction.
func (u *FirmwareImageUnpacker) Manifest(img Image) ([]byte, error) {
	state := u.imageState(img.ID)
	if state == nil {
		return nil, ErrNotOpen
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrNotOpen
	}

	partitions, err := validatePartitions(state.partitions, len(state.raw))
	if err != nil {
		return nil, err
	}
	if !state.hasInventory {
		inventory, err := scanWholeImage(state.raw, partitions, u.limits)
		if err != nil {
			return nil, err
		}
		state.inventory = inventory
		state.hasInventory = true
	}
	return buildManifest(state.info.ID, state.inventory, state.last)
}

// Remaining reports how many total bytes the last extraction left in budget.
func (u *FirmwareImageUnpacker) Remaining(img Image) (int, error) {
	state := u.imageState(img.ID)
	if state == nil {
		return 0, ErrNotOpen
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return 0, ErrNotOpen
	}
	return state.remaining, nil
}

// Close forgets an open image and its extraction state.
func (u *FirmwareImageUnpacker) Close(img Image) error {
	state := u.imageState(img.ID)
	if state == nil {
		return ErrNotOpen
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return ErrNotOpen
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.images[img.ID] != state {
		return ErrNotOpen
	}
	state.closed = true
	delete(u.images, img.ID)
	u.removeOrderLocked(img.ID)
	return nil
}

// Images lists the open image ids in registration order.
func (u *FirmwareImageUnpacker) Images() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.order))
	for _, id := range u.order {
		if _, ok := u.images[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// Stats returns the extraction counters.
func (u *FirmwareImageUnpacker) Stats() Stats {
	u.mu.Lock()
	states := make([]*imageState, 0, len(u.order))
	for _, id := range u.order {
		if state := u.images[id]; state != nil {
			states = append(states, state)
		}
	}
	u.mu.Unlock()

	var stats Stats
	for _, state := range states {
		state.mu.Lock()
		if !state.closed {
			stats.Images++
			for _, entry := range state.last {
				stats.Files++
				stats.Bytes += entry.Size
			}
		}
		state.mu.Unlock()
	}
	return stats
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Seq      int             `json:"seq"`
	Images   []snapshotImage `json:"images"`
	Checksum string          `json:"checksum"`
}

type snapshotImage struct {
	ID        string  `json:"id"`
	Digest    string  `json:"digest"`
	Size      int     `json:"size"`
	Raw       []byte  `json:"raw"`
	Entries   []Entry `json:"entries"`
	Remaining int     `json:"remaining"`
}

// Snapshot serialises the open images and their extraction state.
func (u *FirmwareImageUnpacker) Snapshot() ([]byte, error) {
	u.mu.Lock()
	order := append([]string(nil), u.order...)
	seq := u.seq
	states := make([]*imageState, 0, len(order))
	for _, id := range order {
		if state := u.images[id]; state != nil {
			states = append(states, state)
		}
	}
	u.mu.Unlock()

	images := make([]snapshotImage, 0, len(states))
	for _, state := range states {
		state.mu.Lock()
		if !state.closed {
			images = append(images, snapshotImage{
				ID:        state.info.ID,
				Digest:    state.info.Digest,
				Size:      state.info.Size,
				Raw:       cloneBytes(state.raw),
				Entries:   cloneEntries(state.last),
				Remaining: state.remaining,
			})
		}
		state.mu.Unlock()
	}
	envelope := snapshotEnvelope{Version: snapshotVersion, Seq: seq, Images: images}
	envelope.Checksum = snapshotChecksum(envelope)
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the unpacker state with a snapshot, all or nothing.
func (u *FirmwareImageUnpacker) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || envelope.Seq < 0 || envelope.Checksum != snapshotChecksum(envelope) {
		return ErrBadSnapshot
	}
	if len(envelope.Images) > u.limits.MaxImages {
		return ErrTooManyImages
	}

	images := make(map[string]*imageState, len(envelope.Images))
	order := make([]string, 0, len(envelope.Images))
	maxSeq := envelope.Seq
	for _, item := range envelope.Images {
		if item.ID == "" || item.Size < 0 || item.Remaining < 0 || item.Remaining > u.limits.MaxTotalBytes {
			return ErrBadSnapshot
		}
		if _, exists := images[item.ID]; exists {
			return ErrBadSnapshot
		}
		raw := cloneBytes(item.Raw)
		partitions, err := parseFirmware(raw)
		if err != nil || item.Size != len(raw) {
			return ErrBadSnapshot
		}
		sum := sha256.Sum256(raw)
		if item.Digest != hex.EncodeToString(sum[:]) {
			return ErrBadSnapshot
		}
		if err := validateSnapshotEntries(item.Entries); err != nil {
			return ErrBadSnapshot
		}
		images[item.ID] = &imageState{
			info:       Image{ID: item.ID, Digest: item.Digest, Size: item.Size},
			raw:        raw,
			partitions: partitions,
			last:       cloneEntries(item.Entries),
			remaining:  item.Remaining,
		}
		order = append(order, item.ID)
		if itemSeq := snapshotSequence(item.ID); itemSeq > maxSeq {
			maxSeq = itemSeq
		}
	}

	u.mu.Lock()
	if u.images == nil {
		u.images = make(map[string]*imageState)
	}
	u.images = images
	u.order = order
	if maxSeq > u.seq {
		u.seq = maxSeq
	}
	u.mu.Unlock()
	return nil
}

// Reset drops every open image.
func (u *FirmwareImageUnpacker) Reset() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.images = make(map[string]*imageState)
	u.order = nil
	return nil
}

type extractor struct {
	limits        Limits
	remaining     int
	entries       []Entry
	fileLimit     bool
	totalExceeded bool
}

func newExtractor(limits Limits) *extractor {
	return &extractor{limits: limits, remaining: limits.MaxTotalBytes}
}

func (e *extractor) addData(path string, data []byte) error {
	if len(data) > e.limits.MaxFileBytes {
		e.fileLimit = true
		return nil
	}
	if len(data) > e.remaining {
		e.totalExceeded = true
		for i := range e.entries {
			e.entries[i].Complete = false
		}
		return errTotalLimit
	}
	sum := sha256.Sum256(data)
	e.entries = append(e.entries, Entry{
		Path:     path,
		Size:     len(data),
		SHA256:   hex.EncodeToString(sum[:]),
		Complete: true,
	})
	e.remaining -= len(data)
	return nil
}

func (e *extractor) sortedEntries() []Entry {
	entries := append([]Entry(nil), e.entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries
}

func (e *extractor) walkPartition(raw []byte, partition Partition, prefix string, depth int, sel Selector) error {
	start := uint64(partition.Offset)
	end := start + uint64(partition.Size)
	if end > uint64(len(raw)) {
		return ErrOutOfBounds
	}
	body := raw[int(start):int(end)]
	fullPath := joinFirmwarePath(prefix, partition.Name)

	switch partition.Kind {
	case KindRaw:
		if !selectorMatches(sel, fullPath, partition.Name) {
			return nil
		}
		return e.addData(fullPath, body)
	case KindNested:
		return e.walkContainer(body, fullPath, depth, sel)
	case KindCompressed:
		if isFWFS(body) || isFWIM(body) {
			return e.walkContainer(body, fullPath, depth, sel)
		}
		maxRead := e.remaining
		if e.limits.MaxFileBytes < maxRead {
			maxRead = e.limits.MaxFileBytes
		}
		decoded, overflow, err := decompressPartition(body, maxRead)
		if err != nil {
			return err
		}
		if overflow {
			if e.limits.MaxFileBytes <= e.remaining {
				e.fileLimit = true
				return nil
			}
			e.totalExceeded = true
			for i := range e.entries {
				e.entries[i].Complete = false
			}
			return errTotalLimit
		}
		if isFWFS(decoded) || isFWIM(decoded) {
			return e.walkContainer(decoded, fullPath, depth, sel)
		}
		if !selectorMatches(sel, fullPath, partition.Name) {
			return nil
		}
		return e.addData(fullPath, decoded)
	default:
		return ErrBadImage
	}
}

func (e *extractor) walkContainer(data []byte, prefix string, depth int, sel Selector) error {
	switch {
	case isFWFS(data):
		if depth+1 > e.limits.MaxDepth {
			return ErrBudget
		}
		return e.walkFWFS(data, prefix, sel)
	case isFWIM(data):
		if depth+2 > e.limits.MaxDepth {
			return ErrBudget
		}
		partitions, err := parseFirmware(data)
		if err != nil {
			return err
		}
		partitions, err = validatePartitions(partitions, len(data))
		if err != nil {
			return err
		}
		for _, partition := range partitions {
			if err := e.walkPartition(data, partition, prefix, depth+1, sel); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrBadImage
	}
}

func (e *extractor) walkFWFS(data []byte, prefix string, sel Selector) error {
	if len(data) < 8 {
		return ErrBadImage
	}
	countValue := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if countValue > uint64((len(data)-8)/8) {
		return ErrBadImage
	}
	count := int(countValue)
	offset := 8
	for i := 0; i < count; i++ {
		if offset > len(data)-4 {
			return ErrBadImage
		}
		nameLen := uint64(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if nameLen > uint64(len(data)-offset) {
			return ErrBadImage
		}
		nameEnd := offset + int(nameLen)
		name := string(data[offset:nameEnd])
		offset = nameEnd
		if offset > len(data)-4 {
			return ErrBadImage
		}
		fileLen := uint64(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if fileLen > uint64(len(data)-offset) {
			return ErrBadImage
		}
		fileEnd := offset + int(fileLen)
		body := data[offset:fileEnd]
		offset = fileEnd
		path := joinFirmwarePath(prefix, name)
		if !selectorMatches(sel, path, name) {
			continue
		}
		if err := e.addData(path, body); err != nil {
			return err
		}
	}
	return nil
}

func scanWholeImage(raw []byte, partitions []Partition, limits Limits) ([]Entry, error) {
	ex := newExtractor(limits)
	sel := Selector{}
	for _, partition := range partitions {
		if err := ex.walkPartition(raw, partition, "", 0, sel); err != nil {
			if errors.Is(err, errTotalLimit) || errors.Is(err, ErrBudget) {
				return nil, ErrBudget
			}
			return nil, err
		}
	}
	if ex.fileLimit || ex.totalExceeded {
		return nil, ErrBudget
	}
	return cloneEntries(ex.sortedEntries()), nil
}

func buildManifest(imageID string, inventory, last []Entry) ([]byte, error) {
	type aggregate struct {
		size  int
		paths map[string]struct{}
	}
	byDigest := make(map[string]*aggregate)
	for _, entry := range inventory {
		item := byDigest[entry.SHA256]
		if item == nil {
			item = &aggregate{size: entry.Size, paths: make(map[string]struct{})}
			byDigest[entry.SHA256] = item
		}
		item.paths[entry.Path] = struct{}{}
	}

	files := make([]ManifestFile, 0, len(byDigest))
	for digest, item := range byDigest {
		paths := make([]string, 0, len(item.paths))
		for path := range item.paths {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		files = append(files, ManifestFile{SHA256: digest, Size: item.size, Paths: paths})
	}
	sort.Slice(files, func(i, j int) bool {
		if len(files[i].Paths) == 0 || len(files[j].Paths) == 0 {
			return len(files[i].Paths) < len(files[j].Paths)
		}
		if files[i].Paths[0] != files[j].Paths[0] {
			return files[i].Paths[0] < files[j].Paths[0]
		}
		return files[i].SHA256 < files[j].SHA256
	})

	extracted := make(map[string]struct{}, len(last))
	for _, entry := range last {
		if entry.Complete {
			extracted[entry.Path] = struct{}{}
		}
	}
	missingSet := make(map[string]struct{})
	for _, entry := range inventory {
		if _, ok := extracted[entry.Path]; !ok {
			missingSet[entry.Path] = struct{}{}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for path := range missingSet {
		missing = append(missing, path)
	}
	sort.Strings(missing)

	report := ManifestReport{ImageID: imageID, Files: files, Missing: missing}
	return json.Marshal(report)
}

func parseFirmware(raw []byte) ([]Partition, error) {
	if len(raw) < firmwareHeaderSize {
		return nil, ErrBadImage
	}
	if string(raw[:4]) != firmwareMagic {
		return nil, ErrBadImage
	}
	if binary.LittleEndian.Uint32(raw[4:8]) != 1 {
		return nil, ErrBadImage
	}
	tableOffset := uint64(binary.LittleEndian.Uint32(raw[8:12]))
	count := uint64(binary.LittleEndian.Uint32(raw[12:16]))
	if count == 0 || count > maxPartitions {
		return nil, ErrBadImage
	}
	tableSize := count * firmwareEntrySize
	tableEnd := tableOffset + tableSize
	if tableEnd < tableOffset || tableEnd > uint64(len(raw)) {
		return nil, ErrBadImage
	}
	table := raw[int(tableOffset):int(tableEnd)]
	var sum uint32
	for _, value := range table {
		sum += uint32(value)
	}
	if sum != binary.LittleEndian.Uint32(raw[16:20]) {
		return nil, ErrBadImage
	}

	partitions := make([]Partition, 0, int(count))
	for i := uint64(0); i < count; i++ {
		entry := table[i*firmwareEntrySize : (i+1)*firmwareEntrySize]
		partitions = append(partitions, Partition{
			Name:   decodePartitionName(entry[:16]),
			Offset: binary.LittleEndian.Uint32(entry[16:20]),
			Size:   binary.LittleEndian.Uint32(entry[20:24]),
			Kind:   binary.LittleEndian.Uint32(entry[24:28]),
			Flags:  binary.LittleEndian.Uint32(entry[28:32]),
		})
	}
	return partitions, nil
}

func decodePartitionName(field []byte) string {
	if zero := bytes.IndexByte(field, 0); zero >= 0 {
		field = field[:zero]
	}
	return string(field)
}

func validatePartitions(partitions []Partition, imageSize int) ([]Partition, error) {
	ordered := clonePartitions(partitions)
	for _, partition := range ordered {
		end := uint64(partition.Offset) + uint64(partition.Size)
		if end < uint64(partition.Offset) || end > uint64(imageSize) {
			return nil, ErrOutOfBounds
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Offset < ordered[j].Offset
	})

	var previousEnd uint64
	havePrevious := false
	for _, partition := range ordered {
		if partition.Size == 0 {
			continue
		}
		start := uint64(partition.Offset)
		end := start + uint64(partition.Size)
		if havePrevious && start < previousEnd {
			return nil, ErrOverlap
		}
		if !havePrevious || end > previousEnd {
			previousEnd = end
			havePrevious = true
		}
	}
	return ordered, nil
}

func selectPartitions(partitions []Partition, name string) ([]Partition, error) {
	if name == "" {
		return partitions, nil
	}
	selected := make([]Partition, 0, 1)
	for _, partition := range partitions {
		if partition.Name == name {
			selected = append(selected, partition)
		}
	}
	if len(selected) == 0 {
		return nil, ErrBadSelector
	}
	return selected, nil
}

func selectorMatches(sel Selector, fullPath, relativePath string) bool {
	if sel.Prefix == "" {
		return true
	}
	if strings.HasPrefix(fullPath, sel.Prefix) {
		return true
	}
	return relativePath != "" && strings.HasPrefix(relativePath, sel.Prefix)
}

func normalizePrefix(prefix string) string {
	for {
		switch {
		case strings.HasPrefix(prefix, "./"):
			prefix = prefix[2:]
		case strings.HasPrefix(prefix, "/"):
			prefix = prefix[1:]
		default:
			return prefix
		}
	}
}

func joinFirmwarePath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

func isFWFS(data []byte) bool {
	return len(data) >= 4 && string(data[:4]) == fwfsMagic
}

func isFWIM(data []byte) bool {
	return len(data) >= 4 && string(data[:4]) == firmwareMagic
}

func decompressPartition(data []byte, maxBytes int) ([]byte, bool, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	decoded, overflow, err := decompressAt(data, maxBytes)
	if err == nil {
		return decoded, overflow, nil
	}
	if len(data) > 4 {
		if decoded, overflow, retryErr := decompressAt(data[4:], maxBytes); retryErr == nil {
			return decoded, overflow, nil
		}
	}
	return nil, false, ErrBadImage
}

func decompressAt(data []byte, maxBytes int) ([]byte, bool, error) {
	var reader io.Reader
	var closer io.Closer
	var gzipData []byte
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		gzipReader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false, err
		}
		reader, closer = gzipReader, gzipReader
		gzipData = data
	case len(data) >= 2 && data[0]&0x0f == 8 && (uint16(data[0])<<8|uint16(data[1]))%31 == 0:
		zlibReader, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false, err
		}
		reader, closer = zlibReader, zlibReader
	case len(data) >= 4 && bytes.Equal(data[:4], []byte("PK\x03\x04")):
		zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil || len(zipReader.File) != 1 {
			return nil, false, ErrBadImage
		}
		zipFile, err := zipReader.File[0].Open()
		if err != nil {
			return nil, false, err
		}
		reader, closer = zipFile, zipFile
	default:
		flateReader := flate.NewReader(bytes.NewReader(data))
		reader, closer = flateReader, flateReader
	}
	if closer != nil {
		defer closer.Close()
	}

	limit := int64(maxBytes) + 1
	decoded, err := io.ReadAll(io.LimitReader(reader, limit))
	if int64(len(decoded)) > int64(maxBytes) {
		return nil, true, nil
	}
	if err != nil && gzipData != nil && errors.Is(err, gzip.ErrChecksum) && gzipCRCValid(gzipData, decoded) {
		err = nil
	}
	if err != nil {
		return nil, false, err
	}
	return decoded, false, nil
}

func gzipCRCValid(data, decoded []byte) bool {
	if len(data) < 8 {
		return false
	}
	trailer := data[len(data)-8:]
	return binary.LittleEndian.Uint32(trailer[:4]) == crc32.ChecksumIEEE(decoded)
}

func validateSnapshotEntries(entries []Entry) error {
	for _, entry := range entries {
		if entry.Path == "" || entry.Size < 0 {
			return ErrBadSnapshot
		}
		sum, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(sum) != sha256.Size {
			return ErrBadSnapshot
		}
	}
	return nil
}

func snapshotSequence(id string) int {
	const prefix = "img-"
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func snapshotChecksum(envelope snapshotEnvelope) string {
	envelope.Checksum = ""
	data, err := json.Marshal(envelope)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (u *FirmwareImageUnpacker) imageState(id string) *imageState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.images[id]
}

func (u *FirmwareImageUnpacker) removeOrderLocked(id string) {
	for i, existing := range u.order {
		if existing == id {
			u.order = append(u.order[:i], u.order[i+1:]...)
			return
		}
	}
}

func cloneBytes(data []byte) []byte {
	if data == nil {
		return nil
	}
	return append([]byte(nil), data...)
}

func clonePartitions(partitions []Partition) []Partition {
	return append([]Partition(nil), partitions...)
}

func cloneEntries(entries []Entry) []Entry {
	return append([]Entry(nil), entries...)
}
