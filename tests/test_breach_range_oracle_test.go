package task

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-a-08"
)

func digestOf(password string) string {
	sum := sha1.Sum([]byte(password))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func prefixOf(password string) string {
	return digestOf(password)[:impl.DigestPrefixLen]
}

func suffixOf(password string) string {
	return digestOf(password)[impl.DigestPrefixLen:]
}

func rangeBody(prefix string, lines ...string) []byte {
	var b strings.Builder
	b.WriteString("PREFIX:" + prefix + "\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	return []byte(b.String())
}

type fakeUpstream struct {
	mu       sync.Mutex
	calls    int
	prefixes []string
	bodies   map[string][]byte
	err      error
	delay    time.Duration
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{bodies: map[string][]byte{}}
}

func (f *fakeUpstream) FetchPrefix(ctx context.Context, prefix string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.prefixes = append(f.prefixes, prefix)
	delay := f.delay
	err := f.err
	body, hasBody := f.bodies[prefix]
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if hasBody {
		return body, nil
	}
	return rangeBody(prefix), nil
}

func (f *fakeUpstream) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeUpstream) setDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

func (f *fakeUpstream) putBody(prefix string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[prefix] = body
}

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeUpstream) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.prefixes))
	copy(out, f.prefixes)
	return out
}

func oracleLimits() impl.Limits {
	return impl.Limits{
		MaxCacheEntries:  8,
		MaxResponseBytes: 4096,
		Timeout:          2 * time.Second,
		CacheTTL:         time.Minute,
		CircuitOpenAfter: 2,
	}
}

func TestCleanPasswordAllowsAndSendsOnlyPrefix(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	pw := "correct-horse-battery"
	up.putBody(prefixOf(pw), rangeBody(prefixOf(pw)))
	v, err := o.Check(context.Background(), "tenant-a", pw)
	if err != nil {
		t.Fatalf("clean lookup must succeed, got %v", err)
	}
	if v.Breached {
		t.Fatalf("password with an empty range must not be reported as breached")
	}
	if v.Count != 0 {
		t.Fatalf("hit count = %d, want 0", v.Count)
	}
	if v.Band != impl.BandNone || v.Action != impl.ActionAllow {
		t.Fatalf("band/action = %q/%q, want %q/%q", v.Band, v.Action, impl.BandNone, impl.ActionAllow)
	}
	if v.Prefix != prefixOf(pw) {
		t.Fatalf("verdict prefix = %q, want %q", v.Prefix, prefixOf(pw))
	}
	if v.Source != impl.SourceUpstream {
		t.Fatalf("source = %q, want %q", v.Source, impl.SourceUpstream)
	}
	seen := up.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(seen))
	}
	if seen[0] != prefixOf(pw) || len(seen[0]) != impl.DigestPrefixLen {
		t.Fatalf("upstream saw %q, want the five digit prefix %q", seen[0], prefixOf(pw))
	}
	if seen[0] == digestOf(pw) {
		t.Fatalf("full digest must never leave the process")
	}
}

