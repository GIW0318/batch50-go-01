// Package task guards passkey migration between devices: it binds a device
// public key behind a one shot challenge, advances the credential signature
// counter and freezes a credential when the counter walks backwards.
package task

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

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

type storedCredential struct {
	Credential
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Counter  uint32 `json:"counter"`
	FrozenAt int64  `json:"frozen_at"`
}

type storedDevice struct {
	DeviceBinding
	CredentialID string `json:"credential_id"`
}

type storedChallenge struct {
	Challenge
	Consumed bool `json:"consumed"`
}

type snapshotData struct {
	Version     int                `json:"version"`
	Seq         int64              `json:"seq"`
	Credentials []storedCredential `json:"credentials"`
	Devices     []storedDevice     `json:"devices"`
	Challenges  []storedChallenge  `json:"challenges"`
	Events      []Event            `json:"events"`
}

const snapshotVersion = 1

// PasskeyMigrationGuard owns the credential table, the challenge table and the
// monotonic sequence shared by them.
type PasskeyMigrationGuard struct {
	mu         sync.RWMutex
	cfg        GuardConfig
	seq        int64
	creds      map[string]*storedCredential
	devices    map[string]map[string]*storedDevice
	challenges map[string]*storedChallenge
	events     map[string][]Event
}

// NewPasskeyMigrationGuard builds a guard with the given caps and lifetime.
func NewPasskeyMigrationGuard(cfg GuardConfig) *PasskeyMigrationGuard {
	if cfg.MaxDevices < 0 {
		cfg.MaxDevices = 0
	}
	if cfg.MaxEvents < 0 {
		cfg.MaxEvents = 0
	}
	if cfg.ChallengeTTL < 0 {
		cfg.ChallengeTTL = 0
	}
	return &PasskeyMigrationGuard{
		cfg:        cfg,
		creds:      make(map[string]*storedCredential),
		devices:    make(map[string]map[string]*storedDevice),
		challenges: make(map[string]*storedChallenge),
		events:     make(map[string][]Event),
	}
}

