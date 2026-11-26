// Package task judges whether an uploaded script behaves like a web shell: it
// unwraps a bounded number of encoding layers, scores the dangerous capability
// combinations left behind and keeps the verdict on a content hash so a known
// good hash can be muted for a while.
package task

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verdict states.
const (
	StateMalicious   = "malicious"
	StateSuspicious  = "suspicious"
	StateBenign      = "benign"
	StateWhitelisted = "whitelisted"
)

// Capability feature names.
const (
	FeatureExec      = "exec"
	FeatureDecode    = "decode"
	FeatureNet       = "net"
	FeatureFileWrite = "file_write"
)

// Errors reported by the classifier.
var (
	ErrBadFile     = errors.New("file reference is incomplete")
	ErrTooLarge    = errors.New("file exceeds the size cap")
	ErrBadEncoding = errors.New("encoded payload cannot be decoded")
	ErrBadLayers   = errors.New("layer budget must be positive")
	ErrNoVerdict   = errors.New("file has no verdict yet")
	ErrBadHash     = errors.New("content hash is required")
	ErrBadTTL      = errors.New("whitelist ttl must be positive")
	ErrBadSnapshot = errors.New("snapshot payload is invalid")
)

const (
	scoreExec      = 40
	scoreNet       = 25
	scoreDecode    = 20
	scoreFileWrite = 15

	snapshotVersion = 1
)

const (
	defaultMaxBytes  = 1 << 20
	defaultMaxLayers = 3
	defaultMaxFiles  = 128
	defaultCacheTTL  = time.Minute
)

// FileRef is one uploaded or dropped script.
type FileRef struct {
	ID      string
	Path    string
	Content string
}

// Decoded is the result of unwrapping the encoding layers of a file.
type Decoded struct {
	FileID    string
	Layers    int
	Content   string
	Bytes     int
	Truncated bool
}

// Verdict is the classification of one file. Evidence contains the call sites
// that made a capability count; it is deliberately separate from Features so
// the latter is always a sorted, duplicate-free capability list.
type Verdict struct {
	FileID   string
	Hash     string
	State    string
	Score    int
	Features []string
	Cached   bool
	Layers   int
	Evidence []string
}

// Features is the explainable view of one verdict.
type Features struct {
	FileID   string
	Hash     string
	Score    int
	State    string
	Features []string
	Evidence []string
}

// Limits bounds the decoding and the verdict table.
type Limits struct {
	MaxBytes  int
	MaxLayers int
	MaxFiles  int
	CacheTTL  time.Duration
}

type cachedVerdict struct {
	ref       FileRef
	verdict   Verdict
	scannedAt time.Time
	expiresAt time.Time
	sequence  uint64
}

type whitelistMark struct {
	createdAt time.Time
	expiresAt time.Time
}

type fileGate struct {
	mu    sync.Mutex
	users int
}

// fileLocker serialises operations for the same file without serialising work
// for other files. Gates are reference counted so a long stream of IDs cannot
// make the lock table grow without bound.
type fileLocker struct {
	mu    sync.Mutex
	gates map[string]*fileGate
}

func newFileLocker() fileLocker {
	return fileLocker{gates: make(map[string]*fileGate)}
}

func (l *fileLocker) lock(key string) func() {
	l.mu.Lock()
	gate := l.gates[key]
	if gate == nil {
		gate = &fileGate{}
		l.gates[key] = gate
	}
	gate.users++
	l.mu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		l.mu.Lock()
		gate.users--
		if gate.users == 0 {
			delete(l.gates, key)
		}
		l.mu.Unlock()
	}
}

// WebshellClassifier owns the verdict cache and the hash whitelist.
type WebshellClassifier struct {
	limits Limits
	gates  fileLocker

	mu        sync.RWMutex
	verdicts  map[string]*cachedVerdict
	whitelist map[string]whitelistMark
	sequence  uint64
}

