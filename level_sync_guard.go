// Package task owns the level sync guard: it suppresses player stats inside low
// level zones, restores the baseline on leave and keeps that state across a
// restart.
package task

import "errors"

var errNotImplemented = errors.New("not implemented")

// Errors reported by the level sync guard.
var (
	ErrBadZone     = errors.New("zone id is required")
	ErrUnknownZone = errors.New("unknown zone")
	ErrBadCap      = errors.New("zone level cap must be positive")
	ErrBadRule     = errors.New("sync rule is out of range")
	ErrBadStats    = errors.New("stats must not be negative")
	ErrBadLevel    = errors.New("player level must be positive")
	ErrNotSynced   = errors.New("player is not level synced")
	ErrLimit       = errors.New("level sync table limit reached")
	ErrBadSnapshot = errors.New("snapshot payload is invalid")
)

// Stats is one stat block.
type Stats struct {
	HP      int
	Attack  int
	Defense int
	Speed   int
}

// BaseStats is the stat block captured when a player enters a zone.
type BaseStats = Stats

// SyncRule is the suppression curve applied inside a capped zone. Every level
// above the cap removes PercentPerLevel points of the stat block, and the total
// removal never passes 100 minus MinPercent.
type SyncRule struct {
	PercentPerLevel int
	MinPercent      int
}

// SyncState is the durable sync state of one player.
type SyncState struct {
	Player             string
	Zone               string
	Active             bool
	PlayerLevel        int
	ZoneCap            int
	Base               Stats
	GearLevel          int
	EffectiveGearLevel int
	Gear               Stats
	Buff               Stats
	Seq                int64
}

// Limits bounds the guard tables.
type Limits struct {
	MaxPlayers int
	MaxZones   int
}

// LevelSyncGuard owns zone caps, the sync rule, per player baselines and the
// active suppression state.
type LevelSyncGuard struct {
	limits Limits
}

// NewLevelSyncGuard builds a guard with the given caps.
func NewLevelSyncGuard(limits Limits) *LevelSyncGuard {
	return &LevelSyncGuard{limits: limits}
}

// SetZoneCap registers the level cap of one zone.
func (g *LevelSyncGuard) SetZoneCap(zone string, level int) error {
	return errNotImplemented
}

// SetSyncRule replaces the suppression curve.
func (g *LevelSyncGuard) SetSyncRule(rule SyncRule) error {
	return errNotImplemented
}

// SetPlayerLevel stores the current level of one player.
func (g *LevelSyncGuard) SetPlayerLevel(player string, level int) error {
	return errNotImplemented
}

// SetBaseStats stores the unsuppressed stat block of one player.
func (g *LevelSyncGuard) SetBaseStats(player string, stats Stats) error {
	return errNotImplemented
}

// SetGear stores the equipment level and its per level stat bonus.
func (g *LevelSyncGuard) SetGear(player string, level int, perLevel Stats) error {
	return errNotImplemented
}

// AddBuff adds one relative stat delta to a player.
func (g *LevelSyncGuard) AddBuff(player string, delta Stats) error {
	return errNotImplemented
}

// Enter puts the player into a zone and returns the captured baseline.
func (g *LevelSyncGuard) Enter(player string, zone string) (BaseStats, error) {
	return BaseStats{}, errNotImplemented
}

// Leave restores the player to the unsuppressed stats.
func (g *LevelSyncGuard) Leave(player string) error {
	return errNotImplemented
}

// Effective resolves the stat block the player fights with right now.
func (g *LevelSyncGuard) Effective(player string) (Stats, error) {
	return Stats{}, errNotImplemented
}

// State reads the durable sync state of one player.
func (g *LevelSyncGuard) State(player string) (SyncState, error) {
	return SyncState{}, errNotImplemented
}

// Snapshot serialises zone caps, the sync rule, baselines and active state.
func (g *LevelSyncGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *LevelSyncGuard) Restore(data []byte) error {
	return errNotImplemented
}
