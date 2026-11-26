// Package task inspects DNS queries for tunnelling and data exfiltration. It
// scores the leftmost label entropy, the TXT payload size and the query rate of
// every client, and only exempts whitelist entries registered by full name.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	cfg InspectorConfig
}

// NewDnsTunnelInspector builds an inspector with the given window and caps.
func NewDnsTunnelInspector(cfg InspectorConfig) *DnsTunnelInspector {
	return &DnsTunnelInspector{cfg: cfg}
}

// AllowName registers one fully qualified name that is exempt from scoring.
func (d *DnsTunnelInspector) AllowName(name string) error {
	return errNotImplemented
}

// Observe scores one query and folds it into the window of its client.
func (d *DnsTunnelInspector) Observe(q Query) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Whitelisted reports whether one fully qualified name is registered.
func (d *DnsTunnelInspector) Whitelisted(name string) bool {
	return false
}

// ClientState reads the windowed state of one client.
func (d *DnsTunnelInspector) ClientState(clientID string) (ClientVerdict, bool) {
	return ClientVerdict{}, false
}

// Flush returns the final state of one client and drops it from the table.
func (d *DnsTunnelInspector) Flush(clientID string) (ClientVerdict, error) {
	return ClientVerdict{}, errNotImplemented
}

// Sweep drops clients that have been idle for longer than the window.
func (d *DnsTunnelInspector) Sweep(now int64) []ClientVerdict {
	return nil
}

// Explain lists the signals one query would raise, sorted and deduplicated.
func (d *DnsTunnelInspector) Explain(q Query) []string {
	return nil
}

// Stats reads the counters of the inspector.
func (d *DnsTunnelInspector) Stats() Stats {
	return Stats{}
}

// Snapshot serialises the whitelist, the client windows, the counters and the
// sequence.
func (d *DnsTunnelInspector) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *DnsTunnelInspector) Restore(data []byte) error {
	return errNotImplemented
}
