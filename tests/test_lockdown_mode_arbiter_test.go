package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-17"
)

func arbiterConfig() impl.Config {
	return impl.Config{MaxSurfaces: 8, CloseBudget: 100}
}

func signedActor(name string) impl.Actor {
	return impl.Actor{Name: name, Signed: true}
}

func mustSurface(t *testing.T, a *impl.LockdownArbiter, s impl.Surface) {
	t.Helper()
	if err := a.RegisterSurface(s); err != nil {
		t.Fatalf("register surface %s: %v", s.Name, err)
	}
}

func TestSetLevelRaisesAndClosesSurfaces(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	mustSurface(t, a, impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity, Reopenable: true, Cost: 1})
	mustSurface(t, a, impl.Surface{Name: "debug-probe", Kind: "debug", ClosedAt: impl.LevelConfidentiality, Reopenable: true, Cost: 2})
	mustSurface(t, a, impl.Surface{Name: "perf-counters", Kind: "metrics", ClosedAt: impl.LevelConfidentiality, Reopenable: true, Cost: 1})
	tr, err := a.SetLevel(impl.LevelIntegrity, signedActor("secops"))
	if err != nil {
		t.Fatalf("raise to integrity: %v", err)
	}
	if !tr.Applied || tr.From != impl.LevelNone || tr.To != impl.LevelIntegrity {
		t.Fatalf("transition = %+v, want an applied none to integrity move", tr)
	}
	if len(tr.Closed) != 1 || tr.Closed[0] != "audit-log" {
		t.Fatalf("closed surfaces = %v, want [audit-log]", tr.Closed)
	}
	if len(tr.Skipped) != 0 {
		t.Fatalf("skipped surfaces = %v, want none", tr.Skipped)
	}
	if tr.Cost != 1 {
		t.Fatalf("transition cost = %d, want 1", tr.Cost)
	}
	if tr.Seq != 1 {
		t.Fatalf("transition seq = %d, want 1", tr.Seq)
	}
	if got := a.Current(); got != impl.LevelIntegrity {
		t.Fatalf("current level = %d, want integrity", got)
	}
	st, err := a.Explain("audit-log")
	if err != nil {
		t.Fatalf("explain audit-log: %v", err)
	}
	if st.Open || st.CloseSeq != 1 {
		t.Fatalf("audit-log state = %+v, want closed at seq 1", st)
	}
	st, err = a.Explain("debug-probe")
	if err != nil || !st.Open || st.CloseSeq != 0 {
		t.Fatalf("debug-probe state = %+v err=%v, want open and never closed", st, err)
	}
}

func TestSetLevelSameLevelIsNoOp(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	mustSurface(t, a, impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity, Cost: 1})
	tr, err := a.SetLevel(impl.LevelNone, signedActor("secops"))
	if err != nil {
		t.Fatalf("same level request must not fail: %v", err)
	}
	if tr.Applied {
		t.Fatalf("same level request must not be applied, got %+v", tr)
	}
	if tr.Seq != 0 {
		t.Fatalf("no-op transition consumed seq %d, want 0", tr.Seq)
	}
	if len(tr.Closed) != 0 {
		t.Fatalf("no-op transition closed %v", tr.Closed)
	}
	st, _ := a.Explain("audit-log")
	if !st.Open {
		t.Fatalf("no-op transition changed the surface state: %+v", st)
	}
	tr, err = a.SetLevel(impl.LevelIntegrity, signedActor("secops"))
	if err != nil || !tr.Applied || tr.Seq != 1 {
		t.Fatalf("first real transition = %+v err=%v, want applied with seq 1", tr, err)
	}
}

func TestSetLevelBelowWatermarkRejected(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	if err := a.SetWatermark(impl.LevelIntegrity, signedActor("secops")); err != nil {
		t.Fatalf("raise watermark: %v", err)
	}
	if _, err := a.SetLevel(impl.LevelNone, signedActor("secops")); !errors.Is(err, impl.ErrLevelMonotonic) {
		t.Fatalf("downgrade below the watermark must fail with ErrLevelMonotonic, got %v", err)
	}
	if got := a.Current(); got != impl.LevelIntegrity {
		t.Fatalf("current level after the rejected downgrade = %d, want integrity", got)
	}
	tr, err := a.SetLevel(impl.LevelIntegrity, signedActor("secops"))
	if err != nil || tr.Applied {
		t.Fatalf("level equal to the watermark must be a no-op, got %+v err=%v", tr, err)
	}
}

