// Package task implements the authorization endpoint side of JWT secured
// authorization requests: signed request object parsing, signature and claim
// validation, parameter merging and single use jti handling.
package task

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Signing algorithms the guard recognises. Only asymmetric algorithms are
// accepted on the wire.
const (
	AlgEdDSA = "EdDSA"
	AlgES256 = "ES256"
	AlgRS256 = "RS256"
	AlgHS256 = "HS256"
	AlgNone  = "none"
)

const snapshotVersion = 1

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

type storedRequestURI struct {
	ClientID  string    `json:"client_id"`
	Raw       string    `json:"raw"`
	ExpiresAt time.Time `json:"expires_at"`
}

type snapshotPayload struct {
	Version     int                         `json:"version"`
	Clients     map[string]Client           `json:"clients"`
	JTIs        map[string]time.Time        `json:"jtis"`
	RequestURIs map[string]storedRequestURI `json:"request_uris"`
}

// RequestObjectGuard owns the client registry, the jti tombstones and the
// request_uri store.
type RequestObjectGuard struct {
	cfg Config

	mu       sync.RWMutex
	clients  map[string]Client
	jtis     map[string]time.Time
	uris     map[string]storedRequestURI
	inFlight map[string]int
}

// NewRequestObjectGuard builds a guard with the given caps.
func NewRequestObjectGuard(cfg Config) *RequestObjectGuard {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxObjectBytes <= 0 {
		cfg.MaxObjectBytes = 1 << 20
	}
	if cfg.MaxParams <= 0 {
		cfg.MaxParams = 128
	}
	if cfg.MaxInFlightPerClient <= 0 {
		cfg.MaxInFlightPerClient = 32
	}
	if cfg.MaxTokenAge <= 0 {
		cfg.MaxTokenAge = 10 * time.Minute
	}
	if cfg.JTITTL <= 0 {
		cfg.JTITTL = cfg.MaxTokenAge
	}
	return &RequestObjectGuard{
		cfg:      cfg,
		clients:  make(map[string]Client),
		jtis:     make(map[string]time.Time),
		uris:     make(map[string]storedRequestURI),
		inFlight: make(map[string]int),
	}
}

// RegisterClient adds or replaces one client registration.
func (g *RequestObjectGuard) RegisterClient(c Client) error {
	client := cloneClient(c)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.clients[client.ClientID] = client
	return nil
}

// Client reads one client registration.
func (g *RequestObjectGuard) Client(clientID string) (Client, bool) {
	g.mu.RLock()
	client, ok := g.clients[clientID]
	g.mu.RUnlock()
	if !ok {
		return Client{}, false
	}
	return cloneClient(client), true
}

// RotateKey installs a new signing key and keeps the previous one usable for
// the grace period.
func (g *RequestObjectGuard) RotateKey(clientID, kid string, pub ed25519.PublicKey, grace time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	client, ok := g.clients[clientID]
	if !ok {
		return ErrUnknownClient
	}

	now := g.cfg.Now()
	for i := range client.Keys {
		if client.Keys[i].Grace {
			continue
		}
		client.Keys[i].Grace = true
		client.Keys[i].NotAfter = now.Add(grace)
	}
	client.Keys = append(client.Keys, ClientKey{
		KID:       kid,
		PublicKey: clonePublicKey(pub),
	})
	g.clients[clientID] = client
	return nil
}

// Parse splits a compact JWS into header, claims, signing input and signature.
func (g *RequestObjectGuard) Parse(raw string) (RequestObject, error) {
	if len(raw) > g.cfg.MaxObjectBytes {
		return RequestObject{}, ErrObjectTooLarge
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return RequestObject{}, ErrMalformed
	}

	headerBytes, err := decodeBase64Segment(parts[0])
	if err != nil {
		return RequestObject{}, fmt.Errorf("%w: decode header: %v", ErrMalformed, err)
	}
	claimsBytes, err := decodeBase64Segment(parts[1])
	if err != nil {
		return RequestObject{}, fmt.Errorf("%w: decode claims: %v", ErrMalformed, err)
	}
	signature, err := decodeBase64Segment(parts[2])
	if err != nil {
		return RequestObject{}, fmt.Errorf("%w: decode signature: %v", ErrMalformed, err)
	}

	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return RequestObject{}, fmt.Errorf("%w: header JSON: %v", ErrMalformed, err)
	}
	if header == nil {
		header = make(map[string]any)
	}

	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return RequestObject{}, fmt.Errorf("%w: claims JSON: %v", ErrMalformed, err)
	}
	if claims == nil {
		claims = make(map[string]any)
	}

	return RequestObject{
		Header:       header,
		Claims:       claims,
		SigningInput: parts[0] + "." + parts[1],
		Signature:    signature,
	}, nil
}

