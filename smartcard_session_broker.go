// Package task brokers smart card sessions per reader, hands channels of one
// card out to the requesting applications and keeps the PIN attempt counter and
// lock state of every card.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

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

// SmartcardSessionBroker owns the card table, the reader slots and the session
// table.
type SmartcardSessionBroker struct {
	cfg Config
}

// NewSmartcardSessionBroker builds a broker with the given caps.
func NewSmartcardSessionBroker(cfg Config) *SmartcardSessionBroker {
	return &SmartcardSessionBroker{cfg: cfg}
}

// Provision registers a card with its reference PIN.
func (b *SmartcardSessionBroker) Provision(cardID string, pin []byte) error {
	return errNotImplemented
}

// Plug inserts a card into a reader slot.
func (b *SmartcardSessionBroker) Plug(reader string, cardID string) error {
	return errNotImplemented
}

// Unplug removes the card of a reader and drops every session on it.
func (b *SmartcardSessionBroker) Unplug(reader string) error {
	return errNotImplemented
}

// Open allocates one application channel on the card of a reader.
func (b *SmartcardSessionBroker) Open(reader string, app AID) (Session, error) {
	return Session{}, errNotImplemented
}

// Transmit sends one APDU over a session.
func (b *SmartcardSessionBroker) Transmit(sessionID string, apdu []byte) ([]byte, error) {
	return nil, errNotImplemented
}

// VerifyPIN checks the PIN of the card behind a session.
func (b *SmartcardSessionBroker) VerifyPIN(sessionID string, pin []byte) (PINState, error) {
	return PINState{}, errNotImplemented
}

// Close releases one session channel.
func (b *SmartcardSessionBroker) Close(sessionID string) error {
	return errNotImplemented
}

// ReaderState reports one reader slot.
func (b *SmartcardSessionBroker) ReaderState(reader string) (ReaderView, error) {
	return ReaderView{}, errNotImplemented
}

// PINState reports the PIN bookkeeping of one card.
func (b *SmartcardSessionBroker) PINState(cardID string) (PINState, error) {
	return PINState{}, errNotImplemented
}

// Snapshot serialises the cards, the readers and the counters.
func (b *SmartcardSessionBroker) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (b *SmartcardSessionBroker) Restore(data []byte) error {
	return errNotImplemented
}
