// Package task solves the measurement and arrangement pass of a UI node tree
// and answers hit tests against the arranged boxes.
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

// Size modes understood by the solver.
const (
	ModeFixed   = "fixed"
	ModePercent = "percent"
	ModeFlex    = "flex"
)

// Layout directions of a node.
const (
	DirRow    = "row"
	DirColumn = "column"
)

// Caps applied when the matching limit is left at zero or below.
const (
	DefaultMaxNodes = 256
	DefaultMaxDepth = 16

	snapshotVersion = 1
)

// Errors reported by the solver.
var (
	ErrBadNode       = errors.New("node definition is invalid")
	ErrDuplicateNode = errors.New("node id is already used")
	ErrUnknownNode   = errors.New("node id is unknown")
	ErrBadSize       = errors.New("size specification is invalid")
	ErrBadViewport   = errors.New("viewport is invalid")
	ErrNodeLimit     = errors.New("node count exceeds the configured limit")
	ErrDepthLimit    = errors.New("node depth exceeds the configured limit")
	ErrNoTree        = errors.New("no layout tree has been built")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// LayoutConfig bounds the node table and the depth of the tree. Snap is the
// rounding unit applied to solved boxes, zero keeps the exact values.
type LayoutConfig struct {
	MaxNodes int
	MaxDepth int
	Snap     float64
}

// SizeSpec is the size declaration of one axis.
type SizeSpec struct {
	Mode   string
	Value  float64
	Weight int
}

// NodeSpec is one node of the declaration tree handed to Build.
type NodeSpec struct {
	ID       string
	Dir      string
	Width    SizeSpec
	Height   SizeSpec
	Padding  float64
	Gap      float64
	Clip     bool
	Children []NodeSpec
}

// Rect is an axis aligned box in viewport coordinates.
type Rect struct {
	X float64
	Y float64
	W float64
	H float64
}

// LayoutNode is one solved node.
type LayoutNode struct {
	ID       string
	ParentID string
	Depth    int
	Box      Rect
	Clipped  bool
}

// LayoutTree is the declaration tree flattened in pre-order.
type LayoutTree struct {
	RootID string
	Nodes  []LayoutNode
}

// LayoutResult is the outcome of one solve round.
type LayoutResult struct {
	RootID    string
	Nodes     []LayoutNode
	ViewportW float64
	ViewportH float64
	Solve     int64
}

// LayoutStats reports the counters kept by the solver.
type LayoutStats struct {
	Builds        int
	Solves        int
	Invalidations int
	Rejects       int
}

type uiNode struct {
	spec     NodeSpec
	parentID string
	depth    int
	childIDs []string
	box      Rect
	clipped  bool
}

type snapshotData struct {
	Version   int          `json:"version"`
	Root      *NodeSpec    `json:"root"`
	ViewportW float64      `json:"viewport_w"`
	ViewportH float64      `json:"viewport_h"`
	Nodes     []LayoutNode `json:"nodes"`
}

// UiLayoutSolver owns the declaration tree, the dirty set and the solved boxes.
type UiLayoutSolver struct {
	mu        sync.Mutex
	cfg       LayoutConfig
	maxNodes  int
	maxDepth  int
	rootID    string
	nodes     map[string]*uiNode
	order     []string
	dirty     map[string]struct{}
	viewportW float64
	viewportH float64
	solveSeq  int64
	stats     LayoutStats
}

// NewUiLayoutSolver builds a solver with the given configuration.
func NewUiLayoutSolver(cfg LayoutConfig) *UiLayoutSolver {
	maxNodes := cfg.MaxNodes
	if maxNodes <= 0 {
		maxNodes = DefaultMaxNodes
	}
	maxDepth := cfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	return &UiLayoutSolver{
		cfg:      cfg,
		maxNodes: maxNodes,
		maxDepth: maxDepth,
		dirty:    make(map[string]struct{}),
	}
}

// Build validates the declaration tree and installs it.
func (s *UiLayoutSolver) Build(root NodeSpec) (LayoutTree, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nodes, order, err := buildNodeRecords(root, s.maxNodes, s.maxDepth)
	if err != nil {
		s.stats.Rejects++
		return LayoutTree{}, err
	}

	s.rootID = root.ID
	s.nodes = nodes
	s.order = order
	s.dirty = make(map[string]struct{}, len(order))
	for _, id := range order {
		s.dirty[id] = struct{}{}
	}
	s.stats.Builds++

	return s.declarationTreeLocked(), nil
}

// SetViewport sets the viewport the root node fills.
func (s *UiLayoutSolver) SetViewport(w, h float64) error {
	if !isFinite(w) || !isFinite(h) {
		s.mu.Lock()
		s.stats.Rejects++
		s.mu.Unlock()
		return ErrBadViewport
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	viewportChanged := s.viewportW != w || s.viewportH != h
	s.viewportW = w
	s.viewportH = h
	if viewportChanged {
		s.markAllDirtyLocked()
	}
	return nil
}

// Invalidate marks one node and its subtree for the next solve.
func (s *UiLayoutSolver) Invalidate(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.nodes[nodeID]
	if !ok || rec == nil {
		s.stats.Rejects++
		return ErrUnknownNode
	}

	s.dirty[nodeID] = struct{}{}
	s.stats.Invalidations++
	return nil
}

// Solve measures and arranges the invalidated part of the tree.
func (s *UiLayoutSolver) Solve() (LayoutResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rootID == "" || s.nodes == nil {
		s.stats.Rejects++
		return LayoutResult{}, ErrNoTree
	}

	s.solveDirtyLocked()
	s.solveSeq++
	s.stats.Solves++
	s.dirty = make(map[string]struct{})
	return s.layoutResultLocked(s.solveSeq), nil
}

// HitTest reports the deepest node that contains the point.
func (s *UiLayoutSolver) HitTest(x, y float64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rootID == "" || s.nodes == nil {
		return "", false
	}
	root := s.nodes[s.rootID]
	if root == nil || !rectContains(root.box, x, y) {
		return "", false
	}

	for i := len(s.order) - 1; i >= 0; i-- {
		id := s.order[i]
		rec := s.nodes[id]
		if rec == nil || !rectContains(rec.box, x, y) {
			continue
		}
		if !s.pointPassesClipLocked(rec, x, y) {
			continue
		}
		return id, true
	}

	return s.rootID, true
}

// Node reads one solved node.
func (s *UiLayoutSolver) Node(nodeID string) (LayoutNode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.nodes[nodeID]
	if !ok || rec == nil {
		return LayoutNode{}, false
	}
	return layoutNodeFrom(rec), true
}

// Dirty lists the invalidated node ids in ascending order.
func (s *UiLayoutSolver) Dirty() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.dirty))
	for id := range s.dirty {
		if rec, ok := s.nodes[id]; ok && rec != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// SetSize replaces the size declaration of one node.
func (s *UiLayoutSolver) SetSize(nodeID string, width, height SizeSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.nodes[nodeID]
	if !ok || rec == nil {
		s.stats.Rejects++
		return ErrUnknownNode
	}
	if err := validateSizeSpec(width); err != nil {
		s.stats.Rejects++
		return fmt.Errorf("%w: width: %v", ErrBadSize, err)
	}
	if err := validateSizeSpec(height); err != nil {
		s.stats.Rejects++
		return fmt.Errorf("%w: height: %v", ErrBadSize, err)
	}

	rec.spec.Width = width
	rec.spec.Height = height
	s.dirty[nodeID] = struct{}{}
	return nil
}

// Stats returns a copy of the solver counters.
func (s *UiLayoutSolver) Stats() LayoutStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Snapshot serialises the tree, the viewport and the solved boxes.
func (s *UiLayoutSolver) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload := snapshotData{
		Version:   snapshotVersion,
		ViewportW: s.viewportW,
		ViewportH: s.viewportH,
		Nodes:     s.layoutNodesLocked(),
	}
	if s.rootID != "" && s.nodes != nil {
		root := s.specTreeLocked(s.rootID)
		payload.Root = &root
	}

	data, err := json.Marshal(&payload)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the solver state with a snapshot, all or nothing.
func (s *UiLayoutSolver) Restore(data []byte) error {
	payload, nodes, order, err := decodeSnapshot(data, s.maxNodes, s.maxDepth)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rootID = ""
	s.nodes = nodes
	s.order = order
	s.viewportW = payload.ViewportW
	s.viewportH = payload.ViewportH
	s.dirty = make(map[string]struct{}, len(order))
	for _, id := range order {
		s.dirty[id] = struct{}{}
	}
	if payload.Root != nil {
		s.rootID = payload.Root.ID
	}
	s.solveSeq = 0
	s.stats = LayoutStats{}
	return nil
}

func decodeSnapshot(data []byte, maxNodes, maxDepth int) (snapshotData, map[string]*uiNode, []string, error) {
	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, nil, nil, fmt.Errorf("%w: decode: %v", ErrBadSnapshot, err)
	}
	if payload.Version != snapshotVersion {
		return payload, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, payload.Version)
	}
	if !isFinite(payload.ViewportW) || !isFinite(payload.ViewportH) {
		return payload, nil, nil, fmt.Errorf("%w: non-finite viewport", ErrBadSnapshot)
	}
	if payload.Root == nil {
		if len(payload.Nodes) != 0 {
			return payload, nil, nil, fmt.Errorf("%w: missing root with solved nodes", ErrBadSnapshot)
		}
		return payload, nil, nil, nil
	}

	nodes, order, err := buildNodeRecords(*payload.Root, maxNodes, maxDepth)
	if err != nil {
		return payload, nil, nil, fmt.Errorf("%w: declaration tree: %v", ErrBadSnapshot, err)
	}
	if err := attachSnapshotNodes(payload.Nodes, nodes, order); err != nil {
		return payload, nil, nil, err
	}
	return payload, nodes, order, nil
}

