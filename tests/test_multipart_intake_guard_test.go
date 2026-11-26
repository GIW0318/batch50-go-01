package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-07"
)

const guardBoundary = "----gsb50boundary"

func guardLimits() impl.Limits {
	return impl.Limits{
		MaxParts:        4,
		MaxHeaderBytes:  512,
		MaxNameLen:      16,
		MaxTotalBytes:   4096,
		MaxNestedDepth:  1,
		DuplicatePolicy: impl.DuplicateReject,
		IdleTimeout:     200 * time.Millisecond,
		NestedTypes:     []string{"multipart/mixed"},
	}
}

func multipartHeader(boundary string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	return h
}

func mustBegin(t *testing.T, g *impl.MultipartIntakeGuard, boundary string) impl.Upload {
	t.Helper()
	up, err := g.Begin(multipartHeader(boundary))
	if err != nil {
		t.Fatalf("begin upload with boundary %q: %v", boundary, err)
	}
	return up
}

func mustWrite(t *testing.T, g *impl.MultipartIntakeGuard, id, body string) {
	t.Helper()
	if _, err := g.Write(id, []byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
}

func mustNext(t *testing.T, g *impl.MultipartIntakeGuard, id string) impl.PartMeta {
	t.Helper()
	meta, err := g.NextPart(id)
	if err != nil {
		t.Fatalf("next part: %v", err)
	}
	return meta
}

func drainPart(t *testing.T, g *impl.MultipartIntakeGuard, id string) string {
	t.Helper()
	buf := make([]byte, 7)
	out := make([]byte, 0, 32)
	for {
		n, err := g.ReadPart(id, buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return string(out)
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		if n == 0 {
			t.Fatalf("read part returned no data and no error")
		}
	}
}

func sampleBody(boundary string) string {
	return "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"title\"\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"hello\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"../../etc/passwd\"\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"\r\n" +
		"payload-bytes\r\n" +
		"--" + boundary + "--\r\n"
}

func singleFileBody(boundary, filename string) string {
	return "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"upload\"; filename=\"" + filename + "\"\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"data\r\n" +
		"--" + boundary + "--\r\n"
}

func partsBody(boundary string, names ...string) string {
	var b strings.Builder
	for i, name := range names {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Disposition: form-data; name=\"" + name + "\"\r\n")
		b.WriteString("Content-Type: text/plain\r\n\r\n")
		b.WriteString(fmt.Sprintf("v%d\r\n", i))
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

func TestBeginRejectsMissingOrBadBoundary(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	plain := http.Header{}
	plain.Set("Content-Type", "application/json")
	if _, err := g.Begin(plain); !errors.Is(err, impl.ErrMissingBoundary) {
		t.Fatalf("json content type must fail with ErrMissingBoundary, got %v", err)
	}
	noParam := http.Header{}
	noParam.Set("Content-Type", "multipart/form-data")
	if _, err := g.Begin(noParam); !errors.Is(err, impl.ErrMissingBoundary) {
		t.Fatalf("multipart without a boundary must fail with ErrMissingBoundary, got %v", err)
	}
	spaced := http.Header{}
	spaced.Set("Content-Type", "multipart/form-data; boundary=\"has space\"")
	if _, err := g.Begin(spaced); !errors.Is(err, impl.ErrBadBoundary) {
		t.Fatalf("boundary with a space must fail with ErrBadBoundary, got %v", err)
	}
	long := http.Header{}
	long.Set("Content-Type", "multipart/form-data; boundary="+strings.Repeat("b", 71))
	if _, err := g.Begin(long); !errors.Is(err, impl.ErrBadBoundary) {
		t.Fatalf("boundary over 70 bytes must fail with ErrBadBoundary, got %v", err)
	}
	if got := len(g.Active()); got != 0 {
		t.Fatalf("rejected begins must not open uploads, got %d", got)
	}
}

func TestBeginAcceptsQuotedBoundary(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	h := http.Header{}
	h.Set("Content-Type", "multipart/form-data; boundary=\"----quoted\"")
	first, err := g.Begin(h)
	if err != nil {
		t.Fatalf("quoted boundary must be accepted, got %v", err)
	}
	if first.Boundary != "----quoted" {
		t.Fatalf("parsed boundary = %q, want the unquoted value", first.Boundary)
	}
	if first.State != impl.UploadOpen {
		t.Fatalf("fresh upload state = %q, want %q", first.State, impl.UploadOpen)
	}
	if first.ID == "" {
		t.Fatalf("upload id must not be empty")
	}
	second := mustBegin(t, g, guardBoundary)
	if second.ID == first.ID {
		t.Fatalf("two uploads must not share one id")
	}
	if got := len(g.Active()); got != 2 {
		t.Fatalf("active uploads = %d, want 2", got)
	}
}

func TestNextPartParsesAcrossChunkBoundaries(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	up := mustBegin(t, g, guardBoundary)
	body := sampleBody(guardBoundary)
	chunks := []string{body[:61], body[61:137], body[137:]}
	for _, chunk := range chunks {
		mustWrite(t, g, up.ID, chunk)
	}
	first := mustNext(t, g, up.ID)
	if first.Index != 0 || first.Name != "title" || first.Filename != "" {
		t.Fatalf("first part meta = %+v, want index 0 name title without filename", first)
	}
	if first.ContentType != "text/plain" || first.Size != 5 {
		t.Fatalf("first part content type=%q size=%d, want text/plain and 5", first.ContentType, first.Size)
	}
	if got := drainPart(t, g, up.ID); got != "hello" {
		t.Fatalf("first part payload = %q, want hello", got)
	}
	second := mustNext(t, g, up.ID)
	if second.Index != 1 || second.Name != "file" {
		t.Fatalf("second part meta = %+v, want index 1 name file", second)
	}
	if second.Filename != "passwd" {
		t.Fatalf("second part filename = %q, want the normalised base name passwd", second.Filename)
	}
	if second.Size != 13 {
		t.Fatalf("second part size = %d, want 13", second.Size)
	}
	if got := drainPart(t, g, up.ID); got != "payload-bytes" {
		t.Fatalf("second part payload = %q, want payload-bytes", got)
	}
	if _, err := g.NextPart(up.ID); !errors.Is(err, io.EOF) {
		t.Fatalf("part enumeration must end with io.EOF, got %v", err)
	}
}

func TestNextPartEnforcesPartLimit(t *testing.T) {
	limits := guardLimits()
	limits.MaxParts = 2
	g := impl.NewMultipartIntakeGuard(limits)
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, partsBody(guardBoundary, "a", "b", "c"))
	for i := 0; i < 2; i++ {
		meta := mustNext(t, g, up.ID)
		if meta.Index != i {
			t.Fatalf("part %d has index %d", i, meta.Index)
		}
	}
	if _, err := g.NextPart(up.ID); !errors.Is(err, impl.ErrPartLimit) {
		t.Fatalf("third part must fail with ErrPartLimit, got %v", err)
	}
}

func TestNextPartEnforcesHeaderLimit(t *testing.T) {
	limits := guardLimits()
	limits.MaxHeaderBytes = 64
	g := impl.NewMultipartIntakeGuard(limits)
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, singleFileBody(guardBoundary, "report.bin"))
	if _, err := g.NextPart(up.ID); !errors.Is(err, impl.ErrHeaderLimit) {
		t.Fatalf("oversized part header must fail with ErrHeaderLimit, got %v", err)
	}
}

func TestFilenameNormalizedAndReservedRejected(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	cases := []struct {
		filename string
		want     string
		wantErr  error
	}{
		{filename: "../../etc/passwd", want: "passwd"},
		{filename: "..\\..\\CON", wantErr: impl.ErrBadFilename},
		{filename: "..", wantErr: impl.ErrBadFilename},
		{filename: "", wantErr: impl.ErrBadFilename},
		{filename: strings.Repeat("a", 17), wantErr: impl.ErrNameTooLong},
	}
	for _, tc := range cases {
		up := mustBegin(t, g, guardBoundary)
		mustWrite(t, g, up.ID, singleFileBody(guardBoundary, tc.filename))
		meta, err := g.NextPart(up.ID)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("filename %q must fail with %v, got %v", tc.filename, tc.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("filename %q must be accepted, got %v", tc.filename, err)
		}
		if meta.Filename != tc.want {
			t.Fatalf("filename %q normalised to %q, want %q", tc.filename, meta.Filename, tc.want)
		}
	}
}

func TestWriteQuotaAbortsUpload(t *testing.T) {
	limits := guardLimits()
	limits.MaxTotalBytes = 32
	g := impl.NewMultipartIntakeGuard(limits)
	up := mustBegin(t, g, guardBoundary)
	payload := strings.Repeat("x", 64)
	if _, err := g.Write(up.ID, []byte(payload)); !errors.Is(err, impl.ErrQuotaExceeded) {
		t.Fatalf("writing past the quota must fail with ErrQuotaExceeded, got %v", err)
	}
	if _, ok := g.Inspect(up.ID); ok {
		t.Fatalf("quota killed upload must be removed from the table")
	}
	if got := len(g.Active()); got != 0 {
		t.Fatalf("active uploads after quota kill = %d, want 0", got)
	}
	stats := g.Stats()
	if stats.Rejected != 1 || stats.Active != 0 {
		t.Fatalf("stats after quota kill = %+v, want one rejection and no active upload", stats)
	}
	if _, err := g.Write(up.ID, []byte("y")); !errors.Is(err, impl.ErrUnknownUpload) {
		t.Fatalf("writing to a killed upload must fail with ErrUnknownUpload, got %v", err)
	}
}

func TestFinishBuildsDigestReceipt(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, sampleBody(guardBoundary))
	if meta := mustNext(t, g, up.ID); meta.Name != "title" {
		t.Fatalf("first part name = %q, want title", meta.Name)
	}
	if meta := mustNext(t, g, up.ID); meta.Name != "file" {
		t.Fatalf("second part name = %q, want file", meta.Name)
	}
	if _, err := g.NextPart(up.ID); !errors.Is(err, io.EOF) {
		t.Fatalf("expected the closing delimiter to end the body, got %v", err)
	}
	if err := g.CommitPart(up.ID, 0, []byte("hello")); err != nil {
		t.Fatalf("commit first part: %v", err)
	}
	if err := g.CommitPart(up.ID, 1, []byte("payload-bytes")); err != nil {
		t.Fatalf("commit second part: %v", err)
	}
	receipt, err := g.Finish(up.ID)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if receipt.UploadID != up.ID || receipt.Parts != 2 || receipt.Bytes != 18 {
		t.Fatalf("receipt = %+v, want two parts and 18 payload bytes", receipt)
	}
	sum := sha256.Sum256([]byte("hello" + "payload-bytes"))
	if receipt.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt digest = %q, want the sha256 of the concatenated payloads", receipt.Digest)
	}
	if _, ok := g.Inspect(up.ID); ok {
		t.Fatalf("finished upload must leave the live table")
	}
	stats := g.Stats()
	if stats.Accepted != 1 || stats.Bytes != 18 || stats.Active != 0 {
		t.Fatalf("stats after finish = %+v, want one accepted upload and 18 bytes", stats)
	}
}

