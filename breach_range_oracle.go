// Package task answers leaked password lookups with a k anonymous prefix
// query, grades the hit count and keeps a per tenant budget and circuit
// breaker around the upstream range service.
package task

import (
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hit bands derived from the leaked occurrence count.
const (
	BandNone        = "none"
	BandLow         = "low"
	BandHigh        = "high"
	BandCritical    = "critical"
	BandUnavailable = "unavailable"
	BandBudget      = "budget_exhausted"
)

// Actions attached to a verdict.
const (
	ActionAllow  = "allow"
	ActionWarn   = "warn"
	ActionDeny   = "deny"
	ActionReview = "review"
)

// Call sites that may ask for a lookup.
const (
	StageRegister = "register"
	StageChange   = "change"
	StageLogin    = "login"
)

// Where a verdict came from.
const (
	SourceUpstream = "upstream"
	SourceCache    = "cache"
)

// Circuit breaker states and the anonymous prefix length.
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half_open"

	DigestPrefixLen = 5
	suffixLen       = 35

	defaultBudgetLimit = 100
	snapshotVersion    = 1
)

// Errors returned by the oracle.
var (
	ErrBlankTenant      = errors.New("tenant id is required")
	ErrBlankPassword    = errors.New("password is required")
	ErrUnknownStage     = errors.New("unknown stage")
	ErrBudgetLimit      = errors.New("query budget must be positive")
	ErrBadPrefix        = errors.New("prefix must be five uppercase hex digits")
	ErrUpstreamFailed   = errors.New("upstream query failed")
	ErrUpstreamTimeout  = errors.New("upstream query timed out")
	ErrResponseTooLarge = errors.New("upstream response exceeds the size limit")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Limits bounds the cache, one response and the breaker.
type Limits struct {
	MaxCacheEntries  int
	MaxResponseBytes int
	Timeout          time.Duration
	CacheTTL         time.Duration
	CircuitOpenAfter int
}

// Upstream is the range service. FetchPrefix receives five uppercase hex
// digits only and returns the body of the range response.
type Upstream interface {
	FetchPrefix(ctx context.Context, prefix string) ([]byte, error)
}

// Usage is the per tenant query budget snapshot.
type Usage struct {
	TenantID  string `json:"tenant_id"`
	Limit     int    `json:"limit"`
	Used      int    `json:"used"`
	Remaining int    `json:"remaining"`
}

// Verdict is the outcome of one lookup.
type Verdict struct {
	TenantID string `json:"tenant_id"`
	Stage    string `json:"stage"`
	Prefix   string `json:"prefix"`
	Breached bool   `json:"breached"`
	Count    int    `json:"count"`
	Band     string `json:"band"`
	Action   string `json:"action"`
	Source   string `json:"source"`
}

// Status is a read only view of the cache and the breaker.
type Status struct {
	State    string `json:"state"`
	Cached   int    `json:"cached"`
	Prefixes int    `json:"prefixes"`
	Failures int    `json:"failures"`
}

type budgetState struct {
	Limit   int
	Used    int
	Pending int
}

type cacheEntry struct {
	Prefix   string
	Items    map[string]int
	CachedAt time.Time
	element  *list.Element
}

type ticketState int

const (
	ticketPending ticketState = iota
	ticketCharged
	ticketCanceled
	ticketReleased
)

type budgetTicket struct {
	tenantID string
	state    ticketState
}

type callOutcome struct {
	items       map[string]int
	err         error
	unavailable bool
}

type inflightCall struct {
	prefix  string
	epoch   uint64
	done    chan struct{}
	outcome callOutcome
	tickets []*budgetTicket
}

type snapshotData struct {
	Version  int                   `json:"version"`
	Cache    []snapshotCacheEntry  `json:"cache"`
	Budgets  map[string]snapBudget `json:"budgets"`
	Failures int                   `json:"failures"`
}

type snapshotCacheEntry struct {
	Prefix   string         `json:"prefix"`
	Items    map[string]int `json:"items"`
	CachedAt time.Time      `json:"cached_at"`
}

type snapBudget struct {
	Limit int `json:"limit"`
	Used  int `json:"used"`
}

// BreachRangeOracle owns the prefix cache, the per tenant budgets and the
// breaker state.
type BreachRangeOracle struct {
	mu       sync.Mutex
	up       Upstream
	limits   Limits
	cache    map[string]*cacheEntry
	order    *list.List
	budgets  map[string]*budgetState
	inflight map[string]*inflightCall
	failures int
	epoch    uint64
}

// NewBreachRangeOracle builds an oracle on top of an upstream range service.
func NewBreachRangeOracle(up Upstream, limits Limits) *BreachRangeOracle {
	return &BreachRangeOracle{
		up:       up,
		limits:   limits,
		cache:    make(map[string]*cacheEntry),
		order:    list.New(),
		budgets:  make(map[string]*budgetState),
		inflight: make(map[string]*inflightCall),
	}
}

// Check looks a password up for the login path.
func (o *BreachRangeOracle) Check(ctx context.Context, tenantID, password string) (Verdict, error) {
	return o.CheckFor(ctx, tenantID, StageLogin, password)
}

// CheckFor looks a password up for one named call site.
func (o *BreachRangeOracle) CheckFor(ctx context.Context, tenantID, stage, password string) (Verdict, error) {
	if strings.TrimSpace(tenantID) == "" {
		return Verdict{}, ErrBlankTenant
	}
	if password == "" {
		return Verdict{}, ErrBlankPassword
	}
	if !validStage(stage) {
		return Verdict{}, ErrUnknownStage
	}
	if ctx == nil {
		ctx = context.Background()
	}

	prefix, suffix := passwordRangeParts(password)
	base := Verdict{TenantID: tenantID, Stage: stage, Prefix: prefix}

	o.mu.Lock()
	if o.circuitOpenLocked() {
		o.mu.Unlock()
		return unavailableVerdict(base), nil
	}

	budget := o.budgetLocked(tenantID)
	if budget.Used+budget.Pending >= budget.Limit {
		o.mu.Unlock()
		return budgetExhaustedVerdict(base), nil
	}

	if entry := o.cache[prefix]; entry != nil {
		if o.entryExpiredLocked(entry) {
			o.removeEntryLocked(entry)
		} else {
			o.order.MoveToBack(entry.element)
			budget.Used++
			items := entry.Items
			o.mu.Unlock()
			return gradeVerdict(base, items, suffix, SourceCache), nil
		}
	}

	call, ticket, leader := o.joinLocked(tenantID, prefix)
	o.mu.Unlock()

	if leader {
		o.fetchAndComplete(call, prefix)
	}

	outcome, err := o.waitForCall(ctx, call, ticket)
	if err != nil {
		return Verdict{}, err
	}
	if outcome.err != nil {
		return Verdict{}, outcome.err
	}
	if outcome.unavailable {
		return unavailableVerdict(base), nil
	}
	return gradeVerdict(base, outcome.items, suffix, SourceUpstream), nil
}

// Warm fetches one prefix into the cache without asking for a verdict.
func (o *BreachRangeOracle) Warm(prefix string) error {
	if !validPrefix(prefix) {
		return ErrBadPrefix
	}

	o.mu.Lock()
	if o.circuitOpenLocked() {
		o.mu.Unlock()
		return fmt.Errorf("%w: circuit breaker open", ErrUpstreamFailed)
	}
	if entry := o.cache[prefix]; entry != nil {
		if o.entryExpiredLocked(entry) {
			o.removeEntryLocked(entry)
		} else {
			o.order.MoveToBack(entry.element)
			o.mu.Unlock()
			return nil
		}
	}
	call, leader := o.joinWarmLocked(prefix)
	o.mu.Unlock()

	if leader {
		o.fetchAndComplete(call, prefix)
	}
	<-call.done

	if call.outcome.err != nil {
		return call.outcome.err
	}
	if call.outcome.unavailable {
		return fmt.Errorf("%w: invalid range response", ErrUpstreamFailed)
	}
	return nil
}

// Budget reports the query budget of one tenant.
func (o *BreachRangeOracle) Budget(tenantID string) (Usage, error) {
	if strings.TrimSpace(tenantID) == "" {
		return Usage{}, ErrBlankTenant
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	return o.usageLocked(tenantID), nil
}

// SetBudget replaces the per window query allowance of one tenant.
func (o *BreachRangeOracle) SetBudget(tenantID string, perWindow int) error {
	if strings.TrimSpace(tenantID) == "" {
		return ErrBlankTenant
	}
	if perWindow <= 0 {
		return ErrBudgetLimit
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.budgetLocked(tenantID).Limit = perWindow
	return nil
}

// Health reports the breaker state and the cache size.
func (o *BreachRangeOracle) Health() Status {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.purgeExpiredLocked()
	state := StateClosed
	if o.circuitOpenLocked() {
		state = StateOpen
	}
	size := o.order.Len()
	return Status{State: state, Cached: size, Prefixes: size, Failures: o.failures}
}

// CachePrefixes lists the cached prefixes, most recently used last.
func (o *BreachRangeOracle) CachePrefixes() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.purgeExpiredLocked()
	prefixes := make([]string, 0, o.order.Len())
	for element := o.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		prefixes = append(prefixes, entry.Prefix)
	}
	return prefixes
}

// Reset clears the budget of one tenant.
func (o *BreachRangeOracle) Reset(tenantID string) error {
	if strings.TrimSpace(tenantID) == "" {
		return ErrBlankTenant
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if budget, ok := o.budgets[tenantID]; ok {
		budget.Used = 0
	}
	return nil
}

// Snapshot serialises the cache, the budgets and the breaker counters.
func (o *BreachRangeOracle) Snapshot() ([]byte, error) {
	o.mu.Lock()
	snapshot := o.snapshotLocked()
	o.mu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (o *BreachRangeOracle) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}

	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return err
	}

	cache := make(map[string]*cacheEntry, len(snapshot.Cache))
	order := list.New()
	for _, saved := range snapshot.Cache {
		items := make(map[string]int, len(saved.Items))
		for suffix, count := range saved.Items {
			items[suffix] = count
		}
		entry := &cacheEntry{Prefix: saved.Prefix, Items: items, CachedAt: saved.CachedAt}
		entry.element = order.PushBack(entry)
		cache[saved.Prefix] = entry
	}

	budgets := make(map[string]*budgetState, len(snapshot.Budgets))
	for tenantID, saved := range snapshot.Budgets {
		budgets[tenantID] = &budgetState{Limit: saved.Limit, Used: saved.Used}
	}

	o.mu.Lock()
	o.cache = cache
	o.order = order
	o.budgets = budgets
	o.failures = snapshot.Failures
	o.inflight = make(map[string]*inflightCall)
	o.epoch++
	o.evictOverflowLocked()
	o.mu.Unlock()
	return nil
}

