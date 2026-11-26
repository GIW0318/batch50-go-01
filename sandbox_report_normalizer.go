// Package task folds the behaviour reports of several sandboxes for one
// sample into a versioned consensus and answers per topic with traceable
// evidence.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

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
	ErrEmptyReport     = errors.New("report id, sample id, sandbox and verdict are required")
	ErrBadVerdict      = errors.New("verdict must be malicious, clean or unknown")
	ErrDuplicateReport = errors.New("report already ingested")
	ErrReportLimit     = errors.New("report limit reached for this sample")
	ErrBehaviorLimit   = errors.New("behaviour limit reached for this report")
	ErrSampleLimit     = errors.New("sample limit reached")
	ErrUnknownSample   = errors.New("unknown sample")
	ErrPendingConsensus = errors.New("consensus is not published yet")
	ErrNoConsensus     = errors.New("no published consensus for this sample")
	ErrUnresolved      = errors.New("conflicting verdicts cannot be resolved")
	ErrNoEvidence      = errors.New("no sandbox observed this topic")
	ErrBadDictionary   = errors.New("dictionary entry is incomplete")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
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

// SandboxReportNormalizer owns the per sample merge tables.
type SandboxReportNormalizer struct {
	limits Limits
	dict   []DictionaryEntry
	now    func() time.Time
}

// NewSandboxReportNormalizer builds a normalizer with the given caps and dictionary.
func NewSandboxReportNormalizer(limits Limits, dict []DictionaryEntry) *SandboxReportNormalizer {
	return &SandboxReportNormalizer{limits: limits, dict: dict, now: time.Now}
}

// SetClock replaces the clock used to decide whether a merge window expired.
func (n *SandboxReportNormalizer) SetClock(now func() time.Time) {
	n.now = now
}

// Ingest validates one report, maps its behaviours and stores it as in-flight.
func (n *SandboxReportNormalizer) Ingest(report Report) (Normalized, error) {
	return Normalized{}, errNotImplemented
}

// Expect declares which sandboxes must report before a merge may publish.
func (n *SandboxReportNormalizer) Expect(sampleID string, sandboxes []string) error {
	return errNotImplemented
}

// Merge publishes the consensus when every expected sandbox arrived or the
// merge window expired, and reports ErrPendingConsensus otherwise.
func (n *SandboxReportNormalizer) Merge(sampleID string) (Consensus, error) {
	return Consensus{}, errNotImplemented
}

// Resolve decides one topic of a published consensus.
func (n *SandboxReportNormalizer) Resolve(sampleID string, topic string) (Decision, error) {
	return Decision{}, errNotImplemented
}

// Explain returns the reports, sandboxes and raw API names behind a decision.
func (n *SandboxReportNormalizer) Explain(sampleID string, topic string) (Evidence, error) {
	return Evidence{}, errNotImplemented
}

// Expire drops a sample, published or not, together with its in-flight state.
func (n *SandboxReportNormalizer) Expire(sampleID string) error {
	return errNotImplemented
}

// Pending reports how many reports are held for a sample.
func (n *SandboxReportNormalizer) Pending(sampleID string) int {
	return 0
}

// State reads the in-flight bookkeeping of one sample.
func (n *SandboxReportNormalizer) State(sampleID string) (SampleState, bool) {
	return SampleState{}, false
}

// Samples lists the tracked samples in ascending order.
func (n *SandboxReportNormalizer) Samples() []string {
	return nil
}

// Snapshot serialises every tracked sample and its merge deadline.
func (n *SandboxReportNormalizer) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (n *SandboxReportNormalizer) Restore(data []byte) error {
	return errNotImplemented
}
