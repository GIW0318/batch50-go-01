// Package task owns farm plots: planting, stage advancement on real time,
// watering that only speeds up the current stage, wilting after a dry streak
// and a harvest that settles the plot in one step.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

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

type cropState struct {
	ID         CropID `json:"id"`
	Seed       Seed   `json:"seed"`
	Stage      int    `json:"stage"`
	Progress   int64  `json:"progress_millis"`
	WatersUsed int    `json:"waters_used"`
	DryStreak  int    `json:"dry_streak"`
	Mature     bool   `json:"mature"`
	Wilted     bool   `json:"wilted"`
}

type snapshotPayload struct {
	Version   int                   `json:"version"`
	NowMillis int64                 `json:"now_millis"`
	CropSeq   int64                 `json:"crop_seq"`
	EventSeq  int64                 `json:"event_seq"`
	Water     int                   `json:"water"`
	Seeds     map[Seed]SeedSpec     `json:"seeds"`
	Plots     map[string]*cropState `json:"plots"`
	Events    []PlotEvent           `json:"events"`
}

const snapshotVersion = 1

// CropGrowthLedger owns the seed table, the plots, the water reservoir and the
// audit log. All exported methods are safe for concurrent use.
type CropGrowthLedger struct {
	mu       sync.RWMutex
	limits   Limits
	now      time.Time
	seeds    map[Seed]SeedSpec
	plots    map[string]*cropState
	water    int
	events   []PlotEvent
	eventSeq int64
	cropSeq  int64
}

// NewCropGrowthLedger builds a ledger whose clock starts at now.
func NewCropGrowthLedger(limits Limits, now time.Time) *CropGrowthLedger {
	return &CropGrowthLedger{
		limits: limits,
		now:    now,
		seeds:  make(map[Seed]SeedSpec),
		plots:  make(map[string]*cropState),
	}
}

// RegisterSeed records one crop species.
func (l *CropGrowthLedger) RegisterSeed(spec SeedSpec) error {
	if err := validateSeedSpec(spec); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if spec.StageMillis > l.limits.MaxStageMillis {
		return ErrStageTooLong
	}
	if _, exists := l.seeds[spec.ID]; exists {
		return ErrSeedExists
	}
	l.seeds[spec.ID] = spec
	return nil
}

func validateSeedSpec(spec SeedSpec) error {
	if strings.TrimSpace(string(spec.ID)) == "" ||
		spec.Stages < 1 ||
		spec.StageMillis <= 0 ||
		spec.MaxWaters < 0 ||
		spec.DryLimit < 0 ||
		spec.WiltPercent < 0 ||
		spec.WiltPercent > 100 {
		return ErrBadSeed
	}
	return nil
}

