// Package task keeps the monthly subscription pass ledger: stacked expiry
// dates, a UTC anchored daily reward and refunds that reclaim whole days.
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

const snapshotVersion = 1

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

type accountState struct {
	Orders    []Entitlement     `json:"orders"`
	Entries   []LedgerEntry     `json:"entries"`
	Claims    map[string]string `json:"claims"`
	Debt      int               `json:"debt"`
	EndAt     time.Time         `json:"end_at"`
	TotalDays int               `json:"total_days"`
	Swept     bool              `json:"swept"`
}

type snapshotData struct {
	Version  int                      `json:"version"`
	Sequence int64                    `json:"sequence"`
	Accounts map[string]*accountState `json:"accounts"`
}

// SubscriptionPassLedger owns the orders, the book and the daily claim marks.
type SubscriptionPassLedger struct {
	limits   Limits
	mu       sync.RWMutex
	accounts map[string]*accountState
	sequence int64
}

// NewSubscriptionPassLedger builds a ledger with the given caps.
func NewSubscriptionPassLedger(limits Limits) *SubscriptionPassLedger {
	return &SubscriptionPassLedger{
		limits:   limits,
		accounts: make(map[string]*accountState),
	}
}

// PassDays maps a SKU to the days it carries.
func (l *SubscriptionPassLedger) PassDays(sku string) (int, bool) {
	return passDaysForSKU(sku)
}

func passDaysForSKU(sku string) (int, bool) {
	switch sku {
	case SkuPass30:
		return 30, true
	case SkuPass90:
		return 90, true
	default:
		return 0, false
	}
}

// Purchase adds a pass for the account, stacking onto a running one.
func (l *SubscriptionPassLedger) Purchase(accountID, sku string, at time.Time) (Entitlement, error) {
	if strings.TrimSpace(accountID) == "" {
		return Entitlement{}, ErrBlankAccount
	}

	days, ok := l.PassDays(sku)
	if !ok {
		return Entitlement{}, ErrUnknownSKU
	}

	at = at.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()

	acc := l.accounts[accountID]
	orderCount := 0
	totalDays := 0
	var currentEnd time.Time
	if acc != nil {
		orderCount = len(acc.Orders)
		totalDays = acc.TotalDays
		currentEnd = acc.EndAt
	}

	if orderCount >= l.limits.MaxOrdersPerAccount {
		return Entitlement{}, ErrOrderLimit
	}
	if totalDays+days > l.limits.MaxDays {
		return Entitlement{}, ErrDayLimit
	}

	if acc == nil {
		acc = &accountState{Claims: make(map[string]string)}
		if l.accounts == nil {
			l.accounts = make(map[string]*accountState)
		}
		l.accounts[accountID] = acc
	}
	if acc.Claims == nil {
		acc.Claims = make(map[string]string)
	}

	startAt := at
	if currentEnd.After(at) {
		startAt = currentEnd
	}
	endAt := startAt.AddDate(0, 0, days)

	l.sequence++
	orderID := fmt.Sprintf("ord-%d", l.sequence)
	entitlement := Entitlement{
		OrderID:     orderID,
		AccountID:   accountID,
		SKU:         sku,
		Days:        days,
		PurchasedAt: at,
		StartAt:     startAt,
		EndAt:       endAt,
	}

	acc.Orders = append(acc.Orders, entitlement)
	acc.TotalDays += days
	acc.EndAt = endAt
	acc.Swept = false
	acc.Entries = append(acc.Entries, LedgerEntry{
		AccountID: accountID,
		Kind:      LedgerPurchase,
		Ref:       orderID,
		Delta:     days,
		At:        at,
		Note:      sku,
	})

	return entitlement, nil
}

// ClaimDaily mails the reward for the current UTC day, once per day.
func (l *SubscriptionPassLedger) ClaimDaily(accountID string, at time.Time) (Grant, error) {
	at = at.UTC()
	day := utcDayKey(at)

	l.mu.Lock()
	defer l.mu.Unlock()

	acc := l.accounts[accountID]
	if acc != nil {
		if _, claimed := acc.Claims[day]; claimed {
			return Grant{}, ErrAlreadyClaimed
		}
	}
	if acc == nil || !at.Before(acc.EndAt) {
		return Grant{}, ErrExpired
	}
	if acc.Claims == nil {
		acc.Claims = make(map[string]string)
	}

	l.sequence++
	mailID := fmt.Sprintf("mail-%d", l.sequence)
	amount := l.limits.DailyReward
	grant := Grant{
		AccountID: accountID,
		Day:       day,
		MailID:    mailID,
		Amount:    amount,
		At:        at,
	}

	acc.Claims[day] = mailID
	acc.Entries = append(acc.Entries, LedgerEntry{
		AccountID: accountID,
		Kind:      LedgerClaim,
		Ref:       mailID,
		Delta:     amount,
		At:        at,
		Note:      day,
	})

	return grant, nil
}

