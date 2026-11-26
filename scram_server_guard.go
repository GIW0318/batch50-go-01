// Package task runs the server half of a SCRAM-SHA-256 handshake for the
// database proxy.
package task

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GS2 channel binding flags.
const (
	CBindNone      = "n"
	CBindSupported = "y"
	CBindUsed      = "p"
)

const (
	snapshotType    = "scram-server-guard-snapshot"
	snapshotVersion = 1
)

// Errors returned by the guard.
var (
	ErrBadClientFirst         = errors.New("client-first message is malformed")
	ErrUnknownUser            = errors.New("unknown user")
	ErrIterationsTooLow       = errors.New("iteration count is below the policy minimum")
	ErrBadPolicy              = errors.New("policy is invalid")
	ErrInflightLimit          = errors.New("too many handshakes in flight")
	ErrUnknownState           = errors.New("unknown or expired handshake state")
	ErrNonceMismatch          = errors.New("client nonce does not extend the server nonce")
	ErrChannelBindingMismatch = errors.New("channel binding data does not match the connection")
	ErrAuthenticationFailed   = errors.New("authentication failed")
	ErrLocked                 = errors.New("too many failures for this account and source")
	ErrBadSnapshot            = errors.New("snapshot payload is invalid")
)

// StoredCredential is the salted verifier material kept for one account. No
// plaintext password is ever stored.
type StoredCredential struct {
	TenantID   string `json:"tenant_id"`
	Username   string `json:"username"`
	Salt       []byte `json:"salt"`
	Iterations int    `json:"iterations"`
	StoredKey  []byte `json:"stored_key"`
	ServerKey  []byte `json:"server_key"`
}

// CredentialStore looks up the verifier material of one account.
type CredentialStore interface {
	Lookup(tenantID, username string) (StoredCredential, bool)
}

// ScramPolicy is the server side policy applied to newly started handshakes.
type ScramPolicy struct {
	MinIterations int   `json:"min_iterations"`
	MaxInflight   int   `json:"max_inflight"`
	StateTTL      int64 `json:"state_ttl"`
	MaxFailures   int   `json:"max_failures"`
}

// Principal is the authenticated identity of a finished handshake.
type Principal struct {
	TenantID    string `json:"tenant_id"`
	Username    string `json:"username"`
	AuthzID     string `json:"authzid"`
	Iterations  int    `json:"iterations"`
	ServerFinal []byte `json:"server_final"`
}

// ScramServerGuard owns in-flight handshakes and failure counters.
type ScramServerGuard struct {
	mu       sync.Mutex
	tenantID string
	store    CredentialStore
	policy   ScramPolicy
	now      func() int64
	cbind    []byte
	states   map[string]*handshakeState
	failures map[failureKey]int
}

type failureKey struct {
	Username string `json:"username"`
	Source   string `json:"source"`
}

type handshakeState struct {
	ID          string           `json:"id"`
	Source      string           `json:"source"`
	TenantID    string           `json:"tenant_id"`
	Username    string           `json:"username"`
	AuthzID     string           `json:"authz_id,omitempty"`
	GS2Header   string           `json:"gs2_header"`
	Bare        string           `json:"bare"`
	ClientNonce string           `json:"client_nonce"`
	Nonce       string           `json:"nonce"`
	ServerFirst string           `json:"server_first"`
	Iterations  int              `json:"iterations"`
	StartedAt   int64            `json:"started_at"`
	Policy      ScramPolicy      `json:"policy"`
	Credential  StoredCredential `json:"credential"`
}

type failureSnapshot struct {
	Username string `json:"username"`
	Source   string `json:"source"`
	Count    int    `json:"count"`
}

type snapshotEnvelope struct {
	Type           string            `json:"type"`
	Version        int               `json:"version"`
	TenantID       string            `json:"tenant_id,omitempty"`
	Policy         ScramPolicy       `json:"policy"`
	ChannelBinding []byte            `json:"channel_binding,omitempty"`
	States         []*handshakeState `json:"states"`
	Failures       []failureSnapshot `json:"failures"`
}

type clientFirstInfo struct {
	gs2Header string
	authzID   string
	bare      string
	username  string
	nonce     string
}

type clientFinalInfo struct {
	cBinding string
	nonce    string
	proof    string
	noProof  string
}

var serverNonceCounter uint64

