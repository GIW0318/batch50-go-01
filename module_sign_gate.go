// Package task gates kernel module loading on a signature chain and a policy
// whitelist, keeps the kernel taint bits that the loads produce, and reports
// unsigned modules that enforcement mode has to remove.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const defaultMaxModules = 64

const snapshotFormat = "module-sign-gate-snapshot/v1"

const removalReasonUnsigned = "unsigned"

// Kernel taint bits raised by the gate.
const (
	TaintUnsignedModule = 1 << 0
	TaintOutOfTree      = 1 << 1
	TaintForcedRemoval  = 1 << 2
)

const validTaintBits = TaintUnsignedModule | TaintOutOfTree | TaintForcedRemoval

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

var (
	errEmptyTrustRoot  = errors.New("trust root fingerprint is empty")
	errPolicyTableFull = errors.New("policy table is full")
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
	mu        sync.RWMutex
	cfg       GateConfig
	roots     map[string]struct{}
	policies  map[string]Policy
	modules   map[string]LoadedModule
	audit     []AuditEntry
	taint     TaintState
	enforce   bool
	enforceAt int64
	seq       int64
}

type snapshotData struct {
	Format       string         `json:"format"`
	TrustedRoots []string       `json:"trusted_roots"`
	Policies     []Policy       `json:"policies"`
	Modules      []LoadedModule `json:"modules"`
	Audit        []AuditEntry   `json:"audit"`
	Taint        TaintState     `json:"taint"`
	Enforce      bool           `json:"enforce"`
	EnforceAt    int64          `json:"enforce_at"`
	Seq          int64          `json:"seq"`
	MaxModules   int            `json:"max_modules"`
}

type snapshotEnvelope struct {
	snapshotData
	Checksum string `json:"checksum"`
}

// NewModuleSignGate builds a gate with the given roots and caps.
func NewModuleSignGate(cfg GateConfig) *ModuleSignGate {
	if cfg.MaxModules <= 0 {
		cfg.MaxModules = defaultMaxModules
	}

	gate := &ModuleSignGate{
		cfg:      cfg,
		roots:    make(map[string]struct{}, len(cfg.TrustedRoots)),
		policies: make(map[string]Policy),
		modules:  make(map[string]LoadedModule),
		enforce:  cfg.Enforce,
	}
	for _, fingerprint := range cfg.TrustedRoots {
		fingerprint = strings.TrimSpace(fingerprint)
		if fingerprint != "" {
			gate.roots[fingerprint] = struct{}{}
		}
	}
	return gate
}

// TrustRoot adds one trusted root fingerprint.
func (g *ModuleSignGate) TrustRoot(fingerprint string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return errEmptyTrustRoot
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.roots[fingerprint] = struct{}{}
	return nil
}

