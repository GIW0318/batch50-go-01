package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-14"
)

func imageLimits() impl.Limits {
	return impl.Limits{
		MaxChunksPerImage: 16,
		MaxImageBytes:     1 << 20,
		MaxImages:         4,
		RestoreGrace:      time.Minute,
		Now:               func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func sealPolicy() impl.SealPolicy {
	return impl.SealPolicy{Algorithm: "aes-256-xts", Retention: 24 * time.Hour, Sealed: true}
}

func sealedRef(id string, pages int) impl.ImageRef {
	return impl.ImageRef{ID: id, Pages: pages, Bytes: int64(pages) * 4096}
}

func currentImage(t *testing.T, g *impl.HibernateImageGuard, id string) impl.SealedImage {
	t.Helper()
	for _, img := range g.Images() {
		if img.ID == id {
			return img
		}
	}
	t.Fatalf("image %q is missing from the image table", id)
	return impl.SealedImage{}
}

func TestSealRejectsEmptyAlgorithm(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	policy := sealPolicy()
	policy.Algorithm = ""
	if _, err := g.Seal(sealedRef("img-1", 2), policy); !errors.Is(err, impl.ErrPolicyMismatch) {
		t.Fatalf("seal with an empty algorithm = %v, want ErrPolicyMismatch", err)
	}
}

func TestSealRegistersImage(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	img, err := g.Seal(sealedRef("img-1", 3), sealPolicy())
	if err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if img.ID != "img-1" {
		t.Fatalf("image id = %q, want img-1", img.ID)
	}
	if img.Algorithm != "aes-256-xts" {
		t.Fatalf("image algorithm = %q, want aes-256-xts", img.Algorithm)
	}
	if img.Watermark != 0 || len(img.Chunks) != 0 {
		t.Fatalf("fresh image watermark = %d chunks = %d, want 0 and 0", img.Watermark, len(img.Chunks))
	}
	if img.Completed {
		t.Fatalf("fresh image is already marked complete")
	}
	images := g.Images()
	if len(images) != 1 || images[0].ID != "img-1" {
		t.Fatalf("image table = %+v, want exactly img-1", images)
	}
}

func TestAppendChunkAdvancesWatermark(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	ref := sealedRef("img-2", 3)
	if _, err := g.Seal(ref, sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	sizes := []int{64, 128, 256}
	for i, size := range sizes {
		data := make([]byte, size)
		for j := range data {
			data[j] = byte(i*31 + j)
		}
		idx, err := g.AppendChunk("img-2", data)
		if err != nil {
			t.Fatalf("append chunk %d must succeed, got %v", i, err)
		}
		if idx != i {
			t.Fatalf("append returned index %d, want %d", idx, i)
		}
	}
	chunks := g.Chunks("img-2")
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunk %d index = %d", i, c.Index)
		}
		if c.Size != sizes[i] {
			t.Fatalf("chunk %d size = %d, want %d", i, c.Size, sizes[i])
		}
		if c.State != impl.ChunkWritten {
			t.Fatalf("chunk %d state = %q, want %q", i, c.State, impl.ChunkWritten)
		}
		if c.Hash == "" {
			t.Fatalf("chunk %d has an empty hash", i)
		}
	}
	if img := currentImage(t, g, "img-2"); img.Watermark != 3 {
		t.Fatalf("watermark = %d, want 3", img.Watermark)
	}
}

func TestAppendChunkRejectsAfterCommit(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-3", 1), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-3", []byte("payload")); err != nil {
		t.Fatalf("append must succeed, got %v", err)
	}
	if err := g.Commit("img-3"); err != nil {
		t.Fatalf("commit must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-3", []byte("late")); !errors.Is(err, impl.ErrImageSealed) {
		t.Fatalf("append after commit = %v, want ErrImageSealed", err)
	}
}

func TestAppendChunkEnforcesChunkLimit(t *testing.T) {
	limits := imageLimits()
	limits.MaxChunksPerImage = 2
	g := impl.NewHibernateImageGuard(limits)
	if _, err := g.Seal(sealedRef("img-4", 5), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := g.AppendChunk("img-4", []byte("chunk")); err != nil {
			t.Fatalf("append %d must succeed, got %v", i, err)
		}
	}
	if _, err := g.AppendChunk("img-4", []byte("chunk")); !errors.Is(err, impl.ErrChunkLimit) {
		t.Fatalf("append above the chunk cap = %v, want ErrChunkLimit", err)
	}
	if chunks := g.Chunks("img-4"); len(chunks) != 2 {
		t.Fatalf("chunks after refusal = %d, want 2", len(chunks))
	}
}

func TestAppendChunkEnforcesDiskBudget(t *testing.T) {
	limits := imageLimits()
	limits.MaxImageBytes = 4096
	g := impl.NewHibernateImageGuard(limits)
	if _, err := g.Seal(sealedRef("img-5", 4), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	block := make([]byte, 2000)
	if _, err := g.AppendChunk("img-5", block); err != nil {
		t.Fatalf("first chunk must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-5", block); err != nil {
		t.Fatalf("second chunk must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-5", block); !errors.Is(err, impl.ErrDiskBudget) {
		t.Fatalf("chunk above the disk reservation = %v, want ErrDiskBudget", err)
	}
	if chunks := g.Chunks("img-5"); len(chunks) != 2 {
		t.Fatalf("chunks after refusal = %d, want 2", len(chunks))
	}
}

func TestCommitRequiresAllPages(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-6", 2), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if err := g.Commit("img-6"); !errors.Is(err, impl.ErrIncomplete) {
		t.Fatalf("commit without all pages = %v, want ErrIncomplete", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := g.AppendChunk("img-6", []byte("page")); err != nil {
			t.Fatalf("append %d must succeed, got %v", i, err)
		}
	}
	if err := g.Commit("img-6"); err != nil {
		t.Fatalf("commit with all pages must succeed, got %v", err)
	}
	if err := g.Commit("img-6"); !errors.Is(err, impl.ErrAlreadySealed) {
		t.Fatalf("second commit = %v, want ErrAlreadySealed", err)
	}
}

func TestOpenRejectsAlgorithmMismatch(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-7", 1), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-7", []byte("page")); err != nil {
		t.Fatalf("append must succeed, got %v", err)
	}
	if err := g.Commit("img-7"); err != nil {
		t.Fatalf("commit must succeed, got %v", err)
	}
	img := currentImage(t, g, "img-7")
	if _, err := g.Open(img, impl.KeyRef{ID: "k1", Algorithm: "aes-128-xts"}); !errors.Is(err, impl.ErrPolicyMismatch) {
		t.Fatalf("open with a different algorithm = %v, want ErrPolicyMismatch", err)
	}
	if _, err := g.Open(img, impl.KeyRef{ID: "k1", Algorithm: "aes-256-xts"}); err != nil {
		t.Fatalf("open with the matching algorithm must succeed, got %v", err)
	}
}

func TestOpenRejectsUnsealedImage(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-8", 1), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-8", []byte("page")); err != nil {
		t.Fatalf("append must succeed, got %v", err)
	}
	img := currentImage(t, g, "img-8")
	if _, err := g.Open(img, impl.KeyRef{ID: "k1", Algorithm: "aes-256-xts"}); !errors.Is(err, impl.ErrNotSealed) {
		t.Fatalf("open before commit = %v, want ErrNotSealed", err)
	}
}

func TestCloseRejectsUnknownImage(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if err := g.Close("img-never-sealed"); !errors.Is(err, impl.ErrUnknownImage) {
		t.Fatalf("close unknown image = %v, want ErrUnknownImage", err)
	}
}

func TestVerifyChunkRejectsOutOfRange(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-9", 1), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-9", []byte("page")); err != nil {
		t.Fatalf("append must succeed, got %v", err)
	}
	if err := g.Commit("img-9"); err != nil {
		t.Fatalf("commit must succeed, got %v", err)
	}
	img := currentImage(t, g, "img-9")
	if _, err := g.VerifyChunk(img, 5); !errors.Is(err, impl.ErrChunkRange) {
		t.Fatalf("verify chunk 5 = %v, want ErrChunkRange", err)
	}
	if _, err := g.VerifyChunk(img, -1); !errors.Is(err, impl.ErrChunkRange) {
		t.Fatalf("verify chunk -1 = %v, want ErrChunkRange", err)
	}
	state, err := g.VerifyChunk(img, 0)
	if err != nil {
		t.Fatalf("verify chunk 0 must succeed, got %v", err)
	}
	if state.State != impl.ChunkVerified {
		t.Fatalf("verified chunk state = %q, want %q", state.State, impl.ChunkVerified)
	}
}

func TestVerifyChunkDetectsCorruptChunk(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-10", 1), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-10", []byte("original payload")); err != nil {
		t.Fatalf("append must succeed, got %v", err)
	}
	if err := g.Commit("img-10"); err != nil {
		t.Fatalf("commit must succeed, got %v", err)
	}
	img := currentImage(t, g, "img-10")
	tampered := img
	tampered.Chunks = append([]impl.ChunkState(nil), img.Chunks...)
	tampered.Chunks[0].Hash = "corrupted-hash"
	if _, err := g.VerifyChunk(tampered, 0); !errors.Is(err, impl.ErrChunkCorrupt) {
		t.Fatalf("verify a chunk with a broken hash = %v, want ErrChunkCorrupt", err)
	}
}

