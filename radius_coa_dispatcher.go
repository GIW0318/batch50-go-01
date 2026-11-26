// Package task dispatches RADIUS CoA and Disconnect requests to NAS devices and
// normalises their replies.
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
	cfg      DispatcherConfig
	mu       sync.Mutex
	jobs     map[string]*jobState
	order    []string
	sessions map[string]SessionRecord
	seq      int
	stats    DispatcherStats
}

type jobState struct {
	job           Job
	request       CoaRequest
	result        Result
	createdAt     time.Time
	priority      int
	retryEligible bool
}

type snapshotData struct {
	Version  int             `json:"version"`
	Jobs     []snapshotJob   `json:"jobs"`
	Order    []string        `json:"order"`
	Sessions []SessionRecord `json:"sessions"`
	Seq      int             `json:"seq"`
	Stats    DispatcherStats `json:"stats"`
}

type snapshotJob struct {
	Job           Job        `json:"job"`
	Request       CoaRequest `json:"request"`
	Result        Result     `json:"result"`
	CreatedAt     time.Time  `json:"createdAt"`
	Priority      int        `json:"priority"`
	RetryEligible bool       `json:"retryEligible"`
}

// NewRadiusCoaDispatcher builds a dispatcher with the given bounds.
func NewRadiusCoaDispatcher(cfg DispatcherConfig) *RadiusCoaDispatcher {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RadiusCoaDispatcher{
		cfg:      cfg,
		jobs:     make(map[string]*jobState),
		sessions: make(map[string]SessionRecord),
	}
}

// Submit queues a CoA or Disconnect request for one session.
func (d *RadiusCoaDispatcher) Submit(req CoaRequest) (Job, error) {
	kind := strings.TrimSpace(req.Kind)
	sessionID := strings.TrimSpace(req.SessionID)
	if kind == "" || sessionID == "" {
		return Job{}, ErrBadRequest
	}
	if kind != KindCoA && kind != KindDisconnect {
		return Job{}, ErrUnsupportedKind
	}
	req.Kind = kind
	req.SessionID = sessionID
	req.Attrs = cloneAttributes(req.Attrs)

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, id := range d.order {
		rec, ok := d.jobs[id]
		if ok && rec.job.SessionID == sessionID && isActive(rec.job.State) {
			return rec.job, nil
		}
	}
	if d.cfg.MaxPending > 0 && len(d.order) >= d.cfg.MaxPending {
		return Job{}, ErrQueueFull
	}

	now := d.now()
	id := d.nextJobID()
	rec := &jobState{
		job: Job{
			ID:        id,
			Kind:      kind,
			SessionID: sessionID,
			NasIP:     req.NasIP,
			State:     StateQueued,
			Attempts:  0,
			NextAt:    now,
			Seq:       d.seq,
			Budget:    d.cfg.TotalBudget,
		},
		request:   req,
		result:    Result{JobID: id, SessionID: sessionID, State: StateQueued},
		createdAt: now,
		priority:  req.Priority,
	}
	d.jobs[id] = rec
	d.insertInOrder(id, req.Priority)

	session, ok := d.sessions[sessionID]
	if !ok {
		session = SessionRecord{ID: sessionID, State: SessionOnline}
	}
	if req.NasIP != "" {
		session.NasIP = req.NasIP
	}
	session.Jobs++
	session.LastJob = id
	d.sessions[sessionID] = session
	d.stats.Submitted++

	return rec.job, nil
}

// Outcome reads the recorded result of one job.
func (d *RadiusCoaDispatcher) Outcome(jobID string) (Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.jobs[jobID]
	if !ok {
		return Result{}, ErrUnknownJob
	}
	return rec.result, nil
}

// Retry requeues a job whose last attempt failed.
func (d *RadiusCoaDispatcher) Retry(jobID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.jobs[jobID]
	if !ok {
		return ErrUnknownJob
	}
	eligible := rec.job.State == StateTimeout ||
		(rec.job.State == StateQueued && rec.retryEligible) ||
		(rec.job.State == StateNacked && rec.result.Retryable)
	if !eligible {
		return ErrNotRetryable
	}

	now := d.now()
	if rec.job.State == StateTimeout {
		rec.job.Attempts = 0
		rec.createdAt = now
		rec.result = Result{
			JobID:     rec.job.ID,
			SessionID: rec.job.SessionID,
			State:     StateQueued,
		}
	} else {
		rec.result.State = StateQueued
	}
	rec.job.State = StateQueued
	rec.job.NextAt = now
	rec.retryEligible = false
	if !d.orderContains(rec.job.ID) {
		d.insertInOrder(rec.job.ID, rec.priority)
	}
	d.stats.Retried++
	return nil
}

