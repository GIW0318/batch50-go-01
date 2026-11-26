// Package task orchestrates controlled outbound probes for SSRF validation.
// Every redirect hop is judged on its own resolved address before the next
// request is handed to the operator.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Supported probe schemes.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// Step kinds handed out by Next.
const (
	KindPrimary  = "primary"
	KindRedirect = "redirect"
)

// Probe lifecycle states.
const (
	StateQueued    = "queued"
	StateActive    = "active"
	StateBlocked   = "blocked"
	StateDone      = "done"
	StateExhausted = "exhausted"
	StateCancelled = "cancelled"
)

// Address families returned by ClassifyAddress.
const (
	FamilyPublic      = "public"
	FamilyLoopback    = "loopback"
	FamilyPrivate     = "private"
	FamilyLinkLocal   = "link_local"
	FamilyMulticast   = "multicast"
	FamilyUnspecified = "unspecified"
	FamilyInvalid     = "invalid"
)

// Reason codes attached to a verdict.
const (
	ReasonCompleted          = "reason_completed"
	ReasonBlockedLoopback    = "reason_blocked_loopback"
	ReasonBlockedPrivate     = "reason_blocked_private"
	ReasonBlockedLinkLocal   = "reason_blocked_link_local"
	ReasonBlockedMulticast   = "reason_blocked_multicast"
	ReasonBlockedUnspecified = "reason_blocked_unspecified"
	ReasonBlockedInvalid     = "reason_blocked_invalid"
	ReasonHopBudget          = "reason_hop_budget"
	ReasonTimeBudget         = "reason_time_budget"
	ReasonRequestBudget      = "reason_request_budget"
	ReasonCancelled          = "reason_cancelled"
	ReasonLookupFailed       = "reason_lookup_failed"
)

// Errors reported by the orchestrator.
var (
	ErrBlankTarget     = errors.New("target scheme, host and parameter are required")
	ErrBadScheme       = errors.New("only http and https targets are supported")
	ErrDuplicateProbe  = errors.New("an identical target is already in flight")
	ErrProbeLimit      = errors.New("concurrent probe limit reached")
	ErrUnknownProbe    = errors.New("unknown probe")
	ErrUnknownStep     = errors.New("unknown step")
	ErrStepPending     = errors.New("the current step has not been reported yet")
	ErrStepClosed      = errors.New("step already reported")
	ErrBlockedTarget   = errors.New("target resolves to a blocked address family")
	ErrBudgetExhausted = errors.New("probe budget exhausted")
	ErrBadRedirect     = errors.New("redirect location must be an absolute http or https url")
	ErrProbeCancelled  = errors.New("probe cancelled")
	ErrProbeClosed     = errors.New("probe has no runnable steps")
	ErrLookupFailed    = errors.New("host resolution failed")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// errNotImplemented was the scaffold stub sentinel; completed paths never return it.
var errNotImplemented = errors.New("not implemented")

const (
	stepStatusOpen      = "open"
	stepStatusClosed    = "closed"
	stepStatusCancelled = "cancelled"

	snapshotFormat = "ssrf-probe-orchestrator-v1"

	terminalLookup      = "lookup_failed"
	terminalBadRedirect = "bad_redirect"
)

// Target is one injection point candidate submitted for probing.
type Target struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Method string `json:"method"`
	Param  string `json:"param"`
}

// Options bounds a probe run and injects host resolution.
type Options struct {
	MaxConcurrentProbes int
	MaxHops             int
	TotalMillis         int
	MaxRequestsPerProbe int
	Lookup              func(host string) ([]string, error)
}

// BudgetView is the remaining budget snapshot attached to a step.
type BudgetView struct {
	HopsLeft   int `json:"hops_left"`
	MillisLeft int `json:"millis_left"`
	Requests   int `json:"requests"`
}

