// Package task plans response header injection probes, tracks their
// lifecycle and judges the returned responses.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Payload encodings tried for every injection point.
const (
	VariantCRLF          = "crlf"
	VariantLF            = "lf"
	VariantDoublePercent = "double_percent"
	VariantUTF16         = "utf16"
)

// Sinks a payload can travel through.
const (
	SinkQuery  = "query"
	SinkHeader = "header"
)

// Probe locations reported by Judge.
const (
	LocationHeader = "header"
	LocationBody   = "body"
	LocationNone   = "none"
)

// Reason codes attached to a missed case.
const (
	ReasonFiltered     = "reason_filtered"
	ReasonNormalized   = "reason_normalized"
	ReasonNoReflection = "reason_no_reflection"
)

// ProbeHeader is the response header name a successful injection creates.
const ProbeHeader = "x-gsb-probe"

// Cache entry states retained in the prober state.
const (
	CachePending = "pending"
	CacheActive  = "active"
	CachePurged  = "purged"
)

// Errors returned by the prober.
var (
	ErrBlankPoint           = errors.New("injection point id, path, param and header are required")
	ErrPointLimit           = errors.New("injection point limit reached")
	ErrCaseLimit            = errors.New("per point case budget is below the required variant set")
	ErrUnknownCase          = errors.New("unknown case")
	ErrCaseNotCrafted       = errors.New("case has no crafted request")
	ErrCaseAlreadySubmitted = errors.New("case already has a submitted response")
	ErrNotSubmitted         = errors.New("case has no submitted response")
	ErrCaseCancelled        = errors.New("case was cleaned up")
	ErrRateLimit            = errors.New("request rate limit reached")
	ErrBadSnapshot          = errors.New("snapshot payload is invalid")
)

const (
	requiredCaseCount = 8
	snapshotVersion   = 1
	defaultWindow     = time.Minute
	maxDecodeDepth    = 6
	probeValuePrefix  = "probe-case-"
	cacheKeyPrefix    = "gsb-header-probe:"
)

// HeaderPoint is one request field that may reach a response header.
type HeaderPoint struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Param  string `json:"param"`
	Header string `json:"header"`
}

// Case is one crafted payload for one sink of an injection point.
type Case struct {
	ID      string `json:"id"`
	PointID string `json:"point_id"`
	Variant string `json:"variant"`
	Payload string `json:"payload"`
	Sink    string `json:"sink"`
}

// CaseSet is the planned plan for one injection point.
type CaseSet struct {
	PointID string `json:"point_id"`
	Cases   []Case `json:"cases"`
}