// NewWebshellClassifier builds a classifier with the given caps. Non-positive
// limits are replaced by conservative defaults because the constructor cannot
// return an error.
func NewWebshellClassifier(limits Limits) *WebshellClassifier {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaultMaxBytes
	}
	if limits.MaxLayers <= 0 {
		limits.MaxLayers = defaultMaxLayers
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = defaultMaxFiles
	}
	if limits.CacheTTL <= 0 {
		limits.CacheTTL = defaultCacheTTL
	}
	return &WebshellClassifier{
		limits:    limits,
		gates:     newFileLocker(),
		verdicts:  make(map[string]*cachedVerdict),
		whitelist: make(map[string]whitelistMark),
	}
}

// Scan decodes a file, scores its capabilities and records the verdict.
func (c *WebshellClassifier) Scan(file FileRef) (Verdict, error) {
	if err := validateFile(file); err != nil {
		return Verdict{}, err
	}

	unlock := c.gates.lock(file.ID)
	defer unlock()

	if len([]byte(file.Content)) > c.limits.MaxBytes {
		c.forgetLockedFile(file.ID)
		return Verdict{}, ErrTooLarge
	}
	return c.scanFile(file, false)
}

// Decode unwraps at most maxLayers encoding layers of a file.
func (c *WebshellClassifier) Decode(file FileRef, maxLayers int) (Decoded, error) {
	if err := validateFile(file); err != nil {
		return Decoded{}, err
	}
	if len([]byte(file.Content)) > c.limits.MaxBytes {
		return Decoded{}, ErrTooLarge
	}

	budget, err := c.layerBudget(maxLayers)
	if err != nil {
		return Decoded{}, err
	}
	decoded, err := c.decodeFile(file, budget)
	if err != nil {
		return Decoded{}, err
	}
	return decoded.result, nil
}

// Explain returns the features behind the stored verdict of one file.
func (c *WebshellClassifier) Explain(fileID string) (Features, error) {
	c.mu.RLock()
	entry := c.verdicts[fileID]
	if entry == nil {
		c.mu.RUnlock()
		return Features{}, ErrNoVerdict
	}
	result := Features{
		FileID:   entry.verdict.FileID,
		Hash:     entry.verdict.Hash,
		Score:    entry.verdict.Score,
		State:    entry.verdict.State,
		Features: cloneStrings(entry.verdict.Features),
		Evidence: cloneStrings(entry.verdict.Evidence),
	}
	c.mu.RUnlock()
	return result, nil
}

