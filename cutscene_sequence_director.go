// Package task drives multi track cutscene timelines: cue ordering, per viewer
// clocks, skip with mandatory event replay, and abort with stage rollback.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	snapshotVersion     = 1
	defaultCameraAnchor = "camera-anchor:overworld"
)

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
	mu        sync.Mutex
	limits    Limits
	sequences map[string]*sequenceState
	viewers   map[string]*viewerState
	nextSeq   int64
}

type compiledCue struct {
	Cue
	uid string
}

type sequenceState struct {
	id          string
	tracks      []Track
	cues        []compiledCue
	durationMs  int
	lastCueAtMs int
	hasCues     bool
	watcherIDs  map[string]struct{}
}

type viewerState struct {
	Playback
	lastTickAt         time.Time
	pauseAnchorPending bool
	resyncNextTick     bool
	delivered          map[string]struct{}
}

type snapshotData struct {
	Version   int                `json:"version"`
	Limits    Limits             `json:"limits"`
	NextSeq   int64              `json:"next_seq"`
	Sequences []snapshotSequence `json:"sequences"`
	Viewers   []snapshotViewer   `json:"viewers"`
}

type snapshotSequence struct {
	ID     string  `json:"id"`
	Tracks []Track `json:"tracks"`
}

type snapshotViewer struct {
	Playback
	LastTickAt         time.Time `json:"last_tick_at"`
	PauseAnchorPending bool      `json:"pause_anchor_pending"`
	ResyncNextTick     bool      `json:"resync_next_tick"`
	Delivered          []string  `json:"delivered"`
}

// NewCutsceneSequenceDirector builds a director with the given caps.
func NewCutsceneSequenceDirector(limits Limits) *CutsceneSequenceDirector {
	return &CutsceneSequenceDirector{
		limits:    limits,
		sequences: make(map[string]*sequenceState),
		viewers:   make(map[string]*viewerState),
	}
}

// Load validates a sequence and replaces any previous definition of that id.
func (d *CutsceneSequenceDirector) Load(sequenceID string, tracks []Track) error {
	sequence, err := buildSequence(sequenceID, tracks, d.limits)
	if err != nil {
		return err
	}

	d.mu.Lock()
	d.sequences[sequenceID] = sequence
	d.mu.Unlock()
	return nil
}

func buildSequence(sequenceID string, tracks []Track, limits Limits) (*sequenceState, error) {
	if strings.TrimSpace(sequenceID) == "" {
		return nil, ErrBlankSequenceID
	}
	if len(tracks) == 0 {
		return nil, ErrNoTracks
	}
	if len(tracks) > limits.MaxTracks {
		return nil, ErrTooManyTracks
	}

	validKinds := map[string]struct{}{
		TrackCamera:   {},
		TrackDialogue: {},
		TrackAudio:    {},
		TrackEvent:    {},
	}

	copiedTracks := make([]Track, len(tracks))
	durationByTrackID := make(map[string]int, len(tracks))
	cueCount := 0

	for trackIndex, track := range tracks {
		if _, exists := durationByTrackID[track.ID]; exists {
			return nil, ErrDuplicateTrack
		}
		if _, valid := validKinds[track.Kind]; !valid {
			return nil, ErrBadTrackKind
		}

		track.Cues = append([]Cue(nil), track.Cues...)
		copiedTracks[trackIndex] = track
		durationByTrackID[track.ID] = track.DurationMs
		cueCount += len(track.Cues)
	}

	if cueCount > limits.MaxCues {
		return nil, ErrTooManyCues
	}

	compiledCues := make([]compiledCue, 0, cueCount)
	durationMs := 0

	for trackIndex, track := range copiedTracks {
		if track.DurationMs > durationMs {
			durationMs = track.DurationMs
		}

		for cueIndex, cue := range track.Cues {
			if cue.AtMs < 0 {
				return nil, ErrBadCue
			}
			trackDuration, trackExists := durationByTrackID[cue.TrackID]
			if !trackExists || cue.AtMs > trackDuration {
				return nil, ErrBadCue
			}

			compiledCues = append(compiledCues, compiledCue{
				Cue: cue,
				uid: fmt.Sprintf("%d/%d", trackIndex, cueIndex),
			})
		}
	}

	sort.SliceStable(compiledCues, func(i, j int) bool {
		return compareCue(compiledCues[i].Cue, compiledCues[j].Cue, "", "") < 0
	})

	lastCueAtMs := 0
	if len(compiledCues) > 0 {
		lastCueAtMs = compiledCues[0].AtMs
		for _, cue := range compiledCues {
			if cue.AtMs > lastCueAtMs {
				lastCueAtMs = cue.AtMs
			}
		}
	}

	return &sequenceState{
		id:          sequenceID,
		tracks:      copiedTracks,
		cues:        compiledCues,
		durationMs:  durationMs,
		lastCueAtMs: lastCueAtMs,
		hasCues:     len(compiledCues) > 0,
		watcherIDs:  make(map[string]struct{}),
	}, nil
}

