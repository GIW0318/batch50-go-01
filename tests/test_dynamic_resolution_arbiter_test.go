package task

import (
	"errors"
	"math"
	"testing"

	impl "gsb/batch50-go-game-b-12"
)

func arbiterConfig() impl.ArbiterConfig {
	return impl.ArbiterConfig{TargetMs: 16.0, UpStreak: 3, DownStreak: 2, CooldownFrames: 0}
}

func fastSample() impl.FrameSample {
	return impl.FrameSample{FrameTimeMs: 10.0, InFlight: 0}
}

func slowSample() impl.FrameSample {
	return impl.FrameSample{FrameTimeMs: 22.0, InFlight: 0}
}

func deadBandSample() impl.FrameSample {
	return impl.FrameSample{FrameTimeMs: 16.0, InFlight: 0}
}

func newArbiter(t *testing.T, cfg impl.ArbiterConfig, viewport string, initial float64) *impl.DynamicResolutionArbiter {
	t.Helper()
	a := impl.NewDynamicResolutionArbiter(cfg)
	if err := a.AddViewport(viewport, initial); err != nil {
		t.Fatalf("add viewport %q: %v", viewport, err)
	}
	return a
}

func mustReport(t *testing.T, a *impl.DynamicResolutionArbiter, viewport string, sample impl.FrameSample) {
	t.Helper()
	if err := a.Report(viewport, sample); err != nil {
		t.Fatalf("report on %q: %v", viewport, err)
	}
}

func mustCommit(t *testing.T, a *impl.DynamicResolutionArbiter, viewport string) {
	t.Helper()
	if err := a.CommitRebuild(viewport, true); err != nil {
		t.Fatalf("commit rebuild on %q: %v", viewport, err)
	}
}

func TestAddViewportRejectsBadInput(t *testing.T) {
	a := impl.NewDynamicResolutionArbiter(arbiterConfig())
	if err := a.AddViewport("  ", 1.0); !errors.Is(err, impl.ErrBadViewport) {
		t.Fatalf("blank viewport must fail with ErrBadViewport, got %v", err)
	}
	if err := a.AddViewport("main", 0.55); !errors.Is(err, impl.ErrBadViewport) {
		t.Fatalf("off ladder scale must fail with ErrBadViewport, got %v", err)
	}
	if err := a.AddViewport("main", 1.0); err != nil {
		t.Fatalf("register main: %v", err)
	}
	if err := a.AddViewport("main", 1.0); !errors.Is(err, impl.ErrDuplicateViewport) {
		t.Fatalf("duplicate viewport must fail with ErrDuplicateViewport, got %v", err)
	}
	if err := a.AddViewport("split", 0.5); err != nil {
		t.Fatalf("register split: %v", err)
	}
	got := a.Viewports()
	if len(got) != 2 || got[0] != "main" || got[1] != "split" {
		t.Fatalf("viewports = %v, want the ascending pair [main split]", got)
	}
}

func TestScaleLadderAndRangeClamping(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 1.0)
	if got := a.Scale("main"); got != 1.0 {
		t.Fatalf("initial scale = %v, want 1.0", got)
	}
	if got := a.OverlayScale("main"); got != 1.0 {
		t.Fatalf("overlay scale = %v, want the unscaled 1.0", got)
	}
	full := a.Ladder("main")
	if len(full) != len(impl.ResolutionLadder) {
		t.Fatalf("ladder length = %d, want %d", len(full), len(impl.ResolutionLadder))
	}
	for i := 1; i < len(full); i++ {
		if full[i] <= full[i-1] {
			t.Fatalf("ladder must ascend, got %v", full)
		}
	}
	if err := a.SetRange("main", 0.6, 0.9); err != nil {
		t.Fatalf("set range: %v", err)
	}
	narrow := a.Ladder("main")
	want := []float64{0.6, 0.7, 0.8, 0.9}
	if len(narrow) != len(want) {
		t.Fatalf("narrow ladder = %v, want %v", narrow, want)
	}
	for i := range want {
		if narrow[i] != want[i] {
			t.Fatalf("narrow ladder = %v, want %v", narrow, want)
		}
	}
	if got := a.Scale("main"); got != 0.9 {
		t.Fatalf("scale after narrowing = %v, want it clamped to the upper bound 0.9", got)
	}
	if err := a.SetRange("main", 0.8, 0.7); !errors.Is(err, impl.ErrBadRange) {
		t.Fatalf("inverted range must fail with ErrBadRange, got %v", err)
	}
	if err := a.SetRange("main", 0.8, 0.85); !errors.Is(err, impl.ErrBadRange) {
		t.Fatalf("off ladder bound must fail with ErrBadRange, got %v", err)
	}
	if err := a.SetRange("ghost", 0.5, 1.0); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
	a2 := newArbiter(t, arbiterConfig(), "low", 0.6)
	if err := a2.SetRange("low", 0.8, 1.0); err != nil {
		t.Fatalf("raise range: %v", err)
	}
	if got := a2.Scale("low"); got != 0.8 {
		t.Fatalf("scale after raising the floor = %v, want it clamped up to 0.8", got)
	}
}