func TestCommitPartConcurrentSingleWinner(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, sampleBody(guardBoundary))
	if meta := mustNext(t, g, up.ID); meta.Size != 5 {
		t.Fatalf("first part size = %d, want 5", meta.Size)
	}
	if meta := mustNext(t, g, up.ID); meta.Size != 13 {
		t.Fatalf("second part size = %d, want 13", meta.Size)
	}
	if _, err := g.NextPart(up.ID); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of parts, got %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.CommitPart(up.ID, 0, []byte("hello"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, impl.ErrPartConflict):
				conflicts++
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 7 {
		t.Fatalf("concurrent commit of one index: %d wins and %d conflicts, want 1 and 7", wins, conflicts)
	}
	if err := g.CommitPart(up.ID, 1, []byte("payload-bytes")); err != nil {
		t.Fatalf("commit second part: %v", err)
	}
	receipt, err := g.Finish(up.ID)
	if err != nil {
		t.Fatalf("finish after the race: %v", err)
	}
	if receipt.Parts != 2 || receipt.Bytes != 18 {
		t.Fatalf("receipt after the race = %+v, want two parts and 18 bytes", receipt)
	}
}

func TestFinishRejectsIncompleteCommit(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, sampleBody(guardBoundary))
	mustNext(t, g, up.ID)
	mustNext(t, g, up.ID)
	if _, err := g.NextPart(up.ID); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of parts, got %v", err)
	}
	if err := g.CommitPart(up.ID, 5, []byte("hello")); !errors.Is(err, impl.ErrPartIndex) {
		t.Fatalf("commit for an unparsed index must fail with ErrPartIndex, got %v", err)
	}
	if err := g.CommitPart(up.ID, 0, []byte("hello!")); !errors.Is(err, impl.ErrPartSize) {
		t.Fatalf("commit with the wrong payload size must fail with ErrPartSize, got %v", err)
	}
	if err := g.CommitPart(up.ID, 0, []byte("hello")); err != nil {
		t.Fatalf("commit first part: %v", err)
	}
	if _, err := g.Finish(up.ID); !errors.Is(err, impl.ErrBadState) {
		t.Fatalf("finish with an uncommitted part must fail with ErrBadState, got %v", err)
	}
}

