// Package task handles player requests to recover items that were deleted or
// sold by mistake: disposal receipts with a retention window, re-acquired
// instance detection, support review and a single mail based reissue.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

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

const snapshotVersion = 1

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

type storedReceipt struct {
	Item        DisposedItem `json:"item"`
	CharacterID string       `json:"character_id"`
}

type storedCase struct {
	Case  RestoreCase `json:"case"`
	Value int         `json:"value"`
}

type snapshotData struct {
	Version             int                 `json:"version"`
	ReceiptSeq          int64               `json:"receipt_seq"`
	CaseSeq             int64               `json:"case_seq"`
	GrantSeq            int64               `json:"grant_seq"`
	MailSeq             int64               `json:"mail_seq"`
	Receipts            []storedReceipt     `json:"receipts"`
	Cases               []storedCase        `json:"cases"`
	Grants              []Grant             `json:"grants"`
	GoneCharacters      []string            `json:"gone_characters,omitempty"`
	ReacquiredInstances map[string][]string `json:"reacquired_instances,omitempty"`
}

// ItemRestoreDesk owns the receipt table, the case table and the grants.
type ItemRestoreDesk struct {
	mu     sync.RWMutex
	limits Limits

	receiptSeq int64
	caseSeq    int64
	grantSeq   int64
	mailSeq    int64

	receipts  map[string]storedReceipt
	cases     map[string]storedCase
	caseOrder []string
	grants    []Grant

	goneCharacters map[string]struct{}
	reacquired     map[string]map[string]struct{}
}

// NewItemRestoreDesk builds a desk with the given caps.
func NewItemRestoreDesk(limits Limits) *ItemRestoreDesk {
	return &ItemRestoreDesk{
		limits:         limits,
		receipts:       make(map[string]storedReceipt),
		cases:          make(map[string]storedCase),
		goneCharacters: make(map[string]struct{}),
		reacquired:     make(map[string]map[string]struct{}),
	}
}

// RecordDisposal writes one receipt per disposed item and starts the retention
// clock. A blank receipt id is filled in by the desk.
func (d *ItemRestoreDesk) RecordDisposal(characterID string, items []DisposedItem, at time.Time) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ErrBlankCharacter
	}
	if len(items) == 0 {
		return ErrNoItems
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	planned := make([]storedReceipt, len(items))
	batchReceiptIDs := make(map[string]struct{}, len(items))
	nextReceiptSeq := d.receiptSeq

	for i, item := range items {
		if item.Quantity <= 0 || strings.TrimSpace(item.InstanceID) == "" {
			return ErrBadItem
		}

		receiptID := strings.TrimSpace(item.ReceiptID)
		if receiptID == "" {
			for {
				nextReceiptSeq++
				candidate := fmt.Sprintf("rcpt-%d", nextReceiptSeq)
				_, exists := d.receipts[candidate]
				_, inBatch := batchReceiptIDs[candidate]
				if !exists && !inBatch {
					receiptID = candidate
					break
				}
			}
		} else {
			if _, exists := d.receipts[receiptID]; exists {
				return ErrDuplicateReceipt
			}
			if _, duplicated := batchReceiptIDs[receiptID]; duplicated {
				return ErrDuplicateReceipt
			}
		}

		batchReceiptIDs[receiptID] = struct{}{}

		item.ReceiptID = receiptID
		item.InstanceID = strings.TrimSpace(item.InstanceID)
		item.DisposedAt = at
		item.ExpiresAt = at.AddDate(0, 0, d.limits.RetentionDays)
		item.Used = false

		planned[i] = storedReceipt{Item: item, CharacterID: characterID}
	}

	// Commit only after the complete batch has passed validation. Write the
	// allocated ids back to the caller's slice as part of the same success.
	d.receiptSeq = nextReceiptSeq
	for i, receipt := range planned {
		items[i] = receipt.Item
		d.receipts[receipt.Item.ReceiptID] = receipt
	}
	return nil
}

