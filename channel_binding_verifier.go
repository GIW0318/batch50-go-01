// Package task binds an authentication session to the exporter value of the
// TLS channel that carries it and verifies client proofs against that value.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Channel sources.
const (
	SourceDirect = "direct"
	SourceProxy  = "proxy"
)

// Binding states.
const (
	StateUnbound    = "unbound"
	StateBound      = "bound"
	StateVerified   = "verified"
	StateDowngraded = "downgraded"
	StateExpired    = "expired"
)

// Verdict results.
const (
	VerdictMatch    = "match"
	VerdictMismatch = "mismatch"
)

// Verdict reasons.
const (
	ReasonMatch    = "reason_match"
	ReasonMismatch = "reason_mismatch"
)

// Trace step names recorded per successful operation.
const (
	stepOpen      = "open"
	stepBind      = "bind"
	stepVerify    = "verify"
	stepDowngrade = "downgrade"
	stepReverify  = "reverify"
	stepExpire    = "expire"
)

// Errors returned by the verifier.
var (
	ErrEmptyIdentity     = errors.New("session id, principal and channel id are required")
	ErrSessionExists     = errors.New("session already registered")
	ErrBadSource         = errors.New("channel source must be direct or proxy")
	ErrUntrustedProxy    = errors.New("proxy identity is not trusted")
	ErrInflightLimit     = errors.New("in-flight binding limit reached")
	ErrSessionNotFound   = errors.New("session not found")
	ErrAlreadyBound      = errors.New("exporter already bound for this session")
	ErrBadExporter       = errors.New("exporter value is empty")
	ErrExporterTooLarge  = errors.New("exporter value exceeds the byte cap")
	ErrBadProof          = errors.New("binding proof is empty")
	ErrBindingMismatch   = errors.New("binding proof does not match the channel")
	ErrBindingExpired    = errors.New("channel binding expired")
	ErrNotBound          = errors.New("session has no usable binding")
	ErrBadReason         = errors.New("downgrade reason is required")
	ErrAlreadyDowngraded = errors.New("session already downgraded")
	ErrAlreadyExpired    = errors.New("session already expired")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Limits bounds in-flight bindings, their lifetime and the exporter size.
type Limits struct {
	MaxInflight      int
	BindingTTL       time.Duration
	MaxExporterBytes int
	MaxTrace         int
	TrustedProxies   []string
	Now              func() time.Time
}

// ChannelRef identifies the TLS channel that carries a session.
type ChannelRef struct {
	ID          string
	Source      string
	ForwardedBy string
}

// Binding is the per session binding record.
type Binding struct {
	SessionID  string
	Principal  string
	ChannelID  string
	Source     string
	State      string
	Digest     string
	Reason     string
	BoundAt    time.Time
	ExpiresAt  time.Time
	Verifies   int
	Mismatches int
	Seq        int64
}

// Verdict is the outcome of one proof check.
type Verdict struct {
	SessionID string
	Result    string
	Reason    string
	Digest    string
	At        time.Time
	Seq       int64
}

// Trace records the actions seen for one session.
type Trace struct {
	SessionID string
	Steps     []string
	Seq       int64
}

// session is the internal per session record: one binding plus its trace.
type session struct {
	binding Binding
	channel ChannelRef
	steps   []string
}

// ChannelBindingVerifier owns the session table and its bindings.
type ChannelBindingVerifier struct {
	limits  Limits
	now     func() time.Time
	mu      sync.RWMutex
	session map[string]*session
	pending int
}

// NewChannelBindingVerifier builds a verifier with the given limits.
func NewChannelBindingVerifier(limits Limits) *ChannelBindingVerifier {
	now := limits.Now
	if now == nil {
		now = time.Now
	}
	return &ChannelBindingVerifier{
		limits:  limits,
		now:     now,
		session: make(map[string]*session),
	}
}

func digestOf(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func (v *ChannelBindingVerifier) trustedProxy(identity string) bool {
	for _, trusted := range v.limits.TrustedProxies {
		if identity == trusted {
			return true
		}
	}
	return false
}

// inFlight reports whether the session still counts against MaxInflight.
func inFlight(state string) bool {
	switch state {
	case StateUnbound, StateBound, StateVerified:
		return true
	}
	return false
}

// record appends one successful step to the session trace and advances Seq.
func (v *ChannelBindingVerifier) record(s *session, step string) {
	if v.limits.MaxTrace <= 0 || len(s.steps) < v.limits.MaxTrace {
		s.steps = append(s.steps, step)
	}
	s.binding.Seq++
}

// expireIfStaleLocked lazily ends a binding whose TTL has run out. The caller
// must hold the write lock.
func (v *ChannelBindingVerifier) expireIfStaleLocked(s *session, now time.Time) {
	b := &s.binding
	if (b.State == StateBound || b.State == StateVerified) && !b.ExpiresAt.IsZero() && !now.Before(b.ExpiresAt) {
		b.State = StateExpired
		v.pending--
	}
}

// Open registers one authentication session on a channel.
func (v *ChannelBindingVerifier) Open(sessionID string, principal string, channel ChannelRef) (Binding, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(principal) == "" || strings.TrimSpace(channel.ID) == "" {
		return Binding{}, ErrEmptyIdentity
	}
	if channel.Source != SourceDirect && channel.Source != SourceProxy {
		return Binding{}, ErrBadSource
	}
	if channel.Source == SourceProxy && !v.trustedProxy(channel.ForwardedBy) {
		return Binding{}, ErrUntrustedProxy
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.session[sessionID]; ok {
		return Binding{}, ErrSessionExists
	}
	if v.limits.MaxInflight > 0 && v.pending >= v.limits.MaxInflight {
		return Binding{}, ErrInflightLimit
	}
	s := &session{
		binding: Binding{
			SessionID: sessionID,
			Principal: principal,
			ChannelID: channel.ID,
			Source:    channel.Source,
			State:     StateUnbound,
		},
		channel: channel,
	}
	v.record(s, stepOpen)
	v.session[sessionID] = s
	v.pending++
	return s.binding, nil
}

// Bind collects the exporter value of the channel exactly once.
func (v *ChannelBindingVerifier) Bind(sessionID string, exporter []byte) (Binding, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.session[sessionID]
	if !ok {
		return Binding{}, ErrSessionNotFound
	}
	now := v.now()
	v.expireIfStaleLocked(s, now)
	switch s.binding.State {
	case StateExpired:
		return Binding{}, ErrBindingExpired
	case StateDowngraded:
		return Binding{}, ErrNotBound
	case StateBound, StateVerified:
		return Binding{}, ErrAlreadyBound
	}
	if len(exporter) == 0 {
		return Binding{}, ErrBadExporter
	}
	if v.limits.MaxExporterBytes > 0 && len(exporter) > v.limits.MaxExporterBytes {
		return Binding{}, ErrExporterTooLarge
	}
	s.binding.Digest = digestOf(exporter)
	s.binding.BoundAt = now
	s.binding.ExpiresAt = now.Add(v.limits.BindingTTL)
	s.binding.State = StateBound
	v.record(s, stepBind)
	return s.binding, nil
}

// Verify compares a client proof against the bound exporter value.
func (v *ChannelBindingVerifier) Verify(sessionID string, proof []byte) (Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.session[sessionID]
	if !ok {
		return Verdict{}, ErrSessionNotFound
	}
	now := v.now()
	v.expireIfStaleLocked(s, now)
	switch s.binding.State {
	case StateExpired:
		return Verdict{}, ErrBindingExpired
	case StateUnbound, StateDowngraded:
		return Verdict{}, ErrNotBound
	}
	if len(proof) == 0 {
		return Verdict{}, ErrBadProof
	}
	if digestOf(proof) != s.binding.Digest {
		s.binding.Mismatches++
		return Verdict{
			SessionID: sessionID,
			Result:    VerdictMismatch,
			Reason:    ReasonMismatch,
			Digest:    s.binding.Digest,
			At:        now,
			Seq:       s.binding.Seq,
		}, ErrBindingMismatch
	}
	s.binding.State = StateVerified
	s.binding.Verifies++
	v.record(s, stepVerify)
	return Verdict{
		SessionID: sessionID,
		Result:    VerdictMatch,
		Reason:    ReasonMatch,
		Digest:    s.binding.Digest,
		At:        now,
		Seq:       s.binding.Seq,
	}, nil
}

// Downgrade marks a session as running without a usable binding.
func (v *ChannelBindingVerifier) Downgrade(sessionID string, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrBadReason
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.session[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	switch s.binding.State {
	case StateDowngraded:
		return ErrAlreadyDowngraded
	case StateExpired:
		return ErrAlreadyExpired
	}
	if inFlight(s.binding.State) {
		v.pending--
	}
	s.binding.State = StateDowngraded
	s.binding.Reason = reason
	v.record(s, stepDowngrade)
	return nil
}

// Reverify asks a verified session to prove the binding again after a channel
// update, without dropping the binding.
func (v *ChannelBindingVerifier) Reverify(sessionID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.session[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	now := v.now()
	v.expireIfStaleLocked(s, now)
	switch s.binding.State {
	case StateExpired:
		return ErrBindingExpired
	case StateUnbound, StateDowngraded:
		return ErrNotBound
	}
	s.binding.State = StateBound
	v.record(s, stepReverify)
	return nil
}

// Expire ends the binding of one session.
func (v *ChannelBindingVerifier) Expire(sessionID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.session[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	v.expireIfStaleLocked(s, v.now())
	switch s.binding.State {
	case StateExpired:
		return ErrAlreadyExpired
	case StateDowngraded:
		return ErrAlreadyDowngraded
	}
	v.pending--
	s.binding.State = StateExpired
	v.record(s, stepExpire)
	return nil
}

// Explain returns the recorded action trace of one session.
func (v *ChannelBindingVerifier) Explain(sessionID string) (Trace, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.session[sessionID]
	if !ok {
		return Trace{}, ErrSessionNotFound
	}
	steps := make([]string, len(s.steps))
	copy(steps, s.steps)
	return Trace{SessionID: sessionID, Steps: steps, Seq: s.binding.Seq}, nil
}

// State returns the binding state of one session, or an empty string.
func (v *ChannelBindingVerifier) State(sessionID string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.session[sessionID]
	if !ok {
		return ""
	}
	return s.binding.State
}

// BindingOf reads one binding record.
func (v *ChannelBindingVerifier) BindingOf(sessionID string) (Binding, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.session[sessionID]
	if !ok {
		return Binding{}, false
	}
	return s.binding, true
}

// Pending counts the sessions that still hold an in-flight binding.
func (v *ChannelBindingVerifier) Pending() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.pending
}

// snapshotSession is the serialized form of one session. Bindings are never
// serialized: a restart invalidates them by definition.
type snapshotSession struct {
	SessionID   string   `json:"session_id"`
	Principal   string   `json:"principal"`
	ChannelID   string   `json:"channel_id"`
	Source      string   `json:"source"`
	ForwardedBy string   `json:"forwarded_by,omitempty"`
	Steps       []string `json:"steps"`
	Seq         int64    `json:"seq"`
	Verifies    int      `json:"verifies"`
	Mismatches  int      `json:"mismatches"`
}

type snapshotDoc struct {
	Sessions []snapshotSession `json:"sessions"`
}

// Snapshot serializes the session table without any live binding material.
func (v *ChannelBindingVerifier) Snapshot() ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	doc := snapshotDoc{Sessions: make([]snapshotSession, 0, len(v.session))}
	for _, s := range v.session {
		steps := make([]string, len(s.steps))
		copy(steps, s.steps)
		doc.Sessions = append(doc.Sessions, snapshotSession{
			SessionID:   s.binding.SessionID,
			Principal:   s.binding.Principal,
			ChannelID:   s.binding.ChannelID,
			Source:      s.binding.Source,
			ForwardedBy: s.channel.ForwardedBy,
			Steps:       steps,
			Seq:         s.binding.Seq,
			Verifies:    s.binding.Verifies,
			Mismatches:  s.binding.Mismatches,
		})
	}
	sort.Slice(doc.Sessions, func(i, j int) bool {
		return doc.Sessions[i].SessionID < doc.Sessions[j].SessionID
	})
	return json.Marshal(doc)
}

// Restore replaces the whole state with a snapshot, all or nothing. Every
// restored session comes back unbound: the old channel is gone, so the old
// digest and binding times must not be resurrected.
func (v *ChannelBindingVerifier) Restore(data []byte) error {
	var doc snapshotDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return ErrBadSnapshot
	}
	sessions := make(map[string]*session, len(doc.Sessions))
	for _, entry := range doc.Sessions {
		if strings.TrimSpace(entry.SessionID) == "" || strings.TrimSpace(entry.Principal) == "" || strings.TrimSpace(entry.ChannelID) == "" {
			return ErrBadSnapshot
		}
		if entry.Source != SourceDirect && entry.Source != SourceProxy {
			return ErrBadSnapshot
		}
		if entry.Seq < 1 {
			return ErrBadSnapshot
		}
		if _, dup := sessions[entry.SessionID]; dup {
			return ErrBadSnapshot
		}
		steps := make([]string, len(entry.Steps))
		copy(steps, entry.Steps)
		sessions[entry.SessionID] = &session{
			binding: Binding{
				SessionID:  entry.SessionID,
				Principal:  entry.Principal,
				ChannelID:  entry.ChannelID,
				Source:     entry.Source,
				State:      StateUnbound,
				Verifies:   entry.Verifies,
				Mismatches: entry.Mismatches,
				Seq:        entry.Seq,
			},
			channel: ChannelRef{
				ID:          entry.ChannelID,
				Source:      entry.Source,
				ForwardedBy: entry.ForwardedBy,
			},
			steps: steps,
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.session = sessions
	v.pending = len(sessions)
	return nil
}
