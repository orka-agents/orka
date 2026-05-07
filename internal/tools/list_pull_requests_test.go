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
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestListPullRequestsTool_Metadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := NewListPullRequestsTool(k8sClient)

	if tool.Name() != "list_pull_requests" {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description should not be empty")
	}
	params := tool.Parameters()
	if len(params) == 0 {
		t.Error("parameters should not be empty")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters schema: %v", err)
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, field := range []string{taskNameField, repoURLField, perPageField, pageField} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing %s property", field)
		}
	}
}

func TestListPullRequestsTool_ListOpenPRs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if !strings.Contains(r.URL.Path, "/repos/sozercan/ayna/pulls") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("expected state=open, got %s", r.URL.Query().Get("state"))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
			{
				"number": 10,
				"title": "feat: add search",
				"body": "Implements search functionality",
				"html_url": "https://github.com/sozercan/ayna/pull/10",
				"draft": false,
				"user": {"login": "alice"},
				"base": {"ref": "main"},
				"head": {"ref": "feature/search"},
				"labels": [{"name": "enhancement"}, {"name": "review-needed"}],
				"created_at": "2025-01-15T10:00:00Z"
			},
			{
				"number": 11,
				"title": "fix: typo in readme",
				"body": "Fixes a typo",
				"html_url": "https://github.com/sozercan/ayna/pull/11",
				"draft": false,
				"user": {"login": "bob"},
				"base": {"ref": "main"},
				"head": {"ref": "fix/typo"},
				"labels": [],
				"created_at": "2025-01-16T12:00:00Z"
			}
		]`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{
		k8sClient:  nil,
		apiBaseURL: server.URL,
	}

	t.Setenv("ORKA_GIT_REPO", testSozercanAynaRepoURL)
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.TotalCount != 2 {
		t.Errorf("expected 2 PRs, got %d", listResult.TotalCount)
	}

	pr := listResult.PullRequests[0]
	if pr.Number != 10 {
		t.Errorf("expected PR #10, got #%d", pr.Number)
	}
	if pr.Title != "feat: add search" {
		t.Errorf("unexpected title: %s", pr.Title)
	}
	if pr.Author != "alice" {
		t.Errorf("unexpected author: %s", pr.Author)
	}
	if pr.BaseBranch != testBranch {
		t.Errorf("unexpected base branch: %s", pr.BaseBranch)
	}
	if pr.HeadBranch != "feature/search" {
		t.Errorf("unexpected head branch: %s", pr.HeadBranch)
	}
	if len(pr.Labels) != 2 || pr.Labels[0] != "enhancement" {
		t.Errorf("unexpected labels: %v", pr.Labels)
	}
	if pr.HTMLURL != "https://github.com/sozercan/ayna/pull/10" {
		t.Errorf("unexpected html_url: %s", pr.HTMLURL)
	}
	if pr.Draft {
		t.Error("expected non-draft PR")
	}
}

func TestListPullRequestsTool_BodyTruncation(t *testing.T) {
	longBody := strings.Repeat("a", 600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{
			"number": 1,
			"title": "long body PR",
			"body": %q,
			"html_url": "https://github.com/o/r/pull/1",
			"draft": false,
			"user": {"login": "user"},
			"base": {"ref": "main"},
			"head": {"ref": "dev"},
			"labels": [],
			"created_at": "2025-01-01T00:00:00Z"
		}]`, longBody)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/r")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if len(listResult.PullRequests) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(listResult.PullRequests))
	}
	expectedLen := 500 + len("...")
	if len([]rune(listResult.PullRequests[0].Body)) != expectedLen {
		t.Errorf("expected body to be %d runes (truncated + ...), got %d", expectedLen, len([]rune(listResult.PullRequests[0].Body)))
	}
}

func TestListPullRequestsTool_Pagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perPage := r.URL.Query().Get(perPageField)
		page := r.URL.Query().Get(pageField)
		if perPage != "5" {
			t.Errorf("expected per_page=5, got %s", perPage)
		}
		if page != "2" {
			t.Errorf("expected page=2, got %s", page)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 6,
			"title": "page 2 PR",
			"body": "",
			"html_url": "https://github.com/o/r/pull/6",
			"draft": false,
			"user": {"login": "user"},
			"base": {"ref": "main"},
			"head": {"ref": "feat"},
			"labels": [],
			"created_at": "2025-01-01T00:00:00Z"
		}]`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/r")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{PerPage: 5, Page: 2})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.TotalCount != 1 {
		t.Errorf("expected 1 PR, got %d", listResult.TotalCount)
	}
	if listResult.PullRequests[0].Number != 6 {
		t.Errorf("expected PR #6, got #%d", listResult.PullRequests[0].Number)
	}
}

func TestListPullRequestsTool_DraftPRs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
			{
				"number": 20,
				"title": "WIP: draft feature",
				"body": "Work in progress",
				"html_url": "https://github.com/o/r/pull/20",
				"draft": true,
				"user": {"login": "dev"},
				"base": {"ref": "main"},
				"head": {"ref": "wip/feature"},
				"labels": [{"name": "WIP"}],
				"created_at": "2025-02-01T00:00:00Z"
			},
			{
				"number": 21,
				"title": "Ready PR",
				"body": "Ready for review",
				"html_url": "https://github.com/o/r/pull/21",
				"draft": false,
				"user": {"login": "dev2"},
				"base": {"ref": "main"},
				"head": {"ref": "feature/ready"},
				"labels": [],
				"created_at": "2025-02-02T00:00:00Z"
			}
		]`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/r")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.TotalCount != 2 {
		t.Fatalf("expected 2 PRs, got %d", listResult.TotalCount)
	}
	if !listResult.PullRequests[0].Draft {
		t.Error("expected first PR to be a draft")
	}
	if listResult.PullRequests[1].Draft {
		t.Error("expected second PR to not be a draft")
	}
}

