package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-game-b-14"
)

func tileConfig() impl.TilemapConfig {
	return impl.TilemapConfig{
		ChunkSize:           16,
		Cols:                64,
		Rows:                64,
		MaxLayers:           8,
		MaxInstances:        1024,
		MaxLayerInstances:   512,
		MaxRebuildsPerFrame: 4,
	}
}

func newCompositor(t *testing.T, cfg impl.TilemapConfig, layers ...int) *impl.TilemapChunkCompositor {
	t.Helper()
	c := impl.NewTilemapChunkCompositor(cfg)
	for _, layer := range layers {
		if err := c.AddLayer(layer); err != nil {
			t.Fatalf("add layer %d: %v", layer, err)
		}
	}
	return c
}

func mustTile(t *testing.T, c *impl.TilemapChunkCompositor, layer, x, y int, tile impl.TileID) {
	t.Helper()
	if err := c.SetTile(layer, x, y, tile); err != nil {
		t.Fatalf("set tile layer %d at %d,%d: %v", layer, x, y, err)
	}
}

func mustRebuild(t *testing.T, c *impl.TilemapChunkCompositor, x, y int) int {
	t.Helper()
	n, err := c.Rebuild(impl.ChunkIDOf(x, y))
	if err != nil {
		t.Fatalf("rebuild chunk of %d,%d: %v", x, y, err)
	}
	return n
}

func TestAddLayerValidation(t *testing.T) {
	cfg := tileConfig()
	cfg.MaxLayers = 2
	c := newCompositor(t, cfg)
	if err := c.AddLayer(-1); !errors.Is(err, impl.ErrBadLayer) {
		t.Fatalf("negative layer must fail with ErrBadLayer, got %v", err)
	}
	if err := c.AddLayer(0); err != nil {
		t.Fatalf("add layer 0: %v", err)
	}
	if err := c.AddLayer(1); err != nil {
		t.Fatalf("add layer 1: %v", err)
	}
	if err := c.AddLayer(1); !errors.Is(err, impl.ErrDuplicateLayer) {
		t.Fatalf("duplicate layer must fail with ErrDuplicateLayer, got %v", err)
	}
	if err := c.AddLayer(2); !errors.Is(err, impl.ErrLayerLimit) {
		t.Fatalf("third layer must fail with ErrLayerLimit, got %v", err)
	}
	order := c.LayerOrder()
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Fatalf("layer order = %v, want the registration order [0 1]", order)
	}
}

func TestSetTileValidation(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	if err := c.SetTile(5, 0, 0, "grass"); !errors.Is(err, impl.ErrUnknownLayer) {
		t.Fatalf("unregistered layer must fail with ErrUnknownLayer, got %v", err)
	}
	if err := c.SetTile(0, -1, 0, "grass"); !errors.Is(err, impl.ErrOutOfRange) {
		t.Fatalf("negative x must fail with ErrOutOfRange, got %v", err)
	}
	if err := c.SetTile(0, 0, 64, "grass"); !errors.Is(err, impl.ErrOutOfRange) {
		t.Fatalf("y past the map must fail with ErrOutOfRange, got %v", err)
	}
	if got := c.DirtyChunks(); len(got) != 0 {
		t.Fatalf("rejected writes dirtied %v, want nothing", got)
	}
	mustTile(t, c, 0, 0, 0, "grass")
	dirty := c.DirtyChunks()
	if len(dirty) != 1 || dirty[0] != impl.ChunkIDOf(0, 0) {
		t.Fatalf("dirty chunks = %v, want just the touched chunk", dirty)
	}
	mustTile(t, c, 0, 1, 0, "grass")
	if got := len(c.DirtyChunks()); got != 1 {
		t.Fatalf("a second write in the same chunk made %d entries, want 1", got)
	}
}

func TestRebuildCompilesWithOcclusion(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0, 1)
	mustTile(t, c, 0, 0, 0, "grass")
	mustTile(t, c, 0, 1, 0, "sand")
	mustTile(t, c, 1, 0, 0, "rock")
	n := mustRebuild(t, c, 0, 0)
	if n != 2 {
		t.Fatalf("compiled %d instances, want one per non empty cell", n)
	}
	list, err := c.DrawList(impl.ChunkIDOf(0, 0))
	if err != nil {
		t.Fatalf("draw list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("draw list length = %d, want 2", len(list))
	}
	first := list[0]
	if first.Layer != 1 || first.X != 0 || first.Y != 0 || first.Tile != "rock" {
		t.Fatalf("first instance = %+v, want the upper layer rock at 0,0", first)
	}
	second := list[1]
	if second.Layer != 0 || second.X != 1 || second.Y != 0 || second.Tile != "sand" {
		t.Fatalf("second instance = %+v, want sand at 1,0", second)
	}
	if len(c.DirtyChunks()) != 0 {
		t.Fatalf("rebuild must clear its own dirty entry")
	}
}

