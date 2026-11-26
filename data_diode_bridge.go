// Package task models the send side of a one way import link: monotonic
// sequence allocation, a bounded retransmission window and generation tagged
// keys, with out of band acknowledgements.
package task

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Acknowledgement statuses understood by the bridge.
const (
	AckOK = "ok"
)

// Errors reported by the bridge.
var (
	ErrBadMeta         = errors.New("message metadata is incomplete")
	ErrEmptyPayload    = errors.New("payload must not be empty")
	ErrPayloadTooLarge = errors.New("payload is larger than the configured limit")
	ErrWindowFull      = errors.New("send window is full")
	ErrQueueFull       = errors.New("send queue capacity is exhausted")
	ErrUnknownSeq      = errors.New("sequence number is unknown or already pruned")
	ErrNoAckPending    = errors.New("no acknowledgement is pending")
	ErrBadAck          = errors.New("acknowledgement is malformed or belongs to another generation")
	ErrKeyGenExhausted = errors.New("key generation limit reached")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// Seq is the strictly monotonic message number.
type Seq uint64

// Meta describes the payload without opening it.
type Meta struct {
	Kind        string
	SourceID    string
	ContentType string
	Correlation string
}

// Message is one retained outbound message.
type Message struct {
	Seq        Seq
	Digest     string
	Generation int
	Size       int
	Payload    []byte
	Acked      bool
}

// Ack is one out of band confirmation.
type Ack struct {
	Seq        Seq
	Generation int
	Status     string
	Note       string
}

// Stats reports the queue, window and key state.
type Stats struct {
	Queued        int
	InFlight      int
	Acked         uint64
	BytesInFlight int64
	Watermark     Seq
	Generation    int
	Blocked       bool
}

// Config holds the hard limits of the bridge.
type Config struct {
	WindowMessages   int
	WindowBytes      int64
	MaxQueueMessages int
	MaxPayloadBytes  int
	MaxGeneration    int
	MaxPollBacklog   int
}

type queuedMessage struct {
	seq        Seq
	digest     string
	generation int
	size       int
	payload    []byte
	meta       Meta
	acked      bool
}

type generationRun struct {
	Start      Seq `json:"start"`
	End        Seq `json:"end"`
	Generation int `json:"generation"`
}

// DiodeBridge owns the send queue, the window and the key generation.
type DiodeBridge struct {
	mu sync.Mutex

	cfg Config

	generation int
	queue      map[Seq]*queuedMessage
	watermark  Seq
	lastSeq    Seq
	genRuns    []generationRun

	ackedTotal       uint64
	inFlightMessages int
	inFlightBytes    int64
	pollBacklog      []Ack
}

// NewDiodeBridge builds a bridge with the given limits.
func NewDiodeBridge(cfg Config) *DiodeBridge {
	return &DiodeBridge{
		cfg:        cfg,
		generation: 1,
		queue:      make(map[Seq]*queuedMessage),
	}
}

// Submit queues one payload and assigns the next sequence number.
func (b *DiodeBridge) Submit(payload []byte, meta Meta) (Seq, error) {
	if len(payload) == 0 {
		return 0, ErrEmptyPayload
	}
	if len(payload) > b.cfg.MaxPayloadBytes {
		return 0, ErrPayloadTooLarge
	}
	if strings.TrimSpace(meta.Kind) == "" || strings.TrimSpace(meta.SourceID) == "" {
		return 0, ErrBadMeta
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.queue) >= b.cfg.MaxQueueMessages {
		return 0, ErrQueueFull
	}
	if b.inFlightMessages >= b.cfg.WindowMessages {
		return 0, ErrWindowFull
	}
	size := int64(len(payload))
	if size > b.cfg.WindowBytes-b.inFlightBytes {
		return 0, ErrWindowFull
	}
	if b.lastSeq == ^Seq(0) {
		return 0, ErrQueueFull
	}

	seq := b.lastSeq + 1
	ownedPayload := append([]byte(nil), payload...)
	sum := sha256.Sum256(ownedPayload)
	ownedMeta := meta

	b.queue[seq] = &queuedMessage{
		seq:        seq,
		digest:     hex.EncodeToString(sum[:]),
		generation: b.generation,
		size:       len(ownedPayload),
		payload:    ownedPayload,
		meta:       ownedMeta,
	}
	b.lastSeq = seq
	b.inFlightMessages++
	b.inFlightBytes += size
	b.recordGenerationLocked(seq, b.generation)

	return seq, nil
}

// AcceptAck processes one acknowledgement from the out of band channel.
func (b *DiodeBridge) AcceptAck(ack Ack) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	generation, known := b.generationForSeqLocked(ack.Seq)
	if !known {
		return ErrUnknownSeq
	}
	if ack.Status != AckOK || ack.Generation != generation {
		return ErrBadAck
	}

	msg, retained := b.queue[ack.Seq]
	if !retained {
		if ack.Seq <= b.watermark {
			return nil
		}
		return ErrUnknownSeq
	}
	if msg.acked {
		return nil
	}

	msg.acked = true
	b.inFlightMessages--
	b.inFlightBytes -= int64(msg.size)
	b.ackedTotal++
	b.appendPollLocked(ack)
	b.advanceWatermarkLocked()
	return nil
}

