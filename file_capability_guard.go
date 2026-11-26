// Package task keeps the file capability table of a fleet of edge hosts:
// it converges grants to a whitelist, tracks inherit bits that survive exec,
// reports policy drift and repairs it without widening the approved set.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Violation kinds reported by Reconcile.
const (
	ViolationUnlistedCap      = "unlisted_cap"
	ViolationUnboundedInherit = "unbounded_inherit"
	ViolationPrivilegeBit     = "privilege_bit"
)

// Errors returned by the guard.
var (
	ErrBadPolicy      = errors.New("capability policy is invalid")
	ErrBlankPath      = errors.New("file path is required")
	ErrDuplicateFile  = errors.New("file is already registered")
	ErrFileLimit      = errors.New("registered file limit reached")
	ErrUnknownFile    = errors.New("file is not registered")
	ErrCapNotAllowed  = errors.New("capability is not whitelisted")
	ErrCapLimit       = errors.New("capability limit reached for this file")
	ErrCapAbsent      = errors.New("capability is not granted on this file")
	ErrSetUIDWithCaps = errors.New("setuid bit cannot coexist with capabilities")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// CapabilityPolicy is the approved capability surface of one host class.
type CapabilityPolicy struct {
	Allowed        []string `json:"allowed"`
	InheritAllowed []string `json:"inherit_allowed"`
	MaxPerFile     int      `json:"max_per_file"`
	MaxFiles       int      `json:"max_files"`
}

// FileFlags carries the privilege bits stored next to the capability set.
type FileFlags struct {
	SetUID  bool `json:"set_uid"`
	SetGID  bool `json:"set_gid"`
	Inherit bool `json:"inherit"`
}

// FileRecord is the per path row kept by the guard.
type FileRecord struct {
	Path  string    `json:"path"`
	Caps  []string  `json:"caps"`
	Flags FileFlags `json:"flags"`
	Gen   int64     `json:"gen"`
}

// Violation is one drift finding produced by Reconcile.
type Violation struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Cap  string `json:"cap"`
}

// RepairReport describes what Repair changed on one path.
type RepairReport struct {
	RemovedCaps    []string `json:"removed_caps"`
	ClearedInherit bool     `json:"cleared_inherit"`
	ClearedSetUID  bool     `json:"cleared_set_uid"`
	Gen            int64    `json:"gen"`
}

// FileCapabilityGuard owns the file table and the active policy.
type FileCapabilityGuard struct {
	mu             sync.RWMutex
	policy         CapabilityPolicy
	allowed        map[string]struct{}
	inheritAllowed map[string]struct{}
	files          map[string]*FileRecord
}

type snapshotData struct {
	Version        int               `json:"version,omitempty"`
	Policy         *CapabilityPolicy `json:"policy,omitempty"`
	Allowed        []string          `json:"allowed,omitempty"`
	InheritAllowed []string          `json:"inherit_allowed,omitempty"`
	MaxPerFile     int               `json:"max_per_file,omitempty"`
	MaxFiles       int               `json:"max_files,omitempty"`
	Files          []FileRecord      `json:"files"`
}

// NewFileCapabilityGuard builds a guard around an approved policy.
func NewFileCapabilityGuard(policy CapabilityPolicy) *FileCapabilityGuard {
	policy = clonePolicy(policy)
	allowed, inheritAllowed := policyIndexes(policy)
	return &FileCapabilityGuard{
		policy:         policy,
		allowed:        allowed,
		inheritAllowed: inheritAllowed,
		files:          make(map[string]*FileRecord),
	}
}

// RegisterFile adds a path with its approved capability set and bits.
func (g *FileCapabilityGuard) RegisterFile(path string, caps []string, flags FileFlags) error {
	if blankPath(path) {
		return ErrBlankPath
	}

	normalizedCaps := normalizeCaps(caps)

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.files[path]; exists {
		return ErrDuplicateFile
	}
	if len(g.files) >= g.policy.MaxFiles {
		return ErrFileLimit
	}
	for _, capability := range normalizedCaps {
		if _, allowed := g.allowed[capability]; !allowed {
			return ErrCapNotAllowed
		}
	}
	if len(normalizedCaps) > g.policy.MaxPerFile {
		return ErrCapLimit
	}
	if flags.SetUID && len(normalizedCaps) > 0 {
		return ErrSetUIDWithCaps
	}

	g.files[path] = &FileRecord{
		Path:  path,
		Caps:  normalizedCaps,
		Flags: flags,
		Gen:   1,
	}
	return nil
}

