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

const checkPRReviewMarkerTestSHA = "abc123def456"

func TestCheckPRReviewMarkerTool_Metadata(t *testing.T) {
	tool := NewCheckPRReviewMarkerTool(newFakeClient())

	if tool.Name() != checkPRReviewMarkerToolName {
		t.Errorf("Name() = %q, want %q", tool.Name(), checkPRReviewMarkerToolName)
	}
	if tool.Description() == "" {
		t.Error("Description() returned empty string")
	}

	params := tool.Parameters()
	if len(params) == 0 {
		t.Fatal("Parameters() returned empty JSON")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters schema: %v", err)
	}
	if schema[jsonSchemaTypeField] != jsonSchemaTypeObject {
		t.Errorf("schema type = %v, want %q", schema[jsonSchemaTypeField], jsonSchemaTypeObject)
	}

	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, field := range []string{taskNameField, repoURLField, githubPRNumberField, headSHAField} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing %q property", field)
		}
	}

	required, ok := schema[jsonSchemaRequiredField].([]any)
	if !ok {
		t.Fatalf("schema required = %T, want []any", schema[jsonSchemaRequiredField])
	}
	if len(required) != 1 || required[0] != githubPRNumberField {
		t.Errorf("required = %v, want [%q]", required, githubPRNumberField)
	}
}

func TestCheckPRReviewMarkerTool_NoMarkerFoundWithExplicitHeadSHA(t *testing.T) {
	const prNumber = 42
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		assertCheckPRReviewMarkerAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/issues/%d/comments", prNumber):
			if r.URL.Query().Get(perPageField) != "100" {
				t.Errorf("comments per_page = %q, want 100", r.URL.Query().Get(perPageField))
			}
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/pulls/%d/reviews", prNumber):
			if r.URL.Query().Get(perPageField) != "100" {
				t.Errorf("reviews per_page = %q, want 100", r.URL.Query().Get(perPageField))
			}
			_, _ = fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CheckPRReviewMarkerTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
	args, _ := json.Marshal(CheckPRReviewMarkerArgs{RepoURL: testSozercanAynaRepoURL, PRNumber: prNumber, HeadSHA: checkPRReviewMarkerTestSHA})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	var got CheckPRReviewMarkerResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if got.Found {
		t.Error("Found = true, want false")
	}
	if got.PRNumber != prNumber {
		t.Errorf("PRNumber = %d, want %d", got.PRNumber, prNumber)
	}
	if got.HeadSHA != checkPRReviewMarkerTestSHA {
		t.Errorf("HeadSHA = %q, want %q", got.HeadSHA, checkPRReviewMarkerTestSHA)
	}
	if got.Marker != formatPRReviewMarker(checkPRReviewMarkerTestSHA) {
		t.Errorf("Marker = %q, want %q", got.Marker, formatPRReviewMarker(checkPRReviewMarkerTestSHA))
	}
	if !strings.Contains(got.Message, "no review marker found") {
		t.Errorf("Message = %q, want no marker found", got.Message)
	}
	if requestCount != 2 {
		t.Errorf("requestCount = %d, want 2", requestCount)
	}
}

func TestCheckPRReviewMarkerTool_MarkerFoundInIssueComments(t *testing.T) {
	const prNumber = 43
	const commentURL = "https://github.com/sozercan/ayna/pull/43#issuecomment-1"
	reviewsCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCheckPRReviewMarkerAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/issues/%d/comments", prNumber):
			_, _ = fmt.Fprintf(w, `[{"body":"reviewed\n\n%s","html_url":%q,"user":{"login":"orka-bot"}}]`, formatPRReviewMarker(checkPRReviewMarkerTestSHA), commentURL)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/pulls/%d/reviews", prNumber):
			reviewsCalled = true
			t.Errorf("reviews endpoint should not be called when marker is found in comments")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CheckPRReviewMarkerTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
	args, _ := json.Marshal(CheckPRReviewMarkerArgs{RepoURL: testSozercanAynaRepoURL, PRNumber: prNumber, HeadSHA: checkPRReviewMarkerTestSHA})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	if reviewsCalled {
		t.Fatal("reviews endpoint was called")
	}

	var got CheckPRReviewMarkerResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !got.Found {
		t.Error("Found = false, want true")
	}
	if got.Source != "issue_comment" {
		t.Errorf("Source = %q, want issue_comment", got.Source)
	}
	if got.HTMLURL != commentURL {
		t.Errorf("HTMLURL = %q, want %q", got.HTMLURL, commentURL)
	}
	if got.Author != "orka-bot" {
		t.Errorf("Author = %q, want orka-bot", got.Author)
	}
	if got.Marker != formatPRReviewMarker(checkPRReviewMarkerTestSHA) {
		t.Errorf("Marker = %q, want %q", got.Marker, formatPRReviewMarker(checkPRReviewMarkerTestSHA))
	}
}

func TestCheckPRReviewMarkerTool_MarkerFoundInReviews(t *testing.T) {
	const prNumber = 44
	const reviewURL = "https://github.com/sozercan/ayna/pull/44#pullrequestreview-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCheckPRReviewMarkerAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/issues/%d/comments", prNumber):
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/pulls/%d/reviews", prNumber):
			_, _ = fmt.Fprintf(w, `[{"body":"LGTM\n\n%s","html_url":%q,"user":{"login":"reviewer-bot"}}]`, formatPRReviewMarker(checkPRReviewMarkerTestSHA), reviewURL)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CheckPRReviewMarkerTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
	args, _ := json.Marshal(CheckPRReviewMarkerArgs{RepoURL: testSozercanAynaRepoURL, PRNumber: prNumber, HeadSHA: checkPRReviewMarkerTestSHA})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	var got CheckPRReviewMarkerResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !got.Found {
		t.Error("Found = false, want true")
	}
	if got.Source != "review" {
		t.Errorf("Source = %q, want review", got.Source)
	}
	if got.HTMLURL != reviewURL {
		t.Errorf("HTMLURL = %q, want %q", got.HTMLURL, reviewURL)
	}
	if got.Author != "reviewer-bot" {
		t.Errorf("Author = %q, want reviewer-bot", got.Author)
	}
}

func assertCheckPRReviewMarkerAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if auth := r.Header.Get("Authorization"); auth != testBearerToken {
		t.Errorf("Authorization = %q, want %q", auth, testBearerToken)
	}
}
