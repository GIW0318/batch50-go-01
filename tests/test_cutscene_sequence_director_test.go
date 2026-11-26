package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-11"
)

func stageLimits() impl.Limits {
	return impl.Limits{MaxTracks: 4, MaxCues: 8, MaxViewersPerShow: 3}
}

func cue(id, trackID string, atMs, priority int, mandatory bool) impl.Cue {
	return impl.Cue{
		ID:        id,
		TrackID:   trackID,
		Kind:      impl.TrackEvent,
		AtMs:      atMs,
		Priority:  priority,
		Payload:   id + "-payload",
		Mandatory: mandatory,
	}
}

// openingTracks returns a 500ms opening: camera cues at 100ms and 200ms plus a
// dialogue line at 100ms that outranks the camera cue.
func openingTracks() []impl.Track {
	return []impl.Track{
		{
			ID:         "cam-main",
			Kind:       impl.TrackCamera,
			DurationMs: 500,
			Cues: []impl.Cue{
				{
					ID: "cam-close", TrackID: "cam-main", Kind: impl.TrackCamera,
					AtMs: 100, Priority: 5, Payload: "close-up", Mandatory: false,
				},
				{
					ID: "cam-wide", TrackID: "cam-main", Kind: impl.TrackCamera,
					AtMs: 200, Priority: 1, Payload: "wide-shot", Mandatory: false,
				},
			},
		},
		{
			ID:         "vo-hero",
			Kind:       impl.TrackDialogue,
			DurationMs: 400,
			Cues: []impl.Cue{
				{
					ID: "vo-line", TrackID: "vo-hero", Kind: impl.TrackDialogue,
					AtMs: 100, Priority: 1, Payload: "hold the gate", Mandatory: true,
				},
			},
		},
	}
}

