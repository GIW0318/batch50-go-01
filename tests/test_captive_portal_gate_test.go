package task

import (
	"errors"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-12"
)

var gateTime = time.Date(2026, time.April, 2, 9, 0, 0, 0, time.UTC)

func gateLimits() impl.Limits {
	return impl.Limits{LeaseTTL: 15 * time.Minute, MaxTTL: 30 * time.Minute, MaxRules: 8, MaxLeases: 16}
}

func gardenRules() []impl.Rule {
	return []impl.Rule{
		{Kind: impl.RuleSuffix, Value: "vendor.example"},
		{Kind: impl.RuleCIDR, Value: "203.0.113.0/24"},
	}
}

func newGate() *impl.CaptivePortalGate {
	return impl.NewCaptivePortalGate(gateLimits(), gardenRules())
}

func guest(id, fp string) impl.Client {
	return impl.Client{ID: id, Fingerprint: fp, Addr: "192.0.2.20"}
}

func resolve(host string, now time.Time) impl.Request {
	return impl.Request{Host: host, Phase: impl.PhaseResolve, Now: now}
}

func TestInterceptUnknownClientDivertsToPortal(t *testing.T) {
	g := newGate()
	verdict, err := g.Intercept(guest("guest-1", "fp-alpha"), resolve("news.example", gateTime))
	if err != nil {
		t.Fatalf("intercept must not fail, got %v", err)
	}
	if verdict.Action != impl.ActionPortal {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionPortal)
	}
	if verdict.Reason != impl.ReasonPortalIntercept {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonPortalIntercept)
	}
	if verdict.ClientID != "guest-1" {
		t.Fatalf("verdict client = %q, want guest-1", verdict.ClientID)
	}
	client, ok := g.ClientOf("guest-1")
	if !ok {
		t.Fatalf("intercepted client must be recorded")
	}
	if client.State != impl.ClientPending {
		t.Fatalf("state = %q, want %q", client.State, impl.ClientPending)
	}
	if client.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", client.Attempts)
	}
	if client.Fingerprint != "fp-alpha" {
		t.Fatalf("fingerprint = %q, want fp-alpha", client.Fingerprint)
	}
}

func TestInterceptWalledGardenSuffixMatch(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-2", "fp-beta"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	verdict, err := g.Intercept(guest("guest-2", "fp-beta"), resolve("api.vendor.example", gateTime))
	if err != nil {
		t.Fatalf("second intercept: %v", err)
	}
	if verdict.Action != impl.ActionAllow {
		t.Fatalf("action = %q, want %q for a walled garden host", verdict.Action, impl.ActionAllow)
	}
	if verdict.Reason != impl.ReasonWalledGarden {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonWalledGarden)
	}
	if client, _ := g.ClientOf("guest-2"); client.State != impl.ClientPending {
		t.Fatalf("walled garden access must not authorise the client, state = %q", client.State)
	}
}

func TestInterceptRejectsSuffixLookalike(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-3", "fp-gamma"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	verdict, err := g.Intercept(guest("guest-3", "fp-gamma"), resolve("notvendor.example", gateTime))
	if err != nil {
		t.Fatalf("second intercept: %v", err)
	}
	if verdict.Action != impl.ActionPortal {
		t.Fatalf("action = %q, want %q: a host that merely ends with the suffix must not pass", verdict.Action, impl.ActionPortal)
	}
}

func TestInterceptWalledGardenCIDRMatch(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-4", "fp-delta"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	req := impl.Request{Addr: "203.0.113.9", Phase: impl.PhaseConnect, Now: gateTime}
	verdict, err := g.Intercept(guest("guest-4", "fp-delta"), req)
	if err != nil {
		t.Fatalf("connect intercept: %v", err)
	}
	if verdict.Action != impl.ActionAllow || verdict.Reason != impl.ReasonWalledGarden {
		t.Fatalf("verdict = %+v, want an allow for an address inside the garden block", verdict)
	}
	req.Addr = "203.0.114.9"
	verdict, err = g.Intercept(guest("guest-4", "fp-delta"), req)
	if err != nil {
		t.Fatalf("outside intercept: %v", err)
	}
	if verdict.Action != impl.ActionPortal {
		t.Fatalf("action = %q for an address outside the block, want %q", verdict.Action, impl.ActionPortal)
	}
}

