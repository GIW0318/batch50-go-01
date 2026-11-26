package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-game-c-11"
)

func gameC11Devices() []impl.Device {
	return []impl.Device{
		{ID: "dev-a", Name: "Speakers", Rates: []int{48000}, Preferred: 48000, Available: true},
		{ID: "dev-b", Name: "USB Headset", Rates: []int{44100, 48000}, Preferred: 44100, Available: true},
		{ID: "dev-c", Name: "Bluetooth", Rates: []int{44100}, Preferred: 44100, Available: false},
	}
}

func gameC11Limits() impl.Limits {
	return impl.Limits{ProjectRate: 48000, Fade: 40 * time.Millisecond, MaxDevices: 8, MaxSwitches: 32}
}

func gameC11New(limits impl.Limits) *impl.AudioDeviceSwitcher {
	return impl.NewAudioDeviceSwitcher(gameC11Devices(), limits)
}

func TestDevicesKeepRegistrationOrder(t *testing.T) {
	s := gameC11New(gameC11Limits())
	got := s.Devices()
	if len(got) != 3 {
		t.Fatalf("registered devices = %d, want 3", len(got))
	}
	wantIDs := []string{"dev-a", "dev-b", "dev-c"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("device %d = %q, want %q in registration order", i, got[i].ID, want)
		}
	}
	if got[1].Preferred != 44100 {
		t.Fatalf("device dev-b preferred rate = %d, want 44100", got[1].Preferred)
	}
	if got[2].Available {
		t.Fatalf("device dev-c must stay unavailable, got available=true")
	}
}

func TestSetDeviceRejectsUnknownAndUnavailable(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-ghost"); !errors.Is(err, impl.ErrUnknownDevice) {
		t.Fatalf("unknown device must report ErrUnknownDevice, got %v", err)
	}
	if err := s.SetDevice("dev-c"); !errors.Is(err, impl.ErrDeviceUnavailable) {
		t.Fatalf("unavailable device must report ErrDeviceUnavailable, got %v", err)
	}
	if got := s.Clock().Device; got != "" {
		t.Fatalf("rejected selections must not activate a device, got %q", got)
	}
	if got := len(s.Devices()); got != 3 {
		t.Fatalf("rejected selections must not drop devices, got %d", got)
	}
}

func TestSetDeviceWhileIdleAdoptsDevice(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("idle selection of dev-a must succeed, got %v", err)
	}
	clk := s.Clock()
	if clk.Device != "dev-a" {
		t.Fatalf("active device = %q, want dev-a", clk.Device)
	}
	if clk.Rate != 48000 {
		t.Fatalf("active rate = %d, want the device rate 48000", clk.Rate)
	}
	if clk.Switching || clk.Switches != 0 {
		t.Fatalf("idle selection is not a switch, got switching=%v switches=%d", clk.Switching, clk.Switches)
	}
}

func TestSetDeviceRefusedOnceSamplesPlayed(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(4800); err != nil {
		t.Fatalf("advance 4800 samples: %v", err)
	}
	if err := s.SetDevice("dev-b"); !errors.Is(err, impl.ErrBusy) {
		t.Fatalf("selection during playback must report ErrBusy, got %v", err)
	}
	clk := s.Clock()
	if clk.Device != "dev-a" {
		t.Fatalf("refused selection changed the device to %q", clk.Device)
	}
	if clk.Position != 4800 {
		t.Fatalf("position = %d, want 4800 after the refused selection", clk.Position)
	}
}

func TestSwitchPlansHandoverAtPlayedSample(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(4800); err != nil {
		t.Fatalf("advance: %v", err)
	}
	plan, err := s.Switch("dev-b")
	if err != nil {
		t.Fatalf("switch to dev-b: %v", err)
	}
	if plan.FromDevice != "dev-a" || plan.ToDevice != "dev-b" {
		t.Fatalf("plan endpoints = %q -> %q, want dev-a -> dev-b", plan.FromDevice, plan.ToDevice)
	}
	if plan.FromRate != 48000 {
		t.Fatalf("plan from rate = %d, want 48000", plan.FromRate)
	}
	if plan.ToRate != 48000 {
		t.Fatalf("plan to rate = %d, want the project rate 48000 because dev-b supports it", plan.ToRate)
	}
	if plan.Resample {
		t.Fatalf("plan must not resample when the target supports the project rate")
	}
	if plan.AtSample != 4800 {
		t.Fatalf("plan anchor sample = %d, want the played sample count 4800", plan.AtSample)
	}
	if plan.Index != 1 {
		t.Fatalf("first plan index = %d, want 1", plan.Index)
	}
	if plan.Fade != 40*time.Millisecond {
		t.Fatalf("plan fade = %v, want the configured 40ms", plan.Fade)
	}
	pending, ok := s.Pending()
	if !ok || pending.ToDevice != "dev-b" || pending.Index != plan.Index {
		t.Fatalf("pending plan = %+v ok=%v, want the plan just built", pending, ok)
	}
	if clk := s.Clock(); clk.Device != "dev-a" {
		t.Fatalf("planning must not move playback to %q before Commit", clk.Device)
	}
}

