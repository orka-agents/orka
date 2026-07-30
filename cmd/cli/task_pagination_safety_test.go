/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/cli/client"
)

func TestListFilteredTasksReturnsPartialResultsOnContinuationCycle(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		continuation := r.URL.Query().Get("continue")
		next := "a"
		switch continuation {
		case "a":
			next = "b"
		case "b":
			next = "a"
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{
				"metadata": map[string]any{"name": "task-" + strconv.Itoa(requests)},
				"status":   map[string]any{"phase": "Running"},
			}},
			"metadata": map[string]any{"continue": next},
		})
	}))
	defer srv.Close()

	tasks, truncated, err := listFilteredTasks(
		context.Background(),
		client.New(srv.URL, ""),
		"",
		0,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "continuation cycle detected") {
		t.Fatalf("listFilteredTasks() error = %v, want continuation-cycle error", err)
	}
	if truncated {
		t.Fatal("listFilteredTasks() truncated = true, want false on pagination failure")
	}
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 partial tasks", len(tasks))
	}
}

func TestListFilteredTasksReturnsPartialResultsWhenLaterPageFails(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
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
	}))
	defer srv.Close()

	tasks, truncated, err := listFilteredTasks(
		context.Background(),
		client.New(srv.URL, ""),
		"",
		0,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "after 1 matching items on page 2") {
		t.Fatalf("listFilteredTasks() error = %v, want partial-result page error", err)
	}
	if truncated {
		t.Fatal("listFilteredTasks() truncated = true, want false on page failure")
	}
	if len(tasks) != 1 || tasks[0].Name != "task-1" {
		t.Fatalf("tasks = %#v, want first-page partial result", tasks)
	}
}

func TestNewTaskListCmdHonorsCommandContextCancellation(t *testing.T) {
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
	root.SetArgs([]string{"task", "list", "--server", srv.URL, "--status", "Running"})
	if err := root.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteContext() error = %v, want context canceled", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}