// Whitelist mutes one content hash until the ttl runs out.
func (c *WebshellClassifier) Whitelist(hash string, ttl time.Duration) error {
	normalized := normalizeHash(hash)
	if normalized == "" {
		return ErrBadHash
	}
	if ttl <= 0 {
		return ErrBadTTL
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.whitelist[normalized] = whitelistMark{
		createdAt: now,
		expiresAt: now.Add(ttl),
	}
	for _, entry := range c.verdicts {
		if entry.verdict.Hash == normalized {
			entry.verdict = whitelistedVerdict(entry.verdict)
		}
	}
	return nil
}

// Rescan recomputes the verdict of a file that was scanned before.
func (c *WebshellClassifier) Rescan(fileID string) error {
	unlock := c.gates.lock(fileID)
	defer unlock()

	c.mu.RLock()
	entry := c.verdicts[fileID]
	c.mu.RUnlock()
	if entry == nil {
		return ErrNoVerdict
	}

	_, err := c.scanFile(entry.ref, true)
	return err
}

// Prune drops expired whitelist entries and stale verdicts. now is the single
// authority for expiry comparisons, which keeps cache reclamation independent
// of the clocks on individual hosts.
func (c *WebshellClassifier) Prune(now time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	for fileID, entry := range c.verdicts {
		if !now.Before(entry.expiresAt) {
			delete(c.verdicts, fileID)
			removed++
		}
	}
	for hash, mark := range c.whitelist {
		if !now.Before(mark.expiresAt) {
			delete(c.whitelist, hash)
			removed++
		}
	}
	return removed, nil
}

// Hashes lists the whitelisted content hashes in ascending order.
func (c *WebshellClassifier) Hashes() []string {
	now := time.Now()
	c.mu.RLock()
	hashes := make([]string, 0, len(c.whitelist))
	for hash, mark := range c.whitelist {
		if now.Before(mark.expiresAt) {
			hashes = append(hashes, hash)
		}
	}
	c.mu.RUnlock()
	sort.Strings(hashes)
	return hashes
}

// VerdictFor reads the stored verdict of one file.
func (c *WebshellClassifier) VerdictFor(fileID string) (Verdict, bool) {
	c.mu.RLock()
	entry := c.verdicts[fileID]
	if entry == nil {
		c.mu.RUnlock()
		return Verdict{}, false
	}
	result := cloneVerdict(entry.verdict)
	c.mu.RUnlock()
	return result, true
}

// Forget drops one cached verdict and frees its slot. It is idempotent.
func (c *WebshellClassifier) Forget(fileID string) error {
	unlock := c.gates.lock(fileID)
	defer unlock()
	c.forgetLockedFile(fileID)
	return nil
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type snapshotPayload struct {
	Verdicts  []snapshotVerdict   `json:"verdicts"`
	Whitelist []snapshotWhitelist `json:"whitelist"`
}

type snapshotVerdict struct {
	FileID    string   `json:"file_id"`
	Path      string   `json:"path"`
	Content   string   `json:"content"`
	Hash      string   `json:"hash"`
	State     string   `json:"state"`
	Score     int      `json:"score"`
	Features  []string `json:"features"`
	Evidence  []string `json:"evidence"`
	Cached    bool     `json:"cached"`
	Layers    int      `json:"layers"`
	ScannedAt int64    `json:"scanned_at"`
	ExpiresAt int64    `json:"expires_at"`
	Sequence  uint64   `json:"sequence"`
}

type snapshotWhitelist struct {
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// Snapshot serialises the whitelist and the verdict cache.
func (c *WebshellClassifier) Snapshot() ([]byte, error) {
	c.mu.RLock()
	payload := snapshotPayload{
		Verdicts:  make([]snapshotVerdict, 0, len(c.verdicts)),
		Whitelist: make([]snapshotWhitelist, 0, len(c.whitelist)),
	}
	for _, entry := range c.verdicts {
		payload.Verdicts = append(payload.Verdicts, snapshotVerdict{
			FileID:    entry.ref.ID,
			Path:      entry.ref.Path,
			Content:   entry.ref.Content,
			Hash:      entry.verdict.Hash,
			State:     entry.verdict.State,
			Score:     entry.verdict.Score,
			Features:  cloneStrings(entry.verdict.Features),
			Evidence:  cloneStrings(entry.verdict.Evidence),
			Cached:    entry.verdict.Cached,
			Layers:    entry.verdict.Layers,
			ScannedAt: entry.scannedAt.UnixNano(),
			ExpiresAt: entry.expiresAt.UnixNano(),
			Sequence:  entry.sequence,
		})
	}
	for hash, mark := range c.whitelist {
		payload.Whitelist = append(payload.Whitelist, snapshotWhitelist{
			Hash:      hash,
			CreatedAt: mark.createdAt.UnixNano(),
			ExpiresAt: mark.expiresAt.UnixNano(),
		})
	}
	c.mu.RUnlock()

	sort.Slice(payload.Verdicts, func(i, j int) bool {
		return payload.Verdicts[i].FileID < payload.Verdicts[j].FileID
	})
	sort.Slice(payload.Whitelist, func(i, j int) bool {
		return payload.Whitelist[i].Hash < payload.Whitelist[j].Hash
	})

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrBadSnapshot
	}
	sum := sha256.Sum256(rawPayload)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Payload:  rawPayload,
		Checksum: hex.EncodeToString(sum[:]),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, ErrBadSnapshot
	}
	return data, nil
}

// Restore replaces the state with a snapshot, all or nothing. The checksum
// catches byte damage; the field validation also rejects a syntactically valid
// but internally inconsistent payload before live state is touched.
func (c *WebshellClassifier) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := decodeStrictJSON(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || len(envelope.Payload) == 0 {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Payload)
	expected := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(envelope.Checksum), []byte(expected)) != 1 {
		return ErrBadSnapshot
	}

	var payload snapshotPayload
	if err := decodeStrictJSON(envelope.Payload, &payload); err != nil {
		return ErrBadSnapshot
	}
	if len(payload.Verdicts) > c.limits.MaxFiles {
		return ErrBadSnapshot
	}

	verdicts := make(map[string]*cachedVerdict, len(payload.Verdicts))
	var maxSequence uint64
	for _, item := range payload.Verdicts {
		if strings.TrimSpace(item.FileID) == "" || strings.TrimSpace(item.Path) == "" {
			return ErrBadSnapshot
		}
		if item.Hash != contentDigest(item.Content) {
			return ErrBadSnapshot
		}
		if !validState(item.State) || item.Score < 0 || item.Score > 100 {
			return ErrBadSnapshot
		}
		if item.Layers < 0 || item.Layers > c.limits.MaxLayers {
			return ErrBadSnapshot
		}
		if !validFeatures(item.Features) {
			return ErrBadSnapshot
		}
		if item.State == StateWhitelisted && (item.Score != 0 || len(item.Features) != 0 || len(item.Evidence) != 0) {
			return ErrBadSnapshot
		}
		if item.ScannedAt <= 0 || item.ExpiresAt <= item.ScannedAt {
			return ErrBadSnapshot
		}
		if _, exists := verdicts[item.FileID]; exists {
			return ErrBadSnapshot
		}
		if item.Sequence > maxSequence {
			maxSequence = item.Sequence
		}
		verdict := Verdict{
			FileID:   item.FileID,
			Hash:     item.Hash,
			State:    item.State,
			Score:    item.Score,
			Features: cloneStrings(item.Features),
			Cached:   item.Cached,
			Layers:   item.Layers,
			Evidence: cloneStrings(item.Evidence),
		}
		verdicts[item.FileID] = &cachedVerdict{
			ref: FileRef{
				ID:      item.FileID,
				Path:    item.Path,
				Content: item.Content,
			},
			verdict:   verdict,
			scannedAt: time.Unix(0, item.ScannedAt),
			expiresAt: time.Unix(0, item.ExpiresAt),
			sequence:  item.Sequence,
		}
	}

	whitelist := make(map[string]whitelistMark, len(payload.Whitelist))
	for _, item := range payload.Whitelist {
		hash := normalizeHash(item.Hash)
		if hash == "" || hash != item.Hash {
			return ErrBadSnapshot
		}
		if item.CreatedAt <= 0 || item.ExpiresAt <= item.CreatedAt {
			return ErrBadSnapshot
		}
		if _, exists := whitelist[hash]; exists {
			return ErrBadSnapshot
		}
		whitelist[hash] = whitelistMark{
			createdAt: time.Unix(0, item.CreatedAt),
			expiresAt: time.Unix(0, item.ExpiresAt),
		}
	}

	c.mu.Lock()
	c.verdicts = verdicts
	c.whitelist = whitelist
	c.sequence = maxSequence
	c.mu.Unlock()
	return nil
}

