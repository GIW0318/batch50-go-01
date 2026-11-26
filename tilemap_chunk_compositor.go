// Package task compiles a layered tile map into per chunk draw instances and
// rebuilds dirty chunks inside a per frame rebuild budget.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// TileEmpty is the empty cell marker.
const TileEmpty TileID = ""

// TileTransparent paints nothing and hides nothing below it.
const TileTransparent TileID = "transparent"

// Defaults applied when the matching TilemapConfig field is left unset.
const (
	DefaultChunkSize           = 16
	DefaultCols                = 64
	DefaultRows                = 64
	DefaultMaxLayers           = 8
	DefaultMaxInstances        = 65536
	DefaultMaxLayerInstances   = 4096
	DefaultMaxRebuildsPerFrame = 4
)

const (
	snapshotMagic   = "tilemap-chunk-compositor-v1"
	snapshotVersion = 1
)

// Errors reported by the compositor.
var (
	ErrBadLayer       = errors.New("layer id is invalid")
	ErrDuplicateLayer = errors.New("layer already registered")
	ErrLayerLimit     = errors.New("layer limit reached")
	ErrUnknownLayer   = errors.New("layer not registered")
	ErrBadAnimation   = errors.New("tile animation is invalid")
	ErrOutOfRange     = errors.New("tile coordinate is outside the map")
	ErrUnknownChunk   = errors.New("chunk is outside the map")
	ErrBadOrder       = errors.New("layer order is invalid")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// TileID identifies one tile image.
type TileID string

// ChunkID identifies one chunk by its chunk coordinates.
type ChunkID string

// Camera is the visible tile rectangle of the active view.
type Camera struct {
	X int
	Y int
	W int
	H int
}

// TileInstance is one drawable tile of a compiled chunk.
type TileInstance struct {
	Layer int
	X     int
	Y     int
	Tile  TileID
	Frame int
}

// TilemapConfig bounds the map, the layers and the rebuild budget.
type TilemapConfig struct {
	ChunkSize           int
	Cols                int
	Rows                int
	MaxLayers           int
	MaxInstances        int
	MaxLayerInstances   int
	MaxRebuildsPerFrame int
}

type cellPos struct {
	X int
	Y int
}

type chunkPos struct {
	X int
	Y int
}

type tileRef struct {
	Layer int
	X     int
	Y     int
}

type chunkCache struct {
	Instances []TileInstance
	Dropped   map[int]int
}

// TilemapChunkCompositor owns the tile cells, the layer order, the compiled
// chunk caches and the dirty set.
type TilemapChunkCompositor struct {
	mu        sync.RWMutex
	cfg       TilemapConfig
	chunkCols int
	chunkRows int

	layerOrder []int
	layers     map[int]struct{}
	cells      map[int]map[cellPos]TileID

	animations    map[TileID]int
	tilePositions map[TileID]map[tileRef]struct{}
	tick          uint64

	caches  map[chunkPos]*chunkCache
	dirty   map[chunkPos]struct{}
	backlog []chunkPos

	usedInstances int
	layerUsed     map[int]int
	layerDropped  map[int]int
}

// NewTilemapChunkCompositor builds a compositor for the configured map.
func NewTilemapChunkCompositor(cfg TilemapConfig) *TilemapChunkCompositor {
	cfg = normalizeConfig(cfg)
	return &TilemapChunkCompositor{
		cfg:           cfg,
		chunkCols:     (cfg.Cols + cfg.ChunkSize - 1) / cfg.ChunkSize,
		chunkRows:     (cfg.Rows + cfg.ChunkSize - 1) / cfg.ChunkSize,
		layers:        make(map[int]struct{}),
		cells:         make(map[int]map[cellPos]TileID),
		animations:    make(map[TileID]int),
		tilePositions: make(map[TileID]map[tileRef]struct{}),
		caches:        make(map[chunkPos]*chunkCache),
		dirty:         make(map[chunkPos]struct{}),
		layerUsed:     make(map[int]int),
		layerDropped:  make(map[int]int),
	}
}

func normalizeConfig(cfg TilemapConfig) TilemapConfig {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	if cfg.Cols <= 0 {
		cfg.Cols = DefaultCols
	}
	if cfg.Rows <= 0 {
		cfg.Rows = DefaultRows
	}
	if cfg.MaxLayers <= 0 {
		cfg.MaxLayers = DefaultMaxLayers
	}
	if cfg.MaxInstances <= 0 {
		cfg.MaxInstances = DefaultMaxInstances
	}
	if cfg.MaxLayerInstances <= 0 {
		cfg.MaxLayerInstances = DefaultMaxLayerInstances
	}
	if cfg.MaxRebuildsPerFrame <= 0 {
		cfg.MaxRebuildsPerFrame = DefaultMaxRebuildsPerFrame
	}
	return cfg
}

// ChunkIDOf maps a tile coordinate onto its default-size chunk coordinates.
func ChunkIDOf(x, y int) ChunkID {
	return chunkID(chunkPos{
		X: floorDiv(x, DefaultChunkSize),
		Y: floorDiv(y, DefaultChunkSize),
	})
}

func floorDiv(value, divisor int) int {
	quotient := value / divisor
	remainder := value % divisor
	if remainder != 0 && ((remainder < 0) != (divisor < 0)) {
		quotient--
	}
	return quotient
}

func chunkID(pos chunkPos) ChunkID {
	return ChunkID(strconv.Itoa(pos.X) + "," + strconv.Itoa(pos.Y))
}

func parseChunkID(id ChunkID) (chunkPos, bool) {
	parts := strings.Split(string(id), ",")
	if len(parts) != 2 {
		return chunkPos{}, false
	}
	x, errX := strconv.Atoi(parts[0])
	y, errY := strconv.Atoi(parts[1])
	if errX != nil || errY != nil {
		return chunkPos{}, false
	}
	if strconv.Itoa(x) != parts[0] || strconv.Itoa(y) != parts[1] {
		return chunkPos{}, false
	}
	return chunkPos{X: x, Y: y}, true
}

func compareChunks(a, b chunkPos) bool {
	if a.Y != b.Y {
		return a.Y < b.Y
	}
	return a.X < b.X
}

func sortChunks(positions []chunkPos) {
	sort.Slice(positions, func(i, j int) bool {
		return compareChunks(positions[i], positions[j])
	})
}

func (c *TilemapChunkCompositor) resolveChunkID(id ChunkID) (chunkPos, error) {
	pos, ok := parseChunkID(id)
	if !ok || pos.X < 0 || pos.Y < 0 || pos.X >= c.chunkCols || pos.Y >= c.chunkRows {
		return chunkPos{}, ErrUnknownChunk
	}
	return pos, nil
}

// AddLayer registers a paint layer.
func (c *TilemapChunkCompositor) AddLayer(layer int) error {
	if layer < 0 {
		return ErrBadLayer
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.layers[layer]; exists {
		return ErrDuplicateLayer
	}
	if len(c.layers) >= c.cfg.MaxLayers {
		return ErrLayerLimit
	}

	c.layers[layer] = struct{}{}
	c.cells[layer] = make(map[cellPos]TileID)
	c.layerOrder = append(c.layerOrder, layer)
	return nil
}

// SetAnimatedFrames registers a tile as animated with the given frame count.
func (c *TilemapChunkCompositor) SetAnimatedFrames(tile TileID, frames int) error {
	if tile == TileEmpty || tile == TileTransparent || frames < 2 {
		return ErrBadAnimation
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.animations[tile] = frames
	for ref := range c.tilePositions[tile] {
		chunk := chunkPos{X: ref.X / c.cfg.ChunkSize, Y: ref.Y / c.cfg.ChunkSize}
		c.dirty[chunk] = struct{}{}
	}
	return nil
}

// SetTile writes one cell and marks its chunk dirty.
func (c *TilemapChunkCompositor) SetTile(layer, x, y int, tile TileID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.layers[layer]; !exists {
		return ErrUnknownLayer
	}
	if x < 0 || x >= c.cfg.Cols || y < 0 || y >= c.cfg.Rows {
		return ErrOutOfRange
	}

	pos := cellPos{X: x, Y: y}
	layerCells := c.cells[layer]
	ref := tileRef{Layer: layer, X: x, Y: y}
	if oldTile, exists := layerCells[pos]; exists && oldTile != TileEmpty {
		c.removeTileIndexLocked(oldTile, ref)
	}

	if tile == TileEmpty {
		delete(layerCells, pos)
	} else {
		layerCells[pos] = tile
		refs, ok := c.tilePositions[tile]
		if !ok {
			refs = make(map[tileRef]struct{})
			c.tilePositions[tile] = refs
		}
		refs[ref] = struct{}{}
	}

	chunk := chunkPos{X: x / c.cfg.ChunkSize, Y: y / c.cfg.ChunkSize}
	c.dirty[chunk] = struct{}{}
	return nil
}

func (c *TilemapChunkCompositor) removeTileIndexLocked(tile TileID, ref tileRef) {
	refs := c.tilePositions[tile]
	delete(refs, ref)
	if len(refs) == 0 {
		delete(c.tilePositions, tile)
	}
}

// Rebuild recompiles one chunk immediately and returns its instance count.
func (c *TilemapChunkCompositor) Rebuild(chunk ChunkID) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pos, err := c.resolveChunkID(chunk)
	if err != nil {
		return 0, err
	}

	c.rebuildLocked(pos)
	delete(c.dirty, pos)
	c.removeBacklogLocked(pos)
	return len(c.caches[pos].Instances), nil
}

func (c *TilemapChunkCompositor) removeBacklogLocked(target chunkPos) {
	for i, pos := range c.backlog {
		if pos == target {
			c.backlog = append(c.backlog[:i], c.backlog[i+1:]...)
			return
		}
	}
}

func (c *TilemapChunkCompositor) rebuildLocked(chunk chunkPos) {
	if old := c.caches[chunk]; old != nil {
		c.usedInstances -= len(old.Instances)
		for _, instance := range old.Instances {
			c.layerUsed[instance.Layer]--
			if c.layerUsed[instance.Layer] == 0 {
				delete(c.layerUsed, instance.Layer)
			}
		}
		for layer, count := range old.Dropped {
			c.layerDropped[layer] -= count
			if c.layerDropped[layer] <= 0 {
				delete(c.layerDropped, layer)
			}
		}
	}

	startX := chunk.X * c.cfg.ChunkSize
	startY := chunk.Y * c.cfg.ChunkSize
	endX := startX + c.cfg.ChunkSize
	endY := startY + c.cfg.ChunkSize
	if endX > c.cfg.Cols {
		endX = c.cfg.Cols
	}
	if endY > c.cfg.Rows {
		endY = c.cfg.Rows
	}

	instances := make([]TileInstance, 0, endX-startX)
	dropped := make(map[int]int)

compileCells:
	for y := startY; y < endY; y++ {
		for x := startX; x < endX; x++ {
			layer, tile, found := c.topOpaqueTileLocked(x, y)
			if !found {
				continue
			}
			if c.usedInstances >= c.cfg.MaxInstances {
				break compileCells
			}
			if c.layerUsed[layer] >= c.cfg.MaxLayerInstances {
				dropped[layer]++
				c.layerDropped[layer]++
				continue
			}

			frame := 0
			if frames, animated := c.animations[tile]; animated {
				frame = int(c.tick % uint64(frames))
			}
			instances = append(instances, TileInstance{
				Layer: layer,
				X:     x,
				Y:     y,
				Tile:  tile,
				Frame: frame,
			})
			c.usedInstances++
			c.layerUsed[layer]++
		}
	}

	c.caches[chunk] = &chunkCache{Instances: instances, Dropped: dropped}
}

func (c *TilemapChunkCompositor) topOpaqueTileLocked(x, y int) (int, TileID, bool) {
	pos := cellPos{X: x, Y: y}
	for i := len(c.layerOrder) - 1; i >= 0; i-- {
		layer := c.layerOrder[i]
		tile := c.cells[layer][pos]
		if tile == TileEmpty || tile == TileTransparent {
			continue
		}
		return layer, tile, true
	}
	return 0, TileEmpty, false
}

// Flush recompiles the dirty chunks that fit the per frame budget. Leftovers
// from the previous flush stay ahead of newly dirtied chunks.
func (c *TilemapChunkCompositor) Flush() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	plan := c.flushPlanLocked()
	budget := c.cfg.MaxRebuildsPerFrame
	if budget > len(plan) {
		budget = len(plan)
	}

	for i := 0; i < budget; i++ {
		pos := plan[i]
		if _, dirty := c.dirty[pos]; !dirty {
			continue
		}
		c.rebuildLocked(pos)
		delete(c.dirty, pos)
	}
	c.backlog = append([]chunkPos(nil), plan[budget:]...)
	return budget, nil
}

func (c *TilemapChunkCompositor) flushPlanLocked() []chunkPos {
	plan := make([]chunkPos, 0, len(c.dirty))
	planned := make(map[chunkPos]struct{}, len(c.dirty))

	for _, pos := range c.backlog {
		if _, dirty := c.dirty[pos]; !dirty {
			continue
		}
		if _, seen := planned[pos]; seen {
			continue
		}
		plan = append(plan, pos)
		planned[pos] = struct{}{}
	}

	fresh := make([]chunkPos, 0, len(c.dirty)-len(plan))
	for pos := range c.dirty {
		if _, seen := planned[pos]; !seen {
			fresh = append(fresh, pos)
		}
	}
	sortChunks(fresh)
	return append(plan, fresh...)
}

// DirtyChunks lists the chunks waiting for a rebuild in ascending order.
func (c *TilemapChunkCompositor) DirtyChunks() []ChunkID {
	c.mu.RLock()
	defer c.mu.RUnlock()

	positions := make([]chunkPos, 0, len(c.dirty))
	for pos := range c.dirty {
		positions = append(positions, pos)
	}
	sortChunks(positions)

	ids := make([]ChunkID, 0, len(positions))
	for _, pos := range positions {
		ids = append(ids, chunkID(pos))
	}
	return ids
}

// Visible lists the chunks intersecting the camera rectangle.
func (c *TilemapChunkCompositor) Visible(cam Camera) []ChunkID {
	if cam.W <= 0 || cam.H <= 0 {
		return []ChunkID{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	left := cam.X
	if left < 0 {
		left = 0
	}
	top := cam.Y
	if top < 0 {
		top = 0
	}
	right := cam.X + cam.W
	if right > c.cfg.Cols {
		right = c.cfg.Cols
	}
	bottom := cam.Y + cam.H
	if bottom > c.cfg.Rows {
		bottom = c.cfg.Rows
	}
	if left >= right || top >= bottom {
		return []ChunkID{}
	}

	startChunkX := left / c.cfg.ChunkSize
	endChunkX := (right - 1) / c.cfg.ChunkSize
	startChunkY := top / c.cfg.ChunkSize
	endChunkY := (bottom - 1) / c.cfg.ChunkSize

	ids := make([]ChunkID, 0, (endChunkX-startChunkX+1)*(endChunkY-startChunkY+1))
	for y := startChunkY; y <= endChunkY; y++ {
		for x := startChunkX; x <= endChunkX; x++ {
			ids = append(ids, chunkID(chunkPos{X: x, Y: y}))
		}
	}
	return ids
}

// Animate advances the shared animation tick and dirties affected chunks.
func (c *TilemapChunkCompositor) Animate(chunk ChunkID, tick uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.resolveChunkID(chunk); err != nil {
		return err
	}

	oldTick := c.tick
	c.tick = tick
	if !c.animationPhaseChangedLocked(oldTick, tick) {
		return nil
	}

	affected := make(map[chunkPos]struct{})
	for tile := range c.animations {
		for ref := range c.tilePositions[tile] {
			pos := chunkPos{X: ref.X / c.cfg.ChunkSize, Y: ref.Y / c.cfg.ChunkSize}
			affected[pos] = struct{}{}
		}
	}
	for pos := range affected {
		c.dirty[pos] = struct{}{}
	}
	return nil
}

func (c *TilemapChunkCompositor) animationPhaseChangedLocked(oldTick, newTick uint64) bool {
	if oldTick == newTick {
		return false
	}
	for _, frames := range c.animations {
		if oldTick%uint64(frames) != newTick%uint64(frames) {
			return true
		}
	}
	return false
}

// SetLayerOrder rewrites the paint order from bottom to top.
func (c *TilemapChunkCompositor) SetLayerOrder(order []int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(order) != len(c.layerOrder) {
		return ErrBadOrder
	}
	seen := make(map[int]struct{}, len(order))
	for _, layer := range order {
		if _, registered := c.layers[layer]; !registered {
			return ErrBadOrder
		}
		if _, duplicate := seen[layer]; duplicate {
			return ErrBadOrder
		}
		seen[layer] = struct{}{}
	}

	c.layerOrder = append([]int(nil), order...)
	for _, cells := range c.cells {
		for pos := range cells {
			chunk := chunkPos{X: pos.X / c.cfg.ChunkSize, Y: pos.Y / c.cfg.ChunkSize}
			c.dirty[chunk] = struct{}{}
		}
	}
	return nil
}

// LayerOrder reads the current paint order.
func (c *TilemapChunkCompositor) LayerOrder() []int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]int(nil), c.layerOrder...)
}

