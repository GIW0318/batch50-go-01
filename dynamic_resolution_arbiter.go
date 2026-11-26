// Package task keeps one render scale tier per viewport and drives render
// target rebuilds from frame time feedback.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
)

// ResolutionLadder is the fixed set of render scales a viewport may pick.
var ResolutionLadder = []float64{0.50, 0.60, 0.70, 0.80, 0.90, 1.00}

var ladder = [...]float64{0.50, 0.60, 0.70, 0.80, 0.90, 1.00}

// Defaults applied when the matching ArbiterConfig field is left unset.
const (
	DefaultTargetMs       = 16.0
	DefaultUpStreak       = 8
	DefaultDownStreak     = 3
	DefaultCooldownFrames = 0
)

// Errors reported by the arbiter.
var (
	ErrBadViewport       = errors.New("viewport is invalid")
	ErrDuplicateViewport = errors.New("viewport already registered")
	ErrUnknownViewport   = errors.New("viewport not found")
	ErrBadRange          = errors.New("scale range is invalid")
	ErrBadSample         = errors.New("frame sample is invalid")
	ErrBadReason         = errors.New("freeze reason is required")
	ErrNotFrozen         = errors.New("viewport is not frozen")
	ErrNoRebuild         = errors.New("no pending rebuild")
	ErrRebuildBlocked    = errors.New("rebuild blocked by in flight frames")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// ArbiterConfig tunes the tier ladder policy.
type ArbiterConfig struct {
	TargetMs       float64
	UpStreak       int
	DownStreak     int
	CooldownFrames int
}

// FrameSample is one frame time report of a viewport.
type FrameSample struct {
	FrameTimeMs float64
	InFlight    int
}

// RebuildRequest is the render target rebuild queued by a tier decision.
type RebuildRequest struct {
	Viewport string
	From     float64
	To       float64
	Sample   uint64
}

// ViewportStats is the counter block of one viewport.
type ViewportStats struct {
	Reports    int
	UpShifts   int
	DownShifts int
	Rebuilds   int
	Frozen     bool
	StreakUp   int
	StreakDown int
}

type viewportState struct {
	scale          float64
	min            float64
	max            float64
	frozen         bool
	freezeReason   string
	pending        *RebuildRequest
	stats          ViewportStats
	sampleSeq      uint64
	inFlight       int
	hasUpShift     bool
	lastUpShift    uint64
	hasDownShift   bool
	lastDownShift  uint64
	skipDownSample bool
}

// DynamicResolutionArbiter owns the per viewport tiers, the streak counters
// and the pending rebuild requests.
type DynamicResolutionArbiter struct {
	cfg ArbiterConfig

	mu        sync.Mutex
	viewports map[string]*viewportState
}

// NewDynamicResolutionArbiter builds an arbiter with the given policy.
func NewDynamicResolutionArbiter(cfg ArbiterConfig) *DynamicResolutionArbiter {
	return &DynamicResolutionArbiter{
		cfg:       normalizeConfig(cfg),
		viewports: make(map[string]*viewportState),
	}
}

func normalizeConfig(cfg ArbiterConfig) ArbiterConfig {
	if math.IsNaN(cfg.TargetMs) || math.IsInf(cfg.TargetMs, 0) || cfg.TargetMs <= 0 {
		cfg.TargetMs = DefaultTargetMs
	}
	if cfg.UpStreak <= 0 {
		cfg.UpStreak = DefaultUpStreak
	}
	if cfg.DownStreak <= 0 {
		cfg.DownStreak = DefaultDownStreak
	}
	if cfg.CooldownFrames < 0 {
		cfg.CooldownFrames = DefaultCooldownFrames
	}
	return cfg
}

func validViewport(viewport string) bool {
	return strings.TrimSpace(viewport) != ""
}

func ladderIndex(scale float64) int {
	if math.IsNaN(scale) || math.IsInf(scale, 0) {
		return -1
	}
	for i, step := range ladder {
		if scale == step {
			return i
		}
	}
	return -1
}

// AddViewport registers a viewport at an initial ladder step.
func (a *DynamicResolutionArbiter) AddViewport(viewport string, initial float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !validViewport(viewport) || ladderIndex(initial) < 0 {
		return ErrBadViewport
	}
	if _, ok := a.viewports[viewport]; ok {
		return ErrDuplicateViewport
	}

	a.viewports[viewport] = &viewportState{
		scale: initial,
		min:   ladder[0],
		max:   ladder[len(ladder)-1],
	}
	return nil
}

// RemoveViewport drops a viewport and its counters.
func (a *DynamicResolutionArbiter) RemoveViewport(viewport string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.viewports[viewport]; !ok {
		return ErrUnknownViewport
	}
	delete(a.viewports, viewport)
	return nil
}

// Viewports lists the registered viewports in ascending order.
func (a *DynamicResolutionArbiter) Viewports() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]string, 0, len(a.viewports))
	for viewport := range a.viewports {
		out = append(out, viewport)
	}
	sort.Strings(out)
	return out
}

