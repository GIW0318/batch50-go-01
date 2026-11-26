// Package task implements the device certificate enrollment entry point: one
// time bootstrap credentials, CSR policy checks, an asynchronous issuance queue
// and per device attempt budgets.
package task

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	mu sync.Mutex

	cfg     Config
	caEpoch int

	tokens   map[string]bootstrapToken
	profiles map[string]Profile

	enrollments     map[string]*enrollmentRecord
	pendingByDevice map[string]string
	csrIndex        map[string]string
	keyIndex        map[string]string
	attempts        map[string]int
	serials         map[string]struct{}
}

type bootstrapToken struct {
	TenantID string `json:"tenant_id"`
	Consumed bool   `json:"consumed"`
}

type enrollmentRecord struct {
	ID             string    `json:"id"`
	DeviceID       string    `json:"device_id"`
	TenantID       string    `json:"tenant_id"`
	State          string    `json:"state"`
	Serial         string    `json:"serial,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CSRFingerprint string    `json:"csr_fingerprint"`
	KeyFingerprint string    `json:"key_fingerprint"`
	CAEpoch        int       `json:"ca_epoch"`
	QueuedAt       time.Time `json:"queued_at"`
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type gateSnapshot struct {
	CAEpoch     int                       `json:"ca_epoch"`
	Tokens      map[string]bootstrapToken `json:"tokens"`
	Profiles    map[string]Profile        `json:"profiles"`
	Enrollments []snapshotEnrollment      `json:"enrollments"`
	Attempts    map[string]int            `json:"attempts"`
}

type snapshotEnrollment struct {
	ID             string    `json:"id"`
	DeviceID       string    `json:"device_id"`
	TenantID       string    `json:"tenant_id"`
	State          string    `json:"state"`
	Serial         string    `json:"serial,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CSRFingerprint string    `json:"csr_fingerprint"`
	KeyFingerprint string    `json:"key_fingerprint"`
	CAEpoch        int       `json:"ca_epoch"`
	QueuedAt       time.Time `json:"queued_at"`
}

// NewEnrollmentGate builds a gate with the given caps.
func NewEnrollmentGate(cfg Config) *EnrollmentGate {
	g := &EnrollmentGate{
		cfg:             cfg,
		caEpoch:         1,
		tokens:          make(map[string]bootstrapToken),
		profiles:        make(map[string]Profile),
		enrollments:     make(map[string]*enrollmentRecord),
		pendingByDevice: make(map[string]string),
		csrIndex:        make(map[string]string),
		keyIndex:        make(map[string]string),
		attempts:        make(map[string]int),
		serials:         make(map[string]struct{}),
	}
	g.cfg.DomainSuffix = normaliseDomainSuffix(g.cfg.DomainSuffix)
	return g
}

