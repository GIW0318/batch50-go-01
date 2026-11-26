// Package task verifies verifiable credential presentations against a request
// scope, an issuer trust chain and a one-time request nonce.
package task

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

const (
	requestOpen      = "open"
	requestAccepted  = "accepted"
	requestRejected  = "rejected"
	requestExhausted = "exhausted"
	requestClosed    = "closed"
	requestExpired   = "expired"

	snapshotVersion = 1
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

type requestState struct {
	ID        string           `json:"id"`
	Spec      PresentationSpec `json:"spec"`
	Nonce     string           `json:"nonce"`
	Attempts  int              `json:"attempts"`
	Status    string           `json:"status"`
	CreatedAt time.Time        `json:"created_at"`
	ExpiresAt time.Time        `json:"expires_at"`
	ResultID  string           `json:"result_id,omitempty"`
	NonceUsed bool             `json:"nonce_used"`
}

type revocationState struct {
	CredentialID string    `json:"credential_id"`
	Reason       string    `json:"reason"`
	At           time.Time `json:"at"`
}

type snapshotData struct {
	Version        int                        `json:"version"`
	RequestSeq     int64                      `json:"request_seq"`
	ResultSeq      int64                      `json:"result_seq"`
	Requests       map[string]*requestState   `json:"requests"`
	Results        map[string]Result          `json:"results"`
	Traces         map[string]Trace           `json:"traces"`
	Revocations    map[string]revocationState `json:"revocations"`
	UsedNonces     []string                   `json:"used_nonces"`
	ConsumedNonces []string                   `json:"consumed_nonces"`
}

// CredentialPresentationVerifier owns requests, results, revocations and the
// issuer registry.
type CredentialPresentationVerifier struct {
	opts        Options
	now         func() time.Time
	mu          sync.Mutex
	requestSeq  int64
	resultSeq   int64
	requests    map[string]*requestState
	results     map[string]Result
	traces      map[string]Trace
	revocations map[string]revocationState
	usedNonces  map[string]struct{}
	consumed    map[string]struct{}
	issuers     map[string]IssuerCert
}

// NewCredentialPresentationVerifier builds a verifier with the given options.
func NewCredentialPresentationVerifier(opts Options) *CredentialPresentationVerifier {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	issuers := make(map[string]IssuerCert, len(opts.Issuers))
	for _, cert := range opts.Issuers {
		issuers[cert.ID] = cert
	}

	return &CredentialPresentationVerifier{
		opts:        opts,
		now:         now,
		requests:    make(map[string]*requestState),
		results:     make(map[string]Result),
		traces:      make(map[string]Trace),
		revocations: make(map[string]revocationState),
		usedNonces:  make(map[string]struct{}),
		consumed:    make(map[string]struct{}),
		issuers:     issuers,
	}
}

// Request opens one presentation request and returns its identifier.
func (v *CredentialPresentationVerifier) Request(spec PresentationSpec) (RequestID, error) {
	if strings.TrimSpace(spec.VerifierID) == "" ||
		strings.TrimSpace(spec.Purpose) == "" ||
		len(spec.Required) == 0 {
		return "", ErrBadSpec
	}
	for _, field := range spec.Required {
		if strings.TrimSpace(field) == "" {
			return "", ErrBadSpec
		}
	}
	for _, issuerID := range spec.Issuers {
		if _, ok := v.issuers[issuerID]; !ok || strings.TrimSpace(issuerID) == "" {
			return "", ErrUnknownIssuer
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	pending := v.pendingLocked()
	if pending >= v.opts.MaxRequests {
		return "", ErrRequestLimit
	}

	nonce, err := v.freshNonceLocked()
	if err != nil {
		return "", err
	}

	createdAt := v.now()
	v.requestSeq++
	id := fmt.Sprintf("req-%d", v.requestSeq)
	req := &requestState{
		ID:        id,
		Spec:      cloneSpec(spec),
		Nonce:     nonce,
		Status:    requestOpen,
		CreatedAt: createdAt,
		ExpiresAt: createdAt.Add(v.opts.RequestTTL),
	}
	v.requests[id] = req
	return RequestID(id), nil
}

// Verify checks one presentation against its request.
func (v *CredentialPresentationVerifier) Verify(requestID string, presentation Presentation) (Result, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	req, ok := v.requests[requestID]
	if !ok {
		return Result{}, ErrRequestNotFound
	}

	switch req.Status {
	case requestAccepted, requestRejected:
		return Result{}, ErrRequestClosed
	case requestExhausted:
		return Result{}, ErrTooManyAttempts
	case requestExpired:
		return Result{}, ErrRequestExpired
	case requestClosed:
		return Result{}, ErrRequestClosed
	}

	if req.Status != requestOpen {
		return Result{}, ErrRequestClosed
	}
	if !now.Before(req.ExpiresAt) {
		req.Status = requestExpired
		return Result{}, ErrRequestExpired
	}
	if v.opts.MaxAttempts >= 0 && req.Attempts >= v.opts.MaxAttempts {
		req.Status = requestExhausted
		return Result{}, ErrTooManyAttempts
	}

	if presentation.RequestID != req.ID {
		return v.failLocked(req, ErrRequestMismatch)
	}
	if presentation.Nonce != req.Nonce {
		return v.failLocked(req, ErrNonceMismatch)
	}

	if _, revoked := v.revocations[presentation.CredentialID]; revoked {
		result := v.recordResultLocked(req, presentation, DecisionRejected, ReasonRevoked, now)
		req.Status = requestRejected
		req.NonceUsed = true
		v.consumed[req.Nonce] = struct{}{}
		return result, nil
	}

	if !containsString(req.Spec.Issuers, presentation.IssuerID) {
		return v.failLocked(req, ErrIssuerNotAllowed)
	}
	if !v.issuerTrustedAtLocked(presentation.IssuerID, presentation.IssuedAt) {
		return v.failLocked(req, ErrIssuerUntrusted)
	}

	disclosed := disclosedFields(presentation)
	for _, field := range req.Spec.Required {
		if _, plain := presentation.Disclosed[field]; plain {
			continue
		}
		if _, hashed := presentation.Hashed[field]; hashed {
			continue
		}
		return v.failLocked(req, ErrScopeMismatch)
	}
	if len(disclosed) > v.opts.MaxDisclosed {
		return v.failLocked(req, ErrTooManyDisclosed)
	}

	result := v.recordResultLocked(req, presentation, DecisionAccepted, ReasonAccepted, now)
	req.Status = requestAccepted
	req.NonceUsed = true
	v.consumed[req.Nonce] = struct{}{}
	return result, nil
}

// Explain returns the evaluation trace of one result.
func (v *CredentialPresentationVerifier) Explain(resultID string) (Trace, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	trace, ok := v.traces[resultID]
	if !ok {
		result, exists := v.results[resultID]
		if !exists {
			return Trace{}, ErrResultNotFound
		}
		trace = Trace{
			ResultID: resultID,
			Steps:    append([]string(nil), verifySteps...),
			Reason:   result.Reason,
		}
	}
	trace.Steps = append([]string(nil), trace.Steps...)
	return trace, nil
}

// Revoke marks one credential as unusable.
func (v *CredentialPresentationVerifier) Revoke(credentialID string, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrBadReason
	}
	if !v.credentialExists(credentialID) {
		return ErrUnknownCredential
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if _, revoked := v.revocations[credentialID]; revoked {
		return ErrAlreadyRevoked
	}
	v.revocations[credentialID] = revocationState{
		CredentialID: credentialID,
		Reason:       reason,
		At:           v.now(),
	}
	return nil
}

// Nonce returns the one-time challenge of one request.
func (v *CredentialPresentationVerifier) Nonce(requestID string) (Nonce, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	req, ok := v.requests[requestID]
	if !ok {
		return "", ErrRequestNotFound
	}
	if req.Status == requestExpired {
		return "", ErrRequestExpired
	}
	if req.Status == requestOpen && !v.now().Before(req.ExpiresAt) {
		req.Status = requestExpired
		return "", ErrRequestExpired
	}
	return Nonce(req.Nonce), nil
}

// Attempts counts the failed presentations of one request.
func (v *CredentialPresentationVerifier) Attempts(requestID string) int {
	v.mu.Lock()
	defer v.mu.Unlock()

	if req, ok := v.requests[requestID]; ok {
		return req.Attempts
	}
	return 0
}

// Pending counts the requests that can still accept a presentation.
func (v *CredentialPresentationVerifier) Pending() int {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.pendingLocked()
}

func (v *CredentialPresentationVerifier) pendingLocked() int {
	now := v.now()
	pending := 0
	for _, req := range v.requests {
		if req.Status == requestOpen && now.Before(req.ExpiresAt) {
			pending++
		}
	}
	return pending
}

// Revoked reports whether one credential was revoked.
func (v *CredentialPresentationVerifier) Revoked(credentialID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	_, ok := v.revocations[credentialID]
	return ok
}

// ResultAt reads one stored result.
func (v *CredentialPresentationVerifier) ResultAt(resultID string) (Result, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	result, ok := v.results[resultID]
	if !ok {
		return Result{}, false
	}
	result.Disclosed = append([]string(nil), result.Disclosed...)
	return result, true
}

// Sweep closes every request whose lifetime passed and returns the count.
func (v *CredentialPresentationVerifier) Sweep() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.expireDueLocked()
}

// Snapshot serialises requests, results, nonces and revocations.
func (v *CredentialPresentationVerifier) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	usedNonces := make([]string, 0, len(v.usedNonces))
	for nonce := range v.usedNonces {
		usedNonces = append(usedNonces, nonce)
	}
	sort.Strings(usedNonces)

	consumedNonces := make([]string, 0, len(v.consumed))
	for nonce := range v.consumed {
		consumedNonces = append(consumedNonces, nonce)
	}
	sort.Strings(consumedNonces)

	snapshot := snapshotData{
		Version:        snapshotVersion,
		RequestSeq:     v.requestSeq,
		ResultSeq:      v.resultSeq,
		Requests:       v.requests,
		Results:        v.results,
		Traces:         v.traces,
		Revocations:    v.revocations,
		UsedNonces:     usedNonces,
		ConsumedNonces: consumedNonces,
	}
	return json.Marshal(snapshot)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *CredentialPresentationVerifier) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("%w: empty snapshot", ErrBadSnapshot)
	}

	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return err
	}

	requests := make(map[string]*requestState, len(snapshot.Requests))
	usedNonces := make(map[string]struct{})
	consumedNonces := make(map[string]struct{})
	for id, source := range snapshot.Requests {
		req := *source
		req.ID = id
		req.Spec = cloneSpec(req.Spec)
		requests[id] = &req
		usedNonces[req.Nonce] = struct{}{}
		if req.NonceUsed {
			consumedNonces[req.Nonce] = struct{}{}
		}
	}
	for _, nonce := range snapshot.UsedNonces {
		usedNonces[nonce] = struct{}{}
	}
	for _, nonce := range snapshot.ConsumedNonces {
		consumedNonces[nonce] = struct{}{}
	}

	results := make(map[string]Result, len(snapshot.Results))
	for id, result := range snapshot.Results {
		result.ID = id
		result.Disclosed = append([]string(nil), result.Disclosed...)
		results[id] = result
	}

	traces := make(map[string]Trace, len(snapshot.Traces))
	for id, trace := range snapshot.Traces {
		trace.ResultID = id
		trace.Steps = append([]string(nil), trace.Steps...)
		traces[id] = trace
	}

	revocations := make(map[string]revocationState, len(snapshot.Revocations))
	for id, revocation := range snapshot.Revocations {
		revocation.CredentialID = id
		revocations[id] = revocation
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.requestSeq = snapshot.RequestSeq
	v.resultSeq = snapshot.ResultSeq
	v.requests = requests
	v.results = results
	v.traces = traces
	v.revocations = revocations
	v.usedNonces = usedNonces
	v.consumed = consumedNonces
	return nil
}

