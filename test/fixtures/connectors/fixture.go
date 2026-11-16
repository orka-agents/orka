// Package connectorsfixture is the deterministic stand-in for everything the
// connectors E2E needs outside Orka: an OIDC issuer that signs in a person, an
// OAuth provider that consents, issues short-lived tokens, refreshes and
// revokes them, a resource API that only honours those tokens, and an
// OpenAI-compatible model that drives a read, an approval-gated write, and a
// second read. Never expose it as a real service.
//
//nolint:goconst,lll // Keep the fixed OAuth, JSON, and model wire fixtures readable.
package connectorsfixture

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Config sets the fixture's identities.
type Config struct {
	// Issuer is the exact OIDC issuer URL Orka is configured with.
	Issuer string
	// Audience is the OIDC audience Orka expects.
	Audience string
	// ClientID and ClientSecret are the OAuth client Orka registers.
	ClientID     string
	ClientSecret string
	// AccessTokenTTL bounds every issued access token; short so refresh
	// happens within one Task.
	AccessTokenTTL time.Duration
	// ModelCredential is the bearer the model endpoint expects.
	ModelCredential string
	Now             func() time.Time
}

// signingAlgorithm is the only JWT algorithm the fixture signs with.
const signingAlgorithm = "RS256"

// Fixture is the shared state behind both listeners.
type Fixture struct {
	cfg Config
	key *rsa.PrivateKey
	kid string

	mu       sync.Mutex
	codes    map[string]authorizationCode
	tokens   map[string]issuedToken
	refresh  map[string]string // refresh token -> access token it pairs with
	revoked  []string
	counters Counters
}

type authorizationCode struct {
	challenge   string
	redirectURI string
	scope       string
	expires     time.Time
}

type issuedToken struct {
	expires time.Time
	scope   string
	refresh string
}

// Counters is the observable state assertions read.
type Counters struct {
	Authorizations   int      `json:"authorizations"`
	CodeExchanges    int      `json:"codeExchanges"`
	Refreshes        int      `json:"refreshes"`
	Revocations      int      `json:"revocations"`
	Reads            int      `json:"reads"`
	Writes           int      `json:"writes"`
	Rejected         int      `json:"rejected"`
	DistinctBearers  int      `json:"distinctBearers"`
	ModelTurns       int      `json:"modelTurns"`
	LastWriteTitle   string   `json:"lastWriteTitle"`
	BearerDigests    []string `json:"-"`
	TokensIssued     int      `json:"tokensIssued"`
	RevokedRefreshes int      `json:"revokedRefreshes"`
	// LastAuthorizedScope is the scope set of the last authorization a
	// code was issued for, sorted and space-separated, so the lane can
	// assert that Orka asked for exactly what the mode needs.
	LastAuthorizedScope string `json:"lastAuthorizedScope"`
}

// knownScopes are the only scopes the fixture provider offers; an
// authorization asking for anything else is refused rather than granted.
var knownScopes = map[string]struct{}{"items:read": {}, "items:write": {}}

// normalizedScopes returns a space-separated scope sorted and without
// duplicates, and whether every entry is a scope the fixture offers.
func normalizedScopes(scope string) (string, bool) {
	fields := strings.Fields(scope)
	slices.Sort(fields)
	fields = slices.Compact(fields)
	for _, field := range fields {
		if _, known := knownScopes[field]; !known {
			return "", false
		}
	}
	return strings.Join(fields, " "), true
}

// New builds a fixture with a fresh signing key.
func New(cfg Config) (*Fixture, error) {
	if cfg.Issuer == "" || cfg.Audience == "" || cfg.ClientID == "" || cfg.ClientSecret == "" ||
		cfg.ModelCredential == "" {
		return nil, errors.New("issuer, audience, client id, client secret, and model credential are required")
	}
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = 90 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Fixture{
		cfg: cfg, key: key, kid: randomToken(8),
		codes: map[string]authorizationCode{}, tokens: map[string]issuedToken{}, refresh: map[string]string{},
	}, nil
}

