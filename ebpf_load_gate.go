// Package task admits eBPF programs through an instruction whitelist, a map
// access check, attach target validation and a per tenant load quota.
package task

import "errors"

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Program lifecycle states kept in the gate table.
const (
	ProgLoaded   = "loaded"
	ProgAttached = "attached"
)

// Defaults applied when the gate policy leaves a field unset.
const (
	DefaultMaxPrograms      = 16
	DefaultMaxInsns         = 4096
	DefaultMaxMapsPerProg   = 8
	DefaultMaxAttachTargets = 4
	DefaultRequiredLicense  = "GPL"
)

// Errors reported by the gate.
var (
	ErrBlankTenant       = errors.New("tenant is required")
	ErrBlankProgramName  = errors.New("program name is required")
	ErrEmptyProgram      = errors.New("program carries no instruction")
	ErrBlankMapName      = errors.New("map name is required")
	ErrBadMapSpec        = errors.New("map spec is not usable")
	ErrUnknownMap        = errors.New("map is not registered")
	ErrMapReadOnly       = errors.New("map is read only for this program")
	ErrMapTenantMismatch = errors.New("map belongs to another tenant")
	ErrOpNotAllowed      = errors.New("instruction op is not whitelisted")
	ErrProgTypeDenied    = errors.New("program type is not allowed")
	ErrProgramTooLarge   = errors.New("program exceeds the instruction budget")
	ErrTooManyMaps       = errors.New("program references too many maps")
	ErrLicenseRequired   = errors.New("program license is not accepted")
	ErrQuotaExceeded     = errors.New("tenant load quota is exhausted")
	ErrUnknownHandle     = errors.New("unknown program handle")
	ErrBadAttachTarget   = errors.New("attach target is incomplete")
	ErrAttachLimit       = errors.New("program reached its attach target budget")
	ErrStillAttached     = errors.New("program is still attached")
	ErrNotAttached       = errors.New("program has no attach target")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Instruction is one verified eBPF instruction.
type Instruction struct {
	Op  string `json:"op"`
	Imm int64  `json:"imm"`
	Map string `json:"map"`
}

// Program is the object handed to the loader.
type Program struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"`
	Insns   []Instruction `json:"insns"`
	Maps    []string      `json:"maps"`
	License string        `json:"license"`
}

// MapSpec is one entry of the map registry.
type MapSpec struct {
	Name       string `json:"name"`
	Tenant     string `json:"tenant"`
	MaxEntries int    `json:"max_entries"`
	Writable   bool   `json:"writable"`
}

// AttachTarget is where the program wants to hook in.
type AttachTarget struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	IfIndex int    `json:"if_index"`
}

// GatePolicy bounds the gate tables.
type GatePolicy struct {
	AllowedOps       []string
	AllowedProgTypes []string
	MaxPrograms      int
	MaxInsns         int
	MaxMapsPerProg   int
	MaxAttachTargets int
	RequiredLicense  string
}

// Handle is the gate record for one loaded program.
type Handle struct {
	ID          string   `json:"id"`
	Tenant      string   `json:"tenant"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Fingerprint string   `json:"fingerprint"`
	State       string   `json:"state"`
	Targets     []string `json:"targets"`
}

// GateStats counts what the gate let through and what it turned away.
type GateStats struct {
	Loaded   int `json:"loaded"`
	Attached int `json:"attached"`
	Detached int `json:"detached"`
	Unloaded int `json:"unloaded"`
	Rejected int `json:"rejected"`
}

// EbpfLoadGate owns the map registry, the handle table and the quota counters.
type EbpfLoadGate struct {
	policy GatePolicy
}

// NewEbpfLoadGate builds a gate and fills the unset policy defaults.
func NewEbpfLoadGate(policy GatePolicy) *EbpfLoadGate {
	return &EbpfLoadGate{policy: policy}
}

// RegisterMap records one map so programs may reference it.
func (g *EbpfLoadGate) RegisterMap(spec MapSpec) error {
	return errNotImplemented
}

// CheckMapAccess decides whether a tenant may reach one map.
func (g *EbpfLoadGate) CheckMapAccess(tenant, name string, write bool) error {
	return errNotImplemented
}

// CheckInstructions walks the program body against the op whitelist.
func (g *EbpfLoadGate) CheckInstructions(prog Program) error {
	return errNotImplemented
}

// Fingerprint derives the stable identity of a program body.
func (g *EbpfLoadGate) Fingerprint(prog Program) string {
	return ""
}

// Quota reports the programs a tenant currently holds.
func (g *EbpfLoadGate) Quota(tenant string) (used int, limit int) {
	return 0, 0
}

// Load runs every pre-flight check and records the program handle.
func (g *EbpfLoadGate) Load(tenant string, prog Program) (Handle, error) {
	return Handle{}, errNotImplemented
}

// Attach binds a loaded program to one target.
func (g *EbpfLoadGate) Attach(handleID string, target AttachTarget) error {
	return errNotImplemented
}

// Detach releases every target of one program.
func (g *EbpfLoadGate) Detach(handleID string) error {
	return errNotImplemented
}

// Unload drops the handle and returns its quota slot.
func (g *EbpfLoadGate) Unload(handleID string) error {
	return errNotImplemented
}

// Handle reads one gate record.
func (g *EbpfLoadGate) Handle(id string) (Handle, bool) {
	return Handle{}, false
}

// Attached lists the attached programs of one tenant by handle id.
func (g *EbpfLoadGate) Attached(tenant string) []Handle {
	return nil
}

// Stats returns the gate counters.
func (g *EbpfLoadGate) Stats() GateStats {
	return GateStats{}
}

// Snapshot serialises the handle table, the map registry and the counters.
func (g *EbpfLoadGate) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *EbpfLoadGate) Restore(data []byte) error {
	return errNotImplemented
}
