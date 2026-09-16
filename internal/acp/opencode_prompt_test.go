package acp

import (
	"strings"
	"testing"
)

func TestOpenCodeSystemPromptLiteralBoundary(t *testing.T) {
	for _, prompt := range []string{"", "Research current events and cite source URLs.\nKeep publication and update dates separate.", `Return JSON with {"title":"value"}.`, "Summarize café news accurately."} {
		if err := ValidateOpenCodeSystemPrompt(prompt); err != nil {
			t.Fatalf("valid literal prompt was rejected: %v", err)
		}
	}
	for _, prompt := range []string{"{env:PRIVATE_TEST_SENTINEL}", "{file:/private/test-sentinel}", "{ ENV :PRIVATE_TEST_SENTINEL}", "{{file:../test-sentinel}}", strings.Repeat("<", MaxOpenCodeSystemPromptEncodedBytes/2), strings.Repeat("x", MaxOpenCodeSystemPromptEncodedBytes), string([]byte{0xff})} {
		err := ValidateOpenCodeSystemPrompt(prompt)
		if err == nil {
			t.Fatal("unsafe or oversized prompt was accepted")
		}
		if strings.Contains(err.Error(), "PRIVATE_TEST_SENTINEL") || strings.Contains(err.Error(), "test-sentinel") {
			t.Fatal("validation error disclosed prompt content")
		}
	}
}