// PlainHandler serves what may travel over plain HTTP inside the cluster:
// the OIDC issuer, the model, and the observable state.
func (f *Fixture) PlainHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /oidc/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /oidc/jwks", f.jwks)
	mux.HandleFunc("POST /oidc/mint", f.mint)
	mux.HandleFunc("GET /fixture/state", f.state)
	mux.HandleFunc("POST /v1/chat/completions", f.model)
	mux.HandleFunc("POST /v1/responses", f.model)
	return mux
}

// TLSHandler serves what Orka only talks to over HTTPS: the OAuth provider
// and the resource API the linked token is used against.
func (f *Fixture) TLSHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/authorize", f.authorize)
	mux.HandleFunc("POST /oauth/token", f.token)
	mux.HandleFunc("POST /oauth/revoke", f.revoke)
	mux.HandleFunc("GET /api/items", f.readItems)
	mux.HandleFunc("POST /api/items", f.writeItem)
	return mux
}

// ---- OIDC issuer ----

func (f *Fixture) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": f.cfg.Issuer, "jwks_uri": strings.TrimRight(f.cfg.Issuer, "/") + "/jwks",
		"id_token_signing_alg_values_supported": []string{signingAlgorithm},
	})
}

func (f *Fixture) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := f.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": signingAlgorithm, "kid": f.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// mint signs a token for the requested subject. It is the fixture's stand-in
// for a person signing in; nothing authenticates the call.
func (f *Fixture) mint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject string `json:"subject"`
		Email   string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Subject) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "subject is required"})
		return
	}
	now := f.cfg.Now()
	token, err := f.sign(map[string]any{
		"iss": f.cfg.Issuer, "sub": req.Subject, "aud": f.cfg.Audience, "email": req.Email,
		"preferred_username": req.Subject, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (f *Fixture) sign(claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": signingAlgorithm, "typ": "JWT", "kid": f.kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// ---- OAuth provider ----

func (f *Fixture) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.cfg.ClientID || q.Get("response_type") != "code" || q.Get("redirect_uri") == "" || q.Get("state") == "" {
		http.Error(w, "invalid authorize request", http.StatusBadRequest)
		return
	}
	// PKCE S256 is the advertised flow; an authorization without it must
	// fail the lane rather than silently issue a code.
	if strings.TrimSpace(q.Get("code_challenge")) == "" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "code_challenge with code_challenge_method=S256 is required", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	target := redirect.Query()
	target.Set("state", q.Get("state"))
	f.mu.Lock()
	f.counters.Authorizations++
	scope, known := normalizedScopes(q.Get("scope"))
	switch {
	case q.Get("fixture_decision") == "deny":
		target.Set("error", "access_denied")
	case !known:
		// RFC 6749 §4.1.2.1: a consent that asks for a privilege the
		// provider does not offer fails instead of being granted.
		target.Set("error", "invalid_scope")
	default:
		code := randomToken(24)
		f.codes[code] = authorizationCode{
			challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), scope: scope, expires: f.cfg.Now().Add(5 * time.Minute),
		}
		f.counters.LastAuthorizedScope = scope
		target.Set("code", code)
	}
	f.mu.Unlock()
	redirect.RawQuery = target.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *Fixture) clientAuthenticated(r *http.Request) bool {
	if user, pass, ok := r.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(user), []byte(f.cfg.ClientID)) == 1 && subtle.ConstantTimeCompare([]byte(pass), []byte(f.cfg.ClientSecret)) == 1
	}
	return subtle.ConstantTimeCompare([]byte(r.PostFormValue("client_id")), []byte(f.cfg.ClientID)) == 1 &&
		subtle.ConstantTimeCompare([]byte(r.PostFormValue("client_secret")), []byte(f.cfg.ClientSecret)) == 1
}