func TestReportRejectsBadSamples(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 1.0)
	if err := a.Report("main", impl.FrameSample{FrameTimeMs: 0}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("zero frame time must fail with ErrBadSample, got %v", err)
	}
	if err := a.Report("main", impl.FrameSample{FrameTimeMs: -3}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("negative frame time must fail with ErrBadSample, got %v", err)
	}
	if err := a.Report("main", impl.FrameSample{FrameTimeMs: math.NaN()}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("NaN frame time must fail with ErrBadSample, got %v", err)
	}
	if err := a.Report("main", impl.FrameSample{FrameTimeMs: 10, InFlight: -1}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("negative in flight count must fail with ErrBadSample, got %v", err)
	}
	if err := a.Report("ghost", fastSample()); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
	if got := a.Stats("main").Reports; got != 0 {
		t.Fatalf("rejected samples were counted: reports = %d, want 0", got)
	}
}

func TestUpShiftNeedsFullStreak(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 0.8)
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", fastSample())
	if req, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("two fast samples must not queue a rebuild, got %+v", req)
	}
	mustReport(t, a, "main", fastSample())
	req, ok := a.PendingRebuild("main")
	if !ok {
		t.Fatalf("three fast samples must queue a rebuild")
	}
	if req.From != 0.8 || req.To != 0.9 {
		t.Fatalf("rebuild request = %+v, want 0.8 to 0.9", req)
	}
	if got := a.Scale("main"); got != 0.8 {
		t.Fatalf("scale before the commit = %v, want the old tier 0.8", got)
	}
	mustCommit(t, a, "main")
	if got := a.Scale("main"); got != 0.9 {
		t.Fatalf("scale after the commit = %v, want 0.9", got)
	}
	st := a.Stats("main")
	if st.UpShifts != 1 || st.DownShifts != 0 || st.Rebuilds != 1 {
		t.Fatalf("stats after one up shift = %+v, want one up shift and one rebuild", st)
	}
	if _, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("the committed request must be cleared")
	}
}

func TestUpShiftStopsAtTopTier(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 1.0)
	for i := 0; i < 6; i++ {
		mustReport(t, a, "main", fastSample())
	}
	if req, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("top tier must not queue a rebuild, got %+v", req)
	}
	if got := a.Scale("main"); got != 1.0 {
		t.Fatalf("scale = %v, want it pinned at 1.0", got)
	}
	if st := a.Stats("main"); st.UpShifts != 0 {
		t.Fatalf("up shifts at the top tier = %d, want 0", st.UpShifts)
	}
}

func TestDownShiftNeedsFullStreak(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 0.8)
	mustReport(t, a, "main", slowSample())
	if _, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("one slow sample must not queue a rebuild")
	}
	mustReport(t, a, "main", slowSample())
	req, ok := a.PendingRebuild("main")
	if !ok {
		t.Fatalf("two slow samples must queue a rebuild")
	}
	if req.From != 0.8 || req.To != 0.7 {
		t.Fatalf("rebuild request = %+v, want 0.8 to 0.7", req)
	}
	mustCommit(t, a, "main")
	if got := a.Scale("main"); got != 0.7 {
		t.Fatalf("scale after the down commit = %v, want 0.7", got)
	}
	if st := a.Stats("main"); st.DownShifts != 1 || st.UpShifts != 0 {
		t.Fatalf("stats = %+v, want exactly one down shift", st)
	}
}