type decodedFile struct {
	result Decoded
	layers []string
}

func (c *WebshellClassifier) scanFile(file FileRef, force bool) (Verdict, error) {
	hash := contentDigest(file.Content)
	now := time.Now()

	if !force {
		c.mu.Lock()
		entry := c.verdicts[file.ID]
		if entry != nil &&
			entry.ref.Path == file.Path &&
			entry.verdict.Hash == hash &&
			now.Before(entry.expiresAt) {
			active := c.whitelistActiveLocked(hash, now)
			if active {
				entry.verdict = whitelistedVerdict(entry.verdict)
				entry.verdict.Cached = true
				verdict := cloneVerdict(entry.verdict)
				c.mu.Unlock()
				return verdict, nil
			}
			if entry.verdict.State != StateWhitelisted {
				entry.verdict.Cached = true
				verdict := cloneVerdict(entry.verdict)
				c.mu.Unlock()
				return verdict, nil
			}
		}
		c.mu.Unlock()
	}

	decoded, err := c.decodeFile(file, c.limits.MaxLayers)
	if err != nil {
		c.forgetLockedFile(file.ID)
		return Verdict{}, err
	}

	score, features, evidence := evaluateLayers(file.Path, decoded.layers, decoded.result.Truncated)
	state := stateForScore(score)
	if decoded.result.Truncated && state == StateBenign {
		state = StateSuspicious
	}
	verdict := Verdict{
		FileID:   file.ID,
		Hash:     hash,
		State:    state,
		Score:    score,
		Features: features,
		Layers:   decoded.result.Layers,
		Evidence: evidence,
	}

	commitTime := time.Now()
	c.mu.Lock()
	if c.whitelistActiveLocked(hash, commitTime) {
		verdict = whitelistedVerdict(verdict)
	}
	entry := &cachedVerdict{
		ref:       file,
		verdict:   verdict,
		scannedAt: commitTime,
		expiresAt: commitTime.Add(c.limits.CacheTTL),
	}
	c.storeLocked(entry)
	result := cloneVerdict(entry.verdict)
	c.mu.Unlock()
	return result, nil
}

