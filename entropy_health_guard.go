// Package task keeps entropy source credit, pool level and reseed state for
// a host that must never hand out weak randomness when the pool runs dry.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks every behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

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

// EntropyHealthGuard owns the pool, the credit ledger and the draw log.
type EntropyHealthGuard struct {
	opts Options
}

// NewEntropyHealthGuard builds a guard with an empty pool.
func NewEntropyHealthGuard(opts Options) *EntropyHealthGuard {
	return &EntropyHealthGuard{opts: opts}
}

// Feed records one sample of an entropy source and its credit outcome.
func (g *EntropyHealthGuard) Feed(src string, sample []byte) (Credit, error) {
	return Credit{}, errNotImplemented
}

// Draw takes n bytes out of the pool, optionally waiting for entropy.
func (g *EntropyHealthGuard) Draw(n int, blocking bool) ([]byte, error) {
	return nil, errNotImplemented
}

// Health summarises the pool level and the source credit.
func (g *EntropyHealthGuard) Health() Health {
	return Health{}
}

// Reseed replaces the pool with a fresh seed.
func (g *EntropyHealthGuard) Reseed(seed []byte) error {
	return errNotImplemented
}

// CreditOf reads the ledger entry of one source.
func (g *EntropyHealthGuard) CreditOf(src string) (Credit, bool) {
	return Credit{}, false
}

// Weight maps the credit of one source onto its drawing weight.
func (g *EntropyHealthGuard) Weight(src string) int {
	return 0
}

// PoolBytes reports how many bytes the pool currently holds.
func (g *EntropyHealthGuard) PoolBytes() int {
	return 0
}

// Sources lists the known entropy sources in ascending order.
func (g *EntropyHealthGuard) Sources() []string {
	return nil
}

// Records returns the satisfied draws, oldest first.
func (g *EntropyHealthGuard) Records() []DrawRecord {
	return nil
}

// Snapshot serialises pool, credit ledger and draw log.
func (g *EntropyHealthGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *EntropyHealthGuard) Restore(data []byte) error {
	return errNotImplemented
}
