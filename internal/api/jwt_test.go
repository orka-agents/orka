/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
)

const (
	testOIDCDiscoveryPath   = "/.well-known/openid-configuration"
	testJWKSURIJSONKey      = "jwks_uri"
	testJWKSKeysJSONKey     = "keys"
	testJWTAlgorithmJSONKey = "alg"
	testJWTTypeJSONKey      = "typ"
	testJWTTypeValue        = "JWT"
	testJWTIssuerJSONKey    = "iss"
	testJWTSubjectJSONKey   = "sub"
	testJWTAudienceJSONKey  = "aud"
	testJWTExpiryJSONKey    = "exp"
	testJWKSPathA           = "/jwks-a"
	testJWKSPathB           = "/jwks-b"
	testOIDCAudience        = "orka-api"
)

type cacheTestOIDCProvider struct {
	server *httptest.Server
	aud    string

	mu               sync.RWMutex
	key              *rsa.PrivateKey
	kid              string
	jwksStatus       int
	jwksPadding      int
	jwksEmpty        bool
	jwksStarted      chan<- struct{}
	jwksRelease      <-chan struct{}
	discoveryStatus  int
	discoveryPadding int
	discoveryStarted chan<- struct{}
	discoveryRelease <-chan struct{}

	jwksHits      atomic.Int64
	discoveryHits atomic.Int64
}

type cacheTestClock struct {
	mu  sync.Mutex
	now time.Time
}

type cacheTestBlockingClock struct {
	now     time.Time
	blockAt int64
	calls   atomic.Int64
	started chan<- struct{}
	release <-chan struct{}
}

func (c *cacheTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *cacheTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *cacheTestBlockingClock) Now() time.Time {
	if c.calls.Add(1) == c.blockAt {
		c.started <- struct{}{}
		<-c.release
	}
	return c.now
}

func newCacheTestOIDCProvider(t *testing.T) *cacheTestOIDCProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	provider := &cacheTestOIDCProvider{
		aud: testOIDCAudience,
		key: key,
		kid: "cache-key-1",
	}
	provider.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, testOIDCDiscoveryPath) {
			provider.discoveryHits.Add(1)
			provider.mu.RLock()
			status := provider.discoveryStatus
			padding := provider.discoveryPadding
			started := provider.discoveryStarted
			release := provider.discoveryRelease
			provider.mu.RUnlock()
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			if release != nil {
				<-release
			}
			if status != 0 {
				http.Error(w, http.StatusText(status), status)
				return
			}
			response := map[string]any{testJWKSURIJSONKey: provider.server.URL + "/jwks"}
			if padding > 0 {
				response["padding"] = strings.Repeat("x", padding)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}

		provider.jwksHits.Add(1)

		provider.mu.RLock()
		key := &provider.key.PublicKey
		kid := provider.kid
		status := provider.jwksStatus
		padding := provider.jwksPadding
		empty := provider.jwksEmpty
		started := provider.jwksStarted
		release := provider.jwksRelease
		provider.mu.RUnlock()

		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if release != nil {
			<-release
		}
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}

		keys := []testJWKKey{testJWKFromPublicKey(key, kid)}
		if empty {
			keys = []testJWKKey{}
		}
		response := map[string]any{testJWKSKeysJSONKey: keys}
		if padding > 0 {
			response["padding"] = strings.Repeat("x", padding)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *cacheTestOIDCProvider) snapshot() *testOIDCProvider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return &testOIDCProvider{
		server: p.server,
		key:    p.key,
		kid:    p.kid,
		aud:    p.aud,
	}
}

func (p *cacheTestOIDCProvider) config() OIDCConfig {
	return p.snapshot().config()
}

func (p *cacheTestOIDCProvider) configWithoutJWKSURL() OIDCConfig {
	return p.snapshot().configWithoutJWKSURL()
}

func (p *cacheTestOIDCProvider) issueToken(t *testing.T, opts testOIDCTokenOptions) string {
	t.Helper()
	return p.snapshot().issueToken(t, opts)
}

func (p *cacheTestOIDCProvider) blockJWKS() (<-chan struct{}, func()) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	p.mu.Lock()
	p.jwksStarted = started
	p.jwksRelease = release
	p.mu.Unlock()
	return started, func() { releaseOnce.Do(func() { close(release) }) }
}

func (p *cacheTestOIDCProvider) blockDiscovery() (<-chan struct{}, func()) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	p.mu.Lock()
	p.discoveryStarted = started
	p.discoveryRelease = release
	p.mu.Unlock()
	return started, func() { releaseOnce.Do(func() { close(release) }) }
}

func (p *cacheTestOIDCProvider) rotate(t *testing.T) {
	p.rotateWithKid(t, "cache-key-2")
}

func (p *cacheTestOIDCProvider) rotateWithKid(t *testing.T, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rotated RSA key: %v", err)
	}
	p.mu.Lock()
	p.key = key
	p.kid = kid
	p.mu.Unlock()
}

func (p *cacheTestOIDCProvider) setJWKSStatus(status int) {
	p.mu.Lock()
	p.jwksStatus = status
	p.mu.Unlock()
}

func (p *cacheTestOIDCProvider) setJWKSPadding(size int) {
	p.mu.Lock()
	p.jwksPadding = size
	p.mu.Unlock()
}

func (p *cacheTestOIDCProvider) setJWKSEmpty(empty bool) {
	p.mu.Lock()
	p.jwksEmpty = empty
	p.mu.Unlock()
}

func (p *cacheTestOIDCProvider) setDiscoveryStatus(status int) {
	p.mu.Lock()
	p.discoveryStatus = status
	p.mu.Unlock()
}

func (p *cacheTestOIDCProvider) setDiscoveryPadding(size int) {
	p.mu.Lock()
	p.discoveryPadding = size
	p.mu.Unlock()
}

