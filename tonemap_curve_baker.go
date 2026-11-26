// Package task bakes the tone mapping curve used by the post processing stack
// into lookup tables and answers point samples against the analytic curve.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	limits Limits
}

// NewTonemapCurveBaker builds a baker with the given caps.
func NewTonemapCurveBaker(limits Limits) *TonemapCurveBaker {
	return &TonemapCurveBaker{limits: limits}
}

// SetCurve installs a new parameter set after checking its structure.
func (b *TonemapCurveBaker) SetCurve(params CurveParams) error {
	return errNotImplemented
}

// Current reads the parameter set that is in use.
func (b *TonemapCurveBaker) Current() (CurveParams, bool) {
	return CurveParams{}, false
}

// SetWhitePoint records the display luminance the curve maps to.
func (b *TonemapCurveBaker) SetWhitePoint(nits float64) error {
	return errNotImplemented
}

// WhitePoint reads the luminance currently in use.
func (b *TonemapCurveBaker) WhitePoint() float64 {
	return 0
}

// SetLUTSize sets the entry count of the one dimensional tables.
func (b *TonemapCurveBaker) SetLUTSize(n int) error {
	return errNotImplemented
}

// LUTSize reads the entry count of the one dimensional tables.
func (b *TonemapCurveBaker) LUTSize() int {
	return 0
}

// Bake renders the active curve into a lookup table.
func (b *TonemapCurveBaker) Bake(format string) (LUT, error) {
	return LUT{}, errNotImplemented
}

// Sample evaluates the analytic curve at one display referred input.
func (b *TonemapCurveBaker) Sample(x float64) (float64, error) {
	return 0, errNotImplemented
}

// Validate lists the problems the active parameters would cause.
func (b *TonemapCurveBaker) Validate() []CurveIssue {
	return nil
}

// LUT returns the last table that was baked successfully.
func (b *TonemapCurveBaker) LUT() (LUT, bool) {
	return LUT{}, false
}

// Stats returns a copy of the baker counters.
func (b *TonemapCurveBaker) Stats() BakerStats {
	return BakerStats{}
}

// Snapshot serialises the curve, the white point, the size and the last table.
func (b *TonemapCurveBaker) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the baker state with a snapshot, all or nothing.
func (b *TonemapCurveBaker) Restore(data []byte) error {
	return errNotImplemented
}
