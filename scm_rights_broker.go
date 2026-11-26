// Package task brokers file descriptors received over SCM_RIGHTS on unix
// domain sockets, attributes them to tenants and enforces per tenant quotas.
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
	"syscall"
	"time"
)

// Descriptor kinds understood by the broker.
const (
	KindRegular   = "regular"
	KindDirectory = "dir"
	KindMemfd     = "memfd"
	KindPidfd     = "pidfd"
	KindSocket    = "socket"
)

// Audit events.
const (
	EventGranted         = "granted"
	EventReleased        = "released"
	EventDenied          = "denied"
	EventOrphanReclaimed = "orphan_reclaimed"
)

// Errors returned by the broker.
var (
	ErrBadConn       = errors.New("unix domain connection is required")
	ErrBlankPeer     = errors.New("peer id and tenant are required")
	ErrPeerExists    = errors.New("peer already registered")
	ErrUnknownPeer   = errors.New("unknown peer")
	ErrNoDescriptors = errors.New("no staged descriptors")
	ErrBadLimit      = errors.New("descriptor limit must be positive")
	ErrFDTypeDenied  = errors.New("descriptor type is not allowed")
	ErrQuotaExceeded = errors.New("tenant descriptor quota exceeded")
	ErrUnknownGrant  = errors.New("unknown descriptor grant")
	ErrBadQuota      = errors.New("quota values must not be negative")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// Limits bounds the audit log.
type Limits struct {
	MaxRecords int
	Now        func() time.Time
}

// Quota is the per tenant descriptor budget.
type Quota struct {
	MaxInFlight int
	MaxPerKind  int
	Allowed     []string
}

// Descriptor is one staged file descriptor waiting for admission.
type Descriptor struct {
	Kind      string
	Target    string
	Size      int64
	ObjectKey string
	ReadOnly  bool
}

// Peer is one authenticated unix socket peer.
type Peer struct {
	ID     string
	Tenant string
	UID    uint32
	PID    int
	Seq    uint64
}

// FDGrant is one admitted descriptor.
type FDGrant struct {
	ID        string
	PeerID    string
	Tenant    string
	Kind      string
	Target    string
	Size      int64
	ObjectKey string
	Seq       int64
}

// GrantRecord is one line of the audit log.
type GrantRecord struct {
	GrantID   string
	Tenant    string
	Kind      string
	ObjectKey string
	Event     string
	Reason    string
	Seq       int64
}

// BrokerStats aggregates the live counters of one tenant.
type BrokerStats struct {
	Tenant   string
	InFlight int
	Granted  int
	Released int
}

const defaultMaxRecords = 1024

type stagedBatch struct {
	descs []Descriptor
}

type peerState struct {
	peer    Peer
	conn    *net.UnixConn
	queue   []stagedBatch
	blocked bool
}

type grantState struct {
	grant    FDGrant
	released bool
}

type pendingPeer struct {
	peer Peer
	conn *net.UnixConn
}

// ScmRightsBroker owns the peer table, the quota table and the audit log.
type ScmRightsBroker struct {
	mu sync.Mutex

	limits Limits

	peers  map[string]*peerState
	quotas map[string]Quota
	grants map[string]*grantState
	stats  map[string]BrokerStats

	// pending contains connections that have been authenticated from their
	// kernel credentials but have not yet been registered with a tenant.
	pending map[string]*pendingPeer

	// kindCounts[tenant][kind] counts live grants. objectRefs counts live
	// aliases of one underlying object. Empty object keys are not aliased.
	kindCounts map[string]map[string]int
	objectRefs map[objectRefKey]int

	audit       []GrantRecord
	nextSeq     int64
	nextPeerSeq uint64
}

type objectRefKey struct {
	tenant string
	object string
}

type snapshotPeer struct {
	Peer Peer `json:"peer"`
}

type snapshotGrant struct {
	Grant    FDGrant `json:"grant"`
	Released bool    `json:"released"`
}

type brokerSnapshot struct {
	Version     int              `json:"version"`
	SavedAt     time.Time        `json:"saved_at"`
	NextSeq     int64            `json:"next_seq"`
	NextPeerSeq uint64           `json:"next_peer_seq"`
	Quotas      map[string]Quota `json:"quotas"`
	Peers       []snapshotPeer   `json:"peers"`
	Grants      []snapshotGrant  `json:"grants"`
	Audit       []GrantRecord    `json:"audit"`
}

const snapshotVersion = 1

// NewScmRightsBroker builds a broker with the given log bound.
func NewScmRightsBroker(limits Limits) *ScmRightsBroker {
	if limits.MaxRecords <= 0 {
		limits.MaxRecords = defaultMaxRecords
	}
	if limits.Now == nil {
		limits.Now = time.Now
	}
	return &ScmRightsBroker{
		limits:     limits,
		peers:      make(map[string]*peerState),
		quotas:     make(map[string]Quota),
		grants:     make(map[string]*grantState),
		stats:      make(map[string]BrokerStats),
		pending:    make(map[string]*pendingPeer),
		kindCounts: make(map[string]map[string]int),
		objectRefs: make(map[objectRefKey]int),
	}
}

// AcceptConn authenticates one accepted unix connection through peer credentials.
func (b *ScmRightsBroker) AcceptConn(conn *net.UnixConn) (Peer, error) {
	if conn == nil {
		return Peer{}, ErrBadConn
	}
	ucred, err := peerCredentials(conn)
	if err != nil {
		return Peer{}, ErrBadConn
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// RegisterPeer is the place where an administrator maps a credential UID
	// to a tenant. Resolve the connection's identity through that mapping,
	// rather than through anything supplied by the remote peer.
	var chosen *peerState
	for _, ps := range b.peers {
		if ps.peer.UID != ucred.Uid || ps.conn != nil {
			continue
		}
		if chosen == nil || ps.peer.Seq < chosen.peer.Seq {
			chosen = ps
		}
	}
	if chosen == nil {
		p := Peer{ID: credentialPeerID(ucred), UID: ucred.Uid, PID: int(ucred.Pid)}
		for b.pending[p.ID] != nil {
			p.ID += "-next"
		}
		b.pending[p.ID] = &pendingPeer{peer: p, conn: conn}
		return p, nil
	}
	chosen.conn = conn
	chosen.peer.PID = int(ucred.Pid)
	return chosen.peer, nil
}

// RegisterPeer records one peer that has already been authenticated.
func (b *ScmRightsBroker) RegisterPeer(peerID, tenant string, uid uint32) (Peer, error) {
	if strings.TrimSpace(peerID) == "" || strings.TrimSpace(tenant) == "" {
		return Peer{}, ErrBlankPeer
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.peers[peerID]; ok {
		return Peer{}, ErrPeerExists
	}
	b.nextPeerSeq++
	p := Peer{ID: peerID, Tenant: tenant, UID: uid, Seq: b.nextPeerSeq}
	var conn *net.UnixConn
	if pending := b.takePendingLocked(peerID, uid); pending != nil {
		conn = pending.conn
		p.PID = pending.peer.PID
	}
	b.peers[peerID] = &peerState{peer: p, conn: conn}
	b.ensureStatsLocked(tenant)
	return p, nil
}

// SetQuota installs the descriptor budget of one tenant.
func (b *ScmRightsBroker) SetQuota(tenant string, q Quota) error {
	if strings.TrimSpace(tenant) == "" {
		return ErrBlankPeer
	}
	if q.MaxInFlight < 0 || q.MaxPerKind < 0 {
		return ErrBadQuota
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	cp := q
	cp.Allowed = append([]string(nil), q.Allowed...)
	b.quotas[tenant] = cp
	b.ensureStatsLocked(tenant)
	return nil
}

// Stage queues descriptors as if they had arrived with a single message.
func (b *ScmRightsBroker) Stage(peer Peer, in []Descriptor) error {
	if len(in) == 0 {
		return ErrNoDescriptors
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	ps, ok := b.peers[peer.ID]
	if !ok {
		return ErrUnknownPeer
	}
	batch := stagedBatch{descs: append([]Descriptor(nil), in...)}

	// A failed admission leaves the batch staged so it can be retried with a
	// larger maxFDs. If the caller stages a corrected (for example smaller)
	// batch after a refusal, that batch supersedes the refused one.
	if ps.blocked && len(ps.queue) != 0 {
		ps.queue[0] = batch
		ps.blocked = false
		return nil
	}
	ps.queue = append(ps.queue, batch)
	return nil
}

// Receive admits the staged descriptors of one peer in arrival order.
func (b *ScmRightsBroker) Receive(peer Peer, maxFDs int) ([]FDGrant, error) {
	if maxFDs <= 0 {
		return nil, ErrBadLimit
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	ps, ok := b.peers[peer.ID]
	if !ok {
		return nil, ErrUnknownPeer
	}
	if len(ps.queue) == 0 {
		return nil, ErrNoDescriptors
	}

	batch := &ps.queue[0]
	descs := batch.descs
	if len(descs) > maxFDs {
		ps.blocked = true
		b.recordDeniedLocked(ps.peer, descs, ErrQuotaExceeded)
		return nil, ErrQuotaExceeded
	}

	q, ok := b.quotas[ps.peer.Tenant]
	if !ok {
		ps.blocked = true
		b.recordDeniedLocked(ps.peer, descs, ErrQuotaExceeded)
		return nil, ErrQuotaExceeded
	}

	kinds := make([]string, len(descs))
	allowed := allowedKinds(q)
	for i, d := range descs {
		kinds[i] = classifyKind(d)
		if !validKind(kinds[i]) {
			ps.blocked = true
			b.recordDeniedLocked(ps.peer, descs, ErrFDTypeDenied)
			return nil, ErrFDTypeDenied
		}
		if _, ok := allowed[kinds[i]]; !ok {
			ps.blocked = true
			b.recordDeniedLocked(ps.peer, descs, ErrFDTypeDenied)
			return nil, ErrFDTypeDenied
		}
	}

	st := b.stats[ps.peer.Tenant]
	if st.InFlight+len(descs) > q.MaxInFlight {
		ps.blocked = true
		b.recordDeniedLocked(ps.peer, descs, ErrQuotaExceeded)
		return nil, ErrQuotaExceeded
	}
	perKind := make(map[string]int)
	for _, kind := range kinds {
		perKind[kind]++
	}
	for kind, n := range perKind {
		if b.kindCounts[ps.peer.Tenant][kind]+n > q.MaxPerKind {
			ps.blocked = true
			b.recordDeniedLocked(ps.peer, descs, ErrQuotaExceeded)
			return nil, ErrQuotaExceeded
		}
	}

	// All checks passed. Commit the whole message under the lock so the
	// counters and their audit records become visible together.
	grants := make([]FDGrant, 0, len(descs))
	for i, d := range descs {
		seq := b.nextSeq + 1
		id := "grant-" + strconv.FormatInt(seq, 10)
		g := FDGrant{
			ID:        id,
			PeerID:    ps.peer.ID,
			Tenant:    ps.peer.Tenant,
			Kind:      kinds[i],
			Target:    d.Target,
			Size:      d.Size,
			ObjectKey: d.ObjectKey,
			Seq:       seq,
		}
		b.grants[id] = &grantState{grant: g}
		st = b.stats[ps.peer.Tenant]
		st.Granted++
		st.InFlight++
		b.stats[ps.peer.Tenant] = st
		b.incKindLocked(ps.peer.Tenant, g.Kind)
		b.incObjectLocked(ps.peer.Tenant, g.ObjectKey)
		b.appendAuditLocked(GrantRecord{
			GrantID:   g.ID,
			Tenant:    g.Tenant,
			Kind:      g.Kind,
			ObjectKey: g.ObjectKey,
			Event:     EventGranted,
		})
		grants = append(grants, g)
	}

	ps.queue = ps.queue[1:]
	ps.blocked = false
	return grants, nil
}

// Release closes one admitted descriptor grant.
func (b *ScmRightsBroker) Release(grantID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	gs, ok := b.grants[grantID]
	if !ok {
		return ErrUnknownGrant
	}
	if gs.released {
		return nil
	}
	b.releaseGrantLocked(gs, EventReleased, "")
	return nil
}

// ReleasePeer closes every grant of one peer after it exits.
func (b *ScmRightsBroker) ReleasePeer(peerID string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ps, ok := b.peers[peerID]
	if !ok {
		return 0, ErrUnknownPeer
	}

	count := 0
	for _, id := range b.liveGrantIDsForPeerLocked(peerID) {
		b.releaseGrantLocked(b.grants[id], EventReleased, "peer released")
		count++
	}
	if ps.conn != nil {
		_ = ps.conn.Close()
	}
	delete(b.peers, peerID)
	return count, nil
}

// Sweep reclaims grants of peers that went away before releasing them.
func (b *ScmRightsBroker) Sweep() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, pending := range b.pending {
		if connectionGone(pending.conn) {
			_ = pending.conn.Close()
			delete(b.pending, id)
		}
	}

	var gone []*peerState
	for _, ps := range b.peers {
		if ps.conn != nil && connectionGone(ps.conn) {
			gone = append(gone, ps)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].peer.Seq < gone[j].peer.Seq })

	count := 0
	for _, ps := range gone {
		for _, id := range b.liveGrantIDsForPeerLocked(ps.peer.ID) {
			b.releaseGrantLocked(b.grants[id], EventOrphanReclaimed, "peer exited")
			count++
		}
		if ps.conn != nil {
			_ = ps.conn.Close()
		}
		delete(b.peers, ps.peer.ID)
	}
	return count, nil
}

// Audit returns the audit log, oldest first.
func (b *ScmRightsBroker) Audit() []GrantRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]GrantRecord(nil), b.audit...)
}

// Stats reads the live counters of one tenant.
func (b *ScmRightsBroker) Stats(tenant string) BrokerStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.stats[tenant]
	st.Tenant = tenant
	return st
}

