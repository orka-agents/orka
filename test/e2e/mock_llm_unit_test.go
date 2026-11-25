//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"testing"
)

func TestMockLLMRequestBodySizeCoversRecordedAndTruncatedBodies(t *testing.T) {
	var journal []mockLLMRequest
	if err := json.Unmarshal([]byte(`[
		{"timestamp":1,"body":{"model":"m","messages":[{"role":"user","content":"hi"}]}},
		{"timestamp":2,"body":{"__aimock_truncated":true,"originalByteSize":70000}}
	]`), &journal); err != nil {
		t.Fatal(err)
	}
	recorded := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if journal[0].Body.Size != len(recorded) || journal[0].Body.Model != "m" || len(journal[0].Body.Messages) != 1 {
		t.Fatalf("recorded body = %+v, want size %d", journal[0].Body, len(recorded))
	}
	if journal[1].Body.Size != 70000 {
		t.Fatalf("truncated body size = %d, want its original size", journal[1].Body.Size)
	}
}
