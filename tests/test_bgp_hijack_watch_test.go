package task

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-b-10"
)

func watchConfig() impl.WatchConfig {
	return impl.WatchConfig{
		MaxPeersPerPrefix: 4,
		MaxAlerts:         8,
		MaxASPathLen:      6,
		HoldWindow:        300,
		FlapWindow:        600,
	}
}

func originROA() impl.ROA {
	return impl.ROA{Prefix: "198.51.100.0/24", MaxLen: 24, OriginAS: 13335, NotAfter: 4102444800}
}

func validAnnouncement(peer string, at int64) impl.Announcement {
	return impl.Announcement{
		Peer:     peer,
		Prefix:   "198.51.100.0/24",
		OriginAS: 13335,
		ASPath:   []uint32{174, 3356, 13335},
		At:       at,
	}
}

func hijackAnnouncement(peer, prefix string, at int64) impl.Announcement {
	return impl.Announcement{
		Peer:     peer,
		Prefix:   prefix,
		OriginAS: 15169,
		ASPath:   []uint32{174, 3356, 2914, 4637, 1299, 2914, 7018, 15169},
		At:       at,
	}
}

func TestAnnounceValidRouteAccepted(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	v, err := w.Announce(validAnnouncement("peer-a", 1000))
	if err != nil {
		t.Fatalf("announce a prefix covered by the roa: %v", err)
	}
	if v.Action != impl.ActionAccept {
		t.Fatalf("action = %q, want %q", v.Action, impl.ActionAccept)
	}
	if v.Score != 0 {
		t.Fatalf("score = %d, want 0 when origin matches the roa", v.Score)
	}
	if len(v.Signals) != 0 {
		t.Fatalf("signals = %v, want none", v.Signals)
	}
	if v.Seq <= 0 {
		t.Fatalf("verdict seq = %d, want a positive monotonic number", v.Seq)
	}
	routes := w.Routes("198.51.100.0/24")
	if len(routes) != 1 || routes[0].Peer != "peer-a" || routes[0].OriginAS != 13335 {
		t.Fatalf("routes = %+v, want exactly peer-a with origin 13335", routes)
	}
	if got := w.Stats().Accepted; got != 1 {
		t.Fatalf("accepted counter = %d, want 1", got)
	}
}

func TestAnnounceRejectsMalformedInput(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	badPrefix := validAnnouncement("peer-a", 1000)
	badPrefix.Prefix = "198.51.100.9/24"
	if _, err := w.Announce(badPrefix); !errors.Is(err, impl.ErrBadPrefix) {
		t.Fatalf("prefix with host bits set must fail with ErrBadPrefix, got %v", err)
	}
	badCIDR := validAnnouncement("peer-a", 1000)
	badCIDR.Prefix = "not-a-prefix"
	if _, err := w.Announce(badCIDR); !errors.Is(err, impl.ErrBadPrefix) {
		t.Fatalf("unparsable prefix must fail with ErrBadPrefix, got %v", err)
	}
	badPeer := validAnnouncement("   ", 1000)
	if _, err := w.Announce(badPeer); !errors.Is(err, impl.ErrBadPeer) {
		t.Fatalf("blank peer must fail with ErrBadPeer, got %v", err)
	}
	if got := w.Routes("198.51.100.0/24"); len(got) != 0 {
		t.Fatalf("rejected announcements must not create routes, got %+v", got)
	}
}

func TestAnnounceRejectsBrokenASPath(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	empty := validAnnouncement("peer-a", 1000)
	empty.ASPath = nil
	if _, err := w.Announce(empty); !errors.Is(err, impl.ErrBadPath) {
		t.Fatalf("empty as path must fail with ErrBadPath, got %v", err)
	}
	split := validAnnouncement("peer-a", 1000)
	split.ASPath = []uint32{174, 3356, 15169}
	if _, err := w.Announce(split); !errors.Is(err, impl.ErrBadPath) {
		t.Fatalf("path not ending in the origin as must fail with ErrBadPath, got %v", err)
	}
	if got := w.Stats().Accepted; got != 0 {
		t.Fatalf("rejected paths must not be counted as accepted, got %d", got)
	}
}

