package task

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-30"
)

func testLimits() impl.Limits {
	return impl.Limits{
		MaxBytes:  4096,
		MaxLayers: 3,
		MaxFiles:  16,
		CacheTTL:  time.Minute,
	}
}

func fileRef(id, path, content string) impl.FileRef {
	return impl.FileRef{ID: id, Path: path, Content: content}
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func dangerousBody() string {
	return "runCode($a); postHttp($ch); writeFile('/tmp/x','y');"
}

func TestScanRejectsBlankFileRef(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	if _, err := c.Scan(fileRef(" ", "/upload/a.inc", "runCode(1);")); !errors.Is(err, impl.ErrBadFile) {
		t.Fatalf("blank file id must fail with ErrBadFile, got %v", err)
	}
	if _, err := c.Scan(fileRef("f-1", "", "runCode(1);")); !errors.Is(err, impl.ErrBadFile) {
		t.Fatalf("blank path must fail with ErrBadFile, got %v", err)
	}
	if _, ok := c.VerdictFor("f-1"); ok {
		t.Fatalf("a rejected file must not get a verdict")
	}
}

func TestScanClassifiesMaliciousCombination(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	body := dangerousBody()
	v, err := c.Scan(fileRef("f-2", "/upload/avatar.inc", body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.State != impl.StateMalicious {
		t.Fatalf("state = %q, want %q", v.State, impl.StateMalicious)
	}
	if v.Score != 80 {
		t.Fatalf("score = %d, want exec 40 + net 25 + file 15 = 80", v.Score)
	}
	want := []string{impl.FeatureExec, impl.FeatureFileWrite, impl.FeatureNet}
	if len(v.Features) != len(want) {
		t.Fatalf("features = %v, want %v", v.Features, want)
	}
	for i, name := range want {
		if v.Features[i] != name {
			t.Fatalf("features = %v, want the sorted list %v", v.Features, want)
		}
	}
	if v.Hash != contentHash(body) {
		t.Fatalf("hash = %q, want the sha256 of the raw content", v.Hash)
	}
	if v.Cached {
		t.Fatalf("the first scan must not report a cached verdict")
	}
}

func TestScanClassifiesCleanFile(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	v, err := c.Scan(fileRef("f-3", "/upload/hello.inc", "print 'hello';"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.State != impl.StateBenign {
		t.Fatalf("state = %q, want %q", v.State, impl.StateBenign)
	}
	if v.Score != 0 {
		t.Fatalf("score = %d, want 0", v.Score)
	}
	if len(v.Features) != 0 {
		t.Fatalf("a clean file must not report capabilities, got %v", v.Features)
	}
	if v.Layers != 0 {
		t.Fatalf("layers = %d, want 0 for plain text", v.Layers)
	}
}

func TestScanCountsCapabilityOnce(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	body := "runCode($a); runCode($b); runCode($c); postHttp($ch);"
	v, err := c.Scan(fileRef("f-4", "/upload/dup.inc", body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.Score != 65 {
		t.Fatalf("score = %d, want exec counted once plus net = 65", v.Score)
	}
	if v.State != impl.StateSuspicious {
		t.Fatalf("state = %q, want %q", v.State, impl.StateSuspicious)
	}
}

func TestScanRejectsOversizeFile(t *testing.T) {
	limits := testLimits()
	limits.MaxBytes = 32
	c := impl.NewWebshellClassifier(limits)
	big := strings.Repeat("A", 64)
	if _, err := c.Scan(fileRef("f-5", "/upload/big.inc", big)); !errors.Is(err, impl.ErrTooLarge) {
		t.Fatalf("a file over the size cap must fail with ErrTooLarge, got %v", err)
	}
	if _, ok := c.VerdictFor("f-5"); ok {
		t.Fatalf("an oversized file must not get a verdict")
	}
}

func TestScanScoresDecodedContent(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	inner := "runCode($_POST[1]);"
	wrapped := "b64:" + base64.StdEncoding.EncodeToString([]byte(inner))
	v, err := c.Scan(fileRef("f-6", "/upload/inner.inc", wrapped))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.Layers != 1 {
		t.Fatalf("layers = %d, want 1", v.Layers)
	}
	if v.Score != 40 {
		t.Fatalf("score = %d, want the decoded exec capability to score 40", v.Score)
	}
	if v.State != impl.StateSuspicious {
		t.Fatalf("state = %q, want %q", v.State, impl.StateSuspicious)
	}
	dec, err := c.Decode(fileRef("f-6", "/upload/inner.inc", wrapped), 3)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Content != inner {
		t.Fatalf("decoded content = %q, want %q", dec.Content, inner)
	}
	if dec.Truncated {
		t.Fatalf("one layer under a budget of three must not be truncated")
	}
}

func TestDecodeStopsAtLayerLimit(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	inner := "runCode(1);"
	once := base64.StdEncoding.EncodeToString([]byte(inner))
	twice := "b64:" + base64.StdEncoding.EncodeToString([]byte("b64:"+once))
	dec, err := c.Decode(fileRef("f-7", "/upload/nested.inc", twice), 1)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Layers != 1 {
		t.Fatalf("layers = %d, want the budget 1", dec.Layers)
	}
	if !dec.Truncated {
		t.Fatalf("a payload that still carries a coding prefix after the budget must be truncated")
	}
}

func TestDecodeRejectsMalformedPayload(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	if _, err := c.Decode(fileRef("f-8", "/upload/bad.inc", "b64:%%%not-base64%%%"), 3); !errors.Is(err, impl.ErrBadEncoding) {
		t.Fatalf("a malformed base64 payload must fail with ErrBadEncoding, got %v", err)
	}
	if _, err := c.Decode(fileRef("f-8", "/upload/bad.inc", "hex:zzzz"), 3); !errors.Is(err, impl.ErrBadEncoding) {
		t.Fatalf("a malformed hex payload must fail with ErrBadEncoding, got %v", err)
	}
}

func TestExplainBeforeScanFails(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	if _, err := c.Explain("f-404"); !errors.Is(err, impl.ErrNoVerdict) {
		t.Fatalf("explaining a file without a verdict must fail with ErrNoVerdict, got %v", err)
	}
}

func TestExplainListsFeatures(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	body := dangerousBody()
	v, err := c.Scan(fileRef("f-9", "/upload/avatar.inc", body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f, err := c.Explain("f-9")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if f.FileID != "f-9" || f.Hash != v.Hash {
		t.Fatalf("explained file = %q hash %q, want f-9 and %q", f.FileID, f.Hash, v.Hash)
	}
	if f.Score != v.Score || f.State != v.State {
		t.Fatalf("explained score/state = %d/%q, want %d/%q", f.Score, f.State, v.Score, v.State)
	}
	if len(f.Features) != 3 {
		t.Fatalf("explained features = %v, want three capabilities", f.Features)
	}
}

func TestWhitelistSuppressesVerdict(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	body := "runCode($a); postHttp($ch);"
	if err := c.Whitelist(strings.ToUpper(contentHash(body))+"  ", time.Hour); err != nil {
		t.Fatalf("whitelist: %v", err)
	}
	if got := c.Hashes(); len(got) != 1 || got[0] != contentHash(body) {
		t.Fatalf("whitelisted hashes = %v, want the lower case hash %q", got, contentHash(body))
	}
	v, err := c.Scan(fileRef("f-10", "/upload/known.inc", body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.State != impl.StateWhitelisted {
		t.Fatalf("state = %q, want %q", v.State, impl.StateWhitelisted)
	}
	if v.Score != 0 {
		t.Fatalf("a whitelisted file must score 0, got %d", v.Score)
	}
	if err := c.Whitelist("", time.Hour); !errors.Is(err, impl.ErrBadHash) {
		t.Fatalf("blank hash must fail with ErrBadHash, got %v", err)
	}
}

func TestRescanClearsCachedFlag(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	file := fileRef("f-11", "/upload/hello.inc", "print 'hello';")
	first, err := c.Scan(file)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if first.Cached {
		t.Fatalf("the first scan must not be cached")
	}
	second, err := c.Scan(file)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if !second.Cached {
		t.Fatalf("a repeated scan of the same content must be served from the verdict cache")
	}
	if err := c.Rescan("f-11"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	stored, ok := c.VerdictFor("f-11")
	if !ok {
		t.Fatalf("rescan must keep the verdict readable")
	}
	if stored.Cached {
		t.Fatalf("a forced rescan must not leave the verdict marked as cached")
	}
	if err := c.Rescan("f-404"); !errors.Is(err, impl.ErrNoVerdict) {
		t.Fatalf("rescanning an unknown file must fail with ErrNoVerdict, got %v", err)
	}
}

func TestPruneDropsExpiredVerdicts(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	if _, err := c.Scan(fileRef("f-12", "/upload/x.inc", "print 1;")); err != nil {
		t.Fatalf("scan: %v", err)
	}
	removed, err := c.Prune(time.Now().Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("prune removed %d entries, want 1 stale verdict", removed)
	}
	if _, ok := c.VerdictFor("f-12"); ok {
		t.Fatalf("an expired verdict must be gone")
	}
	again, err := c.Scan(fileRef("f-12", "/upload/x.inc", "print 1;"))
	if err != nil {
		t.Fatalf("scan after prune: %v", err)
	}
	if again.Cached {
		t.Fatalf("a scan after prune must recompute, not reuse the stale verdict")
	}
}

func TestSnapshotRestoreKeepsWhitelistAndVerdicts(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	body := "runCode($a);"
	if _, err := c.Scan(fileRef("f-13", "/upload/one.inc", body)); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := c.Whitelist(contentHash("print 'trusted';"), time.Hour); err != nil {
		t.Fatalf("whitelist: %v", err)
	}
	data, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewWebshellClassifier(testLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	v, ok := restored.VerdictFor("f-13")
	if !ok {
		t.Fatalf("restored classifier lost the verdict of f-13")
	}
	if v.Score != 40 || v.State != impl.StateSuspicious {
		t.Fatalf("restored verdict = %d/%q, want 40/%q", v.Score, v.State, impl.StateSuspicious)
	}
	if got := restored.Hashes(); len(got) != 1 || got[0] != contentHash("print 'trusted';") {
		t.Fatalf("restored whitelist = %v, want one hash", got)
	}
}

func TestRestoreRejectsCorruptPayload(t *testing.T) {
	c := impl.NewWebshellClassifier(testLimits())
	if _, err := c.Scan(fileRef("f-14", "/upload/x.inc", "runCode($_GET[1]);")); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := c.Restore([]byte("###not-a-snapshot###")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := c.VerdictFor("f-14"); !ok {
		t.Fatalf("a rejected restore must not touch live verdicts")
	}
}
