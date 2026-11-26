// Package task guards the public GraphQL endpoint: it analyses documents into
// immutable cost plans and meters tenant budgets over a sliding window.
package task

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reasons reported with an admission decision.
const (
	ReasonAllowed               = "allowed"
	ReasonCostLimit             = "cost_limit"
	ReasonBudgetExhausted       = "budget_exhausted"
	ReasonIntrospectionDisabled = "introspection_disabled"
)

// Errors reported by the guard.
var (
	ErrBadQuery          = errors.New("graphql document cannot be parsed")
	ErrDepthLimit        = errors.New("document depth limit exceeded")
	ErrFieldLimit        = errors.New("document field count limit exceeded")
	ErrFragmentCycle     = errors.New("fragment spread cycle detected")
	ErrUnknownFragment   = errors.New("fragment spread refers to an unknown fragment")
	ErrBadTenant         = errors.New("tenant id is missing or unknown")
	ErrBadCost           = errors.New("cost must be a positive number")
	ErrBudgetExceeded    = errors.New("tenant window budget is exhausted")
	ErrRefundExceeds     = errors.New("refund is larger than the charged amount")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
	ErrUnsupportedDoc    = errors.New("document uses syntax that is not supported")
	ErrIntrospectionCost = errors.New("introspection cost cannot be computed")
)

// Limits bounds one document and one tenant window.
type Limits struct {
	MaxDepth          int
	MaxFields         int
	MaxCost           int
	TenantBudget      int
	WindowLength      time.Duration
	FieldCost         int
	MaxListMultiplier int
	IntrospectionCost int
	CacheSize         int
	ParseTimeout      time.Duration
}

// Plan is the immutable result of analysing one document.
type Plan struct {
	QueryHash     string
	Depth         int
	Fields        int
	Cost          int
	Introspection bool
	Cached        bool
}

// Decision is the admission verdict for one plan.
type Decision struct {
	Allowed   bool
	Cost      int
	Remaining int
	Reason    string
}

// Window is the sliding budget window of one tenant.
type Window struct {
	Used      int
	Limit     int
	Remaining int
}

// CacheStats reports the analysis cache counters.
type CacheStats struct {
	Hits   int
	Misses int
}

// GraphqlCostGuard owns the analysis cache and the tenant windows.
type GraphqlCostGuard struct {
	limits   Limits
	mu       sync.Mutex
	tenants  map[string]*tenantState
	lastTick time.Time

	cache  *list.List
	items  map[string]*list.Element
	hits   int
	misses int
}

type tenantState struct {
	charges       []charge
	used          int
	introDisabled bool
}

type charge struct {
	At   time.Time `json:"at"`
	Cost int       `json:"cost"`
}

type cacheEntry struct {
	key  string
	plan Plan
}

// NewGraphqlCostGuard builds a guard with the given limits.
func NewGraphqlCostGuard(limits Limits) *GraphqlCostGuard {
	return &GraphqlCostGuard{
		limits:   limits,
		tenants:  make(map[string]*tenantState),
		lastTick: time.Now(),
		cache:    list.New(),
		items:    make(map[string]*list.Element),
	}
}

// Analyze parses a document and returns its cost plan.
func (g *GraphqlCostGuard) Analyze(query string, vars map[string]any) (Plan, error) {
	hash := fingerprintQuery(query, vars)

	g.mu.Lock()
	if element, ok := g.items[hash]; ok {
		g.cache.MoveToFront(element)
		g.hits++
		plan := element.Value.(*cacheEntry).plan
		g.mu.Unlock()

		plan.QueryHash = hash
		plan.Cached = true
		return plan, nil
	}
	g.misses++
	g.mu.Unlock()

	document, err := parseGraphQLDocument(query)
	if err != nil {
		return Plan{}, err
	}
	if len(document.operations) == 0 {
		return Plan{}, fmt.Errorf("%w: missing query operation", ErrUnsupportedDoc)
	}
	if len(document.operations) > 1 {
		return Plan{}, fmt.Errorf("%w: multiple operations require an operation name", ErrUnsupportedDoc)
	}
	operation := document.operations[0]
	if err := validateOperationVariables(operation, document); err != nil {
		return Plan{}, err
	}

	builder := &costBuilder{
		document:      document,
		operation:     operation,
		vars:          vars,
		fieldCost:     maxInt(0, g.limits.FieldCost),
		maxDepth:      g.limits.MaxDepth,
		maxFields:     g.limits.MaxFields,
		maxMultiplier: positiveOrOne(g.limits.MaxListMultiplier),
	}
	cost, err := builder.expandSelectionSet(operation.selections, 1)
	if err != nil {
		return Plan{}, err
	}

	if builder.introspection {
		if g.limits.IntrospectionCost <= 0 {
			return Plan{}, ErrIntrospectionCost
		}
		cost = g.limits.IntrospectionCost
	}

	plan := Plan{
		QueryHash:     hash,
		Depth:         builder.depth,
		Fields:        builder.fields,
		Cost:          cost,
		Introspection: builder.introspection,
		Cached:        false,
	}
	g.storePlan(hash, plan)
	return plan, nil
}

