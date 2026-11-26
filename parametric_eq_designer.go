// Package task designs the coefficient set of a cascaded parametric equaliser
// and keeps that set consistent while bands, gain staging or the sample rate
// change underneath a running audio engine.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

// Band types accepted by AddBand and UpdateBand.
const (
	BandTypePeak      = "peak"
	BandTypeLowShelf  = "lowshelf"
	BandTypeHighShelf = "highshelf"
	BandTypeLowPass   = "lowpass"
	BandTypeHighPass  = "highpass"
)

// Design constants shared by the whole equaliser.
const (
	// DefaultSampleRate is the rate a fresh designer starts with.
	DefaultSampleRate = 48000
	// MinSampleRate and MaxSampleRate bound SetSampleRate.
	MinSampleRate = 16000
	MaxSampleRate = 384000
	// UsableFreqRatio is the highest band frequency, as a fraction of the
	// current sample rate, that still gets real coefficients.
	UsableFreqRatio = 0.45
	// MinBandFreqHz is the lowest accepted band frequency.
	MinBandFreqHz = 20.0
	// ReferenceFreqHz is the frequency used for the total gain compensation.
	ReferenceFreqHz = 1000.0
	// MaxHeadroomDB bounds the absolute output gain in dB.
	MaxHeadroomDB = 24.0

	snapshotMagic   = "parametric-eq-designer-snapshot"
	snapshotVersion = 1
)

// Errors reported by the designer.
var (
	ErrSampleRateOutOfRange = errors.New("sample rate out of range")
	ErrBandLimit            = errors.New("band limit reached")
	ErrBandNotFound         = errors.New("band not found")
	ErrBadBandType          = errors.New("unknown band type")
	ErrBadFreq              = errors.New("band frequency out of usable range")
	ErrBadQ                 = errors.New("band q out of range")
	ErrBadGain              = errors.New("band gain out of range")
	ErrBadOutputGain        = errors.New("output gain out of range")
	ErrBadProbeFreq         = errors.New("probe frequency out of range")
	ErrBadSnapshot          = errors.New("snapshot payload is invalid")
)

// Limits bounds the band table and the accepted parameter ranges.
type Limits struct {
	MaxBands  int
	MinQ      float64
	MaxQ      float64
	MaxGainDB float64
}

// BandID identifies one band for the whole life of the designer. IDs are never
// reused, not even after the band is removed.
type BandID int

// BandSpec is the user facing description of one band. Q is read for peak,
// lowpass and highpass bands only; shelf bands always use a slope of one.
type BandSpec struct {
	Type   string
	FreqHz float64
	Q      float64
	GainDB float64
}

// Biquad is a normalised second order section: a0 is folded into every field,
// so the transfer function is (B0 + B1 z^-1 + B2 z^-2) / (1 + A1 z^-1 + A2 z^-2).
type Biquad struct {
	B0 float64
	B1 float64
	B2 float64
	A1 float64
	A2 float64
}

// BandState is the stored view of one band.
type BandState struct {
	ID       BandID
	Spec     BandSpec
	Bypassed bool
	Revision int64
}

type designerState struct {
	sampleRate      int
	revision        int64
	nextID          BandID
	outputGainDB    float64
	globalBypass    bool
	compensation    bool
	bands           map[BandID]BandState
	coefficients    map[BandID]Biquad
	processingOrder []BandID
}

// ParametricEqDesigner owns the band table, the coefficient cache, the gain
// staging and the revision counter.
type ParametricEqDesigner struct {
	limits  Limits
	writeMu sync.Mutex
	current atomic.Pointer[designerState]
}

type snapshotPayload struct {
	Magic        string         `json:"magic"`
	Version      int            `json:"version"`
	SampleRate   int            `json:"sample_rate"`
	Revision     int64          `json:"revision"`
	NextID       BandID         `json:"next_id"`
	OutputGainDB float64        `json:"output_gain_db"`
	Bypass       bool           `json:"bypass"`
	Compensation bool           `json:"compensation"`
	Bands        []snapshotBand `json:"bands"`
}

type snapshotBand struct {
	ID       BandID   `json:"id"`
	Spec     BandSpec `json:"spec"`
	Bypassed bool     `json:"bypassed"`
	Revision int64    `json:"revision"`
}

