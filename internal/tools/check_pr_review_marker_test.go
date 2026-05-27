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
	if requestCount != 1 {
		t.Errorf("requestCount = %d, want 1", requestCount)
	}
}

func TestCheckPRReviewMarkerTool_MarkerFoundInReviews(t *testing.T) {
	const prNumber = 44
	const reviewURL = "https://github.com/sozercan/ayna/pull/44#pullrequestreview-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCheckPRReviewMarkerAuth(t, r)
		switch {
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

func TestCheckPRReviewMarkerTool_MarkerFoundInReviewsPage2(t *testing.T) {
	const prNumber = 46
	const reviewURL = "https://github.com/sozercan/ayna/pull/46#pullrequestreview-101"
	var reviewPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCheckPRReviewMarkerAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/pulls/%d/reviews", prNumber):
			if r.URL.Query().Get(perPageField) != "100" {
				t.Errorf("reviews per_page = %q, want 100", r.URL.Query().Get(perPageField))
			}
			page := r.URL.Query().Get(pageField)
			reviewPages = append(reviewPages, page)
			switch page {
			case "1":
				_, _ = fmt.Fprint(w, checkPRReviewMarkerPageJSON(100, "", "", ""))
			case "2":
				_, _ = fmt.Fprint(w, checkPRReviewMarkerPageJSON(1, "LGTM\n\n"+formatPRReviewMarker(checkPRReviewMarkerTestSHA), reviewURL, "reviewer-bot"))
			default:
				t.Errorf("unexpected reviews page: %q", page)
				w.WriteHeader(http.StatusNotFound)
			}
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
	if strings.Join(reviewPages, ",") != "1,2" {
		t.Fatalf("review pages = %v, want [1 2]", reviewPages)
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

func TestCheckPRReviewMarkerTool_FetchesHeadSHAWhenOmitted(t *testing.T) {
	const prNumber = 47
	const reviewURL = "https://github.com/sozercan/ayna/pull/47#pullrequestreview-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCheckPRReviewMarkerAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/pulls/%d", prNumber):
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"state":"open","merged":false}`, checkPRReviewMarkerTestSHA)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/sozercan/ayna/pulls/%d/reviews", prNumber):
			_, _ = fmt.Fprintf(w, `[{"body":"%s","html_url":%q,"user":{"login":"reviewer-bot"}}]`, formatPRReviewMarker(checkPRReviewMarkerTestSHA), reviewURL)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", testGitHubToken)
	tool := &CheckPRReviewMarkerTool{k8sClient: newFakeClient(), apiBaseURL: server.URL}
	args, _ := json.Marshal(CheckPRReviewMarkerArgs{RepoURL: testSozercanAynaRepoURL, PRNumber: prNumber})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	var got CheckPRReviewMarkerResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !got.Found {
		t.Fatal("Found = false, want true")
	}
	if got.HeadSHA != checkPRReviewMarkerTestSHA {
		t.Errorf("HeadSHA = %q, want %q", got.HeadSHA, checkPRReviewMarkerTestSHA)
	}
}

func TestContainsPRReviewMarkerRequiresExactMarker(t *testing.T) {
	otherText := defaultPRReviewMarkerPrefix + " something else --> " + checkPRReviewMarkerTestSHA
	if containsPRReviewMarker(otherText, checkPRReviewMarkerTestSHA) {
		t.Fatalf("containsPRReviewMarker matched prefix and SHA without exact marker")
	}
	if !containsPRReviewMarker("reviewed\n"+formatPRReviewMarker(checkPRReviewMarkerTestSHA), checkPRReviewMarkerTestSHA) {
		t.Fatalf("containsPRReviewMarker did not match exact marker")
	}
}

func checkPRReviewMarkerPageJSON(count int, markerBody, markerURL, markerAuthor string) string {
	items := make([]map[string]any, count)
	for i := range items {
		body := fmt.Sprintf("no marker %d", i)
		htmlURL := fmt.Sprintf("https://github.com/sozercan/ayna/pull/1#placeholder-%d", i)
		author := "user"
		if markerBody != "" && i == count-1 {
			body = markerBody
			htmlURL = markerURL
			author = markerAuthor
		}
		items[i] = map[string]any{
			"body":     body,
			"html_url": htmlURL,
			"user": map[string]string{
				"login": author,
			},
		}
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func assertCheckPRReviewMarkerAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if auth := r.Header.Get("Authorization"); auth != testBearerToken {
		t.Errorf("Authorization = %q, want %q", auth, testBearerToken)
	}
}