// Scale returns the active render scale of a viewport.
func (a *DynamicResolutionArbiter) Scale(viewport string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	if v, ok := a.viewports[viewport]; ok {
		return v.scale
	}
	return 0
}

// Ladder returns the ladder steps allowed by the current range.
func (a *DynamicResolutionArbiter) Ladder(viewport string) []float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.viewports[viewport]
	if !ok {
		return []float64{}
	}
	return allowedLadder(v.min, v.max)
}

func allowedLadder(min, max float64) []float64 {
	out := make([]float64, 0, len(ladder))
	for _, step := range ladder {
		if step >= min && step <= max {
			out = append(out, step)
		}
	}
	return out
}

// OverlayScale returns the scale used by UI and subtitle layers, which never
// follow the render tier.
func (a *DynamicResolutionArbiter) OverlayScale(viewport string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.viewports[viewport]; ok {
		return 1.0
	}
	return 0
}

// SetRange narrows the allowed ladder steps of a viewport.
func (a *DynamicResolutionArbiter) SetRange(viewport string, min, max float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.viewports[viewport]
	if !ok {
		return ErrUnknownViewport
	}
	if ladderIndex(min) < 0 || ladderIndex(max) < 0 || min >= max {
		return ErrBadRange
	}

	v.min = min
	v.max = max
	v.pending = nil
	if v.scale < min {
		v.scale = min
	} else if v.scale > max {
		v.scale = max
	}
	return nil
}

// Report feeds one frame time sample into the tier decision.
func (a *DynamicResolutionArbiter) Report(viewport string, sample FrameSample) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.viewports[viewport]
	if !ok {
		return ErrUnknownViewport
	}
	if math.IsNaN(sample.FrameTimeMs) || math.IsInf(sample.FrameTimeMs, 0) || sample.FrameTimeMs <= 0 || sample.InFlight < 0 {
		return ErrBadSample
	}

	v.stats.Reports++
	v.sampleSeq++
	v.inFlight = sample.InFlight

	if v.frozen {
		return nil
	}

	upThreshold := a.cfg.TargetMs * 0.9
	downThreshold := a.cfg.TargetMs * 1.15
	skippingDown := v.skipDownSample
	if skippingDown {
		v.skipDownSample = false
	}

	switch {
	case sample.FrameTimeMs <= upThreshold:
		v.stats.StreakUp++
		v.stats.StreakDown = 0
	case sample.FrameTimeMs >= downThreshold:
		v.stats.StreakUp = 0
		if skippingDown {
			v.stats.StreakDown = 0
		} else {
			v.stats.StreakDown++
		}
	default:
		v.stats.StreakUp = 0
		v.stats.StreakDown = 0
	}

	if v.pending != nil {
		return nil
	}

	if sample.FrameTimeMs <= upThreshold && v.stats.StreakUp >= a.cfg.UpStreak && a.canCooldown(v, true) {
		if next, ok := adjacentStep(v.scale, v.min, v.max, 1); ok {
			v.pending = &RebuildRequest{
				Viewport: viewport,
				From:     v.scale,
				To:       next,
				Sample:   v.sampleSeq,
			}
		}
		return nil
	}

	if !skippingDown && sample.FrameTimeMs >= downThreshold && v.stats.StreakDown >= a.cfg.DownStreak && a.canCooldown(v, false) {
		if next, ok := adjacentStep(v.scale, v.min, v.max, -1); ok {
			v.pending = &RebuildRequest{
				Viewport: viewport,
				From:     v.scale,
				To:       next,
				Sample:   v.sampleSeq,
			}
		}
	}
	return nil
}

func (a *DynamicResolutionArbiter) canCooldown(v *viewportState, up bool) bool {
	var has bool
	var last uint64
	if up {
		has, last = v.hasUpShift, v.lastUpShift
	} else {
		has, last = v.hasDownShift, v.lastDownShift
	}
	if !has {
		return true
	}
	return v.sampleSeq >= last && v.sampleSeq-last >= uint64(a.cfg.CooldownFrames)
}

func adjacentStep(scale, min, max float64, direction int) (float64, bool) {
	index := ladderIndex(scale)
	if index < 0 {
		return 0, false
	}
	for i := index + direction; i >= 0 && i < len(ladder); i += direction {
		step := ladder[i]
		if step >= min && step <= max {
			return step, true
		}
	}
	return 0, false
}