// Plant puts one seed into one plot.
func (l *CropGrowthLedger) Plant(plot string, seed Seed) (CropID, error) {
	if strings.TrimSpace(plot) == "" {
		return "", ErrBlankPlot
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, known := l.seeds[seed]; !known {
		return "", ErrUnknownSeed
	}
	if _, occupied := l.plots[plot]; occupied {
		return "", ErrPlotOccupied
	}
	if len(l.plots) >= l.limits.MaxPlots {
		return "", ErrPlotLimit
	}

	l.cropSeq++
	id := CropID(fmt.Sprintf("crop-%d", l.cropSeq))
	l.plots[plot] = &cropState{ID: id, Seed: seed}
	l.appendEventLocked(EventPlanted, plot, id, 0, l.now.UnixMilli())
	return id, nil
}

// Water speeds up the current stage of one plot within its cap.
func (l *CropGrowthLedger) Water(plot string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	crop, ok := l.plots[plot]
	if !ok {
		return ErrNoCrop
	}
	if crop.Mature || crop.Wilted {
		return ErrCropDone
	}

	spec, ok := l.seeds[crop.Seed]
	if !ok {
		return ErrUnknownSeed
	}
	if crop.WatersUsed >= spec.MaxWaters {
		return ErrWaterLimit
	}
	if l.water <= 0 {
		return ErrNoWater
	}

	l.water--
	crop.WatersUsed++
	crop.Progress += spec.WaterBonusMillis
	if crop.Progress < 0 {
		crop.Progress = 0
	}
	if crop.Progress > spec.StageMillis {
		crop.Progress = spec.StageMillis
	}
	l.appendEventLocked(EventWatered, plot, crop.ID, crop.Stage, l.now.UnixMilli())
	return nil
}

// Harvest settles one finished crop and clears the plot.
func (l *CropGrowthLedger) Harvest(plot string) (Yield, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	crop, ok := l.plots[plot]
	if !ok {
		return Yield{}, ErrNoCrop
	}
	if !crop.Mature && !crop.Wilted {
		return Yield{}, ErrNotReady
	}

	spec, ok := l.seeds[crop.Seed]
	if !ok {
		return Yield{}, ErrUnknownSeed
	}

	amount := spec.BaseYield
	wilted := crop.Wilted
	if wilted {
		amount = amount * (100 - spec.WiltPercent) / 100
	}
	yield := Yield{
		Plot:   plot,
		CropID: crop.ID,
		Seed:   crop.Seed,
		Amount: amount,
		Wilted: wilted,
		Stage:  crop.Stage,
	}
	l.appendEventLocked(EventHarvested, plot, crop.ID, crop.Stage, l.now.UnixMilli())
	delete(l.plots, plot)
	return yield, nil
}

// Stage projects the crop of one plot.
func (l *CropGrowthLedger) Stage(plot string) (StageInfo, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	crop, ok := l.plots[plot]
	if !ok {
		return StageInfo{}, ErrNoCrop
	}
	spec, ok := l.seeds[crop.Seed]
	if !ok {
		return StageInfo{}, ErrUnknownSeed
	}

	watersLeft := spec.MaxWaters - crop.WatersUsed
	if watersLeft < 0 {
		watersLeft = 0
	}
	return StageInfo{
		Plot:           plot,
		CropID:         crop.ID,
		Seed:           crop.Seed,
		Stage:          crop.Stage,
		Stages:         spec.Stages,
		ProgressMillis: crop.Progress,
		StageMillis:    spec.StageMillis,
		WatersUsed:     crop.WatersUsed,
		WatersLeft:     watersLeft,
		DryStreak:      crop.DryStreak,
		Wilted:         crop.Wilted,
		Mature:         crop.Mature,
	}, nil
}

// Uproot throws away the crop of one plot.
func (l *CropGrowthLedger) Uproot(plot string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	crop, ok := l.plots[plot]
	if !ok {
		return ErrNoCrop
	}
	l.appendEventLocked(EventUprooted, plot, crop.ID, crop.Stage, l.now.UnixMilli())
	delete(l.plots, plot)
	return nil
}

// Advance moves the ledger clock forward and resolves every plot.
func (l *CropGrowthLedger) Advance(now time.Time) ([]PlotEvent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Before(l.now) {
		return nil, ErrTimeWentBackwards
	}

	elapsedMillis := now.Sub(l.now).Milliseconds()
	l.now = now
	at := now.UnixMilli()

	plots := make([]string, 0, len(l.plots))
	for plot := range l.plots {
		plots = append(plots, plot)
	}
	sort.Strings(plots)

	var advanced []PlotEvent
	for _, plot := range plots {
		crop := l.plots[plot]
		if crop.Mature || crop.Wilted {
			continue
		}
		spec, ok := l.seeds[crop.Seed]
		if !ok {
			continue
		}

		crop.Progress += elapsedMillis
		for {
			if crop.Mature || crop.Wilted || crop.Progress < spec.StageMillis {
				break
			}

			watered := crop.WatersUsed > 0
			if watered {
				crop.DryStreak = 0
			} else {
				crop.DryStreak++
			}
			crop.WatersUsed = 0

			if crop.DryStreak >= spec.DryLimit {
				crop.Wilted = true
				crop.Progress = spec.StageMillis
				event := l.appendEventLocked(EventWilted, plot, crop.ID, crop.Stage, at)
				advanced = append(advanced, event)
				break
			}

			if crop.Stage >= spec.Stages-1 {
				crop.Mature = true
				crop.Stage = spec.Stages - 1
				crop.Progress = spec.StageMillis
				event := l.appendEventLocked(EventMatured, plot, crop.ID, crop.Stage, at)
				advanced = append(advanced, event)
				break
			}

			crop.Progress -= spec.StageMillis
			crop.Stage++
			event := l.appendEventLocked(EventAdvanced, plot, crop.ID, crop.Stage, at)
			advanced = append(advanced, event)
		}
	}

	return advanced, nil
}

// RefillWater adds units to the shared water reservoir.
func (l *CropGrowthLedger) RefillWater(units int) error {
	if units <= 0 {
		return ErrBadWater
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.water += units
	return nil
}

// WaterLeft reads the shared water reservoir.
func (l *CropGrowthLedger) WaterLeft() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.water
}

// PlotCount reads how many plots currently hold a crop.
func (l *CropGrowthLedger) PlotCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.plots)
}

// Events returns a copy of the ledger audit log, oldest first.
func (l *CropGrowthLedger) Events() []PlotEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()

	events := make([]PlotEvent, len(l.events))
	copy(events, l.events)
	return events
}