// RegisterBootstrapToken adds one single use bootstrap token for a tenant.
func (g *EnrollmentGate) RegisterBootstrapToken(token, tenantID string) error {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(tenantID) == "" {
		return ErrBadRequest
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if existing, ok := g.tokens[token]; ok {
		if existing.Consumed || existing.TenantID != tenantID {
			return ErrBootstrapRejected
		}
		return nil
	}
	g.tokens[token] = bootstrapToken{TenantID: tenantID}
	return nil
}

// SetProfile adds or replaces one issuance profile.
func (g *EnrollmentGate) SetProfile(p Profile) error {
	if strings.TrimSpace(p.Name) == "" {
		return ErrBadRequest
	}

	cloned := Profile{
		Name:             p.Name,
		AllowedKeyUsages: append([]string(nil), p.AllowedKeyUsages...),
		AllowedEKUs:      append([]string(nil), p.AllowedEKUs...),
		TTL:              p.TTL,
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.profiles[p.Name] = cloned
	return nil
}

// Enroll validates one request and places it on the issuance queue.
func (g *EnrollmentGate) Enroll(ctx context.Context, req EnrollRequest) (Enrollment, error) {
	if strings.TrimSpace(req.DeviceID) == "" || strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.BootstrapToken) == "" {
		return Enrollment{}, ErrBadRequest
	}
	if ctx == nil {
		return Enrollment{}, ErrBadRequest
	}
	select {
	case <-ctx.Done():
		return Enrollment{}, ctx.Err()
	default:
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-ctx.Done():
		return Enrollment{}, ctx.Err()
	default:
	}

	token, ok := g.tokens[req.BootstrapToken]
	if !ok || token.Consumed || token.TenantID != req.TenantID {
		return Enrollment{}, ErrBootstrapRejected
	}

	// A device owns at most one in-flight CSR. The placeholder lookup and all
	// subsequent work happen while holding the gate lock, so concurrent
	// submissions cannot create two queue entries. A syntactically or
	// policy-invalid resubmission is still rejected with its specific error;
	// only a valid second request is absorbed by the existing enrollment.
	pendingID, hasPending := g.pendingByDevice[req.DeviceID]

	fingerprint := fingerprintCSR(req.CSR)
	if id, ok := g.csrIndex[fingerprint]; ok {
		g.consumeToken(req.BootstrapToken)
		return g.enrollmentView(g.enrollments[id]), nil
	}

	if g.attempts[req.DeviceID] >= g.cfg.MaxAttemptsPerDevice {
		return Enrollment{}, ErrAttemptsExhausted
	}

	profile, err := g.selectProfile(req.Profile)
	if err != nil {
		return g.rejectValidation(req, err)
	}

	parsed, err := parseCSR(req.CSR)
	if err != nil {
		return g.rejectValidation(req, err)
	}
	if err := g.validatePublicKey(parsed.PublicKey); err != nil {
		return g.rejectValidation(req, err)
	}
	if err := g.validateCAFlag(parsed); err != nil {
		return g.rejectValidation(req, err)
	}
	if err := g.validateSANs(parsed.DNSNames); err != nil {
		return g.rejectValidation(req, err)
	}
	if profile != nil {
		if err := validateProfileUsages(profile, parsed); err != nil {
			return g.rejectValidation(req, err)
		}
	}
	if hasPending {
		g.consumeToken(req.BootstrapToken)
		return g.enrollmentView(g.enrollments[pendingID]), nil
	}

	keyFingerprint := fingerprintPublicKey(parsed.RawSubjectPublicKeyInfo)
	if _, ok := g.keyIndex[keyFingerprint]; ok {
		return g.rejectValidation(req, ErrDuplicateEnrollment)
	}

	if g.cfg.MaxQueue > 0 && g.pendingCount(req.TenantID) >= g.cfg.MaxQueue {
		return Enrollment{}, ErrQueueFull
	}

	id, err := randomID("enr-")
	if err != nil {
		return Enrollment{}, err
	}
	now := time.Now()
	if g.cfg.Now != nil {
		now = g.cfg.Now()
	}
	record := &enrollmentRecord{
		ID:             id,
		DeviceID:       req.DeviceID,
		TenantID:       req.TenantID,
		State:          StateQueued,
		CSRFingerprint: fingerprint,
		KeyFingerprint: keyFingerprint,
		CAEpoch:        g.caEpoch,
		QueuedAt:       now,
	}

	g.consumeToken(req.BootstrapToken)
	g.enrollments[id] = record
	g.pendingByDevice[req.DeviceID] = id
	g.csrIndex[fingerprint] = id
	g.keyIndex[keyFingerprint] = id
	return g.enrollmentView(record), nil
}

// Poll reads the state of one enrollment.
func (g *EnrollmentGate) Poll(enrollmentID string) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.enrollments[enrollmentID]
	if !ok {
		return State{}, ErrUnknownEnrollment
	}
	return State{
		ID:       record.ID,
		State:    record.State,
		Attempts: g.attempts[record.DeviceID],
		Serial:   record.Serial,
		Reason:   record.Reason,
	}, nil
}

