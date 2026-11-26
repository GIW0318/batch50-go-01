// Package task answers mouse and box picking against registered scene
// instance bounds inside a fixed per frame traversal budget.
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
	"strconv"
	"strings"
	"sync"
)

// Defaults applied when the matching PickConfig field is left non positive.
const (
	DefaultMaxInstances       = 1024
	DefaultMaxQueriesPerFrame = 64
	DefaultMaxNodesPerFrame   = 4096
)

// Errors reported by the pick service.
var (
	ErrBadInstance       = errors.New("scene instance is invalid")
	ErrDuplicateInstance = errors.New("scene instance already registered")
	ErrUnknownInstance   = errors.New("scene instance not found")
	ErrInstanceLimit     = errors.New("scene instance limit reached")
	ErrBadLayer          = errors.New("layer mask is invalid")
	ErrLayerConflict     = errors.New("layer mask conflicts with an existing binding")
	ErrBadQuery          = errors.New("pick query is invalid")
	ErrUnknownQuery      = errors.New("pick query not found")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Vec3 is a point or direction in world space.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Ray is a normalized pick ray.
type Ray struct {
	Origin Vec3 `json:"origin"`
	Dir    Vec3 `json:"dir"`
}

// AABB is an axis aligned bounding box in world space.
type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

// Instance is one pickable scene instance with its world bounds.
type Instance struct {
	ID         string `json:"id"`
	Layer      string `json:"layer"`
	Box        AABB   `json:"box"`
	Generation uint64 `json:"generation"`
}

// PickQuery is one pick request issued by the input thread.
type PickQuery struct {
	Ray         Ray     `json:"ray"`
	LayerMask   uint32  `json:"layer_mask"`
	MaxDistance float64 `json:"max_distance"`
}

// QueryID identifies one queued pick query.
type QueryID string

// Hit is the resolved nearest instance of one query.
type Hit struct {
	QueryID    QueryID `json:"query_id"`
	InstanceID string  `json:"instance_id"`
	Layer      string  `json:"layer"`
	Distance   float64 `json:"distance"`
	Generation uint64  `json:"generation"`
}

// PickConfig bounds the service tables and the per frame budgets.
type PickConfig struct {
	MaxInstances       int
	MaxQueriesPerFrame int
	MaxNodesPerFrame   int
}

// PickStats is the counter block handed to the frame HUD.
type PickStats struct {
	Frame        uint64 `json:"frame"`
	Submitted    int    `json:"submitted"`
	Executed     int    `json:"executed"`
	Deferred     int    `json:"deferred"`
	Merged       int    `json:"merged"`
	Queued       int    `json:"queued"`
	NodesVisited int    `json:"nodes_visited"`
}

type queryRecord struct {
	query    PickQuery
	executed bool
	hit      Hit
}

// queryKey is the exact identity of a query for same-frame deduplication.
// Floats are represented by their bits so that map lookups remain stable.
type queryKey [8]uint64

const snapshotVersion = 1
const queryIDPrefix = "q-"

// PickQueryService owns the instance table, the layer bindings and the frame
// queue of pending pick queries.
type PickQueryService struct {
	mu sync.RWMutex

	cfg PickConfig

	instances   map[string]Instance
	layers      map[string]uint32
	layerOwners map[uint32]string

	nextGeneration uint64
	nextQuerySeq   uint64

	queries map[QueryID]*queryRecord
	queue   []QueryID
	dedup   map[queryKey]QueryID

	deferredIDs map[QueryID]struct{}
	stats       PickStats
}

// NewPickQueryService builds a service with the given budgets.
func NewPickQueryService(cfg PickConfig) *PickQueryService {
	cfg = normalizeConfig(cfg)
	return &PickQueryService{
		cfg:         cfg,
		instances:   make(map[string]Instance),
		layers:      make(map[string]uint32),
		layerOwners: make(map[uint32]string),
		queries:     make(map[QueryID]*queryRecord),
		dedup:       make(map[queryKey]QueryID),
		deferredIDs: make(map[QueryID]struct{}),
	}
}

func normalizeConfig(cfg PickConfig) PickConfig {
	if cfg.MaxInstances <= 0 {
		cfg.MaxInstances = DefaultMaxInstances
	}
	if cfg.MaxQueriesPerFrame <= 0 {
		cfg.MaxQueriesPerFrame = DefaultMaxQueriesPerFrame
	}
	if cfg.MaxNodesPerFrame <= 0 {
		cfg.MaxNodesPerFrame = DefaultMaxNodesPerFrame
	}
	return cfg
}

func validAABB(box AABB) bool {
	values := [...]float64{
		box.Min.X, box.Min.Y, box.Min.Z,
		box.Max.X, box.Max.Y, box.Max.Z,
	}
	for _, value := range values {
		if math.IsNaN(value) {
			return false
		}
	}
	return box.Min.X <= box.Max.X &&
		box.Min.Y <= box.Max.Y &&
		box.Min.Z <= box.Max.Z
}

func validInstanceIdentity(inst Instance) bool {
	return strings.TrimSpace(inst.ID) != "" &&
		strings.TrimSpace(inst.Layer) != ""
}

func validQuery(q PickQuery) bool {
	nx := q.Ray.Dir.X
	ny := q.Ray.Dir.Y
	nz := q.Ray.Dir.Z
	norm := math.Sqrt(nx*nx + ny*ny + nz*nz)
	if !(math.Abs(norm-1) <= 1e-6) {
		return false
	}
	if !(q.MaxDistance > 0) {
		return false
	}
	return q.LayerMask != 0
}

func queryKeyFor(q PickQuery) queryKey {
	return queryKey{
		floatBits(q.Ray.Origin.X),
		floatBits(q.Ray.Origin.Y),
		floatBits(q.Ray.Origin.Z),
		floatBits(q.Ray.Dir.X),
		floatBits(q.Ray.Dir.Y),
		floatBits(q.Ray.Dir.Z),
		uint64(q.LayerMask),
		floatBits(q.MaxDistance),
	}
}

func floatBits(value float64) uint64 {
	if value == 0 {
		return 0
	}
	return math.Float64bits(value)
}

func makeQueryID(seq uint64) QueryID {
	return QueryID(queryIDPrefix + strconv.FormatUint(seq, 10))
}

func querySequence(id QueryID) (uint64, bool) {
	text := string(id)
	if !strings.HasPrefix(text, queryIDPrefix) {
		return 0, false
	}
	seq, err := strconv.ParseUint(strings.TrimPrefix(text, queryIDPrefix), 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// AddInstance registers one pickable instance.
func (s *PickQueryService) AddInstance(inst Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !validInstanceIdentity(inst) || !validAABB(inst.Box) {
		return ErrBadInstance
	}
	if _, exists := s.instances[inst.ID]; exists {
		return ErrDuplicateInstance
	}
	if len(s.instances) >= s.cfg.MaxInstances {
		return ErrInstanceLimit
	}

	s.nextGeneration++
	inst.Generation = s.nextGeneration
	s.instances[inst.ID] = inst
	return nil
}

// UpdateInstance replaces the world bounds of a registered instance and
// advances its generation.
func (s *PickQueryService) UpdateInstance(instanceID string, box AABB) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.instances[instanceID]
	if !ok {
		return ErrUnknownInstance
	}
	if !validAABB(box) {
		return ErrBadInstance
	}

	s.nextGeneration++
	inst.Box = box
	inst.Generation = s.nextGeneration
	s.instances[instanceID] = inst
	return nil
}

// RemoveInstance drops one instance from the table.
func (s *PickQueryService) RemoveInstance(instanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.instances[instanceID]; !ok {
		return ErrUnknownInstance
	}
	delete(s.instances, instanceID)
	return nil
}

// Instance reads one registered instance.
func (s *PickQueryService) Instance(instanceID string) (Instance, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inst, ok := s.instances[instanceID]
	return inst, ok
}

// Instances lists the registered instance ids in ascending order.
func (s *PickQueryService) Instances() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.instances))
	for id := range s.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SetLayerMask binds a layer name to exactly one bit.
