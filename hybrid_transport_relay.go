// Package task pairs a desktop client with a phone authenticator over a short
// lived relay, forwards one login challenge through the paired channel and
// keeps only the assertion digest after the relay finishes.
package task

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Relay states.
const (
	RelayPending   = "pending"
	RelayAttached  = "attached"
	RelayForwarded = "forwarded"
	RelayCompleted = "completed"
	RelayCanceled  = "canceled"
	RelayExpired   = "expired"
)

// Channel state once the phone finished attaching.
const (
	ChannelOpen   = "open"
	ChannelClosed = "closed"
)

// Audit event kinds.
const (
	EventStarted   = "started"
	EventAttached  = "attached"
	EventForwarded = "forwarded"
	EventCompleted = "completed"
	EventCanceled  = "canceled"
	EventExpired   = "expired"
)

// Audit actors and reason codes.
const (
	ActorDesktop = "desktop"
	ActorPhone   = "phone"

	ReasonPairTokenMismatch = "reason_pair_token_mismatch"
	ReasonCanceledByDesktop = "reason_canceled_by_desktop"
	ReasonCanceledByPhone   = "reason_canceled_by_phone"
	ReasonRelayTimeout      = "reason_relay_timeout"
	ReasonOrderConflict     = "reason_order_conflict"
	ReasonLimitReached      = "reason_limit_reached"
)

// Errors returned by the relay.
var (
	ErrEmptyIdentity     = errors.New("desktop id and phone id are required")
	ErrRelayNotFound     = errors.New("relay not found")
	ErrRelayLimit        = errors.New("pending relay limit reached for this desktop")
	ErrBadLimit          = errors.New("relay limits must not be negative")
	ErrAttachDenied      = errors.New("relay attachment denied")
	ErrRelayExpired      = errors.New("relay expired")
	ErrRelayClosed       = errors.New("relay already finished")
	ErrChannelNotFound   = errors.New("channel not found")
	ErrChallengeTooLarge = errors.New("challenge exceeds the relay byte budget")
	ErrEmptyChallenge    = errors.New("challenge is empty")
	ErrEmptyAssertion    = errors.New("assertion is empty")
	ErrBadActor          = errors.New("cancel must name the desktop or the phone")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Limits bounds relay lifetime, concurrency and audit retention.
type Limits struct {
	MaxPendingPerDesktop int
	DefaultTTL           time.Duration
	MaxTTL               time.Duration
	MaxChallengeBytes    int
	MaxAudit             int
	Now                  func() time.Time
}

// RelayOpts narrows the lifetime and the byte budget of one relay.
type RelayOpts struct {
	TTL      time.Duration
	MaxBytes int
}

// Proof is the near field evidence presented by the phone when attaching.
type Proof struct {
	Token    string
	PhoneKey string
	Nonce    []byte
}

// Relay is one short lived pairing attempt between a desktop and a phone.
type Relay struct {
	ID        string
	DesktopID string
	State     string
	PairToken string
	CreatedAt time.Time
	ExpiresAt time.Time
	MaxBytes  int
	Seq       int64
}

// Channel is the paired transport that carries the challenge.
type Channel struct {
	ID         string
	RelayID    string
	DesktopID  string
	PhoneID    string
	State      string
	Credential string
	Forwards   int
	Seq        int64
}

// Result is the terminal outcome of a relay.
type Result struct {
	RelayID     string
	State       string
	Digest      string
	Bytes       int
	CompletedAt time.Time
	Seq         int64
}

// Event is one audit entry. A completed relay keeps the digest only, never
// the assertion bytes.
type Event struct {
	Kind   string
	Actor  string
	Reason string
	Digest string
	At     time.Time
	Seq    int64
}

type relayRecord struct {
	Relay
	ChannelID     string
	Digest        string
	AssertionSize int
	CompletedAt   time.Time
}

type channelRecord struct {
	Channel
	PhoneKey string
	Nonce    []byte
}

// HybridTransportRelay owns the relay table, the channel table, the counters
// and the audit trail.
type HybridTransportRelay struct {
	mu sync.Mutex

	limits        Limits
	relays        map[string]*relayRecord
	channels      map[string]*channelRecord
	audit         map[string][]Event
	nextRelayID   int64
	nextChannelID int64
}

// NewHybridTransportRelay builds a relay service with the given caps.
func NewHybridTransportRelay(limits Limits) *HybridTransportRelay {
	if limits.Now == nil {
		limits.Now = time.Now
	}
	return &HybridTransportRelay{
		limits:        limits,
		relays:        make(map[string]*relayRecord),
		channels:      make(map[string]*channelRecord),
		audit:         make(map[string][]Event),
		nextRelayID:   1,
		nextChannelID: 1,
	}
}

// Start opens a pending relay for one desktop.
func (r *HybridTransportRelay) Start(desktopID string, opts RelayOpts) (Relay, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if strings.TrimSpace(desktopID) == "" {
		return Relay{}, ErrEmptyIdentity
	}
	if hasNegativeLimits(r.limits) || opts.TTL < 0 || opts.MaxBytes < 0 {
		return Relay{}, ErrBadLimit
	}

	now := r.now()
	if r.limits.MaxPendingPerDesktop > 0 &&
		r.pendingLocked(desktopID, now) >= r.limits.MaxPendingPerDesktop {
		return Relay{}, ErrRelayLimit
	}

	ttl := opts.TTL
	if ttl == 0 {
		ttl = r.limits.DefaultTTL
	}
	if r.limits.MaxTTL > 0 && ttl > r.limits.MaxTTL {
		ttl = r.limits.MaxTTL
	}

	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = r.limits.MaxChallengeBytes
	}
	if r.limits.MaxChallengeBytes > 0 && maxBytes > r.limits.MaxChallengeBytes {
		maxBytes = r.limits.MaxChallengeBytes
	}

	id := "relay-" + strconv.FormatInt(r.nextRelayID, 10)
	r.nextRelayID++
	relay := &relayRecord{Relay: Relay{
		ID:        id,
		DesktopID: desktopID,
		State:     RelayPending,
		PairToken: "pair-" + id,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		MaxBytes:  maxBytes,
		Seq:       1,
	}}
	r.relays[id] = relay
	r.addAuditLocked(id, Event{
		Kind:  EventStarted,
		Actor: desktopID,
		At:    now,
		Seq:   relay.Seq,
	})
	return relay.Relay, nil
}

