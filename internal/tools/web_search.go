/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// WebSearchTool implements web search functionality
type WebSearchTool struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// WebSearchArgs are the arguments for the web search tool
type WebSearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// WebSearchResult represents a search result
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// NewWebSearchTool creates a new web search tool
func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{
		apiKey:  os.Getenv("SEARCH_API_KEY"),
		baseURL: os.Getenv("SEARCH_API_URL"),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Name returns the tool name
func (t *WebSearchTool) Name() string {
	return "web_search"
}

// Description returns the tool description
func (t *WebSearchTool) Description() string {
	return "Search the web for information. Use this when you need to find current information or facts."
}

// Parameters returns the JSON Schema for parameters
func (t *WebSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "The search query"
			},
			"limit": {
				"type": "integer",
				"description": "Maximum number of results to return (default: 5)",
				"default": 5
			}
		},
		"required": ["query"]
	}`)
}

// Execute performs the web search
func (t *WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var searchArgs WebSearchArgs
	if err := json.Unmarshal(args, &searchArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if searchArgs.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	if searchArgs.Limit <= 0 {
		searchArgs.Limit = 5
	}

	// If no API configured, return a placeholder response
	if t.baseURL == "" {
		return t.mockSearch(searchArgs)
	}

	// Build request URL
	reqURL := fmt.Sprintf("%s?q=%s&limit=%d",
		t.baseURL,
		url.QueryEscape(searchArgs.Query),
		searchArgs.Limit,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("search API returned %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}

// mockSearch returns a mock search response when no API is configured
func (t *WebSearchTool) mockSearch(args WebSearchArgs) (string, error) {
	results := []WebSearchResult{
		{
			Title:   "Search Result 1",
			URL:     "https://example.com/result1",
			Snippet: fmt.Sprintf("This is a mock search result for query: %s", args.Query),
		},
		{
			Title:   "Search Result 2",
			URL:     "https://example.com/result2",
			Snippet: "Web search API not configured. Set SEARCH_API_URL and SEARCH_API_KEY environment variables.",
		},
	}

	output, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// Ensure WebSearchTool implements Tool
var _ Tool = (*WebSearchTool)(nil)
