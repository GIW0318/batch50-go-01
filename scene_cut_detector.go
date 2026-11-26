// Package task scores consecutive video frames for scene changes and emits the
// cut points that the ingest pipeline uses to close a segment.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
)

// Detection constants.
const (
	// DefaultHighThreshold is the score that confirms a cut on its own.
	DefaultHighThreshold = 0.55
	// DefaultLowThreshold is the score below which a frame is plain video.
	DefaultLowThreshold = 0.3
	// DefaultMinIntervalMS is the shortest accepted distance between two cuts.
	DefaultMinIntervalMS = 400
	// DefaultHistBins is the luma histogram resolution.
	DefaultHistBins = 8
	// DefaultHistWeight and DefaultEdgeWeight split the score between the two
	// evidence sources and always add up to one.
	DefaultHistWeight = 0.7
	// DefaultEdgeWeight complements DefaultHistWeight.
	DefaultEdgeWeight = 0.3
	// DefaultNominalFrameMS is the frame duration used when timestamps cannot
	// be trusted.
	DefaultNominalFrameMS = 40
	// DefaultMaxHistory is how many frame scores are kept for inspection.
	DefaultMaxHistory = 512
)

const (
	weightTolerance      = 1e-6
	edgeDenominatorFloor = 1e-9
	intervalTolerance    = 1e-9
	snapshotMagic        = "gsb-scene-cut-detector"
	snapshotVersion      = 1
)

// Errors reported by the detector.
var (
	ErrBadConfig       = errors.New("cut configuration is invalid")
	ErrBadThreshold    = errors.New("cut thresholds are invalid")
	ErrBadMinInterval  = errors.New("minimum interval must not be negative")
	ErrBadBins         = errors.New("histogram resolution must be at least two bins")
	ErrBadWeights      = errors.New("histogram and edge weights must add up to one")
	ErrBadNominal      = errors.New("nominal frame duration must be positive")
	ErrBadHistory      = errors.New("score history limit must be positive")
	ErrBadHistogram    = errors.New("luma histogram is invalid")
	ErrBadEdges        = errors.New("edge density is invalid")
	ErrIndexOutOfRange = errors.New("frame index out of range")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// CutConfig is the tunable part of the detector.
type CutConfig struct {
	HighThreshold  float64
	LowThreshold   float64
	HistBins       int
	HistWeight     float64
	EdgeWeight     float64
	MinIntervalMS  int
	NominalFrameMS float64
	MaxHistory     int
}

// FrameStats is the evidence extracted from one decoded frame. Luma holds a
// normalised luma histogram, Edges the normalised edge density.
type FrameStats struct {
	Luma  []float64
	Edges float64
}

// FrameVerdict is the per frame outcome.
type FrameVerdict struct {
	Index int
	PTS   int64
	Score float64
	Cut   bool
}

// CutPoint is one accepted scene change.
type CutPoint struct {
	Index int
	PTS   int64
	Score float64
}

type candidateState struct {
	pending       *CutPoint
	pendingTimeMS float64
	highActive    bool
}

// SceneCutDetector owns the score history, the candidate run and the cut list.
type SceneCutDetector struct {
	mu sync.RWMutex

	cfg CutConfig

	frameCount int
	scores     []float64

	prevLuma  []float64
	prevEdges float64
	hasPrev   bool

	cuts       []CutPoint
	cutTimesMS []float64
	candidate  *candidateState

	monoPTSMS    float64
	rawPTS       int64
	hasClock     bool
	clockTrusted bool
}

// NewSceneCutDetector stores the configuration and starts from an empty clip.
func NewSceneCutDetector(cfg CutConfig) *SceneCutDetector {
	detector := &SceneCutDetector{cfg: cfg}
	if cfg.MaxHistory > 0 {
		detector.scores = make([]float64, cfg.MaxHistory)
	}
	return detector
}

// DefaultCutConfig returns the constants used by the ingest pipeline.
func DefaultCutConfig() CutConfig {
	return CutConfig{
		HighThreshold:  DefaultHighThreshold,
		LowThreshold:   DefaultLowThreshold,
		HistBins:       DefaultHistBins,
		HistWeight:     DefaultHistWeight,
		EdgeWeight:     DefaultEdgeWeight,
		MinIntervalMS:  DefaultMinIntervalMS,
		NominalFrameMS: DefaultNominalFrameMS,
		MaxHistory:     DefaultMaxHistory,
	}
}

// Validate checks every configuration field.
func (d *SceneCutDetector) Validate() error {
	d.mu.RLock()
	cfg := d.cfg
	d.mu.RUnlock()
	return validateConfig(cfg)
}

// PushFrame scores one frame against its predecessor and returns the verdict.
func (d *SceneCutDetector) PushFrame(frame FrameStats, pts int64) (FrameVerdict, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := validateConfig(d.cfg); err != nil {
		return FrameVerdict{}, err
	}
	if err := validateHistogram(frame.Luma, d.cfg.HistBins); err != nil {
		return FrameVerdict{}, err
	}
	if err := validateEdges(frame.Edges); err != nil {
		return FrameVerdict{}, err
	}

	index := d.frameCount
	timeMS := d.advanceClockLocked(pts)

	score := 0.0
	if d.hasPrev {
		score = scoreFrame(d.cfg, d.prevLuma, d.prevEdges, frame.Luma, frame.Edges)
	}

	verdict := FrameVerdict{Index: index, PTS: pts, Score: score}
	if index > 0 {
		d.classifyLocked(score, index, pts, timeMS, &verdict)
	}

	d.scores[index%d.cfg.MaxHistory] = score
	d.prevLuma = append([]float64(nil), frame.Luma...)
	d.prevEdges = frame.Edges
	d.hasPrev = true
	d.frameCount = index + 1

	return verdict, nil
}

// SetThresholds changes both decision thresholds.
func (d *SceneCutDetector) SetThresholds(high, low float64) error {
	if err := validateThresholds(high, low); err != nil {
		return fmt.Errorf("%w: high=%v low=%v", ErrBadThreshold, high, low)
	}

	d.mu.Lock()
	d.cfg.HighThreshold = high
	d.cfg.LowThreshold = low
	d.mu.Unlock()
	return nil
}

// Thresholds reports the active thresholds.
func (d *SceneCutDetector) Thresholds() (float64, float64) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg.HighThreshold, d.cfg.LowThreshold
}

