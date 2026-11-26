/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const agentsAPIPath = "/api/v1/agents"

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

func TestAgentGetShowsIdentityStatusAndReadyFalseReason(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	agent := map[string]any{
		"metadata": map[string]any{"name": "entra-agent", "namespace": "team-a"},
		"spec": map[string]any{
			"model":    map[string]any{"name": "gpt-5.6"},
			"runtime":  map[string]any{"type": "codex"},
			"identity": map[string]any{"entra": map[string]any{"agentIdentityID": "33333333-3333-3333-3333-33333333aaaa"}},
		},
		"status": map[string]any{
			"ready":       false,
			"activeTasks": 0,
			"identityClaim": map[string]any{
				"issuer": "entra",
				"id":     "33333333-3333-3333-3333-33333333aaaa",
			},
			"conditions": []any{
				map[string]any{
					"type":    "IdentityVerified",
					"status":  "True",
					"reason":  "Verified",
					"message": "the issuer issued a token for this identity",
				},
				map[string]any{
					"type":    "Ready",
					"status":  "False",
					"reason":  "ValidationFailed",
					"message": "identity entra/33333333-3333-3333-3333-33333333aaaa is already bound to Agent team-a/primary-agent",
				},
			},
		},
	}
	srv := jsonServer(t, map[string]any{"/api/v1/agents/entra-agent": agent})
	defer srv.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"agent", "get", "entra-agent", "--server", srv.URL, "--namespace", "team-a"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	got := out.String()
	normalized := strings.Join(strings.Fields(got), " ")
	for _, want := range []string{
		"Identity: entra/33333333-3333-3333-3333-33333333aaaa",
		"Identity Verified: True / Verified — the issuer issued a token for this identity",
		"Ready: false (identity entra/33333333-3333-3333-3333-33333333aaaa is already bound to Agent team-a/primary-agent)",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("agent get output missing %q:\n%s", want, got)
		}
	}
}

func TestAgentListShowsIdentityAndReadyColumns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == agentsAPIPath {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"metadata": map[string]any{"name": "entra-agent"},
						"spec":     map[string]any{"model": map[string]any{"name": "gpt-5.6"}, "runtime": map[string]any{"type": "codex"}},
						"status": map[string]any{
							"ready":         true,
							"activeTasks":   1,
							"identityClaim": map[string]any{"issuer": "entra", "id": "33333333-3333-3333-3333-33333333aaaa"},
						},
					},
					{
						"metadata": map[string]any{"name": "plain-agent"},
						"spec":     map[string]any{"model": map[string]any{"name": "gpt-4"}},
						"status":   map[string]any{"ready": false, "activeTasks": 0},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"agent", "list", "--server", srv.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	got := out.String()
	for _, want := range []string{"IDENTITY", "READY", "entra/33333333-3333-3333-3333-33333333aaaa", "true", "false"} {
		if !strings.Contains(got, want) {
			t.Errorf("agent list output missing %q:\n%s", want, got)
		}
	}
}
