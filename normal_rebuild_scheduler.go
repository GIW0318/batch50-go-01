// Package task schedules normal rebuilds for skinned and deformed meshes:
// smooth group accumulation, seam aware splitting and per frame budgets.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Vec2 is a two component texture coordinate.
type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Vec3 is a three component vector.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Tri indexes three vertices of a mesh.
type Tri struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

// RebuildMesh is the geometry the scheduler rebuilds normals for.
type RebuildMesh struct {
	ID    string `json:"id"`
	Verts []Vec3 `json:"verts"`
	UVs   []Vec2 `json:"uvs"`
	Tris  []Tri  `json:"tris"`
}

// SeamPolicy selects how duplicated seam vertices are treated.
type SeamPolicy int

// Supported seam policies.
const (
	SeamSplit SeamPolicy = iota
	SeamMerge
)

// NormalStats is the accounting view of the scheduler.
type NormalStats struct {
	Meshes      int     `json:"meshes"`
	Dirty       int     `json:"dirty"`
	Rebuilt     int     `json:"rebuilt"`
	Deferred    int     `json:"deferred"`
	SmoothAngle float64 `json:"smooth_angle"`
}

// Errors returned by the scheduler.
var (
	ErrBadName     = errors.New("normal scheduler: mesh name is required")
	ErrMeshExists  = errors.New("normal scheduler: mesh already registered")
	ErrBadMesh     = errors.New("normal scheduler: mesh has no triangles")
	ErrUnknownMesh = errors.New("normal scheduler: unknown mesh")
	ErrBadVertex   = errors.New("normal scheduler: vertex index out of range")
	ErrBadBudget   = errors.New("normal scheduler: budget must be positive")
	ErrBadAngle    = errors.New("normal scheduler: smooth angle must be within 0 and 180 degrees")
	ErrBadSeam     = errors.New("normal scheduler: unsupported seam policy")
	ErrBadSnapshot = errors.New("normal scheduler: snapshot payload is invalid")
)

const snapshotVersion = 1

// NormalRebuildScheduler owns the registered meshes, the dirty sets, the
// smooth angle and the seam policy.
type NormalRebuildScheduler struct {
	mu           sync.RWMutex
	smoothAngle  float64
	seamPolicy   SeamPolicy
	meshes       map[string]*rebuildMeshState
	totalRebuilt int
}

type rebuildMeshState struct {
	mesh      RebuildMesh
	normals   []Vec3
	computed  []bool
	dirty     map[int]struct{}
	queue     []int
	queued    map[int]struct{}
	deferred  int
	incident  [][]int
	adjacency [][]int
	peers     [][]int
}

type snapshotData struct {
	Version      int            `json:"version"`
	SmoothAngle  float64        `json:"smooth_angle"`
	SeamPolicy   SeamPolicy     `json:"seam_policy"`
	TotalRebuilt int            `json:"total_rebuilt"`
	Meshes       []snapshotMesh `json:"meshes"`
}

type snapshotMesh struct {
	Name     string      `json:"name"`
	Mesh     RebuildMesh `json:"mesh"`
	Normals  []Vec3      `json:"normals"`
	Computed []bool      `json:"computed"`
	Dirty    []int       `json:"dirty"`
	Queue    []int       `json:"queue"`
	Deferred int         `json:"deferred"`
}

// NewNormalRebuildScheduler builds a scheduler with a 60 degree smooth angle
// and seam splitting enabled.
func NewNormalRebuildScheduler() *NormalRebuildScheduler {
	return &NormalRebuildScheduler{
		smoothAngle: 60,
		seamPolicy:  SeamSplit,
		meshes:      make(map[string]*rebuildMeshState),
	}
}