func (c *WebshellClassifier) layerBudget(requested int) (int, error) {
	if requested <= 0 {
		return 0, ErrBadLayers
	}
	if requested > c.limits.MaxLayers {
		return c.limits.MaxLayers, nil
	}
	return requested, nil
}

type encodingKind int

const (
	encodingNone encodingKind = iota
	encodingBase64
	encodingHex
	encodingURL
)

func (c *WebshellClassifier) decodeFile(file FileRef, maxLayers int) (decodedFile, error) {
	content := file.Content
	layers := []string{content}
	decodedLayers := 0
	truncated := false

	for decodedLayers < maxLayers {
		kind, payload, ok := splitEncodingPrefix(content)
		if !ok {
			break
		}
		next, err := decodePayload(kind, payload)
		if err != nil {
			return decodedFile{}, ErrBadEncoding
		}
		if len([]byte(next)) > c.limits.MaxBytes {
			// The layer hit the size ceiling. Stop at the last fully accepted
			// layer and keep its already observed evidence.
			truncated = true
			break
		}
		content = next
		decodedLayers++
		layers = append(layers, content)
	}

	if _, _, ok := splitEncodingPrefix(content); ok {
		truncated = true
	}

	result := Decoded{
		FileID:    file.ID,
		Layers:    decodedLayers,
		Content:   content,
		Bytes:     len([]byte(content)),
		Truncated: truncated,
	}
	return decodedFile{result: result, layers: layers}, nil
}

func splitEncodingPrefix(content string) (encodingKind, string, bool) {
	if len(content) < 4 {
		return encodingNone, "", false
	}
	switch {
	case strings.EqualFold(content[:4], "b64:"):
		return encodingBase64, content[4:], true
	case strings.EqualFold(content[:4], "hex:"):
		return encodingHex, content[4:], true
	case strings.EqualFold(content[:4], "url:"):
		return encodingURL, content[4:], true
	default:
		return encodingNone, "", false
	}
}

func decodePayload(kind encodingKind, payload string) (string, error) {
	payload = strings.TrimSpace(payload)
	switch kind {
	case encodingBase64:
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", ErrBadEncoding
		}
		return string(decoded), nil
	case encodingHex:
		decoded, err := hex.DecodeString(payload)
		if err != nil {
			return "", ErrBadEncoding
		}
		return string(decoded), nil
	case encodingURL:
		decoded, err := url.QueryUnescape(payload)
		if err != nil {
			return "", ErrBadEncoding
		}
		return decoded, nil
	default:
		return "", ErrBadEncoding
	}
}

type pathClass int

const (
	pathUntrusted pathClass = iota
	pathNeutral
	pathTrusted
)

