package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestMemoryEnableDisableShortDescriptionsUseSentenceCase(t *testing.T) {
	if got := newMemoryEnableDisableCmd("enable").Short; got != "Enable a memory" {
		t.Fatalf("enable short = %q, want %q", got, "Enable a memory")
	}
	if got := newMemoryEnableDisableCmd("disable").Short; got != "Disable a memory" {
		t.Fatalf("disable short = %q, want %q", got, "Disable a memory")
	}
}

const recordedMemoryReviewer = "system:serviceaccount:orka-system:jordan"

func TestMemoryProposalReviewPrintsRecordedReviewer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/memory-proposals/mprop-1/review" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"id": "mprop-1", "namespace": "default", "status": "accepted",
			"reviewer": recordedMemoryReviewer, "reviewNote": "Matches the operations note.",
		})
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "memory", "proposal", "review", "mprop-1", "--status", "accepted", "--note", "Matches the operations note.")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if _, ok := gotBody["reviewer"]; ok {
		t.Fatalf("review body names a reviewer: %#v", gotBody)
	}
	if gotBody["status"] != "accepted" || gotBody["reviewNote"] != "Matches the operations note." {
		t.Fatalf("review body = %#v", gotBody)
	}
	for _, want := range []string{"Status:", "accepted", "Reviewer:", recordedMemoryReviewer} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryProposalReviewAgainstServerWithoutRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "memory", "proposal", "review", "mprop-1", "--status", "accepted")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "Memory proposal reviewed: mprop-1" {
		t.Fatalf("output = %q", out)
	}
}

func TestMemoryProposalApplyPrintsRecordedApplier(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/memory-proposals/mprop-1/apply":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "mem-1", "namespace": "default"}) //nolint:errcheck
		case "GET /api/v1/memory-proposals/mprop-1":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"id": "mprop-1", "status": "applied", "appliedMemoryId": "mem-1", "appliedBy": recordedMemoryReviewer,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "memory", "proposal", "apply", "mprop-1")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if _, ok := gotBody["appliedBy"]; ok {
		t.Fatalf("apply body names an applier: %#v", gotBody)
	}
	for _, want := range []string{"Memory proposal applied: mprop-1 -> mem-1", "Applied by:", recordedMemoryReviewer} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryProposalApplySucceedsWhenTheProposalCannotBeRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(map[string]any{"id": "mem-1"}) //nolint:errcheck
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	out, errOut, err := runCLISplit(t, srv.URL, "memory", "proposal", "apply", "mprop-1")
	if err != nil {
		t.Fatalf("apply: %v\n%s%s", err, out, errOut)
	}
	if strings.TrimSpace(out) != "Memory proposal applied: mprop-1 -> mem-1" {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "could not read the recorded applier") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestMemoryProposalIdentityFlagsAreGone(t *testing.T) {
	for _, args := range [][]string{
		{"memory", "proposal", "review", "mprop-1", "--status", "accepted", "--reviewer", "someone-else"},
		{"memory", "proposal", "apply", "mprop-1", "--applied-by", "someone-else"},
	} {
		_, err := runCLI(t, "http://127.0.0.1:1", args...)
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%v: err = %v, want unknown flag", args, err)
		}
	}
}
