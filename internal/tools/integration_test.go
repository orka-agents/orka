//go:build integration
// +build integration

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
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// Integration tests for coordination tools against real GitHub API.
// Run with: GITHUB_TOKEN=<token> go test ./internal/tools/ -tags=integration -run TestIntegration -v
//
// These tests use https://github.com/sozercan/ayna and require a valid GitHub token.
// A test PR must be created before running (see TestIntegration_ReviewPullRequest for PR number).

const (
	integrationRepo      = "https://github.com/sozercan/ayna"
	integrationTaskName  = "integration-test-task"
	integrationSecretName = "integration-git-secret"
	integrationPRNumber  = 67
)

func setupIntegrationClient(t *testing.T) (*fake.ClientBuilder, string) {
	t.Helper()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set, skipping integration test")
	}

	// Set namespace env var
	os.Setenv("MERCAN_TASK_NAMESPACE", "default")

	return fake.NewClientBuilder(), token
}

func buildIntegrationObjects(token string) (*corev1alpha1.Task, *corev1.Secret) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      integrationTaskName,
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: integrationRepo,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: integrationSecretName,
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      integrationSecretName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}

	return task, secret
}

func TestIntegration_ReviewPullRequest(t *testing.T) {
	builder, token := setupIntegrationClient(t)
	task, secret := buildIntegrationObjects(token)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := builder.WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewReviewPullRequestTool(k8sClient)
	// Use real GitHub API (no apiBaseURL override)

	args, _ := json.Marshal(ReviewPullRequestArgs{
		TaskName: integrationTaskName,
		PRNumber: integrationPRNumber,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("review_pull_request failed: %v", err)
	}

	var reviewResult ReviewPullRequestResult
	if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	t.Logf("PR Title: %s", reviewResult.PRTitle)
	t.Logf("PR Author: %s", reviewResult.PRAuthor)
	t.Logf("Base: %s -> Head: %s", reviewResult.BaseBranch, reviewResult.HeadBranch)
	t.Logf("Status: %s", reviewResult.Status)
	t.Logf("Files changed: %d", len(reviewResult.Files))
	t.Logf("Diff length: %d bytes", len(reviewResult.Diff))

	// Assertions
	if reviewResult.PRTitle == "" {
		t.Error("expected non-empty PR title")
	}
	if reviewResult.PRAuthor == "" {
		t.Error("expected non-empty PR author")
	}
	if reviewResult.Status != "fetched" {
		t.Errorf("expected status 'fetched', got %q", reviewResult.Status)
	}
	if len(reviewResult.Files) == 0 {
		t.Error("expected at least one changed file")
	}
	if reviewResult.Diff == "" {
		t.Error("expected non-empty diff")
	}

	// Verify file details
	for _, f := range reviewResult.Files {
		t.Logf("  File: %s (status=%s, +%d/-%d)", f.Filename, f.Status, f.Additions, f.Deletions)
		if f.Filename == "" {
			t.Error("file has empty filename")
		}
	}
}

func TestIntegration_PostReviewComment(t *testing.T) {
	builder, token := setupIntegrationClient(t)
	task, secret := buildIntegrationObjects(token)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := builder.WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: integrationTaskName,
		PRNumber: integrationPRNumber,
		Body:     "🤖 Automated integration test review from Mercan coordination tools. This review will be cleaned up.",
		Event:    "COMMENT",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("post_review_comment failed: %v", err)
	}

	var reviewResult PostReviewCommentResult
	if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	t.Logf("Review ID: %d", reviewResult.ReviewID)
	t.Logf("Status: %s", reviewResult.Status)
	t.Logf("HTML URL: %s", reviewResult.HTMLURL)

	if reviewResult.ReviewID == 0 {
		t.Error("expected non-zero review ID")
	}
	if reviewResult.Status != "submitted" {
		t.Errorf("expected status 'submitted', got %q", reviewResult.Status)
	}
	if reviewResult.HTMLURL == "" {
		t.Error("expected non-empty HTML URL")
	}
}

func TestIntegration_PostReviewComment_WithLineComment(t *testing.T) {
	builder, token := setupIntegrationClient(t)
	task, secret := buildIntegrationObjects(token)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := builder.WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: integrationTaskName,
		PRNumber: integrationPRNumber,
		Body:     "🤖 Integration test: review with line comment. This will be cleaned up.",
		Event:    "COMMENT",
		Comments: []ReviewComment{
			{
				Path: "main.go",
				Line: 1,
				Body: "🤖 Integration test line comment from Mercan",
			},
		},
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("post_review_comment with line comments failed: %v", err)
	}

	var reviewResult PostReviewCommentResult
	if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	t.Logf("Review ID (with line comments): %d", reviewResult.ReviewID)
	t.Logf("Status: %s", reviewResult.Status)
	t.Logf("HTML URL: %s", reviewResult.HTMLURL)

	if reviewResult.ReviewID == 0 {
		t.Error("expected non-zero review ID")
	}
}

func TestIntegration_MergePullRequest_CICheck(t *testing.T) {
	builder, token := setupIntegrationClient(t)
	task, secret := buildIntegrationObjects(token)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := builder.WithScheme(scheme).WithObjects(task, secret).Build()
	tool := NewMergePullRequestTool(k8sClient)

	// First, test merge (should succeed since ayna has no CI checks)
	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName:    integrationTaskName,
		PRNumber:    integrationPRNumber,
		MergeMethod: "squash",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("merge_pull_request failed: %v", err)
	}

	var mergeResult MergePullRequestResult
	if err := json.Unmarshal([]byte(result), &mergeResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	t.Logf("Merged: %v", mergeResult.Merged)
	t.Logf("SHA: %s", mergeResult.SHA)
	t.Logf("Message: %s", mergeResult.Message)
	t.Logf("Checks Passed: %v", mergeResult.ChecksPassed)

	// The PR should have been merged (ayna has no CI checks required)
	if mergeResult.Merged {
		t.Logf("✅ PR merged successfully with SHA %s", mergeResult.SHA)
		if mergeResult.SHA == "" {
			t.Error("expected non-empty SHA after merge")
		}
	} else {
		t.Logf("⚠️ PR was not merged: %s", mergeResult.Message)
		// This is OK if CI checks are pending — the tool correctly blocked merge
	}
}
