// Package task normalises the framing of SMTP DATA payloads: only CRLF ends a
// line, a lone dot line terminates the body, leading dots are unstuffed and the
// declared size, the received bytes and the chunk count are reconciled.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

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

const (
	snapshotMagic   = "gsb-smtp-data-framing-guard"
	snapshotVersion = 1
	abortedReason   = "aborted"
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
	mu           sync.Mutex
	limits       Limits
	sessions     map[string]*SessionState
	current      *sessionBuffer
	lastBody     []byte
	commandInput []byte
	seq          int64
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
	terminated bool
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotData struct {
	Seq          int64            `json:"seq"`
	Sessions     []SessionState   `json:"sessions"`
	Current      *snapshotSession `json:"current,omitempty"`
	LastBody     []byte           `json:"last_body,omitempty"`
	CommandInput []byte           `json:"command_input,omitempty"`
}

type snapshotSession struct {
	ID         string `json:"id"`
	Declared   int64  `json:"declared"`
	Received   int64  `json:"received"`
	Chunks     int    `json:"chunks"`
	Lines      int    `json:"lines"`
	DotStuffed int    `json:"dot_stuffed"`
	Pending    []byte `json:"pending,omitempty"`
	Body       []byte `json:"body,omitempty"`
	Terminated bool   `json:"terminated"`
}

// NewSmtpDataFramingGuard builds a guard with the given limits.
func NewSmtpDataFramingGuard(limits Limits) *SmtpDataFramingGuard {
	return &SmtpDataFramingGuard{
		limits:   limits,
		sessions: make(map[string]*SessionState),
	}
}

// Begin opens one DATA transaction on the connection.
func (g *SmtpDataFramingGuard) Begin(sessionID string, declared int64) (Transaction, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Transaction{}, ErrBadSession
	}
	if declared <= 0 {
		return Transaction{}, ErrBadDeclared
	}
	if g.limits.MaxBytes > 0 && declared > g.limits.MaxBytes {
		return Transaction{}, ErrSizeLimit
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.sessions == nil {
		g.sessions = make(map[string]*SessionState)
	}
	if g.current != nil {
		return Transaction{}, ErrSessionBusy
	}

	g.seq++
	state := &SessionState{
		ID:       sessionID,
		Declared: declared,
		State:    SessionRunning,
	}
	g.sessions[sessionID] = state
	g.current = &sessionBuffer{
		id:       sessionID,
		declared: declared,
	}
	g.lastBody = nil

	return Transaction{
		ID:        fmt.Sprintf("tx-%d", g.seq),
		SessionID: sessionID,
		Declared:  declared,
		Opened:    g.seq,
	}, nil
}

// Feed appends one chunk to the running transaction.
func (g *SmtpDataFramingGuard) Feed(chunk []byte) (Progress, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	current := g.current
	if current == nil {
		return Progress{}, ErrNoSession
	}
	if len(chunk) == 0 {
		return current.progress(), nil
	}

	current.chunks++

	// Once the dot terminator has been seen, later bytes belong to the
	// connection's command phase rather than to this DATA transaction.
	if current.terminated {
		g.commandInput = append(g.commandInput, chunk...)
		g.syncState(current)
		return current.progress(), nil
	}

	current.pending = append(current.pending, chunk...)
	residual, err := processPending(current, g.limits.MaxLineLength)
	if err != nil {
		g.invalidate(current, err)
		return Progress{}, err
	}

	dataBytes := int64(len(chunk) - len(residual))
	current.received += dataBytes
	if len(residual) > 0 {
		g.commandInput = append(g.commandInput, residual...)
	}

	if current.received > current.declared {
		err := ErrSizeOverflow
		g.invalidate(current, err)
		return Progress{}, err
	}

	g.syncState(current)
	return current.progress(), nil
}

// Finish closes the transaction after reconciling the accounting.
func (g *SmtpDataFramingGuard) Finish(sessionID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	current, err := g.active(sessionID)
	if err != nil {
		return err
	}

	if current.received != current.declared {
		g.syncState(current)
		if state := g.sessions[sessionID]; state != nil {
			state.State = SessionClosed
			state.Reason = ErrSizeMismatch.Error()
		}
		g.current = nil
		g.lastBody = nil
		return ErrSizeMismatch
	}

	if err := current.flushPending(g.limits.MaxLineLength); err != nil {
		g.invalidate(current, err)
		return err
	}

	g.syncState(current)
	if state := g.sessions[sessionID]; state != nil {
		state.State = SessionClosed
		state.Reason = ""
	}
	g.lastBody = bytes.Clone(current.body)
	g.current = nil
	return nil
}

// Abort drops the running transaction and returns the connection to command
// state.
func (g *SmtpDataFramingGuard) Abort(sessionID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	current, err := g.active(sessionID)
	if err != nil {
		return err
	}

	g.syncState(current)
	if state := g.sessions[sessionID]; state != nil {
		state.State = SessionAborted
		state.Reason = abortedReason
	}
	g.current = nil
	g.lastBody = nil
	return nil
}

// Body returns the decoded body of the active or last finished transaction.
func (g *SmtpDataFramingGuard) Body() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return bytes.Clone(g.bodyLocked())
}

