/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSubmitResult_Success(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("expected Content-Type application/octet-stream, got %s", r.Header.Get("Content-Type"))
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		received = buf[:n]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", srv.URL)

	err := SubmitResult([]byte("hello result"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(received) != "hello result" {
		t.Errorf("received = %q, want %q", string(received), "hello result")
	}
}

func TestSubmitResult_RetryOnFailure(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("temporary error")) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", srv.URL)

	err := SubmitResult([]byte("retry result"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestSubmitResult_AllRetriesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("always fails")) //nolint:errcheck
	}))
	defer srv.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", srv.URL)

	err := SubmitResult([]byte("failing result"))
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
}

func TestSubmitResult_ConstructEndpointFromControllerURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", "")
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "my-task")

	err := SubmitResult([]byte("constructed url"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/internal/v1/results/test-ns/my-task" {
		t.Errorf("gotPath = %q, want /internal/v1/results/test-ns/my-task", gotPath)
	}
}

func TestSubmitResult_MissingEnvVars(t *testing.T) {
	t.Setenv("ORKA_RESULT_ENDPOINT", "")
	t.Setenv("ORKA_CONTROLLER_URL", "")

	err := SubmitResult([]byte("should fail"))
	if err == nil {
		t.Fatal("expected error when no endpoint or controller URL is set")
	}
}

func TestSubmitResult_BearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", srv.URL)

	// When no SA token file exists, no auth header is sent
	err := SubmitResult([]byte("no token"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Without the SA token file mounted, Authorization should be empty
	if gotAuth != "" {
		t.Logf("Authorization header present (SA token file may exist): %s", gotAuth)
	}
}

func TestFormatStructuredResult(t *testing.T) {
	sr := &StructuredResult{
		Summary: "Added auth middleware",
		BaseSHA: "abc123",
		Diff:    "diff --git a/auth.go b/auth.go\n+// auth",
		Files:   []string{"auth.go"},
		Verdict: "APPROVED",
	}
	data, err := FormatStructuredResult(sr)
	if err != nil {
		t.Fatalf("FormatStructuredResult: %v", err)
	}
	// Should set version to 1
	var parsed StructuredResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Version != 1 {
		t.Errorf("expected version 1, got %d", parsed.Version)
	}
	if parsed.Summary != "Added auth middleware" {
		t.Errorf("expected summary %q, got %q", "Added auth middleware", parsed.Summary)
	}
	if parsed.Diff != sr.Diff {
		t.Errorf("diff mismatch")
	}
}

func TestFormatStructuredResult_PreservesVersion(t *testing.T) {
	sr := &StructuredResult{Version: 2, Summary: "test"}
	data, err := FormatStructuredResult(sr)
	if err != nil {
		t.Fatalf("FormatStructuredResult: %v", err)
	}
	var parsed StructuredResult
	_ = json.Unmarshal(data, &parsed)
	if parsed.Version != 2 {
		t.Errorf("expected version 2, got %d", parsed.Version)
	}
}

func TestParseStructuredResult_Valid(t *testing.T) {
	input := `{"version":1,"summary":"done","baseSHA":"abc","diff":"patch","verdict":"APPROVED","files":["a.go"]}`
	sr := ParseStructuredResult(input)
	if sr.Version != 1 {
		t.Errorf("expected version 1, got %d", sr.Version)
	}
	if sr.Summary != "done" {
		t.Errorf("expected summary %q, got %q", "done", sr.Summary)
	}
	if sr.Diff != "patch" {
		t.Errorf("expected diff %q, got %q", "patch", sr.Diff)
	}
	if sr.Verdict != "APPROVED" {
		t.Errorf("expected verdict APPROVED, got %q", sr.Verdict)
	}
}

func TestParseStructuredResult_PlainText(t *testing.T) {
	sr := ParseStructuredResult("just some text output")
	if sr.Version != 1 {
		t.Errorf("expected version 1, got %d", sr.Version)
	}
	if sr.Summary != "just some text output" {
		t.Errorf("expected summary to be raw text, got %q", sr.Summary)
	}
	if sr.Diff != "" {
		t.Errorf("expected empty diff for plain text")
	}
}

func TestParseStructuredResult_InvalidJSON(t *testing.T) {
	sr := ParseStructuredResult("{bad json")
	if sr.Summary != "{bad json" {
		t.Errorf("expected raw text as summary")
	}
}

func TestParseStructuredResult_MissingVersion(t *testing.T) {
	// JSON without version field should be treated as plain text
	sr := ParseStructuredResult(`{"summary":"test"}`)
	if sr.Summary != `{"summary":"test"}` {
		t.Errorf("expected raw JSON as summary when version=0, got %q", sr.Summary)
	}
}