// Attach binds a phone to a pending relay through the near field proof.
func (r *HybridTransportRelay) Attach(relayID string, phoneID string, proof Proof) (Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	relay, ok := r.relays[relayID]
	if !ok {
		return Channel{}, ErrRelayNotFound
	}
	now := r.now()
	switch relay.State {
	case RelayExpired:
		return Channel{}, ErrRelayExpired
	case RelayCompleted, RelayCanceled:
		return Channel{}, ErrRelayClosed
	case RelayPending:
		if !now.Before(relay.ExpiresAt) {
			return Channel{}, ErrRelayExpired
		}
	default:
		return Channel{}, ErrAttachDenied
	}
	if strings.TrimSpace(phoneID) == "" {
		return Channel{}, ErrEmptyIdentity
	}
	if !proofMatches(proof, relay.PairToken, "") {
		return Channel{}, ErrAttachDenied
	}

	id := "chan-" + strconv.FormatInt(r.nextChannelID, 10)
	r.nextChannelID++
	channel := &channelRecord{
		Channel: Channel{
			ID:         id,
			RelayID:    relay.ID,
			DesktopID:  relay.DesktopID,
			PhoneID:    phoneID,
			State:      ChannelOpen,
			Credential: "cred-" + id,
		},
		PhoneKey: proof.PhoneKey,
		Nonce:    append([]byte(nil), proof.Nonce...),
	}
	r.channels[id] = channel
	relay.ChannelID = id
	relay.State = RelayAttached
	relay.Seq++
	r.addAuditLocked(relay.ID, Event{
		Kind:  EventAttached,
		Actor: phoneID,
		At:    now,
		Seq:   relay.Seq,
	})
	return channel.Channel, nil
}

