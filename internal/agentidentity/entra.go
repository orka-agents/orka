// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

const (
	// DefaultEntraAuthority is the public-cloud Entra authority.
	DefaultEntraAuthority = "https://login.microsoftonline.com"
	// DefaultTokenFile is where the controller's projected ServiceAccount
	// token, audienced to api://AzureADTokenExchange, is mounted.
	DefaultTokenFile = "/var/run/secrets/orka.ai/agent-identity/token"

	entraExchangeScope = "api://AzureADTokenExchange/.default"
	// entraVerificationResource is the resource an identity's own token is
	// requested for when verifying it; the token is never used.
	entraVerificationResource = "api://AzureADTokenExchange"
	jwtBearerAssertionType    = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	jwtBearerGrantType        = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	formGrantType            = "grant_type"
	formClientID             = "client_id"
	formScope                = "scope"
	formClientAssertionType  = "client_assertion_type"
	formClientAssertion      = "client_assertion"
	maxCachedTokenLifetime   = 5 * time.Minute
	tokenExpirySafetyMargin  = time.Minute
	maxTokenResponseBytes    = 1 << 20
	defaultEntraHTTPDeadline = 15 * time.Second
)

// ErrIssuerUnavailable marks an issuer that could not answer, as opposed to
// one that refused: a timeout, a network failure, throttling, or a server
// error. Callers may retry it later.
var ErrIssuerUnavailable = errors.New("the identity issuer is unavailable")

// entraFlightDeadline bounds one shared fetch: an app token needs the
// exchange token first, so two requests.
const entraFlightDeadline = 2 * defaultEntraHTTPDeadline

// maxCachedTokens bounds the token cache however many identities, targets,
// and policies the controller serves.
const maxCachedTokens = 1024

// EntraIssuer acquires Entra agent identity tokens with the blueprint flow:
// the blueprint authenticates with the controller's federated credential and
// receives an exchange token for one agent identity (fmi_path), which that
// identity then presents to get a token for the target resource.
type EntraIssuer struct {
	// Config names the tenant, blueprint, and authority, the same values the
	// issuer instance frozen into Task bindings is derived from.
	Config EntraConfig
	// Credential returns the controller's federated credential. Defaults to
	// reading DefaultTokenFile on every call, since the kubelet rotates it.
	Credential func() (string, error)
	HTTPClient *http.Client
	Now        func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
	// flights coalesces concurrent misses for one cache key into a single
	// issuer request.
	flights singleflight.Group
	// flightJoined, when set, runs once a caller has started or joined a
	// flight; tests use it to order callers.
	flightJoined func()
}

type cachedToken struct {
	value   string
	expires time.Time
}

// FileCredential reads a projected ServiceAccount token from path.
func FileCredential(path string) func() (string, error) {
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the controller's federated credential: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", errors.New("the controller's federated credential is empty")
		}
		return token, nil
	}
}

// ExchangeToken returns the blueprint's exchange token for one agent identity.
// Obtaining it proves the identity is a child of the configured blueprint.
func (i *EntraIssuer) ExchangeToken(ctx context.Context, identityID string) (string, error) {
	identityID = strings.ToLower(strings.TrimSpace(identityID))
	if !guidPattern.MatchString(identityID) {
		return "", errors.New("Entra agent identity IDs must be GUIDs") //nolint:staticcheck // Product name begins the message.
	}
	return i.cached(ctx, "exchange\x00"+identityID, func(ctx context.Context) (string, time.Duration, error) {
		credential, err := i.credential()
		if err != nil {
			return "", 0, err
		}
		return i.requestToken(ctx, "entra_exchange", url.Values{
			formGrantType:           {"client_credentials"},
			formClientID:            {strings.ToLower(i.Config.BlueprintClientID)},
			formScope:               {entraExchangeScope},
			"fmi_path":              {identityID},
			formClientAssertionType: {jwtBearerAssertionType},
			formClientAssertion:     {credential},
		})
	})
}

// AppToken returns an app-only token for the agent identity and resource,
// cached apart from any policy's tokens. It is how an identity is verified.
func (i *EntraIssuer) AppToken(ctx context.Context, identityID, resource string) (string, error) {
	return i.appToken(ctx, identityID, resource, outboundaccess.PolicyIdentity{})
}

