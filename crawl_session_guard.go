// Package task keeps an authenticated crawling session alive: it logs in,
// carries cookies and a cross site request token on every fetch, rotates an
// expired token without repeating side effects, and fingerprints form state so
// the same form is not submitted twice.
package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Errors reported by the guard.
var (
	ErrNoTransport      = errors.New("no transport is attached")
	ErrBadTarget        = errors.New("login target is incomplete")
	ErrNoSession        = errors.New("session is unknown")
	ErrBadRequest       = errors.New("request is incomplete")
	ErrTooManySessions  = errors.New("session count exceeds the cap")
	ErrReplayExhausted  = errors.New("replay budget for this request is exhausted")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
	ErrLogoutNotAllowed = fmt.Errorf("%w: logout URLs must not be followed", ErrBadRequest)
)

const (
	snapshotMagic   = "gsb-crawl-session-guard-snapshot"
	snapshotVersion = 1
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
	limits Limits

	registryMu sync.RWMutex
	sessions   map[string]*sessionEntry
	order      []string
	seq        int

	transportMu sync.RWMutex
	transport   Transport

	statsMu sync.Mutex
	stats   Stats
}

type sessionEntry struct {
	opCh   chan struct{}
	active sync.WaitGroup

	stateMu sync.RWMutex
	closed  bool
	data    Session
	target  Target
	cache   map[string]Resp
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type snapshotPayload struct {
	Seq      int               `json:"seq"`
	Stats    Stats             `json:"stats"`
	Sessions []snapshotSession `json:"sessions"`
}

type snapshotSession struct {
	ID      string  `json:"id"`
	Session Session `json:"session"`
	Target  Target  `json:"target"`
}

var (
	inputTagPattern     = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	formTagPattern      = regexp.MustCompile(`(?is)<form\b[^>]*>`)
	namedFieldPattern   = regexp.MustCompile(`(?is)<(?:select|textarea)\b[^>]*>`)
	tagAttributePattern = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)

	loginMarkers  = []string{"login", "log-in", "log_in", "signin", "sign-in", "sign_in"}
	logoutMarkers = []string{"logout", "log-out", "log_out", "signout", "sign-out", "sign_out"}
)

// NewCrawlSessionGuard builds a guard with the given caps.
func NewCrawlSessionGuard(limits Limits) *CrawlSessionGuard {
	if limits.MaxSessions < 0 {
		limits.MaxSessions = 0
	}
	if limits.MaxReplays < 0 {
		limits.MaxReplays = 0
	}
	return &CrawlSessionGuard{
		limits:   limits,
		sessions: make(map[string]*sessionEntry),
	}
}

// SetTransport attaches the transport used for every exchange.
func (g *CrawlSessionGuard) SetTransport(t Transport) error {
	g.transportMu.Lock()
	g.transport = t
	g.transportMu.Unlock()
	return nil
}

func (g *CrawlSessionGuard) currentTransport() (Transport, error) {
	g.transportMu.RLock()
	defer g.transportMu.RUnlock()
	if g.transport == nil {
		return nil, ErrNoTransport
	}
	return g.transport, nil
}

// Login authenticates against a target and stores the resulting session.
func (g *CrawlSessionGuard) Login(target Target) (Session, error) {
	t, err := g.currentTransport()
	if err != nil {
		return Session{}, err
	}

	target = normalizeTarget(target)
	if !validTarget(target) {
		return Session{}, ErrBadTarget
	}

	g.registryMu.RLock()
	atCap := g.sessions != nil && len(g.sessions) >= g.limits.MaxSessions
	g.registryMu.RUnlock()
	if atCap {
		return Session{}, ErrTooManySessions
	}

	auth, err := authenticate(context.Background(), t, target)
	if err != nil {
		return Session{}, err
	}

	g.registryMu.Lock()
	if g.sessions == nil {
		g.sessions = make(map[string]*sessionEntry)
	}
	if len(g.sessions) >= g.limits.MaxSessions {
		g.registryMu.Unlock()
		return Session{}, ErrTooManySessions
	}

	g.seq++
	id := "sess-" + strconv.Itoa(g.seq)
	auth.ID = id
	entry := &sessionEntry{
		opCh:   make(chan struct{}, 1),
		data:   cloneSession(auth),
		target: target,
		cache:  make(map[string]Resp),
	}
	g.sessions[id] = entry
	g.order = append(g.order, id)
	g.registryMu.Unlock()

	g.addLogin()
	return cloneSession(auth), nil
}

