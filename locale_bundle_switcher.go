// Package task serves localised game text from registered bundles: locale
// fallback, placeholder formatting, plural variants and whole bundle activation
// with rollback.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultLocale is the last link of every fallback chain.
const DefaultLocale = "en"

const (
	snapshotFormat = "gsb-locale-bundle-switcher"
	snapshotSchema = 1
)

// Errors returned by the switcher.
var (
	ErrBadLocale      = errors.New("locale tag is invalid")
	ErrEmptyBundle    = errors.New("bundle has no texts")
	ErrKeyLimit       = errors.New("bundle key limit reached")
	ErrBytesLimit     = errors.New("bundle byte size limit reached")
	ErrBadPlaceholder = errors.New("text placeholder is malformed")
	ErrUnknownLocale  = errors.New("locale is not registered")
	ErrNoActiveBundle = errors.New("no bundle is active")
	ErrUnknownKey     = errors.New("text key is not available")
	ErrMissingArg     = errors.New("placeholder argument is missing")
	ErrUnknownVersion = errors.New("unknown bundle version")
	ErrVersionLimit   = errors.New("retained version limit reached")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

// Bundle is one registered localisation snapshot. Version is assigned by the
// switcher in registration order, starting at 1.
type Bundle struct {
	Locale  string                       `json:"locale"`
	Version int                          `json:"version"`
	Texts   map[string]string            `json:"texts"`
	Plurals map[string]map[string]string `json:"plurals"`
}

// Activation reports what became active.
type Activation struct {
	Locale  string   `json:"locale"`
	Version int      `json:"version"`
	Keys    int      `json:"keys"`
	Missing []string `json:"missing"`
}

// Limits bounds a single bundle and the retained versions.
type Limits struct {
	MaxKeys     int
	MaxBytes    int
	MaxVersions int
}

// LocaleBundleSwitcher owns the registered bundles and the active pointer.
type LocaleBundleSwitcher struct {
	limits Limits

	mu            sync.RWMutex
	bundles       map[int]Bundle
	latest        map[string]int
	nextVersion   int
	activeVersion int
}

type snapshotData struct {
	Format        string   `json:"format"`
	Schema        int      `json:"schema"`
	NextVersion   int      `json:"next_version"`
	ActiveVersion int      `json:"active_version"`
	ActiveLocale  string   `json:"active_locale"`
	Bundles       []Bundle `json:"bundles"`
}

// NewLocaleBundleSwitcher builds a switcher with the given caps.
func NewLocaleBundleSwitcher(limits Limits) *LocaleBundleSwitcher {
	return &LocaleBundleSwitcher{
		limits:      limits,
		bundles:     make(map[int]Bundle),
		latest:      make(map[string]int),
		nextVersion: 1,
	}
}

func normalizeLocale(locale string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(locale)), "_", "-")
}

func cloneBundle(input Bundle) Bundle {
	out := input
	if input.Texts != nil {
		out.Texts = make(map[string]string, len(input.Texts))
		for key, value := range input.Texts {
			out.Texts[key] = value
		}
	}
	if input.Plurals != nil {
		out.Plurals = make(map[string]map[string]string, len(input.Plurals))
		for key, forms := range input.Plurals {
			out.Plurals[key] = make(map[string]string, len(forms))
			for form, text := range forms {
				out.Plurals[key][form] = text
			}
		}
	}
	return out
}

func bundleKeys(bundle Bundle) map[string]struct{} {
	keys := make(map[string]struct{}, len(bundle.Texts)+len(bundle.Plurals))
	for key := range bundle.Texts {
		keys[key] = struct{}{}
	}
	for key := range bundle.Plurals {
		keys[key] = struct{}{}
	}
	return keys
}

func bundleByteSize(bundle Bundle) int {
	size := 0
	for key, text := range bundle.Texts {
		size += len(key) + len(text)
	}
	for key, forms := range bundle.Plurals {
		size += len(key)
		for form, text := range forms {
			size += len(form) + len(text)
		}
	}
	return size
}

func validatePlaceholders(text string) error {
	inside := false
	for _, r := range text {
		switch r {
		case '{':
			if inside {
				return fmt.Errorf("%w: nested opening brace", ErrBadPlaceholder)
			}
			inside = true
		case '}':
			if !inside {
				return fmt.Errorf("%w: closing brace without opening brace", ErrBadPlaceholder)
			}
			inside = false
		}
	}
	if inside {
		return fmt.Errorf("%w: opening brace without closing brace", ErrBadPlaceholder)
	}
	return nil
}

