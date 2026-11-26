// Package task brokers smart card sessions per reader, hands channels of one
// card out to the requesting applications and keeps the PIN attempt counter and
// lock state of every card.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// AID is an application identifier on the card.
type AID string

// Session is one open application channel.
type Session struct {
	ID      string
	Reader  string
	App     AID
	CardID  string
	Channel int
	Seq     int64
}

// PINState is the PIN bookkeeping kept per card.
type PINState struct {
	CardID    string
	Remaining int
	Locked    bool
	Seq       int64
}

// ReaderView is the state of one reader slot.
type ReaderView struct {
	Reader   string
	Present  bool
	CardID   string
	Sessions int
	Channels int
}

// Config bounds the sessions, the channels and the PIN retries.
type Config struct {
	MaxSessionsPerReader int
	MaxChannelsPerCard   int
	PINRetries           int
	MaxReaders           int
}

// Errors returned by the broker.
var (
	ErrBadCard        = errors.New("card id is required")
	ErrBadPIN         = errors.New("pin length is outside the allowed range")
	ErrBadApp         = errors.New("application id is required")
	ErrBadReader      = errors.New("reader name is required")
	ErrUnknownCard    = errors.New("unknown card")
	ErrReaderExists   = errors.New("reader already holds a card")
	ErrReaderLimit    = errors.New("reader limit reached")
	ErrUnknownReader  = errors.New("unknown reader")
	ErrNoCard         = errors.New("no card present on the reader")
	ErrSessionLimit   = errors.New("session limit reached for the reader")
	ErrChannelLimit   = errors.New("channel limit reached for the card")
	ErrUnknownSession = errors.New("unknown session")
	ErrSessionGone    = errors.New("session no longer valid")
	ErrBadAPDU        = errors.New("apdu is too short")
	ErrPINLocked      = errors.New("pin is locked")
	ErrIndeterminate  = errors.New("apdu outcome is indeterminate")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

const (
	minPINLength = 4
	maxPINLength = 12
)

// cardRecord is the per card state: reference PIN, attempt counter, lock flag
// and the channel numbers currently handed out.
type cardRecord struct {
	id        string
	pin       []byte
	remaining int
	locked    bool
	seq       int64
	channels  map[int]bool
}

// readerSlot is one physical reader; the generation changes on every plug and
// unplug so an in-flight APDU can notice a removal.
type readerSlot struct {
	name       string
	cardID     string
	generation int64
}

// sessionRecord is one open channel of one card on one reader.
type sessionRecord struct {
	id            string
	reader        string
	cardID        string
	app           AID
	channel       int
	seq           int64
	gone          bool
	indeterminate bool
}

// SmartcardSessionBroker owns the card table, the reader slots and the session
// table. The single mutex serialises channel allocation and PIN bookkeeping so
// counter decrements and lock commits are atomic.
type SmartcardSessionBroker struct {
	cfg            Config
	mu             sync.Mutex
	cards          map[string]*cardRecord
	readers        map[string]*readerSlot
	sessions       map[string]*sessionRecord
	nextSessionSeq int64
}

// NewSmartcardSessionBroker builds a broker with the given caps.
func NewSmartcardSessionBroker(cfg Config) *SmartcardSessionBroker {
	return &SmartcardSessionBroker{
		cfg:            cfg,
		cards:          make(map[string]*cardRecord),
		readers:        make(map[string]*readerSlot),
		sessions:       make(map[string]*sessionRecord),
		nextSessionSeq: 1,
	}
}

func validPINLength(pin []byte) bool {
	return len(pin) >= minPINLength && len(pin) <= maxPINLength
}

// Provision registers a card with its reference PIN. Registering the same card
// again resets the remaining attempts and clears the lock.
func (b *SmartcardSessionBroker) Provision(cardID string, pin []byte) error {
	if strings.TrimSpace(cardID) == "" {
		return ErrBadCard
	}
	if !validPINLength(pin) {
		return ErrBadPIN
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if card, ok := b.cards[cardID]; ok {
		card.pin = append([]byte(nil), pin...)
		card.remaining = b.cfg.PINRetries
		card.locked = false
		card.seq++
		return nil
	}
	b.cards[cardID] = &cardRecord{
		id:        cardID,
		pin:       append([]byte(nil), pin...),
		remaining: b.cfg.PINRetries,
		seq:       1,
		channels:  make(map[int]bool),
	}
	return nil
}

// Plug inserts a card into a reader slot.
func (b *SmartcardSessionBroker) Plug(reader string, cardID string) error {
	if strings.TrimSpace(reader) == "" {
		return ErrBadReader
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cards[cardID]; !ok {
		return ErrUnknownCard
	}
	slot, ok := b.readers[reader]
	if ok && slot.cardID != "" {
		return ErrReaderExists
	}
	if !ok {
		if len(b.readers) >= b.cfg.MaxReaders {
			return ErrReaderLimit
		}
		slot = &readerSlot{name: reader}
		b.readers[reader] = slot
	}
	slot.cardID = cardID
	slot.generation++
	return nil
}

// Unplug removes the card of a reader and drops every session on it. The slot
// itself stays behind as an empty reader.
func (b *SmartcardSessionBroker) Unplug(reader string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	slot, ok := b.readers[reader]
	if !ok {
		return ErrUnknownReader
	}
	if slot.cardID != "" {
		card := b.cards[slot.cardID]
		for _, sess := range b.sessions {
			if sess.reader == reader && !sess.gone {
				sess.gone = true
				if card != nil {
					delete(card.channels, sess.channel)
				}
			}
		}
	}
	slot.cardID = ""
	slot.generation++
	return nil
}

// Open allocates one application channel on the card of a reader, taking the
// lowest free channel number of the card.
func (b *SmartcardSessionBroker) Open(reader string, app AID) (Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	slot, ok := b.readers[reader]
	if !ok {
		return Session{}, ErrUnknownReader
	}
	if slot.cardID == "" {
		return Session{}, ErrNoCard
	}
	if strings.TrimSpace(string(app)) == "" {
		return Session{}, ErrBadApp
	}
	onReader := 0
	for _, sess := range b.sessions {
		if sess.reader == reader && !sess.gone {
			onReader++
		}
	}
	if onReader >= b.cfg.MaxSessionsPerReader {
		return Session{}, ErrSessionLimit
	}
	card := b.cards[slot.cardID]
	if len(card.channels) >= b.cfg.MaxChannelsPerCard {
		return Session{}, ErrChannelLimit
	}
	channel := 1
	for card.channels[channel] {
		channel++
	}
	seq := b.nextSessionSeq
	b.nextSessionSeq++
	sess := &sessionRecord{
		id:      fmt.Sprintf("sess-%d", seq),
		reader:  reader,
		cardID:  card.id,
		app:     app,
		channel: channel,
		seq:     seq,
	}
	b.sessions[sess.id] = sess
	card.channels[channel] = true
	return Session{
		ID:      sess.id,
		Reader:  sess.reader,
		App:     sess.app,
		CardID:  sess.cardID,
		Channel: sess.channel,
		Seq:     sess.seq,
	}, nil
}

// Transmit sends one APDU over a session and echoes it back with a 9000 status
// word. If the card is removed while the APDU is on the wire the outcome is
// unknown and ErrIndeterminate is returned.
func (b *SmartcardSessionBroker) Transmit(sessionID string, apdu []byte) ([]byte, error) {
	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		b.mu.Unlock()
		return nil, ErrUnknownSession
	}
	if sess.gone {
		b.mu.Unlock()
		return nil, ErrSessionGone
	}
	if len(apdu) < 4 {
		b.mu.Unlock()
		return nil, ErrBadAPDU
	}
	slot := b.readers[sess.reader]
	var generation int64
	if slot != nil {
		generation = slot.generation
	}
	b.mu.Unlock()

	// The APDU is on the wire here; the card may be removed concurrently.

	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.readers[sess.reader]
	if sess.gone || cur == nil || cur.generation != generation {
		sess.indeterminate = true
		return nil, ErrIndeterminate
	}
	resp := make([]byte, 0, len(apdu)+2)
	resp = append(resp, apdu...)
	resp = append(resp, 0x90, 0x00)
	return resp, nil
}

// VerifyPIN checks the PIN of the card behind a session. A malformed PIN is
// rejected without consuming an attempt, a wrong PIN consumes one attempt and
// locks the card at zero, a correct PIN resets the counter.
func (b *SmartcardSessionBroker) VerifyPIN(sessionID string, pin []byte) (PINState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		return PINState{}, ErrUnknownSession
	}
	if sess.gone {
		return PINState{}, ErrSessionGone
	}
	if !validPINLength(pin) {
		return PINState{}, ErrBadPIN
	}
	card := b.cards[sess.cardID]
	if card == nil {
		return PINState{}, ErrUnknownCard
	}
	if card.locked {
		return PINState{}, ErrPINLocked
	}
	if bytes.Equal(pin, card.pin) {
		card.remaining = b.cfg.PINRetries
		card.seq++
		return PINState{CardID: card.id, Remaining: card.remaining, Locked: card.locked, Seq: card.seq}, nil
	}
	card.remaining--
	if card.remaining <= 0 {
		card.remaining = 0
		card.locked = true
	}
	card.seq++
	return PINState{CardID: card.id, Remaining: card.remaining, Locked: card.locked, Seq: card.seq}, nil
}

// Close releases one session channel so the number can be reused.
func (b *SmartcardSessionBroker) Close(sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		return ErrUnknownSession
	}
	if !sess.gone {
		if card := b.cards[sess.cardID]; card != nil {
			delete(card.channels, sess.channel)
		}
	}
	delete(b.sessions, sessionID)
	return nil
}

