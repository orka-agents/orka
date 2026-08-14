/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	defaultTimeout                = 15 * time.Second
	conformanceMessageLimit       = 1024
	minimumRawCredentialLeakBytes = 8
	redactedValue                 = "[REDACTED]"
	jsonMediaType                 = "application/json"
)

// Target describes one authenticated OMS endpoint and exact binding fixture.
type Target struct {
	BaseURL                string
	AuthorizationValue     string
	Binding                protocol.Binding
	StoreName              string
	RunID                  string
	HTTPClient             *http.Client
	Timeout                time.Duration
	DisableProxy           bool
	InsecureLoopbackOnly   bool
	ProviderCommitGapProof bool
}

// DefaultBinding is safe for an isolated conformance adapter. Production
// adapters should pass their configured binding explicitly.
func DefaultBinding() protocol.Binding {
	binding := protocol.Binding{
		ClusterID: "conformance-cluster", NamespaceUID: "conformance-namespace-uid",
		BackendUID: "conformance-backend-uid", AuthorityEpoch: 1, RoutingEpoch: 1,
		StoreUUID: "11111111-1111-4111-8111-111111111111",
	}
	binding.TenantID = protocol.DeriveTenantID(binding.ClusterID, binding.NamespaceUID)
	return binding
}

type contractClient struct {
	baseURL            string
	rawBearerToken     string
	authorizationValue string
	http               *http.Client
	limits             protocol.CapabilityLimits
	limitsConfigured   bool
}

func newContractClient(target Target) (*contractClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("adapter base URL is invalid")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("adapter base URL contains forbidden components")
	}
	insecureHTTP, err := validateTransportProfile(parsed, target.InsecureLoopbackOnly)
	if err != nil {
		return nil, err
	}
	rawBearerToken, authorizationValue, err := normalizeBearerAuthorization(target.AuthorizationValue)
	if err != nil {
		return nil, err
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := &http.Client{Timeout: timeout}
	if target.HTTPClient != nil {
		copy := *target.HTTPClient
		client = &copy
		if client.Timeout <= 0 {
			client.Timeout = timeout
		}
	}
	if target.DisableProxy || insecureHTTP {
		transport, err := noProxyTransport(client.Transport)
		if err != nil {
			return nil, err
		}
		client.Transport = transport
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &contractClient{
		baseURL:            strings.TrimRight(parsed.String(), "/"),
		rawBearerToken:     rawBearerToken,
		authorizationValue: authorizationValue,
		http:               client,
	}, nil
}

func validateTransportProfile(parsed *url.URL, insecureLoopbackOnly bool) (bool, error) {
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	switch scheme {
	case "https":
		parsed.Scheme = scheme
		return false, nil
	case "http":
		if !insecureLoopbackOnly {
			return false, fmt.Errorf("adapter base URL must use HTTPS unless insecure loopback HTTP is explicitly enabled")
		}
		address, err := netip.ParseAddr(parsed.Hostname())
		if err != nil || !address.IsLoopback() {
			return false, fmt.Errorf("insecure adapter HTTP is restricted to a literal loopback address")
		}
		parsed.Scheme = scheme
		return true, nil
	default:
		return false, fmt.Errorf("adapter base URL must use HTTPS")
	}
}

func noProxyTransport(roundTripper http.RoundTripper) (*http.Transport, error) {
	if roundTripper == nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("default HTTP transport is not configurable")
		}
		transport := base.Clone()
		transport.Proxy = nil
		return transport, nil
	}
	base, ok := roundTripper.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("custom HTTP transport cannot disable proxying")
	}
	transport := base.Clone()
	transport.Proxy = nil
	return transport, nil
}

type contractResponse struct {
	Body       []byte
	StatusCode int
	Header     http.Header
}

func (c *contractClient) do(ctx context.Context, method, path, token string, body []byte) ([]byte, int, error) {
	response, err := c.doResponse(ctx, method, path, token, body, nil)
	return response.Body, response.StatusCode, err
}

