// Package task plans edge and inner tessellation factors for the patches that
// make up one terrain surface. Patches are registered once, re-planned every
// frame and only take effect after the plan is committed.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Caps applied when the matching limit is left at zero or below.
const (
	DefaultMaxPatches    = 64
	DefaultMaxEdgeFactor = 64
)

// Errors reported by the planner.
var (
	ErrBadPatch        = errors.New("patch definition is invalid")
	ErrDuplicatePatch  = errors.New("patch is already registered")
	ErrPatchLimit      = errors.New("patch table is full")
	ErrUnknownPatch    = errors.New("patch is not registered")
	ErrBadDisplacement = errors.New("displacement height is invalid")
	ErrBadRatio        = errors.New("screen ratio must be within 0..1")
	ErrBadView         = errors.New("view parameters are invalid")
	ErrNoPatches       = errors.New("no patch is registered")
	ErrUnknownPlan     = errors.New("unknown tessellation plan")
	ErrNoPendingPlan   = errors.New("there is no pending plan")
	ErrBudgetExceeded  = errors.New("factors cannot fit the per frame budget")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Limits bounds the patch table, the per frame factor budget and the highest
// edge factor a single edge may reach.
type Limits struct {
	MaxPatches    int
	MaxFactorSum  int
	MaxEdgeFactor int
}

// Edge is one border of a patch. Two patches that touch each other register the
// same EdgeID so the planner can recognise the shared border.
type Edge struct {
	EdgeID     string
	NeighborID string
	Length     float64
}

// ViewParams describes the camera state used for one planning round.
type ViewParams struct {
	CameraDistance   float64
	PixelsPerUnit    float64
	PixelErrorBudget float64
	FrameID          int64
}

// PatchFactors is the plan for a single patch. EdgeFactors keeps the order the
// edges were registered in.
type PatchFactors struct {
	PatchID     string
	EdgeFactors []int
	InnerFactor int
	Level       int
}

// PlanStats reports the counters kept by the planner.
type PlanStats struct {
	Plans            int
	Commits          int
	Cancels          int
	PatchCount       int
	CommittedFrameID int64
	DegradeRounds    int
}

// TessellationFactorPlanner owns the patch table, the pending plan and the
// committed factors.
type TessellationFactorPlanner struct {
	limits Limits
}

// NewTessellationFactorPlanner builds a planner with the given caps.
func NewTessellationFactorPlanner(limits Limits) *TessellationFactorPlanner {
	return &TessellationFactorPlanner{limits: limits}
}

// RegisterPatch adds one patch and its border list to the table.
func (p *TessellationFactorPlanner) RegisterPatch(patchID string, edges []Edge) error {
	return errNotImplemented
}

// SetDisplacement records the peak displacement height of one patch.
func (p *TessellationFactorPlanner) SetDisplacement(patchID string, maxHeight float64) error {
	return errNotImplemented
}

// SetScreenRatio records how much of the screen one patch covers.
func (p *TessellationFactorPlanner) SetScreenRatio(patchID string, ratio float64) error {
	return errNotImplemented
}

// Patches lists the registered patch ids in ascending order.
func (p *TessellationFactorPlanner) Patches() []string {
	return nil
}

// PatchEdges reads back the border list of one patch.
func (p *TessellationFactorPlanner) PatchEdges(patchID string) ([]Edge, bool) {
	return nil, false
}

// Plan computes the factors for one frame and keeps them as the pending plan.
func (p *TessellationFactorPlanner) Plan(view ViewParams) ([]PatchFactors, error) {
	return nil, errNotImplemented
}

// PendingPlan reports the id of the plan waiting for a commit.
func (p *TessellationFactorPlanner) PendingPlan() (string, bool) {
	return "", false
}

// Commit makes the pending factors current.
func (p *TessellationFactorPlanner) Commit(planID string) error {
	return errNotImplemented
}

// Cancel drops the pending plan.
func (p *TessellationFactorPlanner) Cancel(planID string) error {
	return errNotImplemented
}

// Factor reads the committed factors of one patch.
func (p *TessellationFactorPlanner) Factor(patchID string) (PatchFactors, bool) {
	return PatchFactors{}, false
}

// Stats returns a copy of the planner counters.
func (p *TessellationFactorPlanner) Stats() PlanStats {
	return PlanStats{}
}

// Snapshot serialises the patch table, the pending plan and the committed
// factors.
func (p *TessellationFactorPlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the planner state with a snapshot, all or nothing.
func (p *TessellationFactorPlanner) Restore(data []byte) error {
	return errNotImplemented
}
