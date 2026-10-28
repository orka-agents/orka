package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestResponsesStreamErrorsDoNotReplay(t *testing.T) {
	for _, test := range []struct {
		name, event, message string
		status               int
	}{
		{"cached404", "", "response not found", http.StatusNotFound},
		{"event404", "", "404 response not found", http.StatusOK},
		{"metadata404", `{"type":"response.created","response":{"id":"resp-test","status":"in_progress"}}`,
			"404 response not found", http.StatusOK},
		{"output404", `{"type":"response.output_text.delta","delta":"partial"}`,
			"404 response not found", http.StatusOK},
		{"metadata tool error", `{"type":"response.created","response":{"id":"resp-test","status":"in_progress"}}`,
			"Tool use is not supported in Responses API without Harmony", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/responses" {
					t.Errorf("stream error replayed on %s", r.URL.Path)
				}
				if test.status != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": test.message}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if test.event != "" {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", test.event)
				}
				event, err := json.Marshal(map[string]string{"type": "error", "message": test.message})
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}))
			defer server.Close()
			provider, err := NewProvider(llm.ProviderConfig{APIKey: "test", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			provider.mode.Store(int32(apiModeResponses))
			chunks, err := provider.Stream(t.Context(), &llm.CompletionRequest{Model: "local"})
			if err != nil {
				t.Fatal(err)
			}
			var streamErr error
			for chunk := range chunks {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			if streamErr == nil || calls.Load() != 1 || apiMode(provider.mode.Load()) != apiModeResponses {
				t.Fatalf("stream error was replayed or hidden: calls=%d mode=%d err=%v",
					calls.Load(), provider.mode.Load(), streamErr)
			}
		})
	}
}

func TestResponsesToolsCompatibilityFallback(t *testing.T) {
	for _, mode := range []apiMode{apiModeUnknown, apiModeResponses} {
		for _, stream := range []bool{false, true} {
			for _, refusal := range []struct {
				name     string
				status   int
				message  string
				fallback bool
			}{
				{"unsupported tools", 400, "Tool use is not supported in Responses API without Harmony", true},
				{"invalid tool input", 400, "Invalid tool schema", false},
				{"authentication", 401, "Tool use is not supported in Responses API without Harmony", false},
				{"authorization", 403, "Tool use is not supported in Responses API without Harmony", false},
				{"server failure", 500, "Tool use is not supported in Responses API without Harmony", false},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/mode=%d", refusal.name, stream, mode), func(t *testing.T) {
					var paths []string
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						paths = append(paths, r.URL.Path)
						w.Header().Set("Content-Type", "application/json")
						if r.URL.Path == "/responses" {
							w.WriteHeader(refusal.status)
							_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": refusal.message, "type": "BadRequestError", "code": refusal.status}})
							return
						}
						if r.URL.Path != "/chat/completions" {
							t.Errorf("unexpected API %s", r.URL.Path)
						}
						var request struct {
							Tools []struct {
								Type     string
								Function struct {
									Name       string
									Parameters json.RawMessage
								}
							}
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Tools) != 1 || request.Tools[0].Type != "function" || request.Tools[0].Function.Name != "read_file" || !json.Valid(request.Tools[0].Function.Parameters) {
							t.Errorf("fallback dropped the declared tool: %#v %v", request, err)
						} else {
							var parameters any
							if err := json.Unmarshal(request.Tools[0].Function.Parameters, &parameters); err != nil || !reflect.DeepEqual(parameters, map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}) {
								t.Errorf("fallback changed the tool schema: %s", request.Tools[0].Function.Parameters)
							}
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprint(w, "data: {\"id\":\"cc-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":null}]}\n\n")
							_, _ = fmt.Fprint(w, "data: {\"id\":\"cc-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							return
						}

						_, _ = fmt.Fprint(w, `{"id":"cc-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`)
					}))
					defer server.Close()
					provider, err := NewProvider(llm.ProviderConfig{APIKey: "test", BaseURL: server.URL})
					if err != nil {
						t.Fatal(err)
					}
					provider.mode.Store(int32(mode))
					request := &llm.CompletionRequest{Model: "local", Messages: []llm.Message{{Role: "user", Content: "question"}}, Tools: []llm.Tool{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}}}
					var answer strings.Builder
					if stream {
						chunks, streamErr := provider.Stream(t.Context(), request)
						err = streamErr
						if err == nil {
							for chunk := range chunks {
								if chunk.Error != nil {
									err = chunk.Error
								}
								answer.WriteString(chunk.Content)
							}
						}
					} else {
						response, completeErr := provider.Complete(t.Context(), request)
						err = completeErr
						if response != nil {
							answer.WriteString(response.Content)
						}
					}
					if refusal.fallback {
						if err != nil || answer.String() != "answer" || !reflect.DeepEqual(paths, []string{"/responses", "/chat/completions"}) {
							t.Fatalf("explicit compatibility fallback failed: answer=%q paths=%v err=%v", answer.String(), paths, err)
						}
					} else if err == nil || answer.Len() != 0 {
						t.Fatalf("non-compatibility failure became success: paths=%v err=%v", paths, err)
					} else {
						for _, path := range paths {
							if path != "/responses" {
								t.Fatalf("non-compatibility failure retried on another surface: %v", paths)
							}
						}
					}
				})
			}
		}
	}
}