func TestSetWatermarkRequiresSignedActor(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	if err := a.SetWatermark(impl.LevelIntegrity, impl.Actor{Name: "intern"}); !errors.Is(err, impl.ErrBadActor) {
		t.Fatalf("unsigned watermark change must fail with ErrBadActor, got %v", err)
	}
	if err := a.SetWatermark(impl.LevelIntegrity, impl.Actor{Signed: true}); !errors.Is(err, impl.ErrBadActor) {
		t.Fatalf("anonymous watermark change must fail with ErrBadActor, got %v", err)
	}
	if err := a.SetWatermark(impl.LevelConfidentiality, signedActor("secops")); err != nil {
		t.Fatalf("signed watermark change: %v", err)
	}
	if got := a.Watermark(); got != impl.LevelConfidentiality {
		t.Fatalf("watermark = %d, want confidentiality", got)
	}
	if got := a.Current(); got != impl.LevelConfidentiality {
		t.Fatalf("current level after the watermark raise = %d, want confidentiality", got)
	}
	tr, err := a.SetLevel(impl.LevelConfidentiality, signedActor("secops"))
	if err != nil || tr.Applied {
		t.Fatalf("level forced by the watermark must already be active, got %+v err=%v", tr, err)
	}
}

func TestSetWatermarkCannotLower(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	if err := a.SetWatermark(impl.LevelConfidentiality, signedActor("secops")); err != nil {
		t.Fatalf("raise watermark: %v", err)
	}
	if err := a.SetWatermark(impl.LevelIntegrity, signedActor("secops")); !errors.Is(err, impl.ErrLevelMonotonic) {
		t.Fatalf("lowering the watermark must fail with ErrLevelMonotonic, got %v", err)
	}
	if got := a.Watermark(); got != impl.LevelConfidentiality {
		t.Fatalf("watermark = %d after the rejected lowering, want confidentiality", got)
	}
	if err := a.SetWatermark(impl.Level(7), signedActor("secops")); !errors.Is(err, impl.ErrInvalidLevel) {
		t.Fatalf("out of range watermark must fail with ErrInvalidLevel, got %v", err)
	}
	if _, err := a.SetLevel(impl.Level(-1), signedActor("secops")); !errors.Is(err, impl.ErrInvalidLevel) {
		t.Fatalf("negative level must fail with ErrInvalidLevel, got %v", err)
	}
}

func TestRegisterSurfaceValidation(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	cases := []struct {
		s   impl.Surface
		err error
	}{
		{impl.Surface{Name: "   ", Kind: "audit", ClosedAt: impl.LevelIntegrity}, impl.ErrBadSurface},
		{impl.Surface{Name: "audit-log", Kind: "", ClosedAt: impl.LevelIntegrity}, impl.ErrBadSurface},
		{impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity, Cost: -1}, impl.ErrBadSurface},
		{impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.Level(9)}, impl.ErrInvalidLevel},
	}
	for _, c := range cases {
		if err := a.RegisterSurface(c.s); !errors.Is(err, c.err) {
			t.Fatalf("register %+v = %v, want %v", c.s, err, c.err)
		}
	}
	if got := len(a.Surfaces()); got != 0 {
		t.Fatalf("rejected surfaces left %d records, want 0", got)
	}
	mustSurface(t, a, impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity})
	if err := a.RegisterSurface(impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelConfidentiality}); !errors.Is(err, impl.ErrSurfaceExists) {
		t.Fatalf("duplicate surface must fail with ErrSurfaceExists, got %v", err)
	}
}