// Request is the concrete request built for one case.
type Request struct {
	CaseID  string            `json:"case_id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Target  string            `json:"target"`
	Headers map[string]string `json:"headers"`
}

// Header is one parsed response header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Response is what came back for one case.
type Response struct {
	CaseID  string   `json:"case_id"`
	Status  int      `json:"status"`
	Raw     []byte   `json:"raw"`
	Headers []Header `json:"headers"`
	Body    string   `json:"body"`
}

// Outcome is the judged result of one case.
type Outcome struct {
	CaseID   string `json:"case_id"`
	Hit      bool   `json:"hit"`
	Location string `json:"location"`
	Reason   string `json:"reason"`
	Header   string `json:"header"`
	Value    string `json:"value"`
}

// Limits bounds the plan size and the request rate.
type Limits struct {
	MaxPoints            int
	MaxCasesPerPoint     int
	MaxRequestsPerWindow int
	Window               time.Duration
}

// CacheEntry records the cache key owned by one probe case.
type CacheEntry struct {
	CaseID   string `json:"case_id"`
	CacheKey string `json:"cache_key"`
	Status   string `json:"status"`
}

type pointState struct {
	point   HeaderPoint
	caseIDs []string
}

type caseState struct {
	data        Case
	crafted     bool
	submitted   bool
	response    Response
	cancelled   bool
	cacheKey    string
	cacheStatus string
}

// HeaderInjectionProber owns the case table and the request rate window.
type HeaderInjectionProber struct {
	mu       sync.Mutex
	limits   Limits
	now      func() time.Time
	points   map[string]*pointState
	cases    map[string]*caseState
	requests []time.Time
	nextCase uint64
}

// NewHeaderInjectionProber builds a prober with the given caps.
func NewHeaderInjectionProber(limits Limits) *HeaderInjectionProber {
	if limits.Window <= 0 {
		limits.Window = defaultWindow
	}
	return &HeaderInjectionProber{
		limits:   limits,
		now:      time.Now,
		points:   make(map[string]*pointState),
		cases:    make(map[string]*caseState),
		nextCase: 1,
	}
}

// SetClock replaces the clock used for the request rate window.
func (p *HeaderInjectionProber) SetClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now != nil {
		p.now = now
	} else {
		p.now = time.Now
	}
}

// Plan builds the fixed variant set for one injection point.
func (p *HeaderInjectionProber) Plan(point HeaderPoint) (CaseSet, error) {
	if strings.TrimSpace(point.ID) == "" ||
		strings.TrimSpace(point.Path) == "" ||
		strings.TrimSpace(point.Param) == "" ||
		strings.TrimSpace(point.Header) == "" {
		return CaseSet{}, ErrBlankPoint
	}
	if p.limits.MaxCasesPerPoint < requiredCaseCount {
		return CaseSet{}, ErrCaseLimit
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.points[point.ID]; ok {
		return p.caseSetLocked(existing), nil
	}
	if len(p.points) >= p.limits.MaxPoints {
		return CaseSet{}, ErrPointLimit
	}

	planned := &pointState{point: point}
	set := CaseSet{PointID: point.ID, Cases: make([]Case, 0, requiredCaseCount)}
	variants := []string{VariantCRLF, VariantLF, VariantDoublePercent, VariantUTF16}
	sinks := []string{SinkQuery, SinkHeader}

	for _, variant := range variants {
		for _, sink := range sinks {
			caseID := fmt.Sprintf("case-%d", p.nextCase)
			p.nextCase++
			c := Case{
				ID:      caseID,
				PointID: point.ID,
				Variant: variant,
				Sink:    sink,
				Payload: payloadFor(variant, caseID),
			}
			p.cases[caseID] = &caseState{
				data:        c,
				cacheKey:    cacheKeyFor(caseID),
				cacheStatus: CachePending,
			}
			planned.caseIDs = append(planned.caseIDs, caseID)
			set.Cases = append(set.Cases, c)
		}
	}
	p.points[point.ID] = planned
	return set, nil
}

// Craft builds the request for one case and charges the rate window.
func (p *HeaderInjectionProber) Craft(caseID string) (Request, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs, ok := p.cases[caseID]
	if !ok {
		return Request{}, ErrUnknownCase
	}
	if cs.cancelled {
		return Request{}, ErrCaseCancelled
	}

	ps := p.points[cs.data.PointID]
	request := buildRequest(cs.data, ps.point)
	now := p.now()
	p.pruneRateWindowLocked(now)

	if p.limits.MaxRequestsPerWindow <= 0 || len(p.requests) >= p.limits.MaxRequestsPerWindow {
		return Request{}, ErrRateLimit
	}

	p.requests = append(p.requests, now)
	cs.crafted = true
	cs.cacheStatus = CacheActive
	return request, nil
}

// Submit stores the response of a crafted case.
func (p *HeaderInjectionProber) Submit(caseID string, resp Response) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs, ok := p.cases[caseID]
	if !ok {
		return ErrUnknownCase
	}
	if cs.cancelled {
		return ErrCaseCancelled
	}
	if !cs.crafted {
		return ErrCaseNotCrafted
	}
	if cs.submitted {
		return ErrCaseAlreadySubmitted
	}

	stored := cloneResponse(resp)
	stored.CaseID = caseID
	cs.response = stored
	cs.submitted = true
	return nil
}

// Judge decides whether the submitted response carries the injected header.
func (p *HeaderInjectionProber) Judge(caseID string) (Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs, ok := p.cases[caseID]
	if !ok {
		return Outcome{}, ErrUnknownCase
	}
	if cs.cancelled {
		return Outcome{}, ErrCaseCancelled
	}
	if !cs.submitted {
		return Outcome{}, ErrNotSubmitted
	}
	return judgeOutcome(cs), nil
}

// Cleanup cancels a case and any cache entry the probe created.
func (p *HeaderInjectionProber) Cleanup(caseID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs, ok := p.cases[caseID]
	if !ok {
		return ErrUnknownCase
	}
	cs.cancelled = true
	cs.cacheStatus = CachePurged
	return nil
}

// Cases lists the cases of one point in planning order.
func (p *HeaderInjectionProber) Cases(pointID string) []Case {
	p.mu.Lock()
	defer p.mu.Unlock()

	ps, ok := p.points[pointID]
	if !ok {
		return nil
	}
	return p.caseSetLocked(ps).Cases
}

// Progress reports submitted and planned case counts of one point.
func (p *HeaderInjectionProber) Progress(pointID string) (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ps, ok := p.points[pointID]
	if !ok {
		return 0, 0
	}
	submitted := 0
	for _, caseID := range ps.caseIDs {
		if cs := p.cases[caseID]; cs != nil && cs.submitted {
			submitted++
		}
	}
	return submitted, len(ps.caseIDs)
}

// Points lists the planned injection points in ascending order.
func (p *HeaderInjectionProber) Points() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	points := make([]string, 0, len(p.points))
	for id := range p.points {
		points = append(points, id)
	}
	sort.Strings(points)
	return points
}

// CacheKey returns the cache key tracked for a case.
func (p *HeaderInjectionProber) CacheKey(caseID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs, ok := p.cases[caseID]
	if !ok {
		return "", ErrUnknownCase
	}
	return cs.cacheKey, nil
}

// CacheEntries lists case-to-cache-key relationships in planning order.
func (p *HeaderInjectionProber) CacheEntries() []CacheEntry {
	p.mu.Lock()
	defer p.mu.Unlock()

	entries := make([]CacheEntry, 0, len(p.cases))
	for _, pointID := range p.pointsLockedSorted() {
		for _, caseID := range p.points[pointID].caseIDs {
			cs := p.cases[caseID]
			entries = append(entries, CacheEntry{
				CaseID:   caseID,
				CacheKey: cs.cacheKey,
				Status:   cs.cacheStatus,
			})
		}
	}
	return entries
}

// CacheMapping returns a copy of the case-id to cache-key index.
func (p *HeaderInjectionProber) CacheMapping() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()

	mapping := make(map[string]string, len(p.cases))
	for caseID, cs := range p.cases {
		mapping[caseID] = cs.cacheKey
	}
	return mapping
}

type snapshotData struct {
	Version     int               `json:"version"`
	NextCase    uint64            `json:"next_case"`
	Points      []snapshotPoint   `json:"points"`
	Cases       []snapshotCase    `json:"cases"`
	RateWindow  []time.Time       `json:"rate_window"`
	CacheKeys   map[string]string `json:"cache_keys"`
	CacheIndex  map[string]string `json:"cache_index"`
	CacheStatus map[string]string `json:"cache_status"`
}

type snapshotPoint struct {
	Point   HeaderPoint `json:"point"`
	CaseIDs []string    `json:"case_ids"`
}

type snapshotCase struct {
	Case        Case      `json:"case"`
	Submitted   bool      `json:"submitted"`
	Response    *Response `json:"response,omitempty"`
	Cancelled   bool      `json:"cancelled"`
	CacheKey    string    `json:"cache_key"`
	CacheStatus string    `json:"cache_status"`
}

// Snapshot serialises points, cases, responses and the rate window.
func (p *HeaderInjectionProber) Snapshot() ([]byte, error) {
	p.mu.Lock()
	doc := p.snapshotLocked()
	p.mu.Unlock()

	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *HeaderInjectionProber) Restore(data []byte) error {
	var doc snapshotData
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	points, cases, rateWindow, nextCase, err := buildSnapshotState(&doc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	p.mu.Lock()
	p.points = points
	p.cases = cases
	p.requests = rateWindow
	p.nextCase = nextCase
	p.mu.Unlock()
	return nil
}

func (p *HeaderInjectionProber) caseSetLocked(ps *pointState) CaseSet {
	set := CaseSet{PointID: ps.point.ID, Cases: make([]Case, 0, len(ps.caseIDs))}
	for _, caseID := range ps.caseIDs {
		set.Cases = append(set.Cases, p.cases[caseID].data)
	}
	return set
}

func (p *HeaderInjectionProber) pointsLockedSorted() []string {
	ids := make([]string, 0, len(p.points))
	for id := range p.points {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (p *HeaderInjectionProber) pruneRateWindowLocked(now time.Time) {
	window := p.limits.Window
	if window <= 0 {
		window = defaultWindow
	}
	kept := make([]time.Time, 0, len(p.requests))
	for _, requestedAt := range p.requests {
		if now.Sub(requestedAt) < window {
			kept = append(kept, requestedAt)
		}
	}
	p.requests = kept
}

func (p *HeaderInjectionProber) snapshotLocked() snapshotData {
	doc := snapshotData{
		Version:     snapshotVersion,
		NextCase:    p.nextCase,
		Points:      make([]snapshotPoint, 0, len(p.points)),
		Cases:       make([]snapshotCase, 0, len(p.cases)),
		RateWindow:  append([]time.Time(nil), p.requests...),
		CacheKeys:   make(map[string]string, len(p.cases)),
		CacheIndex:  make(map[string]string, len(p.cases)),
		CacheStatus: make(map[string]string, len(p.cases)),
	}

	for _, pointID := range p.pointsLockedSorted() {
		ps := p.points[pointID]
		doc.Points = append(doc.Points, snapshotPoint{
			Point:   ps.point,
			CaseIDs: append([]string(nil), ps.caseIDs...),
		})
		for _, caseID := range ps.caseIDs {
			cs := p.cases[caseID]
			item := snapshotCase{
				Case:        cs.data,
				Submitted:   cs.submitted,
				Cancelled:   cs.cancelled,
				CacheKey:    cs.cacheKey,
				CacheStatus: cs.cacheStatus,
			}
			if cs.submitted {
				response := cloneResponse(cs.response)
				item.Response = &response
			}
			doc.Cases = append(doc.Cases, item)
			doc.CacheKeys[caseID] = cs.cacheKey
			doc.CacheIndex[cs.cacheKey] = caseID
			doc.CacheStatus[caseID] = cs.cacheStatus
		}
	}
	return doc
}

func buildSnapshotState(doc *snapshotData) (map[string]*pointState, map[string]*caseState, []time.Time, uint64, error) {
	if doc.Version != snapshotVersion {
		return nil, nil, nil, 0, errors.New("unsupported snapshot version")
	}
	if doc.NextCase == 0 {
		return nil, nil, nil, 0, errors.New("next case sequence is required")
	}

	points := make(map[string]*pointState, len(doc.Points))
	for _, item := range doc.Points {
		point := item.Point
		if strings.TrimSpace(point.ID) == "" || strings.TrimSpace(point.Path) == "" ||
			strings.TrimSpace(point.Param) == "" || strings.TrimSpace(point.Header) == "" {
			return nil, nil, nil, 0, errors.New("snapshot contains a blank injection point")
		}
		if _, exists := points[point.ID]; exists {
			return nil, nil, nil, 0, fmt.Errorf("duplicate point %q", point.ID)
		}
		if len(item.CaseIDs) != requiredCaseCount {
			return nil, nil, nil, 0, fmt.Errorf("point %q has %d cases", point.ID, len(item.CaseIDs))
		}
		points[point.ID] = &pointState{
			point:   point,
			caseIDs: append([]string(nil), item.CaseIDs...),
		}
	}

	cases := make(map[string]*caseState, len(doc.Cases))
	listed := make(map[string]map[string]bool, len(doc.Points))
	for id := range points {
		listed[id] = make(map[string]bool, requiredCaseCount)
	}
	nextCase := doc.NextCase

	for _, item := range doc.Cases {
		c := item.Case
		if c.ID == "" || c.PointID == "" || c.Payload == "" {
			return nil, nil, nil, 0, errors.New("snapshot contains an incomplete case")
		}
		if !validVariant(c.Variant) || !validSink(c.Sink) {
			return nil, nil, nil, 0, fmt.Errorf("case %q has an invalid variant or sink", c.ID)
		}
		if _, ok := points[c.PointID]; !ok {
			return nil, nil, nil, 0, fmt.Errorf("case %q references unknown point %q", c.ID, c.PointID)
		}
		if _, duplicate := cases[c.ID]; duplicate {
			return nil, nil, nil, 0, fmt.Errorf("duplicate case %q", c.ID)
		}

		ps := points[c.PointID]
		found := false
		for _, plannedID := range ps.caseIDs {
			if plannedID == c.ID {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, nil, 0, fmt.Errorf("case %q is not in point order", c.ID)
		}
		if listed[c.PointID][c.ID] {
			return nil, nil, nil, 0, fmt.Errorf("case %q is listed twice", c.ID)
		}
		listed[c.PointID][c.ID] = true

		cacheKey := item.CacheKey
		if cacheKey == "" {
			cacheKey = cacheKeyFor(c.ID)
		}
		cacheStatus := item.CacheStatus
		if cacheStatus == "" {
			if item.Cancelled {
				cacheStatus = CachePurged
			} else {
				cacheStatus = CachePending
			}
		}

		cs := &caseState{
			data:        c,
			submitted:   item.Submitted,
			cancelled:   item.Cancelled,
			cacheKey:    cacheKey,
			cacheStatus: cacheStatus,
		}
		if item.Submitted {
			if item.Response == nil {
				return nil, nil, nil, 0, fmt.Errorf("submitted case %q has no response", c.ID)
			}
			cs.crafted = true
			cs.response = cloneResponse(*item.Response)
			cs.response.CaseID = c.ID
		}
		cases[c.ID] = cs

		trimmed := strings.TrimPrefix(c.ID, "case-")
		if sequence, err := strconv.ParseUint(trimmed, 10, 64); err == nil && sequence >= nextCase {
			nextCase = sequence + 1
		}
	}

	for pointID, ids := range listed {
		if len(ids) != len(points[pointID].caseIDs) {
			return nil, nil, nil, 0, fmt.Errorf("point %q case order does not match its cases", pointID)
		}
	}

	rateWindow := make([]time.Time, 0, len(doc.RateWindow))
	for _, requestedAt := range doc.RateWindow {
		if requestedAt.IsZero() {
			return nil, nil, nil, 0, errors.New("rate window contains a zero timestamp")
		}
		rateWindow = append(rateWindow, requestedAt)
	}
	return points, cases, rateWindow, nextCase, nil
}

func validVariant(variant string) bool {
	switch variant {
	case VariantCRLF, VariantLF, VariantDoublePercent, VariantUTF16:
		return true
	default:
		return false
	}
}

func validSink(sink string) bool {
	return sink == SinkQuery || sink == SinkHeader
}

func payloadFor(variant, caseID string) string {
	value := probeValuePrefix + strings.TrimPrefix(caseID, "case-")
	switch variant {
	case VariantCRLF:
		return "\r\n" + ProbeHeader + ": " + value + "\r\n"
	case VariantLF:
		return "\n" + ProbeHeader + ": " + value + "\n"
	case VariantDoublePercent:
		return "%250d%250a" + ProbeHeader + "%253A%2520" + value + "%250d%250a"
	case VariantUTF16:
		return "%u000d%u000a" + ProbeHeader + "%u003A%u0020" + value + "%u000d%u000a"
	default:
		return ProbeHeader + ": " + value
	}
}

func cacheKeyFor(caseID string) string {
	return cacheKeyPrefix + caseID
}

func buildRequest(c Case, point HeaderPoint) Request {
	headers := map[string]string{"X-Probe-Case": c.ID}
	target := point.Path

	switch c.Sink {
	case SinkQuery:
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		target += separator + url.QueryEscape(point.Param) + "=" + queryPayload(c)
	case SinkHeader:
		headers[point.Header] = c.Payload
	}

	return Request{
		CaseID:  c.ID,
		Method:  point.Method,
		Path:    point.Path,
		Target:  target,
		Headers: headers,
	}
}

func queryPayload(c Case) string {
	switch c.Variant {
	case VariantDoublePercent, VariantUTF16:
		return c.Payload
	default:
		return url.QueryEscape(c.Payload)
	}
}

func cloneResponse(resp Response) Response {
	clone := resp
	if resp.Raw != nil {
		clone.Raw = append([]byte(nil), resp.Raw...)
	}
	if resp.Headers != nil {
		clone.Headers = append([]Header(nil), resp.Headers...)
	}
	return clone
}

func judgeOutcome(cs *caseState) Outcome {
	resp := cs.response
	outcome := Outcome{CaseID: cs.data.ID, Location: LocationNone}

	if header, ok := findNamedHeader(resp.Headers, ProbeHeader); ok {
		return headerHit(outcome, header.Name, header.Value)
	}

	for _, text := range responseTextCandidates(resp.Raw) {
		if name, value, ok := findProbeHeaderInMessage(text); ok {
			return headerHit(outcome, name, value)
		}
	}

	for _, header := range resp.Headers {
		seed := header.Name + ": " + header.Value
		for _, text := range decodingLayers(seed) {
			if name, value, ok := parseProbeHeaderBlock(text); ok {
				return headerHit(outcome, name, value)
			}
		}
	}

	return classifyMiss(cs)
}

func headerHit(base Outcome, name, value string) Outcome {
	base.Hit = true
	base.Location = LocationHeader
	base.Reason = ""
	base.Header = name
	base.Value = value
	return base
}

func findNamedHeader(headers []Header, name string) (Header, bool) {
	for _, header := range headers {
		if strings.EqualFold(strings.TrimSpace(header.Name), name) {
			return header, true
		}
	}
	return Header{}, false
}

func responseTextCandidates(raw []byte) []string {
	seen := make(map[string]bool)
	candidates := make([]string, 0)
	add := func(text string) {
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		candidates = append(candidates, text)
	}

	add(string(raw))
	for _, text := range utf16Candidates(raw) {
		add(text)
	}

	seeds := append([]string(nil), candidates...)
	for _, seed := range seeds {
		current := seed
		for depth := 0; depth < maxDecodeDepth; depth++ {
			add(current)
			decoded := decodeEncodedText(current)
			if decoded == current {
				break
			}
			current = decoded
		}
	}
	return candidates
}

func decodingLayers(seed string) []string {
	layers := []string{seed}
	current := seed
	for depth := 0; depth < maxDecodeDepth; depth++ {
		decoded := decodeEncodedText(current)
		if decoded == current {
			break
		}
		layers = append(layers, decoded)
		current = decoded
	}
	return layers
}

func findProbeHeaderInMessage(message string) (string, string, bool) {
	head, _ := splitHTTPMessage(message)
	return parseProbeHeaderBlock(head)
}

func splitHTTPMessage(message string) (string, string) {
	for _, separator := range []string{"\r\n\r\n", "\n\n"} {
		if index := strings.Index(message, separator); index >= 0 {
			return message[:index], message[index+len(separator):]
		}
	}
	return message, ""
}

func parseProbeHeaderBlock(block string) (string, string, bool) {
	lines := strings.FieldsFunc(block, func(r rune) bool {
		return r == '\n' || r == '\r'
	})
	for _, line := range lines {
		if strings.TrimLeft(line, " \t") != line {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if !strings.EqualFold(name, ProbeHeader) {
			continue
		}
		value := strings.TrimSpace(line[colon+1:])
		return name, value, true
	}
	return "", "", false
}

func decodeEncodedText(input string) string {
	var builder strings.Builder
	for i := 0; i < len(input); {
		if input[i] == '+' {
			builder.WriteByte(' ')
			i++
			continue
		}
		if input[i] == '%' && i+2 < len(input) && isHex(input[i+1]) && isHex(input[i+2]) {
			builder.WriteByte(byte(fromHex(input[i+1])<<4 | fromHex(input[i+2])))
			i += 3
			continue
		}
		if r, next, ok := decodeUPercent(input, i); ok {
			builder.WriteRune(r)
			i = next
			continue
		}
		builder.WriteByte(input[i])
		i++
	}
	return builder.String()
}

func decodeUPercent(input string, start int) (rune, int, bool) {
	if start+6 > len(input) || input[start] != '%' || (input[start+1] != 'u' && input[start+1] != 'U') {
		return 0, start, false
	}
	value64, err := strconv.ParseUint(input[start+2:start+6], 16, 16)
	if err != nil {
		return 0, start, false
	}
	value := rune(value64)
	next := start + 6
	if utf16.IsSurrogate(value) && next+6 <= len(input) &&
		input[next] == '%' && (input[next+1] == 'u' || input[next+1] == 'U') {
		if low64, err := strconv.ParseUint(input[next+2:next+6], 16, 16); err == nil {
			value = utf16.DecodeRune(value, rune(low64))
			next += 6
		}
	}
	return value, next, true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func fromHex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

func utf16Candidates(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		return []string{decodeUTF16(raw[2:], true)}
	}
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		return []string{decodeUTF16(raw[2:], false)}
	}

	oddZeros, evenZeros := 0, 0
	oddTotal, evenTotal := 0, 0
	for i := 0; i < len(raw); i++ {
		if i%2 == 1 {
			oddTotal++
			if raw[i] == 0 {
				oddZeros++
			}
		} else {
			evenTotal++
			if raw[i] == 0 {
				evenZeros++
			}
		}
	}
	if oddTotal >= 4 && oddZeros*2 >= oddTotal {
		return []string{decodeUTF16(raw, true)}
	}
	if evenTotal >= 4 && evenZeros*2 >= evenTotal {
		return []string{decodeUTF16(raw, false)}
	}
	return nil
}

func decodeUTF16(raw []byte, littleEndian bool) string {
	if len(raw)%2 == 1 {
		raw = raw[:len(raw)-1]
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		if littleEndian {
			units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
		} else {
			units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
		}
	}
	return string(utf16.Decode(units))
}

func classifyMiss(cs *caseState) Outcome {
	resp := cs.response
	outcome := Outcome{CaseID: cs.data.ID, Hit: false, Location: LocationNone}
	probeName := strings.ToLower(ProbeHeader)

	probeInBody := strings.Contains(strings.ToLower(resp.Body), probeName)
	probeInHeaderArea := false
	reflected := responseReflectsCase(resp, cs.data)

	for _, header := range resp.Headers {
		text := strings.ToLower(header.Name + " " + header.Value)
		if strings.Contains(text, probeName) {
			probeInHeaderArea = true
		}
	}

	for _, text := range responseTextCandidates(resp.Raw) {
		head, body := splitHTTPMessage(text)
		lowerHead := strings.ToLower(head)
		lowerBody := strings.ToLower(body)
		if strings.Contains(lowerBody, probeName) {
			probeInBody = true
		}
		if strings.Contains(lowerHead, probeName) {
			probeInHeaderArea = true
		}
	}

	switch {
	case probeInBody:
		outcome.Location = LocationBody
		outcome.Reason = ReasonFiltered
	case probeInHeaderArea:
		outcome.Location = LocationHeader
		outcome.Reason = ReasonNormalized
	case reflected:
		outcome.Location = LocationNone
		outcome.Reason = ReasonNormalized
	default:
		outcome.Location = LocationNone
		outcome.Reason = ReasonNoReflection
	}
	return outcome
}

func responseReflectsCase(resp Response, c Case) bool {
	signatures := []string{c.Payload}
	signatures = append(signatures, decodingLayers(c.Payload)...)
	marker := probeValuePrefix + strings.TrimPrefix(c.ID, "case-")
	signatures = append(signatures, marker)

	texts := []string{resp.Body}
	for _, header := range resp.Headers {
		texts = append(texts, header.Name, header.Value)
	}
	texts = append(texts, string(resp.Raw))

	for _, text := range texts {
		for _, layer := range decodingLayers(text) {
			for _, signature := range signatures {
				if signature != "" && strings.Contains(layer, signature) {
					return true
				}
			}
		}
	}
	return false
}
