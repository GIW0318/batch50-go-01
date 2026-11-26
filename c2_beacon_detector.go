// Package task consumes outbound connection events, groups them per
// destination and grades how close the traffic sits to a machine paced beacon:
// regular spacing raises the score, irregular spacing pushes it down, and an
// operator allow entry keeps a known destination quiet for a while.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tracking states reported for one destination.
const (
	StateCollecting = "collecting"
	StateBeacon     = "beacon"
	StateBenign     = "benign"
	StateAllowed    = "allowed"
)

// Errors reported by the detector.
var (
	ErrBadDestination       = errors.New("destination is required")
	ErrDestinationLimit     = errors.New("too many destinations are tracked")
	ErrInsufficientEvidence = errors.New("not enough evidence")
	ErrUnknownDestination   = errors.New("unknown destination")
	ErrBadTTL               = errors.New("allow ttl must be positive")
	ErrBadSnapshot          = errors.New("snapshot payload is invalid")
)

// FlowEvent is one outbound connection as reported by the sensor.
type FlowEvent struct {
	Dst      string
	Src      string
	At       time.Time
	BytesOut int
	BytesIn  int
	Duration time.Duration
}

// Verdict is the cheap per event answer handed back by Ingest.
type Verdict struct {
	Dst     string
	Tracked bool
	Samples int
	State   string
}

// BeaconScore is the graded view of one destination.
type BeaconScore struct {
	Dst             string
	Samples         int
	MeanIntervalSec int64
	JitterPercent   int
	Confidence      int
	PayloadStable   bool
	DurationStable  bool
	State           string
}

// Evidence explains why a destination scored the way it did.
type Evidence struct {
	Dst        string
	Reason     string
	Samples    int
	Historical int
	Intervals  []int64
}

// Limits bounds the tables and the grading thresholds.
type Limits struct {
	MaxDestinations int
	MaxEventsPerDst int
	MinSamples      int
	BeaconThreshold int
	EvidenceHorizon time.Duration
}

// storedEvent is the compact form kept in a destination window. The source is
// retained because shared CDN destinations must be scored per source.
type storedEvent struct {
	At       time.Time     `json:"at"`
	Src      string        `json:"src"`
	BytesOut int           `json:"bytes_out"`
	BytesIn  int           `json:"bytes_in"`
	Duration time.Duration `json:"duration"`
}

// destinationState owns one destination's evidence. The detector's registry
// lock stabilises the map and allow table; this lock serialises the window of
// one destination while letting other destinations make progress.
type destinationState struct {
	mu         sync.Mutex
	dst        string
	events     []storedEvent
	historical int
}

// C2BeaconDetector owns the per destination windows and the allow table.
type C2BeaconDetector struct {
	limits Limits

	mu     sync.RWMutex
	dests  map[string]*destinationState
	allows map[string]time.Time
}

// NewC2BeaconDetector builds a detector with the given caps. A zero limit is
// treated as "use the safe default" so a zero value Limits remains usable.
func NewC2BeaconDetector(limits Limits) *C2BeaconDetector {
	limits = withDefaults(limits)
	return &C2BeaconDetector{
		limits: limits,
		dests:  make(map[string]*destinationState),
		allows: make(map[string]time.Time),
	}
}

func withDefaults(l Limits) Limits {
	if l.MaxDestinations <= 0 {
		l.MaxDestinations = 1024
	}
	if l.MaxEventsPerDst <= 0 {
		l.MaxEventsPerDst = 1024
	}
	if l.MinSamples <= 0 {
		l.MinSamples = 3
	}
	if l.BeaconThreshold <= 0 {
		l.BeaconThreshold = 80
	}
	if l.EvidenceHorizon <= 0 {
		l.EvidenceHorizon = 24 * time.Hour
	}
	return l
}

func normalizeDestination(raw string) (string, error) {
	dst := strings.ToLower(strings.TrimSpace(raw))
	if dst == "" {
		return "", ErrBadDestination
	}
	return dst, nil
}

