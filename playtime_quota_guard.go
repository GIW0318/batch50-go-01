// Package task meters the online time of young accounts: it slices sessions
// into local days, gates new matches on the daily quota and on the curfew
// window, and keeps the parent exemptions apart from the daily allowance.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks the behaviours that still have to be written.
var errNotImplemented = errors.New("not implemented")

// Admission reasons.
const (
	ReasonAllowed        = "allowed"
	ReasonQuotaExhausted = "quota_exhausted"
	ReasonCurfew         = "curfew"
)

// Errors returned by the guard.
var (
	ErrBadAccount         = errors.New("account id is required")
	ErrBadPolicy          = errors.New("policy is invalid")
	ErrAlreadyActive      = errors.New("account already has an open session")
	ErrNoSession          = errors.New("account has no open session")
	ErrBadExemption       = errors.New("exemption needs an operator and a deadline")
	ErrNoExemption        = errors.New("account has no exemption")
	ErrHeartbeatGap       = errors.New("heartbeat gap closed the session")
	ErrCurfewGraceExpired = errors.New("curfew grace expired")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// Policy is the per event play time rule set. Day boundaries are computed in
// the fixed offset UTCOffsetMinutes, curfew hours are local hours.
type Policy struct {
	DailyMinutes        int
	CurfewStartHour     int
	CurfewEndHour       int
	GraceMinutes        int
	HeartbeatGapMinutes int
	UTCOffsetMinutes    int
}

// Admission is the answer to a request to open a new match.
type Admission struct {
	AccountID        string    `json:"account_id"`
	Allowed          bool      `json:"allowed"`
	Reason           string    `json:"reason"`
	RemainingMinutes int       `json:"remaining_minutes"`
	CurfewUntil      time.Time `json:"curfew_until"`
	Seq              int64     `json:"seq"`
}

// SessionState is the server view of the open session of one account.
type SessionState struct {
	AccountID      string    `json:"account_id"`
	StartedAt      time.Time `json:"started_at"`
	LastHeartbeat  time.Time `json:"last_heartbeat"`
	UsedMinutes    int       `json:"used_minutes"`
	Active         bool      `json:"active"`
	CurfewDeadline time.Time `json:"curfew_deadline"`
	Seq            int64     `json:"seq"`
}

// UsageState is the metered usage of one account on one local day.
type UsageState struct {
	AccountID        string `json:"account_id"`
	Day              string `json:"day"`
	UsedMinutes      int    `json:"used_minutes"`
	LimitMinutes     int    `json:"limit_minutes"`
	RemainingMinutes int    `json:"remaining_minutes"`
}

// Exemption is a parent approval that opens the curfew gate only.
type Exemption struct {
	AccountID string    `json:"account_id"`
	Until     time.Time `json:"until"`
	By        string    `json:"by"`
	Seq       int64     `json:"seq"`
}

// Segment is one metered interval inside a local day.
type Segment struct {
	AccountID string    `json:"account_id"`
	Day       string    `json:"day"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Minutes   int       `json:"minutes"`
}

// PlaytimeQuotaGuard owns the sessions, the day buckets and the exemptions.
type PlaytimeQuotaGuard struct {
	policy Policy
}

// NewPlaytimeQuotaGuard builds a guard for one policy.
func NewPlaytimeQuotaGuard(policy Policy) *PlaytimeQuotaGuard {
	return &PlaytimeQuotaGuard{policy: policy}
}

// BeginSession decides whether a new match may start at the given instant.
func (g *PlaytimeQuotaGuard) BeginSession(accountID string, at time.Time) (Admission, error) {
	return Admission{}, errNotImplemented
}

// Heartbeat renews the open session and returns the metered usage.
func (g *PlaytimeQuotaGuard) Heartbeat(accountID string, at time.Time) (SessionState, error) {
	return SessionState{}, errNotImplemented
}

// EndSession closes the open session and books the remaining minutes.
func (g *PlaytimeQuotaGuard) EndSession(accountID, reason string) error {
	return errNotImplemented
}

// GrantExemption records a parent approval until the given instant.
func (g *PlaytimeQuotaGuard) GrantExemption(accountID string, until time.Time, by string) error {
	return errNotImplemented
}

// RevokeExemption drops the exemption of one account.
func (g *PlaytimeQuotaGuard) RevokeExemption(accountID string) error {
	return errNotImplemented
}

// ExemptionOf reads the exemption of one account.
func (g *PlaytimeQuotaGuard) ExemptionOf(accountID string) (Exemption, bool) {
	return Exemption{}, false
}

// Usage reports the metered usage of one account on the local day of day.
func (g *PlaytimeQuotaGuard) Usage(accountID string, day time.Time) (UsageState, error) {
	return UsageState{}, errNotImplemented
}

// Segments lists the metered intervals of one account on one local day.
func (g *PlaytimeQuotaGuard) Segments(accountID string, day time.Time) []Segment {
	return nil
}

// Snapshot serialises sessions, day buckets, exemptions and the sequence.
func (g *PlaytimeQuotaGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PlaytimeQuotaGuard) Restore(data []byte) error {
	return errNotImplemented
}
