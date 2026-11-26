package task

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-34"
)

func detectorLimits() impl.Limits {
	return impl.Limits{
		MaxEntryBytes:               64,
		MaxAlertsPerOriginPerMinute: 4,
		MaxEntries:                  32,
	}
}

func newDetector() *impl.LogInjectionDetector {
	return impl.NewLogInjectionDetector(detectorLimits())
}

func logEntry(id, origin, value string) impl.Entry {
	return impl.Entry{
		ID:        id,
		Origin:    origin,
		Field:     "message",
		FieldType: impl.FieldTypeText,
		Target:    impl.TargetFile,
		Value:     value,
	}
}

func hasSignal(signals []string, want string) bool {
	for _, s := range signals {
		if s == want {
			return true
		}
	}
	return false
}

func TestInspectRejectsEmptyEntry(t *testing.T) {
	d := newDetector()
	if _, err := d.Inspect(impl.Entry{Origin: "app", Value: "text"}); !errors.Is(err, impl.ErrEmptyEntry) {
		t.Fatalf("entry without an id must return ErrEmptyEntry, got %v", err)
	}
	if _, err := d.Inspect(impl.Entry{ID: "e-1", Value: "text"}); !errors.Is(err, impl.ErrEmptyEntry) {
		t.Fatalf("entry without an origin must return ErrEmptyEntry, got %v", err)
	}
}

func TestInspectFlagsNewlineInTextField(t *testing.T) {
	d := newDetector()
	entry := logEntry("e-2", "app", "alice\nlevel=admin")
	finding, err := d.Inspect(entry)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if finding.Level != impl.LevelSuspicious {
		t.Fatalf("level = %q, want %q for one forged newline", finding.Level, impl.LevelSuspicious)
	}
	if len(finding.Signals) != 1 || finding.Signals[0] != impl.SignalNewline {
		t.Fatalf("signals = %v, want exactly %q", finding.Signals, impl.SignalNewline)
	}
	if finding.Length != len(entry.Value) {
		t.Fatalf("length = %d, want %d", finding.Length, len(entry.Value))
	}
}

func TestInspectFlagsTerminalEscapes(t *testing.T) {
	d := newDetector()
	entry := logEntry("e-3", "app", "clicked \\u001b[31mred")
	entry.Target = impl.TargetTerminal
	finding, err := d.Inspect(entry)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if !hasSignal(finding.Signals, impl.SignalANSI) {
		t.Fatalf("signals = %v, want %q", finding.Signals, impl.SignalANSI)
	}
	if finding.Level != impl.LevelSuspicious {
		t.Fatalf("level = %q, want %q", finding.Level, impl.LevelSuspicious)
	}
}

func TestInspectFlagsJSONBreak(t *testing.T) {
	d := newDetector()
	entry := logEntry("e-4", "api", "x\",\"role\":\"admin")
	entry.FieldType = impl.FieldTypeJSON
	finding, err := d.Inspect(entry)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if !hasSignal(finding.Signals, impl.SignalJSON) {
		t.Fatalf("signals = %v, want %q", finding.Signals, impl.SignalJSON)
	}
	if finding.Level != impl.LevelSuspicious {
		t.Fatalf("level = %q, want %q", finding.Level, impl.LevelSuspicious)
	}
}

func TestInspectFlagsOversize(t *testing.T) {
	d := newDetector()
	entry := logEntry("e-5", "app", strings.Repeat("a", 65))
	finding, err := d.Inspect(entry)
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if !hasSignal(finding.Signals, impl.SignalOversize) {
		t.Fatalf("signals = %v, want %q", finding.Signals, impl.SignalOversize)
	}
	if finding.Length != 65 {
		t.Fatalf("length = %d, want the original 65 bytes", finding.Length)
	}
}

func TestInspectQuarantinePolicyReturnsError(t *testing.T) {
	d := newDetector()
	if err := d.Policy("edge", impl.Policy{Level: impl.LevelSuspicious, Action: impl.ActionQuarantine}); err != nil {
		t.Fatalf("policy must be accepted, got %v", err)
	}
	if _, err := d.Inspect(logEntry("e-6", "edge", "user=bob\r\nlevel=root")); !errors.Is(err, impl.ErrEntryQuarantined) {
		t.Fatalf("flagged entry under a quarantine policy must return ErrEntryQuarantined, got %v", err)
	}
}

