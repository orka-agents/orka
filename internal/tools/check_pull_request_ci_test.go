/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	checkPullRequestCITestSHA       = "abc123"
	checkPullRequestCIStatusPending = "pending"
)

func TestCheckPullRequestCITool_Metadata(t *testing.T) {
	tool := NewCheckPullRequestCITool(newFakeClient())

	if tool.Name() != "check_pull_request_ci" {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if !strings.Contains(tool.Description(), "Check GitHub CI status") {
		t.Errorf("unexpected description: %s", tool.Description())
	}

	params := tool.Parameters()
	if len(params) == 0 {
		t.Fatal("parameters should not be empty")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters: %v", err)
	}
	props := schema["properties"].(map[string]any)
	for _, field := range []string{"task_name", "repo_url", "pr_number", "wait_timeout", "poll_interval"} {
		if _, ok := props[field]; !ok {
			t.Errorf("parameters should contain %s", field)
		}
	}
}

func TestCheckPullRequestCITool_CheckStatuses(t *testing.T) {
	tests := []struct {
		name          string
		checkRunsJSON string
		wantStatus    string
		wantPassed    bool
		wantFailed    bool
		wantPending   bool
		wantDetails   string
	}{
		{
			name:          "passed",
			checkRunsJSON: `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"success"}]}`,
			wantStatus:    "passed",
			wantPassed:    true,
		},
		{
			name:          "failed",
			checkRunsJSON: `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"failure"}]}`,
			wantStatus:    "failed",
			wantFailed:    true,
			wantDetails:   "lint",
		},
		{
			name:          checkPullRequestCIStatusPending,
			checkRunsJSON: `{"total_count":1,"check_runs":[{"name":"build","status":"in_progress","conclusion":""}]}`,
			wantStatus:    checkPullRequestCIStatusPending,
			wantPending:   true,
			wantDetails:   "build",
		},
		{
			name:          "no checks",
			checkRunsJSON: `{"total_count":0,"check_runs":[]}`,
			wantStatus:    "no_checks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if auth := r.Header.Get("Authorization"); auth != testBearerToken {
					t.Errorf("unexpected auth header: %s", auth)
				}
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprint(w, tt.checkRunsJSON)
				case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
					t.Errorf("check_pull_request_ci must not call merge API")
					w.WriteHeader(http.StatusInternalServerError)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			task, secret := checkPullRequestCITestObjects()
			tool := &CheckPullRequestCITool{
				k8sClient:  newFakeClient(task, secret),
				apiBaseURL: server.URL,
			}
			t.Setenv("ORKA_TASK_NAMESPACE", "default")

			args, _ := json.Marshal(CheckPullRequestCIArgs{
				TaskName: "coder-task",
				PRNumber: 42,
			})

			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res CheckPullRequestCIResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", res.Status, tt.wantStatus)
			}
			if res.PRNumber != 42 {
				t.Errorf("pr_number = %d, want 42", res.PRNumber)
			}
			if res.HeadSHA != checkPullRequestCITestSHA {
				t.Errorf("head_sha = %q, want %s", res.HeadSHA, checkPullRequestCITestSHA)
			}
			if res.ChecksPassed != tt.wantPassed {
				t.Errorf("checks_passed = %t, want %t", res.ChecksPassed, tt.wantPassed)
			}
			if res.ChecksFailed != tt.wantFailed {
				t.Errorf("checks_failed = %t, want %t", res.ChecksFailed, tt.wantFailed)
			}
			if res.ChecksPending != tt.wantPending {
				t.Errorf("checks_pending = %t, want %t", res.ChecksPending, tt.wantPending)
			}
			if tt.wantDetails != "" && !strings.Contains(res.ChecksDetails, tt.wantDetails) {
				t.Errorf("checks_details = %q, want to contain %q", res.ChecksDetails, tt.wantDetails)
			}
		})
	}
}

func TestCheckPullRequestCITool_WaitsUntilChecksPass(t *testing.T) {
	checkRunsCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			checkRunsCalls++
			w.WriteHeader(http.StatusOK)
			if checkRunsCalls == 1 {
				_, _ = fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"in_progress","conclusion":""}]}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	task, secret := checkPullRequestCITestObjects()
	tool := &CheckPullRequestCITool{
		k8sClient:  newFakeClient(task, secret),
		apiBaseURL: server.URL,
	}
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CheckPullRequestCIArgs{
		TaskName:     "coder-task",
		PRNumber:     42,
		WaitTimeout:  "100ms",
		PollInterval: "1ms",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res CheckPullRequestCIResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Status != "passed" {
		t.Errorf("status = %q, want passed", res.Status)
	}
	if !res.ChecksPassed || res.ChecksPending || res.WaitTimedOut {
		t.Errorf("unexpected result flags: %+v", res)
	}
	if checkRunsCalls < 2 {
		t.Errorf("check runs calls = %d, want at least 2", checkRunsCalls)
	}
}

func TestCheckPullRequestCITool_WaitTimeoutPending(t *testing.T) {
	checkRunsCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			checkRunsCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"queued","conclusion":""}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	task, secret := checkPullRequestCITestObjects()
	tool := &CheckPullRequestCITool{
		k8sClient:  newFakeClient(task, secret),
		apiBaseURL: server.URL,
	}
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CheckPullRequestCIArgs{
		TaskName:     "coder-task",
		PRNumber:     42,
		WaitTimeout:  "5ms",
		PollInterval: "1ms",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res CheckPullRequestCIResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Status != checkPullRequestCIStatusPending {
		t.Errorf("status = %q, want pending", res.Status)
	}
	if !res.ChecksPending || !res.WaitTimedOut {
		t.Errorf("unexpected result flags: %+v", res)
	}
	if !strings.Contains(res.Message, "CI_PENDING") {
		t.Errorf("message = %q, want CI_PENDING", res.Message)
	}
	if checkRunsCalls < 1 {
		t.Errorf("check runs calls = %d, want at least 1", checkRunsCalls)
	}
}

func TestCheckPullRequestCITool_InvalidArgs(t *testing.T) {
	tool := NewCheckPullRequestCITool(newFakeClient())

	_, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "pr_number is required") {
		t.Errorf("unexpected error: %s", err)
	}
}

func checkPullRequestCITestObjects() (*corev1alpha1.Task, *corev1.Secret) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/sozercan/ayna",
					Branch:       "main",
					GitSecretRef: &corev1.LocalObjectReference{Name: "git-creds"},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	return task, secret
}
