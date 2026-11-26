package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-b-13"
)

func newResolver() *impl.MdnsServiceConflictResolver {
	return impl.NewMdnsServiceConflictResolver(impl.Config{MaxRenames: 3, MaxRecords: 32})
}

func printerService() impl.Service {
	return impl.Service{
		Instance: "printer",
		Type:     "_http._tcp.local",
		Host:     "lab-host.local",
		Port:     8080,
		Addr:     "192.0.2.30",
	}
}

func answer(instance string) impl.Message {
	return impl.Message{
		Instance: instance,
		Type:     "_http._tcp.local",
		Records: []impl.Record{
			{Interface: "eth0", Name: instance + "._http._tcp.local", Value: "192.0.2.31"},
		},
	}
}

func TestRegisterStartsProbing(t *testing.T) {
	r := newResolver()
	state, err := r.Register(printerService())
	if err != nil {
		t.Fatalf("register must succeed, got %v", err)
	}
	if state.Instance != "printer" || state.Current != "printer" {
		t.Fatalf("state names = %q/%q, want printer/printer", state.Instance, state.Current)
	}
	if state.Phase != impl.PhaseProbing {
		t.Fatalf("phase = %q, want %q", state.Phase, impl.PhaseProbing)
	}
	if state.Renames != 0 {
		t.Fatalf("renames = %d, want 0", state.Renames)
	}
	if state.Port != 8080 || state.Type != "_http._tcp.local" {
		t.Fatalf("state did not keep the service details: %+v", state)
	}
	stored, ok := r.StateOf("printer")
	if !ok {
		t.Fatalf("registered instance must be readable")
	}
	if stored.Current != "printer" {
		t.Fatalf("stored current = %q, want printer", stored.Current)
	}
	if got := len(r.States()); got != 1 {
		t.Fatalf("states = %d, want 1", got)
	}
}

func TestRegisterRejectsBlankService(t *testing.T) {
	r := newResolver()
	blank := printerService()
	blank.Instance = "  "
	if _, err := r.Register(blank); !errors.Is(err, impl.ErrBadService) {
		t.Fatalf("blank instance must yield ErrBadService, got %v", err)
	}
	noType := printerService()
	noType.Type = ""
	if _, err := r.Register(noType); !errors.Is(err, impl.ErrBadService) {
		t.Fatalf("blank type must yield ErrBadService, got %v", err)
	}
	if got := len(r.States()); got != 0 {
		t.Fatalf("rejected registrations must not be stored, got %d", got)
	}
}

func TestRegisterRejectsDuplicateInstance(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := r.Register(printerService()); !errors.Is(err, impl.ErrDuplicateInstance) {
		t.Fatalf("second register must yield ErrDuplicateInstance, got %v", err)
	}
	if got := len(r.States()); got != 1 {
		t.Fatalf("states = %d, want 1", got)
	}
}

func TestCommitAdvancesPhase(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Commit("printer"); err != nil {
		t.Fatalf("first commit must succeed, got %v", err)
	}
	state, ok := r.StateOf("printer")
	if !ok || state.Phase != impl.PhaseAnnouncing {
		t.Fatalf("phase after first commit = %+v ok=%v, want announcing", state, ok)
	}
	if err := r.Commit("printer"); err != nil {
		t.Fatalf("second commit must succeed, got %v", err)
	}
	state, _ = r.StateOf("printer")
	if state.Phase != impl.PhaseEstablished {
		t.Fatalf("phase after second commit = %q, want %q", state.Phase, impl.PhaseEstablished)
	}
	if err := r.Commit("printer"); !errors.Is(err, impl.ErrBadPhase) {
		t.Fatalf("commit from established must yield ErrBadPhase, got %v", err)
	}
	if state, _ := r.StateOf("printer"); state.Phase != impl.PhaseEstablished {
		t.Fatalf("rejected commit must not move the phase, got %q", state.Phase)
	}
}

func TestObserveWinningConflictRenames(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	event, err := r.Observe(answer("alpha"))
	if err != nil {
		t.Fatalf("observe must succeed, got %v", err)
	}
	if event.Kind != impl.EventRenamed {
		t.Fatalf("event kind = %q, want %q", event.Kind, impl.EventRenamed)
	}
	if event.Peer != "alpha" {
		t.Fatalf("event peer = %q, want alpha", event.Peer)
	}
	if event.Candidate != "printer-2" {
		t.Fatalf("candidate = %q, want printer-2", event.Candidate)
	}
	state, ok := r.StateOf("printer")
	if !ok {
		t.Fatalf("registration must survive a rename under its original key")
	}
	if state.Current != "printer-2" {
		t.Fatalf("current = %q, want printer-2", state.Current)
	}
	if state.Phase != impl.PhaseRenamed {
		t.Fatalf("phase = %q, want %q", state.Phase, impl.PhaseRenamed)
	}
	if state.Renames != 1 {
		t.Fatalf("renames = %d, want 1", state.Renames)
	}
	if _, ok := r.StateOf("printer-2"); ok {
		t.Fatalf("the candidate name must not become a second registration key")
	}
}

