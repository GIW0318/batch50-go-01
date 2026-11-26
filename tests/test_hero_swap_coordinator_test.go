package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-15"
)

func swapLimits() impl.Limits {
	return impl.Limits{
		MaxPendingPerMatch:  4,
		MaxRequestsPerMatch: 10,
		RequestTTL:          10 * time.Minute,
	}
}

func rosterPicks() map[string]string {
	return map[string]string{
		"p1": "hero-axe",
		"p2": "hero-mage",
		"p3": "hero-healer",
	}
}

func TestLockRosterStoresPicks(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-1", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	picks, err := c.Roster("m-1")
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(picks) != 3 {
		t.Fatalf("roster size = %d, want 3", len(picks))
	}
	if picks["p1"] != "hero-axe" || picks["p2"] != "hero-mage" || picks["p3"] != "hero-healer" {
		t.Fatalf("roster = %v, want the locked picks", picks)
	}
	if got := len(c.Pending("m-1")); got != 0 {
		t.Fatalf("a fresh roster must have no pending swaps, got %d", got)
	}
}

func TestLockRosterRejectsDuplicateHero(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	clash := map[string]string{"p1": "hero-axe", "p2": "hero-axe"}
	if err := c.LockRoster("m-dup", clash); !errors.Is(err, impl.ErrDuplicateHero) {
		t.Fatalf("two players on one hero must fail with ErrDuplicateHero, got %v", err)
	}
	if _, err := c.Roster("m-dup"); !errors.Is(err, impl.ErrUnknownMatch) {
		t.Fatalf("a rejected roster must not be stored, got %v", err)
	}
}

func TestLockRosterRejectsEmptyPicks(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("   ", rosterPicks()); !errors.Is(err, impl.ErrBlankMatch) {
		t.Fatalf("blank match id must fail with ErrBlankMatch, got %v", err)
	}
	if err := c.LockRoster("m-2", nil); !errors.Is(err, impl.ErrEmptyRoster) {
		t.Fatalf("an empty roster must fail with ErrEmptyRoster, got %v", err)
	}
	if err := c.LockRoster("m-2", map[string]string{"p1": "   "}); !errors.Is(err, impl.ErrEmptyRoster) {
		t.Fatalf("a blank hero must fail with ErrEmptyRoster, got %v", err)
	}
	if err := c.LockRoster("m-2", map[string]string{"  ": "hero-axe"}); !errors.Is(err, impl.ErrEmptyRoster) {
		t.Fatalf("a blank player must fail with ErrEmptyRoster, got %v", err)
	}
	if _, err := c.Roster("m-2"); !errors.Is(err, impl.ErrUnknownMatch) {
		t.Fatalf("rejected rosters must not be stored, got %v", err)
	}
}

func TestRequestCreatesPendingSwap(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-3", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	req, err := c.Request("m-3", "p1", "p2")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if req.RequestID == "" {
		t.Fatalf("a request must carry an id")
	}
	if req.MatchID != "m-3" || req.FromPlayer != "p1" || req.ToPlayer != "p2" {
		t.Fatalf("request = %+v, want p1 asking p2 in m-3", req)
	}
	if req.State != impl.SwapPending {
		t.Fatalf("request state = %q, want %q", req.State, impl.SwapPending)
	}
	if !req.ExpiresAt.After(req.CreatedAt) {
		t.Fatalf("request expiry %v must be after creation %v", req.ExpiresAt, req.CreatedAt)
	}
	pending := c.Pending("m-3")
	if len(pending) != 1 || pending[0].RequestID != req.RequestID {
		t.Fatalf("pending = %+v, want exactly the new request", pending)
	}
}

func TestRequestRejectsSelfSwap(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-4", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	if _, err := c.Request("m-4", "p1", "p1"); !errors.Is(err, impl.ErrSelfSwap) {
		t.Fatalf("swapping with oneself must fail with ErrSelfSwap, got %v", err)
	}
	if got := len(c.Pending("m-4")); got != 0 {
		t.Fatalf("rejected requests must not be stored, got %d", got)
	}
}

func TestRequestRejectsUnknownPlayer(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-5", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	if _, err := c.Request("m-5", "p9", "p2"); !errors.Is(err, impl.ErrUnknownPlayer) {
		t.Fatalf("a sender outside the roster must fail with ErrUnknownPlayer, got %v", err)
	}
	if _, err := c.Request("m-5", "p1", "p9"); !errors.Is(err, impl.ErrUnknownPlayer) {
		t.Fatalf("a recipient outside the roster must fail with ErrUnknownPlayer, got %v", err)
	}
	if _, err := c.Request("m-404", "p1", "p2"); !errors.Is(err, impl.ErrUnknownMatch) {
		t.Fatalf("a request on an unknown match must fail with ErrUnknownMatch, got %v", err)
	}
}

