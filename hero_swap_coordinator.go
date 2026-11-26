// Package task coordinates hero swaps between players after the pick phase:
// one pending request per player, a two way atomic exchange and a roster that
// closes when the match countdown ends.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Swap request states.
const (
	SwapPending   = "pending"
	SwapAccepted  = "accepted"
	SwapDeclined  = "declined"
	SwapCancelled = "cancelled"
	SwapExpired   = "expired"
)

// Roster states.
const (
	RosterOpen   = "open"
	RosterClosed = "closed"
)

// Errors returned by the coordinator.
var (
	ErrBlankMatch        = errors.New("match id is required")
	ErrEmptyRoster       = errors.New("roster needs players with heroes")
	ErrDuplicateHero     = errors.New("two players picked the same hero")
	ErrUnknownMatch      = errors.New("unknown match")
	ErrRosterLocked      = errors.New("roster of this match is already locked")
	ErrRosterClosed      = errors.New("roster is closed, swaps are over")
	ErrUnknownPlayer     = errors.New("player is not in the roster")
	ErrSelfSwap          = errors.New("a player cannot swap with themself")
	ErrPendingExists     = errors.New("player already has a pending swap")
	ErrRequestLimit      = errors.New("swap request quota reached for this match")
	ErrUnknownRequest    = errors.New("unknown swap request")
	ErrRequestNotPending = errors.New("swap request is not pending")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// SwapRequest is one player asking another for the hero.
type SwapRequest struct {
	RequestID  string    `json:"request_id"`
	MatchID    string    `json:"match_id"`
	FromPlayer string    `json:"from_player"`
	ToPlayer   string    `json:"to_player"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// SwapPair records one completed exchange.
type SwapPair struct {
	PlayerA string `json:"player_a"`
	PlayerB string `json:"player_b"`
	HeroA   string `json:"hero_a"`
	HeroB   string `json:"hero_b"`
}

// SwapResult is what an accepted request produced.
type SwapResult struct {
	RequestID string     `json:"request_id"`
	MatchID   string     `json:"match_id"`
	State     string     `json:"state"`
	Pairs     []SwapPair `json:"pairs"`
	Seq       int64      `json:"seq"`
}

// Limits bounds the live requests and the quota of one match.
type Limits struct {
	MaxPendingPerMatch  int
	MaxRequestsPerMatch int
	RequestTTL          time.Duration
}

// HeroSwapCoordinator owns the rosters, the pending requests and the sequence.
type HeroSwapCoordinator struct {
	mu            sync.RWMutex
	limits        Limits
	matches       map[string]*matchState
	nextRequestID uint64
}

type matchState struct {
	roster   map[string]string
	state    string
	seq      int64
	requests []*swapRequestState
}

type swapRequestState struct {
	request SwapRequest
	result  *SwapResult
}

const snapshotVersion = 1

type snapshotFile struct {
	Version       int                      `json:"version"`
	NextRequestID uint64                   `json:"next_request_id"`
	Matches       map[string]snapshotMatch `json:"matches"`
}

type snapshotMatch struct {
	Roster   map[string]string `json:"roster"`
	State    string            `json:"state"`
	Seq      int64             `json:"seq"`
	Requests []snapshotRequest `json:"requests"`
}

type snapshotRequest struct {
	Request SwapRequest `json:"request"`
	Result  *SwapResult `json:"result,omitempty"`
}

// NewHeroSwapCoordinator builds a coordinator with the given caps.
func NewHeroSwapCoordinator(limits Limits) *HeroSwapCoordinator {
	return &HeroSwapCoordinator{
		limits:        limits,
		matches:       make(map[string]*matchState),
		nextRequestID: 1,
	}
}

// LockRoster stores the final picks of one match before the countdown.
func (c *HeroSwapCoordinator) LockRoster(matchID string, picks map[string]string) error {
	if blank(matchID) {
		return ErrBlankMatch
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.ensureState()
	if _, exists := c.matches[matchID]; exists {
		return ErrRosterLocked
	}
	if err := validatePicks(picks); err != nil {
		return err
	}

	c.matches[matchID] = &matchState{
		roster: cloneRoster(picks),
		state:  RosterOpen,
	}
	return nil
}

// Request asks another player for a swap.
func (c *HeroSwapCoordinator) Request(matchID, fromPlayer, toPlayer string) (SwapRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	match := c.matches[matchID]
	if match == nil {
		return SwapRequest{}, ErrUnknownMatch
	}
	if match.state == RosterClosed {
		return SwapRequest{}, ErrRosterClosed
	}
	if _, ok := match.roster[fromPlayer]; !ok {
		return SwapRequest{}, ErrUnknownPlayer
	}
	if _, ok := match.roster[toPlayer]; !ok {
		return SwapRequest{}, ErrUnknownPlayer
	}
	if fromPlayer == toPlayer {
		return SwapRequest{}, ErrSelfSwap
	}

	now := time.Now()
	match.expirePending(now)
	if match.hasPendingPlayer(fromPlayer) || match.hasPendingPlayer(toPlayer) {
		return SwapRequest{}, ErrPendingExists
	}
	if len(match.requests) >= c.limits.MaxRequestsPerMatch {
		return SwapRequest{}, ErrRequestLimit
	}
	if match.pendingCount() >= c.limits.MaxPendingPerMatch {
		return SwapRequest{}, ErrRequestLimit
	}

	c.ensureState()
	request := SwapRequest{
		RequestID:  fmt.Sprintf("req-%d", c.nextRequestID),
		MatchID:    matchID,
		FromPlayer: fromPlayer,
		ToPlayer:   toPlayer,
		State:      SwapPending,
		CreatedAt:  now,
		ExpiresAt:  now.Add(c.limits.RequestTTL),
	}
	c.nextRequestID++
	match.requests = append(match.requests, &swapRequestState{request: request})
	return request, nil
}

// Accept commits the exchange for one pending request.
func (c *HeroSwapCoordinator) Accept(matchID, requestID string) (SwapResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	match := c.matches[matchID]
	if match == nil {
		return SwapResult{}, ErrUnknownRequest
	}
	requestState := match.findRequest(requestID)
	if requestState == nil {
		return SwapResult{}, ErrUnknownRequest
	}
	if requestState.request.State == SwapAccepted {
		if requestState.result == nil {
			return SwapResult{}, ErrRequestNotPending
		}
		return cloneSwapResult(*requestState.result), nil
	}
	if requestState.request.State != SwapPending {
		return SwapResult{}, ErrRequestNotPending
	}

	now := time.Now()
	match.expirePending(now)
	if requestState.request.State != SwapPending {
		return SwapResult{}, ErrRequestNotPending
	}
	if match.state == RosterClosed {
		return SwapResult{}, ErrRosterClosed
	}

	affected := match.reversePending(requestState)
	heroA, ok := match.roster[requestState.request.FromPlayer]
	if !ok {
		return SwapResult{}, ErrUnknownPlayer
	}
	heroB, ok := match.roster[requestState.request.ToPlayer]
	if !ok {
		return SwapResult{}, ErrUnknownPlayer
	}

	nextRoster := cloneRoster(match.roster)
	nextRoster[requestState.request.FromPlayer] = heroB
	nextRoster[requestState.request.ToPlayer] = heroA
	if hasDuplicateHeroes(nextRoster) {
		return SwapResult{}, ErrDuplicateHero
	}

	nextSeq := match.seq + 1
	results := make(map[*swapRequestState]SwapResult, len(affected))
	for _, state := range affected {
		results[state] = SwapResult{
			RequestID: state.request.RequestID,
			MatchID:   matchID,
			State:     SwapAccepted,
			Pairs: []SwapPair{{
				PlayerA: state.request.FromPlayer,
				PlayerB: state.request.ToPlayer,
				HeroA:   match.roster[state.request.FromPlayer],
				HeroB:   match.roster[state.request.ToPlayer],
			}},
			Seq: nextSeq,
		}
	}

	// The roster replacement and the sequence advance are one critical section:
	// readers see either both old values or both new values.
	match.roster = nextRoster
	match.seq = nextSeq
	for _, state := range affected {
		result := cloneSwapResult(results[state])
		state.request.State = SwapAccepted
		state.result = &result
	}

	return cloneSwapResult(results[requestState]), nil
}

// Decline turns a pending request down and leaves the heroes alone.
func (c *HeroSwapCoordinator) Decline(matchID, requestID string) error {
	return c.finishRequest(matchID, requestID, SwapDeclined)
}

// Cancel withdraws a pending request.
func (c *HeroSwapCoordinator) Cancel(matchID, requestID string) error {
	return c.finishRequest(matchID, requestID, SwapCancelled)
}

// Expire folds the pending requests whose lifetime ran out.
func (c *HeroSwapCoordinator) Expire(now time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	expired := 0
	for _, match := range c.matches {
		expired += match.expirePending(now)
	}
	return expired, nil
}

// CloseRoster seals a match when the countdown reaches its last stage.
func (c *HeroSwapCoordinator) CloseRoster(matchID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	match := c.matches[matchID]
	if match == nil {
		return ErrUnknownMatch
	}
	if match.state == RosterClosed {
		return nil
	}

	match.state = RosterClosed
	for _, state := range match.requests {
		if state.request.State == SwapPending {
			state.request.State = SwapCancelled
			state.result = nil
		}
	}
	return nil
}

// Roster reads the current hero of every player of one match.
func (c *HeroSwapCoordinator) Roster(matchID string) (map[string]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	match := c.matches[matchID]
	if match == nil {
		return nil, ErrUnknownMatch
	}
	return cloneRoster(match.roster), nil
}

// Pending lists the pending requests of one match, oldest first.
func (c *HeroSwapCoordinator) Pending(matchID string) []SwapRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()

	match := c.matches[matchID]
	if match == nil {
		return []SwapRequest{}
	}
	pending := make([]SwapRequest, 0)
	for _, state := range match.requests {
		if state.request.State == SwapPending {
			pending = append(pending, state.request)
		}
	}
	return pending
}

// Snapshot serialises the rosters, the requests and the sequence.
func (c *HeroSwapCoordinator) Snapshot() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snapshot := snapshotFile{
		Version:       snapshotVersion,
		NextRequestID: c.nextRequestID,
		Matches:       make(map[string]snapshotMatch, len(c.matches)),
	}
	if snapshot.NextRequestID == 0 {
		snapshot.NextRequestID = 1
	}
	for matchID, match := range c.matches {
		entry := snapshotMatch{
			Roster:   cloneRoster(match.roster),
			State:    match.state,
			Seq:      match.seq,
			Requests: make([]snapshotRequest, 0, len(match.requests)),
		}
		for _, state := range match.requests {
			request := snapshotRequest{Request: state.request}
			if state.result != nil {
				result := cloneSwapResult(*state.result)
				request.Result = &result
			}
			entry.Requests = append(entry.Requests, request)
		}
		snapshot.Matches[matchID] = entry
	}
	return json.Marshal(snapshot)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *HeroSwapCoordinator) Restore(data []byte) error {
	var snapshot snapshotFile
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ErrBadSnapshot
	}
	if snapshot.Version != snapshotVersion || snapshot.NextRequestID == 0 || snapshot.Matches == nil {
		return ErrBadSnapshot
	}

	restored := make(map[string]*matchState, len(snapshot.Matches))
	seenRequestIDs := make(map[string]struct{})
	maxRequestID := uint64(0)
	for matchID, stored := range snapshot.Matches {
		if blank(matchID) || len(stored.Roster) == 0 {
			return ErrBadSnapshot
		}
		if stored.State != RosterOpen && stored.State != RosterClosed {
			return ErrBadSnapshot
		}
		if stored.Seq < 0 {
			return ErrBadSnapshot
		}
		if err := validatePicks(stored.Roster); err != nil {
			return ErrBadSnapshot
		}

		match := &matchState{
			roster:   cloneRoster(stored.Roster),
			state:    stored.State,
			seq:      stored.Seq,
			requests: make([]*swapRequestState, 0, len(stored.Requests)),
		}
		pendingPlayers := make(map[string]struct{})
		for _, storedRequest := range stored.Requests {
			request := storedRequest.Request
			if request.RequestID == "" || request.MatchID != matchID {
				return ErrBadSnapshot
			}
			if _, duplicate := seenRequestIDs[request.RequestID]; duplicate {
				return ErrBadSnapshot
			}
			seenRequestIDs[request.RequestID] = struct{}{}
			if number, ok := requestIDNumber(request.RequestID); ok && number > maxRequestID {
				maxRequestID = number
			}
			if blank(request.FromPlayer) || blank(request.ToPlayer) || request.FromPlayer == request.ToPlayer {
				return ErrBadSnapshot
			}
			if _, ok := match.roster[request.FromPlayer]; !ok {
				return ErrBadSnapshot
			}
			if _, ok := match.roster[request.ToPlayer]; !ok {
				return ErrBadSnapshot
			}
			if request.CreatedAt.IsZero() || request.ExpiresAt.Before(request.CreatedAt) {
				return ErrBadSnapshot
			}
			if !validRequestState(request.State) {
				return ErrBadSnapshot
			}
			if request.State == SwapPending {
				if match.state == RosterClosed {
					return ErrBadSnapshot
				}
				if _, exists := pendingPlayers[request.FromPlayer]; exists {
					return ErrBadSnapshot
				}
				if _, exists := pendingPlayers[request.ToPlayer]; exists {
					return ErrBadSnapshot
				}
				pendingPlayers[request.FromPlayer] = struct{}{}
				pendingPlayers[request.ToPlayer] = struct{}{}
			}

			state := &swapRequestState{request: request}
			if storedRequest.Result != nil {
				if request.State != SwapAccepted || !validStoredResult(*storedRequest.Result, request, match) {
					return ErrBadSnapshot
				}
				result := cloneSwapResult(*storedRequest.Result)
				state.result = &result
			} else if request.State == SwapAccepted {
				return ErrBadSnapshot
			}
			match.requests = append(match.requests, state)
		}
		restored[matchID] = match
	}

	nextRequestID := snapshot.NextRequestID
	if maxRequestID >= nextRequestID {
		nextRequestID = maxRequestID + 1
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.matches = restored
	c.nextRequestID = nextRequestID
	return nil
}

func (c *HeroSwapCoordinator) ensureState() {
	if c.matches == nil {
		c.matches = make(map[string]*matchState)
	}
	if c.nextRequestID == 0 {
		c.nextRequestID = 1
	}
}

func (c *HeroSwapCoordinator) finishRequest(matchID, requestID, terminalState string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	match := c.matches[matchID]
	if match == nil {
		return ErrUnknownRequest
	}
	requestState := match.findRequest(requestID)
	if requestState == nil {
		return ErrUnknownRequest
	}
	if requestState.request.State != SwapPending {
		return ErrRequestNotPending
	}
	if !time.Now().Before(requestState.request.ExpiresAt) {
		requestState.request.State = SwapExpired
		return ErrRequestNotPending
	}
	requestState.request.State = terminalState
	requestState.result = nil
	return nil
}

func (m *matchState) findRequest(requestID string) *swapRequestState {
	for _, state := range m.requests {
		if state.request.RequestID == requestID {
			return state
		}
	}
	return nil
}

func (m *matchState) pendingCount() int {
	count := 0
	for _, state := range m.requests {
		if state.request.State == SwapPending {
			count++
		}
	}
	return count
}

func (m *matchState) hasPendingPlayer(player string) bool {
	for _, state := range m.requests {
		if state.request.State != SwapPending {
			continue
		}
		if state.request.FromPlayer == player || state.request.ToPlayer == player {
			return true
		}
	}
	return false
}

func (m *matchState) expirePending(now time.Time) int {
	expired := 0
	for _, state := range m.requests {
		if state.request.State == SwapPending && !now.Before(state.request.ExpiresAt) {
			state.request.State = SwapExpired
			state.result = nil
			expired++
		}
	}
	return expired
}

func (m *matchState) reversePending(request *swapRequestState) []*swapRequestState {
	affected := []*swapRequestState{request}
	for _, state := range m.requests {
		if state == request || state.request.State != SwapPending {
			continue
		}
		if state.request.FromPlayer == request.request.ToPlayer && state.request.ToPlayer == request.request.FromPlayer {
			affected = append(affected, state)
			break
		}
	}
	return affected
}

func validatePicks(picks map[string]string) error {
	if len(picks) == 0 {
		return ErrEmptyRoster
	}
	for player, hero := range picks {
		if blank(player) || blank(hero) {
			return ErrEmptyRoster
		}
	}

	heroes := make(map[string]struct{}, len(picks))
	for _, hero := range picks {
		if _, duplicate := heroes[hero]; duplicate {
			return ErrDuplicateHero
		}
		heroes[hero] = struct{}{}
	}
	return nil
}

func hasDuplicateHeroes(roster map[string]string) bool {
	heroes := make(map[string]struct{}, len(roster))
	for _, hero := range roster {
		if _, duplicate := heroes[hero]; duplicate {
			return true
		}
		heroes[hero] = struct{}{}
	}
	return false
}

func validRequestState(state string) bool {
	switch state {
	case SwapPending, SwapAccepted, SwapDeclined, SwapCancelled, SwapExpired:
		return true
	default:
		return false
	}
}

func validStoredResult(result SwapResult, request SwapRequest, match *matchState) bool {
	if result.RequestID != request.RequestID || result.MatchID != request.MatchID || result.State != SwapAccepted {
		return false
	}
	if result.Seq < 1 || result.Seq > match.seq || len(result.Pairs) != 1 {
		return false
	}
	pair := result.Pairs[0]
	if pair.PlayerA != request.FromPlayer || pair.PlayerB != request.ToPlayer {
		return false
	}
	if blank(pair.HeroA) || blank(pair.HeroB) || pair.HeroA == pair.HeroB {
		return false
	}
	if _, ok := match.roster[pair.PlayerA]; !ok {
		return false
	}
	if _, ok := match.roster[pair.PlayerB]; !ok {
		return false
	}
	return true
}

func requestIDNumber(requestID string) (uint64, bool) {
	if !strings.HasPrefix(requestID, "req-") {
		return 0, false
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(requestID, "req-"), 10, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

func cloneRoster(roster map[string]string) map[string]string {
	cloned := make(map[string]string, len(roster))
	for player, hero := range roster {
		cloned[player] = hero
	}
	return cloned
}

func cloneSwapResult(result SwapResult) SwapResult {
	cloned := result
	cloned.Pairs = append([]SwapPair(nil), result.Pairs...)
	return cloned
}

func blank(value string) bool {
	return strings.TrimSpace(value) == ""
}