func TestSwitchRejectsBadTargets(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if _, err := s.Switch("dev-ghost"); !errors.Is(err, impl.ErrUnknownDevice) {
		t.Fatalf("switch to an unknown device must report ErrUnknownDevice, got %v", err)
	}
	if _, err := s.Switch("dev-c"); !errors.Is(err, impl.ErrDeviceUnavailable) {
		t.Fatalf("switch to an unavailable device must report ErrDeviceUnavailable, got %v", err)
	}
	if _, err := s.Switch("dev-a"); !errors.Is(err, impl.ErrSameDevice) {
		t.Fatalf("switch to the active device must report ErrSameDevice, got %v", err)
	}
	if _, ok := s.Pending(); ok {
		t.Fatalf("rejected switches must not leave a pending plan")
	}
}

func TestSwitchBudgetIsEnforced(t *testing.T) {
	limits := gameC11Limits()
	limits.MaxSwitches = 2
	s := gameC11New(limits)
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	for i, target := range []string{"dev-b", "dev-a"} {
		plan, err := s.Switch(target)
		if err != nil {
			t.Fatalf("switch %d to %s: %v", i, target, err)
		}
		if err := s.Commit(plan); err != nil {
			t.Fatalf("commit switch %d: %v", i, err)
		}
		if err := s.Advance(48000); err != nil {
			t.Fatalf("advance past fade %d: %v", i, err)
		}
	}
	if _, err := s.Switch("dev-b"); !errors.Is(err, impl.ErrSwitchLimit) {
		t.Fatalf("third switch must report ErrSwitchLimit, got %v", err)
	}
	if got := s.Stats().Switches; got != 2 {
		t.Fatalf("committed switches = %d, want the budget 2", got)
	}
}

func TestCommitRequiresThePendingPlan(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Commit(impl.Plan{Index: 1, ToDevice: "dev-b"}); !errors.Is(err, impl.ErrNoPlan) {
		t.Fatalf("commit without a pending plan must report ErrNoPlan, got %v", err)
	}
	plan, err := s.Switch("dev-b")
	if err != nil {
		t.Fatalf("switch to dev-b: %v", err)
	}
	wrong := plan
	wrong.Index = plan.Index + 3
	if err := s.Commit(wrong); !errors.Is(err, impl.ErrBadPlan) {
		t.Fatalf("commit of a foreign plan must report ErrBadPlan, got %v", err)
	}
	if err := s.Commit(plan); err != nil {
		t.Fatalf("commit of the pending plan: %v", err)
	}
	if err := s.Commit(plan); !errors.Is(err, impl.ErrNoPlan) {
		t.Fatalf("second commit of the same plan must report ErrNoPlan, got %v", err)
	}
}

