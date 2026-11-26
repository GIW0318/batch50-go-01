// Package task takes audio playback over when the default output device
// changes underneath a running session: it plans the handover, fades the old
// device out, rebuilds the stream at the new device rate, fades it back in and
// keeps the timeline counting played samples instead of wall clock time.
package task

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

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
	mu                sync.Mutex
	projectRate       int
	fade              time.Duration
	maxDevices        int
	maxSwitches       int
	devices           []Device
	active            string
	rate              int
	position          int64
	played            int64
	switching         bool
	fadeRemainSamples int64
	planSeq           int
	switches          int
	pending           *Plan
	stats             SwitchStats
}

// NewAudioDeviceSwitcher registers the device table and the caps.
func NewAudioDeviceSwitcher(devices []Device, limits Limits) *AudioDeviceSwitcher {
	s := &AudioDeviceSwitcher{
		projectRate: limits.ProjectRate,
		fade:        limits.Fade,
		maxDevices:  limits.MaxDevices,
		maxSwitches: limits.MaxSwitches,
	}
	if s.projectRate == 0 {
		s.projectRate = DefaultProjectRate
	}
	if s.maxDevices == 0 {
		s.maxDevices = DefaultMaxDevices
	}
	if s.maxSwitches == 0 {
		s.maxSwitches = DefaultMaxSwitches
	}
	for _, d := range devices {
		if s.maxDevices > 0 && len(s.devices) >= s.maxDevices {
			break
		}
		s.devices = append(s.devices, d)
	}
	return s
}

// findDevice locates a registered endpoint by id.
func (s *AudioDeviceSwitcher) findDevice(id string) (Device, bool) {
	for _, d := range s.devices {
		if d.ID == id {
			return d, true
		}
	}
	return Device{}, false
}

// pickRate opens the device at the project rate when supported and falls
// back to the driver preferred rate otherwise.
func (s *AudioDeviceSwitcher) pickRate(d Device) int {
	for _, r := range d.Rates {
		if r == s.projectRate {
			return r
		}
	}
	if d.Preferred > 0 {
		return d.Preferred
	}
	if len(d.Rates) > 0 {
		return d.Rates[0]
	}
	return s.projectRate
}

// SetDevice picks a device straight away while nothing is playing.
func (s *AudioDeviceSwitcher) SetDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.findDevice(id)
	if !ok {
		s.stats.Rejected++
		return ErrUnknownDevice
	}
	if !d.Available {
		s.stats.Rejected++
		return ErrDeviceUnavailable
	}
	if id == s.active {
		s.stats.Rejected++
		return ErrSameDevice
	}
	if s.played > 0 || s.pending != nil || s.switching {
		s.stats.Rejected++
		return ErrBusy
	}
	s.active = id
	s.rate = s.pickRate(d)
	return nil
}

// Devices lists the registered endpoints in registration order.
func (s *AudioDeviceSwitcher) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, len(s.devices))
	copy(out, s.devices)
	return out
}

// Switch plans a handover to the target device.
func (s *AudioDeviceSwitcher) Switch(id string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.findDevice(id)
	if !ok {
		s.stats.Rejected++
		return Plan{}, ErrUnknownDevice
	}
	if !d.Available {
		s.stats.Rejected++
		return Plan{}, ErrDeviceUnavailable
	}
	if id == s.active {
		s.stats.Rejected++
		return Plan{}, ErrSameDevice
	}
	if s.switches >= s.maxSwitches {
		s.stats.Rejected++
		return Plan{}, ErrSwitchLimit
	}
	toRate := s.pickRate(d)
	s.planSeq++
	plan := Plan{
		Index:      s.planSeq,
		FromDevice: s.active,
		ToDevice:   id,
		FromRate:   s.rate,
		ToRate:     toRate,
		AtSample:   s.played,
		Fade:       s.fade,
		Resample:   toRate != s.rate,
	}
	s.pending = &plan
	s.stats.Plans++
	return plan, nil
}

// Commit applies the pending plan and starts the fade sequence.
func (s *AudioDeviceSwitcher) Commit(plan Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.stats.Rejected++
		return ErrNoPlan
	}
	if plan.Index != s.pending.Index {
		s.stats.Rejected++
		return ErrBadPlan
	}
	p := *s.pending
	s.pending = nil
	fromRate := s.rate
	s.active = p.ToDevice
	s.rate = p.ToRate
	if fromRate > 0 && p.ToRate > 0 && p.ToRate != fromRate {
		s.position = s.position * int64(p.ToRate) / int64(fromRate)
	}
	s.switches++
	s.stats.Switches++
	s.fadeRemainSamples = int64(p.Fade) * int64(s.rate) / int64(time.Second)
	s.switching = s.fadeRemainSamples > 0
	return nil
}

// Pending reports the plan waiting for Commit.
func (s *AudioDeviceSwitcher) Pending() (Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return Plan{}, false
	}
	return *s.pending, true
}

// SetFade changes the fade duration used by later plans.
func (s *AudioDeviceSwitcher) SetFade(d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d < 0 || d > MaxFadeDuration {
		s.stats.Rejected++
		return ErrBadFade
	}
	s.fade = d
	return nil
}

