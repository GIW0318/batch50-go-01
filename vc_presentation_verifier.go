// Package task implements the verifier side of an OpenID4VP exchange: request
// creation, presentation definition matching, holder binding checks and
// credential status list validation.
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
	ReasonIATInvalid        = "iat_outside_validity"
	ReasonStatusUnavailable = "status_unavailable"
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

const snapshotVersion = 1

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

type keyBindingDocument struct {
	Aud   string `json:"aud"`
	Nonce string `json:"nonce"`
	Iat   int64  `json:"iat"`
	Iss   string `json:"iss"`
}

type statusReference struct {
	URI   string `json:"uri"`
	Index int    `json:"index"`
	Epoch int64  `json:"epoch"`
}

type credentialDocument struct {
	ID     string              `json:"id"`
	Format string              `json:"format"`
	Holder string              `json:"holder"`
	Claims map[string]any      `json:"claims"`
	Status *statusReference    `json:"status,omitempty"`
	KB     *keyBindingDocument `json:"kb,omitempty"`
}

type vpTokenDocument struct {
	Credentials []credentialDocument `json:"credentials"`
}

type descriptorMapEntry struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Format string `json:"format,omitempty"`
}

type submissionDocument struct {
	DescriptorMap []descriptorMapEntry `json:"descriptor_map"`
}