func validStage(stage string) bool {
	return stage == StageRegister || stage == StageChange || stage == StageLogin
}

func isUpperHexDigit(char byte) bool {
	return char >= '0' && char <= '9' || char >= 'A' && char <= 'F'
}

func validPrefix(prefix string) bool {
	if len(prefix) != DigestPrefixLen {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if !isUpperHexDigit(prefix[i]) {
			return false
		}
	}
	return true
}

func validSuffix(suffix string) bool {
	if len(suffix) != suffixLen {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if !isUpperHexDigit(suffix[i]) {
			return false
		}
	}
	return true
}

func passwordRangeParts(password string) (string, string) {
	digest := sha1.Sum([]byte(password))
	upper := strings.ToUpper(hex.EncodeToString(digest[:]))
	return upper[:DigestPrefixLen], upper[DigestPrefixLen:]
}

func (o *BreachRangeOracle) budgetLocked(tenantID string) *budgetState {
	budget, ok := o.budgets[tenantID]
	if !ok {
		budget = &budgetState{Limit: defaultBudgetLimit}
		o.budgets[tenantID] = budget
	}
	if budget.Limit <= 0 {
		budget.Limit = defaultBudgetLimit
	}
	return budget
}

func (o *BreachRangeOracle) usageLocked(tenantID string) Usage {
	limit := defaultBudgetLimit
	used := 0
	if budget, ok := o.budgets[tenantID]; ok {
		limit = budget.Limit
		used = budget.Used
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return Usage{
		TenantID:  tenantID,
		Limit:     limit,
		Used:      used,
		Remaining: remaining,
	}
}

func (o *BreachRangeOracle) circuitOpenLocked() bool {
	return o.limits.CircuitOpenAfter > 0 && o.failures >= o.limits.CircuitOpenAfter
}

func (o *BreachRangeOracle) entryExpiredLocked(entry *cacheEntry) bool {
	return o.limits.CacheTTL > 0 && time.Now().After(entry.CachedAt.Add(o.limits.CacheTTL))
}

func (o *BreachRangeOracle) removeEntryLocked(entry *cacheEntry) {
	if entry.element != nil {
		o.order.Remove(entry.element)
	}
	delete(o.cache, entry.Prefix)
}

func (o *BreachRangeOracle) purgeExpiredLocked() {
	var expired []*cacheEntry
	for element := o.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		if o.entryExpiredLocked(entry) {
			expired = append(expired, entry)
		}
	}
	for _, entry := range expired {
		o.removeEntryLocked(entry)
	}
}