// Submit opens a review case for the given receipts, all or nothing.
func (d *ItemRestoreDesk) Submit(characterID string, receiptIDs []string, at time.Time) (RestoreCase, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return RestoreCase{}, ErrBlankCharacter
	}
	if len(receiptIDs) == 0 {
		return RestoreCase{}, ErrNoReceipts
	}
	if len(receiptIDs) > d.limits.MaxReceiptsPerCase {
		return RestoreCase{}, ErrTooManyReceipts
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, gone := d.goneCharacters[characterID]; gone {
		return RestoreCase{}, ErrCharacterGone
	}

	uniqueReceiptIDs := make([]string, 0, len(receiptIDs))
	seenRequestIDs := make(map[string]struct{}, len(receiptIDs))
	unknownIDs := make([]string, 0)
	expiredIDs := make([]string, 0)
	caseValue := 0

	for _, receiptID := range receiptIDs {
		if _, duplicated := seenRequestIDs[receiptID]; duplicated {
			continue
		}
		seenRequestIDs[receiptID] = struct{}{}

		receipt, ok := d.receipts[receiptID]
		if !ok || receipt.CharacterID != characterID {
			unknownIDs = append(unknownIDs, receiptID)
			continue
		}

		if !at.Before(receipt.Item.ExpiresAt) {
			expiredIDs = append(expiredIDs, receiptID)
			continue
		}
		if receipt.Item.Used {
			return RestoreCase{}, fmt.Errorf("%w: %s", ErrReceiptUsed, receiptID)
		}
		if d.isReacquiredLocked(characterID, receipt.Item.InstanceID) {
			return RestoreCase{}, fmt.Errorf("%w: %s", ErrReceiptReacquired, receipt.Item.InstanceID)
		}

		uniqueReceiptIDs = append(uniqueReceiptIDs, receiptID)
		caseValue += receipt.Item.Value
	}

	if len(unknownIDs) > 0 {
		return RestoreCase{}, withIDList(ErrUnknownReceipt, unknownIDs)
	}
	if len(expiredIDs) > 0 {
		return RestoreCase{}, withIDList(ErrReceiptExpired, expiredIDs)
	}

	period := utcDay(at)
	periodCases := 0
	periodValue := 0
	for _, existing := range d.cases {
		if utcDay(existing.Case.OpenedAt) == period {
			periodCases++
			periodValue += existing.Value
		}
	}

	if periodCases+1 > d.limits.MaxCasesPerPeriod {
		return RestoreCase{}, ErrCaseQuota
	}
	if periodValue+caseValue > d.limits.MaxValuePerPeriod {
		return RestoreCase{}, ErrValueQuota
	}

	d.caseSeq++
	caseID := fmt.Sprintf("case-%d", d.caseSeq)
	caseRecord := storedCase{
		Case: RestoreCase{
			CaseID:      caseID,
			CharacterID: characterID,
			Receipts:    uniqueReceiptIDs,
			State:       CasePending,
			OpenedAt:    at,
		},
		Value: caseValue,
	}
	d.cases[caseID] = caseRecord
	d.caseOrder = append(d.caseOrder, caseID)

	return cloneRestoreCase(caseRecord.Case), nil
}