// Verify checks the signature and the registered claims of one request object.
func (g *RequestObjectGuard) Verify(ro RequestObject, client Client) error {
	alg, _ := ro.Header["alg"].(string)
	if !isAllowedAlgorithm(alg, client.Algs) {
		return ErrAlgNotAllowed
	}

	kid, _ := ro.Header["kid"].(string)
	now := g.cfg.Now()
	var verificationKeys []ed25519.PublicKey
	for _, key := range client.Keys {
		if key.KID != kid {
			continue
		}
		if !key.NotAfter.IsZero() && !now.Before(key.NotAfter) {
			continue
		}
		if len(key.PublicKey) == ed25519.PublicKeySize {
			verificationKeys = append(verificationKeys, key.PublicKey)
		}
	}
	if len(verificationKeys) == 0 {
		return ErrUnknownKey
	}

	if len(ro.Signature) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	signingInput := []byte(ro.SigningInput)
	signatureValid := false
	for _, pub := range verificationKeys {
		if ed25519.Verify(pub, signingInput, ro.Signature) {
			signatureValid = true
			break
		}
	}
	if !signatureValid {
		return ErrBadSignature
	}

	issuer, _ := ro.Claims["iss"].(string)
	if issuer != client.ClientID {
		return ErrIssuerMismatch
	}
	if !audienceContains(ro.Claims["aud"], g.cfg.Endpoint) {
		return ErrAudienceMismatch
	}
	if err := validateTimeClaims(ro.Claims, now, g.cfg.MaxTokenAge); err != nil {
		return err
	}

	jti, _ := ro.Claims["jti"].(string)
	if jti == "" {
		return ErrMalformed
	}
	return nil
}

// Merge combines the query parameters with the request object claims.
func (g *RequestObjectGuard) Merge(query url.Values, ro RequestObject) (AuthRequest, error) {
	if _, hasRequest := query["request"]; hasRequest {
		if _, hasRequestURI := query["request_uri"]; hasRequestURI {
			return AuthRequest{}, ErrRequestConflict
		}
	}

	merged := make(map[string]string)
	for key, values := range query {
		value := ""
		if len(values) > 0 {
			value = values[0]
		}
		merged[key] = value
	}

	for key, rawValue := range ro.Claims {
		value := claimString(rawValue)
		if existing, exists := merged[key]; exists && existing != value {
			return AuthRequest{}, fmt.Errorf("%w: %s", ErrParamConflict, key)
		}
		// Assign even for nil so an explicit JSON null remains present and does
		// not fall back to a query value on a later merge.
		merged[key] = value
	}

	if len(merged) > g.cfg.MaxParams {
		return AuthRequest{}, fmt.Errorf("%w: too many parameters", ErrMalformed)
	}

	request := AuthRequest{Extra: make(map[string]string)}
	for key, value := range merged {
		switch key {
		case "client_id":
			request.ClientID = value
		case "response_type":
			request.ResponseType = value
		case "redirect_uri":
			request.RedirectURI = value
		case "scope":
			request.Scope = value
		case "state":
			request.State = value
		case "nonce":
			request.Nonce = value
		default:
			request.Extra[key] = value
		}
	}
	return request, nil
}