func authenticate(ctx context.Context, t Transport, target Target) (Session, error) {
	in, err := t.Do(ctx, Outbound{
		Method: http.MethodPost,
		URL:    target.LoginURL,
		Form: map[string]string{
			"user":     target.User,
			"username": target.User,
		},
		Cookies: map[string]string{},
	})
	if err != nil {
		return Session{}, err
	}

	return Session{
		Target:   target.Name,
		Cookies:  cookiesFromInbound(in),
		Token:    csrfFromInbound(in),
		Gen:      1,
		LoggedIn: true,
	}, nil
}

// Fetch sends one request with the session state and repairs the session at
// most once when the upstream reports an expired token or a dropped login.
func (g *CrawlSessionGuard) Fetch(ctx context.Context, req Req) (Resp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	t, err := g.currentTransport()
	if err != nil {
		return Resp{}, err
	}

	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		return Resp{}, ErrBadRequest
	}
	if isLogoutURL(req.URL) {
		return Resp{}, ErrLogoutNotAllowed
	}

	entry, err := g.acquireEntry(req.SessionID)
	if err != nil {
		return Resp{}, err
	}
	defer entry.active.Done()

	if entry.isClosed() {
		return Resp{}, ErrNoSession
	}

	g.addFetch()
	session := entry.snapshot()
	target := entry.targetSnapshot()
	cacheKey := sessionCacheKey(req, session)
	if cached, ok := entry.cacheGet(cacheKey); ok {
		return cached, nil
	}

	in, err := t.Do(ctx, newOutbound(req, session))
	if err != nil {
		return Resp{}, err
	}
	if isLogoutRedirect(in.Status, in.Headers) {
		return convertInbound(in), ErrLogoutNotAllowed
	}

	resp := convertInbound(in)
	switch {
	case resp.Status == http.StatusUnauthorized:
		resp, err = g.replayAfter401(ctx, t, entry, req, session.Gen)
	case isLoginRedirect(resp.Status, resp.Location, resp.Headers, req.URL, target.LoginURL):
		resp, err = g.replayAfterLogin(ctx, t, entry, req)
	default:
		g.storeCache(req, entry, resp)
		return resp, nil
	}

	if err != nil {
		return Resp{}, err
	}
	if isLogoutRedirect(resp.Status, resp.Headers) {
		return resp, ErrLogoutNotAllowed
	}
	if resp.Status == http.StatusUnauthorized ||
		isLoginRedirect(resp.Status, resp.Location, resp.Headers, req.URL, target.LoginURL) {
		return resp, fmt.Errorf("%w: repair did not produce a valid session response", ErrReplayExhausted)
	}

	g.storeCache(req, entry, resp)
	return resp, nil
}

func (g *CrawlSessionGuard) replayAfter401(
	ctx context.Context,
	t Transport,
	entry *sessionEntry,
	req Req,
	observedGen int,
) (Resp, error) {
	if g.replayBudget() == 0 {
		return Resp{}, ErrReplayExhausted
	}
	if err := entry.lockOp(ctx); err != nil {
		return Resp{}, err
	}
	defer entry.unlockOp()

	if entry.isClosed() {
		return Resp{}, ErrNoSession
	}
	if entry.generation() == observedGen {
		if err := g.rotateLocked(ctx, t, entry); err != nil {
			return Resp{}, err
		}
	}

	latest := entry.snapshot()
	g.addReplay()
	in, err := t.Do(ctx, newOutbound(req, latest))
	if err != nil {
		return Resp{}, fmt.Errorf("%w: replay transport error: %w", ErrReplayExhausted, err)
	}
	return convertInbound(in), nil
}

