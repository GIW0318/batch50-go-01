// Package task bakes the tone mapping curve used by the post processing stack
// into lookup tables and answers point samples against the analytic curve.
package task

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"math"
	"sync"
)

// LUT formats the baker can emit.
const (
	LUTFormat1D   = "1d"
	LUTFormat3D   = "3d"
	LUTFormatHalf = "half"
)

// Sizes and issue codes used across the baker.
const (
	DefaultLUTSize    = 64
	LUTSize3D         = 17
	IssueNonMonotonic = "non_monotonic"
	IssueHardClip     = "hard_clip"
)

const (
	defaultWhitePoint = 1000
	snapshotMagic     = "gsb/batch50-go-game-b-03/tonemap-curve-baker"
	snapshotVersion   = 1
)

// Errors reported by the baker.
var (
	ErrBadCurve      = errors.New("curve parameters are invalid")
	ErrBadInput      = errors.New("sample input is not a finite number")
	ErrBadWhitePoint = errors.New("white point is outside the supported range")
	ErrBadLUTSize    = errors.New("lookup table size is outside the supported range")
	ErrBadFormat     = errors.New("lookup table format is unknown")
	ErrNonMonotonic  = errors.New("curve is not monotonic and cannot be baked")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Limits bounds the lookup table sizes the baker will produce.
type Limits struct {
	MaxLUTSize int
}

// CurveParams is the artist facing parameter set of the tone curve.
type CurveParams struct {
	Exposure float64
	Contrast float64
	Shoulder float64
	Toe      float64
}

// Range is the input interval an issue was observed on.
type Range struct {
	Lo float64
	Hi float64
}

// CurveIssue is one problem found by Validate.
type CurveIssue struct {
	Code   string
	Range  Range
	Detail string
}

// LUT is one baked lookup table. Half keeps the half float bit patterns of the
// clamped output so the GPU upload path does not have to round again.
type LUT struct {
	Format string
	Size   int
	Values []float64
	Half   []uint16
	Frame  int64
}

// BakerStats reports the counters kept by the baker.
type BakerStats struct {
	Bakes    int
	Samples  int
	Sets     int
	Rejects  int
	LastSize int
}

// TonemapCurveBaker owns the active curve, the white point and the last table.
type TonemapCurveBaker struct {
	mu sync.Mutex

	limits     Limits
	maxLUTSize int

	curve      CurveParams
	whitePoint float64
	lutSize    int

	lastLUT LUT
	hasLUT  bool
	stats   BakerStats
}

// NewTonemapCurveBaker builds a baker with the given caps.
func NewTonemapCurveBaker(limits Limits) *TonemapCurveBaker {
	maxLUTSize := limits.MaxLUTSize
	if maxLUTSize <= 0 {
		maxLUTSize = 256
	}
	lutSize := DefaultLUTSize
	if lutSize > maxLUTSize {
		lutSize = maxLUTSize
	}
	return &TonemapCurveBaker{
		limits:     limits,
		maxLUTSize: maxLUTSize,
		curve:      CurveParams{Exposure: 0, Contrast: 1, Shoulder: 2, Toe: 0.5},
		whitePoint: defaultWhitePoint,
		lutSize:    lutSize,
	}
}

// SetCurve installs a new parameter set after checking its structure.
func (b *TonemapCurveBaker) SetCurve(params CurveParams) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !validCurve(params) {
		b.stats.Rejects++
		return ErrBadCurve
	}
	b.curve = params
	b.stats.Sets++
	return nil
}

// Current reads the parameter set that is in use.
func (b *TonemapCurveBaker) Current() (CurveParams, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.curve, true
}

// SetWhitePoint records the display luminance the curve maps to.
func (b *TonemapCurveBaker) SetWhitePoint(nits float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !validWhitePoint(nits) {
		b.stats.Rejects++
		return ErrBadWhitePoint
	}
	b.whitePoint = nits
	return nil
}

