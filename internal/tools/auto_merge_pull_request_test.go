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
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const prStateOpen = "open"

const prStateClosed = "closed"

func TestAutoMergePullRequestTool_Metadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewAutoMergePullRequestTool(k8sClient)

	if tool.Name() != autoMergePullRequestToolName {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description should not be empty")
	}
	if !strings.Contains(tool.Description(), "Poll GitHub CI checks") {
		t.Errorf("unexpected description: %s", tool.Description())
	}
	params := tool.Parameters()
	if len(params) == 0 {
		t.Error("parameters should not be empty")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters: %v", err)
	}
	props := schema[jsonSchemaPropertiesField].(map[string]any)
	for _, field := range []string{taskNameField, githubPRNumberField, mergeMethodField, githubCommitTitleField, githubCommitMessageField, timeoutField} {
		if _, ok := props[field]; !ok {
			t.Errorf("parameters should contain %s", field)
		}
	}
}

func TestAutoMergePullRequestTool_CIPassesImmediately(t *testing.T) {
	var mergeCalled atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pulls/42/merge"):
			mergeCalled.Store(true)
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"sha":"merged456","merged":true,"message":"Pull Request successfully merged"}`)
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

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Merged {
		t.Error("expected merged to be true")
	}
	if !res.ChecksPassed {
		t.Error("expected checks_passed to be true")
	}
	if res.SHA != "merged456" {
		t.Errorf("unexpected SHA: %s", res.SHA)
	}
	if res.Outcome != mergedStatusString {
		t.Errorf("unexpected outcome: %s", res.Outcome)
	}
	if !mergeCalled.Load() {
		t.Error("expected merge API to be called")
	}
}

func TestAutoMergePullRequestTool_CIFails(t *testing.T) {
	var mergeCalled atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
			mergeCalled.Store(true)
			w.WriteHeader(200)
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

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Merged {
		t.Error("expected merged to be false")
	}
	if res.Outcome != "ci_failed" {
		t.Errorf("unexpected outcome: %s", res.Outcome)
	}
	if !strings.Contains(res.ChecksDetails, "lint") {
		t.Errorf("expected checks_details to mention 'lint', got: %s", res.ChecksDetails)
	}
	if mergeCalled.Load() {
		t.Error("merge API should not have been called when CI fails")
	}
}

func TestAutoMergePullRequestTool_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"build","status":"in_progress","conclusion":""}]}`)
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
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Outcome != timeoutField {
		t.Errorf("expected outcome 'timeout', got: %s", res.Outcome)
	}
}

func TestAutoMergePullRequestTool_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"build","status":"in_progress","conclusion":""}]}`)
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
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "2m",
	})

	result, err := tool.Execute(ctx, args)
	// The first iteration runs immediately with a cancelled context.
	// Depending on timing, either the HTTP call fails or we get a timeout/cancelled outcome.
	if err != nil {
		// Error is acceptable when context is cancelled during HTTP calls
		return
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Outcome != timeoutField {
		t.Errorf("expected outcome 'timeout', got: %s", res.Outcome)
	}
}

func TestAutoMergePullRequestTool_MergeMethods(t *testing.T) {
	for _, method := range []string{mergeMethodMerge, mergeMethodRebase} {
		t.Run(method, func(t *testing.T) {
			var capturedMethod string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/10"):
					w.WriteHeader(200)
					_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
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

			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
			tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

			t.Setenv(envOrkaTaskNamespace, defaultNamespace)

			args, _ := json.Marshal(AutoMergePullRequestArgs{
				TaskName:    testCoderTaskName,
				PRNumber:    10,
				MergeMethod: method,
				Timeout:     "1s",
			})

			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if capturedMethod != method {
				t.Errorf("expected merge_method %q, got %q", method, capturedMethod)
			}

			var res AutoMergePullRequestResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Merged {
				t.Error("expected merged to be true")
			}
		})
	}
}

func TestAutoMergePullRequestTool_PRClosedExternally(t *testing.T) {
	var mergeCalled atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"closed","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
			mergeCalled.Store(true)
			w.WriteHeader(200)
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
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Merged {
		t.Error("expected merged to be false")
	}
	if res.Outcome != prStateClosed {
		t.Errorf("unexpected outcome: %s", res.Outcome)
	}
	if mergeCalled.Load() {
		t.Error("merge API should not have been called for closed PR")
	}
}

func TestAutoMergePullRequestTool_PRAlreadyMerged(t *testing.T) {
	var mergeCalled atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"closed","merged":true}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
			mergeCalled.Store(true)
			w.WriteHeader(200)
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
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Merged {
		t.Error("expected merged to be true")
	}
	if res.Outcome != "already_merged" {
		t.Errorf("unexpected outcome: %s", res.Outcome)
	}
	if mergeCalled.Load() {
		t.Error("merge API should not have been called for already-merged PR")
	}
}

func TestAutoMergePullRequestTool_InvalidArgs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewAutoMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args := json.RawMessage(`{"bad": true}`)

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for invalid args")
	}
	if got := err.Error(); !strings.Contains(got, "task_name and pr_number are required") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestAutoMergePullRequestTool_MissingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewAutoMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
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

// --- Helper function unit tests ---

func TestGetGitHubPRDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/sozercan/ayna/pulls/42") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"head":{"sha":"sha999"},"state":"open","merged":false}`)
	}))
	defer server.Close()

	headSHA, state, merged, err := getGitHubPRDetails(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, 42, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if headSHA != "sha999" {
		t.Errorf("unexpected headSHA: %s", headSHA)
	}
	if state != prStateOpen {
		t.Errorf("unexpected state: %s", state)
	}
	if merged {
		t.Error("expected merged to be false")
	}
}

