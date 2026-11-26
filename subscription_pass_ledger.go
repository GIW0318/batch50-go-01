// Package task keeps the monthly subscription pass ledger: stacked expiry
// dates, a UTC anchored daily reward and refunds that reclaim whole days.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Pass SKUs sold by the shop, with the number of days they carry.
const (
	SkuPass30 = "pass-30"
	SkuPass90 = "pass-90"
)

// Ledger entry kinds.
const (
	LedgerPurchase = "purchase"
	LedgerClaim    = "claim"
	LedgerRefund   = "refund"
)

// Errors returned by the ledger.
var (
	ErrBlankAccount    = errors.New("account id is required")
	ErrUnknownSKU      = errors.New("unknown pass sku")
	ErrUnknownOrder    = errors.New("unknown purchase order")
	ErrOrderLimit      = errors.New("order limit reached for this account")
	ErrDayLimit        = errors.New("purchased day limit reached for this account")
	ErrAlreadyRefunded = errors.New("order was already refunded")
	ErrExpired         = errors.New("no active subscription pass")
	ErrAlreadyClaimed  = errors.New("daily reward already claimed for this UTC day")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Entitlement is one purchase. StartAt is where the pass takes over and EndAt
// is the moment it stops being active.
type Entitlement struct {
	OrderID     string    `json:"order_id"`
	AccountID   string    `json:"account_id"`
	SKU         string    `json:"sku"`
	Days        int       `json:"days"`
	PurchasedAt time.Time `json:"purchased_at"`
	StartAt     time.Time `json:"start_at"`
	EndAt       time.Time `json:"end_at"`
	Refunded    bool      `json:"refunded"`
}

// EntitlementState is the read only projection at one instant.
type EntitlementState struct {
	AccountID    string    `json:"account_id"`
	Active       bool      `json:"active"`
	SKU          string    `json:"sku"`
	StartAt      time.Time `json:"start_at"`
	EndAt        time.Time `json:"end_at"`
	Days         int       `json:"days"`
	Debt         int       `json:"debt"`
	ClaimedToday bool      `json:"claimed_today"`
	Today        string    `json:"today"`
}

// Grant is one daily reward mailed to the player.
type Grant struct {
	AccountID string    `json:"account_id"`
	Day       string    `json:"day"`
	MailID    string    `json:"mail_id"`
	Amount    int       `json:"amount"`
	At        time.Time `json:"at"`
}

// RefundResult reports what a refund took back.
type RefundResult struct {
	OrderID        string    `json:"order_id"`
	AccountID      string    `json:"account_id"`
	ReclaimedDays  int       `json:"reclaimed_days"`
	ReclaimedValue int       `json:"reclaimed_value"`
	Debt           int       `json:"debt"`
	EndAt          time.Time `json:"end_at"`
}

// LedgerEntry is one booked line: a purchase, a daily claim or a refund.
type LedgerEntry struct {
	AccountID string    `json:"account_id"`
	Kind      string    `json:"kind"`
	Ref       string    `json:"ref"`
	Delta     int       `json:"delta"`
	At        time.Time `json:"at"`
	Note      string    `json:"note"`
}

// Limits bounds the orders, the stacked days and the book.
type Limits struct {
	MaxOrdersPerAccount int
	MaxDays             int
	DailyReward         int
	MaxLedgerEntries    int
}

// SubscriptionPassLedger owns the orders, the book and the daily claim marks.
type SubscriptionPassLedger struct {
	limits Limits
}

// NewSubscriptionPassLedger builds a ledger with the given caps.
func NewSubscriptionPassLedger(limits Limits) *SubscriptionPassLedger {
	return &SubscriptionPassLedger{limits: limits}
}

// PassDays maps a SKU to the days it carries.
func (l *SubscriptionPassLedger) PassDays(sku string) (int, bool) {
	return 0, false
}

// Purchase adds a pass for the account, stacking onto a running one.
func (l *SubscriptionPassLedger) Purchase(accountID, sku string, at time.Time) (Entitlement, error) {
	return Entitlement{}, errNotImplemented
}

// ClaimDaily mails the reward for the current UTC day, once per day.
func (l *SubscriptionPassLedger) ClaimDaily(accountID string, at time.Time) (Grant, error) {
	return Grant{}, errNotImplemented
}

// EntitlementOf reports the pass state of an account at one instant.
func (l *SubscriptionPassLedger) EntitlementOf(accountID string, at time.Time) (EntitlementState, error) {
	return EntitlementState{}, errNotImplemented
}

// Refund takes back the remaining days of one order.
func (l *SubscriptionPassLedger) Refund(accountID, orderID string, at time.Time) (RefundResult, error) {
	return RefundResult{}, errNotImplemented
}

// Sweep folds the passes that ran out and reports how many accounts changed.
func (l *SubscriptionPassLedger) Sweep(now time.Time) (int, error) {
	return 0, errNotImplemented
}

// Ledger lists the booked lines of one account, oldest first.
func (l *SubscriptionPassLedger) Ledger(accountID string) []LedgerEntry {
	return nil
}

// Orders lists the purchases of one account, oldest first.
func (l *SubscriptionPassLedger) Orders(accountID string) []Entitlement {
	return nil
}

// Debt reports the days an account still owes after a part day refund.
func (l *SubscriptionPassLedger) Debt(accountID string) int {
	return 0
}

// Snapshot serialises orders, the book and the claim marks.
func (l *SubscriptionPassLedger) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *SubscriptionPassLedger) Restore(data []byte) error {
	return errNotImplemented
}
