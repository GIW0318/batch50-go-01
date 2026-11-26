// Package task resolves particle collisions against scene proxies: depth
// buffer and SDF samples, bounce with friction and a per frame query budget.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Vec3 is a three component vector.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// ProxyKind selects how a collision proxy is sampled.
type ProxyKind int

// Supported proxy kinds.
const (
	ProxyPlane ProxyKind = iota
	ProxySphere
)

// CollisionProxy is one scene surface the particles collide with. A plane is
// outside the solid where dot(position, Normal) >= Offset. A sphere is outside
// where the distance to Center is at least Radius.
type CollisionProxy struct {
	ID     string    `json:"id"`
	Kind   ProxyKind `json:"kind"`
	Normal Vec3      `json:"normal"`
	Offset float64   `json:"offset"`
	Center Vec3      `json:"center"`
	Radius float64   `json:"radius"`
}

// Particle is one simulated particle. ID is stable across frames.
type Particle struct {
	ID  int  `json:"id"`
	Pos Vec3 `json:"pos"`
	Vel Vec3 `json:"vel"`
}

// CollisionStats is the accounting view of the resolver.
type CollisionStats struct {
	Proxies  int   `json:"proxies"`
	Queries  int   `json:"queries"`
	Budget   int   `json:"budget"`
	Hits     int   `json:"hits"`
	Deferred int   `json:"deferred"`
	Frames   int64 `json:"frames"`
}

// Errors returned by the resolver.
var (
	ErrBadProxy       = errors.New("particle resolver: collision proxy is invalid")
	ErrBadRestitution = errors.New("particle resolver: restitution must be within 0 and 1")
	ErrBadFriction    = errors.New("particle resolver: friction must be within 0 and 1")
	ErrBadBudget      = errors.New("particle resolver: query budget must be positive")
	ErrBadDelta       = errors.New("particle resolver: delta must not be negative")
	ErrBadSnapshot    = errors.New("particle resolver: snapshot payload is invalid")
)

// ParticleCollisionResolver owns the proxy set, the response coefficients and
// the per frame query budget.
type ParticleCollisionResolver struct {
	queryBudget int
	restitution float64
	friction    float64
}

// NewParticleCollisionResolver builds a resolver with a hard per frame query
// budget.
func NewParticleCollisionResolver(queryBudget int) (*ParticleCollisionResolver, error) {
	return nil, errNotImplemented
}

// SetProxy adds or replaces one collision proxy by its id.
func (r *ParticleCollisionResolver) SetProxy(proxy CollisionProxy) error {
	return errNotImplemented
}

// Proxies reports how many proxies are registered.
func (r *ParticleCollisionResolver) Proxies() int {
	return 0
}

// Resolve integrates one frame and constrains every affordable particle to the
// outside of the proxies.
func (r *ParticleCollisionResolver) Resolve(batch []Particle, dt time.Duration) ([]Particle, error) {
	return nil, errNotImplemented
}

// SetRestitution sets the normal bounce coefficient.
func (r *ParticleCollisionResolver) SetRestitution(v float64) error {
	return errNotImplemented
}

// Restitution reports the normal bounce coefficient.
func (r *ParticleCollisionResolver) Restitution() float64 {
	return 0
}

// SetFriction sets the tangential damping coefficient.
func (r *ParticleCollisionResolver) SetFriction(v float64) error {
	return errNotImplemented
}

// Friction reports the tangential damping coefficient.
func (r *ParticleCollisionResolver) Friction() float64 {
	return 0
}

// Budget reports the queries used by the last frame and the configured limit.
func (r *ParticleCollisionResolver) Budget() (queries int, limit int) {
	return 0, 0
}

// SetQueryBudget changes the per frame query budget.
func (r *ParticleCollisionResolver) SetQueryBudget(n int) error {
	return errNotImplemented
}

// Stats reports the accounting of the resolver.
func (r *ParticleCollisionResolver) Stats() CollisionStats {
	return CollisionStats{}
}

// Snapshot serialises the proxies, the coefficients and the budget counters.
func (r *ParticleCollisionResolver) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *ParticleCollisionResolver) Restore(data []byte) error {
	return errNotImplemented
}
