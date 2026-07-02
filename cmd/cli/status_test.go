/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	statusError  = "error"
	tasksAPIPath = "/api/v1/tasks"
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
		case "/healthz":
			status := "ok"
			if !healthy {
				status = statusError
			}
			json.NewEncoder(w).Encode(map[string]any{"status": status}) //nolint:errcheck
		case "/readyz":
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
		case "/healthz":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
		case "/readyz":
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
