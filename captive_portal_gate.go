// Package task runs the guest network admission gate: unauthorised clients are
// diverted to the portal, only the walled garden stays reachable before an
// authorisation lease is issued and bound to a device fingerprint.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client states tracked by the gate.
const (
	ClientUnknown    = "unknown"
	ClientPending    = "pending"
	ClientAuthorized = "authorized"
	ClientExpired    = "expired"
	ClientBlocked    = "blocked"
)

// Lease states tracked by the gate.
const (
	LeaseActive  = "active"
	LeaseExpired = "expired"
	LeaseRevoked = "revoked"
)

// Request phases at which the gate is consulted.
const (
	PhaseResolve = "resolve"
	PhaseConnect = "connect"
)

// Verdict actions.
const (
	ActionPortal = "portal"
	ActionAllow  = "allow"
	ActionDeny   = "deny"
)

// Reason codes attached to a verdict.
const (
	ReasonPortalIntercept = "reason_portal_intercept"
	ReasonWalledGarden    = "reason_walled_garden"
	ReasonAuthorized      = "reason_authorized"
	ReasonPortalAsset     = "reason_portal_asset"
	ReasonLeaseExpired    = "reason_lease_expired"
	ReasonBlocked         = "reason_blocked"
)

// Walled garden rule kinds.
const (
	RuleSuffix = "suffix"
	RuleCIDR   = "cidr"
)

// Errors returned by the gate.
var (
	ErrBlankClient    = errors.New("client id is required")
	ErrUnknownClient  = errors.New("client is not known to the gate")
	ErrDeviceMismatch = errors.New("device fingerprint does not match the client")
	ErrBadTTL         = errors.New("lease ttl must be positive")
	ErrBadRule        = errors.New("walled garden rule is invalid")
	ErrLeaseNotFound  = errors.New("lease not found")
	ErrLeaseNotActive = errors.New("lease is not active")
	ErrLeaseExpired   = errors.New("lease already expired")
	ErrClientBlocked  = errors.New("client is blocked")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
	ErrLeaseLimit     = errors.New("lease table is full")
	ErrRuleLimit      = errors.New("walled garden rule table is full")
)

const snapshotVersion = 1

// Limits bounds the gate tables and lease lifetime.
type Limits struct {
	LeaseTTL  time.Duration
	MaxTTL    time.Duration
	MaxRules  int
	MaxLeases int
}

// Rule is one walled garden entry: either a domain suffix or an address block.
type Rule struct {
	Kind  string
	Value string
}

// Client is one guest device tracked by the gate.
type Client struct {
	ID          string
	Fingerprint string
	Addr        string
	State       string
	LeaseID     string
	Attempts    int
	FirstSeen   time.Time
}

// Request is one intercepted name resolution or connection attempt.
type Request struct {
	Host  string
	Addr  string
	Path  string
	Phase string
	Now   time.Time
}

// Verdict is the decision returned for one intercepted request.
type Verdict struct {
	ClientID string
	Action   string
	Reason   string
	LeaseID  string
	Host     string
	Addr     string
	Phase    string
}

// Grant is one authorisation request.
type Grant struct {
	Fingerprint string
	TTL         time.Duration
	Now         time.Time
}

// Lease is one authorisation lease bound to a device fingerprint.
type Lease struct {
	ID          string
	ClientID    string
	Fingerprint string
	Issued      time.Time
	Expires     time.Time
	State       string
}

// CaptivePortalGate owns the client table, the lease table and the walled
// garden rules.
type CaptivePortalGate struct {
	mu      sync.RWMutex
	limits  Limits
	rules   []Rule
	clients map[string]*Client
	leases  map[string]*Lease
	seq     int64
}

type snapshotData struct {
	Version int      `json:"version"`
	Seq     int64    `json:"seq"`
	Rules   []Rule   `json:"rules"`
	Clients []Client `json:"clients"`
	Leases  []Lease  `json:"leases"`
}

