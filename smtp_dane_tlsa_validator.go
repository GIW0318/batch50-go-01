// Package task evaluates outbound SMTP DANE obligations, TLSA matching and
// per-MX fallback for a mail relay.
package task

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// TLSA certificate usage values.
const (
	UsagePkixTA = 0
	UsagePkixEE = 1
	UsageDaneTA = 2
	UsageDaneEE = 3
)

// TLSA selector values.
const (
	SelectorFullCert = 0
	SelectorSPKI     = 1
)

// TLSA matching type values.
const (
	MatchExact  = 0
	MatchSHA256 = 1
	MatchSHA512 = 2
)

// Delivery modes reported by a verdict.
const (
	ModeDane     = "dane_secure"
	ModePlain    = "opportunistic_plain"
	ModeFallback = "fallback_next_mx"
	ModeReject   = "reject_delivery"
)

// Verdict causes.
const (
	CauseTlsaMismatch = "tlsa_mismatch"
	CauseTlsaInvalid  = "tlsa_record_invalid"
	CauseNoUsableTlsa = "no_usable_tlsa"
	CauseCertExpired  = "cert_expired"
	CauseAdAbsent     = "ad_flag_absent"
	CauseNoTlsa       = "no_tlsa_records"
	CauseMxExhausted  = "mx_list_exhausted"
	CauseAttemptsOut  = "mx_attempts_exhausted"
)

const snapshotVersion = 1

// Errors surfaced by the validator.
var (
	ErrBlankDomain = errors.New("recipient domain is required")
	ErrNoMx        = errors.New("domain has no mail exchanger")
	ErrMxLimit     = errors.New("mx list exceeds the configured cap")
	ErrLookup      = errors.New("dns lookup failed")
	ErrUnknownMx   = errors.New("mx host is not covered by a cached policy")
	ErrPolicyGone  = errors.New("policy is neither cached nor fresh")
	ErrEmptyChain  = errors.New("certificate chain is empty")
	ErrBadCause    = errors.New("fallback cause is required")
	ErrBadSnapshot = errors.New("snapshot payload is invalid")
)

// Cert is one certificate as presented during the SMTP TLS handshake.
type Cert struct {
	Subject  string
	DER      []byte
	SPKI     []byte
	NotAfter time.Time
	Trusted  bool
}

// TlsaRecord is one TLSA resource record.
type TlsaRecord struct {
	Usage        int
	Selector     int
	MatchingType int
	Data         []byte
}

// TlsaAnswer is the TLSA RRset for one MX host together with the resolver
// authenticated-data flag.
type TlsaAnswer struct {
	AD   bool
	TTL  time.Duration
	Tlsa []TlsaRecord
}

// MxHost is one mail exchanger of a recipient domain.
type MxHost struct {
	Host     string
	Priority int
}

// DanePolicy is the cached delivery policy of one recipient domain.
type DanePolicy struct {
	Domain   string
	AD       bool
	Mx       []MxHost
	Tlsa     map[string]TlsaAnswer
	Expires  time.Time
	Required bool
}

// Verdict is the decision for one MX of one delivery attempt.
type Verdict struct {
	MX      string
	Mode    string
	Secure  bool
	Cause   string
	Next    string
	Records int
	Matched int
}

// DaneStats counts lookup and verdict activity.
type DaneStats struct {
	Policies  int
	Lookups   int
	Verified  int
	Secure    int
	Rejected  int
	Fallbacks int
}

// DaneConfig bounds and wires the validator.
type DaneConfig struct {
	MaxMxPerDomain int
	MaxTlsaRecords int
	MaxAttempts    int
	PolicyTTL      time.Duration
	LookupMx       func(domain string) ([]MxHost, error)
	LookupTlsa     func(mxHost string) (TlsaAnswer, error)
	Now            func() time.Time
}

type inflightResolution struct {
	done   chan struct{}
	policy DanePolicy
	err    error
}

type snapshotEnvelope struct {
	Version  int                   `json:"version"`
	Policies map[string]DanePolicy `json:"policies"`
	Attempts map[string]int        `json:"attempts"`
	Stats    DaneStats             `json:"stats"`
}

