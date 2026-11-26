// Package task plans edge and inner tessellation factors for the patches that
// make up one terrain surface. Patches are registered once, re-planned every
// frame and only take effect after the plan is committed.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

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
	mu               sync.RWMutex
	limits           Limits
	patches          map[string]*patchState
	committed        map[string]PatchFactors
	committedDisp    map[string]float64
	committedPlanIDs map[string]struct{}
	pending          *pendingPlan
	stats            PlanStats
}

type patchState struct {
	id           string
	edges        []Edge
	displacement float64
	ratio        float64
}

type pendingPlan struct {
	id            string
	frameID       int64
	factors       []PatchFactors
	displacements map[string]float64
}

// NewTessellationFactorPlanner builds a planner with the given caps.
func NewTessellationFactorPlanner(limits Limits) *TessellationFactorPlanner {
	limits = normalizeLimits(limits)
	return &TessellationFactorPlanner{
		limits:           limits,
		patches:          make(map[string]*patchState),
		committed:        make(map[string]PatchFactors),
		committedDisp:    make(map[string]float64),
		committedPlanIDs: make(map[string]struct{}),
	}
}

// RegisterPatch adds one patch and its border list to the table.
func (p *TessellationFactorPlanner) RegisterPatch(patchID string, edges []Edge) error {
	if strings.TrimSpace(patchID) == "" || len(edges) == 0 {
		return ErrBadPatch
	}

	storedEdges := make([]Edge, len(edges))
	for i, edge := range edges {
		if strings.TrimSpace(edge.EdgeID) == "" || math.IsNaN(edge.Length) || edge.Length <= 0 {
			return ErrBadPatch
		}
		storedEdges[i] = edge
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.patches[patchID]; exists {
		return ErrDuplicatePatch
	}
	if len(p.patches) >= p.limits.MaxPatches {
		return ErrPatchLimit
	}

	p.patches[patchID] = &patchState{
		id:    patchID,
		edges: storedEdges,
	}
	p.stats.PatchCount = len(p.patches)
	return nil
}

// SetDisplacement records the peak displacement height of one patch.
func (p *TessellationFactorPlanner) SetDisplacement(patchID string, maxHeight float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	patch, ok := p.patches[patchID]
	if !ok {
		return ErrUnknownPatch
	}
	if math.IsNaN(maxHeight) || maxHeight < 0 {
		return ErrBadDisplacement
	}

	patch.displacement = maxHeight
	return nil
}

// SetScreenRatio records how much of the screen one patch covers.
func (p *TessellationFactorPlanner) SetScreenRatio(patchID string, ratio float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	patch, ok := p.patches[patchID]
	if !ok {
		return ErrUnknownPatch
	}
	if math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return ErrBadRatio
	}

	patch.ratio = ratio
	return nil
}

// Patches lists the registered patch ids in ascending order.
func (p *TessellationFactorPlanner) Patches() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ids := make([]string, 0, len(p.patches))
	for id := range p.patches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// PatchEdges reads back the border list of one patch.
func (p *TessellationFactorPlanner) PatchEdges(patchID string) ([]Edge, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	patch, ok := p.patches[patchID]
	if !ok {
		return nil, false
	}
	return append([]Edge(nil), patch.edges...), true
}