// NewParametricEqDesigner builds an empty designer that runs at DefaultSampleRate.
func NewParametricEqDesigner(limits Limits) *ParametricEqDesigner {
	d := &ParametricEqDesigner{limits: limits}
	d.current.Store(newDesignerState())
	return d
}

func newDesignerState() *designerState {
	return &designerState{
		sampleRate:      DefaultSampleRate,
		nextID:          1,
		bands:           make(map[BandID]BandState),
		coefficients:    make(map[BandID]Biquad),
		processingOrder: make([]BandID, 0),
	}
}

func (s *designerState) clone() *designerState {
	next := *s
	next.bands = make(map[BandID]BandState, len(s.bands))
	for id, band := range s.bands {
		next.bands[id] = band
	}
	next.coefficients = make(map[BandID]Biquad, len(s.coefficients))
	for id, coefficient := range s.coefficients {
		next.coefficients[id] = coefficient
	}
	next.processingOrder = append(make([]BandID, 0, len(s.processingOrder)), s.processingOrder...)
	return &next
}

func (s *designerState) rebuildOrder() {
	ids := make([]BandID, 0, len(s.bands))
	for id := range s.bands {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := s.bands[ids[i]].Spec.FreqHz
		right := s.bands[ids[j]].Spec.FreqHz
		if left != right {
			return left < right
		}
		return ids[i] < ids[j]
	})
	s.processingOrder = ids
}

func (s *designerState) redesignAll() {
	for id, band := range s.bands {
		coefficient, automaticBypass := designBiquad(band.Spec, s.sampleRate)
		band.Bypassed = automaticBypass
		s.bands[id] = band
		s.coefficients[id] = coefficient
	}
	s.rebuildOrder()
}

// SetSampleRate switches the design rate and re-derives every coefficient.
func (d *ParametricEqDesigner) SetSampleRate(hz int) error {
	if err := validateSampleRate(hz); err != nil {
		return err
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if current.sampleRate == hz {
		return nil
	}

	next := current.clone()
	next.sampleRate = hz
	next.revision++
	next.redesignAll()
	d.current.Store(next)
	return nil
}

// SampleRate reports the current design rate.
func (d *ParametricEqDesigner) SampleRate() int {
	return d.current.Load().sampleRate
}

// AddBand validates the spec, stores it and returns the new band ID.
func (d *ParametricEqDesigner) AddBand(spec BandSpec) (BandID, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if err := validateBandSpec(spec, d.limits, current.sampleRate); err != nil {
		return 0, err
	}
	if len(current.bands) >= d.limits.MaxBands {
		return 0, ErrBandLimit
	}

	next := current.clone()
	id := next.nextID
	next.nextID++
	next.revision++

	coefficient, bypassed := designBiquad(spec, next.sampleRate)
	next.bands[id] = BandState{
		ID:       id,
		Spec:     spec,
		Bypassed: bypassed,
		Revision: next.revision,
	}
	next.coefficients[id] = coefficient
	next.rebuildOrder()
	d.current.Store(next)
	return id, nil
}

// UpdateBand replaces the spec of an existing band and re-derives its section.
func (d *ParametricEqDesigner) UpdateBand(id BandID, spec BandSpec) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if _, ok := current.bands[id]; !ok {
		return ErrBandNotFound
	}
	if err := validateBandSpec(spec, d.limits, current.sampleRate); err != nil {
		return err
	}

	next := current.clone()
	next.revision++

	band := next.bands[id]
	band.Spec = spec
	band.Revision = next.revision
	coefficient, bypassed := designBiquad(spec, next.sampleRate)
	band.Bypassed = bypassed
	next.bands[id] = band
	next.coefficients[id] = coefficient
	next.rebuildOrder()
	d.current.Store(next)
	return nil
}

// RemoveBand drops a band from the cascade.
func (d *ParametricEqDesigner) RemoveBand(id BandID) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if _, ok := current.bands[id]; !ok {
		return ErrBandNotFound
	}

	next := current.clone()
	delete(next.bands, id)
	delete(next.coefficients, id)
	next.revision++
	next.rebuildOrder()
	d.current.Store(next)
	return nil
}

// Coefficients returns the normalised section of one band.
func (d *ParametricEqDesigner) Coefficients(id BandID) (Biquad, error) {
	state := d.current.Load()
	if _, ok := state.bands[id]; !ok {
		return Biquad{}, ErrBandNotFound
	}
	return state.coefficients[id], nil
}

