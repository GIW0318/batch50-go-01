// Package task resolves HTTPS and SVCB records into usable connection
// parameters, following alias chains and enforcing mandatory keys.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	cfg      ResolverConfig
	mu       sync.Mutex
	cache    map[string]Chain
	inflight map[string]*resolveCall
	stats    ResolverStats
}

type resolveCall struct {
	done  chan struct{}
	chain Chain
	err   error
}

// NewSvcbAliasResolver builds a resolver with the given bounds.
func NewSvcbAliasResolver(cfg ResolverConfig) *SvcbAliasResolver {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SvcbAliasResolver{
		cfg:      cfg,
		cache:    make(map[string]Chain),
		inflight: make(map[string]*resolveCall),
	}
}

// Resolve walks the alias chain of a name and caches the resulting RRset.
func (r *SvcbAliasResolver) Resolve(name string) (Chain, error) {
	return r.resolve(name, false)
}

// Refresh re-resolves a name and replaces its cached chain.
func (r *SvcbAliasResolver) Refresh(name string) (Chain, error) {
	return r.resolve(name, true)
}

// Params merges the winning service records into one parameter set.
func (r *SvcbAliasResolver) Params(chain Chain) (Params, error) {
	if chain.Mode != ModeService || len(chain.Records) == 0 {
		return Params{}, ErrNoServiceRecord
	}

	group := winningRecordGroup(chain.Records)
	if len(group) == 0 {
		return Params{}, ErrNoServiceRecord
	}

	params := Params{
		Name:        firstNonEmpty(group[0].Name, chain.Name),
		Port:        DefaultHTTPSPort,
		RecordCount: len(group),
	}
	params.Target = effectiveTarget(group[0].Name, group[0].Target)

	merged := make(map[string]Param)
	for _, rr := range group {
		if err := validateRecord(rr); err != nil {
			return Params{}, err
		}
		for _, param := range rr.Params {
			if existing, ok := merged[param.Key]; ok {
				if existing.Value != param.Value {
					return Params{}, ErrParamConflict
				}
				continue
			}
			merged[param.Key] = param
		}
	}

	if param, ok := merged[KeyALPN]; ok {
		params.ALPN = dedupePreserve(splitCSV(param.Value))
	}
	if param, ok := merged[KeyPort]; ok {
		port, err := strconv.Atoi(strings.TrimSpace(param.Value))
		if err != nil || port < 1 || port > 65535 {
			return Params{}, ErrBadPort
		}
		params.Port = port
	}
	if param, ok := merged[KeyMandatory]; ok {
		params.Mandatory = splitCSV(param.Value)
	}
	_, params.NoDefaultALPN = merged[KeyNoDefaultALPN]
	if param, ok := merged[KeyECH]; ok {
		params.ECH = param.Value
	}

	_, hasIPv4Hint := merged[KeyIPv4Hint]
	_, hasIPv6Hint := merged[KeyIPv6Hint]
	if param, ok := merged[KeyIPv4Hint]; ok {
		hints, err := parseHints(param.Value, false, r.cfg.MaxHints)
		if err != nil {
			return Params{}, err
		}
		params.IPv4Hints = hints
	}
	if param, ok := merged[KeyIPv6Hint]; ok {
		hints, err := parseHints(param.Value, true, r.cfg.MaxHints)
		if err != nil {
			return Params{}, err
		}
		params.IPv6Hints = hints
	}

	if (!hasIPv4Hint || !hasIPv6Hint) && r.cfg.HintLookup != nil {
		addresses, err := r.cfg.HintLookup(params.Target)
		if err != nil {
			return Params{}, fmt.Errorf("%w: %w", ErrLookup, err)
		}
		for _, address := range addresses {
			address = strings.TrimSpace(address)
			ip := net.ParseIP(address)
			if ip == nil {
				return Params{}, ErrBadHint
			}
			if ip.To4() != nil {
				if hasIPv4Hint {
					continue
				}
				params.IPv4Hints = appendUnique(params.IPv4Hints, address)
			} else {
				if hasIPv6Hint {
					continue
				}
				params.IPv6Hints = appendUnique(params.IPv6Hints, address)
			}
		}
		if len(params.IPv4Hints) > r.cfg.MaxHints || len(params.IPv6Hints) > r.cfg.MaxHints {
			return Params{}, ErrHintLimit
		}
	}

	return params, nil
}

// Validate checks one record on its own.
func (r *SvcbAliasResolver) Validate(rr Record) error {
	return validateRecord(rr)
}

