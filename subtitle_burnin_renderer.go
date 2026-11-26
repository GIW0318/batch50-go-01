// Package task burns a subtitle track into picture frames: it decides which
// cues are visible on a given frame, resolves their style through three levels
// of inheritance, picks a usable font and keeps the per frame glyph count
// inside the render budget.
package task

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Defaults applied when a configuration field is left at zero.
const (
	DefaultMaxLayers   = 3
	DefaultGlyphBudget = 40
	MaxGlyphBudget     = 4096
	MaxLayerCount      = 16
)

// Errors reported by the renderer.
var (
	ErrBadCue      = errors.New("cue is not usable")
	ErrBadArea     = errors.New("safe area is not usable")
	ErrBadStyle    = errors.New("style sheet is not usable")
	ErrFontMissing = errors.New("no font is available for the request")
	ErrBadFont     = errors.New("font reference is not usable")
	ErrBadSnapshot = errors.New("snapshot payload is invalid")
	ErrBadFrame    = errors.New("frame is not usable")
)

// Style is one layer of the three level cascade. A zero field means the level
// does not override it.
type Style struct {
	Font   string `json:"font"`
	Size   int    `json:"size"`
	Color  string `json:"color"`
	Align  string `json:"align"`
	Margin int    `json:"margin"`
}

// StyleSheet is the global style plus the per track and per cue overrides.
type StyleSheet struct {
	Global Style            `json:"global"`
	Tracks map[string]Style `json:"tracks"`
	Cues   map[string]Style `json:"cues"`
}

// Cue is one subtitle line with its frame window.
type Cue struct {
	ID       string        `json:"id"`
	Track    string        `json:"track"`
	Layer    int           `json:"layer"`
	Priority int           `json:"priority"`
	Start    time.Duration `json:"start"`
	End      time.Duration `json:"end"`
	Text     string        `json:"text"`
}

// Rect is the safe area the burn in must stay inside.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// FontRef is a resolved font.
type FontRef struct {
	Family   string `json:"family"`
	Source   string `json:"source"`
	Fallback bool   `json:"fallback"`
}

// Frame is the picture the renderer draws into.
type Frame struct {
	PTS    time.Duration
	Width  int
	Height int
}

// RenderItem is one cue prepared for the frame.
type RenderItem struct {
	Cue      string  `json:"cue"`
	Layer    int     `json:"layer"`
	Text     string  `json:"text"`
	Style    Style   `json:"style"`
	Font     FontRef `json:"font"`
	Glyphs   int     `json:"glyphs"`
	Degraded bool    `json:"degraded"`
}

// RenderStats counts what the renderer did over its lifetime.
type RenderStats struct {
	Frames     int `json:"frames"`
	Items      int `json:"items"`
	Suppressed int `json:"suppressed"`
	Degraded   int `json:"degraded"`
	FontHits   int `json:"font_hits"`
	FontMisses int `json:"font_misses"`
}

// Config carries the layer cap, the glyph budget and the font fallback chain.
type Config struct {
	MaxLayers   int
	GlyphBudget int
	Fallback    []string
}

// SubtitleBurninRenderer owns the cue table, the style sheet and the fonts.
type SubtitleBurninRenderer struct {
	mu        sync.Mutex
	maxLayer  int
	budget    int
	fallback  []string
	sheet     StyleSheet
	cues      []Cue
	fonts     map[string]FontRef
	fontCache map[string]FontRef
	area      Rect
	glyphs    int
	stats     RenderStats
}

type rendererSnapshot struct {
	Magic      string             `json:"magic"`
	Version    int                `json:"version"`
	MaxLayers  int                `json:"max_layers"`
	GlyphLimit int                `json:"glyph_limit"`
	Fallback   []string           `json:"fallback"`
	Cues       []Cue              `json:"cues"`
	Sheet      StyleSheet         `json:"sheet"`
	Fonts      map[string]FontRef `json:"fonts"`
	FontCache  map[string]FontRef `json:"font_cache"`
	SafeArea   Rect               `json:"safe_area"`
}

const (
	snapshotMagic   = "subtitle-burnin-renderer"
	snapshotVersion = 1
)

// NewSubtitleBurninRenderer builds a renderer with the given configuration.
func NewSubtitleBurninRenderer(cfg Config) *SubtitleBurninRenderer {
	maxLayers := cfg.MaxLayers
	if maxLayers <= 0 {
		maxLayers = DefaultMaxLayers
	}

	glyphBudget := cfg.GlyphBudget
	if glyphBudget <= 0 {
		glyphBudget = DefaultGlyphBudget
	}
	if glyphBudget > MaxGlyphBudget {
		glyphBudget = MaxGlyphBudget
	}

	return &SubtitleBurninRenderer{
		maxLayer:  maxLayers,
		budget:    glyphBudget,
		fallback:  cloneStrings(cfg.Fallback),
		sheet:     cloneStyleSheet(StyleSheet{}),
		fonts:     make(map[string]FontRef),
		fontCache: make(map[string]FontRef),
	}
}