// Admit decides whether a plan fits the single query and tenant budgets.
func (g *GraphqlCostGuard) Admit(tenantID string, plan Plan) (Decision, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Decision{}, ErrBadTenant
	}
	if plan.Cost <= 0 {
		return Decision{}, ErrBadCost
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	tenant := g.tenants[tenantID]
	used := 0
	introDisabled := false
	if tenant != nil {
		used = tenant.used
		introDisabled = tenant.introDisabled
	}
	remaining := g.limits.TenantBudget - used
	decision := Decision{Cost: plan.Cost, Remaining: maxInt(0, remaining)}

	if plan.Introspection && introDisabled {
		decision.Allowed = false
		decision.Reason = ReasonIntrospectionDisabled
		return decision, nil
	}
	if plan.Cost > g.limits.MaxCost {
		decision.Allowed = false
		decision.Reason = ReasonCostLimit
		return decision, nil
	}
	if plan.Cost > g.limits.TenantBudget-used {
		decision.Allowed = false
		decision.Reason = ReasonBudgetExhausted
		return decision, nil
	}

	tenant = g.tenantLocked(tenantID)
	tenant.charges = append(tenant.charges, charge{At: g.chargeTimeLocked(), Cost: plan.Cost})
	tenant.used += plan.Cost
	decision.Allowed = true
	decision.Reason = ReasonAllowed
	decision.Remaining = g.limits.TenantBudget - tenant.used
	return decision, nil
}

// Charge books an explicit cost against one tenant window.
func (g *GraphqlCostGuard) Charge(tenantID string, cost int) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return ErrBadTenant
	}
	if cost <= 0 {
		return ErrBadCost
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	used := 0
	if tenant := g.tenants[tenantID]; tenant != nil {
		used = tenant.used
	}
	if cost > g.limits.TenantBudget-used {
		return ErrBudgetExceeded
	}
	tenant := g.tenantLocked(tenantID)
	tenant.charges = append(tenant.charges, charge{At: g.chargeTimeLocked(), Cost: cost})
	tenant.used += cost
	return nil
}

// Refund rolls a charge back when a request failed downstream.
func (g *GraphqlCostGuard) Refund(tenantID string, cost int) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return ErrBadTenant
	}
	if cost <= 0 {
		return ErrBadCost
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	tenant, ok := g.tenants[tenantID]
	if !ok || tenant.used < cost {
		return ErrRefundExceeds
	}

	remaining := cost
	for i := len(tenant.charges) - 1; i >= 0 && remaining > 0; i-- {
		taken := minInt(tenant.charges[i].Cost, remaining)
		tenant.charges[i].Cost -= taken
		tenant.used -= taken
		remaining -= taken
	}
	charges := make([]charge, 0, len(tenant.charges))
	for _, item := range tenant.charges {
		if item.Cost > 0 {
			charges = append(charges, item)
		}
	}
	tenant.charges = charges
	return nil
}

// Usage reports the live window of one tenant.
func (g *GraphqlCostGuard) Usage(tenantID string) Window {
	tenantID = strings.TrimSpace(tenantID)
	g.mu.Lock()
	defer g.mu.Unlock()

	used := 0
	if tenant, ok := g.tenants[tenantID]; ok {
		used = tenant.used
	}
	limit := g.limits.TenantBudget
	return Window{Used: used, Limit: limit, Remaining: maxInt(0, limit-used)}
}

// Tick advances the window clock, reclaiming expired charges.
func (g *GraphqlCostGuard) Tick(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.lastTick = now
	cutoff := now.Add(-g.limits.WindowLength)
	expired := 0
	for _, tenant := range g.tenants {
		kept := tenant.charges[:0]
		used := 0
		for _, item := range tenant.charges {
			if item.At.Before(cutoff) {
				expired++
				continue
			}
			kept = append(kept, item)
			used += item.Cost
		}
		tenant.charges = kept
		tenant.used = used
	}
	return expired
}

// CacheStats reports analysis cache hits and misses.
func (g *GraphqlCostGuard) CacheStats() CacheStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return CacheStats{Hits: g.hits, Misses: g.misses}
}

// SetIntrospection turns introspection queries on or off for one tenant.
func (g *GraphqlCostGuard) SetIntrospection(tenantID string, enabled bool) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return ErrBadTenant
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tenantLocked(tenantID).introDisabled = !enabled
	return nil
}

type snapshotFile struct {
	Version  int                       `json:"version"`
	LastTick time.Time                 `json:"last_tick"`
	Tenants  map[string]tenantSnapshot `json:"tenants"`
	Hits     int                       `json:"hits"`
	Misses   int                       `json:"misses"`
	Cache    []Plan                    `json:"cache"`
}

type tenantSnapshot struct {
	Used             int      `json:"used"`
	Charges          []charge `json:"charges"`
	IntrospectionOff bool     `json:"introspection_disabled"`
}

