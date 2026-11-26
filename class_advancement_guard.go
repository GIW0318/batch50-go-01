// Package task owns the class advancement of one character: the requirement
// gate, the skill point refund, the gear that the new class cannot wear, the
// bag check that makes the whole advancement fail, the cooldown and a state
// that survives a restart.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

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

type undoState struct {
	Class      string          `json:"class"`
	Level      int             `json:"level"`
	Points     int             `json:"points"`
	Skills     []Skill         `json:"skills"`
	Equipment  []string        `json:"equipment"`
	BagFree    int             `json:"bag_free"`
	Flags      map[string]bool `json:"flags"`
	ClassTrail []string        `json:"class_trail"`
}

type transitionRecord struct {
	Transition
	Before *undoState `json:"before,omitempty"`
}

type playerState struct {
	Player        string             `json:"player"`
	Class         string             `json:"class"`
	Level         int                `json:"level"`
	Points        int                `json:"points"`
	Skills        []Skill            `json:"skills"`
	Equipment     []string           `json:"equipment"`
	BagFree       int                `json:"bag_free"`
	Flags         map[string]bool    `json:"flags"`
	ClassTrail    []string           `json:"class_trail"`
	CooldownUntil time.Time          `json:"cooldown_until"`
	Records       []transitionRecord `json:"records"`
	LastTarget    string             `json:"last_target,omitempty"`
	LastResult    *Result            `json:"last_result,omitempty"`
}

type snapshotData struct {
	Version      int                     `json:"version"`
	Limits       Limits                  `json:"limits"`
	Now          time.Time               `json:"now"`
	Seq          int64                   `json:"seq"`
	Classes      map[string]ClassSpec    `json:"classes"`
	Requirements map[string]Requirement  `json:"requirements"`
	Players      map[string]*playerState `json:"players"`
}

const snapshotVersion = 1

// ClassAdvancementGuard owns the class table, the characters and the history.
type ClassAdvancementGuard struct {
	mu           sync.RWMutex
	limits       Limits
	now          time.Time
	seq          int64
	classes      map[string]ClassSpec
	requirements map[string]Requirement
	players      map[string]*playerState
}

// NewClassAdvancementGuard builds a guard whose clock starts at now.
func NewClassAdvancementGuard(limits Limits, now time.Time) *ClassAdvancementGuard {
	return &ClassAdvancementGuard{
		limits:       limits,
		now:          now,
		classes:      make(map[string]ClassSpec),
		requirements: make(map[string]Requirement),
		players:      make(map[string]*playerState),
	}
}

// RegisterClass records one class and the skills it can train.
func (g *ClassAdvancementGuard) RegisterClass(spec ClassSpec) error {
	if isBlank(spec.ID) {
		return ErrBadClass
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.classes[spec.ID]; exists {
		return ErrClassExists
	}
	spec.Skills = cloneStrings(spec.Skills)
	spec.AllowedEquip = cloneStrings(spec.AllowedEquip)
	g.classes[spec.ID] = spec
	return nil
}

// SetRequirement records the gate of one target class.
func (g *ClassAdvancementGuard) SetRequirement(class string, req Requirement) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.classes[class]; !exists {
		return ErrUnknownClass
	}
	if req.MinLevel < 0 {
		return ErrBadRequirement
	}
	req.Prereq = cloneStrings(req.Prereq)
	req.Quests = cloneStrings(req.Quests)
	g.requirements[class] = req
	return nil
}

// Enroll creates a character in its starting class.
func (g *ClassAdvancementGuard) Enroll(player, class string) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.classes[class]; !exists {
		return ErrUnknownClass
	}
	if _, exists := g.players[player]; exists {
		return nil
	}

	g.players[player] = &playerState{
		Player:     player,
		Class:      class,
		Level:      1,
		Skills:     []Skill{},
		Equipment:  []string{},
		Flags:      make(map[string]bool),
		ClassTrail: []string{class},
		Records:    []transitionRecord{},
	}
	return nil
}

// SetPlayerLevel sets the character level.
func (g *ClassAdvancementGuard) SetPlayerLevel(player string, level int) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}
	if level <= 0 {
		return ErrBadLevel
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}
	p.Level = level
	return nil
}