func TestOneInstancePerCellWithStackedLayers(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0, 1)
	for x := 0; x < 3; x++ {
		mustTile(t, c, 0, x, 2, "grass")
		mustTile(t, c, 1, x, 2, "rock")
	}
	n := mustRebuild(t, c, 0, 0)
	if n != 3 {
		t.Fatalf("stacked layers compiled %d instances, want one per cell and three cells", n)
	}
	list, err := c.DrawList(impl.ChunkIDOf(0, 0))
	if err != nil {
		t.Fatalf("draw list: %v", err)
	}
	for i, inst := range list {
		if inst.Layer != 1 || inst.Tile != "rock" {
			t.Fatalf("instance %d = %+v, want the top layer rock", i, inst)
		}
		if inst.X != i || inst.Y != 2 {
			t.Fatalf("instance %d = %+v, want it in ascending x order on row 2", i, inst)
		}
	}
}

func TestRebuildClearsOnlyItsChunk(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	mustTile(t, c, 0, 0, 0, "grass")
	mustTile(t, c, 0, 16, 0, "sand")
	dirty := c.DirtyChunks()
	if len(dirty) != 2 {
		t.Fatalf("dirty chunks = %v, want two", dirty)
	}
	if dirty[0] != impl.ChunkIDOf(0, 0) || dirty[1] != impl.ChunkIDOf(16, 0) {
		t.Fatalf("dirty chunks = %v, want them in ascending order", dirty)
	}
	if n := mustRebuild(t, c, 0, 0); n != 1 {
		t.Fatalf("rebuilt chunk holds %d instances, want 1", n)
	}
	dirty = c.DirtyChunks()
	if len(dirty) != 1 || dirty[0] != impl.ChunkIDOf(16, 0) {
		t.Fatalf("dirty chunks after one rebuild = %v, want the untouched one", dirty)
	}
	if n := mustRebuild(t, c, 0, 16); n != 0 {
		t.Fatalf("an empty chunk compiled %d instances, want 0", n)
	}
	if got := len(c.DirtyChunks()); got != 1 {
		t.Fatalf("rebuilding an undirtied chunk left %d dirty entries, want the untouched one", got)
	}
	if n := mustRebuild(t, c, 16, 0); n != 1 {
		t.Fatalf("second chunk holds %d instances, want 1", n)
	}
	if got := len(c.DirtyChunks()); got != 0 {
		t.Fatalf("dirty chunks = %d after rebuilding both, want 0", got)
	}
}

func TestFlushRespectsRebuildBudget(t *testing.T) {
	cfg := tileConfig()
	cfg.MaxRebuildsPerFrame = 1
	c := newCompositor(t, cfg, 0)
	mustTile(t, c, 0, 0, 0, "grass")
	mustTile(t, c, 0, 16, 0, "grass")
	mustTile(t, c, 0, 32, 0, "grass")
	if got := len(c.DirtyChunks()); got != 3 {
		t.Fatalf("dirty chunks = %d, want 3", got)
	}
	if n, err := c.Flush(); err != nil || n != 1 {
		t.Fatalf("first flush = %d err=%v, want exactly the budget 1", n, err)
	}
	dirty := c.DirtyChunks()
	if len(dirty) != 2 || dirty[0] != impl.ChunkIDOf(16, 0) {
		t.Fatalf("dirty chunks after the first flush = %v, want the two newer ones", dirty)
	}
	if n, err := c.Flush(); err != nil || n != 1 {
		t.Fatalf("second flush = %d err=%v, want 1", n, err)
	}
	if n, err := c.Flush(); err != nil || n != 1 {
		t.Fatalf("third flush = %d err=%v, want the last one", n, err)
	}
	if n, err := c.Flush(); err != nil || n != 0 {
		t.Fatalf("flush with nothing dirty = %d err=%v, want 0", n, err)
	}
}

