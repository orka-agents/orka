/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jws"
)

const (
	// Discovery metadata is reused for five minutes. Unlike signing keys, it has
	// no stale grace: once expired, an unreachable issuer fails closed.
	defaultOIDCDiscoveryTTL                   = 5 * time.Minute
	defaultOIDCDiscoveryRefreshRetryDelay     = 5 * time.Second
	defaultOIDCDiscoveryForcedRefreshCooldown = 5 * time.Second
	defaultMaxOIDCDiscoveryResponseBytes      = int64(64 << 10)
	defaultMaxOIDCDiscoveryCacheEntries       = 64
)

type oidcDiscoveryCacheOptions struct {
	client                *http.Client
	now                   func() time.Time
	requestTimeout        time.Duration
	ttl                   time.Duration
	refreshRetryDelay     time.Duration
	forcedRefreshCooldown time.Duration
	maxResponseBytes      int64
	maxEntries            int
}

type oidcDiscoveryCache struct {
	client                *http.Client
	now                   func() time.Time
	requestTimeout        time.Duration
	ttl                   time.Duration
	refreshRetryDelay     time.Duration
	forcedRefreshCooldown time.Duration
	maxResponseBytes      int64
	maxEntries            int

	mu             sync.Mutex
	entries        map[string]*oidcDiscoveryCacheEntry
	nextGeneration uint64
}

type oidcDiscoveryCacheEntry struct {
	jwksURL            string
	expiresAt          time.Time
	retryAfter         time.Time
	lastRefreshErr     error
	forcedRefreshAfter time.Time
	generation         uint64
	observedGeneration uint64
	lastUsed           time.Time
	refresh            *oidcDiscoveryRefreshCall
}

type oidcDiscoveryRefreshCall struct {
	done       chan struct{}
	jwksURL    string
	generation uint64
	err        error
}

type oidcDiscoverySelection struct {
	jwksURL    string
	generation uint64
	refreshed  bool
}

type oidcDiscoveryDocument struct {
	JWKSURI string `json:"jwks_uri"`
}

