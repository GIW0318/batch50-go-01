// Package task applies double tagged 802.1Q admission on L2 access ports and
// keeps a bounded forwarding table.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Tag protocol identifiers handled by the parser.
const (
	TPID8100 = 0x8100
	TPID88A8 = 0x88a8
	TPID9100 = 0x9100
	TPID9200 = 0x9200
)

// Port modes.
const (
	ModeAccess = "access"
	ModeTrunk  = "trunk"
	ModeQinQ   = "qinq"
)

// Verdict actions.
const (
	ActionAdmit = "admit"
	ActionDrop  = "drop"
)

// Egress actions.
const (
	EgressKeep  = "keep"
	EgressStrip = "strip"
	EgressDrop  = "drop"
)

// Drop and admit reason codes.
const (
	ReasonAdmit        = "tag_admit"
	ReasonDropSVLAN    = "tag_drop_svlan"
	ReasonDropNative   = "tag_drop_native"
	ReasonDropShape    = "tag_drop_shape"
	ReasonDropNoNative = "tag_drop_no_native"
)

// Errors surfaced by the policy.
var (
	ErrShortFrame       = errors.New("frame is shorter than an ethernet header")
	ErrUnknownTPID      = errors.New("tag protocol identifier is not supported")
	ErrTPIDMismatch     = errors.New("outer tag protocol identifier does not match the port")
	ErrBadMode          = errors.New("port mode is not supported")
	ErrSVLANSetTooLarge = errors.New("port service vlan set exceeds the limit")
	ErrUnknownPort      = errors.New("port configuration is not registered")
	ErrNoOuterTag       = errors.New("frame carries no outer tag")
	ErrEgressDropped    = errors.New("frame is dropped on egress")
	ErrTableFull        = errors.New("forwarding table is full")
	ErrBadSnapshot      = errors.New("snapshot payload invalid")
)

// PortConfig is the per port tag policy.
type PortConfig struct {
	Name          string
	Mode          string
	OuterTPID     uint16
	AllowedSVLANs []uint16
	NativeVLAN    uint16
	EgressAction  string
}

// Verdict is the admission answer for one frame.
type Verdict struct {
	Action string
	SVLAN  uint16
	CVLAN  uint16
	Native bool
	Reason string
}

// TagStack is the parsed tag layout of one frame.
type TagStack struct {
	HasOuter  bool
	OuterTPID uint16
	OuterVLAN uint16
	HasInner  bool
	InnerTPID uint16
	InnerVLAN uint16
	InnerTCI  uint16
	EtherType uint16
}

// PortStats counts the frames seen on one port.
type PortStats struct {
	Admitted int
	Dropped  int
	Learned  int
	Moved    int
}

// PolicyConfig bounds the live tables of the policy.
type PolicyConfig struct {
	MaxSVLANsPerPort int
	MaxTableEntries  int
	DefaultIdle      time.Duration
}

// QinQTagPolicy owns the port configs and the forwarding table.
type QinQTagPolicy struct {
	cfg   PolicyConfig
	mu    sync.RWMutex
	ports map[string]PortConfig
	table map[tableKey]tableEntry
	stats map[string]PortStats
}

// tableKey identifies one forwarding entry by service vlan and source MAC.
type tableKey struct {
	svlan uint16
	mac   string
}

// tableEntry records the owner port and the last learning time.
type tableEntry struct {
	port     string
	lastSeen time.Time
}

// NewQinQTagPolicy builds a policy with the given caps.
func NewQinQTagPolicy(cfg PolicyConfig) *QinQTagPolicy {
	return &QinQTagPolicy{
		cfg:   cfg,
		ports: make(map[string]PortConfig),
		table: make(map[tableKey]tableEntry),
		stats: make(map[string]PortStats),
	}
}

// supportedTPID reports whether tpid may carry tags on this policy.
func supportedTPID(tpid uint16) bool {
	return tpid == TPID8100 || tpid == TPID88A8
}

// knownTPID reports whether tpid is a recognised vlan tag identifier.
func knownTPID(tpid uint16) bool {
	return supportedTPID(tpid) || tpid == TPID9100 || tpid == TPID9200
}

// Parse splits one frame into its tag stack.
func (p *QinQTagPolicy) Parse(frame []byte) (TagStack, error) {
	var stack TagStack
	if len(frame) < 14 {
		return stack, ErrShortFrame
	}
	next := uint16(frame[12])<<8 | uint16(frame[13])
	offset := 14
	if knownTPID(next) {
		if !supportedTPID(next) {
			return stack, ErrUnknownTPID
		}
		if len(frame) < offset+4 {
			return stack, ErrShortFrame
		}
		tci := uint16(frame[14])<<8 | uint16(frame[15])
		stack.HasOuter = true
		stack.OuterTPID = next
		stack.OuterVLAN = tci & 0x0fff
		next = uint16(frame[16])<<8 | uint16(frame[17])
		offset = 18
		if knownTPID(next) {
			if !supportedTPID(next) {
				return stack, ErrUnknownTPID
			}
			if len(frame) < offset+4 {
				return stack, ErrShortFrame
			}
			stack.HasInner = true
			stack.InnerTPID = next
			stack.InnerTCI = uint16(frame[18])<<8 | uint16(frame[19])
			stack.InnerVLAN = stack.InnerTCI & 0x0fff
			next = uint16(frame[20])<<8 | uint16(frame[21])
			offset = 22
		}
	}
	stack.EtherType = next
	return stack, nil
}

