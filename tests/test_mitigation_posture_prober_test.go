package task

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-43"
)

type elfSpec struct {
	eType   uint16
	stack   int // 0 means no stack program header, 1 means rw, 2 means rwx
	relro   bool
	bindNow bool
	canary  bool
	ibt     bool
}

func align8(n int) int { return (n + 7) &^ 7 }

func buildNote(ibt bool) []byte {
	note := make([]byte, 32)
	binary.LittleEndian.PutUint32(note[0:], 4)
	binary.LittleEndian.PutUint32(note[4:], 16)
	binary.LittleEndian.PutUint32(note[8:], 5)
	copy(note[12:], "GNU\x00")
	binary.LittleEndian.PutUint32(note[16:], 0xc0000002)
	binary.LittleEndian.PutUint32(note[20:], 4)
	if ibt {
		binary.LittleEndian.PutUint32(note[24:], 1)
	}
	return note
}

func buildELF(spec elfSpec) []byte {
	dynstr := []byte("\x00__stack_chk_fail\x00main\x00")
	symName := uint32(1)
	if !spec.canary {
		symName = 18
	}
	dynsym := make([]byte, 48)
	binary.LittleEndian.PutUint32(dynsym[24:], symName)
	dynsym[28] = 0x12
	binary.LittleEndian.PutUint16(dynsym[30:], 1)

	dynamic := make([]byte, 32)
	if spec.bindNow {
		binary.LittleEndian.PutUint64(dynamic[0:], 30)
		binary.LittleEndian.PutUint64(dynamic[8:], 8)
	}

	note := buildNote(spec.ibt)
	shstr := []byte("\x00.dynstr\x00.dynsym\x00.dynamic\x00.note.gnu.property\x00.shstrtab\x00")

	phnum := 0
	if spec.stack != 0 {
		phnum++
	}
	if spec.relro {
		phnum += 2
	}

	phoff := 64
	dataOff := phoff + phnum*56
	offDynstr := dataOff
	offDynsym := align8(offDynstr + len(dynstr))
	offDynamic := offDynsym + len(dynsym)
	offNote := offDynamic + len(dynamic)
	offShstr := offNote + len(note)
	shoff := align8(offShstr + len(shstr))
	out := make([]byte, shoff+6*64)

	copy(out, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0})
	binary.LittleEndian.PutUint16(out[16:], spec.eType)
	binary.LittleEndian.PutUint16(out[18:], 62)
	binary.LittleEndian.PutUint32(out[20:], 1)
	binary.LittleEndian.PutUint64(out[32:], uint64(phoff))
	binary.LittleEndian.PutUint64(out[40:], uint64(shoff))
	binary.LittleEndian.PutUint16(out[52:], 64)
	binary.LittleEndian.PutUint16(out[54:], 56)
	binary.LittleEndian.PutUint16(out[56:], uint16(phnum))
	binary.LittleEndian.PutUint16(out[58:], 64)
	binary.LittleEndian.PutUint16(out[60:], 6)
	binary.LittleEndian.PutUint16(out[62:], 5)

	copy(out[offDynstr:], dynstr)
	copy(out[offDynsym:], dynsym)
	copy(out[offDynamic:], dynamic)
	copy(out[offNote:], note)
	copy(out[offShstr:], shstr)

	ph := phoff
	if spec.stack != 0 {
		binary.LittleEndian.PutUint32(out[ph:], 0x6474e551)
		flags := uint32(6)
		if spec.stack == 2 {
			flags = 7
		}
		binary.LittleEndian.PutUint32(out[ph+4:], flags)
		ph += 56
	}
	if spec.relro {
		binary.LittleEndian.PutUint32(out[ph:], 0x6474e552)
		binary.LittleEndian.PutUint32(out[ph+4:], 4)
		binary.LittleEndian.PutUint64(out[ph+8:], uint64(offDynamic))
		binary.LittleEndian.PutUint64(out[ph+32:], uint64(len(dynamic)))
		binary.LittleEndian.PutUint64(out[ph+40:], uint64(len(dynamic)))
		ph += 56
		binary.LittleEndian.PutUint32(out[ph:], 2)
		binary.LittleEndian.PutUint32(out[ph+4:], 6)
		binary.LittleEndian.PutUint64(out[ph+8:], uint64(offDynamic))
		binary.LittleEndian.PutUint64(out[ph+32:], uint64(len(dynamic)))
		binary.LittleEndian.PutUint64(out[ph+40:], uint64(len(dynamic)))
		ph += 56
	}

	section := func(i int, nameOff, typ uint32, flags uint64, off, size uint64, link, info uint32, align, entsize uint64) {
		base := shoff + i*64
		binary.LittleEndian.PutUint32(out[base:], nameOff)
		binary.LittleEndian.PutUint32(out[base+4:], typ)
		binary.LittleEndian.PutUint64(out[base+8:], flags)
		binary.LittleEndian.PutUint64(out[base+24:], off)
		binary.LittleEndian.PutUint64(out[base+32:], size)
		binary.LittleEndian.PutUint32(out[base+40:], link)
		binary.LittleEndian.PutUint32(out[base+44:], info)
		binary.LittleEndian.PutUint64(out[base+48:], align)
		binary.LittleEndian.PutUint64(out[base+56:], entsize)
	}
	section(1, 1, 3, 0, uint64(offDynstr), uint64(len(dynstr)), 0, 0, 1, 0)
	section(2, 9, 11, 2, uint64(offDynsym), uint64(len(dynsym)), 1, 1, 8, 24)
	section(3, 17, 6, 2, uint64(offDynamic), uint64(len(dynamic)), 1, 0, 8, 16)
	section(4, 26, 7, 2, uint64(offNote), uint64(len(note)), 0, 0, 8, 0)
	section(5, 45, 3, 0, uint64(offShstr), uint64(len(shstr)), 0, 0, 1, 0)
	return out
}

