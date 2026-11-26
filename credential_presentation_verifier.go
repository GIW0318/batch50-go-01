// Package task verifies verifiable credential presentations against a request
// scope, an issuer trust chain and a one-time request nonce.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Presentation decisions.
const (
	DecisionAccepted = "accepted"
	DecisionRejected = "rejected"
)

// Decision reasons attached to a result.
const (
	ReasonAccepted = "reason_accepted"
	ReasonRevoked  = "reason_revoked"
)

// Explain steps, in evaluation order.
var verifySteps = []string{"request", "nonce", "revocation", "issuer", "scope"}

// Errors returned by the verifier.
var (
	ErrBadSpec           = errors.New("presentation spec is incomplete")
	ErrUnknownIssuer     = errors.New("issuer is not in the trust registry")
	ErrRequestLimit      = errors.New("in-flight request limit reached")
	ErrRequestNotFound   = errors.New("presentation request not found")
	ErrRequestExpired    = errors.New("presentation request expired")
	ErrRequestClosed     = errors.New("presentation request is closed")
	ErrTooManyAttempts   = errors.New("presentation attempts exhausted")
	ErrRequestMismatch   = errors.New("presentation does not belong to this request")
	ErrNonceMismatch     = errors.New("presentation nonce does not match the request")
	ErrIssuerNotAllowed  = errors.New("issuer is not allowed by the request")
	ErrIssuerUntrusted   = errors.New("issuer chain is not trusted at issuance time")
	ErrScopeMismatch     = errors.New("disclosed fields do not satisfy the request scope")
	ErrTooManyDisclosed  = errors.New("too many disclosed fields")
	ErrBadPresentation   = errors.New("presentation is incomplete")
	ErrUnknownCredential = errors.New("credential not found")
	ErrBadReason         = errors.New("revocation reason is required")
	ErrAlreadyRevoked    = errors.New("credential already revoked")
	ErrResultNotFound    = errors.New("result not found")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Options bounds requests, attempts and disclosure volume.
type Options struct {
	MaxRequests  int
	MaxAttempts  int
	RequestTTL   time.Duration
	MaxDisclosed int
	Now          func() time.Time
	Issuers      []IssuerCert
}

// IssuerCert is one certificate of the issuer trust registry.
type IssuerCert struct {
	ID        string
	ParentID  string
	NotBefore time.Time
	NotAfter  time.Time
	Revoked   bool
}

// PresentationSpec is the relying party request for one presentation.
type PresentationSpec struct {
	VerifierID string
	Purpose    string
	Required   []string
	Optional   []string
	Issuers    []string
}

// RequestID identifies one open presentation request.
type RequestID string

// Nonce is the one-time challenge bound to a presentation request.
type Nonce string

// Presentation is what the wallet submits for one request.
type Presentation struct {
	RequestID    string
	CredentialID string
	IssuerID     string
	Subject      string
	Nonce        string
	IssuedAt     time.Time
	Disclosed    map[string]string
	Hashed       map[string]string
}

// Result is the terminal decision of one request.
type Result struct {
	ID        string
	RequestID string
	Decision  string
	Reason    string
	IssuerID  string
	Disclosed []string
	At        time.Time
	Seq       int64
}

// Trace explains how a result was reached.
type Trace struct {
	ResultID string
	Steps    []string
	Reason   string
	Attempts int
}

// CredentialPresentationVerifier owns requests, results, revocations and the
// issuer registry.
type CredentialPresentationVerifier struct {
	opts Options
}

// NewCredentialPresentationVerifier builds a verifier with the given options.
func NewCredentialPresentationVerifier(opts Options) *CredentialPresentationVerifier {
	return &CredentialPresentationVerifier{opts: opts}
}

// Request opens one presentation request and returns its identifier.
func (v *CredentialPresentationVerifier) Request(spec PresentationSpec) (RequestID, error) {
	return "", errNotImplemented
}

// Verify checks one presentation against its request.
func (v *CredentialPresentationVerifier) Verify(requestID string, presentation Presentation) (Result, error) {
	return Result{}, errNotImplemented
}

// Explain returns the evaluation trace of one result.
func (v *CredentialPresentationVerifier) Explain(resultID string) (Trace, error) {
	return Trace{}, errNotImplemented
}

// Revoke marks one credential as unusable.
func (v *CredentialPresentationVerifier) Revoke(credentialID string, reason string) error {
	return errNotImplemented
}

// Nonce returns the one-time challenge of one request.
func (v *CredentialPresentationVerifier) Nonce(requestID string) (Nonce, error) {
	return "", errNotImplemented
}

// Attempts counts the failed presentations of one request.
func (v *CredentialPresentationVerifier) Attempts(requestID string) int {
	return 0
}

// Pending counts the requests that can still accept a presentation.
func (v *CredentialPresentationVerifier) Pending() int {
	return 0
}

// Revoked reports whether one credential was revoked.
func (v *CredentialPresentationVerifier) Revoked(credentialID string) bool {
	return false
}

// ResultAt reads one stored result.
func (v *CredentialPresentationVerifier) ResultAt(resultID string) (Result, bool) {
	return Result{}, false
}

// Sweep closes every request whose lifetime passed and returns the count.
func (v *CredentialPresentationVerifier) Sweep() int {
	return 0
}

// Snapshot serialises requests, results, nonces and revocations.
func (v *CredentialPresentationVerifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *CredentialPresentationVerifier) Restore(data []byte) error {
	return errNotImplemented
}
