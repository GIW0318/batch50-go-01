package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-40"
)

type fakeSite struct {
	mu        sync.Mutex
	current   string
	next      string
	rotations int
	logins    int
	calls     []string
	failAll   bool
	dropOnce  bool
	lastToken string
	lastSID   string
}

func newFakeSite() *fakeSite {
	return &fakeSite{current: "T0", next: "T1"}
}

func (f *fakeSite) Do(ctx context.Context, req impl.Outbound) (impl.Inbound, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.URL)
	f.lastToken = req.Token
	f.lastSID = req.Cookies["sid"]
	switch {
	case strings.HasSuffix(req.URL, "/login"):
		f.logins++
		return impl.Inbound{
			Status:  200,
			Body:    `<input type="hidden" name="csrf_token" value="` + f.current + `">`,
			Headers: map[string]string{"Content-Type": "text/html"},
			Cookies: map[string]string{"sid": "s-1"},
		}, nil
	case strings.HasSuffix(req.URL, "/token"):
		f.rotations++
		f.current = f.next
		return impl.Inbound{Status: 200, Body: "csrf=" + f.current}, nil
	}
	if f.dropOnce {
		f.dropOnce = false
		return impl.Inbound{Status: 302, Headers: map[string]string{"Location": "/login"}}, nil
	}
	if f.failAll || req.Token != f.current {
		return impl.Inbound{Status: 401, Body: "token expired"}, nil
	}
	return impl.Inbound{Status: 200, Body: "ok:" + req.URL}, nil
}

func (f *fakeSite) expire(next string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = next
}

func (f *fakeSite) rotateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rotations
}

func (f *fakeSite) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeSite) dataCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, url := range f.calls {
		if strings.HasSuffix(url, "/login") || strings.HasSuffix(url, "/token") {
			continue
		}
		out = append(out, url)
	}
	return out
}

func (f *fakeSite) lastOutbound() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastToken, f.lastSID
}

func testTarget() impl.Target {
	return impl.Target{
		Name:     "portal",
		BaseURL:  "https://portal.example",
		LoginURL: "https://portal.example/login",
		User:     "scanner",
	}
}

func newGuard(t *testing.T, site *fakeSite) (*impl.CrawlSessionGuard, impl.Session) {
	t.Helper()
	g := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 4, MaxReplays: 1})
	if err := g.SetTransport(site); err != nil {
		t.Fatalf("set transport: %v", err)
	}
	sess, err := g.Login(testTarget())
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return g, sess
}

func getReq(sessionID, url string) impl.Req {
	return impl.Req{SessionID: sessionID, Method: "GET", URL: url}
}

func TestSetTransportRequired(t *testing.T) {
	g := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 4, MaxReplays: 1})
	if _, err := g.Fetch(context.Background(), getReq("sess-1", "https://portal.example/a")); !errors.Is(err, impl.ErrNoTransport) {
		t.Fatalf("fetch without a transport must fail with ErrNoTransport, got %v", err)
	}
	if _, err := g.Login(testTarget()); !errors.Is(err, impl.ErrNoTransport) {
		t.Fatalf("login without a transport must fail with ErrNoTransport, got %v", err)
	}
	if got := g.Sessions(); len(got) != 0 {
		t.Fatalf("no session may be created without a transport, got %v", got)
	}
}

func TestLoginCapturesSessionState(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	if sess.ID == "" {
		t.Fatalf("a session must carry an id")
	}
	if sess.Target != "portal" || !sess.LoggedIn {
		t.Fatalf("session = %+v, want the portal target and a logged in flag", sess)
	}
	if sess.Token != "T0" {
		t.Fatalf("token = %q, want the hidden field value T0", sess.Token)
	}
	if sess.Cookies["sid"] != "s-1" {
		t.Fatalf("cookies = %v, want sid=s-1", sess.Cookies)
	}
	if sess.Gen != 1 {
		t.Fatalf("generation = %d, want 1 right after login", sess.Gen)
	}
	if got := g.Sessions(); len(got) != 1 || got[0] != sess.ID {
		t.Fatalf("sessions = %v, want [%s]", got, sess.ID)
	}
	if stats := g.Stats(); stats.Logins != 1 {
		t.Fatalf("stats = %+v, want one login", stats)
	}
}

func TestLoginRejectsBlankTarget(t *testing.T) {
	g := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 4, MaxReplays: 1})
	if err := g.SetTransport(newFakeSite()); err != nil {
		t.Fatalf("set transport: %v", err)
	}
	cases := []impl.Target{
		{},
		{Name: "portal"},
		{Name: "portal", BaseURL: "https://portal.example"},
		{BaseURL: "https://portal.example", LoginURL: "https://portal.example/login"},
	}
	for _, target := range cases {
		if _, err := g.Login(target); !errors.Is(err, impl.ErrBadTarget) {
			t.Fatalf("target %+v must fail with ErrBadTarget, got %v", target, err)
		}
	}
	if got := g.Sessions(); len(got) != 0 {
		t.Fatalf("a rejected target must not create a session, got %v", got)
	}
}

