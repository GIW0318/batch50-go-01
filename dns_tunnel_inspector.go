// Package task inspects DNS queries for tunnelling and data exfiltration. It
// scores the leftmost label entropy, the TXT payload size and the query rate of
// every client, and only exempts whitelist entries registered by full name.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
)

// Verdict actions returned by Observe.
const (
	ActionAllow   = "allow"
	ActionObserve = "observe"
	ActionBlock   = "block"
)

// Signal codes attached to a verdict and to a client record.
const (
	SignalHighEntropy = "high_entropy"
	SignalLongPayload = "long_payload"
	SignalHighRate    = "high_rate"
)

// Score anchors and decision thresholds.
const (
	ScoreHighEntropy = 35
	ScoreLongPayload = 35
	ScoreHighRate    = 30
	ObserveThreshold = 40
	BlockThreshold   = 80
)

// Whitelist reason attached to an exempt verdict.
const ReasonWhitelisted = "whitelisted"

// Errors returned by the inspector.
var (
	ErrBadQuery     = errors.New("client id or query name is invalid")
	ErrBadQType     = errors.New("query type is not inspected")
	ErrBadWhitelist = errors.New("whitelist entry is invalid")
	ErrClientLimit  = errors.New("client table is full")
	ErrBadSnapshot  = errors.New("snapshot payload is invalid")
)

// Query is one observed DNS query.
type Query struct {
	ClientID  string `json:"client_id"`
	Name      string `json:"name"`
	QType     string `json:"qtype"`
	TXTLength int    `json:"txt_length"`
	At        int64  `json:"at"`
}

// Verdict is the explainable outcome of one query.
type Verdict struct {
	ClientID string   `json:"client_id"`
	Name     string   `json:"name"`
	Action   string   `json:"action"`
	Reason   string   `json:"reason"`
	Score    int      `json:"score"`
	Signals  []string `json:"signals"`
	Seq      int64    `json:"seq"`
}

// ClientVerdict is the windowed state of one client.
type ClientVerdict struct {
	ClientID  string   `json:"client_id"`
	Queries   int      `json:"queries"`
	TXTBytes  int      `json:"txt_bytes"`
	Score     int      `json:"score"`
	Action    string   `json:"action"`
	Signals   []string `json:"signals"`
	FirstSeen int64    `json:"first_seen"`
	LastSeen  int64    `json:"last_seen"`
	Seq       int64    `json:"seq"`
}

// Stats is a copy of the counters kept by the inspector.
type Stats struct {
	Allowed     int `json:"allowed"`
	Observed    int `json:"observed"`
	Blocked     int `json:"blocked"`
	Whitelisted int `json:"whitelisted"`
}

// InspectorConfig bounds the window and the tables of the inspector.
type InspectorConfig struct {
	Window              int64   `json:"window"`
	MaxClients          int     `json:"max_clients"`
	MaxQueriesPerWindow int     `json:"max_queries_per_window"`
	MaxQueriesPerClient int     `json:"max_queries_per_client"`
	MaxTXTLength        int     `json:"max_txt_length"`
	MaxNameLength       int     `json:"max_name_length"`
	EntropyThreshold    float64 `json:"entropy_threshold"`
}

// DnsTunnelInspector owns the whitelist, the per client windows and the
// monotonic sequence shared by them.
type DnsTunnelInspector struct {
	mu        sync.RWMutex
	cfg       InspectorConfig
	whitelist map[string]struct{}
	clients   map[string]*clientWindow
	stats     Stats
	seq       int64
}

const snapshotVersion = 1

type queryRecord struct {
	At        int64  `json:"at"`
	Name      string `json:"name"`
	QType     string `json:"qtype"`
	TXTLength int    `json:"txt_length"`
	Seq       int64  `json:"seq"`
}

type clientWindow struct {
	id      string
	entries []queryRecord
	lastSeq int64
}

// snapshotState is deliberately separate from the live types. Restore can
// parse and validate it completely before taking the inspector lock.
type snapshotState struct {
	Whitelist []string         `json:"whitelist"`
	Clients   []snapshotClient `json:"clients"`
	Stats     Stats            `json:"stats"`
	Seq       int64            `json:"seq"`
}

type snapshotClient struct {
	ClientID string        `json:"client_id"`
	Entries  []queryRecord `json:"entries"`
	LastSeq  int64         `json:"last_seq"`
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	State    json.RawMessage `json:"state"`
	Checksum string          `json:"checksum"`
}

