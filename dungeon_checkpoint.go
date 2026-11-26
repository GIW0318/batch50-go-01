// Package task tracks instanced dungeon runs: checkpoint progress shared by the
// whole party, reconnects that must land on the instance checkpoint, and the
// reclaim queue used when rewards of a rolled back checkpoint are withdrawn.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

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
	mu   sync.RWMutex
	cfg  RunConfig
	runs map[string]*runState
}

type runState struct {
	Run
	Reclaims []Reclaim `json:"reclaims"`
}

type snapshotPayload struct {
	Version int                  `json:"version"`
	Runs    map[string]*runState `json:"runs"`
}

const snapshotVersion = 1

// NewDungeonCheckpoint builds an empty store with the given bounds.
func NewDungeonCheckpoint(cfg RunConfig) *DungeonCheckpoint {
	return &DungeonCheckpoint{
		cfg:  cfg,
		runs: make(map[string]*runState),
	}
}

// OpenRun creates a run with the whole party online at the entrance.
func (d *DungeonCheckpoint) OpenRun(runID string, party []string) (Run, error) {
	if strings.TrimSpace(runID) == "" {
		return Run{}, ErrBlankRun
	}
	if len(party) == 0 {
		return Run{}, ErrEmptyParty
	}

	seenMembers := make(map[string]struct{}, len(party))
	for _, memberID := range party {
		if strings.TrimSpace(memberID) == "" {
			return Run{}, ErrBlankMember
		}
		if _, duplicated := seenMembers[memberID]; duplicated {
			return Run{}, ErrDuplicateMember
		}
		seenMembers[memberID] = struct{}{}
	}
	if len(party) > d.cfg.MaxParty {
		return Run{}, ErrPartyFull
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.runs == nil {
		d.runs = make(map[string]*runState)
	}
	if _, exists := d.runs[runID]; exists {
		return Run{}, ErrRunExists
	}
	if len(d.runs) >= d.cfg.MaxRuns {
		return Run{}, ErrRunLimit
	}

	members := make([]Member, len(party))
	for i, memberID := range party {
		members[i] = Member{
			PlayerID:   memberID,
			State:      MemberOnline,
			Checkpoint: 0,
			Ordinal:    i,
		}
	}

	run := &runState{
		Run: Run{
			ID:         runID,
			State:      RunActive,
			Checkpoint: 0,
			Members:    members,
			Grants:     []Grant{},
			Seq:        0,
		},
		Reclaims: []Reclaim{},
	}
	d.runs[runID] = run
	return cloneRun(run), nil
}

// CloseRun seals a run so no further progress or rewards are accepted.
func (d *DungeonCheckpoint) CloseRun(runID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return err
	}
	run.State = RunClosed
	return nil
}

// Join brings a member back into a run and reports where it lands.
func (d *DungeonCheckpoint) Join(runID, playerID string, claimedCheckpoint int) (Member, error) {
	_ = claimedCheckpoint

	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return Member{}, err
	}

	index := memberIndex(run, playerID)
	if index < 0 {
		return Member{}, ErrUnknownMember
	}
	if run.Members[index].State == MemberOnline {
		return Member{}, ErrMemberOnline
	}

	run.Members[index].State = MemberOnline
	run.Members[index].Checkpoint = run.Checkpoint
	return run.Members[index], nil
}

// Leave marks a member offline without dropping its slot.
func (d *DungeonCheckpoint) Leave(runID, playerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return err
	}

	index := memberIndex(run, playerID)
	if index < 0 {
		return ErrUnknownMember
	}
	run.Members[index].State = MemberOffline
	return nil
}

// Advance moves the whole party to the next checkpoint.
func (d *DungeonCheckpoint) Advance(runID string, checkpoint int) (Run, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return Run{}, err
	}
	if run.State == RunClosed {
		return Run{}, ErrRunClosed
	}

	current := run.Checkpoint
	if checkpoint <= current {
		return Run{}, ErrCheckpointRegress
	}
	if checkpoint != current+1 {
		return Run{}, ErrCheckpointGap
	}
	if checkpoint > d.cfg.Checkpoints {
		return Run{}, ErrCheckpointLimit
	}
	for _, member := range run.Members {
		if member.State != MemberOnline {
			return Run{}, ErrMemberOffline
		}
	}

	run.Checkpoint = checkpoint
	for i := range run.Members {
		run.Members[i].Checkpoint = checkpoint
	}
	return cloneRun(run), nil
}