func TestStreakResetsInsideDeadBand(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 0.8)
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", deadBandSample())
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", fastSample())
	if req, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("a dead band sample must break the streak, got %+v", req)
	}
	if st := a.Stats("main"); st.StreakUp != 2 {
		t.Fatalf("streak after the break = %d, want the two newest samples only", st.StreakUp)
	}
}

func TestCooldownGatesUpShifts(t *testing.T) {
	cfg := impl.ArbiterConfig{TargetMs: 16, UpStreak: 1, DownStreak: 2, CooldownFrames: 3}
	a := newArbiter(t, cfg, "main", 0.6)
	for i := 0; i < 4; i++ {
		mustReport(t, a, "main", fastSample())
		if _, ok := a.PendingRebuild("main"); ok {
			mustCommit(t, a, "main")
		}
	}
	if got := a.Scale("main"); got != 0.8 {
		t.Fatalf("scale after four cooled down samples = %v, want 0.8", got)
	}
	if st := a.Stats("main"); st.UpShifts != 2 {
		t.Fatalf("up shifts = %d, want exactly two inside the cooldown window", st.UpShifts)
	}
}

func TestFreezeKeepsTierButAccountsReports(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 0.8)
	if err := a.Freeze("main", "profile capture"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	frozen, reason := a.Frozen("main")
	if !frozen || reason != "profile capture" {
		t.Fatalf("frozen = %v reason = %q, want true and the operator note", frozen, reason)
	}
	for i := 0; i < 5; i++ {
		mustReport(t, a, "main", slowSample())
	}
	if _, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("a frozen viewport must not queue rebuilds")
	}
	if got := a.Scale("main"); got != 0.8 {
		t.Fatalf("frozen scale = %v, want it pinned at 0.8", got)
	}
	st := a.Stats("main")
	if st.Reports != 5 || st.DownShifts != 0 || !st.Frozen {
		t.Fatalf("frozen stats = %+v, want five accounted reports and no shift", st)
	}
	if err := a.Resume("main"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if frozen, _ := a.Frozen("main"); frozen {
		t.Fatalf("resume must release the freeze")
	}
	if err := a.Resume("main"); !errors.Is(err, impl.ErrNotFrozen) {
		t.Fatalf("resuming twice must fail with ErrNotFrozen, got %v", err)
	}
}

func TestFreezeRequiresReason(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 1.0)
	if err := a.Freeze("main", ""); !errors.Is(err, impl.ErrBadReason) {
		t.Fatalf("blank reason must fail with ErrBadReason, got %v", err)
	}
	if frozen, _ := a.Frozen("main"); frozen {
		t.Fatalf("a rejected freeze must leave the viewport running")
	}
	if err := a.Freeze("ghost", "capture"); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("freezing an unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
}

func TestViewportsStayIndependent(t *testing.T) {
	a := impl.NewDynamicResolutionArbiter(arbiterConfig())
	if err := a.AddViewport("left", 0.8); err != nil {
		t.Fatalf("add left: %v", err)
	}
	if err := a.AddViewport("right", 0.8); err != nil {
		t.Fatalf("add right: %v", err)
	}
	for i := 0; i < 3; i++ {
		mustReport(t, a, "left", fastSample())
	}
	mustReport(t, a, "right", slowSample())
	mustReport(t, a, "right", slowSample())
	mustCommit(t, a, "left")
	mustCommit(t, a, "right")
	if got := a.Scale("left"); got != 0.9 {
		t.Fatalf("left scale = %v, want 0.9", got)
	}
	if got := a.Scale("right"); got != 0.7 {
		t.Fatalf("right scale = %v, want 0.7", got)
	}
	if st := a.Stats("left"); st.UpShifts != 1 || st.DownShifts != 0 {
		t.Fatalf("left stats = %+v, want a single up shift", st)
	}
	if st := a.Stats("right"); st.DownShifts != 1 || st.UpShifts != 0 {
		t.Fatalf("right stats = %+v, want a single down shift", st)
	}
}