func issueES256Token(t *testing.T, issuer, audience string, key *ecdsa.PrivateKey) string {
	t.Helper()
	headerPart := mustBase64JSON(t, map[string]any{
		testJWTAlgorithmJSONKey: "ES256",
		testJWTTypeJSONKey:      testJWTTypeValue,
	})
	claimsPart := mustBase64JSON(t, map[string]any{
		testJWTIssuerJSONKey:   issuer,
		testJWTSubjectJSONKey:  "user-123",
		testJWTAudienceJSONKey: audience,
		testJWTExpiryJSONKey:   time.Now().Add(time.Hour).Unix(),
	})
	signedContent := headerPart + "." + claimsPart
	digest := sha256.Sum256([]byte(signedContent))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("failed to sign ES256 JWT: %v", err)
	}
	const coordinateSize = 32
	signature := make([]byte, 2*coordinateSize)
	r.FillBytes(signature[:coordinateSize])
	s.FillBytes(signature[coordinateSize:])
	return signedContent + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func marshalPublicJWKS(t *testing.T, rawKey any, alg jwa.SignatureAlgorithm) []byte {
	t.Helper()
	key, err := jwk.Import(rawKey)
	if err != nil {
		t.Fatalf("import JWK: %v", err)
	}
	if err := key.Set(jwk.AlgorithmKey, alg); err != nil {
		t.Fatalf("set JWK algorithm: %v", err)
	}
	if err := key.Set(jwk.KeyUsageKey, string(jwk.ForSignature)); err != nil {
		t.Fatalf("set JWK usage: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(key); err != nil {
		t.Fatalf("add JWK: %v", err)
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return body
}

func TestVerifyJWT_MissingJWKSURL(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{})

	_, err := verifyJWT(context.Background(), token, jwtVerificationConfig{
		Issuer:   provider.server.URL,
		Audience: provider.aud,
	})
	if err == nil || !strings.Contains(err.Error(), "missing JWKS URL") {
		t.Fatalf("verifyJWT error = %v, want missing JWKS URL error", err)
	}
}

func TestValidateOIDCToken_RepeatedInvalidSignaturesReuseJWKS(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	token := tamperJWTSignature(t, provider.issueToken(t, testOIDCTokenOptions{}))

	for i := range 8 {
		_, err := validateOIDCToken(context.Background(), token, provider.config())
		if err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("validateOIDCToken attempt %d error = %v, want signature error", i, err)
		}
	}

	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1 for repeated invalid signatures", got)
	}
}

func TestValidateOIDCToken_DiscoveryCachedForRepeatedInvalidSignatures(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	token := tamperJWTSignature(t, provider.issueToken(t, testOIDCTokenOptions{}))

	for i := range 8 {
		_, err := validateOIDCToken(context.Background(), token, provider.configWithoutJWKSURL())
		if err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("validateOIDCToken attempt %d error = %v, want signature error", i, err)
		}
	}

	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches = %d, want 1", got)
	}
	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1", got)
	}
}

func TestValidateOIDCToken_ConcurrentInvalidSignaturesCoalesceDiscoveryFetch(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	started, release := provider.blockDiscovery()
	defer release()
	token := tamperJWTSignature(t, provider.issueToken(t, testOIDCTokenOptions{}))

	const requests = 32
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			_, err := validateOIDCToken(context.Background(), token, provider.configWithoutJWKSURL())
			errs <- err
		})
	}
	close(start)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OIDC discovery fetch")
	}
	release()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("validateOIDCToken error = %v, want signature error", err)
		}
	}
	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches = %d, want 1 for %d concurrent invalid signatures", got, requests)
	}
	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1", got)
	}
}

func TestValidateOIDCToken_DiscoveryOutageUsesFreshCache(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{})
	cfg := provider.configWithoutJWKSURL()
	if _, err := validateOIDCToken(context.Background(), token, cfg); err != nil {
		t.Fatalf("warm discovery cache: %v", err)
	}

	provider.setDiscoveryStatus(http.StatusServiceUnavailable)
	if _, err := validateOIDCToken(context.Background(), token, cfg); err != nil {
		t.Fatalf("validate with fresh discovery cache during outage: %v", err)
	}
	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches = %d, want 1", got)
	}
}

func TestJWTVerifier_DiscoveryOutageFailsClosedAfterExpiry(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const (
		discoveryTTL = time.Minute
		retryDelay   = 5 * time.Second
	)
	verifier := newJWTVerifierWithCacheOptions(
		jwksCacheOptions{now: clock.Now},
		oidcDiscoveryCacheOptions{
			now:               clock.Now,
			ttl:               discoveryTTL,
			refreshRetryDelay: retryDelay,
		},
	)
	token := provider.issueToken(t, testOIDCTokenOptions{})
	cfg := provider.configWithoutJWKSURL()
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier); err != nil {
		t.Fatalf("warm discovery cache: %v", err)
	}

	provider.setDiscoveryStatus(http.StatusServiceUnavailable)
	clock.Advance(discoveryTTL + time.Second)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier); err == nil {
		t.Fatal("token unexpectedly validated after discovery cache expiry during outage")
	}
	if got := provider.discoveryHits.Load(); got != 2 {
		t.Fatalf("OIDC discovery fetches after expiry = %d, want 2", got)
	}

	provider.setDiscoveryStatus(0)
	clock.Advance(retryDelay + time.Second)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier); err != nil {
		t.Fatalf("validate after discovery endpoint recovery: %v", err)
	}
	if got := provider.discoveryHits.Load(); got != 3 {
		t.Fatalf("OIDC discovery fetches after recovery = %d, want 3", got)
	}
}

func TestValidateOIDCToken_RejectsOversizedDiscoveryDocument(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	provider.setDiscoveryPadding((64 << 10) + 4096)

	token := provider.issueToken(t, testOIDCTokenOptions{})
	for i := range 8 {
		_, err := validateOIDCToken(context.Background(), token, provider.configWithoutJWKSURL())
		if err == nil || !strings.Contains(err.Error(), "OIDC discovery response exceeds maximum size") {
			t.Fatalf("validateOIDCToken attempt %d error = %v, want oversized discovery error", i, err)
		}
	}
	if got := provider.jwksHits.Load(); got != 0 {
		t.Fatalf("JWKS fetches = %d, want 0", got)
	}
	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches = %d, want 1 during refresh backoff", got)
	}
}

func TestValidateOIDCToken_ConcurrentInvalidSignaturesCoalesceJWKSFetch(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config()); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}
	started, release := provider.blockJWKS()
	defer release()
	token := tamperJWTSignature(t, provider.issueToken(t, testOIDCTokenOptions{}))

	const requests = 32
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			_, err := validateOIDCToken(context.Background(), token, provider.config())
			errs <- err
		})
	}
	close(start)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for JWKS fetch")
	}
	release()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("validateOIDCToken error = %v, want signature error", err)
		}
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 (initial load plus one refresh for %d concurrent invalid signatures)", got, requests)
	}
}

