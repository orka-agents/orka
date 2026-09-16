//go:build linux

package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	nativeRestoreModel          = "openai/native-restore-fixture"
	nativeRestoreSeedPrompt     = "Fetch the original lookup once, then acknowledge completion."
	nativeRestoreFollowupPrompt = "Use the saved lookup and fetch the current lookup once."
	nativeRestoreSeedAnswer     = "The original lookup is complete."
	nativeRestoreFinalAnswer    = "The saved lookup and current lookup are available."
	nativeRestoreOldTool        = "original_lookup"
	nativeRestoreNewTool        = "current_lookup"
)

// The unique original result is never returned as assistant text or supplied
// in either user prompt. Only the native tool-result row can carry it forward.
type nativeRestoreFixture struct {
	mu             sync.Mutex
	modelCalls     int
	toolCalls      int
	violation      string
	originalResult string
	currentResult  string
	upstreamBearer string
}

func newNativeRestoreFixture(t *testing.T) *nativeRestoreFixture {
	t.Helper()
	values := make([]string, 3)
	for index := range values {
		value, err := randomMCPSecret(32)
		if err != nil {
			t.Fatal(err)
		}
		values[index] = value
	}
	return &nativeRestoreFixture{
		originalResult: "original-tool-result-" + values[0],
		currentResult:  "current-tool-result-" + values[1], upstreamBearer: values[2],
	}
}

type nativeRestoreChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

