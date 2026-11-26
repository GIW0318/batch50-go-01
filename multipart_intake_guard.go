// Package task guards the upload entrance: it parses multipart/form-data
// bodies strictly, normalises part metadata against fixed limits and keeps
// the intake ledger of the gateway.
package task

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Upload lifecycle states.
const (
	UploadOpen    = "open"
	UploadDone    = "done"
	UploadAborted = "aborted"
)

// Duplicate field policies for form fields with the same name.
const (
	DuplicateFirst  = "first"
	DuplicateReject = "reject"
)

// Errors reported by the guard.
var (
	ErrMissingBoundary   = errors.New("multipart boundary is missing")
	ErrBadBoundary       = errors.New("multipart boundary is malformed")
	ErrBadContentType    = errors.New("content type is not multipart/form-data")
	ErrPartLimit         = errors.New("part count limit reached")
	ErrHeaderLimit       = errors.New("part header block is too large")
	ErrNameTooLong       = errors.New("filename is longer than the allowed limit")
	ErrBadFilename       = errors.New("filename is empty, reserved or path only")
	ErrQuotaExceeded     = errors.New("upload quota is exhausted")
	ErrUnknownUpload     = errors.New("upload id is unknown")
	ErrBadState          = errors.New("upload is not in a state that allows this call")
	ErrPartIndex         = errors.New("part index is outside the parsed range")
	ErrPartSize          = errors.New("committed payload does not match the declared part size")
	ErrPartConflict      = errors.New("part index was already committed")
	ErrNestedType        = errors.New("nested content type is not allowed")
	ErrNestedDepth       = errors.New("nested multipart depth limit reached")
	ErrTransferEncoding  = errors.New("content transfer encoding is not supported")
	ErrDuplicateField    = errors.New("duplicate form field rejected by policy")
	ErrBoundaryAmbiguous = errors.New("boundary appears in the body without a delimiter line")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Limits bounds one upload and the live intake table.
type Limits struct {
	MaxParts        int
	MaxHeaderBytes  int
	MaxNameLen      int
	MaxTotalBytes   int64
	MaxNestedDepth  int
	DuplicatePolicy string
	IdleTimeout     time.Duration
	NestedTypes     []string
}

// Upload is the live record of one intake.
type Upload struct {
	ID       string
	Boundary string
	State    string
	Parts    int
	Bytes    int64
	Created  time.Time
}

// PartMeta describes one parsed part before its payload is committed.
type PartMeta struct {
	Index       int
	Name        string
	Filename    string
	ContentType string
	Size        int64
	Nested      bool
}

// Receipt is the outcome of a finished upload.
type Receipt struct {
	UploadID string
	Parts    int
	Bytes    int64
	Digest   string
	At       time.Time
}

// Stats is the intake ledger counters.
type Stats struct {
	Accepted uint64
	Rejected uint64
	Bytes    uint64
	Active   int
}

// MultipartIntakeGuard owns the upload table and the quota counters.
type MultipartIntakeGuard struct {
	mu       sync.Mutex
	limits   Limits
	uploads  map[string]*uploadState
	receipts map[string]Receipt
	stats    Stats
	nextID   uint64
	aborted  map[string]struct{}
}

type uploadState struct {
	id          string
	boundary    string
	state       string
	created     time.Time
	lastWrite   time.Time
	received    int64
	raw         []byte
	topSearch   int
	terminated  bool
	parts       []*partRecord
	nextPart    int
	current     *partRecord
	readPos     int
	seenFields  map[string]struct{}
	fieldValues map[string]string
}

type partRecord struct {
	meta             PartMeta
	payload          []byte
	committed        bool
	committedPayload []byte
	isField          bool
	duplicateField   bool
	parent           *partRecord
	depth            int
	nestedBoundary   string
	nestedSearch     int
	nestedDone       bool
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotState struct {
	Receipts []Receipt `json:"receipts"`
	Stats    Stats     `json:"stats"`
	Active   []string  `json:"active"`
	NextID   uint64    `json:"next_id"`
}

const (
	snapshotMagic   = "gsb-multipart-intake"
	snapshotVersion = 1
)

var reservedFilenames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"CLOCK$": {}, "CONIN$": {}, "CONOUT$": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// NewMultipartIntakeGuard builds a guard with the given limits.
func NewMultipartIntakeGuard(limits Limits) *MultipartIntakeGuard {
	nested := append([]string(nil), limits.NestedTypes...)
	limits.NestedTypes = nested
	return &MultipartIntakeGuard{
		limits:   limits,
		uploads:  make(map[string]*uploadState),
		receipts: make(map[string]Receipt),
		aborted:  make(map[string]struct{}),
	}
}

// Begin validates the request content type and opens an upload.
func (g *MultipartIntakeGuard) Begin(header http.Header) (Upload, error) {
	boundary, err := boundaryFromContentType(header.Get("Content-Type"))
	if err != nil {
		g.mu.Lock()
		g.stats.Rejected++
		g.mu.Unlock()
		return Upload{}, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureMapsLocked()

	g.nextID++
	id := fmt.Sprintf("upload-%d", g.nextID)
	now := time.Now()
	u := &uploadState{
		id:          id,
		boundary:    boundary,
		state:       UploadOpen,
		created:     now,
		lastWrite:   now,
		seenFields:  make(map[string]struct{}),
		fieldValues: make(map[string]string),
	}
	g.uploads[id] = u
	return Upload{
		ID:       id,
		Boundary: boundary,
		State:    UploadOpen,
		Created:  now,
	}, nil
}

// Write feeds one chunk of the raw body into the upload buffer.
func (g *MultipartIntakeGuard) Write(uploadID string, p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return 0, ErrUnknownUpload
	}
	if u.state != UploadOpen {
		return 0, ErrBadState
	}

	chunk := int64(len(p))
	max := g.limits.MaxTotalBytes
	if chunk > max-u.received {
		g.rejectUploadLocked(u)
		return 0, ErrQuotaExceeded
	}

	u.raw = append(u.raw, p...)
	u.received += chunk
	u.lastWrite = time.Now()
	return len(p), nil
}

// NextPart parses the next top level part header block.
func (g *MultipartIntakeGuard) NextPart(uploadID string) (PartMeta, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return PartMeta{}, ErrUnknownUpload
	}
	if u.state != UploadOpen {
		return PartMeta{}, ErrBadState
	}
	if u.terminated {
		return PartMeta{}, io.EOF
	}

	part, next, closing, err := parsePartAt(g, u, u.raw, u.boundary, u.topSearch, 0, nil)
	if err != nil {
		g.rejectUploadLocked(u)
		return PartMeta{}, err
	}
	if closing {
		u.topSearch = next
		u.terminated = true
		u.current = nil
		u.readPos = 0
		return PartMeta{}, io.EOF
	}

	g.acceptPartLocked(u, part)
	u.topSearch = next
	u.current = part
	u.readPos = 0
	return part.meta, nil
}

// NextNested descends into the currently selected nested part.
func (g *MultipartIntakeGuard) NextNested(uploadID string) (PartMeta, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return PartMeta{}, ErrUnknownUpload
	}
	if u.state != UploadOpen || u.current == nil {
		return PartMeta{}, ErrBadState
	}

	parent := u.current
	if !parent.meta.Nested {
		g.rejectUploadLocked(u)
		return PartMeta{}, ErrNestedDepth
	}
	depth := parent.depth + 1
	if depth > g.limits.MaxNestedDepth {
		g.rejectUploadLocked(u)
		return PartMeta{}, ErrNestedDepth
	}
	if parent.nestedDone {
		return PartMeta{}, io.EOF
	}

	part, next, closing, err := parsePartAt(
		g, u, parent.payload, parent.nestedBoundary, parent.nestedSearch, depth, parent,
	)
	if err != nil {
		g.rejectUploadLocked(u)
		return PartMeta{}, err
	}
	if closing {
		parent.nestedSearch = next
		parent.nestedDone = true
		u.current = nil
		u.readPos = 0
		return PartMeta{}, io.EOF
	}

	parent.nestedSearch = next
	g.acceptPartLocked(u, part)
	u.current = part
	u.readPos = 0
	return part.meta, nil
}