type requestRecord struct {
	Request
	Sequence   uint64                 `json:"sequence"`
	CreatedAt  time.Time              `json:"created_at"`
	Definition PresentationDefinition `json:"definition"`
	Status     string                 `json:"status"`
	Submitted  int                    `json:"submitted"`
	Matched    []string               `json:"matched,omitempty"`
	Subject    string                 `json:"subject,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
}

type snapshotDocument struct {
	Version      int                       `json:"version"`
	NextSequence uint64                    `json:"next_sequence"`
	Requests     map[string]*requestRecord `json:"requests"`
	StatusLists  map[string]StatusList     `json:"status_lists"`
}

// PresentationVerifier owns the request table, the descriptor state and the
// status list cache.
type PresentationVerifier struct {
	cfg          Config
	mu           sync.Mutex
	nextSequence uint64
	requests     map[string]*requestRecord
	statusLists  map[string]StatusList
}

// NewPresentationVerifier builds a verifier with the given caps.
func NewPresentationVerifier(cfg Config) *PresentationVerifier {
	cfg.Formats = cloneStrings(cfg.Formats)
	return &PresentationVerifier{
		cfg:         cfg,
		requests:    make(map[string]*requestRecord),
		statusLists: make(map[string]StatusList),
	}
}

// CreateRequest records a new request and returns the nonce to hand to the
// wallet.
func (v *PresentationVerifier) CreateRequest(tenantID string, def PresentationDefinition) (Request, error) {
	def = cloneDefinition(def)
	if err := validateDefinition(v.cfg, def); err != nil {
		return Request{}, err
	}

	id, err := randomToken("req_")
	if err != nil {
		return Request{}, err
	}
	nonce, err := randomToken("nonce_")
	if err != nil {
		return Request{}, err
	}

	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()

	for {
		if _, exists := v.requests[id]; !exists {
			break
		}
		id, err = randomToken("req_")
		if err != nil {
			return Request{}, err
		}
	}

	pending := 0
	for _, record := range v.requests {
		if record.TenantID == tenantID && record.Status == StatusPending {
			pending++
		}
	}
	if pending >= v.cfg.MaxPendingPerTenant {
		return Request{}, ErrRequestLimit
	}

	v.nextSequence++
	record := &requestRecord{
		Request: Request{
			ID:           id,
			TenantID:     tenantID,
			Nonce:        nonce,
			DefinitionID: def.ID,
			ExpiresAt:    now.Add(v.cfg.TTL),
		},
		Sequence:   v.nextSequence,
		CreatedAt:  now,
		Definition: def,
		Status:     StatusPending,
	}
	v.requests[id] = record
	return record.Request, nil
}

// Verify checks one vp_token plus submission against an outstanding request.
func (v *PresentationVerifier) Verify(requestID, vpToken, submission string) (Result, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	record, ok := v.requests[requestID]
	if !ok {
		return Result{}, ErrUnknownRequest
	}
	if record.Status != StatusPending {
		return Result{}, ErrRequestClosed
	}

	now := v.now()
	if !now.Before(record.ExpiresAt) {
		record.Status = StatusExpired
		return Result{}, ErrRequestClosed
	}

	var token vpTokenDocument
	if err := json.Unmarshal([]byte(vpToken), &token); err != nil {
		return Result{}, fmt.Errorf("%w: vp_token: %v", ErrBadToken, err)
	}

	var presented submissionDocument
	if err := json.Unmarshal([]byte(submission), &presented); err != nil {
		return Result{}, fmt.Errorf("%w: submission: %v", ErrBadToken, err)
	}

	descriptors := make(map[string]InputDescriptor, len(record.Definition.Descriptors))
	for _, descriptor := range record.Definition.Descriptors {
		descriptors[descriptor.ID] = descriptor
	}

	mappings := make(map[string]descriptorMapEntry, len(presented.DescriptorMap))
	for _, mapping := range presented.DescriptorMap {
		if _, duplicate := mappings[mapping.ID]; duplicate {
			return Result{}, fmt.Errorf("%w: duplicate mapping for descriptor %q", ErrBadToken, mapping.ID)
		}
		if _, known := descriptors[mapping.ID]; !known {
			return Result{}, fmt.Errorf("%w: mapping for unknown descriptor %q", ErrBadToken, mapping.ID)
		}
		mappings[mapping.ID] = mapping
	}

	matched := make([]string, 0, len(record.Definition.Descriptors))
	subject := ""

	for _, descriptor := range record.Definition.Descriptors {
		mapping, mapped := mappings[descriptor.ID]
		if !mapped {
			return v.concludeRejectedLocked(record, ReasonConstraintFailed), nil
		}

		credential, err := resolveMappedCredential(&token, mapping.Path)
		if err != nil {
			return Result{}, fmt.Errorf("%w: descriptor %q path %q: %v", ErrBadToken, descriptor.ID, mapping.Path, err)
		}

		if reason := v.evaluationReason(record, descriptor, mapping, credential); reason != "" {
			return v.concludeRejectedLocked(record, reason), nil
		}

		matched = append(matched, descriptor.ID)
		if subject == "" {
			subject = credential.Holder
		}
	}

	record.Status = StatusVerified
	record.Reason = ReasonOK
	record.Matched = cloneStrings(matched)
	record.Subject = subject
	record.Submitted = 1

	return Result{
		RequestID: record.ID,
		Status:    StatusVerified,
		Reason:    ReasonOK,
		Matched:   cloneStrings(record.Matched),
		Subject:   record.Subject,
	}, nil
}

// State reads the durable state of one request.
func (v *PresentationVerifier) State(requestID string) (RequestState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	record, ok := v.requests[requestID]
	if !ok {
		return RequestState{}, ErrUnknownRequest
	}
	return requestState(record), nil
}

// Expire sweeps every pending request whose TTL elapsed and returns how many
// were closed.
func (v *PresentationVerifier) Expire(now time.Time) int {
	v.mu.Lock()
	defer v.mu.Unlock()

	swept := 0
	for _, record := range v.requests {
		if record.Status == StatusPending && !now.Before(record.ExpiresAt) {
			record.Status = StatusExpired
			swept++
		}
	}
	return swept
}

// PutStatusList installs or replaces one status list snapshot. An older epoch
// is ignored without replacing the cached snapshot.
func (v *PresentationVerifier) PutStatusList(list StatusList) error {
	if strings.TrimSpace(list.URI) == "" {
		return errors.New("status list uri is required")
	}
	for _, index := range list.Revoked {
		if index < 0 {
			return fmt.Errorf("status list %q contains negative revoked index %d", list.URI, index)
		}
	}

	list.Revoked = cloneInts(list.Revoked)

	v.mu.Lock()
	defer v.mu.Unlock()

	if current, ok := v.statusLists[list.URI]; ok && list.Epoch < current.Epoch {
		return nil
	}
	v.statusLists[list.URI] = list
	return nil
}

// StatusListEpoch reports the epoch currently cached for one status list URI.
func (v *PresentationVerifier) StatusListEpoch(uri string) (int64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	list, ok := v.statusLists[uri]
	if !ok {
		return 0, false
	}
	return list.Epoch, true
}

// SupportedFormats lists the credential formats the verifier accepts.
func (v *PresentationVerifier) SupportedFormats() []string {
	return cloneStrings(v.cfg.Formats)
}

// PendingRequests lists the open requests of one tenant, oldest first.
func (v *PresentationVerifier) PendingRequests(tenantID string) []RequestState {
	v.mu.Lock()
	defer v.mu.Unlock()

	records := make([]*requestRecord, 0)
	for _, record := range v.requests {
		if record.TenantID == tenantID && record.Status == StatusPending {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Sequence < records[j].Sequence
	})

	states := make([]RequestState, 0, len(records))
	for _, record := range records {
		states = append(states, requestState(record))
	}
	return states
}

// Snapshot serialises the request table, the conclusions and the status list
// cache.
func (v *PresentationVerifier) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	snapshot := snapshotDocument{
		Version:      snapshotVersion,
		NextSequence: v.nextSequence,
		Requests:     v.requests,
		StatusLists:  v.statusLists,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole verifier state with a snapshot, all or nothing.
func (v *PresentationVerifier) Restore(data []byte) error {
	var snapshot snapshotDocument
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return err
	}

	if snapshot.Requests == nil {
		snapshot.Requests = make(map[string]*requestRecord)
	}
	if snapshot.StatusLists == nil {
		snapshot.StatusLists = make(map[string]StatusList)
	}

	maxSequence := uint64(0)
	for _, record := range snapshot.Requests {
		if record.Sequence > maxSequence {
			maxSequence = record.Sequence
		}
	}
	if snapshot.NextSequence < maxSequence {
		snapshot.NextSequence = maxSequence
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = snapshot.Requests
	v.statusLists = snapshot.StatusLists
	v.nextSequence = snapshot.NextSequence
	return nil
}

func (v *PresentationVerifier) now() time.Time {
	if v.cfg.Now != nil {
		return v.cfg.Now()
	}
	return time.Now()
}

func validateDefinition(cfg Config, definition PresentationDefinition) error {
	if len(definition.Descriptors) == 0 || len(definition.Descriptors) > cfg.MaxCredentialsPerRequest {
		return ErrBadDefinition
	}

	for _, descriptor := range definition.Descriptors {
		for _, format := range descriptor.Formats {
			if !formatAllowed(cfg.Formats, format) {
				return ErrUnsupportedFormat
			}
		}
	}
	return nil
}

func formatAllowed(allowed []string, format string) bool {
	for _, candidate := range allowed {
		if candidate == format {
			return true
		}
	}
	return false
}

func (v *PresentationVerifier) evaluationReason(
	record *requestRecord,
	descriptor InputDescriptor,
	mapping descriptorMapEntry,
	credential credentialDocument,
) string {
	if !v.credentialFormatAllowed(descriptor, mapping, credential) {
		return ReasonFormatUnsupported
	}

	if credential.KB == nil || credential.KB.Nonce != record.Nonce {
		return ReasonNonceMismatch
	}
	if credential.KB.Aud != v.cfg.Audience {
		return ReasonAudienceMismatch
	}
	if credential.KB.Iss != credential.Holder {
		return ReasonHolderMismatch
	}
	if !iatInsideRequest(credential.KB.Iat, record.CreatedAt, record.ExpiresAt) {
		return ReasonIATInvalid
	}
	if reason := v.statusReason(credential.Status); reason != "" {
		return reason
	}
	if !constraintsSatisfied(descriptor.Fields, credential.Claims) {
		return ReasonConstraintFailed
	}

	return ""
}

func (v *PresentationVerifier) credentialFormatAllowed(
	descriptor InputDescriptor,
	mapping descriptorMapEntry,
	credential credentialDocument,
) bool {
	if credential.Format == "" || !formatAllowed(v.cfg.Formats, credential.Format) {
		return false
	}
	if mapping.Format != "" && !formatAllowed(v.cfg.Formats, mapping.Format) {
		return false
	}
	if len(descriptor.Formats) == 0 {
		return true
	}
	return formatAllowed(descriptor.Formats, credential.Format)
}

func iatInsideRequest(iat int64, issuedAt, expiresAt time.Time) bool {
	if iat <= 0 {
		return false
	}
	return iat >= issuedAt.Unix() && iat <= expiresAt.Unix()
}

func (v *PresentationVerifier) statusReason(status *statusReference) string {
	if status == nil {
		return ""
	}

	list, ok := v.statusLists[status.URI]
	if !ok {
		return ReasonStatusUnavailable
	}
	if list.Epoch != status.Epoch {
		return ReasonStatusEpoch
	}
	for _, revokedIndex := range list.Revoked {
		if revokedIndex == status.Index {
			return ReasonRevoked
		}
	}
	return ""
}

func constraintsSatisfied(constraints []FieldConstraint, claims map[string]any) bool {
	for _, constraint := range constraints {
		value, exists := lookupClaim(claims, constraint.Path)
		if !exists || !valueMatchesAny(value, constraint.Values) {
			return false
		}
	}
	return true
}

func lookupClaim(claims map[string]any, path string) (any, bool) {
	if claims == nil {
		return nil, false
	}

	normalized := strings.TrimPrefix(strings.TrimSpace(path), "$.")
	normalized = strings.TrimPrefix(normalized, "$")
	if normalized == "" {
		return nil, false
	}

	// Wallet claims can use a dotted key literally, such as
	// {"degree.type": "BachelorDegree"}. Check that before traversing dots.
	if value, ok := claims[normalized]; ok {
		return value, true
	}

	parts := strings.Split(normalized, ".")
	var current any = claims
	for _, part := range parts {
		part = strings.TrimSpace(part)
		for part != "" {
			bracket := strings.Index(part, "[")
			if bracket < 0 {
				value, ok := readMapKey(current, part)
				if !ok {
					return nil, false
				}
				current = value
				break
			}

			key := part[:bracket]
			if key != "" {
				value, ok := readMapKey(current, key)
				if !ok {
					return nil, false
				}
				current = value
			}

			closeOffset := strings.Index(part[bracket:], "]")
			if closeOffset < 0 {
				return nil, false
			}
			closeBracket := bracket + closeOffset
			indexText := strings.TrimSpace(part[bracket+1 : closeBracket])
			index, err := strconv.Atoi(strings.Trim(indexText, `"'`))
			if err != nil {
				return nil, false
			}
			array, ok := current.([]any)
			if !ok || index < 0 || index >= len(array) {
				return nil, false
			}
			current = array[index]
			part = part[closeBracket+1:]
		}
	}
	return current, true
}