func compareCue(a, b Cue, viewerA, viewerB string) int {
	if a.AtMs != b.AtMs {
		return a.AtMs - b.AtMs
	}
	if a.Priority != b.Priority {
		return b.Priority - a.Priority
	}
	if a.TrackID != b.TrackID {
		return strings.Compare(a.TrackID, b.TrackID)
	}
	if a.ID != b.ID {
		return strings.Compare(a.ID, b.ID)
	}
	return strings.Compare(viewerA, viewerB)
}

// Play opens a viewer clock at the given wall clock time.
func (d *CutsceneSequenceDirector) Play(sequenceID, viewerID string, at time.Time) (Playback, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	sequence, ok := d.sequences[sequenceID]
	if !ok {
		return Playback{}, ErrUnknownSequence
	}

	if _, watching := sequence.watcherIDs[viewerID]; !watching &&
		len(sequence.watcherIDs) >= d.limits.MaxViewersPerShow {
		return Playback{}, ErrViewerLimit
	}

	if previous, exists := d.viewers[viewerID]; exists {
		if previousSequence, ok := d.sequences[previous.SequenceID]; ok {
			delete(previousSequence.watcherIDs, viewerID)
		}
	}

	d.nextSeq++
	playback := Playback{
		SequenceID:   sequenceID,
		ViewerID:     viewerID,
		State:        StatePlaying,
		PositionMs:   0,
		StartedAt:    at,
		Seq:          d.nextSeq,
		InputLocked:  true,
		CameraAnchor: defaultCameraAnchor,
	}
	d.viewers[viewerID] = &viewerState{
		Playback:   playback,
		lastTickAt: at,
		delivered:  make(map[string]struct{}),
	}
	sequence.watcherIDs[viewerID] = struct{}{}

	return playback, nil
}

// Tick advances every live clock and returns the cues that became due.
func (d *CutsceneSequenceDirector) Tick(now time.Time) ([]Cue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	viewerIDs := make([]string, 0, len(d.viewers))
	for viewerID := range d.viewers {
		viewerIDs = append(viewerIDs, viewerID)
	}
	sort.Strings(viewerIDs)

	var cues []Cue
	for _, viewerID := range viewerIDs {
		viewer := d.viewers[viewerID]
		sequence, ok := d.sequences[viewer.SequenceID]
		if !ok {
			continue
		}
		cues = append(cues, d.tickViewerLocked(sequence, viewer, now)...)
	}

	sort.SliceStable(cues, func(i, j int) bool {
		return compareCue(cues[i], cues[j], cues[i].ViewerID, cues[j].ViewerID) < 0
	})
	return cues, nil
}

func (d *CutsceneSequenceDirector) tickViewerLocked(
	sequence *sequenceState,
	viewer *viewerState,
	now time.Time,
) []Cue {
	switch viewer.State {
	case StatePaused:
		// A Tick while paused still supplies a synthetic time baseline for the
		// next active interval, but never advances the paused position.
		viewer.lastTickAt = now
		viewer.pauseAnchorPending = false
		return nil
	case StatePlaying:
		if viewer.resyncNextTick {
			viewer.lastTickAt = now
			viewer.resyncNextTick = false
		} else if now.After(viewer.lastTickAt) {
			viewer.PositionMs += int(now.Sub(viewer.lastTickAt).Milliseconds())
			viewer.lastTickAt = now
		}
	default:
		return nil
	}

	var due []Cue
	for _, compiled := range sequence.cues {
		if compiled.AtMs > viewer.PositionMs {
			break
		}
		if _, delivered := viewer.delivered[compiled.uid]; delivered {
			continue
		}

		cue := compiled.Cue
		cue.ViewerID = viewer.ViewerID
		due = append(due, cue)
		viewer.delivered[compiled.uid] = struct{}{}
	}

	crossedLastCue := sequence.hasCues && viewer.PositionMs > sequence.lastCueAtMs
	reachedSequenceEnd := viewer.PositionMs >= sequence.durationMs
	if crossedLastCue || reachedSequenceEnd {
		viewer.State = StateFinished
		viewer.InputLocked = false
		viewer.pauseAnchorPending = false
		viewer.resyncNextTick = false
		delete(sequence.watcherIDs, viewer.ViewerID)
	}

	return due
}