//nolint:gocyclo // Each of four protocol stages checks its expected history and tool policy before responding.
func (f *nativeRestoreFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelCalls++
	reject := func(reason string) {
		f.violation = reason
		http.Error(w, "native restore fixture rejected request", http.StatusBadRequest)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" ||
		r.Header.Get("Authorization") != "Bearer "+f.upstreamBearer {
		reject("unexpected provider route or upstream credential")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	var request nativeRestoreChatRequest
	if err != nil || len(body) > 4<<20 || json.Unmarshal(body, &request) != nil ||
		request.Model != "native-restore-fixture" || !request.Stream {
		reject("unexpected provider request shape or model")
		return
	}
	counts := make(map[string]int)
	for _, message := range request.Messages {
		for _, marker := range []string{
			nativeRestoreSeedPrompt, nativeRestoreFollowupPrompt, nativeRestoreSeedAnswer,
			f.originalResult, f.currentResult,
		} {
			counts[message.Role+":"+marker] += bytes.Count(message.Content, []byte(marker))
		}
	}
	tools := make(map[string]bool)
	for _, tool := range request.Tools {
		tools[tool.Function.Name] = true
	}
	if counts["user:"+nativeRestoreSeedPrompt] != 1 {
		reject("original user message is missing or duplicated")
		return
	}
	switch f.modelCalls {
	case 1:
		if counts["user:"+nativeRestoreFollowupPrompt] != 0 || !tools["orka_"+nativeRestoreOldTool] || f.toolCalls != 0 {
			reject("unexpected initial prompt or original tool policy")
			return
		}
		writeNativeRestoreChat(w, "native-original-call", "orka_"+nativeRestoreOldTool, "")
	case 2:
		if counts["tool:"+f.originalResult] != 1 || f.toolCalls != 1 {
			reject("original model turn did not receive exactly one broker result")
			return
		}
		writeNativeRestoreChat(w, "", "", nativeRestoreSeedAnswer)
	case 3, 4:
		if counts["user:"+nativeRestoreFollowupPrompt] != 1 || counts["assistant:"+nativeRestoreSeedAnswer] != 1 ||
			counts["tool:"+f.originalResult] != 1 || tools["orka_"+nativeRestoreOldTool] || !tools["orka_"+nativeRestoreNewTool] ||
			tools["bash"] || tools["edit"] || tools["write"] || tools["apply_patch"] {
			reject("native history is missing, duplicated, or uses stale tool policy")
			return
		}
		if f.modelCalls == 3 {
			if f.toolCalls != 1 {
				reject("restoration replayed an original tool call")
				return
			}
			writeNativeRestoreChat(w, "native-current-call", "orka_"+nativeRestoreNewTool, "")
			return
		}
		if f.toolCalls != 2 || counts["tool:"+f.currentResult] != 1 {
			reject("current prompt did not use the fresh MCP binding exactly once")
			return
		}
		writeNativeRestoreChat(w, "", "", nativeRestoreFinalAnswer)
	default:
		reject("unexpected extra model request")
	}
}

func writeNativeRestoreChat(w http.ResponseWriter, callID, tool, answer string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	write := func(delta map[string]any, finish any, usage any) {
		chunk, _ := json.Marshal(map[string]any{
			"id": "native-restore-chat", "object": "chat.completion.chunk", "created": 1,
			"model":   "native-restore-fixture",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			"usage":   usage,
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
	write(map[string]any{"role": "assistant"}, nil, nil)
	finish := "stop"
	if tool != "" {
		write(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": callID, "type": "function",
			"function": map[string]string{"name": tool, "arguments": "{}"},
		}}}, nil, nil)
		finish = "tool_calls"
	} else {
		write(map[string]any{"content": answer}, nil, nil)
	}
	write(map[string]any{}, finish, map[string]int{"prompt_tokens": 13, "completion_tokens": 7, "total_tokens": 20})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}

func (f *nativeRestoreFixture) Call(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolCalls++
	result := f.originalResult
	wantTool := nativeRestoreOldTool
	if f.toolCalls == 2 {
		result, wantTool = f.currentResult, nativeRestoreNewTool
	}
	if f.toolCalls > 2 || request.Call.ToolName != wantTool {
		f.violation = "broker received an unexpected or stale tool call"
		return harnessv2.MCPBrokerCallResponse{}, errors.New("unexpected native fixture tool")
	}
	encoded, _ := json.Marshal(map[string]string{"value": result})
	return harnessv2.MCPBrokerCallResponse{
		Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: encoded,
	}, nil
}

func (f *nativeRestoreFixture) assertCounts(t *testing.T, modelCalls, toolCalls int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.violation != "" || f.modelCalls != modelCalls || f.toolCalls != toolCalls {
		t.Fatalf("fixture: model requests=%d (want %d), tool calls=%d (want %d), violation=%q",
			f.modelCalls, modelCalls, f.toolCalls, toolCalls, f.violation)
	}
}

// This test-only subprocess preserves the real OpenCode wire traffic. The
// load case removes only resume's advertisement, allowing shared ACP method
// selection to exercise the pinned binary's real session/load implementation.
// The trace contains method names only; no messages, identifiers or headers.
func TestOpenCodeNativeRestoreWireHelper(t *testing.T) {
	if os.Getenv("ORKA_NATIVE_RESTORE_WIRE_HELPER") != "1" {
		return
	}
	if err := runNativeRestoreWireHelper(); err != nil {
		_, _ = io.WriteString(os.Stderr, "native ACP wire helper failed\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func runNativeRestoreWireHelper() error {
	trace, err := os.OpenFile(os.Getenv("ORKA_NATIVE_RESTORE_TRACE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = trace.Close() }()
	command := exec.Command("/opt/opencode/bin/opencode", "--pure", "acp", "--hostname", "127.0.0.1", "--port", "0", "--no-mdns")
	command.Stderr = os.Stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	var traceMu sync.Mutex
	record := func(value string) {
		traceMu.Lock()
		defer traceMu.Unlock()
		_, _ = io.WriteString(trace, value+"\n")
	}
	go func() {
		_ = relayNativeRestoreWire(os.Stdin, stdin, false, record)
		_ = stdin.Close()
	}()
	relayErr := relayNativeRestoreWire(stdout, os.Stdout, true, record)
	return errors.Join(relayErr, command.Wait())
}

func relayNativeRestoreWire(source io.Reader, destination io.Writer, fromAgent bool, record func(string)) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		var message struct {
			Method string                     `json:"method"`
			Result map[string]json.RawMessage `json:"result"`
		}
		if json.Unmarshal(line, &message) != nil {
			return errors.New("invalid native ACP message")
		}
		if !fromAgent && strings.HasPrefix(message.Method, "session/") {
			record("client " + message.Method)
		}
		if fromAgent && message.Method == acp.MethodSessionUpdate {
			record("agent session/update")
		}
		if fromAgent && message.Method == acp.MethodRequestPermission {
			record("agent session/request_permission")
		}
		if fromAgent && os.Getenv("ORKA_NATIVE_RESTORE_FORCE_LOAD") == "1" && message.Result["agentCapabilities"] != nil {
			var wire map[string]any
			if json.Unmarshal(line, &wire) != nil {
				return errors.New("invalid initialize response")
			}
			result, _ := wire["result"].(map[string]any)
			capabilities, _ := result["agentCapabilities"].(map[string]any)
			sessions, _ := capabilities["sessionCapabilities"].(map[string]any)
			delete(sessions, "resume")
			line, _ = json.Marshal(wire)
		}
		if _, err := destination.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return scanner.Err()
}