// Forward sends one challenge through the channel and returns the envelope.
func (r *HybridTransportRelay) Forward(channelID string, challenge []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	channel, ok := r.channels[channelID]
	if !ok {
		return nil, ErrChannelNotFound
	}
	if channel.State != ChannelOpen {
		return nil, ErrRelayClosed
	}
	relay, ok := r.relays[channel.RelayID]
	if !ok || relay.ChannelID != channel.ID {
		return nil, ErrRelayClosed
	}
	now := r.now()
	switch relay.State {
	case RelayExpired:
		return nil, ErrRelayExpired
	case RelayCompleted, RelayCanceled:
		return nil, ErrRelayClosed
	case RelayAttached, RelayForwarded:
		if !now.Before(relay.ExpiresAt) {
			return nil, ErrRelayExpired
		}
	default:
		return nil, ErrRelayClosed
	}
	if len(challenge) == 0 {
		return nil, ErrEmptyChallenge
	}
	if relay.MaxBytes > 0 && len(challenge) > relay.MaxBytes {
		return nil, ErrChallengeTooLarge
	}

	channel.Seq++
	channel.Forwards++
	if relay.State == RelayAttached {
		relay.State = RelayForwarded
		relay.Seq++
		r.addAuditLocked(relay.ID, Event{
			Kind:  EventForwarded,
			Actor: relay.DesktopID,
			At:    now,
			Seq:   relay.Seq,
		})
	}
	return []byte(fmt.Sprintf("channel=%s;seq=%d;", channel.ID, channel.Seq)), nil
}

// Complete records the assertion returned by the phone.
func (r *HybridTransportRelay) Complete(relayID string, assertion []byte) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	relay, ok := r.relays[relayID]
	if !ok {
		return Result{}, ErrRelayNotFound
	}
	now := r.now()
	switch relay.State {
	case RelayExpired:
		return Result{}, ErrRelayExpired
	case RelayCompleted, RelayCanceled:
		return Result{}, ErrRelayClosed
	case RelayForwarded:
		if !now.Before(relay.ExpiresAt) {
			return Result{}, ErrRelayExpired
		}
	default:
		return Result{}, ErrRelayClosed
	}
	if len(assertion) == 0 {
		return Result{}, ErrEmptyAssertion
	}

	sum := sha256.Sum256([]byte(relay.ID + ":" + string(assertion)))
	digest := hex.EncodeToString(sum[:])
	relay.State = RelayCompleted
	relay.Seq++
	relay.Digest = digest
	relay.AssertionSize = len(assertion)
	relay.CompletedAt = now
	r.addAuditLocked(relay.ID, Event{
		Kind:   EventCompleted,
		Actor:  ActorPhone,
		Digest: digest,
		At:     now,
		Seq:    relay.Seq,
	})
	return Result{
		RelayID:     relay.ID,
		State:       relay.State,
		Digest:      digest,
		Bytes:       len(assertion),
		CompletedAt: now,
		Seq:         relay.Seq,
	}, nil
}

// Cancel closes a relay from one of its two ends.
func (r *HybridTransportRelay) Cancel(relayID string, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	relay, ok := r.relays[relayID]
	if !ok {
		return ErrRelayNotFound
	}
	if by != ActorDesktop && by != ActorPhone {
		return ErrBadActor
	}
	now := r.now()
	switch relay.State {
	case RelayExpired:
		return ErrRelayExpired
	case RelayCompleted, RelayCanceled:
		return ErrRelayClosed
	}
	if !now.Before(relay.ExpiresAt) {
		return ErrRelayExpired
	}

	relay.State = RelayCanceled
	relay.Seq++
	reason := ReasonCanceledByDesktop
	if by == ActorPhone {
		reason = ReasonCanceledByPhone
	}
	r.addAuditLocked(relay.ID, Event{
		Kind:   EventCanceled,
		Actor:  by,
		Reason: reason,
		At:     now,
		Seq:    relay.Seq,
	})
	return nil
}

