// Package task allocates the local split screen viewports of up to four
// players, letterboxes each viewport and versions the layout swap.
package task

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
)

// Defaults applied when the matching SplitConfig field is left unset.
const (
	DefaultMaxSlots      = 4
	DefaultMinCellW      = 320
	DefaultMinCellH      = 180
	DefaultTotalBudgetUs = 33333
	DefaultMinBudgetUs   = 2000
)

// Errors reported by the allocator.
var (
	ErrBadSlot         = errors.New("slot name is invalid")
	ErrSlotExists      = errors.New("slot already joined")
	ErrSlotLimit       = errors.New("slot limit reached")
	ErrUnknownSlot     = errors.New("slot not found")
	ErrUnknownViewport = errors.New("viewport not found")
	ErrBadAspect       = errors.New("viewport aspect is invalid")
	ErrBadWindow       = errors.New("window size is invalid")
	ErrLayoutTooSmall  = errors.New("layout would leave a cell below the minimum size")
	ErrBadBudget       = errors.New("render budget is invalid")
	ErrBudgetExceeded  = errors.New("render budget total would be exceeded")
	ErrStalePlan       = errors.New("layout plan version is stale")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// SplitConfig bounds the slots, the minimum cell size and the frame budget.
type SplitConfig struct {
	MaxSlots      int
	MinCellW      int
	MinCellH      int
	TotalBudgetUs int64
	MinBudgetUs   int64
}

// ViewportCell is one viewport rectangle plus its letterboxed content rect.
type ViewportCell struct {
	Viewport string `json:"viewport"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	ContentX int    `json:"content_x"`
	ContentY int    `json:"content_y"`
	ContentW int    `json:"content_w"`
	ContentH int    `json:"content_h"`
}

// LayoutPlan is one immutable layout version.
type LayoutPlan struct {
	Version int            `json:"version"`
	WindowW int            `json:"window_w"`
	WindowH int            `json:"window_h"`
	Cells   []ViewportCell `json:"cells"`
}

// SplitViewportAllocator owns the window size, the joined slots, the per
// viewport budgets and the pending layout version.
type SplitViewportAllocator struct {
	mu  sync.Mutex
	cfg SplitConfig

	windowW int
	windowH int

	// positions holds the slot names by seat; empty seats are reused first.
	positions []string
	aspects   map[string]float64
	budgets   map[string]int64

	active  LayoutPlan
	pending *LayoutPlan
	version int
}

// NewSplitViewportAllocator builds an allocator with the given caps.
func NewSplitViewportAllocator(cfg SplitConfig) *SplitViewportAllocator {
	if cfg.MaxSlots <= 0 {
		cfg.MaxSlots = DefaultMaxSlots
	}
	if cfg.MinCellW <= 0 {
		cfg.MinCellW = DefaultMinCellW
	}
	if cfg.MinCellH <= 0 {
		cfg.MinCellH = DefaultMinCellH
	}
	if cfg.TotalBudgetUs <= 0 {
		cfg.TotalBudgetUs = DefaultTotalBudgetUs
	}
	if cfg.MinBudgetUs <= 0 {
		cfg.MinBudgetUs = DefaultMinBudgetUs
	}
	return &SplitViewportAllocator{
		cfg:       cfg,
		positions: make([]string, cfg.MaxSlots),
		aspects:   make(map[string]float64),
		budgets:   make(map[string]int64),
		active:    LayoutPlan{Version: 1},
		version:   1,
	}
}

func validAspect(aspect float64) bool {
	return aspect > 0 && !math.IsNaN(aspect) && !math.IsInf(aspect, 0)
}

func clonePlan(plan LayoutPlan) LayoutPlan {
	out := plan
	if plan.Cells != nil {
		out.Cells = make([]ViewportCell, len(plan.Cells))
		copy(out.Cells, plan.Cells)
	}
	return out
}

// activeSlots lists the joined slot names in seat order.
func (a *SplitViewportAllocator) activeSlots() []string {
	names := make([]string, 0, len(a.positions))
	for _, name := range a.positions {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

type rect struct {
	x, y, w, h int
}

// cellRects splits the window into one rectangle per active viewport.
func cellRects(n, w, h int) []rect {
	switch n {
	case 0:
		return nil
	case 1:
		return []rect{{0, 0, w, h}}
	case 2:
		return []rect{
			{0, 0, w / 2, h},
			{w / 2, 0, w - w/2, h},
		}
	case 3:
		return []rect{
			{0, 0, w / 2, h},
			{w / 2, 0, w - w/2, h / 2},
			{w / 2, h / 2, w - w/2, h - h/2},
		}
	default:
		rows := (n + 1) / 2
		rects := make([]rect, 0, n)
		for i := 0; i < n; i++ {
			col := i % 2
			row := i / 2
			x, cw := 0, w/2
			if col == 1 {
				x, cw = w/2, w-w/2
			}
			y := row * (h / rows)
			ch := h / rows
			if row == rows-1 {
				ch = h - y
			}
			rects = append(rects, rect{x, y, cw, ch})
		}
		return rects
	}
}

// letterbox centres the content inside the cell without stretching it.
func letterbox(cell *ViewportCell, aspect float64) {
	var contentW, contentH int
	if float64(cell.W)/float64(cell.H) > aspect {
		contentH = cell.H
		contentW = int(math.Round(float64(cell.H) * aspect))
	} else {
		contentW = cell.W
		contentH = int(math.Round(float64(cell.W) / aspect))
	}
	cell.ContentW = contentW
	cell.ContentH = contentH
	cell.ContentX = cell.X + (cell.W-contentW)/2
	cell.ContentY = cell.Y + (cell.H-contentH)/2
}

// buildPlanLocked renders the layout for the current members and window.
func (a *SplitViewportAllocator) buildPlanLocked(version int) LayoutPlan {
	names := a.activeSlots()
	rects := cellRects(len(names), a.windowW, a.windowH)
	cells := make([]ViewportCell, 0, len(names))
	for i, name := range names {
		cell := ViewportCell{
			Viewport: name,
			X:        rects[i].x,
			Y:        rects[i].y,
			W:        rects[i].w,
			H:        rects[i].h,
		}
		letterbox(&cell, a.aspects[name])
		cells = append(cells, cell)
	}
	return LayoutPlan{
		Version: version,
		WindowW: a.windowW,
		WindowH: a.windowH,
		Cells:   cells,
	}
}

// fitsLocked reports whether every cell of a layout with count viewports on
// the given window stays above the configured minimum cell size.
func (a *SplitViewportAllocator) fitsLocked(count, w, h int) bool {
	for _, r := range cellRects(count, w, h) {
		if r.w < a.cfg.MinCellW || r.h < a.cfg.MinCellH {
			return false
		}
	}
	return true
}

// queueLocked replaces the pending version with a freshly built plan.
func (a *SplitViewportAllocator) queueLocked() LayoutPlan {
	a.version++
	plan := a.buildPlanLocked(a.version)
	a.pending = &plan
	return clonePlan(plan)
}

// SetWindow resizes the output window and queues a new layout version.
func (a *SplitViewportAllocator) SetWindow(w, h int) error {
	if w <= 0 || h <= 0 {
		return ErrBadWindow
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.fitsLocked(len(a.activeSlots()), w, h) {
		return ErrLayoutTooSmall
	}
	a.windowW = w
	a.windowH = h
	a.queueLocked()
	return nil
}

// Window reads the current window size.
func (a *SplitViewportAllocator) Window() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.windowW, a.windowH
}

// Join adds one player slot and returns the queued layout version.
func (a *SplitViewportAllocator) Join(slot string, aspect float64) (LayoutPlan, error) {
	if strings.TrimSpace(slot) == "" {
		return LayoutPlan{}, ErrBadSlot
	}
	if !validAspect(aspect) {
		return LayoutPlan{}, ErrBadAspect
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.aspects[slot]; ok {
		return LayoutPlan{}, ErrSlotExists
	}
	seat := -1
	for i, name := range a.positions {
		if name == "" {
			seat = i
			break
		}
	}
	if seat < 0 {
		return LayoutPlan{}, ErrSlotLimit
	}
	var used int64
	for _, budget := range a.budgets {
		used += budget
	}
	if used+a.cfg.MinBudgetUs > a.cfg.TotalBudgetUs {
		return LayoutPlan{}, ErrBudgetExceeded
	}
	if !a.fitsLocked(len(a.activeSlots())+1, a.windowW, a.windowH) {
		return LayoutPlan{}, ErrLayoutTooSmall
	}
	a.positions[seat] = slot
	a.aspects[slot] = aspect
	a.budgets[slot] = a.cfg.MinBudgetUs
	return a.queueLocked(), nil
}

// Leave drops one player slot and returns the queued layout version.
func (a *SplitViewportAllocator) Leave(slot string) (LayoutPlan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.aspects[slot]; !ok {
		return LayoutPlan{}, ErrUnknownSlot
	}
	for i, name := range a.positions {
		if name == slot {
			a.positions[i] = ""
			break
		}
	}
	delete(a.aspects, slot)
	delete(a.budgets, slot)
	return a.queueLocked(), nil
}

// Layout reads the active layout version.
func (a *SplitViewportAllocator) Layout() LayoutPlan {
	a.mu.Lock()
	defer a.mu.Unlock()
	return clonePlan(a.active)
}

// Pending reads the queued layout version, if any.
func (a *SplitViewportAllocator) Pending() (LayoutPlan, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		return LayoutPlan{}, false
	}
	return clonePlan(*a.pending), true
}

// Commit promotes the queued layout version to the active one.
func (a *SplitViewportAllocator) Commit(plan LayoutPlan) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending != nil {
		if plan.Version != a.pending.Version {
			return ErrStalePlan
		}
		a.active = *a.pending
		a.pending = nil
		return nil
	}
	if plan.Version == a.active.Version {
		return nil
	}
	return ErrStalePlan
}

// SetAspect changes the camera aspect of one viewport.
func (a *SplitViewportAllocator) SetAspect(viewport string, aspect float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.aspects[viewport]; !ok {
		return ErrUnknownViewport
	}
	if !validAspect(aspect) {
		return ErrBadAspect
	}
	a.aspects[viewport] = aspect
	a.queueLocked()
	return nil
}

// SetBudget changes the per viewport render budget in microseconds.
func (a *SplitViewportAllocator) SetBudget(viewport string, us int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.budgets[viewport]; !ok {
		return ErrUnknownViewport
	}
	if us < a.cfg.MinBudgetUs {
		return ErrBadBudget
	}
	var total int64
	for name, budget := range a.budgets {
		if name == viewport {
			total += us
		} else {
			total += budget
		}
	}
	if total > a.cfg.TotalBudgetUs {
		return ErrBudgetExceeded
	}
	a.budgets[viewport] = us
	return nil
}

// Budgets returns a copy of the per viewport render budgets.
func (a *SplitViewportAllocator) Budgets() map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int64, len(a.budgets))
	for name, budget := range a.budgets {
		out[name] = budget
	}
	return out
}

// Slots lists the joined slots in viewport order.
func (a *SplitViewportAllocator) Slots() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeSlots()
}

type snapshotSlot struct {
	Name   string  `json:"name"`
	Aspect float64 `json:"aspect"`
	Budget int64   `json:"budget"`
}

type snapshot struct {
	Version int            `json:"version"`
	WindowW int            `json:"window_w"`
	WindowH int            `json:"window_h"`
	Slots   []snapshotSlot `json:"slots"`
}

// Snapshot serialises the window, the slots, the budgets and the versions.
func (a *SplitViewportAllocator) Snapshot() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload := snapshot{
		Version: a.active.Version,
		WindowW: a.windowW,
		WindowH: a.windowH,
	}
	for _, name := range a.activeSlots() {
		payload.Slots = append(payload.Slots, snapshotSlot{
			Name:   name,
			Aspect: a.aspects[name],
			Budget: a.budgets[name],
		})
	}
	return json.Marshal(payload)
}

// Restore replaces the whole state with a snapshot payload.
func (a *SplitViewportAllocator) Restore(data []byte) error {
	var payload snapshot
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Version < 1 || payload.WindowW < 0 || payload.WindowH < 0 {
		return ErrBadSnapshot
	}
	if len(payload.Slots) > a.cfg.MaxSlots {
		return ErrBadSnapshot
	}
	positions := make([]string, a.cfg.MaxSlots)
	aspects := make(map[string]float64, len(payload.Slots))
	budgets := make(map[string]int64, len(payload.Slots))
	var total int64
	for i, slot := range payload.Slots {
		if strings.TrimSpace(slot.Name) == "" {
			return ErrBadSnapshot
		}
		if _, ok := aspects[slot.Name]; ok {
			return ErrBadSnapshot
		}
		if !validAspect(slot.Aspect) {
			return ErrBadSnapshot
		}
		if slot.Budget < a.cfg.MinBudgetUs {
			return ErrBadSnapshot
		}
		total += slot.Budget
		positions[i] = slot.Name
		aspects[slot.Name] = slot.Aspect
		budgets[slot.Name] = slot.Budget
	}
	if total > a.cfg.TotalBudgetUs {
		return ErrBadSnapshot
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.windowW = payload.WindowW
	a.windowH = payload.WindowH
	a.positions = positions
	a.aspects = aspects
	a.budgets = budgets
	a.version = payload.Version
	a.active = a.buildPlanLocked(payload.Version)
	a.pending = nil
	return nil
}
