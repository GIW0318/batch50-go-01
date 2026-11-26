package task

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-10"
)

func zeros(n int) []float32 {
	return make([]float32, n)
}

func stepPCM(n, crossing int) []float32 {
	pcm := make([]float32, n)
	for i := range pcm {
		if i < crossing {
			pcm[i] = 1
		} else {
			pcm[i] = -1
		}
	}
	return pcm
}

func rampPCM(n int, step float32) []float32 {
	pcm := make([]float32, n)
	for i := range pcm {
		pcm[i] = float32(i) * step
	}
	return pcm
}

func almostEqual(a, b float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-5
}

func candidatesEqual(a, b []impl.Candidate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAnalyzeDedupesEqualScores(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	cands, err := p.Analyze("tone", zeros(200))
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %v, want the equal score run merged into one", cands)
	}
	if cands[0].Index != 32 || cands[0].Score != 1 || !cands[0].ZeroCrossing {
		t.Fatalf("candidate = %+v, want index 32 score 1 at a zero crossing", cands[0])
	}
	stored := p.Candidates("tone")
	if !candidatesEqual(stored, cands) {
		t.Fatalf("stored candidates = %v, want %v", stored, cands)
	}
}

func TestAnalyzeRejectsBadInput(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	if _, err := p.Analyze("   ", zeros(200)); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("blank sample key must fail with ErrBadSample, got %v", err)
	}
	if _, err := p.Analyze("short", zeros(64)); !errors.Is(err, impl.ErrShortPCM) {
		t.Fatalf("64 samples must fail with ErrShortPCM, got %v", err)
	}
	if _, err := p.Analyze("nil", nil); !errors.Is(err, impl.ErrShortPCM) {
		t.Fatalf("nil PCM must fail with ErrShortPCM, got %v", err)
	}
	if got := len(p.Samples()); got != 0 {
		t.Fatalf("rejected analyses must not register samples, got %d", got)
	}
}

func TestAnalyzeCapsCandidates(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{MaxCandidates: 3})
	pcm := make([]float32, 220)
	for i := range pcm {
		if i >= 110 {
			pcm[i] = 0.2
		}
	}
	cands, err := p.Analyze("two-level", pcm)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want the configured cap 3", len(cands))
	}
	if cands[0].Index != 32 || cands[0].Score != 1 {
		t.Fatalf("first candidate = %+v, want index 32 with score 1", cands[0])
	}
	for i := 1; i < len(cands); i++ {
		if cands[i].Score > cands[i-1].Score {
			t.Fatalf("candidates are not sorted by descending score: %v", cands)
		}
		if cands[i].Score < 0.75 {
			t.Fatalf("candidate %+v scored below the 0.75 threshold", cands[i])
		}
	}
}

func TestSelectSnapsToZeroCrossing(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	if _, err := p.Analyze("lead", stepPCM(200, 120)); err != nil {
		t.Fatalf("analyse: %v", err)
	}
	loop, err := p.Select("lead", 60)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if loop.Sample != "lead" || loop.Start != 120 || loop.End != 200 || loop.Length != 80 {
		t.Fatalf("loop = %+v, want lead 120..200 of length 80", loop)
	}
	if !loop.ZeroAligned || loop.Degraded || loop.Crossfade != 0 {
		t.Fatalf("loop flags = %+v, want a zero aligned loop without crossfade", loop)
	}
	stored, ok := p.Loop("lead")
	if !ok {
		t.Fatalf("expected the selected loop to be readable")
	}
	if stored != loop {
		t.Fatalf("stored loop = %+v, want %+v", stored, loop)
	}
}

