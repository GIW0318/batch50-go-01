// Package task owns the gem sockets of player equipment: colour matching
// against the socket, extraction per tier policy, set counting recomputed from
// the sockets and a stats summary committed in one step.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Socket colours understood by the guard.
const (
	ColorRed    = "red"
	ColorBlue   = "blue"
	ColorGreen  = "green"
	ColorYellow = "yellow"
)

// Extraction policies of one equipment tier.
const (
	ExtractReturn  = "return"
	ExtractDestroy = "destroy"
)

// Actions recorded in the per item history.
const (
	ActionInlay   = "inlay"
	ActionExtract = "extract"
)

// Bonus granted per active gem set.
const (
	SetBonusAttack  = 10
	SetBonusDefense = 5
	SetBonusCrit    = 5
)

// Errors returned by the guard.
var (
	ErrBlankPlayer      = errors.New("player id is required")
	ErrBadItem          = errors.New("item spec is invalid")
	ErrItemExists       = errors.New("item is already registered")
	ErrItemLimit        = errors.New("item registry limit reached")
	ErrUnknownItem      = errors.New("unknown item")
	ErrNotOwner         = errors.New("item belongs to another player")
	ErrSocketLimit      = errors.New("socket count exceeds the tier limit")
	ErrBadSocketRule    = errors.New("socket rule is invalid")
	ErrSocketIndex      = errors.New("socket index is out of range")
	ErrSocketLocked     = errors.New("socket is not open for this tier")
	ErrSocketOccupied   = errors.New("socket already holds a gem")
	ErrSocketEmpty      = errors.New("socket holds no gem")
	ErrBadGem           = errors.New("gem spec is invalid")
	ErrGemExists        = errors.New("gem is already registered")
	ErrUnknownGem       = errors.New("unknown gem")
	ErrGemNotFound      = errors.New("player has no such gem")
	ErrColorMismatch    = errors.New("gem colour does not match the socket")
	ErrBadAmount        = errors.New("gem amount must be positive")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// GemSpec is the static description of one gem.
type GemSpec struct {
	ID           string `json:"id"`
	Color        string `json:"color"`
	SetID        string `json:"set_id"`
	Attack       int    `json:"attack"`
	Defense      int    `json:"defense"`
	Crit         int    `json:"crit"`
	SetThreshold int    `json:"set_threshold"`
}

// ItemSpec is the static description of one piece of equipment.
type ItemSpec struct {
	ID           string   `json:"id"`
	Owner        string   `json:"owner"`
	Tier         string   `json:"tier"`
	SocketColors []string `json:"socket_colors"`
}

// SocketRule is the per tier opening and extraction policy.
type SocketRule struct {
	Tier          string `json:"tier"`
	MaxSockets    int    `json:"max_sockets"`
	ExtractPolicy string `json:"extract_policy"`
}

// Socket is the projection of one socket.
type Socket struct {
	Index int    `json:"index"`
	Color string `json:"color"`
	Gem   string `json:"gem"`
}

// Stats is the attribute summary of one player.
type Stats struct {
	Attack     int `json:"attack"`
	Defense    int `json:"defense"`
	Crit       int `json:"crit"`
	ActiveSets int `json:"active_sets"`
}

// Result is the outcome of one successful inlay.
type Result struct {
	ItemID    string `json:"item_id"`
	Socket    int    `json:"socket"`
	Gem       string `json:"gem"`
	SetID     string `json:"set_id"`
	SetCount  int    `json:"set_count"`
	SetActive bool   `json:"set_active"`
	Stats     Stats  `json:"stats"`
	Seq       int64  `json:"seq"`
}

// Gem is the outcome of one extraction.
type Gem struct {
	ID        string `json:"id"`
	Color     string `json:"color"`
	SetID     string `json:"set_id"`
	Destroyed bool   `json:"destroyed"`
}

// InlayRecord is one entry of the per item history.
type InlayRecord struct {
	Seq       int64  `json:"seq"`
	Player    string `json:"player"`
	ItemID    string `json:"item_id"`
	Socket    int    `json:"socket"`
	Action    string `json:"action"`
	GemID     string `json:"gem_id"`
	SetID     string `json:"set_id"`
	Destroyed bool   `json:"destroyed"`
}

// Limits bounds the registry sizes and the per item history length.
type Limits struct {
	MaxItems   int `json:"max_items"`
	MaxHistory int `json:"max_history"`
}

// SocketInlayGuard owns the item registry, the gem bag and the socket state.
type SocketInlayGuard struct {
	limits Limits
}

// NewSocketInlayGuard builds a guard with the given caps.
func NewSocketInlayGuard(limits Limits) *SocketInlayGuard {
	return &SocketInlayGuard{limits: limits}
}

// RegisterItem records one piece of equipment and its socket colours.
func (g *SocketInlayGuard) RegisterItem(spec ItemSpec) error {
	return errNotImplemented
}

// RegisterGem records one gem template.
func (g *SocketInlayGuard) RegisterGem(spec GemSpec) error {
	return errNotImplemented
}

// SetSocketRule records the opening and extraction policy of one tier.
func (g *SocketInlayGuard) SetSocketRule(tier string, rule SocketRule) error {
	return errNotImplemented
}

// GrantGem adds gems to the bag of one player.
func (g *SocketInlayGuard) GrantGem(player, gem string, n int) error {
	return errNotImplemented
}

// GemCount reads how many copies of one gem the player still holds.
func (g *SocketInlayGuard) GemCount(player, gem string) int {
	return 0
}

// Inlay puts one gem of the bag into one socket.
func (g *SocketInlayGuard) Inlay(player, itemID string, socket int, gem string) (Result, error) {
	return Result{}, errNotImplemented
}

// Extract takes the gem out of one socket following the tier policy.
func (g *SocketInlayGuard) Extract(player, itemID string, socket int) (Gem, error) {
	return Gem{}, errNotImplemented
}

// Sockets lists the sockets of one item, lowest index first.
func (g *SocketInlayGuard) Sockets(itemID string) ([]Socket, error) {
	return nil, errNotImplemented
}

// Summary recomputes the attribute summary of one player from its sockets.
func (g *SocketInlayGuard) Summary(player string) (Stats, error) {
	return Stats{}, errNotImplemented
}

// History returns a copy of the per item inlay history, oldest first.
func (g *SocketInlayGuard) History(itemID string) []InlayRecord {
	return nil
}

// Snapshot serialises the whole guard state.
func (g *SocketInlayGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SocketInlayGuard) Restore(data []byte) error {
	return errNotImplemented
}
