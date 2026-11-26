package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-d-08"
)

func newEntropyGuard() *impl.EntropyHealthGuard {
	return impl.NewEntropyHealthGuard(impl.Options{MinSeedBytes: 16, BlockTimeout: 40 * time.Millisecond})
}

func fillPool(t *testing.T, g *impl.EntropyHealthGuard, sample string) {
	t.Helper()
	if _, err := g.Feed("hwrng", []byte(sample)); err != nil {
		t.Fatalf("feed %q: %v", sample, err)
	}
}

func TestNewGuardReportsStarvedPool(t *testing.T) {
	g := newEntropyGuard()
	h := g.Health()
	if h.State != impl.StateStarved {
		t.Fatalf("fresh guard state = %q, want %q", h.State, impl.StateStarved)
	}
	if h.PoolBytes != 0 || h.Sources != 0 {
		t.Fatalf("fresh guard health = %+v, want an empty pool and no sources", h)
	}
	if g.PoolBytes() != 0 {
		t.Fatalf("fresh pool bytes = %d, want 0", g.PoolBytes())
	}
}

func TestFeedAcceptsSample(t *testing.T) {
	g := newEntropyGuard()
	credit, err := g.Feed("hwrng", []byte("abcd"))
	if err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	if !credit.Accepted || credit.Reason != impl.ReasonAccepted {
		t.Fatalf("credit = %+v, want an accepted sample", credit)
	}
	if credit.Source != "hwrng" || credit.Score != impl.CreditStep {
		t.Fatalf("credit = %+v, want source hwrng with score %d", credit, impl.CreditStep)
	}
	if g.PoolBytes() != 4 {
		t.Fatalf("pool bytes = %d, want 4", g.PoolBytes())
	}
	if h := g.Health(); h.PoolBytes != 4 || h.Sources != 1 {
		t.Fatalf("health = %+v, want 4 pooled bytes from one source", h)
	}
	if got := g.Sources(); len(got) != 1 || got[0] != "hwrng" {
		t.Fatalf("sources = %v, want [hwrng]", got)
	}
}

func TestFeedRejectsBlankSource(t *testing.T) {
	g := newEntropyGuard()
	if _, err := g.Feed("   ", []byte("abcd")); !errors.Is(err, impl.ErrBadSource) {
		t.Fatalf("blank source must fail with ErrBadSource, got %v", err)
	}
	if g.PoolBytes() != 0 {
		t.Fatalf("rejected feed must not grow the pool, got %d", g.PoolBytes())
	}
}

func TestFeedRejectsEmptySample(t *testing.T) {
	g := newEntropyGuard()
	if _, err := g.Feed("hwrng", nil); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("nil sample must fail with ErrBadSample, got %v", err)
	}
	if _, err := g.Feed("hwrng", []byte{}); !errors.Is(err, impl.ErrBadSample) {
		t.Fatalf("empty sample must fail with ErrBadSample, got %v", err)
	}
	if _, ok := g.CreditOf("hwrng"); ok {
		t.Fatalf("a rejected feed must not create a ledger entry")
	}
}

func TestFeedDuplicateGivesNoCredit(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcd")
	credit, err := g.Feed("hwrng", []byte("abcd"))
	if err != nil {
		t.Fatalf("duplicate feed: %v", err)
	}
	if credit.Accepted || credit.Reason != impl.ReasonDuplicate {
		t.Fatalf("credit = %+v, want a rejected duplicate", credit)
	}
	if credit.Score != impl.CreditStep-impl.CreditPenalty {
		t.Fatalf("credit score = %d, want %d after one duplicate", credit.Score, impl.CreditStep-impl.CreditPenalty)
	}
	if g.PoolBytes() != 4 {
		t.Fatalf("duplicate sample must not grow the pool, got %d", g.PoolBytes())
	}
}

func TestWeightFollowsCreditBands(t *testing.T) {
	g := newEntropyGuard()
	for _, sample := range []string{"s1", "s2", "s3", "s4", "s5"} {
		fillPool(t, g, sample)
	}
	if got := g.Weight("hwrng"); got != 4 {
		t.Fatalf("weight of a source at score 50 = %d, want 4", got)
	}
	if got := g.Weight("never-fed"); got != 0 {
		t.Fatalf("weight of an unknown source = %d, want 0", got)
	}
	if _, ok := g.CreditOf("never-fed"); ok {
		t.Fatalf("unknown source must not have a ledger entry")
	}
}

func TestDrawReturnsPooledBytesInOrder(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcdefgh")
	first, err := g.Draw(3, false)
	if err != nil {
		t.Fatalf("first draw: %v", err)
	}
	if string(first) != "abc" {
		t.Fatalf("first draw = %q, want abc", first)
	}
	if g.PoolBytes() != 5 {
		t.Fatalf("pool bytes = %d, want 5 after drawing 3 of 8", g.PoolBytes())
	}
	second, err := g.Draw(5, false)
	if err != nil {
		t.Fatalf("second draw: %v", err)
	}
	if string(second) != "defgh" {
		t.Fatalf("second draw = %q, want defgh", second)
	}
	if g.PoolBytes() != 0 {
		t.Fatalf("pool bytes = %d, want 0 after draining", g.PoolBytes())
	}
	records := g.Records()
	if len(records) != 2 || records[0].N != 3 || records[1].N != 5 {
		t.Fatalf("records = %+v, want draws of 3 then 5", records)
	}
}

