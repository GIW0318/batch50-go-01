// Package task splits static meshes into lightmap charts and packs them into
// one atlas with stable texel density and pixel aligned padding.
package task

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
)

const (
	defaultSmoothAngle = 60.0
	snapshotMagic      = "gsb.lightmap-unwrapper.snapshot.v1"
	angleEpsilon       = 1e-9
	geometryEpsilon    = 1e-18
)

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
	chartLimit   int
	smoothAngle  float64
	padding      int
	density      float64
	hasAtlas     bool
	atlas        Atlas
	stats        UnwrapStats
}

type snapshotState struct {
	Magic        string      `json:"magic"`
	Version      int         `json:"version"`
	Checksum     string      `json:"checksum"`
	MaxCharts    int         `json:"max_charts"`
	MaxAtlasSize int         `json:"max_atlas_size"`
	ChartLimit   int         `json:"chart_limit"`
	SmoothAngle  float64     `json:"smooth_angle"`
	Padding      int         `json:"padding"`
	Density      float64     `json:"density"`
	HasAtlas     bool        `json:"has_atlas"`
	Atlas        Atlas       `json:"atlas"`
	Stats        UnwrapStats `json:"stats"`
}

type faceInfo struct {
	triIndex int
	normal   Vec3
}

type dsu struct {
	parent []int
	size   []int
}

type vertKey [3]uint64

type edgeKey [2]vertKey

// NewLightmapUnwrapper builds an unwrapper with a hard cap on charts and on
// either atlas dimension.
func NewLightmapUnwrapper(maxCharts, maxAtlasSize int) (*LightmapUnwrapper, error) {
	if maxCharts <= 0 {
		return nil, ErrBadLimit
	}
	if maxAtlasSize <= 0 {
		return nil, ErrBadAtlasSize
	}
	return &LightmapUnwrapper{
		maxCharts:    maxCharts,
		maxAtlasSize: maxAtlasSize,
		chartLimit:   maxCharts,
		smoothAngle:  defaultSmoothAngle,
	}, nil
}

// SetSmoothAngle sets the dihedral angle above which an edge becomes a chart
// boundary.
func (u *LightmapUnwrapper) SetSmoothAngle(deg float64) error {
	if !finiteFloat(deg) || deg < 0 || deg > 180 {
		return ErrBadSmoothAngle
	}
	u.smoothAngle = deg
	return nil
}

// SetPadding sets the minimum pixel gap kept around every chart.
func (u *LightmapUnwrapper) SetPadding(px int) error {
	if px < 0 {
		return ErrBadPadding
	}
	u.padding = px
	return nil
}

// Padding reports the configured pixel gap.
func (u *LightmapUnwrapper) Padding() int {
	if u == nil {
		return 0
	}
	return u.padding
}

// SetChartLimit lowers the number of charts a single mesh may produce.
func (u *LightmapUnwrapper) SetChartLimit(n int) error {
	if n <= 0 || n > u.maxCharts {
		return ErrBadLimit
	}
	u.chartLimit = n
	return nil
}

// Unwrap splits the mesh into charts and packs them into an atlas.
func (u *LightmapUnwrapper) Unwrap(mesh Mesh, texelsPerUnit float64) (Atlas, error) {
	if !positiveFinite(texelsPerUnit) {
		return Atlas{}, ErrBadDensity
	}
	if len(mesh.Tris) == 0 {
		return Atlas{}, ErrEmptyMesh
	}

	faces, skipped := classifyFaces(mesh)
	charts := buildCharts(mesh, faces, u.smoothAngle, texelsPerUnit)
	if len(charts) > u.chartLimit {
		return Atlas{}, fmt.Errorf("%w: produced %d charts, limit is %d", ErrChartLimit, len(charts), u.chartLimit)
	}

	for i := range charts {
		w, h := chartPixelSize(mesh, charts[i], texelsPerUnit)
		charts[i].Rect = Rect{W: w, H: h}
		charts[i].Scale = 1
		charts[i].Density = texelsPerUnit
	}

	width, height, rects, err := packCharts(charts, u.padding, u.maxAtlasSize)
	if err != nil {
		return Atlas{}, err
	}
	for i := range charts {
		charts[i].Rect = rects[i]
		charts[i].Offset = Vec2{X: rects[i].X, Y: rects[i].Y}
	}

	packedTris := 0
	for i := range charts {
		packedTris += len(charts[i].Tris)
	}

	candidate := Atlas{Width: width, Height: height, Charts: charts}
	u.atlas = cloneAtlas(candidate)
	u.hasAtlas = true
	u.density = texelsPerUnit
	u.stats = UnwrapStats{
		Charts:      len(charts),
		Tris:        packedTris,
		Skipped:     skipped,
		Rebakes:     0,
		AtlasWidth:  width,
		AtlasHeight: height,
	}
	return cloneAtlas(u.atlas), nil
}

