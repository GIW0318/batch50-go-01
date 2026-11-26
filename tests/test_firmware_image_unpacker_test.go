package task

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-39"
)

const (
	headerSize = 32
	entrySize  = 32
)

type partitionSpec struct {
	name string
	kind uint32
	body []byte
}

type fileSpec struct {
	name string
	body []byte
}

func checksum(b []byte) uint32 {
	var sum uint32
	for _, c := range b {
		sum += uint32(c)
	}
	return sum
}

func tableEntry(name string, offset, size, kind uint32) []byte {
	e := make([]byte, entrySize)
	copy(e, name)
	binary.LittleEndian.PutUint32(e[16:], offset)
	binary.LittleEndian.PutUint32(e[20:], size)
	binary.LittleEndian.PutUint32(e[24:], kind)
	return e
}

func buildHeader(count int, tableOffset uint32, table []byte) []byte {
	h := make([]byte, headerSize)
	copy(h, "FWIM")
	binary.LittleEndian.PutUint32(h[4:], 1)
	binary.LittleEndian.PutUint32(h[8:], tableOffset)
	binary.LittleEndian.PutUint32(h[12:], uint32(count))
	binary.LittleEndian.PutUint32(h[16:], checksum(table))
	return h
}

func buildImage(parts ...partitionSpec) []byte {
	tableOffset := uint32(headerSize)
	bodyStart := headerSize + len(parts)*entrySize
	table := make([]byte, 0, len(parts)*entrySize)
	var body []byte
	offset := uint32(bodyStart)
	for _, p := range parts {
		table = append(table, tableEntry(p.name, offset, uint32(len(p.body)), p.kind)...)
		body = append(body, p.body...)
		offset += uint32(len(p.body))
	}
	img := make([]byte, 0, bodyStart+len(body))
	img = append(img, buildHeader(len(parts), tableOffset, table)...)
	img = append(img, table...)
	img = append(img, body...)
	return img
}

func buildOverlappingImage() []byte {
	table := append(tableEntry("boot", 100, 40, impl.KindRaw), tableEntry("root", 120, 40, impl.KindRaw)...)
	img := append(buildHeader(2, headerSize, table), table...)
	return append(img, make([]byte, 200)...)
}

func buildFWFS(files ...fileSpec) []byte {
	out := make([]byte, 8)
	copy(out, "FWFS")
	binary.LittleEndian.PutUint32(out[4:], uint32(len(files)))
	for _, f := range files {
		n := make([]byte, 4)
		binary.LittleEndian.PutUint32(n, uint32(len(f.name)))
		out = append(out, n...)
		out = append(out, f.name...)
		s := make([]byte, 4)
		binary.LittleEndian.PutUint32(s, uint32(len(f.body)))
		out = append(out, s...)
		out = append(out, f.body...)
	}
	return out
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newUnpacker() *impl.FirmwareImageUnpacker {
	return impl.NewFirmwareImageUnpacker(impl.Limits{
		MaxDepth: 3, MaxFileBytes: 64, MaxTotalBytes: 1024, MaxImages: 8,
	})
}

func mustOpen(t *testing.T, u *impl.FirmwareImageUnpacker, raw []byte) impl.Image {
	t.Helper()
	img, err := u.Open(raw)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	return img
}

func mustPartitions(t *testing.T, u *impl.FirmwareImageUnpacker, img impl.Image) []impl.Partition {
	t.Helper()
	parts, err := u.Partitions(img)
	if err != nil {
		t.Fatalf("partitions: %v", err)
	}
	return parts
}

func mustExtract(t *testing.T, u *impl.FirmwareImageUnpacker, img impl.Image, sel impl.Selector) []impl.Entry {
	t.Helper()
	entries, err := u.Extract(img, sel)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return entries
}

func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func withUint32(b []byte, field int, value uint32) []byte {
	out := clone(b)
	binary.LittleEndian.PutUint32(out[field:], value)
	return out
}

func TestOpenRejectsBadImages(t *testing.T) {
	u := newUnpacker()
	good := buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("hello")})
	bad := [][]byte{
		{},
		append([]byte("NOPE"), good[4:]...),
		withUint32(good, 4, 9),
		good[:20],
		withUint32(good, 12, 0),
		withUint32(good, 12, 40),
		withUint32(good, 8, 4000),
		withUint32(good, 16, 0xdeadbeef),
	}
	for i, raw := range bad {
		if _, err := u.Open(raw); !errors.Is(err, impl.ErrBadImage) {
			t.Fatalf("bad image %d must fail with ErrBadImage, got %v", i, err)
		}
	}
	if got := u.Images(); len(got) != 0 {
		t.Fatalf("a rejected image must not be registered, got %v", got)
	}
}

