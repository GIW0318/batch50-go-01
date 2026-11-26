// Package task marches shadow rays through a voxelised signed distance field,
// keeps the self shadow bias tied to the voxel size and reports cone shaped
// penumbra for area lights.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Defaults used when the matching limit is left at zero or below.
const (
	DefaultMaxFields = 8
	DefaultMaxLights = 8
	DefaultMaxVoxels = 4096
	DefaultMaxSteps  = 64
	DefaultBaseBias  = 0.001
)

// Errors reported by the raymarcher.
var (
	ErrBadField      = errors.New("field definition is invalid")
	ErrFieldLimit    = errors.New("field table is full")
	ErrVoxelLimit    = errors.New("voxel count exceeds the per field cap")
	ErrBadVoxel      = errors.New("voxel definition is invalid")
	ErrBadLight      = errors.New("light specification is invalid")
	ErrLightLimit    = errors.New("light table is full")
	ErrUnknownField  = errors.New("field is not loaded")
	ErrUnknownLight  = errors.New("light is not registered")
	ErrNoField       = errors.New("no active field")
	ErrNoLight       = errors.New("no active light")
	ErrBadRay        = errors.New("ray parameters are invalid")
	ErrBadStepBudget = errors.New("step budget must be positive")
	ErrBadDistance   = errors.New("bias distance is invalid")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Vec3 is a position or direction in world space.
type Vec3 struct {
	X float64
	Y float64
	Z float64
}

// Voxel is one axis aligned cell of the distance field. Cells whose density is
// at least 0.5 count as occluders.
type Voxel struct {
	Min     Vec3
	Max     Vec3
	Density float64
}

// LightSpec is an area light; Radius 0 means a point light without penumbra.
type LightSpec struct {
	Position  Vec3
	Radius    float64
	Intensity float64
}

// ShadowResult is the outcome of one shadow ray.
type ShadowResult struct {
	Occluded bool
	Certain  bool
	Steps    int
	Depth    float64
	Penumbra float64
}

// Limits bounds the field and light tables and the march itself.
type Limits struct {
	MaxFields int
	MaxLights int
	MaxVoxels int
	MaxSteps  int
	BaseBias  float64
}

// TraceStats counts the marching work done so far.
type TraceStats struct {
	Fields int
	Voxels int
	Traces int64
	Steps  int64
}

// SdfShadowRaymarcher owns the voxel fields, the lights and the step budget.
type SdfShadowRaymarcher struct {
	mu sync.RWMutex

	limits Limits

	fields      map[string][]Voxel
	lights      map[string]LightSpec
	activeField string
	activeLight string
	stepBudget  int

	traces atomic.Int64
	steps  atomic.Int64
}

// NewSdfShadowRaymarcher builds a raymarcher with the given caps.
func NewSdfShadowRaymarcher(limits Limits) *SdfShadowRaymarcher {
	limits = normaliseLimits(limits)
	return &SdfShadowRaymarcher{
		limits:     limits,
		fields:     make(map[string][]Voxel),
		lights:     make(map[string]LightSpec),
		stepBudget: limits.MaxSteps,
	}
}

func normaliseLimits(l Limits) Limits {
	if l.MaxFields <= 0 {
		l.MaxFields = DefaultMaxFields
	}
	if l.MaxLights <= 0 {
		l.MaxLights = DefaultMaxLights
	}
	if l.MaxVoxels <= 0 {
		l.MaxVoxels = DefaultMaxVoxels
	}
	if l.MaxSteps <= 0 {
		l.MaxSteps = DefaultMaxSteps
	}
	if l.BaseBias <= 0 || math.IsNaN(l.BaseBias) || math.IsInf(l.BaseBias, 0) {
		l.BaseBias = DefaultBaseBias
	}
	return l
}

// LoadField stores one voxel field and makes the first loaded field active.
func (m *SdfShadowRaymarcher) LoadField(fieldID string, voxels []Voxel) error {
	if strings.TrimSpace(fieldID) == "" || len(voxels) == 0 {
		return ErrBadField
	}
	for _, voxel := range voxels {
		if !validVoxel(voxel) {
			return ErrBadVoxel
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(voxels) > m.limits.MaxVoxels {
		return ErrVoxelLimit
	}
	if _, exists := m.fields[fieldID]; !exists && len(m.fields) >= m.limits.MaxFields {
		return ErrFieldLimit
	}
	if m.fields == nil {
		m.fields = make(map[string][]Voxel)
	}
	m.fields[fieldID] = append([]Voxel(nil), voxels...)
	if m.activeField == "" {
		m.activeField = fieldID
	}
	return nil
}

// SetLight stores one light and makes the first registered light active.
func (m *SdfShadowRaymarcher) SetLight(lightID string, light LightSpec) error {
	if strings.TrimSpace(lightID) == "" || !validLight(light) {
		return ErrBadLight
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.lights[lightID]; !exists && len(m.lights) >= m.limits.MaxLights {
		return ErrLightLimit
	}
	if m.lights == nil {
		m.lights = make(map[string]LightSpec)
	}
	m.lights[lightID] = light
	if m.activeLight == "" {
		m.activeLight = lightID
	}
	return nil
}

// Trace marches a shadow ray from origin along dir up to maxDist.
func (m *SdfShadowRaymarcher) Trace(origin, dir Vec3, maxDist float64) (ShadowResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeField == "" {
		return ShadowResult{}, ErrNoField
	}
	field, ok := m.fields[m.activeField]
	if !ok {
		return ShadowResult{}, ErrNoField
	}
	if m.activeLight == "" {
		return ShadowResult{}, ErrNoLight
	}
	light, ok := m.lights[m.activeLight]
	if !ok {
		return ShadowResult{}, ErrNoLight
	}

	if !finiteVec(origin) || !finiteVec(dir) {
		return ShadowResult{}, ErrBadRay
	}
	dirLen := math.Sqrt(dir.X*dir.X + dir.Y*dir.Y + dir.Z*dir.Z)
	if dirLen == 0 || math.IsNaN(dirLen) || math.IsInf(dirLen, 0) {
		return ShadowResult{}, ErrBadRay
	}
	if !(maxDist > 0) || math.IsInf(maxDist, 0) {
		return ShadowResult{}, ErrBadRay
	}

	m.traces.Add(1)
	finish := func(result ShadowResult) (ShadowResult, error) {
		m.steps.Add(int64(result.Steps))
		return result, nil
	}
	normal := Vec3{X: dir.X / dirLen, Y: dir.Y / dirLen, Z: dir.Z / dirLen}
	budget := m.stepBudget
	edge := voxelEdge(field)
	surfaceBias := m.limits.BaseBias * edge
	if surfaceBias <= 0 || math.IsNaN(surfaceBias) || math.IsInf(surfaceBias, 0) {
		surfaceBias = 0
	}
	hitRadius := math.Max(surfaceBias, 1e-12)

	startOffset := surfaceBias
	if startOffset > maxDist {
		startOffset = maxDist
	}
	pos := Vec3{
		X: origin.X + normal.X*startOffset,
		Y: origin.Y + normal.Y*startOffset,
		Z: origin.Z + normal.Z*startOffset,
	}
	traveled := startOffset

	steps := 0
	for steps < budget {
		steps++

		dist, inside := nearestOccluder(pos, field)
		if inside || dist <= hitRadius {
			return finish(hitResult(pos, light, traveled, steps))
		}

		remaining := maxDist - traveled
		if remaining <= 0 {
			return finish(ShadowResult{Occluded: false, Certain: true, Steps: steps, Depth: maxDist, Penumbra: 0})
		}

		if dist >= remaining {
			pos = Vec3{
				X: pos.X + normal.X*remaining,
				Y: pos.Y + normal.Y*remaining,
				Z: pos.Z + normal.Z*remaining,
			}
			traveled = maxDist
			dist, inside = nearestOccluder(pos, field)
			if inside || dist <= hitRadius {
				return finish(hitResult(pos, light, traveled, steps))
			}
			return finish(ShadowResult{Occluded: false, Certain: true, Steps: steps, Depth: maxDist, Penumbra: 0})
		}

		// The distance to the nearest occluder is a safe lower bound: stepping
		// by it cannot tunnel through a voxel.
		pos = Vec3{
			X: pos.X + normal.X*dist,
			Y: pos.Y + normal.Y*dist,
			Z: pos.Z + normal.Z*dist,
		}
		traveled += dist
		dist, inside = nearestOccluder(pos, field)
		if inside || dist <= hitRadius {
			return finish(hitResult(pos, light, traveled, steps))
		}
	}

	// The budget ran out before the ray either hit or walked maxDist. This is
	// deliberately uncertain: treating it as a miss would leak light through
	// geometry.
	return finish(ShadowResult{Occluded: false, Certain: false, Steps: steps, Depth: traveled, Penumbra: 0})
}

func hitResult(pos Vec3, light LightSpec, depth float64, steps int) ShadowResult {
	return ShadowResult{
		Occluded: true,
		Certain:  true,
		Steps:    steps,
		Depth:    depth,
		Penumbra: conePenumbra(pos, light, depth),
	}
}

func conePenumbra(hit Vec3, light LightSpec, depth float64) float64 {
	if light.Radius <= 0 || depth <= 0 {
		return 0
	}
	dx := light.Position.X - hit.X
	dy := light.Position.Y - hit.Y
	dz := light.Position.Z - hit.Z
	distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if distance <= 0 {
		return 1
	}
	penumbra := light.Radius * depth / distance
	if math.IsNaN(penumbra) || penumbra < 0 {
		return 0
	}
	if penumbra > 1 {
		return 1
	}
	return penumbra
}

// SetStepBudget changes how many marching steps one ray may take.
func (m *SdfShadowRaymarcher) SetStepBudget(n int) error {
	if n <= 0 {
		return ErrBadStepBudget
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stepBudget = n
	return nil
}

// StepBudget reports the step budget in force.
func (m *SdfShadowRaymarcher) StepBudget() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stepBudget
}

// Bias returns the self shadow offset for a surface distance.
func (m *SdfShadowRaymarcher) Bias(distance float64) (float64, error) {
	if math.IsNaN(distance) || math.IsInf(distance, 0) || distance < 0 {
		return 0, ErrBadDistance
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	edge := voxelEdge(m.fields[m.activeField])
	return m.limits.BaseBias + m.limits.BaseBias*distance*edge, nil
}

// Fields lists the loaded field ids in ascending order.
func (m *SdfShadowRaymarcher) Fields() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.fields))
	for id := range m.fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Lights lists the registered light ids in ascending order.
func (m *SdfShadowRaymarcher) Lights() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.lights))
	for id := range m.lights {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Active reports the active field id and the active light id.
func (m *SdfShadowRaymarcher) Active() (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeField, m.activeLight
}

// SetActiveField switches the field used by Trace.
func (m *SdfShadowRaymarcher) SetActiveField(fieldID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.fields[fieldID]; !ok {
		return ErrUnknownField
	}
	m.activeField = fieldID
	return nil
}

// SetActiveLight switches the light used for the penumbra estimate.
func (m *SdfShadowRaymarcher) SetActiveLight(lightID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.lights[lightID]; !ok {
		return ErrUnknownLight
	}
	m.activeLight = lightID
	return nil
}

// Limits reports the caps in force after defaults have been applied.
func (m *SdfShadowRaymarcher) Limits() Limits {
	return m.limits
}

// Stats reports the field table size and the marching counters.
func (m *SdfShadowRaymarcher) Stats() TraceStats {
	m.mu.RLock()
	voxels := 0
	for _, field := range m.fields {
		voxels += len(field)
	}
	fields := len(m.fields)
	m.mu.RUnlock()
	return TraceStats{
		Fields: fields,
		Voxels: voxels,
		Traces: m.traces.Load(),
		Steps:  m.steps.Load(),
	}
}

const snapshotMagic = "SDFSM001"

type snapshotData struct {
	Version     int             `json:"version"`
	Fields      []snapshotField `json:"fields"`
	Lights      []snapshotLight `json:"lights"`
	ActiveField string          `json:"active_field"`
	ActiveLight string          `json:"active_light"`
	StepBudget  int             `json:"step_budget"`
	Traces      int64           `json:"traces"`
	Steps       int64           `json:"steps"`
}

type snapshotField struct {
	ID     string  `json:"id"`
	Voxels []Voxel `json:"voxels"`
}

type snapshotLight struct {
	ID    string    `json:"id"`
	Light LightSpec `json:"light"`
}

// Snapshot serialises the fields, the lights, the selection and the counters.
func (m *SdfShadowRaymarcher) Snapshot() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snap := snapshotData{
		Version:     1,
		ActiveField: m.activeField,
		ActiveLight: m.activeLight,
		StepBudget:  m.stepBudget,
		Traces:      m.traces.Load(),
		Steps:       m.steps.Load(),
	}
	fieldIDs := make([]string, 0, len(m.fields))
	for id := range m.fields {
		fieldIDs = append(fieldIDs, id)
	}
	sort.Strings(fieldIDs)
	for _, id := range fieldIDs {
		snap.Fields = append(snap.Fields, snapshotField{
			ID:     id,
			Voxels: append([]Voxel(nil), m.fields[id]...),
		})
	}
	lightIDs := make([]string, 0, len(m.lights))
	for id := range m.lights {
		lightIDs = append(lightIDs, id)
	}
	sort.Strings(lightIDs)
	for _, id := range lightIDs {
		snap.Lights = append(snap.Lights, snapshotLight{ID: id, Light: m.lights[id]})
	}

	payload, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(snapshotMagic)+8+len(payload)+sha256.Size)
	out = append(out, snapshotMagic...)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
	out = append(out, length[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	out = append(out, sum[:]...)
	return out, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (m *SdfShadowRaymarcher) Restore(data []byte) error {
	state, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.fields = state.fields
	m.lights = state.lights
	m.activeField = state.activeField
	m.activeLight = state.activeLight
	m.stepBudget = state.stepBudget
	m.traces.Store(state.traces)
	m.steps.Store(state.steps)
	return nil
}

type restoredState struct {
	fields      map[string][]Voxel
	lights      map[string]LightSpec
	activeField string
	activeLight string
	stepBudget  int
	traces      int64
	steps       int64
}

func decodeSnapshot(data []byte) (restoredState, error) {
	var state restoredState
	header := len(snapshotMagic) + 8
	if len(data) < header+sha256.Size {
		return state, ErrBadSnapshot
	}
	if string(data[:len(snapshotMagic)]) != snapshotMagic {
		return state, ErrBadSnapshot
	}
	payloadLen := binary.BigEndian.Uint64(data[len(snapshotMagic):header])
	if payloadLen > uint64(len(data)-header-sha256.Size) ||
		int(payloadLen) != len(data)-header-sha256.Size {
		return state, ErrBadSnapshot
	}
	payload := data[header : header+int(payloadLen)]
	checksum := data[header+int(payloadLen):]
	sum := sha256.Sum256(payload)
	if !bytes.Equal(checksum, sum[:]) {
		return state, ErrBadSnapshot
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var snap snapshotData
	if err := dec.Decode(&snap); err != nil {
		return state, ErrBadSnapshot
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return state, ErrBadSnapshot
	}
	if snap.Version != 1 || snap.StepBudget <= 0 || snap.Traces < 0 || snap.Steps < 0 {
		return state, ErrBadSnapshot
	}

	fields := make(map[string][]Voxel, len(snap.Fields))
	for _, field := range snap.Fields {
		if strings.TrimSpace(field.ID) == "" || len(field.Voxels) == 0 {
			return state, ErrBadSnapshot
		}
		if _, exists := fields[field.ID]; exists {
			return state, ErrBadSnapshot
		}
		for _, voxel := range field.Voxels {
			if !validVoxel(voxel) {
				return state, ErrBadSnapshot
			}
		}
		fields[field.ID] = append([]Voxel(nil), field.Voxels...)
	}
	if len(fields) == 0 {
		if snap.ActiveField != "" {
			return state, ErrBadSnapshot
		}
	} else if _, ok := fields[snap.ActiveField]; !ok {
		return state, ErrBadSnapshot
	}

	lights := make(map[string]LightSpec, len(snap.Lights))
	for _, light := range snap.Lights {
		if strings.TrimSpace(light.ID) == "" || !validLight(light.Light) {
			return state, ErrBadSnapshot
		}
		if _, exists := lights[light.ID]; exists {
			return state, ErrBadSnapshot
		}
		lights[light.ID] = light.Light
	}
	if len(lights) == 0 {
		if snap.ActiveLight != "" {
			return state, ErrBadSnapshot
		}
	} else if _, ok := lights[snap.ActiveLight]; !ok {
		return state, ErrBadSnapshot
	}

	state = restoredState{
		fields:      fields,
		lights:      lights,
		activeField: snap.ActiveField,
		activeLight: snap.ActiveLight,
		stepBudget:  snap.StepBudget,
		traces:      snap.Traces,
		steps:       snap.Steps,
	}
	return state, nil
}

func validVoxel(voxel Voxel) bool {
	if !finiteVec(voxel.Min) || !finiteVec(voxel.Max) {
		return false
	}
	if math.IsNaN(voxel.Density) || math.IsInf(voxel.Density, 0) ||
		voxel.Density < 0 || voxel.Density > 1 {
		return false
	}
	if voxel.Min.X > voxel.Max.X || voxel.Min.Y > voxel.Max.Y || voxel.Min.Z > voxel.Max.Z {
		return false
	}
	return true
}

func validLight(light LightSpec) bool {
	if !finiteVec(light.Position) {
		return false
	}
	if math.IsNaN(light.Radius) || math.IsInf(light.Radius, 0) || light.Radius < 0 {
		return false
	}
	if math.IsNaN(light.Intensity) || math.IsInf(light.Intensity, 0) || light.Intensity <= 0 {
		return false
	}
	return true
}

func finiteVec(v Vec3) bool {
	return !math.IsNaN(v.X) && !math.IsInf(v.X, 0) &&
		!math.IsNaN(v.Y) && !math.IsInf(v.Y, 0) &&
		!math.IsNaN(v.Z) && !math.IsInf(v.Z, 0)
}

// voxelEdge returns a representative cell edge length. It is recomputed from
// the active field on every use so a rebake at another resolution can never
// inherit a stale bias.
func voxelEdge(voxels []Voxel) float64 {
	total := 0.0
	count := 0
	for _, voxel := range voxels {
		edges := [...]float64{
			voxel.Max.X - voxel.Min.X,
			voxel.Max.Y - voxel.Min.Y,
			voxel.Max.Z - voxel.Min.Z,
		}
		for _, edge := range edges {
			if edge > 0 && !math.IsNaN(edge) && !math.IsInf(edge, 0) {
				total += edge
				count++
			}
		}
	}
	if count == 0 {
		return 1
	}
	return total / float64(count)
}

func occluder(voxel Voxel) bool {
	return voxel.Density >= 0.5
}

// nearestOccluder returns the Euclidean distance to the nearest occluding
// voxel surface. A point on or inside an occluder has distance zero.
func nearestOccluder(point Vec3, voxels []Voxel) (float64, bool) {
	best := math.Inf(1)
	for _, voxel := range voxels {
		if !occluder(voxel) {
			continue
		}
		distance, inside := distanceToVoxel(point, voxel)
		if inside {
			return 0, true
		}
		if distance < best {
			best = distance
		}
	}
	return best, false
}

func distanceToVoxel(point Vec3, voxel Voxel) (float64, bool) {
	dx := axisDistance(point.X, voxel.Min.X, voxel.Max.X)
	dy := axisDistance(point.Y, voxel.Min.Y, voxel.Max.Y)
	dz := axisDistance(point.Z, voxel.Min.Z, voxel.Max.Z)
	if dx == 0 && dy == 0 && dz == 0 {
		return 0, true
	}
	return math.Sqrt(dx*dx + dy*dy + dz*dz), false
}

func axisDistance(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum - value
	}
	if value > maximum {
		return value - maximum
	}
	return 0
}