// NewCaptivePortalGate builds a gate with the given tables and rules.
func NewCaptivePortalGate(limits Limits, rules []Rule) *CaptivePortalGate {
	return &CaptivePortalGate{limits: limits, rules: append([]Rule(nil), rules...), clients: map[string]*Client{}, leases: map[string]*Lease{}}
}

// Intercept decides what to do with one request from one client.
func (g *CaptivePortalGate) Intercept(client Client, req Request) (Verdict, error) {
	if strings.TrimSpace(client.ID) == "" {
		return Verdict{}, ErrBlankClient
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureMapsLocked()

	now := req.Now

	record, known := g.clients[client.ID]
	if !known {
		record = &Client{
			ID:        client.ID,
			State:     ClientPending,
			FirstSeen: now,
		}
		g.clients[client.ID] = record
	}

	record.Attempts++
	if record.Fingerprint == "" {
		record.Fingerprint = client.Fingerprint
	}
	if client.Addr != "" {
		record.Addr = client.Addr
	}

	verdict := Verdict{
		ClientID: record.ID,
		Host:     req.Host,
		Addr:     req.Addr,
		Phase:    req.Phase,
	}

	if isPortalAssetRequest(req) {
		verdict.Action = ActionAllow
		verdict.Reason = ReasonPortalAsset
		return verdict, nil
	}

	if record.State == ClientBlocked {
		verdict.Action = ActionDeny
		verdict.Reason = ReasonBlocked
		verdict.LeaseID = record.LeaseID
		return verdict, nil
	}

	if record.LeaseID != "" {
		if lease, ok := g.leases[record.LeaseID]; ok && lease != nil {
			switch lease.State {
			case LeaseActive:
				if !now.Before(lease.Expires) {
					lease.State = LeaseExpired
					record.State = ClientExpired
					verdict.Action = ActionPortal
					verdict.Reason = ReasonLeaseExpired
					verdict.LeaseID = lease.ID
					return verdict, nil
				}
				record.State = ClientAuthorized
				verdict.Action = ActionAllow
				verdict.Reason = ReasonAuthorized
				verdict.LeaseID = lease.ID
				return verdict, nil

			case LeaseExpired:
				record.State = ClientExpired
				verdict.Action = ActionPortal
				verdict.Reason = ReasonLeaseExpired
				verdict.LeaseID = lease.ID
				return verdict, nil

			case LeaseRevoked:
				if record.State != ClientBlocked {
					record.State = ClientPending
					record.LeaseID = ""
				}
			}
		}
	}

	if g.walledGardenMatchLocked(req) {
		verdict.Action = ActionAllow
		verdict.Reason = ReasonWalledGarden
		return verdict, nil
	}

	verdict.Action = ActionPortal
	verdict.Reason = ReasonPortalIntercept
	return verdict, nil
}

// Authorize issues a lease bound to the client device fingerprint.
func (g *CaptivePortalGate) Authorize(clientID string, grant Grant) (Lease, error) {
	if strings.TrimSpace(clientID) == "" {
		return Lease{}, ErrBlankClient
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureMapsLocked()

	client, ok := g.clients[clientID]
	if !ok || client == nil {
		return Lease{}, ErrUnknownClient
	}
	if grant.Fingerprint == "" || grant.Fingerprint != client.Fingerprint {
		return Lease{}, ErrDeviceMismatch
	}
	if grant.TTL <= 0 {
		return Lease{}, ErrBadTTL
	}
	if client.State == ClientBlocked {
		return Lease{}, ErrClientBlocked
	}

	ttl := grant.TTL
	if g.limits.MaxTTL > 0 && ttl > g.limits.MaxTTL {
		ttl = g.limits.MaxTTL
	}

	if g.limits.MaxLeases > 0 {
		activeOthers := 0
		for _, lease := range g.leases {
			if lease.State == LeaseActive &&
				lease.ClientID != clientID &&
				lease.Fingerprint != grant.Fingerprint {
				activeOthers++
			}
		}
		if activeOthers+1 > g.limits.MaxLeases {
			return Lease{}, ErrLeaseLimit
		}
	}

	for _, lease := range g.leases {
		if lease.State != LeaseActive {
			continue
		}
		if lease.ClientID == clientID || lease.Fingerprint == grant.Fingerprint {
			if !grant.Now.Before(lease.Expires) {
				lease.State = LeaseExpired
				if oldClient, ok := g.clients[lease.ClientID]; ok && oldClient != nil && oldClient.LeaseID == lease.ID {
					oldClient.State = ClientExpired
				}
				continue
			}

			lease.State = LeaseRevoked
			if oldClient, ok := g.clients[lease.ClientID]; ok && oldClient != nil && oldClient.LeaseID == lease.ID {
				oldClient.State = ClientPending
				oldClient.LeaseID = ""
			}
		}
	}

	g.seq++
	lease := Lease{
		ID:          fmt.Sprintf("lease-%d", g.seq),
		ClientID:    clientID,
		Fingerprint: grant.Fingerprint,
		Issued:      grant.Now,
		Expires:     grant.Now.Add(ttl),
		State:       LeaseActive,
	}
	g.leases[lease.ID] = &lease
	client.State = ClientAuthorized
	client.LeaseID = lease.ID

	return lease, nil
}

// WalledGarden returns a copy of the configured rules in configuration order.
func (g *CaptivePortalGate) WalledGarden() []Rule {
	g.mu.RLock()
	defer g.mu.RUnlock()

	return append([]Rule(nil), g.rules...)
}

// Revoke kills one lease and blocks the client behind it.
func (g *CaptivePortalGate) Revoke(leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return ErrLeaseNotFound
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureMapsLocked()

	lease, ok := g.leases[leaseID]
	if !ok || lease == nil {
		return ErrLeaseNotFound
	}
	if lease.State != LeaseActive {
		return ErrLeaseNotActive
	}

	lease.State = LeaseRevoked
	if client, ok := g.clients[lease.ClientID]; ok && client != nil {
		client.State = ClientBlocked
	}
	return nil
}

// Expire closes every lease whose deadline has passed and reports how many.
func (g *CaptivePortalGate) Expire(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureMapsLocked()

	closed := 0
	for _, lease := range g.leases {
		if lease == nil || lease.State != LeaseActive {
			continue
		}
		if now.Before(lease.Expires) {
			continue
		}

		lease.State = LeaseExpired
		closed++
		if client, ok := g.clients[lease.ClientID]; ok && client != nil && client.LeaseID == lease.ID {
			client.State = ClientExpired
		}
	}
	return closed
}

// Validate re-checks a lease for an already established connection.
func (g *CaptivePortalGate) Validate(leaseID string, now time.Time) error {
	if strings.TrimSpace(leaseID) == "" {
		return ErrLeaseNotFound
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	lease, ok := g.leases[leaseID]
	if !ok || lease == nil {
		return ErrLeaseNotFound
	}

	switch lease.State {
	case LeaseRevoked:
		return ErrLeaseNotActive
	case LeaseExpired:
		return ErrLeaseExpired
	case LeaseActive:
		if !now.Before(lease.Expires) {
			return ErrLeaseExpired
		}
		return nil
	default:
		return ErrLeaseNotActive
	}
}

// ClientOf reads one client record.
func (g *CaptivePortalGate) ClientOf(clientID string) (Client, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	client, ok := g.clients[clientID]
	if !ok || client == nil {
		return Client{}, false
	}
	return *client, true
}

// LeaseOf reads one lease record.
func (g *CaptivePortalGate) LeaseOf(leaseID string) (Lease, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	lease, ok := g.leases[leaseID]
	if !ok || lease == nil {
		return Lease{}, false
	}
	return *lease, true
}

// Leases returns every lease record, ordered by lease id.
func (g *CaptivePortalGate) Leases() []Lease {
	g.mu.RLock()
	defer g.mu.RUnlock()

	ids := make([]string, 0, len(g.leases))
	for id := range g.leases {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	leases := make([]Lease, 0, len(ids))
	for _, id := range ids {
		if lease := g.leases[id]; lease != nil {
			leases = append(leases, *lease)
		}
	}
	return leases
}

// Snapshot serialises clients, leases, rules and the sequence counter.
func (g *CaptivePortalGate) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	data := snapshotData{
		Version: snapshotVersion,
		Seq:     g.seq,
		Rules:   append([]Rule(nil), g.rules...),
		Clients: make([]Client, 0, len(g.clients)),
		Leases:  make([]Lease, 0, len(g.leases)),
	}

	clientIDs := make([]string, 0, len(g.clients))
	for id := range g.clients {
		clientIDs = append(clientIDs, id)
	}
	sort.Strings(clientIDs)
	for _, id := range clientIDs {
		if client := g.clients[id]; client != nil {
			data.Clients = append(data.Clients, *client)
		}
	}

	leaseIDs := make([]string, 0, len(g.leases))
	for id := range g.leases {
		leaseIDs = append(leaseIDs, id)
	}
	sort.Strings(leaseIDs)
	for _, id := range leaseIDs {
		if lease := g.leases[id]; lease != nil {
			data.Leases = append(data.Leases, *lease)
		}
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *CaptivePortalGate) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("%w: empty snapshot", ErrBadSnapshot)
	}

	var saved snapshotData
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if saved.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, saved.Version)
	}
	if saved.Seq < 0 {
		return fmt.Errorf("%w: negative sequence", ErrBadSnapshot)
	}

	clients := make(map[string]*Client, len(saved.Clients))
	for i := range saved.Clients {
		client := saved.Clients[i]
		if strings.TrimSpace(client.ID) == "" {
			return fmt.Errorf("%w: client id is empty", ErrBadSnapshot)
		}
		if _, exists := clients[client.ID]; exists {
			return fmt.Errorf("%w: duplicate client %q", ErrBadSnapshot, client.ID)
		}
		if !validClientState(client.State) {
			return fmt.Errorf("%w: bad client state %q", ErrBadSnapshot, client.State)
		}
		if client.Attempts < 0 {
			return fmt.Errorf("%w: negative attempts for %q", ErrBadSnapshot, client.ID)
		}
		clients[client.ID] = &client
	}

	leases := make(map[string]*Lease, len(saved.Leases))
	for i := range saved.Leases {
		lease := saved.Leases[i]
		if strings.TrimSpace(lease.ID) == "" {
			return fmt.Errorf("%w: lease id is empty", ErrBadSnapshot)
		}
		if _, exists := leases[lease.ID]; exists {
			return fmt.Errorf("%w: duplicate lease %q", ErrBadSnapshot, lease.ID)
		}
		if !validLeaseState(lease.State) {
			return fmt.Errorf("%w: bad lease state %q", ErrBadSnapshot, lease.State)
		}
		if strings.TrimSpace(lease.ClientID) == "" {
			return fmt.Errorf("%w: lease %q has no client", ErrBadSnapshot, lease.ID)
		}
		client, ok := clients[lease.ClientID]
		if !ok || client == nil {
			return fmt.Errorf("%w: lease %q references missing client %q", ErrBadSnapshot, lease.ID, lease.ClientID)
		}
		if lease.Expires.Before(lease.Issued) {
			return fmt.Errorf("%w: lease %q expires before issue", ErrBadSnapshot, lease.ID)
		}
		leases[lease.ID] = &lease
	}

	for id, client := range clients {
		if client.LeaseID == "" {
			continue
		}
		lease, ok := leases[client.LeaseID]
		if !ok || lease == nil {
			return fmt.Errorf("%w: client %q references missing lease %q", ErrBadSnapshot, id, client.LeaseID)
		}
		if lease.ClientID != id {
			return fmt.Errorf("%w: client %q and lease %q disagree", ErrBadSnapshot, id, lease.ID)
		}
	}

	seq := saved.Seq
	for id := range leases {
		if number := leaseSequenceNumber(id); number > seq {
			seq = number
		}
	}
	rules := append([]Rule(nil), saved.Rules...)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.rules = rules
	g.clients = clients
	g.leases = leases
	g.seq = seq
	return nil
}

