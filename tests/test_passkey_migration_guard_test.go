package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-a-14"
)

func guardConfig() impl.GuardConfig {
	return impl.GuardConfig{MaxDevices: 2, ChallengeTTL: 300, MaxEvents: 8}
}

func firstCredential() impl.Credential {
	return impl.Credential{ID: "cred-1", UserID: "user-1", PubKey: "pk-device-1", DeviceID: "device-1"}
}

func registerCredential(t *testing.T, g *impl.PasskeyMigrationGuard, at int64) {
	t.Helper()
	if err := g.RegisterCredential(firstCredential(), at); err != nil {
		t.Fatalf("register credential: %v", err)
	}
}

func migrateToSecondDevice(t *testing.T, g *impl.PasskeyMigrationGuard, at int64) impl.MigrationResult {
	t.Helper()
	ch, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-1", DeviceID: "device-2", PubKey: "pk-device-2", At: at})
	if err != nil {
		t.Fatalf("begin migration: %v", err)
	}
	res, err := g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: ch.Nonce, Signature: "sig-ok", At: at + 10})
	if err != nil {
		t.Fatalf("approve migration: %v", err)
	}
	return res
}

func TestRegisterCredentialBindsFirstDevice(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	state, ok := g.Credential("cred-1")
	if !ok {
		t.Fatalf("registered credential must be readable")
	}
	if state.State != impl.CredentialActive {
		t.Fatalf("state = %q, want %q", state.State, impl.CredentialActive)
	}
	if state.Counter != 0 {
		t.Fatalf("fresh counter = %d, want 0", state.Counter)
	}
	if state.Devices != 1 {
		t.Fatalf("device count = %d, want 1", state.Devices)
	}
	devices := g.Devices("cred-1")
	if len(devices) != 1 || devices[0].DeviceID != "device-1" {
		t.Fatalf("devices = %+v, want exactly device-1", devices)
	}
	if devices[0].State != impl.DeviceActive || devices[0].PubKey != "pk-device-1" {
		t.Fatalf("device row = %+v, want an active binding with pk-device-1", devices[0])
	}
	events := g.Events("cred-1")
	if len(events) != 1 || events[0].Kind != impl.EventRegistered {
		t.Fatalf("events = %+v, want one registration event", events)
	}
}

func TestRegisterRejectsBlankIdentity(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	cases := []impl.Credential{
		{UserID: "user-1", PubKey: "pk", DeviceID: "device-1"},
		{ID: "cred-1", PubKey: "pk", DeviceID: "device-1"},
		{ID: "cred-1", UserID: "user-1", DeviceID: "device-1"},
		{ID: "cred-1", UserID: "user-1", PubKey: "pk"},
	}
	for _, c := range cases {
		if err := g.RegisterCredential(c, 1000); !errors.Is(err, impl.ErrBadRequest) {
			t.Fatalf("incomplete credential %+v must fail with ErrBadRequest, got %v", c, err)
		}
	}
	if _, ok := g.Credential("cred-1"); ok {
		t.Fatalf("rejected registrations must not create a credential")
	}
}

func TestRegisterRejectsConflictingPubKey(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	conflict := impl.Credential{ID: "cred-1", UserID: "user-1", PubKey: "pk-other", DeviceID: "device-9"}
	if err := g.RegisterCredential(conflict, 1100); !errors.Is(err, impl.ErrCredentialConflict) {
		t.Fatalf("same credential with another key must fail with ErrCredentialConflict, got %v", err)
	}
	devices := g.Devices("cred-1")
	if len(devices) != 1 || devices[0].DeviceID != "device-1" {
		t.Fatalf("devices = %+v, want the original binding untouched", devices)
	}
	state, _ := g.Credential("cred-1")
	if state.State != impl.CredentialActive {
		t.Fatalf("state after a conflict = %q, want %q", state.State, impl.CredentialActive)
	}
}

func TestBeginMigrationRejectsFrozenCredential(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	if err := g.FreezeCredential("cred-1", "counter rollback seen"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	state, ok := g.Credential("cred-1")
	if !ok || state.State != impl.CredentialFrozen {
		t.Fatalf("state = %q ok=%v, want frozen", state.State, ok)
	}
	if state.Reason != "counter rollback seen" {
		t.Fatalf("frozen reason = %q, want the operator note", state.Reason)
	}
	_, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-1", DeviceID: "device-2", PubKey: "pk-device-2", At: 1200})
	if !errors.Is(err, impl.ErrCredentialFrozen) {
		t.Fatalf("migration of a frozen credential must fail with ErrCredentialFrozen, got %v", err)
	}
}

func TestUnknownCredentialRejected(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	if _, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-404", DeviceID: "device-2", PubKey: "pk", At: 1000}); !errors.Is(err, impl.ErrCredentialNotFound) {
		t.Fatalf("migration of an unknown credential must fail with ErrCredentialNotFound, got %v", err)
	}
	if _, err := g.Authenticate("cred-404", "device-1", 1, 1000); !errors.Is(err, impl.ErrCredentialNotFound) {
		t.Fatalf("assertion of an unknown credential must fail with ErrCredentialNotFound, got %v", err)
	}
}

