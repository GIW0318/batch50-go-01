// Package task screens external content before it is folded into a model
// context and authorises the tool calls the model asks for.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

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

// ContextReference records that a user chunk explicitly referenced a
// quarantined chunk. Only Summary is allowed to reach the model.
type ContextReference struct {
	QuarantinedChunkID string `json:"quarantined_chunk_id"`
	SourceChunkID      string `json:"source_chunk_id"`
	Summary            string `json:"summary"`
}

// ContextSegment is the labelled representation of one retained chunk.
type ContextSegment struct {
	Position   int      `json:"position"`
	ChunkID    string   `json:"chunk_id"`
	Origin     Origin   `json:"origin"`
	Trust      string   `json:"trust"`
	Summary    bool     `json:"summary"`
	Content    string   `json:"content,omitempty"`
	References []string `json:"references,omitempty"`
}

// Context is the screened, labelled set of chunks handed to the model.
type Context struct {
	ID         string             `json:"id"`
	ChunkIDs   []string           `json:"chunk_ids"`
	Skipped    int                `json:"skipped"`
	Trust      string             `json:"trust"`
	CreatedAt  time.Time          `json:"created_at"`
	Segments   []ContextSegment   `json:"segments,omitempty"`
	References []ContextReference `json:"references,omitempty"`
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

type trackedChunk struct {
	chunk       Chunk
	finding     Finding
	quarantined bool
	reason      string
	quoteLength int
	quoteHashes map[uint64]struct{}
}

// PromptInjectionGate owns the chunk registry, quarantine list and rate window.
type PromptInjectionGate struct {
	mu sync.Mutex

	limits Limits
	tools  map[string]bool
	now    func() time.Time

	chunks    map[string]*trackedChunk
	contexts  map[string]Context
	decisions map[string]Trace
	rate      []time.Time

	nextContext  int
	nextDecision int
}

// NewPromptInjectionGate builds a gate with the given caps and tool whitelist.
func NewPromptInjectionGate(limits Limits, tools []string) *PromptInjectionGate {
	limits = normaliseLimits(limits)
	whitelist := make(map[string]bool, len(tools))
	for _, name := range tools {
		whitelist[name] = true
	}
	return &PromptInjectionGate{
		limits:    limits,
		tools:     whitelist,
		now:       time.Now,
		chunks:    make(map[string]*trackedChunk),
		contexts:  make(map[string]Context),
		decisions: make(map[string]Trace),
	}
}

func normaliseLimits(limits Limits) Limits {
	if limits.MaxChunksPerContext < 0 {
		limits.MaxChunksPerContext = 0
	}
	if limits.MaxArgsBytes < 0 {
		limits.MaxArgsBytes = 0
	}
	if limits.MaxCallsPerMinute < 0 {
		limits.MaxCallsPerMinute = 0
	}
	if limits.MaxTrackedChunks < 0 {
		limits.MaxTrackedChunks = 0
	}
	return limits
}

// SetClock replaces the clock used for the call rate window.
func (g *PromptInjectionGate) SetClock(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now == nil {
		g.now = time.Now
		return
	}
	g.now = now
}

func (g *PromptInjectionGate) nowLocked() time.Time {
	if g.now == nil {
		return time.Now()
	}
	return g.now()
}

// Inspect screens one chunk and records it in the registry.
func (g *PromptInjectionGate) Inspect(chunk Chunk, origin Origin) (Finding, error) {
	if strings.TrimSpace(chunk.ID) == "" || strings.TrimSpace(string(origin)) == "" {
		return Finding{}, ErrEmptyChunk
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.chunks[chunk.ID]; !exists && len(g.chunks) >= g.limits.MaxTrackedChunks {
		return Finding{}, ErrChunkLimit
	}

	chunk.Origin = origin
	finding := inspectContent(chunk.ID, origin, chunk.Content)
	record, exists := g.chunks[chunk.ID]
	if !exists {
		record = &trackedChunk{}
		g.chunks[chunk.ID] = record
	}
	record.chunk = chunk
	record.finding = finding
	return cloneFinding(finding), nil
}

// BuildContext labels every chunk by origin, drops quarantined ones and
// returns the context the model may see.
func (g *PromptInjectionGate) BuildContext(chunks []Chunk) (Context, error) {
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.ID) == "" || strings.TrimSpace(string(chunk.Origin)) == "" {
			return Context{}, ErrEmptyChunk
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if len(chunks) > g.limits.MaxChunksPerContext {
		return Context{}, ErrContextLimit
	}

	newIDs := make(map[string]struct{})
	for _, chunk := range chunks {
		if _, exists := g.chunks[chunk.ID]; !exists {
			newIDs[chunk.ID] = struct{}{}
		}
	}
	if len(g.chunks)+len(newIDs) > g.limits.MaxTrackedChunks {
		return Context{}, ErrChunkLimit
	}

	// Screen and retain every supplied chunk. A build is all-or-nothing with
	// respect to the tracked-chunk cap: validation happens before this commit.
	stagedFindings := make([]Finding, len(chunks))
	for i, chunk := range chunks {
		stagedFindings[i] = inspectContent(chunk.ID, chunk.Origin, chunk.Content)
	}
	for i, chunk := range chunks {
		record, exists := g.chunks[chunk.ID]
		if !exists {
			record = &trackedChunk{}
			g.chunks[chunk.ID] = record
		}
		record.chunk = chunk
		record.finding = stagedFindings[i]
	}

	ctx := Context{
		ID:        g.nextContextIDLocked(),
		ChunkIDs:  make([]string, 0, len(chunks)),
		CreatedAt: g.nowLocked(),
		Trust:     TrustUntrusted,
	}

	seenTrust := make(map[string]struct{}, 3)
	for i, chunk := range chunks {
		record := g.chunks[chunk.ID]
		if record.quarantined {
			ctx.Skipped++
			continue
		}

		var refs []ContextReference
		if chunk.Origin == OriginUser {
			refs = g.referencesFromLocked(chunk)
		}

		if stagedFindings[i].Level == LevelBlocked && len(refs) == 0 {
			ctx.Skipped++
			continue
		}

		trust := trustForOrigin(chunk.Origin)
		seenTrust[trust] = struct{}{}

		segment := ContextSegment{
			Position: i,
			ChunkID:  chunk.ID,
			Origin:   chunk.Origin,
			Trust:    trust,
			Content:  chunk.Content,
		}
		if len(refs) > 0 {
			segment.Summary = true
			segment.Content = summariseReferences(chunk, refs)
			for _, ref := range refs {
				segment.References = append(segment.References, ref.QuarantinedChunkID)
			}
			ctx.References = append(ctx.References, refs...)
		}

		ctx.ChunkIDs = append(ctx.ChunkIDs, chunk.ID)
		ctx.Segments = append(ctx.Segments, segment)
	}

	switch len(seenTrust) {
	case 0:
		ctx.Trust = TrustUntrusted
	case 1:
		for trust := range seenTrust {
			ctx.Trust = trust
		}
	default:
		ctx.Trust = TrustMixed
	}

	ctx.ChunkIDs = append([]string(nil), ctx.ChunkIDs...)
	ctx.Segments = cloneSegments(ctx.Segments)
	ctx.References = cloneReferences(ctx.References)
	g.contexts[ctx.ID] = ctx
	return cloneContext(ctx), nil
}

// AuthorizeCall checks the tool whitelist, argument size and call rate.
func (g *PromptInjectionGate) AuthorizeCall(ctx Context, call ToolCall) (Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if strings.TrimSpace(ctx.ID) == "" {
		return Decision{}, ErrNoContext
	}

	now := g.nowLocked()
	decision := Decision{
		ID:        g.nextDecisionIDLocked(),
		ContextID: ctx.ID,
		CallID:    call.ID,
		Tool:      call.Name,
		At:        now,
	}

	switch {
	case !g.tools[call.Name]:
		decision.Reason = ReasonToolUnknown
	case argsBytes(call.Args) > g.limits.MaxArgsBytes:
		decision.Reason = ReasonArgsTooLarge
	default:
		g.pruneRateLocked(now)
		if len(g.rate) >= g.limits.MaxCallsPerMinute {
			decision.Reason = ReasonRateLimited
		} else {
			decision.Allowed = true
			decision.Reason = ReasonAllowed
			g.rate = append(g.rate, now)
		}
	}

	trace := Trace{
		DecisionID: decision.ID,
		ContextID:  decision.ContextID,
		CallID:     decision.CallID,
		Tool:       decision.Tool,
		Allowed:    decision.Allowed,
		Reason:     decision.Reason,
		At:         decision.At,
	}
	g.decisions[decision.ID] = trace
	if !decision.Allowed {
		return decision, ErrToolDenied
	}
	return decision, nil
}

// Quarantine keeps a chunk out of every later context.
func (g *PromptInjectionGate) Quarantine(chunkID string, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrBlankReason
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	record, exists := g.chunks[chunkID]
	if !exists {
		return ErrUnknownChunk
	}
	if record.quarantined {
		return nil
	}
	record.quarantined = true
	record.reason = reason
	return nil
}

// Release lifts a quarantine.
func (g *PromptInjectionGate) Release(chunkID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	record, exists := g.chunks[chunkID]
	if !exists || !record.quarantined {
		return ErrNotQuarantined
	}
	record.quarantined = false
	record.reason = ""
	return nil
}

// Explain returns the stored trace of one decision, allowed or denied.
func (g *PromptInjectionGate) Explain(decisionID string) (Trace, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	trace, exists := g.decisions[decisionID]
	if !exists {
		return Trace{}, ErrUnknownDecision
	}
	return trace, nil
}

// Quarantined lists the quarantined chunk ids in ascending order.
func (g *PromptInjectionGate) Quarantined() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ids := make([]string, 0)
	for id, record := range g.chunks {
		if record.quarantined {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Tools lists the whitelisted tool names in ascending order.
func (g *PromptInjectionGate) Tools() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	names := make([]string, 0, len(g.tools))
	for name := range g.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

const snapshotVersion = 1

type snapshotQuarantine struct {
	ChunkID string `json:"chunk_id"`
	Reason  string `json:"reason"`
}

type snapshotChunk struct {
	ID          string   `json:"id"`
	Origin      Origin   `json:"origin"`
	Label       string   `json:"label"`
	Content     string   `json:"content,omitempty"`
	Level       string   `json:"level"`
	Signals     []string `json:"signals"`
	Quarantined bool     `json:"quarantined"`
	Reason      string   `json:"reason,omitempty"`
	QuoteLength int      `json:"quote_length,omitempty"`
	QuoteHashes []uint64 `json:"quote_hashes,omitempty"`
}

type snapshotState struct {
	Version      int                  `json:"version"`
	Quarantines  []snapshotQuarantine `json:"quarantines"`
	Chunks       []snapshotChunk      `json:"chunks"`
	Contexts     []Context            `json:"contexts"`
	Decisions    []Trace              `json:"decisions"`
	RateWindow   []time.Time          `json:"rate_window"`
	References   []ContextReference   `json:"references,omitempty"`
	NextContext  int                  `json:"next_context"`
	NextDecision int                  `json:"next_decision"`
}

// Snapshot serialises the quarantine list, decision traces and rate window.
func (g *PromptInjectionGate) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state := snapshotState{
		Version:      snapshotVersion,
		Quarantines:  make([]snapshotQuarantine, 0),
		Chunks:       make([]snapshotChunk, 0, len(g.chunks)),
		Contexts:     make([]Context, 0, len(g.contexts)),
		Decisions:    make([]Trace, 0, len(g.decisions)),
		RateWindow:   append([]time.Time(nil), g.rate...),
		NextContext:  g.nextContext,
		NextDecision: g.nextDecision,
	}

	for _, record := range g.chunks {
		// Payloads are not part of the durable safety state. Contexts are
		// rebuilt from supplied chunks, while length-matched fingerprints let
		// quarantine references survive a restore without storing raw text.
		quoteLength, quoteHashes := record.quoteLength, fingerprintValues(record.quoteHashes)
		if record.chunk.Content != "" {
			quoteLength, quoteHashes = quoteFingerprint(record.chunk.Content)
		}
		sc := snapshotChunk{
			ID:          record.chunk.ID,
			Origin:      record.chunk.Origin,
			Label:       record.chunk.Label,
			Level:       record.finding.Level,
			Signals:     append([]string(nil), record.finding.Signals...),
			Quarantined: record.quarantined,
			Reason:      record.reason,
			QuoteLength: quoteLength,
			QuoteHashes: quoteHashes,
		}
		state.Chunks = append(state.Chunks, sc)
		if record.quarantined {
			state.Quarantines = append(state.Quarantines, snapshotQuarantine{ChunkID: record.chunk.ID, Reason: record.reason})
		}
	}
	for _, ctx := range g.contexts {
		snapshotCtx := cloneContext(ctx)
		for i := range snapshotCtx.Segments {
			snapshotCtx.Segments[i].Content = ""
		}
		state.Contexts = append(state.Contexts, snapshotCtx)
		state.References = append(state.References, cloneReferences(ctx.References)...)
	}
	for _, trace := range g.decisions {
		state.Decisions = append(state.Decisions, trace)
	}

	sort.Slice(state.Quarantines, func(i, j int) bool { return state.Quarantines[i].ChunkID < state.Quarantines[j].ChunkID })
	sort.Slice(state.Chunks, func(i, j int) bool { return state.Chunks[i].ID < state.Chunks[j].ID })
	sort.Slice(state.Contexts, func(i, j int) bool { return state.Contexts[i].ID < state.Contexts[j].ID })
	sort.Slice(state.Decisions, func(i, j int) bool { return state.Decisions[i].DecisionID < state.Decisions[j].DecisionID })
	sort.Slice(state.References, func(i, j int) bool {
		if state.References[i].SourceChunkID != state.References[j].SourceChunkID {
			return state.References[i].SourceChunkID < state.References[j].SourceChunkID
		}
		return state.References[i].QuarantinedChunkID < state.References[j].QuarantinedChunkID
	})
	state.References = dedupeReferences(state.References)
	sort.Slice(state.RateWindow, func(i, j int) bool { return state.RateWindow[i].Before(state.RateWindow[j]) })

	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *PromptInjectionGate) Restore(data []byte) error {
	decoded, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.chunks = decoded.chunks
	g.contexts = decoded.contexts
	g.decisions = decoded.decisions
	g.rate = decoded.rate
	g.nextContext = decoded.nextContext
	g.nextDecision = decoded.nextDecision
	return nil
}

type decodedState struct {
	chunks       map[string]*trackedChunk
	contexts     map[string]Context
	decisions    map[string]Trace
	rate         []time.Time
	nextContext  int
	nextDecision int
}

func decodeSnapshot(data []byte) (decodedState, error) {
	var state snapshotState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return decodedState{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return decodedState{}, ErrBadSnapshot
	}
	if state.Version != snapshotVersion {
		return decodedState{}, ErrBadSnapshot
	}

	decoded := decodedState{
		chunks:       make(map[string]*trackedChunk, len(state.Chunks)),
		contexts:     make(map[string]Context, len(state.Contexts)),
		decisions:    make(map[string]Trace, len(state.Decisions)),
		rate:         append([]time.Time(nil), state.RateWindow...),
		nextContext:  state.NextContext,
		nextDecision: state.NextDecision,
	}
	if decoded.nextContext < 0 || decoded.nextDecision < 0 {
		return decodedState{}, ErrBadSnapshot
	}

	for _, chunk := range state.Chunks {
		if strings.TrimSpace(chunk.ID) == "" || strings.TrimSpace(string(chunk.Origin)) == "" {
			return decodedState{}, ErrBadSnapshot
		}
		if _, exists := decoded.chunks[chunk.ID]; exists {
			return decodedState{}, ErrBadSnapshot
		}
		if !validLevel(chunk.Level) || !validSignals(chunk.Signals) {
			return decodedState{}, ErrBadSnapshot
		}
		if chunk.QuoteLength < 0 {
			return decodedState{}, ErrBadSnapshot
		}
		record := &trackedChunk{
			chunk: Chunk{
				ID:      chunk.ID,
				Origin:  chunk.Origin,
				Label:   chunk.Label,
				Content: chunk.Content,
			},
			finding: Finding{
				ChunkID: chunk.ID,
				Origin:  chunk.Origin,
				Level:   chunk.Level,
				Signals: append([]string(nil), chunk.Signals...),
			},
			quarantined: chunk.Quarantined,
			reason:      chunk.Reason,
			quoteLength: chunk.QuoteLength,
			quoteHashes: make(map[uint64]struct{}, len(chunk.QuoteHashes)),
		}
		for _, hash := range chunk.QuoteHashes {
			record.quoteHashes[hash] = struct{}{}
		}
		sort.Strings(record.finding.Signals)
		if record.quarantined && strings.TrimSpace(record.reason) == "" {
			return decodedState{}, ErrBadSnapshot
		}
		decoded.chunks[chunk.ID] = record
	}

	seenQuarantine := make(map[string]struct{}, len(state.Quarantines))
	for _, quarantine := range state.Quarantines {
		if strings.TrimSpace(quarantine.ChunkID) == "" || strings.TrimSpace(quarantine.Reason) == "" {
			return decodedState{}, ErrBadSnapshot
		}
		if _, duplicate := seenQuarantine[quarantine.ChunkID]; duplicate {
			return decodedState{}, ErrBadSnapshot
		}
		record, exists := decoded.chunks[quarantine.ChunkID]
		if !exists {
			return decodedState{}, ErrBadSnapshot
		}
		record.quarantined = true
		record.reason = quarantine.Reason
		seenQuarantine[quarantine.ChunkID] = struct{}{}
	}
	for _, record := range decoded.chunks {
		if record.quarantined {
			seenQuarantine[record.chunk.ID] = struct{}{}
		}
	}

	for _, ctx := range state.Contexts {
		if strings.TrimSpace(ctx.ID) == "" {
			return decodedState{}, ErrBadSnapshot
		}
		if _, duplicate := decoded.contexts[ctx.ID]; duplicate {
			return decodedState{}, ErrBadSnapshot
		}
		for _, id := range ctx.ChunkIDs {
			if strings.TrimSpace(id) == "" {
				return decodedState{}, ErrBadSnapshot
			}
		}
		for _, ref := range ctx.References {
			if strings.TrimSpace(ref.QuarantinedChunkID) == "" || strings.TrimSpace(ref.SourceChunkID) == "" {
				return decodedState{}, ErrBadSnapshot
			}
		}
		decoded.contexts[ctx.ID] = cloneContext(ctx)
	}

	for _, trace := range state.Decisions {
		if strings.TrimSpace(trace.DecisionID) == "" {
			return decodedState{}, ErrBadSnapshot
		}
		if _, duplicate := decoded.decisions[trace.DecisionID]; duplicate {
			return decodedState{}, ErrBadSnapshot
		}
		decoded.decisions[trace.DecisionID] = trace
	}

	sort.Slice(decoded.rate, func(i, j int) bool { return decoded.rate[i].Before(decoded.rate[j]) })
	return decoded, nil
}

func (g *PromptInjectionGate) nextContextIDLocked() string {
	for {
		g.nextContext++
		id := fmt.Sprintf("ctx-%d", g.nextContext)
		if _, exists := g.contexts[id]; !exists {
			return id
		}
	}
}

func (g *PromptInjectionGate) nextDecisionIDLocked() string {
	for {
		g.nextDecision++
		id := fmt.Sprintf("dec-%d", g.nextDecision)
		if _, exists := g.decisions[id]; !exists {
			return id
		}
	}
}

func (g *PromptInjectionGate) pruneRateLocked(now time.Time) {
	cutoff := now.Add(-time.Minute)
	kept := g.rate[:0]
	for _, at := range g.rate {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	g.rate = kept
}

func argsBytes(args map[string]string) int {
	total := 0
	for key, value := range args {
		total += len(key) + len(value)
	}
	return total
}

func (g *PromptInjectionGate) referencesFromLocked(source Chunk) []ContextReference {
	refs := make([]ContextReference, 0)
	for _, record := range g.chunks {
		if !record.quarantined || record.chunk.ID == source.ID {
			continue
		}
		explicit := containsChunkReference(source.Content, record.chunk.ID)
		quoted := containsQuotedContent(source.Content, record)
		if !explicit && !quoted {
			continue
		}
		summary := summaryFor(record)
		refs = append(refs, ContextReference{
			QuarantinedChunkID: record.chunk.ID,
			SourceChunkID:      source.ID,
			Summary:            summary,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].QuarantinedChunkID < refs[j].QuarantinedChunkID })
	return dedupeReferences(refs)
}

func containsChunkReference(content string, chunkID string) bool {
	if chunkID == "" {
		return false
	}
	for start := 0; start <= len(content)-len(chunkID); {
		index := strings.Index(content[start:], chunkID)
		if index < 0 {
			return false
		}
		index += start
		beforeOK := index == 0 || !isReferenceWordByte(content[index-1])
		after := index + len(chunkID)
		afterOK := after == len(content) || !isReferenceWordByte(content[after])
		if beforeOK && afterOK {
			return true
		}
		start = index + 1
	}
	return false
}

func isReferenceWordByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func containsQuotedContent(userContent string, record *trackedChunk) bool {
	quarantinedContent := record.chunk.Content
	if quarantinedContent == "" {
		return containsFingerprintQuote(userContent, record.quoteLength, record.quoteHashes)
	}
	if len(userContent) < 8 || len(quarantinedContent) < 8 {
		return false
	}
	threshold := quoteThreshold(quarantinedContent)
	return longestCommonSubstringLen(userContent, quarantinedContent) >= threshold
}

func quoteFingerprint(content string) (int, []uint64) {
	length := quoteThreshold(content)
	if length <= 0 || len(content) < length {
		return 0, nil
	}
	seen := make(map[uint64]struct{}, len(content)-length+1)
	for start := 0; start+length <= len(content); start++ {
		seen[hashWindow(content[start:start+length])] = struct{}{}
	}
	hashes := make([]uint64, 0, len(seen))
	for hash := range seen {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	return length, hashes
}

func fingerprintValues(hashes map[uint64]struct{}) []uint64 {
	values := make([]uint64, 0, len(hashes))
	for hash := range hashes {
		values = append(values, hash)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values
}

func containsFingerprintQuote(content string, length int, hashes map[uint64]struct{}) bool {
	if length <= 0 || len(hashes) == 0 || len(content) < length {
		return false
	}
	for start := 0; start+length <= len(content); start++ {
		if _, exists := hashes[hashWindow(content[start:start+length])]; exists {
			return true
		}
	}
	return false
}

func hashWindow(window string) uint64 {
	const (
		offset64 = uint64(14695981039346656037)
		prime64  = uint64(1099511628211)
	)
	hash := offset64
	for i := 0; i < len(window); i++ {
		hash ^= uint64(window[i])
		hash *= prime64
	}
	return hash
}

func quoteThreshold(content string) int {
	threshold := len(content) / 3
	if threshold < 8 {
		threshold = 8
	}
	if threshold > 64 {
		threshold = 64
	}
	if threshold > len(content) {
		threshold = len(content)
	}
	return threshold
}

func longestCommonSubstringLen(a string, b string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Keep the DP row on the shorter side. The task deals with prose-sized
	// chunks, and this avoids quadratic memory.
	if len(a) < len(b) {
		a, b = b, a
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	best := 0
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				current[j] = previous[j-1] + 1
				if current[j] > best {
					best = current[j]
				}
			} else {
				current[j] = 0
			}
		}
		previous, current = current, previous
		for j := range current {
			current[j] = 0
		}
	}
	return best
}

func summaryFor(record *trackedChunk) string {
	label := strings.TrimSpace(record.chunk.Label)
	if label == "" {
		label = "content"
	}
	return fmt.Sprintf("[summary of quarantined chunk %s (%s); original withheld]", record.chunk.ID, label)
}

func summariseReferences(source Chunk, refs []ContextReference) string {
	if len(refs) == 0 {
		return source.Content
	}
	parts := make([]string, 0, len(refs)+1)
	for _, ref := range refs {
		parts = append(parts, ref.Summary)
	}
	return strings.Join(parts, " ")
}

func trustForOrigin(origin Origin) string {
	switch origin {
	case OriginSystem:
		return TrustTrusted
	case OriginUser:
		return TrustMixed
	default:
		return TrustUntrusted
	}
}

func inspectContent(chunkID string, origin Origin, content string) Finding {
	signals := []string{}
	// System content is authored by the operator rather than supplied as
	// untrusted material. User, tool and external content all go through the
	// same lexical screen; their trust labels still differ in BuildContext.
	if origin != OriginSystem {
		signals = detectSignals(content)
	}
	level := LevelClean
	switch {
	case len(signals) == 0:
		level = LevelClean
	case hasSignal(signals, SignalRoleMarker) || len(signals) >= 2:
		level = LevelBlocked
	default:
		level = LevelSuspicious
	}
	return Finding{
		ChunkID: chunkID,
		Origin:  origin,
		Level:   level,
		Signals: signals,
	}
}

func hasSignal(signals []string, wanted string) bool {
	for _, signal := range signals {
		if signal == wanted {
			return true
		}
	}
	return false
}

var (
	rolePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)<\|(?:im_start|im_end|endoftext|system|user|assistant|tool|developer)\|>`),
		regexp.MustCompile(`(?i)<\s*/?\s*(?:system|assistant|developer|tool)\s*>`),
		regexp.MustCompile(`(?i)\[/?INST\]|<<\s*/?\s*SYS\s*>>`),
		regexp.MustCompile(`(?im)^\s*(?:system|assistant|developer|tool)\s*:`),
		regexp.MustCompile(`(?i)\brole\s*["']?\s*[:=]\s*["']?(?:system|assistant|developer|tool)\b`),
		regexp.MustCompile(`(?im)^\s*#{3,}\s*(?:system|assistant|developer|tool)\s*$`),
		regexp.MustCompile(`(?im)^\s*\[(?:system|assistant|developer|tool)\]\s*`),
		regexp.MustCompile(`(?im)^\s*(?:begin|end)?\s*(?:system|developer)\s+(?:prompt|message)\s*:?\s*$`),
		regexp.MustCompile(`(?i)\b(?:you are|act as|pretend to be)\s+(?:now\s+)?(?:the\s+)?(?:system|developer|assistant|root(?:\s+operator)?|admin(?:istrator)?\s+operator|dan)\b`),
	}

	overridePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override|overrule|bypass)\b[^\n.!?]{0,60}?\b(?:previous|prior|above|earlier|former|original|system|developer)\b[^\n.!?]{0,40}?\b(?:instruction|instructions|prompt|prompts|rule|rules|message|messages|direction|directions|guideline|guidelines)\b`),
		regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget)\s+(?:all\s+)?(?:instructions?|prompts?|rules?|messages?)\b`),
		regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget)\s+(?:everything|anything)\s+(?:above|before|previous|prior)\b`),
		regexp.MustCompile(`(?i)\b(?:ignore|disregard)\s+(?:the\s+)?(?:instructions?|prompts?|rules?)\s+(?:above|before|previous|prior|earlier)\b`),
		regexp.MustCompile(`(?i)\bdo\s+not\s+(?:follow|obey)\b[^\n.!?]{0,40}\b(?:instructions?|prompts?|rules?)\b`),
		regexp.MustCompile(`(?i)\b(?:new|updated|hidden|secret)\s+(?:system\s+)?(?:instructions?|prompt)\s*:`),
		regexp.MustCompile(`(?i)(?:忽略|无视|忘记|忘掉|覆盖|绕过|越过)[^\n。！？]{0,30}(?:之前|以上|前面|先前|原有|系统|全部)[^\n。！？]{0,20}(?:指令|提示|规则|要求|设定|消息)`),
		regexp.MustCompile(`(?i)(?:忽略|无视|忘记)(?:所有)?(?:指令|提示|规则|要求)`),
	}

	toolPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:call|invoke|execute|run|trigger|use|perform)\b[^\n.!?]{0,40}\b(?:tool|function|api|endpoint|command|shell|delete|remove|drop|update|write|create)\b`),
		regexp.MustCompile(`(?i)\b(?:tool_calls?|function_call|tool_call)\b`),
		regexp.MustCompile(`(?i)\b(?:delete|remove|drop|update|write|create|exec)\w*\s+(?:api|endpoint|tool|function|command)\b`),
		regexp.MustCompile(`(?i)\b(?:call|invoke|execute|run|trigger)\s+(?:the\s+)?(?:delete|remove|drop|update|write|create|exec|shell)[a-z0-9_]*\b`),
		regexp.MustCompile(`(?i)\b(?:delete|remove|drop|update|write|create|exec)[_-](?:api|endpoint|tool|function|command|user|customer|config|configuration|record|file|database|table)\b`),
		regexp.MustCompile(`(?i)(?:调用|执行|运行|使用|触发)[^\n。！？]{0,30}(?:工具|函数|接口|命令|API|删除|移除|写入|更新)`),
		regexp.MustCompile(`(?im)^\s*(?:action|tool|function|call)\s*:\s*(?:delete|remove|drop|exec|execute|shell|call|invoke)\b`),
	}

	delimiterPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?im)^\s*(?:-{3,}|={3,}|\*{3,})\s*(?:begin|end|start|stop)?\s*(?:system|developer|assistant|user|instructions?|prompt|context)\b.*(?:-{3,}|={3,}|\*{3,})?\s*$`),
		regexp.MustCompile(`(?im)^\s*(?:begin|end)\s+(?:system|developer|instructions?|prompt|context)\b`),
		regexp.MustCompile("(?m)^\\s*```\\s*(?:system|developer|instructions?|prompt)\\s*$"),
		regexp.MustCompile("(?m)^\\s*```\\s*$"),
		regexp.MustCompile(`(?m)^\s*(?:-{3,}|={3,}|\*{3,})\s*$`),
		regexp.MustCompile(`(?m)^\s*#{3,}\s*$`),
		regexp.MustCompile(`(?i)<\s*/?\s*(?:instructions?|prompt|context)\s*>`),
	}
)

