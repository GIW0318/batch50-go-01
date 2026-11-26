// Package task works out whether a shared edge cache folds a request header
// into its key, plants an invisible marker for the headers it proves influential,
// and keeps every planted payload on a ticket until each cache tier confirms
// that the entry is gone.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Ticket states kept by the prober.
const (
	TicketPending = "pending"
	TicketPurged  = "purged"

	// ReasonClean is the verdict reason returned after every tier is clean.
	ReasonClean = "clean"

	snapshotVersion = 1
	probeTestValue  = "gsb-controlled-probe"
)

// Errors reported by the prober.
var (
	ErrUnknownScope         = errors.New("scope is incomplete or unknown")
	ErrUnknownTicket        = errors.New("unknown ticket")
	ErrBadHeader            = errors.New("header name is required")
	ErrBadPayload           = errors.New("payload marker is required")
	ErrVisiblePayload       = errors.New("visible payload is rejected")
	ErrHighTraffic          = errors.New("outside the agreed low traffic window")
	ErrProbeLimit           = errors.New("probe budget exhausted for this scope")
	ErrScopeBusy            = errors.New("too many scopes are tracked")
	ErrNotInferred          = errors.New("scope key was never inferred")
	ErrHeaderNotInfluential = errors.New("header does not influence the response")
	ErrTicketLimit          = errors.New("ticket table is full")
	ErrPurgeIncomplete      = errors.New("purge did not complete on every tier")
	ErrBadSnapshot          = errors.New("snapshot payload is invalid")
)

// Response is one representation served for the scope key.
type Response struct {
	Status int
	Body   string
}

// CacheTier is one layer of the caching stack, outermost tier first.
type CacheTier interface {
	Name() string
	Fetch(headers map[string]string) (Response, error)
	Store(resp Response) error
	Purge() error
}

// Scope is one cache deployment under test.
type Scope struct {
	ID               string
	Key              string
	Tiers            []CacheTier
	CandidateHeaders []string
	LowTraffic       bool
}

// Limits bounds the work the prober may spend.
type Limits struct {
	MaxProbesPerScope   int
	MaxConcurrentScopes int
	MaxTickets          int
}

// KeyModel is the inferred key composition of one scope.
type KeyModel struct {
	ScopeID    string
	Headers    []string
	ProbesUsed int
}

// Influence is the result of probing one request header.
type Influence struct {
	Header      string
	Influential bool
	ProbesUsed  int
}

// Payload is the marker planted into the cache.
type Payload struct {
	Header  string
	Value   string
	Marker  string
	Visible bool
}

// Ticket records one planted payload until every tier is clean.
type Ticket struct {
	ID      string
	ScopeID string
	Header  string
	Marker  string
	State   string
	Seq     int64
	Tiers   []string
}

// Verdict is the outcome of verifying one ticket.
type Verdict struct {
	TicketID string
	Poisoned bool
	Evidence string
	Reason   string
}

// CachePoisonProber owns inferred models, probe budgets, and the ticket table.
type CachePoisonProber struct {
	limits Limits

	mu           sync.Mutex
	scopes       map[string]*scopeState
	tickets      map[string]Ticket
	order        []string
	seq          int64
	activeScopes int
}

type scopeState struct {
	opMu sync.Mutex

	active      bool
	config      Scope
	inferred    bool
	probes      int
	hasBaseline bool
	baseline    Response
	model       KeyModel
	results     map[string]Influence
}

type snapshotDoc struct {
	Version int             `json:"version"`
	Seq     int64           `json:"seq"`
	Tickets []Ticket        `json:"tickets,omitempty"`
	Scopes  []scopeSnapshot `json:"scopes,omitempty"`
}

type scopeSnapshot struct {
	ID               string               `json:"id"`
	Key              string               `json:"key"`
	CandidateHeaders []string             `json:"candidate_headers,omitempty"`
	LowTraffic       bool                 `json:"low_traffic"`
	ProbesUsed       int                  `json:"probes_used"`
	Inferred         bool                 `json:"inferred"`
	HasBaseline      bool                 `json:"has_baseline"`
	Baseline         Response             `json:"baseline"`
	Model            KeyModel             `json:"model"`
	Results          map[string]Influence `json:"results,omitempty"`
}

