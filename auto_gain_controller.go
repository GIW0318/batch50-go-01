// Package task drives the automatic gain stage of a capture chain: it follows
// the frame level with an asymmetric envelope, freezes below the gate and
// protects the output from clipping.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
)

// Level and range constants of the gain stage.
const (
	// SilenceFloorDBFS is the level reported for an all zero frame.
	SilenceFloorDBFS = -120.0
	// MinTargetDBFS and MaxTargetDBFS bound the level the stage aims at.
	MinTargetDBFS = -60.0
	MaxTargetDBFS = 0.0
	// MaxBoostDB and MinTrimDB bound the configured gain window.
	MaxBoostDB = 40.0
	MinTrimDB  = -60.0
	// MinGateDBFS and MaxGateDBFS bound the gate threshold.
	MinGateDBFS = -90.0
	MaxGateDBFS = -20.0
	// MinLimiterDBFS and MaxLimiterDBFS bound the limiter ceiling.
	MinLimiterDBFS = -12.0
	MaxLimiterDBFS = 0.0
	// MaxTimeConstantMS bounds both envelope time constants.
	MaxTimeConstantMS = 1000.0
	// MinSampleRate and MaxSampleRate bound the accepted sample rates.
	MinSampleRate = 8000
	MaxSampleRate = 384000
)

// AgcConfig is the tunable part of the controller.
type AgcConfig struct {
	SampleRate        int
	TargetDBFS        float64
	MaxGainDB         float64
	MinGainDB         float64
	GateThresholdDBFS float64
	AttackMS          float64
	ReleaseMS         float64
	LimiterDBFS       float64
}

// Errors reported by the controller.
var (
	ErrBadConfig      = errors.New("gain configuration is invalid")
	ErrBadRate        = errors.New("sample rate out of range")
	ErrBadTarget      = errors.New("target level out of range")
	ErrBadGainRange   = errors.New("gain window is invalid")
	ErrBadGate        = errors.New("gate threshold out of range")
	ErrBadTiming      = errors.New("envelope time constant out of range")
	ErrBadLimiter     = errors.New("limiter ceiling out of range")
	ErrBadMaxGain     = errors.New("maximum gain out of range")
	ErrEmptyFrame     = errors.New("frame must contain at least one sample")
	ErrLengthMismatch = errors.New("stereo channels differ in length")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// GateState reports the gate decision of the most recent frame.
type GateState struct {
	Gated  bool
	Frames int64
}

// GainState is what the chain gets back for one processed frame.
type GainState struct {
	Frame        int64
	GainDB       float64
	PeakDBFS     float64
	OutputDBFS   float64
	Gated        bool
	Limited      bool
	ChannelGains []float64
}

// AutoGainController owns the envelope, the gate counter and the limiter
// ceiling of one capture chain. All state moves in a single commit under
// the mutex, so concurrent readers only ever see a fully applied frame.
type AutoGainController struct {
	mu         sync.RWMutex
	cfg        AgcConfig
	gainDB     float64
	frames     int64
	gateFrames int64
	gated      bool
}

// DefaultAgcConfig returns the constants used by the capture chain.
func DefaultAgcConfig() AgcConfig {
	return AgcConfig{
		SampleRate:        48000,
		TargetDBFS:        -12,
		MaxGainDB:         20,
		MinGainDB:         -20,
		GateThresholdDBFS: -50,
		AttackMS:          5,
		ReleaseMS:         200,
		LimiterDBFS:       -1,
	}
}

// NewAutoGainController stores the configuration and starts at unity gain.
func NewAutoGainController(cfg AgcConfig) *AutoGainController {
	return &AutoGainController{cfg: cfg}
}

// Config returns the active configuration.
func (c *AutoGainController) Config() AgcConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func validateConfig(cfg AgcConfig) error {
	if cfg.SampleRate < MinSampleRate || cfg.SampleRate > MaxSampleRate {
		return ErrBadRate
	}
	if !isFinite(cfg.TargetDBFS) || cfg.TargetDBFS < MinTargetDBFS || cfg.TargetDBFS > MaxTargetDBFS {
		return ErrBadTarget
	}
	if !isFinite(cfg.MinGainDB) || !isFinite(cfg.MaxGainDB) ||
		cfg.MinGainDB < MinTrimDB || cfg.MinGainDB > 0 ||
		cfg.MaxGainDB < 0 || cfg.MaxGainDB > MaxBoostDB ||
		cfg.MinGainDB > cfg.MaxGainDB {
		return ErrBadGainRange
	}
	if !isFinite(cfg.GateThresholdDBFS) || cfg.GateThresholdDBFS < MinGateDBFS || cfg.GateThresholdDBFS > MaxGateDBFS {
		return ErrBadGate
	}
	if !isFinite(cfg.AttackMS) || !isFinite(cfg.ReleaseMS) ||
		cfg.AttackMS <= 0 || cfg.AttackMS > MaxTimeConstantMS ||
		cfg.ReleaseMS <= 0 || cfg.ReleaseMS > MaxTimeConstantMS {
		return ErrBadTiming
	}
	if !isFinite(cfg.LimiterDBFS) || cfg.LimiterDBFS < MinLimiterDBFS || cfg.LimiterDBFS > MaxLimiterDBFS {
		return ErrBadLimiter
	}
	return nil
}

// Validate checks every configuration field.
func (c *AutoGainController) Validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return validateConfig(c.cfg)
}

