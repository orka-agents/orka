/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func taskStatusServer(t *testing.T, task map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(task) //nolint:errcheck
	}))
}

func statusRows(out string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")[1:]
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, strings.Fields(line)[0])
	}
	return rows
}

func TestTaskStatusDefaultShowsOnlyWhatAPersonNeeds(t *testing.T) {
	task := map[string]any{
		"metadata": map[string]any{"name": "proxy-78055b2e", "namespace": "orka-system"},
		"spec":     map[string]any{"type": "agent", "workspace": map[string]any{"intent": "write"}},
		"status": map[string]any{
			"phase": "Succeeded",
			"execution": map[string]any{"state": "Succeeded", "outcome": "Succeeded", "attempt": 1,
				"runtimePoolName": "acp-codex-b8fda737b6ec7ac6", "runtimeInstanceID": "cf1f45eb-0172-44a-cc586219", "runtimeSessionGeneration": 2},
			"delivery": map[string]any{"state": "VerifiedExact", "outcome": "VerifiedExact",
				"branch": "orka/add-healthz-endpoint-a7f2b1e9", "verifiedRemoteSHA": "b4cb8efd2c619e6b21e2c6521"},
		},
	}
	srv := taskStatusServer(t, task)
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "task", "status", "proxy-78055b2e")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(statusRows(out), ","); got != "Task,Phase,Delivery,Publication" {
		t.Fatalf("default rows = %s\n%s", got, out)
	}
	for _, want := range []string{"Succeeded", "VerifiedExact", "orka/add-healthz-endpoint-a7f2b1e9"} {
		if !strings.Contains(out, want) {
			t.Errorf("default view missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"acp-codex", "cf1f45eb", "Attempt", "Verified remote", "Namespace"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("default view leaks %q:\n%s", unwanted, out)
		}
	}

	verbose, err := runCLI(t, srv.URL, "task", "status", "proxy-78055b2e", "--verbose")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Namespace", "Execution outcome", "Attempt", "RuntimePool", "acp-codex-b8fda737b6ec7ac6", "Runtime instance", "Runtime session generation", "Verified remote"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("--verbose missing %q:\n%s", want, verbose)
		}
	}

	jsonOut, err := runCLI(t, srv.URL, "task", "status", "proxy-78055b2e", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &status); err != nil {
		t.Fatal(err)
	}
	if nestedString(status, "execution", "runtimePoolName") != "acp-codex-b8fda737b6ec7ac6" || nestedString(status, "delivery", "verifiedRemoteSHA") == "" {
		t.Fatalf("-o json lost fields:\n%s", jsonOut)
	}
}

func TestTaskStatusReadIntentOmitsDeliveryRows(t *testing.T) {
	srv := taskStatusServer(t, map[string]any{
		"metadata": map[string]any{"name": "reader", "namespace": "default"},
		"spec":     map[string]any{"type": "agent", "workspace": map[string]any{"intent": "read"}},
		"status":   map[string]any{"phase": "Running", "execution": map[string]any{"state": "Running", "runtimePoolName": "pool"}},
	})
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "status", "reader")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(statusRows(out), ","); got != "Task,Phase" {
		t.Fatalf("read-intent rows = %s\n%s", got, out)
	}
}

func TestTaskStatusFailedTaskShowsReasonByDefault(t *testing.T) {
	srv := taskStatusServer(t, map[string]any{
		"metadata": map[string]any{"name": "broken", "namespace": "default"},
		"spec":     map[string]any{"type": "agent"},
		"status": map[string]any{
			"phase":     "Failed",
			"execution": map[string]any{"state": "Failed", "outcome": "Failed", "reason": "PromptFailed", "message": "runtime exited with code 2"},
		},
	})
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "status", "broken")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Reason") || !strings.Contains(out, "PromptFailed: runtime exited with code 2") {
		t.Fatalf("failed task reason missing:\n%s", out)
	}
	if got := strings.Join(statusRows(out), ","); got != "Task,Phase,Reason" {
		t.Fatalf("failed rows = %s\n%s", got, out)
	}
}

func TestTaskStatusFailedContainerTaskShowsControllerMessage(t *testing.T) {
	srv := taskStatusServer(t, map[string]any{
		"metadata": map[string]any{"name": "build", "namespace": "default"},
		"spec":     map[string]any{"type": "container", "image": "golang:1.27"},
		"status": map[string]any{
			"phase":            "Failed",
			"message":          "Job has reached the specified backoff limit",
			"executionOutcome": map[string]any{"reason": "BackoffLimitExceeded", "message": "container exited with code 1"},
		},
	})
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "status", "build")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "BackoffLimitExceeded: container exited with code 1") {
		t.Fatalf("container failure detail missing:\n%s", out)
	}
	jsonOut, err := runCLI(t, srv.URL, "task", "status", "build", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOut, "backoff limit") || strings.Contains(jsonOut, "executionOutcome") {
		t.Fatalf("-o json projection changed:\n%s", jsonOut)
	}
}

func TestTaskStatusUnknownOutcomeKeepsReplayPolicyInDefaultView(t *testing.T) {
	srv := taskStatusServer(t, map[string]any{
		"metadata": map[string]any{"name": "uncertain", "namespace": "default"},
		"spec":     map[string]any{"type": "agent", "workspace": map[string]any{"intent": "write"}},
		"status": map[string]any{
			"phase":     "Failed",
			"execution": map[string]any{"state": "OutcomeUnknown", "outcome": "OutcomeUnknown"},
			"delivery":  map[string]any{"state": "PublicationOutcomeUnknown", "branch": "orka/change"},
		},
	})
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "task", "status", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OutcomeUnknown", "PublicationOutcomeUnknown", "orka/change", "No automatic replay"} {
		if !strings.Contains(out, want) {
			t.Errorf("default view missing %q:\n%s", want, out)
		}
	}
}
