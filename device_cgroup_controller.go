// Package task compiles sandbox device access rules into a program, attaches
// it to one container and answers whether a device open is allowed, keeping
// the last good program alive across failed updates.
package task

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Device classes used by the kernel: all, block, char.
const (
	DeviceAll   = "a"
	DeviceBlock = "b"
	DeviceChar  = "c"
)

// Instruction operations.
const (
	OpAllow = "allow"
	OpDeny  = "deny"
)

// WildcardMajor and WildcardMinor mark a rule that matches any number.
const Wildcard = -1

// Reason codes returned by Explain.
const (
	ReasonDefaultDeny   = "default_deny"
	ReasonExactAllow    = "exact_allow"
	ReasonWildcardAllow = "wildcard_allow"
	ReasonExactDeny     = "exact_deny"
	ReasonWildcardDeny  = "wildcard_deny"
	ReasonNoContainer   = "no_container"
)

const (
	snapshotFormat = "gsb-device-cgroup-controller/v1"
	snapshotSchema = 1
)

// Errors returned by the controller.
var (
	ErrBadPolicy      = errors.New("device policy is not acceptable")
	ErrBadDeviceType  = errors.New("unknown device class")
	ErrRuleLimit      = errors.New("rule count over the container limit")
	ErrContainerLimit = errors.New("container count over the node limit")
	ErrNoContainer    = errors.New("container is not attached")
	ErrBadProgram     = errors.New("program was not produced by Compile")
	ErrStaleRevision  = errors.New("policy revision is not newer than the active one")
	ErrCacheLimit     = errors.New("compile cache is full")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Device is one open attempt against a device node.
type Device struct {
	Type      string `json:"type"`
	Major     int    `json:"major"`
	Minor     int    `json:"minor"`
	Path      string `json:"path"`
	HostInode uint64 `json:"host_inode"`
}

// Rule is one allow or deny entry of a policy. A Major or Minor of Wildcard
// matches any number, and a Type of DeviceAll matches every device class.
// Priority breaks ties between rules that match the same device.
type Rule struct {
	Type     string `json:"type"`
	Major    int    `json:"major"`
	Minor    int    `json:"minor"`
	Allow    bool   `json:"allow"`
	Priority int    `json:"priority"`
}

// DevicePolicy is one immutable policy revision.
type DevicePolicy struct {
	Revision int    `json:"revision"`
	Rules    []Rule `json:"rules"`
}

// Instruction is one compiled comparison.
type Instruction struct {
	Op    string `json:"op"`
	Type  string `json:"type"`
	Major int    `json:"major"`
	Minor int    `json:"minor"`
}

// Program is the compiled artefact attached to one container.
type Program struct {
	ID           string        `json:"id"`
	Revision     int           `json:"revision"`
	Rules        int           `json:"rules"`
	Digest       string        `json:"digest"`
	Instructions []Instruction `json:"instructions"`

	// These fields authenticate a value created inside this package and
	// preserve priorities, which are not part of the public instruction ABI.
	attested      bool
	compiledRules []Rule
}

// Version is the effective policy revision of a container.
type Version int

// Verdict is the outcome of one Explain call.
type Verdict struct {
	Allowed    bool    `json:"allowed"`
	Program    string  `json:"program"`
	Rule       string  `json:"rule"`
	Generation Version `json:"generation"`
}

// Reason explains a verdict in human readable terms.
type Reason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Limits bounds the node wide tables.
type Limits struct {
	MaxRulesPerContainer int
	MaxContainers        int
	MaxCacheEntries      int
}

// DeviceCgroupController owns the compiled programs, the container table and
// the compile cache.
type DeviceCgroupController struct {
	limits Limits
	mu     sync.RWMutex

	containers map[string]*containerState
	cache      map[string]*list.Element
	lru        *list.List
}

type containerState struct {
	program Program
	rules   []Rule
}

type cacheEntry struct {
	digest  string
	program Program
}

type snapshotDTO struct {
	Format     string              `json:"format"`
	Schema     int                 `json:"schema"`
	Containers []containerSnapshot `json:"containers"`
	Cache      []programSnapshot   `json:"cache"`
}

type containerSnapshot struct {
	ID      string  `json:"id"`
	Program Program `json:"program"`
	Rules   []Rule  `json:"rules"`
}

type programSnapshot struct {
	Program Program `json:"program"`
	Rules   []Rule  `json:"rules"`
}

// NewDeviceCgroupController builds a controller with the given caps.
func NewDeviceCgroupController(limits Limits) *DeviceCgroupController {
	return &DeviceCgroupController{
		limits:     limits,
		containers: make(map[string]*containerState),
		cache:      make(map[string]*list.Element),
		lru:        list.New(),
	}
}

// Compile turns one policy revision into a program. The digest must not depend
// on the order the caller wrote the rules in.
func (c *DeviceCgroupController) Compile(policy DevicePolicy) (Program, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	program, err := buildProgram(policy, c.limits.MaxRulesPerContainer)
	if err != nil {
		return Program{}, err
	}
	c.putCacheLocked(program)
	return cloneProgram(program), nil
}

// Attach makes a program the active generation of one container.
func (c *DeviceCgroupController) Attach(containerID string, prog Program) error {
	if !prog.attested || len(prog.compiledRules) == 0 {
		return ErrBadProgram
	}

	expected, err := buildProgram(DevicePolicy{
		Revision: prog.Revision,
		Rules:    prog.compiledRules,
	}, c.limits.MaxRulesPerContainer)
	if err != nil {
		return err
	}
	if !samePublicProgram(prog, expected) {
		return ErrBadProgram
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.containers[containerID]; !exists &&
		len(c.containers) >= c.limits.MaxContainers {
		return fmt.Errorf("%w: %d/%d", ErrContainerLimit,
			len(c.containers), c.limits.MaxContainers)
	}

	installed := cloneProgram(expected)
	c.containers[containerID] = &containerState{
		program: installed,
		rules:   cloneRules(expected.compiledRules),
	}
	return nil
}

// Update compiles a newer policy and swaps it in whole; a failure keeps the
// previous generation active.
func (c *DeviceCgroupController) Update(containerID string, policy DevicePolicy) (Version, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state, ok := c.containers[containerID]
	if !ok {
		return 0, ErrNoContainer
	}

	currentRevision := state.program.Revision
	if policy.Revision <= currentRevision {
		return 0, fmt.Errorf("%w: policy=%d active=%d",
			ErrStaleRevision, policy.Revision, currentRevision)
	}

	program, err := buildProgram(policy, c.limits.MaxRulesPerContainer)
	if err != nil {
		return 0, err
	}

	// The write lock has been held throughout, so the existence and revision
	// check above remain valid for this whole-generation swap.
	c.putCacheLocked(program)
	c.containers[containerID] = &containerState{
		program: cloneProgram(program),
		rules:   cloneRules(program.compiledRules),
	}
	return Version(program.Revision), nil
}

// Explain evaluates one device open against the active generation.
func (c *DeviceCgroupController) Explain(containerID string, dev Device) (Verdict, Reason) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	state, ok := c.containers[containerID]
	if !ok {
		return Verdict{Allowed: false}, Reason{
			Code:   ReasonNoContainer,
			Detail: fmt.Sprintf("container %q is not attached", containerID),
		}
	}

	return evaluate(state.program, state.rules, dev)
}

// Program reads back the active program of one container.
func (c *DeviceCgroupController) Program(containerID string) (Program, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	state, ok := c.containers[containerID]
	if !ok {
		return Program{}, false
	}
	return cloneProgram(state.program), true
}

// CurrentVersion reports the effective policy revision of one container.
func (c *DeviceCgroupController) CurrentVersion(containerID string) Version {
	c.mu.RLock()
	defer c.mu.RUnlock()

	state, ok := c.containers[containerID]
	if !ok {
		return 0
	}
	return Version(state.program.Revision)
}

// Detach drops one container and its program.
func (c *DeviceCgroupController) Detach(containerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.containers[containerID]; !ok {
		return ErrNoContainer
	}
	delete(c.containers, containerID)
	return nil
}

// Containers lists the attached containers in ascending id order.
func (c *DeviceCgroupController) Containers() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]string, 0, len(c.containers))
	for id := range c.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// CompileCacheSize reports how many compiled policies are cached.