// SetMinInterval changes the shortest accepted distance between two cuts.
func (d *SceneCutDetector) SetMinInterval(ms int) error {
	if ms < 0 {
		return fmt.Errorf("%w: %d", ErrBadMinInterval, ms)
	}

	d.mu.Lock()
	d.cfg.MinIntervalMS = ms
	d.mu.Unlock()
	return nil
}

// MinInterval reports the active minimum interval in milliseconds.
func (d *SceneCutDetector) MinInterval() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg.MinIntervalMS
}

// Flush closes the active transition and returns every accepted cut point.
func (d *SceneCutDetector) Flush() ([]CutPoint, error) {
	d.mu.Lock()
	d.closeCandidateLocked()
	cuts := cloneCuts(d.cuts)
	d.mu.Unlock()
	return cuts, nil
}

// Reset drops the clip state but keeps the configuration.
func (d *SceneCutDetector) Reset() error {
	d.mu.Lock()
	d.frameCount = 0
	if d.cfg.MaxHistory > 0 {
		d.scores = make([]float64, d.cfg.MaxHistory)
	} else {
		d.scores = nil
	}
	d.prevLuma = nil
	d.prevEdges = 0
	d.hasPrev = false
	d.cuts = nil
	d.cutTimesMS = nil
	d.candidate = nil
	d.monoPTSMS = 0
	d.rawPTS = 0
	d.hasClock = false
	d.clockTrusted = false
	d.mu.Unlock()
	return nil
}

