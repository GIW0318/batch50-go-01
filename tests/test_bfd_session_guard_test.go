package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-02"
)

var baseTime = time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)

func guardConfig() impl.GuardConfig {
	return impl.GuardConfig{
		MaxSessionsPerPeer: 2,
		MaxChainKeys:       4,
		MaxEvents:          8,
		MinDetectMult:      2,
		FlapWindow:         2 * time.Second,
	}
}

func sessionConfig() impl.SessionConfig {
	return impl.SessionConfig{
		DesiredTxMicros:  100000,
		RequiredRxMicros: 300000,
		DetectMult:       3,
		LocalDisc:        11,
		AuthKeyID:        1,
	}
}

func control(state string, at time.Time) impl.ControlPacket {
	return impl.ControlPacket{
		State:            state,
		DesiredTxMicros:  100000,
		RequiredRxMicros: 300000,
		DetectMult:       3,
		LocalDisc:        22,
		ReceivedAt:       at,
	}
}

func mustOpen(t *testing.T, g *impl.BfdSessionGuard, peer string) string {
	t.Helper()
	id, err := g.OpenSession(peer, sessionConfig())
	if err != nil {
		t.Fatalf("open session for %s: %v", peer, err)
	}
	if id == "" {
		t.Fatalf("open session returned an empty session id")
	}
	return string(id)
}

func driveUp(t *testing.T, g *impl.BfdSessionGuard, id string, at time.Time) {
	t.Helper()
	if _, err := g.OnPacket(id, control(impl.StateDown, at)); err != nil {
		t.Fatalf("packet down: %v", err)
	}
	if _, err := g.OnPacket(id, control(impl.StateInit, at.Add(10*time.Millisecond))); err != nil {
		t.Fatalf("packet init: %v", err)
	}
}

