/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const approvalFullID = "acp-tool-approval-v2:sha256:8a8d1a7d418dbaf5b21d3472c9bac9881ca6b4963a23afb2f6f3ef9579f7c5e7"

func approvalFixtures(expires time.Time) []map[string]any {
	return []map[string]any{
		{
			"id": approvalFullID, "status": "pending", "action": "Execute create-work-order",
			"riskSummary": "Review this exact operation and its inputs. The tool has not run.",
			"targetTool":  "create-work-order",
			"targetArgsPreview": map[string]any{
				"asset": "pump-1", "summary": "Inspect the pressure transmitter.",
				"notes": strings.Repeat("Long note. ", 40),
			},
			"severity": "warning", "createdAt": expires.Add(-10 * time.Minute).Format(time.RFC3339),
			"expiresAt": expires.Format(time.RFC3339),
		},
		{
			"id":     "acp-tool-approval-v2:sha256:8a8d1b0000000000000000000000000000000000000000000000000000000000",
			"status": "approved", "action": "Execute delete-asset", "targetTool": "delete-asset",
			"targetArgsPreview": map[string]any{"asset": "pump-2"}, "severity": "critical",
			"decisionActor": "alice", "decisionReason": "Verified with ops.", "decisionTime": expires.Format(time.RFC3339),
			"expiresAt": expires.Format(time.RFC3339),
		},
		{
			"id": "legacy-approval-1", "status": "declined", "action": "Publish branch",
			"decisionActor": "bob", "decisionReason": "Not now.",
		},
	}
}

func approvalsServer(t *testing.T, approvals []map[string]any, decided *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/fibey/approvals":
			json.NewEncoder(w).Encode(map[string]any{"approvals": approvals}) //nolint:errcheck
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/fibey/approvals/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/fibey/approvals/"), "/decision")
			if decided != nil {
				*decided = id
			}
			for _, approval := range approvals {
				if approval["id"] == id {
					decided := map[string]any{}
					maps.Copy(decided, approval)
					decided["status"] = "approved"
					decided["decisionActor"] = "cli-user"
					decided["decisionReason"] = "Inspect the transmitter."
					decided["decisionTime"] = time.Now().UTC().Format(time.RFC3339)
					json.NewEncoder(w).Encode(decided) //nolint:errcheck
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestTaskApprovalsTableShowsToolArgumentsAndExpiry(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	srv := approvalsServer(t, approvalFixtures(time.Now().Add(9*time.Minute+30*time.Second)), nil)
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "approvals", "fibey")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "TOOL") || !strings.Contains(lines[0], "ARGUMENTS") || !strings.Contains(lines[0], "EXPIRES") {
		t.Fatalf("header = %q", lines[0])
	}
	if strings.Contains(out, approvalFullID) || !strings.HasPrefix(lines[1], "8a8d1a7d418d") {
		t.Fatalf("ID was not shortened:\n%s", out)
	}
	if !strings.Contains(lines[1], "create-work-order") || !strings.Contains(lines[1], `asset=pump-1`) || !strings.Contains(lines[1], "in 9m") {
		t.Fatalf("pending row:\n%s", out)
	}
	for _, line := range lines {
		if len([]rune(line)) > 120 {
			t.Fatalf("row wider than the terminal:\n%s", out)
		}
	}
	if !strings.Contains(lines[1], "…") {
		t.Fatalf("long arguments were not cut:\n%s", out)
	}
	if strings.Contains(lines[2], "in ") || !strings.Contains(lines[2], "delete-asset") {
		t.Fatalf("decided row still shows an expiry:\n%s", out)
	}
	if !strings.Contains(lines[3], "Publish branch") {
		t.Fatalf("non-tool approval should show its action:\n%s", out)
	}
	if strings.Contains(out, "Review this exact operation") || strings.Contains(out, "alice") {
		t.Fatalf("risk and decision belong to --wide only:\n%s", out)
	}

	wide, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--wide")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SEVERITY", "RISK", "DECIDED BY", "REASON", "critical", "alice", "Verified with ops.", "Review this exact operation"} {
		if !strings.Contains(wide, want) {
			t.Errorf("--wide missing %q:\n%s", want, wide)
		}
	}
}