// EntitlementOf reports the pass state of an account at one instant.
func (l *SubscriptionPassLedger) EntitlementOf(accountID string, at time.Time) (EntitlementState, error) {
	at = at.UTC()
	day := utcDayKey(at)
	state := EntitlementState{
		AccountID: accountID,
		Today:     day,
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	acc := l.accounts[accountID]
	if acc == nil {
		return state, nil
	}

	state.Active = at.Before(acc.EndAt)
	state.EndAt = acc.EndAt
	state.Days = acc.TotalDays
	state.Debt = acc.Debt
	if acc.Claims != nil {
		_, state.ClaimedToday = acc.Claims[day]
	}
	if len(acc.Orders) > 0 {
		latest := acc.Orders[len(acc.Orders)-1]
		state.SKU = latest.SKU
		state.StartAt = latest.StartAt
		for i := range acc.Orders {
			order := &acc.Orders[i]
			if !at.Before(order.StartAt) && at.Before(order.EndAt) {
				state.SKU = order.SKU
				state.StartAt = order.StartAt
				break
			}
		}
	}

	return state, nil
}

// Refund takes back the remaining days of one order.
func (l *SubscriptionPassLedger) Refund(accountID, orderID string, at time.Time) (RefundResult, error) {
	at = at.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()

	acc := l.accounts[accountID]
	if acc == nil {
		return RefundResult{}, ErrUnknownOrder
	}

	orderIndex := -1
	for i := range acc.Orders {
		if acc.Orders[i].OrderID == orderID {
			orderIndex = i
			break
		}
	}
	if orderIndex == -1 {
		return RefundResult{}, ErrUnknownOrder
	}

	order := &acc.Orders[orderIndex]
	if order.Refunded {
		return RefundResult{}, ErrAlreadyRefunded
	}
	if at.Before(order.StartAt) || !at.Before(order.EndAt) {
		return RefundResult{}, ErrExpired
	}

	remaining := order.EndAt.Sub(at)
	reclaimedDays := int(remaining / (24 * time.Hour))
	debtIncurred := 0
	if reclaimedDays == 0 {
		reclaimedDays = 1
		debtIncurred = 1
		acc.Debt += debtIncurred
	}

	order.Refunded = true
	order.EndAt = at
	acc.EndAt = at

	note := ""
	if debtIncurred > 0 {
		note = fmt.Sprintf("debt:%d", acc.Debt)
	}
	acc.Entries = append(acc.Entries, LedgerEntry{
		AccountID: accountID,
		Kind:      LedgerRefund,
		Ref:       orderID,
		Delta:     -reclaimedDays,
		At:        at,
		Note:      note,
	})

	return RefundResult{
		OrderID:        orderID,
		AccountID:      accountID,
		ReclaimedDays:  reclaimedDays,
		ReclaimedValue: reclaimedDays * l.limits.DailyReward,
		Debt:           acc.Debt,
		EndAt:          at,
	}, nil
}

// Sweep folds the passes that ran out and reports how many accounts changed.
func (l *SubscriptionPassLedger) Sweep(now time.Time) (int, error) {
	now = now.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()

	changed := 0
	for _, acc := range l.accounts {
		if acc == nil || acc.EndAt.IsZero() || now.Before(acc.EndAt) {
			continue
		}
		if acc.Swept {
			continue
		}
		acc.Swept = true
		changed++
	}

	return changed, nil
}

// Ledger lists the booked lines of one account, oldest first.
func (l *SubscriptionPassLedger) Ledger(accountID string) []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	acc := l.accounts[accountID]
	if acc == nil || len(acc.Entries) == 0 {
		return nil
	}
	entries := make([]LedgerEntry, len(acc.Entries))
	copy(entries, acc.Entries)
	return entries
}

