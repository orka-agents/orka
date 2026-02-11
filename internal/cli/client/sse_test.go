/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamChat_StatusEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/chat" {
			t.Fatalf("expected /api/v1/chat, got %s", r.URL.Path)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("expected Accept: text/event-stream, got %s", r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("expected Bearer token, got %s", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: status\ndata: {\"sessionId\":\"s1\",\"provider\":\"openai\",\"model\":\"gpt-4\"}\n\n")
	}))
	defer server.Close()

	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 5 * time.Second
	events, err := c.StreamChat(context.Background(), ChatRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	event := <-events
	if event.Type != EventStatus {
		t.Fatalf("expected status event, got %s", event.Type)
	}

	var data StatusEventData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("failed to unmarshal status data: %v", err)
	}
	if data.SessionID != "s1" {
		t.Fatalf("expected sessionId s1, got %s", data.SessionID)
	}
	if data.Provider != "openai" {
		t.Fatalf("expected provider openai, got %s", data.Provider)
	}
	if data.Model != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %s", data.Model)
	}
}

func TestStreamChat_MessageEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: {\"content\":\"Hello, world!\"}\n\n")
	}))
	defer server.Close()

	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 5 * time.Second
	events, err := c.StreamChat(context.Background(), ChatRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	event := <-events
	if event.Type != EventMessage {
		t.Fatalf("expected message event, got %s", event.Type)
	}

	var data MessageEventData
	json.Unmarshal(event.Data, &data)
	if data.Content != "Hello, world!" {
		t.Fatalf("expected content 'Hello, world!', got %s", data.Content)
	}
}

func TestStreamChat_ToolCallEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: tool_call\ndata: {\"id\":\"tc-1\",\"name\":\"search\",\"args\":{\"query\":\"mercan\"}}\n\n")
	}))
	defer server.Close()

	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 5 * time.Second
	events, err := c.StreamChat(context.Background(), ChatRequest{Message: "search"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	event := <-events
	if event.Type != EventToolCall {
		t.Fatalf("expected tool_call event, got %s", event.Type)
	}

	var data ToolCallEventData
	json.Unmarshal(event.Data, &data)
	if data.ID != "tc-1" {
		t.Fatalf("expected id tc-1, got %s", data.ID)
	}
	if data.Name != "search" {
		t.Fatalf("expected name search, got %s", data.Name)
	}
}

func TestStreamChat_MultipleEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: status\ndata: {\"sessionId\":\"s1\",\"provider\":\"anthropic\",\"model\":\"claude-3\"}\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"content\":\"first\"}\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"content\":\" second\"}\n\n")
	}))
	defer server.Close()

	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 5 * time.Second
	events, err := c.StreamChat(context.Background(), ChatRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Event 1: status
	event1 := <-events
	if event1.Type != EventStatus {
		t.Fatalf("expected status event, got %s", event1.Type)
	}

	// Event 2: message
	event2 := <-events
	if event2.Type != EventMessage {
		t.Fatalf("expected message event, got %s", event2.Type)
	}
	var msg1 MessageEventData
	json.Unmarshal(event2.Data, &msg1)
	if msg1.Content != "first" {
		t.Fatalf("expected content 'first', got %s", msg1.Content)
	}

	// Event 3: message
	event3 := <-events
	if event3.Type != EventMessage {
		t.Fatalf("expected message event, got %s", event3.Type)
	}
	var msg2 MessageEventData
	json.Unmarshal(event3.Data, &msg2)
	if msg2.Content != " second" {
		t.Fatalf("expected content ' second', got %s", msg2.Content)
	}
}

func TestStreamChat_DoneEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: done\ndata: {\"usage\":{\"inputTokens\":100,\"outputTokens\":50,\"llmCalls\":2,\"toolCalls\":1,\"tasksCreated\":0,\"duration\":\"5s\"}}\n\n")
	}))
	defer server.Close()

	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 5 * time.Second
	events, err := c.StreamChat(context.Background(), ChatRequest{Message: "done"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	event := <-events
	if event.Type != EventDone {
		t.Fatalf("expected done event, got %s", event.Type)
	}

	var data DoneEventData
	json.Unmarshal(event.Data, &data)
	if data.Usage.InputTokens != 100 {
		t.Fatalf("expected inputTokens 100, got %d", data.Usage.InputTokens)
	}
	if data.Usage.OutputTokens != 50 {
		t.Fatalf("expected outputTokens 50, got %d", data.Usage.OutputTokens)
	}
	if data.Usage.LLMCalls != 2 {
		t.Fatalf("expected llmCalls 2, got %d", data.Usage.LLMCalls)
	}
	if data.Usage.ToolCalls != 1 {
		t.Fatalf("expected toolCalls 1, got %d", data.Usage.ToolCalls)
	}
	if data.Usage.Duration != "5s" {
		t.Fatalf("expected duration 5s, got %s", data.Usage.Duration)
	}
}

func TestStreamChat_ErrorResponse(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"Unauthorized", http.StatusUnauthorized},
		{"InternalServerError", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(`{"error":"access denied"}`))
			}))
			defer server.Close()

			c := New(server.URL, "token", "default")
			c.HTTPClient.Timeout = 5 * time.Second
			_, err := c.StreamChat(context.Background(), ChatRequest{Message: "hello"})
			if err == nil {
				t.Fatal("expected error for non-2xx response")
			}
		})
	}
}

func TestStreamChat_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected ResponseWriter to implement Flusher")
		}
		// Send one event
		fmt.Fprintf(w, "event: status\ndata: {\"sessionId\":\"s1\",\"provider\":\"openai\",\"model\":\"gpt-4\"}\n\n")
		flusher.Flush()

		// Block until the request context is done (client disconnects)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 10 * time.Second
	events, err := c.StreamChat(ctx, ChatRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read first event
	event := <-events
	if event.Type != EventStatus {
		t.Fatalf("expected status event, got %s", event.Type)
	}

	// Cancel context
	cancel()

	// Channel should close eventually
	select {
	case _, ok := <-events:
		if ok {
			// Draining is fine, just wait for close
			for range events {
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for events channel to close after context cancellation")
	}
}

func TestStreamChat_SSEComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// SSE comment should be skipped
		fmt.Fprintf(w, ": this is a comment\n")
		fmt.Fprintf(w, "event: message\ndata: {\"content\":\"after comment\"}\n\n")
	}))
	defer server.Close()

	c := New(server.URL, "token", "default")
	c.HTTPClient.Timeout = 5 * time.Second
	events, err := c.StreamChat(context.Background(), ChatRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	event := <-events
	if event.Type != EventMessage {
		t.Fatalf("expected message event, got %s", event.Type)
	}

	var data MessageEventData
	json.Unmarshal(event.Data, &data)
	if data.Content != "after comment" {
		t.Fatalf("expected content 'after comment', got %s", data.Content)
	}
}
