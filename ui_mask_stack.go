// Package task maintains the clip rectangle and mask stack used while a
// 2D interface tree is drawn.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
)

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

const snapshotVersion = 1

type entryKind string

const (
	rectEntry entryKind = "rect"
	maskEntry entryKind = "mask"
)

type stackEntry struct {
	token    Token
	kind     entryKind
	before   Rect
	request  Rect
	softness float64
	charge   int
	mask     *MaskRef
}

// MaskStack owns the effective clip rectangle, the mask entries and the
// per frame feather budget.
type MaskStack struct {
	mu sync.RWMutex

	bounds        Rect
	maxDepth      int
	featherBudget int

	clip        Rect
	entries     []stackEntry
	channels    map[string]int
	tokens      map[Token]struct{}
	pushes      int
	pops        int
	featherUsed int
	tokenSerial uint64
}

type snapshotData struct {
	Version       int             `json:"version"`
	Bounds        Rect            `json:"bounds"`
	MaxDepth      int             `json:"max_depth"`
	FeatherBudget int             `json:"feather_budget"`
	Clip          Rect            `json:"clip"`
	Entries       []snapshotEntry `json:"entries"`
	Pushes        int             `json:"pushes"`
	Pops          int             `json:"pops"`
	FeatherUsed   int             `json:"feather_used"`
	TokenSerial   uint64          `json:"token_serial"`
}

type snapshotEntry struct {
	Token    string   `json:"token"`
	Kind     string   `json:"kind"`
	Before   Rect     `json:"before"`
	Rect     Rect     `json:"rect"`
	Softness float64  `json:"softness"`
	Charge   int      `json:"charge"`
	Mask     *MaskRef `json:"mask,omitempty"`
}

// NewMaskStack builds a stack that clips against bounds and enforces a depth
// limit and a per frame feather budget.
func NewMaskStack(bounds Rect, maxDepth, featherBudget int) *MaskStack {
	return &MaskStack{
		bounds:        bounds,
		maxDepth:      maxDepth,
		featherBudget: featherBudget,
		clip:          bounds,
		channels:      make(map[string]int),
		tokens:        make(map[Token]struct{}),
	}
}

// PushRect intersects a rectangle clip with the current clip and returns the
// entry token.
func (s *MaskStack) PushRect(r Rect, softness float64) (Token, error) {
	if !usableRect(r) {
		return "", ErrBadRect
	}
	if !validChargeValue(softness) {
		return "", ErrBadSoftness
	}
	charge := featherCharge(softness)

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= s.maxDepth {
		return "", ErrDepthLimit
	}
	if !s.budgetAvailableLocked(charge) {
		return "", ErrFeatherBudget
	}

	token := s.newTokenLocked()
	before := s.clip
	s.entries = append(s.entries, stackEntry{
		token:    token,
		kind:     rectEntry,
		before:   before,
		request:  r,
		softness: softness,
		charge:   charge,
	})
	s.clip = intersectRect(before, r)
	s.featherUsed += charge
	s.pushes++
	return token, nil
}

// PushMask pushes a mask layer and returns the entry token.
func (s *MaskStack) PushMask(m MaskRef) (Token, error) {
	if strings.TrimSpace(m.ID) == "" || !usableRect(m.Rect) || !validChargeValue(m.Feather) {
		return "", ErrBadMask
	}
	charge := featherCharge(m.Feather)

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= s.maxDepth {
		return "", ErrDepthLimit
	}
	if !s.budgetAvailableLocked(charge) {
		return "", ErrFeatherBudget
	}

	token := s.newTokenLocked()
	before := s.clip
	mask := m
	s.entries = append(s.entries, stackEntry{
		token:    token,
		kind:     maskEntry,
		before:   before,
		request:  m.Rect,
		softness: m.Feather,
		charge:   charge,
		mask:     &mask,
	})
	s.clip = intersectRect(before, m.Rect)
	s.featherUsed += charge
	s.channels[m.ID]++
	s.pushes++
	return token, nil
}

// Pop removes the entry that owns t. Only the current stack top is accepted.
func (s *MaskStack) Pop(t Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) == 0 {
		return ErrStackEmpty
	}
	topIndex := len(s.entries) - 1
	top := s.entries[topIndex]
	if top.token != t {
		return ErrTokenMismatch
	}

	s.featherUsed -= top.charge
	if s.featherUsed < 0 {
		s.featherUsed = 0
	}
	if top.kind == maskEntry && top.mask != nil {
		id := top.mask.ID
		s.channels[id]--
		if s.channels[id] <= 0 {
			delete(s.channels, id)
		}
	}
	delete(s.tokens, top.token)
	s.clip = top.before
	s.entries = s.entries[:topIndex]
	s.pops++
	return nil
}

// Clip reports the effective clip rectangle of the current stack state.
func (s *MaskStack) Clip() Rect {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clip
}

