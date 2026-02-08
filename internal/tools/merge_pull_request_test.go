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

func TestMergePullRequestTool_Metadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewMergePullRequestTool(k8sClient)

	if tool.Name() != "merge_pull_request" {
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
	props := schema["properties"].(map[string]any)
	if _, ok := props["task_name"]; !ok {
		t.Error("parameters should contain task_name")
	}
	if _, ok := props["pr_number"]; !ok {
		t.Error("parameters should contain pr_number")
	}
	if _, ok := props["merge_method"]; !ok {
		t.Error("parameters should contain merge_method")
	}
}

func TestMergePullRequestTool_MissingTask(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(MergePullRequestArgs{
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

func TestMergePullRequestTool_MissingSecret(t *testing.T) {
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
	tool := NewMergePullRequestTool(k8sClient)

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(MergePullRequestArgs{
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
	tool := &MergePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName: "coder-task",
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
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
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
			json.NewDecoder(r.Body).Decode(&body)
			if body["merge_method"] != "squash" {
				t.Errorf("unexpected merge_method: %v", body["merge_method"])
			}
			if body["commit_title"] != "feat: awesome feature (#42)" {
				t.Errorf("unexpected commit_title: %v", body["commit_title"])
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
	tool := &MergePullRequestTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	args, _ := json.Marshal(MergePullRequestArgs{
		TaskName:      "coder-task",
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
	if mergeResult.Message != "Pull request merged successfully" {
		t.Errorf("unexpected message: %s", mergeResult.Message)
	}
}
