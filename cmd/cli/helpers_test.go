/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
		case "/api/v1/namespaces/default/services/orka-api":
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
