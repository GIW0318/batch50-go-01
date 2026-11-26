// Package task owns farm plots: planting, stage advancement on real time,
// watering that only speeds up the current stage, wilting after a dry streak
// and a harvest that settles the plot in one step.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Seed names one registered crop species.
type Seed string

// CropID identifies one planted crop.
type CropID string

// Plot event kinds.
const (
	EventPlanted   = "planted"
	EventWatered   = "watered"
	EventAdvanced  = "stage_advanced"
	EventMatured   = "matured"
	EventWilted    = "wilted"
	EventHarvested = "harvested"
	EventUprooted  = "uprooted"
)

// Errors returned by the ledger.
var (
	ErrBlankPlot         = errors.New("plot id is required")
	ErrBadSeed           = errors.New("seed spec is invalid")
	ErrSeedExists        = errors.New("seed is already registered")
	ErrUnknownSeed       = errors.New("unknown seed")
	ErrStageTooLong      = errors.New("stage duration exceeds the ledger limit")
	ErrPlotOccupied      = errors.New("plot already holds a crop")
	ErrPlotLimit         = errors.New("plot limit reached")
	ErrNoCrop            = errors.New("plot holds no crop")
	ErrCropDone          = errors.New("crop is already finished")
	ErrNotReady          = errors.New("crop cannot be harvested yet")
	ErrWaterLimit        = errors.New("watering cap of this stage is used up")
	ErrNoWater           = errors.New("water reservoir is empty")
	ErrBadWater          = errors.New("water amount must be positive")
	ErrTimeWentBackwards = errors.New("clock moved backwards")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// SeedSpec is the static description of one crop species.
type SeedSpec struct {
	ID               Seed  `json:"id"`
	Stages           int   `json:"stages"`
	StageMillis      int64 `json:"stage_millis"`
	WaterBonusMillis int64 `json:"water_bonus_millis"`
	MaxWaters        int   `json:"max_waters"`
	DryLimit         int   `json:"dry_limit"`
	BaseYield        int   `json:"base_yield"`
	WiltPercent      int   `json:"wilt_percent"`
}

// StageInfo is the projection of one planted crop.
type StageInfo struct {
	Plot           string `json:"plot"`
	CropID         CropID `json:"crop_id"`
	Seed           Seed   `json:"seed"`
	Stage          int    `json:"stage"`
	Stages         int    `json:"stages"`
	ProgressMillis int64  `json:"progress_millis"`
	StageMillis    int64  `json:"stage_millis"`
	WatersUsed     int    `json:"waters_used"`
	WatersLeft     int    `json:"waters_left"`
	DryStreak      int    `json:"dry_streak"`
	Wilted         bool   `json:"wilted"`
	Mature         bool   `json:"mature"`
}

// Yield is the settlement of one harvest.
type Yield struct {
	Plot   string `json:"plot"`
	CropID CropID `json:"crop_id"`
	Seed   Seed   `json:"seed"`
	Amount int    `json:"amount"`
	Wilted bool   `json:"wilted"`
	Stage  int    `json:"stage"`
}

// PlotEvent is one entry of the ledger audit log.
type PlotEvent struct {
	Plot   string `json:"plot"`
	CropID CropID `json:"crop_id"`
	Kind   string `json:"kind"`
	Stage  int    `json:"stage"`
	At     int64  `json:"at"`
	Seq    int64  `json:"seq"`
}

// Limits bounds the plot table, the stage duration and the event log.
type Limits struct {
	MaxPlots       int   `json:"max_plots"`
	MaxStageMillis int64 `json:"max_stage_millis"`
	MaxEvents      int   `json:"max_events"`
}

// CropGrowthLedger owns the seed table, the plots, the water reservoir and the
// audit log. All exported methods are safe for concurrent use.
type CropGrowthLedger struct {
	limits Limits
	now    time.Time
}

// NewCropGrowthLedger builds a ledger whose clock starts at now.
func NewCropGrowthLedger(limits Limits, now time.Time) *CropGrowthLedger {
	return &CropGrowthLedger{limits: limits, now: now}
}

// RegisterSeed records one crop species.
func (l *CropGrowthLedger) RegisterSeed(spec SeedSpec) error {
	return errNotImplemented
}

// Plant puts one seed into one plot.
func (l *CropGrowthLedger) Plant(plot string, seed Seed) (CropID, error) {
	return "", errNotImplemented
}

// Water speeds up the current stage of one plot within its cap.
func (l *CropGrowthLedger) Water(plot string) error {
	return errNotImplemented
}

// Harvest settles one finished crop and clears the plot.
func (l *CropGrowthLedger) Harvest(plot string) (Yield, error) {
	return Yield{}, errNotImplemented
}

// Stage projects the crop of one plot.
func (l *CropGrowthLedger) Stage(plot string) (StageInfo, error) {
	return StageInfo{}, errNotImplemented
}

// Uproot throws away the crop of one plot.
func (l *CropGrowthLedger) Uproot(plot string) error {
	return errNotImplemented
}

// Advance moves the ledger clock forward and resolves every plot.
func (l *CropGrowthLedger) Advance(now time.Time) ([]PlotEvent, error) {
	return nil, errNotImplemented
}

// RefillWater adds units to the shared water reservoir.
func (l *CropGrowthLedger) RefillWater(units int) error {
	return errNotImplemented
}

// WaterLeft reads the shared water reservoir.
func (l *CropGrowthLedger) WaterLeft() int {
	return 0
}

// PlotCount reads how many plots currently hold a crop.
func (l *CropGrowthLedger) PlotCount() int {
	return 0
}

// Events returns a copy of the ledger audit log, oldest first.
func (l *CropGrowthLedger) Events() []PlotEvent {
	return nil
}

// Snapshot serialises the whole ledger state.
func (l *CropGrowthLedger) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *CropGrowthLedger) Restore(data []byte) error {
	return errNotImplemented
}