// Plan computes the factors for one frame and keeps them as the pending plan.
func (p *TessellationFactorPlanner) Plan(view ViewParams) ([]PatchFactors, error) {
	viewErr := validateView(view)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Every new adjudication replaces the one pending plan slot.
	p.pending = nil

	if viewErr != nil {
		return nil, viewErr
	}
	if len(p.patches) == 0 {
		return nil, ErrNoPatches
	}

	ids := sortedPatchIDs(p.patches)
	planned := make([]PatchFactors, 0, len(ids))
	for _, id := range ids {
		patch := p.patches[id]
		edgeFactors := make([]int, len(patch.edges))
		for i, edge := range patch.edges {
			continuous := edge.Length * view.PixelsPerUnit /
				(view.CameraDistance * view.PixelErrorBudget) *
				(1 + patch.displacement)
			edgeFactors[i] = ceilPowerOfTwo(continuous, p.limits.MaxEdgeFactor)
		}
		planned = append(planned, PatchFactors{
			PatchID:     id,
			EdgeFactors: edgeFactors,
		})
	}

	unifySharedEdges(ids, p.patches, planned)
	limitDownwardChangesOneTier(ids, p.patches, p.committed, p.committedDisp, planned, p.limits.MaxEdgeFactor)
	unifySharedEdges(ids, p.patches, planned)

	for i := range planned {
		recomputeDerivedFactors(&planned[i])
	}

	degradeRounds, err := fitFactorBudget(planned, p.patches, p.limits.MaxFactorSum)
	if err != nil {
		return nil, err
	}

	p.stats.Plans++
	p.stats.DegradeRounds += degradeRounds
	planID := fmt.Sprintf("plan-%d", p.stats.Plans)
	factorsForPending := clonePatchFactors(planned)
	displacementsForPending := make(map[string]float64, len(ids))
	for _, id := range ids {
		displacementsForPending[id] = p.patches[id].displacement
	}
	p.pending = &pendingPlan{
		id:            planID,
		frameID:       view.FrameID,
		factors:       factorsForPending,
		displacements: displacementsForPending,
	}

	return clonePatchFactors(p.pending.factors), nil
}

// PendingPlan reports the id of the plan waiting for a commit.
func (p *TessellationFactorPlanner) PendingPlan() (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.pending == nil {
		return "", false
	}
	return p.pending.id, true
}

// Commit makes the pending factors current.
func (p *TessellationFactorPlanner) Commit(planID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, committed := p.committedPlanIDs[planID]; committed {
		return nil
	}
	if p.pending == nil || p.pending.id != planID {
		return ErrUnknownPlan
	}

	committed := make(map[string]PatchFactors, len(p.pending.factors))
	for _, factors := range p.pending.factors {
		committed[factors.PatchID] = cloneOnePatchFactors(factors)
	}

	p.committed = committed
	if p.committedDisp == nil {
		p.committedDisp = make(map[string]float64)
	}
	for patchID, height := range p.pending.displacements {
		p.committedDisp[patchID] = height
	}
	p.committedPlanIDs[planID] = struct{}{}
	p.stats.CommittedFrameID = p.pending.frameID
	p.stats.Commits++
	p.pending = nil
	return nil
}

// Cancel drops the pending plan.
func (p *TessellationFactorPlanner) Cancel(planID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pending == nil {
		return fmt.Errorf("%w: %w", ErrUnknownPlan, ErrNoPendingPlan)
	}
	if p.pending.id != planID {
		return ErrUnknownPlan
	}

	p.pending = nil
	p.stats.Cancels++
	return nil
}

// Factor reads the committed factors of one patch.
func (p *TessellationFactorPlanner) Factor(patchID string) (PatchFactors, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	factors, ok := p.committed[patchID]
	if !ok {
		return PatchFactors{}, false
	}
	return cloneOnePatchFactors(factors), true
}

// Stats returns a copy of the planner counters.
func (p *TessellationFactorPlanner) Stats() PlanStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := p.stats
	stats.PatchCount = len(p.patches)
	return stats
}

