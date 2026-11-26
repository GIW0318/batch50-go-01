package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-28"
)

var beaconBase = time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

func testLimits() impl.Limits {
	return impl.Limits{
		MaxDestinations: 8,
		MaxEventsPerDst: 64,
		MinSamples:      3,
		BeaconThreshold: 80,
		EvidenceHorizon: time.Hour,
	}
}

func beaconEvent(dst, src string, offset time.Duration) impl.FlowEvent {
	return impl.FlowEvent{
		Dst:      dst,
		Src:      src,
		At:       beaconBase.Add(offset),
		BytesOut: 120,
		BytesIn:  64,
		Duration: 900 * time.Millisecond,
	}
}

func ingestAll(t *testing.T, d *impl.C2BeaconDetector, dst string, offsets []time.Duration) {
	t.Helper()
	for _, off := range offsets {
		if _, err := d.Ingest(beaconEvent(dst, "host-7", off)); err != nil {
			t.Fatalf("ingest at %s: %v", off, err)
		}
	}
}

func TestIngestTracksDestination(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	v, err := d.Ingest(beaconEvent(" API.Example.COM ", "host-7", 0))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !v.Tracked {
		t.Fatalf("a fresh destination must be tracked")
	}
	if v.Dst != "api.example.com" {
		t.Fatalf("destination = %q, want the trimmed lower case api.example.com", v.Dst)
	}
	if v.Samples != 1 {
		t.Fatalf("samples = %d, want 1", v.Samples)
	}
	if v.State != impl.StateCollecting {
		t.Fatalf("state = %q, want %q while the window is below the minimum", v.State, impl.StateCollecting)
	}
	if got := d.Samples("api.example.com"); got != 1 {
		t.Fatalf("stored samples = %d, want 1", got)
	}
}

func TestIngestNormalizesDestination(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	if _, err := d.Ingest(beaconEvent("api.example.com", "host-7", 0)); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := d.Ingest(beaconEvent("API.Example.com", "host-7", time.Minute)); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if got := d.Samples("api.example.com"); got != 2 {
		t.Fatalf("mixed case destinations must share one window, samples = %d", got)
	}
	if got := len(d.Destinations()); got != 1 {
		t.Fatalf("tracked destinations = %d, want 1", got)
	}
	if _, err := d.Ingest(beaconEvent("   ", "host-7", 0)); !errors.Is(err, impl.ErrBadDestination) {
		t.Fatalf("blank destination must fail with ErrBadDestination, got %v", err)
	}
}

func TestIngestEnforcesEventWindow(t *testing.T) {
	limits := testLimits()
	limits.MaxEventsPerDst = 3
	d := impl.NewC2BeaconDetector(limits)
	ingestAll(t, d, "cdn.example.net", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute})
	if got := d.Samples("cdn.example.net"); got != 3 {
		t.Fatalf("window holds %d events, want the cap 3", got)
	}
	score, err := d.Score("cdn.example.net")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if score.Samples != 3 {
		t.Fatalf("score samples = %d, want 3", score.Samples)
	}
	if score.MeanIntervalSec != 60 {
		t.Fatalf("mean interval = %d seconds, want 60 after the oldest events were dropped", score.MeanIntervalSec)
	}
}

func TestIngestEnforcesDestinationLimit(t *testing.T) {
	limits := testLimits()
	limits.MaxDestinations = 2
	d := impl.NewC2BeaconDetector(limits)
	if _, err := d.Ingest(beaconEvent("one.example", "host-7", 0)); err != nil {
		t.Fatalf("first destination: %v", err)
	}
	if _, err := d.Ingest(beaconEvent("two.example", "host-7", 0)); err != nil {
		t.Fatalf("second destination: %v", err)
	}
	if _, err := d.Ingest(beaconEvent("three.example", "host-7", 0)); !errors.Is(err, impl.ErrDestinationLimit) {
		t.Fatalf("third destination must fail with ErrDestinationLimit, got %v", err)
	}
	if err := d.Forget("one.example"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := d.Ingest(beaconEvent("three.example", "host-7", 0)); err != nil {
		t.Fatalf("a forgotten slot must be reusable, got %v", err)
	}
}

func TestIngestConcurrentSameDestinationKeepsEveryEvent(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.Ingest(beaconEvent("beat.example", fmt.Sprintf("host-%d", i%4), time.Duration(i)*time.Minute))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ingest: %v", err)
		}
	}
	if got := d.Samples("beat.example"); got != 16 {
		t.Fatalf("window holds %d events after 16 concurrent ingests, want 16", got)
	}
}

