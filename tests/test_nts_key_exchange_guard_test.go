package task

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-20"
)

var ntsBase = time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)

const ntsAlgorithm = "AEAD_AES_SIV_CMAC_256"

func ntsConfig(clock *time.Time) impl.NtsConfig {
	return impl.NtsConfig{
		AllowedAlgorithms:  []string{ntsAlgorithm},
		CookiesPerExchange: impl.DefaultCookiesPerExchange,
		LowWatermark:       2,
		SkewWindow:         2 * time.Minute,
		PoolCap:            impl.DefaultCookiesPerExchange,
		Now:                func() time.Time { return *clock },
	}
}

func ntsNonce() []byte {
	return []byte("0123456789abcdef")
}

func ntsOtherNonce() []byte {
	return []byte("fedcba9876543210")
}

func ntsCookieSet(n int) []impl.Cookie {
	out := make([]impl.Cookie, n)
	for i := range out {
		out[i] = impl.Cookie{
			ID:   []byte(fmt.Sprintf("cookie-%02d", i)),
			Body: []byte("opaque-cookie-body"),
		}
	}
	return out
}

func ntsResponse(nonce []byte, at time.Time) impl.KeResponse {
	return impl.KeResponse{
		Server:       "time.example",
		RequestNonce: nonce,
		UniqueID:     []byte("uniq-id-1"),
		Timestamp:    at,
		Algorithm:    ntsAlgorithm,
		C2SKey:       bytes.Repeat([]byte{0x11}, 32),
		S2CKey:       bytes.Repeat([]byte{0x22}, 32),
		Cookies:      ntsCookieSet(impl.DefaultCookiesPerExchange),
		Negotiated:   []string{"ntske/1"},
	}
}

func ntsPacket(at time.Time, cookieID []byte) impl.Packet {
	return impl.Packet{
		Server:    "time.example",
		CookieID:  cookieID,
		UniqueID:  []byte("uniq-id-1"),
		Timestamp: at,
		Algorithm: ntsAlgorithm,
	}
}

