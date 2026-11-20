/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SharedHTTPClient reuses connections across request-scoped provider SDK clients.
// Credentials and middleware remain on each SDK client, not this HTTP client.
// Callers must not modify the returned client or its transport.
func SharedHTTPClient() *http.Client {
	return sharedHTTPClient()
}

var sharedHTTPClient = sync.OnceValue(func() *http.Client {
	transport := http.DefaultTransport
	if standard, ok := transport.(*http.Transport); ok {
		clone := standard.Clone()
		// Preserve the OpenAI and Anthropic SDKs' response-header deadline without
		// creating a fresh connection pool every time credentials are resolved.
		clone.ResponseHeaderTimeout = 10 * time.Minute
		transport = clone
	}
	return &http.Client{Transport: transport}
})

// BearerTokenSource returns a short-lived bearer token for one request.
type BearerTokenSource func(ctx context.Context) (string, error)

// ErrBearerTokenUnsupported is returned by providers that cannot
// authenticate with a BearerTokenSource.
var ErrBearerTokenUnsupported = &ProviderError{Message: "this provider type does not support token authentication"}

// NewBearerHTTPClient returns a client that authenticates every request with
// a token from source and sends it only to baseURL's https origin. It drops
// any API key header, and it never follows a redirect, so the token cannot be
// forwarded to another endpoint. Connections are shared with
// SharedHTTPClient.
func NewBearerHTTPClient(baseURL string, source BearerTokenSource) (*http.Client, error) {
	origin, err := httpsOrigin(baseURL)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("a bearer token source is required")
	}
	return &http.Client{
		Transport:     &bearerTransport{base: SharedHTTPClient().Transport, origin: origin, source: source},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type bearerTransport struct {
	base   http.RoundTripper
	origin string
	source BearerTokenSource
}

func (t *bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if origin, err := httpsOrigin(request.URL.String()); err != nil || origin != t.origin {
		closeRequestBody(request)
		return nil, errors.New("refusing to send a provider token to a request outside the Provider's endpoint")
	}
	token, err := t.source(request.Context())
	if err != nil {
		closeRequestBody(request)
		return nil, &ProviderError{Message: "could not obtain a token for the provider: " + err.Error(), StatusCode: http.StatusServiceUnavailable}
	}
	authenticated := request.Clone(request.Context())
	authenticated.Header.Del("Api-Key")
	authenticated.Header.Del("X-Api-Key")
	authenticated.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(authenticated)
}

// closeRequestBody honours the RoundTripper contract on early returns.
func closeRequestBody(request *http.Request) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
}

// httpsOrigin returns the lower-case https origin of rawURL, without the
// default port.
func httpsOrigin(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("token authentication requires an https endpoint without credentials")
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	return "https://" + host, nil
}
