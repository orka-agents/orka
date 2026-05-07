/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebSearchTool_Name(t *testing.T) {
	tool := NewWebSearchTool()
	if got := tool.Name(); got != webSearchToolName {
		t.Errorf("Name() = %v, want %v", got, webSearchToolName)
	}
}

func TestWebSearchTool_Description(t *testing.T) {
	tool := NewWebSearchTool()
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestWebSearchTool_Parameters(t *testing.T) {
	tool := NewWebSearchTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	// Verify it's valid JSON
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}

	// Check required fields
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestWebSearchTool_Execute(t *testing.T) {
	tests := []struct {
		name    string
		args    json.RawMessage
		wantErr bool
	}{
		{
			name:    "valid query",
			args:    json.RawMessage(`{"query": "test search"}`),
			wantErr: false,
		},
		{
			name:    "valid query with limit",
			args:    json.RawMessage(`{"query": "test search", "limit": 3}`),
			wantErr: false,
		},
		{
			name:    "empty query",
			args:    json.RawMessage(`{"query": ""}`),
			wantErr: true,
		},
		{
			name:    "missing query",
			args:    json.RawMessage(`{}`),
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			args:    json.RawMessage(invalidJSONText),
			wantErr: true,
		},
		{
			name:    "negative limit uses default",
			args:    json.RawMessage(`{"query": "test", "limit": -1}`),
			wantErr: false,
		},
		{
			name:    "zero limit uses default",
			args:    json.RawMessage(`{"query": "test", "limit": 0}`),
			wantErr: false,
		},
	}

	tool := NewWebSearchTool()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tool.Execute(context.Background(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result == "" {
				t.Error("Execute() returned empty result")
			}
		})
	}
}

func TestWebSearchTool_Execute_MockSearch(t *testing.T) {
	// Test mock search (no API configured)
	tool := NewWebSearchTool()
	// Ensure no API URL is set
	tool.baseURL = ""

	args := json.RawMessage(`{"query": "test query"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify result is valid JSON
	var results []WebSearchResult
	if err := json.Unmarshal([]byte(result), &results); err != nil {
		t.Errorf("Execute() returned invalid JSON: %v", err)
	}

	if len(results) == 0 {
		t.Error("Execute() returned empty results")
	}
}

func TestWebSearchTool_Execute_APISearch(t *testing.T) {
	// Create a test server
	expectedResponse := `[{"title": "Test", "url": "https://example.com", "snippet": "Test result"}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != http.MethodGet {
			t.Errorf("expected GET request, got %s", r.Method)
		}

		query := r.URL.Query().Get("q")
		if query != "test query" {
			t.Errorf("expected query 'test query', got '%s'", query)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(expectedResponse)) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebSearchTool{
		baseURL: server.URL,
		apiKey:  "test-key",
		client:  server.Client(),
	}

	args := json.RawMessage(`{"query": "test query", "limit": 5}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if result != expectedResponse {
		t.Errorf("Execute() = %v, want %v", result, expectedResponse)
	}
}

func TestWebSearchTool_Execute_APIError(t *testing.T) {
	// Create a test server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error")) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebSearchTool{
		baseURL: server.URL,
		client:  server.Client(),
	}

	args := json.RawMessage(`{"query": "test query"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for API failure")
	}
}

func TestWebSearchTool_Execute_WithAuthHeader(t *testing.T) {
	var receivedAuthHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`)) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebSearchTool{
		baseURL: server.URL,
		apiKey:  "test-api-key",
		client:  server.Client(),
	}

	args := json.RawMessage(`{"query": "test"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	expected := "Bearer test-api-key"
	if receivedAuthHeader != expected {
		t.Errorf("Authorization header = %v, want %v", receivedAuthHeader, expected)
	}
}

func TestWebSearchTool_Execute_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Slow response
		<-r.Context().Done()
	}))
	defer server.Close()

	tool := &WebSearchTool{
		baseURL: server.URL,
		client:  server.Client(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	args := json.RawMessage(`{"query": "test"}`)
	_, err := tool.Execute(ctx, args)
	if err == nil {
		t.Error("Execute() expected error for cancelled context")
	}
}

func TestParseDDGResults(t *testing.T) {
	html := `<div class="result">
		<a class="result__a" href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage1&rut=abc">Example Page 1</a>
		<a class="result__snippet" href="#">This is the first result snippet</a>
	</div>
	<div class="result">
		<a class="result__a" href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage2&rut=def"><b>Bold</b> Page 2</a>
		<a class="result__snippet" href="#">Second result with <b>bold</b></a>
	</div>`

	results := parseDDGResults(html, 10)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].URL != "https://example.com/page1" {
		t.Errorf("result[0].URL = %q, want %q", results[0].URL, "https://example.com/page1")
	}
	if results[0].Title != "Example Page 1" {
		t.Errorf("result[0].Title = %q, want %q", results[0].Title, "Example Page 1")
	}

	// Tags should be stripped from title
	if results[1].Title != "Bold Page 2" {
		t.Errorf("result[1].Title = %q, want %q", results[1].Title, "Bold Page 2")
	}
}

func TestDecodeDDGURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"with uddg param", "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Ftest&rut=abc", "https://example.com/test"},
		{"direct URL", "https://example.com/direct", "https://example.com/direct"},
		{"non-http", "javascript:void(0)", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeDDGURL(tt.input)
			if got != tt.expect {
				t.Errorf("decodeDDGURL(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestStripHTMLTags(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"<b>bold</b>", "bold"},
		{"no tags", "no tags"},
		{"<a href='#'>link &amp; text</a>", "link & text"},
		{"<span>a</span> <span>b</span>", "a b"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripHTMLTags(tt.input)
			if got != tt.expect {
				t.Errorf("stripHTMLTags(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}
