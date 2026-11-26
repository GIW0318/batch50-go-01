// Package task runs bidirectional forwarding detection sessions for tunnel
// endpoints, including detection parameter negotiation and key chain auth.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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

// BfdSessionGuard owns the sessions, the key chains and the event log.
type BfdSessionGuard struct {
	cfg GuardConfig
}

// NewBfdSessionGuard builds a guard with the given caps.
func NewBfdSessionGuard(cfg GuardConfig) *BfdSessionGuard {
	return &BfdSessionGuard{cfg: cfg}
}

// OpenSession starts one session in the Down state.
func (g *BfdSessionGuard) OpenSession(peerID string, cfg SessionConfig) (SessionID, error) {
	return "", errNotImplemented
}

// OnPacket feeds one received control packet into a session.
func (g *BfdSessionGuard) OnPacket(sessionID string, pkt ControlPacket) (Transition, error) {
	return Transition{}, errNotImplemented
}

// SetAuthChain replaces the key chain of one peer.
func (g *BfdSessionGuard) SetAuthChain(peerID string, chain []AuthKey) error {
	return errNotImplemented
}

// Tick advances every timer to now and returns the transitions that fired.
func (g *BfdSessionGuard) Tick(now time.Time) []Transition {
	return nil
}

// Session reads one session record.
func (g *BfdSessionGuard) Session(sessionID string) (Session, error) {
	return Session{}, errNotImplemented
}

// NegotiatedDetectMicros reports the detection time of one session.
func (g *BfdSessionGuard) NegotiatedDetectMicros(sessionID string) (int, error) {
	return 0, errNotImplemented
}

// FlapCount reports the suppressed flap count of one peer.
func (g *BfdSessionGuard) FlapCount(peerID string) int {
	return 0
}

// Events returns a copy of the transition log, oldest first.
func (g *BfdSessionGuard) Events() []Transition {
	return nil
}

// CloseSession tears one session down.
func (g *BfdSessionGuard) CloseSession(sessionID string) error {
	return errNotImplemented
}

// Snapshot serialises sessions, chains, flap counters and the event log.
func (g *BfdSessionGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *BfdSessionGuard) Restore(data []byte) error {
	return errNotImplemented
}

// SessionID is the identifier handed back by OpenSession.
type SessionID string