// AllowModule registers or replaces one whitelist entry.
func (g *ModuleSignGate) AllowModule(p Policy) error {
	if !validModuleName(p.Name) || strings.TrimSpace(p.Hash) == "" {
		return ErrBadModule
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Hash = strings.TrimSpace(p.Hash)

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.policies[p.Name]; !exists && len(g.policies) >= g.cfg.MaxModules {
		return fmt.Errorf("%w: %w", ErrPolicyDenied, errPolicyTableFull)
	}
	g.policies[p.Name] = p
	return nil
}

// Load verifies one module and, when accepted, adds it to the table.
func (g *ModuleSignGate) Load(req ModuleRequest) (LoadResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	reject := func(err error) (LoadResult, error) {
		g.appendAuditLocked(req.Name, OutcomeRejected, err.Error(), req.At)
		return LoadResult{Taint: g.taint.Bits}, err
	}

	if err := validateModuleIdentity(req); err != nil {
		return reject(err)
	}

	policyExempt := false
	if policy, ok := g.policies[req.Name]; ok {
		if !policy.Allow || policy.Hash != req.Hash {
			return reject(ErrPolicyDenied)
		}
		policyExempt = true
	}

	if existing, ok := g.modules[req.Name]; ok {
		if existing.Hash != req.Hash {
			return reject(ErrHashMismatch)
		}

		detail := auditDetail(req, "", policyExempt)
		g.appendAuditLocked(req.Name, OutcomeAlreadyLoaded, detail, req.At)
		return LoadResult{
			Name:          req.Name,
			Loaded:        true,
			AlreadyLoaded: true,
			Signed:        existing.Signed,
			PolicyExempt:  policyExempt,
			Taint:         g.taint.Bits,
		}, nil
	}

	signed := !req.Unsigned
	rootFingerprint := ""
	if signed && !policyExempt {
		report, err := g.verifyChainLocked(req)
		if err != nil {
			return reject(err)
		}
		rootFingerprint = report.RootFingerprint
	}

	if g.enforce && !signed {
		return reject(ErrEnforceDenied)
	}

	if len(g.modules) >= g.cfg.MaxModules {
		return reject(ErrModuleLimit)
	}

	addedTaint := uint32(0)
	if !signed && !policyExempt {
		addedTaint |= TaintUnsignedModule
	}
	if req.OutOfTree {
		addedTaint |= TaintOutOfTree
	}

	seq := g.appendAuditLocked(
		req.Name,
		OutcomeLoaded,
		auditDetail(req, rootFingerprint, policyExempt),
		req.At,
	)
	g.raiseTaintLocked(addedTaint, req.At)
	g.modules[req.Name] = LoadedModule{
		Name:     req.Name,
		Version:  req.Version,
		Hash:     req.Hash,
		Signed:   signed,
		LoadedAt: req.At,
		Seq:      seq,
	}

	return LoadResult{
		Name:          req.Name,
		Loaded:        true,
		AlreadyLoaded: false,
		Signed:        signed,
		PolicyExempt:  policyExempt,
		Taint:         g.taint.Bits,
	}, nil
}

// Unload removes one module and reports whether it was present.
func (g *ModuleSignGate) Unload(name string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	existed := false
	detail := "not_loaded"
	if module, ok := g.modules[name]; ok {
		existed = true
		detail = "fingerprint=" + module.Hash
		delete(g.modules, name)
	}
	g.appendAuditLocked(name, OutcomeUnloaded, detail, 0)
	return existed, nil
}

// SetEnforce switches enforcement mode on or off.
func (g *ModuleSignGate) SetEnforce(on bool, at int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enforce = on
	g.enforceAt = at
	return nil
}

// Sweep reports and removes the modules that enforcement mode refuses.
func (g *ModuleSignGate) Sweep(at int64) ([]Removal, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	removals := make([]Removal, 0)
	if !g.enforce {
		return removals, nil
	}

	names := make([]string, 0, len(g.modules))
	for name, module := range g.modules {
		if !module.Signed {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	removed := false
	for _, name := range names {
		module := g.modules[name]
		seq := g.appendAuditLocked(
			name,
			OutcomeSwept,
			"reason="+removalReasonUnsigned+";fingerprint="+module.Hash,
			at,
		)
		removals = append(removals, Removal{
			Name:   name,
			Reason: removalReasonUnsigned,
			Seq:    seq,
		})
		delete(g.modules, name)
		removed = true
	}

	if removed {
		g.raiseTaintLocked(TaintForcedRemoval, at)
	}
	return removals, nil
}

// VerifyChain checks one signature chain without touching the module table.
func (g *ModuleSignGate) VerifyChain(req ModuleRequest) (ChainReport, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.verifyChainLocked(req)
}

// Modules lists the resident modules ordered by name.
func (g *ModuleSignGate) Modules() []LoadedModule {
	g.mu.RLock()
	defer g.mu.RUnlock()

	names := make([]string, 0, len(g.modules))
	for name := range g.modules {
		names = append(names, name)
	}
	sort.Strings(names)

	modules := make([]LoadedModule, 0, len(names))
	for _, name := range names {
		modules = append(modules, g.modules[name])
	}
	return modules
}

// Taint reads the accumulated taint state.
func (g *ModuleSignGate) Taint() TaintState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.taint
}

// AckTaint clears the given taint bits.
func (g *ModuleSignGate) AckTaint(bits uint32, at int64) error {
	if bits == 0 || bits&^validTaintBits != 0 {
		return ErrBadTaint
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.taint.Bits &^= bits
	return nil
}

// Audit returns a copy of the audit trail, oldest first.
func (g *ModuleSignGate) Audit() []AuditEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]AuditEntry(nil), g.audit...)
}