func hardenedSpec() elfSpec {
	return elfSpec{eType: 3, stack: 1, relro: true, bindNow: true, canary: true, ibt: true}
}

func proberConfig43() impl.Config {
	return impl.Config{MaxFileBytes: 1 << 16, MaxWorkers: 2, MaxArtifacts: 16}
}

func TestInspectRejectsUnusableInput(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	if _, err := p.Inspect("empty", nil); !errors.Is(err, impl.ErrEmptyInput) {
		t.Fatalf("empty payload must fail with ErrEmptyInput, got %v", err)
	}
	if _, err := p.Inspect("dos", []byte("MZ\x90\x00stub")); !errors.Is(err, impl.ErrUnknownFormat) {
		t.Fatalf("a non ELF payload must fail with ErrUnknownFormat, got %v", err)
	}
	tiny := impl.NewMitigationPostureProber(impl.Config{MaxFileBytes: 64, MaxWorkers: 1, MaxArtifacts: 4})
	payload := buildELF(hardenedSpec())
	if _, err := tiny.Inspect("big", payload); !errors.Is(err, impl.ErrFileTooLarge) {
		t.Fatalf("a payload above the byte cap must fail with ErrFileTooLarge, got %v", err)
	}
	if got := len(p.Artifacts()); got != 0 {
		t.Fatalf("rejected payloads must not be recorded, got %d artifacts", got)
	}
}

func TestInspectReportsELF64Basics(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	payload := buildELF(hardenedSpec())
	posture, err := p.Inspect("svc-a", payload)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if posture.Format != impl.FormatELF || posture.Class != impl.ClassELF64 {
		t.Fatalf("format=%q class=%q, want elf/elf64", posture.Format, posture.Class)
	}
	if posture.ID != "svc-a" || posture.Name != "svc-a" {
		t.Fatalf("id=%q name=%q, want svc-a", posture.ID, posture.Name)
	}
	sum := sha256.Sum256(payload)
	if posture.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want the lower case SHA-256 of the payload", posture.Digest)
	}
}