// NewScramServerGuard builds a guard for one tenant.
func NewScramServerGuard(tenantID string, store CredentialStore, p ScramPolicy) *ScramServerGuard {
	return &ScramServerGuard{
		tenantID: tenantID,
		store:    store,
		policy:   p,
		now:      func() int64 { return time.Now().Unix() },
		states:   make(map[string]*handshakeState),
		failures: make(map[failureKey]int),
	}
}

// Begin starts a handshake from an unspecified source.
func (g *ScramServerGuard) Begin(clientFirst []byte) ([]byte, string, error) {
	return g.BeginFrom("", clientFirst)
}

// BeginFrom parses the first message, checks account policy and capacity, and
// returns server-first plus a one-shot state identifier.
func (g *ScramServerGuard) BeginFrom(source string, clientFirst []byte) ([]byte, string, error) {
	first, err := parseClientFirst(clientFirst)
	if err != nil {
		return nil, "", err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.currentTimeLocked()
	g.sweepExpiredLocked(now)

	failKey := failureKey{Username: first.username, Source: source}
	if g.failures[failKey] >= g.policy.MaxFailures {
		return nil, "", ErrLocked
	}

	cred, ok := g.store.Lookup(g.tenantID, first.username)
	if !ok {
		return nil, "", ErrUnknownUser
	}
	if cred.Iterations < g.policy.MinIterations {
		return nil, "", ErrIterationsTooLow
	}
	if len(g.states) >= g.policy.MaxInflight {
		return nil, "", ErrInflightLimit
	}

	stateID, err := g.newStateIDLocked()
	if err != nil {
		return nil, "", err
	}
	combinedNonce, err := newServerNonce(first.nonce)
	if err != nil {
		return nil, "", err
	}

	serverFirst := fmt.Sprintf(
		"r=%s,s=%s,i=%d",
		combinedNonce,
		base64.StdEncoding.EncodeToString(cred.Salt),
		cred.Iterations,
	)

	g.states[stateID] = &handshakeState{
		ID:          stateID,
		Source:      source,
		TenantID:    g.tenantID,
		Username:    first.username,
		AuthzID:     first.authzID,
		GS2Header:   first.gs2Header,
		Bare:        first.bare,
		ClientNonce: first.nonce,
		Nonce:       combinedNonce,
		ServerFirst: serverFirst,
		Iterations:  cred.Iterations,
		StartedAt:   now,
		Policy:      g.policy,
		Credential:  cred,
	}

	return []byte(serverFirst), stateID, nil
}

// Continue verifies the final client message and consumes the state.
func (g *ScramServerGuard) Continue(stateID string, clientFinal []byte) (Principal, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok := g.states[stateID]
	if !ok {
		return Principal{}, ErrUnknownState
	}
	now := g.currentTimeLocked()
	if isExpired(state, now) {
		delete(g.states, stateID)
		return Principal{}, ErrUnknownState
	}

	// Every Continue attempt is terminal for this state ID. Concurrent or
	// repeated calls therefore observe ErrUnknownState.
	delete(g.states, stateID)

	final, err := parseClientFinal(clientFinal)
	if err != nil {
		if errors.Is(err, ErrAuthenticationFailed) {
			g.recordFailureLocked(state)
		}
		return Principal{}, err
	}

	if final.nonce != state.Nonce {
		return Principal{}, ErrNonceMismatch
	}

	bindingInput := make([]byte, 0, len(state.GS2Header)+len(g.cbind))
	bindingInput = append(bindingInput, state.GS2Header...)
	bindingInput = append(bindingInput, g.cbind...)
	expectedCBinding := base64.StdEncoding.EncodeToString(bindingInput)
	if final.cBinding != expectedCBinding {
		return Principal{}, ErrChannelBindingMismatch
	}

	proof, err := base64.StdEncoding.DecodeString(final.proof)
	if err != nil || len(proof) != sha256.Size {
		g.recordFailureLocked(state)
		return Principal{}, ErrAuthenticationFailed
	}

	authMessage := state.Bare + "," + state.ServerFirst + "," + final.noProof
	clientSignature := hmacSHA256(state.Credential.StoredKey, authMessage)
	if len(proof) != len(clientSignature) {
		g.recordFailureLocked(state)
		return Principal{}, ErrAuthenticationFailed
	}

	clientKey := make([]byte, sha256.Size)
	for i := range proof {
		clientKey[i] = proof[i] ^ clientSignature[i]
	}
	storedKey := sha256.Sum256(clientKey)
	if !hmac.Equal(storedKey[:], state.Credential.StoredKey) {
		g.recordFailureLocked(state)
		return Principal{}, ErrAuthenticationFailed
	}

	if state.AuthzID != "" {
		if _, exists := g.store.Lookup(g.tenantID, state.AuthzID); !exists {
			g.recordFailureLocked(state)
			return Principal{}, ErrAuthenticationFailed
		}
	}

	serverSignature := hmacSHA256(state.Credential.ServerKey, authMessage)
	serverFinal := "v=" + base64.StdEncoding.EncodeToString(serverSignature)

	return Principal{
		TenantID:    state.TenantID,
		Username:    state.Username,
		AuthzID:     state.AuthzID,
		Iterations:  state.Iterations,
		ServerFinal: []byte(serverFinal),
	}, nil
}

// Abort drops one live handshake state.
func (g *ScramServerGuard) Abort(stateID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok := g.states[stateID]
	if !ok {
		return ErrUnknownState
	}
	if isExpired(state, g.currentTimeLocked()) {
		delete(g.states, stateID)
		return ErrUnknownState
	}
	delete(g.states, stateID)
	return nil
}

// SetPolicy replaces the policy used by later handshakes.
func (g *ScramServerGuard) SetPolicy(p ScramPolicy) error {
	if p.MinIterations <= 0 {
		return ErrBadPolicy
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = p
	return nil
}

// Policy reports the current policy.
func (g *ScramServerGuard) Policy() ScramPolicy {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.policy
}

// Pending counts live, unexpired handshake states.
func (g *ScramServerGuard) Pending() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.currentTimeLocked()
	pending := 0
	for _, state := range g.states {
		if !isExpired(state, now) {
			pending++
		}
	}
	return pending
}

// Sweep removes expired states and reports how many were removed.
func (g *ScramServerGuard) Sweep() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sweepExpiredLocked(g.currentTimeLocked())
}

