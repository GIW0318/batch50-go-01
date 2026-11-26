// Package task guards an NTS association between a client and a time server:
// exported key material, the one-shot cookie pool and server rotation.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCookiesPerExchange is the pool size handed out by a key exchange.
const DefaultCookiesPerExchange = 8

// Verdict reasons.
const (
	ReasonCookieReplay    = "cookie_replay"
	ReasonUnknownCookie   = "unknown_cookie"
	ReasonTimestampWindow = "timestamp_out_of_window"
	ReasonAlgorithm       = "algorithm_mismatch"
	ReasonUniqueID        = "unique_id_mismatch"
	ReasonServerChanged   = "server_changed"
	ReasonAssociationGone = "association_gone"
)

// Errors surfaced by the guard.
var (
	ErrBlankServer            = errors.New("server address is required")
	ErrBadNonce               = errors.New("request nonce must be 16 bytes")
	ErrPendingRequest         = errors.New("a key exchange request is already pending")
	ErrNoPendingRequest       = errors.New("no key exchange request is pending")
	ErrNonceMismatch          = errors.New("response nonce does not match the request")
	ErrReplayNonce            = errors.New("request nonce was already used")
	ErrTimestampWindow        = errors.New("response timestamp is outside the window")
	ErrAlgorithmNotNegotiated = errors.New("algorithm was not negotiated")
	ErrBadKeyMaterial         = errors.New("exported key material is not 32 bytes")
	ErrCookieCount            = errors.New("response does not carry the agreed cookie count")
	ErrCookieExhausted        = errors.New("cookie pool is exhausted")
	ErrNoAssociation          = errors.New("no association is established")
	ErrReestablishRequired    = errors.New("association must be re-established")
	ErrBadSnapshot            = errors.New("snapshot payload is invalid")
)

// KeRequest is one NTS-KE request kept pending until the response arrives.
type KeRequest struct {
	Server    string
	Nonce     []byte
	SentAt    time.Time
	NextProto []string
}

// Cookie is one NTS cookie of the pool.
type Cookie struct {
	ID   []byte
	Body []byte
}

// KeResponse is the NTS-KE response as received from the server.
type KeResponse struct {
	Server       string
	RequestNonce []byte
	UniqueID     []byte
	Timestamp    time.Time
	Algorithm    string
	C2SKey       []byte
	S2CKey       []byte
	Cookies      []Cookie
	Negotiated   []string
}

// Association is the live security association with one time server.
type Association struct {
	Server        string
	Algorithm     string
	UniqueID      []byte
	Cookies       int
	Generation    int
	EstablishedAt time.Time
	NeedsRekey    bool
}

// Packet is one authenticated NTP packet offered to the guard.
type Packet struct {
	Server    string
	CookieID  []byte
	UniqueID  []byte
	Timestamp time.Time
	Algorithm string
}

// Verdict is the answer for one packet.
type Verdict struct {
	Accept     bool
	Reason     string
	Remaining  int
	NeedsRekey bool
}

// NtsConfig bounds and wires the guard.
type NtsConfig struct {
	AllowedAlgorithms  []string
	CookiesPerExchange int
	LowWatermark       int
	SkewWindow         time.Duration
	PoolCap            int
	Now                func() time.Time
}

// NtsStats counts association activity.
type NtsStats struct {
	Established int
	Rekeys      int
	CookieUsed  int
	Rejected    int
}

type associationState struct {
	server        string
	algorithm     string
	uniqueID      []byte
	c2sKey        []byte
	s2cKey        []byte
	generation    int
	establishedAt time.Time
}

// NtsKeyExchangeGuard owns the pending request, the association and the pool.
type NtsKeyExchangeGuard struct {
	cfg NtsConfig
	mu  sync.Mutex

	pending      *KeRequest
	association  *associationState
	pool         []Cookie
	used         map[string]struct{}
	issued       map[string]struct{}
	replayNonces map[string]struct{}
	generation   int
	stats        NtsStats
}