func TestJWKSCache_LateOverlappingLoadSharesCompletedRefresh(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	fetchStarted, releaseFetch := provider.blockJWKS()
	defer releaseFetch()
	lateLoadStarted := make(chan struct{}, 1)
	releaseLateLoad := make(chan struct{})
	var releaseLateLoadOnce sync.Once
	unblockLateLoad := func() { releaseLateLoadOnce.Do(func() { close(releaseLateLoad) }) }
	defer unblockLateLoad()
	clock := &cacheTestBlockingClock{
		now:     time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
		blockAt: 3, // first load, refresh setup, then the overlapping load
		started: lateLoadStarted,
		release: releaseLateLoad,
	}
	cache := newJWKSCache(jwksCacheOptions{now: clock.Now})
	url := provider.config().JWKSURL
	type loadOutcome struct {
		result jwksLoadResult
		err    error
	}
	first := make(chan loadOutcome, 1)
	late := make(chan loadOutcome, 1)
	go func() {
		result, err := cache.load(context.Background(), url)
		first <- loadOutcome{result: result, err: err}
	}()
	select {
	case <-fetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial JWKS fetch")
	}
	go func() {
		result, err := cache.load(context.Background(), url)
		late <- loadOutcome{result: result, err: err}
	}()
	select {
	case <-lateLoadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for overlapping JWKS load")
	}

	releaseFetch()
	firstOutcome := <-first
	if firstOutcome.err != nil || !firstOutcome.result.refreshed {
		t.Fatalf("initial JWKS load: refreshed=%v err=%v, want successful refresh", firstOutcome.result.refreshed, firstOutcome.err)
	}
	unblockLateLoad()
	lateOutcome := <-late
	if lateOutcome.err != nil || !lateOutcome.result.refreshed {
		t.Fatalf("overlapping JWKS load: refreshed=%v err=%v, want completed refresh attribution", lateOutcome.result.refreshed, lateOutcome.err)
	}
	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1", got)
	}
}

func TestOIDCDiscoveryCache_LateOverlappingLoadSharesCompletedRefresh(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	fetchStarted, releaseFetch := provider.blockDiscovery()
	defer releaseFetch()
	lateLoadStarted := make(chan struct{}, 1)
	releaseLateLoad := make(chan struct{})
	var releaseLateLoadOnce sync.Once
	unblockLateLoad := func() { releaseLateLoadOnce.Do(func() { close(releaseLateLoad) }) }
	defer unblockLateLoad()
	clock := &cacheTestBlockingClock{
		now:     time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
		blockAt: 3, // first load, refresh setup, then the overlapping load
		started: lateLoadStarted,
		release: releaseLateLoad,
	}
	cache := newOIDCDiscoveryCache(oidcDiscoveryCacheOptions{now: clock.Now})
	issuer := provider.config().Issuer
	type loadOutcome struct {
		selection oidcDiscoverySelection
		err       error
	}
	first := make(chan loadOutcome, 1)
	late := make(chan loadOutcome, 1)
	go func() {
		selection, err := cache.load(context.Background(), issuer)
		first <- loadOutcome{selection: selection, err: err}
	}()
	select {
	case <-fetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial discovery fetch")
	}
	go func() {
		selection, err := cache.load(context.Background(), issuer)
		late <- loadOutcome{selection: selection, err: err}
	}()
	select {
	case <-lateLoadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for overlapping discovery load")
	}

	releaseFetch()
	firstOutcome := <-first
	if firstOutcome.err != nil || !firstOutcome.selection.refreshed {
		t.Fatalf("initial discovery load: refreshed=%v err=%v, want successful refresh", firstOutcome.selection.refreshed, firstOutcome.err)
	}
	unblockLateLoad()
	lateOutcome := <-late
	if lateOutcome.err != nil || !lateOutcome.selection.refreshed {
		t.Fatalf("overlapping discovery load: refreshed=%v err=%v, want completed refresh attribution", lateOutcome.selection.refreshed, lateOutcome.err)
	}
	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches = %d, want 1", got)
	}
}

func TestValidateOIDCToken_UnknownKidFloodCoalescesRefresh(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config()); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}

	const requests = 32
	tokens := make([]string, requests)
	for i := range tokens {
		tokens[i] = provider.issueToken(t, testOIDCTokenOptions{Kid: fmt.Sprintf("unknown-%d", i)})
	}

	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Go(func() {
			<-start
			_, err := validateOIDCToken(context.Background(), token, provider.config())
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil || !strings.Contains(err.Error(), "kid") {
			t.Fatalf("validateOIDCToken error = %v, want unknown kid error", err)
		}
	}
	for i := range 8 {
		token := provider.issueToken(t, testOIDCTokenOptions{Kid: fmt.Sprintf("later-unknown-%d", i)})
		if _, err := validateOIDCToken(context.Background(), token, provider.config()); err == nil || !strings.Contains(err.Error(), "kid") {
			t.Fatalf("validateOIDCToken later unknown kid error = %v, want unknown kid error", err)
		}
	}

	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 (initial load plus one coalesced unknown-kid refresh)", got)
	}
}

func TestValidateOIDCToken_UnknownKidRefreshSupportsRotation(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config()); err != nil {
		t.Fatalf("validate token before rotation: %v", err)
	}

	provider.rotate(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config()); err != nil {
		t.Fatalf("validate token after rotation: %v", err)
	}

	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 for initial load and rotation refresh", got)
	}
}

