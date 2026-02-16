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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	testBearerToken = "Bearer test-token"
	testBranch      = "main"
	statusCreated   = "created"
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
			url:       "https://github.com/sozercan/ayna",
			wantOwner: "sozercan",
			wantRepo:  "ayna",
		},
		{
			name:      "HTTPS URL with .git",
			url:       "https://github.com/sozercan/ayna.git",
			wantOwner: "sozercan",
			wantRepo:  "ayna",
		},
		{
			name:      "SSH URL",
			url:       "git@github.com:sozercan/ayna.git",
			wantOwner: "sozercan",
			wantRepo:  "ayna",
		},
		{
			name:      "HTTPS URL with trailing slash",
			url:       "https://github.com/sozercan/ayna/",
			wantOwner: "sozercan",
			wantRepo:  "ayna",
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

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "nonexistent",
		HeadBranch: "feature/x",
		BaseBranch: testBranch,
		Title:      "Test PR",
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
		ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/x",
		BaseBranch: testBranch,
		Title:      "Test PR",
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
		if body["title"] != "feat: edit message" {
			t.Errorf("unexpected title: %s", body["title"])
		}
		if body["head"] != "feature/edit-msg" {
			t.Errorf("unexpected head: %s", body["head"])
		}
		if body["base"] != testBranch {
			t.Errorf("unexpected base: %s", body["base"])
		}

		w.WriteHeader(201)
		fmt.Fprintf(w, `{"html_url":"https://github.com/sozercan/ayna/pull/42","number":42}`) //nolint:errcheck
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/sozercan/ayna",
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: "git-creds",
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "default"},
		Data: map[string][]byte{
			"token": []byte("test-token"),
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &CreatePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/edit-msg",
		BaseBranch: testBranch,
		Title:      "feat: edit message",
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
	if publicTool.Name() != "create_pull_request" {
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

func TestCreatePullRequestTool_InvalidArgs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	// Missing required fields
	args := json.RawMessage(`{"task_name": "t"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreatePullRequestTool_InvalidJSON(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{invalid}`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCreatePullRequestTool_NoGitRepo(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					Branch: "main",
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/x",
		BaseBranch: testBranch,
		Title:      "Test PR",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for no gitRepo")
	}
	if !strings.Contains(err.Error(), "no gitRepo") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreatePullRequestTool_NoGitSecretRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/sozercan/ayna",
					Branch:  testBranch,
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/x",
		BaseBranch: testBranch,
		Title:      "Test PR",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for no gitSecretRef")
	}
	if !strings.Contains(err.Error(), "no gitSecretRef") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreatePullRequestTool_EmptyToken(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/sozercan/ayna",
					Branch:       testBranch,
					GitSecretRef: &corev1.LocalObjectReference{Name: "git-creds"},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "default"},
		Data:       map[string][]byte{"other-key": []byte("value")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewCreatePullRequestTool(k8sClient)

	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/x",
		BaseBranch: testBranch,
		Title:      "Test PR",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "does not contain a 'token' or 'password' key") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCreateGitHubPR_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprintf(w, `{"message":"Validation Failed"}`)
	}))
	defer server.Close()

	_, _, err := createGitHubPR("token", "owner", "repo", "head", "base", "title", "body", server.URL)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}
