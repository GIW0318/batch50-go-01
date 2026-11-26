// Package task judges whether an uploaded script behaves like a web shell: it
// unwraps a bounded number of encoding layers, scores the dangerous capability
// combinations left behind and keeps the verdict on a content hash so a known
// good hash can be muted for a while.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

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

// Verdict is the classification of one file.
type Verdict struct {
	FileID   string
	Hash     string
	State    string
	Score    int
	Features []string
	Cached   bool
	Layers   int
}

// Features is the explainable view of one verdict.
type Features struct {
	FileID   string
	Hash     string
	Score    int
	State    string
	Features []string
}

// Limits bounds the decoding and the verdict table.
type Limits struct {
	MaxBytes  int
	MaxLayers int
	MaxFiles  int
	CacheTTL  time.Duration
}

// WebshellClassifier owns the verdict cache and the hash whitelist.
type WebshellClassifier struct {
	limits Limits
}

// NewWebshellClassifier builds a classifier with the given caps.
func NewWebshellClassifier(limits Limits) *WebshellClassifier {
	return &WebshellClassifier{limits: limits}
}

// Scan decodes a file, scores its capabilities and records the verdict.
func (c *WebshellClassifier) Scan(file FileRef) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Decode unwraps at most maxLayers encoding layers of a file.
func (c *WebshellClassifier) Decode(file FileRef, maxLayers int) (Decoded, error) {
	return Decoded{}, errNotImplemented
}

// Explain returns the features behind the stored verdict of one file.
func (c *WebshellClassifier) Explain(fileID string) (Features, error) {
	return Features{}, errNotImplemented
}

// Whitelist mutes one content hash until the ttl runs out.
func (c *WebshellClassifier) Whitelist(hash string, ttl time.Duration) error {
	return errNotImplemented
}

// Rescan recomputes the verdict of a file that was scanned before.
func (c *WebshellClassifier) Rescan(fileID string) error {
	return errNotImplemented
}

// Prune drops expired whitelist entries and stale verdicts.
func (c *WebshellClassifier) Prune(now time.Time) (int, error) {
	return 0, errNotImplemented
}

// Hashes lists the whitelisted content hashes in ascending order.
func (c *WebshellClassifier) Hashes() []string {
	return nil
}

// VerdictFor reads the stored verdict of one file.
func (c *WebshellClassifier) VerdictFor(fileID string) (Verdict, bool) {
	return Verdict{}, false
}

// Forget drops one cached verdict and frees its slot.
func (c *WebshellClassifier) Forget(fileID string) error {
	return errNotImplemented
}

// Snapshot serialises the whitelist and the verdict cache.
func (c *WebshellClassifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the state with a snapshot, all or nothing.
func (c *WebshellClassifier) Restore(data []byte) error {
	return errNotImplemented
}
