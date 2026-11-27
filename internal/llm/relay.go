/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package llm

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type ControllerRelay struct {
	URL   string
	Token BearerTokenSource
}

func NewControllerRelayHTTPClient(baseURL string, relay *ControllerRelay) (*http.Client, error) {
	if relay == nil || relay.Token == nil {
		return nil, errors.New("a controller relay needs a URL and a token source")
	}
	origin, err := httpsOrigin(baseURL)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || strings.Trim(base.EscapedPath(), "/") != "" || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("a relayed Provider's baseURL must be the resource's origin, without a path or query")
	}
	target, err := url.Parse(strings.TrimSpace(relay.URL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("the controller relay URL must be an http or https URL without credentials, query or fragment")
	}
	return &http.Client{
		Transport: &relayTransport{
			base:       SharedHTTPClient().Transport,
			origin:     origin,
			relay:      target,
			relayToken: relay.Token,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type relayTransport struct {
	base       http.RoundTripper
	origin     string
	relay      *url.URL
	relayToken BearerTokenSource
}

func (t *relayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	origin, err := httpsOrigin(request.URL.String())
	if err != nil || origin != t.origin {
		closeRequestBody(request)
		return nil, errors.New("refusing to relay a request outside the Provider's endpoint")
	}
	suffix := request.URL.EscapedPath()
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		closeRequestBody(request)
		return nil, errors.New("refusing to relay a request outside the Provider's endpoint")
	}
	token, err := t.relayToken(request.Context())
	if err != nil || token == "" {
		closeRequestBody(request)
		return nil, &ProviderError{Message: "could not read the worker's token for the controller relay", StatusCode: http.StatusServiceUnavailable}
	}
	relayed := request.Clone(request.Context())
	target := *t.relay
	target.RawPath = strings.TrimSuffix(t.relay.EscapedPath(), "/") + suffix
	decoded, err := url.PathUnescape(target.RawPath)
	if err != nil {
		closeRequestBody(request)
		return nil, errors.New("refusing to relay a request with a malformed path")
	}
	target.Path = decoded
	target.RawQuery = request.URL.RawQuery
	relayed.URL = &target
	relayed.Host = ""
	relayed.Header.Del("Api-Key")
	relayed.Header.Del("X-Api-Key")
	relayed.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(relayed)
}
