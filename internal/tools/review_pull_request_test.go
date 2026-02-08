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

func TestReviewPullRequestTool_Metadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewReviewPullRequestTool(k8sClient)

	if tool.Name() != "review_pull_request" {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description should not be empty")
	}
	params := tool.Parameters()
	if len(params) == 0 {
		t.Error("parameters should not be empty")
	}

	// Verify schema contains expected fields
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters schema: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, ok := props["task_name"]; !ok {
		t.Error("schema missing task_name property")
	}
	if _, ok := props["pr_number"]; !ok {
		t.Error("schema missing pr_number property")
	}
}

func TestReviewPullRequestTool_MissingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewReviewPullRequestTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(ReviewPullRequestArgs{
		TaskName: "nonexistent",
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

func TestReviewPullRequestTool_MissingSecret(t *testing.T) {
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

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewReviewPullRequestTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(ReviewPullRequestArgs{
		TaskName: "coder-task",
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

func TestReviewPullRequestTool_Success(t *testing.T) {
	// Mock GitHub API with 3 endpoints
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", auth)
		}

		accept := r.Header.Get("Accept")
		path := r.URL.Path

		switch {
		case path == "/repos/sozercan/ayna/pulls/42" && accept == "application/vnd.github.v3.diff":
			// PR diff endpoint
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprint(w, "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,4 @@\n package main\n+// new comment\n")

		case path == "/repos/sozercan/ayna/pulls/42":
			// PR details endpoint
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{
				"title": "feat: add comment",
				"body": "Adds a new comment to main.go",
				"user": {"login": "testuser"},
				"base": {"ref": "main"},
				"head": {"ref": "feature/add-comment"}
			}`)

		case path == "/repos/sozercan/ayna/pulls/42/files":
			// PR files endpoint
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[{
				"filename": "main.go",
				"status": "modified",
				"additions": 1,
				"deletions": 0,
				"patch": "@@ -1,3 +1,4 @@\n package main\n+// new comment"
			}]`)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, path)
			w.WriteHeader(404)
		}
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
	tool := &ReviewPullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(ReviewPullRequestArgs{
		TaskName: "coder-task",
		PRNumber: 42,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var reviewResult ReviewPullRequestResult
	if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if reviewResult.PRTitle != "feat: add comment" {
		t.Errorf("unexpected PR title: %s", reviewResult.PRTitle)
	}
	if reviewResult.PRBody != "Adds a new comment to main.go" {
		t.Errorf("unexpected PR body: %s", reviewResult.PRBody)
	}
	if reviewResult.PRAuthor != "testuser" {
		t.Errorf("unexpected PR author: %s", reviewResult.PRAuthor)
	}
	if reviewResult.BaseBranch != "main" {
		t.Errorf("unexpected base branch: %s", reviewResult.BaseBranch)
	}
	if reviewResult.HeadBranch != "feature/add-comment" {
		t.Errorf("unexpected head branch: %s", reviewResult.HeadBranch)
	}
	if reviewResult.Diff == "" {
		t.Error("diff should not be empty")
	}
	if reviewResult.Status != "fetched" {
		t.Errorf("unexpected status: %s", reviewResult.Status)
	}
	if len(reviewResult.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(reviewResult.Files))
	}
	f := reviewResult.Files[0]
	if f.Filename != "main.go" {
		t.Errorf("unexpected filename: %s", f.Filename)
	}
	if f.Status != "modified" {
		t.Errorf("unexpected file status: %s", f.Status)
	}
	if f.Additions != 1 {
		t.Errorf("unexpected additions: %d", f.Additions)
	}
	if f.Deletions != 0 {
		t.Errorf("unexpected deletions: %d", f.Deletions)
	}
	if f.Patch == "" {
		t.Error("patch should not be empty")
	}
}
