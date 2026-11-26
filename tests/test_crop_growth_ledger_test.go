package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-18"
)

var farmClock = time.Unix(1700000000, 0)

func newLedger(maxPlots int) *impl.CropGrowthLedger {
	return impl.NewCropGrowthLedger(impl.Limits{MaxPlots: maxPlots, MaxStageMillis: 60000, MaxEvents: 32}, farmClock)
}

func wheatSpec() impl.SeedSpec {
	return impl.SeedSpec{
		ID:               "wheat",
		Stages:           3,
		StageMillis:      1000,
		WaterBonusMillis: 400,
		MaxWaters:        2,
		DryLimit:         99,
		BaseYield:        20,
		WiltPercent:      50,
	}
}

func mustSeed(t *testing.T, l *impl.CropGrowthLedger, spec impl.SeedSpec) {
	t.Helper()
	if err := l.RegisterSeed(spec); err != nil {
		t.Fatalf("register seed %q: %v", spec.ID, err)
	}
}

func mustPlant(t *testing.T, l *impl.CropGrowthLedger, plot string) impl.CropID {
	t.Helper()
	id, err := l.Plant(plot, "wheat")
	if err != nil {
		t.Fatalf("plant %q: %v", plot, err)
	}
	if id == "" {
		t.Fatalf("planting %q returned an empty crop id", plot)
	}
	return id
}

func mustAdvance(t *testing.T, l *impl.CropGrowthLedger, at time.Time) []impl.PlotEvent {
	t.Helper()
	events, err := l.Advance(at)
	if err != nil {
		t.Fatalf("advance to %s: %v", at, err)
	}
	return events
}

func TestRegisterSeedValidatesSpec(t *testing.T) {
	l := newLedger(4)

	blank := wheatSpec()
	blank.ID = ""
	if err := l.RegisterSeed(blank); !errors.Is(err, impl.ErrBadSeed) {
		t.Fatalf("blank seed id must fail with ErrBadSeed, got %v", err)
	}
	noStages := wheatSpec()
	noStages.Stages = 0
	if err := l.RegisterSeed(noStages); !errors.Is(err, impl.ErrBadSeed) {
		t.Fatalf("zero stages must fail with ErrBadSeed, got %v", err)
	}
	slow := wheatSpec()
	slow.StageMillis = 70000
	if err := l.RegisterSeed(slow); !errors.Is(err, impl.ErrStageTooLong) {
		t.Fatalf("a stage longer than MaxStageMillis must fail with ErrStageTooLong, got %v", err)
	}
	mustSeed(t, l, wheatSpec())
	if err := l.RegisterSeed(wheatSpec()); !errors.Is(err, impl.ErrSeedExists) {
		t.Fatalf("duplicate seed id must fail with ErrSeedExists, got %v", err)
	}
}

func TestPlantValidatesRequest(t *testing.T) {
	l := impl.NewCropGrowthLedger(impl.Limits{MaxPlots: 1, MaxStageMillis: 60000, MaxEvents: 32}, farmClock)
	mustSeed(t, l, wheatSpec())

	if _, err := l.Plant("   ", "wheat"); !errors.Is(err, impl.ErrBlankPlot) {
		t.Fatalf("a blank plot id must fail with ErrBlankPlot, got %v", err)
	}
	if _, err := l.Plant("plot-1", "corn"); !errors.Is(err, impl.ErrUnknownSeed) {
		t.Fatalf("an unregistered seed must fail with ErrUnknownSeed, got %v", err)
	}
	mustPlant(t, l, "plot-1")
	if _, err := l.Plant("plot-1", "wheat"); !errors.Is(err, impl.ErrPlotOccupied) {
		t.Fatalf("planting into an occupied plot must fail with ErrPlotOccupied, got %v", err)
	}
	if _, err := l.Plant("plot-2", "wheat"); !errors.Is(err, impl.ErrPlotLimit) {
		t.Fatalf("planting past MaxPlots must fail with ErrPlotLimit, got %v", err)
	}
}

