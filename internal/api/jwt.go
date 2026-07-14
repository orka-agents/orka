/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	// A cached JWKS is used without I/O for five minutes. If refresh then fails,
	// known keys may be used for one additional five-minute stale grace period;
	// after that hard expiry, verification fails closed until refresh succeeds.
	defaultJWKSFreshTTL              = 5 * time.Minute
	defaultJWKSStaleTTL              = 5 * time.Minute
	defaultJWKSRefreshRetryDelay     = 5 * time.Second
	defaultJWKSForcedRefreshCooldown = 5 * time.Second
	defaultMaxJWKSResponseBytes      = int64(1 << 20)
	defaultMaxJWKSCacheEntries       = 64
	defaultMaxJWKSNegativeKidCount   = 128
)

var (
	errJWTKeyResolution      = errors.New("JWT key resolution failed")
	errJWTSigningKeyNotFound = errors.New("JWT signing key not found")
)

type jwtVerificationConfig struct {
	Issuer            string
	Audience          string
	JWKSURL           string
	AllowedAlgorithms []jwa.SignatureAlgorithm
	RequiredClaims    []string
}

type jwtHeader struct {
	Algorithm jwa.SignatureAlgorithm
	KeyID     string
	Type      string
}

type parsedJWT struct {
	Raw       string
	Header    jwtHeader
	RawClaims json.RawMessage
}

type verifiedJWT struct {
	Token     jwt.Token
	Header    jwtHeader
	RawClaims json.RawMessage
}

type jwksCacheOptions struct {
	client                *http.Client
	now                   func() time.Time
	requestTimeout        time.Duration
	freshTTL              time.Duration
	staleTTL              time.Duration
	refreshRetryDelay     time.Duration
	forcedRefreshCooldown time.Duration
	maxResponseBytes      int64
	maxEntries            int
	maxNegativeKids       int
}

type jwksCache struct {
	client                *http.Client
	now                   func() time.Time
	requestTimeout        time.Duration
	freshTTL              time.Duration
	staleTTL              time.Duration
	refreshRetryDelay     time.Duration
	forcedRefreshCooldown time.Duration
	maxResponseBytes      int64
	maxEntries            int
	maxNegativeKids       int

	mu             sync.Mutex
	entries        map[string]*jwksCacheEntry
	nextGeneration uint64
}

type jwksCacheEntry struct {
	set                jwk.Set
	freshUntil         time.Time
	staleUntil         time.Time
	retryAfter         time.Time
	lastRefreshErr     error
	generation         uint64
	observedGeneration uint64
	forcedRefreshAfter time.Time
	negativeKids       map[[sha256.Size]byte]time.Time
	lastUsed           time.Time
	refresh            *jwksRefreshCall
}

type jwksRefreshCall struct {
	done       chan struct{}
	set        jwk.Set
	generation uint64
	err        error
}

type jwksLoadResult struct {
	set        jwk.Set
	generation uint64
	refreshed  bool
	refreshErr error
}

type jwtSigningKeySelection struct {
	set              jwk.Set
	generation       uint64
	refreshAttempted bool
}

type jwtVerifier struct {
	jwks      *jwksCache
	discovery *oidcDiscoveryCache
}

var processJWTVerifier = newJWTVerifier()

func newJWTVerifier() *jwtVerifier {
	return newJWTVerifierWithCacheOptions(jwksCacheOptions{}, oidcDiscoveryCacheOptions{})
}

func newJWTVerifierWithOptions(opts jwksCacheOptions) *jwtVerifier {
	return newJWTVerifierWithCacheOptions(opts, oidcDiscoveryCacheOptions{
		client:         opts.client,
		now:            opts.now,
		requestTimeout: opts.requestTimeout,
	})
}

func newJWTVerifierWithCacheOptions(jwksOpts jwksCacheOptions, discoveryOpts oidcDiscoveryCacheOptions) *jwtVerifier {
	return &jwtVerifier{
		jwks:      newJWKSCache(jwksOpts),
		discovery: newOIDCDiscoveryCache(discoveryOpts),
	}
}

