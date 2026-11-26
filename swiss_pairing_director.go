// Package task keeps the pairing book of one online swiss event: the roster,
// the pairings of every round, the table results and the standings table
// derived from them.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Float directions recorded for a player inside one round.
const (
	FloatNone = "none"
	FloatUp   = "up"
	FloatDown = "down"
)

// Points awarded per outcome.
const (
	PointWin  = 1.0
	PointDraw = 0.5
	PointBye  = 1.0
)

// Errors returned by the director.
var (
	ErrNoPlayers       = errors.New("player list is empty")
	ErrBlankPlayer     = errors.New("player id is required")
	ErrDuplicatePlayer = errors.New("duplicate player id")
	ErrRosterLocked    = errors.New("roster is locked once the event has started")
	ErrNotRegistered   = errors.New("player is not registered")
	ErrWithdrawn       = errors.New("player already withdrew")
	ErrBadRound        = errors.New("round number is out of range")
	ErrRoundPaired     = errors.New("round is already paired")
	ErrRoundNotPaired  = errors.New("round is not paired")
	ErrRoundLocked     = errors.New("a later round is already paired")
	ErrResultsPending  = errors.New("the previous round still has unreported tables")
	ErrNoTable         = errors.New("table does not exist")
	ErrBadWinner       = errors.New("winner must be one of the two players")
	ErrResultConflict  = errors.New("table already reported with a different result")
	ErrBoardLimit      = errors.New("board limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Player is one entry of the event roster. Rating only seeds the first round.
type Player struct {
	ID        string `json:"id"`
	Rating    int    `json:"rating"`
	Withdrawn bool   `json:"withdrawn"`
}

// Config bounds one event.
type Config struct {
	Boards      int
	FloatWindow int
	MaxRounds   int
}

// Pairing is one line of a round: a table with two players, or a bye.
// Bye entries carry ByeID, leave WhiteID and BlackID empty, use Table 0 and
// are appended after every played table.
type Pairing struct {
	Round   int    `json:"round"`
	Table   int    `json:"table"`
	WhiteID string `json:"white_id"`
	BlackID string `json:"black_id"`
	ByeID   string `json:"bye_id"`
	Float   string `json:"float"`
}

// TableResult is the reported outcome of one table. Draw tables keep WinnerID
// empty.
type TableResult struct {
	Round    int    `json:"round"`
	Table    int    `json:"table"`
	WinnerID string `json:"winner_id"`
	Draw     bool   `json:"draw"`
	Reported bool   `json:"reported"`
}

// Standing is one row of the standings table, ranked from one.
type Standing struct {
	PlayerID  string   `json:"player_id"`
	Rank      int      `json:"rank"`
	Score     float64  `json:"score"`
	Byes      int      `json:"byes"`
	Floats    int      `json:"floats"`
	Opponents []string `json:"opponents"`
}

type roundRecord struct {
	pairings   []Pairing
	results    map[int]TableResult
	floatMarks map[string]string
}

// SwissPairingDirector owns the roster, the pairing book and the results.
type SwissPairingDirector struct {
	mu     sync.RWMutex
	cfg    Config
	roster []Player
	locked bool
	rounds map[int]*roundRecord
}

// NewSwissPairingDirector builds a director for one event.
func NewSwissPairingDirector(cfg Config) *SwissPairingDirector {
	return &SwissPairingDirector{cfg: cfg, rounds: make(map[int]*roundRecord)}
}

// Register loads the roster. It is only accepted before the first pairing.
func (d *SwissPairingDirector) Register(players []Player) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.locked {
		return ErrRosterLocked
	}
	if len(players) == 0 {
		return ErrNoPlayers
	}

	seen := make(map[string]struct{}, len(players))
	roster := make([]Player, 0, len(players))
	for _, p := range players {
		if strings.TrimSpace(p.ID) == "" {
			return ErrBlankPlayer
		}
		if _, exists := seen[p.ID]; exists {
			return ErrDuplicatePlayer
		}
		seen[p.ID] = struct{}{}
		roster = append(roster, p)
	}

	d.roster = roster
	return nil
}

// PairRound pairs one round and returns its table list, bye included.
func (d *SwissPairingDirector) PairRound(round int) ([]Pairing, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cfg.MaxRounds < 1 || round < 1 || round > d.cfg.MaxRounds {
		return nil, ErrBadRound
	}
	if _, paired := d.rounds[round]; paired {
		return nil, ErrRoundPaired
	}
	if len(d.roster) == 0 {
		return nil, ErrNoPlayers
	}

	if round == 1 {
		if len(d.rounds) != 0 {
			return nil, ErrRoundLocked
		}
	} else {
		previous, ok := d.rounds[round-1]
		if !ok {
			for existing := range d.rounds {
				if existing > round {
					return nil, ErrRoundLocked
				}
			}
			return nil, ErrRoundNotPaired
		}
		if tableResultsPending(previous) {
			return nil, ErrResultsPending
		}
		for existing := range d.rounds {
			if existing > round {
				return nil, ErrRoundLocked
			}
		}
	}

	pairings, marks := d.buildRound(round)
	tables := len(pairings)
	if tables > 0 && pairings[tables-1].Table == 0 {
		tables--
	}
	if tables > d.cfg.Boards {
		return nil, ErrBoardLimit
	}

	record := &roundRecord{
		pairings:   pairings,
		results:    make(map[int]TableResult),
		floatMarks: marks,
	}
	d.rounds[round] = record
	d.locked = true
	return clonePairings(pairings), nil
}

// ReportResult records the outcome of one table.
func (d *SwissPairingDirector) ReportResult(round, table int, winnerID string, draw bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if round < 1 || round > d.cfg.MaxRounds {
		return ErrBadRound
	}
	record, ok := d.rounds[round]
	if !ok {
		return ErrRoundNotPaired
	}
	var pairing *Pairing
	for i := range record.pairings {
		if record.pairings[i].Table == table && table != 0 {
			pairing = &record.pairings[i]
			break
		}
	}
	if pairing == nil {
		return ErrNoTable
	}
	if draw {
		if winnerID != "" {
			return ErrBadWinner
		}
	} else if winnerID != pairing.WhiteID && winnerID != pairing.BlackID {
		return ErrBadWinner
	}

	if previous, exists := record.results[table]; exists {
		if previous.Draw == draw && previous.WinnerID == winnerID {
			return nil
		}
		return ErrResultConflict
	}

	record.results[table] = TableResult{
		Round:    round,
		Table:    table,
		WinnerID: winnerID,
		Draw:     draw,
		Reported: true,
	}
	return nil
}

// Standings returns the ranked table, best player first.
func (d *SwissPairingDirector) Standings() []Standing {
	d.mu.RLock()
	defer d.mu.RUnlock()

	scores := make(map[string]float64, len(d.roster))
	byes := make(map[string]int, len(d.roster))
	floats := make(map[string]int, len(d.roster))
	opponents := make(map[string][]string, len(d.roster))

	for _, round := range sortedRoundNumbers(d.rounds) {
		record := d.rounds[round]
		for _, pairing := range record.pairings {
			if pairing.Table == 0 {
				scores[pairing.ByeID] += PointBye
				byes[pairing.ByeID]++
				continue
			}
			opponents[pairing.WhiteID] = append(opponents[pairing.WhiteID], pairing.BlackID)
			opponents[pairing.BlackID] = append(opponents[pairing.BlackID], pairing.WhiteID)
			result, reported := record.results[pairing.Table]
			if !reported {
				continue
			}
			if result.Draw {
				scores[pairing.WhiteID] += PointDraw
				scores[pairing.BlackID] += PointDraw
			} else {
				scores[result.WinnerID] += PointWin
			}
		}
		for playerID, direction := range record.floatMarks {
			if direction == FloatUp || direction == FloatDown {
				floats[playerID]++
			}
		}
	}

	type standingRow struct {
		standing  Standing
		seed      int
		opponents float64
	}
	rows := make([]standingRow, 0, len(d.roster))
	for seed, player := range d.roster {
		oppScore := 0.0
		for _, opponentID := range opponents[player.ID] {
			oppScore += scores[opponentID]
		}
		rows = append(rows, standingRow{
			standing: Standing{
				PlayerID:  player.ID,
				Score:     scores[player.ID],
				Byes:      byes[player.ID],
				Floats:    floats[player.ID],
				Opponents: append([]string(nil), opponents[player.ID]...),
			},
			seed:      seed,
			opponents: oppScore,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].standing.Score != rows[j].standing.Score {
			return rows[i].standing.Score > rows[j].standing.Score
		}
		if rows[i].opponents != rows[j].opponents {
			return rows[i].opponents > rows[j].opponents
		}
		return rows[i].seed < rows[j].seed
	})

	result := make([]Standing, len(rows))
	for i := range rows {
		result[i] = rows[i].standing
		result[i].Rank = i + 1
	}
	return result
}

// Unpair drops the pairings of one round and reverses its reported results.
func (d *SwissPairingDirector) Unpair(round int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if round < 1 || round > d.cfg.MaxRounds {
		return ErrBadRound
	}
	if _, ok := d.rounds[round]; !ok {
		return ErrRoundNotPaired
	}
	for existing := range d.rounds {
		if existing > round {
			return ErrRoundLocked
		}
	}
	delete(d.rounds, round)
	return nil
}

// Round returns a copy of the pairings of one round.
func (d *SwissPairingDirector) Round(round int) ([]Pairing, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if round < 1 || round > d.cfg.MaxRounds {
		return nil, ErrBadRound
	}
	record, ok := d.rounds[round]
	if !ok {
		return nil, ErrRoundNotPaired
	}
	return clonePairings(record.pairings), nil
}

// ByeHistory lists the rounds in which one player sat out, oldest first.
func (d *SwissPairingDirector) ByeHistory(playerID string) []int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var history []int
	for _, round := range sortedRoundNumbers(d.rounds) {
		for _, pairing := range d.rounds[round].pairings {
			if pairing.Table == 0 && pairing.ByeID == playerID {
				history = append(history, round)
				break
			}
		}
	}
	return history
}