// Approve closes a pending case, marks its receipts used and mails the items.
func (d *ItemRestoreDesk) Approve(caseID, operator string) (Grant, error) {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return Grant{}, ErrOperatorRequired
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	caseRecord, ok := d.cases[caseID]
	if !ok {
		return Grant{}, ErrCaseNotFound
	}
	if caseRecord.Case.State != CasePending {
		return Grant{}, ErrCaseClosed
	}

	characterID := caseRecord.Case.CharacterID
	if _, gone := d.goneCharacters[characterID]; gone {
		return Grant{}, ErrCharacterGone
	}

	instanceIDs := make([]string, 0, len(caseRecord.Case.Receipts))
	grantValue := 0
	for _, receiptID := range caseRecord.Case.Receipts {
		receipt, ok := d.receipts[receiptID]
		if !ok || receipt.CharacterID != characterID {
			return Grant{}, fmt.Errorf("%w: %s", ErrUnknownReceipt, receiptID)
		}
		if receipt.Item.Used {
			return Grant{}, fmt.Errorf("%w: %s", ErrReceiptUsed, receiptID)
		}
		if d.isReacquiredLocked(characterID, receipt.Item.InstanceID) {
			return Grant{}, fmt.Errorf("%w: %s", ErrReceiptReacquired, receipt.Item.InstanceID)
		}
		instanceIDs = append(instanceIDs, receipt.Item.InstanceID)
		grantValue += receipt.Item.Value
	}

	approvedAt := time.Now().UTC()
	d.grantSeq++
	grantID := fmt.Sprintf("grant-%d", d.grantSeq)
	d.mailSeq++
	mailID := fmt.Sprintf("mail-%d", d.mailSeq)

	grant := Grant{
		GrantID:     grantID,
		CaseID:      caseID,
		CharacterID: characterID,
		MailID:      mailID,
		InstanceIDs: instanceIDs,
		Value:       grantValue,
		At:          approvedAt,
	}

	// Commit receipt use, case closure and grant/mail creation together.
	for _, receiptID := range caseRecord.Case.Receipts {
		receipt := d.receipts[receiptID]
		receipt.Item.Used = true
		d.receipts[receiptID] = receipt
	}
	caseRecord.Case.State = CaseApproved
	caseRecord.Case.Operator = operator
	d.cases[caseID] = caseRecord
	d.grants = append(d.grants, grant)

	return cloneGrant(grant), nil
}

// Reject closes a pending case without touching the receipts.
func (d *ItemRestoreDesk) Reject(caseID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrReasonRequired
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	caseRecord, ok := d.cases[caseID]
	if !ok {
		return ErrCaseNotFound
	}
	if caseRecord.Case.State != CasePending {
		return ErrCaseClosed
	}

	caseRecord.Case.State = CaseRejected
	caseRecord.Case.Reason = reason
	d.cases[caseID] = caseRecord
	return nil
}

// Status reads one case.
func (d *ItemRestoreDesk) Status(caseID string) (CaseState, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	caseRecord, ok := d.cases[caseID]
	if !ok {
		return CaseState{}, ErrCaseNotFound
	}

	return CaseState{
		CaseID:      caseRecord.Case.CaseID,
		CharacterID: caseRecord.Case.CharacterID,
		Receipts:    cloneStrings(caseRecord.Case.Receipts),
		State:       caseRecord.Case.State,
		Operator:    caseRecord.Case.Operator,
		Reason:      caseRecord.Case.Reason,
	}, nil
}

// Receipt reads one disposal receipt.
func (d *ItemRestoreDesk) Receipt(receiptID string) (DisposedItem, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	receipt, ok := d.receipts[receiptID]
	if !ok {
		return DisposedItem{}, false
	}
	return receipt.Item, true
}

// Grants lists the reissues of one character, oldest first.
func (d *ItemRestoreDesk) Grants(characterID string) []Grant {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var grants []Grant
	for _, grant := range d.grants {
		if grant.CharacterID == characterID {
			grants = append(grants, cloneGrant(grant))
		}
	}
	return grants
}

// OpenCases lists the pending case ids of one character, oldest first.
func (d *ItemRestoreDesk) OpenCases(characterID string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var open []string
	for _, caseID := range d.caseOrder {
		caseRecord := d.cases[caseID]
		if caseRecord.Case.CharacterID == characterID && caseRecord.Case.State == CasePending {
			open = append(open, caseID)
		}
	}
	return open
}

// MarkReacquired records that the player got the same instance back through a
// drop, trade or mail, which vetoes a restore of that receipt.
func (d *ItemRestoreDesk) MarkReacquired(characterID, instanceID string) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ErrBlankCharacter
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return ErrBadItem
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	instances, ok := d.reacquired[characterID]
	if !ok {
		instances = make(map[string]struct{})
		d.reacquired[characterID] = instances
	}
	instances[instanceID] = struct{}{}
	return nil
}

