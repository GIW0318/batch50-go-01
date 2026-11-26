package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-35"
)

func baseTime() time.Time {
	return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
}

func clusterLimits() impl.Limits {
	return impl.Limits{
		MaxTrackedActivities: 8,
		MaxEventsPerActivity: 16,
		MinEventsToCluster:   2,
	}
}

func newClusterer() *impl.ScanSourceClusterer {
	return impl.NewScanSourceClusterer(clusterLimits())
}

func probe(id, source, target string, at time.Time) impl.ProbeEvent {
	return impl.ProbeEvent{ID: id, Source: source, Target: target, Port: 443, Path: "/", At: at}
}

func mustIngest(t *testing.T, c *impl.ScanSourceClusterer, ev impl.ProbeEvent) string {
	t.Helper()
	id, err := c.Ingest(ev)
	if err != nil {
		t.Fatalf("ingest %s: %v", ev.ID, err)
	}
	if id == "" {
		t.Fatalf("ingest %s returned an empty activity id", ev.ID)
	}
	return id
}

func TestIngestRejectsBlankEvent(t *testing.T) {
	c := newClusterer()
	if _, err := c.Ingest(probe("e-1", "", "10.0.0.5", baseTime())); !errors.Is(err, impl.ErrEmptyEvent) {
		t.Fatalf("event without a source must return ErrEmptyEvent, got %v", err)
	}
	if _, err := c.Ingest(probe("e-2", "10.0.0.9", "", baseTime())); !errors.Is(err, impl.ErrEmptyEvent) {
		t.Fatalf("event without a target must return ErrEmptyEvent, got %v", err)
	}
}

func TestIngestRejectsDuplicateEvent(t *testing.T) {
	c := newClusterer()
	first := mustIngest(t, c, probe("e-dup", "10.0.0.9", "10.0.0.5", baseTime()))
	if _, err := c.Ingest(probe("e-dup", "10.0.0.9", "10.0.0.6", baseTime())); !errors.Is(err, impl.ErrDuplicateEvent) {
		t.Fatalf("duplicate event id must return ErrDuplicateEvent, got %v", err)
	}
	events, err := c.Events(first)
	if err != nil {
		t.Fatalf("events of the activity must be readable, got %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stored events = %d, want the duplicate to be dropped", len(events))
	}
}

func TestIngestGroupsBySource(t *testing.T) {
	c := newClusterer()
	later := probe("e-2", "10.0.0.9", "10.0.0.5", baseTime().Add(time.Minute))
	earlier := probe("e-1", "10.0.0.9", "10.0.0.6", baseTime())
	id1 := mustIngest(t, c, later)
	id2 := mustIngest(t, c, earlier)
	if id1 != id2 {
		t.Fatalf("events of one source must share an activity, got %q and %q", id1, id2)
	}
	other := mustIngest(t, c, probe("e-3", "10.0.0.10", "10.0.0.5", baseTime()))
	if other == id1 {
		t.Fatalf("a different source must start its own activity")
	}
	events, err := c.Events(id1)
	if err != nil {
		t.Fatalf("events must be readable, got %v", err)
	}
	if len(events) != 2 || events[0].ID != "e-1" || events[1].ID != "e-2" {
		t.Fatalf("events = %+v, want e-1 then e-2 in time order", events)
	}
	if got := c.Activities(); len(got) != 2 {
		t.Fatalf("activities = %v, want two", got)
	}
}

func TestIngestEnforcesActivityLimit(t *testing.T) {
	limits := clusterLimits()
	limits.MaxTrackedActivities = 2
	c := impl.NewScanSourceClusterer(limits)
	mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.2", "10.0.0.5", baseTime()))
	if _, err := c.Ingest(probe("e-3", "10.0.0.3", "10.0.0.5", baseTime())); !errors.Is(err, impl.ErrActivityLimit) {
		t.Fatalf("third source must hit the activity limit, got %v", err)
	}
	if got := c.Activities(); len(got) != 2 {
		t.Fatalf("activities = %v, want the two already tracked", got)
	}
}