func TestLoadRejectsBlankSequenceID(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("   ", openingTracks()); !errors.Is(err, impl.ErrBlankSequenceID) {
		t.Fatalf("blank sequence id must be rejected with ErrBlankSequenceID, got %v", err)
	}
	if err := d.Load("intro", nil); !errors.Is(err, impl.ErrNoTracks) {
		t.Fatalf("a sequence without tracks must be rejected with ErrNoTracks, got %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err == nil {
		t.Fatalf("a rejected sequence must not be playable")
	}
}

func TestLoadRejectsUnknownTrackKind(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	tracks := []impl.Track{{
		ID:         "hud-banner",
		Kind:       "hud",
		DurationMs: 300,
		Cues:       []impl.Cue{cue("hud-1", "hud-banner", 50, 1, false)},
	}}
	if err := d.Load("intro", tracks); !errors.Is(err, impl.ErrBadTrackKind) {
		t.Fatalf("unknown track kind must be rejected with ErrBadTrackKind, got %v", err)
	}
	dup := []impl.Track{
		{ID: "cam", Kind: impl.TrackCamera, DurationMs: 300},
		{ID: "cam", Kind: impl.TrackAudio, DurationMs: 300},
	}
	if err := d.Load("intro", dup); !errors.Is(err, impl.ErrDuplicateTrack) {
		t.Fatalf("repeated track id must be rejected with ErrDuplicateTrack, got %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); !errors.Is(err, impl.ErrUnknownSequence) {
		t.Fatalf("rejected sequences must stay unloadable, got %v", err)
	}
}

func TestLoadRejectsCueOutsideTrack(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	tracks := []impl.Track{{
		ID:         "cam-main",
		Kind:       impl.TrackCamera,
		DurationMs: 500,
		Cues: []impl.Cue{
			cue("late", "cam-main", 501, 1, false),
		},
	}}
	if err := d.Load("intro", tracks); !errors.Is(err, impl.ErrBadCue) {
		t.Fatalf("cue after the track end must be rejected with ErrBadCue, got %v", err)
	}
	tracks[0].Cues = []impl.Cue{cue("early", "cam-main", -1, 1, false)}
	if err := d.Load("intro", tracks); !errors.Is(err, impl.ErrBadCue) {
		t.Fatalf("cue before zero must be rejected with ErrBadCue, got %v", err)
	}
	tracks[0].Cues = []impl.Cue{cue("orphan", "cam-missing", 10, 1, false)}
	if err := d.Load("intro", tracks); !errors.Is(err, impl.ErrBadCue) {
		t.Fatalf("cue pointing at an unknown track must be rejected with ErrBadCue, got %v", err)
	}
}

func TestLoadEnforcesCapacityLimits(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(impl.Limits{MaxTracks: 2, MaxCues: 3, MaxViewersPerShow: 2})
	threeTracks := []impl.Track{
		{ID: "a", Kind: impl.TrackCamera, DurationMs: 100},
		{ID: "b", Kind: impl.TrackAudio, DurationMs: 100},
		{ID: "c", Kind: impl.TrackEvent, DurationMs: 100},
	}
	if err := d.Load("wide", threeTracks); !errors.Is(err, impl.ErrTooManyTracks) {
		t.Fatalf("three tracks against a cap of two must fail with ErrTooManyTracks, got %v", err)
	}
	tooManyCues := []impl.Track{
		{ID: "a", Kind: impl.TrackCamera, DurationMs: 200, Cues: []impl.Cue{
			cue("c1", "a", 10, 1, false),
			cue("c2", "a", 20, 1, false),
		}},
		{ID: "b", Kind: impl.TrackAudio, DurationMs: 200, Cues: []impl.Cue{
			cue("c3", "b", 10, 1, false),
			cue("c4", "b", 20, 1, false),
		}},
	}
	if err := d.Load("dense", tooManyCues); !errors.Is(err, impl.ErrTooManyCues) {
		t.Fatalf("four cues against a cap of three must fail with ErrTooManyCues, got %v", err)
	}
	if _, err := d.Play("wide", "viewer-1", time.Unix(0, 0)); !errors.Is(err, impl.ErrUnknownSequence) {
		t.Fatalf("over-cap sequences must not be stored, got %v", err)
	}
	if _, err := d.Play("dense", "viewer-1", time.Unix(0, 0)); !errors.Is(err, impl.ErrUnknownSequence) {
		t.Fatalf("over-cap sequences must not be stored, got %v", err)
	}
	if err := d.Load("ok", openingTracks()); err != nil {
		t.Fatalf("a sequence inside the caps must load, got %v", err)
	}
	if _, err := d.Play("ok", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("a loaded sequence must be playable, got %v", err)
	}
}

func TestPlayReturnsFreshPlayback(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	pb, err := d.Play("intro", "viewer-1", time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if pb.SequenceID != "intro" || pb.ViewerID != "viewer-1" {
		t.Fatalf("playback identity = %q/%q, want intro/viewer-1", pb.SequenceID, pb.ViewerID)
	}
	if pb.State != impl.StatePlaying {
		t.Fatalf("playback state = %q, want %q", pb.State, impl.StatePlaying)
	}
	if pb.PositionMs != 0 {
		t.Fatalf("fresh playback position = %d, want 0", pb.PositionMs)
	}
	if pb.Seq <= 0 {
		t.Fatalf("playback sequence = %d, want a positive number", pb.Seq)
	}
	if !pb.InputLocked {
		t.Fatalf("a running cutscene must hold the input lock")
	}
	if pb.CameraAnchor == "" {
		t.Fatalf("a running cutscene must record the camera anchor it took over")
	}
	current, ok := d.Position("viewer-1")
	if !ok {
		t.Fatalf("the viewer clock must be readable after play")
	}
	if current.Seq != pb.Seq {
		t.Fatalf("Position seq = %d, want the playback seq %d", current.Seq, pb.Seq)
	}
	watchers := d.Observers("intro")
	if len(watchers) != 1 || watchers[0] != "viewer-1" {
		t.Fatalf("observers = %v, want exactly viewer-1", watchers)
	}
}

func TestPlayRejectsUnknownSequence(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); !errors.Is(err, impl.ErrUnknownSequence) {
		t.Fatalf("playing an unknown sequence must fail with ErrUnknownSequence, got %v", err)
	}
	if _, ok := d.Position("viewer-1"); ok {
		t.Fatalf("a failed play must not leave a viewer clock behind")
	}
}

func TestPlayEnforcesViewerLimit(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(impl.Limits{MaxTracks: 4, MaxCues: 8, MaxViewersPerShow: 2})
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	for _, viewer := range []string{"viewer-1", "viewer-2"} {
		if _, err := d.Play("intro", viewer, time.Unix(0, 0)); err != nil {
			t.Fatalf("play for %s: %v", viewer, err)
		}
	}
	if _, err := d.Play("intro", "viewer-3", time.Unix(0, 0)); !errors.Is(err, impl.ErrViewerLimit) {
		t.Fatalf("third concurrent viewer must hit ErrViewerLimit, got %v", err)
	}
	if got := len(d.Observers("intro")); got != 2 {
		t.Fatalf("observers = %d, want the configured cap 2", got)
	}
}

func TestTickOrdersCuesByTimeThenPriority(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	delivered, err := d.Tick(time.Unix(0, 250*int64(time.Millisecond)))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(delivered) != 3 {
		t.Fatalf("delivered %d cues, want 3 due within 250ms: %+v", len(delivered), delivered)
	}
	wantIDs := []string{"cam-close", "vo-line", "cam-wide"}
	for i, want := range wantIDs {
		if delivered[i].ID != want {
			t.Fatalf("delivery order = %v, want %v", cueIDs(delivered), wantIDs)
		}
	}
	if delivered[2].AtMs != 200 {
		t.Fatalf("third delivery time = %d, want 200", delivered[2].AtMs)
	}
	for _, c := range delivered {
		if c.ViewerID != "viewer-1" {
			t.Fatalf("cue %q carries viewer %q, want viewer-1", c.ID, c.ViewerID)
		}
	}
}

func cueIDs(cues []impl.Cue) []string {
	out := make([]string, len(cues))
	for i, c := range cues {
		out[i] = c.ID
	}
	return out
}

func TestTickDoesNotRepeatDeliveredCues(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if _, err := d.Tick(time.Unix(0, 250*int64(time.Millisecond))); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	again, err := d.Tick(time.Unix(0, 300*int64(time.Millisecond)))
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second tick redelivered %v, want nothing before the next cue", cueIDs(again))
	}
	tail, err := d.Tick(time.Unix(0, 520*int64(time.Millisecond)))
	if err != nil {
		t.Fatalf("tail tick: %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("tick past the last cue redelivered %v, want nothing", cueIDs(tail))
	}
	if pb, _ := d.Position("viewer-1"); pb.State != impl.StateFinished {
		t.Fatalf("position state after the last cue = %q, want %q", pb.State, impl.StateFinished)
	}
}

func TestTickAdvancesPositionByElapsedTime(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if _, err := d.Tick(time.Unix(0, 120*int64(time.Millisecond))); err != nil {
		t.Fatalf("tick: %v", err)
	}
	pb, ok := d.Position("viewer-1")
	if !ok {
		t.Fatalf("the viewer clock must stay readable")
	}
	if pb.PositionMs != 120 {
		t.Fatalf("position after a 120ms tick = %d, want 120", pb.PositionMs)
	}
	if _, err := d.Tick(time.Unix(0, 300*int64(time.Millisecond))); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	pb, _ = d.Position("viewer-1")
	if pb.PositionMs != 300 {
		t.Fatalf("position after a 300ms tick = %d, want 300", pb.PositionMs)
	}
}

func TestSkipJumpsToEndAndFinishes(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	skipped, err := d.Skip("viewer-1")
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if skipped.State != impl.StateFinished {
		t.Fatalf("state after skip = %q, want %q", skipped.State, impl.StateFinished)
	}
	if skipped.PositionMs != 500 {
		t.Fatalf("position after skip = %d, want the sequence length 500", skipped.PositionMs)
	}
	if skipped.InputLocked {
		t.Fatalf("skip must hand the input lock back")
	}
	after, err := d.Tick(time.Unix(0, 600*int64(time.Millisecond)))
	if err != nil {
		t.Fatalf("tick after skip: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("tick after skip delivered %v, want nothing", cueIDs(after))
	}
}

func TestSkipRejectsViewerNotWatching(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Skip("viewer-9"); !errors.Is(err, impl.ErrViewerNotWatching) {
		t.Fatalf("skip without a clock must fail with ErrViewerNotWatching, got %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if _, err := d.Skip("viewer-2"); !errors.Is(err, impl.ErrViewerNotWatching) {
		t.Fatalf("skip for another viewer must fail with ErrViewerNotWatching, got %v", err)
	}
	if pb, _ := d.Position("viewer-1"); pb.State != impl.StatePlaying {
		t.Fatalf("a foreign skip must leave the running clock playing, got %q", pb.State)
	}
}

func TestPauseAndResumeControlAdvance(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if _, err := d.Tick(time.Unix(0, 100*int64(time.Millisecond))); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := d.Pause("viewer-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := d.Tick(time.Unix(0, 400*int64(time.Millisecond))); err != nil {
		t.Fatalf("tick while paused: %v", err)
	}
	paused, _ := d.Position("viewer-1")
	if paused.State != impl.StatePaused {
		t.Fatalf("state while paused = %q, want %q", paused.State, impl.StatePaused)
	}
	if paused.PositionMs != 100 {
		t.Fatalf("paused position = %d, want it frozen at 100", paused.PositionMs)
	}
	if err := d.Resume("viewer-1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := d.Tick(time.Unix(0, 500*int64(time.Millisecond))); err != nil {
		t.Fatalf("tick after resume: %v", err)
	}
	resumed, _ := d.Position("viewer-1")
	if resumed.PositionMs != 200 {
		t.Fatalf("position after resuming and a 100ms tick = %d, want 200", resumed.PositionMs)
	}
	if resumed.State != impl.StatePlaying {
		t.Fatalf("state after resume = %q, want %q", resumed.State, impl.StatePlaying)
	}
}

func TestAbortStopsSequence(t *testing.T) {
	d := impl.NewCutsceneSequenceDirector(stageLimits())
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if err := d.Abort("intro", "   "); !errors.Is(err, impl.ErrBadAbortReason) {
		t.Fatalf("abort without a reason must fail with ErrBadAbortReason, got %v", err)
	}
	if err := d.Abort("intro-404", "battle started"); !errors.Is(err, impl.ErrUnknownSequence) {
		t.Fatalf("abort of an unknown sequence must fail with ErrUnknownSequence, got %v", err)
	}
	if err := d.Abort("intro", "battle started"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	pb, ok := d.Position("viewer-1")
	if !ok {
		t.Fatalf("the aborted viewer clock must stay readable for the report")
	}
	if pb.State != impl.StateAborted {
		t.Fatalf("state after abort = %q, want %q", pb.State, impl.StateAborted)
	}
	if pb.AbortReason != "battle started" {
		t.Fatalf("abort reason = %q, want the operator note", pb.AbortReason)
	}
	after, err := d.Tick(time.Unix(0, 260*int64(time.Millisecond)))
	if err != nil {
		t.Fatalf("tick after abort: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("tick after abort delivered %v, want nothing", cueIDs(after))
	}
}

func TestSnapshotRestoreKeepsClocks(t *testing.T) {
	limits := stageLimits()
	d := impl.NewCutsceneSequenceDirector(limits)
	if err := d.Load("intro", openingTracks()); err != nil {
		t.Fatalf("load sequence: %v", err)
	}
	if _, err := d.Play("intro", "viewer-1", time.Unix(0, 0)); err != nil {
		t.Fatalf("play sequence: %v", err)
	}
	if _, err := d.Tick(time.Unix(0, 120*int64(time.Millisecond))); err != nil {
		t.Fatalf("tick before the snapshot: %v", err)
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewCutsceneSequenceDirector(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	pb, ok := restored.Position("viewer-1")
	if !ok {
		t.Fatalf("restored director lost the viewer clock")
	}
	if pb.PositionMs != 120 || pb.State != impl.StatePlaying {
		t.Fatalf("restored clock = %d/%q, want 120/playing", pb.PositionMs, pb.State)
	}
	delivered, err := restored.Tick(time.Unix(0, 200*int64(time.Millisecond)))
	if err != nil {
		t.Fatalf("tick after restore: %v", err)
	}
	if len(delivered) != 1 || delivered[0].ID != "cam-wide" {
		t.Fatalf("tick after restore delivered %v, want only the 200ms cue", cueIDs(delivered))
	}
	if delivered[0].ViewerID != "viewer-1" {
		t.Fatalf("restored delivery carries viewer %q, want viewer-1", delivered[0].ViewerID)
	}
	if err := restored.Restore([]byte("not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if pb, _ := restored.Position("viewer-1"); pb.PositionMs != 200 {
		t.Fatalf("a rejected restore must leave the live clock at 200, got %d", pb.PositionMs)
	}
}
