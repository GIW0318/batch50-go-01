package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-06"
)

func openTone(t *testing.T, p *impl.GaplessTrimPlanner, track string, rate, frames, frameSize int, priming, padding int64) impl.TrackView {
	t.Helper()
	v, err := p.Open(track, impl.EncoderInfo{
		SampleRate: rate,
		Frames:     frames,
		FrameSize:  frameSize,
		Priming:    priming,
		Padding:    padding,
	})
	if err != nil {
		t.Fatalf("open track %s: %v", track, err)
	}
	return v
}

func TestOpenReportsPlayableWindow(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	v := openTone(t, p, "tone-a", 48000, 100, 480, 1024, 512)
	if v.Track != "tone-a" {
		t.Fatalf("track = %q, want tone-a", v.Track)
	}
	if v.Encoded != 48000 {
		t.Fatalf("encoded samples = %d, want 100*480 = 48000", v.Encoded)
	}
	if v.Playable != 46464 {
		t.Fatalf("playable samples = %d, want 48000-1024-512 = 46464", v.Playable)
	}
	if v.Duration != 968*time.Millisecond {
		t.Fatalf("playable duration = %v, want 968ms", v.Duration)
	}
	if v.SampleRate != 48000 || v.Priming != 1024 || v.Padding != 512 {
		t.Fatalf("view fields = %+v, want rate 48000 priming 1024 padding 512", v)
	}
	view, ok := p.View("tone-a")
	if !ok {
		t.Fatalf("expected the opened track to be readable")
	}
	if view != v {
		t.Fatalf("View = %+v, want the same window %+v", view, v)
	}
	if tracks := p.Tracks(); len(tracks) != 1 || tracks[0] != "tone-a" {
		t.Fatalf("open tracks = %v, want exactly tone-a", tracks)
	}
}

func TestOpenRejectsInvalidEncoder(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	if _, err := p.Open("   ", impl.EncoderInfo{SampleRate: 48000, Frames: 10, FrameSize: 480}); !errors.Is(err, impl.ErrBlankTrack) {
		t.Fatalf("blank track name must fail with ErrBlankTrack, got %v", err)
	}
	bad := []impl.EncoderInfo{
		{SampleRate: 0, Frames: 10, FrameSize: 480},
		{SampleRate: 48000, Frames: 0, FrameSize: 480},
		{SampleRate: 48000, Frames: 10, FrameSize: 0},
		{SampleRate: 48000, Frames: 2, FrameSize: 10, Priming: 15, Padding: 5},
		{SampleRate: 48000, Frames: 10, FrameSize: 480, Priming: -1},
	}
	for i, info := range bad {
		if _, err := p.Open(fmt.Sprintf("bad-%d", i), info); !errors.Is(err, impl.ErrBadEncoder) {
			t.Fatalf("encoder info %+v must fail with ErrBadEncoder, got %v", info, err)
		}
	}
	if got := len(p.Tracks()); got != 0 {
		t.Fatalf("rejected tracks must not be registered, got %d open tracks", got)
	}
}

func TestOpenReplacesTrackAndDropsClips(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 10, 10)
	if _, err := p.Trim("tone-a", 0, 100*time.Millisecond); err != nil {
		t.Fatalf("first trim: %v", err)
	}
	if got := len(p.Clips()); got != 1 {
		t.Fatalf("clips after trim = %d, want 1", got)
	}
	v := openTone(t, p, "tone-a", 1000, 2000, 1, 0, 0)
	if v.Playable != 2000 {
		t.Fatalf("reopened playable samples = %d, want 2000", v.Playable)
	}
	if got := len(p.Clips()); got != 0 {
		t.Fatalf("reopening the track must drop its segments, got %d clips", got)
	}
}

func TestTrimRecordsSampleWindow(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 50, 20)
	c, err := p.Trim("tone-a", 0, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if c.Track != "tone-a" || c.From != 0 || c.To != 100*time.Millisecond {
		t.Fatalf("clip range = %+v, want tone-a 0..100ms", c)
	}
	if c.StartSample != 50 {
		t.Fatalf("clip start sample = %d, want priming 50", c.StartSample)
	}
	if c.Count != 100 {
		t.Fatalf("clip sample count = %d, want 100", c.Count)
	}
	if c.SampleRate != 1000 {
		t.Fatalf("clip sample rate = %d, want 1000", c.SampleRate)
	}
	if c.Duration != 100*time.Millisecond {
		t.Fatalf("clip duration = %v, want 100ms", c.Duration)
	}
	clips := p.Clips()
	if len(clips) != 1 || clips[0].Count != 100 || clips[0].StartSample != 50 {
		t.Fatalf("recorded clips = %+v, want the trimmed segment", clips)
	}
}