func readMapKey(current any, key string) (any, bool) {
	object, ok := current.(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := object[key]
	return value, ok
}

func valueMatchesAny(value any, accepted []string) bool {
	for _, candidate := range flattenValues(value) {
		for _, expected := range accepted {
			if candidate == expected {
				return true
			}
		}
	}
	return false
}

func flattenValues(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case bool:
		return []string{strconv.FormatBool(typed)}
	case float64:
		return []string{strconv.FormatFloat(typed, 'f', -1, 64)}
	case json.Number:
		return []string{typed.String()}
	case []any:
		var values []string
		for _, item := range typed {
			values = append(values, flattenValues(item)...)
		}
		return values
	default:
		return []string{fmt.Sprint(typed)}
	}
}

func resolveMappedCredential(token *vpTokenDocument, path string) (credentialDocument, error) {
	index, err := credentialPathIndex(path)
	if err != nil {
		return credentialDocument{}, err
	}
	if index < 0 || index >= len(token.Credentials) {
		return credentialDocument{}, fmt.Errorf("credential index %d is outside credentials", index)
	}
	return token.Credentials[index], nil
}

type pathSegment struct {
	key     string
	index   int
	isIndex bool
}

func credentialPathIndex(path string) (int, error) {
	segments, err := parsePathSegments(path)
	if err != nil {
		return 0, err
	}
	if len(segments) != 2 || segments[0].key != "credentials" || !segments[1].isIndex {
		return 0, errors.New("path must identify $.credentials[N]")
	}
	return segments[1].index, nil
}