// Revoke retires one enrollment and its certificate.
func (g *EnrollmentGate) Revoke(enrollmentID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.enrollments[enrollmentID]
	if !ok {
		return ErrUnknownEnrollment
	}
	if record.State == StateRevoked {
		return nil
	}
	record.State = StateRevoked
	if record.Reason == "" {
		record.Reason = "revoked"
	}
	if id, ok := g.pendingByDevice[record.DeviceID]; ok && id == record.ID {
		delete(g.pendingByDevice, record.DeviceID)
	}
	return nil
}

// Budget reports how many attempts a device has left and how many requests it
// still has in flight.
func (g *EnrollmentGate) Budget(deviceID string) (Remaining, error) {
	if strings.TrimSpace(deviceID) == "" {
		return Remaining{}, ErrBadRequest
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	queued := 0
	if id, ok := g.pendingByDevice[deviceID]; ok {
		if record, ok := g.enrollments[id]; ok && record.State == StateQueued {
			queued = 1
		}
	}
	return Remaining{Attempts: g.attempts[deviceID], Queued: queued}, nil
}

// Issue completes the asynchronous issuance of one queued enrollment.
func (g *EnrollmentGate) Issue(enrollmentID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, ok := g.enrollments[enrollmentID]
	if !ok {
		return ErrUnknownEnrollment
	}
	if record.State != StateQueued {
		return nil
	}

	serial, err := g.newSerial()
	if err != nil {
		return err
	}
	record.State = StateIssued
	record.Serial = serial
	delete(g.pendingByDevice, record.DeviceID)
	return nil
}

// RotateCA moves the gate to the next CA generation.
func (g *EnrollmentGate) RotateCA() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.caEpoch++
	return nil
}

// CAEpoch reports the current CA generation.
func (g *EnrollmentGate) CAEpoch() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.caEpoch
}

// Pending counts the queued enrollments of one tenant.
func (g *EnrollmentGate) Pending(tenantID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pendingCount(tenantID)
}

// Sweep drops enrollments that sat in the queue longer than MaxPendingAge and
// returns how many were retired.
func (g *EnrollmentGate) Sweep(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cfg.MaxPendingAge <= 0 {
		return 0
	}
	retired := 0
	for _, record := range g.enrollments {
		if record.State != StateQueued || now.Sub(record.QueuedAt) <= g.cfg.MaxPendingAge {
			continue
		}
		record.State = StateFailed
		record.Reason = "enrollment queue expired"
		if id, ok := g.pendingByDevice[record.DeviceID]; ok && id == record.ID {
			delete(g.pendingByDevice, record.DeviceID)
		}
		if id, ok := g.csrIndex[record.CSRFingerprint]; ok && id == record.ID {
			delete(g.csrIndex, record.CSRFingerprint)
		}
		if id, ok := g.keyIndex[record.KeyFingerprint]; ok && id == record.ID {
			delete(g.keyIndex, record.KeyFingerprint)
		}
		retired++
	}
	return retired
}

