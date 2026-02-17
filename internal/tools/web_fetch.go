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
	"regexp"
	"strings"
	"time"
)

// WebFetchTool implements URL content fetching and extraction
type WebFetchTool struct {
	client *http.Client
}

// WebFetchArgs are the arguments for the web fetch tool
type WebFetchArgs struct {
	URL      string `json:"url"`
	MaxChars int    `json:"max_chars,omitempty"`
	Raw      bool   `json:"raw,omitempty"`
}

// WebFetchResult represents the fetch result
type WebFetchResult struct {
	URL       string `json:"url"`
	Status    int    `json:"status"`
	Content   string `json:"content"`
	Length    int    `json:"length"`
	Truncated bool   `json:"truncated"`
	Extractor string `json:"extractor"`
}

const maxBodySize = 5 * 1024 * 1024 // 5MB

// NewWebFetchTool creates a new web fetch tool
func NewWebFetchTool() *WebFetchTool {
	return &WebFetchTool{
		client: &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects (max 5)")
				}
				return nil
			},
		},
	}
}

// Name returns the tool name
func (t *WebFetchTool) Name() string {
	return "web_fetch"
}

// Description returns the tool description
func (t *WebFetchTool) Description() string {
	return "Fetch and extract content from a URL. Returns extracted text from HTML pages, pretty-printed JSON, or raw content."
}

// Parameters returns the JSON Schema for parameters
func (t *WebFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "The URL to fetch (http or https only)"
			},
			"max_chars": {
				"type": "integer",
				"description": "Maximum characters to return (default: 50000)",
				"default": 50000
			},
			"raw": {
				"type": "boolean",
				"description": "Return raw HTML instead of extracted text (default: false)",
				"default": false
			}
		},
		"required": ["url"]
	}`)
}

// Execute fetches the URL and extracts content
func (t *WebFetchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var fetchArgs WebFetchArgs
	if err := json.Unmarshal(args, &fetchArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if fetchArgs.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	// Validate URL
	parsed, err := url.Parse(fetchArgs.URL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("only http and https URLs are supported")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("URL must have a host")
	}

	if fetchArgs.MaxChars <= 0 {
		fetchArgs.MaxChars = 50000
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchArgs.URL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; OrkaBot/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	var content string
	var extractor string

	switch {
	case strings.Contains(contentType, "application/json"):
		content, extractor = t.extractJSON(body)
	case strings.Contains(contentType, "text/html"):
		if fetchArgs.Raw {
			content = string(body)
			extractor = "raw"
		} else {
			content = extractText(body)
			extractor = "html_text"
		}
	default:
		content = string(body)
		extractor = "raw"
	}

	truncated := false
	if len(content) > fetchArgs.MaxChars {
		content = content[:fetchArgs.MaxChars]
		truncated = true
	}

	result := WebFetchResult{
		URL:       fetchArgs.URL,
		Status:    resp.StatusCode,
		Content:   content,
		Length:    len(content),
		Truncated: truncated,
		Extractor: extractor,
	}

	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// extractJSON pretty-prints JSON content
func (t *WebFetchTool) extractJSON(body []byte) (string, string) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return string(body), "raw"
	}
	pretty, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return string(body), "raw"
	}
	return string(pretty), "json"
}

var (
	scriptRe     = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRe      = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// extractText strips HTML tags, scripts, styles, and collapses whitespace
func extractText(body []byte) string {
	s := string(body)
	s = scriptRe.ReplaceAllString(s, " ")
	s = styleRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = whitespaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Ensure WebFetchTool implements Tool
var _ Tool = (*WebFetchTool)(nil)
