// Package task implements the verifier side of an OpenID4VP exchange: request
// creation, presentation definition matching, holder binding checks and
// credential status list validation.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Lifecycle states of one verification request.
const (
	StatusPending  = "pending"
	StatusVerified = "verified"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
)

// Reason codes attached to a verification result.
const (
	ReasonOK                = "ok"
	ReasonConstraintFailed  = "constraint_failed"
	ReasonNonceMismatch     = "nonce_mismatch"
	ReasonAudienceMismatch  = "audience_mismatch"
	ReasonHolderMismatch    = "holder_mismatch"
	ReasonRevoked           = "credential_revoked"
	ReasonStatusEpoch       = "status_epoch_mismatch"
	ReasonFormatUnsupported = "format_unsupported"
)

// Errors reported by the verifier.
var (
	ErrBadDefinition     = errors.New("presentation definition is invalid")
	ErrUnsupportedFormat = errors.New("presentation format is not supported")
	ErrRequestLimit      = errors.New("pending request limit reached for this tenant")
	ErrUnknownRequest    = errors.New("verification request is unknown")
	ErrRequestClosed     = errors.New("verification request already reached a conclusion")
	ErrBadToken          = errors.New("vp_token or submission is malformed")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Config bounds the verifier. Now is injectable so that expiry can be driven by
// a test clock.
type Config struct {
	MaxPendingPerTenant      int
	MaxCredentialsPerRequest int
	TTL                      time.Duration
	Formats                  []string
	Audience                 string
	Now                      func() time.Time
}

// FieldConstraint pins one claim path to an accepted set of values.
type FieldConstraint struct {
	Path   string
	Values []string
}

// InputDescriptor declares one credential the wallet has to present.
type InputDescriptor struct {
	ID      string
	Formats []string
	Fields  []FieldConstraint
}

// PresentationDefinition is the set of descriptors sent to the wallet.
type PresentationDefinition struct {
	ID          string
	Descriptors []InputDescriptor
}

// Request is one outstanding authorization request.
type Request struct {
	ID           string
	TenantID     string
	Nonce        string
	DefinitionID string
	ExpiresAt    time.Time
}

// RequestState is the durable view of one request.
type RequestState struct {
	ID        string
	TenantID  string
	Status    string
	Submitted int
	Matched   []string
	Subject   string
	Reason    string
}

// Result is the conclusion of one Verify call.
type Result struct {
	RequestID string
	Status    string
	Reason    string
	Matched   []string
	Subject   string
}

// StatusList is one credential status list snapshot keyed by its URI.
type StatusList struct {
	URI     string
	Epoch   int64
	Bits    int
	Revoked []int
}

// PresentationVerifier owns the request table, the descriptor state and the
// status list cache.
type PresentationVerifier struct {
	cfg Config
}

// NewPresentationVerifier builds a verifier with the given caps.
func NewPresentationVerifier(cfg Config) *PresentationVerifier {
	return &PresentationVerifier{cfg: cfg}
}

// CreateRequest records a new request and returns the nonce to hand to the
// wallet.
func (v *PresentationVerifier) CreateRequest(tenantID string, def PresentationDefinition) (Request, error) {
	return Request{}, errNotImplemented
}

// Verify checks one vp_token plus submission against an outstanding request.
func (v *PresentationVerifier) Verify(requestID, vpToken, submission string) (Result, error) {
	return Result{}, errNotImplemented
}

// State reads the durable state of one request.
func (v *PresentationVerifier) State(requestID string) (RequestState, error) {
	return RequestState{}, errNotImplemented
}

// Expire sweeps every pending request whose TTL elapsed and returns how many
// were closed.
func (v *PresentationVerifier) Expire(now time.Time) int {
	return 0
}

// PutStatusList installs or replaces one status list snapshot.
func (v *PresentationVerifier) PutStatusList(list StatusList) error {
	return errNotImplemented
}

// StatusListEpoch reports the epoch currently cached for one status list URI.
func (v *PresentationVerifier) StatusListEpoch(uri string) (int64, bool) {
	return 0, false
}

// SupportedFormats lists the credential formats the verifier accepts.
func (v *PresentationVerifier) SupportedFormats() []string {
	return nil
}

// PendingRequests lists the open requests of one tenant, oldest first.
func (v *PresentationVerifier) PendingRequests(tenantID string) []RequestState {
	return nil
}

// Snapshot serialises the request table, the conclusions and the status list
// cache.
func (v *PresentationVerifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole verifier state with a snapshot, all or nothing.
func (v *PresentationVerifier) Restore(data []byte) error {
	return errNotImplemented
}
