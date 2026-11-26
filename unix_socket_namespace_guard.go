// Package task hands out Unix domain sockets to sandbox jobs and constrains
// which peers may connect, how long an allocation lives and how many
// descriptors a live connection may pass along.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Socket namespaces supported by the guard.
const (
	NamespaceAbstract   = "abstract"
	NamespaceFilesystem = "filesystem"
)

// Binding states reported by Audit and BindingsFor.
const (
	BindingBound    = "bound"
	BindingReleased = "released"
	BindingExpired  = "expired"
)

// Descriptor classes a connection may attempt to pass.
const (
	FDSealedMemfd = "memfd-sealed"
	FDPidfd       = "pidfd"
	FDEventfd     = "eventfd"
	FDRegularFile = "regular-file"
	FDPipe        = "pipe"
)

// Errors returned by the guard.
var (
	ErrBadName        = errors.New("socket name is not acceptable")
	ErrNameTaken      = errors.New("socket name is already bound")
	ErrNoBinding      = errors.New("socket binding not found")
	ErrTenantMismatch = errors.New("caller tenant does not own the socket")
	ErrPeerUntrusted  = errors.New("peer credentials are not trusted for this tenant")
	ErrFDKindDenied   = errors.New("descriptor class is not allowed")
	ErrFDBudget       = errors.New("descriptor budget exhausted")
	ErrSocketLimit    = errors.New("tenant socket limit reached")
	ErrConnLimit      = errors.New("tenant connection limit reached")
	ErrLeaseExpired   = errors.New("binding lease expired")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

const (
	abstractPrefix             = "@gsb/"
	maxAbstractSocketNameBytes = 108
	snapshotFormatVersion      = 1
)

// Options configures the guard. PeerReader is the seam through which the
// kernel peer credentials are read; production fills it with a SO_PEERCRED
// lookup, verification runs replace it with a fixed reader.
type Options struct {
	TenantSocketLimit int
	TenantConnLimit   int
	FDMaxPerConn      int
	FDBytesPerConn    int
	LeaseTTL          time.Duration
	PeerReader        func(tenantID, name string) PeerCred
}

// PeerCred is the peer identity collected right after accept.
type PeerCred struct {
	PID    int
	UID    int
	GID    int
	Cgroup string
	Tenant string
}

// PeerPolicy is applied on the connection side. MaxFDs and MaxFDBytes cap the
// descriptors one connection may pass, AllowedKinds is the class whitelist and
// RequireSameCgroup demands that the peer cgroup carries the owning tenant.
type PeerPolicy struct {
	RequireSameCgroup bool
	AllowedKinds      []string
	MaxFDs            int
	MaxFDBytes        int
}

// Endpoint is the listening side allocation returned by Listen.
type Endpoint struct {
	TenantID     string    `json:"tenant_id"`
	JobID        string    `json:"job_id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	Path         string    `json:"path"`
	LeaseExpires time.Time `json:"lease_expires"`
}

// Conn is one accepted connection.
type Conn struct {
	ID       string   `json:"id"`
	TenantID string   `json:"tenant_id"`
	Name     string   `json:"name"`
	Cred     PeerCred `json:"cred"`
	FDs      int      `json:"fds"`
	Bytes    int      `json:"bytes"`
}

// Binding is one audit row.
type Binding struct {
	TenantID string `json:"tenant_id"`
	JobID    string `json:"job_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Conns    int    `json:"conns"`
	FDs      int    `json:"fds"`
	Seq      int64  `json:"seq"`
}

type fdCharge struct {
	Kind       string    `json:"kind"`
	Size       int       `json:"size"`
	At         time.Time `json:"at"`
	Generation uint64    `json:"generation"`
	Holder     string    `json:"holder"`
}

type fdScope struct {
	FDs   int `json:"fds"`
	Bytes int `json:"bytes"`
}

type connState struct {
	Conn
	policy      PeerPolicy
	openedAt    time.Time
	generation  uint64
	holder      string
	charges     []fdCharge
	holders     map[string]*fdScope
	generations map[uint64]*fdScope
}

type bindingState struct {
	Binding
	leaseExpires time.Time
	conns        map[string]*connState
}

// UnixSocketNamespaceGuard owns the name registry, the connection counters and
// the descriptor budgets.
type UnixSocketNamespaceGuard struct {
	mu            sync.Mutex
	opts          Options
	seq           int64
	nextConnID    uint64
	bindings      map[string]*bindingState
	conns         map[string]*connState
	tenantSockets map[string]int
	tenantConns   map[string]int
}

// NewUnixSocketNamespaceGuard builds a guard with the given options.
func NewUnixSocketNamespaceGuard(opts Options) *UnixSocketNamespaceGuard {
	g := &UnixSocketNamespaceGuard{opts: opts}
	g.initLocked()
	return g
}

func (g *UnixSocketNamespaceGuard) initLocked() {
	if g.bindings == nil {
		g.bindings = make(map[string]*bindingState)
	}
	if g.conns == nil {
		g.conns = make(map[string]*connState)
	}
	if g.tenantSockets == nil {
		g.tenantSockets = make(map[string]int)
	}
	if g.tenantConns == nil {
		g.tenantConns = make(map[string]int)
	}
}

func encodeSocketName(tenantID, jobID, name string) (string, error) {
	parts := []string{tenantID, jobID, name}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || strings.ContainsAny(part, "/\x00") {
			return "", ErrBadName
		}
	}

	encoded := abstractPrefix + tenantID + "/" + jobID + "/" + name
	if len(encoded) > maxAbstractSocketNameBytes {
		return "", ErrBadName
	}
	return encoded, nil
}