func TestValidateOIDCToken_DiscoveryRefreshSupportsJWKSURLRotation(t *testing.T) {
	keyA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate first RSA key: %v", err)
	}
	keyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate second RSA key: %v", err)
	}
	bodyA, err := json.Marshal(map[string]any{testJWKSKeysJSONKey: []testJWKKey{testJWKFromPublicKey(&keyA.PublicKey, "key-a")}})
	if err != nil {
		t.Fatalf("marshal first JWKS: %v", err)
	}
	bodyB, err := json.Marshal(map[string]any{testJWKSKeysJSONKey: []testJWKKey{testJWKFromPublicKey(&keyB.PublicKey, "key-b")}})
	if err != nil {
		t.Fatalf("marshal second JWKS: %v", err)
	}

	var discoveryMu sync.RWMutex
	var discoveryJWKSPath = testJWKSPathA
	var discoveryHits atomic.Int64
	var jwksAHits atomic.Int64
	var jwksBHits atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testOIDCDiscoveryPath:
			discoveryHits.Add(1)
			discoveryMu.RLock()
			path := discoveryJWKSPath
			discoveryMu.RUnlock()
			_ = json.NewEncoder(w).Encode(map[string]any{testJWKSURIJSONKey: server.URL + path})
		case testJWKSPathA:
			jwksAHits.Add(1)
			_, _ = w.Write(bodyA)
		case testJWKSPathB:
			jwksBHits.Add(1)
			_, _ = w.Write(bodyB)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	const audience = testOIDCAudience
	cfg := OIDCConfig{Issuer: server.URL, Audience: audience, AllowedSubjects: []string{"*"}}
	providerA := &testOIDCProvider{server: server, key: keyA, kid: "key-a", aud: audience}
	if _, err := validateOIDCToken(context.Background(), providerA.issueToken(t, testOIDCTokenOptions{}), cfg); err != nil {
		t.Fatalf("validate token with initial discovered JWKS URL: %v", err)
	}

	discoveryMu.Lock()
	discoveryJWKSPath = testJWKSPathB
	discoveryMu.Unlock()
	providerB := &testOIDCProvider{server: server, key: keyB, kid: "key-b", aud: audience}
	if _, err := validateOIDCToken(context.Background(), providerB.issueToken(t, testOIDCTokenOptions{}), cfg); err != nil {
		t.Fatalf("validate token after discovered JWKS URL rotation: %v", err)
	}

	if got := discoveryHits.Load(); got != 2 {
		t.Fatalf("OIDC discovery fetches = %d, want 2", got)
	}
	if got := jwksAHits.Load(); got != 2 {
		t.Fatalf("old JWKS URL fetches = %d, want 2 (initial load plus rotation probe)", got)
	}
	if got := jwksBHits.Load(); got != 1 {
		t.Fatalf("new JWKS URL fetches = %d, want 1", got)
	}
}

func TestValidateOIDCToken_DiscoveryRefreshSupportsJWKSURLRotationAfterStaleRefreshFailure(t *testing.T) {
	keyA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate first RSA key: %v", err)
	}
	keyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate second RSA key: %v", err)
	}
	bodyA, err := json.Marshal(map[string]any{testJWKSKeysJSONKey: []testJWKKey{testJWKFromPublicKey(&keyA.PublicKey, "key-a")}})
	if err != nil {
		t.Fatalf("marshal first JWKS: %v", err)
	}
	bodyB, err := json.Marshal(map[string]any{testJWKSKeysJSONKey: []testJWKKey{testJWKFromPublicKey(&keyB.PublicKey, "key-b")}})
	if err != nil {
		t.Fatalf("marshal second JWKS: %v", err)
	}

	var stateMu sync.RWMutex
	discoveryJWKSPath := testJWKSPathA
	jwksAUnavailable := false
	var discoveryHits atomic.Int64
	var jwksAHits atomic.Int64
	var jwksBHits atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testOIDCDiscoveryPath:
			discoveryHits.Add(1)
			stateMu.RLock()
			path := discoveryJWKSPath
			stateMu.RUnlock()
			_ = json.NewEncoder(w).Encode(map[string]any{testJWKSURIJSONKey: server.URL + path})
		case testJWKSPathA:
			jwksAHits.Add(1)
			stateMu.RLock()
			unavailable := jwksAUnavailable
			stateMu.RUnlock()
			if unavailable {
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(bodyA)
		case testJWKSPathB:
			jwksBHits.Add(1)
			_, _ = w.Write(bodyB)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const jwksFreshTTL = time.Minute
	verifier := newJWTVerifierWithCacheOptions(
		jwksCacheOptions{now: clock.Now, freshTTL: jwksFreshTTL, staleTTL: 2 * time.Minute},
		oidcDiscoveryCacheOptions{now: clock.Now, ttl: 5 * time.Minute},
	)
	const audience = testOIDCAudience
	cfg := OIDCConfig{Issuer: server.URL, Audience: audience, AllowedSubjects: []string{"*"}}
	providerA := &testOIDCProvider{server: server, key: keyA, kid: "key-a", aud: audience}
	if _, err := validateOIDCTokenWithVerifier(context.Background(), providerA.issueToken(t, testOIDCTokenOptions{}), cfg, verifier); err != nil {
		t.Fatalf("validate token with initial discovered JWKS URL: %v", err)
	}

	stateMu.Lock()
	discoveryJWKSPath = testJWKSPathB
	jwksAUnavailable = true
	stateMu.Unlock()
	clock.Advance(jwksFreshTTL + time.Second)
	providerB := &testOIDCProvider{server: server, key: keyB, kid: "key-b", aud: audience}
	if _, err := validateOIDCTokenWithVerifier(context.Background(), providerB.issueToken(t, testOIDCTokenOptions{}), cfg, verifier); err != nil {
		t.Fatalf("validate rotated token after stale JWKS refresh failure: %v", err)
	}

	if got := discoveryHits.Load(); got != 2 {
		t.Fatalf("OIDC discovery fetches = %d, want 2", got)
	}
	if got := jwksAHits.Load(); got != 2 {
		t.Fatalf("old JWKS URL fetches = %d, want 2", got)
	}
	if got := jwksBHits.Load(); got != 1 {
		t.Fatalf("new JWKS URL fetches = %d, want 1", got)
	}
}

func TestValidateOIDCToken_SignatureFailureRefreshSupportsSameKidRotation(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config()); err != nil {
		t.Fatalf("validate token before rotation: %v", err)
	}

	provider.rotateWithKid(t, "cache-key-1")
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config()); err != nil {
		t.Fatalf("validate same-kid token after rotation: %v", err)
	}

	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 for initial load and same-kid rotation refresh", got)
	}
}

func TestValidateOIDCToken_SignatureFailureRefreshSupportsKidlessRotation(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{OmitKid: true}), provider.config()); err != nil {
		t.Fatalf("validate kid-less token before rotation: %v", err)
	}

	provider.rotate(t)
	if _, err := validateOIDCToken(context.Background(), provider.issueToken(t, testOIDCTokenOptions{OmitKid: true}), provider.config()); err != nil {
		t.Fatalf("validate kid-less token after rotation: %v", err)
	}

	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 for initial load and kid-less rotation refresh", got)
	}
}

