// Package task tracks instanced dungeon runs: checkpoint progress shared by the
// whole party, reconnects that must land on the instance checkpoint, and the
// reclaim queue used when rewards of a rolled back checkpoint are withdrawn.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Run states.
const (
	RunActive = "active"
	RunClosed = "closed"
)

// Party member states.
const (
	MemberOnline  = "online"
	MemberOffline = "offline"
)

// Reclaim states.
const (
	ReclaimPending = "pending"
	ReclaimClaimed = "claimed"
)

// Errors returned by the checkpoint store.
var (
	ErrBlankRun          = errors.New("run id is required")
	ErrRunExists         = errors.New("run already exists")
	ErrUnknownRun        = errors.New("run not found")
	ErrRunLimit          = errors.New("run limit reached")
	ErrEmptyParty        = errors.New("party must contain at least one member")
	ErrBlankMember       = errors.New("party member id is required")
	ErrDuplicateMember   = errors.New("party member is duplicated")
	ErrPartyFull         = errors.New("party size limit reached")
	ErrUnknownMember     = errors.New("party member not found")
	ErrMemberOnline      = errors.New("party member is already online")
	ErrMemberOffline     = errors.New("party member is offline")
	ErrCheckpointGap     = errors.New("checkpoint must advance one step at a time")
	ErrCheckpointRegress = errors.New("checkpoint cannot move backwards")
	ErrCheckpointLimit   = errors.New("checkpoint limit reached")
	ErrRunClosed         = errors.New("run is closed")
	ErrBlankItem         = errors.New("reward item is required")
	ErrBadQuantity       = errors.New("reward quantity must be positive")
	ErrGrantLimit        = errors.New("reward grant limit reached")
	ErrRollbackForward   = errors.New("rollback target is not behind the current checkpoint")
	ErrUnknownReclaim    = errors.New("no pending reclaim for this reward")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// RunConfig bounds one dungeon instance store.
type RunConfig struct {
	MaxRuns     int `json:"max_runs"`
	MaxParty    int `json:"max_party"`
	Checkpoints int `json:"checkpoints"`
	MaxGrants   int `json:"max_grants"`
}

// Member is the per player row of one run.
type Member struct {
	PlayerID   string `json:"player_id"`
	State      string `json:"state"`
	Checkpoint int    `json:"checkpoint"`
	Ordinal    int    `json:"ordinal"`
}

// Grant is one reward already handed out inside a run.
type Grant struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	PlayerID   string `json:"player_id"`
	ItemID     string `json:"item_id"`
	Qty        int    `json:"qty"`
	Checkpoint int    `json:"checkpoint"`
	Seq        int64  `json:"seq"`
}

// Reclaim is one reward waiting to be taken back from a player.
type Reclaim struct {
	GrantID    string `json:"grant_id"`
	RunID      string `json:"run_id"`
	PlayerID   string `json:"player_id"`
	ItemID     string `json:"item_id"`
	Qty        int    `json:"qty"`
	Checkpoint int    `json:"checkpoint"`
	State      string `json:"state"`
	Seq        int64  `json:"seq"`
}

// Run is the whole instance state of one dungeon run.
type Run struct {
	ID         string   `json:"id"`
	State      string   `json:"state"`
	Checkpoint int      `json:"checkpoint"`
	Members    []Member `json:"members"`
	Grants     []Grant  `json:"grants"`
	Seq        int64    `json:"seq"`
}

// DungeonCheckpoint owns every live run, its reward ledger and reclaim queue.
type DungeonCheckpoint struct {
	cfg RunConfig
}

// NewDungeonCheckpoint builds an empty store with the given bounds.
func NewDungeonCheckpoint(cfg RunConfig) *DungeonCheckpoint {
	return &DungeonCheckpoint{cfg: cfg}
}

// OpenRun creates a run with the whole party online at the entrance.
func (d *DungeonCheckpoint) OpenRun(runID string, party []string) (Run, error) {
	return Run{}, errNotImplemented
}

// CloseRun seals a run so no further progress or rewards are accepted.
func (d *DungeonCheckpoint) CloseRun(runID string) error {
	return errNotImplemented
}

// Join brings a member back into a run and reports where it lands.
func (d *DungeonCheckpoint) Join(runID, playerID string, claimedCheckpoint int) (Member, error) {
	return Member{}, errNotImplemented
}

// Leave marks a member offline without dropping its slot.
func (d *DungeonCheckpoint) Leave(runID, playerID string) error {
	return errNotImplemented
}

// Advance moves the whole party to the next checkpoint.
func (d *DungeonCheckpoint) Advance(runID string, checkpoint int) (Run, error) {
	return Run{}, errNotImplemented
}

// GrantReward hands one reward to a member at the current checkpoint.
func (d *DungeonCheckpoint) GrantReward(runID, playerID, itemID string, qty int) (Grant, error) {
	return Grant{}, errNotImplemented
}

// Rollback rewinds the run to an earlier checkpoint.
func (d *DungeonCheckpoint) Rollback(runID string, checkpoint int) (Run, error) {
	return Run{}, errNotImplemented
}

// Run reads one run.
func (d *DungeonCheckpoint) Run(runID string) (Run, bool) {
	return Run{}, false
}

// Members lists the party of one run in join order.
func (d *DungeonCheckpoint) Members(runID string) []Member {
	return nil
}

// Grants lists the reward ledger of one run, oldest first.
func (d *DungeonCheckpoint) Grants(runID string) []Grant {
	return nil
}

// Reclaims lists the reclaim queue of one run, oldest first.
func (d *DungeonCheckpoint) Reclaims(runID string) []Reclaim {
	return nil
}

// ClaimReclaim hands one pending reclaim to the reward worker.
func (d *DungeonCheckpoint) ClaimReclaim(runID, playerID, itemID string) (Reclaim, error) {
	return Reclaim{}, errNotImplemented
}

// Snapshot serialises every run, its ledger and its reclaim queue.
func (d *DungeonCheckpoint) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole store, all or nothing.
func (d *DungeonCheckpoint) Restore(data []byte) error {
	return errNotImplemented
}
