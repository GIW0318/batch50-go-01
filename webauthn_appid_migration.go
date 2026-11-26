// Package task keeps legacy U2F authenticators usable while their key handles
// are re-bound to WebAuthn credentials, and decides when a signature counter
// may be trusted.
package task

import (
	"errors"
	"sync"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Credential schemes reported on a credential record.
const (
	SchemeNative = "webauthn"
	SchemeLegacy = "u2f"
)

// Reasons that mark a credential as no longer usable.
const (
	FlagNone       = ""
	FlagSuspicious = "counter_suspicious"
)

// Errors returned by the guard.
var (
	ErrBlankIdentity     = errors.New("user id and credential id are required")
	ErrCredentialLimit   = errors.New("credential limit reached for this user")
	ErrMappingLimit      = errors.New("legacy appid mapping limit reached")
	ErrMappingConflict   = errors.New("key handle is already mapped to another credential")
	ErrBatchLimit        = errors.New("migration batch exceeds the configured limit")
	ErrAppIDMismatch     = errors.New("appid extension does not match a registered legacy appid")
	ErrUnknownCredential = errors.New("unknown credential for this user")
	ErrCounterRollback   = errors.New("signature counter did not advance")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
	ErrCredentialRevoked = errors.New("credential is flagged and may not authenticate")
	ErrBlankLegacyAppID  = errors.New("legacy appid is required")
)

// Limits bounds the live tables of the guard.
type Limits struct {
	MaxCredentialsPerUser int
	MaxAppIDMappings      int
	MaxMigrationBatch     int
}

// Attestation is the registration material returned by the authenticator.
// AppID is filled when the authenticator answers the FIDO appid extension.
type Attestation struct {
	TenantID         string `json:"tenant_id"`
	CredID           []byte `json:"cred_id"`
	KeyHandle        []byte `json:"key_handle"`
	AppID            string `json:"appid"`
	AAGUID           string `json:"aaguid"`
	Counter          uint32 `json:"counter"`
	CounterSupported bool   `json:"counter_supported"`
}

// Credential is the stored state of one registered authenticator.
type Credential struct {
	TenantID         string `json:"tenant_id"`
	UserID           string `json:"user_id"`
	CredID           []byte `json:"cred_id"`
	KeyHandle        []byte `json:"key_handle"`
	AppID            string `json:"appid"`
	Scheme           string `json:"scheme"`
	AAGUID           string `json:"aaguid"`
	Counter          uint32 `json:"counter"`
	CounterSupported bool   `json:"counter_supported"`
	Flag             string `json:"flag"`
	Created          int64  `json:"created"`
}

// Assertion is one authentication attempt against a stored credential.
// AppID is empty when the client did not answer the appid extension.
type Assertion struct {
	TenantID string `json:"tenant_id"`
	CredID   []byte `json:"cred_id"`
	AppID    string `json:"appid"`
	Counter  uint32 `json:"counter"`
}

// Result is the accepted outcome of one authentication attempt.
type Result struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	CredID   []byte `json:"cred_id"`
	AppID    string `json:"appid"`
	Counter  uint32 `json:"counter"`
	Degraded bool   `json:"degraded"`
}

// Stats is a read only view of the live tables.
type Stats struct {
	Credentials int `json:"credentials"`
	Mappings    int `json:"mappings"`
	Migrated    int `json:"migrated"`
	Suspicious  int `json:"suspicious"`
}

// AppIdMigrationGuard owns credentials, the legacy appid mapping table and the
// monotonic counter watermarks.
type AppIdMigrationGuard struct {
	mu     sync.Mutex
	limits Limits
}

// NewAppIdMigrationGuard builds a guard with the given caps.
func NewAppIdMigrationGuard(limits Limits) *AppIdMigrationGuard {
	return &AppIdMigrationGuard{limits: limits}
}

// SetLimits replaces the caps. Existing records are not trimmed.
func (g *AppIdMigrationGuard) SetLimits(limits Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = limits
}

// BindLegacy records that one FIDO key handle now belongs to a WebAuthn
// credential id under the given legacy appid.
func (g *AppIdMigrationGuard) BindLegacy(appid string, keyHandle, credID []byte) error {
	return errNotImplemented
}

// Register stores a fresh credential, rewriting the key handle through the
// legacy mapping table when the attestation carries a legacy appid.
func (g *AppIdMigrationGuard) Register(userID string, att Attestation) (Credential, error) {
	return Credential{}, errNotImplemented
}

// Authenticate advances the counter of one credential after checking the appid
// extension and the counter watermark.
func (g *AppIdMigrationGuard) Authenticate(userID string, assertion Assertion) (Result, error) {
	return Result{}, errNotImplemented
}

// Migrate rewrites every legacy credential of one user into the native scheme
// and reports how many records changed.
func (g *AppIdMigrationGuard) Migrate(userID string) (int, error) {
	return 0, errNotImplemented
}

// Credentials lists the credentials of one user, oldest first.
func (g *AppIdMigrationGuard) Credentials(userID string) []Credential {
	return nil
}

// Mapping resolves one key handle under a legacy appid.
func (g *AppIdMigrationGuard) Mapping(appid string, keyHandle []byte) ([]byte, bool) {
	return nil, false
}

// Stats counts credentials, mappings, migrated records and flagged records.
func (g *AppIdMigrationGuard) Stats() Stats {
	return Stats{}
}

// Snapshot serialises credentials, mappings, counters and the sequence.
func (g *AppIdMigrationGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *AppIdMigrationGuard) Restore(data []byte) error {
	return errNotImplemented
}