// Orders lists the purchases of one account, oldest first.
func (l *SubscriptionPassLedger) Orders(accountID string) []Entitlement {
	l.mu.RLock()
	defer l.mu.RUnlock()

	acc := l.accounts[accountID]
	if acc == nil || len(acc.Orders) == 0 {
		return nil
	}
	orders := make([]Entitlement, len(acc.Orders))
	copy(orders, acc.Orders)
	return orders
}

// Debt reports the days an account still owes after a part day refund.
func (l *SubscriptionPassLedger) Debt(accountID string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if acc := l.accounts[accountID]; acc != nil {
		return acc.Debt
	}
	return 0
}

// Snapshot serialises orders, the book and the claim marks.
func (l *SubscriptionPassLedger) Snapshot() ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	snapshot := snapshotData{
		Version:  snapshotVersion,
		Sequence: l.sequence,
		Accounts: l.accounts,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *SubscriptionPassLedger) Restore(data []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.accounts = snapshot.Accounts
	l.sequence = snapshot.Sequence
	if l.accounts == nil {
		l.accounts = make(map[string]*accountState)
	}
	return nil
}

func utcDayKey(at time.Time) string {
	return at.UTC().Format("2006-01-02")
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}
	if snapshot.Sequence < 0 {
		return fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}
	if snapshot.Accounts == nil {
		snapshot.Accounts = make(map[string]*accountState)
	}

	var maxSequence int64
	observeID := func(id string) {
		parts := strings.Split(id, "-")
		if len(parts) == 0 {
			return
		}
		n, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		if err == nil && n > maxSequence {
			maxSequence = n
		}
	}

	for accountID, acc := range snapshot.Accounts {
		if strings.TrimSpace(accountID) == "" || acc == nil {
			return fmt.Errorf("%w: invalid account state", ErrBadSnapshot)
		}
		if acc.Debt < 0 || acc.TotalDays < 0 {
			return fmt.Errorf("%w: negative account counter", ErrBadSnapshot)
		}
		if acc.Claims == nil {
			acc.Claims = make(map[string]string)
		}

		for day, mailID := range acc.Claims {
			if _, err := time.Parse("2006-01-02", day); err != nil || mailID == "" {
				return fmt.Errorf("%w: invalid claim mark", ErrBadSnapshot)
			}
			observeID(mailID)
		}

		orderIDs := make(map[string]struct{}, len(acc.Orders))
		orderDays := 0
		for i := range acc.Orders {
			order := &acc.Orders[i]
			if order.OrderID == "" || order.AccountID != accountID {
				return fmt.Errorf("%w: invalid order identity", ErrBadSnapshot)
			}
			if _, exists := orderIDs[order.OrderID]; exists {
				return fmt.Errorf("%w: duplicate order", ErrBadSnapshot)
			}
			orderIDs[order.OrderID] = struct{}{}
			days, ok := passDaysForSKU(order.SKU)
			if !ok || order.Days != days || order.Days <= 0 {
				return fmt.Errorf("%w: invalid order sku or days", ErrBadSnapshot)
			}
			if order.EndAt.Before(order.StartAt) {
				return fmt.Errorf("%w: inverted order interval", ErrBadSnapshot)
			}
			orderDays += order.Days
			observeID(order.OrderID)
		}
		if acc.TotalDays != orderDays {
			return fmt.Errorf("%w: total days does not match orders", ErrBadSnapshot)
		}

		for i := range acc.Entries {
			entry := &acc.Entries[i]
			if entry.AccountID != accountID || entry.Ref == "" {
				return fmt.Errorf("%w: invalid ledger identity", ErrBadSnapshot)
			}
			observeID(entry.Ref)
			switch entry.Kind {
			case LedgerPurchase, LedgerRefund:
				if _, ok := orderIDs[entry.Ref]; !ok {
					return fmt.Errorf("%w: ledger line without order", ErrBadSnapshot)
				}
			case LedgerClaim:
				if _, err := time.Parse("2006-01-02", entry.Note); err != nil {
					return fmt.Errorf("%w: invalid claim day", ErrBadSnapshot)
				}
			default:
				return fmt.Errorf("%w: unknown ledger kind", ErrBadSnapshot)
			}
		}
	}

	if maxSequence > snapshot.Sequence {
		snapshot.Sequence = maxSequence
	}
	return nil
}
