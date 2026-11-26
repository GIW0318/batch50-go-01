// Package task inspects delivered executables for compile time mitigations and
// compares a batch against a frozen baseline.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Recognised container formats.
const (
	FormatELF = "elf"
)

// Word sizes.
const (
	ClassELF32 = "elf32"
	ClassELF64 = "elf64"
)

// Tri state conclusions used for address independence, stack protection,
// non executable stack and control flow checks.
const (
	StateEnabled  = "enabled"
	StateDisabled = "disabled"
	StatePresent  = "present"
	StateAbsent   = "absent"
	StateUnknown  = "unknown"
)

// Conclusions of the relocation read only check.
const (
	StateFull    = "full"
	StatePartial = "partial"
	StateNone    = "none"
)

// Severities of one baseline difference.
const (
	SeverityRegression  = "regression"
	SeverityImprovement = "improvement"
)

// Field names used by Delta and Evidence.
const (
	FieldPIE         = "pie"
	FieldRelro       = "relro"
	FieldStackCanary = "stack_canary"
	FieldNXStack     = "nx_stack"
	FieldCFI         = "cfi"
)

// Errors returned by the prober.
var (
	ErrEmptyInput      = errors.New("artifact payload is empty")
	ErrFileTooLarge    = errors.New("artifact is larger than the configured cap")
	ErrUnknownFormat   = errors.New("artifact is not a recognised executable format")
	ErrMalformed       = errors.New("executable header is malformed")
	ErrArtifactLimit   = errors.New("artifact cap reached")
	ErrUnknownArtifact = errors.New("unknown artifact")
	ErrBadBaseline     = errors.New("baseline entry is incomplete")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Config bounds one prober instance.
type Config struct {
	MaxFileBytes int
	MaxWorkers   int
	MaxArtifacts int
}

// Artifact is one delivered binary handed to InspectAll.
type Artifact struct {
	Name string
	Data []byte
}

// Posture is the mitigation inventory of one artifact.
type Posture struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Format      string `json:"format"`
	Class       string `json:"class"`
	PIE         string `json:"pie"`
	Relro       string `json:"relro"`
	StackCanary string `json:"stack_canary"`
	NXStack     string `json:"nx_stack"`
	CFI         string `json:"cfi"`
	Digest      string `json:"digest"`
}

// Delta is one field that differs from the baseline.
type Delta struct {
	Field    string `json:"field"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Severity string `json:"severity"`
}

// Evidence ties one conclusion back to the bytes it was derived from.
type Evidence struct {
	Field  string `json:"field"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

// MitigationPostureProber owns the inspected artifacts and the frozen baseline.
type MitigationPostureProber struct {
	cfg Config
}

// NewMitigationPostureProber builds a prober with the given caps.
func NewMitigationPostureProber(cfg Config) *MitigationPostureProber {
	return &MitigationPostureProber{cfg: cfg}
}

// Inspect reads one artifact and records its posture under name.
func (p *MitigationPostureProber) Inspect(name string, data []byte) (Posture, error) {
	return Posture{}, errNotImplemented
}

// InspectAll reads a batch of artifacts and keeps the input order.
func (p *MitigationPostureProber) InspectAll(items []Artifact) ([]Posture, error) {
	return nil, errNotImplemented
}

// Compare lists the fields that moved between two postures.
func (p *MitigationPostureProber) Compare(base, cur Posture) ([]Delta, error) {
	return nil, errNotImplemented
}

// Baseline replaces the whole baseline in one step.
func (p *MitigationPostureProber) Baseline(items []Posture) error {
	return errNotImplemented
}

// BaselineOf reads one frozen baseline entry.
func (p *MitigationPostureProber) BaselineOf(name string) (Posture, bool) {
	return Posture{}, false
}

// BaselineSize counts the frozen baseline entries.
func (p *MitigationPostureProber) BaselineSize() int {
	return 0
}

// Explain returns one evidence entry per conclusion, ordered by field.
func (p *MitigationPostureProber) Explain(id string) ([]Evidence, error) {
	return nil, errNotImplemented
}

// Posture reads one recorded inventory entry.
func (p *MitigationPostureProber) Posture(id string) (Posture, bool) {
	return Posture{}, false
}

// Artifacts lists the recorded artifact ids in name order.
func (p *MitigationPostureProber) Artifacts() []string {
	return nil
}

// Snapshot serialises the recorded postures and the baseline.
func (p *MitigationPostureProber) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *MitigationPostureProber) Restore(data []byte) error {
	return errNotImplemented
}