func (o *BreachRangeOracle) putCacheLocked(prefix string, items map[string]int) {
	entry, ok := o.cache[prefix]
	if ok {
		entry.Items = items
		entry.CachedAt = time.Now()
		o.order.MoveToBack(entry.element)
	} else {
		entry = &cacheEntry{Prefix: prefix, Items: items, CachedAt: time.Now()}
		entry.element = o.order.PushBack(entry)
		o.cache[prefix] = entry
	}

	o.evictOverflowLocked()
}

func (o *BreachRangeOracle) evictOverflowLocked() {
	capacity := o.limits.MaxCacheEntries
	if capacity < 0 {
		capacity = 0
	}
	for o.order.Len() > capacity {
		oldest := o.order.Front()
		if oldest == nil {
			break
		}
		old := oldest.Value.(*cacheEntry)
		o.removeEntryLocked(old)
	}
}

func (o *BreachRangeOracle) joinLocked(tenantID, prefix string) (*inflightCall, *budgetTicket, bool) {
	call, ok := o.inflight[prefix]
	leader := false
	if !ok {
		call = &inflightCall{prefix: prefix, epoch: o.epoch, done: make(chan struct{})}
		o.inflight[prefix] = call
		leader = true
	}

	budget := o.budgetLocked(tenantID)
	budget.Pending++
	ticket := &budgetTicket{tenantID: tenantID, state: ticketPending}
	call.tickets = append(call.tickets, ticket)
	return call, ticket, leader
}

