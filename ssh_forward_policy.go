// Package task enforces SSH server side port forwarding policy and leases
// reverse forwarding ports to live sessions.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Decision reason codes.
const (
	ReasonAllow            = "forward_allow"
	ReasonDeniedNotListed  = "forward_denied_not_listed"
	ReasonDeniedPort       = "forward_denied_port"
	ReasonDeniedCIDR       = "forward_denied_cidr"
	ReasonDeniedLoopback   = "forward_denied_loopback"
	ReasonDeniedMetadata   = "forward_denied_metadata"
	ReasonDeniedChannelCap = "forward_denied_channel_cap"
	ReasonDeniedByteCap    = "forward_denied_byte_cap"
)

// Session lifecycle states.
const (
	SessionActive = "active"
	SessionClosed = "closed"
)

// Errors surfaced by the policy.
var (
	ErrBlankSession       = errors.New("session id and tenant are required")
	ErrSessionExists      = errors.New("session already registered")
	ErrSessionNotFound    = errors.New("session not found")
	ErrTenantSessionLimit = errors.New("tenant session limit reached")
	ErrPortRange          = errors.New("requested port outside the configured range")
	ErrPortBusy           = errors.New("requested port already leased")
	ErrByteQuota          = errors.New("session byte quota exceeded")
	ErrBadHost            = errors.New("host cannot be normalized")
	ErrBadSnapshot        = errors.New("snapshot payload invalid")
	ErrPortExhausted      = fmt.Errorf("%w: port pool exhausted", ErrPortBusy)
	ErrQueueFull          = fmt.Errorf("%w: port allocation queue full", ErrPortExhausted)

	errStateRestored = errors.New("policy state was restored")
)

const snapshotVersion = 1

// HostPort is one direct-tcpip destination as requested by a client.
type HostPort struct {
	Host string
	Port int
}

// Decision is the answer for one direct-tcpip request.
type Decision struct {
	Allow  bool
	Reason string
	Target string
}

// RemoteForward is one -R style request from a session.
type RemoteForward struct {
	WantPort int
	Tenant   string
}

// BoundPort is one granted reverse forwarding lease.
type BoundPort struct {
	Port      int
	LeaseID   string
	SessionID string
	ExpiresAt time.Time
}

// Session is the live record of one SSH session.
type Session struct {
	ID         string
	Tenant     string
	Priority   int
	State      string
	Channels   int
	RemotePort int
	Bytes      int64
}

// TenantRule is the allow list and the denied networks of one tenant.
type TenantRule struct {
	Hosts         []string
	Ports         []int
	ForbiddenNets []string
}

// PolicyConfig bounds the live tables of the policy.
type PolicyConfig struct {
	MaxSessionsPerTenant  int
	MaxChannelsPerSession int
	MaxBytesPerSession    int64
	PortRangeLow          int
	PortRangeHigh         int
	LeaseTTL              time.Duration
	LeaseQueueDepth       int
	Rules                 map[string]TenantRule
	Resolver              func(host string) []string
}

// SshForwardPolicy owns sessions, the direct-tcpip rules and the port pool.
type SshForwardPolicy struct {
	cfg PolicyConfig

	mu       sync.Mutex
	sessions map[string]*sessionState
	ports    map[int]*leaseState
	waiters  []*portWaiter

	leaseSeq uint64
	queueSeq uint64
}

type sessionState struct {
	info  Session
	ports map[int]struct{}
}

type leaseState struct {
	bound BoundPort
}

type portWaiter struct {
	sessionID string
	priority  int
	seq       uint64
	ch        chan allocateResult
}

type allocateResult struct {
	bound BoundPort
	err   error
}

type snapshotData struct {
	Version  int         `json:"version"`
	Sessions []Session   `json:"sessions"`
	Leases   []BoundPort `json:"leases"`
	LeaseSeq uint64      `json:"lease_seq"`
}

// NewSshForwardPolicy builds a policy with the given caps.
func NewSshForwardPolicy(cfg PolicyConfig) *SshForwardPolicy {
	cfg = cloneConfig(cfg)
	if cfg.Resolver == nil {
		cfg.Resolver = lookupHostAddresses
	}

	return &SshForwardPolicy{
		cfg:      cfg,
		sessions: make(map[string]*sessionState),
		ports:    make(map[int]*leaseState),
	}
}