// NewCachePoisonProber builds a prober with the given caps.
func NewCachePoisonProber(limits Limits) *CachePoisonProber {
	return &CachePoisonProber{
		limits:  limits,
		scopes:  make(map[string]*scopeState),
		tickets: make(map[string]Ticket),
	}
}

// Attach registers a scope and its tiers, keeping its probe and ticket state.
func (p *CachePoisonProber) Attach(scope Scope) error {
	if err := validateScope(scope); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureMapsLocked()
	_, err := p.registerLocked(scope)
	return err
}

// InferKey works out which candidate headers change the response of the scope.
func (p *CachePoisonProber) InferKey(scope Scope) (KeyModel, error) {
	st, cfg, finish, err := p.acquireScope(scope, false)
	if err != nil {
		return KeyModel{ScopeID: scopeIDOrEmpty(scope.ID)}, err
	}
	defer finish()

	p.mu.Lock()
	if st.inferred {
		model := st.model
		p.mu.Unlock()
		return model, nil
	}
	p.mu.Unlock()

	probes, err := p.ensureBaseline(st, cfg)
	if err != nil {
		return KeyModel{ScopeID: cfg.ID, ProbesUsed: probes}, err
	}

	headers, err := candidateHeaders(cfg.CandidateHeaders)
	if err != nil {
		return KeyModel{ScopeID: cfg.ID, ProbesUsed: p.probeCount(st)}, err
	}

	influential := make([]string, 0, len(headers))
	for _, header := range headers {
		result, probeErr := p.probeHeader(st, cfg, header)
		if probeErr != nil {
			return KeyModel{ScopeID: cfg.ID, ProbesUsed: p.probeCount(st)}, probeErr
		}
		if result.Influential {
			influential = append(influential, result.Header)
		}
	}
	sort.Strings(influential)

	model := KeyModel{
		ScopeID:    cfg.ID,
		Headers:    influential,
		ProbesUsed: p.probeCount(st),
	}

	p.mu.Lock()
	st.inferred = true
	st.model = model
	p.mu.Unlock()

	return model, nil
}

// ProbeHeader reports whether one header changes the response of the scope.
func (p *CachePoisonProber) ProbeHeader(scope Scope, header string) (Influence, error) {
	name := strings.TrimSpace(header)
	st, cfg, finish, err := p.acquireScope(scope, true)
	if err != nil {
		return Influence{}, err
	}
	defer finish()

	if name == "" {
		return Influence{}, ErrBadHeader
	}
	return p.probeHeader(st, cfg, name)
}

// Poison plants an invisible marker through one proven header.
func (p *CachePoisonProber) Poison(scope Scope, payload Payload) (Ticket, error) {
	st, cfg, finish, err := p.acquireScope(scope, true)
	if err != nil {
		return Ticket{}, err
	}
	defer finish()

	marker := payload.Marker
	if strings.TrimSpace(marker) == "" {
		return Ticket{}, ErrBadPayload
	}
	if payload.Visible {
		return Ticket{}, ErrVisiblePayload
	}

	header := strings.TrimSpace(payload.Header)
	if header == "" {
		return Ticket{}, ErrBadHeader
	}
	if !cfg.LowTraffic {
		return Ticket{}, ErrHighTraffic
	}

	key := strings.ToLower(header)
	p.mu.Lock()
	influence, proven := st.results[key]
	baseline := st.baseline
	p.mu.Unlock()
	if !proven || !influence.Influential {
		return Ticket{}, ErrHeaderNotInfluential
	}

	tierNames := tierNames(cfg.Tiers)
	ticket := Ticket{}
	p.mu.Lock()
	p.ensureMapsLocked()
	if len(p.tickets) >= p.limits.MaxTickets {
		p.mu.Unlock()
		return Ticket{}, ErrTicketLimit
	}

	p.seq++
	ticket = Ticket{
		ID:      fmt.Sprintf("ticket-%d", p.seq),
		ScopeID: cfg.ID,
		Header:  header,
		Marker:  marker,
		State:   TicketPending,
		Seq:     p.seq,
		Tiers:   append([]string(nil), tierNames...),
	}
	p.tickets[ticket.ID] = cloneTicket(ticket)
	p.order = append(p.order, ticket.ID)
	p.mu.Unlock()

	p.mu.Lock()
	markers := pendingMarkersLocked(p.tickets, cfg.ID)
	p.mu.Unlock()
	planted := invisibleMarkersBody(baseline, markers)
	var storeFailures []string
	for _, tier := range cfg.Tiers {
		if storeErr := tier.Store(Response{Status: responseStatus(baseline), Body: planted}); storeErr != nil {
			storeFailures = append(storeFailures, fmt.Sprintf("%s: %v", tierName(tier), storeErr))
		}
	}
	if len(storeFailures) > 0 {
		return ticket, fmt.Errorf("plant marker for %s: %s", ticket.ID, strings.Join(storeFailures, "; "))
	}

	return ticket, nil
}

