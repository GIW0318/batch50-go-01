// Package task plans gapless playback windows for codec delayed audio tracks:
// it removes encoder priming and padding samples, maps playhead positions onto
// real decoded samples and joins trimmed segments into one queue.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// EncoderInfo describes the delay and frame packing a codec adds around the
// real audio of one track.
type EncoderInfo struct {
	SampleRate int
	Frames     int
	FrameSize  int
	Priming    int64
	Padding    int64
}

// TrackView is the playable window of one opened track in decoded samples.
type TrackView struct {
	Track      string
	SampleRate int
	Priming    int64
	Padding    int64
	Encoded    int64
	Playable   int64
	Duration   time.Duration
}

// Policy trims extra samples at every segment edge on top of the encoder
// delay. The zero value keeps the encoder values only and a minimum of one
// sample per segment.
type Policy struct {
	HeadFade   int64
	TailFade   int64
	MinSamples int64
}

// Clip is one trimmed segment of a track.
type Clip struct {
	Track       string
	From        time.Duration
	To          time.Duration
	StartSample int64
	Count       int64
	SampleRate  int
	Duration    time.Duration
}

// Errors reported by the planner.
var (
	ErrBlankTrack   = errors.New("track name is required")
	ErrBadEncoder   = errors.New("encoder info is invalid")
	ErrUnknownTrack = errors.New("track is not open")
	ErrBadRange     = errors.New("trim range is invalid")
	ErrBadPolicy    = errors.New("trim policy is invalid")
	ErrShortClip    = errors.New("trimmed segment is shorter than the policy minimum")
	ErrNoClips      = errors.New("no clips to join")
	ErrRateMismatch = errors.New("clip sample rates differ")
	ErrBadSnapshot  = errors.New("snapshot payload is invalid")
)

// GaplessTrimPlanner owns the open tracks, their trimmed segments and the
// active edge policy.
type GaplessTrimPlanner struct {
	policy Policy
}

// NewGaplessTrimPlanner builds a planner without any open track.
func NewGaplessTrimPlanner() *GaplessTrimPlanner {
	return &GaplessTrimPlanner{}
}

// Open registers a track or replaces its encoder info, dropping old segments.
func (p *GaplessTrimPlanner) Open(track string, info EncoderInfo) (TrackView, error) {
	return TrackView{}, errNotImplemented
}

// Close removes a track together with its trimmed segments.
func (p *GaplessTrimPlanner) Close(track string) error {
	return errNotImplemented
}

// Trim cuts one segment out of the playable timeline of a track.
func (p *GaplessTrimPlanner) Trim(track string, from, to time.Duration) (Clip, error) {
	return Clip{}, errNotImplemented
}

// Join concatenates trimmed segments into one queue segment.
func (p *GaplessTrimPlanner) Join(clips []Clip) (Clip, error) {
	return Clip{}, errNotImplemented
}

// SampleAt maps a playhead position onto a real decoded sample index.
func (p *GaplessTrimPlanner) SampleAt(track string, pos time.Duration) (int64, error) {
	return 0, errNotImplemented
}

// Duration reports the playable duration of one track.
func (p *GaplessTrimPlanner) Duration(track string) (time.Duration, error) {
	return 0, errNotImplemented
}

// SetPaddingPolicy replaces the edge policy after validating it.
func (p *GaplessTrimPlanner) SetPaddingPolicy(policy Policy) error {
	return errNotImplemented
}

// Policy returns the effective edge policy.
func (p *GaplessTrimPlanner) Policy() Policy {
	return Policy{}
}

// View reads one track view.
func (p *GaplessTrimPlanner) View(track string) (TrackView, bool) {
	return TrackView{}, false
}

// Tracks lists the open track names in ascending order.
func (p *GaplessTrimPlanner) Tracks() []string {
	return nil
}

// Clips returns a copy of the trimmed segments in trimming order.
func (p *GaplessTrimPlanner) Clips() []Clip {
	return nil
}

// Snapshot serialises tracks, segments and the active policy.
func (p *GaplessTrimPlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *GaplessTrimPlanner) Restore(data []byte) error {
	return errNotImplemented
}