// SetQuestFlag marks one quest flag as done for the character.
func (g *ClassAdvancementGuard) SetQuestFlag(player, flag string) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}
	if isBlank(flag) {
		return ErrBadFlag
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}
	if p.Flags == nil {
		p.Flags = make(map[string]bool)
	}
	p.Flags[flag] = true
	return nil
}

// SetSkillPoints sets the unspent skill points of the character.
func (g *ClassAdvancementGuard) SetSkillPoints(player string, points int) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}
	if points < 0 {
		return ErrBadPoints
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}
	p.Points = points
	return nil
}

// LearnSkill invests points of the character into one class skill.
func (g *ClassAdvancementGuard) LearnSkill(player, skill string, points int) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}
	if points <= 0 {
		return ErrBadPoints
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}
	spec, ok := g.classes[p.Class]
	if !ok || !containsString(spec.Skills, skill) {
		return ErrUnknownSkill
	}
	if points > p.Points {
		return ErrNotEnoughPoints
	}

	p.Points -= points
	for i := range p.Skills {
		if p.Skills[i].Name == skill {
			p.Skills[i].Points += points
			return nil
		}
	}
	p.Skills = append(p.Skills, Skill{Name: skill, Points: points})
	return nil
}

// Equip puts one item on the character.
func (g *ClassAdvancementGuard) Equip(player, itemID string) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}
	if isBlank(itemID) {
		return ErrBadItem
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}
	p.Equipment = append(p.Equipment, itemID)
	return nil
}

// SetBagFree sets how many free bag slots the character has.
func (g *ClassAdvancementGuard) SetBagFree(player string, n int) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}
	if n < 0 {
		return ErrBadBag
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}
	p.BagFree = n
	return nil
}

// State projects one character.
func (g *ClassAdvancementGuard) State(player string) (PlayerState, error) {
	if isBlank(player) {
		return PlayerState{}, ErrBlankPlayer
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return PlayerState{}, err
	}
	return PlayerState{
		Player:         p.Player,
		Class:          p.Class,
		Level:          p.Level,
		Points:         p.Points,
		Skills:         cloneSkills(p.Skills),
		Equipment:      cloneStrings(p.Equipment),
		BagFree:        p.BagFree,
		CooldownLeftMs: g.cooldownLeftLocked(p),
	}, nil
}

// Preview evaluates one advancement without touching any state.
func (g *ClassAdvancementGuard) Preview(player, toClass string) (Plan, error) {
	if isBlank(player) {
		return Plan{}, ErrBlankPlayer
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return Plan{}, err
	}
	if _, ok := g.classes[toClass]; !ok {
		return Plan{}, ErrUnknownClass
	}
	return g.buildPlanLocked(p, toClass), nil
}

// Advance commits one advancement.
func (g *ClassAdvancementGuard) Advance(player, toClass string) (Result, error) {
	if isBlank(player) {
		return Result{}, ErrBlankPlayer
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return Result{}, err
	}

	cooldownLeft := g.cooldownLeftLocked(p)
	if cooldownLeft > 0 && p.LastResult != nil && p.LastTarget == toClass {
		return cloneResult(*p.LastResult), nil
	}
	if cooldownLeft > 0 {
		return Result{}, ErrCooldownActive
	}
	if _, ok := g.classes[toClass]; !ok {
		return Result{}, ErrUnknownClass
	}

	plan := g.buildPlanLocked(p, toClass)
	if !plan.OK {
		return Result{}, ErrRequirementNotMet
	}
	if p.BagFree < len(plan.Unequip) {
		return Result{}, ErrBagFull
	}

	before := snapshotUndo(p)
	refund := plan.Refund

	targetSpec := g.classes[toClass]
	targetSkills := stringSet(targetSpec.Skills)
	retainedSkills := make([]Skill, 0, len(p.Skills))
	for _, skill := range p.Skills {
		if _, shared := targetSkills[skill.Name]; shared {
			retainedSkills = append(retainedSkills, skill)
		}
	}

	allowedEquipment := stringSet(targetSpec.AllowedEquip)
	retainedEquipment := make([]string, 0, len(p.Equipment))
	for _, item := range p.Equipment {
		if _, allowed := allowedEquipment[item]; allowed {
			retainedEquipment = append(retainedEquipment, item)
		}
	}

	p.Points += refund
	p.Skills = retainedSkills
	p.Equipment = retainedEquipment
	p.BagFree -= len(plan.Unequip)
	p.Class = toClass
	if !containsString(p.ClassTrail, toClass) {
		p.ClassTrail = append(p.ClassTrail, toClass)
	}

	g.seq++
	transition := Transition{
		Player:     player,
		From:       plan.From,
		To:         toClass,
		Refunded:   refund,
		Unequipped: cloneStrings(plan.Unequip),
		At:         g.now.UnixMilli(),
		Seq:        g.seq,
	}
	p.Records = append(p.Records, transitionRecord{
		Transition: transition,
		Before:     before,
	})
	p.Records = capHistory(p.Records, g.limits.MaxHistory)

	if g.limits.CooldownMillis > 0 {
		p.CooldownUntil = g.now.Add(time.Duration(g.limits.CooldownMillis) * time.Millisecond)
	} else {
		p.CooldownUntil = time.Time{}
	}

	result := resultFromTransition(transition)
	p.LastTarget = toClass
	p.LastResult = &result
	return cloneResult(result), nil
}

