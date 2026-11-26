package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-game-a-05"
)

func plannerLimits() impl.Limits {
	return impl.Limits{MaxFiles: 16, ChunkSize: 4, MaxManifests: 8, MaxClients: 8}
}

func v1Files() []impl.FileEntry {
	return []impl.FileEntry{
		{Path: "a.bin", Size: 10, Hash: "h1"},
		{Path: "b.bin", Size: 8, Hash: "h9"},
	}
}

func findPlan(t *testing.T, m impl.Manifest, path string) impl.FilePlan {
	t.Helper()
	for _, f := range m.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("manifest %s has no plan for %s (files %+v)", m.ID, path, m.Files)
	return impl.FilePlan{}
}

func TestBuildPlansReplacementForChangedHash(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	first, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h1"}})
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if first.FromVersion != "v1" || first.ToVersion != "v2" {
		t.Fatalf("manifest versions = %s..%s, want v1..v2", first.FromVersion, first.ToVersion)
	}
	if len(first.Files) != 1 {
		t.Fatalf("first manifest plans %d files, want 1", len(first.Files))
	}
	if first.Files[0].Action != impl.ActionAdd {
		t.Fatalf("a file without a baseline must be added, got %q", first.Files[0].Action)
	}
	if first.Files[0].Size != 10 || len(first.Files[0].Chunks) != 3 {
		t.Fatalf("first plan = %+v, want 10 bytes in 3 chunks", first.Files[0])
	}
	if first.Seq <= 0 {
		t.Fatalf("manifest sequence = %d, want a positive number", first.Seq)
	}
	second, err := p.Build("v2", "v3", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h2"}})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	plan := findPlan(t, second, "a.bin")
	if plan.Action != impl.ActionReplace {
		t.Fatalf("a changed hash must produce %q, got %q", impl.ActionReplace, plan.Action)
	}
	if len(plan.Chunks) != 3 {
		t.Fatalf("replacement chunks = %d, want 3", len(plan.Chunks))
	}
	if second.TotalBytes != 10 || second.Chunks != 3 {
		t.Fatalf("manifest totals = %d bytes / %d chunks, want 10 / 3", second.TotalBytes, second.Chunks)
	}
	if _, ok := p.ManifestOf(second.ID); !ok {
		t.Fatalf("the published manifest must be readable by id")
	}
}

func TestBuildKeepsUnchangedFiles(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	if _, err := p.Build("v1", "v2", v1Files()); err != nil {
		t.Fatalf("first build: %v", err)
	}
	m, err := p.Build("v2", "v3", v1Files())
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("manifest plans %d files, want 2", len(m.Files))
	}
	for _, f := range m.Files {
		if f.Action != impl.ActionKeep {
			t.Fatalf("%s action = %q, want %q", f.Path, f.Action, impl.ActionKeep)
		}
		if len(f.Chunks) != 0 {
			t.Fatalf("%s carries %d chunks, want none for an unchanged file", f.Path, len(f.Chunks))
		}
	}
	if m.TotalBytes != 0 || m.Chunks != 0 {
		t.Fatalf("an all keep manifest needs %d bytes / %d chunks, want 0", m.TotalBytes, m.Chunks)
	}
}

func TestBuildAddsAndRemovesFiles(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	if _, err := p.Build("v1", "v2", v1Files()); err != nil {
		t.Fatalf("first build: %v", err)
	}
	m, err := p.Build("v2", "v3", []impl.FileEntry{
		{Path: "a.bin", Size: 10, Hash: "h1"},
		{Path: "c.bin", Size: 6, Hash: "h3"},
	})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if len(m.Files) != 3 {
		t.Fatalf("manifest plans %d files, want keep+remove+add", len(m.Files))
	}
	if got := findPlan(t, m, "a.bin").Action; got != impl.ActionKeep {
		t.Fatalf("a.bin action = %q, want %q", got, impl.ActionKeep)
	}
	if got := findPlan(t, m, "b.bin").Action; got != impl.ActionRemove {
		t.Fatalf("b.bin action = %q, want %q", got, impl.ActionRemove)
	}
	added := findPlan(t, m, "c.bin")
	if added.Action != impl.ActionAdd {
		t.Fatalf("c.bin action = %q, want %q", added.Action, impl.ActionAdd)
	}
	if len(added.Chunks) != 2 {
		t.Fatalf("c.bin chunks = %d, want 2 for 6 bytes", len(added.Chunks))
	}
	if m.TotalBytes != 6 || m.Chunks != 2 {
		t.Fatalf("manifest totals = %d bytes / %d chunks, want 6 / 2", m.TotalBytes, m.Chunks)
	}
}