// ReadPart streams the payload of the current part.
func (g *MultipartIntakeGuard) ReadPart(uploadID string, p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return 0, ErrUnknownUpload
	}
	if u.state != UploadOpen || u.current == nil {
		return 0, ErrBadState
	}
	if len(p) == 0 {
		return 0, nil
	}
	if u.readPos >= len(u.current.payload) {
		return 0, io.EOF
	}
	n := copy(p, u.current.payload[u.readPos:])
	u.readPos += n
	return n, nil
}

// CommitPart stores the payload of one part under its index.
func (g *MultipartIntakeGuard) CommitPart(uploadID string, index int, payload []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return ErrUnknownUpload
	}
	if u.state != UploadOpen {
		return ErrBadState
	}
	if index < 0 || index >= len(u.parts) {
		return ErrPartIndex
	}

	part := u.parts[index]
	if part.committed {
		return ErrPartConflict
	}
	if int64(len(payload)) != part.meta.Size {
		return ErrPartSize
	}

	part.committedPayload = append([]byte(nil), payload...)
	part.committed = true
	if part.isField && !part.duplicateField && part.meta.Name != "" {
		if _, exists := u.fieldValues[part.meta.Name]; !exists {
			u.fieldValues[part.meta.Name] = string(payload)
		}
	}
	return nil
}

