package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-44"
)

var clusterBase = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func clusterConfig() impl.Config {
	return impl.Config{Threshold: 50, MaxClusters: 8, MaxMembers: 8, MaxObs: 32, TTL: time.Hour}
}

func baseObservation() impl.Observation {
	return impl.Observation{
		ObsID:         "obs-1",
		Host:          "edge-1.example.test",
		CertPubKey:    "pk-aaa",
		SANs:          []string{"a.example.test"},
		HeaderOrder:   []string{"server", "date", "content-type"},
		ServerHeader:  "nginx/1.24",
		FaviconHash:   "fav-1111",
		RedirectFinal: "https://a.example.test/login",
		SeenAt:        clusterBase,
	}
}

func TestAddCreatesClusterForNewObservation(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	id, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id != "cluster-1" {
		t.Fatalf("first cluster id = %q, want cluster-1", id)
	}
	cluster, ok := c.Cluster(id)
	if !ok {
		t.Fatalf("the cluster must be readable right after Add")
	}
	if len(cluster.Members) != 1 || cluster.Members[0] != "obs-1" {
		t.Fatalf("members = %v, want [obs-1]", cluster.Members)
	}
	if cluster.Confidence != impl.MaxConfidence {
		t.Fatalf("fresh cluster confidence = %d, want %d", cluster.Confidence, impl.MaxConfidence)
	}
	if got := len(c.Clusters()); got != 1 {
		t.Fatalf("cluster count = %d, want 1", got)
	}
}

func TestAddIsIdempotentForSameObsID(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	first, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	repeat := baseObservation()
	repeat.Host = "edge-2.example.test"
	repeat.FaviconHash = "fav-9999"
	second, err := c.Add(repeat)
	if err != nil {
		t.Fatalf("repeat add: %v", err)
	}
	if second != first {
		t.Fatalf("repeated observation moved to %q, want the original %q", second, first)
	}
	if got := len(c.Members(first)); got != 1 {
		t.Fatalf("members after a repeated observation = %d, want 1", got)
	}
	if got := len(c.Clusters()); got != 1 {
		t.Fatalf("cluster count = %d, want 1", got)
	}
}

func TestAddMergesOnSharedCertificateKey(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	first, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	other := impl.Observation{
		ObsID:         "obs-2",
		Host:          "api-9.other.test",
		CertPubKey:    "pk-aaa",
		SANs:          []string{"b.example.test"},
		HeaderOrder:   []string{"date"},
		ServerHeader:  "envoy",
		FaviconHash:   "fav-2222",
		RedirectFinal: "https://b.example.test/",
		SeenAt:        clusterBase.Add(5 * time.Minute),
	}
	second, err := c.Add(other)
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	if second != first {
		t.Fatalf("an observation with the same certificate key landed in %q, want %q", second, first)
	}
	if got := c.Confidence(first); got != impl.WeightCertPubKey {
		t.Fatalf("confidence = %d, want the certificate weight %d", got, impl.WeightCertPubKey)
	}
	members := c.Members(first)
	if len(members) != 2 || members[0] != "obs-1" || members[1] != "obs-2" {
		t.Fatalf("members = %v, want the sorted pair", members)
	}
}

func TestAddStartsNewClusterForWeakOverlap(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	if _, err := c.Add(baseObservation()); err != nil {
		t.Fatalf("add: %v", err)
	}
	weak := impl.Observation{
		ObsID:         "obs-weak",
		Host:          "cdn-7.other.test",
		CertPubKey:    "pk-zzz",
		SANs:          []string{"c.other.test"},
		HeaderOrder:   []string{"date"},
		ServerHeader:  "caddy",
		FaviconHash:   "fav-3333",
		RedirectFinal: "https://a.example.test/login",
		SeenAt:        clusterBase,
	}
	id, err := c.Add(weak)
	if err != nil {
		t.Fatalf("add weak: %v", err)
	}
	if id == "cluster-1" {
		t.Fatalf("a redirect only overlap must not join the first cluster")
	}
	if got := len(c.Clusters()); got != 2 {
		t.Fatalf("cluster count = %d, want 2", got)
	}
	if got := c.Confidence(id); got != impl.MaxConfidence {
		t.Fatalf("new cluster confidence = %d, want %d", got, impl.MaxConfidence)
	}
}

