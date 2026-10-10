// Copyright (c) 2026. MIT License - see LICENSE file for details.
package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"k8s.io/apimachinery/pkg/util/validation"
)

type substrateRouteRoundTripper struct {
	scheme    string
	basePath  string
	transport *http.Transport
}

func (t *substrateRouteRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("substrate route request URL is required")
	}
	clone := request.Clone(request.Context())
	urlCopy := *request.URL
	urlCopy.Scheme = t.scheme
	if t.basePath != "" {
		urlCopy.Path = strings.TrimRight(t.basePath, "/") + "/" + strings.TrimLeft(urlCopy.Path, "/")
		// Path now contains the decoded combination. Clear RawPath so net/http
		// derives a matching escaped form instead of reusing the actor-relative
		// path from the original request.
		urlCopy.RawPath = ""
	}
	clone.URL = &urlCopy
	return t.transport.RoundTrip(clone)
}

func substrateRouteHTTPTransport(routerURL, actorDNSSuffix string) (http.RoundTripper, error) {
	trimmed := strings.TrimSpace(routerURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != urlSchemeHTTP && parsed.Scheme != urlSchemeHTTPS) {
		return nil, fmt.Errorf("substrate router URL is invalid")
	}
	// Tool.status.endpoint republishes the router URL to Tool readers, and the
	// MCP path is appended to it as text.
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.Contains(trimmed, "#") {
		return nil, fmt.Errorf("substrate router URL must not contain credentials, a query, or a fragment")
	}
	routerAddress := parsed.Host
	if parsed.Port() == "" {
		port := "80"
		if parsed.Scheme == urlSchemeHTTPS {
			port = "443"
		}
		routerAddress = net.JoinHostPort(parsed.Hostname(), port)
	}
	normalizedSuffix := strings.ToLower(strings.Trim(strings.TrimSpace(actorDNSSuffix), "."))
	if normalizedSuffix == "" {
		return nil, fmt.Errorf("substrate actor DNS suffix is required")
	}
	if problems := validation.IsDNS1123Subdomain(normalizedSuffix); len(problems) > 0 {
		return nil, fmt.Errorf("substrate actor DNS suffix is invalid: %s", strings.Join(problems, "; "))
	}
	suffix := "." + normalizedSuffix
	transport := harnessv2.NewProxylessTransport()
	if parsed.Scheme == urlSchemeHTTPS {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: parsed.Hostname(),
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			host = address
		}
		if !strings.HasSuffix(strings.ToLower(host), suffix) {
			return nil, fmt.Errorf("substrate route transport refuses non-actor host")
		}
		return dialer.DialContext(ctx, network, routerAddress)
	}
	return &substrateRouteRoundTripper{
		scheme: parsed.Scheme, basePath: strings.TrimRight(parsed.Path, "/"), transport: transport,
	}, nil
}
