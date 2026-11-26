// Package task gates kernel module loading on a signature chain and a policy
// whitelist, keeps the kernel taint bits that the loads produce, and reports
// unsigned modules that enforcement mode has to remove.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Kernel taint bits raised by the gate.
const (
	TaintUnsignedModule = 1 << 0
	TaintOutOfTree      = 1 << 1
	TaintForcedRemoval  = 1 << 2
)

// Load outcomes written to the audit trail.
const (
	OutcomeLoaded        = "loaded"
	OutcomeAlreadyLoaded = "already_loaded"
	OutcomeRejected      = "rejected"
	OutcomeUnloaded      = "unloaded"
	OutcomeSwept         = "swept"
)

// Errors returned by the gate.
var (
	ErrBadModule     = errors.New("module name, version or hash is invalid")
	ErrHashMismatch  = errors.New("module hash differs from the loaded one")
	ErrChainBroken   = errors.New("signature chain is empty or does not chain")
	ErrUntrustedRoot = errors.New("signature chain does not end in a trusted root")
	ErrCertExpired   = errors.New("signature chain contains an expired certificate")
	ErrPolicyDenied  = errors.New("module is whitelisted with a different hash")
	ErrModuleLimit   = errors.New("module table is full")
	ErrEnforceDenied = errors.New("enforce mode refuses an unsigned module")
	ErrBadTaint      = errors.New("taint bits to acknowledge are invalid")
	ErrBadSnapshot   = errors.New("snapshot payload is invalid")
)

// CertLink is one certificate of a module signature chain.
type CertLink struct {
	Subject     string `json:"subject"`
	Issuer      string `json:"issuer"`
	Fingerprint string `json:"fingerprint"`
	NotAfter    int64  `json:"not_after"`
}

// ModuleSignature is the detached signature shipped with a module.
type ModuleSignature struct {
	Sig   string     `json:"sig"`
	Chain []CertLink `json:"chain"`
}

// ModuleRequest is one load attempt.
type ModuleRequest struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Hash      string          `json:"hash"`
	Unsigned  bool            `json:"unsigned"`
	OutOfTree bool            `json:"out_of_tree"`
	SignerKey string          `json:"signer_key"`
	Signature ModuleSignature `json:"signature"`
	At        int64           `json:"at"`
}

// LoadResult is the outcome of one accepted load attempt.
type LoadResult struct {
	Name          string `json:"name"`
	Loaded        bool   `json:"loaded"`
	AlreadyLoaded bool   `json:"already_loaded"`
	Signed        bool   `json:"signed"`
	PolicyExempt  bool   `json:"policy_exempt"`
	Taint         uint32 `json:"taint"`
}

// LoadedModule is one resident module of the table.
type LoadedModule struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Hash     string `json:"hash"`
	Signed   bool   `json:"signed"`
	LoadedAt int64  `json:"loaded_at"`
	Seq      int64  `json:"seq"`
}

// Policy is one whitelist entry keyed by module name and hash.
type Policy struct {
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Allow bool   `json:"allow"`
}

// TaintState is the accumulated kernel taint of the gate.
type TaintState struct {
	Bits  uint32 `json:"bits"`
	Since int64  `json:"since"`
}

// Removal is one module that enforcement mode wants gone.
type Removal struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Seq    int64  `json:"seq"`
}

// AuditEntry is one line of the load audit trail.
type AuditEntry struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
	Seq     int64  `json:"seq"`
	At      int64  `json:"at"`
}

// ChainReport is the result of verifying a signature chain only.
type ChainReport struct {
	RootFingerprint string `json:"root_fingerprint"`
	Links           int    `json:"links"`
	Valid           bool   `json:"valid"`
}

// GateConfig holds the trusted roots and the table caps.
type GateConfig struct {
	TrustedRoots []string `json:"trusted_roots"`
	MaxModules   int      `json:"max_modules"`
	Enforce      bool     `json:"enforce"`
}

// ModuleSignGate owns the trust set, the module table, the taint state and the
// audit trail.
type ModuleSignGate struct {
	cfg GateConfig
}

// NewModuleSignGate builds a gate with the given roots and caps.
func NewModuleSignGate(cfg GateConfig) *ModuleSignGate {
	return &ModuleSignGate{cfg: cfg}
}

// TrustRoot adds one trusted root fingerprint.
func (g *ModuleSignGate) TrustRoot(fingerprint string) error {
	return errNotImplemented
}

// AllowModule registers or replaces one whitelist entry.
func (g *ModuleSignGate) AllowModule(p Policy) error {
	return errNotImplemented
}

// Load verifies one module and, when accepted, adds it to the table.
func (g *ModuleSignGate) Load(req ModuleRequest) (LoadResult, error) {
	return LoadResult{}, errNotImplemented
}

// Unload removes one module and reports whether it was present.
func (g *ModuleSignGate) Unload(name string) (bool, error) {
	return false, errNotImplemented
}

// SetEnforce switches enforcement mode on or off.
func (g *ModuleSignGate) SetEnforce(on bool, at int64) error {
	return errNotImplemented
}

// Sweep reports and removes the modules that enforcement mode refuses.
func (g *ModuleSignGate) Sweep(at int64) ([]Removal, error) {
	return nil, errNotImplemented
}

// VerifyChain checks one signature chain without touching the module table.
func (g *ModuleSignGate) VerifyChain(req ModuleRequest) (ChainReport, error) {
	return ChainReport{}, errNotImplemented
}

// Modules lists the resident modules ordered by name.
func (g *ModuleSignGate) Modules() []LoadedModule {
	return nil
}

// Taint reads the accumulated taint state.
func (g *ModuleSignGate) Taint() TaintState {
	return TaintState{}
}

// AckTaint clears the given taint bits.
func (g *ModuleSignGate) AckTaint(bits uint32, at int64) error {
	return errNotImplemented
}

// Audit returns a copy of the audit trail, oldest first.
func (g *ModuleSignGate) Audit() []AuditEntry {
	return nil
}

// Snapshot serialises the trust set, the module table, the taint and the mode.
func (g *ModuleSignGate) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ModuleSignGate) Restore(data []byte) error {
	return errNotImplemented
}