func TestPlantCreatesFreshCrop(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	id := mustPlant(t, l, "plot-1")

	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after planting: %v", err)
	}
	if info.CropID != id || info.Seed != "wheat" {
		t.Fatalf("stage = %+v, want crop %q of wheat", info, id)
	}
	if info.Stage != 0 || info.Stages != 3 {
		t.Fatalf("stage index = %d/%d, want 0/3", info.Stage, info.Stages)
	}
	if info.ProgressMillis != 0 || info.StageMillis != 1000 {
		t.Fatalf("progress = %d of %d ms, want a fresh stage", info.ProgressMillis, info.StageMillis)
	}
	if info.WatersLeft != 2 || info.WatersUsed != 0 {
		t.Fatalf("waters = used %d left %d, want 0/2", info.WatersUsed, info.WatersLeft)
	}
	if info.Mature || info.Wilted {
		t.Fatalf("a fresh crop reported mature=%v wilted=%v", info.Mature, info.Wilted)
	}
	if got := l.PlotCount(); got != 1 {
		t.Fatalf("plot count = %d, want 1", got)
	}
	events := l.Events()
	if len(events) != 1 || events[0].Kind != impl.EventPlanted || events[0].Plot != "plot-1" {
		t.Fatalf("events after planting = %+v, want one planted event for plot-1", events)
	}
}

func TestAdvanceProgressesWithinStage(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	mustPlant(t, l, "plot-1")

	mustAdvance(t, l, farmClock.Add(500*time.Millisecond))
	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if info.Stage != 0 || info.ProgressMillis != 500 {
		t.Fatalf("after 500 ms the crop is at stage %d with %d ms, want stage 0 with 500", info.Stage, info.ProgressMillis)
	}

	mustAdvance(t, l, farmClock.Add(900*time.Millisecond))
	info, err = l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if info.Stage != 0 || info.ProgressMillis != 900 {
		t.Fatalf("after 900 ms the crop is at stage %d with %d ms, want stage 0 with 900", info.Stage, info.ProgressMillis)
	}
}

func TestAdvanceCrossesStageBoundaries(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	mustPlant(t, l, "plot-1")

	mustAdvance(t, l, farmClock.Add(2500*time.Millisecond))
	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if info.Stage != 2 || info.ProgressMillis != 500 {
		t.Fatalf("after 2500 ms the crop is at stage %d with %d ms, want stage 2 with 500", info.Stage, info.ProgressMillis)
	}
	if info.Mature {
		t.Fatalf("a crop halfway through its last stage reported mature")
	}
	if info.WatersLeft != 2 {
		t.Fatalf("waters left after the stage changes = %d, want the cap reset to 2", info.WatersLeft)
	}
}

func TestWaterSpeedsUpCurrentStage(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	mustPlant(t, l, "plot-1")
	if err := l.RefillWater(5); err != nil {
		t.Fatalf("refill water: %v", err)
	}
	mustAdvance(t, l, farmClock.Add(200*time.Millisecond))

	if err := l.Water("plot-1"); err != nil {
		t.Fatalf("water: %v", err)
	}
	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after watering: %v", err)
	}
	if info.ProgressMillis != 600 {
		t.Fatalf("progress after watering = %d ms, want 200+400", info.ProgressMillis)
	}
	if info.WatersUsed != 1 || info.WatersLeft != 1 {
		t.Fatalf("waters used/left = %d/%d, want 1/1", info.WatersUsed, info.WatersLeft)
	}
	if got := l.WaterLeft(); got != 4 {
		t.Fatalf("reservoir = %d, want 4 after one watering", got)
	}

	mustAdvance(t, l, farmClock.Add(600*time.Millisecond))
	info, err = l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after the boundary: %v", err)
	}
	if info.Stage != 1 || info.ProgressMillis != 0 {
		t.Fatalf("after the boundary the crop is at stage %d with %d ms, want stage 1 with 0", info.Stage, info.ProgressMillis)
	}
	if info.DryStreak != 0 {
		t.Fatalf("a watered stage left a dry streak of %d, want 0", info.DryStreak)
	}
}

