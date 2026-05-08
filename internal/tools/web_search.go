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
	"regexp"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
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
		apiKey:  os.Getenv(workerenv.SearchAPIKey),
		baseURL: os.Getenv(workerenv.SearchAPIURL),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

const webSearchToolName = "web_search"

// Name returns the tool name
func (t *WebSearchTool) Name() string {
	return webSearchToolName
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

	// If no API configured, use DuckDuckGo fallback
	if t.baseURL == "" {
		return t.duckDuckGoSearch(ctx, searchArgs)
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
	defer resp.Body.Close() //nolint:errcheck

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

var (
	ddgLinkRe    = regexp.MustCompile(`<a[^>]+class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	ddgUddgRe    = regexp.MustCompile(`uddg=([^&]+)`)
	ddgTagRe     = regexp.MustCompile(`<[^>]+>`)
)

// duckDuckGoSearch performs a search via DuckDuckGo HTML
func (t *WebSearchTool) duckDuckGoSearch(ctx context.Context, args WebSearchArgs) (string, error) {
	ddgURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(args.Query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ddgURL, nil)
	if err != nil {
		return t.mockSearch(args)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := t.client.Do(req)
	if err != nil {
		return t.mockSearch(args)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return t.mockSearch(args)
	}

	results := parseDDGResults(string(body), args.Limit)
	if len(results) == 0 {
		return t.mockSearch(args)
	}

	output, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// parseDDGResults extracts search results from DuckDuckGo HTML
func parseDDGResults(html string, limit int) []WebSearchResult {
	linkMatches := ddgLinkRe.FindAllStringSubmatch(html, -1)
	snippetMatches := ddgSnippetRe.FindAllStringSubmatch(html, -1)

	results := make([]WebSearchResult, 0, len(linkMatches))
	for i, m := range linkMatches {
		if len(results) >= limit {
			break
		}
		rawURL := m[1]
		title := stripHTMLTags(m[2])

		// Decode DDG redirect URL
		actualURL := decodeDDGURL(rawURL)
		if actualURL == "" || title == "" {
			continue
		}

		snippet := ""
		if i < len(snippetMatches) && len(snippetMatches[i]) > 1 {
			snippet = stripHTMLTags(snippetMatches[i][1])
		}

		results = append(results, WebSearchResult{
			Title:   title,
			URL:     actualURL,
			Snippet: snippet,
		})
	}
	return results
}

// decodeDDGURL extracts the actual URL from a DuckDuckGo redirect URL
func decodeDDGURL(rawURL string) string {
	matches := ddgUddgRe.FindStringSubmatch(rawURL)
	if len(matches) > 1 {
		decoded, err := url.QueryUnescape(matches[1])
		if err != nil {
			return rawURL
		}
		return decoded
	}
	// Not a redirect URL, return as-is if it looks like a URL
	if strings.HasPrefix(rawURL, "http") {
		return rawURL
	}
	return ""
}

// stripHTMLTags removes HTML tags from a string
func stripHTMLTags(s string) string {
	s = ddgTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return strings.TrimSpace(s)
}

// Ensure WebSearchTool implements Tool
var _ Tool = (*WebSearchTool)(nil)