// Rebind rebuilds the transport of a channel after the phone changed network.
func (r *HybridTransportRelay) Rebind(channelID string, proof Proof) (Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.channels[channelID]
	if !ok {
		return Channel{}, ErrChannelNotFound
	}
	if old.State != ChannelOpen {
		return Channel{}, ErrRelayClosed
	}
	relay, ok := r.relays[old.RelayID]
	if !ok || relay.ChannelID != old.ID {
		return Channel{}, ErrRelayClosed
	}
	now := r.now()
	switch relay.State {
	case RelayExpired:
		return Channel{}, ErrRelayExpired
	case RelayCompleted, RelayCanceled:
		return Channel{}, ErrRelayClosed
	case RelayAttached, RelayForwarded:
		if !now.Before(relay.ExpiresAt) {
			return Channel{}, ErrRelayExpired
		}
	default:
		return Channel{}, ErrRelayClosed
	}
	if !proofMatches(proof, relay.PairToken, old.Credential) || proof.PhoneKey != old.PhoneKey {
		return Channel{}, ErrAttachDenied
	}

	old.State = ChannelClosed
	id := "chan-" + strconv.FormatInt(r.nextChannelID, 10)
	r.nextChannelID++
	channel := &channelRecord{
		Channel: Channel{
			ID:         id,
			RelayID:    relay.ID,
			DesktopID:  relay.DesktopID,
			PhoneID:    old.PhoneID,
			State:      ChannelOpen,
			Credential: "cred-" + id,
		},
		PhoneKey: proof.PhoneKey,
		Nonce:    append([]byte(nil), proof.Nonce...),
	}
	r.channels[id] = channel
	relay.ChannelID = id
	return channel.Channel, nil
}

// RelayState returns the state of one relay, or an empty string when unknown.
func (r *HybridTransportRelay) RelayState(relayID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	relay, ok := r.relays[relayID]
	if !ok {
		return ""
	}
	return relay.State
}

// ChannelInfo reads one channel.
func (r *HybridTransportRelay) ChannelInfo(channelID string) (Channel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	channel, ok := r.channels[channelID]
	if !ok {
		return Channel{}, false
	}
	return channel.Channel, true
}

// Pending counts the relays of one desktop that have not finished yet.
func (r *HybridTransportRelay) Pending(desktopID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingLocked(desktopID, r.now())
}

// Audit returns the per relay audit trail, oldest first.
func (r *HybridTransportRelay) Audit(relayID string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := r.audit[relayID]
	if len(events) == 0 {
		return nil
	}
	return append([]Event(nil), events...)
}

// Sweep expires every relay whose deadline passed and returns the count.
func (r *HybridTransportRelay) Sweep() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	expired := 0
	for _, relay := range r.relays {
		if isTerminalRelay(relay.State) || now.Before(relay.ExpiresAt) {
			continue
		}
		relay.State = RelayExpired
		relay.Seq++
		r.addAuditLocked(relay.ID, Event{
			Kind:   EventExpired,
			Reason: ReasonRelayTimeout,
			At:     now,
			Seq:    relay.Seq,
		})
		expired++
	}
	return expired
}

type snapshotPayload struct {
	Version       int                `json:"version"`
	NextRelayID   int64              `json:"next_relay_id"`
	NextChannelID int64              `json:"next_channel_id"`
	Relays        []snapshotRelay    `json:"relays"`
	Channels      []snapshotChannel  `json:"channels"`
	Audit         map[string][]Event `json:"audit"`
}

type snapshotRelay struct {
	ID            string    `json:"id"`
	DesktopID     string    `json:"desktop_id"`
	State         string    `json:"state"`
	PairToken     string    `json:"pair_token"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	MaxBytes      int       `json:"max_bytes"`
	Seq           int64     `json:"seq"`
	ChannelID     string    `json:"channel_id,omitempty"`
	Digest        string    `json:"digest,omitempty"`
	AssertionSize int       `json:"assertion_size,omitempty"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
}

type snapshotChannel struct {
	ID         string `json:"id"`
	RelayID    string `json:"relay_id"`
	DesktopID  string `json:"desktop_id"`
	PhoneID    string `json:"phone_id"`
	State      string `json:"state"`
	Credential string `json:"credential"`
	Forwards   int    `json:"forwards"`
	Seq        int64  `json:"seq"`
	PhoneKey   string `json:"phone_key"`
	Nonce      []byte `json:"nonce,omitempty"`
}