// WhitePoint reads the luminance currently in use.
func (b *TonemapCurveBaker) WhitePoint() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.whitePoint
}

// SetLUTSize sets the entry count of the one dimensional tables.
func (b *TonemapCurveBaker) SetLUTSize(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if n < 1 || n > b.maxLUTSize {
		b.stats.Rejects++
		return ErrBadLUTSize
	}
	b.lutSize = n
	return nil
}

// LUTSize reads the entry count of the one dimensional tables.
func (b *TonemapCurveBaker) LUTSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lutSize
}

// Bake renders the active curve into a lookup table.
func (b *TonemapCurveBaker) Bake(format string) (LUT, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch format {
	case LUTFormat1D, LUTFormat3D, LUTFormatHalf:
	default:
		b.stats.Rejects++
		return LUT{}, ErrBadFormat
	}

	if b.curve.Contrast < 0 {
		b.stats.Rejects++
		return LUT{}, ErrNonMonotonic
	}

	var lut LUT
	switch format {
	case LUTFormat1D:
		values := b.sample1DLocked(b.lutSize)
		lut = LUT{Format: LUTFormat1D, Size: b.lutSize, Values: values}
	case LUTFormat3D:
		values := b.sample3DLocked()
		lut = LUT{Format: LUTFormat3D, Size: LUTSize3D, Values: values}
	case LUTFormatHalf:
		values := b.sample1DLocked(b.lutSize)
		half := make([]uint16, len(values))
		for i, value := range values {
			half[i] = float64ToHalf(clamp01(value))
		}
		lut = LUT{Format: LUTFormatHalf, Size: b.lutSize, Values: values, Half: half}
	}

	b.stats.Bakes++
	lut.Frame = int64(b.stats.Bakes)
	b.stats.LastSize = lut.Size
	b.lastLUT = cloneLUT(lut)
	b.hasLUT = true
	return cloneLUT(lut), nil
}

// Sample evaluates the analytic curve at one display referred input.
func (b *TonemapCurveBaker) Sample(x float64) (float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, ErrBadInput
	}
	x = clamp01(x)
	value := b.sampleLocked(x)
	b.stats.Samples++
	return value, nil
}

// Validate lists the problems the active parameters would cause.
func (b *TonemapCurveBaker) Validate() []CurveIssue {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.validateLocked()
}

// LUT returns the last table that was baked successfully.
func (b *TonemapCurveBaker) LUT() (LUT, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hasLUT {
		return LUT{}, false
	}
	return cloneLUT(b.lastLUT), true
}

// Stats returns a copy of the baker counters.
func (b *TonemapCurveBaker) Stats() BakerStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

type snapshotLUT struct {
	Format string    `json:"format"`
	Size   int       `json:"size"`
	Values []float64 `json:"values"`
	Half   []uint16  `json:"half,omitempty"`
	Frame  int64     `json:"frame"`
}

type snapshotPayload struct {
	Curve      CurveParams  `json:"curve"`
	WhitePoint float64      `json:"white_point"`
	LUTSize    int          `json:"lut_size"`
	HasLUT     bool         `json:"has_lut"`
	LastLUT    *snapshotLUT `json:"last_lut,omitempty"`
}

type snapshotEnvelope struct {
	Magic    string `json:"magic"`
	Version  int    `json:"version"`
	Payload  []byte `json:"payload"`
	Checksum string `json:"checksum"`
}

