// Package task drives window aligned duplicate requests at a suspicious endpoint
// and turns the before and after difference into an explainable race verdict.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Lifecycle states of one probe.
const (
	StatePlanned   = "planned"
	StateFiring    = "firing"
	StateFired     = "fired"
	StateCancelled = "cancelled"
)

// Verdict results of one verified probe.
const (
	ResultConfirmed = "confirmed"
	ResultSafe      = "safe"
	ResultUncertain = "uncertain"
)

// Reason codes carried by a verdict.
const (
	ReasonDuplicateEffects = "reason_duplicate_side_effects"
	ReasonSingleEffect     = "reason_single_effect"
	ReasonIncompleteWindow = "reason_incomplete_window"
)

// Errors returned by the prober.
var (
	ErrBadOperation     = errors.New("operation key, endpoint or request count is invalid")
	ErrRequestLimit     = errors.New("request count above the probe cap")
	ErrConcurrencyLimit = errors.New("concurrency above the request count or the cap")
	ErrRetryLimit       = errors.New("retry count above the cap")
	ErrProbeLimit       = errors.New("probe cap reached")
	ErrUnknownProbe     = errors.New("unknown probe")
	ErrProbeInFlight    = errors.New("another probe of the same business key is in flight")
	ErrAlreadyFired     = errors.New("probe already fired")
	ErrProbeCancelled   = errors.New("probe was cancelled")
	ErrNotFired         = errors.New("probe has not been fired")
	ErrNoTransport      = errors.New("no transport attached")
	ErrCooldown         = errors.New("target is inside the cooldown window")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Config bounds one prober instance.
type Config struct {
	MaxRequests    int
	MaxConcurrency int
	MaxRetries     int
	MaxProbes      int
	Cooldown       time.Duration
}

// Operation is the business action under test. Every probe of one Operation
// replays the same action, and IdempotencyKey mirrors whatever the client
// already sends so the server side behaviour stays unchanged.
type Operation struct {
	Key            string
	Endpoint       string
	Method         string
	Body           string
	Requests       int
	Concurrency    int
	Retries        int
	IdempotencyKey string
}

// Probe is the frozen plan of one window.
type Probe struct {
	ProbeID     string    `json:"probe_id"`
	Key         string    `json:"key"`
	Endpoint    string    `json:"endpoint"`
	Planned     int       `json:"planned"`
	Concurrency int       `json:"concurrency"`
	Retries     int       `json:"retries"`
	State       string    `json:"state"`
	FiredAt     time.Time `json:"fired_at"`
}

// Request is one HTTP level call issued by Fire.
type Request struct {
	ProbeID  string `json:"probe_id"`
	Ordinal  int    `json:"ordinal"`
	Attempt  int    `json:"attempt"`
	Key      string `json:"key"`
	Endpoint string `json:"endpoint"`
	Body     string `json:"body"`
}

// Response is what the endpoint answered. Applied says whether the server
// executed the business action on this call.
type Response struct {
	Status  int    `json:"status"`
	Body    string `json:"body"`
	Applied bool   `json:"applied"`
}

// Outcome is the recorded result of one request.
type Outcome struct {
	Ordinal int    `json:"ordinal"`
	Attempt int    `json:"attempt"`
	Status  int    `json:"status"`
	Applied bool   `json:"applied"`
	Err     string `json:"err"`
}

// Verdict is the conclusion drawn from the state difference of one probe.
type Verdict struct {
	VerdictID  string `json:"verdict_id"`
	ProbeID    string `json:"probe_id"`
	Key        string `json:"key"`
	Result     string `json:"result"`
	Executions int    `json:"executions"`
	Duplicates int    `json:"duplicates"`
	Attempts   int    `json:"attempts"`
	Reason     string `json:"reason"`
}

// Transport is the endpoint driver injected by the caller. Send performs one
// call, Counter returns the current server side side effect count for a
// business key.
type Transport interface {
	Send(req Request) (Response, error)
	Counter(key string) int
}

// RaceWindowProber owns the planned probes, their outcomes and their verdicts.
type RaceWindowProber struct {
	cfg       Config
	transport Transport
	now       func() time.Time
}

// NewRaceWindowProber builds a prober with the given caps.
func NewRaceWindowProber(cfg Config) *RaceWindowProber {
	return &RaceWindowProber{cfg: cfg, now: time.Now}
}

// SetTransport attaches the endpoint driver.
func (p *RaceWindowProber) SetTransport(t Transport) error {
	return errNotImplemented
}

// SetClock replaces the clock used for cooldown decisions.
func (p *RaceWindowProber) SetClock(now func() time.Time) {
}

// Plan validates an operation and freezes it into a probe.
func (p *RaceWindowProber) Plan(op Operation) (Probe, error) {
	return Probe{}, errNotImplemented
}

// Fire issues the planned requests inside one window and records the outcomes.
func (p *RaceWindowProber) Fire(probeID string) ([]Outcome, error) {
	return nil, errNotImplemented
}

// Verify turns the recorded state difference of one probe into a verdict.
func (p *RaceWindowProber) Verify(probeID string) (Verdict, error) {
	return Verdict{}, errNotImplemented
}

// Cancel drops a probe that has not been fired yet.
func (p *RaceWindowProber) Cancel(probeID string) error {
	return errNotImplemented
}

// State reads one probe.
func (p *RaceWindowProber) State(probeID string) (Probe, bool) {
	return Probe{}, false
}

// Probes lists the known probe ids in creation order.
func (p *RaceWindowProber) Probes() []string {
	return nil
}

// Attempts counts the requests actually issued for one business key.
func (p *RaceWindowProber) Attempts(key string) int {
	return 0
}

// Pending counts the probes that were fired but not verified yet.
func (p *RaceWindowProber) Pending() int {
	return 0
}

// Snapshot serialises probes, outcomes, verdicts and the attempt counters.
func (p *RaceWindowProber) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *RaceWindowProber) Restore(data []byte) error {
	return errNotImplemented
}