// Snapshot serialises tenant windows, the introspection switches and counters.
func (g *GraphqlCostGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	data := snapshotFile{
		Version:  1,
		LastTick: g.lastTick,
		Tenants:  make(map[string]tenantSnapshot, len(g.tenants)),
		Hits:     g.hits,
		Misses:   g.misses,
		Cache:    nil,
	}
	for id, tenant := range g.tenants {
		charges := append([]charge(nil), tenant.charges...)
		data.Tenants[id] = tenantSnapshot{
			Used:             tenant.used,
			Charges:          charges,
			IntrospectionOff: tenant.introDisabled,
		}
	}
	for element := g.cache.Front(); element != nil; element = element.Next() {
		data.Cache = append(data.Cache, element.Value.(*cacheEntry).plan)
	}
	g.mu.Unlock()

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return payload, nil
}

// Restore replaces the guard state, all or nothing.
func (g *GraphqlCostGuard) Restore(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: empty snapshot", ErrBadSnapshot)
	}
	var snapshot snapshotFile
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if snapshot.Version != 1 {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, snapshot.Version)
	}
	if snapshot.Hits < 0 || snapshot.Misses < 0 {
		return fmt.Errorf("%w: negative cache counters", ErrBadSnapshot)
	}

	tenants := make(map[string]*tenantState, len(snapshot.Tenants))
	for id, item := range snapshot.Tenants {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: empty tenant", ErrBadSnapshot)
		}
		used := 0
		charges := make([]charge, 0, len(item.Charges))
		for _, itemCharge := range item.Charges {
			if itemCharge.Cost <= 0 {
				return fmt.Errorf("%w: non-positive charge", ErrBadSnapshot)
			}
			charges = append(charges, itemCharge)
			used += itemCharge.Cost
		}
		if item.Used != 0 && item.Used != used {
			return fmt.Errorf("%w: used does not match charges", ErrBadSnapshot)
		}
		tenants[id] = &tenantState{charges: charges, used: used, introDisabled: item.IntrospectionOff}
	}

	cachePlans := make([]Plan, 0, len(snapshot.Cache))
	seen := make(map[string]bool, len(snapshot.Cache))
	for _, plan := range snapshot.Cache {
		if len(plan.QueryHash) != sha256.Size*2 || seen[plan.QueryHash] {
			return fmt.Errorf("%w: invalid cache plan", ErrBadSnapshot)
		}
		if _, err := hex.DecodeString(plan.QueryHash); err != nil {
			return fmt.Errorf("%w: invalid cache hash", ErrBadSnapshot)
		}
		if plan.Cost < 0 || plan.Fields < 0 || plan.Depth < 0 {
			return fmt.Errorf("%w: negative plan", ErrBadSnapshot)
		}
		plan.Cached = false
		cachePlans = append(cachePlans, plan)
		seen[plan.QueryHash] = true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.tenants = tenants
	g.lastTick = snapshot.LastTick
	if g.lastTick.IsZero() {
		g.lastTick = time.Now()
	}
	g.hits = snapshot.Hits
	g.misses = snapshot.Misses
	g.items = make(map[string]*list.Element, len(cachePlans))
	g.cache = list.New()
	cacheLimit := positiveOrZero(g.limits.CacheSize)
	for _, plan := range cachePlans {
		planCopy := plan
		entry := &cacheEntry{key: plan.QueryHash, plan: planCopy}
		g.items[plan.QueryHash] = g.cache.PushBack(entry)
	}
	if cacheLimit > 0 {
		for g.cache.Len() > cacheLimit {
			oldest := g.cache.Back()
			if oldest == nil {
				break
			}
			entry := g.cache.Remove(oldest).(*cacheEntry)
			delete(g.items, entry.key)
		}
	}
	return nil
}

func (g *GraphqlCostGuard) tenantLocked(id string) *tenantState {
	tenant, ok := g.tenants[id]
	if !ok {
		tenant = &tenantState{}
		g.tenants[id] = tenant
	}
	return tenant
}

func (g *GraphqlCostGuard) chargeTimeLocked() time.Time {
	if g.lastTick.IsZero() {
		g.lastTick = time.Now()
	}
	return g.lastTick
}

func (g *GraphqlCostGuard) storePlan(hash string, plan Plan) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.limits.CacheSize <= 0 {
		return
	}
	if element, ok := g.items[hash]; ok {
		g.cache.MoveToFront(element)
		element.Value.(*cacheEntry).plan = plan
		return
	}

	entry := &cacheEntry{key: hash, plan: plan}
	g.items[hash] = g.cache.PushFront(entry)
	for g.cache.Len() > g.limits.CacheSize {
		oldest := g.cache.Back()
		if oldest == nil {
			break
		}
		removed := g.cache.Remove(oldest).(*cacheEntry)
		delete(g.items, removed.key)
	}
}

func fingerprintQuery(query string, vars map[string]any) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("graphql-cost-guard/v1\n"))
	_, _ = hasher.Write([]byte(query))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(canonicalVariables(vars))
	return hex.EncodeToString(hasher.Sum(nil))
}

