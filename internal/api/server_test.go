/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/apierror"
	"github.com/orka-agents/orka/internal/executionmode"
	memoryruntime "github.com/orka-agents/orka/internal/memory"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// mockHealthChecker implements store.HealthChecker for tests.
type mockHealthChecker struct{}

func (m *mockHealthChecker) HealthCheck(_ context.Context) error { return nil }

func TestNewServer(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port:           8080,
		WatchNamespace: "default",
		ExecutionMode:  executionmode.HarnessV2,
	}

	server := NewServer(fakeClient, nil, config)
	if server == nil {
		t.Fatal("NewServer returned nil")
	}

	if server.app == nil {
		t.Error("app is nil")
	}
	if server.handlers == nil {
		t.Error("handlers is nil")
	}
	if server.config.Port != 8080 {
		t.Errorf("Port = %d, want 8080", server.config.Port)
	}
	if server.config.Chat.ExecutionMode != executionmode.HarnessV2 ||
		server.handlers.executionMode != executionmode.HarnessV2 ||
		server.chatHandler.config.ExecutionMode != executionmode.HarnessV2 {
		t.Fatal("execution mode was not propagated to every Agent producer")
	}
}

func TestServer_HealthEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port:          8080,
		HealthChecker: &mockHealthChecker{},
	}

	server := NewServer(fakeClient, nil, config)

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{"healthz", "/healthz", http.StatusOK},
		{"readyz", "/readyz", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

func TestServer_APIRoutes_RequireAuth(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 8080,
	}

	server := NewServer(fakeClient, nil, config)

	// These endpoints should require authentication
	apiEndpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/tasks"},
		{http.MethodPost, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/tools"},
		{http.MethodGet, "/api/v1/agents"},
	}

	for _, ep := range apiEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			// No auth header
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			// Should return 401 Unauthorized
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
		})
	}
}

func TestCustomErrorHandler(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{
			name:     "fiber error",
			err:      fiber.NewError(fiber.StatusBadRequest, "bad request"),
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "fiber not found",
			err:      fiber.NewError(fiber.StatusNotFound, "not found"),
			wantCode: http.StatusNotFound,
		},
		{
			name:     "generic error",
			err:      fiber.NewError(fiber.StatusInternalServerError, "something went wrong"),
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New(fiber.Config{
				ErrorHandler: customErrorHandler,
			})
			app.Get("/api/test", func(c fiber.Ctx) error {
				return tt.err
			})

			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}

			if resp.StatusCode != tt.wantCode {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.wantCode)
			}
		})
	}
}

func TestServerConfig(t *testing.T) {
	config := ServerConfig{
		Port:           8080,
		WatchNamespace: "custom-ns",
	}

	if config.Port != 8080 {
		t.Errorf("Port = %d, want 8080", config.Port)
	}
	if config.WatchNamespace != "custom-ns" {
		t.Errorf("WatchNamespace = %s, want custom-ns", config.WatchNamespace)
	}
}

func TestServer_CORS(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 8080,
	}

	server := NewServer(fakeClient, nil, config)

	// Test OPTIONS preflight request
	req := httptest.NewRequest(http.MethodOptions, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")

	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	// Should allow CORS
	corsHeader := resp.Header.Get("Access-Control-Allow-Origin")
	if corsHeader == "" {
		t.Error("Missing Access-Control-Allow-Origin header")
	}
}

func TestServer_RequestID(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 8080,
	}

	server := NewServer(fakeClient, nil, config)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	// Should have request ID header
	requestID := resp.Header.Get("X-Request-Id")
	if requestID == "" {
		t.Error("Missing X-Request-Id header")
	}
}

func TestServer_Start_ContextCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 0, // Use port 0 to avoid conflicts
	}

	server := NewServer(fakeClient, nil, config)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start(ctx)
	}()

	// Cancel context to trigger shutdown
	cancel()

	err := <-errCh
	if err != nil {
		t.Errorf("Start() returned error on clean shutdown: %v", err)
	}
}

func TestCustomErrorHandler_NonFiberError(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler,
	})
	app.Get("/api/fail", func(c fiber.Ctx) error {
		return fmt.Errorf("plain go error")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/fail", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

func TestCustomErrorHandler_NonAPI404_ServesSPA(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler,
	})
	app.State().Set(dashboardStateKey, true)
	app.Get("/api/test", func(c fiber.Ctx) error {
		return c.SendString("api")
	})

	// Request to a non-API, non-health path that doesn't exist
	req := httptest.NewRequest(http.MethodGet, "/some-ui-route", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %s, want text/html", ct)
	}
}

