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
)

func TestListIssuesTool_Metadata(t *testing.T) {
	tool := NewListIssuesTool(newFakeClient())

	if tool.Name() != "list_issues" {
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
	for _, field := range []string{taskNameField, repoURLField, "unassigned_only", perPageField, pageField} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing %s property", field)
		}
	}
}

func TestListIssuesTool_DefaultUnassigned(t *testing.T) {
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()

		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if accept := r.Header.Get("Accept"); accept != "application/vnd.github+json" {
			t.Errorf("unexpected accept header: %s", accept)
		}
		if version := r.Header.Get("X-GitHub-Api-Version"); version != testAPIVersion {
			t.Errorf("unexpected API version header: %s", version)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
			{
				"number": 10,
				"title": "Bug: something broken",
				"body": "Details about the bug",
				"user": {"login": "alice"},
				"labels": [{"name": "bug"}, {"name": "priority:high"}],
				"created_at": "2025-01-15T10:00:00Z",
				"html_url": "https://github.com/testorg/testrepo/issues/10"
			},
			{
				"number": 11,
				"title": "Feature request",
				"body": "Add new feature",
				"user": {"login": "bob"},
				"labels": [],
				"created_at": "2025-01-16T10:00:00Z",
				"html_url": "https://github.com/testorg/testrepo/issues/11"
			}
		]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify default unassigned filter is applied
	if !strings.Contains(receivedURL, "assignee=none") {
		t.Errorf("expected assignee=none in URL, got: %s", receivedURL)
	}
	if !strings.Contains(receivedURL, "state=open") {
		t.Errorf("expected state=open in URL, got: %s", receivedURL)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.Count != 2 {
		t.Errorf("expected 2 issues, got %d", listResult.Count)
	}
	if len(listResult.Issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(listResult.Issues))
	}

	issue := listResult.Issues[0]
	if issue.Number != 10 {
		t.Errorf("expected issue number 10, got %d", issue.Number)
	}
	if issue.Title != "Bug: something broken" {
		t.Errorf("unexpected title: %s", issue.Title)
	}
	if issue.Author != "alice" {
		t.Errorf("unexpected author: %s", issue.Author)
	}
	if len(issue.Labels) != 2 || issue.Labels[0] != "bug" || issue.Labels[1] != "priority:high" {
		t.Errorf("unexpected labels: %v", issue.Labels)
	}
	if issue.HTMLURL != "https://github.com/testorg/testrepo/issues/10" {
		t.Errorf("unexpected html_url: %s", issue.HTMLURL)
	}
}

