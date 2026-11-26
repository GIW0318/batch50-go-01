package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	impl "gsb/batch50-go-safety-c-07"
)

func guardLimits() impl.GuardLimits {
	return impl.GuardLimits{MaxFiles: 64, MaxFileSize: 4096, MaxFindings: 32, MaxWatch: 2, MaxConcurrent: 4}
}

func newGuard(t *testing.T) *impl.LinkerInjectionGuard {
	t.Helper()
	return impl.NewLinkerInjectionGuard(guardLimits())
}

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return full
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func findKind(items []impl.Finding, kind string) (impl.Finding, bool) {
	for _, item := range items {
		if item.Kind == kind {
			return item, true
		}
	}
	return impl.Finding{}, false
}

func TestScanFlagsNonEmptyPreloadAndIgnoresComments(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "# managed by gsb\n\n")
	g := newGuard(t)
	clean, err := g.Scan(root)
	if err != nil {
		t.Fatalf("scan of a comment only preload: %v", err)
	}
	if clean.Len() != 0 {
		t.Fatalf("a preload file holding only comments must yield no findings, got %+v", clean.Items)
	}
	writeFile(t, root, "etc/ld.so.preload", "# managed by gsb\n/tmp/evil.so\n")
	dirty, err := g.Scan(root)
	if err != nil {
		t.Fatalf("scan of an injected preload: %v", err)
	}
	if dirty.Len() != 1 {
		t.Fatalf("findings = %+v, want exactly the injected preload line", dirty.Items)
	}
	got := dirty.Items[0]
	if got.Kind != impl.KindPreloadFile {
		t.Fatalf("kind = %q, want %q", got.Kind, impl.KindPreloadFile)
	}
	if got.Path != "/etc/ld.so.preload" {
		t.Fatalf("path = %q, want /etc/ld.so.preload", got.Path)
	}
	if !strings.Contains(got.Detail, "/tmp/evil.so") {
		t.Fatalf("detail = %q, want the offending library path", got.Detail)
	}
}

func TestScanRejectsUncontrolledEnvValues(t *testing.T) {
	g := newGuard(t)
	bad := g.CheckEnv([]string{"PATH=/usr/bin", "LD_PRELOAD=/tmp/evil.so"})
	if bad.Len() != 1 {
		t.Fatalf("findings = %+v, want one hijack finding", bad.Items)
	}
	if bad.Items[0].Kind != impl.KindEnvHijack || !strings.Contains(bad.Items[0].Detail, "LD_PRELOAD") {
		t.Fatalf("finding = %+v, want an LD_PRELOAD hijack", bad.Items[0])
	}
	allowed := g.CheckEnv([]string{
		"LD_PRELOAD=" + impl.DefaultPolicy().AllowedPreload,
		"LD_LIBRARY_PATH=" + impl.DefaultPolicy().AllowedLibraryPath,
	})
	if allowed.Len() != 0 {
		t.Fatalf("controlled values must pass, got %+v", allowed.Items)
	}
	audit := g.CheckEnv([]string{"LD_AUDIT=/tmp/a.so"})
	if audit.Len() != 1 || !strings.Contains(audit.Items[0].Detail, "LD_AUDIT") {
		t.Fatalf("an uncontrolled LD_AUDIT must be reported, got %+v", audit.Items)
	}
}

func TestScanRejectsRelativeRunPath(t *testing.T) {
	g := newGuard(t)
	rel := g.CheckRunPath("lib:/usr/lib")
	if rel.Len() != 1 {
		t.Fatalf("findings = %+v, want one relative entry", rel.Items)
	}
	if rel.Items[0].Kind != impl.KindRunPath || !strings.Contains(rel.Items[0].Detail, "lib") {
		t.Fatalf("finding = %+v, want the relative entry lib", rel.Items[0])
	}
	if dot := g.CheckRunPath("."); dot.Len() != 1 {
		t.Fatalf("a bare dot entry must be reported, got %+v", dot.Items)
	}
	if ok := g.CheckRunPath("/usr/lib:/lib"); ok.Len() != 0 {
		t.Fatalf("absolute read only entries must pass, got %+v", ok.Items)
	}
}

func TestScanFlagsWorldWritableLibraryDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "# managed\n")
	dir := filepath.Join(root, "usr", "lib")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir usr/lib: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod usr/lib: %v", err)
	}
	g := newGuard(t)
	found, err := g.Scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	item, ok := findKind(found.Items, impl.KindWritableLibDir)
	if !ok {
		t.Fatalf("findings = %+v, want a writable library directory", found.Items)
	}
	if item.Path != "/usr/lib" {
		t.Fatalf("path = %q, want /usr/lib", item.Path)
	}
}

func TestScanReportsCacheDigestMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/libfoo.so.1", "AAAA")
	writeFile(t, root, "lib/libok.so.2", "GOOD")
	cache := "/lib/libfoo.so.1 " + sha256Hex("BBBB") + "\n" + "/lib/libok.so.2 " + sha256Hex("GOOD") + "\n"
	writeFile(t, root, "etc/ld.so.cache", cache)
	g := newGuard(t)
	found, err := g.Scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if found.Len() != 1 {
		t.Fatalf("findings = %+v, want only the tampered cache entry", found.Items)
	}
	item := found.Items[0]
	if item.Kind != impl.KindCacheMismatch || item.Path != "/etc/ld.so.cache" {
		t.Fatalf("finding = %+v, want a cache mismatch on /etc/ld.so.cache", item)
	}
	if !strings.Contains(item.Detail, "libfoo.so.1") {
		t.Fatalf("detail = %q, want the mismatched library", item.Detail)
	}
}

func TestScanEnforcesFileCountLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "# managed\n")
	for i := 0; i < 6; i++ {
		writeFile(t, root, filepath.ToSlash(filepath.Join("lib", "libx"+string(rune('a'+i))+".so")), "LIB")
	}
	g := impl.NewLinkerInjectionGuard(impl.GuardLimits{MaxFiles: 4, MaxFileSize: 4096, MaxFindings: 32, MaxWatch: 2, MaxConcurrent: 2})
	if _, err := g.Scan(root); !errors.Is(err, impl.ErrScanLimit) {
		t.Fatalf("seven files with a four file cap must fail with ErrScanLimit, got %v", err)
	}
}

func TestScanEnforcesFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", strings.Repeat("/opt/lib/x.so\n", 20))
	g := impl.NewLinkerInjectionGuard(impl.GuardLimits{MaxFiles: 64, MaxFileSize: 32, MaxFindings: 32, MaxWatch: 2, MaxConcurrent: 2})
	if _, err := g.Scan(root); !errors.Is(err, impl.ErrScanLimit) {
		t.Fatalf("an oversized preload file must fail with ErrScanLimit, got %v", err)
	}
}

func TestScanAllMergesFindingsInPathOrder(t *testing.T) {
	first := t.TempDir()
	writeFile(t, first, "etc/ld.so.preload", "/tmp/evil.so\n")
	second := t.TempDir()
	writeFile(t, second, "etc/ld.so.preload", "# managed\n")
	dir := filepath.Join(second, "usr", "lib")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	g := newGuard(t)
	merged, err := g.ScanAll([]string{second, first})
	if err != nil {
		t.Fatalf("scan all: %v", err)
	}
	if merged.Len() != 2 {
		t.Fatalf("findings = %+v, want one finding from each root", merged.Items)
	}
	if merged.Items[0].Kind != impl.KindPreloadFile || merged.Items[1].Kind != impl.KindWritableLibDir {
		t.Fatalf("merged order = %v, want preload first then the writable directory", merged.Kinds())
	}
}

func TestGuardBlocksOnActiveFindings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "/tmp/evil.so\n")
	g := newGuard(t)
	if err := g.AddRoot(root); err != nil {
		t.Fatalf("add root: %v", err)
	}
	if _, err := g.Scan(root); err != nil {
		t.Fatalf("scan: %v", err)
	}
	called := false
	err := g.Guard(impl.ExecContext{
		Bin: "/usr/bin/psql",
		Env: []string{"PATH=/usr/bin"},
		Run: func() error { called = true; return nil },
	})
	if !errors.Is(err, impl.ErrLaunchBlocked) {
		t.Fatalf("an unresolved preload finding must block the launch, got %v", err)
	}
	if called {
		t.Fatalf("a blocked launch must not run the job")
	}
}

func TestGuardRejectsUncontrolledEnvBeforeRun(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "# managed\n")
	g := newGuard(t)
	if err := g.AddRoot(root); err != nil {
		t.Fatalf("add root: %v", err)
	}
	if _, err := g.Scan(root); err != nil {
		t.Fatalf("scan: %v", err)
	}
	called := false
	err := g.Guard(impl.ExecContext{
		Bin: "/usr/bin/psql",
		Env: []string{"LD_PRELOAD=/tmp/evil.so"},
		Run: func() error { called = true; return nil },
	})
	if !errors.Is(err, impl.ErrEnvHijack) {
		t.Fatalf("a hijacked environment must fail with ErrEnvHijack, got %v", err)
	}
	if called {
		t.Fatalf("the environment check must happen before the job runs")
	}
}

