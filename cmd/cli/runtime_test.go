package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimePoolListRendersCapacityAndAdmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-pools" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{
				"metadata": map[string]any{"name": "codex-read", "namespace": "default"},
				"status": map[string]any{
					"lifecycle": "Serving", "admissionState": "Accepting", "currentReplicas": 1, "desiredReplicas": 1,
					"capacity": map[string]any{"residentSessions": 3, "maxResidentSessions": 10, "runningPrompts": 2, "maxRunningPrompts": 4, "queuedTasks": 1},
				},
			}},
		})
	}))
	defer server.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--server", server.URL, "--token", "test-token", "runtime-pool", "list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex-read", "Serving", "Accepting", "3/10", "2/4"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestAgentRuntimeListRendersOnlyV2Identity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{
				"metadata": map[string]any{"name": "external-codex", "namespace": "default"},
				"spec": map[string]any{
					"contractVersion": "orka.harness.v2",
					"capabilities": map[string]any{
						"runtimeInstanceID": "external-instance-1",
						"profile":           map[string]any{"workspaceIntent": "read", "providerKind": "openai", "model": "gpt-5"},
					},
				},
				"status": map[string]any{"ready": true},
			}},
		})
	}))
	defer server.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--server", server.URL, "--token", "test-token", "agent-runtime", "list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"external-codex", "true", "orka.harness.v2", "read", "openai/gpt-5"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
	for _, legacy := range []string{"supportsContinuation", "toolExecutionModes", "brokeredToolClasses"} {
		if strings.Contains(out.String(), legacy) {
			t.Fatalf("output contains legacy field %q: %s", legacy, out.String())
		}
	}
}

func TestRuntimePoolCommandIsReadOnly(t *testing.T) {
	cmd := newRuntimePoolCmd()
	for _, child := range cmd.Commands() {
		switch child.Name() {
		case "list", "get":
		default:
			t.Fatalf("unexpected mutating RuntimePool command %q", child.Name())
		}
	}
}
