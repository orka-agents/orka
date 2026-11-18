/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// eventsServer serves a task event stream with paging by `after`/`limit`
// and `type` filters, the way the API does.
func eventsServer(t *testing.T, all []map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		types := r.URL.Query()["type"]
		var page []map[string]any
		for _, event := range all {
			seq := int64(event["seq"].(int))
			if seq <= after {
				continue
			}
			if len(types) > 0 {
				match := false
				for _, typ := range types {
					if typ == event["type"] {
						match = true
					}
				}
				if !match {
					continue
				}
			}
			if limit > 0 && len(page) >= limit {
				break
			}
			page = append(page, event)
		}
		if page == nil {
			page = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"namespace": "default", "streamType": "task", "streamId": "proxy-1",
			"afterSeq": after, "latestSeq": len(all), "events": page,
		})
	}))
	return srv, &queries
}

func eventFixtures(n int) []map[string]any {
	events := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		typ := "ToolCallCompleted"
		if i%3 == 0 {
			typ = "ModelMessage"
		}
		events = append(events, map[string]any{
			"seq": i, "type": typ, "severity": "info",
			"summary": "event " + strconv.Itoa(i) + " " + strings.Repeat("x", 300),
		})
	}
	return events
}

func TestTaskEventsTailKeepsLastMatchingEvents(t *testing.T) {
	srv, queries := eventsServer(t, eventFixtures(1200))
	defer srv.Close()
	t.Setenv("COLUMNS", "120")

	out, err := runCLI(t, srv.URL, "task", "events", "proxy-1", "--tail", "2")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "1199") || !strings.HasPrefix(lines[2], "1200") {
		t.Fatalf("--tail 2 output:\n%s", out)
	}
	if len(*queries) < 3 {
		t.Fatalf("expected paging through the stream, got %d requests: %v", len(*queries), *queries)
	}

	out, err = runCLI(t, srv.URL, "task", "events", "proxy-1", "--type", "modelmessage", "--tail", "1")
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "1200") || !strings.Contains(lines[1], "ModelMessage") {
		t.Fatalf("--type ModelMessage --tail 1 output:\n%s", out)
	}
	last := (*queries)[len(*queries)-1]
	if !strings.Contains(last, "type=ModelMessage") {
		t.Fatalf("case-insensitive --type was not canonicalised: %q", last)
	}

	jsonOut, err := runCLI(t, srv.URL, "task", "events", "proxy-1", "--tail", "1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &resp); err != nil {
		t.Fatal(err)
	}
	events, _ := resp["events"].([]any)
	if len(events) != 1 || !strings.HasSuffix(anyString(events[0].(map[string]any)["summary"]), "x") {
		t.Fatalf("json tail output is truncated or wrong: %s", jsonOut)
	}
	// The envelope keeps the caller's cursor, not the last page's.
	if int64Field(resp, "afterSeq") != 0 || int64Field(resp, "latestSeq") != 1200 {
		t.Fatalf("tail envelope cursor changed: afterSeq=%v latestSeq=%v", resp["afterSeq"], resp["latestSeq"])
	}
}

func TestSessionEventsCapTheTaskColumn(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	long := strings.Repeat("very-long-task-name-", 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{{ //nolint:errcheck
			"seq": 1, "taskName": long, "taskSeq": 1, "type": "ModelMessage", "severity": "info", "summary": strings.Repeat("y", 200),
		}}})
	}))
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "session", "events", "s1")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if len([]rune(line)) > 100 {
			t.Fatalf("session event row wider than the terminal (%d):\n%s", len([]rune(line)), out)
		}
	}
	if strings.Contains(out, long) {
		t.Fatalf("task column was not capped:\n%s", out)
	}
	wide, err := runCLI(t, srv.URL, "session", "events", "s1", "--wide")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wide, long) {
		t.Fatalf("--wide should print the full task name:\n%s", wide)
	}
}

func TestSessionEventsFitAnEightyColumnTerminalWithLongTypes(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	long := strings.Repeat("very-long-task-name-", 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{{ //nolint:errcheck
			"seq": 1, "taskName": long, "taskSeq": 1, "type": "WorkspacePreparationCompleted", "severity": "warning", "summary": strings.Repeat("y", 200),
		}}})
	}))
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "session", "events", "s1")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if len([]rune(line)) > 80 {
			t.Fatalf("session event row wider than 80 columns (%d):\n%s", len([]rune(line)), out)
		}
	}
	if !strings.Contains(out, "WorkspacePreparationCompleted") {
		t.Fatalf("event type must not be cut:\n%s", out)
	}
}

func TestTaskEventsTableFitsTerminalUnlessWide(t *testing.T) {
	srv, _ := eventsServer(t, eventFixtures(2))
	defer srv.Close()
	t.Setenv("COLUMNS", "100")

	out, err := runCLI(t, srv.URL, "task", "events", "proxy-1")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if len([]rune(line)) > 100 {
			t.Fatalf("row wider than the terminal: %d runes\n%s", len([]rune(line)), out)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("long summary was not cut with an ellipsis:\n%s", out)
	}
	wide, err := runCLI(t, srv.URL, "task", "events", "proxy-1", "--wide")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wide, "…") || !strings.Contains(wide, strings.Repeat("x", 300)) {
		t.Fatalf("--wide cut the summary:\n%s", wide)
	}
}

func TestTaskEventsShowModelMessageContent(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	content := "Added GET /healthz returning 200 with the build version. " + strings.Repeat("Also refreshed the README. ", 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{{ //nolint:errcheck
			"seq": 7, "type": "ModelMessage", "severity": "info", "summary": "model returned message", "contentText": content,
		}}, "latestSeq": 7})
	}))
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "events", "proxy-1", "--type", "ModelMessage", "--tail", "1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "model returned message") || !strings.Contains(out, "Added GET /healthz") || !strings.Contains(out, "…") {
		t.Fatalf("default row should show truncated content:\n%s", out)
	}
	wide, err := runCLI(t, srv.URL, "task", "events", "proxy-1", "--wide")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wide, strings.TrimSpace(content)) {
		t.Fatalf("--wide should print the full content:\n%s", wide)
	}
}

func TestTaskEventsRejectsUnknownTypeAndListsNamesInHelp(t *testing.T) {
	srv, _ := eventsServer(t, nil)
	defer srv.Close()
	_, err := runCLI(t, srv.URL, "task", "events", "proxy-1", "--type", "Message")
	if err == nil || !strings.Contains(err.Error(), "ModelMessage") {
		t.Fatalf("unknown type error = %v", err)
	}
	help, _ := runCLI(t, srv.URL, "task", "events", "--help")
	for _, want := range []string{"ModelMessage", "ToolCallCompleted", "--tail"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}
