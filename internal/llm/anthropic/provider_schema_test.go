package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestCompletePreservesToolSchemaKeywordsOnHTTPWire(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"runtime":{"type":"string"},"prompt":{"$ref":"#/$defs/prompt"}},"required":["runtime"],"$defs":{"prompt":{"type":"string"}},"allOf":[{"if":{"properties":{"runtime":{"const":"opencode"}},"required":["runtime"]},"then":{"properties":{"prompt":{"maxLength":32766}}}}],"additionalProperties":false}`)
	captured := make(chan json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Tools) != 1 {
			http.Error(w, "expected one tool definition", http.StatusBadRequest)
			return
		}
		captured <- request.Tools[0].InputSchema
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg-schema","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), &llm.CompletionRequest{
		Model: "test-model", MaxTokens: 64, Messages: []llm.Message{{Role: "user", Content: "Check the tool schema."}},
		Tools: []llm.Tool{{Name: "schema-test", Description: "Schema preservation fixture", Parameters: schema}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(schema, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(<-captured, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool schema changed on the HTTP wire: got %#v, want %#v", got, want)
	}
}