func TestOriginMismatchWithPathAnomalyAlerts(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	v, err := w.Announce(hijackAnnouncement("peer-h", "198.51.100.0/24", 1000))
	if err != nil {
		t.Fatalf("announce foreign origin: %v", err)
	}
	if v.Action != impl.ActionAlert {
		t.Fatalf("action = %q, want %q", v.Action, impl.ActionAlert)
	}
	if v.Score != impl.ScoreOriginMismatch+impl.ScorePathAnomaly {
		t.Fatalf("score = %d, want %d", v.Score, impl.ScoreOriginMismatch+impl.ScorePathAnomaly)
	}
	want := []string{impl.SignalOriginMismatch, impl.SignalPathAnomaly}
	if len(v.Signals) != len(want) {
		t.Fatalf("signals = %v, want %v", v.Signals, want)
	}
	for i, name := range want {
		if v.Signals[i] != name {
			t.Fatalf("signals = %v, want the sorted list %v", v.Signals, want)
		}
	}
	alert, ok := w.AlertState("198.51.100.0/24")
	if !ok {
		t.Fatalf("expected an open alert for the hijacked prefix")
	}
	if alert.Seq != v.Seq {
		t.Fatalf("alert seq = %d, want the verdict seq %d", alert.Seq, v.Seq)
	}
	if len(alert.Peers) != 1 || alert.Peers[0] != "peer-h" {
		t.Fatalf("alert peers = %v, want [peer-h]", alert.Peers)
	}
	if got := w.Stats().Alerted; got != 1 {
		t.Fatalf("alerted counter = %d, want 1", got)
	}
}

func TestRoutesListedPerPeerAndUpdated(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(validAnnouncement("peer-b", 1000)); err != nil {
		t.Fatalf("announce peer-b: %v", err)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1000)); err != nil {
		t.Fatalf("announce peer-a: %v", err)
	}
	routes := w.Routes("198.51.100.0/24")
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2 peers", len(routes))
	}
	if routes[0].Peer != "peer-a" || routes[1].Peer != "peer-b" {
		t.Fatalf("routes must be ordered by peer name, got %q then %q", routes[0].Peer, routes[1].Peer)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1500)); err != nil {
		t.Fatalf("re-announce peer-a: %v", err)
	}
	routes = w.Routes("198.51.100.0/24")
	if len(routes) != 2 {
		t.Fatalf("a repeated announcement must update the row, got %d rows", len(routes))
	}
	if routes[0].FirstSeen != 1000 || routes[0].LastSeen != 1500 {
		t.Fatalf("route first/last = %d/%d, want 1000/1500", routes[0].FirstSeen, routes[0].LastSeen)
	}
}

func TestWithdrawIsIdempotentAndCountsFlaps(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1000)); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if err := w.Withdraw("peer-a", "198.51.100.0/24"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := len(w.Routes("198.51.100.0/24")); got != 0 {
		t.Fatalf("routes after withdraw = %d, want 0", got)
	}
	if got := w.Stats().Withdrawn; got != 1 {
		t.Fatalf("withdrawn counter = %d, want 1", got)
	}
	if err := w.Withdraw("peer-a", "198.51.100.0/24"); err != nil {
		t.Fatalf("withdraw of a missing route must be idempotent, got %v", err)
	}
	if got := w.Stats().Withdrawn; got != 1 {
		t.Fatalf("withdrawn counter after a repeat = %d, want 1", got)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1100)); err != nil {
		t.Fatalf("re-announce after withdraw: %v", err)
	}
	if got := w.Stats().Flaps; got != 1 {
		t.Fatalf("flap counter = %d, want 1 for a re-announce inside the window", got)
	}
}