// Cached reads a chain without triggering a lookup.
func (r *SvcbAliasResolver) Cached(name string) (Chain, bool) {
	key := strings.TrimSpace(name)
	if key == "" {
		return Chain{}, false
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	chain, ok := r.cache[key]
	if !ok {
		return Chain{}, false
	}
	if !now.Before(chain.Expires) {
		delete(r.cache, key)
		return Chain{}, false
	}
	return cloneChain(chain), true
}

// Flush drops one cached chain and reports whether it was present.
func (r *SvcbAliasResolver) Flush(name string) bool {
	key := strings.TrimSpace(name)
	if key == "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	_, ok := r.cache[key]
	if ok {
		delete(r.cache, key)
	}
	return ok
}

// Stats reports counters of the resolver.
func (r *SvcbAliasResolver) Stats() ResolverStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// Snapshot serialises the chain cache and the counters.
func (r *SvcbAliasResolver) Snapshot() ([]byte, error) {
	r.mu.Lock()
	payload := snapshotPayload{
		Cache: make(map[string]Chain, len(r.cache)),
		Stats: r.stats,
	}
	for name, chain := range r.cache {
		payload.Cache[name] = cloneChain(chain)
	}
	r.mu.Unlock()

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(raw)
	envelope := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Payload:  raw,
		Checksum: hex.EncodeToString(checksum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole cache with a snapshot, all or nothing.
func (r *SvcbAliasResolver) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Magic != snapshotMagic || envelope.Version != snapshotVersion ||
		len(envelope.Payload) == 0 || envelope.Checksum == "" {
		return ErrBadSnapshot
	}

	checksum := sha256.Sum256(envelope.Payload)
	if hex.EncodeToString(checksum[:]) != envelope.Checksum {
		return ErrBadSnapshot
	}

	var payload snapshotPayload
	decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return ErrBadSnapshot
	}
	if decoder.More() {
		return ErrBadSnapshot
	}

	cache := make(map[string]Chain)
	for name, chain := range payload.Cache {
		cache[name] = cloneChain(chain)
	}

	r.mu.Lock()
	r.cache = cache
	r.stats = payload.Stats
	r.mu.Unlock()
	return nil
}

const (
	snapshotMagic   = "gsb-svcb-alias-resolver-v1"
	snapshotVersion = 1
)

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type snapshotPayload struct {
	Cache map[string]Chain `json:"cache"`
	Stats ResolverStats    `json:"stats"`
}

func (r *SvcbAliasResolver) resolve(name string, force bool) (Chain, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		r.mu.Lock()
		r.stats.Rejected++
		r.mu.Unlock()
		return Chain{}, ErrBlankName
	}
	now := r.now()

	r.mu.Lock()
	if r.cache == nil {
		r.cache = make(map[string]Chain)
	}
	if r.inflight == nil {
		r.inflight = make(map[string]*resolveCall)
	}

	if !force {
		if chain, ok := r.cache[key]; ok && now.Before(chain.Expires) {
			r.stats.CacheHits++
			r.mu.Unlock()
			return cloneChain(chain), nil
		}
	}

	if call, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		<-call.done
		return cloneChain(call.chain), call.err
	}

	call := &resolveCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.mu.Unlock()

	chain, err := r.buildChain(key)
	if err == nil {
		chain = cloneChain(chain)
	}

	r.mu.Lock()
	if current, ok := r.inflight[key]; ok && current == call {
		delete(r.inflight, key)
	}
	if err == nil {
		r.cache[key] = cloneChain(chain)
		r.stats.Resolved++
		r.stats.AliasHops += chain.Depth
	} else {
		r.stats.Rejected++
	}
	call.chain = chain
	call.err = err
	r.mu.Unlock()

	close(call.done)
	return cloneChain(chain), err
}

func (r *SvcbAliasResolver) buildChain(name string) (Chain, error) {
	current := name
	visited := map[string]struct{}{current: {}}
	aliases := make([]string, 0)
	depth := 0

	for {
		records, err := r.lookup(current)
		if err != nil {
			return Chain{}, err
		}
		if len(records) == 0 {
			return Chain{}, ErrNoRecord
		}
		if len(records) > r.cfg.MaxRecords {
			return Chain{}, ErrRecordLimit
		}

		mode, aliasRecord, serviceRecords, err := classifyAnswer(records)
		if err != nil {
			return Chain{}, err
		}

		if mode == ModeService {
			return Chain{
				Name:    name,
				Mode:    ModeService,
				Records: serviceRecords,
				Aliases: aliases,
				Depth:   depth,
				Expires: r.now().Add(r.cfg.ChainTTL),
			}, nil
		}

		target := strings.TrimSpace(aliasRecord.Target)
		if _, seen := visited[target]; seen {
			return Chain{}, ErrAliasLoop
		}
		if depth+1 > r.cfg.MaxAliasDepth {
			return Chain{}, ErrAliasDepth
		}

		depth++
		aliases = append(aliases, target)
		visited[target] = struct{}{}
		current = target
	}
}

func (r *SvcbAliasResolver) lookup(name string) ([]Record, error) {
	if r.cfg.Lookup == nil {
		return nil, fmt.Errorf("%w: no lookup function", ErrLookup)
	}
	records, err := r.cfg.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLookup, err)
	}
	return records, nil
}

func (r *SvcbAliasResolver) now() time.Time {
	if r.cfg.Now != nil {
		return r.cfg.Now()
	}
	return time.Now()
}

