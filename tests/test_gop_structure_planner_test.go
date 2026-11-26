package task

import (
	"errors"
	"math"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-c-05"
)

func gopLimits() impl.Limits {
	return impl.Limits{MaxGOP: 8, MaxBFrames: 3, MaxIDRPerMinute: 600, MaxFrames: 64, NominalFPS: 25}
}

func closedProfile(maxGOP, depth int) impl.GopProfile {
	return impl.GopProfile{Mode: impl.GopClosed, MaxGOP: maxGOP, MinGOP: 1, BFrameDepth: depth}
}

func plainFrames(n int) []impl.FrameHint {
	out := make([]impl.FrameHint, n)
	for i := range out {
		out[i] = impl.FrameHint{PTS: int64(i) * 40}
	}
	return out
}

func TestDefaultPlannerState(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	profile := planner.Profile()
	if profile.Mode != impl.GopClosed {
		t.Fatalf("default mode = %q, want %q", profile.Mode, impl.GopClosed)
	}
	if profile.MaxGOP != 8 || profile.MinGOP != 1 || profile.BFrameDepth != 0 {
		t.Fatalf("default profile = %+v, want the encoder maximum with no B frames", profile)
	}
	if got := planner.Constraints().MaxFrames; got != 64 {
		t.Fatalf("constraints max frames = %d, want 64", got)
	}
	if got := planner.Stats().IDRs; got != 0 {
		t.Fatalf("stats before any plan report %d IDRs", got)
	}
	if _, ok := planner.PlanFrame(0); ok {
		t.Fatalf("an empty planner must not answer frame queries")
	}
}

func TestSetProfileValidatesMode(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	bad := closedProfile(4, 2)
	bad.Mode = "pyramid"
	if err := planner.SetProfile(bad); !errors.Is(err, impl.ErrBadMode) {
		t.Fatalf("unknown mode must fail, got %v", err)
	}
	if got := planner.Profile().Mode; got != impl.GopClosed {
		t.Fatalf("rejected profile changed the mode to %q", got)
	}
	open := closedProfile(4, 2)
	open.Mode = impl.GopOpen
	if err := planner.SetProfile(open); err != nil {
		t.Fatalf("open profile: %v", err)
	}
	if got := planner.Profile().Mode; got != impl.GopOpen {
		t.Fatalf("mode = %q, want %q", got, impl.GopOpen)
	}
}

func TestSetProfileValidatesGOP(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	zero := closedProfile(0, 0)
	if err := planner.SetProfile(zero); !errors.Is(err, impl.ErrBadGOP) {
		t.Fatalf("zero GOP length must fail, got %v", err)
	}
	inverted := closedProfile(4, 0)
	inverted.MinGOP = 5
	if err := planner.SetProfile(inverted); !errors.Is(err, impl.ErrBadGOP) {
		t.Fatalf("minimum above maximum must fail, got %v", err)
	}
	over := closedProfile(9, 0)
	if err := planner.SetProfile(over); !errors.Is(err, impl.ErrBadGOP) {
		t.Fatalf("GOP above the encoder limit must fail, got %v", err)
	}
	if got := planner.Profile().MaxGOP; got != 8 {
		t.Fatalf("rejected profiles changed the maximum to %d", got)
	}
	if err := planner.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	if got := planner.Profile().MaxGOP; got != 4 {
		t.Fatalf("maximum GOP after the change = %d, want 4", got)
	}
}

func TestSetProfileValidatesBFrames(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	negative := closedProfile(4, -1)
	if err := planner.SetProfile(negative); !errors.Is(err, impl.ErrBadBFrames) {
		t.Fatalf("negative B frame depth must fail, got %v", err)
	}
	deep := closedProfile(4, 4)
	if err := planner.SetProfile(deep); !errors.Is(err, impl.ErrBadBFrames) {
		t.Fatalf("B frame depth above three must fail, got %v", err)
	}
	limits := gopLimits()
	limits.MaxBFrames = 2
	small := impl.NewGopStructurePlanner(limits)
	if err := small.SetProfile(closedProfile(4, 3)); !errors.Is(err, impl.ErrBadBFrames) {
		t.Fatalf("depth above the encoder limit must fail, got %v", err)
	}
	if err := planner.SetProfile(closedProfile(4, 2)); err != nil {
		t.Fatalf("valid depth: %v", err)
	}
	if got := planner.Profile().BFrameDepth; got != 2 {
		t.Fatalf("depth after the change = %d, want 2", got)
	}
}

