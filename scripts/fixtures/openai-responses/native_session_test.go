package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const nativeTestModel = "native-session-fixture"

func TestNativeSessionProvesEarlierToolDiagnostic(t *testing.T) {
	f, history, diagnostic := seedNativeSession(t)
	before := readNativeObservation(t, f)
	if before.Requests != 2 || before.ToolCalls != 1 || before.EarlierResultSeen || before.CanonicalBootstrap {
		t.Fatal("first turn did not record exactly one completed tool result")
	}
	request := nativeContinuation(history)
	response := sendNativeRequest(t, f, request)
	assertNativeAnswer(t, response, nativeSecondMarker)
	if strings.Contains(response.Body.String(), diagnostic) || strings.Contains(response.Body.String(), f.filename) {
		t.Fatal("final answer exposed the private tool diagnostic")
	}
	after := readNativeObservation(t, f)
	if after.Requests != 3 || after.ToolCalls != 1 || !after.EarlierResultSeen || after.CanonicalBootstrap {
		t.Fatal("resumed turn did not prove earlier native tool history")
	}
	public := httptest.NewRecorder()
	f.handleObservation(public, httptest.NewRequest(http.MethodGet, "/fixture/native-session", nil))
	var fields map[string]any
	if json.Unmarshal(public.Body.Bytes(), &fields) != nil || len(fields) != 4 {
		t.Fatal("native diagnostics exposed unexpected fields")
	}
	for _, value := range []string{diagnostic, f.filename, f.call.ID, nativeTestModel, "private-provider-header"} {
		if strings.Contains(public.Body.String(), value) {
			t.Fatal("native diagnostics exposed private request or result data")
		}
	}
}

func TestNativeSessionRejectsTextReconstructionAndRepeatedTools(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*nativeChatRequest, string)
	}{
		{"only user text", func(r *nativeChatRequest, result string) {
			r.Messages = slices.Delete(r.Messages, 3, 4)
			r.Messages[len(r.Messages)-1].Content = nativeContent(nativeSecondMarker + " " + result)
		}},
		{"only assistant text", func(r *nativeChatRequest, _ string) {
			r.Messages[3].Role = nativeAssistantRole
		}},
		{"only tool-call arguments", func(r *nativeChatRequest, _ string) {
			r.Messages = slices.Delete(r.Messages, 3, 4)
		}},
		{"canonical bootstrap", func(r *nativeChatRequest, _ string) {
			r.Messages[len(r.Messages)-1].Content = nativeContent(canonicalTranscriptHeader + nativeSecondMarker)
		}},
		{"changed diagnostic", func(r *nativeChatRequest, result string) {
			r.Messages[3].Content = nativeContent(result + "changed")
		}},
		{"wrong result call ID", func(r *nativeChatRequest, _ string) {
			r.Messages[3].ToolCallID = "other-call"
		}},
		{"repeated result", func(r *nativeChatRequest, _ string) {
			r.Messages = slices.Insert(r.Messages, 4, r.Messages[3])
		}},
		{"repeated call", func(r *nativeChatRequest, _ string) {
			r.Messages[2].ToolCalls = append(r.Messages[2].ToolCalls, r.Messages[2].ToolCalls[0])
		}},
		{"result after active prompt", func(r *nativeChatRequest, _ string) {
			result := r.Messages[3]
			r.Messages = append(slices.Delete(r.Messages, 3, 4), result)
		}},
		{"call after result", func(r *nativeChatRequest, _ string) {
			r.Messages[2], r.Messages[3] = r.Messages[3], r.Messages[2]
		}},
		{"changed read path", func(r *nativeChatRequest, _ string) {
			r.Messages[2].ToolCalls[0].Function.Arguments = `{"filePath":"another-file"}`
		}},
		{"old marker only", func(r *nativeChatRequest, _ string) {
			r.Messages[len(r.Messages)-1].Content = nativeContent("This active prompt has no fixture marker.")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, history, diagnostic := seedNativeSession(t)
			request := nativeContinuation(history)
			test.mutate(&request, diagnostic)
			assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
			observation := readNativeObservation(t, f)
			if observation.Requests != 3 || observation.ToolCalls != 1 || observation.EarlierResultSeen ||
				observation.CanonicalBootstrap != (test.name == "canonical bootstrap") {
				t.Fatal("unsupported continuation was accepted or caused another tool call")
			}
		})
	}
}

func TestNativeSessionNeverRetriesRead(t *testing.T) {
	t.Run("repeated initial request", func(t *testing.T) {
		f := &nativeSessionFixture{}
		request := nativeInitialRequest(nativeFirstMarker)
		first := parseNativeStream(t, sendNativeRequest(t, f, request))
		if len(first[1].Choices[0].Delta.ToolCalls) != 1 {
			t.Fatal("initial request did not issue one read")
		}
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
		observation := readNativeObservation(t, f)
		if observation.Requests != 3 || observation.ToolCalls != 1 {
			t.Fatal("repeated model requests issued another native tool call")
		}
	})
	t.Run("repeated resumed request", func(t *testing.T) {
		f, history, _ := seedNativeSession(t)
		request := nativeContinuation(history)
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeSecondMarker)
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
		observation := readNativeObservation(t, f)
		if observation.Requests != 4 || observation.ToolCalls != 1 {
			t.Fatal("replayed continuation was not counted or issued a tool")
		}
	})
}