func (g *CaptivePortalGate) ensureMapsLocked() {
	if g.clients == nil {
		g.clients = make(map[string]*Client)
	}
	if g.leases == nil {
		g.leases = make(map[string]*Lease)
	}
}

func (g *CaptivePortalGate) walledGardenMatchLocked(req Request) bool {
	host := normalizeHost(req.Host)
	requestIP := parseIP(req.Addr)

	for _, rule := range g.rules {
		switch strings.ToLower(strings.TrimSpace(rule.Kind)) {
		case RuleSuffix:
			if domainSuffixMatch(host, rule.Value) {
				return true
			}
		case RuleCIDR:
			if req.Phase != PhaseConnect || requestIP == nil {
				continue
			}
			value := strings.TrimSpace(rule.Value)
			if _, network, err := net.ParseCIDR(value); err == nil && network.Contains(requestIP) {
				return true
			}
		}
	}
	return false
}

func isPortalAssetRequest(req Request) bool {
	raw := strings.TrimSpace(req.Path)
	if raw == "" {
		return false
	}
	if cut := strings.IndexAny(raw, "?#"); cut >= 0 {
		raw = raw[:cut]
	}
	raw = strings.ReplaceAll(raw, "\\", "/")
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}

	cleaned := strings.ToLower(path.Clean(raw))
	if cleaned == "/favicon.ico" {
		return true
	}

	segments := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	for _, segment := range segments {
		if segment == "assets" || segment == "asset" || segment == "static" || strings.Contains(segment, "assets") {
			return true
		}
	}

	switch path.Ext(cleaned) {
	case ".css", ".js", ".mjs", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".woff", ".woff2", ".ttf", ".eot", ".map":
		return true
	default:
		return false
	}
}