func TestFinishRequiresTerminator(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	up := mustBegin(t, g, guardBoundary)
	if _, err := g.Finish(up.ID); !errors.Is(err, impl.ErrBadState) {
		t.Fatalf("finish before any body must fail with ErrBadState, got %v", err)
	}
	mustWrite(t, g, up.ID, sampleBody(guardBoundary))
	mustNext(t, g, up.ID)
	mustNext(t, g, up.ID)
	if _, err := g.Finish(up.ID); !errors.Is(err, impl.ErrBadState) {
		t.Fatalf("finish before the closing delimiter must fail with ErrBadState, got %v", err)
	}
}

func TestAbortReclaimsAndIsIdempotent(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, sampleBody(guardBoundary))
	if got := len(g.Active()); got != 1 {
		t.Fatalf("active uploads before abort = %d, want 1", got)
	}
	if err := g.Abort(up.ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, ok := g.Inspect(up.ID); ok {
		t.Fatalf("aborted upload must leave the table")
	}
	stats := g.Stats()
	if stats.Rejected != 1 || stats.Active != 0 {
		t.Fatalf("stats after abort = %+v, want one rejection and no active upload", stats)
	}
	if err := g.Abort(up.ID); err != nil {
		t.Fatalf("repeated abort must be idempotent, got %v", err)
	}
	if _, err := g.Write(up.ID, []byte("late")); !errors.Is(err, impl.ErrUnknownUpload) {
		t.Fatalf("writing to an aborted upload must fail with ErrUnknownUpload, got %v", err)
	}
	next := mustBegin(t, g, guardBoundary)
	if next.ID == up.ID {
		t.Fatalf("a new upload must not reuse the aborted id")
	}
	if got := len(g.Active()); got != 1 {
		t.Fatalf("active uploads after restarting intake = %d, want 1", got)
	}
}

