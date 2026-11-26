// Package task keeps legacy U2F authenticators usable while their key handles
// are re-bound to WebAuthn credentials, and decides when a signature counter
// may be trusted.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

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

const snapshotVersion = 1

type credentialKey struct {
	tenant string
	user   string
	credID string
}

type mappingKey struct {
	appID     string
	keyHandle string
}

type mappingRecord struct {
	AppID     string `json:"appid"`
	KeyHandle []byte `json:"key_handle"`
	CredID    []byte `json:"cred_id"`
}

type snapshotData struct {
	Version     int             `json:"version"`
	Seq         int64           `json:"seq"`
	Migrated    int             `json:"migrated"`
	Credentials []Credential    `json:"credentials"`
	Mappings    []mappingRecord `json:"mappings"`
}

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
	mu       sync.Mutex
	limits   Limits
	seq      int64
	migrated int
	creds    map[credentialKey]*Credential
	mappings map[mappingKey][]byte
}

// NewAppIdMigrationGuard builds a guard with the given caps.
func NewAppIdMigrationGuard(limits Limits) *AppIdMigrationGuard {
	return &AppIdMigrationGuard{
		limits:   limits,
		creds:    make(map[credentialKey]*Credential),
		mappings: make(map[mappingKey][]byte),
	}
}

func (g *AppIdMigrationGuard) ensureLocked() {
	if g.creds == nil {
		g.creds = make(map[credentialKey]*Credential)
	}
	if g.mappings == nil {
		g.mappings = make(map[mappingKey][]byte)
	}
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	cloned := make([]byte, len(value))
	copy(cloned, value)
	return cloned
}

func cloneCredential(cred Credential) Credential {
	cred.CredID = cloneBytes(cred.CredID)
	cred.KeyHandle = cloneBytes(cred.KeyHandle)
	return cred
}

func newCredentialKey(tenantID, userID string, credID []byte) credentialKey {
	return credentialKey{tenant: tenantID, user: userID, credID: string(credID)}
}

func newMappingKey(appID string, keyHandle []byte) mappingKey {
	return mappingKey{appID: appID, keyHandle: string(keyHandle)}
}

func blankIdentity(userID string, credID []byte) bool {
	return strings.TrimSpace(userID) == "" || len(credID) == 0
}

func (g *AppIdMigrationGuard) legacyAppIDRegisteredLocked(appID string) bool {
	for key := range g.mappings {
		if key.appID == appID {
			return true
		}
	}
	return false
}

func (g *AppIdMigrationGuard) countUserCredentialsLocked(tenantID, userID string) int {
	count := 0
	for key := range g.creds {
		if key.tenant == tenantID && key.user == userID {
			count++
		}
	}
	return count
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
	if strings.TrimSpace(appid) == "" {
		return ErrBlankLegacyAppID
	}
	if len(keyHandle) == 0 || len(credID) == 0 {
		return ErrBlankIdentity
	}

	appid = strings.TrimSpace(appid)
	key := newMappingKey(appid, keyHandle)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	existing, exists := g.mappings[key]
	if exists {
		if bytes.Equal(existing, credID) {
			return nil
		}
		return ErrMappingConflict
	}
	if len(g.mappings) >= g.limits.MaxAppIDMappings {
		return ErrMappingLimit
	}

	g.mappings[key] = cloneBytes(credID)
	return nil
}