func TestSelectRejectsUnknownMissingAndShort(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	if _, err := p.Select("ghost", 50); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("selecting an unanalysed sample must fail with ErrUnknownSample, got %v", err)
	}
	if _, err := p.Analyze("ramp", rampPCM(200, 0.5)); err != nil {
		t.Fatalf("analyse ramp: %v", err)
	}
	if _, err := p.Select("ramp", 50); !errors.Is(err, impl.ErrNoCandidate) {
		t.Fatalf("a steep ramp must leave no candidate and fail with ErrNoCandidate, got %v", err)
	}
	if _, err := p.Analyze("tiny", zeros(90)); err != nil {
		t.Fatalf("analyse tiny: %v", err)
	}
	if _, err := p.Select("tiny", 50); !errors.Is(err, impl.ErrShortLoop) {
		t.Fatalf("a loop below the minimum must fail with ErrShortLoop, got %v", err)
	}
	if _, ok := p.Loop("tiny"); ok {
		t.Fatalf("a rejected selection must not be recorded")
	}
}

func TestSetCrossfadeWindow(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{SampleRate: 1000, MaxCrossfade: 8})
	if _, err := p.Analyze("pad", zeros(200)); err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if err := p.SetCrossfade("pad", 5*time.Millisecond); err != nil {
		t.Fatalf("set crossfade: %v", err)
	}
	if got := p.Crossfade("pad"); got != 5*time.Millisecond {
		t.Fatalf("crossfade = %v, want 5ms", got)
	}
	if err := p.SetCrossfade("pad", -time.Millisecond); !errors.Is(err, impl.ErrBadCrossfade) {
		t.Fatalf("negative crossfade must fail with ErrBadCrossfade, got %v", err)
	}
	if err := p.SetCrossfade("pad", 9*time.Millisecond); !errors.Is(err, impl.ErrBadCrossfade) {
		t.Fatalf("crossfade beyond the sample cap must fail with ErrBadCrossfade, got %v", err)
	}
	if got := p.Crossfade("pad"); got != 5*time.Millisecond {
		t.Fatalf("rejected crossfades must keep the old value, got %v", got)
	}
	if err := p.SetCrossfade("ghost", time.Millisecond); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("crossfade on an unanalysed sample must fail with ErrUnknownSample, got %v", err)
	}
	if err := p.SetCrossfade("   ", time.Millisecond); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("blank sample key must fail with ErrBadSample, got %v", err)
	}
}

func TestValidateRejectsBadLoopPoints(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{MinLoop: 16})
	valid := impl.LoopPoint{Sample: "pad", Start: 10, End: 90, Length: 80, Crossfade: 4}
	if err := p.Validate(valid); err != nil {
		t.Fatalf("valid loop rejected: %v", err)
	}
	badStart := valid
	badStart.Start = -1
	if err := p.Validate(badStart); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("negative start must fail with ErrBadLoop, got %v", err)
	}
	empty := impl.LoopPoint{Sample: "pad", Start: 10, End: 10, Length: 0}
	if err := p.Validate(empty); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("empty range must fail with ErrBadLoop, got %v", err)
	}
	mismatch := impl.LoopPoint{Sample: "pad", Start: 10, End: 90, Length: 40}
	if err := p.Validate(mismatch); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("length mismatch must fail with ErrBadLoop, got %v", err)
	}
	wide := impl.LoopPoint{Sample: "pad", Start: 10, End: 90, Length: 80, Crossfade: 81}
	if err := p.Validate(wide); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("crossfade wider than the loop must fail with ErrBadLoop, got %v", err)
	}
	negative := impl.LoopPoint{Sample: "pad", Start: 10, End: 90, Length: 80, Crossfade: -1}
	if err := p.Validate(negative); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("negative crossfade must fail with ErrBadLoop, got %v", err)
	}
	short := impl.LoopPoint{Sample: "pad", Start: 0, End: 10, Length: 10}
	if err := p.Validate(short); !errors.Is(err, impl.ErrShortLoop) {
		t.Fatalf("loop below the minimum must fail with ErrShortLoop, got %v", err)
	}
	blank := valid
	blank.Sample = "  "
	if err := p.Validate(blank); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("blank sample key must fail with ErrBadSample, got %v", err)
	}
}