// Finish seals the upload, returning a receipt with the payload digest.
func (g *MultipartIntakeGuard) Finish(uploadID string) (Receipt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return Receipt{}, ErrUnknownUpload
	}
	if u.state != UploadOpen || !u.terminated {
		return Receipt{}, ErrBadState
	}

	leaves := make([]*partRecord, 0, len(u.parts))
	for _, part := range u.parts {
		if part.meta.Nested {
			continue
		}
		if !part.committed {
			return Receipt{}, ErrBadState
		}
		leaves = append(leaves, part)
	}
	sort.Slice(leaves, func(i, j int) bool {
		return leaves[i].meta.Index < leaves[j].meta.Index
	})

	h := sha256.New()
	var total int64
	for _, part := range leaves {
		h.Write(part.committedPayload)
		total += int64(len(part.committedPayload))
	}
	receipt := Receipt{
		UploadID: uploadID,
		Parts:    len(leaves),
		Bytes:    total,
		Digest:   hex.EncodeToString(h.Sum(nil)),
		At:       time.Now(),
	}

	delete(g.uploads, uploadID)
	g.releaseUploadLocked(u)
	g.ensureMapsLocked()
	g.receipts[uploadID] = receipt
	g.stats.Accepted++
	g.stats.Bytes += uint64(total)
	return receipt, nil
}

// Abort drops the upload and reclaims its buffer.
func (g *MultipartIntakeGuard) Abort(uploadID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if u, ok := g.uploads[uploadID]; ok {
		g.rejectUploadLocked(u)
		return nil
	}
	if _, ok := g.aborted[uploadID]; ok {
		return nil
	}
	return ErrUnknownUpload
}

// Inspect reads one upload record.
func (g *MultipartIntakeGuard) Inspect(uploadID string) (Upload, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return Upload{}, false
	}
	return uploadRecord(u), true
}