func TestAddRejectsEmptyObservation(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	if _, err := c.Add(impl.Observation{}); !errors.Is(err, impl.ErrEmptyObservation) {
		t.Fatalf("an empty observation must fail with ErrEmptyObservation, got %v", err)
	}
	if _, err := c.Add(impl.Observation{ObsID: "obs-no-host"}); !errors.Is(err, impl.ErrEmptyObservation) {
		t.Fatalf("an observation without a host must fail with ErrEmptyObservation, got %v", err)
	}
	if got := len(c.Clusters()); got != 0 {
		t.Fatalf("rejected observations created %d clusters", got)
	}
}

func TestAddHonoursMemberAndClusterLimits(t *testing.T) {
	wide := impl.NewAssetFingerprintClusterer(impl.Config{Threshold: 50, MaxClusters: 2, MaxMembers: 8, MaxObs: 32, TTL: time.Hour})
	for i, host := range []string{"one.test", "two.test"} {
		obs := baseObservation()
		obs.ObsID = fmt.Sprintf("obs-wide-%d", i)
		obs.Host = host
		obs.CertPubKey = fmt.Sprintf("pk-%d", i)
		obs.FaviconHash = fmt.Sprintf("fav-%d", i)
		obs.SANs = []string{host}
		obs.RedirectFinal = "https://" + host + "/"
		if _, err := wide.Add(obs); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	third := baseObservation()
	third.ObsID = "obs-wide-2"
	third.Host = "three.test"
	third.CertPubKey = "pk-third"
	third.FaviconHash = "fav-third"
	third.SANs = []string{"three.test"}
	third.RedirectFinal = "https://three.test/"
	if _, err := wide.Add(third); !errors.Is(err, impl.ErrClusterLimit) {
		t.Fatalf("a third instance above the cluster cap must fail with ErrClusterLimit, got %v", err)
	}

	narrow := impl.NewAssetFingerprintClusterer(impl.Config{Threshold: 50, MaxClusters: 8, MaxMembers: 2, MaxObs: 32, TTL: time.Hour})
	for i := 0; i < 2; i++ {
		obs := baseObservation()
		obs.ObsID = fmt.Sprintf("obs-narrow-%d", i)
		obs.Host = fmt.Sprintf("narrow-%d.test", i)
		obs.SANs = []string{fmt.Sprintf("narrow-%d.test", i)}
		obs.FaviconHash = fmt.Sprintf("fav-narrow-%d", i)
		if _, err := narrow.Add(obs); err != nil {
			t.Fatalf("narrow add %d: %v", i, err)
		}
	}
	overflow := baseObservation()
	overflow.ObsID = "obs-narrow-2"
	overflow.Host = "narrow-2.test"
	overflow.SANs = []string{"narrow-2.test"}
	overflow.FaviconHash = "fav-narrow-2"
	if _, err := narrow.Add(overflow); !errors.Is(err, impl.ErrMemberLimit) {
		t.Fatalf("a member above the cluster cap must fail with ErrMemberLimit, got %v", err)
	}
}

func TestMergeCombinesMembersAndIsIdempotent(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	left, err := c.Add(impl.Observation{ObsID: "obs-l", Host: "l.test", CertPubKey: "pk-l", SeenAt: clusterBase})
	if err != nil {
		t.Fatalf("add left: %v", err)
	}
	right, err := c.Add(impl.Observation{ObsID: "obs-r", Host: "r.test", CertPubKey: "pk-r", SeenAt: clusterBase})
	if err != nil {
		t.Fatalf("add right: %v", err)
	}
	merged, err := c.Merge(left, right)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged == left || merged == right {
		t.Fatalf("merge must return a fresh id, got %q", merged)
	}
	members := c.Members(merged)
	if len(members) != 2 || members[0] != "obs-l" || members[1] != "obs-r" {
		t.Fatalf("merged members = %v, want [obs-l obs-r]", members)
	}
	if got := len(c.Clusters()); got != 1 {
		t.Fatalf("cluster count after merge = %d, want 1", got)
	}
	if got := c.Confidence(merged); got != 90 {
		t.Fatalf("merged confidence = %d, want 90 after one merge step", got)
	}
	same, err := c.Merge(merged, merged)
	if err != nil {
		t.Fatalf("merging a cluster with itself must be idempotent, got %v", err)
	}
	if same != merged {
		t.Fatalf("self merge returned %q, want %q", same, merged)
	}
	if got := len(c.Members(merged)); got != 2 {
		t.Fatalf("self merge changed the member count to %d", got)
	}
}

func TestMergeRejectsUnknownCluster(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	one, err := c.Add(impl.Observation{ObsID: "obs-1", Host: "one.test", CertPubKey: "pk-1", SeenAt: clusterBase})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	two, err := c.Add(impl.Observation{ObsID: "obs-2", Host: "two.test", CertPubKey: "pk-2", SeenAt: clusterBase})
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	if _, err := c.Merge(one, "cluster-404"); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("merging an unknown cluster must fail with ErrUnknownCluster, got %v", err)
	}
	if _, err := c.Merge(one, two); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := c.Merge(one, two); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("merging two retired ids must fail with ErrUnknownCluster, got %v", err)
	}
}