// Step is one outbound request the operator must execute and report back.
type Step struct {
	ID      string     `json:"id"`
	ProbeID string     `json:"probe_id"`
	Kind    string     `json:"kind"`
	Hop     int        `json:"hop"`
	URL     string     `json:"url"`
	Budget  BudgetView `json:"budget"`
}

// Observation is what the operator saw when executing one step.
type Observation struct {
	StepID        string `json:"step_id"`
	StatusCode    int    `json:"status_code"`
	Location      string `json:"location"`
	ElapsedMillis int    `json:"elapsed_millis"`
	Error         string `json:"error"`
}

// Verdict is the final, auditable outcome of one probe.
type Verdict struct {
	ProbeID   string   `json:"probe_id"`
	Blocked   bool     `json:"blocked"`
	Cancelled bool     `json:"cancelled"`
	Done      bool     `json:"done"`
	Reason    string   `json:"reason"`
	Hop       int      `json:"hop"`
	Evidence  []string `json:"evidence"`
	Requests  int      `json:"requests"`
}

// ProbeState is a read only view of one probe's bookkeeping.
type ProbeState struct {
	ProbeID       string `json:"probe_id"`
	Target        Target `json:"target"`
	State         string `json:"state"`
	Hop           int    `json:"hop"`
	Requests      int    `json:"requests"`
	ElapsedMillis int    `json:"elapsed_millis"`
	Reason        string `json:"reason"`
}

type probeRecord struct {
	ID            string   `json:"id"`
	Target        Target   `json:"target"`
	State         string   `json:"state"`
	Reason        string   `json:"reason,omitempty"`
	TerminalError string   `json:"terminal_error,omitempty"`
	Hop           int      `json:"hop"`
	NextHop       int      `json:"next_hop"`
	NextKind      string   `json:"next_kind"`
	NextURL       string   `json:"next_url"`
	ActiveStepID  string   `json:"active_step_id,omitempty"`
	Requests      int      `json:"requests"`
	ElapsedMillis int      `json:"elapsed_millis"`
	Audit         []string `json:"audit,omitempty"`
	Verdict       Verdict  `json:"verdict"`
	HasVerdict    bool     `json:"has_verdict"`
}

type stepRecord struct {
	Step              Step     `json:"step"`
	Status            string   `json:"status"`
	ResolvedAddresses []string `json:"resolved_addresses,omitempty"`
}

// SsrfProbeOrchestrator owns the probe table, the hop budgets and the verdicts.
type SsrfProbeOrchestrator struct {
	mu       sync.Mutex
	opts     Options
	probes   map[string]*probeRecord
	steps    map[string]*stepRecord
	probeSeq uint64
	stepSeq  uint64
}

type limitsSnapshot struct {
	MaxConcurrentProbes int `json:"max_concurrent_probes"`
	MaxHops             int `json:"max_hops"`
	TotalMillis         int `json:"total_millis"`
	MaxRequestsPerProbe int `json:"max_requests_per_probe"`
}

type snapshotData struct {
	Format   string                 `json:"format"`
	ProbeSeq uint64                 `json:"probe_seq"`
	StepSeq  uint64                 `json:"step_seq"`
	Limits   limitsSnapshot         `json:"limits"`
	Probes   map[string]probeRecord `json:"probes"`
	Steps    map[string]stepRecord  `json:"steps"`
}

// NewSsrfProbeOrchestrator builds an orchestrator with the given caps.
func NewSsrfProbeOrchestrator(opts Options) *SsrfProbeOrchestrator {
	if opts.Lookup == nil {
		opts.Lookup = literalLookup
	}
	return &SsrfProbeOrchestrator{
		opts:   opts,
		probes: make(map[string]*probeRecord),
		steps:  make(map[string]*stepRecord),
	}
}

