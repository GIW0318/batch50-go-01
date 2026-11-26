// Package task links a guardian account to a ward account, records the
// consent scope and drives the review, suspension and unbinding states.
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

// Link states.
const (
	LinkInvited   = "invited"
	LinkActive    = "active"
	LinkReadonly  = "readonly"
	LinkSuspended = "suspended"
	LinkRevoked   = "revoked"
	LinkInvalid   = "invalid"
	LinkExpired   = "expired"
)

// Actors accepted by Revoke.
const (
	ActorGuardian = "guardian"
	ActorWard     = "ward"
	ActorSystem   = "system"
)

// Errors returned by the link service.
var (
	ErrEmptyIdentity  = errors.New("account id is required")
	ErrAccountExists  = errors.New("account already enrolled")
	ErrBadBirthDate   = errors.New("birth date must be in the past")
	ErrAccountUnknown = errors.New("account is not enrolled")
	ErrSelfLink       = errors.New("guardian and ward must be different accounts")
	ErrWardAdult      = errors.New("ward is not a minor")
	ErrLinkLimit      = errors.New("guardian or ward link limit reached")
	ErrBadScope       = errors.New("consent scope is empty, too wide or already expired")
	ErrLinkNotFound   = errors.New("guardian link not found")
	ErrLinkClosed     = errors.New("guardian link is closed")
	ErrLinkInvalid    = errors.New("guardian link is invalid")
	ErrLinkExpired    = errors.New("guardian link expired")
	ErrProofRejected  = errors.New("guardian proof rejected")
	ErrBadReason      = errors.New("reason is required")
	ErrBadActor       = errors.New("actor must be the guardian, the ward or the system")
	ErrScopeViolation = errors.New("operation exceeds the consent scope")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Config drives the age threshold, limits and the review cadence.
type Config struct {
	AdultAge            int
	MaxWardsPerGuardian int
	MaxLinksPerWard     int
	MaxProofAttempts    int
	ReviewInterval      time.Duration
	ReviewGrace         time.Duration
	MaxScopeActions     int
	Now                 func() time.Time
}

// Scope is the set of actions a guardian may perform and its deadline.
type Scope struct {
	Actions   []string
	ExpiresAt time.Time
}

// Proof is the one-time acceptance code delivered to the ward.
type Proof struct {
	Code string
}

// Decision is the outcome of one periodic review.
type Decision struct {
	Approve bool
	Note    string
}

// Link is one guardian and ward relationship.
type Link struct {
	ID         string
	GuardianID string
	WardID     string
	State      string
	Actions    []string
	ScopeUntil time.Time
	CreatedAt  time.Time
	AcceptedAt time.Time
	ReviewAt   time.Time
	Seq        int64
	Attempts   int
	AcceptCode string
}

type linkRecord struct {
	Link
	ResumeState    string
	RequiresReview bool
}

// GuardianConsentLink owns accounts, links and the review clock.
type GuardianConsentLink struct {
	mu               sync.RWMutex
	cfg              Config
	accounts         map[string]time.Time
	links            map[string]*linkRecord
	guardianDisabled map[string]bool
	nextSeq          int64
}

// NewGuardianConsentLink builds a link service with the given config.
func NewGuardianConsentLink(cfg Config) *GuardianConsentLink {
	if cfg.AdultAge <= 0 {
		cfg.AdultAge = 18
	}
	if cfg.MaxWardsPerGuardian <= 0 {
		cfg.MaxWardsPerGuardian = 2
	}
	if cfg.MaxLinksPerWard <= 0 {
		cfg.MaxLinksPerWard = 2
	}
	if cfg.MaxProofAttempts <= 0 {
		cfg.MaxProofAttempts = 3
	}
	if cfg.ReviewInterval <= 0 {
		cfg.ReviewInterval = 30 * 24 * time.Hour
	}
	if cfg.ReviewGrace < 0 {
		cfg.ReviewGrace = 0
	}
	if cfg.MaxScopeActions <= 0 {
		cfg.MaxScopeActions = 3
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &GuardianConsentLink{
		cfg:              cfg,
		accounts:         make(map[string]time.Time),
		links:            make(map[string]*linkRecord),
		guardianDisabled: make(map[string]bool),
	}
}

// Enroll registers one account with its birth date.
func (g *GuardianConsentLink) Enroll(accountID string, born time.Time) error {
	if strings.TrimSpace(accountID) == "" {
		return ErrEmptyIdentity
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.accounts[accountID]; exists {
		return ErrAccountExists
	}
	if !born.Before(g.now()) {
		return ErrBadBirthDate
	}
	g.accounts[accountID] = born
	return nil
}

// Invite creates a pending guardian link with a consent scope.
func (g *GuardianConsentLink) Invite(guardianID string, wardID string, scope Scope) (Link, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	_, guardianKnown := g.accounts[guardianID]
	wardBorn, wardKnown := g.accounts[wardID]
	if !guardianKnown || !wardKnown {
		return Link{}, ErrAccountUnknown
	}
	if guardianID == wardID {
		return Link{}, ErrSelfLink
	}
	if g.guardianDisabled[guardianID] {
		return Link{}, ErrLinkClosed
	}
	if g.isAdult(wardBorn, now) {
		return Link{}, ErrWardAdult
	}

	actions, expiresAt, err := g.normalizeScope(scope, now)
	if err != nil {
		return Link{}, err
	}
	if g.openCountForGuardian(guardianID) >= g.cfg.MaxWardsPerGuardian ||
		g.openCountForWard(wardID) >= g.cfg.MaxLinksPerWard {
		return Link{}, ErrLinkLimit
	}

	g.nextSeq++
	id := fmt.Sprintf("link-%d", g.nextSeq)
	record := &linkRecord{Link: Link{
		ID:         id,
		GuardianID: guardianID,
		WardID:     wardID,
		State:      LinkInvited,
		Actions:    actions,
		ScopeUntil: expiresAt,
		CreatedAt:  now,
		Seq:        g.nextSeq,
		AcceptCode: "code-" + id,
	}}
	g.links[id] = record
	return cloneLink(record), nil
}

// Accept activates a pending link with the one-time acceptance code.
func (g *GuardianConsentLink) Accept(linkID string, proof Proof) (Link, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.links[linkID]
	if !ok {
		return Link{}, ErrLinkNotFound
	}
	switch record.State {
	case LinkInvalid:
		return Link{}, ErrLinkInvalid
	case LinkExpired:
		return Link{}, ErrLinkExpired
	case LinkInvited:
		// Continue below.
	default:
		return Link{}, ErrLinkClosed
	}

	now := g.now()
	if g.guardianDisabled[record.GuardianID] {
		record.State = LinkSuspended
		return Link{}, ErrLinkClosed
	}
	if born, enrolled := g.accounts[record.WardID]; enrolled && g.isAdult(born, now) {
		g.closeLink(record, LinkRevoked)
		return Link{}, ErrWardAdult
	}
	if !now.Before(record.ScopeUntil) {
		g.closeLink(record, LinkExpired)
		return Link{}, ErrLinkExpired
	}

	if proof.Code != record.AcceptCode {
		record.Attempts++
		if record.Attempts >= g.cfg.MaxProofAttempts {
			g.closeLink(record, LinkInvalid)
		}
		return Link{}, ErrProofRejected
	}

	if g.hasActiveLinkForPair(record.GuardianID, record.WardID, record.ID) {
		return Link{}, ErrLinkClosed
	}

	record.State = LinkActive
	record.AcceptedAt = now
	record.ReviewAt = now.Add(g.cfg.ReviewInterval)
	record.RequiresReview = false
	record.ResumeState = ""
	return cloneLink(record), nil
}

// Revoke ends a link from one of its actors.
func (g *GuardianConsentLink) Revoke(linkID string, by string, reason string) error {
	switch by {
	case ActorGuardian, ActorWard, ActorSystem:
	default:
		return ErrBadActor
	}
	if strings.TrimSpace(reason) == "" {
		return ErrBadReason
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.links[linkID]
	if !ok {
		return ErrLinkNotFound
	}
	if isClosedState(record.State) {
		return ErrLinkClosed
	}
	g.closeLink(record, LinkRevoked)
	return nil
}

// Review records the periodic review of one link.
func (g *GuardianConsentLink) Review(linkID string, decision Decision) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.links[linkID]
	if !ok {
		return ErrLinkNotFound
	}
	switch record.State {
	case LinkActive, LinkReadonly:
		// Continue below.
	case LinkExpired:
		return ErrLinkExpired
	default:
		return ErrLinkClosed
	}

	now := g.now()
	if born, enrolled := g.accounts[record.WardID]; enrolled && g.isAdult(born, now) {
		g.closeLink(record, LinkRevoked)
		return ErrWardAdult
	}
	if !now.Before(record.ScopeUntil) {
		g.closeLink(record, LinkExpired)
		return ErrLinkExpired
	}

	if decision.Approve {
		if g.hasActiveLinkForPair(record.GuardianID, record.WardID, record.ID) {
			return ErrLinkLimit
		}
		record.State = LinkActive
		record.ReviewAt = now.Add(g.cfg.ReviewInterval)
		record.RequiresReview = false
		record.ResumeState = ""
		return nil
	}

	record.State = LinkReadonly
	record.RequiresReview = false
	record.ResumeState = ""
	return nil
}

// Due lists the active links whose review deadline passed, oldest first.
func (g *GuardianConsentLink) Due(now time.Time) []Link {
	g.mu.RLock()
	defer g.mu.RUnlock()

	due := make([]*linkRecord, 0)
	for _, record := range g.links {
		if record.State != LinkActive || record.ReviewAt.IsZero() || now.Before(record.ReviewAt) {
			continue
		}
		due = append(due, record)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ReviewAt.Equal(due[j].ReviewAt) {
			return due[i].Seq < due[j].Seq
		}
		return due[i].ReviewAt.Before(due[j].ReviewAt)
	})

	result := make([]Link, 0, len(due))
	for _, record := range due {
		result = append(result, cloneLink(record))
	}
	return result
}

// Allowed reports whether an action stays inside the consent scope.
func (g *GuardianConsentLink) Allowed(linkID string, action string) (bool, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	record, ok := g.links[linkID]
	if !ok {
		return false, ErrLinkNotFound
	}
	if record.State != LinkActive || record.RequiresReview ||
		g.guardianDisabled[record.GuardianID] {
		return false, nil
	}

	now := g.now()
	if born, enrolled := g.accounts[record.WardID]; !enrolled || g.isAdult(born, now) {
		return false, nil
	}
	if !now.Before(record.ScopeUntil) {
		return false, nil
	}
	for _, permitted := range record.Actions {
		if permitted == action {
			return true, nil
		}
	}
	return false, nil
}

// Grant narrows the consent scope of an active link.
func (g *GuardianConsentLink) Grant(linkID string, scope Scope) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.links[linkID]
	if !ok {
		return ErrLinkNotFound
	}
	switch record.State {
	case LinkActive:
		// Continue below.
	case LinkExpired:
		return ErrLinkExpired
	default:
		return ErrLinkClosed
	}

	now := g.now()
	if born, enrolled := g.accounts[record.WardID]; enrolled && g.isAdult(born, now) {
		g.closeLink(record, LinkRevoked)
		return ErrWardAdult
	}
	if !now.Before(record.ScopeUntil) {
		g.closeLink(record, LinkExpired)
		return ErrLinkExpired
	}

	actions, expiresAt, err := g.normalizeScope(scope, now)
	if err != nil {
		return err
	}
	if expiresAt.After(record.ScopeUntil) {
		return ErrScopeViolation
	}
	for _, action := range actions {
		if !containsAction(record.Actions, action) {
			return ErrScopeViolation
		}
	}

	record.Actions = actions
	record.ScopeUntil = expiresAt
	return nil
}

// DisableGuardian suspends every open link of one guardian.
func (g *GuardianConsentLink) DisableGuardian(guardianID string, reason string) (int, error) {
	if strings.TrimSpace(reason) == "" {
		return 0, ErrBadReason
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.guardianDisabled[guardianID] = true
	count := 0
	for _, record := range g.links {
		if record.GuardianID != guardianID || !isOpenState(record.State) || record.State == LinkSuspended {
			continue
		}
		record.ResumeState = record.State
		record.State = LinkSuspended
		record.RequiresReview = true
		count++
	}
	return count, nil
}

// EnableGuardian moves suspended links back to review.
func (g *GuardianConsentLink) EnableGuardian(guardianID string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.guardianDisabled, guardianID)
	now := g.now()
	count := 0
	for _, record := range g.links {
		if record.GuardianID != guardianID || record.State != LinkSuspended {
			continue
		}

		if born, enrolled := g.accounts[record.WardID]; enrolled && g.isAdult(born, now) {
			g.closeLink(record, LinkRevoked)
			continue
		}
		if !now.Before(record.ScopeUntil) {
			g.closeLink(record, LinkExpired)
			continue
		}

		resume := record.ResumeState
		if resume == "" {
			resume = LinkActive
		}
		record.ResumeState = ""
		count++
		if resume == LinkInvited {
			record.State = LinkInvited
			record.RequiresReview = false
			continue
		}
		record.State = LinkActive
		record.ReviewAt = now
		record.RequiresReview = true
	}
	return count, nil
}

// Sweep downgrades links whose review is overdue beyond the grace window.
func (g *GuardianConsentLink) Sweep(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	changed := 0
	for _, record := range g.links {
		if isClosedState(record.State) {
			continue
		}

		if born, enrolled := g.accounts[record.WardID]; enrolled && g.isAdult(born, now) {
			g.closeLink(record, LinkRevoked)
			changed++
			continue
		}
		if !now.Before(record.ScopeUntil) {
			g.closeLink(record, LinkExpired)
			changed++
			continue
		}
		if record.State == LinkActive && !record.ReviewAt.IsZero() &&
			now.After(record.ReviewAt.Add(g.cfg.ReviewGrace)) {
			record.State = LinkReadonly
			record.RequiresReview = false
			changed++
		}
	}
	return changed
}

// Attempts counts the rejected acceptance codes of one link.
func (g *GuardianConsentLink) Attempts(linkID string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	record, ok := g.links[linkID]
	if !ok {
		return 0
	}
	return record.Attempts
}

// State returns the state of one link, or an empty string.
func (g *GuardianConsentLink) State(linkID string) string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	record, ok := g.links[linkID]
	if !ok {
		return ""
	}
	return record.State
}