func TestApplyBlendsCrossfadeHead(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{MinLoop: 16})
	pcm := rampPCM(40, 1)
	loop := impl.LoopPoint{Sample: "seg", Start: 10, End: 30, Length: 20, Crossfade: 2}
	out, err := p.Apply(loop, pcm)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(out) != 20 {
		t.Fatalf("rendered length = %d, want the loop length 20", len(out))
	}
	want0 := float32(10)*(1.0/3.0) + float32(28)*(2.0/3.0)
	want1 := float32(11)*(2.0/3.0) + float32(29)*(1.0/3.0)
	if !almostEqual(out[0], want0) {
		t.Fatalf("out[0] = %v, want %v", out[0], want0)
	}
	if !almostEqual(out[1], want1) {
		t.Fatalf("out[1] = %v, want %v", out[1], want1)
	}
	if !almostEqual(out[2], 12) {
		t.Fatalf("out[2] = %v, want the raw sample 12", out[2])
	}
	if !almostEqual(out[19], 29) {
		t.Fatalf("out[19] = %v, want the raw sample 29", out[19])
	}
}

func TestApplyWithoutCrossfadeCopiesLoop(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{MinLoop: 8})
	pcm := rampPCM(20, 1)
	loop := impl.LoopPoint{Sample: "flat", Start: 5, End: 15, Length: 10}
	out, err := p.Apply(loop, pcm)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(out) != 10 {
		t.Fatalf("rendered length = %d, want 10", len(out))
	}
	for i := 0; i < 10; i++ {
		if !almostEqual(out[i], pcm[5+i]) {
			t.Fatalf("out[%d] = %v, want the raw sample %v", i, out[i], pcm[5+i])
		}
	}
}

func TestApplyRejectsOutOfRangeLoops(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{MinLoop: 8})
	pcm := rampPCM(30, 1)
	beyond := impl.LoopPoint{Sample: "seg", Start: 10, End: 40, Length: 30}
	if _, err := p.Apply(beyond, pcm); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("loop end beyond the PCM must fail with ErrBadLoop, got %v", err)
	}
	negative := impl.LoopPoint{Sample: "seg", Start: -1, End: 10, Length: 11}
	if _, err := p.Apply(negative, pcm); !errors.Is(err, impl.ErrBadLoop) {
		t.Fatalf("negative loop start must fail with ErrBadLoop, got %v", err)
	}
}

func TestDiscardForgetsSample(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	if _, err := p.Analyze("kick", zeros(200)); err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if _, err := p.Select("kick", 50); err != nil {
		t.Fatalf("select: %v", err)
	}
	if err := p.Discard("kick"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, ok := p.Loop("kick"); ok {
		t.Fatalf("a discarded sample must not keep its loop point")
	}
	if got := len(p.Candidates("kick")); got != 0 {
		t.Fatalf("discarded sample kept %d candidates", got)
	}
	if _, err := p.Select("kick", 50); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("selecting a discarded sample must fail with ErrUnknownSample, got %v", err)
	}
	if err := p.Discard("kick"); !errors.Is(err, impl.ErrUnknownSample) {
		t.Fatalf("discarding twice must fail with ErrUnknownSample, got %v", err)
	}
	if err := p.Discard("   "); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("blank sample key must fail with ErrBadSample, got %v", err)
	}
}

func TestLayerKeysStayIndependent(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{SampleRate: 1000})
	if _, err := p.Analyze("kick#1", zeros(200)); err != nil {
		t.Fatalf("analyse layer 1: %v", err)
	}
	if _, err := p.Analyze("kick#2", zeros(220)); err != nil {
		t.Fatalf("analyse layer 2: %v", err)
	}
	if err := p.SetCrossfade("kick#1", 3*time.Millisecond); err != nil {
		t.Fatalf("set crossfade on layer 1: %v", err)
	}
	one, err := p.Select("kick#1", 50)
	if err != nil {
		t.Fatalf("select layer 1: %v", err)
	}
	if one.End != 200 || one.Crossfade != 3 {
		t.Fatalf("layer 1 loop = %+v, want end 200 with a 3 sample crossfade", one)
	}
	two, err := p.Select("kick#2", 50)
	if err != nil {
		t.Fatalf("select layer 2: %v", err)
	}
	if two.End != 220 || two.Crossfade != 0 {
		t.Fatalf("layer 2 loop = %+v, want end 220 without a crossfade", two)
	}
	if got := p.Crossfade("kick#2"); got != 0 {
		t.Fatalf("layer 2 crossfade = %v, want 0", got)
	}
	if err := p.Discard("kick#1"); err != nil {
		t.Fatalf("discard layer 1: %v", err)
	}
	samples := p.Samples()
	if len(samples) != 1 || samples[0] != "kick#2" {
		t.Fatalf("samples = %v, want only kick#2", samples)
	}
	if _, ok := p.Loop("kick#2"); !ok {
		t.Fatalf("discarding layer 1 must not touch layer 2")
	}
}

