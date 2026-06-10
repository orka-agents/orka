/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFormatAge(t *testing.T) {
	tests := []struct {
		name      string
		timestamp string
		wantExact string // exact match, or "" if we just want a suffix check
		wantSfx   string // suffix to check
	}{
		{"empty", "", "<unknown>", ""},
		{"invalid", "not-a-date", "not-a-date", ""},
		{"seconds_ago", time.Now().Add(-30 * time.Second).Format(time.RFC3339), "", "s"},
		{"minutes_ago", time.Now().Add(-5 * time.Minute).Format(time.RFC3339), "", "m"},
		{"hours_ago", time.Now().Add(-3 * time.Hour).Format(time.RFC3339), "", "h"},
		{"days_ago", time.Now().Add(-48 * time.Hour).Format(time.RFC3339), "", "d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAge(tt.timestamp)
			if tt.wantExact != "" {
				if got != tt.wantExact {
					t.Errorf("formatAge(%q) = %q, want %q", tt.timestamp, got, tt.wantExact)
				}
			} else if tt.wantSfx != "" {
				if len(got) == 0 {
					t.Errorf("formatAge(%q) returned empty string", tt.timestamp)
				} else if got[len(got)-1:] != tt.wantSfx {
					t.Errorf("formatAge(%q) = %q, want suffix %q", tt.timestamp, got, tt.wantSfx)
				}
			}
		})
	}
}

func TestNewTaskCmd(t *testing.T) {
	cmd := newTaskCmd()

	if cmd.Use != "task" {
		t.Errorf("Use = %q, want %q", cmd.Use, "task")
	}

	// Verify subcommands exist
	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Use] = true
	}
	for _, want := range []string{"create <prompt>", "list", "get <name>", "logs <name>", "delete <name>"} {
		if !subNames[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestNewTaskCreateCmdFlags(t *testing.T) {
	cmd := newTaskCreateCmd()

	// Verify flags
	for _, flagName := range []string{"type", "agent", "provider", "timeout"} {
		if cmd.Flags().Lookup(flagName) == nil {
			t.Errorf("missing flag %q", flagName)
		}
	}

	// Verify default values
	typeVal, _ := cmd.Flags().GetString("type")
	if typeVal != "ai" {
		t.Errorf("default type = %q, want %q", typeVal, "ai")
	}
	providerVal, _ := cmd.Flags().GetString("provider")
	if providerVal != "default" {
		t.Errorf("default provider = %q, want %q", providerVal, "default")
	}
}

func TestNewTaskListCmdAliases(t *testing.T) {
	cmd := newTaskListCmd()

	found := slices.Contains(cmd.Aliases, "ls")
	if !found {
		t.Error("expected 'ls' alias on list command")
	}

	// Verify flags
	if cmd.Flags().Lookup("status") == nil {
		t.Error("missing flag 'status'")
	}
	if cmd.Flags().Lookup("limit") == nil {
		t.Error("missing flag 'limit'")
	}
}

func TestNewTaskGetCmdArgs(t *testing.T) {
	cmd := newTaskGetCmd()
	cmd.SetArgs([]string{})
	// Without args, should error (requires exactly 1)
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no args provided")
	}
}

func TestNewTaskDeleteCmdAliases(t *testing.T) {
	cmd := newTaskDeleteCmd()
	found := slices.Contains(cmd.Aliases, "rm")
	if !found {
		t.Error("expected 'rm' alias on delete command")
	}
}

func TestNewTaskLogsCmdFlags(t *testing.T) {
	cmd := newTaskLogsCmd()

	flag := cmd.Flags().Lookup("follow")
	if flag == nil {
		t.Fatal("missing flag 'follow'")
	}
	if flag.Shorthand != "f" {
		t.Errorf("follow shorthand = %q, want %q", flag.Shorthand, "f")
	}
}

// ---------------------------------------------------------------------------
// Task command execution with mock servers
// ---------------------------------------------------------------------------

func taskAPIServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == tasksAPIPath:
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"metadata": map[string]any{"name": "task-abc123", "namespace": "default"},
			})
		case r.Method == http.MethodGet && r.URL.Path == tasksAPIPath:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"metadata": map[string]any{
							"name": "t1", "namespace": "default",
							"creationTimestamp": time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
							"labels":            map[string]any{"orka.ai/parent-task": "parent-task"},
						},
						"spec": map[string]any{
							"type":        "ai",
							"transaction": map[string]any{"id": "txn-123"},
						},
						"status": map[string]any{"phase": "Succeeded"},
					},
					{
						"metadata": map[string]any{
							"name": "t2", "namespace": "default",
							"creationTimestamp": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
						},
						"spec":   map[string]any{"type": "container"},
						"status": map[string]any{"phase": "Running"},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/my-task":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"metadata": map[string]any{"name": "my-task"},
				"spec": map[string]any{
					"transaction": map[string]any{
						"id":      "txn-123",
						"profile": "kontxt",
					},
				},
				"status": map[string]any{"phase": "Succeeded"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/my-task/logs":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"logs": "line1\nline2\n",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/msg-task/logs":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"message": "Task is still pending",
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/tasks/my-task":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "not found: %s %s", r.Method, r.URL.Path) //nolint:errcheck
		}
	}))
}

func TestNewTaskCreateCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "create", "--server", srv.URL, "do something"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskCreateCmd_WithAgent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "create", "--server", srv.URL, "--agent", "my-agent", "--type", "agent", "do stuff"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskListCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "list", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskListCmd_WithStatus(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "list", "--server", srv.URL, "--status", "Running"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskListCmd_WithStatusScansPaginatedResults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != tasksAPIPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests = append(requests, r.URL.RawQuery)
		if got := r.URL.Query().Get("limit"); got != "500" {
			t.Errorf("limit query = %q, want 500 for filtered pagination", got)
		}

		switch r.URL.Query().Get("continue") {
		case "":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"metadata": map[string]any{
							"name":              "first-page-task",
							"namespace":         "default",
							"creationTimestamp": time.Now().Format(time.RFC3339),
						},
						"spec":   map[string]any{"type": "ai"},
						"status": map[string]any{"phase": "Succeeded"},
					},
				},
				"metadata": map[string]any{"continue": "next"},
			})
		case "next":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"metadata": map[string]any{
							"name":              "matching-task",
							"namespace":         "default",
							"creationTimestamp": time.Now().Format(time.RFC3339),
						},
						"spec":   map[string]any{"type": "ai"},
						"status": map[string]any{"phase": "Running"},
					},
				},
			})
		default:
			t.Errorf("unexpected continue token %q", r.URL.Query().Get("continue"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "list", "--server", srv.URL, "--status", "Running"})

	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if !strings.Contains(stdout, "matching-task") {
		t.Fatalf("stdout = %q, want paginated matching task", stdout)
	}
}

func TestNewTaskListCmd_WithTransaction(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "list", "--server", srv.URL, "--transaction", "txn-123"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskListCmd_Empty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}}) //nolint:errcheck
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "list", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskGetCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "get", "--server", srv.URL, "my-task"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskGetCmd_ShowTransaction(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "get", "--server", srv.URL, "--show-transaction", "my-task"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskGetCmd_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "get", "--server", srv.URL, "nonexistent"})

	if err := root.Execute(); err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestNewTaskLogsCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "logs", "--server", srv.URL, "my-task"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskLogsCmd_MessageOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "logs", "--server", srv.URL, "msg-task"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskDeleteCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "delete", "--server", srv.URL, "my-task"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskDeleteCmd_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "delete", "--server", srv.URL, "nonexistent"})

	if err := root.Execute(); err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestNewTaskLogsCmd_NoArgs(t *testing.T) {
	cmd := newTaskLogsCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewTaskDeleteCmd_NoArgs(t *testing.T) {
	cmd := newTaskDeleteCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewTaskLogsCmd_Follow(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/my-task/logs" && r.URL.Query().Get("follow") == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			fmt.Fprint(w, "event: log\ndata: {\"line\":\"log line 1\"}\n\n") //nolint:errcheck
			if ok {
				flusher.Flush()
			}
			// Close immediately to end stream
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "logs", "--server", srv.URL, "--follow", "my-task"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskLogsCmd_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "not found") //nolint:errcheck
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "logs", "--server", srv.URL, "nonexistent"})

	if err := root.Execute(); err == nil {
		t.Error("expected error for nonexistent task logs")
	}
}

func TestNewTaskCreateCmd_NoArgs(t *testing.T) {
	cmd := newTaskCreateCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewTaskListCmd_WithStatusNoMatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "list", "--server", srv.URL, "--status", "Pending"})

	// Should display "No tasks found." since no Pending tasks
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewTaskCreateCmd_ContainerType(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "create", "--server", srv.URL, "--type", "container", "run my container"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestTaskCreateManifestInjectsNamespace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	manifest := filepath.Join(tmp, "task.yaml")
	manifestData := []byte("metadata:\n  name: manifest-task\nspec:\n  type: container\n  image: alpine\n")
	if err := os.WriteFile(manifest, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != tasksAPIPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"metadata": map[string]any{"name": "manifest-task", "namespace": "team-a"},
		})
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"task", "create", "--server", srv.URL, "--namespace", "team-a", "-f", manifest})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	metadata, ok := captured["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing in request: %#v", captured)
	}
	if got := metadata["namespace"]; got != "team-a" {
		t.Fatalf("metadata.namespace = %#v, want team-a", got)
	}
}

func TestTaskCreateRequiresPromptForDefaultAI(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{"task", "create", "--server", "http://127.0.0.1:1"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("Execute() error = %v, want prompt required", err)
	}
}
