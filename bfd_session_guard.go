// Package task runs bidirectional forwarding detection sessions for tunnel
// endpoints, including detection parameter negotiation and key chain auth.
package task

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session states.
const (
	StateDown      = "down"
	StateInit      = "init"
	StateUp        = "up"
	StateAdminDown = "admin_down"
)

// Transition reason codes.
const (
	ReasonNegotiated = "bfd_negotiated"
	ReasonPeerUp     = "bfd_peer_up"
	ReasonDetect     = "bfd_detect_timer"
	ReasonPeerReset  = "bfd_peer_reset"
	ReasonAdmin      = "bfd_admin_down"
	ReasonAuthOK     = "bfd_auth_ok"
)

// Errors surfaced by the guard.
var (
	ErrBadConfig      = errors.New("session configuration is invalid")
	ErrUnknownSession = errors.New("unknown bfd session")
	ErrPeerSessionCap = errors.New("peer session limit reached")
	ErrAuthKey        = errors.New("authentication key rejected")
	ErrBadChain       = errors.New("authentication chain is invalid")
	ErrChainTooLong   = errors.New("authentication chain too long")
	ErrBadPacket      = errors.New("control packet is invalid")
	ErrBadSnapshot    = errors.New("snapshot payload invalid")
)

// SessionConfig is the local detection configuration of a session.
type SessionConfig struct {
	DesiredTxMicros  int
	RequiredRxMicros int
	DetectMult       int
	LocalDisc        uint32
	AuthKeyID        int
}

// ControlPacket is one received control packet. AuthKeyID 0 means the packet
// carries no authentication section.
type ControlPacket struct {
	State            string
	DesiredTxMicros  int
	RequiredRxMicros int
	DetectMult       int
	LocalDisc        uint32
	AuthKeyID        int
	AuthDigest       string
	ReceivedAt       time.Time
}

// AuthKey is one generation of a peer key chain.
type AuthKey struct {
	ID         int
	Generation int
	Secret     string
	ValidFrom  time.Time
	ValidUntil time.Time
}

// Transition records one fired state change.
type Transition struct {
	SessionID string
	PeerID    string
	From      string
	To        string
	Reason    string
	At        time.Time
}

// Session is the live record of one peer session.
type Session struct {
	ID           string
	PeerID       string
	State        string
	TxMicros     int
	DetectMicros int
	AuthKeyID    int
	Diag         string
}

// GuardConfig bounds the live tables of the guard.
type GuardConfig struct {
	MaxSessionsPerPeer int
	MaxChainKeys       int
	MaxEvents          int
	MinDetectMult      int
	FlapWindow         time.Duration
}

type sessionRecord struct {
	id           SessionID
	peerID       string
	cfg          SessionConfig
	state        string
	txMicros     int
	detectMicros int
	authKeyID    int
	diag         string
	lastRx       time.Time
	hasRx        bool
	validRx      uint64
	authFailures uint64
}

type flapRecord struct {
	at     time.Time
	count  int
	reason string
}

// BfdSessionGuard owns the sessions, the key chains and the event log.
type BfdSessionGuard struct {
	mu sync.Mutex

	cfg GuardConfig

	sessions     map[SessionID]*sessionRecord
	sessionOrder []SessionID
	peerCounts   map[string]int
	chains       map[string][]AuthKey
	flaps        map[string]flapRecord
	events       []Transition
	nextID       uint64
}

// NewBfdSessionGuard builds a guard with the given caps.
func NewBfdSessionGuard(cfg GuardConfig) *BfdSessionGuard {
	return &BfdSessionGuard{
		cfg:          cfg,
		sessions:     make(map[SessionID]*sessionRecord),
		peerCounts:   make(map[string]int),
		chains:       make(map[string][]AuthKey),
		flaps:        make(map[string]flapRecord),
		sessionOrder: make([]SessionID, 0),
	}
}

