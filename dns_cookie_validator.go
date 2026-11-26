// Package task validates DNS COOKIE options at a resolver ingress and issues
// server cookies across key generations.
package task

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"
)

// Verdict actions.
const (
	ActionAccept    = "accept"
	ActionBadCookie = "badcookie"
	ActionDrop      = "drop"
)

// Verdict reasons.
const (
	CauseCookieAbsent  = "cookie_absent"
	CauseCurrentKey    = "current_key"
	CauseClientOnly    = "client_only"
	CausePreviousKey   = "previous_key"
	CauseStaleKey      = "stale_key"
	CauseForgedServer  = "server_cookie_mismatch"
	CauseMalformed     = "malformed_cookie"
	CauseRetryStorm    = "retry_storm"
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

// degradedEntry tracks the retry window of one client address.
type degradedEntry struct {
	WindowStart time.Time `json:"window_start"`
	Retries     int       `json:"retries"`
	Degraded    int       `json:"degraded"`
}

// keyGeneration is one immutable key generation.
type keyGeneration struct {
	Key       []byte
	RotatedAt time.Time
}

// DnsCookieValidator owns the key generations and the degradation table.
type DnsCookieValidator struct {
	cfg       CookieConfig
	serverLen int

	mu       sync.Mutex
	gen      int
	current  keyGeneration
	previous keyGeneration
	hasPrev  bool
	stats    CookieStats
	degraded map[netip.Addr]*degradedEntry
}

// NewDnsCookieValidator builds a validator with the given key material.
func NewDnsCookieValidator(cfg CookieConfig) *DnsCookieValidator {
	serverLen := cfg.ServerCookieLen
	if serverLen == 0 {
		serverLen = 16
	}
	if serverLen < ServerCookieMinLen {
		serverLen = ServerCookieMinLen
	}
	if serverLen > ServerCookieMaxLen {
		serverLen = ServerCookieMaxLen
	}
	key := make([]byte, len(cfg.Key))
	copy(key, cfg.Key)
	return &DnsCookieValidator{
		cfg:       cfg,
		serverLen: serverLen,
		gen:       1,
		current: keyGeneration{
			Key:       key,
			RotatedAt: cfgNow(cfg),
		},
		degraded: make(map[netip.Addr]*degradedEntry),
	}
}

func cfgNow(cfg CookieConfig) time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}

func (v *DnsCookieValidator) now(fallback time.Time) time.Time {
	if !fallback.IsZero() {
		return fallback
	}
	return cfgNow(v.cfg)
}

// deriveKey produces the key of the generation after the given one.
func deriveKey(prev []byte, gen int) []byte {
	h := sha256.New()
	h.Write([]byte("dns-cookie-rotate"))
	h.Write(prev)
	var genBuf [8]byte
	binary.BigEndian.PutUint64(genBuf[:], uint64(gen))
	h.Write(genBuf[:])
	return h.Sum(nil)
}

// deriveServerCookie computes the server cookie for one key, client cookie
// and client address.
func (v *DnsCookieValidator) deriveServerCookie(key, client []byte, addr netip.Addr) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("dns-cookie-server"))
	mac.Write(client)
	if addr.Is4() {
		mac.Write([]byte{0x04})
		b := addr.As4()
		mac.Write(b[:])
	} else {
		mac.Write([]byte{0x06})
		b := addr.As16()
		mac.Write(b[:])
	}
	return mac.Sum(nil)[:v.serverLen]
}

// Issue derives the server cookie for one client cookie and client address.
func (v *DnsCookieValidator) Issue(client Cookie, addr netip.Addr) (Cookie, error) {
	if len(client.Client) != ClientCookieLen {
		return Cookie{}, ErrBadClientCookie
	}
	if !addr.IsValid() {
		return Cookie{}, ErrBadAddr
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.current.Key) == 0 {
		return Cookie{}, ErrNoKey
	}
	out := Cookie{
		Client: append([]byte(nil), client.Client...),
		Server: v.deriveServerCookie(v.current.Key, client.Client, addr),
	}
	return out, nil
}

