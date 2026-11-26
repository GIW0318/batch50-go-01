// Package task scans video frames for hard matte bars, keeps a candidate crop
// rectangle stable across several frames and publishes the crop only once the
// candidate stops moving.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Defaults applied when a limit is left at zero.
const (
	DefaultThreshold    = 40
	DefaultStableWindow = 3
	MaxStableWindow     = 120
	MaxLuma             = 255

	snapshotVersion = 1
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
	rect      CropRect
	candidate CropRect
	streak    int
	stats     DetectStats
}

type edges struct {
	top    int
	bottom int
	left   int
	right  int
}

type detectorSnapshot struct {
	Version      int         `json:"version"`
	Threshold    int         `json:"threshold"`
	Window       int         `json:"stable_window"`
	Width        int         `json:"width"`
	Height       int         `json:"height"`
	Rect         CropRect    `json:"rect"`
	Candidate    CropRect    `json:"candidate"`
	Streak       int         `json:"streak"`
	Stats        DetectStats `json:"stats"`
	FrameCounter int64       `json:"frame_counter"`
}

// NewLetterboxDetector builds a detector with the given configuration.
func NewLetterboxDetector(cfg Config) *LetterboxDetector {
	threshold := cfg.Threshold
	if threshold == 0 {
		threshold = DefaultThreshold
	}
	if threshold < 1 {
		threshold = 1
	} else if threshold > MaxLuma {
		threshold = MaxLuma
	}

	window := cfg.StableWindow
	if window == 0 {
		window = DefaultStableWindow
	}
	if window < 1 {
		window = 1
	} else if window > MaxStableWindow {
		window = MaxStableWindow
	}

	return &LetterboxDetector{
		threshold: threshold,
		window:    window,
	}
}

// Feed scans one frame and reports the matte it found.
func (d *LetterboxDetector) Feed(frame Frame) (Detect, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !validFrame(frame) {
		d.stats.Rejected++
		return Detect{}, ErrBadFrame
	}

	oldWidth, oldHeight := d.width, d.height
	d.width = frame.Width
	d.height = frame.Height
	d.stats.Frames++

	if oldWidth == 0 || oldHeight == 0 {
		d.rect = fullRect(frame.Width, frame.Height)
	} else if oldWidth != frame.Width || oldHeight != frame.Height {
		d.rect = fullRect(frame.Width, frame.Height)
		d.candidate = CropRect{}
		d.streak = 0
	}

	found := scanEdges(frame, d.threshold)
	candidate := CropRect{
		X: found.left,
		Y: found.top,
		W: frame.Width - found.left - found.right,
		H: frame.Height - found.top - found.bottom,
	}
	if !validRect(candidate, frame.Width, frame.Height) {
		candidate = fullRect(frame.Width, frame.Height)
	}
	candidate = alignEven(candidate, frame.Width, frame.Height)

	if candidate == d.candidate {
		d.streak++
	} else {
		d.candidate = candidate
		d.streak = 1
	}
	if d.streak > d.stats.MaxStreak {
		d.stats.MaxStreak = d.streak
	}

	result := Detect{
		Frame:  int64(d.stats.Frames),
		Top:    found.top,
		Bottom: found.bottom,
		Left:   found.left,
		Right:  found.right,
		Streak: d.streak,
	}

	if d.streak >= d.window {
		d.rect = candidate
		d.stats.Stable++
		result.Stable = true
		d.streak = 0
	}

	return result, nil
}

// Rect reads the crop rectangle currently in force.
func (d *LetterboxDetector) Rect() CropRect {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rect
}

// FrameSize reports the geometry of the last accepted frame.
func (d *LetterboxDetector) FrameSize() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.width, d.height
}

// SetThreshold changes the luma value below which an edge line counts as matte.
func (d *LetterboxDetector) SetThreshold(luma int) error {
	if luma < 1 || luma > MaxLuma {
		return ErrBadThreshold
	}
	d.mu.Lock()
	d.threshold = luma
	d.mu.Unlock()
	return nil
}

// SetStableWindow changes how many identical candidates are required.
func (d *LetterboxDetector) SetStableWindow(n int) error {
	if n < 1 || n > MaxStableWindow {
		return ErrBadWindow
	}
	d.mu.Lock()
	d.window = n
	d.mu.Unlock()
	return nil
}

// Commit installs a crop rectangle chosen by the operator.
func (d *LetterboxDetector) Commit(rect CropRect) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.width <= 0 || d.height <= 0 {
		return ErrNoFrame
	}
	if !validRect(rect, d.width, d.height) {
		return ErrBadRect
	}

	d.rect = rect
	d.candidate = rect
	d.streak = 0
	d.stats.Manual++
	return nil
}

// Reset drops the scan state and the counters.
func (d *LetterboxDetector) Reset() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.stats = DetectStats{}
	d.streak = 0
	d.candidate = CropRect{}
	if d.width > 0 && d.height > 0 {
		d.rect = fullRect(d.width, d.height)
	} else {
		d.rect = CropRect{}
	}
	return nil
}

// Stats reads the lifetime counters.
func (d *LetterboxDetector) Stats() DetectStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}