func newOIDCDiscoveryCache(opts oidcDiscoveryCacheOptions) *oidcDiscoveryCache {
	if opts.requestTimeout <= 0 {
		opts.requestTimeout = authHTTPTimeout
	}
	if opts.client == nil {
		opts.client = &http.Client{Timeout: opts.requestTimeout}
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.ttl <= 0 {
		opts.ttl = defaultOIDCDiscoveryTTL
	}
	if opts.refreshRetryDelay <= 0 {
		opts.refreshRetryDelay = defaultOIDCDiscoveryRefreshRetryDelay
	}
	if opts.forcedRefreshCooldown <= 0 {
		opts.forcedRefreshCooldown = defaultOIDCDiscoveryForcedRefreshCooldown
	}
	if opts.maxResponseBytes <= 0 {
		opts.maxResponseBytes = defaultMaxOIDCDiscoveryResponseBytes
	}
	if opts.maxEntries <= 0 {
		opts.maxEntries = defaultMaxOIDCDiscoveryCacheEntries
	}
	return &oidcDiscoveryCache{
		client:                opts.client,
		now:                   opts.now,
		requestTimeout:        opts.requestTimeout,
		ttl:                   opts.ttl,
		refreshRetryDelay:     opts.refreshRetryDelay,
		forcedRefreshCooldown: opts.forcedRefreshCooldown,
		maxResponseBytes:      opts.maxResponseBytes,
		maxEntries:            opts.maxEntries,
		entries:               make(map[string]*oidcDiscoveryCacheEntry),
	}
}

func (v *jwtVerifier) verifyParsedJWTWithDiscovery(ctx context.Context, parsed *parsedJWT, cfg jwtVerificationConfig) (*verifiedJWT, error) {
	if jwksURL := strings.TrimSpace(cfg.JWKSURL); jwksURL != "" {
		cfg.JWKSURL = jwksURL
		return v.verifyParsedJWT(ctx, parsed, cfg)
	}

	selection, err := v.discovery.load(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("fetch OIDC discovery document: %w", err)
	}
	cfg.JWKSURL = selection.jwksURL
	verified, verifyErr := v.verifyParsedJWT(ctx, parsed, cfg)
	if verifyErr == nil {
		v.discovery.noteVerificationSuccess(cfg.Issuer, selection.generation)
		return verified, nil
	}
	if ctx.Err() != nil {
		return verified, verifyErr
	}
	if !jwtVerificationMayNeedDiscoveryRefresh(verifyErr) {
		v.discovery.noteVerificationSuccess(cfg.Issuer, selection.generation)
		return verified, verifyErr
	}
	if selection.refreshed {
		v.discovery.noteVerificationFailure(cfg.Issuer, selection.generation)
		return nil, verifyErr
	}

	currentURL, currentGeneration, _, refreshErr := v.discovery.refreshForRotation(ctx, cfg.Issuer, selection.generation)
	if refreshErr != nil {
		return nil, fmt.Errorf("%v; refresh OIDC discovery: %w", verifyErr, refreshErr)
	}
	if currentURL == "" || currentURL == selection.jwksURL {
		v.discovery.noteVerificationFailure(cfg.Issuer, currentGeneration)
		return nil, verifyErr
	}
	cfg.JWKSURL = currentURL
	verified, verifyErr = v.verifyParsedJWT(ctx, parsed, cfg)
	if verifyErr == nil {
		v.discovery.noteVerificationSuccess(cfg.Issuer, currentGeneration)
		return verified, nil
	}
	if ctx.Err() == nil && !jwtVerificationMayNeedDiscoveryRefresh(verifyErr) {
		v.discovery.noteVerificationSuccess(cfg.Issuer, currentGeneration)
	} else if ctx.Err() == nil {
		v.discovery.noteVerificationFailure(cfg.Issuer, currentGeneration)
	}
	return verified, verifyErr
}

func jwtVerificationMayNeedDiscoveryRefresh(err error) bool {
	return errors.Is(err, errJWTKeyResolution) ||
		errors.Is(err, errJWTSigningKeyNotFound) ||
		errors.Is(err, jws.VerificationError())
}

func (c *oidcDiscoveryCache) load(ctx context.Context, issuer string) (oidcDiscoverySelection, error) {
	now := c.now()
	c.mu.Lock()
	entry := c.entries[issuer]
	if entry != nil {
		entry.lastUsed = now
		if entry.jwksURL != "" && now.Before(entry.expiresAt) {
			selection := oidcDiscoverySelection{
				jwksURL:    entry.jwksURL,
				generation: entry.generation,
				refreshed:  entry.observedGeneration != entry.generation,
			}
			c.mu.Unlock()
			return selection, nil
		}
		if entry.refresh != nil {
			call := entry.refresh
			c.mu.Unlock()
			jwksURL, generation, err := waitForOIDCDiscoveryRefresh(ctx, call)
			return oidcDiscoverySelection{jwksURL: jwksURL, generation: generation, refreshed: err == nil}, err
		}
		if now.Before(entry.retryAfter) {
			err := entry.lastRefreshErr
			c.mu.Unlock()
			if err == nil {
				err = errors.New("OIDC discovery refresh retry is deferred")
			}
			return oidcDiscoverySelection{}, err
		}
	}
	c.mu.Unlock()

	jwksURL, generation, err := c.refresh(ctx, issuer)
	return oidcDiscoverySelection{jwksURL: jwksURL, generation: generation, refreshed: err == nil}, err
}

func (c *oidcDiscoveryCache) refresh(ctx context.Context, issuer string) (string, uint64, error) {
	now := c.now()
	c.mu.Lock()
	entry, err := c.getOrCreateEntryLocked(issuer, now)
	if err != nil {
		c.mu.Unlock()
		return "", 0, err
	}
	entry.lastUsed = now
	if entry.refresh != nil {
		call := entry.refresh
		c.mu.Unlock()
		return waitForOIDCDiscoveryRefresh(ctx, call)
	}
	if entry.jwksURL != "" && now.Before(entry.expiresAt) {
		jwksURL := entry.jwksURL
		generation := entry.generation
		c.mu.Unlock()
		return jwksURL, generation, nil
	}
	if now.Before(entry.retryAfter) {
		err := entry.lastRefreshErr
		generation := entry.generation
		c.mu.Unlock()
		if err == nil {
			err = errors.New("OIDC discovery refresh retry is deferred")
		}
		return "", generation, err
	}

	call := &oidcDiscoveryRefreshCall{done: make(chan struct{})}
	entry.refresh = call
	entry.retryAfter = now.Add(c.refreshRetryDelay)
	c.mu.Unlock()
	go c.completeRefresh(issuer, entry, call)
	return waitForOIDCDiscoveryRefresh(ctx, call)
}

func (c *oidcDiscoveryCache) refreshForRotation(ctx context.Context, issuer string, expectedGeneration uint64) (string, uint64, bool, error) {
	now := c.now()
	c.mu.Lock()
	entry, err := c.getOrCreateEntryLocked(issuer, now)
	if err != nil {
		c.mu.Unlock()
		return "", 0, false, err
	}
	entry.lastUsed = now
	if entry.refresh != nil {
		call := entry.refresh
		c.mu.Unlock()
		jwksURL, generation, err := waitForOIDCDiscoveryRefresh(ctx, call)
		return jwksURL, generation, true, err
	}
	if expectedGeneration != 0 && entry.jwksURL != "" && entry.generation != expectedGeneration && now.Before(entry.expiresAt) {
		jwksURL := entry.jwksURL
		generation := entry.generation
		c.mu.Unlock()
		return jwksURL, generation, true, nil
	}
	if now.Before(entry.forcedRefreshAfter) {
		jwksURL := entry.jwksURL
		generation := entry.generation
		c.mu.Unlock()
		return jwksURL, generation, false, nil
	}
	if now.Before(entry.retryAfter) {
		jwksURL := entry.jwksURL
		generation := entry.generation
		err := entry.lastRefreshErr
		c.mu.Unlock()
		if err == nil {
			return jwksURL, generation, false, nil
		}
		return jwksURL, generation, false, err
	}

	call := &oidcDiscoveryRefreshCall{done: make(chan struct{})}
	entry.refresh = call
	entry.forcedRefreshAfter = now.Add(c.forcedRefreshCooldown)
	entry.retryAfter = now.Add(c.refreshRetryDelay)
	c.mu.Unlock()
	go c.completeRefresh(issuer, entry, call)
	jwksURL, generation, err := waitForOIDCDiscoveryRefresh(ctx, call)
	return jwksURL, generation, true, err
}

func (c *oidcDiscoveryCache) noteVerificationFailure(issuer string, generation uint64) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[issuer]; entry != nil && entry.generation == generation {
		entry.observedGeneration = generation
		if !now.Before(entry.forcedRefreshAfter) {
			entry.forcedRefreshAfter = now.Add(c.forcedRefreshCooldown)
		}
	}
}

