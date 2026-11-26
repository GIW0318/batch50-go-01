// Package task keeps kernel boot parameter baselines per cluster node, plans
// generation scoped changes against the live entry, and compares a written
// generation with the command line the kernel reports after a restart.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

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

const (
	defaultMaxParams      = 256
	defaultMaxValueLen    = 4096
	defaultMaxGenerations = 16
	defaultBatchSize      = 1

	// These are deliberately hard caps rather than truncation points. Kernel
	// command lines and node inventories are bounded control-plane inputs.
	maxNodeCount       = 4096
	maxReportedCmdline = 64 << 10
)

var hotParams = map[string]bool{
	ParamConsoleLevel:  true,
	ParamNUMABalancing: true,
	ParamPanic:         true,
}

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

type nodeState struct {
	role               string
	generations        []Generation
	current            ParamSet
	reportedSinceWrite bool
	events             []PolicyEvent
}

// BootParamPolicy owns node roles, generations and the reported command lines.
type BootParamPolicy struct {
	limits PolicyLimits
	mu     sync.Mutex
	nodes  map[string]*nodeState
	seq    int64
}

// NewBootParamPolicy builds a policy with the given caps.
func NewBootParamPolicy(limits PolicyLimits) *BootParamPolicy {
	if limits.MaxParams <= 0 {
		limits.MaxParams = defaultMaxParams
	}
	if limits.MaxValueLen <= 0 {
		limits.MaxValueLen = defaultMaxValueLen
	}
	if limits.MaxGenerations <= 0 {
		limits.MaxGenerations = defaultMaxGenerations
	}
	if limits.BatchSize <= 0 {
		limits.BatchSize = defaultBatchSize
	}
	return &BootParamPolicy{
		limits: limits,
		nodes:  make(map[string]*nodeState),
	}
}

// RegisterNode registers a node with its role and writes generation zero.
func (p *BootParamPolicy) RegisterNode(nodeID, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !validRole(role) {
		return ErrBadRole
	}
	if p.nodes == nil {
		p.nodes = make(map[string]*nodeState)
	}
	if _, ok := p.nodes[nodeID]; ok {
		// Registration is idempotent and never rewrites an existing role.
		return nil
	}
	if len(p.nodes) >= maxNodeCount {
		return ErrParamLimit
	}

	params, err := p.validateAndNormalize(baselineForRole(role))
	if err != nil {
		return err
	}
	gen := Generation{
		Index:    0,
		Params:   cloneParamSet(params),
		Rendered: renderParamSet(params),
		Live:     true,
	}
	st := &nodeState{
		role:        role,
		generations: []Generation{gen},
		current:     cloneParamSet(params),
	}
	p.nodes[nodeID] = st
	p.recordEventLocked(nodeID, st, "register", role)
	return nil
}

// SetRole changes the role of a registered node and recomputes its baseline.
func (p *BootParamPolicy) SetRole(nodeID, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ErrUnknownNode
	}
	if !validRole(role) {
		return ErrBadRole
	}
	if _, err := p.validateAndNormalize(baselineForRole(role)); err != nil {
		return err
	}
	if st.role == role {
		return nil
	}
	st.role = role
	p.recordEventLocked(nodeID, st, "role", role)
	return nil
}

// Baseline returns the role baseline of a node, sorted by parameter name.
func (p *BootParamPolicy) Baseline(nodeID string) (ParamSet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ParamSet{}, ErrUnknownNode
	}
	return cloneParamSet(normalizedSet(baselineForRole(st.role))), nil
}