func TestBreachedPasswordGradesByCount(t *testing.T) {
	cases := []struct {
		password string
		count    int
		band     string
		action   string
	}{
		{"pw-low-1", 3, impl.BandLow, impl.ActionWarn},
		{"pw-high-1", 500, impl.BandHigh, ""},
		{"pw-critical-1", 10000, impl.BandCritical, impl.ActionDeny},
	}
	for _, tc := range cases {
		up := newFakeUpstream()
		up.putBody(prefixOf(tc.password), rangeBody(prefixOf(tc.password), suffixOf(tc.password)+":"+itoa(tc.count)))
		o := impl.NewBreachRangeOracle(up, oracleLimits())
		v, err := o.Check(context.Background(), "tenant-a", tc.password)
		if err != nil {
			t.Fatalf("%s: lookup must succeed, got %v", tc.password, err)
		}
		if !v.Breached {
			t.Fatalf("%s: expected a breach verdict", tc.password)
		}
		if v.Count != tc.count {
			t.Fatalf("%s: count = %d, want %d", tc.password, v.Count, tc.count)
		}
		if v.Band != tc.band {
			t.Fatalf("%s: band = %q, want %q", tc.password, v.Band, tc.band)
		}
		if tc.action != "" && v.Action != tc.action {
			t.Fatalf("%s: action = %q, want %q", tc.password, v.Action, tc.action)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestCheckRejectsBlankInputs(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	if _, err := o.Check(context.Background(), "  ", "pw"); !errors.Is(err, impl.ErrBlankTenant) {
		t.Fatalf("blank tenant must be rejected with ErrBlankTenant, got %v", err)
	}
	if _, err := o.Check(context.Background(), "tenant-a", ""); !errors.Is(err, impl.ErrBlankPassword) {
		t.Fatalf("blank password must be rejected with ErrBlankPassword, got %v", err)
	}
	if _, err := o.CheckFor(context.Background(), "tenant-a", "signup", "pw"); !errors.Is(err, impl.ErrUnknownStage) {
		t.Fatalf("unknown stage must be rejected with ErrUnknownStage, got %v", err)
	}
	if got := up.callCount(); got != 0 {
		t.Fatalf("rejected requests must not reach upstream, got %d calls", got)
	}
}

func TestSecondCheckUsesCache(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	pw := "cache-me-please"
	up.putBody(prefixOf(pw), rangeBody(prefixOf(pw), suffixOf(pw)+":42"))
	first, err := o.Check(context.Background(), "tenant-a", pw)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	second, err := o.Check(context.Background(), "tenant-a", pw)
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if got := up.callCount(); got != 1 {
		t.Fatalf("upstream calls = %d, want a single fetch", got)
	}
	if second.Source != impl.SourceCache {
		t.Fatalf("second source = %q, want %q", second.Source, impl.SourceCache)
	}
	if second.Count != first.Count || second.Band != first.Band || second.Breached != first.Breached {
		t.Fatalf("cached verdict %+v differs from upstream verdict %+v", second, first)
	}
}

func TestConcurrentSamePrefixCoalesces(t *testing.T) {
	up := newFakeUpstream()
	up.setDelay(30 * time.Millisecond)
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	pw := "shared-prefix-password"
	up.putBody(prefixOf(pw), rangeBody(prefixOf(pw), suffixOf(pw)+":7"))
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]impl.Verdict, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			results[n], errs[n] = o.Check(context.Background(), "tenant-a", pw)
		}(i)
	}
	close(start)
	wg.Wait()
	if got := up.callCount(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 shared request", got)
	}
	for i := 0; i < 8; i++ {
		if errs[i] != nil {
			t.Fatalf("waiter %d failed: %v", i, errs[i])
		}
		if !results[i].Breached || results[i].Count != 7 {
			t.Fatalf("waiter %d verdict = %+v, want breached with count 7", i, results[i])
		}
	}
}