func (g *CrawlSessionGuard) replayAfterLogin(
	ctx context.Context,
	t Transport,
	entry *sessionEntry,
	req Req,
) (Resp, error) {
	if g.replayBudget() == 0 {
		return Resp{}, ErrReplayExhausted
	}
	if err := entry.lockOp(ctx); err != nil {
		return Resp{}, err
	}
	defer entry.unlockOp()

	if entry.isClosed() {
		return Resp{}, ErrNoSession
	}

	target := entry.targetSnapshot()
	auth, err := authenticate(ctx, t, target)
	if err != nil {
		return Resp{}, fmt.Errorf("%w: re-authentication failed: %w", ErrReplayExhausted, err)
	}

	entry.stateMu.Lock()
	if entry.closed {
		entry.stateMu.Unlock()
		return Resp{}, ErrNoSession
	}
	old := entry.data
	auth.ID = old.ID
	auth.Gen = old.Gen
	if auth.Cookies == nil {
		auth.Cookies = make(map[string]string)
	}
	for key, value := range old.Cookies {
		if _, exists := auth.Cookies[key]; !exists {
			auth.Cookies[key] = value
		}
	}
	entry.data = auth
	entry.cache = make(map[string]Resp)
	entry.stateMu.Unlock()
	g.addLogin()

	latest := entry.snapshot()
	g.addReplay()
	in, err := t.Do(ctx, newOutbound(req, latest))
	if err != nil {
		return Resp{}, fmt.Errorf("%w: replay transport error: %w", ErrReplayExhausted, err)
	}
	return convertInbound(in), nil
}

// Refresh rotates the request token of one session.
func (g *CrawlSessionGuard) Refresh(sessionID string) error {
	entry, err := g.acquireEntry(sessionID)
	if err != nil {
		return err
	}
	defer entry.active.Done()

	if err := entry.lockOp(context.Background()); err != nil {
		return err
	}
	defer entry.unlockOp()

	if entry.isClosed() {
		return ErrNoSession
	}
	t, err := g.currentTransport()
	if err != nil {
		return err
	}
	return g.rotateLocked(context.Background(), t, entry)
}

func (g *CrawlSessionGuard) rotateLocked(ctx context.Context, t Transport, entry *sessionEntry) error {
	target := entry.targetSnapshot()
	current := entry.snapshot()
	tokenURL := strings.TrimRight(target.BaseURL, "/") + "/token"

	in, err := t.Do(ctx, Outbound{
		Method:  http.MethodPost,
		URL:     tokenURL,
		Cookies: cloneStringMap(current.Cookies),
		Token:   current.Token,
		Form:    map[string]string{"csrf": current.Token},
	})
	if err != nil {
		return err
	}
	if in.Status != 0 && (in.Status < 200 || in.Status >= 300) {
		return fmt.Errorf("token endpoint returned status %d", in.Status)
	}

	token := parseRefreshToken(in.Body)
	if token == "" {
		token = csrfFromInbound(in)
	}
	if token == "" {
		return errors.New("token endpoint response did not contain a csrf token")
	}

	entry.stateMu.Lock()
	if entry.closed {
		entry.stateMu.Unlock()
		return ErrNoSession
	}
	if entry.data.Cookies == nil {
		entry.data.Cookies = make(map[string]string)
	}
	for key, value := range cookiesFromInbound(in) {
		entry.data.Cookies[key] = value
	}
	entry.data.Token = token
	entry.data.Gen++
	entry.stateMu.Unlock()

	g.addRotation()
	return nil
}

