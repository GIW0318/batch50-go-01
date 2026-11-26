// Package task screens log entries for injection attempts before they are
// written to disk or pushed to a collector.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	ErrEmptyEntry      = errors.New("entry id and origin are required")
	ErrUnknownEntry    = errors.New("unknown entry")
	ErrEntryQuarantined = errors.New("entry is quarantined")
	ErrBlankOrigin     = errors.New("origin is required")
	ErrBadPolicy       = errors.New("policy action is not supported")
	ErrBadTTL          = errors.New("suppression ttl must be positive")
	ErrAlertRate       = errors.New("alert rate limit reached for this origin")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
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

// LogInjectionDetector owns the entry registry, policies and alert counters.
type LogInjectionDetector struct {
	limits Limits
	now    func() time.Time
}

// NewLogInjectionDetector builds a detector with the given caps.
func NewLogInjectionDetector(limits Limits) *LogInjectionDetector {
	return &LogInjectionDetector{limits: limits, now: time.Now}
}

// SetClock replaces the clock used for suppression and alert windows.
func (d *LogInjectionDetector) SetClock(now func() time.Time) {
	d.now = now
}

// Inspect collects injection signals and applies the origin policy.
func (d *LogInjectionDetector) Inspect(entry Entry) (Finding, error) {
	return Finding{}, errNotImplemented
}

// Sanitize returns the escaped copy of an inspected entry together with its
// original length and digest.
func (d *LogInjectionDetector) Sanitize(entry Entry) (Entry, error) {
	return Entry{}, errNotImplemented
}

// Policy installs the handling rule of one origin and bumps its version.
func (d *LogInjectionDetector) Policy(origin string, p Policy) error {
	return errNotImplemented
}

// PolicyState reads the current rule and version of one origin.
func (d *LogInjectionDetector) PolicyState(origin string) (Policy, int, bool) {
	return Policy{}, 0, false
}

// Explain returns the trace stored for one entry.
func (d *LogInjectionDetector) Explain(entryID string) (Trace, error) {
	return Trace{}, errNotImplemented
}

// Suppress mutes alerts of one origin for the given window.
func (d *LogInjectionDetector) Suppress(origin string, ttl time.Duration) error {
	return errNotImplemented
}

// Suppression reads the end of the active suppression window of one origin.
func (d *LogInjectionDetector) Suppression(origin string) (time.Time, bool) {
	return time.Time{}, false
}

// Findings lists the held findings in ascending entry id order.
func (d *LogInjectionDetector) Findings() []Finding {
	return nil
}

// Snapshot serialises entries, traces, policies, suppression and counters.
func (d *LogInjectionDetector) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *LogInjectionDetector) Restore(data []byte) error {
	return errNotImplemented
}
