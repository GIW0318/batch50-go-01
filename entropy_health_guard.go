// Package task keeps entropy source credit, pool level and reseed state for
// a host that must never hand out weak randomness when the pool runs dry.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pool health states reported by Health.
const (
	StateOK       = "ok"
	StateDegraded = "degraded"
	StateStarved  = "starved"
)

// Reasons attached to a credit result.
const (
	ReasonAccepted  = "reason_accepted"
	ReasonDuplicate = "reason_duplicate"
)

// Credit accounting anchors.
const (
	CreditMax     = 100
	CreditStep    = 10
	CreditPenalty = 5
	CreditFloor   = 1
)

// Errors returned by the guard.
var (
	ErrBadSource   = errors.New("entropy source name is not usable")
	ErrBadSample   = errors.New("entropy sample is empty")
	ErrBadDraw     = errors.New("requested random byte count is not usable")
	ErrShortSeed   = errors.New("seed is shorter than the configured minimum")
	ErrPoolEmpty   = errors.New("entropy pool cannot satisfy the request")
	ErrBadSnapshot = errors.New("entropy snapshot is not usable")
)

const snapshotVersion = 1

// Options tunes the guard.
type Options struct {
	// MinSeedBytes is the smallest seed accepted by Reseed.
	MinSeedBytes int
	// BlockTimeout bounds how long a blocking Draw may wait.
	BlockTimeout time.Duration
}

// Credit is the credit ledger entry of one entropy source.
type Credit struct {
	Source   string `json:"source"`
	Score    int    `json:"score"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason"`
}

// Health is the pool and source summary returned by Health.
type Health struct {
	PoolBytes int    `json:"pool_bytes"`
	Sources   int    `json:"sources"`
	State     string `json:"state"`
}

// DrawRecord is one satisfied draw.
type DrawRecord struct {
	N       int  `json:"n"`
	Blocked bool `json:"blocked"`
}

type snapshotData struct {
	Version     int               `json:"version"`
	Pool        []byte            `json:"pool"`
	Credits     map[string]Credit `json:"credits"`
	LastSamples map[string][]byte `json:"last_samples"`
	Records     []DrawRecord      `json:"records"`
}

// EntropyHealthGuard owns the pool, the credit ledger and the draw log.
type EntropyHealthGuard struct {
	opts Options

	mu             sync.Mutex
	cond           *sync.Cond
	pool           []byte
	credits        map[string]Credit
	lastSamples    map[string][]byte
	records        []DrawRecord
	reseedRequired bool
}

// NewEntropyHealthGuard builds a guard with an empty pool.
func NewEntropyHealthGuard(opts Options) *EntropyHealthGuard {
	g := &EntropyHealthGuard{
		opts:           opts,
		credits:        make(map[string]Credit),
		lastSamples:    make(map[string][]byte),
		records:        make([]DrawRecord, 0),
		reseedRequired: false,
	}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Feed records one sample of an entropy source and its credit outcome.
func (g *EntropyHealthGuard) Feed(src string, sample []byte) (Credit, error) {
	if isBlankSource(src) {
		return Credit{}, ErrBadSource
	}
	if len(sample) == 0 {
		return Credit{}, ErrBadSample
	}

	incoming := bytes.Clone(sample)

	g.mu.Lock()
	defer g.mu.Unlock()

	current, known := g.credits[src]
	if known && bytes.Equal(g.lastSamples[src], incoming) {
		score := current.Score - CreditPenalty
		if score < CreditFloor {
			score = CreditFloor
		}
		credit := Credit{
			Source:   src,
			Score:    score,
			Accepted: false,
			Reason:   ReasonDuplicate,
		}
		g.credits[src] = credit
		return credit, nil
	}

	score := CreditStep
	if known {
		score = current.Score + CreditStep
		if score > CreditMax {
			score = CreditMax
		}
	}

	credit := Credit{
		Source:   src,
		Score:    score,
		Accepted: true,
		Reason:   ReasonAccepted,
	}
	g.credits[src] = credit
	g.lastSamples[src] = incoming
	g.pool = append(g.pool, incoming...)
	g.cond.Broadcast()

	return credit, nil
}

// Draw takes n bytes out of the pool, optionally waiting for entropy.
func (g *EntropyHealthGuard) Draw(n int, blocking bool) ([]byte, error) {
	if n <= 0 {
		return nil, ErrBadDraw
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !blocking {
		if !g.canDrawLocked(n) {
			return nil, ErrPoolEmpty
		}
		return g.drawLocked(n, false), nil
	}

	deadline := time.Now().Add(g.opts.BlockTimeout)
	timer := time.AfterFunc(g.opts.BlockTimeout, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer timer.Stop()

	for !g.canDrawLocked(n) {
		g.cond.Wait()
		if !g.canDrawLocked(n) && !time.Now().Before(deadline) {
			return nil, ErrPoolEmpty
		}
	}

	return g.drawLocked(n, true), nil
}

// Health summarises the pool level and the source credit.
func (g *EntropyHealthGuard) Health() Health {
	g.mu.Lock()
	defer g.mu.Unlock()

	health := Health{
		PoolBytes: len(g.pool),
		Sources:   len(g.credits),
		State:     StateStarved,
	}

	if !g.reseedRequired && health.PoolBytes > 0 {
		health.State = StateDegraded
		for _, credit := range g.credits {
			if weightForScore(credit.Score) >= 4 {
				health.State = StateOK
				break
			}
		}
	}

	return health
}

// Reseed replaces the pool with a fresh seed.
func (g *EntropyHealthGuard) Reseed(seed []byte) error {
	if len(seed) < g.opts.MinSeedBytes {
		return ErrShortSeed
	}

	freshSeed := bytes.Clone(seed)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.pool = freshSeed
	g.reseedRequired = false
	g.cond.Broadcast()
	return nil
}

// CreditOf reads the ledger entry of one source.
func (g *EntropyHealthGuard) CreditOf(src string) (Credit, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	credit, ok := g.credits[src]
	return credit, ok
}

// Weight maps the credit of one source onto its drawing weight.
func (g *EntropyHealthGuard) Weight(src string) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	credit, ok := g.credits[src]
	if !ok {
		return 0
	}
	return weightForScore(credit.Score)
}

// PoolBytes reports how many bytes the pool currently holds.
func (g *EntropyHealthGuard) PoolBytes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pool)
}