// MaskAt reports the mask layer at the given stack depth, outermost first.
func (s *MaskStack) MaskAt(depth int) (MaskRef, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if depth < 0 || depth >= len(s.entries) {
		return MaskRef{}, false
	}
	entry := s.entries[depth]
	if entry.kind != maskEntry || entry.mask == nil {
		return MaskRef{}, false
	}
	return *entry.mask, true
}

// Depth reports how many entries are on the stack.
func (s *MaskStack) Depth() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Reset clears the stack and restores the base clip.
func (s *MaskStack) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = nil
	s.channels = make(map[string]int)
	s.tokens = make(map[Token]struct{})
	s.clip = s.bounds
	s.pushes = 0
	s.pops = 0
	s.featherUsed = 0
	return nil
}

// FeatherUsed reports the feather pixels charged in the current frame.
func (s *MaskStack) FeatherUsed() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.featherUsed
}

// Stats reports the accounting of the stack.
func (s *MaskStack) Stats() MaskStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return MaskStats{
		Depth:        len(s.entries),
		Pushes:       s.pushes,
		Pops:         s.pops,
		Masks:        len(s.channels),
		FeatherUsed:  s.featherUsed,
		FeatherLimit: s.featherBudget,
	}
}

// Snapshot serialises the stack entries and the feather accounting.
func (s *MaskStack) Snapshot() ([]byte, error) {
	s.mu.RLock()
	dto := snapshotData{
		Version:       snapshotVersion,
		Bounds:        s.bounds,
		MaxDepth:      s.maxDepth,
		FeatherBudget: s.featherBudget,
		Clip:          s.clip,
		Entries:       make([]snapshotEntry, 0, len(s.entries)),
		Pushes:        s.pushes,
		Pops:          s.pops,
		FeatherUsed:   s.featherUsed,
		TokenSerial:   s.tokenSerial,
	}
	for _, entry := range s.entries {
		item := snapshotEntry{
			Token:    string(entry.token),
			Kind:     string(entry.kind),
			Before:   entry.before,
			Rect:     entry.request,
			Softness: entry.softness,
			Charge:   entry.charge,
		}
		if entry.mask != nil {
			mask := *entry.mask
			item.Mask = &mask
		}
		dto.Entries = append(dto.Entries, item)
	}
	s.mu.RUnlock()

	data, err := json.Marshal(&dto)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole stack state with a snapshot, all or nothing.
func (s *MaskStack) Restore(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var dto snapshotData
	if err := decoder.Decode(&dto); err != nil {
		return ErrBadSnapshot
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrBadSnapshot
	}

	restored, err := dto.build()
	if err != nil {
		return ErrBadSnapshot
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tokenSerial > restored.tokenSerial {
		restored.tokenSerial = s.tokenSerial
	}
	s.bounds = restored.bounds
	s.maxDepth = restored.maxDepth
	s.featherBudget = restored.featherBudget
	s.clip = restored.clip
	s.entries = restored.entries
	s.channels = restored.channels
	s.tokens = restored.tokens
	s.pushes = restored.pushes
	s.pops = restored.pops
	s.featherUsed = restored.featherUsed
	s.tokenSerial = restored.tokenSerial
	return nil
}

func (s *MaskStack) newTokenLocked() Token {
	for {
		s.tokenSerial++
		token := Token(fmt.Sprintf("mask-token-%d", s.tokenSerial))
		if _, exists := s.tokens[token]; exists {
			continue
		}
		s.tokens[token] = struct{}{}
		return token
	}
}

func (s *MaskStack) budgetAvailableLocked(charge int) bool {
	if charge == 0 {
		return true
	}
	if s.featherBudget < 0 || s.featherUsed < 0 || s.featherUsed > s.featherBudget {
		return false
	}
	return charge <= s.featherBudget-s.featherUsed
}

func usableRect(r Rect) bool {
	return finiteRect(r) && r.W > 0 && r.H > 0
}

func finiteRect(r Rect) bool {
	return finiteNumber(r.X) && finiteNumber(r.Y) && finiteNumber(r.W) && finiteNumber(r.H)
}

func finiteNumber(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func validChargeValue(v float64) bool {
	return finiteNumber(v) && v >= 0
}

func featherCharge(v float64) int {
	if v <= 0 {
		return 0
	}
	cost := math.Ceil(v)
	maxInt := int(^uint(0) >> 1)
	if cost >= float64(maxInt) {
		return maxInt
	}
	return int(cost)
}

func intersectRect(current, clip Rect) Rect {
	x1 := math.Max(current.X, clip.X)
	y1 := math.Max(current.Y, clip.Y)
	x2 := math.Min(current.X+current.W, clip.X+clip.W)
	y2 := math.Min(current.Y+current.H, clip.Y+clip.H)
	if x2-x1 <= 0 || y2-y1 <= 0 {
		return Rect{}
	}
	return Rect{X: x1, Y: y1, W: x2 - x1, H: y2 - y1}
}

func (d snapshotData) build() (*MaskStack, error) {
	if d.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if !finiteRect(d.Bounds) || !finiteRect(d.Clip) {
		return nil, fmt.Errorf("%w: non finite rectangle", ErrBadSnapshot)
	}
	if d.Pushes < 0 || d.Pops < 0 || d.FeatherUsed < 0 {
		return nil, fmt.Errorf("%w: negative accounting", ErrBadSnapshot)
	}
	if len(d.Entries) > 0 && d.MaxDepth < len(d.Entries) {
		return nil, fmt.Errorf("%w: depth exceeds limit", ErrBadSnapshot)
	}
	if d.Pushes < len(d.Entries) || d.Pops != d.Pushes-len(d.Entries) {
		return nil, fmt.Errorf("%w: inconsistent push/pop accounting", ErrBadSnapshot)
	}
	if d.FeatherBudget < 0 && d.FeatherUsed != 0 {
		return nil, fmt.Errorf("%w: feather usage exceeds budget", ErrBadSnapshot)
	}
	if d.FeatherBudget >= 0 && d.FeatherUsed > d.FeatherBudget {
		return nil, fmt.Errorf("%w: feather usage exceeds budget", ErrBadSnapshot)
	}

	restored := NewMaskStack(d.Bounds, d.MaxDepth, d.FeatherBudget)
	restored.tokens = make(map[Token]struct{}, len(d.Entries))
	restored.entries = make([]stackEntry, 0, len(d.Entries))
	clip := d.Bounds
	featherUsed := 0

	for index, item := range d.Entries {
		token := Token(item.Token)
		if strings.TrimSpace(item.Token) == "" {
			return nil, fmt.Errorf("%w: empty token at %d", ErrBadSnapshot, index)
		}
		if _, exists := restored.tokens[token]; exists {
			return nil, fmt.Errorf("%w: duplicate token at %d", ErrBadSnapshot, index)
		}
		if !finiteRect(item.Before) || !finiteRect(item.Rect) || !validChargeValue(item.Softness) {
			return nil, fmt.Errorf("%w: invalid entry geometry at %d", ErrBadSnapshot, index)
		}
		if item.Before != clip {
			return nil, fmt.Errorf("%w: inconsistent prior clip at %d", ErrBadSnapshot, index)
		}
		if item.Charge < 0 {
			return nil, fmt.Errorf("%w: negative charge at %d", ErrBadSnapshot, index)
		}

		entry := stackEntry{
			token:    token,
			before:   item.Before,
			request:  item.Rect,
			softness: item.Softness,
			charge:   item.Charge,
		}

		switch entryKind(item.Kind) {
		case rectEntry:
			if item.Mask != nil || !usableRect(item.Rect) {
				return nil, fmt.Errorf("%w: invalid rect entry at %d", ErrBadSnapshot, index)
			}
			if item.Charge != featherCharge(item.Softness) {
				return nil, fmt.Errorf("%w: wrong feather charge at %d", ErrBadSnapshot, index)
			}
			entry.kind = rectEntry

		case maskEntry:
			if item.Mask == nil {
				return nil, fmt.Errorf("%w: missing mask at %d", ErrBadSnapshot, index)
			}
			mask := *item.Mask
			if strings.TrimSpace(mask.ID) == "" || !usableRect(mask.Rect) || !validChargeValue(mask.Feather) {
				return nil, fmt.Errorf("%w: invalid mask at %d", ErrBadSnapshot, index)
			}
			if mask.Rect != item.Rect || mask.Feather != item.Softness {
				return nil, fmt.Errorf("%w: inconsistent mask geometry at %d", ErrBadSnapshot, index)
			}
			if item.Charge != featherCharge(mask.Feather) {
				return nil, fmt.Errorf("%w: wrong mask feather charge at %d", ErrBadSnapshot, index)
			}
			entry.kind = maskEntry
			entry.mask = &mask
			restored.channels[mask.ID]++

		default:
			return nil, fmt.Errorf("%w: unknown entry kind at %d", ErrBadSnapshot, index)
		}

		maxInt := int(^uint(0) >> 1)
		if item.Charge > maxInt-featherUsed {
			return nil, fmt.Errorf("%w: feather accounting overflow at %d", ErrBadSnapshot, index)
		}
		featherUsed += item.Charge
		clip = intersectRect(clip, item.Rect)
		restored.tokens[token] = struct{}{}
		restored.entries = append(restored.entries, entry)
	}

	if clip != d.Clip {
		return nil, fmt.Errorf("%w: inconsistent final clip", ErrBadSnapshot)
	}
	if featherUsed != d.FeatherUsed {
		return nil, fmt.Errorf("%w: inconsistent feather usage", ErrBadSnapshot)
	}

	restored.clip = clip
	restored.pushes = d.Pushes
	restored.pops = d.Pops
	restored.featherUsed = featherUsed
	restored.tokenSerial = d.TokenSerial
	return restored, nil
}