func TestRegisterSurfaceLimit(t *testing.T) {
	a := impl.NewLockdownArbiter(impl.Config{MaxSurfaces: 2, CloseBudget: 10})
	mustSurface(t, a, impl.Surface{Name: "alpha", Kind: "debug", ClosedAt: impl.LevelIntegrity})
	mustSurface(t, a, impl.Surface{Name: "beta", Kind: "debug", ClosedAt: impl.LevelIntegrity})
	if err := a.RegisterSurface(impl.Surface{Name: "gamma", Kind: "debug", ClosedAt: impl.LevelIntegrity}); !errors.Is(err, impl.ErrSurfaceLimit) {
		t.Fatalf("third surface must fail with ErrSurfaceLimit, got %v", err)
	}
	if got := len(a.Surfaces()); got != 2 {
		t.Fatalf("surface count = %d, want the cap 2", got)
	}
}

func TestExplainAndSurfacesReportState(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	if _, err := a.Explain("audit-log"); !errors.Is(err, impl.ErrUnknownSurface) {
		t.Fatalf("explain of an unknown surface must fail with ErrUnknownSurface, got %v", err)
	}
	mustSurface(t, a, impl.Surface{Name: "zeta", Kind: "metrics", ClosedAt: impl.LevelConfidentiality, Reopenable: true, Cost: 1})
	mustSurface(t, a, impl.Surface{Name: "alpha", Kind: "audit", ClosedAt: impl.LevelIntegrity, Reopenable: true, Cost: 1})
	mustSurface(t, a, impl.Surface{Name: "mike", Kind: "debug", ClosedAt: impl.LevelConfidentiality, Cost: 1})
	names := []string{}
	for _, st := range a.Surfaces() {
		names = append(names, st.Name)
	}
	if len(names) != 3 || names[0] != "alpha" || names[1] != "mike" || names[2] != "zeta" {
		t.Fatalf("surface order = %v, want alpha,mike,zeta", names)
	}
	st, err := a.Explain("alpha")
	if err != nil {
		t.Fatalf("explain alpha: %v", err)
	}
	if !st.Open || st.Kind != "audit" || st.ClosedAt != impl.LevelIntegrity || !st.Reopenable {
		t.Fatalf("alpha state = %+v, want open audit surface closing at integrity", st)
	}
	if _, err := a.SetLevel(impl.LevelConfidentiality, signedActor("secops")); err != nil {
		t.Fatalf("raise level: %v", err)
	}
	for _, name := range []string{"alpha", "mike", "zeta"} {
		st, err := a.Explain(name)
		if err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		if st.Open {
			t.Fatalf("%s is still open at the confidentiality level", name)
		}
		if st.CloseSeq <= 0 {
			t.Fatalf("%s has close seq %d, want a positive mark", name, st.CloseSeq)
		}
	}
}

func TestTransitionBudgetSkipsExpensiveSurfaces(t *testing.T) {
	a := impl.NewLockdownArbiter(impl.Config{MaxSurfaces: 8, CloseBudget: 10})
	mustSurface(t, a, impl.Surface{Name: "cheap-guard", Kind: "debug", ClosedAt: impl.LevelIntegrity, Cost: 4})
	mustSurface(t, a, impl.Surface{Name: "expensive-trace", Kind: "trace", ClosedAt: impl.LevelIntegrity, Cost: 50})
	tr, err := a.SetLevel(impl.LevelIntegrity, signedActor("secops"))
	if err != nil {
		t.Fatalf("raise level: %v", err)
	}
	if !tr.Applied || tr.To != impl.LevelIntegrity {
		t.Fatalf("transition = %+v, want the level to be committed", tr)
	}
	if len(tr.Closed) != 1 || tr.Closed[0] != "cheap-guard" {
		t.Fatalf("closed = %v, want [cheap-guard]", tr.Closed)
	}
	if len(tr.Skipped) != 1 || tr.Skipped[0] != "expensive-trace" {
		t.Fatalf("skipped = %v, want [expensive-trace]", tr.Skipped)
	}
	if tr.Cost != 4 {
		t.Fatalf("transition cost = %d, want 4", tr.Cost)
	}
	cheap, _ := a.Explain("cheap-guard")
	if cheap.Open {
		t.Fatalf("cheap-guard must be closed")
	}
	expensive, _ := a.Explain("expensive-trace")
	if !expensive.Open {
		t.Fatalf("expensive-trace must stay open when the close budget is exhausted")
	}
}