const snapshotVersion = 1

type snapshotAccount struct {
	ID   string    `json:"id"`
	Born time.Time `json:"born"`
}

type snapshotLink struct {
	ID             string    `json:"id"`
	GuardianID     string    `json:"guardian_id"`
	WardID         string    `json:"ward_id"`
	State          string    `json:"state"`
	Actions        []string  `json:"actions"`
	ScopeUntil     time.Time `json:"scope_until"`
	CreatedAt      time.Time `json:"created_at"`
	AcceptedAt     time.Time `json:"accepted_at"`
	ReviewAt       time.Time `json:"review_at"`
	Seq            int64     `json:"seq"`
	Attempts       int       `json:"attempts"`
	AcceptCode     string    `json:"accept_code"`
	ResumeState    string    `json:"resume_state,omitempty"`
	RequiresReview bool      `json:"requires_review,omitempty"`
}

type snapshotData struct {
	Version           int               `json:"version"`
	Accounts          []snapshotAccount `json:"accounts"`
	Links             []snapshotLink    `json:"links"`
	DisabledGuardians []string          `json:"disabled_guardians,omitempty"`
	NextSeq           int64             `json:"next_seq"`
}

// Snapshot serialises accounts, links, counters and the review deadlines.
func (g *GuardianConsentLink) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	data := snapshotData{
		Version: snapshotVersion,
		NextSeq: g.nextSeq,
	}
	accountIDs := make([]string, 0, len(g.accounts))
	for accountID := range g.accounts {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	for _, accountID := range accountIDs {
		data.Accounts = append(data.Accounts, snapshotAccount{ID: accountID, Born: g.accounts[accountID]})
	}

	linkIDs := make([]string, 0, len(g.links))
	for linkID := range g.links {
		linkIDs = append(linkIDs, linkID)
	}
	sort.Slice(linkIDs, func(i, j int) bool {
		return g.links[linkIDs[i]].Seq < g.links[linkIDs[j]].Seq
	})
	for _, linkID := range linkIDs {
		record := g.links[linkID]
		data.Links = append(data.Links, snapshotLink{
			ID:             record.ID,
			GuardianID:     record.GuardianID,
			WardID:         record.WardID,
			State:          record.State,
			Actions:        append([]string(nil), record.Actions...),
			ScopeUntil:     record.ScopeUntil,
			CreatedAt:      record.CreatedAt,
			AcceptedAt:     record.AcceptedAt,
			ReviewAt:       record.ReviewAt,
			Seq:            record.Seq,
			Attempts:       record.Attempts,
			AcceptCode:     record.AcceptCode,
			ResumeState:    record.ResumeState,
			RequiresReview: record.RequiresReview,
		})
	}

	for guardianID := range g.guardianDisabled {
		data.DisabledGuardians = append(data.DisabledGuardians, guardianID)
	}
	sort.Strings(data.DisabledGuardians)

	return json.Marshal(data)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *GuardianConsentLink) Restore(data []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil || snapshot.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if snapshot.NextSeq < 0 {
		return ErrBadSnapshot
	}

	accounts := make(map[string]time.Time, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if strings.TrimSpace(account.ID) == "" {
			return ErrBadSnapshot
		}
		if _, exists := accounts[account.ID]; exists {
			return ErrBadSnapshot
		}
		accounts[account.ID] = account.Born
	}

	links := make(map[string]*linkRecord, len(snapshot.Links))
	seqs := make(map[int64]struct{}, len(snapshot.Links))
	activePairs := make(map[string]struct{})
	var maxSeq int64
	for _, link := range snapshot.Links {
		if !validState(link.State) || link.Seq <= 0 || link.Attempts < 0 || link.ID == "" {
			return ErrBadSnapshot
		}
		if link.ID != fmt.Sprintf("link-%d", link.Seq) || link.AcceptCode != "code-"+link.ID {
			return ErrBadSnapshot
		}
		if _, exists := links[link.ID]; exists {
			return ErrBadSnapshot
		}
		if _, exists := seqs[link.Seq]; exists {
			return ErrBadSnapshot
		}
		if _, exists := accounts[link.GuardianID]; !exists {
			return ErrBadSnapshot
		}
		if _, exists := accounts[link.WardID]; !exists {
			return ErrBadSnapshot
		}
		if link.GuardianID == link.WardID || len(link.Actions) == 0 ||
			hasDuplicateActions(link.Actions) || !sort.StringsAreSorted(link.Actions) {
			return ErrBadSnapshot
		}
		if link.State == LinkSuspended && link.ResumeState != "" &&
			link.ResumeState != LinkInvited && link.ResumeState != LinkActive && link.ResumeState != LinkReadonly {
			return ErrBadSnapshot
		}
		if link.State == LinkActive {
			pair := pairKey(link.GuardianID, link.WardID)
			if _, exists := activePairs[pair]; exists {
				return ErrBadSnapshot
			}
			activePairs[pair] = struct{}{}
		}
		if link.Seq > maxSeq {
			maxSeq = link.Seq
		}

		actions := append([]string(nil), link.Actions...)
		links[link.ID] = &linkRecord{
			Link: Link{
				ID:         link.ID,
				GuardianID: link.GuardianID,
				WardID:     link.WardID,
				State:      link.State,
				Actions:    actions,
				ScopeUntil: link.ScopeUntil,
				CreatedAt:  link.CreatedAt,
				AcceptedAt: link.AcceptedAt,
				ReviewAt:   link.ReviewAt,
				Seq:        link.Seq,
				Attempts:   link.Attempts,
				AcceptCode: link.AcceptCode,
			},
			ResumeState:    link.ResumeState,
			RequiresReview: link.RequiresReview,
		}
		seqs[link.Seq] = struct{}{}
	}

	if snapshot.NextSeq < maxSeq {
		return ErrBadSnapshot
	}
	disabled := make(map[string]bool, len(snapshot.DisabledGuardians))
	for _, guardianID := range snapshot.DisabledGuardians {
		if strings.TrimSpace(guardianID) == "" {
			return ErrBadSnapshot
		}
		disabled[guardianID] = true
	}

	g.mu.Lock()
	g.accounts = accounts
	g.links = links
	g.guardianDisabled = disabled
	g.nextSeq = snapshot.NextSeq
	g.mu.Unlock()
	return nil
}

