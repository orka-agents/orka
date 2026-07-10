/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/workers/common"
)

const taskFailRetry = "task-fail-retry"

func TestWaitForTasksTool_Name(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	if got := tool.Name(); got != waitForTasksToolName {
		t.Errorf("Name() = %v, want %v", got, waitForTasksToolName)
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

	var paramsSchema map[string]any
	if err := json.Unmarshal(params, &paramsSchema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if paramsSchema[jsonSchemaTypeField] != typeObject {
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
					ObjectMeta: metav1.ObjectMeta{Name: testTaskAName, Namespace: testNamespace},
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
					ObjectMeta: metav1.ObjectMeta{Name: testTaskBName, Namespace: testNamespace},
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
			resultMap:     map[string]string{testTaskAName: "result from task-a", testTaskBName: "result from task-b"},
			args:          WaitForTasksArgs{Tasks: []string{testTaskAName, testTaskBName}, Timeout: "1s"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: testTaskAName, Agent: "agent-a", Phase: taskPhaseSucceededString, Result: "result from task-a"},
				{Task: testTaskBName, Agent: "agent-b", Phase: taskPhaseSucceededString, Result: "result from task-b"},
			},
		},
		{
			name: "mixed results",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: testTaskOKName, Namespace: testNamespace},
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
					ObjectMeta: metav1.ObjectMeta{Name: testTaskFailName, Namespace: testNamespace},
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
			resultMap:     map[string]string{testTaskOKName: "success output"},
			args:          WaitForTasksArgs{Tasks: []string{testTaskOKName, testTaskFailName}, Timeout: "1s"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: testTaskOKName, Agent: "agent-ok", Phase: taskPhaseSucceededString, Result: "success output"},
				{Task: testTaskFailName, Agent: "agent-fail", Phase: taskPhaseFailedString, Result: "error: out of memory"},
			},
		},
		{
			name: "timeout with pending tasks",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: testTaskPendingName, Namespace: testNamespace},
					Spec: corev1alpha1.TaskSpec{
						Type: corev1alpha1.TaskTypeAI,
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseRunning,
					},
				},
			},
			args:          WaitForTasksArgs{Tasks: []string{testTaskPendingName}, Timeout: shortPollIntervalString},
			wantCompleted: false,
			wantResults: []TaskResultInfo{
				{Task: testTaskPendingName, Phase: taskPhaseRunningString},
			},
		},
		{
			name:          "missing task remains incomplete through timeout",
			tasks:         []corev1alpha1.Task{},
			args:          WaitForTasksArgs{Tasks: []string{testNonexistentName}, Timeout: shortPollIntervalString},
			wantCompleted: false,
			wantResults: []TaskResultInfo{
				{Task: testNonexistentName, Phase: taskPhaseErrorString},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envOrkaTaskNamespace, testNamespace)

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
						json.NewEncoder(w).Encode(map[string]string{resultField: result}) //nolint:errcheck
						return
					}
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			t.Setenv(envOrkaControllerURL, srv.URL)

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
				if want.Phase == taskPhaseErrorString && gotR.Result == "" {
					t.Errorf("Results[%d].Result should contain error info", i)
				}
			}
		})
	}
}

func TestWaitForTasksTool_Execute_TransientGetErrorTimesOutIncomplete(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	transientErr := errors.New("temporary Kubernetes read failure")
	getCalls := 0
	fakeClient := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			getCalls++
			return transientErr
		},
	})

	result, err := NewWaitForTasksTool(fakeClient).Execute(
		context.Background(),
		json.RawMessage(`{"tasks":["temporarily-unreadable"],"timeout":"100ms"}`),
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if got.Completed {
		t.Fatal("Completed = true after transient Get errors, want false")
	}
	if getCalls < 2 {
		t.Fatalf("Get calls = %d, want at least 2 attempts before timeout", getCalls)
	}
	if len(got.Results) != 1 || got.Results[0].Phase != taskPhaseErrorString {
		t.Fatalf("Results = %#v, want one Error result", got.Results)
	}
	if !strings.Contains(got.Results[0].Result, transientErr.Error()) {
		t.Fatalf("Result = %q, want transient error details", got.Results[0].Result)
	}
}