func normalizeSource(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func allowActive(allows map[string]time.Time, dst string, now time.Time) bool {
	expiry, ok := allows[dst]
	return ok && expiry.After(now)
}

// insertEvent keeps the window in (At, Src) order, drops an exact duplicate
// (same instant and same source) and enforces the per destination cap by
// evicting the oldest entries. The returned drop count is evidence that left
// the live window and therefore belongs in the historical count.
func insertEvent(events []storedEvent, ev storedEvent, max int) ([]storedEvent, int, bool) {
	for _, have := range events {
		if have.Src == ev.Src && have.At.Equal(ev.At) {
			return events, 0, true
		}
	}

	idx := sort.Search(len(events), func(i int) bool {
		have := events[i]
		if have.At.Equal(ev.At) {
			return have.Src > ev.Src
		}
		return have.At.After(ev.At)
	})

	next := make([]storedEvent, 0, len(events)+1)
	next = append(next, events[:idx]...)
	next = append(next, ev)
	next = append(next, events[idx:]...)

	dropped := 0
	if max > 0 && len(next) > max {
		dropped = len(next) - max
		trimmed := make([]storedEvent, max)
		copy(trimmed, next[dropped:])
		next = trimmed
	}
	return next, dropped, false
}

// Ingest admits one outbound event and returns the running state.
func (d *C2BeaconDetector) Ingest(ev FlowEvent) (Verdict, error) {
	dst, err := normalizeDestination(ev.Dst)
	if err != nil {
		return Verdict{}, err
	}
	ev.Dst = dst
	ev.Src = normalizeSource(ev.Src)
	stored := storedEvent{
		At:       ev.At,
		Src:      ev.Src,
		BytesOut: ev.BytesOut,
		BytesIn:  ev.BytesIn,
		Duration: ev.Duration,
	}
	now := time.Now()

	// Fast path: an existing destination only needs the registry read lock so
	// other destinations can ingest and score concurrently. The destination
	// lock makes the window update all-or-nothing for readers of this window.
	d.mu.RLock()
	st := d.dests[dst]
	if st != nil {
		st.mu.Lock()
		allowed := allowActive(d.allows, dst, now)
		var dropped int
		st.events, dropped, _ = insertEvent(st.events, stored, d.limits.MaxEventsPerDst)
		st.historical += dropped
		verdict := verdictFor(dst, st.events, allowed, d.limits)
		st.mu.Unlock()
		d.mu.RUnlock()
		return verdict, nil
	}
	d.mu.RUnlock()

	// Slow path: creating a destination changes the registry and therefore
	// needs the exclusive side of the same lock that guards the allow table.
	d.mu.Lock()
	defer d.mu.Unlock()

	st = d.dests[dst]
	if st == nil {
		if len(d.dests) >= d.limits.MaxDestinations {
			return Verdict{}, ErrDestinationLimit
		}
		st = &destinationState{dst: dst}
		d.dests[dst] = st
	}
	allowed := allowActive(d.allows, dst, now)
	var dropped int
	st.events, dropped, _ = insertEvent(st.events, stored, d.limits.MaxEventsPerDst)
	st.historical += dropped
	return verdictFor(dst, st.events, allowed, d.limits), nil
}

func verdictFor(dst string, events []storedEvent, allowed bool, limits Limits) Verdict {
	samples := len(events)
	state := StateCollecting
	if allowed {
		state = StateAllowed
	} else if samples >= limits.MinSamples {
		analysis := analyzeEvents(dst, events, limits)
		if analysis.evaluated {
			state = analysis.score.State
		}
	}
	return Verdict{Dst: dst, Tracked: true, Samples: samples, State: state}
}

// Score grades one destination from the events kept in its window.
func (d *C2BeaconDetector) Score(dst string) (BeaconScore, error) {
	dst, err := normalizeDestination(dst)
	if err != nil {
		return BeaconScore{}, err
	}
	now := time.Now()

	d.mu.RLock()
	st := d.dests[dst]
	allowed := allowActive(d.allows, dst, now)
	if st == nil {
		d.mu.RUnlock()
		if allowed {
			return BeaconScore{Dst: dst, State: StateAllowed, Confidence: 0}, nil
		}
		return BeaconScore{}, ErrUnknownDestination
	}

	st.mu.Lock()
	samples := len(st.events)
	if allowed {
		st.mu.Unlock()
		d.mu.RUnlock()
		return BeaconScore{Dst: dst, Samples: samples, State: StateAllowed, Confidence: 0}, nil
	}
	analysis := analyzeEvents(dst, st.events, d.limits)
	st.mu.Unlock()
	d.mu.RUnlock()

	if !analysis.evaluated || samples < d.limits.MinSamples {
		return BeaconScore{}, ErrInsufficientEvidence
	}
	return analysis.score, nil
}

// Explain returns the evidence behind the score of one destination.
func (d *C2BeaconDetector) Explain(dst string) (Evidence, error) {
	dst, err := normalizeDestination(dst)
	if err != nil {
		return Evidence{}, err
	}
	now := time.Now()

	d.mu.RLock()
	st := d.dests[dst]
	if st == nil {
		d.mu.RUnlock()
		return Evidence{}, ErrInsufficientEvidence
	}

	st.mu.Lock()
	samples := len(st.events)
	if samples == 0 {
		st.mu.Unlock()
		d.mu.RUnlock()
		return Evidence{}, ErrInsufficientEvidence
	}
	allowed := allowActive(d.allows, dst, now)
	analysis := analyzeEvents(dst, st.events, d.limits)
	historical := st.historical
	st.mu.Unlock()
	d.mu.RUnlock()

	reason := analysis.reason
	if reason == "" {
		reason = "collecting:insufficient_samples"
	}
	if allowed {
		reason = "allowed:ttl"
	}
	return Evidence{
		Dst:        dst,
		Reason:     reason,
		Samples:    samples,
		Historical: historical,
		Intervals:  analysis.intervals,
	}, nil
}

// Allow keeps a destination quiet until the ttl runs out.
func (d *C2BeaconDetector) Allow(dst string, ttl time.Duration) error {
	dst, err := normalizeDestination(dst)
	if err != nil {
		return err
	}
	if ttl <= 0 {
		return ErrBadTTL
	}
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	for name, expiry := range d.allows {
		if !expiry.After(now) {
			delete(d.allows, name)
		}
	}
	d.allows[dst] = now.Add(ttl)
	return nil
}

// Decay drops expired allow entries and moves aged events to history.
func (d *C2BeaconDetector) Decay(now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for name, expiry := range d.allows {
		if !expiry.After(now) {
			delete(d.allows, name)
		}
	}

	cutoff := now.Add(-d.limits.EvidenceHorizon)
	for _, st := range d.dests {
		kept := make([]storedEvent, 0, len(st.events))
		dropped := 0
		for _, ev := range st.events {
			if ev.At.Before(cutoff) {
				dropped++
				continue
			}
			kept = append(kept, ev)
		}
		st.events = kept
		st.historical += dropped
	}
	return nil
}

const snapshotVersion = 1

type snapshotState struct {
	Version      int             `json:"version"`
	Destinations []snapshotDest  `json:"destinations"`
	Allows       []snapshotAllow `json:"allows"`
}

type snapshotDest struct {
	Dst        string        `json:"dst"`
	Historical int           `json:"historical"`
	Events     []storedEvent `json:"events"`
}

type snapshotAllow struct {
	Dst     string    `json:"dst"`
	Expires time.Time `json:"expires"`
}

// Snapshot serialises the windows, the allow table and the counters.
func (d *C2BeaconDetector) Snapshot() ([]byte, error) {
	d.mu.RLock()

	names := make([]string, 0, len(d.dests))
	for name := range d.dests {
		names = append(names, name)
	}
	sort.Strings(names)

	snap := snapshotState{
		Version:      snapshotVersion,
		Destinations: make([]snapshotDest, 0, len(names)),
		Allows:       make([]snapshotAllow, 0, len(d.allows)),
	}
	for _, name := range names {
		st := d.dests[name]
		st.mu.Lock()
		events := append([]storedEvent(nil), st.events...)
		historical := st.historical
		st.mu.Unlock()
		snap.Destinations = append(snap.Destinations, snapshotDest{
			Dst:        name,
			Historical: historical,
			Events:     events,
		})
	}

	allowNames := make([]string, 0, len(d.allows))
	for name := range d.allows {
		allowNames = append(allowNames, name)
	}
	sort.Strings(allowNames)
	for _, name := range allowNames {
		snap.Allows = append(snap.Allows, snapshotAllow{Dst: name, Expires: d.allows[name]})
	}
	d.mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the state with a snapshot, all or nothing.
func (d *C2BeaconDetector) Restore(data []byte) error {
	dests, allows, err := decodeSnapshot(data, d.limits)
	if err != nil {
		return err
	}

	d.mu.Lock()
	d.dests = dests
	d.allows = allows
	d.mu.Unlock()
	return nil
}

func badSnapshot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadSnapshot, fmt.Sprintf(format, args...))
}