func TestDowngradeReopensReopenableSurfaces(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	mustSurface(t, a, impl.Surface{Name: "probe-a", Kind: "debug", ClosedAt: impl.LevelConfidentiality, Reopenable: true, Cost: 1})
	mustSurface(t, a, impl.Surface{Name: "probe-b", Kind: "debug", ClosedAt: impl.LevelIntegrity, Reopenable: true, Cost: 1})
	if _, err := a.SetLevel(impl.LevelConfidentiality, signedActor("secops")); err != nil {
		t.Fatalf("raise to confidentiality: %v", err)
	}
	for _, name := range []string{"probe-a", "probe-b"} {
		if st, _ := a.Explain(name); st.Open {
			t.Fatalf("%s must be closed at the confidentiality level", name)
		}
	}
	tr, err := a.SetLevel(impl.LevelIntegrity, signedActor("secops"))
	if err != nil {
		t.Fatalf("step back to integrity: %v", err)
	}
	if !tr.Applied || tr.From != impl.LevelConfidentiality || tr.To != impl.LevelIntegrity {
		t.Fatalf("downgrade transition = %+v, want confidentiality to integrity", tr)
	}
	aState, _ := a.Explain("probe-a")
	if !aState.Open {
		t.Fatalf("probe-a must reopen on the downgrade, state = %+v", aState)
	}
	bState, _ := a.Explain("probe-b")
	if bState.Open {
		t.Fatalf("probe-b still applies at the integrity level and must stay closed, state = %+v", bState)
	}
	tr, err = a.SetLevel(impl.LevelNone, signedActor("secops"))
	if err != nil || !tr.Applied {
		t.Fatalf("step back to none = %+v err=%v, want applied", tr, err)
	}
	if st, _ := a.Explain("probe-b"); !st.Open {
		t.Fatalf("probe-b must reopen once the level drops to none, state = %+v", st)
	}
	if got := a.Current(); got != impl.LevelNone {
		t.Fatalf("current level = %d, want none", got)
	}
}

func TestSetLevelSerializesConcurrentRaises(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	var wg sync.WaitGroup
	var mu sync.Mutex
	applied, noop := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, err := a.SetLevel(impl.LevelConfidentiality, signedActor("secops"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("concurrent raise: %v", err)
				return
			}
			if tr.Applied {
				applied++
			} else {
				noop++
			}
		}()
	}
	wg.Wait()
	if applied != 1 || noop != 11 {
		t.Fatalf("concurrent raises applied=%d noop=%d, want 1 applied and 11 no-ops", applied, noop)
	}
	if got := a.Current(); got != impl.LevelConfidentiality {
		t.Fatalf("current level after the race = %d, want confidentiality", got)
	}
	tr, err := a.SetLevel(impl.LevelNone, signedActor("secops"))
	if err != nil || !tr.Applied || tr.Seq != 2 {
		t.Fatalf("transition after the race = %+v err=%v, want applied with seq 2", tr, err)
	}
}

func TestSnapshotRestoreKeepsWatermarkAndSurfaces(t *testing.T) {
	cfg := arbiterConfig()
	a := impl.NewLockdownArbiter(cfg)
	mustSurface(t, a, impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity, Cost: 1})
	mustSurface(t, a, impl.Surface{Name: "debug-probe", Kind: "debug", ClosedAt: impl.LevelConfidentiality, Cost: 1})
	if _, err := a.SetLevel(impl.LevelIntegrity, signedActor("secops")); err != nil {
		t.Fatalf("raise level: %v", err)
	}
	if err := a.SetWatermark(impl.LevelIntegrity, signedActor("secops")); err != nil {
		t.Fatalf("raise watermark: %v", err)
	}
	data, err := a.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	b := impl.NewLockdownArbiter(cfg)
	if err := b.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := b.Watermark(); got != impl.LevelIntegrity {
		t.Fatalf("restored watermark = %d, want integrity", got)
	}
	if got := b.Current(); got < impl.LevelIntegrity {
		t.Fatalf("restored level = %d, must not be below the watermark", got)
	}
	if got := len(b.Surfaces()); got != 2 {
		t.Fatalf("restored surfaces = %d, want 2", got)
	}
	st, err := b.Explain("audit-log")
	if err != nil {
		t.Fatalf("explain restored audit-log: %v", err)
	}
	if st.Open || st.CloseSeq != 1 {
		t.Fatalf("restored audit-log = %+v, want closed at seq 1", st)
	}
	if _, err := b.SetLevel(impl.LevelNone, signedActor("secops")); !errors.Is(err, impl.ErrLevelMonotonic) {
		t.Fatalf("restored arbiter must still refuse a downgrade below the watermark, got %v", err)
	}
	tr, err := b.SetLevel(impl.LevelConfidentiality, signedActor("secops"))
	if err != nil {
		t.Fatalf("raise after restore: %v", err)
	}
	if !tr.Applied || tr.Seq != 2 {
		t.Fatalf("transition after restore = %+v, want applied with seq 2", tr)
	}
}

