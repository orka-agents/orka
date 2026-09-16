package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	nativeFirstMarker        = "ORKA_NATIVE_FIRST_OK"
	nativeSecondMarker       = "ORKA_NATIVE_SECOND_OK"
	nativeFailureAnswer      = "ORKA_NATIVE_FIXTURE_FAILED"
	nativeReadTool           = "read"
	nativeFunctionType       = "function"
	nativeAssistantRole      = "assistant"
	nativeToolRole           = "tool"
	nativeObjectField        = "object"
	nativeRoleField          = "role"
	nativeDeltaField         = "delta"
	nativeContentField       = "content"
	nativeMaxMessages        = 1024
	nativeMaxTools           = 512
	nativeMaxToolResultBytes = 4096
)

type nativeSessionObservation struct {
	Requests           uint64 `json:"requests"`
	ToolCalls          uint64 `json:"toolCalls"`
	EarlierResultSeen  bool   `json:"earlierResultSeen"`
	CanonicalBootstrap bool   `json:"canonicalBootstrap"`
}

// One fixture process exercises one cold-resume scenario. Only the counters
// below are public; the generated filename and exact tool diagnostic remain
// in memory and never enter logs or assistant answers.
type nativeSessionFixture struct {
	mu          sync.Mutex
	observation nativeSessionObservation
	call        nativeChatCall
	filename    string
	result      string
}

type nativeChatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type nativeChatCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type"`
	Function nativeChatFunction `json:"function"`
}

type nativeChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []nativeChatCall `json:"tool_calls,omitempty"`
}

type nativeChatRequest struct {
	Model    string              `json:"model"`
	Stream   bool                `json:"stream"`
	Messages []nativeChatMessage `json:"messages"`
	Tools    []nativeChatCall    `json:"tools,omitempty"`
}

func (f *nativeSessionFixture) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.observation.Requests++
	requestNumber := f.observation.Requests
	f.mu.Unlock()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	var request nativeChatRequest
	if err != nil || json.Unmarshal(body, &request) != nil || !request.Stream ||
		strings.TrimSpace(request.Model) == "" || len(request.Model) > 512 ||
		len(request.Messages) == 0 || len(request.Messages) > nativeMaxMessages || len(request.Tools) > nativeMaxTools {
		http.Error(w, "invalid native session request", http.StatusBadRequest)
		return
	}
	texts := make([]string, len(request.Messages))
	latestUser := -1
	canonical := false
	for index, message := range request.Messages {
		text, ok := nativeChatText(message.Content)
		if !ok {
			http.Error(w, "invalid native session message", http.StatusBadRequest)
			return
		}
		texts[index] = text
		canonical = canonical || strings.Contains(text, "Orka canonical session transcript")
		if message.Role == messageRoleUser {
			latestUser = index
		}
	}
	f.mu.Lock()
	f.observation.CanonicalBootstrap = f.observation.CanonicalBootstrap || canonical
	answer, call := f.next(requestNumber, request, texts, latestUser)
	f.mu.Unlock()
	hold := time.Duration(0)
	if call == nil && latestUser >= 0 {
		hold = nativeAnswerHold(answer, texts[latestUser])
	}
	writeNativeChat(w, r, request.Model, answer, call, hold)
}

func nativeAnswerHold(answer, text string) time.Duration {
	if answer != nativeFirstMarker && answer != nativeSecondMarker {
		return 0
	}
	return min(requestHold([]byte(text)), 20*time.Second)
}

func (f *nativeSessionFixture) handleObservation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	observation := f.observation
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, observation)
}

