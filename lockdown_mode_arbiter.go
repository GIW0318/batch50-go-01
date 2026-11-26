// Package task arbitrates kernel lockdown levels, keeps the level watermark that
// callers may not walk back, and drives the controlled surfaces that have to be
// closed while a level is in force.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Level is a lockdown level. Higher numbers are stricter.
type Level int

const (
	// LevelNone keeps every controlled surface open.
	LevelNone Level = 0
	// LevelIntegrity blocks kernel self modification.
	LevelIntegrity Level = 1
	// LevelConfidentiality also blocks user space kernel memory access.
	LevelConfidentiality Level = 2
)

// Actor identifies who asks for a level change. Only signed actors may move the
// watermark.
type Actor struct {
	Name   string
	Signed bool
}

// Surface is one controlled interface that has to be closed while the active
// level reaches ClosedAt.
type Surface struct {
	Name       string
	Kind       string
	ClosedAt   Level
	Reopenable bool
	Cost       int
	Critical   bool
}

// SurfaceState is the snapshot of one surface.
type SurfaceState struct {
	Name       string
	Kind       string
	Open       bool
	ClosedAt   Level
	Reopenable bool
	Critical   bool
	CloseSeq   int64
}

// Transition is the explainable outcome of one SetLevel call.
type Transition struct {
	From    Level
	To      Level
	Applied bool
	Closed  []string
	Skipped []string
	Cost    int
	Seq     int64
}

// Config bounds the registered surfaces and the close work of one transition.
type Config struct {
	MaxSurfaces int
	CloseBudget int
}

// Errors returned by the arbiter.
var (
	ErrInvalidLevel   = errors.New("invalid lockdown level")
	ErrBadActor       = errors.New("actor name is required")
	ErrLevelMonotonic = errors.New("level watermark can only move upwards")
	ErrUnknownSurface = errors.New("unknown controlled surface")
	ErrSurfaceExists  = errors.New("controlled surface already registered")
	ErrBadSurface     = errors.New("controlled surface record is invalid")
	ErrSurfaceLimit   = errors.New("controlled surface limit reached")
	ErrSurfaceBlocked = errors.New("transition would close a critical surface")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// LockdownArbiter owns the level, the watermark and the surface table.
type LockdownArbiter struct {
	cfg Config
}

// NewLockdownArbiter builds an arbiter with the given caps.
func NewLockdownArbiter(cfg Config) *LockdownArbiter {
	return &LockdownArbiter{cfg: cfg}
}

// Current reports the active lockdown level.
func (a *LockdownArbiter) Current() Level {
	return LevelNone
}

// Watermark reports the level that may not be walked back.
func (a *LockdownArbiter) Watermark() Level {
	return LevelNone
}

// RegisterSurface adds one controlled surface to the table.
func (a *LockdownArbiter) RegisterSurface(s Surface) error {
	return errNotImplemented
}

// SetLevel moves the active level and closes or reopens the surfaces it owns.
func (a *LockdownArbiter) SetLevel(target Level, by Actor) (Transition, error) {
	return Transition{}, errNotImplemented
}

// SetWatermark raises the watermark for a signed actor.
func (a *LockdownArbiter) SetWatermark(target Level, by Actor) error {
	return errNotImplemented
}

// Explain reports one surface and why it is open or closed.
func (a *LockdownArbiter) Explain(surface string) (SurfaceState, error) {
	return SurfaceState{}, errNotImplemented
}

// Surfaces lists every controlled surface, ordered by name.
func (a *LockdownArbiter) Surfaces() []SurfaceState {
	return nil
}

// Snapshot serialises the level, the watermark and the surface table.
func (a *LockdownArbiter) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (a *LockdownArbiter) Restore(data []byte) error {
	return errNotImplemented
}