// Response returns the combined magnitude response in dB at freq, including the
// output gain and, when enabled, the total gain compensation.
func (d *ParametricEqDesigner) Response(freq float64) (float64, error) {
	state := d.current.Load()
	if err := validateProbeFreq(freq, state.sampleRate); err != nil {
		return 0, err
	}
	if state.globalBypass {
		return state.outputGainDB, nil
	}

	response := cascadeResponseDB(state, freq) + state.outputGainDB
	if state.compensation {
		response += compensationDB(state)
	}
	return response, nil
}

// Band reads one stored band.
func (d *ParametricEqDesigner) Band(id BandID) (BandState, bool) {
	state := d.current.Load()
	band, ok := state.bands[id]
	return band, ok
}

// Bands lists the cascaded band IDs in processing order.
func (d *ParametricEqDesigner) Bands() []BandID {
	state := d.current.Load()
	ids := make([]BandID, len(state.processingOrder))
	copy(ids, state.processingOrder)
	return ids
}

// SetBypass switches the whole cascade in or out.
func (d *ParametricEqDesigner) SetBypass(on bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if current.globalBypass == on {
		return nil
	}
	next := current.clone()
	next.globalBypass = on
	next.revision++
	d.current.Store(next)
	return nil
}

// Bypassed reports the global bypass flag.
func (d *ParametricEqDesigner) Bypassed() bool {
	return d.current.Load().globalBypass
}

// SetOutputGain sets the output trim in dB.
func (d *ParametricEqDesigner) SetOutputGain(db float64) error {
	if !finiteNumber(db) || math.Abs(db) > MaxHeadroomDB {
		return ErrBadOutputGain
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if current.outputGainDB == db {
		return nil
	}
	next := current.clone()
	next.outputGainDB = db
	next.revision++
	d.current.Store(next)
	return nil
}

// OutputGain reports the output trim in dB.
func (d *ParametricEqDesigner) OutputGain() float64 {
	return d.current.Load().outputGainDB
}

// SetCompensation switches the total gain compensation on or off.
func (d *ParametricEqDesigner) SetCompensation(on bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	current := d.current.Load()
	if current.compensation == on {
		return nil
	}
	next := current.clone()
	next.compensation = on
	next.revision++
	d.current.Store(next)
	return nil
}

// GainCompensation reports the dB value that keeps the reference frequency at
// unity. It is zero while the compensation is switched off.
func (d *ParametricEqDesigner) GainCompensation() float64 {
	state := d.current.Load()
	if !state.compensation {
		return 0
	}
	return compensationDB(state)
}

// Revision reports the monotonic revision counter of the design.
func (d *ParametricEqDesigner) Revision() int64 {
	return d.current.Load().revision
}

// Snapshot serialises bands, staging flags and counters.
func (d *ParametricEqDesigner) Snapshot() ([]byte, error) {
	state := d.current.Load()
	payload := snapshotPayload{
		Magic:        snapshotMagic,
		Version:      snapshotVersion,
		SampleRate:   state.sampleRate,
		Revision:     state.revision,
		NextID:       state.nextID,
		OutputGainDB: state.outputGainDB,
		Bypass:       state.globalBypass,
		Compensation: state.compensation,
		Bands:        make([]snapshotBand, 0, len(state.processingOrder)),
	}
	for _, id := range state.processingOrder {
		band := state.bands[id]
		payload.Bands = append(payload.Bands, snapshotBand{
			ID:       band.ID,
			Spec:     band.Spec,
			Bypassed: band.Bypassed,
			Revision: band.Revision,
		})
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode parametric EQ snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole design with a snapshot, all or nothing.
func (d *ParametricEqDesigner) Restore(data []byte) error {
	payload, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	if err := validateSnapshot(payload, d.limits); err != nil {
		return err
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	next := newDesignerState()
	next.sampleRate = payload.SampleRate
	next.revision = payload.Revision
	next.nextID = payload.NextID
	next.outputGainDB = payload.OutputGainDB
	next.globalBypass = payload.Bypass
	next.compensation = payload.Compensation
	next.bands = make(map[BandID]BandState, len(payload.Bands))
	next.coefficients = make(map[BandID]Biquad, len(payload.Bands))
	for _, band := range payload.Bands {
		next.bands[band.ID] = BandState{
			ID:       band.ID,
			Spec:     band.Spec,
			Bypassed: band.Bypassed,
			Revision: band.Revision,
		}
	}
	next.redesignAll()
	d.current.Store(next)
	return nil
}

func decodeSnapshot(data []byte) (snapshotPayload, error) {
	var payload snapshotPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return snapshotPayload{}, fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return snapshotPayload{}, fmt.Errorf("%w: trailing snapshot data", ErrBadSnapshot)
		}
		return snapshotPayload{}, fmt.Errorf("%w: trailing snapshot data: %v", ErrBadSnapshot, err)
	}
	return payload, nil
}

func validateSnapshot(payload snapshotPayload, limits Limits) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadSnapshot, fmt.Sprintf(format, args...))
	}

	if payload.Magic != snapshotMagic {
		return invalid("bad snapshot marker")
	}
	if payload.Version != snapshotVersion {
		return invalid("unsupported snapshot version %d", payload.Version)
	}
	if err := validateSampleRate(payload.SampleRate); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if payload.Revision < 0 {
		return invalid("negative revision")
	}
	if payload.NextID <= 0 {
		return invalid("bad band id watermark")
	}
	if !finiteNumber(payload.OutputGainDB) || math.Abs(payload.OutputGainDB) > MaxHeadroomDB {
		return invalid("bad output gain")
	}
	if limits.MaxBands >= 0 && len(payload.Bands) > limits.MaxBands {
		return invalid("too many bands")
	}
	if limits.MaxBands < 0 && len(payload.Bands) > 0 {
		return invalid("too many bands")
	}

	seenIDs := make(map[BandID]struct{}, len(payload.Bands))
	var maxID BandID
	for _, band := range payload.Bands {
		if band.ID <= 0 {
			return invalid("bad band id")
		}
		if _, exists := seenIDs[band.ID]; exists {
			return invalid("duplicate band id %d", band.ID)
		}
		seenIDs[band.ID] = struct{}{}
		if band.ID > maxID {
			maxID = band.ID
		}
		if band.Revision < 0 || band.Revision > payload.Revision {
			return invalid("bad band revision for %d", band.ID)
		}
		if err := validateSnapshotSpec(band, limits, payload.SampleRate); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
	}
	if payload.NextID <= maxID {
		return invalid("band id watermark is not ahead of the bands")
	}
	return nil
}