func TestBuildComputesFixedSizeChunks(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "pack.bin", Size: 10, Hash: "hp"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	chunks := findPlan(t, m, "pack.bin").Chunks
	if len(chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(chunks))
	}
	wantOffset := []int64{0, 4, 8}
	wantLength := []int{4, 4, 2}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunk %d index = %d, want %d", i, c.Index, i)
		}
		if c.Offset != wantOffset[i] || c.Length != wantLength[i] {
			t.Fatalf("chunk %d = offset %d length %d, want offset %d length %d",
				i, c.Offset, c.Length, wantOffset[i], wantLength[i])
		}
		if c.Path != "pack.bin" {
			t.Fatalf("chunk %d path = %q, want pack.bin", i, c.Path)
		}
	}
	if m.Chunks != 3 {
		t.Fatalf("manifest chunk total = %d, want 3", m.Chunks)
	}
}

func TestBuildRejectsInvalidFileLists(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	if _, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "  ", Size: 4, Hash: "h"}}); !errors.Is(err, impl.ErrBadFile) {
		t.Fatalf("a blank path must fail with ErrBadFile, got %v", err)
	}
	if _, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: -1, Hash: "h"}}); !errors.Is(err, impl.ErrBadFile) {
		t.Fatalf("a negative size must fail with ErrBadFile, got %v", err)
	}
	dup := []impl.FileEntry{{Path: "a.bin", Size: 4, Hash: "h1"}, {Path: "a.bin", Size: 8, Hash: "h2"}}
	if _, err := p.Build("v1", "v2", dup); !errors.Is(err, impl.ErrBadFile) {
		t.Fatalf("a duplicated path must fail with ErrBadFile, got %v", err)
	}
	if _, err := p.Build("", "v2", []impl.FileEntry{{Path: "a.bin", Size: 4, Hash: "h"}}); !errors.Is(err, impl.ErrBadVersion) {
		t.Fatalf("a blank from version must fail with ErrBadVersion, got %v", err)
	}
	if _, err := p.Build("v1", "v1", []impl.FileEntry{{Path: "a.bin", Size: 4, Hash: "h"}}); !errors.Is(err, impl.ErrBadVersion) {
		t.Fatalf("the same version on both sides must fail with ErrBadVersion, got %v", err)
	}
}

func TestBuildEnforcesFileLimit(t *testing.T) {
	limits := plannerLimits()
	limits.MaxFiles = 2
	p := impl.NewPatchManifestPlanner(limits)
	three := []impl.FileEntry{
		{Path: "a.bin", Size: 4, Hash: "h1"},
		{Path: "b.bin", Size: 4, Hash: "h2"},
		{Path: "c.bin", Size: 4, Hash: "h3"},
	}
	if _, err := p.Build("v1", "v2", three); !errors.Is(err, impl.ErrFileLimit) {
		t.Fatalf("a file list over MaxFiles must fail with ErrFileLimit, got %v", err)
	}
	if _, err := p.Build("v1", "v2", three[:2]); err != nil {
		t.Fatalf("a file list at the cap must be accepted, got %v", err)
	}
}