func TestInterceptAuthorizedAllowsWithLease(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-5", "fp-epsilon"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	lease, err := g.Authorize("guest-5", impl.Grant{Fingerprint: "fp-epsilon", TTL: 10 * time.Minute, Now: gateTime})
	if err != nil {
		t.Fatalf("authorize must succeed, got %v", err)
	}
	verdict, err := g.Intercept(guest("guest-5", "fp-epsilon"), resolve("news.example", gateTime.Add(time.Minute)))
	if err != nil {
		t.Fatalf("intercept after authorisation: %v", err)
	}
	if verdict.Action != impl.ActionAllow {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionAllow)
	}
	if verdict.Reason != impl.ReasonAuthorized {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonAuthorized)
	}
	if verdict.LeaseID != lease.ID {
		t.Fatalf("verdict lease = %q, want %q", verdict.LeaseID, lease.ID)
	}
}

func TestInterceptExpiredLeaseReturnsPortal(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-6", "fp-zeta"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	lease, err := g.Authorize("guest-6", impl.Grant{Fingerprint: "fp-zeta", TTL: 10 * time.Minute, Now: gateTime})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	verdict, err := g.Intercept(guest("guest-6", "fp-zeta"), resolve("news.example", gateTime.Add(time.Hour)))
	if err != nil {
		t.Fatalf("late intercept: %v", err)
	}
	if verdict.Action != impl.ActionPortal {
		t.Fatalf("action = %q, want %q once the lease is over", verdict.Action, impl.ActionPortal)
	}
	if verdict.Reason != impl.ReasonLeaseExpired {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonLeaseExpired)
	}
	stored, ok := g.LeaseOf(lease.ID)
	if !ok {
		t.Fatalf("lease must stay readable after expiry")
	}
	if stored.State != impl.LeaseExpired {
		t.Fatalf("lease state = %q, want %q", stored.State, impl.LeaseExpired)
	}
	if client, _ := g.ClientOf("guest-6"); client.State != impl.ClientExpired {
		t.Fatalf("client state = %q, want %q", client.State, impl.ClientExpired)
	}
}

func TestInterceptBlockedClientDenied(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-7", "fp-eta"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	lease, err := g.Authorize("guest-7", impl.Grant{Fingerprint: "fp-eta", TTL: 10 * time.Minute, Now: gateTime})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := g.Revoke(lease.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	verdict, err := g.Intercept(guest("guest-7", "fp-eta"), resolve("news.example", gateTime.Add(time.Minute)))
	if err != nil {
		t.Fatalf("intercept after revoke: %v", err)
	}
	if verdict.Action != impl.ActionDeny {
		t.Fatalf("action = %q, want %q", verdict.Action, impl.ActionDeny)
	}
	if verdict.Reason != impl.ReasonBlocked {
		t.Fatalf("reason = %q, want %q", verdict.Reason, impl.ReasonBlocked)
	}
}

func TestAuthorizeUnknownClientRejected(t *testing.T) {
	g := newGate()
	if _, err := g.Authorize("guest-missing", impl.Grant{Fingerprint: "fp-theta", TTL: time.Minute, Now: gateTime}); !errors.Is(err, impl.ErrUnknownClient) {
		t.Fatalf("authorising an unseen client must yield ErrUnknownClient, got %v", err)
	}
	if got := len(g.Leases()); got != 0 {
		t.Fatalf("rejected authorisation must not create a lease, got %d", got)
	}
}

func TestAuthorizeFingerprintMismatchRejected(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-8", "fp-iota"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	if _, err := g.Authorize("guest-8", impl.Grant{Fingerprint: "fp-someone-else", TTL: time.Minute, Now: gateTime}); !errors.Is(err, impl.ErrDeviceMismatch) {
		t.Fatalf("a foreign fingerprint must yield ErrDeviceMismatch, got %v", err)
	}
	if got := len(g.Leases()); got != 0 {
		t.Fatalf("mismatched authorisation must not create a lease, got %d", got)
	}
	if client, _ := g.ClientOf("guest-8"); client.State != impl.ClientPending {
		t.Fatalf("client state = %q after a rejected authorisation, want %q", client.State, impl.ClientPending)
	}
}

func TestAuthorizeClampsTTL(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-9", "fp-kappa"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	lease, err := g.Authorize("guest-9", impl.Grant{Fingerprint: "fp-kappa", TTL: 2 * time.Hour, Now: gateTime})
	if err != nil {
		t.Fatalf("authorize must succeed, got %v", err)
	}
	want := gateTime.Add(30 * time.Minute)
	if !lease.Expires.Equal(want) {
		t.Fatalf("expires = %v, want the clamped %v", lease.Expires, want)
	}
	if lease.State != impl.LeaseActive {
		t.Fatalf("lease state = %q, want %q", lease.State, impl.LeaseActive)
	}
}