func normalizeHost(value string) string {
	host := strings.TrimSpace(value)
	if host == "" {
		return ""
	}
	if onlyHost, _, err := net.SplitHostPort(host); err == nil {
		host = onlyHost
	} else {
		host = strings.Trim(host, "[]")
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func normalizedDomainRule(value string) string {
	domain := normalizeHost(value)
	domain = strings.TrimPrefix(domain, "*.")
	return strings.TrimPrefix(domain, ".")
}

func domainSuffixMatch(host, suffix string) bool {
	normalizedHost := normalizeHost(host)
	normalizedSuffix := normalizedDomainRule(suffix)
	if normalizedHost == "" || normalizedSuffix == "" {
		return false
	}
	return normalizedHost == normalizedSuffix || strings.HasSuffix(normalizedHost, "."+normalizedSuffix)
}

func parseIP(value string) net.IP {
	address := strings.TrimSpace(value)
	if address == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	} else {
		address = strings.Trim(address, "[]")
	}
	return net.ParseIP(address)
}

func validClientState(state string) bool {
	switch state {
	case ClientUnknown, ClientPending, ClientAuthorized, ClientExpired, ClientBlocked:
		return true
	default:
		return false
	}
}

func validLeaseState(state string) bool {
	switch state {
	case LeaseActive, LeaseExpired, LeaseRevoked:
		return true
	default:
		return false
	}
}

func leaseSequenceNumber(id string) int64 {
	if !strings.HasPrefix(id, "lease-") {
		return 0
	}
	number, err := strconv.ParseInt(strings.TrimPrefix(id, "lease-"), 10, 64)
	if err != nil || number < 0 {
		return 0
	}
	return number
}