func (c *oidcDiscoveryCache) noteVerificationSuccess(issuer string, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[issuer]; entry != nil && entry.generation == generation {
		entry.observedGeneration = generation
	}
}

func (c *oidcDiscoveryCache) completeRefresh(issuer string, entry *oidcDiscoveryCacheEntry, call *oidcDiscoveryRefreshCall) {
	jwksURL, err := c.fetch(issuer)
	now := c.now()
	c.mu.Lock()
	if err == nil {
		c.nextGeneration++
		if c.nextGeneration == 0 {
			c.nextGeneration++
		}
		entry.jwksURL = jwksURL
		entry.generation = c.nextGeneration
		entry.expiresAt = now.Add(c.ttl)
		entry.retryAfter = time.Time{}
		entry.lastRefreshErr = nil
	} else {
		entry.retryAfter = now.Add(c.refreshRetryDelay)
		entry.lastRefreshErr = err
	}
	entry.lastUsed = now
	if entry.refresh == call {
		entry.refresh = nil
	}
	call.jwksURL = jwksURL
	call.generation = entry.generation
	call.err = err
	close(call.done)
	c.mu.Unlock()
}

func waitForOIDCDiscoveryRefresh(ctx context.Context, call *oidcDiscoveryRefreshCall) (string, uint64, error) {
	select {
	case <-call.done:
		return call.jwksURL, call.generation, call.err
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
}

func (c *oidcDiscoveryCache) fetch(issuer string) (string, error) {
	// Discovery refreshes are shared across requests, so they use an independent
	// bounded deadline instead of being canceled with the first waiter.
	ctx, cancel := context.WithTimeout(context.Background(), c.requestTimeout)
	defer cancel()
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > c.maxResponseBytes {
		return "", fmt.Errorf("OIDC discovery response exceeds maximum size of %d bytes", c.maxResponseBytes)
	}
	var document oidcDiscoveryDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return "", fmt.Errorf("parse OIDC discovery document: %w", err)
	}
	if strings.TrimSpace(document.JWKSURI) == "" {
		return "", errors.New("OIDC discovery document missing jwks_uri")
	}
	return strings.TrimSpace(document.JWKSURI), nil
}

func (c *oidcDiscoveryCache) getOrCreateEntryLocked(issuer string, now time.Time) (*oidcDiscoveryCacheEntry, error) {
	if entry := c.entries[issuer]; entry != nil {
		return entry, nil
	}
	if len(c.entries) >= c.maxEntries {
		var oldestIssuer string
		var oldestUsed time.Time
		for candidateIssuer, entry := range c.entries {
			if entry.refresh != nil {
				continue
			}
			if oldestIssuer == "" || entry.lastUsed.Before(oldestUsed) {
				oldestIssuer = candidateIssuer
				oldestUsed = entry.lastUsed
			}
		}
		if oldestIssuer == "" {
			return nil, errors.New("OIDC discovery cache capacity is exhausted by active refreshes")
		}
		delete(c.entries, oldestIssuer)
	}
	entry := &oidcDiscoveryCacheEntry{lastUsed: now}
	c.entries[issuer] = entry
	return entry, nil
}