// Plan evaluates a wanted parameter set and returns the increment to apply.
func (p *BootParamPolicy) Plan(nodeID string, params ParamSet) (ChangePlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ChangePlan{}, ErrUnknownNode
	}
	want, err := p.validateAndNormalize(params)
	if err != nil {
		return ChangePlan{}, err
	}
	latest := st.generations[len(st.generations)-1]
	current := mapOf(latest.Params)
	wanted := mapOf(want)

	add := make([]Param, 0)
	for _, param := range want.Entries {
		oldValue, exists := current[param.Key]
		if !exists || oldValue != param.Value {
			add = append(add, param)
		}
	}
	remove := make([]string, 0)
	for _, param := range latest.Params.Entries {
		if _, exists := wanted[param.Key]; !exists {
			remove = append(remove, param.Key)
		}
	}
	// Both input orderings are already ascending because the sets were
	// normalized before the maps were compared.
	return ChangePlan{
		NodeID:      nodeID,
		Generation:  latest.Index,
		Role:        st.role,
		Add:         add,
		Remove:      remove,
		Fingerprint: fingerprintParams(want),
	}, nil
}

// Apply writes a planned generation and advances the node generation.
func (p *BootParamPolicy) Apply(nodeID string, plan ChangePlan) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ErrUnknownNode
	}
	if plan.NodeID != nodeID {
		return ErrBadPlan
	}

	latest := st.generations[len(st.generations)-1]
	if plan.Generation != latest.Index {
		return ErrStalePlan
	}
	if plan.Fingerprint == "" {
		return ErrBadPlan
	}

	result := mapOf(latest.Params)
	for _, key := range plan.Remove {
		delete(result, key)
	}
	for _, param := range plan.Add {
		result[param.Key] = param.Value
	}
	want, err := p.validateAndNormalize(setFromMap(result))
	if err != nil {
		return err
	}
	if fingerprintParams(want) != plan.Fingerprint {
		return ErrBadPlan
	}

	newGeneration := Generation{
		Index:    latest.Index + 1,
		Params:   cloneParamSet(want),
		Rendered: renderParamSet(want),
		Live:     false,
	}
	nextGenerations := append(cloneGenerations(st.generations), newGeneration)
	nextGenerations = p.trimGenerations(nextGenerations)
	nextCurrent := applyHotParams(st.current, want)

	st.generations = nextGenerations
	st.current = nextCurrent
	st.reportedSinceWrite = false
	p.recordEventLocked(nodeID, st, "apply", fmt.Sprintf("generation=%d fingerprint=%s", newGeneration.Index, plan.Fingerprint))
	return nil
}

// Verify compares the newest generation with the reported running parameters.
func (p *BootParamPolicy) Verify(nodeID string) (Drift, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return Drift{}, ErrUnknownNode
	}
	latest := st.generations[len(st.generations)-1]
	want := cloneParamSet(latest.Params)
	got := cloneParamSet(st.current)

	pending := make([]string, 0)
	if !st.reportedSinceWrite {
		pending = pendingColdParams(want, got)
	}
	ignored := make(map[string]bool, len(pending))
	for _, key := range pending {
		ignored[key] = true
	}

	missing, extra, differs := diffParamSets(want, got, ignored)
	return Drift{
		NodeID:  nodeID,
		Missing: missing,
		Extra:   extra,
		Differs: differs,
		Pending: pending,
	}, nil
}

// Rollback writes a new generation holding the previous retained content.
func (p *BootParamPolicy) Rollback(nodeID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ErrUnknownNode
	}
	if len(st.generations) < 2 {
		return ErrNoGeneration
	}

	latest := st.generations[len(st.generations)-1]
	previous := st.generations[len(st.generations)-2]
	target := cloneParamSet(previous.Params)
	newGeneration := Generation{
		Index:    latest.Index + 1,
		Params:   cloneParamSet(target),
		Rendered: renderParamSet(target),
		Live:     false,
	}
	nextGenerations := append(cloneGenerations(st.generations), newGeneration)
	nextGenerations = p.trimGenerations(nextGenerations)

	st.generations = nextGenerations
	st.current = applyHotParams(st.current, target)
	st.reportedSinceWrite = false
	p.recordEventLocked(nodeID, st, "rollback", fmt.Sprintf("generation=%d from=%d", newGeneration.Index, previous.Index))
	return nil
}