func TestWaitForTasksTool_Execute_RetriesNotFoundCacheMissUntilRecovery(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "eventually-visible", Namespace: testNamespace},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	getCalls := 0
	fakeClient := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			if getCalls == 1 {
				return apierrors.NewNotFound(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, task)

	result, err := NewWaitForTasksTool(fakeClient).Execute(
		context.Background(),
		json.RawMessage(`{"tasks":["eventually-visible"],"timeout":"100ms"}`),
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !got.Completed {
		t.Fatalf("Completed = false after NotFound cache miss recovered, result = %#v", got)
	}
	if getCalls < 2 {
		t.Fatalf("Get calls = %d, want a retry after NotFound", getCalls)
	}
	if len(got.Results) != 1 || got.Results[0].Phase != taskPhaseSucceededString {
		t.Fatalf("Results = %#v, want eventually visible task to succeed", got.Results)
	}
}

func TestWaitForTasksTool_Execute_PermanentGetErrorCompletesWithError(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	getCalls := 0
	fakeClient := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			getCalls++
			return apierrors.NewForbidden(
				schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"},
				key.Name,
				errors.New("access denied"),
			)
		},
	})

	result, err := NewWaitForTasksTool(fakeClient).Execute(
		context.Background(),
		json.RawMessage(`{"tasks":["forbidden-task"],"timeout":"1m"}`),
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !got.Completed {
		t.Fatalf("Completed = false for permanent Get error, result = %#v", got)
	}
	if getCalls != 1 {
		t.Fatalf("Get calls = %d, want no retries for permanent error", getCalls)
	}
	if len(got.Results) != 1 || got.Results[0].Phase != taskPhaseErrorString {
		t.Fatalf("Results = %#v, want one Error result", got.Results)
	}
}

