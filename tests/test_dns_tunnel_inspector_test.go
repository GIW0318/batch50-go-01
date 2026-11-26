package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-b-11"
)

func inspectorConfig() impl.InspectorConfig {
	return impl.InspectorConfig{
		Window:              300,
		MaxClients:          8,
		MaxQueriesPerWindow: 3,
		MaxQueriesPerClient: 64,
		MaxTXTLength:        180,
		MaxNameLength:       100,
		EntropyThreshold:    3.5,
	}
}

func normalQuery(clientID string, at int64) impl.Query {
	return impl.Query{ClientID: clientID, Name: "www.example.net", QType: "A", At: at}
}

func tunnelQuery(clientID string, at int64) impl.Query {
	return impl.Query{
		ClientID:  clientID,
		Name:      "x7f2k9q4m1z8p3.tunnel.example",
		QType:     "TXT",
		TXTLength: 300,
		At:        at,
	}
}

func TestObserveBenignQueryAllowed(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	v, err := d.Observe(normalQuery("client-1", 1000))
	if err != nil {
		t.Fatalf("observe a plain A query: %v", err)
	}
	if v.Action != impl.ActionAllow {
		t.Fatalf("action = %q, want %q", v.Action, impl.ActionAllow)
	}
	if v.Score != 0 {
		t.Fatalf("score = %d, want 0 for a short plain name", v.Score)
	}
	if len(v.Signals) != 0 {
		t.Fatalf("signals = %v, want none", v.Signals)
	}
	if v.Seq <= 0 {
		t.Fatalf("verdict seq = %d, want a positive number", v.Seq)
	}
	if got := d.Stats().Allowed; got != 1 {
		t.Fatalf("allowed counter = %d, want 1", got)
	}
}

func TestObserveRejectsBlankFields(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	blankClient := normalQuery("   ", 1000)
	if _, err := d.Observe(blankClient); !errors.Is(err, impl.ErrBadQuery) {
		t.Fatalf("blank client id must fail with ErrBadQuery, got %v", err)
	}
	blankName := normalQuery("client-1", 1000)
	blankName.Name = "  "
	if _, err := d.Observe(blankName); !errors.Is(err, impl.ErrBadQuery) {
		t.Fatalf("blank query name must fail with ErrBadQuery, got %v", err)
	}
	if got := len(d.Sweep(2000)); got != 0 {
		t.Fatalf("rejected queries must not create client state, got %d clients", got)
	}
}

func TestObserveRejectsMalformedName(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	for _, name := range []string{"a..b.example", "bad..example", "label-" + longLabel() + ".example"} {
		q := normalQuery("client-1", 1000)
		q.Name = name
		if _, err := d.Observe(q); !errors.Is(err, impl.ErrBadQuery) {
			t.Fatalf("malformed name %q must fail with ErrBadQuery, got %v", name, err)
		}
	}
}

func longLabel() string {
	label := ""
	for i := 0; i < 64; i++ {
		label += "a"
	}
	return label
}

func TestObserveRejectsUnsupportedQType(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	q := normalQuery("client-1", 1000)
	q.QType = "AXFR"
	if _, err := d.Observe(q); !errors.Is(err, impl.ErrBadQType) {
		t.Fatalf("unsupported query type must fail with ErrBadQType, got %v", err)
	}
	if got := d.Stats().Allowed; got != 0 {
		t.Fatalf("rejected query types must not be counted, got %d", got)
	}
}

func TestEntropyWithLongPayloadObserves(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	v, err := d.Observe(tunnelQuery("client-1", 1000))
	if err != nil {
		t.Fatalf("observe the tunnel shaped query: %v", err)
	}
	if v.Action != impl.ActionObserve {
		t.Fatalf("action = %q, want %q", v.Action, impl.ActionObserve)
	}
	if v.Score != impl.ScoreHighEntropy+impl.ScoreLongPayload {
		t.Fatalf("score = %d, want %d", v.Score, impl.ScoreHighEntropy+impl.ScoreLongPayload)
	}
	want := []string{impl.SignalHighEntropy, impl.SignalLongPayload}
	if len(v.Signals) != len(want) {
		t.Fatalf("signals = %v, want %v", v.Signals, want)
	}
	for i, name := range want {
		if v.Signals[i] != name {
			t.Fatalf("signals = %v, want the sorted list %v", v.Signals, want)
		}
	}
	if got := d.Stats().Observed; got != 1 {
		t.Fatalf("observed counter = %d, want 1", got)
	}
}