// ReportRunning records the command line a node reports after a restart.
func (p *BootParamPolicy) ReportRunning(nodeID, cmdline string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ErrUnknownNode
	}
	if len(cmdline) > maxReportedCmdline {
		return ErrParamLimit
	}
	reported, err := parseCommandLine(cmdline)
	if err != nil {
		return ErrBadRunningParams
	}

	st.current = reported
	st.reportedSinceWrite = true
	p.markLiveGenerationLocked(st, reported)
	p.recordEventLocked(nodeID, st, "report", fmt.Sprintf("parameters=%d", len(reported.Entries)))
	return nil
}

// Generations returns the retained generations of a node, oldest first.
func (p *BootParamPolicy) Generations(nodeID string) ([]Generation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return nil, ErrUnknownNode
	}
	return cloneGenerations(st.generations), nil
}

// Current returns the parameters the node currently runs with.
func (p *BootParamPolicy) Current(nodeID string) (ParamSet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return ParamSet{}, ErrUnknownNode
	}
	return cloneParamSet(st.current), nil
}

// Render returns the boot entry text of one retained generation.
func (p *BootParamPolicy) Render(nodeID string, index int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return "", ErrUnknownNode
	}
	for _, gen := range st.generations {
		if gen.Index == index {
			return gen.Rendered, nil
		}
	}
	return "", ErrNoGeneration
}

// BatchApply advances several nodes in batches and stops at the first failure.
func (p *BootParamPolicy) BatchApply(nodeIDs []string, plans map[string]ChangePlan) (BatchResult, error) {
	result := BatchResult{}
	if len(nodeIDs) == 0 {
		return result, nil
	}

	ordered := uniqueSorted(nodeIDs)
	batchSize := p.limits.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	for start := 0; start < len(ordered); start += batchSize {
		end := start + batchSize
		if end > len(ordered) {
			end = len(ordered)
		}
		for _, nodeID := range ordered[start:end] {
			plan, ok := plans[nodeID]
			if !ok {
				result.Failed = nodeID
				return result, fmt.Errorf("%w: missing plan for %s", ErrBatchAborted, nodeID)
			}
			if err := p.Apply(nodeID, plan); err != nil {
				result.Failed = nodeID
				return result, fmt.Errorf("%w: %s: %w", ErrBatchAborted, nodeID, err)
			}
			result.Applied = append(result.Applied, nodeID)
		}
	}
	return result, nil
}

// Events returns a copy of the per node change log, oldest first.
func (p *BootParamPolicy) Events(nodeID string) []PolicyEvent {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.nodes[nodeID]
	if !ok {
		return nil
	}
	events := make([]PolicyEvent, len(st.events))
	copy(events, st.events)
	return events
}

func (p *BootParamPolicy) validateAndNormalize(params ParamSet) (ParamSet, error) {
	set := normalizedSet(params)
	if len(set.Entries) > p.limits.MaxParams {
		return ParamSet{}, ErrParamLimit
	}
	for _, param := range set.Entries {
		if len(param.Value) > p.limits.MaxValueLen {
			return ParamSet{}, ErrParamLimit
		}
		if forbiddenParam(param) {
			return ParamSet{}, ErrForbiddenParam
		}
	}
	return set, nil
}

func (p *BootParamPolicy) trimGenerations(gens []Generation) []Generation {
	max := p.limits.MaxGenerations
	if max <= 0 || len(gens) <= max {
		return gens
	}
	out := make([]Generation, max)
	copy(out, gens[len(gens)-max:])
	return out
}

func (p *BootParamPolicy) markLiveGenerationLocked(st *nodeState, reported ParamSet) {
	for i := range st.generations {
		st.generations[i].Live = false
	}

	reportedMap := filteredParamMap(reported)
	for i := len(st.generations) - 1; i >= 0; i-- {
		if mapsEqual(filteredParamMap(st.generations[i].Params), reportedMap) {
			st.generations[i].Live = true
			return
		}
	}
	// A drifted report cannot identify an exact retained entry; the newest
	// written entry is still the one that was attempted at boot.
	if len(st.generations) > 0 {
		st.generations[len(st.generations)-1].Live = true
	}
}

