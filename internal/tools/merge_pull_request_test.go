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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestMergePullRequestTool_Metadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewMergePullRequestTool(k8sClient)

	if tool.Name() != mergePullRequestToolName {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description should not be empty")
	}
	if !strings.Contains(tool.Description(), "Merge a GitHub pull request") {
		t.Errorf("unexpected description: %s", tool.Description())
	}
	params := tool.Parameters()
	if len(params) == 0 {
		t.Error("parameters should not be empty")
	}
	// Verify schema contains expected fields
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters: %v", err)
	}
	props := schema[jsonSchemaPropertiesField].(map[string]any)
	if _, ok := props[taskNameField]; !ok {
		t.Error("parameters should contain task_name")
	}
	if _, ok := props[githubPRNumberField]; !ok {
		t.Error("parameters should contain pr_number")
	}
	if _, ok := props[mergeMethodField]; !ok {
		t.Error("parameters should contain merge_method")
	}
}

func TestMergePullRequestTool_MissingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testNonexistentName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get task") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMergePullRequestTool_MissingSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: testGitCredsSecretName,
					},
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get git secret") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMergePullRequestTool_CIChecksFailed(t *testing.T) {
	// Mock GitHub API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			// Return PR details with head SHA
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":"abc123"},"number":42}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			// Return failed check
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"failure"}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: testGitCredsSecretName,
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &MergePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var mergeResult MergePullRequestResult
	if err := json.Unmarshal([]byte(result), &mergeResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if mergeResult.Merged {
		t.Error("expected merged to be false")
	}
	if mergeResult.ChecksPassed {
		t.Error("expected checks_passed to be false")
	}
	if !strings.Contains(mergeResult.Message, "lint") {
		t.Errorf("expected message to mention failing check, got: %s", mergeResult.Message)
	}
}

func TestMergePullRequestTool_Success(t *testing.T) {
	// Mock GitHub API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			// Return PR details with head SHA
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":"abc123"},"number":42}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			// All checks passed
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pulls/42/merge"):
			// Verify merge request body
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			if body[mergeMethodField] != defaultMergeMethod {
				t.Errorf("unexpected merge_method: %v", body[mergeMethodField])
			}
			if body[githubCommitTitleField] != "feat: awesome feature (#42)" {
				t.Errorf("unexpected commit_title: %v", body[githubCommitTitleField])
			}
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"sha":"def456","merged":true,"message":"Pull Request successfully merged"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: testGitCredsSecretName,
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &MergePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName:      testCoderTaskName,
		PRNumber:      42,
		CommitTitle:   "feat: awesome feature (#42)",
		CommitMessage: "Implements the awesome feature",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var mergeResult MergePullRequestResult
	if err := json.Unmarshal([]byte(result), &mergeResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !mergeResult.Merged {
		t.Error("expected merged to be true")
	}
	if !mergeResult.ChecksPassed {
		t.Error("expected checks_passed to be true")
	}
	if mergeResult.SHA != "def456" {
		t.Errorf("unexpected SHA: %s", mergeResult.SHA)
	}
	if mergeResult.Message != pullRequestMergedMessage {
		t.Errorf("unexpected message: %s", mergeResult.Message)
	}
}

func TestMergePullRequestTool_Execute_APIError(t *testing.T) {
	// Mock GitHub API returning 500 on PR details fetch
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":"internal server error"}`)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: testGitCredsSecretName,
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &MergePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get PR head SHA") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMergePullRequestTool_Execute_InvalidArgs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	// Pass args missing required fields
	args := json.RawMessage(`{"bad": true}`)

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for invalid args")
	}
	if got := err.Error(); !strings.Contains(got, "task_name and pr_number are required") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMergePullRequestTool_Execute_DifferentMergeMethods(t *testing.T) {
	for _, method := range []string{mergeMethodMerge, mergeMethodRebase} {
		t.Run(method, func(t *testing.T) {
			var capturedMethod string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/10"):
					w.WriteHeader(200)
					_, _ = fmt.Fprintf(w, `{"head":{"sha":"abc123"},"number":10}`)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
					w.WriteHeader(200)
					_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pulls/10/merge"):
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if mm, ok := body[mergeMethodField].(string); ok {
						capturedMethod = mm
					}
					w.WriteHeader(200)
					_, _ = fmt.Fprintf(w, `{"sha":"merged123","merged":true,"message":"merged"}`)
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()

			scheme := runtime.NewScheme()
			_ = corev1alpha1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAgent,
					AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
						Workspace: &corev1alpha1.WorkspaceConfig{
							GitRepo: testSozercanAynaRepoURL,
							Branch:  testBranch,
							GitSecretRef: &corev1.LocalObjectReference{
								Name: testGitCredsSecretName,
							},
						},
					},
				},
			}

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
				Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
			}

			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
			tool := &MergePullRequestTool{
				k8sClient:  k8sClient,
				apiBaseURL: server.URL,
			}

			t.Setenv(envOrkaTaskNamespace, defaultNamespace)

			args, _ := json.Marshal(MergePullRequestArgs{
				TaskName:    testCoderTaskName,
				PRNumber:    10,
				MergeMethod: method,
			})

			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if capturedMethod != method {
				t.Errorf("expected merge_method %q, got %q", method, capturedMethod)
			}

			var mergeResult MergePullRequestResult
			if err := json.Unmarshal([]byte(result), &mergeResult); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !mergeResult.Merged {
				t.Error("expected merged to be true")
			}
		})
	}
}

func TestMergePullRequestTool_Execute_PRAlreadyMerged(t *testing.T) {
	// Mock GitHub API: PR fetch and checks succeed, but merge returns 405
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":"abc123"},"number":42}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pulls/42/merge"):
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = fmt.Fprintf(w, `{"message":"Pull Request is not mergeable","documentation_url":"https://docs.github.com"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: testGitCredsSecretName,
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &MergePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for already-merged PR")
	}
	if got := err.Error(); !strings.Contains(got, "failed to merge pull request") {
		t.Errorf("unexpected error: %s", got)
	}
	if got := err.Error(); !strings.Contains(got, "405") {
		t.Errorf("expected error to contain status code 405, got: %s", got)
	}
}

func TestMergePullRequestTool_NoWorkspace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for task without workspace")
	}
	if !strings.Contains(err.Error(), "does not have workspace") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestMergePullRequestTool_NoGitRepo(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					Branch: testBranch,
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty gitRepo")
	}
	if !strings.Contains(err.Error(), "no gitRepo") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestMergePullRequestTool_NoGitSecretRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for no gitSecretRef")
	}
	if !strings.Contains(err.Error(), "no gitSecretRef") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestMergePullRequestTool_EmptyToken(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

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
		Data:       map[string][]byte{otherSecretKey: []byte("value")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "does not contain a 'token' or 'password' key") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestMergePullRequestTool_PasswordKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":"abc123"},"number":42}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"sha":"def456","merged":true,"message":"merged"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

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
		Data:       map[string][]byte{passwordKey: []byte("my-password")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &MergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var mergeResult MergePullRequestResult
	if err := json.Unmarshal([]byte(result), &mergeResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !mergeResult.Merged {
		t.Error("expected merged=true")
	}
}