func buildNodeRecords(root NodeSpec, maxNodes, maxDepth int) (map[string]*uiNode, []string, error) {
	nodes := make(map[string]*uiNode)
	order := make([]string, 0)

	var walk func(spec NodeSpec, parentID string, depth int) error
	walk = func(spec NodeSpec, parentID string, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("%w: id %q depth %d", ErrDepthLimit, spec.ID, depth)
		}
		if len(order)+1 > maxNodes {
			return fmt.Errorf("%w: at id %q", ErrNodeLimit, spec.ID)
		}
		if strings.TrimSpace(spec.ID) == "" {
			return fmt.Errorf("%w: empty id at depth %d", ErrBadNode, depth)
		}
		if _, exists := nodes[spec.ID]; exists {
			return fmt.Errorf("%w: id %q", ErrDuplicateNode, spec.ID)
		}
		if err := validateNodeSpec(spec); err != nil {
			return err
		}

		children := spec.Children
		spec.Children = nil

		rec := &uiNode{
			spec:     spec,
			parentID: parentID,
			depth:    depth,
			childIDs: make([]string, 0, len(children)),
			clipped:  spec.Clip,
		}
		nodes[spec.ID] = rec
		order = append(order, spec.ID)

		for _, child := range children {
			if err := walk(child, spec.ID, depth+1); err != nil {
				return err
			}
			rec.childIDs = append(rec.childIDs, child.ID)
		}
		return nil
	}

	if err := walk(root, "", 0); err != nil {
		return nil, nil, err
	}
	return nodes, order, nil
}

