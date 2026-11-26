// Package task owns the level sync guard: it suppresses player stats inside low
// level zones, restores the baseline on leave and keeps that state across a
// restart.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

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

const snapshotVersion = 1

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

type playerState struct {
	level        int
	base         Stats
	gearLevel    int
	gearPerLevel Stats
	buff         Stats
	active       bool
	zone         string
	seq          int64
}

// LevelSyncGuard owns zone caps, the sync rule, per player baselines and the
// active suppression state.
type LevelSyncGuard struct {
	mu      sync.RWMutex
	limits  Limits
	zones   map[string]int
	rule    SyncRule
	players map[string]*playerState
}

// NewLevelSyncGuard builds a guard with the given caps.
func NewLevelSyncGuard(limits Limits) *LevelSyncGuard {
	return &LevelSyncGuard{
		limits:  limits,
		zones:   make(map[string]int),
		players: make(map[string]*playerState),
	}
}

// SetZoneCap registers the level cap of one zone.
func (g *LevelSyncGuard) SetZoneCap(zone string, level int) error {
	if zone == "" {
		return ErrBadZone
	}
	if level <= 0 {
		return ErrBadCap
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.zones[zone]; !exists && g.zoneLimitReached() {
		return ErrLimit
	}
	g.zones[zone] = level
	g.bumpActiveInZone(zone)
	return nil
}

// SetSyncRule replaces the suppression curve.
func (g *LevelSyncGuard) SetSyncRule(rule SyncRule) error {
	if !validRule(rule) {
		return ErrBadRule
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.rule = rule
	for _, player := range g.players {
		if player.active {
			player.seq++
		}
	}
	return nil
}

// SetPlayerLevel stores the current level of one player.
func (g *LevelSyncGuard) SetPlayerLevel(player string, level int) error {
	if player == "" {
		return ErrBadZone
	}
	if level <= 0 {
		return ErrBadLevel
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, err := g.ensurePlayer(player)
	if err != nil {
		return err
	}
	state.level = level
	state.seq++
	return nil
}

// SetBaseStats stores the unsuppressed stat block of one player.
func (g *LevelSyncGuard) SetBaseStats(player string, stats Stats) error {
	if player == "" {
		return ErrBadZone
	}
	if !validStats(stats) {
		return ErrBadStats
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, err := g.ensurePlayer(player)
	if err != nil {
		return err
	}
	state.base = stats
	state.seq++
	return nil
}

// SetGear stores the equipment level and its per level stat bonus.
func (g *LevelSyncGuard) SetGear(player string, level int, perLevel Stats) error {
	if player == "" {
		return ErrBadZone
	}
	if level < 0 || !validStats(perLevel) {
		return ErrBadStats
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, err := g.ensurePlayer(player)
	if err != nil {
		return err
	}
	state.gearLevel = level
	state.gearPerLevel = perLevel
	state.seq++
	return nil
}

// AddBuff adds one relative stat delta to a player.
func (g *LevelSyncGuard) AddBuff(player string, delta Stats) error {
	if player == "" {
		return ErrBadZone
	}
	if !validStats(delta) {
		return ErrBadStats
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, err := g.ensurePlayer(player)
	if err != nil {
		return err
	}
	state.buff = addStats(state.buff, delta)
	state.seq++
	return nil
}

// Enter puts the player into a zone and returns the captured baseline.
func (g *LevelSyncGuard) Enter(player string, zone string) (BaseStats, error) {
	if player == "" || zone == "" {
		return BaseStats{}, ErrBadZone
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.zones[zone]; !exists {
		return BaseStats{}, ErrUnknownZone
	}

	state, exists := g.players[player]
	if !exists || state.level <= 0 {
		return BaseStats{}, ErrBadLevel
	}

	state.active = true
	state.zone = zone
	state.seq++
	return state.base, nil
}

// Leave restores the player to the unsuppressed stats.
func (g *LevelSyncGuard) Leave(player string) error {
	if player == "" {
		return ErrBadZone
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, exists := g.players[player]
	if !exists || !state.active {
		return ErrNotSynced
	}
	state.active = false
	state.zone = ""
	state.seq++
	return nil
}

// Effective resolves the stat block the player fights with right now.
func (g *LevelSyncGuard) Effective(player string) (Stats, error) {
	if player == "" {
		return Stats{}, ErrBadZone
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	state, exists := g.players[player]
	if !exists {
		return Stats{}, nil
	}
	return g.effectiveLocked(state), nil
}

// State reads the durable sync state of one player.
func (g *LevelSyncGuard) State(player string) (SyncState, error) {
	if player == "" {
		return SyncState{}, ErrBadZone
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	state, exists := g.players[player]
	if !exists {
		return SyncState{Player: player}, nil
	}
	return g.syncStateLocked(player, state), nil
}

type snapshotPlayer struct {
	Level        int    `json:"level"`
	Base         Stats  `json:"base"`
	GearLevel    int    `json:"gear_level"`
	GearPerLevel Stats  `json:"gear_per_level"`
	Buff         Stats  `json:"buff"`
	Active       bool   `json:"active"`
	Zone         string `json:"zone"`
	Seq          int64  `json:"seq"`
}

type snapshotData struct {
	Version int                       `json:"version"`
	Zones   map[string]int            `json:"zones"`
	Rule    SyncRule                  `json:"rule"`
	Players map[string]snapshotPlayer `json:"players"`
}

// Snapshot serialises zone caps, the sync rule, baselines and active state.
func (g *LevelSyncGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	data := snapshotData{
		Version: snapshotVersion,
		Zones:   make(map[string]int, len(g.zones)),
		Rule:    g.rule,
		Players: make(map[string]snapshotPlayer, len(g.players)),
	}
	for zone, cap := range g.zones {
		data.Zones[zone] = cap
	}
	for player, state := range g.players {
		data.Players[player] = snapshotPlayer{
			Level:        state.level,
			Base:         state.base,
			GearLevel:    state.gearLevel,
			GearPerLevel: state.gearPerLevel,
			Buff:         state.buff,
			Active:       state.active,
			Zone:         state.zone,
			Seq:          state.seq,
		}
	}
	return json.Marshal(data)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *LevelSyncGuard) Restore(data []byte) error {
	var snapshot snapshotData
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ErrBadSnapshot
	}
	if err := ensureSnapshotEOF(decoder); err != nil {
		return ErrBadSnapshot
	}
	if snapshot.Version != snapshotVersion || !validRule(snapshot.Rule) {
		return ErrBadSnapshot
	}
	if snapshot.Zones == nil || snapshot.Players == nil {
		return ErrBadSnapshot
	}

	g.mu.RLock()
	maxPlayers, maxZones := g.limits.MaxPlayers, g.limits.MaxZones
	g.mu.RUnlock()
	if exceedsLimit(len(snapshot.Zones), maxZones) || exceedsLimit(len(snapshot.Players), maxPlayers) {
		return ErrLimit
	}

	zones := make(map[string]int, len(snapshot.Zones))
	for zone, cap := range snapshot.Zones {
		if zone == "" || cap <= 0 {
			return ErrBadSnapshot
		}
		zones[zone] = cap
	}

	players := make(map[string]*playerState, len(snapshot.Players))
	for player, saved := range snapshot.Players {
		if player == "" || saved.Level < 0 || saved.GearLevel < 0 || saved.Seq < 0 ||
			!validStats(saved.Base) || !validStats(saved.GearPerLevel) || !validStats(saved.Buff) {
			return ErrBadSnapshot
		}
		if saved.Active {
			if saved.Level <= 0 || saved.Zone == "" {
				return ErrBadSnapshot
			}
			if _, exists := zones[saved.Zone]; !exists {
				return ErrBadSnapshot
			}
		} else if saved.Zone != "" {
			return ErrBadSnapshot
		}
		players[player] = &playerState{
			level:        saved.Level,
			base:         saved.Base,
			gearLevel:    saved.GearLevel,
			gearPerLevel: saved.GearPerLevel,
			buff:         saved.Buff,
			active:       saved.Active,
			zone:         saved.Zone,
			seq:          saved.Seq,
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.zones = zones
	g.rule = snapshot.Rule
	g.players = players
	return nil
}

func (g *LevelSyncGuard) ensurePlayer(player string) (*playerState, error) {
	if state, exists := g.players[player]; exists {
		return state, nil
	}
	if g.playerLimitReached() {
		return nil, ErrLimit
	}
	state := &playerState{}
	g.players[player] = state
	return state, nil
}

func (g *LevelSyncGuard) playerLimitReached() bool {
	return exceedsLimit(len(g.players), g.limits.MaxPlayers)
}

func (g *LevelSyncGuard) zoneLimitReached() bool {
	return exceedsLimit(len(g.zones), g.limits.MaxZones)
}

func (g *LevelSyncGuard) bumpActiveInZone(zone string) {
	for _, player := range g.players {
		if player.active && player.zone == zone {
			player.seq++
		}
	}
}

func (g *LevelSyncGuard) effectiveLocked(state *playerState) Stats {
	gearLevel := state.gearLevel
	zoneCap := 0
	if state.active {
		zoneCap = g.zones[state.zone]
		if gearLevel > zoneCap {
			gearLevel = zoneCap
		}
	}

	total := addStats(addStats(state.base, gearBonus(state.gearPerLevel, gearLevel)), state.buff)
	if !state.active || state.level <= zoneCap || g.rule.PercentPerLevel == 0 {
		return total
	}

	reduction := int64(state.level-zoneCap) * int64(g.rule.PercentPerLevel)
	maxReduction := int64(100 - g.rule.MinPercent)
	if reduction > maxReduction {
		reduction = maxReduction
	}
	return scaleStats(total, int(100-reduction))
}

func (g *LevelSyncGuard) syncStateLocked(player string, state *playerState) SyncState {
	zoneCap := 0
	gearLevel := state.gearLevel
	if state.active {
		zoneCap = g.zones[state.zone]
		if gearLevel > zoneCap {
			gearLevel = zoneCap
		}
	}
	return SyncState{
		Player:             player,
		Zone:               state.zone,
		Active:             state.active,
		PlayerLevel:        state.level,
		ZoneCap:            zoneCap,
		Base:               state.base,
		GearLevel:          state.gearLevel,
		EffectiveGearLevel: gearLevel,
		Gear:               gearBonus(state.gearPerLevel, gearLevel),
		Buff:               state.buff,
		Seq:                state.seq,
	}
}

func validRule(rule SyncRule) bool {
	return rule.PercentPerLevel >= 0 && rule.PercentPerLevel <= 100 &&
		rule.MinPercent >= 0 && rule.MinPercent <= 100
}

func validStats(stats Stats) bool {
	return stats.HP >= 0 && stats.Attack >= 0 && stats.Defense >= 0 && stats.Speed >= 0
}

func addStats(left, right Stats) Stats {
	return Stats{
		HP:      left.HP + right.HP,
		Attack:  left.Attack + right.Attack,
		Defense: left.Defense + right.Defense,
		Speed:   left.Speed + right.Speed,
	}
}

func scaleStats(stats Stats, percent int) Stats {
	return Stats{
		HP:      stats.HP * percent / 100,
		Attack:  stats.Attack * percent / 100,
		Defense: stats.Defense * percent / 100,
		Speed:   stats.Speed * percent / 100,
	}
}

func gearBonus(perLevel Stats, level int) Stats {
	return Stats{
		HP:      perLevel.HP * level,
		Attack:  perLevel.Attack * level,
		Defense: perLevel.Defense * level,
		Speed:   perLevel.Speed * level,
	}
}

func exceedsLimit(size, limit int) bool {
	return limit > 0 && size >= limit
}

func ensureSnapshotEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("trailing snapshot data")
}
