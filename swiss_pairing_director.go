// Package task keeps the pairing book of one online swiss event: the roster,
// the pairings of every round, the table results and the standings table
// derived from them.
package task

import "errors"

// errNotImplemented marks the behaviours that still have to be written.
var errNotImplemented = errors.New("not implemented")

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

// SwissPairingDirector owns the roster, the pairing book and the results.
type SwissPairingDirector struct {
	cfg Config
}

// NewSwissPairingDirector builds a director for one event.
func NewSwissPairingDirector(cfg Config) *SwissPairingDirector {
	return &SwissPairingDirector{cfg: cfg}
}

// Register loads the roster. It is only accepted before the first pairing.
func (d *SwissPairingDirector) Register(players []Player) error {
	return errNotImplemented
}

// PairRound pairs one round and returns its table list, bye included.
func (d *SwissPairingDirector) PairRound(round int) ([]Pairing, error) {
	return nil, errNotImplemented
}

// ReportResult records the outcome of one table.
func (d *SwissPairingDirector) ReportResult(round, table int, winnerID string, draw bool) error {
	return errNotImplemented
}

// Standings returns the ranked table, best player first.
func (d *SwissPairingDirector) Standings() []Standing {
	return nil
}

// Unpair drops the pairings of one round and reverses its reported results.
func (d *SwissPairingDirector) Unpair(round int) error {
	return errNotImplemented
}

// Round returns a copy of the pairings of one round.
func (d *SwissPairingDirector) Round(round int) ([]Pairing, error) {
	return nil, errNotImplemented
}

// ByeHistory lists the rounds in which one player sat out, oldest first.
func (d *SwissPairingDirector) ByeHistory(playerID string) []int {
	return nil
}

// Floats counts how many rounds one player was moved between score groups.
func (d *SwissPairingDirector) Floats(playerID string) int {
	return 0
}

// Withdraw removes one player from every later pairing.
func (d *SwissPairingDirector) Withdraw(playerID string) error {
	return errNotImplemented
}

// Snapshot serialises the roster, the pairing book and the results.
func (d *SwissPairingDirector) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *SwissPairingDirector) Restore(data []byte) error {
	return errNotImplemented
}
