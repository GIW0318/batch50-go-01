// Package task merges seccomp allow lists so a profile can only ever be
// tightened, then applies it with NO_NEW_PRIVS and full thread coverage.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
)

// Rule actions accepted by the filter.
const (
	ActionAllow = "allow"
	ActionErrno = "errno"
	ActionKill  = "kill"
	ActionTrace = "trace"
	ActionLog   = "log"
)

// Defaults applied when the guard policy leaves a field unset.
const (
	DefaultMaxRules   = 128
	DefaultMaxThreads = 64
)

// defaultProtectedSyscalls is the built-in escape hatch watchlist, ascending.
var defaultProtectedSyscalls = []string{"bpf", "kexec_load", "ptrace", "userfaultfd"}

// Errors reported by the guard.
var (
	ErrBlankProfile      = errors.New("profile name is required")
	ErrBlankSyscall      = errors.New("syscall name is required")
	ErrBadAction         = errors.New("rule action is not recognised")
	ErrPermissiveDefault = errors.New("default action must deny")
	ErrProtectedSyscall  = errors.New("protected syscall cannot be allowed")
	ErrDuplicateRule     = errors.New("syscall has more than one rule")
	ErrRuleLimit         = errors.New("rule budget is exhausted")
	ErrWidening          = errors.New("merged profile would widen the filter")
	ErrNoNewPrivs        = errors.New("no_new_privs is required")
	ErrBadThreadCount    = errors.New("thread count must be positive")
	ErrThreadLimit       = errors.New("thread count exceeds the budget")
	ErrNotAllThreads     = errors.New("filter is not applied to every thread")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// SyscallRule is one entry of a seccomp allow list.
type SyscallRule struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Errno  int    `json:"errno"`
}

// Profile is the filter handed to the runtime.
type Profile struct {
	Name          string        `json:"name"`
	DefaultAction string        `json:"default_action"`
	Rules         []SyscallRule `json:"rules"`
	NoNewPrivs    bool          `json:"no_new_privs"`
	AllThreads    bool          `json:"all_threads"`
	ThreadCount   int           `json:"thread_count"`
}