func parseSocketName(fullName string) (tenantID, jobID, name string, err error) {
	if !strings.HasPrefix(fullName, abstractPrefix) {
		return "", "", "", ErrBadName
	}
	segments := strings.Split(strings.TrimPrefix(fullName, abstractPrefix), "/")
	if len(segments) != 3 {
		return "", "", "", ErrBadName
	}
	if _, err := encodeSocketName(segments[0], segments[1], segments[2]); err != nil {
		return "", "", "", err
	}
	return segments[0], segments[1], segments[2], nil
}

func canonicalDialName(name string) string {
	if strings.HasPrefix(name, "gsb/") {
		return "@" + name
	}
	return name
}

func cgroupHasTenant(cgroup, tenantID string) bool {
	if tenantID == "" {
		return false
	}
	segments := strings.Split(cgroup, "/")
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "gsb" && segments[i+1] == tenantID {
			return true
		}
	}
	return false
}

func effectiveCap(primary, fallback int) int {
	best := 0
	for _, candidate := range []int{primary, fallback} {
		if candidate > 0 && (best == 0 || candidate < best) {
			best = candidate
		}
	}
	return best
}

func remainingQuota(used, limit int) int {
	if limit <= 0 {
		return -1
	}
	remaining := limit - used
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Listen reserves a socket name for one job and returns the endpoint.
func (g *UnixSocketNamespaceGuard) Listen(tenantID, jobID, name string) (Endpoint, error) {
	fullName, err := encodeSocketName(tenantID, jobID, name)
	if err != nil {
		return Endpoint{}, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	if _, exists := g.bindings[fullName]; exists {
		return Endpoint{}, ErrNameTaken
	}
	if g.opts.TenantSocketLimit > 0 && g.tenantSockets[tenantID] >= g.opts.TenantSocketLimit {
		return Endpoint{}, ErrSocketLimit
	}

	g.seq++
	now := time.Now()
	expires := now.Add(g.opts.LeaseTTL)
	binding := &bindingState{
		Binding: Binding{
			TenantID: tenantID,
			JobID:    jobID,
			Name:     fullName,
			Kind:     NamespaceAbstract,
			State:    BindingBound,
			Seq:      g.seq,
		},
		leaseExpires: expires,
		conns:        make(map[string]*connState),
	}
	g.bindings[fullName] = binding
	g.tenantSockets[tenantID]++

	return Endpoint{
		TenantID:     tenantID,
		JobID:        jobID,
		Name:         fullName,
		Kind:         NamespaceAbstract,
		Path:         "",
		LeaseExpires: expires,
	}, nil
}

// Dial connects to a bound name on behalf of a tenant and applies the peer
// policy to the recorded credentials.
func (g *UnixSocketNamespaceGuard) Dial(tenantID, name string, policy PeerPolicy) (Conn, error) {
	lookupName := canonicalDialName(name)

	g.mu.Lock()
	g.initLocked()
	binding, exists := g.bindings[lookupName]
	if !exists {
		g.mu.Unlock()
		return Conn{}, ErrNoBinding
	}
	if binding.TenantID != tenantID {
		g.mu.Unlock()
		return Conn{}, ErrTenantMismatch
	}
	if !binding.leaseExpires.After(time.Now()) {
		g.mu.Unlock()
		return Conn{}, ErrLeaseExpired
	}
	reader := g.opts.PeerReader
	g.mu.Unlock()

	cred := PeerCred{}
	if reader != nil {
		cred = reader(tenantID, name)
	}
	cred.Tenant = tenantID
	if cred.PID <= 0 {
		return Conn{}, ErrPeerUntrusted
	}
	if policy.RequireSameCgroup && !cgroupHasTenant(cred.Cgroup, tenantID) {
		return Conn{}, ErrPeerUntrusted
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	binding, exists = g.bindings[lookupName]
	if !exists {
		return Conn{}, ErrNoBinding
	}
	if binding.TenantID != tenantID {
		return Conn{}, ErrTenantMismatch
	}
	if !binding.leaseExpires.After(time.Now()) {
		return Conn{}, ErrLeaseExpired
	}
	if g.opts.TenantConnLimit > 0 && g.tenantConns[tenantID] >= g.opts.TenantConnLimit {
		return Conn{}, ErrConnLimit
	}

	g.nextConnID++
	connID := "conn-" + strconv.FormatUint(g.nextConnID, 10)
	policyCopy := policy
	if policy.AllowedKinds != nil {
		policyCopy.AllowedKinds = append([]string(nil), policy.AllowedKinds...)
	}
	now := time.Now()
	conn := &connState{
		Conn: Conn{
			ID:       connID,
			TenantID: tenantID,
			Name:     lookupName,
			Cred:     cred,
		},
		policy:      policyCopy,
		openedAt:    now,
		generation:  uint64(binding.Seq),
		holder:      "pid:" + strconv.Itoa(cred.PID) + ":uid:" + strconv.Itoa(cred.UID),
		charges:     make([]fdCharge, 0),
		holders:     make(map[string]*fdScope),
		generations: make(map[uint64]*fdScope),
	}
	binding.conns[connID] = conn
	g.conns[connID] = conn
	g.tenantConns[tenantID]++

	return conn.Conn, nil
}

func (g *UnixSocketNamespaceGuard) removeBindingLocked(fullName string) {
	binding := g.bindings[fullName]
	if binding == nil {
		return
	}

	if g.tenantSockets[binding.TenantID] > 0 {
		g.tenantSockets[binding.TenantID]--
		if g.tenantSockets[binding.TenantID] == 0 {
			delete(g.tenantSockets, binding.TenantID)
		}
	}

	connCount := len(binding.conns)
	if connCount > 0 {
		for connID := range binding.conns {
			delete(g.conns, connID)
		}
		remaining := g.tenantConns[binding.TenantID] - connCount
		if remaining <= 0 {
			delete(g.tenantConns, binding.TenantID)
		} else {
			g.tenantConns[binding.TenantID] = remaining
		}
	}

	delete(g.bindings, fullName)
}

// Release drops every binding of one job and frees its quotas.
func (g *UnixSocketNamespaceGuard) Release(jobID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	names := make([]string, 0)
	for name, binding := range g.bindings {
		if binding.JobID == jobID {
			names = append(names, name)
		}
	}
	for _, name := range names {
		g.removeBindingLocked(name)
	}
	return nil
}

// Audit returns a stable copy of every binding the guard knows about.
func (g *UnixSocketNamespaceGuard) Audit() []Binding {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	rows := make([]Binding, 0, len(g.bindings))
	for _, binding := range g.bindings {
		row := binding.Binding
		row.State = BindingBound
		row.Kind = NamespaceAbstract
		row.Conns = len(binding.conns)
		row.FDs = 0
		for _, conn := range binding.conns {
			row.FDs += conn.FDs
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Seq < rows[j].Seq
	})
	return rows
}

// BindingsFor returns the bindings of one tenant.
func (g *UnixSocketNamespaceGuard) BindingsFor(tenantID string) []Binding {
	all := g.Audit()
	rows := make([]Binding, 0)
	for _, row := range all {
		if row.TenantID == tenantID {
			rows = append(rows, row)
		}
	}
	return rows
}

// PeerIdentity reads back the credentials recorded for one connection.
func (g *UnixSocketNamespaceGuard) PeerIdentity(connID string) (PeerCred, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	conn, ok := g.conns[connID]
	if !ok {
		return PeerCred{}, ErrNoBinding
	}
	return conn.Cred, nil
}

// AdmitFD charges one descriptor against the connection budget.
func (g *UnixSocketNamespaceGuard) AdmitFD(connID, kind string, size int, now time.Time) error {
	return g.admitFD(connID, kind, size, now, "", 0)
}

func (g *UnixSocketNamespaceGuard) admitFD(connID, kind string, size int, now time.Time, holder string, generation uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	conn, ok := g.conns[connID]
	if !ok {
		return ErrNoBinding
	}
	if !fdKindAllowed(conn.policy.AllowedKinds, kind) {
		return ErrFDKindDenied
	}
	if size < 0 {
		return ErrFDBudget
	}

	maxFDs := effectiveCap(conn.policy.MaxFDs, g.opts.FDMaxPerConn)
	maxBytes := effectiveCap(conn.policy.MaxFDBytes, g.opts.FDBytesPerConn)
	if maxFDs > 0 && conn.FDs >= maxFDs {
		return ErrFDBudget
	}
	if maxBytes > 0 && conn.Bytes+size > maxBytes {
		return ErrFDBudget
	}

	if holder == "" {
		holder = conn.holder
	}
	if generation == 0 {
		generation = conn.generation
	}

	holderUsage := conn.holders[holder]
	if holderUsage == nil {
		holderUsage = &fdScope{}
		conn.holders[holder] = holderUsage
	}
	generationUsage := conn.generations[generation]
	if generationUsage == nil {
		generationUsage = &fdScope{}
		conn.generations[generation] = generationUsage
	}

	charge := fdCharge{Kind: kind, Size: size, At: now, Generation: generation, Holder: holder}
	conn.charges = append(conn.charges, charge)
	conn.FDs++
	conn.Bytes += size
	holderUsage.FDs++
	holderUsage.Bytes += size
	generationUsage.FDs++
	generationUsage.Bytes += size
	return nil
}

func fdKindAllowed(allowed []string, kind string) bool {
	for _, candidate := range allowed {
		if candidate == kind {
			return true
		}
	}
	return false
}

// FDBudgetLeft reports the remaining byte budget of one connection.
func (g *UnixSocketNamespaceGuard) FDBudgetLeft(connID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	conn, ok := g.conns[connID]
	if !ok {
		return 0
	}
	maxBytes := effectiveCap(conn.policy.MaxFDBytes, g.opts.FDBytesPerConn)
	if maxBytes <= 0 {
		return int(^uint(0) >> 1)
	}
	left := maxBytes - conn.Bytes
	if left < 0 {
		return 0
	}
	return left
}

// ExpireLeases reclaims bindings whose lease ran out and reports their names.
func (g *UnixSocketNamespaceGuard) ExpireLeases(now time.Time) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	expired := make([]*bindingState, 0)
	for _, binding := range g.bindings {
		if !binding.leaseExpires.After(now) {
			expired = append(expired, binding)
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		return expired[i].Seq < expired[j].Seq
	})

	names := make([]string, 0, len(expired))
	for _, binding := range expired {
		names = append(names, binding.Name)
		g.removeBindingLocked(binding.Name)
	}
	return names
}

type optionsSnapshot struct {
	TenantSocketLimit int           `json:"tenant_socket_limit"`
	TenantConnLimit   int           `json:"tenant_conn_limit"`
	FDMaxPerConn      int           `json:"fd_max_per_conn"`
	FDBytesPerConn    int           `json:"fd_bytes_per_conn"`
	LeaseTTL          time.Duration `json:"lease_ttl"`
}

type quotaSnapshot struct {
	Sockets              int `json:"sockets"`
	Connections          int `json:"connections"`
	SocketLimit          int `json:"socket_limit"`
	ConnLimit            int `json:"conn_limit"`
	SocketsRemaining     int `json:"sockets_remaining"`
	ConnectionsRemaining int `json:"connections_remaining"`
}

type bindingSnapshot struct {
	TenantID     string    `json:"tenant_id"`
	JobID        string    `json:"job_id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	State        string    `json:"state"`
	Seq          int64     `json:"seq"`
	LeaseExpires time.Time `json:"lease_expires"`
	ConnIDs      []string  `json:"conn_ids"`
}

type connSnapshot struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"`
	BindingName string     `json:"binding_name"`
	Cred        PeerCred   `json:"cred"`
	Policy      PeerPolicy `json:"policy"`
	FDs         int        `json:"fds"`
	Bytes       int        `json:"bytes"`
	OpenedAt    time.Time  `json:"opened_at"`
	Generation  uint64     `json:"generation"`
	Holder      string     `json:"holder"`
	Charges     []fdCharge `json:"charges"`
}

type snapshotFile struct {
	Version     int                      `json:"version"`
	Sequence    int64                    `json:"sequence"`
	NextConnID  uint64                   `json:"next_conn_id"`
	Options     optionsSnapshot          `json:"options"`
	Quotas      map[string]quotaSnapshot `json:"quotas"`
	Bindings    []bindingSnapshot        `json:"bindings"`
	Connections []connSnapshot           `json:"connections"`
}

type restoredState struct {
	seq           int64
	nextConnID    uint64
	bindings      map[string]*bindingState
	conns         map[string]*connState
	tenantSockets map[string]int
	tenantConns   map[string]int
}

// Snapshot serialises the whole registry.
func (g *UnixSocketNamespaceGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()

	quotas := make(map[string]quotaSnapshot)
	for tenantID, used := range g.tenantSockets {
		quotas[tenantID] = quotaSnapshot{Sockets: used}
	}
	for tenantID, used := range g.tenantConns {
		quota := quotas[tenantID]
		quota.Connections = used
		quotas[tenantID] = quota
	}
	for tenantID, quota := range quotas {
		quota.SocketLimit = g.opts.TenantSocketLimit
		quota.ConnLimit = g.opts.TenantConnLimit
		quota.SocketsRemaining = remainingQuota(quota.Sockets, g.opts.TenantSocketLimit)
		quota.ConnectionsRemaining = remainingQuota(quota.Connections, g.opts.TenantConnLimit)
		quotas[tenantID] = quota
	}

	orderedBindings := make([]*bindingState, 0, len(g.bindings))
	for _, binding := range g.bindings {
		orderedBindings = append(orderedBindings, binding)
	}
	sort.Slice(orderedBindings, func(i, j int) bool {
		return orderedBindings[i].Seq < orderedBindings[j].Seq
	})

	bindings := make([]bindingSnapshot, 0, len(orderedBindings))
	connections := make([]connSnapshot, 0)
	for _, binding := range orderedBindings {
		connIDs := make([]string, 0, len(binding.conns))
		for connID := range binding.conns {
			connIDs = append(connIDs, connID)
		}
		sort.Strings(connIDs)
		bindings = append(bindings, bindingSnapshot{
			TenantID:     binding.TenantID,
			JobID:        binding.JobID,
			Name:         binding.Name,
			Kind:         binding.Kind,
			State:        BindingBound,
			Seq:          binding.Seq,
			LeaseExpires: binding.leaseExpires,
			ConnIDs:      connIDs,
		})

		for _, conn := range binding.conns {
			charges := append([]fdCharge(nil), conn.charges...)
			connections = append(connections, connSnapshot{
				ID:          conn.ID,
				TenantID:    conn.TenantID,
				BindingName: binding.Name,
				Cred:        conn.Cred,
				Policy:      conn.policy,
				FDs:         conn.FDs,
				Bytes:       conn.Bytes,
				OpenedAt:    conn.openedAt,
				Generation:  conn.generation,
				Holder:      conn.holder,
				Charges:     charges,
			})
		}
	}
	sort.Slice(connections, func(i, j int) bool {
		return connections[i].ID < connections[j].ID
	})

	payload := snapshotFile{
		Version:    snapshotFormatVersion,
		Sequence:   g.seq,
		NextConnID: g.nextConnID,
		Options: optionsSnapshot{
			TenantSocketLimit: g.opts.TenantSocketLimit,
			TenantConnLimit:   g.opts.TenantConnLimit,
			FDMaxPerConn:      g.opts.FDMaxPerConn,
			FDBytesPerConn:    g.opts.FDBytesPerConn,
			LeaseTTL:          g.opts.LeaseTTL,
		},
		Quotas:      quotas,
		Bindings:    bindings,
		Connections: connections,
	}
	return json.Marshal(payload)
}

// Restore replaces the registry with a snapshot, all or nothing.
func (g *UnixSocketNamespaceGuard) Restore(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return ErrBadSnapshot
	}

	var payload snapshotFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: trailing snapshot content", ErrBadSnapshot)
	}
	if payload.Version != snapshotFormatVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, payload.Version)
	}

	state, err := payload.buildRestoredState()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq = state.seq
	g.nextConnID = state.nextConnID
	g.bindings = state.bindings
	g.conns = state.conns
	g.tenantSockets = state.tenantSockets
	g.tenantConns = state.tenantConns
	return nil
}

