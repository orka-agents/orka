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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestSetupStaticFiles_IndexHTML(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %s, want text/html", ct)
	}
}

func TestSetupStaticFiles_SPARoutes(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080})

	spaRoutes := []string{
		"/login",
		"/tasks",
		"/tasks/some-task-id",
		"/sessions",
		"/sessions/some-session-id",
		"/agents",
		"/agents/my-agent",
		"/tools",
		"/tools/my-tool",
	}

	for _, route := range spaRoutes {
		t.Run(route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, route, nil)
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, "text/html") {
				t.Errorf("Content-Type = %s, want text/html", ct)
			}
		})
	}
}

func TestSetupStaticFiles_Favicon(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080})

	req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	// Favicon may or may not exist in stub embed; check it doesn't panic
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 200 or 404", resp.StatusCode)
	}
}

func TestSetupStaticFiles_Assets(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080})

	tests := []struct {
		name        string
		path        string
		wantCT      string
		expectExist bool
	}{
		{"JS asset", "/assets/main.js", "application/javascript", false},
		{"CSS asset", "/assets/style.css", "text/css", false},
		{"other asset", "/assets/image.png", "application/octet-stream", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}

			// Assets may not exist in test embed, but the route handler should respond
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
				t.Errorf("StatusCode = %d, want 200 or 404", resp.StatusCode)
			}
		})
	}
}

func TestSetupStaticFiles_DoesNotInterfereWithAPI(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080})

	// Health endpoints should still work
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// API endpoints should still require auth
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	resp, err = server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("API StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSetupStaticFiles_IndexContent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := server.app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}

	// The embedded stub has content (at least "x\n")
	if len(body) == 0 {
		t.Error("expected non-empty body for index.html")
	}
}
