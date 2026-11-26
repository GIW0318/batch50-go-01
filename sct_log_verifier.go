// Package task verifies the Signed Certificate Timestamps attached to a TLS
// certificate against an approved log list, a merge delay budget and a
// toleranced clock window.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const snapshotVersion = 1

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
	MaxSctsPerCert   int   `json:"max_scts_per_cert"`
	MaxMergeDelayMs  int64 `json:"max_merge_delay_ms"`
	SkewToleranceMs  int64 `json:"skew_tolerance_ms"`
	MaxRejectReasons int   `json:"max_reject_reasons"`
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
	mu     sync.RWMutex
	policy VerifierPolicy
	logs   map[string]LogPolicy
	stats  VerifierStats
}

type verifierSnapshot struct {
	Version int            `json:"version"`
	Policy  VerifierPolicy `json:"policy"`
	Logs    []LogPolicy    `json:"logs"`
	Stats   VerifierStats  `json:"stats"`
}

// NewSctLogVerifier builds a verifier and fills the unset policy defaults.
func NewSctLogVerifier(policy VerifierPolicy) *SctLogVerifier {
	return &SctLogVerifier{
		policy: normalizePolicy(policy),
		logs:   make(map[string]LogPolicy),
	}
}

// ApproveLog registers a log, or re-registers an identical entry.
func (v *SctLogVerifier) ApproveLog(p LogPolicy) error {
	if strings.TrimSpace(p.LogID) == "" {
		return ErrBlankLogID
	}
	if !validLogState(p.State) {
		return ErrBadLogState
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	existing, ok := v.logs[p.LogID]
	if ok {
		if existing.State != p.State || existing.MaxMergeDelayMs != p.MaxMergeDelayMs {
			return ErrLogConflict
		}
		return nil
	}

	v.logs[p.LogID] = p
	return nil
}

// RevokeLog fences one log off and keeps the first revocation watermark.
func (v *SctLogVerifier) RevokeLog(logID string, at int64) error {
	if strings.TrimSpace(logID) == "" {
		return ErrBlankLogID
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	log, ok := v.logs[logID]
	if !ok {
		return ErrUnknownLog
	}
	if log.State == LogRevoked {
		return nil
	}

	log.State = LogRevoked
	log.RevokedAt = at
	v.logs[logID] = log
	return nil
}

// Log reads one approved entry.
func (v *SctLogVerifier) Log(logID string) (LogPolicy, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	log, ok := v.logs[logID]
	return log, ok
}

// Verify grades every SCT carried by the certificate and decides the leaf.
func (v *SctLogVerifier) Verify(cert Certificate, at int64) (Decision, error) {
	if strings.TrimSpace(cert.Serial) == "" {
		return Decision{}, ErrBlankSerial
	}
	if cert.NotAfter <= cert.NotBefore {
		return Decision{}, ErrInvalidValidity
	}

	policy, logs := v.consistentState()
	if len(cert.Scts) > policy.MaxSctsPerCert {
		return Decision{}, ErrTooManyScts
	}

	decision := Decision{
		Checked:  len(cert.Scts),
		Rejected: make(map[string]string),
	}

	for _, sct := range cert.Scts {
		result := gradeSct(cert, sct, at, logs, policy)
		if result.Verdict == VerdictAllowed {
			decision.Valid++
			if decision.UsedLog == "" {
				decision.UsedLog = result.LogID
			}
			continue
		}
		addRejection(&decision, result, policy.MaxRejectReasons)
	}

	if decision.Valid > 0 {
		decision.Allowed = true
		decision.Verdict = VerdictAllowed
		decision.Reason = ReasonSctValid
	} else {
		decision.Allowed = false
		decision.Verdict = VerdictDenied
		decision.Reason = ReasonNoValidSct
	}

	v.recordVerdict(decision.Allowed)
	return decision, nil
}

// VerifySct grades a single SCT against the certificate envelope.
func (v *SctLogVerifier) VerifySct(cert Certificate, sct SctEntry, at int64) (SctResult, error) {
	if strings.TrimSpace(cert.Serial) == "" {
		return SctResult{}, ErrBlankSerial
	}
	if cert.NotAfter <= cert.NotBefore {
		return SctResult{}, ErrInvalidValidity
	}

	policy, logs := v.consistentState()
	return gradeSct(cert, sct, at, logs, policy), nil
}

// MergeDelay reports how late the SCT arrived relative to the certificate.
func (v *SctLogVerifier) MergeDelay(cert Certificate, sct SctEntry, log LogPolicy) (int64, error) {
	v.mu.RLock()
	policy := normalizePolicy(v.policy)
	v.mu.RUnlock()

	return measureMergeDelay(cert, sct, log, policy)
}

// ApprovedLogs lists the registered log ids in ascending order.
func (v *SctLogVerifier) ApprovedLogs() []string {
	v.mu.RLock()
	ids := make([]string, 0, len(v.logs))
	for logID := range v.logs {
		ids = append(ids, logID)
	}
	v.mu.RUnlock()

	sort.Strings(ids)
	return ids
}

// Stats returns the verification counters.
func (v *SctLogVerifier) Stats() VerifierStats {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.stats
}

// Snapshot serialises the log list, the watermarks and the counters.
func (v *SctLogVerifier) Snapshot() ([]byte, error) {
	v.mu.RLock()
	payload := verifierSnapshot{
		Version: snapshotVersion,
		Policy:  v.policy,
		Logs:    make([]LogPolicy, 0, len(v.logs)),
		Stats:   v.stats,
	}
	for _, log := range v.logs {
		payload.Logs = append(payload.Logs, log)
	}
	v.mu.RUnlock()

	sort.Slice(payload.Logs, func(i, j int) bool {
		return payload.Logs[i].LogID < payload.Logs[j].LogID
	})

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *SctLogVerifier) Restore(data []byte) error {
	payload, logs, policy, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	v.mu.Lock()
	v.policy = policy
	v.logs = logs
	v.stats = payload.Stats
	v.mu.Unlock()
	return nil
}

func normalizePolicy(policy VerifierPolicy) VerifierPolicy {
	if policy.MaxSctsPerCert <= 0 {
		policy.MaxSctsPerCert = DefaultMaxSctsPerCert
	}
	if policy.MaxMergeDelayMs <= 0 {
		policy.MaxMergeDelayMs = DefaultMaxMergeDelayMs
	}
	if policy.MaxRejectReasons <= 0 {
		policy.MaxRejectReasons = DefaultMaxRejectReasons
	}
	if policy.SkewToleranceMs < 0 {
		policy.SkewToleranceMs = 0
	}
	return policy
}

func validLogState(state string) bool {
	return state == LogActive || state == LogRetired || state == LogRevoked
}

func (v *SctLogVerifier) consistentState() (VerifierPolicy, map[string]LogPolicy) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	policy := normalizePolicy(v.policy)
	logs := make(map[string]LogPolicy, len(v.logs))
	for logID, log := range v.logs {
		logs[logID] = log
	}
	return policy, logs
}

func (v *SctLogVerifier) recordVerdict(allowed bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.stats.Verified++
	if allowed {
		v.stats.Allowed++
	} else {
		v.stats.Denied++
	}
}

func gradeSct(
	cert Certificate,
	sct SctEntry,
	at int64,
	logs map[string]LogPolicy,
	policy VerifierPolicy,
) SctResult {
	result := SctResult{
		LogID:        sct.LogID,
		Verdict:      VerdictDenied,
		MergeDelayMs: sct.Timestamp - cert.NotBefore,
	}

	if !validSignature(sct.Signature) {
		result.Reason = ReasonBadSignature
		return result
	}
	if !validHashAlg(sct.HashAlg) {
		result.Reason = ReasonUnsupportedHash
		return result
	}

	log, ok := logs[sct.LogID]
	if !ok {
		result.Reason = ReasonUnknownLog
		return result
	}
	if log.State == LogRetired {
		result.Reason = ReasonLogRetired
		return result
	}
	if log.State == LogRevoked && sct.Timestamp >= log.RevokedAt {
		result.Reason = ReasonLogRevoked
		return result
	}

	if sct.Timestamp < cert.NotBefore-policy.SkewToleranceMs {
		result.Reason = ReasonOutsideValidity
		return result
	}
	if sct.Timestamp > cert.NotAfter {
		result.Reason = ReasonOutsideValidity
		return result
	}
	if sct.Timestamp > at+policy.SkewToleranceMs {
		result.Reason = ReasonTimestampSkew
		return result
	}

	delay, err := measureMergeDelay(cert, sct, log, policy)
	result.MergeDelayMs = delay
	if errors.Is(err, ErrTimestampSkew) {
		result.Reason = ReasonTimestampSkew
		return result
	}
	if errors.Is(err, ErrMergeDelay) {
		result.Reason = ReasonMergeDelayExceeded
		return result
	}
	if err != nil {
		result.Reason = ReasonNoValidSct
		return result
	}

	result.Verdict = VerdictAllowed
	result.Reason = ReasonSctValid
	return result
}

func validSignature(signature string) bool {
	return strings.HasPrefix(signature, "sct-sig:") && len(signature) >= 20
}

func validHashAlg(hashAlg string) bool {
	return hashAlg == "sha256" || hashAlg == "sha384"
}

func measureMergeDelay(
	cert Certificate,
	sct SctEntry,
	log LogPolicy,
	policy VerifierPolicy,
) (int64, error) {
	delay := sct.Timestamp - cert.NotBefore
	if delay < -policy.SkewToleranceMs {
		return delay, ErrTimestampSkew
	}

	limit := log.MaxMergeDelayMs
	if limit <= 0 {
		limit = policy.MaxMergeDelayMs
	}
	if delay > limit {
		return delay, ErrMergeDelay
	}
	return delay, nil
}

func addRejection(decision *Decision, result SctResult, limit int) {
	if _, exists := decision.Rejected[result.LogID]; !exists && len(decision.Rejected) >= limit {
		return
	}
	decision.Rejected[result.LogID] = result.Reason
}

func decodeSnapshot(data []byte) (verifierSnapshot, map[string]LogPolicy, VerifierPolicy, error) {
	var payload verifierSnapshot
	if len(data) == 0 {
		return payload, nil, VerifierPolicy{}, fmt.Errorf("%w: empty snapshot", ErrBadSnapshot)
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, nil, VerifierPolicy{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if payload.Version != snapshotVersion {
		return payload, nil, VerifierPolicy{}, fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if payload.Stats.Verified < 0 || payload.Stats.Allowed < 0 || payload.Stats.Denied < 0 {
		return payload, nil, VerifierPolicy{}, fmt.Errorf("%w: negative counters", ErrBadSnapshot)
	}

	logs := make(map[string]LogPolicy, len(payload.Logs))
	for _, log := range payload.Logs {
		if strings.TrimSpace(log.LogID) == "" || !validLogState(log.State) {
			return payload, nil, VerifierPolicy{}, fmt.Errorf("%w: invalid log policy", ErrBadSnapshot)
		}
		if _, exists := logs[log.LogID]; exists {
			return payload, nil, VerifierPolicy{}, fmt.Errorf("%w: duplicate log", ErrBadSnapshot)
		}
		logs[log.LogID] = log
	}

	policy := normalizePolicy(payload.Policy)
	return payload, logs, policy, nil
}