func TestTaskApprovalsSingleViewPrintsEveryArgument(t *testing.T) {
	approvals := approvalFixtures(time.Now().Add(9 * time.Minute))
	srv := approvalsServer(t, approvals, nil)
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "8a8d1a7d")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID:", approvalFullID, "Status:", "pending", "Tool:", "create-work-order", "Arguments:", "  asset:", "pump-1", "  summary:", "Inspect the pressure transmitter.", "  notes:", "Expires:", "in "} {
		if !strings.Contains(out, want) {
			t.Errorf("single view missing %q:\n%s", want, out)
		}
	}
	jsonOut, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.MarshalIndent(map[string]any{"approvals": approvals}, "", "  ")
	if jsonOut != string(expected)+"\n" {
		t.Fatalf("-o json changed:\n%s", jsonOut)
	}
}

func TestResolveApprovalMatchesPrefixes(t *testing.T) {
	list := map[string]any{"approvals": toAnySlice(approvalFixtures(time.Now()))}
	for _, arg := range []string{"8a8d1a7d418d", "8a8d1a7d", "acp-tool-approval-v2:sha256:8a8d1a7d", approvalFullID} {
		got, err := resolveApproval(list, arg)
		if err != nil || firstString(got, "id") != approvalFullID {
			t.Fatalf("resolveApproval(%q) = %v, %v", arg, got, err)
		}
	}
	if got, err := resolveApproval(list, "legacy"); err != nil || firstString(got, "id") != "legacy-approval-1" {
		t.Fatalf("resolveApproval(legacy) = %v, %v", got, err)
	}
	_, err := resolveApproval(list, "8a8d1")
	if err == nil || !strings.Contains(err.Error(), "matches 2 approvals") || !strings.Contains(err.Error(), "create-work-order") || !strings.Contains(err.Error(), "delete-asset") {
		t.Fatalf("ambiguous prefix error = %v", err)
	}
	if _, err := resolveApproval(list, "zzz"); err == nil || !strings.Contains(err.Error(), "no approval matches") {
		t.Fatalf("no match error = %v", err)
	}
}

func toAnySlice(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func TestTaskApproveWithFullIDPostsDirectly(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			// A least-privilege approver may not read the task at all.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": approvalFullID, "status": "approved", "decisionActor": "approver"}) //nolint:errcheck
	}))
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "approve", "fibey", approvalFullID)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(requests) != 1 || !strings.HasPrefix(requests[0], "POST ") {
		t.Fatalf("full ID should post directly, requests = %v", requests)
	}
	if !strings.Contains(out, "approved") {
		t.Fatalf("decision output:\n%s", out)
	}
}

func TestTaskApproveSendsFullIDForPrefixAndPrintsDecision(t *testing.T) {
	var decided string
	srv := approvalsServer(t, approvalFixtures(time.Now().Add(9*time.Minute)), &decided)
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "approve", "fibey", "8a8d1a7d418d", "--reason", "Inspect the transmitter.")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if decided != approvalFullID {
		t.Fatalf("decision sent to %q", decided)
	}
	for _, want := range []string{"Status:", "approved", "Decided by:", "cli-user", "Reason:", "Inspect the transmitter.", "Tool:", "create-work-order"} {
		if !strings.Contains(out, want) {
			t.Errorf("decision view missing %q:\n%s", want, out)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("decision printed JSON by default:\n%s", out)
	}
	if _, err := runCLI(t, srv.URL, "task", "decline", "fibey", "8a8d1"); err == nil || !strings.Contains(err.Error(), "matches 2 approvals") {
		t.Fatalf("ambiguous decline error = %v", err)
	}
	if _, err := runCLI(t, srv.URL, "task", "decline", "fibey", "nope"); err == nil || !strings.Contains(err.Error(), "no approval matches") {
		t.Fatalf("unmatched decline error = %v", err)
	}
	jsonOut, err := runCLI(t, srv.URL, "task", "approve", "fibey", "8a8d1a7d418d", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var decision map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &decision); err != nil || decision["id"] != approvalFullID {
		t.Fatalf("-o json decision = %s (%v)", jsonOut, err)
	}
}

