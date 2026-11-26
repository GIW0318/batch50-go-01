// Package task owns the equipment salvage ledger: tier tables, per item
// yields, batch settlement, bag accounting and restart recovery.
package task

import "errors"

var errNotImplemented = errors.New("not implemented")

// Errors reported by the salvage ledger.
var (
	ErrBadPlayer     = errors.New("player id is required")
	ErrBadItem       = errors.New("item id is required")
	ErrUnknownTier   = errors.New("no yield table covers this item")
	ErrBadTable      = errors.New("yield table definition is invalid")
	ErrBoundItem     = errors.New("bound equipment cannot be salvaged")
	ErrQuestItem     = errors.New("quest item cannot be salvaged")
	ErrEmptyBatch    = errors.New("salvage batch is empty")
	ErrBatchTooLarge = errors.New("salvage batch exceeds the batch limit")
	ErrBadLimit      = errors.New("limit must be positive")
	ErrBagFull       = errors.New("bag cannot hold the whole batch")
	ErrLimit         = errors.New("salvage table limit reached")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// ItemRef identifies one equipment instance handed to the salvager.
type ItemRef struct {
	ID    string
	Tier  string
	Plus  int
	Bound bool
	Quest bool
}

// Yield is one material stack produced by a salvage step.
type Yield struct {
	Material string
	Count    int
}

// Table is the yield definition of one tier at one enhancement level.
type Table struct {
	Plus      int
	Materials []Yield
}

// BatchResult is the outcome of one accepted batch.
type BatchResult struct {
	Player string
	Seq    int64
	Items  int
	Yields []Yield
}

// BatchRecord is the history entry of one accepted batch.
type BatchRecord struct {
	Seq    int64
	Player string
	Items  []string
	Yields []Yield
}

// Limits bounds the ledger tables.
type Limits struct {
	MaxPlayers int
	MaxBatch   int
	MaxHistory int
}

// SalvageYieldLedger owns yield tables, salvage history, bags and the
// already salvaged set.
type SalvageYieldLedger struct {
	limits Limits
}

// NewSalvageYieldLedger builds a ledger with the given caps.
func NewSalvageYieldLedger(limits Limits) *SalvageYieldLedger {
	return &SalvageYieldLedger{limits: limits}
}

// SetTable registers the yield table of one tier at one enhancement level.
func (l *SalvageYieldLedger) SetTable(tier string, table Table) error {
	return errNotImplemented
}

// YieldOf resolves the materials of one equipment instance.
func (l *SalvageYieldLedger) YieldOf(item ItemRef) ([]Yield, error) {
	return nil, errNotImplemented
}

// Preview resolves the summed materials of a batch without recording it.
func (l *SalvageYieldLedger) Preview(items []ItemRef) ([]Yield, error) {
	return nil, errNotImplemented
}

// Salvage consumes a batch of equipment and records the outcome.
func (l *SalvageYieldLedger) Salvage(player string, items []ItemRef) (BatchResult, error) {
	return BatchResult{}, errNotImplemented
}

// History returns the accepted batches of one player, oldest first.
func (l *SalvageYieldLedger) History(player string) []BatchRecord {
	return nil
}

// SetBatchLimit caps how many items one batch may carry.
func (l *SalvageYieldLedger) SetBatchLimit(n int) error {
	return errNotImplemented
}

// SetBagCapacity caps how many material units one bag may hold.
func (l *SalvageYieldLedger) SetBagCapacity(n int) error {
	return errNotImplemented
}

// Bag returns a copy of the material counts of one player.
func (l *SalvageYieldLedger) Bag(player string) map[string]int {
	return nil
}

// AlreadySalvaged tells whether an equipment instance was consumed before.
func (l *SalvageYieldLedger) AlreadySalvaged(id string) bool {
	return false
}

// Snapshot serialises tables, history, bags and the salvaged set.
func (l *SalvageYieldLedger) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *SalvageYieldLedger) Restore(data []byte) error {
	return errNotImplemented
}