func (g *GuardianConsentLink) now() time.Time {
	return g.cfg.Now()
}

func (g *GuardianConsentLink) isAdult(born time.Time, now time.Time) bool {
	return !now.Before(born.AddDate(g.cfg.AdultAge, 0, 0))
}

func (g *GuardianConsentLink) normalizeScope(scope Scope, now time.Time) ([]string, time.Time, error) {
	if len(scope.Actions) == 0 || !scope.ExpiresAt.After(now) {
		return nil, time.Time{}, ErrBadScope
	}

	unique := make(map[string]struct{}, len(scope.Actions))
	for _, action := range scope.Actions {
		if strings.TrimSpace(action) == "" {
			return nil, time.Time{}, ErrBadScope
		}
		unique[action] = struct{}{}
	}
	if len(unique) == 0 || len(unique) > g.cfg.MaxScopeActions {
		return nil, time.Time{}, ErrBadScope
	}

	actions := make([]string, 0, len(unique))
	for action := range unique {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions, scope.ExpiresAt, nil
}

func (g *GuardianConsentLink) openCountForGuardian(guardianID string) int {
	count := 0
	for _, record := range g.links {
		if record.GuardianID == guardianID && isOpenState(record.State) {
			count++
		}
	}
	return count
}

func (g *GuardianConsentLink) openCountForWard(wardID string) int {
	count := 0
	for _, record := range g.links {
		if record.WardID == wardID && isOpenState(record.State) {
			count++
		}
	}
	return count
}

func (g *GuardianConsentLink) hasActiveLinkForPair(guardianID string, wardID string, exceptID string) bool {
	for _, record := range g.links {
		if record.ID != exceptID && record.State == LinkActive &&
			record.GuardianID == guardianID && record.WardID == wardID {
			return true
		}
	}
	return false
}

func (g *GuardianConsentLink) closeLink(record *linkRecord, state string) {
	record.State = state
	record.RequiresReview = false
	record.ResumeState = ""
}

func cloneLink(record *linkRecord) Link {
	link := record.Link
	link.Actions = append([]string(nil), record.Actions...)
	return link
}

func containsAction(actions []string, action string) bool {
	for _, candidate := range actions {
		if candidate == action {
			return true
		}
	}
	return false
}

func hasDuplicateActions(actions []string) bool {
	seen := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if strings.TrimSpace(action) == "" {
			return true
		}
		if _, exists := seen[action]; exists {
			return true
		}
		seen[action] = struct{}{}
	}
	return false
}

func isClosedState(state string) bool {
	return state == LinkRevoked || state == LinkInvalid || state == LinkExpired
}

func isOpenState(state string) bool {
	return state == LinkInvited || state == LinkActive || state == LinkReadonly || state == LinkSuspended
}

func validState(state string) bool {
	switch state {
	case LinkInvited, LinkActive, LinkReadonly, LinkSuspended, LinkRevoked, LinkInvalid, LinkExpired:
		return true
	default:
		return false
	}
}

func pairKey(guardianID string, wardID string) string {
	return guardianID + "\x00" + wardID
}