// ReaderState reports one reader slot.
func (b *SmartcardSessionBroker) ReaderState(reader string) (ReaderView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	slot, ok := b.readers[reader]
	if !ok {
		return ReaderView{}, ErrUnknownReader
	}
	view := ReaderView{Reader: reader, Present: slot.cardID != "", CardID: slot.cardID}
	for _, sess := range b.sessions {
		if sess.reader == reader && !sess.gone {
			view.Sessions++
			view.Channels++
		}
	}
	return view, nil
}

// PINState reports the PIN bookkeeping of one card.
func (b *SmartcardSessionBroker) PINState(cardID string) (PINState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	card, ok := b.cards[cardID]
	if !ok {
		return PINState{}, ErrUnknownCard
	}
	return PINState{CardID: card.id, Remaining: card.remaining, Locked: card.locked, Seq: card.seq}, nil
}

type snapshotCard struct {
	CardID    string `json:"card_id"`
	PIN       []byte `json:"pin"`
	Remaining int    `json:"remaining"`
	Locked    bool   `json:"locked"`
	Seq       int64  `json:"seq"`
}

type snapshotReader struct {
	Name       string `json:"name"`
	CardID     string `json:"card_id"`
	Generation int64  `json:"generation"`
}

type snapshot struct {
	Version        int              `json:"version"`
	Cards          []snapshotCard   `json:"cards"`
	Readers        []snapshotReader `json:"readers"`
	NextSessionSeq int64            `json:"next_session_seq"`
}