func TestGetGitHubPRDetails_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":"internal server error"}`)
	}))
	defer server.Close()

	_, _, _, err := getGitHubPRDetails(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, 42, server.URL)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	apiErr, ok := err.(*githubAPIError) //nolint:errorlint
	if !ok {
		t.Fatalf("expected githubAPIError, got %T", err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("unexpected status code: %d", apiErr.StatusCode)
	}
}

func TestCheckCIStatusDetailed_AllPassed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":3,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"success"},{"name":"optional","status":"completed","conclusion":"neutral"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Error("expected Passed to be true")
	}
	if result.Failed {
		t.Error("expected Failed to be false")
	}
	if result.Pending {
		t.Error("expected Pending to be false")
	}
}

func TestCheckCIStatusDetailed_SomePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"deploy","status":"in_progress","conclusion":""}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Error("expected Passed to be false")
	}
	if result.Failed {
		t.Error("expected Failed to be false")
	}
	if !result.Pending {
		t.Error("expected Pending to be true")
	}
	if !strings.Contains(result.Details, "deploy") {
		t.Errorf("expected details to mention 'deploy', got: %s", result.Details)
	}
}

func TestCheckCIStatusDetailed_SomeFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"e2e","status":"completed","conclusion":"failure"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Error("expected Passed to be false")
	}
	if !result.Failed {
		t.Error("expected Failed to be true")
	}
	if result.Pending {
		t.Error("expected Pending to be false")
	}
	if !strings.Contains(result.Details, "e2e") {
		t.Errorf("expected details to mention 'e2e', got: %s", result.Details)
	}
}

func TestIsTransientHTTPError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"429 rate limit", &githubAPIError{StatusCode: 429, Body: "rate limit"}, true},
		{"500 server error", &githubAPIError{StatusCode: 500, Body: serverErrorMessage}, true},
		{"503 unavailable", &githubAPIError{StatusCode: 503, Body: "unavailable"}, true},
		{"404 not found", &githubAPIError{StatusCode: 404, Body: notFoundMessage}, false},
		{"non-github error", fmt.Errorf("random error"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isTransientHTTPError(tc.err)
			if got != tc.expected {
				t.Errorf("isTransientHTTPError(%v) = %v, want %v", tc.err, got, tc.expected)
			}
		})
	}
}