// Active lists open uploads ordered by id.
func (g *MultipartIntakeGuard) Active() []Upload {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]Upload, 0, len(g.uploads))
	for _, u := range g.uploads {
		out = append(out, uploadRecord(u))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// Expire aborts uploads idle for longer than the configured timeout.
func (g *MultipartIntakeGuard) Expire(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	expired := 0
	for id, u := range g.uploads {
		if now.Sub(u.lastWrite) <= g.limits.IdleTimeout {
			continue
		}
		delete(g.uploads, id)
		g.releaseUploadLocked(u)
		g.ensureMapsLocked()
		g.aborted[id] = struct{}{}
		g.stats.Rejected++
		expired++
	}
	return expired
}

// FieldValue reads a committed plain form field value.
func (g *MultipartIntakeGuard) FieldValue(uploadID, name string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	u, ok := g.uploads[uploadID]
	if !ok {
		return "", false
	}
	value, ok := u.fieldValues[name]
	return value, ok
}

// ReceiptOf reads the receipt of one finished upload.
func (g *MultipartIntakeGuard) ReceiptOf(uploadID string) (Receipt, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	receipt, ok := g.receipts[uploadID]
	return receipt, ok
}

// Stats reports the intake counters.
func (g *MultipartIntakeGuard) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()

	stats := g.stats
	stats.Active = len(g.uploads)
	return stats
}

// Snapshot serialises receipts and the counters.
func (g *MultipartIntakeGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.ensureMapsLocked()
	state := snapshotState{
		Receipts: make([]Receipt, 0, len(g.receipts)),
		Stats:    g.stats,
		Active:   make([]string, 0, len(g.uploads)),
		NextID:   g.nextID,
	}
	state.Stats.Active = len(g.uploads)
	for _, receipt := range g.receipts {
		state.Receipts = append(state.Receipts, receipt)
	}
	for id := range g.uploads {
		state.Active = append(state.Active, id)
	}
	sort.Slice(state.Receipts, func(i, j int) bool {
		return state.Receipts[i].UploadID < state.Receipts[j].UploadID
	})
	sort.Strings(state.Active)

	rawState, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(rawState)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(sum[:]),
		State:    rawState,
	}
	return json.Marshal(envelope)
}

// Restore loads a snapshot and returns the number of dropped in flight uploads.
func (g *MultipartIntakeGuard) Restore(data []byte) (int, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return 0, ErrBadSnapshot
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return 0, ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.State)
	if !strings.EqualFold(envelope.Checksum, hex.EncodeToString(sum[:])) {
		return 0, ErrBadSnapshot
	}

	var state snapshotState
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return 0, ErrBadSnapshot
	}
	receiptIDs := make(map[string]struct{}, len(state.Receipts))
	for _, receipt := range state.Receipts {
		if !receipt.valid() {
			return 0, ErrBadSnapshot
		}
		if _, exists := receiptIDs[receipt.UploadID]; exists {
			return 0, ErrBadSnapshot
		}
		receiptIDs[receipt.UploadID] = struct{}{}
	}
	activeIDs := make(map[string]struct{}, len(state.Active))
	for _, id := range state.Active {
		if id == "" {
			return 0, ErrBadSnapshot
		}
		if _, exists := activeIDs[id]; exists {
			return 0, ErrBadSnapshot
		}
		activeIDs[id] = struct{}{}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureMapsLocked()

	droppedSet := make(map[string]struct{}, len(state.Active)+len(g.uploads))
	for id := range activeIDs {
		droppedSet[id] = struct{}{}
	}
	for id := range g.uploads {
		droppedSet[id] = struct{}{}
	}
	for _, u := range g.uploads {
		g.releaseUploadLocked(u)
	}

	receipts := make(map[string]Receipt, len(state.Receipts))
	for _, receipt := range state.Receipts {
		receipts[receipt.UploadID] = receipt
	}
	g.receipts = receipts
	g.uploads = make(map[string]*uploadState)
	g.aborted = make(map[string]struct{})

	nextID := state.NextID
	for id := range receiptIDs {
		if n, ok := uploadNumber(id); ok && n > nextID {
			nextID = n
		}
	}
	for id := range activeIDs {
		if n, ok := uploadNumber(id); ok && n > nextID {
			nextID = n
		}
	}
	g.nextID = nextID

	dropped := len(droppedSet)
	g.stats = state.Stats
	g.stats.Active = 0
	g.stats.Rejected += uint64(dropped)
	return dropped, nil
}

