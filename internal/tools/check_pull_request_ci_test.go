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

	if tool.Name() != checkPullRequestCIToolName {
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
	props := schema[jsonSchemaPropertiesField].(map[string]any)
	for _, field := range []string{taskNameField, repoURLField, githubPRNumberField, "wait_timeout", "poll_interval"} {
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
			name:          passedStatusString,
			checkRunsJSON: `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"success"}]}`,
			wantStatus:    passedStatusString,
			wantPassed:    true,
		},
		{
			name:          failedStatusString,
			checkRunsJSON: `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"failure"}]}`,
			wantStatus:    failedStatusString,
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
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)

			args, _ := json.Marshal(CheckPullRequestCIArgs{
				TaskName: testCoderTaskName,
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
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CheckPullRequestCIArgs{
		TaskName:     testCoderTaskName,
		PRNumber:     42,
		WaitTimeout:  shortPollIntervalString,
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
	if res.Status != passedStatusString {
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
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CheckPullRequestCIArgs{
		TaskName:     testCoderTaskName,
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

// Demo 10 run 2026-06-01 10:50 PT regressed when check_pull_request_ci
// returned a TERMINAL "no_checks" result on a freshly opened PR — the
// coordinator declared success at status=no_checks while GitHub Actions
// had simply not registered any check_run objects yet (workflows take
// 10-60 seconds to be queued on a fresh PR). The real CI then ran and
// posted a lint=FAILURE that the demo wrapper missed.
//
// Fix: when wait_timeout > 0 AND the head SHA has no check_runs, treat
// no_checks as NON-terminal — keep polling until either (a) check runs
// register and resolve to a real status, or (b) the wait_timeout
// elapses. Only at (b) do we report status=no_checks as the final
// answer, distinguishing "wait timed out with no checks registered"
// from "checks registered but didn't finish in time".
//
// Test (a): server returns "no checks yet" twice, then a passing check.
// The tool should keep polling through the empty responses and finally
// report status=success.
func TestCheckPullRequestCITool_NoChecksKeepsPollingUntilChecksRegister(t *testing.T) {
	checkRunsCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			checkRunsCalls++
			w.WriteHeader(http.StatusOK)
			if checkRunsCalls <= 2 {
				// First two calls: no checks registered yet (the fresh-PR window).
				_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
			} else {
				// Third call: passing check_run registered.
				_, _ = fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
			}
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
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CheckPullRequestCIArgs{
		TaskName:     testCoderTaskName,
		PRNumber:     42,
		WaitTimeout:  "200ms",
		PollInterval: "5ms",
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
		t.Errorf("status = %q, want passed (no_checks should have been treated as pending)", res.Status)
	}
	if checkRunsCalls < 3 {
		t.Errorf("check runs calls = %d, want at least 3 (polled through 2 empty responses)", checkRunsCalls)
	}
}

// Test (b): the head SHA never gets check_runs registered. After the
// wait_timeout elapses we should report status=no_checks distinctly
// (not as CI_PENDING) so the coordinator knows this is the terminal
// state, not a transient pending one.
func TestCheckPullRequestCITool_NoChecksTerminalAfterTimeout(t *testing.T) {
	checkRunsCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			checkRunsCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
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
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CheckPullRequestCIArgs{
		TaskName:     testCoderTaskName,
		PRNumber:     42,
		WaitTimeout:  "30ms",
		PollInterval: "5ms",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res CheckPullRequestCIResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Status != "no_checks" {
		t.Errorf("status = %q, want no_checks (terminal after timeout)", res.Status)
	}
	if !res.WaitTimedOut {
		t.Errorf("WaitTimedOut should be true, got: %+v", res)
	}
	if !strings.Contains(res.Message, "no CI checks have been registered") {
		t.Errorf("message = %q, want the no-checks-after-timeout phrasing", res.Message)
	}
	if checkRunsCalls < 2 {
		t.Errorf("check runs calls = %d, want at least 2 (polled multiple times)", checkRunsCalls)
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
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      testSozercanAynaRepoURL,
					Branch:       testBranch,
					GitSecretRef: &corev1.LocalObjectReference{Name: testGitCredsSecretName},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}
	return task, secret
}
