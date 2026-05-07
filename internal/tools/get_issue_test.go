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
)

const testAuthorAlice = "alice"

func TestGetIssueTool_Metadata(t *testing.T) {
	tool := NewGetIssueTool(nil)

	if tool.Name() != "get_issue" {
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
	props := schema[jsonSchemaPropertiesField].(map[string]any)
	for _, field := range []string{taskNameField, repoURLField, githubIssueNumberField} {
		if _, ok := props[field]; !ok {
			t.Errorf("parameters should contain %s", field)
		}
	}
	required, ok := schema[jsonSchemaRequiredField].([]any)
	if !ok {
		t.Fatal("schema missing required field")
	}
	if len(required) != 1 || required[0].(string) != githubIssueNumberField {
		t.Errorf("expected required=[issue_number], got %v", required)
	}
}

func TestGetIssueTool_FullDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if r.Header.Get("X-GitHub-Api-Version") != testAPIVersion {
			t.Errorf("unexpected API version header: %s", r.Header.Get("X-GitHub-Api-Version"))
		}

		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/42") && !strings.Contains(r.URL.Path, "/comments"):
			_, _ = fmt.Fprint(w, `{
				"number": 42,
				"title": "Fix the bug",
				"body": "This is the full body of the issue.",
				"state": "open",
				"html_url": "https://github.com/testorg/testrepo/issues/42",
				"comments": 2,
				"user": {"login": "alice"},
				"labels": [{"name": "bug"}, {"name": "priority:high"}],
				"assignees": [{"login": "bob"}, {"login": "charlie"}],
				"created_at": "2025-01-15T10:00:00Z"
			}`)
		case strings.HasSuffix(r.URL.Path, "/issues/42/comments"):
			if r.URL.Query().Get(perPageField) != "30" {
				t.Errorf("expected per_page=30, got %s", r.URL.Query().Get(perPageField))
			}
			_, _ = fmt.Fprint(w, `[
				{"user": {"login": "dave"}, "body": "I can reproduce this.", "created_at": "2025-01-15T11:00:00Z"},
				{"user": {"login": "alice"}, "body": "Thanks for confirming.", "created_at": "2025-01-15T12:00:00Z"}
			]`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 42,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var issue GetIssueResult
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if issue.Number != 42 {
		t.Errorf("number = %d, want 42", issue.Number)
	}
	if issue.Title != "Fix the bug" {
		t.Errorf("title = %q, want %q", issue.Title, "Fix the bug")
	}
	if issue.Body != "This is the full body of the issue." {
		t.Errorf("body = %q, want full body", issue.Body)
	}
	if issue.Author != testAuthorAlice {
		t.Errorf("author = %q, want alice", issue.Author)
	}
	if issue.State != "open" {
		t.Errorf("state = %q, want open", issue.State)
	}
	if issue.CreatedAt != "2025-01-15T10:00:00Z" {
		t.Errorf("created_at = %q", issue.CreatedAt)
	}
	if issue.HTMLURL != "https://github.com/testorg/testrepo/issues/42" {
		t.Errorf("html_url = %q", issue.HTMLURL)
	}
	if issue.CommentCount != 2 {
		t.Errorf("comment_count = %d, want 2", issue.CommentCount)
	}
	if len(issue.Comments) != 2 {
		t.Fatalf("comments len = %d, want 2", len(issue.Comments))
	}
	if issue.Comments[0].Author != "dave" {
		t.Errorf("comment[0].author = %q, want dave", issue.Comments[0].Author)
	}
	if issue.Comments[0].Body != "I can reproduce this." {
		t.Errorf("comment[0].body = %q", issue.Comments[0].Body)
	}
	if issue.Comments[1].Author != testAuthorAlice {
		t.Errorf("comment[1].author = %q, want alice", issue.Comments[1].Author)
	}
}

func TestGetIssueTool_NoComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/10") && !strings.Contains(r.URL.Path, "/comments"):
			_, _ = fmt.Fprint(w, `{
				"number": 10,
				"title": "Simple issue",
				"body": "No comments here.",
				"state": "closed",
				"html_url": "https://github.com/org/repo/issues/10",
				"comments": 0,
				"user": {"login": "author1"},
				"labels": [],
				"assignees": [],
				"created_at": "2025-02-01T08:00:00Z"
			}`)
		case strings.HasSuffix(r.URL.Path, "/issues/10/comments"):
			fmt.Fprint(w, `[]`) //nolint:errcheck
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		RepoURL:     testOrgRepoURL,
		IssueNumber: 10,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var issue GetIssueResult
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if issue.Number != 10 {
		t.Errorf("number = %d, want 10", issue.Number)
	}
	if issue.State != "closed" {
		t.Errorf("state = %q, want closed", issue.State)
	}
	if issue.CommentCount != 0 {
		t.Errorf("comment_count = %d, want 0", issue.CommentCount)
	}
	if len(issue.Comments) != 0 {
		t.Errorf("comments len = %d, want 0", len(issue.Comments))
	}
	if len(issue.Labels) != 0 {
		t.Errorf("labels len = %d, want 0", len(issue.Labels))
	}
	if len(issue.Assignees) != 0 {
		t.Errorf("assignees len = %d, want 0", len(issue.Assignees))
	}
}