func parsePathSegments(path string) ([]pathSegment, error) {
	text := strings.TrimSpace(path)
	if text == "" {
		return nil, errors.New("empty path")
	}
	text = strings.TrimPrefix(text, "$")

	segments := make([]pathSegment, 0, 2)
	for text != "" {
		switch text[0] {
		case '.':
			text = text[1:]
			if text == "" {
				return nil, errors.New("unterminated path segment")
			}
			if text[0] == '.' {
				return nil, errors.New("consecutive dots in path")
			}
			if text[0] == '[' {
				continue
			}
			end := strings.IndexAny(text, ".[")
			key := text
			if end >= 0 {
				if end == 0 {
					return nil, errors.New("empty path key")
				}
				key = text[:end]
				text = text[end:]
			} else {
				text = ""
			}
			if index, err := strconv.Atoi(key); err == nil {
				if index < 0 {
					return nil, fmt.Errorf("negative path index %d", index)
				}
				segments = append(segments, pathSegment{index: index, isIndex: true})
			} else {
				segments = append(segments, pathSegment{key: key})
			}
		case '[':
			end := strings.Index(text, "]")
			if end < 0 {
				return nil, errors.New("unterminated bracket segment")
			}
			inside := strings.TrimSpace(text[1:end])
			text = text[end+1:]
			if len(inside) >= 2 && (inside[0] == '\'' || inside[0] == '"') && inside[len(inside)-1] == inside[0] {
				segments = append(segments, pathSegment{key: inside[1 : len(inside)-1]})
				continue
			}
			index, err := strconv.Atoi(inside)
			if err != nil || index < 0 {
				return nil, fmt.Errorf("invalid bracket segment %q", inside)
			}
			segments = append(segments, pathSegment{index: index, isIndex: true})
		default:
			end := strings.IndexAny(text, ".[")
			key := text
			if end >= 0 {
				key = text[:end]
				text = text[end:]
			} else {
				text = ""
			}
			segments = append(segments, pathSegment{key: key})
		}
	}
	return segments, nil
}