func TestOpenRegistersImageAndClose(t *testing.T) {
	u := newUnpacker()
	raw := buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("hello")})
	img := mustOpen(t, u, raw)
	if img.ID == "" {
		t.Fatalf("an open image must carry an id")
	}
	if img.Size != len(raw) {
		t.Fatalf("size = %d, want %d", img.Size, len(raw))
	}
	if img.Digest != digestOf(raw) {
		t.Fatalf("digest = %q, want the sha256 of the image", img.Digest)
	}
	if got := u.Images(); len(got) != 1 || got[0] != img.ID {
		t.Fatalf("images = %v, want [%s]", got, img.ID)
	}
	if err := u.Close(img); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := u.Images(); len(got) != 0 {
		t.Fatalf("a closed image must be gone, got %v", got)
	}
	if _, err := u.Extract(img, impl.Selector{}); !errors.Is(err, impl.ErrNotOpen) {
		t.Fatalf("extracting a closed image must fail with ErrNotOpen, got %v", err)
	}
}

func TestOpenEnforcesImageCap(t *testing.T) {
	u := impl.NewFirmwareImageUnpacker(impl.Limits{MaxDepth: 3, MaxFileBytes: 64, MaxTotalBytes: 1024, MaxImages: 1})
	mustOpen(t, u, buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("one")}))
	if _, err := u.Open(buildImage(partitionSpec{name: "root", kind: impl.KindRaw, body: []byte("two")})); !errors.Is(err, impl.ErrTooManyImages) {
		t.Fatalf("a second image under a cap of one must fail with ErrTooManyImages, got %v", err)
	}
	if got := u.Images(); len(got) != 1 {
		t.Fatalf("images = %v, want one open image", got)
	}
}

func TestPartitionsReturnsTable(t *testing.T) {
	u := newUnpacker()
	raw := buildImage(
		partitionSpec{name: "root", kind: impl.KindRaw, body: []byte("12345678")},
		partitionSpec{name: "boot", kind: impl.KindNested, body: []byte("abcde")},
	)
	img := mustOpen(t, u, raw)
	parts := mustPartitions(t, u, img)
	if len(parts) != 2 {
		t.Fatalf("partitions = %d, want 2", len(parts))
	}
	wantNames := []string{"root", "boot"}
	wantOffset := []uint32{uint32(headerSize + 2*entrySize), uint32(headerSize + 2*entrySize + 8)}
	wantSize := []uint32{8, 5}
	wantKind := []uint32{impl.KindRaw, impl.KindNested}
	for i := range parts {
		if parts[i].Name != wantNames[i] {
			t.Fatalf("partition %d name = %q, want %q", i, parts[i].Name, wantNames[i])
		}
		if parts[i].Offset != wantOffset[i] || parts[i].Size != wantSize[i] {
			t.Fatalf("partition %d = %d+%d, want %d+%d", i, parts[i].Offset, parts[i].Size, wantOffset[i], wantSize[i])
		}
		if parts[i].Kind != wantKind[i] {
			t.Fatalf("partition %d kind = %d, want %d", i, parts[i].Kind, wantKind[i])
		}
	}
}

func TestPartitionsRejectsOverlap(t *testing.T) {
	u := newUnpacker()
	img := mustOpen(t, u, buildOverlappingImage())
	if _, err := u.Partitions(img); !errors.Is(err, impl.ErrOverlap) {
		t.Fatalf("overlapping partitions must fail with ErrOverlap, got %v", err)
	}
}