// DrawList reads the compiled instance list of one chunk.
func (c *TilemapChunkCompositor) DrawList(chunk ChunkID) ([]TileInstance, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	pos, err := c.resolveChunkID(chunk)
	if err != nil {
		return nil, err
	}
	cache := c.caches[pos]
	if cache == nil {
		return []TileInstance{}, nil
	}
	return append([]TileInstance{}, cache.Instances...), nil
}

// Budget reports the compiled instance total and the configured hard cap.
func (c *TilemapChunkCompositor) Budget() (used, limit int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usedInstances, c.cfg.MaxInstances
}

// LayerBudget reports the compiled instance count of one layer.
func (c *TilemapChunkCompositor) LayerBudget(layer int) (used, limit int, degraded bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.layerUsed[layer], c.cfg.MaxLayerInstances, c.layerDropped[layer] > 0
}

type snapshotEnvelope struct {
	Magic    string `json:"magic"`
	Version  int    `json:"version"`
	Checksum string `json:"checksum"`
	Data     []byte `json:"data"`
}

type snapshotCell struct {
	Layer int    `json:"layer"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Tile  TileID `json:"tile"`
}

type snapshotCache struct {
	ID        ChunkID        `json:"id"`
	Instances []TileInstance `json:"instances"`
	Dropped   map[int]int    `json:"dropped,omitempty"`
}

type snapshotData struct {
	LayerOrder []int           `json:"layer_order"`
	Cells      []snapshotCell  `json:"cells"`
	Animations map[string]int  `json:"animations"`
	Tick       uint64          `json:"tick"`
	Dirty      []ChunkID       `json:"dirty"`
	Caches     []snapshotCache `json:"caches"`
}

// Snapshot serialises the cells, the layer order, the tick and the dirty set.
func (c *TilemapChunkCompositor) Snapshot() ([]byte, error) {
	c.mu.RLock()
	data := c.snapshotDataLocked()
	c.mu.RUnlock()

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(checksum[:]),
		Data:     payload,
	}
	return json.Marshal(envelope)
}

func (c *TilemapChunkCompositor) snapshotDataLocked() snapshotData {
	data := snapshotData{
		LayerOrder: append([]int(nil), c.layerOrder...),
		Animations: make(map[string]int, len(c.animations)),
		Tick:       c.tick,
	}
	for tile, frames := range c.animations {
		data.Animations[string(tile)] = frames
	}

	for layer, cells := range c.cells {
		for pos, tile := range cells {
			data.Cells = append(data.Cells, snapshotCell{
				Layer: layer,
				X:     pos.X,
				Y:     pos.Y,
				Tile:  tile,
			})
		}
	}
	sort.Slice(data.Cells, func(i, j int) bool {
		a, b := data.Cells[i], data.Cells[j]
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		if a.X != b.X {
			return a.X < b.X
		}
		return a.Layer < b.Layer
	})

	dirtyPositions := make([]chunkPos, 0, len(c.dirty))
	for pos := range c.dirty {
		dirtyPositions = append(dirtyPositions, pos)
	}
	sortChunks(dirtyPositions)
	for _, pos := range dirtyPositions {
		data.Dirty = append(data.Dirty, chunkID(pos))
	}

	cachePositions := make([]chunkPos, 0, len(c.caches))
	for pos := range c.caches {
		cachePositions = append(cachePositions, pos)
	}
	sortChunks(cachePositions)
	for _, pos := range cachePositions {
		cache := c.caches[pos]
		dropped := make(map[int]int, len(cache.Dropped))
		for layer, count := range cache.Dropped {
			dropped[layer] = count
		}
		data.Caches = append(data.Caches, snapshotCache{
			ID:        chunkID(pos),
			Instances: append([]TileInstance(nil), cache.Instances...),
			Dropped:   dropped,
		})
	}
	return data
}

// Restore replaces the whole state with a snapshot payload.
func (c *TilemapChunkCompositor) Restore(raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cells == nil {
		c.cfg = normalizeConfig(c.cfg)
		c.chunkCols = (c.cfg.Cols + c.cfg.ChunkSize - 1) / c.cfg.ChunkSize
		c.chunkRows = (c.cfg.Rows + c.cfg.ChunkSize - 1) / c.cfg.ChunkSize
		c.layers = make(map[int]struct{})
		c.cells = make(map[int]map[cellPos]TileID)
		c.animations = make(map[TileID]int)
		c.tilePositions = make(map[TileID]map[tileRef]struct{})
		c.caches = make(map[chunkPos]*chunkCache)
		c.dirty = make(map[chunkPos]struct{})
		c.layerUsed = make(map[int]int)
		c.layerDropped = make(map[int]int)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	var envelope snapshotEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("%w: decode envelope: %v", ErrBadSnapshot, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing snapshot data", ErrBadSnapshot)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion ||
		envelope.Checksum == "" || len(envelope.Data) == 0 {
		return fmt.Errorf("%w: invalid envelope", ErrBadSnapshot)
	}

	checksum := sha256.Sum256(envelope.Data)
	if hex.EncodeToString(checksum[:]) != envelope.Checksum {
		return fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var data snapshotData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return fmt.Errorf("%w: decode payload: %v", ErrBadSnapshot, err)
	}
	state, err := c.buildRestoreStateLocked(&data)
	if err != nil {
		return err
	}

	c.layerOrder = state.order
	c.layers = state.layers
	c.cells = state.cells
	c.animations = state.animations
	c.tilePositions = state.tilePositions
	c.tick = data.Tick
	c.caches = state.caches
	c.dirty = state.dirty
	c.backlog = state.backlog
	c.usedInstances = state.usedInstances
	c.layerUsed = state.layerUsed
	c.layerDropped = state.layerDropped
	return nil
}

type restoredState struct {
	order         []int
	layers        map[int]struct{}
	cells         map[int]map[cellPos]TileID
	animations    map[TileID]int
	tilePositions map[TileID]map[tileRef]struct{}
	caches        map[chunkPos]*chunkCache
	dirty         map[chunkPos]struct{}
	backlog       []chunkPos
	usedInstances int
	layerUsed     map[int]int
	layerDropped  map[int]int
}

func (c *TilemapChunkCompositor) buildRestoreStateLocked(data *snapshotData) (restoredState, error) {
	state := restoredState{
		order:         append([]int(nil), data.LayerOrder...),
		layers:        make(map[int]struct{}, len(data.LayerOrder)),
		cells:         make(map[int]map[cellPos]TileID, len(data.LayerOrder)),
		animations:    make(map[TileID]int, len(data.Animations)),
		tilePositions: make(map[TileID]map[tileRef]struct{}),
		caches:        make(map[chunkPos]*chunkCache, len(data.Caches)),
		dirty:         make(map[chunkPos]struct{}, len(data.Dirty)),
		layerUsed:     make(map[int]int),
		layerDropped:  make(map[int]int),
	}

	for _, layer := range state.order {
		if layer < 0 {
			return restoredState{}, fmt.Errorf("%w: negative layer", ErrBadSnapshot)
		}
		if _, duplicate := state.layers[layer]; duplicate {
			return restoredState{}, fmt.Errorf("%w: duplicate layer", ErrBadSnapshot)
		}
		state.layers[layer] = struct{}{}
		state.cells[layer] = make(map[cellPos]TileID)
	}

	seenCells := make(map[tileRef]struct{}, len(data.Cells))
	for _, savedCell := range data.Cells {
		if _, ok := state.layers[savedCell.Layer]; !ok {
			return restoredState{}, fmt.Errorf("%w: unknown cell layer", ErrBadSnapshot)
		}
		if savedCell.X < 0 || savedCell.X >= c.cfg.Cols ||
			savedCell.Y < 0 || savedCell.Y >= c.cfg.Rows {
			return restoredState{}, fmt.Errorf("%w: cell out of range", ErrBadSnapshot)
		}
		ref := tileRef{Layer: savedCell.Layer, X: savedCell.X, Y: savedCell.Y}
		if _, duplicate := seenCells[ref]; duplicate {
			return restoredState{}, fmt.Errorf("%w: duplicate cell", ErrBadSnapshot)
		}
		seenCells[ref] = struct{}{}
		if savedCell.Tile == TileEmpty {
			continue
		}

		pos := cellPos{X: savedCell.X, Y: savedCell.Y}
		state.cells[savedCell.Layer][pos] = savedCell.Tile
		refs, ok := state.tilePositions[savedCell.Tile]
		if !ok {
			refs = make(map[tileRef]struct{})
			state.tilePositions[savedCell.Tile] = refs
		}
		refs[ref] = struct{}{}
	}

	for tile, frames := range data.Animations {
		id := TileID(tile)
		if id == TileEmpty || id == TileTransparent || frames < 2 {
			return restoredState{}, fmt.Errorf("%w: bad animation", ErrBadSnapshot)
		}
		state.animations[id] = frames
	}

	for _, id := range data.Dirty {
		pos, err := c.resolveChunkID(id)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: dirty chunk: %v", ErrBadSnapshot, err)
		}
		state.dirty[pos] = struct{}{}
	}
	state.backlog = make([]chunkPos, 0, len(state.dirty))
	for pos := range state.dirty {
		state.backlog = append(state.backlog, pos)
	}
	sortChunks(state.backlog)

	for _, savedCache := range data.Caches {
		pos, err := c.resolveChunkID(savedCache.ID)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: cached chunk: %v", ErrBadSnapshot, err)
		}
		if _, duplicate := state.caches[pos]; duplicate {
			return restoredState{}, fmt.Errorf("%w: duplicate cached chunk", ErrBadSnapshot)
		}

		instances := make([]TileInstance, 0, len(savedCache.Instances))
		for _, instance := range savedCache.Instances {
			if _, ok := state.layers[instance.Layer]; !ok {
				return restoredState{}, fmt.Errorf("%w: cached layer", ErrBadSnapshot)
			}
			if instance.X < 0 || instance.X >= c.cfg.Cols ||
				instance.Y < 0 || instance.Y >= c.cfg.Rows || instance.Frame < 0 {
				return restoredState{}, fmt.Errorf("%w: cached instance", ErrBadSnapshot)
			}
			instanceChunk := chunkPos{
				X: instance.X / c.cfg.ChunkSize,
				Y: instance.Y / c.cfg.ChunkSize,
			}
			if instanceChunk != pos {
				return restoredState{}, fmt.Errorf("%w: instance chunk mismatch", ErrBadSnapshot)
			}
			instances = append(instances, instance)
			state.usedInstances++
			state.layerUsed[instance.Layer]++
		}

		dropped := make(map[int]int, len(savedCache.Dropped))
		for layer, count := range savedCache.Dropped {
			if _, ok := state.layers[layer]; !ok || count < 0 {
				return restoredState{}, fmt.Errorf("%w: cached layer budget", ErrBadSnapshot)
			}
			dropped[layer] = count
			state.layerDropped[layer] += count
		}
		state.caches[pos] = &chunkCache{Instances: instances, Dropped: dropped}
	}

	if state.usedInstances > c.cfg.MaxInstances {
		return restoredState{}, fmt.Errorf("%w: global budget exceeded", ErrBadSnapshot)
	}
	for layer, used := range state.layerUsed {
		if used > c.cfg.MaxLayerInstances {
			return restoredState{}, fmt.Errorf("%w: layer budget exceeded: layer %d", ErrBadSnapshot, layer)
		}
	}
	return state, nil
}
