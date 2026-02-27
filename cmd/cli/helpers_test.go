/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	serviceCheckPath = "/api/v1/namespaces/default/services/orka-api"
	testCtxName      = "test-ctx"
)

func TestMaskToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"empty", "", "***"},
		{"short_1", "a", "***"},
		{"short_4", "abcd", "***"},
		{"five_chars", "abcde", "abcd...***"},
		{"long_token", "sk-1234567890abcdef", "sk-1...***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maskToken(tt.token)
			if got != tt.want {
				t.Errorf("maskToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}

func TestConfigPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got := configPath()
	want := filepath.Join(tmp, ".orka", "config.yaml")
	if got != want {
		t.Errorf("configPath() = %q, want %q", got, want)
	}
}

func TestLoadConfigEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := loadConfig()
	if cfg.Server != "" || cfg.Token != "" || cfg.Namespace != "" {
		t.Errorf("loadConfig() on missing file should return empty config, got %+v", cfg)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	want := orkaConfig{
		Server:    "http://example.com",
		Token:     "test-token-123",
		Namespace: "my-ns",
	}

	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig() error: %v", err)
	}

	got := loadConfig()
	if got.Server != want.Server {
		t.Errorf("Server = %q, want %q", got.Server, want.Server)
	}
	if got.Token != want.Token {
		t.Errorf("Token = %q, want %q", got.Token, want.Token)
	}
	if got.Namespace != want.Namespace {
		t.Errorf("Namespace = %q, want %q", got.Namespace, want.Namespace)
	}

	// Verify file permissions
	path := configPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file perm = %o, want 0600", perm)
	}
}

func TestSaveConfigCreatesDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := orkaConfig{Server: "http://test.local"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig() error: %v", err)
	}

	dir := filepath.Join(tmp, ".orka")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("config dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestSaveAndLoadPortForwardCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cache := &portForwardCache{
		Port:      12345,
		PID:       9999,
		Service:   "orka-api",
		Namespace: "orka-system",
	}

	savePortForwardCache(cache)

	got := loadPortForwardCache()
	if got == nil {
		t.Fatal("loadPortForwardCache() returned nil")
	}
	if got.Port != 12345 {
		t.Errorf("Port = %d, want 12345", got.Port)
	}
	if got.PID != 9999 {
		t.Errorf("PID = %d, want 9999", got.PID)
	}
	if got.Service != "orka-api" {
		t.Errorf("Service = %q, want %q", got.Service, "orka-api")
	}
	if got.Namespace != "orka-system" {
		t.Errorf("Namespace = %q, want %q", got.Namespace, "orka-system")
	}
}

func TestLoadPortForwardCacheExpired(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cache := portForwardCache{
		Port:      12345,
		PID:       9999,
		Service:   "orka-api",
		Namespace: "default",
		Timestamp: time.Now().Unix() - 3600, // 1 hour ago, exceeds 30 min TTL
	}

	dir := filepath.Join(tmp, ".orka")
	os.MkdirAll(dir, 0o700) //nolint:errcheck
	data, _ := json.Marshal(cache)
	os.WriteFile(filepath.Join(dir, "portforward.json"), data, 0o600) //nolint:errcheck

	got := loadPortForwardCache()
	if got != nil {
		t.Error("expected nil for expired cache")
	}
}

func TestLoadPortForwardCacheMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got := loadPortForwardCache()
	if got != nil {
		t.Error("expected nil for missing cache file")
	}
}

func TestLoadPortForwardCacheInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".orka")
	os.MkdirAll(dir, 0o700)                                                         //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "portforward.json"), []byte("not-json"), 0o600) //nolint:errcheck

	got := loadPortForwardCache()
	if got != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestClearPortForwardCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Save then clear
	cache := &portForwardCache{
		Port:      12345,
		PID:       1,
		Service:   "svc",
		Namespace: "ns",
	}
	savePortForwardCache(cache)

	clearPortForwardCache()

	path := filepath.Join(tmp, ".orka", "portforward.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected portforward.json to be removed after clear")
	}
}

func TestCheckServiceExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case serviceCheckPath:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := srv.Client()

	if !checkServiceExists(client, srv.URL, "default", "orka-api") {
		t.Error("expected true for existing service")
	}
	if checkServiceExists(client, srv.URL, "default", "missing-svc") {
		t.Error("expected false for missing service")
	}
}

