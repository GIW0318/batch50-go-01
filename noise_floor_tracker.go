// Package task tracks the noise floor of a multi band denoiser and derives the
// per band suppression gain that the capture chain applies to every frame.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
)

// NoiseConfig describes the frame layout and every tracking constant of the
// noise floor estimator.
type NoiseConfig struct {
	// Bands is the number of spectral bands carved out of each frame.
	Bands int
	// FrameSize is the number of float32 samples in one frame.
	FrameSize int
	// SampleRate is the capture rate used to turn frame counts into seconds.
	SampleRate int
	// Window is the length, in frames, of the minimum statistics window.
	Window int
	// BiasDB is added to the window minimum to get the floor target.
	BiasDB float64
	// CreepDBPerSec limits how fast the floor may rise.
	CreepDBPerSec float64
	// DecayDBPerSec limits how fast the floor may fall.
	DecayDBPerSec float64
	// SpeechMarginDB is the signal to floor distance where speech starts.
	SpeechMarginDB float64
	// SuppressMarginDB is the signal to floor distance kept untouched by the gain.
	SuppressMarginDB float64
	// DelayMS is the time constant used when the gain moves down.
	DelayMS float64
	// ReleaseMS is the time constant used when the gain moves up.
	ReleaseMS float64
	// GainFloorDB is the most attenuation the chain may apply.
	GainFloorDB float64
	// InitialFloorDB is the floor every band starts from.
	InitialFloorDB float64
}

// Errors reported by the tracker.
var (
	ErrBadConfig     = errors.New("noise configuration is invalid")
	ErrBadBands      = errors.New("band count must be positive")
	ErrBadFrameSize  = errors.New("frame size must be positive and divisible by the band count")
	ErrBadSampleRate = errors.New("sample rate out of range")
	ErrBadWindow     = errors.New("minimum statistics window out of range")
	ErrBadTiming     = errors.New("attack and release time constants must be positive")
	ErrBadGainFloor  = errors.New("gain floor out of range")
	ErrBadBias       = errors.New("floor bias out of range")
	ErrBadRate       = errors.New("floor tracking rate out of range")
	ErrBadInitial    = errors.New("initial floor out of range")
	ErrFrameSize     = errors.New("frame length does not match the configuration")
	ErrBandIndex     = errors.New("band index out of range")
	ErrBadReset      = errors.New("reset reason is required")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// DefaultNoiseConfig returns the constants used by the capture chain.
func DefaultNoiseConfig() NoiseConfig {
	return NoiseConfig{
		Bands:            4,
		FrameSize:        480,
		SampleRate:       48000,
		Window:           24,
		BiasDB:           1.5,
		CreepDBPerSec:    6,
		DecayDBPerSec:    12,
		SpeechMarginDB:   3,
		SuppressMarginDB: 3,
		DelayMS:          5,
		ReleaseMS:        150,
		GainFloorDB:      -12,
		InitialFloorDB:   -60,
	}
}

// FrameDecision is what the chain gets back for one pushed frame.
type FrameDecision struct {
	Frame  int64
	Speech bool
	Floors []float64
	Gains  []float64
	At     time.Time
}

// NoiseFloorTracker owns the per band floor state, the statistics window and
// the smoothed suppression gains.
type NoiseFloorTracker struct {
	mu sync.RWMutex

	cfg NoiseConfig

	frame         int64
	floors        []float64
	gains         []float64
	probabilities []float64

	window      [][]float64
	windowPos   int
	windowCount int

	lastAt  time.Time
	hasLast bool
}

// NewNoiseFloorTracker stores the configuration and starts every band at the
// configured initial floor with unity gain.
func NewNoiseFloorTracker(cfg NoiseConfig) *NoiseFloorTracker {
	t := &NoiseFloorTracker{cfg: cfg}
	t.initializeStateLocked()
	return t
}

// Config returns the active configuration.
func (t *NoiseFloorTracker) Config() NoiseConfig {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg
}

// Validate checks every configuration field.
func (t *NoiseFloorTracker) Validate() error {
	t.mu.RLock()
	cfg := t.cfg
	t.mu.RUnlock()
	return validateNoiseConfig(cfg)
}

// Bands reports the configured band count.
func (t *NoiseFloorTracker) Bands() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.Bands
}