func (g *MultipartIntakeGuard) ensureMapsLocked() {
	if g.uploads == nil {
		g.uploads = make(map[string]*uploadState)
	}
	if g.receipts == nil {
		g.receipts = make(map[string]Receipt)
	}
	if g.aborted == nil {
		g.aborted = make(map[string]struct{})
	}
}

func (g *MultipartIntakeGuard) rejectUploadLocked(u *uploadState) {
	if _, ok := g.uploads[u.id]; !ok {
		return
	}
	delete(g.uploads, u.id)
	g.releaseUploadLocked(u)
	g.ensureMapsLocked()
	g.aborted[u.id] = struct{}{}
	g.stats.Rejected++
}

func (g *MultipartIntakeGuard) acceptPartLocked(u *uploadState, part *partRecord) {
	part.meta.Index = u.nextPart
	u.nextPart++
	if part.isField && part.meta.Name != "" {
		if _, exists := u.seenFields[part.meta.Name]; exists {
			part.duplicateField = true
		} else {
			u.seenFields[part.meta.Name] = struct{}{}
		}
	}
	u.parts = append(u.parts, part)
}

func (g *MultipartIntakeGuard) releaseUploadLocked(u *uploadState) {
	for _, part := range u.parts {
		part.payload = nil
		part.committedPayload = nil
		part.parent = nil
	}
	u.raw = nil
	u.parts = nil
	u.current = nil
	u.seenFields = nil
	u.fieldValues = nil
}

func uploadRecord(u *uploadState) Upload {
	return Upload{
		ID:       u.id,
		Boundary: u.boundary,
		State:    u.state,
		Parts:    len(u.parts),
		Bytes:    u.received,
		Created:  u.created,
	}
}

func boundaryFromContentType(contentType string) (string, error) {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return "", ErrMissingBoundary
	}

	mediaType, params, err := mime.ParseMediaType(trimmed)
	if err != nil {
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "multipart/form-data") && strings.Contains(lower, "boundary") {
			return "", ErrBadBoundary
		}
		return "", ErrMissingBoundary
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") {
		return "", ErrMissingBoundary
	}
	boundary, ok := params["boundary"]
	if !ok {
		return "", ErrMissingBoundary
	}
	return validateBoundary(boundary)
}

func validateBoundary(boundary string) (string, error) {
	if boundary == "" {
		return "", ErrBadBoundary
	}
	if len(boundary) > 70 {
		return "", ErrBadBoundary
	}
	for _, r := range boundary {
		if r < 32 || r == 127 || unicode.IsSpace(r) {
			return "", ErrBadBoundary
		}
	}
	return boundary, nil
}

func normalizeFilename(name string, max int) (string, error) {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	if base == "" || base == "." || base == ".." || base == "/" {
		return "", ErrBadFilename
	}
	if max >= 0 && len(base) > max {
		return "", ErrNameTooLong
	}
	stem := base
	if i := strings.IndexByte(base, '.'); i >= 0 {
		stem = base[:i]
	}
	if _, reserved := reservedFilenames[strings.ToUpper(stem)]; reserved {
		return "", ErrBadFilename
	}
	return base, nil
}

func transferEncodingAllowed(h textproto.MIMEHeader) bool {
	values := h.Values("Content-Transfer-Encoding")
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "7bit", "8bit":
			default:
				return false
			}
		}
	}
	return true
}

func duplicatePolicyRejects(policy string) bool {
	return strings.EqualFold(strings.TrimSpace(policy), DuplicateReject)
}

func (g *MultipartIntakeGuard) nestedTypeAllowed(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	for _, allowed := range g.limits.NestedTypes {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == mediaType {
			return true
		}
		if strings.HasSuffix(allowed, "/*") &&
			strings.HasPrefix(mediaType, strings.TrimSuffix(allowed, "*")) {
			return true
		}
	}
	return false
}