func TestHighRateAloneDoesNotBlock(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	var last impl.Verdict
	for i := 0; i < 6; i++ {
		v, err := d.Observe(normalQuery("client-1", int64(1000+i)))
		if err != nil {
			t.Fatalf("observe query %d: %v", i, err)
		}
		last = v
	}
	if last.Action != impl.ActionAllow {
		t.Fatalf("action of a fast plain client = %q, want %q", last.Action, impl.ActionAllow)
	}
	found := false
	for _, signal := range last.Signals {
		if signal == impl.SignalHighRate {
			found = true
		}
	}
	if !found {
		t.Fatalf("signals = %v, want the rate signal for the sixth query", last.Signals)
	}
	stats := d.Stats()
	if stats.Allowed != 6 || stats.Blocked != 0 {
		t.Fatalf("stats = %+v, want six allowed and no block", stats)
	}
}

func TestThreeSignalsBlockClient(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	var last impl.Verdict
	for i := 0; i < 4; i++ {
		v, err := d.Observe(tunnelQuery("client-1", int64(1000+i)))
		if err != nil {
			t.Fatalf("observe tunnel query %d: %v", i, err)
		}
		last = v
	}
	if last.Action != impl.ActionBlock {
		t.Fatalf("action = %q, want %q once all three signals are present", last.Action, impl.ActionBlock)
	}
	if last.Score != impl.ScoreHighEntropy+impl.ScoreLongPayload+impl.ScoreHighRate {
		t.Fatalf("score = %d, want 100", last.Score)
	}
	want := []string{impl.SignalHighEntropy, impl.SignalHighRate, impl.SignalLongPayload}
	if len(last.Signals) != len(want) {
		t.Fatalf("signals = %v, want %v", last.Signals, want)
	}
	for i, name := range want {
		if last.Signals[i] != name {
			t.Fatalf("signals = %v, want the sorted list %v", last.Signals, want)
		}
	}
	state, ok := d.ClientState("client-1")
	if !ok {
		t.Fatalf("expected a client window after four queries")
	}
	if state.Action != impl.ActionBlock {
		t.Fatalf("client action = %q, want %q", state.Action, impl.ActionBlock)
	}
	stats := d.Stats()
	if stats.Observed != 3 || stats.Blocked != 1 {
		t.Fatalf("stats = %+v, want three observed and one block", stats)
	}
}

func TestWhitelistedHostExemptFromScoring(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	if err := d.AllowName("x7f2k9q4m1z8p3.tunnel.example"); err != nil {
		t.Fatalf("register whitelist host: %v", err)
	}
	v, err := d.Observe(tunnelQuery("client-1", 1000))
	if err != nil {
		t.Fatalf("observe a whitelisted name: %v", err)
	}
	if v.Action != impl.ActionAllow || v.Reason != impl.ReasonWhitelisted {
		t.Fatalf("verdict = %+v, want an allow with the whitelist reason", v)
	}
	stats := d.Stats()
	if stats.Whitelisted != 1 {
		t.Fatalf("whitelisted counter = %d, want 1", stats.Whitelisted)
	}
	if stats.Allowed != 0 {
		t.Fatalf("a whitelisted query must not be scored, got allowed=%d", stats.Allowed)
	}
	if _, ok := d.ClientState("client-1"); ok {
		t.Fatalf("a whitelisted query must not open a client window")
	}
}

func TestWhitelistMatchesWholeNameOnly(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	if err := d.AllowName("corp.example"); err != nil {
		t.Fatalf("register whitelist name: %v", err)
	}
	if !d.Whitelisted("corp.example") {
		t.Fatalf("the registered name must be whitelisted")
	}
	if d.Whitelisted("notcorp.example") {
		t.Fatalf("a name that only shares the suffix must not be whitelisted")
	}
}

func TestWhitelistRejectsMalformedEntry(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	for _, name := range []string{"", "   ", "a..b.example", ".corp.example"} {
		if err := d.AllowName(name); !errors.Is(err, impl.ErrBadWhitelist) {
			t.Fatalf("whitelist entry %q must fail with ErrBadWhitelist, got %v", name, err)
		}
	}
}