// Snapshot serialises the curve, the white point, the size and the last table.
func (b *TonemapCurveBaker) Snapshot() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	payload := snapshotPayload{
		Curve:      b.curve,
		WhitePoint: b.whitePoint,
		LUTSize:    b.lutSize,
		HasLUT:     b.hasLUT,
	}
	if b.hasLUT {
		payload.LastLUT = &snapshotLUT{
			Format: b.lastLUT.Format,
			Size:   b.lastLUT.Size,
			Values: append([]float64(nil), b.lastLUT.Values...),
			Half:   append([]uint16(nil), b.lastLUT.Half...),
			Frame:  b.lastLUT.Frame,
		}
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(payloadBytes)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Payload:  payloadBytes,
		Checksum: fmtHex(checksum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the baker state with a snapshot, all or nothing.
func (b *TonemapCurveBaker) Restore(data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	payload, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}
	if !validCurve(payload.Curve) ||
		!validWhitePoint(payload.WhitePoint) ||
		payload.LUTSize < 1 || payload.LUTSize > b.maxLUTSize {
		return ErrBadSnapshot
	}

	var lastLUT LUT
	if payload.HasLUT {
		if payload.LastLUT == nil {
			return ErrBadSnapshot
		}
		lastLUT, err = restoreLUT(*payload.LastLUT)
		if err != nil {
			return ErrBadSnapshot
		}
	} else if payload.LastLUT != nil {
		return ErrBadSnapshot
	}

	b.curve = payload.Curve
	b.whitePoint = payload.WhitePoint
	b.lutSize = payload.LUTSize
	b.lastLUT = lastLUT
	b.hasLUT = payload.HasLUT
	return nil
}

func (b *TonemapCurveBaker) sample1DLocked(size int) []float64 {
	values := make([]float64, size)
	if size == 1 {
		values[0] = b.sampleLocked(0)
		return values
	}
	for i := range values {
		x := float64(i) / float64(size-1)
		values[i] = b.sampleLocked(x)
	}
	return values
}

func (b *TonemapCurveBaker) sample3DLocked() []float64 {
	const points = LUTSize3D * LUTSize3D * LUTSize3D
	values := make([]float64, points)
	denominator := float64(3 * (LUTSize3D - 1))
	index := 0
	for r := 0; r < LUTSize3D; r++ {
		for g := 0; g < LUTSize3D; g++ {
			for blue := 0; blue < LUTSize3D; blue++ {
				x := float64(r+g+blue) / denominator
				values[index] = b.sampleLocked(x)
				index++
			}
		}
	}
	return values
}

func (b *TonemapCurveBaker) sampleLocked(x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}

	pivot := (x-0.5)*b.curve.Contrast + 0.5
	pivot = clamp01(pivot)
	v := pivot * math.Exp2(b.curve.Exposure)
	response := curveResponse(v, b.curve)

	whitePivot := clamp01((1-0.5)*b.curve.Contrast + 0.5)
	whiteResponse := curveResponse(whitePivot*math.Exp2(b.curve.Exposure), b.curve)
	if !positiveFinite(whiteResponse) {
		whiteResponse = curveResponse(math.Exp2(b.curve.Exposure), b.curve)
	}
	if !positiveFinite(whiteResponse) {
		return clamp01(response)
	}
	return clamp01(response / whiteResponse)
}

func (b *TonemapCurveBaker) validateLocked() []CurveIssue {
	issues := make([]CurveIssue, 0)
	if b.curve.Contrast < 0 {
		issues = append(issues, CurveIssue{
			Code:   IssueNonMonotonic,
			Range:  Range{Lo: 0, Hi: 1},
			Detail: "inverted contrast makes the curve decrease across its input domain",
		})
	}
	return issues
}

func curveResponse(v float64, params CurveParams) float64 {
	shoulder := 1 - math.Exp(-v/params.Shoulder)
	toe := 1 + params.Toe*math.Log1p(v)
	return shoulder * toe
}

func validCurve(params CurveParams) bool {
	return finite(params.Exposure) &&
		finite(params.Contrast) &&
		finite(params.Shoulder) &&
		finite(params.Toe) &&
		params.Shoulder > 0 && params.Shoulder <= 8 &&
		params.Toe >= 0 && params.Toe <= 8 &&
		params.Exposure >= -8 && params.Exposure <= 8 &&
		params.Contrast >= -4 && params.Contrast <= 4
}