// RefundSkills returns every invested point and clears the skill table.
func (g *ClassAdvancementGuard) RefundSkills(player string) (int, error) {
	if isBlank(player) {
		return 0, ErrBlankPlayer
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return 0, err
	}

	refunded := 0
	for _, skill := range p.Skills {
		refunded += skill.Points
	}
	p.Points += refunded
	p.Skills = []Skill{}
	return refunded, nil
}

// Revert undoes the last committed advancement of one character.
func (g *ClassAdvancementGuard) Revert(player string) error {
	if isBlank(player) {
		return ErrBlankPlayer
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, err := g.playerLocked(player)
	if err != nil {
		return err
	}

	index := -1
	for i := len(p.Records) - 1; i >= 0; i-- {
		if !p.Records[i].Reverted {
			index = i
			break
		}
	}
	if index < 0 || p.Records[index].Before == nil {
		return ErrNoTransition
	}

	record := p.Records[index]
	before := record.Before
	p.Class = before.Class
	p.Level = before.Level
	p.Points = before.Points
	p.Skills = cloneSkills(before.Skills)
	p.Equipment = cloneStrings(before.Equipment)
	p.BagFree = before.BagFree
	p.Flags = cloneBoolSet(before.Flags)
	p.ClassTrail = cloneStrings(before.ClassTrail)
	p.Records[index].Reverted = true

	if p.LastResult != nil && p.LastResult.Seq == record.Seq {
		p.LastResult = nil
		p.LastTarget = ""
	}
	return nil
}

// CooldownLeft reads the remaining advancement cooldown of one character.
func (g *ClassAdvancementGuard) CooldownLeft(player string) int64 {
	if isBlank(player) {
		return 0
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	p, ok := g.players[player]
	if !ok {
		return 0
	}
	return g.cooldownLeftLocked(p)
}

// AdvanceClock moves the guard clock forward.
func (g *ClassAdvancementGuard) AdvanceClock(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if now.Before(g.now) {
		return ErrTimeWentBackwards
	}
	g.now = now
	return nil
}

// History returns a copy of the per player advancement history, oldest first.
func (g *ClassAdvancementGuard) History(player string) []Transition {
	if isBlank(player) {
		return nil
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	p, ok := g.players[player]
	if !ok {
		return nil
	}
	history := make([]Transition, 0, len(p.Records))
	for _, record := range p.Records {
		transition := record.Transition
		transition.Unequipped = cloneStrings(transition.Unequipped)
		history = append(history, transition)
	}
	return history
}

// Snapshot serialises the whole guard state.
func (g *ClassAdvancementGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	snapshot := snapshotData{
		Version:      snapshotVersion,
		Limits:       g.limits,
		Now:          g.now,
		Seq:          g.seq,
		Classes:      g.classes,
		Requirements: g.requirements,
		Players:      g.players,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal class advancement snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ClassAdvancementGuard) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return ErrBadSnapshot
	}

	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return err
	}
	normalizeSnapshot(&snapshot)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.now = snapshot.Now
	g.seq = snapshot.Seq
	g.classes = snapshot.Classes
	g.requirements = snapshot.Requirements
	g.players = snapshot.Players
	for _, player := range g.players {
		player.Records = capHistory(player.Records, g.limits.MaxHistory)
	}
	return nil
}

func (g *ClassAdvancementGuard) playerLocked(player string) (*playerState, error) {
	p, ok := g.players[player]
	if !ok {
		return nil, ErrUnknownPlayer
	}
	return p, nil
}

func (g *ClassAdvancementGuard) cooldownLeftLocked(p *playerState) int64 {
	if p.CooldownUntil.IsZero() {
		return 0
	}
	left := p.CooldownUntil.Sub(g.now)
	if left <= 0 {
		return 0
	}
	return left.Milliseconds()
}

func (g *ClassAdvancementGuard) buildPlanLocked(p *playerState, toClass string) Plan {
	requirement := g.requirements[toClass]
	missing := make([]string, 0)

	if p.Level < requirement.MinLevel {
		missing = append(missing, "level")
	}
	for _, class := range requirement.Prereq {
		if !containsString(p.ClassTrail, class) {
			missing = append(missing, "class:"+class)
		}
	}
	for _, flag := range requirement.Quests {
		if p.Flags == nil || !p.Flags[flag] {
			missing = append(missing, "quest:"+flag)
		}
	}
	sort.Strings(missing)

	targetSpec := g.classes[toClass]
	targetSkills := stringSet(targetSpec.Skills)
	refund := 0
	for _, skill := range p.Skills {
		if _, shared := targetSkills[skill.Name]; !shared {
			refund += skill.Points
		}
	}

	allowedEquipment := stringSet(targetSpec.AllowedEquip)
	unequip := make([]string, 0)
	for _, item := range p.Equipment {
		if _, allowed := allowedEquipment[item]; !allowed {
			unequip = append(unequip, item)
		}
	}

	return Plan{
		Player:  p.Player,
		From:    p.Class,
		To:      toClass,
		OK:      len(missing) == 0,
		Missing: missing,
		Refund:  refund,
		Unequip: unequip,
	}
}

func snapshotUndo(p *playerState) *undoState {
	return &undoState{
		Class:      p.Class,
		Level:      p.Level,
		Points:     p.Points,
		Skills:     cloneSkills(p.Skills),
		Equipment:  cloneStrings(p.Equipment),
		BagFree:    p.BagFree,
		Flags:      cloneBoolSet(p.Flags),
		ClassTrail: cloneStrings(p.ClassTrail),
	}
}

func resultFromTransition(transition Transition) Result {
	return Result{
		Player:     transition.Player,
		From:       transition.From,
		To:         transition.To,
		Refunded:   transition.Refunded,
		Unequipped: cloneStrings(transition.Unequipped),
		Seq:        transition.Seq,
	}
}

func cloneResult(result Result) Result {
	result.Unequipped = cloneStrings(result.Unequipped)
	return result
}

func cloneStrings(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneSkills(values []Skill) []Skill {
	out := make([]Skill, len(values))
	copy(out, values)
	return out
}

func cloneBoolSet(values map[string]bool) map[string]bool {
	out := make(map[string]bool, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func capHistory(records []transitionRecord, maxHistory int) []transitionRecord {
	if maxHistory < 0 {
		maxHistory = 0
	}
	if len(records) <= maxHistory {
		return records
	}
	if maxHistory == 0 {
		return []transitionRecord{}
	}
	return append([]transitionRecord(nil), records[len(records)-maxHistory:]...)
}

func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if snapshot.Classes == nil || snapshot.Requirements == nil || snapshot.Players == nil {
		return fmt.Errorf("%w: missing tables", ErrBadSnapshot)
	}
	if snapshot.Seq < 0 {
		return fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}

	for id, spec := range snapshot.Classes {
		if isBlank(id) || isBlank(spec.ID) || spec.ID != id {
			return fmt.Errorf("%w: invalid class %q", ErrBadSnapshot, id)
		}
	}
	for class, requirement := range snapshot.Requirements {
		if _, ok := snapshot.Classes[class]; !ok || requirement.MinLevel < 0 {
			return fmt.Errorf("%w: invalid requirement %q", ErrBadSnapshot, class)
		}
	}

	maxSeq := snapshot.Seq
	for id, player := range snapshot.Players {
		if err := validateSnapshotPlayer(id, player, snapshot); err != nil {
			return err
		}
		for _, record := range player.Records {
			if record.Seq > maxSeq {
				maxSeq = record.Seq
			}
		}
		if player.LastResult != nil && player.LastResult.Seq > maxSeq {
			maxSeq = player.LastResult.Seq
		}
	}
	snapshot.Seq = maxSeq
	return nil
}

func validateSnapshotPlayer(id string, p *playerState, snapshot *snapshotData) error {
	if p == nil || isBlank(id) || isBlank(p.Player) || p.Player != id {
		return fmt.Errorf("%w: invalid player %q", ErrBadSnapshot, id)
	}
	if _, ok := snapshot.Classes[p.Class]; !ok {
		return fmt.Errorf("%w: unknown class for player %q", ErrBadSnapshot, id)
	}
	if p.Level <= 0 || p.Points < 0 || p.BagFree < 0 {
		return fmt.Errorf("%w: invalid numeric state for %q", ErrBadSnapshot, id)
	}
	for _, skill := range p.Skills {
		if isBlank(skill.Name) || skill.Points <= 0 {
			return fmt.Errorf("%w: invalid skill for %q", ErrBadSnapshot, id)
		}
	}
	for _, item := range p.Equipment {
		if isBlank(item) {
			return fmt.Errorf("%w: invalid equipment for %q", ErrBadSnapshot, id)
		}
	}
	if len(p.ClassTrail) == 0 {
		return fmt.Errorf("%w: missing class trail for %q", ErrBadSnapshot, id)
	}
	for _, class := range p.ClassTrail {
		if _, ok := snapshot.Classes[class]; !ok {
			return fmt.Errorf("%w: invalid class trail for %q", ErrBadSnapshot, id)
		}
	}
	for i := range p.Records {
		record := &p.Records[i]
		if record.Seq <= 0 || record.At < 0 || record.Refunded < 0 {
			return fmt.Errorf("%w: invalid transition for %q", ErrBadSnapshot, id)
		}
		if record.Player != id {
			return fmt.Errorf("%w: transition player mismatch for %q", ErrBadSnapshot, id)
		}
		if _, ok := snapshot.Classes[record.From]; !ok {
			return fmt.Errorf("%w: invalid transition source for %q", ErrBadSnapshot, id)
		}
		if _, ok := snapshot.Classes[record.To]; !ok {
			return fmt.Errorf("%w: invalid transition target for %q", ErrBadSnapshot, id)
		}
		for _, item := range record.Unequipped {
			if isBlank(item) {
				return fmt.Errorf("%w: invalid transition equipment for %q", ErrBadSnapshot, id)
			}
		}
		if record.Before != nil {
			if _, ok := snapshot.Classes[record.Before.Class]; !ok {
				return fmt.Errorf("%w: invalid undo class for %q", ErrBadSnapshot, id)
			}
			if record.Before.Level <= 0 || record.Before.Points < 0 || record.Before.BagFree < 0 {
				return fmt.Errorf("%w: invalid undo state for %q", ErrBadSnapshot, id)
			}
		}
	}
	if p.LastResult != nil {
		if p.LastResult.Seq <= 0 || p.LastResult.Player != id {
			return fmt.Errorf("%w: invalid idempotent result for %q", ErrBadSnapshot, id)
		}
		if _, ok := snapshot.Classes[p.LastResult.To]; !ok {
			return fmt.Errorf("%w: invalid idempotent target for %q", ErrBadSnapshot, id)
		}
	}
	return nil
}

func normalizeSnapshot(snapshot *snapshotData) {
	for _, player := range snapshot.Players {
		if player.Flags == nil {
			player.Flags = make(map[string]bool)
		}
		if player.Skills == nil {
			player.Skills = []Skill{}
		}
		if player.Equipment == nil {
			player.Equipment = []string{}
		}
		if player.Records == nil {
			player.Records = []transitionRecord{}
		}
	}
}
