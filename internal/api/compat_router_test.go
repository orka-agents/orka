/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func compatRouterTokenClient(t *testing.T, identities map[string]string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, authenticationv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
			review, ok := object.(*authenticationv1.TokenReview)
			if !ok {
				return fmt.Errorf("router may only create TokenReviews")
			}
			username, ok := identities[review.Spec.Token]
			review.Status = authenticationv1.TokenReviewStatus{
				Authenticated: ok,
				User: authenticationv1.UserInfo{
					Username: username, UID: "fixture-uid",
					Groups: []string{"system:authenticated", "system:serviceaccounts"},
				},
			}
			return nil
		},
	}).Build()
}

func TestCompatRouterConfiguration(t *testing.T) {
	kube := compatRouterTokenClient(t, nil)
	password := rand.Text()
	credentialedOrigin := &url.URL{Scheme: "http", Host: "orka-api", User: url.UserPassword("fixture-user", password)}
	for _, tc := range []struct {
		name   string
		routes map[string]string
	}{
		{"empty", nil},
		{"invalid namespace", map[string]string{"team/a": "http://orka-api"}},
		{"missing host", map[string]string{"team-a": "https://"}},
		{"non-http", map[string]string{"team-a": "file:///tmp/router"}},
		{"credentials", map[string]string{"team-a": credentialedOrigin.String()}},
		{"path", map[string]string{"team-a": "http://orka-api/api"}},
		{"query", map[string]string{"team-a": "http://orka-api?namespace=team-b"}},
		{"fragment", map[string]string{"team-a": "http://orka-api#fragment"}},
		{"invalid url", map[string]string{"team-a": "http://%"}},
		{"zero port", map[string]string{"team-a": "http://orka-api:0"}},
		{"out-of-range port", map[string]string{"team-a": "http://orka-api:65536"}},
		{"large port", map[string]string{"team-a": "http://orka-api:99999"}},
		{"overflowing port", map[string]string{"team-a": "http://orka-api:9999999999999999999999999999"}},
		{"IPv6 out-of-range port", map[string]string{"team-a": "http://[::1]:65536"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCompatRouter(kube, tc.routes)
			require.Error(t, err)
			require.False(t, strings.Contains(err.Error(), password), "configuration errors must not include URL credentials")
		})
	}
	for _, origin := range []string{"http://orka-api", "https://orka-api", "http://orka-api:1", "http://orka-api:65535", "http://[::1]", "http://[::1]:65535"} {
		router, err := NewCompatRouter(kube, map[string]string{"team-a": origin})
		require.NoError(t, err)
		router.Close()
	}
	_, err := NewCompatRouter(nil, map[string]string{"team-a": "http://orka-api"})
	require.Error(t, err)
}