func (s *PickQueryService) SetLayerMask(layer string, mask uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(layer) == "" || mask == 0 || mask&(mask-1) != 0 {
		return ErrBadLayer
	}
	if _, exists := s.layers[layer]; exists {
		return ErrLayerConflict
	}
	if owner, exists := s.layerOwners[mask]; exists && owner != layer {
		return ErrLayerConflict
	}

	s.layers[layer] = mask
	s.layerOwners[mask] = layer
	return nil
}

// LayerMask reads the bit bound to a layer.
func (s *PickQueryService) LayerMask(layer string) (uint32, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if strings.TrimSpace(layer) == "" {
		return 0, false
	}
	mask, ok := s.layers[layer]
	return mask, ok
}

// Submit queues one pick query for the current frame.
func (s *PickQueryService) Submit(q PickQuery) (QueryID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !validQuery(q) {
		return "", ErrBadQuery
	}

	key := queryKeyFor(q)
	if id, ok := s.dedup[key]; ok {
		s.stats.Submitted++
		s.stats.Merged++
		return id, nil
	}

	s.nextQuerySeq++
	id := makeQueryID(s.nextQuerySeq)
	s.queries[id] = &queryRecord{query: q}
	s.queue = append(s.queue, id)
	s.dedup[key] = id

	s.stats.Submitted++
	s.stats.Queued = len(s.queue)
	return id, nil
}

