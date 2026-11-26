package task

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-18"
)

var cookieBase = time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)

func cookieCfg(clock *time.Time) impl.CookieConfig {
	return impl.CookieConfig{
		ServerCookieLen:  16,
		RotationInterval: time.Hour,
		GracePeriod:      10 * time.Minute,
		RetryWindow:      30 * time.Second,
		MaxRetries:       2,
		RequireServer:    true,
		Key:              []byte("cookie-secret-material-32-bytes!"),
		Now:              func() time.Time { return *clock },
	}
}

func clientCookie() []byte {
	return []byte("cook-123")
}

func cookieAddr() netip.Addr {
	return netip.MustParseAddr("192.0.2.10")
}

func otherCookieAddr() netip.Addr {
	return netip.MustParseAddr("192.0.2.11")
}

func TestIssueIsDeterministicPerClientAndAddr(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	first, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(first.Server) != 16 {
		t.Fatalf("server cookie length = %d, want 16", len(first.Server))
	}
	if string(first.Client) != string(clientCookie()) {
		t.Fatalf("issued client cookie = %q, want the request value", first.Client)
	}
	second, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}
	if string(second.Server) != string(first.Server) {
		t.Fatalf("same inputs produced %x then %x", first.Server, second.Server)
	}
	third, err := v.Issue(impl.Cookie{Client: clientCookie()}, otherCookieAddr())
	if err != nil {
		t.Fatalf("third issue: %v", err)
	}
	if string(third.Server) == string(first.Server) {
		t.Fatalf("server cookie ignores the client address")
	}
}

func TestIssueRejectsBadClientCookieOrAddr(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	if _, err := v.Issue(impl.Cookie{Client: []byte("short")}, cookieAddr()); !errors.Is(err, impl.ErrBadClientCookie) {
		t.Fatalf("seven byte client cookie: err = %v, want ErrBadClientCookie", err)
	}
	if _, err := v.Issue(impl.Cookie{Client: clientCookie()}, netip.Addr{}); !errors.Is(err, impl.ErrBadAddr) {
		t.Fatalf("zero address: err = %v, want ErrBadAddr", err)
	}
}

func TestCheckAcceptsMatchingCookie(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	issued, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	verdict, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Server: issued.Server,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(issued.Server),
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if verdict.Action != impl.ActionAccept {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionAccept)
	}
	if verdict.Reason != impl.CauseCurrentKey {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CauseCurrentKey)
	}
	if verdict.Degraded {
		t.Fatalf("a full valid cookie must not be counted as degraded")
	}
	if got := v.Stats().Accepted; got != 1 {
		t.Fatalf("accepted counter = %d, want 1", got)
	}
}

func TestCheckAcceptsRequestWithoutCookie(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	verdict, err := v.Check(impl.CookieRequest{Addr: cookieAddr(), Now: clock})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if verdict.Action != impl.ActionAccept {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionAccept)
	}
	if verdict.Reason != impl.CauseCookieAbsent {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CauseCookieAbsent)
	}
	if verdict.Degraded {
		t.Fatalf("a first query without a cookie is not a degradation")
	}
	stats := v.Stats()
	if stats.Accepted != 1 || stats.Degraded != 0 {
		t.Fatalf("stats = %+v, want 1 accepted and no degradation", stats)
	}
}

func TestCheckReturnsBadCookieForClientOnly(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	first, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen,
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if first.Action != impl.ActionBadCookie {
		t.Fatalf("action = %q, want %q", first.Action, impl.ActionBadCookie)
	}
	if !first.Degraded {
		t.Fatalf("client only request must be counted as degraded")
	}
	if len(first.Cookie.Server) != 16 {
		t.Fatalf("bad cookie answer carries %d server bytes, want 16", len(first.Cookie.Server))
	}
	if string(first.Cookie.Client) != string(clientCookie()) {
		t.Fatalf("bad cookie answer lost the client cookie")
	}
	clock = clock.Add(5 * time.Second)
	second, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen,
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if second.Action != impl.ActionBadCookie {
		t.Fatalf("second action = %q, want %q", second.Action, impl.ActionBadCookie)
	}
	if second.Degraded {
		t.Fatalf("the retry window must not count the same client twice")
	}
	if got := v.Stats().Degraded; got != 1 {
		t.Fatalf("degraded counter = %d, want 1", got)
	}
	if got := v.DegradedCount(cookieAddr()); got != 1 {
		t.Fatalf("degraded count for the address = %d, want 1", got)
	}
}