// Advance moves the timeline by the number of samples actually played.
func (s *AudioDeviceSwitcher) Advance(samples int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if samples <= 0 {
		s.stats.Rejected++
		return ErrBadAdvance
	}
	s.position += samples
	rate := s.rate
	if rate <= 0 {
		rate = s.projectRate
	}
	s.played += samples * int64(s.projectRate) / int64(rate)
	if s.switching {
		s.fadeRemainSamples -= samples
		if s.fadeRemainSamples <= 0 {
			s.fadeRemainSamples = 0
			s.switching = false
		}
	}
	s.stats.Advanced += samples
	return nil
}

// Clock reads the timeline of the active endpoint.
func (s *AudioDeviceSwitcher) Clock() DeviceClock {
	s.mu.Lock()
	defer s.mu.Unlock()
	clk := DeviceClock{
		Device:    s.active,
		Rate:      s.rate,
		Position:  s.position,
		Played:    s.played,
		Switching: s.switching,
		Switches:  s.switches,
	}
	if s.projectRate > 0 {
		clk.Elapsed = time.Duration(s.played/int64(s.projectRate))*time.Second +
			time.Duration(s.played%int64(s.projectRate))*time.Second/time.Duration(s.projectRate)
	}
	if s.rate > 0 && s.fadeRemainSamples > 0 {
		clk.FadeRemain = time.Duration(s.fadeRemainSamples) * time.Second / time.Duration(s.rate)
	}
	return clk
}

// DropDevice removes an endpoint that disappeared from the system.
func (s *AudioDeviceSwitcher) DropDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, d := range s.devices {
		if d.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.stats.Rejected++
		return ErrUnknownDevice
	}
	s.devices = append(s.devices[:idx], s.devices[idx+1:]...)
	s.stats.Dropped++
	if s.pending != nil && s.pending.ToDevice == id {
		s.pending = nil
	}
	if s.active != id {
		return nil
	}
	s.switching = false
	s.fadeRemainSamples = 0
	for k := 0; k < len(s.devices); k++ {
		d := s.devices[(idx+k)%len(s.devices)]
		if !d.Available {
			continue
		}
		newRate := s.pickRate(d)
		if s.rate > 0 && newRate > 0 && newRate != s.rate {
			s.position = s.position * int64(newRate) / int64(s.rate)
		}
		s.active = d.ID
		s.rate = newRate
		return nil
	}
	s.active = ""
	s.rate = 0
	return nil
}

// Stats reads the lifetime counters.
func (s *AudioDeviceSwitcher) Stats() SwitchStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// snapshotVersion tags the payload layout written by Snapshot.
const snapshotVersion = 1

// snapshotState is the serialisable view of the whole switcher state.
type snapshotState struct {
	Version           int           `json:"version"`
	ProjectRate       int           `json:"project_rate"`
	Fade              time.Duration `json:"fade"`
	MaxDevices        int           `json:"max_devices"`
	MaxSwitches       int           `json:"max_switches"`
	Devices           []Device      `json:"devices"`
	Active            string        `json:"active"`
	Rate              int           `json:"rate"`
	Position          int64         `json:"position"`
	Played            int64         `json:"played"`
	Switching         bool          `json:"switching"`
	FadeRemainSamples int64         `json:"fade_remain_samples"`
	PlanSeq           int           `json:"plan_seq"`
	Switches          int           `json:"switches"`
	Pending           *Plan         `json:"pending"`
	Stats             SwitchStats   `json:"stats"`
}

// Snapshot serialises the device table, the timeline and the plan state.
func (s *AudioDeviceSwitcher) Snapshot() ([]byte, error) {
	s.mu.Lock()
	state := snapshotState{
		Version:           snapshotVersion,
		ProjectRate:       s.projectRate,
		Fade:              s.fade,
		MaxDevices:        s.maxDevices,
		MaxSwitches:       s.maxSwitches,
		Devices:           append([]Device(nil), s.devices...),
		Active:            s.active,
		Rate:              s.rate,
		Position:          s.position,
		Played:            s.played,
		Switching:         s.switching,
		FadeRemainSamples: s.fadeRemainSamples,
		PlanSeq:           s.planSeq,
		Switches:          s.switches,
		Stats:             s.stats,
	}
	if s.pending != nil {
		p := *s.pending
		state.Pending = &p
	}
	s.mu.Unlock()
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *AudioDeviceSwitcher) Restore(data []byte) error {
	var state snapshotState
	if err := json.Unmarshal(data, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.Version != snapshotVersion || state.ProjectRate <= 0 {
		return ErrBadSnapshot
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectRate = state.ProjectRate
	s.fade = state.Fade
	s.maxDevices = state.MaxDevices
	s.maxSwitches = state.MaxSwitches
	s.devices = append([]Device(nil), state.Devices...)
	s.active = state.Active
	s.rate = state.Rate
	s.position = state.Position
	s.played = state.Played
	s.switching = state.Switching
	s.fadeRemainSamples = state.FadeRemainSamples
	s.planSeq = state.PlanSeq
	s.switches = state.Switches
	s.stats = state.Stats
	if state.Pending != nil {
		p := *state.Pending
		s.pending = &p
	} else {
		s.pending = nil
	}
	return nil
}