func (c *contractClient) doResponse(
	ctx context.Context,
	method, path, token string,
	body []byte,
	extraHeaders http.Header,
) (contractResponse, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return contractResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", jsonMediaType)
	}
	if token != "" {
		request.Header.Set("Authorization", completeBearerAuthorizationValue(token))
	}
	for key, values := range extraHeaders {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.http.Do(request)
	if err != nil {
		return contractResponse{}, err
	}
	defer response.Body.Close() //nolint:errcheck
	result := contractResponse{StatusCode: response.StatusCode, Header: response.Header.Clone()}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, protocol.MaxAdapterResponseBytes+1))
	if err != nil {
		return result, err
	}
	result.Body = responseBody
	if err := c.validateResponseBytes(path, responseBody); err != nil {
		return result, err
	}
	if err := validateResponseHeaders(result.Header); err != nil {
		return result, err
	}
	return result, nil
}

func validateResponseHeaders(header http.Header) error {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != jsonMediaType {
		return errors.New("adapter response Content-Type must be application/json")
	}
	for _, value := range header.Values("Cache-Control") {
		for directive := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				return nil
			}
		}
	}
	return errors.New("adapter response Cache-Control must include no-store")
}

func (c *contractClient) postJSON(ctx context.Context, path string, value any) ([]byte, int, error) {
	body, err := c.marshalJSONRequest(path, value)
	if err != nil {
		return nil, 0, err
	}
	return c.do(ctx, http.MethodPost, path, c.authorizationValue, body)
}

func (c *contractClient) configureLimits(limits protocol.CapabilityLimits) {
	c.limits = limits
	c.limitsConfigured = true
}

func (c *contractClient) validateResponseBytes(path string, body []byte) error {
	if len(body) > protocol.MaxAdapterResponseBytes {
		return fmt.Errorf(
			"adapter response from %s exceeded hard maxResponseBytes=%d bytes",
			strings.TrimSpace(path),
			protocol.MaxAdapterResponseBytes,
		)
	}
	if c.limitsConfigured && len(body) > c.limits.MaxResponseBytes {
		return fmt.Errorf(
			"adapter response from %s exceeded advertised maxResponseBytes=%d bytes",
			strings.TrimSpace(path),
			c.limits.MaxResponseBytes,
		)
	}
	return nil
}

func (c *contractClient) marshalJSONRequest(name string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := c.validateRequestBytes(name, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *contractClient) validateRequestBytes(name string, body []byte) error {
	if !c.limitsConfigured || len(body) <= c.limits.MaxRequestBytes {
		return nil
	}
	return fmt.Errorf(
		"adapter advertises maxRequestBytes=%d, but the %s conformance request requires %d bytes; "+
			"advertised limits are incompatible with this required proof",
		c.limits.MaxRequestBytes,
		strings.TrimSpace(name),
		len(body),
	)
}

func completeBearerAuthorizationValue(value string) string {
	_, authorizationValue, err := normalizeBearerAuthorization(value)
	if err != nil {
		return ""
	}
	return authorizationValue
}

func normalizeBearerAuthorization(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", fmt.Errorf("adapter bearer token is required")
	}
	raw := value
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		raw = strings.TrimSpace(value[len("Bearer "):])
	}
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") || strings.ContainsAny(raw, "\x00") {
		return "", "", fmt.Errorf("adapter bearer token is invalid")
	}
	return raw, "Bearer " + raw, nil
}

func credentialVariants(value string) []string {
	raw, authorizationValue, err := normalizeBearerAuthorization(value)
	if err != nil {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil
		}
		return []string{trimmed}
	}
	return []string{authorizationValue, raw}
}

func containsCredential(body []byte, authorizationValue string) bool {
	raw, complete, err := normalizeBearerAuthorization(authorizationValue)
	if err != nil {
		for _, credential := range credentialVariants(authorizationValue) {
			if bytes.Contains(body, []byte(credential)) {
				return true
			}
		}
		return false
	}
	if bytes.Contains(body, []byte(complete)) {
		return true
	}
	// Short raw tokens are indistinguishable from ordinary JSON text. Keep them
	// valid for adapters that accept them, but only treat the unambiguous full
	// Authorization value as a leak.
	return len(raw) >= minimumRawCredentialLeakBytes && bytes.Contains(body, []byte(raw))
}

func sanitizeOutputText(value, authorizationValue string, limit int) string {
	for _, credential := range credentialVariants(authorizationValue) {
		value = strings.ReplaceAll(value, credential, redactedValue)
	}
	return protocol.SanitizeMessage(value, limit)
}

func safeError(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	return protocol.SanitizeMessage(prefix+": "+err.Error(), conformanceMessageLimit)
}
