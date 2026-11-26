// Package task keeps the tamper evident audit log of a control plane: every
// record is chained to its predecessor, anchors pin the head, and a broken
// link raises one alarm and freezes further appends.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"sync"
)

// AlarmKindBrokenChain is raised when Verify finds the first unlinked record.
const AlarmKindBrokenChain = "broken_chain"

const (
	zeroDigest    = "0000000000000000000000000000000000000000000000000000000000000000"
	redactAction  = "redact"
	redactActor   = "system"
	redactPrefix  = "redacted:"
	nullJSONToken = "null"
)

// Errors returned by the guard.
var (
	ErrBlankActor      = errors.New("audit actor is required")
	ErrBlankAction     = errors.New("audit action is required")
	ErrPayloadTooLarge = errors.New("audit payload exceeds the configured limit")
	ErrChainFull       = errors.New("audit chain reached its entry limit")
	ErrNoEntries       = errors.New("audit chain has no entries to anchor")
	ErrAnchorCurrent   = errors.New("audit chain head is already anchored")
	ErrChainBroken     = errors.New("audit chain link is broken")
	ErrAnchorMismatch  = errors.New("audit chain head does not match the latest anchor")
	ErrBadRedaction    = errors.New("redaction request is invalid")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// ChainConfig bounds one audit chain.
type ChainConfig struct {
	MaxEntries  int   `json:"max_entries"`
	MaxPayload  int   `json:"max_payload"`
	AnchorEvery int64 `json:"anchor_every"`
}

// Entry is one linked audit record.
type Entry struct {
	Seq      int64  `json:"seq"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Payload  string `json:"payload"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// Anchor pins the chain head at one point in time and cross signs the previous
// anchor so rotated segments stay verifiable.
type Anchor struct {
	Index    int    `json:"index"`
	Seq      int64  `json:"seq"`
	Head     string `json:"head"`
	PrevHead string `json:"prev_head"`
	CrossSig string `json:"cross_sig"`
}

// Alarm is one integrity finding kept for the on call engineer.
type Alarm struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// AuditChainGuard owns the entries, the anchors and the alarm feed.
type AuditChainGuard struct {
	cfg     ChainConfig
	mu      sync.RWMutex
	entries []Entry
	anchors []Anchor
	alarms  []Alarm
	broken  bool
}

// NewAuditChainGuard builds an empty chain with the given bounds.
func NewAuditChainGuard(cfg ChainConfig) *AuditChainGuard {
	return &AuditChainGuard{cfg: cfg}
}

// Append adds one record to the tail of the chain.
func (g *AuditChainGuard) Append(actor, action string, payload []byte) (Entry, error) {
	if strings.TrimSpace(actor) == "" {
		return Entry{}, ErrBlankActor
	}
	if strings.TrimSpace(action) == "" {
		return Entry{}, ErrBlankAction
	}
	if g.cfg.MaxPayload >= 0 && len(payload) > g.cfg.MaxPayload {
		return Entry{}, ErrPayloadTooLarge
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.broken {
		return Entry{}, ErrChainBroken
	}
	if g.cfg.MaxEntries >= 0 && len(g.entries) >= g.cfg.MaxEntries {
		return Entry{}, ErrChainFull
	}

	entry := Entry{
		Seq:      int64(len(g.entries)) + 1,
		Actor:    actor,
		Action:   action,
		Payload:  string(payload),
		PrevHash: zeroDigest,
	}
	if n := len(g.entries); n > 0 {
		entry.PrevHash = g.entries[n-1].Hash
	}
	entry.Hash = hashEntry(entry)

	g.entries = append(g.entries, entry)

	if g.cfg.AnchorEvery > 0 && entry.Seq%g.cfg.AnchorEvery == 0 {
		if _, err := g.rotateLocked(); err != nil {
			return entry, fmt.Errorf("auto anchor: %w", err)
		}
	}

	return entry, nil
}

// Entries returns a copy of the chain, oldest first.
func (g *AuditChainGuard) Entries() []Entry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]Entry(nil), g.entries...)
}

// Head returns the newest entry.
func (g *AuditChainGuard) Head() (Entry, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.entries) == 0 {
		return Entry{}, false
	}
	return g.entries[len(g.entries)-1], true
}

