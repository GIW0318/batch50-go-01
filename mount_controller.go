// Package task owns the summon, mount and dismount lifecycle between a player
// and a mount: guided summoning, forced dismounts per zone, durability
// bookkeeping and a mount state that survives a restart.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SummonID identifies one summon attempt of one mount.
type SummonID string

// Mount phases reported by State.
const (
	PhaseIdle      = "idle"
	PhaseSummoning = "summoning"
	PhaseReady     = "ready"
	PhaseMounted   = "mounted"
	PhaseCooldown  = "cooldown"
)

// Reasons attached to dismount and void events.
const (
	ReasonPlayerRequest  = "player_request"
	ReasonZoneForbidden  = "zone_forbidden"
	ReasonDurabilityZero = "durability_zero"
	ReasonKnockedDown    = "knocked_down"
)

// Event kinds appended to the per player log.
const (
	EventSummonStarted = "summon_started"
	EventSummonReady   = "summon_ready"
	EventSummonVoided  = "summon_voided"
	EventMountUp       = "mount_up"
	EventDismount      = "dismount"
	EventMountBroken   = "mount_broken"
	EventCooldownEnd   = "cooldown_end"
)

// Errors returned by the controller.
var (
	ErrBlankPlayer       = errors.New("player id is required")
	ErrBadReason         = errors.New("reason is required")
	ErrBadMountSpec      = errors.New("mount spec is invalid")
	ErrMountExists       = errors.New("mount is already registered")
	ErrMountLimit        = errors.New("mount registry limit reached")
	ErrUnknownMount      = errors.New("unknown mount")
	ErrMountNotOwned     = errors.New("mount belongs to another player")
	ErrSummonPending     = errors.New("another summon is already in progress")
	ErrAlreadyMounted    = errors.New("player is already mounted")
	ErrGuideIncomplete   = errors.New("summon guide is not finished yet")
	ErrNoSummon          = errors.New("no pending summon for this mount")
	ErrNotMounted        = errors.New("player is not mounted")
	ErrZoneForbidsSummon = errors.New("zone forbids summoning")
	ErrBadZone           = errors.New("zone id is required")
	ErrCooldownActive    = errors.New("summon cooldown is still running")
	ErrBadAmount         = errors.New("damage amount must be positive")
	ErrTimeWentBackwards = errors.New("clock moved backwards")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// MountSpec is the static description of one summonable mount.
type MountSpec struct {
	ID             string `json:"id"`
	Owner          string `json:"owner"`
	MaxDurability  int    `json:"max_durability"`
	GuideMillis    int64  `json:"guide_millis"`
	CooldownMillis int64  `json:"cooldown_millis"`
}

// ZoneRule marks a zone as closed for mounts, for summoning, or both.
type ZoneRule struct {
	Zone         string `json:"zone"`
	ForbidMount  bool   `json:"forbid_mount"`
	ForbidSummon bool   `json:"forbid_summon"`
}

// Durability is the durability reading of one mount instance.
type Durability struct {
	MountID string `json:"mount_id"`
	Current int    `json:"current"`
	Max     int    `json:"max"`
	Broken  bool   `json:"broken"`
}

// MountState is the per player projection of the controller.
type MountState struct {
	Player         string   `json:"player"`
	MountID        string   `json:"mount_id"`
	Phase          string   `json:"phase"`
	Summon         SummonID `json:"summon"`
	Zone           string   `json:"zone"`
	Durability     int      `json:"durability"`
	MaxDurability  int      `json:"max_durability"`
	GuideLeftMs    int64    `json:"guide_left_ms"`
	CooldownLeftMs int64    `json:"cooldown_left_ms"`
	Seq            int64    `json:"seq"`
}

// MountEvent is one entry of the per player audit log.
type MountEvent struct {
	Player  string   `json:"player"`
	MountID string   `json:"mount_id"`
	Summon  SummonID `json:"summon"`
	Kind    string   `json:"kind"`
	Reason  string   `json:"reason"`
	Seq     int64    `json:"seq"`
}

// MountLimits bounds the registry and the per player event log.
type MountLimits struct {
	MaxMounts int `json:"max_mounts"`
	MaxEvents int `json:"max_events"`
}

// MountController owns the mount registry, the per player lifecycle and the
// event log. All exported methods are safe for concurrent use.
type MountController struct {
	mu            sync.Mutex
	limits        MountLimits
	now           time.Time
	mounts        map[string]MountSpec
	zones         map[string]ZoneRule
	players       map[string]*playerState
	nextSummonSeq int64
}

// NewMountController builds a controller whose clock starts at now.
func NewMountController(limits MountLimits, now time.Time) *MountController {
	return &MountController{
		limits:  limits,
		now:     now,
		mounts:  make(map[string]MountSpec),
		zones:   make(map[string]ZoneRule),
		players: make(map[string]*playerState),
	}
}

type playerState struct {
	Player         string       `json:"player"`
	Zone           string       `json:"zone"`
	Phase          string       `json:"phase"`
	MountID        string       `json:"mount_id"`
	Summon         SummonID     `json:"summon"`
	GuideLeftMs    int64        `json:"guide_left_ms"`
	CooldownLeftMs int64        `json:"cooldown_left_ms"`
	Durability     int          `json:"durability"`
	MaxDurability  int          `json:"max_durability"`
	Events         []MountEvent `json:"events"`
	Seq            int64        `json:"seq"`
}

type snapshotData struct {
	Version       int                     `json:"version"`
	Now           time.Time               `json:"now"`
	Limits        MountLimits             `json:"limits"`
	NextSummonSeq int64                   `json:"next_summon_seq"`
	Mounts        map[string]MountSpec    `json:"mounts"`
	Zones         map[string]ZoneRule     `json:"zones"`
	Players       map[string]*playerState `json:"players"`
}

const snapshotVersion = 1

func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func validateMountSpec(spec MountSpec) error {
	if isBlank(spec.ID) || spec.MaxDurability <= 0 || spec.GuideMillis < 0 || spec.CooldownMillis < 0 {
		return ErrBadMountSpec
	}
	return nil
}

func (c *MountController) ensureMapsLocked() {
	if c.mounts == nil {
		c.mounts = make(map[string]MountSpec)
	}
	if c.zones == nil {
		c.zones = make(map[string]ZoneRule)
	}
	if c.players == nil {
		c.players = make(map[string]*playerState)
	}
}

func (c *MountController) playerLocked(player string) *playerState {
	state, ok := c.players[player]
	if !ok || state == nil {
		state = &playerState{Player: player, Phase: PhaseIdle}
		c.players[player] = state
	}
	if state.Phase == "" {
		state.Phase = PhaseIdle
	}
	return state
}

func (c *MountController) appendEventLocked(state *playerState, kind, mountID string, summon SummonID, reason string) MountEvent {
	state.Seq++
	event := MountEvent{
		Player:  state.Player,
		MountID: mountID,
		Summon:  summon,
		Kind:    kind,
		Reason:  reason,
		Seq:     state.Seq,
	}
	state.Events = append(state.Events, event)

	limit := c.limits.MaxEvents
	if limit < 0 {
		limit = 0
	}
	if len(state.Events) > limit {
		if limit == 0 {
			state.Events = nil
		} else {
			kept := make([]MountEvent, limit)
			copy(kept, state.Events[len(state.Events)-limit:])
			state.Events = kept
		}
	}
	return event
}

func (state *playerState) toMountState() MountState {
	phase := state.Phase
	if phase == "" {
		phase = PhaseIdle
	}
	return MountState{
		Player:         state.Player,
		MountID:        state.MountID,
		Phase:          phase,
		Summon:         state.Summon,
		Zone:           state.Zone,
		Durability:     state.Durability,
		MaxDurability:  state.MaxDurability,
		GuideLeftMs:    state.GuideLeftMs,
		CooldownLeftMs: state.CooldownLeftMs,
		Seq:            state.Seq,
	}
}

func hasDamageableInstance(phase string) bool {
	return phase == PhaseSummoning || phase == PhaseReady || phase == PhaseMounted
}

// RegisterMount records one summonable mount.
func (c *MountController) RegisterMount(spec MountSpec) error {
	if err := validateMountSpec(spec); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	if _, exists := c.mounts[spec.ID]; exists {
		return ErrMountExists
	}
	limit := c.limits.MaxMounts
	if limit < 0 {
		limit = 0
	}
	if len(c.mounts) >= limit {
		return ErrMountLimit
	}

	c.mounts[spec.ID] = spec
	return nil
}

// Summon starts the guided summon of one mount for one player.
func (c *MountController) Summon(player, mountID string) (SummonID, error) {
	if isBlank(player) {
		return "", ErrBlankPlayer
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	spec, exists := c.mounts[mountID]
	if !exists {
		return "", ErrUnknownMount
	}
	if spec.Owner != player {
		return "", ErrMountNotOwned
	}

	state := c.playerLocked(player)
	switch state.Phase {
	case PhaseMounted:
		return "", ErrAlreadyMounted
	case PhaseSummoning, PhaseReady:
		if state.MountID == mountID {
			return state.Summon, nil
		}
		return "", ErrSummonPending
	case PhaseCooldown:
		return "", ErrCooldownActive
	}

	if rule, ok := c.zones[state.Zone]; ok && rule.ForbidSummon {
		return "", ErrZoneForbidsSummon
	}

	c.nextSummonSeq++
	summonID := SummonID(fmt.Sprintf("summon-%d", c.nextSummonSeq))
	state.Phase = PhaseSummoning
	state.MountID = spec.ID
	state.Summon = summonID
	state.GuideLeftMs = spec.GuideMillis
	state.CooldownLeftMs = 0
	state.MaxDurability = spec.MaxDurability
	state.Durability = spec.MaxDurability
	c.appendEventLocked(state, EventSummonStarted, spec.ID, summonID, "")

	return summonID, nil
}

// MountUp commits a finished summon and puts the player on the mount.
func (c *MountController) MountUp(player, mountID string) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	spec, exists := c.mounts[mountID]
	if !exists {
		return ErrUnknownMount
	}
	if spec.Owner != player {
		return ErrMountNotOwned
	}

	state := c.playerLocked(player)
	if state.MountID != mountID || state.Summon == "" {
		return ErrNoSummon
	}

	switch state.Phase {
	case PhaseSummoning:
		return ErrGuideIncomplete
	case PhaseReady:
		state.Phase = PhaseMounted
		state.GuideLeftMs = 0
		c.appendEventLocked(state, EventMountUp, mountID, state.Summon, "")
		return nil
	case PhaseMounted:
		return nil
	default:
		return ErrNoSummon
	}
}

// Dismount takes the player off the current mount.
func (c *MountController) Dismount(player, reason string) error {
	if isBlank(reason) {
		return ErrBadReason
	}
	if isBlank(player) {
		return ErrBlankPlayer
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	state := c.playerLocked(player)
	if state.Phase != PhaseMounted {
		return ErrNotMounted
	}

	c.appendEventLocked(state, EventDismount, state.MountID, state.Summon, reason)
	state.Phase = PhaseIdle
	state.CooldownLeftMs = 0
	return nil
}

// State projects the lifecycle of one player.
func (c *MountController) State(player string) (MountState, error) {
	if isBlank(player) {
		return MountState{}, ErrBlankPlayer
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	state, ok := c.players[player]
	if !ok || state == nil {
		return MountState{Player: player, Phase: PhaseIdle}, nil
	}
	return state.toMountState(), nil
}

// SetZoneRule records the mount rules of one zone.
func (c *MountController) SetZoneRule(zone string, rule ZoneRule) error {
	if isBlank(zone) {
		return ErrBadZone
	}
	zone = strings.TrimSpace(zone)
	rule.Zone = zone

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()
	c.zones[zone] = rule
	return nil
}

// EnterZone moves a player into a zone and applies that zone's rules.
func (c *MountController) EnterZone(player, zone string) ([]MountEvent, error) {
	if isBlank(player) {
		return nil, ErrBlankPlayer
	}
	if isBlank(zone) {
		return nil, ErrBadZone
	}
	zone = strings.TrimSpace(zone)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	state := c.playerLocked(player)
	state.Zone = zone
	rule := c.zones[zone]

	if (state.Phase == PhaseSummoning || state.Phase == PhaseReady) &&
		(rule.ForbidSummon || rule.ForbidMount) {
		event := c.voidSummonLocked(state, ReasonZoneForbidden)
		return []MountEvent{event}, nil
	}
	if state.Phase == PhaseMounted && rule.ForbidMount {
		event := c.dismountLocked(state, ReasonZoneForbidden)
		return []MountEvent{event}, nil
	}
	return nil, nil
}

// Advance moves the controller clock forward and resolves guides and cooldowns.
func (c *MountController) Advance(now time.Time) ([]MountEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	if now.Before(c.now) {
		return nil, ErrTimeWentBackwards
	}
	elapsedMillis := now.Sub(c.now).Milliseconds()
	if elapsedMillis < 0 {
		elapsedMillis = 0
	}
	c.now = now

	players := make([]string, 0, len(c.players))
	for player := range c.players {
		players = append(players, player)
	}
	sort.Strings(players)

	var events []MountEvent
	for _, player := range players {
		state := c.players[player]
		if state == nil {
			continue
		}

		switch state.Phase {
		case PhaseSummoning:
			state.GuideLeftMs -= elapsedMillis
			if state.GuideLeftMs <= 0 {
				state.GuideLeftMs = 0
				state.Phase = PhaseReady
				events = append(events, c.appendEventLocked(
					state, EventSummonReady, state.MountID, state.Summon, "",
				))
			}
		case PhaseCooldown:
			state.CooldownLeftMs -= elapsedMillis
			if state.CooldownLeftMs <= 0 {
				state.CooldownLeftMs = 0
				state.Phase = PhaseIdle
				events = append(events, c.appendEventLocked(
					state, EventCooldownEnd, state.MountID, state.Summon, "",
				))
			}
		}
	}
	return events, nil
}

func (c *MountController) voidSummonLocked(state *playerState, reason string) MountEvent {
	event := c.appendEventLocked(
		state, EventSummonVoided, state.MountID, state.Summon, reason,
	)
	state.Phase = PhaseIdle
	state.GuideLeftMs = 0
	state.CooldownLeftMs = 0
	state.Summon = ""
	state.MountID = ""
	state.Durability = state.MaxDurability
	return event
}

func (c *MountController) dismountLocked(state *playerState, reason string) MountEvent {
	event := c.appendEventLocked(
		state, EventDismount, state.MountID, state.Summon, reason,
	)
	state.Phase = PhaseIdle
	state.CooldownLeftMs = 0
	return event
}

// Damage reduces the durability of the active instance of one mount.
func (c *MountController) Damage(mountID string, amount int) (Durability, error) {
	if amount <= 0 {
		return Durability{}, ErrBadAmount
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	spec, exists := c.mounts[mountID]
	if !exists {
		return Durability{}, ErrNoSummon
	}
	state := c.players[spec.Owner]
	if state == nil || state.MountID != mountID || !hasDamageableInstance(state.Phase) {
		return Durability{}, ErrNoSummon
	}

	current := state.Durability
	if amount >= current {
		current = 0
	} else {
		current -= amount
	}
	state.Durability = current

	broken := current == 0
	if broken {
		state.Phase = PhaseCooldown
		state.GuideLeftMs = 0
		state.CooldownLeftMs = spec.CooldownMillis
		c.appendEventLocked(state, EventMountBroken, mountID, state.Summon, ReasonDurabilityZero)
	}

	return Durability{
		MountID: mountID,
		Current: current,
		Max:     spec.MaxDurability,
		Broken:  broken,
	}, nil
}

// Interrupt cancels a guide or a ride because of the given cause.
func (c *MountController) Interrupt(player, cause string) ([]MountEvent, error) {
	if isBlank(cause) {
		return nil, ErrBadReason
	}
	if isBlank(player) {
		return nil, ErrBlankPlayer
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	state, exists := c.players[player]
	if !exists || state == nil {
		return nil, nil
	}

	switch state.Phase {
	case PhaseSummoning, PhaseReady:
		return []MountEvent{c.voidSummonLocked(state, cause)}, nil
	case PhaseMounted:
		return []MountEvent{c.dismountLocked(state, cause)}, nil
	default:
		return nil, nil
	}
}

// Events returns a copy of the per player event log, oldest first.
func (c *MountController) Events(player string) []MountEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	state, exists := c.players[player]
	if !exists || state == nil || len(state.Events) == 0 {
		return []MountEvent{}
	}
	events := make([]MountEvent, len(state.Events))
	copy(events, state.Events)
	return events
}

// Snapshot serialises the whole controller state.
func (c *MountController) Snapshot() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureMapsLocked()

	payload := snapshotData{
		Version:       snapshotVersion,
		Now:           c.now,
		Limits:        c.limits,
		NextSummonSeq: c.nextSummonSeq,
		Mounts:        c.mounts,
		Zones:         c.zones,
		Players:       c.players,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *MountController) Restore(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return ErrBadSnapshot
	}

	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if err := validateSnapshot(&payload); err != nil {
		return ErrBadSnapshot
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = payload.Now
	c.limits = payload.Limits
	c.nextSummonSeq = payload.NextSummonSeq
	c.mounts = payload.Mounts
	c.zones = payload.Zones
	c.players = payload.Players
	c.ensureMapsLocked()

	for playerID, state := range c.players {
		if state == nil {
			state = &playerState{Player: playerID, Phase: PhaseIdle}
			c.players[playerID] = state
		}
		if state.Player == "" {
			state.Player = playerID
		}
		if state.Phase == "" {
			state.Phase = PhaseIdle
		}
	}
	return nil
}

func validateSnapshot(payload *snapshotData) error {
	if payload.Version != snapshotVersion || payload.NextSummonSeq < 0 {
		return ErrBadSnapshot
	}
	for id, spec := range payload.Mounts {
		if spec.ID == "" {
			spec.ID = id
		}
		if err := validateMountSpec(spec); err != nil {
			return ErrBadSnapshot
		}
	}
	for zone, rule := range payload.Zones {
		if isBlank(zone) {
			return ErrBadSnapshot
		}
		if rule.Zone == "" {
			rule.Zone = zone
		}
	}

	for playerID, state := range payload.Players {
		if isBlank(playerID) || state == nil || state.Seq < 0 ||
			state.GuideLeftMs < 0 || state.CooldownLeftMs < 0 || state.Durability < 0 {
			return ErrBadSnapshot
		}
		if state.Phase != PhaseIdle && state.Phase != PhaseSummoning &&
			state.Phase != PhaseReady && state.Phase != PhaseMounted &&
			state.Phase != PhaseCooldown {
			return ErrBadSnapshot
		}
		if len(state.Events) > 0 {
			previousSeq := int64(0)
			for _, event := range state.Events {
				if event.Seq <= previousSeq {
					return ErrBadSnapshot
				}
				previousSeq = event.Seq
			}
			if previousSeq > state.Seq {
				return ErrBadSnapshot
			}
		}
		if state.MountID != "" {
			spec, ok := payload.Mounts[state.MountID]
			if !ok || spec.Owner != playerID || state.MaxDurability != spec.MaxDurability ||
				state.Durability > spec.MaxDurability {
				return ErrBadSnapshot
			}
		}
	}
	return nil
}