func TestOpenSessionStartsDown(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	s, err := g.Session(id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if s.PeerID != "peer-1" {
		t.Fatalf("peer = %q, want peer-1", s.PeerID)
	}
	if s.State != impl.StateDown {
		t.Fatalf("state = %q, want %q", s.State, impl.StateDown)
	}
	if s.TxMicros != 100000 {
		t.Fatalf("tx micros = %d, want 100000", s.TxMicros)
	}
	if s.DetectMicros != 900000 {
		t.Fatalf("detect micros = %d, want 900000", s.DetectMicros)
	}
	if s.AuthKeyID != 1 {
		t.Fatalf("auth key id = %d, want 1", s.AuthKeyID)
	}
}

func TestOpenSessionRejectsBadConfig(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	if _, err := g.OpenSession("   ", sessionConfig()); !errors.Is(err, impl.ErrBadConfig) {
		t.Fatalf("blank peer: err = %v, want ErrBadConfig", err)
	}
	bad := sessionConfig()
	bad.DesiredTxMicros = 0
	if _, err := g.OpenSession("peer-1", bad); !errors.Is(err, impl.ErrBadConfig) {
		t.Fatalf("zero tx interval: err = %v, want ErrBadConfig", err)
	}
	bad = sessionConfig()
	bad.DetectMult = 0
	if _, err := g.OpenSession("peer-1", bad); !errors.Is(err, impl.ErrBadConfig) {
		t.Fatalf("zero detect multiplier: err = %v, want ErrBadConfig", err)
	}
}

func TestOpenSessionEnforcesPeerSessionCap(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	mustOpen(t, g, "peer-1")
	mustOpen(t, g, "peer-1")
	if _, err := g.OpenSession("peer-1", sessionConfig()); !errors.Is(err, impl.ErrPeerSessionCap) {
		t.Fatalf("third session: err = %v, want ErrPeerSessionCap", err)
	}
	if _, err := g.OpenSession("peer-2", sessionConfig()); err != nil {
		t.Fatalf("other peer must stay unaffected, got %v", err)
	}
}

func TestOnPacketMovesDownToInit(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	tr, err := g.OnPacket(id, control(impl.StateDown, baseTime))
	if err != nil {
		t.Fatalf("packet: %v", err)
	}
	if tr.SessionID != id || tr.PeerID != "peer-1" {
		t.Fatalf("transition identity mismatch: %+v", tr)
	}
	if tr.From != impl.StateDown || tr.To != impl.StateInit {
		t.Fatalf("transition = %s -> %s, want down -> init", tr.From, tr.To)
	}
	if tr.Reason != impl.ReasonNegotiated {
		t.Fatalf("reason = %q, want %q", tr.Reason, impl.ReasonNegotiated)
	}
	if !tr.At.Equal(baseTime) {
		t.Fatalf("transition time = %v, want %v", tr.At, baseTime)
	}
	s, err := g.Session(id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if s.State != impl.StateInit {
		t.Fatalf("state = %q, want %q", s.State, impl.StateInit)
	}
}

func TestOnPacketMovesInitToUp(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	if _, err := g.OnPacket(id, control(impl.StateDown, baseTime)); err != nil {
		t.Fatalf("packet down: %v", err)
	}
	tr, err := g.OnPacket(id, control(impl.StateInit, baseTime.Add(10*time.Millisecond)))
	if err != nil {
		t.Fatalf("packet init: %v", err)
	}
	if tr.From != impl.StateInit || tr.To != impl.StateUp {
		t.Fatalf("transition = %s -> %s, want init -> up", tr.From, tr.To)
	}
	if tr.Reason != impl.ReasonPeerUp {
		t.Fatalf("reason = %q, want %q", tr.Reason, impl.ReasonPeerUp)
	}
	s, err := g.Session(id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if s.State != impl.StateUp {
		t.Fatalf("state = %q, want %q", s.State, impl.StateUp)
	}
}

func TestOnPacketRejectsUnknownSession(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	if _, err := g.OnPacket("sess-missing", control(impl.StateDown, baseTime)); !errors.Is(err, impl.ErrUnknownSession) {
		t.Fatalf("unknown session: err = %v, want ErrUnknownSession", err)
	}
}

func TestOnPacketRejectsAuthFailures(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	chain := []impl.AuthKey{{ID: 1, Generation: 1, Secret: "secret-one", ValidFrom: baseTime.Add(-time.Hour), ValidUntil: baseTime.Add(time.Hour)}}
	if err := g.SetAuthChain("peer-1", chain); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	missing := control(impl.StateDown, baseTime)
	missing.AuthKeyID = 9
	missing.AuthDigest = "secret-one"
	if _, err := g.OnPacket(id, missing); !errors.Is(err, impl.ErrAuthKey) {
		t.Fatalf("unknown key id: err = %v, want ErrAuthKey", err)
	}
	badDigest := control(impl.StateDown, baseTime)
	badDigest.AuthKeyID = 1
	badDigest.AuthDigest = "secret-two"
	if _, err := g.OnPacket(id, badDigest); !errors.Is(err, impl.ErrAuthKey) {
		t.Fatalf("bad digest: err = %v, want ErrAuthKey", err)
	}
	s, err := g.Session(id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if s.State != impl.StateDown {
		t.Fatalf("rejected packets moved the session to %q", s.State)
	}
	good := control(impl.StateDown, baseTime)
	good.AuthKeyID = 1
	good.AuthDigest = "secret-one"
	if _, err := g.OnPacket(id, good); err != nil {
		t.Fatalf("valid authenticated packet: %v", err)
	}
}

func TestTickDeclaresDownOnDetectTimeout(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	driveUp(t, g, id, baseTime)
	if got := g.Tick(baseTime.Add(500 * time.Millisecond)); len(got) != 0 {
		t.Fatalf("tick before the detection time fired %d transitions", len(got))
	}
	fired := g.Tick(baseTime.Add(10*time.Millisecond + 900*time.Millisecond))
	if len(fired) != 1 {
		t.Fatalf("tick at the detection time fired %d transitions, want 1", len(fired))
	}
	if fired[0].From != impl.StateUp || fired[0].To != impl.StateDown {
		t.Fatalf("transition = %s -> %s, want up -> down", fired[0].From, fired[0].To)
	}
	if fired[0].Reason != impl.ReasonDetect {
		t.Fatalf("reason = %q, want %q", fired[0].Reason, impl.ReasonDetect)
	}
	if got := g.FlapCount("peer-1"); got != 1 {
		t.Fatalf("flap count = %d, want 1", got)
	}
	if got := g.Tick(baseTime.Add(10*time.Millisecond + 2*time.Second)); len(got) != 0 {
		t.Fatalf("second tick on a down session fired %d transitions", len(got))
	}
}

func TestTickKeepsUpWhilePacketsArrive(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	driveUp(t, g, id, baseTime)
	at := baseTime.Add(20 * time.Millisecond)
	for i := 0; i < 4; i++ {
		at = at.Add(300 * time.Millisecond)
		if _, err := g.OnPacket(id, control(impl.StateUp, at)); err != nil {
			t.Fatalf("packet up %d: %v", i, err)
		}
		if got := g.Tick(at.Add(100 * time.Millisecond)); len(got) != 0 {
			t.Fatalf("round %d fired %d transitions while packets arrive", i, len(got))
		}
	}
	s, err := g.Session(id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if s.State != impl.StateUp {
		t.Fatalf("state = %q, want up", s.State)
	}
}

func TestSetAuthChainRejectsBadChain(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	if err := g.SetAuthChain("peer-1", nil); !errors.Is(err, impl.ErrBadChain) {
		t.Fatalf("empty chain: err = %v, want ErrBadChain", err)
	}
	dup := []impl.AuthKey{
		{ID: 1, Generation: 1, Secret: "a", ValidFrom: baseTime, ValidUntil: baseTime.Add(time.Hour)},
		{ID: 1, Generation: 2, Secret: "b", ValidFrom: baseTime, ValidUntil: baseTime.Add(time.Hour)},
	}
	if err := g.SetAuthChain("peer-1", dup); !errors.Is(err, impl.ErrBadChain) {
		t.Fatalf("duplicate key ids: err = %v, want ErrBadChain", err)
	}
	long := make([]impl.AuthKey, 0, 5)
	for i := 1; i <= 5; i++ {
		long = append(long, impl.AuthKey{ID: i, Generation: i, Secret: "k", ValidFrom: baseTime, ValidUntil: baseTime.Add(time.Hour)})
	}
	if err := g.SetAuthChain("peer-1", long); !errors.Is(err, impl.ErrChainTooLong) {
		t.Fatalf("oversized chain: err = %v, want ErrChainTooLong", err)
	}
}

func TestSetAuthChainAcceptsTwoGenerations(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	chain := []impl.AuthKey{
		{ID: 1, Generation: 1, Secret: "old-secret", ValidFrom: baseTime.Add(-2 * time.Hour), ValidUntil: baseTime.Add(time.Hour)},
		{ID: 2, Generation: 2, Secret: "new-secret", ValidFrom: baseTime.Add(30 * time.Minute), ValidUntil: baseTime.Add(3 * time.Hour)},
		{ID: 3, Generation: 0, Secret: "retired", ValidFrom: baseTime.Add(-2 * time.Hour), ValidUntil: baseTime.Add(-time.Hour)},
	}
	if err := g.SetAuthChain("peer-1", chain); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	oldGen := control(impl.StateDown, baseTime.Add(10*time.Minute))
	oldGen.AuthKeyID = 1
	oldGen.AuthDigest = "old-secret"
	if _, err := g.OnPacket(id, oldGen); err != nil {
		t.Fatalf("old generation key: %v", err)
	}
	newGen := control(impl.StateInit, baseTime.Add(40*time.Minute))
	newGen.AuthKeyID = 2
	newGen.AuthDigest = "new-secret"
	if _, err := g.OnPacket(id, newGen); err != nil {
		t.Fatalf("new generation key: %v", err)
	}
	expired := control(impl.StateInit, baseTime.Add(50*time.Minute))
	expired.AuthKeyID = 3
	expired.AuthDigest = "retired"
	if _, err := g.OnPacket(id, expired); !errors.Is(err, impl.ErrAuthKey) {
		t.Fatalf("expired key: err = %v, want ErrAuthKey", err)
	}
}

func TestNegotiatedDetectUsesLargerInterval(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	got, err := g.NegotiatedDetectMicros(id)
	if err != nil {
		t.Fatalf("negotiated detect: %v", err)
	}
	if got != 900000 {
		t.Fatalf("detect micros = %d, want 900000", got)
	}
	cfg := impl.SessionConfig{DesiredTxMicros: 500000, RequiredRxMicros: 100000, DetectMult: 2}
	slow, err := g.OpenSession("peer-2", cfg)
	if err != nil {
		t.Fatalf("open slow session: %v", err)
	}
	got, err = g.NegotiatedDetectMicros(string(slow))
	if err != nil {
		t.Fatalf("negotiated detect: %v", err)
	}
	if got != 1000000 {
		t.Fatalf("detect micros = %d, want 1000000", got)
	}
}

func TestFlapsMergedWithinWindow(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	driveUp(t, g, id, baseTime)
	first := g.Tick(baseTime.Add(10*time.Millisecond + 900*time.Millisecond))
	if len(first) != 1 || first[0].To != impl.StateDown {
		t.Fatalf("first timeout transitions = %+v", first)
	}
	if _, err := g.OnPacket(id, control(impl.StateDown, baseTime.Add(time.Second))); err != nil {
		t.Fatalf("packet down: %v", err)
	}
	if _, err := g.OnPacket(id, control(impl.StateInit, baseTime.Add(1010*time.Millisecond))); err != nil {
		t.Fatalf("packet init: %v", err)
	}
	second := g.Tick(baseTime.Add(1920 * time.Millisecond))
	if len(second) != 1 || second[0].To != impl.StateDown {
		t.Fatalf("second timeout transitions = %+v", second)
	}
	if got := g.FlapCount("peer-1"); got != 1 {
		t.Fatalf("flaps within the suppression window = %d, want 1", got)
	}
}

func TestEventsBoundedByLimit(t *testing.T) {
	cfg := guardConfig()
	cfg.MaxEvents = 4
	cfg.FlapWindow = 100 * time.Millisecond
	g := impl.NewBfdSessionGuard(cfg)
	id := mustOpen(t, g, "peer-1")
	if _, err := g.OnPacket(id, control(impl.StateDown, baseTime)); err != nil {
		t.Fatalf("packet down: %v", err)
	}
	if _, err := g.OnPacket(id, control(impl.StateInit, baseTime.Add(time.Second))); err != nil {
		t.Fatalf("packet init: %v", err)
	}
	if got := g.Tick(baseTime.Add(2 * time.Second)); len(got) != 1 {
		t.Fatalf("first timeout transitions = %d, want 1", len(got))
	}
	if _, err := g.OnPacket(id, control(impl.StateDown, baseTime.Add(3*time.Second))); err != nil {
		t.Fatalf("packet down: %v", err)
	}
	if _, err := g.OnPacket(id, control(impl.StateInit, baseTime.Add(4*time.Second))); err != nil {
		t.Fatalf("packet init: %v", err)
	}
	if got := g.Tick(baseTime.Add(5 * time.Second)); len(got) != 1 {
		t.Fatalf("second timeout transitions = %d, want 1", len(got))
	}
	events := g.Events()
	if len(events) != 4 {
		t.Fatalf("event log length = %d, want 4", len(events))
	}
	if events[0].From != impl.StateUp || events[0].To != impl.StateDown {
		t.Fatalf("oldest kept event = %s -> %s, want up -> down", events[0].From, events[0].To)
	}
	if last := events[len(events)-1]; last.Reason != impl.ReasonDetect {
		t.Fatalf("newest event reason = %q, want %q", last.Reason, impl.ReasonDetect)
	}
}

func TestSnapshotRestoreKeepsStateAndFlaps(t *testing.T) {
	g := impl.NewBfdSessionGuard(guardConfig())
	id := mustOpen(t, g, "peer-1")
	driveUp(t, g, id, baseTime)
	if got := g.Tick(baseTime.Add(10*time.Millisecond + 900*time.Millisecond)); len(got) != 1 {
		t.Fatalf("timeout transitions = %d, want 1", len(got))
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewBfdSessionGuard(guardConfig())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	s, err := restored.Session(id)
	if err != nil {
		t.Fatalf("session after restore: %v", err)
	}
	if s.State != impl.StateDown || s.PeerID != "peer-1" {
		t.Fatalf("session after restore = %+v", s)
	}
	if got := restored.FlapCount("peer-1"); got != 1 {
		t.Fatalf("flap count after restore = %d, want 1", got)
	}
	tr, err := restored.OnPacket(id, control(impl.StateDown, baseTime.Add(3*time.Second)))
	if err != nil {
		t.Fatalf("packet after restore: %v", err)
	}
	if tr.To != impl.StateInit {
		t.Fatalf("restored session moved to %q, want init", tr.To)
	}
}