func TestIngestEnforcesEventLimit(t *testing.T) {
	limits := clusterLimits()
	limits.MaxEventsPerActivity = 2
	c := impl.NewScanSourceClusterer(limits)
	id := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.6", baseTime()))
	if _, err := c.Ingest(probe("e-3", "10.0.0.1", "10.0.0.7", baseTime())); !errors.Is(err, impl.ErrEventLimit) {
		t.Fatalf("third event of one activity must hit the event limit, got %v", err)
	}
	events, err := c.Events(id)
	if err != nil {
		t.Fatalf("events must be readable, got %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("stored events = %d, want the configured cap 2", len(events))
	}
}

func TestClusterNeedsEvidence(t *testing.T) {
	c := newClusterer()
	id := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	if _, err := c.Cluster(id); !errors.Is(err, impl.ErrInsufficientEvidence) {
		t.Fatalf("one event is not enough evidence, got %v", err)
	}
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.6", baseTime().Add(time.Second)))
	clusterID, err := c.Cluster(id)
	if err != nil {
		t.Fatalf("two events must be clusterable, got %v", err)
	}
	if clusterID == "" {
		t.Fatalf("cluster id must not be empty")
	}
	again, err := c.Cluster(id)
	if err != nil {
		t.Fatalf("repeated cluster must succeed, got %v", err)
	}
	if again != clusterID {
		t.Fatalf("repeated cluster id = %q, want the stable %q", again, clusterID)
	}
}

func TestClusterUnknownActivity(t *testing.T) {
	c := newClusterer()
	if _, err := c.Cluster("ghost"); !errors.Is(err, impl.ErrUnknownActivity) {
		t.Fatalf("cluster of an unknown activity must return ErrUnknownActivity, got %v", err)
	}
}

func TestClusterFeaturesTargets(t *testing.T) {
	c := newClusterer()
	id := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.9", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.5", baseTime().Add(time.Second)))
	mustIngest(t, c, probe("e-3", "10.0.0.1", "10.0.0.9", baseTime().Add(2*time.Second)))
	clusterID, err := c.Cluster(id)
	if err != nil {
		t.Fatalf("cluster must succeed, got %v", err)
	}
	features, err := c.Explain(clusterID)
	if err != nil {
		t.Fatalf("explain must succeed, got %v", err)
	}
	if len(features.Targets) != 2 || features.Targets[0] != "10.0.0.5" || features.Targets[1] != "10.0.0.9" {
		t.Fatalf("targets = %v, want the deduplicated ascending pair", features.Targets)
	}
	if len(features.Sources) != 1 || features.Sources[0] != "10.0.0.1" {
		t.Fatalf("sources = %v, want only 10.0.0.1", features.Sources)
	}
	if features.Events != 3 {
		t.Fatalf("events = %d, want 3", features.Events)
	}
	if len(features.Activities) != 1 || features.Activities[0] != id {
		t.Fatalf("activities = %v, want exactly %q", features.Activities, id)
	}
}

func TestMergeUnknownAndSameActivity(t *testing.T) {
	c := newClusterer()
	first := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.6", baseTime().Add(time.Second)))
	if _, err := c.Cluster(first); err != nil {
		t.Fatalf("cluster must succeed, got %v", err)
	}
	if _, err := c.Merge("ghost", first); !errors.Is(err, impl.ErrUnknownActivity) {
		t.Fatalf("merge with an unknown activity must return ErrUnknownActivity, got %v", err)
	}
	if _, err := c.Merge(first, first); !errors.Is(err, impl.ErrSameActivity) {
		t.Fatalf("merge with itself must return ErrSameActivity, got %v", err)
	}
}