// appToken returns an app-only token for the agent identity and resource,
// cached under the policy that asked for it, so an edited or recreated
// policy never reuses its predecessor's token.
func (i *EntraIssuer) appToken(ctx context.Context, identityID, resource string, policy outboundaccess.PolicyIdentity) (string, error) {
	identityID = strings.ToLower(strings.TrimSpace(identityID))
	resource = strings.TrimSpace(resource)
	if resource == "" || strings.HasSuffix(resource, "/.default") {
		return "", errors.New("an Entra resource without the /.default suffix is required")
	}
	return i.cached(ctx, appTokenCacheKey("app", identityID, resource, policy), func(ctx context.Context) (string, time.Duration, error) {
		exchange, err := i.ExchangeToken(ctx, identityID)
		if err != nil {
			return "", 0, err
		}
		return i.requestToken(ctx, "entra_app", url.Values{
			formGrantType:           {"client_credentials"},
			formClientID:            {identityID},
			formScope:               {entraScope(resource)},
			formClientAssertionType: {jwtBearerAssertionType},
			formClientAssertion:     {exchange},
		})
	})
}

// OnBehalfOfToken returns a token for the agent identity acting for the user
// whose token is userAssertion, cached under the policy that asked for it and
// the user token, so an edited or recreated policy, or another requester,
// never reuses it. The user token must be audienced to the blueprint; Entra
// rejects any other.
func (i *EntraIssuer) OnBehalfOfToken(ctx context.Context, identityID, resource, userAssertion string, policy outboundaccess.PolicyIdentity) (string, error) {
	identityID = strings.ToLower(strings.TrimSpace(identityID))
	resource = strings.TrimSpace(resource)
	if resource == "" || strings.HasSuffix(resource, "/.default") {
		return "", errors.New("an Entra resource without the /.default suffix is required")
	}
	if strings.TrimSpace(userAssertion) == "" {
		return "", errors.New("an on-behalf-of exchange requires the requester's token")
	}
	user := sha256.Sum256([]byte(userAssertion))
	return i.cached(ctx, appTokenCacheKey("obo", identityID, resource, policy)+"\x00"+hex.EncodeToString(user[:]), func(ctx context.Context) (string, time.Duration, error) {
		exchange, err := i.ExchangeToken(ctx, identityID)
		if err != nil {
			return "", 0, err
		}
		return i.requestToken(ctx, "entra_obo", url.Values{
			formGrantType:           {jwtBearerGrantType},
			formClientID:            {identityID},
			formScope:               {entraScope(resource)},
			formClientAssertionType: {jwtBearerAssertionType},
			formClientAssertion:     {exchange},
			"assertion":             {userAssertion},
			"requested_token_use":   {"on_behalf_of"},
		})
	})
}

// entraScope is the resource's .default scope. The resource is kept exactly
// as configured: one registered with a trailing slash, such as
// https://database.windows.net/, needs it to select that audience.
func entraScope(resource string) string {
	return resource + "/.default"
}

// appTokenCacheKey identifies a token by flow, identity, target, and the
// policy UID and generation it was issued for (ADR 0039).
func appTokenCacheKey(flow, identityID, resource string, policy outboundaccess.PolicyIdentity) string {
	return flow + "\x00" + identityID + "\x00" + resource + "\x00" + policy.UID + "\x00" + strconv.FormatInt(policy.Generation, 10)
}

func (i *EntraIssuer) credential() (string, error) {
	if i.Credential != nil {
		return i.Credential()
	}
	return FileCredential(DefaultTokenFile)()
}