func TestSnapshotRestoreKeepsCandidatesAndLoop(t *testing.T) {
	opts := impl.Options{SampleRate: 1000}
	p := impl.NewLoopPointPlanner(opts)
	pcm := stepPCM(200, 120)
	if _, err := p.Analyze("lead", pcm); err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if err := p.SetCrossfade("lead", 4*time.Millisecond); err != nil {
		t.Fatalf("set crossfade: %v", err)
	}
	loop, err := p.Select("lead", 60)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if loop.Start != 120 || loop.Crossfade != 4 {
		t.Fatalf("loop = %+v, want start 120 with a 4 sample crossfade", loop)
	}
	before := p.Candidates("lead")
	beforeOut, err := p.Apply(loop, pcm)
	if err != nil {
		t.Fatalf("apply before snapshot: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewLoopPointPlanner(opts)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := r.Candidates("lead"); !candidatesEqual(got, before) {
		t.Fatalf("restored candidates = %v, want %v", got, before)
	}
	restored, ok := r.Loop("lead")
	if !ok {
		t.Fatalf("restored planner lost the selected loop")
	}
	if restored != loop {
		t.Fatalf("restored loop = %+v, want %+v", restored, loop)
	}
	afterOut, err := r.Apply(restored, pcm)
	if err != nil {
		t.Fatalf("apply after restore: %v", err)
	}
	if len(afterOut) != len(beforeOut) {
		t.Fatalf("restored render length = %d, want %d", len(afterOut), len(beforeOut))
	}
	for i := range afterOut {
		if !almostEqual(afterOut[i], beforeOut[i]) {
			t.Fatalf("restored render differs at %d: %v vs %v", i, afterOut[i], beforeOut[i])
		}
	}
}

func TestConcurrentAnalyzeAndSelect(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("voice-%02d", i)
			if _, err := p.Analyze(name, zeros(200)); err != nil {
				errs <- fmt.Errorf("analyse %s: %w", name, err)
				return
			}
			loop, err := p.Select(name, 50)
			if err != nil {
				errs <- fmt.Errorf("select %s: %w", name, err)
				return
			}
			if loop.Start != 32 || loop.End != 200 {
				errs <- fmt.Errorf("loop %s = %+v, want 32..200", name, loop)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent analysis failed: %v", err)
	}
	if got := len(p.Samples()); got != 8 {
		t.Fatalf("analysed samples = %d, want 8", got)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	p := impl.NewLoopPointPlanner(impl.Options{})
	pcm := stepPCM(200, 120)
	if _, err := p.Analyze("lead", pcm); err != nil {
		t.Fatalf("analyse: %v", err)
	}
	loop, err := p.Select("lead", 60)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if err := p.Restore([]byte("no-snapshot-here")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	kept, ok := p.Loop("lead")
	if !ok || kept != loop {
		t.Fatalf("rejected restore changed the loop to %+v ok=%v", kept, ok)
	}
	if got := len(p.Candidates("lead")); got == 0 {
		t.Fatalf("rejected restore dropped the candidate list")
	}
	out, err := p.Apply(kept, pcm)
	if err != nil {
		t.Fatalf("apply after rejected restore: %v", err)
	}
	if len(out) != 80 {
		t.Fatalf("render length after rejected restore = %d, want 80", len(out))
	}
}