// Verify reports whether the planted marker is still served for the ticket.
func (p *CachePoisonProber) Verify(ticketID string) (Verdict, error) {
	id := strings.TrimSpace(ticketID)
	if id == "" {
		return Verdict{}, ErrUnknownTicket
	}

	p.mu.Lock()
	ticket, ok := p.tickets[id]
	p.mu.Unlock()
	if !ok {
		return Verdict{TicketID: id}, ErrUnknownTicket
	}

	_, cfg, finish, err := p.acquireKnownScope(ticket.ScopeID)
	if err != nil {
		return Verdict{TicketID: ticket.ID}, err
	}
	defer finish()

	if len(cfg.Tiers) == 0 {
		return Verdict{TicketID: ticket.ID}, ErrUnknownScope
	}

	names := tierNames(cfg.Tiers)
	var fetchFailures []string
	for index, tier := range cfg.Tiers {
		name := tierName(tier)
		resp, fetchErr := tier.Fetch(map[string]string{})
		if fetchErr != nil {
			fetchFailures = append(fetchFailures, fmt.Sprintf("%s: %v", name, fetchErr))
			continue
		}
		if strings.Contains(resp.Body, ticket.Marker) {
			evidence := fmt.Sprintf(
				"marker original text %s was served by cache layer %d/%d (%s); the hit is on the %s tier",
				ticket.Marker, index+1, len(cfg.Tiers), name, name,
			)
			return Verdict{
				TicketID: ticket.ID,
				Poisoned: true,
				Evidence: evidence,
				Reason:   "marker_hit",
			}, nil
		}
	}

	evidence := fmt.Sprintf(
		"marker original text %s was absent from every tier in outer-to-inner order: %s",
		ticket.Marker, strings.Join(names, " -> "),
	)
	if len(fetchFailures) > 0 {
		evidence += "; fetch could not confirm: " + strings.Join(fetchFailures, "; ")
		verdict := Verdict{TicketID: ticket.ID, Poisoned: false, Evidence: evidence, Reason: "fetch_failed"}
		return verdict, fmt.Errorf("verify %s: %s", ticket.ID, strings.Join(fetchFailures, "; "))
	}

	reason := "marker_not_found"
	if ticket.State == TicketPurged {
		reason = ReasonClean
		evidence = fmt.Sprintf(
			"marker original text %s is gone after outer-to-inner purge; all tiers confirmed clean: %s",
			ticket.Marker, strings.Join(names, " -> "),
		)

		p.mu.Lock()
		if current, exists := p.tickets[ticket.ID]; exists && current.State != TicketPurged {
			current.State = TicketPurged
			p.tickets[ticket.ID] = current
		}
		p.mu.Unlock()
	}

	return Verdict{
		TicketID: ticket.ID,
		Poisoned: false,
		Evidence: evidence,
		Reason:   reason,
	}, nil
}

