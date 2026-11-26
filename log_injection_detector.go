// Package task screens log entries for injection attempts before they are
// written to disk or pushed to a collector.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Screening levels of one entry.
const (
	LevelClean       = "clean"
	LevelSuspicious  = "suspicious"
	LevelQuarantined = "quarantined"
	LevelSuppressed  = "suppressed"
)

// Actions a per origin policy may request.
const (
	ActionAllow      = "allow"
	ActionRedact     = "redact"
	ActionQuarantine = "quarantine"
)

// Field types declared by the producer of an entry.
const (
	FieldTypeText   = "text"
	FieldTypeStack  = "stack"
	FieldTypeJSON   = "json"
	FieldTypeNumber = "number"
)

// Output targets an entry is heading to.
const (
	TargetFile     = "file"
	TargetTerminal = "terminal"
	TargetSiem     = "siem"
)

// Signal names collected by Inspect.
const (
	SignalNewline  = "newline_in_single_line"
	SignalANSI     = "ansi_escape"
	SignalJSON     = "json_break"
	SignalOversize = "oversize"
	SignalControl  = "control_char"
)

// Errors returned by the detector.
var (
	ErrEmptyEntry       = errors.New("entry id and origin are required")
	ErrUnknownEntry     = errors.New("unknown entry")
	ErrEntryQuarantined = errors.New("entry is quarantined")
	ErrBlankOrigin      = errors.New("origin is required")
	ErrBadPolicy        = errors.New("policy action is not supported")
	ErrBadTTL           = errors.New("suppression ttl must be positive")
	ErrAlertRate        = errors.New("alert rate limit reached for this origin")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Entry is one log record offered to the detector.
type Entry struct {
	ID          string `json:"id"`
	Origin      string `json:"origin"`
	Field       string `json:"field"`
	FieldType   string `json:"field_type"`
	Target      string `json:"target"`
	Value       string `json:"value"`
	Seq         int64  `json:"seq"`
	OriginalLen int    `json:"original_len"`
	Digest      string `json:"digest"`
}

// Finding is the screening result of one entry.
type Finding struct {
	EntryID string   `json:"entry_id"`
	Origin  string   `json:"origin"`
	Level   string   `json:"level"`
	Signals []string `json:"signals"`
	Length  int      `json:"length"`
}

// Policy is the per origin handling rule.
type Policy struct {
	Level  string `json:"level"`
	Action string `json:"action"`
}

// Trace is the explainable record of one inspection.
type Trace struct {
	EntryID       string   `json:"entry_id"`
	Origin        string   `json:"origin"`
	Action        string   `json:"action"`
	Signals       []string `json:"signals"`
	PolicyVersion int      `json:"policy_version"`
}

// Limits bounds entry size and alert volume.
type Limits struct {
	MaxEntryBytes               int
	MaxAlertsPerOriginPerMinute int
	MaxEntries                  int
}

// policyState is the versioned rule installed for one origin.
type policyState struct {
	Policy  Policy `json:"policy"`
	Version int    `json:"version"`
}

// snapshotState is the serialisable form of the whole detector state.
type snapshotState struct {
	Findings     []Finding              `json:"findings"`
	Traces       map[string]Trace       `json:"traces"`
	Entries      map[string]Entry       `json:"entries"`
	Policies     map[string]policyState `json:"policies"`
	Suppressions map[string]time.Time   `json:"suppressions"`
	Alerts       map[string][]time.Time `json:"alerts"`
}

// LogInjectionDetector owns the entry registry, policies and alert counters.
type LogInjectionDetector struct {
	limits Limits

	mu           sync.Mutex
	now          func() time.Time
	findings     []Finding
	traces       map[string]Trace
	entries      map[string]Entry
	policies     map[string]policyState
	suppressions map[string]time.Time
	alerts       map[string][]time.Time
}

// NewLogInjectionDetector builds a detector with the given caps.
func NewLogInjectionDetector(limits Limits) *LogInjectionDetector {
	return &LogInjectionDetector{
		limits:       limits,
		now:          time.Now,
		traces:       make(map[string]Trace),
		entries:      make(map[string]Entry),
		policies:     make(map[string]policyState),
		suppressions: make(map[string]time.Time),
		alerts:       make(map[string][]time.Time),
	}
}

// SetClock replaces the clock used for suppression and alert windows.
func (d *LogInjectionDetector) SetClock(now func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
}

// Inspect collects injection signals and applies the origin policy.
func (d *LogInjectionDetector) Inspect(entry Entry) (Finding, error) {
	if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.Origin) == "" {
		return Finding{}, ErrEmptyEntry
	}
	signals := collectSignals(entry, d.limits)
	level := LevelClean
	switch {
	case len(signals) == 1:
		level = LevelSuspicious
	case len(signals) >= 2:
		level = LevelQuarantined
	}
	finding := Finding{
		EntryID: entry.ID,
		Origin:  entry.Origin,
		Level:   level,
		Signals: signals,
		Length:  len(entry.Value),
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()

	state, hasPolicy := d.policies[entry.Origin]
	action := ActionRedact
	version := 1
	if hasPolicy {
		action = state.Policy.Action
		version = state.Version
	}
	trace := Trace{
		EntryID:       entry.ID,
		Origin:        entry.Origin,
		Action:        action,
		Signals:       signals,
		PolicyVersion: version,
	}

	if len(signals) > 0 {
		if until, ok := d.suppressions[entry.Origin]; ok && until.After(now) {
			finding.Level = LevelSuppressed
			d.storeLocked(entry, finding, trace)
			return finding, nil
		}
		if d.limits.MaxAlertsPerOriginPerMinute > 0 {
			window := pruneWindow(d.alerts[entry.Origin], now)
			if len(window) >= d.limits.MaxAlertsPerOriginPerMinute {
				d.alerts[entry.Origin] = window
				return Finding{}, ErrAlertRate
			}
			d.alerts[entry.Origin] = append(window, now)
		}
		if hasPolicy && action == ActionQuarantine {
			d.storeLocked(entry, finding, trace)
			return Finding{}, ErrEntryQuarantined
		}
	}
	d.storeLocked(entry, finding, trace)
	return finding, nil
}

// storeLocked records one inspected entry unless the registry is full.
func (d *LogInjectionDetector) storeLocked(entry Entry, finding Finding, trace Trace) {
	if d.limits.MaxEntries > 0 && len(d.findings) >= d.limits.MaxEntries {
		return
	}
	d.findings = append(d.findings, finding)
	d.traces[entry.ID] = trace
	d.entries[entry.ID] = entry
}

// Sanitize returns the escaped copy of an inspected entry together with its
// original length and digest.
func (d *LogInjectionDetector) Sanitize(entry Entry) (Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stored, ok := d.entries[entry.ID]
	if !ok {
		return Entry{}, ErrUnknownEntry
	}
	clean := stored
	clean.Value = escapeValue(stored.Value)
	clean.OriginalLen = len(stored.Value)
	sum := sha256.Sum256([]byte(stored.Value))
	clean.Digest = hex.EncodeToString(sum[:])
	return clean, nil
}

// Policy installs the handling rule of one origin and bumps its version.
func (d *LogInjectionDetector) Policy(origin string, p Policy) error {
	if strings.TrimSpace(origin) == "" {
		return ErrBlankOrigin
	}
	switch p.Action {
	case ActionAllow, ActionRedact, ActionQuarantine:
	default:
		return ErrBadPolicy
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.policies[origin]
	state.Policy = p
	state.Version++
	d.policies[origin] = state
	return nil
}

// PolicyState reads the current rule and version of one origin.
func (d *LogInjectionDetector) PolicyState(origin string) (Policy, int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.policies[origin]
	if !ok {
		return Policy{}, 0, false
	}
	return state.Policy, state.Version, true
}

// Explain returns the trace stored for one entry.
func (d *LogInjectionDetector) Explain(entryID string) (Trace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	trace, ok := d.traces[entryID]
	if !ok {
		return Trace{}, ErrUnknownEntry
	}
	return trace, nil
}

// Suppress mutes alerts of one origin for the given window.
func (d *LogInjectionDetector) Suppress(origin string, ttl time.Duration) error {
	if strings.TrimSpace(origin) == "" {
		return ErrBlankOrigin
	}
	if ttl <= 0 {
		return ErrBadTTL
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.suppressions[origin] = d.now().Add(ttl)
	return nil
}

// Suppression reads the end of the active suppression window of one origin.
func (d *LogInjectionDetector) Suppression(origin string) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.suppressions[origin]
	if !ok || !until.After(d.now()) {
		return time.Time{}, false
	}
	return until, true
}

// Findings lists the held findings in ascending entry id order.
func (d *LogInjectionDetector) Findings() []Finding {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Finding, len(d.findings))
	copy(out, d.findings)
	sort.SliceStable(out, func(i, j int) bool { return out[i].EntryID < out[j].EntryID })
	return out
}

// Snapshot serialises entries, traces, policies, suppression and counters.
func (d *LogInjectionDetector) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := snapshotState{
		Findings:     append([]Finding(nil), d.findings...),
		Traces:       d.traces,
		Entries:      d.entries,
		Policies:     d.policies,
		Suppressions: d.suppressions,
		Alerts:       d.alerts,
	}
	return json.Marshal(state)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *LogInjectionDetector) Restore(data []byte) error {
	var state snapshotState
	if err := json.Unmarshal(data, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.Traces == nil || state.Entries == nil || state.Policies == nil ||
		state.Suppressions == nil || state.Alerts == nil {
		return ErrBadSnapshot
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.findings = state.Findings
	d.traces = state.Traces
	d.entries = state.Entries
	d.policies = state.Policies
	d.suppressions = state.Suppressions
	d.alerts = state.Alerts
	return nil
}

// collectSignals gathers the injection signals of one entry in ascending
// signal name order.
func collectSignals(entry Entry, limits Limits) []string {
	set := make(map[string]struct{})
	value := entry.Value
	if limits.MaxEntryBytes > 0 && len(value) > limits.MaxEntryBytes {
		set[SignalOversize] = struct{}{}
	}
	if entry.FieldType != FieldTypeStack && strings.ContainsAny(value, "\n\r") {
		set[SignalNewline] = struct{}{}
	}
	if hasEscapeSequence(value) {
		set[SignalANSI] = struct{}{}
	}
	if entry.FieldType == FieldTypeJSON &&
		(strings.Contains(value, `","`) || strings.Contains(value, `}{`)) {
		set[SignalJSON] = struct{}{}
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == 0x1b {
			continue
		}
		if (b < 0x20 && b != '\n' && b != '\r' && b != '\t') || b == 0x7f {
			set[SignalControl] = struct{}{}
			break
		}
	}
	signals := make([]string, 0, len(set))
	for name := range set {
		signals = append(signals, name)
	}
	sort.Strings(signals)
	return signals
}

// hasEscapeSequence reports whether the value carries a terminal escape,
// either as a raw ESC byte or as escaped text such as [.
func hasEscapeSequence(value string) bool {
	if strings.ContainsRune(value, 0x1b) {
		return true
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, `\u001b`) ||
		strings.Contains(lower, `\x1b`) ||
		strings.Contains(lower, `\033`)
}

// escapeValue renders newlines, carriage returns, escape sequences and other
// control characters as printable text. The mapping is deterministic so the
// same original value always yields the same sanitised value.
func escapeValue(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == 0x1b:
			b.WriteString(`\u001b`)
		case c < 0x20 || c == 0x7f:
			const hexdigits = "0123456789abcdef"
			b.WriteString(`\x`)
			b.WriteByte(hexdigits[c>>4])
			b.WriteByte(hexdigits[c&0x0f])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// pruneWindow drops alert timestamps at or before the one minute cutoff.
func pruneWindow(timestamps []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-time.Minute)
	kept := timestamps[:0]
	for _, ts := range timestamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	return kept
}