// Snapshot serialises the trust set, the module table, the taint and the mode.
func (g *ModuleSignGate) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	data := snapshotData{
		Format:       snapshotFormat,
		TrustedRoots: sortedKeys(g.roots),
		Policies:     g.sortedPoliciesLocked(),
		Modules:      g.modulesSortedLocked(),
		Audit:        append([]AuditEntry(nil), g.audit...),
		Taint:        g.taint,
		Enforce:      g.enforce,
		EnforceAt:    g.enforceAt,
		Seq:          g.seq,
		MaxModules:   g.cfg.MaxModules,
	}
	canonical, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(canonical)

	envelope := snapshotEnvelope{
		snapshotData: data,
		Checksum:     hex.EncodeToString(checksum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *ModuleSignGate) Restore(data []byte) error {
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	if err := validateSnapshot(snapshot, g.cfg.MaxModules); err != nil {
		return err
	}

	roots := make(map[string]struct{}, len(snapshot.TrustedRoots))
	for _, fingerprint := range snapshot.TrustedRoots {
		roots[fingerprint] = struct{}{}
	}

	policies := make(map[string]Policy, len(snapshot.Policies))
	for _, policy := range snapshot.Policies {
		policies[policy.Name] = policy
	}

	modules := make(map[string]LoadedModule, len(snapshot.Modules))
	for _, module := range snapshot.Modules {
		modules[module.Name] = module
	}

	audit := append([]AuditEntry(nil), snapshot.Audit...)

	g.mu.Lock()
	defer g.mu.Unlock()

	taint := snapshot.Taint
	if g.taint.Bits != 0 {
		taint.Bits |= g.taint.Bits
		switch {
		case snapshot.Taint.Bits == 0:
			taint.Since = g.taint.Since
		case snapshot.Taint.Since < g.taint.Since:
			taint.Since = snapshot.Taint.Since
		default:
			taint.Since = g.taint.Since
		}
	}

	g.roots = roots
	g.policies = policies
	g.modules = modules
	g.audit = audit
	g.taint = taint
	g.enforce = snapshot.Enforce
	g.enforceAt = snapshot.EnforceAt
	g.seq = snapshot.Seq
	return nil
}

func (g *ModuleSignGate) verifyChainLocked(req ModuleRequest) (ChainReport, error) {
	chain := req.Signature.Chain
	if len(chain) == 0 {
		return ChainReport{}, ErrChainBroken
	}

	for i := 0; i+1 < len(chain); i++ {
		if chain[i].Issuer != chain[i+1].Subject {
			return ChainReport{}, ErrChainBroken
		}
	}

	rootFingerprint := chain[len(chain)-1].Fingerprint
	if _, trusted := g.roots[rootFingerprint]; !trusted {
		return ChainReport{}, ErrUntrustedRoot
	}

	for _, cert := range chain {
		if req.At > cert.NotAfter {
			return ChainReport{}, ErrCertExpired
		}
	}

	return ChainReport{
		RootFingerprint: rootFingerprint,
		Links:           len(chain),
		Valid:           true,
	}, nil
}

func (g *ModuleSignGate) policyExemptLocked(req ModuleRequest) bool {
	policy, ok := g.policies[req.Name]
	return ok && policy.Allow && policy.Hash == req.Hash
}

func (g *ModuleSignGate) appendAuditLocked(name, outcome, detail string, at int64) int64 {
	g.seq++
	entry := AuditEntry{
		Name:    name,
		Outcome: outcome,
		Detail:  detail,
		Seq:     g.seq,
		At:      at,
	}
	g.audit = append(g.audit, entry)
	return g.seq
}

func (g *ModuleSignGate) raiseTaintLocked(bits uint32, at int64) {
	if bits == 0 {
		return
	}
	if g.taint.Bits == 0 {
		g.taint.Bits = bits
		g.taint.Since = at
		return
	}
	g.taint.Bits |= bits
}

func (g *ModuleSignGate) sortedPoliciesLocked() []Policy {
	names := make([]string, 0, len(g.policies))
	for name := range g.policies {
		names = append(names, name)
	}
	sort.Strings(names)

	policies := make([]Policy, 0, len(names))
	for _, name := range names {
		policies = append(policies, g.policies[name])
	}
	return policies
}

func (g *ModuleSignGate) modulesSortedLocked() []LoadedModule {
	names := make([]string, 0, len(g.modules))
	for name := range g.modules {
		names = append(names, name)
	}
	sort.Strings(names)

	modules := make([]LoadedModule, 0, len(names))
	for _, name := range names {
		modules = append(modules, g.modules[name])
	}
	return modules
}

func validateModuleIdentity(req ModuleRequest) error {
	if !validModuleName(req.Name) || strings.TrimSpace(req.Version) == "" || strings.TrimSpace(req.Hash) == "" {
		return ErrBadModule
	}
	return nil
}

func validModuleName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return true
}

