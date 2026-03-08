/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/workers/common"
)

const taskFailRetry = "task-fail-retry"

func TestWaitForTasksTool_Name(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	if got := tool.Name(); got != "wait_for_tasks" {
		t.Errorf("Name() = %v, want %v", got, "wait_for_tasks")
	}
}

func TestWaitForTasksTool_Description(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	if desc := tool.Description(); desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestWaitForTasksTool_Parameters(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestWaitForTasksTool_Execute(t *testing.T) {
	tests := []struct {
		name          string
		tasks         []corev1alpha1.Task
		resultMap     map[string]string // taskName -> result (served via HTTP)
		args          WaitForTasksArgs
		wantCompleted bool
		wantResults   []TaskResultInfo
		wantErr       bool
	}{
		{
			name: "all tasks succeeded",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-a", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-a"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						ResultRef: &corev1alpha1.ResultReference{
							Available: true,
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-b", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-b"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						ResultRef: &corev1alpha1.ResultReference{
							Available: true,
						},
					},
				},
			},
			resultMap: map[string]string{
				"task-a": "result from task-a",
				"task-b": "result from task-b",
			},
			args:          WaitForTasksArgs{Tasks: []string{"task-a", "task-b"}, Timeout: "1s"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: "task-a", Agent: "agent-a", Phase: "Succeeded", Result: "result from task-a"},
				{Task: "task-b", Agent: "agent-b", Phase: "Succeeded", Result: "result from task-b"},
			},
		},
		{
			name: "mixed results",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-ok", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-ok"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						ResultRef: &corev1alpha1.ResultReference{
							Available: true,
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-fail", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-fail"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase:   corev1alpha1.TaskPhaseFailed,
						Message: "error: out of memory",
					},
				},
			},
			resultMap: map[string]string{
				"task-ok": "success output",
			},
			args:          WaitForTasksArgs{Tasks: []string{"task-ok", "task-fail"}, Timeout: "1s"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: "task-ok", Agent: "agent-ok", Phase: "Succeeded", Result: "success output"},
				{Task: "task-fail", Agent: "agent-fail", Phase: "Failed", Result: "error: out of memory"},
			},
		},
		{
			name: "timeout with pending tasks",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-pending", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type: corev1alpha1.TaskTypeAI,
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseRunning,
					},
				},
			},
			args:          WaitForTasksArgs{Tasks: []string{"task-pending"}, Timeout: "100ms"},
			wantCompleted: false,
			wantResults: []TaskResultInfo{
				{Task: "task-pending", Phase: "Running"},
			},
		},
		{
			name:          "missing task",
			tasks:         []corev1alpha1.Task{},
			args:          WaitForTasksArgs{Tasks: []string{"nonexistent"}, Timeout: "100ms"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: "nonexistent", Phase: "Error"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

			// Set up HTTP test server for result fetching
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Extract task name from path: /api/v1/tasks/{taskName}/result
				parts := make([]string, 0)
				for _, p := range splitPath(r.URL.Path) {
					if p != "" {
						parts = append(parts, p)
					}
				}
				// path: api/v1/tasks/{taskName}/result
				if len(parts) >= 5 && parts[3] != "" {
					taskName := parts[3]
					if result, ok := tt.resultMap[taskName]; ok {
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(map[string]string{"result": result}) //nolint:errcheck
						return
					}
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

			scheme := newTestScheme()
			objs := make([]client.Object, 0, len(tt.tasks))
			for i := range tt.tasks {
				objs = append(objs, &tt.tasks[i])
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&corev1alpha1.Task{}).
				Build()

			tool := NewWaitForTasksTool(fakeClient)

			argsJSON, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("failed to marshal args: %v", err)
			}

			result, err := tool.Execute(context.Background(), argsJSON)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			var got WaitForTasksResult
			if err := json.Unmarshal([]byte(result), &got); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}

			if got.Completed != tt.wantCompleted {
				t.Errorf("Completed = %v, want %v", got.Completed, tt.wantCompleted)
			}

			if len(got.Results) != len(tt.wantResults) {
				t.Fatalf("Results length = %d, want %d", len(got.Results), len(tt.wantResults))
			}

			for i, want := range tt.wantResults {
				gotR := got.Results[i]
				if gotR.Task != want.Task {
					t.Errorf("Results[%d].Task = %q, want %q", i, gotR.Task, want.Task)
				}
				if gotR.Agent != want.Agent {
					t.Errorf("Results[%d].Agent = %q, want %q", i, gotR.Agent, want.Agent)
				}
				if gotR.Phase != want.Phase {
					t.Errorf("Results[%d].Phase = %q, want %q", i, gotR.Phase, want.Phase)
				}
				if want.Result != "" && gotR.Result != want.Result {
					t.Errorf("Results[%d].Result = %q, want %q", i, gotR.Result, want.Result)
				}
				// For error cases, just check result is non-empty
				if want.Phase == "Error" && gotR.Result == "" {
					t.Errorf("Results[%d].Result should contain error info", i)
				}
			}
		})
	}
}

