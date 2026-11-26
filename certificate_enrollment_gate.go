// Package task implements the device certificate enrollment entry point: one
// time bootstrap credentials, CSR policy checks, an asynchronous issuance queue
// and per device attempt budgets.
package task

import (
	"context"
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Enrollment states.
const (
	StateQueued  = "queued"
	StateIssued  = "issued"
	StateFailed  = "failed"
	StateRevoked = "revoked"
)

// Errors reported by the gate.
var (
	ErrBadRequest          = errors.New("enrollment request is incomplete")
	ErrBootstrapRejected   = errors.New("bootstrap credential is not usable")
	ErrBadCSR              = errors.New("csr cannot be parsed")
	ErrKeyTooWeak          = errors.New("public key is below the required strength")
	ErrSANMissing          = errors.New("csr carries no subject alternative name")
	ErrSANOutsideTenant    = errors.New("subject alternative name is outside the tenant domain")
	ErrKeyUsageMismatch    = errors.New("csr key usage does not match the profile")
	ErrCAFlagForbidden     = errors.New("csr requests ca basic constraints")
	ErrWildcardTooBroad    = errors.New("wildcard subject alternative name is too broad")
	ErrUnknownProfile      = errors.New("issuance profile is not registered")
	ErrDuplicateEnrollment = errors.New("this public key is already enrolled")
	ErrAttemptsExhausted   = errors.New("device exhausted its enrollment attempts")
	ErrQueueFull           = errors.New("enrollment queue is full for this tenant")
	ErrUnknownEnrollment   = errors.New("enrollment is unknown")
	ErrBadSnapshot         = errors.New("snapshot payload is invalid")
)

// Config bounds the gate. Now is injectable so that expiry can be driven by a
// test clock.
type Config struct {
	DomainSuffix         string
	MinRSABits           int
	MaxAttemptsPerDevice int
	MaxQueue             int
	MaxPendingAge        time.Duration
	RetryAfter           time.Duration
	Now                  func() time.Time
}

// Profile is one issuance profile: allowed key usages, extended key usages and
// certificate lifetime.
type Profile struct {
	Name             string
	AllowedKeyUsages []string
	AllowedEKUs      []string
	TTL              time.Duration
}

// EnrollRequest is one inbound certificate request.
type EnrollRequest struct {
	DeviceID       string
	TenantID       string
	BootstrapToken string
	CSR            []byte
	Profile        string
}

// Enrollment is the handle returned to the device.
type Enrollment struct {
	ID             string
	DeviceID       string
	TenantID       string
	State          string
	RetryAfter     time.Duration
	CAEpoch        int
	Serial         string
	CSRFingerprint string
}

// State is the durable view of one enrollment.
type State struct {
	ID       string
	State    string
	Attempts int
	Serial   string
	Reason   string
}

// Remaining is the per device budget report.
type Remaining struct {
	Attempts int
	Queued   int
}

// EnrollmentGate owns the token ledger, the CSR queue and the device budgets.
type EnrollmentGate struct {
	cfg Config
}

// NewEnrollmentGate builds a gate with the given caps.
func NewEnrollmentGate(cfg Config) *EnrollmentGate {
	return &EnrollmentGate{cfg: cfg}
}

// RegisterBootstrapToken adds one single use bootstrap token for a tenant.
func (g *EnrollmentGate) RegisterBootstrapToken(token, tenantID string) error {
	return errNotImplemented
}

// SetProfile adds or replaces one issuance profile.
func (g *EnrollmentGate) SetProfile(p Profile) error {
	return errNotImplemented
}

// Enroll validates one request and places it on the issuance queue.
func (g *EnrollmentGate) Enroll(ctx context.Context, req EnrollRequest) (Enrollment, error) {
	return Enrollment{}, errNotImplemented
}

// Poll reads the state of one enrollment.
func (g *EnrollmentGate) Poll(enrollmentID string) (State, error) {
	return State{}, errNotImplemented
}

// Revoke retires one enrollment and its certificate.
func (g *EnrollmentGate) Revoke(enrollmentID string) error {
	return errNotImplemented
}

// Budget reports how many attempts a device has left and how many requests it
// still has in flight.
func (g *EnrollmentGate) Budget(deviceID string) (Remaining, error) {
	return Remaining{}, errNotImplemented
}

// Issue completes the asynchronous issuance of one queued enrollment.
func (g *EnrollmentGate) Issue(enrollmentID string) error {
	return errNotImplemented
}

// RotateCA moves the gate to the next CA generation.
func (g *EnrollmentGate) RotateCA() error {
	return errNotImplemented
}

// CAEpoch reports the current CA generation.
func (g *EnrollmentGate) CAEpoch() int {
	return 0
}

// Pending counts the queued enrollments of one tenant.
func (g *EnrollmentGate) Pending(tenantID string) int {
	return 0
}

// Sweep drops enrollments that sat in the queue longer than MaxPendingAge and
// returns how many were retired.
func (g *EnrollmentGate) Sweep(now time.Time) int {
	return 0
}

// Snapshot serialises the token ledger, the queue and the device budgets.
func (g *EnrollmentGate) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole gate state with a snapshot, all or nothing.
func (g *EnrollmentGate) Restore(data []byte) error {
	return errNotImplemented
}