// Len is the number of chained entries.
func (g *AuditChainGuard) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.entries)
}

// Verify recomputes every digest and reports the first broken link.
func (g *AuditChainGuard) Verify() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for i := range g.entries {
		entry := g.entries[i]
		expectedPrev := zeroDigest
		if i > 0 {
			expectedPrev = g.entries[i-1].Hash
		}

		broken := entry.PrevHash != expectedPrev || hashEntry(entry) != entry.Hash
		if !broken {
			continue
		}

		g.broken = true
		if !g.alarmExistsLocked(entry.Seq) {
			g.alarms = append(g.alarms, Alarm{
				Seq:    entry.Seq,
				Kind:   AlarmKindBrokenChain,
				Detail: fmt.Sprintf("audit chain digest mismatch at sequence %d", entry.Seq),
			})
		}
		return fmt.Errorf("%w at sequence %d", ErrChainBroken, entry.Seq)
	}

	g.broken = false
	return nil
}

// Rotate pins the current head as a new anchor.
func (g *AuditChainGuard) Rotate() (Anchor, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rotateLocked()
}

// Anchors returns a copy of every anchor, oldest first.
func (g *AuditChainGuard) Anchors() []Anchor {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]Anchor(nil), g.anchors...)
}

// LatestAnchor returns the newest anchor.
func (g *AuditChainGuard) LatestAnchor() (Anchor, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.anchors) == 0 {
		return Anchor{}, false
	}
	return g.anchors[len(g.anchors)-1], true
}

// Alarms returns a copy of the integrity alarm feed.
func (g *AuditChainGuard) Alarms() []Alarm {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]Alarm(nil), g.alarms...)
}

// ClearAlarms empties the feed and reports how many alarms were dropped.
func (g *AuditChainGuard) ClearAlarms() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := len(g.alarms)
	g.alarms = nil
	return count
}