// Snapshot serialises peers, quotas, grants and the audit log.
func (b *ScmRightsBroker) Snapshot() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := brokerSnapshot{
		Version:     snapshotVersion,
		SavedAt:     b.limits.Now(),
		NextSeq:     b.nextSeq,
		NextPeerSeq: b.nextPeerSeq,
		Quotas:      make(map[string]Quota, len(b.quotas)),
		Audit:       append([]GrantRecord(nil), b.audit...),
	}
	for tenant, q := range b.quotas {
		cp := q
		cp.Allowed = append([]string(nil), q.Allowed...)
		snap.Quotas[tenant] = cp
	}

	peerIDs := make([]string, 0, len(b.peers))
	for id := range b.peers {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)
	snap.Peers = make([]snapshotPeer, 0, len(peerIDs))
	for _, id := range peerIDs {
		snap.Peers = append(snap.Peers, snapshotPeer{Peer: b.peers[id].peer})
	}

	grantIDs := make([]string, 0, len(b.grants))
	for id := range b.grants {
		grantIDs = append(grantIDs, id)
	}
	sort.Slice(grantIDs, func(i, j int) bool {
		left := b.grants[grantIDs[i]]
		right := b.grants[grantIDs[j]]
		if left.grant.Seq == right.grant.Seq {
			return grantIDs[i] < grantIDs[j]
		}
		return left.grant.Seq < right.grant.Seq
	})
	snap.Grants = make([]snapshotGrant, 0, len(grantIDs))
	for _, id := range grantIDs {
		gs := b.grants[id]
		snap.Grants = append(snap.Grants, snapshotGrant{Grant: gs.grant, Released: gs.released})
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return nil, ErrBadSnapshot
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (b *ScmRightsBroker) Restore(data []byte) error {
	var snap brokerSnapshot
	if len(data) == 0 || json.Unmarshal(data, &snap) != nil || snap.Version != snapshotVersion {
		return ErrBadSnapshot
	}
	if snap.NextSeq < 0 || snap.NextPeerSeq < 0 {
		return ErrBadSnapshot
	}

	tmp := NewScmRightsBroker(b.limits)
	tmp.nextSeq = snap.NextSeq
	tmp.nextPeerSeq = snap.NextPeerSeq

	for tenant, q := range snap.Quotas {
		if strings.TrimSpace(tenant) == "" || q.MaxInFlight < 0 || q.MaxPerKind < 0 {
			return ErrBadSnapshot
		}
		cp := q
		cp.Allowed = append([]string(nil), q.Allowed...)
		tmp.quotas[tenant] = cp
		tmp.ensureStatsLocked(tenant)
	}

	maxPeerSeq := uint64(0)
	for _, sp := range snap.Peers {
		p := sp.Peer
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Tenant) == "" || p.Seq == 0 {
			return ErrBadSnapshot
		}
		if _, exists := tmp.peers[p.ID]; exists {
			return ErrBadSnapshot
		}
		tmp.peers[p.ID] = &peerState{peer: p}
		tmp.ensureStatsLocked(p.Tenant)
		if p.Seq > maxPeerSeq {
			maxPeerSeq = p.Seq
		}
	}
	if tmp.nextPeerSeq < maxPeerSeq {
		return ErrBadSnapshot
	}

	for _, rec := range snap.Audit {
		if rec.Seq <= 0 || !validEvent(rec.Event) {
			return ErrBadSnapshot
		}
		if len(tmp.audit) != 0 && rec.Seq <= tmp.audit[len(tmp.audit)-1].Seq {
			return ErrBadSnapshot
		}
		tmp.audit = append(tmp.audit, rec)
	}
	if len(tmp.audit) > tmp.limits.MaxRecords {
		tmp.audit = append([]GrantRecord(nil), tmp.audit[len(tmp.audit)-tmp.limits.MaxRecords:]...)
	}

	maxGrantSeq := int64(0)
	for _, sg := range snap.Grants {
		g := sg.Grant
		if g.ID == "" || g.PeerID == "" || g.Tenant == "" || g.Seq <= 0 || !validKind(g.Kind) {
			return ErrBadSnapshot
		}
		if _, exists := tmp.grants[g.ID]; exists {
			return ErrBadSnapshot
		}
		if g.Seq > maxGrantSeq {
			maxGrantSeq = g.Seq
		}
		tmp.grants[g.ID] = &grantState{grant: g, released: sg.Released}
		tmp.ensureStatsLocked(g.Tenant)
	}

	maxAuditSeq := int64(0)
	if len(tmp.audit) != 0 {
		maxAuditSeq = tmp.audit[len(tmp.audit)-1].Seq
	}
	if maxGrantSeq > tmp.nextSeq || maxAuditSeq > tmp.nextSeq {
		return ErrBadSnapshot
	}

	// Rebuild live counters from grant tombstones. Any grant that was live
	// when the snapshot was taken belongs to the previous process and is an
	// orphan now, so it is reclaimed before the restored state is published.
	grantIDs := make([]string, 0, len(tmp.grants))
	for id := range tmp.grants {
		grantIDs = append(grantIDs, id)
	}
	sort.Slice(grantIDs, func(i, j int) bool {
		return tmp.grants[grantIDs[i]].grant.Seq < tmp.grants[grantIDs[j]].grant.Seq
	})
	for _, id := range grantIDs {
		gs := tmp.grants[id]
		st := tmp.stats[gs.grant.Tenant]
		st.Granted++
		if gs.released {
			st.Released++
		} else {
			gs.released = true
			st.Released++
			tmp.appendAuditLocked(GrantRecord{
				GrantID:   gs.grant.ID,
				Tenant:    gs.grant.Tenant,
				Kind:      gs.grant.Kind,
				ObjectKey: gs.grant.ObjectKey,
				Event:     EventOrphanReclaimed,
				Reason:    "restored after restart",
			})
		}
		tmp.stats[gs.grant.Tenant] = st
	}
	// The replacement is one assignment protected by the broker mutex. A
	// malformed snapshot returned above without touching any live state.
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSeq = tmp.nextSeq
	b.nextPeerSeq = tmp.nextPeerSeq
	b.peers = tmp.peers
	b.quotas = tmp.quotas
	b.grants = tmp.grants
	b.stats = tmp.stats
	b.pending = make(map[string]*pendingPeer)
	b.kindCounts = tmp.kindCounts
	b.objectRefs = tmp.objectRefs
	b.audit = tmp.audit
	return nil
}