func TestAutoMergePullRequestTool_ForcePushNewSHA(t *testing.T) {
	// Test getGitHubPRDetails returns different SHAs on successive calls
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			n := callCount.Add(1)
			if n == 1 {
				w.WriteHeader(200)
				_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
			} else {
				w.WriteHeader(200)
				_, _ = fmt.Fprintf(w, `{"head":{"sha":"def456"},"state":"open","merged":false}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	sha1, state1, merged1, err := getGitHubPRDetails(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, 42, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha1 != checkPullRequestCITestSHA {
		t.Errorf("expected first SHA 'abc123', got: %s", sha1)
	}
	if state1 != prStateOpen {
		t.Errorf("unexpected state: %s", state1)
	}
	if merged1 {
		t.Error("expected merged to be false")
	}

	sha2, _, _, err := getGitHubPRDetails(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, 42, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha2 != "def456" {
		t.Errorf("expected second SHA 'def456', got: %s", sha2)
	}
}

func TestGitHubAPIError_Error(t *testing.T) {
	tests := []struct {
		name       string
		err        *githubAPIError
		wantSubstr string
	}{
		{
			name:       "contains status code",
			err:        &githubAPIError{StatusCode: 404, Body: notFoundMessage},
			wantSubstr: "404",
		},
		{
			name:       "contains body",
			err:        &githubAPIError{StatusCode: 500, Body: serverErrorMessage},
			wantSubstr: serverErrorMessage,
		},
		{
			name:       "empty body",
			err:        &githubAPIError{StatusCode: 422, Body: ""},
			wantSubstr: "422",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("Error() = %q, want substring %q", got, tc.wantSubstr)
			}
		})
	}
}

func TestPollOnce_TransientPRError(t *testing.T) {
	// Test that pollOnce handles transient errors from getGitHubPRDetails
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			callCount++
			// Return 429 rate limit (transient)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"message":"rate limited"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	pc := &pollContext{
		token: testGitHubToken, owner: testGitHubOwner, repo: testRepositoryName,
		prNumber: 42, mergeMethod: defaultMergeMethod,
		baseURL: server.URL, logger: logr.Discard(),
	}

	result, done, err := pc.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("expected no error for transient PR error, got: %v", err)
	}
	if done {
		t.Error("expected done=false for transient error")
	}
	if result != nil {
		t.Error("expected nil result for transient error")
	}
}

func TestPollOnce_NonTransientPRError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"message":"not found"}`)
	}))
	defer server.Close()

	pc := &pollContext{
		token: testGitHubToken, owner: testGitHubOwner, repo: testRepositoryName,
		prNumber: 42, mergeMethod: defaultMergeMethod,
		baseURL: server.URL, logger: logr.Discard(),
	}

	_, _, err := pc.pollOnce(context.Background())
	if err == nil {
		t.Fatal("expected error for non-transient PR error")
	}
	if !strings.Contains(err.Error(), "failed to get PR details") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPollOnce_TransientCIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"message":"rate limited"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	pc := &pollContext{
		token: testGitHubToken, owner: testGitHubOwner, repo: testRepositoryName,
		prNumber: 42, mergeMethod: defaultMergeMethod,
		baseURL: server.URL, logger: logr.Discard(),
	}

	result, done, err := pc.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("expected no error for transient CI error, got: %v", err)
	}
	if done {
		t.Error("expected done=false for transient CI error")
	}
	if result != nil {
		t.Error("expected nil result for transient CI error")
	}
}

func TestPollOnce_NonTransientCIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"message":"not found"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	pc := &pollContext{
		token: testGitHubToken, owner: testGitHubOwner, repo: testRepositoryName,
		prNumber: 42, mergeMethod: defaultMergeMethod,
		baseURL: server.URL, logger: logr.Discard(),
	}

	_, _, err := pc.pollOnce(context.Background())
	if err == nil {
		t.Fatal("expected error for non-transient CI error")
	}
	if !strings.Contains(err.Error(), "failed to check CI status") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPollOnce_MergeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/") && !strings.Contains(r.URL.Path, "/merge"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
		case strings.Contains(r.URL.Path, "/merge"):
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"message":"merge conflict"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	pc := &pollContext{
		token: testGitHubToken, owner: testGitHubOwner, repo: testRepositoryName,
		prNumber: 42, mergeMethod: defaultMergeMethod,
		baseURL: server.URL, logger: logr.Discard(),
	}

	_, _, err := pc.pollOnce(context.Background())
	if err == nil {
		t.Fatal("expected error for merge failure")
	}
	if !strings.Contains(err.Error(), "failed to merge pull request") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPollOnce_CIPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"build","status":"queued","conclusion":""}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	pc := &pollContext{
		token: testGitHubToken, owner: testGitHubOwner, repo: testRepositoryName,
		prNumber: 42, mergeMethod: defaultMergeMethod,
		baseURL: server.URL, logger: logr.Discard(),
	}

	result, done, err := pc.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Error("expected done=false for pending CI")
	}
	if result != nil {
		t.Error("expected nil result for pending CI")
	}
}