// Frame reports how many frames have been accepted since the last reset.
func (t *NoiseFloorTracker) Frame() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.frame
}

// PushFrame folds one frame into the floor statistics and returns the gains to
// apply to that same frame.
func (t *NoiseFloorTracker) PushFrame(frame []float32, at time.Time) (FrameDecision, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := validateNoiseConfig(t.cfg); err != nil {
		return FrameDecision{}, fmt.Errorf("%w: %v", ErrBadConfig, err)
	}
	if len(frame) != t.cfg.FrameSize || t.cfg.FrameSize%t.cfg.Bands != 0 {
		return FrameDecision{}, ErrFrameSize
	}

	bands := t.cfg.Bands
	width := t.cfg.FrameSize / bands
	levels := make([]float64, bands)
	harmonic := make([]bool, bands)
	for band := 0; band < bands; band++ {
		start := band * width
		sum := 0.0
		peak := 0.0
		for _, raw := range frame[start : start+width] {
			sample := float64(raw)
			if math.IsNaN(sample) || math.IsInf(sample, 0) {
				sample = 0
			}
			square := sample * sample
			sum += square
			if abs := math.Abs(sample); abs > peak {
				peak = abs
			}
		}
		mean := sum / float64(width)
		if mean < 1e-20 {
			mean = 1e-20
		}
		levels[band] = 10 * math.Log10(mean)

		rms := math.Sqrt(mean)
		if peak > 0 && levels[band] > t.floors[band]+3 {
			crestDB := 20 * math.Log10(peak/rms)
			harmonic[band] = crestDB < 6
		}
	}

	// The current frame joins the minimum statistics window before the target
	// is calculated, so a single quiet frame starts pulling the floor down in
	// the same commit. A harmonic frame freezes the floor instead: music must
	// not be learned as room tone, even though it remains in the window for
	// later minimum statistics.
	slot := t.windowPos
	for band := 0; band < bands; band++ {
		t.window[band][slot] = levels[band]
	}
	count := t.windowCount + 1
	if count > t.cfg.Window {
		count = t.cfg.Window
	}

	dt := t.frameDurationSeconds(at)
	for band := 0; band < bands; band++ {
		if harmonic[band] {
			continue
		}

		minimum := t.window[band][0]
		for i := 1; i < count; i++ {
			if t.window[band][i] < minimum {
				minimum = t.window[band][i]
			}
		}
		target := minimum + t.cfg.BiasDB
		change := target - t.floors[band]
		limit := t.cfg.CreepDBPerSec * dt
		if change < 0 {
			limit = t.cfg.DecayDBPerSec * dt
		}
		if change > limit {
			change = limit
		} else if change < -limit {
			change = -limit
		}
		t.floors[band] += change
	}

	speech := false
	for band := 0; band < bands; band++ {
		probability := (levels[band] - t.floors[band] - t.cfg.SpeechMarginDB) / 6
		if probability < 0 {
			probability = 0
		} else if probability > 1 {
			probability = 1
		}
		t.probabilities[band] = probability
		if probability > 0.5 {
			speech = true
		}

		target := levels[band] - (t.floors[band] + t.cfg.SuppressMarginDB)
		if target > 0 {
			target = 0
		}
		if target < t.cfg.GainFloorDB {
			target = t.cfg.GainFloorDB
		}

		current := t.gains[band]
		tauMS := t.cfg.ReleaseMS
		if target < current {
			tauMS = t.cfg.DelayMS
		}
		current += smoothingCoefficient(dt, tauMS) * (target - current)
		if current > 0 {
			current = 0
		}
		if current < t.cfg.GainFloorDB {
			current = t.cfg.GainFloorDB
		}
		t.gains[band] = current
	}

	t.windowPos = (slot + 1) % t.cfg.Window
	t.windowCount = count
	t.frame++
	t.lastAt = at
	t.hasLast = true

	floors := append([]float64(nil), t.floors...)
	gains := append([]float64(nil), t.gains...)
	return FrameDecision{
		Frame:  t.frame,
		Speech: speech,
		Floors: floors,
		Gains:  gains,
		At:     at,
	}, nil
}