func TestRequestRejectsSecondPendingForSamePlayer(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-6", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	if _, err := c.Request("m-6", "p1", "p2"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := c.Request("m-6", "p1", "p3"); !errors.Is(err, impl.ErrPendingExists) {
		t.Fatalf("a sender with a pending swap must fail with ErrPendingExists, got %v", err)
	}
	if _, err := c.Request("m-6", "p3", "p2"); !errors.Is(err, impl.ErrPendingExists) {
		t.Fatalf("a recipient with a pending swap must fail with ErrPendingExists, got %v", err)
	}
	if _, err := c.Request("m-6", "p2", "p3"); !errors.Is(err, impl.ErrPendingExists) {
		t.Fatalf("the other side of a pending swap must fail with ErrPendingExists, got %v", err)
	}
	if got := len(c.Pending("m-6")); got != 1 {
		t.Fatalf("pending = %d, want only the first request", got)
	}
}

func TestRequestRejectsAfterClose(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-7", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	if err := c.CloseRoster("m-7"); err != nil {
		t.Fatalf("close roster: %v", err)
	}
	if _, err := c.Request("m-7", "p1", "p2"); !errors.Is(err, impl.ErrRosterClosed) {
		t.Fatalf("a closed roster must reject new swaps with ErrRosterClosed, got %v", err)
	}
	if picks, err := c.Roster("m-7"); err != nil || picks["p1"] != "hero-axe" {
		t.Fatalf("a closed roster must stay readable, got %v / %v", picks, err)
	}
}

func TestAcceptSwapsHeroesAndIncrementsSeq(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-8", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	req, err := c.Request("m-8", "p1", "p2")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := c.Accept("m-8", req.RequestID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.State != impl.SwapAccepted {
		t.Fatalf("result state = %q, want %q", res.State, impl.SwapAccepted)
	}
	if res.Seq != 1 {
		t.Fatalf("result sequence = %d, want the first committed exchange to be 1", res.Seq)
	}
	if len(res.Pairs) != 1 {
		t.Fatalf("result pairs = %+v, want one exchange", res.Pairs)
	}
	pair := res.Pairs[0]
	if pair.PlayerA != "p1" || pair.PlayerB != "p2" {
		t.Fatalf("pair players = %q/%q, want p1/p2", pair.PlayerA, pair.PlayerB)
	}
	if pair.HeroA != "hero-axe" || pair.HeroB != "hero-mage" {
		t.Fatalf("pair heroes = %q/%q, want the heroes held before the swap", pair.HeroA, pair.HeroB)
	}
	picks, err := c.Roster("m-8")
	if err != nil {
		t.Fatalf("roster after accept: %v", err)
	}
	if picks["p1"] != "hero-mage" || picks["p2"] != "hero-axe" {
		t.Fatalf("roster after the swap = %v, want p1 on the mage and p2 on the axe", picks)
	}
	if got := len(c.Pending("m-8")); got != 0 {
		t.Fatalf("an accepted request must leave the pending list, got %d", got)
	}
}

func TestAcceptRejectsUnknownRequest(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-9", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	if _, err := c.Accept("m-9", "req-404"); !errors.Is(err, impl.ErrUnknownRequest) {
		t.Fatalf("accepting an unknown request must fail with ErrUnknownRequest, got %v", err)
	}
	if _, err := c.Accept("m-404", "req-404"); !errors.Is(err, impl.ErrUnknownRequest) {
		t.Fatalf("accepting on an unknown match must fail with ErrUnknownRequest, got %v", err)
	}
}

