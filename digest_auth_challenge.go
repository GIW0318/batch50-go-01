// Package task implements the server side of an HTTP Digest challenge: nonce
// generation, generation aware validation, nonce count watermarks and replay
// governance for the gateway.
package task

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"
)

// Digest algorithms the guard knows about. MD5 is recognised only so that it
// can be rejected explicitly.
const (
	AlgorithmSHA256    = "SHA-256"
	AlgorithmSHA512256 = "SHA-512-256"
	AlgorithmMD5       = "MD5"
	AlgorithmNone      = "none"
)

// Quality of protection values accepted on the wire.
const (
	QopAuth    = "auth"
	QopAuthInt = "auth-int"
)

// Errors reported by the guard.
var (
	ErrBadChallenge      = errors.New("challenge parameters are invalid")
	ErrAlgorithmRejected = errors.New("digest algorithm is not accepted")
	ErrQopRejected       = errors.New("quality of protection is not accepted")
	ErrNonceInvalid      = errors.New("nonce is unknown or belongs to another realm")
	ErrStaleNonce        = errors.New("nonce has expired")
	ErrReplay            = errors.New("nonce count was already used")
	ErrAuthFailed        = errors.New("credentials rejected")
	ErrSessionLimit      = errors.New("per user session limit reached")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

const (
	snapshotVersion = 1
	nonceTokenSize  = 16
)

// Config bounds the guard. Now is injectable so that expiry can be driven by a
// test clock.
type Config struct {
	Realm              string
	NonceWindow        time.Duration
	MaxNoncesPerUser   int
	MaxSessionsPerUser int
	MaxFailuresPerUser int
	Now                func() time.Time
}

// Header is the challenge that goes back to the client as WWW-Authenticate.
type Header struct {
	Realm     string
	Nonce     string
	Opaque    string
	Algorithm string
	Qop       string
	Stale     bool
}

// AuthRequest is one inbound Authorization header unpacked into fields.
type AuthRequest struct {
	Username    string
	Realm       string
	Nonce       string
	URI         string
	Method      string
	Qop         string
	NC          int64
	CNonce      string
	Response    string
	Algorithm   string
	Body        []byte
	Fingerprint string
}

// Principal is the authenticated caller returned by Verify.
type Principal struct {
	Username string
	Nonce    string
	NC       int64
	Sessions int
}

// NonceUsage is the per user accounting exposed for operators.
type NonceUsage struct {
	Username string
	Issued   int
	Active   int
	Replays  int
	MaxNC    int64
}

// DigestChallengeGuard owns the secret generations, the nonce table and the
// nonce count watermarks.
type DigestChallengeGuard struct {
	cfg          Config
	mu           sync.Mutex
	secret       []byte
	generation   uint64
	nextSequence uint64
	passwords    map[string]string
	users        map[string]*userState
	nonces       map[string]*nonceRecord
}

type userState struct {
	Issued           int                      `json:"issued"`
	Replays          int                      `json:"replays"`
	MaxNC            int64                    `json:"max_nc"`
	Failures         int                      `json:"failures"`
	FailureExpiresAt time.Time                `json:"failure_expires_at,omitempty"`
	NonceOrder       []string                 `json:"nonce_order,omitempty"`
	Sessions         map[string]*sessionState `json:"sessions,omitempty"`
}

type sessionState struct {
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

type nonceRecord struct {
	Value      string           `json:"value"`
	Opaque     string           `json:"opaque"`
	Username   string           `json:"username"`
	Realm      string           `json:"realm"`
	Generation uint64           `json:"generation"`
	Sequence   uint64           `json:"sequence"`
	IssuedAt   time.Time        `json:"issued_at"`
	ExpiresAt  time.Time        `json:"expires_at"`
	Watermark  int64            `json:"watermark"`
	Watermarks map[string]int64 `json:"watermarks,omitempty"`
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type snapshotState struct {
	Generation   uint64                  `json:"generation"`
	Secret       []byte                  `json:"secret"`
	NextSequence uint64                  `json:"next_sequence"`
	Users        map[string]*userState   `json:"users"`
	Nonces       map[string]*nonceRecord `json:"nonces"`
}

// NewDigestChallengeGuard builds a guard with the given caps.
func NewDigestChallengeGuard(cfg Config) *DigestChallengeGuard {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NonceWindow <= 0 {
		cfg.NonceWindow = 5 * time.Minute
	}
	if cfg.MaxNoncesPerUser <= 0 {
		cfg.MaxNoncesPerUser = 128
	}
	if cfg.MaxSessionsPerUser <= 0 {
		cfg.MaxSessionsPerUser = 16
	}
	if cfg.MaxFailuresPerUser <= 0 {
		cfg.MaxFailuresPerUser = 5
	}

	secret := make([]byte, sha256.Size)
	if _, err := rand.Read(secret); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("digest-fallback-secret-%d", time.Now().UnixNano())))
		secret = sum[:]
	}

	return &DigestChallengeGuard{
		cfg:        cfg,
		secret:     secret,
		generation: 1,
		passwords:  make(map[string]string),
		users:      make(map[string]*userState),
		nonces:     make(map[string]*nonceRecord),
	}
}