// Snapshot serialises the whole ledger state.
func (l *CropGrowthLedger) Snapshot() ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	payload := snapshotPayload{
		Version:   snapshotVersion,
		NowMillis: l.now.UnixMilli(),
		CropSeq:   l.cropSeq,
		EventSeq:  l.eventSeq,
		Water:     l.water,
		Seeds:     make(map[Seed]SeedSpec, len(l.seeds)),
		Plots:     make(map[string]*cropState, len(l.plots)),
		Events:    make([]PlotEvent, len(l.events)),
	}
	for id, spec := range l.seeds {
		payload.Seeds[id] = spec
	}
	for plot, crop := range l.plots {
		copied := *crop
		payload.Plots[plot] = &copied
	}
	copy(payload.Events, l.events)

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *CropGrowthLedger) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&payload); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	seeds := make(map[Seed]SeedSpec, len(payload.Seeds))
	for id, spec := range payload.Seeds {
		seeds[id] = spec
	}
	plots := make(map[string]*cropState, len(payload.Plots))
	for plot, crop := range payload.Plots {
		copied := *crop
		plots[plot] = &copied
	}
	events := make([]PlotEvent, len(payload.Events))
	copy(events, payload.Events)
	if maxEvents := l.limits.MaxEvents; maxEvents >= 0 && len(events) > maxEvents {
		events = append([]PlotEvent(nil), events[len(events)-maxEvents:]...)
	} else if l.limits.MaxEvents < 0 {
		events = nil
	}

	l.now = time.UnixMilli(payload.NowMillis)
	l.cropSeq = payload.CropSeq
	l.eventSeq = payload.EventSeq
	l.water = payload.Water
	l.seeds = seeds
	l.plots = plots
	l.events = events
	return nil
}

func validateSnapshot(payload *snapshotPayload) error {
	if payload.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if payload.CropSeq < 0 || payload.EventSeq < 0 || payload.Water < 0 {
		return fmt.Errorf("%w: negative sequence or water", ErrBadSnapshot)
	}

	for id, spec := range payload.Seeds {
		if spec.ID == "" {
			spec.ID = id
		}
		if spec.ID != id {
			return fmt.Errorf("%w: seed id mismatch", ErrBadSnapshot)
		}
		if err := validateSeedSpec(spec); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
	}

	for plot, crop := range payload.Plots {
		if strings.TrimSpace(plot) == "" || crop == nil {
			return fmt.Errorf("%w: invalid plot", ErrBadSnapshot)
		}
		spec, ok := payload.Seeds[crop.Seed]
		if !ok || crop.ID == "" || crop.Stage < 0 || crop.Stage >= spec.Stages {
			return fmt.Errorf("%w: invalid crop reference", ErrBadSnapshot)
		}
		if crop.Progress < 0 || crop.Progress > spec.StageMillis {
			return fmt.Errorf("%w: invalid progress", ErrBadSnapshot)
		}
		if crop.WatersUsed < 0 || crop.WatersUsed > spec.MaxWaters || crop.DryStreak < 0 {
			return fmt.Errorf("%w: invalid water or dry state", ErrBadSnapshot)
		}
		if crop.Mature && (crop.Stage != spec.Stages-1 || crop.Progress != spec.StageMillis) {
			return fmt.Errorf("%w: invalid mature crop", ErrBadSnapshot)
		}
	}

	var lastSeq int64
	for index, event := range payload.Events {
		if event.Plot == "" || event.CropID == "" || event.Stage < 0 || event.Seq <= 0 || event.Seq > payload.EventSeq {
			return fmt.Errorf("%w: invalid event", ErrBadSnapshot)
		}
		if index > 0 && event.Seq <= lastSeq {
			return fmt.Errorf("%w: event sequence is not increasing", ErrBadSnapshot)
		}
		lastSeq = event.Seq
		switch event.Kind {
		case EventPlanted, EventWatered, EventAdvanced, EventMatured, EventWilted, EventHarvested, EventUprooted:
		default:
			return fmt.Errorf("%w: unknown event kind", ErrBadSnapshot)
		}
	}

	return nil
}

func (l *CropGrowthLedger) appendEventLocked(kind string, plot string, cropID CropID, stage int, at int64) PlotEvent {
	l.eventSeq++
	event := PlotEvent{
		Plot:   plot,
		CropID: cropID,
		Kind:   kind,
		Stage:  stage,
		At:     at,
		Seq:    l.eventSeq,
	}
	l.events = append(l.events, event)

	maxEvents := l.limits.MaxEvents
	if maxEvents < 0 {
		maxEvents = 0
	}
	if len(l.events) > maxEvents {
		l.events = append([]PlotEvent(nil), l.events[len(l.events)-maxEvents:]...)
	}
	return event
}
