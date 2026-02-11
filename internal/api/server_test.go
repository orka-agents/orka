/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

func TestNewServer(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port:           8080,
		MetricsPort:    9090,
		WatchNamespace: "default",
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
}

func TestServer_HealthEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := ServerConfig{
		Port: 8080,
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
		MetricsPort:    9090,
		WatchNamespace: "custom-ns",
	}

	if config.Port != 8080 {
		t.Errorf("Port = %d, want 8080", config.Port)
	}
	if config.MetricsPort != 9090 {
		t.Errorf("MetricsPort = %d, want 9090", config.MetricsPort)
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