func TestJWTVerifier_InvalidSignaturesDoNotSlideRotationRefreshCooldown(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const forcedRefreshCooldown = 5 * time.Second
	verifier := newJWTVerifierWithOptions(jwksCacheOptions{
		now:                   clock.Now,
		forcedRefreshCooldown: forcedRefreshCooldown,
	})
	validToken := provider.issueToken(t, testOIDCTokenOptions{})
	if _, err := validateOIDCTokenWithVerifier(context.Background(), validToken, provider.config(), verifier); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}
	tamperedToken := tamperJWTSignature(t, validToken)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), tamperedToken, provider.config(), verifier); err == nil {
		t.Fatal("tampered token unexpectedly validated")
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches after first invalid signature = %d, want 2", got)
	}

	clock.Advance(4 * time.Second)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), tamperedToken, provider.config(), verifier); err == nil {
		t.Fatal("second tampered token unexpectedly validated")
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches inside forced-refresh cooldown = %d, want 2", got)
	}

	clock.Advance(2 * time.Second)
	provider.rotateWithKid(t, "cache-key-1")
	if _, err := validateOIDCTokenWithVerifier(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), provider.config(), verifier); err != nil {
		t.Fatalf("validate same-kid rotation after original cooldown: %v", err)
	}
	if got := provider.jwksHits.Load(); got != 3 {
		t.Fatalf("JWKS fetches after rotation = %d, want 3", got)
	}
}

func TestJWTVerifier_KidlessAlgorithmRotationRefreshesMissingCompatibleKey(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}

	var bodyMu sync.RWMutex
	body := marshalPublicJWKS(t, &rsaKey.PublicKey, jwa.RS256())
	var jwksHits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jwksHits.Add(1)
		bodyMu.RLock()
		defer bodyMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	const audience = testOIDCAudience
	verifier := newJWTVerifier()
	cfg := jwtVerificationConfig{
		Issuer:            server.URL,
		Audience:          audience,
		JWKSURL:           server.URL,
		AllowedAlgorithms: []jwa.SignatureAlgorithm{jwa.RS256(), jwa.ES256()},
	}
	rsaProvider := &testOIDCProvider{server: server, key: rsaKey, kid: "unused", aud: audience}
	if _, err := verifier.verifyJWT(context.Background(), rsaProvider.issueToken(t, testOIDCTokenOptions{OmitKid: true}), cfg); err != nil {
		t.Fatalf("validate kid-less RSA token: %v", err)
	}

	bodyMu.Lock()
	body = marshalPublicJWKS(t, &ecKey.PublicKey, jwa.ES256())
	bodyMu.Unlock()
	if _, err := verifier.verifyJWT(context.Background(), issueES256Token(t, server.URL, audience, ecKey), cfg); err != nil {
		t.Fatalf("validate kid-less ES256 token after algorithm rotation: %v", err)
	}
	if got := jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 for initial load and kid-less algorithm rotation", got)
	}
}

func TestJWTVerifier_RotationRetryUsesRefreshCompletedAfterSnapshot(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	verifier := newJWTVerifier()
	cfg := jwtVerificationConfig{
		Issuer:   provider.server.URL,
		Audience: provider.aud,
		JWKSURL:  provider.server.URL,
	}
	if _, err := verifier.verifyJWT(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), cfg); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}

	provider.rotateWithKid(t, "cache-key-1")
	parsed, err := parseCompactJWT(provider.issueToken(t, testOIDCTokenOptions{}))
	if err != nil {
		t.Fatalf("parse rotated token: %v", err)
	}
	staleSelection, err := verifier.jwks.signingKeys(context.Background(), cfg.JWKSURL, parsed.Header)
	if err != nil {
		t.Fatalf("select cached signing key: %v", err)
	}
	if _, _, refreshed, err := verifier.jwks.refreshForRotation(context.Background(), cfg.JWKSURL, 0); err != nil || !refreshed {
		t.Fatalf("complete concurrent rotation refresh: refreshed=%v err=%v", refreshed, err)
	}

	if _, err := verifier.parseJWTWithRotationRetry(context.Background(), parsed, cfg.JWKSURL, staleSelection); err != nil {
		t.Fatalf("retry rotated token after concurrent refresh: %v", err)
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2", got)
	}
}

func TestJWTVerifier_RotationRetryUsesTTLRefreshCompletedAfterSnapshot(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const freshTTL = time.Minute
	verifier := newJWTVerifierWithOptions(jwksCacheOptions{now: clock.Now, freshTTL: freshTTL})
	cfg := jwtVerificationConfig{
		Issuer:   provider.server.URL,
		Audience: provider.aud,
		JWKSURL:  provider.config().JWKSURL,
	}
	if _, err := verifier.verifyJWT(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), cfg); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}

	provider.rotateWithKid(t, "cache-key-1")
	parsed, err := parseCompactJWT(provider.issueToken(t, testOIDCTokenOptions{}))
	if err != nil {
		t.Fatalf("parse rotated token: %v", err)
	}
	staleSelection, err := verifier.jwks.signingKeys(context.Background(), cfg.JWKSURL, parsed.Header)
	if err != nil {
		t.Fatalf("select cached signing key: %v", err)
	}
	clock.Advance(freshTTL + time.Second)
	if _, _, err := verifier.jwks.refresh(context.Background(), cfg.JWKSURL); err != nil {
		t.Fatalf("complete ordinary TTL refresh: %v", err)
	}
	provider.setJWKSStatus(http.StatusServiceUnavailable)

	if _, err := verifier.parseJWTWithRotationRetry(context.Background(), parsed, cfg.JWKSURL, staleSelection); err != nil {
		t.Fatalf("retry rotated token using completed TTL refresh: %v", err)
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2 without a redundant post-snapshot fetch", got)
	}
}