// RunFrame executes the queued queries that fit the frame budgets and returns
// how many of them ran.
func (s *PickQueryService) RunFrame() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stats.Frame++
	s.dedup = make(map[queryKey]QueryID)

	executed := 0
	nodesThisFrame := 0
	instanceIDs := s.sortedInstanceIDs()

	for len(s.queue) > 0 && executed < s.cfg.MaxQueriesPerFrame {
		id := s.queue[0]
		record, ok := s.queries[id]
		if !ok || record.executed {
			s.queue = s.queue[1:]
			continue
		}

		nodesForQuery := len(s.instances)
		if executed > 0 && nodesThisFrame+nodesForQuery > s.cfg.MaxNodesPerFrame {
			break
		}

		s.queue = s.queue[1:]
		hit := s.executeQuery(record.query, instanceIDs)
		hit.QueryID = id
		record.hit = hit
		record.executed = true

		executed++
		nodesThisFrame += nodesForQuery
	}

	s.stats.Executed += executed
	s.stats.NodesVisited += nodesThisFrame
	for _, id := range s.queue {
		if _, seen := s.deferredIDs[id]; !seen {
			s.deferredIDs[id] = struct{}{}
			s.stats.Deferred++
		}
	}
	s.stats.Queued = len(s.queue)
	return executed
}

// Resolve reads the outcome of one submitted query.
func (s *PickQueryService) Resolve(id QueryID) (Hit, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.queries[id]
	if !ok {
		return Hit{}, false, ErrUnknownQuery
	}
	if !record.executed {
		return Hit{}, false, nil
	}
	return record.hit, true, nil
}

// ResolveBatch answers a slice of queries outside the frame budget.
func (s *PickQueryService) ResolveBatch(qs []PickQuery) ([]Hit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, q := range qs {
		if !validQuery(q) {
			return nil, ErrBadQuery
		}
	}

	hits := make([]Hit, len(qs))
	instanceIDs := s.sortedInstanceIDs()
	for i, q := range qs {
		hits[i] = s.executeQuery(q, instanceIDs)
	}
	return hits, nil
}

// Stats returns the frame counters.
func (s *PickQueryService) Stats() PickStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := s.stats
	stats.Queued = len(s.queue)
	return stats
}

