// Package task owns the new player tutorial ledger: ordered step completion,
// whole flow skips, one shot step rewards, flow revisions and restart recovery.
package task

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"
)

// Errors reported by the tutorial guard.
var (
	ErrBadPlayer       = errors.New("player id is required")
	ErrBadStep         = errors.New("step id is required")
	ErrBadFlow         = errors.New("tutorial flow definition is invalid")
	ErrUnknownFlow     = errors.New("unknown tutorial flow")
	ErrUnknownStep     = errors.New("unknown tutorial step")
	ErrUnknownPlayer   = errors.New("unknown tutorial player")
	ErrOutOfOrder      = errors.New("tutorial step completed out of order")
	ErrFlowSkipped     = errors.New("tutorial flow was skipped")
	ErrAlreadyRewarded = errors.New("step reward already granted")
	ErrStepNotReached  = errors.New("step is not completed yet")
	ErrDuplicateStep   = errors.New("duplicate step id in flow")
	ErrLimit           = errors.New("tutorial table limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// StepDef is one step of a tutorial flow definition.
type StepDef struct {
	ID       string
	Title    string
	Reward   int
	Optional bool
}

// Step is the resolved view of the step a player currently stands on.
type Step struct {
	FlowID  string
	ID      string
	Index   int
	Total   int
	Done    bool
	Skipped bool
	Version int
}

// Progress is the durable per player position inside one tutorial flow.
// Sub carries the sub state reached inside the current step.
type Progress struct {
	Player   string
	FlowID   string
	StepID   string
	Index    int
	Total    int
	Sub      int
	Rewarded bool
	Skipped  bool
	Seq      int64
}

// RewardRecord is one granted step reward.
type RewardRecord struct {
	Player string
	FlowID string
	StepID string
	Amount int
	Seq    int64
}

// Limits bounds the tutorial tables.
type Limits struct {
	MaxPlayers          int
	MaxRewardsPerPlayer int
	MaxStepsPerFlow     int
}

// flowDef is one registered tutorial flow plus its revision number.
type flowDef struct {
	steps   []StepDef
	version int
}

// playerState is the durable per player tutorial position.
type playerState struct {
	id        string
	flowID    string
	current   string
	completed map[string]bool
	rewarded  map[string]bool
	sub       int
	done      bool
	skipped   bool
	seq       int64
}

// TutorialProgressGuard owns flow definitions, player progress and rewards.
type TutorialProgressGuard struct {
	mu      sync.RWMutex
	limits  Limits
	flows   map[string]*flowDef
	players map[string]*playerState
	rewards map[string][]RewardRecord
	seq     int64
}

// NewTutorialProgressGuard builds a guard with the given caps.
func NewTutorialProgressGuard(limits Limits) *TutorialProgressGuard {
	return &TutorialProgressGuard{
		limits:  limits,
		flows:   make(map[string]*flowDef),
		players: make(map[string]*playerState),
		rewards: make(map[string][]RewardRecord),
	}
}

func stepIndex(steps []StepDef, id string) int {
	for i, s := range steps {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// progressOf builds the durable view of one player. Caller must hold the lock.
func (g *TutorialProgressGuard) progressOf(p *playerState) Progress {
	flow := g.flows[p.flowID]
	idx := stepIndex(flow.steps, p.current)
	return Progress{
		Player:   p.id,
		FlowID:   p.flowID,
		StepID:   p.current,
		Index:    idx + 1,
		Total:    len(flow.steps),
		Sub:      p.sub,
		Rewarded: p.rewarded[p.current],
		Skipped:  p.skipped,
		Seq:      p.seq,
	}
}

// SetFlow registers or replaces one tutorial flow definition.
func (g *TutorialProgressGuard) SetFlow(flowID string, steps []StepDef) error {
	if flowID == "" || len(steps) == 0 {
		return ErrBadFlow
	}
	if g.limits.MaxStepsPerFlow > 0 && len(steps) > g.limits.MaxStepsPerFlow {
		return ErrLimit
	}
	seen := make(map[string]bool, len(steps))
	for _, s := range steps {
		if s.ID == "" {
			return ErrBadStep
		}
		if seen[s.ID] {
			return ErrDuplicateStep
		}
		seen[s.ID] = true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	def := &flowDef{steps: append([]StepDef(nil), steps...)}
	if old, ok := g.flows[flowID]; ok {
		def.version = old.version + 1
	} else {
		def.version = 1
	}
	g.flows[flowID] = def

	// Re-anchor players whose current step vanished from the new revision so
	// they land on the first step they have not completed yet, never backwards.
	for _, p := range g.players {
		if p.flowID != flowID || p.skipped || p.done {
			continue
		}
		if stepIndex(def.steps, p.current) >= 0 {
			continue
		}
		next := -1
		for i, s := range def.steps {
			if !p.completed[s.ID] {
				next = i
				break
			}
		}
		if next < 0 {
			p.done = true
			p.current = def.steps[len(def.steps)-1].ID
		} else {
			p.current = def.steps[next].ID
		}
		p.sub = 0
	}
	return nil
}

// Flow returns a copy of one flow definition.
func (g *TutorialProgressGuard) Flow(flowID string) []StepDef {
	g.mu.RLock()
	defer g.mu.RUnlock()
	def, ok := g.flows[flowID]
	if !ok {
		return nil
	}
	return append([]StepDef(nil), def.steps...)
}

// Begin starts a player on the given flow at its first step. Re-beginning an
// existing player is a no-op so a relogin never rewinds progress or sub state.
func (g *TutorialProgressGuard) Begin(player string, flowID string) error {
	if player == "" {
		return ErrBadPlayer
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	def, ok := g.flows[flowID]
	if !ok {
		return ErrUnknownFlow
	}
	if _, ok := g.players[player]; ok {
		return nil
	}
	if g.limits.MaxPlayers > 0 && len(g.players) >= g.limits.MaxPlayers {
		return ErrLimit
	}
	g.seq++
	g.players[player] = &playerState{
		id:        player,
		flowID:    flowID,
		current:   def.steps[0].ID,
		completed: make(map[string]bool),
		rewarded:  make(map[string]bool),
		seq:       g.seq,
	}
	return nil
}

// Complete marks the named step of the player's current flow as done.
func (g *TutorialProgressGuard) Complete(player string, step string) (Progress, error) {
	if player == "" {
		return Progress{}, ErrBadPlayer
	}
	if step == "" {
		return Progress{}, ErrBadStep
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.players[player]
	if !ok {
		return Progress{}, ErrUnknownPlayer
	}
	if p.skipped {
		return Progress{}, ErrFlowSkipped
	}
	def := g.flows[p.flowID]
	idx := stepIndex(def.steps, step)
	if idx < 0 {
		return Progress{}, ErrUnknownStep
	}
	if p.completed[step] {
		// Idempotent replay of an already finished step: same position, same
		// sequence, no ledger writes.
		return g.progressOf(p), nil
	}
	if p.done || step != p.current {
		return Progress{}, ErrOutOfOrder
	}
	p.completed[step] = true
	p.sub = 0
	if idx+1 < len(def.steps) {
		p.current = def.steps[idx+1].ID
	} else {
		p.done = true
	}
	g.seq++
	p.seq = g.seq
	return g.progressOf(p), nil
}

// Skip marks the whole remaining flow of one player as skipped.
func (g *TutorialProgressGuard) Skip(player string, flowID string) error {
	if player == "" {
		return ErrBadPlayer
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.players[player]
	if !ok {
		return ErrUnknownPlayer
	}
	if _, ok := g.flows[flowID]; !ok {
		return ErrUnknownFlow
	}
	if p.skipped {
		return nil
	}
	p.skipped = true
	g.seq++
	p.seq = g.seq
	return nil
}

// Replay restarts the presentation of a flow for one player. Already granted
// rewards stay granted and a skipped flow stays skipped, so a replay can
// never pay a step reward twice.
func (g *TutorialProgressGuard) Replay(player string) error {
	if player == "" {
		return ErrBadPlayer
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.players[player]
	if !ok {
		return ErrUnknownPlayer
	}
	def := g.flows[p.flowID]
	p.current = def.steps[0].ID
	p.completed = make(map[string]bool)
	p.done = false
	p.sub = 0
	g.seq++
	p.seq = g.seq
	return nil
}

// Current resolves the step the player stands on right now.
func (g *TutorialProgressGuard) Current(player string) (Step, error) {
	if player == "" {
		return Step{}, ErrBadPlayer
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	p, ok := g.players[player]
	if !ok {
		return Step{}, ErrUnknownPlayer
	}
	def := g.flows[p.flowID]
	return Step{
		FlowID:  p.flowID,
		ID:      p.current,
		Index:   stepIndex(def.steps, p.current) + 1,
		Total:   len(def.steps),
		Done:    p.done || p.skipped,
		Skipped: p.skipped,
		Version: def.version,
	}, nil
}

// Grant hands out the reward attached to one completed step.
func (g *TutorialProgressGuard) Grant(player string, step string) error {
	if player == "" {
		return ErrBadPlayer
	}
	if step == "" {
		return ErrBadStep
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.players[player]
	if !ok {
		return ErrUnknownPlayer
	}
	def := g.flows[p.flowID]
	idx := stepIndex(def.steps, step)
	if idx < 0 {
		return ErrUnknownStep
	}
	if p.skipped {
		return ErrFlowSkipped
	}
	if p.rewarded[step] {
		return ErrAlreadyRewarded
	}
	if !p.completed[step] {
		return ErrStepNotReached
	}
	ledger := g.rewards[player]
	if g.limits.MaxRewardsPerPlayer > 0 && len(ledger) >= g.limits.MaxRewardsPerPlayer {
		return ErrLimit
	}
	g.seq++
	p.rewarded[step] = true
	g.rewards[player] = append(ledger, RewardRecord{
		Player: player,
		FlowID: p.flowID,
		StepID: step,
		Amount: def.steps[idx].Reward,
		Seq:    g.seq,
	})
	return nil
}

// State reads the durable progress of one player.
func (g *TutorialProgressGuard) State(player string) (Progress, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	p, ok := g.players[player]
	if !ok {
		return Progress{}, false
	}
	return g.progressOf(p), true
}

// Rewards returns the granted step rewards of one player, oldest first.
func (g *TutorialProgressGuard) Rewards(player string) []RewardRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]RewardRecord(nil), g.rewards[player]...)
}

// snapshotFlow is the serialized form of one flow definition.
type snapshotFlow struct {
	Steps   []StepDef `json:"steps"`
	Version int       `json:"version"`
}

// snapshotPlayer is the serialized form of one player's progress.
type snapshotPlayer struct {
	FlowID    string   `json:"flow_id"`
	Current   string   `json:"current"`
	Completed []string `json:"completed"`
	Rewarded  []string `json:"rewarded"`
	Sub       int      `json:"sub"`
	Done      bool     `json:"done"`
	Skipped   bool     `json:"skipped"`
	Seq       int64    `json:"seq"`
}

// snapshot is the full durable state of the guard.
type snapshot struct {
	Flows   map[string]snapshotFlow   `json:"flows"`
	Players map[string]snapshotPlayer `json:"players"`
	Rewards map[string][]RewardRecord `json:"rewards"`
	Seq     int64                     `json:"seq"`
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Snapshot serialises flow definitions, progress and the reward ledger.
func (g *TutorialProgressGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	snap := snapshot{
		Flows:   make(map[string]snapshotFlow, len(g.flows)),
		Players: make(map[string]snapshotPlayer, len(g.players)),
		Rewards: make(map[string][]RewardRecord, len(g.rewards)),
		Seq:     g.seq,
	}
	for id, def := range g.flows {
		snap.Flows[id] = snapshotFlow{
			Steps:   append([]StepDef(nil), def.steps...),
			Version: def.version,
		}
	}
	for id, p := range g.players {
		snap.Players[id] = snapshotPlayer{
			FlowID:    p.flowID,
			Current:   p.current,
			Completed: sortedKeys(p.completed),
			Rewarded:  sortedKeys(p.rewarded),
			Sub:       p.sub,
			Done:      p.done,
			Skipped:   p.skipped,
			Seq:       p.seq,
		}
	}
	for id, ledger := range g.rewards {
		snap.Rewards[id] = append([]RewardRecord(nil), ledger...)
	}
	return json.Marshal(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *TutorialProgressGuard) Restore(data []byte) error {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}

	flows := make(map[string]*flowDef, len(snap.Flows))
	for id, sf := range snap.Flows {
		if id == "" || len(sf.Steps) == 0 {
			return ErrBadSnapshot
		}
		seen := make(map[string]bool, len(sf.Steps))
		for _, s := range sf.Steps {
			if s.ID == "" || seen[s.ID] {
				return ErrBadSnapshot
			}
			seen[s.ID] = true
		}
		flows[id] = &flowDef{
			steps:   append([]StepDef(nil), sf.Steps...),
			version: sf.Version,
		}
	}

	players := make(map[string]*playerState, len(snap.Players))
	for id, sp := range snap.Players {
		if id == "" {
			return ErrBadSnapshot
		}
		def, ok := flows[sp.FlowID]
		if !ok || stepIndex(def.steps, sp.Current) < 0 {
			return ErrBadSnapshot
		}
		completed := make(map[string]bool, len(sp.Completed))
		for _, s := range sp.Completed {
			if stepIndex(def.steps, s) < 0 {
				return ErrBadSnapshot
			}
			completed[s] = true
		}
		rewarded := make(map[string]bool, len(sp.Rewarded))
		for _, s := range sp.Rewarded {
			if stepIndex(def.steps, s) < 0 {
				return ErrBadSnapshot
			}
			rewarded[s] = true
		}
		players[id] = &playerState{
			id:        id,
			flowID:    sp.FlowID,
			current:   sp.Current,
			completed: completed,
			rewarded:  rewarded,
			sub:       sp.Sub,
			done:      sp.Done,
			skipped:   sp.Skipped,
			seq:       sp.Seq,
		}
	}

	rewards := make(map[string][]RewardRecord, len(snap.Rewards))
	for id, ledger := range snap.Rewards {
		p, ok := players[id]
		if !ok {
			return ErrBadSnapshot
		}
		def := flows[p.flowID]
		for _, rec := range ledger {
			if rec.Player != id || rec.FlowID != p.flowID || stepIndex(def.steps, rec.StepID) < 0 {
				return ErrBadSnapshot
			}
		}
		rewards[id] = append([]RewardRecord(nil), ledger...)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.flows = flows
	g.players = players
	g.rewards = rewards
	g.seq = snap.Seq
	return nil
}