func TestGuardDetectsPostLaunchChange(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "# managed\n")
	g := newGuard(t)
	if err := g.AddRoot(root); err != nil {
		t.Fatalf("add root: %v", err)
	}
	if _, err := g.Scan(root); err != nil {
		t.Fatalf("scan: %v", err)
	}
	called := false
	err := g.Guard(impl.ExecContext{
		Bin: "/usr/bin/psql",
		Env: []string{"PATH=/usr/bin"},
		Run: func() error {
			called = true
			writeFile(t, root, "etc/ld.so.preload", "/tmp/evil.so\n")
			return nil
		},
	})
	if !errors.Is(err, impl.ErrSnapshotChanged) {
		t.Fatalf("a key path rewritten during the launch must fail with ErrSnapshotChanged, got %v", err)
	}
	if !called {
		t.Fatalf("the job must run before the post launch recheck")
	}
}

func TestGuardAllowsCleanLaunch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/ld.so.preload", "# managed\n")
	g := newGuard(t)
	if err := g.AddRoot(root); err != nil {
		t.Fatalf("add root: %v", err)
	}
	if _, err := g.Scan(root); err != nil {
		t.Fatalf("scan: %v", err)
	}
	runs := 0
	err := g.Guard(impl.ExecContext{
		Bin: "/usr/bin/psql",
		Env: []string{"PATH=/usr/bin", "LD_LIBRARY_PATH=" + impl.DefaultPolicy().AllowedLibraryPath},
		Run: func() error { runs++; return nil },
	})
	if err != nil {
		t.Fatalf("a clean launch must pass, got %v", err)
	}
	if runs != 1 {
		t.Fatalf("job ran %d times, want exactly once", runs)
	}
}

func TestRepairQuarantinesOriginalAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	original := "# managed by gsb\n/tmp/evil.so\n"
	preload := writeFile(t, root, "etc/ld.so.preload", original)
	g := newGuard(t)
	found, err := g.Scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if found.Len() != 1 {
		t.Fatalf("findings = %+v, want one preload finding", found.Items)
	}
	if err := g.Repair(root, found.Items[0]); err != nil {
		t.Fatalf("repair: %v", err)
	}
	cleaned, err := os.ReadFile(preload)
	if err != nil {
		t.Fatalf("read repaired preload: %v", err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("repaired preload = %q, want an empty file", string(cleaned))
	}
	quarantine := filepath.Join(root, "var", "lib", "gsb-quarantine", "ld.so.preload")
	kept, err := os.ReadFile(quarantine)
	if err != nil {
		t.Fatalf("quarantine copy missing: %v", err)
	}
	if string(kept) != original {
		t.Fatalf("quarantine copy = %q, want the original content", string(kept))
	}
	if err := g.Repair(root, found.Items[0]); err != nil {
		t.Fatalf("second repair must be idempotent, got %v", err)
	}
	again, err := os.ReadFile(quarantine)
	if err != nil {
		t.Fatalf("quarantine copy vanished after the second repair: %v", err)
	}
	if string(again) != original {
		t.Fatalf("second repair overwrote the evidence with %q", string(again))
	}
	entries, err := os.ReadDir(filepath.Dir(quarantine))
	if err != nil {
		t.Fatalf("read quarantine dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine dir holds %d files, want a single evidence copy", len(entries))
	}
	log := g.Audit()
	if len(log) == 0 || log[len(log)-1].Phase != impl.PhaseRepaired {
		t.Fatalf("audit log = %+v, want a repaired record at the end", log)
	}
}

func TestRepairUnknownFindingFails(t *testing.T) {
	root := t.TempDir()
	g := newGuard(t)
	err := g.Repair(root, impl.Finding{Kind: impl.KindPreloadFile, Path: "/etc/ld.so.preload"})
	if !errors.Is(err, impl.ErrFindingGone) {
		t.Fatalf("repairing a missing preload file must fail with ErrFindingGone, got %v", err)
	}
}

func TestWatchEnforcesPathCap(t *testing.T) {
	g := newGuard(t)
	if err := g.Watch([]string{"opt/b", "opt/a"}); err != nil {
		t.Fatalf("watch two paths with a cap of two: %v", err)
	}
	watched := g.Watched()
	if len(watched) != 2 || watched[0] != "opt/a" || watched[1] != "opt/b" {
		t.Fatalf("watched = %v, want the two registered paths in ascending order", watched)
	}
	if err := g.Watch([]string{"opt/c"}); !errors.Is(err, impl.ErrWatchLimit) {
		t.Fatalf("a third path must fail with ErrWatchLimit, got %v", err)
	}
	if got := g.Watched(); len(got) != 2 {
		t.Fatalf("a rejected watch must not grow the table, got %v", got)
	}
	if err := g.Watch(nil); !errors.Is(err, impl.ErrNoWatch) {
		t.Fatalf("an empty watch request must fail with ErrNoWatch, got %v", err)
	}
}
