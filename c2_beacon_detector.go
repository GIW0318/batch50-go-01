// Package task consumes outbound connection events, groups them per
// destination and grades how close the traffic sits to a machine paced beacon:
// regular spacing raises the score, irregular spacing pushes it down, and an
// operator allow entry keeps a known destination quiet for a while.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

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

// C2BeaconDetector owns the per destination windows and the allow table.
type C2BeaconDetector struct {
	limits Limits
}

// NewC2BeaconDetector builds a detector with the given caps.
func NewC2BeaconDetector(limits Limits) *C2BeaconDetector {
	return &C2BeaconDetector{limits: limits}
}

// Ingest admits one outbound event and returns the running state.
func (d *C2BeaconDetector) Ingest(ev FlowEvent) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Score grades one destination from the events kept in its window.
func (d *C2BeaconDetector) Score(dst string) (BeaconScore, error) {
	return BeaconScore{}, errNotImplemented
}

// Explain returns the evidence behind the score of one destination.
func (d *C2BeaconDetector) Explain(dst string) (Evidence, error) {
	return Evidence{}, errNotImplemented
}

// Allow keeps a destination quiet until the ttl runs out.
func (d *C2BeaconDetector) Allow(dst string, ttl time.Duration) error {
	return errNotImplemented
}

// Decay drops expired allow entries and moves aged events to history.
func (d *C2BeaconDetector) Decay(now time.Time) error {
	return errNotImplemented
}

// Snapshot serialises the windows, the allow table and the counters.
func (d *C2BeaconDetector) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the state with a snapshot, all or nothing.
func (d *C2BeaconDetector) Restore(data []byte) error {
	return errNotImplemented
}

// Allowlist lists the destinations still allowed at the given moment.
func (d *C2BeaconDetector) Allowlist(now time.Time) []string {
	return nil
}

// Destinations lists the tracked destinations in ascending order.
func (d *C2BeaconDetector) Destinations() []string {
	return nil
}

// Samples reports how many events the window of one destination holds.
func (d *C2BeaconDetector) Samples(dst string) int {
	return 0
}

// Forget drops one destination and frees its slot.
func (d *C2BeaconDetector) Forget(dst string) error {
	return errNotImplemented
}
