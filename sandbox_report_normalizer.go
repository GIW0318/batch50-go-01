// Package task folds the behaviour reports of several sandboxes for one
// sample into a versioned consensus and answers per topic with traceable
// evidence.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verdicts a sandbox may return for one sample.
const (
	VerdictMalicious = "malicious"
	VerdictClean     = "clean"
	VerdictUnknown   = "unknown"
)

// Outcomes of a per topic decision. Missing means no sandbox could observe
// the capability; negative means a sandbox looked and found nothing.
const (
	OutcomeMalicious = "malicious"
	OutcomeClean     = "clean"
	OutcomeMissing   = "missing"
	OutcomeNegative  = "negative"
)

// Reason codes carried by a decision.
const (
	ReasonUnanimous = "reason_unanimous"
	ReasonMajority  = "reason_majority"
)

// TopicUnclassified collects behaviours that the dictionary does not cover.
const TopicUnclassified = "unclassified"

// Errors returned by the normalizer.
var (
	ErrEmptyReport      = errors.New("report id, sample id, sandbox and verdict are required")
	ErrBadVerdict       = errors.New("verdict must be malicious, clean or unknown")
	ErrDuplicateReport  = errors.New("report already ingested")
	ErrReportLimit      = errors.New("report limit reached for this sample")
	ErrBehaviorLimit    = errors.New("behaviour limit reached for this report")
	ErrSampleLimit      = errors.New("sample limit reached")
	ErrUnknownSample    = errors.New("unknown sample")
	ErrPendingConsensus = errors.New("consensus is not published yet")
	ErrNoConsensus      = errors.New("no published consensus for this sample")
	ErrUnresolved       = errors.New("conflicting verdicts cannot be resolved")
	ErrNoEvidence       = errors.New("no sandbox observed this topic")
	ErrBadDictionary    = errors.New("dictionary entry is incomplete")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// DictionaryEntry maps the API name a sandbox uses to a capability topic.
// A Sandbox of "*" applies to every sandbox that has no exact entry.
type DictionaryEntry struct {
	Sandbox string `json:"sandbox"`
	API     string `json:"api"`
	Topic   string `json:"topic"`
}

// Limits bounds the live tables and the merge window.
type Limits struct {
	MaxReportsPerSample   int
	MaxBehaviorsPerReport int
	MaxSamples            int
	MergeWindow           time.Duration
}

// Behavior is one API occurrence counted by a sandbox.
type Behavior struct {
	API   string `json:"api"`
	Count int    `json:"count"`
}

// Report is the raw submission of one sandbox for one sample.
type Report struct {
	ReportID   string     `json:"report_id"`
	SampleID   string     `json:"sample_id"`
	Sandbox    string     `json:"sandbox"`
	Verdict    string     `json:"verdict"`
	ReceivedAt time.Time  `json:"received_at"`
	Behaviors  []Behavior `json:"behaviors"`
}

// TopicCount is one normalised capability bucket.
type TopicCount struct {
	Topic     string `json:"topic"`
	Count     int    `json:"count"`
	Sandboxes int    `json:"sandboxes"`
}

// Normalized is the accepted, dictionary mapped view of one report.
type Normalized struct {
	SampleID string       `json:"sample_id"`
	ReportID string       `json:"report_id"`
	Sandbox  string       `json:"sandbox"`
	Topics   []TopicCount `json:"topics"`
}

// Consensus is the merged view of one sample.
type Consensus struct {
	SampleID    string       `json:"sample_id"`
	Version     int          `json:"version"`
	ReportCount int          `json:"report_count"`
	Sandboxes   []string     `json:"sandboxes"`
	Topics      []TopicCount `json:"topics"`
	Complete    bool         `json:"complete"`
	Missing     []string     `json:"missing"`
	PublishedAt time.Time    `json:"published_at"`
}

// Decision is the resolved outcome for one topic of one sample.
type Decision struct {
	DecisionID string `json:"decision_id"`
	SampleID   string `json:"sample_id"`
	Topic      string `json:"topic"`
	Outcome    string `json:"outcome"`
	Votes      int    `json:"votes"`
	Reason     string `json:"reason"`
	Version    int    `json:"version"`
}

// Evidence ties a decision back to the reports and raw API names behind it.
type Evidence struct {
	DecisionID string              `json:"decision_id"`
	SampleID   string              `json:"sample_id"`
	Topic      string              `json:"topic"`
	ReportIDs  []string            `json:"report_ids"`
	Sandboxes  []string            `json:"sandboxes"`
	RawAPIs    map[string][]string `json:"raw_apis"`
	Version    int                 `json:"version"`
}

// SampleState is the in-flight bookkeeping of one sample.
type SampleState struct {
	SampleID  string    `json:"sample_id"`
	Reports   int       `json:"reports"`
	Expected  int       `json:"expected"`
	Published bool      `json:"published"`
	Deadline  time.Time `json:"deadline"`
}

// reportRecord is the immutable part of an accepted report that is retained
// for merge and evidence generation.
type reportRecord struct {
	ReportID   string
	SampleID   string
	Sandbox    string
	Verdict    string
	ReceivedAt time.Time
	Behaviors  []Behavior
}

// sampleRecord holds all in-flight and published state of one sample.
type sampleRecord struct {
	id             string
	reports        map[string]*reportRecord
	expected       map[string]struct{}
	expectDeclared bool
	deadline       time.Time
	version        int
	consensus      *Consensus
}

// SandboxReportNormalizer owns the per sample merge tables.
type SandboxReportNormalizer struct {
	mu     sync.Mutex
	limits Limits
	dict   []DictionaryEntry

	exactTopics    map[string]string
	wildcardTopics map[string]string
	apiTopics      map[string]string
	ambiguousAPIs  map[string]struct{}
	dictErr        error

	now       func() time.Time
	samples   map[string]*sampleRecord
	reportIDs map[string]string
}

const (
	snapshotMagic  = "sandbox-report-normalizer/v1"
	snapshotSchema = 1
)

type snapshotPayload struct {
	Magic   string           `json:"magic"`
	Schema  int              `json:"schema"`
	Samples []snapshotSample `json:"samples"`
}

type snapshotSample struct {
	SampleID       string           `json:"sample_id"`
	Reports        []snapshotReport `json:"reports"`
	Expected       []string         `json:"expected"`
	ExpectDeclared bool             `json:"expect_declared"`
	Deadline       time.Time        `json:"deadline"`
	Version        int              `json:"version"`
	Consensus      *Consensus       `json:"consensus,omitempty"`
}

type snapshotReport struct {
	ReportID   string     `json:"report_id"`
	SampleID   string     `json:"sample_id"`
	Sandbox    string     `json:"sandbox"`
	Verdict    string     `json:"verdict"`
	ReceivedAt time.Time  `json:"received_at"`
	Behaviors  []Behavior `json:"behaviors"`
}

// NewSandboxReportNormalizer builds a normalizer with the given caps and dictionary.
func NewSandboxReportNormalizer(limits Limits, dict []DictionaryEntry) *SandboxReportNormalizer {
	n := &SandboxReportNormalizer{
		limits:         limits,
		dict:           append([]DictionaryEntry(nil), dict...),
		exactTopics:    make(map[string]string),
		wildcardTopics: make(map[string]string),
		apiTopics:      make(map[string]string),
		ambiguousAPIs:  make(map[string]struct{}),
		now:            time.Now,
		samples:        make(map[string]*sampleRecord),
		reportIDs:      make(map[string]string),
	}
	for _, entry := range n.dict {
		if strings.TrimSpace(entry.Sandbox) == "" || strings.TrimSpace(entry.API) == "" || strings.TrimSpace(entry.Topic) == "" {
			n.dictErr = ErrBadDictionary
			break
		}
		if entry.Sandbox == "*" {
			if _, exists := n.wildcardTopics[entry.API]; !exists {
				n.wildcardTopics[entry.API] = entry.Topic
			}
			continue
		}
		key := dictionaryKey(entry.Sandbox, entry.API)
		if _, exists := n.exactTopics[key]; !exists {
			n.exactTopics[key] = entry.Topic
		}
		if topic, exists := n.apiTopics[entry.API]; !exists {
			n.apiTopics[entry.API] = entry.Topic
		} else if topic != entry.Topic {
			n.ambiguousAPIs[entry.API] = struct{}{}
		}
	}
	return n
}

// SetClock replaces the clock used to decide whether a merge window expired.
func (n *SandboxReportNormalizer) SetClock(now func() time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	n.now = now
}

// Ingest validates one report, maps its behaviours and stores it as in-flight.
func (n *SandboxReportNormalizer) Ingest(report Report) (Normalized, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.dictErr != nil {
		return Normalized{}, n.dictErr
	}
	if strings.TrimSpace(report.ReportID) == "" || strings.TrimSpace(report.SampleID) == "" || strings.TrimSpace(report.Sandbox) == "" {
		return Normalized{}, ErrEmptyReport
	}
	if !validVerdict(report.Verdict) {
		return Normalized{}, ErrBadVerdict
	}

	maxBehaviors := n.limits.MaxBehaviorsPerReport
	if maxBehaviors < 0 {
		maxBehaviors = 0
	}
	if len(report.Behaviors) > maxBehaviors {
		return Normalized{}, ErrBehaviorLimit
	}

	if _, exists := n.reportIDs[report.ReportID]; exists {
		return Normalized{}, ErrDuplicateReport
	}

	maxReports := n.limits.MaxReportsPerSample
	if maxReports < 0 {
		maxReports = 0
	}
	s, exists := n.samples[report.SampleID]
	if exists {
		if len(s.reports) >= maxReports {
			return Normalized{}, ErrReportLimit
		}
	} else {
		if maxReports <= 0 {
			return Normalized{}, ErrReportLimit
		}
		maxSamples := n.limits.MaxSamples
		if maxSamples < 0 {
			maxSamples = 0
		}
		if len(n.samples) >= maxSamples {
			return Normalized{}, ErrSampleLimit
		}
		s = &sampleRecord{
			id:       report.SampleID,
			reports:  make(map[string]*reportRecord),
			expected: make(map[string]struct{}),
		}
		n.samples[report.SampleID] = s
	}

	record := &reportRecord{
		ReportID:   report.ReportID,
		SampleID:   report.SampleID,
		Sandbox:    report.Sandbox,
		Verdict:    report.Verdict,
		ReceivedAt: report.ReceivedAt,
		Behaviors:  cloneBehaviors(report.Behaviors),
	}
	if len(s.reports) == 0 {
		firstSeen := n.now()
		if record.ReceivedAt.After(firstSeen) {
			firstSeen = record.ReceivedAt
		}
		s.deadline = firstSeen.Add(n.limits.MergeWindow)
	}
	s.reports[record.ReportID] = record
	n.reportIDs[record.ReportID] = record.SampleID

	topics := n.topicsFor(record.Sandbox, record.Behaviors)
	if s.consensus != nil {
		n.recomputeLocked(s)
	}
	return Normalized{
		SampleID: record.SampleID,
		ReportID: record.ReportID,
		Sandbox:  record.Sandbox,
		Topics:   topics,
	}, nil
}

// Expect declares which sandboxes must report before a merge may publish.
func (n *SandboxReportNormalizer) Expect(sampleID string, sandboxes []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if strings.TrimSpace(sampleID) == "" {
		return ErrEmptyReport
	}
	s, exists := n.samples[sampleID]
	if !exists {
		maxSamples := n.limits.MaxSamples
		if maxSamples < 0 {
			maxSamples = 0
		}
		if len(n.samples) >= maxSamples {
			return ErrSampleLimit
		}
		s = &sampleRecord{
			id:       sampleID,
			reports:  make(map[string]*reportRecord),
			expected: make(map[string]struct{}),
		}
		n.samples[sampleID] = s
	}

	changed := false
	if !s.expectDeclared {
		s.expectDeclared = true
		changed = true
	}
	for _, sandbox := range sandboxes {
		if strings.TrimSpace(sandbox) == "" {
			continue
		}
		if _, present := s.expected[sandbox]; !present {
			s.expected[sandbox] = struct{}{}
			changed = true
		}
	}
	if changed && s.consensus != nil {
		n.recomputeLocked(s)
	}
	return nil
}

// Merge publishes the consensus when every expected sandbox arrived or the
// merge window expired, and reports ErrPendingConsensus otherwise.
func (n *SandboxReportNormalizer) Merge(sampleID string) (Consensus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	s, exists := n.samples[sampleID]
	if !exists {
		return Consensus{}, ErrUnknownSample
	}
	if s.consensus != nil {
		return *cloneConsensus(s.consensus), nil
	}
	if len(s.reports) == 0 {
		return Consensus{}, ErrPendingConsensus
	}

	arrived := n.allExpectedArrivedLocked(s)
	expired := !s.deadline.IsZero() && !n.now().Before(s.deadline)
	if !arrived && !expired {
		return Consensus{}, ErrPendingConsensus
	}

	version := s.version + 1
	if version < 1 {
		version = 1
	}
	consensus := n.buildConsensusLocked(s, version)
	s.version = version
	s.consensus = &consensus
	return *cloneConsensus(&consensus), nil
}

// Resolve decides one topic of a published consensus.
func (n *SandboxReportNormalizer) Resolve(sampleID string, topic string) (Decision, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	s, exists := n.samples[sampleID]
	if !exists {
		return Decision{}, ErrUnknownSample
	}
	if s.consensus == nil {
		return Decision{}, ErrNoConsensus
	}

	version := s.consensus.Version
	obs := n.observeTopicLocked(s, topic)
	if len(obs.participants) == 0 {
		return n.absentDecisionLocked(s, topic, version), nil
	}

	maliciousVotes := 0
	cleanVotes := 0
	unknownVotes := 0
	for _, vote := range obs.participants {
		switch vote.verdict {
		case VerdictMalicious:
			maliciousVotes++
		case VerdictClean:
			cleanVotes++
		default:
			unknownVotes++
		}
	}

	totalVotes := maliciousVotes + cleanVotes + unknownVotes
	base := Decision{
		DecisionID: decisionID(sampleID, topic, version),
		SampleID:   sampleID,
		Topic:      topic,
		Votes:      totalVotes,
		Version:    version,
	}

	switch {
	case maliciousVotes > cleanVotes:
		base.Outcome = OutcomeMalicious
		if cleanVotes == 0 && unknownVotes == 0 {
			base.Reason = ReasonUnanimous
		} else {
			base.Reason = ReasonMajority
		}
		return base, nil
	case cleanVotes > maliciousVotes:
		base.Outcome = OutcomeClean
		if maliciousVotes == 0 && unknownVotes == 0 {
			base.Reason = ReasonUnanimous
		} else {
			base.Reason = ReasonMajority
		}
		return base, nil
	case maliciousVotes > 0 && cleanVotes > 0:
		return Decision{}, ErrUnresolved
	default:
		base.Outcome = OutcomeMissing
		return base, nil
	}
}

// Explain returns the reports, sandboxes and raw API names behind a decision.
func (n *SandboxReportNormalizer) Explain(sampleID string, topic string) (Evidence, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	s, exists := n.samples[sampleID]
	if !exists {
		return Evidence{}, ErrUnknownSample
	}
	if s.consensus == nil {
		return Evidence{}, ErrNoConsensus
	}
	obs := n.observeTopicLocked(s, topic)
	if len(obs.participants) == 0 {
		return Evidence{}, ErrNoEvidence
	}

	rawAPIs := make(map[string][]string, len(obs.rawAPIs))
	for sandbox, apis := range obs.rawAPIs {
		rawAPIs[sandbox] = append([]string(nil), apis...)
	}
	return Evidence{
		DecisionID: decisionID(sampleID, topic, s.consensus.Version),
		SampleID:   sampleID,
		Topic:      topic,
		ReportIDs:  append([]string(nil), obs.reportIDs...),
		Sandboxes:  append([]string(nil), obs.sandboxes...),
		RawAPIs:    rawAPIs,
		Version:    s.consensus.Version,
	}, nil
}

// Expire drops a sample, published or not, together with its in-flight state.
func (n *SandboxReportNormalizer) Expire(sampleID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	s, exists := n.samples[sampleID]
	if !exists {
		return nil
	}
	for reportID := range s.reports {
		delete(n.reportIDs, reportID)
	}
	delete(n.samples, sampleID)
	return nil
}

// Pending reports how many reports are held for a sample.
func (n *SandboxReportNormalizer) Pending(sampleID string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, exists := n.samples[sampleID]
	if !exists {
		return 0
	}
	return len(s.reports)
}

// State reads the in-flight bookkeeping of one sample.
func (n *SandboxReportNormalizer) State(sampleID string) (SampleState, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, exists := n.samples[sampleID]
	if !exists {
		return SampleState{}, false
	}
	return SampleState{
		SampleID:  s.id,
		Reports:   len(s.reports),
		Expected:  len(s.expected),
		Published: s.consensus != nil,
		Deadline:  s.deadline,
	}, true
}

// Samples lists the tracked samples in ascending order.
func (n *SandboxReportNormalizer) Samples() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := make([]string, 0, len(n.samples))
	for id := range n.samples {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Snapshot serialises every tracked sample and its merge deadline.
func (n *SandboxReportNormalizer) Snapshot() ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	payload := snapshotPayload{
		Magic:   snapshotMagic,
		Schema:  snapshotSchema,
		Samples: make([]snapshotSample, 0, len(n.samples)),
	}
	sampleIDs := make([]string, 0, len(n.samples))
	for id := range n.samples {
		sampleIDs = append(sampleIDs, id)
	}
	sort.Strings(sampleIDs)
	for _, id := range sampleIDs {
		s := n.samples[id]
		reportIDs := make([]string, 0, len(s.reports))
		for reportID := range s.reports {
			reportIDs = append(reportIDs, reportID)
		}
		sort.Strings(reportIDs)
		reports := make([]snapshotReport, 0, len(reportIDs))
		for _, reportID := range reportIDs {
			record := s.reports[reportID]
			reports = append(reports, snapshotReport{
				ReportID:   record.ReportID,
				SampleID:   record.SampleID,
				Sandbox:    record.Sandbox,
				Verdict:    record.Verdict,
				ReceivedAt: record.ReceivedAt,
				Behaviors:  cloneBehaviors(record.Behaviors),
			})
		}
		expected := make([]string, 0, len(s.expected))
		for sandbox := range s.expected {
			expected = append(expected, sandbox)
		}
		sort.Strings(expected)
		payload.Samples = append(payload.Samples, snapshotSample{
			SampleID:       s.id,
			Reports:        reports,
			Expected:       expected,
			ExpectDeclared: s.expectDeclared,
			Deadline:       s.deadline,
			Version:        s.version,
			Consensus:      cloneConsensus(s.consensus),
		})
	}
	return json.Marshal(payload)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (n *SandboxReportNormalizer) Restore(data []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.dictErr != nil {
		return n.dictErr
	}

	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrBadSnapshot
	}
	if payload.Magic != snapshotMagic || payload.Schema != snapshotSchema {
		return ErrBadSnapshot
	}

	maxSamples := n.limits.MaxSamples
	if maxSamples < 0 {
		maxSamples = 0
	}
	if len(payload.Samples) > maxSamples {
		return ErrBadSnapshot
	}
	maxReports := n.limits.MaxReportsPerSample
	if maxReports < 0 {
		maxReports = 0
	}
	maxBehaviors := n.limits.MaxBehaviorsPerReport
	if maxBehaviors < 0 {
		maxBehaviors = 0
	}

	samples := make(map[string]*sampleRecord, len(payload.Samples))
	reportIDs := make(map[string]string)
	for _, encoded := range payload.Samples {
		if strings.TrimSpace(encoded.SampleID) == "" {
			return ErrBadSnapshot
		}
		if _, duplicate := samples[encoded.SampleID]; duplicate {
			return ErrBadSnapshot
		}
		if encoded.Version < 0 {
			return ErrBadSnapshot
		}
		if len(encoded.Reports) > maxReports {
			return ErrBadSnapshot
		}

		expected := make(map[string]struct{}, len(encoded.Expected))
		for _, sandbox := range encoded.Expected {
			if strings.TrimSpace(sandbox) == "" {
				return ErrBadSnapshot
			}
			expected[sandbox] = struct{}{}
		}

		reports := make(map[string]*reportRecord, len(encoded.Reports))
		for _, report := range encoded.Reports {
			if strings.TrimSpace(report.ReportID) == "" ||
				strings.TrimSpace(report.SampleID) == "" ||
				strings.TrimSpace(report.Sandbox) == "" {
				return ErrBadSnapshot
			}
			if report.SampleID != encoded.SampleID || !validVerdict(report.Verdict) {
				return ErrBadSnapshot
			}
			if len(report.Behaviors) > maxBehaviors {
				return ErrBadSnapshot
			}
			if _, duplicate := reportIDs[report.ReportID]; duplicate {
				return ErrBadSnapshot
			}
			record := &reportRecord{
				ReportID:   report.ReportID,
				SampleID:   report.SampleID,
				Sandbox:    report.Sandbox,
				Verdict:    report.Verdict,
				ReceivedAt: report.ReceivedAt,
				Behaviors:  cloneBehaviors(report.Behaviors),
			}
			reports[record.ReportID] = record
			reportIDs[record.ReportID] = encoded.SampleID
		}

		var consensus *Consensus
		if encoded.Consensus != nil {
			if encoded.Consensus.SampleID != encoded.SampleID || encoded.Consensus.Version != encoded.Version {
				return ErrBadSnapshot
			}
			consensus = cloneConsensus(encoded.Consensus)
		}

		samples[encoded.SampleID] = &sampleRecord{
			id:             encoded.SampleID,
			reports:        reports,
			expected:       expected,
			expectDeclared: encoded.ExpectDeclared,
			deadline:       encoded.Deadline,
			version:        encoded.Version,
			consensus:      consensus,
		}
	}

	n.samples = samples
	n.reportIDs = reportIDs
	return nil
}

func validVerdict(verdict string) bool {
	switch verdict {
	case VerdictMalicious, VerdictClean, VerdictUnknown:
		return true
	default:
		return false
	}
}

func dictionaryKey(sandbox, api string) string {
	return sandbox + "\x00" + api
}

func cloneBehaviors(in []Behavior) []Behavior {
	if in == nil {
		return nil
	}
	return append([]Behavior(nil), in...)
}

func cloneConsensus(in *Consensus) *Consensus {
	if in == nil {
		return nil
	}
	out := *in
	out.Sandboxes = append([]string(nil), in.Sandboxes...)
	out.Missing = append([]string(nil), in.Missing...)
	out.Topics = append([]TopicCount(nil), in.Topics...)
	return &out
}

func (n *SandboxReportNormalizer) topicFor(sandbox, api string) string {
	if topic, exists := n.exactTopics[dictionaryKey(sandbox, api)]; exists {
		return topic
	}
	if topic, exists := n.wildcardTopics[api]; exists {
		return topic
	}
	if _, ambiguous := n.ambiguousAPIs[api]; !ambiguous {
		if topic, exists := n.apiTopics[api]; exists {
			return topic
		}
	}
	return TopicUnclassified
}

func (n *SandboxReportNormalizer) topicsFor(sandbox string, behaviors []Behavior) []TopicCount {
	counts := make(map[string]int)
	for _, behavior := range behaviors {
		topics := n.topicFor(sandbox, behavior.API)
		counts[topics] += behavior.Count
	}
	return sortedTopicCounts(counts, func(string) int { return 1 })
}

func sortedTopicCounts(counts map[string]int, sandboxes func(string) int) []TopicCount {
	topics := make([]string, 0, len(counts))
	for topic := range counts {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	out := make([]TopicCount, 0, len(topics))
	for _, topic := range topics {
		out = append(out, TopicCount{Topic: topic, Count: counts[topic], Sandboxes: sandboxes(topic)})
	}
	return out
}

func (n *SandboxReportNormalizer) recomputeLocked(s *sampleRecord) {
	version := s.version + 1
	if version < 1 {
		version = 1
	}
	consensus := n.buildConsensusLocked(s, version)
	s.version = version
	s.consensus = &consensus
}

func (n *SandboxReportNormalizer) buildConsensusLocked(s *sampleRecord, version int) Consensus {
	reportCounts := make(map[string]int)
	topicSandboxes := make(map[string]map[string]struct{})
	sandboxes := make(map[string]struct{})

	for _, record := range s.reports {
		sandboxes[record.Sandbox] = struct{}{}
		for _, behavior := range record.Behaviors {
			topic := n.topicFor(record.Sandbox, behavior.API)
			reportCounts[topic] += behavior.Count
			if topicSandboxes[topic] == nil {
				topicSandboxes[topic] = make(map[string]struct{})
			}
			topicSandboxes[topic][record.Sandbox] = struct{}{}
		}
	}

	topics := sortedTopicCounts(reportCounts, func(topic string) int {
		return len(topicSandboxes[topic])
	})

	sandboxNames := make([]string, 0, len(sandboxes))
	for sandbox := range sandboxes {
		sandboxNames = append(sandboxNames, sandbox)
	}
	sort.Strings(sandboxNames)

	missing := n.missingSandboxesLocked(s)
	return Consensus{
		SampleID:    s.id,
		Version:     version,
		ReportCount: len(s.reports),
		Sandboxes:   sandboxNames,
		Topics:      topics,
		Complete:    s.expectDeclared && len(missing) == 0,
		Missing:     missing,
		PublishedAt: n.now(),
	}
}

func (n *SandboxReportNormalizer) missingSandboxesLocked(s *sampleRecord) []string {
	if !s.expectDeclared {
		return nil
	}
	present := make(map[string]struct{}, len(s.reports))
	for _, record := range s.reports {
		present[record.Sandbox] = struct{}{}
	}
	missing := make([]string, 0, len(s.expected))
	for sandbox := range s.expected {
		if _, ok := present[sandbox]; !ok {
			missing = append(missing, sandbox)
		}
	}
	sort.Strings(missing)
	return missing
}

func (n *SandboxReportNormalizer) allExpectedArrivedLocked(s *sampleRecord) bool {
	if !s.expectDeclared {
		return false
	}
	if len(s.expected) == 0 {
		return len(s.reports) > 0
	}
	present := make(map[string]struct{}, len(s.reports))
	for _, record := range s.reports {
		present[record.Sandbox] = struct{}{}
	}
	for sandbox := range s.expected {
		if _, ok := present[sandbox]; !ok {
			return false
		}
	}
	return true
}

type topicVote struct {
	verdict    string
	receivedAt time.Time
	reportID   string
}

type topicObservation struct {
	participants map[string]topicVote
	reportIDs    []string
	sandboxes    []string
	rawAPIs      map[string][]string
}

func (n *SandboxReportNormalizer) observeTopicLocked(s *sampleRecord, topic string) topicObservation {
	obs := topicObservation{
		participants: make(map[string]topicVote),
		rawAPIs:      make(map[string][]string),
	}
	reportIDSet := make(map[string]struct{})
	rawAPISet := make(map[string]map[string]struct{})
	sandboxSet := make(map[string]struct{})

	for _, record := range s.reports {
		contributes := false
		for _, behavior := range record.Behaviors {
			if n.topicFor(record.Sandbox, behavior.API) != topic {
				continue
			}
			contributes = true
			if rawAPISet[record.Sandbox] == nil {
				rawAPISet[record.Sandbox] = make(map[string]struct{})
			}
			rawAPISet[record.Sandbox][behavior.API] = struct{}{}
		}
		if !contributes {
			continue
		}
		reportIDSet[record.ReportID] = struct{}{}
		sandboxSet[record.Sandbox] = struct{}{}

		vote, ok := obs.participants[record.Sandbox]
		if !ok || record.ReceivedAt.After(vote.receivedAt) ||
			(record.ReceivedAt.Equal(vote.receivedAt) && record.ReportID > vote.reportID) {
			obs.participants[record.Sandbox] = topicVote{
				verdict:    record.Verdict,
				receivedAt: record.ReceivedAt,
				reportID:   record.ReportID,
			}
		}
	}

	obs.reportIDs = make([]string, 0, len(reportIDSet))
	for reportID := range reportIDSet {
		obs.reportIDs = append(obs.reportIDs, reportID)
	}
	sort.Strings(obs.reportIDs)

	obs.sandboxes = make([]string, 0, len(sandboxSet))
	for sandbox := range sandboxSet {
		obs.sandboxes = append(obs.sandboxes, sandbox)
	}
	sort.Strings(obs.sandboxes)

	for _, sandbox := range obs.sandboxes {
		apis := make([]string, 0, len(rawAPISet[sandbox]))
		for api := range rawAPISet[sandbox] {
			apis = append(apis, api)
		}
		sort.Strings(apis)
		obs.rawAPIs[sandbox] = apis
	}
	return obs
}

func (n *SandboxReportNormalizer) absentDecisionLocked(s *sampleRecord, topic string, version int) Decision {
	missing := n.missingSandboxesLocked(s)
	if s.expectDeclared && len(missing) > 0 {
		return Decision{
			DecisionID: decisionID(s.id, topic, version),
			SampleID:   s.id,
			Topic:      topic,
			Outcome:    OutcomeMissing,
			Version:    version,
		}
	}

	latest := make(map[string]topicVote)
	for _, record := range s.reports {
		vote, ok := latest[record.Sandbox]
		if !ok || record.ReceivedAt.After(vote.receivedAt) ||
			(record.ReceivedAt.Equal(vote.receivedAt) && record.ReportID > vote.reportID) {
			latest[record.Sandbox] = topicVote{
				verdict:    record.Verdict,
				receivedAt: record.ReceivedAt,
				reportID:   record.ReportID,
			}
		}
	}

	clean := 0
	for _, vote := range latest {
		if vote.verdict == VerdictClean {
			clean++
		}
	}
	if clean == 0 {
		return Decision{
			DecisionID: decisionID(s.id, topic, version),
			SampleID:   s.id,
			Topic:      topic,
			Outcome:    OutcomeMissing,
			Version:    version,
		}
	}
	reason := ReasonMajority
	if clean == len(latest) {
		reason = ReasonUnanimous
	}
	return Decision{
		DecisionID: decisionID(s.id, topic, version),
		SampleID:   s.id,
		Topic:      topic,
		Outcome:    OutcomeNegative,
		Votes:      clean,
		Reason:     reason,
		Version:    version,
	}
}

func decisionID(sampleID, topic string, version int) string {
	return fmt.Sprintf("%s:%s:v%d", sampleID, topic, version)
}