func TestBudgetExhaustion(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	if err := o.SetBudget("tenant-b", 2); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	for i, pw := range []string{"budget-one", "budget-two"} {
		if _, err := o.Check(context.Background(), "tenant-b", pw); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	v, err := o.Check(context.Background(), "tenant-b", "budget-three")
	if err != nil {
		t.Fatalf("exhausted lookup must still answer, got %v", err)
	}
	if v.Band != impl.BandBudget {
		t.Fatalf("band = %q, want %q", v.Band, impl.BandBudget)
	}
	if v.Breached {
		t.Fatalf("exhausted budget must not report a breach")
	}
	if got := up.callCount(); got != 2 {
		t.Fatalf("upstream calls = %d, want the budget of 2", got)
	}
	usage, err := o.Budget("tenant-b")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if usage.Limit != 2 || usage.Used != 2 || usage.Remaining != 0 {
		t.Fatalf("usage = %+v, want limit 2 used 2 remaining 0", usage)
	}
}

func TestSetBudgetRejectsInvalidLimit(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	if err := o.SetBudget("tenant-c", 0); !errors.Is(err, impl.ErrBudgetLimit) {
		t.Fatalf("zero budget must be rejected with ErrBudgetLimit, got %v", err)
	}
	if err := o.SetBudget("tenant-c", -5); !errors.Is(err, impl.ErrBudgetLimit) {
		t.Fatalf("negative budget must be rejected with ErrBudgetLimit, got %v", err)
	}
	usage, err := o.Budget("tenant-c")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if usage.Limit <= 0 {
		t.Fatalf("rejected budgets must not change the allowance, got %+v", usage)
	}
	if usage.Used != 0 || usage.Remaining != usage.Limit {
		t.Fatalf("fresh tenant usage = %+v, want nothing spent", usage)
	}
}

func TestCircuitOpensAfterConsecutiveFailures(t *testing.T) {
	up := newFakeUpstream()
	up.setErr(errors.New("connection refused"))
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	for i := 0; i < 2; i++ {
		_, err := o.Check(context.Background(), "tenant-d", "circuit-"+itoa(i))
		if !errors.Is(err, impl.ErrUpstreamFailed) {
			t.Fatalf("failure %d must surface as ErrUpstreamFailed, got %v", i, err)
		}
	}
	status := o.Health()
	if status.State != impl.StateOpen {
		t.Fatalf("breaker state = %q, want %q", status.State, impl.StateOpen)
	}
	if status.Failures != 2 {
		t.Fatalf("failure counter = %d, want 2", status.Failures)
	}
	up.setErr(nil)
	callsBefore := up.callCount()
	v, err := o.Check(context.Background(), "tenant-d", "circuit-healed")
	if err != nil {
		t.Fatalf("open breaker must answer without an error, got %v", err)
	}
	if v.Band != impl.BandUnavailable {
		t.Fatalf("band = %q, want %q", v.Band, impl.BandUnavailable)
	}
	if got := up.callCount(); got != callsBefore {
		t.Fatalf("open breaker still queried upstream: %d -> %d", callsBefore, got)
	}
}

func TestOversizedResponseRejected(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, impl.Limits{
		MaxCacheEntries:  8,
		MaxResponseBytes: 32,
		Timeout:          time.Second,
		CacheTTL:         time.Minute,
		CircuitOpenAfter: 5,
	})
	pw := "huge-range"
	up.putBody(prefixOf(pw), []byte(strings.Repeat("A", 512)))
	_, err := o.Check(context.Background(), "tenant-e", pw)
	if !errors.Is(err, impl.ErrResponseTooLarge) {
		t.Fatalf("oversized range body must be rejected with ErrResponseTooLarge, got %v", err)
	}
	if got := len(o.CachePrefixes()); got != 0 {
		t.Fatalf("rejected responses must not be cached, got %d entries", got)
	}
}

func TestUpstreamTimeout(t *testing.T) {
	up := newFakeUpstream()
	up.setDelay(120 * time.Millisecond)
	o := impl.NewBreachRangeOracle(up, impl.Limits{
		MaxCacheEntries:  8,
		MaxResponseBytes: 4096,
		Timeout:          10 * time.Millisecond,
		CacheTTL:         time.Minute,
		CircuitOpenAfter: 5,
	})
	_, err := o.Check(context.Background(), "tenant-f", "slow-upstream")
	if !errors.Is(err, impl.ErrUpstreamTimeout) {
		t.Fatalf("slow upstream must surface as ErrUpstreamTimeout, got %v", err)
	}
}

func TestCacheEvictsLeastRecentlyUsedPrefix(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, impl.Limits{
		MaxCacheEntries:  2,
		MaxResponseBytes: 4096,
		Timeout:          time.Second,
		CacheTTL:         time.Minute,
		CircuitOpenAfter: 5,
	})
	for _, prefix := range []string{"AAAAA", "BBBBB"} {
		if err := o.Warm(prefix); err != nil {
			t.Fatalf("warm %s: %v", prefix, err)
		}
	}
	if err := o.Warm("AAAAA"); err != nil {
		t.Fatalf("refresh warm: %v", err)
	}
	if err := o.Warm("CCCCC"); err != nil {
		t.Fatalf("warm CCCCC: %v", err)
	}
	prefixes := o.CachePrefixes()
	if len(prefixes) != 2 {
		t.Fatalf("cache holds %d prefixes, want the cap of 2", len(prefixes))
	}
	present := map[string]bool{}
	for _, p := range prefixes {
		present[p] = true
	}
	if !present["AAAAA"] || !present["CCCCC"] {
		t.Fatalf("cache contents = %v, want the freshly used AAAAA and CCCCC", prefixes)
	}
	if present["BBBBB"] {
		t.Fatalf("least recently used prefix BBBBB was not evicted: %v", prefixes)
	}
}

