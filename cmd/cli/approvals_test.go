/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
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