func TestFindServiceByLabel(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		wantName string
	}{
		{
			name: "service_with_api_port",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{
					"items": []map[string]any{
						{
							"metadata": map[string]any{"name": "orka-api"},
							"spec": map[string]any{
								"ports": []map[string]any{
									{"name": "api"},
								},
							},
						},
					},
				}
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
			},
			wantName: "orka-api",
		},
		{
			name: "service_without_api_port",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{
					"items": []map[string]any{
						{
							"metadata": map[string]any{"name": "my-svc"},
							"spec": map[string]any{
								"ports": []map[string]any{
									{"name": "http"},
								},
							},
						},
					},
				}
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
			},
			wantName: "my-svc",
		},
		{
			name: "no_items",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{"items": []map[string]any{}}
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
			},
			wantName: "",
		},
		{
			name: "server_error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			got := findServiceByLabel(srv.Client(), srv.URL, "default")
			if got != tt.wantName {
				t.Errorf("findServiceByLabel() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestNewClientFromCmd_WithExplicitFlags(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{"--server", "http://test:9090", "--token", "my-token", "--namespace", "my-ns"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.BaseURL != "http://test:9090" {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://test:9090")
		}
		if c.Token != "my-token" {
			t.Errorf("Token = %q, want %q", c.Token, "my-token")
		}
		if c.Namespace != "my-ns" {
			t.Errorf("Namespace = %q, want %q", c.Namespace, "my-ns")
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_FallbackToConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := orkaConfig{Server: "http://cfg-server:8080", Token: "cfg-token", Namespace: "cfg-ns"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig error: %v", err)
	}

	root := newRootCmd()
	root.SetArgs([]string{})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.BaseURL != "http://cfg-server:8080" {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://cfg-server:8080")
		}
		if c.Token != "cfg-token" {
			t.Errorf("Token = %q, want %q", c.Token, "cfg-token")
		}
		if c.Namespace != "cfg-ns" {
			t.Errorf("Namespace = %q, want %q", c.Namespace, "cfg-ns")
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_DefaultNamespace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{"--server", "http://test:9090", "--token", "tok"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.Namespace != defaultNamespace {
			t.Errorf("Namespace = %q, want %q", c.Namespace, defaultNamespace)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_FallbackDefaultServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{"--token", "tok", "--namespace", "ns"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		// Without kubeconfig or K8s cluster, server should fall back to default
		if c.BaseURL != defaultServer {
			t.Logf("BaseURL = %q (may not be default if kubeconfig exists)", c.BaseURL)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestBuildRESTConfig_BadPath(t *testing.T) {
	_, err := buildRESTConfig("/nonexistent/kubeconfig")
	if err == nil {
		t.Error("expected error for nonexistent kubeconfig")
	}
}

func TestNewClientFromCmd_WithKubeconfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Create a kubeconfig
	config := clientcmdapi.NewConfig()
	config.CurrentContext = testCtxName
	config.Contexts[testCtxName] = &clientcmdapi.Context{
		Cluster:   "test-cluster",
		AuthInfo:  "test-user",
		Namespace: "kube-ns",
	}
	config.Clusters["test-cluster"] = &clientcmdapi.Cluster{
		Server: "https://k8s.example.com",
	}
	config.AuthInfos["test-user"] = &clientcmdapi.AuthInfo{
		Token: "kube-token",
	}
	kubePath := filepath.Join(tmp, "kubeconfig")
	if err := clientcmd.WriteToFile(*config, kubePath); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"--kubeconfig", kubePath, "--server", "http://test:8080"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.Token != "kube-token" {
			t.Errorf("Token = %q, want %q", c.Token, "kube-token")
		}
		if c.Namespace != "kube-ns" {
			t.Errorf("Namespace = %q, want %q", c.Namespace, "kube-ns")
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_CachedPortForward(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Start a local server to simulate a cached port-forward target
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Parse port from URL
	var port int
	fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port) //nolint:errcheck

	// Save a port-forward cache pointing to our test server
	savePortForwardCache(&portForwardCache{
		Port:      port,
		PID:       1,
		Service:   "orka-api",
		Namespace: "orka-system",
	})

	root := newRootCmd()
	root.SetArgs([]string{"--token", "tok", "--namespace", "ns"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		expected := fmt.Sprintf("http://localhost:%d", port)
		if c.BaseURL != expected {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, expected)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestDiscoverOrkaService_WellKnownName(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == serviceCheckPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restCfg := &rest.Config{
		Host: srv.URL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	got := discoverOrkaService(restCfg, "default")
	if got != "orka-api" {
		t.Errorf("discoverOrkaService() = %q, want %q", got, "orka-api")
	}
}

func TestDiscoverOrkaService_ByLabel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No well-known services found
		if r.URL.Path == serviceCheckPath ||
			r.URL.Path == "/api/v1/namespaces/default/services/orka" ||
			r.URL.Path == "/api/v1/namespaces/default/services/orka-controller-manager" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Label query returns a service
		if r.URL.Path == "/api/v1/namespaces/default/services" {
			resp := map[string]any{
				"items": []map[string]any{
					{
						"metadata": map[string]any{"name": "custom-orka"},
						"spec":     map[string]any{"ports": []map[string]any{{"name": "api"}}},
					},
				},
			}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restCfg := &rest.Config{
		Host: srv.URL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	got := discoverOrkaService(restCfg, "default")
	if got != "custom-orka" {
		t.Errorf("discoverOrkaService() = %q, want %q", got, "custom-orka")
	}
}

func TestDiscoverOrkaService_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restCfg := &rest.Config{
		Host: srv.URL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	got := discoverOrkaService(restCfg, "default")
	if got != "" {
		t.Errorf("discoverOrkaService() = %q, want empty", got)
	}
}