// MergeResult is the auditable outcome of one merge.
type MergeResult struct {
	Profile Profile  `json:"profile"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Widened []string `json:"widened"`
}

// ApplyReport describes what the runtime installed.
type ApplyReport struct {
	Profile string   `json:"profile"`
	Threads int      `json:"threads"`
	Blocked int      `json:"blocked"`
	Escapes []string `json:"escapes"`
}

// GuardPolicy bounds the guard tables.
type GuardPolicy struct {
	ProtectedSyscalls      []string
	MaxRules               int
	MaxThreads             int
	AllowMissingNoNewPrivs bool
	AllowPartialThreads    bool
}

// GuardStats counts the profiles that went through the guard.
type GuardStats struct {
	Validated int `json:"validated"`
	Merged    int `json:"merged"`
	Applied   int `json:"applied"`
}

// SeccompProfileGuard owns the policy, the applied profile and the counters.
type SeccompProfileGuard struct {
	policy    GuardPolicy
	protected map[string]bool

	mu         sync.Mutex
	applied    Profile
	hasApplied bool
	stats      GuardStats
}

// NewSeccompProfileGuard builds a guard and fills the unset policy defaults.
func NewSeccompProfileGuard(policy GuardPolicy) *SeccompProfileGuard {
	protected := policy.ProtectedSyscalls
	if len(protected) == 0 {
		protected = defaultProtectedSyscalls
	}
	protected = append([]string(nil), protected...)
	sort.Strings(protected)
	policy.ProtectedSyscalls = protected
	if policy.MaxRules <= 0 {
		policy.MaxRules = DefaultMaxRules
	}
	if policy.MaxThreads <= 0 {
		policy.MaxThreads = DefaultMaxThreads
	}
	set := make(map[string]bool, len(protected))
	for _, name := range protected {
		set[name] = true
	}
	return &SeccompProfileGuard{policy: policy, protected: set}
}

// Policy returns the effective policy after the defaults are filled.
func (g *SeccompProfileGuard) Policy() GuardPolicy {
	return g.policy
}

func validAction(action string) bool {
	switch action {
	case ActionAllow, ActionErrno, ActionKill, ActionTrace, ActionLog:
		return true
	}
	return false
}

// validateProfile runs the structural and policy checks. NO_NEW_PRIVS and
// thread coverage are gated later, at Apply time.
func (g *SeccompProfileGuard) validateProfile(profile Profile) error {
	if strings.TrimSpace(profile.Name) == "" {
		return ErrBlankProfile
	}
	if profile.DefaultAction == ActionAllow {
		return ErrPermissiveDefault
	}
	if !validAction(profile.DefaultAction) {
		return ErrBadAction
	}
	if len(profile.Rules) > g.policy.MaxRules {
		return ErrRuleLimit
	}
	seen := make(map[string]bool, len(profile.Rules))
	for _, rule := range profile.Rules {
		if strings.TrimSpace(rule.Name) == "" {
			return ErrBlankSyscall
		}
		if !validAction(rule.Action) {
			return ErrBadAction
		}
		if seen[rule.Name] {
			return ErrDuplicateRule
		}
		seen[rule.Name] = true
		if rule.Action == ActionAllow && g.protected[rule.Name] {
			return ErrProtectedSyscall
		}
	}
	return nil
}

// Validate checks a profile without installing it.
func (g *SeccompProfileGuard) Validate(profile Profile) error {
	if err := g.validateProfile(profile); err != nil {
		return err
	}
	g.mu.Lock()
	g.stats.Validated++
	g.mu.Unlock()
	return nil
}

// Merge folds a narrowed profile into the baseline. The merge is rejected as
// a whole when next would allow anything the baseline did not already allow.
func (g *SeccompProfileGuard) Merge(base, next Profile) (MergeResult, error) {
	if err := g.validateProfile(base); err != nil {
		return MergeResult{}, err
	}
	if err := g.validateProfile(next); err != nil {
		return MergeResult{}, err
	}

	baseRules := make(map[string]SyscallRule, len(base.Rules))
	baseAllow := make(map[string]bool, len(base.Rules))
	for _, rule := range base.Rules {
		baseRules[rule.Name] = rule
		if rule.Action == ActionAllow {
			baseAllow[rule.Name] = true
		}
	}
	nextAllow := make(map[string]bool, len(next.Rules))
	var widened []string
	for _, rule := range next.Rules {
		if rule.Action == ActionAllow {
			nextAllow[rule.Name] = true
			if !baseAllow[rule.Name] {
				widened = append(widened, rule.Name)
			}
		}
	}
	if len(widened) > 0 {
		sort.Strings(widened)
		return MergeResult{Widened: widened}, ErrWidening
	}

	var added, removed []string
	for _, rule := range next.Rules {
		if _, ok := baseRules[rule.Name]; !ok {
			added = append(added, rule.Name)
		}
	}
	for _, rule := range base.Rules {
		if rule.Action == ActionAllow && !nextAllow[rule.Name] {
			removed = append(removed, rule.Name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	merged := next
	merged.Rules = g.SortRules(next.Rules)

	g.mu.Lock()
	g.stats.Merged++
	g.mu.Unlock()
	return MergeResult{Profile: merged, Added: added, Removed: removed}, nil
}

// Tighten replaces the action of the given syscalls on a copy of the profile.
func (g *SeccompProfileGuard) Tighten(profile Profile, rules []SyscallRule) (Profile, error) {
	replacements := make(map[string]SyscallRule, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.Name) == "" {
			return Profile{}, ErrBlankSyscall
		}
		if !validAction(rule.Action) {
			return Profile{}, ErrBadAction
		}
		if _, dup := replacements[rule.Name]; dup {
			return Profile{}, ErrDuplicateRule
		}
		if rule.Action == ActionAllow && g.protected[rule.Name] {
			return Profile{}, ErrProtectedSyscall
		}
		replacements[rule.Name] = rule
	}
	out := profile
	out.Rules = make([]SyscallRule, len(profile.Rules))
	copy(out.Rules, profile.Rules)
	for i := range out.Rules {
		if rule, ok := replacements[out.Rules[i].Name]; ok {
			out.Rules[i] = rule
		}
	}
	return out, nil
}

// SortRules returns the rules ordered by syscall name without touching input.
func (g *SeccompProfileGuard) SortRules(rules []SyscallRule) []SyscallRule {
	out := make([]SyscallRule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RuleFor reads the rule attached to one syscall.
func (g *SeccompProfileGuard) RuleFor(profile Profile, name string) (SyscallRule, bool) {
	for _, rule := range profile.Rules {
		if rule.Name == name {
			return rule, true
		}
	}
	return SyscallRule{}, false
}

// CheckEscape lists protected syscalls the profile would allow.
func (g *SeccompProfileGuard) CheckEscape(profile Profile) []string {
	allowed := make(map[string]bool, len(profile.Rules))
	ruled := make(map[string]bool, len(profile.Rules))
	for _, rule := range profile.Rules {
		ruled[rule.Name] = true
		if rule.Action == ActionAllow {
			allowed[rule.Name] = true
		}
	}
	var escapes []string
	for _, name := range g.policy.ProtectedSyscalls {
		if allowed[name] || (profile.DefaultAction == ActionAllow && !ruled[name]) {
			escapes = append(escapes, name)
		}
	}
	sort.Strings(escapes)
	return escapes
}

func countBlocked(rules []SyscallRule) int {
	blocked := 0
	for _, rule := range rules {
		if rule.Action != ActionAllow {
			blocked++
		}
	}
	return blocked
}

func cloneProfile(profile Profile) Profile {
	out := profile
	out.Rules = append([]SyscallRule(nil), profile.Rules...)
	return out
}

func sameProfile(a, b Profile) bool {
	if a.Name != b.Name || a.DefaultAction != b.DefaultAction ||
		a.NoNewPrivs != b.NoNewPrivs || a.AllThreads != b.AllThreads ||
		a.ThreadCount != b.ThreadCount || len(a.Rules) != len(b.Rules) {
		return false
	}
	rules := make(map[string]SyscallRule, len(a.Rules))
	for _, rule := range a.Rules {
		rules[rule.Name] = rule
	}
	for _, rule := range b.Rules {
		other, ok := rules[rule.Name]
		if !ok || other != rule {
			return false
		}
	}
	return true
}

// Apply installs the profile after every gate has passed.
func (g *SeccompProfileGuard) Apply(profile Profile) (ApplyReport, error) {
	if err := g.validateProfile(profile); err != nil {
		return ApplyReport{}, err
	}
	if !profile.NoNewPrivs && !g.policy.AllowMissingNoNewPrivs {
		return ApplyReport{}, ErrNoNewPrivs
	}
	if profile.ThreadCount <= 0 {
		return ApplyReport{}, ErrBadThreadCount
	}
	if profile.ThreadCount > g.policy.MaxThreads {
		return ApplyReport{}, ErrThreadLimit
	}
	if profile.ThreadCount > 1 && !profile.AllThreads && !g.policy.AllowPartialThreads {
		return ApplyReport{}, ErrNotAllThreads
	}
	report := ApplyReport{
		Profile: profile.Name,
		Threads: profile.ThreadCount,
		Blocked: countBlocked(profile.Rules),
		Escapes: g.CheckEscape(profile),
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hasApplied && sameProfile(g.applied, profile) {
		return report, nil
	}
	g.applied = cloneProfile(profile)
	g.hasApplied = true
	g.stats.Applied++
	return report, nil
}

// Applied reads the profile currently installed.
func (g *SeccompProfileGuard) Applied() (Profile, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.hasApplied {
		return Profile{}, false
	}
	return cloneProfile(g.applied), true
}

// Stats returns the guard counters.
func (g *SeccompProfileGuard) Stats() GuardStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

type snapshot struct {
	Applied *Profile   `json:"applied"`
	Stats   GuardStats `json:"stats"`
}

// Snapshot serialises the applied profile and the counters.
func (g *SeccompProfileGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	payload := snapshot{Stats: g.stats}
	if g.hasApplied {
		applied := cloneProfile(g.applied)
		payload.Applied = &applied
	}
	return json.Marshal(payload)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SeccompProfileGuard) Restore(data []byte) error {
	var payload snapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return ErrBadSnapshot
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return ErrBadSnapshot
	}
	if payload.Stats.Validated < 0 || payload.Stats.Merged < 0 || payload.Stats.Applied < 0 {
		return ErrBadSnapshot
	}
	if payload.Applied != nil {
		if err := g.validateProfile(*payload.Applied); err != nil {
			return ErrBadSnapshot
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if payload.Applied != nil {
		g.applied = cloneProfile(*payload.Applied)
		g.hasApplied = true
	} else {
		g.applied = Profile{}
		g.hasApplied = false
	}
	g.stats = payload.Stats
	return nil
}