func TestCustomErrorHandler_NonAPI404_WithoutDashboardReturnsJSON(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler,
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/some-ui-route", nil))
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %s, want application/json", ct)
	}
}

func TestCustomErrorHandler_API404_ReturnsJSON(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	// API 404s should return JSON error, not SPA fallback
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	bodyStr := string(body[:n])
	if len(bodyStr) == 0 {
		t.Error("expected non-empty body for API 404")
	}
}

func TestCustomErrorHandler_Health404_ReturnsJSON(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler,
	})

	// /healthz is excluded from SPA fallback
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestCustomErrorHandler_CompatAPI404_ReturnsProviderError(t *testing.T) {
	// An unrouted path under a compatibility API must not be answered with the
	// dashboard's index page: the caller is a provider SDK, and HTML parsed as
	// JSON surfaces several frames from the cause. It also has to carry that
	// provider's error envelope, which is the only shape its client reads.
	tests := []struct {
		name      string
		path      string
		assertErr func(t *testing.T, body []byte)
	}{
		{
			name: "openai",
			path: "/openai/v1/nonexistent",
			assertErr: func(t *testing.T, body []byte) {
				t.Helper()
				var payload OAIError
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("body is not an OpenAI error envelope: %v (%s)", err, body)
				}
				if payload.Error.Type != "invalid_request_error" {
					t.Errorf("error.type = %q, want invalid_request_error", payload.Error.Type)
				}
				if !strings.Contains(payload.Error.Message, "/openai/v1/nonexistent") {
					t.Errorf("error.message = %q, want it to name the path", payload.Error.Message)
				}
			},
		},
		{
			name: "openai upper case",
			path: "/OPENAI/v1/nonexistent",
			assertErr: func(t *testing.T, body []byte) {
				t.Helper()
				var payload OAIError
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("body is not an OpenAI error envelope: %v (%s)", err, body)
				}
				if payload.Error.Type != "invalid_request_error" {
					t.Errorf("error.type = %q, want invalid_request_error", payload.Error.Type)
				}
			},
		},
		{
			name: "anthropic",
			path: "/anthropic/v1/nonexistent",
			assertErr: func(t *testing.T, body []byte) {
				t.Helper()
				var payload AnthropicError
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("body is not an Anthropic error envelope: %v (%s)", err, body)
				}
				if payload.Type != "error" {
					t.Errorf("type = %q, want error", payload.Type)
				}
				if payload.Error.Type != "not_found_error" {
					t.Errorf("error.type = %q, want not_found_error", payload.Error.Type)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})

			resp, err := app.Test(httptest.NewRequest(http.MethodPost, tt.path, nil))
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("Failed to read body: %v", err)
			}
			tt.assertErr(t, body)
		})
	}
}

func TestCustomErrorHandler_OpenAIConversations_ReturnsNotImplemented(t *testing.T) {
	// Saved conversations remain unsupported; the error names the stateless route.
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/openai/v1/conversations", nil))
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotImplemented)
	}
	if got := resp.Header.Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry = %q, want false", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	var payload OAIError
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not an OpenAI error envelope: %v (%s)", err, body)
	}
	if !strings.Contains(payload.Error.Message, "/openai/v1/responses") {
		t.Errorf("error.message = %q, want it to name the supported route", payload.Error.Message)
	}
}

func TestServer_SetupRoutes_WithChat(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 8080,
		Chat: ChatConfig{Enabled: true},
	}

	server := NewServer(fakeClient, nil, config)
	if server.chatHandler == nil {
		t.Error("chatHandler should be non-nil when chat is enabled")
	}

	// Chat endpoint should require auth (return 401 without token)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", nil)
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestServer_SetupRoutes_OpenAI(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 8080,
	}

	server := NewServer(fakeClient, nil, config)

	// OpenAI-compatible endpoints should require auth
	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/openai/v1/chat/completions"},
		{http.MethodPost, "/openai/v1/responses"},
		{http.MethodGet, "/openai/v1/models"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
		})
	}
}