func TestCompatRouterRoutesUsingAuthenticatedCredential(t *testing.T) {
	identities := map[string]string{}
	for _, ns := range []string{"team-a", "team-b"} {
		identities[t.Name()+ns] = "system:serviceaccount:" + ns + ":chat-client"
	}
	const body = `{"model":"same/model","user":"team-b","messages":[{"role":"user","content":"namespace=team-b"}],"max_tokens":64}`
	routes := map[string]string{}
	var calls atomic.Int32
	for _, namespace := range []string{"team-a", "team-b"} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			authorized := r.Header.Get(AuthHeader) == BearerPrefix+t.Name()+namespace
			if !authorized || r.URL.Query().Get("namespace") != namespace {
				http.Error(w, "credential or namespace changed", http.StatusForbidden)
				return
			}
			for _, header := range []string{XAPIKeyHeader, "Cookie", "Impersonate-User", "X-Custom-Context-Token", "Forwarded", "X-Forwarded-Host"} {
				if r.Header.Get(header) != "" {
					http.Error(w, "unexpected authentication or forwarding header", http.StatusBadRequest)
					return
				}
			}
			if r.Method == http.MethodPost {
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != body || r.Header.Get("X-Orka-Tools") != "disabled" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
					http.Error(w, "request body or protocol headers changed", http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Request-Id", "fixture-request")
			_, _ = fmt.Fprintf(w, `{"namespace":%q}`, namespace)
		}))
		t.Cleanup(upstream.Close)
		routes[namespace] = upstream.URL
	}
	router, err := NewCompatRouter(compatRouterTokenClient(t, identities), routes)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	// Configuration is frozen, not a caller-controlled mutable routing table.
	routes["team-a"] = "http://unused.invalid"
	entry := httptest.NewServer(router)
	t.Cleanup(entry.Close)
	for _, namespace := range []string{"team-a", "team-b"} {
		for _, tc := range []struct{ path, method, header string }{
			{"/openai/v1/chat/completions", http.MethodPost, AuthHeader},
			{"/openai/v1/models", http.MethodGet, AuthHeader},
			{"/anthropic/v1/messages", http.MethodPost, XAPIKeyHeader},
			{"/anthropic/v1/models", http.MethodGet, XAPIKeyHeader},
			{"/anthropic/v1/messages", http.MethodPost, AuthHeader},
			{"/anthropic/v1/models", http.MethodGet, AuthHeader},
		} {
			for _, query := range []string{"", "?namespace=" + namespace} {
				req, err := http.NewRequestWithContext(t.Context(), tc.method, entry.URL+tc.path+query, strings.NewReader(body))
				require.NoError(t, err)
				token := t.Name() + namespace
				if tc.header == AuthHeader {
					token = BearerPrefix + token
					other := "team-b"
					if namespace == "team-b" {
						other = "team-a"
					}
					req.Header.Set(XAPIKeyHeader, t.Name()+other)
				}
				req.Header.Set(tc.header, token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Orka-Tools", "disabled")
				req.Header.Set("Anthropic-Version", "2023-06-01")
				req.Header.Set("Cookie", "ignored=fixture")
				req.Header.Set("Impersonate-User", "admin")
				req.Header.Set("X-Custom-Context-Token", "ignored-fixture")
				req.Header.Set("Forwarded", "host=other.invalid")
				req.Header.Set("X-Forwarded-Host", "other.invalid")
				resp, err := entry.Client().Do(req)
				require.NoError(t, err)
				data, err := io.ReadAll(resp.Body)
				require.NoError(t, resp.Body.Close())
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode, "%s %s", tc.path, data)
				require.JSONEq(t, fmt.Sprintf(`{"namespace":%q}`, namespace), string(data))
				require.Equal(t, "fixture-request", resp.Header.Get("Request-Id"))
			}
		}
	}
	require.EqualValues(t, 24, calls.Load())
}

func TestCompatRouterDenialsNeverReachInstallation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	identities := map[string]string{
		t.Name() + "valid":    "system:serviceaccount:team-a:client",
		t.Name() + "disabled": "system:serviceaccount:team-c:client",
		t.Name() + "user":     "regular-user",
		t.Name() + "partial":  "system:serviceaccount:team-a:",
	}
	router, err := NewCompatRouter(compatRouterTokenClient(t, identities), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages", "/openai/v1/models", "/anthropic/v1/models"} {
		for _, tc := range []struct {
			name, token, query, transaction string
			status                          int
		}{
			{name: "missing", status: http.StatusUnauthorized},
			{name: "invalid", token: "invalid", status: http.StatusUnauthorized},
			{name: "namespace-less", token: "user", status: http.StatusForbidden},
			{name: "malformed identity", token: "partial", status: http.StatusForbidden},
			{name: "disabled", token: "disabled", status: http.StatusForbidden},
			{name: "mismatch", token: "valid", query: "?namespace=team-b", status: http.StatusForbidden},
			{name: "duplicate mismatch", token: "valid", query: "?namespace=team-a&namespace=team-b", status: http.StatusForbidden},
			{name: "invalid query", token: "valid", query: "?namespace=%zz", status: http.StatusBadRequest},
			{name: "transaction credential", token: "valid", transaction: "fixture-txn", status: http.StatusUnauthorized},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				method := http.MethodPost
				if strings.HasSuffix(path, "/models") {
					method = http.MethodGet
				}
				req := httptest.NewRequest(method, path+tc.query, strings.NewReader(`{}`))
				if tc.token != "" {
					// Use the parent test's credential names.
					parent, _, _ := strings.Cut(t.Name(), "/")
					req.Header.Set(AuthHeader, BearerPrefix+parent+tc.token)
				}
				req.Header.Set(TransactionTokenHeaderName, tc.transaction)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				require.Equal(t, tc.status, response.Code)
				var envelope map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
				require.IsType(t, map[string]any{}, envelope["error"])
				if strings.HasPrefix(path, "/anthropic/") {
					require.Equal(t, "error", envelope["type"])
				}
			})
		}
	}
	require.Zero(t, calls.Load())
}