func TestMergeCombinesClusters(t *testing.T) {
	c := newClusterer()
	a := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.7", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.8", baseTime().Add(time.Second)))
	b := mustIngest(t, c, probe("e-3", "10.0.0.2", "10.0.0.5", baseTime().Add(2*time.Second)))
	mustIngest(t, c, probe("e-4", "10.0.0.2", "10.0.0.6", baseTime().Add(3*time.Second)))
	clusterA, err := c.Cluster(a)
	if err != nil {
		t.Fatalf("cluster a: %v", err)
	}
	clusterB, err := c.Cluster(b)
	if err != nil {
		t.Fatalf("cluster b: %v", err)
	}
	merged, err := c.Merge(a, b)
	if err != nil {
		t.Fatalf("merge must succeed, got %v", err)
	}
	if merged != clusterA {
		t.Fatalf("merge returned %q, want the cluster of the first activity %q", merged, clusterA)
	}
	if clusterA == clusterB {
		t.Fatalf("test setup error: the two activities must start in different clusters")
	}
	features, err := c.Explain(clusterA)
	if err != nil {
		t.Fatalf("explain after merge: %v", err)
	}
	if len(features.Activities) != 2 || features.Activities[0] != a || features.Activities[1] != b {
		t.Fatalf("merged activities = %v, want %q and %q", features.Activities, a, b)
	}
	if len(features.Sources) != 2 || features.Sources[0] != "10.0.0.1" || features.Sources[1] != "10.0.0.2" {
		t.Fatalf("merged sources = %v, want both sources ascending", features.Sources)
	}
	if len(features.Targets) != 4 {
		t.Fatalf("merged targets = %v, want all four targets", features.Targets)
	}
	if features.Events != 4 {
		t.Fatalf("merged events = %d, want 4", features.Events)
	}
}

func TestCoolDownValidation(t *testing.T) {
	c := newClusterer()
	now := baseTime()
	c.SetClock(func() time.Time { return now })
	if err := c.CoolDown("ghost", time.Minute); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("cool down of an unknown cluster must return ErrUnknownCluster, got %v", err)
	}
	id := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.6", baseTime().Add(time.Second)))
	clusterID, err := c.Cluster(id)
	if err != nil {
		t.Fatalf("cluster must succeed, got %v", err)
	}
	if err := c.CoolDown(clusterID, 0); !errors.Is(err, impl.ErrBadTTL) {
		t.Fatalf("non positive ttl must return ErrBadTTL, got %v", err)
	}
	if err := c.CoolDown(clusterID, time.Minute); err != nil {
		t.Fatalf("cool down must succeed, got %v", err)
	}
	features, err := c.Explain(clusterID)
	if err != nil {
		t.Fatalf("explain must succeed, got %v", err)
	}
	if !features.CoolingUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("cooling until = %v, want %v", features.CoolingUntil, now.Add(time.Minute))
	}
	if features.Archived {
		t.Fatalf("a cooled cluster must stay live until it expires")
	}
}

func TestExpireArchivesCooledCluster(t *testing.T) {
	c := newClusterer()
	now := baseTime()
	c.SetClock(func() time.Time { return now })
	id := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.6", baseTime().Add(time.Second)))
	clusterID, err := c.Cluster(id)
	if err != nil {
		t.Fatalf("cluster must succeed, got %v", err)
	}
	if err := c.CoolDown(clusterID, time.Minute); err != nil {
		t.Fatalf("cool down must succeed, got %v", err)
	}
	now = now.Add(2 * time.Minute)
	archived, err := c.Expire()
	if err != nil {
		t.Fatalf("expire must succeed, got %v", err)
	}
	if archived != 1 {
		t.Fatalf("archived clusters = %d, want 1", archived)
	}
	if got := c.Activities(); len(got) != 0 {
		t.Fatalf("live activities = %v, want none after archiving", got)
	}
	if got := c.Archived(); len(got) != 1 || got[0] != clusterID {
		t.Fatalf("archived list = %v, want %q", got, clusterID)
	}
	features, err := c.Explain(clusterID)
	if err != nil {
		t.Fatalf("archived clusters must still be explainable, got %v", err)
	}
	if !features.Archived {
		t.Fatalf("features must report the cluster as archived")
	}
	again, err := c.Expire()
	if err != nil {
		t.Fatalf("second expire must succeed, got %v", err)
	}
	if again != 0 {
		t.Fatalf("second expire archived %d clusters, want 0", again)
	}
}

