/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewAuditCmd(t *testing.T) {
	cmd := newAuditCmd()
	if cmd.Use != "audit" {
		t.Fatalf("Use = %q, want audit", cmd.Use)
	}
	if len(cmd.Commands()) != 1 || cmd.Commands()[0].Use != "trace <transaction-id>" {
		t.Fatalf("audit subcommands = %#v, want trace", cmd.Commands())
	}
}

func TestNewAuditTraceCmd_Execute(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := taskAPIServer()
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"audit", "trace", "--server", srv.URL, "txn-123"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestNewAuditTraceCmd_ScansPaginatedResults(t *testing.T) {
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
							"name":              "other-transaction-task",
							"namespace":         "default",
							"creationTimestamp": time.Now().Format(time.RFC3339),
						},
						"spec": map[string]any{
							"type":        "ai",
							"transaction": map[string]any{"id": "txn-other"},
						},
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
							"name":              "trace-task",
							"namespace":         "default",
							"creationTimestamp": time.Now().Format(time.RFC3339),
						},
						"spec": map[string]any{
							"type":        "ai",
							"transaction": map[string]any{"id": "txn-123"},
						},
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
	root.SetArgs([]string{"audit", "trace", "--server", srv.URL, "txn-123"})

	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if !strings.Contains(stdout, "trace-task") {
		t.Fatalf("stdout = %q, want paginated matching task", stdout)
	}
}
