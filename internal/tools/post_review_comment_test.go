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

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

const testReviewStatus = "submitted"

func TestPostReviewCommentTool_Metadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	if tool.Name() != "post_review_comment" {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description should not be empty")
	}
	params := tool.Parameters()
	if len(params) == 0 {
		t.Error("parameters should not be empty")
	}

	// Verify schema has required fields
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters schema: %v", err)
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("schema missing required field")
	}
	requiredSet := make(map[string]bool)
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	for _, field := range []string{"task_name", "pr_number", "body", "event"} {
		if !requiredSet[field] {
			t.Errorf("expected %q in required fields", field)
		}
	}
}

func TestPostReviewCommentTool_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if !strings.HasSuffix(r.URL.Path, "/pulls/10/reviews") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["body"] != "Looks good overall" {
			t.Errorf("unexpected body: %v", body["body"])
		}
		if body["event"] != "APPROVE" {
			t.Errorf("unexpected event: %v", body["event"])
		}
		// Verify comments are present
		comments, ok := body["comments"].([]any)
		if !ok || len(comments) != 1 {
			t.Errorf("expected 1 comment, got %v", body["comments"])
		}

		w.WriteHeader(200)
		fmt.Fprintf(w, `{"id":101,"html_url":"https://github.com/sozercan/ayna/pull/10#pullrequestreview-101"}`)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "review-task", Namespace: "default"},
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
	tool := &PostReviewCommentTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "review-task",
		PRNumber: 10,
		Body:     "Looks good overall",
		Event:    "APPROVE",
		Comments: []ReviewComment{
			{Path: "main.go", Line: 42, Body: "Nice refactor"},
		},
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var reviewResult PostReviewCommentResult
	if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if reviewResult.ReviewID != 101 {
		t.Errorf("unexpected review ID: %d", reviewResult.ReviewID)
	}
	if reviewResult.Status != testReviewStatus {
		t.Errorf("unexpected status: %s", reviewResult.Status)
	}
	if reviewResult.HTMLURL != "https://github.com/sozercan/ayna/pull/10#pullrequestreview-101" {
		t.Errorf("unexpected HTML URL: %s", reviewResult.HTMLURL)
	}
}

func TestPostReviewCommentTool_WithoutComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)

		// Verify comments are NOT present in the payload
		if _, ok := body["comments"]; ok {
			t.Error("expected comments to be omitted when empty")
		}
		if body["event"] != "COMMENT" {
			t.Errorf("unexpected event: %v", body["event"])
		}

		w.WriteHeader(200)
		fmt.Fprintf(w, `{"id":102,"html_url":"https://github.com/sozercan/ayna/pull/5#pullrequestreview-102"}`)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comment-task", Namespace: "default"},
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
			"password": []byte("pw-token"),
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	tool := &PostReviewCommentTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "comment-task",
		PRNumber: 5,
		Body:     "General feedback",
		Event:    "COMMENT",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var reviewResult PostReviewCommentResult
	if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if reviewResult.ReviewID != 102 {
		t.Errorf("unexpected review ID: %d", reviewResult.ReviewID)
	}
	if reviewResult.Status != testReviewStatus {
		t.Errorf("unexpected status: %s", reviewResult.Status)
	}
}

func TestPostReviewCommentTool_MissingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "nonexistent",
		PRNumber: 1,
		Body:     "review",
		Event:    "COMMENT",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get task") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestPostReviewCommentTool_InvalidEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "some-task",
		PRNumber: 1,
		Body:     "review",
		Event:    "INVALID_EVENT",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for invalid event")
	}
	if got := err.Error(); !strings.Contains(got, "invalid event value") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestPostReviewCommentTool_NoWorkspace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "no-ws-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "no-ws-task",
		PRNumber: 1,
		Body:     "review",
		Event:    "COMMENT",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for task without workspace")
	}
	if got := err.Error(); !strings.Contains(got, "does not have workspace") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestPostReviewCommentTool_Execute_MissingSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-missing-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/sozercan/ayna",
					Branch:  "main",
					GitSecretRef: &corev1.LocalObjectReference{
						Name: "nonexistent-secret",
					},
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	tool := NewPostReviewCommentTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "secret-missing-task",
		PRNumber: 1,
		Body:     "review",
		Event:    "COMMENT",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get git secret") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestPostReviewCommentTool_Execute_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"message":"Internal Server Error"}`)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "api-error-task", Namespace: "default"},
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
	tool := &PostReviewCommentTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(PostReviewCommentArgs{
		TaskName: "api-error-task",
		PRNumber: 10,
		Body:     "review body",
		Event:    "COMMENT",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	if got := err.Error(); !strings.Contains(got, "GitHub API returned 500") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestPostReviewCommentTool_Execute_AllEventTypes(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{name: "approve", event: "APPROVE"},
		{name: "request_changes", event: "REQUEST_CHANGES"},
		{name: "comment", event: "COMMENT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			receivedEvent := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				receivedEvent = body["event"].(string)

				w.WriteHeader(200)
				fmt.Fprintf(w, `{"id":200,"html_url":"https://github.com/sozercan/ayna/pull/3#pullrequestreview-200"}`)
			}))
			defer server.Close()

			scheme := runtime.NewScheme()
			_ = corev1alpha1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "event-task", Namespace: "default"},
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
			tool := &PostReviewCommentTool{
				k8sClient:  k8sClient,
				apiBaseURL: server.URL,
			}

			t.Setenv("MERCAN_TASK_NAMESPACE", "default")

			args, _ := json.Marshal(PostReviewCommentArgs{
				TaskName: "event-task",
				PRNumber: 3,
				Body:     "Review with event " + tc.event,
				Event:    tc.event,
			})

			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("unexpected error for event %s: %v", tc.event, err)
			}

			if receivedEvent != tc.event {
				t.Errorf("expected event %s, got %s", tc.event, receivedEvent)
			}

			var reviewResult PostReviewCommentResult
			if err := json.Unmarshal([]byte(result), &reviewResult); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if reviewResult.ReviewID != 200 {
				t.Errorf("unexpected review ID: %d", reviewResult.ReviewID)
			}
			if reviewResult.Status != testReviewStatus {
				t.Errorf("unexpected status: %s", reviewResult.Status)
			}
		})
	}
}