// Register stores a fresh credential, rewriting the key handle through the
// legacy mapping table when the attestation carries a legacy appid.
func (g *AppIdMigrationGuard) Register(userID string, att Attestation) (Credential, error) {
	userID = strings.TrimSpace(userID)
	if blankIdentity(userID, att.CredID) {
		return Credential{}, ErrBlankIdentity
	}

	appID := strings.TrimSpace(att.AppID)
	if appID != "" && len(att.KeyHandle) == 0 {
		return Credential{}, ErrBlankIdentity
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	credKey := newCredentialKey(att.TenantID, userID, att.CredID)
	_, alreadyRegistered := g.creds[credKey]
	if !alreadyRegistered &&
		g.countUserCredentialsLocked(att.TenantID, userID) >= g.limits.MaxCredentialsPerUser {
		return Credential{}, ErrCredentialLimit
	}

	var mappingKey mappingKey
	createMapping := false
	if appID != "" {
		mappingKey = newMappingKey(appID, att.KeyHandle)
		mappedCredID, mappingExists := g.mappings[mappingKey]
		if mappingExists {
			if !bytes.Equal(mappedCredID, att.CredID) {
				return Credential{}, ErrMappingConflict
			}
		} else {
			if !g.legacyAppIDRegisteredLocked(appID) {
				return Credential{}, ErrAppIDMismatch
			}
			if len(g.mappings) >= g.limits.MaxAppIDMappings {
				return Credential{}, ErrMappingLimit
			}
			createMapping = true
		}
	}

	g.seq++
	cred := Credential{
		TenantID:         att.TenantID,
		UserID:           userID,
		CredID:           cloneBytes(att.CredID),
		KeyHandle:        cloneBytes(att.KeyHandle),
		AppID:            appID,
		Scheme:           SchemeNative,
		AAGUID:           att.AAGUID,
		Counter:          att.Counter,
		CounterSupported: att.CounterSupported,
		Flag:             FlagNone,
		Created:          g.seq,
	}
	if appID != "" {
		cred.Scheme = SchemeLegacy
	}

	g.creds[credKey] = &cred
	if createMapping {
		g.mappings[mappingKey] = cloneBytes(att.CredID)
	}

	return cloneCredential(cred), nil
}

// Authenticate advances the counter of one credential after checking the appid
// extension and the counter watermark.
func (g *AppIdMigrationGuard) Authenticate(userID string, assertion Assertion) (Result, error) {
	userID = strings.TrimSpace(userID)
	if blankIdentity(userID, assertion.CredID) {
		return Result{}, ErrBlankIdentity
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	credKey := newCredentialKey(assertion.TenantID, userID, assertion.CredID)
	cred, exists := g.creds[credKey]
	if !exists {
		return Result{}, ErrUnknownCredential
	}
	if cred.Flag == FlagSuspicious {
		return Result{}, ErrCredentialRevoked
	}

	degraded := false
	if assertion.AppID != "" {
		if assertion.AppID != cred.AppID {
			return Result{}, ErrAppIDMismatch
		}
	} else if cred.AppID != "" {
		if cred.Scheme != SchemeLegacy {
			return Result{}, ErrAppIDMismatch
		}
		degraded = true
	}

	if cred.CounterSupported {
		if assertion.Counter <= cred.Counter {
			cred.Flag = FlagSuspicious
			return Result{}, ErrCounterRollback
		}
	}
	cred.Counter = assertion.Counter

	return Result{
		TenantID: cred.TenantID,
		UserID:   cred.UserID,
		CredID:   cloneBytes(cred.CredID),
		AppID:    cred.AppID,
		Counter:  cred.Counter,
		Degraded: degraded,
	}, nil
}

// Migrate rewrites every legacy credential of one user into the native scheme
// and reports how many records changed.
func (g *AppIdMigrationGuard) Migrate(userID string) (int, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, ErrBlankIdentity
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	pending := make([]*Credential, 0)
	for _, cred := range g.creds {
		if cred.UserID == userID && cred.Scheme == SchemeLegacy && cred.AppID != "" {
			pending = append(pending, cred)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}
	if len(pending) > g.limits.MaxMigrationBatch {
		return 0, ErrBatchLimit
	}

	for _, cred := range pending {
		cred.Scheme = SchemeNative
		cred.AppID = ""
	}
	g.migrated += len(pending)
	return len(pending), nil
}

// Credentials lists the credentials of one user, oldest first.
func (g *AppIdMigrationGuard) Credentials(userID string) []Credential {
	userID = strings.TrimSpace(userID)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	credentials := make([]Credential, 0)
	for _, cred := range g.creds {
		if cred.UserID == userID {
			credentials = append(credentials, cloneCredential(*cred))
		}
	}
	sort.SliceStable(credentials, func(i, j int) bool {
		if credentials[i].Created != credentials[j].Created {
			return credentials[i].Created < credentials[j].Created
		}
		return string(credentials[i].CredID) < string(credentials[j].CredID)
	})
	return credentials
}

// Mapping resolves one key handle under a legacy appid.
func (g *AppIdMigrationGuard) Mapping(appid string, keyHandle []byte) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	credID, exists := g.mappings[newMappingKey(appid, keyHandle)]
	if !exists {
		return nil, false
	}
	return cloneBytes(credID), true
}

// Stats counts credentials, mappings, migrated records and flagged records.
func (g *AppIdMigrationGuard) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	stats := Stats{
		Mappings: len(g.mappings),
		Migrated: g.migrated,
	}
	for _, cred := range g.creds {
		stats.Credentials++
		if cred.Flag == FlagSuspicious {
			stats.Suspicious++
		}
	}
	return stats
}

// Snapshot serialises credentials, mappings, counters and the sequence.
func (g *AppIdMigrationGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()

	snapshot := snapshotData{
		Version:     snapshotVersion,
		Seq:         g.seq,
		Migrated:    g.migrated,
		Credentials: make([]Credential, 0, len(g.creds)),
		Mappings:    make([]mappingRecord, 0, len(g.mappings)),
	}
	for _, cred := range g.creds {
		snapshot.Credentials = append(snapshot.Credentials, cloneCredential(*cred))
	}
	sort.Slice(snapshot.Credentials, func(i, j int) bool {
		if snapshot.Credentials[i].Created != snapshot.Credentials[j].Created {
			return snapshot.Credentials[i].Created < snapshot.Credentials[j].Created
		}
		return string(snapshot.Credentials[i].CredID) < string(snapshot.Credentials[j].CredID)
	})
	for key, credID := range g.mappings {
		snapshot.Mappings = append(snapshot.Mappings, mappingRecord{
			AppID:     key.appID,
			KeyHandle: cloneBytes([]byte(key.keyHandle)),
			CredID:    cloneBytes(credID),
		})
	}
	sort.Slice(snapshot.Mappings, func(i, j int) bool {
		if snapshot.Mappings[i].AppID != snapshot.Mappings[j].AppID {
			return snapshot.Mappings[i].AppID < snapshot.Mappings[j].AppID
		}
		return string(snapshot.Mappings[i].KeyHandle) < string(snapshot.Mappings[j].KeyHandle)
	})

	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *AppIdMigrationGuard) Restore(data []byte) error {
	var snapshot snapshotData
	if len(bytes.TrimSpace(data)) == 0 {
		return ErrBadSnapshot
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ErrBadSnapshot
	}
	if snapshot.Version != snapshotVersion || snapshot.Seq < 0 || snapshot.Migrated < 0 {
		return ErrBadSnapshot
	}

	credentials := make(map[credentialKey]*Credential, len(snapshot.Credentials))
	var maxCreated int64
	for i := range snapshot.Credentials {
		cred := cloneCredential(snapshot.Credentials[i])
		if strings.TrimSpace(cred.UserID) == "" || len(cred.CredID) == 0 || cred.Created <= 0 {
			return ErrBadSnapshot
		}
		if cred.Scheme != SchemeNative && cred.Scheme != SchemeLegacy {
			return ErrBadSnapshot
		}
		if cred.Flag != FlagNone && cred.Flag != FlagSuspicious {
			return ErrBadSnapshot
		}
		if (cred.Scheme == SchemeLegacy) != (cred.AppID != "") {
			return ErrBadSnapshot
		}

		key := newCredentialKey(cred.TenantID, cred.UserID, cred.CredID)
		if _, exists := credentials[key]; exists {
			return ErrBadSnapshot
		}
		credentials[key] = &cred
		if cred.Created > maxCreated {
			maxCreated = cred.Created
		}
	}

	mappings := make(map[mappingKey][]byte, len(snapshot.Mappings))
	for _, record := range snapshot.Mappings {
		if strings.TrimSpace(record.AppID) == "" || len(record.KeyHandle) == 0 || len(record.CredID) == 0 {
			return ErrBadSnapshot
		}
		record.AppID = strings.TrimSpace(record.AppID)
		key := newMappingKey(record.AppID, record.KeyHandle)
		if existing, exists := mappings[key]; exists && !bytes.Equal(existing, record.CredID) {
			return ErrBadSnapshot
		}
		mappings[key] = cloneBytes(record.CredID)
	}

	seq := snapshot.Seq
	if maxCreated > seq {
		seq = maxCreated
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLocked()
	g.creds = credentials
	g.mappings = mappings
	g.seq = seq
	g.migrated = snapshot.Migrated
	return nil
}
