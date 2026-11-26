package task

import (
	"errors"
	"math"
	"testing"
	"time"

	impl "gsb/batch50-go-game-b-20"
)

func newResolver(t *testing.T, budget int) *impl.ParticleCollisionResolver {
	t.Helper()
	r, err := impl.NewParticleCollisionResolver(budget)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	if r == nil {
		t.Fatalf("new resolver returned nil without an error")
	}
	return r
}

// floorPlane keeps everything with y >= 0 outside the solid.
func floorPlane() impl.CollisionProxy {
	return impl.CollisionProxy{
		ID:     "floor",
		Kind:   impl.ProxyPlane,
		Normal: impl.Vec3{X: 0, Y: 1, Z: 0},
		Offset: 0,
	}
}

// farWall keeps everything with y >= -100 outside, so particles near the
// origin never touch it.
func farWall() impl.CollisionProxy {
	return impl.CollisionProxy{
		ID:     "wall",
		Kind:   impl.ProxyPlane,
		Normal: impl.Vec3{X: 0, Y: 1, Z: 0},
		Offset: -100,
	}
}

// unitBall is a sphere of radius 1 at the origin.
func unitBall() impl.CollisionProxy {
	return impl.CollisionProxy{
		ID:     "ball",
		Kind:   impl.ProxySphere,
		Center: impl.Vec3{X: 0, Y: 0, Z: 0},
		Radius: 1,
	}
}

func newParticle(id int, pos, vel impl.Vec3) impl.Particle {
	return impl.Particle{ID: id, Pos: pos, Vel: vel}
}

func near(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func vecNear(a, b impl.Vec3) bool {
	return near(a.X, b.X) && near(a.Y, b.Y) && near(a.Z, b.Z)
}

func TestNewResolverRejectsBadBudget(t *testing.T) {
	if _, err := impl.NewParticleCollisionResolver(0); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("zero query budget = %v, want ErrBadBudget", err)
	}
	if _, err := impl.NewParticleCollisionResolver(-4); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("negative query budget = %v, want ErrBadBudget", err)
	}
	if _, err := impl.NewParticleCollisionResolver(64); err != nil {
		t.Fatalf("valid budget rejected: %v", err)
	}
}

func TestSetProxyRejectsBadProxy(t *testing.T) {
	r := newResolver(t, 32)
	blank := floorPlane()
	blank.ID = "   "
	if err := r.SetProxy(blank); !errors.Is(err, impl.ErrBadProxy) {
		t.Fatalf("blank proxy id = %v, want ErrBadProxy", err)
	}
	unknownKind := floorPlane()
	unknownKind.Kind = impl.ProxyKind(9)
	if err := r.SetProxy(unknownKind); !errors.Is(err, impl.ErrBadProxy) {
		t.Fatalf("unknown proxy kind = %v, want ErrBadProxy", err)
	}
	flatNormal := floorPlane()
	flatNormal.Normal = impl.Vec3{}
	if err := r.SetProxy(flatNormal); !errors.Is(err, impl.ErrBadProxy) {
		t.Fatalf("zero plane normal = %v, want ErrBadProxy", err)
	}
	tinyBall := unitBall()
	tinyBall.Radius = 0
	if err := r.SetProxy(tinyBall); !errors.Is(err, impl.ErrBadProxy) {
		t.Fatalf("zero sphere radius = %v, want ErrBadProxy", err)
	}
	if got := r.Proxies(); got != 0 {
		t.Fatalf("proxy count after rejects = %d, want 0", got)
	}
}

func TestSetProxyReplacesSameID(t *testing.T) {
	r := newResolver(t, 32)
	first := floorPlane()
	if err := r.SetProxy(first); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	second := floorPlane()
	second.Offset = 2
	if err := r.SetProxy(second); err != nil {
		t.Fatalf("replace proxy: %v", err)
	}
	if got := r.Proxies(); got != 1 {
		t.Fatalf("proxy count after replacing one id = %d, want 1", got)
	}
	if err := r.SetProxy(unitBall()); err != nil {
		t.Fatalf("set second proxy: %v", err)
	}
	if got := r.Proxies(); got != 2 {
		t.Fatalf("proxy count = %d, want 2", got)
	}
}