// SetClock replaces the time source used for TTL accounting.
func (g *ScramServerGuard) SetClock(now func() int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = now
}

// SetChannelBinding sets the connection channel-binding digest.
func (g *ScramServerGuard) SetChannelBinding(digest []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cbind = append([]byte(nil), digest...)
}

// Failures reports the failure count for one account and source.
func (g *ScramServerGuard) Failures(username, source string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failures[failureKey{Username: username, Source: source}]
}

// Snapshot serializes policy, handshake states and failure counters.
func (g *ScramServerGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	snapshot := snapshotEnvelope{
		Type:           snapshotType,
		Version:        snapshotVersion,
		TenantID:       g.tenantID,
		Policy:         g.policy,
		ChannelBinding: append([]byte(nil), g.cbind...),
		States:         make([]*handshakeState, 0, len(g.states)),
		Failures:       make([]failureSnapshot, 0, len(g.failures)),
	}

	for _, state := range g.states {
		stateCopy := *state
		snapshot.States = append(snapshot.States, &stateCopy)
	}
	sort.Slice(snapshot.States, func(i, j int) bool {
		return snapshot.States[i].ID < snapshot.States[j].ID
	})

	for key, count := range g.failures {
		snapshot.Failures = append(snapshot.Failures, failureSnapshot{
			Username: key.Username,
			Source:   key.Source,
			Count:    count,
		})
	}
	sort.Slice(snapshot.Failures, func(i, j int) bool {
		if snapshot.Failures[i].Username != snapshot.Failures[j].Username {
			return snapshot.Failures[i].Username < snapshot.Failures[j].Username
		}
		return snapshot.Failures[i].Source < snapshot.Failures[j].Source
	})

	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore atomically replaces restorable guard state. Invalid data leaves the
// current guard untouched.
func (g *ScramServerGuard) Restore(data []byte) error {
	var snapshot snapshotEnvelope
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ErrBadSnapshot
	}
	if snapshot.Type != snapshotType || snapshot.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if snapshot.Policy.MinIterations <= 0 {
		return ErrBadSnapshot
	}

	states := make(map[string]*handshakeState, len(snapshot.States))
	for _, state := range snapshot.States {
		if state == nil || state.ID == "" || state.Nonce == "" || state.Bare == "" ||
			state.GS2Header == "" || state.ServerFirst == "" {
			return ErrBadSnapshot
		}
		if _, duplicate := states[state.ID]; duplicate {
			return ErrBadSnapshot
		}
		if len(state.Credential.StoredKey) != sha256.Size ||
			len(state.Credential.ServerKey) != sha256.Size {
			return ErrBadSnapshot
		}
		states[state.ID] = state
	}

	failures := make(map[failureKey]int, len(snapshot.Failures))
	for _, item := range snapshot.Failures {
		if item.Count < 0 {
			return ErrBadSnapshot
		}
		key := failureKey{Username: item.Username, Source: item.Source}
		failures[key] += item.Count
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.policy = snapshot.Policy
	g.states = states
	g.failures = failures
	g.cbind = append([]byte(nil), snapshot.ChannelBinding...)
	if snapshot.TenantID != "" {
		g.tenantID = snapshot.TenantID
	}
	return nil
}

func (g *ScramServerGuard) currentTimeLocked() int64 {
	if g.now != nil {
		return g.now()
	}
	return time.Now().Unix()
}

func (g *ScramServerGuard) sweepExpiredLocked(now int64) int {
	removed := 0
	for id, state := range g.states {
		if isExpired(state, now) {
			delete(g.states, id)
			removed++
		}
	}
	return removed
}

func (g *ScramServerGuard) recordFailureLocked(state *handshakeState) {
	key := failureKey{Username: state.Username, Source: state.Source}
	g.failures[key]++
}

func (g *ScramServerGuard) newStateIDLocked() (string, error) {
	for {
		var raw [24]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		if _, exists := g.states[id]; !exists {
			return id, nil
		}
	}
}

func isExpired(state *handshakeState, now int64) bool {
	return now-state.StartedAt >= state.Policy.StateTTL
}

func newServerNonce(clientNonce string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:24]); err != nil {
		return "", err
	}
	counter := atomic.AddUint64(&serverNonceCounter, 1)
	binary.BigEndian.PutUint64(raw[24:], counter)
	return clientNonce + hex.EncodeToString(raw[:]), nil
}