func (b *ScmRightsBroker) ensureStatsLocked(tenant string) {
	st := b.stats[tenant]
	st.Tenant = tenant
	b.stats[tenant] = st
}

func (b *ScmRightsBroker) takePendingLocked(peerID string, uid uint32) *pendingPeer {
	if pending := b.pending[peerID]; pending != nil && pending.peer.UID == uid {
		delete(b.pending, peerID)
		return pending
	}
	for id, pending := range b.pending {
		if pending.peer.UID == uid {
			delete(b.pending, id)
			return pending
		}
	}
	return nil
}

func (b *ScmRightsBroker) incKindLocked(tenant, kind string) {
	m := b.kindCounts[tenant]
	if m == nil {
		m = make(map[string]int)
		b.kindCounts[tenant] = m
	}
	m[kind]++
}

func (b *ScmRightsBroker) decKindLocked(tenant, kind string) {
	m := b.kindCounts[tenant]
	if m == nil {
		return
	}
	if m[kind] <= 1 {
		delete(m, kind)
		if len(m) == 0 {
			delete(b.kindCounts, tenant)
		}
		return
	}
	m[kind]--
}

func (b *ScmRightsBroker) incObjectLocked(tenant, object string) {
	if object == "" {
		return
	}
	b.objectRefs[objectRefKey{tenant: tenant, object: object}]++
}

