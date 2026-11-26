package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-15"
)

func guardianConfig(now func() time.Time) impl.Config {
	return impl.Config{
		AdultAge:            18,
		MaxWardsPerGuardian: 2,
		MaxLinksPerWard:     2,
		MaxProofAttempts:    3,
		ReviewInterval:      30 * 24 * time.Hour,
		ReviewGrace:         7 * 24 * time.Hour,
		MaxScopeActions:     3,
		Now:                 now,
	}
}

func guardianScope(now time.Time) impl.Scope {
	return impl.Scope{
		Actions:   []string{"view_progress", "manage_settings", "view_progress"},
		ExpiresAt: now.Add(365 * 24 * time.Hour),
	}
}

func enrollAccounts(t *testing.T, g *impl.GuardianConsentLink, accounts ...string) {
	t.Helper()
	for _, account := range accounts {
		if err := g.Enroll(account, time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("enroll %s: %v", account, err)
		}
	}
}

func invite(t *testing.T, g *impl.GuardianConsentLink, guardian string, ward string, now time.Time) impl.Link {
	t.Helper()
	link, err := g.Invite(guardian, ward, guardianScope(now))
	if err != nil {
		t.Fatalf("invite %s -> %s: %v", guardian, ward, err)
	}
	return link
}

func acceptLink(t *testing.T, g *impl.GuardianConsentLink, link impl.Link) impl.Link {
	t.Helper()
	accepted, err := g.Accept(link.ID, impl.Proof{Code: link.AcceptCode})
	if err != nil {
		t.Fatalf("accept %s: %v", link.ID, err)
	}
	return accepted
}

func TestEnrollRegistersAccount(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	if _, err := g.Invite("guardian-1", "ward-1", guardianScope(now)); !errors.Is(err, impl.ErrAccountUnknown) {
		t.Fatalf("invite before enroll must return ErrAccountUnknown, got %v", err)
	}
	enrollAccounts(t, g, "guardian-1", "ward-1")
	if err := g.Enroll("guardian-1", time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, impl.ErrAccountExists) {
		t.Fatalf("duplicate enroll must return ErrAccountExists, got %v", err)
	}
	link := invite(t, g, "guardian-1", "ward-1", now)
	if link.GuardianID != "guardian-1" || link.WardID != "ward-1" {
		t.Fatalf("link identity = %+v, want guardian-1 and ward-1", link)
	}
}

func TestEnrollRejectsBlankIdentity(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	if err := g.Enroll("   ", time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, impl.ErrEmptyIdentity) {
		t.Fatalf("blank account must return ErrEmptyIdentity, got %v", err)
	}
	if err := g.Enroll("ward-1", now.Add(24*time.Hour)); !errors.Is(err, impl.ErrBadBirthDate) {
		t.Fatalf("future birth date must return ErrBadBirthDate, got %v", err)
	}
	if _, err := g.Invite("guardian-1", "ward-1", guardianScope(now)); !errors.Is(err, impl.ErrAccountUnknown) {
		t.Fatalf("rejected enroll must leave the account unknown, got %v", err)
	}
}

func TestInviteRejectsSelfLink(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1")
	if _, err := g.Invite("guardian-1", "guardian-1", guardianScope(now)); !errors.Is(err, impl.ErrSelfLink) {
		t.Fatalf("self link must return ErrSelfLink, got %v", err)
	}
}

func TestInviteCreatesInvitedLink(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	if link.ID != "link-1" {
		t.Fatalf("link id = %q, want link-1", link.ID)
	}
	if link.State != impl.LinkInvited {
		t.Fatalf("state = %q, want invited", link.State)
	}
	if link.AcceptCode != "code-link-1" {
		t.Fatalf("accept code = %q, want code-link-1", link.AcceptCode)
	}
	if len(link.Actions) != 2 || link.Actions[0] != "manage_settings" || link.Actions[1] != "view_progress" {
		t.Fatalf("actions = %v, want deduplicated and sorted", link.Actions)
	}
	if !link.ScopeUntil.Equal(now.Add(365 * 24 * time.Hour)) {
		t.Fatalf("scope until = %v, want one year out", link.ScopeUntil)
	}
	if link.Seq != 1 || link.Attempts != 0 {
		t.Fatalf("seq/attempts = %d/%d, want 1/0", link.Seq, link.Attempts)
	}
	if got := len(g.Due(now)); got != 0 {
		t.Fatalf("a fresh invite must not be due, got %d", got)
	}
}

func TestInviteEnforcesWardCap(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1", "ward-2", "ward-3")
	invite(t, g, "guardian-1", "ward-1", now)
	invite(t, g, "guardian-1", "ward-2", now)
	if _, err := g.Invite("guardian-1", "ward-3", guardianScope(now)); !errors.Is(err, impl.ErrLinkLimit) {
		t.Fatalf("third ward must return ErrLinkLimit, got %v", err)
	}
	if got := g.State("link-3"); got != "" {
		t.Fatalf("rejected invite must not create a link, got %q", got)
	}
}

