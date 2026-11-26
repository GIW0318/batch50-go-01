// Package task plans the GOP structure of an encode: where IDRs go, which
// frames become P or B frames and how display order maps onto decode order.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

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

const snapshotVersion = 1

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

type plannerState struct {
	profile GopProfile
	cuts    map[int]struct{}
	plan    []FramePlan
	stats   GopStats
}

type snapshotData struct {
	Version int         `json:"version"`
	Profile GopProfile  `json:"profile"`
	Cuts    []int       `json:"cuts"`
	Plan    []FramePlan `json:"plan"`
	Stats   GopStats    `json:"stats"`
}

// GopStructurePlanner owns the limits, the profile, the cut requests and the
// last plan it produced.
type GopStructurePlanner struct {
	limits   Limits
	writeMu  sync.Mutex
	snapshot atomic.Pointer[plannerState]
}

type gopSegment struct {
	start     int
	end       int
	shortened bool
}

// NewGopStructurePlanner stores the limits and starts from a closed profile
// with the largest GOP the encoder allows and no B frames.
func NewGopStructurePlanner(limits Limits) *GopStructurePlanner {
	planner := &GopStructurePlanner{limits: limits}
	profile := GopProfile{
		Mode:        GopClosed,
		MaxGOP:      limits.MaxGOP,
		MinGOP:      1,
		BFrameDepth: 0,
	}
	planner.snapshot.Store(&plannerState{
		profile: profile,
		cuts:    make(map[int]struct{}),
	})
	return planner
}

// Constraints returns the limits the planner was built with.
func (p *GopStructurePlanner) Constraints() Limits {
	return p.limits
}

// Profile returns the active profile.
func (p *GopStructurePlanner) Profile() GopProfile {
	return p.current().profile
}