func TestClientWindowAccumulates(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	for i, txt := range []int{0, 10, 20} {
		q := normalQuery("client-1", int64(100+i*10))
		q.TXTLength = txt
		if _, err := d.Observe(q); err != nil {
			t.Fatalf("observe query %d: %v", i, err)
		}
	}
	state, ok := d.ClientState("client-1")
	if !ok {
		t.Fatalf("expected a client window")
	}
	if state.Queries != 3 {
		t.Fatalf("queries = %d, want 3", state.Queries)
	}
	if state.TXTBytes != 30 {
		t.Fatalf("txt bytes = %d, want 30", state.TXTBytes)
	}
	if state.FirstSeen != 100 || state.LastSeen != 120 {
		t.Fatalf("first/last seen = %d/%d, want 100/120", state.FirstSeen, state.LastSeen)
	}
}

func TestSweepDropsIdleClients(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	if _, err := d.Observe(normalQuery("client-1", 100)); err != nil {
		t.Fatalf("observe: %v", err)
	}
	dropped := d.Sweep(401)
	if len(dropped) != 1 {
		t.Fatalf("sweep dropped %d clients, want 1 after the window", len(dropped))
	}
	if dropped[0].ClientID != "client-1" {
		t.Fatalf("dropped client = %q, want client-1", dropped[0].ClientID)
	}
	if _, ok := d.ClientState("client-1"); ok {
		t.Fatalf("a swept client must be gone from the table")
	}
	if got := len(d.Sweep(500)); got != 0 {
		t.Fatalf("a second sweep dropped %d clients, want 0", got)
	}
}

func TestClientLimitEnforced(t *testing.T) {
	cfg := inspectorConfig()
	cfg.MaxClients = 2
	d := impl.NewDnsTunnelInspector(cfg)
	for _, client := range []string{"client-1", "client-2"} {
		if _, err := d.Observe(normalQuery(client, 1000)); err != nil {
			t.Fatalf("observe %s: %v", client, err)
		}
	}
	if _, err := d.Observe(normalQuery("client-3", 1000)); !errors.Is(err, impl.ErrClientLimit) {
		t.Fatalf("third client must fail with ErrClientLimit, got %v", err)
	}
	if _, err := d.Observe(normalQuery("client-1", 1010)); err != nil {
		t.Fatalf("a tracked client must keep working at the cap, got %v", err)
	}
}

func TestConcurrentObserveCountsEveryQuery(t *testing.T) {
	d := impl.NewDnsTunnelInspector(inspectorConfig())
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Observe(normalQuery("client-1", 500)); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent queries failed, want none", failures)
	}
	state, ok := d.ClientState("client-1")
	if !ok {
		t.Fatalf("expected a client window after the concurrent load")
	}
	if state.Queries != 64 {
		t.Fatalf("queries = %d, want 64", state.Queries)
	}
	if got := d.Stats().Allowed; got != 64 {
		t.Fatalf("allowed counter = %d, want 64", got)
	}
}

func TestSnapshotRestoreKeepsState(t *testing.T) {
	cfg := inspectorConfig()
	d := impl.NewDnsTunnelInspector(cfg)
	if err := d.AllowName("ops.corp.example"); err != nil {
		t.Fatalf("register whitelist: %v", err)
	}
	last := int64(0)
	for i, txt := range []int{10, 20} {
		q := normalQuery("client-1", int64(1000+i*10))
		q.TXTLength = txt
		v, err := d.Observe(q)
		if err != nil {
			t.Fatalf("observe query %d: %v", i, err)
		}
		last = v.Seq
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewDnsTunnelInspector(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !restored.Whitelisted("ops.corp.example") {
		t.Fatalf("restored inspector lost the whitelist entry")
	}
	state, ok := restored.ClientState("client-1")
	if !ok {
		t.Fatalf("restored inspector lost the client window")
	}
	if state.Queries != 2 || state.TXTBytes != 30 {
		t.Fatalf("restored client = %+v, want 2 queries and 30 bytes", state)
	}
	if got := restored.Stats().Allowed; got != 2 {
		t.Fatalf("restored allowed counter = %d, want 2", got)
	}
	v, err := restored.Observe(normalQuery("client-2", 2000))
	if err != nil {
		t.Fatalf("observe after restore: %v", err)
	}
	if v.Seq <= last {
		t.Fatalf("sequence after restore = %d, must continue past %d", v.Seq, last)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	state, _ = restored.ClientState("client-1")
	if state.Queries != 2 {
		t.Fatalf("failed restore must not touch live windows, got queries=%d", state.Queries)
	}
}
