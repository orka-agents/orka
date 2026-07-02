/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	chatConfigPath = "/api/v1/chat/config"
	chatAPIPath    = "/api/v1/chat"
)

// ---------------------------------------------------------------------------
// renderMarkdown
// ---------------------------------------------------------------------------

func TestRenderMarkdownBasic(t *testing.T) {
	out := renderMarkdown("# Hello\n\nWorld")
	if out == "" {
		t.Fatal("renderMarkdown returned empty for valid markdown")
	}
	if !strings.Contains(out, "Hello") {
		t.Errorf("expected output to contain 'Hello', got %q", out)
	}
}

func TestRenderMarkdownEmpty(t *testing.T) { //nolint:unparam
	out := renderMarkdown("")
	// Empty input should still produce some output (or empty)
	_ = out
}

func TestRenderMarkdownPlainText(t *testing.T) {
	out := renderMarkdown("just plain text")
	if !strings.Contains(out, "just plain text") {
		t.Errorf("expected output to contain plain text, got %q", out)
	}
}

func TestRenderMarkdownCodeBlock(t *testing.T) {
	md := "```go\nfmt.Println(\"hello\")\n```"
	out := renderMarkdown(md)
	if out == "" {
		t.Error("renderMarkdown returned empty for code block")
	}
}

// ---------------------------------------------------------------------------
// handleToolCallEvent
// ---------------------------------------------------------------------------

func TestHandleToolCallEvent_DelegateTask(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"agentRef": "code-reviewer"})
	data := client.SSEEventData{
		Name: "delegate_task",
		Args: args,
	}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "Delegating to code-reviewer") {
		t.Errorf("expected delegation message, got %q", buf.String())
	}
}

func TestHandleToolCallEvent_CreateAgentTask(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"agent": "planner"})
	data := client.SSEEventData{
		Name: "create_agent_task",
		Args: args,
	}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "Delegating to planner") {
		t.Errorf("expected delegation message, got %q", buf.String())
	}
}

func TestHandleToolCallEvent_CheckTaskProgress(t *testing.T) { //nolint:unparam
	data := client.SSEEventData{Name: "check_task_progress"}

	old := os.Stderr
	_, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old
	// Should not panic — suppressed
}

func TestHandleToolCallEvent_FetchTaskOutput(t *testing.T) {
	data := client.SSEEventData{Name: "fetch_task_output"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityV)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "Fetching result") {
		t.Errorf("expected 'Fetching result' at verbosity V, got %q", buf.String())
	}
}

func TestHandleToolCallEvent_FetchTaskOutputDefault(t *testing.T) {
	data := client.SSEEventData{Name: "fetch_task_output"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	// At default verbosity, fetch_task_output should not print
	if strings.Contains(buf.String(), "Fetching result") {
		t.Errorf("should not show fetching result at default verbosity")
	}
}

func TestHandleToolCallEvent_DefaultTool(t *testing.T) {
	data := client.SSEEventData{Name: "some_other_tool"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "some_other_tool") {
		t.Errorf("expected tool name in output, got %q", buf.String())
	}
}

func TestHandleToolCallEvent_DefaultToolVV(t *testing.T) {
	data := client.SSEEventData{Name: "some_other_tool"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityVV)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	// At VV, default tool name printing is suppressed (verbosity < VerbosityVV is false)
	if strings.Contains(buf.String(), "⚙") {
		t.Errorf("should not show ⚙ at VV verbosity for default tool")
	}
}

func TestHandleToolCallEvent_DelegateVV(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"agent": "test"})
	data := client.SSEEventData{
		Name: "delegate_task",
		Args: args,
	}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolCallEvent(data, VerbosityVV)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	// At VV, delegation message is suppressed (verbosity < VerbosityVV is false)
	if strings.Contains(buf.String(), "Delegating") {
		t.Errorf("should not show delegation at VV verbosity")
	}
}

// ---------------------------------------------------------------------------
// handleToolResultEvent
// ---------------------------------------------------------------------------

func TestHandleToolResultEvent_CheckTaskProgress(t *testing.T) {
	result, _ := json.Marshal(map[string]any{
		"phase":    "Running",
		"name":     "my-task",
		"duration": "10s",
	})
	data := client.SSEEventData{
		Name:   "check_task_progress",
		Result: result,
	}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	out := buf.String()
	if !strings.Contains(out, "my-task") {
		t.Errorf("expected task name, got %q", out)
	}
	if !strings.Contains(out, "Running") {
		t.Errorf("expected phase, got %q", out)
	}
}

