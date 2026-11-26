package api

import (
	"bytes"
	"container/list"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/security"
)

const (
	githubAppAPIBaseURL           = "https://api.github.com"
	githubAppRequestTimeout       = 15 * time.Second
	githubAppJWTLifetime          = 9 * time.Minute
	githubAppJWTClockSkew         = 60 * time.Second
	maxGitHubAppResponseBytes     = 64 << 10
	defaultGitHubAppCooldown      = 60 * time.Second
	maxGitHubAppFallbackCooldown  = 16 * time.Minute
	githubAppThrottleStateIdleTTL = time.Hour
	maxGitHubAppVerifiedKeys      = 1024
	maxGitHubAppThrottleEntries   = 4096

	githubAppContentsPermission = "contents"
)

// isGitHubAppSecret reports whether secret is explicitly typed as a GitHub App
// credential Secret (orka.ai/github-app). Because the designation lives on the
// Secret itself, the broker refuses raw token or SSH-signing release from it
// regardless of the Task's reference type or whether the key parses.
func isCanonicalGitHubAppID(id string) bool {
	if len(id) == 0 || len(id) > 20 || id[0] == '0' {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func isGitHubAppSecret(secret *corev1.Secret) bool {
	return secret != nil && secret.Type == corev1.SecretType(corev1alpha1.GitHubAppSecretType)
}

// githubAppMintRequest is one installation-token request: the App, the
// repositories the token covers (all in one installation) and the permissions.
type githubAppMintRequest struct {
	AppID        string
	PrivateKey   *rsa.PrivateKey
	Owner        string
	Repositories []string
	Permissions  map[string]string
}

// githubAppTokenMinter mints a GitHub App installation token. It is an
// interface so broker tests can run without GitHub.
type githubAppTokenMinter interface {
	MintInstallationToken(ctx context.Context, request githubAppMintRequest) (string, error)
}

// githubAppMintError carries the broker status, a non-secret error code, and
// an optional Retry-After delay in whole seconds when GitHub throttles minting.
type githubAppMintError struct {
	status     int
	code       string
	retryAfter int64
}

func (e *githubAppMintError) Error() string { return e.code }

func githubAppRefused(code string) error {
	return &githubAppMintError{status: http.StatusForbidden, code: code}
}

func githubAppUnavailable() error {
	return &githubAppMintError{status: http.StatusServiceUnavailable, code: "github_app_unavailable"}
}

func githubAppUnavailableWithRetryAfter(retryAfter time.Duration) error {
	seconds := max(int64((retryAfter+time.Second-1)/time.Second), int64(1))
	return &githubAppMintError{status: http.StatusServiceUnavailable, code: "github_app_unavailable", retryAfter: seconds}
}

func githubAppRateLimitUnavailable(retryAfter time.Duration) error {
	seconds := max(int64((retryAfter+time.Second-1)/time.Second), int64(1))
	return &githubAppMintError{status: http.StatusTooManyRequests, code: "github_app_rate_limited", retryAfter: seconds}
}

// mintGitHubAppCredential turns a GitHubApp Secret into an installation token
// for the Task's repository and the role's permission.
func (s *Server) mintGitHubAppCredential(
	ctx context.Context,
	workspace *corev1alpha1.WorkspaceConfig,
	role publisherservice.CredentialRole,
	appID, privateKey []byte,
) (string, error) {
	request, err := githubAppRequestForRole(workspace, role)
	if err != nil {
		return "", err
	}
	request.AppID = strings.TrimSpace(string(appID))
	if !isCanonicalGitHubAppID(request.AppID) {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	if request.PrivateKey, err = parseGitHubAppPrivateKey(privateKey); err != nil {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	minter := s.githubAppMinter
	if minter == nil {
		s.githubAppMinterOnce.Do(func() {
			s.defaultGitHubAppMinter = newGitHubAppHTTPMinter(githubAppAPIBaseURL, nil)
		})
		minter = s.defaultGitHubAppMinter
	}
	return minter.MintInstallationToken(ctx, request)
}

// githubAppRequestForRole selects the repositories and permissions for one
// credential role. Source reads cover the source repository; target reads and
// writes cover the publication repository; the forge role covers the pull
// request base and head, because reconciliation reads the head ref and opens
// the pull request on the base.
func githubAppRequestForRole(workspace *corev1alpha1.WorkspaceConfig, role publisherservice.CredentialRole) (githubAppMintRequest, error) {
	if workspace == nil {
		return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
	}
	source := strings.TrimSpace(workspace.GitRepo)
	target := strings.TrimSpace(workspace.PublicationGitRepo)
	if target == "" {
		target = source
	}
	var repositoryURLs []string
	var permissions map[string]string
	switch role {
	case publisherservice.CredentialRoleSourceRead:
		repositoryURLs, permissions = []string{source}, map[string]string{githubAppContentsPermission: githubPermissionRead}
	case publisherservice.CredentialRoleTargetRead:
		repositoryURLs, permissions = []string{target}, map[string]string{githubAppContentsPermission: githubPermissionRead}
	case publisherservice.CredentialRoleTargetWrite:
		repositoryURLs, permissions = []string{target}, map[string]string{githubAppContentsPermission: githubPermissionWrite}
	case publisherservice.CredentialRoleForge:
		repositoryURLs = []string{source, target}
		permissions = map[string]string{"pull_requests": githubPermissionWrite, githubAppContentsPermission: githubPermissionRead}
	default:
		return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
	}
	request := githubAppMintRequest{Permissions: permissions}
	for _, repositoryURL := range repositoryURLs {
		owner, name, err := security.ParseGitHubRepositoryURL(repositoryURL)
		if err != nil {
			return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
		}
		if request.Owner == "" {
			request.Owner = owner
		} else if !strings.EqualFold(request.Owner, owner) {
			// One installation token covers one account's repositories.
			return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
		}
		duplicate := false
		for _, existing := range request.Repositories {
			duplicate = duplicate || strings.EqualFold(existing, name)
		}
		if !duplicate {
			request.Repositories = append(request.Repositories, name)
		}
	}
	return request, nil
}

func parseGitHubAppPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(data))
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}
	return parseGitHubAppPrivateKeyBlock(block)
}

func parseGitHubAppPrivateKeyBlock(block *pem.Block) (*rsa.PrivateKey, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("private key PEM type is unsupported")
	}
}