func validateNodeSpec(spec NodeSpec) error {
	if spec.Dir != "" && spec.Dir != DirRow && spec.Dir != DirColumn {
		return fmt.Errorf("%w: id %q has direction %q", ErrBadNode, spec.ID, spec.Dir)
	}
	if err := validateBuildSize(spec.Width); err != nil {
		return fmt.Errorf("%w: id %q width: %v", ErrBadNode, spec.ID, err)
	}
	if err := validateBuildSize(spec.Height); err != nil {
		return fmt.Errorf("%w: id %q height: %v", ErrBadNode, spec.ID, err)
	}
	if !isFinite(spec.Padding) || spec.Padding < 0 {
		return fmt.Errorf("%w: id %q has bad padding", ErrBadNode, spec.ID)
	}
	if !isFinite(spec.Gap) {
		return fmt.Errorf("%w: id %q has non-finite gap", ErrBadNode, spec.ID)
	}
	return nil
}

func validateBuildSize(size SizeSpec) error {
	if err := validateSizeSpec(size); err != nil {
		return err
	}
	if size.Mode == ModeFlex && size.Weight < 0 {
		return ErrBadNode
	}
	return nil
}

func validateSizeSpec(size SizeSpec) error {
	if size.Weight < 0 {
		return ErrBadSize
	}
	if !isFinite(size.Value) || size.Value < 0 {
		return ErrBadSize
	}
	switch size.Mode {
	case "", ModeFixed, ModeFlex:
		return nil
	case ModePercent:
		if size.Value > 100 {
			return ErrBadSize
		}
		return nil
	default:
		return ErrBadSize
	}
}

