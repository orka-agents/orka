/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompatRouterCORSPreflight(t *testing.T) {
	t.Setenv("ORKA_CORS_ALLOWED_ORIGINS", " https://chat.example.test, https://other.example.test ")
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	router, err := NewCompatRouter(compatRouterTokenClient(t, nil), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, tc := range []struct {
		path, method string
	}{
		{"/openai/v1/models", http.MethodGet},
		{"/anthropic/v1/models", http.MethodGet},
		{"/openai/v1/chat/completions", http.MethodPost},
		{"/anthropic/v1/messages", http.MethodPost},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			req.Header.Set("Origin", "https://chat.example.test")
			req.Header.Set("Access-Control-Request-Method", tc.method)
			req.Header.Set("Access-Control-Request-Headers", "authorization, x-api-key, content-type, x-orka-tools, anthropic-version")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusNoContent, response.Code)
			require.Equal(t, "https://chat.example.test", response.Header().Get("Access-Control-Allow-Origin"))
			require.Equal(t, tc.method, response.Header().Get("Access-Control-Allow-Methods"))
			require.Contains(t, strings.ToLower(response.Header().Get("Access-Control-Allow-Headers")), "authorization")
			require.Empty(t, response.Header().Get("Access-Control-Allow-Credentials"))
			require.Contains(t, strings.Join(response.Header().Values("Vary"), ","), "Origin")
			require.Empty(t, response.Body.String(), "preflight must not reveal tenant data")
		})
	}
	require.Zero(t, forwarded.Load(), "preflight must not select or contact an installation")
}

func TestCompatRouterCORSRejectsUnsupportedPreflight(t *testing.T) {
	t.Setenv("ORKA_CORS_ALLOWED_ORIGINS", "https://chat.example.test")
	router, err := NewCompatRouter(compatRouterTokenClient(t, nil), map[string]string{"team-a": "http://127.0.0.1:1"})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, tc := range []struct {
		name, origin, method, path, headers string
		status                              int
	}{
		{"origin", "https://outside.example.test", http.MethodPost, "/anthropic/v1/messages", "content-type", http.StatusForbidden},
		{"missing origin", "", http.MethodPost, "/anthropic/v1/messages", "content-type", http.StatusForbidden},
		{"route", "https://chat.example.test", http.MethodGet, "/api/v1/tasks", "authorization", http.StatusNotFound},
		{"method", "https://chat.example.test", http.MethodDelete, "/anthropic/v1/models", "authorization", http.StatusNotFound},
		{"missing method", "https://chat.example.test", "", "/anthropic/v1/models", "authorization", http.StatusNotFound},
		{"impersonation", "https://chat.example.test", http.MethodGet, "/anthropic/v1/models", "Impersonate-User", http.StatusForbidden},
		{"transaction token", "https://chat.example.test", http.MethodGet, "/anthropic/v1/models", TransactionTokenHeaderName, http.StatusForbidden},
		{"unknown SDK header", "https://chat.example.test", http.MethodGet, "/anthropic/v1/models", "X-Stainless-Unknown", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", tc.method)
			req.Header.Set("Access-Control-Request-Headers", tc.headers)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code)
			require.Empty(t, response.Header().Get("Access-Control-Allow-Methods"))
			if tc.origin != "https://chat.example.test" {
				require.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))
			}
		})
	}
}

func TestCompatRouterCORSActualRequestsStillAuthenticate(t *testing.T) {
	t.Setenv("ORKA_CORS_ALLOWED_ORIGINS", "https://chat.example.test")
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		if r.Header.Get("Origin") != "" {
			t.Error("router forwarded Origin instead of enforcing its own policy")
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "backend-only")
		_, _ = io.WriteString(w, "private models")
	}))
	t.Cleanup(upstream.Close)
	token := t.Name()
	router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, tc := range []struct {
		name, origin, credential, allowed string
		status                            int
	}{
		{"allowed", "https://chat.example.test", token, "https://chat.example.test", http.StatusOK},
		{"unlisted origin", "https://outside.example.test", token, "", http.StatusOK},
		{"no origin", "", token, "", http.StatusOK},
		{"missing credential", "https://chat.example.test", "", "https://chat.example.test", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set(XAPIKeyHeader, tc.credential)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code)
			require.Equal(t, tc.allowed, response.Header().Get("Access-Control-Allow-Origin"))
			require.Empty(t, response.Header().Get("Access-Control-Allow-Credentials"))
			require.Empty(t, response.Header().Get("Access-Control-Expose-Headers"))
			if tc.status == http.StatusUnauthorized {
				require.NotContains(t, response.Body.String(), "private models")
			}
		})
	}
	require.Equal(t, int32(3), forwarded.Load())
}