// Fingerprint reduces a form state to a stable digest.
func (g *CrawlSessionGuard) Fingerprint(resp Resp) string {
	form := resp.Form
	if form.Action == "" && len(form.Fields) == 0 && len(form.Hidden) == 0 && resp.Body != "" {
		form = extractFirstForm(resp.Body)
	}

	fields := uniqueSortedStrings(form.Fields)
	keys := make([]string, 0, len(form.Hidden))
	for key := range form.Hidden {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	hidden := make([][2]string, 0, len(keys))
	for _, key := range keys {
		hidden = append(hidden, [2]string{key, form.Hidden[key]})
	}

	canonical := struct {
		Action string      `json:"action"`
		Fields []string    `json:"fields"`
		Hidden [][2]string `json:"hidden"`
	}{
		Action: form.Action,
		Fields: fields,
		Hidden: hidden,
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SessionState reads one stored session.
func (g *CrawlSessionGuard) SessionState(sessionID string) (Session, bool) {
	g.registryMu.RLock()
	entry, ok := g.sessions[sessionID]
	g.registryMu.RUnlock()
	if !ok {
		return Session{}, false
	}
	return entry.snapshot(), true
}

// Sessions lists the session ids in creation order.
func (g *CrawlSessionGuard) Sessions() []string {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	return append(make([]string, 0, len(g.order)), g.order...)
}

// Close drops one session.
func (g *CrawlSessionGuard) Close(sessionID string) error {
	g.registryMu.Lock()
	entry, ok := g.sessions[sessionID]
	if !ok {
		g.registryMu.Unlock()
		return ErrNoSession
	}
	delete(g.sessions, sessionID)
	g.removeOrderLocked(sessionID)
	g.registryMu.Unlock()

	shutdownEntry(entry)
	return nil
}

// Stats returns the counters.
func (g *CrawlSessionGuard) Stats() Stats {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	return g.stats
}

// Snapshot serialises the sessions and the counters.
func (g *CrawlSessionGuard) Snapshot() ([]byte, error) {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	ids := append([]string(nil), g.order...)
	entries := make([]*sessionEntry, 0, len(ids))
	for _, id := range ids {
		if entry, ok := g.sessions[id]; ok {
			entries = append(entries, entry)
		}
	}
	seq := g.seq

	sessions := make([]snapshotSession, 0, len(entries))
	for _, entry := range entries {
		state := entry.snapshot()
		target := entry.targetSnapshot()
		sessions = append(sessions, snapshotSession{
			ID:      state.ID,
			Session: cloneSession(state),
			Target:  cloneTarget(target),
		})
	}
	stats := g.Stats()

	payload := snapshotPayload{
		Seq:      seq,
		Stats:    stats,
		Sessions: sessions,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	digest := sha256.Sum256(raw)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Payload:  raw,
		Checksum: hex.EncodeToString(digest[:]),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the guard state with a snapshot, all or nothing.
func (g *CrawlSessionGuard) Restore(data []byte) error {
	payload, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	entries := make(map[string]*sessionEntry, len(payload.Sessions))
	order := make([]string, 0, len(payload.Sessions))
	for _, saved := range payload.Sessions {
		state := cloneSession(saved.Session)
		target := cloneTarget(saved.Target)
		entries[saved.ID] = &sessionEntry{
			opCh:   make(chan struct{}, 1),
			data:   state,
			target: target,
			cache:  make(map[string]Resp),
		}
		order = append(order, saved.ID)
	}

	g.registryMu.Lock()
	old := g.sessions
	g.sessions = entries
	g.order = order
	g.seq = payload.Seq
	g.registryMu.Unlock()

	g.statsMu.Lock()
	g.stats = payload.Stats
	g.statsMu.Unlock()

	for _, entry := range old {
		shutdownEntry(entry)
	}

	g.statsMu.Lock()
	g.stats = payload.Stats
	g.statsMu.Unlock()
	return nil
}

// Reset drops every session.
func (g *CrawlSessionGuard) Reset() error {
	g.registryMu.Lock()
	old := g.sessions
	g.sessions = make(map[string]*sessionEntry)
	g.order = nil
	g.seq = 0
	g.registryMu.Unlock()

	for _, entry := range old {
		shutdownEntry(entry)
	}

	g.statsMu.Lock()
	g.stats = Stats{}
	g.statsMu.Unlock()
	return nil
}

func (g *CrawlSessionGuard) acquireEntry(id string) (*sessionEntry, error) {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	entry, ok := g.sessions[id]
	if !ok {
		return nil, ErrNoSession
	}
	entry.active.Add(1)
	return entry, nil
}

func (g *CrawlSessionGuard) removeOrderLocked(id string) {
	for i, existing := range g.order {
		if existing == id {
			g.order = append(g.order[:i], g.order[i+1:]...)
			return
		}
	}
}

func (g *CrawlSessionGuard) replayBudget() int {
	budget := g.limits.MaxReplays
	if budget <= 0 {
		return 1
	}
	if budget > 1 {
		return 1
	}
	return budget
}

func (g *CrawlSessionGuard) addLogin() {
	g.statsMu.Lock()
	g.stats.Logins++
	g.statsMu.Unlock()
}

func (g *CrawlSessionGuard) addRotation() {
	g.statsMu.Lock()
	g.stats.Rotations++
	g.statsMu.Unlock()
}

func (g *CrawlSessionGuard) addReplay() {
	g.statsMu.Lock()
	g.stats.Replays++
	g.statsMu.Unlock()
}

func (g *CrawlSessionGuard) addFetch() {
	g.statsMu.Lock()
	g.stats.Fetches++
	g.statsMu.Unlock()
}

func (e *sessionEntry) lockOp(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case e.opCh <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *sessionEntry) unlockOp() {
	<-e.opCh
}

func shutdownEntry(entry *sessionEntry) {
	if entry == nil {
		return
	}
	entry.opCh <- struct{}{}
	entry.stateMu.Lock()
	entry.closed = true
	entry.data.Cookies = nil
	entry.data.Token = ""
	entry.cache = nil
	entry.stateMu.Unlock()
	<-entry.opCh
	entry.active.Wait()
}

func (e *sessionEntry) isClosed() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.closed
}

func (e *sessionEntry) snapshot() Session {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return cloneSession(e.data)
}

func (e *sessionEntry) generation() int {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.data.Gen
}

func (e *sessionEntry) targetSnapshot() Target {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return cloneTarget(e.target)
}

func (e *sessionEntry) cacheGet(key string) (Resp, bool) {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	resp, ok := e.cache[key]
	if !ok {
		return Resp{}, false
	}
	return cloneResp(resp), true
}

func (e *sessionEntry) cacheSet(key string, resp Resp) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.closed || e.cache == nil {
		return
	}
	e.cache[key] = cloneResp(resp)
}

func (g *CrawlSessionGuard) storeCache(req Req, entry *sessionEntry, resp Resp) {
	method := normalizeMethod(req.Method)
	if method != http.MethodGet && method != http.MethodHead {
		return
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return
	}
	state := entry.snapshot()
	entry.cacheSet(sessionCacheKey(req, state), resp)
}

func normalizeTarget(target Target) Target {
	target.Name = strings.TrimSpace(target.Name)
	target.BaseURL = strings.TrimSpace(target.BaseURL)
	target.LoginURL = strings.TrimSpace(target.LoginURL)
	target.User = strings.TrimSpace(target.User)
	return target
}

func validTarget(target Target) bool {
	return target.Name != "" && target.BaseURL != "" && target.LoginURL != "" && target.User != ""
}

func normalizeMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return http.MethodGet
	}
	return method
}

func newOutbound(req Req, session Session) Outbound {
	return Outbound{
		Method:  normalizeMethod(req.Method),
		URL:     req.URL,
		Form:    cloneStringMap(req.Form),
		Cookies: cloneStringMap(session.Cookies),
		Token:   session.Token,
	}
}

func sessionCacheKey(req Req, session Session) string {
	var builder strings.Builder
	builder.WriteString("method=")
	builder.WriteString(normalizeMethod(req.Method))
	builder.WriteByte(0)
	builder.WriteString("url=")
	builder.WriteString(req.URL)
	builder.WriteByte(0)
	builder.WriteString("gen=")
	builder.WriteString(strconv.Itoa(session.Gen))
	builder.WriteByte(0)
	builder.WriteString("token=")
	builder.WriteString(session.Token)
	if len(req.Form) > 0 {
		builder.WriteByte(0)
		builder.WriteString("form=")
		builder.WriteString(canonicalStringMap(req.Form))
	}
	return builder.String()
}

func canonicalStringMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte(0)
		builder.WriteString(values[key])
		builder.WriteByte(0)
	}
	return builder.String()
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneSession(session Session) Session {
	session.Cookies = cloneStringMap(session.Cookies)
	return session
}

