// Package task drives runtime material property animation: curve sampling,
// shared to instance splitting and per material time bookkeeping.
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
	"strings"
	"sync"
	"time"
)

// CurveID identifies one authored animation curve.
type CurveID string

// BindingID identifies one material property binding.
type BindingID string

// InstanceID identifies one materialized material instance.
type InstanceID string

// LoopMode selects what happens after the last key frame is reached.
type LoopMode int

// Supported loop modes.
const (
	LoopOnce LoopMode = iota
	LoopRepeat
	LoopPingPong
)

// Animated property names. Only these four names are accepted.
const (
	PropEmissive  = "emissive"
	PropOpacity   = "opacity"
	PropUVOffsetU = "uv_offset_u"
	PropUVOffsetV = "uv_offset_v"
)

// Key is one curve control point. At is relative to the material phase.
type Key struct {
	At    time.Duration `json:"at"`
	Value float64       `json:"value"`
}

// AnimStats is the accounting view of the animator.
type AnimStats struct {
	Materials int   `json:"materials"`
	Curves    int   `json:"curves"`
	Keys      int   `json:"keys"`
	Bindings  int   `json:"bindings"`
	Instances int   `json:"instances"`
	Frames    int64 `json:"frames"`
}

// Errors returned by the animator.
var (
	ErrBadMaterial       = errors.New("material animator: material name is required")
	ErrDuplicateMaterial = errors.New("material animator: material already registered")
	ErrUnknownMaterial   = errors.New("material animator: unknown material")
	ErrUnknownProperty   = errors.New("material animator: unknown animated property")
	ErrUnknownCurve      = errors.New("material animator: unknown curve")
	ErrUnknownBinding    = errors.New("material animator: unknown binding")
	ErrBadKeys           = errors.New("material animator: key frames must be non negative and ordered")
	ErrCurveLimit        = errors.New("material animator: curve budget exhausted")
	ErrKeyLimit          = errors.New("material animator: key frame budget exhausted")
	ErrInstanceLimit     = errors.New("material animator: instance budget exhausted")
	ErrBadDelta          = errors.New("material animator: delta must not be negative")
	ErrBadLoopMode       = errors.New("material animator: unsupported loop mode")
	ErrBadSnapshot       = errors.New("material animator: snapshot payload is invalid")
)

const snapshotVersion = 1

// MaterialAnimator owns the curve table, the material table, the bindings,
// the instance ledger and the clock used for sampling.
type MaterialAnimator struct {
	mu sync.RWMutex

	maxCurves    int
	maxKeys      int
	maxInstances int

	frames         int64
	nextBindingID  int64
	nextInstanceID int64

	materials map[string]*materialState
	curves    map[CurveID][]Key
	bindings  map[BindingID]*bindingState
	instances map[InstanceID]*instanceState
}

type materialState struct {
	base     map[string]float64
	phase    time.Duration
	loop     LoopMode
	paused   bool
	order    []BindingID
	props    map[string]BindingID
	instance InstanceID
}

type bindingState struct {
	id       BindingID
	material string
	property string
	curve    CurveID
}

type instanceState struct {
	id          InstanceID
	material    string
	retired     bool
	retireFrame int64
}

// NewMaterialAnimator builds an animator with hard caps on curves, key frames
// per curve and material instances.
func NewMaterialAnimator(maxCurves, maxKeys, maxInstances int) *MaterialAnimator {
	if maxCurves < 0 {
		maxCurves = 0
	}
	if maxKeys < 0 {
		maxKeys = 0
	}
	if maxInstances < 0 {
		maxInstances = 0
	}

	return &MaterialAnimator{
		maxCurves:    maxCurves,
		maxKeys:      maxKeys,
		maxInstances: maxInstances,
		materials:    make(map[string]*materialState),
		curves:       make(map[CurveID][]Key),
		bindings:     make(map[BindingID]*bindingState),
		instances:    make(map[InstanceID]*instanceState),
	}
}

