// Package task dispatches RADIUS CoA and Disconnect requests to NAS devices and
// normalises their replies.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented marks behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Request kinds accepted by the dispatcher.
const (
	KindCoA        = "coa"
	KindDisconnect = "disconnect"
)

// Job lifecycle states.
const (
	StateQueued   = "queued"
	StateInflight = "inflight"
	StateAcked    = "acked"
	StateNacked   = "nacked"
	StateTimeout  = "timeout"
	StateCanceled = "canceled"
)

// Session states recorded next to the jobs.
const (
	SessionOnline     = "online"
	SessionTerminated = "terminated"
	SessionUnknown    = "unknown"
)

// RADIUS reply codes that matter to the dispatcher.
const (
	CodeDisconnectAck = 40
	CodeCoAAck        = 41
	CodeCoANak        = 44
	CodeDisconnectNak = 45
)

// Error-Cause values carried by a NAK.
const (
	CauseUnsupportedAttribute = 401
	CauseUnsupportedValue     = 402
	CauseAdminProhibited      = 403
	CauseNotRoutable          = 404
	CauseSessionNotFound      = 405
	CauseSessionNotRemovable  = 406
	CauseResourcesUnavailable = 506
	CauseRequestInitiated     = 507
)

// Errors surfaced by the dispatcher.
var (
	ErrBadRequest       = errors.New("request kind and session id are required")
	ErrUnsupportedKind  = errors.New("request kind is not supported")
	ErrQueueFull        = errors.New("pending job cap reached")
	ErrUnknownJob       = errors.New("job not found")
	ErrJobClosed        = errors.New("job is already concluded")
	ErrNotRetryable     = errors.New("job cannot be retried")
	ErrProtocolMismatch = errors.New("reply code does not match the request kind")
	ErrUnknownSession   = errors.New("session is not known")
	ErrBadSnapshot      = errors.New("snapshot payload is invalid")
)

// Attribute is one RADIUS attribute of a request.
type Attribute struct {
	Name  string
	Value string
}

// CoaRequest is one CoA-Request or Disconnect-Request to a NAS.
type CoaRequest struct {
	Kind      string
	SessionID string
	NasIP     string
	UserName  string
	Attrs     []Attribute
	Priority  int
}

// Job is the queued unit of work for one session.
type Job struct {
	ID        string
	Kind      string
	SessionID string
	NasIP     string
	State     string
	Attempts  int
	NextAt    time.Time
	Seq       int
	Budget    time.Duration
}

// Reply is a CoA-ACK, CoA-NAK, Disconnect-ACK or Disconnect-NAK from a NAS.
type Reply struct {
	Code   int
	Cause  int
	Detail string
}

// Result is the recorded outcome of one job.
type Result struct {
	JobID     string
	SessionID string
	State     string
	Code      int
	Cause     int
	Retryable bool
	Attempts  int
	Elapsed   time.Duration
}

// SessionRecord is the dispatcher view of one NAS session.
type SessionRecord struct {
	ID      string
	NasIP   string
	State   string
	Jobs    int
	LastJob string
}

// DispatcherConfig bounds retransmission and the pending table.
type DispatcherConfig struct {
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	MaxAttempts int
	TotalBudget time.Duration
	MaxPending  int
	Now         func() time.Time
}

// DispatcherStats counts job transitions.
type DispatcherStats struct {
	Submitted   int
	Acked       int
	Nacked      int
	TimedOut    int
	Canceled    int
	Retried     int
	LateReplies int
}

// RadiusCoaDispatcher owns the job table, the retry schedule and the sessions.
type RadiusCoaDispatcher struct {
	cfg DispatcherConfig
	mu  sync.Mutex
}

// NewRadiusCoaDispatcher builds a dispatcher with the given bounds.
func NewRadiusCoaDispatcher(cfg DispatcherConfig) *RadiusCoaDispatcher {
	return &RadiusCoaDispatcher{cfg: cfg}
}

// Submit queues a CoA or Disconnect request for one session.
func (d *RadiusCoaDispatcher) Submit(req CoaRequest) (Job, error) {
	return Job{}, errNotImplemented
}

// Outcome reads the recorded result of one job.
func (d *RadiusCoaDispatcher) Outcome(jobID string) (Result, error) {
	return Result{}, errNotImplemented
}

// Retry requeues a job whose last attempt failed.
func (d *RadiusCoaDispatcher) Retry(jobID string) error {
	return errNotImplemented
}

// Cancel drops a job that has not been concluded yet.
func (d *RadiusCoaDispatcher) Cancel(jobID string) error {
	return errNotImplemented
}

// Pending lists every queued or in flight job in dispatch order.
func (d *RadiusCoaDispatcher) Pending() []Job {
	return nil
}

// Complete records a NAS reply for one job and normalises the answer.
func (d *RadiusCoaDispatcher) Complete(jobID string, reply Reply) (Result, error) {
	return Result{}, errNotImplemented
}

// Tick dispatches every job that is due at the given instant.
func (d *RadiusCoaDispatcher) Tick(now time.Time) []Job {
	return nil
}

// SessionState reads the dispatcher view of one session.
func (d *RadiusCoaDispatcher) SessionState(sessionID string) (SessionRecord, error) {
	return SessionRecord{}, errNotImplemented
}

// Stats reports counters of the dispatcher.
func (d *RadiusCoaDispatcher) Stats() DispatcherStats {
	return DispatcherStats{}
}

// Snapshot serialises jobs, sessions and the sequence counter.
func (d *RadiusCoaDispatcher) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *RadiusCoaDispatcher) Restore(data []byte) error {
	return errNotImplemented
}