func cloneTarget(target Target) Target {
	return target
}

func cloneResp(resp Resp) Resp {
	resp.Headers = cloneStringMap(resp.Headers)
	resp.Cookies = cloneStringMap(resp.Cookies)
	resp.Form.Fields = append([]string(nil), resp.Form.Fields...)
	resp.Form.Hidden = cloneStringMap(resp.Form.Hidden)
	return resp
}

func convertInbound(in Inbound) Resp {
	headers := cloneStringMap(in.Headers)
	return Resp{
		Status:   in.Status,
		Body:     in.Body,
		Headers:  headers,
		Cookies:  cloneStringMap(in.Cookies),
		Location: headerValue(headers, "Location"),
		Form:     extractFirstForm(in.Body),
	}
}

func csrfFromInbound(in Inbound) string {
	if token, ok := extractHiddenCSRF(in.Body); ok {
		return token
	}
	return csrfHeader(in.Headers)
}

func extractHiddenCSRF(body string) (string, bool) {
	for _, tag := range inputTagPattern.FindAllString(body, -1) {
		attrs := parseTagAttributes(tag)
		if attrs["name"] == "csrf_token" {
			return attrs["value"], true
		}
	}
	return "", false
}

func extractFirstForm(body string) Form {
	loc := formTagPattern.FindStringIndex(body)
	if loc == nil {
		return Form{}
	}

	openTag := body[loc[0]:loc[1]]
	attrs := parseTagAttributes(openTag)
	form := Form{
		Action: attrs["action"],
		Fields: []string{},
		Hidden: make(map[string]string),
	}

	section := body[loc[1]:]
	if closeIndex := strings.Index(strings.ToLower(section), "</form>"); closeIndex >= 0 {
		section = section[:closeIndex]
	}

	for _, tag := range inputTagPattern.FindAllString(section, -1) {
		tagAttrs := parseTagAttributes(tag)
		name := tagAttrs["name"]
		if name == "" {
			continue
		}
		form.Fields = append(form.Fields, name)
		if strings.EqualFold(tagAttrs["type"], "hidden") {
			form.Hidden[name] = tagAttrs["value"]
		}
	}
	for _, tag := range namedFieldPattern.FindAllString(section, -1) {
		tagAttrs := parseTagAttributes(tag)
		if name := tagAttrs["name"]; name != "" {
			form.Fields = append(form.Fields, name)
		}
	}
	return form
}