// tagDepth counts the recognised tag layers of a parsed stack.
func tagDepth(stack TagStack) int {
	switch {
	case stack.HasInner:
		return 2
	case stack.HasOuter:
		return 1
	default:
		return 0
	}
}

// allowedSVLAN reports whether svlan is in the port allowed set.
func allowedSVLAN(set []uint16, svlan uint16) bool {
	for _, v := range set {
		if v == svlan {
			return true
		}
	}
	return false
}

// Classify admits or drops one ingress frame.
func (p *QinQTagPolicy) Classify(frame []byte, port PortConfig) (Verdict, error) {
	stack, err := p.Parse(frame)
	if err != nil {
		return Verdict{}, err
	}
	var wantDepth int
	switch port.Mode {
	case ModeAccess:
		wantDepth = 0
	case ModeTrunk:
		wantDepth = 1
	case ModeQinQ:
		wantDepth = 2
	default:
		return Verdict{}, ErrBadMode
	}
	depth := tagDepth(stack)
	if depth != wantDepth {
		return p.drop(port.Name, ReasonDropShape), nil
	}
	if depth == 0 {
		if port.NativeVLAN == 0 {
			return p.drop(port.Name, ReasonDropNoNative), nil
		}
		return p.admit(port.Name, Verdict{
			Action: ActionAdmit,
			SVLAN:  port.NativeVLAN,
			Native: true,
			Reason: ReasonAdmit,
		}), nil
	}
	if stack.OuterTPID != port.OuterTPID {
		return Verdict{}, ErrTPIDMismatch
	}
	if !allowedSVLAN(port.AllowedSVLANs, stack.OuterVLAN) {
		return p.drop(port.Name, ReasonDropSVLAN), nil
	}
	verdict := Verdict{
		Action: ActionAdmit,
		SVLAN:  stack.OuterVLAN,
		Reason: ReasonAdmit,
	}
	if depth == 2 {
		if port.NativeVLAN != 0 && stack.InnerVLAN == port.NativeVLAN {
			return p.drop(port.Name, ReasonDropNative), nil
		}
		verdict.CVLAN = stack.InnerVLAN
	}
	return p.admit(port.Name, verdict), nil
}

// admit records one admitted frame and returns the verdict.
func (p *QinQTagPolicy) admit(port string, verdict Verdict) Verdict {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats[port]
	stats.Admitted++
	p.stats[port] = stats
	return verdict
}

// drop records one dropped frame and returns the verdict.
func (p *QinQTagPolicy) drop(port string, reason string) Verdict {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats[port]
	stats.Dropped++
	p.stats[port] = stats
	return Verdict{Action: ActionDrop, Reason: reason}
}

// Egress rewrites one frame for the given egress port.
func (p *QinQTagPolicy) Egress(frame []byte, port PortConfig) ([]byte, error) {
	switch port.EgressAction {
	case EgressKeep:
		out := make([]byte, len(frame))
		copy(out, frame)
		return out, nil
	case EgressDrop:
		return nil, ErrEgressDropped
	case EgressStrip:
		stack, err := p.Parse(frame)
		if err != nil {
			return nil, err
		}
		if !stack.HasOuter {
			return nil, ErrNoOuterTag
		}
		if port.OuterTPID != 0 && stack.OuterTPID != port.OuterTPID {
			return nil, ErrTPIDMismatch
		}
		out := make([]byte, 0, len(frame)-4)
		out = append(out, frame[:12]...)
		out = append(out, frame[16:]...)
		return out, nil
	default:
		return nil, ErrBadMode
	}
}

// Learn records the source MAC of one frame against its service vlan.
func (p *QinQTagPolicy) Learn(port string, frame []byte) error {
	return p.LearnAt(port, frame, time.Now())
}

