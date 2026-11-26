// Package task plans loop points for instrument samples: it scores tail versus
// head similarity, snaps the chosen point onto a zero crossing, sizes the
// crossfade window and applies it to the PCM data.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures the planner. Zero values fall back to 44100 Hz, a 32
// sample analysis window, a 64 sample minimum loop, 256 candidates and a 4096
// sample crossfade cap.
type Options struct {
	SampleRate    int
	Window        int
	MinLoop       int
	MaxCandidates int
	MaxCrossfade  int
}

// Candidate is one scored loop start position.
type Candidate struct {
	Index        int
	Score        float64
	ZeroCrossing bool
}

// LoopPoint is the selected loop of one sample.
type LoopPoint struct {
	Sample      string
	Start       int
	End         int
	Length      int
	Crossfade   int
	Degraded    bool
	ZeroAligned bool
}

// Errors reported by the planner.
var (
	ErrBadSample     = errors.New("sample key is required")
	ErrShortPCM      = errors.New("sample is too short to analyse")
	ErrUnknownSample = errors.New("sample was not analysed")
	ErrNoCandidate   = errors.New("no loop candidate found")
	ErrShortLoop     = errors.New("loop is shorter than the minimum")
	ErrBadLoop       = errors.New("loop point is invalid")
	ErrBadCrossfade  = errors.New("crossfade length is invalid")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

const (
	defaultSampleRate    = 44100
	defaultWindow        = 32
	defaultMinLoop       = 64
	defaultMaxCandidates = 256
	defaultMaxCrossfade  = 4096

	snapshotFormat  = "gsb.sample-loop-planner/v1"
	snapshotVersion = 1
)

type sampleState struct {
	pcm              []float32
	candidates       []Candidate
	loop             LoopPoint
	hasLoop          bool
	crossfade        time.Duration
	crossfadeSamples int
}

// LoopPointPlanner owns the analysed PCM, the candidate lists, the selected
// loops and the crossfade settings.
type LoopPointPlanner struct {
	mu      sync.RWMutex
	opts    Options
	samples map[string]*sampleState
}

type snapshotData struct {
	Format  string           `json:"format"`
	Version int              `json:"version"`
	Options Options          `json:"options"`
	Samples []snapshotSample `json:"samples"`
}

type snapshotSample struct {
	Key              string        `json:"key"`
	PCM              []float32     `json:"pcm"`
	Candidates       []Candidate   `json:"candidates"`
	Crossfade        time.Duration `json:"crossfade"`
	CrossfadeSamples int           `json:"crossfade_samples"`
	Loop             *LoopPoint    `json:"loop"`
}

// NewLoopPointPlanner builds a planner for the given options.
func NewLoopPointPlanner(opts Options) *LoopPointPlanner {
	if opts.SampleRate <= 0 {
		opts.SampleRate = defaultSampleRate
	}
	if opts.Window <= 0 {
		opts.Window = defaultWindow
	}
	if opts.MinLoop <= 0 {
		opts.MinLoop = defaultMinLoop
	}
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = defaultMaxCandidates
	}
	if opts.MaxCrossfade <= 0 {
		opts.MaxCrossfade = defaultMaxCrossfade
	}

	return &LoopPointPlanner{
		opts:    opts,
		samples: make(map[string]*sampleState),
	}
}