func TestVisibleIntersectsCamera(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	near := c.Visible(impl.Camera{X: 0, Y: 0, W: 32, H: 16})
	if len(near) != 2 {
		t.Fatalf("visible chunks = %v, want the two chunks on the first row", near)
	}
	if near[0] != impl.ChunkIDOf(0, 0) || near[1] != impl.ChunkIDOf(16, 0) {
		t.Fatalf("visible chunks = %v, want them ordered by row then column", near)
	}
	corner := c.Visible(impl.Camera{X: 63, Y: 63, W: 16, H: 16})
	if len(corner) != 1 || corner[0] != impl.ChunkIDOf(63, 63) {
		t.Fatalf("corner camera sees %v, want only the last chunk", corner)
	}
	if got := c.Visible(impl.Camera{X: 0, Y: 0, W: 0, H: 16}); len(got) != 0 {
		t.Fatalf("empty camera width returned %v, want nothing", got)
	}
}

func TestAnimateSharesTickAcrossChunks(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	if err := c.SetAnimatedFrames("torch", 3); err != nil {
		t.Fatalf("register animation: %v", err)
	}
	mustTile(t, c, 0, 0, 0, "torch")
	mustTile(t, c, 0, 16, 0, "torch")
	if n := mustRebuild(t, c, 0, 0); n != 1 {
		t.Fatalf("first chunk holds %d instances, want 1", n)
	}
	if n := mustRebuild(t, c, 16, 0); n != 1 {
		t.Fatalf("second chunk holds %d instances, want 1", n)
	}
	first, err := c.DrawList(impl.ChunkIDOf(0, 0))
	if err != nil {
		t.Fatalf("draw list: %v", err)
	}
	if first[0].Frame != 0 {
		t.Fatalf("frame at tick zero = %d, want 0", first[0].Frame)
	}
	if err := c.Animate(impl.ChunkIDOf(0, 0), 4); err != nil {
		t.Fatalf("animate: %v", err)
	}
	if got := len(c.DirtyChunks()); got != 2 {
		t.Fatalf("dirty chunks after the tick change = %d, want both torch chunks", got)
	}
	if n := mustRebuild(t, c, 16, 0); n != 1 {
		t.Fatalf("second chunk holds %d instances, want 1", n)
	}
	second, err := c.DrawList(impl.ChunkIDOf(16, 0))
	if err != nil {
		t.Fatalf("draw list: %v", err)
	}
	if second[0].Frame != 1 {
		t.Fatalf("frame at tick four = %d, want the shared phase 1", second[0].Frame)
	}
	if err := c.Animate(impl.ChunkIDOf(64, 64), 5); !errors.Is(err, impl.ErrUnknownChunk) {
		t.Fatalf("animating an off map chunk must fail with ErrUnknownChunk, got %v", err)
	}
}

func TestSetAnimatedFramesValidation(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	if err := c.SetAnimatedFrames("", 3); !errors.Is(err, impl.ErrBadAnimation) {
		t.Fatalf("empty tile must fail with ErrBadAnimation, got %v", err)
	}
	if err := c.SetAnimatedFrames(impl.TileTransparent, 3); !errors.Is(err, impl.ErrBadAnimation) {
		t.Fatalf("the transparent marker must fail with ErrBadAnimation, got %v", err)
	}
	if err := c.SetAnimatedFrames("torch", 1); !errors.Is(err, impl.ErrBadAnimation) {
		t.Fatalf("a single frame must fail with ErrBadAnimation, got %v", err)
	}
	if err := c.SetAnimatedFrames("torch", 3); err != nil {
		t.Fatalf("register animation: %v", err)
	}
}

func TestSetLayerOrderValidation(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0, 1)
	if err := c.SetLayerOrder([]int{0}); !errors.Is(err, impl.ErrBadOrder) {
		t.Fatalf("a partial order must fail with ErrBadOrder, got %v", err)
	}
	if err := c.SetLayerOrder([]int{0, 0}); !errors.Is(err, impl.ErrBadOrder) {
		t.Fatalf("a duplicated layer must fail with ErrBadOrder, got %v", err)
	}
	if err := c.SetLayerOrder([]int{0, 2}); !errors.Is(err, impl.ErrBadOrder) {
		t.Fatalf("an unregistered layer must fail with ErrBadOrder, got %v", err)
	}
	if err := c.SetLayerOrder([]int{1, 0}); err != nil {
		t.Fatalf("set layer order: %v", err)
	}
	order := c.LayerOrder()
	if len(order) != 2 || order[0] != 1 || order[1] != 0 {
		t.Fatalf("layer order = %v, want the requested [1 0]", order)
	}
}