func TestPolicyValidationAndVersion(t *testing.T) {
	d := newDetector()
	if err := d.Policy("   ", impl.Policy{Action: impl.ActionRedact}); !errors.Is(err, impl.ErrBlankOrigin) {
		t.Fatalf("blank origin must return ErrBlankOrigin, got %v", err)
	}
	if err := d.Policy("app", impl.Policy{Action: "drop"}); !errors.Is(err, impl.ErrBadPolicy) {
		t.Fatalf("unsupported action must return ErrBadPolicy, got %v", err)
	}
	if err := d.Policy("app", impl.Policy{Action: impl.ActionRedact}); err != nil {
		t.Fatalf("redact policy must be accepted, got %v", err)
	}
	policy, version, ok := d.PolicyState("app")
	if !ok || policy.Action != impl.ActionRedact || version != 1 {
		t.Fatalf("policy state = %+v version=%d ok=%v, want redact at version 1", policy, version, ok)
	}
	if err := d.Policy("app", impl.Policy{Action: impl.ActionAllow}); err != nil {
		t.Fatalf("second policy must be accepted, got %v", err)
	}
	policy, version, ok = d.PolicyState("app")
	if !ok || policy.Action != impl.ActionAllow || version != 2 {
		t.Fatalf("policy state = %+v version=%d ok=%v, want allow at version 2", policy, version, ok)
	}
}

func TestSanitizeEscapesAndKeepsDigest(t *testing.T) {
	d := newDetector()
	entry := logEntry("e-7", "app", "line1\nline2\tend")
	if _, err := d.Inspect(entry); err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	clean, err := d.Sanitize(entry)
	if err != nil {
		t.Fatalf("sanitize must succeed, got %v", err)
	}
	if strings.Contains(clean.Value, "\n") {
		t.Fatalf("sanitized value still contains a raw newline: %q", clean.Value)
	}
	if !strings.Contains(clean.Value, "\\n") {
		t.Fatalf("sanitized value = %q, want an escaped newline", clean.Value)
	}
	if clean.OriginalLen != len(entry.Value) {
		t.Fatalf("original length = %d, want %d", clean.OriginalLen, len(entry.Value))
	}
	if len(clean.Digest) != 64 {
		t.Fatalf("digest = %q, want a 64 character hex digest", clean.Digest)
	}
	again, err := d.Sanitize(entry)
	if err != nil {
		t.Fatalf("repeated sanitize must succeed, got %v", err)
	}
	if again.Value != clean.Value || again.Digest != clean.Digest {
		t.Fatalf("repeated sanitize is not stable: %q vs %q", again.Value, clean.Value)
	}
}

func TestSanitizeUnknownEntry(t *testing.T) {
	d := newDetector()
	if _, err := d.Sanitize(impl.Entry{ID: "ghost", Origin: "app"}); !errors.Is(err, impl.ErrUnknownEntry) {
		t.Fatalf("sanitize of an uninspected entry must return ErrUnknownEntry, got %v", err)
	}
}

func TestExplainTrace(t *testing.T) {
	d := newDetector()
	entry := logEntry("e-8", "app", "ok\nforged=1")
	if _, err := d.Inspect(entry); err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	trace, err := d.Explain("e-8")
	if err != nil {
		t.Fatalf("explain must succeed, got %v", err)
	}
	if trace.EntryID != "e-8" || trace.Origin != "app" {
		t.Fatalf("trace identity = %+v", trace)
	}
	if trace.Action != impl.ActionRedact {
		t.Fatalf("action = %q, want the default redact", trace.Action)
	}
	if len(trace.Signals) != 1 || trace.Signals[0] != impl.SignalNewline {
		t.Fatalf("trace signals = %v, want %q", trace.Signals, impl.SignalNewline)
	}
	if trace.PolicyVersion < 1 {
		t.Fatalf("policy version = %d, want at least 1", trace.PolicyVersion)
	}
}

func TestSuppressWindowHidesAlerts(t *testing.T) {
	d := newDetector()
	now := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })
	if err := d.Suppress("app", time.Minute); err != nil {
		t.Fatalf("suppress must succeed, got %v", err)
	}
	suppressed, err := d.Inspect(logEntry("e-9", "app", "a\nb"))
	if err != nil {
		t.Fatalf("inspect during suppression must succeed, got %v", err)
	}
	if suppressed.Level != impl.LevelSuppressed {
		t.Fatalf("level = %q, want %q inside the window", suppressed.Level, impl.LevelSuppressed)
	}
	if len(suppressed.Signals) != 1 {
		t.Fatalf("suppressed findings must still record signals, got %v", suppressed.Signals)
	}
	now = now.Add(2 * time.Minute)
	after, err := d.Inspect(logEntry("e-10", "app", "c\nd"))
	if err != nil {
		t.Fatalf("inspect after the window must succeed, got %v", err)
	}
	if after.Level != impl.LevelSuspicious {
		t.Fatalf("level = %q, want %q after the window", after.Level, impl.LevelSuspicious)
	}
}