// Analyze scores loop candidates of one sample PCM buffer.
func (p *LoopPointPlanner) Analyze(sample string, pcm []float32) ([]Candidate, error) {
	if blankSample(sample) {
		return nil, ErrBadSample
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(pcm) < 2*p.opts.Window+1 {
		return nil, ErrShortPCM
	}

	data := cloneFloat32(pcm)
	candidates := scoreCandidates(data, p.opts.Window, p.opts.MaxCandidates)

	state := p.samples[sample]
	if state == nil {
		state = &sampleState{}
		p.samples[sample] = state
	}
	state.pcm = data
	state.candidates = candidates
	// A re-analysis refreshes candidates and PCM, but deliberately leaves an
	// explicitly selected loop in place until Select is called again.

	return cloneCandidates(candidates), nil
}

// Candidates returns a copy of the candidates of one sample.
func (p *LoopPointPlanner) Candidates(sample string) []Candidate {
	p.mu.RLock()
	defer p.mu.RUnlock()

	state, ok := p.samples[sample]
	if !ok {
		return nil
	}
	return cloneCandidates(state.candidates)
}

// Select snaps the candidate nearest to at onto a zero crossing.
func (p *LoopPointPlanner) Select(sample string, at int) (LoopPoint, error) {
	if blankSample(sample) {
		return LoopPoint{}, ErrBadSample
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	state, ok := p.samples[sample]
	if !ok {
		return LoopPoint{}, ErrUnknownSample
	}
	if len(state.candidates) == 0 {
		return LoopPoint{}, ErrNoCandidate
	}

	target := nearestCandidateIndex(state.candidates, at)
	start, zeroAligned := nearestZeroCrossing(state.pcm, target)
	length := len(state.pcm) - start

	// A non-zero target can lie inside a constant plateau between two sign
	// crossings. Prefer a zero crossing that does not shorten the loop below
	// the configured minimum. An exact zero target is already snapped and is
	// therefore checked directly afterwards.
	if length < p.opts.MinLoop && !isZeroCrossing(state.pcm, target) {
		maxStart := len(state.pcm) - p.opts.MinLoop
		if feasible, found := nearestZeroCrossingAtOrBefore(state.pcm, target, maxStart); found {
			start = feasible
			zeroAligned = true
			length = len(state.pcm) - start
		}
	}

	if length < p.opts.MinLoop {
		return LoopPoint{}, ErrShortLoop
	}

	loop := normalizeLoopCrossfade(LoopPoint{
		Sample:      sample,
		Start:       start,
		End:         len(state.pcm),
		Length:      length,
		Crossfade:   state.crossfadeSamples,
		ZeroAligned: zeroAligned,
	})
	state.loop = loop
	state.hasLoop = true
	return loop, nil
}

// Loop reads the selected loop of one sample.
func (p *LoopPointPlanner) Loop(sample string) (LoopPoint, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	state, ok := p.samples[sample]
	if !ok || !state.hasLoop {
		return LoopPoint{}, false
	}
	return state.loop, true
}

// SetCrossfade configures the crossfade window of one sample.
func (p *LoopPointPlanner) SetCrossfade(sample string, d time.Duration) error {
	if blankSample(sample) {
		return ErrBadSample
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	state, ok := p.samples[sample]
	if !ok {
		return ErrUnknownSample
	}

	samples, err := durationToSamples(d, p.opts.SampleRate, p.opts.MaxCrossfade)
	if err != nil {
		return err
	}

	state.crossfade = d
	state.crossfadeSamples = samples
	if state.hasLoop {
		state.loop = normalizeLoopCrossfade(LoopPoint{
			Sample:      state.loop.Sample,
			Start:       state.loop.Start,
			End:         state.loop.End,
			Length:      state.loop.Length,
			ZeroAligned: state.loop.ZeroAligned,
			Crossfade:   samples,
		})
	}
	return nil
}

// Crossfade returns the configured crossfade window of one sample.
func (p *LoopPointPlanner) Crossfade(sample string) time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()

	state, ok := p.samples[sample]
	if !ok {
		return 0
	}
	return state.crossfade
}

// Validate checks one loop point against the loop rules.
func (p *LoopPointPlanner) Validate(loop LoopPoint) error {
	return validateLoop(loop, p.opts.MinLoop)
}

// Apply renders one loop iteration with the crossfade window applied.
func (p *LoopPointPlanner) Apply(loop LoopPoint, pcm []float32) ([]float32, error) {
	if err := p.Validate(loop); err != nil {
		return nil, err
	}
	if loop.End > len(pcm) {
		return nil, ErrBadLoop
	}

	start := loop.Start
	length := loop.Length

	// A crossfade covering the complete loop leaves a degenerate one-sample
	// playback point. Validate still guarantees that length is positive.
	if loop.Degraded || loop.Crossfade == length {
		return []float32{pcm[start]}, nil
	}

	out := make([]float32, length)
	copy(out, pcm[start:loop.End])

	crossfade := loop.Crossfade
	for i := 0; i < crossfade; i++ {
		headWeight := float32(i+1) / float32(crossfade+1)
		tailWeight := float32(crossfade-i) / float32(crossfade+1)
		head := pcm[start+i]
		tail := pcm[loop.End-crossfade+i]
		out[i] = headWeight*head + tailWeight*tail
	}

	return out, nil
}

// Discard forgets everything known about one sample.
func (p *LoopPointPlanner) Discard(sample string) error {
	if blankSample(sample) {
		return ErrBadSample
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.samples[sample]; !ok {
		return ErrUnknownSample
	}
	delete(p.samples, sample)
	return nil
}

// Samples lists the analysed sample keys in ascending order.
func (p *LoopPointPlanner) Samples() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	keys := make([]string, 0, len(p.samples))
	for key := range p.samples {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Snapshot serialises the analysed PCM, candidates, loops and crossfades.
func (p *LoopPointPlanner) Snapshot() ([]byte, error) {
	p.mu.RLock()
	dto := snapshotData{
		Format:  snapshotFormat,
		Version: snapshotVersion,
		Options: p.opts,
		Samples: make([]snapshotSample, 0, len(p.samples)),
	}

	keys := make([]string, 0, len(p.samples))
	for key := range p.samples {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		state := p.samples[key]
		item := snapshotSample{
			Key:              key,
			PCM:              cloneFloat32(state.pcm),
			Candidates:       cloneCandidates(state.candidates),
			Crossfade:        state.crossfade,
			CrossfadeSamples: state.crossfadeSamples,
		}
		if state.hasLoop {
			loop := state.loop
			item.Loop = &loop
		}
		dto.Samples = append(dto.Samples, item)
	}
	p.mu.RUnlock()

	data, err := json.Marshal(dto)
	if err != nil {
		return nil, fmt.Errorf("encode sample-loop snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *LoopPointPlanner) Restore(data []byte) error {
	var dto snapshotData
	if err := json.Unmarshal(data, &dto); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.validateSnapshot(&dto); err != nil {
		return err
	}

	next := make(map[string]*sampleState, len(dto.Samples))
	for _, item := range dto.Samples {
		state := &sampleState{
			pcm:              cloneFloat32(item.PCM),
			candidates:       cloneCandidates(item.Candidates),
			crossfade:        item.Crossfade,
			crossfadeSamples: item.CrossfadeSamples,
		}
		if item.Loop != nil {
			state.loop = *item.Loop
			state.hasLoop = true
		}
		next[item.Key] = state
	}
	p.samples = next
	return nil
}

func (p *LoopPointPlanner) validateSnapshot(dto *snapshotData) error {
	if dto.Format != snapshotFormat || dto.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported snapshot format", ErrBadSnapshot)
	}
	if dto.Options.SampleRate <= 0 || dto.Options.Window <= 0 ||
		dto.Options.MinLoop <= 0 || dto.Options.MaxCandidates <= 0 ||
		dto.Options.MaxCrossfade <= 0 {
		return fmt.Errorf("%w: invalid snapshot options", ErrBadSnapshot)
	}

	// Restarted planners remain the authority for configured safety limits.
	if dto.Options.Window > p.opts.Window {
		return fmt.Errorf("%w: analysis window exceeds configuration", ErrBadSnapshot)
	}
	if dto.Options.MaxCandidates > p.opts.MaxCandidates {
		return fmt.Errorf("%w: candidate cap exceeds configuration", ErrBadSnapshot)
	}

	seen := make(map[string]struct{}, len(dto.Samples))
	for _, item := range dto.Samples {
		if blankSample(item.Key) {
			return fmt.Errorf("%w: blank sample key", ErrBadSnapshot)
		}
		if _, duplicate := seen[item.Key]; duplicate {
			return fmt.Errorf("%w: duplicate sample %q", ErrBadSnapshot, item.Key)
		}
		seen[item.Key] = struct{}{}

		if len(item.PCM) < 2*dto.Options.Window+1 {
			return fmt.Errorf("%w: short PCM for %q", ErrBadSnapshot, item.Key)
		}
		if len(item.Candidates) > p.opts.MaxCandidates {
			return fmt.Errorf("%w: too many candidates for %q", ErrBadSnapshot, item.Key)
		}
		if dto.Options.MaxCandidates > p.opts.MaxCandidates {
			return fmt.Errorf("%w: candidate cap exceeds configuration", ErrBadSnapshot)
		}

		expected := scoreCandidates(item.PCM, dto.Options.Window, dto.Options.MaxCandidates)
		if !slices.Equal(expected, item.Candidates) {
			return fmt.Errorf("%w: inconsistent candidates for %q", ErrBadSnapshot, item.Key)
		}
		if item.Crossfade < 0 || item.CrossfadeSamples < 0 ||
			item.CrossfadeSamples > p.opts.MaxCrossfade {
			return fmt.Errorf("%w: invalid crossfade for %q", ErrBadSnapshot, item.Key)
		}

		if item.Loop != nil {
			loop := *item.Loop
			if err := validateLoop(loop, p.opts.MinLoop); err != nil {
				return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
			}
			if loop.Sample != item.Key || loop.End > len(item.PCM) {
				return fmt.Errorf("%w: loop range mismatch for %q", ErrBadSnapshot, item.Key)
			}
			if loop.ZeroAligned != isZeroCrossing(item.PCM, loop.Start) {
				return fmt.Errorf("%w: zero-crossing flag mismatch for %q", ErrBadSnapshot, item.Key)
			}

			effectiveCrossfade := item.CrossfadeSamples
			degraded := false
			if effectiveCrossfade >= loop.Length {
				effectiveCrossfade = loop.Length
				degraded = true
			}
			if loop.Crossfade != effectiveCrossfade || loop.Degraded != degraded {
				return fmt.Errorf("%w: crossfade state mismatch for %q", ErrBadSnapshot, item.Key)
			}
		}
	}
	return nil
}

func validateLoop(loop LoopPoint, minLoop int) error {
	if blankSample(loop.Sample) {
		return ErrBadSample
	}
	if loop.Start < 0 {
		return ErrBadLoop
	}
	if loop.End <= loop.Start {
		return ErrBadLoop
	}
	if loop.Length != loop.End-loop.Start {
		return ErrBadLoop
	}
	if loop.Crossfade < 0 || loop.Crossfade > loop.Length {
		return ErrBadLoop
	}
	if loop.Length < minLoop {
		return ErrShortLoop
	}
	return nil
}

func normalizeLoopCrossfade(loop LoopPoint) LoopPoint {
	if loop.Crossfade >= loop.Length {
		loop.Crossfade = loop.Length
		loop.Degraded = true
	}
	return loop
}

func blankSample(sample string) bool {
	return strings.TrimSpace(sample) == ""
}

func cloneFloat32(values []float32) []float32 {
	if values == nil {
		return nil
	}
	cloned := make([]float32, len(values))
	copy(cloned, values)
	return cloned
}

func cloneCandidates(candidates []Candidate) []Candidate {
	cloned := make([]Candidate, len(candidates))
	copy(cloned, candidates)
	return cloned
}

func scoreCandidates(pcm []float32, window, limit int) []Candidate {
	scored := make([]Candidate, 0)

	// i is the first sample of the head window; pcm[i-window:i] is its
	// adjacent tail window. The strict upper bound leaves the analysis length
	// at 2*window+1 for the smallest accepted PCM.
	for i := window; i < len(pcm)-window; i++ {
		var difference float64
		for j := 0; j < window; j++ {
			tail := float64(pcm[i-window+j])
			head := float64(pcm[i+j])
			difference += math.Abs(tail - head)
		}

		meanDifference := difference / float64(window)
		if meanDifference > 0.25 {
			continue
		}

		score := math.Round((1-meanDifference)*1000) / 1000
		scored = append(scored, Candidate{
			Index:        i,
			Score:        score,
			ZeroCrossing: isZeroCrossing(pcm, i),
		})
	}

	merged := make([]Candidate, 0, len(scored))
	previousIndex := 0
	havePrevious := false
	for _, candidate := range scored {
		if havePrevious && candidate.Index == previousIndex+1 &&
			merged[len(merged)-1].Score == candidate.Score {
			previousIndex = candidate.Index
			continue
		}
		merged = append(merged, candidate)
		previousIndex = candidate.Index
		havePrevious = true
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].Index < merged[j].Index
	})

	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func isZeroCrossing(pcm []float32, index int) bool {
	if index < 0 || index >= len(pcm) {
		return false
	}
	if pcm[index] == 0 {
		return true
	}
	if index == 0 {
		return false
	}

	previous := pcm[index-1]
	current := pcm[index]
	return (previous < 0 && current > 0) || (previous > 0 && current < 0)
}

func nearestCandidateIndex(candidates []Candidate, at int) int {
	best := candidates[0].Index
	bestDistance := absInt(at - best)

	for _, candidate := range candidates[1:] {
		distance := absInt(at - candidate.Index)
		if distance < bestDistance || (distance == bestDistance && candidate.Index < best) {
			best = candidate.Index
			bestDistance = distance
		}
	}
	return best
}

func nearestZeroCrossing(pcm []float32, target int) (int, bool) {
	return nearestZeroCrossingAtOrBefore(pcm, target, len(pcm)-1)
}

func nearestZeroCrossingAtOrBefore(pcm []float32, target, maxIndex int) (int, bool) {
	best := target
	bestDistance := 0
	found := false

	for index := 0; index < len(pcm) && index <= maxIndex; index++ {
		if !isZeroCrossing(pcm, index) {
			continue
		}

		distance := absInt(index - target)
		if !found || distance < bestDistance || (distance == bestDistance && index < best) {
			best = index
			bestDistance = distance
			found = true
		}
	}
	return best, found
}

func durationToSamples(d time.Duration, sampleRate, maxSamples int) (int, error) {
	if d < 0 {
		return 0, ErrBadCrossfade
	}

	value := d.Seconds() * float64(sampleRate)
	if math.IsNaN(value) || value < 0 || value >= float64(maxSamples)+1 {
		return 0, ErrBadCrossfade
	}

	samples := int(value)
	if samples < 0 || samples > maxSamples {
		return 0, ErrBadCrossfade
	}
	return samples, nil
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