// Load installs the style sheet used for later frames. The sheet is copied
// before it is swapped in so readers never observe a partially loaded sheet.
func (r *SubtitleBurninRenderer) Load(style StyleSheet) error {
	next := cloneStyleSheet(style)
	r.mu.Lock()
	r.sheet = next
	r.mu.Unlock()
	return nil
}

// AddCue appends one cue to the track.
func (r *SubtitleBurninRenderer) AddCue(cue Cue) error {
	if !validCue(cue) {
		return ErrBadCue
	}

	r.mu.Lock()
	r.cues = append(r.cues, cue)
	r.mu.Unlock()
	return nil
}

// Cues lists the cues overlapping the given range, ordered by start time.
func (r *SubtitleBurninRenderer) Cues(from, to time.Duration) []Cue {
	if from >= to {
		return []Cue{}
	}

	r.mu.Lock()
	out := make([]Cue, 0)
	for _, cue := range r.cues {
		if cue.Start < to && cue.End > from {
			out = append(out, cue)
		}
	}
	r.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Start < out[j].Start
	})
	return out
}

// At prepares the visible cues for one frame.
func (r *SubtitleBurninRenderer) At(frame Frame) ([]RenderItem, error) {
	if frame.Width <= 0 || frame.Height <= 0 {
		return nil, ErrBadFrame
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	byLayer := make(map[int]Cue)
	suppressed := 0
	for _, cue := range r.cues {
		if frame.PTS < cue.Start || frame.PTS >= cue.End {
			continue
		}
		current, exists := byLayer[cue.Layer]
		if !exists || cue.Priority > current.Priority {
			if exists {
				suppressed++
			}
			byLayer[cue.Layer] = cue
		} else {
			suppressed++
		}
	}

	layers := make([]int, 0, len(byLayer))
	for layer := range byLayer {
		layers = append(layers, layer)
	}
	sort.Ints(layers)
	if len(layers) > r.maxLayer {
		suppressed += len(layers) - r.maxLayer
		layers = layers[:r.maxLayer]
	}

	items := make([]RenderItem, 0, len(layers))
	usedGlyphs := 0
	degraded := 0
	fontHits := 0
	fontMisses := 0
	for index, layer := range layers {
		cue := byLayer[layer]
		style := r.styleFor(cue)
		font, ok := r.resolveFontLocked(style.Font)
		if !ok {
			// Rendering can continue without a usable font reference: the
			// public resolver still reports ErrFontMissing, while a frame keeps
			// all of its cues and records the miss for diagnostics.
			fontMisses++
		} else {
			fontHits++
		}

		text := cue.Text
		glyphCount := utf8.RuneCountInString(text)
		isDegraded := false
		// The first (lowest) layer is the primary subtitle. It is never
		// shortened or dropped. Secondary layers use an ellipsis when their
		// full text would put the frame over budget.
		if index > 0 && usedGlyphs+glyphCount > r.budget {
			text = "…"
			glyphCount = 1
			isDegraded = true
			degraded++
		}
		usedGlyphs += glyphCount

		items = append(items, RenderItem{
			Cue:      cue.ID,
			Layer:    cue.Layer,
			Text:     text,
			Style:    style,
			Font:     font,
			Glyphs:   glyphCount,
			Degraded: isDegraded,
		})
	}

	r.glyphs = usedGlyphs
	r.stats.Frames++
	r.stats.Items += len(items)
	r.stats.Suppressed += suppressed
	r.stats.Degraded += degraded
	r.stats.FontHits += fontHits
	r.stats.FontMisses += fontMisses
	return items, nil
}

// SetSafeArea changes the rectangle the burn in must stay inside.
func (r *SubtitleBurninRenderer) SetSafeArea(rect Rect) error {
	if !validArea(rect) {
		return ErrBadArea
	}
	r.mu.Lock()
	r.area = rect
	r.mu.Unlock()
	return nil
}

// ResolveFont maps a requested family to an installed font.
func (r *SubtitleBurninRenderer) ResolveFont(name string) (FontRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	font, ok := r.resolveFontLocked(name)
	if !ok {
		r.stats.FontMisses++
		return FontRef{}, ErrFontMissing
	}
	r.stats.FontHits++
	return font, nil
}

// RegisterFont installs one font family.
func (r *SubtitleBurninRenderer) RegisterFont(name string, ref FontRef) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(ref.Family) == "" {
		return ErrBadFont
	}

	r.mu.Lock()
	r.fonts[name] = ref
	// A cached resolution may point at the old reference for this name, so
	// registering a font invalidates all resolved names.
	r.fontCache = make(map[string]FontRef)
	r.mu.Unlock()
	return nil
}

// ReloadFonts drops the resolved font cache after the font set changed.
func (r *SubtitleBurninRenderer) ReloadFonts() error {
	r.mu.Lock()
	r.fontCache = make(map[string]FontRef)
	r.mu.Unlock()
	return nil
}