func (s *PickQueryService) sortedInstanceIDs() []string {
	ids := make([]string, 0, len(s.instances))
	for id := range s.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *PickQueryService) executeQuery(q PickQuery, instanceIDs []string) Hit {
	best := Hit{}
	found := false

	for _, id := range instanceIDs {
		inst := s.instances[id]
		layerMask, bound := s.layers[inst.Layer]
		if !bound || q.LayerMask&layerMask == 0 {
			continue
		}

		distance, hit := rayAABB(q.Ray, inst.Box)
		if !hit || distance > q.MaxDistance {
			continue
		}
		if found && (distance > best.Distance ||
			(distance == best.Distance && id >= best.InstanceID)) {
			continue
		}

		best.InstanceID = inst.ID
		best.Layer = inst.Layer
		best.Distance = distance
		best.Generation = inst.Generation
		found = true
	}
	return best
}

func rayAABB(ray Ray, box AABB) (float64, bool) {
	tMin := 0.0
	tMax := math.Inf(1)

	axes := [...]struct {
		origin    float64
		direction float64
		boxMin    float64
		boxMax    float64
	}{
		{ray.Origin.X, ray.Dir.X, box.Min.X, box.Max.X},
		{ray.Origin.Y, ray.Dir.Y, box.Min.Y, box.Max.Y},
		{ray.Origin.Z, ray.Dir.Z, box.Min.Z, box.Max.Z},
	}

	for _, axis := range axes {
		if axis.direction == 0 {
			if axis.origin < axis.boxMin || axis.origin > axis.boxMax {
				return 0, false
			}
			continue
		}

		t1 := (axis.boxMin - axis.origin) / axis.direction
		t2 := (axis.boxMax - axis.origin) / axis.direction
		if math.IsNaN(t1) || math.IsNaN(t2) {
			return 0, false
		}
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		if t1 > tMin {
			tMin = t1
		}
		if t2 < tMax {
			tMax = t2
		}
		if tMin > tMax {
			return 0, false
		}
	}

	if tMax < 0 {
		return 0, false
	}
	return tMin, true
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

type snapshotState struct {
	Instances      []Instance      `json:"instances"`
	Layers         []snapshotLayer `json:"layers"`
	Queries        []snapshotQuery `json:"queries"`
	Pending        []QueryID       `json:"pending"`
	Dedup          []snapshotDedup `json:"dedup,omitempty"`
	DeferredIDs    []QueryID       `json:"deferred_ids,omitempty"`
	NextGeneration uint64          `json:"next_generation"`
	NextQuerySeq   uint64          `json:"next_query_seq"`
	Stats          PickStats       `json:"stats"`
}

type snapshotLayer struct {
	Name string `json:"name"`
	Mask uint32 `json:"mask"`
}

type snapshotQuery struct {
	ID       QueryID   `json:"id"`
	Query    PickQuery `json:"query"`
	Executed bool      `json:"executed"`
	Hit      Hit       `json:"hit"`
}

type snapshotDedup struct {
	ID  QueryID  `json:"id"`
	Key queryKey `json:"key"`
}

// Snapshot serialises the instance table, the layer bindings, the pending
// queue and the counters.
func (s *PickQueryService) Snapshot() ([]byte, error) {
	s.mu.RLock()
	state := s.snapshotState()
	s.mu.RUnlock()

	stateData, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(stateData)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Checksum: hex.EncodeToString(sum[:]),
		State:    json.RawMessage(stateData),
	}
	return json.Marshal(envelope)
}

