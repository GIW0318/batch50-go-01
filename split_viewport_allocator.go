// Package task allocates the local split screen viewports of up to four
// players, letterboxes each viewport and versions the layout swap.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

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
	cfg SplitConfig
}

// NewSplitViewportAllocator builds an allocator with the given caps.
func NewSplitViewportAllocator(cfg SplitConfig) *SplitViewportAllocator {
	return &SplitViewportAllocator{cfg: cfg}
}

// SetWindow resizes the output window and queues a new layout version.
func (a *SplitViewportAllocator) SetWindow(w, h int) error {
	return errNotImplemented
}

// Window reads the current window size.
func (a *SplitViewportAllocator) Window() (int, int) {
	return 0, 0
}

// Join adds one player slot and returns the queued layout version.
func (a *SplitViewportAllocator) Join(slot string, aspect float64) (LayoutPlan, error) {
	return LayoutPlan{}, errNotImplemented
}

// Leave drops one player slot and returns the queued layout version.
func (a *SplitViewportAllocator) Leave(slot string) (LayoutPlan, error) {
	return LayoutPlan{}, errNotImplemented
}

// Layout reads the active layout version.
func (a *SplitViewportAllocator) Layout() LayoutPlan {
	return LayoutPlan{}
}

// Pending reads the queued layout version, if any.
func (a *SplitViewportAllocator) Pending() (LayoutPlan, bool) {
	return LayoutPlan{}, false
}

// Commit promotes the queued layout version to the active one.
func (a *SplitViewportAllocator) Commit(plan LayoutPlan) error {
	return errNotImplemented
}

// SetAspect changes the camera aspect of one viewport.
func (a *SplitViewportAllocator) SetAspect(viewport string, aspect float64) error {
	return errNotImplemented
}

// SetBudget changes the per viewport render budget in microseconds.
func (a *SplitViewportAllocator) SetBudget(viewport string, us int64) error {
	return errNotImplemented
}

// Budgets returns a copy of the per viewport render budgets.
func (a *SplitViewportAllocator) Budgets() map[string]int64 {
	return nil
}

// Slots lists the joined slots in viewport order.
func (a *SplitViewportAllocator) Slots() []string {
	return nil
}

// Snapshot serialises the window, the slots, the budgets and the versions.
func (a *SplitViewportAllocator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot payload.
func (a *SplitViewportAllocator) Restore(data []byte) error {
	return errNotImplemented
}