// GrantReward hands one reward to a member at the current checkpoint.
func (d *DungeonCheckpoint) GrantReward(runID, playerID, itemID string, qty int) (Grant, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return Grant{}, err
	}
	if run.State == RunClosed {
		return Grant{}, ErrRunClosed
	}
	if strings.TrimSpace(itemID) == "" {
		return Grant{}, ErrBlankItem
	}
	if qty <= 0 {
		return Grant{}, ErrBadQuantity
	}

	index := memberIndex(run, playerID)
	if index < 0 {
		return Grant{}, ErrUnknownMember
	}
	if run.Members[index].State != MemberOnline {
		return Grant{}, ErrMemberOffline
	}

	for _, grant := range run.Grants {
		if grant.PlayerID == playerID &&
			grant.ItemID == itemID &&
			grant.Checkpoint == run.Checkpoint {
			return grant, nil
		}
	}
	if len(run.Grants) >= d.cfg.MaxGrants {
		return Grant{}, ErrGrantLimit
	}

	run.Seq++
	grant := Grant{
		ID:         fmt.Sprintf("grant-%d", run.Seq),
		RunID:      runID,
		PlayerID:   playerID,
		ItemID:     itemID,
		Qty:        qty,
		Checkpoint: run.Checkpoint,
		Seq:        run.Seq,
	}
	run.Grants = append(run.Grants, grant)
	return grant, nil
}

// Rollback rewinds the run to an earlier checkpoint.
func (d *DungeonCheckpoint) Rollback(runID string, checkpoint int) (Run, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return Run{}, err
	}
	if run.State == RunClosed {
		return Run{}, ErrRunClosed
	}
	if checkpoint < 0 {
		return Run{}, ErrCheckpointRegress
	}
	if checkpoint >= run.Checkpoint {
		return Run{}, ErrRollbackForward
	}

	retained := make([]Grant, 0, len(run.Grants))
	for _, grant := range run.Grants {
		if grant.Checkpoint <= checkpoint {
			retained = append(retained, grant)
			continue
		}
		if !reclaimExists(run, grant.ID) {
			run.Seq++
			run.Reclaims = append(run.Reclaims, Reclaim{
				GrantID:    grant.ID,
				RunID:      runID,
				PlayerID:   grant.PlayerID,
				ItemID:     grant.ItemID,
				Qty:        grant.Qty,
				Checkpoint: grant.Checkpoint,
				State:      ReclaimPending,
				Seq:        run.Seq,
			})
		}
	}
	run.Grants = retained

	run.Checkpoint = checkpoint
	for i := range run.Members {
		run.Members[i].Checkpoint = checkpoint
	}
	return cloneRun(run), nil
}

// Run reads one run.
func (d *DungeonCheckpoint) Run(runID string) (Run, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	run, ok := d.runs[runID]
	if !ok {
		return Run{}, false
	}
	return cloneRun(run), true
}

// Members lists the party of one run in join order.
func (d *DungeonCheckpoint) Members(runID string) []Member {
	d.mu.RLock()
	defer d.mu.RUnlock()

	run, ok := d.runs[runID]
	if !ok {
		return nil
	}
	members := make([]Member, len(run.Members))
	copy(members, run.Members)
	return members
}

// Grants lists the reward ledger of one run, oldest first.
func (d *DungeonCheckpoint) Grants(runID string) []Grant {
	d.mu.RLock()
	defer d.mu.RUnlock()

	run, ok := d.runs[runID]
	if !ok {
		return nil
	}
	grants := make([]Grant, len(run.Grants))
	copy(grants, run.Grants)
	return grants
}

// Reclaims lists the reclaim queue of one run, oldest first.
func (d *DungeonCheckpoint) Reclaims(runID string) []Reclaim {
	d.mu.RLock()
	defer d.mu.RUnlock()

	run, ok := d.runs[runID]
	if !ok {
		return nil
	}
	reclaims := make([]Reclaim, len(run.Reclaims))
	copy(reclaims, run.Reclaims)
	return reclaims
}

// ClaimReclaim hands one pending reclaim to the reward worker.
func (d *DungeonCheckpoint) ClaimReclaim(runID, playerID, itemID string) (Reclaim, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	run, err := d.runLocked(runID)
	if err != nil {
		return Reclaim{}, err
	}

	for i := range run.Reclaims {
		reclaim := &run.Reclaims[i]
		if reclaim.State == ReclaimPending &&
			reclaim.PlayerID == playerID &&
			reclaim.ItemID == itemID {
			reclaim.State = ReclaimClaimed
			return *reclaim, nil
		}
	}
	return Reclaim{}, ErrUnknownReclaim
}

// Snapshot serialises every run, its ledger and its reclaim queue.
func (d *DungeonCheckpoint) Snapshot() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	runs := d.runs
	if runs == nil {
		runs = make(map[string]*runState)
	}
	payload := snapshotPayload{Version: snapshotVersion, Runs: runs}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode dungeon checkpoint snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole store, all or nothing.