// SmtpDaneTlsaValidator owns the policy cache and the per-MX verdicts.
type SmtpDaneTlsaValidator struct {
	cfg      DaneConfig
	mu       sync.Mutex
	policies map[string]DanePolicy
	attempts map[string]int
	stats    DaneStats
	inflight map[string]*inflightResolution
}

// NewSmtpDaneTlsaValidator builds a validator with the given bounds.
func NewSmtpDaneTlsaValidator(cfg DaneConfig) *SmtpDaneTlsaValidator {
	if cfg.PolicyTTL <= 0 {
		cfg.PolicyTTL = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SmtpDaneTlsaValidator{
		cfg:      cfg,
		policies: make(map[string]DanePolicy),
		attempts: make(map[string]int),
		inflight: make(map[string]*inflightResolution),
	}
}

// Policy resolves and caches the MX list and the per-MX TLSA answers of a
// recipient domain.
func (v *SmtpDaneTlsaValidator) Policy(domain string) (DanePolicy, error) {
	key, err := normalizeDomain(domain)
	if err != nil {
		return DanePolicy{}, err
	}
	return v.resolve(key, false)
}

// Verify checks one presented chain against the TLSA answer of one MX.
func (v *SmtpDaneTlsaValidator) Verify(mx string, chain []Cert) (Verdict, error) {
	if len(chain) == 0 {
		return Verdict{}, ErrEmptyChain
	}

	mxKey := normalizeHost(mx)
	v.mu.Lock()
	answer, err := v.answerForMXLocked(mxKey)
	v.mu.Unlock()
	if err != nil {
		return Verdict{}, err
	}

	now := v.cfg.Now()
	for _, cert := range chain {
		if now.After(cert.NotAfter) {
			verdict := Verdict{MX: mxKey, Mode: ModeReject, Cause: CauseCertExpired, Records: len(answer.Tlsa)}
			v.recordVerdict(verdict)
			return verdict, nil
		}
	}

	verdict := Verdict{MX: mxKey, Records: len(answer.Tlsa)}
	if !answer.AD {
		verdict.Mode = ModePlain
		verdict.Cause = CauseAdAbsent
		v.recordVerdict(verdict)
		return verdict, nil
	}

	if len(answer.Tlsa) == 0 {
		verdict.Mode = ModePlain
		verdict.Cause = CauseNoTlsa
		v.recordVerdict(verdict)
		return verdict, nil
	}

	if !v.Classify(answer) {
		verdict.Mode = ModeReject
		verdict.Cause = CauseTlsaInvalid
		v.recordVerdict(verdict)
		return verdict, nil
	}

	matched := 0
	for _, record := range answer.Tlsa {
		if recordMatchesChain(record, chain) {
			matched++
		}
	}
	verdict.Matched = matched

	if matched == 0 {
		verdict.Mode = ModeReject
		verdict.Cause = CauseTlsaMismatch
	} else {
		verdict.Mode = ModeDane
		verdict.Secure = true
	}
	v.recordVerdict(verdict)
	return verdict, nil
}

// Fallback skips a failed MX and reports the next hop in preference order.
func (v *SmtpDaneTlsaValidator) Fallback(mx string, cause string) (Verdict, error) {
	if strings.TrimSpace(cause) == "" {
		return Verdict{}, ErrBadCause
	}

	mxKey := normalizeHost(mx)
	v.mu.Lock()
	defer v.mu.Unlock()

	policy, index, err := v.policyForMXLocked(mxKey)
	if err != nil {
		return Verdict{}, err
	}

	v.attempts[mxKey]++
	for nextIndex := index + 1; nextIndex < len(policy.Mx); nextIndex++ {
		next := policy.Mx[nextIndex].Host
		if v.attempts[next] > 0 {
			continue
		}
		v.stats.Fallbacks++
		return Verdict{MX: mxKey, Mode: ModeFallback, Next: next}, nil
	}

	v.stats.Rejected++
	return Verdict{MX: mxKey, Mode: ModeReject, Cause: CauseMxExhausted}, nil
}

// Refresh re-resolves a domain and replaces its cached policy.
func (v *SmtpDaneTlsaValidator) Refresh(domain string) error {
	key, err := normalizeDomain(domain)
	if err != nil {
		return err
	}
	_, err = v.resolve(key, true)
	return err
}

// Cached reads the policy of a domain without triggering a lookup.
func (v *SmtpDaneTlsaValidator) Cached(domain string) (DanePolicy, bool) {
	key, err := normalizeDomain(domain)
	if err != nil {
		return DanePolicy{}, false
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	policy, ok := v.policies[key]
	if !ok || !v.freshLocked(policy) {
		return DanePolicy{}, false
	}
	return clonePolicy(policy), true
}

// MXOrder reports the cached preference order of a domain.
func (v *SmtpDaneTlsaValidator) MXOrder(domain string) ([]MxHost, error) {
	key, err := normalizeDomain(domain)
	if err != nil {
		return nil, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	policy, ok := v.policies[key]
	if !ok {
		return nil, ErrPolicyGone
	}
	if !v.freshLocked(policy) {
		return nil, ErrPolicyGone
	}
	return append([]MxHost(nil), policy.Mx...), nil
}

// Classify reports whether a TLSA answer carries a usable record set.
func (v *SmtpDaneTlsaValidator) Classify(answer TlsaAnswer) bool {
	if len(answer.Tlsa) == 0 {
		return false
	}
	if v.cfg.MaxTlsaRecords > 0 && len(answer.Tlsa) > v.cfg.MaxTlsaRecords {
		return false
	}
	for _, record := range answer.Tlsa {
		if !validTlsaRecord(record) {
			return false
		}
	}
	return true
}

// Stats reports counters of the validator.
func (v *SmtpDaneTlsaValidator) Stats() DaneStats {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stats
}

// Snapshot serialises the policy cache, the failure marks and the counters.
func (v *SmtpDaneTlsaValidator) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Policies: make(map[string]DanePolicy, len(v.policies)),
		Attempts: make(map[string]int, len(v.attempts)),
		Stats:    v.stats,
	}
	for domain, policy := range v.policies {
		envelope.Policies[domain] = clonePolicy(policy)
	}
	for mx, attempts := range v.attempts {
		envelope.Attempts[mx] = attempts
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (v *SmtpDaneTlsaValidator) Restore(data []byte) error {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	next, err := envelope.validated()
	if err != nil {
		return err
	}

	v.mu.Lock()
	v.policies = next.Policies
	v.attempts = next.Attempts
	v.stats = next.Stats
	v.mu.Unlock()
	return nil
}

func (v *SmtpDaneTlsaValidator) resolve(domain string, force bool) (DanePolicy, error) {
	v.mu.Lock()
	if flight, ok := v.inflight[domain]; ok {
		v.mu.Unlock()
		<-flight.done
		if flight.err != nil {
			return DanePolicy{}, flight.err
		}
		return clonePolicy(flight.policy), nil
	}

	if !force {
		if policy, ok := v.freshPolicyLocked(domain); ok {
			out := clonePolicy(policy)
			v.mu.Unlock()
			return out, nil
		}
	}

	flight := &inflightResolution{done: make(chan struct{})}
	v.inflight[domain] = flight
	v.mu.Unlock()

	policy, err := v.buildPolicy(domain)

	v.mu.Lock()
	delete(v.inflight, domain)
	if err != nil {
		flight.err = err
		close(flight.done)
		v.mu.Unlock()
		return DanePolicy{}, err
	}

	stored := clonePolicy(policy)
	v.policies[domain] = stored
	v.stats.Policies++
	flight.policy = clonePolicy(stored)
	close(flight.done)
	v.mu.Unlock()

	return clonePolicy(stored), nil
}

func (v *SmtpDaneTlsaValidator) buildPolicy(domain string) (DanePolicy, error) {
	if v.cfg.LookupMx == nil {
		return DanePolicy{}, fmt.Errorf("%w: mx resolver is not configured", ErrLookup)
	}

	v.trackLookup()
	rawMX, err := v.cfg.LookupMx(domain)
	if err != nil {
		return DanePolicy{}, fmt.Errorf("%w: mx lookup for %s: %v", ErrLookup, domain, err)
	}
	if len(rawMX) == 0 {
		return DanePolicy{}, ErrNoMx
	}

	mxHosts := normalizeMXHosts(rawMX)
	if len(mxHosts) == 0 {
		return DanePolicy{}, ErrNoMx
	}
	if v.cfg.MaxMxPerDomain > 0 && len(mxHosts) > v.cfg.MaxMxPerDomain {
		return DanePolicy{}, ErrMxLimit
	}

	policy := DanePolicy{
		Domain:  domain,
		Mx:      mxHosts,
		Tlsa:    make(map[string]TlsaAnswer, len(mxHosts)),
		Expires: v.cfg.Now().Add(v.cfg.PolicyTTL),
	}

	if v.cfg.LookupTlsa == nil {
		return DanePolicy{}, fmt.Errorf("%w: tlsa resolver is not configured", ErrLookup)
	}

	for _, mx := range mxHosts {
		v.trackLookup()
		answer, lookupErr := v.cfg.LookupTlsa(mx.Host)
		if lookupErr != nil {
			return DanePolicy{}, fmt.Errorf("%w: tlsa lookup for %s: %v", ErrLookup, mx.Host, lookupErr)
		}
		answer = cloneAnswer(answer)
		policy.Tlsa[mx.Host] = answer
		if answer.AD {
			policy.AD = true
			if len(answer.Tlsa) > 0 {
				policy.Required = true
			}
		}
	}

	return policy, nil
}

func (v *SmtpDaneTlsaValidator) trackLookup() {
	v.mu.Lock()
	v.stats.Lookups++
	v.mu.Unlock()
}

func (v *SmtpDaneTlsaValidator) recordVerdict(verdict Verdict) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stats.Verified++
	if verdict.Secure {
		v.stats.Secure++
	}
	if verdict.Mode == ModeReject {
		v.stats.Rejected++
	}
}

func (v *SmtpDaneTlsaValidator) freshLocked(policy DanePolicy) bool {
	return v.cfg.Now().Before(policy.Expires)
}

func (v *SmtpDaneTlsaValidator) freshPolicyLocked(domain string) (DanePolicy, bool) {
	policy, ok := v.policies[domain]
	if !ok || !v.freshLocked(policy) {
		return DanePolicy{}, false
	}
	return policy, true
}

func (v *SmtpDaneTlsaValidator) answerForMXLocked(mx string) (TlsaAnswer, error) {
	policy, _, err := v.policyForMXLocked(mx)
	if err != nil {
		return TlsaAnswer{}, err
	}
	return cloneAnswer(policy.Tlsa[mx]), nil
}

func (v *SmtpDaneTlsaValidator) policyForMXLocked(mx string) (DanePolicy, int, error) {
	domains := make([]string, 0, len(v.policies))
	for domain := range v.policies {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	stale := false
	for _, domain := range domains {
		policy := v.policies[domain]
		for index, candidate := range policy.Mx {
			if candidate.Host != mx {
				continue
			}
			if v.freshLocked(policy) {
				return policy, index, nil
			}
			stale = true
		}
	}
	if stale {
		return DanePolicy{}, 0, ErrPolicyGone
	}
	return DanePolicy{}, 0, ErrUnknownMx
}

func normalizeDomain(domain string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(domain))
	if normalized == "" {
		return "", ErrBlankDomain
	}
	return normalized, nil
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

func normalizeMXHosts(in []MxHost) []MxHost {
	hosts := make([]MxHost, 0, len(in))
	for _, host := range in {
		normalized := normalizeHost(host.Host)
		if normalized == "" {
			continue
		}
		hosts = append(hosts, MxHost{Host: normalized, Priority: host.Priority})
	}

	sort.SliceStable(hosts, func(i, j int) bool {
		if hosts[i].Priority != hosts[j].Priority {
			return hosts[i].Priority < hosts[j].Priority
		}
		return hosts[i].Host < hosts[j].Host
	})

	unique := make([]MxHost, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		if _, ok := seen[host.Host]; ok {
			continue
		}
		seen[host.Host] = struct{}{}
		unique = append(unique, host)
	}
	return unique
}

func validTlsaRecord(record TlsaRecord) bool {
	if record.Usage < UsagePkixTA || record.Usage > UsageDaneEE {
		return false
	}
	if record.Selector < SelectorFullCert || record.Selector > SelectorSPKI {
		return false
	}
	if record.MatchingType < MatchExact || record.MatchingType > MatchSHA512 {
		return false
	}
	return len(record.Data) > 0
}

func recordMatchesChain(record TlsaRecord, chain []Cert) bool {
	for _, cert := range chain {
		var selected []byte
		switch record.Selector {
		case SelectorFullCert:
			selected = cert.DER
		case SelectorSPKI:
			selected = cert.SPKI
		default:
			return false
		}

		var expected []byte
		switch record.MatchingType {
		case MatchExact:
			expected = selected
		case MatchSHA256:
			digest := sha256.Sum256(selected)
			expected = digest[:]
		case MatchSHA512:
			digest := sha512.Sum512(selected)
			expected = digest[:]
		default:
			return false
		}

		if bytes.Equal(record.Data, expected) {
			return true
		}
	}
	return false
}

func clonePolicy(in DanePolicy) DanePolicy {
	out := in
	out.Mx = append([]MxHost(nil), in.Mx...)
	if in.Tlsa != nil {
		out.Tlsa = make(map[string]TlsaAnswer, len(in.Tlsa))
		for host, answer := range in.Tlsa {
			out.Tlsa[host] = cloneAnswer(answer)
		}
	}
	return out
}

func cloneAnswer(in TlsaAnswer) TlsaAnswer {
	out := in
	if in.Tlsa != nil {
		out.Tlsa = make([]TlsaRecord, len(in.Tlsa))
		for index, record := range in.Tlsa {
			out.Tlsa[index] = record
			out.Tlsa[index].Data = append([]byte(nil), record.Data...)
		}
	}
	return out
}

func (envelope snapshotEnvelope) validated() (snapshotEnvelope, error) {
	if envelope.Version != snapshotVersion {
		return snapshotEnvelope{}, fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, envelope.Version)
	}
	if envelope.Policies == nil || envelope.Attempts == nil {
		return snapshotEnvelope{}, fmt.Errorf("%w: missing state", ErrBadSnapshot)
	}
	if envelope.Stats.Policies < 0 || envelope.Stats.Lookups < 0 || envelope.Stats.Verified < 0 ||
		envelope.Stats.Secure < 0 || envelope.Stats.Rejected < 0 || envelope.Stats.Fallbacks < 0 {
		return snapshotEnvelope{}, fmt.Errorf("%w: negative counter", ErrBadSnapshot)
	}

	next := snapshotEnvelope{
		Version:  snapshotVersion,
		Policies: make(map[string]DanePolicy, len(envelope.Policies)),
		Attempts: make(map[string]int, len(envelope.Attempts)),
		Stats:    envelope.Stats,
	}

	seenDomains := make(map[string]struct{}, len(envelope.Policies))
	for rawDomain, policy := range envelope.Policies {
		domain, err := normalizeDomain(rawDomain)
		if err != nil {
			return snapshotEnvelope{}, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
		if _, exists := seenDomains[domain]; exists {
			return snapshotEnvelope{}, fmt.Errorf("%w: duplicate domain %s", ErrBadSnapshot, domain)
		}
		seenDomains[domain] = struct{}{}

		policy.Domain = domain
		policy.Mx = normalizeMXHosts(policy.Mx)
		if policy.Tlsa == nil {
			policy.Tlsa = make(map[string]TlsaAnswer)
		}
		answers := make(map[string]TlsaAnswer, len(policy.Tlsa))
		for rawHost, answer := range policy.Tlsa {
			host := normalizeHost(rawHost)
			if host == "" {
				return snapshotEnvelope{}, fmt.Errorf("%w: blank TLSA host", ErrBadSnapshot)
			}
			answers[host] = cloneAnswer(answer)
		}
		for _, mx := range policy.Mx {
			if _, ok := answers[mx.Host]; !ok {
				answers[mx.Host] = TlsaAnswer{}
			}
		}
		policy.Tlsa = answers
		if policy.Expires.IsZero() {
			return snapshotEnvelope{}, fmt.Errorf("%w: missing expiry", ErrBadSnapshot)
		}
		next.Policies[domain] = clonePolicy(policy)
	}

	for rawHost, attempts := range envelope.Attempts {
		host := normalizeHost(rawHost)
		if host == "" || attempts < 0 {
			return snapshotEnvelope{}, fmt.Errorf("%w: invalid attempt marker", ErrBadSnapshot)
		}
		next.Attempts[host] += attempts
	}

	return next, nil
}