// Freeze pins the current tier and records the operator reason.
func (a *DynamicResolutionArbiter) Freeze(viewport, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.viewports[viewport]
	if !ok {
		return ErrUnknownViewport
	}
	if strings.TrimSpace(reason) == "" {
		return ErrBadReason
	}

	v.frozen = true
	v.freezeReason = reason
	v.stats.Frozen = true
	v.pending = nil
	v.stats.StreakUp = 0
	v.stats.StreakDown = 0
	v.skipDownSample = false
	return nil
}

// Resume releases a frozen viewport.
func (a *DynamicResolutionArbiter) Resume(viewport string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.viewports[viewport]
	if !ok {
		return ErrUnknownViewport
	}
	if !v.frozen {
		return ErrNotFrozen
	}

	v.frozen = false
	v.freezeReason = ""
	v.stats.Frozen = false
	v.stats.StreakUp = 0
	v.stats.StreakDown = 0
	v.skipDownSample = false
	v.pending = nil
	return nil
}

// Frozen reports the freeze state and its reason.
func (a *DynamicResolutionArbiter) Frozen(viewport string) (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if v, ok := a.viewports[viewport]; ok {
		return v.frozen, v.freezeReason
	}
	return false, ""
}

// PendingRebuild reads the queued rebuild request of a viewport.
func (a *DynamicResolutionArbiter) PendingRebuild(viewport string) (RebuildRequest, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.viewports[viewport]
	if !ok || v.pending == nil {
		return RebuildRequest{}, false
	}
	return *v.pending, true
}

// CommitRebuild applies or drops the queued rebuild request.
func (a *DynamicResolutionArbiter) CommitRebuild(viewport string, ok bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	v, exists := a.viewports[viewport]
	if !exists {
		return ErrUnknownViewport
	}
	if v.pending == nil {
		return ErrNoRebuild
	}
	if v.inFlight > 0 {
		return ErrRebuildBlocked
	}

	request := *v.pending
	if !ok {
		v.pending = nil
		return nil
	}

	v.scale = request.To
	v.pending = nil
	v.stats.Rebuilds++
	v.stats.StreakUp = 0
	v.stats.StreakDown = 0

	if request.To > request.From {
		v.stats.UpShifts++
		v.hasUpShift = true
		v.lastUpShift = v.sampleSeq
		v.skipDownSample = true
	} else {
		v.stats.DownShifts++
		v.hasDownShift = true
		v.lastDownShift = v.sampleSeq
		v.skipDownSample = false
	}
	return nil
}

// Stats returns the counter block of one viewport.
func (a *DynamicResolutionArbiter) Stats(viewport string) ViewportStats {
	a.mu.Lock()
	defer a.mu.Unlock()

	if v, ok := a.viewports[viewport]; ok {
		return v.stats
	}
	return ViewportStats{}
}

const snapshotVersion = 1

type snapshotPayload struct {
	Version   int                `json:"version"`
	Viewports []snapshotViewport `json:"viewports"`
}

type snapshotViewport struct {
	Viewport       string          `json:"viewport"`
	Scale          float64         `json:"scale"`
	Min            float64         `json:"min"`
	Max            float64         `json:"max"`
	Frozen         bool            `json:"frozen"`
	FreezeReason   string          `json:"freeze_reason,omitempty"`
	Stats          ViewportStats   `json:"stats"`
	Pending        *RebuildRequest `json:"pending,omitempty"`
	SampleSeq      uint64          `json:"sample_seq"`
	InFlight       int             `json:"in_flight"`
	HasUpShift     bool            `json:"has_up_shift"`
	LastUpShift    uint64          `json:"last_up_shift,omitempty"`
	HasDownShift   bool            `json:"has_down_shift"`
	LastDownShift  uint64          `json:"last_down_shift,omitempty"`
	SkipDownSample bool            `json:"skip_down_sample,omitempty"`
}