func TestPlanRejectsEmptyAndOversized(t *testing.T) {
	limits := gopLimits()
	limits.MaxFrames = 16
	planner := impl.NewGopStructurePlanner(limits)
	if err := planner.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if _, err := planner.Plan(nil); !errors.Is(err, impl.ErrEmptyPlan) {
		t.Fatalf("nil frame list must fail, got %v", err)
	}
	if _, err := planner.Plan([]impl.FrameHint{}); !errors.Is(err, impl.ErrEmptyPlan) {
		t.Fatalf("empty frame list must fail, got %v", err)
	}
	if _, err := planner.Plan(make([]impl.FrameHint, 17)); !errors.Is(err, impl.ErrFrameLimit) {
		t.Fatalf("frame list above the limit must fail, got %v", err)
	}
	if _, ok := planner.PlanFrame(0); ok {
		t.Fatalf("rejected plans must not be visible")
	}
}

func TestPlanFrameTypesAndGOPs(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 2)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	plan, err := planner.Plan(plainFrames(8))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan) != 8 {
		t.Fatalf("plan length = %d, want 8", len(plan))
	}
	wantTypes := []string{"I", "B", "B", "P", "I", "B", "B", "P"}
	wantIDR := []bool{true, false, false, false, true, false, false, false}
	wantGOP := []int{0, 0, 0, 0, 1, 1, 1, 1}
	wantPOC := []int{0, 1, 2, 3, 0, 1, 2, 3}
	for i := range plan {
		if plan[i].Index != i {
			t.Fatalf("frame %d reports index %d", i, plan[i].Index)
		}
		if plan[i].Type != wantTypes[i] {
			t.Fatalf("frame %d type = %q, want %q (plan %+v)", i, plan[i].Type, wantTypes[i], plan)
		}
		if plan[i].IDR != wantIDR[i] {
			t.Fatalf("frame %d IDR flag = %v, want %v", i, plan[i].IDR, wantIDR[i])
		}
		if plan[i].GOP != wantGOP[i] {
			t.Fatalf("frame %d gop = %d, want %d", i, plan[i].GOP, wantGOP[i])
		}
		if plan[i].POC != wantPOC[i] {
			t.Fatalf("frame %d poc = %d, want %d", i, plan[i].POC, wantPOC[i])
		}
		stored, ok := planner.PlanFrame(i)
		if !ok {
			t.Fatalf("PlanFrame(%d) is missing", i)
		}
		if stored.Type != plan[i].Type || stored.Decode != plan[i].Decode {
			t.Fatalf("PlanFrame(%d) = %+v, want the planned %+v", i, stored, plan[i])
		}
	}
}

func TestPlanDecodeOrderMapping(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 2)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	plan, err := planner.Plan(plainFrames(8))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	wantDecode := []int{0, 2, 3, 1, 4, 6, 7, 5}
	seen := make([]bool, 8)
	for i := range plan {
		if plan[i].Decode != wantDecode[i] {
			t.Fatalf("frame %d decode position = %d, want %d", i, plan[i].Decode, wantDecode[i])
		}
		if plan[i].Decode < 0 || plan[i].Decode >= 8 || seen[plan[i].Decode] {
			t.Fatalf("decode positions are not a permutation: %+v", plan)
		}
		seen[plan[i].Decode] = true
	}
	if plan[0].Ref != -1 {
		t.Fatalf("the IDR must not reference anything, got %d", plan[0].Ref)
	}
	if plan[1].Ref != 3 || plan[2].Ref != 3 {
		t.Fatalf("B frames must reference the following anchor 3, got %d and %d", plan[1].Ref, plan[2].Ref)
	}
	if plan[3].Ref != 0 {
		t.Fatalf("the anchor at 3 must reference the IDR, got %d", plan[3].Ref)
	}
	if plan[5].Ref != 7 || plan[6].Ref != 7 {
		t.Fatalf("second GOP B frames must reference 7, got %d and %d", plan[5].Ref, plan[6].Ref)
	}
	if plan[7].Ref != 4 {
		t.Fatalf("the anchor at 7 must reference the IDR at 4, got %d", plan[7].Ref)
	}
}

