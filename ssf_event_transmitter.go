// Package task signs security event tokens, fans them out to the receivers
// whose subscription matches the event, and keeps a delivery ledger with a
// bounded retry plan.
package task

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event types understood by the transmitter.
const (
	EventSessionRevoked    = "session-revoked"
	EventAccountDisabled   = "account-disabled"
	EventCredentialChanged = "credential-changed"
)

// Delivery states recorded in the ledger.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	StatusGone      = "gone"
)

// Errors returned by the transmitter.
var (
	ErrBlankTenant      = errors.New("tenant id is required")
	ErrUnknownEventType = errors.New("event type is not recognised")
	ErrEmptySubject     = errors.New("event subject range is empty")
	ErrBlankReceiver    = errors.New("receiver id and endpoint are required")
	ErrNoEventTypes     = errors.New("receiver must subscribe to at least one event type")
	ErrNoSigningKey     = errors.New("no signing key for this tenant")
	ErrUnknownEvent     = errors.New("unknown event id")
	ErrUnknownReceiver  = errors.New("receiver is not part of this event")
	ErrAlreadyDelivered = errors.New("event already delivered to this receiver")
	ErrDeliveryExpired  = errors.New("delivery attempts exhausted")
	ErrQueueFull        = errors.New("in flight queue is full")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

var (
	errNilSender       = errors.New("sender is not configured")
	errRetryInProgress = errors.New("delivery retry is already in progress")
	failureIDSequence  uint64
	backoffCapSeconds  = int64(300)
	snapshotFormatVer  = 1
)

// EventID identifies one published security event.
type EventID string

// SecurityEvent is one signal pushed to the subscribers.
type SecurityEvent struct {
	TenantID string   `json:"tenant_id"`
	Type     string   `json:"type"`
	Subject  string   `json:"subject"`
	Scope    []string `json:"scope"`
	IssuedAt int64    `json:"issued_at"`
}

// ReceiverSpec is one subscription. An empty Subjects list matches any
// subject of the event scope.
type ReceiverSpec struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	Endpoint    string   `json:"endpoint"`
	EventTypes  []string `json:"event_types"`
	Subjects    []string `json:"subjects"`
	MaxAttempts int      `json:"max_attempts"`
}