func TestNativeSessionRequiresAdvertisedReadAndActualDiagnostic(t *testing.T) {
	t.Run("read unavailable", func(t *testing.T) {
		f := &nativeSessionFixture{}
		request := nativeInitialRequest(nativeFirstMarker)
		request.Tools[0].Function.Name = "bash"
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
		if readNativeObservation(t, f).ToolCalls != 0 {
			t.Fatal("fixture requested an unavailable native read tool")
		}
	})
	t.Run("argument echo is not a result", func(t *testing.T) {
		f := &nativeSessionFixture{}
		request := nativeInitialRequest(nativeFirstMarker)
		frames := parseNativeStream(t, sendNativeRequest(t, f, request))
		call := frames[1].Choices[0].Delta.ToolCalls[0].nativeChatCall
		request.Messages = append(request.Messages,
			nativeChatMessage{Role: nativeAssistantRole, ToolCalls: []nativeChatCall{call}},
			nativeChatMessage{Role: nativeToolRole, ToolCallID: call.ID, Content: nativeContent(call.Function.Arguments)},
		)
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
		if f.result != "" {
			t.Fatal("fixture accepted a read argument as a completed diagnostic")
		}
	})
}

func TestNativeSessionBoundsAndMethods(t *testing.T) {
	for _, name := range []string{"body", "messages", "tools", "content", "non-streaming"} {
		t.Run(name, func(t *testing.T) {
			f := &nativeSessionFixture{}
			request := nativeInitialRequest(nativeFirstMarker)
			switch name {
			case "body":
				request.Messages[1].Content = nativeContent(strings.Repeat("x", maxRequestBytes))
			case "messages":
				request.Messages = make([]nativeChatMessage, nativeMaxMessages+1)
			case "tools":
				request.Tools = make([]nativeChatCall, nativeMaxTools+1)
			case "content":
				request.Messages[1].Content = json.RawMessage(`{"text":"invalid content object"}`)
			case "non-streaming":
				request.Stream = false
			}
			response := sendNativeRequest(t, f, request)
			if response.Code != http.StatusBadRequest || readNativeObservation(t, f).ToolCalls != 0 {
				t.Fatal("invalid request was not rejected without a retryable HTTP status")
			}
		})
	}
	t.Run("tool result", func(t *testing.T) {
		f, history, _ := seedNativeSession(t)
		request := nativeContinuation(history)
		request.Messages[3].Content = nativeContent(strings.Repeat("x", nativeMaxToolResultBytes+1))
		assertNativeAnswer(t, sendNativeRequest(t, f, request), nativeFailureAnswer)
	})
	t.Run("methods", func(t *testing.T) {
		f := &nativeSessionFixture{}
		chat, observation := httptest.NewRecorder(), httptest.NewRecorder()
		f.handleChatCompletions(chat, httptest.NewRequest(http.MethodGet, "/chat/completions", nil))
		f.handleObservation(observation, httptest.NewRequest(http.MethodPost, "/fixture/native-session", nil))
		if chat.Code != http.StatusMethodNotAllowed || observation.Code != http.StatusMethodNotAllowed ||
			readNativeObservation(t, f).Requests != 0 {
			t.Fatal("unsupported fixture methods changed native scenario state")
		}
	})
}

func TestNativeSessionHoldAppliesOnlyToFinalAnswers(t *testing.T) {
	for _, test := range []struct {
		answer string
		text   string
		want   time.Duration
	}{
		{nativeFirstMarker, "ORKA_HOLD_20S " + nativeFirstMarker, 20 * time.Second},
		{nativeSecondMarker, "ORKA_HOLD_300S " + nativeSecondMarker, 20 * time.Second},
		{nativeSecondMarker, nativeSecondMarker, 0},
		{nativeFailureAnswer, "ORKA_HOLD_20S " + nativeSecondMarker, 0},
	} {
		if nativeAnswerHold(test.answer, test.text) != test.want {
			t.Fatal("native final-answer hold did not honor its bounded active prompt")
		}
	}
	// Cancellation would end a held final response before its answer. An
	// initial tool-call response must still stream immediately without a hold.
	f := &nativeSessionFixture{}
	request := nativeInitialRequest("ORKA_HOLD_20S " + nativeFirstMarker)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(body))
	f.handleChatCompletions(response, r)
	frames := parseNativeStream(t, response)
	if len(frames[1].Choices[0].Delta.ToolCalls) != 1 {
		t.Fatal("initial read was held with the final answer")
	}
}

func nativeInitialRequest(prompt string) nativeChatRequest {
	return nativeChatRequest{
		Model: nativeTestModel, Stream: true,
		Messages: []nativeChatMessage{
			{Role: "system", Content: nativeContent("The working directory is /workspace/native-session.")},
			{Role: messageRoleUser, Content: nativeContent(prompt)},
		},
		Tools: []nativeChatCall{{Type: nativeFunctionType, Function: nativeChatFunction{Name: nativeReadTool}}},
	}
}