func TestPlanStartsWithIDR(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	plan, err := planner.Plan(plainFrames(3))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan[0].IDR || plan[0].Type != impl.FrameI {
		t.Fatalf("the first frame = %+v, want a leading IDR", plan[0])
	}
	if plan[0].POC != 0 || plan[0].Ref != -1 {
		t.Fatalf("the leading IDR = %+v, want poc 0 and no reference", plan[0])
	}
	for i := 1; i < len(plan); i++ {
		if plan[i].IDR {
			t.Fatalf("frame %d is an unexpected IDR", i)
		}
		if plan[i].Type != impl.FrameP {
			t.Fatalf("frame %d type = %q, want P without B frames", i, plan[i].Type)
		}
		if plan[i].POC != i || plan[i].Ref != i-1 {
			t.Fatalf("frame %d = %+v, want poc %d and the previous frame as reference", i, plan[i], i)
		}
	}
}

func TestSceneCutForcesIDR(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(6, 0)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	frames := plainFrames(6)
	frames[2].SceneCut = true
	frames[2].Complexity = 0.9
	plan, err := planner.Plan(frames)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var idrs []int
	for i := range plan {
		if plan[i].IDR {
			idrs = append(idrs, i)
		}
	}
	if len(idrs) != 2 || idrs[0] != 0 || idrs[1] != 2 {
		t.Fatalf("IDR positions = %v, want 0 and the scene cut at 2", idrs)
	}
	if plan[2].Type != impl.FrameI || plan[2].POC != 0 {
		t.Fatalf("the scene cut frame = %+v, want a fresh I frame with poc 0", plan[2])
	}
	if plan[2].GOP != 1 {
		t.Fatalf("the scene cut opened gop %d, want 1", plan[2].GOP)
	}
	if plan[1].POC != 1 {
		t.Fatalf("poc before the cut = %d, want 1", plan[1].POC)
	}
}

func TestInsertCutValidates(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if err := planner.InsertCut(-1); !errors.Is(err, impl.ErrCutOutOfRange) {
		t.Fatalf("negative cut index must fail, got %v", err)
	}
	if err := planner.InsertCut(2); err != nil {
		t.Fatalf("first cut request: %v", err)
	}
	if err := planner.InsertCut(2); err != nil {
		t.Fatalf("repeated cut request must be idempotent, got %v", err)
	}
	plan, err := planner.Plan(plainFrames(6))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	idrs := 0
	for i := range plan {
		if plan[i].IDR {
			idrs++
		}
	}
	if idrs != 2 {
		t.Fatalf("plan has %d IDRs, want 0 and the requested cut at 2 only", idrs)
	}
	if !plan[2].IDR {
		t.Fatalf("the requested cut at frame 2 was dropped")
	}
	if err := planner.InsertCut(3); !errors.Is(err, impl.ErrPlanExists) {
		t.Fatalf("cut requests after a plan must fail, got %v", err)
	}
	if err := planner.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := planner.InsertCut(9); err != nil {
		t.Fatalf("cut request after reset: %v", err)
	}
	if _, err := planner.Plan(plainFrames(6)); !errors.Is(err, impl.ErrCutOutOfRange) {
		t.Fatalf("a cut outside the clip must fail the plan, got %v", err)
	}
	if _, ok := planner.PlanFrame(0); ok {
		t.Fatalf("a failed plan must not be visible")
	}
}