func TestLoginEnforcesSessionCap(t *testing.T) {
	g := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 1, MaxReplays: 1})
	if err := g.SetTransport(newFakeSite()); err != nil {
		t.Fatalf("set transport: %v", err)
	}
	if _, err := g.Login(testTarget()); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := g.Login(testTarget()); !errors.Is(err, impl.ErrTooManySessions) {
		t.Fatalf("a second session under a cap of one must fail with ErrTooManySessions, got %v", err)
	}
	if got := g.Sessions(); len(got) != 1 {
		t.Fatalf("sessions = %v, want one", got)
	}
}

func TestFetchSendsSessionState(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	resp, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/items"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Status != 200 || resp.Body != "ok:https://portal.example/items" {
		t.Fatalf("response = %d/%q, want the upstream body", resp.Status, resp.Body)
	}
	token, sid := site.lastOutbound()
	if token != "T0" || sid != "s-1" {
		t.Fatalf("outbound carried token %q and cookie %q, want T0 and s-1", token, sid)
	}
	if stats := g.Stats(); stats.Fetches != 1 {
		t.Fatalf("stats = %+v, want one fetch", stats)
	}
	if calls := site.dataCalls(); len(calls) != 1 {
		t.Fatalf("upstream calls = %v, want exactly one", calls)
	}
}

func TestFetchRotatesExpiredToken(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	site.expire("T1")
	resp, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/items"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d, want the replayed request to succeed", resp.Status)
	}
	if site.rotateCount() != 1 {
		t.Fatalf("token rotations = %d, want 1", site.rotateCount())
	}
	stats := g.Stats()
	if stats.Rotations != 1 || stats.Replays != 1 {
		t.Fatalf("stats = %+v, want one rotation and one replay", stats)
	}
	stored, ok := g.SessionState(sess.ID)
	if !ok || stored.Token != "T1" {
		t.Fatalf("stored session = %+v, want the rotated token T1", stored)
	}
	if stored.Gen != 2 {
		t.Fatalf("generation = %d, want 2 after one rotation", stored.Gen)
	}
	if calls := site.dataCalls(); len(calls) != 2 {
		t.Fatalf("upstream calls = %v, want the original and one replay", calls)
	}
}

func TestFetchReplaysAtMostOnce(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	site.failAll = true
	if _, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/items")); !errors.Is(err, impl.ErrReplayExhausted) {
		t.Fatalf("a persistent expiry must fail with ErrReplayExhausted, got %v", err)
	}
	if calls := site.dataCalls(); len(calls) != 2 {
		t.Fatalf("upstream calls = %v, want the original and exactly one replay", calls)
	}
	if stats := g.Stats(); stats.Replays != 1 {
		t.Fatalf("stats = %+v, want one replay attempt", stats)
	}
}

func TestFetchReloginsDroppedSession(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	site.dropOnce = true
	resp, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/items"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d, want the replayed request to succeed", resp.Status)
	}
	if site.loginCount() != 2 {
		t.Fatalf("logins = %d, want a second login for the dropped session", site.loginCount())
	}
	stats := g.Stats()
	if stats.Logins != 2 || stats.Replays != 1 {
		t.Fatalf("stats = %+v, want two logins and one replay", stats)
	}
}

func TestFetchRejectsUnknownSessionAndBlankURL(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	if _, err := g.Fetch(context.Background(), getReq("sess-404", "https://portal.example/a")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("an unknown session must fail with ErrNoSession, got %v", err)
	}
	if _, err := g.Fetch(context.Background(), getReq(sess.ID, " ")); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("a blank url must fail with ErrBadRequest, got %v", err)
	}
	if calls := site.dataCalls(); len(calls) != 0 {
		t.Fatalf("rejected requests must not reach the transport, got %v", calls)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	if err := g.Refresh(sess.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	stored, ok := g.SessionState(sess.ID)
	if !ok || stored.Token != "T1" || stored.Gen != 2 {
		t.Fatalf("stored session = %+v, want token T1 and generation 2", stored)
	}
	if stats := g.Stats(); stats.Rotations != 1 {
		t.Fatalf("stats = %+v, want one rotation", stats)
	}
	if resp, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/a")); err != nil || resp.Status != 200 {
		t.Fatalf("a fetch after a refresh must succeed, got %v/%v", resp.Status, err)
	}
	if err := g.Refresh("sess-404"); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("refreshing an unknown session must fail with ErrNoSession, got %v", err)
	}
}

