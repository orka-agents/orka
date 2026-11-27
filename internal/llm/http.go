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

func SharedHTTPClient() *http.Client {
	return sharedHTTPClient()
}

var sharedHTTPClient = sync.OnceValue(func() *http.Client {
	transport := http.DefaultTransport
	if standard, ok := transport.(*http.Transport); ok {
		clone := standard.Clone()
		clone.ResponseHeaderTimeout = 10 * time.Minute
		transport = clone
	}
	return &http.Client{Transport: transport}
})

type BearerTokenSource func(ctx context.Context) (string, error)

var ErrBearerTokenUnsupported = &ProviderError{Message: "this provider type does not support token authentication"}

func NewBearerHTTPClient(baseURL string, source BearerTokenSource) (*http.Client, error) {
	return NewBearerHTTPClientWithTransport(baseURL, source, nil)
}

func NewBearerHTTPClientWithTransport(baseURL string, source BearerTokenSource, base http.RoundTripper) (*http.Client, error) {
	origin, err := httpsOrigin(baseURL)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("a bearer token source is required")
	}
	if base == nil {
		base = SharedHTTPClient().Transport
	}
	return &http.Client{
		Transport:     &bearerTransport{base: base, origin: origin, source: source},
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

func closeRequestBody(request *http.Request) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
}

func isASCIIHost(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return false
		}
	}
	return true
}

func httpsOrigin(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || !isASCIIHost(parsed.Hostname()) || parsed.User != nil {
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
