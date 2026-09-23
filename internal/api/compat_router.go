/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const anthropicErrorEnvelopeType = "error"

// CompatRouter forwards the four model compatibility routes to independent,
// namespace-scoped Orka installations. It authenticates ServiceAccount tokens;
// the receiving installation reauthenticates the same token and authorizes the
// route and each tool operation. It has no access to tenant resources or stores.
type CompatRouter struct {
	client      client.Client
	routes      map[string]*url.URL
	transport   *http.Transport
	corsOrigins []string
}

// NewCompatRouter freezes the operator's namespace-to-installation allowlist.
// Targets are API origins, without a path, credentials, query or fragment.
func NewCompatRouter(c client.Client, namespaces map[string]string) (*CompatRouter, error) {
	if c == nil || len(namespaces) == 0 {
		return nil, fmt.Errorf("TokenReview client and at least one namespace route are required")
	}
	routes := make(map[string]*url.URL, len(namespaces))
	for namespace, origin := range namespaces {
		if len(utilvalidation.IsDNS1123Label(namespace)) != 0 {
			return nil, fmt.Errorf("route namespace must be a Kubernetes namespace name")
		}
		target, err := url.Parse(origin)
		if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
			target.User != nil || (target.Path != "" && target.Path != "/") || target.RawPath != "" ||
			target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || target.Opaque != "" {
			// Do not echo URLs: a rejected configuration may contain credentials.
			return nil, fmt.Errorf("route for namespace %s must be an http(s) origin without credentials, path, query or fragment", namespace)
		}
		if port := target.Port(); port != "" {
			value, err := strconv.ParseUint(port, 10, 16)
			if err != nil || value == 0 {
				return nil, fmt.Errorf("route for namespace %s must use a port between 1 and 65535", namespace)
			}
		}
		routes[namespace] = target
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Destinations are explicitly configured services. Environment proxies must
	// not receive caller credentials or change where these requests go.
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.DisableCompression = true
	origins := os.Getenv("ORKA_CORS_ALLOWED_ORIGINS")
	if origins == "" {
		origins = "*"
	}
	return &CompatRouter{client: c, routes: routes, transport: transport, corsOrigins: strings.Split(origins, ",")}, nil
}

// Close releases idle connections when the router shuts down.
func (r *CompatRouter) Close() {
	r.transport.CloseIdleConnections()
}

func (r *CompatRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := routeLookupPath(req.URL.Path)
	// Credential-selected responses must never enter a shared cache, including
	// when clients use x-api-key rather than Authorization.
	w.Header().Set("Cache-Control", "private, no-store")
	if req.Method == http.MethodGet && (path == "/healthz" || path == "/readyz") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.handleCORS(w, req, path) {
		return
	}
	if !compatRouterRoute(req.Method, path) {
		writeCompatRouterError(w, path, http.StatusNotFound, "unknown compatibility route")
		return
	}
	// Other identity types retain their installation endpoint and namespace
	// contracts. Never route using one credential and authorize using another.
	if req.Header.Get(TransactionTokenHeaderName) != "" {
		writeCompatRouterError(w, path, http.StatusUnauthorized, "use the installation endpoint for transaction tokens")
		return
	}
	token, err := extractAuthTokenFromHeaders(req.Header.Get)
	if err != nil {
		writeCompatRouterError(w, path, http.StatusUnauthorized, "missing or invalid authentication header")
		return
	}
	authCtx, cancel := context.WithTimeout(req.Context(), authHTTPTimeout)
	identity, err := validateToken(authCtx, r.client, token)
	cancel()
	if err != nil {
		writeCompatRouterError(w, path, http.StatusUnauthorized, "invalid ServiceAccount token")
		return
	}
	if identity.Namespace == "" {
		writeCompatRouterError(w, path, http.StatusForbidden, "namespace-bound ServiceAccount identity required")
		return
	}
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		writeCompatRouterError(w, path, http.StatusBadRequest, "invalid query parameters")
		return
	}
	for _, namespace := range query["namespace"] {
		if namespace != "" && namespace != identity.Namespace {
			writeCompatRouterError(w, path, http.StatusForbidden, "namespace must match the authenticated ServiceAccount")
			return
		}
	}
	target, enabled := r.routes[identity.Namespace]
	if !enabled {
		writeCompatRouterError(w, path, http.StatusForbidden, "namespace is not enabled at this endpoint")
		return
	}
	// Also bind the receiving installation's namespace check. A misconfigured
	// route cannot silently run the work in a different watched namespace.
	query.Set("namespace", identity.Namespace)
	if req.ContentLength > defaultAPIRequestBodyLimit {
		writeCompatRouterError(w, path, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, defaultAPIRequestBodyLimit)
	proxy := &httputil.ReverseProxy{
		Transport:     r.transport,
		FlushInterval: -1, // Forward SSE immediately, including terminal events.
		ErrorLog:      stdlog.New(io.Discard, "", 0),
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.URL.Path = path
			p.Out.URL.RawPath = ""
			p.Out.URL.RawQuery = query.Encode()
			// Only protocol headers cross the router. In particular, custom
			// context-token headers, cookies and impersonation headers cannot
			// override the credential that selected this namespace.
			p.Out.Header = make(http.Header)
			for _, name := range compatRouterRequestHeaders {
				if values := p.In.Header.Values(name); len(values) != 0 {
					p.Out.Header[name] = append([]string(nil), values...)
				}
			}
			p.Out.Header.Set(AuthHeader, BearerPrefix+token)
			p.Out.Trailer = nil
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Set("Cache-Control", "private, no-store")
			response.Header.Del("Expires")
			// This listener owns its CORS policy; an installation must not widen
			// it through headers copied by the reverse proxy.
			for name := range response.Header {
				if strings.HasPrefix(strings.ToLower(name), "access-control-") {
					response.Header.Del(name)
				}
			}

			if response.StatusCode >= 300 && response.StatusCode < 400 {
				return fmt.Errorf("installation redirects are not supported")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				writeCompatRouterError(w, path, http.StatusRequestEntityTooLarge, "request body is too large")
				return
			}
			writeCompatRouterError(w, path, http.StatusServiceUnavailable, "namespace installation is unavailable")
		},
	}
	proxy.ServeHTTP(w, req)
}

var compatRouterRequestHeaders = []string{
	"Content-Type", "Accept", "Accept-Encoding", "User-Agent",
	"Anthropic-Version", "Anthropic-Beta", "X-Orka-Tools", "X-Request-Id",
	"Traceparent", "Tracestate",
}

func compatRouterRoute(method, path string) bool {
	switch method + " " + path {
	case "POST /openai/v1/chat/completions", "GET /openai/v1/models",
		"POST /anthropic/v1/messages", "GET /anthropic/v1/models":
		return true
	default:
		return false
	}
}

func writeCompatRouterError(w http.ResponseWriter, path string, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	errType := OAIErrorTypeInvalidRequest
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
	case http.StatusForbidden:
		errType = "permission_error"
	case http.StatusServiceUnavailable:
		errType = "api_error"
	case http.StatusNotFound:
		errType = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		errType = "request_too_large"
	}
	var response any = OAIError{Error: OAIErrorDetail{Message: message, Type: errType}}
	if strings.HasPrefix(path, "/anthropic") {
		response = AnthropicError{Type: anthropicErrorEnvelopeType, Error: AnthropicErrorDetail{Type: errType, Message: message}}
	}
	_ = json.NewEncoder(w).Encode(response)
}
