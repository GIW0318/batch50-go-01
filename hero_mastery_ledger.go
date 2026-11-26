// Package task owns the hero mastery ledger: per match experience conversion,
// level promotion, milestone rewards and the per hero leaderboard.
package task

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
)

// Errors reported by the mastery ledger.
var (
	ErrBadIdentity     = errors.New("account id and hero id are required")
	ErrBadXP           = errors.New("raw experience must be positive")
	ErrBadMode         = errors.New("mode name is required")
	ErrUnknownMode     = errors.New("unknown game mode")
	ErrBadFactor       = errors.New("mode factor is out of range")
	ErrUnknownPair     = errors.New("unknown account and hero pair")
	ErrLevelNotReached = errors.New("mastery level has not been reached")
	ErrAlreadyClaimed  = errors.New("milestone reward already claimed")
	ErrBadLevel        = errors.New("milestone level must be at least two")
	ErrLimit           = errors.New("mastery table limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// maxProcessedBattles bounds the idempotency table so a long season cannot
// grow it without limit; the oldest battle ids are evicted first.
const maxProcessedBattles = 1 << 16

// Gain describes one experience award.
type Gain struct {
	Account      string
	Hero         string
	RawXP        int
	Effective    int
	LevelBefore  int
	LevelAfter   int
	XPAfter      int
	LevelsGained int
	Capped       bool
	Duplicate    bool
}

// MasteryLevel is the level view of one hero.
type MasteryLevel struct {
	Level  int
	XP     int
	NextXP int
}

// Progress is the durable mastery record of one account and hero pair.
type Progress struct {
	Account string
	Hero    string
	XP      int
	Level   int
	Claimed []int
}

// Reward is one milestone payout.
type Reward struct {
	Account string
	Hero    string
	Level   int
	Amount  int
}

// Entry is one leaderboard row.
type Entry struct {
	Account string
	XP      int
	Level   int
}

// Limits bounds the mastery tables.
type Limits struct {
	MaxPairs      int
	MaxLevel      int
	MaxXPPerMatch int
}

type pairKey struct {
	account string
	hero    string
}

type pairState struct {
	account string
	hero    string
	xp      int
	level   int
	claimed map[int]bool
}

// HeroMasteryLedger owns experience, levels, milestones and the leaderboard.
type HeroMasteryLedger struct {
	mu          sync.RWMutex
	limits      Limits
	pairs       map[pairKey]*pairState
	modes       map[string]float64
	battles     map[string]struct{}
	battleOrder []string
}

// NewHeroMasteryLedger builds a ledger with the given caps.
func NewHeroMasteryLedger(limits Limits) *HeroMasteryLedger {
	return &HeroMasteryLedger{
		limits:  limits,
		pairs:   make(map[pairKey]*pairState),
		modes:   make(map[string]float64),
		battles: make(map[string]struct{}),
	}
}

// MasteryThreshold is the total experience needed to reach the given level.
func MasteryThreshold(level int) int {
	if level < 1 {
		return 0
	}
	return 60 * level * (level - 1)
}

func (l *HeroMasteryLedger) maxLevel() int {
	if l.limits.MaxLevel > 0 {
		return l.limits.MaxLevel
	}
	return math.MaxInt32
}

// convert validates the award inputs and converts raw experience into the
// effective per match amount.
func (l *HeroMasteryLedger) convert(account, hero string, rawXP int, mode string) (int, bool, error) {
	if account == "" || hero == "" {
		return 0, false, ErrBadIdentity
	}
	if rawXP <= 0 {
		return 0, false, ErrBadXP
	}
	if mode == "" {
		return 0, false, ErrBadMode
	}
	factor, ok := l.modes[mode]
	if !ok {
		return 0, false, ErrUnknownMode
	}
	effective := int(math.Floor(float64(rawXP) * factor))
	capped := false
	if l.limits.MaxXPPerMatch > 0 && effective > l.limits.MaxXPPerMatch {
		effective = l.limits.MaxXPPerMatch
		capped = true
	}
	return effective, capped, nil
}

// applyLocked credits effective experience to one pair and promotes it to the
// highest reachable level. The caller must hold the write lock.
func (l *HeroMasteryLedger) applyLocked(account, hero string, rawXP, effective int, capped bool) (Gain, error) {
	key := pairKey{account: account, hero: hero}
	state, ok := l.pairs[key]
	if !ok {
		if l.limits.MaxPairs > 0 && len(l.pairs) >= l.limits.MaxPairs {
			return Gain{}, ErrLimit
		}
		state = &pairState{account: account, hero: hero, level: 1, claimed: make(map[int]bool)}
		l.pairs[key] = state
	}
	gain := Gain{
		Account:     account,
		Hero:        hero,
		RawXP:       rawXP,
		Effective:   effective,
		LevelBefore: state.level,
		Capped:      capped,
	}
	maxLevel := l.maxLevel()
	if state.level < maxLevel {
		state.xp += effective
		if l.limits.MaxLevel > 0 {
			if capXP := MasteryThreshold(l.limits.MaxLevel); state.xp > capXP {
				state.xp = capXP
			}
		}
		for state.level < maxLevel && state.xp >= MasteryThreshold(state.level+1) {
			state.level++
		}
	}
	gain.LevelAfter = state.level
	gain.LevelsGained = state.level - gain.LevelBefore
	gain.XPAfter = state.xp
	return gain, nil
}

// Award adds the experience of one finished match.
func (l *HeroMasteryLedger) Award(account string, hero string, rawXP int, mode string) (Gain, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	effective, capped, err := l.convert(account, hero, rawXP, mode)
	if err != nil {
		return Gain{}, err
	}
	return l.applyLocked(account, hero, rawXP, effective, capped)
}

// AwardBattle adds experience reported for one identified battle. A battle id
// is credited at most once; repeats report a duplicate gain.
func (l *HeroMasteryLedger) AwardBattle(account string, hero string, battleID string, rawXP int, mode string) (Gain, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if battleID != "" {
		if _, seen := l.battles[battleID]; seen {
			gain := Gain{Account: account, Hero: hero, Duplicate: true}
			if state, ok := l.pairs[pairKey{account: account, hero: hero}]; ok {
				gain.LevelBefore = state.level
				gain.LevelAfter = state.level
				gain.XPAfter = state.xp
			}
			return gain, nil
		}
	}
	effective, capped, err := l.convert(account, hero, rawXP, mode)
	if err != nil {
		return Gain{}, err
	}
	gain, err := l.applyLocked(account, hero, rawXP, effective, capped)
	if err != nil {
		return Gain{}, err
	}
	if battleID != "" {
		l.battles[battleID] = struct{}{}
		l.battleOrder = append(l.battleOrder, battleID)
		for len(l.battleOrder) > maxProcessedBattles {
			delete(l.battles, l.battleOrder[0])
			l.battleOrder = l.battleOrder[1:]
		}
	}
	return gain, nil
}

// Level reads the mastery level of one account and hero pair.
func (l *HeroMasteryLedger) Level(account string, hero string) (MasteryLevel, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	state, ok := l.pairs[pairKey{account: account, hero: hero}]
	if !ok {
		return MasteryLevel{}, ErrUnknownPair
	}
	return MasteryLevel{
		Level:  state.level,
		XP:     state.xp,
		NextXP: MasteryThreshold(state.level + 1),
	}, nil
}

// Progress reads the durable mastery record of one account and hero pair.
func (l *HeroMasteryLedger) Progress(account string, hero string) (Progress, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	state, ok := l.pairs[pairKey{account: account, hero: hero}]
	if !ok {
		return Progress{}, ErrUnknownPair
	}
	claimed := make([]int, 0, len(state.claimed))
	for level := range state.claimed {
		claimed = append(claimed, level)
	}
	sort.Ints(claimed)
	return Progress{
		Account: state.account,
		Hero:    state.hero,
		XP:      state.xp,
		Level:   state.level,
		Claimed: claimed,
	}, nil
}

// SetModeFactor registers the experience factor of one game mode.
func (l *HeroMasteryLedger) SetModeFactor(mode string, factor float64) error {
	if mode == "" {
		return ErrBadMode
	}
	if factor <= 0 || factor > 3 || math.IsNaN(factor) {
		return ErrBadFactor
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.modes[mode] = factor
	return nil
}

// ClaimMilestone hands out the milestone reward of one level.
func (l *HeroMasteryLedger) ClaimMilestone(account string, hero string, level int) (Reward, error) {
	if level < 2 {
		return Reward{}, ErrBadLevel
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.pairs[pairKey{account: account, hero: hero}]
	if !ok {
		return Reward{}, ErrUnknownPair
	}
	if level > state.level {
		return Reward{}, ErrLevelNotReached
	}
	if state.claimed[level] {
		return Reward{}, ErrAlreadyClaimed
	}
	state.claimed[level] = true
	return Reward{Account: account, Hero: hero, Level: level, Amount: level * 100}, nil
}

// Leaderboard lists the top accounts of one hero by experience.
func (l *HeroMasteryLedger) Leaderboard(hero string, n int) []Entry {
	if n <= 0 {
		return nil
	}
	l.mu.RLock()
	entries := make([]Entry, 0, len(l.pairs))
	for _, state := range l.pairs {
		if state.hero == hero {
			entries = append(entries, Entry{Account: state.account, XP: state.xp, Level: state.level})
		}
	}
	l.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].XP != entries[j].XP {
			return entries[i].XP > entries[j].XP
		}
		return entries[i].Account < entries[j].Account
	})
	if n < len(entries) {
		entries = entries[:n]
	}
	return entries
}

type snapshotPair struct {
	Account string `json:"account"`
	Hero    string `json:"hero"`
	XP      int    `json:"xp"`
	Level   int    `json:"level"`
	Claimed []int  `json:"claimed"`
}

type snapshot struct {
	Pairs   []snapshotPair     `json:"pairs"`
	Modes   map[string]float64 `json:"modes"`
	Battles []string           `json:"battles"`
}

// Snapshot serialises experience, levels, mode factors and claimed milestones.
func (l *HeroMasteryLedger) Snapshot() ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	snap := snapshot{
		Pairs:   make([]snapshotPair, 0, len(l.pairs)),
		Modes:   make(map[string]float64, len(l.modes)),
		Battles: make([]string, 0, len(l.battles)),
	}
	for _, state := range l.pairs {
		claimed := make([]int, 0, len(state.claimed))
		for level := range state.claimed {
			claimed = append(claimed, level)
		}
		sort.Ints(claimed)
		snap.Pairs = append(snap.Pairs, snapshotPair{
			Account: state.account,
			Hero:    state.hero,
			XP:      state.xp,
			Level:   state.level,
			Claimed: claimed,
		})
	}
	for mode, factor := range l.modes {
		snap.Modes[mode] = factor
	}
	snap.Battles = append(snap.Battles, l.battleOrder...)
	return json.Marshal(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *HeroMasteryLedger) Restore(data []byte) error {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}
	if snap.Pairs == nil || snap.Modes == nil {
		return ErrBadSnapshot
	}
	pairs := make(map[pairKey]*pairState, len(snap.Pairs))
	for _, entry := range snap.Pairs {
		if entry.Account == "" || entry.Hero == "" || entry.Level < 1 || entry.XP < 0 {
			return ErrBadSnapshot
		}
		key := pairKey{account: entry.Account, hero: entry.Hero}
		if _, dup := pairs[key]; dup {
			return ErrBadSnapshot
		}
		claimed := make(map[int]bool, len(entry.Claimed))
		for _, level := range entry.Claimed {
			claimed[level] = true
		}
		pairs[key] = &pairState{
			account: entry.Account,
			hero:    entry.Hero,
			xp:      entry.XP,
			level:   entry.Level,
			claimed: claimed,
		}
	}
	modes := make(map[string]float64, len(snap.Modes))
	for mode, factor := range snap.Modes {
		if mode == "" || factor <= 0 || factor > 3 || math.IsNaN(factor) {
			return ErrBadSnapshot
		}
		modes[mode] = factor
	}
	battles := make(map[string]struct{}, len(snap.Battles))
	battleOrder := make([]string, 0, len(snap.Battles))
	for _, id := range snap.Battles {
		if id == "" {
			return ErrBadSnapshot
		}
		if _, dup := battles[id]; dup {
			return ErrBadSnapshot
		}
		battles[id] = struct{}{}
		battleOrder = append(battleOrder, id)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pairs = pairs
	l.modes = modes
	l.battles = battles
	l.battleOrder = battleOrder
	return nil
}