type githubAppThrottleState struct {
	until          time.Time
	secondaryUntil time.Time
	consecutive    int
	lastDelay      time.Duration
	lastUpdated    time.Time
	unavailable    bool
	orderElem      *list.Element
}

type githubAppVerifiedKey struct {
	appID    string
	lastUsed time.Time
}

type githubAppKeyRevocation struct {
	gen      uint64
	lastUsed time.Time
}

type githubAppHTTPMinter struct {
	baseURL string
	client  *http.Client
	now     func() time.Time

	cooldownMu     sync.Mutex
	throttles      map[string]githubAppThrottleState
	throttleOrder  *list.List
	verifiedKeys   map[string]githubAppVerifiedKey
	revocationGens map[string]githubAppKeyRevocation
	revocationSeq  uint64
}

func newGitHubAppHTTPMinter(baseURL string, client *http.Client) *githubAppHTTPMinter {
	if client == nil {
		client = &http.Client{Timeout: githubAppRequestTimeout}
	} else {
		clone := *client
		client = &clone
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &githubAppHTTPMinter{
		baseURL:        strings.TrimRight(baseURL, "/"),
		client:         client,
		now:            time.Now,
		throttles:      map[string]githubAppThrottleState{},
		throttleOrder:  list.New(),
		verifiedKeys:   map[string]githubAppVerifiedKey{},
		revocationGens: map[string]githubAppKeyRevocation{},
	}
}

func throttleStateExpiredIdle(state githubAppThrottleState, now time.Time) bool {
	if state.until.After(now) {
		return false
	}
	idleSince := state.lastUpdated
	if state.until.After(idleSince) {
		idleSince = state.until
	}
	return now.Sub(idleSince) >= githubAppThrottleStateIdleTTL
}

func (m *githubAppHTTPMinter) deleteThrottleLocked(key string) {
	if state, ok := m.throttles[key]; ok {
		if state.orderElem != nil {
			m.throttleOrder.Remove(state.orderElem)
		}
		delete(m.throttles, key)
	}
}

func (m *githubAppHTTPMinter) setThrottleLocked(key string, state githubAppThrottleState, now time.Time) {
	if existing, ok := m.throttles[key]; ok && existing.orderElem != nil {
		state.orderElem = existing.orderElem
		m.throttleOrder.MoveToBack(state.orderElem)
		m.throttles[key] = state
		return
	}
	if len(m.throttles) >= maxGitHubAppThrottleEntries {
		// Evict oldest expired-idle entries first, then oldest inactive entries,
		// so active cooldowns are preserved and cache growth stays bounded in O(1)
		// amortized work per insert.
		evicted := false
		for elem := m.throttleOrder.Front(); elem != nil && len(m.throttles) >= maxGitHubAppThrottleEntries; {
			next := elem.Next()
			k := elem.Value.(string)
			if s, ok := m.throttles[k]; !ok || throttleStateExpiredIdle(s, now) {
				m.deleteThrottleLocked(k)
				evicted = true
			} else if !evicted && !s.until.After(now) {
				m.deleteThrottleLocked(k)
				evicted = true
				break
			}
			elem = next
		}
		if len(m.throttles) >= maxGitHubAppThrottleEntries {
			// All entries have active cooldowns; evict the oldest unverified key
			// entry first, or the oldest entry overall as a last resort.
			var fallback string
			for elem := m.throttleOrder.Front(); elem != nil; elem = elem.Next() {
				k := elem.Value.(string)
				if fallback == "" {
					fallback = k
				}
				if strings.HasPrefix(k, "key:") {
					if _, verified := m.verifiedKeys[k]; !verified {
						m.deleteThrottleLocked(k)
						fallback = ""
						break
					}
				}
			}
			if fallback != "" {
				m.deleteThrottleLocked(fallback)
			}
		}
	}
	state.orderElem = m.throttleOrder.PushBack(key)
	m.throttles[key] = state
}

func (m *githubAppHTTPMinter) activeCooldownError(keys []string) error {
	now := m.now()
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	var best time.Duration
	bestUnavailable := false
	for _, key := range keys {
		state, ok := m.throttles[key]
		if !ok {
			continue
		}
		if remaining := state.until.Sub(now); remaining > 0 {
			if remaining > best || (remaining == best && !state.unavailable) {
				best = remaining
				bestUnavailable = state.unavailable
			}
			continue
		}
		if throttleStateExpiredIdle(state, now) {
			m.deleteThrottleLocked(key)
		}
	}
	if best <= 0 {
		return nil
	}
	if bestUnavailable {
		return githubAppUnavailableWithRetryAfter(best)
	}
	return githubAppRateLimitUnavailable(best)
}

func (m *githubAppHTTPMinter) pruneExpiredFrontLocked(now time.Time) {
	// Trim expired-idle entries from the oldest end in O(1) amortized time
	// instead of scanning the entire map on every insertion.
	for elem := m.throttleOrder.Front(); elem != nil; {
		next := elem.Next()
		key := elem.Value.(string)
		state, ok := m.throttles[key]
		if ok && !throttleStateExpiredIdle(state, now) {
			break
		}
		m.deleteThrottleLocked(key)
		elem = next
	}
}

type githubAppThrottleScope struct {
	appID        string
	keyOnlyKeys  []string
	verifiedKeys []string
	verified     bool
}

func (m *githubAppHTTPMinter) recordKeysLocked(scope githubAppThrottleScope, now time.Time) []string {
	if scope.verified {
		return scope.verifiedKeys
	}
	if len(scope.keyOnlyKeys) > 0 && strings.HasPrefix(scope.keyOnlyKeys[0], "key:") {
		if entry, ok := m.verifiedKeys[scope.keyOnlyKeys[0]]; ok && entry.appID == scope.appID {
			entry.lastUsed = now
			m.verifiedKeys[scope.keyOnlyKeys[0]] = entry
			return scope.verifiedKeys
		}
	}
	return scope.keyOnlyKeys
}

func (m *githubAppHTTPMinter) recordRateLimit(scope githubAppThrottleScope, header http.Header) time.Duration {
	now := m.now()
	primaryExhausted := strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0"
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	m.pruneExpiredFrontLocked(now)
	keys := m.recordKeysLocked(scope, now)
	var bestDelay time.Duration
	for _, key := range keys {
		state := m.throttles[key]
		if primaryExhausted {
			delay := githubAppExplicitRetryDelay(header, now)
			if delay <= 0 {
				delay = defaultGitHubAppCooldown
			}
			if until := now.Add(delay); !until.Before(state.until) {
				state.until = until
				state.unavailable = false
			}
			state.lastUpdated = now
			m.setThrottleLocked(key, state, now)
			bestDelay = max(bestDelay, state.until.Sub(now))
			continue
		}
		if state.secondaryUntil.After(now) {
			// A secondary-limit cooldown is already active for this round; do not
			// compound backoff for concurrent responses, but still honor the current
			// response's explicit header or secondary-limit minimum.
			minDelay := githubAppRetryDelay(header, now, 1, 0)
			state.lastDelay = max(state.lastDelay, minDelay)
			if until := now.Add(minDelay); !until.Before(state.until) {
				state.until = until
				state.unavailable = false
			}
			if until := now.Add(minDelay); until.After(state.secondaryUntil) {
				state.secondaryUntil = until
			}
			state.lastUpdated = now
			m.setThrottleLocked(key, state, now)
			bestDelay = max(bestDelay, state.until.Sub(now))
			continue
		}
		priorDelay := state.lastDelay
		state.consecutive++
		delay := githubAppRetryDelay(header, now, state.consecutive, priorDelay)
		until := now.Add(delay)
		if !until.Before(state.until) {
			state.until = until
			state.unavailable = false
		}
		state.secondaryUntil = until
		state.lastDelay = delay
		state.lastUpdated = now
		m.setThrottleLocked(key, state, now)
		bestDelay = max(bestDelay, state.until.Sub(now))
	}
	return bestDelay
}

func (m *githubAppHTTPMinter) recordServiceUnavailableRetryAfter(scope githubAppThrottleScope, header http.Header) time.Duration {
	now := m.now()
	delay := parseRetryAfterHeader(header, now)
	if delay <= 0 {
		return 0
	}
	until := now.Add(delay)
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	m.pruneExpiredFrontLocked(now)
	keys := m.recordKeysLocked(scope, now)
	var bestDelay time.Duration
	for _, key := range keys {
		state := m.throttles[key]
		if until.After(state.until) {
			state.until = until
			state.unavailable = true
		}
		state.lastUpdated = now
		m.setThrottleLocked(key, state, now)
		bestDelay = max(bestDelay, state.until.Sub(now))
	}
	return bestDelay
}

func (m *githubAppHTTPMinter) recordExhaustedQuota(scope githubAppThrottleScope, header http.Header) {
	if strings.TrimSpace(header.Get("X-RateLimit-Remaining")) != "0" {
		return
	}
	now := m.now()
	raw := strings.TrimSpace(header.Get("X-RateLimit-Reset"))
	if raw == "" {
		return
	}
	unixSec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return
	}
	until := time.Unix(unixSec, 0)
	if !until.After(now) {
		return
	}
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	m.pruneExpiredFrontLocked(now)
	keys := m.recordKeysLocked(scope, now)
	for _, key := range keys {
		state := m.throttles[key]
		if until.After(state.until) {
			state.until = until
			state.unavailable = false
		}
		state.lastUpdated = now
		m.setThrottleLocked(key, state, now)
	}
}