func TestBuildIsStableForTheSameVersionPair(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	first, err := p.Build("v1", "v2", v1Files())
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := p.Build("v1", "v2", v1Files())
	if err != nil {
		t.Fatalf("repeated build: %v", err)
	}
	if first.ID == "" || second.ID == "" {
		t.Fatalf("manifest ids must not be empty, got %q and %q", first.ID, second.ID)
	}
	if first.ID != second.ID {
		t.Fatalf("the same version pair produced ids %q and %q, want one stable id", first.ID, second.ID)
	}
	if first.Seq != second.Seq {
		t.Fatalf("a repeated build changed the sequence %d -> %d", first.Seq, second.Seq)
	}
	if len(second.Files) != len(first.Files) {
		t.Fatalf("repeated build plans %d files, want the original %d", len(second.Files), len(first.Files))
	}
}

func TestBeginPinsManifestPerClient(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m1, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h1"}})
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	m2, err := p.Build("v2", "v3", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h2"}})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	plan, err := p.Begin(m1.ID, "client-1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if plan.ManifestID != m1.ID || plan.ClientID != "client-1" {
		t.Fatalf("download plan = %+v, want it tied to %s/client-1", plan, m1.ID)
	}
	if len(plan.Chunks) != 3 {
		t.Fatalf("begin returned %d chunks, want the 3 planned chunks", len(plan.Chunks))
	}
	if plan.Resume {
		t.Fatalf("a first download must not be flagged as a resume")
	}
	if plan.Seq <= 0 {
		t.Fatalf("download plan sequence = %d, want a positive number", plan.Seq)
	}
	pinned, ok := p.PinnedManifest("client-1")
	if !ok || pinned != m1.ID {
		t.Fatalf("pinned manifest = %q ok=%v, want %s", pinned, ok, m1.ID)
	}
	if _, err := p.Begin(m2.ID, "client-1"); !errors.Is(err, impl.ErrVersionPinned) {
		t.Fatalf("switching manifests mid download must fail with ErrVersionPinned, got %v", err)
	}
	if _, err := p.Begin(m1.ID, "client-2"); err != nil {
		t.Fatalf("another client must be able to start the same manifest: %v", err)
	}
	if _, err := p.Begin("m-404", "client-3"); !errors.Is(err, impl.ErrManifestNotFound) {
		t.Fatalf("an unknown manifest must fail with ErrManifestNotFound, got %v", err)
	}
}

func TestBeginRejectsClientsWithoutDisk(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h1"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := p.SetClientDisk("client-1", 5); err != nil {
		t.Fatalf("set disk: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); !errors.Is(err, impl.ErrDiskFull) {
		t.Fatalf("a short disk must fail with ErrDiskFull, got %v", err)
	}
	if _, ok := p.PinnedManifest("client-1"); ok {
		t.Fatalf("a refused begin must not pin the manifest")
	}
	if err := p.SetClientDisk("client-1", 10); err != nil {
		t.Fatalf("raise the disk budget: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("a disk matching the payload must be accepted, got %v", err)
	}
	if _, err := p.Begin(m.ID, "   "); !errors.Is(err, impl.ErrBadClient) {
		t.Fatalf("a blank client must fail with ErrBadClient, got %v", err)
	}
	if err := p.SetClientDisk("  ", 100); !errors.Is(err, impl.ErrBadClient) {
		t.Fatalf("a blank client on the disk API must fail with ErrBadClient, got %v", err)
	}
}

func TestAckChunkAdvancesProgress(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h1"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	pr, ok := p.ProgressOf(m.ID, "client-1", "a.bin")
	if !ok {
		t.Fatalf("progress of a planned file must be readable")
	}
	if pr.Confirmed != 0 || pr.Total != 3 || pr.Done {
		t.Fatalf("initial progress = %+v, want 0 of 3 and not done", pr)
	}
	for i := 0; i < 3; i++ {
		got, err := p.AckChunk(m.ID, "client-1", "a.bin", i)
		if err != nil {
			t.Fatalf("ack chunk %d: %v", i, err)
		}
		if got.Confirmed != i+1 {
			t.Fatalf("confirmed chunks after ack %d = %d, want %d", i, got.Confirmed, i+1)
		}
		if got.Total != 3 {
			t.Fatalf("total chunks = %d, want 3", got.Total)
		}
		if got.Done != (i == 2) {
			t.Fatalf("done after ack %d = %v, want %v", i, got.Done, i == 2)
		}
		if got.Seq <= 0 {
			t.Fatalf("progress sequence = %d, want a positive number", got.Seq)
		}
	}
}

func TestAckChunkIsIdempotent(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 10, Hash: "h1"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	first, err := p.AckChunk(m.ID, "client-1", "a.bin", 1)
	if err != nil {
		t.Fatalf("first ack: %v", err)
	}
	second, err := p.AckChunk(m.ID, "client-1", "a.bin", 1)
	if err != nil {
		t.Fatalf("repeated ack must stay successful: %v", err)
	}
	if first.Confirmed != 1 || second.Confirmed != 1 {
		t.Fatalf("confirmed chunks = %d then %d, want 1 both times", first.Confirmed, second.Confirmed)
	}
	if second.Done {
		t.Fatalf("one confirmed chunk of three must not mark the file done")
	}
}

func TestVerifyReportsMissingFiles(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	if _, err := p.Build("v1", "v2", v1Files()); err != nil {
		t.Fatalf("first build: %v", err)
	}
	m, err := p.Build("v2", "v3", []impl.FileEntry{
		{Path: "a.bin", Size: 10, Hash: "h2"},
		{Path: "b.bin", Size: 8, Hash: "h9"},
		{Path: "c.bin", Size: 6, Hash: "h3"},
	})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	first, err := p.Verify(m.ID, "client-1")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if first.OK {
		t.Fatalf("a fresh client must not pass the verification")
	}
	if first.FilesTotal != 3 {
		t.Fatalf("files total = %d, want 3", first.FilesTotal)
	}
	if first.FilesDone != 1 {
		t.Fatalf("files done = %d, want the kept file counted as done", first.FilesDone)
	}
	wantMissing := []string{"a.bin", "c.bin"}
	if len(first.Missing) != 2 || first.Missing[0] != wantMissing[0] || first.Missing[1] != wantMissing[1] {
		t.Fatalf("missing files = %v, want %v in path order", first.Missing, wantMissing)
	}
	for i := 0; i < 3; i++ {
		if _, err := p.AckChunk(m.ID, "client-1", "a.bin", i); err != nil {
			t.Fatalf("ack a.bin chunk %d: %v", i, err)
		}
	}
	mid, err := p.Verify(m.ID, "client-1")
	if err != nil {
		t.Fatalf("verify after a.bin: %v", err)
	}
	if mid.OK || mid.FilesDone != 2 {
		t.Fatalf("verify after a.bin = %+v, want 2 done and not ok", mid)
	}
	if len(mid.Missing) != 1 || mid.Missing[0] != "c.bin" {
		t.Fatalf("missing files = %v, want [c.bin]", mid.Missing)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.AckChunk(m.ID, "client-1", "c.bin", i); err != nil {
			t.Fatalf("ack c.bin chunk %d: %v", i, err)
		}
	}
	done, err := p.Verify(m.ID, "client-1")
	if err != nil {
		t.Fatalf("final verify: %v", err)
	}
	if !done.OK || done.FilesDone != 3 || len(done.Missing) != 0 {
		t.Fatalf("final verify = %+v, want ok with 3 of 3 files", done)
	}
}

func TestPauseAndResumeKeepConfirmedChunks(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 12, Hash: "h1"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := p.AckChunk(m.ID, "client-1", "a.bin", 0); err != nil {
		t.Fatalf("ack chunk 0: %v", err)
	}
	if err := p.Pause(m.ID, "client-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := p.Pause(m.ID, "client-1"); !errors.Is(err, impl.ErrAlreadyPaused) {
		t.Fatalf("pausing twice must fail with ErrAlreadyPaused, got %v", err)
	}
	plan, err := p.Resume(m.ID, "client-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !plan.Resume {
		t.Fatalf("a resumed plan must be flagged as a resume")
	}
	if len(plan.Chunks) != 2 {
		t.Fatalf("resume returned %d chunks, want the 2 unfinished ones", len(plan.Chunks))
	}
	for i, c := range plan.Chunks {
		if c.Index != i+1 {
			t.Fatalf("resume chunk %d has index %d, want %d", i, c.Index, i+1)
		}
	}
	if _, err := p.Resume(m.ID, "client-1"); !errors.Is(err, impl.ErrNotPaused) {
		t.Fatalf("resuming a live download must fail with ErrNotPaused, got %v", err)
	}
	if _, err := p.Resume(m.ID, "client-404"); !errors.Is(err, impl.ErrNoPlan) {
		t.Fatalf("resuming without a plan must fail with ErrNoPlan, got %v", err)
	}
}

func TestConcurrentAckChunksConverge(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	m, err := p.Build("v1", "v2", []impl.FileEntry{{Path: "a.bin", Size: 12, Hash: "h1"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := p.AckChunk(m.ID, "client-1", "a.bin", i%3); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if failed != 0 {
		t.Fatalf("%d concurrent chunk acks failed, want every distinct chunk accepted", failed)
	}
	res, err := p.Verify(m.ID, "client-1")
	if err != nil {
		t.Fatalf("verify after the race: %v", err)
	}
	if !res.OK || res.FilesDone != 1 {
		t.Fatalf("verify after the race = %+v, want the single file done", res)
	}
}

func TestSnapshotRestoreKeepsBitmaps(t *testing.T) {
	p := impl.NewPatchManifestPlanner(plannerLimits())
	if _, err := p.Build("v1", "v2", v1Files()); err != nil {
		t.Fatalf("first build: %v", err)
	}
	m, err := p.Build("v2", "v3", []impl.FileEntry{
		{Path: "a.bin", Size: 10, Hash: "h2"},
		{Path: "c.bin", Size: 6, Hash: "h3"},
	})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if _, err := p.Begin(m.ID, "client-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := p.AckChunk(m.ID, "client-1", "a.bin", 0); err != nil {
		t.Fatalf("ack chunk 0: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewPatchManifestPlanner(plannerLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if pinned, ok := restored.PinnedManifest("client-1"); !ok || pinned != m.ID {
		t.Fatalf("restored pin = %q ok=%v, want %s", pinned, ok, m.ID)
	}
	pr, ok := restored.ProgressOf(m.ID, "client-1", "a.bin")
	if !ok || pr.Confirmed != 1 || pr.Total != 3 {
		t.Fatalf("restored progress = %+v ok=%v, want 1 of 3", pr, ok)
	}
	if _, ok := restored.ManifestOf(m.ID); !ok {
		t.Fatalf("the restored manifest must stay readable")
	}
	res, err := restored.Verify(m.ID, "client-1")
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if res.OK || len(res.Missing) != 2 {
		t.Fatalf("verify after restore = %+v, want two unfinished files", res)
	}
	if err := restored.Restore([]byte("not-a-manifest")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	after, err := restored.Verify(m.ID, "client-1")
	if err != nil {
		t.Fatalf("verify after the rejected restore: %v", err)
	}
	if after.OK || len(after.Missing) != 2 {
		t.Fatalf("a rejected restore must not touch live bitmaps, got %+v", after)
	}
	next, err := restored.AckChunk(m.ID, "client-1", "a.bin", 1)
	if err != nil {
		t.Fatalf("ack after the rejected restore: %v", err)
	}
	if next.Confirmed != 2 {
		t.Fatalf("confirmed chunks after the rejected restore = %d, want 2", next.Confirmed)
	}
}