func TestRestoreRejectsCorruptSnapshotKeepsState(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	mustSurface(t, a, impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity, Cost: 1})
	if err := a.Restore([]byte("}{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := a.Current(); got != impl.LevelNone {
		t.Fatalf("failed restore changed the level to %d", got)
	}
	if got := len(a.Surfaces()); got != 1 {
		t.Fatalf("failed restore changed the surface table, count = %d", got)
	}
	if st, err := a.Explain("audit-log"); err != nil || !st.Open {
		t.Fatalf("surface must keep working after a rejected restore, state = %+v err=%v", st, err)
	}
}

func TestRaiseDoesNotRecloseSurfaces(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	mustSurface(t, a, impl.Surface{Name: "audit-log", Kind: "audit", ClosedAt: impl.LevelIntegrity, Reopenable: true, Cost: 1})
	mustSurface(t, a, impl.Surface{Name: "debug-probe", Kind: "debug", ClosedAt: impl.LevelConfidentiality, Reopenable: true, Cost: 1})
	tr, err := a.SetLevel(impl.LevelIntegrity, signedActor("secops"))
	if err != nil || len(tr.Closed) != 1 {
		t.Fatalf("first raise = %+v err=%v, want audit-log closed", tr, err)
	}
	tr, err = a.SetLevel(impl.LevelConfidentiality, signedActor("secops"))
	if err != nil {
		t.Fatalf("second raise: %v", err)
	}
	if len(tr.Closed) != 1 || tr.Closed[0] != "debug-probe" {
		t.Fatalf("second raise closed %v, want only debug-probe", tr.Closed)
	}
	if tr.Seq != 2 {
		t.Fatalf("second raise seq = %d, want 2", tr.Seq)
	}
	st, _ := a.Explain("audit-log")
	if st.CloseSeq != 1 {
		t.Fatalf("audit-log close seq = %d, must stay 1 and not be rewritten", st.CloseSeq)
	}
}

func TestRegisterSurfaceAtCurrentLevelIsBornClosed(t *testing.T) {
	a := impl.NewLockdownArbiter(arbiterConfig())
	if _, err := a.SetLevel(impl.LevelConfidentiality, signedActor("secops")); err != nil {
		t.Fatalf("raise level: %v", err)
	}
	mustSurface(t, a, impl.Surface{Name: "late-probe", Kind: "debug", ClosedAt: impl.LevelIntegrity, Cost: 1})
	st, err := a.Explain("late-probe")
	if err != nil {
		t.Fatalf("explain late-probe: %v", err)
	}
	if st.Open {
		t.Fatalf("a surface that is forbidden at the current level must be registered closed, state = %+v", st)
	}
	mustSurface(t, a, impl.Surface{Name: "future-probe", Kind: "debug", ClosedAt: impl.LevelConfidentiality, Reopenable: false, Cost: 1})
	future, err := a.Explain("future-probe")
	if err != nil {
		t.Fatalf("explain future-probe: %v", err)
	}
	if future.Open {
		t.Fatalf("a surface closed at the active level must be closed on admission, state = %+v", future)
	}
}