func TestScoreNeedsEnoughEvidence(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "short.example", []time.Duration{0, time.Minute})
	if _, err := d.Score("short.example"); !errors.Is(err, impl.ErrInsufficientEvidence) {
		t.Fatalf("two samples against a minimum of three must fail with ErrInsufficientEvidence, got %v", err)
	}
	if _, err := d.Score("missing.example"); !errors.Is(err, impl.ErrUnknownDestination) {
		t.Fatalf("an untracked destination must fail with ErrUnknownDestination, got %v", err)
	}
}

func TestScorePeriodicFlowIsBeacon(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "beacon.example", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute})
	score, err := d.Score("beacon.example")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if score.State != impl.StateBeacon {
		t.Fatalf("state = %q, want %q for evenly spaced traffic", score.State, impl.StateBeacon)
	}
	if score.Confidence != 100 {
		t.Fatalf("confidence = %d, want 100 when every interval is identical", score.Confidence)
	}
	if score.MeanIntervalSec != 60 {
		t.Fatalf("mean interval = %d, want 60", score.MeanIntervalSec)
	}
	if score.JitterPercent != 0 {
		t.Fatalf("jitter = %d percent, want 0", score.JitterPercent)
	}
}

func TestScoreIrregularFlowIsBenign(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "chatty.example", []time.Duration{0, 30 * time.Second, 2 * time.Minute, 150 * time.Second})
	score, err := d.Score("chatty.example")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if score.State != impl.StateBenign {
		t.Fatalf("state = %q, want %q for uneven spacing", score.State, impl.StateBenign)
	}
	if score.Confidence != 0 {
		t.Fatalf("confidence = %d, want 0 when the spread is wider than the mean", score.Confidence)
	}
	if score.MeanIntervalSec != 50 {
		t.Fatalf("mean interval = %d, want (30+90+30)/3 = 50", score.MeanIntervalSec)
	}
}

func TestScoreOutOfOrderEventsUseChronologicalOrder(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "shuffled.example", []time.Duration{3 * time.Minute, 0, 2 * time.Minute, time.Minute})
	score, err := d.Score("shuffled.example")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if score.MeanIntervalSec != 60 || score.Confidence != 100 {
		t.Fatalf("shuffled events scored mean=%d confidence=%d, want 60 and 100", score.MeanIntervalSec, score.Confidence)
	}
}

func TestExplainReportsWindowAndReason(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "beacon.example", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute})
	ev, err := d.Explain("beacon.example")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if ev.Samples != 4 {
		t.Fatalf("evidence samples = %d, want 4", ev.Samples)
	}
	if ev.Reason == "" {
		t.Fatalf("evidence must carry a reason")
	}
	if len(ev.Intervals) != 3 {
		t.Fatalf("evidence intervals = %v, want three entries", ev.Intervals)
	}
	for i, got := range ev.Intervals {
		if got != 60 {
			t.Fatalf("evidence interval %d = %d seconds, want 60", i, got)
		}
	}
	if _, err := d.Explain("missing.example"); !errors.Is(err, impl.ErrInsufficientEvidence) {
		t.Fatalf("explaining a destination without events must fail with ErrInsufficientEvidence, got %v", err)
	}
}

