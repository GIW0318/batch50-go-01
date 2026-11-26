// Package task screens external content before it is folded into a model
// context and authorises the tool calls the model asks for.
package task

import (
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Origin is where a content chunk came from.
type Origin string

// Known origins. External and tool content is never trusted on its own.
const (
	OriginSystem   Origin = "system"
	OriginUser     Origin = "user"
	OriginTool     Origin = "tool"
	OriginExternal Origin = "external"
)

// Screening levels.
const (
	LevelClean      = "clean"
	LevelSuspicious = "suspicious"
	LevelBlocked    = "blocked"
)

// Trust labels attached to a built context.
const (
	TrustTrusted   = "trusted"
	TrustUntrusted = "untrusted"
	TrustMixed     = "mixed"
)

// Signal names collected by Inspect.
const (
	SignalRoleMarker    = "role_marker"
	SignalOverride      = "override_phrase"
	SignalToolDirective = "tool_directive"
	SignalDelimiter     = "delimiter_break"
)

// Reason codes carried by a tool decision.
const (
	ReasonAllowed      = "reason_allowed"
	ReasonToolUnknown  = "reason_tool_unknown"
	ReasonArgsTooLarge = "reason_args_too_large"
	ReasonRateLimited  = "reason_rate_limited"
)

// Errors returned by the gate.
var (
	ErrEmptyChunk      = errors.New("chunk id and origin are required")
	ErrUnknownChunk    = errors.New("unknown chunk")
	ErrChunkLimit      = errors.New("tracked chunk limit reached")
	ErrContextLimit    = errors.New("chunk limit reached for this context")
	ErrBlankReason     = errors.New("quarantine reason is required")
	ErrNotQuarantined  = errors.New("chunk is not quarantined")
	ErrToolDenied      = errors.New("tool call denied")
	ErrNoContext       = errors.New("context is not known to the gate")
	ErrUnknownDecision = errors.New("unknown decision")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Chunk is one segment of content offered to the model.
type Chunk struct {
	ID      string `json:"id"`
	Origin  Origin `json:"origin"`
	Label   string `json:"label"`
	Content string `json:"content"`
}

// Finding is the screening result of one chunk.
type Finding struct {
	ChunkID string   `json:"chunk_id"`
	Origin  Origin   `json:"origin"`
	Level   string   `json:"level"`
	Signals []string `json:"signals"`
}

// Context is the screened, labelled set of chunks handed to the model.
type Context struct {
	ID        string    `json:"id"`
	ChunkIDs  []string  `json:"chunk_ids"`
	Skipped   int       `json:"skipped"`
	Trust     string    `json:"trust"`
	CreatedAt time.Time `json:"created_at"`
}

// ToolCall is a request the model wants to execute.
type ToolCall struct {
	ID   string            `json:"id"`
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// Decision is the gate verdict for one tool call.
type Decision struct {
	ID        string    `json:"id"`
	ContextID string    `json:"context_id"`
	CallID    string    `json:"call_id"`
	Tool      string    `json:"tool"`
	Allowed   bool      `json:"allowed"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// Trace is the explainable record of one decision.
type Trace struct {
	DecisionID string    `json:"decision_id"`
	ContextID  string    `json:"context_id"`
	CallID     string    `json:"call_id"`
	Tool       string    `json:"tool"`
	Allowed    bool      `json:"allowed"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// Limits bounds the context size and the tool call rate.
type Limits struct {
	MaxChunksPerContext int
	MaxArgsBytes        int
	MaxCallsPerMinute   int
	MaxTrackedChunks    int
}

// PromptInjectionGate owns the chunk registry, quarantine list and rate window.
type PromptInjectionGate struct {
	limits Limits
	tools  map[string]bool
	now    func() time.Time
}

// NewPromptInjectionGate builds a gate with the given caps and tool whitelist.
func NewPromptInjectionGate(limits Limits, tools []string) *PromptInjectionGate {
	whitelist := make(map[string]bool, len(tools))
	for _, name := range tools {
		whitelist[name] = true
	}
	return &PromptInjectionGate{limits: limits, tools: whitelist, now: time.Now}
}

// SetClock replaces the clock used for the call rate window.
func (g *PromptInjectionGate) SetClock(now func() time.Time) {
	g.now = now
}

// Inspect screens one chunk and records it in the registry.
func (g *PromptInjectionGate) Inspect(chunk Chunk, origin Origin) (Finding, error) {
	return Finding{}, errNotImplemented
}

// BuildContext labels every chunk by origin, drops quarantined ones and
// returns the context the model may see.
func (g *PromptInjectionGate) BuildContext(chunks []Chunk) (Context, error) {
	return Context{}, errNotImplemented
}

// AuthorizeCall checks the tool whitelist, argument size and call rate.
func (g *PromptInjectionGate) AuthorizeCall(ctx Context, call ToolCall) (Decision, error) {
	return Decision{}, errNotImplemented
}

// Quarantine keeps a chunk out of every later context.
func (g *PromptInjectionGate) Quarantine(chunkID string, reason string) error {
	return errNotImplemented
}

// Release lifts a quarantine.
func (g *PromptInjectionGate) Release(chunkID string) error {
	return errNotImplemented
}

// Explain returns the stored trace of one decision, allowed or denied.
func (g *PromptInjectionGate) Explain(decisionID string) (Trace, error) {
	return Trace{}, errNotImplemented
}

// Quarantined lists the quarantined chunk ids in ascending order.
func (g *PromptInjectionGate) Quarantined() []string {
	return nil
}

// Tools lists the whitelisted tool names in ascending order.
func (g *PromptInjectionGate) Tools() []string {
	return nil
}

// Snapshot serialises the quarantine list, decision traces and rate window.
func (g *PromptInjectionGate) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PromptInjectionGate) Restore(data []byte) error {
	return errNotImplemented
}