func TestDrawRejectsNonPositiveCount(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcd")
	for _, n := range []int{0, -3} {
		if _, err := g.Draw(n, false); !errors.Is(err, impl.ErrBadDraw) {
			t.Fatalf("draw of %d bytes must fail with ErrBadDraw, got %v", n, err)
		}
	}
	if g.PoolBytes() != 4 {
		t.Fatalf("rejected draws must not consume the pool, got %d", g.PoolBytes())
	}
}

func TestBlockingDrawTimesOutWhenStarved(t *testing.T) {
	g := newEntropyGuard()
	start := time.Now()
	if _, err := g.Draw(4, true); !errors.Is(err, impl.ErrPoolEmpty) {
		t.Fatalf("blocking draw on an empty pool must fail with ErrPoolEmpty, got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond {
		t.Fatalf("blocking draw returned after %s, want it to wait for entropy", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("blocking draw waited %s, far beyond the configured 40ms", elapsed)
	}
}

func TestConcurrentDrawsDoNotOverIssue(t *testing.T) {
	g := newEntropyGuard()
	pool := make([]byte, 64)
	for i := range pool {
		pool[i] = byte(i + 1)
	}
	if _, err := g.Feed("hwrng", pool); err != nil {
		t.Fatalf("feed 64 bytes: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, bad, taken := 0, 0, 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := g.Draw(4, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bad++
				return
			}
			ok++
			taken += len(out)
		}()
	}
	wg.Wait()
	if ok != 16 {
		t.Fatalf("concurrent draws satisfied %d requests, want exactly 16 (64 pooled bytes / 4)", ok)
	}
	if bad != 16 {
		t.Fatalf("concurrent draws refused %d requests, want 16", bad)
	}
	if taken != 64 {
		t.Fatalf("concurrent draws handed out %d bytes, want 64 without over issuing", taken)
	}
	if g.PoolBytes() != 0 {
		t.Fatalf("pool bytes after the race = %d, want 0", g.PoolBytes())
	}
}

func TestDrainLeavesPoolStarved(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcd")
	out, err := g.Draw(4, false)
	if err != nil {
		t.Fatalf("drain the pool: %v", err)
	}
	if string(out) != "abcd" {
		t.Fatalf("drained bytes = %q, want abcd", out)
	}
	if h := g.Health(); h.State != impl.StateStarved || h.PoolBytes != 0 {
		t.Fatalf("health after drain = %+v, want a starved empty pool", h)
	}
}

func TestReseedRejectsShortSeed(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcd")
	if err := g.Reseed([]byte("short")); !errors.Is(err, impl.ErrShortSeed) {
		t.Fatalf("short seed must fail with ErrShortSeed, got %v", err)
	}
	if g.PoolBytes() != 4 {
		t.Fatalf("rejected seed must leave the pool alone, got %d", g.PoolBytes())
	}
}

func TestReseedResetsPoolAndHealth(t *testing.T) {
	g := newEntropyGuard()
	seed := []byte("0123456789abcdef")
	if err := g.Reseed(seed); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if g.PoolBytes() != len(seed) {
		t.Fatalf("pool bytes = %d, want %d", g.PoolBytes(), len(seed))
	}
	out, err := g.Draw(2, false)
	if err != nil {
		t.Fatalf("draw after reseed: %v", err)
	}
	if string(out) != "01" {
		t.Fatalf("draw after reseed = %q, want 01", out)
	}
	if h := g.Health(); h.State != impl.StateDegraded {
		t.Fatalf("health after reseed without sources = %q, want %q", h.State, impl.StateDegraded)
	}
}

func TestSnapshotRestoreKeepsCredit(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcdefgh")
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newEntropyGuard()
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	credit, ok := restored.CreditOf("hwrng")
	if !ok || credit.Score != impl.CreditStep || credit.Source != "hwrng" {
		t.Fatalf("restored credit = %+v ok=%v, want hwrng at score %d", credit, ok, impl.CreditStep)
	}
	if h := restored.Health(); h.State != impl.StateStarved {
		t.Fatalf("restored health = %q, want %q until a fresh seed arrives", h.State, impl.StateStarved)
	}
	seed := []byte("0123456789abcdef0123456789abcdef")
	if err := restored.Reseed(seed); err != nil {
		t.Fatalf("reseed after restore: %v", err)
	}
	out, err := restored.Draw(2, false)
	if err != nil {
		t.Fatalf("draw after reseed: %v", err)
	}
	if string(out) != "01" {
		t.Fatalf("draw after reseed = %q, want 01", out)
	}
	if h := restored.Health(); h.State != impl.StateDegraded {
		t.Fatalf("health after reseed = %q, want %q", h.State, impl.StateDegraded)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	g := newEntropyGuard()
	fillPool(t, g, "abcd")
	if err := g.Restore([]byte("}{ broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if g.PoolBytes() != 4 {
		t.Fatalf("rejected restore must leave the pool alone, got %d", g.PoolBytes())
	}
	out, err := g.Draw(2, false)
	if err != nil {
		t.Fatalf("guard must keep serving after a rejected restore, got %v", err)
	}
	if string(out) != "ab" {
		t.Fatalf("draw after rejected restore = %q, want ab", out)
	}
}