func (d *DungeonCheckpoint) Restore(data []byte) error {
	var payload snapshotPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&payload); err != nil {
		return ErrBadSnapshot
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrBadSnapshot
	}
	if err := validateSnapshot(&payload); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.runs == nil {
		d.runs = make(map[string]*runState)
	}
	d.runs = payload.Runs
	return nil
}

func (d *DungeonCheckpoint) runLocked(runID string) (*runState, error) {
	if d.runs == nil {
		d.runs = make(map[string]*runState)
	}
	run, ok := d.runs[runID]
	if !ok {
		return nil, ErrUnknownRun
	}
	return run, nil
}

func memberIndex(run *runState, playerID string) int {
	for i := range run.Members {
		if run.Members[i].PlayerID == playerID {
			return i
		}
	}
	return -1
}

func reclaimExists(run *runState, grantID string) bool {
	for i := range run.Reclaims {
		if run.Reclaims[i].GrantID == grantID {
			return true
		}
	}
	return false
}

func cloneRun(run *runState) Run {
	cloned := run.Run
	cloned.Members = append(make([]Member, 0, len(run.Members)), run.Members...)
	cloned.Grants = append(make([]Grant, 0, len(run.Grants)), run.Grants...)
	return cloned
}

func validateSnapshot(payload *snapshotPayload) error {
	if payload == nil || payload.Version != snapshotVersion || payload.Runs == nil {
		return ErrBadSnapshot
	}

	for runID, run := range payload.Runs {
		if run == nil || strings.TrimSpace(runID) == "" || run.ID != runID {
			return ErrBadSnapshot
		}
		if run.State != RunActive && run.State != RunClosed {
			return ErrBadSnapshot
		}
		if run.Checkpoint < 0 || len(run.Members) == 0 {
			return ErrBadSnapshot
		}

		memberIDs := make(map[string]struct{}, len(run.Members))
		for i, member := range run.Members {
			if strings.TrimSpace(member.PlayerID) == "" || member.Ordinal != i {
				return ErrBadSnapshot
			}
			if _, duplicated := memberIDs[member.PlayerID]; duplicated {
				return ErrBadSnapshot
			}
			memberIDs[member.PlayerID] = struct{}{}
			if member.State != MemberOnline && member.State != MemberOffline {
				return ErrBadSnapshot
			}
			if member.Checkpoint != run.Checkpoint {
				return ErrBadSnapshot
			}
		}

		grantIDs := make(map[string]struct{}, len(run.Grants))
		grantKeys := make(map[string]struct{}, len(run.Grants))
		var maxSeq int64
		for _, grant := range run.Grants {
			if grant.ID == "" || grant.RunID != runID || grant.Qty <= 0 || grant.Seq <= 0 {
				return ErrBadSnapshot
			}
			if strings.TrimSpace(grant.ItemID) == "" || grant.Checkpoint < 0 {
				return ErrBadSnapshot
			}
			if _, ok := memberIDs[grant.PlayerID]; !ok {
				return ErrBadSnapshot
			}
			if _, duplicated := grantIDs[grant.ID]; duplicated {
				return ErrBadSnapshot
			}
			grantIDs[grant.ID] = struct{}{}
			key := fmt.Sprintf("%s\x00%s\x00%d", grant.PlayerID, grant.ItemID, grant.Checkpoint)
			if _, duplicated := grantKeys[key]; duplicated {
				return ErrBadSnapshot
			}
			grantKeys[key] = struct{}{}
			if grant.Seq > maxSeq {
				maxSeq = grant.Seq
			}
		}

		reclaimGrantIDs := make(map[string]struct{}, len(run.Reclaims))
		for _, reclaim := range run.Reclaims {
			if reclaim.GrantID == "" || reclaim.RunID != runID || reclaim.Qty <= 0 || reclaim.Seq <= 0 {
				return ErrBadSnapshot
			}
			if strings.TrimSpace(reclaim.ItemID) == "" || reclaim.Checkpoint < 0 {
				return ErrBadSnapshot
			}
			if _, ok := memberIDs[reclaim.PlayerID]; !ok {
				return ErrBadSnapshot
			}
			if _, duplicated := reclaimGrantIDs[reclaim.GrantID]; duplicated {
				return ErrBadSnapshot
			}
			reclaimGrantIDs[reclaim.GrantID] = struct{}{}
			if reclaim.State != ReclaimPending && reclaim.State != ReclaimClaimed {
				return ErrBadSnapshot
			}
			if reclaim.Seq > maxSeq {
				maxSeq = reclaim.Seq
			}
		}
		if run.Seq < maxSeq {
			return ErrBadSnapshot
		}
	}
	return nil
}