func (s *PickQueryService) snapshotState() snapshotState {
	instances := make([]Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].ID < instances[j].ID
	})

	layers := make([]snapshotLayer, 0, len(s.layers))
	for name, mask := range s.layers {
		layers = append(layers, snapshotLayer{Name: name, Mask: mask})
	}
	sort.Slice(layers, func(i, j int) bool {
		return layers[i].Name < layers[j].Name
	})

	queries := make([]snapshotQuery, 0, len(s.queries))
	for id, record := range s.queries {
		queries = append(queries, snapshotQuery{
			ID:       id,
			Query:    record.query,
			Executed: record.executed,
			Hit:      record.hit,
		})
	}
	sort.Slice(queries, func(i, j int) bool {
		return queryIDLess(queries[i].ID, queries[j].ID)
	})

	pending := append([]QueryID(nil), s.queue...)

	dedup := make([]snapshotDedup, 0, len(s.dedup))
	for key, id := range s.dedup {
		dedup = append(dedup, snapshotDedup{ID: id, Key: key})
	}
	sort.Slice(dedup, func(i, j int) bool {
		return queryIDLess(dedup[i].ID, dedup[j].ID)
	})

	deferred := make([]QueryID, 0, len(s.deferredIDs))
	for id := range s.deferredIDs {
		deferred = append(deferred, id)
	}
	sort.Slice(deferred, func(i, j int) bool {
		return queryIDLess(deferred[i], deferred[j])
	})

	stats := s.stats
	stats.Queued = len(pending)
	return snapshotState{
		Instances:      instances,
		Layers:         layers,
		Queries:        queries,
		Pending:        pending,
		Dedup:          dedup,
		DeferredIDs:    deferred,
		NextGeneration: s.nextGeneration,
		NextQuerySeq:   s.nextQuerySeq,
		Stats:          stats,
	}
}

func queryIDLess(left, right QueryID) bool {
	leftSeq, leftOK := querySequence(left)
	rightSeq, rightOK := querySequence(right)
	if leftOK && rightOK {
		if leftSeq != rightSeq {
			return leftSeq < rightSeq
		}
	}
	return left < right
}