// RemoveFile drops one path from the table.
func (g *FileCapabilityGuard) RemoveFile(path string) error {
	if blankPath(path) {
		return ErrBlankPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.files[path]; !exists {
		return ErrUnknownFile
	}
	delete(g.files, path)
	return nil
}

// Grant adds one capability to an already registered path.
func (g *FileCapabilityGuard) Grant(path, capability string) error {
	if blankPath(path) {
		return ErrBlankPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	record, exists := g.files[path]
	if !exists {
		return ErrUnknownFile
	}
	if hasCapability(record.Caps, capability) {
		return nil
	}
	if _, allowed := g.allowed[capability]; !allowed {
		return ErrCapNotAllowed
	}
	if len(record.Caps) >= g.policy.MaxPerFile {
		return ErrCapLimit
	}

	record.Caps = append(record.Caps, capability)
	sort.Strings(record.Caps)
	if record.Flags.SetUID {
		record.Flags.SetUID = false
	}
	record.Gen++
	return nil
}

// Revoke removes one capability from an already registered path.
func (g *FileCapabilityGuard) Revoke(path, capability string) error {
	if blankPath(path) {
		return ErrBlankPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	record, exists := g.files[path]
	if !exists {
		return ErrUnknownFile
	}
	index := sort.SearchStrings(record.Caps, capability)
	if index >= len(record.Caps) || record.Caps[index] != capability {
		return ErrCapAbsent
	}

	record.Caps = append(record.Caps[:index], record.Caps[index+1:]...)
	record.Gen++
	return nil
}

// SetFlags replaces the privilege bits of one path.
func (g *FileCapabilityGuard) SetFlags(path string, flags FileFlags) error {
	if blankPath(path) {
		return ErrBlankPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	record, exists := g.files[path]
	if !exists {
		return ErrUnknownFile
	}
	if record.Flags == flags {
		return nil
	}
	if flags.SetUID && len(record.Caps) > 0 {
		return ErrSetUIDWithCaps
	}

	record.Flags = flags
	record.Gen++
	return nil
}

// File reads one row.
func (g *FileCapabilityGuard) File(path string) (FileRecord, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	record, exists := g.files[path]
	if !exists {
		return FileRecord{}, false
	}
	return cloneRecord(record), true
}

// Files returns every row ordered by path.
func (g *FileCapabilityGuard) Files() []FileRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()

	paths := make([]string, 0, len(g.files))
	for path := range g.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	records := make([]FileRecord, 0, len(paths))
	for _, path := range paths {
		records = append(records, cloneRecord(g.files[path]))
	}
	return records
}

// EffectiveCaps is the capability set that actually reaches exec.
func (g *FileCapabilityGuard) EffectiveCaps(path string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	record, exists := g.files[path]
	if !exists {
		return []string{}
	}

	caps := make([]string, 0, len(record.Caps))
	for _, capability := range record.Caps {
		if _, allowed := g.allowed[capability]; !allowed {
			continue
		}
		if record.Flags.Inherit {
			if _, inheritable := g.inheritAllowed[capability]; !inheritable {
				continue
			}
		}
		caps = append(caps, capability)
	}
	return caps
}

// ReloadPolicy swaps the whitelist without touching the stored rows.
func (g *FileCapabilityGuard) ReloadPolicy(policy CapabilityPolicy) error {
	if !validPolicy(policy) {
		return ErrBadPolicy
	}

	policy = clonePolicy(policy)
	allowed, inheritAllowed := policyIndexes(policy)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = policy
	g.allowed = allowed
	g.inheritAllowed = inheritAllowed
	return nil
}

// Reconcile lists every row that drifted away from the active policy.
func (g *FileCapabilityGuard) Reconcile() []Violation {
	g.mu.RLock()
	defer g.mu.RUnlock()

	paths := make([]string, 0, len(g.files))
	for path := range g.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	findings := make([]Violation, 0)
	for _, path := range paths {
		record := g.files[path]

		if record.Flags.SetUID && len(record.Caps) > 0 {
			findings = append(findings, Violation{
				Path: path,
				Kind: ViolationPrivilegeBit,
			})
		}

		for _, capability := range record.Caps {
			if _, allowed := g.allowed[capability]; !allowed {
				findings = append(findings, Violation{
					Path: path,
					Kind: ViolationUnlistedCap,
					Cap:  capability,
				})
			}
			if record.Flags.Inherit {
				if _, inheritable := g.inheritAllowed[capability]; !inheritable {
					findings = append(findings, Violation{
						Path: path,
						Kind: ViolationUnboundedInherit,
						Cap:  capability,
					})
				}
			}
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Cap < findings[j].Cap
	})
	return findings
}

// Repair converges one row onto the active policy.
func (g *FileCapabilityGuard) Repair(path string) (RepairReport, error) {
	if blankPath(path) {
		return RepairReport{}, ErrBlankPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	record, exists := g.files[path]
	if !exists {
		return RepairReport{}, ErrUnknownFile
	}

	report := RepairReport{RemovedCaps: []string{}}
	changed := false
	hadCaps := len(record.Caps) > 0

	keptCaps := make([]string, 0, len(record.Caps))
	for _, capability := range record.Caps {
		if _, allowed := g.allowed[capability]; !allowed {
			report.RemovedCaps = append(report.RemovedCaps, capability)
			changed = true
			continue
		}
		keptCaps = append(keptCaps, capability)
	}
	record.Caps = keptCaps

	if record.Flags.Inherit {
		for _, capability := range record.Caps {
			if _, inheritable := g.inheritAllowed[capability]; !inheritable {
				record.Flags.Inherit = false
				report.ClearedInherit = true
				changed = true
				break
			}
		}
	}

	if record.Flags.SetUID && hadCaps {
		record.Flags.SetUID = false
		report.ClearedSetUID = true
		changed = true
	}

	if changed {
		record.Gen++
	}
	report.Gen = record.Gen
	return report, nil
}

// Snapshot serialises the policy and every row.
func (g *FileCapabilityGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()

	policy := clonePolicy(g.policy)
	paths := make([]string, 0, len(g.files))
	for path := range g.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	files := make([]FileRecord, 0, len(paths))
	for _, path := range paths {
		files = append(files, cloneRecord(g.files[path]))
	}
	g.mu.RUnlock()

	payload := snapshotData{
		Version:        1,
		Policy:         &policy,
		Allowed:        policy.Allowed,
		InheritAllowed: policy.InheritAllowed,
		MaxPerFile:     policy.MaxPerFile,
		MaxFiles:       policy.MaxFiles,
		Files:          files,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole table and policy, all or nothing.
func (g *FileCapabilityGuard) Restore(data []byte) error {
	var payload snapshotData
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	policy := payload.resolvedPolicy()
	if !validPolicy(policy) {
		return fmt.Errorf("%w: invalid policy", ErrBadSnapshot)
	}

	nextFiles := make(map[string]*FileRecord, len(payload.Files))
	for _, record := range payload.Files {
		if blankPath(record.Path) {
			return fmt.Errorf("%w: blank path", ErrBadSnapshot)
		}
		if _, exists := nextFiles[record.Path]; exists {
			return fmt.Errorf("%w: duplicate path %q", ErrBadSnapshot, record.Path)
		}
		if record.Gen < 1 {
			return fmt.Errorf("%w: invalid generation for %q", ErrBadSnapshot, record.Path)
		}

		record.Caps = normalizeCaps(record.Caps)
		copyRecord := record
		copyRecord.Caps = append([]string{}, record.Caps...)
		nextFiles[record.Path] = &copyRecord
	}

	policy = clonePolicy(policy)
	allowed, inheritAllowed := policyIndexes(policy)

	g.mu.Lock()
	g.policy = policy
	g.allowed = allowed
	g.inheritAllowed = inheritAllowed
	g.files = nextFiles
	g.mu.Unlock()
	return nil
}

func blankPath(path string) bool {
	return strings.TrimSpace(path) == ""
}

func clonePolicy(policy CapabilityPolicy) CapabilityPolicy {
	cloned := policy
	cloned.Allowed = append([]string{}, policy.Allowed...)
	cloned.InheritAllowed = append([]string{}, policy.InheritAllowed...)
	if cloned.Allowed == nil {
		cloned.Allowed = []string{}
	}
	if cloned.InheritAllowed == nil {
		cloned.InheritAllowed = []string{}
	}
	return cloned
}

func policyIndexes(policy CapabilityPolicy) (map[string]struct{}, map[string]struct{}) {
	allowed := make(map[string]struct{}, len(policy.Allowed))
	for _, capability := range policy.Allowed {
		allowed[capability] = struct{}{}
	}
	inheritAllowed := make(map[string]struct{}, len(policy.InheritAllowed))
	for _, capability := range policy.InheritAllowed {
		inheritAllowed[capability] = struct{}{}
	}
	return allowed, inheritAllowed
}

func validPolicy(policy CapabilityPolicy) bool {
	if len(policy.Allowed) == 0 || policy.MaxPerFile <= 0 || policy.MaxFiles <= 0 {
		return false
	}

	allowed := make(map[string]struct{}, len(policy.Allowed))
	for _, capability := range policy.Allowed {
		allowed[capability] = struct{}{}
	}
	for _, capability := range policy.InheritAllowed {
		if _, ok := allowed[capability]; !ok {
			return false
		}
	}
	return true
}

func normalizeCaps(caps []string) []string {
	seen := make(map[string]struct{}, len(caps))
	normalized := make([]string, 0, len(caps))
	for _, capability := range caps {
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		normalized = append(normalized, capability)
	}
	sort.Strings(normalized)
	return normalized
}

func hasCapability(caps []string, capability string) bool {
	index := sort.SearchStrings(caps, capability)
	return index < len(caps) && caps[index] == capability
}

func cloneRecord(record *FileRecord) FileRecord {
	cloned := *record
	cloned.Caps = append([]string{}, record.Caps...)
	if cloned.Caps == nil {
		cloned.Caps = []string{}
	}
	return cloned
}

func (s snapshotData) resolvedPolicy() CapabilityPolicy {
	if s.Policy != nil {
		return *s.Policy
	}
	return CapabilityPolicy{
		Allowed:        s.Allowed,
		InheritAllowed: s.InheritAllowed,
		MaxPerFile:     s.MaxPerFile,
		MaxFiles:       s.MaxFiles,
	}
}
