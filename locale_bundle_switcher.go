// Package task serves localised game text from registered bundles: locale
// fallback, placeholder formatting, plural variants and whole bundle activation
// with rollback.
package task

import "errors"

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// DefaultLocale is the last link of every fallback chain.
const DefaultLocale = "en"

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
}

// NewLocaleBundleSwitcher builds a switcher with the given caps.
func NewLocaleBundleSwitcher(limits Limits) *LocaleBundleSwitcher {
	return &LocaleBundleSwitcher{limits: limits}
}

// Register normalises the locale tag and stores the bundle as a new version.
func (s *LocaleBundleSwitcher) Register(locale string, bundle Bundle) error {
	return errNotImplemented
}

// Activate switches the whole bundle of one locale in one step.
func (s *LocaleBundleSwitcher) Activate(locale string) (Activation, error) {
	return Activation{}, errNotImplemented
}

// Resolve formats one text of the active chain with the given arguments.
func (s *LocaleBundleSwitcher) Resolve(key string, args map[string]string) (string, error) {
	return "", errNotImplemented
}

// ResolvePlural formats one plural text for the given count.
func (s *LocaleBundleSwitcher) ResolvePlural(key string, count int, args map[string]string) (string, error) {
	return "", errNotImplemented
}

// Missing lists the default locale keys the given locale does not carry.
func (s *LocaleBundleSwitcher) Missing(locale string) []string {
	return nil
}

// Rollback reactivates a previously registered bundle version.
func (s *LocaleBundleSwitcher) Rollback(version int) error {
	return errNotImplemented
}

// Active reports the active locale, empty while nothing is active.
func (s *LocaleBundleSwitcher) Active() string {
	return ""
}

// Version reports the active bundle version.
func (s *LocaleBundleSwitcher) Version() int {
	return 0
}

// Keys lists the keys of one registered locale, sorted.
func (s *LocaleBundleSwitcher) Keys(locale string) []string {
	return nil
}

// Bundle reads one registered bundle of a locale: the newest version.
func (s *LocaleBundleSwitcher) Bundle(locale string) (Bundle, bool) {
	return Bundle{}, false
}

// Snapshot serialises the registered versions and the active pointer.
func (s *LocaleBundleSwitcher) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (s *LocaleBundleSwitcher) Restore(data []byte) error {
	return errNotImplemented
}