func TestInviteRejectsBadScope(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	if _, err := g.Invite("guardian-1", "ward-1", impl.Scope{Actions: nil, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, impl.ErrBadScope) {
		t.Fatalf("empty scope must return ErrBadScope, got %v", err)
	}
	if _, err := g.Invite("guardian-1", "ward-1", impl.Scope{Actions: []string{"view_progress"}, ExpiresAt: now.Add(-time.Minute)}); !errors.Is(err, impl.ErrBadScope) {
		t.Fatalf("expired scope must return ErrBadScope, got %v", err)
	}
	wide := impl.Scope{
		Actions:   []string{"a", "b", "c", "d"},
		ExpiresAt: now.Add(time.Hour),
	}
	if _, err := g.Invite("guardian-1", "ward-1", wide); !errors.Is(err, impl.ErrBadScope) {
		t.Fatalf("four actions must return ErrBadScope, got %v", err)
	}
}

func TestAcceptActivatesLink(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	accepted := acceptLink(t, g, link)
	if accepted.State != impl.LinkActive {
		t.Fatalf("state = %q, want active", accepted.State)
	}
	if !accepted.AcceptedAt.Equal(now) {
		t.Fatalf("accepted at = %v, want %v", accepted.AcceptedAt, now)
	}
	if !accepted.ReviewAt.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("review at = %v, want the 30 day interval", accepted.ReviewAt)
	}
	if got := g.State(link.ID); got != impl.LinkActive {
		t.Fatalf("state query = %q, want active", got)
	}
}

func TestAcceptRejectsWrongCode(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	if _, err := g.Accept(link.ID, impl.Proof{Code: "code-link-9"}); !errors.Is(err, impl.ErrProofRejected) {
		t.Fatalf("wrong code must return ErrProofRejected, got %v", err)
	}
	if got := g.State(link.ID); got != impl.LinkInvited {
		t.Fatalf("state after a rejected code = %q, want invited", got)
	}
	if got := g.Attempts(link.ID); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	accepted := acceptLink(t, g, link)
	if accepted.State != impl.LinkActive {
		t.Fatalf("correct code after one failure must activate, got %q", accepted.State)
	}
}

func TestAcceptInvalidatesAfterCap(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	cfg := guardianConfig(func() time.Time { return now })
	cfg.MaxProofAttempts = 2
	g := impl.NewGuardianConsentLink(cfg)
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	for i := 0; i < 2; i++ {
		if _, err := g.Accept(link.ID, impl.Proof{Code: "wrong"}); !errors.Is(err, impl.ErrProofRejected) {
			t.Fatalf("attempt %d must return ErrProofRejected, got %v", i, err)
		}
	}
	if got := g.State(link.ID); got != impl.LinkInvalid {
		t.Fatalf("state after the cap = %q, want invalid", got)
	}
	if got := g.Attempts(link.ID); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if _, err := g.Accept(link.ID, impl.Proof{Code: link.AcceptCode}); !errors.Is(err, impl.ErrLinkInvalid) {
		t.Fatalf("the correct code must no longer work, got %v", err)
	}
}

func TestAcceptTwiceClosed(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	acceptLink(t, g, link)
	if _, err := g.Accept(link.ID, impl.Proof{Code: link.AcceptCode}); !errors.Is(err, impl.ErrLinkClosed) {
		t.Fatalf("second accept must return ErrLinkClosed, got %v", err)
	}
}

func TestAllowedChecksActions(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	acceptLink(t, g, link)
	ok, err := g.Allowed(link.ID, "view_progress")
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if !ok {
		t.Fatalf("view_progress must be inside the scope")
	}
	ok, err = g.Allowed(link.ID, "purchase_approval")
	if err != nil {
		t.Fatalf("allowed outside scope: %v", err)
	}
	if ok {
		t.Fatalf("purchase_approval must stay outside the scope")
	}
	if _, err := g.Allowed("link-404", "view_progress"); !errors.Is(err, impl.ErrLinkNotFound) {
		t.Fatalf("unknown link must return ErrLinkNotFound, got %v", err)
	}
}