func (m *githubAppHTTPMinter) resetRateLimit(appID string, keys []string) {
	now := m.now()
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	resetKey := func(key string) {
		if state, ok := m.throttles[key]; ok && state.until.After(now) {
			// Preserve secondary backoff counters if a concurrent secondary
			// rate limit is currently active; otherwise (e.g. a primary quota
			// reset from this successful response), clear secondary history.
			if !state.secondaryUntil.After(now) {
				state.consecutive = 0
				state.lastDelay = 0
				m.setThrottleLocked(key, state, now)
			}
			return
		}
		m.deleteThrottleLocked(key)
	}
	for _, key := range keys {
		resetKey(key)
	}
	if appID != "" {
		for fp, entry := range m.verifiedKeys {
			if entry.appID == appID {
				resetKey(fp)
			}
		}
	}
}

func (m *githubAppHTTPMinter) currentRevocationGen(keyFingerprint string) uint64 {
	if keyFingerprint == "" {
		return 0
	}
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	return m.revocationGens[keyFingerprint].gen
}

func (m *githubAppHTTPMinter) invalidateVerifiedKey(scope githubAppThrottleScope) {
	if len(scope.keyOnlyKeys) == 0 || !strings.HasPrefix(scope.keyOnlyKeys[0], "key:") {
		return
	}
	keyFingerprint := scope.keyOnlyKeys[0]
	now := m.now()
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if _, exists := m.revocationGens[keyFingerprint]; !exists && len(m.revocationGens) >= maxGitHubAppVerifiedKeys {
		var oldestKey string
		var oldestTime time.Time
		for fp, entry := range m.revocationGens {
			if oldestKey == "" || entry.lastUsed.Before(oldestTime) {
				oldestKey = fp
				oldestTime = entry.lastUsed
			}
		}
		if oldestKey != "" {
			delete(m.revocationGens, oldestKey)
		}
	}
	m.revocationSeq++
	m.revocationGens[keyFingerprint] = githubAppKeyRevocation{
		gen:      m.revocationSeq,
		lastUsed: now,
	}
	if entry, ok := m.verifiedKeys[keyFingerprint]; ok && entry.appID == scope.appID {
		delete(m.verifiedKeys, keyFingerprint)
	}
}