func TestFingerprintStableAcrossFieldOrder(t *testing.T) {
	g := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 4, MaxReplays: 1})
	first := impl.Resp{Form: impl.Form{
		Action: "/save",
		Fields: []string{"title", "body", "tags"},
		Hidden: map[string]string{"csrf": "abc", "version": "2"},
	}}
	second := impl.Resp{Form: impl.Form{
		Action: "/save",
		Fields: []string{"tags", "title", "body"},
		Hidden: map[string]string{"version": "2", "csrf": "abc"},
	}}
	fp := g.Fingerprint(first)
	if len(fp) != 64 {
		t.Fatalf("fingerprint = %q, want 64 hex characters", fp)
	}
	if other := g.Fingerprint(second); other != fp {
		t.Fatalf("fingerprint changed with field order: %q vs %q", fp, other)
	}
}

func TestFingerprintDetectsActionAndHiddenChanges(t *testing.T) {
	g := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 4, MaxReplays: 1})
	base := impl.Resp{Form: impl.Form{
		Action: "/save",
		Fields: []string{"title", "body"},
		Hidden: map[string]string{"csrf": "abc"},
	}}
	fp := g.Fingerprint(base)
	action := impl.Resp{Form: impl.Form{
		Action: "/publish",
		Fields: []string{"title", "body"},
		Hidden: map[string]string{"csrf": "abc"},
	}}
	if g.Fingerprint(action) == fp {
		t.Fatalf("a different form action must change the fingerprint")
	}
	hidden := impl.Resp{Form: impl.Form{
		Action: "/save",
		Fields: []string{"title", "body"},
		Hidden: map[string]string{"csrf": "abd"},
	}}
	if g.Fingerprint(hidden) == fp {
		t.Fatalf("a different hidden value must change the fingerprint")
	}
	fields := impl.Resp{Form: impl.Form{
		Action: "/save",
		Fields: []string{"title", "body", "extra"},
		Hidden: map[string]string{"csrf": "abc"},
	}}
	if g.Fingerprint(fields) == fp {
		t.Fatalf("a different field name set must change the fingerprint")
	}
}

func TestConcurrentFetchSerialisesRotation(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	site.expire("T1")
	const workers = 8
	var wg sync.WaitGroup
	failures := make([]error, workers)
	statuses := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/list"))
			if err != nil {
				failures[idx] = err
				return
			}
			statuses[idx] = resp.Status
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if failures[i] != nil {
			t.Fatalf("worker %d failed: %v", i, failures[i])
		}
		if statuses[i] != 200 {
			t.Fatalf("worker %d status = %d, want 200", i, statuses[i])
		}
	}
	if site.rotateCount() != 1 {
		t.Fatalf("token rotations = %d, want exactly one rotation for the whole batch", site.rotateCount())
	}
	if stats := g.Stats(); stats.Rotations != 1 {
		t.Fatalf("stats = %+v, want one rotation", stats)
	}
}

func TestSnapshotRestoreKeepsSessions(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	if _, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/a")); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewCrawlSessionGuard(impl.Limits{MaxSessions: 4, MaxReplays: 1})
	if err := restored.SetTransport(site); err != nil {
		t.Fatalf("set transport: %v", err)
	}
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Sessions(); len(got) != 1 || got[0] != sess.ID {
		t.Fatalf("restored sessions = %v, want [%s]", got, sess.ID)
	}
	stored, ok := restored.SessionState(sess.ID)
	if !ok || stored.Token != "T0" || !stored.LoggedIn {
		t.Fatalf("restored session = %+v, want the logged in session", stored)
	}
	if stats := restored.Stats(); stats.Fetches != 1 || stats.Logins != 1 {
		t.Fatalf("restored stats = %+v, want the counters to survive", stats)
	}
	if resp, err := restored.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/b")); err != nil || resp.Status != 200 {
		t.Fatalf("a restored session must still fetch, got %v/%v", resp.Status, err)
	}
	if err := restored.Restore([]byte("###broken###")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Sessions(); len(got) != 1 {
		t.Fatalf("a rejected restore must not touch the sessions, got %v", got)
	}
}

func TestCloseDropsSession(t *testing.T) {
	site := newFakeSite()
	g, sess := newGuard(t, site)
	if err := g.Close(sess.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := g.Sessions(); len(got) != 0 {
		t.Fatalf("a closed session must be gone, got %v", got)
	}
	if _, err := g.Fetch(context.Background(), getReq(sess.ID, "https://portal.example/a")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("fetching a closed session must fail with ErrNoSession, got %v", err)
	}
	if err := g.Close(sess.ID); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("closing a session twice must fail with ErrNoSession, got %v", err)
	}
}