// NewNtsKeyExchangeGuard builds a guard with the given bounds.
func NewNtsKeyExchangeGuard(cfg NtsConfig) *NtsKeyExchangeGuard {
	if cfg.CookiesPerExchange <= 0 {
		cfg.CookiesPerExchange = DefaultCookiesPerExchange
	}
	if cfg.PoolCap <= 0 {
		cfg.PoolCap = cfg.CookiesPerExchange
	}
	if cfg.CookiesPerExchange > cfg.PoolCap {
		cfg.CookiesPerExchange = cfg.PoolCap
	}
	if cfg.LowWatermark <= 0 {
		cfg.LowWatermark = 2
	}
	if cfg.SkewWindow <= 0 {
		cfg.SkewWindow = 2 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.AllowedAlgorithms = append([]string(nil), cfg.AllowedAlgorithms...)
	return &NtsKeyExchangeGuard{
		cfg:          cfg,
		used:         make(map[string]struct{}),
		issued:       make(map[string]struct{}),
		replayNonces: make(map[string]struct{}),
	}
}

// Begin records one key exchange request and returns its nonce.
func (g *NtsKeyExchangeGuard) Begin(server string, nonce []byte) (KeRequest, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return KeRequest{}, ErrBlankServer
	}
	if len(nonce) != 16 {
		return KeRequest{}, ErrBadNonce
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.pending != nil {
		return KeRequest{}, ErrPendingRequest
	}
	nonceKey := string(nonce)
	if _, ok := g.replayNonces[nonceKey]; ok {
		return KeRequest{}, ErrReplayNonce
	}

	g.replayNonces[nonceKey] = struct{}{}
	req := KeRequest{
		Server:    server,
		Nonce:     cloneBytes(nonce),
		SentAt:    g.cfg.Now(),
		NextProto: append([]string(nil), g.cfg.AllowedAlgorithms...),
	}
	g.pending = cloneRequest(req)
	return *cloneRequest(req), nil
}

// Establish validates a response and installs the association and cookie pool.
func (g *NtsKeyExchangeGuard) Establish(res KeResponse) (Association, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.pending == nil {
		return Association{}, ErrNoPendingRequest
	}
	if !bytes.Equal(res.RequestNonce, g.pending.Nonce) {
		return Association{}, ErrNonceMismatch
	}

	request := *g.pending

	if strings.TrimSpace(res.Server) != request.Server {
		g.pending = nil
		g.invalidateAssociationLocked()
		return Association{}, ErrReestablishRequired
	}
	if !g.algorithmAllowedLocked(res.Algorithm) {
		g.pending = nil
		g.invalidateAssociationLocked()
		return Association{}, ErrAlgorithmNotNegotiated
	}
	if len(res.C2SKey) != 32 || len(res.S2CKey) != 32 {
		return Association{}, ErrBadKeyMaterial
	}
	if len(res.Cookies) != g.cfg.CookiesPerExchange {
		return Association{}, ErrCookieCount
	}

	seenCookies := make(map[string]struct{}, len(res.Cookies))
	cookies := make([]Cookie, len(res.Cookies))
	for i, cookie := range res.Cookies {
		key := string(cookie.ID)
		if _, duplicate := seenCookies[key]; duplicate {
			return Association{}, ErrCookieCount
		}
		seenCookies[key] = struct{}{}
		cookies[i] = cloneCookie(cookie)
	}

	now := g.cfg.Now()
	if outsideWindow(now, res.Timestamp, g.cfg.SkewWindow) {
		g.pending = nil
		return Association{}, ErrTimestampWindow
	}

	g.pending = nil
	hadAssociation := g.association != nil
	g.generation++
	g.association = &associationState{
		server:        request.Server,
		algorithm:     res.Algorithm,
		uniqueID:      cloneBytes(res.UniqueID),
		c2sKey:        cloneBytes(res.C2SKey),
		s2cKey:        cloneBytes(res.S2CKey),
		generation:    g.generation,
		establishedAt: now,
	}
	g.pool = cookies
	g.used = make(map[string]struct{}, len(cookies))
	g.issued = make(map[string]struct{}, len(cookies))
	g.stats.Established++
	if hadAssociation {
		g.stats.Rekeys++
	}

	return g.associationViewLocked(), nil
}

// TakeCookie removes one unused cookie from the pool for a single request.
func (g *NtsKeyExchangeGuard) TakeCookie() (Cookie, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.association == nil {
		return Cookie{}, ErrNoAssociation
	}
	if len(g.pool) == 0 {
		return Cookie{}, ErrCookieExhausted
	}

	cookie := g.pool[0]
	g.pool[0] = Cookie{}
	g.pool = g.pool[1:]
	key := string(cookie.ID)
	g.used[key] = struct{}{}
	g.issued[key] = struct{}{}
	g.stats.CookieUsed++

	return cloneCookie(cookie), nil
}

// Authenticate checks one packet against the association and the used cookies.
func (g *NtsKeyExchangeGuard) Authenticate(pkt Packet) (Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.association == nil {
		return Verdict{}, ErrNoAssociation
	}
	if strings.TrimSpace(pkt.Server) != g.association.server {
		return g.rejectLocked(ReasonServerChanged), nil
	}
	if !bytes.Equal(pkt.UniqueID, g.association.uniqueID) {
		return g.rejectLocked(ReasonUniqueID), nil
	}
	if pkt.Algorithm != g.association.algorithm {
		return g.rejectLocked(ReasonAlgorithm), nil
	}
	if outsideWindow(g.cfg.Now(), pkt.Timestamp, g.cfg.SkewWindow) {
		return g.rejectLocked(ReasonTimestampWindow), nil
	}

	key := string(pkt.CookieID)
	if _, ok := g.issued[key]; !ok {
		if _, wasUsed := g.used[key]; wasUsed {
			return g.rejectLocked(ReasonCookieReplay), nil
		}
		return g.rejectLocked(ReasonUnknownCookie), nil
	}

	delete(g.issued, key)
	return Verdict{
		Accept:     true,
		Remaining:  len(g.pool),
		NeedsRekey: g.needsRekeyLocked(),
	}, nil
}

// Rotate voids the association when the server endpoint changes.
func (g *NtsKeyExchangeGuard) Rotate(server string) error {
	server = strings.TrimSpace(server)

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.association == nil {
		return ErrNoAssociation
	}
	if server == g.association.server {
		return nil
	}

	g.pending = nil
	g.invalidateAssociationLocked()
	return ErrReestablishRequired
}

// PoolSize reports how many unused cookies remain.
func (g *NtsKeyExchangeGuard) PoolSize() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pool)
}