func TestGetIssueTool_IssueNumberRequired(t *testing.T) {
	tool := NewGetIssueTool(nil)

	// Test with issue_number = 0 (default)
	args, _ := json.Marshal(GetIssueArgs{
		RepoURL: testOrgRepoURL,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for missing issue_number")
	}
	if !strings.Contains(err.Error(), "issue_number is required") {
		t.Errorf("got error %q, want it to contain 'issue_number is required'", err.Error())
	}

	// Test with negative issue_number
	args, _ = json.Marshal(GetIssueArgs{
		RepoURL:     testOrgRepoURL,
		IssueNumber: -1,
	})

	_, err = tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for negative issue_number")
	}
	if !strings.Contains(err.Error(), "issue_number is required") {
		t.Errorf("got error %q, want it to contain 'issue_number is required'", err.Error())
	}
}

func TestGetIssueTool_GitHub404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		RepoURL:     testOrgRepoURL,
		IssueNumber: 9999,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("got error %q, want it to contain '404'", err.Error())
	}
}

func TestGetIssueTool_NoRepoURL(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	t.Setenv("ORKA_GIT_REPO", "")

	tool := NewGetIssueTool(nil)

	args, _ := json.Marshal(GetIssueArgs{
		IssueNumber: 1,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when no repo URL available")
	}
	if !strings.Contains(err.Error(), "no repo_url, task_name, or ORKA_GIT_REPO provided") {
		t.Errorf("got error %q, want repo resolution error", err.Error())
	}
}

func TestGetIssueTool_WithRepoURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/comments") {
			fmt.Fprint(w, `[]`) //nolint:errcheck
			return
		}
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{
			"number": 5,
			"title": "Test",
			"body": "body",
			"state": "open",
			"html_url": "https://github.com/myorg/myrepo/issues/5",
			"comments": 0,
			"user": {"login": "user1"},
			"labels": [],
			"assignees": [],
			"created_at": "2025-03-01T00:00:00Z"
		}`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		RepoURL:     testMyOrgRepoURL,
		IssueNumber: 5,
	})

	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "/repos/myorg/myrepo/issues/5"
	if gotPath != expected {
		t.Errorf("request path = %q, want %q", gotPath, expected)
	}
}

func TestGetIssueTool_CommentsEndpointFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/7") && !strings.Contains(r.URL.Path, "/comments"):
			_, _ = fmt.Fprint(w, `{
				"number": 7,
				"title": "Issue with broken comments",
				"body": "body text",
				"state": "open",
				"html_url": "https://github.com/org/repo/issues/7",
				"comments": 3,
				"user": {"login": "author"},
				"labels": [{"name": "help wanted"}],
				"assignees": [{"login": "dev1"}],
				"created_at": "2025-04-01T00:00:00Z"
			}`)
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			w.WriteHeader(500)
			fmt.Fprint(w, `{"message":"Internal Server Error"}`) //nolint:errcheck
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		RepoURL:     testOrgRepoURL,
		IssueNumber: 7,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected no error when comments fail, got: %v", err)
	}

	var issue GetIssueResult
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	// Issue details should still be returned
	if issue.Number != 7 {
		t.Errorf("number = %d, want 7", issue.Number)
	}
	if issue.Title != "Issue with broken comments" {
		t.Errorf("title = %q", issue.Title)
	}
	// Comments should be empty (graceful degradation)
	if len(issue.Comments) != 0 {
		t.Errorf("comments len = %d, want 0 (graceful degradation)", len(issue.Comments))
	}
	// But comment_count from the issue itself should still be 3
	if issue.CommentCount != 3 {
		t.Errorf("comment_count = %d, want 3", issue.CommentCount)
	}
}

func TestGetIssueTool_LabelsAndAssignees(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/comments") {
			fmt.Fprint(w, `[]`) //nolint:errcheck
			return
		}
		_, _ = fmt.Fprint(w, `{
			"number": 20,
			"title": "Feature request",
			"body": "Please add this feature.",
			"state": "open",
			"html_url": "https://github.com/org/repo/issues/20",
			"comments": 0,
			"user": {"login": "requester"},
			"labels": [
				{"name": "enhancement"},
				{"name": "good first issue"},
				{"name": "area/ui"}
			],
			"assignees": [
				{"login": "dev1"},
				{"login": "dev2"},
				{"login": "dev3"}
			],
			"created_at": "2025-05-01T09:00:00Z"
		}`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		RepoURL:     testOrgRepoURL,
		IssueNumber: 20,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var issue GetIssueResult
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	expectedLabels := []string{"enhancement", "good first issue", "area/ui"}
	if len(issue.Labels) != len(expectedLabels) {
		t.Fatalf("labels len = %d, want %d", len(issue.Labels), len(expectedLabels))
	}
	for i, l := range expectedLabels {
		if issue.Labels[i] != l {
			t.Errorf("labels[%d] = %q, want %q", i, issue.Labels[i], l)
		}
	}

	expectedAssignees := []string{"dev1", "dev2", "dev3"}
	if len(issue.Assignees) != len(expectedAssignees) {
		t.Fatalf("assignees len = %d, want %d", len(issue.Assignees), len(expectedAssignees))
	}
	for i, a := range expectedAssignees {
		if issue.Assignees[i] != a {
			t.Errorf("assignees[%d] = %q, want %q", i, issue.Assignees[i], a)
		}
	}
}

func TestGetIssueTool_EnvVarRepoFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/comments") {
			fmt.Fprint(w, `[]`) //nolint:errcheck
			return
		}
		// Verify it resolved from ORKA_GIT_REPO
		if !strings.Contains(r.URL.Path, "/repos/envorg/envrepo/issues/1") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{
			"number": 1,
			"title": "Env issue",
			"body": "From env",
			"state": "open",
			"html_url": "https://github.com/envorg/envrepo/issues/1",
			"comments": 0,
			"user": {"login": "envuser"},
			"labels": [],
			"assignees": [],
			"created_at": "2025-06-01T00:00:00Z"
		}`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	t.Setenv("ORKA_GIT_REPO", "https://github.com/envorg/envrepo")

	tool := &GetIssueTool{apiBaseURL: server.URL}

	args, _ := json.Marshal(GetIssueArgs{
		IssueNumber: 1,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var issue GetIssueResult
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if issue.Number != 1 {
		t.Errorf("number = %d, want 1", issue.Number)
	}
}
