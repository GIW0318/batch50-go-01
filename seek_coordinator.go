// Package task coordinates player seek requests: it anchors each request on the
// nearest keyframe before the target, decodes forward inside a frame budget,
// reuses cached GOPs and keeps exactly one request able to deliver a frame.
package task

import (
	"errors"
	"sync"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Defaults applied when a limit is left at zero.
const (
	DefaultBudget     = 64
	DefaultCacheBytes = 1 << 20
	MaxBudget         = 4096
)

// RequestID identifies one seek request.
type RequestID int64

// Errors reported by the coordinator.
var (
	ErrBadTarget      = errors.New("seek target is out of range")
	ErrNotSeekable    = errors.New("no keyframe range covers the target")
	ErrUnknownRequest = errors.New("unknown seek request")
	ErrRequestClosed  = errors.New("seek request is already closed")
	ErrSuperseded     = errors.New("seek request was superseded")
	ErrBadBudget      = errors.New("decode budget is out of range")
	ErrBadGOP         = errors.New("group of pictures is not usable")
	ErrGOPTooLarge    = errors.New("group of pictures does not fit the cache")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Frame is the frame a seek landed on. Exact is false when the decode budget
// ran out before the requested frame.
type Frame struct {
	Index       int64         `json:"index"`
	PTS         time.Duration `json:"pts"`
	AnchorIndex int64         `json:"anchor_index"`
	Decoded     int           `json:"decoded"`
	Exact       bool          `json:"exact"`
	Request     RequestID     `json:"request"`
}

// GOPInfo describes one group of pictures that may be decoded from its keyframe.
type GOPInfo struct {
	KeyIndex int64         `json:"key_index"`
	KeyPTS   time.Duration `json:"key_pts"`
	FrameDur time.Duration `json:"frame_dur"`
	Frames   int           `json:"frames"`
	Bytes    int           `json:"bytes"`
}

// SeekStats counts what the coordinator did over its lifetime.
type SeekStats struct {
	Requests   int `json:"requests"`
	Delivered  int `json:"delivered"`
	Cancelled  int `json:"cancelled"`
	Superseded int `json:"superseded"`
	Decoded    int `json:"decoded"`
	CacheHits  int `json:"cache_hits"`
	Evicted    int `json:"evicted"`
}

// Limits carries the per request decode budget and the cache byte cap.
type Limits struct {
	Budget     int
	CacheBytes int
}

// SeekCoordinator owns the request table, the GOP cache and the play position.
type SeekCoordinator struct {
	mu       sync.Mutex
	budget   int
	cacheCap int
	seq      int64
	position time.Duration
	cache    []GOPInfo
	requests map[RequestID]*request
	stats    SeekStats
}

// request is the bookkeeping entry of one seek.
type request struct {
	id     RequestID
	target time.Duration
	gop    GOPInfo
	anchor int64
	landed Frame
	state  string
}

// NewSeekCoordinator builds a coordinator with the given caps.
func NewSeekCoordinator(limits Limits) *SeekCoordinator {
	return &SeekCoordinator{budget: limits.Budget, cacheCap: limits.CacheBytes}
}

// Seek registers a new jump target and supersedes the requests still pending.
func (c *SeekCoordinator) Seek(target time.Duration) (RequestID, error) {
	return 0, errNotImplemented
}

// Await delivers the frame the request landed on.
func (c *SeekCoordinator) Await(id RequestID) (Frame, error) {
	return Frame{}, errNotImplemented
}

// Cancel closes a request that has not been awaited yet.
func (c *SeekCoordinator) Cancel(id RequestID) error {
	return errNotImplemented
}

// SetBudget changes how many frames one request may decode.
func (c *SeekCoordinator) SetBudget(frames int) error {
	return errNotImplemented
}

// CacheGOP registers one group of pictures for reuse.
func (c *SeekCoordinator) CacheGOP(gop GOPInfo) error {
	return errNotImplemented
}

// Position reports the play position of the last delivered frame.
func (c *SeekCoordinator) Position() time.Duration {
	return 0
}

// CacheSize reports the bytes held by the GOP cache.
func (c *SeekCoordinator) CacheSize() int {
	return 0
}

// Stats reads the lifetime counters.
func (c *SeekCoordinator) Stats() SeekStats {
	return SeekStats{}
}

// Snapshot serialises the play position, the GOP cache and the request counter.
func (c *SeekCoordinator) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *SeekCoordinator) Restore(data []byte) error {
	return errNotImplemented
}