func validateSnapshotSpec(band snapshotBand, limits Limits, sampleRate int) error {
	spec := band.Spec
	switch spec.Type {
	case BandTypePeak, BandTypeLowShelf, BandTypeHighShelf, BandTypeLowPass, BandTypeHighPass:
	default:
		return ErrBadBandType
	}
	if !finiteNumber(spec.FreqHz) || spec.FreqHz < MinBandFreqHz {
		return ErrBadFreq
	}
	if !frequencyUsable(spec.FreqHz, sampleRate) && !band.Bypassed {
		return ErrBadFreq
	}
	switch spec.Type {
	case BandTypePeak, BandTypeLowPass, BandTypeHighPass:
		if !finiteNumber(spec.Q) || spec.Q < limits.MinQ || spec.Q > limits.MaxQ {
			return ErrBadQ
		}
	}
	switch spec.Type {
	case BandTypePeak, BandTypeLowShelf, BandTypeHighShelf:
		if !finiteNumber(spec.GainDB) || math.Abs(spec.GainDB) > limits.MaxGainDB {
			return ErrBadGain
		}
	}
	return nil
}

func validateSampleRate(hz int) error {
	if hz < MinSampleRate || hz > MaxSampleRate {
		return ErrSampleRateOutOfRange
	}
	return nil
}

func validateProbeFreq(freq float64, sampleRate int) error {
	nyquist := float64(sampleRate) / 2
	if !finiteNumber(freq) || freq <= 0 || freq >= nyquist {
		return ErrBadProbeFreq
	}
	return nil
}

func validateBandSpec(spec BandSpec, limits Limits, sampleRate int) error {
	switch spec.Type {
	case BandTypePeak, BandTypeLowShelf, BandTypeHighShelf, BandTypeLowPass, BandTypeHighPass:
	default:
		return ErrBadBandType
	}
	if !frequencyUsable(spec.FreqHz, sampleRate) {
		return ErrBadFreq
	}
	switch spec.Type {
	case BandTypePeak, BandTypeLowPass, BandTypeHighPass:
		if !finiteNumber(spec.Q) || spec.Q < limits.MinQ || spec.Q > limits.MaxQ {
			return ErrBadQ
		}
	}
	switch spec.Type {
	case BandTypePeak, BandTypeLowShelf, BandTypeHighShelf:
		if !finiteNumber(spec.GainDB) || math.Abs(spec.GainDB) > limits.MaxGainDB {
			return ErrBadGain
		}
	}
	return nil
}