// Skip pushes one viewer straight to the end of the sequence.
func (d *CutsceneSequenceDirector) Skip(viewerID string) (Playback, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	viewer, sequence, err := d.watchingViewerLocked(viewerID)
	if err != nil {
		return Playback{}, err
	}

	// Mandatory cues join the atomic delivery watermark in timeline order.
	// Optional cues are not handed out during a skip.
	for _, compiled := range sequence.cues {
		if !compiled.Mandatory {
			continue
		}
		viewer.delivered[compiled.uid] = struct{}{}
	}

	viewer.PositionMs = sequence.durationMs
	viewer.State = StateFinished
	viewer.InputLocked = false
	viewer.pauseAnchorPending = false
	viewer.resyncNextTick = false
	delete(sequence.watcherIDs, viewerID)

	return viewer.Playback, nil
}

// Pause freezes one viewer clock without losing its position.
func (d *CutsceneSequenceDirector) Pause(viewerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	viewer, _, err := d.watchingViewerLocked(viewerID)
	if err != nil {
		return err
	}
	if viewer.State == StatePlaying {
		viewer.State = StatePaused
		viewer.pauseAnchorPending = true
		viewer.resyncNextTick = false
	}
	return nil
}

// Resume unfreezes a paused viewer clock.
func (d *CutsceneSequenceDirector) Resume(viewerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	viewer, _, err := d.watchingViewerLocked(viewerID)
	if err != nil {
		return err
	}
	if viewer.State != StatePaused {
		return nil
	}

	needsResync := viewer.pauseAnchorPending
	viewer.State = StatePlaying
	viewer.pauseAnchorPending = false
	viewer.resyncNextTick = needsResync
	return nil
}

// Abort stops a sequence for every live viewer and releases the stage guards.
func (d *CutsceneSequenceDirector) Abort(sequenceID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrBadAbortReason
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	sequence, ok := d.sequences[sequenceID]
	if !ok {
		return ErrUnknownSequence
	}

	for _, viewer := range d.viewers {
		if viewer.SequenceID != sequenceID {
			continue
		}
		viewer.State = StateAborted
		viewer.AbortReason = reason
		viewer.InputLocked = false
		viewer.pauseAnchorPending = false
		viewer.resyncNextTick = false
	}
	sequence.watcherIDs = make(map[string]struct{})
	return nil
}

// Position reads one viewer clock.
func (d *CutsceneSequenceDirector) Position(viewerID string) (Playback, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	viewer, ok := d.viewers[viewerID]
	if !ok {
		return Playback{}, false
	}
	return viewer.Playback, true
}

// Observers lists the live viewers of one sequence, ordered by viewer id.
func (d *CutsceneSequenceDirector) Observers(sequenceID string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	sequence, ok := d.sequences[sequenceID]
	if !ok {
		return []string{}
	}

	viewers := make([]string, 0, len(sequence.watcherIDs))
	for viewerID := range sequence.watcherIDs {
		viewers = append(viewers, viewerID)
	}
	sort.Strings(viewers)
	return viewers
}

func (d *CutsceneSequenceDirector) watchingViewerLocked(viewerID string) (*viewerState, *sequenceState, error) {
	viewer, ok := d.viewers[viewerID]
	if !ok {
		return nil, nil, ErrViewerNotWatching
	}
	sequence, ok := d.sequences[viewer.SequenceID]
	if !ok {
		return nil, nil, ErrUnknownSequence
	}
	if _, watching := sequence.watcherIDs[viewerID]; !watching {
		return nil, nil, ErrViewerNotWatching
	}
	return viewer, sequence, nil
}