func (m *githubAppHTTPMinter) markKeyVerifiedForApp(keyFingerprint, appID string, expectedGen uint64) {
	if keyFingerprint == "" || appID == "" {
		return
	}
	now := m.now()
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if m.revocationGens[keyFingerprint].gen != expectedGen {
		return
	}
	if _, exists := m.verifiedKeys[keyFingerprint]; !exists && len(m.verifiedKeys) >= maxGitHubAppVerifiedKeys {
		// Evict the least-recently-used entry rather than discarding active associations.
		var oldestKey string
		var oldestTime time.Time
		for fp, entry := range m.verifiedKeys {
			if oldestKey == "" || entry.lastUsed.Before(oldestTime) {
				oldestKey = fp
				oldestTime = entry.lastUsed
			}
		}
		if oldestKey != "" {
			delete(m.verifiedKeys, oldestKey)
		}
	}
	m.verifiedKeys[keyFingerprint] = githubAppVerifiedKey{appID: appID, lastUsed: now}
	// Promote any unexpired key-only throttle or secondary backoff history
	// recorded before verification onto the shared App scope so other signing
	// keys for this App honor both active cooldowns and consecutive backoff.
	keyState, ok := m.throttles[keyFingerprint]
	if !ok || throttleStateExpiredIdle(keyState, now) {
		return
	}
	appKey := "app:" + appID
	appState := m.throttles[appKey]
	if keyState.until.After(appState.until) || (keyState.until.Equal(appState.until) && !keyState.unavailable) {
		appState.until = keyState.until
		appState.unavailable = keyState.unavailable
	}
	if keyState.secondaryUntil.After(appState.secondaryUntil) {
		appState.secondaryUntil = keyState.secondaryUntil
	}
	appState.consecutive = max(appState.consecutive, keyState.consecutive)
	appState.lastDelay = max(appState.lastDelay, keyState.lastDelay)
	if keyState.lastUpdated.After(appState.lastUpdated) {
		appState.lastUpdated = keyState.lastUpdated
	}
	m.setThrottleLocked(appKey, appState, now)
}

