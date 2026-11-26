// Package task implements the server side of an HTTP Digest challenge: nonce
// generation, generation aware validation, nonce count watermarks and replay
// governance for the gateway.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Digest algorithms the guard knows about. MD5 is recognised only so that it
// can be rejected explicitly.
const (
	AlgorithmSHA256    = "SHA-256"
	AlgorithmSHA512256 = "SHA-512-256"
	AlgorithmMD5       = "MD5"
	AlgorithmNone      = "none"
)

// Quality of protection values accepted on the wire.
const (
	QopAuth    = "auth"
	QopAuthInt = "auth-int"
)

// Errors reported by the guard.
var (
	ErrBadChallenge      = errors.New("challenge parameters are invalid")
	ErrAlgorithmRejected = errors.New("digest algorithm is not accepted")
	ErrQopRejected       = errors.New("quality of protection is not accepted")
	ErrNonceInvalid      = errors.New("nonce is unknown or belongs to another realm")
	ErrStaleNonce        = errors.New("nonce has expired")
	ErrReplay            = errors.New("nonce count was already used")
	ErrAuthFailed        = errors.New("credentials rejected")
	ErrSessionLimit      = errors.New("per user session limit reached")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Config bounds the guard. Now is injectable so that expiry can be driven by a
// test clock.
type Config struct {
	Realm              string
	NonceWindow        time.Duration
	MaxNoncesPerUser   int
	MaxSessionsPerUser int
	MaxFailuresPerUser int
	Now                func() time.Time
}

// Header is the challenge that goes back to the client as WWW-Authenticate.
type Header struct {
	Realm     string
	Nonce     string
	Opaque    string
	Algorithm string
	Qop       string
	Stale     bool
}

// AuthRequest is one inbound Authorization header unpacked into fields.
type AuthRequest struct {
	Username    string
	Realm       string
	Nonce       string
	URI         string
	Method      string
	Qop         string
	NC          int64
	CNonce      string
	Response    string
	Algorithm   string
	Body        []byte
	Fingerprint string
}

// Principal is the authenticated caller returned by Verify.
type Principal struct {
	Username string
	Nonce    string
	NC       int64
	Sessions int
}

// NonceUsage is the per user accounting exposed for operators.
type NonceUsage struct {
	Username string
	Issued   int
	Active   int
	Replays  int
	MaxNC    int64
}

// DigestChallengeGuard owns the secret generations, the nonce table and the
// nonce count watermarks.
type DigestChallengeGuard struct {
	cfg Config
}

// NewDigestChallengeGuard builds a guard with the given caps.
func NewDigestChallengeGuard(cfg Config) *DigestChallengeGuard {
	return &DigestChallengeGuard{cfg: cfg}
}

// SetPassword registers the cleartext password used to recompute the digest.
func (g *DigestChallengeGuard) SetPassword(username, password string) error {
	return errNotImplemented
}

// Challenge issues a fresh nonce for one principal in one realm.
func (g *DigestChallengeGuard) Challenge(realm, username string) (Header, error) {
	return Header{}, errNotImplemented
}

// Verify checks one Authorization header and claims its nonce count.
func (g *DigestChallengeGuard) Verify(req AuthRequest) (Principal, error) {
	return Principal{}, errNotImplemented
}

// RotateSecret moves the guard to the next secret generation, which
// invalidates every nonce issued before the call.
func (g *DigestChallengeGuard) RotateSecret() error {
	return errNotImplemented
}

// Usage reports the nonce accounting of one user.
func (g *DigestChallengeGuard) Usage(username string) NonceUsage {
	return NonceUsage{}
}

// ValidateAlgorithm reports whether the guard accepts one digest algorithm.
func (g *DigestChallengeGuard) ValidateAlgorithm(algorithm string) error {
	return errNotImplemented
}

// NonceAge reports how long ago a live nonce was issued.
func (g *DigestChallengeGuard) NonceAge(nonce string) (time.Duration, error) {
	return 0, errNotImplemented
}

// Sweep reclaims expired nonces and returns how many records went away.
func (g *DigestChallengeGuard) Sweep(now time.Time) int {
	return 0
}

// ActiveNonces counts the live nonces of one user.
func (g *DigestChallengeGuard) ActiveNonces(username string) int {
	return 0
}

// Snapshot serialises the nonce table, the watermarks and the secret generation.
func (g *DigestChallengeGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole guard state with a snapshot, all or nothing.
func (g *DigestChallengeGuard) Restore(data []byte) error {
	return errNotImplemented
}