// Submit registers one probe for a target and returns its identifier.
func (o *SsrfProbeOrchestrator) Submit(target Target) (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(target.Scheme))
	host := strings.TrimSpace(target.Host)
	param := strings.TrimSpace(target.Param)
	if host == "" || param == "" {
		return "", ErrBlankTarget
	}
	if scheme != SchemeHTTP && scheme != SchemeHTTPS {
		return "", ErrBadScheme
	}

	target.Scheme = scheme
	target.Host = host
	target.Param = param

	o.mu.Lock()
	defer o.mu.Unlock()

	active := 0
	for _, probe := range o.probes {
		if holdsSlot(probe.State) {
			active++
		}
	}
	if o.opts.MaxConcurrentProbes > 0 && active >= o.opts.MaxConcurrentProbes {
		return "", ErrProbeLimit
	}

	o.probeSeq++
	id := fmt.Sprintf("probe-%012d", o.probeSeq)
	probe := &probeRecord{
		ID:       id,
		Target:   target,
		State:    StateQueued,
		NextHop:  1,
		NextKind: KindPrimary,
		NextURL:  primaryURL(target),
	}
	o.probes[id] = probe
	return id, nil
}

// Next hands out the next step of a probe in chain order.
func (o *SsrfProbeOrchestrator) Next(probeID string) (Step, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	probe, ok := o.probes[probeID]
	if !ok {
		return Step{}, ErrUnknownProbe
	}

	switch probe.State {
	case StateCancelled:
		return Step{}, ErrProbeCancelled
	case StateBlocked:
		if probe.TerminalError == terminalBadRedirect {
			return Step{}, ErrBadRedirect
		}
		if probe.TerminalError == terminalLookup || probe.Reason == ReasonLookupFailed {
			return Step{}, ErrLookupFailed
		}
		return Step{}, ErrBlockedTarget
	case StateExhausted:
		return Step{}, ErrBudgetExhausted
	case StateDone:
		return Step{}, ErrProbeClosed
	}

	if probe.ActiveStepID != "" {
		record, exists := o.steps[probe.ActiveStepID]
		if !exists || record.Status != stepStatusOpen {
			probe.ActiveStepID = ""
		} else {
			return Step{}, ErrStepPending
		}
	}

	hop := probe.NextHop
	if hop < 1 {
		hop = 1
	}
	probe.Hop = hop

	if o.opts.MaxHops > 0 && hop > o.opts.MaxHops {
		return Step{}, o.exhaustLocked(probe, ReasonHopBudget, hop)
	}
	if o.opts.TotalMillis > 0 && probe.ElapsedMillis >= o.opts.TotalMillis {
		return Step{}, o.exhaustLocked(probe, ReasonTimeBudget, hop)
	}
	if o.opts.MaxRequestsPerProbe > 0 && probe.Requests >= o.opts.MaxRequestsPerProbe {
		return Step{}, o.exhaustLocked(probe, ReasonRequestBudget, hop)
	}

	host := hostFromURL(probe.NextURL)
	addresses, err := o.opts.Lookup(host)
	if err != nil {
		o.blockLookupLocked(probe, hop, host, err)
		return Step{}, ErrLookupFailed
	}
	if len(addresses) == 0 {
		o.blockLookupLocked(probe, hop, host, ErrLookupFailed)
		return Step{}, ErrLookupFailed
	}

	audit := []string{fmt.Sprintf("hop=%d", hop), "kind=" + probe.NextKind}
	blockedFamily := ""
	for _, address := range addresses {
		family := classifyAddress(address)
		address = strings.TrimSpace(address)
		audit = append(audit, "address="+address+" family="+family, address, family)
		if family != FamilyPublic && blockedFamily == "" {
			blockedFamily = family
		}
	}
	probe.Audit = append(probe.Audit, audit...)
	if blockedFamily != "" {
		reason := reasonForFamily(blockedFamily)
		probe.State = StateBlocked
		probe.Reason = reason
		probe.TerminalError = ""
		probe.Verdict = Verdict{
			ProbeID:  probe.ID,
			Blocked:  true,
			Reason:   reason,
			Hop:      hop,
			Evidence: cloneStrings(probe.Audit),
			Requests: probe.Requests,
		}
		probe.HasVerdict = true
		return Step{}, ErrBlockedTarget
	}

	o.stepSeq++
	stepID := fmt.Sprintf("step-%012d", o.stepSeq)
	probe.Requests++

	hopsLeft := 0
	if o.opts.MaxHops > 0 {
		hopsLeft = o.opts.MaxHops - hop + 1
	}
	millisLeft := 0
	if o.opts.TotalMillis > 0 {
		millisLeft = o.opts.TotalMillis - probe.ElapsedMillis
		if millisLeft < 0 {
			millisLeft = 0
		}
	}

	step := Step{
		ID:      stepID,
		ProbeID: probe.ID,
		Kind:    probe.NextKind,
		Hop:     hop,
		URL:     probe.NextURL,
		Budget: BudgetView{
			HopsLeft:   hopsLeft,
			MillisLeft: millisLeft,
			Requests:   probe.Requests,
		},
	}
	o.steps[stepID] = &stepRecord{
		Step:              step,
		Status:            stepStatusOpen,
		ResolvedAddresses: cloneStrings(addresses),
	}
	probe.ActiveStepID = stepID
	probe.State = StateActive
	return step, nil
}