func TestSetRestitutionRejectsOutOfRange(t *testing.T) {
	r := newResolver(t, 32)
	if err := r.SetRestitution(-0.1); !errors.Is(err, impl.ErrBadRestitution) {
		t.Fatalf("negative restitution = %v, want ErrBadRestitution", err)
	}
	if err := r.SetRestitution(1.1); !errors.Is(err, impl.ErrBadRestitution) {
		t.Fatalf("restitution above one = %v, want ErrBadRestitution", err)
	}
	if got := r.Restitution(); !near(got, 0.5) {
		t.Fatalf("restitution after rejects = %v, want the default 0.5", got)
	}
	if err := r.SetRestitution(0.8); err != nil {
		t.Fatalf("set restitution 0.8: %v", err)
	}
	if got := r.Restitution(); !near(got, 0.8) {
		t.Fatalf("restitution = %v, want 0.8", got)
	}
}

func TestSetFrictionRejectsOutOfRange(t *testing.T) {
	r := newResolver(t, 32)
	if err := r.SetFriction(-0.01); !errors.Is(err, impl.ErrBadFriction) {
		t.Fatalf("negative friction = %v, want ErrBadFriction", err)
	}
	if err := r.SetFriction(1.5); !errors.Is(err, impl.ErrBadFriction) {
		t.Fatalf("friction above one = %v, want ErrBadFriction", err)
	}
	if got := r.Friction(); !near(got, 0.25) {
		t.Fatalf("friction after rejects = %v, want the default 0.25", got)
	}
	if err := r.SetFriction(0); err != nil {
		t.Fatalf("set friction zero: %v", err)
	}
	if got := r.Friction(); !near(got, 0) {
		t.Fatalf("friction = %v, want 0", got)
	}
}

func TestSetQueryBudgetRejectsNonPositive(t *testing.T) {
	r := newResolver(t, 64)
	if err := r.SetQueryBudget(0); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("zero budget = %v, want ErrBadBudget", err)
	}
	if err := r.SetQueryBudget(-2); !errors.Is(err, impl.ErrBadBudget) {
		t.Fatalf("negative budget = %v, want ErrBadBudget", err)
	}
	if _, limit := r.Budget(); limit != 64 {
		t.Fatalf("budget limit after rejects = %d, want the original 64", limit)
	}
	if err := r.SetQueryBudget(8); err != nil {
		t.Fatalf("set budget 8: %v", err)
	}
	if _, limit := r.Budget(); limit != 8 {
		t.Fatalf("budget limit = %d, want 8", limit)
	}
}

func TestResolveRejectsNegativeDelta(t *testing.T) {
	r := newResolver(t, 32)
	if _, err := r.Resolve(nil, -time.Millisecond); !errors.Is(err, impl.ErrBadDelta) {
		t.Fatalf("negative delta = %v, want ErrBadDelta", err)
	}
	out, err := r.Resolve(nil, 0)
	if err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty batch produced %d particles, want 0", len(out))
	}
	if got := r.Stats().Frames; got != 1 {
		t.Fatalf("frame counter = %d, want 1: the rejected frame must not count", got)
	}
}

func TestResolveIntegratesFreeParticle(t *testing.T) {
	r := newResolver(t, 16)
	r.SetQueryBudget(16)
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: 0, Z: 0}, impl.Vec3{X: 2, Y: 0, Z: 0}),
	}
	out, err := r.Resolve(batch, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("result length = %d, want 1", len(out))
	}
	if !vecNear(out[0].Pos, impl.Vec3{X: 0.2, Y: 0, Z: 0}) {
		t.Fatalf("integrated position = %+v, want (0.2,0,0) for 2 m/s over 100ms", out[0].Pos)
	}
	if !vecNear(out[0].Vel, impl.Vec3{X: 2, Y: 0, Z: 0}) {
		t.Fatalf("free particle velocity = %+v, want it unchanged", out[0].Vel)
	}
	if queries, _ := r.Budget(); queries != 0 {
		t.Fatalf("queries without any proxy = %d, want 0", queries)
	}
}