// Status reads the live association.
func (g *NtsKeyExchangeGuard) Status() (Association, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.association == nil {
		return Association{}, ErrNoAssociation
	}
	return g.associationViewLocked(), nil
}

// Stats reports counters of the guard.
func (g *NtsKeyExchangeGuard) Stats() NtsStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

const (
	snapshotMagic   = "gsb-nts-key-exchange-guard"
	snapshotVersion = 1
)

type snapshotEnvelope struct {
	Magic    string `json:"magic"`
	Version  int    `json:"version"`
	Payload  []byte `json:"payload"`
	Checksum string `json:"checksum"`
}

type guardSnapshot struct {
	Generation  int                `json:"generation"`
	Association *associationRecord `json:"association,omitempty"`
	Pool        []Cookie           `json:"pool"`
	UsedIDs     [][]byte           `json:"used_ids"`
	ReplayNonce [][]byte           `json:"replay_nonces,omitempty"`
	Pending     *KeRequest         `json:"pending,omitempty"`
	Stats       NtsStats           `json:"stats"`
}

type associationRecord struct {
	Server        string    `json:"server"`
	Algorithm     string    `json:"algorithm"`
	UniqueID      []byte    `json:"unique_id"`
	C2SKey        []byte    `json:"c2s_key"`
	S2CKey        []byte    `json:"s2c_key"`
	Generation    int       `json:"generation"`
	EstablishedAt time.Time `json:"established_at"`
}