func (p *BootParamPolicy) recordEventLocked(nodeID string, st *nodeState, kind, detail string) {
	p.seq++
	st.events = append(st.events, PolicyEvent{
		Seq:    p.seq,
		NodeID: nodeID,
		Kind:   kind,
		Detail: detail,
	})
}

func validRole(role string) bool {
	switch role {
	case RoleCompute, RoleStorage, RoleControl:
		return true
	default:
		return false
	}
}

func baselineForRole(role string) ParamSet {
	var iommu, numa, panicValue string
	switch role {
	case RoleCompute:
		iommu, numa, panicValue = "strict", "1", "10"
	case RoleStorage:
		iommu, numa, panicValue = "on", "0", "5"
	case RoleControl:
		iommu, numa, panicValue = "pt", "0", "3"
	}
	return ParamSet{Entries: []Param{
		{Key: ParamAudit, Value: "on"},
		{Key: ParamConsoleLevel, Value: "3"},
		{Key: ParamIOMMU, Value: iommu},
		{Key: ParamMitigations, Value: "auto"},
		{Key: ParamNUMABalancing, Value: numa},
		{Key: ParamPanic, Value: panicValue},
		{Key: ParamRescueShell, Value: "0"},
		{Key: ParamTHP, Value: "madvise"},
	}}
}

func forbiddenParam(param Param) bool {
	value := strings.TrimSpace(param.Value)
	lower := strings.ToLower(value)
	switch param.Key {
	case ParamRescueShell:
		switch lower {
		case "1", "on", "true", "yes", "enabled", "enable":
			return true
		}
	case ParamIOMMU, ParamAudit, ParamMitigations:
		switch lower {
		case "off", "0", "false", "no", "disabled", "disable", "none":
			return true
		}
	case ParamModuleBlacklist:
		if value == "" {
			return true
		}
		fields := strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		})
		for _, module := range fields {
			switch strings.ToLower(module) {
			case "nvme", "ahci":
				return true
			}
		}
	case ParamConsoleLevel:
		level, err := strconv.Atoi(value)
		if err != nil || level < 1 || level > 4 {
			return true
		}
	}
	return false
}

func normalizedSet(set ParamSet) ParamSet {
	values := make(map[string]string, len(set.Entries))
	for _, param := range set.Entries {
		values[param.Key] = param.Value
	}
	return setFromMap(values)
}

func setFromMap(values map[string]string) ParamSet {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]Param, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, Param{Key: key, Value: values[key]})
	}
	return ParamSet{Entries: entries}
}

func mapOf(set ParamSet) map[string]string {
	values := make(map[string]string, len(set.Entries))
	for _, param := range set.Entries {
		values[param.Key] = param.Value
	}
	return values
}

func cloneParamSet(set ParamSet) ParamSet {
	entries := make([]Param, len(set.Entries))
	copy(entries, set.Entries)
	return ParamSet{Entries: entries}
}

func cloneGenerations(gens []Generation) []Generation {
	out := make([]Generation, len(gens))
	for i, gen := range gens {
		out[i] = Generation{
			Index:    gen.Index,
			Params:   cloneParamSet(gen.Params),
			Rendered: gen.Rendered,
			Live:     gen.Live,
		}
	}
	return out
}

func applyHotParams(current ParamSet, wanted ParamSet) ParamSet {
	values := mapOf(current)
	wantedValues := mapOf(wanted)
	for key := range hotParams {
		if value, ok := wantedValues[key]; ok {
			values[key] = value
		} else {
			delete(values, key)
		}
	}
	return setFromMap(values)
}