func TestInspectPIEFlagsByType(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	dyn, err := p.Inspect("pie", buildELF(elfSpec{eType: 3, stack: 1}))
	if err != nil {
		t.Fatalf("inspect shared object: %v", err)
	}
	if dyn.PIE != impl.StateEnabled {
		t.Fatalf("ET_DYN pie = %q, want %q", dyn.PIE, impl.StateEnabled)
	}
	exe, err := p.Inspect("exe", buildELF(elfSpec{eType: 2, stack: 1}))
	if err != nil {
		t.Fatalf("inspect executable: %v", err)
	}
	if exe.PIE != impl.StateDisabled {
		t.Fatalf("ET_EXEC pie = %q, want %q", exe.PIE, impl.StateDisabled)
	}
}

func TestInspectRelroFullWhenBindNow(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	posture, err := p.Inspect("full", buildELF(elfSpec{eType: 3, stack: 1, relro: true, bindNow: true}))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if posture.Relro != impl.StateFull {
		t.Fatalf("relro = %q, want %q for a read only relocation segment with bind now", posture.Relro, impl.StateFull)
	}
}

func TestInspectRelroNoneWithoutSegment(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	posture, err := p.Inspect("none", buildELF(elfSpec{eType: 3, stack: 1}))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if posture.Relro != impl.StateNone {
		t.Fatalf("relro = %q, want %q when the payload has no relocation segment", posture.Relro, impl.StateNone)
	}
}

func TestInspectNXFromGNUStack(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	rw, err := p.Inspect("rw-stack", buildELF(elfSpec{eType: 3, stack: 1}))
	if err != nil {
		t.Fatalf("inspect rw stack: %v", err)
	}
	if rw.NXStack != impl.StateEnabled {
		t.Fatalf("nx stack = %q, want %q for a non executable stack header", rw.NXStack, impl.StateEnabled)
	}
	rwx, err := p.Inspect("rwx-stack", buildELF(elfSpec{eType: 3, stack: 2}))
	if err != nil {
		t.Fatalf("inspect rwx stack: %v", err)
	}
	if rwx.NXStack != impl.StateDisabled {
		t.Fatalf("nx stack = %q, want %q for an executable stack header", rwx.NXStack, impl.StateDisabled)
	}
}

func TestInspectNXUnknownWithoutStackHeader(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	posture, err := p.Inspect("no-stack-header", buildELF(elfSpec{eType: 3}))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if posture.NXStack != impl.StateUnknown {
		t.Fatalf("nx stack = %q, want %q when the stack header is missing", posture.NXStack, impl.StateUnknown)
	}
}

func TestInspectCanaryFromSymbolTable(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	guarded, err := p.Inspect("guarded", buildELF(elfSpec{eType: 3, stack: 1, canary: true}))
	if err != nil {
		t.Fatalf("inspect guarded: %v", err)
	}
	if guarded.StackCanary != impl.StatePresent {
		t.Fatalf("stack canary = %q, want %q when the symbol is exported", guarded.StackCanary, impl.StatePresent)
	}
	plain, err := p.Inspect("plain", buildELF(elfSpec{eType: 3, stack: 1}))
	if err != nil {
		t.Fatalf("inspect plain: %v", err)
	}
	if plain.StackCanary != impl.StateAbsent {
		t.Fatalf("stack canary = %q, want %q when the symbol table has no such name", plain.StackCanary, impl.StateAbsent)
	}
}

func TestInspectCFIFromPropertyNote(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	withIBT, err := p.Inspect("ibt", buildELF(elfSpec{eType: 3, stack: 1, ibt: true}))
	if err != nil {
		t.Fatalf("inspect ibt: %v", err)
	}
	if withIBT.CFI != impl.StatePresent {
		t.Fatalf("cfi = %q, want %q when the note carries the IBT feature bit", withIBT.CFI, impl.StatePresent)
	}
	without, err := p.Inspect("no-ibt", buildELF(elfSpec{eType: 3, stack: 1}))
	if err != nil {
		t.Fatalf("inspect without ibt: %v", err)
	}
	if without.CFI != impl.StateAbsent {
		t.Fatalf("cfi = %q, want %q when the feature note is clear", without.CFI, impl.StateAbsent)
	}
}

