// Package task validates DNS COOKIE options at a resolver ingress and issues
// server cookies across key generations.
package task

import (
	"errors"
	"net/netip"
	"sync"
	"time"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Verdict actions.
const (
	ActionAccept    = "accept"
	ActionBadCookie = "badcookie"
	ActionDrop      = "drop"
)

// Verdict reasons.
const (
	CauseCookieAbsent = "cookie_absent"
	CauseCurrentKey   = "current_key"
	CauseClientOnly   = "client_only"
	CausePreviousKey  = "previous_key"
	CauseStaleKey     = "stale_key"
	CauseForgedServer = "server_cookie_mismatch"
	CauseMalformed    = "malformed_cookie"
	CauseRetryStorm   = "retry_storm"
	CauseOptionTooLong = "option_too_long"
)

// Protocol length bounds of the COOKIE option.
const (
	ClientCookieLen    = 8
	ServerCookieMinLen = 8
	ServerCookieMaxLen = 32
	CookieOptionMaxLen = ClientCookieLen + ServerCookieMaxLen
)

// Errors surfaced by the validator.
var (
	ErrBadClientCookie = errors.New("client cookie must be 8 bytes")
	ErrBadAddr         = errors.New("client address is not valid")
	ErrNoKey           = errors.New("validator has no secret key")
	ErrRotateTooSoon   = errors.New("key rotation is not due yet")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Cookie is one COOKIE option split into its two parts.
type Cookie struct {
	Client []byte
	Server []byte
}

// CookieRequest is one incoming query carrying an optional COOKIE option.
type CookieRequest struct {
	Client []byte
	Server []byte
	Addr   netip.Addr
	RawLen int
	Now    time.Time
}

// Verdict is the answer for one request.
type Verdict struct {
	Action   string
	Reason   string
	Cookie   Cookie
	Degraded bool
	Attack   bool
}

// CookieConfig bounds the validator.
type CookieConfig struct {
	ServerCookieLen  int
	RotationInterval time.Duration
	GracePeriod      time.Duration
	RetryWindow      time.Duration
	MaxRetries       int
	RequireServer    bool
	Key              []byte
	Now              func() time.Time
}

// CookieStats counts verdicts and key activity.
type CookieStats struct {
	Accepted    int
	BadCookie   int
	Dropped     int
	Degraded    int
	Attacks     int
	Rotations   int
	PrevKeyHits int
}

// DnsCookieValidator owns the key generations and the degradation table.
type DnsCookieValidator struct {
	cfg CookieConfig
	mu  sync.Mutex
}

// NewDnsCookieValidator builds a validator with the given key material.
func NewDnsCookieValidator(cfg CookieConfig) *DnsCookieValidator {
	return &DnsCookieValidator{cfg: cfg}
}

// Issue derives the server cookie for one client cookie and client address.
func (v *DnsCookieValidator) Issue(client Cookie, addr netip.Addr) (Cookie, error) {
	return Cookie{}, errNotImplemented
}

// Check classifies one request and, when needed, hands out a fresh cookie.
func (v *DnsCookieValidator) Check(req CookieRequest) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Rotate promotes the current key and starts a fresh generation.
func (v *DnsCookieValidator) Rotate(now time.Time) error {
	return errNotImplemented
}

// Generation reports the current key generation and the rotation instant.
func (v *DnsCookieValidator) Generation() (int, time.Time) {
	return 0, time.Time{}
}

// DegradedCount reports how often one client address degraded in the window.
func (v *DnsCookieValidator) DegradedCount(addr netip.Addr) int {
	return 0
}

// Stats reports counters of the validator.
func (v *DnsCookieValidator) Stats() CookieStats {
	return CookieStats{}
}

// Reset drops counters and the degradation table without touching the keys.
func (v *DnsCookieValidator) Reset(now time.Time) error {
	return errNotImplemented
}

// Snapshot serialises the key generations, the counters and the degradation
// table.
func (v *DnsCookieValidator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *DnsCookieValidator) Restore(data []byte) error {
	return errNotImplemented
}
