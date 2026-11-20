// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/entraclient"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

const (
	// DefaultEntraAuthority is the public-cloud Entra authority.
	DefaultEntraAuthority = entraclient.DefaultAuthority
	// DefaultTokenFile is where the controller's projected ServiceAccount
	// token, audienced to api://AzureADTokenExchange, is mounted.
	DefaultTokenFile = "/var/run/secrets/orka.ai/agent-identity/token"

	entraExchangeScope = "api://AzureADTokenExchange/.default"
	// entraVerificationResource is the resource an identity's own token is
	// requested for when verifying it; the token is never used.
	entraVerificationResource = "api://AzureADTokenExchange"
	jwtBearerAssertionType    = entraclient.JWTBearerAssertionType
	jwtBearerGrantType        = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	formGrantType           = "grant_type"
	formClientID            = "client_id"
	formScope               = "scope"
	formClientAssertionType = "client_assertion_type"
	formClientAssertion     = "client_assertion"
	maxCachedTokenLifetime  = entraclient.MaxCachedTokenLifetime
)

// ErrIssuerUnavailable marks an issuer that could not answer, as opposed to
// one that refused: a timeout, a network failure, throttling, or a server
// error. Callers may retry it later.
var ErrIssuerUnavailable = entraclient.ErrIssuerUnavailable

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

	cacheOnce sync.Once
	cache     *entraclient.Cache
	// flightJoined, when set, runs once a caller has started or joined a
	// flight; tests use it to order callers.
	flightJoined func()
}

// FileCredential reads a projected ServiceAccount token from path. A missing
// or empty token is ErrIssuerUnavailable: Entra was never asked, so it says
// nothing about the identity.
func FileCredential(path string) func() (string, error) {
	return entraclient.FileCredential(path)
}

// ExchangeToken returns the blueprint's exchange token for one agent identity.
// Obtaining it proves the identity is a child of the configured blueprint.
func (i *EntraIssuer) ExchangeToken(ctx context.Context, identityID string) (string, error) {
	identityID = strings.ToLower(strings.TrimSpace(identityID))
	if !guidPattern.MatchString(identityID) {
		return "", errors.New("Entra agent identity IDs must be GUIDs") //nolint:staticcheck // Product name begins the message.
	}
	return i.cached(ctx, "exchange\x00"+identityID, func(ctx context.Context) (string, time.Duration, error) {
		return i.requestExchangeToken(ctx, identityID)
	})
}

func (i *EntraIssuer) requestExchangeToken(ctx context.Context, identityID string) (string, time.Duration, error) {
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
}

func (i *EntraIssuer) requestAppToken(ctx context.Context, identityID, resource, exchange string) (string, time.Duration, error) {
	return i.requestToken(ctx, "entra_app", url.Values{
		formGrantType:           {"client_credentials"},
		formClientID:            {identityID},
		formScope:               {entraScope(resource)},
		formClientAssertionType: {jwtBearerAssertionType},
		formClientAssertion:     {exchange},
	})
}

// VerifyIdentity asks Entra, past the token cache, for the identity's own
// token for resource. A cached token is up to five minutes old, so treating
// it as proof would let an Agent edit renew trust in an identity Entra has
// since disabled. The tokens it obtains are not cached or returned.
func (i *EntraIssuer) VerifyIdentity(ctx context.Context, identityID, resource string) error {
	identityID = strings.ToLower(strings.TrimSpace(identityID))
	if !guidPattern.MatchString(identityID) {
		return errors.New("Entra agent identity IDs must be GUIDs") //nolint:staticcheck // Product name begins the message.
	}
	exchange, _, err := i.requestExchangeToken(ctx, identityID)
	if err != nil {
		return err
	}
	_, _, err = i.requestAppToken(ctx, identityID, resource, exchange)
	return err
}

// AppToken returns an app-only token for the agent identity and resource,
// cached apart from any policy's tokens.
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
		return i.requestAppToken(ctx, identityID, resource, exchange)
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

// cached returns a live cached token or fetches one; see entraclient.Cache.
// Every flow's tokens share this cache and its bound.
func (i *EntraIssuer) cached(ctx context.Context, key string, fetch func(context.Context) (string, time.Duration, error)) (string, error) {
	return i.tokenCache().Get(ctx, key, fetch)
}

func (i *EntraIssuer) tokenCache() *entraclient.Cache {
	i.cacheOnce.Do(func() {
		i.cache = &entraclient.Cache{Now: i.now, FlightJoined: i.flightJoined}
	})
	return i.cache
}

// requestToken posts one token request. Errors carry only Entra's error code,
// never a credential or the response body.
func (i *EntraIssuer) requestToken(ctx context.Context, step string, form url.Values) (string, time.Duration, error) {
	client := entraclient.Client{
		Authority:     i.Config.authority(),
		TenantID:      i.Config.TenantID,
		HTTPClient:    i.HTTPClient,
		MetricAdapter: "agentIdentity",
	}
	return client.RequestToken(ctx, step, form)
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
	// Identities are honored only while Agent use is enforced; a claim that
	// outlived a restart without the check must not get tokens.
	if !s.Config.AgentUseAuthorization {
		return "", errors.New("agent identities require --agent-use-authorization-enabled on the controller")
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
		// It is asked for fresh, never taken from the cache.
		return s.Entra.VerifyIdentity(ctx, id, entraVerificationResource)
	default:
		return fmt.Errorf("identity issuer %q is not supported", issuer)
	}
}
