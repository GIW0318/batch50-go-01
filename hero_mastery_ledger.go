// Package task owns the hero mastery ledger: per match experience conversion,
// level promotion, milestone rewards and the per hero leaderboard.
package task

import "errors"

var errNotImplemented = errors.New("not implemented")

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

// HeroMasteryLedger owns experience, levels, milestones and the leaderboard.
type HeroMasteryLedger struct {
	limits Limits
}

// NewHeroMasteryLedger builds a ledger with the given caps.
func NewHeroMasteryLedger(limits Limits) *HeroMasteryLedger {
	return &HeroMasteryLedger{limits: limits}
}

// MasteryThreshold is the total experience needed to reach the given level.
func MasteryThreshold(level int) int {
	return 0
}

// Award adds the experience of one finished match.
func (l *HeroMasteryLedger) Award(account string, hero string, rawXP int, mode string) (Gain, error) {
	return Gain{}, errNotImplemented
}

// AwardBattle adds experience reported for one identified battle.
func (l *HeroMasteryLedger) AwardBattle(account string, hero string, battleID string, rawXP int, mode string) (Gain, error) {
	return Gain{}, errNotImplemented
}

// Level reads the mastery level of one account and hero pair.
func (l *HeroMasteryLedger) Level(account string, hero string) (MasteryLevel, error) {
	return MasteryLevel{}, errNotImplemented
}

// Progress reads the durable mastery record of one account and hero pair.
func (l *HeroMasteryLedger) Progress(account string, hero string) (Progress, error) {
	return Progress{}, errNotImplemented
}

// SetModeFactor registers the experience factor of one game mode.
func (l *HeroMasteryLedger) SetModeFactor(mode string, factor float64) error {
	return errNotImplemented
}

// ClaimMilestone hands out the milestone reward of one level.
func (l *HeroMasteryLedger) ClaimMilestone(account string, hero string, level int) (Reward, error) {
	return Reward{}, errNotImplemented
}

// Leaderboard lists the top accounts of one hero by experience.
func (l *HeroMasteryLedger) Leaderboard(hero string, n int) []Entry {
	return nil
}

// Snapshot serialises experience, levels, mode factors and claimed milestones.
func (l *HeroMasteryLedger) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *HeroMasteryLedger) Restore(data []byte) error {
	return errNotImplemented
}