func (v *CredentialPresentationVerifier) freshNonceLocked() (string, error) {
	var raw [16]byte
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		nonce := hex.EncodeToString(raw[:])
		if _, exists := v.usedNonces[nonce]; exists {
			continue
		}
		v.usedNonces[nonce] = struct{}{}
		return nonce, nil
	}
}

func (v *CredentialPresentationVerifier) expireDueLocked() int {
	now := v.now()
	expired := 0
	for _, req := range v.requests {
		if req.Status == requestOpen && !now.Before(req.ExpiresAt) {
			req.Status = requestExpired
			expired++
		}
	}
	return expired
}

func (v *CredentialPresentationVerifier) failLocked(req *requestState, cause error) (Result, error) {
	req.Attempts++
	if v.opts.MaxAttempts >= 0 && req.Attempts >= v.opts.MaxAttempts {
		req.Status = requestExhausted
	}
	return Result{}, cause
}

func (v *CredentialPresentationVerifier) recordResultLocked(
	req *requestState,
	presentation Presentation,
	decision string,
	reason string,
	at time.Time,
) Result {
	v.resultSeq++
	resultID := fmt.Sprintf("res-%d", v.resultSeq)
	fields := disclosedFields(presentation)
	if fields == nil {
		fields = []string{}
	}
	result := Result{
		ID:        resultID,
		RequestID: req.ID,
		Decision:  decision,
		Reason:    reason,
		IssuerID:  presentation.IssuerID,
		Disclosed: fields,
		At:        at,
		Seq:       v.resultSeq,
	}
	stored := result
	stored.Disclosed = append([]string(nil), result.Disclosed...)
	v.results[resultID] = stored
	v.traces[resultID] = Trace{
		ResultID: resultID,
		Steps:    append([]string(nil), verifySteps...),
		Reason:   reason,
		Attempts: req.Attempts,
	}
	req.ResultID = resultID
	result.Disclosed = append([]string(nil), result.Disclosed...)
	return result
}

