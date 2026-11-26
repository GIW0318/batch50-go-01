// Package task keeps kernel boot parameter baselines per cluster node, plans
// generation scoped changes against the live entry, and compares a written
// generation with the command line the kernel reports after a restart.
package task

import (
	"errors"
	"sort"
	"sync"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Node roles drive which baseline a node receives.
const (
	RoleCompute = "compute"
	RoleStorage = "storage"
	RoleControl = "control"
)

// Boot parameter names that the policy understands.
const (
	ParamAudit           = "audit"
	ParamConsoleLevel    = "console_loglevel"
	ParamIOMMU           = "iommu"
	ParamMitigations     = "mitigations"
	ParamModuleBlacklist = "module_blacklist"
	ParamNUMABalancing   = "numa_balancing"
	ParamPanic           = "panic"
	ParamRescueShell     = "rescue_shell"
	ParamTHP             = "transparent_hugepage"
)

// Errors returned by the policy.
var (
	ErrUnknownNode      = errors.New("node is not registered")
	ErrBadRole          = errors.New("unknown node role")
	ErrForbiddenParam   = errors.New("parameter is outside the allowed baseline")
	ErrParamLimit       = errors.New("parameter set exceeds the configured limits")
	ErrBadPlan          = errors.New("change plan does not belong to this node")
	ErrStalePlan        = errors.New("change plan was built against an older generation")
	ErrNoGeneration     = errors.New("no retained generation to roll back to")
	ErrBatchAborted     = errors.New("batch stopped at the first failing batch")
	ErrBadRunningParams = errors.New("reported command line is not parseable")
)

// Param is one key value pair of a kernel command line.
type Param struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ParamSet is an ordered list of parameters. Order is preserved so that
// rendering a boot entry stays reproducible.
type ParamSet struct {
	Entries []Param `json:"entries"`
}

// Get returns the value stored for key.
func (p ParamSet) Get(key string) (string, bool) {
	for _, e := range p.Entries {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}

// Keys returns the parameter names in ascending order.
func (p ParamSet) Keys() []string {
	keys := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		keys = append(keys, e.Key)
	}
	sort.Strings(keys)
	return keys
}

// Len reports how many parameters the set holds.
func (p ParamSet) Len() int { return len(p.Entries) }

// PolicyLimits bounds the live tables of the policy.
type PolicyLimits struct {
	MaxParams      int
	MaxValueLen    int
	MaxGenerations int
	BatchSize      int
}

// Generation is one written boot entry plus the parameters it carries.
type Generation struct {
	Index    int      `json:"index"`
	Params   ParamSet `json:"params"`
	Rendered string   `json:"rendered"`
	Live     bool     `json:"live"`
}

// ChangePlan is the increment between the current generation and a wanted set.
type ChangePlan struct {
	NodeID      string   `json:"node_id"`
	Generation  int      `json:"generation"`
	Role        string   `json:"role"`
	Add         []Param  `json:"add"`
	Remove      []string `json:"remove"`
	Fingerprint string   `json:"fingerprint"`
}

// ParamDiff is one key whose written value and reported value disagree.
type ParamDiff struct {
	Key  string `json:"key"`
	Want string `json:"want"`
	Got  string `json:"got"`
}

// Drift is the difference between the newest generation and what runs now.
type Drift struct {
	NodeID  string      `json:"node_id"`
	Missing []Param     `json:"missing"`
	Extra   []Param     `json:"extra"`
	Differs []ParamDiff `json:"differs"`
	Pending []string    `json:"pending"`
}

// Clean reports whether the node runs exactly the newest generation.
func (d Drift) Clean() bool {
	return len(d.Missing) == 0 && len(d.Extra) == 0 && len(d.Differs) == 0 && len(d.Pending) == 0
}

// BatchResult records what a batch run managed to commit.
type BatchResult struct {
	Applied []string `json:"applied"`
	Failed  string   `json:"failed"`
}

// PolicyEvent is one line of the per node change log.
type PolicyEvent struct {
	Seq    int64  `json:"seq"`
	NodeID string `json:"node_id"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// BootParamPolicy owns node roles, generations and the reported command lines.
type BootParamPolicy struct {
	limits PolicyLimits
	mu     sync.Mutex
}

// NewBootParamPolicy builds a policy with the given caps.
func NewBootParamPolicy(limits PolicyLimits) *BootParamPolicy {
	return &BootParamPolicy{limits: limits}
}

// RegisterNode registers a node with its role and writes generation zero.
func (p *BootParamPolicy) RegisterNode(nodeID, role string) error {
	return errNotImplemented
}

// SetRole changes the role of a registered node and recomputes its baseline.
func (p *BootParamPolicy) SetRole(nodeID, role string) error {
	return errNotImplemented
}

// Baseline returns the role baseline of a node, sorted by parameter name.
func (p *BootParamPolicy) Baseline(nodeID string) (ParamSet, error) {
	return ParamSet{}, errNotImplemented
}

// Plan evaluates a wanted parameter set and returns the increment to apply.
func (p *BootParamPolicy) Plan(nodeID string, params ParamSet) (ChangePlan, error) {
	return ChangePlan{}, errNotImplemented
}

// Apply writes a planned generation and advances the node generation.
func (p *BootParamPolicy) Apply(nodeID string, plan ChangePlan) error {
	return errNotImplemented
}

// Verify compares the newest generation with the reported running parameters.
func (p *BootParamPolicy) Verify(nodeID string) (Drift, error) {
	return Drift{}, errNotImplemented
}

// Rollback writes a new generation holding the previous retained content.
func (p *BootParamPolicy) Rollback(nodeID string) error {
	return errNotImplemented
}

// ReportRunning records the command line a node reports after a restart.
func (p *BootParamPolicy) ReportRunning(nodeID, cmdline string) error {
	return errNotImplemented
}

// Generations returns the retained generations of a node, oldest first.
func (p *BootParamPolicy) Generations(nodeID string) ([]Generation, error) {
	return nil, errNotImplemented
}

// Current returns the parameters the node currently runs with.
func (p *BootParamPolicy) Current(nodeID string) (ParamSet, error) {
	return ParamSet{}, errNotImplemented
}

// Render returns the boot entry text of one retained generation.
func (p *BootParamPolicy) Render(nodeID string, index int) (string, error) {
	return "", errNotImplemented
}

// BatchApply advances several nodes in batches and stops at the first failure.
func (p *BootParamPolicy) BatchApply(nodeIDs []string, plans map[string]ChangePlan) (BatchResult, error) {
	return BatchResult{}, errNotImplemented
}

// Events returns a copy of the per node change log, oldest first.
func (p *BootParamPolicy) Events(nodeID string) []PolicyEvent {
	return nil
}
