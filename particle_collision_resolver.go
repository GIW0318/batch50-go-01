// Package task resolves particle collisions against scene proxies: depth
// buffer and SDF samples, bounce with friction and a per frame query budget.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

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

const (
	// penetrationLimit separates ordinary overlap correction from a tunneled
	// particle that should return to its position at the start of the frame.
	penetrationLimit = 1.0
	contactEpsilon   = 1e-12

	snapshotFormat  = "particle-collision-resolver-snapshot"
	snapshotVersion = 1
)

// ParticleCollisionResolver owns the proxy set, the response coefficients and
// the per frame query budget.
type ParticleCollisionResolver struct {
	mu           sync.RWMutex
	proxies      map[string]CollisionProxy
	deferredIDs  []int
	queryBudget  int
	frameQueries int
	restitution  float64
	friction     float64
	stats        CollisionStats
}

type contact struct {
	normal Vec3
	depth  float64
}

type snapshotEnvelope struct {
	Format   string          `json:"format"`
	Version  int             `json:"version"`
	State    json.RawMessage `json:"state"`
	Checksum string          `json:"checksum"`
}

type snapshotState struct {
	QueryBudget  int              `json:"query_budget"`
	Restitution  float64          `json:"restitution"`
	Friction     float64          `json:"friction"`
	Proxies      []CollisionProxy `json:"proxies"`
	DeferredIDs  []int            `json:"deferred_ids"`
	FrameQueries int              `json:"frame_queries"`
	Stats        CollisionStats   `json:"stats"`
}

// NewParticleCollisionResolver builds a resolver with a hard per frame query
// budget.
func NewParticleCollisionResolver(queryBudget int) (*ParticleCollisionResolver, error) {
	if queryBudget <= 0 {
		return nil, ErrBadBudget
	}

	return &ParticleCollisionResolver{
		proxies:     make(map[string]CollisionProxy),
		queryBudget: queryBudget,
		restitution: 0.5,
		friction:    0.25,
		stats: CollisionStats{
			Budget: queryBudget,
		},
	}, nil
}

// SetProxy adds or replaces one collision proxy by its id.
func (r *ParticleCollisionResolver) SetProxy(proxy CollisionProxy) error {
	if err := validateProxy(proxy); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.proxies == nil {
		r.proxies = make(map[string]CollisionProxy)
	}
	r.proxies[proxy.ID] = proxy
	return nil
}

// Proxies reports how many proxies are registered.
func (r *ParticleCollisionResolver) Proxies() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.proxies)
}

