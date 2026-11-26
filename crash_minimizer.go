// Package task shrinks a crashing input down to the shortest form that still
// reaches the same crash cluster: every cut is replayed, the best input only
// moves forward after the replay stays in the original cluster, and the replay
// budget is a hard stop.
package task

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/adler32"
	"hash/crc32"
	"hash/crc64"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Job states.
const (
	StatusRunning   = "running"
	StatusConverged = "converged"
	StatusBudget    = "budget_exhausted"
	StatusCancelled = "cancelled"
)

// Errors reported by the minimizer.
var (
	ErrBadSample       = errors.New("sample data is required")
	ErrJobLimit        = errors.New("too many jobs are tracked")
	ErrUnknownJob      = errors.New("unknown job")
	ErrNoCandidate     = errors.New("no candidate is waiting for a result")
	ErrBudgetExhausted = errors.New("replay budget exhausted")
	ErrJobCancelled    = errors.New("job is cancelled")
	ErrConverged       = errors.New("no further candidate is available")
	ErrDifferentCrash  = errors.New("candidate crashed in a different cluster")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

var errNoCut = errors.New("input cannot be cut further")

// ClusterID names one crash cluster reported by the target.
type ClusterID string

// Sample is the original crashing input.
type Sample struct {
	ID   string
	Data []byte
}

// Input is one candidate handed to the replayer.
type Input struct {
	Data []byte
}

// Job is one minimisation run.
type Job struct {
	ID      string
	Cluster ClusterID
	Status  string
}

// Progress reports the counters of one job.
type Progress struct {
	Replays  int
	Accepted int
	Rejected int
	BestLen  int
	Status   string
}

// Limits bounds the job table and the replay budget.
type Limits struct {
	MaxReplays int
	MaxJobs    int
}

// CrashMinimizer owns the job table and the trimming state of every job.
type CrashMinimizer struct {
	limits Limits
	mu     sync.RWMutex
	jobs   map[string]*minimizerJob
}

type minimizerJob struct {
	mu sync.Mutex

	id       string
	cluster  ClusterID
	status   string
	best     []byte
	layout   frameLayout
	phase    cutPhase
	pending  *Input
	replays  int
	accepted int
	rejected int
}

type cutPhase uint8

const (
	cutTail cutPhase = iota
	cutHead
)

// NewCrashMinimizer builds a minimizer with the given caps.
func NewCrashMinimizer(limits Limits) *CrashMinimizer {
	return &CrashMinimizer{
		limits: limits,
		jobs:   make(map[string]*minimizerJob),
	}
}

// Start opens a job for one sample and its original crash cluster.
func (m *CrashMinimizer) Start(sample Sample, cluster ClusterID) (Job, error) {
	if strings.TrimSpace(sample.ID) == "" || len(sample.Data) == 0 {
		return Job{}, ErrBadSample
	}

	layout, ok := discoverLayout(sample.Data)
	if !ok {
		return Job{}, ErrBadSample
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.hasCapacityLocked() {
		return Job{}, ErrJobLimit
	}

	id := m.uniqueJobIDLocked(sample.ID)
	data := cloneBytes(sample.Data)
	m.jobs[id] = &minimizerJob{
		id:      id,
		cluster: cluster,
		status:  StatusRunning,
		best:    data,
		layout:  layout,
		phase:   cutTail,
	}
	return Job{ID: id, Cluster: cluster, Status: StatusRunning}, nil
}

// Candidate returns the cut that has to be replayed next.
func (m *CrashMinimizer) Candidate(jobID string) (Input, error) {
	job, ok := m.lookup(jobID)
	if !ok {
		return Input{}, ErrUnknownJob
	}

	job.mu.Lock()
	defer job.mu.Unlock()

	if job.pending != nil {
		return Input{Data: cloneBytes(job.pending.Data)}, nil
	}

	switch job.status {
	case StatusCancelled:
		return Input{}, ErrJobCancelled
	case StatusConverged:
		return Input{}, ErrConverged
	case StatusBudget:
		return Input{}, ErrBudgetExhausted
	case StatusRunning:
	default:
		return Input{}, ErrUnknownJob
	}

	next, err := job.layout.candidate(job.best, job.phase)
	if errors.Is(err, errNoCut) {
		job.status = StatusConverged
		return Input{}, ErrConverged
	}
	if err != nil {
		return Input{}, err
	}

	if m.limits.MaxReplays <= 0 || job.replays >= m.limits.MaxReplays {
		job.status = StatusBudget
		return Input{}, ErrBudgetExhausted
	}

	job.pending = &Input{Data: next}
	return Input{Data: cloneBytes(next)}, nil
}

// Result records the replay outcome of the outstanding candidate.
func (m *CrashMinimizer) Result(jobID string, crashed bool, cluster ClusterID) error {
	job, ok := m.lookup(jobID)
	if !ok {
		return ErrUnknownJob
	}

	job.mu.Lock()
	defer job.mu.Unlock()

	if job.status == StatusCancelled {
		return ErrJobCancelled
	}
	if job.pending == nil || job.status != StatusRunning {
		return ErrNoCandidate
	}

	candidate := job.pending
	job.pending = nil
	job.replays++

	sameCrash := crashed && cluster == job.cluster
	if sameCrash {
		job.best = cloneBytes(candidate.Data)
		job.accepted++
		job.phase = cutTail
		return nil
	}

	job.rejected++
	job.advanceAfterRejection()
	if crashed {
		return ErrDifferentCrash
	}
	return nil
}

// Best returns the smallest input proven to keep the original crash cluster.
func (m *CrashMinimizer) Best(jobID string) (Input, error) {
	job, ok := m.lookup(jobID)
	if !ok {
		return Input{}, ErrUnknownJob
	}

	job.mu.Lock()
	defer job.mu.Unlock()
	return Input{Data: cloneBytes(job.best)}, nil
}

// Cancel stops one job and keeps its best input readable.
func (m *CrashMinimizer) Cancel(jobID string) error {
	job, ok := m.lookup(jobID)
	if !ok {
		return ErrUnknownJob
	}

	job.mu.Lock()
	defer job.mu.Unlock()

	if job.status == StatusRunning {
		job.status = StatusCancelled
		job.pending = nil
	}
	return nil
}

// Progress reports the counters of one job.
func (m *CrashMinimizer) Progress(jobID string) (Progress, error) {
	job, ok := m.lookup(jobID)
	if !ok {
		return Progress{}, ErrUnknownJob
	}

	job.mu.Lock()
	defer job.mu.Unlock()

	return Progress{
		Replays:  job.replays,
		Accepted: job.accepted,
		Rejected: job.rejected,
		BestLen:  len(job.best),
		Status:   job.status,
	}, nil
}

// Status reports the state of one job.
func (m *CrashMinimizer) Status(jobID string) (string, error) {
	job, ok := m.lookup(jobID)
	if !ok {
		return "", ErrUnknownJob
	}

	job.mu.Lock()
	defer job.mu.Unlock()
	return job.status, nil
}

// Jobs lists the tracked jobs in ascending order.
func (m *CrashMinimizer) Jobs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Snapshot serialises the job table and the best inputs.
func (m *CrashMinimizer) Snapshot() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Lock every job before reading. This gives the snapshot one consistent
	// point without holding any lock while doing the JSON encoding.
	for _, id := range ids {
		m.jobs[id].mu.Lock()
	}
	defer func() {
		for _, id := range ids {
			m.jobs[id].mu.Unlock()
		}
	}()

	doc := snapshotDocument{
		Version: snapshotVersion,
		Jobs:    make([]snapshotJob, 0, len(ids)),
	}
	for _, id := range ids {
		job := m.jobs[id]
		doc.Jobs = append(doc.Jobs, snapshotJob{
			ID:       job.id,
			Cluster:  string(job.cluster),
			Status:   job.status,
			Best:     cloneBytes(job.best),
			Replays:  job.replays,
			Accepted: job.accepted,
			Rejected: job.rejected,
			Phase:    uint8(job.phase),
			Layout:   job.layout.snapshot(),
		})
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return sealSnapshot(payload), nil
}

// Restore replaces the job table with a snapshot, all or nothing.
func (m *CrashMinimizer) Restore(data []byte) error {
	doc, err := openSnapshot(data)
	if err != nil {
		return ErrBadSnapshot
	}
	jobs, err := m.validateSnapshot(doc)
	if err != nil {
		return ErrBadSnapshot
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Quiesce calls that already looked up an old job before replacing the
	// table. New lookups are blocked by the write lock.
	oldJobs := make(map[string]*minimizerJob, len(m.jobs))
	for id, job := range m.jobs {
		oldJobs[id] = job
		job.mu.Lock()
	}
	m.jobs = jobs
	for _, job := range oldJobs {
		job.mu.Unlock()
	}
	return nil
}

func (m *CrashMinimizer) lookup(jobID string) (*minimizerJob, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[jobID]
	return job, ok
}

func (m *CrashMinimizer) hasCapacityLocked() bool {
	if m.limits.MaxJobs <= 0 {
		return false
	}
	return len(m.jobs) < m.limits.MaxJobs
}

func (m *CrashMinimizer) uniqueJobIDLocked(base string) string {
	if _, exists := m.jobs[base]; !exists {
		return base
	}
	for n := 1; ; n++ {
		id := base + "#" + strconv.Itoa(n)
		if _, exists := m.jobs[id]; !exists {
			return id
		}
	}
}

func (j *minimizerJob) advanceAfterRejection() {
	switch j.phase {
	case cutTail:
		j.phase = cutHead
	case cutHead:
		j.phase = cutTail
		j.status = StatusConverged
	}
}

func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

// --- framed input handling -------------------------------------------------

// Samples produced by fuzzers are often framed. The two common wrappers are a
// length field (binary or decimal) and/or a checksum trailer. Only the logical
// payload is minimised; header fields and checksums are rebuilt after every cut.
type frameKind uint8

const (
	frameRaw frameKind = iota
	frameBinary
	frameText
)

type lengthMode uint8

const (
	lengthPayload lengthMode = iota
	lengthPrefixPayload
	lengthPayloadTrailer
	lengthFull
)

type checksumKind uint8

const (
	checksumNone checksumKind = iota
	checksumCRC32
	checksumCRC32C
	checksumAdler32
	checksumFNV32a
	checksumSum32
	checksumCRC64ISO
	checksumCRC64ECMA
	checksumFNV64a
	checksumSum64
	checksumMD5
	checksumSHA1
	checksumSHA256
	checksumSHA512
)

type checksumEncoding uint8

const (
	checksumRawLittle checksumEncoding = iota
	checksumRawBig
	checksumHexLower
	checksumHexUpper
)

type checksumScope uint8

const (
	checksumPayload checksumScope = iota
	checksumPrefixPayload
)

type checksumSpec struct {
	kind     checksumKind
	encoding checksumEncoding
}

type frameLayout struct {
	kind      frameKind
	payloadAt int
	fieldAt   int
	fieldWide int
	little    bool
	mode      lengthMode
	delimiter byte
	checksum  checksumSpec
	scope     checksumScope
}

var (
	crc64ISOTable  = crc64.MakeTable(crc64.ISO)
	crc64ECMATable = crc64.MakeTable(crc64.ECMA)
	crc32CTable    = crc32.MakeTable(crc32.Castagnoli)
)

func (l frameLayout) trailerLen() int {
	return checksumLength(l.checksum)
}

func (l frameLayout) candidate(data []byte, phase cutPhase) ([]byte, error) {
	if !l.validFor(data) {
		return nil, errNoCut
	}
	end := len(data) - l.trailerLen()
	if l.payloadAt < 0 || l.payloadAt > end {
		return nil, errNoCut
	}
	payload := data[l.payloadAt:end]
	// The tail cut keeps the first half. For an even-length input the head cut
	// starts one byte before the midpoint, so the pivot byte is not lost when a
	// fuzzer trigger straddles the split.
	half := len(payload) / 2
	if half <= 0 {
		return nil, errNoCut
	}

	var next []byte
	switch phase {
	case cutTail:
		next = payload[:half]
	case cutHead:
		start := half
		if len(payload)%2 == 0 {
			start--
		}
		next = payload[start:]
	default:
		return nil, errNoCut
	}
	if len(next) >= len(payload) {
		return nil, errNoCut
	}

	rebuilt, err := l.rebuild(data, next)
	if err != nil || len(rebuilt) >= len(data) || !l.validFor(rebuilt) {
		return nil, errNoCut
	}
	return rebuilt, nil
}

func (l frameLayout) rebuild(data, payload []byte) ([]byte, error) {
	if l.payloadAt < 0 || l.payloadAt > len(data) {
		return nil, errNoCut
	}
	trailer := l.trailerLen()
	if l.payloadAt+trailer > len(data) {
		return nil, errNoCut
	}

	header := cloneBytes(data[:l.payloadAt])
	target := l.declaredLength(len(header), len(payload))

	switch l.kind {
	case frameRaw:
		// A checksum-only frame has no header. A plain raw frame has neither a
		// header nor a trailer.
	case frameBinary:
		if err := writeBinaryLength(header, l.fieldAt, l.fieldWide, l.little, target); err != nil {
			return nil, err
		}
	case frameText:
		if err := writeTextLength(header, l.fieldAt, l.fieldWide, target); err != nil {
			return nil, err
		}
	default:
		return nil, errNoCut
	}

	out := append(header, payload...)
	if l.checksum.kind != checksumNone {
		content := out
		if l.scope == checksumPayload {
			content = payload
		}
		digest := checksumBytes(l.checksum, content)
		out = append(out, digest...)
	}
	return out, nil
}

func (l frameLayout) declaredLength(headerLen, payloadLen int) uint64 {
	switch l.mode {
	case lengthPayload:
		return uint64(payloadLen)
	case lengthPrefixPayload:
		return uint64(headerLen + payloadLen)
	case lengthPayloadTrailer:
		return uint64(payloadLen + l.trailerLen())
	case lengthFull:
		return uint64(headerLen + payloadLen + l.trailerLen())
	default:
		return uint64(payloadLen)
	}
}

func (l frameLayout) validFor(data []byte) bool {
	trailer := l.trailerLen()
	if l.payloadAt < 0 || l.payloadAt+trailer > len(data) {
		return false
	}

	payload := data[l.payloadAt : len(data)-trailer]
	switch l.kind {
	case frameRaw:
		if l.fieldAt != 0 || l.fieldWide != 0 || l.mode != lengthPayload || l.delimiter != 0 {
			return false
		}
	case frameBinary:
		if l.fieldAt < 0 || l.fieldWide <= 0 || l.fieldAt+l.fieldWide > l.payloadAt {
			return false
		}
		if !validBinaryWidth(l.fieldWide) {
			return false
		}
		declared, ok := readBinaryLength(data, l.fieldAt, l.fieldWide, l.little)
		if !ok || declared != l.declaredLength(l.payloadAt, len(payload)) {
			return false
		}
	case frameText:
		if l.fieldAt < 0 || l.fieldWide <= 0 || l.fieldAt+l.fieldWide > l.payloadAt {
			return false
		}
		if l.delimiter != 0 {
			if l.fieldAt+l.fieldWide >= l.payloadAt || data[l.fieldAt+l.fieldWide] != l.delimiter {
				return false
			}
		}
		declared, ok := readTextLength(data, l.fieldAt, l.fieldWide)
		if !ok || declared != l.declaredLength(l.payloadAt, len(payload)) {
			return false
		}
	default:
		return false
	}

	if l.checksum.kind == checksumNone {
		return true
	}
	if checksumLength(l.checksum) == 0 {
		return false
	}
	content := payload
	if l.scope == checksumPrefixPayload {
		content = data[:len(data)-trailer]
	}
	return bytes.Equal(data[len(data)-trailer:], checksumBytes(l.checksum, content))
}

func validBinaryWidth(width int) bool {
	return width == 2 || width == 4 || width == 8
}

func readBinaryLength(data []byte, offset, width int, little bool) (uint64, bool) {
	if offset < 0 || width <= 0 || offset+width > len(data) || !validBinaryWidth(width) {
		return 0, false
	}
	field := data[offset : offset+width]
	switch width {
	case 2:
		if little {
			return uint64(binary.LittleEndian.Uint16(field)), true
		}
		return uint64(binary.BigEndian.Uint16(field)), true
	case 4:
		if little {
			return uint64(binary.LittleEndian.Uint32(field)), true
		}
		return uint64(binary.BigEndian.Uint32(field)), true
	case 8:
		if little {
			return binary.LittleEndian.Uint64(field), true
		}
		return binary.BigEndian.Uint64(field), true
	default:
		return 0, false
	}
}

func writeBinaryLength(header []byte, offset, width int, little bool, value uint64) error {
	if offset < 0 || !validBinaryWidth(width) || offset+width > len(header) {
		return errNoCut
	}
	field := header[offset : offset+width]
	switch width {
	case 2:
		if value > 1<<16-1 {
			return errNoCut
		}
		if little {
			binary.LittleEndian.PutUint16(field, uint16(value))
		} else {
			binary.BigEndian.PutUint16(field, uint16(value))
		}
	case 4:
		if value > 1<<32-1 {
			return errNoCut
		}
		if little {
			binary.LittleEndian.PutUint32(field, uint32(value))
		} else {
			binary.BigEndian.PutUint32(field, uint32(value))
		}
	case 8:
		if little {
			binary.LittleEndian.PutUint64(field, value)
		} else {
			binary.BigEndian.PutUint64(field, value)
		}
	default:
		return errNoCut
	}
	return nil
}

func readTextLength(data []byte, offset, width int) (uint64, bool) {
	if offset < 0 || width <= 0 || offset+width > len(data) {
		return 0, false
	}
	for _, b := range data[offset : offset+width] {
		if b < '0' || b > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(string(data[offset:offset+width]), 10, 64)
	return value, err == nil
}

func writeTextLength(header []byte, offset, width int, value uint64) error {
	if offset < 0 || width <= 0 || offset+width > len(header) {
		return errNoCut
	}
	digits := strconv.FormatUint(value, 10)
	if len(digits) > width {
		return errNoCut
	}
	padding := width - len(digits)
	for i := 0; i < padding; i++ {
		header[offset+i] = '0'
	}
	copy(header[offset+padding:offset+width], digits)
	return nil
}

func checksumLength(spec checksumSpec) int {
	raw := rawChecksumLength(spec.kind)
	if raw == 0 {
		return 0
	}
	switch spec.encoding {
	case checksumHexLower, checksumHexUpper:
		return raw * 2
	default:
		return raw
	}
}

func rawChecksumLength(kind checksumKind) int {
	switch kind {
	case checksumCRC32, checksumCRC32C, checksumAdler32, checksumFNV32a, checksumSum32:
		return 4
	case checksumCRC64ISO, checksumCRC64ECMA, checksumFNV64a, checksumSum64:
		return 8
	case checksumMD5:
		return 16
	case checksumSHA1:
		return 20
	case checksumSHA256:
		return 32
	case checksumSHA512:
		return 64
	default:
		return 0
	}
}

func checksumBytes(spec checksumSpec, data []byte) []byte {
	raw := rawChecksum(spec.kind, data)
	if raw == nil {
		return nil
	}
	switch spec.encoding {
	case checksumRawLittle:
		out := cloneBytes(raw)
		for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
			out[left], out[right] = out[right], out[left]
		}
		return out
	case checksumHexLower:
		return []byte(hex.EncodeToString(raw))
	case checksumHexUpper:
		return []byte(strings.ToUpper(hex.EncodeToString(raw)))
	default:
		return raw
	}
}

func rawChecksum(kind checksumKind, data []byte) []byte {
	switch kind {
	case checksumCRC32:
		return uint32Bytes(crc32.ChecksumIEEE(data))
	case checksumCRC32C:
		return uint32Bytes(crc32.Checksum(data, crc32CTable))
	case checksumAdler32:
		return uint32Bytes(adler32.Checksum(data))
	case checksumFNV32a:
		h := fnv.New32a()
		_, _ = h.Write(data)
		return uint32Bytes(h.Sum32())
	case checksumSum32:
		var sum uint32
		for _, b := range data {
			sum += uint32(b)
		}
		return uint32Bytes(sum)
	case checksumCRC64ISO:
		return uint64Bytes(crc64.Checksum(data, crc64ISOTable))
	case checksumCRC64ECMA:
		return uint64Bytes(crc64.Checksum(data, crc64ECMATable))
	case checksumFNV64a:
		h := fnv.New64a()
		_, _ = h.Write(data)
		return uint64Bytes(h.Sum64())
	case checksumSum64:
		var sum uint64
		for _, b := range data {
			sum += uint64(b)
		}
		return uint64Bytes(sum)
	case checksumMD5:
		sum := md5.Sum(data)
		return sum[:]
	case checksumSHA1:
		sum := sha1.Sum(data)
		return sum[:]
	case checksumSHA256:
		sum := sha256.Sum256(data)
		return sum[:]
	case checksumSHA512:
		sum := sha512.Sum512(data)
		return sum[:]
	default:
		return nil
	}
}

func uint32Bytes(value uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, value)
	return out
}

func uint64Bytes(value uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, value)
	return out
}

func allChecksumKinds() []checksumKind {
	return []checksumKind{
		checksumCRC32,
		checksumCRC32C,
		checksumAdler32,
		checksumFNV32a,
		checksumSum32,
		checksumCRC64ISO,
		checksumCRC64ECMA,
		checksumFNV64a,
		checksumSum64,
		checksumMD5,
		checksumSHA1,
		checksumSHA256,
		checksumSHA512,
	}
}

func allChecksumEncodings() []checksumEncoding {
	return []checksumEncoding{
		checksumRawLittle,
		checksumRawBig,
		checksumHexLower,
		checksumHexUpper,
	}
}

func allChecksumSpecs() []checksumSpec {
	specs := make([]checksumSpec, 0, len(allChecksumKinds())*len(allChecksumEncodings()))
	for _, kind := range allChecksumKinds() {
		for _, encoding := range allChecksumEncodings() {
			specs = append(specs, checksumSpec{kind: kind, encoding: encoding})
		}
	}
	return specs
}

func checksumMatches(data []byte, payloadAt int, spec checksumSpec, scope checksumScope) bool {
	trailer := checksumLength(spec)
	if trailer <= 0 || payloadAt < 0 || payloadAt+trailer > len(data) {
		return false
	}
	payload := data[payloadAt : len(data)-trailer]
	content := payload
	if scope == checksumPrefixPayload {
		content = data[:len(data)-trailer]
	}
	return bytes.Equal(data[len(data)-trailer:], checksumBytes(spec, content))
}

type layoutCandidate struct {
	layout frameLayout
	score  int
}

func discoverLayout(data []byte) (frameLayout, bool) {
	candidates := make([]layoutCandidate, 0, 16)
	candidates = append(candidates, detectChecksumOnly(data)...)
	candidates = append(candidates, detectBinaryLayouts(data)...)
	textCandidates, textHint := detectTextLayouts(data)
	candidates = append(candidates, textCandidates...)

	best := layoutCandidate{}
	found := false
	for _, candidate := range candidates {
		if !candidate.layout.validFor(data) {
			continue
		}
		if !found || candidate.score > best.score {
			best = candidate
			found = true
		}
	}

	binaryHint := binaryLengthHint(data)
	if !found {
		if textHint || binaryHint {
			return frameLayout{}, false
		}
		return frameLayout{}, true
	}

	// A very small leading integer or an explicit "12:" style prefix is a
	// strong framing signal. If no actual frame validates, treat the input as a
	// malformed/provider-corrupt framed sample rather than accidentally cutting
	// through its header.
	if (textHint || binaryHint) && best.layout.kind == frameRaw {
		return frameLayout{}, false
	}
	return best.layout, true
}

func detectChecksumOnly(data []byte) []layoutCandidate {
	out := make([]layoutCandidate, 0, 8)
	for _, spec := range allChecksumSpecs() {
		trailer := checksumLength(spec)
		if trailer <= 0 || trailer >= len(data) {
			continue
		}
		// A zero trailer is common in plain binary inputs (and several weak
		// checksums are zero for all-zero data). Requiring a non-zero trailer
		// avoids treating such an input as a checksum-only frame by accident.
		if allZero(data[len(data)-trailer:]) {
			continue
		}
		if checksumMatches(data, 0, spec, checksumPayload) {
			out = append(out, layoutCandidate{
				layout: frameLayout{
					kind:      frameRaw,
					payloadAt: 0,
					checksum:  spec,
					scope:     checksumPayload,
				},
				score: 110,
			})
		}
	}
	return out
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func detectBinaryLayouts(data []byte) []layoutCandidate {
	out := make([]layoutCandidate, 0, 32)
	const maxFieldOffset = 12
	specs := allChecksumSpecs()

	for offset := 0; offset <= maxFieldOffset; offset++ {
		for _, width := range []int{2, 4, 8} {
			payloadAt := offset + width
			if payloadAt > len(data) {
				continue
			}
			for _, little := range []bool{true, false} {
				value, ok := readBinaryLength(data, offset, width, little)
				if !ok {
					continue
				}
				for mode := lengthPayload; mode <= lengthFull; mode++ {
					noTrailerLen := len(data) - payloadAt
					if noTrailerLen >= 0 && value == declaredForMode(mode, payloadAt, noTrailerLen, 0) {
						out = append(out, layoutCandidate{
							layout: frameLayout{
								kind:      frameBinary,
								payloadAt: payloadAt,
								fieldAt:   offset,
								fieldWide: width,
								little:    little,
								mode:      mode,
							},
							score: 100,
						})
					}

					for _, spec := range specs {
						trailer := checksumLength(spec)
						payloadLen := len(data) - payloadAt - trailer
						if trailer <= 0 || payloadLen < 0 {
							continue
						}
						if value != declaredForMode(mode, payloadAt, payloadLen, trailer) {
							continue
						}
						for _, scope := range []checksumScope{checksumPayload, checksumPrefixPayload} {
							if !checksumMatches(data, payloadAt, spec, scope) {
								continue
							}
							out = append(out, layoutCandidate{
								layout: frameLayout{
									kind:      frameBinary,
									payloadAt: payloadAt,
									fieldAt:   offset,
									fieldWide: width,
									little:    little,
									mode:      mode,
									checksum:  spec,
									scope:     scope,
								},
								score: 130,
							})
						}
					}
				}
			}
		}
	}
	return out
}

func detectTextLayouts(data []byte) ([]layoutCandidate, bool) {
	run := 0
	for run < len(data) && data[run] >= '0' && data[run] <= '9' {
		run++
	}
	if run == 0 || run > 16 {
		return nil, false
	}
	value, err := strconv.ParseUint(string(data[:run]), 10, 64)
	if err != nil {
		return nil, true
	}

	delimiter := byte(0)
	payloadAt := run
	if run < len(data) && isLengthDelimiter(data[run]) {
		delimiter = data[run]
		payloadAt = run + 1
	}

	hint := delimiter != 0
	if !hint && run >= 4 && value <= uint64(len(data))*2 {
		hint = true
	}

	out := make([]layoutCandidate, 0, 8)
	specs := append([]checksumSpec{{kind: checksumNone}}, allChecksumSpecs()...)
	for _, spec := range specs {
		trailer := checksumLength(spec)
		if payloadAt+trailer > len(data) {
			continue
		}
		payloadLen := len(data) - payloadAt - trailer
		for mode := lengthPayload; mode <= lengthFull; mode++ {
			if value != declaredForMode(mode, payloadAt, payloadLen, trailer) {
				continue
			}
			if spec.kind != checksumNone && !checksumMatches(data, payloadAt, spec, checksumPayload) &&
				!checksumMatches(data, payloadAt, spec, checksumPrefixPayload) {
				continue
			}

			scope := checksumPayload
			if spec.kind != checksumNone && !checksumMatches(data, payloadAt, spec, checksumPayload) {
				scope = checksumPrefixPayload
			}
			score := 100
			if delimiter != 0 {
				score = 105
			}
			if spec.kind != checksumNone {
				score = 135
			}
			out = append(out, layoutCandidate{
				layout: frameLayout{
					kind:      frameText,
					payloadAt: payloadAt,
					fieldAt:   0,
					fieldWide: run,
					mode:      mode,
					delimiter: delimiter,
					checksum:  spec,
					scope:     scope,
				},
				score: score,
			})
		}
	}

	if len(out) == 0 && !hint {
		return nil, false
	}
	return out, hint
}

func isLengthDelimiter(b byte) bool {
	switch b {
	case ':', '|', ';', ',', '\n', '\r', '\t', ' ':
		return true
	default:
		return false
	}
}

func declaredForMode(mode lengthMode, payloadAt, payloadLen, trailerLen int) uint64 {
	switch mode {
	case lengthPayload:
		return uint64(payloadLen)
	case lengthPrefixPayload:
		return uint64(payloadAt + payloadLen)
	case lengthPayloadTrailer:
		return uint64(payloadLen + trailerLen)
	case lengthFull:
		return uint64(payloadAt + payloadLen + trailerLen)
	default:
		return uint64(payloadLen)
	}
}

func binaryLengthHint(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	// A short integer with three zero high bytes is the usual little-endian
	// representation of a length prefix. The same test also catches an
	// all-zero-looking big-endian prefix.
	for _, width := range []int{2, 4, 8} {
		if width > len(data) {
			continue
		}
		field := data[:width]
		zeros := 0
		for _, b := range field {
			if b == 0 {
				zeros++
			}
		}
		if zeros < width-1 {
			continue
		}
		for _, little := range []bool{true, false} {
			value, ok := readBinaryLength(data, 0, width, little)
			if ok && value <= uint64(len(data)) {
				return true
			}
		}
	}
	return false
}

// --- snapshot --------------------------------------------------------------

const (
	snapshotMagic   = "CRSHMIN1"
	snapshotVersion = 1
	snapshotDigest  = 32
)

type snapshotDocument struct {
	Version int           `json:"version"`
	Jobs    []snapshotJob `json:"jobs"`
}

type snapshotJob struct {
	ID       string        `json:"id"`
	Cluster  string        `json:"cluster"`
	Status   string        `json:"status"`
	Best     []byte        `json:"best"`
	Replays  int           `json:"replays"`
	Accepted int           `json:"accepted"`
	Rejected int           `json:"rejected"`
	Phase    uint8         `json:"phase"`
	Layout   snapshotFrame `json:"layout"`
}

type snapshotFrame struct {
	Kind      uint8 `json:"kind"`
	PayloadAt int   `json:"payload_at"`
	FieldAt   int   `json:"field_at"`
	FieldWide int   `json:"field_wide"`
	Little    bool  `json:"little"`
	Mode      uint8 `json:"mode"`
	Delimiter uint8 `json:"delimiter"`
	SumKind   uint8 `json:"sum_kind"`
	SumEnc    uint8 `json:"sum_encoding"`
	Scope     uint8 `json:"scope"`
}

func (l frameLayout) snapshot() snapshotFrame {
	return snapshotFrame{
		Kind:      uint8(l.kind),
		PayloadAt: l.payloadAt,
		FieldAt:   l.fieldAt,
		FieldWide: l.fieldWide,
		Little:    l.little,
		Mode:      uint8(l.mode),
		Delimiter: l.delimiter,
		SumKind:   uint8(l.checksum.kind),
		SumEnc:    uint8(l.checksum.encoding),
		Scope:     uint8(l.scope),
	}
}

func (s snapshotFrame) layout() (frameLayout, bool) {
	l := frameLayout{
		kind:      frameKind(s.Kind),
		payloadAt: s.PayloadAt,
		fieldAt:   s.FieldAt,
		fieldWide: s.FieldWide,
		little:    s.Little,
		mode:      lengthMode(s.Mode),
		delimiter: s.Delimiter,
		checksum: checksumSpec{
			kind:     checksumKind(s.SumKind),
			encoding: checksumEncoding(s.SumEnc),
		},
		scope: checksumScope(s.Scope),
	}

	if l.kind != frameRaw && l.kind != frameBinary && l.kind != frameText {
		return frameLayout{}, false
	}
	if l.mode < lengthPayload || l.mode > lengthFull {
		return frameLayout{}, false
	}
	if l.checksum.kind != checksumNone && rawChecksumLength(l.checksum.kind) == 0 {
		return frameLayout{}, false
	}
	switch l.checksum.encoding {
	case checksumRawLittle, checksumRawBig, checksumHexLower, checksumHexUpper:
	default:
		return frameLayout{}, false
	}
	if l.scope != checksumPayload && l.scope != checksumPrefixPayload {
		return frameLayout{}, false
	}
	return l, true
}

func sealSnapshot(payload []byte) []byte {
	out := make([]byte, 0, len(snapshotMagic)+4+len(payload)+snapshotDigest)
	out = append(out, []byte(snapshotMagic)...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	out = append(out, sum[:]...)
	return out
}

func openSnapshot(data []byte) (snapshotDocument, error) {
	headerLen := len(snapshotMagic) + 4
	if len(data) < headerLen+snapshotDigest || string(data[:len(snapshotMagic)]) != snapshotMagic {
		return snapshotDocument{}, ErrBadSnapshot
	}
	payloadLen := int(binary.BigEndian.Uint32(data[len(snapshotMagic):headerLen]))
	if payloadLen < 0 || len(data) != headerLen+payloadLen+snapshotDigest {
		return snapshotDocument{}, ErrBadSnapshot
	}
	payload := data[headerLen : headerLen+payloadLen]
	want := sha256.Sum256(payload)
	if !bytes.Equal(data[headerLen+payloadLen:], want[:]) {
		return snapshotDocument{}, ErrBadSnapshot
	}

	var doc snapshotDocument
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return snapshotDocument{}, ErrBadSnapshot
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return snapshotDocument{}, ErrBadSnapshot
	}
	if doc.Version != snapshotVersion || doc.Jobs == nil {
		return snapshotDocument{}, ErrBadSnapshot
	}
	return doc, nil
}

func (m *CrashMinimizer) validateSnapshot(doc snapshotDocument) (map[string]*minimizerJob, error) {
	if m.limits.MaxJobs <= 0 && len(doc.Jobs) > 0 {
		return nil, ErrBadSnapshot
	}
	if m.limits.MaxJobs > 0 && len(doc.Jobs) > m.limits.MaxJobs {
		return nil, ErrBadSnapshot
	}

	jobs := make(map[string]*minimizerJob, len(doc.Jobs))
	for _, snap := range doc.Jobs {
		if snap.ID == "" || len(snap.Best) == 0 {
			return nil, ErrBadSnapshot
		}
		if _, exists := jobs[snap.ID]; exists {
			return nil, ErrBadSnapshot
		}
		if snap.Status != StatusRunning && snap.Status != StatusConverged &&
			snap.Status != StatusBudget && snap.Status != StatusCancelled {
			return nil, ErrBadSnapshot
		}
		if snap.Replays < 0 || snap.Accepted < 0 || snap.Rejected < 0 {
			return nil, ErrBadSnapshot
		}
		if snap.Replays != snap.Accepted+snap.Rejected {
			return nil, ErrBadSnapshot
		}
		if m.limits.MaxReplays > 0 && snap.Replays > m.limits.MaxReplays {
			return nil, ErrBadSnapshot
		}
		if m.limits.MaxReplays <= 0 && snap.Replays > 0 {
			return nil, ErrBadSnapshot
		}
		if snap.Phase != uint8(cutTail) && snap.Phase != uint8(cutHead) {
			return nil, ErrBadSnapshot
		}
		layout, ok := snap.Layout.layout()
		if !ok || !layout.validFor(snap.Best) {
			return nil, ErrBadSnapshot
		}
		jobs[snap.ID] = &minimizerJob{
			id:       snap.ID,
			cluster:  ClusterID(snap.Cluster),
			status:   snap.Status,
			best:     cloneBytes(snap.Best),
			layout:   layout,
			phase:    cutPhase(snap.Phase),
			replays:  snap.Replays,
			accepted: snap.Accepted,
			rejected: snap.Rejected,
		}
	}
	return jobs, nil
}