func newJWKSCache(opts jwksCacheOptions) *jwksCache {
	if opts.requestTimeout <= 0 {
		opts.requestTimeout = authHTTPTimeout
	}
	if opts.client == nil {
		opts.client = &http.Client{Timeout: opts.requestTimeout}
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.freshTTL <= 0 {
		opts.freshTTL = defaultJWKSFreshTTL
	}
	if opts.staleTTL <= 0 {
		opts.staleTTL = defaultJWKSStaleTTL
	}
	if opts.refreshRetryDelay <= 0 {
		opts.refreshRetryDelay = defaultJWKSRefreshRetryDelay
	}
	if opts.forcedRefreshCooldown <= 0 {
		opts.forcedRefreshCooldown = defaultJWKSForcedRefreshCooldown
	}
	if opts.maxResponseBytes <= 0 {
		opts.maxResponseBytes = defaultMaxJWKSResponseBytes
	}
	if opts.maxEntries <= 0 {
		opts.maxEntries = defaultMaxJWKSCacheEntries
	}
	if opts.maxNegativeKids <= 0 {
		opts.maxNegativeKids = defaultMaxJWKSNegativeKidCount
	}

	return &jwksCache{
		client:                opts.client,
		now:                   opts.now,
		requestTimeout:        opts.requestTimeout,
		freshTTL:              opts.freshTTL,
		staleTTL:              opts.staleTTL,
		refreshRetryDelay:     opts.refreshRetryDelay,
		forcedRefreshCooldown: opts.forcedRefreshCooldown,
		maxResponseBytes:      opts.maxResponseBytes,
		maxEntries:            opts.maxEntries,
		maxNegativeKids:       opts.maxNegativeKids,
		entries:               make(map[string]*jwksCacheEntry),
	}
}

func verifyJWT(ctx context.Context, raw string, cfg jwtVerificationConfig) (*verifiedJWT, error) {
	return processJWTVerifier.verifyJWT(ctx, raw, cfg)
}

func (v *jwtVerifier) verifyJWT(ctx context.Context, raw string, cfg jwtVerificationConfig) (*verifiedJWT, error) {
	parsed, err := parseCompactJWT(raw)
	if err != nil {
		return nil, err
	}
	return v.verifyParsedJWT(ctx, parsed, cfg)
}

func (v *jwtVerifier) verifyParsedJWT(ctx context.Context, parsed *parsedJWT, cfg jwtVerificationConfig) (*verifiedJWT, error) {
	if !jwtSigningAlgorithmAllowed(parsed.Header.Algorithm, cfg.AllowedAlgorithms) {
		return nil, fmt.Errorf("unsupported JWT signing algorithm %q", parsed.Header.Algorithm.String())
	}
	if strings.TrimSpace(cfg.JWKSURL) == "" {
		return nil, errors.New("missing JWKS URL")
	}

	selection, err := v.jwks.signingKeys(ctx, cfg.JWKSURL, parsed.Header)
	if err != nil {
		return nil, err
	}

	tok, err := v.parseJWTWithRotationRetry(ctx, parsed, cfg.JWKSURL, selection)
	if err != nil {
		return nil, fmt.Errorf("verify JWT signature: %w", err)
	}

	validateOptions := make([]jwt.ValidateOption, 0, 2+len(jwtRequiredClaims(cfg.RequiredClaims)))
	if cfg.Issuer != "" {
		validateOptions = append(validateOptions, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		validateOptions = append(validateOptions, jwt.WithAudience(cfg.Audience))
	}
	for _, claim := range jwtRequiredClaims(cfg.RequiredClaims) {
		validateOptions = append(validateOptions, jwt.WithRequiredClaim(claim))
	}

	if err := jwt.Validate(tok, validateOptions...); err != nil {
		return nil, normalizeJWTValidationError(err)
	}
	if jwtClaimRequired(jwt.SubjectKey, cfg.RequiredClaims) {
		subject, ok := tok.Subject()
		if !ok || subject == "" {
			return nil, errors.New("missing subject")
		}
	}

	return &verifiedJWT{
		Token:     tok,
		Header:    parsed.Header,
		RawClaims: parsed.RawClaims,
	}, nil
}

func (v *jwtVerifier) parseJWTWithRotationRetry(ctx context.Context, parsed *parsedJWT, jwksURL string, selection jwtSigningKeySelection) (jwt.Token, error) {
	tok, err := parseJWTWithKeySet(parsed.Raw, selection.set)
	usedGeneration := selection.generation
	if err != nil && errors.Is(err, jws.VerificationError()) && !selection.refreshAttempted {
		currentSet, currentGeneration, _, refreshErr := v.jwks.refreshForRotation(ctx, jwksURL, selection.generation)
		switch {
		case refreshErr != nil:
			err = fmt.Errorf("%w: %v; refresh JWKS after signature failure: %w", errJWTKeyResolution, err, refreshErr)
		case currentSet != nil && currentGeneration != selection.generation:
			usedGeneration = currentGeneration
			filteredSet, filterErr := filterJWTSigningKeys(currentSet, parsed.Header)
			if filterErr != nil {
				if errors.Is(filterErr, errJWTSigningKeyNotFound) {
					v.jwks.noteKeySelectionMiss(jwksURL, parsed.Header.KeyID, currentGeneration, true)
				}
				err = filterErr
			} else {
				tok, err = parseJWTWithKeySet(parsed.Raw, filteredSet)
			}
		}
	}
	if err == nil {
		v.jwks.noteVerificationSuccess(jwksURL, usedGeneration)
	} else if errors.Is(err, jws.VerificationError()) {
		v.jwks.noteSignatureFailure(jwksURL, usedGeneration)
	}
	return tok, err
}

func parseJWTWithKeySet(raw string, keySet jwk.Set) (jwt.Token, error) {
	return jwt.ParseString(raw,
		jwt.WithValidate(false),
		jwt.WithKeySet(keySet, jws.WithUseDefault(true)),
	)
}

func (c *jwksCache) signingKeys(ctx context.Context, url string, header jwtHeader) (jwtSigningKeySelection, error) {
	loaded, err := c.load(ctx, url)
	if err != nil {
		return jwtSigningKeySelection{}, fmt.Errorf("%w: fetch JWKS: %w", errJWTKeyResolution, err)
	}

	filtered, filterErr := filterJWTSigningKeys(loaded.set, header)
	if filterErr == nil {
		return jwtSigningKeySelection{
			set:              filtered,
			generation:       loaded.generation,
			refreshAttempted: loaded.refreshed || loaded.refreshErr != nil,
		}, nil
	}
	if !errors.Is(filterErr, errJWTSigningKeyNotFound) {
		return jwtSigningKeySelection{}, filterErr
	}

	// A cache fill or TTL refresh already consulted the issuer for this request.
	// Do not immediately fetch the same JWKS a second time for a key-set miss.
	if loaded.refreshed {
		c.noteKeySelectionMiss(url, header.KeyID, loaded.generation, true)
		return jwtSigningKeySelection{}, filterErr
	}
	if loaded.refreshErr != nil {
		c.noteKeySelectionMiss(url, header.KeyID, loaded.generation, true)
		return jwtSigningKeySelection{}, fmt.Errorf("%w: %w; refresh JWKS: %v", errJWTKeyResolution, filterErr, loaded.refreshErr)
	}

	var currentSet jwk.Set
	var currentGeneration uint64
	if header.KeyID == "" {
		currentSet, currentGeneration, _, err = c.refreshForRotation(ctx, url, loaded.generation)
	} else {
		currentSet, currentGeneration, _, err = c.refreshForUnknownKid(ctx, url, header.KeyID, loaded.generation)
	}
	if err != nil {
		return jwtSigningKeySelection{}, fmt.Errorf("%w: %v; refresh JWKS: %w", errJWTKeyResolution, filterErr, err)
	}
	if currentSet == nil || currentGeneration == loaded.generation {
		return jwtSigningKeySelection{}, filterErr
	}

	filtered, filterErr = filterJWTSigningKeys(currentSet, header)
	if filterErr != nil {
		if errors.Is(filterErr, errJWTSigningKeyNotFound) {
			c.noteKeySelectionMiss(url, header.KeyID, currentGeneration, false)
		}
		return jwtSigningKeySelection{}, filterErr
	}
	return jwtSigningKeySelection{set: filtered, generation: currentGeneration, refreshAttempted: true}, nil
}

func (c *jwksCache) load(ctx context.Context, url string) (jwksLoadResult, error) {
	now := c.now()
	c.mu.Lock()
	entry := c.entries[url]
	if entry != nil {
		entry.lastUsed = now
		if entry.set != nil && now.Before(entry.freshUntil) {
			set := entry.set
			generation := entry.generation
			refreshed := entry.observedGeneration != generation
			c.mu.Unlock()
			return jwksLoadResult{
				set:        set,
				generation: generation,
				refreshed:  refreshed,
			}, nil
		}
		if entry.refresh != nil {
			call := entry.refresh
			c.mu.Unlock()
			set, generation, err := waitForJWKSRefresh(ctx, call)
			if err == nil {
				return jwksLoadResult{set: set, generation: generation, refreshed: true}, nil
			}
			if ctx.Err() != nil {
				return jwksLoadResult{}, ctx.Err()
			}
			return c.loadStaleAfterRefreshError(url, err)
		}
		if now.Before(entry.retryAfter) {
			if entry.set != nil && now.Before(entry.staleUntil) {
				result := jwksLoadResult{set: entry.set, generation: entry.generation, refreshErr: entry.lastRefreshErr}
				c.mu.Unlock()
				return result, nil
			}
			err := entry.lastRefreshErr
			c.mu.Unlock()
			if err == nil {
				err = errors.New("JWKS cache expired while refresh retry is deferred")
			}
			return jwksLoadResult{}, err
		}
	}
	c.mu.Unlock()

	set, generation, err := c.refresh(ctx, url)
	if err == nil {
		return jwksLoadResult{set: set, generation: generation, refreshed: true}, nil
	}
	if ctx.Err() != nil {
		return jwksLoadResult{}, ctx.Err()
	}
	return c.loadStaleAfterRefreshError(url, err)
}

func (c *jwksCache) loadStaleAfterRefreshError(url string, refreshErr error) (jwksLoadResult, error) {
	now := c.now()
	c.mu.Lock()
	entry := c.entries[url]
	if entry != nil && entry.set != nil && now.Before(entry.staleUntil) {
		result := jwksLoadResult{set: entry.set, generation: entry.generation, refreshErr: refreshErr}
		c.mu.Unlock()
		return result, nil
	}
	c.mu.Unlock()
	return jwksLoadResult{}, refreshErr
}

func (c *jwksCache) refresh(ctx context.Context, url string) (jwk.Set, uint64, error) {
	now := c.now()
	c.mu.Lock()
	entry, err := c.getOrCreateEntryLocked(url, now)
	if err != nil {
		c.mu.Unlock()
		return nil, 0, err
	}
	entry.lastUsed = now
	if entry.refresh != nil {
		call := entry.refresh
		c.mu.Unlock()
		return waitForJWKSRefresh(ctx, call)
	}
	if entry.set != nil && now.Before(entry.freshUntil) {
		set := entry.set
		generation := entry.generation
		c.mu.Unlock()
		return set, generation, nil
	}
	if now.Before(entry.retryAfter) {
		err := entry.lastRefreshErr
		generation := entry.generation
		c.mu.Unlock()
		if err == nil {
			err = errors.New("JWKS refresh retry is deferred")
		}
		return nil, generation, err
	}

	call := &jwksRefreshCall{done: make(chan struct{})}
	entry.refresh = call
	entry.retryAfter = now.Add(c.refreshRetryDelay)
	c.mu.Unlock()

	go c.completeRefresh(url, entry, call)
	return waitForJWKSRefresh(ctx, call)
}

func (c *jwksCache) refreshForUnknownKid(ctx context.Context, url, kid string, expectedGeneration uint64) (jwk.Set, uint64, bool, error) {
	now := c.now()
	digest := sha256.Sum256([]byte(kid))
	c.mu.Lock()
	entry := c.entries[url]
	if entry != nil && entry.refresh == nil && c.negativeKidActiveLocked(entry, digest, now) {
		set := entry.set
		generation := entry.generation
		c.mu.Unlock()
		return set, generation, false, nil
	}
	c.mu.Unlock()

	set, generation, refreshed, err := c.refreshForRotation(ctx, url, expectedGeneration)
	if err == nil && !refreshed {
		c.noteUnknownKid(url, kid, generation, false)
	}
	return set, generation, refreshed, err
}

func (c *jwksCache) refreshForRotation(ctx context.Context, url string, expectedGeneration uint64) (jwk.Set, uint64, bool, error) {
	now := c.now()
	c.mu.Lock()
	entry, err := c.getOrCreateEntryLocked(url, now)
	if err != nil {
		c.mu.Unlock()
		return nil, 0, false, err
	}
	entry.lastUsed = now
	if entry.refresh != nil {
		call := entry.refresh
		c.mu.Unlock()
		set, generation, err := waitForJWKSRefresh(ctx, call)
		return set, generation, true, err
	}
	if expectedGeneration != 0 && entry.set != nil && entry.generation != expectedGeneration && now.Before(entry.staleUntil) {
		set := entry.set
		generation := entry.generation
		c.mu.Unlock()
		return set, generation, true, nil
	}
	if now.Before(entry.forcedRefreshAfter) {
		set := entry.set
		generation := entry.generation
		c.mu.Unlock()
		return set, generation, false, nil
	}
	if now.Before(entry.retryAfter) {
		set := entry.set
		generation := entry.generation
		err := entry.lastRefreshErr
		c.mu.Unlock()
		if err == nil {
			return set, generation, false, nil
		}
		return set, generation, false, err
	}

	call := &jwksRefreshCall{done: make(chan struct{})}
	entry.refresh = call
	entry.forcedRefreshAfter = now.Add(c.forcedRefreshCooldown)
	entry.retryAfter = now.Add(c.refreshRetryDelay)
	c.mu.Unlock()

	go c.completeRefresh(url, entry, call)
	set, generation, err := waitForJWKSRefresh(ctx, call)
	return set, generation, true, err
}

func (c *jwksCache) completeRefresh(url string, entry *jwksCacheEntry, call *jwksRefreshCall) {
	set, err := c.fetch(url)
	now := c.now()

	c.mu.Lock()
	if err == nil {
		c.nextGeneration++
		if c.nextGeneration == 0 {
			c.nextGeneration++
		}
		entry.set = set
		entry.generation = c.nextGeneration
		entry.freshUntil = now.Add(c.freshTTL)
		entry.staleUntil = entry.freshUntil.Add(c.staleTTL)
		entry.retryAfter = time.Time{}
		entry.lastRefreshErr = nil
		entry.negativeKids = nil
	} else {
		entry.retryAfter = now.Add(c.refreshRetryDelay)
		entry.lastRefreshErr = err
	}
	entry.lastUsed = now
	if entry.refresh == call {
		entry.refresh = nil
	}
	call.set = set
	call.generation = entry.generation
	call.err = err
	close(call.done)
	c.mu.Unlock()
}

func waitForJWKSRefresh(ctx context.Context, call *jwksRefreshCall) (jwk.Set, uint64, error) {
	select {
	case <-call.done:
		return call.set, call.generation, call.err
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

func (c *jwksCache) fetch(url string) (jwk.Set, error) {
	// The shared refresh must outlive any one request waiting on it, while this
	// independent deadline still bounds the outbound operation.
	ctx, cancel := context.WithTimeout(context.Background(), c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxResponseBytes {
		return nil, fmt.Errorf("JWKS response exceeds maximum size of %d bytes", c.maxResponseBytes)
	}
	set, err := jwk.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	publicSet, err := publicJWKSForCache(set)
	if err != nil {
		return nil, fmt.Errorf("sanitize JWKS public keys: %w", err)
	}
	return publicSet, nil
}

func publicJWKSForCache(set jwk.Set) (jwk.Set, error) {
	publicSet := jwk.NewSet()
	for i := 0; i < set.Len(); i++ {
		key, ok := set.Key(i)
		if !ok {
			return nil, fmt.Errorf("JWK missing at index %d", i)
		}
		// Symmetric JWKs have no public form. Never retain their secret material,
		// but do not reject unrelated asymmetric signing keys in a mixed set.
		if key.KeyType() == jwa.OctetSeq() {
			continue
		}

		rawPublicKey, err := jwk.PublicRawKeyOf(key)
		if err != nil {
			return nil, fmt.Errorf("export public JWK at index %d: %w", i, err)
		}
		cleanKey, err := jwk.Import(rawPublicKey)
		if err != nil {
			return nil, fmt.Errorf("import public JWK at index %d: %w", i, err)
		}
		if err := copyPublicJWKMetadata(key, cleanKey); err != nil {
			return nil, fmt.Errorf("copy public JWK metadata at index %d: %w", i, err)
		}
		if err := publicSet.AddKey(cleanKey); err != nil {
			return nil, fmt.Errorf("add public JWK: %w", err)
		}
	}
	return publicSet, nil
}

func copyPublicJWKMetadata(source, destination jwk.Key) error {
	if value, ok := source.KeyID(); ok {
		if err := destination.Set(jwk.KeyIDKey, value); err != nil {
			return fmt.Errorf("set kid: %w", err)
		}
	}
	if value, ok := source.Algorithm(); ok {
		if err := destination.Set(jwk.AlgorithmKey, value); err != nil {
			return fmt.Errorf("set alg: %w", err)
		}
	}
	if value, ok := source.KeyUsage(); ok {
		if err := destination.Set(jwk.KeyUsageKey, value); err != nil {
			return fmt.Errorf("set use: %w", err)
		}
	}
	if value, ok := source.KeyOps(); ok {
		if err := destination.Set(jwk.KeyOpsKey, value); err != nil {
			return fmt.Errorf("set key_ops: %w", err)
		}
	}
	if value, ok := source.X509URL(); ok {
		if err := destination.Set(jwk.X509URLKey, value); err != nil {
			return fmt.Errorf("set x5u: %w", err)
		}
	}
	if value, ok := source.X509CertChain(); ok {
		if err := destination.Set(jwk.X509CertChainKey, value); err != nil {
			return fmt.Errorf("set x5c: %w", err)
		}
	}
	if value, ok := source.X509CertThumbprint(); ok {
		if err := destination.Set(jwk.X509CertThumbprintKey, value); err != nil {
			return fmt.Errorf("set x5t: %w", err)
		}
	}
	if value, ok := source.X509CertThumbprintS256(); ok {
		if err := destination.Set(jwk.X509CertThumbprintS256Key, value); err != nil {
			return fmt.Errorf("set x5t#S256: %w", err)
		}
	}
	return nil
}

func (c *jwksCache) noteUnknownKid(url, kid string, generation uint64, setCooldown bool) {
	now := c.now()
	digest := sha256.Sum256([]byte(kid))
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[url]
	if entry == nil || entry.generation != generation {
		return
	}
	entry.observedGeneration = generation
	if setCooldown {
		c.startForcedRefreshCooldownLocked(entry, now)
	}
	c.addNegativeKidLocked(entry, digest, now)
}

func (c *jwksCache) noteKeySelectionMiss(url, kid string, generation uint64, setCooldown bool) {
	if kid != "" {
		c.noteUnknownKid(url, kid, generation, setCooldown)
		return
	}
	c.noteKeylessFailure(url, generation, setCooldown)
}

func (c *jwksCache) noteKeylessFailure(url string, generation uint64, setCooldown bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[url]; entry != nil && entry.generation == generation {
		entry.observedGeneration = generation
		if !setCooldown {
			return
		}
		c.startForcedRefreshCooldownLocked(entry, now)
	}
}

func (c *jwksCache) noteSignatureFailure(url string, generation uint64) {
	c.noteKeylessFailure(url, generation, true)
}

func (c *jwksCache) noteVerificationSuccess(url string, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[url]; entry != nil && entry.generation == generation {
		entry.observedGeneration = generation
	}
}

func (c *jwksCache) startForcedRefreshCooldownLocked(entry *jwksCacheEntry, now time.Time) {
	if !now.Before(entry.forcedRefreshAfter) {
		entry.forcedRefreshAfter = now.Add(c.forcedRefreshCooldown)
	}
}

func (c *jwksCache) negativeKidActiveLocked(entry *jwksCacheEntry, digest [sha256.Size]byte, now time.Time) bool {
	if entry.negativeKids == nil {
		return false
	}
	expiresAt, ok := entry.negativeKids[digest]
	if !ok {
		return false
	}
	if now.Before(expiresAt) {
		return true
	}
	delete(entry.negativeKids, digest)
	return false
}

func (c *jwksCache) addNegativeKidLocked(entry *jwksCacheEntry, digest [sha256.Size]byte, now time.Time) {
	if entry.negativeKids == nil {
		entry.negativeKids = make(map[[sha256.Size]byte]time.Time)
	}
	if len(entry.negativeKids) >= c.maxNegativeKids {
		var oldestDigest [sha256.Size]byte
		var oldestExpiry time.Time
		for candidate, expiresAt := range entry.negativeKids {
			if !now.Before(expiresAt) {
				delete(entry.negativeKids, candidate)
				continue
			}
			if oldestExpiry.IsZero() || expiresAt.Before(oldestExpiry) {
				oldestDigest = candidate
				oldestExpiry = expiresAt
			}
		}
		if len(entry.negativeKids) >= c.maxNegativeKids && !oldestExpiry.IsZero() {
			delete(entry.negativeKids, oldestDigest)
		}
	}
	entry.negativeKids[digest] = now.Add(c.forcedRefreshCooldown)
}

func (c *jwksCache) getOrCreateEntryLocked(url string, now time.Time) (*jwksCacheEntry, error) {
	if entry := c.entries[url]; entry != nil {
		return entry, nil
	}

	if len(c.entries) >= c.maxEntries {
		var oldestURL string
		var oldestUsed time.Time
		for candidateURL, entry := range c.entries {
			if entry.refresh != nil {
				continue
			}
			if oldestURL == "" || entry.lastUsed.Before(oldestUsed) {
				oldestURL = candidateURL
				oldestUsed = entry.lastUsed
			}
		}
		if oldestURL == "" {
			return nil, errors.New("JWKS cache capacity is exhausted by active refreshes")
		}
		delete(c.entries, oldestURL)
	}

	entry := &jwksCacheEntry{lastUsed: now}
	c.entries[url] = entry
	return entry, nil
}

func parseCompactJWT(raw string) (*parsedJWT, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}

	msg, err := jws.Parse([]byte(raw), jws.WithCompact())
	if err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}

	signatures := msg.Signatures()
	if len(signatures) != 1 {
		return nil, errors.New("invalid JWT format")
	}

	protectedHeaders := signatures[0].ProtectedHeaders()
	if protectedHeaders == nil {
		return nil, errors.New("missing JWT protected header")
	}

	alg, ok := protectedHeaders.Algorithm()
	if !ok {
		return nil, errors.New("missing JWT signing algorithm")
	}
	keyID, _ := protectedHeaders.KeyID()
	typ, _ := protectedHeaders.Type()

	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}

	return &parsedJWT{
		Raw: raw,
		Header: jwtHeader{
			Algorithm: alg,
			KeyID:     keyID,
			Type:      typ,
		},
		RawClaims: json.RawMessage(rawClaims),
	}, nil
}

func jwtSigningAlgorithmAllowed(alg jwa.SignatureAlgorithm, allowed []jwa.SignatureAlgorithm) bool {
	if len(allowed) == 0 {
		allowed = []jwa.SignatureAlgorithm{jwa.RS256()}
	}

	for _, candidate := range allowed {
		if alg.String() == candidate.String() {
			return true
		}
	}
	return false
}

func jwtRequiredClaims(required []string) []string {
	if len(required) == 0 {
		return []string{jwt.SubjectKey, jwt.ExpirationKey}
	}
	return required
}

func jwtClaimRequired(name string, required []string) bool {
	return slices.Contains(jwtRequiredClaims(required), name)
}

func filterJWTSigningKeys(keySet jwk.Set, header jwtHeader) (jwk.Set, error) {
	expectedKeyType, err := jwtSigningKeyType(header.Algorithm)
	if err != nil {
		return nil, err
	}
	expectedKeyTypeName := expectedKeyType.String()

	filtered := jwk.NewSet()

	for i := 0; i < keySet.Len(); i++ {
		key, ok := keySet.Key(i)
		if !ok {
			continue
		}
		if header.KeyID != "" {
			keyID, ok := key.KeyID()
			if !ok || keyID != header.KeyID {
				continue
			}
		}
		if key.KeyType().String() != expectedKeyTypeName {
			continue
		}
		if use, ok := key.KeyUsage(); ok && use != "" && use != string(jwk.ForSignature) {
			continue
		}
		if alg, ok := key.Algorithm(); ok && alg != nil && alg.String() != header.Algorithm.String() {
			continue
		}

		clone, err := key.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone JWK: %w", err)
		}
		if alg, ok := clone.Algorithm(); !ok || alg == nil {
			if err := clone.Set(jwk.AlgorithmKey, header.Algorithm); err != nil {
				return nil, fmt.Errorf("set JWK algorithm: %w", err)
			}
		}
		if err := filtered.AddKey(clone); err != nil {
			return nil, fmt.Errorf("add JWK: %w", err)
		}
	}

	if filtered.Len() == 0 {
		if header.KeyID != "" {
			return nil, fmt.Errorf("%w: no usable %s signing key found for kid %q", errJWTSigningKeyNotFound, expectedKeyTypeName, header.KeyID)
		}
		return nil, fmt.Errorf("%w: no usable %s signing key found", errJWTSigningKeyNotFound, expectedKeyTypeName)
	}
	return filtered, nil
}

func jwtSigningKeyType(alg jwa.SignatureAlgorithm) (jwa.KeyType, error) {
	switch alg.String() {
	case jwa.RS256().String(), jwa.RS384().String(), jwa.RS512().String(),
		jwa.PS256().String(), jwa.PS384().String(), jwa.PS512().String():
		return jwa.RSA(), nil
	case jwa.ES256().String(), jwa.ES256K().String(), jwa.ES384().String(), jwa.ES512().String():
		return jwa.EC(), nil
	case jwa.EdDSA().String(), jwa.EdDSAEd25519().String():
		return jwa.OKP(), nil
	default:
		return jwa.InvalidKeyType(), fmt.Errorf("unsupported JWT signing algorithm %q", alg.String())
	}
}

func normalizeJWTValidationError(err error) error {
	switch {
	case errors.Is(err, jwt.InvalidAudienceError()):
		return fmt.Errorf("invalid audience: %w", err)
	case errors.Is(err, jwt.InvalidIssuerError()):
		return fmt.Errorf("invalid issuer: %w", err)
	case errors.Is(err, jwt.TokenExpiredError()):
		return fmt.Errorf("token expired: %w", err)
	case errors.Is(err, jwt.TokenNotYetValidError()):
		return fmt.Errorf("token not valid yet: %w", err)
	case errors.Is(err, jwt.MissingRequiredClaimError()):
		msg := err.Error()
		switch {
		case strings.Contains(msg, `"`+jwt.SubjectKey+`"`):
			return fmt.Errorf("missing subject: %w", err)
		case strings.Contains(msg, `"`+jwt.ExpirationKey+`"`):
			return fmt.Errorf("invalid expiration: %w", err)
		default:
			return fmt.Errorf("missing required claim: %w", err)
		}
	default:
		return err
	}
}