const snapshotVersion = 1

// Snapshot serialises the cards, the readers and the counters. Sessions are
// runtime state and are never part of a snapshot.
func (b *SmartcardSessionBroker) Snapshot() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	snap := snapshot{
		Version:        snapshotVersion,
		Cards:          make([]snapshotCard, 0, len(b.cards)),
		Readers:        make([]snapshotReader, 0, len(b.readers)),
		NextSessionSeq: b.nextSessionSeq,
	}
	for _, card := range b.cards {
		snap.Cards = append(snap.Cards, snapshotCard{
			CardID:    card.id,
			PIN:       append([]byte(nil), card.pin...),
			Remaining: card.remaining,
			Locked:    card.locked,
			Seq:       card.seq,
		})
	}
	for _, slot := range b.readers {
		snap.Readers = append(snap.Readers, snapshotReader{
			Name:       slot.name,
			CardID:     slot.cardID,
			Generation: slot.generation,
		})
	}
	return json.Marshal(snap)
}

// Restore replaces the whole state with a snapshot, all or nothing: the new
// state is built and validated first and only swapped in when every step
// succeeds. Sessions are never restored; the sequence counter only moves
// forward.
func (b *SmartcardSessionBroker) Restore(data []byte) error {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}
	if snap.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if snap.NextSessionSeq < 1 {
		return ErrBadSnapshot
	}
	cards := make(map[string]*cardRecord, len(snap.Cards))
	for _, sc := range snap.Cards {
		if strings.TrimSpace(sc.CardID) == "" || !validPINLength(sc.PIN) {
			return ErrBadSnapshot
		}
		if sc.Remaining < 0 || sc.Remaining > b.cfg.PINRetries || sc.Seq < 1 {
			return ErrBadSnapshot
		}
		if _, dup := cards[sc.CardID]; dup {
			return ErrBadSnapshot
		}
		cards[sc.CardID] = &cardRecord{
			id:        sc.CardID,
			pin:       append([]byte(nil), sc.PIN...),
			remaining: sc.Remaining,
			locked:    sc.Locked,
			seq:       sc.Seq,
			channels:  make(map[int]bool),
		}
	}
	if len(snap.Readers) > b.cfg.MaxReaders {
		return ErrBadSnapshot
	}
	readers := make(map[string]*readerSlot, len(snap.Readers))
	for _, sr := range snap.Readers {
		if strings.TrimSpace(sr.Name) == "" {
			return ErrBadSnapshot
		}
		if _, dup := readers[sr.Name]; dup {
			return ErrBadSnapshot
		}
		if sr.CardID != "" {
			if _, ok := cards[sr.CardID]; !ok {
				return ErrBadSnapshot
			}
		}
		readers[sr.Name] = &readerSlot{
			name:       sr.Name,
			cardID:     sr.CardID,
			generation: sr.Generation,
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cards = cards
	b.readers = readers
	b.sessions = make(map[string]*sessionRecord)
	b.nextSessionSeq = snap.NextSessionSeq
	return nil
}