func TestCompatRouterUnavailableAndRedirects(t *testing.T) {
	for _, path := range []string{"/openai/v1/models", "/anthropic/v1/models"} {
		for _, redirect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/redirect=%t", path, redirect), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "https://other.invalid", http.StatusTemporaryRedirect)
				}))
				if !redirect {
					upstream.Close()
				} else {
					t.Cleanup(upstream.Close)
				}
				token := t.Name()
				router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
				require.NoError(t, err)
				t.Cleanup(router.Close)
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set(AuthHeader, BearerPrefix+token)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
				require.Empty(t, response.Header().Get("Location"))
				require.Contains(t, response.Body.String(), "namespace installation is unavailable")
			})
		}
	}
}

func TestCompatRouterStreamingAndCancellation(t *testing.T) {
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Request-Id", "stream-request")
				_, _ = io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			t.Cleanup(upstream.Close)
			token := t.Name()
			router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
			require.NoError(t, err)
			t.Cleanup(router.Close)
			entry := httptest.NewServer(router)
			t.Cleanup(entry.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, entry.URL+path, strings.NewReader(`{"stream":true}`))
			require.NoError(t, err)
			req.Header.Set(AuthHeader, BearerPrefix+token)
			resp, err := entry.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
			require.Equal(t, "stream-request", resp.Header.Get("Request-Id"))
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "data: first\n", line, "first event must arrive before upstream closes")
			cancel()
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("client cancellation did not close the upstream request")
			}
		})
	}
}

func TestCompatRouterInvalidBearerDoesNotUseAPIKey(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	token := t.Name()
	router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, authorization := range []string{"Bearer invalid-fixture", "Basic invalid-fixture", "bearer " + token} {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		req.Header.Set(AuthHeader, authorization)
		req.Header.Set(XAPIKeyHeader, token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}
	require.Zero(t, calls.Load())
}

func TestCompatRouterPreservesBackendErrors(t *testing.T) {
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			const body = `{"error":{"type":"rate_limit_error","message":"installation limit"}}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(upstream.Close)
			token := t.Name()
			router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
			require.NoError(t, err)
			t.Cleanup(router.Close)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set(AuthHeader, BearerPrefix+token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusTooManyRequests, response.Code)
			require.Equal(t, "12", response.Header().Get("Retry-After"))
			require.Equal(t, body, response.Body.String())
		})
	}
}

func TestCompatRouterJSONRequestDeadline(t *testing.T) {
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			started, canceled := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-t.Context().Done():
				}
			}))
			t.Cleanup(upstream.Close)
			token := t.Name()
			router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
			require.NoError(t, err)
			t.Cleanup(router.Close)
			entry := httptest.NewServer(router)
			t.Cleanup(entry.Close)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, entry.URL+path, strings.NewReader(`{"stream":false}`))
			require.NoError(t, err)
			req.Header.Set(AuthHeader, BearerPrefix+token)
			response, err := entry.Client().Do(req)
			if response != nil {
				_ = response.Body.Close()
			}
			require.ErrorIs(t, err, context.DeadlineExceeded)
			select {
			case <-started:
			default:
				t.Fatal("request did not reach the installation before its deadline")
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("JSON request deadline did not cancel the installation request")
			}
		})
	}
}

func TestCompatRouterRequestBodyLimit(t *testing.T) {
	var completeBodies atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err == nil {
			completeBodies.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(upstream.Close)
	token := t.Name()
	router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	for _, lengthKnown := range []bool{true, false} {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(strings.Repeat("x", defaultAPIRequestBodyLimit+1)))
		if !lengthKnown {
			req.ContentLength = -1
		}
		req.Header.Set(AuthHeader, BearerPrefix+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		require.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
	}
	require.Zero(t, completeBodies.Load())
}