// Purge removes the planted entry from every tier of the ticket scope.
func (p *CachePoisonProber) Purge(ticketID string) error {
	id := strings.TrimSpace(ticketID)
	if id == "" {
		return ErrUnknownTicket
	}

	p.mu.Lock()
	ticket, ok := p.tickets[id]
	p.mu.Unlock()
	if !ok {
		return ErrUnknownTicket
	}
	if ticket.State == TicketPurged {
		return nil
	}

	st, cfg, finish, err := p.acquireKnownScope(ticket.ScopeID)
	if err != nil {
		return err
	}
	defer finish()

	if len(cfg.Tiers) == 0 {
		return ErrUnknownScope
	}

	var incomplete []string
	for _, tier := range cfg.Tiers {
		name := tierName(tier)
		if purgeErr := tier.Purge(); purgeErr != nil {
			incomplete = append(incomplete, fmt.Sprintf("%s purge error: %v", name, purgeErr))
		}

		resp, fetchErr := tier.Fetch(map[string]string{})
		switch {
		case fetchErr != nil:
			incomplete = append(incomplete, fmt.Sprintf("%s post-purge fetch error: %v", name, fetchErr))
		case strings.Contains(resp.Body, ticket.Marker):
			incomplete = append(incomplete, fmt.Sprintf("%s still serves marker %q", name, ticket.Marker))
		}
	}

	if len(incomplete) > 0 {
		return fmt.Errorf("%w: ticket %s: %s", ErrPurgeIncomplete, ticket.ID, strings.Join(incomplete, "; "))
	}

	p.mu.Lock()
	if current, exists := p.tickets[ticket.ID]; exists {
		current.State = TicketPurged
		p.tickets[ticket.ID] = current
	}
	survivors := pendingMarkersLocked(p.tickets, ticket.ScopeID)
	baseline := st.baseline
	p.mu.Unlock()

	if len(survivors) > 0 {
		remaining := invisibleMarkersBody(baseline, survivors)
		for index := len(cfg.Tiers) - 1; index >= 0; index-- {
			tier := cfg.Tiers[index]
			if storeErr := tier.Store(Response{Status: responseStatus(baseline), Body: remaining}); storeErr != nil {
				return fmt.Errorf("restore remaining markers after purge %s: %w", ticket.ID, storeErr)
			}
		}
	}

	return nil
}

// Snapshot serialises ticket state together with per-scope probe accounting.
func (p *CachePoisonProber) Snapshot() ([]byte, error) {
	p.mu.Lock()
	p.ensureMapsLocked()

	doc := snapshotDoc{
		Version: snapshotVersion,
		Seq:     p.seq,
		Tickets: make([]Ticket, 0, len(p.order)),
		Scopes:  make([]scopeSnapshot, 0, len(p.scopes)),
	}
	for _, id := range p.order {
		if ticket, ok := p.tickets[id]; ok {
			doc.Tickets = append(doc.Tickets, cloneTicket(ticket))
		}
	}

	for id, st := range p.scopes {
		results := make(map[string]Influence, len(st.results))
		for key, result := range st.results {
			results[key] = result
		}
		doc.Scopes = append(doc.Scopes, scopeSnapshot{
			ID:               id,
			Key:              st.config.Key,
			CandidateHeaders: append([]string(nil), st.config.CandidateHeaders...),
			LowTraffic:       st.config.LowTraffic,
			ProbesUsed:       st.probes,
			Inferred:         st.inferred,
			HasBaseline:      st.hasBaseline,
			Baseline:         st.baseline,
			Model:            cloneModel(st.model),
			Results:          results,
		})
	}
	p.mu.Unlock()

	sort.Slice(doc.Scopes, func(i, j int) bool { return doc.Scopes[i].ID < doc.Scopes[j].ID })

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces prober state with a validated snapshot, all or nothing.
func (p *CachePoisonProber) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("%w: empty snapshot", ErrBadSnapshot)
	}

	var doc snapshotDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(doc); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	tickets := make(map[string]Ticket, len(doc.Tickets))
	order := make([]string, 0, len(doc.Tickets))
	for _, ticket := range doc.Tickets {
		ticket = cloneTicket(ticket)
		tickets[ticket.ID] = ticket
		order = append(order, ticket.ID)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return tickets[order[i]].Seq < tickets[order[j]].Seq
	})

	scopes := make(map[string]*scopeState, len(doc.Scopes))
	for _, saved := range doc.Scopes {
		results := make(map[string]Influence, len(saved.Results))
		for key, result := range saved.Results {
			results[strings.ToLower(key)] = result
		}
		scopes[saved.ID] = &scopeState{
			active:      false,
			inferred:    saved.Inferred,
			probes:      saved.ProbesUsed,
			hasBaseline: saved.HasBaseline,
			baseline:    saved.Baseline,
			model:       cloneModel(saved.Model),
			results:     results,
			config: Scope{
				ID:               saved.ID,
				Key:              saved.Key,
				CandidateHeaders: append([]string(nil), saved.CandidateHeaders...),
				LowTraffic:       saved.LowTraffic,
			},
		}
	}

	p.mu.Lock()
	p.seq = doc.Seq
	p.tickets = tickets
	p.order = order
	p.scopes = scopes
	p.activeScopes = 0
	p.mu.Unlock()

	return nil
}