// Cancel drops a job that has not been concluded yet.
func (d *RadiusCoaDispatcher) Cancel(jobID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.jobs[jobID]
	if !ok {
		return ErrUnknownJob
	}
	if !isActive(rec.job.State) {
		return ErrJobClosed
	}
	now := d.now()
	rec.job.State = StateCanceled
	rec.result = Result{
		JobID:     rec.job.ID,
		SessionID: rec.job.SessionID,
		State:     StateCanceled,
		Attempts:  rec.job.Attempts,
		Elapsed:   elapsed(now, rec.createdAt),
	}
	rec.retryEligible = false
	d.removeFromOrder(jobID)
	d.stats.Canceled++
	return nil
}

// Pending lists every queued or in flight job in dispatch order.
func (d *RadiusCoaDispatcher) Pending() []Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	pending := make([]Job, 0, len(d.order))
	for _, id := range d.order {
		if rec, ok := d.jobs[id]; ok && isActive(rec.job.State) {
			pending = append(pending, rec.job)
		}
	}
	return pending
}

// Complete records a NAS reply for one job and normalises the answer.
func (d *RadiusCoaDispatcher) Complete(jobID string, reply Reply) (Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.jobs[jobID]
	if !ok {
		return Result{}, ErrUnknownJob
	}
	if isClosed(rec.job.State) {
		d.stats.LateReplies++
		return rec.result, ErrJobClosed
	}
	if !replyMatches(rec.job.Kind, reply.Code) {
		return rec.result, ErrProtocolMismatch
	}

	now := d.now()
	elapsedTime := elapsed(now, rec.createdAt)
	budgetExpired := rec.job.Budget > 0 && elapsedTime >= rec.job.Budget
	tooManyAttempts := d.cfg.MaxAttempts > 0 && rec.job.Attempts > d.cfg.MaxAttempts
	if budgetExpired || tooManyAttempts {
		return d.concludeTimeout(rec, now, reply.Code, reply.Cause), nil
	}

	switch reply.Code {
	case CodeCoAAck, CodeDisconnectAck:
		rec.job.State = StateAcked
		rec.result = Result{
			JobID:     rec.job.ID,
			SessionID: rec.job.SessionID,
			State:     StateAcked,
			Code:      reply.Code,
			Attempts:  rec.job.Attempts,
			Elapsed:   elapsedTime,
		}
		rec.retryEligible = false
		d.removeFromOrder(jobID)
		d.stats.Acked++
		d.concludeSession(rec.job.Kind, rec.job.SessionID)
		return rec.result, nil

	case CodeCoANak, CodeDisconnectNak:
		d.stats.Nacked++
		retryable := reply.Cause == CauseResourcesUnavailable || reply.Cause == CauseRequestInitiated
		rec.result = Result{
			JobID:     rec.job.ID,
			SessionID: rec.job.SessionID,
			State:     StateNacked,
			Code:      reply.Code,
			Cause:     reply.Cause,
			Retryable: retryable,
			Attempts:  rec.job.Attempts,
			Elapsed:   elapsedTime,
		}
		if retryable {
			rec.job.State = StateQueued
			rec.job.NextAt = now.Add(backoffFor(d.cfg, rec.job.Attempts))
			rec.result.State = StateQueued
			rec.retryEligible = true
			if !d.orderContains(jobID) {
				d.insertInOrder(jobID, rec.priority)
			}
			d.stats.Retried++
		} else {
			rec.job.State = StateNacked
			rec.retryEligible = false
			d.removeFromOrder(jobID)
		}
		return rec.result, nil
	}

	return rec.result, ErrProtocolMismatch
}

// Tick dispatches every job that is due at the given instant.
func (d *RadiusCoaDispatcher) Tick(now time.Time) []Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := append([]string(nil), d.order...)
	dispatched := make([]Job, 0, len(ids))
	for _, id := range ids {
		rec, ok := d.jobs[id]
		if !ok || !isActive(rec.job.State) {
			continue
		}
		elapsedTime := elapsed(now, rec.createdAt)
		if rec.job.Budget > 0 && elapsedTime >= rec.job.Budget {
			d.concludeTimeout(rec, now, 0, 0)
			continue
		}
		if now.Before(rec.job.NextAt) {
			continue
		}
		if d.cfg.MaxAttempts > 0 && rec.job.Attempts >= d.cfg.MaxAttempts {
			d.concludeTimeout(rec, now, 0, 0)
			continue
		}

		rec.job.Attempts++
		rec.job.State = StateInflight
		rec.job.NextAt = now.Add(backoffFor(d.cfg, rec.job.Attempts))
		rec.result = Result{
			JobID:     rec.job.ID,
			SessionID: rec.job.SessionID,
			State:     StateInflight,
			Attempts:  rec.job.Attempts,
			Elapsed:   elapsedTime,
		}
		rec.retryEligible = false
		dispatched = append(dispatched, rec.job)
	}
	return dispatched
}