// Validate reports overlap, out of bounds and density issues of an atlas.
func (u *LightmapUnwrapper) Validate(atlas Atlas) []Issue {
	var issues []Issue
	padding := 0
	var known map[ChartID]struct{}
	requestedDensity := 0.0
	if u != nil {
		padding = u.padding
		requestedDensity = u.density
		if u.hasAtlas {
			known = make(map[ChartID]struct{}, len(u.atlas.Charts))
			for _, chart := range u.atlas.Charts {
				known[chart.ID] = struct{}{}
			}
		}
	}

	for _, chart := range atlas.Charts {
		if isOutOfBounds(chart.Rect, atlas.Width, atlas.Height) {
			issues = append(issues, Issue{
				Chart:  chart.ID,
				Kind:   IssueOutOfBounds,
				Detail: fmt.Sprintf("rect %+v is outside %dx%d", chart.Rect, atlas.Width, atlas.Height),
			})
		}
		if !positiveFinite(chart.Density) {
			issues = append(issues, Issue{
				Chart:  chart.ID,
				Kind:   IssueDensity,
				Detail: fmt.Sprintf("density %v must be positive and finite", chart.Density),
			})
		} else if _, ok := known[chart.ID]; ok && requestedDensity > 0 {
			scale := chart.Scale
			if scale == 0 {
				scale = 1
			}
			expected := requestedDensity * scale
			if scale < 0 {
				issues = append(issues, Issue{
					Chart:  chart.ID,
					Kind:   IssueDensity,
					Detail: fmt.Sprintf("scale %g must not be negative", chart.Scale),
				})
			} else if positiveFinite(expected) && !almostEqual(chart.Density, expected) {
				issues = append(issues, Issue{
					Chart: chart.ID,
					Kind:  IssueDensity,
					Detail: fmt.Sprintf(
						"density %g does not match scale %g (expected %g)",
						chart.Density, scale, expected,
					),
				})
			}
		}
	}

	for i := 0; i < len(atlas.Charts); i++ {
		for j := i + 1; j < len(atlas.Charts); j++ {
			a := atlas.Charts[i]
			b := atlas.Charts[j]
			if rectsConflict(a.Rect, b.Rect, padding) {
				issues = append(issues, Issue{
					Chart:  a.ID,
					Kind:   IssueOverlap,
					Detail: fmt.Sprintf("chart %s conflicts with %s using padding %d", a.ID, b.ID, padding),
				})
				issues = append(issues, Issue{
					Chart:  b.ID,
					Kind:   IssueOverlap,
					Detail: fmt.Sprintf("chart %s conflicts with %s using padding %d", b.ID, a.ID, padding),
				})
			}
		}
	}

	return issues
}