// NewDnsTunnelInspector builds an inspector with the given window and caps.
func NewDnsTunnelInspector(cfg InspectorConfig) *DnsTunnelInspector {
	return &DnsTunnelInspector{
		cfg:       cfg,
		whitelist: make(map[string]struct{}),
		clients:   make(map[string]*clientWindow),
	}
}

// AllowName registers one fully qualified name that is exempt from scoring.
func (d *DnsTunnelInspector) AllowName(name string) error {
	if !validWhitelistName(name) {
		return ErrBadWhitelist
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.whitelist[name] = struct{}{}
	return nil
}

// Observe scores one query and folds it into the window of its client.
func (d *DnsTunnelInspector) Observe(q Query) (Verdict, error) {
	normalized, err := normalizeQuery(q)
	if err != nil {
		return Verdict{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.whitelist[normalized.Name]; ok {
		d.seq++
		d.stats.Whitelisted++
		return Verdict{
			ClientID: normalized.ClientID,
			Name:     normalized.Name,
			Action:   ActionAllow,
			Reason:   ReasonWhitelisted,
			Score:    0,
			Signals:  []string{},
			Seq:      d.seq,
		}, nil
	}

	client, ok := d.clients[normalized.ClientID]
	if !ok {
		if len(d.clients) >= d.cfg.MaxClients {
			return Verdict{}, ErrClientLimit
		}
		client = &clientWindow{id: normalized.ClientID}
		d.clients[normalized.ClientID] = client
	}

	d.seq++
	record := queryRecord{
		At:        normalized.At,
		Name:      normalized.Name,
		QType:     normalized.QType,
		TXTLength: normalized.TXTLength,
		Seq:       d.seq,
	}

	now := normalized.At
	if len(client.entries) > 0 && client.entries[len(client.entries)-1].At > now {
		now = client.entries[len(client.entries)-1].At
	}
	client.entries = insertRecord(client.entries, record)
	d.trimClient(client, now)
	client.lastSeq = record.Seq

	highEntropy := leftLabelEntropy(normalized.Name) > d.cfg.EntropyThreshold
	longPayload := txtLengthExceeded(normalized, d.cfg) || len(normalized.Name) > d.cfg.MaxNameLength
	highRate := len(client.entries) > d.cfg.MaxQueriesPerWindow
	signals := makeSignals(highEntropy, longPayload, highRate)
	score, action := scoreSignals(signals)

	d.recordStats(action)
	return Verdict{
		ClientID: normalized.ClientID,
		Name:     normalized.Name,
		Action:   action,
		Score:    score,
		Signals:  signals,
		Seq:      d.seq,
	}, nil
}

// Whitelisted reports whether one fully qualified name is registered.
func (d *DnsTunnelInspector) Whitelisted(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.whitelist[name]
	return ok
}

// ClientState reads the windowed state of one client.
func (d *DnsTunnelInspector) ClientState(clientID string) (ClientVerdict, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	client, ok := d.clients[clientID]
	if !ok {
		return ClientVerdict{}, false
	}
	return d.clientVerdict(client), true
}

// Flush returns the final state of one client and drops it from the table.
func (d *DnsTunnelInspector) Flush(clientID string) (ClientVerdict, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	client, ok := d.clients[clientID]
	if !ok {
		return ClientVerdict{}, nil
	}
	state := d.clientVerdict(client)
	delete(d.clients, clientID)
	return state, nil
}

// Sweep drops clients that have been idle for longer than the window.
func (d *DnsTunnelInspector) Sweep(now int64) []ClientVerdict {
	d.mu.Lock()
	defer d.mu.Unlock()

	ids := make([]string, 0, len(d.clients))
	for id := range d.clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var dropped []ClientVerdict
	for _, id := range ids {
		client := d.clients[id]
		if len(client.entries) == 0 || now-client.entries[len(client.entries)-1].At > d.cfg.Window {
			dropped = append(dropped, d.clientVerdict(client))
			delete(d.clients, id)
			continue
		}
		d.trimClient(client, now)
	}
	return dropped
}

// Explain lists the signals one query would raise, sorted and deduplicated.
func (d *DnsTunnelInspector) Explain(q Query) []string {
	normalized, err := normalizeQuery(q)
	if err != nil {
		return nil
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	if _, ok := d.whitelist[normalized.Name]; ok {
		return []string{}
	}

	highEntropy := leftLabelEntropy(normalized.Name) > d.cfg.EntropyThreshold
	longPayload := txtLengthExceeded(normalized, d.cfg) || len(normalized.Name) > d.cfg.MaxNameLength

	count := 1
	if client, ok := d.clients[normalized.ClientID]; ok {
		now := normalized.At
		if len(client.entries) > 0 && client.entries[len(client.entries)-1].At > now {
			now = client.entries[len(client.entries)-1].At
		}
		cutoff := now - d.cfg.Window
		count = 0
		for _, entry := range client.entries {
			if entry.At >= cutoff {
				count++
			}
		}
		count++
	}
	if limit := d.cfg.MaxQueriesPerClient; limit >= 0 && count > limit {
		count = limit
	}

	highRate := count > d.cfg.MaxQueriesPerWindow
	return makeSignals(highEntropy, longPayload, highRate)
}

// Stats reads the counters of the inspector.
func (d *DnsTunnelInspector) Stats() Stats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.stats
}

// Snapshot serialises the whitelist, the client windows, the counters and the
// sequence.
func (d *DnsTunnelInspector) Snapshot() ([]byte, error) {
	d.mu.RLock()
	state := snapshotState{
		Whitelist: make([]string, 0, len(d.whitelist)),
		Clients:   make([]snapshotClient, 0, len(d.clients)),
		Stats:     d.stats,
		Seq:       d.seq,
	}
	for name := range d.whitelist {
		state.Whitelist = append(state.Whitelist, name)
	}
	for _, client := range d.clients {
		entries := make([]queryRecord, len(client.entries))
		copy(entries, client.entries)
		state.Clients = append(state.Clients, snapshotClient{
			ClientID: client.id,
			Entries:  entries,
			LastSeq:  client.lastSeq,
		})
	}
	d.mu.RUnlock()

	sort.Strings(state.Whitelist)
	sort.Slice(state.Clients, func(i, j int) bool {
		return state.Clients[i].ClientID < state.Clients[j].ClientID
	})

	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		State:    raw,
		Checksum: hex.EncodeToString(sum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *DnsTunnelInspector) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return ErrBadSnapshot
	}

	sum := sha256.Sum256(envelope.State)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return ErrBadSnapshot
	}

	var state snapshotState
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return ErrBadSnapshot
	}
	if state.Seq < 0 || state.Stats.Allowed < 0 || state.Stats.Observed < 0 ||
		state.Stats.Blocked < 0 || state.Stats.Whitelisted < 0 {
		return ErrBadSnapshot
	}
	if len(state.Clients) > d.cfg.MaxClients {
		return ErrBadSnapshot
	}

	whitelist := make(map[string]struct{}, len(state.Whitelist))
	for _, name := range state.Whitelist {
		if !validWhitelistName(name) {
			return ErrBadSnapshot
		}
		whitelist[name] = struct{}{}
	}

	clients := make(map[string]*clientWindow, len(state.Clients))
	for _, saved := range state.Clients {
		if strings.TrimSpace(saved.ClientID) == "" {
			return ErrBadSnapshot
		}
		if _, exists := clients[saved.ClientID]; exists {
			return ErrBadSnapshot
		}
		if saved.LastSeq < 0 || saved.LastSeq > state.Seq {
			return ErrBadSnapshot
		}
		if limit := d.cfg.MaxQueriesPerClient; limit >= 0 && len(saved.Entries) > limit {
			return ErrBadSnapshot
		}

		entries := make([]queryRecord, len(saved.Entries))
		var maxEntrySeq int64
		for i, entry := range saved.Entries {
			normalized, err := normalizeQuery(Query{
				ClientID:  saved.ClientID,
				Name:      entry.Name,
				QType:     entry.QType,
				TXTLength: entry.TXTLength,
				At:        entry.At,
			})
			if err != nil || entry.Seq <= 0 || entry.Seq > saved.LastSeq || entry.Seq > state.Seq {
				return ErrBadSnapshot
			}
			if i > 0 && entry.At < saved.Entries[i-1].At {
				return ErrBadSnapshot
			}
			if entry.Seq > maxEntrySeq {
				maxEntrySeq = entry.Seq
			}
			entries[i] = queryRecord{
				At:        normalized.At,
				Name:      normalized.Name,
				QType:     normalized.QType,
				TXTLength: normalized.TXTLength,
				Seq:       entry.Seq,
			}
		}
		if saved.LastSeq < maxEntrySeq {
			return ErrBadSnapshot
		}
		clients[saved.ClientID] = &clientWindow{
			id:      saved.ClientID,
			entries: entries,
			lastSeq: saved.LastSeq,
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.whitelist = whitelist
	d.clients = clients
	d.stats = state.Stats
	if state.Seq > d.seq {
		d.seq = state.Seq
	}
	return nil
}

func validWhitelistName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	return validName(name)
}

func validName(name string) bool {
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
	}
	return true
}

