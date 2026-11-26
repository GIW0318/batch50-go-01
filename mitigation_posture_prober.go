// Package task inspects delivered executables for compile time mitigations and
// compares a batch against a frozen baseline.
package task

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

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

const (
	defaultMaxFileBytes = 64 << 20
	defaultMaxArtifacts = 4096

	snapshotVersion = 1

	ptGNUStack     = elf.PT_GNU_STACK
	ptGNURelRO     = elf.PT_GNU_RELRO
	ptGNUProperty  = elf.PT_NOTE
	shtGNUProperty = elf.SHT_NOTE

	// Values from the ELF gABI and GNU property ABI.
	dtBindNow  = elf.DT_BIND_NOW
	dtFlags    = elf.DT_FLAGS
	dtFlagsOne = elf.DT_FLAGS_1
	dfBindNow  = uint64(elf.DF_BIND_NOW)
	dfOneNow   = uint64(elf.DF_1_NOW)

	ntGNUPropertyType0 = uint32(5)
	gnuPropertyX86AND  = uint32(0xc0000002)
	gnuPropertyX86OR   = uint32(0xc0000003)
	gnuFeatureIBT      = uint32(1)
	gnuFeatureSHSTK    = uint32(2)

	pfExec = uint32(elf.PF_X)
)

var explainFieldOrder = []string{
	FieldStackCanary,
	FieldCFI,
	FieldNXStack,
	FieldPIE,
	FieldRelro,
}

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

	mu        sync.RWMutex
	postures  map[string]Posture
	evidence  map[string][]Evidence
	baselines map[string]Posture
}

type inspection struct {
	posture  Posture
	evidence []Evidence
}

type dynamicEntry struct {
	tag   uint64
	value uint64
}

type dynamicInfo struct {
	bindNow  bool
	hasEntry bool
}

type snapshotEnvelope struct {
	Version int             `json:"version"`
	Payload json.RawMessage `json:"payload"`
	Digest  string          `json:"digest"`
}

type snapshotState struct {
	Postures []Posture             `json:"postures"`
	Baseline []Posture             `json:"baseline"`
	Evidence map[string][]Evidence `json:"evidence,omitempty"`
}

// NewMitigationPostureProber builds a prober with the given caps.
func NewMitigationPostureProber(cfg Config) *MitigationPostureProber {
	return &MitigationPostureProber{
		cfg:       cfg,
		postures:  make(map[string]Posture),
		evidence:  make(map[string][]Evidence),
		baselines: make(map[string]Posture),
	}
}