func githubAppKeyThrottleKeys(request githubAppMintRequest) []string {
	if request.PrivateKey != nil {
		if der, err := x509.MarshalPKIXPublicKey(&request.PrivateKey.PublicKey); err == nil {
			sum := sha256.Sum256(der)
			return []string{"key:" + base64.RawURLEncoding.EncodeToString(sum[:])}
		}
	}
	return []string{"app:" + request.AppID}
}

func githubAppVerifiedThrottleKeys(request githubAppMintRequest) []string {
	keys := githubAppKeyThrottleKeys(request)
	appKey := "app:" + request.AppID
	if len(keys) == 0 || keys[0] != appKey {
		keys = append(keys, appKey)
	}
	return keys
}

func (m *githubAppHTTPMinter) MintInstallationToken(ctx context.Context, request githubAppMintRequest) (string, error) {
	if request.PrivateKey == nil || request.AppID == "" || request.Owner == "" || len(request.Repositories) == 0 {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	// Before GitHub has ever authenticated this signing key for request.AppID
	// (or after a 401 invalidates a revoked key), key failure cooldowns only by
	// the signing-key fingerprint so an unverified or revoked key cannot poison
	// another key's cooldown. While verified, share cooldowns across the App.
	keyOnlyKeys := githubAppKeyThrottleKeys(request)
	verifiedKeys := githubAppVerifiedThrottleKeys(request)
	scope := githubAppThrottleScope{
		appID:        request.AppID,
		keyOnlyKeys:  keyOnlyKeys,
		verifiedKeys: verifiedKeys,
	}
	if err := m.activeCooldownError(verifiedKeys); err != nil {
		return "", err
	}
	appJWT, err := signGitHubAppJWT(request.AppID, request.PrivateKey, m.now())
	if err != nil {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	var installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	var keyFingerprint string
	if len(keyOnlyKeys) > 0 && strings.HasPrefix(keyOnlyKeys[0], "key:") {
		keyFingerprint = keyOnlyKeys[0]
	}
	lookupGen := m.currentRevocationGen(keyFingerprint)
	installationPath := "/repos/" + url.PathEscape(request.Owner) + "/" + url.PathEscape(request.Repositories[0]) + "/installation"
	finalInstallationURL, err := m.do(ctx, verifiedKeys, scope, http.MethodGet, installationPath, appJWT, nil, http.StatusOK, &installation)
	if err != nil {
		return "", err
	}
	if keyFingerprint != "" {
		m.markKeyVerifiedForApp(keyFingerprint, request.AppID, lookupGen)
	}
	if installation.ID < 1 {
		return "", githubAppUnavailable()
	}
	installationOwner := strings.TrimSpace(installation.Account.Login)
	if installationOwner == "" {
		return "", githubAppUnavailable()
	}
	if !strings.EqualFold(installationOwner, request.Owner) {
		return "", githubAppRefused("github_app_repository_invalid")
	}
	body := map[string]any{"permissions": request.Permissions}
	if finalInstallationURL.EscapedPath() == installationPath {
		body["repositories"] = request.Repositories
	} else if repoID, ok := parseGitHubInstallationRepositoryID(finalInstallationURL.Path); ok && len(request.Repositories) == 1 {
		body["repository_ids"] = []int64{repoID}
	} else {
		// The lookup redirected away from the named repository path and the
		// broker cannot bind every requested repository to an immutable ID.
		return "", githubAppRefused("github_app_repository_invalid")
	}
	var token struct {
		Token string `json:"token"`
	}
	tokenPath := "/app/installations/" + strconv.FormatInt(installation.ID, 10) + "/access_tokens"
	if _, err := m.do(ctx, verifiedKeys, scope, http.MethodPost, tokenPath, appJWT, body, http.StatusCreated, &token); err != nil {
		return "", err
	}
	token.Token = strings.TrimSpace(token.Token)
	if token.Token == "" || len(token.Token) > maxBrokeredCredentialBytes || strings.ContainsAny(token.Token, " \t\r\n\x00") {
		return "", githubAppUnavailable()
	}
	m.resetRateLimit(request.AppID, verifiedKeys)
	return token.Token, nil
}

func parseGitHubInstallationRepositoryID(path string) (int64, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "repositories" || parts[2] != "installation" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

func (m *githubAppHTTPMinter) do(ctx context.Context, checkKeys []string, recordScope githubAppThrottleScope, method, path, appJWT string, body any, wantStatus int, out any) (*url.URL, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, githubAppUnavailable()
		}
	}
	const maxGitHubAppRedirects = 3
	targetURL := m.baseURL + path
	for redirects := 0; ; redirects++ {
		if err := m.activeCooldownError(checkKeys); err != nil {
			return nil, err
		}
		var reader io.Reader
		if encoded != nil {
			reader = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
		if err != nil {
			return nil, githubAppUnavailable()
		}
		request.Header.Set("Authorization", "Bearer "+appJWT)
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if encoded != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := m.client.Do(request)
		if err != nil {
			return nil, githubAppUnavailable()
		}
		nextURL, handled, err := m.handleResponse(response, recordScope, request.URL, wantStatus, out)
		_ = response.Body.Close()
		if !handled {
			if redirects >= maxGitHubAppRedirects {
				return nil, githubAppUnavailable()
			}
			targetURL = nextURL
			continue
		}
		return request.URL, err
	}
}

func (m *githubAppHTTPMinter) handleResponse(response *http.Response, recordScope githubAppThrottleScope, requestURL *url.URL, wantStatus int, out any) (string, bool, error) {
	// If GitHub rejects authentication, drop any cached verification for this
	// signing key before recording quota headers so a revoked key cannot
	// attribute subsequent throttles to the App's replacement key.
	if response.StatusCode == http.StatusUnauthorized {
		m.invalidateVerifiedKey(recordScope)
		recordScope.verified = false
	}
	// Handle status/header-indicated throttling before reading the body so a
	// truncated 429 or header-bearing 403 still records GitHub's retry delay.
	if response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden && githubAppHeaderRateLimited(response.Header)) {
		delay := m.recordRateLimit(recordScope, response.Header)
		return "", true, githubAppRateLimitUnavailable(delay)
	}
	// Record exhausted primary quota as soon as headers arrive, regardless of
	// status code or whether the response body can be read completely. Once
	// GitHub returns the expected authenticated status, share the quota reset
	// across all verified keys for the App (including App ID).
	quotaScope := recordScope
	if response.StatusCode == wantStatus {
		quotaScope.verified = true
	}
	m.recordExhaustedQuota(quotaScope, response.Header)
	if response.StatusCode == http.StatusServiceUnavailable {
		if delay := m.recordServiceUnavailableRetryAfter(recordScope, response.Header); delay > 0 {
			return "", true, githubAppUnavailableWithRetryAfter(delay)
		}
	}
	// Classify definitive non-body-dependent statuses before reading the body so
	// truncated or oversized error bodies never turn them into retryable 503s.
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		nextURL, sameOrigin := m.resolveSameOriginLocation(requestURL, response.Header.Get("Location"))
		if !sameOrigin {
			return "", true, githubAppRefused("github_app_rejected")
		}
		return nextURL, false, nil
	case http.StatusNotFound:
		return "", true, githubAppRefused("github_app_not_installed")
	case http.StatusUnauthorized, http.StatusUnprocessableEntity:
		return "", true, githubAppRefused("github_app_rejected")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubAppResponseBytes+1))
	if err != nil || len(data) > maxGitHubAppResponseBytes {
		return "", true, githubAppUnavailable()
	}
	switch {
	case response.StatusCode == wantStatus:
	case response.StatusCode == http.StatusForbidden && githubAppBodyRateLimited(data):
		delay := m.recordRateLimit(recordScope, response.Header)
		return "", true, githubAppRateLimitUnavailable(delay)
	case response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= http.StatusInternalServerError:
		return "", true, githubAppUnavailable()
	default:
		// 403 (permission or repository not granted to the installation) and
		// anything unexpected.
		return "", true, githubAppRefused("github_app_rejected")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return "", true, githubAppUnavailable()
	}
	return "", true, nil
}

