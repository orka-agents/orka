/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
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

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "nonexistent",
		HeadBranch: "feature/x",
		BaseBranch: "main",
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

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/x",
		BaseBranch: "main",
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
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", auth)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "feat: edit message" {
			t.Errorf("unexpected title: %s", body["title"])
		}
		if body["head"] != "feature/edit-msg" {
			t.Errorf("unexpected head: %s", body["head"])
		}
		if body["base"] != "main" {
			t.Errorf("unexpected base: %s", body["base"])
		}

		w.WriteHeader(201)
		fmt.Fprintf(w, `{"html_url":"https://github.com/sozercan/ayna/pull/42","number":42}`)
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
					Branch:  "main",
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

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(CreatePullRequestArgs{
		TaskName:   "coder-task",
		HeadBranch: "feature/edit-msg",
		BaseBranch: "main",
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
	if prResult.Status != "created" {
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