func (o *BreachRangeOracle) joinWarmLocked(prefix string) (*inflightCall, bool) {
	call, ok := o.inflight[prefix]
	if ok {
		return call, false
	}
	call = &inflightCall{prefix: prefix, epoch: o.epoch, done: make(chan struct{})}
	o.inflight[prefix] = call
	return call, true
}

func (o *BreachRangeOracle) waitForCall(ctx context.Context, call *inflightCall, ticket *budgetTicket) (callOutcome, error) {
	select {
	case <-call.done:
		return call.outcome, nil
	case <-ctx.Done():
		o.cancelTicket(call, ticket)
		select {
		case <-call.done:
			return call.outcome, nil
		default:
			return callOutcome{}, ctx.Err()
		}
	}
}

func (o *BreachRangeOracle) cancelTicket(call *inflightCall, ticket *budgetTicket) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ticket.state != ticketPending {
		return
	}
	ticket.state = ticketCanceled
	if budget, ok := o.budgets[ticket.tenantID]; ok && budget.Pending > 0 {
		budget.Pending--
	}
}

type upstreamResult struct {
	body []byte
	err  error
}

func (o *BreachRangeOracle) fetchAndComplete(call *inflightCall, prefix string) {
	if o.up == nil {
		o.completeCall(call, callOutcome{err: fmt.Errorf("%w: missing upstream", ErrUpstreamFailed)})
		return
	}

	fetchCtx, cancel := context.WithTimeout(context.Background(), o.limits.Timeout)
	defer cancel()

	results := make(chan upstreamResult, 1)
	go func() {
		body, err := o.up.FetchPrefix(fetchCtx, prefix)
		results <- upstreamResult{body: body, err: err}
	}()

	var result upstreamResult
	select {
	case result = <-results:
		if errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
			o.completeCall(call, callOutcome{err: ErrUpstreamTimeout})
			return
		}
	case <-fetchCtx.Done():
		o.completeCall(call, callOutcome{err: ErrUpstreamTimeout})
		return
	}

	if result.err != nil {
		if errors.Is(result.err, context.DeadlineExceeded) || errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
			o.completeCall(call, callOutcome{err: ErrUpstreamTimeout})
			return
		}
		o.completeCall(call, callOutcome{err: fmt.Errorf("%w: %v", ErrUpstreamFailed, result.err)})
		return
	}

	if o.limits.MaxResponseBytes >= 0 && len(result.body) > o.limits.MaxResponseBytes {
		o.completeCall(call, callOutcome{err: ErrResponseTooLarge})
		return
	}

	items, err := parseRangeResponse(prefix, result.body)
	if err != nil {
		o.completeCall(call, callOutcome{unavailable: true})
		return
	}
	o.completeCall(call, callOutcome{items: items})
}

