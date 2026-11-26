// Package task answers mouse and box picking against registered scene
// instance bounds inside a fixed per frame traversal budget.
package task

import "errors"

// errNotImplemented marks behaviour that is still missing.
var errNotImplemented = errors.New("not implemented")

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

// PickQueryService owns the instance table, the layer bindings and the frame
// queue of pending pick queries.
type PickQueryService struct {
	cfg PickConfig
}

// NewPickQueryService builds a service with the given budgets.
func NewPickQueryService(cfg PickConfig) *PickQueryService {
	return &PickQueryService{cfg: cfg}
}

// AddInstance registers one pickable instance.
func (s *PickQueryService) AddInstance(inst Instance) error {
	return errNotImplemented
}

// UpdateInstance replaces the world bounds of a registered instance and
// advances its generation.
func (s *PickQueryService) UpdateInstance(instanceID string, box AABB) error {
	return errNotImplemented
}

// RemoveInstance drops one instance from the table.
func (s *PickQueryService) RemoveInstance(instanceID string) error {
	return errNotImplemented
}

// Instance reads one registered instance.
func (s *PickQueryService) Instance(instanceID string) (Instance, bool) {
	return Instance{}, false
}

// Instances lists the registered instance ids in ascending order.
func (s *PickQueryService) Instances() []string {
	return nil
}

// SetLayerMask binds a layer name to exactly one bit.
func (s *PickQueryService) SetLayerMask(layer string, mask uint32) error {
	return errNotImplemented
}

// LayerMask reads the bit bound to a layer.
func (s *PickQueryService) LayerMask(layer string) (uint32, bool) {
	return 0, false
}

// Submit queues one pick query for the current frame.
func (s *PickQueryService) Submit(q PickQuery) (QueryID, error) {
	return "", errNotImplemented
}

// RunFrame executes the queued queries that fit the frame budgets and returns
// how many of them ran.
func (s *PickQueryService) RunFrame() int {
	return 0
}

// Resolve reads the outcome of one submitted query.
func (s *PickQueryService) Resolve(id QueryID) (Hit, bool, error) {
	return Hit{}, false, errNotImplemented
}

// ResolveBatch answers a slice of queries outside the frame budget.
func (s *PickQueryService) ResolveBatch(qs []PickQuery) ([]Hit, error) {
	return nil, errNotImplemented
}

// Stats returns the frame counters.
func (s *PickQueryService) Stats() PickStats {
	return PickStats{}
}

// Snapshot serialises the instance table, the layer bindings, the pending
// queue and the counters.
func (s *PickQueryService) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot payload.
func (s *PickQueryService) Restore(data []byte) error {
	return errNotImplemented
}