func validWhitePoint(nits float64) bool {
	return finite(nits) && nits > 0 && nits <= 10000
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func positiveFinite(value float64) bool {
	return finite(value) && value > 0
}

func clamp01(value float64) float64 {
	if value <= 0 {
		return 0
	}
	if value >= 1 {
		return 1
	}
	return value
}

func cloneLUT(lut LUT) LUT {
	lut.Values = append([]float64(nil), lut.Values...)
	lut.Half = append([]uint16(nil), lut.Half...)
	return lut
}

func restoreLUT(data snapshotLUT) (LUT, error) {
	lut := LUT{
		Format: data.Format,
		Size:   data.Size,
		Values: append([]float64(nil), data.Values...),
		Half:   append([]uint16(nil), data.Half...),
		Frame:  data.Frame,
	}
	if lut.Size < 1 || lut.Frame < 0 {
		return LUT{}, ErrBadSnapshot
	}

	switch lut.Format {
	case LUTFormat1D:
		if len(lut.Values) != lut.Size {
			return LUT{}, ErrBadSnapshot
		}
	case LUTFormat3D:
		if lut.Size != LUTSize3D || len(lut.Values) != LUTSize3D*LUTSize3D*LUTSize3D {
			return LUT{}, ErrBadSnapshot
		}
	case LUTFormatHalf:
		if len(lut.Values) != lut.Size || len(lut.Half) != lut.Size {
			return LUT{}, ErrBadSnapshot
		}
		for _, raw := range lut.Half {
			if raw > 0x3C00 {
				return LUT{}, ErrBadSnapshot
			}
		}
	default:
		return LUT{}, ErrBadSnapshot
	}

	for _, value := range lut.Values {
		if !finite(value) || value < 0 || value > 1 {
			return LUT{}, ErrBadSnapshot
		}
	}
	return lut, nil
}

func decodeSnapshot(data []byte) (snapshotPayload, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return snapshotPayload{}, err
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion || len(envelope.Payload) == 0 {
		return snapshotPayload{}, ErrBadSnapshot
	}

	checksum := sha256.Sum256(envelope.Payload)
	if subtle.ConstantTimeCompare([]byte(envelope.Checksum), []byte(fmtHex(checksum[:]))) != 1 {
		return snapshotPayload{}, ErrBadSnapshot
	}

	var payload snapshotPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return snapshotPayload{}, err
	}
	return payload, nil
}

func fmtHex(data []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(data)*2)
	for i, value := range data {
		encoded[i*2] = digits[value>>4]
		encoded[i*2+1] = digits[value&0x0f]
	}
	return string(encoded)
}

func float64ToHalf(value float64) uint16 {
	if math.IsNaN(value) {
		return 0
	}
	if value <= 0 {
		return 0
	}
	if value >= 1 {
		return 0x3C00
	}

	bits := math.Float32bits(float32(value))
	sign := uint16((bits >> 16) & 0x8000)
	exponent := int((bits >> 23) & 0xff)
	fraction := bits & 0x7fffff
	halfExponent := exponent - 127 + 15

	if halfExponent <= 0 {
		if halfExponent < -10 {
			return sign
		}
		fraction |= 0x800000
		shift := uint32(14 - halfExponent)
		half := uint16(fraction >> shift)
		remainder := fraction & ((uint32(1) << shift) - 1)
		halfway := uint32(1) << (shift - 1)
		if remainder > halfway || (remainder == halfway && half&1 != 0) {
			half++
		}
		return sign | half
	}
	if halfExponent >= 31 {
		return sign | 0x7C00
	}

	half := sign | uint16(halfExponent<<10) | uint16(fraction>>13)
	remainder := fraction & 0x1fff
	if remainder > 0x1000 || (remainder == 0x1000 && half&1 != 0) {
		half++
	}
	return half
}