func parseClientFirst(raw []byte) (clientFirstInfo, error) {
	message := string(raw)
	headerEnd := strings.Index(message, ",,")
	if headerEnd <= 0 || headerEnd+2 >= len(message) {
		return clientFirstInfo{}, ErrBadClientFirst
	}

	header := message[:headerEnd+2]
	core := header[:len(header)-2]
	headerFields := strings.Split(core, ",")
	flag := headerFields[0]
	switch {
	case flag == CBindNone || flag == CBindSupported:
	case strings.HasPrefix(flag, CBindUsed+"=") && len(flag) > len(CBindUsed)+1:
	default:
		return clientFirstInfo{}, ErrBadClientFirst
	}

	authzID := ""
	authzSeen := false
	for _, field := range headerFields[1:] {
		if !strings.HasPrefix(field, "a=") {
			return clientFirstInfo{}, ErrBadClientFirst
		}
		if authzSeen {
			return clientFirstInfo{}, ErrBadClientFirst
		}
		authzSeen = true
		authzID = field[2:]
	}

	bare := message[headerEnd+2:]
	bareFields := strings.Split(bare, ",")
	if len(bareFields) != 2 ||
		!strings.HasPrefix(bareFields[0], "n=") ||
		!strings.HasPrefix(bareFields[1], "r=") {
		return clientFirstInfo{}, ErrBadClientFirst
	}

	username := bareFields[0][2:]
	nonce := bareFields[1][2:]
	if len(nonce) < 8 {
		return clientFirstInfo{}, ErrBadClientFirst
	}

	return clientFirstInfo{
		gs2Header: header,
		authzID:   authzID,
		bare:      bare,
		username:  username,
		nonce:     nonce,
	}, nil
}

func parseClientFinal(raw []byte) (clientFinalInfo, error) {
	fields := strings.Split(string(raw), ",")
	if len(fields) < 3 {
		if !hasPrefixedField(fields, "c=") {
			return clientFinalInfo{}, ErrChannelBindingMismatch
		}
		if !hasPrefixedField(fields, "r=") {
			return clientFinalInfo{}, ErrNonceMismatch
		}
		return clientFinalInfo{}, ErrAuthenticationFailed
	}
	if !strings.HasPrefix(fields[0], "c=") {
		return clientFinalInfo{}, ErrChannelBindingMismatch
	}
	if !strings.HasPrefix(fields[1], "r=") {
		return clientFinalInfo{}, ErrNonceMismatch
	}
	if !strings.HasPrefix(fields[2], "p=") || len(fields) != 3 {
		return clientFinalInfo{}, ErrAuthenticationFailed
	}

	return clientFinalInfo{
		cBinding: fields[0][2:],
		nonce:    fields[1][2:],
		proof:    fields[2][2:],
		noProof:  fields[0] + "," + fields[1],
	}, nil
}

func hasPrefixedField(fields []string, prefix string) bool {
	for _, field := range fields {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return mac.Sum(nil)
}