// Inspect reads one artifact and records its posture under name.
func (p *MitigationPostureProber) Inspect(name string, data []byte) (Posture, error) {
	ins, err := p.inspectPayload(name, data)
	if err != nil {
		return Posture{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.postures[name]; !exists && len(p.postures) >= p.maxArtifacts() {
		return Posture{}, ErrArtifactLimit
	}
	p.postures[name] = ins.posture
	p.evidence[name] = cloneEvidence(ins.evidence)
	return ins.posture, nil
}

// InspectAll reads a batch of artifacts and keeps the input order.
func (p *MitigationPostureProber) InspectAll(items []Artifact) ([]Posture, error) {
	if len(items) > p.maxArtifacts() {
		return nil, ErrArtifactLimit
	}

	results := make([]inspection, len(items))
	errs := make([]error, len(items))
	if len(items) != 0 {
		workers := p.maxWorkers()
		if workers > len(items) {
			workers = len(items)
		}
		jobs := make(chan int)
		var wg sync.WaitGroup
		wg.Add(workers)
		for worker := 0; worker < workers; worker++ {
			go func() {
				defer wg.Done()
				for index := range jobs {
					results[index], errs[index] = p.inspectPayload(items[index].Name, items[index].Data)
				}
			}()
		}
		for index := range items {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	newNames := make(map[string]struct{})
	for _, result := range results {
		if _, exists := p.postures[result.posture.ID]; !exists {
			newNames[result.posture.ID] = struct{}{}
		}
	}
	if len(p.postures)+len(newNames) > p.maxArtifacts() {
		return nil, ErrArtifactLimit
	}
	out := make([]Posture, len(results))
	for index, result := range results {
		out[index] = result.posture
		p.postures[result.posture.ID] = result.posture
		p.evidence[result.posture.ID] = cloneEvidence(result.evidence)
	}
	return out, nil
}

// Compare lists the fields that moved between two postures.
func (p *MitigationPostureProber) Compare(base, cur Posture) ([]Delta, error) {
	fields := []string{FieldCFI, FieldNXStack, FieldPIE, FieldRelro, FieldStackCanary}
	deltas := make([]Delta, 0, len(fields))
	for _, field := range fields {
		before := postureField(base, field)
		after := postureField(cur, field)
		if before == after {
			continue
		}
		deltas = append(deltas, Delta{
			Field:    field,
			Before:   before,
			After:    after,
			Severity: compareSeverity(field, before, after),
		})
	}
	return deltas, nil
}

// Baseline replaces the whole baseline in one step.
func (p *MitigationPostureProber) Baseline(items []Posture) error {
	replacement := make(map[string]Posture, len(items))
	for _, item := range items {
		normalized, ok := normalizeBaselineEntry(item)
		if !ok {
			return ErrBadBaseline
		}
		replacement[normalized.Name] = normalized
	}

	p.mu.Lock()
	p.baselines = replacement
	p.mu.Unlock()
	return nil
}

// BaselineOf reads one frozen baseline entry.
func (p *MitigationPostureProber) BaselineOf(name string) (Posture, bool) {
	p.mu.RLock()
	posture, ok := p.baselines[name]
	p.mu.RUnlock()
	return posture, ok
}

// BaselineSize counts the frozen baseline entries.
func (p *MitigationPostureProber) BaselineSize() int {
	p.mu.RLock()
	size := len(p.baselines)
	p.mu.RUnlock()
	return size
}

// Explain returns one evidence entry per conclusion, ordered by field.
func (p *MitigationPostureProber) Explain(id string) ([]Evidence, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	posture, ok := p.postures[id]
	if !ok {
		return nil, ErrUnknownArtifact
	}

	byField := make(map[string]Evidence, len(p.evidence[id]))
	for _, item := range p.evidence[id] {
		byField[item.Field] = item
	}
	out := make([]Evidence, 0, len(explainFieldOrder))
	for _, field := range explainFieldOrder {
		if item, exists := byField[field]; exists {
			out = append(out, item)
			continue
		}
		out = append(out, fallbackEvidence(posture, field))
	}
	return out, nil
}

// Posture reads one recorded inventory entry.
func (p *MitigationPostureProber) Posture(id string) (Posture, bool) {
	p.mu.RLock()
	posture, ok := p.postures[id]
	p.mu.RUnlock()
	return posture, ok
}

// Artifacts lists the recorded artifact ids in name order.
func (p *MitigationPostureProber) Artifacts() []string {
	p.mu.RLock()
	names := make([]string, 0, len(p.postures))
	for name := range p.postures {
		names = append(names, name)
	}
	p.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Snapshot serialises the recorded postures and the baseline.
func (p *MitigationPostureProber) Snapshot() ([]byte, error) {
	p.mu.RLock()
	state := snapshotState{
		Postures: make([]Posture, 0, len(p.postures)),
		Baseline: make([]Posture, 0, len(p.baselines)),
		Evidence: make(map[string][]Evidence, len(p.evidence)),
	}
	for name, posture := range p.postures {
		state.Postures = append(state.Postures, posture)
		state.Evidence[name] = cloneEvidence(p.evidence[name])
	}
	for _, posture := range p.baselines {
		state.Baseline = append(state.Baseline, posture)
	}
	p.mu.RUnlock()

	sort.Slice(state.Postures, func(i, j int) bool {
		return state.Postures[i].Name < state.Postures[j].Name
	})
	sort.Slice(state.Baseline, func(i, j int) bool {
		return state.Baseline[i].Name < state.Baseline[j].Name
	})
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	envelope := snapshotEnvelope{
		Version: snapshotVersion,
		Payload: payload,
		Digest:  hex.EncodeToString(sum[:]),
	}
	return json.Marshal(envelope)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (p *MitigationPostureProber) Restore(data []byte) error {
	if len(data) == 0 {
		return ErrBadSnapshot
	}

	var envelope snapshotEnvelope
	if err := unmarshalStrict(data, &envelope); err != nil {
		return ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || len(envelope.Payload) == 0 {
		return ErrBadSnapshot
	}
	sum := sha256.Sum256(envelope.Payload)
	if hex.EncodeToString(sum[:]) != envelope.Digest {
		return ErrBadSnapshot
	}

	var state snapshotState
	if err := unmarshalStrict(envelope.Payload, &state); err != nil {
		return ErrBadSnapshot
	}

	postures := make(map[string]Posture, len(state.Postures))
	for _, posture := range state.Postures {
		if posture.ID != posture.Name {
			return ErrBadSnapshot
		}
		if _, duplicate := postures[posture.Name]; duplicate {
			return ErrBadSnapshot
		}
		postures[posture.Name] = posture
	}
	baselines := make(map[string]Posture, len(state.Baseline))
	for _, posture := range state.Baseline {
		normalized, ok := normalizeBaselineEntry(posture)
		if !ok {
			return ErrBadSnapshot
		}
		if _, duplicate := baselines[normalized.Name]; duplicate {
			return ErrBadSnapshot
		}
		baselines[normalized.Name] = normalized
	}
	evidence := make(map[string][]Evidence, len(state.Evidence))
	for id, items := range state.Evidence {
		if _, exists := postures[id]; !exists {
			return ErrBadSnapshot
		}
		evidence[id] = cloneEvidence(items)
	}

	p.mu.Lock()
	p.postures = postures
	p.evidence = evidence
	p.baselines = baselines
	p.mu.Unlock()
	return nil
}

func (p *MitigationPostureProber) inspectPayload(name string, data []byte) (inspection, error) {
	if len(data) == 0 {
		return inspection{}, ErrEmptyInput
	}
	if limit := p.maxFileBytes(); limit > 0 && len(data) > limit {
		return inspection{}, ErrFileTooLarge
	}
	if len(data) < 4 || !bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return inspection{}, ErrUnknownFormat
	}
	if len(data) < 5 {
		return inspection{}, ErrMalformed
	}

	var class string
	switch data[4] {
	case byte(elf.ELFCLASS32):
		class = ClassELF32
	case byte(elf.ELFCLASS64):
		class = ClassELF64
	default:
		return inspection{}, ErrMalformed
	}

	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return inspection{}, ErrMalformed
	}
	defer file.Close()

	posture := Posture{
		ID:     name,
		Name:   name,
		Format: FormatELF,
		Class:  class,
	}
	evidence := make([]Evidence, 0, len(explainFieldOrder))

	posture.PIE, evidence = inspectPIE(file)
	nxState, nxEvidence := inspectNXStack(file)
	posture.NXStack = nxState
	evidence = append(evidence, nxEvidence)
	relroState, relroEvidence := inspectRELRO(file, data)
	posture.Relro = relroState
	evidence = append(evidence, relroEvidence)
	canary, canaryEvidence := inspectCanary(file)
	posture.StackCanary = canary
	evidence = append(evidence, canaryEvidence)
	cfi, cfiEvidence := inspectCFI(file, data)
	posture.CFI = cfi
	evidence = append(evidence, cfiEvidence)

	sum := sha256.Sum256(data)
	posture.Digest = hex.EncodeToString(sum[:])
	evidence = orderEvidence(evidence)
	return inspection{posture: posture, evidence: evidence}, nil
}

func inspectPIE(file *elf.File) (string, []Evidence) {
	var state, detail string
	switch file.Type {
	case elf.ET_DYN:
		state = StateEnabled
		detail = "ELF 文件头 e_type=ET_DYN，按地址无关共享对象/可执行文件处理"
	case elf.ET_EXEC:
		state = StateDisabled
		detail = "ELF 文件头 e_type=ET_EXEC，地址无关未启用"
	default:
		state = StateUnknown
		detail = fmt.Sprintf("ELF 文件头 e_type=%s，无法判定地址无关性", file.Type)
	}
	return state, []Evidence{{Field: FieldPIE, Source: "文件头", Detail: detail}}
}

func inspectNXStack(file *elf.File) (string, Evidence) {
	seen := false
	executable := false
	for _, program := range file.Progs {
		if program.Type != ptGNUStack {
			continue
		}
		seen = true
		if uint32(program.Flags)&pfExec != 0 {
			executable = true
		}
	}
	if !seen {
		return StateUnknown, Evidence{Field: FieldNXStack, Source: "程序头", Detail: "未找到 PT_GNU_STACK 栈段程序头，无法判定"}
	}
	if executable {
		return StateDisabled, Evidence{Field: FieldNXStack, Source: "程序头", Detail: "PT_GNU_STACK 栈段包含 PF_X 执行位"}
	}
	return StateEnabled, Evidence{Field: FieldNXStack, Source: "程序头", Detail: "PT_GNU_STACK 栈段不含 PF_X 执行位"}
}

func inspectRELRO(file *elf.File, data []byte) (string, Evidence) {
	hasRELRO := false
	for _, program := range file.Progs {
		if program.Type == ptGNURelRO {
			hasRELRO = true
			break
		}
	}
	dyn, err := readDynamic(file, data)
	if err != nil {
		dyn = dynamicInfo{}
	}
	bindNow := dyn.bindNow

	var state string
	switch {
	case hasRELRO && bindNow:
		state = StateFull
	case hasRELRO || bindNow:
		state = StatePartial
	default:
		state = StateNone
	}

	relroDetail := "未找到 PT_GNU_RELRO"
	if hasRELRO {
		relroDetail = "找到 PT_GNU_RELRO"
	}
	bindDetail := "动态段未声明立即绑定"
	if bindNow {
		bindDetail = "动态段声明立即绑定"
	}
	return state, Evidence{
		Field:  FieldRelro,
		Source: "程序头",
		Detail: fmt.Sprintf("%s；%s；结论为 %s", relroDetail, bindDetail, state),
	}
}

func inspectCanary(file *elf.File) (string, Evidence) {
	hasSymbolTable := false
	parsedTable := false
	found := false
	var foundName string
	for _, section := range file.Sections {
		if section.Type != elf.SHT_SYMTAB && section.Type != elf.SHT_DYNSYM {
			continue
		}
		hasSymbolTable = true
		names, err := readSymbolNames(file, section)
		if err != nil {
			continue
		}
		parsedTable = true
		for _, name := range names {
			if isCanarySymbol(name) {
				found = true
				foundName = name
				break
			}
		}
		if found {
			break
		}
	}
	if found {
		return StatePresent, Evidence{
			Field:  FieldStackCanary,
			Source: "符号表",
			Detail: fmt.Sprintf("符号表包含栈保护检查符号 %s", foundName),
		}
	}
	if hasSymbolTable && parsedTable {
		return StateAbsent, Evidence{
			Field:  FieldStackCanary,
			Source: "符号表",
			Detail: "符号表存在，但未找到 __stack_chk_fail 或其带后缀的检查函数符号",
		}
	}

	if sectionName, marker, ok := canaryFromBuildAttributes(file); ok {
		return StatePresent, Evidence{
			Field:  FieldStackCanary,
			Source: "属性笔记",
			Detail: fmt.Sprintf("符号表缺失或不可解析；构建属性节 %s 声明 %s", sectionName, marker),
		}
	}
	if hasSymbolTable {
		return StateUnknown, Evidence{
			Field:  FieldStackCanary,
			Source: "符号表",
			Detail: "符号表存在但无法解析，构建属性与属性笔记不足以判定栈保护",
		}
	}
	return StateUnknown, Evidence{
		Field:  FieldStackCanary,
		Source: "符号表",
		Detail: "符号表缺失，构建属性与属性笔记不足以判定栈保护",
	}
}

func inspectCFI(file *elf.File, data []byte) (string, Evidence) {
	sawGNUProperty := false
	sawX86Feature := false
	inconclusive := false
	var featureBits uint32

	handleNotes := func(noteData []byte) {
		notes, ok := parseNotes(noteData, file.ByteOrder)
		if !ok {
			inconclusive = true
		}
		for _, note := range notes {
			if strings.TrimRight(note.name, "\x00") != "GNU" || note.typ != ntGNUPropertyType0 {
				continue
			}
			sawGNUProperty = true
			properties, ok := parseGNUProperties(note.desc, file.ByteOrder, file.Class == elf.ELFCLASS64)
			if !ok {
				inconclusive = true
			}
			for _, property := range properties {
				if property.typ != gnuPropertyX86AND && property.typ != gnuPropertyX86OR {
					continue
				}
				if len(property.data) < 4 {
					continue
				}
				sawX86Feature = true
				featureBits |= file.ByteOrder.Uint32(property.data[:4])
			}
		}
	}

	for _, section := range file.Sections {
		if section.Type != shtGNUProperty {
			continue
		}
		sectionData, err := section.Data()
		if err != nil {
			inconclusive = true
			continue
		}
		handleNotes(sectionData)
	}
	for _, program := range file.Progs {
		if program.Type != ptGNUProperty {
			continue
		}
		start, size, ok := fileRange(program.Off, program.Filesz, len(data))
		if !ok {
			inconclusive = true
			continue
		}
		handleNotes(data[start : start+size])
	}

	if sawX86Feature {
		if featureBits&(gnuFeatureIBT|gnuFeatureSHSTK) != 0 {
			return StatePresent, Evidence{
				Field:  FieldCFI,
				Source: "属性笔记",
				Detail: fmt.Sprintf("GNU property note 的 x86 特性位已置位（0x%x）", featureBits),
			}
		}
		return StateAbsent, Evidence{
			Field:  FieldCFI,
			Source: "属性笔记",
			Detail: "GNU property note 存在，但 x86 IBT/SHSTK 特性位未置位",
		}
	}
	detail := "未找到 GNU property note，无法判定控制流检查"
	if sawGNUProperty || inconclusive {
		detail = "GNU property note 存在但不可完整解析，无法判定 x86 特性位"
	}
	return StateUnknown, Evidence{
		Field:  FieldCFI,
		Source: "属性笔记",
		Detail: detail,
	}
}

func readSymbolNames(file *elf.File, section *elf.Section) ([]string, error) {
	entrySize := 24
	if file.Class == elf.ELFCLASS32 {
		entrySize = 16
	}
	if section.Entsize != 0 && section.Entsize != uint64(entrySize) {
		return nil, ErrMalformed
	}
	if section.Link >= uint32(len(file.Sections)) {
		return nil, ErrMalformed
	}
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	if len(data)%entrySize != 0 {
		return nil, ErrMalformed
	}
	stringTable, err := file.Sections[section.Link].Data()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(data)/entrySize)
	for offset := 0; offset+entrySize <= len(data); offset += entrySize {
		nameOffset := int(file.ByteOrder.Uint32(data[offset : offset+4]))
		if nameOffset == 0 {
			names = append(names, "")
			continue
		}
		if nameOffset < 0 || nameOffset >= len(stringTable) {
			return nil, ErrMalformed
		}
		end := bytes.IndexByte(stringTable[nameOffset:], 0)
		if end < 0 {
			return nil, ErrMalformed
		}
		names = append(names, string(stringTable[nameOffset:nameOffset+end]))
	}
	return names, nil
}

func isCanarySymbol(name string) bool {
	if name == "__stack_chk_fail" {
		return true
	}
	return strings.HasPrefix(name, "__stack_chk_fail")
}

func canaryFromBuildAttributes(file *elf.File) (string, string, bool) {
	for _, section := range file.Sections {
		name := strings.ToLower(section.Name)
		if !strings.Contains(name, "build.attributes") && !strings.Contains(name, "build-attributes") {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		text := strings.ToLower(string(data))
		text = strings.ReplaceAll(text, "no-stack-protector", " ")
		text = strings.ReplaceAll(text, "no_stack_protector", " ")
		for _, marker := range []string{"stack-protector", "stack_protector", "stack protector"} {
			if strings.Contains(text, marker) {
				return section.Name, marker, true
			}
		}
	}
	return "", "", false
}

func readDynamic(file *elf.File, data []byte) (dynamicInfo, error) {
	var info dynamicInfo
	entrySize := 8
	if file.Class == elf.ELFCLASS64 {
		entrySize = 16
	}

	parse := func(entries []dynamicEntry) {
		for _, entry := range entries {
			info.hasEntry = true
			switch entry.tag {
			case uint64(dtBindNow):
				info.bindNow = true
			case uint64(dtFlags):
				if entry.value&dfBindNow != 0 {
					info.bindNow = true
				}
			case uint64(dtFlagsOne):
				if entry.value&dfOneNow != 0 {
					info.bindNow = true
				}
			}
		}
	}

	for _, program := range file.Progs {
		if program.Type != elf.PT_DYNAMIC {
			continue
		}
		start, size, ok := fileRange(program.Off, program.Filesz, len(data))
		if !ok {
			return info, ErrMalformed
		}
		entries, ok := parseDynamicEntries(data[start:start+size], file.ByteOrder, entrySize)
		if !ok {
			return info, ErrMalformed
		}
		parse(entries)
	}
	for _, section := range file.Sections {
		if section.Type != elf.SHT_DYNAMIC {
			continue
		}
		sectionData, err := section.Data()
		if err != nil {
			return info, ErrMalformed
		}
		entries, ok := parseDynamicEntries(sectionData, file.ByteOrder, entrySize)
		if !ok {
			return info, ErrMalformed
		}
		parse(entries)
	}
	return info, nil
}

func parseDynamicEntries(data []byte, order interface {
	Uint32([]byte) uint32
	Uint64([]byte) uint64
}, entrySize int) ([]dynamicEntry, bool) {
	if len(data)%entrySize != 0 {
		return nil, false
	}
	entries := make([]dynamicEntry, 0, len(data)/entrySize)
	for offset := 0; offset+entrySize <= len(data); offset += entrySize {
		entry := dynamicEntry{}
		if entrySize == 8 {
			entry.tag = uint64(order.Uint32(data[offset : offset+4]))
			entry.value = uint64(order.Uint32(data[offset+4 : offset+8]))
		} else {
			entry.tag = order.Uint64(data[offset : offset+8])
			entry.value = order.Uint64(data[offset+8 : offset+16])
		}
		entries = append(entries, entry)
	}
	return entries, true
}

type parsedNote struct {
	name string
	typ  uint32
	desc []byte
}

func parseNotes(data []byte, order interface {
	Uint32([]byte) uint32
}) ([]parsedNote, bool) {
	notes := make([]parsedNote, 0)
	for offset := 0; ; {
		if offset == len(data) {
			return notes, true
		}
		if len(data)-offset < 12 {
			return notes, false
		}
		namesz := int(order.Uint32(data[offset : offset+4]))
		descsz := int(order.Uint32(data[offset+4 : offset+8]))
		typ := order.Uint32(data[offset+8 : offset+12])
		offset += 12
		if namesz < 0 || descsz < 0 {
			return notes, false
		}
		nameEnd := offset + align(namesz, 4)
		if nameEnd < offset || nameEnd > len(data) {
			return notes, false
		}
		name := string(data[offset : offset+namesz])
		descStart := nameEnd
		descEnd := descStart + descsz
		if descEnd < descStart || descEnd > len(data) {
			return notes, false
		}
		next := descEnd + padding(descsz, 4)
		if next < descEnd || next > len(data) {
			return notes, false
		}
		notes = append(notes, parsedNote{name: name, typ: typ, desc: data[descStart:descEnd]})
		offset = next
	}
}

type parsedProperty struct {
	typ  uint32
	data []byte
}

func parseGNUProperties(data []byte, order interface {
	Uint32([]byte) uint32
}, is64 bool) ([]parsedProperty, bool) {
	alignment := 4
	if is64 {
		alignment = 8
	}
	properties := make([]parsedProperty, 0)
	for offset := 0; ; {
		if offset == len(data) {
			return properties, true
		}
		if len(data)-offset < 8 {
			return properties, false
		}
		typ := order.Uint32(data[offset : offset+4])
		size := int(order.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size < 0 || size > len(data)-offset {
			return properties, false
		}
		end := offset + size
		properties = append(properties, parsedProperty{typ: typ, data: data[offset:end]})
		next := end + padding(size, alignment)
		if next < end || next > len(data) {
			return properties, false
		}
		offset = next
	}
}

func fileRange(offset, size uint64, dataLen int) (int, int, bool) {
	if offset > uint64(dataLen) {
		return 0, 0, false
	}
	if size > uint64(dataLen)-offset {
		return 0, 0, false
	}
	return int(offset), int(size), true
}

func align(value, alignment int) int {
	return (value + alignment - 1) &^ (alignment - 1)
}

func padding(value, alignment int) int {
	return align(value, alignment) - value
}

func (p *MitigationPostureProber) maxFileBytes() int {
	if p.cfg.MaxFileBytes > 0 {
		return p.cfg.MaxFileBytes
	}
	return defaultMaxFileBytes
}

func (p *MitigationPostureProber) maxWorkers() int {
	if p.cfg.MaxWorkers > 0 {
		return p.cfg.MaxWorkers
	}
	return 1
}

func (p *MitigationPostureProber) maxArtifacts() int {
	if p.cfg.MaxArtifacts > 0 {
		return p.cfg.MaxArtifacts
	}
	return defaultMaxArtifacts
}

func normalizeBaselineEntry(posture Posture) (Posture, bool) {
	if posture.ID == "" {
		return Posture{}, false
	}
	if posture.Name == "" {
		posture.Name = posture.ID
	}
	return posture, true
}

func cloneEvidence(items []Evidence) []Evidence {
	if items == nil {
		return nil
	}
	out := make([]Evidence, len(items))
	copy(out, items)
	return out
}

func orderEvidence(items []Evidence) []Evidence {
	byField := make(map[string]Evidence, len(items))
	for _, item := range items {
		byField[item.Field] = item
	}
	out := make([]Evidence, 0, len(items))
	for _, field := range explainFieldOrder {
		if item, exists := byField[field]; exists {
			out = append(out, item)
		}
	}
	return out
}

func postureField(posture Posture, field string) string {
	switch field {
	case FieldPIE:
		return posture.PIE
	case FieldRelro:
		return posture.Relro
	case FieldStackCanary:
		return posture.StackCanary
	case FieldNXStack:
		return posture.NXStack
	case FieldCFI:
		return posture.CFI
	default:
		return ""
	}
}

func compareSeverity(field, before, after string) string {
	if before == StateUnknown || before == "" {
		if after != StateUnknown && after != "" {
			return SeverityImprovement
		}
		return SeverityRegression
	}
	if after == StateUnknown || after == "" {
		return SeverityRegression
	}
	beforeRank := stateRank(field, before)
	afterRank := stateRank(field, after)
	if afterRank > beforeRank {
		return SeverityImprovement
	}
	return SeverityRegression
}

func stateRank(field, state string) int {
	switch field {
	case FieldRelro:
		switch state {
		case StateFull:
			return 3
		case StatePartial:
			return 2
		case StateNone:
			return 1
		}
	case FieldPIE, FieldNXStack:
		switch state {
		case StateEnabled:
			return 2
		case StateDisabled:
			return 1
		}
	case FieldStackCanary, FieldCFI:
		switch state {
		case StatePresent:
			return 2
		case StateAbsent:
			return 1
		}
	}
	return 0
}

func fallbackEvidence(posture Posture, field string) Evidence {
	source := ""
	switch field {
	case FieldPIE:
		source = "文件头"
	case FieldNXStack, FieldRelro:
		source = "程序头"
	case FieldStackCanary:
		source = "符号表"
	case FieldCFI:
		source = "属性笔记"
	}
	return Evidence{
		Field:  field,
		Source: source,
		Detail: fmt.Sprintf("恢复的结论为 %s", postureField(posture, field)),
	}
}

func unmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing JSON value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	}
	return nil
}