func parseTagAttributes(tag string) map[string]string {
	attrs := make(map[string]string)
	for _, match := range tagAttributePattern.FindAllStringSubmatch(tag, -1) {
		key := strings.ToLower(match[1])
		if key == "" || key == "input" || key == "form" || key == "select" || key == "textarea" {
			continue
		}
		value := match[2]
		if value == "" {
			value = match[3]
		}
		if value == "" {
			value = match[4]
		}
		attrs[key] = html.UnescapeString(value)
	}
	return attrs
}

func headerValue(headers map[string]string, name string) string {
	if value, ok := headers[name]; ok {
		return value
	}
	wanted := strings.ToLower(name)
	for key, value := range headers {
		if strings.ToLower(key) == wanted {
			return value
		}
	}
	return ""
}

func csrfHeader(headers map[string]string) string {
	common := []string{
		"X-CSRF-Token",
		"X-Csrf-Token",
		"CSRF-Token",
		"Csrf-Token",
		"X-CSRFTOKEN",
	}
	for _, name := range common {
		if value := headerValue(headers, name); value != "" {
			return value
		}
	}
	for key, value := range headers {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "csrf") && strings.Contains(lower, "token") && value != "" {
			return value
		}
	}
	return ""
}

func cookiesFromInbound(in Inbound) map[string]string {
	cookies := make(map[string]string)
	for key, value := range in.Cookies {
		cookies[key] = value
	}
	if setCookie := headerValue(in.Headers, "Set-Cookie"); setCookie != "" {
		first := strings.Split(setCookie, ";")[0]
		if key, value, ok := strings.Cut(first, "="); ok {
			cookies[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return cookies
}

func parseRefreshToken(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if values, err := url.ParseQuery(body); err == nil {
		for _, key := range []string{"csrf", "csrf_token"} {
			if token := strings.TrimSpace(values.Get(key)); token != "" {
				return token
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		for _, prefix := range []string{"csrf=", "csrf_token="} {
			if strings.HasPrefix(lower, prefix) {
				return strings.TrimSpace(line[len(prefix):])
			}
		}
	}
	return ""
}

func isLogoutURL(raw string) bool {
	return locationHasMarker(raw, logoutMarkers)
}

func locationHasMarker(raw string, markers []string) bool {
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return containsMarker(strings.ToLower(raw), markers)
	}
	text := strings.ToLower(parsed.Path)
	for _, key := range []string{"action", "op", "do", "mode", "page"} {
		for _, value := range parsed.Query()[key] {
			text += " " + strings.ToLower(value)
		}
	}
	return containsMarker(text, markers)
}

func containsMarker(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func isLogoutRedirect(status int, headers map[string]string) bool {
	if !isRedirectStatus(status) {
		return false
	}
	location := headerValue(headers, "Location")
	return location != "" && isLogoutURL(location)
}

func isLoginRedirect(status int, location string, headers map[string]string, requestURL, loginURL string) bool {
	if !isRedirectStatus(status) {
		return false
	}
	if location == "" {
		location = headerValue(headers, "Location")
	}
	if location == "" {
		return false
	}
	if locationHasMarker(location, loginMarkers) {
		return true
	}
	return sameEndpoint(location, loginURL, requestURL)
}

func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func sameEndpoint(location, endpoint, base string) bool {
	locationURL := resolveReference(location, base)
	endpointURL, err := url.Parse(endpoint)
	if locationURL == nil || err != nil || endpointURL == nil {
		return false
	}
	if !strings.EqualFold(locationURL.Host, endpointURL.Host) {
		return false
	}
	return strings.TrimRight(locationURL.Path, "/") == strings.TrimRight(endpointURL.Path, "/")
}

func resolveReference(reference, base string) *url.URL {
	refURL, err := url.Parse(reference)
	if err != nil {
		return nil
	}
	if refURL.IsAbs() || base == "" {
		return refURL
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return refURL
	}
	return baseURL.ResolveReference(refURL)
}

func decodeSnapshot(data []byte) (snapshotPayload, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return snapshotPayload{}, fmt.Errorf("%w: invalid json: %v", ErrBadSnapshot, err)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion {
		return snapshotPayload{}, fmt.Errorf("%w: unknown snapshot format", ErrBadSnapshot)
	}
	if len(envelope.Payload) == 0 || envelope.Checksum == "" {
		return snapshotPayload{}, fmt.Errorf("%w: missing payload or checksum", ErrBadSnapshot)
	}

	digest := sha256.Sum256(envelope.Payload)
	if envelope.Checksum != hex.EncodeToString(digest[:]) {
		return snapshotPayload{}, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var payload snapshotPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return snapshotPayload{}, fmt.Errorf("%w: invalid payload: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshotPayload(&payload); err != nil {
		return snapshotPayload{}, err
	}
	return payload, nil
}

func validateSnapshotPayload(payload *snapshotPayload) error {
	if payload.Seq < 0 || payload.Stats.Logins < 0 || payload.Stats.Rotations < 0 ||
		payload.Stats.Replays < 0 || payload.Stats.Fetches < 0 {
		return fmt.Errorf("%w: negative sequence or counter", ErrBadSnapshot)
	}
	if payload.Sessions == nil {
		payload.Sessions = []snapshotSession{}
	}

	ids := make(map[string]struct{}, len(payload.Sessions))
	maxSeq := payload.Seq
	for i := range payload.Sessions {
		saved := &payload.Sessions[i]
		if saved.ID == "" || saved.Session.ID != saved.ID {
			return fmt.Errorf("%w: session id mismatch", ErrBadSnapshot)
		}
		if saved.Session.Gen < 1 || !saved.Session.LoggedIn {
			return fmt.Errorf("%w: session generation or login state is invalid", ErrBadSnapshot)
		}
		if saved.Session.Target != saved.Target.Name || !validTarget(saved.Target) {
			return fmt.Errorf("%w: session target is incomplete", ErrBadSnapshot)
		}
		if _, exists := ids[saved.ID]; exists {
			return fmt.Errorf("%w: duplicate session id", ErrBadSnapshot)
		}
		ids[saved.ID] = struct{}{}
		if saved.Session.Cookies == nil {
			saved.Session.Cookies = make(map[string]string)
		}
		if sequence := sessionSequence(saved.ID); sequence > maxSeq {
			maxSeq = sequence
		}
	}
	payload.Seq = maxSeq
	return nil
}

func sessionSequence(id string) int {
	if !strings.HasPrefix(id, "sess-") {
		return 0
	}
	sequence, err := strconv.Atoi(strings.TrimPrefix(id, "sess-"))
	if err != nil {
		return 0
	}
	return sequence
}