func decodeSnapshot(data []byte, limits Limits) (map[string]*destinationState, map[string]time.Time, error) {
	var snap snapshotState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return nil, nil, badSnapshot("decode: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, nil, badSnapshot("trailing data")
	}
	if snap.Version != snapshotVersion {
		return nil, nil, badSnapshot("unsupported version %d", snap.Version)
	}
	if len(snap.Destinations) > limits.MaxDestinations {
		return nil, nil, badSnapshot("too many destinations")
	}

	dests := make(map[string]*destinationState, len(snap.Destinations))
	for _, sd := range snap.Destinations {
		dst, err := normalizeDestination(sd.Dst)
		if err != nil {
			return nil, nil, badSnapshot("bad destination %q", sd.Dst)
		}
		if _, exists := dests[dst]; exists {
			return nil, nil, badSnapshot("duplicate destination %q", dst)
		}
		if sd.Historical < 0 {
			return nil, nil, badSnapshot("negative historical count for %q", dst)
		}
		if len(sd.Events) > limits.MaxEventsPerDst {
			return nil, nil, badSnapshot("too many events for %q", dst)
		}

		st := &destinationState{dst: dst, historical: sd.Historical}
		seen := make(map[string]struct{}, len(sd.Events))
		for _, ev := range sd.Events {
			ev.Src = normalizeSource(ev.Src)
			key := ev.Src + "\x00" + ev.At.UTC().Format(time.RFC3339Nano)
			if _, exists := seen[key]; exists {
				return nil, nil, badSnapshot("duplicate event for %q at %s", dst, ev.At.UTC().Format(time.RFC3339Nano))
			}
			seen[key] = struct{}{}
			st.events = append(st.events, ev)
		}
		sort.Slice(st.events, func(i, j int) bool {
			if st.events[i].At.Equal(st.events[j].At) {
				return st.events[i].Src < st.events[j].Src
			}
			return st.events[i].At.Before(st.events[j].At)
		})
		dests[dst] = st
	}

	allows := make(map[string]time.Time, len(snap.Allows))
	for _, sa := range snap.Allows {
		dst, err := normalizeDestination(sa.Dst)
		if err != nil {
			return nil, nil, badSnapshot("bad allow destination %q", sa.Dst)
		}
		if _, exists := allows[dst]; exists {
			return nil, nil, badSnapshot("duplicate allow destination %q", dst)
		}
		if sa.Expires.IsZero() {
			return nil, nil, badSnapshot("allow destination %q has no expiry", dst)
		}
		allows[dst] = sa.Expires
	}
	return dests, allows, nil
}