// Snapshot serialises relays, channels, counters and the audit trail.
func (r *HybridTransportRelay) Snapshot() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	relayIDs := make([]string, 0, len(r.relays))
	for id := range r.relays {
		relayIDs = append(relayIDs, id)
	}
	sort.Strings(relayIDs)
	relays := make([]snapshotRelay, 0, len(relayIDs))
	for _, id := range relayIDs {
		relay := r.relays[id]
		relays = append(relays, snapshotRelay{
			ID:            relay.ID,
			DesktopID:     relay.DesktopID,
			State:         relay.State,
			PairToken:     relay.PairToken,
			CreatedAt:     relay.CreatedAt,
			ExpiresAt:     relay.ExpiresAt,
			MaxBytes:      relay.MaxBytes,
			Seq:           relay.Seq,
			ChannelID:     relay.ChannelID,
			Digest:        relay.Digest,
			AssertionSize: relay.AssertionSize,
			CompletedAt:   relay.CompletedAt,
		})
	}

	channelIDs := make([]string, 0, len(r.channels))
	for id := range r.channels {
		channelIDs = append(channelIDs, id)
	}
	sort.Strings(channelIDs)
	channels := make([]snapshotChannel, 0, len(channelIDs))
	for _, id := range channelIDs {
		channel := r.channels[id]
		channels = append(channels, snapshotChannel{
			ID:         channel.ID,
			RelayID:    channel.RelayID,
			DesktopID:  channel.DesktopID,
			PhoneID:    channel.PhoneID,
			State:      channel.State,
			Credential: channel.Credential,
			Forwards:   channel.Forwards,
			Seq:        channel.Seq,
			PhoneKey:   channel.PhoneKey,
			Nonce:      append([]byte(nil), channel.Nonce...),
		})
	}

	audit := make(map[string][]Event, len(r.audit))
	for id, events := range r.audit {
		audit[id] = append([]Event(nil), events...)
	}
	return json.Marshal(snapshotPayload{
		Version:       1,
		NextRelayID:   r.nextRelayID,
		NextChannelID: r.nextChannelID,
		Relays:        relays,
		Channels:      channels,
		Audit:         audit,
	})
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *HybridTransportRelay) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Version != 1 || payload.NextRelayID < 1 || payload.NextChannelID < 1 {
		return ErrBadSnapshot
	}

	relays := make(map[string]*relayRecord, len(payload.Relays))
	maxRelayID := int64(0)
	for _, saved := range payload.Relays {
		number, ok := sequenceNumber(saved.ID, "relay-")
		if !ok || relays[saved.ID] != nil || saved.DesktopID == "" ||
			saved.PairToken != "pair-"+saved.ID || saved.MaxBytes < 0 || saved.Seq < 1 ||
			!validRelayState(saved.State) {
			return ErrBadSnapshot
		}
		if number > maxRelayID {
			maxRelayID = number
		}
		relays[saved.ID] = &relayRecord{
			Relay: Relay{
				ID:        saved.ID,
				DesktopID: saved.DesktopID,
				State:     saved.State,
				PairToken: saved.PairToken,
				CreatedAt: saved.CreatedAt,
				ExpiresAt: saved.ExpiresAt,
				MaxBytes:  saved.MaxBytes,
				Seq:       saved.Seq,
			},
			ChannelID:     saved.ChannelID,
			Digest:        saved.Digest,
			AssertionSize: saved.AssertionSize,
			CompletedAt:   saved.CompletedAt,
		}
	}
	if payload.NextRelayID <= maxRelayID {
		return ErrBadSnapshot
	}

	channels := make(map[string]*channelRecord, len(payload.Channels))
	maxChannelID := int64(0)
	for _, saved := range payload.Channels {
		number, ok := sequenceNumber(saved.ID, "chan-")
		if !ok || channels[saved.ID] != nil || saved.RelayID == "" ||
			saved.DesktopID == "" || saved.PhoneID == "" ||
			saved.Credential != "cred-"+saved.ID || saved.Forwards < 0 || saved.Seq < 0 ||
			(saved.State != ChannelOpen && saved.State != ChannelClosed) {
			return ErrBadSnapshot
		}
		relay, ok := relays[saved.RelayID]
		if !ok || relay.DesktopID != saved.DesktopID {
			return ErrBadSnapshot
		}
		if number > maxChannelID {
			maxChannelID = number
		}
		channels[saved.ID] = &channelRecord{
			Channel: Channel{
				ID:         saved.ID,
				RelayID:    saved.RelayID,
				DesktopID:  saved.DesktopID,
				PhoneID:    saved.PhoneID,
				State:      saved.State,
				Credential: saved.Credential,
				Forwards:   saved.Forwards,
				Seq:        saved.Seq,
			},
			PhoneKey: saved.PhoneKey,
			Nonce:    append([]byte(nil), saved.Nonce...),
		}
	}
	if payload.NextChannelID <= maxChannelID {
		return ErrBadSnapshot
	}
	for _, relay := range relays {
		if relay.ChannelID == "" {
			continue
		}
		channel, ok := channels[relay.ChannelID]
		if !ok || channel.RelayID != relay.ID {
			return ErrBadSnapshot
		}
	}

	audit := make(map[string][]Event, len(payload.Audit))
	for relayID, events := range payload.Audit {
		if relays[relayID] == nil || len(events) == 0 {
			return ErrBadSnapshot
		}
		for _, event := range events {
			if event.Kind == "" || event.Seq < 1 {
				return ErrBadSnapshot
			}
		}
		audit[relayID] = append([]Event(nil), events...)
	}

	r.mu.Lock()
	r.relays = relays
	r.channels = channels
	r.audit = audit
	r.nextRelayID = payload.NextRelayID
	r.nextChannelID = payload.NextChannelID
	r.mu.Unlock()
	return nil
}