func (f *Fixture) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !f.clientAuthenticated(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.cfg.Now()
	var scope string
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		code, ok := f.codes[r.PostFormValue("code")]
		delete(f.codes, r.PostFormValue("code"))
		if !ok || now.After(code.expires) || code.redirectURI != r.PostFormValue("redirect_uri") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		if code.challenge != "" {
			sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
				return
			}
		}
		scope = code.scope
		f.counters.CodeExchanges++
	case "refresh_token":
		access, ok := f.refresh[r.PostFormValue("refresh_token")]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		scope = f.tokens[access].scope
		delete(f.refresh, r.PostFormValue("refresh_token"))
		delete(f.tokens, access)
		f.counters.Refreshes++
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	access, refresh := "fx-access-"+randomToken(18), "fx-refresh-"+randomToken(18)
	f.tokens[access] = issuedToken{expires: now.Add(f.cfg.AccessTokenTTL), scope: scope, refresh: refresh}
	f.refresh[refresh] = access
	f.counters.TokensIssued++
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(f.cfg.AccessTokenTTL.Seconds()),
		"refresh_token": refresh, "scope": scope,
	})
}

func (f *Fixture) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !f.clientAuthenticated(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	token := r.PostFormValue("token")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters.Revocations++
	if access, ok := f.refresh[token]; ok {
		delete(f.refresh, token)
		delete(f.tokens, access)
		f.counters.RevokedRefreshes++
	} else if issued, ok := f.tokens[token]; ok {
		delete(f.refresh, issued.refresh)
		delete(f.tokens, token)
	}
	f.revoked = append(f.revoked, digest(token))
	w.WriteHeader(http.StatusOK)
}

// ---- resource API ----

// bearerAccepted reports whether the request carries a live access token
// and returns that token's granted scope. The resource API enforces the
// scope, so a consent that stopped asking for items:write would fail the
// lane's write rather than pass unnoticed.
func (f *Fixture) bearerAccepted(r *http.Request) (string, bool) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer == "" || bearer == r.Header.Get("Authorization") {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	issued, ok := f.tokens[bearer]
	if !ok || f.cfg.Now().After(issued.expires) {
		f.counters.Rejected++
		return "", false
	}
	seen := false
	for _, d := range f.counters.BearerDigests {
		if d == digest(bearer) {
			seen = true
		}
	}
	if !seen {
		f.counters.BearerDigests = append(f.counters.BearerDigests, digest(bearer))
		f.counters.DistinctBearers++
	}
	return issued.scope, true
}

// hasScope reports whether a space-separated granted scope carries want.
func hasScope(granted, want string) bool {
	return slices.Contains(strings.Fields(granted), want)
}

// scopeRefused records a bearer that is live but not granted the scope.
func (f *Fixture) scopeRefused(w http.ResponseWriter, want string) {
	f.mu.Lock()
	f.counters.Rejected++
	f.mu.Unlock()
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "insufficient_scope", "required": want})
}

func (f *Fixture) readItems(w http.ResponseWriter, r *http.Request) {
	scope, ok := f.bearerAccepted(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad or expired bearer"})
		return
	}
	if !hasScope(scope, "items:read") {
		f.scopeRefused(w, "items:read")
		return
	}
	f.mu.Lock()
	f.counters.Reads++
	reads := f.counters.Reads
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{{"id": 1, "title": "first item"}}, "read": reads})
}

func (f *Fixture) writeItem(w http.ResponseWriter, r *http.Request) {
	scope, ok := f.bearerAccepted(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad or expired bearer"})
		return
	}
	if !hasScope(scope, "items:write") {
		f.scopeRefused(w, "items:write")
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	f.mu.Lock()
	f.counters.Writes++
	f.counters.LastWriteTitle = body.Title
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": 2, "title": body.Title, "status": "created"})
}

func (f *Fixture) state(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, http.StatusOK, f.counters)
}

// Snapshot returns the counters for in-process tests.
func (f *Fixture) Snapshot() Counters {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counters
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Errorf("fixture randomness unavailable: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