func TestOIDCDiscoveryCache_RotationUsesTTLRefreshCompletedAfterSnapshot(t *testing.T) {
	var stateMu sync.RWMutex
	discoveryPath := testJWKSPathA
	discoveryStatus := 0
	var discoveryHits atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, testOIDCDiscoveryPath) {
			http.NotFound(w, r)
			return
		}
		discoveryHits.Add(1)
		stateMu.RLock()
		path := discoveryPath
		status := discoveryStatus
		stateMu.RUnlock()
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{testJWKSURIJSONKey: server.URL + path})
	}))
	t.Cleanup(server.Close)

	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const discoveryTTL = time.Minute
	cache := newOIDCDiscoveryCache(oidcDiscoveryCacheOptions{now: clock.Now, ttl: discoveryTTL})
	selection, err := cache.load(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("load initial discovery document: %v", err)
	}

	stateMu.Lock()
	discoveryPath = testJWKSPathB
	stateMu.Unlock()
	clock.Advance(discoveryTTL + time.Second)
	currentURL, currentGeneration, err := cache.refresh(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("complete ordinary discovery TTL refresh: %v", err)
	}
	stateMu.Lock()
	discoveryStatus = http.StatusServiceUnavailable
	stateMu.Unlock()

	reusedURL, reusedGeneration, refreshed, err := cache.refreshForRotation(context.Background(), server.URL, selection.generation)
	if err != nil {
		t.Fatalf("reuse completed discovery refresh: %v", err)
	}
	if !refreshed || reusedURL != currentURL || reusedGeneration != currentGeneration {
		t.Fatalf("reused discovery = (%q, %d, refreshed=%v), want (%q, %d, true)", reusedURL, reusedGeneration, refreshed, currentURL, currentGeneration)
	}
	if got := discoveryHits.Load(); got != 2 {
		t.Fatalf("OIDC discovery fetches = %d, want 2 without a redundant post-snapshot fetch", got)
	}
}

func TestJWTVerifier_MixedJWKSUsesPublicKeysWithoutCachingSymmetricSecrets(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	const rsaKid = "mixed-rsa-key"
	rsaJWK, err := jwk.Import(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("import RSA JWK: %v", err)
	}
	if err := rsaJWK.Set(jwk.KeyIDKey, rsaKid); err != nil {
		t.Fatalf("set RSA JWK kid: %v", err)
	}
	if err := rsaJWK.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("set RSA JWK algorithm: %v", err)
	}
	if err := rsaJWK.Set(jwk.KeyUsageKey, string(jwk.ForSignature)); err != nil {
		t.Fatalf("set RSA JWK usage: %v", err)
	}
	symmetricJWK, err := jwk.Import([]byte("test-only-symmetric-secret"))
	if err != nil {
		t.Fatalf("import symmetric JWK: %v", err)
	}
	if err := symmetricJWK.Set(jwk.KeyIDKey, "unrelated-symmetric-key"); err != nil {
		t.Fatalf("set symmetric JWK kid: %v", err)
	}
	mixedSet := jwk.NewSet()
	if err := mixedSet.AddKey(rsaJWK); err != nil {
		t.Fatalf("add RSA JWK: %v", err)
	}
	if err := mixedSet.AddKey(symmetricJWK); err != nil {
		t.Fatalf("add symmetric JWK: %v", err)
	}
	body, err := json.Marshal(mixedSet)
	if err != nil {
		t.Fatalf("marshal mixed JWKS: %v", err)
	}

	var jwksHits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jwksHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	const audience = testOIDCAudience
	provider := &testOIDCProvider{server: server, key: privateKey, kid: rsaKid, aud: audience}
	verifier := newJWTVerifier()
	cfg := provider.config()
	if _, err := validateOIDCTokenWithVerifier(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), cfg, verifier); err != nil {
		t.Fatalf("validate token from mixed JWKS: %v", err)
	}

	verifier.jwks.mu.Lock()
	entry := verifier.jwks.entries[cfg.JWKSURL]
	cachedKeyCount := 0
	cachedKeyType := ""
	if entry != nil && entry.set != nil {
		cachedKeyCount = entry.set.Len()
		if key, ok := entry.set.Key(0); ok {
			cachedKeyType = key.KeyType().String()
		}
	}
	verifier.jwks.mu.Unlock()
	if cachedKeyCount != 1 || cachedKeyType != jwa.RSA().String() {
		t.Fatalf("cached mixed JWKS has %d key(s) of type %s, want one RSA public key", cachedKeyCount, cachedKeyType)
	}
	if got := jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1", got)
	}
}

func TestJWTVerifier_PrivateAsymmetricJWKDoesNotCachePrivateFields(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	const (
		rsaKid                = "private-rsa-key"
		otherPrimesField      = "oth"
		privateExtensionField = "private_extension"
		privateExtensionValue = "test-only-private-extension"
	)
	privateJWK, err := jwk.Import(privateKey)
	if err != nil {
		t.Fatalf("import private RSA JWK: %v", err)
	}
	if err := privateJWK.Set(jwk.KeyIDKey, rsaKid); err != nil {
		t.Fatalf("set RSA JWK kid: %v", err)
	}
	if err := privateJWK.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("set RSA JWK algorithm: %v", err)
	}
	if err := privateJWK.Set(jwk.KeyUsageKey, string(jwk.ForSignature)); err != nil {
		t.Fatalf("set RSA JWK usage: %v", err)
	}
	if err := privateJWK.Set(otherPrimesField, []any{map[string]any{"r": "secret-prime"}}); err != nil {
		t.Fatalf("set RSA other-primes field: %v", err)
	}
	if err := privateJWK.Set(privateExtensionField, privateExtensionValue); err != nil {
		t.Fatalf("set private JWK extension: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(privateJWK); err != nil {
		t.Fatalf("add private RSA JWK: %v", err)
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal private JWKS: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	provider := &testOIDCProvider{server: server, key: privateKey, kid: rsaKid, aud: testOIDCAudience}
	verifier := newJWTVerifier()
	cfg := provider.config()
	if _, err := validateOIDCTokenWithVerifier(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), cfg, verifier); err != nil {
		t.Fatalf("validate token from private-key JWKS: %v", err)
	}

	verifier.jwks.mu.Lock()
	entry := verifier.jwks.entries[cfg.JWKSURL]
	var cachedKey jwk.Key
	if entry != nil && entry.set != nil {
		cachedKey, _ = entry.set.Key(0)
	}
	verifier.jwks.mu.Unlock()
	if cachedKey == nil {
		t.Fatal("cached public JWK is missing")
	}
	privateFields := []string{
		jwk.RSADKey,
		jwk.RSAPKey,
		jwk.RSAQKey,
		jwk.RSADPKey,
		jwk.RSADQKey,
		jwk.RSAQIKey,
		otherPrimesField,
		privateExtensionField,
	}
	for _, field := range privateFields {
		if cachedKey.Has(field) {
			t.Fatalf("cached public JWK retained private field %q", field)
		}
	}
	cachedJSON, err := json.Marshal(cachedKey)
	if err != nil {
		t.Fatalf("marshal cached public JWK: %v", err)
	}
	if strings.Contains(string(cachedJSON), privateExtensionValue) || strings.Contains(string(cachedJSON), "secret-prime") {
		t.Fatalf("cached public JWK retained private extension data: %s", cachedJSON)
	}
}