func classifyPath(path string) (pathClass, string) {
	normalized := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	for _, segment := range strings.Split(normalized, "/") {
		switch segment {
		case "upload", "uploads", "tmp", "temp", "cache", "attachment", "attachments", "dropbox":
			return pathUntrusted, "upload or temporary path"
		}
	}
	for _, extension := range []string{
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".svg", ".ico",
		".tif", ".tiff", ".pdf", ".txt", ".md", ".log",
	} {
		if strings.HasSuffix(normalized, extension) {
			return pathUntrusted, "disguised media path"
		}
	}

	for _, marker := range []string{
		"/app/", "/application/", "/src/", "/vendor/", "/framework/",
		"/frameworks/", "/controllers/", "/controller/", "/models/",
		"/model/", "/views/", "/view/", "/templates/", "/template/",
		"/routes/", "/route/", "/lib/", "/internal/", "/pkg/", "/cmd/",
		"/var/www/", "/srv/www/", "/usr/share/", "/public/", "/web/",
		"/inetpub/wwwroot/",
	} {
		if strings.Contains(normalized, marker) {
			return pathTrusted, "framework path"
		}
	}
	return pathNeutral, "path outside recognized framework roots"
}

type callSite struct {
	name       string
	lowerName  string
	arguments  string
	line       int
	column     int
	definition bool
}

var callCapabilities = map[string]string{
	"runcode":     FeatureExec,
	"evalexpr":    FeatureExec,
	"callnative":  FeatureExec,
	"opensocket":  FeatureNet,
	"posthttp":    FeatureNet,
	"fetchremote": FeatureNet,
	"frombase64":  FeatureDecode,
	"inflateblob": FeatureDecode,
	"rot13text":   FeatureDecode,
	"writefile":   FeatureFileWrite,
	"appendfile":  FeatureFileWrite,
}

type evidenceCall struct {
	layer      int
	line       int
	column     int
	name       string
	capability string
	reason     string
}

func evaluateLayers(path string, layers []string, truncated bool) (int, []string, []string) {
	class, reason := classifyPath(path)
	seen := make(map[string]bool)
	var evidenceCalls []evidenceCall

	for layer, content := range layers {
		for _, site := range findCallSites(content) {
			suspicious, callReason := suspiciousCall(class, reason, site)
			if !suspicious {
				continue
			}
			capability := callCapabilities[site.lowerName]
			if capability == "" {
				continue
			}
			seen[capability] = true
			evidenceCalls = append(evidenceCalls, evidenceCall{
				layer:      layer,
				line:       site.line,
				column:     site.column,
				name:       site.name,
				capability: capability,
				reason:     callReason,
			})
		}
	}

	features := make([]string, 0, len(seen))
	for capability := range seen {
		features = append(features, capability)
	}
	sort.Strings(features)

	score := 0
	for _, capability := range features {
		switch capability {
		case FeatureExec:
			score += scoreExec
		case FeatureNet:
			score += scoreNet
		case FeatureDecode:
			score += scoreDecode
		case FeatureFileWrite:
			score += scoreFileWrite
		}
	}
	if score > 100 {
		score = 100
	}

	sort.Slice(evidenceCalls, func(i, j int) bool {
		left, right := evidenceCalls[i], evidenceCalls[j]
		if left.layer != right.layer {
			return left.layer < right.layer
		}
		if left.line != right.line {
			return left.line < right.line
		}
		if left.column != right.column {
			return left.column < right.column
		}
		return left.name < right.name
	})
	evidence := make([]string, 0, len(evidenceCalls)+1)
	for _, item := range evidenceCalls {
		evidence = append(evidence, fmt.Sprintf(
			"layer %d line %d column %d: %s (%s; %s)",
			item.layer, item.line, item.column, item.name, item.capability, item.reason,
		))
	}
	if truncated {
		evidence = append(evidence, fmt.Sprintf(
			"decoding truncated after %d decoded layer(s); verdict downgraded",
			len(layers)-1,
		))
	}
	return score, features, evidence
}

