// Package task maintains the clip rectangle and mask stack used while a
// 2D interface tree is drawn.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Token identifies one entry of the mask stack.
type Token string

// Rect is an axis aligned rectangle in interface pixels. W and H must be
// positive for a usable rectangle.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// MaskRef describes one mask layer: a rounded rectangle plus a feather width.
type MaskRef struct {
	ID      string  `json:"id"`
	Rect    Rect    `json:"rect"`
	Feather float64 `json:"feather"`
}

// MaskStats is the accounting view of the stack.
type MaskStats struct {
	Depth        int `json:"depth"`
	Pushes       int `json:"pushes"`
	Pops         int `json:"pops"`
	Masks        int `json:"masks"`
	FeatherUsed  int `json:"feather_used"`
	FeatherLimit int `json:"feather_limit"`
}

// Errors returned by the stack.
var (
	ErrBadRect       = errors.New("ui mask stack: rectangle must be non empty")
	ErrBadSoftness   = errors.New("ui mask stack: softness must not be negative")
	ErrBadMask       = errors.New("ui mask stack: mask reference is invalid")
	ErrStackEmpty    = errors.New("ui mask stack: stack is empty")
	ErrTokenMismatch = errors.New("ui mask stack: token does not match the stack top")
	ErrDepthLimit    = errors.New("ui mask stack: mask depth limit reached")
	ErrFeatherBudget = errors.New("ui mask stack: feather budget exhausted for this frame")
	ErrBadSnapshot   = errors.New("ui mask stack: snapshot payload is invalid")
)

// MaskStack owns the effective clip rectangle, the mask entries and the
// per frame feather budget.
type MaskStack struct {
	bounds        Rect
	maxDepth      int
	featherBudget int
}

// NewMaskStack builds a stack that clips against bounds and enforces a depth
// limit and a per frame feather budget.
func NewMaskStack(bounds Rect, maxDepth, featherBudget int) *MaskStack {
	return &MaskStack{bounds: bounds, maxDepth: maxDepth, featherBudget: featherBudget}
}

// PushRect intersects a rectangle clip with the current clip and returns the
// entry token.
func (s *MaskStack) PushRect(r Rect, softness float64) (Token, error) {
	return "", errNotImplemented
}

// PushMask pushes a mask layer and returns the entry token.
func (s *MaskStack) PushMask(m MaskRef) (Token, error) {
	return "", errNotImplemented
}

// Pop removes the entry that owns t. Only the current stack top is accepted.
func (s *MaskStack) Pop(t Token) error {
	return errNotImplemented
}

// Clip reports the effective clip rectangle of the current stack state.
func (s *MaskStack) Clip() Rect {
	return Rect{}
}

// MaskAt reports the mask layer at the given stack depth, outermost first.
func (s *MaskStack) MaskAt(depth int) (MaskRef, bool) {
	return MaskRef{}, false
}

// Depth reports how many entries are on the stack.
func (s *MaskStack) Depth() int {
	return 0
}

// Reset clears the stack and restores the base clip.
func (s *MaskStack) Reset() error {
	return errNotImplemented
}

// FeatherUsed reports the feather pixels charged in the current frame.
func (s *MaskStack) FeatherUsed() int {
	return 0
}

// Stats reports the accounting of the stack.
func (s *MaskStack) Stats() MaskStats {
	return MaskStats{}
}

// Snapshot serialises the stack entries and the feather accounting.
func (s *MaskStack) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole stack state with a snapshot, all or nothing.
func (s *MaskStack) Restore(data []byte) error {
	return errNotImplemented
}
