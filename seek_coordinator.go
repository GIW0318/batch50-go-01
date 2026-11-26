// Package task coordinates player seek requests: it anchors each request on the
// nearest keyframe before the target, decodes forward inside a frame budget,
// reuses cached GOPs and keeps exactly one request able to deliver a frame.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Defaults applied when a limit is left at zero.
const (
	DefaultBudget     = 64
	DefaultCacheBytes = 1 << 20
	MaxBudget         = 4096

	snapshotMagic   = "gsb-batch50-go-game-c-15-seek-coordinator"
	snapshotVersion = 1
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

type requestState uint8

const (
	requestPending requestState = iota
	requestDecoding
	requestDelivered
	requestCanceled
	requestSuperseded
	requestFailed
)

type cacheEntry struct {
	gop  GOPInfo
	id   int64
	refs int
}

type decodeAnchor struct {
	index    int64
	pts      time.Duration
	frameDur time.Duration
	inexact  bool
}

type decodePlan struct {
	id          RequestID
	epoch       int64
	cacheID     int64
	gop         GOPInfo
	anchorIndex int64
	startIndex  int64
	targetIndex int64
	steps       int64
	budget      int
}

// request is the bookkeeping entry of one seek.
type request struct {
	id     RequestID
	target time.Duration
	state  requestState
	done   chan struct{}
	once   sync.Once
	frame  Frame
	err    error
}

// SeekCoordinator owns the request table, the GOP cache and the play position.
type SeekCoordinator struct {
	mu         sync.Mutex
	budget     int
	cacheCap   int
	seq        RequestID
	cacheSeq   int64
	epoch      int64
	position   time.Duration
	anchor     *decodeAnchor
	cache      []cacheEntry
	cacheBytes int
	requests   map[RequestID]*request
	stats      SeekStats
}

type snapshotEnvelope struct {
	Magic    string          `json:"magic"`
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotAnchor struct {
	Index    int64 `json:"index"`
	PTS      int64 `json:"pts_ns"`
	FrameDur int64 `json:"frame_dur_ns"`
	Inexact  bool  `json:"inexact"`
}

type snapshotState struct {
	Budget     int             `json:"budget"`
	CacheCap   int             `json:"cache_bytes"`
	Seq        int64           `json:"seq"`
	Epoch      int64           `json:"epoch"`
	Position   int64           `json:"position_ns"`
	Anchor     *snapshotAnchor `json:"anchor,omitempty"`
	Cache      []GOPInfo       `json:"cache"`
	CacheBytes int             `json:"cache_size"`
	Stats      SeekStats       `json:"stats"`
}

// NewSeekCoordinator builds a coordinator with the given caps.
func NewSeekCoordinator(limits Limits) *SeekCoordinator {
	budget := limits.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	if budget > MaxBudget {
		budget = MaxBudget
	}

	cacheCap := limits.CacheBytes
	if cacheCap <= 0 {
		cacheCap = DefaultCacheBytes
	}

	return &SeekCoordinator{
		budget:   budget,
		cacheCap: cacheCap,
		requests: make(map[RequestID]*request),
		cache:    make([]cacheEntry, 0),
	}
}

// Seek registers a new jump target and supersedes the requests still pending.
func (c *SeekCoordinator) Seek(target time.Duration) (RequestID, error) {
	if target < 0 {
		return 0, ErrBadTarget
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, _, _, err := c.selectGOPLocked(target); err != nil {
		return 0, err
	}

	c.seq++
	id := c.seq

	for _, old := range c.requests {
		if old.state == requestPending || old.state == requestDecoding {
			old.state = requestSuperseded
			old.err = ErrSuperseded
			old.closeDone()
			c.stats.Superseded++
		}
	}

	c.requests[id] = &request{
		id:     id,
		target: target,
		state:  requestPending,
		done:   make(chan struct{}),
	}
	c.stats.Requests++

	return id, nil
}

// Await delivers the frame the request landed on.
func (c *SeekCoordinator) Await(id RequestID) (Frame, error) {
	c.mu.Lock()

	for {
		req, ok := c.requests[id]
		if !ok {
			c.mu.Unlock()
			return Frame{}, ErrUnknownRequest
		}

		switch req.state {
		case requestDelivered:
			frame := req.frame
			c.mu.Unlock()
			return frame, nil
		case requestCanceled:
			c.mu.Unlock()
			return Frame{}, ErrRequestClosed
		case requestSuperseded:
			c.mu.Unlock()
			return Frame{}, ErrSuperseded
		case requestFailed:
			err := req.err
			c.mu.Unlock()
			if err == nil {
				err = ErrNotSeekable
			}
			return Frame{}, err
		case requestDecoding:
			done := req.done
			c.mu.Unlock()
			<-done
			c.mu.Lock()
			continue
		}

		plan, err := c.planLocked(id, req.target)
		if err != nil {
			req.state = requestFailed
			req.err = err
			req.closeDone()
			c.mu.Unlock()
			return Frame{}, err
		}
		req.state = requestDecoding

		c.mu.Unlock()
		frame := runDecodePlan(plan)
		return c.finishDecode(id, plan, frame)
	}
}

// Cancel closes a request that has not been awaited yet.
func (c *SeekCoordinator) Cancel(id RequestID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req, ok := c.requests[id]
	if !ok {
		return ErrUnknownRequest
	}

	switch req.state {
	case requestPending, requestDecoding:
		req.state = requestCanceled
		req.err = ErrRequestClosed
		req.closeDone()
		c.stats.Cancelled++
		return nil
	case requestCanceled:
		return ErrRequestClosed
	case requestSuperseded:
		return errors.Join(ErrRequestClosed, ErrSuperseded)
	default:
		return ErrRequestClosed
	}
}

// SetBudget changes how many frames one request may decode.
func (c *SeekCoordinator) SetBudget(frames int) error {
	if frames < 1 || frames > MaxBudget {
		return ErrBadBudget
	}

	c.mu.Lock()
	c.budget = frames
	c.mu.Unlock()
	return nil
}

// CacheGOP registers one group of pictures for reuse.
func (c *SeekCoordinator) CacheGOP(gop GOPInfo) error {
	if err := validateGOP(gop); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if gop.Bytes > c.cacheCap {
		return ErrGOPTooLarge
	}

	c.cacheSeq++
	c.cache = append(c.cache, cacheEntry{gop: gop, id: c.cacheSeq})
	c.cacheBytes += gop.Bytes
	c.evictLocked()
	return nil
}

// Position reports the play position of the last delivered frame.
func (c *SeekCoordinator) Position() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.position
}

// CacheSize reports the bytes held by the GOP cache.
func (c *SeekCoordinator) CacheSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cacheBytes
}

// Stats reads the lifetime counters.
func (c *SeekCoordinator) Stats() SeekStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Snapshot serialises the play position, the GOP cache and the request counter.
func (c *SeekCoordinator) Snapshot() ([]byte, error) {
	c.mu.Lock()
	state := snapshotState{
		Budget:     c.budget,
		CacheCap:   c.cacheCap,
		Seq:        int64(c.seq),
		Epoch:      c.epoch,
		Position:   int64(c.position),
		Cache:      make([]GOPInfo, 0, len(c.cache)),
		CacheBytes: c.cacheBytes,
		Stats:      c.stats,
	}
	if c.anchor != nil {
		state.Anchor = &snapshotAnchor{
			Index:    c.anchor.index,
			PTS:      int64(c.anchor.pts),
			FrameDur: int64(c.anchor.frameDur),
			Inexact:  c.anchor.inexact,
		}
	}
	for _, entry := range c.cache {
		state.Cache = append(state.Cache, entry.gop)
	}
	c.mu.Unlock()

	rawState, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("%w: encode state: %v", ErrBadSnapshot, err)
	}
	checksum := sha256.Sum256(rawState)

	file := snapshotEnvelope{
		Magic:    snapshotMagic,
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(checksum[:]),
		State:    rawState,
	}
	data, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("%w: encode envelope: %v", ErrBadSnapshot, err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (c *SeekCoordinator) Restore(data []byte) error {
	state, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, old := range c.requests {
		if old.state == requestPending || old.state == requestDecoding {
			old.state = requestCanceled
			old.err = ErrRequestClosed
			old.closeDone()
		}
	}

	c.epoch++
	c.budget = state.Budget
	c.cacheCap = state.CacheCap
	c.seq = RequestID(state.Seq)
	c.position = time.Duration(state.Position)
	c.stats = state.Stats
	c.cacheBytes = 0
	c.cacheSeq = int64(len(state.Cache))
	c.cache = make([]cacheEntry, 0, len(state.Cache))
	for i, gop := range state.Cache {
		c.cache = append(c.cache, cacheEntry{gop: gop, id: int64(i + 1)})
		c.cacheBytes += gop.Bytes
	}
	c.requests = make(map[RequestID]*request)

	if state.Anchor != nil {
		c.anchor = &decodeAnchor{
			index:    state.Anchor.Index,
			pts:      time.Duration(state.Anchor.PTS),
			frameDur: time.Duration(state.Anchor.FrameDur),
			inexact:  state.Anchor.Inexact,
		}
	} else {
		c.anchor = nil
	}

	return nil
}

func (r *request) closeDone() {
	r.once.Do(func() {
		close(r.done)
	})
}

func validateGOP(gop GOPInfo) error {
	if gop.KeyIndex < 0 || gop.KeyPTS < 0 || gop.FrameDur <= 0 || gop.Frames <= 0 || gop.Bytes <= 0 {
		return ErrBadGOP
	}
	return nil
}

func (c *SeekCoordinator) selectGOPLocked(target time.Duration) (GOPInfo, int64, int64, error) {
	var selected GOPInfo
	found := false
	selectedCacheID := int64(0)

	for i := range c.cache {
		gop := c.cache[i].gop
		if target < gop.KeyPTS {
			continue
		}

		offset := int64((target - gop.KeyPTS) / gop.FrameDur)
		if offset < 0 || offset >= int64(gop.Frames) {
			continue
		}

		if !found || gop.KeyPTS > selected.KeyPTS ||
			(gop.KeyPTS == selected.KeyPTS && gop.KeyIndex > selected.KeyIndex) {
			selected = gop
			selectedCacheID = c.cache[i].id
			found = true
		}
	}

	if !found {
		return GOPInfo{}, 0, 0, ErrNotSeekable
	}

	offset := int64((target - selected.KeyPTS) / selected.FrameDur)
	targetIndex := selected.KeyIndex + offset
	return selected, targetIndex, selectedCacheID, nil
}

func (c *SeekCoordinator) planLocked(id RequestID, target time.Duration) (decodePlan, error) {
	gop, targetIndex, cacheID, err := c.selectGOPLocked(target)
	if err != nil {
		return decodePlan{}, err
	}

	for i := range c.cache {
		if c.cache[i].id == cacheID {
			c.cache[i].refs++
			c.stats.CacheHits++
			break
		}
	}

	anchorIndex := gop.KeyIndex
	startIndex := gop.KeyIndex
	if c.anchor != nil && c.anchor.inexact && c.canContinueFromAnchorLocked(gop) && c.anchor.index <= targetIndex {
		anchorIndex = c.anchor.index
		startIndex = c.anchor.index
	}

	steps := targetIndex - startIndex
	if steps < 0 {
		anchorIndex = gop.KeyIndex
		startIndex = gop.KeyIndex
		steps = targetIndex - startIndex
	}

	return decodePlan{
		id:          id,
		epoch:       c.epoch,
		cacheID:     cacheID,
		gop:         gop,
		anchorIndex: anchorIndex,
		startIndex:  startIndex,
		targetIndex: targetIndex,
		steps:       steps,
		budget:      c.budget,
	}, nil
}

func (c *SeekCoordinator) canContinueFromAnchorLocked(gop GOPInfo) bool {
	anchor := c.anchor
	if anchor == nil || !anchor.inexact {
		return false
	}
	if anchor.index < gop.KeyIndex || anchor.index >= gop.KeyIndex+int64(gop.Frames) {
		return false
	}
	expectedPTS := gop.KeyPTS + time.Duration(anchor.index-gop.KeyIndex)*gop.FrameDur
	return anchor.pts == expectedPTS && anchor.frameDur == gop.FrameDur
}

func runDecodePlan(plan decodePlan) Frame {
	decoded := plan.steps
	exact := true
	if decoded > int64(plan.budget) {
		decoded = int64(plan.budget)
		exact = false
	}

	index := plan.startIndex + decoded
	pts := plan.gop.KeyPTS + time.Duration(index-plan.gop.KeyIndex)*plan.gop.FrameDur

	return Frame{
		Index:       index,
		PTS:         pts,
		AnchorIndex: plan.anchorIndex,
		Decoded:     int(decoded),
		Exact:       exact,
		Request:     plan.id,
	}
}

func (c *SeekCoordinator) finishDecode(id RequestID, plan decodePlan, frame Frame) (Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if plan.epoch != c.epoch {
		return Frame{}, ErrRequestClosed
	}

	c.stats.Decoded += frame.Decoded
	for i := range c.cache {
		if c.cache[i].id == plan.cacheID {
			if c.cache[i].refs > 0 {
				c.cache[i].refs--
			}
			break
		}
	}
	c.evictLocked()

	req, ok := c.requests[id]
	if !ok {
		return Frame{}, ErrUnknownRequest
	}

	switch req.state {
	case requestCanceled:
		return Frame{}, ErrRequestClosed
	case requestSuperseded:
		return Frame{}, ErrSuperseded
	case requestDecoding:
		frame.Request = id
		req.frame = frame
		req.state = requestDelivered
		req.closeDone()

		c.stats.Delivered++
		c.position = frame.PTS
		if frame.Exact {
			c.anchor = nil
		} else {
			c.anchor = &decodeAnchor{
				index:    frame.Index,
				pts:      frame.PTS,
				frameDur: plan.gop.FrameDur,
				inexact:  true,
			}
		}
		return frame, nil
	default:
		return Frame{}, ErrRequestClosed
	}
}

func (c *SeekCoordinator) evictLocked() {
	for c.cacheBytes > c.cacheCap {
		oldestEligible := -1
		for i := range c.cache {
			if c.cache[i].refs == 0 {
				oldestEligible = i
				break
			}
		}
		if oldestEligible == -1 {
			return
		}

		removed := c.cache[oldestEligible]
		c.cache = append(c.cache[:oldestEligible], c.cache[oldestEligible+1:]...)
		c.cacheBytes -= removed.gop.Bytes
		c.stats.Evicted++
	}
}

func decodeSnapshot(data []byte) (snapshotState, error) {
	var file snapshotEnvelope
	if err := json.Unmarshal(data, &file); err != nil {
		return snapshotState{}, fmt.Errorf("%w: malformed json", ErrBadSnapshot)
	}
	if file.Magic != snapshotMagic || file.Version != snapshotVersion || len(file.State) == 0 {
		return snapshotState{}, fmt.Errorf("%w: unsupported envelope", ErrBadSnapshot)
	}

	expectedChecksum := sha256.Sum256(file.State)
	actualChecksum, err := hex.DecodeString(file.Checksum)
	if err != nil || len(actualChecksum) != sha256.Size ||
		hex.EncodeToString(actualChecksum) != hex.EncodeToString(expectedChecksum[:]) {
		return snapshotState{}, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	var state snapshotState
	if err := json.Unmarshal(file.State, &state); err != nil {
		return snapshotState{}, fmt.Errorf("%w: malformed state", ErrBadSnapshot)
	}
	if err := validateSnapshotState(state); err != nil {
		return snapshotState{}, err
	}
	return state, nil
}

func validateSnapshotState(state snapshotState) error {
	if state.Budget < 1 || state.Budget > MaxBudget {
		return fmt.Errorf("%w: bad budget", ErrBadSnapshot)
	}
	if state.CacheCap <= 0 || state.Seq < 0 || state.Position < 0 || state.Epoch < 0 {
		return fmt.Errorf("%w: bad scalar", ErrBadSnapshot)
	}
	if state.Stats.Requests < 0 || state.Stats.Delivered < 0 || state.Stats.Cancelled < 0 ||
		state.Stats.Superseded < 0 || state.Stats.Decoded < 0 || state.Stats.CacheHits < 0 ||
		state.Stats.Evicted < 0 {
		return fmt.Errorf("%w: bad stats", ErrBadSnapshot)
	}
	if state.Anchor != nil && (state.Anchor.Index < 0 || state.Anchor.PTS < 0 || state.Anchor.FrameDur <= 0) {
		return fmt.Errorf("%w: bad anchor", ErrBadSnapshot)
	}

	totalBytes := 0
	for _, gop := range state.Cache {
		if err := validateGOP(gop); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
		}
		totalBytes += gop.Bytes
	}
	if totalBytes != state.CacheBytes || totalBytes > state.CacheCap {
		return fmt.Errorf("%w: cache accounting mismatch", ErrBadSnapshot)
	}
	return nil
}