// Check classifies one request and, when needed, hands out a fresh cookie.
func (v *DnsCookieValidator) Check(req CookieRequest) (Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now(req.Now)

	// First-time query without any COOKIE option: normal path, let it pass.
	if len(req.Client) == 0 && len(req.Server) == 0 {
		v.stats.Accepted++
		return Verdict{Action: ActionAccept, Reason: CauseCookieAbsent}, nil
	}

	// The whole COOKIE option exceeds the protocol length bound: drop it
	// instead of validating a truncated option.
	if req.RawLen > CookieOptionMaxLen {
		v.stats.Dropped++
		return Verdict{Action: ActionDrop, Reason: CauseOptionTooLong}, nil
	}

	// The client cookie must be exactly 8 bytes; never truncate it.
	if len(req.Client) != ClientCookieLen {
		v.stats.Dropped++
		return Verdict{Action: ActionDrop, Reason: CauseMalformed}, nil
	}

	if len(req.Server) > 0 {
		return v.checkFullCookie(req, now), nil
	}
	return v.checkClientOnly(req, now), nil
}

// checkFullCookie handles a request carrying both cookie parts.
func (v *DnsCookieValidator) checkFullCookie(req CookieRequest, now time.Time) Verdict {
	// The server cookie length must match what this validator derives for
	// the client cookie; anything else is a format error.
	if len(req.Server) != v.serverLen {
		v.stats.Dropped++
		return Verdict{Action: ActionDrop, Reason: CauseMalformed}
	}

	want := v.deriveServerCookie(v.current.Key, req.Client, req.Addr)
	if hmac.Equal(want, req.Server) {
		v.stats.Accepted++
		return Verdict{Action: ActionAccept, Reason: CauseCurrentKey}
	}

	if v.hasPrev {
		prevWant := v.deriveServerCookie(v.previous.Key, req.Client, req.Addr)
		if hmac.Equal(prevWant, req.Server) {
			if now.Before(v.current.RotatedAt.Add(v.cfg.GracePeriod)) {
				v.stats.Accepted++
				v.stats.PrevKeyHits++
				return Verdict{Action: ActionAccept, Reason: CausePreviousKey}
			}
			v.stats.Dropped++
			return Verdict{Action: ActionDrop, Reason: CauseStaleKey}
		}
	}

	// An ordinary mismatch (forged cookie, NAT address change, ...): answer
	// with a fresh cookie, count a bad cookie, never an attack.
	v.stats.BadCookie++
	return Verdict{
		Action: ActionBadCookie,
		Reason: CauseForgedServer,
		Cookie: Cookie{
			Client: append([]byte(nil), req.Client...),
			Server: v.deriveServerCookie(v.current.Key, req.Client, req.Addr),
		},
	}
}

// checkClientOnly handles a request carrying only the client cookie.
func (v *DnsCookieValidator) checkClientOnly(req CookieRequest, now time.Time) Verdict {
	entry := v.degraded[req.Addr]
	if entry == nil || !now.Before(entry.WindowStart.Add(v.cfg.RetryWindow)) {
		entry = &degradedEntry{WindowStart: now}
		v.degraded[req.Addr] = entry
		entry.Degraded++
		v.stats.Degraded++
	}
	entry.Retries++
	degraded := entry.Retries == 1

	if entry.Retries > v.cfg.MaxRetries {
		v.stats.Dropped++
		return Verdict{Action: ActionDrop, Reason: CauseRetryStorm}
	}

	fresh := Cookie{
		Client: append([]byte(nil), req.Client...),
		Server: v.deriveServerCookie(v.current.Key, req.Client, req.Addr),
	}
	if !v.cfg.RequireServer {
		v.stats.Accepted++
		return Verdict{Action: ActionAccept, Reason: CauseClientOnly, Degraded: degraded}
	}
	v.stats.BadCookie++
	return Verdict{
		Action:   ActionBadCookie,
		Reason:   CauseClientOnly,
		Cookie:   fresh,
		Degraded: degraded,
	}
}