// Snapshot serialises the token ledger, the queue and the device budgets.
func (g *EnrollmentGate) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	snap := gateSnapshot{
		CAEpoch:     g.caEpoch,
		Tokens:      make(map[string]bootstrapToken, len(g.tokens)),
		Profiles:    make(map[string]Profile, len(g.profiles)),
		Attempts:    make(map[string]int, len(g.attempts)),
		Enrollments: make([]snapshotEnrollment, 0, len(g.enrollments)),
	}
	for token, state := range g.tokens {
		snap.Tokens[token] = state
	}
	for name, profile := range g.profiles {
		snap.Profiles[name] = Profile{
			Name:             profile.Name,
			AllowedKeyUsages: append([]string(nil), profile.AllowedKeyUsages...),
			AllowedEKUs:      append([]string(nil), profile.AllowedEKUs...),
			TTL:              profile.TTL,
		}
	}
	for device, attempts := range g.attempts {
		snap.Attempts[device] = attempts
	}
	for _, record := range g.enrollments {
		snap.Enrollments = append(snap.Enrollments, snapshotFromRecord(record))
	}

	payload, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	envelope := snapshotEnvelope{
		Version:  1,
		Payload:  payload,
		Checksum: checksum(payload),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole gate state with a snapshot, all or nothing.
func (g *EnrollmentGate) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Version != 1 || envelope.Checksum == "" || len(envelope.Payload) == 0 {
		return ErrBadSnapshot
	}
	if checksum(envelope.Payload) != envelope.Checksum {
		return ErrBadSnapshot
	}

	var snap gateSnapshot
	if err := json.Unmarshal(envelope.Payload, &snap); err != nil {
		return ErrBadSnapshot
	}
	restored, err := buildRestoredGate(g.cfg, snap)
	if err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.caEpoch = restored.caEpoch
	g.tokens = restored.tokens
	g.profiles = restored.profiles
	g.enrollments = restored.enrollments
	g.pendingByDevice = restored.pendingByDevice
	g.csrIndex = restored.csrIndex
	g.keyIndex = restored.keyIndex
	g.attempts = restored.attempts
	g.serials = restored.serials
	return nil
}

func (g *EnrollmentGate) consumeToken(token string) {
	state := g.tokens[token]
	state.Consumed = true
	g.tokens[token] = state
}

func (g *EnrollmentGate) rejectValidation(req EnrollRequest, err error) (Enrollment, error) {
	g.consumeToken(req.BootstrapToken)
	g.attempts[req.DeviceID]++
	return Enrollment{}, err
}

func (g *EnrollmentGate) pendingCount(tenantID string) int {
	count := 0
	for _, record := range g.enrollments {
		if record.State == StateQueued && record.TenantID == tenantID {
			count++
		}
	}
	return count
}

func (g *EnrollmentGate) enrollmentView(record *enrollmentRecord) Enrollment {
	if record == nil {
		return Enrollment{}
	}
	return Enrollment{
		ID:             record.ID,
		DeviceID:       record.DeviceID,
		TenantID:       record.TenantID,
		State:          record.State,
		RetryAfter:     g.cfg.RetryAfter,
		CAEpoch:        record.CAEpoch,
		Serial:         record.Serial,
		CSRFingerprint: record.CSRFingerprint,
	}
}

func (g *EnrollmentGate) selectProfile(name string) (*Profile, error) {
	if name == "" {
		return nil, nil
	}
	profile, ok := g.profiles[name]
	if !ok {
		return nil, ErrUnknownProfile
	}
	return &profile, nil
}

func (g *EnrollmentGate) validatePublicKey(key any) error {
	switch pub := key.(type) {
	case *rsa.PublicKey:
		if pub.N == nil || pub.N.BitLen() < g.cfg.MinRSABits {
			return ErrKeyTooWeak
		}
	case *ecdsa.PublicKey:
		if pub.Curve != elliptic.P256() && pub.Curve != elliptic.P384() {
			return ErrKeyTooWeak
		}
	default:
		return ErrKeyTooWeak
	}
	return nil
}

func (g *EnrollmentGate) validateSANs(dnsNames []string) error {
	if len(dnsNames) == 0 {
		return ErrSANMissing
	}
	for _, rawName := range dnsNames {
		name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rawName)), ".")
		if strings.Contains(name, "*") {
			if !validWildcard(name, g.cfg.DomainSuffix) {
				return ErrWildcardTooBroad
			}
			continue
		}
		if !withinDomain(name, g.cfg.DomainSuffix) {
			return ErrSANOutsideTenant
		}
	}
	return nil
}

func (g *EnrollmentGate) newSerial() (string, error) {
	for i := 0; i < 8; i++ {
		serial, err := randomID("")
		if err != nil {
			return "", err
		}
		serial = strings.ToUpper(serial)
		if _, exists := g.serials[serial]; !exists {
			g.serials[serial] = struct{}{}
			return serial, nil
		}
	}
	return "", errors.New("could not allocate a unique certificate serial")
}

