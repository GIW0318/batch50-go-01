// Package task validates kernel live patches against target hosts and orders
// their loading into a deterministic, transactional stack.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

// Errors returned by the guard.
var (
	ErrBlankPatch        = errors.New("patch id is required")
	ErrPatchExists       = errors.New("patch already registered")
	ErrPatchLimit        = errors.New("patch table is full")
	ErrCyclicPatch       = errors.New("patch depends on itself")
	ErrBadVersionRange   = errors.New("kernel version range is invalid")
	ErrBlankHost         = errors.New("host name and kernel version are required")
	ErrHostExists        = errors.New("host already registered")
	ErrHostLimit         = errors.New("host table is full")
	ErrUnknownHost       = errors.New("unknown host")
	ErrUnknownPatch      = errors.New("unknown patch")
	ErrPrereqMissing     = errors.New("prerequisite patch is missing")
	ErrKernelMismatch    = errors.New("patch does not apply to this kernel")
	ErrPatchConflict     = errors.New("patches conflict with each other")
	ErrUnknownPlan       = errors.New("unknown plan")
	ErrPlanConsumed      = errors.New("plan has already been applied")
	ErrPlanNotApplied    = errors.New("plan was never applied")
	ErrStackLimit        = errors.New("patch stack depth limit reached")
	ErrHostBusy          = errors.New("host already has a patch transaction in flight")
	ErrBadRollbackReason = errors.New("rollback reason is required")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Patch describes one live patch and its declarations.
type Patch struct {
	ID        string
	Prereqs   []string
	Conflicts []string
	MinKernel string
	MaxKernel string
	BuildID   string
	Seq       int64
}

// PatchID is the identity of a registered patch.
type PatchID string

// Plan is a deterministic loading order for one host.
type Plan struct {
	ID     string
	Host   string
	Order  []PatchID
	Kernel string
	Seq    uint64
}

// Result is the outcome of one applied plan.
type Result struct {
	PlanID  string
	Host    string
	Applied []PatchID
	Depth   int
}

// HostState is the live projection of one target host.
type HostState struct {
	Host         string
	Kernel       string
	BuildID      string
	Stack        []PatchID
	Busy         bool
	LastRollback string
	LastError    string
}

// Limits bounds the host table, the patch table and the stack depth.
type Limits struct {
	MaxStackDepth int
	MaxHosts      int
	MaxPatches    int
	Now           func() time.Time
}

// LivepatchConsistencyGuard owns the patch table, the host table and the plans.
type LivepatchConsistencyGuard struct {
	limits Limits
}

// NewLivepatchConsistencyGuard builds a guard with the given caps.
func NewLivepatchConsistencyGuard(limits Limits) *LivepatchConsistencyGuard {
	return &LivepatchConsistencyGuard{limits: limits}
}

// RegisterHost records one target host with its kernel and build identity.
func (g *LivepatchConsistencyGuard) RegisterHost(host, kernel, buildID string) error {
	return errNotImplemented
}

// Register records one patch and returns its identifier.
func (g *LivepatchConsistencyGuard) Register(p Patch) (PatchID, error) {
	return "", errNotImplemented
}

// Plan computes the deterministic loading order for one host.
func (g *LivepatchConsistencyGuard) Plan(host string) (Plan, error) {
	return Plan{}, errNotImplemented
}

// Apply loads every patch of one plan in order, all or nothing.
func (g *LivepatchConsistencyGuard) Apply(planID string) (Result, error) {
	return Result{}, errNotImplemented
}

// Rollback removes the patches loaded by one plan, newest first.
func (g *LivepatchConsistencyGuard) Rollback(planID string, reason string) error {
	return errNotImplemented
}

// Status reads the live state of one host.
func (g *LivepatchConsistencyGuard) Status(host string) (HostState, error) {
	return HostState{}, errNotImplemented
}

// Hosts lists the registered hosts in registration order.
func (g *LivepatchConsistencyGuard) Hosts() []string {
	return nil
}

// PatchInfo reads one registered patch.
func (g *LivepatchConsistencyGuard) PatchInfo(id PatchID) (Patch, bool) {
	return Patch{}, false
}

// Snapshot serialises patches, hosts, stacks and plans.
func (g *LivepatchConsistencyGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *LivepatchConsistencyGuard) Restore(data []byte) error {
	return errNotImplemented
}