func TestDiscardRemovesImage(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if _, err := g.Seal(sealedRef("img-11", 1), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	if _, err := g.AppendChunk("img-11", []byte("page")); err != nil {
		t.Fatalf("append must succeed, got %v", err)
	}
	if err := g.Discard("img-11"); err != nil {
		t.Fatalf("discard must succeed, got %v", err)
	}
	if images := g.Images(); len(images) != 0 {
		t.Fatalf("image table after discard = %+v, want empty", images)
	}
	if err := g.Discard("img-11"); !errors.Is(err, impl.ErrUnknownImage) {
		t.Fatalf("second discard = %v, want ErrUnknownImage", err)
	}
}

func TestSnapshotRestoreKeepsWatermark(t *testing.T) {
	limits := imageLimits()
	g := impl.NewHibernateImageGuard(limits)
	if _, err := g.Seal(sealedRef("img-12", 3), sealPolicy()); err != nil {
		t.Fatalf("seal must succeed, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := g.AppendChunk("img-12", []byte("chunk-data")); err != nil {
			t.Fatalf("append %d must succeed, got %v", i, err)
		}
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewHibernateImageGuard(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	chunks := restored.Chunks("img-12")
	if len(chunks) != 2 {
		t.Fatalf("restored chunks = %d, want 2", len(chunks))
	}
	idx, err := restored.AppendChunk("img-12", []byte("chunk-data"))
	if err != nil {
		t.Fatalf("append after restore must succeed, got %v", err)
	}
	if idx != 2 {
		t.Fatalf("append after restore returned index %d, want 2", idx)
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	g := impl.NewHibernateImageGuard(imageLimits())
	if err := g.Restore([]byte("not a sealed image snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("restore garbage = %v, want ErrBadSnapshot", err)
	}
}