func TestTrimRejectsRangesOutsideTrack(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 10, 10)
	cases := []struct {
		from time.Duration
		to   time.Duration
	}{
		{-time.Millisecond, 10 * time.Millisecond},
		{10 * time.Millisecond, 10 * time.Millisecond},
		{20 * time.Millisecond, 10 * time.Millisecond},
		{0, 981 * time.Millisecond},
	}
	for _, c := range cases {
		if _, err := p.Trim("tone-a", c.from, c.to); !errors.Is(err, impl.ErrBadRange) {
			t.Fatalf("trim %v..%v must fail with ErrBadRange, got %v", c.from, c.to, err)
		}
	}
	if _, err := p.Trim("ghost", 0, 10*time.Millisecond); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("trim on an unknown track must fail with ErrUnknownTrack, got %v", err)
	}
	if got := len(p.Clips()); got != 0 {
		t.Fatalf("rejected ranges must not be recorded, got %d clips", got)
	}
}

func TestTrimHonoursPolicyMinimum(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 0, 0)
	if err := p.SetPaddingPolicy(impl.Policy{MinSamples: 200}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	if _, err := p.Trim("tone-a", 0, 100*time.Millisecond); !errors.Is(err, impl.ErrShortClip) {
		t.Fatalf("100 sample segment under a 200 sample minimum must fail with ErrShortClip, got %v", err)
	}
	if got := len(p.Clips()); got != 0 {
		t.Fatalf("rejected short segment must not be recorded, got %d clips", got)
	}
	c, err := p.Trim("tone-a", 0, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("trim of 300 samples must pass the minimum: %v", err)
	}
	if c.Count != 300 {
		t.Fatalf("clip sample count = %d, want 300", c.Count)
	}
}

func TestPolicyValidationKeepsEffectiveValues(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	got := p.Policy()
	if got.HeadFade != 0 || got.TailFade != 0 || got.MinSamples != 1 {
		t.Fatalf("default policy = %+v, want no fades and a one sample minimum", got)
	}
	for _, bad := range []impl.Policy{{HeadFade: -1}, {TailFade: -2}, {MinSamples: -3}} {
		if err := p.SetPaddingPolicy(bad); !errors.Is(err, impl.ErrBadPolicy) {
			t.Fatalf("policy %+v must fail with ErrBadPolicy, got %v", bad, err)
		}
	}
	got = p.Policy()
	if got.HeadFade != 0 || got.TailFade != 0 || got.MinSamples != 1 {
		t.Fatalf("rejected policies must not change the effective one, got %+v", got)
	}
	if err := p.SetPaddingPolicy(impl.Policy{HeadFade: 100, TailFade: 40, MinSamples: 500}); err != nil {
		t.Fatalf("set valid policy: %v", err)
	}
	got = p.Policy()
	if got.HeadFade != 100 || got.TailFade != 40 || got.MinSamples != 500 {
		t.Fatalf("effective policy = %+v, want 100/40/500", got)
	}
}

func TestDurationFollowsSampleRate(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	v := openTone(t, p, "tone-a", 22050, 100, 441, 1000, 500)
	if v.Encoded != 44100 {
		t.Fatalf("encoded samples = %d, want 44100", v.Encoded)
	}
	if v.Playable != 42600 {
		t.Fatalf("playable samples = %d, want 42600", v.Playable)
	}
	want := time.Duration(int64(42600) * int64(time.Second) / int64(22050))
	d, err := p.Duration("tone-a")
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if d != want {
		t.Fatalf("duration = %v, want %v", d, want)
	}
	if v.Duration != want {
		t.Fatalf("view duration = %v, want %v", v.Duration, want)
	}
	if _, err := p.Duration("ghost"); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("duration on an unknown track must fail with ErrUnknownTrack, got %v", err)
	}
}

func TestSampleAtMapsToDecodedSamples(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 50, 20)
	cases := []struct {
		pos  time.Duration
		want int64
	}{
		{0, 50},
		{250 * time.Millisecond, 300},
		{929 * time.Millisecond, 979},
	}
	for _, c := range cases {
		idx, err := p.SampleAt("tone-a", c.pos)
		if err != nil {
			t.Fatalf("sample at %v: %v", c.pos, err)
		}
		if idx != c.want {
			t.Fatalf("sample at %v = %d, want %d", c.pos, idx, c.want)
		}
	}
	for ms := 100; ms <= 103; ms++ {
		idx, err := p.SampleAt("tone-a", time.Duration(ms)*time.Millisecond)
		if err != nil {
			t.Fatalf("sample at %dms: %v", ms, err)
		}
		if want := int64(50 + ms); idx != want {
			t.Fatalf("sample at %dms = %d, want %d", ms, idx, want)
		}
	}
}