// AddMaterial registers a shared material with its base property values.
func (a *MaterialAnimator) AddMaterial(name string, base map[string]float64) error {
	if blankMaterialName(name) {
		return ErrBadMaterial
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.materials[name]; exists {
		return ErrDuplicateMaterial
	}

	copiedBase := make(map[string]float64, len(base))
	for prop, value := range base {
		if !supportedProperty(prop) {
			return ErrUnknownProperty
		}
		copiedBase[prop] = value
	}

	a.materials[name] = &materialState{
		base:  copiedBase,
		loop:  LoopRepeat,
		props: make(map[string]BindingID),
	}
	return nil
}

// SetCurve stores or replaces one animation curve.
func (a *MaterialAnimator) SetCurve(id CurveID, keys []Key) error {
	copiedKeys := append([]Key(nil), keys...)
	if err := validateKeys(copiedKeys, a.maxKeys); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.curves[id]; !exists && len(a.curves) >= a.maxCurves {
		return ErrCurveLimit
	}
	a.curves[id] = copiedKeys
	return nil
}

// Bind attaches a curve to one property of one material.
func (a *MaterialAnimator) Bind(material, prop string, curve CurveID) (BindingID, error) {
	if blankMaterialName(material) {
		return "", ErrBadMaterial
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	m, ok := a.materials[material]
	if !ok {
		return "", ErrUnknownMaterial
	}
	if !supportedProperty(prop) {
		return "", ErrUnknownProperty
	}
	if _, ok := a.curves[curve]; !ok {
		return "", ErrUnknownCurve
	}

	if id, ok := m.props[prop]; ok {
		if binding := a.bindings[id]; binding != nil {
			binding.curve = curve
		}
		a.reviveInstanceLocked(m)
		return id, nil
	}

	a.nextBindingID++
	id := BindingID(fmt.Sprintf("binding-%d", a.nextBindingID))
	binding := &bindingState{
		id:       id,
		material: material,
		property: prop,
		curve:    curve,
	}
	a.bindings[id] = binding
	m.props[prop] = id
	m.order = append(m.order, id)
	a.reviveInstanceLocked(m)
	return id, nil
}

// Unbind removes one binding and lets the property fall back to the base value.
func (a *MaterialAnimator) Unbind(id BindingID) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	binding, ok := a.bindings[id]
	if !ok {
		return ErrUnknownBinding
	}

	m, ok := a.materials[binding.material]
	if !ok {
		delete(a.bindings, id)
		return ErrUnknownBinding
	}

	if current := m.props[binding.property]; current == id {
		delete(m.props, binding.property)
	}
	m.order = removeBindingID(m.order, id)
	delete(a.bindings, id)

	if len(m.props) == 0 && m.instance != "" {
		if instance := a.instances[m.instance]; instance != nil {
			instance.retired = true
			instance.retireFrame = a.frames
		}
	}
	return nil
}

// Advance moves every unpaused material phase by dt and bumps the frame counter.
func (a *MaterialAnimator) Advance(dt time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if dt < 0 {
		return ErrBadDelta
	}

	a.reapRetiredInstancesLocked()
	for _, m := range a.materials {
		if !m.paused {
			m.phase += dt
		}
	}
	a.frames++
	return nil
}

// Value samples one property of one material at the current phase.
func (a *MaterialAnimator) Value(material, prop string) (float64, error) {
	if blankMaterialName(material) {
		return 0, ErrBadMaterial
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	m, ok := a.materials[material]
	if !ok {
		return 0, ErrUnknownMaterial
	}
	if !supportedProperty(prop) {
		return 0, ErrUnknownProperty
	}

	id, bound := m.props[prop]
	if !bound {
		return m.base[prop], nil
	}
	binding, ok := a.bindings[id]
	if !ok {
		return m.base[prop], nil
	}
	keys, ok := a.curves[binding.curve]
	if !ok {
		return m.base[prop], nil
	}
	return sampleCurve(keys, m.phase, m.loop), nil
}

// Materialize splits a material that carries animated properties into its own
// instance and returns the instance id.
func (a *MaterialAnimator) Materialize(material string) (InstanceID, error) {
	if blankMaterialName(material) {
		return "", ErrBadMaterial
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	m, ok := a.materials[material]
	if !ok {
		return "", ErrUnknownMaterial
	}

	if m.instance != "" {
		if instance, ok := a.instances[m.instance]; ok {
			instance.retired = false
			instance.retireFrame = 0
			return m.instance, nil
		}
		m.instance = ""
	}

	if len(a.instances) >= a.maxInstances {
		return "", ErrInstanceLimit
	}

	a.nextInstanceID++
	id := InstanceID(fmt.Sprintf("instance-%d", a.nextInstanceID))
	a.instances[id] = &instanceState{id: id, material: material}
	m.instance = id
	return id, nil
}

// SetLoop changes the loop mode of one material.
func (a *MaterialAnimator) SetLoop(material string, mode LoopMode) error {
	if blankMaterialName(material) {
		return ErrBadMaterial
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	m, ok := a.materials[material]
	if !ok {
		return ErrUnknownMaterial
	}
	if mode < LoopOnce || mode > LoopPingPong {
		return ErrBadLoopMode
	}
	m.loop = mode
	return nil
}

// Loop reports the loop mode of one material.
func (a *MaterialAnimator) Loop(material string) (LoopMode, error) {
	if blankMaterialName(material) {
		return LoopOnce, ErrBadMaterial
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	m, ok := a.materials[material]
	if !ok {
		return LoopOnce, ErrUnknownMaterial
	}
	return m.loop, nil
}

// SetPaused freezes or resumes the phase of one material.
func (a *MaterialAnimator) SetPaused(material string, paused bool) error {
	if blankMaterialName(material) {
		return ErrBadMaterial
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	m, ok := a.materials[material]
	if !ok {
		return ErrUnknownMaterial
	}
	m.paused = paused
	return nil
}

// Bindings lists the binding ids of one material in creation order.
func (a *MaterialAnimator) Bindings(material string) []BindingID {
	if blankMaterialName(material) {
		return []BindingID{}
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	m, ok := a.materials[material]
	if !ok {
		return []BindingID{}
	}
	ids := make([]BindingID, 0, len(m.order))
	return append(ids, m.order...)
}

// Stats reports the current accounting of the animator.
func (a *MaterialAnimator) Stats() AnimStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	stats := AnimStats{
		Materials: len(a.materials),
		Curves:    len(a.curves),
		Bindings:  len(a.bindings),
		Instances: len(a.instances),
		Frames:    a.frames,
	}
	for _, keys := range a.curves {
		stats.Keys += len(keys)
	}
	return stats
}

// Snapshot serialises materials, curves, bindings, instances and phases.
func (a *MaterialAnimator) Snapshot() ([]byte, error) {
	a.mu.RLock()
	data := snapshotData{
		Frames:         a.frames,
		NextBindingID:  a.nextBindingID,
		NextInstanceID: a.nextInstanceID,
		Materials:      a.snapshotMaterialsLocked(),
		Curves:         a.snapshotCurvesLocked(),
		Bindings:       a.snapshotBindingsLocked(),
		Instances:      a.snapshotInstancesLocked(),
	}
	a.mu.RUnlock()

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)

	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(digest[:]),
		Data:     payload,
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *MaterialAnimator) Restore(raw []byte) error {
	var envelope snapshotEnvelope
	if err := decodeSnapshotJSON(raw, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || len(envelope.Data) == 0 || envelope.Checksum == "" {
		return ErrBadSnapshot
	}

	digest := sha256.Sum256(envelope.Data)
	if envelope.Checksum != hex.EncodeToString(digest[:]) {
		return ErrBadSnapshot
	}

	var data snapshotData
	if err := decodeSnapshotJSON(envelope.Data, &data); err != nil {
		return ErrBadSnapshot
	}
	state, err := buildRestoredState(&data, a.maxCurves, a.maxKeys, a.maxInstances)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.frames = data.Frames
	a.nextBindingID = state.nextBindingID
	a.nextInstanceID = state.nextInstanceID
	a.materials = state.materials
	a.curves = state.curves
	a.bindings = state.bindings
	a.instances = state.instances
	return nil
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Data     json.RawMessage `json:"data"`
}

type snapshotData struct {
	Frames         int64              `json:"frames"`
	NextBindingID  int64              `json:"next_binding_id"`
	NextInstanceID int64              `json:"next_instance_id"`
	Materials      []materialSnapshot `json:"materials"`
	Curves         []curveSnapshot    `json:"curves"`
	Bindings       []bindingSnapshot  `json:"bindings"`
	Instances      []instanceSnapshot `json:"instances"`
}

type materialSnapshot struct {
	Name         string             `json:"name"`
	Base         map[string]float64 `json:"base"`
	Phase        time.Duration      `json:"phase"`
	Loop         LoopMode           `json:"loop"`
	Paused       bool               `json:"paused"`
	BindingOrder []BindingID        `json:"binding_order"`
	Instance     InstanceID         `json:"instance"`
}

type curveSnapshot struct {
	ID   CurveID `json:"id"`
	Keys []Key   `json:"keys"`
}

type bindingSnapshot struct {
	ID       BindingID `json:"id"`
	Material string    `json:"material"`
	Property string    `json:"property"`
	Curve    CurveID   `json:"curve"`
}

type instanceSnapshot struct {
	ID          InstanceID `json:"id"`
	Material    string     `json:"material"`
	Retired     bool       `json:"retired"`
	RetireFrame int64      `json:"retire_frame"`
}

type restoredState struct {
	materials      map[string]*materialState
	curves         map[CurveID][]Key
	bindings       map[BindingID]*bindingState
	instances      map[InstanceID]*instanceState
	nextBindingID  int64
	nextInstanceID int64
}

func (a *MaterialAnimator) snapshotMaterialsLocked() []materialSnapshot {
	names := make([]string, 0, len(a.materials))
	for name := range a.materials {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]materialSnapshot, 0, len(names))
	for _, name := range names {
		m := a.materials[name]
		base := make(map[string]float64, len(m.base))
		for prop, value := range m.base {
			base[prop] = value
		}
		out = append(out, materialSnapshot{
			Name:         name,
			Base:         base,
			Phase:        m.phase,
			Loop:         m.loop,
			Paused:       m.paused,
			BindingOrder: append([]BindingID(nil), m.order...),
			Instance:     m.instance,
		})
	}
	return out
}

func (a *MaterialAnimator) snapshotCurvesLocked() []curveSnapshot {
	ids := make([]CurveID, 0, len(a.curves))
	for id := range a.curves {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]curveSnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, curveSnapshot{
			ID:   id,
			Keys: append([]Key(nil), a.curves[id]...),
		})
	}
	return out
}

func (a *MaterialAnimator) snapshotBindingsLocked() []bindingSnapshot {
	ids := make([]BindingID, 0, len(a.bindings))
	for id := range a.bindings {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]bindingSnapshot, 0, len(ids))
	for _, id := range ids {
		b := a.bindings[id]
		out = append(out, bindingSnapshot{
			ID:       b.id,
			Material: b.material,
			Property: b.property,
			Curve:    b.curve,
		})
	}
	return out
}

func (a *MaterialAnimator) snapshotInstancesLocked() []instanceSnapshot {
	ids := make([]InstanceID, 0, len(a.instances))
	for id := range a.instances {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]instanceSnapshot, 0, len(ids))
	for _, id := range ids {
		instance := a.instances[id]
		out = append(out, instanceSnapshot{
			ID:          instance.id,
			Material:    instance.material,
			Retired:     instance.retired,
			RetireFrame: instance.retireFrame,
		})
	}
	return out
}

func buildRestoredState(data *snapshotData, maxCurves, maxKeys, maxInstances int) (*restoredState, error) {
	if data.Frames < 0 || data.NextBindingID < 0 || data.NextInstanceID < 0 {
		return nil, errors.New("negative counters")
	}

	state := &restoredState{
		materials: make(map[string]*materialState),
		curves:    make(map[CurveID][]Key),
		bindings:  make(map[BindingID]*bindingState),
		instances: make(map[InstanceID]*instanceState),
	}

	for _, mSnapshot := range data.Materials {
		name := mSnapshot.Name
		if blankMaterialName(name) {
			return nil, errors.New("material name is empty")
		}
		if _, exists := state.materials[name]; exists {
			return nil, fmt.Errorf("duplicate material %q", name)
		}
		if mSnapshot.Phase < 0 {
			return nil, fmt.Errorf("negative phase for %q", name)
		}
		if mSnapshot.Loop < LoopOnce || mSnapshot.Loop > LoopPingPong {
			return nil, fmt.Errorf("bad loop mode for %q", name)
		}

		base := make(map[string]float64, len(mSnapshot.Base))
		for prop, value := range mSnapshot.Base {
			if !supportedProperty(prop) {
				return nil, fmt.Errorf("unknown property %q", prop)
			}
			base[prop] = value
		}

		state.materials[name] = &materialState{
			base:     base,
			phase:    mSnapshot.Phase,
			loop:     mSnapshot.Loop,
			paused:   mSnapshot.Paused,
			order:    append([]BindingID(nil), mSnapshot.BindingOrder...),
			props:    make(map[string]BindingID),
			instance: mSnapshot.Instance,
		}
	}

	if len(data.Curves) > maxCurves {
		return nil, ErrCurveLimit
	}
	for _, curve := range data.Curves {
		if _, exists := state.curves[curve.ID]; exists {
			return nil, fmt.Errorf("duplicate curve %q", curve.ID)
		}
		keys := append([]Key(nil), curve.Keys...)
		if err := validateKeys(keys, maxKeys); err != nil {
			return nil, err
		}
		state.curves[curve.ID] = keys
	}

	for _, binding := range data.Bindings {
		if binding.ID == "" {
			return nil, errors.New("empty binding id")
		}
		if _, exists := state.bindings[binding.ID]; exists {
			return nil, fmt.Errorf("duplicate binding %q", binding.ID)
		}
		m, ok := state.materials[binding.Material]
		if !ok {
			return nil, fmt.Errorf("binding %q references unknown material", binding.ID)
		}
		if !supportedProperty(binding.Property) {
			return nil, fmt.Errorf("binding %q references unknown property", binding.ID)
		}
		if _, ok := state.curves[binding.Curve]; !ok {
			return nil, fmt.Errorf("binding %q references unknown curve", binding.ID)
		}
		if _, exists := m.props[binding.Property]; exists {
			return nil, fmt.Errorf("duplicate property binding for %q/%q", binding.Material, binding.Property)
		}

		state.bindings[binding.ID] = &bindingState{
			id:       binding.ID,
			material: binding.Material,
			property: binding.Property,
			curve:    binding.Curve,
		}
		m.props[binding.Property] = binding.ID
	}

	for name, m := range state.materials {
		seen := make(map[BindingID]bool, len(m.order))
		order := make([]BindingID, 0, len(m.order))
		for _, id := range m.order {
			if id == "" || seen[id] {
				return nil, fmt.Errorf("bad binding order for material %q", name)
			}
			seen[id] = true
			binding, ok := state.bindings[id]
			if !ok || binding.material != name || m.props[binding.property] != id {
				return nil, fmt.Errorf("binding order mismatch for material %q", name)
			}
			order = append(order, id)
		}
		if len(order) != len(m.props) {
			return nil, fmt.Errorf("binding order is incomplete for material %q", name)
		}
		m.order = order
	}

	if len(data.Instances) > maxInstances {
		return nil, ErrInstanceLimit
	}
	for _, instance := range data.Instances {
		if instance.ID == "" {
			return nil, errors.New("empty instance id")
		}
		if _, exists := state.instances[instance.ID]; exists {
			return nil, fmt.Errorf("duplicate instance %q", instance.ID)
		}
		if instance.RetireFrame < 0 {
			return nil, fmt.Errorf("negative retire frame for instance %q", instance.ID)
		}
		m, ok := state.materials[instance.Material]
		if !ok {
			return nil, fmt.Errorf("instance %q references unknown material", instance.ID)
		}
		if m.instance != instance.ID {
			return nil, fmt.Errorf("instance %q allocation does not match material", instance.ID)
		}
		state.instances[instance.ID] = &instanceState{
			id:          instance.ID,
			material:    instance.Material,
			retired:     instance.Retired,
			retireFrame: instance.RetireFrame,
		}
	}
	for name, m := range state.materials {
		if m.instance != "" {
			instance, ok := state.instances[m.instance]
			if !ok {
				return nil, fmt.Errorf("material %q references missing instance", name)
			}
			if instance.material != name {
				return nil, fmt.Errorf("instance %q is owned by another material", m.instance)
			}
		}
	}

	state.nextBindingID = data.NextBindingID
	if maxID := maxGeneratedBindingID(state.bindings, "binding-"); maxID > state.nextBindingID {
		state.nextBindingID = maxID
	}
	state.nextInstanceID = data.NextInstanceID
	if maxID := maxGeneratedInstanceID(state.instances, "instance-"); maxID > state.nextInstanceID {
		state.nextInstanceID = maxID
	}
	return state, nil
}

func (a *MaterialAnimator) reviveInstanceLocked(m *materialState) {
	if m.instance == "" {
		return
	}
	if instance, ok := a.instances[m.instance]; ok {
		instance.retired = false
		instance.retireFrame = 0
	}
}

func (a *MaterialAnimator) reapRetiredInstancesLocked() {
	for id, instance := range a.instances {
		if !instance.retired || instance.retireFrame > a.frames {
			continue
		}
		delete(a.instances, id)
		if m, ok := a.materials[instance.material]; ok && m.instance == id {
			m.instance = ""
		}
	}
}

func supportedProperty(prop string) bool {
	switch prop {
	case PropEmissive, PropOpacity, PropUVOffsetU, PropUVOffsetV:
		return true
	default:
		return false
	}
}

func blankMaterialName(name string) bool {
	return strings.TrimSpace(name) == ""
}

func validateKeys(keys []Key, maxKeys int) error {
	if len(keys) == 0 {
		return ErrBadKeys
	}
	if len(keys) > maxKeys {
		return ErrKeyLimit
	}
	for i, key := range keys {
		if key.At < 0 {
			return ErrBadKeys
		}
		if i > 0 && key.At < keys[i-1].At {
			return ErrBadKeys
		}
	}
	return nil
}

func removeBindingID(ids []BindingID, target BindingID) []BindingID {
	filtered := ids[:0]
	for _, id := range ids {
		if id != target {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func sampleCurve(keys []Key, phase time.Duration, mode LoopMode) float64 {
	if len(keys) == 0 {
		return 0
	}

	first := keys[0]
	last := keys[len(keys)-1]
	if phase == 0 || phase <= first.At {
		return first.Value
	}
	if len(keys) == 1 || last.At <= first.At {
		return first.Value
	}

	end := last.At
	switch mode {
	case LoopOnce:
		if phase >= end {
			return last.Value
		}
	case LoopRepeat:
		if phase == end {
			return last.Value
		}
		if phase > end {
			phase %= end
			if phase == 0 || phase <= first.At {
				return first.Value
			}
		}
	case LoopPingPong:
		if phase == end {
			return last.Value
		}
		if phase > end {
			cycle := 2 * end
			wrapped := phase % cycle
			if wrapped > end {
				wrapped = cycle - wrapped
			}
			phase = wrapped
			if phase == 0 || phase <= first.At {
				return first.Value
			}
		}
	default:
		if phase >= end {
			return last.Value
		}
	}

	if phase >= end {
		return last.Value
	}

	for i := 1; i < len(keys); i++ {
		right := keys[i]
		if phase == right.At {
			return right.Value
		}
		if phase < right.At {
			left := keys[i-1]
			span := right.At - left.At
			if span <= 0 {
				return right.Value
			}
			ratio := float64(phase-left.At) / float64(span)
			return left.Value + ratio*(right.Value-left.Value)
		}
	}
	return last.Value
}

func decodeSnapshotJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}

	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("snapshot contains multiple JSON values")
		}
		return err
	}
	return nil
}

func maxGeneratedBindingID(items map[BindingID]*bindingState, prefix string) int64 {
	var max int64
	for id := range items {
		text := strings.TrimPrefix(string(id), prefix)
		if text == string(id) {
			continue
		}
		if value, err := parseGeneratedID(text); err == nil && value > max {
			max = value
		}
	}
	return max
}

func maxGeneratedInstanceID(items map[InstanceID]*instanceState, prefix string) int64 {
	var max int64
	for id := range items {
		text := strings.TrimPrefix(string(id), prefix)
		if text == string(id) {
			continue
		}
		if value, err := parseGeneratedID(text); err == nil && value > max {
			max = value
		}
	}
	return max
}

func parseGeneratedID(text string) (int64, error) {
	var value int64
	_, err := fmt.Sscanf(text, "%d", &value)
	if err != nil {
		return 0, err
	}
	return value, nil
}
