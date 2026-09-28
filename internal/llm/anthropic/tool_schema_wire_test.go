package anthropic

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
	for _, schema := range []string{
		`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`,
		`{"type":"object","$defs":{"precise":{"type":"number","maximum":9007199254740993,"minimum":-9007199254740995,"multipleOf":0.12345678901234567890123456789}},"properties":{"value":{"$ref":"#/$defs/precise"}},"required":["value"],"additionalProperties":false}`,
	} {
		captured := make(chan json.RawMessage, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Tools []struct {
					InputSchema json.RawMessage `json:"input_schema"`
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
			captured <- request.Tools[0].InputSchema
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg-test","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
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
}