func cloneConfig(cfg PolicyConfig) PolicyConfig {
	cloned := cfg
	if cfg.Rules != nil {
		cloned.Rules = make(map[string]TenantRule, len(cfg.Rules))
		for tenant, rule := range cfg.Rules {
			rule.Hosts = append([]string(nil), rule.Hosts...)
			rule.Ports = append([]int(nil), rule.Ports...)
			rule.ForbiddenNets = append([]string(nil), rule.ForbiddenNets...)
			cloned.Rules[tenant] = rule
		}
	}
	return cloned
}

func lookupHostAddresses(host string) []string {
	addresses, err := net.LookupHost(host)
	if err != nil {
		return nil
	}
	return addresses
}

func sessionKey(sessionID string) string {
	return strings.TrimSpace(sessionID)
}

// RegisterSession records one authenticated session.
func (p *SshForwardPolicy) RegisterSession(sessionID, tenant string, priority int) (Session, error) {
	id := sessionKey(sessionID)
	tenant = strings.TrimSpace(tenant)
	if id == "" || tenant == "" {
		return Session{}, ErrBlankSession
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.sessions[id]; exists {
		return Session{}, ErrSessionExists
	}

	tenantSessions := 0
	for _, session := range p.sessions {
		if session.info.Tenant == tenant {
			tenantSessions++
		}
	}
	if tenantSessions >= p.cfg.MaxSessionsPerTenant {
		return Session{}, ErrTenantSessionLimit
	}

	session := &sessionState{
		info: Session{
			ID:       id,
			Tenant:   tenant,
			Priority: priority,
			State:    SessionActive,
		},
		ports: make(map[int]struct{}),
	}
	p.sessions[id] = session
	return session.info, nil
}

// CheckDirect admits or denies one direct-tcpip channel.
func (p *SshForwardPolicy) CheckDirect(sessionID string, dest HostPort) (Decision, error) {
	id := sessionKey(sessionID)

	p.mu.Lock()
	session, ok := p.sessions[id]
	if !ok {
		p.mu.Unlock()
		return Decision{}, ErrSessionNotFound
	}
	rule := p.cfg.Rules[session.info.Tenant]
	maxChannels := p.cfg.MaxChannelsPerSession
	p.mu.Unlock()

	host, err := normalizeHostValue(dest.Host)
	if err != nil {
		return Decision{}, err
	}

	decision, err := p.evaluateDestination(host, dest.Port, rule)
	if err != nil || !decision.Allow {
		return decision, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	session, ok = p.sessions[id]
	if !ok {
		return Decision{}, ErrSessionNotFound
	}
	if session.info.Channels >= maxChannels {
		return Decision{
			Allow:  false,
			Reason: ReasonDeniedChannelCap,
			Target: decision.Target,
		}, nil
	}

	session.info.Channels++
	return decision, nil
}

func (p *SshForwardPolicy) evaluateDestination(host string, port int, rule TenantRule) (Decision, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	deny := func(reason string) (Decision, error) {
		return Decision{Allow: false, Reason: reason, Target: target}, nil
	}

	ip := net.ParseIP(host)
	if ip != nil {
		if reason := classifyAddress(ip, rule); reason != "" {
			return deny(reason)
		}
	} else if isLoopbackName(host) {
		return deny(ReasonDeniedLoopback)
	}

	if !hostIsAllowed(host, rule.Hosts) {
		return deny(ReasonDeniedNotListed)
	}
	if !portIsAllowed(port, rule.Ports) {
		return deny(ReasonDeniedPort)
	}

	if ip == nil {
		resolver := p.cfg.Resolver
		if resolver == nil {
			resolver = lookupHostAddresses
		}
		for _, address := range resolver(host) {
			resolvedHost, err := normalizeHostValue(address)
			if err != nil {
				return Decision{}, fmt.Errorf("%w: resolver returned %q", ErrBadHost, address)
			}
			resolvedIP := net.ParseIP(resolvedHost)
			if resolvedIP == nil {
				return Decision{}, fmt.Errorf("%w: resolver returned non-IP address %q", ErrBadHost, address)
			}
			if reason := classifyAddress(resolvedIP, rule); reason != "" {
				return deny(reason)
			}
		}
	}

	return Decision{Allow: true, Reason: ReasonAllow, Target: target}, nil
}

func classifyAddress(ip net.IP, rule TenantRule) string {
	ip = embeddedIPv4(ip)
	if ip.IsLoopback() {
		return ReasonDeniedLoopback
	}
	if isMetadataAddress(ip) {
		return ReasonDeniedMetadata
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return ReasonDeniedMetadata
	}
	if forbiddenByCIDR(ip, rule.ForbiddenNets) {
		return ReasonDeniedCIDR
	}
	return ""
}

func embeddedIPv4(ip net.IP) net.IP {
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4
	}
	if len(ip) == net.IPv6len && ip[12] != 0 {
		for index := 0; index < 12; index++ {
			if ip[index] != 0 {
				return ip
			}
		}
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	return ip
}

func isLoopbackName(host string) bool {
	return host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		host == "ip6-localhost" ||
		host == "ip6-loopback"
}

func isMetadataAddress(ip net.IP) bool {
	knownMetadata := []string{
		"169.254.169.254",
		"100.100.100.200",
		"fd00:ec2::254",
	}
	for _, literal := range knownMetadata {
		if candidate := net.ParseIP(literal); candidate != nil && candidate.Equal(ip) {
			return true
		}
	}
	return false
}

func forbiddenByCIDR(ip net.IP, networks []string) bool {
	for _, raw := range networks {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.Contains(raw, "/") {
			_, network, err := net.ParseCIDR(raw)
			if err != nil {
				continue
			}
			if network.Contains(ip) {
				return true
			}
			continue
		}
		if candidate := net.ParseIP(raw); candidate != nil && candidate.Equal(ip) {
			return true
		}
	}
	return false
}

func hostIsAllowed(host string, allowed []string) bool {
	hostIP := net.ParseIP(host)
	for _, raw := range allowed {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}

		if strings.Contains(entry, "/") {
			if hostIP == nil {
				continue
			}
			_, network, err := net.ParseCIDR(entry)
			if err == nil && network.Contains(hostIP) {
				return true
			}
			continue
		}

		normalized, err := normalizeHostValue(entry)
		if err != nil {
			continue
		}
		if normalized == host {
			return true
		}
	}
	return false
}

func portIsAllowed(port int, allowed []int) bool {
	for _, candidate := range allowed {
		if candidate == port {
			return true
		}
	}
	return false
}

// AllocateRemote leases one reverse forwarding port to a session.
func (p *SshForwardPolicy) AllocateRemote(sessionID string, req RemoteForward) (BoundPort, error) {
	id := sessionKey(sessionID)

	p.mu.Lock()
	session, ok := p.sessions[id]
	if !ok {
		p.mu.Unlock()
		return BoundPort{}, ErrSessionNotFound
	}

	low := p.cfg.PortRangeLow
	high := p.cfg.PortRangeHigh
	wantPort := req.WantPort
	if wantPort < 0 || (wantPort != 0 && (wantPort < low || wantPort > high)) {
		p.mu.Unlock()
		return BoundPort{}, ErrPortRange
	}

	now := time.Now()
	if wantPort != 0 {
		if lease, exists := p.ports[wantPort]; exists {
			if lease.bound.SessionID != id {
				p.mu.Unlock()
				return BoundPort{}, ErrPortBusy
			}
			bound := lease.bound
			p.mu.Unlock()
			return bound, nil
		}

		bound := p.bindLocked(session, wantPort, now)
		p.mu.Unlock()
		return bound, nil
	}

	if port, available := p.freePortLocked(); available {
		bound := p.bindLocked(session, port, now)
		p.mu.Unlock()
		return bound, nil
	}

	waiter, err := p.enqueueLocked(session)
	if err != nil {
		p.mu.Unlock()
		return BoundPort{}, err
	}
	p.mu.Unlock()

	result := <-waiter.ch
	return result.bound, result.err
}

func (p *SshForwardPolicy) bindLocked(session *sessionState, port int, now time.Time) BoundPort {
	p.leaseSeq++
	bound := BoundPort{
		Port:      port,
		LeaseID:   "lease-" + strconv.FormatUint(p.leaseSeq, 10),
		SessionID: session.info.ID,
		ExpiresAt: now.Add(p.cfg.LeaseTTL),
	}

	p.ports[port] = &leaseState{bound: bound}
	session.ports[port] = struct{}{}
	session.info.RemotePort = port
	return bound
}

func (p *SshForwardPolicy) freePortLocked() (int, bool) {
	low := p.cfg.PortRangeLow
	high := p.cfg.PortRangeHigh
	if low > high {
		return 0, false
	}
	for port := low; port <= high; port++ {
		if _, used := p.ports[port]; !used {
			return port, true
		}
		if port == int(^uint(0)>>1) {
			break
		}
	}
	return 0, false
}

func (p *SshForwardPolicy) enqueueLocked(session *sessionState) (*portWaiter, error) {
	depth := p.cfg.LeaseQueueDepth
	if depth < 0 {
		depth = 0
	}

	if len(p.waiters) < depth {
		return p.addWaiterLocked(session), nil
	}
	if depth == 0 {
		return nil, ErrPortExhausted
	}

	lowestPriorityWaiter := p.waiters[len(p.waiters)-1]
	if session.info.Priority > lowestPriorityWaiter.priority {
		p.waiters = p.waiters[:len(p.waiters)-1]
		lowestPriorityWaiter.ch <- allocateResult{err: ErrQueueFull}
		return p.addWaiterLocked(session), nil
	}

	return nil, ErrPortExhausted
}

func (p *SshForwardPolicy) addWaiterLocked(session *sessionState) *portWaiter {
	p.queueSeq++
	waiter := &portWaiter{
		sessionID: session.info.ID,
		priority:  session.info.Priority,
		seq:       p.queueSeq,
		ch:        make(chan allocateResult, 1),
	}
	p.waiters = append(p.waiters, waiter)
	sort.SliceStable(p.waiters, func(i, j int) bool {
		if p.waiters[i].priority != p.waiters[j].priority {
			return p.waiters[i].priority > p.waiters[j].priority
		}
		return p.waiters[i].seq < p.waiters[j].seq
	})
	return waiter
}

func (p *SshForwardPolicy) grantWaitersLocked(now time.Time) {
	for len(p.waiters) > 0 {
		port, available := p.freePortLocked()
		if !available {
			return
		}

		waiter := p.waiters[0]
		p.waiters[0] = nil
		p.waiters = p.waiters[1:]

		session, exists := p.sessions[waiter.sessionID]
		if !exists {
			waiter.ch <- allocateResult{err: ErrSessionNotFound}
			continue
		}

		bound := p.bindLocked(session, port, now)
		waiter.ch <- allocateResult{bound: bound}
	}
}

func (p *SshForwardPolicy) cancelSessionWaitersLocked(sessionID string, err error) {
	kept := p.waiters[:0]
	for _, waiter := range p.waiters {
		if waiter.sessionID == sessionID {
			waiter.ch <- allocateResult{err: err}
		} else {
			kept = append(kept, waiter)
		}
	}
	p.waiters = kept
}

func (p *SshForwardPolicy) cancelAllWaitersLocked(err error) {
	for _, waiter := range p.waiters {
		waiter.ch <- allocateResult{err: err}
	}
	p.waiters = nil
}

// Release drops a session and every port lease it holds.
func (p *SshForwardPolicy) Release(sessionID string) error {
	id := sessionKey(sessionID)

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.sessions[id]; !exists {
		return nil
	}

	p.cancelSessionWaitersLocked(id, ErrSessionNotFound)
	delete(p.sessions, id)
	for port, lease := range p.ports {
		if lease.bound.SessionID == id {
			delete(p.ports, port)
		}
	}

	p.grantWaitersLocked(time.Now())
	return nil
}

// SessionState reads one session record.
func (p *SshForwardPolicy) SessionState(sessionID string) (Session, error) {
	id := sessionKey(sessionID)

	p.mu.Lock()
	defer p.mu.Unlock()

	session, ok := p.sessions[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return session.info, nil
}

// NormalizeHost canonicalises an address literal or a domain name.
func (p *SshForwardPolicy) NormalizeHost(host string) (string, error) {
	return normalizeHostValue(host)
}

func normalizeHostValue(host string) (string, error) {
	normalized := strings.TrimSpace(host)
	if normalized == "" {
		return "", ErrBadHost
	}

	if strings.HasPrefix(normalized, "[") {
		if bracketedHost, _, err := net.SplitHostPort(normalized); err == nil {
			normalized = bracketedHost
		} else {
			normalized = strings.TrimPrefix(normalized, "[")
			normalized = strings.TrimSuffix(normalized, "]")
		}
	}

	if ip := net.ParseIP(normalized); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
		return ip.String(), nil
	}

	if canonical, ok, err := normalizeLegacyIPv4(normalized); err != nil {
		return "", err
	} else if ok {
		return canonical, nil
	}

	normalized = strings.ToLower(strings.TrimSuffix(normalized, "."))
	if normalized == "" || !validHostName(normalized) {
		return "", ErrBadHost
	}
	return normalized, nil
}

func normalizeLegacyIPv4(host string) (string, bool, error) {
	if host == "" || strings.ContainsAny(host, ":/[]") {
		return "", false, nil
	}

	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(parts) < 1 || len(parts) > 4 {
		return "", false, nil
	}

	values := make([]uint64, len(parts))
	for index, part := range parts {
		if part == "" {
			return "", false, nil
		}
		value, err := parseLegacyIPv4Part(part)
		if err != nil {
			if looksNumericToken(part) {
				return "", true, fmt.Errorf("%w: invalid IPv4 literal %q", ErrBadHost, host)
			}
			return "", false, nil
		}
		values[index] = value
	}

	var address uint32
	switch len(values) {
	case 1:
		if values[0] > 0xffffffff {
			return "", true, ErrBadHost
		}
		address = uint32(values[0])
	case 2:
		if values[0] > 0xff || values[1] > 0xffffff {
			return "", true, ErrBadHost
		}
		address = uint32(values[0])<<24 | uint32(values[1])
	case 3:
		if values[0] > 0xff || values[1] > 0xff || values[2] > 0xffff {
			return "", true, ErrBadHost
		}
		address = uint32(values[0])<<24 | uint32(values[1])<<16 | uint32(values[2])
	case 4:
		for _, value := range values {
			if value > 0xff {
				return "", true, ErrBadHost
			}
		}
		address = uint32(values[0])<<24 | uint32(values[1])<<16 |
			uint32(values[2])<<8 | uint32(values[3])
	}

	ip := net.IPv4(
		byte(address>>24),
		byte(address>>16),
		byte(address>>8),
		byte(address),
	)
	return ip.String(), true, nil
}

func parseLegacyIPv4Part(part string) (uint64, error) {
	if strings.Contains(part, "_") {
		return 0, strconv.ErrSyntax
	}

	base := 10
	switch {
	case len(part) >= 2 && part[0] == '0' && (part[1] == 'x' || part[1] == 'X'):
		base = 16
	case len(part) > 1 && part[0] == '0':
		base = 8
	}
	return strconv.ParseUint(part, base, 64)
}

func looksNumericToken(part string) bool {
	if len(part) >= 2 && part[0] == '0' && (part[1] == 'x' || part[1] == 'X') {
		if len(part) == 2 {
			return false
		}
		for _, char := range part[2:] {
			if !isHexDigit(char) {
				return false
			}
		}
		return true
	}

	if part == "" {
		return false
	}
	for _, char := range part {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func isHexDigit(char rune) bool {
	return (char >= '0' && char <= '9') ||
		(char >= 'a' && char <= 'f') ||
		(char >= 'A' && char <= 'F')
}

func validHostName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}

	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, char := range label {
			if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '-' || char == '_' {
				continue
			}
			return false
		}
	}
	return true
}