func (r *HybridTransportRelay) now() time.Time {
	if r.limits.Now == nil {
		return time.Now()
	}
	return r.limits.Now()
}

func (r *HybridTransportRelay) pendingLocked(desktopID string, now time.Time) int {
	count := 0
	for _, relay := range r.relays {
		if relay.DesktopID != desktopID || isTerminalRelay(relay.State) {
			continue
		}
		if !now.Before(relay.ExpiresAt) {
			continue
		}
		count++
	}
	return count
}

func (r *HybridTransportRelay) addAuditLocked(relayID string, event Event) {
	events := append(r.audit[relayID], event)
	if max := r.limits.MaxAudit; max > 0 && len(events) > max {
		events = append([]Event(nil), events[len(events)-max:]...)
	}
	r.audit[relayID] = events
}

func proofMatches(proof Proof, pairToken, channelCredential string) bool {
	if strings.TrimSpace(proof.PhoneKey) == "" || len(proof.Nonce) == 0 {
		return false
	}
	if secureEqual(proof.Token, pairToken) {
		return true
	}
	return channelCredential != "" && secureEqual(proof.Token, channelCredential)
}

func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func sequenceNumber(id, prefix string) (int64, bool) {
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(id, prefix)
	if suffix == "" || strings.HasPrefix(suffix, "0") || strings.HasPrefix(suffix, "-") {
		return 0, false
	}
	number, err := strconv.ParseInt(suffix, 10, 64)
	return number, err == nil && number > 0
}

func isTerminalRelay(state string) bool {
	return state == RelayCompleted || state == RelayCanceled || state == RelayExpired
}

func validRelayState(state string) bool {
	switch state {
	case RelayPending, RelayAttached, RelayForwarded, RelayCompleted, RelayCanceled, RelayExpired:
		return true
	default:
		return false
	}
}

func hasNegativeLimits(limits Limits) bool {
	return limits.MaxPendingPerDesktop < 0 || limits.DefaultTTL < 0 ||
		limits.MaxTTL < 0 || limits.MaxChallengeBytes < 0 || limits.MaxAudit < 0
}