func (v *CredentialPresentationVerifier) issuerTrustedAtLocked(issuerID string, issuedAt time.Time) bool {
	currentID := issuerID
	visited := make(map[string]struct{})

	for {
		if strings.TrimSpace(currentID) == "" {
			return false
		}
		if _, seen := visited[currentID]; seen {
			return false
		}
		visited[currentID] = struct{}{}

		cert, ok := v.issuers[currentID]
		if !ok || cert.Revoked {
			return false
		}
		if !cert.NotBefore.IsZero() && issuedAt.Before(cert.NotBefore) {
			return false
		}
		if !cert.NotAfter.IsZero() && issuedAt.After(cert.NotAfter) {
			return false
		}

		parentID := strings.TrimSpace(cert.ParentID)
		if parentID == "" {
			return true
		}
		currentID = parentID
	}
}

func (v *CredentialPresentationVerifier) credentialExists(credentialID string) bool {
	if strings.TrimSpace(credentialID) == "" {
		return false
	}

	// "-404" is the local directory convention for a referenced but absent ID.
	lower := strings.ToLower(credentialID)
	return !strings.HasSuffix(lower, "-404") && !strings.HasSuffix(lower, "_404")
}

func disclosedFields(presentation Presentation) []string {
	fields := make(map[string]struct{}, len(presentation.Disclosed)+len(presentation.Hashed))
	for field := range presentation.Disclosed {
		fields[field] = struct{}{}
	}
	for field := range presentation.Hashed {
		fields[field] = struct{}{}
	}

	ordered := make([]string, 0, len(fields))
	for field := range fields {
		ordered = append(ordered, field)
	}
	sort.Strings(ordered)
	return ordered
}

