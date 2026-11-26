// Package task binds an authentication session to the exporter value of the
// TLS channel that carries it and verifies client proofs against that value.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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

// ChannelBindingVerifier owns the session table and its bindings.
type ChannelBindingVerifier struct {
	limits Limits
}

// NewChannelBindingVerifier builds a verifier with the given limits.
func NewChannelBindingVerifier(limits Limits) *ChannelBindingVerifier {
	return &ChannelBindingVerifier{limits: limits}
}

// Open registers one authentication session on a channel.
func (v *ChannelBindingVerifier) Open(sessionID string, principal string, channel ChannelRef) (Binding, error) {
	return Binding{}, errNotImplemented
}

// Bind collects the exporter value of the channel exactly once.
func (v *ChannelBindingVerifier) Bind(sessionID string, exporter []byte) (Binding, error) {
	return Binding{}, errNotImplemented
}

// Verify compares a client proof against the bound exporter value.
func (v *ChannelBindingVerifier) Verify(sessionID string, proof []byte) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Downgrade marks a session as running without a usable binding.
func (v *ChannelBindingVerifier) Downgrade(sessionID string, reason string) error {
	return errNotImplemented
}

// Reverify asks a verified session to prove the binding again after a channel
// update, without dropping the binding.
func (v *ChannelBindingVerifier) Reverify(sessionID string) error {
	return errNotImplemented
}

// Expire ends the binding of one session.
func (v *ChannelBindingVerifier) Expire(sessionID string) error {
	return errNotImplemented
}

// Explain returns the recorded action trace of one session.
func (v *ChannelBindingVerifier) Explain(sessionID string) (Trace, error) {
	return Trace{}, errNotImplemented
}

// State returns the binding state of one session, or an empty string.
func (v *ChannelBindingVerifier) State(sessionID string) string {
	return ""
}

// BindingOf reads one binding record.
func (v *ChannelBindingVerifier) BindingOf(sessionID string) (Binding, bool) {
	return Binding{}, false
}

// Pending counts the sessions that still hold an in-flight binding.
func (v *ChannelBindingVerifier) Pending() int {
	return 0
}

// Snapshot serialises the session table, counters and traces. Bindings are not
// carried over, because a new process never owns the old TLS channel.
func (v *ChannelBindingVerifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *ChannelBindingVerifier) Restore(data []byte) error {
	return errNotImplemented
}