func TestObserveLosingConflictKeepsName(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	event, err := r.Observe(answer("zulu"))
	if err != nil {
		t.Fatalf("observe must succeed, got %v", err)
	}
	if event.Kind != impl.EventKept {
		t.Fatalf("event kind = %q, want %q when the peer yields", event.Kind, impl.EventKept)
	}
	state, _ := r.StateOf("printer")
	if state.Current != "printer" {
		t.Fatalf("current = %q, want the original name", state.Current)
	}
	if state.Renames != 0 {
		t.Fatalf("renames = %d, want 0", state.Renames)
	}
	if events := r.Events("printer"); len(events) != 1 || events[0].Kind != impl.EventKept {
		t.Fatalf("events = %+v, want one kept event", events)
	}
}

func TestObserveCachesForeignRecords(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	msg := impl.Message{
		Instance: "scanner",
		Type:     "_http._tcp.local",
		Records:  []impl.Record{{Interface: "eth0", Name: "scanner._http._tcp.local", Value: "192.0.2.40"}},
	}
	event, err := r.Observe(msg)
	if err != nil {
		t.Fatalf("observe must succeed, got %v", err)
	}
	if event.Kind != impl.EventCached {
		t.Fatalf("event kind = %q, want %q", event.Kind, impl.EventCached)
	}
	cache := r.Cache()
	got := cache["eth0"]
	if len(got) != 1 {
		t.Fatalf("eth0 records = %d, want 1", len(got))
	}
	if got[0].Value != "192.0.2.40" {
		t.Fatalf("cached value = %q, want 192.0.2.40", got[0].Value)
	}
	if state, _ := r.StateOf("printer"); state.Renames != 0 {
		t.Fatalf("a foreign answer must not rename our service")
	}
}

func TestObserveCacheFlushReplacesBucket(t *testing.T) {
	r := newResolver()
	first := impl.Message{
		Instance:   "scanner",
		Type:       "_http._tcp.local",
		CacheFlush: true,
		Records: []impl.Record{
			{Interface: "eth0", Name: "scanner._http._tcp.local", Value: "old"},
			{Interface: "eth1", Name: "scanner._http._tcp.local", Value: "other-iface"},
		},
	}
	if _, err := r.Observe(first); err != nil {
		t.Fatalf("first observe: %v", err)
	}
	second := impl.Message{
		Instance:   "scanner",
		Type:       "_http._tcp.local",
		CacheFlush: true,
		Records:    []impl.Record{{Interface: "eth0", Name: "scanner._http._tcp.local", Value: "new"}},
	}
	if _, err := r.Observe(second); err != nil {
		t.Fatalf("second observe: %v", err)
	}
	cache := r.Cache()
	if got := cache["eth0"]; len(got) != 1 || got[0].Value != "new" {
		t.Fatalf("eth0 bucket = %+v, want only the flushed record", got)
	}
	if got := cache["eth1"]; len(got) != 1 || got[0].Value != "other-iface" {
		t.Fatalf("eth1 bucket = %+v, want the untouched record", got)
	}
}

func TestObserveWithoutCacheFlushAppends(t *testing.T) {
	r := newResolver()
	for _, value := range []string{"one", "two"} {
		msg := impl.Message{
			Instance: "scanner",
			Type:     "_http._tcp.local",
			Records:  []impl.Record{{Interface: "eth2", Name: "scanner._http._tcp.local", Value: value}},
		}
		if _, err := r.Observe(msg); err != nil {
			t.Fatalf("observe %q: %v", value, err)
		}
	}
	got := r.Cache()["eth2"]
	if len(got) != 2 {
		t.Fatalf("eth2 records = %d, want 2 without the cache-flush bit", len(got))
	}
	if got[0].Value != "one" || got[1].Value != "two" {
		t.Fatalf("eth2 records = %+v, want one then two", got)
	}
}