func canonicalVariables(vars map[string]any) []byte {
	if len(vars) == 0 {
		return []byte("{}")
	}
	normalized := normalizeCanonicalValue(vars)
	payload, err := json.Marshal(normalized)
	if err != nil {
		payload = []byte(fmt.Sprintf("%#v", normalized))
	}
	return payload
}

func normalizeCanonicalValue(value any) any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Map:
		result := make(map[string]any, rv.Len())
		for _, key := range rv.MapKeys() {
			result[fmt.Sprint(key.Interface())] = normalizeCanonicalValue(rv.MapIndex(key).Interface())
		}
		return result
	case reflect.Slice, reflect.Array:
		result := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			result[i] = normalizeCanonicalValue(rv.Index(i).Interface())
		}
		return result
	default:
		return value
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func positiveOrOne(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}

func positiveOrZero(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func saturatedAdd(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	if b < 0 && a < math.MinInt-b {
		return 0
	}
	return a + b
}

func saturatedMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	if a > math.MaxInt/b {
		return math.MaxInt
	}
	return a * b
}

// GraphQL AST and parser.

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlOperation struct {
	kind                string
	name                string
	variableDefinitions map[string]*gqlVariableDefinition
	selections          []gqlSelection
}

type gqlVariableDefinition struct {
	name         string
	defaultValue *gqlValue
}

type gqlFragment struct {
	name          string
	typeCondition string
	selections    []gqlSelection
}

type gqlSelection interface {
	isSelection()
}

type gqlField struct {
	alias      string
	name       string
	arguments  []gqlArgument
	selections []gqlSelection
}

type gqlFragmentSpread struct {
	name string
}

type gqlInlineFragment struct {
	typeCondition string
	selections    []gqlSelection
}

func (*gqlField) isSelection()          {}
func (*gqlFragmentSpread) isSelection() {}
func (*gqlInlineFragment) isSelection() {}

type gqlArgument struct {
	name  string
	value gqlValue
}

type gqlValueKind int

const (
	gqlNull gqlValueKind = iota
	gqlVariable
	gqlInt
	gqlFloat
	gqlString
	gqlBoolean
	gqlEnum
	gqlList
	gqlObject
)

type gqlValue struct {
	kind     gqlValueKind
	intValue int64
	floatVal float64
	str      string
	boolean  bool
	list     []gqlValue
	object   []gqlObjectField
}

type gqlObjectField struct {
	name  string
	value gqlValue
}

type tokenKind int

const (
	tkEOF tokenKind = iota
	tkName
	tkInt
	tkFloat
	tkString
	tkPunct
)

type gqlToken struct {
	kind  tokenKind
	value string
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func parseGraphQLDocument(query string) (*gqlDocument, error) {
	tokens, err := tokenizeGraphQL(query)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrBadQuery)
	}
	parser := &gqlParser{tokens: tokens}
	document := &gqlDocument{fragments: make(map[string]*gqlFragment)}

	for !parser.atEOF() {
		token := parser.peek()
		switch {
		case token.kind == tkPunct && token.value == "{":
			operation, err := parser.parseOperation()
			if err != nil {
				return nil, err
			}
			document.operations = append(document.operations, operation)
		case token.kind == tkName && isOperationKeyword(token.value):
			operation, err := parser.parseOperation()
			if err != nil {
				return nil, err
			}
			document.operations = append(document.operations, operation)
		case token.kind == tkName && token.value == "fragment":
			fragment, err := parser.parseFragment()
			if err != nil {
				return nil, err
			}
			if _, exists := document.fragments[fragment.name]; exists {
				return nil, fmt.Errorf("%w: duplicate fragment %q", ErrBadQuery, fragment.name)
			}
			document.fragments[fragment.name] = fragment
		default:
			return nil, badQueryf("unexpected top-level token %q", token.value)
		}
	}
	return document, nil
}

func badQueryf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadQuery, fmt.Sprintf(format, args...))
}

func isOperationKeyword(value string) bool {
	return value == "query" || value == "mutation" || value == "subscription"
}

func tokenizeGraphQL(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	for i := 0; i < len(source); {
		char := source[i]
		if char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == ',' {
			i++
			continue
		}
		if char == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if isNameStartByte(char) {
			start := i
			i++
			for i < len(source) && isNameContinueByte(source[i]) {
				i++
			}
			tokens = append(tokens, gqlToken{kind: tkName, value: source[start:i]})
			continue
		}
		if char == '-' || (char >= '0' && char <= '9') {
			token, next, err := lexNumber(source, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, gqlToken{kind: token.kind, value: token.value})
			i = next
			continue
		}
		if char == '"' {
			token, next, err := lexString(source, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token)
			i = next
			continue
		}
		if char == '.' {
			if i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
				tokens = append(tokens, gqlToken{kind: tkPunct, value: "..."})
				i += 3
				continue
			}
			return nil, badQueryf("illegal character %q", char)
		}
		if isAllowedPunctuation(char) {
			tokens = append(tokens, gqlToken{kind: tkPunct, value: string(char)})
			i++
			continue
		}
		return nil, badQueryf("illegal character %q", char)
	}
	return tokens, nil
}