func (m *githubAppHTTPMinter) resolveSameOriginLocation(requestURL *url.URL, location string) (string, bool) {
	location = strings.TrimSpace(location)
	if location == "" || requestURL == nil {
		return "", false
	}
	base, err := url.Parse(m.baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", false
	}
	target, err := requestURL.Parse(location)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return "", false
	}
	return target.String(), true
}

func parseRetryAfterHeader(header http.Header, now time.Time) time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return delta
		}
	}
	return 0
}

func githubAppExplicitRetryDelay(header http.Header, now time.Time) time.Duration {
	best := parseRetryAfterHeader(header, now)
	if strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0" {
		if raw := strings.TrimSpace(header.Get("X-RateLimit-Reset")); raw != "" {
			if unixSec, err := strconv.ParseInt(raw, 10, 64); err == nil {
				if delta := time.Unix(unixSec, 0).Sub(now); delta > 0 {
					best = max(best, delta)
				}
			}
		}
	}
	return best
}

// githubAppRetryDelay extracts the wait duration requested by GitHub's
// Retry-After and (when primary quota is exhausted) X-RateLimit-Reset headers,
// returning the longer applicable delay. For repeated secondary rate limits
// (when primary quota is not exhausted), it treats Retry-After as a minimum
// and backs off exponentially from the prior effective delay across
// consecutive rounds.
func githubAppRetryDelay(header http.Header, now time.Time, consecutive int, priorDelay time.Duration) time.Duration {
	best := githubAppExplicitRetryDelay(header, now)
	primaryExhausted := strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0"
	if (primaryExhausted || consecutive <= 1) && best > 0 {
		return best
	}
	backoff := defaultGitHubAppCooldown
	if consecutive > 1 {
		if priorDelay > 0 {
			backoff = max(min(priorDelay*2, maxGitHubAppFallbackCooldown), priorDelay, defaultGitHubAppCooldown)
		} else {
			for i := 1; i < consecutive && backoff < maxGitHubAppFallbackCooldown; i++ {
				backoff = min(backoff*2, maxGitHubAppFallbackCooldown)
			}
		}
	}
	return max(best, backoff)
}

// githubAppHeaderRateLimited reports response headers that establish GitHub
// throttling without needing to inspect the body.
func githubAppHeaderRateLimited(header http.Header) bool {
	return strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0" || strings.TrimSpace(header.Get("Retry-After")) != ""
}

func githubAppBodyRateLimited(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("rate limit")) ||
		bytes.Contains(lower, []byte("maximum number of login attempts exceeded"))
}

// signGitHubAppJWT signs the short-lived RS256 App JWT GitHub requires for
// App-level endpoints. iat is backdated for clock skew; exp stays under
// GitHub's ten-minute limit.
func signGitHubAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-githubAppJWTClockSkew).Unix(),
		"exp": now.Add(githubAppJWTLifetime).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func githubAppErrorResponse(err error) (int, string, int64) {
	if mintErr, ok := errors.AsType[*githubAppMintError](err); ok {
		return mintErr.status, mintErr.code, mintErr.retryAfter
	}
	return http.StatusServiceUnavailable, "github_app_unavailable", 0
}