// Allowlist lists the destinations still allowed at the given moment.
func (d *C2BeaconDetector) Allowlist(now time.Time) []string {
	d.mu.RLock()
	names := make([]string, 0, len(d.allows))
	for name, expiry := range d.allows {
		if expiry.After(now) {
			names = append(names, name)
		}
	}
	d.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Destinations lists the tracked destinations in ascending order.
func (d *C2BeaconDetector) Destinations() []string {
	d.mu.RLock()
	names := make([]string, 0, len(d.dests))
	for name := range d.dests {
		names = append(names, name)
	}
	d.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Samples reports how many events the window of one destination holds.
func (d *C2BeaconDetector) Samples(dst string) int {
	dst, err := normalizeDestination(dst)
	if err != nil {
		return 0
	}

	d.mu.RLock()
	st := d.dests[dst]
	if st == nil {
		d.mu.RUnlock()
		return 0
	}
	st.mu.Lock()
	samples := len(st.events)
	st.mu.Unlock()
	d.mu.RUnlock()
	return samples
}

// Forget drops one destination and frees its slot.
func (d *C2BeaconDetector) Forget(dst string) error {
	dst, err := normalizeDestination(dst)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	_, tracked := d.dests[dst]
	_, allowed := d.allows[dst]
	if !tracked && !allowed {
		return ErrUnknownDestination
	}
	delete(d.dests, dst)
	delete(d.allows, dst)
	return nil
}

// eventAnalysis is the explainable result of grading one window.
type eventAnalysis struct {
	total     int
	evaluated bool
	score     BeaconScore
	reason    string
	intervals []int64
}

type eventGroup struct {
	src    string
	events []storedEvent
}

type groupMeasurement struct {
	events         int
	intervalsSec   []int64
	meanSec        int64
	jitter         int
	confidence     int
	payloadStable  bool
	durationStable bool
	state          string
	resampled      bool
}

// analyzeEvents first splits a destination window by source, then grades each
// source on its own cadence. Shared CDN tenants therefore cannot pollute one
// another's intervals. The strongest source cadence is reported for the
// destination, with source_grouped recorded in the reason.
func analyzeEvents(dst string, events []storedEvent, limits Limits) eventAnalysis {
	a := eventAnalysis{total: len(events)}
	groups := buildGroups(events)

	var best *groupMeasurement
	var evidence *eventGroup
	for i := range groups {
		g := &groups[i]
		if evidence == nil || len(g.events) > len(evidence.events) {
			evidence = g
		}
		if len(g.events) < 2 {
			continue
		}
		intervals := intervalsOf(g.events)
		m := measureGroup(g.events, intervals, limits)
		if best == nil || betterMeasurement(m, *best) {
			copy := m
			best = &copy
		}
	}

	if best != nil {
		a.intervals = best.intervalsSec
	} else if evidence != nil && len(evidence.events) >= 2 {
		a.intervals = intervalSecondsOf(evidence.events)
	}

	if best == nil || a.total < limits.MinSamples {
		a.reason = "collecting:insufficient_samples"
		return a
	}

	a.evaluated = true
	a.score = BeaconScore{
		Dst:             dst,
		Samples:         a.total,
		MeanIntervalSec: best.meanSec,
		JitterPercent:   best.jitter,
		Confidence:      best.confidence,
		PayloadStable:   best.payloadStable,
		DurationStable:  best.durationStable,
		State:           best.state,
	}
	a.reason = reasonFor(*best, len(groups), limits)
	return a
}

func buildGroups(events []storedEvent) []eventGroup {
	bySource := make(map[string][]storedEvent)
	for _, ev := range events {
		bySource[ev.Src] = append(bySource[ev.Src], ev)
	}

	sources := make([]string, 0, len(bySource))
	for src := range bySource {
		sources = append(sources, src)
	}
	sort.Strings(sources)

	groups := make([]eventGroup, 0, len(sources))
	for _, src := range sources {
		events := bySource[src]
		sort.Slice(events, func(i, j int) bool {
			if events[i].At.Equal(events[j].At) {
				return events[i].Src < events[j].Src
			}
			return events[i].At.Before(events[j].At)
		})

		deduped := events[:0]
		for _, ev := range events {
			if len(deduped) > 0 {
				last := deduped[len(deduped)-1]
				if last.Src == ev.Src && last.At.Equal(ev.At) {
					continue
				}
			}
			deduped = append(deduped, ev)
		}
		groups = append(groups, eventGroup{src: src, events: deduped})
	}
	return groups
}

func intervalsOf(events []storedEvent) []time.Duration {
	if len(events) < 2 {
		return nil
	}
	intervals := make([]time.Duration, 0, len(events)-1)
	for i := 1; i < len(events); i++ {
		intervals = append(intervals, events[i].At.Sub(events[i-1].At))
	}
	return intervals
}

func intervalSecondsOf(events []storedEvent) []int64 {
	intervals := intervalsOf(events)
	out := make([]int64, len(intervals))
	for i, interval := range intervals {
		out[i] = int64(interval / time.Second)
	}
	return out
}

func measureGroup(events []storedEvent, intervals []time.Duration, limits Limits) groupMeasurement {
	span := events[len(events)-1].At.Sub(events[0].At)
	mean := span / time.Duration(len(events)-1)

	m := groupMeasurement{
		events:       len(events),
		intervalsSec: make([]int64, len(intervals)),
		meanSec:      int64(mean / time.Second),
	}
	for i, interval := range intervals {
		m.intervalsSec[i] = int64(interval / time.Second)
	}

	m.payloadStable = stableSeries(payloadValues(events))
	m.durationStable = stableSeries(durationValues(events))

	baseJitter := jitterPercentOfIntervals(intervals)
	effectiveJitter := baseJitter
	baseConfidence := 100 - baseJitter
	if baseConfidence < 0 {
		baseConfidence = 0
	}
	// Resampling is a rescue path for a beacon that looks irregular at the
	// individual interval scale. It is only applied when the basic cadence is
	// below threshold, the payload/duration are stable, and the wider spans
	// actually converge enough to clear the threshold. This keeps the raw
	// interval formula authoritative for windows that stay benign.
	if len(events) >= 5 && m.payloadStable && m.durationStable && baseConfidence < limits.BeaconThreshold {
		if resampledJitter, ok := resampledIntervalJitter(intervals); ok && resampledJitter < baseJitter {
			resampledConfidence := 100 - resampledJitter
			if resampledConfidence >= limits.BeaconThreshold {
				effectiveJitter = resampledJitter
				m.resampled = true
			}
		}
	}
	m.jitter = effectiveJitter
	m.confidence = 100 - effectiveJitter
	if m.confidence < 0 {
		m.confidence = 0
	}
	if m.confidence >= limits.BeaconThreshold && m.payloadStable && m.durationStable {
		m.state = StateBeacon
	} else {
		m.state = StateBenign
	}
	return m
}

func betterMeasurement(candidate, current groupMeasurement) bool {
	if candidate.confidence != current.confidence {
		return candidate.confidence > current.confidence
	}
	if candidate.events != current.events {
		return candidate.events > current.events
	}
	return false
}

func reasonFor(m groupMeasurement, sources int, limits Limits) string {
	var reason string
	switch {
	case m.state == StateBeacon && m.resampled:
		reason = "beacon:resampled"
	case m.state == StateBeacon:
		reason = "beacon:periodic"
	case m.confidence >= limits.BeaconThreshold && (!m.payloadStable || !m.durationStable):
		reason = "benign:stability"
	case m.resampled:
		reason = "benign:irregular:resampled"
	default:
		reason = "benign:irregular"
	}
	if !m.payloadStable {
		reason += ":payload_unstable"
	}
	if !m.durationStable {
		reason += ":duration_unstable"
	}
	if sources > 1 {
		reason += ":source_grouped"
	}
	return reason
}

// jitterPercentOfIntervals implements the required measure:
// (max interval - min interval) / mean interval * 100.
func jitterPercentOfIntervals(intervals []time.Duration) int {
	if len(intervals) == 0 {
		return 0
	}
	values := make([]float64, len(intervals))
	for i, interval := range intervals {
		values[i] = float64(interval)
	}
	return jitterPercentOfValues(values)
}

func jitterPercentOfValues(values []float64) int {
	if len(values) == 0 {
		return 0
	}
	minValue, maxValue, sum := values[0], values[0], 0.0
	for _, value := range values {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
		sum += value
	}
	mean := sum / float64(len(values))
	if mean <= 0 {
		if maxValue == minValue {
			return 0
		}
		return 100
	}
	jitter := int(math.Round(((maxValue - minValue) / mean) * 100))
	if jitter < 0 {
		return 0
	}
	return jitter
}

// resampledIntervalJitter is the variable-span resampling step. Consecutive
// intervals are grouped into progressively wider spans (2..4 original
// intervals) and the grouped spans are asked to converge. A high-jitter
// beacon whose individual intervals oscillate can therefore still show a
// stable, longer cadence without relying on a fixed bucket histogram.
func resampledIntervalJitter(intervals []time.Duration) (int, bool) {
	if len(intervals) < 4 {
		return 0, false
	}

	best := 0
	found := false
	for width := 2; width <= 4; width++ {
		spans := len(intervals) / width
		if spans < 2 {
			continue
		}
		values := make([]float64, spans)
		for span := 0; span < spans; span++ {
			total := 0.0
			for i := span * width; i < (span+1)*width; i++ {
				total += float64(intervals[i])
			}
			values[span] = total
		}
		jitter := jitterPercentOfValues(values)
		if !found || jitter < best {
			best = jitter
			found = true
		}
	}
	return best, found
}

const stabilitySpreadLimit = 0.25

func stableSeries(values []float64) bool {
	if len(values) == 0 {
		return false
	}
	minValue, maxValue, sum := values[0], values[0], 0.0
	for _, value := range values {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
		sum += value
	}
	mean := sum / float64(len(values))
	if mean == 0 {
		return maxValue == minValue
	}
	if mean < 0 {
		mean = -mean
	}
	return (maxValue-minValue)/mean <= stabilitySpreadLimit+1e-9
}

func payloadValues(events []storedEvent) []float64 {
	values := make([]float64, len(events))
	for i, ev := range events {
		values[i] = float64(ev.BytesOut + ev.BytesIn)
	}
	return values
}

func durationValues(events []storedEvent) []float64 {
	values := make([]float64, len(events))
	for i, ev := range events {
		values[i] = float64(ev.Duration)
	}
	return values
}