func TestListIssuesTool_FiltersPullRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return a mix of issues and PRs (GitHub issues API includes PRs)
		_, _ = fmt.Fprint(w, `[
			{
				"number": 1,
				"title": "Real issue",
				"body": "This is a real issue",
				"user": {"login": "alice"},
				"labels": [],
				"created_at": "2025-01-15T10:00:00Z",
				"html_url": "https://github.com/testorg/testrepo/issues/1"
			},
			{
				"number": 2,
				"title": "This is a PR",
				"body": "PR body",
				"user": {"login": "bob"},
				"labels": [],
				"created_at": "2025-01-16T10:00:00Z",
				"html_url": "https://github.com/testorg/testrepo/pull/2",
				"pull_request": {
					"url": "https://api.github.com/repos/testorg/testrepo/pulls/2"
				}
			},
			{
				"number": 3,
				"title": "Another real issue",
				"body": "Second issue",
				"user": {"login": "charlie"},
				"labels": [{"name": "enhancement"}],
				"created_at": "2025-01-17T10:00:00Z",
				"html_url": "https://github.com/testorg/testrepo/issues/3"
			}
		]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	// Should have 2 issues (PR #2 filtered out)
	if listResult.Count != 2 {
		t.Errorf("expected 2 issues (PR filtered), got %d", listResult.Count)
	}
	for _, issue := range listResult.Issues {
		if issue.Number == 2 {
			t.Error("PR #2 should have been filtered out")
		}
	}
	if listResult.Issues[0].Number != 1 {
		t.Errorf("expected first issue #1, got #%d", listResult.Issues[0].Number)
	}
	if listResult.Issues[1].Number != 3 {
		t.Errorf("expected second issue #3, got #%d", listResult.Issues[1].Number)
	}
}

func TestListIssuesTool_UnassignedOnlyFalse(t *testing.T) {
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	unassigned := false
	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL:        testOrgTestRepoURL,
		UnassignedOnly: &unassigned,
	})

	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should NOT have assignee=none when unassigned_only is explicitly false
	if strings.Contains(receivedURL, "assignee=none") {
		t.Errorf("should not have assignee=none when unassigned_only=false, got: %s", receivedURL)
	}
}

func TestListIssuesTool_BodyTruncation(t *testing.T) {
	longBody := strings.Repeat("x", 600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := fmt.Sprintf(`[{
			"number": 1,
			"title": "Long body issue",
			"body": %q,
			"user": {"login": "alice"},
			"labels": [],
			"created_at": "2025-01-15T10:00:00Z",
			"html_url": "https://github.com/testorg/testrepo/issues/1"
		}]`, longBody)
		_, _ = fmt.Fprint(w, resp)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if len(listResult.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(listResult.Issues))
	}
	expectedLen := maxIssueBodyLength + len("...")
	if len([]rune(listResult.Issues[0].Body)) != expectedLen {
		t.Errorf("expected body to be %d runes (truncated + ...), got %d", expectedLen, len([]rune(listResult.Issues[0].Body)))
	}
}

func TestListIssuesTool_Pagination(t *testing.T) {
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
		PerPage: 50,
		Page:    3,
	})

	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(receivedURL, "per_page=50") {
		t.Errorf("expected per_page=50 in URL, got: %s", receivedURL)
	}
	if !strings.Contains(receivedURL, "page=3") {
		t.Errorf("expected page=3 in URL, got: %s", receivedURL)
	}
}

func TestListIssuesTool_PerPageCappedAt100(t *testing.T) {
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
		PerPage: 200,
	})

	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(receivedURL, "per_page=100") {
		t.Errorf("expected per_page capped to 100, got: %s", receivedURL)
	}
}

func TestListIssuesTool_GitHubAPI404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

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

func TestListIssuesTool_NoRepoURL(t *testing.T) {
	// Clear all repo sources
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient: newFakeClient(),
	}

	args, _ := json.Marshal(ListIssuesArgs{})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when no repo URL is available")
	}
	if !strings.Contains(err.Error(), "no repo_url, task_name, or ORKA_GIT_REPO provided") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestListIssuesTool_WithRepoURL(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 5,
			"title": "Test issue",
			"body": "Body text",
			"user": {"login": "tester"},
			"labels": [{"name": "good first issue"}],
			"created_at": "2025-02-01T12:00:00Z",
			"html_url": "https://github.com/myorg/myrepo/issues/5"
		}]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testMyOrgRepoURL,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify correct path is called
	if receivedPath != "/repos/myorg/myrepo/issues" {
		t.Errorf("expected /repos/myorg/myrepo/issues, got: %s", receivedPath)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.Count != 1 {
		t.Errorf("expected 1 issue, got %d", listResult.Count)
	}
	if listResult.Issues[0].Number != 5 {
		t.Errorf("expected issue #5, got #%d", listResult.Issues[0].Number)
	}
	if listResult.Issues[0].Author != "tester" {
		t.Errorf("unexpected author: %s", listResult.Issues[0].Author)
	}
	if len(listResult.Issues[0].Labels) != 1 || listResult.Issues[0].Labels[0] != "good first issue" {
		t.Errorf("unexpected labels: %v", listResult.Issues[0].Labels)
	}
}

func TestListIssuesTool_DefaultPagination(t *testing.T) {
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	// No per_page or page specified — should default to 30 and 1
	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(receivedURL, "per_page=30") {
		t.Errorf("expected default per_page=30 in URL, got: %s", receivedURL)
	}
	if !strings.Contains(receivedURL, "page=1") {
		t.Errorf("expected default page=1 in URL, got: %s", receivedURL)
	}
}

func TestListIssuesTool_EmptyLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 1,
			"title": "No labels",
			"body": "",
			"user": {"login": "alice"},
			"labels": [],
			"created_at": "2025-01-15T10:00:00Z",
			"html_url": "https://github.com/testorg/testrepo/issues/1"
		}]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if len(listResult.Issues[0].Labels) != 0 {
		t.Errorf("expected empty labels, got: %v", listResult.Issues[0].Labels)
	}
}

func TestListIssuesTool_HasNextPageTrueWhenFullPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return 3 items (matching per_page=3) with one being a PR
		_, _ = fmt.Fprint(w, `[
			{"number":1,"title":"Issue 1","body":"","user":{"login":"a"},"labels":[],"created_at":"2025-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/1"},
			{"number":2,"title":"PR","body":"","user":{"login":"b"},"labels":[],"created_at":"2025-01-02T00:00:00Z","html_url":"https://github.com/o/r/pull/2","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/2"}},
			{"number":3,"title":"Issue 3","body":"","user":{"login":"c"},"labels":[],"created_at":"2025-01-03T00:00:00Z","html_url":"https://github.com/o/r/issues/3"}
		]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: "https://github.com/o/r",
		PerPage: 3,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if listResult.Count != 2 {
		t.Errorf("expected 2 issues after PR filtering, got %d", listResult.Count)
	}
	if !listResult.HasNextPage {
		t.Error("expected HasNextPage to be true when raw API response is a full page")
	}
}

func TestListIssuesTool_HasNextPageFalseWhenPartialPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return 2 items when per_page=5 → partial page
		_, _ = fmt.Fprint(w, `[
			{"number":1,"title":"Issue 1","body":"","user":{"login":"a"},"labels":[],"created_at":"2025-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/1"},
			{"number":2,"title":"Issue 2","body":"","user":{"login":"b"},"labels":[],"created_at":"2025-01-02T00:00:00Z","html_url":"https://github.com/o/r/issues/2"}
		]`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: "https://github.com/o/r",
		PerPage: 5,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if listResult.Count != 2 {
		t.Errorf("expected 2 issues, got %d", listResult.Count)
	}
	if listResult.HasNextPage {
		t.Error("expected HasNextPage to be false when API returns fewer items than perPage")
	}
}

func TestListIssuesTool_BodyTruncationMultiByteUTF8(t *testing.T) {
	// 498 ASCII chars + 2 emoji (4 bytes each) = 500 runes but 506 bytes.
	// Byte-based slicing at 500 would split inside the first emoji.
	multiByteBody := strings.Repeat("a", 498) + "🎉🎉"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := fmt.Sprintf(`[{
			"number": 1,
			"title": "Emoji body issue",
			"body": %q,
			"user": {"login": "alice"},
			"labels": [],
			"created_at": "2025-01-15T10:00:00Z",
			"html_url": "https://github.com/testorg/testrepo/issues/1"
		}]`, multiByteBody)
		_, _ = fmt.Fprint(w, resp)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &ListIssuesTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(ListIssuesArgs{
		RepoURL: testOrgTestRepoURL,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult ListIssuesResult
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if len(listResult.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(listResult.Issues))
	}

	body := listResult.Issues[0].Body
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
