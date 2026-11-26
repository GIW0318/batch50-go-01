// Package task keeps an authenticated crawling session alive: it logs in,
// carries cookies and a cross site request token on every fetch, rotates an
// expired token without repeating side effects, and fingerprints form state so
// the same form is not submitted twice.
package task

import (
	"context"
	"errors"
	"sync"
)

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Errors reported by the guard.
var (
	ErrNoTransport     = errors.New("no transport is attached")
	ErrBadTarget       = errors.New("login target is incomplete")
	ErrNoSession       = errors.New("session is unknown")
	ErrBadRequest      = errors.New("request is incomplete")
	ErrTooManySessions = errors.New("session count exceeds the cap")
	ErrReplayExhausted = errors.New("replay budget for this request is exhausted")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Target is one site the crawler logs into.
type Target struct {
	Name     string
	BaseURL  string
	LoginURL string
	User     string
}

// Session is the crawling state kept for one logged in target.
type Session struct {
	ID       string
	Target   string
	Cookies  map[string]string
	Token    string
	Gen      int
	LoggedIn bool
}

// Req is one crawl request.
type Req struct {
	SessionID string
	Method    string
	URL       string
	Form      map[string]string
}

// Resp is one upstream response.
type Resp struct {
	Status   int
	Body     string
	Headers  map[string]string
	Cookies  map[string]string
	Location string
	Form     Form
}

// Form is the state of a form found in a response.
type Form struct {
	Action string
	Fields []string
	Hidden map[string]string
}

// Outbound is what the transport is asked to send.
type Outbound struct {
	Method  string
	URL     string
	Form    map[string]string
	Cookies map[string]string
	Token   string
}

// Inbound is what the transport returns.
type Inbound struct {
	Status  int
	Body    string
	Headers map[string]string
	Cookies map[string]string
}

// Transport performs one HTTP exchange. It is supplied by the caller so the
// guard itself stays local and deterministic.
type Transport interface {
	Do(ctx context.Context, req Outbound) (Inbound, error)
}

// Stats counts the session work done so far.
type Stats struct {
	Logins    int
	Rotations int
	Replays   int
	Fetches   int
}

// Limits bounds the session table and the replay attempts.
type Limits struct {
	MaxSessions int
	MaxReplays  int
}

// CrawlSessionGuard owns the sessions and the attached transport.
type CrawlSessionGuard struct {
	mu        sync.Mutex
	limits    Limits
	transport Transport
	sessions  map[string]Session
	order     []string
	stats     Stats
	seq       int
}

// NewCrawlSessionGuard builds a guard with the given caps.
func NewCrawlSessionGuard(limits Limits) *CrawlSessionGuard {
	return &CrawlSessionGuard{limits: limits}
}

// SetTransport attaches the transport used for every exchange.
func (g *CrawlSessionGuard) SetTransport(t Transport) error {
	return errNotImplemented
}

// Login authenticates against a target and stores the resulting session.
func (g *CrawlSessionGuard) Login(target Target) (Session, error) {
	return Session{}, errNotImplemented
}

// Fetch sends one request with the session state and repairs the session at
// most once when the upstream reports an expired token or a dropped login.
func (g *CrawlSessionGuard) Fetch(ctx context.Context, req Req) (Resp, error) {
	return Resp{}, errNotImplemented
}

// Refresh rotates the request token of one session.
func (g *CrawlSessionGuard) Refresh(sessionID string) error {
	return errNotImplemented
}

// Fingerprint reduces a form state to a stable digest.
func (g *CrawlSessionGuard) Fingerprint(resp Resp) string {
	return ""
}

// SessionState reads one stored session.
func (g *CrawlSessionGuard) SessionState(sessionID string) (Session, bool) {
	return Session{}, false
}

// Sessions lists the session ids in creation order.
func (g *CrawlSessionGuard) Sessions() []string {
	return nil
}

// Close drops one session.
func (g *CrawlSessionGuard) Close(sessionID string) error {
	return errNotImplemented
}

// Stats returns the counters.
func (g *CrawlSessionGuard) Stats() Stats {
	return Stats{}
}

// Snapshot serialises the sessions and the counters.
func (g *CrawlSessionGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the guard state with a snapshot, all or nothing.
func (g *CrawlSessionGuard) Restore(data []byte) error {
	return errNotImplemented
}

// Reset drops every session.
func (g *CrawlSessionGuard) Reset() error {
	return errNotImplemented
}
