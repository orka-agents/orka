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
	"strings"
	"sync"
	"time"

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

	entraExchangeScope       = "api://AzureADTokenExchange/.default"
	jwtBearerAssertionType   = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	maxCachedTokenLifetime   = 5 * time.Minute
	tokenExpirySafetyMargin  = time.Minute
	maxTokenResponseBytes    = 1 << 20
	defaultEntraHTTPDeadline = 15 * time.Second
)

// EntraIssuer acquires Entra agent identity tokens with the blueprint flow:
// the blueprint authenticates with the controller's federated credential and
// receives an exchange token for one agent identity (fmi_path), which that
// identity then presents to get a token for the target resource.
type EntraIssuer struct {
	Config EntraConfig
	// Authority is the Entra authority URL; DefaultEntraAuthority when empty.
	Authority string
	// Credential returns the controller's federated credential. Defaults to
	// reading DefaultTokenFile on every call, since the kubelet rotates it.
	Credential func() (string, error)
	HTTPClient *http.Client
	Now        func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
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
	return i.cached("exchange\x00"+identityID, func() (string, time.Duration, error) {
		credential, err := i.credential()
		if err != nil {
			return "", 0, err
		}
		return i.requestToken(ctx, "entra_exchange", url.Values{
			"grant_type":            {"client_credentials"},
			"client_id":             {strings.ToLower(i.Config.BlueprintClientID)},
			"scope":                 {entraExchangeScope},
			"fmi_path":              {identityID},
			"client_assertion_type": {jwtBearerAssertionType},
			"client_assertion":      {credential},
		})
	})
}

// AppToken returns an app-only token for the agent identity and resource.
func (i *EntraIssuer) AppToken(ctx context.Context, identityID, resource string) (string, error) {
	identityID = strings.ToLower(strings.TrimSpace(identityID))
	resource = strings.TrimSpace(resource)
	if resource == "" || strings.HasSuffix(resource, "/.default") {
		return "", errors.New("an Entra resource without the /.default suffix is required")
	}
	return i.cached("app\x00"+identityID+"\x00"+resource, func() (string, time.Duration, error) {
		exchange, err := i.ExchangeToken(ctx, identityID)
		if err != nil {
			return "", 0, err
		}
		return i.requestToken(ctx, "entra_app", url.Values{
			"grant_type":            {"client_credentials"},
			"client_id":             {identityID},
			"scope":                 {strings.TrimSuffix(resource, "/") + "/.default"},
			"client_assertion_type": {jwtBearerAssertionType},
			"client_assertion":      {exchange},
		})
	})
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
// five minutes, and never past a minute before the token expires.
func (i *EntraIssuer) cached(key string, fetch func() (string, time.Duration, error)) (string, error) {
	digest := sha256.Sum256([]byte(key))
	cacheKey := hex.EncodeToString(digest[:])
	now := i.now()
	i.mu.Lock()
	if entry, ok := i.cache[cacheKey]; ok && now.Before(entry.expires) {
		i.mu.Unlock()
		return entry.value, nil
	}
	i.mu.Unlock()

	token, lifetime, err := fetch()
	if err != nil {
		return "", err
	}
	lifetime -= tokenExpirySafetyMargin
	if lifetime > maxCachedTokenLifetime {
		lifetime = maxCachedTokenLifetime
	}
	if lifetime > 0 {
		i.mu.Lock()
		if i.cache == nil {
			i.cache = map[string]cachedToken{}
		}
		i.cache[cacheKey] = cachedToken{value: token, expires: now.Add(lifetime)}
		i.mu.Unlock()
	}
	return token, nil
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
	authority := strings.TrimSuffix(strings.TrimSpace(i.Authority), "/")
	if authority == "" {
		authority = DefaultEntraAuthority
	}
	endpoint := authority + "/" + url.PathEscape(strings.ToLower(i.Config.TenantID)) + "/oauth2/v2.0/token"
	ctx, cancel := context.WithTimeout(ctx, defaultEntraHTTPDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build the Entra token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	client := i.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultEntraHTTPDeadline}
	}
	response, err := client.Do(request)
	if err != nil {
		reason = "unreachable"
		return "", 0, errors.New("the Entra token endpoint is unreachable")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes))
	if err != nil {
		return "", 0, errors.New("read the Entra token response")
	}
	var parsed entraTokenResponse
	_ = json.Unmarshal(body, &parsed)
	if response.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		code := strings.TrimSpace(parsed.Error)
		if code == "" {
			code = "unknown_error"
		}
		reason = code
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
	if req.Mode != corev1alpha1.OutboundAgentIdentityModeSelf {
		return "", fmt.Errorf("agent identity mode %q is not supported", req.Mode)
	}
	switch req.Identity.Issuer {
	case corev1alpha1.AgentIdentityIssuerEntra:
		if s.Entra == nil {
			return "", errors.New("the Entra issuer is not configured on this controller")
		}
		return s.Entra.AppToken(ctx, req.Identity.ID, req.Resource)
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
		_, err := s.Entra.ExchangeToken(ctx, id)
		return err
	default:
		return fmt.Errorf("identity issuer %q is not supported", issuer)
	}
}