func TestSampleAtRejectsOutOfRange(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 50, 20)
	for _, pos := range []time.Duration{-time.Millisecond, 930 * time.Millisecond, 2 * time.Second} {
		if _, err := p.SampleAt("tone-a", pos); !errors.Is(err, impl.ErrBadRange) {
			t.Fatalf("sample at %v must fail with ErrBadRange, got %v", pos, err)
		}
	}
	if _, err := p.SampleAt("ghost", 0); !errors.Is(err, impl.ErrUnknownTrack) {
		t.Fatalf("sample at an unknown track must fail with ErrUnknownTrack, got %v", err)
	}
}

func TestJoinRejectsEmptyClipList(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	if _, err := p.Join(nil); !errors.Is(err, impl.ErrNoClips) {
		t.Fatalf("join of nil must fail with ErrNoClips, got %v", err)
	}
	if _, err := p.Join([]impl.Clip{}); !errors.Is(err, impl.ErrNoClips) {
		t.Fatalf("join of an empty list must fail with ErrNoClips, got %v", err)
	}
}

func TestJoinSingleClipKeepsWindow(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 50, 20)
	c, err := p.Trim("tone-a", 0, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	j, err := p.Join([]impl.Clip{c})
	if err != nil {
		t.Fatalf("join single clip: %v", err)
	}
	if j.Track != c.Track || j.StartSample != c.StartSample || j.Count != c.Count || j.SampleRate != c.SampleRate {
		t.Fatalf("joined clip = %+v, want the single segment window %+v", j, c)
	}
	if got := len(p.Clips()); got != 1 {
		t.Fatalf("join must not consume recorded segments, got %d clips", got)
	}
}

func TestConcurrentTrimsAreSerialized(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 0, 0)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := p.Trim("tone-a", time.Duration(i*10)*time.Millisecond, time.Duration(i*10+10)*time.Millisecond)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent trim failed: %v", err)
		}
	}
	clips := p.Clips()
	if len(clips) != 10 {
		t.Fatalf("recorded clips = %d, want all 10 concurrent segments", len(clips))
	}
	seen := make(map[time.Duration]bool, len(clips))
	for _, c := range clips {
		if c.Count != 10 {
			t.Fatalf("clip %v..%v has %d samples, want 10", c.From, c.To, c.Count)
		}
		if seen[c.From] {
			t.Fatalf("duplicate segment starting at %v", c.From)
		}
		seen[c.From] = true
	}
}

func TestSnapshotRestoreKeepsTrackState(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 50, 20)
	openTone(t, p, "tone-b", 2000, 2000, 1, 100, 100)
	if _, err := p.Trim("tone-a", 0, 100*time.Millisecond); err != nil {
		t.Fatalf("trim tone-a: %v", err)
	}
	if _, err := p.Trim("tone-b", 0, 100*time.Millisecond); err != nil {
		t.Fatalf("trim tone-b: %v", err)
	}
	if err := p.SetPaddingPolicy(impl.Policy{HeadFade: 10, MinSamples: 20}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	before, err := p.Duration("tone-a")
	if err != nil {
		t.Fatalf("duration before snapshot: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewGaplessTrimPlanner()
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(r.Clips()); got != 2 {
		t.Fatalf("restored clips = %d, want 2", got)
	}
	if d, err := r.Duration("tone-a"); err != nil || d != before {
		t.Fatalf("restored duration = %v (err %v), want %v", d, err, before)
	}
	if idx, err := r.SampleAt("tone-a", 0); err != nil || idx != 50 {
		t.Fatalf("restored sample at 0 = %d (err %v), want 50", idx, err)
	}
	if got := r.Policy(); got.HeadFade != 10 || got.MinSamples != 20 || got.TailFade != 0 {
		t.Fatalf("restored policy = %+v, want head fade 10 minimum 20", got)
	}
	if _, err := r.Trim("tone-b", 0, 50*time.Millisecond); err != nil {
		t.Fatalf("trim after restore: %v", err)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	p := impl.NewGaplessTrimPlanner()
	openTone(t, p, "tone-a", 1000, 1000, 1, 50, 20)
	if _, err := p.Trim("tone-a", 0, 100*time.Millisecond); err != nil {
		t.Fatalf("trim: %v", err)
	}
	if err := p.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(p.Clips()); got != 1 {
		t.Fatalf("rejected restore must keep recorded segments, got %d clips", got)
	}
	if idx, err := p.SampleAt("tone-a", 0); err != nil || idx != 50 {
		t.Fatalf("live track must keep working after a rejected restore, got %d (err %v)", idx, err)
	}
	if _, ok := p.View("tone-a"); !ok {
		t.Fatalf("live track view must survive a rejected restore")
	}
}