// NoiseFloor reads the current floor of one band in dB.
func (t *NoiseFloorTracker) NoiseFloor(band int) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if band < 0 || band >= len(t.floors) {
		return 0, ErrBandIndex
	}
	return t.floors[band], nil
}

// Gain reads the current suppression gain of one band in dB.
func (t *NoiseFloorTracker) Gain(band int) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if band < 0 || band >= len(t.gains) {
		return 0, ErrBandIndex
	}
	return t.gains[band], nil
}

// SpeechProbability reads the current speech posterior of one band.
func (t *NoiseFloorTracker) SpeechProbability(band int) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if band < 0 || band >= len(t.probabilities) {
		return 0, ErrBandIndex
	}
	return t.probabilities[band], nil
}

// SetGainFloor changes the most attenuation the chain may apply.
func (t *NoiseFloorTracker) SetGainFloor(db float64) error {
	if !validNumberInRange(db, -60, 0) {
		return ErrBadGainFloor
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.cfg.GainFloorDB = db
	for band := range t.gains {
		if t.gains[band] < db {
			t.gains[band] = db
		}
	}
	return nil
}

// GainFloor reports the active gain floor in dB.
func (t *NoiseFloorTracker) GainFloor() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.GainFloorDB
}

// Reset drops the statistics window and every gain after an operator action.
func (t *NoiseFloorTracker) Reset(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrBadReset
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initializeStateLocked()
	return nil
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type snapshotState struct {
	Config        NoiseConfig `json:"config"`
	Frame         int64       `json:"frame"`
	Floors        []float64   `json:"floors"`
	Gains         []float64   `json:"gains"`
	Probabilities []float64   `json:"probabilities"`
	Window        [][]float64 `json:"window"`
	WindowPos     int         `json:"window_pos"`
	WindowCount   int         `json:"window_count"`
	HasLast       bool        `json:"has_last"`
	LastAt        time.Time   `json:"last_at"`
}

const (
	snapshotMagic   = "gsb/noise-floor-tracker"
	snapshotVersion = 1
)

// Snapshot serialises floors, gains, the window and the counters.
func (t *NoiseFloorTracker) Snapshot() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	state := snapshotState{
		Config:        t.cfg,
		Frame:         t.frame,
		Floors:        append([]float64(nil), t.floors...),
		Gains:         append([]float64(nil), t.gains...),
		Probabilities: append([]float64(nil), t.probabilities...),
		Window:        make([][]float64, len(t.window)),
		WindowPos:     t.windowPos,
		WindowCount:   t.windowCount,
		HasLast:       t.hasLast,
		LastAt:        t.lastAt,
	}
	for band := range t.window {
		state.Window[band] = append([]float64(nil), t.window[band]...)
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Payload:  payload,
		Checksum: hex.EncodeToString(sum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (t *NoiseFloorTracker) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := decodeStrict(data, &envelope); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported snapshot", ErrBadSnapshot)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, envelope.Payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	payload := compact.Bytes()
	sum := sha256.Sum256(payload)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var state snapshotState
	if err := decodeStrict(payload, &state); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshotState(state); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if !sameBandLayout(t.cfg, state.Config) {
		return fmt.Errorf("%w: band layout differs", ErrBadSnapshot)
	}

	t.cfg = state.Config
	t.frame = state.Frame
	t.floors = state.Floors
	t.gains = state.Gains
	t.probabilities = state.Probabilities
	t.window = state.Window
	t.windowPos = state.WindowPos
	t.windowCount = state.WindowCount
	t.lastAt = state.LastAt
	t.hasLast = state.HasLast
	return nil
}

func validateNoiseConfig(cfg NoiseConfig) error {
	if cfg.Bands <= 0 {
		return ErrBadBands
	}
	if cfg.FrameSize <= 0 || cfg.FrameSize%cfg.Bands != 0 {
		return ErrBadFrameSize
	}
	if cfg.SampleRate < 8000 || cfg.SampleRate > 192000 {
		return ErrBadSampleRate
	}
	if cfg.Window < 1 || cfg.Window > 1000 {
		return ErrBadWindow
	}
	if !positiveFinite(cfg.DelayMS) || !positiveFinite(cfg.ReleaseMS) {
		return ErrBadTiming
	}
	if !validNumberInRange(cfg.GainFloorDB, -60, 0) {
		return ErrBadGainFloor
	}
	if !validNumberInRange(cfg.BiasDB, 0, 20) {
		return ErrBadBias
	}
	if !validNumberInRange(cfg.CreepDBPerSec, 0, 60) || !validNumberInRange(cfg.DecayDBPerSec, 0, 60) {
		return ErrBadRate
	}
	if !validNumberInRange(cfg.InitialFloorDB, -120, -10) {
		return ErrBadInitial
	}
	if math.IsNaN(cfg.SpeechMarginDB) || math.IsInf(cfg.SpeechMarginDB, 0) ||
		math.IsNaN(cfg.SuppressMarginDB) || math.IsInf(cfg.SuppressMarginDB, 0) {
		return ErrBadConfig
	}
	return nil
}

func (t *NoiseFloorTracker) initializeStateLocked() {
	bands := t.cfg.Bands
	if bands < 0 {
		bands = 0
	}
	window := t.cfg.Window
	if window < 0 {
		window = 0
	}

	t.frame = 0
	t.floors = make([]float64, bands)
	for band := range t.floors {
		t.floors[band] = t.cfg.InitialFloorDB
	}
	t.gains = make([]float64, bands)
	t.probabilities = make([]float64, bands)
	t.window = make([][]float64, bands)
	for band := range t.window {
		t.window[band] = make([]float64, window)
	}
	t.windowPos = 0
	t.windowCount = 0
	t.lastAt = time.Time{}
	t.hasLast = false
}

func (t *NoiseFloorTracker) frameDurationSeconds(at time.Time) float64 {
	nominal := float64(t.cfg.FrameSize) / float64(t.cfg.SampleRate)
	if !t.hasLast || at.IsZero() || t.lastAt.IsZero() || !at.After(t.lastAt) {
		return nominal
	}
	return at.Sub(t.lastAt).Seconds()
}

func smoothingCoefficient(dt, tauMS float64) float64 {
	if dt <= 0 {
		return 0
	}
	tau := tauMS / 1000
	if tau <= 0 {
		return 1
	}
	alpha := 1 - math.Exp(-dt/tau)
	if alpha < 0 {
		return 0
	}
	if alpha > 1 {
		return 1
	}
	return alpha
}

func positiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func validNumberInRange(v, minimum, maximum float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= minimum && v <= maximum
}

func sameBandLayout(a, b NoiseConfig) bool {
	return a.Bands == b.Bands &&
		a.FrameSize == b.FrameSize &&
		a.SampleRate == b.SampleRate &&
		a.Window == b.Window
}

func validateSnapshotState(state snapshotState) error {
	if err := validateNoiseConfig(state.Config); err != nil {
		return err
	}
	if state.Frame < 0 {
		return errors.New("negative frame counter")
	}
	bands := state.Config.Bands
	window := state.Config.Window
	if len(state.Floors) != bands || len(state.Gains) != bands || len(state.Probabilities) != bands {
		return errors.New("band vector length mismatch")
	}
	if len(state.Window) != bands {
		return errors.New("window band count mismatch")
	}
	for band := 0; band < bands; band++ {
		if len(state.Window[band]) != window {
			return errors.New("window length mismatch")
		}
		if !finite(state.Floors[band]) || !finite(state.Gains[band]) || !finite(state.Probabilities[band]) {
			return errors.New("non-finite band state")
		}
		if state.Gains[band] > 0 || state.Gains[band] < state.Config.GainFloorDB {
			return errors.New("gain outside its bounds")
		}
		if state.Probabilities[band] < 0 || state.Probabilities[band] > 1 {
			return errors.New("speech probability outside [0,1]")
		}
		for _, level := range state.Window[band] {
			if !finite(level) {
				return errors.New("non-finite window level")
			}
		}
	}
	expectedCount := window
	if state.Frame < int64(expectedCount) {
		expectedCount = int(state.Frame)
	}
	if state.WindowCount != expectedCount {
		return errors.New("window count does not match the frame counter")
	}
	expectedPos := int(state.Frame % int64(window))
	if state.WindowPos != expectedPos {
		return errors.New("window position does not match the frame counter")
	}
	return nil
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing snapshot data")
		}
		return err
	}
	return nil
}
