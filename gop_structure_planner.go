// Package task plans the GOP structure of an encode: where IDRs go, which
// frames become P or B frames and how display order maps onto decode order.
package task

import "errors"

// errNotImplemented marks behaviour that still has to be written.
var errNotImplemented = errors.New("not implemented")

// GOP modes.
const (
	GopClosed = "closed"
	GopOpen   = "open"
)

// Frame types produced by the planner.
const (
	FrameI = "I"
	FrameP = "P"
	FrameB = "B"
)

// Issue codes reported by Validate.
const (
	IssueIDRBudget         = "idr_budget_exceeded"
	IssuePOCNotReset       = "poc_not_reset"
	IssueRefCrossesIDR     = "ref_crosses_idr"
	IssueMissingLeadingIDR = "missing_leading_idr"
)

// Errors reported by the planner.
var (
	ErrBadMode       = errors.New("gop mode is unknown")
	ErrBadGOP        = errors.New("gop length is out of range")
	ErrBadBFrames    = errors.New("b frame depth is out of range")
	ErrEmptyPlan     = errors.New("frame list is empty")
	ErrFrameLimit    = errors.New("frame list exceeds the configured limit")
	ErrCutOutOfRange = errors.New("cut index is outside the clip")
	ErrPlanExists    = errors.New("a plan already exists")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Limits bounds the encoder capabilities and the clip budget.
type Limits struct {
	MaxGOP          int
	MaxBFrames      int
	MaxIDRPerMinute int
	MaxFrames       int
	NominalFPS      float64
}

// GopProfile is the per encode structural profile.
type GopProfile struct {
	Mode        string
	MaxGOP      int
	MinGOP      int
	BFrameDepth int
}

// FrameHint is the evidence the planner gets for one display frame.
type FrameHint struct {
	SceneCut   bool
	Complexity float64
	PTS        int64
}

// FramePlan is the decision taken for one display frame. Decode is the global
// decode position, Ref the display index this frame predicts from (-1 for I).
type FramePlan struct {
	Index  int
	Type   string
	IDR    bool
	GOP    int
	POC    int
	Decode int
	Ref    int
}

// GopStats summarises one plan.
type GopStats struct {
	GOPs       int
	IDRs       int
	Shortened  int
	BFrames    int
	AverageGOP float64
}

// GopIssue is one validation finding.
type GopIssue struct {
	Code   string
	Frame  int
	Detail string
}

// GopStructurePlanner owns the limits, the profile, the cut requests and the
// last plan it produced.
type GopStructurePlanner struct {
	limits Limits
}

// NewGopStructurePlanner stores the limits and starts from a closed profile
// with the largest GOP the encoder allows and no B frames.
func NewGopStructurePlanner(limits Limits) *GopStructurePlanner {
	return &GopStructurePlanner{limits: limits}
}

// Constraints returns the limits the planner was built with.
func (p *GopStructurePlanner) Constraints() Limits {
	return Limits{}
}

// Profile returns the active profile.
func (p *GopStructurePlanner) Profile() GopProfile {
	return GopProfile{}
}

// SetProfile validates and stores a new profile.
func (p *GopStructurePlanner) SetProfile(profile GopProfile) error {
	return errNotImplemented
}

// Plan maps the frame hints onto GOP boundaries, frame types and decode order.
func (p *GopStructurePlanner) Plan(frames []FrameHint) ([]FramePlan, error) {
	return nil, errNotImplemented
}

// InsertCut requests an extra IDR at the given display index.
func (p *GopStructurePlanner) InsertCut(atFrame int) error {
	return errNotImplemented
}

// Validate reports every structural violation of the current plan.
func (p *GopStructurePlanner) Validate() []GopIssue {
	return nil
}

// Stats summarises the current plan.
func (p *GopStructurePlanner) Stats() GopStats {
	return GopStats{}
}

// PlanFrame reads the decision for one display index.
func (p *GopStructurePlanner) PlanFrame(index int) (FramePlan, bool) {
	return FramePlan{}, false
}

// Reset drops the plan and every requested cut.
func (p *GopStructurePlanner) Reset() error {
	return errNotImplemented
}

// Snapshot serialises the profile, the cut requests and the plan.
func (p *GopStructurePlanner) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *GopStructurePlanner) Restore(data []byte) error {
	return errNotImplemented
}
