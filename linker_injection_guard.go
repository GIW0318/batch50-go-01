// Package task inspects the dynamic linker injection surface of an image,
// gates job launches on the scan snapshot, and repairs what it can while
// keeping the original file for forensics.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Finding kinds reported by a scan.
const (
	KindPreloadFile    = "preload_file"
	KindEnvHijack      = "env_hijack"
	KindRunPath        = "runpath"
	KindWritableLibDir = "writable_lib_dir"
	KindCacheMismatch  = "cache_mismatch"
)

// Loader environment variables that the guard controls.
const (
	VarPreload     = "LD_PRELOAD"
	VarAudit       = "LD_AUDIT"
	VarLibraryPath = "LD_LIBRARY_PATH"
)

// Audit phases written by the guard.
const (
	PhaseScanned  = "scanned"
	PhaseBlocked  = "blocked"
	PhaseLaunched = "launched"
	PhaseRepaired = "repaired"
)

// Errors returned by the guard.
var (
	ErrScanLimit       = errors.New("scan exceeded the configured limits")
	ErrNoRoot          = errors.New("scan root does not exist")
	ErrEnvHijack       = errors.New("loader environment holds an uncontrolled value")
	ErrLaunchBlocked   = errors.New("launch blocked by an unresolved linker finding")
	ErrSnapshotChanged = errors.New("scan snapshot changed during the launch")
	ErrFindingGone     = errors.New("finding is not present on the scanned tree")
	ErrNotRepairable   = errors.New("finding cannot be repaired in place")
	ErrWatchLimit      = errors.New("watch path limit reached")
	ErrNoWatch         = errors.New("at least one watch path is required")
	ErrBadPolicy       = errors.New("policy value is empty")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

const (
	imagePreloadPath = "/etc/ld.so.preload"
	imageCachePath   = "/etc/ld.so.cache"
	quarantineRelDir = "var/lib/gsb-quarantine"
	quarantineName   = "ld.so.preload"
	overlayUpperDir  = "var/lib/gsb-overlay/upper"
	highRiskSeverity = 80

	defaultMaxFiles      = 16384
	defaultMaxFileSize   = 64 << 20
	defaultMaxFindings   = 4096
	defaultMaxWatch      = 256
	defaultMaxConcurrent = 8
)

var emptyDigest = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// GuardLimits bounds one scan and the watcher table.
type GuardLimits struct {
	MaxFiles      int
	MaxFileSize   int64
	MaxFindings   int
	MaxWatch      int
	MaxConcurrent int
}

// Policy holds the only loader values a job may run with.
type Policy struct {
	AllowedPreload     string
	AllowedAudit       string
	AllowedLibraryPath string
}

// Finding is one suspicious spot on the scanned tree.
type Finding struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Detail   string `json:"detail"`
	Severity int    `json:"severity"`
	Digest   string `json:"digest"`
}

// Findings is the ordered result of one scan.
type Findings struct {
	Items []Finding `json:"items"`
}

// Len reports how many findings the result holds.
func (f Findings) Len() int { return len(f.Items) }

// Kinds returns the finding kinds in the order they were recorded.
func (f Findings) Kinds() []string {
	kinds := make([]string, 0, len(f.Items))
	for _, item := range f.Items {
		kinds = append(kinds, item.Kind)
	}
	return kinds
}

// ExecContext is one job launch that the guard has to clear.
type ExecContext struct {
	Bin  string
	Args []string
	Env  []string
	Cwd  string
	Run  func() error
}