func blank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func randomToken(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

func (g *PasskeyMigrationGuard) nextSequenceLocked() int64 {
	g.seq++
	return g.seq
}

func (g *PasskeyMigrationGuard) appendEventLocked(credID, kind, deviceID, detail string, at int64) Event {
	event := Event{
		CredentialID: credID,
		Kind:         kind,
		DeviceID:     deviceID,
		Detail:       detail,
		At:           at,
		Seq:          g.nextSequenceLocked(),
	}

	events := append(g.events[credID], event)
	switch {
	case g.cfg.MaxEvents == 0:
		events = nil
	case g.cfg.MaxEvents > 0 && len(events) > g.cfg.MaxEvents:
		kept := make([]Event, g.cfg.MaxEvents)
		copy(kept, events[len(events)-g.cfg.MaxEvents:])
		events = kept
	}
	g.events[credID] = events
	return event
}

// RegisterCredential stores one passkey and binds its first device.
func (g *PasskeyMigrationGuard) RegisterCredential(c Credential, at int64) error {
	if blank(c.ID) || blank(c.UserID) || blank(c.PubKey) || blank(c.DeviceID) {
		return ErrBadRequest
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if existing, ok := g.creds[c.ID]; ok {
		if existing.Credential == c {
			return nil
		}
		return ErrCredentialConflict
	}

	cred := &storedCredential{
		Credential: c,
		State:      CredentialActive,
	}
	device := &storedDevice{
		CredentialID: c.ID,
		DeviceBinding: DeviceBinding{
			DeviceID: c.DeviceID,
			PubKey:   c.PubKey,
			State:    DeviceActive,
			BoundAt:  at,
		},
	}

	g.creds[c.ID] = cred
	g.devices[c.ID] = map[string]*storedDevice{c.DeviceID: device}
	event := g.appendEventLocked(c.ID, EventRegistered, c.DeviceID, "", at)
	device.Seq = event.Seq
	return nil
}

// BeginMigration issues a challenge for one target device.
func (g *PasskeyMigrationGuard) BeginMigration(req MigrationRequest) (Challenge, error) {
	if blank(req.CredentialID) || blank(req.DeviceID) || blank(req.PubKey) {
		return Challenge{}, ErrBadRequest
	}

	id, err := randomToken("ch_", 16)
	if err != nil {
		return Challenge{}, fmt.Errorf("generate challenge id: %w", err)
	}
	nonce, err := randomToken("nonce_", 24)
	if err != nil {
		return Challenge{}, fmt.Errorf("generate challenge nonce: %w", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	cred, ok := g.creds[req.CredentialID]
	if !ok {
		return Challenge{}, ErrCredentialNotFound
	}
	if cred.State == CredentialFrozen {
		return Challenge{}, ErrCredentialFrozen
	}

	devices := g.devices[req.CredentialID]
	if _, targetExists := devices[req.DeviceID]; !targetExists && len(devices) >= g.cfg.MaxDevices {
		return Challenge{}, ErrDeviceLimit
	}

	challenge := &storedChallenge{
		Challenge: Challenge{
			ID:           id,
			CredentialID: req.CredentialID,
			DeviceID:     req.DeviceID,
			PubKey:       req.PubKey,
			Nonce:        nonce,
			ExpiresAt:    req.At + g.cfg.ChallengeTTL,
			Seq:          g.nextSequenceLocked(),
		},
	}
	g.challenges[id] = challenge
	return challenge.Challenge, nil
}

// ApproveMigration consumes a challenge and binds the target device.
func (g *PasskeyMigrationGuard) ApproveMigration(challengeID string, proof MigrationProof) (MigrationResult, error) {
	if blank(challengeID) {
		return MigrationResult{}, ErrBadRequest
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	challenge, ok := g.challenges[challengeID]
	if !ok {
		return MigrationResult{}, ErrBadRequest
	}
	if challenge.Consumed {
		return MigrationResult{}, ErrChallengeReused
	}
	if proof.Nonce != challenge.Nonce || blank(proof.Signature) {
		return MigrationResult{}, ErrBadRequest
	}
	if proof.At > challenge.ExpiresAt {
		return MigrationResult{}, ErrChallengeExpired
	}

	cred, ok := g.creds[challenge.CredentialID]
	if !ok {
		return MigrationResult{}, ErrCredentialNotFound
	}
	if cred.State == CredentialFrozen {
		return MigrationResult{}, ErrCredentialFrozen
	}

	devices := g.devices[challenge.CredentialID]
	target, targetExists := devices[challenge.DeviceID]
	if !targetExists && len(devices) >= g.cfg.MaxDevices {
		return MigrationResult{}, ErrDeviceLimit
	}

	var retired *storedDevice
	for _, device := range devices {
		if device.DeviceID == challenge.DeviceID || device.State != DeviceActive {
			continue
		}
		device.State = DeviceRetired
		if retired == nil || device.Seq > retired.Seq ||
			(device.Seq == retired.Seq && device.DeviceID < retired.DeviceID) {
			retired = device
		}
	}

	if targetExists {
		target.PubKey = challenge.PubKey
		target.State = DeviceActive
		target.BoundAt = proof.At
	} else {
		target = &storedDevice{
			CredentialID: challenge.CredentialID,
			DeviceBinding: DeviceBinding{
				DeviceID: challenge.DeviceID,
				PubKey:   challenge.PubKey,
				State:    DeviceActive,
				BoundAt:  proof.At,
			},
		}
		devices[challenge.DeviceID] = target
	}

	challenge.Consumed = true
	retiredDevice := ""
	detail := ""
	if retired != nil {
		retiredDevice = retired.DeviceID
		detail = "retired_device=" + retiredDevice
	}
	event := g.appendEventLocked(challenge.CredentialID, EventMigration, challenge.DeviceID, detail, proof.At)
	target.Seq = event.Seq

	return MigrationResult{
		CredentialID:  challenge.CredentialID,
		DeviceID:      challenge.DeviceID,
		RetiredDevice: retiredDevice,
		Bound:         true,
		Seq:           event.Seq,
	}, nil
}

// Authenticate checks one assertion counter against the credential watermark.
func (g *PasskeyMigrationGuard) Authenticate(credID, deviceID string, counter uint32, at int64) (AuthResult, error) {
	if blank(credID) || blank(deviceID) {
		return AuthResult{}, ErrBadRequest
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	cred, ok := g.creds[credID]
	if !ok {
		return AuthResult{CredentialID: credID, DeviceID: deviceID, Counter: counter}, ErrCredentialNotFound
	}

	result := AuthResult{
		CredentialID: credID,
		DeviceID:     deviceID,
		Counter:      counter,
		Watermark:    cred.Counter,
	}
	if cred.State == CredentialFrozen {
		return result, ErrCredentialFrozen
	}

	device, ok := g.devices[credID][deviceID]
	if !ok {
		return result, ErrBadRequest
	}
	if device.State == DeviceRetired {
		return result, ErrDeviceRetired
	}

	if counter <= cred.Counter {
		reason := fmt.Sprintf(
			"counter rollback/clone detected: counter=%d watermark=%d device=%s",
			counter, cred.Counter, deviceID,
		)
		cred.State = CredentialFrozen
		cred.Reason = reason
		cred.FrozenAt = at
		event := g.appendEventLocked(credID, EventFrozen, deviceID, reason, at)
		result.Seq = event.Seq
		return result, ErrCounterRollback
	}

	cred.Counter = counter
	device.Counter = counter
	event := g.appendEventLocked(credID, EventAuthenticated, deviceID, "", at)
	result.Watermark = cred.Counter
	result.OK = true
	result.Seq = event.Seq
	return result, nil
}

// BindDevice binds another device to a credential without a challenge.
func (g *PasskeyMigrationGuard) BindDevice(credID, deviceID, pubKey string, at int64) error {
	if blank(credID) || blank(deviceID) || blank(pubKey) {
		return ErrBadRequest
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	cred, ok := g.creds[credID]
	if !ok {
		return ErrCredentialNotFound
	}
	if cred.State == CredentialFrozen {
		return ErrCredentialFrozen
	}

	devices := g.devices[credID]
	if device, ok := devices[deviceID]; ok {
		device.PubKey = pubKey
		device.State = DeviceActive
		device.BoundAt = at
		device.Seq = g.nextSequenceLocked()
		return nil
	}
	if len(devices) >= g.cfg.MaxDevices {
		return ErrDeviceLimit
	}

	devices[deviceID] = &storedDevice{
		CredentialID: credID,
		DeviceBinding: DeviceBinding{
			DeviceID: deviceID,
			PubKey:   pubKey,
			State:    DeviceActive,
			BoundAt:  at,
			Seq:      g.nextSequenceLocked(),
		},
	}
	return nil
}

// FreezeCredential freezes a credential and its devices.
func (g *PasskeyMigrationGuard) FreezeCredential(credID, reason string) error {
	if blank(credID) {
		return ErrBadRequest
	}
	reason = strings.TrimSpace(reason)
	at := time.Now().Unix()

	g.mu.Lock()
	defer g.mu.Unlock()

	cred, ok := g.creds[credID]
	if !ok {
		return ErrCredentialNotFound
	}
	cred.State = CredentialFrozen
	cred.Reason = reason
	cred.FrozenAt = at
	g.appendEventLocked(credID, EventFrozen, "", reason, at)
	return nil
}

// Unfreeze releases a frozen credential after an operator decision.
func (g *PasskeyMigrationGuard) Unfreeze(credID, reason string, at int64) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrBadRequest
	}
	if blank(credID) {
		return ErrBadRequest
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	cred, ok := g.creds[credID]
	if !ok {
		return ErrCredentialNotFound
	}
	cred.State = CredentialActive
	cred.Reason = ""
	cred.FrozenAt = 0
	g.appendEventLocked(credID, EventUnfrozen, "", reason, at)
	return nil
}

// Credential reads the projected state of one credential.
func (g *PasskeyMigrationGuard) Credential(credID string) (CredentialState, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	cred, ok := g.creds[credID]
	if !ok {
		return CredentialState{}, false
	}
	return CredentialState{
		ID:       cred.ID,
		UserID:   cred.UserID,
		State:    cred.State,
		Reason:   cred.Reason,
		Counter:  cred.Counter,
		Devices:  len(g.devices[credID]),
		FrozenAt: cred.FrozenAt,
	}, true
}

// Devices lists the device bindings of one credential, ordered by device id.
func (g *PasskeyMigrationGuard) Devices(credID string) []DeviceBinding {
	g.mu.RLock()
	defer g.mu.RUnlock()

	devices := g.devices[credID]
	if len(devices) == 0 {
		return nil
	}
	out := make([]DeviceBinding, 0, len(devices))
	for _, device := range devices {
		out = append(out, device.DeviceBinding)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

// Events returns a copy of the per credential log, oldest first.
func (g *PasskeyMigrationGuard) Events(credID string) []Event {
	g.mu.RLock()
	defer g.mu.RUnlock()

	events := g.events[credID]
	if len(events) == 0 {
		return nil
	}
	out := append([]Event(nil), events...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Seq < out[j].Seq
	})
	return out
}

// SweepExpired drops expired, unused challenges and returns how many were
// dropped. Consumed challenges stay as replay tombstones.
func (g *PasskeyMigrationGuard) SweepExpired(now int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	removed := 0
	for id, challenge := range g.challenges {
		if challenge.Consumed {
			continue
		}
		if challenge.ExpiresAt < now {
			delete(g.challenges, id)
			removed++
		}
	}
	return removed
}

// Snapshot serialises credentials, devices, challenges, the log and the seq.
func (g *PasskeyMigrationGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	snap := snapshotData{
		Version: snapshotVersion,
		Seq:     g.seq,
	}
	for _, cred := range g.creds {
		snap.Credentials = append(snap.Credentials, *cred)
	}
	for _, devices := range g.devices {
		for _, device := range devices {
			snap.Devices = append(snap.Devices, *device)
		}
	}
	for _, challenge := range g.challenges {
		snap.Challenges = append(snap.Challenges, *challenge)
	}
	for _, events := range g.events {
		snap.Events = append(snap.Events, events...)
	}

	sort.Slice(snap.Credentials, func(i, j int) bool {
		return snap.Credentials[i].ID < snap.Credentials[j].ID
	})
	sort.Slice(snap.Devices, func(i, j int) bool {
		if snap.Devices[i].CredentialID != snap.Devices[j].CredentialID {
			return snap.Devices[i].CredentialID < snap.Devices[j].CredentialID
		}
		return snap.Devices[i].DeviceID < snap.Devices[j].DeviceID
	})
	sort.Slice(snap.Challenges, func(i, j int) bool {
		return snap.Challenges[i].ID < snap.Challenges[j].ID
	})
	sort.Slice(snap.Events, func(i, j int) bool {
		if snap.Events[i].CredentialID != snap.Events[j].CredentialID {
			return snap.Events[i].CredentialID < snap.Events[j].CredentialID
		}
		return snap.Events[i].Seq < snap.Events[j].Seq
	})

	data, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PasskeyMigrationGuard) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return ErrBadSnapshot
	}

	var snap snapshotData
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	creds, devices, challenges, events, highSeq, err := buildSnapshotState(&snap, g.cfg.MaxEvents)
	if err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.creds = creds
	g.devices = devices
	g.challenges = challenges
	g.events = events
	// Keep the global sequence monotonic even when an older snapshot is
	// restored over a live guard.
	if highSeq > g.seq {
		g.seq = highSeq
	}
	return nil
}

func buildSnapshotState(snap *snapshotData, maxEvents int) (
	map[string]*storedCredential,
	map[string]map[string]*storedDevice,
	map[string]*storedChallenge,
	map[string][]Event,
	int64,
	error,
) {
	if snap.Version != snapshotVersion || snap.Seq < 0 {
		return nil, nil, nil, nil, 0, ErrBadSnapshot
	}

	highSeq := snap.Seq
	creds := make(map[string]*storedCredential, len(snap.Credentials))
	for i := range snap.Credentials {
		cred := snap.Credentials[i]
		if blank(cred.ID) || blank(cred.UserID) || blank(cred.PubKey) || blank(cred.DeviceID) {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		switch cred.State {
		case "":
			cred.State = CredentialActive
		case CredentialActive, CredentialFrozen:
		default:
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if _, exists := creds[cred.ID]; exists {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		creds[cred.ID] = &cred
	}

	devices := make(map[string]map[string]*storedDevice, len(snap.Devices))
	for i := range snap.Devices {
		device := snap.Devices[i]
		if blank(device.CredentialID) || blank(device.DeviceID) || blank(device.PubKey) || device.Seq < 0 {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if _, ok := creds[device.CredentialID]; !ok {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		switch device.State {
		case "":
			device.State = DeviceActive
		case DeviceActive, DeviceRetired:
		default:
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if devices[device.CredentialID] == nil {
			devices[device.CredentialID] = make(map[string]*storedDevice)
		}
		if _, exists := devices[device.CredentialID][device.DeviceID]; exists {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		devices[device.CredentialID][device.DeviceID] = &device
		highSeq = max64(highSeq, device.Seq)
	}

	challenges := make(map[string]*storedChallenge, len(snap.Challenges))
	for i := range snap.Challenges {
		challenge := snap.Challenges[i]
		if blank(challenge.ID) || blank(challenge.CredentialID) || blank(challenge.DeviceID) ||
			blank(challenge.PubKey) || blank(challenge.Nonce) || challenge.Seq < 0 {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if _, ok := creds[challenge.CredentialID]; !ok {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if _, exists := challenges[challenge.ID]; exists {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		challenges[challenge.ID] = &challenge
		highSeq = max64(highSeq, challenge.Seq)
	}

	events := make(map[string][]Event)
	for _, event := range snap.Events {
		if blank(event.CredentialID) || blank(event.Kind) || event.Seq <= 0 {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if _, ok := creds[event.CredentialID]; !ok {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		events[event.CredentialID] = append(events[event.CredentialID], event)
		highSeq = max64(highSeq, event.Seq)
	}
	for credID, credentialEvents := range events {
		if !sort.SliceIsSorted(credentialEvents, func(i, j int) bool {
			return credentialEvents[i].Seq < credentialEvents[j].Seq
		}) {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		for i := 1; i < len(credentialEvents); i++ {
			if credentialEvents[i].Seq == credentialEvents[i-1].Seq {
				return nil, nil, nil, nil, 0, ErrBadSnapshot
			}
		}
		switch {
		case maxEvents == 0:
			events[credID] = nil
		case maxEvents > 0 && len(credentialEvents) > maxEvents:
			kept := make([]Event, maxEvents)
			copy(kept, credentialEvents[len(credentialEvents)-maxEvents:])
			events[credID] = kept
		}
	}

	return creds, devices, challenges, events, highSeq, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