// SessionState reads the dispatcher view of one session.
func (d *RadiusCoaDispatcher) SessionState(sessionID string) (SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	session, ok := d.sessions[sessionID]
	if !ok {
		return SessionRecord{}, ErrUnknownSession
	}
	return session, nil
}

// Stats reports counters of the dispatcher.
func (d *RadiusCoaDispatcher) Stats() DispatcherStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}

// Snapshot serialises jobs, sessions and the sequence counter.
func (d *RadiusCoaDispatcher) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	jobs := make([]snapshotJob, 0, len(d.jobs))
	for _, rec := range d.jobs {
		jobs = append(jobs, snapshotJob{
			Job:           rec.job,
			Request:       cloneRequest(rec.request),
			Result:        rec.result,
			CreatedAt:     rec.createdAt,
			Priority:      rec.priority,
			RetryEligible: rec.retryEligible,
		})
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Job.Seq != jobs[j].Job.Seq {
			return jobs[i].Job.Seq < jobs[j].Job.Seq
		}
		return jobs[i].Job.ID < jobs[j].Job.ID
	})

	sessions := make([]SessionRecord, 0, len(d.sessions))
	for _, session := range d.sessions {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	order := append([]string(nil), d.order...)
	if order == nil {
		order = []string{}
	}
	data, err := json.Marshal(snapshotData{
		Version:  1,
		Jobs:     jobs,
		Order:    order,
		Sessions: sessions,
		Seq:      d.seq,
		Stats:    d.stats,
	})
	if err != nil {
		return nil, ErrBadSnapshot
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (d *RadiusCoaDispatcher) Restore(data []byte) error {
	var snapshot snapshotData
	if len(data) == 0 || json.Unmarshal(data, &snapshot) != nil || snapshot.Version != 1 ||
		snapshot.Jobs == nil || snapshot.Order == nil || snapshot.Sessions == nil {
		return ErrBadSnapshot
	}

	jobs := make(map[string]*jobState, len(snapshot.Jobs))
	maxSeq := 0
	for _, stored := range snapshot.Jobs {
		job := stored.Job
		if job.ID == "" || job.SessionID == "" || !validKind(job.Kind) || !validState(job.State) {
			return ErrBadSnapshot
		}
		if job.Seq < 0 || job.Attempts < 0 || stored.CreatedAt.IsZero() {
			return ErrBadSnapshot
		}
		if stored.Result.JobID != job.ID || stored.Result.SessionID != job.SessionID ||
			stored.Result.State != job.State || stored.Result.Attempts != job.Attempts {
			return ErrBadSnapshot
		}
		if _, exists := jobs[job.ID]; exists {
			return ErrBadSnapshot
		}
		jobs[job.ID] = &jobState{
			job:           job,
			request:       cloneRequest(stored.Request),
			result:        stored.Result,
			createdAt:     stored.CreatedAt,
			priority:      stored.Priority,
			retryEligible: stored.RetryEligible,
		}
		if job.Seq > maxSeq {
			maxSeq = job.Seq
		}
	}

	order := make([]string, 0, len(snapshot.Order))
	inOrder := make(map[string]struct{}, len(snapshot.Order))
	for _, id := range snapshot.Order {
		rec, ok := jobs[id]
		if !ok || !isActive(rec.job.State) {
			return ErrBadSnapshot
		}
		if _, duplicate := inOrder[id]; duplicate {
			return ErrBadSnapshot
		}
		order = append(order, id)
		inOrder[id] = struct{}{}
	}
	for id, rec := range jobs {
		if isActive(rec.job.State) {
			if _, ok := inOrder[id]; !ok {
				return ErrBadSnapshot
			}
		}
	}
	activeSessions := make(map[string]string, len(order))
	for _, id := range order {
		sessionID := jobs[id].job.SessionID
		if previous, exists := activeSessions[sessionID]; exists && previous != id {
			return ErrBadSnapshot
		}
		activeSessions[sessionID] = id
	}

	sessions := make(map[string]SessionRecord, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		if session.ID == "" || session.Jobs < 0 || !validSessionState(session.State) {
			return ErrBadSnapshot
		}
		if _, exists := sessions[session.ID]; exists {
			return ErrBadSnapshot
		}
		if _, exists := jobs[session.LastJob]; !exists && session.LastJob != "" {
			return ErrBadSnapshot
		}
		sessions[session.ID] = session
	}
	for _, rec := range jobs {
		if _, ok := sessions[rec.job.SessionID]; !ok {
			return ErrBadSnapshot
		}
	}
	if snapshot.Seq < 0 || snapshot.Seq < maxSeq || negativeStats(snapshot.Stats) {
		return ErrBadSnapshot
	}

	d.jobs = jobs
	d.order = order
	d.sessions = sessions
	d.seq = snapshot.Seq
	d.stats = snapshot.Stats
	return nil
}

func (d *RadiusCoaDispatcher) now() time.Time {
	return d.cfg.Now()
}

func (d *RadiusCoaDispatcher) nextJobID() string {
	for {
		d.seq++
		id := fmt.Sprintf("job-%d", d.seq)
		if _, exists := d.jobs[id]; !exists {
			return id
		}
	}
}

func (d *RadiusCoaDispatcher) insertInOrder(id string, priority int) {
	position := len(d.order)
	for i, queuedID := range d.order {
		rec, ok := d.jobs[queuedID]
		if ok && rec.priority < priority {
			position = i
			break
		}
	}
	d.order = append(d.order, "")
	copy(d.order[position+1:], d.order[position:])
	d.order[position] = id
}

func (d *RadiusCoaDispatcher) removeFromOrder(id string) {
	for i, queuedID := range d.order {
		if queuedID == id {
			d.order = append(d.order[:i], d.order[i+1:]...)
			return
		}
	}
}

func (d *RadiusCoaDispatcher) orderContains(id string) bool {
	for _, queuedID := range d.order {
		if queuedID == id {
			return true
		}
	}
	return false
}

func (d *RadiusCoaDispatcher) concludeTimeout(rec *jobState, now time.Time, code, cause int) Result {
	rec.job.State = StateTimeout
	rec.retryEligible = false
	rec.result = Result{
		JobID:     rec.job.ID,
		SessionID: rec.job.SessionID,
		State:     StateTimeout,
		Code:      code,
		Cause:     cause,
		Attempts:  rec.job.Attempts,
		Elapsed:   elapsed(now, rec.createdAt),
	}
	d.removeFromOrder(rec.job.ID)
	d.stats.TimedOut++
	return rec.result
}

func (d *RadiusCoaDispatcher) concludeSession(kind, sessionID string) {
	session, ok := d.sessions[sessionID]
	if !ok {
		return
	}
	switch kind {
	case KindDisconnect:
		session.State = SessionTerminated
	case KindCoA:
		session.State = SessionOnline
	}
	d.sessions[sessionID] = session
}

func backoffFor(cfg DispatcherConfig, attempts int) time.Duration {
	delay := cfg.BaseBackoff
	if delay < 0 {
		delay = 0
	}
	for i := 1; i < attempts; i++ {
		if cfg.MaxBackoff > 0 && delay >= cfg.MaxBackoff {
			delay = cfg.MaxBackoff
			break
		}
		delay *= 2
	}
	if cfg.MaxBackoff > 0 && delay > cfg.MaxBackoff {
		return cfg.MaxBackoff
	}
	return delay
}

func elapsed(now, started time.Time) time.Duration {
	if now.Before(started) {
		return 0
	}
	return now.Sub(started)
}

func isActive(state string) bool {
	return state == StateQueued || state == StateInflight
}

func isClosed(state string) bool {
	return state == StateAcked || state == StateNacked || state == StateTimeout || state == StateCanceled
}

func validKind(kind string) bool {
	return kind == KindCoA || kind == KindDisconnect
}

func validState(state string) bool {
	return state == StateQueued || state == StateInflight || isClosed(state)
}

func validSessionState(state string) bool {
	return state == SessionOnline || state == SessionTerminated
}

func replyMatches(kind string, code int) bool {
	switch kind {
	case KindCoA:
		return code == CodeCoAAck || code == CodeCoANak
	case KindDisconnect:
		return code == CodeDisconnectAck || code == CodeDisconnectNak
	default:
		return false
	}
}

func cloneAttributes(attrs []Attribute) []Attribute {
	if attrs == nil {
		return nil
	}
	cloned := make([]Attribute, len(attrs))
	copy(cloned, attrs)
	return cloned
}

func cloneRequest(req CoaRequest) CoaRequest {
	req.Attrs = cloneAttributes(req.Attrs)
	return req
}

func negativeStats(stats DispatcherStats) bool {
	return stats.Submitted < 0 || stats.Acked < 0 || stats.Nacked < 0 ||
		stats.TimedOut < 0 || stats.Canceled < 0 || stats.Retried < 0 ||
		stats.LateReplies < 0
}