// Snapshot serialises every viewport tier, range, streak and pending rebuild.
func (a *DynamicResolutionArbiter) Snapshot() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	names := make([]string, 0, len(a.viewports))
	for viewport := range a.viewports {
		names = append(names, viewport)
	}
	sort.Strings(names)

	payload := snapshotPayload{
		Version:   snapshotVersion,
		Viewports: make([]snapshotViewport, 0, len(names)),
	}
	for _, viewport := range names {
		v := a.viewports[viewport]
		item := snapshotViewport{
			Viewport:       viewport,
			Scale:          v.scale,
			Min:            v.min,
			Max:            v.max,
			Frozen:         v.frozen,
			FreezeReason:   v.freezeReason,
			Stats:          v.stats,
			SampleSeq:      v.sampleSeq,
			InFlight:       v.inFlight,
			HasUpShift:     v.hasUpShift,
			LastUpShift:    v.lastUpShift,
			HasDownShift:   v.hasDownShift,
			LastDownShift:  v.lastDownShift,
			SkipDownSample: v.skipDownSample,
		}
		if v.pending != nil {
			request := *v.pending
			item.Pending = &request
		}
		payload.Viewports = append(payload.Viewports, item)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrBadSnapshot
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot payload.
func (a *DynamicResolutionArbiter) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return ErrBadSnapshot
	}
	rawViewports, ok := fields["viewports"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawViewports), []byte("null")) {
		return ErrBadSnapshot
	}

	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	restored := make(map[string]*viewportState, len(payload.Viewports))
	for _, item := range payload.Viewports {
		v, err := validateSnapshotViewport(item)
		if err != nil {
			return ErrBadSnapshot
		}
		if _, exists := restored[item.Viewport]; exists {
			return ErrBadSnapshot
		}
		restored[item.Viewport] = v
	}

	a.mu.Lock()
	a.viewports = restored
	a.mu.Unlock()
	return nil
}

func validateSnapshotViewport(item snapshotViewport) (*viewportState, error) {
	if !validViewport(item.Viewport) {
		return nil, ErrBadSnapshot
	}
	scaleIndex := ladderIndex(item.Scale)
	minIndex := ladderIndex(item.Min)
	maxIndex := ladderIndex(item.Max)
	if scaleIndex < 0 || minIndex < 0 || maxIndex < 0 || item.Min >= item.Max {
		return nil, ErrBadSnapshot
	}
	if item.Scale < item.Min || item.Scale > item.Max {
		return nil, ErrBadSnapshot
	}
	if item.InFlight < 0 {
		return nil, ErrBadSnapshot
	}
	if item.Stats.Reports < 0 || item.Stats.UpShifts < 0 || item.Stats.DownShifts < 0 || item.Stats.Rebuilds < 0 || item.Stats.StreakUp < 0 || item.Stats.StreakDown < 0 {
		return nil, ErrBadSnapshot
	}
	if uint64(item.Stats.Reports) != item.SampleSeq {
		return nil, ErrBadSnapshot
	}
	if item.Stats.Frozen != item.Frozen {
		return nil, ErrBadSnapshot
	}
	if item.Frozen && strings.TrimSpace(item.FreezeReason) == "" || !item.Frozen && item.FreezeReason != "" {
		return nil, ErrBadSnapshot
	}
	if item.Stats.Rebuilds != item.Stats.UpShifts+item.Stats.DownShifts {
		return nil, ErrBadSnapshot
	}
	if item.HasUpShift != (item.Stats.UpShifts > 0) || item.HasDownShift != (item.Stats.DownShifts > 0) {
		return nil, ErrBadSnapshot
	}
	if item.HasUpShift && item.LastUpShift > item.SampleSeq || !item.HasUpShift && item.LastUpShift != 0 {
		return nil, ErrBadSnapshot
	}
	if item.HasDownShift && item.LastDownShift > item.SampleSeq || !item.HasDownShift && item.LastDownShift != 0 {
		return nil, ErrBadSnapshot
	}
	if item.Pending != nil {
		if item.Pending.Viewport != item.Viewport || item.Pending.From != item.Scale || item.Pending.Sample > item.SampleSeq {
			return nil, ErrBadSnapshot
		}
		targetIndex := ladderIndex(item.Pending.To)
		if targetIndex < 0 || targetIndex < minIndex || targetIndex > maxIndex || targetIndex != scaleIndex-1 && targetIndex != scaleIndex+1 {
			return nil, ErrBadSnapshot
		}
		if item.Frozen {
			return nil, ErrBadSnapshot
		}
	}
	if item.SkipDownSample && (!item.HasUpShift || item.Pending != nil) {
		return nil, ErrBadSnapshot
	}
	if item.Frozen && (item.Stats.StreakUp != 0 || item.Stats.StreakDown != 0 || item.Pending != nil || item.SkipDownSample) {
		return nil, ErrBadSnapshot
	}

	stats := item.Stats
	stats.Frozen = item.Frozen
	return &viewportState{
		scale:          item.Scale,
		min:            item.Min,
		max:            item.Max,
		frozen:         item.Frozen,
		freezeReason:   item.FreezeReason,
		pending:        item.Pending,
		stats:          stats,
		sampleSeq:      item.SampleSeq,
		inFlight:       item.InFlight,
		hasUpShift:     item.HasUpShift,
		lastUpShift:    item.LastUpShift,
		hasDownShift:   item.HasDownShift,
		lastDownShift:  item.LastDownShift,
		skipDownSample: item.SkipDownSample,
	}, nil
}