// Snapshot serialises the patch table, the pending plan and the committed
// factors.
func (p *TessellationFactorPlanner) Snapshot() ([]byte, error) {
	p.mu.RLock()
	snapshot := snapshotData{
		Magic:            snapshotMagic,
		Version:          snapshotVersion,
		Limits:           p.limits,
		Stats:            p.stats,
		CommittedFrameID: p.stats.CommittedFrameID,
	}

	ids := sortedPatchIDs(p.patches)
	snapshot.Patches = make([]snapshotPatch, 0, len(ids))
	for _, id := range ids {
		patch := p.patches[id]
		snapshot.Patches = append(snapshot.Patches, snapshotPatch{
			ID:           patch.id,
			Edges:        append([]Edge(nil), patch.edges...),
			Displacement: patch.displacement,
			Ratio:        patch.ratio,
		})
	}

	committedIDs := make([]string, 0, len(p.committed))
	for id := range p.committed {
		committedIDs = append(committedIDs, id)
	}
	sort.Strings(committedIDs)
	snapshot.CommittedFactors = make([]PatchFactors, 0, len(committedIDs))
	for _, id := range committedIDs {
		snapshot.CommittedFactors = append(snapshot.CommittedFactors, cloneOnePatchFactors(p.committed[id]))
	}
	snapshot.CommittedDisplacement = cloneFloatMap(p.committedDisp)

	planIDs := make([]string, 0, len(p.committedPlanIDs))
	for id := range p.committedPlanIDs {
		planIDs = append(planIDs, id)
	}
	sort.Strings(planIDs)
	snapshot.CommittedPlanIDs = planIDs

	if p.pending != nil {
		snapshot.Pending = &snapshotPending{
			ID:            p.pending.id,
			FrameID:       p.pending.frameID,
			Factors:       clonePatchFactors(p.pending.factors),
			Displacements: cloneFloatMap(p.pending.displacements),
		}
	}
	p.mu.RUnlock()

	payload, err := encodeGob(snapshot)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Payload:  payload,
		Checksum: checksum,
	}
	return encodeGob(envelope)
}

// Restore replaces the planner state with a snapshot, all or nothing.
func (p *TessellationFactorPlanner) Restore(data []byte) error {
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	limits := normalizeLimits(snapshot.Limits)
	patches := make(map[string]*patchState, len(snapshot.Patches))
	for _, patch := range snapshot.Patches {
		patches[patch.ID] = &patchState{
			id:           patch.ID,
			edges:        append([]Edge(nil), patch.Edges...),
			displacement: patch.Displacement,
			ratio:        patch.Ratio,
		}
	}

	committed := make(map[string]PatchFactors, len(snapshot.CommittedFactors))
	for _, factors := range snapshot.CommittedFactors {
		committed[factors.PatchID] = cloneOnePatchFactors(factors)
	}
	committedDisp := cloneFloatMap(snapshot.CommittedDisplacement)

	committedPlanIDs := make(map[string]struct{}, len(snapshot.CommittedPlanIDs))
	for _, id := range snapshot.CommittedPlanIDs {
		committedPlanIDs[id] = struct{}{}
	}

	var pending *pendingPlan
	if snapshot.Pending != nil {
		pending = &pendingPlan{
			id:            snapshot.Pending.ID,
			frameID:       snapshot.Pending.FrameID,
			factors:       clonePatchFactors(snapshot.Pending.Factors),
			displacements: cloneFloatMap(snapshot.Pending.Displacements),
		}
	}

	stats := snapshot.Stats
	stats.PatchCount = len(patches)
	stats.CommittedFrameID = snapshot.CommittedFrameID

	p.mu.Lock()
	p.limits = limits
	p.patches = patches
	p.committed = committed
	p.committedDisp = committedDisp
	p.committedPlanIDs = committedPlanIDs
	p.pending = pending
	p.stats = stats
	p.mu.Unlock()
	return nil
}

const (
	snapshotMagic   = "gsb/batch50-go-game-b-01/tessellation-factor-planner"
	snapshotVersion = 1
)

type snapshotEnvelope struct {
	Magic    string
	Version  int
	Payload  []byte
	Checksum [32]byte
}

type snapshotData struct {
	Magic                 string
	Version               int
	Limits                Limits
	Patches               []snapshotPatch
	CommittedFactors      []PatchFactors
	CommittedDisplacement map[string]float64
	CommittedPlanIDs      []string
	Pending               *snapshotPending
	Stats                 PlanStats
	CommittedFrameID      int64
}

