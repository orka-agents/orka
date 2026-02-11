/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// EventType identifies the kind of SSE event sent by the chat endpoint.
type EventType string

const (
	EventStatus     EventType = "status"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventMessage    EventType = "message"
	EventError      EventType = "error"
	EventDone       EventType = "done"
)

// SSEEvent represents a parsed SSE event from the chat stream.
type SSEEvent struct {
	Type EventType
	Data json.RawMessage
}

// StatusEventData is the payload for "status" events.
type StatusEventData struct {
	SessionID string `json:"sessionId"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

// ToolCallEventData is the payload for "tool_call" events.
type ToolCallEventData struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResultEventData is the payload for "tool_result" events.
type ToolResultEventData struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Result json.RawMessage `json:"result"`
}

// MessageEventData is the payload for "message" events.
type MessageEventData struct {
	Content string `json:"content"`
}

// ErrorEventData is the payload for "error" events.
type ErrorEventData struct {
	Error string `json:"error"`
}

// DoneEventData is the payload for "done" events.
type DoneEventData struct {
	Usage ChatUsage `json:"usage"`
}

// doSSE executes a POST request with SSE accept headers and returns the raw response.
func (c *Client) doSSE(ctx context.Context, path string, body io.Reader) (*http.Response, error) {
	reqURL := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	return resp, nil
}

// StreamChat sends a chat request and returns a channel of SSE events.
// It POSTs to /api/v1/chat with Accept: text/event-stream header.
// The channel is closed when the stream ends (done event, error, or context cancellation).
func (c *Client) StreamChat(ctx context.Context, req ChatRequest) (<-chan SSEEvent, error) {
	body, err := encodeBody(req)
	if err != nil {
		return nil, err
	}

	resp, err := c.doSSE(ctx, "/api/v1/chat", body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan SSEEvent, 16)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		var eventType string
		var dataLines []string

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()

			// SSE comment — skip
			if strings.HasPrefix(line, ":") {
				continue
			}

			// Blank line signals end of an event block
			if line == "" {
				if eventType != "" && len(dataLines) > 0 {
					data := strings.Join(dataLines, "\n")
					event := SSEEvent{
						Type: EventType(eventType),
						Data: json.RawMessage(data),
					}
					select {
					case ch <- event:
					case <-ctx.Done():
						return
					}
				}
				eventType = ""
				dataLines = nil
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "event:") {
				eventType = strings.TrimPrefix(line, "event:")
			} else if strings.HasPrefix(line, "data: ") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
			}
		}

		// Flush any remaining event (stream ended without trailing blank line)
		if eventType != "" && len(dataLines) > 0 {
			data := strings.Join(dataLines, "\n")
			event := SSEEvent{
				Type: EventType(eventType),
				Data: json.RawMessage(data),
			}
			select {
			case ch <- event:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}
