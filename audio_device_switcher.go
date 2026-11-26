// Package task takes audio playback over when the default output device
// changes underneath a running session: it plans the handover, fades the old
// device out, rebuilds the stream at the new device rate, fades it back in and
// keeps the timeline counting played samples instead of wall clock time.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults applied by NewAudioDeviceSwitcher when a field is left at zero.
const (
	DefaultProjectRate = 48000
	DefaultMaxDevices  = 8
	DefaultMaxSwitches = 32
)

// MaxFadeDuration is the largest fade accepted by SetFade.
const MaxFadeDuration = 2 * time.Second

// Errors reported by the switcher.
var (
	ErrUnknownDevice     = errors.New("unknown audio device")
	ErrDeviceUnavailable = errors.New("audio device is not available")
	ErrSameDevice        = errors.New("audio device is already active")
	ErrSwitchLimit       = errors.New("switch budget exhausted")
	ErrBusy              = errors.New("playback is running, use a switch plan")
	ErrBadFade           = errors.New("fade duration is out of range")
	ErrBadAdvance        = errors.New("advance must take a positive sample count")
	ErrNoPlan            = errors.New("no switch plan is pending")
	ErrBadPlan           = errors.New("switch plan does not match the pending one")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Device is one output endpoint known to the switcher. Rates lists the sample
// rates the endpoint can open in ascending order, Preferred is the rate the
// driver picks when the requested rate is not supported.
type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Rates     []int  `json:"rates"`
	Preferred int    `json:"preferred"`
	Available bool   `json:"available"`
}

// Limits carries the project rate and the hard caps of the switcher.
type Limits struct {
	ProjectRate int
	Fade        time.Duration
	MaxDevices  int
	MaxSwitches int
}

// Plan is one proposed handover from the active device to a target device.
// AtSample is the played sample count the handover starts from.
type Plan struct {
	Index      int           `json:"index"`
	FromDevice string        `json:"from_device"`
	ToDevice   string        `json:"to_device"`
	FromRate   int           `json:"from_rate"`
	ToRate     int           `json:"to_rate"`
	AtSample   int64         `json:"at_sample"`
	Fade       time.Duration `json:"fade"`
	Resample   bool          `json:"resample"`
}

// DeviceClock is the timeline as seen by the active endpoint.
type DeviceClock struct {
	Device     string        `json:"device"`
	Rate       int           `json:"rate"`
	Position   int64         `json:"position"`
	Played     int64         `json:"played"`
	Elapsed    time.Duration `json:"elapsed"`
	FadeRemain time.Duration `json:"fade_remain"`
	Switching  bool          `json:"switching"`
	Switches   int           `json:"switches"`
}

// SwitchStats counts what the switcher did over its lifetime.
type SwitchStats struct {
	Plans    int   `json:"plans"`
	Switches int   `json:"switches"`
	Dropped  int   `json:"dropped"`
	Advanced int64 `json:"advanced"`
	Rejected int   `json:"rejected"`
}

// AudioDeviceSwitcher owns the device table and the played sample timeline.
type AudioDeviceSwitcher struct {
	mu          sync.Mutex
	projectRate int
	fade        time.Duration
	maxDevices  int
	maxSwitches int
	devices     []Device
	active      string
	position    int64
	played      int64
	switching   bool
	fadeRemain  time.Duration
	planSeq     int
	switches    int
	pending     *Plan
	stats       SwitchStats
}

// NewAudioDeviceSwitcher registers the device table and the caps.
func NewAudioDeviceSwitcher(devices []Device, limits Limits) *AudioDeviceSwitcher {
	s := &AudioDeviceSwitcher{
		projectRate: limits.ProjectRate,
		fade:        limits.Fade,
		maxDevices:  limits.MaxDevices,
		maxSwitches: limits.MaxSwitches,
	}
	for _, d := range devices {
		if s.maxDevices > 0 && len(s.devices) >= s.maxDevices {
			break
		}
		s.devices = append(s.devices, d)
	}
	return s
}

// SetDevice picks a device straight away while nothing is playing.
func (s *AudioDeviceSwitcher) SetDevice(id string) error {
	return errNotImplemented
}

// Devices lists the registered endpoints in registration order.
func (s *AudioDeviceSwitcher) Devices() []Device {
	return nil
}

// Switch plans a handover to the target device.
func (s *AudioDeviceSwitcher) Switch(id string) (Plan, error) {
	return Plan{}, errNotImplemented
}

// Commit applies the pending plan and starts the fade sequence.
func (s *AudioDeviceSwitcher) Commit(plan Plan) error {
	return errNotImplemented
}

// Pending reports the plan waiting for Commit.
func (s *AudioDeviceSwitcher) Pending() (Plan, bool) {
	return Plan{}, false
}

// SetFade changes the fade duration used by later plans.
func (s *AudioDeviceSwitcher) SetFade(d time.Duration) error {
	return errNotImplemented
}

// Advance moves the timeline by the number of samples actually played.
func (s *AudioDeviceSwitcher) Advance(samples int64) error {
	return errNotImplemented
}

// Clock reads the timeline of the active endpoint.
func (s *AudioDeviceSwitcher) Clock() DeviceClock {
	return DeviceClock{}
}

// DropDevice removes an endpoint that disappeared from the system.
func (s *AudioDeviceSwitcher) DropDevice(id string) error {
	return errNotImplemented
}

// Stats reads the lifetime counters.
func (s *AudioDeviceSwitcher) Stats() SwitchStats {
	return SwitchStats{}
}

// Snapshot serialises the device table, the timeline and the plan state.
func (s *AudioDeviceSwitcher) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *AudioDeviceSwitcher) Restore(data []byte) error {
	return errNotImplemented
}