// Tickets lists the tickets of one scope, oldest first.
func (p *CachePoisonProber) Tickets(scopeID string) []Ticket {
	id := strings.TrimSpace(scopeID)
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Ticket, 0)
	for _, ticketID := range p.order {
		ticket, ok := p.tickets[ticketID]
		if ok && ticket.ScopeID == id {
			out = append(out, cloneTicket(ticket))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// PendingPurge lists the tickets that still need a purge, oldest first.
func (p *CachePoisonProber) PendingPurge() []Ticket {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Ticket, 0)
	for _, ticket := range p.tickets {
		if ticket.State == TicketPending {
			out = append(out, cloneTicket(ticket))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// ProbesUsed reports how many probes the scope has spent so far.
func (p *CachePoisonProber) ProbesUsed(scopeID string) int {
	id := strings.TrimSpace(scopeID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.scopes[id]; ok {
		return st.probes
	}
	return 0
}

// Release frees the tracking slot of a scope and keeps its tickets and budget.
func (p *CachePoisonProber) Release(scopeID string) error {
	id := strings.TrimSpace(scopeID)
	if id == "" {
		return ErrUnknownScope
	}

	p.mu.Lock()
	st, ok := p.scopes[id]
	p.mu.Unlock()
	if !ok {
		return ErrUnknownScope
	}

	st.opMu.Lock()
	defer st.opMu.Unlock()

	p.mu.Lock()
	if st.active {
		st.active = false
		p.activeScopes--
	}
	p.mu.Unlock()
	return nil
}

func (p *CachePoisonProber) acquireScope(scope Scope, requireInferred bool) (*scopeState, Scope, func(), error) {
	if err := validateScope(scope); err != nil {
		return nil, Scope{}, func() {}, err
	}

	p.mu.Lock()
	p.ensureMapsLocked()
	if requireInferred {
		id := strings.TrimSpace(scope.ID)
		if _, exists := p.scopes[id]; !exists {
			p.mu.Unlock()
			return nil, Scope{}, func() {}, ErrNotInferred
		}
	}
	st, err := p.registerLocked(scope)
	p.mu.Unlock()
	if err != nil {
		return nil, Scope{}, func() {}, err
	}

	st.opMu.Lock()

	p.mu.Lock()
	if !st.active {
		if p.activeScopes >= p.limits.MaxConcurrentScopes {
			p.mu.Unlock()
			st.opMu.Unlock()
			return nil, Scope{}, func() {}, ErrScopeBusy
		}
		st.active = true
		p.activeScopes++
	}
	cfg := st.config
	inferred := st.inferred
	p.mu.Unlock()

	if requireInferred && !inferred {
		st.opMu.Unlock()
		return nil, Scope{}, func() {}, ErrNotInferred
	}

	finish := func() { st.opMu.Unlock() }
	return st, cfg, finish, nil
}

func (p *CachePoisonProber) acquireKnownScope(scopeID string) (*scopeState, Scope, func(), error) {
	p.mu.Lock()
	st, ok := p.scopes[scopeID]
	p.mu.Unlock()
	if !ok {
		return nil, Scope{}, func() {}, ErrUnknownScope
	}

	st.opMu.Lock()
	p.mu.Lock()
	cfg := st.config
	p.mu.Unlock()

	finish := func() { st.opMu.Unlock() }
	return st, cfg, finish, nil
}

func (p *CachePoisonProber) registerLocked(scope Scope) (*scopeState, error) {
	p.ensureMapsLocked()

	scope.ID = strings.TrimSpace(scope.ID)
	scope.Key = strings.TrimSpace(scope.Key)
	scope.Tiers = append([]CacheTier(nil), scope.Tiers...)
	scope.CandidateHeaders = append([]string(nil), scope.CandidateHeaders...)

	st, ok := p.scopes[scope.ID]
	if !ok {
		if p.activeScopes >= p.limits.MaxConcurrentScopes {
			return nil, ErrScopeBusy
		}
		st = &scopeState{
			active:  true,
			config:  scope,
			results: make(map[string]Influence),
		}
		p.scopes[scope.ID] = st
		p.activeScopes++
		return st, nil
	}

	if !st.active && p.activeScopes >= p.limits.MaxConcurrentScopes {
		return nil, ErrScopeBusy
	}

	st.config = scope
	if st.results == nil {
		st.results = make(map[string]Influence)
	}
	if !st.active {
		st.active = true
		p.activeScopes++
	}
	return st, nil
}

func (p *CachePoisonProber) ensureMapsLocked() {
	if p.scopes == nil {
		p.scopes = make(map[string]*scopeState)
	}
	if p.tickets == nil {
		p.tickets = make(map[string]Ticket)
	}
}

func (p *CachePoisonProber) ensureBaseline(st *scopeState, cfg Scope) (int, error) {
	p.mu.Lock()
	if st.hasBaseline {
		used := st.probes
		p.mu.Unlock()
		return used, nil
	}
	if st.probes >= p.limits.MaxProbesPerScope {
		used := st.probes
		p.mu.Unlock()
		return used, ErrProbeLimit
	}
	st.probes++
	used := st.probes
	p.mu.Unlock()

	resp, err := cfg.Tiers[0].Fetch(map[string]string{})
	if err != nil {
		return used, fmt.Errorf("fetch baseline for scope %q: %w", cfg.ID, err)
	}

	p.mu.Lock()
	st.baseline = resp
	st.hasBaseline = true
	p.mu.Unlock()
	return used, nil
}

func (p *CachePoisonProber) probeHeader(st *scopeState, cfg Scope, name string) (Influence, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Influence{}, ErrBadHeader
	}
	key := strings.ToLower(name)

	p.mu.Lock()
	if prior, ok := st.results[key]; ok {
		result := prior
		p.mu.Unlock()
		return result, nil
	}
	if !st.hasBaseline {
		p.mu.Unlock()
		return Influence{Header: name}, ErrNotInferred
	}
	if st.probes >= p.limits.MaxProbesPerScope {
		used := st.probes
		p.mu.Unlock()
		return Influence{Header: name, ProbesUsed: used}, ErrProbeLimit
	}

	st.probes++
	used := st.probes
	baselineBody := st.baseline.Body
	p.mu.Unlock()

	resp, err := cfg.Tiers[0].Fetch(map[string]string{name: probeTestValue})
	if err != nil {
		return Influence{Header: name, ProbesUsed: used}, fmt.Errorf("probe %q in scope %q: %w", name, cfg.ID, err)
	}

	result := Influence{
		Header:      name,
		Influential: resp.Body != baselineBody,
		ProbesUsed:  used,
	}

	p.mu.Lock()
	st.results[key] = result
	p.mu.Unlock()

	return result, nil
}

func (p *CachePoisonProber) probeCount(st *scopeState) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return st.probes
}

func validateScope(scope Scope) error {
	if strings.TrimSpace(scope.ID) == "" || strings.TrimSpace(scope.Key) == "" || len(scope.Tiers) == 0 {
		return ErrUnknownScope
	}
	for _, tier := range scope.Tiers {
		if tier == nil || strings.TrimSpace(tier.Name()) == "" {
			return ErrUnknownScope
		}
	}
	return nil
}

func candidateHeaders(candidates []string) ([]string, error) {
	seen := make(map[string]struct{}, len(candidates))
	headers := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		header := strings.TrimSpace(candidate)
		if header == "" {
			return nil, ErrBadHeader
		}
		key := strings.ToLower(header)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		headers = append(headers, header)
	}
	return headers, nil
}

func validateSnapshot(doc snapshotDoc) error {
	if doc.Version != snapshotVersion {
		return fmt.Errorf("unsupported snapshot version %d", doc.Version)
	}

	seenTickets := make(map[string]struct{}, len(doc.Tickets))
	seenSeqs := make(map[int64]struct{}, len(doc.Tickets))
	var maxSeq int64
	for _, ticket := range doc.Tickets {
		if strings.TrimSpace(ticket.ID) == "" {
			return errors.New("ticket id is empty")
		}
		if _, ok := seenTickets[ticket.ID]; ok {
			return fmt.Errorf("duplicate ticket id %q", ticket.ID)
		}
		seenTickets[ticket.ID] = struct{}{}

		if ticket.Seq < 1 {
			return fmt.Errorf("ticket %q has invalid sequence %d", ticket.ID, ticket.Seq)
		}
		if _, ok := seenSeqs[ticket.Seq]; ok {
			return fmt.Errorf("duplicate ticket sequence %d", ticket.Seq)
		}
		seenSeqs[ticket.Seq] = struct{}{}
		if ticket.Seq > maxSeq {
			maxSeq = ticket.Seq
		}

		if ticket.State != TicketPending && ticket.State != TicketPurged {
			return fmt.Errorf("ticket %q has invalid state %q", ticket.ID, ticket.State)
		}
		if strings.TrimSpace(ticket.ScopeID) == "" {
			return fmt.Errorf("ticket %q has no scope", ticket.ID)
		}
		if strings.TrimSpace(ticket.Marker) == "" {
			return fmt.Errorf("ticket %q has no marker", ticket.ID)
		}
		if strings.TrimSpace(ticket.Header) == "" {
			return fmt.Errorf("ticket %q has no header", ticket.ID)
		}
		if len(ticket.Tiers) == 0 {
			return fmt.Errorf("ticket %q has no cache tiers", ticket.ID)
		}
	}
	if doc.Seq < maxSeq {
		return fmt.Errorf("snapshot sequence %d is below ticket sequence %d", doc.Seq, maxSeq)
	}

	seenScopes := make(map[string]struct{}, len(doc.Scopes))
	for _, scope := range doc.Scopes {
		if strings.TrimSpace(scope.ID) == "" {
			return errors.New("scope id is empty")
		}
		if _, ok := seenScopes[scope.ID]; ok {
			return fmt.Errorf("duplicate scope %q", scope.ID)
		}
		seenScopes[scope.ID] = struct{}{}
		if scope.ProbesUsed < 0 {
			return fmt.Errorf("scope %q has negative probe count", scope.ID)
		}
	}
	return nil
}

func cloneTicket(ticket Ticket) Ticket {
	ticket.Tiers = append([]string(nil), ticket.Tiers...)
	return ticket
}

func cloneModel(model KeyModel) KeyModel {
	model.Headers = append([]string(nil), model.Headers...)
	return model
}

func tierNames(tiers []CacheTier) []string {
	names := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		names = append(names, tierName(tier))
	}
	return names
}

func tierName(tier CacheTier) string {
	if tier == nil {
		return "unknown-tier"
	}
	name := strings.TrimSpace(tier.Name())
	if name == "" {
		return "unnamed-tier"
	}
	return name
}

func responseStatus(resp Response) int {
	if resp.Status == 0 {
		return 200
	}
	return resp.Status
}

func invisibleMarkersBody(baseline Response, markers []string) string {
	body := baseline.Body
	for _, marker := range markers {
		if !strings.Contains(body, marker) {
			body += "<!--cache-poison-probe:" + marker + "-->"
		}
	}
	return body
}

func pendingMarkersLocked(tickets map[string]Ticket, scopeID string) []string {
	scoped := make([]Ticket, 0)
	for _, ticket := range tickets {
		if ticket.ScopeID == scopeID && ticket.State == TicketPending {
			scoped = append(scoped, ticket)
		}
	}
	sort.Slice(scoped, func(i, j int) bool { return scoped[i].Seq < scoped[j].Seq })

	markers := make([]string, 0, len(scoped))
	for _, ticket := range scoped {
		markers = append(markers, ticket.Marker)
	}
	return markers
}

func scopeIDOrEmpty(id string) string {
	return strings.TrimSpace(id)
}