func TestCheckDropsRepeatedDegradation(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	for i := 0; i < 2; i++ {
		verdict, err := v.Check(impl.CookieRequest{
			Client: clientCookie(),
			Addr:   cookieAddr(),
			RawLen: impl.ClientCookieLen,
			Now:    clock,
		})
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if verdict.Action != impl.ActionBadCookie {
			t.Fatalf("check %d action = %q, want %q", i, verdict.Action, impl.ActionBadCookie)
		}
		clock = clock.Add(time.Second)
	}
	verdict, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen,
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("third check: %v", err)
	}
	if verdict.Action != impl.ActionDrop {
		t.Fatalf("third action = %q, want %q", verdict.Action, impl.ActionDrop)
	}
	if verdict.Reason != impl.CauseRetryStorm {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CauseRetryStorm)
	}
	if got := v.Stats().Dropped; got != 1 {
		t.Fatalf("dropped counter = %d, want 1", got)
	}
}

func TestCheckDropsMalformedClientCookie(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	verdict, err := v.Check(impl.CookieRequest{
		Client: []byte("short"),
		Addr:   cookieAddr(),
		RawLen: 5,
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if verdict.Action != impl.ActionDrop {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionDrop)
	}
	if verdict.Reason != impl.CauseMalformed {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CauseMalformed)
	}
	if got := v.Stats().Dropped; got != 1 {
		t.Fatalf("dropped counter = %d, want 1", got)
	}
}

func TestCheckRejectsForgedServerCookie(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	forged := make([]byte, 16)
	for i := range forged {
		forged[i] = 0xAB
	}
	verdict, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Server: forged,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(forged),
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if verdict.Action != impl.ActionBadCookie {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionBadCookie)
	}
	if verdict.Reason != impl.CauseForgedServer {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CauseForgedServer)
	}
	if len(verdict.Cookie.Server) != 16 {
		t.Fatalf("retry answer carries %d server bytes, want a fresh 16", len(verdict.Cookie.Server))
	}
	stats := v.Stats()
	if stats.BadCookie != 1 || stats.Accepted != 0 {
		t.Fatalf("stats = %+v, want one bad cookie and no acceptance", stats)
	}
}

func TestRotatePromotesGeneration(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	gen, at := v.Generation()
	if gen != 1 {
		t.Fatalf("generation = %d, want 1", gen)
	}
	if !at.Equal(cookieBase) {
		t.Fatalf("rotation instant = %v, want %v", at, cookieBase)
	}
	clock = cookieBase.Add(time.Hour)
	if err := v.Rotate(clock); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	gen, at = v.Generation()
	if gen != 2 {
		t.Fatalf("generation = %d, want 2", gen)
	}
	if !at.Equal(clock) {
		t.Fatalf("rotation instant = %v, want %v", at, clock)
	}
	if got := v.Stats().Rotations; got != 1 {
		t.Fatalf("rotations counter = %d, want 1", got)
	}
}

func TestRotateRejectsBeforeInterval(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	clock = cookieBase.Add(10 * time.Minute)
	if err := v.Rotate(clock); !errors.Is(err, impl.ErrRotateTooSoon) {
		t.Fatalf("early rotate: err = %v, want ErrRotateTooSoon", err)
	}
	gen, at := v.Generation()
	if gen != 1 || !at.Equal(cookieBase) {
		t.Fatalf("rejected rotation moved the key: gen=%d at=%v", gen, at)
	}
}

func TestPreviousKeyAcceptedWithinGrace(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	issued, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	clock = cookieBase.Add(time.Hour)
	if err := v.Rotate(clock); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	clock = clock.Add(time.Minute)
	verdict, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Server: issued.Server,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(issued.Server),
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if verdict.Action != impl.ActionAccept {
		t.Fatalf("action = %q, want %q inside the grace period", verdict.Action, impl.ActionAccept)
	}
	if verdict.Reason != impl.CausePreviousKey {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CausePreviousKey)
	}
	if got := v.Stats().PrevKeyHits; got != 1 {
		t.Fatalf("previous key hits = %d, want 1", got)
	}
}