// PollAck returns the next unread acknowledgement, oldest first.
func (b *DiodeBridge) PollAck() (Ack, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.pollBacklog) == 0 {
		return Ack{}, ErrNoAckPending
	}
	ack := b.pollBacklog[0]
	b.pollBacklog[0] = Ack{}
	b.pollBacklog = b.pollBacklog[1:]
	return ack, nil
}

// Stats reports queue, window and key counters.
func (b *DiodeBridge) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	return Stats{
		Queued:        len(b.queue),
		InFlight:      b.inFlightMessages,
		Acked:         b.ackedTotal,
		BytesInFlight: b.inFlightBytes,
		Watermark:     b.watermark,
		Generation:    b.generation,
		Blocked:       b.inFlightMessages >= b.cfg.WindowMessages || b.inFlightBytes >= b.cfg.WindowBytes,
	}
}

// RotateKey advances the key generation for new messages.
func (b *DiodeBridge) RotateKey() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.generation >= b.cfg.MaxGeneration {
		return ErrKeyGenExhausted
	}
	b.generation++
	return nil
}

// Generation returns the current key generation.
func (b *DiodeBridge) Generation() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.generation
}

// Lookup reads one retained message.
func (b *DiodeBridge) Lookup(seq Seq) (Message, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	msg, ok := b.queue[seq]
	if !ok {
		return Message{}, false
	}
	return cloneMessage(msg), true
}

// InFlight lists unacknowledged messages in sequence order.
func (b *DiodeBridge) InFlight() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	seqs := make([]Seq, 0, b.inFlightMessages)
	for seq, msg := range b.queue {
		if !msg.acked {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	messages := make([]Message, 0, len(seqs))
	for _, seq := range seqs {
		messages = append(messages, cloneMessage(b.queue[seq]))
	}
	return messages
}

// Resend requeues a retained message without changing its sequence number.
func (b *DiodeBridge) Resend(seq Seq) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.queue[seq]; !ok {
		return ErrUnknownSeq
	}
	return nil
}

type snapshotEnvelope struct {
	Version  int             `json:"version"`
	State    json.RawMessage `json:"state"`
	Checksum string          `json:"checksum"`
}

type snapshotMessage struct {
	Seq        Seq    `json:"seq"`
	Digest     string `json:"digest"`
	Generation int    `json:"generation"`
	Size       int    `json:"size"`
	Acked      bool   `json:"acked"`
	Payload    []byte `json:"payload"`
	Meta       Meta   `json:"meta"`
}

type snapshotState struct {
	LastSeq     Seq               `json:"last_seq"`
	Watermark   Seq               `json:"watermark"`
	Generation  int               `json:"generation"`
	Acked       uint64            `json:"acked"`
	Messages    []snapshotMessage `json:"messages"`
	GenRuns     []generationRun   `json:"generation_runs"`
	PollBacklog []Ack             `json:"poll_backlog"`
}

const snapshotVersion = 1

