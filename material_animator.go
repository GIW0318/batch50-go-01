// Package task drives runtime material property animation: curve sampling,
// shared to instance splitting and per material time bookkeeping.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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

// MaterialAnimator owns the curve table, the material table, the bindings,
// the instance ledger and the clock used for sampling.
type MaterialAnimator struct {
	maxCurves    int
	maxKeys      int
	maxInstances int
}

// NewMaterialAnimator builds an animator with hard caps on curves, key frames
// per curve and material instances.
func NewMaterialAnimator(maxCurves, maxKeys, maxInstances int) *MaterialAnimator {
	return &MaterialAnimator{maxCurves: maxCurves, maxKeys: maxKeys, maxInstances: maxInstances}
}

// AddMaterial registers a shared material with its base property values.
func (a *MaterialAnimator) AddMaterial(name string, base map[string]float64) error {
	return errNotImplemented
}

// SetCurve stores or replaces one animation curve.
func (a *MaterialAnimator) SetCurve(id CurveID, keys []Key) error {
	return errNotImplemented
}

// Bind attaches a curve to one property of one material.
func (a *MaterialAnimator) Bind(material, prop string, curve CurveID) (BindingID, error) {
	return "", errNotImplemented
}

// Unbind removes one binding and lets the property fall back to the base value.
func (a *MaterialAnimator) Unbind(id BindingID) error {
	return errNotImplemented
}

// Advance moves every unpaused material phase by dt and bumps the frame counter.
func (a *MaterialAnimator) Advance(dt time.Duration) error {
	return errNotImplemented
}

// Value samples one property of one material at the current phase.
func (a *MaterialAnimator) Value(material, prop string) (float64, error) {
	return 0, errNotImplemented
}

// Materialize splits a material that carries animated properties into its own
// instance and returns the instance id.
func (a *MaterialAnimator) Materialize(material string) (InstanceID, error) {
	return "", errNotImplemented
}

// SetLoop changes the loop mode of one material.
func (a *MaterialAnimator) SetLoop(material string, mode LoopMode) error {
	return errNotImplemented
}

// Loop reports the loop mode of one material.
func (a *MaterialAnimator) Loop(material string) (LoopMode, error) {
	return 0, errNotImplemented
}

// SetPaused freezes or resumes the phase of one material.
func (a *MaterialAnimator) SetPaused(material string, paused bool) error {
	return errNotImplemented
}

// Bindings lists the binding ids of one material in creation order.
func (a *MaterialAnimator) Bindings(material string) []BindingID {
	return nil
}

// Stats reports the current accounting of the animator.
func (a *MaterialAnimator) Stats() AnimStats {
	return AnimStats{}
}

// Snapshot serialises materials, curves, bindings, instances and phases.
func (a *MaterialAnimator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *MaterialAnimator) Restore(data []byte) error {
	return errNotImplemented
}