func (i *EntraIssuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// cached returns a live cached token or fetches one. An entry lives at most
// five minutes, and never past a minute before the token expires. Every
// flow's tokens share this cache and its bound.
//
// Concurrent misses for one key share a single fetch. The fetch runs under a
// context no caller can cancel, bounded by entraFlightDeadline, so a caller
// that gives up does not fail the others; each caller still returns when its
// own context ends. A caller whose deadline passes while it waits gets
// ErrIssuerUnavailable. A failed fetch is returned to every waiter and
// nothing is cached.
func (i *EntraIssuer) cached(ctx context.Context, key string, fetch func(context.Context) (string, time.Duration, error)) (string, error) {
	digest := sha256.Sum256([]byte(key))
	cacheKey := hex.EncodeToString(digest[:])
	if token, ok := i.lookup(cacheKey); ok {
		return token, nil
	}
	results := i.flights.DoChan(cacheKey, func() (any, error) {
		if token, ok := i.lookup(cacheKey); ok {
			return token, nil
		}
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), entraFlightDeadline)
		defer cancel()
		now := i.now()
		token, lifetime, err := fetch(flightCtx)
		if err != nil {
			return "", err
		}
		lifetime -= tokenExpirySafetyMargin
		if lifetime > maxCachedTokenLifetime {
			lifetime = maxCachedTokenLifetime
		}
		if lifetime > 0 {
			i.mu.Lock()
			i.storeLocked(cacheKey, cachedToken{value: token, expires: now.Add(lifetime)}, i.now())
			i.mu.Unlock()
		}
		return token, nil
	})
	if i.flightJoined != nil {
		i.flightJoined()
	}
	select {
	case result := <-results:
		if result.Err != nil {
			return "", result.Err
		}
		token, _ := result.Val.(string)
		return token, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("the Entra token request did not finish in time: %w", ErrIssuerUnavailable)
		}
		return "", ctx.Err()
	}
}

// lookup returns a live cached token, dropping an expired one.
func (i *EntraIssuer) lookup(cacheKey string) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	entry, ok := i.cache[cacheKey]
	if !ok {
		return "", false
	}
	if i.now().Before(entry.expires) {
		return entry.value, true
	}
	delete(i.cache, cacheKey)
	return "", false
}

// storeLocked caches one entry. It first drops every expired entry, so keys
// never looked up again are reclaimed, and when the cache is still full it
// evicts the entries closest to expiry. The caller holds i.mu.
func (i *EntraIssuer) storeLocked(key string, entry cachedToken, now time.Time) {
	if i.cache == nil {
		i.cache = map[string]cachedToken{}
	}
	for existing, cached := range i.cache {
		if !now.Before(cached.expires) {
			delete(i.cache, existing)
		}
	}
	if _, replacing := i.cache[key]; !replacing {
		for len(i.cache) >= maxCachedTokens {
			oldest, oldestExpiry := "", time.Time{}
			for existing, cached := range i.cache {
				if oldest == "" || cached.expires.Before(oldestExpiry) {
					oldest, oldestExpiry = existing, cached.expires
				}
			}
			delete(i.cache, oldest)
		}
	}
	i.cache[key] = entry
}

type entraTokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Error       string          `json:"error"`
	ErrorCodes  []int           `json:"error_codes"`
}

// requestToken posts one token request. Errors carry only Entra's error code,
// never a credential or the response body.
func (i *EntraIssuer) requestToken(ctx context.Context, step string, form url.Values) (token string, lifetime time.Duration, err error) {
	started := time.Now()
	reason := "issued"
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}
		metrics.RecordTokenExchange("agentIdentity", step, result, reason, time.Since(started).Seconds())
	}()
	endpoint := i.Config.authority() + "/" + url.PathEscape(strings.ToLower(i.Config.TenantID)) + "/oauth2/v2.0/token"
	ctx, cancel := context.WithTimeout(ctx, defaultEntraHTTPDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build the Entra token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: defaultEntraHTTPDeadline}
	if i.HTTPClient != nil {
		copied := *i.HTTPClient
		client = &copied
	}
	// A redirect would replay the form, which carries the controller's
	// federated credential or the blueprint's exchange token, to wherever
	// the response points. Entra's token endpoint never redirects, so the
	// 3xx is returned as a refusal instead.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		reason = "unreachable"
		return "", 0, fmt.Errorf("the Entra token endpoint is unreachable: %w", ErrIssuerUnavailable)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes))
	if err != nil {
		return "", 0, fmt.Errorf("read the Entra token response: %w", ErrIssuerUnavailable)
	}
	var parsed entraTokenResponse
	_ = json.Unmarshal(body, &parsed)
	if response.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		code := strings.TrimSpace(parsed.Error)
		if code == "" {
			code = "unknown_error"
		}
		reason = code
		if response.StatusCode >= http.StatusInternalServerError || response.StatusCode == http.StatusTooManyRequests ||
			code == "temporarily_unavailable" {
			return "", 0, fmt.Errorf("Entra could not answer the token request: %s (HTTP %d): %w", code, response.StatusCode, ErrIssuerUnavailable) //nolint:staticcheck // Product name begins the message.
		}
		if len(parsed.ErrorCodes) > 0 {
			return "", 0, fmt.Errorf("Entra refused the token request: %s (AADSTS%d, HTTP %d)", code, parsed.ErrorCodes[0], response.StatusCode) //nolint:staticcheck // Product name begins the message.
		}
		return "", 0, fmt.Errorf("Entra refused the token request: %s (HTTP %d)", code, response.StatusCode) //nolint:staticcheck // Product name begins the message.
	}
	return parsed.AccessToken, parseExpiresIn(parsed.ExpiresIn), nil
}