func auditDetail(req ModuleRequest, rootFingerprint string, policyExempt bool) string {
	parts := []string{"fingerprint=" + req.Hash}
	if req.SignerKey != "" {
		parts = append(parts, "signer="+req.SignerKey)
	}
	if rootFingerprint != "" {
		parts = append(parts, "root="+rootFingerprint)
	}
	if policyExempt {
		parts = append(parts, "policy_exempt=true")
	}
	return strings.Join(parts, ";")
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func decodeSnapshot(data []byte) (snapshotData, error) {
	var envelope snapshotEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return snapshotData{}, fmt.Errorf("%w: decode: %v", ErrBadSnapshot, err)
	}

	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return snapshotData{}, fmt.Errorf("%w: trailing payload", ErrBadSnapshot)
	}

	canonical, err := json.Marshal(envelope.snapshotData)
	if err != nil {
		return snapshotData{}, fmt.Errorf("%w: encode: %v", ErrBadSnapshot, err)
	}
	checksum := sha256.Sum256(canonical)
	if envelope.Checksum != hex.EncodeToString(checksum[:]) {
		return snapshotData{}, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}
	return envelope.snapshotData, nil
}

func validateSnapshot(snapshot snapshotData, currentMax int) error {
	if snapshot.Format != snapshotFormat {
		return fmt.Errorf("%w: unsupported format", ErrBadSnapshot)
	}
	if snapshot.Seq < 0 || snapshot.EnforceAt < 0 || snapshot.MaxModules <= 0 {
		return fmt.Errorf("%w: negative sequence or time", ErrBadSnapshot)
	}
	if snapshot.Taint.Bits&^validTaintBits != 0 || snapshot.Taint.Since < 0 {
		return fmt.Errorf("%w: invalid taint", ErrBadSnapshot)
	}
	if currentMax > 0 && (len(snapshot.Modules) > currentMax || len(snapshot.Policies) > currentMax) {
		return fmt.Errorf("%w: exceeds configured cap", ErrBadSnapshot)
	}
	if len(snapshot.Modules) > snapshot.MaxModules || len(snapshot.Policies) > snapshot.MaxModules {
		return fmt.Errorf("%w: exceeds snapshot cap", ErrBadSnapshot)
	}

	roots := make(map[string]struct{}, len(snapshot.TrustedRoots))
	for _, fingerprint := range snapshot.TrustedRoots {
		if strings.TrimSpace(fingerprint) == "" {
			return fmt.Errorf("%w: empty root", ErrBadSnapshot)
		}
		if _, exists := roots[fingerprint]; exists {
			return fmt.Errorf("%w: duplicate root", ErrBadSnapshot)
		}
		roots[fingerprint] = struct{}{}
	}

	policyNames := make(map[string]struct{}, len(snapshot.Policies))
	for _, policy := range snapshot.Policies {
		if !validModuleName(policy.Name) || strings.TrimSpace(policy.Hash) == "" {
			return fmt.Errorf("%w: invalid policy", ErrBadSnapshot)
		}
		if _, exists := policyNames[policy.Name]; exists {
			return fmt.Errorf("%w: duplicate policy", ErrBadSnapshot)
		}
		policyNames[policy.Name] = struct{}{}
	}

	moduleNames := make(map[string]struct{}, len(snapshot.Modules))
	var maxSeq int64
	for _, module := range snapshot.Modules {
		if !validModuleName(module.Name) || strings.TrimSpace(module.Version) == "" || strings.TrimSpace(module.Hash) == "" {
			return fmt.Errorf("%w: invalid module", ErrBadSnapshot)
		}
		if module.Seq <= 0 || module.LoadedAt < 0 {
			return fmt.Errorf("%w: invalid module sequence", ErrBadSnapshot)
		}
		if _, exists := moduleNames[module.Name]; exists {
			return fmt.Errorf("%w: duplicate module", ErrBadSnapshot)
		}
		moduleNames[module.Name] = struct{}{}
		if module.Seq > maxSeq {
			maxSeq = module.Seq
		}
	}

	var previousSeq int64
	for _, entry := range snapshot.Audit {
		if entry.Seq <= previousSeq || entry.At < 0 || strings.TrimSpace(entry.Outcome) == "" {
			return fmt.Errorf("%w: invalid audit sequence", ErrBadSnapshot)
		}
		previousSeq = entry.Seq
		if entry.Seq > maxSeq {
			maxSeq = entry.Seq
		}
	}
	if snapshot.Seq < maxSeq {
		return fmt.Errorf("%w: sequence is behind state", ErrBadSnapshot)
	}

	return nil
}