func TestHandleToolResultEvent_CheckTaskProgressNoName(t *testing.T) {
	result, _ := json.Marshal(map[string]any{
		"phase": "Pending",
	})
	data := client.SSEEventData{
		Name:   "wait_for_task",
		Result: result,
	}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "task") {
		t.Errorf("expected default 'task' name, got %q", buf.String())
	}
}

func TestHandleToolResultEvent_CheckTaskProgressVV(t *testing.T) {
	result, _ := json.Marshal(map[string]any{"phase": "Running", "name": "t1"})
	data := client.SSEEventData{
		Name:   "check_task_progress",
		Result: result,
	}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityVV)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	// VV suppresses the ↻ output
	if strings.Contains(buf.String(), "↻") {
		t.Error("should not show ↻ at VV verbosity")
	}
}

func TestHandleToolResultEvent_FetchTaskOutput(t *testing.T) {
	data := client.SSEEventData{Name: "fetch_task_output"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityV)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "Result received") {
		t.Errorf("expected 'Result received' at V, got %q", buf.String())
	}
}

func TestHandleToolResultEvent_FetchTaskOutputDefault(t *testing.T) {
	data := client.SSEEventData{Name: "fetch_task_output"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if strings.Contains(buf.String(), "Result received") {
		t.Error("should not show at default verbosity")
	}
}

func TestHandleToolResultEvent_DefaultToolV(t *testing.T) {
	data := client.SSEEventData{Name: "custom_tool"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityV)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	if !strings.Contains(buf.String(), "custom_tool") {
		t.Errorf("expected tool name at V, got %q", buf.String())
	}
}

func TestHandleToolResultEvent_DefaultToolDefault(t *testing.T) {
	data := client.SSEEventData{Name: "custom_tool"}

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	handleToolResultEvent(data, VerbosityDefault)

	w.Close() //nolint:errcheck
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	// Default verbosity should not show ✓ for non-special tools
	if strings.Contains(buf.String(), "✓") {
		t.Error("should not show at default verbosity")
	}
}

// ---------------------------------------------------------------------------
// finishStream
// ---------------------------------------------------------------------------

func TestFinishStream_TTYWithContent(t *testing.T) {
	var buf strings.Builder
	buf.WriteString("# Hello World")

	code := finishStream(&buf, true, true)
	if code != 0 {
		t.Errorf("finishStream returned %d, want 0", code)
	}
}

func TestFinishStream_TTYEmptyContent(t *testing.T) {
	var buf strings.Builder

	code := finishStream(&buf, false, true)
	if code != 0 {
		t.Errorf("finishStream returned %d, want 0", code)
	}
}

func TestFinishStream_NonTTYWithContent(t *testing.T) {
	var buf strings.Builder
	buf.WriteString("some content")

	code := finishStream(&buf, true, false)
	if code != 0 {
		t.Errorf("finishStream returned %d, want 0", code)
	}
}

func TestFinishStream_NonTTYNoContent(t *testing.T) {
	var buf strings.Builder

	code := finishStream(&buf, false, false)
	if code != 0 {
		t.Errorf("finishStream returned %d, want 0", code)
	}
}

// ---------------------------------------------------------------------------
// streamChat — with httptest mock SSE server
// ---------------------------------------------------------------------------

func sseServer(events []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			json.NewEncoder(w).Encode( //nolint:errcheck
				client.ChatConfigResponse{Enabled: true, Provider: "test", Model: "gpt-test"})
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			for _, e := range events {
				fmt.Fprint(w, e) //nolint:errcheck
				if ok {
					flusher.Flush()
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestStreamChat_MessageAndDone(t *testing.T) {
	msgData, _ := json.Marshal(client.SSEEventData{Content: "Hello from LLM"})
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := sseServer([]string{
		fmt.Sprintf("event: message\ndata: %s\n\n", msgData),
		fmt.Sprintf("event: done\ndata: %s\n\n", doneData),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "test-session"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0", code)
	}
}

func TestStreamChat_ErrorEvent(t *testing.T) {
	errData, _ := json.Marshal(client.SSEEventData{Error: "something went wrong"})

	srv := sseServer([]string{
		fmt.Sprintf("event: error\ndata: %s\n\n", errData),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 2 {
		t.Errorf("streamChat returned %d, want 2 for error event", code)
	}
}

func TestStreamChat_StatusEvent(t *testing.T) {
	statusData, _ := json.Marshal(client.SSEEventData{SessionID: "sess-123"})
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := sseServer([]string{
		fmt.Sprintf("event: status\ndata: %s\n\n", statusData),
		fmt.Sprintf("event: done\ndata: %s\n\n", doneData),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0", code)
	}
}

func TestStreamChat_ToolCallAndResult(t *testing.T) {
	tcArgs, _ := json.Marshal(map[string]any{"agent": "worker"})
	tcData, _ := json.Marshal(client.SSEEventData{Name: "delegate_task", Args: tcArgs})
	trResult, _ := json.Marshal(map[string]any{"phase": "Succeeded"})
	trData, _ := json.Marshal(client.SSEEventData{Name: "check_task_progress", Result: trResult})
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := sseServer([]string{
		fmt.Sprintf("event: tool_call\ndata: %s\n\n", tcData),
		fmt.Sprintf("event: tool_result\ndata: %s\n\n", trData),
		fmt.Sprintf("event: done\ndata: %s\n\n", doneData),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "do stuff", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0", code)
	}
}

func TestStreamChat_MessageThenToolCall(t *testing.T) {
	msgData, _ := json.Marshal(client.SSEEventData{Content: "Let me help"})
	tcData, _ := json.Marshal(client.SSEEventData{Name: "run_code"})
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := sseServer([]string{
		fmt.Sprintf("event: message\ndata: %s\n\n", msgData),
		fmt.Sprintf("event: tool_call\ndata: %s\n\n", tcData),
		fmt.Sprintf("event: done\ndata: %s\n\n", doneData),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "help", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0", code)
	}
}

func TestStreamChat_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal error") //nolint:errcheck
	}))
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 1 {
		t.Errorf("streamChat returned %d, want 1 for server error", code)
	}
}

func TestStreamChat_InvalidSSEData(t *testing.T) {
	srv := sseServer([]string{
		"event: message\ndata: not-json\n\n",
		fmt.Sprintf("event: done\ndata: %s\n\n", `{}`),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0 (invalid JSON should be skipped)", code)
	}
}

func TestStreamChat_TTYWithContent(t *testing.T) {
	msgData, _ := json.Marshal(client.SSEEventData{Content: "# Heading\n\nSome **bold** text"})
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := sseServer([]string{
		fmt.Sprintf("event: message\ndata: %s\n\n", msgData),
		fmt.Sprintf("event: done\ndata: %s\n\n", doneData),
	})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "s1"}

	// stdoutTTY=true triggers renderMarkdown path in finishStream
	code := streamChat(context.Background(), c, req, VerbosityDefault, false, true)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0", code)
	}
}

func TestStreamChat_EmptyStream(t *testing.T) {
	// Server sends nothing — stream ends immediately
	srv := sseServer([]string{})
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")
	req := client.ChatRequest{Message: "hello", SessionID: "s1"}

	code := streamChat(context.Background(), c, req, VerbosityDefault, false, false)
	if code != 0 {
		t.Errorf("streamChat returned %d, want 0 for empty stream", code)
	}
}

// ---------------------------------------------------------------------------
// newRunCmd
// ---------------------------------------------------------------------------

func TestNewRunCmd_Structure(t *testing.T) {
	cmd := newRunCmd()

	if cmd.Use != "run [prompt]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "run [prompt]")
	}

	for _, flag := range []string{"agent", "session", "model", "provider", "verbose"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("missing flag %q", flag)
		}
	}
}

func TestNewRunCmd_MutuallyExclusiveFlags(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(client.ChatConfigResponse{Enabled: true}) //nolint:errcheck
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--agent", "a", "--provider", "p", "--server", srv.URL, "hello"})

	err := root.Execute()
	if err == nil {
		t.Error("expected error for mutually exclusive --agent and --provider")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' error, got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// trailingAnsiPad regex
// ---------------------------------------------------------------------------

func TestTrailingAnsiPad(t *testing.T) {
	input := "hello\x1b[0m   \t  "
	got := trailingAnsiPad.ReplaceAllString(input, "")
	if strings.Contains(got, "\x1b") {
		t.Errorf("expected ANSI stripped, got %q", got)
	}
	if !strings.HasPrefix(got, "hello") {
		t.Errorf("expected prefix 'hello', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// newRunCmd integration — full chat flow through cobra
// ---------------------------------------------------------------------------

func TestNewRunCmd_OneShotViaCobra(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	msgData, _ := json.Marshal(client.SSEEventData{Content: "Hello!"})
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			json.NewEncoder(w).Encode( //nolint:errcheck
				client.ChatConfigResponse{Enabled: true, Provider: "openai", Model: "gpt-4"})
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msgData) //nolint:errcheck
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData)   //nolint:errcheck
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--session", "test-sess", "hello world"})

	// runOneShot calls os.Exit on non-zero, but 0 is fine
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewRunCmd_OneShotWithModel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			json.NewEncoder(w).Encode( //nolint:errcheck
				client.ChatConfigResponse{Enabled: true, Provider: "openai", Model: "gpt-4"})
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--model", "gpt-5", "--session", "s1", "test"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewRunCmd_OneShotWithAgent(t *testing.T) { //nolint:dupl
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			json.NewEncoder(w).Encode(client.ChatConfigResponse{Enabled: true}) //nolint:errcheck
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--agent", "my-agent", "--session", "s1", "do stuff"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewRunCmd_ChatDisabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(client.ChatConfigResponse{Enabled: false}) //nolint:errcheck
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--session", "s1", "hello"})

	err := root.Execute()
	if err == nil {
		t.Error("expected error when chat is disabled")
	}
	if err != nil && !strings.Contains(err.Error(), "disabled") {
		t.Errorf("expected 'disabled' error, got %q", err.Error())
	}
}

func TestNewRunCmd_ServerUnreachable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", "http://127.0.0.1:1", "--session", "s1", "hello"})

	err := root.Execute()
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestNewRunCmd_ProviderOnly(t *testing.T) { //nolint:dupl
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			// No provider or model set
			json.NewEncoder(w).Encode(client.ChatConfigResponse{Enabled: true}) //nolint:errcheck
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--provider", "my-prov", "--session", "s1", "hi"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewRunCmd_ModelOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			json.NewEncoder(w).Encode(client.ChatConfigResponse{Enabled: true, Model: "base-model"}) //nolint:errcheck
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--model", "custom", "--session", "s1", "hi"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewRunCmd_NoProviderNoModel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatConfigPath:
			json.NewEncoder(w).Encode(client.ChatConfigResponse{Enabled: true}) //nolint:errcheck
		case chatAPIPath:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"run", "--server", srv.URL, "--session", "s1", "hi"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runREPL — test by overriding os.Stdin
// ---------------------------------------------------------------------------

func TestRunREPL_QuitCommand(t *testing.T) {
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")

	// Feed REPL commands via a pipe
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "/quit") //nolint:errcheck
		w.Close()                //nolint:errcheck
	}()

	err := runREPL(c, "test-session", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_ExitCommand(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "/exit") //nolint:errcheck
		w.Close()                //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_HelpCommand(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "/help") //nolint:errcheck
		fmt.Fprintln(w, "/quit") //nolint:errcheck
		w.Close()                //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_SessionCommand(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "/session") //nolint:errcheck
		fmt.Fprintln(w, "/quit")    //nolint:errcheck
		w.Close()                   //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_ClearCommand(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "/clear") //nolint:errcheck
		fmt.Fprintln(w, "/quit")  //nolint:errcheck
		w.Close()                 //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_UnknownCommand(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "/unknown") //nolint:errcheck
		fmt.Fprintln(w, "/quit")    //nolint:errcheck
		w.Close()                   //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_EmptyLine(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "")      //nolint:errcheck
		fmt.Fprintln(w, "  ")    //nolint:errcheck
		fmt.Fprintln(w, "/quit") //nolint:errcheck
		w.Close()                //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_ChatMessage(t *testing.T) {
	doneData, _ := json.Marshal(client.SSEEventData{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", doneData) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "hello world") //nolint:errcheck
		fmt.Fprintln(w, "/quit")       //nolint:errcheck
		w.Close()                      //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_ChatError(t *testing.T) {
	errData, _ := json.Marshal(client.SSEEventData{Error: "boom"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", errData) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.NewWithNamespace(srv.URL, "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "hello") //nolint:errcheck
		fmt.Fprintln(w, "/quit") //nolint:errcheck
		w.Close()                //nolint:errcheck
	}()

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}

func TestRunREPL_EOF(t *testing.T) {
	c := client.NewWithNamespace("http://unused", "", "default")

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	// Close immediately — EOF
	w.Close() //nolint:errcheck

	err := runREPL(c, "s1", "", "", "", VerbosityDefault, false, false)
	if err != nil {
		t.Fatalf("runREPL error: %v", err)
	}
}
