// Package task normalises the framing of SMTP DATA payloads: only CRLF ends a
// line, a lone dot line terminates the body, leading dots are unstuffed and the
// declared size, the received bytes and the chunk count are reconciled.
package task

import (
	"errors"
)

// errNotImplemented is returned by behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Session states kept by the guard.
const (
	SessionRunning = "running"
	SessionClosed  = "closed"
	SessionAborted = "aborted"
)

// Errors returned by the guard.
var (
	ErrBadSession      = errors.New("session id is required")
	ErrBadDeclared     = errors.New("declared size must be positive")
	ErrSessionBusy     = errors.New("another session already owns the connection")
	ErrNoSession       = errors.New("no session is open on this connection")
	ErrSessionNotFound = errors.New("session not found")
	ErrBareLF          = errors.New("line feed without carriage return")
	ErrBareCR          = errors.New("carriage return without line feed")
	ErrLineTooLong     = errors.New("line exceeds the configured limit")
	ErrSizeOverflow    = errors.New("payload exceeds the declared size")
	ErrSizeMismatch    = errors.New("received bytes do not match the declared size")
	ErrSizeLimit       = errors.New("declared size exceeds the configured limit")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Limits bounds one DATA transaction.
type Limits struct {
	MaxLineLength int
	MaxBytes      int64
}

// Transaction is the handle returned by Begin.
type Transaction struct {
	ID        string
	SessionID string
	Declared  int64
	Opened    int64
}

// SessionState is the accounting record of one session.
type SessionState struct {
	ID       string
	Declared int64
	Received int64
	Lines    int
	Chunks   int
	State    string
	Reason   string
}

// Progress reports the accounting after one chunk.
type Progress struct {
	Received   int64
	Remaining  int64
	Lines      int
	Chunks     int
	DotStuffed int
	InData     bool
}

// SmtpDataFramingGuard owns the open transaction and the session history.
type SmtpDataFramingGuard struct {
	limits   Limits
	sessions map[string]*SessionState
	current  *sessionBuffer
	seq      int64
}

// sessionBuffer holds the raw bytes that have no line ending yet plus the
// decoded body of the running transaction.
type sessionBuffer struct {
	id         string
	declared   int64
	received   int64
	chunks     int
	lines      int
	dotStuffed int
	pending    []byte
	body       []byte
}

// NewSmtpDataFramingGuard builds a guard with the given limits.
func NewSmtpDataFramingGuard(limits Limits) *SmtpDataFramingGuard {
	return &SmtpDataFramingGuard{limits: limits, sessions: map[string]*SessionState{}}
}

// Begin opens one DATA transaction on the connection.
func (g *SmtpDataFramingGuard) Begin(sessionID string, declared int64) (Transaction, error) {
	return Transaction{}, errNotImplemented
}

// Feed appends one chunk to the running transaction.
func (g *SmtpDataFramingGuard) Feed(chunk []byte) (Progress, error) {
	return Progress{}, errNotImplemented
}

// Finish closes the transaction after reconciling the accounting.
func (g *SmtpDataFramingGuard) Finish(sessionID string) error {
	return errNotImplemented
}

// Abort drops the running transaction and returns the connection to command
// state.
func (g *SmtpDataFramingGuard) Abort(sessionID string) error {
	return errNotImplemented
}

// Body returns the decoded body of the last finished transaction.
func (g *SmtpDataFramingGuard) Body() []byte {
	return nil
}

// BodyLines returns the decoded body split into lines without CRLF.
func (g *SmtpDataFramingGuard) BodyLines() []string {
	return nil
}

// State reads one session record.
func (g *SmtpDataFramingGuard) State(sessionID string) (SessionState, bool) {
	return SessionState{}, false
}

// Sessions returns every session record, ordered by session id.
func (g *SmtpDataFramingGuard) Sessions() []SessionState {
	return nil
}

// Snapshot serialises the running transaction and the session history.
func (g *SmtpDataFramingGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SmtpDataFramingGuard) Restore(data []byte) error {
	return errNotImplemented
}