func TestCheckCIStatusDetailed_SkippedConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"optional","status":"completed","conclusion":"skipped"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Error("expected Passed=true when all checks are success/skipped")
	}
}

func TestCheckCIStatusDetailed_CancelledConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"cancelled"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Failed {
		t.Error("expected Failed=true for cancelled conclusion")
	}
	if !strings.Contains(result.Details, cancelledStatusString) {
		t.Errorf("expected details to mention 'cancelled', got: %s", result.Details)
	}
}

func TestCheckCIStatusDetailed_TimedOutConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"timed_out"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Failed {
		t.Error("expected Failed=true for timed_out conclusion")
	}
}

func TestCheckCIStatusDetailed_ActionRequiredConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"action_required"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Failed {
		t.Error("expected Failed=true for action_required conclusion")
	}
}

func TestCheckCIStatusDetailed_StaleConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"stale"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Failed {
		t.Error("expected Failed=true for stale conclusion")
	}
}

func TestCheckCIStatusDetailed_UnknownConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"unknown_thing"}]}`)
	}))
	defer server.Close()

	result, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Failed {
		t.Error("expected Failed=true for unknown conclusion")
	}
}

func TestCheckCIStatusDetailed_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":"error"}`)
	}))
	defer server.Close()

	_, err := checkCIStatusDetailed(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, checkPullRequestCITestSHA, server.URL)
	if err == nil {
		t.Fatal("expected error for API error")
	}
}

func TestAutoMergePullRequestTool_InvalidTimeout(t *testing.T) {
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
		Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewAutoMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "not-a-duration",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for invalid timeout")
	}
	if !strings.Contains(err.Error(), invalidTimeoutCaseName) {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestAutoMergePullRequestTool_NoWorkspace(t *testing.T) {
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
	tool := NewAutoMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
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

func TestAutoMergePullRequestTool_NoGitSecretRef(t *testing.T) {
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
	tool := NewAutoMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for task without gitSecretRef")
	}
	if !strings.Contains(err.Error(), "no gitSecretRef") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestAutoMergePullRequestTool_EmptyToken(t *testing.T) {
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
	tool := NewAutoMergePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
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

func TestAutoMergePullRequestTool_PasswordKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"sha":"merged456","merged":true,"message":"merged"}`)
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
		Data:       map[string][]byte{passwordKey: []byte("pwd-token")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &AutoMergePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(AutoMergePullRequestArgs{
		TaskName: testCoderTaskName,
		PRNumber: 42,
		Timeout:  "1s",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res AutoMergePullRequestResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Merged {
		t.Error("expected merged=true")
	}
}

func TestAutoMergePullRequestTool_Transient5xx(t *testing.T) {
	// Test that getGitHubPRDetails returns a transient error on 500, and succeeds on retry
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/pulls/") {
			w.WriteHeader(404)
			return
		}
		n := callCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"message":"server error"}`)
		} else {
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPullRequestCITestSHA)
		}
	}))
	defer server.Close()

	// First call: should return transient error
	_, _, _, err := getGitHubPRDetails(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, 42, server.URL)
	if err == nil {
		t.Fatal("expected error on first call")
	}
	if !isTransientHTTPError(err) {
		t.Errorf("expected transient HTTP error, got: %v", err)
	}

	// Second call: should succeed
	sha, state, merged, err := getGitHubPRDetails(context.Background(), testGitHubToken, testGitHubOwner, testRepositoryName, 42, server.URL)
	if err != nil {
		t.Fatalf("unexpected error on retry: %v", err)
	}
	if sha != checkPullRequestCITestSHA {
		t.Errorf("unexpected SHA: %s", sha)
	}
	if state != prStateOpen {
		t.Errorf("unexpected state: %s", state)
	}
	if merged {
		t.Error("expected merged to be false")
	}
}