func normalizeQuery(q Query) (Query, error) {
	if strings.TrimSpace(q.ClientID) == "" || strings.TrimSpace(q.Name) == "" || !validName(q.Name) {
		return Query{}, ErrBadQuery
	}

	q.QType = strings.ToUpper(strings.TrimSpace(q.QType))
	switch q.QType {
	case "A", "AAAA", "TXT", "CNAME", "MX":
	default:
		return Query{}, ErrBadQType
	}
	return q, nil
}

func leftLabelEntropy(name string) float64 {
	label := name
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		label = name[:dot]
	}
	if label == "" {
		return 0
	}

	counts := make(map[byte]int, len(label))
	for i := 0; i < len(label); i++ {
		counts[label[i]]++
	}

	var entropy float64
	total := float64(len(label))
	for _, count := range counts {
		p := float64(count) / total
		entropy -= p * math.Log2(p)
	}
	return entropy
}

func txtLengthExceeded(q Query, cfg InspectorConfig) bool {
	return q.TXTLength > cfg.MaxTXTLength
}

func makeSignals(highEntropy, longPayload, highRate bool) []string {
	signals := make([]string, 0, 3)
	if highEntropy {
		signals = append(signals, SignalHighEntropy)
	}
	if highRate {
		signals = append(signals, SignalHighRate)
	}
	if longPayload {
		signals = append(signals, SignalLongPayload)
	}
	sort.Strings(signals)
	return signals
}

