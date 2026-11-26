// Package task implements the authorization endpoint side of JWT secured
// authorization requests: signed request object parsing, signature and claim
// validation, parameter merging and single use jti handling.
package task

import (
	"crypto/ed25519"
	"errors"
	"net/url"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Signing algorithms the guard recognises. Only asymmetric algorithms are
// accepted on the wire.
const (
	AlgEdDSA = "EdDSA"
	AlgES256 = "ES256"
	AlgRS256 = "RS256"
	AlgHS256 = "HS256"
	AlgNone  = "none"
)

// Errors reported by the guard.
var (
	ErrMalformed         = errors.New("request object is malformed")
	ErrObjectTooLarge    = errors.New("request object exceeds the configured size")
	ErrAlgNotAllowed     = errors.New("signing algorithm is not allowed")
	ErrUnknownClient     = errors.New("client is not registered")
	ErrUnknownKey        = errors.New("kid is not registered for this client")
	ErrBadSignature      = errors.New("signature does not verify")
	ErrIssuerMismatch    = errors.New("iss must equal client_id")
	ErrAudienceMismatch  = errors.New("aud must contain this authorization endpoint")
	ErrExpired           = errors.New("request object is outside the accepted time window")
	ErrParamConflict     = errors.New("query parameter conflicts with the request object")
	ErrRequestConflict   = errors.New("request and request_uri must not both be present")
	ErrJTReused          = errors.New("jti has already been used")
	ErrRequestURIUsed    = errors.New("request_uri has already been consumed")
	ErrRequestURIExpired = errors.New("request_uri has expired")
	ErrInFlightLimit     = errors.New("in flight request limit reached for this client")
	ErrBadSnapshot       = errors.New("snapshot payload is invalid")
)

// Config bounds the guard. Now is injectable so that windows can be driven by a
// test clock.
type Config struct {
	Endpoint             string
	MaxObjectBytes       int
	MaxParams            int
	MaxInFlightPerClient int
	MaxTokenAge          time.Duration
	JTITTL               time.Duration
	Now                  func() time.Time
}

// ClientKey is one registered verification key of a client. Grace marks a key
// that is still accepted while a rotation is settling.
type ClientKey struct {
	KID       string
	PublicKey ed25519.PublicKey
	NotAfter  time.Time
	Grace     bool
}

// Client is one registered OAuth client.
type Client struct {
	ClientID string
	Algs     []string
	Keys     []ClientKey
}

// RequestObject is one parsed JWS request object.
type RequestObject struct {
	Header       map[string]any
	Claims       map[string]any
	SigningInput string
	Signature    []byte
}

// AuthRequest is the merged authorization request.
type AuthRequest struct {
	ClientID     string
	ResponseType string
	RedirectURI  string
	Scope        string
	State        string
	Nonce        string
	Extra        map[string]string
}

// RequestObjectGuard owns the client registry, the jti tombstones and the
// request_uri store.
type RequestObjectGuard struct {
	cfg Config
}

// NewRequestObjectGuard builds a guard with the given caps.
func NewRequestObjectGuard(cfg Config) *RequestObjectGuard {
	return &RequestObjectGuard{cfg: cfg}
}

// RegisterClient adds or replaces one client registration.
func (g *RequestObjectGuard) RegisterClient(c Client) error {
	return errNotImplemented
}

// Client reads one client registration.
func (g *RequestObjectGuard) Client(clientID string) (Client, bool) {
	return Client{}, false
}

// RotateKey installs a new signing key and keeps the previous one usable for
// the grace period.
func (g *RequestObjectGuard) RotateKey(clientID, kid string, pub ed25519.PublicKey, grace time.Duration) error {
	return errNotImplemented
}

// Parse splits a compact JWS into header, claims, signing input and signature.
func (g *RequestObjectGuard) Parse(raw string) (RequestObject, error) {
	return RequestObject{}, errNotImplemented
}

// Verify checks the signature and the registered claims of one request object.
func (g *RequestObjectGuard) Verify(ro RequestObject, client Client) error {
	return errNotImplemented
}

// Merge combines the query parameters with the request object claims.
func (g *RequestObjectGuard) Merge(query url.Values, ro RequestObject) (AuthRequest, error) {
	return AuthRequest{}, errNotImplemented
}

// Consume burns one jti and leaves a tombstone behind.
func (g *RequestObjectGuard) Consume(jti string) error {
	return errNotImplemented
}

// Consumed reports whether a jti is already burnt.
func (g *RequestObjectGuard) Consumed(jti string) bool {
	return false
}

// StoreRequestURI keeps one request object behind a request_uri reference.
func (g *RequestObjectGuard) StoreRequestURI(uri, raw string, ttl time.Duration) error {
	return errNotImplemented
}

// ParseRequestURI resolves and consumes one request_uri reference.
func (g *RequestObjectGuard) ParseRequestURI(uri string) (RequestObject, error) {
	return RequestObject{}, errNotImplemented
}

// InFlight reports how many request objects of one client are being processed.
func (g *RequestObjectGuard) InFlight(clientID string) int {
	return 0
}

// Snapshot serialises the client registry, the jti tombstones and the
// request_uri store.
func (g *RequestObjectGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole guard state with a snapshot, all or nothing.
func (g *RequestObjectGuard) Restore(data []byte) error {
	return errNotImplemented
}

// Sweep drops expired tombstones and request_uri entries, returning how many
// records went away.
func (g *RequestObjectGuard) Sweep(now time.Time) int {
	return 0
}
