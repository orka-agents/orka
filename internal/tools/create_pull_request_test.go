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

const (
	testBearerToken = "Bearer test-token"
	testBranch      = "main"
	statusCreated   = GitHubPullRequestStatusCreated
	statusExisting  = "existing"
)

func TestParseGitHubRepo(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{
			name:      "HTTPS URL",
			url:       testSozercanAynaRepoURL,
			wantOwner: testGitHubOwner,
			wantRepo:  testRepositoryName,
		},
		{
			name:      "HTTPS URL with .git",
			url:       "https://github.com/sozercan/ayna.git",
			wantOwner: testGitHubOwner,
			wantRepo:  testRepositoryName,
		},
		{
			name:      "SSH URL",
			url:       "git@github.com:sozercan/ayna.git",
			wantOwner: testGitHubOwner,
			wantRepo:  testRepositoryName,
		},
		{
			name:      "HTTPS URL with trailing slash",
			url:       "https://github.com/sozercan/ayna/",
			wantOwner: testGitHubOwner,
			wantRepo:  testRepositoryName,
		},
		{
			name:    "non-GitHub URL",
			url:     "https://gitlab.com/user/repo",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := parseGitHubRepo(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got owner=%s repo=%s", owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

func TestCreatePullRequestTool_MissingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testNonexistentName,
		HeadBranch: testFeatureBranch,
		BaseBranch: testBranch,
		Title:      testPullRequestTitle,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get task") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestCreatePullRequestTool_NoWorkspace(t *testing.T) {
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
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testFeatureBranch,
		BaseBranch: testBranch,
		Title:      testPullRequestTitle,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for task without workspace")
	}
	if got := err.Error(); !strings.Contains(got, "does not have workspace") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestCreatePullRequestTool_Success(t *testing.T) {
	largeBody := strings.Repeat("x", 9000)

	// Mock GitHub API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if body[titleField] != testEditCommitTitle {
			t.Errorf("unexpected title: %s", body[titleField])
		}
		if body["head"] != testEditMessageBranch {
			t.Errorf("unexpected head: %s", body["head"])
		}
		if body["base"] != testBranch {
			t.Errorf("unexpected base: %s", body["base"])
		}

		w.WriteHeader(201)
		fmt.Fprintf(w, `{"html_url":"https://github.com/sozercan/ayna/pull/42","number":42,"body":%q}`, largeBody) //nolint:errcheck
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
	tool := &CreatePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testEditMessageBranch,
		BaseBranch: testBranch,
		Title:      testEditCommitTitle,
		Body:       "Implements #19",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var prResult CreatePullRequestResult
	if err := json.Unmarshal([]byte(result), &prResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if prResult.PRURL != "https://github.com/sozercan/ayna/pull/42" {
		t.Errorf("unexpected PR URL: %s", prResult.PRURL)
	}
	if prResult.PRNumber != 42 {
		t.Errorf("unexpected PR number: %d", prResult.PRNumber)
	}
	if prResult.Status != statusCreated {
		t.Errorf("unexpected status: %s", prResult.Status)
	}

	// Also verify the tool interface
	publicTool := NewCreatePullRequestTool(k8sClient)
	if publicTool.Name() != createPullRequestToolName {
		t.Errorf("unexpected name: %s", publicTool.Name())
	}
	if publicTool.Description() == "" {
		t.Error("description should not be empty")
	}
	params := publicTool.Parameters()
	if len(params) == 0 {
		t.Error("parameters should not be empty")
	}
}

func TestCreatePullRequestTool_ExistingPR(t *testing.T) {
	largeBody := strings.Repeat("x", 9000)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}

		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for sozercan:feature/edit-msg."}]}`) //nolint:errcheck
		case http.MethodGet:
			if got := r.URL.Query().Get("head"); got != "sozercan:feature/edit-msg" {
				t.Errorf("unexpected head query: %s", got)
			}
			if got := r.URL.Query().Get("base"); got != testBranch {
				t.Errorf("unexpected base query: %s", got)
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"html_url":"https://github.com/sozercan/ayna/pull/43","number":43,"body":%q}]`, largeBody) //nolint:errcheck
		default:
			t.Fatalf("unexpected method: %s", r.Method)
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
	tool := &CreatePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testEditMessageBranch,
		BaseBranch: testBranch,
		Title:      testEditCommitTitle,
		Body:       "Implements #19",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var prResult CreatePullRequestResult
	if err := json.Unmarshal([]byte(result), &prResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if prResult.PRURL != "https://github.com/sozercan/ayna/pull/43" {
		t.Errorf("unexpected PR URL: %s", prResult.PRURL)
	}
	if prResult.PRNumber != 43 {
		t.Errorf("unexpected PR number: %d", prResult.PRNumber)
	}
	if prResult.Status != statusExisting {
		t.Errorf("unexpected status: %s", prResult.Status)
	}
	if requests != 2 {
		t.Errorf("expected 2 GitHub API requests, got %d", requests)
	}
}

func TestCreatePullRequestTool_InvalidArgs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	// Missing required fields
	args := json.RawMessage(`{"task_name": "t"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
	if !strings.Contains(err.Error(), jsonSchemaRequiredField) {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreatePullRequestTool_InvalidJSON(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	_, err := tool.Execute(context.Background(), json.RawMessage(invalidJSONText))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCreatePullRequestTool_NoGitRepo(t *testing.T) {
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
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testFeatureBranch,
		BaseBranch: testBranch,
		Title:      testPullRequestTitle,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for no gitRepo")
	}
	if !strings.Contains(err.Error(), "no gitRepo") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreatePullRequestTool_NoGitSecretRefWithoutFallbackToken(t *testing.T) {
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
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testFeatureBranch,
		BaseBranch: testBranch,
		Title:      testPullRequestTitle,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for no token")
	}
	if !strings.Contains(err.Error(), "could not resolve GitHub token") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreatePullRequestTool_EmptyToken(t *testing.T) {
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
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testFeatureBranch,
		BaseBranch: testBranch,
		Title:      testPullRequestTitle,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "does not contain a 'token', 'password', or 'GITHUB_TOKEN' key") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreateGitHubPR_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprintf(w, `{"message":"Validation Failed","errors":[{"message":"No commits between base and head"}]}`)
	}))
	defer server.Close()

	_, _, _, err := createGitHubPR(context.Background(), tokenKey, "owner", "repo", "head", "base", titleField, "body", server.URL)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}