// AuditRecord is one line of the guard log.
type AuditRecord struct {
	Seq    int64  `json:"seq"`
	Phase  string `json:"phase"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

type pathDigest struct {
	Exists bool   `json:"exists"`
	Digest string `json:"digest"`
}

type rootSnapshot struct {
	Root     string                `json:"root"`
	Findings []Finding             `json:"findings"`
	Paths    map[string]pathDigest `json:"paths"`
}

type guardSnapshotData struct {
	Version   int                     `json:"version"`
	Limits    GuardLimits             `json:"limits"`
	Policy    Policy                  `json:"policy"`
	Roots     []string                `json:"roots"`
	Snapshots map[string]rootSnapshot `json:"snapshots"`
	Last      []Finding               `json:"last"`
	Blocked   map[string]string       `json:"blocked"`
	Watched   []string                `json:"watched"`
	Audit     []AuditRecord           `json:"audit"`
	Seq       int64                   `json:"seq"`
}

// LinkerInjectionGuard owns the scan snapshots, the blocklist and the watcher.
type LinkerInjectionGuard struct {
	limits    GuardLimits
	policy    Policy
	mu        sync.RWMutex
	lifeMu    sync.RWMutex
	roots     map[string]struct{}
	snapshots map[string]*rootSnapshot
	last      []Finding
	blocked   map[string]string
	watched   map[string]struct{}
	audit     []AuditRecord
	seq       int64
	scanSlots chan struct{}
}

// DefaultPolicy returns the loader values that images are allowed to carry.
func DefaultPolicy() Policy {
	return Policy{
		AllowedPreload:     "/usr/lib/gsb/libtrace.so",
		AllowedAudit:       "",
		AllowedLibraryPath: "/usr/lib/gsb",
	}
}

func normalizeLimits(limits GuardLimits) GuardLimits {
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = defaultMaxFiles
	}
	if limits.MaxFileSize <= 0 {
		limits.MaxFileSize = defaultMaxFileSize
	}
	if limits.MaxFindings <= 0 {
		limits.MaxFindings = defaultMaxFindings
	}
	if limits.MaxWatch <= 0 {
		limits.MaxWatch = defaultMaxWatch
	}
	if limits.MaxConcurrent <= 0 {
		limits.MaxConcurrent = defaultMaxConcurrent
	}
	return limits
}

// NewLinkerInjectionGuard builds a guard with the given caps.
func NewLinkerInjectionGuard(limits GuardLimits) *LinkerInjectionGuard {
	limits = normalizeLimits(limits)
	return &LinkerInjectionGuard{
		limits:    limits,
		policy:    DefaultPolicy(),
		roots:     make(map[string]struct{}),
		snapshots: make(map[string]*rootSnapshot),
		blocked:   make(map[string]string),
		watched:   make(map[string]struct{}),
		scanSlots: make(chan struct{}, limits.MaxConcurrent),
	}
}

// SetPolicy replaces the controlled loader values.
func (g *LinkerInjectionGuard) SetPolicy(p Policy) error {
	if strings.TrimSpace(p.AllowedPreload) == "" || strings.TrimSpace(p.AllowedLibraryPath) == "" {
		return fmt.Errorf("%w: preload and library path must be configured", ErrBadPolicy)
	}
	g.mu.Lock()
	g.policy = p
	g.mu.Unlock()
	return nil
}

func (g *LinkerInjectionGuard) getPolicy() Policy {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.policy
}

func (g *LinkerInjectionGuard) getLimits() GuardLimits {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.limits
}

func (g *LinkerInjectionGuard) appendAuditLocked(phase, target, detail string) AuditRecord {
	g.seq++
	record := AuditRecord{Seq: g.seq, Phase: phase, Path: target, Detail: detail}
	g.audit = append(g.audit, record)
	return record
}

func (g *LinkerInjectionGuard) addAudit(phase, target, detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.appendAuditLocked(phase, target, detail)
}

// AddRoot registers a root filesystem that later launches are checked against.
func (g *LinkerInjectionGuard) AddRoot(root string) error {
	realRoot, err := existingRoot(root)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.roots[realRoot] = struct{}{}
	g.mu.Unlock()
	return nil
}

func existingRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("%w: empty root path", ErrNoRoot)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoRoot, err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNoRoot, root)
		}
		return "", err
	}
	info, err := os.Stat(realRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNoRoot, root)
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrNoRoot, root)
	}
	return filepath.Clean(realRoot), nil
}

func imagePathOf(root, hostPath string) (string, error) {
	rel, err := filepath.Rel(root, hostPath)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "/", nil
	}
	return path.Clean("/" + rel), nil
}

func hostPathFor(root, imagePath string) string {
	cleanImage := path.Clean("/" + filepath.ToSlash(imagePath))
	rel := strings.TrimPrefix(cleanImage, "/")
	if rel == "" {
		return filepath.Clean(root)
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

func overlayPathFor(root, imagePath string) string {
	cleanImage := path.Clean("/" + filepath.ToSlash(imagePath))
	rel := strings.TrimPrefix(cleanImage, "/")
	return filepath.Join(root, filepath.FromSlash(path.Join(overlayUpperDir, rel)))
}

func effectiveHostPath(root, imagePath string) string {
	upper := overlayPathFor(root, imagePath)
	if info, err := os.Stat(upper); err == nil && !info.IsDir() {
		return upper
	}
	return hostPathFor(root, imagePath)
}

func readFileLimited(hostPath string, maxBytes int64) ([]byte, string, error) {
	file, err := os.Open(hostPath)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	if maxBytes <= 0 {
		maxBytes = defaultMaxFileSize
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("%w: %s is larger than %d bytes", ErrScanLimit, hostPath, maxBytes)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func digestHostPath(hostPath string) (pathDigest, error) {
	file, err := os.Open(hostPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return pathDigest{Exists: false}, nil
		}
		return pathDigest{}, err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return pathDigest{}, err
	}
	return pathDigest{Exists: true, Digest: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func preloadEntries(data []byte) []string {
	entries := make([]string, 0)
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	return entries
}

type scanJob struct {
	root            string
	limits          GuardLimits
	items           []Finding
	paths           map[string]pathDigest
	listedLibs      map[string]struct{}
	reportedLibDirs map[string]struct{}
	cachePresent    bool
	fileCount       int
}

// Scan walks one root filesystem and reports linker injection findings.
func (g *LinkerInjectionGuard) Scan(rootfs string) (Findings, error) {
	root, err := existingRoot(rootfs)
	if err != nil {
		return Findings{}, err
	}

	limit := cap(g.scanSlots)
	select {
	case g.scanSlots <- struct{}{}:
		defer func() { <-g.scanSlots }()
	default:
		return Findings{}, fmt.Errorf("%w: at most %d concurrent scans are allowed", ErrScanLimit, limit)
	}

	g.lifeMu.RLock()
	defer g.lifeMu.RUnlock()
	return g.scanAndCommit(root)
}

func (g *LinkerInjectionGuard) scanAndCommit(root string) (Findings, error) {
	limits := g.getLimits()
	job := &scanJob{
		root:            root,
		limits:          limits,
		items:           make([]Finding, 0),
		paths:           make(map[string]pathDigest),
		listedLibs:      make(map[string]struct{}),
		reportedLibDirs: make(map[string]struct{}),
	}

	if err := job.scanPreload(); err != nil {
		return Findings{}, err
	}
	if err := job.scanCache(); err != nil {
		return Findings{}, err
	}
	if err := filepath.WalkDir(root, job.walk); err != nil {
		return Findings{}, err
	}
	if limits.MaxFindings > 0 && len(job.items) > limits.MaxFindings {
		return Findings{}, fmt.Errorf("%w: more than MaxFindings=%d", ErrScanLimit, limits.MaxFindings)
	}

	items := sortedFindings(job.items)
	snapshot := &rootSnapshot{
		Root:     root,
		Findings: cloneFindings(items),
		Paths:    clonePathDigests(job.paths),
	}

	g.mu.Lock()
	g.roots[root] = struct{}{}
	g.snapshots[root] = snapshot
	g.last = cloneFindings(items)
	g.appendAuditLocked(PhaseScanned, root, fmt.Sprintf("%d findings", len(items)))
	g.mu.Unlock()

	return Findings{Items: items}, nil
}

func cloneFindings(items []Finding) []Finding {
	if items == nil {
		return nil
	}
	out := make([]Finding, len(items))
	copy(out, items)
	return out
}

func clonePathDigests(in map[string]pathDigest) map[string]pathDigest {
	out := make(map[string]pathDigest, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (j *scanJob) scanPreload() error {
	hostPath := effectiveHostPath(j.root, imagePreloadPath)
	data, digest, err := readFileLimited(hostPath, j.limits.MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		j.paths[imagePreloadPath] = pathDigest{Exists: false}
		return nil
	}
	if err != nil {
		return err
	}

	j.paths[imagePreloadPath] = pathDigest{Exists: true, Digest: digest}
	for _, entry := range preloadEntries(data) {
		j.items = append(j.items, Finding{
			Kind:     KindPreloadFile,
			Path:     imagePreloadPath,
			Detail:   "uncontrolled library entry in " + imagePreloadPath + ": " + entry,
			Severity: highRiskSeverity + 20,
			Digest:   digest,
		})
	}
	return nil
}

func (j *scanJob) scanCache() error {
	hostPath := effectiveHostPath(j.root, imageCachePath)
	data, digest, err := readFileLimited(hostPath, j.limits.MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		j.paths[imageCachePath] = pathDigest{Exists: false}
		return nil
	}
	if err != nil {
		return err
	}

	j.cachePresent = true
	j.paths[imageCachePath] = pathDigest{Exists: true, Digest: digest}
	for lineNumber, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			j.addCacheMismatch("", fmt.Sprintf("malformed cache line %d: %q", lineNumber+1, line), "")
			continue
		}

		libraryImagePath := path.Clean(fields[0])
		expectedDigest := strings.ToLower(strings.TrimSpace(fields[1]))
		j.listedLibs[libraryImagePath] = struct{}{}
		if !path.IsAbs(filepath.ToSlash(fields[0])) {
			j.addCacheMismatch(libraryImagePath, "cache entry must be an absolute root path: "+fields[0], "")
			continue
		}

		libraryHostPath := hostPathFor(j.root, libraryImagePath)
		_, actualDigest, err := readFileLimited(libraryHostPath, j.limits.MaxFileSize)
		if errors.Is(err, fs.ErrNotExist) {
			j.addCacheMismatch(libraryImagePath, "cached library is missing: "+libraryImagePath, expectedDigest)
			continue
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(expectedDigest, actualDigest) {
			j.addCacheMismatch(
				libraryImagePath,
				fmt.Sprintf("cache digest mismatch for %s: expected %s, actual %s", libraryImagePath, expectedDigest, actualDigest),
				actualDigest,
			)
		}
	}
	return nil
}

func (j *scanJob) addCacheMismatch(libraryPath, detail, digest string) {
	j.items = append(j.items, Finding{
		Kind:     KindCacheMismatch,
		Path:     imageCachePath,
		Detail:   detail,
		Severity: highRiskSeverity + 5,
		Digest:   digest,
	})
}

func isLibraryFileName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".so") {
		return true
	}
	index := strings.Index(lower, ".so.")
	return index >= 0 && index+4 < len(lower) && lower[index+4] >= '0' && lower[index+4] <= '9'
}

func isNamedLibraryDirectory(imagePath, name string) bool {
	switch imagePath {
	case "/lib", "/lib64", "/usr/lib", "/usr/lib64", "/usr/local/lib", "/usr/local/lib64":
		return true
	}
	lowerName := strings.ToLower(name)
	if (lowerName == "lib" || lowerName == "lib64") && path.Dir(imagePath) != "/var" {
		return true
	}
	return false
}

func (j *scanJob) addWritableLibDir(imagePath string) {
	if _, seen := j.reportedLibDirs[imagePath]; seen {
		return
	}
	j.reportedLibDirs[imagePath] = struct{}{}
	j.items = append(j.items, Finding{
		Kind:     KindWritableLibDir,
		Path:     imagePath,
		Detail:   "library directory is writable by users other than its owner",
		Severity: highRiskSeverity + 10,
	})
}

func (j *scanJob) walk(hostPath string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	imagePath, err := imagePathOf(j.root, hostPath)
	if err != nil {
		return err
	}
	if imagePath == "/proc" || strings.HasPrefix(imagePath, "/proc/") {
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}

	if entry.IsDir() {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if mode.Perm()&0o002 != 0 && isNamedLibraryDirectory(imagePath, entry.Name()) {
			j.addWritableLibDir(imagePath)
		}
		return nil
	}

	if !entry.Type().IsRegular() {
		return nil
	}

	j.fileCount++
	if j.limits.MaxFiles > 0 && j.fileCount > j.limits.MaxFiles {
		return fmt.Errorf("%w: traversed more than MaxFiles=%d ordinary files", ErrScanLimit, j.limits.MaxFiles)
	}

	info, err := entry.Info()
	if err != nil {
		return err
	}
	if j.limits.MaxFileSize > 0 && info.Size() > j.limits.MaxFileSize {
		return fmt.Errorf("%w: %s size %d exceeds MaxFileSize=%d", ErrScanLimit, imagePath, info.Size(), j.limits.MaxFileSize)
	}

	if isLibraryFileName(entry.Name()) {
		parentImagePath := path.Dir(imagePath)
		if parentInfo, statErr := os.Stat(filepath.Dir(hostPath)); statErr == nil {
			if parentInfo.Mode().Perm()&0o002 != 0 {
				j.addWritableLibDir(parentImagePath)
			}
		}
		if j.cachePresent {
			if _, listed := j.listedLibs[path.Clean(imagePath)]; !listed {
				j.addCacheMismatch(
					imagePath,
					"library present in a library directory but absent from "+imageCachePath+": "+imagePath,
					"",
				)
			}
		}
	}

	return nil
}

// ScanAll scans several roots with bounded concurrency and merges the results.
func (g *LinkerInjectionGuard) ScanAll(roots []string) (Findings, error) {
	if len(roots) == 0 {
		return Findings{Items: []Finding{}}, nil
	}

	realRoots := make([]string, len(roots))
	for index, root := range roots {
		realRoot, err := existingRoot(root)
		if err != nil {
			return Findings{}, err
		}
		realRoots[index] = realRoot
	}

	results := make([][]Finding, len(realRoots))
	scanErrs := make([]error, len(realRoots))
	var wg sync.WaitGroup

	for index, root := range realRoots {
		g.scanSlots <- struct{}{}
		wg.Add(1)
		go func(index int, root string) {
			defer wg.Done()
			defer func() { <-g.scanSlots }()

			g.lifeMu.RLock()
			defer g.lifeMu.RUnlock()
			result, err := g.scanAndCommit(root)
			if err == nil {
				results[index] = result.Items
			}
			scanErrs[index] = err
		}(index, root)
	}
	wg.Wait()

	for _, err := range scanErrs {
		if err != nil {
			return Findings{}, err
		}
	}

	merged := make([]Finding, 0)
	for _, items := range results {
		merged = append(merged, items...)
	}
	merged = sortedFindings(merged)

	g.mu.Lock()
	g.last = cloneFindings(merged)
	g.mu.Unlock()

	return Findings{Items: merged}, nil
}

type digestTarget struct {
	label string
	host  string
	want  *pathDigest
}

// Guard clears a job launch against the current scan snapshots.
func (g *LinkerInjectionGuard) Guard(ctx ExecContext) error {
	g.lifeMu.RLock()
	defer g.lifeMu.RUnlock()

	policy := g.getPolicy()
	if bad := checkEnvWithPolicy(ctx.Env, policy); len(bad.Items) > 0 {
		names := make([]string, 0, len(bad.Items))
		for _, item := range bad.Items {
			names = append(names, item.Detail)
		}
		g.addAudit(PhaseBlocked, ctx.Bin, strings.Join(names, "; "))
		return fmt.Errorf("%w: %s", ErrEnvHijack, strings.Join(g.envVariableNames(bad.Items), ", "))
	}

	if reason, blocked := g.blockReason(ctx.Bin); blocked {
		g.addAudit(PhaseBlocked, ctx.Bin, reason)
		return fmt.Errorf("%w: binary %q: %s", ErrLaunchBlocked, ctx.Bin, reason)
	}

	g.mu.RLock()
	snapshots := make([]*rootSnapshot, 0, len(g.snapshots))
	rootSet := make(map[string]struct{}, len(g.roots)+len(g.snapshots))
	for root := range g.roots {
		rootSet[root] = struct{}{}
	}
	for root, snapshot := range g.snapshots {
		rootSet[root] = struct{}{}
		copySnapshot := &rootSnapshot{
			Root:     snapshot.Root,
			Findings: cloneFindings(snapshot.Findings),
			Paths:    clonePathDigests(snapshot.Paths),
		}
		snapshots = append(snapshots, copySnapshot)
	}
	watched := make([]string, 0, len(g.watched))
	for path := range g.watched {
		watched = append(watched, path)
	}
	cwd := ctx.Cwd
	g.mu.RUnlock()

	for _, snapshot := range snapshots {
		for _, item := range snapshot.Findings {
			if item.Severity >= highRiskSeverity {
				detail := item.Kind + " " + item.Path
				g.addAudit(PhaseBlocked, ctx.Bin, detail)
				return fmt.Errorf("%w: unresolved high-risk finding %s at %s", ErrLaunchBlocked, item.Kind, item.Path)
			}
		}
	}

	targets := make([]digestTarget, 0)
	for _, snapshot := range snapshots {
		for imagePath, wanted := range snapshot.Paths {
			wantedCopy := wanted
			targets = append(targets, digestTarget{
				label: snapshot.Root + ":" + imagePath,
				host:  effectiveHostPath(snapshot.Root, imagePath),
				want:  &wantedCopy,
			})
		}
	}

	roots := make([]string, 0, len(rootSet))
	for root := range rootSet {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	sort.Strings(watched)
	for _, watchedPath := range watched {
		targets = append(targets, digestTarget{label: watchedPath, host: watchedPath})
		if cwd != "" && !filepath.IsAbs(watchedPath) {
			targets = append(targets, digestTarget{
				label: "cwd:" + watchedPath,
				host:  filepath.Join(cwd, watchedPath),
			})
		}
		for _, root := range roots {
			imagePath := watchedPath
			if !path.IsAbs(filepath.ToSlash(imagePath)) {
				imagePath = "/" + imagePath
			}
			targets = append(targets, digestTarget{
				label: root + ":" + watchedPath,
				host:  hostPathFor(root, imagePath),
			})
		}
	}

	preLaunch := make(map[string]pathDigest, len(targets))
	labels := make(map[string]string, len(targets))
	for _, target := range targets {
		current, err := digestHostPath(target.host)
		if err != nil {
			g.addAudit(PhaseBlocked, ctx.Bin, "cannot read snapshot path "+target.label)
			return fmt.Errorf("%w: cannot read %s: %v", ErrSnapshotChanged, target.label, err)
		}
		if target.want != nil && current != *target.want {
			return fmt.Errorf("%w: %s differs from the scan snapshot before launch", ErrSnapshotChanged, target.label)
		}
		preLaunch[target.host] = current
		labels[target.host] = target.label
	}

	var runErr error
	if ctx.Run != nil {
		runErr = ctx.Run()
	}

	var changed error
	for hostPath, wanted := range preLaunch {
		current, err := digestHostPath(hostPath)
		if err != nil {
			changed = errors.Join(changed, fmt.Errorf("%w: cannot reread %s: %v", ErrSnapshotChanged, labels[hostPath], err))
			continue
		}
		if current != wanted {
			changed = errors.Join(changed, fmt.Errorf("%w: %s was rewritten during launch", ErrSnapshotChanged, labels[hostPath]))
		}
	}
	if changed != nil {
		g.addAudit(PhaseBlocked, ctx.Bin, changed.Error())
		return errors.Join(runErr, changed)
	}

	g.addAudit(PhaseLaunched, ctx.Bin, "")
	return runErr
}

func (g *LinkerInjectionGuard) envVariableNames(items []Finding) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		switch {
		case strings.Contains(item.Detail, VarPreload):
			names = append(names, VarPreload)
		case strings.Contains(item.Detail, VarAudit):
			names = append(names, VarAudit)
		case strings.Contains(item.Detail, VarLibraryPath):
			names = append(names, VarLibraryPath)
		}
	}
	return names
}

func (g *LinkerInjectionGuard) blockReason(binary string) (string, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for blocked, reason := range g.blocked {
		if binaryNamesMatch(blocked, binary) {
			if reason == "" {
				return "binary is blocked", true
			}
			return reason, true
		}
	}
	return "", false
}

func binaryNamesMatch(blocked, binary string) bool {
	if blocked == "" || binary == "" {
		return false
	}
	if blocked == binary {
		return true
	}
	return filepath.Base(filepath.Clean(blocked)) == filepath.Base(filepath.Clean(binary))
}

// Repair fixes one finding atomically and keeps the original for forensics.
func (g *LinkerInjectionGuard) Repair(rootfs string, f Finding) error {
	root, err := existingRoot(rootfs)
	if err != nil {
		return err
	}

	g.lifeMu.Lock()
	defer g.lifeMu.Unlock()

	if f.Path != imagePreloadPath {
		return fmt.Errorf("%w: only %s repair is implemented", ErrNotRepairable, imagePreloadPath)
	}
	if f.Kind != "" && f.Kind != KindPreloadFile {
		return fmt.Errorf("%w: %s", ErrNotRepairable, f.Kind)
	}

	targetHostPath := hostPathFor(root, f.Path)
	info, err := os.Stat(targetHostPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrFindingGone, f.Path)
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%w: %s is a directory", ErrNotRepairable, f.Path)
	}

	current, err := os.ReadFile(targetHostPath)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}

	quarantineDir := filepath.Join(root, filepath.FromSlash(quarantineRelDir))
	if err := os.MkdirAll(quarantineDir, 0o755); err != nil {
		return err
	}
	primaryEvidence := filepath.Join(quarantineDir, quarantineName)
	evidenceExists := false
	if _, err := os.Stat(primaryEvidence); err == nil {
		evidenceExists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if !evidenceExists {
		if err := writeEvidence(primaryEvidence, current); err != nil {
			if errors.Is(err, fs.ErrExist) {
				evidenceExists = true
			} else {
				return err
			}
		} else {
			syncDirectory(quarantineDir)
		}
	}

	needsEmptyWrite := true
	if evidenceExists && len(preloadEntries(current)) == 0 {
		needsEmptyWrite = false
	}
	if needsEmptyWrite {
		if evidenceExists {
			sum := sha256.Sum256(current)
			reinfectionEvidence := filepath.Join(quarantineDir, quarantineName+"."+hex.EncodeToString(sum[:])[:16])
			if err := writeEvidence(reinfectionEvidence, current); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		if err := atomicWriteEmptyFile(targetHostPath, mode); err != nil {
			if !isUnsupportedWriteError(err) {
				return err
			}
			upperPath := overlayPathFor(root, f.Path)
			if overlayErr := atomicWriteEmptyFile(upperPath, mode); overlayErr != nil {
				return errors.Join(err, overlayErr)
			}
			workDir := filepath.Join(root, "var/lib/gsb-overlay/work")
			_ = os.MkdirAll(workDir, 0o755)
		}
	}

	kind := f.Kind
	if kind == "" {
		kind = KindPreloadFile
	}
	g.mu.Lock()
	if snapshot, ok := g.snapshots[root]; ok {
		filtered := make([]Finding, 0, len(snapshot.Findings))
		for _, item := range snapshot.Findings {
			if item.Kind == kind && item.Path == f.Path {
				continue
			}
			filtered = append(filtered, item)
		}
		snapshot.Findings = filtered
		if snapshot.Paths == nil {
			snapshot.Paths = make(map[string]pathDigest)
		}
		snapshot.Paths[f.Path] = pathDigest{Exists: true, Digest: emptyDigest}
	}
	g.last = filterFindings(g.last, kind, f.Path)
	g.appendAuditLocked(PhaseRepaired, f.Path, primaryEvidence)
	g.mu.Unlock()

	return nil
}

func filterFindings(items []Finding, kind, imagePath string) []Finding {
	filtered := make([]Finding, 0, len(items))
	for _, item := range items {
		if item.Kind == kind && item.Path == imagePath {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func writeEvidence(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func atomicWriteEmptyFile(hostPath string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(filepath.Dir(hostPath), ".gsb-linker-guard-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := tempFile.Chmod(mode); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, hostPath); err != nil {
		return err
	}
	cleanup = false
	syncDirectory(filepath.Dir(hostPath))
	return nil
}

func syncDirectory(dir string) {
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = handle.Sync()
	_ = handle.Close()
}

func isUnsupportedWriteError(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EACCES, syscall.EPERM, syscall.EROFS:
			return true
		}
	}
	return false
}

// Watch registers paths whose digests are rechecked after every launch.
func (g *LinkerInjectionGuard) Watch(paths []string) error {
	if len(paths) == 0 {
		return ErrNoWatch
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	candidates := make(map[string]struct{}, len(paths))
	for _, watchedPath := range paths {
		candidates[watchedPath] = struct{}{}
	}
	added := 0
	for watchedPath := range candidates {
		if _, exists := g.watched[watchedPath]; !exists {
			added++
		}
	}
	if len(g.watched)+added > g.limits.MaxWatch {
		return fmt.Errorf("%w: table holds %d and request adds %d, cap %d", ErrWatchLimit, len(g.watched), added, g.limits.MaxWatch)
	}
	for watchedPath := range candidates {
		g.watched[watchedPath] = struct{}{}
	}
	return nil
}

// Watched returns the registered watch paths in ascending order.
func (g *LinkerInjectionGuard) Watched() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.watched))
	for watchedPath := range g.watched {
		out = append(out, watchedPath)
	}
	sort.Strings(out)
	return out
}

// Block refuses launches of one binary until the block is lifted.
func (g *LinkerInjectionGuard) Block(binary, reason string) error {
	if strings.TrimSpace(binary) == "" {
		return errors.New("cannot block an empty binary name")
	}
	g.mu.Lock()
	g.blocked[binary] = reason
	g.appendAuditLocked(PhaseBlocked, binary, reason)
	g.mu.Unlock()
	return nil
}

// Blocked returns the blocked binaries in ascending order.
func (g *LinkerInjectionGuard) Blocked() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.blocked))
	for binary := range g.blocked {
		out = append(out, binary)
	}
	sort.Strings(out)
	return out
}

// CheckEnv reports loader variables that carry uncontrolled values.
func (g *LinkerInjectionGuard) CheckEnv(env []string) Findings {
	return checkEnvWithPolicy(env, g.getPolicy())
}

func checkEnvWithPolicy(env []string, policy Policy) Findings {
	items := make([]Finding, 0)
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		var allowed string
		switch name {
		case VarPreload:
			allowed = policy.AllowedPreload
		case VarAudit:
			allowed = policy.AllowedAudit
		case VarLibraryPath:
			allowed = policy.AllowedLibraryPath
		default:
			continue
		}
		if value == allowed {
			continue
		}
		items = append(items, Finding{
			Kind:     KindEnvHijack,
			Path:     "/proc/self/environ",
			Detail:   fmt.Sprintf("%s=%q is not controlled; allowed value is %q", name, value, allowed),
			Severity: highRiskSeverity + 15,
		})
	}
	return Findings{Items: sortedFindings(items)}
}

// CheckRunPath reports relative or writable entries of a runtime search path.
func (g *LinkerInjectionGuard) CheckRunPath(rpath string) Findings {
	items := make([]Finding, 0)
	if strings.TrimSpace(rpath) == "" {
		return Findings{Items: items}
	}
	for _, rawEntry := range strings.Split(rpath, ":") {
		entry := strings.TrimSpace(rawEntry)
		reportPath := entry
		if entry == "" {
			reportPath = "."
		}
		if path.IsAbs(filepath.ToSlash(entry)) {
			continue
		}
		items = append(items, Finding{
			Kind:     KindRunPath,
			Path:     reportPath,
			Detail:   fmt.Sprintf("relative runtime search path entry %q in RUNPATH %q", reportPath, rpath),
			Severity: highRiskSeverity,
		})
	}
	return Findings{Items: sortedFindings(items)}
}

// LastScan returns the findings recorded by the most recent scan.
func (g *LinkerInjectionGuard) LastScan() []Finding {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return cloneFindings(g.last)
}

// Audit returns a copy of the guard log, oldest first.
func (g *LinkerInjectionGuard) Audit() []AuditRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]AuditRecord, len(g.audit))
	copy(out, g.audit)
	return out
}

// Snapshot serialises the scan snapshots, the blocklist and the watch table.
func (g *LinkerInjectionGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	dto := guardSnapshotData{
		Version:   1,
		Limits:    g.limits,
		Policy:    g.policy,
		Roots:     make([]string, 0, len(g.roots)),
		Snapshots: make(map[string]rootSnapshot, len(g.snapshots)),
		Last:      cloneFindings(g.last),
		Blocked:   make(map[string]string, len(g.blocked)),
		Watched:   make([]string, 0, len(g.watched)),
		Audit:     make([]AuditRecord, len(g.audit)),
		Seq:       g.seq,
	}
	for root := range g.roots {
		dto.Roots = append(dto.Roots, root)
	}
	for root, snapshot := range g.snapshots {
		dto.Snapshots[root] = rootSnapshot{
			Root:     snapshot.Root,
			Findings: cloneFindings(snapshot.Findings),
			Paths:    clonePathDigests(snapshot.Paths),
		}
	}
	for binary, reason := range g.blocked {
		dto.Blocked[binary] = reason
	}
	for watchedPath := range g.watched {
		dto.Watched = append(dto.Watched, watchedPath)
	}
	copy(dto.Audit, g.audit)
	g.mu.RUnlock()

	sort.Strings(dto.Roots)
	sort.Strings(dto.Watched)
	data, err := json.MarshalIndent(dto, "", "  ")
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *LinkerInjectionGuard) Restore(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("%w: empty payload", ErrBadSnapshot)
	}
	var dto guardSnapshotData
	if err := json.Unmarshal(data, &dto); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}

	limits := normalizeLimits(dto.Limits)
	roots := make(map[string]struct{}, len(dto.Roots))
	for _, root := range dto.Roots {
		roots[root] = struct{}{}
	}
	snapshots := make(map[string]*rootSnapshot, len(dto.Snapshots))
	for root, snapshot := range dto.Snapshots {
		snapshotCopy := snapshot
		snapshotCopy.Findings = cloneFindings(snapshot.Findings)
		snapshotCopy.Paths = clonePathDigests(snapshot.Paths)
		snapshots[root] = &snapshotCopy
	}
	blocked := make(map[string]string, len(dto.Blocked))
	for binary, reason := range dto.Blocked {
		blocked[binary] = reason
	}
	watched := make(map[string]struct{}, len(dto.Watched))
	for _, watchedPath := range dto.Watched {
		watched[watchedPath] = struct{}{}
	}
	audit := make([]AuditRecord, len(dto.Audit))
	copy(audit, dto.Audit)

	g.mu.Lock()
	g.limits = limits
	g.policy = dto.Policy
	g.roots = roots
	g.snapshots = snapshots
	g.last = cloneFindings(dto.Last)
	g.blocked = blocked
	g.watched = watched
	g.audit = audit
	g.seq = dto.Seq
	g.mu.Unlock()
	return nil
}

// sortedFindings orders findings by path and then by kind.
func sortedFindings(items []Finding) []Finding {
	out := make([]Finding, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