func TestRevokeUnknownLeaseRejected(t *testing.T) {
	g := newGate()
	if err := g.Revoke("lease-missing"); !errors.Is(err, impl.ErrLeaseNotFound) {
		t.Fatalf("unknown lease must yield ErrLeaseNotFound, got %v", err)
	}
}

func TestRevokeTwiceRejected(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-10", "fp-lambda"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("first intercept: %v", err)
	}
	lease, err := g.Authorize("guest-10", impl.Grant{Fingerprint: "fp-lambda", TTL: 10 * time.Minute, Now: gateTime})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := g.Revoke(lease.ID); err != nil {
		t.Fatalf("first revoke must succeed, got %v", err)
	}
	if err := g.Revoke(lease.ID); !errors.Is(err, impl.ErrLeaseNotActive) {
		t.Fatalf("second revoke must yield ErrLeaseNotActive, got %v", err)
	}
	stored, ok := g.LeaseOf(lease.ID)
	if !ok {
		t.Fatalf("revoked lease must stay readable")
	}
	if stored.State != impl.LeaseRevoked {
		t.Fatalf("lease state = %q, want %q", stored.State, impl.LeaseRevoked)
	}
}

func TestExpireCountsOnce(t *testing.T) {
	g := newGate()
	for _, id := range []string{"guest-11", "guest-12"} {
		if _, err := g.Intercept(guest(id, "fp-"+id), resolve("news.example", gateTime)); err != nil {
			t.Fatalf("intercept %s: %v", id, err)
		}
		if _, err := g.Authorize(id, impl.Grant{Fingerprint: "fp-" + id, TTL: 10 * time.Minute, Now: gateTime}); err != nil {
			t.Fatalf("authorize %s: %v", id, err)
		}
	}
	if got := g.Expire(gateTime.Add(time.Hour)); got != 2 {
		t.Fatalf("expired leases = %d, want 2", got)
	}
	if got := g.Expire(gateTime.Add(time.Hour)); got != 0 {
		t.Fatalf("second sweep = %d, want 0: expiry must be reported once", got)
	}
	for _, lease := range g.Leases() {
		if lease.State != impl.LeaseExpired {
			t.Fatalf("lease %s state = %q, want %q", lease.ID, lease.State, impl.LeaseExpired)
		}
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	g := newGate()
	if _, err := g.Intercept(guest("guest-13", "fp-mu"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("pending intercept: %v", err)
	}
	if _, err := g.Intercept(guest("guest-14", "fp-nu"), resolve("news.example", gateTime)); err != nil {
		t.Fatalf("second intercept: %v", err)
	}
	lease, err := g.Authorize("guest-14", impl.Grant{Fingerprint: "fp-nu", TTL: 20 * time.Minute, Now: gateTime})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	fresh := impl.NewCaptivePortalGate(gateLimits(), nil)
	if err := fresh.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	if got, ok := fresh.ClientOf("guest-13"); !ok || got.State != impl.ClientPending {
		t.Fatalf("pending client not restored: %+v ok=%v", got, ok)
	}
	if got, ok := fresh.ClientOf("guest-14"); !ok || got.State != impl.ClientAuthorized || got.LeaseID != lease.ID {
		t.Fatalf("authorised client not restored: %+v ok=%v", got, ok)
	}
	if got, ok := fresh.LeaseOf(lease.ID); !ok || !got.Expires.Equal(lease.Expires) {
		t.Fatalf("lease not restored: %+v ok=%v", got, ok)
	}
	if rules := fresh.WalledGarden(); len(rules) != 2 {
		t.Fatalf("restored rules = %d, want 2", len(rules))
	}
	if err := fresh.Restore([]byte("not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must yield ErrBadSnapshot, got %v", err)
	}
	if _, ok := fresh.ClientOf("guest-14"); !ok {
		t.Fatalf("failed restore must not wipe the live state")
	}
}

func TestConcurrentInterceptCountsAttempts(t *testing.T) {
	g := newGate()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Intercept(guest("guest-15", "fp-xi"), resolve("news.example", gateTime)); err != nil {
				t.Errorf("concurrent intercept: %v", err)
			}
		}()
	}
	wg.Wait()
	client, ok := g.ClientOf("guest-15")
	if !ok {
		t.Fatalf("client must exist after concurrent intercepts")
	}
	if client.Attempts != 6 {
		t.Fatalf("attempts = %d, want 6: concurrent intercepts must not lose counts", client.Attempts)
	}
	if client.State != impl.ClientPending {
		t.Fatalf("state = %q, want %q", client.State, impl.ClientPending)
	}
}