func (c *DeviceCgroupController) CompileCacheSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// Export serialises compiled programs, the container table and the cache.
func (c *DeviceCgroupController) Export() ([]byte, error) {
	c.mu.RLock()
	ids := make([]string, 0, len(c.containers))
	for id := range c.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	snapshot := snapshotDTO{
		Format:     snapshotFormat,
		Schema:     snapshotSchema,
		Containers: make([]containerSnapshot, 0, len(ids)),
		Cache:      make([]programSnapshot, 0, c.lru.Len()),
	}

	for _, id := range ids {
		state := c.containers[id]
		snapshot.Containers = append(snapshot.Containers, containerSnapshot{
			ID:      id,
			Program: cloneProgram(state.program),
			Rules:   cloneRules(state.rules),
		})
	}

	// Front is most recently used. Export in MRU-to-LRU order so Import can
	// append directly into the same list orientation.
	for element := c.lru.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		snapshot.Cache = append(snapshot.Cache, programSnapshot{
			Program: cloneProgram(entry.program),
			Rules:   cloneRules(entry.program.compiledRules),
		})
	}
	c.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Import replaces the whole state with an exported payload, all or nothing.
func (c *DeviceCgroupController) Import(data []byte) error {
	var snapshot snapshotDTO
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if snapshot.Format != snapshotFormat || snapshot.Schema != snapshotSchema {
		return fmt.Errorf("%w: unsupported snapshot format or schema", ErrBadSnapshot)
	}

	nextContainers := make(map[string]*containerState, len(snapshot.Containers))
	for _, item := range snapshot.Containers {
		if item.ID == "" {
			return fmt.Errorf("%w: container id is empty", ErrBadSnapshot)
		}
		if _, exists := nextContainers[item.ID]; exists {
			return fmt.Errorf("%w: duplicate container %q", ErrBadSnapshot, item.ID)
		}
		program, err := restoreSnapshotProgram(item.Program, item.Rules,
			c.limits.MaxRulesPerContainer)
		if err != nil {
			return err
		}
		nextContainers[item.ID] = &containerState{
			program: program,
			rules:   cloneRules(program.compiledRules),
		}
	}

	if c.limits.MaxContainers >= 0 && len(nextContainers) > c.limits.MaxContainers {
		return fmt.Errorf("%w: %d/%d", ErrContainerLimit,
			len(nextContainers), c.limits.MaxContainers)
	}

	restoredCache := make([]Program, 0, len(snapshot.Cache))
	for _, item := range snapshot.Cache {
		program, err := restoreSnapshotProgram(item.Program, item.Rules,
			c.limits.MaxRulesPerContainer)
		if err != nil {
			return err
		}
		restoredCache = append(restoredCache, program)
	}

	nextCache := make(map[string]*list.Element, len(restoredCache))
	nextLRU := list.New()
	for _, program := range restoredCache {
		if _, exists := nextCache[program.Digest]; exists {
			continue
		}
		if c.limits.MaxCacheEntries > 0 && nextLRU.Len() >= c.limits.MaxCacheEntries {
			// Snapshot order is MRU first, so remaining entries are older.
			continue
		}
		element := nextLRU.PushBack(&cacheEntry{
			digest:  program.Digest,
			program: cloneProgram(program),
		})
		nextCache[program.Digest] = element
	}

	c.mu.Lock()
	c.containers = nextContainers
	c.cache = nextCache
	c.lru = nextLRU
	c.mu.Unlock()
	return nil
}

