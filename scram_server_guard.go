// Package task runs the server half of a SCRAM-SHA-256 handshake for the
// database proxy: it validates the nonce concatenation, the channel binding
// flag and data, the iteration floor and the authorisation identity, and it
// keeps every handshake state bounded.
package task

import (
	"errors"
	"sync"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// GS2 channel binding flags.
const (
	CBindNone      = "n"
	CBindSupported = "y"
	CBindUsed      = "p"
)

// Errors returned by the guard.
var (
	ErrBadClientFirst         = errors.New("client-first message is malformed")
	ErrUnknownUser            = errors.New("unknown user")
	ErrIterationsTooLow       = errors.New("iteration count is below the policy minimum")
	ErrBadPolicy              = errors.New("policy is invalid")
	ErrInflightLimit          = errors.New("too many handshakes in flight")
	ErrUnknownState           = errors.New("unknown or expired handshake state")
	ErrNonceMismatch          = errors.New("client nonce does not extend the server nonce")
	ErrChannelBindingMismatch = errors.New("channel binding data does not match the connection")
	ErrAuthenticationFailed   = errors.New("authentication failed")
	ErrLocked                 = errors.New("too many failures for this account and source")
	ErrBadSnapshot            = errors.New("snapshot payload is invalid")
)

// StoredCredential is the salted verifier material kept for one account. No
// plaintext password is ever stored.
type StoredCredential struct {
	TenantID   string `json:"tenant_id"`
	Username   string `json:"username"`
	Salt       []byte `json:"salt"`
	Iterations int    `json:"iterations"`
	StoredKey  []byte `json:"stored_key"`
	ServerKey  []byte `json:"server_key"`
}

// CredentialStore looks up the verifier material of one account.
type CredentialStore interface {
	Lookup(tenantID, username string) (StoredCredential, bool)
}

// ScramPolicy is the server side policy applied to newly started handshakes.
type ScramPolicy struct {
	MinIterations int   `json:"min_iterations"`
	MaxInflight   int   `json:"max_inflight"`
	StateTTL      int64 `json:"state_ttl"`
	MaxFailures   int   `json:"max_failures"`
}

// Principal is the authenticated identity of a finished handshake. ServerFinal
// carries the server-final message the proxy must send back.
type Principal struct {
	TenantID    string `json:"tenant_id"`
	Username    string `json:"username"`
	AuthzID     string `json:"authzid"`
	Iterations  int    `json:"iterations"`
	ServerFinal []byte `json:"server_final"`
}

// ScramServerGuard owns the in flight handshakes, the failure counters and the
// policy snapshot taken at the start of each handshake.
type ScramServerGuard struct {
	mu       sync.Mutex
	tenantID string
	store    CredentialStore
	policy   ScramPolicy
	now      func() int64
	cbind    []byte
}

// NewScramServerGuard builds a guard for one tenant on top of a credential
// store and an initial policy.
func NewScramServerGuard(tenantID string, store CredentialStore, p ScramPolicy) *ScramServerGuard {
	return &ScramServerGuard{tenantID: tenantID, store: store, policy: p}
}

// Begin parses a client-first message and answers with the server-first
// message and the handshake id.
func (g *ScramServerGuard) Begin(clientFirst []byte) (serverFirst []byte, stateID string, err error) {
	return nil, "", errNotImplemented
}

// BeginFrom is Begin with the source address of the connection, used by the
// failure counters.
func (g *ScramServerGuard) BeginFrom(source string, clientFirst []byte) (serverFirst []byte, stateID string, err error) {
	return nil, "", errNotImplemented
}

// Continue verifies a client-final message and returns the authenticated
// principal.
func (g *ScramServerGuard) Continue(stateID string, clientFinal []byte) (Principal, error) {
	return Principal{}, errNotImplemented
}

// Abort drops one handshake state.
func (g *ScramServerGuard) Abort(stateID string) error {
	return errNotImplemented
}

// SetPolicy replaces the policy used by handshakes started afterwards.
func (g *ScramServerGuard) SetPolicy(p ScramPolicy) error {
	return errNotImplemented
}

// Policy reports the current policy.
func (g *ScramServerGuard) Policy() ScramPolicy {
	return ScramPolicy{}
}

// Pending counts the live handshake states.
func (g *ScramServerGuard) Pending() int {
	return 0
}

// Sweep drops the handshake states whose TTL has elapsed and reports how many
// were dropped.
func (g *ScramServerGuard) Sweep() int {
	return 0
}

// SetClock replaces the wall clock used for TTL accounting.
func (g *ScramServerGuard) SetClock(now func() int64) {
}

// SetChannelBinding sets the channel binding digest of the current connection.
func (g *ScramServerGuard) SetChannelBinding(digest []byte) {
}

// Failures reports the authentication failure count of one account and source.
func (g *ScramServerGuard) Failures(username, source string) int {
	return 0
}

// Snapshot serialises the policy, the live states and the failure counters.
func (g *ScramServerGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ScramServerGuard) Restore(data []byte) error {
	return errNotImplemented
}
