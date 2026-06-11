package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryUpdateFileUsesManifestNamespace(t *testing.T) {
	const manifestNamespace = "team-a"
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	manifest := filepath.Join(tmp, "memory.yaml")
	manifestBody := []byte("namespace: " + manifestNamespace + "\ncontent: updated memory\n")
	if err := os.WriteFile(manifest, manifestBody, 0o600); err != nil {
		t.Fatal(err)
	}

	var gotNamespace string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/memories/mem-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotNamespace = r.URL.Query().Get("namespace")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "mem-1", "namespace": gotNamespace}) //nolint:errcheck
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"memory", "update", "mem-1", "--server", srv.URL, "-f", manifest})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotNamespace != manifestNamespace {
		t.Fatalf("namespace query = %q, want %s", gotNamespace, manifestNamespace)
	}
	if gotBody["namespace"] != manifestNamespace {
		t.Fatalf("body namespace = %#v, want %s", gotBody["namespace"], manifestNamespace)
	}
}