func nativeContinuation(history []nativeChatMessage) nativeChatRequest {
	request := nativeInitialRequest(nativeSecondMarker)
	request.Messages = append(slices.Clone(history), nativeChatMessage{
		Role: messageRoleUser, Content: nativeContent(nativeSecondMarker),
	})
	return request
}

func seedNativeSession(t *testing.T) (*nativeSessionFixture, []nativeChatMessage, string) {
	t.Helper()
	f := &nativeSessionFixture{}
	request := nativeInitialRequest(nativeFirstMarker)
	frames := parseNativeStream(t, sendNativeRequest(t, f, request))
	if len(frames[1].Choices[0].Delta.ToolCalls) != 1 || *frames[2].Choices[0].FinishReason != "tool_calls" {
		t.Fatal("first native request did not stream exactly one tool call")
	}
	call := frames[1].Choices[0].Delta.ToolCalls[0].nativeChatCall
	var arguments struct {
		FilePath string `json:"filePath"`
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || call.Function.Name != nativeReadTool ||
		!strings.HasPrefix(arguments.FilePath, "orka-native-diagnostic-") || strings.ContainsAny(arguments.FilePath, "/\\") {
		t.Fatal("fixture did not request a private missing file through native read")
	}
	diagnostic := "Error: File not found: /workspace/native-session/" + arguments.FilePath + "\n"
	request.Messages = append(request.Messages,
		nativeChatMessage{Role: nativeAssistantRole, ToolCalls: []nativeChatCall{call}},
		nativeChatMessage{Role: nativeToolRole, ToolCallID: call.ID, Content: nativeContent(diagnostic)},
	)
	response := sendNativeRequest(t, f, request)
	assertNativeAnswer(t, response, nativeFirstMarker)
	if strings.Contains(response.Body.String(), arguments.FilePath) {
		t.Fatal("first assistant answer exposed the native diagnostic")
	}
	history := append(request.Messages, nativeChatMessage{
		Role: nativeAssistantRole, Content: nativeContent(nativeFirstMarker),
	})
	return f, history, diagnostic
}

func nativeContent(text string) json.RawMessage {
	encoded, _ := json.Marshal(text)
	return encoded
}

func sendNativeRequest(t *testing.T, f *nativeSessionFixture, request nativeChatRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer private-provider-header")
	response := httptest.NewRecorder()
	f.handleChatCompletions(response, r)
	return response
}

func readNativeObservation(t *testing.T, f *nativeSessionFixture) nativeSessionObservation {
	t.Helper()
	response := httptest.NewRecorder()
	f.handleObservation(response, httptest.NewRequest(http.MethodGet, "/fixture/native-session", nil))
	var observation nativeSessionObservation
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &observation) != nil {
		t.Fatal("invalid native fixture observation")
	}
	return observation
}

type nativeTestChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index int `json:"index"`
				nativeChatCall
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	} `json:"usage"`
}

func parseNativeStream(t *testing.T, response *httptest.ResponseRecorder) []nativeTestChunk {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" ||
		!response.Flushed {
		t.Fatal("fixture did not return a flushed Chat Completions event stream")
	}
	frames := strings.Split(strings.TrimSpace(response.Body.String()), "\n\n")
	if len(frames) != 4 || frames[3] != "data: [DONE]" {
		t.Fatal("native fixture stream had missing or extra events")
	}
	chunks := make([]nativeTestChunk, 3)
	for index, frame := range frames[:3] {
		data, ok := strings.CutPrefix(frame, "data: ")
		chunk := &chunks[index]
		if !ok || json.Unmarshal([]byte(data), chunk) != nil || chunk.ID == "" || chunk.Model != nativeTestModel ||
			chunk.Object != "chat.completion.chunk" || chunk.Created != 1 || len(chunk.Choices) != 1 ||
			chunk.Choices[0].Index != 0 || index > 0 && chunk.ID != chunks[0].ID {
			t.Fatal("native fixture emitted an invalid Chat Completions chunk")
		}
	}
	if chunks[0].Choices[0].Delta.Role != nativeAssistantRole || chunks[0].Choices[0].FinishReason != nil ||
		chunks[1].Choices[0].FinishReason != nil || chunks[2].Choices[0].FinishReason == nil ||
		chunks[2].Usage == nil || chunks[2].Usage.Prompt+chunks[2].Usage.Completion != chunks[2].Usage.Total {
		t.Fatal("native fixture omitted role, finish reason, or usage")
	}
	return chunks
}

func assertNativeAnswer(t *testing.T, response *httptest.ResponseRecorder, expected string) {
	t.Helper()
	chunks := parseNativeStream(t, response)
	if chunks[1].Choices[0].Delta.Content != expected || len(chunks[1].Choices[0].Delta.ToolCalls) != 0 ||
		*chunks[2].Choices[0].FinishReason != "stop" {
		t.Fatal("native fixture did not return the expected fixed final answer")
	}
}
