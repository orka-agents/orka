// Copyright (c) 2026. MIT License - see LICENSE file for details.

package provideridentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/entraclient"
)

const (
	CloudAzurePublic       = "AzurePublic"
	CloudAzureUSGovernment = "AzureUSGovernment"
	CloudAzureChina        = "AzureChina"

	DefaultTokenFile     = "/var/run/secrets/orka.ai/provider-identity/token"
	DefaultTokenAudience = "api://AzureADTokenExchange"

	publicDataPlaneScope = "https://ai.azure.com/.default"
	metricAdapter        = "providerIdentity"
	schemeHTTPS          = "https"
)

var ErrNotConfigured = errors.New("the Provider identity is not configured on this controller")

var ErrEndpointNotAllowed = errors.New("the Provider endpoint is not in the identity's allowedEndpoints")

var ErrIssuerUnavailable = entraclient.ErrIssuerUnavailable

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type cloudDefaults struct {
	authority string
	audience  string
	scope     string
}

var clouds = map[string]cloudDefaults{
	CloudAzurePublic:       {authority: entraclient.DefaultAuthority, audience: "api://AzureADTokenExchange", scope: publicDataPlaneScope},
	CloudAzureUSGovernment: {authority: "https://login.microsoftonline.us", audience: "api://AzureADTokenExchangeUSGov"},
	CloudAzureChina:        {authority: "https://login.chinacloudapi.cn"},
}

type Config struct {
	TokenAudience string     `json:"tokenAudience,omitempty"`
	Identities    []Identity `json:"identities"`
}

type Identity struct {
	Name             string   `json:"name"`
	TenantID         string   `json:"tenantID"`
	ClientID         string   `json:"clientID"`
	Cloud            string   `json:"cloud,omitempty"`
	Authority        string   `json:"authority,omitempty"`
	ExchangeAudience string   `json:"exchangeAudience,omitempty"`
	Scope            string   `json:"scope,omitempty"`
	AllowedEndpoints []string `json:"allowedEndpoints"`
}

func Parse(raw string) (Config, error) {
	var config Config
	if strings.TrimSpace(raw) == "" {
		return config, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse the Provider identity configuration: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("parse the Provider identity configuration: trailing data after the JSON object")
	}
	return config, config.Validate()
}

type resolved struct {
	name      string
	tenantID  string
	clientID  string
	authority string
	scope     string
	origins   map[string]struct{}
}

func (c Config) tokenAudience() string {
	if audience := strings.TrimSpace(c.TokenAudience); audience != "" {
		return audience
	}
	return DefaultTokenAudience
}

func (c Config) Validate() error {
	_, err := c.resolve()
	return err
}

func (c Config) resolve() (map[string]resolved, error) {
	identities := make(map[string]resolved, len(c.Identities))
	for _, identity := range c.Identities {
		entry, err := identity.resolve(c.tokenAudience())
		if err != nil {
			return nil, fmt.Errorf("Provider identity %q: %w", identity.Name, err) //nolint:staticcheck // Resource kind begins the message.
		}
		if _, duplicate := identities[entry.name]; duplicate {
			return nil, fmt.Errorf("Provider identity %q is configured more than once", entry.name) //nolint:staticcheck // Resource kind begins the message.
		}
		identities[entry.name] = entry
	}
	return identities, nil
}

func (i Identity) resolve(tokenAudience string) (resolved, error) {
	if errs := k8svalidation.IsDNS1123Label(i.Name); len(errs) > 0 {
		return resolved{}, errors.New("the name must be a DNS label")
	}
	if !guidPattern.MatchString(i.TenantID) || !guidPattern.MatchString(i.ClientID) {
		return resolved{}, errors.New("tenantID and clientID must be GUIDs without surrounding space")
	}
	cloud := i.Cloud
	if cloud == "" {
		cloud = CloudAzurePublic
	}
	defaults, ok := clouds[cloud]
	if !ok {
		return resolved{}, fmt.Errorf("cloud must be %s, %s, or %s", CloudAzurePublic, CloudAzureUSGovernment, CloudAzureChina)
	}
	authority := defaults.authority
	if i.Authority != "" {
		authority = i.Authority
	}
	if err := entraclient.ValidateAuthority(authority); err != nil {
		return resolved{}, err
	}
	audience := defaults.audience
	if i.ExchangeAudience != "" {
		audience = i.ExchangeAudience
	}
	if audience == "" {
		return resolved{}, fmt.Errorf("set exchangeAudience: no token-exchange audience is documented for %s", cloud)
	}
	if audience != tokenAudience {
		return resolved{}, fmt.Errorf("exchange audience %q does not match the controller's projected token audience %q", audience, tokenAudience)
	}
	scope := defaults.scope
	if i.Scope != "" {
		scope = i.Scope
	}
	if scope == "" {
		return resolved{}, fmt.Errorf("set scope: no data-plane scope is documented for %s", cloud)
	}
	if err := validateScope(scope); err != nil {
		return resolved{}, err
	}
	if len(i.AllowedEndpoints) == 0 {
		return resolved{}, errors.New("allowedEndpoints must list at least one https origin")
	}
	origins := make(map[string]struct{}, len(i.AllowedEndpoints))
	for _, endpoint := range i.AllowedEndpoints {
		origin, err := exactOrigin(endpoint)
		if err != nil {
			return resolved{}, fmt.Errorf("allowedEndpoints: %w", err)
		}
		origins[origin] = struct{}{}
	}
	return resolved{
		name:      i.Name,
		tenantID:  strings.ToLower(i.TenantID),
		clientID:  strings.ToLower(i.ClientID),
		authority: entraclient.CanonicalAuthority(authority),
		scope:     scope,
		origins:   origins,
	}, nil
}