func validateBundlePlaceholders(bundle Bundle) error {
	for _, text := range bundle.Texts {
		if err := validatePlaceholders(text); err != nil {
			return err
		}
	}
	for _, forms := range bundle.Plurals {
		for _, text := range forms {
			if err := validatePlaceholders(text); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareBundle(locale string, input Bundle, limits Limits) (Bundle, error) {
	bundle := cloneBundle(input)
	bundle.Locale = locale
	bundle.Version = 0

	if len(bundle.Texts) == 0 && len(bundle.Plurals) == 0 {
		return Bundle{}, ErrEmptyBundle
	}

	keyCount := len(bundleKeys(bundle))
	if keyCount > limits.MaxKeys {
		return Bundle{}, fmt.Errorf("%w: %d keys exceed %d", ErrKeyLimit, keyCount, limits.MaxKeys)
	}

	byteSize := bundleByteSize(bundle)
	if byteSize > limits.MaxBytes {
		return Bundle{}, fmt.Errorf("%w: %d bytes exceed %d", ErrBytesLimit, byteSize, limits.MaxBytes)
	}

	if err := validateBundlePlaceholders(bundle); err != nil {
		return Bundle{}, err
	}

	return bundle, nil
}

// Register normalises the locale tag and stores the bundle as a new version.
func (s *LocaleBundleSwitcher) Register(locale string, bundle Bundle) error {
	normalized := normalizeLocale(locale)
	if normalized == "" {
		return ErrBadLocale
	}

	candidate, err := prepareBundle(normalized, bundle, s.limits)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.bundles) >= s.limits.MaxVersions {
		return fmt.Errorf("%w: %d versions exceed %d", ErrVersionLimit, len(s.bundles), s.limits.MaxVersions)
	}

	version := s.nextVersion
	candidate.Version = version
	s.bundles[version] = candidate
	s.latest[normalized] = version
	s.nextVersion++
	return nil
}

func fallbackChain(activeLocale string) []string {
	chain := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(locale string) {
		if locale == "" {
			return
		}
		if _, ok := seen[locale]; ok {
			return
		}
		seen[locale] = struct{}{}
		chain = append(chain, locale)
	}

	add(activeLocale)
	if separator := strings.IndexByte(activeLocale, '-'); separator > 0 {
		add(activeLocale[:separator])
	}
	add(DefaultLocale)
	return chain
}

func (s *LocaleBundleSwitcher) bundleForChainLocked(locale, activeLocale string, activeVersion int) (Bundle, bool) {
	if locale == activeLocale {
		bundle, ok := s.bundles[activeVersion]
		return bundle, ok
	}

	version, ok := s.latest[locale]
	if !ok {
		return Bundle{}, false
	}
	bundle, ok := s.bundles[version]
	return bundle, ok
}

func formatText(template string, args map[string]string) (string, error) {
	if err := validatePlaceholders(template); err != nil {
		return "", err
	}

	var result strings.Builder
	var name strings.Builder
	inside := false

	for _, r := range template {
		switch {
		case r == '{':
			if inside {
				return "", fmt.Errorf("%w: nested opening brace", ErrBadPlaceholder)
			}
			inside = true
			name.Reset()
		case r == '}':
			if !inside {
				return "", fmt.Errorf("%w: closing brace without opening brace", ErrBadPlaceholder)
			}
			placeholder := name.String()
			value, ok := args[placeholder]
			if !ok {
				return "", fmt.Errorf("%w: %q", ErrMissingArg, placeholder)
			}
			result.WriteString(value)
			inside = false
		case inside:
			name.WriteRune(r)
		default:
			result.WriteRune(r)
		}
	}

	if inside {
		return "", fmt.Errorf("%w: opening brace without closing brace", ErrBadPlaceholder)
	}
	return result.String(), nil
}

func pluralVariant(forms map[string]string, count int) (string, bool) {
	wanted := "other"
	alternate := "one"
	if count == 1 {
		wanted = "one"
		alternate = "other"
	}

	if text, ok := forms[wanted]; ok {
		return text, true
	}
	if text, ok := forms[alternate]; ok {
		return text, true
	}

	variants := make([]string, 0, len(forms))
	for variant := range forms {
		variants = append(variants, variant)
	}
	sort.Strings(variants)
	if len(variants) == 0 {
		return "", false
	}
	return forms[variants[0]], true
}

func pluralArgs(args map[string]string, count int) map[string]string {
	resolved := make(map[string]string, len(args)+1)
	for key, value := range args {
		resolved[key] = value
	}
	resolved["count"] = strconv.Itoa(count)
	return resolved
}

func sortedMissingKeys(defaultBundle, targetBundle Bundle) []string {
	targetKeys := bundleKeys(targetBundle)
	missing := make([]string, 0)
	for key := range bundleKeys(defaultBundle) {
		if _, ok := targetKeys[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

func (s *LocaleBundleSwitcher) missingLocked(locale string) []string {
	if _, ok := s.latest[locale]; !ok {
		return nil
	}
	defaultVersion, ok := s.latest[DefaultLocale]
	if !ok {
		return []string{}
	}
	targetVersion := s.latest[locale]
	return sortedMissingKeys(s.bundles[defaultVersion], s.bundles[targetVersion])
}

// Activate switches the whole bundle of one locale in one step.
func (s *LocaleBundleSwitcher) Activate(locale string) (Activation, error) {
	normalized := normalizeLocale(locale)

	s.mu.Lock()
	defer s.mu.Unlock()

	version, ok := s.latest[normalized]
	if !ok {
		if normalized == "" {
			return Activation{}, fmt.Errorf("%w: %w", ErrUnknownLocale, ErrBadLocale)
		}
		return Activation{}, fmt.Errorf("%w: %q", ErrUnknownLocale, locale)
	}

	s.activeVersion = version
	bundle := s.bundles[version]
	return Activation{
		Locale:  bundle.Locale,
		Version: bundle.Version,
		Keys:    len(bundleKeys(bundle)),
		Missing: s.missingLocked(bundle.Locale),
	}, nil
}

// Resolve formats one text of the active chain with the given arguments.
func (s *LocaleBundleSwitcher) Resolve(key string, args map[string]string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.activeVersion == 0 {
		return "", ErrNoActiveBundle
	}
	activeBundle, ok := s.bundles[s.activeVersion]
	if !ok {
		return "", ErrNoActiveBundle
	}

	for _, locale := range fallbackChain(activeBundle.Locale) {
		bundle, ok := s.bundleForChainLocked(locale, activeBundle.Locale, s.activeVersion)
		if !ok {
			continue
		}
		template, ok := bundle.Texts[key]
		if !ok {
			continue
		}
		return formatText(template, args)
	}

	return "", fmt.Errorf("%w: %q", ErrUnknownKey, key)
}

// ResolvePlural formats one plural text for the given count.
func (s *LocaleBundleSwitcher) ResolvePlural(key string, count int, args map[string]string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.activeVersion == 0 {
		return "", ErrNoActiveBundle
	}
	activeBundle, ok := s.bundles[s.activeVersion]
	if !ok {
		return "", ErrNoActiveBundle
	}

	resolvedArgs := pluralArgs(args, count)
	for _, locale := range fallbackChain(activeBundle.Locale) {
		bundle, ok := s.bundleForChainLocked(locale, activeBundle.Locale, s.activeVersion)
		if !ok {
			continue
		}
		forms, ok := bundle.Plurals[key]
		if !ok {
			continue
		}
		template, ok := pluralVariant(forms, count)
		if !ok {
			continue
		}
		return formatText(template, resolvedArgs)
	}

	return "", fmt.Errorf("%w: %q", ErrUnknownKey, key)
}

// Missing lists the default locale keys the given locale does not carry.
func (s *LocaleBundleSwitcher) Missing(locale string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.missingLocked(locale)
}

// Rollback reactivates a previously registered bundle version.
func (s *LocaleBundleSwitcher) Rollback(version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bundle, ok := s.bundles[version]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownVersion, version)
	}
	s.activeVersion = bundle.Version
	return nil
}

// Active reports the active locale, empty while nothing is active.
func (s *LocaleBundleSwitcher) Active() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.activeVersion == 0 {
		return ""
	}
	return s.bundles[s.activeVersion].Locale
}

// Version reports the active bundle version.
func (s *LocaleBundleSwitcher) Version() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeVersion
}

// Keys lists the keys of one registered locale, sorted.
func (s *LocaleBundleSwitcher) Keys(locale string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	version, ok := s.latest[locale]
	if !ok {
		return nil
	}
	keys := bundleKeys(s.bundles[version])
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Bundle reads one registered bundle of a locale: the newest version.
func (s *LocaleBundleSwitcher) Bundle(locale string) (Bundle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	version, ok := s.latest[locale]
	if !ok {
		return Bundle{}, false
	}
	return cloneBundle(s.bundles[version]), true
}

// Snapshot serialises the registered versions and the active pointer.
func (s *LocaleBundleSwitcher) Snapshot() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bundles := make([]Bundle, 0, len(s.bundles))
	for _, bundle := range s.bundles {
		bundles = append(bundles, cloneBundle(bundle))
	}
	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].Version < bundles[j].Version
	})

	activeLocale := ""
	if s.activeVersion != 0 {
		activeLocale = s.bundles[s.activeVersion].Locale
	}

	payload := snapshotData{
		Format:        snapshotFormat,
		Schema:        snapshotSchema,
		NextVersion:   s.nextVersion,
		ActiveVersion: s.activeVersion,
		ActiveLocale:  activeLocale,
		Bundles:       bundles,
	}
	return json.Marshal(payload)
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *LocaleBundleSwitcher) Restore(data []byte) error {
	var payload snapshotData
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: trailing snapshot content", ErrBadSnapshot)
	}

	if payload.Format != snapshotFormat || payload.Schema != snapshotSchema {
		return fmt.Errorf("%w: unsupported snapshot format", ErrBadSnapshot)
	}
	if payload.NextVersion < 1 || len(payload.Bundles) != payload.NextVersion-1 {
		return fmt.Errorf("%w: invalid version sequence", ErrBadSnapshot)
	}
	if payload.ActiveVersion < 0 || payload.ActiveVersion >= payload.NextVersion {
		return fmt.Errorf("%w: invalid active version", ErrBadSnapshot)
	}

	versions := make(map[int]Bundle, len(payload.Bundles))
	latest := make(map[string]int)
	for _, rawBundle := range payload.Bundles {
		if rawBundle.Version <= 0 || rawBundle.Version >= payload.NextVersion {
			return fmt.Errorf("%w: version %d is outside the sequence", ErrBadSnapshot, rawBundle.Version)
		}
		if _, exists := versions[rawBundle.Version]; exists {
			return fmt.Errorf("%w: duplicate version %d", ErrBadSnapshot, rawBundle.Version)
		}

		normalized := normalizeLocale(rawBundle.Locale)
		if normalized == "" || normalized != rawBundle.Locale {
			return fmt.Errorf("%w: invalid locale %q", ErrBadSnapshot, rawBundle.Locale)
		}

		prepared, err := prepareBundle(normalized, rawBundle, s.limits)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrBadSnapshot, err)
		}
		prepared.Version = rawBundle.Version
		versions[prepared.Version] = prepared

		if current, exists := latest[normalized]; !exists || prepared.Version > current {
			latest[normalized] = prepared.Version
		}
	}

	for version := 1; version < payload.NextVersion; version++ {
		if _, ok := versions[version]; !ok {
			return fmt.Errorf("%w: missing version %d", ErrBadSnapshot, version)
		}
	}

	if payload.ActiveVersion != 0 {
		activeBundle, ok := versions[payload.ActiveVersion]
		if !ok {
			return fmt.Errorf("%w: active version is absent", ErrBadSnapshot)
		}
		if payload.ActiveLocale != activeBundle.Locale {
			return fmt.Errorf("%w: active locale pointer does not match version", ErrBadSnapshot)
		}
	} else if payload.ActiveLocale != "" {
		return fmt.Errorf("%w: active locale pointer without active version", ErrBadSnapshot)
	}

	if s.limits.MaxVersions < 0 || len(versions) > s.limits.MaxVersions {
		return fmt.Errorf("%w: %w", ErrBadSnapshot, ErrVersionLimit)
	}

	s.mu.Lock()
	s.bundles = versions
	s.latest = latest
	s.nextVersion = payload.NextVersion
	s.activeVersion = payload.ActiveVersion
	s.mu.Unlock()
	return nil
}