func TestSplitProducesSingletonClusters(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	first, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	for i := 2; i <= 3; i++ {
		obs := baseObservation()
		obs.ObsID = fmt.Sprintf("obs-%d", i)
		obs.Host = fmt.Sprintf("edge-%d.example.test", i)
		obs.SANs = []string{fmt.Sprintf("edge-%d.example.test", i)}
		obs.FaviconHash = fmt.Sprintf("fav-%d", i)
		if _, err := c.Add(obs); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if got := len(c.Members(first)); got != 3 {
		t.Fatalf("cluster members before split = %d, want 3", got)
	}
	ids, err := c.Split(first)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("split returned %d ids, want 3", len(ids))
	}
	if _, ok := c.Cluster(first); ok {
		t.Fatalf("the original cluster must be gone after a split")
	}
	if got := len(c.Clusters()); got != 3 {
		t.Fatalf("cluster count after split = %d, want 3", got)
	}
	for _, id := range ids {
		if got := len(c.Members(id)); got != 1 {
			t.Fatalf("split cluster %q has %d members, want 1", id, got)
		}
	}
	if _, err := c.Split("cluster-404"); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("splitting an unknown cluster must fail with ErrUnknownCluster, got %v", err)
	}
}

func TestExplainOrdersSignalsByWeight(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	first, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	other := baseObservation()
	other.ObsID = "obs-2"
	other.Host = "edge-2.example.test"
	other.SANs = []string{"z.example.test"}
	other.HeaderOrder = []string{"date"}
	other.ServerHeader = "envoy"
	other.RedirectFinal = "https://z.example.test/"
	if _, err := c.Add(other); err != nil {
		t.Fatalf("add second: %v", err)
	}
	signals, err := c.Explain(first)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("signal count = %d, want the certificate and favicon evidence", len(signals))
	}
	if signals[0].Name != impl.SignalCertPubKey || signals[0].Weight != impl.WeightCertPubKey {
		t.Fatalf("first signal = %+v, want cert_pubkey with weight %d", signals[0], impl.WeightCertPubKey)
	}
	if signals[1].Name != impl.SignalFavicon || signals[1].Weight != impl.WeightFavicon {
		t.Fatalf("second signal = %+v, want favicon with weight %d", signals[1], impl.WeightFavicon)
	}
	for i, signal := range signals {
		if signal.Detail == "" {
			t.Fatalf("signal %d has no detail", i)
		}
	}
	if _, err := c.Explain("cluster-404"); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("explaining an unknown cluster must fail with ErrUnknownCluster, got %v", err)
	}
}

func TestConfidenceDecaysAcrossMerges(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := c.Add(impl.Observation{
			ObsID:      fmt.Sprintf("obs-%d", i),
			Host:       fmt.Sprintf("host-%d.test", i),
			CertPubKey: fmt.Sprintf("pk-%d", i),
			SeenAt:     clusterBase,
		})
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	first, err := c.Merge(ids[0], ids[1])
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if got := c.Confidence(first); got != 90 {
		t.Fatalf("confidence after one merge = %d, want 90", got)
	}
	second, err := c.Merge(first, ids[2])
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if got := c.Confidence(second); got != 81 {
		t.Fatalf("confidence after two merges = %d, want 81", got)
	}
}