// Report records the observation of one executed step.
func (o *SsrfProbeOrchestrator) Report(stepID string, obs Observation) error {
	if stepID == "" {
		stepID = obs.StepID
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	record, ok := o.steps[stepID]
	if !ok {
		return ErrUnknownStep
	}
	probe, ok := o.probes[record.Step.ProbeID]
	if !ok {
		return ErrUnknownStep
	}
	if record.Status != stepStatusOpen || probe.ActiveStepID != stepID {
		return ErrStepClosed
	}

	location := obs.Location
	isRedirect := isRedirectStatus(obs.StatusCode)
	validLocation := strings.TrimSpace(location) != "" && validAbsoluteHTTPURL(location)
	invalidRedirect := isRedirect && strings.TrimSpace(location) != "" && !validAbsoluteHTTPURL(location)
	hasRedirect := isRedirect && validLocation

	if obs.ElapsedMillis > 0 {
		probe.ElapsedMillis += obs.ElapsedMillis
	}
	record.Status = stepStatusClosed
	probe.ActiveStepID = ""

	if invalidRedirect {
		reason := ReasonBlockedInvalid
		probe.Audit = append(probe.Audit,
			"redirect="+strings.TrimSpace(location),
			FamilyInvalid,
			"reason="+reason,
		)
		probe.State = StateBlocked
		probe.Reason = reason
		probe.TerminalError = terminalBadRedirect
		probe.Verdict = Verdict{
			ProbeID:  probe.ID,
			Blocked:  true,
			Reason:   reason,
			Hop:      record.Step.Hop,
			Evidence: cloneStrings(probe.Audit),
			Requests: probe.Requests,
		}
		probe.HasVerdict = true
		return ErrBadRedirect
	}

	if hasRedirect {
		probe.NextHop = record.Step.Hop + 1
		probe.NextKind = KindRedirect
		probe.NextURL = location
		probe.State = StateActive
		return nil
	}

	probe.State = StateDone
	probe.Reason = ReasonCompleted
	probe.TerminalError = ""
	probe.Audit = append(probe.Audit, fmt.Sprintf("hop=%d result=completed", record.Step.Hop))
	probe.Verdict = Verdict{
		ProbeID:  probe.ID,
		Done:     true,
		Reason:   ReasonCompleted,
		Hop:      record.Step.Hop,
		Evidence: cloneStrings(probe.Audit),
		Requests: probe.Requests,
	}
	probe.HasVerdict = true
	return nil
}

// Verdict returns the final judgement of a probe.
func (o *SsrfProbeOrchestrator) Verdict(probeID string) (Verdict, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	probe, ok := o.probes[probeID]
	if !ok {
		return Verdict{}, ErrUnknownProbe
	}

	verdict := probe.Verdict
	if !probe.HasVerdict {
		verdict = Verdict{
			ProbeID:  probe.ID,
			Hop:      probe.Hop,
			Requests: probe.Requests,
		}
	}
	verdict.Evidence = cloneStrings(verdict.Evidence)
	return verdict, nil
}

// Cancel stops a probe and releases its concurrency slot.
func (o *SsrfProbeOrchestrator) Cancel(probeID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	probe, ok := o.probes[probeID]
	if !ok {
		return ErrUnknownProbe
	}
	if probe.State == StateCancelled || isTerminalState(probe.State) {
		return nil
	}

	if probe.ActiveStepID != "" {
		if record, ok := o.steps[probe.ActiveStepID]; ok {
			record.Status = stepStatusCancelled
		}
	}
	probe.ActiveStepID = ""
	probe.State = StateCancelled
	probe.Reason = ReasonCancelled
	probe.TerminalError = ""
	probe.Audit = append(probe.Audit, fmt.Sprintf("result=cancelled hop=%d", probe.Hop))
	probe.Verdict = Verdict{
		ProbeID:   probe.ID,
		Cancelled: true,
		Reason:    ReasonCancelled,
		Hop:       probe.Hop,
		Evidence:  cloneStrings(probe.Audit),
		Requests:  probe.Requests,
	}
	probe.HasVerdict = true
	return nil
}

// Probe reads one probe's bookkeeping.
func (o *SsrfProbeOrchestrator) Probe(probeID string) (ProbeState, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	probe, ok := o.probes[probeID]
	if !ok {
		return ProbeState{}, false
	}
	return ProbeState{
		ProbeID:       probe.ID,
		Target:        probe.Target,
		State:         probe.State,
		Hop:           probe.Hop,
		Requests:      probe.Requests,
		ElapsedMillis: probe.ElapsedMillis,
		Reason:        probe.Reason,
	}, true
}

// Pending lists the probes that still hold a concurrency slot, ordered by id.
func (o *SsrfProbeOrchestrator) Pending() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	ids := make([]string, 0)
	for id, probe := range o.probes {
		if holdsSlot(probe.State) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// ClassifyAddress maps one literal address to its address family.
func (o *SsrfProbeOrchestrator) ClassifyAddress(addr string) string {
	return classifyAddress(addr)
}

// Snapshot serialises the probe table, chain positions and budgets.
func (o *SsrfProbeOrchestrator) Snapshot() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	data := snapshotData{
		Format:   snapshotFormat,
		ProbeSeq: o.probeSeq,
		StepSeq:  o.stepSeq,
		Limits: limitsSnapshot{
			MaxConcurrentProbes: o.opts.MaxConcurrentProbes,
			MaxHops:             o.opts.MaxHops,
			TotalMillis:         o.opts.TotalMillis,
			MaxRequestsPerProbe: o.opts.MaxRequestsPerProbe,
		},
		Probes: make(map[string]probeRecord, len(o.probes)),
		Steps:  make(map[string]stepRecord, len(o.steps)),
	}
	for id, probe := range o.probes {
		data.Probes[id] = *probe
	}
	for id, record := range o.steps {
		data.Steps[id] = *record
	}

	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (o *SsrfProbeOrchestrator) Restore(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ErrBadSnapshot
	}

	var data snapshotData
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return ErrBadSnapshot
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrBadSnapshot
	}

	probes, steps, probeSeq, stepSeq, err := data.buildState()
	if err != nil {
		return ErrBadSnapshot
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	lookup := o.opts.Lookup
	o.opts = Options{
		MaxConcurrentProbes: data.Limits.MaxConcurrentProbes,
		MaxHops:             data.Limits.MaxHops,
		TotalMillis:         data.Limits.TotalMillis,
		MaxRequestsPerProbe: data.Limits.MaxRequestsPerProbe,
		Lookup:              lookup,
	}
	if o.opts.Lookup == nil {
		o.opts.Lookup = literalLookup
	}
	o.probes = probes
	o.steps = steps
	o.probeSeq = probeSeq
	o.stepSeq = stepSeq
	return nil
}

func (data snapshotData) buildState() (map[string]*probeRecord, map[string]*stepRecord, uint64, uint64, error) {
	if data.Format != snapshotFormat {
		return nil, nil, 0, 0, ErrBadSnapshot
	}

	probes := make(map[string]*probeRecord, len(data.Probes))
	steps := make(map[string]*stepRecord, len(data.Steps))

	for mapID, value := range data.Probes {
		record := value
		if mapID == "" || record.ID != mapID || !knownState(record.State) {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if record.NextHop < 1 || record.Hop < 0 || record.Requests < 0 || record.ElapsedMillis < 0 {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if record.NextKind != KindPrimary && record.NextKind != KindRedirect {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if strings.TrimSpace(record.NextURL) == "" {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if _, err := url.Parse(record.NextURL); err != nil {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if isTerminalState(record.State) != record.HasVerdict {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if record.HasVerdict {
			if record.Verdict.ProbeID != record.ID || strings.TrimSpace(record.Verdict.Reason) == "" {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		}
		if record.ActiveStepID != "" {
			step, ok := data.Steps[record.ActiveStepID]
			if !ok || step.Status != stepStatusOpen || step.Step.ID != record.ActiveStepID ||
				step.Step.ProbeID != record.ID {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		}

		switch record.State {
		case StateQueued:
			if record.ActiveStepID != "" || record.Hop != 0 || record.Requests != 0 {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		case StateCancelled:
			if !record.Verdict.Cancelled || record.Reason != ReasonCancelled {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		case StateDone:
			if !record.Verdict.Done || record.Reason != ReasonCompleted {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		case StateExhausted:
			if record.Reason != ReasonHopBudget && record.Reason != ReasonTimeBudget &&
				record.Reason != ReasonRequestBudget {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		case StateBlocked:
			if !record.Verdict.Blocked || record.Reason == "" {
				return nil, nil, 0, 0, ErrBadSnapshot
			}
		}
		probes[record.ID] = &record
	}

	for mapID, value := range data.Steps {
		record := value
		if mapID == "" || record.Step.ID != mapID {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if record.Status != stepStatusOpen && record.Status != stepStatusClosed &&
			record.Status != stepStatusCancelled {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		probe, ok := probes[record.Step.ProbeID]
		if !ok || record.Step.Hop < 1 {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if record.Status == stepStatusOpen && probe.ActiveStepID != record.Step.ID {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		if probe.ActiveStepID == record.Step.ID && record.Status != stepStatusOpen {
			return nil, nil, 0, 0, ErrBadSnapshot
		}
		steps[record.Step.ID] = &record
	}

	probeSeq := data.ProbeSeq
	stepSeq := data.StepSeq
	for id := range probes {
		if seq := sequenceFromID(id, "probe-"); seq > probeSeq {
			probeSeq = seq
		}
	}
	for id := range steps {
		if seq := sequenceFromID(id, "step-"); seq > stepSeq {
			stepSeq = seq
		}
	}
	return probes, steps, probeSeq, stepSeq, nil
}

func (o *SsrfProbeOrchestrator) exhaustLocked(probe *probeRecord, reason string, hop int) error {
	probe.Audit = append(probe.Audit, fmt.Sprintf(
		"budget_exhausted=%s attempted_hop=%d requests=%d elapsed_ms=%d",
		reason, hop, probe.Requests, probe.ElapsedMillis,
	))
	probe.State = StateExhausted
	probe.Reason = reason
	probe.Hop = hop
	probe.ActiveStepID = ""
	probe.Verdict = Verdict{
		ProbeID:  probe.ID,
		Reason:   reason,
		Hop:      hop,
		Evidence: cloneStrings(probe.Audit),
		Requests: probe.Requests,
	}
	probe.HasVerdict = true
	return ErrBudgetExhausted
}

func (o *SsrfProbeOrchestrator) blockLookupLocked(probe *probeRecord, hop int, host string, cause error) {
	probe.Audit = append(probe.Audit, "hop="+strconv.Itoa(hop), "host="+host)
	if cause != nil {
		probe.Audit = append(probe.Audit, "lookup_error="+cause.Error())
	}
	probe.Audit = append(probe.Audit, ReasonLookupFailed)
	probe.State = StateBlocked
	probe.Reason = ReasonLookupFailed
	probe.TerminalError = terminalLookup
	probe.Hop = hop
	probe.ActiveStepID = ""
	probe.Verdict = Verdict{
		ProbeID:  probe.ID,
		Blocked:  true,
		Reason:   ReasonLookupFailed,
		Hop:      hop,
		Evidence: cloneStrings(probe.Audit),
		Requests: probe.Requests,
	}
	probe.HasVerdict = true
}

func classifyAddress(value string) string {
	value = strings.TrimSpace(value)
	if host, port, err := net.SplitHostPort(value); err == nil && port != "" {
		value = host
	}
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return FamilyInvalid
	}
	addr = addr.Unmap()

	switch {
	case addr.IsUnspecified():
		return FamilyUnspecified
	case addr.IsLoopback():
		return FamilyLoopback
	case addr.IsLinkLocalUnicast():
		return FamilyLinkLocal
	case addr.IsMulticast():
		return FamilyMulticast
	case addr.IsPrivate():
		return FamilyPrivate
	default:
		return FamilyPublic
	}
}

func reasonForFamily(family string) string {
	switch family {
	case FamilyLoopback:
		return ReasonBlockedLoopback
	case FamilyPrivate:
		return ReasonBlockedPrivate
	case FamilyLinkLocal:
		return ReasonBlockedLinkLocal
	case FamilyMulticast:
		return ReasonBlockedMulticast
	case FamilyUnspecified:
		return ReasonBlockedUnspecified
	default:
		return ReasonBlockedInvalid
	}
}

func literalLookup(host string) ([]string, error) {
	trimmed := strings.TrimSpace(host)
	trimmed = strings.TrimPrefix(trimmed, "[")
	trimmed = strings.TrimSuffix(trimmed, "]")
	if ip := net.ParseIP(trimmed); ip != nil {
		return []string{ip.String()}, nil
	}
	return nil, ErrLookupFailed
}

func primaryURL(target Target) string {
	host := strings.TrimSpace(target.Host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	authority := host
	if target.Port > 0 && !isDefaultPort(target.Scheme, target.Port) {
		authority = net.JoinHostPort(host, strconv.Itoa(target.Port))
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	param := strings.TrimLeft(strings.TrimSpace(target.Param), "/")
	return target.Scheme + "://" + authority + "/" + param
}

func isDefaultPort(scheme string, port int) bool {
	return (scheme == SchemeHTTP && port == 80) || (scheme == SchemeHTTPS && port == 443)
}

func hostFromURL(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return raw
}

func validAbsoluteHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == SchemeHTTP || scheme == SchemeHTTPS
}

func isRedirectStatus(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	default:
		return false
	}
}

func holdsSlot(state string) bool {
	return state == StateQueued || state == StateActive
}

func isTerminalState(state string) bool {
	switch state {
	case StateBlocked, StateDone, StateExhausted, StateCancelled:
		return true
	default:
		return false
	}
}

func knownState(state string) bool {
	return holdsSlot(state) || isTerminalState(state)
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func sequenceFromID(id, prefix string) uint64 {
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	sequence, err := strconv.ParseUint(id[len(prefix):], 10, 64)
	if err != nil {
		return 0
	}
	return sequence
}