// Snapshot serialises the queue, the watermark and the key generation.
func (b *DiodeBridge) Snapshot() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	state := b.snapshotStateLocked()
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(stateBytes)
	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		State:    stateBytes,
		Checksum: hex.EncodeToString(sum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the bridge state, all or nothing.
func (b *DiodeBridge) Restore(data []byte) error {
	state, err := decodeSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.validateSnapshotLocked(state); err != nil {
		return ErrBadSnapshot
	}

	queue := make(map[Seq]*queuedMessage, len(state.Messages))
	var inFlightMessages int
	var inFlightBytes int64
	for _, snapshotMsg := range state.Messages {
		payload := append([]byte(nil), snapshotMsg.Payload...)
		msg := &queuedMessage{
			seq:        snapshotMsg.Seq,
			digest:     snapshotMsg.Digest,
			generation: snapshotMsg.Generation,
			size:       snapshotMsg.Size,
			payload:    payload,
			meta:       snapshotMsg.Meta,
			acked:      snapshotMsg.Acked,
		}
		queue[msg.seq] = msg
		if !msg.acked {
			inFlightMessages++
			inFlightBytes += int64(msg.size)
		}
	}

	b.lastSeq = state.LastSeq
	b.watermark = state.Watermark
	b.generation = state.Generation
	b.ackedTotal = state.Acked
	b.queue = queue
	b.genRuns = append([]generationRun(nil), state.GenRuns...)
	b.pollBacklog = append([]Ack(nil), state.PollBacklog...)
	b.inFlightMessages = inFlightMessages
	b.inFlightBytes = inFlightBytes
	return nil
}

func (b *DiodeBridge) recordGenerationLocked(seq Seq, generation int) {
	last := len(b.genRuns) - 1
	if last >= 0 && b.genRuns[last].Generation == generation {
		b.genRuns[last].End = seq
		return
	}
	b.genRuns = append(b.genRuns, generationRun{
		Start:      seq,
		End:        seq,
		Generation: generation,
	})
}

func (b *DiodeBridge) generationForSeqLocked(seq Seq) (int, bool) {
	if msg, ok := b.queue[seq]; ok {
		return msg.generation, true
	}
	if seq == 0 || seq > b.lastSeq {
		return 0, false
	}
	for _, run := range b.genRuns {
		if seq < run.Start {
			break
		}
		if seq <= run.End {
			return run.Generation, true
		}
	}
	return 0, false
}

func (b *DiodeBridge) appendPollLocked(ack Ack) {
	if b.cfg.MaxPollBacklog <= 0 {
		b.pollBacklog = append(b.pollBacklog, ack)
		return
	}
	if len(b.pollBacklog) < b.cfg.MaxPollBacklog {
		b.pollBacklog = append(b.pollBacklog, ack)
		return
	}
	copy(b.pollBacklog, b.pollBacklog[1:])
	b.pollBacklog[len(b.pollBacklog)-1] = ack
}

func (b *DiodeBridge) advanceWatermarkLocked() {
	oldWatermark := b.watermark
	for {
		if b.watermark == ^Seq(0) {
			break
		}
		next := b.watermark + 1
		msg, ok := b.queue[next]
		if !ok || !msg.acked {
			break
		}
		b.watermark = next
	}
	for seq := oldWatermark + 1; seq <= b.watermark; seq++ {
		delete(b.queue, seq)
	}
}

func cloneMessage(msg *queuedMessage) Message {
	return Message{
		Seq:        msg.seq,
		Digest:     msg.digest,
		Generation: msg.generation,
		Size:       msg.size,
		Payload:    append([]byte(nil), msg.payload...),
		Acked:      msg.acked,
	}
}

func (b *DiodeBridge) snapshotStateLocked() snapshotState {
	messages := make([]snapshotMessage, 0, len(b.queue))
	for _, msg := range b.queue {
		messages = append(messages, snapshotMessage{
			Seq:        msg.seq,
			Digest:     msg.digest,
			Generation: msg.generation,
			Size:       msg.size,
			Acked:      msg.acked,
			Payload:    append([]byte(nil), msg.payload...),
			Meta:       msg.meta,
		})
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].Seq < messages[j].Seq })

	return snapshotState{
		LastSeq:     b.lastSeq,
		Watermark:   b.watermark,
		Generation:  b.generation,
		Acked:       b.ackedTotal,
		Messages:    messages,
		GenRuns:     append([]generationRun(nil), b.genRuns...),
		PollBacklog: append([]Ack(nil), b.pollBacklog...),
	}
}