func TestListPullRequestsTool_APIError404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/nonexistent")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected error to mention 404, got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("expected error to mention Not Found, got: %s", err.Error())
	}
}

func TestListPullRequestsTool_NoRepoURL(t *testing.T) {
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListPullRequestsTool{}

	args, _ := json.Marshal(ListPullRequestsArgs{})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when no repo URL is available")
	}
	if !strings.Contains(err.Error(), "no repo_url, task_name, or ORKA_GIT_REPO provided") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestListPullRequestsTool_WithRepoURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repos/myorg/myrepo/pulls") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 5,
			"title": "test PR",
			"body": "testing repo_url param",
			"html_url": "https://github.com/myorg/myrepo/pull/5",
			"draft": false,
			"user": {"login": "tester"},
			"base": {"ref": "main"},
			"head": {"ref": "test-branch"},
			"labels": [{"name": "test"}],
			"created_at": "2025-03-01T00:00:00Z"
		}]`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{
		RepoURL: testMyOrgRepoURL,
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.TotalCount != 1 {
		t.Errorf("expected 1 PR, got %d", listResult.TotalCount)
	}
	if listResult.PullRequests[0].Author != "tester" {
		t.Errorf("unexpected author: %s", listResult.PullRequests[0].Author)
	}
}

func TestListPullRequestsTool_EmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/r")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.TotalCount != 0 {
		t.Errorf("expected 0 PRs, got %d", listResult.TotalCount)
	}
	if len(listResult.PullRequests) != 0 {
		t.Errorf("expected empty pull_requests, got %d", len(listResult.PullRequests))
	}
}

func TestListPullRequestsTool_WithTaskName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 99,
			"title": "task-based PR",
			"body": "",
			"html_url": "https://github.com/sozercan/ayna/pull/99",
			"draft": false,
			"user": {"login": "taskuser"},
			"base": {"ref": "main"},
			"head": {"ref": "task-branch"},
			"labels": [],
			"created_at": "2025-04-01T00:00:00Z"
		}]`)
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
	tool := &ListPullRequestsTool{
		k8sClient:  k8sClient,
		apiBaseURL: server.URL,
	}

	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	args, _ := json.Marshal(ListPullRequestsArgs{
		TaskName: testCoderTaskName,
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.TotalCount != 1 {
		t.Errorf("expected 1 PR, got %d", listResult.TotalCount)
	}
	if listResult.PullRequests[0].Number != 99 {
		t.Errorf("expected PR #99, got #%d", listResult.PullRequests[0].Number)
	}
}

func TestListPullRequestsTool_BodyTruncationMultiByteUTF8(t *testing.T) {
	// 498 ASCII chars + 2 emoji (4 bytes each) = 500 runes but 506 bytes.
	// Byte-based slicing at 500 would split inside the first emoji.
	multiByteBody := strings.Repeat("a", 498) + "🎉🎉"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{
			"number": 1,
			"title": "Emoji body PR",
			"body": %q,
			"html_url": "https://github.com/o/r/pull/1",
			"draft": false,
			"user": {"login": "user"},
			"base": {"ref": "main"},
			"head": {"ref": "dev"},
			"labels": [],
			"created_at": "2025-01-01T00:00:00Z"
		}]`, multiByteBody)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/r")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListPullRequestsResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if len(listResult.PullRequests) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(listResult.PullRequests))
	}

	body := listResult.PullRequests[0].Body
	if !utf8.ValidString(body) {
		t.Error("truncated body is not valid UTF-8")
	}
	if strings.Contains(body, "\ufffd") {
		t.Error("truncated body contains replacement character U+FFFD")
	}
	// 500 runes exactly — should not be truncated
	if len([]rune(body)) != 500 {
		t.Errorf("expected 500 runes (no truncation needed), got %d", len([]rune(body)))
	}
}

func TestListPullRequestsTool_PerPageCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perPage := r.URL.Query().Get(perPageField)
		if perPage != "100" {
			t.Errorf("expected per_page capped to 100, got %s", perPage)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	tool := &ListPullRequestsTool{apiBaseURL: server.URL}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/o/r")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	args, _ := json.Marshal(ListPullRequestsArgs{PerPage: 200})
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