func TestExtractReturnsRawPartition(t *testing.T) {
	u := newUnpacker()
	body := []byte("hello firmware")
	img := mustOpen(t, u, buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: body}))
	entries := mustExtract(t, u, img, impl.Selector{})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(entries), entries)
	}
	if entries[0].Path != "boot" {
		t.Fatalf("path = %q, want boot", entries[0].Path)
	}
	if entries[0].Size != len(body) {
		t.Fatalf("size = %d, want %d", entries[0].Size, len(body))
	}
	if entries[0].SHA256 != digestOf(body) {
		t.Fatalf("digest = %q, want the sha256 of the payload", entries[0].SHA256)
	}
	if !entries[0].Complete {
		t.Fatalf("a fully extracted file must be marked complete")
	}
}

func TestExtractNestedFilesystem(t *testing.T) {
	u := newUnpacker()
	raw := buildImage(partitionSpec{
		name: "rootfs", kind: impl.KindNested,
		body: buildFWFS(
			fileSpec{name: "etc/passwd", body: []byte("root:x:0:0")},
			fileSpec{name: "init.sh", body: []byte("#!/bin/sh")},
		),
	})
	img := mustOpen(t, u, raw)
	entries := mustExtract(t, u, img, impl.Selector{})
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}
	wantPath := []string{"rootfs/etc/passwd", "rootfs/init.sh"}
	wantBody := []string{"root:x:0:0", "#!/bin/sh"}
	for i := range wantPath {
		if entries[i].Path != wantPath[i] {
			t.Fatalf("entry %d path = %q, want %q", i, entries[i].Path, wantPath[i])
		}
		if entries[i].Size != len(wantBody[i]) || entries[i].SHA256 != digestOf([]byte(wantBody[i])) {
			t.Fatalf("entry %d = %d/%q, want %d bytes of the nested file", i, entries[i].Size, entries[i].SHA256, len(wantBody[i]))
		}
		if !entries[i].Complete {
			t.Fatalf("entry %d must be complete", i)
		}
	}
}

func TestExtractSelectorLimitsScope(t *testing.T) {
	u := newUnpacker()
	raw := buildImage(
		partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("kernel")},
		partitionSpec{name: "rootfs", kind: impl.KindNested, body: buildFWFS(fileSpec{name: "init.sh", body: []byte("run")})},
	)
	img := mustOpen(t, u, raw)
	only := mustExtract(t, u, img, impl.Selector{Partition: "rootfs"})
	if len(only) != 1 || only[0].Path != "rootfs/init.sh" {
		t.Fatalf("selected entries = %+v, want just rootfs/init.sh", only)
	}
	all := mustExtract(t, u, img, impl.Selector{})
	if len(all) != 2 {
		t.Fatalf("entries = %d, want 2 across both partitions", len(all))
	}
}

func TestExtractRejectsUnknownPartition(t *testing.T) {
	u := newUnpacker()
	img := mustOpen(t, u, buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("x")}))
	if _, err := u.Extract(img, impl.Selector{Partition: "nope"}); !errors.Is(err, impl.ErrBadSelector) {
		t.Fatalf("an unknown partition must fail with ErrBadSelector, got %v", err)
	}
}

func TestExtractEnforcesDepthBudget(t *testing.T) {
	inner := buildImage(partitionSpec{name: "leaf", kind: impl.KindRaw, body: []byte("z")})
	outer := buildImage(partitionSpec{name: "stage", kind: impl.KindNested, body: inner})
	u := impl.NewFirmwareImageUnpacker(impl.Limits{MaxDepth: 1, MaxFileBytes: 64, MaxTotalBytes: 1024, MaxImages: 4})
	img := mustOpen(t, u, outer)
	if _, err := u.Extract(img, impl.Selector{}); !errors.Is(err, impl.ErrBudget) {
		t.Fatalf("nesting past the depth budget must fail with ErrBudget, got %v", err)
	}
}

func TestExtractEnforcesTotalBudget(t *testing.T) {
	u := impl.NewFirmwareImageUnpacker(impl.Limits{MaxDepth: 3, MaxFileBytes: 64, MaxTotalBytes: 8, MaxImages: 4})
	raw := buildImage(
		partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("abcde")},
		partitionSpec{name: "root", kind: impl.KindRaw, body: []byte("fghij")},
	)
	img := mustOpen(t, u, raw)
	entries, err := u.Extract(img, impl.Selector{})
	if !errors.Is(err, impl.ErrBudget) {
		t.Fatalf("exceeding the total budget must fail with ErrBudget, got %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "boot" {
		t.Fatalf("partial entries = %+v, want the one file finished before the budget ran out", entries)
	}
	if entries[0].Complete {
		t.Fatalf("a partial extraction must mark its entries as incomplete")
	}
}

