// Package task compiles sandbox device access rules into a program, attaches
// it to one container and answers whether a device open is allowed, keeping
// the last good program alive across failed updates.
package task

import "errors"

// errNotImplemented marks every behaviour that still has to be built.
var errNotImplemented = errors.New("not implemented")

// Device classes used by the kernel: all, block, char.
const (
	DeviceAll   = "a"
	DeviceBlock = "b"
	DeviceChar  = "c"
)

// Instruction operations.
const (
	OpAllow = "allow"
	OpDeny  = "deny"
)

// WildcardMajor and WildcardMinor mark a rule that matches any number.
const Wildcard = -1

// Reason codes returned by Explain.
const (
	ReasonDefaultDeny   = "default_deny"
	ReasonExactAllow    = "exact_allow"
	ReasonWildcardAllow = "wildcard_allow"
	ReasonExactDeny     = "exact_deny"
	ReasonWildcardDeny  = "wildcard_deny"
	ReasonNoContainer   = "no_container"
)

// Errors returned by the controller.
var (
	ErrBadPolicy      = errors.New("device policy is not acceptable")
	ErrBadDeviceType  = errors.New("unknown device class")
	ErrRuleLimit      = errors.New("rule count over the container limit")
	ErrContainerLimit = errors.New("container count over the node limit")
	ErrNoContainer    = errors.New("container is not attached")
	ErrBadProgram     = errors.New("program was not produced by Compile")
	ErrStaleRevision  = errors.New("policy revision is not newer than the active one")
	ErrCacheLimit     = errors.New("compile cache is full")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Device is one open attempt against a device node.
type Device struct {
	Type      string `json:"type"`
	Major     int    `json:"major"`
	Minor     int    `json:"minor"`
	Path      string `json:"path"`
	HostInode uint64 `json:"host_inode"`
}

// Rule is one allow or deny entry of a policy. A Major or Minor of Wildcard
// matches any number, and a Type of DeviceAll matches every device class.
// Priority breaks ties between rules that match the same device.
type Rule struct {
	Type     string `json:"type"`
	Major    int    `json:"major"`
	Minor    int    `json:"minor"`
	Allow    bool   `json:"allow"`
	Priority int    `json:"priority"`
}

// DevicePolicy is one immutable policy revision.
type DevicePolicy struct {
	Revision int    `json:"revision"`
	Rules    []Rule `json:"rules"`
}

// Instruction is one compiled comparison.
type Instruction struct {
	Op    string `json:"op"`
	Type  string `json:"type"`
	Major int    `json:"major"`
	Minor int    `json:"minor"`
}

// Program is the compiled artefact attached to one container.
type Program struct {
	ID           string        `json:"id"`
	Revision     int           `json:"revision"`
	Rules        int           `json:"rules"`
	Digest       string        `json:"digest"`
	Instructions []Instruction `json:"instructions"`
}

// Version is the effective policy revision of a container.
type Version int

// Verdict is the outcome of one Explain call.
type Verdict struct {
	Allowed    bool    `json:"allowed"`
	Program    string  `json:"program"`
	Rule       string  `json:"rule"`
	Generation Version `json:"generation"`
}

// Reason explains a verdict in human readable terms.
type Reason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Limits bounds the node wide tables.
type Limits struct {
	MaxRulesPerContainer int
	MaxContainers        int
	MaxCacheEntries      int
}

// DeviceCgroupController owns the compiled programs, the container table and
// the compile cache.
type DeviceCgroupController struct {
	limits Limits
}

// NewDeviceCgroupController builds a controller with the given caps.
func NewDeviceCgroupController(limits Limits) *DeviceCgroupController {
	return &DeviceCgroupController{limits: limits}
}

// Compile turns one policy revision into a program. The digest must not depend
// on the order the caller wrote the rules in.
func (c *DeviceCgroupController) Compile(policy DevicePolicy) (Program, error) {
	return Program{}, errNotImplemented
}

// Attach makes a program the active generation of one container.
func (c *DeviceCgroupController) Attach(containerID string, prog Program) error {
	return errNotImplemented
}

// Update compiles a newer policy and swaps it in whole; a failure keeps the
// previous generation active.
func (c *DeviceCgroupController) Update(containerID string, policy DevicePolicy) (Version, error) {
	return 0, errNotImplemented
}

// Explain evaluates one device open against the active generation.
func (c *DeviceCgroupController) Explain(containerID string, dev Device) (Verdict, Reason) {
	return Verdict{}, Reason{}
}

// Program reads back the active program of one container.
func (c *DeviceCgroupController) Program(containerID string) (Program, bool) {
	return Program{}, false
}

// CurrentVersion reports the effective policy revision of one container.
func (c *DeviceCgroupController) CurrentVersion(containerID string) Version {
	return 0
}

// Detach drops one container and its program.
func (c *DeviceCgroupController) Detach(containerID string) error {
	return errNotImplemented
}

// Containers lists the attached containers in ascending id order.
func (c *DeviceCgroupController) Containers() []string {
	return nil
}

// CompileCacheSize reports how many compiled policies are cached.
func (c *DeviceCgroupController) CompileCacheSize() int {
	return 0
}

// Export serialises compiled programs, the container table and the cache.
func (c *DeviceCgroupController) Export() ([]byte, error) {
	return nil, errNotImplemented
}

// Import replaces the whole state with an exported payload, all or nothing.
func (c *DeviceCgroupController) Import(data []byte) error {
	return errNotImplemented
}
