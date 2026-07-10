/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	agentContinueKey  = "continue"
	agentNextContinue = "next"
	agentsAPIPath     = "/api/v1/agents"
)

func TestNewAgentCmd(t *testing.T) {
	cmd := newAgentCmd()

	if cmd.Use != "agent" {
		t.Errorf("Use = %q, want %q", cmd.Use, "agent")
	}
	if cmd.Short != "Manage agents" {
		t.Errorf("Short = %q, want %q", cmd.Short, "Manage agents")
	}

	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Use] = true
	}
	for _, want := range []string{"list", "get <name>", "delete <name>"} {
		if !subNames[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestNewAgentGetCmdRequiresArgs(t *testing.T) {
	cmd := newAgentGetCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewAgentDeleteCmdRequiresArgs(t *testing.T) {
	cmd := newAgentDeleteCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewAgentListCmdStructure(t *testing.T) {
	cmd := newAgentListCmd()

	if cmd.Use != "list" {
		t.Errorf("Use = %q, want %q", cmd.Use, "list")
	}
	if cmd.Short != "List agents" {
		t.Errorf("Short = %q, want %q", cmd.Short, "List agents")
	}
}

// ---------------------------------------------------------------------------
// Agent command execution with mock server
// ---------------------------------------------------------------------------

func agentAPIServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == agentsAPIPath:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"metadata": map[string]any{"name": "agent-1"},
						"spec": map[string]any{
							"model":   map[string]any{"name": "gpt-4"},
							"runtime": map[string]any{"type": "builtin"},
						},
					},
					{
						"metadata": map[string]any{"name": "agent-2"},
						"spec":     map[string]any{},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/agent-1":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"metadata": map[string]any{"name": "agent-1"},
				"spec":     map[string]any{"model": map[string]any{"name": "gpt-4"}},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/agents/agent-1":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestNewAgentListCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := agentAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "list", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewAgentListCmd_ListsThe101stAgent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			items := make([]map[string]any, 100)
			for i := range items {
				items[i] = map[string]any{
					"metadata": map[string]any{"name": fmt.Sprintf("agent-%03d", i+1)},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    items,
				"metadata": map[string]any{agentContinueKey: agentNextContinue},
			})
		case 2:
			if got := r.URL.Query().Get(agentContinueKey); got != agentNextContinue {
				t.Errorf("continue query = %q, want %s", got, agentNextContinue)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{"metadata": map[string]any{"name": "agent-101"}},
				},
				"metadata": map[string]any{},
			})
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "list", "--server", srv.URL})

	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if !strings.Contains(stdout, "agent-101") {
		t.Fatalf("stdout = %q, want agent-101", stdout)
	}
}

func TestNewAgentListCmd_Empty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}}) //nolint:errcheck
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "list", "--server", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewAgentListCmd_HonorsCommandContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}}) //nolint:errcheck
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "list", "--server", srv.URL})

	err := root.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteContext() error = %v, want context canceled", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestNewAgentGetCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := agentAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "get", "--server", srv.URL, "agent-1"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewAgentDeleteCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := agentAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "delete", "--server", srv.URL, "agent-1"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewAgentDeleteCmd_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := agentAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"agent", "delete", "--server", srv.URL, "nonexistent"})

	if err := root.Execute(); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}