// Snapshot serialises the published rect, the candidate and the counters.
func (d *LetterboxDetector) Snapshot() ([]byte, error) {
	d.mu.Lock()
	snapshot := detectorSnapshot{
		Version:      snapshotVersion,
		Threshold:    d.threshold,
		Window:       d.window,
		Width:        d.width,
		Height:       d.height,
		Rect:         d.rect,
		Candidate:    d.candidate,
		Streak:       d.streak,
		Stats:        d.stats,
		FrameCounter: int64(d.stats.Frames),
	}
	d.mu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode letterbox snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *LetterboxDetector) Restore(data []byte) error {
	var snapshot detectorSnapshot
	if len(data) == 0 {
		return fmt.Errorf("%w: empty payload", ErrBadSnapshot)
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}

	d.mu.Lock()
	d.threshold = snapshot.Threshold
	d.window = snapshot.Window
	d.width = snapshot.Width
	d.height = snapshot.Height
	d.rect = snapshot.Rect
	d.candidate = snapshot.Candidate
	d.streak = snapshot.Streak
	d.stats = snapshot.Stats
	d.mu.Unlock()
	return nil
}

func validFrame(frame Frame) bool {
	if frame.Width <= 0 || frame.Height <= 0 {
		return false
	}
	if frame.Width > math.MaxInt/frame.Height {
		return false
	}
	return len(frame.Luma) == frame.Width*frame.Height
}

func fullRect(width, height int) CropRect {
	return CropRect{X: 0, Y: 0, W: width, H: height}
}

func validRect(rect CropRect, width, height int) bool {
	return rect.X >= 0 &&
		rect.Y >= 0 &&
		rect.W > 0 &&
		rect.H > 0 &&
		rect.X <= width-rect.W &&
		rect.Y <= height-rect.H
}

// alignEven floors the origin and size to the even coordinate grid. Valid
// candidates stay in frame; degenerate one-pixel candidates use an even full
// frame when possible and retain a valid rect in one-pixel geometries.
func alignEven(rect CropRect, width, height int) CropRect {
	aligned := CropRect{
		X: rect.X &^ 1,
		Y: rect.Y &^ 1,
		W: rect.W &^ 1,
		H: rect.H &^ 1,
	}
	if validRect(aligned, width, height) {
		return aligned
	}

	evenFull := CropRect{X: 0, Y: 0, W: width &^ 1, H: height &^ 1}
	if validRect(evenFull, width, height) {
		return evenFull
	}
	return rect
}

func scanEdges(frame Frame, threshold int) edges {
	width := frame.Width
	height := frame.Height
	luma := frame.Luma

	result := edges{}

	for y := 0; y < height; y++ {
		if rowIsMatte(luma, y*width, width, threshold) {
			result.top++
			continue
		}
		break
	}

	for y := height - 1; y >= 0; y-- {
		if rowIsMatte(luma, y*width, width, threshold) {
			result.bottom++
			continue
		}
		break
	}

	for x := 0; x < width; x++ {
		if columnIsMatte(luma, x, width, height, threshold) {
			result.left++
			continue
		}
		break
	}

	for x := width - 1; x >= 0; x-- {
		if columnIsMatte(luma, x, width, height, threshold) {
			result.right++
			continue
		}
		break
	}

	// If every line is below threshold there is no brighter content reference.
	// Treat that ambiguous dark frame as uncropped rather than letting the two
	// independent edge scans overlap and consume the picture.
	if result.top+result.bottom >= height || result.left+result.right >= width {
		return edges{}
	}

	return result
}

func rowIsMatte(luma []uint8, offset, width, threshold int) bool {
	for x := 0; x < width; x++ {
		if int(luma[offset+x]) >= threshold {
			return false
		}
	}
	return true
}

func columnIsMatte(luma []uint8, x, width, height, threshold int) bool {
	for y := 0; y < height; y++ {
		if int(luma[y*width+x]) >= threshold {
			return false
		}
	}
	return true
}

func validateSnapshot(snapshot detectorSnapshot) error {
	bad := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrBadSnapshot, reason)
	}

	if snapshot.Version != snapshotVersion {
		return bad("unsupported version")
	}
	if snapshot.Threshold < 1 || snapshot.Threshold > MaxLuma {
		return bad("threshold out of range")
	}
	if snapshot.Window < 1 || snapshot.Window > MaxStableWindow {
		return bad("window out of range")
	}
	if snapshot.Width < 0 || snapshot.Height < 0 {
		return bad("negative geometry")
	}
	if (snapshot.Width == 0) != (snapshot.Height == 0) {
		return bad("inconsistent geometry")
	}
	if snapshot.Streak < 0 {
		return bad("negative streak")
	}
	if snapshot.FrameCounter < 0 {
		return bad("negative frame counter")
	}

	stats := snapshot.Stats
	if stats.Frames < 0 || stats.Stable < 0 || stats.Rejected < 0 ||
		stats.Manual < 0 || stats.MaxStreak < 0 {
		return bad("negative statistic")
	}
	if int64(stats.Frames) != snapshot.FrameCounter {
		return bad("frame counter mismatch")
	}
	if stats.MaxStreak < snapshot.Streak {
		return bad("streak exceeds maximum streak statistic")
	}

	if snapshot.Width == 0 {
		if snapshot.Rect != (CropRect{}) {
			return bad("rect without frame geometry")
		}
		return nil
	}

	if !validRect(snapshot.Rect, snapshot.Width, snapshot.Height) {
		return bad("invalid published rect")
	}
	if snapshot.Streak > 0 &&
		!validRect(snapshot.Candidate, snapshot.Width, snapshot.Height) {
		return bad("invalid candidate rect")
	}
	return nil
}