func TestWarmRejectsMalformedPrefix(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	for _, prefix := range []string{"", "ABCD", "ABCDEF", "abcde", "GGGGG", "12 45"} {
		if err := o.Warm(prefix); !errors.Is(err, impl.ErrBadPrefix) {
			t.Fatalf("prefix %q must be rejected with ErrBadPrefix, got %v", prefix, err)
		}
	}
	if got := up.callCount(); got != 0 {
		t.Fatalf("malformed prefixes must not reach upstream, got %d calls", got)
	}
	if got := len(o.CachePrefixes()); got != 0 {
		t.Fatalf("malformed prefixes must not be cached, got %d entries", got)
	}
}

func TestHealthReportsCacheSize(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	fresh := o.Health()
	if fresh.State != impl.StateClosed || fresh.Cached != 0 || fresh.Prefixes != 0 {
		t.Fatalf("fresh health = %+v, want a closed empty cache", fresh)
	}
	for _, prefix := range []string{"11111", "22222"} {
		if err := o.Warm(prefix); err != nil {
			t.Fatalf("warm %s: %v", prefix, err)
		}
	}
	status := o.Health()
	if status.Cached != 2 || status.Prefixes != 2 {
		t.Fatalf("health = %+v, want 2 cached prefixes", status)
	}
	if status.State != impl.StateClosed {
		t.Fatalf("healthy cache must stay closed, got %q", status.State)
	}
}

func TestResetClearsTenantBudget(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	if err := o.SetBudget("tenant-g", 5); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if _, err := o.Check(context.Background(), "tenant-g", "reset-me"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if usage, _ := o.Budget("tenant-g"); usage.Used != 1 {
		t.Fatalf("used = %d before reset, want 1", usage.Used)
	}
	if err := o.Reset("tenant-g"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	usage, err := o.Budget("tenant-g")
	if err != nil {
		t.Fatalf("budget after reset: %v", err)
	}
	if usage.Limit != 5 || usage.Used != 0 || usage.Remaining != 5 {
		t.Fatalf("usage after reset = %+v, want limit 5 used 0 remaining 5", usage)
	}
	if got := up.callCount(); got != 1 {
		t.Fatalf("reset must not touch upstream, calls = %d", got)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	up := newFakeUpstream()
	o := impl.NewBreachRangeOracle(up, oracleLimits())
	if err := o.SetBudget("tenant-h", 3); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	for _, prefix := range []string{"33333", "44444"} {
		if err := o.Warm(prefix); err != nil {
			t.Fatalf("warm %s: %v", prefix, err)
		}
	}
	if _, err := o.Check(context.Background(), "tenant-h", "snapshot-pw"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	snap, err := o.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewBreachRangeOracle(up, oracleLimits())
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("restore of a fresh snapshot must succeed, got %v", err)
	}
	if got := len(restored.CachePrefixes()); got != 3 {
		t.Fatalf("restored cache holds %d prefixes, want 3", got)
	}
	usage, err := restored.Budget("tenant-h")
	if err != nil {
		t.Fatalf("restored budget: %v", err)
	}
	if usage.Limit != 3 || usage.Used != 1 || usage.Remaining != 2 {
		t.Fatalf("restored usage = %+v, want limit 3 used 1 remaining 2", usage)
	}
	if err := restored.Restore([]byte("\x00broken")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must be rejected with ErrBadSnapshot, got %v", err)
	}
	if got := len(restored.CachePrefixes()); got != 3 {
		t.Fatalf("rejected restore must keep the live cache, got %d entries", got)
	}
}