func parsePartAt(
	g *MultipartIntakeGuard,
	u *uploadState,
	data []byte,
	boundary string,
	from int,
	depth int,
	parent *partRecord,
) (*partRecord, int, bool, error) {
	_, next, closing, found, err := locateDelimiter(data, boundary, from)
	if err != nil {
		return nil, 0, false, err
	}
	if !found {
		return nil, 0, false, ErrBadState
	}
	if closing {
		return nil, next, true, nil
	}
	if len(u.parts) >= g.limits.MaxParts {
		return nil, 0, false, ErrPartLimit
	}

	headers, bodyStart, err := readPartHeaders(data, next, g.limits.MaxHeaderBytes)
	if err != nil {
		return nil, 0, false, err
	}

	payloadStart := bodyStart
	payloadEnd, _, _, payloadFound, err := locateDelimiter(data, boundary, bodyStart)
	if err != nil {
		return nil, 0, false, err
	}
	if !payloadFound {
		return nil, 0, false, ErrBadState
	}
	nextPartAt := payloadEnd
	payloadEnd = trimDelimiterPrefix(data, payloadStart, payloadEnd)
	if payloadEnd < payloadStart {
		payloadEnd = payloadStart
	}

	if !transferEncodingAllowed(headers) {
		return nil, 0, false, ErrTransferEncoding
	}

	contentType := headers.Get("Content-Type")
	mediaType := ""
	params := map[string]string(nil)
	if strings.TrimSpace(contentType) != "" {
		mediaType, params, err = mime.ParseMediaType(contentType)
		if err != nil {
			return nil, 0, false, ErrBadState
		}
		mediaType = strings.ToLower(mediaType)
	}
	nested := strings.HasPrefix(mediaType, "multipart/")
	nestedBoundary := ""
	if nested {
		if !g.nestedTypeAllowed(mediaType) {
			return nil, 0, false, ErrNestedType
		}
		value, ok := params["boundary"]
		if !ok {
			return nil, 0, false, ErrMissingBoundary
		}
		nestedBoundary, err = validateBoundary(value)
		if err != nil {
			return nil, 0, false, err
		}
	}

	name := ""
	rawFilename := ""
	hasFilename := false
	disposition := headers.Get("Content-Disposition")
	if strings.TrimSpace(disposition) != "" {
		_, dispositionParams, parseErr := mime.ParseMediaType(disposition)
		if parseErr != nil {
			return nil, 0, false, ErrBadState
		}
		if value, ok := dispositionParams["name"]; ok {
			name = value
		} else if value, ok := dispositionParams["name*"]; ok {
			name = value
		}
		if value, ok := dispositionParams["filename"]; ok {
			rawFilename = value
			hasFilename = true
		} else if value, ok := dispositionParams["filename*"]; ok {
			rawFilename = value
			hasFilename = true
		}
	}

	filename := ""
	if hasFilename {
		filename, err = normalizeFilename(rawFilename, g.limits.MaxNameLen)
		if err != nil {
			return nil, 0, false, err
		}
	}

	isField := !nested && !hasFilename
	if isField && name != "" && duplicatePolicyRejects(g.limits.DuplicatePolicy) {
		if _, exists := u.seenFields[name]; exists {
			return nil, 0, false, ErrDuplicateField
		}
	}

	part := &partRecord{
		meta: PartMeta{
			Name:        name,
			Filename:    filename,
			ContentType: mediaType,
			Size:        int64(payloadEnd - payloadStart),
			Nested:      nested,
		},
		payload:        data[payloadStart:payloadEnd],
		isField:        isField,
		parent:         parent,
		depth:          depth,
		nestedBoundary: nestedBoundary,
	}
	return part, nextPartAt, false, nil
}