// Rotate promotes the current key and starts a fresh generation.
func (v *DnsCookieValidator) Rotate(now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if now.Before(v.current.RotatedAt.Add(v.cfg.RotationInterval)) {
		return ErrRotateTooSoon
	}
	v.previous = v.current
	v.hasPrev = true
	v.gen++
	v.current = keyGeneration{
		Key:       deriveKey(v.previous.Key, v.gen),
		RotatedAt: now,
	}
	v.stats.Rotations++
	return nil
}

// Generation reports the current key generation and the rotation instant.
func (v *DnsCookieValidator) Generation() (int, time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen, v.current.RotatedAt
}

// DegradedCount reports how often one client address degraded in the window.
func (v *DnsCookieValidator) DegradedCount(addr netip.Addr) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if entry := v.degraded[addr]; entry != nil {
		return entry.Degraded
	}
	return 0
}

// Stats reports counters of the validator.
func (v *DnsCookieValidator) Stats() CookieStats {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stats
}

// Reset drops counters and the degradation table without touching the keys.
func (v *DnsCookieValidator) Reset(now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stats = CookieStats{}
	v.degraded = make(map[netip.Addr]*degradedEntry)
	return nil
}

// snapshotState is the serialised form of the whole validator state.
type snapshotState struct {
	Version    int                      `json:"version"`
	Generation int                      `json:"generation"`
	RotatedAt  time.Time                `json:"rotated_at"`
	CurrentKey []byte                   `json:"current_key"`
	HasPrev    bool                     `json:"has_previous"`
	PrevKey    []byte                   `json:"previous_key,omitempty"`
	PrevAt     time.Time                `json:"previous_at,omitempty"`
	Stats      CookieStats              `json:"stats"`
	Degraded   map[string]degradedEntry `json:"degraded,omitempty"`
}

const snapshotVersion = 1

// Snapshot serialises the key generations, the counters and the degradation
// table.
func (v *DnsCookieValidator) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	state := snapshotState{
		Version:    snapshotVersion,
		Generation: v.gen,
		RotatedAt:  v.current.RotatedAt,
		CurrentKey: append([]byte(nil), v.current.Key...),
		HasPrev:    v.hasPrev,
		Stats:      v.stats,
	}
	if v.hasPrev {
		state.PrevKey = append([]byte(nil), v.previous.Key...)
		state.PrevAt = v.previous.RotatedAt
	}
	if len(v.degraded) > 0 {
		state.Degraded = make(map[string]degradedEntry, len(v.degraded))
		for addr, entry := range v.degraded {
			state.Degraded[addr.String()] = *entry
		}
	}
	return json.Marshal(state)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *DnsCookieValidator) Restore(data []byte) error {
	var state snapshotState
	if err := json.Unmarshal(data, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if state.Generation < 1 || len(state.CurrentKey) == 0 {
		return ErrBadSnapshot
	}
	if state.HasPrev && len(state.PrevKey) == 0 {
		return ErrBadSnapshot
	}
	degraded := make(map[netip.Addr]*degradedEntry, len(state.Degraded))
	for raw, entry := range state.Degraded {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return ErrBadSnapshot
		}
		copyEntry := entry
		degraded[addr] = &copyEntry
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.gen = state.Generation
	v.current = keyGeneration{Key: state.CurrentKey, RotatedAt: state.RotatedAt}
	v.hasPrev = state.HasPrev
	if state.HasPrev {
		v.previous = keyGeneration{Key: state.PrevKey, RotatedAt: state.PrevAt}
	} else {
		v.previous = keyGeneration{}
	}
	v.stats = state.Stats
	v.degraded = degraded
	return nil
}