// SetProfile validates and stores a new profile.
func (p *GopStructurePlanner) SetProfile(profile GopProfile) error {
	if err := validateProfile(profile, p.limits); err != nil {
		return err
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	next := *p.current()
	next.profile = profile
	p.snapshot.Store(&next)
	return nil
}

// Plan maps the frame hints onto GOP boundaries, frame types and decode order.
func (p *GopStructurePlanner) Plan(frames []FrameHint) ([]FramePlan, error) {
	if len(frames) == 0 {
		return nil, ErrEmptyPlan
	}
	if len(frames) > p.limits.MaxFrames {
		return nil, ErrFrameLimit
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	current := p.current()
	plan, stats, err := buildPlan(frames, current.profile, current.cuts)
	if err != nil {
		return nil, err
	}

	p.snapshot.Store(&plannerState{
		profile: current.profile,
		cuts:    current.cuts,
		plan:    plan,
		stats:   stats,
	})
	return append([]FramePlan(nil), plan...), nil
}

// InsertCut requests an extra IDR at the given display index.
func (p *GopStructurePlanner) InsertCut(atFrame int) error {
	if atFrame < 0 {
		return ErrCutOutOfRange
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	current := p.current()
	if len(current.plan) > 0 {
		return ErrPlanExists
	}
	if _, exists := current.cuts[atFrame]; exists {
		return nil
	}

	cuts := make(map[int]struct{}, len(current.cuts)+1)
	for cut := range current.cuts {
		cuts[cut] = struct{}{}
	}
	cuts[atFrame] = struct{}{}

	next := *current
	next.cuts = cuts
	p.snapshot.Store(&next)
	return nil
}

// Validate reports every structural violation of the current plan.
func (p *GopStructurePlanner) Validate() []GopIssue {
	plan := p.current().plan
	if len(plan) == 0 {
		return nil
	}

	issues := make([]GopIssue, 0)
	idrCount := 0

	if !plan[0].IDR || plan[0].Type != FrameI {
		issues = append(issues, GopIssue{
			Code:   IssueMissingLeadingIDR,
			Frame:  0,
			Detail: "the first frame must be an IDR I frame",
		})
	}

	gopStart := 0
	for index, frame := range plan {
		if frame.IDR {
			idrCount++
			if frame.POC != 0 {
				issues = append(issues, GopIssue{
					Code:   IssuePOCNotReset,
					Frame:  index,
					Detail: "POC must reset to zero at an IDR",
				})
			}
			if frame.Ref != -1 {
				issues = append(issues, GopIssue{
					Code:   IssueRefCrossesIDR,
					Frame:  index,
					Detail: "an I frame must not use a reference frame",
				})
			}
			gopStart = index
			continue
		}

		if frame.POC != index-gopStart {
			issues = append(issues, GopIssue{
				Code:   IssuePOCNotReset,
				Frame:  index,
				Detail: "POC must be continuous within a GOP",
			})
		}
		if frame.Ref < 0 || frame.Ref >= len(plan) || plan[frame.Ref].GOP != frame.GOP {
			issues = append(issues, GopIssue{
				Code:   IssueRefCrossesIDR,
				Frame:  index,
				Detail: "reference frames must stay inside the same GOP",
			})
		}
	}

	budget := idrBudget(p.limits, len(plan))
	if idrCount > budget {
		issues = append(issues, GopIssue{
			Code:   IssueIDRBudget,
			Frame:  0,
			Detail: fmt.Sprintf("IDR count %d exceeds budget %d", idrCount, budget),
		})
	}

	return issues
}

// Stats summarises the current plan.
func (p *GopStructurePlanner) Stats() GopStats {
	return p.current().stats
}

// PlanFrame reads the decision for one display index.
func (p *GopStructurePlanner) PlanFrame(index int) (FramePlan, bool) {
	plan := p.current().plan
	if index < 0 || index >= len(plan) {
		return FramePlan{}, false
	}
	return plan[index], true
}

// Reset drops the plan and every requested cut.
func (p *GopStructurePlanner) Reset() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	current := p.current()
	p.snapshot.Store(&plannerState{
		profile: current.profile,
		cuts:    make(map[int]struct{}),
	})
	return nil
}

// Snapshot serialises the profile, the cut requests and the plan.
func (p *GopStructurePlanner) Snapshot() ([]byte, error) {
	current := p.current()
	cuts := make([]int, 0, len(current.cuts))
	for cut := range current.cuts {
		cuts = append(cuts, cut)
	}
	sort.Ints(cuts)

	return json.Marshal(snapshotData{
		Version: snapshotVersion,
		Profile: current.profile,
		Cuts:    cuts,
		Plan:    current.plan,
		Stats:   current.stats,
	})
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *GopStructurePlanner) Restore(data []byte) error {
	state, err := decodeSnapshot(data, p.limits)
	if err != nil {
		return ErrBadSnapshot
	}

	p.writeMu.Lock()
	p.snapshot.Store(state)
	p.writeMu.Unlock()
	return nil
}

func (p *GopStructurePlanner) current() *plannerState {
	return p.snapshot.Load()
}

func validateProfile(profile GopProfile, limits Limits) error {
	if profile.Mode != GopClosed && profile.Mode != GopOpen {
		return ErrBadMode
	}
	if profile.MaxGOP < 1 || profile.MaxGOP > limits.MaxGOP ||
		profile.MinGOP < 1 || profile.MinGOP > profile.MaxGOP {
		return ErrBadGOP
	}
	if profile.BFrameDepth < 0 || profile.BFrameDepth > 3 ||
		profile.BFrameDepth > limits.MaxBFrames {
		return ErrBadBFrames
	}
	return nil
}

func buildPlan(frames []FrameHint, profile GopProfile, requestedCuts map[int]struct{}) ([]FramePlan, GopStats, error) {
	frameCount := len(frames)
	for cut := range requestedCuts {
		if cut < 0 || cut >= frameCount {
			return nil, GopStats{}, ErrCutOutOfRange
		}
	}

	forcedIDRs := make(map[int]struct{}, frameCount+len(requestedCuts)+1)
	forcedIDRs[0] = struct{}{}
	for index, hint := range frames {
		if hint.SceneCut {
			forcedIDRs[index] = struct{}{}
		}
	}
	for cut := range requestedCuts {
		forcedIDRs[cut] = struct{}{}
	}

	segments := make([]gopSegment, 0)
	for start := 0; start < frameCount; {
		regularEnd := start + profile.MaxGOP
		if regularEnd > frameCount {
			regularEnd = frameCount
		}

		end := regularEnd
		shortened := false
		for cut := range forcedIDRs {
			if cut > start && cut < end {
				end = cut
				shortened = true
			}
		}

		segments = append(segments, gopSegment{
			start:     start,
			end:       end,
			shortened: shortened,
		})
		start = end
	}

	plan := make([]FramePlan, frameCount)
	for gopIndex, segment := range segments {
		start := segment.start
		end := segment.end

		plan[start] = FramePlan{
			Index: start,
			Type:  FrameI,
			IDR:   true,
			GOP:   gopIndex,
			POC:   0,
			Ref:   -1,
		}

		lastAnchor := start
		anchorInterval := profile.BFrameDepth + 1
		for anchorOffset := anchorInterval; start+anchorOffset < end; anchorOffset += anchorInterval {
			anchor := start + anchorOffset
			for index := lastAnchor + 1; index < anchor; index++ {
				plan[index] = FramePlan{
					Index: index,
					Type:  FrameB,
					GOP:   gopIndex,
					POC:   index - start,
					Ref:   anchor,
				}
			}
			plan[anchor] = FramePlan{
				Index: anchor,
				Type:  FrameP,
				GOP:   gopIndex,
				POC:   anchor - start,
				Ref:   lastAnchor,
			}
			lastAnchor = anchor
		}

		for index := lastAnchor + 1; index < end; index++ {
			plan[index] = FramePlan{
				Index: index,
				Type:  FrameP,
				GOP:   gopIndex,
				POC:   index - start,
				Ref:   lastAnchor,
			}
			lastAnchor = index
		}
	}

	decodePosition := 0
	for _, segment := range segments {
		start := segment.start
		end := segment.end

		plan[start].Decode = decodePosition
		decodePosition++

		lastAnchor := start
		for index := start + 1; index < end; index++ {
			if plan[index].Type != FrameP {
				continue
			}

			plan[index].Decode = decodePosition
			decodePosition++

			for referenceIndex := lastAnchor + 1; referenceIndex < index; referenceIndex++ {
				if plan[referenceIndex].Type == FrameB {
					plan[referenceIndex].Decode = decodePosition
					decodePosition++
				}
			}
			lastAnchor = index
		}
	}

	stats := GopStats{
		GOPs:       len(segments),
		IDRs:       len(segments),
		AverageGOP: float64(frameCount) / float64(len(segments)),
	}
	for _, segment := range segments {
		if segment.shortened {
			stats.Shortened++
		}
	}
	for _, frame := range plan {
		if frame.Type == FrameB {
			stats.BFrames++
		}
	}

	return plan, stats, nil
}

func idrBudget(limits Limits, frameCount int) int {
	budget := 1
	if limits.NominalFPS > 0 {
		clipSeconds := float64(frameCount) / limits.NominalFPS
		budget = int(math.Ceil(float64(limits.MaxIDRPerMinute) * clipSeconds / 60.0))
	}
	if budget < 1 {
		return 1
	}
	return budget
}

func decodeSnapshot(data []byte, limits Limits) (*plannerState, error) {
	var dto snapshotData
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if dto.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported snapshot version", ErrBadSnapshot)
	}
	if err := validateProfile(dto.Profile, limits); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if len(dto.Plan) > limits.MaxFrames {
		return nil, fmt.Errorf("%w: plan exceeds frame limit", ErrBadSnapshot)
	}

	cuts := make(map[int]struct{}, len(dto.Cuts))
	for _, cut := range dto.Cuts {
		if cut < 0 {
			return nil, fmt.Errorf("%w: negative cut index", ErrBadSnapshot)
		}
		cuts[cut] = struct{}{}
	}

	return &plannerState{
		profile: dto.Profile,
		cuts:    cuts,
		plan:    append([]FramePlan(nil), dto.Plan...),
		stats:   dto.Stats,
	}, nil
}