func TestJWTVerifier_EmptyJWKSWithdrawsCachedKeys(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const freshTTL = time.Minute
	verifier := newJWTVerifierWithOptions(jwksCacheOptions{
		now:      clock.Now,
		freshTTL: freshTTL,
		staleTTL: 2 * time.Minute,
	})
	token := provider.issueToken(t, testOIDCTokenOptions{})
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}

	provider.setJWKSEmpty(true)
	clock.Advance(freshTTL + time.Second)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err == nil {
		t.Fatal("token signed by withdrawn key unexpectedly validated")
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2", got)
	}
}

func TestValidateOIDCToken_RejectsOversizedJWKS(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	provider.setJWKSPadding((1 << 20) + 4096)

	token := provider.issueToken(t, testOIDCTokenOptions{})
	for i := range 8 {
		_, err := validateOIDCToken(context.Background(), token, provider.config())
		if err == nil || !strings.Contains(err.Error(), "JWKS response exceeds maximum size") {
			t.Fatalf("validateOIDCToken attempt %d error = %v, want oversized JWKS error", i, err)
		}
	}
	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1 during refresh backoff", got)
	}
}

func TestJWTVerifier_JWKSOutageUsesBoundedStaleCacheAndFailsClosedAfterExpiry(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	const (
		freshTTL   = time.Minute
		staleTTL   = 2 * time.Minute
		retryDelay = 5 * time.Second
	)
	verifier := newJWTVerifierWithOptions(jwksCacheOptions{
		now:               clock.Now,
		freshTTL:          freshTTL,
		staleTTL:          staleTTL,
		refreshRetryDelay: retryDelay,
	})
	token := provider.issueToken(t, testOIDCTokenOptions{})

	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}
	provider.setJWKSStatus(http.StatusServiceUnavailable)
	clock.Advance(freshTTL + time.Second)

	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err != nil {
		t.Fatalf("validate with stale JWKS during outage: %v", err)
	}
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err != nil {
		t.Fatalf("validate with stale JWKS during refresh backoff: %v", err)
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches during stale outage = %d, want 2 (initial load plus one failed refresh)", got)
	}

	clock.Advance(staleTTL)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err == nil {
		t.Fatal("validateOIDCTokenWithVerifier succeeded after the hard stale expiry during outage")
	}
	if got := provider.jwksHits.Load(); got != 3 {
		t.Fatalf("JWKS fetches after hard expiry = %d, want 3", got)
	}

	provider.setJWKSStatus(0)
	clock.Advance(retryDelay + time.Second)
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, provider.config(), verifier); err != nil {
		t.Fatalf("validate after JWKS endpoint recovery: %v", err)
	}
	if got := provider.jwksHits.Load(); got != 4 {
		t.Fatalf("JWKS fetches after recovery = %d, want 4", got)
	}
}

func TestJWTVerifier_ConcurrentJWKSOutageCoalescesAndBacksOff(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	provider.setJWKSStatus(http.StatusServiceUnavailable)
	verifier := newJWTVerifier()
	token := provider.issueToken(t, testOIDCTokenOptions{})
	cfg := provider.config()

	const requests = 32
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			_, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil || !strings.Contains(err.Error(), "fetch JWKS") {
			t.Fatalf("validateOIDCTokenWithVerifier error = %v, want JWKS fetch error", err)
		}
	}
	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches = %d, want 1 for %d concurrent failures", got, requests)
	}
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier); err == nil {
		t.Fatal("token unexpectedly validated during JWKS refresh backoff")
	}
	if got := provider.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetches during retry backoff = %d, want 1", got)
	}
}

func TestJWTVerifier_ConcurrentDiscoveryOutageCoalescesAndBacksOff(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	provider.setDiscoveryStatus(http.StatusServiceUnavailable)
	verifier := newJWTVerifier()
	token := provider.issueToken(t, testOIDCTokenOptions{})
	cfg := provider.configWithoutJWKSURL()

	const requests = 32
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			_, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil || !strings.Contains(err.Error(), "fetch OIDC discovery document") {
			t.Fatalf("validateOIDCTokenWithVerifier error = %v, want discovery fetch error", err)
		}
	}
	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches = %d, want 1 for %d concurrent failures", got, requests)
	}
	if _, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier); err == nil {
		t.Fatal("token unexpectedly validated during discovery refresh backoff")
	}
	if got := provider.discoveryHits.Load(); got != 1 {
		t.Fatalf("OIDC discovery fetches during retry backoff = %d, want 1", got)
	}
	if got := provider.jwksHits.Load(); got != 0 {
		t.Fatalf("JWKS fetches = %d, want 0 while discovery is unavailable", got)
	}
}

