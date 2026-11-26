// Package task handles player requests to recover items that were deleted or
// sold by mistake: disposal receipts with a retention window, re-acquired
// instance detection, support review and a single mail based reissue.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Disposal channels understood by RecordDisposal.
const (
	ChannelDeleted = "delete"
	ChannelSold    = "sell"
)

// Restore case states.
const (
	CasePending  = "pending"
	CaseApproved = "approved"
	CaseRejected = "rejected"
)

// Errors returned by the restore desk.
var (
	ErrBlankCharacter    = errors.New("character id is required")
	ErrNoItems           = errors.New("at least one disposed item is required")
	ErrBadItem           = errors.New("disposed item is incomplete")
	ErrDuplicateReceipt  = errors.New("duplicate receipt id")
	ErrNoReceipts        = errors.New("at least one receipt is required")
	ErrTooManyReceipts   = errors.New("receipt limit reached for one case")
	ErrUnknownReceipt    = errors.New("unknown disposal receipt")
	ErrReceiptExpired    = errors.New("disposal receipt is past its retention window")
	ErrReceiptUsed       = errors.New("disposal receipt was already restored")
	ErrReceiptReacquired = errors.New("item instance was acquired again after disposal")
	ErrCharacterGone     = errors.New("character no longer exists")
	ErrCaseNotFound      = errors.New("restore case not found")
	ErrCaseClosed        = errors.New("restore case is already closed")
	ErrCaseQuota         = errors.New("restore case quota reached for this period")
	ErrValueQuota        = errors.New("restore value quota reached for this period")
	ErrOperatorRequired  = errors.New("operator is required")
	ErrReasonRequired    = errors.New("reject reason is required")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// DisposedItem is one thing the player threw away, with the receipt the desk
// writes at disposal time. ExpiresAt and Used are filled in by the desk.
type DisposedItem struct {
	ReceiptID  string    `json:"receipt_id"`
	InstanceID string    `json:"instance_id"`
	ItemID     string    `json:"item_id"`
	Name       string    `json:"name"`
	Quantity   int       `json:"quantity"`
	Value      int       `json:"value"`
	Channel    string    `json:"channel"`
	DisposedAt time.Time `json:"disposed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Used       bool      `json:"used"`
}

// RestoreCase is one review ticket opened for a set of receipts.
type RestoreCase struct {
	CaseID      string    `json:"case_id"`
	CharacterID string    `json:"character_id"`
	Receipts    []string  `json:"receipts"`
	State       string    `json:"state"`
	OpenedAt    time.Time `json:"opened_at"`
	Operator    string    `json:"operator"`
	Reason      string    `json:"reason"`
}

// CaseState is the read only projection of a case.
type CaseState struct {
	CaseID      string   `json:"case_id"`
	CharacterID string   `json:"character_id"`
	Receipts    []string `json:"receipts"`
	State       string   `json:"state"`
	Operator    string   `json:"operator"`
	Reason      string   `json:"reason"`
}

// Grant is the single reissue produced by an approved case. MailID names the
// in game mail that carries the instances.
type Grant struct {
	GrantID     string    `json:"grant_id"`
	CaseID      string    `json:"case_id"`
	CharacterID string    `json:"character_id"`
	MailID      string    `json:"mail_id"`
	InstanceIDs []string  `json:"instance_ids"`
	Value       int       `json:"value"`
	At          time.Time `json:"at"`
}

// Limits bounds retention and the per period allowances.
type Limits struct {
	RetentionDays      int
	MaxReceiptsPerCase int
	MaxCasesPerPeriod  int
	MaxValuePerPeriod  int
}

// ItemRestoreDesk owns the receipt table, the case table and the grants.
type ItemRestoreDesk struct {
	limits Limits
}

// NewItemRestoreDesk builds a desk with the given caps.
func NewItemRestoreDesk(limits Limits) *ItemRestoreDesk {
	return &ItemRestoreDesk{limits: limits}
}

// RecordDisposal writes one receipt per disposed item and starts the retention
// clock. A blank receipt id is filled in by the desk.
func (d *ItemRestoreDesk) RecordDisposal(characterID string, items []DisposedItem, at time.Time) error {
	return errNotImplemented
}

// Submit opens a review case for the given receipts, all or nothing.
func (d *ItemRestoreDesk) Submit(characterID string, receiptIDs []string, at time.Time) (RestoreCase, error) {
	return RestoreCase{}, errNotImplemented
}

// Approve closes a pending case, marks its receipts used and mails the items.
func (d *ItemRestoreDesk) Approve(caseID, operator string) (Grant, error) {
	return Grant{}, errNotImplemented
}

// Reject closes a pending case without touching the receipts.
func (d *ItemRestoreDesk) Reject(caseID, reason string) error {
	return errNotImplemented
}

// Status reads one case.
func (d *ItemRestoreDesk) Status(caseID string) (CaseState, error) {
	return CaseState{}, errNotImplemented
}

// Receipt reads one disposal receipt.
func (d *ItemRestoreDesk) Receipt(receiptID string) (DisposedItem, bool) {
	return DisposedItem{}, false
}

// Grants lists the reissues of one character, oldest first.
func (d *ItemRestoreDesk) Grants(characterID string) []Grant {
	return nil
}

// OpenCases lists the pending case ids of one character, oldest first.
func (d *ItemRestoreDesk) OpenCases(characterID string) []string {
	return nil
}

// MarkReacquired records that the player got the same instance back through a
// drop, trade or mail, which vetoes a restore of that receipt.
func (d *ItemRestoreDesk) MarkReacquired(characterID, instanceID string) error {
	return errNotImplemented
}

// MarkCharacterGone records that the character was deleted, which vetoes every
// receipt it still holds.
func (d *ItemRestoreDesk) MarkCharacterGone(characterID string) error {
	return errNotImplemented
}

// Snapshot serialises receipts, cases and grants.
func (d *ItemRestoreDesk) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *ItemRestoreDesk) Restore(data []byte) error {
	return errNotImplemented
}