// parseExpiresIn accepts expires_in as a number or a numeric string. An
// unknown lifetime is treated as zero, so the token is used once and not cached.
func parseExpiresIn(raw json.RawMessage) time.Duration {
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err == nil {
		return time.Duration(seconds) * time.Second
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		var parsed int64
		if _, err := fmt.Sscanf(strings.TrimSpace(text), "%d", &parsed); err == nil {
			return time.Duration(parsed) * time.Second
		}
	}
	return 0
}

// Source issues Agent identity credentials for the controller's resolver and
// verifies identities for the Agent controller.
type Source struct {
	Config Config
	Entra  *EntraIssuer
}

// AgentIdentityToken implements outboundaccess.AgentIdentityTokenSource.
func (s *Source) AgentIdentityToken(ctx context.Context, req outboundaccess.AgentIdentityTokenRequest) (string, error) {
	if s == nil {
		return "", errors.New("no agent identity issuer is configured")
	}
	instance, ok := s.Config.Instance(req.Identity.Issuer)
	if !ok {
		return "", fmt.Errorf("identity issuer %q is not configured on this controller", req.Identity.Issuer)
	}
	if instance != req.Identity.Instance {
		return "", errors.New("the identity issuer configuration changed since the Task was bound; re-run the Task")
	}
	switch req.Identity.Issuer {
	case corev1alpha1.AgentIdentityIssuerEntra:
		if s.Entra == nil {
			return "", errors.New("the Entra issuer is not configured on this controller")
		}
		switch req.Mode {
		case corev1alpha1.OutboundAgentIdentityModeSelf:
			return s.Entra.appToken(ctx, req.Identity.ID, req.Resource, req.Policy)
		case corev1alpha1.OutboundAgentIdentityModeDelegated:
			// Entra enforces both sides: the user's own rights at the
			// resource, and what was granted to the agent identity.
			if err := requireAudience(req.UserAssertion, s.Entra.Config.BlueprintClientID); err != nil {
				return "", err
			}
			return s.Entra.OnBehalfOfToken(ctx, req.Identity.ID, req.Resource, req.UserAssertion, req.Policy)
		default:
			return "", fmt.Errorf("agent identity mode %q is not supported", req.Mode)
		}
	default:
		return "", fmt.Errorf("identity issuer %q is not supported", req.Identity.Issuer)
	}
}

// Verify proves an Agent's identity with its issuer.
func (s *Source) Verify(ctx context.Context, agent *corev1alpha1.Agent) error {
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return nil
	}
	if s == nil {
		return errors.New("no agent identity issuer is configured")
	}
	switch issuer {
	case corev1alpha1.AgentIdentityIssuerEntra:
		if s.Entra == nil {
			return errors.New("the Entra issuer is not configured on this controller")
		}
		// The blueprint's exchange token alone does not prove the identity
		// is usable: Entra still issues it for a disabled agent identity.
		// The identity's own token does, and Entra issues it for this
		// cloud-neutral resource whatever the identity has been granted.
		_, err := s.Entra.AppToken(ctx, id, entraVerificationResource)
		return err
	default:
		return fmt.Errorf("identity issuer %q is not supported", issuer)
	}
}
