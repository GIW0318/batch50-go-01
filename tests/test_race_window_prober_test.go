package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-42"
)

type fakeTransport struct {
	mu          sync.Mutex
	executions  map[string]int
	maxApply    int
	sent        int
	inFlight    int
	maxInFlight int
	failOrdinal map[int]bool
	delay       time.Duration
	started     chan struct{}
	hold        chan struct{}
}

func newFakeTransport(maxApply int) *fakeTransport {
	return &fakeTransport{executions: map[string]int{}, maxApply: maxApply, failOrdinal: map[int]bool{}}
}

func (f *fakeTransport) Send(req impl.Request) (impl.Response, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	delay := f.delay
	started := f.started
	hold := f.hold
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if hold != nil {
		<-hold
	}

	f.mu.Lock()
	f.inFlight--
	f.sent++
	fail := f.failOrdinal[req.Ordinal]
	applied := false
	if !fail && f.executions[req.Key] < f.maxApply {
		f.executions[req.Key]++
		applied = true
	}
	f.mu.Unlock()
	if fail {
		return impl.Response{}, fmt.Errorf("connection reset by peer")
	}
	return impl.Response{Status: 200, Body: "ok", Applied: applied}, nil
}

func (f *fakeTransport) Counter(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executions[key]
}

func (f *fakeTransport) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

func proberConfig() impl.Config {
	return impl.Config{MaxRequests: 8, MaxConcurrency: 4, MaxRetries: 2, MaxProbes: 8, Cooldown: 0}
}

func claimOperation() impl.Operation {
	return impl.Operation{Key: "claim-77", Endpoint: "https://api.example.test/claim", Method: "POST", Body: `{"item":"sword"}`, Requests: 4, Concurrency: 2}
}

func plannedProber(t *testing.T, f *fakeTransport) (*impl.RaceWindowProber, impl.Probe) {
	t.Helper()
	p := impl.NewRaceWindowProber(proberConfig())
	if err := p.SetTransport(f); err != nil {
		t.Fatalf("set transport: %v", err)
	}
	probe, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p, probe
}

func TestPlanNormalizesProbe(t *testing.T) {
	p := impl.NewRaceWindowProber(proberConfig())
	probe, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if probe.ProbeID != "probe-1" {
		t.Fatalf("first probe id = %q, want probe-1", probe.ProbeID)
	}
	if probe.State != impl.StatePlanned {
		t.Fatalf("probe state = %q, want %q", probe.State, impl.StatePlanned)
	}
	if probe.Planned != 4 || probe.Concurrency != 2 {
		t.Fatalf("probe planned=%d concurrency=%d, want 4 and 2", probe.Planned, probe.Concurrency)
	}
	if probe.Key != "claim-77" {
		t.Fatalf("probe key = %q, want claim-77", probe.Key)
	}
	second, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if second.ProbeID != "probe-2" {
		t.Fatalf("second probe id = %q, want probe-2", second.ProbeID)
	}
}

func TestPlanRejectsInvalidOperation(t *testing.T) {
	p := impl.NewRaceWindowProber(proberConfig())
	blank := claimOperation()
	blank.Key = "   "
	if _, err := p.Plan(blank); !errors.Is(err, impl.ErrBadOperation) {
		t.Fatalf("blank business key must fail with ErrBadOperation, got %v", err)
	}
	single := claimOperation()
	single.Requests = 1
	if _, err := p.Plan(single); !errors.Is(err, impl.ErrBadOperation) {
		t.Fatalf("a single request window must fail with ErrBadOperation, got %v", err)
	}
	wide := claimOperation()
	wide.Requests = 9
	if _, err := p.Plan(wide); !errors.Is(err, impl.ErrRequestLimit) {
		t.Fatalf("requests above the cap must fail with ErrRequestLimit, got %v", err)
	}
	serial := claimOperation()
	serial.Concurrency = 0
	if _, err := p.Plan(serial); !errors.Is(err, impl.ErrConcurrencyLimit) {
		t.Fatalf("zero concurrency must fail with ErrConcurrencyLimit, got %v", err)
	}
	over := claimOperation()
	over.Concurrency = 6
	if _, err := p.Plan(over); !errors.Is(err, impl.ErrConcurrencyLimit) {
		t.Fatalf("concurrency above the request count must fail with ErrConcurrencyLimit, got %v", err)
	}
}