// OpenSession starts one session in the Down state.
func (g *BfdSessionGuard) OpenSession(peerID string, cfg SessionConfig) (SessionID, error) {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" || !validSessionConfig(cfg) {
		return "", ErrBadConfig
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.peerCounts[peerID] >= g.cfg.MaxSessionsPerPeer {
		return "", ErrPeerSessionCap
	}

	detect, ok := detectMicros(cfg.DesiredTxMicros, cfg.RequiredRxMicros, cfg.DetectMult)
	if !ok {
		return "", ErrBadConfig
	}

	id := g.allocateSessionIDLocked()
	rec := &sessionRecord{
		id:           id,
		peerID:       peerID,
		cfg:          cfg,
		state:        StateDown,
		txMicros:     cfg.DesiredTxMicros,
		detectMicros: detect,
		authKeyID:    cfg.AuthKeyID,
	}
	g.sessions[id] = rec
	g.sessionOrder = append(g.sessionOrder, id)
	g.peerCounts[peerID]++

	return id, nil
}

// OnPacket feeds one received control packet into a session.
func (g *BfdSessionGuard) OnPacket(sessionID string, pkt ControlPacket) (Transition, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	rec, ok := g.sessions[SessionID(sessionID)]
	if !ok {
		return Transition{}, ErrUnknownSession
	}

	if pkt.AuthKeyID != 0 {
		if err := g.authenticateLocked(rec.peerID, pkt); err != nil {
			rec.authFailures++
			return Transition{}, ErrAuthKey
		}
	}

	if !validState(pkt.State) {
		return Transition{}, ErrBadPacket
	}
	if pkt.DesiredTxMicros <= 0 || pkt.RequiredRxMicros <= 0 || pkt.DetectMult <= 0 {
		return Transition{}, ErrBadPacket
	}

	// A peer may not negotiate below the local safety floor. Keep the
	// previously agreed timer and do not treat this packet as a live,
	// negotiable control packet.
	if g.cfg.MinDetectMult > 0 && pkt.DetectMult < g.cfg.MinDetectMult {
		rec.diag = ReasonNegotiated
		return Transition{}, nil
	}

	detect, ok := detectMicros(rec.cfg.DesiredTxMicros, pkt.RequiredRxMicros, pkt.DetectMult)
	if !ok {
		return Transition{}, ErrBadPacket
	}
	rec.detectMicros = detect
	if pkt.AuthKeyID != 0 {
		rec.authKeyID = pkt.AuthKeyID
	}

	rec.validRx++
	if !rec.hasRx || pkt.ReceivedAt.After(rec.lastRx) {
		rec.lastRx = pkt.ReceivedAt
		rec.hasRx = true
	}

	to, reason := nextTransition(rec.state, pkt.State)
	if to == "" {
		return Transition{}, nil
	}
	return g.transitionLocked(rec, to, reason, pkt.ReceivedAt), nil
}

// SetAuthChain replaces the key chain of one peer.
func (g *BfdSessionGuard) SetAuthChain(peerID string, chain []AuthKey) error {
	peerID = strings.TrimSpace(peerID)
	if err := validateChain(chain, g.cfg.MaxChainKeys); err != nil {
		return err
	}
	if peerID == "" {
		return ErrBadChain
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.chains[peerID] = cloneChain(chain)
	return nil
}

// Tick advances every timer to now and returns the transitions that fired.
func (g *BfdSessionGuard) Tick(now time.Time) []Transition {
	g.mu.Lock()
	defer g.mu.Unlock()

	var fired []Transition
	for _, id := range g.sessionOrder {
		rec, ok := g.sessions[id]
		if !ok || !rec.hasRx {
			continue
		}
		if rec.state != StateInit && rec.state != StateUp {
			continue
		}
		if rec.detectMicros <= 0 || now.Before(rec.lastRx) {
			continue
		}
		detectDuration, ok := detectDuration(rec.detectMicros)
		if !ok || now.Sub(rec.lastRx) < detectDuration {
			continue
		}
		fired = append(fired, g.transitionLocked(rec, StateDown, ReasonDetect, now))
	}
	return fired
}

// Session reads one session record.
func (g *BfdSessionGuard) Session(sessionID string) (Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	rec, ok := g.sessions[SessionID(sessionID)]
	if !ok {
		return Session{}, ErrUnknownSession
	}
	return publicSession(rec), nil
}

// NegotiatedDetectMicros reports the detection time of one session.
func (g *BfdSessionGuard) NegotiatedDetectMicros(sessionID string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	rec, ok := g.sessions[SessionID(sessionID)]
	if !ok {
		return 0, ErrUnknownSession
	}
	return rec.detectMicros, nil
}

// FlapCount reports the suppressed flap count of one peer.
func (g *BfdSessionGuard) FlapCount(peerID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.flaps[peerID].count
}

// Events returns a copy of the transition log, oldest first.
func (g *BfdSessionGuard) Events() []Transition {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.events) == 0 {
		return []Transition{}
	}
	return append([]Transition(nil), g.events...)
}

