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

const testAPIVersion = "2022-11-28"

func TestCommentOnIssueTool_Metadata(t *testing.T) {
	tool := NewCommentOnIssueTool(newFakeClient())

	if tool.Name() != "comment_on_issue" {
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
	required, ok := schema[jsonSchemaRequiredField].([]any)
	if !ok {
		t.Fatal("schema missing required field")
	}
	requiredSet := make(map[string]bool)
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	for _, field := range []string{githubIssueNumberField, githubBodyField} {
		if !requiredSet[field] {
			t.Errorf("expected %q in required fields", field)
		}
	}

	// Verify all properties exist
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, field := range []string{taskNameField, repoURLField, githubIssueNumberField, githubBodyField} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing %s property", field)
		}
	}
}

func TestCommentOnIssueTool_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != testBearerToken {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if accept := r.Header.Get("Accept"); accept != "application/vnd.github+json" {
			t.Errorf("unexpected accept header: %s", accept)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("unexpected content-type header: %s", ct)
		}
		if version := r.Header.Get("X-GitHub-Api-Version"); version != testAPIVersion {
			t.Errorf("unexpected API version header: %s", version)
		}
		if !strings.HasSuffix(r.URL.Path, "/issues/42/comments") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if body[githubBodyField] != "🤖 Agent is working on this issue" {
			t.Errorf("unexpected body: %v", body[githubBodyField])
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":12345,"html_url":"https://github.com/testorg/testrepo/issues/42#issuecomment-12345"}`) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 42,
		Body:        "🤖 Agent is working on this issue",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var commentResult CommentOnIssueResult
	if err := json.Unmarshal([]byte(result), &commentResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if commentResult.CommentID != 12345 {
		t.Errorf("expected comment ID 12345, got %d", commentResult.CommentID)
	}
	if commentResult.Status != statusCreated {
		t.Errorf("expected status 'created', got %s", commentResult.Status)
	}
	if commentResult.HTMLURL != "https://github.com/testorg/testrepo/issues/42#issuecomment-12345" {
		t.Errorf("unexpected HTML URL: %s", commentResult.HTMLURL)
	}
}

func TestCommentOnIssueTool_IssueNumberZero(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient: newFakeClient(),
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 0,
		Body:        testCommentBody,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for issue_number=0")
	}
	if !strings.Contains(err.Error(), "issue_number is required") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCommentOnIssueTool_IssueNumberNegative(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient: newFakeClient(),
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: -1,
		Body:        testCommentBody,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for negative issue_number")
	}
	if !strings.Contains(err.Error(), "issue_number is required") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCommentOnIssueTool_EmptyBody(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient: newFakeClient(),
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 1,
		Body:        "",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	if !strings.Contains(err.Error(), "body is required") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCommentOnIssueTool_GitHub404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"Not Found"}`) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 9999,
		Body:        "comment on missing issue",
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

func TestCommentOnIssueTool_GitHub422(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(w, `{"message":"Validation Failed","errors":[{"resource":"IssueComment","code":"unprocessable"}]}`) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 1,
		Body:        testCommentBody,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("expected error to mention 422, got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("expected error to mention Validation Failed, got: %s", err.Error())
	}
}

func TestCommentOnIssueTool_NoRepoURL(t *testing.T) {
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient: newFakeClient(),
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		IssueNumber: 1,
		Body:        "comment",
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when no repo URL is available")
	}
	if !strings.Contains(err.Error(), "no repo_url, task_name, or ORKA_GIT_REPO provided") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

func TestCommentOnIssueTool_WithRepoURL(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":999,"html_url":"https://github.com/myorg/myrepo/issues/7#issuecomment-999"}`) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testMyOrgRepoURL,
		IssueNumber: 7,
		Body:        "status update",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedPath != "/repos/myorg/myrepo/issues/7/comments" {
		t.Errorf("expected /repos/myorg/myrepo/issues/7/comments, got: %s", receivedPath)
	}

	var commentResult CommentOnIssueResult
	if err := json.Unmarshal([]byte(result), &commentResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if commentResult.CommentID != 999 {
		t.Errorf("expected comment ID 999, got %d", commentResult.CommentID)
	}
	if commentResult.Status != statusCreated {
		t.Errorf("expected status 'created', got %s", commentResult.Status)
	}
}

func TestCommentOnIssueTool_EmojiMarkdownContent(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":555,"html_url":"https://github.com/testorg/testrepo/issues/3#issuecomment-555"}`) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)

	tool := &CommentOnIssueTool{
		k8sClient:  newFakeClient(),
		apiBaseURL: server.URL,
	}

	markdownBody := "## 🤖 Agent Status Update\n\n" +
		"- ✅ Code analysis complete\n" +
		"- 🔧 Fix applied in `main.go`\n" +
		"- 🚀 PR #42 created\n\n" +
		"```go\nfmt.Println(\"Hello, world!\")\n```"

	args, _ := json.Marshal(CommentOnIssueArgs{
		RepoURL:     testOrgTestRepoURL,
		IssueNumber: 3,
		Body:        markdownBody,
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the full markdown body was sent to GitHub
	if receivedBody[githubBodyField] != markdownBody {
		t.Errorf("expected markdown body to be preserved, got: %v", receivedBody[githubBodyField])
	}

	var commentResult CommentOnIssueResult
	if err := json.Unmarshal([]byte(result), &commentResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if commentResult.CommentID != 555 {
		t.Errorf("expected comment ID 555, got %d", commentResult.CommentID)
	}
	if commentResult.Status != statusCreated {
		t.Errorf("expected status 'created', got %s", commentResult.Status)
	}
}