func TestInspectDeterministicAndConcurrent(t *testing.T) {
	payload := buildELF(hardenedSpec())
	p := impl.NewMitigationPostureProber(impl.Config{MaxFileBytes: 1 << 16, MaxWorkers: 4, MaxArtifacts: 32})
	first, err := p.Inspect("same", payload)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	second, err := p.Inspect("same", payload)
	if err != nil {
		t.Fatalf("second inspect: %v", err)
	}
	if first != second {
		t.Fatalf("two runs over identical bytes disagree: %+v vs %+v", first, second)
	}
	if got := len(p.Artifacts()); got != 1 {
		t.Fatalf("re-inspecting one name recorded %d artifacts, want 1", got)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	mismatch := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := p.Inspect(string(rune('a'+i))+"-bin", payload)
			if err != nil {
				mu.Lock()
				mismatch++
				mu.Unlock()
				return
			}
			if got.PIE != first.PIE || got.Relro != first.Relro || got.StackCanary != first.StackCanary {
				mu.Lock()
				mismatch++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if mismatch != 0 {
		t.Fatalf("%d concurrent inspections disagreed with the serial run", mismatch)
	}
	if got := len(p.Artifacts()); got != 9 {
		t.Fatalf("recorded artifacts = %d, want 9", got)
	}
}

func TestInspectAllPreservesOrder(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	items := []impl.Artifact{
		{Name: "first", Data: buildELF(elfSpec{eType: 3, stack: 1})},
		{Name: "second", Data: buildELF(elfSpec{eType: 2, stack: 2})},
		{Name: "third", Data: buildELF(hardenedSpec())},
	}
	got, err := p.InspectAll(items)
	if err != nil {
		t.Fatalf("inspect all: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("batch results = %d, want 3", len(got))
	}
	for i, want := range []struct{ name, pie string }{
		{"first", impl.StateEnabled},
		{"second", impl.StateDisabled},
		{"third", impl.StateEnabled},
	} {
		if got[i].Name != want.name {
			t.Fatalf("result %d name = %q, want %q", i, got[i].Name, want.name)
		}
		if got[i].PIE != want.pie {
			t.Fatalf("result %d pie = %q, want %q", i, got[i].PIE, want.pie)
		}
	}
}

func TestCompareReportsSeverity(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	base := impl.Posture{ID: "svc", Name: "svc", PIE: impl.StateEnabled, Relro: impl.StateFull, StackCanary: impl.StatePresent}
	cur := impl.Posture{ID: "svc", Name: "svc", PIE: impl.StateDisabled, Relro: impl.StateNone, StackCanary: impl.StateAbsent}
	deltas, err := p.Compare(base, cur)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(deltas) != 3 {
		t.Fatalf("delta count = %d, want 3 worsened fields", len(deltas))
	}
	wantFields := []string{impl.FieldPIE, impl.FieldRelro, impl.FieldStackCanary}
	for i, field := range wantFields {
		if deltas[i].Field != field {
			t.Fatalf("delta %d field = %q, want %q", i, deltas[i].Field, field)
		}
		if deltas[i].Severity != impl.SeverityRegression {
			t.Fatalf("delta %d severity = %q, want %q", i, deltas[i].Severity, impl.SeverityRegression)
		}
	}
	if deltas[0].Before != impl.StateEnabled || deltas[0].After != impl.StateDisabled {
		t.Fatalf("pie delta before=%q after=%q, want enabled->disabled", deltas[0].Before, deltas[0].After)
	}

	better := impl.Posture{ID: "svc", Name: "svc", PIE: impl.StateEnabled, Relro: impl.StateFull, StackCanary: impl.StatePresent}
	improved, err := p.Compare(cur, better)
	if err != nil {
		t.Fatalf("compare improvement: %v", err)
	}
	if len(improved) != 3 {
		t.Fatalf("improvement delta count = %d, want 3", len(improved))
	}
	if improved[0].Severity != impl.SeverityImprovement {
		t.Fatalf("improvement severity = %q, want %q", improved[0].Severity, impl.SeverityImprovement)
	}

	same, err := p.Compare(base, base)
	if err != nil {
		t.Fatalf("compare identical: %v", err)
	}
	if len(same) != 0 {
		t.Fatalf("identical postures produced %d deltas, want none", len(same))
	}
}

func TestBaselineReplacesWholeSet(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	old := []impl.Posture{
		{ID: "svc-a", Name: "svc-a", PIE: impl.StateEnabled},
		{ID: "svc-b", Name: "svc-b", PIE: impl.StateDisabled},
	}
	if err := p.Baseline(old); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if got := p.BaselineSize(); got != 2 {
		t.Fatalf("baseline size = %d, want 2", got)
	}
	replacement := []impl.Posture{{ID: "svc-c", Name: "svc-c", PIE: impl.StateEnabled}}
	if err := p.Baseline(replacement); err != nil {
		t.Fatalf("replace baseline: %v", err)
	}
	if got := p.BaselineSize(); got != 1 {
		t.Fatalf("baseline size after replacement = %d, want 1", got)
	}
	if _, ok := p.BaselineOf("svc-a"); ok {
		t.Fatalf("the previous baseline entry must be gone after a full replacement")
	}
	if entry, ok := p.BaselineOf("svc-c"); !ok || entry.PIE != impl.StateEnabled {
		t.Fatalf("replacement baseline entry missing or wrong: %+v ok=%v", entry, ok)
	}
	if err := p.Baseline([]impl.Posture{{ID: "", Name: "broken"}}); !errors.Is(err, impl.ErrBadBaseline) {
		t.Fatalf("an incomplete baseline entry must fail with ErrBadBaseline, got %v", err)
	}
	if got := p.BaselineSize(); got != 1 {
		t.Fatalf("a rejected baseline must keep the previous one, size = %d", got)
	}
}

func TestExplainReturnsEvidenceAndRejectsUnknown(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	if _, err := p.Inspect("svc-a", buildELF(hardenedSpec())); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	evidence, err := p.Explain("svc-a")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(evidence) != 5 {
		t.Fatalf("evidence entries = %d, want one per conclusion", len(evidence))
	}
	wantFields := []string{impl.FieldStackCanary, impl.FieldCFI, impl.FieldNXStack, impl.FieldPIE, impl.FieldRelro}
	for i, field := range wantFields {
		if evidence[i].Field != field {
			t.Fatalf("evidence %d field = %q, want %q", i, evidence[i].Field, field)
		}
		if evidence[i].Source == "" || evidence[i].Detail == "" {
			t.Fatalf("evidence %d is incomplete: %+v", i, evidence[i])
		}
	}
	if _, err := p.Explain("svc-404"); !errors.Is(err, impl.ErrUnknownArtifact) {
		t.Fatalf("unknown artifact must fail with ErrUnknownArtifact, got %v", err)
	}
}

func TestSnapshotRestoreKeepsBaselineAndPostures(t *testing.T) {
	p := impl.NewMitigationPostureProber(proberConfig43())
	if _, err := p.Inspect("svc-a", buildELF(hardenedSpec())); err != nil {
		t.Fatalf("inspect svc-a: %v", err)
	}
	if _, err := p.Inspect("svc-b", buildELF(elfSpec{eType: 2, stack: 2})); err != nil {
		t.Fatalf("inspect svc-b: %v", err)
	}
	if err := p.Baseline([]impl.Posture{{ID: "svc-a", Name: "svc-a", PIE: impl.StateEnabled}}); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewMitigationPostureProber(proberConfig43())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.BaselineSize(); got != 1 {
		t.Fatalf("restored baseline size = %d, want 1", got)
	}
	if got := len(restored.Artifacts()); got != 2 {
		t.Fatalf("restored artifacts = %d, want 2", got)
	}
	if _, err := restored.Explain("svc-a"); err != nil {
		t.Fatalf("explain after restore: %v", err)
	}
	posture, ok := restored.Posture("svc-b")
	if !ok || posture.PIE != impl.StateDisabled || posture.NXStack != impl.StateDisabled {
		t.Fatalf("restored posture = %+v ok=%v, want the ET_EXEC rwx entry", posture, ok)
	}
}