// SetPassword registers the cleartext password used to recompute the digest.
func (g *DigestChallengeGuard) SetPassword(username, password string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("digest: username must not be blank")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.passwords[username] = password
	g.userLocked(username)
	return nil
}

// Challenge issues a fresh nonce for one principal in one realm.
func (g *DigestChallengeGuard) Challenge(realm, username string) (Header, error) {
	if strings.TrimSpace(username) == "" || realm != g.cfg.Realm {
		return Header{}, ErrBadChallenge
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.issueLocked(realm, username)
}

// Verify checks one Authorization header and claims its nonce count.
func (g *DigestChallengeGuard) Verify(req AuthRequest) (Principal, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.cfg.Now()

	if err := g.ValidateAlgorithm(req.Algorithm); err != nil {
		return Principal{}, err
	}
	if err := validateQop(req.Qop); err != nil {
		return Principal{}, err
	}

	password, known := g.passwords[req.Username]
	if !known {
		// Run the same digest shape for unknown users to avoid turning timing
		// into a username oracle.
		_ = verifyDigest(req, "invalid-digest-password")
		return Principal{}, ErrAuthFailed
	}

	user := g.userLocked(req.Username)
	if g.failureBlockedLocked(user, now) {
		return Principal{}, ErrAuthFailed
	}
	if !verifyDigest(req, password) {
		g.recordFailureLocked(user, now)
		return Principal{}, ErrAuthFailed
	}
	g.resetFailuresLocked(user)

	record := g.nonces[req.Nonce]
	if record == nil || record.Generation != g.generation {
		return Principal{}, ErrNonceInvalid
	}
	if req.Realm != g.cfg.Realm || record.Realm != req.Realm {
		return Principal{}, ErrNonceInvalid
	}
	if record.Username != req.Username {
		return Principal{}, ErrNonceInvalid
	}

	if !now.Before(record.ExpiresAt) {
		return Principal{}, ErrStaleNonce
	}

	scope := watermarkScope(req)
	if record.Watermarks == nil {
		record.Watermarks = make(map[string]int64)
	}
	watermark := record.Watermarks[scope]
	if req.NC <= watermark {
		user.Replays++
		return Principal{}, ErrReplay
	}

	sessions, err := g.claimSessionLocked(user, scope, now)
	if err != nil {
		return Principal{}, err
	}

	// The check and assignment happen under the guard mutex, so concurrent
	// retries of one identical request cannot both win the NC claim.
	record.Watermarks[scope] = req.NC
	if req.NC > record.Watermark {
		record.Watermark = req.NC
	}
	if req.NC > user.MaxNC {
		user.MaxNC = req.NC
	}

	return Principal{
		Username: req.Username,
		Nonce:    record.Value,
		NC:       req.NC,
		Sessions: sessions,
	}, nil
}

// RotateSecret moves the guard to the next secret generation, which
// invalidates every nonce issued before the call.
func (g *DigestChallengeGuard) RotateSecret() error {
	secret := make([]byte, sha256.Size)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("rotate digest secret: %w", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.secret = secret
	g.generation++
	g.nonces = make(map[string]*nonceRecord)
	for _, user := range g.users {
		user.NonceOrder = nil
		user.MaxNC = 0
		user.Sessions = make(map[string]*sessionState)
	}
	return nil
}

// Usage reports the nonce accounting of one user.
func (g *DigestChallengeGuard) Usage(username string) NonceUsage {
	g.mu.Lock()
	defer g.mu.Unlock()

	usage := NonceUsage{Username: username}
	user := g.users[username]
	if user == nil {
		return usage
	}

	usage.Issued = user.Issued
	usage.Replays = user.Replays
	usage.MaxNC = user.MaxNC
	usage.Active = g.activeNoncesLocked(username, g.cfg.Now())
	return usage
}

// ValidateAlgorithm reports whether the guard accepts one digest algorithm.
func (g *DigestChallengeGuard) ValidateAlgorithm(algorithm string) error {
	switch algorithm {
	case AlgorithmSHA256, AlgorithmSHA512256:
		return nil
	default:
		return ErrAlgorithmRejected
	}
}

// NonceAge reports how long ago a live nonce was issued.
func (g *DigestChallengeGuard) NonceAge(nonce string) (time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	record := g.nonces[nonce]
	if record == nil || record.Generation != g.generation {
		return 0, ErrNonceInvalid
	}
	return g.cfg.Now().Sub(record.IssuedAt), nil
}

// Sweep reclaims expired nonces and returns how many records went away.
func (g *DigestChallengeGuard) Sweep(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	swept := 0
	for nonce, record := range g.nonces {
		if now.Before(record.ExpiresAt) {
			continue
		}

		delete(g.nonces, nonce)
		if user := g.users[record.Username]; user != nil {
			user.NonceOrder = removeString(user.NonceOrder, nonce)
		}
		swept++
	}
	return swept
}

// ActiveNonces counts the live nonces of one user.
func (g *DigestChallengeGuard) ActiveNonces(username string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeNoncesLocked(username, g.cfg.Now())
}

// Snapshot serialises the nonce table, the watermarks and the secret generation.
func (g *DigestChallengeGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	state := snapshotState{
		Generation:   g.generation,
		Secret:       append([]byte(nil), g.secret...),
		NextSequence: g.nextSequence,
		Users:        g.users,
		Nonces:       g.nonces,
	}
	payload, err := json.Marshal(state)
	g.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("marshal digest snapshot: %w", err)
	}

	checksum := sha256.Sum256(payload)
	envelope, err := json.Marshal(snapshotEnvelope{
		Version:  snapshotVersion,
		Payload:  payload,
		Checksum: hex.EncodeToString(checksum[:]),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal digest snapshot envelope: %w", err)
	}
	return envelope, nil
}

// Restore replaces the whole guard state with a snapshot, all or nothing.
func (g *DigestChallengeGuard) Restore(data []byte) error {
	state, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if state.Users == nil {
		state.Users = make(map[string]*userState)
	}
	if state.Nonces == nil {
		state.Nonces = make(map[string]*nonceRecord)
	}
	g.generation = state.Generation
	g.secret = append([]byte(nil), state.Secret...)
	g.nextSequence = state.NextSequence
	g.users = state.Users
	g.nonces = state.Nonces
	if g.passwords == nil {
		g.passwords = make(map[string]string)
	}
	return nil
}

func (g *DigestChallengeGuard) issueLocked(realm, username string) (Header, error) {
	nonce, opaque, err := g.newTokensLocked(realm, username)
	if err != nil {
		return Header{}, err
	}

	now := g.cfg.Now()
	user := g.userLocked(username)
	g.evictOldestNonceLocked(user)

	g.nextSequence++
	record := &nonceRecord{
		Value:      nonce,
		Opaque:     opaque,
		Username:   username,
		Realm:      realm,
		Generation: g.generation,
		Sequence:   g.nextSequence,
		IssuedAt:   now,
		ExpiresAt:  now.Add(g.cfg.NonceWindow),
		Watermarks: make(map[string]int64),
	}
	g.nonces[nonce] = record
	user.NonceOrder = append(user.NonceOrder, nonce)
	user.Issued++

	return Header{
		Realm:     realm,
		Nonce:     nonce,
		Opaque:    opaque,
		Algorithm: AlgorithmSHA256,
		Qop:       QopAuth,
		Stale:     false,
	}, nil
}

func (g *DigestChallengeGuard) newTokensLocked(realm, username string) (string, string, error) {
	id := make([]byte, nonceTokenSize)
	opaqueBytes := make([]byte, nonceTokenSize)
	if _, err := rand.Read(id); err != nil {
		return "", "", fmt.Errorf("generate nonce: %w", err)
	}
	if _, err := rand.Read(opaqueBytes); err != nil {
		return "", "", fmt.Errorf("generate opaque: %w", err)
	}

	now := g.cfg.Now()
	window := int64(0)
	if nanos := g.cfg.NonceWindow.Nanoseconds(); nanos > 0 {
		window = now.UnixNano() / nanos
	}

	mac := hmac.New(sha256.New, g.secret)
	fmt.Fprintf(mac, "digest-nonce-v1\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s",
		g.generation, window, now.UnixNano(), realm, username, hex.EncodeToString(id))

	nonce := "dn1-" + hex.EncodeToString(id) + "-" + hex.EncodeToString(mac.Sum(nil))
	opaque := hex.EncodeToString(opaqueBytes)
	return nonce, opaque, nil
}

func (g *DigestChallengeGuard) userLocked(username string) *userState {
	user := g.users[username]
	if user == nil {
		user = &userState{Sessions: make(map[string]*sessionState)}
		g.users[username] = user
	}
	if user.Sessions == nil {
		user.Sessions = make(map[string]*sessionState)
	}
	return user
}

func (g *DigestChallengeGuard) evictOldestNonceLocked(user *userState) {
	limit := g.cfg.MaxNoncesPerUser
	for len(user.NonceOrder) >= limit && len(user.NonceOrder) > 0 {
		oldest := user.NonceOrder[0]
		user.NonceOrder = user.NonceOrder[1:]
		delete(g.nonces, oldest)
	}
}

func (g *DigestChallengeGuard) activeNoncesLocked(username string, now time.Time) int {
	user := g.users[username]
	if user == nil {
		return 0
	}

	active := 0
	for _, value := range user.NonceOrder {
		record := g.nonces[value]
		if record == nil || record.Generation != g.generation {
			continue
		}
		if now.Before(record.ExpiresAt) {
			active++
		}
	}
	return active
}

func (g *DigestChallengeGuard) activeSessionsLocked(user *userState, now time.Time) int {
	active := 0
	for key, session := range user.Sessions {
		if session == nil || !now.Before(session.LastSeen.Add(g.cfg.NonceWindow)) {
			delete(user.Sessions, key)
			continue
		}
		active++
	}
	return active
}

func (g *DigestChallengeGuard) claimSessionLocked(user *userState, scope string, now time.Time) (int, error) {
	active := g.activeSessionsLocked(user, now)
	session := user.Sessions[scope]
	if session == nil {
		if active >= g.cfg.MaxSessionsPerUser {
			return active, ErrSessionLimit
		}
		user.Sessions[scope] = &sessionState{
			Key:       scope,
			CreatedAt: now,
			LastSeen:  now,
		}
		active++
		return active, nil
	}

	session.LastSeen = now
	return active, nil
}

func (g *DigestChallengeGuard) failureBlockedLocked(user *userState, now time.Time) bool {
	if user.Failures < g.cfg.MaxFailuresPerUser {
		return false
	}
	if user.FailureExpiresAt.IsZero() || now.Before(user.FailureExpiresAt) {
		return true
	}
	user.Failures = 0
	user.FailureExpiresAt = time.Time{}
	return false
}

func (g *DigestChallengeGuard) recordFailureLocked(user *userState, now time.Time) {
	if user.Failures == 0 || user.FailureExpiresAt.IsZero() || !now.Before(user.FailureExpiresAt) {
		user.Failures = 1
	} else {
		user.Failures++
	}
	user.FailureExpiresAt = now.Add(g.cfg.NonceWindow)
}

func (g *DigestChallengeGuard) resetFailuresLocked(user *userState) {
	user.Failures = 0
	user.FailureExpiresAt = time.Time{}
}

func validateQop(qop string) error {
	switch qop {
	case QopAuth, QopAuthInt:
		return nil
	default:
		return ErrQopRejected
	}
}

func watermarkScope(req AuthRequest) string {
	fingerprint := strings.TrimSpace(req.Fingerprint)
	cnonce := strings.TrimSpace(req.CNonce)
	if fingerprint == "" && cnonce == "" {
		return "shared"
	}
	return "fingerprint:" + fingerprint + "|cnonce:" + cnonce
}

func verifyDigest(req AuthRequest, password string) bool {
	given := strings.ToLower(strings.TrimSpace(req.Response))
	for _, newHash := range digestHashFactories(req.Algorithm) {
		expected := computeDigest(req, password, newHash)
		if hmac.Equal([]byte(given), []byte(expected)) {
			return true
		}
	}
	return false
}

func digestHashFactories(algorithm string) []func() hash.Hash {
	factories := []func() hash.Hash{sha256.New}
	if algorithm == AlgorithmSHA512256 {
		factories = append(factories, sha512.New512_256)
	}
	return factories
}

func computeDigest(req AuthRequest, password string, newHash func() hash.Hash) string {
	ha1 := hashHex(newHash, req.Username+":"+req.Realm+":"+password)

	ha2Text := req.Method + ":" + req.URI
	if req.Qop == QopAuthInt {
		bodyHash := hashBytesHex(newHash, req.Body)
		ha2Text += ":" + bodyHash
	}
	ha2 := hashHex(newHash, ha2Text)

	nc := fmt.Sprintf("%08x", req.NC)
	responseText := ha1 + ":" + req.Nonce + ":" + nc + ":" + req.CNonce + ":" + req.Qop + ":" + ha2
	return hashHex(newHash, responseText)
}

func hashHex(newHash func() hash.Hash, value string) string {
	return hashBytesHex(newHash, []byte(value))
}

func hashBytesHex(newHash func() hash.Hash, value []byte) string {
	h := newHash()
	_, _ = h.Write(value)
	return hex.EncodeToString(h.Sum(nil))
}

func removeString(values []string, target string) []string {
	filtered := values[:0]
	for _, value := range values {
		if value != target {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func decodeSnapshot(data []byte) (snapshotState, error) {
	var state snapshotState
	if len(data) == 0 {
		return state, fmt.Errorf("%w: empty payload", ErrBadSnapshot)
	}

	var envelope snapshotEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return state, fmt.Errorf("%w: invalid envelope: %v", ErrBadSnapshot, err)
	}
	if err := expectEOF(decoder); err != nil {
		return state, err
	}

	if envelope.Version != snapshotVersion {
		return state, fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, envelope.Version)
	}
	if len(envelope.Payload) == 0 || envelope.Checksum == "" {
		return state, fmt.Errorf("%w: missing payload or checksum", ErrBadSnapshot)
	}

	checksum, err := hex.DecodeString(envelope.Checksum)
	if err != nil {
		return state, fmt.Errorf("%w: invalid checksum encoding", ErrBadSnapshot)
	}
	actualChecksum := sha256.Sum256(envelope.Payload)
	if !hmac.Equal(checksum, actualChecksum[:]) {
		return state, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	payloadDecoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	payloadDecoder.DisallowUnknownFields()
	if err := payloadDecoder.Decode(&state); err != nil {
		return snapshotState{}, fmt.Errorf("%w: invalid state: %v", ErrBadSnapshot, err)
	}
	if err := expectEOF(payloadDecoder); err != nil {
		return snapshotState{}, err
	}
	if err := validateSnapshotState(&state); err != nil {
		return snapshotState{}, err
	}
	return state, nil
}

func expectEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: trailing payload: %v", ErrBadSnapshot, err)
	}
	return fmt.Errorf("%w: trailing payload", ErrBadSnapshot)
}

func validateSnapshotState(state *snapshotState) error {
	if state.Generation == 0 {
		return fmt.Errorf("%w: generation is missing", ErrBadSnapshot)
	}
	if len(state.Secret) < 16 {
		return fmt.Errorf("%w: secret is missing", ErrBadSnapshot)
	}

	maxSequence := uint64(0)
	for nonce, record := range state.Nonces {
		if record == nil {
			return fmt.Errorf("%w: nil nonce record", ErrBadSnapshot)
		}
		if record.Value != nonce {
			return fmt.Errorf("%w: nonce key mismatch", ErrBadSnapshot)
		}
		if strings.TrimSpace(record.Username) == "" || record.Realm == "" {
			return fmt.Errorf("%w: nonce identity is invalid", ErrBadSnapshot)
		}
		if record.Generation != state.Generation {
			return fmt.Errorf("%w: nonce generation is stale", ErrBadSnapshot)
		}
		if record.IssuedAt.IsZero() || !record.ExpiresAt.After(record.IssuedAt) {
			return fmt.Errorf("%w: nonce lifetime is invalid", ErrBadSnapshot)
		}
		if record.Watermark < 0 {
			return fmt.Errorf("%w: watermark is negative", ErrBadSnapshot)
		}
		for scope, watermark := range record.Watermarks {
			if strings.TrimSpace(scope) == "" || watermark < 0 {
				return fmt.Errorf("%w: device watermark is invalid", ErrBadSnapshot)
			}
		}
		if _, ok := state.Users[record.Username]; !ok {
			return fmt.Errorf("%w: nonce user is missing", ErrBadSnapshot)
		}
		if record.Sequence > maxSequence {
			maxSequence = record.Sequence
		}
	}

	for username, user := range state.Users {
		if user == nil || strings.TrimSpace(username) == "" {
			return fmt.Errorf("%w: user state is invalid", ErrBadSnapshot)
		}
		if user.Issued < 0 || user.Replays < 0 || user.MaxNC < 0 || user.Failures < 0 {
			return fmt.Errorf("%w: user counters are invalid", ErrBadSnapshot)
		}

		seen := make(map[string]bool, len(user.NonceOrder))
		for _, nonce := range user.NonceOrder {
			if seen[nonce] {
				return fmt.Errorf("%w: duplicate nonce order entry", ErrBadSnapshot)
			}
			seen[nonce] = true

			record, ok := state.Nonces[nonce]
			if !ok || record.Username != username {
				return fmt.Errorf("%w: nonce order is inconsistent", ErrBadSnapshot)
			}
		}
	}

	if state.NextSequence < maxSequence {
		return fmt.Errorf("%w: sequence is behind nonce records", ErrBadSnapshot)
	}
	return nil
}
