// Package task splits static meshes into lightmap charts and packs them into
// one atlas with stable texel density and pixel aligned padding.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Vec2 is a two component vector used for atlas coordinates.
type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Vec3 is a three component position.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Rect is the packed rectangle of one chart inside the atlas, in pixels.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Tri indexes three vertices of a mesh.
type Tri struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

// Mesh is the static geometry that has to be unwrapped.
type Mesh struct {
	ID    string `json:"id"`
	Verts []Vec3 `json:"verts"`
	UVs   []Vec2 `json:"uvs"`
	Tris  []Tri  `json:"tris"`
}

// ChartID identifies one lightmap chart.
type ChartID string

// Chart is one packed group of triangles.
type Chart struct {
	ID      ChartID `json:"id"`
	Tris    []int   `json:"tris"`
	Rect    Rect    `json:"rect"`
	Offset  Vec2    `json:"offset"`
	Scale   float64 `json:"scale"`
	Density float64 `json:"density"`
}

// Atlas is the packed result for one mesh.
type Atlas struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Charts []Chart `json:"charts"`
}

// Issue kinds reported by Validate.
const (
	IssueOverlap     = "overlap"
	IssueOutOfBounds = "out_of_bounds"
	IssueDensity     = "density_mismatch"
)

// Issue is one validation finding of an atlas.
type Issue struct {
	Chart  ChartID `json:"chart"`
	Kind   string  `json:"kind"`
	Detail string  `json:"detail"`
}

// UnwrapStats is the accounting view of the unwrapper.
type UnwrapStats struct {
	Charts      int `json:"charts"`
	Tris        int `json:"tris"`
	Skipped     int `json:"skipped"`
	Rebakes     int `json:"rebakes"`
	AtlasWidth  int `json:"atlas_width"`
	AtlasHeight int `json:"atlas_height"`
}

// Errors returned by the unwrapper.
var (
	ErrBadLimit       = errors.New("lightmap unwrapper: chart limit must be positive and within the configured maximum")
	ErrBadAtlasSize   = errors.New("lightmap unwrapper: atlas size must be positive")
	ErrEmptyMesh      = errors.New("lightmap unwrapper: mesh has no triangles")
	ErrBadDensity     = errors.New("lightmap unwrapper: texels per unit must be positive")
	ErrBadPadding     = errors.New("lightmap unwrapper: padding must not be negative")
	ErrBadSmoothAngle = errors.New("lightmap unwrapper: smooth angle must be within 0 and 180 degrees")
	ErrChartLimit     = errors.New("lightmap unwrapper: chart limit reached")
	ErrUnknownChart   = errors.New("lightmap unwrapper: unknown chart")
	ErrBadSnapshot    = errors.New("lightmap unwrapper: snapshot payload is invalid")
)

// LightmapUnwrapper owns the smooth angle, the padding, the chart limit and
// the last packed atlas.
type LightmapUnwrapper struct {
	maxCharts    int
	maxAtlasSize int
}

// NewLightmapUnwrapper builds an unwrapper with a hard cap on charts and on
// either atlas dimension.
func NewLightmapUnwrapper(maxCharts, maxAtlasSize int) (*LightmapUnwrapper, error) {
	return nil, errNotImplemented
}

// SetSmoothAngle sets the dihedral angle above which an edge becomes a chart
// boundary.
func (u *LightmapUnwrapper) SetSmoothAngle(deg float64) error {
	return errNotImplemented
}

// SetPadding sets the minimum pixel gap kept around every chart.
func (u *LightmapUnwrapper) SetPadding(px int) error {
	return errNotImplemented
}

// Padding reports the configured pixel gap.
func (u *LightmapUnwrapper) Padding() int {
	return 0
}

// SetChartLimit lowers the number of charts a single mesh may produce.
func (u *LightmapUnwrapper) SetChartLimit(n int) error {
	return errNotImplemented
}

// Unwrap splits the mesh into charts and packs them into an atlas.
func (u *LightmapUnwrapper) Unwrap(mesh Mesh, texelsPerUnit float64) (Atlas, error) {
	return Atlas{}, errNotImplemented
}

// Validate reports overlap, out of bounds and density issues of an atlas.
func (u *LightmapUnwrapper) Validate(atlas Atlas) []Issue {
	return nil
}

// Rebake repacks one chart and keeps the other charts in place.
func (u *LightmapUnwrapper) Rebake(chart ChartID) error {
	return errNotImplemented
}

// Charts lists the charts of the last unwrap in packing order.
func (u *LightmapUnwrapper) Charts() []Chart {
	return nil
}

// ChartDensity reports the texel density recorded for one chart.
func (u *LightmapUnwrapper) ChartDensity(chart ChartID) (float64, error) {
	return 0, errNotImplemented
}

// Stats reports the accounting of the last unwrap.
func (u *LightmapUnwrapper) Stats() UnwrapStats {
	return UnwrapStats{}
}

// Snapshot serialises the settings and the packed atlas.
func (u *LightmapUnwrapper) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (u *LightmapUnwrapper) Restore(data []byte) error {
	return errNotImplemented
}