func locateDelimiter(data []byte, boundary string, from int) (int, int, bool, bool, error) {
	if from < 0 {
		from = 0
	}
	prefix := []byte("--" + boundary)
	for i := from; i+len(prefix) <= len(data); i++ {
		if i != 0 && data[i-1] != '\n' {
			continue
		}
		if !bytes.Equal(data[i:i+len(prefix)], prefix) {
			continue
		}

		j := i + len(prefix)
		closing := false
		if j < len(data) && data[j] == '-' {
			if j+1 >= len(data) {
				return 0, 0, false, false, nil
			}
			if data[j+1] != '-' {
				return 0, 0, false, false, ErrBoundaryAmbiguous
			}
			closing = true
			j += 2
		}
		for j < len(data) && (data[j] == ' ' || data[j] == '\t') {
			j++
		}
		if j == len(data) {
			if closing {
				return i, j, true, true, nil
			}
			return 0, 0, false, false, nil
		}
		if data[j] == '\r' {
			if j+1 >= len(data) {
				return 0, 0, false, false, nil
			}
			if data[j+1] != '\n' {
				return 0, 0, false, false, ErrBoundaryAmbiguous
			}
			return i, j + 2, closing, true, nil
		}
		if data[j] == '\n' {
			return i, j + 1, closing, true, nil
		}
		return 0, 0, false, false, ErrBoundaryAmbiguous
	}
	return 0, 0, false, false, nil
}

func trimDelimiterPrefix(data []byte, start, end int) int {
	if end >= start+2 && data[end-2] == '\r' && data[end-1] == '\n' {
		return end - 2
	}
	if end > start && data[end-1] == '\n' {
		return end - 1
	}
	return end
}

func readPartHeaders(data []byte, start, max int) (textproto.MIMEHeader, int, error) {
	headerTextEnd, blankEnd, ok := headerTerminator(data, start)
	if !ok {
		remaining := len(data) - start
		if max >= 0 && remaining > max {
			return nil, 0, ErrHeaderLimit
		}
		return nil, 0, ErrBadState
	}
	if max >= 0 && blankEnd-start > max {
		return nil, 0, ErrHeaderLimit
	}

	block := make([]byte, 0, headerTextEnd-start+2)
	block = append(block, data[start:headerTextEnd]...)
	if blankEnd-start > 0 && data[blankEnd-1] == '\n' {
		if blankEnd >= 2 && data[blankEnd-2] == '\r' {
			block = append(block, '\r', '\n')
		} else {
			block = append(block, '\n')
		}
	} else {
		block = append(block, '\r', '\n')
	}
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(block)))
	headers, err := reader.ReadMIMEHeader()
	if err != nil {
		return nil, 0, ErrBadState
	}
	return headers, blankEnd, nil
}

func headerTerminator(data []byte, start int) (int, int, bool) {
	if start >= len(data) {
		return 0, 0, false
	}
	if data[start] == '\r' {
		if start+1 < len(data) && data[start+1] == '\n' {
			return start, start + 2, true
		}
		return 0, 0, false
	}
	if data[start] == '\n' {
		return start, start + 1, true
	}

	cr := bytes.Index(data[start:], []byte("\r\n\r\n"))
	lf := bytes.Index(data[start:], []byte("\n\n"))
	switch {
	case cr >= 0 && (lf < 0 || cr <= lf):
		idx := start + cr
		return idx + 2, idx + 4, true
	case lf >= 0:
		idx := start + lf
		return idx + 1, idx + 2, true
	default:
		return 0, 0, false
	}
}

func (r Receipt) valid() bool {
	if r.UploadID == "" || r.Parts < 0 || r.Bytes < 0 || len(r.Digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(r.Digest)
	return err == nil
}

func uploadNumber(id string) (uint64, bool) {
	if !strings.HasPrefix(id, "upload-") {
		return 0, false
	}
	var n uint64
	_, err := fmt.Sscanf(id, "upload-%d", &n)
	return n, err == nil
}