// Cuts returns a copy of the accepted cut points.
func (d *SceneCutDetector) Cuts() []CutPoint {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return cloneCuts(d.cuts)
}

// Score reads the score of one frame that is still in the history window.
func (d *SceneCutDetector) Score(index int) (float64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if index < 0 || index >= d.frameCount {
		return 0, fmt.Errorf("%w: frame %d has not been scored", ErrIndexOutOfRange, index)
	}
	if index < d.frameCount-d.cfg.MaxHistory {
		return 0, fmt.Errorf("%w: frame %d has been evicted", ErrIndexOutOfRange, index)
	}
	return d.scores[index%d.cfg.MaxHistory], nil
}

// FrameCount reports how many frames have been scored.
func (d *SceneCutDetector) FrameCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.frameCount
}

// Snapshot serialises the history, the cut list and the thresholds.
func (d *SceneCutDetector) Snapshot() ([]byte, error) {
	d.mu.RLock()
	state := snapshotData{
		Cfg:          d.cfg,
		FrameCount:   d.frameCount,
		Scores:       append([]float64(nil), d.scores...),
		PrevLuma:     append([]float64(nil), d.prevLuma...),
		PrevEdges:    d.prevEdges,
		HasPrev:      d.hasPrev,
		Cuts:         cloneCuts(d.cuts),
		CutTimesMS:   append([]float64(nil), d.cutTimesMS...),
		MonoPTSMS:    d.monoPTSMS,
		RawPTS:       d.rawPTS,
		HasClock:     d.hasClock,
		ClockTrusted: d.clockTrusted,
	}
	if d.candidate != nil {
		state.Candidate = &candidateData{
			PendingTimeMS: d.candidate.pendingTimeMS,
			HighActive:    d.candidate.highActive,
		}
		if d.candidate.pending != nil {
			pending := *d.candidate.pending
			state.Candidate.Pending = &pending
		}
	}
	d.mu.RUnlock()

	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(checksum[:]),
		Payload:  payload,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *SceneCutDetector) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion ||
		envelope.Checksum == "" || len(envelope.Payload) == 0 {
		return fmt.Errorf("%w: missing detector envelope", ErrBadSnapshot)
	}

	checksum := sha256.Sum256(envelope.Payload)
	if hex.EncodeToString(checksum[:]) != envelope.Checksum {
		return fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var state snapshotData
	if err := json.Unmarshal(envelope.Payload, &state); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshotData(&state); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	d.mu.Lock()
	d.cfg = state.Cfg
	d.frameCount = state.FrameCount
	d.scores = append([]float64(nil), state.Scores...)
	d.prevLuma = append([]float64(nil), state.PrevLuma...)
	d.prevEdges = state.PrevEdges
	d.hasPrev = state.HasPrev
	d.cuts = cloneCuts(state.Cuts)
	d.cutTimesMS = append([]float64(nil), state.CutTimesMS...)
	d.candidate = restoreCandidate(state.Candidate)
	d.monoPTSMS = state.MonoPTSMS
	d.rawPTS = state.RawPTS
	d.hasClock = state.HasClock
	d.clockTrusted = state.ClockTrusted
	d.mu.Unlock()
	return nil
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Payload  json.RawMessage `json:"payload"`
}

type snapshotData struct {
	Cfg          CutConfig      `json:"config"`
	FrameCount   int            `json:"frame_count"`
	Scores       []float64      `json:"scores"`
	PrevLuma     []float64      `json:"prev_luma"`
	PrevEdges    float64        `json:"prev_edges"`
	HasPrev      bool           `json:"has_prev"`
	Cuts         []CutPoint     `json:"cuts"`
	CutTimesMS   []float64      `json:"cut_times_ms"`
	Candidate    *candidateData `json:"candidate"`
	MonoPTSMS    float64        `json:"mono_pts_ms"`
	RawPTS       int64          `json:"raw_pts"`
	HasClock     bool           `json:"has_clock"`
	ClockTrusted bool           `json:"clock_trusted"`
}