// Snapshot serialises the association, the pool and the used cookie ids.
func (g *NtsKeyExchangeGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state := guardSnapshot{
		Generation:  g.generation,
		Stats:       g.stats,
		Pool:        cloneCookies(g.pool),
		UsedIDs:     sortedByteSlices(g.used),
		ReplayNonce: sortedByteSlices(g.replayNonces),
	}
	if g.association != nil {
		state.Association = &associationRecord{
			Server:        g.association.server,
			Algorithm:     g.association.algorithm,
			UniqueID:      cloneBytes(g.association.uniqueID),
			C2SKey:        cloneBytes(g.association.c2sKey),
			S2CKey:        cloneBytes(g.association.s2cKey),
			Generation:    g.association.generation,
			EstablishedAt: g.association.establishedAt,
		}
	}
	if g.pending != nil {
		state.Pending = cloneRequest(*g.pending)
	}

	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Payload:  payload,
		Checksum: hex.EncodeToString(sum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *NtsKeyExchangeGuard) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion || len(envelope.Payload) == 0 {
		return ErrBadSnapshot
	}
	checksum, err := hex.DecodeString(envelope.Checksum)
	if err != nil || len(checksum) != sha256.Size {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Payload)
	if !bytes.Equal(checksum, sum[:]) {
		return ErrBadSnapshot
	}

	var state guardSnapshot
	if err := json.Unmarshal(envelope.Payload, &state); err != nil {
		return ErrBadSnapshot
	}
	if err := g.validateSnapshot(state); err != nil {
		return ErrBadSnapshot
	}

	restoredUsed := make(map[string]struct{}, len(state.UsedIDs))
	for _, id := range state.UsedIDs {
		restoredUsed[string(id)] = struct{}{}
	}
	restoredReplayNonces := make(map[string]struct{}, len(state.ReplayNonce))
	for _, nonce := range state.ReplayNonce {
		restoredReplayNonces[string(nonce)] = struct{}{}
	}

	var restoredAssociation *associationState
	if state.Association != nil {
		restoredAssociation = &associationState{
			server:        state.Association.Server,
			algorithm:     state.Association.Algorithm,
			uniqueID:      cloneBytes(state.Association.UniqueID),
			c2sKey:        cloneBytes(state.Association.C2SKey),
			s2cKey:        cloneBytes(state.Association.S2CKey),
			generation:    state.Association.Generation,
			establishedAt: state.Association.EstablishedAt,
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.generation = state.Generation
	g.association = restoredAssociation
	g.pool = cloneCookies(state.Pool)
	g.used = restoredUsed
	g.issued = make(map[string]struct{})
	g.replayNonces = restoredReplayNonces
	g.pending = cloneRequestPointer(state.Pending)
	g.stats = state.Stats
	return nil
}

func (g *NtsKeyExchangeGuard) validateSnapshot(state guardSnapshot) error {
	if state.Generation < 0 ||
		state.Stats.Established < 0 ||
		state.Stats.Rekeys < 0 ||
		state.Stats.CookieUsed < 0 ||
		state.Stats.Rejected < 0 {
		return ErrBadSnapshot
	}

	for _, nonce := range state.ReplayNonce {
		if len(nonce) != 16 {
			return ErrBadSnapshot
		}
	}
	if state.Pending != nil {
		if strings.TrimSpace(state.Pending.Server) == "" || len(state.Pending.Nonce) != 16 {
			return ErrBadSnapshot
		}
	}

	poolIDs := make(map[string]struct{}, len(state.Pool))
	for _, cookie := range state.Pool {
		key := string(cookie.ID)
		if _, duplicate := poolIDs[key]; duplicate {
			return ErrBadSnapshot
		}
		poolIDs[key] = struct{}{}
	}

	usedIDs := make(map[string]struct{}, len(state.UsedIDs))
	for _, id := range state.UsedIDs {
		key := string(id)
		if _, duplicate := usedIDs[key]; duplicate {
			return ErrBadSnapshot
		}
		usedIDs[key] = struct{}{}
	}
	for key := range poolIDs {
		if _, consumed := usedIDs[key]; consumed {
			return ErrBadSnapshot
		}
	}

	if state.Association == nil {
		if len(state.Pool) != 0 || len(state.UsedIDs) != 0 {
			return ErrBadSnapshot
		}
		return nil
	}
	if strings.TrimSpace(state.Association.Server) == "" ||
		state.Association.Algorithm == "" ||
		state.Association.Generation < 1 ||
		state.Association.Generation > state.Generation ||
		len(state.Association.C2SKey) != 32 ||
		len(state.Association.S2CKey) != 32 {
		return ErrBadSnapshot
	}
	if len(state.Pool)+len(state.UsedIDs) != g.cfg.CookiesPerExchange {
		return ErrBadSnapshot
	}
	return nil
}

func (g *NtsKeyExchangeGuard) algorithmAllowedLocked(algorithm string) bool {
	for _, allowed := range g.cfg.AllowedAlgorithms {
		if algorithm == allowed {
			return true
		}
	}
	return false
}

func (g *NtsKeyExchangeGuard) associationViewLocked() Association {
	return Association{
		Server:        g.association.server,
		Algorithm:     g.association.algorithm,
		UniqueID:      cloneBytes(g.association.uniqueID),
		Cookies:       len(g.pool),
		Generation:    g.association.generation,
		EstablishedAt: g.association.establishedAt,
		NeedsRekey:    g.needsRekeyLocked(),
	}
}

func (g *NtsKeyExchangeGuard) needsRekeyLocked() bool {
	return g.association != nil && len(g.pool) <= g.cfg.LowWatermark
}

func (g *NtsKeyExchangeGuard) rejectLocked(reason string) Verdict {
	g.stats.Rejected++
	return Verdict{
		Accept:     false,
		Reason:     reason,
		Remaining:  len(g.pool),
		NeedsRekey: g.needsRekeyLocked(),
	}
}

func (g *NtsKeyExchangeGuard) invalidateAssociationLocked() {
	g.association = nil
	g.pool = nil
	g.used = make(map[string]struct{})
	g.issued = make(map[string]struct{})
}

func outsideWindow(now, at time.Time, window time.Duration) bool {
	diff := now.Sub(at)
	if diff < 0 {
		diff = -diff
	}
	return diff > window
}

func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	return append([]byte(nil), src...)
}

func cloneCookie(cookie Cookie) Cookie {
	return Cookie{ID: cloneBytes(cookie.ID), Body: cloneBytes(cookie.Body)}
}

func cloneCookies(cookies []Cookie) []Cookie {
	if cookies == nil {
		return nil
	}
	out := make([]Cookie, len(cookies))
	for i, cookie := range cookies {
		out[i] = cloneCookie(cookie)
	}
	return out
}

func cloneRequest(req KeRequest) *KeRequest {
	return &KeRequest{
		Server:    req.Server,
		Nonce:     cloneBytes(req.Nonce),
		SentAt:    req.SentAt,
		NextProto: append([]string(nil), req.NextProto...),
	}
}

func cloneRequestPointer(req *KeRequest) *KeRequest {
	if req == nil {
		return nil
	}
	return cloneRequest(*req)
}

func sortedByteSlices(set map[string]struct{}) [][]byte {
	out := make([][]byte, 0, len(set))
	for value := range set {
		out = append(out, []byte(value))
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i], out[j]) < 0
	})
	return out
}