// Consume burns one jti and leaves a tombstone behind.
func (g *RequestObjectGuard) Consume(jti string) error {
	if jti == "" {
		return ErrMalformed
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, used := g.jtis[jti]; used {
		return ErrJTReused
	}
	g.jtis[jti] = g.cfg.Now().Add(g.cfg.JTITTL)
	return nil
}

// Consumed reports whether a jti is already burnt. It intentionally does not
// sweep expired tombstones.
func (g *RequestObjectGuard) Consumed(jti string) bool {
	if jti == "" {
		return false
	}

	g.mu.RLock()
	_, used := g.jtis[jti]
	g.mu.RUnlock()
	return used
}

// StoreRequestURI keeps one request object behind a request_uri reference.
func (g *RequestObjectGuard) StoreRequestURI(uri, raw string, ttl time.Duration) error {
	clientID := clientIDFromRawRequest(raw)

	expiresAt := g.cfg.Now().Add(ttl)

	g.mu.Lock()
	defer g.mu.Unlock()

	if existing, exists := g.uris[uri]; exists {
		switch {
		case existing.ClientID == clientID:
			// Replacing the payload keeps the same single in-flight slot.
		case g.inFlight[clientID] >= g.cfg.MaxInFlightPerClient:
			return ErrInFlightLimit
		default:
			g.decrementInFlightLocked(existing.ClientID)
			g.inFlight[clientID]++
		}
	} else {
		if g.inFlight[clientID] >= g.cfg.MaxInFlightPerClient {
			return ErrInFlightLimit
		}
		g.inFlight[clientID]++
	}

	g.uris[uri] = storedRequestURI{
		ClientID:  clientID,
		Raw:       raw,
		ExpiresAt: expiresAt,
	}
	return nil
}

func clientIDFromRawRequest(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := decodeBase64Segment(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ClientID string `json:"client_id"`
		Iss      string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.ClientID != "" {
		return claims.ClientID
	}
	return claims.Iss
}

// ParseRequestURI resolves and consumes one request_uri reference.
func (g *RequestObjectGuard) ParseRequestURI(uri string) (RequestObject, error) {
	g.mu.Lock()
	entry, ok := g.uris[uri]
	if !ok {
		g.mu.Unlock()
		return RequestObject{}, reusedOrExpiredURIError()
	}

	now := g.cfg.Now()
	if !entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt) {
		delete(g.uris, uri)
		g.decrementInFlightLocked(entry.ClientID)
		g.mu.Unlock()
		return RequestObject{}, ErrRequestURIExpired
	}

	// Consumption is atomic under the write lock. The payload is removed so a
	// repeat resolution cannot expand it again.
	delete(g.uris, uri)
	g.decrementInFlightLocked(entry.ClientID)
	raw := entry.Raw
	g.mu.Unlock()

	return g.Parse(raw)
}

// InFlight reports how many request objects of one client are being processed.
func (g *RequestObjectGuard) InFlight(clientID string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.inFlight[clientID]
}

// BeginInFlight reserves one per-client processing slot. It is an explicit
// lifecycle hook for HTTP or gateway handlers that do not store a request_uri.
func (g *RequestObjectGuard) BeginInFlight(clientID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight[clientID] >= g.cfg.MaxInFlightPerClient {
		return ErrInFlightLimit
	}
	g.inFlight[clientID]++
	return nil
}

// FinishInFlight releases one slot reserved by BeginInFlight.
func (g *RequestObjectGuard) FinishInFlight(clientID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.decrementInFlightLocked(clientID)
}

// Sweep drops expired tombstones and request_uri entries, returning how many
// records went away.
func (g *RequestObjectGuard) Sweep(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	removed := 0
	for jti, expiresAt := range g.jtis {
		if !expiresAt.After(now) {
			delete(g.jtis, jti)
			removed++
		}
	}
	for uri, entry := range g.uris {
		if !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(now) {
			delete(g.uris, uri)
			g.decrementInFlightLocked(entry.ClientID)
			removed++
		}
	}
	return removed
}

// Snapshot serialises the client registry, the jti tombstones and the
// request_uri store.
func (g *RequestObjectGuard) Snapshot() ([]byte, error) {
	g.mu.RLock()
	payload := snapshotPayload{
		Version:     snapshotVersion,
		Clients:     cloneClientsMap(g.clients),
		JTIs:        make(map[string]time.Time, len(g.jtis)),
		RequestURIs: make(map[string]storedRequestURI, len(g.uris)),
	}
	for jti, expiresAt := range g.jtis {
		payload.JTIs[jti] = expiresAt
	}
	for uri, entry := range g.uris {
		payload.RequestURIs[uri] = entry
	}
	g.mu.RUnlock()

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("snapshot guard state: %w", err)
	}
	return data, nil
}

