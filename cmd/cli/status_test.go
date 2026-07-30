/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	statusError      = "error"
	statusHealthPath = "/healthz"
	statusReadyPath  = "/readyz"
	tasksAPIPath     = "/api/v1/tasks"
)

func TestNewStatusCmd_Structure(t *testing.T) {
	cmd := newStatusCmd()

	if cmd.Use != "status" {
		t.Errorf("Use = %q, want %q", cmd.Use, "status")
	}
	if cmd.Short == "" {
		t.Error("Short description should not be empty")
	}
}

func statusServer(healthy, ready bool, tasks []client.TaskSummary, agents []client.AgentSummary) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case statusHealthPath:
			status := "ok"
			if !healthy {
				status = statusError
			}
			json.NewEncoder(w).Encode(map[string]any{"status": status}) //nolint:errcheck
		case statusReadyPath:
			status := "ok"
			if !ready {
				status = statusError
			}
			json.NewEncoder(w).Encode(map[string]any{"status": status}) //nolint:errcheck
		case tasksAPIPath:
			// Build items list from summaries (simplified)
			items := make([]map[string]any, len(tasks))
			for i, t := range tasks {
				items[i] = map[string]any{
					"metadata": map[string]any{"name": t.Name, "namespace": t.Namespace},
					"spec":     map[string]any{"type": t.Type},
					"status":   map[string]any{"phase": t.Phase},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items}) //nolint:errcheck
		case "/api/v1/agents":
			items := make([]map[string]any, len(agents))
			for i, a := range agents {
				items[i] = map[string]any{
					"metadata": map[string]any{"name": a.Name},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestNewStatusCmd_HealthyServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := statusServer(true, true,
		[]client.TaskSummary{
			{Name: "t1", Phase: "Running"},
			{Name: "t2", Phase: "Succeeded"},
		},
		[]client.AgentSummary{{Name: "a1"}},
	)
	defer srv.Close()

	cmd := newStatusCmd()
	root := newRootCmd()
	root.AddCommand(cmd)
	root.SetArgs([]string{"status", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewStatusCmdCountsAllTaskAndAgentPagesInSelectedNamespace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		namespace         = "inventory-ns"
		taskContinuation  = "task-cursor+/=? segment"
		agentContinuation = "agent-cursor+/=? segment"
	)
	var taskRequests, agentRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case statusHealthPath, statusReadyPath:
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
		case tasksAPIPath:
			taskRequests++
			if got := r.URL.Query().Get("namespace"); got != namespace {
				t.Errorf("task namespace = %q, want %q", got, namespace)
			}
			if got := r.URL.Query().Get("limit"); got != "500" {
				t.Errorf("task limit = %q, want 500", got)
			}
			if taskRequests == 1 {
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"items": []map[string]any{{
						"metadata": map[string]any{"name": "task-1", "namespace": namespace},
						"status":   map[string]any{"phase": "Running"},
					}},
					"metadata": map[string]any{"continue": taskContinuation},
				})
				return
			}
			if got := r.URL.Query().Get("continue"); got != taskContinuation {
				t.Errorf("task continue = %q, want %q", got, taskContinuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{{
					"metadata": map[string]any{"name": "task-2", "namespace": namespace},
					"status":   map[string]any{"phase": "Succeeded"},
				}},
				"metadata": map[string]any{},
			})
		case agentsAPIPath:
			agentRequests++
			if got := r.URL.Query().Get("namespace"); got != namespace {
				t.Errorf("agent namespace = %q, want %q", got, namespace)
			}
			if agentRequests == 1 {
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"items":    []map[string]any{{"metadata": map[string]any{"name": "agent-1"}}},
					"metadata": map[string]any{"continue": agentContinuation},
				})
				return
			}
			if got := r.URL.Query().Get("continue"); got != agentContinuation {
				t.Errorf("agent continue = %q, want %q", got, agentContinuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    []map[string]any{{"metadata": map[string]any{"name": "agent-2"}}},
				"metadata": map[string]any{},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"status", "--server", srv.URL, "--namespace", namespace})

	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if taskRequests != 2 || agentRequests != 2 {
		t.Fatalf("task/agent requests = %d/%d, want 2/2", taskRequests, agentRequests)
	}
	for _, want := range []string{"Running:    1", "Succeeded:  1", "Agents:    2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
}

func TestNewStatusCmdReportsPartialInventoryErrorInsteadOfUndercount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var taskRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case statusHealthPath, statusReadyPath:
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
		case tasksAPIPath:
			taskRequests++
			if taskRequests == 1 {
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"items": []map[string]any{{
						"metadata": map[string]any{"name": "task-1"},
						"status":   map[string]any{"phase": "Running"},
					}},
					"metadata": map[string]any{"continue": "next"},
				})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "later task page failed") //nolint:errcheck
		case agentsAPIPath:
			json.NewEncoder(w).Encode(map[string]any{"items": []any{}}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	root := newRootCmd()
	root.SetErr(&stderr)
	root.SetArgs([]string{"status", "--server", srv.URL})
	if _, err := captureOutput(t, root.Execute); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(stderr.String(), "after 1 matching items on page 2") {
		t.Fatalf("stderr = %q, want partial inventory error", stderr.String())
	}
	if strings.Contains(stderr.String(), "Running:    1") {
		t.Fatalf("stderr = %q, should not present partial inventory as a complete count", stderr.String())
	}
}

func TestNewStatusCmdHonorsCommandContextCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := newRootCmd()
	root.SetArgs([]string{"status", "--server", srv.URL})
	if err := root.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteContext() error = %v, want context canceled", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestNewStatusCmd_UnhealthyServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := statusServer(false, false, nil, nil)
	defer srv.Close()

	cmd := newStatusCmd()
	root := newRootCmd()
	root.AddCommand(cmd)
	root.SetArgs([]string{"status", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewStatusCmd_UnreachableServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newStatusCmd()
	root := newRootCmd()
	root.AddCommand(cmd)
	root.SetArgs([]string{"status", "--server", "http://127.0.0.1:1"})

	// Should not error — it prints the error inline
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewStatusCmd_TaskErrors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case statusHealthPath:
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
		case statusReadyPath:
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
		case tasksAPIPath:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "task error") //nolint:errcheck
		case "/api/v1/agents":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "agent error") //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cmd := newStatusCmd()
	root := newRootCmd()
	root.AddCommand(cmd)
	root.SetArgs([]string{"status", "--server", srv.URL})

	// Errors are printed to stderr, not returned
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewStatusCmd_AutonomousTasks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	tasks := []client.TaskSummary{
		{Name: "auto-1", Phase: "Running", Iteration: 3},
		{Name: "normal", Phase: "Succeeded", Iteration: 0},
	}

	srv := statusServer(true, true, tasks, nil)
	defer srv.Close()

	cmd := newStatusCmd()
	root := newRootCmd()
	root.AddCommand(cmd)
	root.SetArgs([]string{"status", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewStatusCmd_ScheduledTasks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	tasks := []client.TaskSummary{
		{Name: "sched-1", Phase: "Scheduled"},
	}

	srv := statusServer(true, true, tasks, nil)
	defer srv.Close()

	cmd := newStatusCmd()
	root := newRootCmd()
	root.AddCommand(cmd)
	root.SetArgs([]string{"status", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}