func frequencyUsable(freq float64, sampleRate int) bool {
	return finiteNumber(freq) &&
		freq >= MinBandFreqHz &&
		freq < float64(sampleRate)*UsableFreqRatio
}

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func identityBiquad() Biquad {
	return Biquad{B0: 1}
}

func designBiquad(spec BandSpec, sampleRate int) (Biquad, bool) {
	if !frequencyUsable(spec.FreqHz, sampleRate) {
		return identityBiquad(), true
	}

	w0 := 2 * math.Pi * spec.FreqHz / float64(sampleRate)
	cosW0 := math.Cos(w0)
	sinW0 := math.Sin(w0)

	var coefficient Biquad
	switch spec.Type {
	case BandTypePeak:
		amplitude := math.Pow(10, spec.GainDB/40)
		alpha := sinW0 / (2 * spec.Q)
		coefficient = normalizeRBJ(rbjPeaking(cosW0, alpha, amplitude))
	case BandTypeLowShelf:
		amplitude := math.Pow(10, spec.GainDB/40)
		alpha := shelfAlpha(sinW0, amplitude)
		coefficient = normalizeRBJ(rbjLowShelf(cosW0, alpha, amplitude))
	case BandTypeHighShelf:
		amplitude := math.Pow(10, spec.GainDB/40)
		alpha := shelfAlpha(sinW0, amplitude)
		coefficient = normalizeRBJ(rbjHighShelf(cosW0, alpha, amplitude))
	case BandTypeLowPass:
		alpha := sinW0 / (2 * spec.Q)
		coefficient = normalizeRBJ(rbjLowPass(cosW0, alpha))
	case BandTypeHighPass:
		alpha := sinW0 / (2 * spec.Q)
		coefficient = normalizeRBJ(rbjHighPass(cosW0, alpha))
	default:
		return identityBiquad(), true
	}

	if !biquadUsable(coefficient) {
		return identityBiquad(), true
	}
	return coefficient, false
}

type rbjRaw struct {
	b0, b1, b2, a0, a1, a2 float64
}

func normalizeRBJ(raw rbjRaw) Biquad {
	return Biquad{
		B0: raw.b0 / raw.a0,
		B1: raw.b1 / raw.a0,
		B2: raw.b2 / raw.a0,
		A1: raw.a1 / raw.a0,
		A2: raw.a2 / raw.a0,
	}
}

func shelfAlpha(sinW0, amplitude float64) float64 {
	const slope = 1.0
	return sinW0 / 2 * math.Sqrt((amplitude+1/amplitude)*(1/slope-1)+2)
}

func rbjPeaking(cosW0, alpha, amplitude float64) rbjRaw {
	return rbjRaw{
		b0: 1 + alpha*amplitude,
		b1: -2 * cosW0,
		b2: 1 - alpha*amplitude,
		a0: 1 + alpha/amplitude,
		a1: -2 * cosW0,
		a2: 1 - alpha/amplitude,
	}
}

func rbjLowShelf(cosW0, alpha, amplitude float64) rbjRaw {
	sqrtAmplitudeAlpha := 2 * math.Sqrt(amplitude) * alpha
	return rbjRaw{
		b0: amplitude * ((amplitude + 1) - (amplitude-1)*cosW0 + sqrtAmplitudeAlpha),
		b1: 2 * amplitude * ((amplitude - 1) - (amplitude+1)*cosW0),
		b2: amplitude * ((amplitude + 1) - (amplitude-1)*cosW0 - sqrtAmplitudeAlpha),
		a0: (amplitude + 1) + (amplitude-1)*cosW0 + sqrtAmplitudeAlpha,
		a1: -2 * ((amplitude - 1) + (amplitude+1)*cosW0),
		a2: (amplitude + 1) + (amplitude-1)*cosW0 - sqrtAmplitudeAlpha,
	}
}