func decodeSnapshot(data []byte) (snapshotState, error) {
	var envelope snapshotEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return snapshotState{}, err
	}
	if envelope.Version != snapshotVersion || len(envelope.State) == 0 {
		return snapshotState{}, ErrBadSnapshot
	}

	sum := sha256.Sum256(envelope.State)
	want := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(want), []byte(envelope.Checksum)) != 1 {
		return snapshotState{}, ErrBadSnapshot
	}

	var state snapshotState
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return snapshotState{}, err
	}
	return state, nil
}

func (b *DiodeBridge) validateSnapshotLocked(state snapshotState) error {
	if state.Generation < 1 || state.Generation > b.cfg.MaxGeneration {
		return ErrBadSnapshot
	}
	if state.Watermark > state.LastSeq {
		return ErrBadSnapshot
	}
	if state.LastSeq == ^Seq(0) {
		return ErrBadSnapshot
	}
	if state.Acked < uint64(state.Watermark) || state.Acked > uint64(state.LastSeq) {
		return ErrBadSnapshot
	}
	if state.LastSeq-state.Watermark != Seq(len(state.Messages)) {
		return ErrBadSnapshot
	}
	if len(state.Messages) > b.cfg.MaxQueueMessages {
		return ErrBadSnapshot
	}
	if b.cfg.MaxPollBacklog > 0 && len(state.PollBacklog) > b.cfg.MaxPollBacklog {
		return ErrBadSnapshot
	}

	messages := append([]snapshotMessage(nil), state.Messages...)
	sort.Slice(messages, func(i, j int) bool { return messages[i].Seq < messages[j].Seq })

	var inFlightMessages int
	var inFlightBytes int64
	var retainedAcked uint64
	for i, msg := range messages {
		if msg.Seq != state.Watermark+Seq(i+1) {
			return ErrBadSnapshot
		}
		if msg.Size <= 0 || msg.Size != len(msg.Payload) || msg.Size > b.cfg.MaxPayloadBytes {
			return ErrBadSnapshot
		}
		sum := sha256.Sum256(msg.Payload)
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(msg.Digest)) != 1 {
			return ErrBadSnapshot
		}
		if msg.Generation < 1 || msg.Generation > state.Generation {
			return ErrBadSnapshot
		}
		if i == 0 && msg.Acked {
			return ErrBadSnapshot
		}
		if msg.Acked {
			retainedAcked++
		} else {
			inFlightMessages++
			inFlightBytes += int64(msg.Size)
		}
	}
	if state.Acked != uint64(state.Watermark)+retainedAcked {
		return ErrBadSnapshot
	}
	if inFlightMessages > b.cfg.WindowMessages || inFlightBytes > b.cfg.WindowBytes {
		return ErrBadSnapshot
	}
	if err := validateGenerationRuns(state.GenRuns, state.LastSeq, state.Generation); err != nil {
		return ErrBadSnapshot
	}
	for _, msg := range messages {
		generation, ok := generationAt(state.GenRuns, msg.Seq)
		if !ok || generation != msg.Generation {
			return ErrBadSnapshot
		}
	}
	for _, ack := range state.PollBacklog {
		if ack.Status != AckOK {
			return ErrBadSnapshot
		}
		generation, ok := generationAt(state.GenRuns, ack.Seq)
		if !ok || generation != ack.Generation {
			return ErrBadSnapshot
		}
	}
	return nil
}

func validateGenerationRuns(runs []generationRun, lastSeq Seq, generation int) error {
	if lastSeq == 0 {
		if len(runs) != 0 {
			return ErrBadSnapshot
		}
		return nil
	}
	if len(runs) == 0 || runs[0].Start != 1 {
		return ErrBadSnapshot
	}

	var expectedStart Seq = 1
	previousGeneration := 0
	for _, run := range runs {
		if run.Start != expectedStart || run.End < run.Start {
			return ErrBadSnapshot
		}
		if run.Generation < 1 || run.Generation > generation || run.Generation < previousGeneration {
			return ErrBadSnapshot
		}
		expectedStart = run.End + 1
		previousGeneration = run.Generation
	}
	if runs[len(runs)-1].End != lastSeq {
		return ErrBadSnapshot
	}
	return nil
}

func generationAt(runs []generationRun, seq Seq) (int, bool) {
	if seq == 0 {
		return 0, false
	}
	for _, run := range runs {
		if seq < run.Start {
			break
		}
		if seq <= run.End {
			return run.Generation, true
		}
	}
	return 0, false
}