// peakDBFS folds a frame down to its peak absolute level in dBFS. An all
// zero frame reports the silence floor.
func peakDBFS(frame []float32) float64 {
	peak := 0.0
	for _, v := range frame {
		if a := math.Abs(float64(v)); a > peak {
			peak = a
		}
	}
	if peak < 1e-6 {
		return SilenceFloorDBFS
	}
	return 20 * math.Log10(peak)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// step moves the gain one frame towards the target: attack when the gain
// has to fall, release when it has to rise.
func step(cfg AgcConfig, gain, target, frameSeconds float64) float64 {
	tc := cfg.ReleaseMS
	if target < gain {
		tc = cfg.AttackMS
	}
	alpha := 1 - math.Exp(-frameSeconds/(tc/1000))
	return gain + alpha*(target-gain)
}

// processFrame runs the shared gate/envelope/limiter pipeline. The caller
// must hold the write lock; the state is committed exactly once per frame.
func (c *AutoGainController) processFrame(peak float64, frameLen, channels int) GainState {
	cfg := c.cfg
	c.frames++
	state := GainState{
		Frame:        c.frames,
		PeakDBFS:     peak,
		ChannelGains: make([]float64, channels),
	}
	if peak < cfg.GateThresholdDBFS {
		// Below the gate the envelope freezes: no smoothing step and no
		// memory of what the level did while the gate was closed.
		c.gateFrames++
		c.gated = true
		state.GainDB = c.gainDB
		state.Gated = true
		state.OutputDBFS = peak + c.gainDB
		for i := range state.ChannelGains {
			state.ChannelGains[i] = c.gainDB
		}
		return state
	}
	c.gateFrames = 0
	c.gated = false

	// The target never aims above 0 dBFS, even if a misconfigured chain
	// asks for it: clipping once ruins the whole take.
	targetLevel := math.Min(cfg.TargetDBFS, MaxTargetDBFS)
	target := clamp(targetLevel-peak, cfg.MinGainDB, cfg.MaxGainDB)
	frameSeconds := float64(frameLen) / float64(cfg.SampleRate)
	gain := step(cfg, c.gainDB, target, frameSeconds)

	if peak+gain > cfg.LimiterDBFS {
		gain = cfg.LimiterDBFS - peak
		state.Limited = true
	}
	c.gainDB = gain
	state.GainDB = gain
	state.OutputDBFS = peak + gain
	for i := range state.ChannelGains {
		state.ChannelGains[i] = gain
	}
	return state
}

// Process runs one mono frame through gate, envelope and limiter.
func (c *AutoGainController) Process(frame []float32) (GainState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateConfig(c.cfg); err != nil {
		return GainState{}, fmt.Errorf("%w: %v", ErrBadConfig, err)
	}
	if len(frame) == 0 {
		return GainState{}, ErrEmptyFrame
	}
	return c.processFrame(peakDBFS(frame), len(frame), 1), nil
}

// ProcessStereo runs one linked stereo pair through the same chain. The
// louder of the two channels drives the shared envelope so neither channel
// is pushed against the limiter on its own.
func (c *AutoGainController) ProcessStereo(left, right []float32) (GainState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateConfig(c.cfg); err != nil {
		return GainState{}, fmt.Errorf("%w: %v", ErrBadConfig, err)
	}
	if len(left) == 0 && len(right) == 0 {
		return GainState{}, ErrEmptyFrame
	}
	if len(left) != len(right) {
		return GainState{}, ErrLengthMismatch
	}
	peak := math.Max(peakDBFS(left), peakDBFS(right))
	return c.processFrame(peak, len(left), 2), nil
}

// SetTargetDBFS changes the level the stage aims at.
func (c *AutoGainController) SetTargetDBFS(db float64) error {
	if !isFinite(db) || db < MinTargetDBFS || db > MaxTargetDBFS {
		return ErrBadTarget
	}
	c.mu.Lock()
	c.cfg.TargetDBFS = db
	c.mu.Unlock()
	return nil
}

// SetMaxGain changes the highest gain the stage may apply. Tightening the
// ceiling clamps the live gain immediately, not on the next frame.
func (c *AutoGainController) SetMaxGain(db float64) error {
	if !isFinite(db) || db < 0 || db > MaxBoostDB {
		return ErrBadMaxGain
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if db < c.cfg.MinGainDB {
		return ErrBadMaxGain
	}
	c.cfg.MaxGainDB = db
	if c.gainDB > db {
		c.gainDB = db
	}
	return nil
}

// SetGateThreshold changes the level below which the gain is frozen.
func (c *AutoGainController) SetGateThreshold(db float64) error {
	if !isFinite(db) || db < MinGateDBFS || db > MaxGateDBFS {
		return ErrBadGate
	}
	c.mu.Lock()
	c.cfg.GateThresholdDBFS = db
	c.mu.Unlock()
	return nil
}

// GateState reports the gate decision and the consecutive gated frame count.
func (c *AutoGainController) GateState() GateState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return GateState{Gated: c.gated, Frames: c.gateFrames}
}

// GainDB reports the gain currently applied to the signal.
func (c *AutoGainController) GainDB() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gainDB
}