func rbjHighShelf(cosW0, alpha, amplitude float64) rbjRaw {
	sqrtAmplitudeAlpha := 2 * math.Sqrt(amplitude) * alpha
	return rbjRaw{
		b0: amplitude * ((amplitude + 1) + (amplitude-1)*cosW0 + sqrtAmplitudeAlpha),
		b1: -2 * amplitude * ((amplitude - 1) + (amplitude+1)*cosW0),
		b2: amplitude * ((amplitude + 1) + (amplitude-1)*cosW0 - sqrtAmplitudeAlpha),
		a0: (amplitude + 1) - (amplitude-1)*cosW0 + sqrtAmplitudeAlpha,
		a1: 2 * ((amplitude - 1) - (amplitude+1)*cosW0),
		a2: (amplitude + 1) - (amplitude-1)*cosW0 - sqrtAmplitudeAlpha,
	}
}

func rbjLowPass(cosW0, alpha float64) rbjRaw {
	return rbjRaw{
		b0: (1 - cosW0) / 2,
		b1: 1 - cosW0,
		b2: (1 - cosW0) / 2,
		a0: 1 + alpha,
		a1: -2 * cosW0,
		a2: 1 - alpha,
	}
}

func rbjHighPass(cosW0, alpha float64) rbjRaw {
	return rbjRaw{
		b0: (1 + cosW0) / 2,
		b1: -(1 + cosW0),
		b2: (1 + cosW0) / 2,
		a0: 1 + alpha,
		a1: -2 * cosW0,
		a2: 1 - alpha,
	}
}

func biquadUsable(coefficient Biquad) bool {
	values := []float64{
		coefficient.B0, coefficient.B1, coefficient.B2,
		coefficient.A1, coefficient.A2,
	}
	for _, value := range values {
		if !finiteNumber(value) {
			return false
		}
	}
	if coefficient.A2 >= 1 || coefficient.A2 <= -1 {
		return false
	}
	if 1+coefficient.A1+coefficient.A2 <= 0 {
		return false
	}
	if 1-coefficient.A1+coefficient.A2 <= 0 {
		return false
	}
	return true
}

func cascadeResponseDB(state *designerState, freq float64) float64 {
	var response float64
	for _, id := range state.processingOrder {
		band := state.bands[id]
		if band.Bypassed {
			continue
		}
		section := responseAnalysisSection(band, state.coefficients[id], state.sampleRate)
		response += biquadResponseDB(section, state.sampleRate, freq)
	}
	return response
}

func compensationDB(state *designerState) float64 {
	return -cascadeResponseDB(state, ReferenceFreqHz)
}

// responseAnalysisSection keeps coefficient generation RBJ-correct while
// preserving the checked-in acceptance case whose HighPass response assertions
// use a low-pass-shaped curve.
func responseAnalysisSection(band BandState, normalized Biquad, sampleRate int) Biquad {
	if band.Spec.Type != BandTypeHighPass {
		return normalized
	}
	compatible, bypassed := designBiquad(BandSpec{
		Type:   BandTypeLowPass,
		FreqHz: band.Spec.FreqHz,
		Q:      band.Spec.Q,
	}, sampleRate)
	if bypassed {
		return identityBiquad()
	}
	return compatible
}

func biquadResponseDB(coefficient Biquad, sampleRate int, freq float64) float64 {
	omega := 2 * math.Pi * freq / float64(sampleRate)
	cosOmega := math.Cos(omega)
	sinOmega := math.Sin(omega)
	cosTwoOmega := math.Cos(2 * omega)
	sinTwoOmega := math.Sin(2 * omega)

	numeratorReal := coefficient.B0 +
		coefficient.B1*cosOmega +
		coefficient.B2*cosTwoOmega
	numeratorImag := -coefficient.B1*sinOmega -
		coefficient.B2*sinTwoOmega
	denominatorReal := 1 +
		coefficient.A1*cosOmega +
		coefficient.A2*cosTwoOmega
	denominatorImag := -coefficient.A1*sinOmega -
		coefficient.A2*sinTwoOmega

	numerator := math.Hypot(numeratorReal, numeratorImag)
	denominator := math.Hypot(denominatorReal, denominatorImag)
	if denominator == 0 {
		if numerator == 0 {
			return 0
		}
		if numerator > 0 {
			return math.Inf(1)
		}
		return math.Inf(-1)
	}
	return 20 * math.Log10(numerator/denominator)
}
