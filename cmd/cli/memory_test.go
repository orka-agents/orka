package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const memoryAPIPath = "/api/v1/memories"

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

func TestMemoryEnableDisableShortDescriptionsUseSentenceCase(t *testing.T) {
	if got := newMemoryEnableDisableCmd("enable").Short; got != "Enable a memory" {
		t.Fatalf("enable short = %q, want %q", got, "Enable a memory")
	}
	if got := newMemoryEnableDisableCmd("disable").Short; got != "Disable a memory" {
		t.Fatalf("disable short = %q, want %q", got, "Disable a memory")
	}
}

func TestMemoryCreateSendsIdempotencyKeyAndReportsDeferredOperation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != memoryAPIPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotKey = r.Header.Get("Idempotency-Key")
		w.Header().Set("Location", "/api/v1/memory-operations/mop-1")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "mop-1", "state": "queued"})
	}))
	defer srv.Close()

	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		"memory", "create", "--server", srv.URL, "--content", "remember this",
		"--idempotency-key", "stable-key",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotKey != "stable-key" {
		t.Fatalf("Idempotency-Key = %q, want stable-key", gotKey)
	}
	if !strings.Contains(output.String(), "Memory created accepted: mop-1") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestMemoryCreateWaitsForDeferredOperation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/memories":
			w.Header().Set("Location", "/api/v1/memory-operations/mop-2")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "mop-2", "state": "queued"})
		case "/api/v1/memory-operations/mop-2":
			polls++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "mop-2", "memoryId": "mem-2", "state": "succeeded"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		"memory", "create", "--server", srv.URL, "--content", "remember this", "--wait", "--wait-timeout", "3s",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if polls != 1 {
		t.Fatalf("polls = %d, want 1", polls)
	}
	if !strings.Contains(output.String(), "Memory created: mem-2") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestMemoryCreateWaitFailurePrintsDurableOperationContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != memoryAPIPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", "/api/v1/memory-operations/mop-wait")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "mop-wait", "state": "queued"})
	}))
	defer srv.Close()

	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		"memory", "create", "--server", srv.URL, "--content", "remember this", "--wait",
		"--wait-timeout", "10ms", "--idempotency-key", "stable-wait-key",
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	for _, value := range []string{"/api/v1/memory-operations/mop-wait", "mop-wait", "stable-wait-key"} {
		if !strings.Contains(output.String(), value) || !strings.Contains(err.Error(), value) {
			t.Fatalf("missing %q from output=%q error=%q", value, output.String(), err.Error())
		}
	}
}