type candidateData struct {
	Pending       *CutPoint `json:"pending"`
	PendingTimeMS float64   `json:"pending_time_ms"`
	HighActive    bool      `json:"high_active"`
}

func restoreCandidate(data *candidateData) *candidateState {
	if data == nil {
		return nil
	}
	candidate := &candidateState{
		pendingTimeMS: data.PendingTimeMS,
		highActive:    data.HighActive,
	}
	if data.Pending != nil {
		pending := *data.Pending
		candidate.pending = &pending
	}
	return candidate
}

func validateConfig(cfg CutConfig) error {
	if cfg.HistBins < 2 {
		return configError(ErrBadBins)
	}

	weightSum := cfg.HistWeight + cfg.EdgeWeight
	if math.IsNaN(weightSum) || math.Abs(weightSum-1) > weightTolerance {
		return configError(ErrBadWeights)
	}

	if !finiteNumber(cfg.NominalFrameMS) || cfg.NominalFrameMS <= 0 {
		return configError(ErrBadNominal)
	}
	if cfg.MaxHistory <= 0 {
		return configError(ErrBadHistory)
	}
	if cfg.MinIntervalMS < 0 {
		return configError(ErrBadMinInterval)
	}
	if err := validateThresholds(cfg.HighThreshold, cfg.LowThreshold); err != nil {
		return configError(err)
	}
	return nil
}

func configError(specific error) error {
	return fmt.Errorf("%w: %w", ErrBadConfig, specific)
}

func validateThresholds(high, low float64) error {
	if !finiteNumber(high) || !finiteNumber(low) ||
		!(low > 0) || !(low < high) || high > 1 {
		return ErrBadThreshold
	}
	return nil
}

func validateHistogram(luma []float64, bins int) error {
	if len(luma) != bins {
		return ErrBadHistogram
	}

	sum := 0.0
	for _, bin := range luma {
		if !finiteNumber(bin) || bin < 0 {
			return ErrBadHistogram
		}
		sum += bin
	}
	if !finiteNumber(sum) || sum < 0.99 || sum > 1.01 {
		return ErrBadHistogram
	}
	return nil
}

func validateEdges(edges float64) error {
	if !finiteNumber(edges) || edges < 0 || edges > 1 {
		return ErrBadEdges
	}
	return nil
}

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func scoreFrame(cfg CutConfig, prevLuma []float64, prevEdges float64, luma []float64, edges float64) float64 {
	histogramDistance := 0.0
	for i := range luma {
		histogramDistance += math.Abs(luma[i] - prevLuma[i])
	}
	histogramDistance *= 0.5

	edgeMaximum := math.Max(math.Max(prevEdges, edges), edgeDenominatorFloor)
	edgeChange := math.Abs(edges-prevEdges) / edgeMaximum

	return cfg.HistWeight*histogramDistance + cfg.EdgeWeight*edgeChange
}

func (d *SceneCutDetector) advanceClockLocked(pts int64) float64 {
	if !d.hasClock {
		d.hasClock = true
		d.clockTrusted = true
		d.rawPTS = pts
		d.monoPTSMS = float64(pts)
		return d.monoPTSMS
	}

	if pts > d.rawPTS && float64(pts) >= d.monoPTSMS {
		d.rawPTS = pts
		d.monoPTSMS = float64(pts)
		d.clockTrusted = true
		return d.monoPTSMS
	}

	d.clockTrusted = false
	d.rawPTS = pts
	d.monoPTSMS += d.cfg.NominalFrameMS
	return d.monoPTSMS
}

