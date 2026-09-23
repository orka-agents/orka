/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/cli/client"
)

func taskListFixture(name, typ, phase, agent, image, created string) map[string]any {
	spec := map[string]any{"type": typ}
	if agent != "" {
		spec["agentRef"] = map[string]any{"name": agent}
	}
	if image != "" {
		spec["image"] = image
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "default", "creationTimestamp": created},
		"spec":     spec,
		"status":   map[string]any{"phase": phase},
	}
}

func TestTaskListShowsAgentColumnAndOrdersByCreation(t *testing.T) {
	now := time.Now().UTC()
	items := []map[string]any{
		taskListFixture("proxy-newest", "agent", "Running", "coder", "", now.Add(-10*time.Second).Format(time.RFC3339)),
		taskListFixture("proxy-oldest", "ai", "Succeeded", "support", "", now.Add(-2*time.Hour).Format(time.RFC3339)),
		taskListFixture("proxy-check", "container", "Failed", "", "registry.example.com/team/golang:1.27", now.Add(-time.Minute).Format(time.RFC3339)),
	}
	var selectors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectors = append(selectors, r.URL.Query().Get("labelSelector"))
		json.NewEncoder(w).Encode(map[string]any{"items": items}) //nolint:errcheck
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "AGENT") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "proxy-oldest") || !strings.Contains(lines[2], "proxy-check") || !strings.Contains(lines[3], "proxy-newest") {
		t.Fatalf("rows are not oldest-first:\n%s", out)
	}
	if !strings.Contains(lines[1], "support") || !strings.Contains(lines[2], "golang:1.27") || !strings.Contains(lines[3], "coder") {
		t.Fatalf("AGENT column wrong:\n%s", out)
	}
	if strings.Contains(out, "registry.example.com") {
		t.Fatalf("image was not shortened:\n%s", out)
	}
	if selectors[0] != "" {
		t.Fatalf("labelSelector sent without -l: %q", selectors[0])
	}

	jsonOut, err := runCLI(t, srv.URL, "task", "list", "-l", "orka.ai/source=anthropic-proxy", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if selectors[len(selectors)-1] != "orka.ai/source=anthropic-proxy" {
		t.Fatalf("labelSelector query = %q", selectors[len(selectors)-1])
	}
	var summaries []client.TaskSummary
	if err := json.Unmarshal([]byte(jsonOut), &summaries); err != nil {
		t.Fatalf("json output: %v\n%s", err, jsonOut)
	}
	// JSON keeps the API order and carries the agent reference and image.
	if summaries[0].Name != "proxy-newest" || summaries[0].Agent != "coder" {
		t.Fatalf("json summaries = %+v", summaries)
	}
	if strings.Contains(jsonOut, "\"image\"") {
		t.Fatalf("json output gained an image field:\n%s", jsonOut)
	}
}

func TestTaskListSinceFiltersByCreationTime(t *testing.T) {
	now := time.Now().UTC()
	items := []map[string]any{
		taskListFixture("recent", "agent", "Running", "coder", "", now.Add(-2*time.Minute).Format(time.RFC3339)),
		taskListFixture("old", "agent", "Succeeded", "coder", "", now.Add(-3*time.Hour).Format(time.RFC3339)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("labelSelector"); got != "orka.ai/source=anthropic-proxy" {
			t.Errorf("labelSelector = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items}) //nolint:errcheck
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "list", "-l", "orka.ai/source=anthropic-proxy", "--since", "10m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "recent") || strings.Contains(out, "old") {
		t.Fatalf("--since 10m output:\n%s", out)
	}
	stamp := now.Add(-time.Hour).Format(time.RFC3339)
	out, err = runCLI(t, srv.URL, "task", "list", "-l", "orka.ai/source=anthropic-proxy", "--since", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "recent") || strings.Contains(out, "old") {
		t.Fatalf("--since %s output:\n%s", stamp, out)
	}
	if _, err := runCLI(t, srv.URL, "task", "list", "--since", "later"); err == nil {
		t.Fatal("invalid --since accepted")
	}
}

func TestTaskListRejectsInvalidSelectorFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid labelSelector: unable to parse requirement"}) //nolint:errcheck
	}))
	defer srv.Close()
	_, err := runCLI(t, srv.URL, "task", "list", "-l", "a==b==c")
	if err == nil || !strings.Contains(err.Error(), "invalid labelSelector") {
		t.Fatalf("error = %v", err)
	}
}

func TestWatchLoopReprintsOnlyWhenTheFrameChanges(t *testing.T) {
	frames := []string{"A\n", "A\n", "B\n", "B\n"}
	var out strings.Builder
	calls := 0
	err := watchLoop(context.Background(), &out, time.Millisecond, func(context.Context) (string, bool, error) {
		frame := frames[calls]
		calls++
		return frame, calls == len(frames), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Count(text, "A\n") != 1 || strings.Count(text, "B\n") != 1 {
		t.Fatalf("frames reprinted without a change:\n%s", text)
	}
	if strings.Count(text, "\n--- ") != 1 {
		t.Fatalf("expected one timestamp separator:\n%s", text)
	}
}

func TestWatchLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	calls := 0
	err := watchLoop(ctx, &out, time.Millisecond, func(context.Context) (string, bool, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return "frame\n", false, nil
	})
	if err != nil {
		t.Fatalf("cancel is not an error: %v", err)
	}
}

func TestRenderTaskListTableChangesWhenPhaseChanges(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	before := renderTaskListTable([]client.TaskSummary{{Name: "t1", Type: "agent", Phase: "Running", Agent: "coder", Age: created}})
	after := renderTaskListTable([]client.TaskSummary{{Name: "t1", Type: "agent", Phase: "Succeeded", Agent: "coder", Age: created}})
	if before == after {
		t.Fatal("phase change did not change the rendered table")
	}
	added := renderTaskListTable([]client.TaskSummary{
		{Name: "t1", Type: "agent", Phase: "Running", Agent: "coder", Age: created},
		{Name: "t2", Type: "container", Phase: "Pending", Image: "busybox", Age: created},
	})
	if added == before {
		t.Fatal("a new task did not change the rendered table")
	}
}