func TestEventsOrderedByTime(t *testing.T) {
	c := newClusterer()
	id := mustIngest(t, c, probe("e-late", "10.0.0.1", "10.0.0.5", baseTime().Add(2*time.Minute)))
	mustIngest(t, c, probe("e-early", "10.0.0.1", "10.0.0.6", baseTime()))
	mustIngest(t, c, probe("e-mid", "10.0.0.1", "10.0.0.7", baseTime().Add(time.Minute)))
	events, err := c.Events(id)
	if err != nil {
		t.Fatalf("events must be readable, got %v", err)
	}
	want := []string{"e-early", "e-mid", "e-late"}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want three", events)
	}
	for i, id := range want {
		if events[i].ID != id {
			t.Fatalf("event %d = %q, want %q (events must be time ordered)", i, events[i].ID, id)
		}
	}
	if _, err := c.Events("ghost"); !errors.Is(err, impl.ErrUnknownActivity) {
		t.Fatalf("events of an unknown activity must return ErrUnknownActivity, got %v", err)
	}
}

func TestSnapshotRestoreClusterAndCooldown(t *testing.T) {
	c := newClusterer()
	now := baseTime()
	c.SetClock(func() time.Time { return now })
	id := mustIngest(t, c, probe("e-1", "10.0.0.1", "10.0.0.5", baseTime()))
	mustIngest(t, c, probe("e-2", "10.0.0.1", "10.0.0.6", baseTime().Add(time.Second)))
	clusterID, err := c.Cluster(id)
	if err != nil {
		t.Fatalf("cluster must succeed, got %v", err)
	}
	if err := c.CoolDown(clusterID, 3*time.Minute); err != nil {
		t.Fatalf("cool down must succeed, got %v", err)
	}
	blob, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewScanSourceClusterer(clusterLimits())
	restored.SetClock(func() time.Time { return now })
	if err := restored.Restore(blob); err != nil {
		t.Fatalf("restore must accept its own snapshot, got %v", err)
	}
	features, err := restored.Explain(clusterID)
	if err != nil {
		t.Fatalf("explain after restore must succeed, got %v", err)
	}
	if features.Events != 2 || features.CoolingUntil.IsZero() {
		t.Fatalf("restored features = %+v, want two events and an active cool down", features)
	}
	if got := restored.Activities(); len(got) != 1 || got[0] != id {
		t.Fatalf("restored activities = %v, want %q", got, id)
	}
	events, err := restored.Events(id)
	if err != nil || len(events) != 2 {
		t.Fatalf("restored events = %+v err=%v, want two", events, err)
	}
	if err := restored.Restore([]byte("broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if got := restored.Activities(); len(got) != 1 {
		t.Fatalf("failed restore must keep the previous activities, got %v", got)
	}
}

func TestConcurrentIngestSameSource(t *testing.T) {
	limits := clusterLimits()
	limits.MaxEventsPerActivity = 100
	c := impl.NewScanSourceClusterer(limits)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := probe("e-burst-"+string(rune('a'+i)), "10.0.0.1", "10.0.0.5", baseTime().Add(time.Duration(i)*time.Second))
			if _, err := c.Ingest(ev); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 20 {
		t.Fatalf("accepted events = %d, want all 20", accepted)
	}
	if got := c.Activities(); len(got) != 1 {
		t.Fatalf("activities = %v, want exactly one for a single source", got)
	}
	events, err := c.Events(got0(c))
	if err != nil {
		t.Fatalf("events must be readable, got %v", err)
	}
	if len(events) != 20 {
		t.Fatalf("stored events = %d, want 20 regardless of arrival order", len(events))
	}
}

func got0(c *impl.ScanSourceClusterer) string {
	ids := c.Activities()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
