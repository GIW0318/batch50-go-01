// Package task links a guardian account to a ward account, records the
// consent scope and drives the review, suspension and unbinding states.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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

// GuardianConsentLink owns accounts, links and the review clock.
type GuardianConsentLink struct {
	cfg Config
}

// NewGuardianConsentLink builds a link service with the given config.
func NewGuardianConsentLink(cfg Config) *GuardianConsentLink {
	return &GuardianConsentLink{cfg: cfg}
}

// Enroll registers one account with its birth date.
func (g *GuardianConsentLink) Enroll(accountID string, born time.Time) error {
	return errNotImplemented
}

// Invite creates a pending guardian link with a consent scope.
func (g *GuardianConsentLink) Invite(guardianID string, wardID string, scope Scope) (Link, error) {
	return Link{}, errNotImplemented
}

// Accept activates a pending link with the one-time acceptance code.
func (g *GuardianConsentLink) Accept(linkID string, proof Proof) (Link, error) {
	return Link{}, errNotImplemented
}

// Revoke ends a link from one of its actors.
func (g *GuardianConsentLink) Revoke(linkID string, by string, reason string) error {
	return errNotImplemented
}

// Review records the periodic review of one link.
func (g *GuardianConsentLink) Review(linkID string, decision Decision) error {
	return errNotImplemented
}

// Due lists the active links whose review deadline passed, oldest first.
func (g *GuardianConsentLink) Due(now time.Time) []Link {
	return nil
}

// Allowed reports whether an action stays inside the consent scope.
func (g *GuardianConsentLink) Allowed(linkID string, action string) (bool, error) {
	return false, errNotImplemented
}

// Grant narrows the consent scope of an active link.
func (g *GuardianConsentLink) Grant(linkID string, scope Scope) error {
	return errNotImplemented
}

// DisableGuardian suspends every open link of one guardian.
func (g *GuardianConsentLink) DisableGuardian(guardianID string, reason string) (int, error) {
	return 0, errNotImplemented
}

// EnableGuardian moves suspended links back to review.
func (g *GuardianConsentLink) EnableGuardian(guardianID string) (int, error) {
	return 0, errNotImplemented
}

// Sweep downgrades links whose review is overdue beyond the grace window.
func (g *GuardianConsentLink) Sweep(now time.Time) int {
	return 0
}

// Attempts counts the rejected acceptance codes of one link.
func (g *GuardianConsentLink) Attempts(linkID string) int {
	return 0
}

// State returns the state of one link, or an empty string.
func (g *GuardianConsentLink) State(linkID string) string {
	return ""
}

// Snapshot serialises accounts, links, counters and the review deadlines.
func (g *GuardianConsentLink) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *GuardianConsentLink) Restore(data []byte) error {
	return errNotImplemented
}
