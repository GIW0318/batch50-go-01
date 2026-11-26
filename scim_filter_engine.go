// Package task implements the query front end used by the SCIM 2.0 user and
// group list endpoints: filter parsing, schema aware evaluation, deterministic
// ordering and cursor based paging.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Comparison operators the filter language understands.
const (
	OpEq = "eq"
	OpNe = "ne"
	OpCo = "co"
	OpSw = "sw"
	OpEw = "ew"
	OpPr = "pr"
	OpGt = "gt"
	OpGe = "ge"
	OpLt = "lt"
	OpLe = "le"
)

// ValueKind tells the evaluator how a value must be compared.
type ValueKind string

// Value kinds supported by the engine.
const (
	KindString ValueKind = "string"
	KindNumber ValueKind = "number"
	KindBool   ValueKind = "bool"
)

// Errors reported by the engine.
var (
	ErrBadFilter       = errors.New("filter expression is malformed")
	ErrUnknownAttr     = errors.New("attribute path is not registered")
	ErrDuplicateSchema = errors.New("attribute schema is already registered")
	ErrUnknownKind     = errors.New("resource kind is unknown")
	ErrDuplicateID     = errors.New("resource id already exists")
	ErrNotFound        = errors.New("resource not found")
	ErrCursorInvalid   = errors.New("cursor is malformed or was altered")
	ErrCursorMismatch  = errors.New("cursor was issued for another expression")
	ErrResultLimit     = errors.New("matched resources exceed the configured limit")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Value is one attribute value. Multi valued attributes hold several of them.
type Value struct {
	Kind ValueKind
	Str  string
	Num  float64
	Bool bool
}

// AttrSchema declares one attribute path of the directory schema. Sub is the
// sub attribute name of a complex attribute and stays empty for top level
// attributes. CaseExact decides whether string comparison is case sensitive.
type AttrSchema struct {
	Path      string
	Sub       string
	CaseExact bool
	Kind      ValueKind
}

// AttrRef is one attribute reference returned by Explain.
type AttrRef struct {
	Path string
	Sub  string
}

// Resource is one directory object. Attrs maps an attribute path to all of its
// values, so single valued attributes simply carry one element.
type Resource struct {
	ID    string
	Kind  string
	Attrs map[string][]Value
}

// Limits bounds the compiled filter cache, the page size and the result set.
type Limits struct {
	MaxCacheEntries int
	MaxPageSize     int
	MaxResults      int
}

// QueryRequest is one list request as it arrives from the SCIM endpoint.
type QueryRequest struct {
	Filter     string
	SortBy     string
	Descending bool
	PageSize   int
	Cursor     string
}

// QueryPage is one page of a list response.
type QueryPage struct {
	Resources   []Resource
	NextCursor  string
	Total       int
	Fingerprint string
}

// CacheStats reports how the compiled filter cache is doing.
type CacheStats struct {
	Entries int
	Hits    int
	Misses  int
}

// Expr is a parsed filter expression.
type Expr struct {
	text string
}

// ScimFilterEngine owns the schema registry, the resource table and the
// compiled filter cache.
type ScimFilterEngine struct {
	limits Limits
}

// NewScimFilterEngine builds an engine with the given caps.
func NewScimFilterEngine(limits Limits) *ScimFilterEngine {
	return &ScimFilterEngine{limits: limits}
}

// RegisterSchema adds one attribute path to the directory schema.
func (e *ScimFilterEngine) RegisterSchema(s AttrSchema) error {
	return errNotImplemented
}

// Parse compiles one filter string into a reusable expression.
func (e *ScimFilterEngine) Parse(filter string) (Expr, error) {
	return Expr{}, errNotImplemented
}

// Match evaluates an expression against one resource.
func (e *ScimFilterEngine) Match(expr Expr, res Resource) (bool, error) {
	return false, errNotImplemented
}

// Query filters, orders and pages the resources of one kind.
func (e *ScimFilterEngine) Query(kind string, req QueryRequest) (QueryPage, error) {
	return QueryPage{}, errNotImplemented
}

// Explain lists the attribute references an expression touches, sorted and
// deduplicated.
func (e *ScimFilterEngine) Explain(expr Expr) []AttrRef {
	return nil
}

// Fingerprint returns the stable identity of a compiled expression.
func (e *ScimFilterEngine) Fingerprint(expr Expr) string {
	return ""
}

// Put stores or replaces one resource.
func (e *ScimFilterEngine) Put(res Resource) error {
	return errNotImplemented
}

// Get reads one resource by kind and id.
func (e *ScimFilterEngine) Get(kind, id string) (Resource, bool) {
	return Resource{}, false
}

// Delete removes one resource and leaves a tombstone behind.
func (e *ScimFilterEngine) Delete(kind, id string) error {
	return errNotImplemented
}

// Snapshot serialises the schema, the resources, the tombstones and the cache
// fingerprints.
func (e *ScimFilterEngine) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (e *ScimFilterEngine) Restore(data []byte) error {
	return errNotImplemented
}

// CacheStats reports the compiled expression cache.
func (e *ScimFilterEngine) CacheStats() CacheStats {
	return CacheStats{}
}