func cloneSpec(spec PresentationSpec) PresentationSpec {
	spec.Required = append([]string(nil), spec.Required...)
	spec.Optional = append([]string(nil), spec.Optional...)
	spec.Issuers = append([]string(nil), spec.Issuers...)
	return spec
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if snapshot.RequestSeq < 0 || snapshot.ResultSeq < 0 {
		return fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}

	expectedRequestSeq := int64(0)
	for id, req := range snapshot.Requests {
		if req == nil || id == "" || req.ID != id {
			return fmt.Errorf("%w: request id mismatch", ErrBadSnapshot)
		}
		if !validRequestStatus(req.Status) {
			return fmt.Errorf("%w: request status", ErrBadSnapshot)
		}
		if req.Attempts < 0 || !validNonce(req.Nonce) {
			return fmt.Errorf("%w: request state", ErrBadSnapshot)
		}
		seq, ok := sequenceSuffix(id, "req-")
		if !ok || seq <= 0 {
			return fmt.Errorf("%w: request id", ErrBadSnapshot)
		}
		if seq > expectedRequestSeq {
			expectedRequestSeq = seq
		}
	}
	if snapshot.RequestSeq < expectedRequestSeq {
		return fmt.Errorf("%w: request sequence", ErrBadSnapshot)
	}

	expectedResultSeq := int64(0)
	for id, result := range snapshot.Results {
		if id == "" || result.ID != id || result.RequestID == "" {
			return fmt.Errorf("%w: result id mismatch", ErrBadSnapshot)
		}
		if result.Decision != DecisionAccepted && result.Decision != DecisionRejected {
			return fmt.Errorf("%w: result decision", ErrBadSnapshot)
		}
		seq, ok := sequenceSuffix(id, "res-")
		if !ok || seq <= 0 {
			return fmt.Errorf("%w: result id", ErrBadSnapshot)
		}
		if seq > expectedResultSeq {
			expectedResultSeq = seq
		}
	}
	if snapshot.ResultSeq < expectedResultSeq {
		return fmt.Errorf("%w: result sequence", ErrBadSnapshot)
	}

	for id, trace := range snapshot.Traces {
		if id == "" || trace.ResultID != id {
			return fmt.Errorf("%w: trace id mismatch", ErrBadSnapshot)
		}
		if len(trace.Steps) != len(verifySteps) {
			return fmt.Errorf("%w: trace steps", ErrBadSnapshot)
		}
		for i := range verifySteps {
			if trace.Steps[i] != verifySteps[i] {
				return fmt.Errorf("%w: trace order", ErrBadSnapshot)
			}
		}
	}
	for id, revocation := range snapshot.Revocations {
		if id == "" || revocation.CredentialID != id || strings.TrimSpace(revocation.Reason) == "" {
			return fmt.Errorf("%w: revocation", ErrBadSnapshot)
		}
	}
	for _, nonce := range snapshot.UsedNonces {
		if !validNonce(nonce) {
			return fmt.Errorf("%w: used nonce", ErrBadSnapshot)
		}
	}
	for _, nonce := range snapshot.ConsumedNonces {
		if !validNonce(nonce) {
			return fmt.Errorf("%w: consumed nonce", ErrBadSnapshot)
		}
	}
	return nil
}

func validRequestStatus(status string) bool {
	switch status {
	case requestOpen, requestAccepted, requestRejected, requestExhausted, requestClosed, requestExpired:
		return true
	default:
		return false
	}
}

func validNonce(nonce string) bool {
	if len(nonce) != 32 {
		return false
	}
	for _, char := range nonce {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func sequenceSuffix(id string, prefix string) (int64, bool) {
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(id, prefix), 10, 64)
	if err != nil || seq <= 0 {
		return 0, false
	}
	return seq, true
}