// Restore replaces the whole guard state with a snapshot, all or nothing.
func (g *RequestObjectGuard) Restore(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if payload.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, payload.Version)
	}
	if payload.Clients == nil {
		payload.Clients = make(map[string]Client)
	}
	if payload.JTIs == nil {
		payload.JTIs = make(map[string]time.Time)
	}
	if payload.RequestURIs == nil {
		payload.RequestURIs = make(map[string]storedRequestURI)
	}
	clients := cloneClientsMap(payload.Clients)
	inFlight := make(map[string]int)
	for _, entry := range payload.RequestURIs {
		inFlight[entry.ClientID]++
	}

	g.mu.Lock()
	g.clients = clients
	g.jtis = payload.JTIs
	g.uris = payload.RequestURIs
	g.inFlight = inFlight
	g.mu.Unlock()
	return nil
}

func (g *RequestObjectGuard) decrementInFlightLocked(clientID string) {
	if g.inFlight[clientID] <= 1 {
		delete(g.inFlight, clientID)
		return
	}
	g.inFlight[clientID]--
}

func reusedOrExpiredURIError() error {
	return fmt.Errorf("%w: %w", ErrRequestURIUsed, ErrRequestURIExpired)
}

func decodeBase64Segment(segment string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(segment)
}

func isAllowedAlgorithm(alg string, allowed []string) bool {
	if alg == "" {
		return false
	}
	normalized := strings.ToUpper(strings.TrimSpace(alg))
	if normalized == strings.ToUpper(AlgNone) || strings.HasPrefix(normalized, "HS") {
		return false
	}
	for _, candidate := range allowed {
		if candidate == alg {
			return true
		}
	}
	return false
}

func audienceContains(audience any, endpoint string) bool {
	switch aud := audience.(type) {
	case string:
		return aud == endpoint
	case []string:
		for _, item := range aud {
			if item == endpoint {
				return true
			}
		}
	case []any:
		for _, item := range aud {
			if value, ok := item.(string); ok && value == endpoint {
				return true
			}
		}
	}
	return false
}

func validateTimeClaims(claims map[string]any, now time.Time, maxAge time.Duration) error {
	issuedAt, ok := numericTime(claims["iat"])
	if !ok {
		issuedAt = time.Time{}
	}
	if maxAge > 0 && issuedAt.Before(now.Add(-maxAge)) {
		return ErrExpired
	}

	expiresAt, ok := numericTime(claims["exp"])
	if !ok || !now.Before(expiresAt) {
		return ErrExpired
	}
	return nil
}

func numericTime(value any) (time.Time, bool) {
	switch number := value.(type) {
	case float64:
		return time.Unix(int64(number), 0).UTC(), true
	case float32:
		return time.Unix(int64(number), 0).UTC(), true
	case int:
		return time.Unix(int64(number), 0).UTC(), true
	case int64:
		return time.Unix(number, 0).UTC(), true
	case json.Number:
		seconds, err := number.Int64()
		if err != nil {
			if floatSeconds, floatErr := number.Float64(); floatErr == nil {
				seconds = int64(floatSeconds)
			} else {
				return time.Time{}, false
			}
		}
		return time.Unix(seconds, 0).UTC(), true
	default:
		return time.Time{}, false
	}
}

func claimString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		return strconv.FormatBool(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

func cloneClientsMap(source map[string]Client) map[string]Client {
	clients := make(map[string]Client, len(source))
	for clientID, client := range source {
		clients[clientID] = cloneClient(client)
	}
	return clients
}

func cloneClient(client Client) Client {
	cloned := client
	cloned.Algs = append([]string(nil), client.Algs...)
	cloned.Keys = make([]ClientKey, len(client.Keys))
	for i, key := range client.Keys {
		cloned.Keys[i] = key
		cloned.Keys[i].PublicKey = clonePublicKey(key.PublicKey)
	}
	return cloned
}

func clonePublicKey(publicKey ed25519.PublicKey) ed25519.PublicKey {
	if publicKey == nil {
		return nil
	}
	return append(ed25519.PublicKey(nil), publicKey...)
}
