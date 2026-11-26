// Package task watches BGP announcements for prefix hijacks. It keeps the ROA
// table, the per prefix route table and the alert table, and grades every
// announcement with RPKI origin validation plus AS path anomalies.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Verdict actions returned by Announce.
const (
	ActionAccept  = "accept"
	ActionPending = "pending"
	ActionAlert   = "alert"
)

// Signal codes attached to a verdict and to an alert.
const (
	SignalOriginMismatch = "origin_mismatch"
	SignalPathAnomaly    = "path_anomaly"
	SignalROANotFound    = "roa_not_found"
)

// Score anchors and decision thresholds.
const (
	ScoreOriginMismatch = 40
	ScorePathAnomaly    = 30
	ScoreROANotFound    = 10
	PendingThreshold    = 30
	AlertThreshold      = 60
)

// Errors returned by the watch.
var (
	ErrBadPrefix     = errors.New("prefix is not a valid network")
	ErrBadPeer       = errors.New("peer name is required")
	ErrBadPath       = errors.New("as path is empty or does not end with the origin as")
	ErrBadROA        = errors.New("roa prefix or max length is invalid")
	ErrPeerLimit     = errors.New("peer limit reached for this prefix")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
	ErrStaleSnapshot = errors.New("snapshot is older than the live state")
)

// ROA is one validated origin authorisation loaded from the RPKI cache.
type ROA struct {
	Prefix   string `json:"prefix"`
	MaxLen   int    `json:"max_len"`
	OriginAS uint32 `json:"origin_as"`
	NotAfter int64  `json:"not_after"`
}

// Announcement is one prefix announced by one peer.
type Announcement struct {
	Peer     string   `json:"peer"`
	Prefix   string   `json:"prefix"`
	OriginAS uint32   `json:"origin_as"`
	ASPath   []uint32 `json:"as_path"`
	At       int64    `json:"at"`
}

// Route is the current record of one peer for one prefix.
type Route struct {
	Peer      string   `json:"peer"`
	Prefix    string   `json:"prefix"`
	OriginAS  uint32   `json:"origin_as"`
	ASPath   []uint32 `json:"as_path"`
	FirstSeen int64    `json:"first_seen"`
	LastSeen  int64    `json:"last_seen"`
}

// Alert is the open hijack record of one prefix.
type Alert struct {
	Prefix      string   `json:"prefix"`
	Score       int      `json:"score"`
	Signals     []string `json:"signals"`
	Seq         int64    `json:"seq"`
	WindowStart int64    `json:"window_start"`
	WindowEnd   int64    `json:"window_end"`
	Peers       []string `json:"peers"`
}

// Verdict is the explainable outcome of one announcement.
type Verdict struct {
	Prefix  string   `json:"prefix"`
	Action  string   `json:"action"`
	Score   int      `json:"score"`
	Signals []string `json:"signals"`
	Seq     int64    `json:"seq"`
}

// WatchConfig bounds the tables and the time windows of the watch.
type WatchConfig struct {
	MaxPeersPerPrefix int   `json:"max_peers_per_prefix"`
	MaxAlerts         int   `json:"max_alerts"`
	MaxASPathLen      int   `json:"max_as_path_len"`
	HoldWindow        int64 `json:"hold_window"`
	FlapWindow        int64 `json:"flap_window"`
}

// Stats is a copy of the counters kept by the watch.
type Stats struct {
	Accepted  int `json:"accepted"`
	Pending   int `json:"pending"`
	Alerted   int `json:"alerted"`
	Withdrawn int `json:"withdrawn"`
	Dismissed int `json:"dismissed"`
	Evicted   int `json:"evicted"`
	Flaps     int `json:"flaps"`
	Alerts    int `json:"alerts"`
}

// BgpHijackWatch owns the ROA table, the route table, the alert table and the
// monotonic sequence shared by all of them.
type BgpHijackWatch struct {
	cfg WatchConfig
}

// NewBgpHijackWatch builds a watch with the given caps and windows.
func NewBgpHijackWatch(cfg WatchConfig) *BgpHijackWatch {
	return &BgpHijackWatch{cfg: cfg}
}

// SetROA inserts or replaces one origin authorisation.
func (w *BgpHijackWatch) SetROA(roa ROA) error {
	return errNotImplemented
}

// Announce records one announcement and returns its graded verdict.
func (w *BgpHijackWatch) Announce(a Announcement) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Withdraw removes the route of one peer for one prefix.
func (w *BgpHijackWatch) Withdraw(peer, prefix string) error {
	return errNotImplemented
}

// Routes lists the live routes of one prefix, ordered by peer name.
func (w *BgpHijackWatch) Routes(prefix string) []Route {
	return nil
}

// AlertState reads the open alert of one prefix.
func (w *BgpHijackWatch) AlertState(prefix string) (Alert, bool) {
	return Alert{}, false
}

// Dismiss closes the open alert of one prefix.
func (w *BgpHijackWatch) Dismiss(prefix string) error {
	return errNotImplemented
}

// Sweep closes alerts that have not seen a new announcement within the hold
// window and returns them.
func (w *BgpHijackWatch) Sweep(now int64) []Alert {
	return nil
}

// ROAs lists the loaded origin authorisations ordered by prefix, then origin.
func (w *BgpHijackWatch) ROAs() []ROA {
	return nil
}

// Stats reads the counters of the watch.
func (w *BgpHijackWatch) Stats() Stats {
	return Stats{}
}

// Snapshot serialises the ROA table, the routes, the alerts and the sequence.
func (w *BgpHijackWatch) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (w *BgpHijackWatch) Restore(data []byte) error {
	return errNotImplemented
}