func TestExtractEnforcesFileBudget(t *testing.T) {
	u := impl.NewFirmwareImageUnpacker(impl.Limits{MaxDepth: 3, MaxFileBytes: 4, MaxTotalBytes: 1024, MaxImages: 4})
	img := mustOpen(t, u, buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("abcdefgh")}))
	entries, err := u.Extract(img, impl.Selector{})
	if !errors.Is(err, impl.ErrBudget) {
		t.Fatalf("a file over the per-file budget must fail with ErrBudget, got %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("an over-budget file must not be reported, got %+v", entries)
	}
}

func TestManifestDeduplicatesByDigest(t *testing.T) {
	u := newUnpacker()
	raw := buildImage(
		partitionSpec{name: "root", kind: impl.KindRaw, body: []byte("shared")},
		partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("shared")},
	)
	img := mustOpen(t, u, raw)
	mustExtract(t, u, img, impl.Selector{})
	data, err := u.Manifest(img)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var report impl.ManifestReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("manifest is not json: %v", err)
	}
	if report.ImageID != img.ID {
		t.Fatalf("manifest image = %q, want %q", report.ImageID, img.ID)
	}
	if len(report.Files) != 1 {
		t.Fatalf("manifest files = %+v, want one digest", report.Files)
	}
	file := report.Files[0]
	if file.SHA256 != digestOf([]byte("shared")) || file.Size != 6 {
		t.Fatalf("manifest file = %+v, want the shared digest and size", file)
	}
	if len(file.Paths) != 2 || file.Paths[0] != "boot" || file.Paths[1] != "root" {
		t.Fatalf("manifest paths = %v, want [boot root]", file.Paths)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("manifest missing = %v, want none", report.Missing)
	}
}

func TestSnapshotRestoreKeepsImages(t *testing.T) {
	u := newUnpacker()
	first := mustOpen(t, u, buildImage(partitionSpec{name: "boot", kind: impl.KindRaw, body: []byte("kernel")}))
	mustOpen(t, u, buildImage(partitionSpec{name: "root", kind: impl.KindRaw, body: []byte("data")}))
	mustExtract(t, u, first, impl.Selector{})
	data, err := u.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newUnpacker()
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Images(); len(got) != 2 {
		t.Fatalf("restored images = %v, want two", got)
	}
	entries := mustExtract(t, restored, first, impl.Selector{})
	if len(entries) != 1 || entries[0].Path != "boot" {
		t.Fatalf("restored extraction = %+v, want the boot partition", entries)
	}
	if err := restored.Restore([]byte("###broken###")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Images(); len(got) != 2 {
		t.Fatalf("a rejected restore must not touch the open images, got %v", got)
	}
}

func TestConcurrentExtractAllowsOneRunner(t *testing.T) {
	u := newUnpacker()
	img := mustOpen(t, u, buildImage(partitionSpec{
		name: "rootfs", kind: impl.KindNested,
		body: buildFWFS(fileSpec{name: "init.sh", body: []byte("run")}),
	}))
	const workers = 8
	var wg sync.WaitGroup
	results := make([][]impl.Entry, workers)
	failures := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entries, err := u.Extract(img, impl.Selector{})
			results[idx] = entries
			failures[idx] = err
		}(i)
	}
	wg.Wait()
	success := 0
	for i := 0; i < workers; i++ {
		if failures[i] != nil {
			if !errors.Is(failures[i], impl.ErrBusy) {
				t.Fatalf("worker %d failed with %v, want success or ErrBusy", i, failures[i])
			}
			continue
		}
		success++
		if len(results[i]) != 1 || results[i][0].Path != "rootfs/init.sh" {
			t.Fatalf("worker %d entries = %+v, want the extracted file", i, results[i])
		}
	}
	if success == 0 {
		t.Fatalf("at least one extraction must run")
	}
	again := mustExtract(t, u, img, impl.Selector{})
	if len(again) != 1 {
		t.Fatalf("a sequential extraction after the workers = %+v, want one entry", again)
	}
}