func (v *PresentationVerifier) concludeRejectedLocked(record *requestRecord, reason string) Result {
	record.Status = StatusRejected
	record.Reason = reason
	record.Matched = nil
	record.Subject = ""
	record.Submitted = 1
	return Result{
		RequestID: record.ID,
		Status:    StatusRejected,
		Reason:    reason,
	}
}

func requestState(record *requestRecord) RequestState {
	return RequestState{
		ID:        record.ID,
		TenantID:  record.TenantID,
		Status:    record.Status,
		Submitted: record.Submitted,
		Matched:   cloneStrings(record.Matched),
		Subject:   record.Subject,
		Reason:    record.Reason,
	}
}

func validateSnapshot(snapshot *snapshotDocument) error {
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}

	for id, record := range snapshot.Requests {
		if record == nil {
			return fmt.Errorf("%w: request %q is null", ErrBadSnapshot, id)
		}
		if record.ID != id {
			return fmt.Errorf("%w: request id %q does not match key %q", ErrBadSnapshot, record.ID, id)
		}
		if record.Nonce == "" {
			return fmt.Errorf("%w: request %q has no nonce", ErrBadSnapshot, id)
		}
		if record.ExpiresAt.IsZero() || record.CreatedAt.IsZero() {
			return fmt.Errorf("%w: request %q has invalid validity window", ErrBadSnapshot, id)
		}
		switch record.Status {
		case StatusPending:
			if record.Submitted != 0 {
				return fmt.Errorf("%w: pending request %q has submission count %d", ErrBadSnapshot, id, record.Submitted)
			}
		case StatusVerified, StatusRejected, StatusExpired:
			if record.Submitted < 0 || record.Submitted > 1 {
				return fmt.Errorf("%w: request %q has invalid submission count %d", ErrBadSnapshot, id, record.Submitted)
			}
		default:
			return fmt.Errorf("%w: request %q has unknown status %q", ErrBadSnapshot, id, record.Status)
		}
	}

	for uri, list := range snapshot.StatusLists {
		if uri == "" {
			return fmt.Errorf("%w: status list has empty uri key", ErrBadSnapshot)
		}
		if list.URI != "" && list.URI != uri {
			return fmt.Errorf("%w: status list uri %q does not match key %q", ErrBadSnapshot, list.URI, uri)
		}
		for _, index := range list.Revoked {
			if index < 0 {
				return fmt.Errorf("%w: status list %q has negative index", ErrBadSnapshot, uri)
			}
		}
	}
	return nil
}

func randomToken(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneInts(values []int) []int {
	if values == nil {
		return nil
	}
	cloned := make([]int, len(values))
	copy(cloned, values)
	return cloned
}

func cloneDefinition(definition PresentationDefinition) PresentationDefinition {
	cloned := definition
	if definition.Descriptors != nil {
		cloned.Descriptors = make([]InputDescriptor, len(definition.Descriptors))
		for i, descriptor := range definition.Descriptors {
			cloned.Descriptors[i] = descriptor
			cloned.Descriptors[i].Formats = cloneStrings(descriptor.Formats)
			if descriptor.Fields != nil {
				cloned.Descriptors[i].Fields = make([]FieldConstraint, len(descriptor.Fields))
				for j, field := range descriptor.Fields {
					cloned.Descriptors[i].Fields[j] = field
					cloned.Descriptors[i].Fields[j].Values = cloneStrings(field.Values)
				}
			}
		}
	}
	return cloned
}
