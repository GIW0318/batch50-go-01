package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-25"
)

func triageOptions() impl.Options {
	return impl.Options{MaxClusters: 8, MaxSamplesPerCluster: 8, MinSamplesForGrade: 2, MinReproducibility: 80}
}

func crashSample(input string) impl.Sample {
	return impl.Sample{
		InputID: input,
		Frames: []impl.Frame{
			{Module: "target.exe", Function: "parse_header", Offset: 0x120},
			{Module: "target.exe", Function: "main", Offset: 0x40},
		},
		FaultAddress:    "0x41414141",
		Registers:       map[string]string{"rax": "0x41414141", "rip": "0x401120"},
		WriteControlled: true,
		Reproducibility: 95,
		Signal:          "SIGSEGV",
	}
}

func weakSample(input string) impl.Sample {
	sample := crashSample(input)
	sample.WriteControlled = false
	return sample
}

func otherSample(input string) impl.Sample {
	sample := crashSample(input)
	sample.Frames = []impl.Frame{
		{Module: "target.exe", Function: "parse_body", Offset: 0x220},
		{Module: "target.exe", Function: "main", Offset: 0x40},
	}
	return sample
}

func submitSample(t *testing.T, c *impl.CrashTriageClassifier, sample impl.Sample) string {
	t.Helper()
	crashID, err := c.Submit(sample)
	if err != nil {
		t.Fatalf("submit %s: %v", sample.InputID, err)
	}
	if crashID == "" {
		t.Fatalf("submit %s returned an empty crash id", sample.InputID)
	}
	return crashID
}