// Restore replaces the whole state with a snapshot payload.
func (s *PickQueryService) Restore(data []byte) error {
	state, err := decodeSnapshot(data, s.cfg.MaxInstances)
	if err != nil {
		return ErrBadSnapshot
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances = state.instances
	s.layers = state.layers
	s.layerOwners = state.layerOwners
	s.nextGeneration = state.nextGeneration
	s.nextQuerySeq = state.nextQuerySeq
	s.queries = state.queries
	s.queue = state.pending
	s.dedup = state.dedup
	s.deferredIDs = state.deferredIDs
	s.stats = state.stats
	return nil
}

type restoredState struct {
	instances      map[string]Instance
	layers         map[string]uint32
	layerOwners    map[uint32]string
	nextGeneration uint64
	nextQuerySeq   uint64
	queries        map[QueryID]*queryRecord
	pending        []QueryID
	dedup          map[queryKey]QueryID
	deferredIDs    map[QueryID]struct{}
	stats          PickStats
}

func decodeSnapshot(data []byte, maxInstances int) (restoredState, error) {
	var zero restoredState

	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return zero, err
	}
	if envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return zero, ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.State)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return zero, ErrBadSnapshot
	}

	var snapshot snapshotState
	decoder := json.NewDecoder(bytes.NewReader(envelope.State))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return zero, err
	}
	if err := ensureEOF(decoder); err != nil {
		return zero, err
	}

	if len(snapshot.Instances) > maxInstances {
		return zero, ErrBadSnapshot
	}

	instances := make(map[string]Instance, len(snapshot.Instances))
	var maxGeneration uint64
	for _, inst := range snapshot.Instances {
		if !validInstanceIdentity(inst) || !validAABB(inst.Box) || inst.Generation == 0 {
			return zero, ErrBadSnapshot
		}
		if _, exists := instances[inst.ID]; exists {
			return zero, ErrBadSnapshot
		}
		instances[inst.ID] = inst
		if inst.Generation > maxGeneration {
			maxGeneration = inst.Generation
		}
	}
	if maxGeneration > snapshot.NextGeneration {
		return zero, ErrBadSnapshot
	}

	layers := make(map[string]uint32, len(snapshot.Layers))
	layerOwners := make(map[uint32]string, len(snapshot.Layers))
	for _, binding := range snapshot.Layers {
		if strings.TrimSpace(binding.Name) == "" ||
			binding.Mask == 0 || binding.Mask&(binding.Mask-1) != 0 {
			return zero, ErrBadSnapshot
		}
		if _, exists := layers[binding.Name]; exists {
			return zero, ErrBadSnapshot
		}
		if _, exists := layerOwners[binding.Mask]; exists {
			return zero, ErrBadSnapshot
		}
		layers[binding.Name] = binding.Mask
		layerOwners[binding.Mask] = binding.Name
	}

	queries := make(map[QueryID]*queryRecord, len(snapshot.Queries))
	var maxQuerySeq uint64
	for _, query := range snapshot.Queries {
		seq, ok := querySequence(query.ID)
		if !ok || seq > snapshot.NextQuerySeq || !validQuery(query.Query) {
			return zero, ErrBadSnapshot
		}
		if _, exists := queries[query.ID]; exists {
			return zero, ErrBadSnapshot
		}
		if seq > maxQuerySeq {
			maxQuerySeq = seq
		}
		hit := query.Hit
		if query.Executed {
			if hit.QueryID != query.ID {
				return zero, ErrBadSnapshot
			}
			if hit.InstanceID == "" && hit.Distance != 0 {
				return zero, ErrBadSnapshot
			}
			if hit.InstanceID != "" && math.IsNaN(hit.Distance) {
				return zero, ErrBadSnapshot
			}
		}
		queries[query.ID] = &queryRecord{
			query:    query.Query,
			executed: query.Executed,
			hit:      hit,
		}
	}
	_ = maxQuerySeq

	pending := append([]QueryID(nil), snapshot.Pending...)
	pendingSet := make(map[QueryID]struct{}, len(pending))
	for _, id := range pending {
		if _, duplicate := pendingSet[id]; duplicate {
			return zero, ErrBadSnapshot
		}
		record, exists := queries[id]
		if !exists || record.executed {
			return zero, ErrBadSnapshot
		}
		pendingSet[id] = struct{}{}
	}
	for id, record := range queries {
		if !record.executed {
			if _, queued := pendingSet[id]; !queued {
				return zero, ErrBadSnapshot
			}
		}
	}

	dedup := make(map[queryKey]QueryID, len(snapshot.Dedup))
	for _, completion := range snapshot.Dedup {
		record, exists := queries[completion.ID]
		if !exists || record.executed {
			return zero, ErrBadSnapshot
		}
		if _, queued := pendingSet[completion.ID]; !queued {
			return zero, ErrBadSnapshot
		}
		if _, duplicate := dedup[completion.Key]; duplicate {
			return zero, ErrBadSnapshot
		}
		dedup[completion.Key] = completion.ID
	}

	deferredIDs := make(map[QueryID]struct{}, len(snapshot.DeferredIDs))
	for _, id := range snapshot.DeferredIDs {
		if _, exists := queries[id]; !exists {
			return zero, ErrBadSnapshot
		}
		if _, duplicate := deferredIDs[id]; duplicate {
			return zero, ErrBadSnapshot
		}
		deferredIDs[id] = struct{}{}
	}

	stats := snapshot.Stats
	if stats.Queued != len(pending) ||
		stats.Submitted < 0 || stats.Executed < 0 || stats.Deferred < 0 ||
		stats.Merged < 0 || stats.NodesVisited < 0 ||
		stats.Deferred != len(deferredIDs) {
		return zero, ErrBadSnapshot
	}

	return restoredState{
		instances:      instances,
		layers:         layers,
		layerOwners:    layerOwners,
		nextGeneration: snapshot.NextGeneration,
		nextQuerySeq:   snapshot.NextQuerySeq,
		queries:        queries,
		pending:        pending,
		dedup:          dedup,
		deferredIDs:    deferredIDs,
		stats:          stats,
	}, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ErrBadSnapshot
		}
		return err
	}
	return nil
}