func validateScope(scope string) error {
	parsed, err := url.Parse(scope)
	if err != nil || strings.TrimSpace(scope) != scope || !strings.HasSuffix(scope, "/.default") ||
		(parsed.Scheme != schemeHTTPS && parsed.Scheme != "api") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("scope must be an https:// or api:// resource ending in /.default")
	}
	return nil
}

func isASCIIHost(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return false
		}
	}
	return true
}

func exactOrigin(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || strings.TrimSpace(endpoint) != endpoint || parsed.Scheme != schemeHTTPS || parsed.Hostname() == "" ||
		!isASCIIHost(parsed.Hostname()) || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || strings.Contains(parsed.Hostname(), "*") {
		return "", fmt.Errorf("%q must be an exact https origin such as https://example.openai.azure.com", endpoint)
	}
	return canonicalOrigin(parsed), nil
}

func canonicalOrigin(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host
}

func Origin(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Hostname() == "" || !isASCIIHost(parsed.Hostname()) || parsed.User != nil {
		return "", errors.New("the Provider baseURL must be an https URL without credentials")
	}
	return canonicalOrigin(parsed), nil
}

type Source struct {
	Credential func() (string, error)
	HTTPClient *http.Client
	Now        func() time.Time

	identities map[string]resolved
	cache      *entraclient.Cache
}

func NewSource(config Config, credential func() (string, error)) (*Source, error) {
	identities, err := config.resolve()
	if err != nil {
		return nil, err
	}
	if len(identities) == 0 {
		return nil, nil
	}
	source := &Source{Credential: credential, identities: identities}
	source.cache = &entraclient.Cache{Now: source.now}
	return source, nil
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) Check(identity, baseURL string) error {
	_, err := s.lookup(identity, baseURL)
	return err
}

func (s *Source) lookup(identity, baseURL string) (resolved, error) {
	if s == nil {
		return resolved{}, ErrNotConfigured
	}
	entry, ok := s.identities[identity]
	if !ok {
		return resolved{}, ErrNotConfigured
	}
	origin, err := Origin(baseURL)
	if err != nil {
		return resolved{}, err
	}
	if _, allowed := entry.origins[origin]; !allowed {
		return resolved{}, ErrEndpointNotAllowed
	}
	return entry, nil
}

func (s *Source) Token(ctx context.Context, identity, baseURL string) (string, error) {
	entry, err := s.lookup(identity, baseURL)
	if err != nil {
		return "", err
	}
	key := strings.Join([]string{"provider", entry.name, entry.tenantID, entry.clientID, entry.authority, entry.scope}, "\x00")
	return s.cache.Get(ctx, key, func(ctx context.Context) (string, time.Duration, error) {
		return s.request(ctx, entry)
	})
}

func (s *Source) Verify(ctx context.Context, identity, baseURL string) error {
	entry, err := s.lookup(identity, baseURL)
	if err != nil {
		return err
	}
	_, _, err = s.request(ctx, entry)
	return err
}

func (s *Source) request(ctx context.Context, entry resolved) (string, time.Duration, error) {
	if s.Credential == nil {
		return "", 0, fmt.Errorf("no federated credential is configured: %w", ErrIssuerUnavailable)
	}
	credential, err := s.Credential()
	if err != nil {
		return "", 0, err
	}
	client := entraclient.Client{
		Authority:     entry.authority,
		TenantID:      entry.tenantID,
		HTTPClient:    s.HTTPClient,
		MetricAdapter: metricAdapter,
	}
	return client.RequestToken(ctx, "entra_app", url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {entry.clientID},
		"scope":                 {entry.scope},
		"client_assertion_type": {entraclient.JWTBearerAssertionType},
		"client_assertion":      {credential},
	})
}