func parseCSR(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		return nil, ErrBadCSR
	}
	return csr, nil
}

func (g *EnrollmentGate) validateCAFlag(csr *x509.CertificateRequest) error {
	const basicConstraintsOID = "2.5.29.19"
	for _, ext := range csr.Extensions {
		if ext.Id.String() != basicConstraintsOID {
			continue
		}
		var constraints struct {
			IsCA       bool `asn1:"optional"`
			MaxPathLen int  `asn1:"optional,default:-1"`
		}
		if _, err := asn1.Unmarshal(ext.Value, &constraints); err != nil {
			return ErrBadCSR
		}
		if constraints.IsCA {
			return ErrCAFlagForbidden
		}
	}
	return nil
}

func validateProfileUsages(profile *Profile, csr *x509.CertificateRequest) error {
	requestedKU, requestedEKUs, err := requestedUsages(csr)
	if err != nil {
		return ErrBadCSR
	}

	allowedKU := x509.KeyUsage(0)
	for _, usage := range profile.AllowedKeyUsages {
		if bit, ok := keyUsageValue(usage); ok {
			allowedKU |= bit
		}
	}
	if requestedKU&^allowedKU != 0 {
		return ErrKeyUsageMismatch
	}

	allowedOIDs := make(map[string]struct{})
	allowAnyEKU := false
	for _, usage := range profile.AllowedEKUs {
		trimmed := strings.TrimSpace(usage)
		if oid, ok := parseOID(trimmed); ok {
			allowedOIDs[oid.String()] = struct{}{}
			if oid.Equal(asn1.ObjectIdentifier{2, 5, 29, 37, 0}) {
				allowAnyEKU = true
			}
			continue
		}
		if oid, ok := extKeyUsageOID(trimmed); ok {
			allowedOIDs[oid.String()] = struct{}{}
			if oid.Equal(asn1.ObjectIdentifier{2, 5, 29, 37, 0}) {
				allowAnyEKU = true
			}
		}
	}
	if allowAnyEKU {
		return nil
	}
	for _, oid := range requestedEKUs {
		if _, ok := allowedOIDs[oid.String()]; !ok {
			return ErrKeyUsageMismatch
		}
	}
	return nil
}

func keyUsageValue(raw string) (x509.KeyUsage, bool) {
	switch normaliseUsageName(raw) {
	case "digitalsignature", "keyusagedigitalsignature":
		return x509.KeyUsageDigitalSignature, true
	case "contentcommitment", "nonrepudiation", "keyusagecontentcommitment", "keyusagenonrepudiation":
		return x509.KeyUsageContentCommitment, true
	case "keyencipherment", "keyusagekeyencipherment":
		return x509.KeyUsageKeyEncipherment, true
	case "dataencipherment", "keyusagedataencipherment":
		return x509.KeyUsageDataEncipherment, true
	case "keyagreement", "keyusagekeyagreement":
		return x509.KeyUsageKeyAgreement, true
	case "certsign", "certificatesign", "keycertsign", "keyusagecertsign":
		return x509.KeyUsageCertSign, true
	case "crlsign", "crlsigning", "keyusagecrlsign":
		return x509.KeyUsageCRLSign, true
	case "encipheronly", "keyusageencipheronly":
		return x509.KeyUsageEncipherOnly, true
	case "decipheronly", "keyusagedecipheronly":
		return x509.KeyUsageDecipherOnly, true
	default:
		return 0, false
	}
}