// Budget reports the glyph count of the last frame and the budget.
func (r *SubtitleBurninRenderer) Budget() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.glyphs, r.budget
}

// Stats reads the lifetime counters.
func (r *SubtitleBurninRenderer) Stats() RenderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// Snapshot serialises the cue table, the sheet and the font registry.
func (r *SubtitleBurninRenderer) Snapshot() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := rendererSnapshot{
		Magic:      snapshotMagic,
		Version:    snapshotVersion,
		MaxLayers:  r.maxLayer,
		GlyphLimit: r.budget,
		Fallback:   cloneStrings(r.fallback),
		Cues:       append([]Cue(nil), r.cues...),
		Sheet:      cloneStyleSheet(r.sheet),
		Fonts:      cloneFontMap(r.fonts),
		FontCache:  cloneFontMap(r.fontCache),
		SafeArea:   r.area,
	}
	return json.Marshal(snapshot)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *SubtitleBurninRenderer) Restore(data []byte) error {
	var snapshot rendererSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ErrBadSnapshot
	}
	if snapshot.Magic != snapshotMagic || snapshot.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if snapshot.MaxLayers <= 0 || snapshot.GlyphLimit <= 0 || snapshot.GlyphLimit > MaxGlyphBudget {
		return ErrBadSnapshot
	}
	for _, cue := range snapshot.Cues {
		if !validCue(cue) {
			return ErrBadSnapshot
		}
	}
	if !validSnapshotArea(snapshot.SafeArea) {
		return ErrBadSnapshot
	}
	for name, font := range snapshot.Fonts {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(font.Family) == "" {
			return ErrBadSnapshot
		}
	}
	for _, font := range snapshot.FontCache {
		if strings.TrimSpace(font.Family) == "" {
			return ErrBadSnapshot
		}
	}

	r.mu.Lock()
	r.maxLayer = snapshot.MaxLayers
	r.budget = snapshot.GlyphLimit
	r.fallback = cloneStrings(snapshot.Fallback)
	r.cues = append([]Cue(nil), snapshot.Cues...)
	r.sheet = cloneStyleSheet(snapshot.Sheet)
	r.fonts = cloneFontMap(snapshot.Fonts)
	r.fontCache = cloneFontMap(snapshot.FontCache)
	r.area = snapshot.SafeArea
	r.glyphs = 0
	r.mu.Unlock()
	return nil
}

func validCue(cue Cue) bool {
	return strings.TrimSpace(cue.ID) != "" &&
		strings.TrimSpace(cue.Text) != "" &&
		cue.End > cue.Start &&
		cue.Layer >= 0
}

func validArea(rect Rect) bool {
	return rect.X >= 0 && rect.Y >= 0 && rect.W > 0 && rect.H > 0
}

func validSnapshotArea(rect Rect) bool {
	return rect == (Rect{}) || validArea(rect)
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneStyle(style Style) Style {
	return style
}

func cloneStyleSheet(sheet StyleSheet) StyleSheet {
	out := StyleSheet{
		Global: cloneStyle(sheet.Global),
		Tracks: make(map[string]Style, len(sheet.Tracks)),
		Cues:   make(map[string]Style, len(sheet.Cues)),
	}
	for name, style := range sheet.Tracks {
		out.Tracks[name] = cloneStyle(style)
	}
	for name, style := range sheet.Cues {
		out.Cues[name] = cloneStyle(style)
	}
	return out
}

func cloneFontMap(fonts map[string]FontRef) map[string]FontRef {
	out := make(map[string]FontRef, len(fonts))
	for name, ref := range fonts {
		out[name] = ref
	}
	return out
}

func mergeStyle(base, override Style) Style {
	if override.Font != "" {
		base.Font = override.Font
	}
	if override.Size != 0 {
		base.Size = override.Size
	}
	if override.Color != "" {
		base.Color = override.Color
	}
	if override.Align != "" {
		base.Align = override.Align
	}
	if override.Margin != 0 {
		base.Margin = override.Margin
	}
	return base
}

func (r *SubtitleBurninRenderer) styleFor(cue Cue) Style {
	style := cloneStyle(r.sheet.Global)
	style = mergeStyle(style, r.sheet.Tracks[cue.Track])
	style = mergeStyle(style, r.sheet.Cues[cue.ID])
	return style
}

func (r *SubtitleBurninRenderer) resolveFontLocked(name string) (FontRef, bool) {
	if ref, ok := r.fontCache[name]; ok {
		return ref, true
	}
	if ref, ok := r.fonts[name]; ok {
		ref.Fallback = false
		r.fontCache[name] = ref
		return ref, true
	}
	for _, fallback := range r.fallback {
		ref, ok := r.fonts[fallback]
		if !ok {
			continue
		}
		ref.Fallback = true
		r.fontCache[name] = ref
		return ref, true
	}
	return FontRef{}, false
}
