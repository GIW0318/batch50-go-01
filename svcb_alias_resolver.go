// Package task resolves HTTPS and SVCB records into usable connection
// parameters, following alias chains and enforcing mandatory keys.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// RRset modes.
const (
	ModeAlias   = "alias"
	ModeService = "service"
)

// Service parameter keys understood by the resolver.
const (
	KeyALPN          = "alpn"
	KeyPort          = "port"
	KeyIPv4Hint      = "ipv4hint"
	KeyIPv6Hint      = "ipv6hint"
	KeyMandatory     = "mandatory"
	KeyNoDefaultALPN = "no-default-alpn"
	KeyECH           = "ech"
)

// DefaultHTTPSPort is used when the port parameter is absent.
const DefaultHTTPSPort = 443

// Errors surfaced by the resolver.
var (
	ErrBlankName        = errors.New("name is required")
	ErrNoRecord         = errors.New("answer carries no record")
	ErrLookup           = errors.New("dns lookup failed")
	ErrRecordLimit      = errors.New("answer exceeds the record cap")
	ErrMixedMode        = errors.New("alias and service records are mixed")
	ErrBadPriority      = errors.New("priority is out of range")
	ErrBadTarget        = errors.New("target is not usable")
	ErrAliasLoop        = errors.New("alias chain loops")
	ErrAliasDepth       = errors.New("alias chain exceeds the depth cap")
	ErrAliasParams      = errors.New("alias record carries service parameters")
	ErrNoUsableRecord   = errors.New("no usable record in the answer")
	ErrNoServiceRecord  = errors.New("chain carries no service record")
	ErrDuplicateParam   = errors.New("duplicate service parameter key")
	ErrUnknownMandatory = errors.New("mandatory lists a key this resolver does not understand")
	ErrMandatoryMissing = errors.New("mandatory lists a key absent from the record")
	ErrParamConflict    = errors.New("service parameters conflict")
	ErrBadPort          = errors.New("port parameter is not usable")
	ErrBadHint          = errors.New("address hint is not usable")
	ErrHintLimit        = errors.New("address hints exceed the configured cap")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Param is one service parameter of a record.
type Param struct {
	Key   string
	Value string
}

// Record is one HTTPS or SVCB record.
type Record struct {
	Name     string
	Priority int
	Target   string
	Params   []Param
	TTL      time.Duration
}

// Chain is the resolved alias chain ending in one service RRset.
type Chain struct {
	Name    string
	Mode    string
	Records []Record
	Aliases []string
	Depth   int
	Expires time.Time
}

// Params is the merged connection parameter set of a chain.
type Params struct {
	Name          string
	Target        string
	ALPN          []string
	Port          int
	IPv4Hints     []string
	IPv6Hints     []string
	Mandatory     []string
	NoDefaultALPN bool
	ECH           string
	RecordCount   int
}

// ResolverConfig bounds and wires the resolver.
type ResolverConfig struct {
	MaxAliasDepth int
	MaxHints      int
	MaxRecords    int
	ChainTTL      time.Duration
	Lookup        func(name string) ([]Record, error)
	HintLookup    func(name string) ([]string, error)
	Now           func() time.Time
}

// ResolverStats counts resolution activity.
type ResolverStats struct {
	Resolved  int
	AliasHops int
	CacheHits int
	Rejected  int
}

// SvcbAliasResolver owns the chain cache and the parameter merge rules.
type SvcbAliasResolver struct {
	cfg ResolverConfig
	mu  sync.Mutex
}

// NewSvcbAliasResolver builds a resolver with the given bounds.
func NewSvcbAliasResolver(cfg ResolverConfig) *SvcbAliasResolver {
	return &SvcbAliasResolver{cfg: cfg}
}

// Resolve walks the alias chain of a name and caches the resulting RRset.
func (r *SvcbAliasResolver) Resolve(name string) (Chain, error) {
	return Chain{}, errNotImplemented
}

// Params merges the winning service records into one parameter set.
func (r *SvcbAliasResolver) Params(chain Chain) (Params, error) {
	return Params{}, errNotImplemented
}

// Validate checks one record on its own.
func (r *SvcbAliasResolver) Validate(rr Record) error {
	return errNotImplemented
}

// Cached reads a chain without triggering a lookup.
func (r *SvcbAliasResolver) Cached(name string) (Chain, bool) {
	return Chain{}, false
}

// Refresh re-resolves a name and replaces its cached chain.
func (r *SvcbAliasResolver) Refresh(name string) (Chain, error) {
	return Chain{}, errNotImplemented
}

// Flush drops one cached chain and reports whether it was present.
func (r *SvcbAliasResolver) Flush(name string) bool {
	return false
}

// Stats reports counters of the resolver.
func (r *SvcbAliasResolver) Stats() ResolverStats {
	return ResolverStats{}
}

// Snapshot serialises the chain cache and the counters.
func (r *SvcbAliasResolver) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole cache with a snapshot, all or nothing.
func (r *SvcbAliasResolver) Restore(data []byte) error {
	return errNotImplemented
}
