/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommentOnIssueToolMarksOnlyDefiniteFailuresAsNoEffect(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		body        string
		wantSummary string
	}{
		{
			name: "missing permission", status: http.StatusForbidden,
			body:        `{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest","status":"403"}`,
			wantSummary: "GitHub refused the request (HTTP 403): Resource not accessible by integration",
		},
		{
			name: "validation", status: http.StatusUnprocessableEntity,
			body:        `{"message":"Validation Failed","errors":[{"resource":"IssueComment","code":"custom","message":"body is too long"}]}`,
			wantSummary: "GitHub refused the request (HTTP 422): Validation Failed; body is too long",
		},
		{name: "unparsed body", status: http.StatusNotFound, body: `not json`, wantSummary: "GitHub refused the request (HTTP 404)"},
		{name: "request timeout", status: http.StatusRequestTimeout, body: `{"message":"Timeout"}`},
		{name: "server error", status: http.StatusBadGateway, body: `{"message":"Bad Gateway"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			t.Setenv("GITHUB_TOKEN", testGitHubToken)
			tool := &CommentOnIssueTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
			args, _ := json.Marshal(CommentOnIssueArgs{RepoURL: testOrgTestRepoURL, IssueNumber: 1, Body: testCommentBody})

			_, err := tool.Execute(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "GitHub API returned") || !strings.Contains(err.Error(), test.body) {
				t.Fatalf("Execute() error = %v, want the full GitHub response kept for logs", err)
			}
			noEffect, ok := errors.AsType[*NoEffectError](err)
			if test.wantSummary == "" {
				if ok {
					t.Fatalf("HTTP %d was marked as having no effect: %q", test.status, noEffect.Summary)
				}
				return
			}
			if !ok || noEffect.Summary != test.wantSummary {
				t.Fatalf("NoEffectError = %#v, want summary %q", noEffect, test.wantSummary)
			}
		})
	}
}

func TestCommentOnIssueToolKeepsRefusalsAfterRedirectsUncertain(t *testing.T) {
	// The POST may have taken effect before redirecting to a result the
	// token may not read.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/testorg/testrepo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/result", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /result", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CommentOnIssueTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
	args, _ := json.Marshal(CommentOnIssueArgs{RepoURL: testOrgTestRepoURL, IssueNumber: 1, Body: testCommentBody})

	_, err := tool.Execute(context.Background(), args)
	if _, ok := errors.AsType[*NoEffectError](err); err == nil || !strings.Contains(err.Error(), "403") || ok {
		t.Fatalf("Execute() error = %v, want an uncertain 403 after a redirect", err)
	}
}

func TestGitHubRefusalSummaryKeepsTheWholeMessage(t *testing.T) {
	// The caller redacts the summary before bounding it, so it must arrive whole.
	message := strings.Repeat("m", 2*maxGitHubErrorNoteBytes)
	body, _ := json.Marshal(map[string]string{"message": message})
	err := githubStatusError(&http.Response{StatusCode: http.StatusForbidden, Request: &http.Request{}}, body)
	if noEffect, ok := errors.AsType[*NoEffectError](err); !ok || !strings.HasSuffix(noEffect.Summary, message) {
		t.Fatalf("githubStatusError() = %v, want the whole message in the summary", err)
	}
}

func TestCommentOnIssueToolMarksFailuresBeforeSendingAsNoEffect(t *testing.T) {
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CommentOnIssueTool{k8sClient: newFakeClient()}
	for _, args := range []CommentOnIssueArgs{
		{IssueNumber: 1, Body: "no repository"},
		{RepoURL: testOrgTestRepoURL, IssueNumber: 1},
	} {
		raw, _ := json.Marshal(args)
		_, err := tool.Execute(context.Background(), raw)
		noEffect, ok := errors.AsType[*NoEffectError](err)
		if !ok || noEffect.Summary != "The request was not sent: "+err.Error() {
			t.Fatalf("Execute(%+v) error = %v, want a not-sent failure", args, err)
		}
	}
}

func TestCommentOnIssueToolTransportFailureKeepsOutcomeUncertain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, _ := w.(http.Hijacker)
		connection, _, _ := hijacker.Hijack()
		_ = connection.Close()
	}))
	defer server.Close()
	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CommentOnIssueTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
	args, _ := json.Marshal(CommentOnIssueArgs{RepoURL: testOrgTestRepoURL, IssueNumber: 1, Body: testCommentBody})

	_, err := tool.Execute(context.Background(), args)
	if _, ok := errors.AsType[*NoEffectError](err); err == nil || ok {
		t.Fatalf("Execute() error = %v, want an uncertain transport failure", err)
	}
}

func TestGitHubMutationsMarkRefusalsAsNoEffect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer server.Close()
	const want = "GitHub refused the request (HTTP 403): Resource not accessible by integration"

	_, _, err := postGitHubReview(context.Background(), testGitHubToken, "owner", "repo", 1, "body", reviewEventComment, nil, server.URL)
	if noEffect, ok := errors.AsType[*NoEffectError](err); !ok || noEffect.Summary != want {
		t.Fatalf("postGitHubReview() error = %v, want a refusal", err)
	}
	_, err = CreateOrGetGitHubPullRequest(context.Background(), testGitHubToken, "owner", "repo", "head", "main", "title", "body", server.URL)
	if noEffect, ok := errors.AsType[*NoEffectError](err); !ok || noEffect.Summary != want {
		t.Fatalf("CreateOrGetGitHubPullRequest() error = %v, want a refusal", err)
	}
}

func TestGitHubMutationToolsMarkScopeFailuresAsNoEffect(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	for name, call := range map[string]func() error{
		"create_pull_request": func() error {
			args, _ := json.Marshal(CreatePullRequestArgs{TaskName: testNonexistentName, HeadBranch: testFeatureBranch, BaseBranch: testBranch, Title: testPullRequestTitle})
			_, err := NewCreatePullRequestTool(newFakeClient()).Execute(context.Background(), args)
			return err
		},
		"post_review_comment": func() error {
			args, _ := json.Marshal(PostReviewCommentArgs{TaskName: testNonexistentName, PRNumber: 1, Body: testReviewBody, Event: reviewEventComment})
			_, err := NewPostReviewCommentTool(newFakeClient()).Execute(context.Background(), args)
			return err
		},
	} {
		err := call()
		if _, ok := errors.AsType[*NoEffectError](err); !ok || !strings.Contains(err.Error(), "failed to get task") {
			t.Fatalf("%s error = %v, want a not-sent scope failure", name, err)
		}
	}
}

func TestCreateOrGetGitHubPullRequestMarksRefusedDuplicateAsNoEffect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for owner:head."}]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := CreateOrGetGitHubPullRequest(context.Background(), testGitHubToken, "owner", "repo", "head", "main", "title", "body", server.URL)
	noEffect, ok := errors.AsType[*NoEffectError](err)
	if !ok || !strings.Contains(noEffect.Summary, "already exists") || !strings.Contains(err.Error(), "lookup failed") {
		t.Fatalf("CreateOrGetGitHubPullRequest() error = %v, want a refused create whose lookup failed", err)
	}
}