func attachSnapshotNodes(saved []LayoutNode, nodes map[string]*uiNode, order []string) error {
	if len(saved) != len(order) {
		return fmt.Errorf("%w: expected %d solved nodes, got %d", ErrBadSnapshot, len(order), len(saved))
	}

	for i, id := range order {
		layout := saved[i]
		rec := nodes[id]
		if layout.ID != id || layout.ParentID != rec.parentID || layout.Depth != rec.depth {
			return fmt.Errorf("%w: solved node %d does not match declaration tree", ErrBadSnapshot, i)
		}
		if !finiteRect(layout.Box) || layout.Box.W < 0 || layout.Box.H < 0 {
			return fmt.Errorf("%w: solved node %q has an invalid box", ErrBadSnapshot, id)
		}
		rec.box = layout.Box
		rec.clipped = layout.Clipped
	}
	return nil
}

func (s *UiLayoutSolver) solveDirtyLocked() {
	boxes := make(map[string]Rect, len(s.order))
	root := s.nodes[s.rootID]
	rootBox := Rect{
		X: 0,
		Y: 0,
		W: nonNegative(s.viewportW),
		H: nonNegative(s.viewportH),
	}
	boxes[s.rootID] = rootBox
	s.layoutChildrenLocked(root, rootBox, boxes)

	if s.cfg.Snap > 0 && isFinite(s.cfg.Snap) {
		for id, box := range boxes {
			boxes[id] = snapRect(box, s.cfg.Snap)
		}
	}

	clipped := make(map[string]bool, len(s.order))
	for _, id := range s.order {
		rec := s.nodes[id]
		clipped[id] = rec.spec.Clip || s.computeClippedLocked(rec, boxes)
	}

	scope := s.dirtyScopeLocked()
	for _, id := range s.order {
		if !scope[id] {
			continue
		}
		rec := s.nodes[id]
		rec.box = boxes[id]
		rec.clipped = clipped[id]
	}
}

func (s *UiLayoutSolver) dirtyScopeLocked() map[string]bool {
	scope := make(map[string]bool, len(s.order))
	stack := make([]string, 0, len(s.dirty))
	for id := range s.dirty {
		if _, ok := s.nodes[id]; ok {
			stack = append(stack, id)
		}
	}

	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if scope[id] {
			continue
		}
		rec := s.nodes[id]
		if rec == nil {
			continue
		}
		scope[id] = true
		stack = append(stack, rec.childIDs...)
	}
	return scope
}