// Snapshot serialises the sequences, the viewer clocks and the watermarks.
func (d *CutsceneSequenceDirector) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	snapshot := snapshotData{
		Version: snapshotVersion,
		Limits:  d.limits,
		NextSeq: d.nextSeq,
	}

	sequenceIDs := make([]string, 0, len(d.sequences))
	for sequenceID := range d.sequences {
		sequenceIDs = append(sequenceIDs, sequenceID)
	}
	sort.Strings(sequenceIDs)

	for _, sequenceID := range sequenceIDs {
		sequence := d.sequences[sequenceID]
		snapshot.Sequences = append(snapshot.Sequences, snapshotSequence{
			ID:     sequence.id,
			Tracks: sequence.tracks,
		})
	}

	viewerIDs := make([]string, 0, len(d.viewers))
	for viewerID := range d.viewers {
		viewerIDs = append(viewerIDs, viewerID)
	}
	sort.Strings(viewerIDs)

	for _, viewerID := range viewerIDs {
		viewer := d.viewers[viewerID]
		delivered := make([]string, 0, len(viewer.delivered))
		for uid := range viewer.delivered {
			delivered = append(delivered, uid)
		}
		sort.Strings(delivered)

		snapshot.Viewers = append(snapshot.Viewers, snapshotViewer{
			Playback:           viewer.Playback,
			LastTickAt:         viewer.lastTickAt,
			PauseAnchorPending: viewer.pauseAnchorPending,
			ResyncNextTick:     viewer.resyncNextTick,
			Delivered:          delivered,
		})
	}

	data, err := json.Marshal(&snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal cutscene snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *CutsceneSequenceDirector) Restore(data []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}
	if snapshot.NextSeq < 0 {
		return fmt.Errorf("%w: negative playback sequence", ErrBadSnapshot)
	}

	sequences := make(map[string]*sequenceState, len(snapshot.Sequences))
	for _, record := range snapshot.Sequences {
		if _, duplicate := sequences[record.ID]; duplicate {
			return fmt.Errorf("%w: duplicate sequence %q", ErrBadSnapshot, record.ID)
		}
		sequence, err := buildSequence(record.ID, record.Tracks, d.limits)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
		sequences[record.ID] = sequence
	}

	viewers := make(map[string]*viewerState, len(snapshot.Viewers))
	nextSeq := snapshot.NextSeq
	for _, record := range snapshot.Viewers {
		if _, duplicate := viewers[record.ViewerID]; duplicate {
			return fmt.Errorf("%w: duplicate viewer %q", ErrBadSnapshot, record.ViewerID)
		}

		sequence, ok := sequences[record.SequenceID]
		if !ok {
			return fmt.Errorf("%w: viewer %q references unknown sequence", ErrBadSnapshot, record.ViewerID)
		}
		if !validPlaybackState(record.State) {
			return fmt.Errorf("%w: invalid playback state %q", ErrBadSnapshot, record.State)
		}
		if record.PositionMs < 0 {
			return fmt.Errorf("%w: negative position", ErrBadSnapshot)
		}
		if record.Seq < 1 {
			return fmt.Errorf("%w: invalid playback sequence", ErrBadSnapshot)
		}

		validUIDs := make(map[string]struct{}, len(sequence.cues))
		for _, compiled := range sequence.cues {
			validUIDs[compiled.uid] = struct{}{}
		}
		delivered := make(map[string]struct{}, len(record.Delivered))
		for _, uid := range record.Delivered {
			if _, valid := validUIDs[uid]; !valid {
				return fmt.Errorf("%w: unknown delivered cue %q", ErrBadSnapshot, uid)
			}
			delivered[uid] = struct{}{}
		}

		playback := record.Playback
		switch playback.State {
		case StatePlaying, StatePaused:
			if !playback.InputLocked || playback.CameraAnchor == "" {
				return fmt.Errorf("%w: active playback is missing stage guards", ErrBadSnapshot)
			}
			sequence.watcherIDs[playback.ViewerID] = struct{}{}
			if len(sequence.watcherIDs) > d.limits.MaxViewersPerShow {
				return fmt.Errorf("%w: viewer cap exceeded", ErrBadSnapshot)
			}
		default:
			if playback.InputLocked {
				return fmt.Errorf("%w: terminal playback retains input lock", ErrBadSnapshot)
			}
		}
		if playback.State == StateAborted && strings.TrimSpace(playback.AbortReason) == "" {
			return fmt.Errorf("%w: aborted playback lacks reason", ErrBadSnapshot)
		}
		if playback.Seq > nextSeq {
			nextSeq = playback.Seq
		}

		viewers[playback.ViewerID] = &viewerState{
			Playback:           playback,
			lastTickAt:         record.LastTickAt,
			pauseAnchorPending: record.PauseAnchorPending,
			resyncNextTick:     record.ResyncNextTick,
			delivered:          delivered,
		}
	}

	d.mu.Lock()
	d.sequences = sequences
	d.viewers = viewers
	d.nextSeq = nextSeq
	d.mu.Unlock()
	return nil
}

func validPlaybackState(state string) bool {
	switch state {
	case StatePlaying, StatePaused, StateFinished, StateAborted:
		return true
	default:
		return false
	}
}