func TestSubmitCreatesCluster(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	crashID := submitSample(t, c, crashSample("input-1"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	if clusterID == "" {
		t.Fatalf("cluster id must not be empty")
	}
	views := c.Clusters()
	if len(views) != 1 || views[0].ID != clusterID {
		t.Fatalf("clusters = %+v, want exactly the new cluster", views)
	}
	if views[0].Level != impl.LevelNone || views[0].Label != impl.LabelNone {
		t.Fatalf("fresh cluster level=%d label=%q, want none/none", views[0].Level, views[0].Label)
	}
	crashes := c.Crashes(clusterID)
	if len(crashes) != 1 || crashes[0] != crashID {
		t.Fatalf("cluster crashes = %v, want [%s]", crashes, crashID)
	}
}

func TestSubmitRejectsBlankSample(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	blankInput := crashSample("   ")
	if _, err := c.Submit(blankInput); !errors.Is(err, impl.ErrBlankSample) {
		t.Fatalf("blank input id must fail with ErrBlankSample, got %v", err)
	}
	noFrames := crashSample("input-2")
	noFrames.Frames = nil
	if _, err := c.Submit(noFrames); !errors.Is(err, impl.ErrBlankSample) {
		t.Fatalf("sample without frames must fail with ErrBlankSample, got %v", err)
	}
	noAddress := crashSample("input-3")
	noAddress.FaultAddress = ""
	if _, err := c.Submit(noAddress); !errors.Is(err, impl.ErrBlankSample) {
		t.Fatalf("sample without a fault address must fail with ErrBlankSample, got %v", err)
	}
	if got := len(c.Clusters()); got != 0 {
		t.Fatalf("rejected samples must not create clusters, got %d", got)
	}
}

func TestSubmitGroupsSameFingerprint(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	first := submitSample(t, c, crashSample("input-1"))
	second := submitSample(t, c, crashSample("input-2"))
	clusterA, err := c.Cluster(first)
	if err != nil {
		t.Fatalf("first cluster: %v", err)
	}
	clusterB, err := c.Cluster(second)
	if err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	if clusterA != clusterB {
		t.Fatalf("identical fingerprints landed in %q and %q", clusterA, clusterB)
	}
	crashes := c.Crashes(clusterA)
	if len(crashes) != 2 || crashes[0] != first || crashes[1] != second {
		t.Fatalf("cluster crashes = %v, want submission order [%s %s]", crashes, first, second)
	}
}

func TestSubmitSeparatesDifferentRoots(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	first := submitSample(t, c, crashSample("input-1"))
	second := submitSample(t, c, otherSample("input-2"))
	clusterA, err := c.Cluster(first)
	if err != nil {
		t.Fatalf("first cluster: %v", err)
	}
	clusterB, err := c.Cluster(second)
	if err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	if clusterA == clusterB {
		t.Fatalf("different crash roots must not share a cluster")
	}
	if got := len(c.Clusters()); got != 2 {
		t.Fatalf("clusters = %d, want 2", got)
	}
}

func TestSubmitEnforcesClusterLimit(t *testing.T) {
	opts := triageOptions()
	opts.MaxClusters = 2
	c := impl.NewCrashTriageClassifier(opts)
	submitSample(t, c, crashSample("input-1"))
	submitSample(t, c, otherSample("input-2"))
	third := otherSample("input-3")
	third.Frames[0].Function = "parse_footer"
	if _, err := c.Submit(third); !errors.Is(err, impl.ErrClusterLimit) {
		t.Fatalf("third distinct root under a cap of two must fail with ErrClusterLimit, got %v", err)
	}
	if got := len(c.Clusters()); got != 2 {
		t.Fatalf("clusters after rejection = %d, want 2", got)
	}
}

func TestSubmitRetainsNewestSamples(t *testing.T) {
	opts := triageOptions()
	opts.MaxSamplesPerCluster = 2
	c := impl.NewCrashTriageClassifier(opts)
	first := submitSample(t, c, crashSample("input-1"))
	second := submitSample(t, c, crashSample("input-2"))
	third := submitSample(t, c, crashSample("input-3"))
	clusterID, err := c.Cluster(third)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	crashes := c.Crashes(clusterID)
	if len(crashes) != 2 || crashes[0] != second || crashes[1] != third {
		t.Fatalf("retained crashes = %v, want the newest window [%s %s]", crashes, second, third)
	}
	if _, err := c.Cluster(first); !errors.Is(err, impl.ErrUnknownCrash) {
		t.Fatalf("evicted crash must fail with ErrUnknownCrash, got %v", err)
	}
}

func TestGradeNeedsEnoughSamples(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	crashID := submitSample(t, c, crashSample("input-1"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	if _, err := c.Grade(clusterID); !errors.Is(err, impl.ErrInsufficientEvidence) {
		t.Fatalf("grading one sample under a minimum of two must fail with ErrInsufficientEvidence, got %v", err)
	}
}

func TestGradeMarksExploitable(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	submitSample(t, c, crashSample("input-1"))
	crashID := submitSample(t, c, crashSample("input-2"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	grade, err := c.Grade(clusterID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if grade.Level != impl.LevelExploitable || grade.Label != impl.LabelExploitable {
		t.Fatalf("grade = %d/%q, want %d/%q", grade.Level, grade.Label, impl.LevelExploitable, impl.LabelExploitable)
	}
	if grade.Reason != impl.ReasonGradeRaised {
		t.Fatalf("grade reason = %q, want %q", grade.Reason, impl.ReasonGradeRaised)
	}
	if len(grade.History) != 1 || grade.History[0].Level != impl.LevelExploitable {
		t.Fatalf("history = %+v, want one raised entry", grade.History)
	}
	views := c.Clusters()
	if len(views) != 1 || views[0].Level != impl.LevelExploitable {
		t.Fatalf("cluster view level = %+v, want the applied grade", views)
	}
}

func TestGradeMarksProbable(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	submitSample(t, c, weakSample("input-1"))
	crashID := submitSample(t, c, weakSample("input-2"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	grade, err := c.Grade(clusterID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if grade.Level != impl.LevelProbable || grade.Label != impl.LabelProbable {
		t.Fatalf("grade = %d/%q, want probable", grade.Level, grade.Label)
	}
}

func TestGradeIsMonotonic(t *testing.T) {
	opts := triageOptions()
	opts.MaxSamplesPerCluster = 2
	c := impl.NewCrashTriageClassifier(opts)
	submitSample(t, c, crashSample("input-1"))
	crashID := submitSample(t, c, crashSample("input-2"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	first, err := c.Grade(clusterID)
	if err != nil {
		t.Fatalf("first grade: %v", err)
	}
	if first.Level != impl.LevelExploitable {
		t.Fatalf("first grade level = %d, want exploitable", first.Level)
	}
	submitSample(t, c, weakSample("input-3"))
	submitSample(t, c, weakSample("input-4"))
	second, err := c.Grade(clusterID)
	if err != nil {
		t.Fatalf("second grade: %v", err)
	}
	if second.Level != impl.LevelExploitable {
		t.Fatalf("grade dropped to %d, the policy only allows upward moves", second.Level)
	}
	if second.Reason != impl.ReasonGradeKept {
		t.Fatalf("second grade reason = %q, want %q", second.Reason, impl.ReasonGradeKept)
	}
	if len(second.History) != 2 {
		t.Fatalf("history length = %d, want the raise and the keep entry", len(second.History))
	}
	if second.History[1].Reason != impl.ReasonGradeKept {
		t.Fatalf("last history reason = %q, want %q", second.History[1].Reason, impl.ReasonGradeKept)
	}
}

func TestRecheckCanLower(t *testing.T) {
	opts := triageOptions()
	opts.MaxSamplesPerCluster = 2
	c := impl.NewCrashTriageClassifier(opts)
	submitSample(t, c, crashSample("input-1"))
	crashID := submitSample(t, c, crashSample("input-2"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	if _, err := c.Grade(clusterID); err != nil {
		t.Fatalf("grade: %v", err)
	}
	submitSample(t, c, weakSample("input-3"))
	submitSample(t, c, weakSample("input-4"))
	if _, err := c.Grade(clusterID); err != nil {
		t.Fatalf("second grade: %v", err)
	}
	grade, err := c.Recheck(clusterID)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if grade.Level != impl.LevelProbable || grade.Label != impl.LabelProbable {
		t.Fatalf("recheck grade = %d/%q, want probable after the review", grade.Level, grade.Label)
	}
	if grade.Reason != impl.ReasonRecheckDowngrade {
		t.Fatalf("recheck reason = %q, want %q", grade.Reason, impl.ReasonRecheckDowngrade)
	}
	if len(grade.History) != 3 || grade.History[2].Level != impl.LevelProbable {
		t.Fatalf("history = %+v, want raise, keep and downgrade", grade.History)
	}
}

func TestMergeMovesCrashes(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	first := submitSample(t, c, crashSample("input-1"))
	second := submitSample(t, c, otherSample("input-2"))
	clusterA, err := c.Cluster(first)
	if err != nil {
		t.Fatalf("cluster A: %v", err)
	}
	clusterB, err := c.Cluster(second)
	if err != nil {
		t.Fatalf("cluster B: %v", err)
	}
	if err := c.Merge(clusterA, clusterB); err != nil {
		t.Fatalf("merge: %v", err)
	}
	views := c.Clusters()
	if len(views) != 1 || views[0].ID != clusterB {
		t.Fatalf("active clusters = %+v, want only the merge target", views)
	}
	if got := len(c.Crashes(clusterB)); got != 2 {
		t.Fatalf("target cluster crashes = %d, want both samples", got)
	}
	if err := c.Merge(clusterA, clusterB); !errors.Is(err, impl.ErrClusterMerged) {
		t.Fatalf("merging an already merged cluster must fail with ErrClusterMerged, got %v", err)
	}
}

func TestMergeRejectsSelfAndUnknown(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	crashID := submitSample(t, c, crashSample("input-1"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	if err := c.Merge(clusterID, clusterID); !errors.Is(err, impl.ErrMergeSelf) {
		t.Fatalf("self merge must fail with ErrMergeSelf, got %v", err)
	}
	if err := c.Merge(clusterID, "cluster-404"); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("merging into an unknown cluster must fail with ErrUnknownCluster, got %v", err)
	}
}

func TestUnknownLookups(t *testing.T) {
	c := impl.NewCrashTriageClassifier(triageOptions())
	if _, err := c.Cluster("crash-404"); !errors.Is(err, impl.ErrUnknownCrash) {
		t.Fatalf("unknown crash must fail with ErrUnknownCrash, got %v", err)
	}
	if _, err := c.Grade("cluster-404"); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("unknown cluster grade must fail with ErrUnknownCluster, got %v", err)
	}
	if _, err := c.Recheck("cluster-404"); !errors.Is(err, impl.ErrUnknownCluster) {
		t.Fatalf("unknown cluster recheck must fail with ErrUnknownCluster, got %v", err)
	}
}

func TestSnapshotRestoreKeepsGrades(t *testing.T) {
	opts := triageOptions()
	c := impl.NewCrashTriageClassifier(opts)
	submitSample(t, c, crashSample("input-1"))
	crashID := submitSample(t, c, crashSample("input-2"))
	clusterID, err := c.Cluster(crashID)
	if err != nil {
		t.Fatalf("cluster lookup: %v", err)
	}
	if _, err := c.Grade(clusterID); err != nil {
		t.Fatalf("grade: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewCrashTriageClassifier(opts)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	views := restored.Clusters()
	if len(views) != 1 || views[0].Level != impl.LevelExploitable {
		t.Fatalf("restored clusters = %+v, want the graded cluster", views)
	}
	if got := len(restored.Crashes(clusterID)); got != 2 {
		t.Fatalf("restored crashes = %d, want 2", got)
	}
	grade, err := restored.Grade(clusterID)
	if err != nil {
		t.Fatalf("grade after restore: %v", err)
	}
	if grade.Level != impl.LevelExploitable || len(grade.History) != 1 {
		t.Fatalf("restored grade = %+v, want exploitable with one history entry", grade)
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(restored.Clusters()); got != 1 {
		t.Fatalf("failed restore must not touch live state, got %d clusters", got)
	}
}