func buildProgram(policy DevicePolicy, maxRules int) (Program, error) {
	if policy.Revision < 1 || len(policy.Rules) == 0 {
		return Program{}, fmt.Errorf("%w: revision=%d rules=%d",
			ErrBadPolicy, policy.Revision, len(policy.Rules))
	}
	if len(policy.Rules) > maxRules {
		return Program{}, fmt.Errorf("%w: rules=%d max=%d",
			ErrRuleLimit, len(policy.Rules), maxRules)
	}

	for _, rule := range policy.Rules {
		if rule.Type != DeviceAll && rule.Type != DeviceBlock && rule.Type != DeviceChar {
			return Program{}, fmt.Errorf("%w: %q", ErrBadDeviceType, rule.Type)
		}
	}

	rules := canonicalRules(policy.Rules)
	instructions := make([]Instruction, 0, len(rules))
	for _, rule := range rules {
		if !rule.Allow {
			instructions = append(instructions, instructionForRule(rule))
		}
	}
	for _, rule := range rules {
		if rule.Allow {
			instructions = append(instructions, instructionForRule(rule))
		}
	}

	digest := digestPolicy(policy.Revision, rules)
	return Program{
		ID:            "prog-" + digest,
		Revision:      policy.Revision,
		Rules:         len(rules),
		Digest:        digest,
		Instructions:  instructions,
		attested:      true,
		compiledRules: rules,
	}, nil
}