func suspiciousCall(class pathClass, pathReason string, site callSite) (bool, string) {
	switch class {
	case pathUntrusted:
		if site.definition {
			return true, "dangerous capability wrapper in " + pathReason
		}
		return true, pathReason
	case pathNeutral:
		if site.definition {
			return true, "dangerous capability wrapper in " + pathReason
		}
		return true, pathReason
	case pathTrusted:
		if site.definition {
			return false, pathReason
		}
		if requestControlled(site.arguments) {
			return true, "request-controlled argument"
		}
		return false, "framework context with non-request argument"
	default:
		return true, pathReason
	}
}

func requestControlled(arguments string) bool {
	lower := strings.ToLower(arguments)
	for _, marker := range []string{
		"$_get", "$_post", "$_request", "$_cookie", "$_files",
		"php://input", "getparameter(", "getparam(", "getquery(", "getbody(",
		"->input(", "->request(", "->query(", "->param(", "->body(",
		"->cookie(", "->header(", "request(", "input(", "query(",
		"param(", "body(", "cookie(", "header(",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	for i := 0; i < len(arguments); i++ {
		if arguments[i] != '$' {
			continue
		}
		j := i + 1
		for j < len(arguments) && isIdentifierPart(arguments[j]) {
			j++
		}
		if j == i+1 {
			continue
		}
		name := strings.ToLower(arguments[i+1 : j])
		for _, marker := range []string{
			"input", "param", "query", "body", "cookie", "header",
			"payload", "cmd", "command", "shell",
		} {
			if strings.Contains(name, marker) {
				return true
			}
		}
	}
	return false
}

func findCallSites(content string) []callSite {
	var sites []callSite
	for i := 0; i < len(content); {
		switch {
		case content[i] == '/' && i+1 < len(content) && content[i+1] == '/':
			i = skipLineComment(content, i+2)
			continue
		case content[i] == '#':
			i = skipLineComment(content, i+1)
			continue
		case content[i] == '/' && i+1 < len(content) && content[i+1] == '*':
			i = skipBlockComment(content, i+2)
			continue
		case content[i] == '\'' || content[i] == '"' || content[i] == '`':
			i = skipQuoted(content, i, content[i])
			continue
		}

		if !isIdentifierStart(content[i]) {
			i++
			continue
		}
		start := i
		for i < len(content) && isIdentifierPart(content[i]) {
			i++
		}
		if start > 0 && content[start-1] == '$' {
			continue
		}
		name := content[start:i]
		lowerName := strings.ToLower(name)
		if callCapabilities[lowerName] == "" {
			continue
		}
		j := i
		for j < len(content) && isSpace(content[j]) {
			j++
		}
		if j >= len(content) || content[j] != '(' {
			continue
		}
		args, end, ok := extractArguments(content, j)
		if !ok {
			end = len(content)
		}
		sites = append(sites, callSite{
			name:       name,
			lowerName:  lowerName,
			arguments:  args,
			line:       lineAt(content, start),
			column:     columnAt(content, start),
			definition: isDefinition(content, start),
		})
		i = end
	}
	return sites
}

func extractArguments(content string, open int) (string, int, bool) {
	start := open + 1
	depth := 1
	for i := start; i < len(content); {
		switch {
		case content[i] == '/' && i+1 < len(content) && content[i+1] == '/':
			i = skipLineComment(content, i+2)
			continue
		case content[i] == '#':
			i = skipLineComment(content, i+1)
			continue
		case content[i] == '/' && i+1 < len(content) && content[i+1] == '*':
			i = skipBlockComment(content, i+2)
			continue
		case content[i] == '\'' || content[i] == '"' || content[i] == '`':
			i = skipQuoted(content, i, content[i])
			continue
		case content[i] == '(':
			depth++
		case content[i] == ')':
			depth--
			if depth == 0 {
				return content[start:i], i + 1, true
			}
		}
		i++
	}
	return content[start:], len(content), false
}

func skipLineComment(content string, i int) int {
	for i < len(content) && content[i] != '\n' {
		i++
	}
	return i
}

func skipBlockComment(content string, i int) int {
	for i+1 < len(content) {
		if content[i] == '*' && content[i+1] == '/' {
			return i + 2
		}
		i++
	}
	return len(content)
}

func skipQuoted(content string, i int, quote byte) int {
	i++
	for i < len(content) {
		if content[i] == '\\' {
			i += 2
			continue
		}
		if content[i] == quote {
			return i + 1
		}
		i++
	}
	return len(content)
}

func isIdentifierStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isIdentifierPart(value byte) bool {
	return isIdentifierStart(value) || value >= '0' && value <= '9'
}

func isSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f'
}