// LeasePool returns a copy of every live port lease.
func (p *SshForwardPolicy) LeasePool() []BoundPort {
	p.mu.Lock()
	defer p.mu.Unlock()

	pool := make([]BoundPort, 0, len(p.ports))
	for _, lease := range p.ports {
		pool = append(pool, lease.bound)
	}
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].Port < pool[j].Port
	})
	return pool
}

// ReclaimExpired frees leases whose TTL elapsed and reports the count.
func (p *SshForwardPolicy) ReclaimExpired(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	reclaimed := 0
	for port, lease := range p.ports {
		if lease.bound.ExpiresAt.After(now) {
			continue
		}

		delete(p.ports, port)
		reclaimed++

		session, exists := p.sessions[lease.bound.SessionID]
		if !exists {
			continue
		}
		delete(session.ports, port)
		if session.info.RemotePort == port {
			session.info.RemotePort = minimumPort(session.ports)
		}
	}

	p.grantWaitersLocked(time.Now())
	return reclaimed
}

func minimumPort(ports map[int]struct{}) int {
	minimum := 0
	for port := range ports {
		if minimum == 0 || port < minimum {
			minimum = port
		}
	}
	return minimum
}

// SessionCount reports how many live sessions a tenant holds.
func (p *SshForwardPolicy) SessionCount(tenant string) int {
	tenant = strings.TrimSpace(tenant)

	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	for _, session := range p.sessions {
		if session.info.Tenant == tenant {
			count++
		}
	}
	return count
}