func TestServer_SetupRoutes_InternalAPI(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")

	config := ServerConfig{
		Port:         8080,
		ResultStore:  ss,
		SessionStore: ss,
		PlanStore:    ss,
		MessageStore: ss,
	}

	server := NewServer(fakeClient, nil, config)
	if server.internalHandlers == nil {
		t.Error("internalHandlers should be non-nil when stores are provided")
	}

	// Internal endpoints should require auth
	internalEndpoints := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/internal/v1/results/default/task1"},
		{http.MethodGet, "/internal/v1/sessions/default/session1/transcript"},
		{http.MethodPost, "/internal/v1/plans/default/task1"},
		{http.MethodGet, "/internal/v1/plans/default/task1"},
		{http.MethodPost, "/internal/v1/messages/default"},
		{http.MethodGet, "/internal/v1/messages/default/task1"},
	}

	for _, ep := range internalEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
		})
	}
}

func TestCustomErrorHandlerIncludesMemoryReasonAndRetryAfter(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Get("/api/v1/test", func(fiber.Ctx) error {
		return apierror.New(http.StatusServiceUnavailable, memoryruntime.ReasonBackendUnavailable, "memory backend unavailable").WithRetryAfter(3 * time.Second)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/test", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "3" {
		t.Fatalf("status=%d retry-after=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	var body map[string]map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"]["reason"] != memoryruntime.ReasonBackendUnavailable || body["error"]["code"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("body = %#v", body)
	}
}

func TestCustomErrorHandler_InternalAPI404DoesNotServeSPA(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/internal/v1/memories/default/missing", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if contentType := resp.Header.Get("Content-Type"); contentType == "text/html; charset=utf-8" {
		t.Fatalf("internal API 404 used SPA fallback: %q", contentType)
	}
}

func TestCustomErrorHandlerExposesOpaqueIncompleteSearchCursor(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Get("/api/v1/search", func(fiber.Ctx) error {
		return &memoryruntime.IncompleteSearchError{
			Cause:  apierror.New(http.StatusServiceUnavailable, memoryruntime.ReasonResultSetIncomplete, "incomplete"),
			Cursor: "msc-safe-cursor",
		}
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	var body struct {
		Error struct {
			Reason string `json:"reason"`
			Cursor string `json:"cursor"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Reason != memoryruntime.ReasonResultSetIncomplete || body.Error.Cursor != "msc-safe-cursor" {
		t.Fatalf("error body = %#v", body.Error)
	}
}

func TestServerPropagatesCustomContextTokenScopesToInternalMemoryHandlers(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ss := sqlite.NewStore(db, ":memory:")
	custom := ContextTokenAuthorizationConfig{
		Mode:                     ContextTokenAuthorizationModeEnforce,
		MemoryReadScopes:         []string{"custom:memory:read"},
		MemoryOperateScopes:      []string{"custom:memory:operate"},
		MemorySearchRemoteScopes: []string{"custom:memory:remote"},
	}
	tts := testDirectTaskIncomingTTSConfig()
	server := NewServer(fakeClient, nil, ServerConfig{
		Port: 8080, ResultStore: ss, SessionStore: ss, PlanStore: ss, MessageStore: ss,
		ContextTokenAuthorization: custom, ContextTokenTTS: tts,
	})
	if server.internalHandlers == nil {
		t.Fatal("internal handlers were not configured")
	}
	if server.handlers == nil || server.handlers.contextTokenTTS != tts {
		t.Fatalf("public handler TTS config = %#v, want %#v", server.handlers.contextTokenTTS, tts)
	}
	got := server.internalHandlers.contextTokenAuthorization
	if !slices.Equal(got.MemoryReadScopes, custom.MemoryReadScopes) ||
		!slices.Equal(got.MemoryOperateScopes, custom.MemoryOperateScopes) ||
		!slices.Equal(got.MemorySearchRemoteScopes, custom.MemorySearchRemoteScopes) {
		t.Fatalf("internal authorization = %#v, want custom scopes %#v", got, custom)
	}
}

func TestMemoryServiceErrorPreservesIncompleteSearchCursor(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Get("/api/v1/search", func(fiber.Ctx) error {
		return memoryServiceError(&memoryruntime.IncompleteSearchError{
			Cause:  apierror.New(http.StatusServiceUnavailable, memoryruntime.ReasonResultSetIncomplete, "incomplete"),
			Cursor: "msc-mapped-cursor",
		})
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct {
			Cursor string `json:"cursor"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Cursor != "msc-mapped-cursor" {
		t.Fatalf("mapped cursor = %q", body.Error.Cursor)
	}
}
