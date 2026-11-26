// Package task plans gapless playback windows for codec delayed audio tracks:
// it removes encoder priming and padding samples, maps playhead positions onto
// real decoded samples and joins trimmed segments into one queue segment.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMinSamples int64 = 1
	snapshotVersion   int   = 1
)

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
	mu     sync.Mutex
	policy Policy
	tracks map[string]*trackState
	clips  []Clip
}

type trackState struct {
	info EncoderInfo
	view TrackView
}

type snapshotEnvelope struct {
	Version int             `json:"version"`
	Policy  Policy          `json:"policy"`
	Tracks  []snapshotTrack `json:"tracks"`
	Clips   []Clip          `json:"clips"`
}

type snapshotTrack struct {
	Name string      `json:"name"`
	Info EncoderInfo `json:"info"`
}

// NewGaplessTrimPlanner builds a planner without any open track.
func NewGaplessTrimPlanner() *GaplessTrimPlanner {
	return &GaplessTrimPlanner{
		policy: Policy{MinSamples: defaultMinSamples},
		tracks: make(map[string]*trackState),
	}
}

// Open registers a track or replaces its encoder info, dropping old segments.
func (p *GaplessTrimPlanner) Open(track string, info EncoderInfo) (TrackView, error) {
	if isBlankTrack(track) {
		return TrackView{}, ErrBlankTrack
	}

	view, err := makeTrackView(track, info)
	if err != nil {
		return TrackView{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	p.tracks[track] = &trackState{info: info, view: view}
	p.dropTrackClipsLocked(track)

	return view, nil
}

// Close removes a track together with its trimmed segments.
func (p *GaplessTrimPlanner) Close(track string) error {
	if isBlankTrack(track) {
		return ErrBlankTrack
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	if _, ok := p.tracks[track]; !ok {
		return ErrUnknownTrack
	}

	delete(p.tracks, track)
	p.dropTrackClipsLocked(track)
	return nil
}

// Trim cuts one segment out of the playable timeline of a track.
func (p *GaplessTrimPlanner) Trim(track string, from, to time.Duration) (Clip, error) {
	if isBlankTrack(track) {
		return Clip{}, ErrBlankTrack
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	state, ok := p.tracks[track]
	if !ok {
		return Clip{}, ErrUnknownTrack
	}

	if from < 0 || to <= from || to > state.view.Duration {
		return Clip{}, ErrBadRange
	}

	rate := state.view.SampleRate
	fromSamples := samplesForDuration(from, rate)
	count := samplesForDuration(to-from, rate)
	policy := p.policy

	if policy.HeadFade > count {
		return Clip{}, ErrShortClip
	}
	count -= policy.HeadFade
	if policy.TailFade > count {
		return Clip{}, ErrShortClip
	}
	count -= policy.TailFade

	minimum := policy.MinSamples
	if minimum == 0 {
		minimum = defaultMinSamples
	}
	if count < minimum {
		return Clip{}, ErrShortClip
	}

	clip := Clip{
		Track:       track,
		From:        from,
		To:          to,
		StartSample: state.view.Priming + fromSamples + policy.HeadFade,
		Count:       count,
		SampleRate:  rate,
		Duration:    durationForSamples(count, rate),
	}

	p.clips = append(p.clips, clip)
	return clip, nil
}

// Join concatenates trimmed segments into one queue segment.
func (p *GaplessTrimPlanner) Join(clips []Clip) (Clip, error) {
	if len(clips) == 0 {
		return Clip{}, ErrNoClips
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	rate := clips[0].SampleRate
	if rate <= 0 {
		return Clip{}, ErrRateMismatch
	}

	count := clips[0].Count
	for i := 1; i < len(clips); i++ {
		if clips[i].SampleRate != rate {
			return Clip{}, ErrRateMismatch
		}
		count += clips[i].Count - 1
	}

	joined := clips[0]
	joined.Count = count
	joined.SampleRate = rate
	joined.Duration = durationForSamples(count, rate)
	return joined, nil
}

// SampleAt maps a playhead position onto a real decoded sample index.
func (p *GaplessTrimPlanner) SampleAt(track string, pos time.Duration) (int64, error) {
	if isBlankTrack(track) {
		return 0, ErrBlankTrack
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	state, ok := p.tracks[track]
	if !ok {
		return 0, ErrUnknownTrack
	}
	if pos < 0 || pos >= state.view.Duration {
		return 0, ErrBadRange
	}

	return state.view.Priming + samplesForDuration(pos, state.view.SampleRate), nil
}

// Duration reports the playable duration of one track.
func (p *GaplessTrimPlanner) Duration(track string) (time.Duration, error) {
	if isBlankTrack(track) {
		return 0, ErrBlankTrack
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	state, ok := p.tracks[track]
	if !ok {
		return 0, ErrUnknownTrack
	}
	return state.view.Duration, nil
}

// SetPaddingPolicy replaces the edge policy after validating it.
func (p *GaplessTrimPlanner) SetPaddingPolicy(policy Policy) error {
	if policy.HeadFade < 0 || policy.TailFade < 0 || policy.MinSamples < 0 {
		return ErrBadPolicy
	}
	if policy.MinSamples == 0 {
		policy.MinSamples = defaultMinSamples
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	p.policy = policy
	return nil
}

// Policy returns the effective edge policy.
func (p *GaplessTrimPlanner) Policy() Policy {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	return p.policy
}

// View reads one track view.
func (p *GaplessTrimPlanner) View(track string) (TrackView, bool) {
	if isBlankTrack(track) {
		return TrackView{}, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	state, ok := p.tracks[track]
	if !ok {
		return TrackView{}, false
	}
	return state.view, true
}

// Tracks lists the open track names in ascending order.
func (p *GaplessTrimPlanner) Tracks() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()
	tracks := make([]string, 0, len(p.tracks))
	for name := range p.tracks {
		tracks = append(tracks, name)
	}
	sort.Strings(tracks)
	return tracks
}

// Clips returns a copy of the trimmed segments in trimming order.
func (p *GaplessTrimPlanner) Clips() []Clip {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]Clip(nil), p.clips...)
}

// Snapshot serialises tracks, segments and the active policy.
func (p *GaplessTrimPlanner) Snapshot() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.initLocked()

	tracks := make([]snapshotTrack, 0, len(p.tracks))
	for name, state := range p.tracks {
		tracks = append(tracks, snapshotTrack{Name: name, Info: state.info})
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].Name < tracks[j].Name })

	envelope := snapshotEnvelope{
		Version: snapshotVersion,
		Policy:  p.policy,
		Tracks:  tracks,
		Clips:   append([]Clip(nil), p.clips...),
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode planner snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *GaplessTrimPlanner) Restore(data []byte) error {
	envelope, tracks, clips, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.policy = envelope.Policy
	p.tracks = tracks
	p.clips = clips
	return nil
}

func (p *GaplessTrimPlanner) initLocked() {
	if p.tracks == nil {
		p.tracks = make(map[string]*trackState)
	}
	if p.policy.MinSamples == 0 {
		p.policy.MinSamples = defaultMinSamples
	}
}

func (p *GaplessTrimPlanner) dropTrackClipsLocked(track string) {
	if len(p.clips) == 0 {
		return
	}

	kept := make([]Clip, 0, len(p.clips))
	for _, clip := range p.clips {
		if clip.Track != track {
			kept = append(kept, clip)
		}
	}
	p.clips = kept
}

func isBlankTrack(track string) bool {
	return strings.TrimSpace(track) == ""
}

func makeTrackView(track string, info EncoderInfo) (TrackView, error) {
	if info.SampleRate <= 0 || info.Frames <= 0 || info.FrameSize <= 0 {
		return TrackView{}, ErrBadEncoder
	}

	encoded := int64(info.Frames) * int64(info.FrameSize)
	if encoded <= 0 {
		return TrackView{}, ErrBadEncoder
	}
	if info.Priming < 0 || info.Padding < 0 {
		return TrackView{}, ErrBadEncoder
	}
	if info.Priming >= encoded || info.Padding >= encoded-info.Priming {
		return TrackView{}, ErrBadEncoder
	}

	playable := encoded - info.Priming - info.Padding
	return TrackView{
		Track:      track,
		SampleRate: info.SampleRate,
		Priming:    info.Priming,
		Padding:    info.Padding,
		Encoded:    encoded,
		Playable:   playable,
		Duration:   durationForSamples(playable, info.SampleRate),
	}, nil
}

func samplesForDuration(value time.Duration, sampleRate int) int64 {
	if value <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(value) * int64(sampleRate) / int64(time.Second)
}

func durationForSamples(samples int64, sampleRate int) time.Duration {
	if samples <= 0 || sampleRate <= 0 {
		return 0
	}
	return time.Duration(samples * int64(time.Second) / int64(sampleRate))
}

func decodeSnapshot(data []byte) (snapshotEnvelope, map[string]*trackState, []Clip, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var envelope snapshotEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
	}

	if envelope.Version != snapshotVersion {
		return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
	}
	if envelope.Policy.HeadFade < 0 || envelope.Policy.TailFade < 0 || envelope.Policy.MinSamples < 0 {
		return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
	}
	if envelope.Policy.MinSamples == 0 {
		envelope.Policy.MinSamples = defaultMinSamples
	}

	tracks := make(map[string]*trackState, len(envelope.Tracks))
	for _, saved := range envelope.Tracks {
		view, err := makeTrackView(saved.Name, saved.Info)
		if err != nil {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		if _, exists := tracks[saved.Name]; exists {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		tracks[saved.Name] = &trackState{info: saved.Info, view: view}
	}

	clips := make([]Clip, 0, len(envelope.Clips))
	for _, clip := range envelope.Clips {
		state, ok := tracks[clip.Track]
		if !ok {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		if clip.SampleRate != state.view.SampleRate || clip.Count < 0 {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		if clip.From < 0 || clip.To <= clip.From || clip.To > state.view.Duration {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		playableEnd := state.view.Priming + state.view.Playable
		if clip.StartSample < state.view.Priming ||
			clip.StartSample > playableEnd ||
			clip.Count > playableEnd-clip.StartSample {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		if clip.Duration != durationForSamples(clip.Count, clip.SampleRate) {
			return snapshotEnvelope{}, nil, nil, ErrBadSnapshot
		}
		clips = append(clips, clip)
	}
	envelope.Clips = clips

	return envelope, tracks, clips, nil
}