func isDefinition(content string, start int) bool {
	i := start - 1
	for i >= 0 && isSpace(content[i]) {
		i--
	}
	if i >= 0 && content[i] == '&' {
		i--
		for i >= 0 && isSpace(content[i]) {
			i--
		}
	}
	end := i + 1
	for i >= 0 && isIdentifierPart(content[i]) {
		i--
	}
	word := strings.ToLower(content[i+1 : end])
	return word == "function" || word == "fn"
}

func lineAt(content string, offset int) int {
	if offset > len(content) {
		offset = len(content)
	}
	return strings.Count(content[:offset], "\n") + 1
}

func columnAt(content string, offset int) int {
	if offset > len(content) {
		offset = len(content)
	}
	lineStart := strings.LastIndex(content[:offset], "\n") + 1
	return offset - lineStart + 1
}

func stateForScore(score int) string {
	switch {
	case score >= 70:
		return StateMalicious
	case score >= 40:
		return StateSuspicious
	default:
		return StateBenign
	}
}

func whitelistedVerdict(verdict Verdict) Verdict {
	verdict.State = StateWhitelisted
	verdict.Score = 0
	verdict.Features = nil
	verdict.Evidence = nil
	return verdict
}

func (c *WebshellClassifier) whitelistActiveLocked(hash string, now time.Time) bool {
	mark, ok := c.whitelist[hash]
	return ok && now.Before(mark.expiresAt)
}

func (c *WebshellClassifier) storeLocked(entry *cachedVerdict) {
	if _, exists := c.verdicts[entry.ref.ID]; !exists && len(c.verdicts) >= c.limits.MaxFiles {
		var oldestID string
		var oldest *cachedVerdict
		for fileID, candidate := range c.verdicts {
			if oldest == nil ||
				candidate.scannedAt.Before(oldest.scannedAt) ||
				(candidate.scannedAt.Equal(oldest.scannedAt) && candidate.sequence < oldest.sequence) {
				oldestID = fileID
				oldest = candidate
			}
		}
		if oldestID != "" {
			delete(c.verdicts, oldestID)
		}
	}
	c.sequence++
	entry.sequence = c.sequence
	c.verdicts[entry.ref.ID] = entry
}

func (c *WebshellClassifier) forgetLockedFile(fileID string) {
	c.mu.Lock()
	delete(c.verdicts, fileID)
	c.mu.Unlock()
}

func validateFile(file FileRef) error {
	if strings.TrimSpace(file.ID) == "" || strings.TrimSpace(file.Path) == "" {
		return ErrBadFile
	}
	return nil
}

func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func normalizeHash(hash string) string {
	return strings.ToLower(strings.Join(strings.Fields(hash), ""))
}

func cloneVerdict(verdict Verdict) Verdict {
	verdict.Features = cloneStrings(verdict.Features)
	verdict.Evidence = cloneStrings(verdict.Evidence)
	return verdict
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func validState(state string) bool {
	switch state {
	case StateMalicious, StateSuspicious, StateBenign, StateWhitelisted:
		return true
	default:
		return false
	}
}

func validFeatures(features []string) bool {
	previous := ""
	for _, feature := range features {
		switch feature {
		case FeatureExec, FeatureDecode, FeatureNet, FeatureFileWrite:
		default:
			return false
		}
		if previous != "" && feature <= previous {
			return false
		}
		previous = feature
	}
	return true
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