// A successful run has exactly three model requests: issue one read, consume
// its completed diagnostic, then observe that diagnostic in the resumed turn.
// Repeated requests fail without issuing another tool call.
func (f *nativeSessionFixture) next(
	number uint64, request nativeChatRequest, texts []string, latestUser int,
) (string, *nativeChatCall) {
	if number != f.observation.Requests || f.observation.CanonicalBootstrap || latestUser < 0 {
		return nativeFailureAnswer, nil
	}
	marker := nativeActiveMarker(texts[latestUser])
	switch number {
	case 1:
		if marker != nativeFirstMarker || !nativeReadAvailable(request.Tools) {
			return nativeFailureAnswer, nil
		}
		for _, message := range request.Messages {
			if message.Role == nativeToolRole || len(message.ToolCalls) != 0 {
				return nativeFailureAnswer, nil
			}
		}
		f.filename = "orka-native-diagnostic-" + rand.Text()
		arguments, _ := json.Marshal(map[string]string{"filePath": f.filename})
		f.call = nativeChatCall{
			ID: "call_orka_native_diagnostic", Type: nativeFunctionType,
			Function: nativeChatFunction{Name: nativeReadTool, Arguments: string(arguments)},
		}
		f.observation.ToolCalls++
		call := f.call
		return "", &call
	case 2, 3:
		result, resultIndex, ok := f.toolResult(request.Messages, texts)
		if !ok {
			return nativeFailureAnswer, nil
		}
		if number == 2 {
			if marker != nativeFirstMarker || resultIndex <= latestUser || !f.validDiagnostic(result) {
				return nativeFailureAnswer, nil
			}
			f.result = result
			return nativeFirstMarker, nil
		}
		if marker != nativeSecondMarker || resultIndex >= latestUser || f.result == "" || result != f.result {
			return nativeFailureAnswer, nil
		}
		f.observation.EarlierResultSeen = true
		return nativeSecondMarker, nil
	default:
		return nativeFailureAnswer, nil
	}
}

func nativeActiveMarker(text string) string {
	first, second := strings.LastIndex(text, nativeFirstMarker), strings.LastIndex(text, nativeSecondMarker)
	if second > first {
		return nativeSecondMarker
	}
	if first >= 0 {
		return nativeFirstMarker
	}
	return ""
}

func nativeReadAvailable(tools []nativeChatCall) bool {
	count := 0
	for _, tool := range tools {
		if tool.Type == nativeFunctionType && tool.Function.Name == nativeReadTool {
			count++
		}
	}
	return count == 1
}

func (f *nativeSessionFixture) toolResult(messages []nativeChatMessage, texts []string) (string, int, bool) {
	if f.call.ID == "" {
		return "", -1, false
	}
	callIndex, resultIndex := -1, -1
	result := ""
	for index, message := range messages {
		for _, call := range message.ToolCalls {
			var arguments struct {
				FilePath string `json:"filePath"`
			}
			if callIndex >= 0 || message.Role != nativeAssistantRole || call.ID != f.call.ID ||
				call.Type != nativeFunctionType || call.Function.Name != nativeReadTool ||
				json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || arguments.FilePath != f.filename {
				return "", -1, false
			}
			callIndex = index
		}
		if message.Role == nativeToolRole {
			if resultIndex >= 0 || callIndex < 0 || callIndex >= index || message.ToolCallID != f.call.ID ||
				len(texts[index]) == 0 || len(texts[index]) > nativeMaxToolResultBytes {
				return "", -1, false
			}
			resultIndex, result = index, texts[index]
		}
	}
	return result, resultIndex, callIndex >= 0 && resultIndex >= 0
}

func (f *nativeSessionFixture) validDiagnostic(result string) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	filename, ok := strings.CutPrefix(strings.TrimPrefix(line, "Error: "), "File not found: ")
	return ok && path.IsAbs(filename) && path.Clean(filename) == filename && path.Base(filename) == f.filename
}

func nativeChatText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil || len(parts) > nativeMaxMessages {
		return "", false
	}
	var combined strings.Builder
	for _, part := range parts {
		if part.Type != "text" {
			return "", false
		}
		combined.WriteString(part.Text)
	}
	return combined.String(), true
}

func writeNativeChat(
	w http.ResponseWriter, r *http.Request, model, answer string, call *nativeChatCall, hold time.Duration,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	id := fmt.Sprintf("chatcmpl_orka_native_%d", responseSequence.Add(1))
	write := func(delta map[string]any, finish any, usage any) {
		chunk, _ := json.Marshal(map[string]any{
			"id": id, nativeObjectField: "chat.completion.chunk", "created": 1, responseModelField: model,
			"choices": []any{map[string]any{"index": 0, nativeDeltaField: delta, "finish_reason": finish}},
			"usage":   usage,
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
	}
	write(map[string]any{nativeRoleField: nativeAssistantRole}, nil, nil)
	finish := "stop"
	if call != nil {
		write(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": call.ID, "type": call.Type, "function": call.Function,
		}}}, nil, nil)
		finish = "tool_calls"
	} else {
		holdBeforeCompletion(r.Context(), w, answer, hold, true)
		if r.Context().Err() != nil {
			return
		}
		write(map[string]any{nativeContentField: answer}, nil, nil)
	}
	write(map[string]any{}, finish, map[string]int{"prompt_tokens": 13, "completion_tokens": 7, "total_tokens": 20})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