// BodyLines returns the decoded body split into lines without CRLF.
func (g *SmtpDataFramingGuard) BodyLines() []string {
	g.mu.Lock()
	body := bytes.Clone(g.bodyLocked())
	g.mu.Unlock()

	if len(body) == 0 {
		return []string{}
	}
	text := string(body)
	if strings.HasSuffix(text, "\r\n") {
		text = text[:len(text)-2]
	}
	return strings.Split(text, "\r\n")
}

// State reads one session record.
func (g *SmtpDataFramingGuard) State(sessionID string) (SessionState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok := g.sessions[sessionID]
	if !ok || state == nil {
		return SessionState{}, false
	}
	return *state, true
}

// Sessions returns every session record, ordered by session id.
func (g *SmtpDataFramingGuard) Sessions() []SessionState {
	g.mu.Lock()
	defer g.mu.Unlock()

	ids := make([]string, 0, len(g.sessions))
	for id := range g.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]SessionState, 0, len(ids))
	for _, id := range ids {
		if state := g.sessions[id]; state != nil {
			out = append(out, *state)
		}
	}
	return out
}

// DrainCommandInput returns and clears bytes left after a DATA terminator.
func (g *SmtpDataFramingGuard) DrainCommandInput() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := bytes.Clone(g.commandInput)
	g.commandInput = nil
	return out
}

// Snapshot serialises the running transaction and the session history.
func (g *SmtpDataFramingGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.current != nil {
		g.syncState(g.current)
	}

	ids := make([]string, 0, len(g.sessions))
	for id := range g.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	data := snapshotData{
		Seq:          g.seq,
		Sessions:     make([]SessionState, 0, len(ids)),
		LastBody:     bytes.Clone(g.lastBody),
		CommandInput: bytes.Clone(g.commandInput),
	}
	for _, id := range ids {
		if state := g.sessions[id]; state != nil {
			data.Sessions = append(data.Sessions, *state)
		}
	}
	if g.current != nil {
		data.Current = newSnapshotSession(g.current)
	}

	stateJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(stateJSON)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(sum[:]),
		State:    stateJSON,
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SmtpDataFramingGuard) Restore(payload []byte) error {
	next, err := decodeSnapshot(payload)
	if err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.sessions = next.sessions
	g.current = next.current
	g.lastBody = next.lastBody
	g.commandInput = next.commandInput
	g.seq = next.seq
	return nil
}

type restoredState struct {
	sessions     map[string]*SessionState
	current      *sessionBuffer
	lastBody     []byte
	commandInput []byte
	seq          int64
}