// Floats counts how many rounds one player was moved between score groups.
func (d *SwissPairingDirector) Floats(playerID string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	count := 0
	for _, round := range d.rounds {
		direction := round.floatMarks[playerID]
		if direction == FloatUp || direction == FloatDown {
			count++
		}
	}
	return count
}

// Withdraw removes one player from every later pairing.
func (d *SwissPairingDirector) Withdraw(playerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for i := range d.roster {
		if d.roster[i].ID != playerID {
			continue
		}
		if d.roster[i].Withdrawn {
			return ErrWithdrawn
		}
		d.roster[i].Withdrawn = true
		return nil
	}
	return ErrNotRegistered
}

const snapshotVersion = 1

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotState struct {
	Config       Config                `json:"config"`
	Roster       []snapshotPlayer      `json:"roster"`
	Locked       bool                  `json:"locked"`
	NextSequence int                   `json:"next_sequence"`
	Rounds       []snapshotRound       `json:"rounds"`
	ByeLedger    []snapshotByeRecord   `json:"bye_ledger"`
	FloatLedger  []snapshotFloatRecord `json:"float_ledger"`
}

type snapshotPlayer struct {
	ID        string `json:"id"`
	Rating    int    `json:"rating"`
	Withdrawn bool   `json:"withdrawn"`
	Seed      int    `json:"seed"`
}

