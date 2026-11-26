// Package task verifies the Signed Certificate Timestamps attached to a TLS
// certificate against an approved log list, a merge delay budget and a
// toleranced clock window.
package task

import "errors"

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// States kept in the approved log list.
const (
	LogActive  = "active"
	LogRetired = "retired"
	LogRevoked = "revoked"
)

// Verdicts reported for one certificate.
const (
	VerdictAllowed = "allowed"
	VerdictDenied  = "denied"
)

// Reasons attached to a certificate decision or to a single SCT result.
const (
	ReasonSctValid           = "sct_valid"
	ReasonNoValidSct         = "no_valid_sct"
	ReasonBadSignature       = "bad_signature"
	ReasonUnsupportedHash    = "unsupported_hash"
	ReasonUnknownLog         = "unknown_log"
	ReasonLogRetired         = "log_retired"
	ReasonLogRevoked         = "log_revoked"
	ReasonOutsideValidity    = "outside_validity"
	ReasonTimestampSkew      = "timestamp_skew"
	ReasonMergeDelayExceeded = "merge_delay_exceeded"
)

// Defaults applied when the verifier policy leaves a field unset.
const (
	DefaultMaxSctsPerCert   = 8
	DefaultMaxMergeDelayMs  = 86400000
	DefaultMaxRejectReasons = 16
)

// Errors reported by the verifier.
var (
	ErrBlankSerial     = errors.New("certificate serial is required")
	ErrBlankLogID      = errors.New("log id is required")
	ErrBadLogState     = errors.New("log state is not recognised")
	ErrLogConflict     = errors.New("log policy conflicts with the approved entry")
	ErrUnknownLog      = errors.New("log is not approved")
	ErrInvalidValidity = errors.New("certificate validity window is inverted")
	ErrTooManyScts     = errors.New("too many scts attached to the certificate")
	ErrMergeDelay      = errors.New("sct arrived beyond the merge delay limit")
	ErrTimestampSkew   = errors.New("sct timestamp is farther off than tolerated")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// SctEntry is one Signed Certificate Timestamp carried by the certificate.
type SctEntry struct {
	LogID     string `json:"log_id"`
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`
	HashAlg   string `json:"hash_alg"`
}

// Certificate is the leaf the handshake wants to accept.
type Certificate struct {
	Serial    string     `json:"serial"`
	NotBefore int64      `json:"not_before"`
	NotAfter  int64      `json:"not_after"`
	Scts      []SctEntry `json:"scts"`
}

// LogPolicy is one entry of the approved log list.
type LogPolicy struct {
	LogID           string `json:"log_id"`
	State           string `json:"state"`
	MaxMergeDelayMs int64  `json:"max_merge_delay_ms"`
	RevokedAt       int64  `json:"revoked_at"`
}

// SctResult is the outcome of one SCT judged on its own.
type SctResult struct {
	LogID        string `json:"log_id"`
	Verdict      string `json:"verdict"`
	Reason       string `json:"reason"`
	MergeDelayMs int64  `json:"merge_delay_ms"`
}

// Decision is the aggregate outcome for one certificate.
type Decision struct {
	Allowed  bool              `json:"allowed"`
	Verdict  string            `json:"verdict"`
	Reason   string            `json:"reason"`
	UsedLog  string            `json:"used_log"`
	Valid    int               `json:"valid"`
	Checked  int               `json:"checked"`
	Rejected map[string]string `json:"rejected"`
}

// VerifierPolicy bounds the verifier tables.
type VerifierPolicy struct {
	MaxSctsPerCert   int
	MaxMergeDelayMs  int64
	SkewToleranceMs  int64
	MaxRejectReasons int
}

// VerifierStats counts the traffic seen by the verifier.
type VerifierStats struct {
	Verified int `json:"verified"`
	Allowed  int `json:"allowed"`
	Denied   int `json:"denied"`
}

// SctLogVerifier owns the approved list, the revocation watermarks and the
// per certificate decision path.
type SctLogVerifier struct {
	policy VerifierPolicy
}

// NewSctLogVerifier builds a verifier and fills the unset policy defaults.
func NewSctLogVerifier(policy VerifierPolicy) *SctLogVerifier {
	return &SctLogVerifier{policy: policy}
}

// ApproveLog registers a log, or re-registers an identical entry.
func (v *SctLogVerifier) ApproveLog(p LogPolicy) error {
	return errNotImplemented
}

// RevokeLog fences one log off and keeps the first revocation watermark.
func (v *SctLogVerifier) RevokeLog(logID string, at int64) error {
	return errNotImplemented
}

// Log reads one approved entry.
func (v *SctLogVerifier) Log(logID string) (LogPolicy, bool) {
	return LogPolicy{}, false
}

// Verify grades every SCT carried by the certificate and decides the leaf.
func (v *SctLogVerifier) Verify(cert Certificate, at int64) (Decision, error) {
	return Decision{}, errNotImplemented
}

// VerifySct grades a single SCT against the certificate envelope.
func (v *SctLogVerifier) VerifySct(cert Certificate, sct SctEntry, at int64) (SctResult, error) {
	return SctResult{}, errNotImplemented
}

// MergeDelay reports how late the SCT arrived relative to the certificate.
func (v *SctLogVerifier) MergeDelay(cert Certificate, sct SctEntry, log LogPolicy) (int64, error) {
	return 0, errNotImplemented
}

// ApprovedLogs lists the registered log ids in ascending order.
func (v *SctLogVerifier) ApprovedLogs() []string {
	return nil
}

// Stats returns the verification counters.
func (v *SctLogVerifier) Stats() VerifierStats {
	return VerifierStats{}
}

// Snapshot serialises the log list, the watermarks and the counters.
func (v *SctLogVerifier) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *SctLogVerifier) Restore(data []byte) error {
	return errNotImplemented
}