func (d *SceneCutDetector) classifyLocked(score float64, index int, pts int64, timeMS float64, verdict *FrameVerdict) {
	switch {
	case score >= d.cfg.HighThreshold:
		d.candidate = &candidateState{highActive: true}
		cut := CutPoint{Index: index, PTS: pts, Score: score}
		verdict.Cut = d.arbitrateCutLocked(cut, timeMS)

	case score >= d.cfg.LowThreshold:
		cut := CutPoint{Index: index, PTS: pts, Score: score}
		if d.candidate == nil {
			d.candidate = &candidateState{}
		}
		if !d.candidate.highActive {
			pending := cut
			d.candidate.pending = &pending
			d.candidate.pendingTimeMS = timeMS
		}

	default:
		d.closeCandidateLocked()
	}
}

func (d *SceneCutDetector) closeCandidateLocked() {
	if d.candidate == nil {
		return
	}
	if d.candidate.pending != nil {
		d.arbitrateCutLocked(*d.candidate.pending, d.candidate.pendingTimeMS)
	}
	d.candidate = nil
}

func (d *SceneCutDetector) arbitrateCutLocked(cut CutPoint, timeMS float64) bool {
	last := len(d.cuts) - 1
	if last < 0 {
		d.appendCutLocked(cut, timeMS)
		return true
	}

	distance := timeMS - d.cutTimesMS[last]
	if distance+intervalTolerance >= float64(d.cfg.MinIntervalMS) {
		d.appendCutLocked(cut, timeMS)
		return true
	}

	if cut.Score > d.cuts[last].Score {
		d.cuts[last] = cut
		d.cutTimesMS[last] = timeMS
		return true
	}
	return false
}

func (d *SceneCutDetector) appendCutLocked(cut CutPoint, timeMS float64) {
	d.cuts = append(d.cuts, cut)
	d.cutTimesMS = append(d.cutTimesMS, timeMS)
}

func cloneCuts(cuts []CutPoint) []CutPoint {
	result := make([]CutPoint, len(cuts))
	copy(result, cuts)
	return result
}

func validateSnapshotData(data *snapshotData) error {
	if err := validateConfig(data.Cfg); err != nil {
		return err
	}
	if data.FrameCount < 0 {
		return errors.New("negative frame count")
	}
	if len(data.Scores) != data.Cfg.MaxHistory {
		return errors.New("score window length does not match history capacity")
	}
	for _, score := range data.Scores {
		if !finiteNumber(score) {
			return errors.New("non-finite score")
		}
	}
	if data.FrameCount > 0 {
		if !data.HasClock {
			return errors.New("missing timestamp clock")
		}
		if !data.HasPrev {
			return errors.New("missing predecessor frame")
		}
		if err := validateHistogram(data.PrevLuma, data.Cfg.HistBins); err != nil {
			return err
		}
		if err := validateEdges(data.PrevEdges); err != nil {
			return err
		}
	} else if data.HasClock || data.HasPrev || len(data.Cuts) != 0 || data.Candidate != nil {
		return errors.New("empty frame count carries frame state")
	}
	if data.HasClock && !finiteNumber(data.MonoPTSMS) {
		return errors.New("non-finite monotonic timestamp")
	}
	if data.ClockTrusted && !data.HasClock {
		return errors.New("trusted clock without clock state")
	}
	if len(data.Cuts) != len(data.CutTimesMS) {
		return errors.New("cut timestamp length mismatch")
	}

	lastIndex := -1
	for i, cut := range data.Cuts {
		if cut.Index < 0 || cut.Index >= data.FrameCount || cut.Index <= lastIndex {
			return errors.New("cut index is out of order")
		}
		if !finiteNumber(cut.Score) || !finiteNumber(data.CutTimesMS[i]) {
			return errors.New("non-finite cut data")
		}
		lastIndex = cut.Index
	}

	if data.Candidate != nil {
		if !finiteNumber(data.Candidate.PendingTimeMS) {
			return errors.New("non-finite candidate timestamp")
		}
		if pending := data.Candidate.Pending; pending != nil {
			if pending.Index < 0 || pending.Index >= data.FrameCount || !finiteNumber(pending.Score) {
				return errors.New("invalid pending cut")
			}
		}
	}
	return nil
}