func TestAlertRateLimit(t *testing.T) {
	limits := detectorLimits()
	limits.MaxAlertsPerOriginPerMinute = 2
	d := impl.NewLogInjectionDetector(limits)
	for i := 0; i < 2; i++ {
		if _, err := d.Inspect(logEntry("e-rate", "app", "x\ny")); err != nil {
			t.Fatalf("alert %d within the budget must succeed, got %v", i, err)
		}
	}
	if _, err := d.Inspect(logEntry("e-over", "app", "z\nw")); !errors.Is(err, impl.ErrAlertRate) {
		t.Fatalf("third alert for one origin must return ErrAlertRate, got %v", err)
	}
	clean, err := d.Inspect(logEntry("e-clean", "app", "GET /health 200"))
	if err != nil {
		t.Fatalf("clean entries must not consume the alert budget, got %v", err)
	}
	if clean.Level != impl.LevelClean {
		t.Fatalf("clean level = %q, want %q", clean.Level, impl.LevelClean)
	}
}

func TestSnapshotRestoreSuppressionAndPolicy(t *testing.T) {
	d := newDetector()
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })
	if err := d.Policy("app", impl.Policy{Action: impl.ActionRedact}); err != nil {
		t.Fatalf("first policy: %v", err)
	}
	if err := d.Policy("app", impl.Policy{Action: impl.ActionAllow}); err != nil {
		t.Fatalf("second policy: %v", err)
	}
	if err := d.Suppress("app", 5*time.Minute); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	blob, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewLogInjectionDetector(detectorLimits())
	restored.SetClock(func() time.Time { return now })
	if err := restored.Restore(blob); err != nil {
		t.Fatalf("restore must accept its own snapshot, got %v", err)
	}
	policy, version, ok := restored.PolicyState("app")
	if !ok || policy.Action != impl.ActionAllow || version != 2 {
		t.Fatalf("restored policy = %+v version=%d ok=%v, want allow at version 2", policy, version, ok)
	}
	if until, ok := restored.Suppression("app"); !ok || !until.After(now) {
		t.Fatalf("restored suppression = %v ok=%v, want an active window", until, ok)
	}
	finding, err := restored.Inspect(logEntry("e-11", "app", "p\nq"))
	if err != nil {
		t.Fatalf("inspect after restore must succeed, got %v", err)
	}
	if finding.Level != impl.LevelSuppressed {
		t.Fatalf("restored suppression must still hide alerts, got %q", finding.Level)
	}
	if err := restored.Restore([]byte("broken snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must return ErrBadSnapshot, got %v", err)
	}
	if until, ok := restored.Suppression("app"); !ok || !until.After(now) {
		t.Fatalf("failed restore must keep the suppression window, got %v ok=%v", until, ok)
	}
}

func TestConcurrentInspectRespectsAlertRate(t *testing.T) {
	limits := detectorLimits()
	limits.MaxAlertsPerOriginPerMinute = 5
	d := impl.NewLogInjectionDetector(limits)
	now := time.Date(2026, 7, 3, 11, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, limited := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.Inspect(logEntry("e-burst", "app", "left\nright"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, impl.ErrAlertRate):
				limited++
			}
		}(i)
	}
	wg.Wait()
	if accepted != 5 {
		t.Fatalf("accepted alerts = %d, want exactly the per minute cap 5", accepted)
	}
	if limited != 15 {
		t.Fatalf("rate limited alerts = %d, want 15", limited)
	}
	if stored := d.Findings(); len(stored) != 5 {
		t.Fatalf("stored findings = %d, want 5", len(stored))
	}
}

func TestInspectKeepsCleanEntry(t *testing.T) {
	d := newDetector()
	finding, err := d.Inspect(logEntry("e-12", "app", "GET /health 200 3ms"))
	if err != nil {
		t.Fatalf("inspect must succeed, got %v", err)
	}
	if finding.Level != impl.LevelClean {
		t.Fatalf("level = %q, want %q", finding.Level, impl.LevelClean)
	}
	if len(finding.Signals) != 0 {
		t.Fatalf("signals = %v, want none", finding.Signals)
	}
	if finding.Length != len("GET /health 200 3ms") {
		t.Fatalf("length = %d, want %d", finding.Length, len("GET /health 200 3ms"))
	}
}