func TestProbeLimitEnforced(t *testing.T) {
	cfg := impl.Config{MaxRequests: 8, MaxConcurrency: 4, MaxRetries: 2, MaxProbes: 2, Cooldown: 0}
	p := impl.NewRaceWindowProber(cfg)
	for i := 0; i < 2; i++ {
		if _, err := p.Plan(claimOperation()); err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
	}
	if _, err := p.Plan(claimOperation()); !errors.Is(err, impl.ErrProbeLimit) {
		t.Fatalf("third plan must fail with ErrProbeLimit, got %v", err)
	}
	if got := len(p.Probes()); got != 2 {
		t.Fatalf("planned probes = %d, want the cap 2", got)
	}
}

func TestFireSendsOneRequestPerOrdinal(t *testing.T) {
	f := newFakeTransport(4)
	p, probe := plannedProber(t, f)
	outcomes, err := p.Fire(probe.ProbeID)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(outcomes) != 4 {
		t.Fatalf("outcome count = %d, want 4", len(outcomes))
	}
	seen := map[int]bool{}
	for _, o := range outcomes {
		if o.Ordinal < 1 || o.Ordinal > 4 {
			t.Fatalf("outcome ordinal %d out of the planned range", o.Ordinal)
		}
		if seen[o.Ordinal] {
			t.Fatalf("ordinal %d reported twice", o.Ordinal)
		}
		seen[o.Ordinal] = true
		if o.Status != 200 {
			t.Fatalf("ordinal %d status = %d, want 200", o.Ordinal, o.Status)
		}
	}
	if got := p.Attempts("claim-77"); got != 4 {
		t.Fatalf("attempts for the business key = %d, want 4", got)
	}
	state, ok := p.State(probe.ProbeID)
	if !ok || state.State != impl.StateFired {
		t.Fatalf("probe state after fire = %q ok=%v, want fired", state.State, ok)
	}
}

func TestFireRequiresTransportAndPlannedState(t *testing.T) {
	p := impl.NewRaceWindowProber(proberConfig())
	probe, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := p.Fire(probe.ProbeID); !errors.Is(err, impl.ErrNoTransport) {
		t.Fatalf("fire without a transport must fail with ErrNoTransport, got %v", err)
	}
	if err := p.SetTransport(newFakeTransport(1)); err != nil {
		t.Fatalf("set transport: %v", err)
	}
	if _, err := p.Fire("probe-404"); !errors.Is(err, impl.ErrUnknownProbe) {
		t.Fatalf("unknown probe must fail with ErrUnknownProbe, got %v", err)
	}
	if _, err := p.Fire(probe.ProbeID); err != nil {
		t.Fatalf("first fire: %v", err)
	}
	if _, err := p.Fire(probe.ProbeID); !errors.Is(err, impl.ErrAlreadyFired) {
		t.Fatalf("second fire must fail with ErrAlreadyFired, got %v", err)
	}
}

func TestFireHonoursConcurrencyCap(t *testing.T) {
	f := newFakeTransport(4)
	f.delay = 2 * time.Millisecond
	p, probe := plannedProber(t, f)
	if _, err := p.Fire(probe.ProbeID); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if got := f.peak(); got > 2 {
		t.Fatalf("peak in flight requests = %d, want at most the planned concurrency 2", got)
	}
	if got := f.peak(); got != 2 {
		t.Fatalf("peak in flight requests = %d, want the window to actually run 2 side by side", got)
	}
}

