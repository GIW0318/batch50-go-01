// Package task watches BGP announcements for prefix hijacks. It keeps the ROA
// table, the per prefix route table and the alert table, and grades every
// announcement with RPKI origin validation plus AS path anomalies.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
)

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

const snapshotFormatVersion = 1

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
	ASPath    []uint32 `json:"as_path"`
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

type snapshotData struct {
	Format    int              `json:"format"`
	Seq       int64            `json:"seq"`
	ROAs      []ROA            `json:"roas"`
	Routes    []Route          `json:"routes"`
	Alerts    []Alert          `json:"alerts"`
	FlapSince map[string]int64 `json:"flap_since,omitempty"`
	Stats     Stats            `json:"stats"`
}

// BgpHijackWatch owns the ROA table, the route table, the alert table and the
// monotonic sequence shared by all of them.
type BgpHijackWatch struct {
	mu        sync.RWMutex
	cfg       WatchConfig
	seq       int64
	roas      map[string]ROA
	routes    map[string]map[string]Route
	alerts    map[string]Alert
	flapSince map[string]int64
	stats     Stats
}

// NewBgpHijackWatch builds a watch with the given caps and windows.
func NewBgpHijackWatch(cfg WatchConfig) *BgpHijackWatch {
	return &BgpHijackWatch{
		cfg:       cfg,
		roas:      make(map[string]ROA),
		routes:    make(map[string]map[string]Route),
		alerts:    make(map[string]Alert),
		flapSince: make(map[string]int64),
	}
}

// SetROA inserts or replaces one origin authorisation.
func (w *BgpHijackWatch) SetROA(roa ROA) error {
	normalized, err := validateROA(roa)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()
	w.roas[roaKey(normalized)] = normalized
	w.nextSeqLocked()
	return nil
}

