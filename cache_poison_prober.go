// Package task works out whether a shared edge cache folds a request header
// into its key, plants an invisible marker for the headers it proves unkeyed,
// and keeps every planted payload on a ticket until each cache tier confirms
// that the entry is gone.
package task

import "errors"

// errNotImplemented is returned by behaviour that is not built yet.
var errNotImplemented = errors.New("not implemented")

// Ticket states kept by the prober.
const (
	TicketPending = "pending"
	TicketPurged  = "purged"
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
	// Name labels the tier, for example edge, mid or origin.
	Name() string
	// Fetch answers what the tier serves for the scope key when the given
	// extra request headers are attached.
	Fetch(headers map[string]string) (Response, error)
	// Store plants a response for the scope key.
	Store(resp Response) error
	// Purge drops the stored entry for the scope key.
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

// CachePoisonProber owns the inferred models and the ticket table.
type CachePoisonProber struct {
	limits Limits
}

// NewCachePoisonProber builds a prober with the given caps.
func NewCachePoisonProber(limits Limits) *CachePoisonProber {
	return &CachePoisonProber{limits: limits}
}

// Attach registers a scope and its tiers, keeping an existing registration.
func (p *CachePoisonProber) Attach(scope Scope) error {
	return errNotImplemented
}

// InferKey works out which candidate headers change the response of the scope.
func (p *CachePoisonProber) InferKey(scope Scope) (KeyModel, error) {
	return KeyModel{}, errNotImplemented
}

// ProbeHeader reports whether one header changes the response of the scope.
func (p *CachePoisonProber) ProbeHeader(scope Scope, header string) (Influence, error) {
	return Influence{}, errNotImplemented
}

// Poison plants an invisible marker through one proven header.
func (p *CachePoisonProber) Poison(scope Scope, payload Payload) (Ticket, error) {
	return Ticket{}, errNotImplemented
}

// Verify reports whether the planted marker is still served for the ticket.
func (p *CachePoisonProber) Verify(ticketID string) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Purge removes the planted entry from every tier of the ticket scope.
func (p *CachePoisonProber) Purge(ticketID string) error {
	return errNotImplemented
}

// Snapshot serialises the ticket table.
func (p *CachePoisonProber) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the ticket table with a snapshot, all or nothing.
func (p *CachePoisonProber) Restore(data []byte) error {
	return errNotImplemented
}

// Tickets lists the tickets of one scope, oldest first.
func (p *CachePoisonProber) Tickets(scopeID string) []Ticket {
	return nil
}

// PendingPurge lists the tickets that still need a purge, oldest first.
func (p *CachePoisonProber) PendingPurge() []Ticket {
	return nil
}

// ProbesUsed reports how many probes the scope has spent so far.
func (p *CachePoisonProber) ProbesUsed(scopeID string) int {
	return 0
}

// Release frees the tracking slot of a scope and keeps its tickets.
func (p *CachePoisonProber) Release(scopeID string) error {
	return errNotImplemented
}