func TestWaterRejectsInvalidRequests(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())

	if err := l.Water("plot-1"); !errors.Is(err, impl.ErrNoCrop) {
		t.Fatalf("watering an empty plot must fail with ErrNoCrop, got %v", err)
	}
	mustPlant(t, l, "plot-1")
	if err := l.Water("plot-1"); !errors.Is(err, impl.ErrNoWater) {
		t.Fatalf("watering with an empty reservoir must fail with ErrNoWater, got %v", err)
	}
	if err := l.RefillWater(1); err != nil {
		t.Fatalf("refill water: %v", err)
	}
	if err := l.Water("plot-1"); err != nil {
		t.Fatalf("water: %v", err)
	}
	if got := l.WaterLeft(); got != 0 {
		t.Fatalf("reservoir after the watering = %d, want 0", got)
	}
	if err := l.Water("plot-1"); !errors.Is(err, impl.ErrNoWater) {
		t.Fatalf("watering an empty reservoir must fail with ErrNoWater, got %v", err)
	}
	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if info.WatersUsed != 1 || info.ProgressMillis != 400 {
		t.Fatalf("stage after the rejected waterings = %+v, want one watering applied", info)
	}
}

func TestHarvestRequiresFinishedCrop(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())

	if _, err := l.Harvest("plot-1"); !errors.Is(err, impl.ErrNoCrop) {
		t.Fatalf("harvesting an empty plot must fail with ErrNoCrop, got %v", err)
	}
	mustPlant(t, l, "plot-1")
	if _, err := l.Harvest("plot-1"); !errors.Is(err, impl.ErrNotReady) {
		t.Fatalf("harvesting a fresh crop must fail with ErrNotReady, got %v", err)
	}
	mustAdvance(t, l, farmClock.Add(500*time.Millisecond))
	if _, err := l.Harvest("plot-1"); !errors.Is(err, impl.ErrNotReady) {
		t.Fatalf("harvesting a growing crop must fail with ErrNotReady, got %v", err)
	}
}

func TestHarvestSettlesAndClearsPlot(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	id := mustPlant(t, l, "plot-1")
	mustAdvance(t, l, farmClock.Add(3000*time.Millisecond))

	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage before harvest: %v", err)
	}
	if !info.Mature {
		t.Fatalf("a crop fed three stages of real time is not mature: %+v", info)
	}
	found := false
	for _, ev := range l.Events() {
		if ev.Kind == impl.EventMatured {
			found = true
		}
	}
	if !found {
		t.Fatalf("reaching maturity did not append a matured event")
	}

	y, err := l.Harvest("plot-1")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if y.CropID != id || y.Amount != 20 || y.Wilted {
		t.Fatalf("yield = %+v, want crop %q worth 20 and not wilted", y, id)
	}
	if got := l.PlotCount(); got != 0 {
		t.Fatalf("plot count after harvest = %d, want 0", got)
	}
	if _, err := l.Harvest("plot-1"); !errors.Is(err, impl.ErrNoCrop) {
		t.Fatalf("a repeated harvest must fail with ErrNoCrop, got %v", err)
	}
	if _, err := l.Stage("plot-1"); !errors.Is(err, impl.ErrNoCrop) {
		t.Fatalf("reading the stage after harvest must fail with ErrNoCrop, got %v", err)
	}
}

func TestUprootClearsPlot(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	mustPlant(t, l, "plot-1")

	if err := l.Uproot("plot-1"); err != nil {
		t.Fatalf("uproot: %v", err)
	}
	if got := l.PlotCount(); got != 0 {
		t.Fatalf("plot count after uproot = %d, want 0", got)
	}
	if _, err := l.Stage("plot-1"); !errors.Is(err, impl.ErrNoCrop) {
		t.Fatalf("reading the stage after uproot must fail with ErrNoCrop, got %v", err)
	}
	if err := l.Uproot("plot-1"); !errors.Is(err, impl.ErrNoCrop) {
		t.Fatalf("uprooting an empty plot must fail with ErrNoCrop, got %v", err)
	}
	found := false
	for _, ev := range l.Events() {
		if ev.Kind == impl.EventUprooted && ev.Plot == "plot-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("uprooting did not append an uprooted event")
	}
}