func TestApproveMigrationBindsNewDeviceAndRetiresOld(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	res := migrateToSecondDevice(t, g, 2000)
	if !res.Bound {
		t.Fatalf("result = %+v, want a bound migration", res)
	}
	if res.RetiredDevice != "device-1" {
		t.Fatalf("retired device = %q, want device-1", res.RetiredDevice)
	}
	devices := g.Devices("cred-1")
	if len(devices) != 2 {
		t.Fatalf("devices = %d, want 2 after the migration", len(devices))
	}
	if devices[0].DeviceID != "device-1" || devices[0].State != impl.DeviceRetired {
		t.Fatalf("first device = %+v, want the retired original", devices[0])
	}
	if devices[1].DeviceID != "device-2" || devices[1].State != impl.DeviceActive {
		t.Fatalf("second device = %+v, want an active new binding", devices[1])
	}
	foundMigration := false
	for _, ev := range g.Events("cred-1") {
		if ev.Kind == impl.EventMigration && ev.DeviceID == "device-2" {
			foundMigration = true
		}
	}
	if !foundMigration {
		t.Fatalf("expected a migration event for device-2, got %+v", g.Events("cred-1"))
	}
	_, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-1", DeviceID: "device-3", PubKey: "pk-device-3", At: 2200})
	if !errors.Is(err, impl.ErrDeviceLimit) {
		t.Fatalf("migration beyond MaxDevices must fail with ErrDeviceLimit, got %v", err)
	}
}

func TestApproveRejectsExpiredChallenge(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	ch, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-1", DeviceID: "device-2", PubKey: "pk-device-2", At: 2000})
	if err != nil {
		t.Fatalf("begin migration: %v", err)
	}
	if ch.ExpiresAt != 2300 {
		t.Fatalf("challenge expiry = %d, want the issued time plus the ttl 2300", ch.ExpiresAt)
	}
	_, err = g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: ch.Nonce, Signature: "sig-ok", At: 2301})
	if !errors.Is(err, impl.ErrChallengeExpired) {
		t.Fatalf("approval after the ttl must fail with ErrChallengeExpired, got %v", err)
	}
	if got := len(g.Devices("cred-1")); got != 1 {
		t.Fatalf("devices after an expired challenge = %d, want 1", got)
	}
}

func TestApproveRejectsReusedChallenge(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	ch, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-1", DeviceID: "device-2", PubKey: "pk-device-2", At: 2000})
	if err != nil {
		t.Fatalf("begin migration: %v", err)
	}
	if _, err := g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: ch.Nonce, Signature: "sig-ok", At: 2100}); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	_, err = g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: ch.Nonce, Signature: "sig-ok", At: 2150})
	if !errors.Is(err, impl.ErrChallengeReused) {
		t.Fatalf("second approval must fail with ErrChallengeReused, got %v", err)
	}
	if got := len(g.Devices("cred-1")); got != 2 {
		t.Fatalf("devices after a replay = %d, want 2", got)
	}
}

func TestApproveRejectsProofNonceMismatch(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	ch, err := g.BeginMigration(impl.MigrationRequest{CredentialID: "cred-1", DeviceID: "device-2", PubKey: "pk-device-2", At: 2000})
	if err != nil {
		t.Fatalf("begin migration: %v", err)
	}
	if _, err := g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: "wrong-nonce", Signature: "sig-ok", At: 2100}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("wrong nonce must fail with ErrBadRequest, got %v", err)
	}
	if _, err := g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: ch.Nonce, Signature: "  ", At: 2100}); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("blank proof signature must fail with ErrBadRequest, got %v", err)
	}
	res, err := g.ApproveMigration(ch.ID, impl.MigrationProof{Nonce: ch.Nonce, Signature: "sig-ok", At: 2200})
	if err != nil {
		t.Fatalf("a rejected proof must not consume the challenge, got %v", err)
	}
	if !res.Bound {
		t.Fatalf("result = %+v, want a bound migration", res)
	}
}

func TestAuthenticateAdvancesWatermark(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	res, err := g.Authenticate("cred-1", "device-1", 5, 3000)
	if err != nil {
		t.Fatalf("authenticate with a fresh counter: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want ok", res)
	}
	if res.Counter != 5 || res.Watermark != 5 {
		t.Fatalf("counter/watermark = %d/%d, want 5/5", res.Counter, res.Watermark)
	}
	state, _ := g.Credential("cred-1")
	if state.Counter != 5 {
		t.Fatalf("credential counter = %d, want 5", state.Counter)
	}
	if got := g.Devices("cred-1")[0].Counter; got != 5 {
		t.Fatalf("device counter = %d, want 5", got)
	}
}

