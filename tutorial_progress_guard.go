// Package task owns the new player tutorial ledger: ordered step completion,
// whole flow skips, one shot step rewards, flow revisions and restart recovery.
package task

import "errors"

var errNotImplemented = errors.New("not implemented")

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

// TutorialProgressGuard owns flow definitions, player progress and rewards.
type TutorialProgressGuard struct {
	limits Limits
}

// NewTutorialProgressGuard builds a guard with the given caps.
func NewTutorialProgressGuard(limits Limits) *TutorialProgressGuard {
	return &TutorialProgressGuard{limits: limits}
}

// SetFlow registers or replaces one tutorial flow definition.
func (g *TutorialProgressGuard) SetFlow(flowID string, steps []StepDef) error {
	return errNotImplemented
}

// Flow returns a copy of one flow definition.
func (g *TutorialProgressGuard) Flow(flowID string) []StepDef {
	return nil
}

// Begin starts a player on the given flow at its first step.
func (g *TutorialProgressGuard) Begin(player string, flowID string) error {
	return errNotImplemented
}

// Complete marks the named step of the player's current flow as done.
func (g *TutorialProgressGuard) Complete(player string, step string) (Progress, error) {
	return Progress{}, errNotImplemented
}

// Skip marks the whole remaining flow of one player as skipped.
func (g *TutorialProgressGuard) Skip(player string, flowID string) error {
	return errNotImplemented
}

// Replay restarts the presentation of a flow for one player.
func (g *TutorialProgressGuard) Replay(player string) error {
	return errNotImplemented
}

// Current resolves the step the player stands on right now.
func (g *TutorialProgressGuard) Current(player string) (Step, error) {
	return Step{}, errNotImplemented
}

// Grant hands out the reward attached to one completed step.
func (g *TutorialProgressGuard) Grant(player string, step string) error {
	return errNotImplemented
}

// State reads the durable progress of one player.
func (g *TutorialProgressGuard) State(player string) (Progress, bool) {
	return Progress{}, false
}

// Rewards returns the granted step rewards of one player, oldest first.
func (g *TutorialProgressGuard) Rewards(player string) []RewardRecord {
	return nil
}

// Snapshot serialises flow definitions, progress and the reward ledger.
func (g *TutorialProgressGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *TutorialProgressGuard) Restore(data []byte) error {
	return errNotImplemented
}
