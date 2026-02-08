/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestPrepareWorkspace_NoOp(t *testing.T) {
	// When MERCAN_PRIOR_TASK is not set, PrepareWorkspace should be a no-op
	os.Unsetenv("MERCAN_PRIOR_TASK")
	err := PrepareWorkspace("/tmp/test")
	if err != nil {
		t.Errorf("expected no error when MERCAN_PRIOR_TASK not set, got: %v", err)
	}
}

func TestPrepareWorkspace_MissingControllerURL(t *testing.T) {
	t.Setenv("MERCAN_PRIOR_TASK", "task-1")
	t.Setenv("MERCAN_CONTROLLER_URL", "")
	err := PrepareWorkspace("/tmp/test")
	if err == nil {
		t.Fatal("expected error when MERCAN_CONTROLLER_URL is empty")
	}
}

func TestPrepareWorkspace_NoDiffInResult(t *testing.T) {
	// Mock server returns a structured result with no diff
	sr := StructuredResult{Version: 1, Summary: "completed"}
	resultJSON, _ := json.Marshal(sr)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"result": string(resultJSON)})
	}))
	defer server.Close()

	t.Setenv("MERCAN_PRIOR_TASK", "task-1")
	t.Setenv("MERCAN_PRIOR_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_CONTROLLER_URL", server.URL)

	err := PrepareWorkspace("/tmp/test")
	if err != nil {
		t.Errorf("expected no error when diff is empty, got: %v", err)
	}
}

func TestFinalizeResult_EmptyWorkDir(t *testing.T) {
	data, err := FinalizeResult("", "hello output")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "hello output" {
		t.Errorf("expected plain text output, got %q", string(data))
	}
}

func TestFinalizeResult_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	data, err := FinalizeResult(dir, "agent did stuff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "agent did stuff" {
		t.Errorf("expected plain text fallback, got %q", string(data))
	}
}

func TestParseDiffStatFiles(t *testing.T) {
	stat := ` auth.go       | 10 +++++++---
 middleware.go | 5 +++++
 2 files changed, 12 insertions(+), 3 deletions(-)
`
	files := parseDiffStatFiles(stat)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0] != "auth.go" {
		t.Errorf("expected auth.go, got %q", files[0])
	}
	if files[1] != "middleware.go" {
		t.Errorf("expected middleware.go, got %q", files[1])
	}
}