type snapshotPatch struct {
	ID           string
	Edges        []Edge
	Displacement float64
	Ratio        float64
}

type snapshotPending struct {
	ID            string
	FrameID       int64
	Factors       []PatchFactors
	Displacements map[string]float64
}

func normalizeLimits(limits Limits) Limits {
	if limits.MaxPatches <= 0 {
		limits.MaxPatches = DefaultMaxPatches
	}
	if limits.MaxEdgeFactor <= 0 {
		limits.MaxEdgeFactor = DefaultMaxEdgeFactor
	}

	power := 1
	for power <= limits.MaxEdgeFactor/2 {
		power *= 2
	}
	limits.MaxEdgeFactor = power
	return limits
}

func validateView(view ViewParams) error {
	if !validPositiveFloat(view.CameraDistance) ||
		!validPositiveFloat(view.PixelsPerUnit) ||
		!validPositiveFloat(view.PixelErrorBudget) {
		return ErrBadView
	}
	return nil
}

func validPositiveFloat(value float64) bool {
	return !math.IsNaN(value) && value > 0
}

func sortedPatchIDs(patches map[string]*patchState) []string {
	ids := make([]string, 0, len(patches))
	for id := range patches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func ceilPowerOfTwo(value float64, cap int) int {
	if cap < 1 || math.IsNaN(value) || value <= 1 {
		return 1
	}

	factor := 1
	for factor < cap {
		epsilon := 1e-12 * math.Max(1, float64(factor))
		if value <= float64(factor)+epsilon {
			break
		}
		factor *= 2
	}
	return factor
}

func recomputeDerivedFactors(factors *PatchFactors) {
	maxEdge := 1
	for _, edgeFactor := range factors.EdgeFactors {
		if edgeFactor > maxEdge {
			maxEdge = edgeFactor
		}
	}

	factors.InnerFactor = maxEdge / 2
	if factors.InnerFactor < 1 {
		factors.InnerFactor = 1
	}
	factors.Level = factorLevel(maxEdge)
}

func factorLevel(factor int) int {
	level := 0
	for factor > 1 {
		factor /= 2
		level++
	}
	return level
}

func powerOfTwo(level int) int {
	if level <= 0 {
		return 1
	}
	return 1 << level
}

func unifySharedEdges(ids []string, patches map[string]*patchState, planned []PatchFactors) {
	maxFactorByEdgeID := make(map[string]int)
	for i, id := range ids {
		patch := patches[id]
		for edgeIndex := range planned[i].EdgeFactors {
			edgeID := patch.edges[edgeIndex].EdgeID
			maxFactorByEdgeID[edgeID] = maxInt(maxFactorByEdgeID[edgeID], planned[i].EdgeFactors[edgeIndex])
		}
	}

	for i, id := range ids {
		patch := patches[id]
		for edgeIndex := range planned[i].EdgeFactors {
			edgeID := patch.edges[edgeIndex].EdgeID
			planned[i].EdgeFactors[edgeIndex] = maxFactorByEdgeID[edgeID]
		}
	}
}

func limitDownwardChangesOneTier(
	ids []string,
	patches map[string]*patchState,
	committed map[string]PatchFactors,
	committedDisplacement map[string]float64,
	planned []PatchFactors,
	maxEdgeFactor int,
) {
	previousByEdgeID := make(map[string]int)
	for patchID, factors := range committed {
		patch, ok := patches[patchID]
		if !ok {
			continue
		}
		for i, factor := range factors.EdgeFactors {
			if i >= len(patch.edges) {
				continue
			}
			edgeID := patch.edges[i].EdgeID
			previousByEdgeID[edgeID] = maxInt(previousByEdgeID[edgeID], factor)
		}
	}

	for i, id := range ids {
		patch := patches[id]
		displacementRemoved := patch.displacement == 0 && committedDisplacement[id] > 0
		for edgeIndex := range planned[i].EdgeFactors {
			if displacementRemoved {
				continue
			}
			edgeID := patch.edges[edgeIndex].EdgeID
			previous, ok := previousByEdgeID[edgeID]
			if !ok {
				continue
			}

			target := planned[i].EdgeFactors[edgeIndex]
			previousLevel := factorLevel(previous)
			targetLevel := factorLevel(target)
			if previousLevel > 0 && targetLevel < previousLevel-1 {
				target = powerOfTwo(previousLevel - 1)
			}
			if target > maxEdgeFactor {
				target = maxEdgeFactor
			}
			planned[i].EdgeFactors[edgeIndex] = target
		}
	}
}

func fitFactorBudget(planned []PatchFactors, patches map[string]*patchState, maxFactorSum int) (int, error) {
	if maxFactorSum <= 0 {
		return 0, nil
	}

	rounds := 0
	for factorSum(planned) > maxFactorSum {
		victim := -1
		for i := range planned {
			if maxEdgeFactorOf(planned[i]) <= 1 {
				continue
			}
			if victim == -1 || patches[planned[i].PatchID].ratio < patches[planned[victim].PatchID].ratio {
				victim = i
			}
		}

		if victim == -1 {
			return rounds, ErrBudgetExceeded
		}

		tier := maxEdgeFactorOf(planned[victim])
		for i := range planned {
			for edgeIndex, factor := range planned[i].EdgeFactors {
				if factor == tier {
					planned[i].EdgeFactors[edgeIndex] = factor / 2
				}
			}
			recomputeDerivedFactors(&planned[i])
		}
		rounds++
	}

	return rounds, nil
}

func factorSum(factors []PatchFactors) int {
	total := 0
	for _, factors := range factors {
		for _, edgeFactor := range factors.EdgeFactors {
			total += edgeFactor
		}
		total += factors.InnerFactor
	}
	return total
}

func maxEdgeFactorOf(factors PatchFactors) int {
	maxEdge := 1
	for _, factor := range factors.EdgeFactors {
		maxEdge = maxInt(maxEdge, factor)
	}
	return maxEdge
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clonePatchFactors(in []PatchFactors) []PatchFactors {
	if in == nil {
		return nil
	}
	out := make([]PatchFactors, len(in))
	for i := range in {
		out[i] = cloneOnePatchFactors(in[i])
	}
	return out
}

func cloneOnePatchFactors(in PatchFactors) PatchFactors {
	in.EdgeFactors = append([]int(nil), in.EdgeFactors...)
	return in
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func encodeGob(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func decodeSnapshot(data []byte) (*snapshotData, error) {
	var envelope snapshotEnvelope
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("%w: invalid envelope", ErrBadSnapshot)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}

	checksum := sha256.Sum256(envelope.Payload)
	if checksum != envelope.Checksum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var snapshot snapshotData
	if err := gob.NewDecoder(bytes.NewReader(envelope.Payload)).Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("%w: invalid payload", ErrBadSnapshot)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Magic != snapshotMagic || snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported payload version", ErrBadSnapshot)
	}

	seenPatches := make(map[string]struct{}, len(snapshot.Patches))
	for i := range snapshot.Patches {
		patch := &snapshot.Patches[i]
		if strings.TrimSpace(patch.ID) == "" {
			return fmt.Errorf("%w: empty patch id", ErrBadSnapshot)
		}
		if _, exists := seenPatches[patch.ID]; exists {
			return fmt.Errorf("%w: duplicate patch", ErrBadSnapshot)
		}
		seenPatches[patch.ID] = struct{}{}

		if len(patch.Edges) == 0 {
			return fmt.Errorf("%w: empty edge list", ErrBadSnapshot)
		}
		for _, edge := range patch.Edges {
			if strings.TrimSpace(edge.EdgeID) == "" || math.IsNaN(edge.Length) || edge.Length <= 0 {
				return fmt.Errorf("%w: invalid edge", ErrBadSnapshot)
			}
		}
		if math.IsNaN(patch.Displacement) || patch.Displacement < 0 {
			return fmt.Errorf("%w: invalid displacement", ErrBadSnapshot)
		}
		if math.IsNaN(patch.Ratio) || patch.Ratio < 0 || patch.Ratio > 1 {
			return fmt.Errorf("%w: invalid ratio", ErrBadSnapshot)
		}
	}
	for patchID, height := range snapshot.CommittedDisplacement {
		if _, ok := seenPatches[patchID]; !ok || math.IsNaN(height) || height < 0 {
			return fmt.Errorf("%w: invalid committed displacement", ErrBadSnapshot)
		}
	}

	seenFactorPatches := make(map[string]struct{})
	validateFactors := func(factors *PatchFactors) error {
		patchIndex, ok := findSnapshotPatch(snapshot.Patches, factors.PatchID)
		if !ok {
			return fmt.Errorf("%w: unknown factor patch", ErrBadSnapshot)
		}
		if len(factors.EdgeFactors) != len(snapshot.Patches[patchIndex].Edges) {
			return fmt.Errorf("%w: edge factor count mismatch", ErrBadSnapshot)
		}

		maxEdge := 1
		for _, factor := range factors.EdgeFactors {
			if factor < 1 || !isPowerOfTwo(factor) {
				return fmt.Errorf("%w: invalid edge factor", ErrBadSnapshot)
			}
			maxEdge = maxInt(maxEdge, factor)
		}
		expectedInner := maxEdge / 2
		if expectedInner < 1 {
			expectedInner = 1
		}
		if factors.InnerFactor != expectedInner || factors.Level != factorLevel(maxEdge) {
			return fmt.Errorf("%w: inconsistent derived factor", ErrBadSnapshot)
		}
		return nil
	}

	for _, factors := range snapshot.CommittedFactors {
		if _, duplicate := seenFactorPatches[factors.PatchID]; duplicate {
			return fmt.Errorf("%w: duplicate committed factors", ErrBadSnapshot)
		}
		seenFactorPatches[factors.PatchID] = struct{}{}
		if err := validateFactors(&factors); err != nil {
			return err
		}
	}

	if snapshot.Pending != nil {
		if strings.TrimSpace(snapshot.Pending.ID) == "" {
			return fmt.Errorf("%w: empty plan id", ErrBadSnapshot)
		}
		for patchID, height := range snapshot.Pending.Displacements {
			if _, ok := seenPatches[patchID]; !ok || math.IsNaN(height) || height < 0 {
				return fmt.Errorf("%w: invalid pending displacement", ErrBadSnapshot)
			}
		}
		pendingPatches := make(map[string]struct{})
		for i := range snapshot.Pending.Factors {
			factors := &snapshot.Pending.Factors[i]
			if _, duplicate := pendingPatches[factors.PatchID]; duplicate {
				return fmt.Errorf("%w: duplicate pending factors", ErrBadSnapshot)
			}
			pendingPatches[factors.PatchID] = struct{}{}
			if err := validateFactors(factors); err != nil {
				return err
			}
		}
	}

	if snapshot.Stats.Plans < 0 || snapshot.Stats.Commits < 0 ||
		snapshot.Stats.Cancels < 0 || snapshot.Stats.DegradeRounds < 0 {
		return fmt.Errorf("%w: negative counter", ErrBadSnapshot)
	}
	for _, id := range snapshot.CommittedPlanIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: empty committed plan id", ErrBadSnapshot)
		}
	}
	return nil
}

func findSnapshotPatch(patches []snapshotPatch, id string) (int, bool) {
	for i := range patches {
		if patches[i].ID == id {
			return i, true
		}
	}
	return -1, false
}

func isPowerOfTwo(value int) bool {
	return value > 0 && value&(value-1) == 0
}