func TestResolvePlanePushesParticleOut(t *testing.T) {
	r := newResolver(t, 16)
	if err := r.SetProxy(floorPlane()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := r.SetRestitution(1); err != nil {
		t.Fatalf("set restitution: %v", err)
	}
	if err := r.SetFriction(0); err != nil {
		t.Fatalf("set friction: %v", err)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: -0.5, Z: 0}, impl.Vec3{X: 0, Y: -1, Z: 0}),
	}
	out, err := r.Resolve(batch, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !near(out[0].Pos.Y, 0) {
		t.Fatalf("separated position y = %v, want 0 on the plane", out[0].Pos.Y)
	}
	if !near(out[0].Vel.Y, 1) {
		t.Fatalf("bounced normal velocity = %v, want +1 with restitution 1", out[0].Vel.Y)
	}
	if queries, limit := r.Budget(); queries != 1 || limit != 16 {
		t.Fatalf("budget = (%d,%d), want (1,16)", queries, limit)
	}
	if got := r.Stats().Hits; got != 1 {
		t.Fatalf("hit counter = %d, want 1", got)
	}
}

func TestResolveRestitutionScalesBounce(t *testing.T) {
	dead := newResolver(t, 16)
	if err := dead.SetProxy(floorPlane()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := dead.SetRestitution(0); err != nil {
		t.Fatalf("set restitution 0: %v", err)
	}
	if err := dead.SetFriction(0); err != nil {
		t.Fatalf("set friction: %v", err)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: -0.5, Z: 0}, impl.Vec3{X: 0, Y: -1, Z: 0}),
	}
	out, err := dead.Resolve(batch, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !near(out[0].Vel.Y, 0) {
		t.Fatalf("normal velocity with restitution 0 = %v, want 0", out[0].Vel.Y)
	}
	half := newResolver(t, 16)
	if err := half.SetProxy(floorPlane()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := half.SetRestitution(0.5); err != nil {
		t.Fatalf("set restitution 0.5: %v", err)
	}
	if err := half.SetFriction(0); err != nil {
		t.Fatalf("set friction: %v", err)
	}
	out, err = half.Resolve(batch, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !near(out[0].Vel.Y, 0.5) {
		t.Fatalf("normal velocity with restitution 0.5 = %v, want 0.5", out[0].Vel.Y)
	}
}

func TestResolveFrictionDampsTangential(t *testing.T) {
	r := newResolver(t, 16)
	if err := r.SetProxy(floorPlane()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := r.SetRestitution(0); err != nil {
		t.Fatalf("set restitution: %v", err)
	}
	if err := r.SetFriction(0.5); err != nil {
		t.Fatalf("set friction: %v", err)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: -0.5, Z: 0}, impl.Vec3{X: 2, Y: -1, Z: 0}),
	}
	out, err := r.Resolve(batch, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !near(out[0].Vel.X, 1) {
		t.Fatalf("tangential velocity = %v, want 2*(1-0.5)=1", out[0].Vel.X)
	}
	if !near(out[0].Vel.Y, 0) {
		t.Fatalf("normal velocity = %v, want 0", out[0].Vel.Y)
	}
	if !near(out[0].Pos.Y, 0) {
		t.Fatalf("position y = %v, want the particle resting on the plane", out[0].Pos.Y)
	}
}

func TestResolveSphereSeparatesParticle(t *testing.T) {
	r := newResolver(t, 16)
	if err := r.SetProxy(unitBall()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := r.SetRestitution(1); err != nil {
		t.Fatalf("set restitution: %v", err)
	}
	if err := r.SetFriction(0); err != nil {
		t.Fatalf("set friction: %v", err)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0.75, Y: 0, Z: 0}, impl.Vec3{X: -2, Y: 0, Z: 0}),
	}
	out, err := r.Resolve(batch, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !vecNear(out[0].Pos, impl.Vec3{X: 1, Y: 0, Z: 0}) {
		t.Fatalf("separated position = %+v, want (1,0,0) on the sphere surface", out[0].Pos)
	}
	if !vecNear(out[0].Vel, impl.Vec3{X: 2, Y: 0, Z: 0}) {
		t.Fatalf("reflected velocity = %+v, want (2,0,0)", out[0].Vel)
	}
}

func TestBudgetCountsQueries(t *testing.T) {
	r := newResolver(t, 16)
	if err := r.SetProxy(farWall()); err != nil {
		t.Fatalf("set first proxy: %v", err)
	}
	farBall := unitBall()
	farBall.ID = "far-ball"
	farBall.Center = impl.Vec3{X: 1000, Y: 0, Z: 0}
	if err := r.SetProxy(farBall); err != nil {
		t.Fatalf("set second proxy: %v", err)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: 0, Z: 0}, impl.Vec3{X: 1, Y: 0, Z: 0}),
		newParticle(2, impl.Vec3{X: 5, Y: 0, Z: 0}, impl.Vec3{X: 1, Y: 0, Z: 0}),
		newParticle(3, impl.Vec3{X: 9, Y: 0, Z: 0}, impl.Vec3{X: 1, Y: 0, Z: 0}),
	}
	out, err := r.Resolve(batch, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("result length = %d, want 3", len(out))
	}
	queries, limit := r.Budget()
	if queries != 6 || limit != 16 {
		t.Fatalf("budget = (%d,%d), want (6,16) for three particles against two proxies", queries, limit)
	}
	stats := r.Stats()
	if stats.Queries != 6 || stats.Deferred != 0 || stats.Hits != 0 {
		t.Fatalf("stats = %+v, want 6 queries, no deferral and no hit", stats)
	}
}