// ChargeBytes accounts forwarded payload against the session quota.
func (p *SshForwardPolicy) ChargeBytes(sessionID string, n int64) error {
	id := sessionKey(sessionID)
	if n < 0 {
		return errors.New("charge bytes must be non-negative")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	session, ok := p.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}

	quota := p.cfg.MaxBytesPerSession
	if session.info.Bytes > quota || n > quota-session.info.Bytes {
		return ErrByteQuota
	}

	session.info.Bytes += n
	return nil
}

// Snapshot serialises sessions, leases and the lease counter.
func (p *SshForwardPolicy) Snapshot() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := snapshotData{
		Version:  snapshotVersion,
		Sessions: make([]Session, 0, len(p.sessions)),
		Leases:   make([]BoundPort, 0, len(p.ports)),
		LeaseSeq: p.leaseSeq,
	}

	for _, session := range p.sessions {
		data.Sessions = append(data.Sessions, session.info)
	}
	sort.Slice(data.Sessions, func(i, j int) bool {
		return data.Sessions[i].ID < data.Sessions[j].ID
	})

	for _, lease := range p.ports {
		data.Leases = append(data.Leases, lease.bound)
	}
	sort.Slice(data.Leases, func(i, j int) bool {
		return data.Leases[i].Port < data.Leases[j].Port
	})

	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *SshForwardPolicy) Restore(data []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}

	sessions := make(map[string]*sessionState, len(snapshot.Sessions))
	for _, recorded := range snapshot.Sessions {
		info := recorded
		id := sessionKey(info.ID)
		tenant := strings.TrimSpace(info.Tenant)
		if id == "" || tenant == "" {
			return fmt.Errorf("%w: session id and tenant are required", ErrBadSnapshot)
		}
		if _, duplicate := sessions[id]; duplicate {
			return fmt.Errorf("%w: duplicate session %q", ErrBadSnapshot, id)
		}
		if info.Channels < 0 || info.Bytes < 0 {
			return fmt.Errorf("%w: negative session counter for %q", ErrBadSnapshot, id)
		}
		if info.State == "" {
			info.State = SessionActive
		}

		info.ID = id
		info.Tenant = tenant
		sessions[id] = &sessionState{
			info:  info,
			ports: make(map[int]struct{}),
		}
	}

	ports := make(map[int]*leaseState, len(snapshot.Leases))
	for _, bound := range snapshot.Leases {
		bound.SessionID = sessionKey(bound.SessionID)
		if bound.LeaseID == "" || bound.SessionID == "" {
			return fmt.Errorf("%w: lease is missing identity", ErrBadSnapshot)
		}
		if bound.Port < 0 {
			return fmt.Errorf("%w: negative lease port", ErrBadSnapshot)
		}
		session, exists := sessions[bound.SessionID]
		if !exists {
			return fmt.Errorf("%w: lease %q references missing session", ErrBadSnapshot, bound.LeaseID)
		}
		if _, duplicate := ports[bound.Port]; duplicate {
			return fmt.Errorf("%w: duplicate port %d", ErrBadSnapshot, bound.Port)
		}

		ports[bound.Port] = &leaseState{bound: bound}
		session.ports[bound.Port] = struct{}{}
	}

	leaseSeq := snapshot.LeaseSeq
	for _, bound := range snapshot.Leases {
		if strings.HasPrefix(bound.LeaseID, "lease-") {
			if sequence, err := strconv.ParseUint(strings.TrimPrefix(bound.LeaseID, "lease-"), 10, 64); err == nil && sequence > leaseSeq {
				leaseSeq = sequence
			}
		}
	}

	for id, session := range sessions {
		if session.info.RemotePort == 0 {
			session.info.RemotePort = minimumPort(session.ports)
			continue
		}
		if _, exists := session.ports[session.info.RemotePort]; !exists {
			return fmt.Errorf("%w: session %q references missing port %d", ErrBadSnapshot, id, session.info.RemotePort)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.cancelAllWaitersLocked(errStateRestored)
	p.sessions = sessions
	p.ports = ports
	p.waiters = nil
	p.leaseSeq = leaseSeq
	return nil
}