// splitPath splits a URL path into non-empty segments.
func splitPath(path string) []string {
	var result []string
	for s := range strings.SplitSeq(path, "/") {
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

func TestWaitForTasksTool_Execute_EmptyTasks(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{"tasks": []}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for empty tasks")
	}
}

func TestWaitForTasksTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{invalid}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}

func TestWaitForTasksTool_Execute_InvalidTimeout(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{"tasks": ["t1"], "timeout": "not-a-duration"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid timeout")
	}
}

func TestWaitForTasksTool_Execute_MissingNamespace(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "")
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{"tasks": ["t1"]}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for missing namespace")
	}
}

func TestWaitForTasksTool_Execute_StructuredResult(t *testing.T) {
	// Create a structured result with diff (which should be stripped)
	sr := common.StructuredResult{
		Version:  1,
		Summary:  "Implemented auth middleware",
		BaseSHA:  "abc123def",
		Diff:     "diff --git a/auth.go b/auth.go\n+package auth\n+// lots of code",
		Verdict:  "APPROVED",
		Feedback: "Looks great!",
		Files:    []string{"auth.go", "middleware.go"},
	}
	srJSON, _ := json.Marshal(sr)

	// Mock server that returns the structured result
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": string(srJSON)}) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task-1",
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelIteration: "2",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: "coder"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)

	args, _ := json.Marshal(WaitForTasksArgs{
		Tasks:   []string{"child-task-1"},
		Timeout: "5s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &waitResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if !waitResult.Completed {
		t.Error("expected completed=true")
	}
	if len(waitResult.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(waitResult.Results))
	}

	r := waitResult.Results[0]

	// Result should contain summary but NOT the diff
	if r.Summary != "Implemented auth middleware" {
		t.Errorf("expected summary, got %q", r.Summary)
	}
	if r.Verdict != "APPROVED" {
		t.Errorf("expected verdict APPROVED, got %q", r.Verdict)
	}
	if r.Feedback != "Looks great!" {
		t.Errorf("expected feedback, got %q", r.Feedback)
	}
	if r.BaseSHA != "abc123def" {
		t.Errorf("expected baseSHA, got %q", r.BaseSHA)
	}
	if r.Iteration != "2" {
		t.Errorf("expected iteration=2, got %q", r.Iteration)
	}
	// Most important: diff should NOT be in the result
	if strings.Contains(r.Result, "diff --git") {
		t.Error("result should NOT contain raw diff")
	}
	if len(r.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(r.Files))
	}
}