func isNameStartByte(char byte) bool {
	return char == '_' || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z')
}

func isNameContinueByte(char byte) bool {
	return isNameStartByte(char) || (char >= '0' && char <= '9')
}

func isAllowedPunctuation(char byte) bool {
	switch char {
	case '!', '$', '&', '(', ')', ':', '=', '@', '[', ']', '{', '|', '}':
		return true
	default:
		return false
	}
}

func lexNumber(source string, start int) (gqlToken, int, error) {
	i := start
	isFloat := false
	if source[i] == '-' {
		i++
	}
	if i >= len(source) || source[i] < '0' || source[i] > '9' {
		return gqlToken{}, i, badQueryf("malformed number")
	}
	if source[i] == '0' {
		i++
		if i < len(source) && source[i] >= '0' && source[i] <= '9' {
			return gqlToken{}, i, badQueryf("malformed number")
		}
	} else {
		for i < len(source) && source[i] >= '0' && source[i] <= '9' {
			i++
		}
	}
	if i < len(source) && source[i] == '.' {
		isFloat = true
		i++
		digits := 0
		for i < len(source) && source[i] >= '0' && source[i] <= '9' {
			digits++
			i++
		}
		if digits == 0 {
			return gqlToken{}, i, badQueryf("malformed float")
		}
	}
	if i < len(source) && (source[i] == 'e' || source[i] == 'E') {
		isFloat = true
		i++
		if i < len(source) && (source[i] == '+' || source[i] == '-') {
			i++
		}
		digits := 0
		for i < len(source) && source[i] >= '0' && source[i] <= '9' {
			digits++
			i++
		}
		if digits == 0 {
			return gqlToken{}, i, badQueryf("malformed exponent")
		}
	}
	kind := tkInt
	if isFloat {
		kind = tkFloat
	}
	return gqlToken{kind: kind, value: source[start:i]}, i, nil
}

func lexString(source string, start int) (gqlToken, int, error) {
	if start+2 < len(source) && source[start:start+3] == `"""` {
		i := start + 3
		for i < len(source) {
			if i+3 < len(source) && source[i:i+4] == `\"""` {
				i += 4
				continue
			}
			if i+2 < len(source) && source[i:i+3] == `"""` {
				return gqlToken{kind: tkString, value: source[start+3 : i]}, i + 3, nil
			}
			i++
		}
		return gqlToken{}, i, badQueryf("unterminated block string")
	}

	i := start + 1
	for i < len(source) {
		switch source[i] {
		case '\\':
			i += 2
			continue
		case '"':
			raw := source[start : i+1]
			value, err := strconv.Unquote(strings.ReplaceAll(raw, `\/`, `/`))
			if err != nil {
				return gqlToken{}, i, badQueryf("malformed string")
			}
			return gqlToken{kind: tkString, value: value}, i + 1, nil
		case '\n', '\r':
			return gqlToken{}, i, badQueryf("unterminated string")
		default:
			i++
		}
	}
	return gqlToken{}, i, badQueryf("unterminated string")
}

func (p *gqlParser) atEOF() bool {
	return p.pos >= len(p.tokens)
}

func (p *gqlParser) peek() gqlToken {
	if p.atEOF() {
		return gqlToken{kind: tkEOF}
	}
	return p.tokens[p.pos]
}

func (p *gqlParser) advance() gqlToken {
	token := p.peek()
	if !p.atEOF() {
		p.pos++
	}
	return token
}

