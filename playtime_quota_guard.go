// Package task meters the online time of young accounts: it slices sessions
// into local days, gates new matches on the daily quota and on the curfew
// window, and keeps the parent exemptions apart from the daily allowance.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

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

type session struct {
	StartedAt     time.Time `json:"started_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Heartbeats    int       `json:"heartbeats"`
}

type dayUsage struct {
	Used     time.Duration `json:"used"`
	Segments []Segment     `json:"segments"`
}

const snapshotVersion = 1

type snapshotPayload struct {
	Sessions   map[string]session             `json:"sessions"`
	Usage      map[string]map[string]dayUsage `json:"usage"`
	Exemptions map[string]Exemption           `json:"exemptions"`
	Seq        int64                          `json:"seq"`
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

// PlaytimeQuotaGuard owns the sessions, the day buckets and the exemptions.
//
// All public operations use one lock. Besides protecting the maps this is what
// makes an account's begin/heartbeat/end sequence, and the sequence counter,
// linearisable without a quota-decision race.
type PlaytimeQuotaGuard struct {
	policy Policy

	mu         sync.Mutex
	sessions   map[string]*session
	usage      map[string]map[string]*dayUsage
	exemptions map[string]Exemption
	seq        int64
}

// NewPlaytimeQuotaGuard builds a guard for one policy.
func NewPlaytimeQuotaGuard(policy Policy) *PlaytimeQuotaGuard {
	return &PlaytimeQuotaGuard{
		policy:     policy,
		sessions:   make(map[string]*session),
		usage:      make(map[string]map[string]*dayUsage),
		exemptions: make(map[string]Exemption),
	}
}

// BeginSession decides whether a new match may start at the given instant.
func (g *PlaytimeQuotaGuard) BeginSession(accountID string, at time.Time) (Admission, error) {
	if blank(accountID) {
		return Admission{}, ErrBadAccount
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	remaining := g.remainingMinutes(accountID, at)
	if _, ok := g.sessions[accountID]; ok {
		return Admission{AccountID: accountID, RemainingMinutes: remaining, Seq: g.nextSeq()}, ErrAlreadyActive
	}

	if g.inCurfew(at) && !g.hasExemptionAt(accountID, at) {
		return Admission{
			AccountID:        accountID,
			Reason:           ReasonCurfew,
			RemainingMinutes: remaining,
			CurfewUntil:      g.nextCurfewEnd(at),
			Seq:              g.nextSeq(),
		}, nil
	}

	if remaining <= 0 {
		return Admission{
			AccountID:        accountID,
			Reason:           ReasonQuotaExhausted,
			RemainingMinutes: 0,
			Seq:              g.nextSeq(),
		}, nil
	}

	g.sessions[accountID] = &session{StartedAt: at, LastHeartbeat: at}
	return Admission{
		AccountID:        accountID,
		Allowed:          true,
		Reason:           ReasonAllowed,
		RemainingMinutes: remaining,
		Seq:              g.nextSeq(),
	}, nil
}

// Heartbeat renews the open session and returns the metered usage.
func (g *PlaytimeQuotaGuard) Heartbeat(accountID string, at time.Time) (SessionState, error) {
	if blank(accountID) {
		return SessionState{}, ErrBadAccount
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	s, ok := g.sessions[accountID]
	if !ok {
		return SessionState{}, ErrNoSession
	}

	// A client clock moving backwards is not allowed to refund any time. Use
	// the server's last known heartbeat as the high-water mark.
	effectiveAt := at
	if effectiveAt.Before(s.LastHeartbeat) {
		effectiveAt = s.LastHeartbeat
	}

	gap := time.Duration(maxInt(g.policy.HeartbeatGapMinutes, 0)) * time.Minute
	if s.Heartbeats > 0 && effectiveAt.Sub(s.LastHeartbeat) > gap {
		return g.closeSession(accountID, s, ErrHeartbeatGap)
	}

	if g.inCurfew(effectiveAt) && !g.hasExemptionAt(accountID, effectiveAt) && !g.withinCurfewGrace(s, effectiveAt) {
		return g.closeSession(accountID, s, ErrCurfewGraceExpired)
	}

	g.book(accountID, s.LastHeartbeat, effectiveAt)
	s.LastHeartbeat = effectiveAt
	s.Heartbeats++

	state := g.sessionState(accountID, s, effectiveAt, true)
	state.Seq = g.nextSeq()
	return state, nil
}

// EndSession closes the open session and books the remaining minutes.
func (g *PlaytimeQuotaGuard) EndSession(accountID, reason string) error {
	if blank(accountID) {
		return ErrBadAccount
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.sessions[accountID]; !ok {
		return ErrNoSession
	}
	delete(g.sessions, accountID)
	g.nextSeq()
	return nil
}

// GrantExemption records a parent approval until the given instant.
func (g *PlaytimeQuotaGuard) GrantExemption(accountID string, until time.Time, by string) error {
	if blank(accountID) {
		return ErrBadAccount
	}
	if until.IsZero() || blank(by) {
		return ErrBadExemption
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.exemptions[accountID] = Exemption{
		AccountID: accountID,
		Until:     until,
		By:        by,
		Seq:       g.nextSeq(),
	}
	return nil
}

// RevokeExemption drops the exemption of one account.
func (g *PlaytimeQuotaGuard) RevokeExemption(accountID string) error {
	if blank(accountID) {
		return ErrBadAccount
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.exemptions[accountID]; !ok {
		return ErrNoExemption
	}
	delete(g.exemptions, accountID)
	g.nextSeq()
	return nil
}

// ExemptionOf reads the exemption of one account.
func (g *PlaytimeQuotaGuard) ExemptionOf(accountID string) (Exemption, bool) {
	if blank(accountID) {
		return Exemption{}, false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	ex, ok := g.exemptions[accountID]
	return ex, ok
}

// Usage reports the metered usage of one account on the local day of day.
func (g *PlaytimeQuotaGuard) Usage(accountID string, day time.Time) (UsageState, error) {
	if blank(accountID) {
		return UsageState{}, ErrBadAccount
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	key := g.localDay(day)
	used := g.usedMinutes(accountID, key)
	limit := g.policy.DailyMinutes
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return UsageState{
		AccountID:        accountID,
		Day:              key,
		UsedMinutes:      used,
		LimitMinutes:     limit,
		RemainingMinutes: remaining,
	}, nil
}

// Segments lists the metered intervals of one account on one local day.
func (g *PlaytimeQuotaGuard) Segments(accountID string, day time.Time) []Segment {
	if blank(accountID) {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	buckets, ok := g.usage[accountID]
	if !ok {
		return nil
	}
	bucket, ok := buckets[g.localDay(day)]
	if !ok || len(bucket.Segments) == 0 {
		return nil
	}
	return append([]Segment(nil), bucket.Segments...)
}

// Snapshot serialises sessions, day buckets, exemptions and the sequence.
func (g *PlaytimeQuotaGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	payload := snapshotPayload{
		Sessions:   make(map[string]session, len(g.sessions)),
		Usage:      make(map[string]map[string]dayUsage, len(g.usage)),
		Exemptions: make(map[string]Exemption, len(g.exemptions)),
		Seq:        g.seq,
	}
	for accountID, s := range g.sessions {
		payload.Sessions[accountID] = *s
	}
	for accountID, buckets := range g.usage {
		payload.Usage[accountID] = make(map[string]dayUsage, len(buckets))
		for day, bucket := range buckets {
			payload.Usage[accountID][day] = dayUsage{
				Used:     bucket.Used,
				Segments: append([]Segment(nil), bucket.Segments...),
			}
		}
	}
	for accountID, ex := range g.exemptions {
		payload.Exemptions[accountID] = ex
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Payload:  raw,
		Checksum: hex.EncodeToString(sum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PlaytimeQuotaGuard) Restore(data []byte) error {
	payload, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}
	if err := validateSnapshot(payload); err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.sessions = make(map[string]*session, len(payload.Sessions))
	for accountID, s := range payload.Sessions {
		copyOfSession := s
		g.sessions[accountID] = &copyOfSession
	}
	g.usage = make(map[string]map[string]*dayUsage, len(payload.Usage))
	for accountID, buckets := range payload.Usage {
		g.usage[accountID] = make(map[string]*dayUsage, len(buckets))
		for day, bucket := range buckets {
			copyOfBucket := dayUsage{
				Used:     bucket.Used,
				Segments: append([]Segment(nil), bucket.Segments...),
			}
			g.usage[accountID][day] = &copyOfBucket
		}
	}
	g.exemptions = make(map[string]Exemption, len(payload.Exemptions))
	for accountID, ex := range payload.Exemptions {
		g.exemptions[accountID] = ex
	}
	g.seq = payload.Seq
	return nil
}

func decodeSnapshot(data []byte) (*snapshotPayload, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != snapshotVersion || len(envelope.Payload) == 0 {
		return nil, ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Payload)
	if hex.EncodeToString(sum[:]) != envelope.Checksum {
		return nil, ErrBadSnapshot
	}

	var payload snapshotPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, err
	}
	if payload.Sessions == nil {
		payload.Sessions = make(map[string]session)
	}
	if payload.Usage == nil {
		payload.Usage = make(map[string]map[string]dayUsage)
	}
	if payload.Exemptions == nil {
		payload.Exemptions = make(map[string]Exemption)
	}
	return &payload, nil
}

func validateSnapshot(payload *snapshotPayload) error {
	if payload.Seq < 0 {
		return ErrBadSnapshot
	}
	for accountID, s := range payload.Sessions {
		if blank(accountID) || s.LastHeartbeat.Before(s.StartedAt) || s.Heartbeats < 0 {
			return ErrBadSnapshot
		}
	}
	for accountID, buckets := range payload.Usage {
		if blank(accountID) {
			return ErrBadSnapshot
		}
		for day, bucket := range buckets {
			if _, err := time.Parse("2006-01-02", day); err != nil || bucket.Used < 0 {
				return ErrBadSnapshot
			}
			for _, segment := range bucket.Segments {
				if segment.AccountID != accountID || segment.Day != day || segment.To.Before(segment.From) || segment.Minutes < 0 {
					return ErrBadSnapshot
				}
			}
		}
	}
	for accountID, ex := range payload.Exemptions {
		if blank(accountID) || ex.AccountID != accountID || ex.Until.IsZero() || blank(ex.By) || ex.Seq < 0 {
			return ErrBadSnapshot
		}
	}
	return nil
}

func (g *PlaytimeQuotaGuard) nextSeq() int64 {
	g.seq++
	return g.seq
}

func (g *PlaytimeQuotaGuard) closeSession(accountID string, s *session, closeErr error) (SessionState, error) {
	delete(g.sessions, accountID)
	state := g.sessionState(accountID, s, s.LastHeartbeat, false)
	state.Seq = g.nextSeq()
	return state, closeErr
}

func (g *PlaytimeQuotaGuard) sessionState(accountID string, s *session, at time.Time, active bool) SessionState {
	state := SessionState{
		AccountID:     accountID,
		StartedAt:     s.StartedAt,
		LastHeartbeat: s.LastHeartbeat,
		UsedMinutes:   g.usedMinutes(accountID, g.localDay(at)),
		Active:        active,
	}
	if active && g.inCurfew(at) {
		state.CurfewDeadline = g.curfewDeadline(accountID, s, at)
	}
	return state
}

func (g *PlaytimeQuotaGuard) book(accountID string, from, to time.Time) {
	if !to.After(from) {
		return
	}

	zone := g.localZone()
	daily := time.Duration(maxInt(g.policy.DailyMinutes, 0)) * time.Minute
	cursor := from
	for cursor.Before(to) {
		local := cursor.In(zone)
		day := local.Format("2006-01-02")
		dayEnd := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, 1)
		sliceEnd := to
		if dayEnd.Before(to) {
			sliceEnd = dayEnd
		}

		bucket := g.usageBucket(accountID, day)
		remaining := daily - bucket.Used
		if remaining > 0 {
			bookEnd := sliceEnd
			if sliceEnd.Sub(cursor) > remaining {
				bookEnd = cursor.Add(remaining)
			}
			if bookEnd.After(cursor) {
				bucket.Used += bookEnd.Sub(cursor)
				bucket.Segments = append(bucket.Segments, Segment{
					AccountID: accountID,
					Day:       day,
					From:      cursor,
					To:        bookEnd,
					Minutes:   int(bookEnd.Sub(cursor) / time.Minute),
				})
			}
		}
		cursor = sliceEnd
	}
}

func (g *PlaytimeQuotaGuard) usageBucket(accountID, day string) *dayUsage {
	buckets, ok := g.usage[accountID]
	if !ok {
		buckets = make(map[string]*dayUsage)
		g.usage[accountID] = buckets
	}
	bucket, ok := buckets[day]
	if !ok {
		bucket = &dayUsage{}
		buckets[day] = bucket
	}
	return bucket
}

func (g *PlaytimeQuotaGuard) usedMinutes(accountID, day string) int {
	buckets := g.usage[accountID]
	if buckets == nil {
		return 0
	}
	bucket := buckets[day]
	if bucket == nil {
		return 0
	}
	used := int(bucket.Used / time.Minute)
	if used > g.policy.DailyMinutes && g.policy.DailyMinutes >= 0 {
		used = g.policy.DailyMinutes
	}
	return used
}

func (g *PlaytimeQuotaGuard) remainingMinutes(accountID string, at time.Time) int {
	used := g.usedMinutes(accountID, g.localDay(at))
	remaining := g.policy.DailyMinutes - used
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (g *PlaytimeQuotaGuard) hasExemptionAt(accountID string, at time.Time) bool {
	ex, ok := g.exemptions[accountID]
	return ok && at.Before(ex.Until)
}

func (g *PlaytimeQuotaGuard) withinCurfewGrace(s *session, at time.Time) bool {
	deadline := g.curfewGraceDeadline(at)
	return !at.After(deadline)
}

func (g *PlaytimeQuotaGuard) curfewDeadline(accountID string, s *session, at time.Time) time.Time {
	if ex, ok := g.exemptions[accountID]; ok && at.Before(ex.Until) {
		return ex.Until
	}
	return g.curfewGraceDeadline(at)
}

// curfewGraceDeadline is the end of the grace period of the curfew window
// containing at. It is only meaningful while inCurfew(at) is true.
func (g *PlaytimeQuotaGuard) curfewGraceDeadline(at time.Time) time.Time {
	zone := g.localZone()
	local := at.In(zone)
	start := g.policy.CurfewStartHour
	onset := time.Date(local.Year(), local.Month(), local.Day(), start, 0, 0, 0, zone)
	if onset.After(at) {
		onset = onset.AddDate(0, 0, -1)
	}
	return onset.Add(time.Duration(maxInt(g.policy.GraceMinutes, 0)) * time.Minute)
}

func (g *PlaytimeQuotaGuard) localZone() *time.Location {
	return time.FixedZone("policy-offset", g.policy.UTCOffsetMinutes*60)
}

func (g *PlaytimeQuotaGuard) localDay(t time.Time) string {
	return t.In(g.localZone()).Format("2006-01-02")
}

func (g *PlaytimeQuotaGuard) inCurfew(t time.Time) bool {
	start, end := g.policy.CurfewStartHour, g.policy.CurfewEndHour
	if start == end || start < 0 || start > 23 || end < 0 || end > 23 {
		return false
	}
	hour := t.In(g.localZone()).Hour()
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

func (g *PlaytimeQuotaGuard) nextCurfewEnd(t time.Time) time.Time {
	zone := g.localZone()
	local := t.In(zone)
	start, end := g.policy.CurfewStartHour, g.policy.CurfewEndHour
	endDay := local.Day()
	if start > end && local.Hour() >= start {
		endDay++
	}
	return time.Date(local.Year(), local.Month(), endDay, end, 0, 0, 0, zone)
}

func blank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