func TestBudgetExhaustedParticlesStayPut(t *testing.T) {
	r := newResolver(t, 1)
	if err := r.SetProxy(floorPlane()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := r.SetRestitution(1); err != nil {
		t.Fatalf("set restitution: %v", err)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: -0.5, Z: 0}, impl.Vec3{X: 0, Y: -1, Z: 0}),
		newParticle(2, impl.Vec3{X: 3, Y: -0.75, Z: 0}, impl.Vec3{X: 0, Y: -1, Z: 0}),
	}
	out, err := r.Resolve(batch, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("result length = %d, want 2", len(out))
	}
	if !near(out[0].Pos.Y, 0) {
		t.Fatalf("first particle position y = %v, want it resolved onto the plane", out[0].Pos.Y)
	}
	if out[1].ID != 2 {
		t.Fatalf("second result id = %d, want the batch order preserved", out[1].ID)
	}
	if !vecNear(out[1].Pos, impl.Vec3{X: 3, Y: -0.75, Z: 0}) {
		t.Fatalf("deferred particle position = %+v, want it unchanged", out[1].Pos)
	}
	if !vecNear(out[1].Vel, impl.Vec3{X: 0, Y: -1, Z: 0}) {
		t.Fatalf("deferred particle velocity = %+v, want it unchanged", out[1].Vel)
	}
	if queries, _ := r.Budget(); queries != 1 {
		t.Fatalf("queries used = %d, want the budget of 1", queries)
	}
	if got := r.Stats().Deferred; got != 1 {
		t.Fatalf("deferred counter = %d, want 1", got)
	}
}

func TestSnapshotRestoreKeepsBudgetAndSettings(t *testing.T) {
	r := newResolver(t, 12)
	if err := r.SetProxy(floorPlane()); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	farBall := unitBall()
	farBall.ID = "far-ball"
	farBall.Center = impl.Vec3{X: 1000, Y: 0, Z: 0}
	if err := r.SetProxy(farBall); err != nil {
		t.Fatalf("set second proxy: %v", err)
	}
	if err := r.SetRestitution(0.8); err != nil {
		t.Fatalf("set restitution: %v", err)
	}
	if err := r.SetFriction(0.4); err != nil {
		t.Fatalf("set friction: %v", err)
	}
	payload, err := r.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := newResolver(t, 3)
	if err := restored.Restore(payload); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Restitution(); !near(got, 0.8) {
		t.Fatalf("restitution after restore = %v, want 0.8", got)
	}
	if got := restored.Friction(); !near(got, 0.4) {
		t.Fatalf("friction after restore = %v, want 0.4", got)
	}
	if _, limit := restored.Budget(); limit != 12 {
		t.Fatalf("budget limit after restore = %d, want 12", limit)
	}
	if got := restored.Proxies(); got != 2 {
		t.Fatalf("proxy count after restore = %d, want 2", got)
	}
	batch := []impl.Particle{
		newParticle(1, impl.Vec3{X: 0, Y: 0, Z: 0}, impl.Vec3{X: 1, Y: 0, Z: 0}),
	}
	out, err := restored.Resolve(batch, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("resolve after restore: %v", err)
	}
	if !vecNear(out[0].Pos, impl.Vec3{X: 0.1, Y: 0, Z: 0}) {
		t.Fatalf("integrated position after restore = %+v, want (0.1,0,0)", out[0].Pos)
	}
	if got := restored.Stats().Frames; got != 1 {
		t.Fatalf("frame counter after restore = %d, want 1", got)
	}
}