func canonicalRules(input []Rule) []Rule {
	rules := cloneRules(input)
	sort.Slice(rules, func(i, j int) bool {
		return ruleLess(rules[i], rules[j])
	})
	return rules
}

func ruleLess(a, b Rule) bool {
	if rankA, rankB := typeRank(a.Type), typeRank(b.Type); rankA != rankB {
		return rankA < rankB
	}
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	if a.Allow != b.Allow {
		return !a.Allow // deny entries have the stable leading position.
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return false
}

func typeRank(deviceType string) int {
	switch deviceType {
	case DeviceAll:
		return 0
	case DeviceBlock:
		return 1
	case DeviceChar:
		return 2
	default:
		return 3
	}
}

func digestPolicy(revision int, rules []Rule) string {
	hash := sha256.New()
	writeLine := func(format string, args ...any) {
		fmt.Fprintf(hash, format+"\n", args...)
	}

	writeLine("device-cgroup-program/v1")
	writeLine("revision=%d", revision)
	writeLine("rules=%d", len(rules))
	for _, rule := range rules {
		writeLine("rule type=%s major=%d minor=%d allow=%t priority=%d",
			rule.Type, rule.Major, rule.Minor, rule.Allow, rule.Priority)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func instructionForRule(rule Rule) Instruction {
	op := OpAllow
	if !rule.Allow {
		op = OpDeny
	}
	return Instruction{
		Op:    op,
		Type:  rule.Type,
		Major: rule.Major,
		Minor: rule.Minor,
	}
}

func samePublicProgram(got, want Program) bool {
	return got.ID == want.ID &&
		got.Revision == want.Revision &&
		got.Rules == want.Rules &&
		got.Digest == want.Digest &&
		reflect.DeepEqual(got.Instructions, want.Instructions)
}

func restoreSnapshotProgram(raw Program, rules []Rule, maxRules int) (Program, error) {
	expected, err := buildProgram(DevicePolicy{
		Revision: raw.Revision,
		Rules:    rules,
	}, maxRules)
	if err != nil {
		return Program{}, fmt.Errorf("%w: %w", ErrBadSnapshot, err)
	}
	if !samePublicProgram(raw, expected) {
		return Program{}, fmt.Errorf("%w: program %q does not match its rules and digest",
			ErrBadSnapshot, raw.ID)
	}
	return expected, nil
}

func (c *DeviceCgroupController) putCacheLocked(program Program) {
	if c.limits.MaxCacheEntries <= 0 {
		return
	}

	key := program.Digest
	if element, ok := c.cache[key]; ok {
		c.lru.MoveToFront(element)
		element.Value.(*cacheEntry).program = cloneProgram(program)
		return
	}

	element := c.lru.PushFront(&cacheEntry{
		digest:  key,
		program: cloneProgram(program),
	})
	c.cache[key] = element

	for c.lru.Len() > c.limits.MaxCacheEntries {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		entry := c.lru.Remove(oldest).(*cacheEntry)
		delete(c.cache, entry.digest)
	}
}

func evaluate(program Program, rules []Rule, dev Device) (Verdict, Reason) {
	var bestAllow *Rule
	var bestDeny *Rule

	for i := range rules {
		rule := &rules[i]
		if !ruleMatches(*rule, dev) {
			continue
		}
		if rule.Allow {
			if bestAllow == nil || preferRule(*rule, *bestAllow) {
				bestAllow = rule
			}
		} else {
			if bestDeny == nil || preferRule(*rule, *bestDeny) {
				bestDeny = rule
			}
		}
	}

	verdict := Verdict{
		Program:    program.ID,
		Generation: Version(program.Revision),
	}

	// An explicit deny generation always wins over an allow generation. Priority
	// and specificity choose the strongest rule within each effect.
	selected := bestDeny
	if selected == nil {
		selected = bestAllow
	}
	if selected == nil {
		verdict.Allowed = false
		return verdict, Reason{
			Code: ReasonDefaultDeny,
			Detail: fmt.Sprintf(
				"no rule matched class=%s major=%d minor=%d host_inode=%d",
				dev.Type, dev.Major, dev.Minor, dev.HostInode,
			),
		}
	}

	verdict.Allowed = selected.Allow
	verdict.Rule = ruleLabel(*selected)
	code := reasonCode(*selected)
	return verdict, Reason{
		Code: code,
		Detail: fmt.Sprintf(
			"selected=%s class=%s major=%d minor=%d host_inode=%d generation=%d",
			verdict.Rule, dev.Type, dev.Major, dev.Minor, dev.HostInode, program.Revision,
		),
	}
}

func ruleMatches(rule Rule, dev Device) bool {
	// The device class is part of identity. Reusing major/minor numbers after
	// hotplug therefore cannot match a rule for a different class.
	if rule.Type != DeviceAll && rule.Type != dev.Type {
		return false
	}
	return numberMatches(rule.Major, dev.Major) &&
		numberMatches(rule.Minor, dev.Minor)
}

func numberMatches(expected, actual int) bool {
	return isWildcardNumber(expected) || expected == actual
}

func isWildcardNumber(value int) bool {
	return value == Wildcard || value < 0
}

func preferRule(candidate, current Rule) bool {
	if candidate.Priority != current.Priority {
		return candidate.Priority > current.Priority
	}

	candidateSpecificity := ruleSpecificity(candidate)
	currentSpecificity := ruleSpecificity(current)
	if candidateSpecificity != currentSpecificity {
		return candidateSpecificity > currentSpecificity
	}

	return ruleLess(candidate, current)
}

func ruleSpecificity(rule Rule) int {
	specificity := 0
	if rule.Type != DeviceAll {
		specificity++
	}
	if !isWildcardNumber(rule.Major) {
		specificity++
	}
	if !isWildcardNumber(rule.Minor) {
		specificity++
	}
	return specificity
}

func reasonCode(rule Rule) string {
	exact := rule.Type != DeviceAll &&
		!isWildcardNumber(rule.Major) &&
		!isWildcardNumber(rule.Minor)

	if rule.Allow {
		if exact {
			return ReasonExactAllow
		}
		return ReasonWildcardAllow
	}
	if exact {
		return ReasonExactDeny
	}
	return ReasonWildcardDeny
}

func ruleLabel(rule Rule) string {
	op := OpAllow
	if !rule.Allow {
		op = OpDeny
	}
	return fmt.Sprintf("%s:%s:%s:%s:p%d",
		op, rule.Type, numberLabel(rule.Major), numberLabel(rule.Minor), rule.Priority)
}

func numberLabel(value int) string {
	if isWildcardNumber(value) {
		return "*"
	}
	return fmt.Sprintf("%d", value)
}

func cloneProgram(program Program) Program {
	clone := program
	if program.Instructions != nil {
		clone.Instructions = append([]Instruction(nil), program.Instructions...)
	}
	if program.compiledRules != nil {
		clone.compiledRules = append([]Rule(nil), program.compiledRules...)
	}
	return clone
}

func cloneRules(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	return append([]Rule(nil), rules...)
}