func (s *UiLayoutSolver) layoutChildrenLocked(parent *uiNode, parentBox Rect, boxes map[string]Rect) {
	row := parent.spec.Dir != DirColumn
	content := contentRect(parentBox, parent.spec.Padding)
	contentMain := content.W
	contentCross := content.H
	if !row {
		contentMain, contentCross = content.H, content.W
	}

	count := len(parent.childIDs)
	mainSizes := make([]float64, count)
	crossSizes := make([]float64, count)
	weights := make([]int, count)
	flexIndexes := make([]int, 0, count)
	baseSum := 0.0

	for i, childID := range parent.childIDs {
		child := s.nodes[childID]
		mainSpec := child.spec.Width
		crossSpec := child.spec.Height
		if !row {
			mainSpec, crossSpec = child.spec.Height, child.spec.Width
		}

		crossSizes[i] = resolveCrossSize(crossSpec, contentCross)
		if mainSpec.Mode == ModeFlex {
			flexIndexes = append(flexIndexes, i)
			weights[i] = mainSpec.Weight
			continue
		}

		mainSizes[i] = resolveBaseMainSize(mainSpec, contentMain)
		baseSum += mainSizes[i]
	}

	gapTotal := 0.0
	if count > 1 {
		gapTotal = parent.spec.Gap * float64(count-1)
	}
	remaining := contentMain - gapTotal - baseSum
	distributeFlex(remaining, flexIndexes, weights, mainSizes)

	cursorMain := content.X
	cursorCross := content.Y
	if !row {
		cursorMain, cursorCross = content.Y, content.X
	}

	for i, childID := range parent.childIDs {
		var box Rect
		if row {
			box = Rect{
				X: cursorMain,
				Y: cursorCross,
				W: mainSizes[i],
				H: crossSizes[i],
			}
			cursorMain += mainSizes[i] + parent.spec.Gap
		} else {
			box = Rect{
				X: cursorCross,
				Y: cursorMain,
				W: crossSizes[i],
				H: mainSizes[i],
			}
			cursorMain += mainSizes[i] + parent.spec.Gap
		}

		boxes[childID] = box
		s.layoutChildrenLocked(s.nodes[childID], box, boxes)
	}
}

func resolveBaseMainSize(size SizeSpec, contentMain float64) float64 {
	switch size.Mode {
	case ModeFixed:
		return nonNegative(size.Value)
	case ModePercent:
		return nonNegative(contentMain * size.Value / 100)
	default:
		return 0
	}
}

func resolveCrossSize(size SizeSpec, contentCross float64) float64 {
	switch size.Mode {
	case "", ModeFlex:
		return contentCross
	case ModeFixed:
		return nonNegative(size.Value)
	case ModePercent:
		return nonNegative(contentCross * size.Value / 100)
	default:
		return 0
	}
}

func distributeFlex(remaining float64, indexes []int, weights []int, sizes []float64) {
	if len(indexes) == 0 {
		return
	}
	if remaining <= 0 {
		for _, index := range indexes {
			sizes[index] = 0
		}
		return
	}

	totalWeight := 0
	for _, index := range indexes {
		if weights[index] > 0 {
			totalWeight += weights[index]
		}
	}

	baseSum := 0.0
	for _, index := range indexes {
		if totalWeight > 0 && weights[index] > 0 {
			share := math.Floor(remaining * float64(weights[index]) / float64(totalWeight))
			sizes[index] = share
			baseSum += share
		} else {
			sizes[index] = 0
		}
	}

	leftover := remaining - baseSum
	for _, index := range indexes {
		if leftover <= 0 {
			break
		}
		add := math.Min(1, leftover)
		sizes[index] += add
		leftover -= add
	}
	if leftover > 0 {
		sizes[indexes[0]] += leftover
	}

	sum := 0.0
	for _, index := range indexes {
		sum += sizes[index]
	}
	sizes[indexes[0]] += remaining - sum
}