type snapshotRound struct {
	Round    int                   `json:"round"`
	Pairings []Pairing             `json:"pairings"`
	Results  []TableResult         `json:"results"`
	Floats   []snapshotFloatRecord `json:"floats"`
}

type snapshotByeRecord struct {
	Round    int    `json:"round"`
	PlayerID string `json:"player_id"`
}

type snapshotFloatRecord struct {
	Round     int    `json:"round"`
	PlayerID  string `json:"player_id"`
	Direction string `json:"direction"`
}

// Snapshot serialises the roster, the pairing book and the results.
func (d *SwissPairingDirector) Snapshot() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	state := snapshotState{
		Config:       d.cfg,
		Locked:       d.locked,
		NextSequence: len(d.roster),
		Roster:       make([]snapshotPlayer, 0, len(d.roster)),
		Rounds:       make([]snapshotRound, 0, len(d.rounds)),
		ByeLedger:    make([]snapshotByeRecord, 0),
		FloatLedger:  make([]snapshotFloatRecord, 0),
	}
	for seed, player := range d.roster {
		state.Roster = append(state.Roster, snapshotPlayer{
			ID:        player.ID,
			Rating:    player.Rating,
			Withdrawn: player.Withdrawn,
			Seed:      seed,
		})
	}
	for _, round := range sortedRoundNumbers(d.rounds) {
		record := d.rounds[round]
		sr := snapshotRound{
			Round:    round,
			Pairings: clonePairings(record.pairings),
			Results:  make([]TableResult, 0, len(record.results)),
			Floats:   make([]snapshotFloatRecord, 0, len(record.floatMarks)),
		}
		for _, pairing := range record.pairings {
			if pairing.Table == 0 && pairing.ByeID != "" {
				state.ByeLedger = append(state.ByeLedger, snapshotByeRecord{
					Round: round, PlayerID: pairing.ByeID,
				})
			}
		}
		for playerID, direction := range record.floatMarks {
			if direction == FloatUp || direction == FloatDown {
				record := snapshotFloatRecord{Round: round, PlayerID: playerID, Direction: direction}
				sr.Floats = append(sr.Floats, record)
				state.FloatLedger = append(state.FloatLedger, record)
			}
		}
		sort.Slice(sr.Floats, func(i, j int) bool {
			if sr.Floats[i].PlayerID != sr.Floats[j].PlayerID {
				return sr.Floats[i].PlayerID < sr.Floats[j].PlayerID
			}
			return sr.Floats[i].Direction < sr.Floats[j].Direction
		})
		for _, result := range record.results {
			sr.Results = append(sr.Results, result)
		}
		sort.Slice(sr.Results, func(i, j int) bool { return sr.Results[i].Table < sr.Results[j].Table })
		state.Rounds = append(state.Rounds, sr)
	}
	sort.Slice(state.ByeLedger, func(i, j int) bool {
		if state.ByeLedger[i].Round != state.ByeLedger[j].Round {
			return state.ByeLedger[i].Round < state.ByeLedger[j].Round
		}
		return state.ByeLedger[i].PlayerID < state.ByeLedger[j].PlayerID
	})
	sort.Slice(state.FloatLedger, func(i, j int) bool {
		if state.FloatLedger[i].Round != state.FloatLedger[j].Round {
			return state.FloatLedger[i].Round < state.FloatLedger[j].Round
		}
		if state.FloatLedger[i].PlayerID != state.FloatLedger[j].PlayerID {
			return state.FloatLedger[i].PlayerID < state.FloatLedger[j].PlayerID
		}
		return state.FloatLedger[i].Direction < state.FloatLedger[j].Direction
	})

	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(stateBytes)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(sum[:]),
		State:    stateBytes,
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *SwissPairingDirector) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}
	var envelope snapshotEnvelope
	if err := decodeJSON(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.State)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return ErrBadSnapshot
	}
	var state snapshotState
	if err := decodeJSON(envelope.State, &state); err != nil {
		return ErrBadSnapshot
	}

	cfg, roster, rounds, err := validateAndBuildSnapshot(state)
	if err != nil {
		return ErrBadSnapshot
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
	d.roster = roster
	d.locked = state.Locked
	d.rounds = rounds
	return nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing json value")
		}
		return err
	}
	return nil
}

