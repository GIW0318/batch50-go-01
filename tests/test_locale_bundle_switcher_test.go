package task

import (
	"errors"
	"sort"
	"testing"

	impl "gsb/batch50-go-game-a-14"
)

func localeLimits() impl.Limits {
	return impl.Limits{MaxKeys: 8, MaxBytes: 4096, MaxVersions: 8}
}

func texts(kv ...string) map[string]string {
	out := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestRegisterNormalizesLocale(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("  ZH_Hant  ", impl.Bundle{Texts: texts("title", "標題")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	b, ok := s.Bundle("zh-hant")
	if !ok {
		t.Fatalf("the bundle must be readable under the normalised locale zh-hant")
	}
	if b.Locale != "zh-hant" {
		t.Fatalf("stored locale = %q, want the normalised zh-hant", b.Locale)
	}
	if b.Version != 1 {
		t.Fatalf("first registered version = %d, want 1", b.Version)
	}
	if got := s.Keys("zh-hant"); len(got) != 1 || got[0] != "title" {
		t.Fatalf("keys = %v, want [title]", got)
	}
	if _, ok := s.Bundle("ZH_HANT"); ok {
		t.Fatalf("the raw tag must not be stored as a second locale")
	}
}

func TestRegisterRejectsBadLocaleAndEmptyBundle(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("   ", impl.Bundle{Texts: texts("title", "Title")}); !errors.Is(err, impl.ErrBadLocale) {
		t.Fatalf("blank locale must be rejected with ErrBadLocale, got %v", err)
	}
	if err := s.Register("en", impl.Bundle{}); !errors.Is(err, impl.ErrEmptyBundle) {
		t.Fatalf("a bundle without texts must be rejected with ErrEmptyBundle, got %v", err)
	}
	if err := s.Register("en", impl.Bundle{Texts: map[string]string{}}); !errors.Is(err, impl.ErrEmptyBundle) {
		t.Fatalf("an empty text map must be rejected with ErrEmptyBundle, got %v", err)
	}
	if _, ok := s.Bundle("en"); ok {
		t.Fatalf("rejected registrations must not store a bundle")
	}
}

func TestRegisterEnforcesKeyLimit(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(impl.Limits{MaxKeys: 2, MaxBytes: 4096, MaxVersions: 8})
	wide := texts("a", "1", "b", "2", "c", "3")
	if err := s.Register("en", impl.Bundle{Texts: wide}); !errors.Is(err, impl.ErrKeyLimit) {
		t.Fatalf("three keys against a cap of two must fail with ErrKeyLimit, got %v", err)
	}
	if _, ok := s.Bundle("en"); ok {
		t.Fatalf("a rejected bundle must not be stored")
	}
	if err := s.Register("en", impl.Bundle{Texts: texts("a", "1", "b", "2")}); err != nil {
		t.Fatalf("a bundle inside the key cap must register, got %v", err)
	}
	if got := len(s.Keys("en")); got != 2 {
		t.Fatalf("stored keys = %d, want 2", got)
	}
}

func TestRegisterEnforcesBytesLimit(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(impl.Limits{MaxKeys: 8, MaxBytes: 16, MaxVersions: 8})
	heavy := texts("title", "12345678901234567890")
	if err := s.Register("en", impl.Bundle{Texts: heavy}); !errors.Is(err, impl.ErrBytesLimit) {
		t.Fatalf("an oversized bundle must fail with ErrBytesLimit, got %v", err)
	}
	if _, ok := s.Bundle("en"); ok {
		t.Fatalf("an oversized bundle must not be stored")
	}
	if err := s.Register("en", impl.Bundle{Texts: texts("a", "b")}); err != nil {
		t.Fatalf("a small bundle must register, got %v", err)
	}
}

func TestRegisterRejectsMalformedPlaceholder(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	open := texts("greeting", "Hello {name")
	if err := s.Register("en", impl.Bundle{Texts: open}); !errors.Is(err, impl.ErrBadPlaceholder) {
		t.Fatalf("an unterminated placeholder must fail with ErrBadPlaceholder, got %v", err)
	}
	extra := texts("greeting", "Hi {name}}")
	if err := s.Register("en", impl.Bundle{Texts: extra}); !errors.Is(err, impl.ErrBadPlaceholder) {
		t.Fatalf("a stray closing brace must fail with ErrBadPlaceholder, got %v", err)
	}
	if _, ok := s.Bundle("en"); ok {
		t.Fatalf("a bundle with a broken placeholder must not be stored")
	}
}

func TestActivateReportsBundleSummary(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{Texts: texts("greeting", "Hello", "title", "Title")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	a, err := s.Activate("EN")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if a.Locale != "en" {
		t.Fatalf("activation locale = %q, want the normalised en", a.Locale)
	}
	if a.Version != 1 {
		t.Fatalf("activation version = %d, want 1", a.Version)
	}
	if a.Keys != 2 {
		t.Fatalf("activation keys = %d, want 2", a.Keys)
	}
	if len(a.Missing) != 0 {
		t.Fatalf("the default locale must not miss keys, got %v", a.Missing)
	}
	if got := s.Active(); got != "en" {
		t.Fatalf("active locale = %q, want en", got)
	}
	if got := s.Version(); got != 1 {
		t.Fatalf("active version = %d, want 1", got)
	}
}

func TestActivateRejectsUnknownLocale(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if _, err := s.Activate("ja"); !errors.Is(err, impl.ErrUnknownLocale) {
		t.Fatalf("activating an unregistered locale must fail with ErrUnknownLocale, got %v", err)
	}
	if got := s.Active(); got != "" {
		t.Fatalf("a failed first activation must leave nothing active, got %q", got)
	}
	if err := s.Register("en", impl.Bundle{Texts: texts("title", "Title")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if _, err := s.Activate("en"); err != nil {
		t.Fatalf("activate en: %v", err)
	}
	if _, err := s.Activate("ja"); !errors.Is(err, impl.ErrUnknownLocale) {
		t.Fatalf("an unknown locale must not replace the active one, got %v", err)
	}
	if got := s.Active(); got != "en" {
		t.Fatalf("active locale after the failed switch = %q, want en", got)
	}
}

func TestResolveSubstitutesPlaceholders(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{Texts: texts(
		"welcome", "Hello {name}, you have {count} new reports",
	)}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if _, err := s.Activate("en"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, err := s.Resolve("welcome", map[string]string{"name": "Ada", "count": "3"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "Hello Ada, you have 3 new reports"; got != want {
		t.Fatalf("resolved text = %q, want %q", got, want)
	}
	plain, err := s.Resolve("title", nil)
	if !errors.Is(err, impl.ErrUnknownKey) {
		t.Fatalf("a key absent from the bundle must fail with ErrUnknownKey, got %q / %v", plain, err)
	}
}

func TestResolveRejectsMissingArg(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{Texts: texts("welcome", "Hello {name}, {count} reports")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if _, err := s.Activate("en"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if got, err := s.Resolve("welcome", map[string]string{"name": "Ada"}); !errors.Is(err, impl.ErrMissingArg) {
		t.Fatalf("a missing argument must fail with ErrMissingArg, got %q / %v", got, err)
	}
	if got, err := s.Resolve("welcome", nil); !errors.Is(err, impl.ErrMissingArg) {
		t.Fatalf("resolving with no arguments must fail with ErrMissingArg, got %q / %v", got, err)
	}
}

func TestResolveFallsBackThroughLanguageChain(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{Texts: texts("greeting", "Hello", "title", "Title")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if err := s.Register("zh", impl.Bundle{Texts: texts("title", "标题")}); err != nil {
		t.Fatalf("register zh: %v", err)
	}
	if err := s.Register("zh-tw", impl.Bundle{Texts: texts("greeting", "你好")}); err != nil {
		t.Fatalf("register zh-tw: %v", err)
	}
	if _, err := s.Activate("zh-tw"); err != nil {
		t.Fatalf("activate zh-tw: %v", err)
	}
	exact, err := s.Resolve("greeting", nil)
	if err != nil {
		t.Fatalf("resolve the exact locale text: %v", err)
	}
	if exact != "你好" {
		t.Fatalf("exact locale text = %q, want 你好", exact)
	}
	viaLanguage, err := s.Resolve("title", nil)
	if err != nil {
		t.Fatalf("resolve through the language link: %v", err)
	}
	if viaLanguage != "标题" {
		t.Fatalf("language fallback text = %q, want 标题", viaLanguage)
	}
}

func TestResolveRejectsUnknownKeyWithoutActiveBundle(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if _, err := s.Resolve("title", nil); !errors.Is(err, impl.ErrNoActiveBundle) {
		t.Fatalf("resolving before activation must fail with ErrNoActiveBundle, got %v", err)
	}
	if _, err := s.ResolvePlural("apple", 1, nil); !errors.Is(err, impl.ErrNoActiveBundle) {
		t.Fatalf("plural resolving before activation must fail with ErrNoActiveBundle, got %v", err)
	}
	if err := s.Register("en", impl.Bundle{Texts: texts("title", "Title")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if _, err := s.Activate("en"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if got, err := s.Resolve("absent", nil); !errors.Is(err, impl.ErrUnknownKey) {
		t.Fatalf("an unknown key must fail with ErrUnknownKey, got %q / %v", got, err)
	}
	if got, err := s.ResolvePlural("absent", 2, nil); !errors.Is(err, impl.ErrUnknownKey) {
		t.Fatalf("an unknown plural key must fail with ErrUnknownKey, got %q / %v", got, err)
	}
}

func TestResolvePluralUsesCountForm(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{
		Texts: texts("title", "Title"),
		Plurals: map[string]map[string]string{
			"apple": {"one": "{count} apple", "other": "{count} apples"},
		},
	}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if _, err := s.Activate("en"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	one, err := s.ResolvePlural("apple", 1, nil)
	if err != nil {
		t.Fatalf("plural for one: %v", err)
	}
	if one != "1 apple" {
		t.Fatalf("plural text for count 1 = %q, want 1 apple", one)
	}
	many, err := s.ResolvePlural("apple", 4, nil)
	if err != nil {
		t.Fatalf("plural for four: %v", err)
	}
	if many != "4 apples" {
		t.Fatalf("plural text for count 4 = %q, want 4 apples", many)
	}
	if _, err := s.ResolvePlural("title", 2, nil); !errors.Is(err, impl.ErrUnknownKey) {
		t.Fatalf("a plain text has no plural form and must fail with ErrUnknownKey, got %v", err)
	}
}

func TestMissingListsKeysAbsentFromLocale(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{Texts: texts("title", "Title", "body", "Body", "score", "Score")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if err := s.Register("zh", impl.Bundle{Texts: texts("title", "标题")}); err != nil {
		t.Fatalf("register zh: %v", err)
	}
	missing := s.Missing("zh")
	want := []string{"body", "score"}
	if len(missing) != len(want) {
		t.Fatalf("missing keys = %v, want %v", missing, want)
	}
	sorted := append([]string(nil), missing...)
	sort.Strings(sorted)
	for i := range want {
		if sorted[i] != want[i] {
			t.Fatalf("missing keys = %v, want the sorted %v", missing, want)
		}
	}
	if got := s.Missing("en"); len(got) != 0 {
		t.Fatalf("the default locale must miss nothing, got %v", got)
	}
	if got := s.Missing("ja"); len(got) != 0 {
		t.Fatalf("an unregistered locale must report no keys, got %v", got)
	}
	if got := s.Keys("zh"); len(got) != 1 || got[0] != "title" {
		t.Fatalf("zh keys = %v, want [title]", got)
	}
}

func TestRollbackRestoresPreviousVersion(t *testing.T) {
	s := impl.NewLocaleBundleSwitcher(localeLimits())
	if err := s.Register("en", impl.Bundle{Texts: texts("title", "Title")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if err := s.Register("zh", impl.Bundle{Texts: texts("title", "标题")}); err != nil {
		t.Fatalf("register zh: %v", err)
	}
	if _, err := s.Activate("zh"); err != nil {
		t.Fatalf("activate zh: %v", err)
	}
	if got := s.Version(); got != 2 {
		t.Fatalf("active version = %d, want the second registered 2", got)
	}
	if err := s.Rollback(1); err != nil {
		t.Fatalf("rollback to version 1: %v", err)
	}
	if got := s.Active(); got != "en" {
		t.Fatalf("active locale after rollback = %q, want en", got)
	}
	if got := s.Version(); got != 1 {
		t.Fatalf("active version after rollback = %d, want 1", got)
	}
	text, err := s.Resolve("title", nil)
	if err != nil {
		t.Fatalf("resolve after rollback: %v", err)
	}
	if text != "Title" {
		t.Fatalf("text after rollback = %q, want the English Title", text)
	}
	if err := s.Rollback(99); !errors.Is(err, impl.ErrUnknownVersion) {
		t.Fatalf("an unknown version must fail with ErrUnknownVersion, got %v", err)
	}
	if got := s.Active(); got != "en" {
		t.Fatalf("a failed rollback must keep the active locale, got %q", got)
	}
}

func TestSnapshotRestoreKeepsActiveVersion(t *testing.T) {
	limits := localeLimits()
	s := impl.NewLocaleBundleSwitcher(limits)
	if err := s.Register("en", impl.Bundle{Texts: texts("title", "Title", "body", "Body")}); err != nil {
		t.Fatalf("register en: %v", err)
	}
	if err := s.Register("zh", impl.Bundle{Texts: texts("title", "标题")}); err != nil {
		t.Fatalf("register zh: %v", err)
	}
	if _, err := s.Activate("zh"); err != nil {
		t.Fatalf("activate zh: %v", err)
	}
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewLocaleBundleSwitcher(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Active(); got != "zh" {
		t.Fatalf("restored active locale = %q, want zh", got)
	}
	if got := restored.Version(); got != 2 {
		t.Fatalf("restored active version = %d, want 2", got)
	}
	text, err := restored.Resolve("title", nil)
	if err != nil {
		t.Fatalf("resolve after restore: %v", err)
	}
	if text != "标题" {
		t.Fatalf("restored text = %q, want 标题", text)
	}
	if got := restored.Missing("zh"); len(got) != 1 || got[0] != "body" {
		t.Fatalf("missing keys after restore = %v, want [body]", got)
	}
	if err := restored.Restore([]byte("{corrupt")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Active(); got != "zh" {
		t.Fatalf("a rejected restore must not touch the live state, got %q", got)
	}
}