func TestRefillWaterValidatesAmount(t *testing.T) {
	l := newLedger(4)

	if err := l.RefillWater(0); !errors.Is(err, impl.ErrBadWater) {
		t.Fatalf("refilling zero units must fail with ErrBadWater, got %v", err)
	}
	if err := l.RefillWater(-3); !errors.Is(err, impl.ErrBadWater) {
		t.Fatalf("refilling a negative amount must fail with ErrBadWater, got %v", err)
	}
	if got := l.WaterLeft(); got != 0 {
		t.Fatalf("reservoir after the rejected refills = %d, want 0", got)
	}
	if err := l.RefillWater(5); err != nil {
		t.Fatalf("refill 5 units: %v", err)
	}
	if err := l.RefillWater(2); err != nil {
		t.Fatalf("second refill: %v", err)
	}
	if got := l.WaterLeft(); got != 7 {
		t.Fatalf("reservoir = %d, want 7", got)
	}
}

func TestAdvanceRejectsBackwardsClock(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	mustPlant(t, l, "plot-1")
	mustAdvance(t, l, farmClock.Add(1000*time.Millisecond))

	if _, err := l.Advance(farmClock.Add(500 * time.Millisecond)); !errors.Is(err, impl.ErrTimeWentBackwards) {
		t.Fatalf("advancing to an earlier time must fail with ErrTimeWentBackwards, got %v", err)
	}
	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after the rejected advance: %v", err)
	}
	if info.Stage != 1 || info.ProgressMillis != 0 {
		t.Fatalf("a rejected advance moved the crop to stage %d with %d ms", info.Stage, info.ProgressMillis)
	}
}

func TestAdvanceStopsAtMaturity(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	mustPlant(t, l, "plot-1")

	mustAdvance(t, l, farmClock.Add(3000*time.Millisecond))
	first, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage at maturity: %v", err)
	}
	if !first.Mature || first.Stage != 2 || first.ProgressMillis != 1000 {
		t.Fatalf("crop at maturity = %+v, want stage 2 of 3 with a full stage", first)
	}
	mustAdvance(t, l, farmClock.Add(9000*time.Millisecond))
	second, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after extra time: %v", err)
	}
	if second.Stage != first.Stage || second.ProgressMillis != first.ProgressMillis || !second.Mature {
		t.Fatalf("extra real time moved a mature crop: before %+v after %+v", first, second)
	}
}

func TestSnapshotRestoreContinuesGrowth(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	id := mustPlant(t, l, "plot-1")
	mustAdvance(t, l, farmClock.Add(1200*time.Millisecond))

	data, err := l.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewCropGrowthLedger(impl.Limits{MaxPlots: 4, MaxStageMillis: 60000, MaxEvents: 32}, farmClock.Add(-time.Hour))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	info, err := restored.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after restore: %v", err)
	}
	if info.CropID != id || info.Stage != 1 || info.ProgressMillis != 200 {
		t.Fatalf("restored stage = %+v, want crop %q at stage 1 with 200 ms", info, id)
	}
	mustAdvance(t, restored, farmClock.Add(1800*time.Millisecond))
	info, err = restored.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after the restored advance: %v", err)
	}
	if info.Stage != 1 || info.ProgressMillis != 800 {
		t.Fatalf("stage after 600 ms of restored time = %+v, want stage 1 with 800 ms", info)
	}
	second, err := restored.Plant("plot-2", "wheat")
	if err != nil {
		t.Fatalf("plant after restore: %v", err)
	}
	if second == id {
		t.Fatalf("planting after restore reused the crop id %q", id)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	l := newLedger(4)
	mustSeed(t, l, wheatSpec())
	id := mustPlant(t, l, "plot-1")
	mustAdvance(t, l, farmClock.Add(1200*time.Millisecond))

	if err := l.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	info, err := l.Stage("plot-1")
	if err != nil {
		t.Fatalf("stage after a rejected restore: %v", err)
	}
	if info.CropID != id || info.Stage != 1 || info.ProgressMillis != 200 {
		t.Fatalf("a rejected restore changed the live crop to %+v", info)
	}
}