func extKeyUsageOID(raw string) (asn1.ObjectIdentifier, bool) {
	switch normaliseUsageName(raw) {
	case "any", "anyextendedkeyusage", "extkeyusageany":
		return asn1.ObjectIdentifier{2, 5, 29, 37, 0}, true
	case "serverauth", "tlsserverauth", "servers", "extkeyusageserverauth", "tlsserverauthentication", "tlsserver":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}, true
	case "clientauth", "tlsclientauth", "client", "extkeyusageclientauth", "tlsclientauthentication", "tlsclient":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}, true
	case "codesigning", "extkeyusagecodesigning":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 3}, true
	case "emailprotection", "smime", "extkeyusageemailprotection":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 4}, true
	case "ipsecendsystem", "extkeyusageipsecendsystem":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 5}, true
	case "ipsectunnel", "extkeyusageipsectunnel":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 6}, true
	case "ipsecuser", "extkeyusageipsecuser":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 7}, true
	case "timestamping", "extkeyusagetimestamping":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}, true
	case "ocspsigning", "extkeyusageocspsigning":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 9}, true
	case "microsoftservergatedcrypto", "extkeyusagemicrosoftservergatedcrypto":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 10, 3, 3}, true
	case "netscapeservergatedcrypto", "extkeyusagenetscapeservergatedcrypto":
		return asn1.ObjectIdentifier{2, 16, 840, 1, 113730, 4, 1}, true
	case "microsoftcommercialcodesigning", "extkeyusagemicrosoftcommercialcodesigning":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 22}, true
	case "microsoftkernelcodesigning", "extkeyusagemicrosoftkernelcodesigning":
		return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 61, 1, 1}, true
	default:
		return nil, false
	}
}

func normaliseUsageName(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	return strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(raw)
}

func parseOID(raw string) (asn1.ObjectIdentifier, bool) {
	if raw == "" || !strings.Contains(raw, ".") {
		return nil, false
	}
	parts := strings.Split(raw, ".")
	oid := make(asn1.ObjectIdentifier, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return nil, false
		}
		oid = append(oid, value)
	}
	return oid, true
}

func requestedUsages(csr *x509.CertificateRequest) (x509.KeyUsage, []asn1.ObjectIdentifier, error) {
	var ku x509.KeyUsage
	var ekuOIDs []asn1.ObjectIdentifier
	for _, ext := range csr.Extensions {
		switch ext.Id.String() {
		case "2.5.29.15":
			var bits asn1.BitString
			if _, err := asn1.Unmarshal(ext.Value, &bits); err != nil {
				return 0, nil, err
			}
			for i := 0; i < 9; i++ {
				if bits.At(i) != 0 {
					ku |= 1 << uint(i)
				}
			}
		case "2.5.29.37":
			var oids []asn1.ObjectIdentifier
			if _, err := asn1.Unmarshal(ext.Value, &oids); err != nil {
				return 0, nil, err
			}
			ekuOIDs = append(ekuOIDs, oids...)
		}
	}
	return ku, ekuOIDs, nil
}

func fingerprintCSR(csr []byte) string {
	sum := sha256.Sum256(csr)
	return hex.EncodeToString(sum[:])
}

func fingerprintPublicKey(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func normaliseDomainSuffix(suffix string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(suffix)), ".")
}

func withinDomain(name, suffix string) bool {
	return suffix != "" && name != "" && (name == suffix || strings.HasSuffix(name, "."+suffix))
}

func validWildcard(name, suffix string) bool {
	if !strings.HasPrefix(name, "*.") || strings.Count(name, "*") != 1 {
		return false
	}
	base := strings.TrimPrefix(name, "*.")
	return base != "" && withinDomain(base, suffix)
}

func randomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func checksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func snapshotFromRecord(record *enrollmentRecord) snapshotEnrollment {
	return snapshotEnrollment{
		ID:             record.ID,
		DeviceID:       record.DeviceID,
		TenantID:       record.TenantID,
		State:          record.State,
		Serial:         record.Serial,
		Reason:         record.Reason,
		CSRFingerprint: record.CSRFingerprint,
		KeyFingerprint: record.KeyFingerprint,
		CAEpoch:        record.CAEpoch,
		QueuedAt:       record.QueuedAt,
	}
}