func TestPreviousKeyRejectedAfterGrace(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	issued, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	clock = cookieBase.Add(time.Hour)
	if err := v.Rotate(clock); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	clock = clock.Add(10*time.Minute + time.Second)
	verdict, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Server: issued.Server,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(issued.Server),
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if verdict.Action != impl.ActionDrop {
		t.Fatalf("action = %q, want %q past the grace period", verdict.Action, impl.ActionDrop)
	}
	if verdict.Reason != impl.CauseStaleKey {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.CauseStaleKey)
	}
	if got := v.Stats().PrevKeyHits; got != 0 {
		t.Fatalf("stale cookie counted as a previous key hit: %d", got)
	}
}

func TestStatsCountsEveryAction(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	issued, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Server: issued.Server,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(issued.Server),
		Now:    clock,
	}); err != nil {
		t.Fatalf("accept check: %v", err)
	}
	if _, err := v.Check(impl.CookieRequest{
		Client: []byte("bad"),
		Addr:   cookieAddr(),
		RawLen: 3,
		Now:    clock,
	}); err != nil {
		t.Fatalf("drop check: %v", err)
	}
	if _, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Addr:   otherCookieAddr(),
		RawLen: impl.ClientCookieLen,
		Now:    clock,
	}); err != nil {
		t.Fatalf("client only check: %v", err)
	}
	stats := v.Stats()
	if stats.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", stats.Accepted)
	}
	if stats.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", stats.Dropped)
	}
	if stats.BadCookie != 1 {
		t.Fatalf("bad cookie = %d, want 1", stats.BadCookie)
	}
	if stats.Degraded != 1 {
		t.Fatalf("degraded = %d, want 1", stats.Degraded)
	}
}

func TestSnapshotRestoreKeepsGenerationAndCounters(t *testing.T) {
	clock := cookieBase
	cfg := cookieCfg(&clock)
	v := impl.NewDnsCookieValidator(cfg)
	issued, err := v.Issue(impl.Cookie{Client: clientCookie()}, cookieAddr())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := v.Check(impl.CookieRequest{
		Client: clientCookie(),
		Server: issued.Server,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(issued.Server),
		Now:    clock,
	}); err != nil {
		t.Fatalf("check: %v", err)
	}
	clock = cookieBase.Add(time.Hour)
	if err := v.Rotate(clock); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewDnsCookieValidator(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	gen, at := restored.Generation()
	if gen != 2 {
		t.Fatalf("generation after restore = %d, want 2", gen)
	}
	if !at.Equal(clock) {
		t.Fatalf("rotation instant after restore = %v, want %v", at, clock)
	}
	if got := restored.Stats().Accepted; got != 1 {
		t.Fatalf("accepted counter after restore = %d, want 1", got)
	}
	fresh, err := restored.Issue(impl.Cookie{Client: []byte("cook-999")}, cookieAddr())
	if err != nil {
		t.Fatalf("issue after restore: %v", err)
	}
	verdict, err := restored.Check(impl.CookieRequest{
		Client: []byte("cook-999"),
		Server: fresh.Server,
		Addr:   cookieAddr(),
		RawLen: impl.ClientCookieLen + len(fresh.Server),
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("check after restore: %v", err)
	}
	if verdict.Action != impl.ActionAccept {
		t.Fatalf("restored validator rejected its own cookie: %+v", verdict)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	clock := cookieBase
	v := impl.NewDnsCookieValidator(cookieCfg(&clock))
	for _, payload := range [][]byte{nil, []byte("null"), []byte("{bad"), []byte(`{"version":9}`)} {
		if err := v.Restore(payload); !errors.Is(err, impl.ErrBadSnapshot) {
			t.Fatalf("payload %q: err = %v, want ErrBadSnapshot", string(payload), err)
		}
	}
	gen, at := v.Generation()
	if gen != 1 || !at.Equal(cookieBase) {
		t.Fatalf("corrupt restore touched the key state: gen=%d at=%v", gen, at)
	}
	if err := v.Reset(clock); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := v.Stats().Accepted; got != 0 {
		t.Fatalf("reset left %d accepted entries", got)
	}
	if got := v.DegradedCount(cookieAddr()); got != 0 {
		t.Fatalf("reset left %d degraded entries", got)
	}
}