// Frames reports how many frames have been processed.
func (c *AutoGainController) Frames() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.frames
}

// snapshotVersion tags the payload layout so foreign data is rejected.
const snapshotVersion = 1

type snapshotPayload struct {
	Version    int       `json:"version"`
	Config     AgcConfig `json:"config"`
	GainDB     float64   `json:"gain_db"`
	Frames     int64     `json:"frames"`
	GateFrames int64     `json:"gate_frames"`
	Gated      bool      `json:"gated"`
}

// Snapshot serialises the envelope, the gate counter and the configuration.
func (c *AutoGainController) Snapshot() ([]byte, error) {
	c.mu.RLock()
	payload := snapshotPayload{
		Version:    snapshotVersion,
		Config:     c.cfg,
		GainDB:     c.gainDB,
		Frames:     c.frames,
		GateFrames: c.gateFrames,
		Gated:      c.gated,
	}
	c.mu.RUnlock()
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing. Any
// defect in the payload leaves the live gain and counters untouched.
func (c *AutoGainController) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if payload.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, payload.Version)
	}
	if err := validateConfig(payload.Config); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if !isFinite(payload.GainDB) || payload.Frames < 0 || payload.GateFrames < 0 {
		return ErrBadSnapshot
	}
	c.mu.Lock()
	c.cfg = payload.Config
	c.gainDB = payload.GainDB
	c.frames = payload.Frames
	c.gateFrames = payload.GateFrames
	c.gated = payload.Gated
	c.mu.Unlock()
	return nil
}
