// Package task selects a decryption key generation for encrypted SAML
// assertions, enforces the transport algorithm allow list and keeps the key
// ring generations consistent across restarts.
package task

import (
	"errors"
	"sync"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Key transport algorithms understood by the key ring.
const (
	AlgRSAOAEP256 = "rsa-oaep-256"
	AlgRSAOAEP1   = "rsa-oaep-1"
	AlgRSA15      = "rsa1_5"
)

// Encrypted data types accepted from a response.
const (
	TypeAssertion = "assertion"
	TypeResponse  = "response"
)

// Errors returned by the key ring.
var (
	ErrBlankTenant         = errors.New("tenant id is required")
	ErrBlankVersion        = errors.New("key version is required")
	ErrKeyLimit            = errors.New("key ring limit reached for this tenant")
	ErrDuplicateKey        = errors.New("key version already exists for this tenant")
	ErrAlgorithmForbidden  = errors.New("key transport algorithm is forbidden")
	ErrAlgorithmNotAllowed = errors.New("key transport algorithm is not in the allow list")
	ErrUnknownKey          = errors.New("unknown key version for this tenant")
	ErrKeyRetired          = errors.New("key version is retired")
	ErrRecipientMismatch   = errors.New("assertion recipient does not match the key recipient")
	ErrBadEnvelope         = errors.New("encrypted assertion envelope is invalid")
	ErrUnsupportedType     = errors.New("encrypted data type is not supported")
	ErrNestedTooDeep       = errors.New("nested encryption exceeds the depth limit")
	ErrDecryptFailed       = errors.New("assertion could not be decrypted")
	ErrPayloadTooLarge     = errors.New("decrypted assertion exceeds the size limit")
	ErrTenantMismatch      = errors.New("assertion tenant does not match the key ring tenant")
	ErrBadPayload          = errors.New("decrypted assertion payload is invalid")
	ErrBadSnapshot         = errors.New("snapshot payload is invalid")
)

// Version labels one key generation of a tenant.
type Version string

// KeyEntry is one private key generation held for a tenant.
type KeyEntry struct {
	Version    Version `json:"version"`
	Recipient  string  `json:"recipient"`
	Algorithm  string  `json:"algorithm"`
	PrivateKey []byte  `json:"private_key"`
	Retired    bool    `json:"retired"`
}

// Limits bounds the key ring and one unwrap.
type Limits struct {
	MaxKeysPerTenant int
	MaxNestedDepth   int
	MaxPayloadBytes  int
	MaxInflight      int
}

// Decrypter unwraps the ciphertext of one envelope with the given key. It is
// provided by the HSM or KMS that holds the private key material.
type Decrypter interface {
	Unwrap(k KeyEntry, ciphertext []byte) ([]byte, error)
}

// Envelope is the JSON body posted by the identity provider.
type Envelope struct {
	Type       string `json:"type"`
	Recipient  string `json:"recipient"`
	Algorithm  string `json:"algorithm"`
	KeyVersion string `json:"key_version"`
	Depth      int    `json:"depth"`
	Ciphertext []byte `json:"ciphertext"`
}

// Assertion is the decrypted and validated assertion.
type Assertion struct {
	TenantID   string   `json:"tenant_id"`
	Recipient  string   `json:"recipient"`
	Subject    string   `json:"subject"`
	IssuedAt   int64    `json:"issued_at"`
	Attributes []string `json:"attributes"`
	KeyVersion Version  `json:"key_version"`
}

// EncryptedAssertionKeyring owns the per tenant generations, the retirement
// tombstones and the algorithm allow list.
type EncryptedAssertionKeyring struct {
	mu     sync.Mutex
	dec    Decrypter
	limits Limits
}

// NewEncryptedAssertionKeyring builds a key ring on top of a decrypter. The
// default transport allow list holds rsa-oaep-256 and rsa-oaep-1.
func NewEncryptedAssertionKeyring(dec Decrypter, limits Limits) *EncryptedAssertionKeyring {
	return &EncryptedAssertionKeyring{dec: dec, limits: limits}
}

// AddKey installs one generation for a tenant and returns its version label.
func (k *EncryptedAssertionKeyring) AddKey(tenantID string, entry KeyEntry) (Version, error) {
	return "", errNotImplemented
}

// Retire marks a generation as retired so it stops accepting new responses.
func (k *EncryptedAssertionKeyring) Retire(tenantID, version string) error {
	return errNotImplemented
}

// Keys lists the generations of one tenant, oldest first, retired ones included.
func (k *EncryptedAssertionKeyring) Keys(tenantID string) ([]KeyEntry, error) {
	return nil, errNotImplemented
}

// Current reports the newest generation that still accepts new responses.
func (k *EncryptedAssertionKeyring) Current(tenantID string) (Version, error) {
	return "", errNotImplemented
}

// DecryptResponse parses one response envelope, picks the generation it needs
// and returns the decrypted assertion.
func (k *EncryptedAssertionKeyring) DecryptResponse(tenantID string, raw []byte) (Assertion, error) {
	return Assertion{}, errNotImplemented
}

// SetAlgorithmAllowlist replaces the accepted key transport algorithms.
func (k *EncryptedAssertionKeyring) SetAlgorithmAllowlist(algs []string) error {
	return errNotImplemented
}

// Purge drops every generation of one tenant.
func (k *EncryptedAssertionKeyring) Purge(tenantID string) error {
	return errNotImplemented
}

// Count reports how many generations a tenant holds, retired ones included.
func (k *EncryptedAssertionKeyring) Count(tenantID string) int {
	return 0
}

// Snapshot serialises every generation, the tombstones and the allow list.
func (k *EncryptedAssertionKeyring) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole key ring with a snapshot, all or nothing.
func (k *EncryptedAssertionKeyring) Restore(data []byte) error {
	return errNotImplemented
}
