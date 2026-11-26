// Package task applies double tagged 802.1Q admission on L2 access ports and
// keeps a bounded forwarding table.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	cfg PolicyConfig
}

// NewQinQTagPolicy builds a policy with the given caps.
func NewQinQTagPolicy(cfg PolicyConfig) *QinQTagPolicy {
	return &QinQTagPolicy{cfg: cfg}
}

// Parse splits one frame into its tag stack.
func (p *QinQTagPolicy) Parse(frame []byte) (TagStack, error) {
	return TagStack{}, errNotImplemented
}

// Classify admits or drops one ingress frame.
func (p *QinQTagPolicy) Classify(frame []byte, port PortConfig) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Egress rewrites one frame for the given egress port.
func (p *QinQTagPolicy) Egress(frame []byte, port PortConfig) ([]byte, error) {
	return nil, errNotImplemented
}

// Learn records the source MAC of one frame against its service vlan.
func (p *QinQTagPolicy) Learn(port string, frame []byte) error {
	return errNotImplemented
}

// LearnAt records a source MAC with an explicit timestamp.
func (p *QinQTagPolicy) LearnAt(port string, frame []byte, at time.Time) error {
	return errNotImplemented
}

// Lookup resolves the port that owns one MAC under one service vlan.
func (p *QinQTagPolicy) Lookup(svlan uint16, mac string) (string, bool) {
	return "", false
}

// PortStats reads the counters of one port.
func (p *QinQTagPolicy) PortStats(port string) PortStats {
	return PortStats{}
}

// SetPortConfig registers or replaces the config of one port.
func (p *QinQTagPolicy) SetPortConfig(port PortConfig) error {
	return errNotImplemented
}

// AgingSweep drops entries older than idle and reports the count.
func (p *QinQTagPolicy) AgingSweep(now time.Time, idle time.Duration) int {
	return 0
}

// EntryCount reports how many forwarding entries are live.
func (p *QinQTagPolicy) EntryCount() int {
	return 0
}

// Snapshot serialises the forwarding table and the port counters.
func (p *QinQTagPolicy) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *QinQTagPolicy) Restore(data []byte) error {
	return errNotImplemented
}