func TestCancelFreesPendingSlot(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-10", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	req, err := c.Request("m-10", "p1", "p2")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := len(c.Pending("m-10")); got != 1 {
		t.Fatalf("pending before cancel = %d, want 1", got)
	}
	if err := c.Cancel("m-10", req.RequestID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := len(c.Pending("m-10")); got != 0 {
		t.Fatalf("pending after cancel = %d, want 0", got)
	}
	if err := c.Cancel("m-10", req.RequestID); !errors.Is(err, impl.ErrRequestNotPending) {
		t.Fatalf("cancelling a withdrawn request must fail with ErrRequestNotPending, got %v", err)
	}
	again, err := c.Request("m-10", "p1", "p3")
	if err != nil {
		t.Fatalf("the freed slot must accept a new request, got %v", err)
	}
	if again.RequestID == req.RequestID {
		t.Fatalf("a new request must get a new id")
	}
	if err := c.Cancel("m-10", "req-404"); !errors.Is(err, impl.ErrUnknownRequest) {
		t.Fatalf("cancelling an unknown request must fail with ErrUnknownRequest, got %v", err)
	}
}

func TestDeclineKeepsHeroes(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-11", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	req, err := c.Request("m-11", "p2", "p3")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := c.Decline("m-11", req.RequestID); err != nil {
		t.Fatalf("decline: %v", err)
	}
	picks, err := c.Roster("m-11")
	if err != nil {
		t.Fatalf("roster after decline: %v", err)
	}
	if picks["p2"] != "hero-mage" || picks["p3"] != "hero-healer" {
		t.Fatalf("roster after the decline = %v, want both heroes untouched", picks)
	}
	if got := len(c.Pending("m-11")); got != 0 {
		t.Fatalf("a declined request must leave the pending list, got %d", got)
	}
	if err := c.Decline("m-11", "req-404"); !errors.Is(err, impl.ErrUnknownRequest) {
		t.Fatalf("declining an unknown request must fail with ErrUnknownRequest, got %v", err)
	}
}

func TestExpireReleasesPendingRequests(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if err := c.LockRoster("m-12", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	if _, err := c.Request("m-12", "p1", "p2"); err != nil {
		t.Fatalf("request: %v", err)
	}
	n, err := c.Expire(time.Now().Add(11 * time.Minute))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expire folded %d requests, want 1", n)
	}
	if got := len(c.Pending("m-12")); got != 0 {
		t.Fatalf("pending after expire = %d, want 0", got)
	}
	n, err = c.Expire(time.Now().Add(12 * time.Minute))
	if err != nil {
		t.Fatalf("second expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("a second expire folded %d requests, want 0", n)
	}
	if _, err := c.Request("m-12", "p1", "p2"); err != nil {
		t.Fatalf("the freed slot must accept a new request, got %v", err)
	}
}

func TestRosterRejectsUnknownMatch(t *testing.T) {
	c := impl.NewHeroSwapCoordinator(swapLimits())
	if _, err := c.Roster("m-404"); !errors.Is(err, impl.ErrUnknownMatch) {
		t.Fatalf("roster of an unknown match must fail with ErrUnknownMatch, got %v", err)
	}
	if got := len(c.Pending("m-404")); got != 0 {
		t.Fatalf("pending of an unknown match = %d, want 0", got)
	}
	if err := c.CloseRoster("m-404"); !errors.Is(err, impl.ErrUnknownMatch) {
		t.Fatalf("closing an unknown match must fail with ErrUnknownMatch, got %v", err)
	}
}

func TestSnapshotRestoreKeepsRosterAndPending(t *testing.T) {
	limits := swapLimits()
	c := impl.NewHeroSwapCoordinator(limits)
	if err := c.LockRoster("m-13", rosterPicks()); err != nil {
		t.Fatalf("lock roster: %v", err)
	}
	req, err := c.Request("m-13", "p1", "p3")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewHeroSwapCoordinator(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	pending := restored.Pending("m-13")
	if len(pending) != 1 || pending[0].RequestID != req.RequestID {
		t.Fatalf("restored pending = %+v, want the request %s", pending, req.RequestID)
	}
	res, err := restored.Accept("m-13", req.RequestID)
	if err != nil {
		t.Fatalf("accept after restore: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].HeroA != "hero-axe" || res.Pairs[0].HeroB != "hero-healer" {
		t.Fatalf("restored exchange = %+v, want the axe and the healer", res.Pairs)
	}
	picks, err := restored.Roster("m-13")
	if err != nil {
		t.Fatalf("roster after restore and accept: %v", err)
	}
	if picks["p1"] != "hero-healer" || picks["p3"] != "hero-axe" {
		t.Fatalf("roster after the restored exchange = %v, want the heroes exchanged", picks)
	}
	if err := restored.Restore([]byte("not-a-roster-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if picks, _ := restored.Roster("m-13"); picks["p1"] != "hero-healer" {
		t.Fatalf("a rejected restore must not touch the live roster, got %v", picks)
	}
}