func TestDismissClosesOpenAlert(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(hijackAnnouncement("peer-h", "198.51.100.0/24", 1000)); err != nil {
		t.Fatalf("announce hijack: %v", err)
	}
	if err := w.Dismiss("198.51.100.0/24"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if _, ok := w.AlertState("198.51.100.0/24"); ok {
		t.Fatalf("dismissed alert must not stay open")
	}
	if got := w.Stats().Dismissed; got != 1 {
		t.Fatalf("dismissed counter = %d, want 1", got)
	}
	if err := w.Dismiss("198.51.100.0/24"); err != nil {
		t.Fatalf("dismiss of a missing alert must be idempotent, got %v", err)
	}
	if got := w.Stats().Dismissed; got != 1 {
		t.Fatalf("dismissed counter after a repeat = %d, want 1", got)
	}
}

func TestSweepClosesStaleAlerts(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(hijackAnnouncement("peer-h", "198.51.100.0/24", 2000)); err != nil {
		t.Fatalf("announce hijack: %v", err)
	}
	if closed := w.Sweep(2200); len(closed) != 0 {
		t.Fatalf("sweep inside the hold window closed %d alerts, want 0", len(closed))
	}
	if _, ok := w.AlertState("198.51.100.0/24"); !ok {
		t.Fatalf("alert must stay open inside the hold window")
	}
	closed := w.Sweep(2400)
	if len(closed) != 1 {
		t.Fatalf("sweep after the hold window closed %d alerts, want 1", len(closed))
	}
	if closed[0].Prefix != "198.51.100.0/24" {
		t.Fatalf("closed alert prefix = %q, want the hijacked prefix", closed[0].Prefix)
	}
	if _, ok := w.AlertState("198.51.100.0/24"); ok {
		t.Fatalf("swept alert must not stay open")
	}
	if got := w.Stats().Alerts; got != 0 {
		t.Fatalf("open alert counter = %d, want 0 after the sweep", got)
	}
}

func TestPeerLimitPerPrefix(t *testing.T) {
	cfg := watchConfig()
	cfg.MaxPeersPerPrefix = 2
	w := impl.NewBgpHijackWatch(cfg)
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	for _, peer := range []string{"peer-a", "peer-b"} {
		if _, err := w.Announce(validAnnouncement(peer, 1000)); err != nil {
			t.Fatalf("announce %s: %v", peer, err)
		}
	}
	if _, err := w.Announce(validAnnouncement("peer-c", 1000)); !errors.Is(err, impl.ErrPeerLimit) {
		t.Fatalf("third peer must hit ErrPeerLimit, got %v", err)
	}
	routes := w.Routes("198.51.100.0/24")
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want the configured cap 2", len(routes))
	}
}

func TestAlertTableCapEvictsOldest(t *testing.T) {
	cfg := watchConfig()
	cfg.MaxAlerts = 1
	w := impl.NewBgpHijackWatch(cfg)
	for _, roa := range []impl.ROA{
		{Prefix: "198.51.100.0/24", MaxLen: 24, OriginAS: 13335, NotAfter: 4102444800},
		{Prefix: "203.0.113.0/24", MaxLen: 24, OriginAS: 13335, NotAfter: 4102444800},
	} {
		if err := w.SetROA(roa); err != nil {
			t.Fatalf("set roa %s: %v", roa.Prefix, err)
		}
	}
	if _, err := w.Announce(hijackAnnouncement("peer-h", "198.51.100.0/24", 1000)); err != nil {
		t.Fatalf("announce first hijack: %v", err)
	}
	if _, err := w.Announce(hijackAnnouncement("peer-h", "203.0.113.0/24", 1010)); err != nil {
		t.Fatalf("announce second hijack: %v", err)
	}
	if got := w.Stats().Evicted; got != 1 {
		t.Fatalf("evicted counter = %d, want 1", got)
	}
	if _, ok := w.AlertState("198.51.100.0/24"); ok {
		t.Fatalf("the oldest alert must be evicted when the table is full")
	}
	if _, ok := w.AlertState("203.0.113.0/24"); !ok {
		t.Fatalf("the newest alert must stay open")
	}
	if got := w.Stats().Alerts; got != 1 {
		t.Fatalf("open alert counter = %d, want the configured cap 1", got)
	}
}

func TestConcurrentAnnounceSerialised(t *testing.T) {
	cfg := watchConfig()
	cfg.MaxPeersPerPrefix = 64
	w := impl.NewBgpHijackWatch(cfg)
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seqs := make([]int64, 0, 32)
	failures := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := w.Announce(validAnnouncement(fmt.Sprintf("peer-%02d", i), 1000))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				return
			}
			seqs = append(seqs, v.Seq)
		}(i)
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent announcements failed, want none", failures)
	}
	if len(w.Routes("198.51.100.0/24")) != 32 {
		t.Fatalf("routes = %d, want one row per peer (32)", len(w.Routes("198.51.100.0/24")))
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("verdict sequence must be strictly increasing, got %v", seqs)
		}
	}
	if got := w.Stats().Accepted; got != 32 {
		t.Fatalf("accepted counter = %d, want 32", got)
	}
}

