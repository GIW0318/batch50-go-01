// Package task arbitrates kernel lockdown levels, keeps the level watermark that
// callers may not walk back, and drives the controlled surfaces that have to be
// closed while a level is in force.
package task

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Level is a lockdown level. Higher numbers are stricter.
type Level int

const (
	// LevelNone keeps every controlled surface open.
	LevelNone Level = 0
	// LevelIntegrity blocks kernel self modification.
	LevelIntegrity Level = 1
	// LevelConfidentiality also blocks user space kernel memory access.
	LevelConfidentiality Level = 2
)

// Actor identifies who asks for a level change. Only signed actors may move the
// watermark.
type Actor struct {
	Name   string
	Signed bool
}

// Surface is one controlled interface that has to be closed while the active
// level reaches ClosedAt.
type Surface struct {
	Name       string
	Kind       string
	ClosedAt   Level
	Reopenable bool
	Cost       int
	Critical   bool
}

// SurfaceState is the snapshot of one surface.
type SurfaceState struct {
	Name       string
	Kind       string
	Open       bool
	ClosedAt   Level
	Reopenable bool
	Critical   bool
	CloseSeq   int64
}

// Transition is the explainable outcome of one SetLevel call.
type Transition struct {
	From    Level
	To      Level
	Applied bool
	Closed  []string
	Skipped []string
	Cost    int
	Seq     int64
}

// Config bounds the registered surfaces and the close work of one transition.
type Config struct {
	MaxSurfaces int
	CloseBudget int
}

// Errors returned by the arbiter.
var (
	ErrInvalidLevel   = errors.New("invalid lockdown level")
	ErrBadActor       = errors.New("actor name is required")
	ErrLevelMonotonic = errors.New("level watermark can only move upwards")
	ErrUnknownSurface = errors.New("unknown controlled surface")
	ErrSurfaceExists  = errors.New("controlled surface already registered")
	ErrBadSurface     = errors.New("controlled surface record is invalid")
	ErrSurfaceLimit   = errors.New("controlled surface limit reached")
	ErrSurfaceBlocked = errors.New("transition would close a critical surface")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

type surfaceRecord struct {
	Surface
	Open     bool
	CloseSeq int64
}

// LockdownArbiter owns the level, the watermark and the surface table.
type LockdownArbiter struct {
	mu        sync.RWMutex
	cfg       Config
	level     Level
	watermark Level
	seq       int64
	surfaces  map[string]*surfaceRecord
}

type snapshotSurface struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	ClosedAt   Level  `json:"closed_at"`
	Reopenable bool   `json:"reopenable"`
	Cost       int    `json:"cost"`
	Critical   bool   `json:"critical"`
	Open       bool   `json:"open"`
	CloseSeq   int64  `json:"close_seq"`
}

type snapshotData struct {
	Version        int               `json:"version"`
	Level          Level             `json:"level"`
	Watermark      json.RawMessage   `json:"watermark"`
	WatermarkAudit json.RawMessage   `json:"watermark_audit"`
	Seq            int64             `json:"seq"`
	Surfaces       []snapshotSurface `json:"surfaces"`
}

// NewLockdownArbiter builds an arbiter with the given caps.
func NewLockdownArbiter(cfg Config) *LockdownArbiter {
	return &LockdownArbiter{
		cfg:       cfg,
		level:     LevelNone,
		watermark: LevelNone,
		surfaces:  make(map[string]*surfaceRecord),
	}
}

func validLevel(level Level) bool {
	return level >= LevelNone && level <= LevelConfidentiality
}

func validActor(actor Actor) bool {
	return strings.TrimSpace(actor.Name) != ""
}

func surfaceStateFromRecord(record *surfaceRecord) SurfaceState {
	return SurfaceState{
		Name:       record.Name,
		Kind:       record.Kind,
		Open:       record.Open,
		ClosedAt:   record.ClosedAt,
		Reopenable: record.Reopenable,
		Critical:   record.Critical,
		CloseSeq:   record.CloseSeq,
	}
}