func TestExpireDropsOnlyIdleUploads(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	idle := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, idle.ID, "--"+guardBoundary+"\r\n")
	time.Sleep(150 * time.Millisecond)
	fresh := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, fresh.ID, "--"+guardBoundary+"\r\n")
	if n := g.Expire(time.Now().Add(100 * time.Millisecond)); n != 1 {
		t.Fatalf("expire dropped %d uploads, want only the idle one", n)
	}
	if _, ok := g.Inspect(idle.ID); ok {
		t.Fatalf("idle upload must be gone after expire")
	}
	if _, ok := g.Inspect(fresh.ID); !ok {
		t.Fatalf("recently written upload must survive expire")
	}
	if got := len(g.Active()); got != 1 {
		t.Fatalf("active uploads after expire = %d, want 1", got)
	}
}

func TestSnapshotRestoreDropsInFlightUploads(t *testing.T) {
	limits := guardLimits()
	g := impl.NewMultipartIntakeGuard(limits)
	done := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, done.ID, sampleBody(guardBoundary))
	mustNext(t, g, done.ID)
	mustNext(t, g, done.ID)
	if _, err := g.NextPart(done.ID); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of parts, got %v", err)
	}
	if err := g.CommitPart(done.ID, 0, []byte("hello")); err != nil {
		t.Fatalf("commit first part: %v", err)
	}
	if err := g.CommitPart(done.ID, 1, []byte("payload-bytes")); err != nil {
		t.Fatalf("commit second part: %v", err)
	}
	receipt, err := g.Finish(done.ID)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	open := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, open.ID, "--"+guardBoundary+"\r\n")
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewMultipartIntakeGuard(limits)
	dropped, err := restored.Restore(data)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("restore dropped %d in flight uploads, want 1", dropped)
	}
	back, ok := restored.ReceiptOf(done.ID)
	if !ok {
		t.Fatalf("restored guard lost the finished receipt")
	}
	if back.Digest != receipt.Digest || back.Bytes != receipt.Bytes {
		t.Fatalf("restored receipt = %+v, want digest %q and %d bytes", back, receipt.Digest, receipt.Bytes)
	}
	if _, ok := restored.Inspect(open.ID); ok {
		t.Fatalf("in flight upload must not survive a restart")
	}
	if _, err := restored.Restore([]byte("{broken-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
}

func TestNestedWhitelistAndDepth(t *testing.T) {
	g := impl.NewMultipartIntakeGuard(guardLimits())
	inner := "--inner\r\n" +
		"Content-Disposition: form-data; name=\"leaf\"\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"nested\r\n" +
		"--inner--\r\n"
	body := "--" + guardBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"bundle\"\r\n" +
		"Content-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		inner +
		"\r\n--" + guardBoundary + "--\r\n"
	up := mustBegin(t, g, guardBoundary)
	mustWrite(t, g, up.ID, body)
	outer := mustNext(t, g, up.ID)
	if !outer.Nested {
		t.Fatalf("multipart/mixed part must be marked nested, got %+v", outer)
	}
	leaf, err := g.NextNested(up.ID)
	if err != nil {
		t.Fatalf("descend into the allowed nested part: %v", err)
	}
	if leaf.Name != "leaf" || leaf.Size != 6 || leaf.Nested {
		t.Fatalf("nested part meta = %+v, want leaf with 6 payload bytes", leaf)
	}
	if _, err := g.NextNested(up.ID); !errors.Is(err, impl.ErrNestedDepth) {
		t.Fatalf("descending past the depth limit must fail with ErrNestedDepth, got %v", err)
	}

	bad := impl.NewMultipartIntakeGuard(guardLimits())
	badBody := "--" + guardBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"bundle\"\r\n" +
		"Content-Type: multipart/related; boundary=inner\r\n\r\n" +
		"--inner--\r\n" +
		"\r\n--" + guardBoundary + "--\r\n"
	upBad := mustBegin(t, bad, guardBoundary)
	mustWrite(t, bad, upBad.ID, badBody)
	if _, err := bad.NextPart(upBad.ID); !errors.Is(err, impl.ErrNestedType) {
		t.Fatalf("non whitelisted nested type must fail with ErrNestedType, got %v", err)
	}
}