func contentRect(box Rect, padding float64) Rect {
	return Rect{
		X: box.X + padding,
		Y: box.Y + padding,
		W: math.Max(0, box.W-2*padding),
		H: math.Max(0, box.H-2*padding),
	}
}

func snapRect(box Rect, unit float64) Rect {
	snap := func(value float64) float64 {
		return math.Round(value/unit) * unit
	}
	return Rect{X: snap(box.X), Y: snap(box.Y), W: snap(box.W), H: snap(box.H)}
}

func (s *UiLayoutSolver) pointPassesClipLocked(rec *uiNode, x, y float64) bool {
	for parentID := rec.parentID; parentID != ""; {
		parent := s.nodes[parentID]
		if parent == nil {
			return true
		}
		if parent.spec.Clip && !rectContains(parent.box, x, y) {
			return false
		}
		parentID = parent.parentID
	}
	return true
}

func (s *UiLayoutSolver) computeClippedLocked(rec *uiNode, boxes map[string]Rect) bool {
	ownBox, ok := boxes[rec.spec.ID]
	if !ok {
		ownBox = rec.box
	}
	for parentID := rec.parentID; parentID != ""; {
		parent := s.nodes[parentID]
		if parent == nil {
			break
		}
		parentBox, ok := boxes[parentID]
		if !ok {
			parentBox = parent.box
		}
		if parent.spec.Clip && !rectContainsRect(parentBox, ownBox) {
			return true
		}
		parentID = parent.parentID
	}
	return false
}

func rectContains(r Rect, x, y float64) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

func rectContainsRect(outer, inner Rect) bool {
	return inner.X >= outer.X &&
		inner.Y >= outer.Y &&
		inner.X+inner.W <= outer.X+outer.W &&
		inner.Y+inner.H <= outer.Y+outer.H
}

func (s *UiLayoutSolver) markAllDirtyLocked() {
	if s.dirty == nil {
		s.dirty = make(map[string]struct{})
	}
	for _, id := range s.order {
		s.dirty[id] = struct{}{}
	}
}

func (s *UiLayoutSolver) declarationTreeLocked() LayoutTree {
	nodes := make([]LayoutNode, 0, len(s.order))
	for _, id := range s.order {
		nodes = append(nodes, layoutNodeFrom(s.nodes[id]))
	}
	return LayoutTree{RootID: s.rootID, Nodes: nodes}
}

func (s *UiLayoutSolver) layoutResultLocked(seq int64) LayoutResult {
	return LayoutResult{
		RootID:    s.rootID,
		Nodes:     s.layoutNodesLocked(),
		ViewportW: s.viewportW,
		ViewportH: s.viewportH,
		Solve:     seq,
	}
}

func (s *UiLayoutSolver) layoutNodesLocked() []LayoutNode {
	nodes := make([]LayoutNode, 0, len(s.order))
	for _, id := range s.order {
		nodes = append(nodes, layoutNodeFrom(s.nodes[id]))
	}
	return nodes
}

func layoutNodeFrom(rec *uiNode) LayoutNode {
	if rec == nil {
		return LayoutNode{}
	}
	return LayoutNode{
		ID:       rec.spec.ID,
		ParentID: rec.parentID,
		Depth:    rec.depth,
		Box:      rec.box,
		Clipped:  rec.clipped,
	}
}

func (s *UiLayoutSolver) specTreeLocked(id string) NodeSpec {
	rec := s.nodes[id]
	spec := rec.spec
	spec.Children = make([]NodeSpec, 0, len(rec.childIDs))
	for _, childID := range rec.childIDs {
		spec.Children = append(spec.Children, s.specTreeLocked(childID))
	}
	return spec
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func nonNegative(value float64) float64 {
	if math.IsNaN(value) || value <= 0 {
		return 0
	}
	return value
}

func finiteRect(rect Rect) bool {
	return isFinite(rect.X) && isFinite(rect.Y) && isFinite(rect.W) && isFinite(rect.H)
}