func TestJWKSCache_EvictsLeastRecentlyUsedEntryAtCapacity(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	cache := newJWKSCache(jwksCacheOptions{now: clock.Now, maxEntries: 2})
	urls := []string{
		provider.server.URL + "/jwks?cache=0",
		provider.server.URL + "/jwks?cache=1",
		provider.server.URL + "/jwks?cache=2",
	}

	if _, err := cache.load(context.Background(), urls[0]); err != nil {
		t.Fatalf("load first JWKS: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), urls[1]); err != nil {
		t.Fatalf("load second JWKS: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), urls[0]); err != nil {
		t.Fatalf("touch first JWKS: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), urls[2]); err != nil {
		t.Fatalf("load third JWKS: %v", err)
	}

	cache.mu.Lock()
	entryCount := len(cache.entries)
	_, keptFirst := cache.entries[urls[0]]
	_, evictedSecond := cache.entries[urls[1]]
	_, keptThird := cache.entries[urls[2]]
	cache.mu.Unlock()
	if entryCount != 2 || !keptFirst || evictedSecond || !keptThird {
		t.Fatalf("JWKS cache entries = %d, first=%v second=%v third=%v; want bounded LRU contents", entryCount, keptFirst, evictedSecond, keptThird)
	}

	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), urls[1]); err != nil {
		t.Fatalf("reload evicted JWKS: %v", err)
	}
	if got := provider.jwksHits.Load(); got != 4 {
		t.Fatalf("JWKS fetches = %d, want 4 including the evicted entry reload", got)
	}
}

func TestOIDCDiscoveryCache_EvictsLeastRecentlyUsedEntryAtCapacity(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	clock := &cacheTestClock{now: time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)}
	cache := newOIDCDiscoveryCache(oidcDiscoveryCacheOptions{now: clock.Now, maxEntries: 2})
	issuers := []string{
		provider.server.URL + "/issuer-0",
		provider.server.URL + "/issuer-1",
		provider.server.URL + "/issuer-2",
	}

	if _, err := cache.load(context.Background(), issuers[0]); err != nil {
		t.Fatalf("load first discovery document: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), issuers[1]); err != nil {
		t.Fatalf("load second discovery document: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), issuers[0]); err != nil {
		t.Fatalf("touch first discovery document: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), issuers[2]); err != nil {
		t.Fatalf("load third discovery document: %v", err)
	}

	cache.mu.Lock()
	entryCount := len(cache.entries)
	_, keptFirst := cache.entries[issuers[0]]
	_, evictedSecond := cache.entries[issuers[1]]
	_, keptThird := cache.entries[issuers[2]]
	cache.mu.Unlock()
	if entryCount != 2 || !keptFirst || evictedSecond || !keptThird {
		t.Fatalf("discovery cache entries = %d, first=%v second=%v third=%v; want bounded LRU contents", entryCount, keptFirst, evictedSecond, keptThird)
	}

	clock.Advance(time.Second)
	if _, err := cache.load(context.Background(), issuers[1]); err != nil {
		t.Fatalf("reload evicted discovery document: %v", err)
	}
	if got := provider.discoveryHits.Load(); got != 4 {
		t.Fatalf("OIDC discovery fetches = %d, want 4 including the evicted entry reload", got)
	}
}

func TestJWKSCache_NegativeKidEntriesAreBounded(t *testing.T) {
	provider := newCacheTestOIDCProvider(t)
	const maxNegativeKids = 3
	verifier := newJWTVerifierWithOptions(jwksCacheOptions{maxNegativeKids: maxNegativeKids})
	cfg := provider.config()
	if _, err := validateOIDCTokenWithVerifier(context.Background(), provider.issueToken(t, testOIDCTokenOptions{}), cfg, verifier); err != nil {
		t.Fatalf("warm JWKS cache: %v", err)
	}

	for i := range 12 {
		token := provider.issueToken(t, testOIDCTokenOptions{Kid: fmt.Sprintf("unknown-bounded-%d", i)})
		if _, err := validateOIDCTokenWithVerifier(context.Background(), token, cfg, verifier); err == nil {
			t.Fatalf("unknown kid %d unexpectedly validated", i)
		}
	}

	verifier.jwks.mu.Lock()
	entry := verifier.jwks.entries[cfg.JWKSURL]
	negativeKidCount := 0
	if entry != nil {
		negativeKidCount = len(entry.negativeKids)
	}
	verifier.jwks.mu.Unlock()
	if negativeKidCount != maxNegativeKids {
		t.Fatalf("negative kid entries = %d, want bounded size %d", negativeKidCount, maxNegativeKids)
	}
	if got := provider.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want initial load plus one bounded rotation refresh", got)
	}
}

func TestAuthMetadataCaches_BoundOutboundRequestDuration(t *testing.T) {
	const requestTimeout = 25 * time.Millisecond

	tests := []struct {
		name string
		load func(context.Context, string) error
		url  func(string) string
	}{
		{
			name: "JWKS",
			load: func(ctx context.Context, url string) error {
				_, err := newJWKSCache(jwksCacheOptions{requestTimeout: requestTimeout}).load(ctx, url)
				return err
			},
			url: func(serverURL string) string { return serverURL + "/jwks" },
		},
		{
			name: "OIDC discovery",
			load: func(ctx context.Context, issuer string) error {
				_, err := newOIDCDiscoveryCache(oidcDiscoveryCacheOptions{requestTimeout: requestTimeout}).load(ctx, issuer)
				return err
			},
			url: func(serverURL string) string { return serverURL },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			}))
			t.Cleanup(server.Close)

			started := time.Now()
			err := tt.load(context.Background(), tt.url(server.URL))
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("metadata request unexpectedly succeeded")
			}
			if elapsed > 500*time.Millisecond {
				t.Fatalf("metadata request took %v, want a bounded timeout near %v", elapsed, requestTimeout)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("metadata fetches = %d, want 1", got)
			}
		})
	}
}

func TestFilterJWTSigningKeys_AllowsECKeyForES256(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate EC key: %v", err)
	}

	key, err := jwk.Import(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to import EC key: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, "ec-key"); err != nil {
		t.Fatalf("failed to set JWK kid: %v", err)
	}

	keySet := jwk.NewSet()
	if err := keySet.AddKey(key); err != nil {
		t.Fatalf("failed to add EC key: %v", err)
	}

	filtered, err := filterJWTSigningKeys(keySet, jwtHeader{
		Algorithm: jwa.ES256(),
		KeyID:     "ec-key",
	})
	if err != nil {
		t.Fatalf("filterJWTSigningKeys returned error: %v", err)
	}
	if filtered.Len() != 1 {
		t.Fatalf("filtered key count = %d, want 1", filtered.Len())
	}

	filteredKey, ok := filtered.Key(0)
	if !ok {
		t.Fatal("filtered key missing at index 0")
	}
	if got := filteredKey.KeyType().String(); got != jwa.EC().String() {
		t.Fatalf("filtered key type = %s, want %s", got, jwa.EC().String())
	}
	alg, ok := filteredKey.Algorithm()
	if !ok || alg == nil || alg.String() != jwa.ES256().String() {
		t.Fatalf("filtered key algorithm = %v, want %s", alg, jwa.ES256().String())
	}
}