func (o *BreachRangeOracle) completeCall(call *inflightCall, outcome callOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if call.epoch != o.epoch {
		call.outcome = outcome
		close(call.done)
		return
	}

	delete(o.inflight, call.prefix)
	if outcome.err != nil || outcome.unavailable {
		o.failures++
	} else {
		o.failures = 0
		o.putCacheLocked(call.prefix, outcome.items)
	}

	for _, ticket := range call.tickets {
		if ticket.state != ticketPending {
			continue
		}
		budget := o.budgetLocked(ticket.tenantID)
		if budget.Pending > 0 {
			budget.Pending--
		}
		if outcome.err == nil && !outcome.unavailable {
			budget.Used++
			ticket.state = ticketCharged
		} else {
			ticket.state = ticketReleased
		}
	}

	call.outcome = outcome
	close(call.done)
}

func parseRangeResponse(wantPrefix string, body []byte) (map[string]int, error) {
	lines := strings.Split(string(body), "\n")
	if len(lines) == 0 {
		return nil, ErrUpstreamFailed
	}

	header := strings.TrimSuffix(lines[0], "\r")
	const headerPrefix = "PREFIX:"
	if !strings.HasPrefix(header, headerPrefix) {
		return nil, ErrUpstreamFailed
	}
	gotPrefix := header[len(headerPrefix):]
	if gotPrefix != wantPrefix || !validPrefix(gotPrefix) {
		return nil, ErrUpstreamFailed
	}

	items := make(map[string]int)
	for index := 1; index < len(lines); index++ {
		line := strings.TrimSuffix(lines[index], "\r")
		if index == len(lines)-1 && line == "" {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) != 2 || !validSuffix(parts[0]) || parts[1] == "" {
			return nil, ErrUpstreamFailed
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			return nil, ErrUpstreamFailed
		}
		items[parts[0]] = count
	}
	return items, nil
}

func gradeVerdict(base Verdict, items map[string]int, suffix, source string) Verdict {
	count := items[suffix]
	verdict := base
	verdict.Source = source
	verdict.Count = count

	switch {
	case count <= 0:
		verdict.Breached = false
		verdict.Band = BandNone
		verdict.Action = ActionAllow
	case count < 10:
		verdict.Breached = true
		verdict.Band = BandLow
		verdict.Action = ActionWarn
	case count < 1000:
		verdict.Breached = true
		verdict.Band = BandHigh
		verdict.Action = ActionWarn
	default:
		verdict.Breached = true
		verdict.Band = BandCritical
		verdict.Action = ActionDeny
	}
	return verdict
}

func degradationAction(stage string) string {
	if stage == StageLogin {
		return ActionReview
	}
	return ActionDeny
}

func budgetExhaustedVerdict(base Verdict) Verdict {
	base.Breached = false
	base.Count = 0
	base.Band = BandBudget
	base.Action = degradationAction(base.Stage)
	return base
}

func unavailableVerdict(base Verdict) Verdict {
	base.Breached = false
	base.Count = 0
	base.Band = BandUnavailable
	base.Action = degradationAction(base.Stage)
	return base
}

func (o *BreachRangeOracle) snapshotLocked() snapshotData {
	o.purgeExpiredLocked()
	snapshot := snapshotData{
		Version:  snapshotVersion,
		Cache:    make([]snapshotCacheEntry, 0, o.order.Len()),
		Budgets:  make(map[string]snapBudget, len(o.budgets)),
		Failures: o.failures,
	}

	for element := o.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		items := make(map[string]int, len(entry.Items))
		for suffix, count := range entry.Items {
			items[suffix] = count
		}
		snapshot.Cache = append(snapshot.Cache, snapshotCacheEntry{
			Prefix:   entry.Prefix,
			Items:    items,
			CachedAt: entry.CachedAt,
		})
	}

	for tenantID, budget := range o.budgets {
		snapshot.Budgets[tenantID] = snapBudget{Limit: budget.Limit, Used: budget.Used}
	}
	return snapshot
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Version != snapshotVersion || snapshot.Failures < 0 {
		return ErrBadSnapshot
	}

	seenPrefixes := make(map[string]struct{}, len(snapshot.Cache))
	for _, entry := range snapshot.Cache {
		if !validPrefix(entry.Prefix) || entry.CachedAt.IsZero() {
			return ErrBadSnapshot
		}
		if _, duplicated := seenPrefixes[entry.Prefix]; duplicated {
			return ErrBadSnapshot
		}
		seenPrefixes[entry.Prefix] = struct{}{}

		for suffix, count := range entry.Items {
			if !validSuffix(suffix) || count < 0 {
				return ErrBadSnapshot
			}
		}
	}

	for tenantID, budget := range snapshot.Budgets {
		if strings.TrimSpace(tenantID) == "" || budget.Limit <= 0 || budget.Used < 0 {
			return ErrBadSnapshot
		}
	}
	return nil
}