// approvalsWatchServer serves the approvals list from a function of the call
// count, with the task's phase in the response the way the server reports
// it, so a test can script the moment a request appears. The task detail
// route answers with the same phase and counts its calls, for the fallback
// a server without taskPhase needs.
func approvalsWatchServer(t *testing.T, phase string, approvals func(call int) []map[string]any) *httptest.Server {
	t.Helper()
	return approvalsWatchServerWith(t, phase, true, approvals, nil)
}

func approvalsWatchServerWith(t *testing.T, phase string, includePhase bool, approvals func(call int) []map[string]any, taskReads *int) *httptest.Server {
	t.Helper()
	return approvalsWatchServerFull(t, phase, false, includePhase, approvals, taskReads)
}

func approvalsWatchServerFull(t *testing.T, phase string, deleting, includePhase bool, approvals func(call int) []map[string]any, taskReads *int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/fibey/approvals":
			mu.Lock()
			calls++
			items := approvals(calls)
			mu.Unlock()
			body := map[string]any{"namespace": "default", "taskName": "fibey", "taskUID": "uid-1", "approvals": items}
			if includePhase {
				body["taskPhase"] = phase
				if deleting {
					body["taskDeleting"] = true
				}
			}
			json.NewEncoder(w).Encode(body) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/fibey":
			mu.Lock()
			if taskReads != nil {
				*taskReads++
			}
			mu.Unlock()
			metadata := map[string]any{"name": "fibey", "uid": "uid-1"}
			if deleting {
				metadata["deletionTimestamp"] = "2026-09-23T20:00:00Z"
			}
			json.NewEncoder(w).Encode(map[string]any{"metadata": metadata, "status": map[string]any{"phase": phase}}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// runCLISplit is runCLI with stdout and stderr captured separately.
func runCLISplit(t *testing.T, serverURL string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--server", serverURL, "--token", "test-token", "--namespace", "default"}, args...))
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestTaskApprovalsWatchPrintsTheFirstPendingRequest(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	pending := approvalFixtures(time.Now().Add(9 * time.Minute))[:1]
	srv := approvalsWatchServer(t, "Running", func(call int) []map[string]any {
		if call < 3 {
			return nil
		}
		return pending
	})
	defer srv.Close()

	out, errOut, err := runCLISplit(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(errOut, "Waiting for an approval request...") != 1 {
		t.Fatalf("waiting notice should be printed once on stderr, got %q", errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "create-work-order") || !strings.Contains(lines[1], "pending") {
		t.Fatalf("watch should print the table once it has a pending row:\n%s", out)
	}
	if strings.Contains(out, "---") || strings.Contains(out, "Waiting") {
		t.Fatalf("stdout should hold only the table:\n%s", out)
	}
}

func TestTaskApprovalsWatchIgnoresDecidedRequests(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	fixtures := approvalFixtures(time.Now().Add(9 * time.Minute))
	srv := approvalsWatchServer(t, "Running", func(call int) []map[string]any {
		if call < 2 {
			return fixtures[1:] // approved and declined only
		}
		return fixtures
	})
	defer srv.Close()

	out, errOut, err := runCLISplit(t, srv.URL, "task", "approvals", "fibey", "-w", "--interval", "10ms")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "Waiting for an approval request...") {
		t.Fatalf("decided requests alone should keep waiting, stderr = %q", errOut)
	}
	if !strings.Contains(out, "create-work-order") || !strings.Contains(out, "delete-asset") {
		t.Fatalf("the full list is printed once a request is pending:\n%s", out)
	}
}

func TestTaskApprovalsWatchJSONPrintsOneDocument(t *testing.T) {
	pending := approvalFixtures(time.Now().Add(9 * time.Minute))[:1]
	srv := approvalsWatchServer(t, "Running", func(call int) []map[string]any {
		if call < 2 {
			return nil
		}
		return pending
	})
	defer srv.Close()

	out, _, err := runCLISplit(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if items, _ := doc["approvals"].([]any); len(items) != 1 {
		t.Fatalf("approvals = %v", doc["approvals"])
	}
}

func TestTaskApprovalsWatchFailsWhenTheTaskFinishesFirst(t *testing.T) {
	srv := approvalsWatchServer(t, "Succeeded", func(int) []map[string]any { return nil })
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
	if err == nil || !strings.Contains(err.Error(), "Succeeded") || !strings.Contains(err.Error(), "fibey") {
		t.Fatalf("error = %v\n%s", err, out)
	}
	if strings.Contains(out, "ID ") {
		t.Fatalf("no table should be printed:\n%s", out)
	}
}

func TestTaskApprovalsWatchRejectsAPendingRequestOnAFinishedTask(t *testing.T) {
	pending := approvalFixtures(time.Now().Add(9 * time.Minute))[:1]
	srv := approvalsWatchServer(t, "Cancelled", func(int) []map[string]any { return pending })
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
	if err == nil || !strings.Contains(err.Error(), "Cancelled") {
		t.Fatalf("a request the server will refuse to decide must not end the wait: err = %v\n%s", err, out)
	}
	if strings.Contains(out, "create-work-order") {
		t.Fatalf("no table should be printed:\n%s", out)
	}
}

func TestTaskApprovalsWatchReadsThePhaseFromTheApprovalsResponse(t *testing.T) {
	pending := approvalFixtures(time.Now().Add(9 * time.Minute))[:1]
	taskReads := 0
	srv := approvalsWatchServerWith(t, "Running", true, func(call int) []map[string]any {
		if call < 2 {
			return nil
		}
		return pending
	}, &taskReads)
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
	if err != nil {
		t.Fatal(err)
	}
	if taskReads != 0 {
		t.Fatalf("the phase in the approvals response should make a separate Task read unnecessary, got %d reads", taskReads)
	}
	if !strings.Contains(out, "create-work-order") {
		t.Fatalf("table missing:\n%s", out)
	}
}

func TestTaskApprovalsWatchFallsBackToATaskReadWithoutTaskPhase(t *testing.T) {
	pending := approvalFixtures(time.Now().Add(9 * time.Minute))[:1]
	taskReads := 0
	srv := approvalsWatchServerWith(t, "Failed", false, func(int) []map[string]any { return pending }, &taskReads)
	defer srv.Close()

	_, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("error = %v", err)
	}
	if taskReads == 0 {
		t.Fatal("an older server without taskPhase should be asked for the Task")
	}
}

func TestTaskApprovalsWatchRejectsAPendingRequestOnADeletingTask(t *testing.T) {
	pending := approvalFixtures(time.Now().Add(9 * time.Minute))[:1]
	for name, includePhase := range map[string]bool{"from the approvals response": true, "from a Task read": false} {
		t.Run(name, func(t *testing.T) {
			srv := approvalsWatchServerFull(t, "Running", true, includePhase, func(int) []map[string]any { return pending }, nil)
			defer srv.Close()

			out, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
			if err == nil || !strings.Contains(err.Error(), "being deleted") {
				t.Fatalf("a request the server refuses with 410 must not end the wait: err = %v\n%s", err, out)
			}
			if strings.Contains(out, "create-work-order") {
				t.Fatalf("no table should be printed:\n%s", out)
			}
		})
	}
	t.Run("a finished task names its phase even while deleting", func(t *testing.T) {
		srv := approvalsWatchServerFull(t, "Succeeded", true, true, func(int) []map[string]any { return pending }, nil)
		defer srv.Close()

		_, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms")
		if err == nil || !strings.Contains(err.Error(), "Succeeded") || strings.Contains(err.Error(), "being deleted") {
			t.Fatalf("error = %v, want the terminal phase", err)
		}
	})
}

func TestTaskApprovalsWatchHonoursTimeout(t *testing.T) {
	srv := approvalsWatchServer(t, "Running", func(int) []map[string]any { return nil })
	defer srv.Close()

	_, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--interval", "10ms", "--timeout", "60ms")
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for task fibey") {
		t.Fatalf("error = %v", err)
	}
	if _, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "--watch", "--timeout", "soon"); err == nil || !strings.Contains(err.Error(), "invalid timeout") {
		t.Fatalf("error = %v", err)
	}
}

func TestTaskApprovalsWatchRejectsAnID(t *testing.T) {
	srv := approvalsWatchServer(t, "Running", func(int) []map[string]any { return nil })
	defer srv.Close()

	_, err := runCLI(t, srv.URL, "task", "approvals", "fibey", "8a8d1a7d", "--watch")
	if err == nil || !strings.Contains(err.Error(), "--watch") {
		t.Fatalf("error = %v", err)
	}
}