// CloseSession tears one session down.
func (g *BfdSessionGuard) CloseSession(sessionID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := SessionID(sessionID)
	rec, ok := g.sessions[id]
	if !ok {
		return ErrUnknownSession
	}

	if rec.state != StateAdminDown {
		_ = g.transitionLocked(rec, StateAdminDown, ReasonAdmin, time.Now())
	}

	delete(g.sessions, id)
	for i, existing := range g.sessionOrder {
		if existing == id {
			g.sessionOrder = append(g.sessionOrder[:i], g.sessionOrder[i+1:]...)
			break
		}
	}
	if g.peerCounts[rec.peerID] <= 1 {
		delete(g.peerCounts, rec.peerID)
	} else {
		g.peerCounts[rec.peerID]--
	}
	return nil
}

const snapshotVersion = 1

type snapshotSession struct {
	ID           SessionID     `json:"id"`
	PeerID       string        `json:"peer_id"`
	Config       SessionConfig `json:"config"`
	State        string        `json:"state"`
	TxMicros     int           `json:"tx_micros"`
	DetectMicros int           `json:"detect_micros"`
	AuthKeyID    int           `json:"auth_key_id"`
	Diag         string        `json:"diag,omitempty"`
	LastRx       time.Time     `json:"last_rx"`
	HasRx        bool          `json:"has_rx"`
	ValidRx      uint64        `json:"valid_rx"`
	AuthFailures uint64        `json:"auth_failures"`
}

type snapshotFlap struct {
	At     time.Time `json:"at"`
	Count  int       `json:"count"`
	Reason string    `json:"reason"`
}

type snapshotData struct {
	Version  int                     `json:"version"`
	NextID   uint64                  `json:"next_id"`
	Sessions []snapshotSession       `json:"sessions"`
	Chains   map[string][]AuthKey    `json:"chains"`
	Flaps    map[string]snapshotFlap `json:"flaps"`
	Events   []Transition            `json:"events"`
}