func classifyAnswer(records []Record) (string, Record, []Record, error) {
	aliasCount := 0
	var aliasRecord Record
	for _, rr := range records {
		if rr.Priority < 0 || rr.Priority > 65535 {
			return "", Record{}, nil, ErrBadPriority
		}
		if rr.Priority == 0 {
			aliasCount++
			aliasRecord = rr
		}
	}

	if aliasCount > 1 || (aliasCount == 1 && len(records) > 1) {
		return "", Record{}, nil, ErrMixedMode
	}

	if aliasCount == 1 {
		target := strings.TrimSpace(aliasRecord.Target)
		if target == "" || target == "." {
			return "", Record{}, nil, ErrBadTarget
		}
		if len(aliasRecord.Params) > 0 {
			return "", Record{}, nil, ErrAliasParams
		}
		return ModeAlias, aliasRecord, nil, nil
	}

	serviceRecords := append([]Record(nil), records...)
	for _, rr := range serviceRecords {
		if strings.TrimSpace(rr.Target) == "" {
			return "", Record{}, nil, ErrBadTarget
		}
	}
	sort.SliceStable(serviceRecords, func(i, j int) bool {
		if serviceRecords[i].Priority != serviceRecords[j].Priority {
			return serviceRecords[i].Priority < serviceRecords[j].Priority
		}
		return serviceRecords[i].Target < serviceRecords[j].Target
	})
	return ModeService, Record{}, serviceRecords, nil
}

func winningRecordGroup(records []Record) []Record {
	minimum := 0
	found := false
	for _, rr := range records {
		if rr.Priority <= 0 {
			continue
		}
		if !found || rr.Priority < minimum {
			minimum = rr.Priority
			found = true
		}
	}
	if !found {
		return nil
	}

	group := make([]Record, 0)
	for _, rr := range records {
		if rr.Priority == minimum {
			group = append(group, rr)
		}
	}
	sort.SliceStable(group, func(i, j int) bool {
		return group[i].Target < group[j].Target
	})
	return group
}

func validateRecord(rr Record) error {
	if rr.Priority < 0 || rr.Priority > 65535 {
		return ErrBadPriority
	}

	target := strings.TrimSpace(rr.Target)
	if target == "" {
		return ErrBadTarget
	}
	if rr.Priority == 0 && target == "." {
		return ErrBadTarget
	}
	if rr.Priority == 0 {
		if len(rr.Params) > 0 {
			return ErrAliasParams
		}
		return nil
	}

	values := make(map[string]Param)
	for _, param := range rr.Params {
		if _, exists := values[param.Key]; exists {
			return ErrDuplicateParam
		}
		values[param.Key] = param
	}

	if param, ok := values[KeyPort]; ok && !validPortValue(param.Value) {
		return ErrBadPort
	}

	if param, ok := values[KeyMandatory]; ok {
		for _, key := range splitCSV(param.Value) {
			if !knownParamKey(key) {
				return ErrUnknownMandatory
			}
			if _, exists := values[key]; !exists {
				return ErrMandatoryMissing
			}
		}
	}

	if _, ok := values[KeyNoDefaultALPN]; ok {
		if _, hasALPN := values[KeyALPN]; !hasALPN {
			return ErrParamConflict
		}
	}

	if param, ok := values[KeyIPv4Hint]; ok {
		if err := validateHintList(param.Value, false); err != nil {
			return err
		}
	}
	if param, ok := values[KeyIPv6Hint]; ok {
		if err := validateHintList(param.Value, true); err != nil {
			return err
		}
	}

	return nil
}

func knownParamKey(key string) bool {
	switch key {
	case KeyALPN, KeyPort, KeyIPv4Hint, KeyIPv6Hint,
		KeyMandatory, KeyNoDefaultALPN, KeyECH:
		return true
	default:
		return false
	}
}

func validPortValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	port, err := strconv.Atoi(value)
	return err == nil && port >= 1 && port <= 65535
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func dedupePreserve(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func validateHintList(raw string, wantV6 bool) error {
	for _, value := range splitCSV(raw) {
		ip := net.ParseIP(value)
		if ip == nil {
			return ErrBadHint
		}
		isV4 := ip.To4() != nil
		if wantV6 && isV4 {
			return ErrBadHint
		}
		if !wantV6 && !isV4 {
			return ErrBadHint
		}
	}
	return nil
}

func parseHints(raw string, wantV6 bool, maxHints int) ([]string, error) {
	if err := validateHintList(raw, wantV6); err != nil {
		return nil, err
	}

	hints := dedupePreserve(splitCSV(raw))
	if len(hints) > maxHints {
		return nil, ErrHintLimit
	}
	return hints, nil
}

func effectiveTarget(owner, target string) string {
	target = strings.TrimSpace(target)
	if target == "." || target == "" {
		return strings.TrimSpace(owner)
	}
	return target
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneChain(chain Chain) Chain {
	result := chain
	if chain.Records != nil {
		result.Records = make([]Record, len(chain.Records))
		for i, record := range chain.Records {
			result.Records[i] = record
			if record.Params != nil {
				result.Records[i].Params = append([]Param(nil), record.Params...)
			}
		}
	}
	if chain.Aliases != nil {
		result.Aliases = append([]string(nil), chain.Aliases...)
	}
	return result
}