func TestCommitRebuildRules(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 0.8)
	if err := a.CommitRebuild("main", true); !errors.Is(err, impl.ErrNoRebuild) {
		t.Fatalf("commit without a request must fail with ErrNoRebuild, got %v", err)
	}
	if err := a.CommitRebuild("ghost", true); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("commit on an unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", fastSample())
	if err := a.CommitRebuild("main", false); err != nil {
		t.Fatalf("failed rebuild commit must be reported through the ok flag, got %v", err)
	}
	if got := a.Scale("main"); got != 0.8 {
		t.Fatalf("scale after a failed rebuild = %v, want the original tier 0.8", got)
	}
	if _, ok := a.PendingRebuild("main"); ok {
		t.Fatalf("a failed rebuild must drop the request")
	}
	if st := a.Stats("main"); st.UpShifts != 0 {
		t.Fatalf("a failed rebuild counted %d up shifts, want none", st.UpShifts)
	}
}

func TestSnapshotRestoreKeepsStreak(t *testing.T) {
	cfg := arbiterConfig()
	a := newArbiter(t, cfg, "main", 0.8)
	mustReport(t, a, "main", fastSample())
	mustReport(t, a, "main", fastSample())
	data, err := a.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewDynamicResolutionArbiter(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Scale("main"); got != 0.8 {
		t.Fatalf("restored scale = %v, want 0.8", got)
	}
	if got := restored.Stats("main").StreakUp; got != 2 {
		t.Fatalf("restored streak = %d, want the two carried samples", got)
	}
	mustReport(t, restored, "main", fastSample())
	if _, ok := restored.PendingRebuild("main"); !ok {
		t.Fatalf("the carried streak must complete on the next sample")
	}
	mustCommit(t, restored, "main")
	if got := restored.Scale("main"); got != 0.9 {
		t.Fatalf("scale after the carried shift = %v, want 0.9", got)
	}
	if st := restored.Stats("main"); st.Reports != 3 {
		t.Fatalf("restored report counter = %d, want 3", st.Reports)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	a := newArbiter(t, arbiterConfig(), "main", 0.9)
	mustReport(t, a, "main", fastSample())
	if err := a.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if got := a.Scale("main"); got != 0.9 {
		t.Fatalf("failed restore moved the scale to %v, want 0.9", got)
	}
	if got := a.Stats("main").Reports; got != 1 {
		t.Fatalf("failed restore changed the report counter to %d, want 1", got)
	}
}

func TestRemoveViewportDropsState(t *testing.T) {
	a := impl.NewDynamicResolutionArbiter(arbiterConfig())
	if err := a.AddViewport("main", 0.8); err != nil {
		t.Fatalf("add main: %v", err)
	}
	if err := a.AddViewport("split", 0.7); err != nil {
		t.Fatalf("add split: %v", err)
	}
	if err := a.RemoveViewport("ghost"); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("removing an unknown viewport must fail with ErrUnknownViewport, got %v", err)
	}
	if err := a.RemoveViewport("main"); err != nil {
		t.Fatalf("remove main: %v", err)
	}
	if got := a.Scale("main"); got != 0 {
		t.Fatalf("removed viewport scale = %v, want 0", got)
	}
	if got := a.Ladder("main"); len(got) != 0 {
		t.Fatalf("removed viewport ladder = %v, want empty", got)
	}
	if err := a.Report("main", fastSample()); !errors.Is(err, impl.ErrUnknownViewport) {
		t.Fatalf("reporting on a removed viewport must fail with ErrUnknownViewport, got %v", err)
	}
	if got := a.Viewports(); len(got) != 1 || got[0] != "split" {
		t.Fatalf("viewports after removal = %v, want [split]", got)
	}
}
