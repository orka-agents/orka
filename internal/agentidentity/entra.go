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
	DefaultEntraAuthority = entraclient.DefaultAuthority
	DefaultTokenFile      = "/var/run/secrets/orka.ai/agent-identity/token"

	entraExchangeScope        = "api://AzureADTokenExchange/.default"
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

var ErrIssuerUnavailable = entraclient.ErrIssuerUnavailable

type EntraIssuer struct {
	Config     EntraConfig
	Credential func() (string, error)
	HTTPClient *http.Client
	Now        func() time.Time

	cacheOnce    sync.Once
	cache        *entraclient.Cache
	flightJoined func()
}
func FileCredential(path string) func() (string, error) {
	return entraclient.FileCredential(path)
}

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

func (i *EntraIssuer) AppToken(ctx context.Context, identityID, resource string) (string, error) {
	return i.appToken(ctx, identityID, resource, outboundaccess.PolicyIdentity{})
}

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

func entraScope(resource string) string {
	return resource + "/.default"
}

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

func (i *EntraIssuer) cached(ctx context.Context, key string, fetch func(context.Context) (string, time.Duration, error)) (string, error) {
	return i.tokenCache().Get(ctx, key, fetch)
}

func (i *EntraIssuer) tokenCache() *entraclient.Cache {
	i.cacheOnce.Do(func() {
		i.cache = &entraclient.Cache{Now: i.now, FlightJoined: i.flightJoined}
	})
	return i.cache
}

func (i *EntraIssuer) requestToken(ctx context.Context, step string, form url.Values) (string, time.Duration, error) {
	client := entraclient.Client{
		Authority:     i.Config.authority(),
		TenantID:      i.Config.TenantID,
		HTTPClient:    i.HTTPClient,
		MetricAdapter: "agentIdentity",
	}
	return client.RequestToken(ctx, step, form)
}

type Source struct {
	Config Config
	Entra  *EntraIssuer
}

func (s *Source) AgentIdentityToken(ctx context.Context, req outboundaccess.AgentIdentityTokenRequest) (string, error) {
	if s == nil {
		return "", errors.New("no agent identity issuer is configured")
	}
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
		return s.Entra.VerifyIdentity(ctx, id, entraVerificationResource)
	default:
		return fmt.Errorf("identity issuer %q is not supported", issuer)
	}
}
