// Package task owns the class advancement of one character: the requirement
// gate, the skill point refund, the gear that the new class cannot wear, the
// bag check that makes the whole advancement fail, the cooldown and a state
// that survives a restart.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Errors returned by the guard.
var (
	ErrBlankPlayer       = errors.New("player id is required")
	ErrUnknownPlayer     = errors.New("player is not enrolled")
	ErrBadClass          = errors.New("class spec is invalid")
	ErrClassExists       = errors.New("class is already registered")
	ErrUnknownClass      = errors.New("unknown class")
	ErrBadRequirement    = errors.New("requirement is invalid")
	ErrBadLevel          = errors.New("level must be positive")
	ErrBadPoints         = errors.New("skill points must be positive")
	ErrNotEnoughPoints   = errors.New("not enough skill points available")
	ErrUnknownSkill      = errors.New("skill does not belong to the current class")
	ErrBadItem           = errors.New("item id is required")
	ErrBadBag            = errors.New("bag free slots cannot be negative")
	ErrBadFlag           = errors.New("quest flag is required")
	ErrRequirementNotMet = errors.New("advancement requirements are not met")
	ErrBagFull           = errors.New("bag has no room for the unequipped gear")
	ErrCooldownActive    = errors.New("advancement cooldown is still running")
	ErrNoTransition      = errors.New("no advancement to revert")
	ErrTimeWentBackwards = errors.New("clock moved backwards")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Requirement is the gate of one target class.
type Requirement struct {
	MinLevel int      `json:"min_level"`
	Prereq   []string `json:"prereq"`
	Quests   []string `json:"quests"`
}

// ClassSpec is the static description of one class.
type ClassSpec struct {
	ID           string   `json:"id"`
	Skills       []string `json:"skills"`
	AllowedEquip []string `json:"allowed_equip"`
}

// Skill is one invested skill of a player.
type Skill struct {
	Name   string `json:"name"`
	Points int    `json:"points"`
}

// PlayerState is the projection of one character.
type PlayerState struct {
	Player         string   `json:"player"`
	Class          string   `json:"class"`
	Level          int      `json:"level"`
	Points         int      `json:"points"`
	Skills         []Skill  `json:"skills"`
	Equipment      []string `json:"equipment"`
	BagFree        int      `json:"bag_free"`
	CooldownLeftMs int64    `json:"cooldown_left_ms"`
}

// Plan is the read only evaluation of one advancement.
type Plan struct {
	Player  string   `json:"player"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	OK      bool     `json:"ok"`
	Missing []string `json:"missing"`
	Refund  int      `json:"refund"`
	Unequip []string `json:"unequip"`
}

// Result is the outcome of one committed advancement.
type Result struct {
	Player     string   `json:"player"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Refunded   int      `json:"refunded"`
	Unequipped []string `json:"unequipped"`
	Seq        int64    `json:"seq"`
}

// Transition is one entry of the per player advancement history.
type Transition struct {
	Player     string   `json:"player"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Refunded   int      `json:"refunded"`
	Unequipped []string `json:"unequipped"`
	At         int64    `json:"at"`
	Seq        int64    `json:"seq"`
	Reverted   bool     `json:"reverted"`
}

// Limits bounds the history length and the advancement cooldown.
type Limits struct {
	MaxHistory     int   `json:"max_history"`
	CooldownMillis int64 `json:"cooldown_millis"`
}

// ClassAdvancementGuard owns the class table, the characters and the history.
type ClassAdvancementGuard struct {
	limits Limits
	now    time.Time
}

// NewClassAdvancementGuard builds a guard whose clock starts at now.
func NewClassAdvancementGuard(limits Limits, now time.Time) *ClassAdvancementGuard {
	return &ClassAdvancementGuard{limits: limits, now: now}
}

// RegisterClass records one class and the skills it can train.
func (g *ClassAdvancementGuard) RegisterClass(spec ClassSpec) error {
	return errNotImplemented
}

// SetRequirement records the gate of one target class.
func (g *ClassAdvancementGuard) SetRequirement(class string, req Requirement) error {
	return errNotImplemented
}

// Enroll creates a character in its starting class.
func (g *ClassAdvancementGuard) Enroll(player, class string) error {
	return errNotImplemented
}

// SetPlayerLevel sets the character level.
func (g *ClassAdvancementGuard) SetPlayerLevel(player string, level int) error {
	return errNotImplemented
}

// SetQuestFlag marks one quest flag as done for the character.
func (g *ClassAdvancementGuard) SetQuestFlag(player, flag string) error {
	return errNotImplemented
}

// SetSkillPoints sets the unspent skill points of the character.
func (g *ClassAdvancementGuard) SetSkillPoints(player string, points int) error {
	return errNotImplemented
}

// LearnSkill invests points of the character into one class skill.
func (g *ClassAdvancementGuard) LearnSkill(player, skill string, points int) error {
	return errNotImplemented
}

// Equip puts one item on the character.
func (g *ClassAdvancementGuard) Equip(player, itemID string) error {
	return errNotImplemented
}

// SetBagFree sets how many free bag slots the character has.
func (g *ClassAdvancementGuard) SetBagFree(player string, n int) error {
	return errNotImplemented
}

// State projects one character.
func (g *ClassAdvancementGuard) State(player string) (PlayerState, error) {
	return PlayerState{}, errNotImplemented
}

// Preview evaluates one advancement without touching any state.
func (g *ClassAdvancementGuard) Preview(player, toClass string) (Plan, error) {
	return Plan{}, errNotImplemented
}

// Advance commits one advancement.
func (g *ClassAdvancementGuard) Advance(player, toClass string) (Result, error) {
	return Result{}, errNotImplemented
}

// RefundSkills returns every invested point and clears the skill table.
func (g *ClassAdvancementGuard) RefundSkills(player string) (int, error) {
	return 0, errNotImplemented
}

// Revert undoes the last committed advancement of one character.
func (g *ClassAdvancementGuard) Revert(player string) error {
	return errNotImplemented
}

// CooldownLeft reads the remaining advancement cooldown of one character.
func (g *ClassAdvancementGuard) CooldownLeft(player string) int64 {
	return 0
}

// AdvanceClock moves the guard clock forward.
func (g *ClassAdvancementGuard) AdvanceClock(now time.Time) error {
	return errNotImplemented
}

// History returns a copy of the per player advancement history, oldest first.
func (g *ClassAdvancementGuard) History(player string) []Transition {
	return nil
}

// Snapshot serialises the whole guard state.
func (g *ClassAdvancementGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ClassAdvancementGuard) Restore(data []byte) error {
	return errNotImplemented
}