// RegisterMesh stores one mesh so its normals can be rebuilt later.
func (s *NormalRebuildScheduler) RegisterMesh(name string, mesh RebuildMesh) error {
	if strings.TrimSpace(name) == "" {
		return ErrBadName
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.meshes == nil {
		s.meshes = make(map[string]*rebuildMeshState)
	}
	if _, exists := s.meshes[name]; exists {
		return ErrMeshExists
	}
	if err := validateMesh(mesh); err != nil {
		return err
	}

	s.meshes[name] = newMeshState(mesh)
	return nil
}

// MarkDirty adds vertices to the dirty set of one mesh.
func (s *NormalRebuildScheduler) MarkDirty(mesh string, verts []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.meshStateLocked(mesh)
	if err != nil {
		return err
	}

	vertexCount := len(state.mesh.Verts)
	for _, vertex := range verts {
		if vertex < 0 || vertex >= vertexCount {
			return ErrBadVertex
		}
	}

	requested := make(map[int]struct{}, len(verts))
	for _, vertex := range verts {
		requested[vertex] = struct{}{}
	}
	for _, vertex := range sortedSetKeys(requested) {
		state.dirty[vertex] = struct{}{}
		state.enqueueLocked(vertex)
	}
	state.enqueueRingLocked(requested, s.seamPolicy)
	return nil
}

// Rebuild recomputes at most budget vertices of one mesh and reports how many
// vertices were actually rebuilt.
func (s *NormalRebuildScheduler) Rebuild(mesh string, budget int) (int, error) {
	if budget <= 0 {
		return 0, ErrBadBudget
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.meshStateLocked(mesh)
	if err != nil {
		return 0, err
	}

	for _, vertex := range sortedSetKeys(state.dirty) {
		state.enqueueLocked(vertex)
	}

	rebuilt := 0
	for rebuilt < budget && len(state.queue) > 0 {
		vertex := state.queue[0]
		state.queue = state.queue[1:]
		delete(state.queued, vertex)

		state.normals[vertex] = state.calculateNormalLocked(vertex, s.smoothAngle, s.seamPolicy)
		state.computed[vertex] = true
		delete(state.dirty, vertex)

		rebuilt++
	}

	state.deferred = len(state.queue)
	s.totalRebuilt += rebuilt
	return rebuilt, nil
}

// Normals returns a copy of the current vertex normals of one mesh.
func (s *NormalRebuildScheduler) Normals(mesh string) ([]Vec3, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state, err := s.meshStateLocked(mesh)
	if err != nil {
		return nil, err
	}
	return append([]Vec3(nil), state.normals...), nil
}

// SetSmoothAngle sets the angle above which an edge becomes hard.
func (s *NormalRebuildScheduler) SetSmoothAngle(deg float64) error {
	if math.IsNaN(deg) || deg < 0 || deg > 180 {
		return ErrBadAngle
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.smoothAngle != deg {
		s.smoothAngle = deg
		s.invalidateComputedLocked()
	}
	return nil
}

// SmoothAngle reports the configured smooth angle.
func (s *NormalRebuildScheduler) SmoothAngle() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.smoothAngle
}

// SetSeamPolicy selects how seam duplicates are handled.
func (s *NormalRebuildScheduler) SetSeamPolicy(p SeamPolicy) error {
	if p != SeamSplit && p != SeamMerge {
		return ErrBadSeam
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.seamPolicy != p {
		s.seamPolicy = p
		s.invalidateComputedLocked()
	}
	return nil
}

// DirtyCount reports how many vertices still wait for a rebuild.
func (s *NormalRebuildScheduler) DirtyCount(mesh string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state, err := s.meshStateLocked(mesh)
	if err != nil {
		return 0
	}
	return len(state.dirty)
}

// Stats reports the accounting of the scheduler.
func (s *NormalRebuildScheduler) Stats() NormalStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := NormalStats{
		Meshes:      len(s.meshes),
		Rebuilt:     s.totalRebuilt,
		SmoothAngle: s.smoothAngle,
	}
	for _, state := range s.meshes {
		stats.Dirty += len(state.dirty)
		stats.Deferred += state.deferred
	}
	return stats
}

// Snapshot serialises meshes, dirty sets, normals and settings.
func (s *NormalRebuildScheduler) Snapshot() ([]byte, error) {
	s.mu.RLock()
	data := snapshotData{
		Version:      snapshotVersion,
		SmoothAngle:  s.smoothAngle,
		SeamPolicy:   s.seamPolicy,
		TotalRebuilt: s.totalRebuilt,
		Meshes:       make([]snapshotMesh, 0, len(s.meshes)),
	}

	names := make([]string, 0, len(s.meshes))
	for name := range s.meshes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		state := s.meshes[name]
		dirty := make([]int, 0, len(state.dirty))
		for vertex := range state.dirty {
			dirty = append(dirty, vertex)
		}
		sort.Ints(dirty)

		data.Meshes = append(data.Meshes, snapshotMesh{
			Name:     name,
			Mesh:     cloneMesh(state.mesh),
			Normals:  append([]Vec3(nil), state.normals...),
			Computed: append([]bool(nil), state.computed...),
			Dirty:    dirty,
			Queue:    append([]int(nil), state.queue...),
			Deferred: state.deferred,
		})
	}
	s.mu.RUnlock()

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *NormalRebuildScheduler) Restore(data []byte) error {
	restored, err := snapshotFromBytes(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.smoothAngle = restored.smoothAngle
	s.seamPolicy = restored.seamPolicy
	s.totalRebuilt = restored.totalRebuilt
	s.meshes = restored.meshes
	return nil
}

func (s *NormalRebuildScheduler) meshStateLocked(name string) (*rebuildMeshState, error) {
	if s == nil || s.meshes == nil {
		return nil, ErrUnknownMesh
	}
	state, ok := s.meshes[name]
	if !ok || state == nil {
		return nil, ErrUnknownMesh
	}
	return state, nil
}

func (s *NormalRebuildScheduler) invalidateComputedLocked() {
	for _, state := range s.meshes {
		invalidated := make(map[int]struct{})
		for vertex, wasComputed := range state.computed {
			if wasComputed {
				state.dirty[vertex] = struct{}{}
				state.enqueueLocked(vertex)
				invalidated[vertex] = struct{}{}
			}
		}
		state.enqueueRingLocked(invalidated, s.seamPolicy)
	}
}

func validateMesh(mesh RebuildMesh) error {
	if len(mesh.Tris) == 0 {
		return ErrBadMesh
	}

	vertexCount := len(mesh.Verts)
	for _, tri := range mesh.Tris {
		if !validVertexIndex(tri.A, vertexCount) ||
			!validVertexIndex(tri.B, vertexCount) ||
			!validVertexIndex(tri.C, vertexCount) {
			return ErrBadMesh
		}
	}
	return nil
}

func validVertexIndex(vertex, count int) bool {
	return vertex >= 0 && vertex < count
}

func newMeshState(mesh RebuildMesh) *rebuildMeshState {
	vertexCount := len(mesh.Verts)
	state := &rebuildMeshState{
		mesh:      mesh,
		normals:   make([]Vec3, vertexCount),
		computed:  make([]bool, vertexCount),
		dirty:     make(map[int]struct{}),
		queued:    make(map[int]struct{}),
		incident:  make([][]int, vertexCount),
		adjacency: make([][]int, vertexCount),
		peers:     make([][]int, vertexCount),
	}

	adjacentSets := make([]map[int]struct{}, vertexCount)
	for vertex := 0; vertex < vertexCount; vertex++ {
		adjacentSets[vertex] = make(map[int]struct{})
	}

	for triIndex, tri := range mesh.Tris {
		for _, vertex := range []int{tri.A, tri.B, tri.C} {
			state.incident[vertex] = append(state.incident[vertex], triIndex)
		}
		addAdjacency(adjacentSets, tri.A, tri.B)
		addAdjacency(adjacentSets, tri.A, tri.C)
		addAdjacency(adjacentSets, tri.B, tri.C)
	}

	for vertex := 0; vertex < vertexCount; vertex++ {
		state.adjacency[vertex] = sortedSetKeys(adjacentSets[vertex])
	}

	positionGroups := make(map[Vec3][]int, vertexCount)
	for vertex, position := range mesh.Verts {
		positionGroups[position] = append(positionGroups[position], vertex)
	}
	for _, group := range positionGroups {
		sort.Ints(group)
		for _, vertex := range group {
			state.peers[vertex] = append([]int(nil), group...)
		}
	}

	return state
}

func addAdjacency(sets []map[int]struct{}, from, to int) {
	if from == to {
		return
	}
	sets[from][to] = struct{}{}
	sets[to][from] = struct{}{}
}

func sortedSetKeys(set map[int]struct{}) []int {
	values := make([]int, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Ints(values)
	return values
}

func (state *rebuildMeshState) enqueueLocked(vertex int) {
	if _, queued := state.queued[vertex]; queued {
		return
	}
	state.queued[vertex] = struct{}{}
	state.queue = append(state.queue, vertex)
}

func (state *rebuildMeshState) enqueueRingLocked(roots map[int]struct{}, policy SeamPolicy) {
	ring := make(map[int]struct{})
	for root := range roots {
		for _, neighbor := range state.adjacency[root] {
			ring[neighbor] = struct{}{}
		}
		if policy == SeamMerge {
			for _, peer := range state.peers[root] {
				ring[peer] = struct{}{}
			}
		}
	}

	for _, vertex := range sortedSetKeys(ring) {
		state.enqueueLocked(vertex)
	}
}

func (state *rebuildMeshState) calculateNormalLocked(vertex int, smoothAngle float64, policy SeamPolicy) Vec3 {
	triangles := state.incidentTrianglesLocked(vertex, policy)
	if len(triangles) == 0 {
		return Vec3{}
	}

	var baseNormal Vec3
	haveBase := false
	var weighted Vec3

	for _, triIndex := range triangles {
		cross := triangleCross(state.mesh, triIndex)
		length := vecLength(cross)
		if length == 0 {
			continue
		}
		normal := vecScale(cross, 1/length)

		if !haveBase {
			baseNormal = normal
			haveBase = true
			weighted = vecAdd(weighted, cross)
			continue
		}

		if angleWithin(baseNormal, normal, smoothAngle) {
			weighted = vecAdd(weighted, cross)
		}
	}

	length := vecLength(weighted)
	if length == 0 {
		return Vec3{}
	}
	return vecScale(weighted, 1/length)
}

func (state *rebuildMeshState) incidentTrianglesLocked(vertex int, policy SeamPolicy) []int {
	if policy != SeamMerge {
		return append([]int(nil), state.incident[vertex]...)
	}

	vertices := state.peers[vertex]
	if len(vertices) == 0 {
		vertices = []int{vertex}
	}

	seen := make(map[int]struct{})
	triangles := make([]int, 0)
	for _, peer := range vertices {
		for _, triIndex := range state.incident[peer] {
			if _, ok := seen[triIndex]; ok {
				continue
			}
			seen[triIndex] = struct{}{}
			triangles = append(triangles, triIndex)
		}
	}
	sort.Ints(triangles)
	return triangles
}

func triangleCross(mesh RebuildMesh, triIndex int) Vec3 {
	tri := mesh.Tris[triIndex]
	a := mesh.Verts[tri.A]
	b := mesh.Verts[tri.B]
	c := mesh.Verts[tri.C]
	ab := vecSub(b, a)
	ac := vecSub(c, a)
	return vecCross(ab, ac)
}

func angleWithin(a, b Vec3, maxDegrees float64) bool {
	if maxDegrees >= 180 {
		return true
	}

	dot := vecDot(a, b)
	if dot > 1 {
		dot = 1
	} else if dot < -1 {
		dot = -1
	}
	angle := math.Acos(dot) * 180 / math.Pi
	return angle <= maxDegrees+1e-9
}

func vecAdd(a, b Vec3) Vec3 {
	return Vec3{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z}
}

func vecSub(a, b Vec3) Vec3 {
	return Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}

func vecScale(a Vec3, scale float64) Vec3 {
	return Vec3{X: a.X * scale, Y: a.Y * scale, Z: a.Z * scale}
}

func vecDot(a, b Vec3) float64 {
	return a.X*b.X + a.Y*b.Y + a.Z*b.Z
}

func vecLength(a Vec3) float64 {
	return math.Sqrt(vecDot(a, a))
}

func vecCross(a, b Vec3) Vec3 {
	return Vec3{
		X: a.Y*b.Z - a.Z*b.Y,
		Y: a.Z*b.X - a.X*b.Z,
		Z: a.X*b.Y - a.Y*b.X,
	}
}

func cloneMesh(mesh RebuildMesh) RebuildMesh {
	return RebuildMesh{
		ID:    mesh.ID,
		Verts: append([]Vec3(nil), mesh.Verts...),
		UVs:   append([]Vec2(nil), mesh.UVs...),
		Tris:  append([]Tri(nil), mesh.Tris...),
	}
}

func snapshotFromBytes(data []byte) (*NormalRebuildScheduler, error) {
	if len(data) == 0 {
		return nil, ErrBadSnapshot
	}

	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if payload.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if math.IsNaN(payload.SmoothAngle) || payload.SmoothAngle < 0 || payload.SmoothAngle > 180 {
		return nil, fmt.Errorf("%w: bad smooth angle", ErrBadSnapshot)
	}
	if payload.SeamPolicy != SeamSplit && payload.SeamPolicy != SeamMerge {
		return nil, fmt.Errorf("%w: bad seam policy", ErrBadSnapshot)
	}
	if payload.TotalRebuilt < 0 {
		return nil, fmt.Errorf("%w: negative rebuilt count", ErrBadSnapshot)
	}

	scheduler := NewNormalRebuildScheduler()
	scheduler.smoothAngle = payload.SmoothAngle
	scheduler.seamPolicy = payload.SeamPolicy
	scheduler.totalRebuilt = payload.TotalRebuilt

	names := make(map[string]struct{}, len(payload.Meshes))
	for _, item := range payload.Meshes {
		if strings.TrimSpace(item.Name) == "" {
			return nil, fmt.Errorf("%w: empty mesh name", ErrBadSnapshot)
		}
		if _, exists := names[item.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate mesh", ErrBadSnapshot)
		}
		names[item.Name] = struct{}{}

		if err := validateMesh(item.Mesh); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}

		vertexCount := len(item.Mesh.Verts)
		if len(item.Normals) != vertexCount || len(item.Computed) != vertexCount {
			return nil, fmt.Errorf("%w: normal state length mismatch", ErrBadSnapshot)
		}
		if item.Deferred < 0 {
			return nil, fmt.Errorf("%w: negative deferred count", ErrBadSnapshot)
		}

		state := newMeshState(item.Mesh)
		state.normals = append([]Vec3(nil), item.Normals...)
		state.computed = append([]bool(nil), item.Computed...)
		state.deferred = item.Deferred

		for _, vertex := range item.Dirty {
			if !validVertexIndex(vertex, vertexCount) {
				return nil, fmt.Errorf("%w: bad dirty vertex", ErrBadSnapshot)
			}
			state.dirty[vertex] = struct{}{}
		}

		seenQueue := make(map[int]struct{})
		for _, vertex := range item.Queue {
			if !validVertexIndex(vertex, vertexCount) {
				return nil, fmt.Errorf("%w: bad queued vertex", ErrBadSnapshot)
			}
			if _, ok := seenQueue[vertex]; ok {
				continue
			}
			seenQueue[vertex] = struct{}{}
			state.enqueueLocked(vertex)
		}
		for vertex := range state.dirty {
			state.enqueueLocked(vertex)
		}
		if state.deferred > len(state.queue) {
			return nil, fmt.Errorf("%w: deferred count mismatch", ErrBadSnapshot)
		}

		scheduler.meshes[item.Name] = state
	}

	return scheduler, nil
}