func TestEvictDropsIdleClusters(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	if _, err := c.Add(baseObservation()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := c.Add(impl.Observation{ObsID: "obs-2", Host: "two.test", CertPubKey: "pk-2", SeenAt: clusterBase}); err != nil {
		t.Fatalf("add second: %v", err)
	}
	if got := c.Evict(clusterBase.Add(30 * time.Minute)); got != 0 {
		t.Fatalf("evicted %d clusters inside the TTL window, want 0", got)
	}
	if got := len(c.Clusters()); got != 2 {
		t.Fatalf("cluster count inside the TTL window = %d, want 2", got)
	}
	if got := c.Evict(clusterBase.Add(2 * time.Hour)); got != 2 {
		t.Fatalf("evicted %d clusters past the TTL window, want 2", got)
	}
	if got := len(c.Clusters()); got != 0 {
		t.Fatalf("cluster count after eviction = %d, want 0", got)
	}
}

func TestClustersAndMembersSorted(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	first, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, id := range []string{"obs-9", "obs-2"} {
		obs := baseObservation()
		obs.ObsID = id
		obs.Host = id + ".test"
		obs.SANs = []string{id + ".test"}
		obs.FaviconHash = "fav-" + id
		if _, err := c.Add(obs); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	members := c.Members(first)
	want := []string{"obs-1", "obs-2", "obs-9"}
	if len(members) != len(want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	for i := range want {
		if members[i] != want[i] {
			t.Fatalf("members = %v, want the ascending list %v", members, want)
		}
	}
	other, err := c.Add(impl.Observation{ObsID: "obs-solo", Host: "solo.test", CertPubKey: "pk-solo", SeenAt: clusterBase})
	if err != nil {
		t.Fatalf("add solo: %v", err)
	}
	ids := c.Clusters()
	if len(ids) != 2 {
		t.Fatalf("cluster list = %v, want 2 ids", ids)
	}
	if ids[0] > ids[1] {
		t.Fatalf("cluster ids are not ascending: %v", ids)
	}
	if ids[0] != first || ids[1] != other {
		t.Fatalf("cluster ids = %v, want %q then %q", ids, first, other)
	}
}

func TestSnapshotRestoreKeepsClustersAndRejectsCorrupt(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(clusterConfig())
	first, err := c.Add(baseObservation())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	other := baseObservation()
	other.ObsID = "obs-2"
	other.Host = "edge-2.example.test"
	other.SANs = []string{"edge-2.example.test"}
	if _, err := c.Add(other); err != nil {
		t.Fatalf("add second: %v", err)
	}
	solo, err := c.Add(impl.Observation{ObsID: "obs-solo", Host: "solo.test", CertPubKey: "pk-solo", SeenAt: clusterBase})
	if err != nil {
		t.Fatalf("add solo: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewAssetFingerprintClusterer(clusterConfig())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(restored.Clusters()); got != 2 {
		t.Fatalf("restored cluster count = %d, want 2", got)
	}
	members := restored.Members(first)
	if len(members) != 2 || members[0] != "obs-1" || members[1] != "obs-2" {
		t.Fatalf("restored members = %v, want the original pair", members)
	}
	if got := restored.Confidence(first); got != c.Confidence(first) {
		t.Fatalf("restored confidence = %d, want %d", got, c.Confidence(first))
	}
	if _, err := restored.Explain(solo); err != nil {
		t.Fatalf("explain after restore: %v", err)
	}
	if err := c.Restore([]byte("{not-a-cluster-set")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(c.Clusters()); got != 2 {
		t.Fatalf("a rejected restore must keep the live clusters, got %d", got)
	}
}

func TestConcurrentAddsShareOneCluster(t *testing.T) {
	c := impl.NewAssetFingerprintClusterer(impl.Config{Threshold: 50, MaxClusters: 16, MaxMembers: 16, MaxObs: 64, TTL: time.Hour})
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := 0
	ids := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			obs := baseObservation()
			obs.ObsID = fmt.Sprintf("obs-%d", i)
			obs.Host = fmt.Sprintf("edge-%d.example.test", i)
			obs.SANs = []string{fmt.Sprintf("edge-%d.example.test", i)}
			obs.FaviconHash = fmt.Sprintf("fav-%d", i)
			id, err := c.Add(obs)
			if err != nil {
				mu.Lock()
				errs++
				mu.Unlock()
				return
			}
			mu.Lock()
			ids[i] = id
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if errs != 0 {
		t.Fatalf("%d concurrent adds failed", errs)
	}
	if got := len(c.Clusters()); got != 1 {
		t.Fatalf("concurrent adds produced %d clusters, want 1", got)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[0] {
			t.Fatalf("observation %d landed in %q, want %q", i, ids[i], ids[0])
		}
	}
	if got := len(c.Members(ids[0])); got != 8 {
		t.Fatalf("cluster members = %d, want 8", got)
	}
}
