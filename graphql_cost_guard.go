// Package task guards the public GraphQL endpoint: it analyses documents into
// immutable cost plans and meters tenant budgets over a sliding window.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by behaviour that is not wired up yet.
var errNotImplemented = errors.New("not implemented")

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
	limits Limits
}

// NewGraphqlCostGuard builds a guard with the given limits.
func NewGraphqlCostGuard(limits Limits) *GraphqlCostGuard {
	return &GraphqlCostGuard{limits: limits}
}

// Analyze parses a document and returns its cost plan.
func (g *GraphqlCostGuard) Analyze(query string, vars map[string]any) (Plan, error) {
	return Plan{}, errNotImplemented
}

// Admit decides whether a plan fits the single query and tenant budgets.
func (g *GraphqlCostGuard) Admit(tenantID string, plan Plan) (Decision, error) {
	return Decision{}, errNotImplemented
}

// Charge books an explicit cost against one tenant window.
func (g *GraphqlCostGuard) Charge(tenantID string, cost int) error {
	return errNotImplemented
}

// Refund rolls a charge back when a request failed downstream.
func (g *GraphqlCostGuard) Refund(tenantID string, cost int) error {
	return errNotImplemented
}

// Usage reports the live window of one tenant.
func (g *GraphqlCostGuard) Usage(tenantID string) Window {
	return Window{}
}

// Tick advances the window clock, reclaiming expired charges.
func (g *GraphqlCostGuard) Tick(now time.Time) int {
	return 0
}

// CacheStats reports analysis cache hits and misses.
func (g *GraphqlCostGuard) CacheStats() CacheStats {
	return CacheStats{}
}

// SetIntrospection turns introspection queries on or off for one tenant.
func (g *GraphqlCostGuard) SetIntrospection(tenantID string, enabled bool) error {
	return errNotImplemented
}

// Snapshot serialises tenant windows, the introspection switches and counters.
func (g *GraphqlCostGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the guard state, all or nothing.
func (g *GraphqlCostGuard) Restore(data []byte) error {
	return errNotImplemented
}