// MarkCharacterGone records that the character was deleted, which vetoes every
// receipt it still holds.
func (d *ItemRestoreDesk) MarkCharacterGone(characterID string) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ErrBlankCharacter
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.goneCharacters[characterID] = struct{}{}
	return nil
}

// Snapshot serialises receipts, cases and grants.
func (d *ItemRestoreDesk) Snapshot() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	data := snapshotData{
		Version:    snapshotVersion,
		ReceiptSeq: d.receiptSeq,
		CaseSeq:    d.caseSeq,
		GrantSeq:   d.grantSeq,
		MailSeq:    d.mailSeq,
		Cases:      make([]storedCase, 0, len(d.caseOrder)),
		Grants:     make([]Grant, 0, len(d.grants)),
	}

	receiptIDs := make([]string, 0, len(d.receipts))
	for receiptID := range d.receipts {
		receiptIDs = append(receiptIDs, receiptID)
	}
	sort.Strings(receiptIDs)
	data.Receipts = make([]storedReceipt, 0, len(receiptIDs))
	for _, receiptID := range receiptIDs {
		data.Receipts = append(data.Receipts, d.receipts[receiptID])
	}

	for _, caseID := range d.caseOrder {
		data.Cases = append(data.Cases, d.cases[caseID])
	}
	for _, grant := range d.grants {
		data.Grants = append(data.Grants, cloneGrant(grant))
	}

	goneIDs := make([]string, 0, len(d.goneCharacters))
	for characterID := range d.goneCharacters {
		goneIDs = append(goneIDs, characterID)
	}
	sort.Strings(goneIDs)
	data.GoneCharacters = goneIDs

	if len(d.reacquired) > 0 {
		data.ReacquiredInstances = make(map[string][]string, len(d.reacquired))
		characterIDs := make([]string, 0, len(d.reacquired))
		for characterID := range d.reacquired {
			characterIDs = append(characterIDs, characterID)
		}
		sort.Strings(characterIDs)
		for _, characterID := range characterIDs {
			instanceIDs := make([]string, 0, len(d.reacquired[characterID]))
			for instanceID := range d.reacquired[characterID] {
				instanceIDs = append(instanceIDs, instanceID)
			}
			sort.Strings(instanceIDs)
			data.ReacquiredInstances[characterID] = instanceIDs
		}
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *ItemRestoreDesk) Restore(payload []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}
	if snapshot.ReceiptSeq < 0 || snapshot.CaseSeq < 0 || snapshot.GrantSeq < 0 || snapshot.MailSeq < 0 {
		return fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}

	next := NewItemRestoreDesk(d.limits)
	next.receiptSeq = snapshot.ReceiptSeq
	next.caseSeq = snapshot.CaseSeq
	next.grantSeq = snapshot.GrantSeq
	next.mailSeq = snapshot.MailSeq

	for _, receipt := range snapshot.Receipts {
		receiptID := strings.TrimSpace(receipt.Item.ReceiptID)
		if receiptID == "" {
			return fmt.Errorf("%w: empty receipt id", ErrBadSnapshot)
		}
		if _, duplicated := next.receipts[receiptID]; duplicated {
			return fmt.Errorf("%w: duplicate receipt %s", ErrBadSnapshot, receiptID)
		}
		receipt.Item.ReceiptID = receiptID
		next.receipts[receiptID] = receipt
	}

	for _, caseRecord := range snapshot.Cases {
		caseID := strings.TrimSpace(caseRecord.Case.CaseID)
		if caseID == "" {
			return fmt.Errorf("%w: empty case id", ErrBadSnapshot)
		}
		if _, duplicated := next.cases[caseID]; duplicated {
			return fmt.Errorf("%w: duplicate case %s", ErrBadSnapshot, caseID)
		}
		switch caseRecord.Case.State {
		case CasePending, CaseApproved, CaseRejected:
		default:
			return fmt.Errorf("%w: case %s has invalid state", ErrBadSnapshot, caseID)
		}
		caseRecord.Case.CaseID = caseID
		next.cases[caseID] = caseRecord
		next.caseOrder = append(next.caseOrder, caseID)
	}

	for _, caseID := range next.caseOrder {
		caseRecord := next.cases[caseID]
		if strings.TrimSpace(caseRecord.Case.CharacterID) == "" {
			return fmt.Errorf("%w: case %s has empty character", ErrBadSnapshot, caseID)
		}
		if len(caseRecord.Case.Receipts) == 0 {
			return fmt.Errorf("%w: case %s has no receipts", ErrBadSnapshot, caseID)
		}
		for _, receiptID := range caseRecord.Case.Receipts {
			receipt, ok := next.receipts[receiptID]
			if !ok {
				return fmt.Errorf("%w: case %s references missing receipt %s", ErrBadSnapshot, caseID, receiptID)
			}
			if receipt.CharacterID != caseRecord.Case.CharacterID {
				return fmt.Errorf("%w: case %s references receipt %s from another character", ErrBadSnapshot, caseID, receiptID)
			}
		}
	}

	for _, grant := range snapshot.Grants {
		if strings.TrimSpace(grant.GrantID) == "" {
			return fmt.Errorf("%w: empty grant id", ErrBadSnapshot)
		}
		caseRecord, ok := next.cases[grant.CaseID]
		if !ok {
			return fmt.Errorf("%w: grant %s references missing case %s", ErrBadSnapshot, grant.GrantID, grant.CaseID)
		}
		if caseRecord.Case.State != CaseApproved || caseRecord.Case.CharacterID != grant.CharacterID {
			return fmt.Errorf("%w: grant %s does not match its approved case", ErrBadSnapshot, grant.GrantID)
		}
		grant.InstanceIDs = cloneStrings(grant.InstanceIDs)
		next.grants = append(next.grants, grant)
	}

	for _, characterID := range snapshot.GoneCharacters {
		if characterID = strings.TrimSpace(characterID); characterID != "" {
			next.goneCharacters[characterID] = struct{}{}
		}
	}
	for characterID, instanceIDs := range snapshot.ReacquiredInstances {
		characterID = strings.TrimSpace(characterID)
		if characterID == "" {
			continue
		}
		instances := make(map[string]struct{}, len(instanceIDs))
		for _, instanceID := range instanceIDs {
			if instanceID = strings.TrimSpace(instanceID); instanceID != "" {
				instances[instanceID] = struct{}{}
			}
		}
		next.reacquired[characterID] = instances
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.receiptSeq = next.receiptSeq
	d.caseSeq = next.caseSeq
	d.grantSeq = next.grantSeq
	d.mailSeq = next.mailSeq
	d.receipts = next.receipts
	d.cases = next.cases
	d.caseOrder = next.caseOrder
	d.grants = next.grants
	d.goneCharacters = next.goneCharacters
	d.reacquired = next.reacquired
	return nil
}

func (d *ItemRestoreDesk) isReacquiredLocked(characterID, instanceID string) bool {
	instances, ok := d.reacquired[characterID]
	if !ok {
		return false
	}
	_, reacquired := instances[instanceID]
	return reacquired
}

func utcDay(at time.Time) string {
	return at.UTC().Format("2006-01-02")
}

func withIDList(base error, ids []string) error {
	return fmt.Errorf("%w: %s", base, strings.Join(ids, ", "))
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneRestoreCase(caseRecord RestoreCase) RestoreCase {
	caseRecord.Receipts = cloneStrings(caseRecord.Receipts)
	return caseRecord
}

func cloneGrant(grant Grant) Grant {
	grant.InstanceIDs = cloneStrings(grant.InstanceIDs)
	return grant
}