func (b *ScmRightsBroker) decObjectLocked(tenant, object string) {
	if object == "" {
		return
	}
	key := objectRefKey{tenant: tenant, object: object}
	if b.objectRefs[key] <= 1 {
		delete(b.objectRefs, key)
		return
	}
	b.objectRefs[key]--
}

func (b *ScmRightsBroker) releaseGrantLocked(gs *grantState, event, reason string) {
	if gs.released {
		return
	}
	gs.released = true
	st := b.stats[gs.grant.Tenant]
	if st.InFlight > 0 {
		st.InFlight--
	}
	st.Released++
	b.stats[gs.grant.Tenant] = st
	b.decKindLocked(gs.grant.Tenant, gs.grant.Kind)
	b.decObjectLocked(gs.grant.Tenant, gs.grant.ObjectKey)
	b.appendAuditLocked(GrantRecord{
		GrantID:   gs.grant.ID,
		Tenant:    gs.grant.Tenant,
		Kind:      gs.grant.Kind,
		ObjectKey: gs.grant.ObjectKey,
		Event:     event,
		Reason:    reason,
	})
}

func (b *ScmRightsBroker) liveGrantIDsForPeerLocked(peerID string) []string {
	ids := make([]string, 0)
	for id, gs := range b.grants {
		if gs.grant.PeerID == peerID && !gs.released {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return b.grants[ids[i]].grant.Seq < b.grants[ids[j]].grant.Seq
	})
	return ids
}