func buildRestoredGate(cfg Config, snap gateSnapshot) (*EnrollmentGate, error) {
	if snap.CAEpoch < 1 {
		return nil, ErrBadSnapshot
	}
	g := &EnrollmentGate{
		cfg:             cfg,
		caEpoch:         snap.CAEpoch,
		tokens:          make(map[string]bootstrapToken, len(snap.Tokens)),
		profiles:        make(map[string]Profile, len(snap.Profiles)),
		enrollments:     make(map[string]*enrollmentRecord, len(snap.Enrollments)),
		pendingByDevice: make(map[string]string),
		csrIndex:        make(map[string]string),
		keyIndex:        make(map[string]string),
		attempts:        make(map[string]int, len(snap.Attempts)),
		serials:         make(map[string]struct{}),
	}
	g.cfg.DomainSuffix = normaliseDomainSuffix(g.cfg.DomainSuffix)

	for token, state := range snap.Tokens {
		if strings.TrimSpace(token) == "" || strings.TrimSpace(state.TenantID) == "" {
			return nil, ErrBadSnapshot
		}
		g.tokens[token] = state
	}
	for name, profile := range snap.Profiles {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(profile.Name) == "" || name != profile.Name {
			return nil, ErrBadSnapshot
		}
		profile.AllowedKeyUsages = append([]string(nil), profile.AllowedKeyUsages...)
		profile.AllowedEKUs = append([]string(nil), profile.AllowedEKUs...)
		g.profiles[name] = profile
	}
	for device, attempts := range snap.Attempts {
		if strings.TrimSpace(device) == "" || attempts < 0 {
			return nil, ErrBadSnapshot
		}
		g.attempts[device] = attempts
	}

	for _, item := range snap.Enrollments {
		if item.ID == "" || strings.TrimSpace(item.DeviceID) == "" || strings.TrimSpace(item.TenantID) == "" || item.CSRFingerprint == "" || item.KeyFingerprint == "" || item.CAEpoch < 1 {
			return nil, ErrBadSnapshot
		}
		if _, exists := g.enrollments[item.ID]; exists {
			return nil, ErrBadSnapshot
		}
		switch item.State {
		case StateQueued, StateIssued, StateFailed, StateRevoked:
		default:
			return nil, ErrBadSnapshot
		}
		if (item.State == StateIssued || item.State == StateRevoked) && item.Serial != "" {
			if _, exists := g.serials[item.Serial]; exists {
				return nil, ErrBadSnapshot
			}
			g.serials[item.Serial] = struct{}{}
		}
		if item.State == StateIssued && item.Serial == "" {
			return nil, ErrBadSnapshot
		}
		record := &enrollmentRecord{
			ID:             item.ID,
			DeviceID:       item.DeviceID,
			TenantID:       item.TenantID,
			State:          item.State,
			Serial:         item.Serial,
			Reason:         item.Reason,
			CSRFingerprint: item.CSRFingerprint,
			KeyFingerprint: item.KeyFingerprint,
			CAEpoch:        item.CAEpoch,
			QueuedAt:       item.QueuedAt,
		}
		g.enrollments[record.ID] = record

		if record.State == StateQueued {
			if _, exists := g.pendingByDevice[record.DeviceID]; exists {
				return nil, ErrBadSnapshot
			}
			g.pendingByDevice[record.DeviceID] = record.ID
		}
		if record.State == StateQueued || record.State == StateIssued || record.State == StateRevoked {
			if _, exists := g.csrIndex[record.CSRFingerprint]; exists {
				return nil, ErrBadSnapshot
			}
			if _, exists := g.keyIndex[record.KeyFingerprint]; exists {
				return nil, ErrBadSnapshot
			}
			g.csrIndex[record.CSRFingerprint] = record.ID
			g.keyIndex[record.KeyFingerprint] = record.ID
		}
	}
	return g, nil
}