// Rebake repacks one chart and keeps the other charts in place.
func (u *LightmapUnwrapper) Rebake(chartID ChartID) error {
	if u == nil || !u.hasAtlas {
		return fmt.Errorf("%w: %q", ErrUnknownChart, chartID)
	}
	index := -1
	for i := range u.atlas.Charts {
		if u.atlas.Charts[i].ID == chartID {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("%w: %q", ErrUnknownChart, chartID)
	}

	target := u.atlas.Charts[index]
	if u.density > 0 {
		scale := target.Scale
		if scale == 0 {
			scale = 1
			target.Scale = 1
		}
		if scale > 0 {
			target.Density = u.density * scale
		}
	}

	others := make([]Chart, 0, len(u.atlas.Charts)-1)
	for i, chart := range u.atlas.Charts {
		if i != index {
			others = append(others, chart)
		}
	}

	currentFits := placementFits(target.Rect.X, target.Rect.Y, target.Rect.W, target.Rect.H,
		others, u.padding, u.atlas.Width, u.atlas.Height)
	if !currentFits {
		x, y, w, h, newWidth, newHeight, found := u.findRebakePlacement(target, others)
		if !found {
			return fmt.Errorf("%w: chart %q cannot be repacked within %d",
				ErrBadAtlasSize, chartID, u.maxAtlasSize)
		}
		target.Rect = Rect{X: x, Y: y, W: w, H: h}
		target.Offset = Vec2{X: x, Y: y}
		u.atlas.Width = newWidth
		u.atlas.Height = newHeight
		u.stats.AtlasWidth = newWidth
		u.stats.AtlasHeight = newHeight
	}

	u.atlas.Charts[index] = target
	u.stats.Rebakes++
	return nil
}

// Charts lists the charts of the last unwrap in packing order.
func (u *LightmapUnwrapper) Charts() []Chart {
	if u == nil || !u.hasAtlas || len(u.atlas.Charts) == 0 {
		return nil
	}
	return cloneCharts(u.atlas.Charts)
}

// ChartDensity reports the texel density recorded for one chart.
func (u *LightmapUnwrapper) ChartDensity(chartID ChartID) (float64, error) {
	if u != nil && u.hasAtlas {
		for _, chart := range u.atlas.Charts {
			if chart.ID == chartID {
				return chart.Density, nil
			}
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrUnknownChart, chartID)
}

// Stats reports the accounting of the last unwrap.
func (u *LightmapUnwrapper) Stats() UnwrapStats {
	if u == nil {
		return UnwrapStats{}
	}
	return u.stats
}

// Snapshot serialises the settings and the packed atlas.
func (u *LightmapUnwrapper) Snapshot() ([]byte, error) {
	if u == nil {
		return nil, ErrBadSnapshot
	}
	state := snapshotState{
		Magic:        snapshotMagic,
		Version:      1,
		MaxCharts:    u.maxCharts,
		MaxAtlasSize: u.maxAtlasSize,
		ChartLimit:   u.chartLimit,
		SmoothAngle:  u.smoothAngle,
		Padding:      u.padding,
		Density:      u.density,
		HasAtlas:     u.hasAtlas,
		Atlas:        cloneAtlas(u.atlas),
		Stats:        u.stats,
	}
	checksum, err := snapshotChecksum(state)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	state.Checksum = checksum
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (u *LightmapUnwrapper) Restore(data []byte) error {
	if u == nil {
		return ErrBadSnapshot
	}
	var state snapshotState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	providedChecksum := state.Checksum
	state.Checksum = ""
	expectedChecksum, err := snapshotChecksum(state)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if providedChecksum != expectedChecksum {
		return fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}
	state.Checksum = providedChecksum
	if err := validateSnapshot(state); err != nil {
		return err
	}

	u.maxCharts = state.MaxCharts
	u.maxAtlasSize = state.MaxAtlasSize
	u.chartLimit = state.ChartLimit
	u.smoothAngle = state.SmoothAngle
	u.padding = state.Padding
	u.density = state.Density
	u.hasAtlas = state.HasAtlas
	u.atlas = cloneAtlas(state.Atlas)
	u.stats = state.Stats
	return nil
}

func snapshotChecksum(state snapshotState) (string, error) {
	state.Checksum = ""
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum), nil
}

func validateSnapshot(state snapshotState) error {
	if state.Magic != snapshotMagic || state.Version != 1 {
		return fmt.Errorf("%w: unsupported snapshot format", ErrBadSnapshot)
	}
	if state.MaxCharts <= 0 || state.MaxAtlasSize <= 0 {
		return fmt.Errorf("%w: bad construction limits", ErrBadSnapshot)
	}
	if state.ChartLimit <= 0 || state.ChartLimit > state.MaxCharts {
		return fmt.Errorf("%w: bad chart limit", ErrBadSnapshot)
	}
	if !finiteFloat(state.SmoothAngle) || state.SmoothAngle < 0 || state.SmoothAngle > 180 {
		return fmt.Errorf("%w: bad smooth angle", ErrBadSnapshot)
	}
	if state.Padding < 0 {
		return fmt.Errorf("%w: bad padding", ErrBadSnapshot)
	}
	if state.Density != 0 && !positiveFinite(state.Density) {
		return fmt.Errorf("%w: bad density", ErrBadSnapshot)
	}
	if state.HasAtlas {
		if state.Atlas.Width <= 0 || state.Atlas.Height <= 0 {
			return fmt.Errorf("%w: bad atlas dimensions", ErrBadSnapshot)
		}
		if state.Atlas.Width > state.MaxAtlasSize || state.Atlas.Height > state.MaxAtlasSize {
			return fmt.Errorf("%w: atlas exceeds hard size cap", ErrBadSnapshot)
		}
		if len(state.Atlas.Charts) != state.Stats.Charts {
			return fmt.Errorf("%w: chart stats do not match atlas", ErrBadSnapshot)
		}
		if state.Stats.AtlasWidth != state.Atlas.Width || state.Stats.AtlasHeight != state.Atlas.Height {
			return fmt.Errorf("%w: atlas size stats do not match atlas", ErrBadSnapshot)
		}
		seenIDs := make(map[ChartID]struct{}, len(state.Atlas.Charts))
		for _, chart := range state.Atlas.Charts {
			if chart.ID == "" || !positiveFinite(chart.Density) {
				return fmt.Errorf("%w: bad chart %q", ErrBadSnapshot, chart.ID)
			}
			if _, exists := seenIDs[chart.ID]; exists {
				return fmt.Errorf("%w: duplicate chart %q", ErrBadSnapshot, chart.ID)
			}
			seenIDs[chart.ID] = struct{}{}
		}
	}
	return nil
}

func classifyFaces(mesh Mesh) ([]faceInfo, int) {
	faces := make([]faceInfo, 0, len(mesh.Tris))
	skipped := 0
	for triIndex, tri := range mesh.Tris {
		cross, ok := triangleCross(mesh, tri)
		if !ok {
			skipped++
			continue
		}
		edge1 := subVec3(vertexAt(mesh, tri.A), vertexAt(mesh, tri.B))
		edge2 := subVec3(vertexAt(mesh, tri.A), vertexAt(mesh, tri.C))
		edgeProduct := dotVec3(edge1, edge1) * dotVec3(edge2, edge2)
		areaSquared := dotVec3(cross, cross)
		if areaSquared == 0 || edgeProduct == 0 || areaSquared <= geometryEpsilon*edgeProduct {
			skipped++
			continue
		}
		length := math.Sqrt(areaSquared)
		faces = append(faces, faceInfo{
			triIndex: triIndex,
			normal:   scaleVec3(cross, 1/length),
		})
	}
	return faces, skipped
}

func triangleCross(mesh Mesh, tri Tri) (Vec3, bool) {
	if tri.A < 0 || tri.A >= len(mesh.Verts) ||
		tri.B < 0 || tri.B >= len(mesh.Verts) ||
		tri.C < 0 || tri.C >= len(mesh.Verts) {
		return Vec3{}, false
	}
	a := mesh.Verts[tri.A]
	b := mesh.Verts[tri.B]
	c := mesh.Verts[tri.C]
	if !finiteVec3(a) || !finiteVec3(b) || !finiteVec3(c) {
		return Vec3{}, false
	}
	return crossVec3(subVec3(b, a), subVec3(c, a)), true
}

func vertexAt(mesh Mesh, index int) Vec3 {
	if index < 0 || index >= len(mesh.Verts) {
		return Vec3{}
	}
	return mesh.Verts[index]
}

func buildCharts(mesh Mesh, faces []faceInfo, smoothAngle, density float64) []Chart {
	if len(faces) == 0 {
		return []Chart{}
	}

	union := newDSU(len(faces))
	edges := make(map[edgeKey][]int)

	for i, face := range faces {
		tri := mesh.Tris[face.triIndex]
		points := [3]Vec3{
			mesh.Verts[tri.A],
			mesh.Verts[tri.B],
			mesh.Verts[tri.C],
		}
		for edge := 0; edge < 3; edge++ {
			a := points[edge]
			b := points[(edge+1)%3]
			key := makeEdgeKey(a, b)
			for _, other := range edges[key] {
				angle := normalAngle(face.normal, faces[other].normal)
				if angle <= smoothAngle+angleEpsilon {
					union.union(i, other)
				}
			}
			edges[key] = append(edges[key], i)
		}
	}

	rootToChart := make(map[int]int)
	charts := make([]Chart, 0)
	for i := range faces {
		root := union.find(i)
		chartIndex, ok := rootToChart[root]
		if !ok {
			chartIndex = len(charts)
			rootToChart[root] = chartIndex
			charts = append(charts, Chart{
				ID:      ChartID(fmt.Sprintf("chart-%d", chartIndex+1)),
				Tris:    make([]int, 0),
				Scale:   1,
				Density: density,
			})
		}
		charts[chartIndex].Tris = append(charts[chartIndex].Tris, faces[i].triIndex)
	}

	return charts
}

func chartPixelSize(mesh Mesh, chart Chart, density float64) (float64, float64) {
	worldW, worldH := chartWorldSize(mesh, chart)
	w := math.Ceil(worldW*density - angleEpsilon)
	h := math.Ceil(worldH*density - angleEpsilon)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

func chartWorldSize(mesh Mesh, chart Chart) (float64, float64) {
	bestW, bestH := 0.0, 0.0
	bestArea := -1.0

	for axis := 0; axis < 3; axis++ {
		minU, minV := math.Inf(1), math.Inf(1)
		maxU, maxV := math.Inf(-1), math.Inf(-1)
		for _, triIndex := range chart.Tris {
			if triIndex < 0 || triIndex >= len(mesh.Tris) {
				continue
			}
			tri := mesh.Tris[triIndex]
			for _, vertexIndex := range []int{tri.A, tri.B, tri.C} {
				if vertexIndex < 0 || vertexIndex >= len(mesh.Verts) {
					continue
				}
				u, v := projectVertex(mesh.Verts[vertexIndex], axis)
				minU = math.Min(minU, u)
				maxU = math.Max(maxU, u)
				minV = math.Min(minV, v)
				maxV = math.Max(maxV, v)
			}
		}
		if math.IsInf(minU, 1) {
			continue
		}
		w := maxU - minU
		h := maxV - minV
		if w < 0 {
			w = 0
		}
		if h < 0 {
			h = 0
		}
		if area := w * h; area > bestArea {
			bestArea = area
			bestW, bestH = w, h
		}
	}

	if bestW <= angleEpsilon {
		bestW = 1
	}
	if bestH <= angleEpsilon {
		bestH = 1
	}
	return bestW, bestH
}

func projectVertex(p Vec3, normalAxis int) (float64, float64) {
	switch normalAxis {
	case 0:
		return p.Y, p.Z
	case 1:
		return p.X, p.Z
	default:
		return p.X, p.Y
	}
}

func packCharts(charts []Chart, padding, maxSize int) (int, int, []Rect, error) {
	dimensions := candidateDimensions(maxSize)
	var bestW, bestH int
	var bestRects []Rect
	bestScore := math.Inf(1)

	for _, width := range dimensions {
		for _, height := range dimensions {
			rects, ok := tryShelfPack(charts, width, height, padding)
			if !ok {
				continue
			}
			score := float64(width)*float64(height) +
				float64(maxInt(width, height))*1e-6 +
				math.Abs(float64(width-height))*1e-9
			if score < bestScore {
				bestScore = score
				bestW = width
				bestH = height
				bestRects = rects
			}
		}
	}

	if bestRects == nil {
		return 0, 0, nil, fmt.Errorf(
			"%w: %d charts need more than %dx%d with padding %d",
			ErrBadAtlasSize, len(charts), maxSize, maxSize, padding,
		)
	}
	return bestW, bestH, bestRects, nil
}

func candidateDimensions(maxSize int) []int {
	dimensions := make([]int, 0)
	size := 1
	for {
		dimensions = append(dimensions, size)
		if size > maxSize/2 {
			break
		}
		size *= 2
	}
	if dimensions[len(dimensions)-1] != maxSize {
		dimensions = append(dimensions, maxSize)
	}
	sort.Ints(dimensions)
	return dimensions
}

func tryShelfPack(charts []Chart, width, height, padding int) ([]Rect, bool) {
	rects := make([]Rect, len(charts))
	x := float64(padding)
	y := float64(padding)
	rowHeight := 0.0
	pad := float64(padding)

	for i, chart := range charts {
		w := chart.Rect.W
		h := chart.Rect.H
		if w <= 0 || h <= 0 {
			return nil, false
		}
		if x+w+pad > float64(width) {
			x = pad
			y += rowHeight + pad
			rowHeight = 0
		}
		if x+w+pad > float64(width) || y+h+pad > float64(height) {
			return nil, false
		}
		rects[i] = Rect{
			X: math.Round(x),
			Y: math.Round(y),
			W: math.Round(w),
			H: math.Round(h),
		}
		x += w + pad
		rowHeight = math.Max(rowHeight, h)
	}
	return rects, true
}

func (u *LightmapUnwrapper) findRebakePlacement(target Chart, others []Chart) (float64, float64, float64, float64, int, int, bool) {
	dimensions := candidateDimensions(u.maxAtlasSize)
	minimumWidth := maxInt(u.atlas.Width, int(math.Ceil(target.Rect.W+2*float64(u.padding))))
	minimumHeight := maxInt(u.atlas.Height, int(math.Ceil(target.Rect.H+2*float64(u.padding))))

	for _, width := range dimensions {
		if width < minimumWidth {
			continue
		}
		for _, height := range dimensions {
			if height < minimumHeight {
				continue
			}
			xs := candidateCoordinates(others, u.padding, width, true)
			ys := candidateCoordinates(others, u.padding, height, false)
			for _, y := range ys {
				for _, x := range xs {
					if placementFits(x, y, target.Rect.W, target.Rect.H, others, u.padding, width, height) {
						return x, y, target.Rect.W, target.Rect.H, width, height, true
					}
				}
			}
		}
	}
	return 0, 0, 0, 0, 0, 0, false
}

func candidateCoordinates(charts []Chart, padding, limit int, horizontal bool) []float64 {
	values := []float64{float64(padding), 0}
	pad := float64(padding)
	for _, chart := range charts {
		if horizontal {
			values = append(values, chart.Rect.X, chart.Rect.X+chart.Rect.W+pad)
		} else {
			values = append(values, chart.Rect.Y, chart.Rect.Y+chart.Rect.H+pad)
		}
	}
	sort.Float64s(values)

	unique := make([]float64, 0, len(values))
	for _, value := range values {
		if value < 0 || value > float64(limit) {
			continue
		}
		value = math.Round(value)
		if len(unique) == 0 || value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

func placementFits(x, y, w, h float64, others []Chart, padding, atlasWidth, atlasHeight int) bool {
	if x < 0 || y < 0 || w <= 0 || h <= 0 {
		return false
	}
	pad := float64(padding)
	if x+angleEpsilon < pad || y+angleEpsilon < pad {
		return false
	}
	if x+w+pad > float64(atlasWidth)+angleEpsilon || y+h+pad > float64(atlasHeight)+angleEpsilon {
		return false
	}
	candidate := Rect{X: x, Y: y, W: w, H: h}
	for _, other := range others {
		if rectsConflict(candidate, other.Rect, padding) {
			return false
		}
	}
	return true
}

func isOutOfBounds(rect Rect, width, height int) bool {
	return rect.X < -angleEpsilon ||
		rect.Y < -angleEpsilon ||
		rect.W < -angleEpsilon ||
		rect.H < -angleEpsilon ||
		rect.X+rect.W > float64(width)+angleEpsilon ||
		rect.Y+rect.H > float64(height)+angleEpsilon
}

func rectsConflict(a, b Rect, padding int) bool {
	gapX := separationGap(a.X, a.X+a.W, b.X, b.X+b.W)
	gapY := separationGap(a.Y, a.Y+a.H, b.Y, b.Y+b.H)
	required := float64(padding)
	return gapX+angleEpsilon < required && gapY+angleEpsilon < required
}

func separationGap(aMin, aMax, bMin, bMax float64) float64 {
	if aMax <= bMin {
		return bMin - aMax
	}
	if bMax <= aMin {
		return aMin - bMax
	}
	return -1
}

func newDSU(n int) *dsu {
	parent := make([]int, n)
	size := make([]int, n)
	for i := range parent {
		parent[i] = i
		size[i] = 1
	}
	return &dsu{parent: parent, size: size}
}

func (d *dsu) find(x int) int {
	for d.parent[x] != x {
		d.parent[x] = d.parent[d.parent[x]]
		x = d.parent[x]
	}
	return x
}

func (d *dsu) union(a, b int) {
	a = d.find(a)
	b = d.find(b)
	if a == b {
		return
	}
	if d.size[a] < d.size[b] {
		a, b = b, a
	}
	d.parent[b] = a
	d.size[a] += d.size[b]
}

func normalAngle(a, b Vec3) float64 {
	value := dotVec3(a, b)
	if value > 1 {
		value = 1
	}
	if value < -1 {
		value = -1
	}
	return math.Acos(value) * 180 / math.Pi
}

func subVec3(a, b Vec3) Vec3 {
	return Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}

func crossVec3(a, b Vec3) Vec3 {
	return Vec3{
		X: a.Y*b.Z - a.Z*b.Y,
		Y: a.Z*b.X - a.X*b.Z,
		Z: a.X*b.Y - a.Y*b.X,
	}
}

func dotVec3(a, b Vec3) float64 {
	return a.X*b.X + a.Y*b.Y + a.Z*b.Z
}

func scaleVec3(a Vec3, scale float64) Vec3 {
	return Vec3{X: a.X * scale, Y: a.Y * scale, Z: a.Z * scale}
}

func finiteVec3(v Vec3) bool {
	return finiteFloat(v.X) && finiteFloat(v.Y) && finiteFloat(v.Z)
}

func finiteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func positiveFinite(v float64) bool {
	return finiteFloat(v) && v > 0
}

func almostEqual(a, b float64) bool {
	diff := math.Abs(a - b)
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return diff <= 1e-9*scale
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func makeEdgeKey(a, b Vec3) edgeKey {
	ka := pointKey(a)
	kb := pointKey(b)
	if vertKeyLess(kb, ka) {
		ka, kb = kb, ka
	}
	return edgeKey{ka, kb}
}

func pointKey(p Vec3) vertKey {
	if p.X == 0 {
		p.X = 0
	}
	if p.Y == 0 {
		p.Y = 0
	}
	if p.Z == 0 {
		p.Z = 0
	}
	return vertKey{math.Float64bits(p.X), math.Float64bits(p.Y), math.Float64bits(p.Z)}
}

func vertKeyLess(a, b vertKey) bool {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func cloneAtlas(atlas Atlas) Atlas {
	atlas.Charts = cloneCharts(atlas.Charts)
	return atlas
}

func cloneCharts(charts []Chart) []Chart {
	if charts == nil {
		return nil
	}
	result := make([]Chart, len(charts))
	for i, chart := range charts {
		chart.Tris = append([]int(nil), chart.Tris...)
		result[i] = chart
	}
	return result
}