func scoreSignals(signals []string) (int, string) {
	score := 0
	for _, signal := range signals {
		switch signal {
		case SignalHighEntropy:
			score += ScoreHighEntropy
		case SignalLongPayload:
			score += ScoreLongPayload
		case SignalHighRate:
			score += ScoreHighRate
		}
	}

	switch {
	case score >= BlockThreshold:
		return score, ActionBlock
	case score >= ObserveThreshold:
		return score, ActionObserve
	default:
		return score, ActionAllow
	}
}

func insertRecord(entries []queryRecord, record queryRecord) []queryRecord {
	index := sort.Search(len(entries), func(i int) bool {
		return entries[i].At > record.At
	})
	entries = append(entries, queryRecord{})
	copy(entries[index+1:], entries[index:])
	entries[index] = record
	return entries
}

func (d *DnsTunnelInspector) trimClient(client *clientWindow, now int64) {
	if len(client.entries) > 0 {
		cutoff := now - d.cfg.Window
		first := sort.Search(len(client.entries), func(i int) bool {
			return client.entries[i].At >= cutoff
		})
		if first > 0 {
			entries := make([]queryRecord, len(client.entries)-first)
			copy(entries, client.entries[first:])
			client.entries = entries
		}
	}

	limit := d.cfg.MaxQueriesPerClient
	if limit < 0 {
		limit = 0
	}
	if len(client.entries) > limit {
		entries := make([]queryRecord, limit)
		copy(entries, client.entries[len(client.entries)-limit:])
		client.entries = entries
	}
}

func (d *DnsTunnelInspector) clientVerdict(client *clientWindow) ClientVerdict {
	var highEntropy, longPayload bool
	var txtBytes int
	for _, entry := range client.entries {
		if leftLabelEntropy(entry.Name) > d.cfg.EntropyThreshold {
			highEntropy = true
		}
		if entry.TXTLength > d.cfg.MaxTXTLength || len(entry.Name) > d.cfg.MaxNameLength {
			longPayload = true
		}
		txtBytes += entry.TXTLength
	}
	highRate := len(client.entries) > d.cfg.MaxQueriesPerWindow
	signals := makeSignals(highEntropy, longPayload, highRate)
	score, action := scoreSignals(signals)

	state := ClientVerdict{
		ClientID: client.id,
		Queries:  len(client.entries),
		TXTBytes: txtBytes,
		Score:    score,
		Action:   action,
		Signals:  signals,
		Seq:      client.lastSeq,
	}
	if len(client.entries) > 0 {
		state.FirstSeen = client.entries[0].At
		state.LastSeen = client.entries[len(client.entries)-1].At
	}
	return state
}

func (d *DnsTunnelInspector) recordStats(action string) {
	switch action {
	case ActionBlock:
		d.stats.Blocked++
	case ActionObserve:
		d.stats.Observed++
	default:
		d.stats.Allowed++
	}
}
