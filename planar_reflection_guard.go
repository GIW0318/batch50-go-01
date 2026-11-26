// Package task builds mirror cameras for planar reflections, keeps the oblique
// near plane of each mirror consistent with the reflecting plane and paces the
// reflection atlas updates of every registered plane.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
)

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxPlanes   = 16
	DefaultAtlasWidth  = 4096
	DefaultAtlasHeight = 4096
)

// Errors reported by the reflection guard.
var (
	ErrBadPlane       = errors.New("plane specification is invalid")
	ErrDuplicatePlane = errors.New("plane is already registered")
	ErrPlaneLimit     = errors.New("plane table is full")
	ErrUnknownPlane   = errors.New("plane is not registered")
	ErrBadCamera      = errors.New("camera parameters are invalid")
	ErrBadUpdateRate  = errors.New("update rate must be positive")
	ErrBadFrame       = errors.New("frame number must strictly increase")
	ErrBadResolution  = errors.New("reflection resolution is invalid")
	ErrAtlasBudget    = errors.New("reflection atlas budget is exceeded")
	ErrNoFrame        = errors.New("no frame is open")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Vec3 is a position or direction in world space.
type Vec3 struct {
	X float64
	Y float64
	Z float64
}

// Vec4 is a clip plane written as (x, y, z, w); the kept half space is the one
// where x*X+y*Y+z*Z+w is not negative.
type Vec4 struct {
	X float64
	Y float64
	Z float64
	W float64
}

// Limits bounds the number of planes and the shared reflection atlas.
type Limits struct {
	MaxPlanes   int
	AtlasWidth  int
	AtlasHeight int
}

// PlaneSpec describes one reflecting plane.
type PlaneSpec struct {
	Point    Vec3
	Normal   Vec3
	Priority int
	Width    int
	Height   int
}

// CameraParams describes the player camera used to build a mirror camera.
type CameraParams struct {
	Position Vec3
	Forward  Vec3
	Up       Vec3
	FOV      float64
	Near     float64
	Far      float64
}

// MirrorCamera is the reflected camera with the oblique near plane applied.
type MirrorCamera struct {
	PlaneID   string
	Position  Vec3
	Forward   Vec3
	Up        Vec3
	Near      float64
	Far       float64
	ClipPlane Vec4
	Enabled   bool
	Reason    string
}

// AtlasEntry is the viewport one plane owns inside the shared atlas.
type AtlasEntry struct {
	PlaneID string
	X       int
	Y       int
	W       int
	H       int
}

// AtlasLayout is the current packing of the shared reflection atlas.
type AtlasLayout struct {
	Width   int
	Height  int
	Entries []AtlasEntry
}

// ReflectionState is what State and Commit report for one plane.
type ReflectionState struct {
	PlaneID    string
	Generation int64
	Frame      int
	EveryN     int
	Updated    bool
	AtlasX     int
	AtlasY     int
	AtlasW     int
	AtlasH     int
	Enabled    bool
}

// ReflectionStats counts the atlas traffic of the guard.
type ReflectionStats struct {
	Planes  int
	Updates int64
	Skipped int64
}

type planeRecord struct {
	id          string
	point       Vec3
	normal      Vec3
	priority    int
	width       int
	height      int
	everyN      int
	generation  int64
	lastFrame   int
	hasRendered bool
	updated     bool
	invalidated bool
	enabled     bool
	reason      string
	seq         int
	atlasX      int
	atlasY      int
}

// PlanarReflectionGuard owns the plane table, the atlas packing and the frame
// counter used for update pacing.
type PlanarReflectionGuard struct {
	mu        sync.Mutex
	limits    Limits
	planes    map[string]*planeRecord
	frame     int
	frameOpen bool
	nextSeq   int
	updates   int64
	skipped   int64
}

// NewPlanarReflectionGuard builds a guard with the given caps.
func NewPlanarReflectionGuard(limits Limits) *PlanarReflectionGuard {
	return &PlanarReflectionGuard{
		limits: normalizeLimits(limits),
		planes: make(map[string]*planeRecord),
	}
}

// RegisterPlane adds one reflecting plane to the table and the atlas.
func (g *PlanarReflectionGuard) RegisterPlane(planeID string, plane PlaneSpec) error {
	if strings.TrimSpace(planeID) == "" {
		return ErrBadPlane
	}
	normal, ok := normalizeNormal(plane.Normal)
	if !ok || plane.Width <= 0 || plane.Height <= 0 || !finiteVec(plane.Point) {
		return ErrBadPlane
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.planes[planeID]; exists {
		return ErrDuplicatePlane
	}
	if len(g.planes) >= g.limits.MaxPlanes {
		return ErrPlaneLimit
	}

	record := &planeRecord{
		id:       planeID,
		point:    plane.Point,
		normal:   normal,
		priority: plane.Priority,
		width:    plane.Width,
		height:   plane.Height,
		everyN:   1,
		enabled:  true,
		seq:      g.nextSeq,
	}

	all := g.planeListLocked()
	all = append(all, record)
	positions, ok := packPlanes(all, g.limits.AtlasWidth, g.limits.AtlasHeight)
	if !ok {
		return ErrAtlasBudget
	}

	for _, p := range all {
		pos := positions[p.id]
		p.atlasX = pos[0]
		p.atlasY = pos[1]
	}
	g.planes[planeID] = record
	g.nextSeq++
	return nil
}

// BuildMirrorCamera reflects the camera about the given plane.
func (g *PlanarReflectionGuard) BuildMirrorCamera(planeID string, camera CameraParams) (MirrorCamera, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	plane, ok := g.planes[planeID]
	if !ok {
		return MirrorCamera{}, ErrUnknownPlane
	}
	if !validCamera(camera) {
		return MirrorCamera{}, ErrBadCamera
	}

	n := plane.normal
	position := reflectPoint(camera.Position, plane.point, n)
	forward := reflectDirection(camera.Forward, n)
	up := reflectDirection(camera.Up, n)
	signedDistance := dot(sub(camera.Position, plane.point), n)

	mirror := MirrorCamera{
		PlaneID:   planeID,
		Position:  position,
		Forward:   forward,
		Up:        up,
		Near:      camera.Near,
		Far:       camera.Far,
		ClipPlane: Vec4{X: n.X, Y: n.Y, Z: n.Z, W: -dot(n, plane.point)},
		Enabled:   true,
	}
	if signedDistance < 0 {
		mirror.Enabled = false
		mirror.Reason = "camera is below the reflecting plane"
		plane.enabled = false
		plane.reason = mirror.Reason
	} else {
		plane.enabled = true
		plane.reason = ""
	}
	return mirror, nil
}

// SetUpdateRate changes how many frames apart one plane may be re-rendered.
func (g *PlanarReflectionGuard) SetUpdateRate(planeID string, everyN int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	plane, ok := g.planes[planeID]
	if !ok {
		return ErrUnknownPlane
	}
	if everyN <= 0 {
		return ErrBadUpdateRate
	}
	plane.everyN = everyN
	return nil
}

// Commit records one reflection render attempt for the current frame.
func (g *PlanarReflectionGuard) Commit(planeID string) (ReflectionState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.frameOpen {
		return ReflectionState{}, ErrNoFrame
	}
	plane, ok := g.planes[planeID]
	if !ok {
		return ReflectionState{}, ErrUnknownPlane
	}

	if plane.hasRendered && plane.lastFrame == g.frame {
		plane.updated = false
		g.skipped++
		return g.stateLocked(plane), nil
	}

	due := plane.invalidated || !plane.hasRendered || g.frame-plane.lastFrame >= plane.everyN
	if !due {
		plane.updated = false
		g.skipped++
		return g.stateLocked(plane), nil
	}

	plane.generation++
	plane.lastFrame = g.frame
	plane.hasRendered = true
	plane.updated = true
	plane.invalidated = false
	g.updates++
	return g.stateLocked(plane), nil
}

// Invalidate forces the next commit of a plane regardless of the update rate.
func (g *PlanarReflectionGuard) Invalidate(planeID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	plane, ok := g.planes[planeID]
	if !ok {
		return ErrUnknownPlane
	}
	plane.invalidated = true
	return nil
}

// State reads the reflection state of one plane.
func (g *PlanarReflectionGuard) State(planeID string) (ReflectionState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	plane, ok := g.planes[planeID]
	if !ok {
		return ReflectionState{}, false
	}
	return g.stateLocked(plane), true
}

// Planes lists the registered plane ids in ascending order.
func (g *PlanarReflectionGuard) Planes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ids := make([]string, 0, len(g.planes))
	for id := range g.planes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SetResolution changes the reflection resolution of one plane.
func (g *PlanarReflectionGuard) SetResolution(planeID string, width, height int) error {
	if width <= 0 || height <= 0 {
		return ErrBadResolution
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	plane, ok := g.planes[planeID]
	if !ok {
		return ErrUnknownPlane
	}

	oldWidth, oldHeight := plane.width, plane.height
	plane.width, plane.height = width, height
	all := g.planeListLocked()
	positions, ok := packPlanes(all, g.limits.AtlasWidth, g.limits.AtlasHeight)
	if !ok {
		plane.width, plane.height = oldWidth, oldHeight
		return ErrAtlasBudget
	}
	for _, p := range all {
		pos := positions[p.id]
		p.atlasX = pos[0]
		p.atlasY = pos[1]
	}
	return nil
}

// BeginFrame opens the frame used by Commits.
func (g *PlanarReflectionGuard) BeginFrame(frame int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if frame <= g.frame {
		return ErrBadFrame
	}
	g.frame = frame
	g.frameOpen = true
	return nil
}

// Frame reports the frame number currently open, or the last closed one.
func (g *PlanarReflectionGuard) Frame() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.frame
}

// Atlas reports the current atlas packing.
func (g *PlanarReflectionGuard) Atlas() AtlasLayout {
	g.mu.Lock()
	defer g.mu.Unlock()

	layout := AtlasLayout{Width: g.limits.AtlasWidth, Height: g.limits.AtlasHeight}
	layout.Entries = make([]AtlasEntry, 0, len(g.planes))
	for _, p := range g.planes {
		layout.Entries = append(layout.Entries, AtlasEntry{
			PlaneID: p.id,
			X:       p.atlasX,
			Y:       p.atlasY,
			W:       p.width,
			H:       p.height,
		})
	}
	sort.Slice(layout.Entries, func(i, j int) bool {
		if layout.Entries[i].Y != layout.Entries[j].Y {
			return layout.Entries[i].Y < layout.Entries[j].Y
		}
		if layout.Entries[i].X != layout.Entries[j].X {
			return layout.Entries[i].X < layout.Entries[j].X
		}
		return layout.Entries[i].PlaneID < layout.Entries[j].PlaneID
	})
	return layout
}

// Stats reports the number of planes and the commit counters.
func (g *PlanarReflectionGuard) Stats() ReflectionStats {
	g.mu.Lock()
	defer g.mu.Unlock()

	return ReflectionStats{Planes: len(g.planes), Updates: g.updates, Skipped: g.skipped}
}

const (
	snapshotVersion    = 1
	snapshotMagic      = "PRGSNP01"
	snapshotHeaderSize = len(snapshotMagic) + 8 + sha256.Size
)

type snapshotPlane struct {
	ID          string `json:"id"`
	Point       Vec3   `json:"point"`
	Normal      Vec3   `json:"normal"`
	Priority    int    `json:"priority"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	EveryN      int    `json:"every_n"`
	Generation  int64  `json:"generation"`
	LastFrame   int    `json:"last_frame"`
	HasRendered bool   `json:"has_rendered"`
	Updated     bool   `json:"updated"`
	Invalidated bool   `json:"invalidated"`
	Enabled     bool   `json:"enabled"`
	Reason      string `json:"reason,omitempty"`
	Seq         int    `json:"seq"`
}

type snapshotData struct {
	Version int             `json:"version"`
	Limits  Limits          `json:"limits"`
	Frame   int             `json:"frame"`
	Updates int64           `json:"updates"`
	Skipped int64           `json:"skipped"`
	Planes  []snapshotPlane `json:"planes"`
}

// Snapshot serialises the plane table, the atlas and the frame counter.
func (g *PlanarReflectionGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	snap := snapshotData{
		Version: snapshotVersion,
		Limits:  g.limits,
		Frame:   g.frame,
		Updates: g.updates,
		Skipped: g.skipped,
		Planes:  make([]snapshotPlane, 0, len(g.planes)),
	}
	ids := make([]string, 0, len(g.planes))
	for id := range g.planes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := g.planes[id]
		snap.Planes = append(snap.Planes, snapshotPlane{
			ID:          p.id,
			Point:       p.point,
			Normal:      p.normal,
			Priority:    p.priority,
			Width:       p.width,
			Height:      p.height,
			EveryN:      p.everyN,
			Generation:  p.generation,
			LastFrame:   p.lastFrame,
			HasRendered: p.hasRendered,
			Updated:     p.updated,
			Invalidated: p.invalidated,
			Enabled:     p.enabled,
			Reason:      p.reason,
			Seq:         p.seq,
		})
	}
	g.mu.Unlock()
	return encodeSnapshot(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PlanarReflectionGuard) Restore(data []byte) error {
	snap, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}

	limits := normalizeLimits(snap.Limits)
	if snap.Version != snapshotVersion || len(snap.Planes) > limits.MaxPlanes || snap.Updates < 0 || snap.Skipped < 0 {
		return ErrBadSnapshot
	}

	planes := make(map[string]*planeRecord, len(snap.Planes))
	records := make([]*planeRecord, 0, len(snap.Planes))
	nextSeq := 0
	for _, sp := range snap.Planes {
		if strings.TrimSpace(sp.ID) == "" || sp.Width <= 0 || sp.Height <= 0 || sp.EveryN <= 0 || sp.Generation < 0 || !finiteVec(sp.Point) {
			return ErrBadSnapshot
		}
		normal, ok := normalizeNormal(sp.Normal)
		if !ok {
			return ErrBadSnapshot
		}
		if _, exists := planes[sp.ID]; exists {
			return ErrBadSnapshot
		}
		record := &planeRecord{
			id:          sp.ID,
			point:       sp.Point,
			normal:      normal,
			priority:    sp.Priority,
			width:       sp.Width,
			height:      sp.Height,
			everyN:      sp.EveryN,
			generation:  sp.Generation,
			lastFrame:   sp.LastFrame,
			hasRendered: sp.HasRendered,
			updated:     sp.Updated,
			invalidated: sp.Invalidated,
			enabled:     sp.Enabled,
			reason:      sp.Reason,
			seq:         sp.Seq,
		}
		planes[sp.ID] = record
		records = append(records, record)
		if sp.Seq >= nextSeq {
			nextSeq = sp.Seq + 1
		}
	}

	positions, ok := packPlanes(records, limits.AtlasWidth, limits.AtlasHeight)
	if !ok {
		return ErrBadSnapshot
	}
	for _, p := range records {
		pos := positions[p.id]
		p.atlasX = pos[0]
		p.atlasY = pos[1]
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = limits
	g.planes = planes
	g.frame = snap.Frame
	g.frameOpen = false
	g.nextSeq = nextSeq
	g.updates = snap.Updates
	g.skipped = snap.Skipped
	return nil
}

func (g *PlanarReflectionGuard) stateLocked(plane *planeRecord) ReflectionState {
	return ReflectionState{
		PlaneID:    plane.id,
		Generation: plane.generation,
		Frame:      plane.lastFrame,
		EveryN:     plane.everyN,
		Updated:    plane.updated,
		AtlasX:     plane.atlasX,
		AtlasY:     plane.atlasY,
		AtlasW:     plane.width,
		AtlasH:     plane.height,
		Enabled:    plane.enabled,
	}
}

func (g *PlanarReflectionGuard) planeListLocked() []*planeRecord {
	planes := make([]*planeRecord, 0, len(g.planes))
	for _, p := range g.planes {
		planes = append(planes, p)
	}
	return planes
}

func normalizeLimits(limits Limits) Limits {
	if limits.MaxPlanes <= 0 {
		limits.MaxPlanes = DefaultMaxPlanes
	}
	if limits.AtlasWidth <= 0 {
		limits.AtlasWidth = DefaultAtlasWidth
	}
	if limits.AtlasHeight <= 0 {
		limits.AtlasHeight = DefaultAtlasHeight
	}
	return limits
}

func normalizeNormal(v Vec3) (Vec3, bool) {
	if !finiteVec(v) {
		return Vec3{}, false
	}
	lengthSquared := v.X*v.X + v.Y*v.Y + v.Z*v.Z
	if lengthSquared == 0 || math.IsNaN(lengthSquared) || math.IsInf(lengthSquared, 0) {
		return Vec3{}, false
	}
	inverseLength := 1 / math.Sqrt(lengthSquared)
	normal := Vec3{X: v.X * inverseLength, Y: v.Y * inverseLength, Z: v.Z * inverseLength}
	if !finiteVec(normal) {
		return Vec3{}, false
	}
	return normal, true
}

func finiteVec(v Vec3) bool {
	return !math.IsNaN(v.X) && !math.IsInf(v.X, 0) &&
		!math.IsNaN(v.Y) && !math.IsInf(v.Y, 0) &&
		!math.IsNaN(v.Z) && !math.IsInf(v.Z, 0)
}

func validCamera(camera CameraParams) bool {
	if !finiteVec(camera.Position) || !finiteVec(camera.Forward) || !finiteVec(camera.Up) {
		return false
	}
	if isZero(camera.Position) || isZero(camera.Forward) || isZero(camera.Up) {
		return false
	}
	if !(camera.FOV > 0) || !(camera.Near > 0) || !(camera.Far > camera.Near) {
		return false
	}
	return true
}

func isZero(v Vec3) bool {
	return v.X == 0 && v.Y == 0 && v.Z == 0
}

func dot(a, b Vec3) float64 {
	return a.X*b.X + a.Y*b.Y + a.Z*b.Z
}

func sub(a, b Vec3) Vec3 {
	return Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}

func reflectPoint(point, planePoint, normal Vec3) Vec3 {
	offset := sub(point, planePoint)
	scale := 2 * dot(offset, normal)
	return Vec3{X: point.X - scale*normal.X, Y: point.Y - scale*normal.Y, Z: point.Z - scale*normal.Z}
}

func reflectDirection(direction, normal Vec3) Vec3 {
	scale := 2 * dot(direction, normal)
	return Vec3{X: direction.X - scale*normal.X, Y: direction.Y - scale*normal.Y, Z: direction.Z - scale*normal.Z}
}

const maxInt64Value = int64(^uint64(0) >> 1)

func areaOf(width, height int) (int64, bool) {
	if width <= 0 || height <= 0 {
		return 0, false
	}
	w, h := int64(width), int64(height)
	if w > maxInt64Value/h {
		return 0, false
	}
	return w * h, true
}

func packPlanes(planes []*planeRecord, atlasWidth, atlasHeight int) (map[string][2]int, bool) {
	atlasArea, ok := areaOf(atlasWidth, atlasHeight)
	if !ok {
		atlasArea = maxInt64Value
	}

	var totalArea int64
	for _, p := range planes {
		if p.width <= 0 || p.height <= 0 || p.width > atlasWidth || p.height > atlasHeight {
			return nil, false
		}
		area, ok := areaOf(p.width, p.height)
		if !ok || area > atlasArea || totalArea > atlasArea-area {
			return nil, false
		}
		totalArea += area
	}

	primary := append([]*planeRecord(nil), planes...)
	sort.Slice(primary, func(i, j int) bool {
		if primary[i].seq != primary[j].seq {
			return primary[i].seq < primary[j].seq
		}
		return primary[i].id < primary[j].id
	})
	if positions, ok := shelfPack(primary, atlasWidth, atlasHeight); ok {
		return positions, true
	}

	byHeight := append([]*planeRecord(nil), planes...)
	sort.Slice(byHeight, func(i, j int) bool {
		if byHeight[i].height != byHeight[j].height {
			return byHeight[i].height > byHeight[j].height
		}
		if byHeight[i].width != byHeight[j].width {
			return byHeight[i].width > byHeight[j].width
		}
		return byHeight[i].id < byHeight[j].id
	})
	if positions, ok := shelfPack(byHeight, atlasWidth, atlasHeight); ok {
		return positions, true
	}

	byArea := append([]*planeRecord(nil), planes...)
	sort.Slice(byArea, func(i, j int) bool {
		ai, _ := areaOf(byArea[i].width, byArea[i].height)
		aj, _ := areaOf(byArea[j].width, byArea[j].height)
		if ai != aj {
			return ai > aj
		}
		return byArea[i].id < byArea[j].id
	})
	return shelfPack(byArea, atlasWidth, atlasHeight)
}

func shelfPack(order []*planeRecord, atlasWidth, atlasHeight int) (map[string][2]int, bool) {
	positions := make(map[string][2]int, len(order))
	x, y, rowHeight := 0, 0, 0
	for _, p := range order {
		if p.width > atlasWidth || p.height > atlasHeight {
			return nil, false
		}
		if x > atlasWidth-p.width {
			y += rowHeight
			x = 0
			rowHeight = 0
		}
		if y > atlasHeight-p.height {
			return nil, false
		}
		positions[p.id] = [2]int{x, y}
		x += p.width
		if p.height > rowHeight {
			rowHeight = p.height
		}
	}
	return positions, true
}

func encodeSnapshot(snap snapshotData) ([]byte, error) {
	payload, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	data := make([]byte, 0, snapshotHeaderSize+len(payload))
	data = append(data, snapshotMagic...)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
	data = append(data, length[:]...)
	data = append(data, digest[:]...)
	data = append(data, payload...)
	return data, nil
}

func decodeSnapshot(data []byte) (snapshotData, error) {
	var snap snapshotData
	if len(data) < snapshotHeaderSize || string(data[:len(snapshotMagic)]) != snapshotMagic {
		return snap, ErrBadSnapshot
	}
	length := binary.BigEndian.Uint64(data[len(snapshotMagic) : len(snapshotMagic)+8])
	if length != uint64(len(data)-snapshotHeaderSize) {
		return snap, ErrBadSnapshot
	}
	digest := data[len(snapshotMagic)+8 : snapshotHeaderSize]
	payload := data[snapshotHeaderSize:]
	actual := sha256.Sum256(payload)
	if !bytes.Equal(digest, actual[:]) {
		return snap, ErrBadSnapshot
	}
	if err := json.Unmarshal(payload, &snap); err != nil {
		return snapshotData{}, ErrBadSnapshot
	}
	return snap, nil
}