// Snapshot serialises sessions, chains, flap counters and the event log.
func (g *BfdSessionGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()

	data := snapshotData{
		Version:  snapshotVersion,
		NextID:   g.nextID,
		Sessions: make([]snapshotSession, 0, len(g.sessionOrder)),
		Chains:   make(map[string][]AuthKey, len(g.chains)),
		Flaps:    make(map[string]snapshotFlap, len(g.flaps)),
		Events:   append([]Transition(nil), g.events...),
	}
	for _, id := range g.sessionOrder {
		rec, ok := g.sessions[id]
		if !ok {
			continue
		}
		data.Sessions = append(data.Sessions, snapshotSession{
			ID:           rec.id,
			PeerID:       rec.peerID,
			Config:       rec.cfg,
			State:        rec.state,
			TxMicros:     rec.txMicros,
			DetectMicros: rec.detectMicros,
			AuthKeyID:    rec.authKeyID,
			Diag:         rec.diag,
			LastRx:       rec.lastRx,
			HasRx:        rec.hasRx,
			ValidRx:      rec.validRx,
			AuthFailures: rec.authFailures,
		})
	}
	for peer, chain := range g.chains {
		data.Chains[peer] = cloneChain(chain)
	}
	for peer, flap := range g.flaps {
		data.Flaps[peer] = snapshotFlap{At: flap.at, Count: flap.count, Reason: flap.reason}
	}
	g.mu.Unlock()

	return json.Marshal(data)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *BfdSessionGuard) Restore(data []byte) error {
	var snap snapshotData
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}

	sessions := make(map[SessionID]*sessionRecord, len(snap.Sessions))
	order := make([]SessionID, 0, len(snap.Sessions))
	peerCounts := make(map[string]int)
	seenIDs := make(map[SessionID]struct{}, len(snap.Sessions))
	for _, stored := range snap.Sessions {
		if stored.ID == "" {
			return fmt.Errorf("%w: empty session id", ErrBadSnapshot)
		}
		if _, exists := seenIDs[stored.ID]; exists {
			return fmt.Errorf("%w: duplicate session id", ErrBadSnapshot)
		}
		peerID := strings.TrimSpace(stored.PeerID)
		if peerID == "" || !validSessionConfig(stored.Config) || !validState(stored.State) {
			return fmt.Errorf("%w: invalid session record", ErrBadSnapshot)
		}
		if stored.TxMicros <= 0 || stored.DetectMicros <= 0 {
			return fmt.Errorf("%w: invalid session timers", ErrBadSnapshot)
		}
		if peerCounts[peerID] >= g.cfg.MaxSessionsPerPeer {
			return fmt.Errorf("%w: session cap exceeded", ErrBadSnapshot)
		}
		seenIDs[stored.ID] = struct{}{}
		peerCounts[peerID]++
		order = append(order, stored.ID)
		sessions[stored.ID] = &sessionRecord{
			id:           stored.ID,
			peerID:       peerID,
			cfg:          stored.Config,
			state:        stored.State,
			txMicros:     stored.TxMicros,
			detectMicros: stored.DetectMicros,
			authKeyID:    stored.AuthKeyID,
			diag:         stored.Diag,
			lastRx:       stored.LastRx,
			hasRx:        stored.HasRx,
			validRx:      stored.ValidRx,
			authFailures: stored.AuthFailures,
		}
	}

	chains := make(map[string][]AuthKey, len(snap.Chains))
	for peer, chain := range snap.Chains {
		peer = strings.TrimSpace(peer)
		if peer == "" {
			return fmt.Errorf("%w: invalid chain peer", ErrBadSnapshot)
		}
		if err := validateChain(chain, g.cfg.MaxChainKeys); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
		chains[peer] = cloneChain(chain)
	}

	flaps := make(map[string]flapRecord, len(snap.Flaps))
	for peer, flap := range snap.Flaps {
		if strings.TrimSpace(peer) == "" || flap.Count < 0 {
			return fmt.Errorf("%w: invalid flap record", ErrBadSnapshot)
		}
		flaps[peer] = flapRecord{at: flap.At, count: flap.Count, reason: flap.Reason}
	}

	for _, event := range snap.Events {
		if event.SessionID == "" || strings.TrimSpace(event.PeerID) == "" ||
			!validState(event.From) || !validState(event.To) || event.Reason == "" {
			return fmt.Errorf("%w: invalid event record", ErrBadSnapshot)
		}
	}
	events := append([]Transition(nil), snap.Events...)
	events = boundEvents(events, g.cfg.MaxEvents)

	nextID := snap.NextID
	if nextID == 0 {
		for id := range sessions {
			if n, ok := parseSessionNumber(id); ok && n >= nextID {
				nextID = n + 1
			}
		}
	}
	for {
		candidate := SessionID("sess-" + strconv.FormatUint(nextID, 10))
		if _, exists := sessions[candidate]; !exists && nextID != 0 {
			break
		}
		nextID++
	}

	g.mu.Lock()
	g.sessions = sessions
	g.sessionOrder = order
	g.peerCounts = peerCounts
	g.chains = chains
	g.flaps = flaps
	g.events = events
	g.nextID = nextID
	g.mu.Unlock()

	return nil
}

func (g *BfdSessionGuard) authenticateLocked(peerID string, pkt ControlPacket) error {
	chain, ok := g.chains[peerID]
	if !ok {
		return ErrAuthKey
	}
	for i := range chain {
		key := &chain[i]
		if key.ID != pkt.AuthKeyID {
			continue
		}
		if !key.ValidFrom.IsZero() && pkt.ReceivedAt.Before(key.ValidFrom) {
			return ErrAuthKey
		}
		if !key.ValidUntil.IsZero() && pkt.ReceivedAt.After(key.ValidUntil) {
			return ErrAuthKey
		}
		if subtle.ConstantTimeCompare([]byte(key.Secret), []byte(pkt.AuthDigest)) != 1 {
			return ErrAuthKey
		}
		return nil
	}
	return ErrAuthKey
}

func (g *BfdSessionGuard) transitionLocked(rec *sessionRecord, to, reason string, at time.Time) Transition {
	from := rec.state
	if from == to {
		return Transition{}
	}
	rec.state = to
	rec.diag = reason
	tr := Transition{
		SessionID: string(rec.id),
		PeerID:    rec.peerID,
		From:      from,
		To:        to,
		Reason:    reason,
		At:        at,
	}
	g.appendEventLocked(tr)
	if from == StateUp && to == StateDown {
		g.recordFlapLocked(rec.peerID, at, reason)
	}
	return tr
}

