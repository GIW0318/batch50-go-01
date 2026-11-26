// Package task verifies RFC 3161 style timestamp tokens against a trust store:
// it binds the token to a message imprint and a nonce, and it re-validates the
// timestamping certificate at the moment the token was signed instead of at the
// moment the check runs.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Verdict reasons.
const (
	ReasonOK         = "ok"
	ReasonImprint    = "imprint"
	ReasonCertWindow = "cert-window"
	ReasonRevoked    = "revoked"
	ReasonUntrusted  = "untrusted"
	ReasonPolicy     = "policy"
	ReasonMalformed  = "malformed"
)

// Errors reported by the verifier.
var (
	ErrBadToken        = errors.New("timestamp token is malformed")
	ErrNonceMissing    = errors.New("token carries no nonce")
	ErrNonceMismatch   = errors.New("token nonce does not match the request")
	ErrImprintMismatch = errors.New("message imprint is invalid")
	ErrBadTrust        = errors.New("trust anchor is invalid")
	ErrBadRevocation   = errors.New("revocation entry is invalid")
	ErrTooManyTokens   = errors.New("batch exceeds the token cap")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Imprint is the message digest the token must cover, plus the nonce the
// requester sent with it.
type Imprint struct {
	Algorithm string
	Digest    string
	Nonce     string
}

// Cert is one certificate of a timestamping chain.
type Cert struct {
	Subject   string
	Issuer    string
	Serial    string
	NotBefore time.Time
	NotAfter  time.Time
	EKUs      []string
}

// Verdict is the outcome of checking one token.
type Verdict struct {
	TokenID string
	TSA     string
	Valid   bool
	Reason  string
	GenTime time.Time
	Cached  bool
	Skew    time.Duration
}

// Stats counts the verification work done so far.
type Stats struct {
	Verified int
	Rejected int
	Cached   int
	Trusted  int
}

// Limits bounds the batch size, the verdict cache and supplies the local clock.
type Limits struct {
	MaxBatch  int
	CacheSize int
	Now       time.Time
}

// TimestampAuthorityVerifier owns the trust store, the revocation list and the
// verdict cache.
type TimestampAuthorityVerifier struct {
	mu       sync.Mutex
	limits   Limits
	trust    map[string][]Cert
	policies map[string]bool
	revoked  map[string]time.Time
	cache    map[string]Verdict
	verdicts map[string]Verdict
	stats    Stats
}

// NewTimestampAuthorityVerifier builds a verifier with the given limits.
func NewTimestampAuthorityVerifier(limits Limits) *TimestampAuthorityVerifier {
	return &TimestampAuthorityVerifier{limits: limits}
}

// Trust registers a timestamping authority and its certificate chain.
func (v *TimestampAuthorityVerifier) Trust(tsa string, chain []Cert) error {
	return errNotImplemented
}

// AllowPolicy registers a timestamp policy identifier as acceptable.
func (v *TimestampAuthorityVerifier) AllowPolicy(policyID string) error {
	return errNotImplemented
}

// Revoke marks a certificate serial as revoked from the given moment.
func (v *TimestampAuthorityVerifier) Revoke(serial string, at time.Time) error {
	return errNotImplemented
}

// Verify checks one token against the imprint.
func (v *TimestampAuthorityVerifier) Verify(token []byte, imprint Imprint) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Batch checks a list of tokens and keeps their order.
func (v *TimestampAuthorityVerifier) Batch(tokens [][]byte, imprint Imprint) ([]Verdict, error) {
	return nil, errNotImplemented
}

// Stats returns the counters.
func (v *TimestampAuthorityVerifier) Stats() Stats {
	return Stats{}
}

// Trusted lists the registered authority names in ascending order.
func (v *TimestampAuthorityVerifier) Trusted() []string {
	return nil
}

// VerdictFor reads the stored verdict of one token.
func (v *TimestampAuthorityVerifier) VerdictFor(tokenID string) (Verdict, bool) {
	return Verdict{}, false
}

// Snapshot serialises the trust store, revocations and the verdict cache.
func (v *TimestampAuthorityVerifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the verifier state with a snapshot, all or nothing.
func (v *TimestampAuthorityVerifier) Restore(data []byte) error {
	return errNotImplemented
}

// Reset drops the trust store, revocations and every stored verdict.
func (v *TimestampAuthorityVerifier) Reset() error {
	return errNotImplemented
}