// Announce records one announcement and returns its graded verdict.
func (w *BgpHijackWatch) Announce(a Announcement) (Verdict, error) {
	prefix, _, err := parseCanonicalPrefix(a.Prefix)
	if err != nil {
		return Verdict{}, err
	}
	peer, err := normalizePeer(a.Peer)
	if err != nil {
		return Verdict{}, err
	}
	if err := validateASPath(a); err != nil {
		return Verdict{}, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()

	peers := w.routes[prefix]
	_, knownPeer := peers[peer]
	if !knownPeer && len(peers) >= w.cfg.MaxPeersPerPrefix {
		return Verdict{}, ErrPeerLimit
	}

	seq := w.nextSeqLocked()
	signals := w.signalsLocked(prefix, a)
	score := scoreSignals(signals)
	action := ActionAccept
	switch {
	case score >= AlertThreshold:
		action = ActionAlert
	case score >= PendingThreshold:
		action = ActionPending
	}

	w.recordRouteLocked(prefix, peer, a)
	if since, ok := w.flapSince[prefix]; ok {
		elapsed := a.At - since
		if elapsed >= 0 && elapsed < w.cfg.FlapWindow {
			w.stats.Flaps++
		}
		delete(w.flapSince, prefix)
	}

	verdict := Verdict{
		Prefix:  prefix,
		Action:  action,
		Score:   score,
		Signals: cloneStrings(signals),
		Seq:     seq,
	}

	if action == ActionAlert {
		w.recordAlertLocked(prefix, a, signals, score, seq)
		w.stats.Alerted++
	} else {
		w.refreshAlertWindowLocked(prefix, a.At)
		if action == ActionPending {
			w.stats.Pending++
		} else {
			w.stats.Accepted++
		}
	}

	return verdict, nil
}

// Withdraw removes the route of one peer for one prefix.
func (w *BgpHijackWatch) Withdraw(peer, prefix string) error {
	canonical, _, err := parseCanonicalPrefix(prefix)
	if err != nil {
		return err
	}
	normalizedPeer, err := normalizePeer(peer)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()

	peers, ok := w.routes[canonical]
	if !ok {
		return nil
	}
	removed, ok := peers[normalizedPeer]
	if !ok {
		return nil
	}

	lastSeen := removed.LastSeen
	for _, route := range peers {
		if route.LastSeen > lastSeen {
			lastSeen = route.LastSeen
		}
	}

	delete(peers, normalizedPeer)
	w.stats.Withdrawn++
	if len(peers) == 0 {
		delete(w.routes, canonical)
		w.flapSince[canonical] = lastSeen
	}
	w.nextSeqLocked()
	return nil
}

// Routes lists the live routes of one prefix, ordered by peer name.
func (w *BgpHijackWatch) Routes(prefix string) []Route {
	canonical, _, err := parseCanonicalPrefix(prefix)
	if err != nil {
		return []Route{}
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	routes := make([]Route, 0, len(w.routes[canonical]))
	for _, route := range w.routes[canonical] {
		routes = append(routes, cloneRoute(route))
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Peer < routes[j].Peer
	})
	return routes
}

// AlertState reads the open alert of one prefix.
func (w *BgpHijackWatch) AlertState(prefix string) (Alert, bool) {
	canonical, _, err := parseCanonicalPrefix(prefix)
	if err != nil {
		return Alert{}, false
	}

	w.mu.RLock()
	defer w.mu.RUnlock()
	alert, ok := w.alerts[canonical]
	if !ok {
		return Alert{}, false
	}
	return cloneAlert(alert), true
}

// Dismiss closes the open alert of one prefix.
func (w *BgpHijackWatch) Dismiss(prefix string) error {
	canonical, _, err := parseCanonicalPrefix(prefix)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()

	if _, ok := w.alerts[canonical]; !ok {
		return nil
	}
	delete(w.alerts, canonical)
	w.stats.Dismissed++
	w.nextSeqLocked()
	return nil
}

// Sweep closes alerts that have not seen a new announcement within the hold
// window and returns them.
func (w *BgpHijackWatch) Sweep(now int64) []Alert {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()

	closed := make([]Alert, 0)
	for prefix, alert := range w.alerts {
		if now <= alert.WindowEnd {
			continue
		}
		closed = append(closed, cloneAlert(alert))
		delete(w.alerts, prefix)
	}
	if len(closed) == 0 {
		return []Alert{}
	}

	sort.Slice(closed, func(i, j int) bool {
		return closed[i].Seq < closed[j].Seq
	})
	w.nextSeqLocked()
	return closed
}

// ROAs lists the loaded origin authorisations ordered by prefix, then origin.
func (w *BgpHijackWatch) ROAs() []ROA {
	w.mu.RLock()
	defer w.mu.RUnlock()

	roas := make([]ROA, 0, len(w.roas))
	for _, roa := range w.roas {
		roas = append(roas, roa)
	}
	sortROAs(roas)
	return roas
}

// Stats reads the counters of the watch.
func (w *BgpHijackWatch) Stats() Stats {
	w.mu.RLock()
	defer w.mu.RUnlock()

	stats := w.stats
	stats.Alerts = len(w.alerts)
	return stats
}

// Snapshot serialises the ROA table, the routes, the alerts and the sequence.
func (w *BgpHijackWatch) Snapshot() ([]byte, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	data := snapshotData{
		Format:    snapshotFormatVersion,
		Seq:       w.seq,
		ROAs:      make([]ROA, 0, len(w.roas)),
		Routes:    make([]Route, 0),
		Alerts:    make([]Alert, 0, len(w.alerts)),
		FlapSince: make(map[string]int64, len(w.flapSince)),
		Stats:     w.stats,
	}
	data.Stats.Alerts = len(w.alerts)

	for _, roa := range w.roas {
		data.ROAs = append(data.ROAs, roa)
	}
	sortROAs(data.ROAs)

	for _, peers := range w.routes {
		for _, route := range peers {
			data.Routes = append(data.Routes, cloneRoute(route))
		}
	}
	sort.Slice(data.Routes, func(i, j int) bool {
		if data.Routes[i].Prefix != data.Routes[j].Prefix {
			return data.Routes[i].Prefix < data.Routes[j].Prefix
		}
		return data.Routes[i].Peer < data.Routes[j].Peer
	})

	for _, alert := range w.alerts {
		data.Alerts = append(data.Alerts, cloneAlert(alert))
	}
	sort.Slice(data.Alerts, func(i, j int) bool {
		return data.Alerts[i].Seq < data.Alerts[j].Seq
	})

	for prefix, since := range w.flapSince {
		data.FlapSince[prefix] = since
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (w *BgpHijackWatch) Restore(data []byte) error {
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	state, err := stateFromSnapshot(snapshot)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()

	if snapshot.Seq < w.seq {
		return ErrStaleSnapshot
	}

	w.seq = snapshot.Seq
	w.roas = state.roas
	w.routes = state.routes
	w.alerts = state.alerts
	w.flapSince = state.flapSince
	w.stats = snapshot.Stats
	return nil
}

type restoredState struct {
	roas      map[string]ROA
	routes    map[string]map[string]Route
	alerts    map[string]Alert
	flapSince map[string]int64
}

func (w *BgpHijackWatch) initLocked() {
	if w.roas == nil {
		w.roas = make(map[string]ROA)
	}
	if w.routes == nil {
		w.routes = make(map[string]map[string]Route)
	}
	if w.alerts == nil {
		w.alerts = make(map[string]Alert)
	}
	if w.flapSince == nil {
		w.flapSince = make(map[string]int64)
	}
}

func (w *BgpHijackWatch) nextSeqLocked() int64 {
	w.seq++
	return w.seq
}

func (w *BgpHijackWatch) recordRouteLocked(prefix, peer string, a Announcement) {
	peers := w.routes[prefix]
	if peers == nil {
		peers = make(map[string]Route)
		w.routes[prefix] = peers
	}
	if route, ok := peers[peer]; ok {
		route.LastSeen = a.At
		peers[peer] = route
		return
	}

	peers[peer] = Route{
		Peer:      peer,
		Prefix:    prefix,
		OriginAS:  a.OriginAS,
		ASPath:    append(make([]uint32, 0, len(a.ASPath)), a.ASPath...),
		FirstSeen: a.At,
		LastSeen:  a.At,
	}
}

func (w *BgpHijackWatch) signalsLocked(prefix string, a Announcement) []string {
	signals := make(map[string]struct{})
	_, announcementNetwork, err := net.ParseCIDR(prefix)
	if err != nil {
		return []string{}
	}
	announcementOnes, _ := announcementNetwork.Mask.Size()

	covered := false
	originAuthorized := false
	for _, roa := range w.roas {
		if roa.NotAfter < a.At {
			continue
		}
		_, roaNetwork, err := net.ParseCIDR(roa.Prefix)
		if err != nil {
			continue
		}
		roaOnes, _ := roaNetwork.Mask.Size()
		if announcementOnes < roaOnes ||
			announcementOnes > roa.MaxLen ||
			!roaNetwork.Contains(announcementNetwork.IP) {
			continue
		}

		covered = true
		if roa.OriginAS == a.OriginAS {
			originAuthorized = true
		}
	}

	switch {
	case !covered:
		signals[SignalROANotFound] = struct{}{}
	case !originAuthorized:
		signals[SignalOriginMismatch] = struct{}{}
	}

	if len(a.ASPath) > w.cfg.MaxASPathLen {
		signals[SignalPathAnomaly] = struct{}{}
	}
	for _, asn := range a.ASPath {
		if isPrivateASN(asn) {
			signals[SignalPathAnomaly] = struct{}{}
			break
		}
	}

	result := make([]string, 0, len(signals))
	for signal := range signals {
		result = append(result, signal)
	}
	sort.Strings(result)
	return result
}

func (w *BgpHijackWatch) recordAlertLocked(
	prefix string,
	a Announcement,
	signals []string,
	score int,
	seq int64,
) {
	peers := []string{a.Peer}
	if previous, ok := w.alerts[prefix]; ok {
		peers = append(peers, previous.Peers...)
	}

	w.alerts[prefix] = Alert{
		Prefix:      prefix,
		Score:       score,
		Signals:     cloneStrings(signals),
		Seq:         seq,
		WindowStart: a.At,
		WindowEnd:   a.At + w.cfg.HoldWindow,
		Peers:       sortedUniqueStrings(peers),
	}
	w.evictAlertOverflowLocked()
}

func (w *BgpHijackWatch) refreshAlertWindowLocked(prefix string, at int64) {
	alert, ok := w.alerts[prefix]
	if !ok {
		return
	}
	alert.WindowStart = at
	alert.WindowEnd = at + w.cfg.HoldWindow
	w.alerts[prefix] = alert
}

func (w *BgpHijackWatch) evictAlertOverflowLocked() {
	maxAlerts := w.cfg.MaxAlerts
	if maxAlerts < 0 {
		maxAlerts = 0
	}
	for len(w.alerts) > maxAlerts {
		w.evictOldestAlertLocked()
	}
}

func (w *BgpHijackWatch) evictOldestAlertLocked() {
	oldestPrefix := ""
	var oldestSeq int64
	first := true
	for prefix, alert := range w.alerts {
		if first || alert.Seq < oldestSeq {
			first = false
			oldestPrefix = prefix
			oldestSeq = alert.Seq
		}
	}
	if first {
		return
	}
	delete(w.alerts, oldestPrefix)
	w.stats.Evicted++
}

func parseCanonicalPrefix(prefix string) (string, *net.IPNet, error) {
	ip, network, err := net.ParseCIDR(prefix)
	if err != nil || ip == nil || network == nil {
		return "", nil, ErrBadPrefix
	}
	if !ip.Equal(network.IP) {
		return "", nil, ErrBadPrefix
	}
	return network.String(), network, nil
}

func normalizePeer(peer string) (string, error) {
	normalized := strings.TrimSpace(peer)
	if normalized == "" {
		return "", ErrBadPeer
	}
	return normalized, nil
}

func validateASPath(a Announcement) error {
	if len(a.ASPath) == 0 || a.ASPath[len(a.ASPath)-1] != a.OriginAS {
		return ErrBadPath
	}
	return nil
}

func validateROA(roa ROA) (ROA, error) {
	ip, network, err := net.ParseCIDR(roa.Prefix)
	if err != nil || ip == nil || network == nil || !ip.Equal(network.IP) {
		return ROA{}, fmt.Errorf("%w: invalid prefix", ErrBadROA)
	}

	ones, bits := network.Mask.Size()
	if roa.MaxLen < ones || roa.MaxLen > bits {
		return ROA{}, fmt.Errorf("%w: max length %d", ErrBadROA, roa.MaxLen)
	}

	roa.Prefix = network.String()
	return roa, nil
}

func roaKey(roa ROA) string {
	return fmt.Sprintf("%s@%d", roa.Prefix, roa.OriginAS)
}

func sortROAs(roas []ROA) {
	sort.Slice(roas, func(i, j int) bool {
		if roas[i].Prefix != roas[j].Prefix {
			return roas[i].Prefix < roas[j].Prefix
		}
		if roas[i].OriginAS != roas[j].OriginAS {
			return roas[i].OriginAS < roas[j].OriginAS
		}
		return roas[i].MaxLen < roas[j].MaxLen
	})
}

func scoreSignals(signals []string) int {
	score := 0
	for _, signal := range signals {
		switch signal {
		case SignalOriginMismatch:
			score += ScoreOriginMismatch
		case SignalPathAnomaly:
			score += ScorePathAnomaly
		case SignalROANotFound:
			score += ScoreROANotFound
		}
	}
	return score
}

func isPrivateASN(asn uint32) bool {
	return (asn >= 64512 && asn <= 65534) ||
		(asn >= 4200000000 && asn <= 4294967294)
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append(make([]string, 0, len(values)), values...)
}

func cloneUint32(values []uint32) []uint32 {
	if len(values) == 0 {
		return []uint32{}
	}
	return append(make([]uint32, 0, len(values)), values...)
}

func cloneRoute(route Route) Route {
	route.ASPath = cloneUint32(route.ASPath)
	return route
}

func cloneAlert(alert Alert) Alert {
	alert.Signals = cloneStrings(alert.Signals)
	alert.Peers = cloneStrings(alert.Peers)
	return alert
}

func decodeSnapshot(data []byte) (snapshotData, error) {
	var snapshot snapshotData
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshotData{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return snapshotData{}, fmt.Errorf("%w: trailing JSON data", ErrBadSnapshot)
		}
		return snapshotData{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	if snapshot.Format != snapshotFormatVersion || snapshot.Seq < 0 {
		return snapshotData{}, fmt.Errorf("%w: unsupported format or sequence", ErrBadSnapshot)
	}
	return snapshot, nil
}

func stateFromSnapshot(snapshot snapshotData) (restoredState, error) {
	state := restoredState{
		roas:      make(map[string]ROA, len(snapshot.ROAs)),
		routes:    make(map[string]map[string]Route, len(snapshot.Routes)),
		alerts:    make(map[string]Alert, len(snapshot.Alerts)),
		flapSince: make(map[string]int64, len(snapshot.FlapSince)),
	}

	for _, roa := range snapshot.ROAs {
		normalized, err := validateROA(roa)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
		key := roaKey(normalized)
		if _, exists := state.roas[key]; exists {
			return restoredState{}, fmt.Errorf("%w: duplicate ROA %s", ErrBadSnapshot, key)
		}
		state.roas[key] = normalized
	}

	for _, route := range snapshot.Routes {
		prefix, _, err := parseCanonicalPrefix(route.Prefix)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: route prefix", ErrBadSnapshot)
		}
		peer, err := normalizePeer(route.Peer)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: route peer", ErrBadSnapshot)
		}
		if len(route.ASPath) == 0 || route.ASPath[len(route.ASPath)-1] != route.OriginAS {
			return restoredState{}, fmt.Errorf("%w: route AS path", ErrBadSnapshot)
		}

		route.Prefix = prefix
		route.Peer = peer
		route.ASPath = cloneUint32(route.ASPath)
		peers := state.routes[prefix]
		if peers == nil {
			peers = make(map[string]Route)
			state.routes[prefix] = peers
		}
		if _, exists := peers[peer]; exists {
			return restoredState{}, fmt.Errorf("%w: duplicate route %s/%s", ErrBadSnapshot, prefix, peer)
		}
		peers[peer] = route
	}

	for _, alert := range snapshot.Alerts {
		prefix, _, err := parseCanonicalPrefix(alert.Prefix)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: alert prefix", ErrBadSnapshot)
		}
		if alert.Seq <= 0 || alert.Seq > snapshot.Seq || alert.Score < AlertThreshold {
			return restoredState{}, fmt.Errorf("%w: alert sequence or score", ErrBadSnapshot)
		}
		if err := validateSnapshotSignals(alert.Signals, alert.Score); err != nil {
			return restoredState{}, err
		}
		if len(alert.Peers) == 0 || !sort.StringsAreSorted(alert.Peers) || hasDuplicateStrings(alert.Peers) {
			return restoredState{}, fmt.Errorf("%w: alert peers", ErrBadSnapshot)
		}
		if _, exists := state.alerts[prefix]; exists {
			return restoredState{}, fmt.Errorf("%w: duplicate alert %s", ErrBadSnapshot, prefix)
		}

		alert.Prefix = prefix
		alert.Signals = cloneStrings(alert.Signals)
		alert.Peers = cloneStrings(alert.Peers)
		state.alerts[prefix] = alert
	}

	for prefix, since := range snapshot.FlapSince {
		canonical, _, err := parseCanonicalPrefix(prefix)
		if err != nil {
			return restoredState{}, fmt.Errorf("%w: flap prefix", ErrBadSnapshot)
		}
		state.flapSince[canonical] = since
	}

	stats := snapshot.Stats
	if stats.Accepted < 0 || stats.Pending < 0 || stats.Alerted < 0 ||
		stats.Withdrawn < 0 || stats.Dismissed < 0 || stats.Evicted < 0 ||
		stats.Flaps < 0 || stats.Alerts < 0 {
		return restoredState{}, fmt.Errorf("%w: negative stats", ErrBadSnapshot)
	}

	return state, nil
}

func validateSnapshotSignals(signals []string, score int) error {
	if !sort.StringsAreSorted(signals) || hasDuplicateStrings(signals) {
		return fmt.Errorf("%w: signal order", ErrBadSnapshot)
	}
	for _, signal := range signals {
		switch signal {
		case SignalOriginMismatch, SignalPathAnomaly, SignalROANotFound:
		default:
			return fmt.Errorf("%w: unknown signal %q", ErrBadSnapshot, signal)
		}
	}
	if scoreSignals(signals) != score {
		return fmt.Errorf("%w: signal score", ErrBadSnapshot)
	}
	return nil
}

func hasDuplicateStrings(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return true
		}
	}
	return false
}