func TestCompatRouterCORSDefaultOrigin(t *testing.T) {
	t.Setenv("ORKA_CORS_ALLOWED_ORIGINS", "")
	router, err := NewCompatRouter(compatRouterTokenClient(t, nil), map[string]string{"team-a": "http://127.0.0.1:1"})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	req := httptest.NewRequest(http.MethodOptions, "/openai/v1/models", nil)
	req.Header.Set("Origin", "https://chat.example.test")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "*", response.Header().Get("Access-Control-Allow-Origin"))
	require.Empty(t, response.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCompatRouterModelResponsesNeverEnterSharedCache(t *testing.T) {
	identities, routes := make(map[string]string), make(map[string]string)
	for _, namespace := range []string{"team-a", "team-b"} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Expires", time.Now().Add(time.Hour).Format(http.TimeFormat))
			_, _ = io.WriteString(w, namespace+"-private-model")
		}))
		t.Cleanup(upstream.Close)
		routes[namespace] = upstream.URL
		identities[t.Name()+namespace] = "system:serviceaccount:" + namespace + ":client"
	}
	router, err := NewCompatRouter(compatRouterTokenClient(t, identities), routes)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, path := range []string{"/openai/v1/models", "/anthropic/v1/models"} {
		for _, header := range []string{AuthHeader, XAPIKeyHeader} {
			for _, namespace := range []string{"team-a", "team-b"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				credential := t.Name() + namespace
				if header == AuthHeader {
					credential = BearerPrefix + credential
				}
				req.Header.Set(header, credential)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				require.Equal(t, http.StatusOK, response.Code)
				require.Equal(t, namespace+"-private-model", response.Body.String())
				require.NotEmpty(t, response.Header().Values("Cache-Control"))
				for _, value := range response.Header().Values("Cache-Control") {
					require.Equal(t, "private, no-store", value, "installation headers must not permit shared caching")
				}
				require.Empty(t, response.Header().Get("Expires"))
			}
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
}

func TestCompatRouterBrowserSDKHeaders(t *testing.T) {
	t.Setenv("ORKA_CORS_ALLOWED_ORIGINS", "https://chat.example.test")
	// These are the TypeScript SDK's browser/platform/retry headers, including
	// the streaming helper marker, not a copy of the production allowlist.
	sdkHeaders := []string{
		"anthropic-dangerous-direct-browser-access",
		"x-stainless-lang", "x-stainless-package-version", "x-stainless-os",
		"x-stainless-arch", "x-stainless-runtime", "x-stainless-runtime-version",
		"x-stainless-retry-count", "x-stainless-timeout", "x-stainless-helper-method",
	}
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		for _, header := range sdkHeaders {
			if r.Header.Get(header) != "" {
				t.Errorf("SDK-only header %s crossed the forwarding boundary", header)
			}
		}
		_, _ = io.WriteString(w, "complete")
	}))
	t.Cleanup(upstream.Close)
	token := t.Name()
	router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, path := range []string{"/anthropic/v1/messages", "/openai/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			preflight := httptest.NewRequest(http.MethodOptions, path, nil)
			preflight.Header.Set("Origin", "https://chat.example.test")
			preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
			requested := append([]string{"content-type", "authorization", "x-api-key", "anthropic-version"}, sdkHeaders...)
			preflight.Header.Set("Access-Control-Request-Headers", strings.Join(requested, ", "))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, preflight)
			require.Equal(t, http.StatusNoContent, response.Code)
			allowed := strings.ToLower(response.Header().Get("Access-Control-Allow-Headers"))
			for _, header := range requested {
				require.Contains(t, allowed, header)
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Origin", "https://chat.example.test")
			req.Header.Set(XAPIKeyHeader, token)
			for _, header := range sdkHeaders {
				req.Header.Set(header, "fixture")
			}
			response = httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, "complete", response.Body.String())
			require.Equal(t, "https://chat.example.test", response.Header().Get("Access-Control-Allow-Origin"))
		})
	}
	require.Equal(t, int32(2), forwarded.Load(), "only authenticated messages, not preflights, may reach an installation")
}