func TestWaitForTasksTool_Execute_HonorsCanceledContextDuringGetErrors(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	fakeClient := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errors.New("temporary Kubernetes read failure")
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := NewWaitForTasksTool(fakeClient).Execute(
		ctx,
		json.RawMessage(`{"tasks":["temporarily-unreadable"],"timeout":"1m"}`),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
	if result != "" {
		t.Fatalf("Execute() result = %q, want empty result on cancellation", result)
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
	args := json.RawMessage(invalidJSONText)
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

func TestWaitForTasksTool_Execute_TruncatesLongStructuredSummary(t *testing.T) {
	longSummary := strings.Repeat("x", maxWaitTaskSummaryChars+128)
	sr := common.StructuredResult{Version: 1, Summary: longSummary}
	srJSON, _ := json.Marshal(sr)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{resultField: string(srJSON)}) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaControllerURL, server.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "long-summary", Namespace: defaultNamespace},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()

	tool := NewWaitForTasksTool(fakeClient)
	args, _ := json.Marshal(WaitForTasksArgs{Tasks: []string{"long-summary"}, Timeout: "5s"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var waitResult WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &waitResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	got := waitResult.Results[0].Summary
	if len(got) >= len(longSummary) {
		t.Fatalf("summary was not truncated: got %d want less than %d", len(got), len(longSummary))
	}
	if !strings.Contains(got, "summary truncated") {
		t.Fatalf("summary missing truncation marker: %q", got)
	}
	if waitResult.Results[0].Result != got {
		t.Fatalf("result should match truncated summary")
	}
}

func TestWaitForTasksTool_Execute_MissingNamespace(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, "")
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
		json.NewEncoder(w).Encode(map[string]string{resultField: string(srJSON)}) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaControllerURL, server.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testChildTaskName,
			Namespace: defaultNamespace,
			Labels: map[string]string{
				labels.LabelIteration: "2",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: testCoderAgentName},
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
		Tasks:   []string{testChildTaskName},
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
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	// Create a failed task with auto-retry annotations
	failedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskFailRetry,
			Namespace: testNamespace,
			Annotations: map[string]string{
				labels.AnnotationAutoRetry:      trueStr,
				labels.AnnotationMaxRetries:     "2",
				labels.AnnotationRetryCount:     "0",
				labels.AnnotationOriginalPrompt: "Implement the feature",
			},
			Labels: map[string]string{
				labels.LabelParentTask:     "parent",
				labels.LabelDelegatedAgent: testCoderAgentName,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: testCoderAgentName},
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
	t.Setenv(envOrkaControllerURL, srv.URL)

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

	if originalResult.Phase != taskPhaseFailedString {
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
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	// Task with retries already exhausted
	failedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-exhausted",
			Namespace: testNamespace,
			Annotations: map[string]string{
				labels.AnnotationAutoRetry:      trueStr,
				labels.AnnotationMaxRetries:     "2",
				labels.AnnotationRetryCount:     "2",
				labels.AnnotationOriginalPrompt: "Do something",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: testCoderAgentName},
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
	t.Setenv(envOrkaControllerURL, srv.URL)

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
	if r.Phase != taskPhaseFailedString {
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
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	// Succeeded task with auto-retry — should NOT trigger retry
	succeededTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-success",
			Namespace: testNamespace,
			Annotations: map[string]string{
				labels.AnnotationAutoRetry:  trueStr,
				labels.AnnotationMaxRetries: "2",
				labels.AnnotationRetryCount: "0",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: testCoderAgentName},
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
	t.Setenv(envOrkaControllerURL, srv.URL)

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
	if r.Phase != taskPhaseSucceededString {
		t.Errorf("expected Phase=Succeeded, got %q", r.Phase)
	}
}

func TestWaitForTasksTool_Execute_FetchResultNon200(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	// Server returns 500 for result fetch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()
	t.Setenv(envOrkaControllerURL, srv.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-err", Namespace: testNamespace},
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
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	// Server returns 200 but invalid JSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-valid-json"))
	}))
	defer srv.Close()
	t.Setenv(envOrkaControllerURL, srv.URL)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-bad-json", Namespace: testNamespace},
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
	t.Setenv(envOrkaTaskNamespace, testNamespace)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv(envOrkaControllerURL, srv.URL)

	// Task with no ResultRef but with a status message
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-msg", Namespace: testNamespace},
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

func TestWaitForTasksTool_Execute_PreservesStructuredData(t *testing.T) {
	sr := common.StructuredResult{
		Version: 1,
		Summary: "structured child output",
		Data: map[string]any{
			"incident": "quincy-north",
			"score":    float64(0.98),
			"nested":   map[string]any{"owner": "ops"},
		},
		Artifacts: []common.ArtifactRef{{Filename: "evidence.json", ContentType: "application/json", Size: 42}},
	}
	srJSON, _ := json.Marshal(sr)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "structured-data", Namespace: defaultNamespace},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()
	resultStore := newFakeWaitResultStore(map[string]string{"structured-data": string(srJSON)})
	toolCtx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, ResultStore: resultStore})

	tool := NewWaitForTasksTool(fakeClient)
	args, _ := json.Marshal(WaitForTasksArgs{Tasks: []string{"structured-data"}, Timeout: "1s"})
	result, err := tool.Execute(toolCtx, args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	var waitResult WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &waitResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	data := waitResult.Results[0].Data
	if data["incident"] != "quincy-north" || data["score"] != float64(0.98) {
		t.Fatalf("data = %#v", data)
	}
	if got := waitResult.Results[0].Artifacts; len(got) != 1 || got[0].Filename != "evidence.json" {
		t.Fatalf("artifacts = %#v", got)
	}
}

func TestWaitForTasksTool_Execute_TruncatesOversizedStructuredData(t *testing.T) {
	sr := common.StructuredResult{
		Version: 1,
		Summary: "large data",
		Data: map[string]any{
			"blob": strings.Repeat("x", maxWaitTaskDataBytes+1),
		},
	}
	srJSON, _ := json.Marshal(sr)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "large-data", Namespace: defaultNamespace},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(task).
		WithStatusSubresource(&corev1alpha1.Task{}).
		Build()
	resultStore := newFakeWaitResultStore(map[string]string{"large-data": string(srJSON)})
	toolCtx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, ResultStore: resultStore})

	result, err := NewWaitForTasksTool(fakeClient).Execute(toolCtx, json.RawMessage(`{"tasks":["large-data"],"timeout":"1s"}`))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	var waitResult WaitForTasksResult
	if err := json.Unmarshal([]byte(result), &waitResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	data := waitResult.Results[0].Data
	if data["truncated"] != true || data["originalBytes"] == nil {
		t.Fatalf("data = %#v, want truncation marker", data)
	}
}

type fakeWaitResultStore struct {
	values map[string]string
}

func newFakeWaitResultStore(values map[string]string) *fakeWaitResultStore {
	return &fakeWaitResultStore{values: values}
}

func (s *fakeWaitResultStore) GetResult(_ context.Context, _, taskName string) ([]byte, error) {
	value, ok := s.values[taskName]
	if !ok {
		return nil, fmt.Errorf("result not found")
	}
	return []byte(value), nil
}