// Resolve integrates one frame and constrains every affordable particle to the
// outside of the proxies.
func (r *ParticleCollisionResolver) Resolve(batch []Particle, dt time.Duration) ([]Particle, error) {
	if dt < 0 {
		return nil, ErrBadDelta
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.frameQueries = 0
	r.stats.Frames++

	out := make([]Particle, len(batch))
	copy(out, batch)
	if len(batch) == 0 {
		return out, nil
	}

	proxyIDs := make([]string, 0, len(r.proxies))
	for id := range r.proxies {
		proxyIDs = append(proxyIDs, id)
	}
	sort.Strings(proxyIDs)

	deferredBefore := make(map[int]struct{}, len(r.deferredIDs))
	for _, id := range r.deferredIDs {
		deferredBefore[id] = struct{}{}
	}

	priority := make([]int, 0, len(batch))
	ordinary := make([]int, 0, len(batch))
	for i, particle := range batch {
		if _, ok := deferredBefore[particle.ID]; ok {
			priority = append(priority, i)
		} else {
			ordinary = append(ordinary, i)
		}
	}
	sort.SliceStable(priority, func(i, j int) bool {
		return batch[priority[i]].ID < batch[priority[j]].ID
	})
	order := append(priority, ordinary...)

	deferredAfter := make(map[int]struct{})
	seconds := float64(dt) / float64(time.Second)

	for _, index := range order {
		if r.frameQueries >= r.queryBudget {
			r.deferParticle(batch[index].ID, deferredAfter)
			continue
		}

		particle := out[index]
		previousPosition := particle.Pos
		particle.Pos = addVec(particle.Pos, scaleVec(particle.Vel, seconds))

		contacts := make([]contact, 0, len(proxyIDs))
		budgetExhausted := false
		for _, id := range proxyIDs {
			if r.frameQueries >= r.queryBudget {
				budgetExhausted = true
				break
			}

			r.frameQueries++
			r.stats.Queries++
			if current, hit := detectContact(particle.Pos, r.proxies[id]); hit {
				contacts = append(contacts, current)
			}
		}

		if budgetExhausted {
			r.deferParticle(batch[index].ID, deferredAfter)
			continue
		}

		for range contacts {
			r.stats.Hits++
		}
		if len(contacts) > 0 {
			deep := false
			for _, current := range contacts {
				if current.depth > penetrationLimit {
					deep = true
					break
				}
			}
			particle = resolveContacts(particle, contacts, r.restitution, r.friction)
			if deep {
				particle.Pos = previousPosition
			}
		}

		out[index] = particle
	}

	r.deferredIDs = sortedIDs(deferredAfter)
	return out, nil
}

// SetRestitution sets the normal bounce coefficient.
func (r *ParticleCollisionResolver) SetRestitution(v float64) error {
	if !inUnitRange(v) {
		return ErrBadRestitution
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.restitution = v
	return nil
}

// Restitution reports the normal bounce coefficient.
func (r *ParticleCollisionResolver) Restitution() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.restitution
}

// SetFriction sets the tangential damping coefficient.
func (r *ParticleCollisionResolver) SetFriction(v float64) error {
	if !inUnitRange(v) {
		return ErrBadFriction
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.friction = v
	return nil
}

// Friction reports the tangential damping coefficient.
func (r *ParticleCollisionResolver) Friction() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.friction
}

// Budget reports the queries used by the last frame and the configured limit.
func (r *ParticleCollisionResolver) Budget() (queries int, limit int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.frameQueries, r.queryBudget
}

// SetQueryBudget changes the per frame query budget.
func (r *ParticleCollisionResolver) SetQueryBudget(n int) error {
	if n <= 0 {
		return ErrBadBudget
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.queryBudget = n
	r.stats.Budget = n
	return nil
}

// Stats reports the accounting of the resolver.
func (r *ParticleCollisionResolver) Stats() CollisionStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := r.stats
	stats.Proxies = len(r.proxies)
	stats.Budget = r.queryBudget
	return stats
}

// Snapshot serialises the proxies, the coefficients and the budget counters.
func (r *ParticleCollisionResolver) Snapshot() ([]byte, error) {
	r.mu.RLock()
	state := snapshotState{
		QueryBudget:  r.queryBudget,
		Restitution:  r.restitution,
		Friction:     r.friction,
		Proxies:      make([]CollisionProxy, 0, len(r.proxies)),
		DeferredIDs:  append([]int(nil), r.deferredIDs...),
		FrameQueries: r.frameQueries,
		Stats:        r.stats,
	}
	for _, id := range sortedProxyIDs(r.proxies) {
		state.Proxies = append(state.Proxies, r.proxies[id])
	}
	r.mu.RUnlock()

	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(stateBytes)
	envelope := snapshotEnvelope{
		Format:   snapshotFormat,
		Version:  snapshotVersion,
		State:    stateBytes,
		Checksum: hex.EncodeToString(checksum[:]),
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (r *ParticleCollisionResolver) Restore(data []byte) error {
	state, err := decodeSnapshot(data)
	if err != nil {
		return err
	}

	proxies := make(map[string]CollisionProxy, len(state.Proxies))
	for _, proxy := range state.Proxies {
		if err := validateProxy(proxy); err != nil {
			return ErrBadSnapshot
		}
		if _, exists := proxies[proxy.ID]; exists {
			return ErrBadSnapshot
		}
		proxies[proxy.ID] = proxy
	}

	deferred := append([]int(nil), state.DeferredIDs...)
	sort.Ints(deferred)
	if len(deferred) > 1 {
		compact := deferred[:1]
		for _, id := range deferred[1:] {
			if id != compact[len(compact)-1] {
				compact = append(compact, id)
			}
		}
		deferred = compact
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.proxies = proxies
	r.deferredIDs = deferred
	r.queryBudget = state.QueryBudget
	r.frameQueries = state.FrameQueries
	r.restitution = state.Restitution
	r.friction = state.Friction
	r.stats = state.Stats
	r.stats.Budget = state.QueryBudget
	r.stats.Proxies = len(proxies)
	return nil
}

func (r *ParticleCollisionResolver) deferParticle(id int, deferred map[int]struct{}) {
	r.stats.Deferred++
	deferred[id] = struct{}{}
}

func decodeSnapshot(data []byte) (snapshotState, error) {
	var state snapshotState
	if len(data) == 0 {
		return state, ErrBadSnapshot
	}

	var envelope snapshotEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return state, ErrBadSnapshot
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return state, ErrBadSnapshot
	}
	if envelope.Format != snapshotFormat || envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return state, ErrBadSnapshot
	}

	checksum := sha256.Sum256(envelope.State)
	if hex.EncodeToString(checksum[:]) != envelope.Checksum {
		return state, ErrBadSnapshot
	}

	stateDecoder := json.NewDecoder(bytes.NewReader(envelope.State))
	stateDecoder.DisallowUnknownFields()
	if err := stateDecoder.Decode(&state); err != nil {
		return snapshotState{}, ErrBadSnapshot
	}
	if err := stateDecoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return snapshotState{}, ErrBadSnapshot
	}

	if state.QueryBudget <= 0 || !inUnitRange(state.Restitution) || !inUnitRange(state.Friction) {
		return snapshotState{}, ErrBadSnapshot
	}
	if state.FrameQueries < 0 || state.Stats.Queries < 0 || state.Stats.Hits < 0 ||
		state.Stats.Deferred < 0 || state.Stats.Frames < 0 {
		return snapshotState{}, ErrBadSnapshot
	}
	return state, nil
}

func validateProxy(proxy CollisionProxy) error {
	if strings.TrimSpace(proxy.ID) == "" {
		return ErrBadProxy
	}
	if !finiteVec(proxy.Normal) || !finiteVec(proxy.Center) || !finite(proxy.Offset) {
		return ErrBadProxy
	}

	switch proxy.Kind {
	case ProxyPlane:
		normalSquared := dotVec(proxy.Normal, proxy.Normal)
		if normalSquared <= 0 {
			return ErrBadProxy
		}
	case ProxySphere:
		if !finite(proxy.Radius) || proxy.Radius <= 0 {
			return ErrBadProxy
		}
	default:
		return ErrBadProxy
	}
	return nil
}

func detectContact(position Vec3, proxy CollisionProxy) (contact, bool) {
	switch proxy.Kind {
	case ProxyPlane:
		length := math.Sqrt(dotVec(proxy.Normal, proxy.Normal))
		normal := scaleVec(proxy.Normal, 1/length)
		distance := dotVec(position, normal) - proxy.Offset/length
		if distance >= -contactEpsilon {
			return contact{}, false
		}
		return contact{normal: normal, depth: -distance}, true

	case ProxySphere:
		offset := subVec(position, proxy.Center)
		distance := math.Sqrt(dotVec(offset, offset))
		if proxy.Radius-distance <= contactEpsilon {
			return contact{}, false
		}

		var normal Vec3
		if distance == 0 {
			normal = Vec3{X: 0, Y: 1, Z: 0}
		} else {
			normal = scaleVec(offset, 1/distance)
		}
		return contact{normal: normal, depth: proxy.Radius - distance}, true

	default:
		return contact{}, false
	}
}

func resolveContacts(particle Particle, contacts []contact, restitution, friction float64) Particle {
	primary := contacts[0]
	for _, current := range contacts[1:] {
		if current.depth < primary.depth {
			primary = current
		}
	}

	// The closest surface owns the positional correction. This keeps a corner
	// from alternating which face pushes the particle out.
	particle.Pos = addVec(particle.Pos, scaleVec(primary.normal, primary.depth))

	incomingNormal := dotVec(particle.Vel, primary.normal)
	normalVelocity := scaleVec(primary.normal, incomingNormal)
	tangent := subVec(particle.Vel, normalVelocity)
	tangent = scaleVec(tangent, 1-friction)
	if incomingNormal < 0 {
		normalVelocity = scaleVec(primary.normal, -incomingNormal*restitution)
	}
	particle.Vel = addVec(tangent, normalVelocity)

	// A particle gets at most one restitution bounce in a frame. Remaining
	// faces only remove velocity directed into them, making the particle slide.
	for _, current := range contacts {
		if current.normal == primary.normal {
			continue
		}
		inward := dotVec(particle.Vel, current.normal)
		if inward < 0 {
			particle.Vel = subVec(particle.Vel, scaleVec(current.normal, inward))
		}
	}
	return particle
}

func sortedProxyIDs(proxies map[string]CollisionProxy) []string {
	ids := make([]string, 0, len(proxies))
	for id := range proxies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedIDs(values map[int]struct{}) []int {
	ids := make([]int, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func inUnitRange(v float64) bool {
	return !math.IsNaN(v) && v >= 0 && v <= 1
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func finiteVec(v Vec3) bool {
	return finite(v.X) && finite(v.Y) && finite(v.Z)
}

func addVec(a, b Vec3) Vec3 {
	return Vec3{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z}
}

func subVec(a, b Vec3) Vec3 {
	return Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}

func scaleVec(a Vec3, scalar float64) Vec3 {
	return Vec3{X: a.X * scalar, Y: a.Y * scalar, Z: a.Z * scalar}
}

func dotVec(a, b Vec3) float64 {
	return a.X*b.X + a.Y*b.Y + a.Z*b.Z
}