func decodeSnapshot(payload []byte) (restoredState, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return restoredState{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion {
		return restoredState{}, fmt.Errorf("%w: unsupported snapshot", ErrBadSnapshot)
	}

	sum := sha256.Sum256(envelope.State)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return restoredState{}, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var data snapshotData
	if err := json.Unmarshal(envelope.State, &data); err != nil {
		return restoredState{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(data); err != nil {
		return restoredState{}, err
	}

	result := restoredState{
		sessions:     make(map[string]*SessionState, len(data.Sessions)),
		seq:          data.Seq,
		lastBody:     bytes.Clone(data.LastBody),
		commandInput: bytes.Clone(data.CommandInput),
	}
	for i := range data.Sessions {
		state := data.Sessions[i]
		result.sessions[state.ID] = &state
	}
	if data.Current != nil {
		result.current = &sessionBuffer{
			id:         data.Current.ID,
			declared:   data.Current.Declared,
			received:   data.Current.Received,
			chunks:     data.Current.Chunks,
			lines:      data.Current.Lines,
			dotStuffed: data.Current.DotStuffed,
			pending:    bytes.Clone(data.Current.Pending),
			body:       bytes.Clone(data.Current.Body),
			terminated: data.Current.Terminated,
		}
	}
	return result, nil
}

func validateSnapshot(data snapshotData) error {
	if data.Seq < 0 {
		return fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}

	seen := make(map[string]struct{}, len(data.Sessions))
	running := 0
	for _, state := range data.Sessions {
		if strings.TrimSpace(state.ID) == "" {
			return fmt.Errorf("%w: empty session id", ErrBadSnapshot)
		}
		if _, ok := seen[state.ID]; ok {
			return fmt.Errorf("%w: duplicate session %q", ErrBadSnapshot, state.ID)
		}
		seen[state.ID] = struct{}{}
		if state.Declared <= 0 || state.Received < 0 || state.Lines < 0 || state.Chunks < 0 {
			return fmt.Errorf("%w: invalid accounting for %q", ErrBadSnapshot, state.ID)
		}
		switch state.State {
		case SessionRunning:
			running++
		case SessionClosed, SessionAborted:
		default:
			return fmt.Errorf("%w: invalid state %q", ErrBadSnapshot, state.State)
		}
	}
	if running > 1 {
		return fmt.Errorf("%w: more than one running session", ErrBadSnapshot)
	}

	current := data.Current
	if current == nil {
		if running != 0 {
			return fmt.Errorf("%w: running session is missing its buffer", ErrBadSnapshot)
		}
		return nil
	}

	if strings.TrimSpace(current.ID) == "" || current.Declared <= 0 {
		return fmt.Errorf("%w: invalid current session", ErrBadSnapshot)
	}
	if current.Received < 0 || current.Chunks < 0 || current.Lines < 0 || current.DotStuffed < 0 {
		return fmt.Errorf("%w: negative current accounting", ErrBadSnapshot)
	}
	if current.Received > current.Declared {
		return fmt.Errorf("%w: current session overflows declared size", ErrBadSnapshot)
	}
	if bytes.IndexByte(current.Pending, '\n') >= 0 {
		return fmt.Errorf("%w: complete line remains pending", ErrBadSnapshot)
	}
	if cr := bytes.IndexByte(current.Pending, '\r'); cr >= 0 && cr != len(current.Pending)-1 {
		return fmt.Errorf("%w: invalid carriage return in pending line", ErrBadSnapshot)
	}
	if current.Terminated && len(current.Pending) != 0 {
		return fmt.Errorf("%w: terminated session retains pending bytes", ErrBadSnapshot)
	}

	state, ok := findSession(data.Sessions, current.ID)
	if !ok || state.State != SessionRunning {
		return fmt.Errorf("%w: current session has no running record", ErrBadSnapshot)
	}
	if state.Declared != current.Declared || state.Received != current.Received ||
		state.Lines != current.Lines || state.Chunks != current.Chunks {
		return fmt.Errorf("%w: current accounting does not match its record", ErrBadSnapshot)
	}
	return nil
}

func findSession(states []SessionState, id string) (SessionState, bool) {
	for _, state := range states {
		if state.ID == id {
			return state, true
		}
	}
	return SessionState{}, false
}

func newSnapshotSession(current *sessionBuffer) *snapshotSession {
	return &snapshotSession{
		ID:         current.id,
		Declared:   current.declared,
		Received:   current.received,
		Chunks:     current.chunks,
		Lines:      current.lines,
		DotStuffed: current.dotStuffed,
		Pending:    bytes.Clone(current.pending),
		Body:       bytes.Clone(current.body),
		Terminated: current.terminated,
	}
}

func (g *SmtpDataFramingGuard) active(sessionID string) (*sessionBuffer, error) {
	if g.current == nil || g.current.id != sessionID {
		return nil, ErrSessionNotFound
	}
	return g.current, nil
}

func (g *SmtpDataFramingGuard) bodyLocked() []byte {
	if g.current != nil {
		return g.current.body
	}
	return g.lastBody
}

func (g *SmtpDataFramingGuard) syncState(current *sessionBuffer) {
	state, ok := g.sessions[current.id]
	if !ok || state == nil {
		return
	}
	state.Received = current.received
	state.Lines = current.lines
	state.Chunks = current.chunks
}

func (g *SmtpDataFramingGuard) invalidate(current *sessionBuffer, reason error) {
	g.syncState(current)
	if state, ok := g.sessions[current.id]; ok && state != nil {
		state.State = SessionAborted
		state.Reason = reason.Error()
	}
	if g.current == current {
		g.current = nil
	}
	g.lastBody = nil
}

func (current *sessionBuffer) progress() Progress {
	return Progress{
		Received:   current.received,
		Remaining:  current.declared - current.received,
		Lines:      current.lines,
		Chunks:     current.chunks,
		DotStuffed: current.dotStuffed,
		InData:     !current.terminated,
	}
}

func (current *sessionBuffer) flushPending(maxLineLength int) error {
	if len(current.pending) == 0 {
		return nil
	}

	pending := current.pending
	if bytes.IndexByte(pending, '\n') >= 0 {
		return ErrBareLF
	}
	if cr := bytes.IndexByte(pending, '\r'); cr >= 0 {
		return ErrBareCR
	}
	if maxLineLength > 0 && len(pending) > maxLineLength {
		return ErrLineTooLong
	}
	if bytes.Equal(pending, []byte(".")) {
		current.terminated = true
		current.pending = nil
		return nil
	}

	decoded, stuffed := unstuffLeadingDots(pending)
	current.body = append(current.body, decoded...)
	current.lines++
	if stuffed {
		current.dotStuffed++
	}
	current.pending = nil
	return nil
}

func processPending(current *sessionBuffer, maxLineLength int) ([]byte, error) {
	for len(current.pending) > 0 {
		lf := bytes.IndexByte(current.pending, '\n')
		cr := bytes.IndexByte(current.pending, '\r')

		if cr >= 0 && (lf < 0 || cr < lf) {
			if cr == len(current.pending)-1 {
				if err := checkPendingLineLength(current.pending, maxLineLength); err != nil {
					return nil, err
				}
				break
			}
			if current.pending[cr+1] != '\n' {
				return nil, ErrBareCR
			}
		}

		if lf < 0 {
			if err := checkPendingLineLength(current.pending, maxLineLength); err != nil {
				return nil, err
			}
			break
		}

		if lf == 0 || current.pending[lf-1] != '\r' {
			return nil, ErrBareLF
		}

		line := current.pending[:lf-1]
		if maxLineLength > 0 && len(line) > maxLineLength {
			return nil, ErrLineTooLong
		}

		if bytes.Equal(line, []byte(".")) {
			residual := bytes.Clone(current.pending[lf+1:])
			current.pending = nil
			current.terminated = true
			return residual, nil
		}

		decoded, stuffed := unstuffLeadingDots(line)
		current.body = append(current.body, decoded...)
		current.body = append(current.body, '\r', '\n')
		current.lines++
		if stuffed {
			current.dotStuffed++
		}
		current.pending = current.pending[lf+1:]
	}
	return nil, nil
}

func checkPendingLineLength(pending []byte, maxLineLength int) error {
	if maxLineLength <= 0 {
		return nil
	}
	rawLength := len(pending)
	if rawLength > 0 && pending[rawLength-1] == '\r' {
		rawLength--
	}
	if rawLength > maxLineLength {
		return ErrLineTooLong
	}
	return nil
}

func unstuffLeadingDots(line []byte) ([]byte, bool) {
	if len(line) < 2 || line[0] != '.' || line[1] != '.' {
		return line, false
	}
	decoded := make([]byte, 0, len(line)-1)
	decoded = append(decoded, '.')
	decoded = append(decoded, line[2:]...)
	return decoded, true
}