func TestObserveConflictOnEstablishedRenames(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Commit("printer"); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if err := r.Commit("printer"); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	event, err := r.Observe(answer("alpha"))
	if err != nil {
		t.Fatalf("observe on an established registration: %v", err)
	}
	if event.Kind != impl.EventRenamed || event.Candidate != "printer-2" {
		t.Fatalf("event = %+v, want a rename to printer-2", event)
	}
	state, _ := r.StateOf("printer")
	if state.Phase != impl.PhaseRenamed {
		t.Fatalf("phase = %q, want %q", state.Phase, impl.PhaseRenamed)
	}
	if state.Current != "printer-2" {
		t.Fatalf("current = %q, want printer-2", state.Current)
	}
}

func TestRenameCandidateIncrementsSequence(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.Observe(answer("alpha")); err != nil {
		t.Fatalf("first conflict: %v", err)
	}
	event, err := r.Observe(answer("aardvark"))
	if err != nil {
		t.Fatalf("second conflict: %v", err)
	}
	if event.Candidate != "printer-3" {
		t.Fatalf("second candidate = %q, want printer-3", event.Candidate)
	}
	state, _ := r.StateOf("printer")
	if state.Current != "printer-3" {
		t.Fatalf("current = %q, want printer-3", state.Current)
	}
	if state.Renames != 2 {
		t.Fatalf("renames = %d, want 2", state.Renames)
	}
}

func TestWithdrawRemovesInstance(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Withdraw("printer"); err != nil {
		t.Fatalf("withdraw must succeed, got %v", err)
	}
	if _, ok := r.StateOf("printer"); ok {
		t.Fatalf("withdrawn instance must be gone")
	}
	if got := len(r.States()); got != 0 {
		t.Fatalf("states = %d, want 0", got)
	}
	if err := r.Withdraw("printer"); !errors.Is(err, impl.ErrUnknownInstance) {
		t.Fatalf("second withdraw must yield ErrUnknownInstance, got %v", err)
	}
}

func TestEventsAreSequenced(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.Observe(answer("alpha")); err != nil {
		t.Fatalf("first observe: %v", err)
	}
	if _, err := r.Observe(answer("aardvark")); err != nil {
		t.Fatalf("second observe: %v", err)
	}
	events := r.Events("printer")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Seq <= 0 || events[1].Seq <= events[0].Seq {
		t.Fatalf("event sequence = %d then %d, want strictly increasing positive numbers", events[0].Seq, events[1].Seq)
	}
	if events[0].Kind != impl.EventRenamed || events[1].Kind != impl.EventRenamed {
		t.Fatalf("event kinds = %q/%q, want two renames", events[0].Kind, events[1].Kind)
	}
}

func TestConcurrentObserveAdvancesOnce(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Observe(answer("alpha")); err != nil {
				t.Errorf("concurrent observe: %v", err)
			}
		}()
	}
	wg.Wait()
	events := r.Events("printer")
	renamed := 0
	for _, event := range events {
		if event.Kind == impl.EventRenamed {
			renamed++
		}
	}
	if renamed != 1 {
		t.Fatalf("rename events = %d, want exactly 1 for one peer name", renamed)
	}
	state, _ := r.StateOf("printer")
	if state.Renames != 1 {
		t.Fatalf("renames = %d, want 1", state.Renames)
	}
	if state.Current != "printer-2" {
		t.Fatalf("current = %q, want printer-2", state.Current)
	}
	if len(events) != 6 {
		t.Fatalf("events = %d, want one log entry per observation", len(events))
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	r := newResolver()
	if _, err := r.Register(printerService()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Commit("printer"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := r.Observe(answer("alpha")); err != nil {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := r.Observe(impl.Message{
		Instance: "scanner",
		Type:     "_http._tcp.local",
		Records:  []impl.Record{{Interface: "eth0", Name: "scanner._http._tcp.local", Value: "192.0.2.50"}},
	}); err != nil {
		t.Fatalf("foreign answer: %v", err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	fresh := newResolver()
	if err := fresh.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	state, ok := fresh.StateOf("printer")
	if !ok {
		t.Fatalf("registration not restored")
	}
	if state.Current != "printer-2" || state.Phase != impl.PhaseRenamed || state.Renames != 1 {
		t.Fatalf("restored state = %+v, want printer-2 renamed once", state)
	}
	if events := fresh.Events("printer"); len(events) != 1 || events[0].Kind != impl.EventRenamed {
		t.Fatalf("restored events = %+v, want the rename entry", events)
	}
	if got := fresh.Cache()["eth0"]; len(got) != 1 || got[0].Value != "192.0.2.50" {
		t.Fatalf("restored cache = %+v, want the scanner record", got)
	}
	if err := fresh.Restore([]byte("broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must yield ErrBadSnapshot, got %v", err)
	}
	if _, ok := fresh.StateOf("printer"); !ok {
		t.Fatalf("failed restore must not wipe the live state")
	}
}
