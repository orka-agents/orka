package openai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestToolSchemaPreservedOnProviderWire(t *testing.T) {
	for _, mode := range []apiMode{apiModeResponses, apiModeChatCompletions} {
		name := "responses"
		if mode == apiModeChatCompletions {
			name = "chat-completions"
		}
		t.Run(name, func(t *testing.T) {
			for _, schema := range []string{
				`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`,
				`{"type":"object","$defs":{"precise":{"type":"number","maximum":9007199254740993,"minimum":-9007199254740995,"multipleOf":0.12345678901234567890123456789}},"properties":{"value":{"$ref":"#/$defs/precise"}},"required":["value"],"additionalProperties":false}`,
			} {
				captured := make(chan json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Tools []struct {
							Parameters json.RawMessage `json:"parameters"`
							Function   struct {
								Parameters json.RawMessage `json:"parameters"`
							} `json:"function"`
						} `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if len(request.Tools) != 1 {
						t.Error("expected one tool definition")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					raw := request.Tools[0].Parameters
					if mode == apiModeChatCompletions {
						raw = request.Tools[0].Function.Parameters
					}
					captured <- raw
					w.Header().Set("Content-Type", "application/json")
					if mode == apiModeResponses {
						_, _ = w.Write([]byte(`{"id":"resp-test","object":"response","status":"completed","model":"test-model","output":[{"id":"msg-test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}]}`))
					} else {
						_, _ = w.Write([]byte(`{"id":"chat-test","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
					}
				}))
				provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				provider.mode.Store(int32(mode))
				_, err = provider.Complete(t.Context(), &llm.CompletionRequest{
					Model: "test-model", Messages: []llm.Message{{Role: "user", Content: "read"}},
					Tools: []llm.Tool{{Name: "reviewed", Description: "Reviewed description", Parameters: json.RawMessage(schema)}},
				})
				server.Close()
				if err != nil {
					t.Fatal(err)
				}
				raw := <-captured
				var got, want any
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				if err := decoder.Decode(&got); err != nil {
					t.Fatal(err)
				}
				decoder = json.NewDecoder(bytes.NewReader([]byte(schema)))
				decoder.UseNumber()
				if err := decoder.Decode(&want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("provider wire changed reviewed schema:\ngot %s\nwant %s", raw, schema)
				}
			}
		})
	}
}