func TestRevokeRequiresReason(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	acceptLink(t, g, link)
	if err := g.Revoke(link.ID, impl.ActorGuardian, "   "); !errors.Is(err, impl.ErrBadReason) {
		t.Fatalf("blank reason must return ErrBadReason, got %v", err)
	}
	if got := g.State(link.ID); got != impl.LinkActive {
		t.Fatalf("rejected revoke must keep the link active, got %q", got)
	}
	if err := g.Revoke(link.ID, "browser", "x"); !errors.Is(err, impl.ErrBadActor) {
		t.Fatalf("unknown actor must return ErrBadActor, got %v", err)
	}
	if err := g.Revoke(link.ID, impl.ActorGuardian, "consent_withdrawn"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := g.State(link.ID); got != impl.LinkRevoked {
		t.Fatalf("state = %q, want revoked", got)
	}
	ok, err := g.Allowed(link.ID, "view_progress")
	if err != nil {
		t.Fatalf("allowed after revoke: %v", err)
	}
	if ok {
		t.Fatalf("a revoked link must not allow any action")
	}
	if err := g.Revoke(link.ID, impl.ActorSystem, "again"); !errors.Is(err, impl.ErrLinkClosed) {
		t.Fatalf("second revoke must return ErrLinkClosed, got %v", err)
	}
}

func TestReviewDowngradesToReadonly(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	acceptLink(t, g, link)
	if err := g.Review(link.ID, impl.Decision{Approve: false, Note: "scope too wide"}); err != nil {
		t.Fatalf("review reject: %v", err)
	}
	if got := g.State(link.ID); got != impl.LinkReadonly {
		t.Fatalf("state = %q, want readonly", got)
	}
	ok, err := g.Allowed(link.ID, "view_progress")
	if err != nil {
		t.Fatalf("allowed while readonly: %v", err)
	}
	if ok {
		t.Fatalf("readonly links must not allow actions")
	}
	if err := g.Review(link.ID, impl.Decision{Approve: true}); err != nil {
		t.Fatalf("review approve: %v", err)
	}
	if got := g.State(link.ID); got != impl.LinkActive {
		t.Fatalf("state = %q, want active after approval", got)
	}
	ok, err = g.Allowed(link.ID, "view_progress")
	if err != nil || !ok {
		t.Fatalf("approved link must allow in-scope actions, ok=%v err=%v", ok, err)
	}
}

func TestDueListsOverdueThenSweepDowngrades(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	g := impl.NewGuardianConsentLink(guardianConfig(func() time.Time { return now }))
	enrollAccounts(t, g, "guardian-1", "ward-1")
	link := invite(t, g, "guardian-1", "ward-1", now)
	acceptLink(t, g, link)
	if got := len(g.Due(now)); got != 0 {
		t.Fatalf("fresh link must not be due, got %d", got)
	}
	overdue := now.Add(30*24*time.Hour + 8*24*time.Hour)
	due := g.Due(overdue)
	if len(due) != 1 || due[0].ID != link.ID {
		t.Fatalf("due list = %+v, want exactly the accepted link", due)
	}
	if got := g.Sweep(overdue); got != 1 {
		t.Fatalf("sweep = %d, want 1", got)
	}
	if got := g.State(link.ID); got != impl.LinkReadonly {
		t.Fatalf("state after sweep = %q, want readonly", got)
	}
	ok, err := g.Allowed(link.ID, "view_progress")
	if err != nil || ok {
		t.Fatalf("overdue link must not allow actions, ok=%v err=%v", ok, err)
	}
	if got := g.Sweep(overdue); got != 0 {
		t.Fatalf("second sweep = %d, want 0", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	g := impl.NewGuardianConsentLink(guardianConfig(clock))
	enrollAccounts(t, g, "guardian-1", "ward-1", "ward-2")
	active := invite(t, g, "guardian-1", "ward-1", now)
	acceptLink(t, g, active)
	closed := invite(t, g, "guardian-1", "ward-2", now)
	if err := g.Revoke(closed.ID, impl.ActorWard, "no_longer_needed"); err != nil {
		t.Fatalf("revoke second link: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewGuardianConsentLink(guardianConfig(clock))
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.State(active.ID); got != impl.LinkActive {
		t.Fatalf("restored active link state = %q, want active", got)
	}
	if got := restored.State(closed.ID); got != impl.LinkRevoked {
		t.Fatalf("restored revoked link state = %q, want revoked", got)
	}
	if got := len(restored.Due(now.Add(31 * 24 * time.Hour))); got != 1 {
		t.Fatalf("restored review deadline must be kept, due count = %d", got)
	}
	ok, err := restored.Allowed(active.ID, "view_progress")
	if err != nil || !ok {
		t.Fatalf("restored link must allow in-scope actions, ok=%v err=%v", ok, err)
	}
	next := invite(t, restored, "guardian-1", "ward-2", now)
	if next.ID != "link-3" {
		t.Fatalf("link id after restore = %q, want link-3", next.ID)
	}
	broken := impl.NewGuardianConsentLink(guardianConfig(clock))
	if err := broken.Restore([]byte("{not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if got := broken.State("link-1"); got != "" {
		t.Fatalf("failed restore must not apply partial state, got %q", got)
	}
}
