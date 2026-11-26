// Package task drives multi track cutscene timelines: cue ordering, per viewer
// clocks, skip with mandatory event replay, and abort with stage rollback.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Track kinds accepted by Load.
const (
	TrackCamera   = "camera"
	TrackDialogue = "dialogue"
	TrackAudio    = "audio"
	TrackEvent    = "event"
)

// Playback states reported by Play, Skip, Pause and Resume.
const (
	StatePlaying  = "playing"
	StatePaused   = "paused"
	StateFinished = "finished"
	StateAborted  = "aborted"
)

// Errors returned by the director.
var (
	ErrBlankSequenceID   = errors.New("sequence id is required")
	ErrNoTracks          = errors.New("a sequence needs at least one track")
	ErrBadTrackKind      = errors.New("unknown track kind")
	ErrDuplicateTrack    = errors.New("duplicate track id")
	ErrTooManyTracks     = errors.New("track limit reached")
	ErrTooManyCues       = errors.New("cue limit reached")
	ErrBadCue            = errors.New("cue time outside its track")
	ErrUnknownSequence   = errors.New("unknown sequence")
	ErrViewerLimit       = errors.New("concurrent viewer limit reached")
	ErrViewerNotWatching = errors.New("viewer is not watching this sequence")
	ErrBadAbortReason    = errors.New("abort reason is required")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Cue is one timed instruction on a track. ViewerID is empty while the cue is
// authored in Load and carries the recipient once Tick or Skip hands it out.
type Cue struct {
	ID        string `json:"id"`
	TrackID   string `json:"track_id"`
	Kind      string `json:"kind"`
	AtMs      int    `json:"at_ms"`
	Priority  int    `json:"priority"`
	Payload   string `json:"payload"`
	Mandatory bool   `json:"mandatory"`
	ViewerID  string `json:"viewer_id"`
}

// Track groups cues of one kind. Duration is the track length in milliseconds.
type Track struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	DurationMs int    `json:"duration_ms"`
	Cues       []Cue  `json:"cues"`
}

// Playback is one viewer's clock over a sequence. InputLocked and CameraAnchor
// describe the stage guards that Play applies and Abort must undo.
type Playback struct {
	SequenceID   string    `json:"sequence_id"`
	ViewerID     string    `json:"viewer_id"`
	State        string    `json:"state"`
	PositionMs   int       `json:"position_ms"`
	StartedAt    time.Time `json:"started_at"`
	Seq          int64     `json:"seq"`
	InputLocked  bool      `json:"input_locked"`
	CameraAnchor string    `json:"camera_anchor"`
	AbortReason  string    `json:"abort_reason"`
}

// Limits bounds the loaded table and the live viewers.
type Limits struct {
	MaxTracks         int
	MaxCues           int
	MaxViewersPerShow int
}

// CutsceneSequenceDirector owns the loaded sequences, the per viewer clocks and
// the delivery watermark used to keep cues from being replayed.
type CutsceneSequenceDirector struct {
	limits Limits
}

// NewCutsceneSequenceDirector builds a director with the given caps.
func NewCutsceneSequenceDirector(limits Limits) *CutsceneSequenceDirector {
	return &CutsceneSequenceDirector{limits: limits}
}

// Load validates a sequence and replaces any previous definition of that id.
func (d *CutsceneSequenceDirector) Load(sequenceID string, tracks []Track) error {
	return errNotImplemented
}

// Play opens a viewer clock at the given wall clock time.
func (d *CutsceneSequenceDirector) Play(sequenceID, viewerID string, at time.Time) (Playback, error) {
	return Playback{}, errNotImplemented
}

// Tick advances every live clock and returns the cues that became due.
func (d *CutsceneSequenceDirector) Tick(now time.Time) ([]Cue, error) {
	return nil, errNotImplemented
}

// Skip pushes one viewer straight to the end of the sequence.
func (d *CutsceneSequenceDirector) Skip(viewerID string) (Playback, error) {
	return Playback{}, errNotImplemented
}

// Pause freezes one viewer clock without losing its position.
func (d *CutsceneSequenceDirector) Pause(viewerID string) error {
	return errNotImplemented
}

// Resume unfreezes a paused viewer clock.
func (d *CutsceneSequenceDirector) Resume(viewerID string) error {
	return errNotImplemented
}

// Abort stops a sequence for every viewer and releases the stage guards.
func (d *CutsceneSequenceDirector) Abort(sequenceID, reason string) error {
	return errNotImplemented
}

// Position reads one viewer clock.
func (d *CutsceneSequenceDirector) Position(viewerID string) (Playback, bool) {
	return Playback{}, false
}

// Observers lists the viewers of one sequence, ordered by viewer id.
func (d *CutsceneSequenceDirector) Observers(sequenceID string) []string {
	return nil
}

// Snapshot serialises the sequences, the viewer clocks and the watermarks.
func (d *CutsceneSequenceDirector) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *CutsceneSequenceDirector) Restore(data []byte) error {
	return errNotImplemented
}