func TestCommitFadesOutAndInAcrossDevices(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(4800); err != nil {
		t.Fatalf("advance: %v", err)
	}
	plan, err := s.Switch("dev-b")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := s.Commit(plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	clk := s.Clock()
	if clk.Device != "dev-b" {
		t.Fatalf("clock device after commit = %q, want dev-b", clk.Device)
	}
	if clk.Rate != 48000 {
		t.Fatalf("clock rate after commit = %d, want 48000", clk.Rate)
	}
	if !clk.Switching {
		t.Fatalf("a fade of %v must still be running right after commit", plan.Fade)
	}
	if clk.FadeRemain != 40*time.Millisecond {
		t.Fatalf("fade remaining = %v, want the full 40ms once", clk.FadeRemain)
	}
	if clk.Switches != 1 {
		t.Fatalf("switch counter = %d, want 1 after one commit", clk.Switches)
	}
	if err := s.Advance(1920); err != nil {
		t.Fatalf("advance the fade window: %v", err)
	}
	clk = s.Clock()
	if clk.Switching || clk.FadeRemain != 0 {
		t.Fatalf("fade must finish after 40ms of samples, got switching=%v remain=%v", clk.Switching, clk.FadeRemain)
	}
	if clk.Position != 4800+1920 {
		t.Fatalf("position = %d, want %d", clk.Position, 4800+1920)
	}
}

func TestPositionAdvancesOncePerPlayedSample(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(4800); err != nil {
		t.Fatalf("advance: %v", err)
	}
	plan, err := s.Switch("dev-b")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := s.Commit(plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.Advance(2400); err != nil {
		t.Fatalf("advance during fade: %v", err)
	}
	clk := s.Clock()
	if clk.Position != 7200 {
		t.Fatalf("timeline position = %d, want 4800+2400=7200 exactly once", clk.Position)
	}
	if clk.Played != 7200 {
		t.Fatalf("played samples = %d, want 7200 at an unchanged rate", clk.Played)
	}
	if want := 150 * time.Millisecond; clk.Elapsed != want {
		t.Fatalf("elapsed = %v, want %v for 7200 samples at 48000", clk.Elapsed, want)
	}
}

func TestSetFadeValidatesAndZeroSkipsTheFade(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetFade(-1 * time.Millisecond); !errors.Is(err, impl.ErrBadFade) {
		t.Fatalf("negative fade must report ErrBadFade, got %v", err)
	}
	if err := s.SetFade(3 * time.Second); !errors.Is(err, impl.ErrBadFade) {
		t.Fatalf("fade above the maximum must report ErrBadFade, got %v", err)
	}
	if err := s.SetFade(0); err != nil {
		t.Fatalf("zero fade must be accepted, got %v", err)
	}
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	plan, err := s.Switch("dev-b")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if plan.Fade != 0 {
		t.Fatalf("plan fade = %v, want 0 after SetFade(0)", plan.Fade)
	}
	if err := s.Commit(plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if clk := s.Clock(); clk.Switching || clk.FadeRemain != 0 {
		t.Fatalf("zero fade must complete at commit, got switching=%v remain=%v", clk.Switching, clk.FadeRemain)
	}
}

func TestSnapshotRestoreKeepsDeviceAndTimeline(t *testing.T) {
	limits := gameC11Limits()
	s := gameC11New(limits)
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(4800); err != nil {
		t.Fatalf("advance: %v", err)
	}
	plan, err := s.Switch("dev-b")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := s.Commit(plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := gameC11New(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	clk := restored.Clock()
	if clk.Device != "dev-b" {
		t.Fatalf("restored device = %q, want dev-b", clk.Device)
	}
	if clk.Position != 4800 {
		t.Fatalf("restored position = %d, want 4800", clk.Position)
	}
	if clk.Switches != 1 {
		t.Fatalf("restored switch counter = %d, want 1", clk.Switches)
	}
	if !clk.Switching || clk.FadeRemain != 40*time.Millisecond {
		t.Fatalf("restored fade state = switching %v remain %v, want an unfinished 40ms fade", clk.Switching, clk.FadeRemain)
	}
	if err := restored.Advance(1920); err != nil {
		t.Fatalf("advance after restore: %v", err)
	}
	if clk := restored.Clock(); clk.Switching || clk.Position != 6720 {
		t.Fatalf("after restore the fade must finish at 6720 samples, got switching=%v position=%d", clk.Switching, clk.Position)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(2400); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := s.Restore([]byte("\x00not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must report ErrBadSnapshot, got %v", err)
	}
	clk := s.Clock()
	if clk.Device != "dev-a" || clk.Position != 2400 {
		t.Fatalf("failed restore touched live state: device=%q position=%d", clk.Device, clk.Position)
	}
	if err := s.Advance(600); err != nil {
		t.Fatalf("advance after a rejected restore: %v", err)
	}
	if got := s.Clock().Position; got != 3000 {
		t.Fatalf("position after a rejected restore = %d, want 3000", got)
	}
}

func TestConcurrentAdvanceKeepsTimelineExact(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := s.Advance(100); err != nil {
					t.Errorf("advance: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := s.Clock().Position; got != 16000 {
		t.Fatalf("position after 160 concurrent advances = %d, want 16000", got)
	}
	if got := s.Stats().Advanced; got != 16000 {
		t.Fatalf("advanced sample counter = %d, want 16000", got)
	}
}

func TestAdvanceRejectsNonPositiveSamples(t *testing.T) {
	s := gameC11New(gameC11Limits())
	if err := s.SetDevice("dev-a"); err != nil {
		t.Fatalf("select dev-a: %v", err)
	}
	if err := s.Advance(1000); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := s.Advance(0); !errors.Is(err, impl.ErrBadAdvance) {
		t.Fatalf("zero advance must report ErrBadAdvance, got %v", err)
	}
	if err := s.Advance(-500); !errors.Is(err, impl.ErrBadAdvance) {
		t.Fatalf("negative advance must report ErrBadAdvance, got %v", err)
	}
	if got := s.Clock().Position; got != 1000 {
		t.Fatalf("position = %d, want 1000 after the rejected advances", got)
	}
}