// Redact appends a tombstone record for one earlier entry.
func (g *AuditChainGuard) Redact(seq int64, reason string) (Entry, error) {
	if strings.TrimSpace(reason) == "" {
		return Entry{}, ErrBadRedaction
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if seq < 1 || seq > int64(len(g.entries)) {
		return Entry{}, ErrBadRedaction
	}
	if g.isRedactedLocked(seq) {
		return Entry{}, ErrBadRedaction
	}
	if g.broken {
		return Entry{}, ErrChainBroken
	}
	if g.cfg.MaxEntries >= 0 && len(g.entries) >= g.cfg.MaxEntries {
		return Entry{}, ErrChainFull
	}

	payload := []byte(fmt.Sprintf("%s%d:%s", redactPrefix, seq, reason))
	if g.cfg.MaxPayload >= 0 && len(payload) > g.cfg.MaxPayload {
		return Entry{}, ErrPayloadTooLarge
	}

	entry := Entry{
		Seq:      int64(len(g.entries)) + 1,
		Actor:    redactActor,
		Action:   redactAction,
		Payload:  string(payload),
		PrevHash: zeroDigest,
	}
	if n := len(g.entries); n > 0 {
		entry.PrevHash = g.entries[n-1].Hash
	}
	entry.Hash = hashEntry(entry)
	g.entries = append(g.entries, entry)

	if g.cfg.AnchorEvery > 0 && entry.Seq%g.cfg.AnchorEvery == 0 {
		if _, err := g.rotateLocked(); err != nil {
			return entry, fmt.Errorf("auto anchor: %w", err)
		}
	}

	return entry, nil
}

// Snapshot serialises entries, anchors and alarms.
func (g *AuditChainGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	doc := snapshotDocument{
		Entries: append([]Entry(nil), g.entries...),
		Anchors: append([]Anchor(nil), g.anchors...),
		Alarms:  append([]Alarm(nil), g.alarms...),
	}
	if doc.Entries == nil {
		doc.Entries = []Entry{}
	}
	if doc.Anchors == nil {
		doc.Anchors = []Anchor{}
	}
	if doc.Alarms == nil {
		doc.Alarms = []Alarm{}
	}
	return json.Marshal(doc)
}

// Restore replaces the chain, all or nothing.
func (g *AuditChainGuard) Restore(data []byte) error {
	next, broken, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries = next.Entries
	g.anchors = next.Anchors
	g.alarms = next.Alarms
	g.broken = broken
	return nil
}

type snapshotDocument struct {
	Entries []Entry  `json:"entries"`
	Anchors []Anchor `json:"anchors"`
	Alarms  []Alarm  `json:"alarms"`
}

func (g *AuditChainGuard) rotateLocked() (Anchor, error) {
	if len(g.entries) == 0 {
		return Anchor{}, ErrNoEntries
	}

	head := g.entries[len(g.entries)-1]
	if len(g.anchors) > 0 {
		latest := g.anchors[len(g.anchors)-1]
		if latest.Seq == head.Seq && latest.Head == head.Hash {
			return Anchor{}, ErrAnchorCurrent
		}
	}

	anchor := Anchor{
		Index:    len(g.anchors) + 1,
		Seq:      head.Seq,
		Head:     head.Hash,
		PrevHead: zeroDigest,
	}
	previousCrossSig := zeroDigest
	if len(g.anchors) > 0 {
		previous := g.anchors[len(g.anchors)-1]
		anchor.PrevHead = previous.Head
		previousCrossSig = previous.CrossSig
	}
	anchor.CrossSig = hashAnchor(anchor.Index, anchor.Seq, anchor.Head, anchor.PrevHead, previousCrossSig)
	g.anchors = append(g.anchors, anchor)
	return anchor, nil
}

func (g *AuditChainGuard) alarmExistsLocked(seq int64) bool {
	for _, alarm := range g.alarms {
		if alarm.Kind == AlarmKindBrokenChain && alarm.Seq == seq {
			return true
		}
	}
	return false
}

func (g *AuditChainGuard) isRedactedLocked(seq int64) bool {
	for _, entry := range g.entries {
		if entry.Action != redactAction {
			continue
		}
		if redactedSeq, ok := parseRedactionSeq(entry.Payload); ok && redactedSeq == seq {
			return true
		}
	}
	return false
}

func hashEntry(entry Entry) string {
	h := sha256.New()
	writeInt64Digest(h, entry.Seq)
	writeStringDigest(h, entry.Actor)
	writeStringDigest(h, entry.Action)
	writeStringDigest(h, entry.Payload)
	writeStringDigest(h, entry.PrevHash)
	return hex.EncodeToString(h.Sum(nil))
}

func hashAnchor(index int, seq int64, head, prevHead, previousCrossSig string) string {
	h := sha256.New()
	writeInt64Digest(h, int64(index))
	writeInt64Digest(h, seq)
	writeStringDigest(h, head)
	writeStringDigest(h, prevHead)
	writeStringDigest(h, previousCrossSig)
	return hex.EncodeToString(h.Sum(nil))
}

func writeInt64Digest(h hash.Hash, value int64) {
	var width [8]byte
	binary.BigEndian.PutUint64(width[:], uint64(value))
	h.Write(width[:])
}

func writeStringDigest(h hash.Hash, value string) {
	var width [8]byte
	binary.BigEndian.PutUint64(width[:], uint64(len(value)))
	h.Write(width[:])
	h.Write([]byte(value))
}

func parseRedactionSeq(payload string) (int64, bool) {
	rest, ok := strings.CutPrefix(payload, redactPrefix)
	if !ok {
		return 0, false
	}
	seqText, _, ok := strings.Cut(rest, ":")
	if !ok || seqText == "" {
		return 0, false
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

func decodeSnapshot(data []byte) (snapshotDocument, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return snapshotDocument{}, false, fmt.Errorf("%w: invalid json", ErrBadSnapshot)
	}

	for _, key := range []string{"entries", "anchors", "alarms"} {
		raw, ok := root[key]
		if !ok || isNullJSON(raw) {
			return snapshotDocument{}, false, fmt.Errorf("%w: missing %s", ErrBadSnapshot, key)
		}
	}

	var rawEntries []json.RawMessage
	if err := json.Unmarshal(root["entries"], &rawEntries); err != nil {
		return snapshotDocument{}, false, fmt.Errorf("%w: entries", ErrBadSnapshot)
	}
	var rawAnchors []json.RawMessage
	if err := json.Unmarshal(root["anchors"], &rawAnchors); err != nil {
		return snapshotDocument{}, false, fmt.Errorf("%w: anchors", ErrBadSnapshot)
	}
	var rawAlarms []json.RawMessage
	if err := json.Unmarshal(root["alarms"], &rawAlarms); err != nil {
		return snapshotDocument{}, false, fmt.Errorf("%w: alarms", ErrBadSnapshot)
	}

	entries := make([]Entry, 0, len(rawEntries))
	requiredEntryFields := []string{"seq", "actor", "action", "payload", "prev_hash", "hash"}
	for i, raw := range rawEntries {
		if err := requireJSONFields(raw, requiredEntryFields); err != nil {
			return snapshotDocument{}, false, fmt.Errorf("%w: entry %d", ErrBadSnapshot, i+1)
		}
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return snapshotDocument{}, false, fmt.Errorf("%w: entry %d", ErrBadSnapshot, i+1)
		}
		entries = append(entries, entry)
	}

	anchors := make([]Anchor, 0, len(rawAnchors))
	requiredAnchorFields := []string{"index", "seq", "head", "prev_head", "cross_sig"}
	for i, raw := range rawAnchors {
		if err := requireJSONFields(raw, requiredAnchorFields); err != nil {
			return snapshotDocument{}, false, fmt.Errorf("%w: anchor %d", ErrBadSnapshot, i+1)
		}
		var anchor Anchor
		if err := json.Unmarshal(raw, &anchor); err != nil {
			return snapshotDocument{}, false, fmt.Errorf("%w: anchor %d", ErrBadSnapshot, i+1)
		}
		anchors = append(anchors, anchor)
	}

	alarms := make([]Alarm, 0, len(rawAlarms))
	requiredAlarmFields := []string{"seq", "kind", "detail"}
	for i, raw := range rawAlarms {
		if err := requireJSONFields(raw, requiredAlarmFields); err != nil {
			return snapshotDocument{}, false, fmt.Errorf("%w: alarm %d", ErrBadSnapshot, i+1)
		}
		var alarm Alarm
		if err := json.Unmarshal(raw, &alarm); err != nil {
			return snapshotDocument{}, false, fmt.Errorf("%w: alarm %d", ErrBadSnapshot, i+1)
		}
		alarms = append(alarms, alarm)
	}

	for i, entry := range entries {
		if entry.Seq != int64(i+1) {
			return snapshotDocument{}, false, fmt.Errorf("%w: sequence at %d", ErrBadSnapshot, i+1)
		}
		expectedPrev := zeroDigest
		if i > 0 {
			expectedPrev = entries[i-1].Hash
		}
		if entry.PrevHash != expectedPrev {
			return snapshotDocument{}, false, fmt.Errorf("%w: prev hash at %d", ErrBadSnapshot, entry.Seq)
		}
	}

	var previousAnchorSeq int64
	for i, anchor := range anchors {
		if anchor.Index != i+1 {
			return snapshotDocument{}, false, fmt.Errorf("%w: anchor index %d", ErrBadSnapshot, i+1)
		}
		if i > 0 && anchor.Seq <= previousAnchorSeq {
			return snapshotDocument{}, false, fmt.Errorf("%w: anchor sequence %d", ErrBadSnapshot, anchor.Index)
		}
		if anchor.Seq < 1 || anchor.Seq > int64(len(entries)) {
			return snapshotDocument{}, false, fmt.Errorf("%w: anchor %d", ErrAnchorMismatch, anchor.Index)
		}
		if entries[anchor.Seq-1].Hash != anchor.Head {
			return snapshotDocument{}, false, fmt.Errorf("%w: anchor %d", ErrAnchorMismatch, anchor.Index)
		}
		previousAnchorSeq = anchor.Seq
	}

	return snapshotDocument{Entries: entries, Anchors: anchors, Alarms: alarms}, hasBrokenChainAlarm(alarms), nil
}

func requireJSONFields(raw json.RawMessage, fields []string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for _, field := range fields {
		value, ok := object[field]
		if !ok || isNullJSON(value) {
			return fmt.Errorf("missing field %s", field)
		}
	}
	return nil
}

func isNullJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte(nullJSONToken))
}

func hasBrokenChainAlarm(alarms []Alarm) bool {
	for _, alarm := range alarms {
		if alarm.Kind == AlarmKindBrokenChain {
			return true
		}
	}
	return false
}