func TestAuthenticateRejectsCounterRollback(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	if _, err := g.Authenticate("cred-1", "device-1", 5, 3000); err != nil {
		t.Fatalf("first assertion: %v", err)
	}
	if _, err := g.Authenticate("cred-1", "device-1", 5, 3010); !errors.Is(err, impl.ErrCounterRollback) {
		t.Fatalf("a repeated counter must fail with ErrCounterRollback, got %v", err)
	}
	state, _ := g.Credential("cred-1")
	if state.State != impl.CredentialFrozen {
		t.Fatalf("state after a rollback = %q, want %q", state.State, impl.CredentialFrozen)
	}
	if _, err := g.Authenticate("cred-1", "device-1", 6, 3020); !errors.Is(err, impl.ErrCredentialFrozen) {
		t.Fatalf("assertion on a frozen credential must fail with ErrCredentialFrozen, got %v", err)
	}
}

func TestRetiredDeviceCannotAuthenticate(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	migrateToSecondDevice(t, g, 2000)
	if _, err := g.Authenticate("cred-1", "device-1", 9, 3000); !errors.Is(err, impl.ErrDeviceRetired) {
		t.Fatalf("assertion from a retired device must fail with ErrDeviceRetired, got %v", err)
	}
	state, _ := g.Credential("cred-1")
	if state.State != impl.CredentialActive {
		t.Fatalf("state = %q, want an active credential after the retired assertion", state.State)
	}
}

func TestUnfreezeRequiresReason(t *testing.T) {
	g := impl.NewPasskeyMigrationGuard(guardConfig())
	registerCredential(t, g, 1000)
	if err := g.FreezeCredential("cred-1", "manual hold"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if err := g.Unfreeze("cred-1", "   ", 3000); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("unfreeze without a reason must fail with ErrBadRequest, got %v", err)
	}
	state, _ := g.Credential("cred-1")
	if state.State != impl.CredentialFrozen {
		t.Fatalf("state after a rejected unfreeze = %q, want frozen", state.State)
	}
	if err := g.Unfreeze("cred-1", "helpdesk verified the owner", 3100); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	state, _ = g.Credential("cred-1")
	if state.State != impl.CredentialActive {
		t.Fatalf("state after unfreeze = %q, want active", state.State)
	}
	if state.Counter != 0 {
		t.Fatalf("unfreeze must not reset the counter, got %d", state.Counter)
	}
}

func TestEventsStayWithinCap(t *testing.T) {
	cfg := guardConfig()
	cfg.MaxEvents = 4
	g := impl.NewPasskeyMigrationGuard(cfg)
	registerCredential(t, g, 1000)
	for counter := uint32(1); counter <= 5; counter++ {
		if _, err := g.Authenticate("cred-1", "device-1", counter, 2000+int64(counter)); err != nil {
			t.Fatalf("authenticate %d: %v", counter, err)
		}
	}
	events := g.Events("cred-1")
	if len(events) != 4 {
		t.Fatalf("retained events = %d, want the configured cap 4", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("event sequence must be strictly increasing, got %d then %d", events[i-1].Seq, events[i].Seq)
		}
	}
	last := events[len(events)-1]
	if last.Kind != impl.EventAuthenticated || last.DeviceID != "device-1" {
		t.Fatalf("newest event = %+v, want the last assertion", last)
	}
}

func TestSnapshotRestoreKeepsWatermark(t *testing.T) {
	cfg := guardConfig()
	g := impl.NewPasskeyMigrationGuard(cfg)
	registerCredential(t, g, 1000)
	if _, err := g.Authenticate("cred-1", "device-1", 3, 2000); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewPasskeyMigrationGuard(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	state, ok := restored.Credential("cred-1")
	if !ok {
		t.Fatalf("restored guard lost the credential")
	}
	if state.Counter != 3 {
		t.Fatalf("restored counter = %d, want 3", state.Counter)
	}
	res, err := restored.Authenticate("cred-1", "device-1", 4, 2100)
	if err != nil {
		t.Fatalf("assertion after restore: %v", err)
	}
	if !res.OK || res.Watermark != 4 {
		t.Fatalf("result after restore = %+v, want ok with watermark 4", res)
	}
	if err := restored.Restore([]byte("{broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	state, _ = restored.Credential("cred-1")
	if state.Counter != 4 || state.State != impl.CredentialActive {
		t.Fatalf("failed restore must not touch live state, got %+v", state)
	}
}