// DeliveryState is one ledger row: the delivery of one event to one receiver.
// NextIn is the backoff in seconds before the next attempt, computed as two to
// the power of attempts minus one and capped at 300.
type DeliveryState struct {
	ReceiverID string `json:"receiver_id"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	LastCode   int    `json:"last_code"`
	NextIn     int64  `json:"next_in"`
}

// Sender pushes one signed token to one receiver endpoint and returns the HTTP
// status code the receiver answered with.
type Sender interface {
	Deliver(receiverID, endpoint, token string) (int, error)
}

// Limits bounds the ledger and the in flight queue.
type Limits struct {
	MaxEvents   int
	MaxInFlight int
}

type deliveryRecord struct {
	receiverID  string
	endpoint    string
	token       string
	maxAttempts int
	status      string
	attempts    int
	lastCode    int
	nextIn      int64
	retrying    bool
}

type eventRecord struct {
	id         string
	sequence   uint64
	event      SecurityEvent
	deliveries []*deliveryRecord
	finished   bool
}

// SetEventTransmitter owns subscriptions, the signing keys and the ledger.
type SetEventTransmitter struct {
	mu            sync.Mutex
	sender        Sender
	limits        Limits
	keys          map[string][]byte
	subscriptions map[string]map[string]ReceiverSpec
	events        map[string]*eventRecord
	finished      []string
	nextSequence  uint64
	epoch         uint64
}

// NewSetEventTransmitter builds a transmitter on top of a sender.
func NewSetEventTransmitter(sender Sender, limits Limits) *SetEventTransmitter {
	return &SetEventTransmitter{
		sender:        sender,
		limits:        limits,
		keys:          make(map[string][]byte),
		subscriptions: make(map[string]map[string]ReceiverSpec),
		events:        make(map[string]*eventRecord),
	}
}

// SetKey installs the signing key of one tenant.
func (t *SetEventTransmitter) SetKey(tenantID string, key []byte) error {
	if strings.TrimSpace(tenantID) == "" {
		return ErrBlankTenant
	}
	keyCopy := append([]byte(nil), key...)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys[tenantID] = keyCopy
	return nil
}

// Subscribe creates or replaces one subscription. Only events published after
// the change are matched against the new spec.
func (t *SetEventTransmitter) Subscribe(spec ReceiverSpec) error {
	if strings.TrimSpace(spec.ID) == "" || strings.TrimSpace(spec.Endpoint) == "" {
		return ErrBlankReceiver
	}
	if strings.TrimSpace(spec.TenantID) == "" {
		return ErrBlankTenant
	}

	eventTypes := make([]string, 0, len(spec.EventTypes))
	for _, eventType := range spec.EventTypes {
		eventType = strings.TrimSpace(eventType)
		if eventType == "" {
			continue
		}
		if !isKnownEventType(eventType) {
			return ErrUnknownEventType
		}
		eventTypes = append(eventTypes, eventType)
	}
	if len(eventTypes) == 0 {
		return ErrNoEventTypes
	}

	subjects := make([]string, 0, len(spec.Subjects))
	for _, subject := range spec.Subjects {
		subject = strings.TrimSpace(subject)
		if subject != "" {
			subjects = append(subjects, subject)
		}
	}

	normalized := spec
	normalized.EventTypes = eventTypes
	normalized.Subjects = subjects
	if normalized.MaxAttempts < 1 {
		normalized.MaxAttempts = 1
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.subscriptions[spec.TenantID] == nil {
		t.subscriptions[spec.TenantID] = make(map[string]ReceiverSpec)
	}
	t.subscriptions[spec.TenantID][spec.ID] = copyReceiverSpec(normalized)
	return nil
}

// Publish signs and delivers one event to every matching receiver and returns
// its ledger id.
func (t *SetEventTransmitter) Publish(evt SecurityEvent) (EventID, error) {
	if err := validateEvent(evt); err != nil {
		return "", err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	key, ok := t.keys[evt.TenantID]
	if !ok {
		return "", ErrNoSigningKey
	}

	specs := t.matchingSubscribersLocked(evt)
	if len(specs) > 0 {
		maxInFlight := t.maxInFlightLocked()
		if t.pendingLocked()+len(specs) > maxInFlight {
			return "", ErrQueueFull
		}
	}

	eventID := t.newEventIDLocked()
	t.nextSequence++
	record := &eventRecord{
		id:       eventID,
		sequence: t.nextSequence,
		event: SecurityEvent{
			TenantID: evt.TenantID,
			Type:     evt.Type,
			Subject:  evt.Subject,
			Scope:    copyStrings(evt.Scope),
			IssuedAt: evt.IssuedAt,
		},
	}

	for _, spec := range specs {
		token, err := signEventToken(key, evt.TenantID, spec.ID, evt.IssuedAt, eventID, evt.Type)
		if err != nil {
			return "", err
		}
		record.deliveries = append(record.deliveries, &deliveryRecord{
			receiverID:  spec.ID,
			endpoint:    spec.Endpoint,
			token:       token,
			maxAttempts: spec.MaxAttempts,
			status:      StatusPending,
		})
	}

	t.events[eventID] = record
	for _, delivery := range record.deliveries {
		t.performDeliveryLocked(delivery)
	}
	t.finishIfDoneLocked(record)
	return EventID(eventID), nil
}

// Delivery reports the ledger rows of one event.
func (t *SetEventTransmitter) Delivery(eventID string) ([]DeliveryState, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	record, ok := t.events[eventID]
	if !ok {
		return nil, ErrUnknownEvent
	}
	states := make([]DeliveryState, 0, len(record.deliveries))
	for _, delivery := range record.deliveries {
		states = append(states, DeliveryState{
			ReceiverID: delivery.receiverID,
			Status:     delivery.status,
			Attempts:   delivery.attempts,
			LastCode:   delivery.lastCode,
			NextIn:     delivery.nextIn,
		})
	}
	return states, nil
}

// Retry re-attempts one delivery with the token it was first signed with.
func (t *SetEventTransmitter) Retry(eventID, receiverID string) error {
	t.mu.Lock()
	record, ok := t.events[eventID]
	if !ok {
		t.mu.Unlock()
		return ErrUnknownEvent
	}
	delivery := findDelivery(record, receiverID)
	if delivery == nil {
		t.mu.Unlock()
		return ErrUnknownReceiver
	}

	switch delivery.status {
	case StatusDelivered:
		t.mu.Unlock()
		return ErrAlreadyDelivered
	case StatusFailed, StatusGone:
		t.mu.Unlock()
		return ErrDeliveryExpired
	case StatusPending:
		if delivery.attempts >= delivery.maxAttempts {
			delivery.status = StatusFailed
			delivery.nextIn = 0
			t.finishIfDoneLocked(record)
			t.mu.Unlock()
			return ErrDeliveryExpired
		}
	default:
		t.mu.Unlock()
		return ErrUnknownEvent
	}

	if delivery.retrying {
		t.mu.Unlock()
		return errRetryInProgress
	}

	delivery.retrying = true
	endpoint := delivery.endpoint
	token := delivery.token
	epoch := t.epoch
	t.mu.Unlock()

	code, deliverErr := t.deliver(receiverID, endpoint, token)

	t.mu.Lock()
	defer t.mu.Unlock()

	if epoch != t.epoch {
		return errRetryInProgress
	}
	record, ok = t.events[eventID]
	if !ok {
		return ErrUnknownEvent
	}
	delivery = findDelivery(record, receiverID)
	if delivery == nil {
		return ErrUnknownReceiver
	}
	delivery.retrying = false
	delivery.attempts++
	t.applyDeliveryResultLocked(delivery, code, deliverErr)
	t.finishIfDoneLocked(record)
	return nil
}

// Subscribers lists the current subscriptions of one tenant.
func (t *SetEventTransmitter) Subscribers(tenantID string) []ReceiverSpec {
	t.mu.Lock()
	defer t.mu.Unlock()

	tenantSubscriptions := t.subscriptions[tenantID]
	if len(tenantSubscriptions) == 0 {
		return []ReceiverSpec{}
	}
	ids := make([]string, 0, len(tenantSubscriptions))
	for id := range tenantSubscriptions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	subscriptions := make([]ReceiverSpec, 0, len(ids))
	for _, id := range ids {
		subscriptions = append(subscriptions, copyReceiverSpec(tenantSubscriptions[id]))
	}
	return subscriptions
}

// Pending counts the deliveries that are still waiting for a retry.
func (t *SetEventTransmitter) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingLocked()
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotState struct {
	Keys          map[string][]byte                  `json:"keys"`
	Subscriptions map[string]map[string]ReceiverSpec `json:"subscriptions"`
	Events        []snapshotEvent                    `json:"events"`
	Finished      []string                           `json:"finished"`
	NextSequence  uint64                             `json:"next_sequence"`
}

type snapshotEvent struct {
	ID         string             `json:"id"`
	Sequence   uint64             `json:"sequence"`
	TenantID   string             `json:"tenant_id"`
	Type       string             `json:"type"`
	Subject    string             `json:"subject"`
	Scope      []string           `json:"scope"`
	IssuedAt   int64              `json:"issued_at"`
	Deliveries []snapshotDelivery `json:"deliveries"`
}

type snapshotDelivery struct {
	ReceiverID  string `json:"receiver_id"`
	Endpoint    string `json:"endpoint"`
	Token       string `json:"token"`
	MaxAttempts int    `json:"max_attempts"`
	Status      string `json:"status"`
	Attempts    int    `json:"attempts"`
	LastCode    int    `json:"last_code"`
	NextIn      int64  `json:"next_in"`
}

// Snapshot serialises subscriptions, keys, the ledger and the retry plan.
func (t *SetEventTransmitter) Snapshot() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state := snapshotState{
		Keys:          make(map[string][]byte, len(t.keys)),
		Subscriptions: make(map[string]map[string]ReceiverSpec, len(t.subscriptions)),
		Events:        make([]snapshotEvent, 0, len(t.events)),
		Finished:      append([]string(nil), t.finished...),
		NextSequence:  t.nextSequence,
	}
	for tenantID, key := range t.keys {
		state.Keys[tenantID] = append([]byte(nil), key...)
	}
	for tenantID, subscriptions := range t.subscriptions {
		copied := make(map[string]ReceiverSpec, len(subscriptions))
		for receiverID, spec := range subscriptions {
			copied[receiverID] = copyReceiverSpec(spec)
		}
		state.Subscriptions[tenantID] = copied
	}

	eventIDs := make([]string, 0, len(t.events))
	for eventID := range t.events {
		eventIDs = append(eventIDs, eventID)
	}
	sort.Strings(eventIDs)
	for _, eventID := range eventIDs {
		record := t.events[eventID]
		snapshot := snapshotEvent{
			ID:         record.id,
			Sequence:   record.sequence,
			TenantID:   record.event.TenantID,
			Type:       record.event.Type,
			Subject:    record.event.Subject,
			Scope:      copyStrings(record.event.Scope),
			IssuedAt:   record.event.IssuedAt,
			Deliveries: make([]snapshotDelivery, 0, len(record.deliveries)),
		}
		for _, delivery := range record.deliveries {
			snapshot.Deliveries = append(snapshot.Deliveries, snapshotDelivery{
				ReceiverID:  delivery.receiverID,
				Endpoint:    delivery.endpoint,
				Token:       delivery.token,
				MaxAttempts: delivery.maxAttempts,
				Status:      delivery.status,
				Attempts:    delivery.attempts,
				LastCode:    delivery.lastCode,
				NextIn:      delivery.nextIn,
			})
		}
		state.Events = append(state.Events, snapshot)
	}

	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	envelope := snapshotEnvelope{
		Version:  snapshotFormatVer,
		Checksum: digest(stateBytes),
		State:    stateBytes,
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (t *SetEventTransmitter) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotFormatVer || len(envelope.State) == 0 || digest(envelope.State) != envelope.Checksum {
		return ErrBadSnapshot
	}

	var state snapshotState
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return ErrBadSnapshot
	}

	keys, subscriptions, events, finished, nextSequence, err := buildRestoredState(state)
	if err != nil {
		return ErrBadSnapshot
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	pending := pendingForEvents(events)
	if pending > t.maxInFlightLocked() {
		return ErrBadSnapshot
	}

	t.keys = keys
	t.subscriptions = subscriptions
	t.events = events
	t.finished = finished
	t.nextSequence = nextSequence
	t.epoch++
	t.evictFinishedLocked()
	return nil
}

func (t *SetEventTransmitter) deliver(receiverID, endpoint, token string) (int, error) {
	if t.sender == nil {
		return 0, errNilSender
	}
	return t.sender.Deliver(receiverID, endpoint, token)
}

func (t *SetEventTransmitter) performDeliveryLocked(delivery *deliveryRecord) {
	delivery.attempts++
	code, err := t.deliver(delivery.receiverID, delivery.endpoint, delivery.token)
	t.applyDeliveryResultLocked(delivery, code, err)
}

func (t *SetEventTransmitter) applyDeliveryResultLocked(delivery *deliveryRecord, code int, deliverErr error) {
	delivery.lastCode = code
	if code == 410 {
		delivery.status = StatusGone
		delivery.nextIn = 0
		return
	}
	if deliverErr != nil || code < 200 || code >= 300 {
		if delivery.attempts >= delivery.maxAttempts {
			delivery.status = StatusFailed
			delivery.nextIn = 0
			return
		}
		delivery.status = StatusPending
		delivery.nextIn = backoffSeconds(delivery.attempts)
		return
	}
	delivery.status = StatusDelivered
	delivery.nextIn = 0
}

func (t *SetEventTransmitter) matchingSubscribersLocked(evt SecurityEvent) []ReceiverSpec {
	tenantSubscriptions := t.subscriptions[evt.TenantID]
	if len(tenantSubscriptions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(tenantSubscriptions))
	for id, spec := range tenantSubscriptions {
		if subscriptionMatches(spec, evt) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	matches := make([]ReceiverSpec, 0, len(ids))
	for _, id := range ids {
		matches = append(matches, copyReceiverSpec(tenantSubscriptions[id]))
	}
	return matches
}

func (t *SetEventTransmitter) pendingLocked() int {
	return pendingForEvents(t.events)
}

func (t *SetEventTransmitter) maxInFlightLocked() int {
	if t.limits.MaxInFlight < 0 {
		return 0
	}
	return t.limits.MaxInFlight
}

func (t *SetEventTransmitter) maxEventsLocked() int {
	if t.limits.MaxEvents < 0 {
		return 0
	}
	return t.limits.MaxEvents
}

func (t *SetEventTransmitter) finishIfDoneLocked(record *eventRecord) {
	if record.finished || !eventFinished(record) {
		return
	}
	record.finished = true
	t.finished = append(t.finished, record.id)
	t.evictFinishedLocked()
}

func (t *SetEventTransmitter) evictFinishedLocked() {
	maxEvents := t.maxEventsLocked()
	for len(t.finished) > maxEvents {
		oldestIndex := 0
		oldestSequence := t.events[t.finished[0]].sequence
		for i := 1; i < len(t.finished); i++ {
			sequence := t.events[t.finished[i]].sequence
			if sequence < oldestSequence {
				oldestIndex = i
				oldestSequence = sequence
			}
		}
		oldest := t.finished[oldestIndex]
		t.finished = append(t.finished[:oldestIndex], t.finished[oldestIndex+1:]...)
		delete(t.events, oldest)
	}
}

func (t *SetEventTransmitter) newEventIDLocked() string {
	for {
		id := newOpaqueID("evt-")
		if _, exists := t.events[id]; !exists {
			return id
		}
	}
}

func pendingForEvents(events map[string]*eventRecord) int {
	pending := 0
	for _, record := range events {
		for _, delivery := range record.deliveries {
			if delivery.status == StatusPending {
				pending++
			}
		}
	}
	return pending
}

func eventFinished(record *eventRecord) bool {
	for _, delivery := range record.deliveries {
		if delivery.status == StatusPending {
			return false
		}
	}
	return true
}

func findDelivery(record *eventRecord, receiverID string) *deliveryRecord {
	for _, delivery := range record.deliveries {
		if delivery.receiverID == receiverID {
			return delivery
		}
	}
	return nil
}

func backoffSeconds(attempts int) int64 {
	if attempts <= 1 {
		return 1
	}
	shift := attempts - 1
	if shift >= 9 {
		return backoffCapSeconds
	}
	value := int64(1) << uint(shift)
	if value > backoffCapSeconds {
		return backoffCapSeconds
	}
	return value
}

func validateEvent(evt SecurityEvent) error {
	if strings.TrimSpace(evt.TenantID) == "" {
		return ErrBlankTenant
	}
	if !isKnownEventType(evt.Type) {
		return ErrUnknownEventType
	}
	if !hasNonBlankString(evt.Scope) {
		return ErrEmptySubject
	}
	return nil
}

func isKnownEventType(eventType string) bool {
	switch eventType {
	case EventSessionRevoked, EventAccountDisabled, EventCredentialChanged:
		return true
	default:
		return false
	}
}

func hasNonBlankString(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func subscriptionMatches(spec ReceiverSpec, evt SecurityEvent) bool {
	typeMatches := false
	for _, eventType := range spec.EventTypes {
		if eventType == evt.Type {
			typeMatches = true
			break
		}
	}
	if !typeMatches {
		return false
	}
	if len(spec.Subjects) == 0 {
		return true
	}

	eventSubjects := make([]string, 0, len(evt.Scope)+1)
	eventSubjects = append(eventSubjects, evt.Subject)
	eventSubjects = append(eventSubjects, evt.Scope...)
	for _, allowed := range spec.Subjects {
		for _, subject := range eventSubjects {
			if strings.TrimSpace(allowed) == strings.TrimSpace(subject) {
				return true
			}
		}
	}
	return false
}

func copyReceiverSpec(spec ReceiverSpec) ReceiverSpec {
	spec.EventTypes = copyStrings(spec.EventTypes)
	spec.Subjects = copyStrings(spec.Subjects)
	return spec
}

func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	copied := make([]string, len(values))
	copy(copied, values)
	return copied
}

type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type tokenClaims struct {
	Iss    string   `json:"iss"`
	Aud    string   `json:"aud"`
	JTI    string   `json:"jti"`
	Iat    int64    `json:"iat"`
	Events []string `json:"events"`
}

func signEventToken(key []byte, tenantID, receiverID string, issuedAt int64, eventID, eventType string) (string, error) {
	headerBytes, err := json.Marshal(tokenHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(tokenClaims{
		Iss:    tenantID,
		Aud:    receiverID,
		JTI:    eventID + ":" + receiverID,
		Iat:    issuedAt,
		Events: []string{eventType},
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signingInput))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + signature, nil
}

func newOpaqueID(prefix string) string {
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err == nil {
		return prefix + base64.RawURLEncoding.EncodeToString(randomBytes)
	}
	sequence := atomic.AddUint64(&failureIDSequence, 1)
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(sequence, 10)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func buildRestoredState(state snapshotState) (map[string][]byte, map[string]map[string]ReceiverSpec, map[string]*eventRecord, []string, uint64, error) {
	keys := make(map[string][]byte, len(state.Keys))
	for tenantID, key := range state.Keys {
		if strings.TrimSpace(tenantID) == "" {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		keys[tenantID] = append([]byte(nil), key...)
	}

	subscriptions := make(map[string]map[string]ReceiverSpec, len(state.Subscriptions))
	for tenantID, tenantSubscriptions := range state.Subscriptions {
		if strings.TrimSpace(tenantID) == "" {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		copied := make(map[string]ReceiverSpec, len(tenantSubscriptions))
		for receiverID, spec := range tenantSubscriptions {
			if receiverID != spec.ID || spec.TenantID != tenantID || strings.TrimSpace(receiverID) == "" || strings.TrimSpace(spec.Endpoint) == "" || !hasKnownEventType(spec.EventTypes) || spec.MaxAttempts < 1 {
				return nil, nil, nil, nil, 0, ErrBadSnapshot
			}
			copied[receiverID] = copyReceiverSpec(spec)
		}
		subscriptions[tenantID] = copied
	}

	events := make(map[string]*eventRecord, len(state.Events))
	terminal := make(map[string]bool, len(state.Events))
	terminalCount := 0
	var maxSequence uint64
	for _, snapshot := range state.Events {
		if snapshot.ID == "" || snapshot.Sequence == 0 || strings.TrimSpace(snapshot.TenantID) == "" || !isKnownEventType(snapshot.Type) || !hasNonBlankString(snapshot.Scope) {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if _, exists := events[snapshot.ID]; exists {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		if snapshot.Sequence > maxSequence {
			maxSequence = snapshot.Sequence
		}
		record := &eventRecord{
			id:       snapshot.ID,
			sequence: snapshot.Sequence,
			event: SecurityEvent{
				TenantID: snapshot.TenantID,
				Type:     snapshot.Type,
				Subject:  snapshot.Subject,
				Scope:    copyStrings(snapshot.Scope),
				IssuedAt: snapshot.IssuedAt,
			},
		}
		for _, snapshotDelivery := range snapshot.Deliveries {
			if strings.TrimSpace(snapshotDelivery.ReceiverID) == "" || strings.TrimSpace(snapshotDelivery.Endpoint) == "" || snapshotDelivery.Token == "" || snapshotDelivery.MaxAttempts < 1 || !validDeliverySnapshot(snapshotDelivery) {
				return nil, nil, nil, nil, 0, ErrBadSnapshot
			}
			if findDelivery(record, snapshotDelivery.ReceiverID) != nil {
				return nil, nil, nil, nil, 0, ErrBadSnapshot
			}
			record.deliveries = append(record.deliveries, &deliveryRecord{
				receiverID:  snapshotDelivery.ReceiverID,
				endpoint:    snapshotDelivery.Endpoint,
				token:       snapshotDelivery.Token,
				maxAttempts: snapshotDelivery.MaxAttempts,
				status:      snapshotDelivery.Status,
				attempts:    snapshotDelivery.Attempts,
				lastCode:    snapshotDelivery.LastCode,
				nextIn:      snapshotDelivery.NextIn,
			})
		}
		record.finished = eventFinished(record)
		terminal[snapshot.ID] = record.finished
		if record.finished {
			terminalCount++
		}
		events[snapshot.ID] = record
	}

	finished := make([]string, 0, len(state.Finished))
	seenFinished := make(map[string]bool, len(state.Finished))
	for _, eventID := range state.Finished {
		if seenFinished[eventID] || !terminal[eventID] {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
		seenFinished[eventID] = true
		finished = append(finished, eventID)
	}
	if len(finished) != terminalCount {
		return nil, nil, nil, nil, 0, ErrBadSnapshot
	}
	for eventID, isFinished := range terminal {
		if isFinished && !seenFinished[eventID] {
			return nil, nil, nil, nil, 0, ErrBadSnapshot
		}
	}

	if state.NextSequence < maxSequence {
		return nil, nil, nil, nil, 0, ErrBadSnapshot
	}
	return keys, subscriptions, events, finished, state.NextSequence, nil
}

func hasKnownEventType(eventTypes []string) bool {
	for _, eventType := range eventTypes {
		if !isKnownEventType(strings.TrimSpace(eventType)) {
			return false
		}
	}
	return len(eventTypes) > 0
}

func validDeliverySnapshot(delivery snapshotDelivery) bool {
	switch delivery.Status {
	case StatusDelivered:
		if delivery.Attempts < 1 || delivery.NextIn != 0 {
			return false
		}
	case StatusFailed:
		if delivery.Attempts < delivery.MaxAttempts || delivery.NextIn != 0 {
			return false
		}
	case StatusGone:
		if delivery.Attempts < 1 || delivery.NextIn != 0 {
			return false
		}
	case StatusPending:
		if delivery.Attempts < 1 || delivery.Attempts >= delivery.MaxAttempts || delivery.NextIn != backoffSeconds(delivery.Attempts) {
			return false
		}
	default:
		return false
	}
	return true
}