func (b *ScmRightsBroker) recordDeniedLocked(peer Peer, descs []Descriptor, reason error) {
	for _, d := range descs {
		b.appendAuditLocked(GrantRecord{
			Tenant:    peer.Tenant,
			Kind:      classifyKind(d),
			ObjectKey: d.ObjectKey,
			Event:     EventDenied,
			Reason:    reason.Error(),
		})
	}
}

func (b *ScmRightsBroker) appendAuditLocked(rec GrantRecord) {
	b.nextSeq++
	rec.Seq = b.nextSeq
	b.audit = append(b.audit, rec)
	if len(b.audit) > b.limits.MaxRecords {
		drop := len(b.audit) - b.limits.MaxRecords
		b.audit = append([]GrantRecord(nil), b.audit[drop:]...)
	}
}

func allowedKinds(q Quota) map[string]struct{} {
	out := make(map[string]struct{}, len(q.Allowed))
	for _, kind := range q.Allowed {
		out[classifyKind(Descriptor{Kind: kind})] = struct{}{}
	}
	return out
}

func classifyKind(d Descriptor) string {
	kind := strings.ToLower(strings.TrimSpace(d.Kind))
	switch kind {
	case KindRegular, "file":
		return KindRegular
	case KindDirectory, "directory":
		return KindDirectory
	case KindMemfd, "memfd:":
		return KindMemfd
	case KindPidfd, "pidfd:":
		return KindPidfd
	case KindSocket, "unix":
		return KindSocket
	case "":
		target := strings.ToLower(strings.TrimSpace(d.Target))
		switch {
		case strings.HasPrefix(target, "memfd:"):
			return KindMemfd
		case strings.HasPrefix(target, "pidfd:"):
			return KindPidfd
		case strings.HasPrefix(target, "dir:"), strings.HasPrefix(target, "directory:"):
			return KindDirectory
		case strings.HasPrefix(target, "socket:"), strings.HasPrefix(target, "unix:"):
			return KindSocket
		default:
			return KindRegular
		}
	default:
		return kind
	}
}