func (a *LockdownArbiter) sortedNamesLocked() []string {
	names := make([]string, 0, len(a.surfaces))
	for name := range a.surfaces {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Current reports the active lockdown level.
func (a *LockdownArbiter) Current() Level {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.level
}

// Watermark reports the level that may not be walked back.
func (a *LockdownArbiter) Watermark() Level {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.watermark
}

// RegisterSurface adds one controlled surface to the table.
func (a *LockdownArbiter) RegisterSurface(s Surface) error {
	if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Kind) == "" || s.Cost < 0 {
		return ErrBadSurface
	}
	if !validLevel(s.ClosedAt) {
		return ErrInvalidLevel
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.surfaces[s.Name]; exists {
		return ErrSurfaceExists
	}
	if len(a.surfaces) >= a.cfg.MaxSurfaces {
		return ErrSurfaceLimit
	}

	record := &surfaceRecord{Surface: s, Open: true}
	if s.ClosedAt <= a.level {
		record.Open = false
		record.CloseSeq = a.seq
		if record.CloseSeq <= 0 {
			record.CloseSeq = 1
		}
	}
	a.surfaces[s.Name] = record
	return nil
}

type closeDecision struct {
	record *surfaceRecord
}

// planRaiseLocked checks the complete candidate set before any state is changed.
// Budget decisions are also computed here so commit is a simple application of
// an already accepted plan.
func (a *LockdownArbiter) planRaiseLocked(target Level) ([]closeDecision, []string, int, error) {
	names := a.sortedNamesLocked()
	for _, name := range names {
		record := a.surfaces[name]
		if record.Open && record.ClosedAt <= target && record.Critical {
			return nil, nil, 0, ErrSurfaceBlocked
		}
	}

	decisions := make([]closeDecision, 0)
	skipped := make([]string, 0)
	cost := 0
	for _, name := range names {
		record := a.surfaces[name]
		if !record.Open || record.ClosedAt > target {
			continue
		}
		if record.Cost > a.cfg.CloseBudget-cost {
			skipped = append(skipped, name)
			continue
		}
		cost += record.Cost
		decisions = append(decisions, closeDecision{record: record})
	}
	return decisions, skipped, cost, nil
}

func (a *LockdownArbiter) applyRaiseLocked(target Level, seq int64, decisions []closeDecision) {
	for _, decision := range decisions {
		decision.record.Open = false
		decision.record.CloseSeq = seq
	}
	a.level = target
	a.seq = seq
}

func (a *LockdownArbiter) applyDowngradeLocked(target Level, seq int64) {
	for _, name := range a.sortedNamesLocked() {
		record := a.surfaces[name]
		if record.Reopenable && record.ClosedAt > target {
			record.Open = true
			record.CloseSeq = 0
		}
	}
	a.level = target
	a.seq = seq
}

// SetLevel moves the active level and closes or reopens the surfaces it owns.
func (a *LockdownArbiter) SetLevel(target Level, by Actor) (Transition, error) {
	if !validLevel(target) {
		return Transition{}, ErrInvalidLevel
	}
	if !validActor(by) {
		return Transition{}, ErrBadActor
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	from := a.level
	if target < a.watermark {
		return Transition{From: from, To: target}, ErrLevelMonotonic
	}
	if target == from {
		return Transition{From: from, To: target, Applied: false, Seq: 0, Closed: []string{}, Skipped: []string{}}, nil
	}

	seq := a.seq + 1
	if target > from {
		decisions, skipped, cost, err := a.planRaiseLocked(target)
		if err != nil {
			return Transition{From: from, To: target}, err
		}
		closed := make([]string, 0, len(decisions))
		for _, decision := range decisions {
			closed = append(closed, decision.record.Name)
		}

		// All preflight work succeeded; committing under the write lock makes
		// the level and every surface visible as one state generation.
		a.applyRaiseLocked(target, seq, decisions)
		return Transition{
			From:    from,
			To:      target,
			Applied: true,
			Closed:  closed,
			Skipped: skipped,
			Cost:    cost,
			Seq:     seq,
		}, nil
	}

	a.applyDowngradeLocked(target, seq)
	return Transition{
		From:    from,
		To:      target,
		Applied: true,
		Closed:  []string{},
		Skipped: []string{},
		Cost:    0,
		Seq:     seq,
	}, nil
}

// SetWatermark raises the watermark for a signed actor.
func (a *LockdownArbiter) SetWatermark(target Level, by Actor) error {
	if !validLevel(target) {
		return ErrInvalidLevel
	}
	if !validActor(by) || !by.Signed {
		return ErrBadActor
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if target < a.watermark {
		return ErrLevelMonotonic
	}
	if target == a.watermark {
		return nil
	}

	// A watermark which pulls the active level upwards is itself a lockdown
	// change. Plan surfaces first so a critical surface cannot leave a partial
	// close behind.
	var decisions []closeDecision
	seq := a.seq
	raisesLevel := a.level < target
	if raisesLevel {
		var err error
		decisions, _, _, err = a.planRaiseLocked(target)
		if err != nil {
			return err
		}
		seq++
	}

	a.watermark = target
	if raisesLevel {
		a.applyRaiseLocked(target, seq, decisions)
	}
	return nil
}

// Explain reports one surface and why it is open or closed.
func (a *LockdownArbiter) Explain(surface string) (SurfaceState, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	record, exists := a.surfaces[surface]
	if !exists {
		return SurfaceState{}, ErrUnknownSurface
	}
	return surfaceStateFromRecord(record), nil
}

// Surfaces lists every controlled surface, ordered by name.
func (a *LockdownArbiter) Surfaces() []SurfaceState {
	a.mu.RLock()
	defer a.mu.RUnlock()

	names := a.sortedNamesLocked()
	states := make([]SurfaceState, 0, len(names))
	for _, name := range names {
		states = append(states, surfaceStateFromRecord(a.surfaces[name]))
	}
	return states
}

// Snapshot serialises the level, the watermark and the surface table.
func (a *LockdownArbiter) Snapshot() ([]byte, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	surfaces := make([]snapshotSurface, 0, len(a.surfaces))
	for _, name := range a.sortedNamesLocked() {
		record := a.surfaces[name]
		surfaces = append(surfaces, snapshotSurface{
			Name:       record.Name,
			Kind:       record.Kind,
			ClosedAt:   record.ClosedAt,
			Reopenable: record.Reopenable,
			Cost:       record.Cost,
			Critical:   record.Critical,
			Open:       record.Open,
			CloseSeq:   record.CloseSeq,
		})
	}

	watermark, err := json.Marshal(a.watermark)
	if err != nil {
		return nil, err
	}
	payload := snapshotData{
		Version:        1,
		Level:          a.level,
		Watermark:      watermark,
		WatermarkAudit: watermark,
		Seq:            a.seq,
		Surfaces:       surfaces,
	}
	return json.Marshal(payload)
}

func rawLevelValue(raw json.RawMessage) (Level, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return LevelNone, false
	}
	var level Level
	if err := json.Unmarshal(raw, &level); err != nil || !validLevel(level) {
		return LevelNone, false
	}
	return level, true
}

func decodeSnapshot(data []byte) (snapshotData, map[string]json.RawMessage, error) {
	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return snapshotData{}, nil, ErrBadSnapshot
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return snapshotData{}, nil, ErrBadSnapshot
	}
	return payload, fields, nil
}

func restoreWatermark(payload snapshotData, fields map[string]json.RawMessage) Level {
	candidates := []json.RawMessage{payload.Watermark}
	for _, key := range []string{
		"watermark", "Watermark", "wm", "Wm",
		"watermark_primary", "WatermarkPrimary",
		"primary_watermark", "PrimaryWatermark",
		"main_watermark", "MainWatermark",
	} {
		if raw, ok := fields[key]; ok {
			candidates = append(candidates, raw)
		}
	}

	audit := []json.RawMessage{payload.WatermarkAudit}
	for _, key := range []string{
		"watermark_audit", "WatermarkAudit",
		"audit_watermark", "AuditWatermark",
		"watermarkAudit", "auditWatermark",
		"watermark_backup", "WatermarkBackup",
		"watermark_copy", "WatermarkCopy",
		"audit", "Audit", "audit_copy", "AuditCopy",
		"wm_audit", "wmAudit",
	} {
		if raw, ok := fields[key]; ok {
			audit = append(audit, raw)
		}
	}

	primary, primaryOK := LevelNone, false
	for _, raw := range candidates {
		if value, ok := rawLevelValue(raw); ok {
			if !primaryOK || value > primary {
				primary = value
			}
			primaryOK = true
		}
	}

	auditLevel, auditOK := LevelNone, false
	for _, raw := range audit {
		if value, ok := rawLevelValue(raw); ok {
			if !auditOK || value > auditLevel {
				auditLevel = value
			}
			auditOK = true
		}
	}

	switch {
	case primaryOK && auditOK:
		if auditLevel > primary {
			return auditLevel
		}
		return primary
	case primaryOK:
		return primary
	case auditOK:
		return auditLevel
	default:
		return LevelConfidentiality
	}
}

func validateSnapshot(payload snapshotData, watermark Level) (Level, int64, map[string]*surfaceRecord, error) {
	if !validLevel(payload.Level) {
		return LevelNone, 0, nil, ErrBadSnapshot
	}
	level := payload.Level
	if level < watermark {
		level = watermark
	}
	seq := payload.Seq
	if seq < 0 {
		return LevelNone, 0, nil, ErrBadSnapshot
	}

	records := make(map[string]*surfaceRecord, len(payload.Surfaces))
	for _, saved := range payload.Surfaces {
		surface := Surface{
			Name:       saved.Name,
			Kind:       saved.Kind,
			ClosedAt:   saved.ClosedAt,
			Reopenable: saved.Reopenable,
			Cost:       saved.Cost,
			Critical:   saved.Critical,
		}
		if strings.TrimSpace(surface.Name) == "" || strings.TrimSpace(surface.Kind) == "" || surface.Cost < 0 {
			return LevelNone, 0, nil, ErrBadSnapshot
		}
		if !validLevel(surface.ClosedAt) {
			return LevelNone, 0, nil, ErrBadSnapshot
		}
		if _, exists := records[surface.Name]; exists {
			return LevelNone, 0, nil, ErrBadSnapshot
		}

		open := saved.Open
		closeSeq := saved.CloseSeq
		if !open {
			if closeSeq <= 0 {
				closeSeq = seq
			}
			if closeSeq <= 0 {
				closeSeq = 1
			}
		} else {
			closeSeq = 0
		}

		records[surface.Name] = &surfaceRecord{
			Surface:  surface,
			Open:     open,
			CloseSeq: closeSeq,
		}
	}
	return level, seq, records, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *LockdownArbiter) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return ErrBadSnapshot
	}

	payload, fields, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	watermark := restoreWatermark(payload, fields)
	level, seq, records, err := validateSnapshot(payload, watermark)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.level = level
	a.watermark = watermark
	a.seq = seq
	a.surfaces = records
	return nil
}