func mustEstablish(t *testing.T, g *impl.NtsKeyExchangeGuard, at time.Time) impl.Association {
	t.Helper()
	if _, err := g.Begin("time.example", ntsNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	assoc, err := g.Establish(ntsResponse(ntsNonce(), at))
	if err != nil {
		t.Fatalf("establish: %v", err)
	}
	return assoc
}

func TestBeginRejectsBlankServerAndBadNonce(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Begin("  ", ntsNonce()); !errors.Is(err, impl.ErrBlankServer) {
		t.Fatalf("blank server: err = %v, want ErrBlankServer", err)
	}
	if _, err := g.Begin("time.example", []byte("short")); !errors.Is(err, impl.ErrBadNonce) {
		t.Fatalf("short nonce: err = %v, want ErrBadNonce", err)
	}
	req, err := g.Begin("time.example", ntsNonce())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if req.Server != "time.example" {
		t.Fatalf("request server = %q, want time.example", req.Server)
	}
	if !bytes.Equal(req.Nonce, ntsNonce()) {
		t.Fatalf("request nonce = %x, want %x", req.Nonce, ntsNonce())
	}
	if !req.SentAt.Equal(clock) {
		t.Fatalf("sent at = %v, want %v", req.SentAt, clock)
	}
}

func TestEstablishRequiresPendingRequest(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Establish(ntsResponse(ntsNonce(), clock)); !errors.Is(err, impl.ErrNoPendingRequest) {
		t.Fatalf("establish without begin: err = %v, want ErrNoPendingRequest", err)
	}
	if _, err := g.Status(); !errors.Is(err, impl.ErrNoAssociation) {
		t.Fatalf("status: err = %v, want ErrNoAssociation", err)
	}
}

func TestEstablishRejectsNonceMismatch(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Begin("time.example", ntsNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Establish(ntsResponse(ntsOtherNonce(), clock)); !errors.Is(err, impl.ErrNonceMismatch) {
		t.Fatalf("wrong nonce: err = %v, want ErrNonceMismatch", err)
	}
	assoc, err := g.Establish(ntsResponse(ntsNonce(), clock))
	if err != nil {
		t.Fatalf("correct nonce after a mismatch: %v", err)
	}
	if assoc.Cookies != impl.DefaultCookiesPerExchange {
		t.Fatalf("cookies = %d, want %d", assoc.Cookies, impl.DefaultCookiesPerExchange)
	}
}

func TestEstablishRejectsReplayedNonce(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	if _, err := g.Begin("time.example", ntsNonce()); !errors.Is(err, impl.ErrReplayNonce) {
		t.Fatalf("replayed nonce: err = %v, want ErrReplayNonce", err)
	}
	if _, err := g.Begin("time.example", ntsOtherNonce()); err != nil {
		t.Fatalf("fresh nonce: %v", err)
	}
}

func TestEstablishRejectsBadKeyMaterial(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Begin("time.example", ntsNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	shortC2S := ntsResponse(ntsNonce(), clock)
	shortC2S.C2SKey = bytes.Repeat([]byte{0x11}, 16)
	if _, err := g.Establish(shortC2S); !errors.Is(err, impl.ErrBadKeyMaterial) {
		t.Fatalf("short c2s key: err = %v, want ErrBadKeyMaterial", err)
	}
	shortS2C := ntsResponse(ntsNonce(), clock)
	shortS2C.S2CKey = bytes.Repeat([]byte{0x22}, 31)
	if _, err := g.Establish(shortS2C); !errors.Is(err, impl.ErrBadKeyMaterial) {
		t.Fatalf("short s2c key: err = %v, want ErrBadKeyMaterial", err)
	}
	if _, err := g.Status(); !errors.Is(err, impl.ErrNoAssociation) {
		t.Fatalf("rejected material installed an association: %v", err)
	}
}

func TestEstablishRequiresEightCookies(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Begin("time.example", ntsNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	fewer := ntsResponse(ntsNonce(), clock)
	fewer.Cookies = ntsCookieSet(7)
	if _, err := g.Establish(fewer); !errors.Is(err, impl.ErrCookieCount) {
		t.Fatalf("seven cookies: err = %v, want ErrCookieCount", err)
	}
	if got := g.PoolSize(); got != 0 {
		t.Fatalf("pool size = %d, want 0 after a rejected response", got)
	}
}

func TestEstablishRejectsStaleTimestamp(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Begin("time.example", ntsNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	old := ntsResponse(ntsNonce(), clock.Add(-10*time.Minute))
	if _, err := g.Establish(old); !errors.Is(err, impl.ErrTimestampWindow) {
		t.Fatalf("old timestamp: err = %v, want ErrTimestampWindow", err)
	}
	if _, err := g.Begin("time.example", ntsOtherNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	future := ntsResponse(ntsOtherNonce(), clock.Add(10*time.Minute))
	if _, err := g.Establish(future); !errors.Is(err, impl.ErrTimestampWindow) {
		t.Fatalf("future timestamp: err = %v, want ErrTimestampWindow", err)
	}
}

func TestEstablishReturnsAssociation(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	if _, err := g.Begin("time.example", ntsNonce()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	assoc, err := g.Establish(ntsResponse(ntsNonce(), clock))
	if err != nil {
		t.Fatalf("establish: %v", err)
	}
	if assoc.Server != "time.example" || assoc.Algorithm != ntsAlgorithm {
		t.Fatalf("association = %+v", assoc)
	}
	if assoc.Generation != 1 {
		t.Fatalf("generation = %d, want 1", assoc.Generation)
	}
	if assoc.Cookies != 8 {
		t.Fatalf("cookies = %d, want 8", assoc.Cookies)
	}
	if !bytes.Equal(assoc.UniqueID, []byte("uniq-id-1")) {
		t.Fatalf("unique id = %q, want uniq-id-1", assoc.UniqueID)
	}
	if !assoc.EstablishedAt.Equal(clock) {
		t.Fatalf("established at = %v, want %v", assoc.EstablishedAt, clock)
	}
	if got := g.PoolSize(); got != 8 {
		t.Fatalf("pool size = %d, want 8", got)
	}
	status, err := g.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Server != "time.example" || status.Cookies != 8 {
		t.Fatalf("status = %+v", status)
	}
	if got := g.Stats().Established; got != 1 {
		t.Fatalf("established counter = %d, want 1", got)
	}
}

func TestTakeCookiePopsFifo(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	for i := 0; i < 3; i++ {
		cookie, err := g.TakeCookie()
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		want := fmt.Sprintf("cookie-%02d", i)
		if string(cookie.ID) != want {
			t.Fatalf("take %d id = %q, want %q", i, cookie.ID, want)
		}
	}
	if got := g.PoolSize(); got != 5 {
		t.Fatalf("pool size = %d, want 5", got)
	}
}

func TestTakeCookieShrinksPoolAndFlagsRekey(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	for i := 0; i < 5; i++ {
		if _, err := g.TakeCookie(); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	status, err := g.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Cookies != 3 {
		t.Fatalf("remaining = %d, want 3", status.Cookies)
	}
	if status.NeedsRekey {
		t.Fatalf("rekey must not be flagged above the low watermark")
	}
	if _, err := g.TakeCookie(); err != nil {
		t.Fatalf("take sixth: %v", err)
	}
	status, err = g.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Cookies != 2 {
		t.Fatalf("remaining = %d, want 2", status.Cookies)
	}
	if !status.NeedsRekey {
		t.Fatalf("rekey must be flagged at the low watermark")
	}
}

func TestAuthenticateAcceptsFreshCookie(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	cookie, err := g.TakeCookie()
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	verdict, err := g.Authenticate(ntsPacket(clock, cookie.ID))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !verdict.Accept {
		t.Fatalf("fresh cookie rejected: %+v", verdict)
	}
	if verdict.Remaining != 7 {
		t.Fatalf("remaining = %d, want 7", verdict.Remaining)
	}
	if verdict.NeedsRekey {
		t.Fatalf("rekey flagged with seven cookies left")
	}
	if got := g.Stats().CookieUsed; got != 1 {
		t.Fatalf("cookie used counter = %d, want 1", got)
	}
}

func TestAuthenticateRejectsUnknownCookie(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	verdict, err := g.Authenticate(ntsPacket(clock, []byte("cookie-99")))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if verdict.Accept {
		t.Fatalf("unknown cookie accepted: %+v", verdict)
	}
	if verdict.Reason != impl.ReasonUnknownCookie {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonUnknownCookie)
	}
	if _, err := g.TakeCookie(); err != nil {
		t.Fatalf("take: %v", err)
	}
	unused, err := g.Authenticate(ntsPacket(clock, []byte("cookie-01")))
	if err != nil {
		t.Fatalf("authenticate unused cookie: %v", err)
	}
	if unused.Accept {
		t.Fatalf("cookie still sitting in the pool accepted: %+v", unused)
	}
	if got := g.Stats().Rejected; got != 2 {
		t.Fatalf("rejected counter = %d, want 2", got)
	}
}

func TestAuthenticateRejectsTimestampOutsideWindow(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	cookie, err := g.TakeCookie()
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	verdict, err := g.Authenticate(ntsPacket(clock.Add(-5*time.Minute), cookie.ID))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if verdict.Accept {
		t.Fatalf("stale packet accepted: %+v", verdict)
	}
	if verdict.Reason != impl.ReasonTimestampWindow {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonTimestampWindow)
	}
	if got := g.Stats().Rejected; got != 1 {
		t.Fatalf("rejected counter = %d, want 1", got)
	}
}

func TestRotateInvalidatesAssociation(t *testing.T) {
	clock := ntsBase
	g := impl.NewNtsKeyExchangeGuard(ntsConfig(&clock))
	mustEstablish(t, g, clock)
	if err := g.Rotate("backup.example"); !errors.Is(err, impl.ErrReestablishRequired) {
		t.Fatalf("server change: err = %v, want ErrReestablishRequired", err)
	}
	if _, err := g.Status(); !errors.Is(err, impl.ErrNoAssociation) {
		t.Fatalf("status after rotation: err = %v, want ErrNoAssociation", err)
	}
	if _, err := g.TakeCookie(); !errors.Is(err, impl.ErrNoAssociation) {
		t.Fatalf("take after rotation: err = %v, want ErrNoAssociation", err)
	}
	if _, err := g.Authenticate(ntsPacket(clock, []byte("cookie-00"))); !errors.Is(err, impl.ErrNoAssociation) {
		t.Fatalf("authenticate after rotation: err = %v, want ErrNoAssociation", err)
	}
	if got := g.PoolSize(); got != 0 {
		t.Fatalf("pool size after rotation = %d, want 0", got)
	}
}

func TestSnapshotRestoreKeepsConsumedCookies(t *testing.T) {
	clock := ntsBase
	cfg := ntsConfig(&clock)
	g := impl.NewNtsKeyExchangeGuard(cfg)
	mustEstablish(t, g, clock)
	first, err := g.TakeCookie()
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := g.TakeCookie(); err != nil {
		t.Fatalf("take: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewNtsKeyExchangeGuard(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.PoolSize(); got != 6 {
		t.Fatalf("pool size after restore = %d, want 6", got)
	}
	next, err := restored.TakeCookie()
	if err != nil {
		t.Fatalf("take after restore: %v", err)
	}
	if string(next.ID) != "cookie-02" {
		t.Fatalf("next cookie = %q, want cookie-02", next.ID)
	}
	reused, err := restored.Authenticate(ntsPacket(clock, first.ID))
	if err != nil {
		t.Fatalf("authenticate consumed cookie: %v", err)
	}
	if reused.Accept {
		t.Fatalf("consumed cookie revived after restore: %+v", reused)
	}
	if reused.Reason != impl.ReasonCookieReplay && reused.Reason != impl.ReasonUnknownCookie {
		t.Fatalf("reason = %q, want a replay style rejection", reused.Reason)
	}
	if err := restored.Restore([]byte("garbage")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt restore: err = %v, want ErrBadSnapshot", err)
	}
	if got := restored.PoolSize(); got != 5 {
		t.Fatalf("corrupt restore changed the pool to %d, want 5", got)
	}
}
