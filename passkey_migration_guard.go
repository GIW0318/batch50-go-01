// Package task guards passkey migration between devices: it binds a device
// public key behind a one shot challenge, advances the credential signature
// counter and freezes a credential when the counter walks backwards.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Credential states.
const (
	CredentialActive = "active"
	CredentialFrozen = "frozen"
)

// Device binding states.
const (
	DeviceActive  = "active"
	DeviceRetired = "retired"
)

// Event kinds appended to the per credential log.
const (
	EventRegistered     = "credential_registered"
	EventMigration      = "migration_approved"
	EventAuthenticated  = "authenticated"
	EventFrozen         = "credential_frozen"
	EventUnfrozen       = "credential_unfrozen"
	EventChallengeGone  = "challenge_expired"
	EventDeviceRetired  = "device_retired"
	EventMigrationBegin = "migration_started"
)

// Errors returned by the guard.
var (
	ErrBadRequest         = errors.New("credential, device or proof fields are invalid")
	ErrCredentialNotFound = errors.New("credential not found")
	ErrCredentialConflict = errors.New("credential already registered with another public key")
	ErrCredentialFrozen   = errors.New("credential is frozen")
	ErrChallengeExpired   = errors.New("migration challenge has expired")
	ErrChallengeReused    = errors.New("migration challenge has already been consumed")
	ErrDeviceRetired      = errors.New("device binding is retired")
	ErrCounterRollback    = errors.New("signature counter did not move forward")
	ErrDeviceLimit        = errors.New("device limit reached for this credential")
	ErrBadSnapshot        = errors.New("snapshot payload is invalid")
)

// Credential is the record registered for one passkey.
type Credential struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	PubKey   string `json:"pub_key"`
	DeviceID string `json:"device_id"`
}

// CredentialState is the projected state of one credential.
type CredentialState struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Counter  uint32 `json:"counter"`
	Devices  int    `json:"devices"`
	FrozenAt int64  `json:"frozen_at"`
}

// DeviceBinding is one device that holds the credential public key.
type DeviceBinding struct {
	DeviceID string `json:"device_id"`
	PubKey   string `json:"pub_key"`
	State    string `json:"state"`
	Counter  uint32 `json:"counter"`
	BoundAt  int64  `json:"bound_at"`
	Seq      int64  `json:"seq"`
}

// MigrationRequest starts a cross device migration.
type MigrationRequest struct {
	CredentialID string `json:"credential_id"`
	DeviceID     string `json:"device_id"`
	PubKey       string `json:"pub_key"`
	At           int64  `json:"at"`
}

// Challenge is the one shot nonce handed to the target device.
type Challenge struct {
	ID           string `json:"id"`
	CredentialID string `json:"credential_id"`
	DeviceID     string `json:"device_id"`
	PubKey       string `json:"pub_key"`
	Nonce        string `json:"nonce"`
	ExpiresAt    int64  `json:"expires_at"`
	Seq          int64  `json:"seq"`
}

// MigrationProof is the answer of the target device to a challenge.
type MigrationProof struct {
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
	At        int64  `json:"at"`
}

// MigrationResult is the outcome of an approved migration.
type MigrationResult struct {
	CredentialID  string `json:"credential_id"`
	DeviceID      string `json:"device_id"`
	RetiredDevice string `json:"retired_device"`
	Bound         bool   `json:"bound"`
	Seq           int64  `json:"seq"`
}

// AuthResult is the outcome of one assertion.
type AuthResult struct {
	CredentialID string `json:"credential_id"`
	DeviceID     string `json:"device_id"`
	Counter      uint32 `json:"counter"`
	Watermark    uint32 `json:"watermark"`
	OK           bool   `json:"ok"`
	Seq          int64  `json:"seq"`
}

// Event is one line of the per credential audit log.
type Event struct {
	CredentialID string `json:"credential_id"`
	Kind         string `json:"kind"`
	DeviceID     string `json:"device_id"`
	Detail       string `json:"detail"`
	At           int64  `json:"at"`
	Seq          int64  `json:"seq"`
}

// GuardConfig bounds the device table, the challenge lifetime and the log.
type GuardConfig struct {
	MaxDevices   int   `json:"max_devices"`
	ChallengeTTL int64 `json:"challenge_ttl"`
	MaxEvents    int   `json:"max_events"`
}

// PasskeyMigrationGuard owns the credential table, the challenge table and the
// monotonic sequence shared by them.
type PasskeyMigrationGuard struct {
	cfg GuardConfig
}

// NewPasskeyMigrationGuard builds a guard with the given caps and lifetime.
func NewPasskeyMigrationGuard(cfg GuardConfig) *PasskeyMigrationGuard {
	return &PasskeyMigrationGuard{cfg: cfg}
}

// RegisterCredential stores one passkey and binds its first device.
func (g *PasskeyMigrationGuard) RegisterCredential(c Credential, at int64) error {
	return errNotImplemented
}

// BeginMigration issues a challenge for one target device.
func (g *PasskeyMigrationGuard) BeginMigration(req MigrationRequest) (Challenge, error) {
	return Challenge{}, errNotImplemented
}

// ApproveMigration consumes a challenge and binds the target device.
func (g *PasskeyMigrationGuard) ApproveMigration(challengeID string, proof MigrationProof) (MigrationResult, error) {
	return MigrationResult{}, errNotImplemented
}

// Authenticate checks one assertion counter against the credential watermark.
func (g *PasskeyMigrationGuard) Authenticate(credID, deviceID string, counter uint32, at int64) (AuthResult, error) {
	return AuthResult{}, errNotImplemented
}

// BindDevice binds another device to a credential without a challenge.
func (g *PasskeyMigrationGuard) BindDevice(credID, deviceID, pubKey string, at int64) error {
	return errNotImplemented
}

// FreezeCredential freezes a credential and its devices.
func (g *PasskeyMigrationGuard) FreezeCredential(credID, reason string) error {
	return errNotImplemented
}

// Unfreeze releases a frozen credential after an operator decision.
func (g *PasskeyMigrationGuard) Unfreeze(credID, reason string, at int64) error {
	return errNotImplemented
}

// Credential reads the projected state of one credential.
func (g *PasskeyMigrationGuard) Credential(credID string) (CredentialState, bool) {
	return CredentialState{}, false
}

// Devices lists the device bindings of one credential, ordered by device id.
func (g *PasskeyMigrationGuard) Devices(credID string) []DeviceBinding {
	return nil
}

// Events returns a copy of the per credential log, oldest first.
func (g *PasskeyMigrationGuard) Events(credID string) []Event {
	return nil
}

// SweepExpired drops expired challenges and returns how many were dropped.
func (g *PasskeyMigrationGuard) SweepExpired(now int64) int {
	return 0
}

// Snapshot serialises credentials, devices, challenges, the log and the seq.
func (g *PasskeyMigrationGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PasskeyMigrationGuard) Restore(data []byte) error {
	return errNotImplemented
}
