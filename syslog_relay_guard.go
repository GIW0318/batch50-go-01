// Package task relays node logs to a central collector using RFC 5424
// framing, peer verification and a bounded on-disk queue.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Framing modes supported by the relay.
const (
	FramingOctetCounted   = "octet_counted"
	FramingNonTransparent = "non_transparent"
)

// Errors surfaced by the relay.
var (
	ErrBadPolicy     = errors.New("relay policy is invalid")
	ErrBadMessage    = errors.New("message is invalid")
	ErrQueueFull     = errors.New("relay queue is full")
	ErrPeerUntrusted = errors.New("peer certificate chain is not trusted")
	ErrSNIMismatch   = errors.New("peer certificate does not match the server name")
	ErrBadSnapshot   = errors.New("snapshot payload invalid")
)

// Message is one log record handed to the relay.
type Message struct {
	Source      string
	Priority    int
	AppName     string
	MsgID       string
	Timestamp   time.Time
	Body        []byte
	OriginalLen int
}

// RelayPolicy is the operator configuration of the relay.
type RelayPolicy struct {
	Framing             string
	MaxBodyBytes        int
	QueueMaxMessages    int
	QueueMaxBytes       int
	HighPrioritySources []string
	HighPriorityReserve int
	AllowedCAs          []string
	ServerName          string
}

// PeerState is the result of the last certificate verification.
type PeerState struct {
	Verified   bool
	ServerName string
	CA         string
	Generation int
}

// CertInfo is one certificate presented by the collector.
type CertInfo struct {
	CA       string
	Subject  string
	DNSNames []string
	NotAfter time.Time
}

// SourceStatus is a per-source view used by operators to see backlog and
// rejected records. It is returned by SourceStats.
type SourceStatus struct {
	Source            string
	PendingMessages   int
	PendingBytes      int
	InflightMessages  int
	InflightBytes     int
	DroppedMessages   int
	DroppedBytes      int
	LastDroppedID     string
	LastDroppedReason string
}