func TestWaitForTasksTool_Execute_AutoRetry(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

	// Create a failed task with auto-retry annotations
	failedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskFailRetry,
			Namespace: "test-ns",
			Annotations: map[string]string{
				labels.AnnotationAutoRetry:      "true",
				labels.AnnotationMaxRetries:     "2",
				labels.AnnotationRetryCount:     "0",
				labels.AnnotationOriginalPrompt: "Implement the feature",
			},
			Labels: map[string]string{
				labels.LabelParentTask:     "parent",
				labels.LabelDelegatedAgent: "coder",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "coder"},
			Prompt:   "Implement the feature",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseFailed,
			Message: "out of memory",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(failedTask).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)

	args, _ := json.Marshal(WaitForTasksArgs{
		Tasks:   []string{taskFailRetry},
		Timeout: "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &waitResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// With auto-retry disabled in wait_for_tasks, the task should stay Failed
	// (retry logic is now handled by the coordinator LLM)
	var originalResult *TaskResultInfo
	for i := range waitResult.Results {
		if waitResult.Results[i].Task == taskFailRetry {
			originalResult = &waitResult.Results[i]
			break
		}
	}
	if originalResult == nil {
		t.Fatal("original task not found in results")
	}

	if originalResult.Phase != "Failed" {
		t.Errorf("expected Phase=Failed, got %q", originalResult.Phase)
	}
	if originalResult.FailureDetails == nil {
		t.Fatal("expected FailureDetails to be set")
	}
	if originalResult.FailureDetails.Message != "out of memory" {
		t.Errorf("expected failure message 'out of memory', got %q", originalResult.FailureDetails.Message)
	}
	if originalResult.FailureDetails.RetryCount != 0 {
		t.Errorf("expected retryCount=0, got %d", originalResult.FailureDetails.RetryCount)
	}
	if originalResult.FailureDetails.MaxRetries != 2 {
		t.Errorf("expected maxRetries=2, got %d", originalResult.FailureDetails.MaxRetries)
	}

	// Verify no retry task was created (auto-retry is disabled)
	taskList := &corev1alpha1.TaskList{}
	if err := fakeClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(taskList.Items) != 1 {
		t.Errorf("expected 1 task (original only), got %d", len(taskList.Items))
	}
}

func TestWaitForTasksTool_Execute_AutoRetryExhausted(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

	// Task with retries already exhausted
	failedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-exhausted",
			Namespace: "test-ns",
			Annotations: map[string]string{
				labels.AnnotationAutoRetry:      "true",
				labels.AnnotationMaxRetries:     "2",
				labels.AnnotationRetryCount:     "2",
				labels.AnnotationOriginalPrompt: "Do something",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "coder"},
			Prompt:   "Do something",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseFailed,
			Message: "failed again",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(failedTask).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)

	args, _ := json.Marshal(WaitForTasksArgs{
		Tasks:   []string{"task-exhausted"},
		Timeout: "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &waitResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(waitResult.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(waitResult.Results))
	}

	r := waitResult.Results[0]
	if r.Retried {
		t.Error("expected Retried=false when retries exhausted")
	}
	if r.Phase != "Failed" {
		t.Errorf("expected Phase=Failed, got %q", r.Phase)
	}
	if r.FailureDetails == nil {
		t.Fatal("expected FailureDetails")
	}
	if r.FailureDetails.RetryCount != 2 {
		t.Errorf("expected retryCount=2, got %d", r.FailureDetails.RetryCount)
	}
	if r.FailureDetails.MaxRetries != 2 {
		t.Errorf("expected maxRetries=2, got %d", r.FailureDetails.MaxRetries)
	}

	// No retry task should have been created
	taskList := &corev1alpha1.TaskList{}
	if err := fakeClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(taskList.Items) != 1 {
		t.Errorf("expected 1 task (no retry created), got %d", len(taskList.Items))
	}
}

func TestWaitForTasksTool_Execute_NoAutoRetryOnSuccess(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

	// Succeeded task with auto-retry — should NOT trigger retry
	succeededTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-success",
			Namespace: "test-ns",
			Annotations: map[string]string{
				labels.AnnotationAutoRetry:  "true",
				labels.AnnotationMaxRetries: "2",
				labels.AnnotationRetryCount: "0",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "coder"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseSucceeded,
			Message: "all good",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(succeededTask).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)

	args, _ := json.Marshal(WaitForTasksArgs{
		Tasks:   []string{"task-success"},
		Timeout: "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	_ = json.Unmarshal([]byte(result), &waitResult)

	r := waitResult.Results[0]
	if r.Retried {
		t.Error("should not retry a succeeded task")
	}
	if r.Phase != "Succeeded" {
		t.Errorf("expected Phase=Succeeded, got %q", r.Phase)
	}
}

func TestWaitForTasksTool_Execute_FetchResultNon200(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

	// Server returns 500 for result fetch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-err", Namespace: "test-ns"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "agent-err"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)
	args, _ := json.Marshal(WaitForTasksArgs{Tasks: []string{"task-err"}, Timeout: "1s"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	_ = json.Unmarshal([]byte(result), &waitResult)

	if len(waitResult.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(waitResult.Results))
	}
	if !strings.Contains(waitResult.Results[0].Result, "error reading result") {
		t.Errorf("expected error reading result, got %q", waitResult.Results[0].Result)
	}
}

func TestWaitForTasksTool_Execute_FetchResultInvalidJSON(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

	// Server returns 200 but invalid JSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-valid-json"))
	}))
	defer srv.Close()
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-bad-json", Namespace: "test-ns"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "agent-x"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)
	args, _ := json.Marshal(WaitForTasksArgs{Tasks: []string{"task-bad-json"}, Timeout: "1s"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	_ = json.Unmarshal([]byte(result), &waitResult)

	if len(waitResult.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(waitResult.Results))
	}
	if !strings.Contains(waitResult.Results[0].Result, "error reading result") {
		t.Errorf("expected error reading result, got %q", waitResult.Results[0].Result)
	}
}

func TestWaitForTasksTool_Execute_FallbackToMessage(t *testing.T) {
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ORKA_CONTROLLER_URL", srv.URL)

	// Task with no ResultRef but with a status message
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-msg", Namespace: "test-ns"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "agent-m"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseSucceeded,
			Message: "completed with warnings",
		},
	}

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)
	args, _ := json.Marshal(WaitForTasksArgs{Tasks: []string{"task-msg"}, Timeout: "1s"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	_ = json.Unmarshal([]byte(result), &waitResult)

	if len(waitResult.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(waitResult.Results))
	}
	if waitResult.Results[0].Result != "completed with warnings" {
		t.Errorf("expected message fallback, got %q", waitResult.Results[0].Result)
	}
}
