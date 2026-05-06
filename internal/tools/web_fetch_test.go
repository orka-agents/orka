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

func TestWebFetchTool_Name(t *testing.T) {
	tool := NewWebFetchTool()
	if got := tool.Name(); got != webFetchToolName {
		t.Errorf("Name() = %v, want %v", got, webFetchToolName)
	}
}

func TestWebFetchTool_Description(t *testing.T) {
	tool := NewWebFetchTool()
	if desc := tool.Description(); desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestWebFetchTool_Parameters(t *testing.T) {
	tool := NewWebFetchTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestWebFetchTool_Execute_HTML(t *testing.T) {
	html := `<html><head><title>Test</title><script>var x=1;</script><style>body{}</style></head>
	<body><h1>Hello World</h1><p>This is a test page.</p></body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html)) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebFetchTool{client: server.Client()}
	args := json.RawMessage(`{"url": "` + server.URL + `"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(result), &fetchResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if fetchResult.Extractor != "html_text" {
		t.Errorf("extractor = %q, want %q", fetchResult.Extractor, "html_text")
	}
	// Script and style content should be stripped
	if strContains(fetchResult.Content, "var x=1") {
		t.Error("content should not contain script content")
	}
	if strContains(fetchResult.Content, "body{}") {
		t.Error("content should not contain style content")
	}
	if !strContains(fetchResult.Content, "Hello World") {
		t.Error("content should contain text")
	}
	if !strContains(fetchResult.Content, "This is a test page") {
		t.Error("content should contain paragraph text")
	}
}

func TestWebFetchTool_Execute_JSON(t *testing.T) {
	jsonData := `{"key":"value","num":42}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jsonData)) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebFetchTool{client: server.Client()}
	args := json.RawMessage(`{"url": "` + server.URL + `"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(result), &fetchResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if fetchResult.Extractor != "json" {
		t.Errorf("extractor = %q, want %q", fetchResult.Extractor, "json")
	}
	if !strContains(fetchResult.Content, `"key": "value"`) {
		t.Error("content should be pretty-printed JSON")
	}
}

func TestWebFetchTool_Execute_Raw(t *testing.T) {
	html := `<html><body><h1>Test</h1></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html)) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebFetchTool{client: server.Client()}
	args := json.RawMessage(`{"url": "` + server.URL + `", "raw": true}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(result), &fetchResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if fetchResult.Extractor != "raw" {
		t.Errorf("extractor = %q, want %q", fetchResult.Extractor, "raw")
	}
	if fetchResult.Content != html {
		t.Errorf("content = %q, want %q", fetchResult.Content, html)
	}
}

func TestWebFetchTool_Execute_URLValidation(t *testing.T) {
	tool := NewWebFetchTool()

	tests := []struct {
		name string
		url  string
	}{
		{"file scheme", `file:///etc/passwd`},
		{"empty host", `http://`},
		{"no scheme", `example.com`},
		{"ftp scheme", `ftp://example.com`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := json.RawMessage(`{"url": "` + tt.url + `"}`)
			_, err := tool.Execute(context.Background(), args)
			if err == nil {
				t.Error("Execute() expected error for invalid URL")
			}
		})
	}
}

func TestWebFetchTool_Execute_EmptyURL(t *testing.T) {
	tool := NewWebFetchTool()
	args := json.RawMessage(`{"url": ""}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for empty URL")
	}
}

func TestWebFetchTool_Execute_Truncation(t *testing.T) {
	longContent := make([]byte, 1000)
	for i := range longContent {
		longContent[i] = 'a'
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(longContent) //nolint:errcheck
	}))
	defer server.Close()

	tool := &WebFetchTool{client: server.Client()}
	args := json.RawMessage(`{"url": "` + server.URL + `", "max_chars": 100}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(result), &fetchResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !fetchResult.Truncated {
		t.Error("expected truncated = true")
	}
	if fetchResult.Length != 100 {
		t.Errorf("length = %d, want 100", fetchResult.Length)
	}
}

func TestWebFetchTool_Execute_Redirect(t *testing.T) {
	finalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("final destination")) //nolint:errcheck
	}))
	defer finalServer.Close()

	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalServer.URL, http.StatusFound)
	}))
	defer redirectServer.Close()

	tool := NewWebFetchTool()
	args := json.RawMessage(`{"url": "` + redirectServer.URL + `"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(result), &fetchResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !strContains(fetchResult.Content, "final destination") {
		t.Error("should follow redirect to final destination")
	}
}

func TestWebFetchTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewWebFetchTool()
	args := json.RawMessage(invalidJSONText)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}

func strContains(s, substr string) bool {
	return len(s) >= len(substr) && len(substr) > 0 && strContainsCheck(s, substr)
}

func strContainsCheck(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