func (payload snapshotFile) buildRestoredState() (*restoredState, error) {
	if payload.Sequence < 0 {
		return nil, errors.New("snapshot sequence is negative")
	}
	state := &restoredState{
		seq:           payload.Sequence,
		nextConnID:    payload.NextConnID,
		bindings:      make(map[string]*bindingState),
		conns:         make(map[string]*connState),
		tenantSockets: make(map[string]int),
		tenantConns:   make(map[string]int),
	}

	seenSequences := make(map[int64]bool)
	for _, saved := range payload.Bindings {
		tenantID, jobID, shortName, err := parseSocketName(saved.Name)
		if err != nil {
			return nil, err
		}
		if saved.TenantID != tenantID || saved.JobID != jobID {
			return nil, fmt.Errorf("binding identity does not match %q", saved.Name)
		}
		if shortName == "" || saved.Seq <= 0 || seenSequences[saved.Seq] {
			return nil, fmt.Errorf("binding %q has an invalid sequence", saved.Name)
		}
		if saved.Kind != NamespaceAbstract || (saved.State != "" && saved.State != BindingBound) {
			return nil, fmt.Errorf("binding %q has invalid kind or state", saved.Name)
		}
		if saved.LeaseExpires.IsZero() {
			return nil, fmt.Errorf("binding %q has no lease", saved.Name)
		}
		if _, exists := state.bindings[saved.Name]; exists {
			return nil, fmt.Errorf("duplicate binding %q", saved.Name)
		}

		seenSequences[saved.Seq] = true
		state.bindings[saved.Name] = &bindingState{
			Binding: Binding{
				TenantID: tenantID,
				JobID:    jobID,
				Name:     saved.Name,
				Kind:     NamespaceAbstract,
				State:    BindingBound,
				Seq:      saved.Seq,
			},
			leaseExpires: saved.LeaseExpires,
			conns:        make(map[string]*connState),
		}
		state.tenantSockets[tenantID]++
		if saved.Seq > state.seq {
			state.seq = saved.Seq
		}
	}

	seenConns := make(map[string]bool)
	for _, saved := range payload.Connections {
		if saved.ID == "" || seenConns[saved.ID] {
			return nil, fmt.Errorf("connection id %q is invalid or duplicate", saved.ID)
		}
		binding := state.bindings[saved.BindingName]
		if binding == nil {
			return nil, fmt.Errorf("connection %q references a missing binding", saved.ID)
		}
		if saved.TenantID != binding.TenantID {
			return nil, fmt.Errorf("connection %q has the wrong tenant", saved.ID)
		}
		if saved.Cred.Tenant != "" && saved.Cred.Tenant != binding.TenantID {
			return nil, fmt.Errorf("connection %q credentials have the wrong tenant", saved.ID)
		}
		if saved.FDs < 0 || saved.Bytes < 0 || len(saved.Charges) != saved.FDs {
			return nil, fmt.Errorf("connection %q has inconsistent FD totals", saved.ID)
		}

		policy := saved.Policy
		if policy.AllowedKinds != nil {
			policy.AllowedKinds = append([]string(nil), policy.AllowedKinds...)
		}
		maxFDs := effectiveCap(policy.MaxFDs, payload.Options.FDMaxPerConn)
		maxBytes := effectiveCap(policy.MaxFDBytes, payload.Options.FDBytesPerConn)
		if maxFDs > 0 && saved.FDs > maxFDs {
			return nil, fmt.Errorf("connection %q exceeds its FD count budget", saved.ID)
		}

		holders := make(map[string]*fdScope)
		generations := make(map[uint64]*fdScope)
		totalBytes := 0
		for _, charge := range saved.Charges {
			if charge.Size < 0 {
				return nil, fmt.Errorf("connection %q has a negative descriptor size", saved.ID)
			}
			totalBytes += charge.Size
			holder := holders[charge.Holder]
			if holder == nil {
				holder = &fdScope{}
				holders[charge.Holder] = holder
			}
			holder.FDs++
			holder.Bytes += charge.Size

			generation := generations[charge.Generation]
			if generation == nil {
				generation = &fdScope{}
				generations[charge.Generation] = generation
			}
			generation.FDs++
			generation.Bytes += charge.Size
		}
		if totalBytes != saved.Bytes {
			return nil, fmt.Errorf("connection %q has inconsistent byte total", saved.ID)
		}
		if maxBytes > 0 && saved.Bytes > maxBytes {
			return nil, fmt.Errorf("connection %q exceeds its byte budget", saved.ID)
		}

		cred := saved.Cred
		cred.Tenant = binding.TenantID
		conn := &connState{
			Conn: Conn{
				ID:       saved.ID,
				TenantID: binding.TenantID,
				Name:     binding.Name,
				Cred:     cred,
				FDs:      saved.FDs,
				Bytes:    saved.Bytes,
			},
			policy:      policy,
			openedAt:    saved.OpenedAt,
			generation:  saved.Generation,
			holder:      saved.Holder,
			charges:     append([]fdCharge(nil), saved.Charges...),
			holders:     holders,
			generations: generations,
		}
		if conn.generation == 0 {
			conn.generation = uint64(binding.Seq)
		}
		if conn.holder == "" {
			conn.holder = "pid:" + strconv.Itoa(cred.PID) + ":uid:" + strconv.Itoa(cred.UID)
		}

		seenConns[saved.ID] = true
		state.conns[saved.ID] = conn
		binding.conns[saved.ID] = conn
		state.tenantConns[binding.TenantID]++

		if strings.HasPrefix(saved.ID, "conn-") {
			if number, err := strconv.ParseUint(strings.TrimPrefix(saved.ID, "conn-"), 10, 64); err == nil && number > state.nextConnID {
				state.nextConnID = number
			}
		}
	}

	for _, savedBinding := range payload.Bindings {
		binding := state.bindings[savedBinding.Name]
		listed := make(map[string]bool, len(savedBinding.ConnIDs))
		for _, connID := range savedBinding.ConnIDs {
			if listed[connID] || binding.conns[connID] == nil {
				return nil, fmt.Errorf("binding %q has an invalid connection list", savedBinding.Name)
			}
			listed[connID] = true
		}
		if len(listed) != len(binding.conns) {
			return nil, fmt.Errorf("binding %q is missing restored connections", savedBinding.Name)
		}
	}

	if len(payload.Quotas) != len(state.tenantSockets) {
		return nil, errors.New("snapshot quota table does not match bindings")
	}
	for tenantID, savedQuota := range payload.Quotas {
		if savedQuota.Sockets != state.tenantSockets[tenantID] || savedQuota.Connections != state.tenantConns[tenantID] {
			return nil, fmt.Errorf("snapshot quota for %q is inconsistent", tenantID)
		}
		if savedQuota.SocketLimit != payload.Options.TenantSocketLimit || savedQuota.ConnLimit != payload.Options.TenantConnLimit {
			return nil, fmt.Errorf("snapshot quota limits for %q are inconsistent", tenantID)
		}
		wantSocketsRemaining := remainingQuota(savedQuota.Sockets, payload.Options.TenantSocketLimit)
		wantConnsRemaining := remainingQuota(savedQuota.Connections, payload.Options.TenantConnLimit)
		if savedQuota.SocketsRemaining != wantSocketsRemaining || savedQuota.ConnectionsRemaining != wantConnsRemaining {
			return nil, fmt.Errorf("snapshot remaining quota for %q is inconsistent", tenantID)
		}
	}

	return state, nil
}