type dropStat struct {
	Messages int       `json:"messages"`
	Bytes    int       `json:"bytes"`
	LastID   string    `json:"last_id,omitempty"`
	LastAt   time.Time `json:"last_at,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// SyslogRelayGuard owns the queue, the framing and the peer state.
type SyslogRelayGuard struct {
	mu sync.Mutex

	cfg         RelayPolicy
	highSources map[string]struct{}

	pending  []Message
	inflight []Message

	nextSeq    uint64
	peer       PeerState
	generation int

	dropped map[string]*dropStat
}

const (
	snapshotMagic   = "gsb/batch50-go-safety-b-04/syslog-relay-guard"
	snapshotVersion = 1

	// RFC 5424 permits at most six fractional-second digits in TIME-SECFRAC.
	rfc5424TimestampLayout = "2006-01-02T15:04:05.999999Z07:00"
)

type snapshotState struct {
	Magic      string               `json:"magic"`
	Version    int                  `json:"version"`
	NextSeq    uint64               `json:"next_seq"`
	Pending    []Message            `json:"pending"`
	Inflight   []Message            `json:"inflight"`
	Peer       PeerState            `json:"peer"`
	Generation int                  `json:"generation"`
	Dropped    map[string]*dropStat `json:"dropped,omitempty"`
}

// NewSyslogRelayGuard builds a relay with the given policy.
func NewSyslogRelayGuard(p RelayPolicy) (*SyslogRelayGuard, error) {
	cfg, err := validatePolicy(p)
	if err != nil {
		return nil, err
	}
	g := &SyslogRelayGuard{
		cfg:         cfg,
		highSources: makeHighSources(cfg.HighPrioritySources),
		nextSeq:     1,
		dropped:     make(map[string]*dropStat),
	}
	return g, nil
}

func validatePolicy(p RelayPolicy) (RelayPolicy, error) {
	if p.Framing != FramingOctetCounted && p.Framing != FramingNonTransparent {
		return RelayPolicy{}, ErrBadPolicy
	}
	if p.MaxBodyBytes <= 0 || p.QueueMaxMessages <= 0 || p.QueueMaxBytes <= 0 {
		return RelayPolicy{}, ErrBadPolicy
	}
	if p.HighPriorityReserve < 0 || p.HighPriorityReserve > p.QueueMaxMessages {
		return RelayPolicy{}, ErrBadPolicy
	}
	p.HighPrioritySources = append([]string(nil), p.HighPrioritySources...)
	p.AllowedCAs = append([]string(nil), p.AllowedCAs...)
	return p, nil
}

func makeHighSources(sources []string) map[string]struct{} {
	m := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		source = strings.TrimSpace(source)
		if source != "" {
			m[source] = struct{}{}
		}
	}
	return m
}

// Write normalises and queues one message.
func (g *SyslogRelayGuard) Write(msg Message) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	normalised, err := g.normalizeLocked(msg)
	if err != nil {
		return err
	}
	return g.enqueueLocked(normalised)
}

// Flush moves the pending queue to the in-flight batch. Flushing an already
// flushed batch is a no-op: in-flight records are only removed by Ack, so a
// reconnect cannot enqueue them again.
func (g *SyslogRelayGuard) Flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if len(g.pending) == 0 {
		return nil
	}
	g.inflight = append(g.inflight, g.pending...)
	g.pending = nil
	return nil
}

// Peer reports the last verified peer.
func (g *SyslogRelayGuard) Peer() PeerState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peer
}

// SetPolicy swaps the relay policy without dropping queued messages.
func (g *SyslogRelayGuard) SetPolicy(p RelayPolicy) error {
	cfg, err := validatePolicy(p)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg = cfg
	g.highSources = makeHighSources(cfg.HighPrioritySources)
	return nil
}

// Normalize validates one message and truncates an oversized body.
func (g *SyslogRelayGuard) Normalize(msg Message) (Message, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.normalizeLocked(msg)
}

func (g *SyslogRelayGuard) normalizeLocked(msg Message) (Message, error) {
	msg.Source = strings.TrimSpace(msg.Source)
	if msg.Source == "" || len(msg.Body) == 0 {
		return Message{}, ErrBadMessage
	}
	if msg.Priority < 0 || msg.Priority > 191 {
		return Message{}, ErrBadMessage
	}

	msg.AppName = strings.TrimSpace(msg.AppName)
	msg.MsgID = strings.TrimSpace(msg.MsgID)
	if msg.MsgID == "" {
		msg.MsgID = g.nextMessageID(msg.Source)
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	} else {
		msg.Timestamp = msg.Timestamp.UTC()
	}

	originalLen := len(msg.Body)
	body := msg.Body
	if len(body) > g.cfg.MaxBodyBytes {
		body = body[:g.cfg.MaxBodyBytes]
	}
	msg.Body = append([]byte(nil), body...)
	msg.OriginalLen = originalLen
	return msg, nil
}

func (g *SyslogRelayGuard) nextMessageID(source string) string {
	seq := g.nextSeq
	if seq == 0 {
		seq = 1
	}
	g.nextSeq = seq + 1
	return source + "-" + strconv.FormatUint(seq, 10)
}

func (g *SyslogRelayGuard) enqueueLocked(msg Message) error {
	totalMessages, totalBytes := g.storedTotals()
	if totalMessages >= g.cfg.QueueMaxMessages ||
		len(msg.Body) > g.cfg.QueueMaxBytes-totalBytes {
		g.recordDrop(msg, "queue_full")
		return ErrQueueFull
	}

	// Slots reserved for high-priority sources cannot be consumed by a
	// low-priority source. The reserve is a capacity reservation, not an
	// eviction policy: existing records are never overwritten.
	if _, high := g.highSources[msg.Source]; !high {
		reserve := g.cfg.HighPriorityReserve
		if len(g.highSources) == 0 {
			reserve = 0
		}
		lowCap := g.cfg.QueueMaxMessages - reserve
		if lowCap < 0 {
			lowCap = 0
		}
		if g.storedLowCount()+1 > lowCap {
			g.recordDrop(msg, "high_priority_reserve")
			return ErrQueueFull
		}
	}

	g.pending = append(g.pending, copyMessage(msg))
	return nil
}

func (g *SyslogRelayGuard) storedTotals() (int, int) {
	messages := len(g.pending) + len(g.inflight)
	bytes := 0
	for i := range g.pending {
		bytes += len(g.pending[i].Body)
	}
	for i := range g.inflight {
		bytes += len(g.inflight[i].Body)
	}
	return messages, bytes
}

func (g *SyslogRelayGuard) storedLowCount() int {
	low := 0
	for i := range g.pending {
		if _, high := g.highSources[g.pending[i].Source]; !high {
			low++
		}
	}
	for i := range g.inflight {
		if _, high := g.highSources[g.inflight[i].Source]; !high {
			low++
		}
	}
	return low
}

func (g *SyslogRelayGuard) recordDrop(msg Message, reason string) {
	if g.dropped == nil {
		g.dropped = make(map[string]*dropStat)
	}
	stat := g.dropped[msg.Source]
	if stat == nil {
		stat = &dropStat{}
		g.dropped[msg.Source] = stat
	}
	stat.Messages++
	stat.Bytes += len(msg.Body)
	stat.LastID = msg.MsgID
	stat.LastAt = time.Now().UTC()
	stat.Reason = reason
}

// Frame renders one message with octet counted framing.
func (g *SyslogRelayGuard) Frame(msg Message) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	normalised, err := g.normalizeLocked(msg)
	if err != nil {
		return nil, err
	}
	payload := buildPayload(normalised, false)
	prefix := strconv.Itoa(len(payload)) + " "
	return append([]byte(prefix), payload...), nil
}

// FrameNonTransparent renders one message with newline framing. LF and CR in
// the body are escaped before the single framing trailer is appended, so a
// body line break can never split a record into two frames.
func (g *SyslogRelayGuard) FrameNonTransparent(msg Message) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	normalised, err := g.normalizeLocked(msg)
	if err != nil {
		return nil, err
	}
	payload := buildPayload(normalised, true)
	return append(payload, '\n'), nil
}

func buildPayload(msg Message, escapeBody bool) []byte {
	body := msg.Body
	if escapeBody {
		body = escapeNonTransparent(body)
	}
	app := msg.AppName
	if app == "" {
		app = "-"
	}
	header := fmt.Sprintf("<%d>1 %s %s %s - %s - ",
		msg.Priority,
		msg.Timestamp.UTC().Format(rfc5424TimestampLayout),
		msg.Source,
		app,
		msg.MsgID,
	)
	payload := make([]byte, 0, len(header)+len(body))
	payload = append(payload, header...)
	payload = append(payload, body...)
	return payload
}

func escapeNonTransparent(body []byte) []byte {
	var b strings.Builder
	b.Grow(len(body))
	for _, c := range body {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteByte(c)
		}
	}
	return []byte(b.String())
}

// VerifyPeer checks a presented chain against the allowed CAs and SNI. It
// accepts either CA in the policy list, which keeps the old CA usable while a
// new CA is being rolled in. A failed verification leaves the last verified
// peer untouched.
func (g *SyslogRelayGuard) VerifyPeer(chain []CertInfo, serverName string) (PeerState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		serverName = strings.TrimSpace(g.cfg.ServerName)
	}
	if len(chain) == 0 {
		return PeerState{}, ErrPeerUntrusted
	}

	trustedCA := ""
	for i := range chain {
		if g.caAllowed(chain[i].CA) {
			trustedCA = strings.TrimSpace(chain[i].CA)
			if i == 0 {
				break
			}
		}
	}
	if trustedCA == "" {
		return PeerState{}, ErrPeerUntrusted
	}

	names := chain[0].DNSNames
	if len(names) == 0 {
		for i := range chain {
			if len(chain[i].DNSNames) != 0 {
				names = chain[i].DNSNames
				break
			}
		}
	}
	if serverName == "" || !matchesAnyDNSName(names, serverName) {
		return PeerState{}, ErrSNIMismatch
	}

	generation := g.generation
	if !g.peer.Verified || g.peer.CA != trustedCA {
		generation++
		g.generation = generation
	}
	state := PeerState{
		Verified:   true,
		ServerName: serverName,
		CA:         trustedCA,
		Generation: generation,
	}
	g.peer = state
	return state, nil
}

func (g *SyslogRelayGuard) caAllowed(ca string) bool {
	ca = strings.TrimSpace(ca)
	if ca == "" {
		return false
	}
	for _, allowed := range g.cfg.AllowedCAs {
		if strings.TrimSpace(allowed) == ca {
			return true
		}
	}
	return false
}

func matchesAnyDNSName(names []string, serverName string) bool {
	for _, name := range names {
		if dnsNameMatches(name, serverName) {
			return true
		}
	}
	return false
}

func dnsNameMatches(pattern, serverName string) bool {
	pattern = strings.TrimSpace(pattern)
	serverName = strings.TrimSpace(serverName)
	if pattern == "" || serverName == "" {
		return false
	}
	if strings.EqualFold(pattern, serverName) {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		if len(serverName) <= len(suffix) {
			return false
		}
		if !strings.EqualFold(serverName[len(serverName)-len(suffix):], suffix) {
			return false
		}
		label := serverName[:len(serverName)-len(suffix)]
		return label != "" && !strings.Contains(label, ".")
	}
	return false
}

// QueueDepth reports the pending message count and body bytes.
func (g *SyslogRelayGuard) QueueDepth() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	bytes := 0
	for i := range g.pending {
		bytes += len(g.pending[i].Body)
	}
	return len(g.pending), bytes
}

// Pending returns a copy of the pending queue, oldest first.
func (g *SyslogRelayGuard) Pending() []Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneMessages(g.pending)
}

// Inflight returns a copy of the unacknowledged batch, oldest first.
func (g *SyslogRelayGuard) Inflight() []Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneMessages(g.inflight)
}

// Ack removes one delivered message from the in-flight batch. An unknown or
// repeated message id is a successful no-op so retries are idempotent.
func (g *SyslogRelayGuard) Ack(msgID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for i := range g.inflight {
		if g.inflight[i].MsgID != msgID {
			continue
		}
		copy(g.inflight[i:], g.inflight[i+1:])
		g.inflight[len(g.inflight)-1] = Message{}
		g.inflight = g.inflight[:len(g.inflight)-1]
		return nil
	}
	return nil
}

// SourceStats returns a deterministic per-source trace of backlog, in-flight
// records and rejected records.
func (g *SyslogRelayGuard) SourceStats() []SourceStatus {
	g.mu.Lock()
	defer g.mu.Unlock()

	stats := make(map[string]*SourceStatus)
	get := func(source string) *SourceStatus {
		stat := stats[source]
		if stat == nil {
			stat = &SourceStatus{Source: source}
			stats[source] = stat
		}
		return stat
	}
	for i := range g.pending {
		stat := get(g.pending[i].Source)
		stat.PendingMessages++
		stat.PendingBytes += len(g.pending[i].Body)
	}
	for i := range g.inflight {
		stat := get(g.inflight[i].Source)
		stat.InflightMessages++
		stat.InflightBytes += len(g.inflight[i].Body)
	}
	for source, drop := range g.dropped {
		stat := get(source)
		stat.DroppedMessages += drop.Messages
		stat.DroppedBytes += drop.Bytes
		stat.LastDroppedID = drop.LastID
		stat.LastDroppedReason = drop.Reason
	}

	sources := make([]string, 0, len(stats))
	for source := range stats {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	out := make([]SourceStatus, 0, len(sources))
	for _, source := range sources {
		out = append(out, *stats[source])
	}
	return out
}

// Snapshot serialises the queue, the in-flight batch and the id watermark.
func (g *SyslogRelayGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	snap := snapshotState{
		Magic:      snapshotMagic,
		Version:    snapshotVersion,
		NextSeq:    g.nextSeq,
		Pending:    cloneMessages(g.pending),
		Inflight:   cloneMessages(g.inflight),
		Peer:       g.peer,
		Generation: g.generation,
	}
	if len(g.dropped) != 0 {
		snap.Dropped = make(map[string]*dropStat, len(g.dropped))
		for source, stat := range g.dropped {
			cp := *stat
			snap.Dropped[source] = &cp
		}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SyslogRelayGuard) Restore(data []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var snap snapshotState
	if err := json.Unmarshal(data, &snap); err != nil {
		return ErrBadSnapshot
	}
	if snap.Magic != snapshotMagic || snap.Version != snapshotVersion {
		return ErrBadSnapshot
	}

	watermark := snap.NextSeq
	if watermark == 0 {
		watermark = 1
	}
	for _, msg := range append(append([]Message(nil), snap.Pending...), snap.Inflight...) {
		if strings.TrimSpace(msg.Source) == "" || len(msg.Body) == 0 || strings.TrimSpace(msg.MsgID) == "" {
			return ErrBadSnapshot
		}
		if msg.Priority < 0 || msg.Priority > 191 {
			return ErrBadSnapshot
		}
		if seq, ok := trailingSequence(msg.MsgID); ok {
			if seq >= watermark && seq != ^uint64(0) {
				watermark = seq + 1
			}
		}
	}

	// The complete payload has been validated; there has been no partial
	// mutation. Replace all state in one critical section.
	g.pending = cloneMessages(snap.Pending)
	g.inflight = cloneMessages(snap.Inflight)
	g.nextSeq = watermark
	g.peer = snap.Peer
	g.generation = snap.Generation
	g.dropped = make(map[string]*dropStat, len(snap.Dropped))
	for source, stat := range snap.Dropped {
		if stat == nil {
			continue
		}
		cp := *stat
		g.dropped[source] = &cp
	}
	return nil
}

func trailingSequence(msgID string) (uint64, bool) {
	dash := strings.LastIndexByte(msgID, '-')
	if dash < 0 || dash == len(msgID)-1 {
		return 0, false
	}
	seq, err := strconv.ParseUint(msgID[dash+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

func cloneMessages(src []Message) []Message {
	if len(src) == 0 {
		return nil
	}
	out := make([]Message, len(src))
	for i := range src {
		out[i] = copyMessage(src[i])
	}
	return out
}

func copyMessage(msg Message) Message {
	msg.Body = append([]byte(nil), msg.Body...)
	return msg
}