func validKind(kind string) bool {
	switch kind {
	case KindRegular, KindDirectory, KindMemfd, KindPidfd, KindSocket:
		return true
	default:
		return false
	}
}

func validEvent(event string) bool {
	switch event {
	case EventGranted, EventReleased, EventDenied, EventOrphanReclaimed:
		return true
	default:
		return false
	}
}

func peerCredentials(conn *net.UnixConn) (*syscall.Ucred, error) {
	if conn == nil || conn.RemoteAddr() == nil {
		return nil, ErrBadConn
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		cred  *syscall.Ucred
		opErr error
	)
	if err := raw.Control(func(fd uintptr) {
		accepting, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
		if err != nil {
			opErr = err
			return
		}
		if accepting != 0 {
			opErr = ErrBadConn
			return
		}
		cred, opErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if opErr != nil || cred == nil {
		return nil, ErrBadConn
	}
	return cred, nil
}

func credentialPeerID(cred *syscall.Ucred) string {
	return fmt.Sprintf("uid:%d:pid:%d", cred.Uid, cred.Pid)
}

func connectionGone(conn *net.UnixConn) bool {
	if conn == nil {
		return true
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return true
	}
	var gone bool
	err = raw.Control(func(fd uintptr) {
		var buf [1]byte
		n, _, recvErr := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		switch {
		case n > 0:
			gone = false
		case recvErr == nil:
			gone = true // orderly shutdown: read side is at EOF
		case recvErr == syscall.EAGAIN || recvErr == syscall.EWOULDBLOCK || recvErr == syscall.EINTR:
			gone = false
		default:
			gone = true
		}
	})
	if err != nil {
		return true
	}
	return gone
}