func TestAllowSuppressesScoring(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "known.example", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute})
	if err := d.Allow("known.example", time.Hour); err != nil {
		t.Fatalf("allow: %v", err)
	}
	score, err := d.Score("known.example")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if score.State != impl.StateAllowed {
		t.Fatalf("state = %q, want %q while the destination is allowed", score.State, impl.StateAllowed)
	}
	if score.Confidence != 0 {
		t.Fatalf("confidence = %d, want 0 for an allowed destination", score.Confidence)
	}
	if got := d.Allowlist(time.Now()); len(got) != 1 || got[0] != "known.example" {
		t.Fatalf("allowlist = %v, want only known.example", got)
	}
	if err := d.Allow("known.example", 0); !errors.Is(err, impl.ErrBadTTL) {
		t.Fatalf("a zero ttl must fail with ErrBadTTL, got %v", err)
	}
}

func TestAllowExpiresAfterTTL(t *testing.T) {
	limits := testLimits()
	limits.EvidenceHorizon = 8760 * time.Hour
	d := impl.NewC2BeaconDetector(limits)
	ingestAll(t, d, "known.example", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute})
	if err := d.Allow("known.example", 30*time.Minute); err != nil {
		t.Fatalf("allow: %v", err)
	}
	later := time.Now().Add(2 * time.Hour)
	if err := d.Decay(later); err != nil {
		t.Fatalf("decay: %v", err)
	}
	if got := d.Allowlist(later); len(got) != 0 {
		t.Fatalf("allowlist after expiry = %v, want empty", got)
	}
	score, err := d.Score("known.example")
	if err != nil {
		t.Fatalf("score after expiry: %v", err)
	}
	if score.State == impl.StateAllowed {
		t.Fatalf("an expired allow entry must not keep the destination quiet")
	}
}

func TestDecayAgesOldEvidence(t *testing.T) {
	limits := testLimits()
	limits.EvidenceHorizon = 90 * time.Second
	d := impl.NewC2BeaconDetector(limits)
	ingestAll(t, d, "beacon.example", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute})
	if err := d.Decay(beaconBase.Add(200 * time.Second)); err != nil {
		t.Fatalf("decay: %v", err)
	}
	if got := d.Samples("beacon.example"); got != 2 {
		t.Fatalf("samples after decay = %d, want the two newest events", got)
	}
	ev, err := d.Explain("beacon.example")
	if err != nil {
		t.Fatalf("explain after decay: %v", err)
	}
	if ev.Samples != 2 {
		t.Fatalf("evidence samples = %d, want 2", ev.Samples)
	}
	if ev.Historical != 2 {
		t.Fatalf("historical events = %d, want 2 aged out", ev.Historical)
	}
}

func TestSnapshotRestoreKeepsWindowsAndAllowlist(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "beacon.example", []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute})
	if err := d.Allow("beacon.example", time.Hour); err != nil {
		t.Fatalf("allow: %v", err)
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewC2BeaconDetector(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Samples("beacon.example"); got != 4 {
		t.Fatalf("restored samples = %d, want 4", got)
	}
	score, err := restored.Score("beacon.example")
	if err != nil {
		t.Fatalf("score after restore: %v", err)
	}
	if score.State != impl.StateAllowed {
		t.Fatalf("restored allow entry was lost, state = %q", score.State)
	}
	if got := restored.Allowlist(time.Now()); len(got) != 1 || got[0] != "beacon.example" {
		t.Fatalf("restored allowlist = %v, want beacon.example", got)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	d := impl.NewC2BeaconDetector(testLimits())
	ingestAll(t, d, "beacon.example", []time.Duration{0, time.Minute, 2 * time.Minute})
	if err := d.Restore([]byte("garbage-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := d.Samples("beacon.example"); got != 3 {
		t.Fatalf("a rejected restore must not touch live windows, samples = %d", got)
	}
	if _, err := d.Score("beacon.example"); err != nil {
		t.Fatalf("scoring must keep working after a rejected restore, got %v", err)
	}
}