func validateAndBuildSnapshot(state snapshotState) (Config, []Player, map[int]*roundRecord, error) {
	if state.Config.MaxRounds < 0 || state.Config.Boards < 0 || state.Config.FloatWindow < 0 {
		return Config{}, nil, nil, ErrBadSnapshot
	}
	if state.NextSequence != len(state.Roster) {
		return Config{}, nil, nil, ErrBadSnapshot
	}
	if len(state.Rounds) > state.Config.MaxRounds {
		return Config{}, nil, nil, ErrBadSnapshot
	}
	if len(state.Rounds) > 0 && !state.Locked {
		return Config{}, nil, nil, ErrBadSnapshot
	}

	rosterBySeed := make([]Player, len(state.Roster))
	registered := make(map[string]struct{}, len(state.Roster))
	for _, player := range state.Roster {
		if player.Seed < 0 || player.Seed >= len(state.Roster) {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		if rosterBySeed[player.Seed].ID != "" || strings.TrimSpace(player.ID) == "" {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		if _, duplicate := registered[player.ID]; duplicate {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		registered[player.ID] = struct{}{}
		rosterBySeed[player.Seed] = Player{
			ID: player.ID, Rating: player.Rating, Withdrawn: player.Withdrawn,
		}
	}

	rounds := make(map[int]*roundRecord, len(state.Rounds))
	seenRounds := make(map[int]struct{}, len(state.Rounds))
	for _, round := range state.Rounds {
		if round.Round < 1 || round.Round > state.Config.MaxRounds {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		if _, duplicate := seenRounds[round.Round]; duplicate {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		seenRounds[round.Round] = struct{}{}
		record, err := validateSnapshotRound(round, registered)
		if err != nil {
			return Config{}, nil, nil, err
		}
		rounds[round.Round] = record
	}
	for round := 1; round <= len(state.Rounds); round++ {
		if _, ok := rounds[round]; !ok {
			return Config{}, nil, nil, ErrBadSnapshot
		}
	}

	derivedByes := make(map[string]struct{})
	for _, round := range state.Rounds {
		for _, pairing := range round.Pairings {
			if pairing.Table == 0 && pairing.ByeID != "" {
				derivedByes[byeKey(round.Round, pairing.ByeID)] = struct{}{}
			}
		}
	}
	if len(derivedByes) != len(state.ByeLedger) {
		return Config{}, nil, nil, ErrBadSnapshot
	}
	seenByes := make(map[string]struct{}, len(state.ByeLedger))
	for _, bye := range state.ByeLedger {
		key := byeKey(bye.Round, bye.PlayerID)
		if _, ok := derivedByes[key]; !ok {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		if _, duplicate := seenByes[key]; duplicate {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		seenByes[key] = struct{}{}
	}

	derivedFloats := make(map[string]string)
	for _, round := range state.Rounds {
		for _, float := range round.Floats {
			key := floatKey(float.Round, float.PlayerID)
			if _, duplicate := derivedFloats[key]; duplicate {
				return Config{}, nil, nil, ErrBadSnapshot
			}
			derivedFloats[key] = float.Direction
		}
	}
	if len(derivedFloats) != len(state.FloatLedger) {
		return Config{}, nil, nil, ErrBadSnapshot
	}
	for _, float := range state.FloatLedger {
		key := floatKey(float.Round, float.PlayerID)
		direction, ok := derivedFloats[key]
		if !ok || direction != float.Direction || !validFloat(direction) {
			return Config{}, nil, nil, ErrBadSnapshot
		}
		record := rounds[float.Round]
		if record == nil || !playerAppearsInRound(record.pairings, float.PlayerID) {
			return Config{}, nil, nil, ErrBadSnapshot
		}
	}

	return state.Config, rosterBySeed, rounds, nil
}

func validateSnapshotRound(round snapshotRound, registered map[string]struct{}) (*roundRecord, error) {
	record := &roundRecord{
		pairings:   clonePairings(round.Pairings),
		results:    make(map[int]TableResult),
		floatMarks: make(map[string]string),
	}
	usedPlayers := make(map[string]struct{}, len(round.Pairings))
	tableNumbers := make(map[int]struct{}, len(round.Pairings))
	expectedTable := 1
	byeSeen := false

	for i, pairing := range record.pairings {
		if pairing.Round != round.Round {
			return nil, ErrBadSnapshot
		}
		if !validFloat(pairing.Float) {
			return nil, ErrBadSnapshot
		}
		if pairing.Table == 0 {
			if byeSeen || i != len(record.pairings)-1 {
				return nil, ErrBadSnapshot
			}
			byeSeen = true
			if pairing.ByeID == "" || pairing.WhiteID != "" || pairing.BlackID != "" {
				return nil, ErrBadSnapshot
			}
			if _, ok := registered[pairing.ByeID]; !ok {
				return nil, ErrBadSnapshot
			}
			if _, duplicate := usedPlayers[pairing.ByeID]; duplicate {
				return nil, ErrBadSnapshot
			}
			usedPlayers[pairing.ByeID] = struct{}{}
			continue
		}
		if pairing.Table != expectedTable || pairing.ByeID != "" {
			return nil, ErrBadSnapshot
		}
		expectedTable++
		if pairing.WhiteID == "" || pairing.BlackID == "" || pairing.WhiteID == pairing.BlackID {
			return nil, ErrBadSnapshot
		}
		for _, playerID := range []string{pairing.WhiteID, pairing.BlackID} {
			if _, ok := registered[playerID]; !ok {
				return nil, ErrBadSnapshot
			}
			if _, duplicate := usedPlayers[playerID]; duplicate {
				return nil, ErrBadSnapshot
			}
			usedPlayers[playerID] = struct{}{}
		}
		if _, duplicate := tableNumbers[pairing.Table]; duplicate {
			return nil, ErrBadSnapshot
		}
		tableNumbers[pairing.Table] = struct{}{}
	}

	seenResults := make(map[int]struct{}, len(round.Results))
	for _, result := range round.Results {
		if result.Round != round.Round || result.Table < 1 || !result.Reported {
			return nil, ErrBadSnapshot
		}
		if _, ok := tableNumbers[result.Table]; !ok {
			return nil, ErrBadSnapshot
		}
		if _, duplicate := seenResults[result.Table]; duplicate {
			return nil, ErrBadSnapshot
		}
		seenResults[result.Table] = struct{}{}
		var pairing *Pairing
		for i := range record.pairings {
			if record.pairings[i].Table == result.Table {
				pairing = &record.pairings[i]
				break
			}
		}
		if pairing == nil {
			return nil, ErrBadSnapshot
		}
		if result.Draw {
			if result.WinnerID != "" {
				return nil, ErrBadSnapshot
			}
		} else if result.WinnerID != pairing.WhiteID && result.WinnerID != pairing.BlackID {
			return nil, ErrBadSnapshot
		}
		record.results[result.Table] = result
	}

	seenFloatPlayers := make(map[string]struct{}, len(round.Floats))
	for _, float := range round.Floats {
		if float.Round != round.Round || !validFloat(float.Direction) {
			return nil, ErrBadSnapshot
		}
		if _, duplicate := seenFloatPlayers[float.PlayerID]; duplicate {
			return nil, ErrBadSnapshot
		}
		seenFloatPlayers[float.PlayerID] = struct{}{}
		if !playerAppearsInRound(record.pairings, float.PlayerID) {
			return nil, ErrBadSnapshot
		}
		record.floatMarks[float.PlayerID] = float.Direction
	}
	for _, pairing := range record.pairings {
		if pairing.Float == FloatNone {
			continue
		}
		found := record.floatMarks[pairing.WhiteID] == pairing.Float ||
			record.floatMarks[pairing.BlackID] == pairing.Float
		if !found {
			return nil, ErrBadSnapshot
		}
	}
	return record, nil
}

func validFloat(direction string) bool {
	return direction == FloatNone || direction == FloatUp || direction == FloatDown
}

func playerAppearsInRound(pairings []Pairing, playerID string) bool {
	for _, pairing := range pairings {
		if pairing.WhiteID == playerID || pairing.BlackID == playerID || pairing.ByeID == playerID {
			return true
		}
	}
	return false
}

func byeKey(round int, playerID string) string {
	return strconv.Itoa(round) + "|" + playerID
}

func floatKey(round int, playerID string) string {
	return strconv.Itoa(round) + "|" + playerID
}

func tableResultsPending(record *roundRecord) bool {
	for _, pairing := range record.pairings {
		if pairing.Table == 0 {
			continue
		}
		if _, reported := record.results[pairing.Table]; !reported {
			return true
		}
	}
	return false
}

func sortedRoundNumbers(rounds map[int]*roundRecord) []int {
	numbers := make([]int, 0, len(rounds))
	for round := range rounds {
		numbers = append(numbers, round)
	}
	sort.Ints(numbers)
	return numbers
}

func clonePairings(pairings []Pairing) []Pairing {
	return append([]Pairing(nil), pairings...)
}

type playerView struct {
	id     string
	rating int
	seed   int
	score  float64
}

type matchedPair struct {
	white      playerView
	black      playerView
	whiteFloat string
	blackFloat string
}

func (d *SwissPairingDirector) buildRound(round int) ([]Pairing, map[string]string) {
	history := d.playedHistory()
	previousByes := d.byePlayers()

	scores := d.scores()
	active := make([]playerView, 0, len(d.roster))
	for seed, player := range d.roster {
		if player.Withdrawn {
			continue
		}
		active = append(active, playerView{
			id: player.ID, rating: player.Rating, seed: seed, score: scores[player.ID],
		})
	}
	firstRound := len(d.rounds) == 0
	if firstRound {
		sort.SliceStable(active, func(i, j int) bool {
			if active[i].rating != active[j].rating {
				return active[i].rating > active[j].rating
			}
			return active[i].seed < active[j].seed
		})
	} else {
		sort.SliceStable(active, func(i, j int) bool {
			if active[i].score != active[j].score {
				return active[i].score > active[j].score
			}
			return active[i].seed < active[j].seed
		})
	}

	var byeID string
	if len(active)%2 == 1 {
		byeIndex := chooseBye(active, previousByes)
		byeID = active[byeIndex].id
		active = append(active[:byeIndex], active[byeIndex+1:]...)
	}

	marks := make(map[string]string)
	var matched []matchedPair
	if firstRound {
		matched = append(matched, matchGroup(active, history)...)
	} else {
		groups := splitScoreGroups(active)
		groupPairs := make(map[int][]matchedPair, len(groups))
		usedGroups := make([]bool, len(groups))
		for i := 0; i < len(groups); i++ {
			if usedGroups[i] {
				continue
			}
			if len(groups[i])%2 == 0 {
				groupPairs[i] = matchGroup(groups[i], history)
				usedGroups[i] = true
				continue
			}
			j := nextOddGroup(groups, i+1)
			usedGroups[i] = true
			if j < 0 {
				continue
			}
			usedGroups[j] = true
			cross := chooseCrossGroupPair(groups[i], groups[j], history)
			groupPairs[i] = append([]matchedPair{cross.pair}, matchGroup(cross.highRemainder, history)...)
			groupPairs[j] = matchGroup(cross.lowRemainder, history)
		}
		for i := range groups {
			matched = append(matched, groupPairs[i]...)
		}
	}
	matched = improveMatching(active, history, matched)

	pairings := make([]Pairing, 0, len(matched)+1)
	for i, pair := range matched {
		if pair.whiteFloat == FloatUp || pair.whiteFloat == FloatDown {
			marks[pair.white.id] = pair.whiteFloat
		}
		if pair.blackFloat == FloatUp || pair.blackFloat == FloatDown {
			marks[pair.black.id] = pair.blackFloat
		}
		float := FloatNone
		if pair.whiteFloat == FloatUp || pair.whiteFloat == FloatDown {
			float = pair.whiteFloat
		} else if pair.blackFloat == FloatUp || pair.blackFloat == FloatDown {
			float = pair.blackFloat
		}
		pairings = append(pairings, Pairing{
			Round:   round,
			Table:   i + 1,
			WhiteID: pair.white.id,
			BlackID: pair.black.id,
			Float:   float,
		})
	}
	if byeID != "" {
		pairings = append(pairings, Pairing{
			Round: round,
			Table: 0,
			ByeID: byeID,
			Float: FloatNone,
		})
	}
	return pairings, marks
}

func chooseBye(players []playerView, previousByes map[string]bool) int {
	for i := len(players) - 1; i >= 0; i-- {
		if !previousByes[players[i].id] {
			return i
		}
	}
	return len(players) - 1
}

func splitScoreGroups(players []playerView) [][]playerView {
	if len(players) == 0 {
		return nil
	}
	groups := make([][]playerView, 0)
	start := 0
	for i := 1; i <= len(players); i++ {
		if i == len(players) || players[i].score != players[start].score {
			groups = append(groups, players[start:i])
			start = i
		}
	}
	return groups
}

func nextOddGroup(groups [][]playerView, start int) int {
	for i := start; i < len(groups); i++ {
		if len(groups[i])%2 == 1 {
			return i
		}
	}
	return -1
}

type crossGroupChoice struct {
	pair          matchedPair
	highRemainder []playerView
	lowRemainder  []playerView
}

func chooseCrossGroupPair(high, low []playerView, history map[string]bool) crossGroupChoice {
	type candidate struct {
		highIndex int
		lowIndex  int
		cost      int
		distance  int
	}
	best := candidate{cost: 1 << 30}
	for highIndex := len(high) - 1; highIndex >= 0; highIndex-- {
		for lowIndex := 0; lowIndex < len(low); lowIndex++ {
			highRemainder := removePlayer(high, highIndex)
			lowRemainder := removePlayer(low, lowIndex)
			highRematches := countRematches(matchGroup(highRemainder, history), history)
			lowRematches := countRematches(matchGroup(lowRemainder, history), history)
			cost := highRematches + lowRematches
			if played(history, high[highIndex].id, low[lowIndex].id) {
				cost++
			}
			distance := (len(high) - 1 - highIndex) + lowIndex
			if cost < best.cost || (cost == best.cost && distance < best.distance) {
				best = candidate{
					highIndex: highIndex,
					lowIndex:  lowIndex,
					cost:      cost,
					distance:  distance,
				}
			}
		}
	}
	highPlayer := high[best.highIndex]
	lowPlayer := low[best.lowIndex]
	return crossGroupChoice{
		pair: matchedPair{
			white:      highPlayer,
			black:      lowPlayer,
			whiteFloat: FloatDown,
			blackFloat: FloatUp,
		},
		highRemainder: removePlayer(high, best.highIndex),
		lowRemainder:  removePlayer(low, best.lowIndex),
	}
}

func removePlayer(players []playerView, index int) []playerView {
	result := make([]playerView, 0, len(players)-1)
	result = append(result, players[:index]...)
	result = append(result, players[index+1:]...)
	return result
}

func played(history map[string]bool, first, second string) bool {
	if first > second {
		first, second = second, first
	}
	return history[first+"\x00"+second]
}

func countRematches(pairs []matchedPair, history map[string]bool) int {
	count := 0
	for _, pair := range pairs {
		if played(history, pair.white.id, pair.black.id) {
			count++
		}
	}
	return count
}

type matchTally struct {
	rematches int
	scoreDiff int
	offFold   int
	seedDist  int
}

func (t matchTally) add(other matchTally) matchTally {
	return matchTally{
		rematches: t.rematches + other.rematches,
		scoreDiff: t.scoreDiff + other.scoreDiff,
		offFold:   t.offFold + other.offFold,
		seedDist:  t.seedDist + other.seedDist,
	}
}

func (t matchTally) less(other matchTally) bool {
	if t.rematches != other.rematches {
		return t.rematches < other.rematches
	}
	if t.scoreDiff != other.scoreDiff {
		return t.scoreDiff < other.scoreDiff
	}
	if t.offFold != other.offFold {
		return t.offFold < other.offFold
	}
	return t.seedDist < other.seedDist
}

func improveMatching(players []playerView, history map[string]bool, current []matchedPair) []matchedPair {
	currentRematches := countRematches(current, history)
	if currentRematches == 0 || len(players) > 20 || len(players)%2 != 0 {
		return current
	}
	improved, tally := globalMatching(players, history)
	if improved == nil || tally.rematches >= currentRematches {
		return current
	}
	return improved
}

func globalMatching(players []playerView, history map[string]bool) ([]matchedPair, matchTally) {
	n := len(players)
	if n == 0 {
		return nil, matchTally{}
	}
	groupIndex := make([]int, n)
	groupRank := make([]int, n)
	groupSize := make([]int, n)
	groupStart := 0
	for i := 1; i <= n; i++ {
		if i != n && players[i].score == players[groupStart].score {
			continue
		}
		size := i - groupStart
		for j := groupStart; j < i; j++ {
			groupIndex[j] = groupStart
			groupRank[j] = j - groupStart
			groupSize[j] = size
		}
		groupStart = i
	}

	scoreUnits := make([]int, n)
	for i, player := range players {
		scoreUnits[i] = int(player.score*2 + 0.5)
	}

	edgeTally := func(i, j int) matchTally {
		tally := matchTally{seedDist: j - i}
		if played(history, players[i].id, players[j].id) {
			tally.rematches = 1
		}
		diff := scoreUnits[i] - scoreUnits[j]
		if diff < 0 {
			diff = -diff
		}
		tally.scoreDiff = diff
		if groupIndex[i] == groupIndex[j] {
			half := groupSize[i] / 2
			want := groupRank[i] + half
			if groupRank[i] >= half {
				want = groupRank[i] - half
			}
			if groupRank[j] != want {
				tally.offFold = 1
			}
		}
		return tally
	}

	const infinity = 1 << 30
	states := 1 << n
	memo := make([]matchTally, states)
	seen := make([]bool, states)
	choice := make([]int, states)
	var solve func(mask int) matchTally
	solve = func(mask int) matchTally {
		if mask == (1<<n)-1 {
			return matchTally{}
		}
		if seen[mask] {
			return memo[mask]
		}
		first := -1
		for i := 0; i < n; i++ {
			if mask&(1<<i) == 0 {
				first = i
				break
			}
		}
		best := matchTally{rematches: infinity}
		bestPartner := -1
		for j := first + 1; j < n; j++ {
			if mask&(1<<j) != 0 {
				continue
			}
			total := edgeTally(first, j).add(solve(mask | (1 << first) | (1 << j)))
			if total.less(best) {
				best = total
				bestPartner = j
			}
		}
		memo[mask] = best
		seen[mask] = true
		choice[mask] = bestPartner
		return best
	}
	tally := solve(0)
	if n%2 != 0 {
		return nil, tally
	}
	pairs := make([]matchedPair, 0, n/2)
	mask := 0
	for mask != (1<<n)-1 {
		first := -1
		for i := 0; i < n; i++ {
			if mask&(1<<i) == 0 {
				first = i
				break
			}
		}
		partner := choice[mask]
		if partner < 0 {
			return nil, tally
		}
		pair := matchedPair{white: players[first], black: players[partner]}
		if players[first].score != players[partner].score {
			pair.whiteFloat = FloatDown
			pair.blackFloat = FloatUp
		}
		pairs = append(pairs, pair)
		mask |= (1 << first) | (1 << partner)
	}
	return pairs, tally
}

func matchGroup(players []playerView, history map[string]bool) []matchedPair {
	if len(players) < 2 {
		return nil
	}
	half := len(players) / 2
	upper := players[:half]
	lower := players[half:]

	pairs, removals := matchHalves(upper, lower, history)
	result := make([]matchedPair, 0, half)
	for i := range pairs {
		result = append(result, matchedPair{
			white: upper[i],
			black: lower[removals[i]],
		})
	}
	return result
}

func matchHalves(upper, lower []playerView, history map[string]bool) ([][2]int, []int) {
	n := len(upper)
	if n == 0 {
		return nil, nil
	}
	if n > 16 {
		return greedyHalves(upper, lower, history)
	}

	const infinity = 1 << 20
	memo := make(map[int]int, 1<<n)
	choice := make(map[int]int, 1<<n)
	var solve func(mask int) int
	solve = func(mask int) int {
		if mask == (1<<n)-1 {
			return 0
		}
		if value, ok := memo[mask]; ok {
			return value
		}
		row := bits.OnesCount(uint(mask))
		best := infinity
		bestColumn := -1
		for _, column := range columnOrder(row, n) {
			bit := 1 << column
			if mask&bit != 0 {
				continue
			}
			cost := 0
			if played(history, upper[row].id, lower[column].id) {
				cost = 1
			}
			total := cost + solve(mask|bit)
			if total < best {
				best = total
				bestColumn = column
			}
		}
		memo[mask] = best
		choice[mask] = bestColumn
		return best
	}
	solve(0)
	pairs := make([][2]int, n)
	columns := make([]int, n)
	mask := 0
	for row := 0; row < n; row++ {
		column := choice[mask]
		pairs[row] = [2]int{row, column}
		columns[row] = column
		mask |= 1 << column
	}
	return pairs, columns
}

func greedyHalves(upper, lower []playerView, history map[string]bool) ([][2]int, []int) {
	n := len(upper)
	columns := make([]int, n)
	for i := range columns {
		columns[i] = -1
	}
	used := make([]bool, n)
	for row := 0; row < n; row++ {
		for _, column := range columnOrder(row, n) {
			if used[column] || played(history, upper[row].id, lower[column].id) {
				continue
			}
			columns[row] = column
			used[column] = true
			break
		}
		if columns[row] < 0 {
			for column := 0; column < n; column++ {
				if !used[column] {
					columns[row] = column
					used[column] = true
					break
				}
			}
		}
	}
	pairs := make([][2]int, n)
	for i, column := range columns {
		pairs[i] = [2]int{i, column}
	}
	return pairs, columns
}

func columnOrder(row, n int) []int {
	order := make([]int, 0, n)
	order = append(order, row)
	for i := row + 1; i < n; i++ {
		order = append(order, i)
	}
	for i := 0; i < row; i++ {
		order = append(order, i)
	}
	return order
}

func (d *SwissPairingDirector) playedHistory() map[string]bool {
	history := make(map[string]bool)
	for _, round := range d.rounds {
		for _, pairing := range round.pairings {
			if pairing.Table == 0 {
				continue
			}
			first, second := pairing.WhiteID, pairing.BlackID
			if first > second {
				first, second = second, first
			}
			history[first+"\x00"+second] = true
		}
	}
	return history
}

func (d *SwissPairingDirector) byePlayers() map[string]bool {
	byes := make(map[string]bool)
	for _, round := range d.rounds {
		for _, pairing := range round.pairings {
			if pairing.Table == 0 && pairing.ByeID != "" {
				byes[pairing.ByeID] = true
			}
		}
	}
	return byes
}

func (d *SwissPairingDirector) scores() map[string]float64 {
	scores := make(map[string]float64, len(d.roster))
	for _, round := range d.rounds {
		for _, pairing := range round.pairings {
			if pairing.Table == 0 {
				scores[pairing.ByeID] += PointBye
				continue
			}
			result, ok := round.results[pairing.Table]
			if !ok {
				continue
			}
			if result.Draw {
				scores[pairing.WhiteID] += PointDraw
				scores[pairing.BlackID] += PointDraw
			} else {
				scores[result.WinnerID] += PointWin
			}
		}
	}
	return scores
}
