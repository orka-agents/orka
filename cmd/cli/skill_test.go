/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewSkillListCmdListsAllPagesAndForwardsNamespace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		namespace    = "inventory-ns"
		continuation = "skill-cursor+/=? segment"
	)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("namespace"); got != namespace {
			t.Errorf("namespace query = %q, want %q", got, namespace)
		}
		if requests == 1 {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    []map[string]any{{"name": "skill-1", "phase": "Ready"}},
				"metadata": map[string]any{"continue": continuation},
			})
			return
		}
		if got := r.URL.Query().Get("continue"); got != continuation {
			t.Errorf("continue query = %q, want %q", got, continuation)
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items":    []map[string]any{{"name": "skill-2", "phase": "Ready"}},
			"metadata": map[string]any{},
		})
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"skill", "list", "--server", srv.URL, "--namespace", namespace})
	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if !strings.Contains(stdout, "skill-1") || !strings.Contains(stdout, "skill-2") {
		t.Fatalf("stdout = %q, want skills from both pages", stdout)
	}
}

func TestNewSkillListCmdHonorsCommandContextCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}}) //nolint:errcheck
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := newRootCmd()
	root.SetArgs([]string{"skill", "list", "--server", srv.URL})
	if err := root.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteContext() error = %v, want context canceled", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}