func (p *gqlParser) eatPunctuation(value string) bool {
	if p.peek().kind == tkPunct && p.peek().value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expectPunctuation(value string) error {
	if !p.eatPunctuation(value) {
		return badQueryf("expected %q near %q", value, p.peek().value)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	token := p.advance()
	if token.kind != tkName {
		return "", badQueryf("expected name near %q", token.value)
	}
	return token.value, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	operation := &gqlOperation{kind: "query", variableDefinitions: make(map[string]*gqlVariableDefinition)}
	if p.peek().kind == tkName && isOperationKeyword(p.peek().value) {
		operation.kind = p.advance().value
	}
	if p.peek().kind == tkName {
		operation.name = p.advance().value
	}
	if p.eatPunctuation("(") {
		for !p.eatPunctuation(")") {
			if err := p.parseVariableDefinition(operation); err != nil {
				return nil, err
			}
		}
	}
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	selections, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	operation.selections = selections
	return operation, nil
}

func (p *gqlParser) parseVariableDefinition(operation *gqlOperation) error {
	if err := p.expectPunctuation("$"); err != nil {
		return err
	}
	name, err := p.expectName()
	if err != nil {
		return err
	}
	if err := p.expectPunctuation(":"); err != nil {
		return err
	}
	if err := p.skipType(); err != nil {
		return err
	}
	definition := &gqlVariableDefinition{name: name}
	if p.eatPunctuation("=") {
		defaultValue, err := p.parseValue()
		if err != nil {
			return err
		}
		if defaultValue.kind == gqlVariable {
			return badQueryf("variable default cannot reference $%s", defaultValue.str)
		}
		definition.defaultValue = &defaultValue
	}
	if err := p.parseDirectives(); err != nil {
		return err
	}
	operation.variableDefinitions[name] = definition
	return nil
}

func (p *gqlParser) skipType() error {
	if p.eatPunctuation("[") {
		if err := p.skipType(); err != nil {
			return err
		}
		if err := p.expectPunctuation("]"); err != nil {
			return err
		}
		p.eatPunctuation("!")
		return nil
	}
	if _, err := p.expectName(); err != nil {
		return err
	}
	p.eatPunctuation("!")
	return nil
}

func (p *gqlParser) parseDirectives() error {
	for p.eatPunctuation("@") {
		if _, err := p.expectName(); err != nil {
			return err
		}
		if _, err := p.parseArguments(); err != nil {
			return err
		}
	}
	return nil
}

func (p *gqlParser) parseArguments() ([]gqlArgument, error) {
	if !p.eatPunctuation("(") {
		return nil, nil
	}
	var arguments []gqlArgument
	for !p.eatPunctuation(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunctuation(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, gqlArgument{name: name, value: value})
	}
	return arguments, nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expectPunctuation("{"); err != nil {
		return nil, err
	}
	return p.parseSelectionsUntilClose()
}

func (p *gqlParser) parseSelectionsUntilClose() ([]gqlSelection, error) {
	var selections []gqlSelection
	for !p.eatPunctuation("}") {
		if p.atEOF() {
			return nil, badQueryf("unclosed selection set")
		}
		if p.peek().kind == tkPunct && p.peek().value == "..." {
			p.advance()
			if p.eatPunctuation("{") {
				inline := &gqlInlineFragment{}
				children, err := p.parseSelectionsUntilClose()
				if err != nil {
					return nil, err
				}
				inline.selections = children
				selections = append(selections, inline)
				continue
			}
			if p.peek().kind == tkName && p.peek().value == "on" {
				p.advance()
				typeName, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.parseDirectives(); err != nil {
					return nil, err
				}
				children, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, &gqlInlineFragment{typeCondition: typeName, selections: children})
				continue
			}
			fragmentName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.parseDirectives(); err != nil {
				return nil, err
			}
			selections = append(selections, &gqlFragmentSpread{name: fragmentName})
			continue
		}

		field, err := p.parseField()
		if err != nil {
			return nil, err
		}
		selections = append(selections, field)
	}
	if len(selections) == 0 {
		return nil, badQueryf("empty selection set")
	}
	return selections, nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	firstName, err := p.expectName()
	if err != nil {
		return nil, err
	}
	field := &gqlField{}
	if p.eatPunctuation(":") {
		field.alias = firstName
		field.name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	} else {
		field.name = firstName
	}
	arguments, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	field.arguments = arguments
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	if p.peek().kind == tkPunct && p.peek().value == "{" {
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		field.selections = children
	}
	return field, nil
}

func (p *gqlParser) parseFragment() (*gqlFragment, error) {
	if _, err := p.expectName(); err != nil { // literal "fragment"
		return nil, err
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	fragment := &gqlFragment{name: name}
	if p.peek().kind == tkName && p.peek().value == "on" {
		p.advance()
		typeName, err := p.expectName()
		if err != nil {
			return nil, err
		}
		fragment.typeCondition = typeName
	}
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	selections, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	fragment.selections = selections
	return fragment, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	token := p.peek()
	switch {
	case token.kind == tkPunct && token.value == "$":
		p.advance()
		name, err := p.expectName()
		if err != nil {
			return gqlValue{}, err
		}
		return gqlValue{kind: gqlVariable, str: name}, nil
	case token.kind == tkInt:
		p.advance()
		number, err := strconv.ParseInt(token.value, 10, 64)
		if err != nil {
			return gqlValue{}, badQueryf("invalid integer %q", token.value)
		}
		return gqlValue{kind: gqlInt, intValue: number}, nil
	case token.kind == tkFloat:
		p.advance()
		number, err := strconv.ParseFloat(token.value, 64)
		if err != nil {
			return gqlValue{}, badQueryf("invalid float %q", token.value)
		}
		return gqlValue{kind: gqlFloat, floatVal: number}, nil
	case token.kind == tkString:
		p.advance()
		return gqlValue{kind: gqlString, str: token.value}, nil
	case token.kind == tkName:
		p.advance()
		switch token.value {
		case "true":
			return gqlValue{kind: gqlBoolean, boolean: true}, nil
		case "false":
			return gqlValue{kind: gqlBoolean, boolean: false}, nil
		case "null":
			return gqlValue{kind: gqlNull}, nil
		default:
			return gqlValue{kind: gqlEnum, str: token.value}, nil
		}
	case token.kind == tkPunct && token.value == "[":
		p.advance()
		var values []gqlValue
		for !p.eatPunctuation("]") {
			if p.atEOF() {
				return gqlValue{}, badQueryf("unclosed list value")
			}
			value, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			values = append(values, value)
		}
		return gqlValue{kind: gqlList, list: values}, nil
	case token.kind == tkPunct && token.value == "{":
		p.advance()
		var fields []gqlObjectField
		for !p.eatPunctuation("}") {
			if p.atEOF() {
				return gqlValue{}, badQueryf("unclosed object value")
			}
			name, err := p.expectName()
			if err != nil {
				return gqlValue{}, err
			}
			if err := p.expectPunctuation(":"); err != nil {
				return gqlValue{}, err
			}
			value, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			fields = append(fields, gqlObjectField{name: name, value: value})
		}
		return gqlValue{kind: gqlObject, object: fields}, nil
	default:
		return gqlValue{}, badQueryf("unexpected value near %q", token.value)
	}
}

func (d *gqlDocument) primaryOperation() *gqlOperation {
	if d == nil || len(d.operations) == 0 {
		return nil
	}
	for _, operation := range d.operations {
		if operation.kind == "query" {
			return operation
		}
	}
	return d.operations[0]
}

func validateOperationVariables(operation *gqlOperation, document *gqlDocument) error {
	seenFragments := make(map[string]bool)
	var walk func([]gqlSelection) error
	walk = func(selections []gqlSelection) error {
		for _, selection := range selections {
			switch item := selection.(type) {
			case *gqlField:
				for _, argument := range item.arguments {
					if err := validateValueVariables(argument.value, operation); err != nil {
						return err
					}
				}
				if err := walk(item.selections); err != nil {
					return err
				}
			case *gqlInlineFragment:
				if err := walk(item.selections); err != nil {
					return err
				}
			case *gqlFragmentSpread:
				if seenFragments[item.name] {
					continue
				}
				seenFragments[item.name] = true
				fragment, ok := document.fragments[item.name]
				if !ok {
					return fmt.Errorf("%w: %s", ErrUnknownFragment, item.name)
				}
				if err := walk(fragment.selections); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(operation.selections)
}

func validateValueVariables(value gqlValue, operation *gqlOperation) error {
	switch value.kind {
	case gqlVariable:
		if operation.variableDefinitions[value.str] == nil {
			return fmt.Errorf("%w: $%s is not defined", ErrBadQuery, value.str)
		}
	case gqlList:
		for _, item := range value.list {
			if err := validateValueVariables(item, operation); err != nil {
				return validateValueVariables(item, operation)
			}
		}
	case gqlObject:
		for _, field := range value.object {
			if err := validateValueVariables(field.value, operation); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cost expansion.

type costBuilder struct {
	document      *gqlDocument
	operation     *gqlOperation
	vars          map[string]any
	fieldCost     int
	maxDepth      int
	maxFields     int
	maxMultiplier int

	fields        int
	depth         int
	introspection bool
}

type logicalField struct {
	field      *gqlField
	selections []gqlSelection
}

func (b *costBuilder) expandSelectionSet(selections []gqlSelection, depth int) (int, error) {
	groups := make([]*logicalField, 0)
	index := make(map[string]int)
	activeFragments := make(map[string]bool)
	if err := b.collectSelections(selections, depth, &groups, index, activeFragments); err != nil {
		return 0, err
	}

	setCost := 0
	for _, group := range groups {
		childCost, err := b.expandSelectionSet(group.selections, depth+1)
		if err != nil {
			return 0, err
		}
		multiplier := b.listMultiplier(group.field)
		fieldCost := saturatedAdd(b.fieldCost, saturatedMul(multiplier, childCost))
		setCost = saturatedAdd(setCost, fieldCost)
	}
	return setCost, nil
}

func (b *costBuilder) collectSelections(
	selections []gqlSelection,
	depth int,
	groups *[]*logicalField,
	index map[string]int,
	active map[string]bool,
) error {
	for _, selection := range selections {
		switch item := selection.(type) {
		case *gqlField:
			if depth > b.maxDepth {
				return ErrDepthLimit
			}
			b.fields++
			if b.fields > b.maxFields {
				return ErrFieldLimit
			}
			if depth > b.depth {
				b.depth = depth
			}
			if isIntrospectionFieldName(item.name) {
				b.introspection = true
			}

			key := b.fieldIdentityKey(item)
			if existing, ok := index[key]; ok {
				(*groups)[existing].selections = append((*groups)[existing].selections, item.selections...)
				continue
			}
			index[key] = len(*groups)
			*groups = append(*groups, &logicalField{
				field:      item,
				selections: append([]gqlSelection(nil), item.selections...),
			})
		case *gqlInlineFragment:
			if err := b.collectSelections(item.selections, depth, groups, index, active); err != nil {
				return err
			}
		case *gqlFragmentSpread:
			fragment, ok := b.document.fragments[item.name]
			if !ok {
				return ErrUnknownFragment
			}
			if active[item.name] {
				return ErrFragmentCycle
			}
			active[item.name] = true
			err := b.collectSelections(fragment.selections, depth, groups, index, active)
			delete(active, item.name)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func isIntrospectionFieldName(name string) bool {
	return name == "__schema" || name == "__type"
}

func (b *costBuilder) fieldIdentityKey(field *gqlField) string {
	responseName := field.alias
	if responseName == "" {
		responseName = field.name
	}

	var key strings.Builder
	key.WriteString(responseName)
	key.WriteByte(0)
	key.WriteString(field.name)

	arguments := append([]gqlArgument(nil), field.arguments...)
	sort.SliceStable(arguments, func(i, j int) bool { return arguments[i].name < arguments[j].name })
	for _, argument := range arguments {
		resolved, _ := b.resolveValue(argument.value)
		payload, err := json.Marshal(resolved)
		if err != nil {
			payload = []byte(fmt.Sprintf("%v", resolved))
		}
		key.WriteByte(0)
		key.WriteString(argument.name)
		key.WriteByte('=')
		key.Write(payload)
	}
	return key.String()
}

func (b *costBuilder) listMultiplier(field *gqlField) int {
	multiplier := 1
	found := false
	for _, argument := range field.arguments {
		if argument.name != "first" && argument.name != "last" && argument.name != "limit" {
			continue
		}
		resolved, ok := b.resolveValue(argument.value)
		if !ok {
			continue
		}
		value, ok := integerFromAny(resolved)
		if !ok {
			continue
		}
		if value < 0 {
			value = 0
		}
		if value > int64(b.maxMultiplier) {
			value = int64(b.maxMultiplier)
		}
		candidate := int(value)
		if !found || candidate > multiplier {
			multiplier = candidate
			found = true
		}
	}
	return multiplier
}

func (b *costBuilder) resolveValue(value gqlValue) (any, bool) {
	switch value.kind {
	case gqlVariable:
		if b.vars != nil {
			if supplied, ok := b.vars[value.str]; ok {
				return supplied, true
			}
		}
		if b.operation != nil {
			if definition := b.operation.variableDefinitions[value.str]; definition != nil && definition.defaultValue != nil {
				return literalValueToAny(*definition.defaultValue), true
			}
		}
		return nil, false
	case gqlInt:
		return value.intValue, true
	case gqlFloat:
		return value.floatVal, true
	case gqlString:
		return value.str, true
	case gqlBoolean:
		return value.boolean, true
	case gqlEnum:
		return value.str, true
	case gqlNull:
		return nil, true
	case gqlList:
		result := make([]any, 0, len(value.list))
		for _, item := range value.list {
			resolved, _ := b.resolveValue(item)
			result = append(result, resolved)
		}
		return result, true
	case gqlObject:
		result := make(map[string]any, len(value.object))
		for _, field := range value.object {
			resolved, _ := b.resolveValue(field.value)
			result[field.name] = resolved
		}
		return result, true
	default:
		return nil, false
	}
}

func literalValueToAny(value gqlValue) any {
	switch value.kind {
	case gqlInt:
		return value.intValue
	case gqlFloat:
		return value.floatVal
	case gqlString:
		return value.str
	case gqlBoolean:
		return value.boolean
	case gqlNull:
		return nil
	case gqlEnum:
		return value.str
	case gqlList:
		result := make([]any, 0, len(value.list))
		for _, item := range value.list {
			result = append(result, literalValueToAny(item))
		}
		return result
	case gqlObject:
		result := make(map[string]any, len(value.object))
		for _, field := range value.object {
			result[field.name] = literalValueToAny(field.value)
		}
		return result
	default:
		return nil
	}
}

func integerFromAny(value any) (int64, bool) {
	if value == nil {
		return 0, false
	}
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int8:
		return int64(number), true
	case int16:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case uint:
		return unsignedToInt64(uint64(number)), true
	case uint8:
		return int64(number), true
	case uint16:
		return int64(number), true
	case uint32:
		return int64(number), true
	case uint64:
		return unsignedToInt64(number), true
	case uintptr:
		return unsignedToInt64(uint64(number)), true
	case float32:
		return floatToInt64(float64(number))
	case float64:
		return floatToInt64(number)
	case json.Number:
		if integer, err := number.Int64(); err == nil {
			return integer, true
		}
		if float, err := number.Float64(); err == nil {
			return floatToInt64(float)
		}
	case string:
		text := strings.TrimSpace(number)
		if integer, err := strconv.ParseInt(text, 10, 64); err == nil {
			return integer, true
		}
		if float, err := strconv.ParseFloat(text, 64); err == nil {
			return floatToInt64(float)
		}
	}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return 0, false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return unsignedToInt64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return floatToInt64(rv.Float())
	case reflect.String:
		return integerFromAny(rv.String())
	}
	return 0, false
}

func floatToInt64(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64, true
	}
	if value <= float64(math.MinInt64) {
		return math.MinInt64, true
	}
	return int64(value), true
}

func unsignedToInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}