// Sources lists the known entropy sources in ascending order.
func (g *EntropyHealthGuard) Sources() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	sources := make([]string, 0, len(g.credits))
	for src := range g.credits {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	return sources
}

// Records returns the satisfied draws, oldest first.
func (g *EntropyHealthGuard) Records() []DrawRecord {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append(make([]DrawRecord, 0, len(g.records)), g.records...)
}

// Snapshot serialises pool, credit ledger and draw log.
func (g *EntropyHealthGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	snapshot := snapshotData{
		Version:     snapshotVersion,
		Pool:        bytes.Clone(g.pool),
		Credits:     make(map[string]Credit, len(g.credits)),
		LastSamples: make(map[string][]byte, len(g.lastSamples)),
		Records:     append(make([]DrawRecord, 0, len(g.records)), g.records...),
	}
	for src, credit := range g.credits {
		snapshot.Credits[src] = credit
	}
	for src, sample := range g.lastSamples {
		snapshot.LastSamples[src] = bytes.Clone(sample)
	}
	g.mu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode entropy snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *EntropyHealthGuard) Restore(data []byte) error {
	var snapshot snapshotData
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("%w: decode snapshot: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	credits := make(map[string]Credit, len(snapshot.Credits))
	lastSamples := make(map[string][]byte, len(snapshot.LastSamples))
	for src, credit := range snapshot.Credits {
		credit.Source = src
		credits[src] = credit
		lastSamples[src] = bytes.Clone(snapshot.LastSamples[src])
	}
	records := append(make([]DrawRecord, 0, len(snapshot.Records)), snapshot.Records...)
	pool := bytes.Clone(snapshot.Pool)

	g.mu.Lock()
	g.pool = pool
	g.credits = credits
	g.lastSamples = lastSamples
	g.records = records
	g.reseedRequired = true
	g.cond.Broadcast()
	g.mu.Unlock()

	return nil
}

func (g *EntropyHealthGuard) canDrawLocked(n int) bool {
	return !g.reseedRequired && len(g.pool) >= n
}

func (g *EntropyHealthGuard) drawLocked(n int, blocking bool) []byte {
	out := bytes.Clone(g.pool[:n])
	for i := 0; i < n; i++ {
		g.pool[i] = 0
	}
	g.pool = g.pool[n:]
	g.records = append(g.records, DrawRecord{N: n, Blocked: blocking})
	return out
}

func weightForScore(score int) int {
	switch {
	case score >= 90:
		return 8
	case score >= 70:
		return 6
	case score >= 50:
		return 4
	case score >= 20:
		return 2
	default:
		return 1
	}
}

func isBlankSource(src string) bool {
	return len(strings.TrimSpace(src)) == 0
}

func validateSnapshot(snapshot *snapshotData) error {
	if snapshot.Version != snapshotVersion {
		return errors.New("unsupported snapshot version")
	}

	for src, credit := range snapshot.Credits {
		if isBlankSource(src) {
			return errors.New("snapshot has a blank source")
		}
		if credit.Source != "" && credit.Source != src {
			return errors.New("snapshot credit source mismatch")
		}
		if credit.Score < CreditFloor || credit.Score > CreditMax {
			return errors.New("snapshot credit score is out of range")
		}
		switch credit.Reason {
		case ReasonAccepted:
			if !credit.Accepted {
				return errors.New("snapshot accepted credit is marked rejected")
			}
		case ReasonDuplicate:
			if credit.Accepted {
				return errors.New("snapshot duplicate credit is marked accepted")
			}
		default:
			return errors.New("snapshot credit has an unknown reason")
		}

		lastSample, ok := snapshot.LastSamples[src]
		if !ok || len(lastSample) == 0 {
			return errors.New("snapshot source is missing its last sample")
		}
	}

	for src, sample := range snapshot.LastSamples {
		if _, ok := snapshot.Credits[src]; !ok {
			return errors.New("snapshot last sample has no credit ledger entry")
		}
		if len(sample) == 0 {
			return errors.New("snapshot last sample is empty")
		}
	}

	for _, record := range snapshot.Records {
		if record.N <= 0 {
			return errors.New("snapshot draw record has a non-positive count")
		}
	}

	return nil
}