func TestSnapshotRestoreKeepsState(t *testing.T) {
	cfg := watchConfig()
	w := impl.NewBgpHijackWatch(cfg)
	for _, roa := range []impl.ROA{
		{Prefix: "198.51.100.0/24", MaxLen: 24, OriginAS: 13335, NotAfter: 4102444800},
		{Prefix: "203.0.113.0/24", MaxLen: 24, OriginAS: 13335, NotAfter: 4102444800},
	} {
		if err := w.SetROA(roa); err != nil {
			t.Fatalf("set roa %s: %v", roa.Prefix, err)
		}
	}
	v1, err := w.Announce(validAnnouncement("peer-a", 1000))
	if err != nil {
		t.Fatalf("announce valid: %v", err)
	}
	v2, err := w.Announce(hijackAnnouncement("peer-h", "203.0.113.0/24", 1010))
	if err != nil {
		t.Fatalf("announce hijack: %v", err)
	}
	if err := w.Dismiss("203.0.113.0/24"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	data, err := w.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewBgpHijackWatch(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(restored.Routes("198.51.100.0/24")); got != 1 {
		t.Fatalf("restored routes = %d, want 1", got)
	}
	if got := len(restored.ROAs()); got != 2 {
		t.Fatalf("restored roas = %d, want 2", got)
	}
	if _, ok := restored.AlertState("203.0.113.0/24"); ok {
		t.Fatalf("a dismissed alert must not come back after a restore")
	}
	v3, err := restored.Announce(validAnnouncement("peer-b", 2000))
	if err != nil {
		t.Fatalf("announce after restore: %v", err)
	}
	if v3.Seq <= v2.Seq || v3.Seq <= v1.Seq {
		t.Fatalf("sequence after restore = %d, must continue past %d", v3.Seq, v2.Seq)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1000)); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if err := w.Restore([]byte("{\"routes\":[}]")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(w.Routes("198.51.100.0/24")); got != 1 {
		t.Fatalf("failed restore must not touch live routes, got %d", got)
	}
	if got := w.Stats().Accepted; got != 1 {
		t.Fatalf("failed restore must not touch live counters, got accepted=%d", got)
	}
}

func TestRestoreRejectsStaleSnapshot(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1000)); err != nil {
		t.Fatalf("announce: %v", err)
	}
	data, err := w.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, peer := range []string{"peer-b", "peer-c"} {
		if _, err := w.Announce(validAnnouncement(peer, 1100)); err != nil {
			t.Fatalf("announce %s: %v", peer, err)
		}
	}
	if err := w.Restore(data); !errors.Is(err, impl.ErrStaleSnapshot) {
		t.Fatalf("older snapshot must fail with ErrStaleSnapshot, got %v", err)
	}
	if got := len(w.Routes("198.51.100.0/24")); got != 3 {
		t.Fatalf("live routes after a rejected restore = %d, want 3", got)
	}
}

func TestStatsCounters(t *testing.T) {
	w := impl.NewBgpHijackWatch(watchConfig())
	if err := w.SetROA(originROA()); err != nil {
		t.Fatalf("set roa: %v", err)
	}
	if _, err := w.Announce(validAnnouncement("peer-a", 1000)); err != nil {
		t.Fatalf("announce valid: %v", err)
	}
	if _, err := w.Announce(hijackAnnouncement("peer-h", "198.51.100.0/24", 1100)); err != nil {
		t.Fatalf("announce hijack: %v", err)
	}
	stats := w.Stats()
	if stats.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", stats.Accepted)
	}
	if stats.Alerted != 1 {
		t.Fatalf("alerted = %d, want 1", stats.Alerted)
	}
	if stats.Alerts != 1 {
		t.Fatalf("open alerts = %d, want 1", stats.Alerts)
	}
	if stats.Withdrawn != 0 || stats.Dismissed != 0 {
		t.Fatalf("unexpected withdrawal counters: %+v", stats)
	}
}