func TestRebuildRejectsUnknownChunk(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	if _, err := c.Rebuild("9,9"); !errors.Is(err, impl.ErrUnknownChunk) {
		t.Fatalf("off map rebuild must fail with ErrUnknownChunk, got %v", err)
	}
	if _, err := c.DrawList("9,9"); !errors.Is(err, impl.ErrUnknownChunk) {
		t.Fatalf("off map draw list must fail with ErrUnknownChunk, got %v", err)
	}
	mustTile(t, c, 0, 0, 0, "grass")
	if _, err := c.Rebuild(""); !errors.Is(err, impl.ErrUnknownChunk) {
		t.Fatalf("empty chunk id must fail with ErrUnknownChunk, got %v", err)
	}
}

func TestLayerBudgetCapsAndFlags(t *testing.T) {
	cfg := tileConfig()
	cfg.MaxLayerInstances = 2
	c := newCompositor(t, cfg, 0)
	for x := 0; x < 3; x++ {
		mustTile(t, c, 0, x, 0, "grass")
	}
	n := mustRebuild(t, c, 0, 0)
	if n != 2 {
		t.Fatalf("compiled %d instances under a layer cap of 2, want 2", n)
	}
	used, limit, degraded := c.LayerBudget(0)
	if used != 2 || limit != 2 || !degraded {
		t.Fatalf("layer budget = used %d limit %d degraded %v, want 2/2/true", used, limit, degraded)
	}
	list, err := c.DrawList(impl.ChunkIDOf(0, 0))
	if err != nil {
		t.Fatalf("draw list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("draw list length = %d, want the capped 2", len(list))
	}
	if list[0].X != 0 || list[1].X != 1 {
		t.Fatalf("kept instances = %+v, want the first two cells", list)
	}
}

func TestBudgetCountsCompiledInstances(t *testing.T) {
	cfg := tileConfig()
	cfg.MaxInstances = 2
	c := newCompositor(t, cfg, 0, 1, 2)
	mustTile(t, c, 0, 0, 0, "grass")
	mustTile(t, c, 1, 1, 0, "sand")
	mustTile(t, c, 2, 2, 0, "rock")
	n := mustRebuild(t, c, 0, 0)
	if n != 2 {
		t.Fatalf("compiled %d instances under a global cap of 2, want 2", n)
	}
	used, limit := c.Budget()
	if used != 2 || limit != 2 {
		t.Fatalf("budget = %d/%d, want 2/2", used, limit)
	}
}

func TestSnapshotRestoreKeepsDirtyAndTick(t *testing.T) {
	cfg := tileConfig()
	c := newCompositor(t, cfg, 0, 1)
	if err := c.SetAnimatedFrames("torch", 3); err != nil {
		t.Fatalf("register animation: %v", err)
	}
	mustTile(t, c, 0, 0, 0, "torch")
	mustTile(t, c, 1, 1, 1, "rock")
	if n := mustRebuild(t, c, 0, 0); n != 2 {
		t.Fatalf("chunk holds %d instances, want 2", n)
	}
	if err := c.Animate(impl.ChunkIDOf(0, 0), 5); err != nil {
		t.Fatalf("animate: %v", err)
	}
	if got := len(c.DirtyChunks()); got != 1 {
		t.Fatalf("dirty chunks = %d, want the animated one", got)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewTilemapChunkCompositor(cfg)
	for _, layer := range []int{0, 1} {
		if err := restored.AddLayer(layer); err != nil {
			t.Fatalf("add layer %d after restore: %v", layer, err)
		}
	}
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	dirty := restored.DirtyChunks()
	if len(dirty) != 1 || dirty[0] != impl.ChunkIDOf(0, 0) {
		t.Fatalf("restored dirty chunks = %v, want the animated chunk", dirty)
	}
	if n := mustRebuild(t, restored, 0, 0); n != 2 {
		t.Fatalf("restored chunk holds %d instances, want 2", n)
	}
	list, err := restored.DrawList(impl.ChunkIDOf(0, 0))
	if err != nil {
		t.Fatalf("restored draw list: %v", err)
	}
	if list[0].Tile != "torch" || list[0].Frame != 2 {
		t.Fatalf("restored first instance = %+v, want torch at the carried phase 2", list[0])
	}
	order := restored.LayerOrder()
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Fatalf("restored layer order = %v, want [0 1]", order)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	c := newCompositor(t, tileConfig(), 0)
	mustTile(t, c, 0, 0, 0, "grass")
	if err := c.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must fail with ErrBadSnapshot, got %v", err)
	}
	if n := mustRebuild(t, c, 0, 0); n != 1 {
		t.Fatalf("chunk after the failed restore holds %d instances, want 1", n)
	}
	order := c.LayerOrder()
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("failed restore changed the layer order to %v", order)
	}
}
