// Package task owns the summon, mount and dismount lifecycle between a player
// and a mount: guided summoning, forced dismounts per zone, durability
// bookkeeping and a mount state that survives a restart.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	limits MountLimits
	now    time.Time
}

// NewMountController builds a controller whose clock starts at now.
func NewMountController(limits MountLimits, now time.Time) *MountController {
	return &MountController{limits: limits, now: now}
}

// RegisterMount records one summonable mount.
func (c *MountController) RegisterMount(spec MountSpec) error {
	return errNotImplemented
}

// Summon starts the guided summon of one mount for one player.
func (c *MountController) Summon(player, mountID string) (SummonID, error) {
	return "", errNotImplemented
}

// MountUp commits a finished summon and puts the player on the mount.
func (c *MountController) MountUp(player, mountID string) error {
	return errNotImplemented
}

// Dismount takes the player off the current mount.
func (c *MountController) Dismount(player, reason string) error {
	return errNotImplemented
}

// State projects the lifecycle of one player.
func (c *MountController) State(player string) (MountState, error) {
	return MountState{}, errNotImplemented
}

// SetZoneRule records the mount rules of one zone.
func (c *MountController) SetZoneRule(zone string, rule ZoneRule) error {
	return errNotImplemented
}

// EnterZone moves a player into a zone and applies that zone's rules.
func (c *MountController) EnterZone(player, zone string) ([]MountEvent, error) {
	return nil, errNotImplemented
}

// Advance moves the controller clock forward and resolves guides and cooldowns.
func (c *MountController) Advance(now time.Time) ([]MountEvent, error) {
	return nil, errNotImplemented
}

// Damage reduces the durability of the active instance of one mount.
func (c *MountController) Damage(mountID string, amount int) (Durability, error) {
	return Durability{}, errNotImplemented
}

// Interrupt cancels a guide or a ride because of the given cause.
func (c *MountController) Interrupt(player, cause string) ([]MountEvent, error) {
	return nil, errNotImplemented
}

// Events returns a copy of the per player event log, oldest first.
func (c *MountController) Events(player string) []MountEvent {
	return nil
}

// Snapshot serialises the whole controller state.
func (c *MountController) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *MountController) Restore(data []byte) error {
	return errNotImplemented
}