func detectSignals(content string) []string {
	seen := make(map[string]struct{}, 4)
	if anyPatternMatches(rolePatterns, content) {
		seen[SignalRoleMarker] = struct{}{}
	}
	if anyPatternMatches(overridePatterns, content) {
		seen[SignalOverride] = struct{}{}
	}
	if anyPatternMatches(toolPatterns, content) {
		seen[SignalToolDirective] = struct{}{}
	}
	if anyPatternMatches(delimiterPatterns, content) {
		seen[SignalDelimiter] = struct{}{}
	}
	signals := make([]string, 0, len(seen))
	for signal := range seen {
		signals = append(signals, signal)
	}
	sort.Strings(signals)
	return signals
}

func anyPatternMatches(patterns []*regexp.Regexp, content string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}

func validLevel(level string) bool {
	switch level {
	case LevelClean, LevelSuspicious, LevelBlocked:
		return true
	default:
		return false
	}
}

func validSignals(signals []string) bool {
	for _, signal := range signals {
		switch signal {
		case SignalRoleMarker, SignalOverride, SignalToolDirective, SignalDelimiter:
		default:
			return false
		}
	}
	return true
}

func cloneFinding(finding Finding) Finding {
	finding.Signals = append([]string(nil), finding.Signals...)
	return finding
}

func cloneSegments(segments []ContextSegment) []ContextSegment {
	if segments == nil {
		return nil
	}
	cloned := make([]ContextSegment, len(segments))
	for i, segment := range segments {
		cloned[i] = segment
		cloned[i].References = append([]string(nil), segment.References...)
	}
	return cloned
}

func cloneReferences(refs []ContextReference) []ContextReference {
	return append([]ContextReference(nil), refs...)
}

func cloneContext(ctx Context) Context {
	ctx.ChunkIDs = append([]string(nil), ctx.ChunkIDs...)
	ctx.Segments = cloneSegments(ctx.Segments)
	ctx.References = cloneReferences(ctx.References)
	return ctx
}

func dedupeReferences(refs []ContextReference) []ContextReference {
	if len(refs) == 0 {
		return []ContextReference{}
	}
	seen := make(map[string]struct{}, len(refs))
	deduped := make([]ContextReference, 0, len(refs))
	for _, ref := range refs {
		key := ref.SourceChunkID + "\x00" + ref.QuarantinedChunkID + "\x00" + ref.Summary
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, ref)
	}
	return deduped
}
