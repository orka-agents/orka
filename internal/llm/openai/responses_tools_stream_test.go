package openai

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestResponsesToolsFallbackNeverReplaysAfterOutput(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\",\"output_index\":0}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"error\",\"message\":\"unsupported_api\"}\n\n")
	}))
	defer server.Close()
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	provider.mode.Store(int32(apiModeResponses))
	chunks, err := provider.Stream(t.Context(), &llm.CompletionRequest{Model: "local", Messages: []llm.Message{{Role: "user", Content: "question"}}})
	if err != nil {
		t.Fatal(err)
	}
	var answer strings.Builder
	failed := false
	for chunk := range chunks {
		answer.WriteString(chunk.Content)
		failed = failed || chunk.Error != nil
	}
	if !failed || answer.String() != "partial" || !reflect.DeepEqual(paths, []string{"/responses"}) {
		t.Fatalf("partial response was replayed or concealed: answer=%q paths=%v failed=%t", answer.String(), paths, failed)
	}
}
