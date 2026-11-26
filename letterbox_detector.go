// Package task scans video frames for hard matte bars, keeps a candidate crop
// rectangle stable across several frames and publishes the crop only once the
// candidate stops moving.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults applied when a limit is left at zero.
const (
	DefaultThreshold    = 40
	DefaultStableWindow = 3
	MaxStableWindow     = 120
	MaxLuma             = 255
)

// Errors reported by the detector.
var (
	ErrBadFrame     = errors.New("frame geometry or luma plane is invalid")
	ErrBadThreshold = errors.New("luma threshold is out of range")
	ErrBadWindow    = errors.New("stable window is out of range")
	ErrBadRect      = errors.New("crop rectangle is not usable")
	ErrNoFrame      = errors.New("no frame has been accepted yet")
	ErrBadSnapshot  = errors.New("snapshot payload is invalid")
)

// Frame is one decoded picture. Luma is row major, top left first.
type Frame struct {
	Width  int
	Height int
	Luma   []uint8
	PTS    time.Duration
}

// CropRect is the content rectangle left after the matte is removed.
type CropRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Detect is the per frame result of the edge scan.
type Detect struct {
	Frame  int64 `json:"frame"`
	Top    int   `json:"top"`
	Bottom int   `json:"bottom"`
	Left   int   `json:"left"`
	Right  int   `json:"right"`
	Streak int   `json:"streak"`
	Stable bool  `json:"stable"`
}

// DetectStats counts what the detector saw over its lifetime.
type DetectStats struct {
	Frames    int `json:"frames"`
	Stable    int `json:"stable"`
	Rejected  int `json:"rejected"`
	Manual    int `json:"manual"`
	MaxStreak int `json:"max_streak"`
}

// Config carries the luma threshold and the stability requirement.
type Config struct {
	Threshold    int
	StableWindow int
}

// LetterboxDetector owns the scan state, the candidate and the published rect.
type LetterboxDetector struct {
	mu        sync.Mutex
	threshold int
	window    int
	width     int
	height    int
	seen      bool
	rect      CropRect
	candidate CropRect
	streak    int
	frameSeq  int64
	stats     DetectStats
}

// NewLetterboxDetector builds a detector with the given configuration.
func NewLetterboxDetector(cfg Config) *LetterboxDetector {
	return &LetterboxDetector{threshold: cfg.Threshold, window: cfg.StableWindow}
}

// Feed scans one frame and reports the matte it found.
func (d *LetterboxDetector) Feed(frame Frame) (Detect, error) {
	return Detect{}, errNotImplemented
}

// Rect reads the crop rectangle currently in force.
func (d *LetterboxDetector) Rect() CropRect {
	return CropRect{}
}

// FrameSize reports the geometry of the last accepted frame.
func (d *LetterboxDetector) FrameSize() (int, int) {
	return 0, 0
}

// SetThreshold changes the luma value below which an edge row counts as matte.
func (d *LetterboxDetector) SetThreshold(luma int) error {
	return errNotImplemented
}

// SetStableWindow changes how many identical candidates are required.
func (d *LetterboxDetector) SetStableWindow(n int) error {
	return errNotImplemented
}

// Commit installs a crop rectangle chosen by the operator.
func (d *LetterboxDetector) Commit(rect CropRect) error {
	return errNotImplemented
}

// Reset drops the scan state and the counters.
func (d *LetterboxDetector) Reset() error {
	return errNotImplemented
}

// Stats reads the lifetime counters.
func (d *LetterboxDetector) Stats() DetectStats {
	return DetectStats{}
}

// Snapshot serialises the published rect, the candidate and the counters.
func (d *LetterboxDetector) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *LetterboxDetector) Restore(data []byte) error {
	return errNotImplemented
}