// LearnAt records a source MAC with an explicit timestamp.
func (p *QinQTagPolicy) LearnAt(port string, frame []byte, at time.Time) error {
	stack, err := p.Parse(frame)
	if err != nil {
		return err
	}
	if len(frame) < 14 {
		return ErrShortFrame
	}
	mac := net.HardwareAddr(frame[6:12]).String()

	p.mu.Lock()
	defer p.mu.Unlock()

	svlan := stack.OuterVLAN
	if !stack.HasOuter {
		cfg, ok := p.ports[port]
		if !ok {
			return ErrUnknownPort
		}
		if cfg.NativeVLAN == 0 {
			return ErrNoOuterTag
		}
		svlan = cfg.NativeVLAN
	}

	key := tableKey{svlan: svlan, mac: mac}
	stats := p.stats[port]
	if entry, ok := p.table[key]; ok {
		if entry.port != port {
			entry.port = port
			stats.Moved++
		}
		entry.lastSeen = at
		p.table[key] = entry
		p.stats[port] = stats
		return nil
	}
	if p.cfg.MaxTableEntries > 0 && len(p.table) >= p.cfg.MaxTableEntries {
		return ErrTableFull
	}
	p.table[key] = tableEntry{port: port, lastSeen: at}
	stats.Learned++
	p.stats[port] = stats
	return nil
}

// Lookup resolves the port that owns one MAC under one service vlan.
func (p *QinQTagPolicy) Lookup(svlan uint16, mac string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.table[tableKey{svlan: svlan, mac: mac}]
	if !ok {
		return "", false
	}
	return entry.port, true
}

// PortStats reads the counters of one port.
func (p *QinQTagPolicy) PortStats(port string) PortStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stats[port]
}

// SetPortConfig registers or replaces the config of one port.
func (p *QinQTagPolicy) SetPortConfig(port PortConfig) error {
	switch port.Mode {
	case ModeAccess, ModeTrunk, ModeQinQ:
	default:
		return ErrBadMode
	}
	if p.cfg.MaxSVLANsPerPort > 0 && len(port.AllowedSVLANs) > p.cfg.MaxSVLANsPerPort {
		return ErrSVLANSetTooLarge
	}
	port.AllowedSVLANs = append([]uint16(nil), port.AllowedSVLANs...)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ports[port.Name] = port
	return nil
}

// AgingSweep drops entries older than idle and reports the count.
func (p *QinQTagPolicy) AgingSweep(now time.Time, idle time.Duration) int {
	if idle <= 0 {
		idle = p.cfg.DefaultIdle
	}
	if idle <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := 0
	for key, entry := range p.table {
		if now.Sub(entry.lastSeen) > idle {
			delete(p.table, key)
			removed++
		}
	}
	return removed
}

// EntryCount reports how many forwarding entries are live.
func (p *QinQTagPolicy) EntryCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.table)
}

// snapshotVersion tags the serialised state layout.
const snapshotVersion = 1

// snapshotEntry is one serialised forwarding entry.
type snapshotEntry struct {
	SVLAN    uint16 `json:"svlan"`
	MAC      string `json:"mac"`
	Port     string `json:"port"`
	LastSeen int64  `json:"last_seen"`
}

// snapshotState is the serialised policy state.
type snapshotState struct {
	Version int                   `json:"version"`
	Entries []snapshotEntry       `json:"entries"`
	Stats   map[string]PortStats  `json:"stats"`
	Ports   map[string]PortConfig `json:"ports"`
}

// Snapshot serialises the forwarding table and the port counters.
func (p *QinQTagPolicy) Snapshot() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state := snapshotState{
		Version: snapshotVersion,
		Entries: make([]snapshotEntry, 0, len(p.table)),
		Stats:   make(map[string]PortStats, len(p.stats)),
		Ports:   make(map[string]PortConfig, len(p.ports)),
	}
	for key, entry := range p.table {
		state.Entries = append(state.Entries, snapshotEntry{
			SVLAN:    key.svlan,
			MAC:      key.mac,
			Port:     entry.port,
			LastSeen: entry.lastSeen.UnixNano(),
		})
	}
	for port, stats := range p.stats {
		state.Stats[port] = stats
	}
	for name, cfg := range p.ports {
		cfg.AllowedSVLANs = append([]uint16(nil), cfg.AllowedSVLANs...)
		state.Ports[name] = cfg
	}
	return json.Marshal(state)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *QinQTagPolicy) Restore(data []byte) error {
	var state snapshotState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if state.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, state.Version)
	}
	ports := make(map[string]PortConfig, len(state.Ports))
	for name, cfg := range state.Ports {
		cfg.AllowedSVLANs = append([]uint16(nil), cfg.AllowedSVLANs...)
		ports[name] = cfg
	}
	stats := make(map[string]PortStats, len(state.Stats))
	for port, counters := range state.Stats {
		stats[port] = counters
	}
	table := make(map[tableKey]tableEntry, len(state.Entries))
	for _, entry := range state.Entries {
		if entry.MAC == "" || entry.Port == "" {
			return fmt.Errorf("%w: entry with empty mac or port", ErrBadSnapshot)
		}
		key := tableKey{svlan: entry.SVLAN, mac: entry.MAC}
		table[key] = tableEntry{
			port:     entry.Port,
			lastSeen: time.Unix(0, entry.LastSeen),
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ports = ports
	p.stats = stats
	p.table = table
	return nil
}