func TestVerifyConfirmedOnRepeatedEffects(t *testing.T) {
	f := newFakeTransport(4)
	p, probe := plannedProber(t, f)
	if _, err := p.Fire(probe.ProbeID); err != nil {
		t.Fatalf("fire: %v", err)
	}
	v, err := p.Verify(probe.ProbeID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Result != impl.ResultConfirmed {
		t.Fatalf("result = %q, want %q", v.Result, impl.ResultConfirmed)
	}
	if v.Reason != impl.ReasonDuplicateEffects {
		t.Fatalf("reason = %q, want %q", v.Reason, impl.ReasonDuplicateEffects)
	}
	if v.Executions != 4 {
		t.Fatalf("executions = %d, want 4", v.Executions)
	}
	if v.Duplicates != 3 {
		t.Fatalf("duplicates = %d, want 3", v.Duplicates)
	}
	if v.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", v.Attempts)
	}
	if v.VerdictID != "verdict-1" {
		t.Fatalf("verdict id = %q, want verdict-1", v.VerdictID)
	}
}

func TestVerifySafeOnSingleEffect(t *testing.T) {
	f := newFakeTransport(1)
	p, probe := plannedProber(t, f)
	if _, err := p.Fire(probe.ProbeID); err != nil {
		t.Fatalf("fire: %v", err)
	}
	v, err := p.Verify(probe.ProbeID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Result != impl.ResultSafe {
		t.Fatalf("result = %q, want %q", v.Result, impl.ResultSafe)
	}
	if v.Reason != impl.ReasonSingleEffect {
		t.Fatalf("reason = %q, want %q", v.Reason, impl.ReasonSingleEffect)
	}
	if v.Executions != 1 || v.Duplicates != 0 {
		t.Fatalf("executions=%d duplicates=%d, want 1 and 0", v.Executions, v.Duplicates)
	}
}

func TestVerifyUncertainOnTransportError(t *testing.T) {
	f := newFakeTransport(4)
	f.failOrdinal[3] = true
	p, probe := plannedProber(t, f)
	outcomes, err := p.Fire(probe.ProbeID)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	failed := 0
	for _, o := range outcomes {
		if o.Err != "" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("recorded failures = %d, want the one reset request", failed)
	}
	v, err := p.Verify(probe.ProbeID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Result != impl.ResultUncertain {
		t.Fatalf("result = %q, want %q for an incomplete window", v.Result, impl.ResultUncertain)
	}
	if v.Reason != impl.ReasonIncompleteWindow {
		t.Fatalf("reason = %q, want %q", v.Reason, impl.ReasonIncompleteWindow)
	}
}

func TestVerifyRequiresFiredProbe(t *testing.T) {
	p := impl.NewRaceWindowProber(proberConfig())
	if _, err := p.Verify("probe-404"); !errors.Is(err, impl.ErrUnknownProbe) {
		t.Fatalf("unknown probe must fail with ErrUnknownProbe, got %v", err)
	}
	probe, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := p.Verify(probe.ProbeID); !errors.Is(err, impl.ErrNotFired) {
		t.Fatalf("verifying a planned probe must fail with ErrNotFired, got %v", err)
	}
}

func TestSameKeyCannotFireTwoBatches(t *testing.T) {
	f := newFakeTransport(4)
	f.started = make(chan struct{}, 1)
	f.hold = make(chan struct{})
	p, first := plannedProber(t, f)
	second, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("plan second: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.Fire(first.ProbeID)
		done <- err
	}()
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("the first window never started")
	}
	if _, err := p.Fire(second.ProbeID); !errors.Is(err, impl.ErrProbeInFlight) {
		t.Fatalf("a second batch on the same key must fail with ErrProbeInFlight, got %v", err)
	}
	close(f.hold)
	if err := <-done; err != nil {
		t.Fatalf("first fire: %v", err)
	}
	if _, err := p.Fire(second.ProbeID); err != nil {
		t.Fatalf("the same key may fire again once the first window closed, got %v", err)
	}
}

func TestCancelStopsPlannedProbe(t *testing.T) {
	p, probe := plannedProber(t, newFakeTransport(1))
	if err := p.Cancel(probe.ProbeID); err != nil {
		t.Fatalf("cancel planned probe: %v", err)
	}
	if state, ok := p.State(probe.ProbeID); !ok || state.State != impl.StateCancelled {
		t.Fatalf("cancelled probe state = %q ok=%v, want cancelled", state.State, ok)
	}
	if _, err := p.Fire(probe.ProbeID); !errors.Is(err, impl.ErrProbeCancelled) {
		t.Fatalf("firing a cancelled probe must fail with ErrProbeCancelled, got %v", err)
	}
	if err := p.Cancel(probe.ProbeID); err != nil {
		t.Fatalf("cancelling twice must be idempotent, got %v", err)
	}
	if err := p.Cancel("probe-404"); !errors.Is(err, impl.ErrUnknownProbe) {
		t.Fatalf("cancelling an unknown probe must fail with ErrUnknownProbe, got %v", err)
	}
	fired, err := p.Plan(claimOperation())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := p.Fire(fired.ProbeID); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if err := p.Cancel(fired.ProbeID); !errors.Is(err, impl.ErrAlreadyFired) {
		t.Fatalf("cancelling a fired probe must fail with ErrAlreadyFired, got %v", err)
	}
}

func TestPendingAndVerdictIDs(t *testing.T) {
	f := newFakeTransport(2)
	p, first := plannedProber(t, f)
	second, err := p.Plan(impl.Operation{Key: "claim-88", Endpoint: "https://api.example.test/claim", Requests: 3, Concurrency: 1})
	if err != nil {
		t.Fatalf("plan second: %v", err)
	}
	if _, err := p.Fire(first.ProbeID); err != nil {
		t.Fatalf("fire first: %v", err)
	}
	if _, err := p.Fire(second.ProbeID); err != nil {
		t.Fatalf("fire second: %v", err)
	}
	if got := p.Pending(); got != 2 {
		t.Fatalf("pending probes = %d, want 2 before verification", got)
	}
	v1, err := p.Verify(first.ProbeID)
	if err != nil {
		t.Fatalf("verify first: %v", err)
	}
	v2, err := p.Verify(second.ProbeID)
	if err != nil {
		t.Fatalf("verify second: %v", err)
	}
	if got := p.Pending(); got != 0 {
		t.Fatalf("pending probes = %d, want 0 after both verdicts", got)
	}
	if v1.VerdictID != "verdict-1" || v2.VerdictID != "verdict-2" {
		t.Fatalf("verdict ids = %q and %q, want verdict-1 and verdict-2", v1.VerdictID, v2.VerdictID)
	}
}

func TestSnapshotRestoreKeepsVerdict(t *testing.T) {
	f := newFakeTransport(4)
	p, probe := plannedProber(t, f)
	if _, err := p.Fire(probe.ProbeID); err != nil {
		t.Fatalf("fire: %v", err)
	}
	v, err := p.Verify(probe.ProbeID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewRaceWindowProber(proberConfig())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	state, ok := restored.State(probe.ProbeID)
	if !ok || state.State != impl.StateFired {
		t.Fatalf("restored probe state = %q ok=%v, want fired", state.State, ok)
	}
	if got := restored.Attempts("claim-77"); got != 4 {
		t.Fatalf("restored attempts = %d, want 4", got)
	}
	again, err := restored.Verify(probe.ProbeID)
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if again.VerdictID != v.VerdictID || again.Executions != v.Executions {
		t.Fatalf("restored verdict = %+v, want id=%q executions=%d", again, v.VerdictID, v.Executions)
	}
	if got := len(restored.Probes()); got != 1 {
		t.Fatalf("restored probe list = %d, want 1", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	f := newFakeTransport(4)
	p, probe := plannedProber(t, f)
	if _, err := p.Fire(probe.ProbeID); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if _, err := p.Verify(probe.ProbeID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := p.Restore([]byte("{broken-window")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(p.Probes()); got != 1 {
		t.Fatalf("a rejected restore must keep the live probes, got %d", got)
	}
	if _, err := p.Verify(probe.ProbeID); err != nil {
		t.Fatalf("live verdict must survive a rejected restore, got %v", err)
	}
}