func pendingColdParams(want, got ParamSet) []string {
	wantValues := mapOf(want)
	gotValues := mapOf(got)
	keys := unionKeys(wantValues, gotValues)
	pending := make([]string, 0)
	for _, key := range keys {
		if hotParams[key] || isKernelImageParam(key) {
			continue
		}
		wantValue, wantOK := wantValues[key]
		gotValue, gotOK := gotValues[key]
		if wantOK != gotOK || wantValue != gotValue {
			pending = append(pending, key)
		}
	}
	return pending
}

func diffParamSets(want, got ParamSet, ignored map[string]bool) ([]Param, []Param, []ParamDiff) {
	wantValues := filteredParamMap(want)
	gotValues := filteredParamMap(got)
	keys := unionKeys(wantValues, gotValues)
	missing := make([]Param, 0)
	extra := make([]Param, 0)
	differs := make([]ParamDiff, 0)
	for _, key := range keys {
		if ignored[key] {
			continue
		}
		wantValue, wantOK := wantValues[key]
		gotValue, gotOK := gotValues[key]
		switch {
		case wantOK && !gotOK:
			missing = append(missing, Param{Key: key, Value: wantValue})
		case !wantOK && gotOK:
			extra = append(extra, Param{Key: key, Value: gotValue})
		case wantOK && gotOK && wantValue != gotValue:
			differs = append(differs, ParamDiff{Key: key, Want: wantValue, Got: gotValue})
		}
	}
	return missing, extra, differs
}

func filteredParamMap(set ParamSet) map[string]string {
	values := make(map[string]string, len(set.Entries))
	for _, param := range set.Entries {
		if isKernelImageParam(param.Key) {
			continue
		}
		values[param.Key] = param.Value
	}
	return values
}

func unionKeys(a, b map[string]string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for key := range a {
		seen[key] = true
	}
	for key := range b {
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func isKernelImageParam(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	switch key {
	case "BOOT_IMAGE", "BOOTIMAGE", "INITRD", "INITRD_IMAGE", "KERNEL",
		"KERNEL_IMAGE", "IMAGE", "IMAGE_PATH", "KERNEL_IMAGE_PATH",
		"INITRD_IMAGE_PATH", "VMLINUZ", "VMLINUZ_PATH":
		return true
	}
	return strings.HasPrefix(key, "BOOT_IMAGE_") || strings.HasSuffix(key, "_IMAGE_PATH")
}

func fingerprintParams(set ParamSet) string {
	hash := sha256.New()
	for _, param := range set.Entries {
		fmt.Fprintf(hash, "%d:%s=%d:%s\n", len(param.Key), param.Key, len(param.Value), param.Value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func renderParamSet(set ParamSet) string {
	lines := make([]string, 0, len(set.Entries))
	for _, param := range set.Entries {
		lines = append(lines, param.Key+"="+quoteParamValue(param.Value))
	}
	return strings.Join(lines, "\n")
}

func quoteParamValue(value string) string {
	needsQuote := strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.ContainsRune(value, '"')
	if !needsQuote {
		return value
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func parseCommandLine(cmdline string) (ParamSet, error) {
	words, err := splitCommandLine(cmdline)
	if err != nil {
		return ParamSet{}, err
	}
	values := make(map[string]string, len(words))
	for _, word := range words {
		if word == "" {
			continue
		}
		key, value, hasValue := strings.Cut(word, "=")
		if key == "" {
			return ParamSet{}, ErrBadRunningParams
		}
		if !hasValue {
			value = ""
		}
		values[key] = value
	}
	return setFromMap(values), nil
}

func splitCommandLine(cmdline string) ([]string, error) {
	words := make([]string, 0)
	var word strings.Builder
	started := false
	inSingle := false
	inDouble := false
	escaped := false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}

	for _, r := range cmdline {
		if escaped {
			word.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			started = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			started = true
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			started = true
			continue
		}
		if unicode.IsSpace(r) && !inSingle && !inDouble {
			flush()
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped {
		word.WriteRune('\\')
	}
	if inSingle || inDouble {
		return nil, ErrBadRunningParams
	}
	flush()
	return words, nil
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