func TestPlanFrameCountMatchesHint(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	single, err := planner.Plan(plainFrames(1))
	if err != nil {
		t.Fatalf("single frame plan: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("single frame plan length = %d", len(single))
	}
	plan, err := planner.Plan(plainFrames(17))
	if err != nil {
		t.Fatalf("17 frame plan: %v", err)
	}
	if len(plan) != 17 {
		t.Fatalf("plan length = %d, want 17", len(plan))
	}
	if _, ok := planner.PlanFrame(16); !ok {
		t.Fatalf("the last frame is missing from the plan")
	}
	if _, ok := planner.PlanFrame(-1); ok {
		t.Fatalf("negative frame index must not resolve")
	}
	if _, ok := planner.PlanFrame(17); ok {
		t.Fatalf("frame index past the clip must not resolve")
	}
}

func TestStatsCounts(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 2)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if _, err := planner.Plan(plainFrames(8)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	stats := planner.Stats()
	if stats.GOPs != 2 {
		t.Fatalf("stats gops = %d, want 2", stats.GOPs)
	}
	if stats.IDRs != 2 {
		t.Fatalf("stats idrs = %d, want 2", stats.IDRs)
	}
	if stats.BFrames != 4 {
		t.Fatalf("stats b frames = %d, want 4", stats.BFrames)
	}
	if math.Abs(stats.AverageGOP-4) > 1e-9 {
		t.Fatalf("average gop = %v, want 4", stats.AverageGOP)
	}
}

func TestValidateReportsIDRBudget(t *testing.T) {
	limits := impl.Limits{MaxGOP: 8, MaxBFrames: 3, MaxIDRPerMinute: 60, MaxFrames: 64, NominalFPS: 10}
	planner := impl.NewGopStructurePlanner(limits)
	if err := planner.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	plan, err := planner.Plan(plainFrames(10))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	idrs := 0
	for i := range plan {
		if plan[i].IDR {
			idrs++
		}
	}
	if idrs != 3 {
		t.Fatalf("plan has %d IDRs, want 3 for a ten frame clip with a four frame GOP", idrs)
	}
	issues := planner.Validate()
	found := false
	for _, issue := range issues {
		if issue.Code == impl.IssueIDRBudget {
			found = true
		}
	}
	if !found {
		t.Fatalf("validate did not report the IDR budget, got %+v", issues)
	}
	roomy := impl.Limits{MaxGOP: 8, MaxBFrames: 3, MaxIDRPerMinute: 600, MaxFrames: 64, NominalFPS: 10}
	second := impl.NewGopStructurePlanner(roomy)
	if err := second.SetProfile(closedProfile(4, 0)); err != nil {
		t.Fatalf("second profile: %v", err)
	}
	if _, err := second.Plan(plainFrames(10)); err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if issues := second.Validate(); len(issues) != 0 {
		t.Fatalf("a plan inside the budget must validate clean, got %+v", issues)
	}
}

func TestSnapshotRestoreRoundTrips(t *testing.T) {
	limits := gopLimits()
	planner := impl.NewGopStructurePlanner(limits)
	if err := planner.SetProfile(closedProfile(4, 2)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	original, err := planner.Plan(plainFrames(8))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	data, err := planner.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewGopStructurePlanner(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Profile().MaxGOP; got != 4 {
		t.Fatalf("restored maximum gop = %d, want 4", got)
	}
	for i := range original {
		frame, ok := restored.PlanFrame(i)
		if !ok {
			t.Fatalf("restored plan is missing frame %d", i)
		}
		if frame != original[i] {
			t.Fatalf("restored frame %d = %+v, want %+v", i, frame, original[i])
		}
	}
	stats := restored.Stats()
	if stats.IDRs != 2 || stats.BFrames != 4 {
		t.Fatalf("restored stats = %+v, want 2 IDRs and 4 B frames", stats)
	}
	if err := restored.Restore([]byte("[]")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if frame, ok := restored.PlanFrame(0); !ok || frame.Type != impl.FrameI {
		t.Fatalf("a failed restore must keep the live plan, got %+v ok=%v", frame, ok)
	}
}

func TestConcurrentReadsStayConsistent(t *testing.T) {
	planner := impl.NewGopStructurePlanner(gopLimits())
	if err := planner.SetProfile(closedProfile(4, 2)); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if _, err := planner.Plan(plainFrames(16)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	valid := map[string]bool{impl.FrameI: true, impl.FrameP: true, impl.FrameB: true}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				index := (w*10 + i) % 16
				frame, ok := planner.PlanFrame(index)
				if !ok || frame.Index != index || !valid[frame.Type] {
					errs <- errors.New("plan query returned an inconsistent frame")
					return
				}
				if planner.Stats().IDRs == 0 {
					errs <- errors.New("stats lost the IDR count")
					return
				}
				if planner.Profile().MaxGOP != 4 {
					errs <- errors.New("profile changed under readers")
					return
				}
				planner.Validate()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read failed: %v", err)
	}
	if got := planner.Stats().GOPs; got != 4 {
		t.Fatalf("final gop count = %d, want 4", got)
	}
}