func (g *BfdSessionGuard) appendEventLocked(tr Transition) {
	if g.cfg.MaxEvents <= 0 {
		return
	}
	g.events = append(g.events, tr)
	if len(g.events) > g.cfg.MaxEvents {
		g.events = append([]Transition(nil), g.events[len(g.events)-g.cfg.MaxEvents:]...)
	}
}

func (g *BfdSessionGuard) recordFlapLocked(peerID string, at time.Time, reason string) {
	rec := g.flaps[peerID]
	if rec.count > 0 && g.cfg.FlapWindow > 0 && at.Sub(rec.at) < g.cfg.FlapWindow {
		return
	}
	rec.at = at
	rec.count++
	rec.reason = reason
	g.flaps[peerID] = rec
}

func (g *BfdSessionGuard) allocateSessionIDLocked() SessionID {
	for {
		g.nextID++
		id := SessionID("sess-" + strconv.FormatUint(g.nextID, 10))
		if _, exists := g.sessions[id]; !exists {
			return id
		}
	}
}

func nextTransition(current, packetState string) (to, reason string) {
	if packetState == StateAdminDown {
		if current == StateAdminDown {
			return "", ""
		}
		return StateAdminDown, ReasonAdmin
	}

	if current == StateAdminDown {
		return "", ""
	}

	switch current {
	case StateDown:
		return StateInit, ReasonNegotiated
	case StateInit:
		if packetState == StateDown {
			return StateDown, ReasonPeerReset
		}
		return StateUp, ReasonPeerUp
	case StateUp:
		if packetState == StateDown {
			return StateDown, ReasonPeerReset
		}
	}
	return "", ""
}

func validSessionConfig(cfg SessionConfig) bool {
	return cfg.DesiredTxMicros > 0 && cfg.RequiredRxMicros > 0 && cfg.DetectMult > 0
}

func validState(state string) bool {
	switch state {
	case StateDown, StateInit, StateUp, StateAdminDown:
		return true
	default:
		return false
	}
}

func detectMicros(localTx, peerRx, mult int) (int, bool) {
	if localTx <= 0 || peerRx <= 0 || mult <= 0 {
		return 0, false
	}
	interval := localTx
	if peerRx > interval {
		interval = peerRx
	}
	value := int64(interval) * int64(mult)
	maxInt := int64(^uint(0) >> 1)
	if value <= 0 || value > maxInt {
		return 0, false
	}
	return int(value), true
}

func detectDuration(micros int) (time.Duration, bool) {
	if micros <= 0 || int64(micros) > int64(^uint64(0)>>1)/int64(time.Microsecond) {
		return 0, false
	}
	return time.Duration(micros) * time.Microsecond, true
}

func validateChain(chain []AuthKey, maxKeys int) error {
	if len(chain) == 0 {
		return ErrBadChain
	}
	if len(chain) > maxKeys {
		return ErrChainTooLong
	}
	seen := make(map[int]struct{}, len(chain))
	for _, key := range chain {
		if key.ID <= 0 || key.Secret == "" {
			return ErrBadChain
		}
		if _, exists := seen[key.ID]; exists {
			return ErrBadChain
		}
		if !key.ValidFrom.IsZero() && !key.ValidUntil.IsZero() && key.ValidUntil.Before(key.ValidFrom) {
			return ErrBadChain
		}
		seen[key.ID] = struct{}{}
	}
	return nil
}

func cloneChain(chain []AuthKey) []AuthKey {
	return append([]AuthKey(nil), chain...)
}

func publicSession(rec *sessionRecord) Session {
	return Session{
		ID:           string(rec.id),
		PeerID:       rec.peerID,
		State:        rec.state,
		TxMicros:     rec.txMicros,
		DetectMicros: rec.detectMicros,
		AuthKeyID:    rec.authKeyID,
		Diag:         rec.diag,
	}
}

func boundEvents(events []Transition, maxEvents int) []Transition {
	if maxEvents <= 0 {
		return []Transition{}
	}
	if len(events) > maxEvents {
		return append([]Transition(nil), events[len(events)-maxEvents:]...)
	}
	return append([]Transition(nil), events...)
}

func parseSessionNumber(id SessionID) (uint64, bool) {
	text := string(id)
	if !strings.HasPrefix(text, "sess-") {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(text, "sess-"), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}

// SessionID is the identifier handed back by OpenSession.
type SessionID string
