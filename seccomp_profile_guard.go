// Package task merges seccomp allow lists so a profile can only ever be
// tightened, then applies it with NO_NEW_PRIVS and full thread coverage.
package task

import "errors"

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

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
	policy GuardPolicy
}

// NewSeccompProfileGuard builds a guard and fills the unset policy defaults.
func NewSeccompProfileGuard(policy GuardPolicy) *SeccompProfileGuard {
	return &SeccompProfileGuard{policy: policy}
}

// Policy returns the effective policy after the defaults are filled.
func (g *SeccompProfileGuard) Policy() GuardPolicy {
	return GuardPolicy{}
}

// Validate checks a profile without installing it.
func (g *SeccompProfileGuard) Validate(profile Profile) error {
	return errNotImplemented
}

// Merge folds a narrowed profile into the baseline.
func (g *SeccompProfileGuard) Merge(base, next Profile) (MergeResult, error) {
	return MergeResult{}, errNotImplemented
}

// Tighten replaces the action of the given syscalls on a copy of the profile.
func (g *SeccompProfileGuard) Tighten(profile Profile, rules []SyscallRule) (Profile, error) {
	return Profile{}, errNotImplemented
}

// SortRules returns the rules ordered by syscall name without touching input.
func (g *SeccompProfileGuard) SortRules(rules []SyscallRule) []SyscallRule {
	return nil
}

// RuleFor reads the rule attached to one syscall.
func (g *SeccompProfileGuard) RuleFor(profile Profile, name string) (SyscallRule, bool) {
	return SyscallRule{}, false
}

// CheckEscape lists protected syscalls the profile would allow.
func (g *SeccompProfileGuard) CheckEscape(profile Profile) []string {
	return nil
}

// Apply installs the profile after every gate has passed.
func (g *SeccompProfileGuard) Apply(profile Profile) (ApplyReport, error) {
	return ApplyReport{}, errNotImplemented
}

// Applied reads the profile currently installed.
func (g *SeccompProfileGuard) Applied() (Profile, bool) {
	return Profile{}, false
}

// Stats returns the guard counters.
func (g *SeccompProfileGuard) Stats() GuardStats {
	return GuardStats{}
}

// Snapshot serialises the applied profile and the counters.
func (g *SeccompProfileGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SeccompProfileGuard) Restore(data []byte) error {
	return errNotImplemented
}
