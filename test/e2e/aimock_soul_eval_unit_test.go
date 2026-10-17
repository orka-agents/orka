//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/agentcontext"
)

// The soul eval oracle must accept the real composed prompt and reject each
// delivery defect it exists to catch.
func TestSoulEvalCheckControls(t *testing.T) {
	const marker = "[e2e:soul-eval-unit-first]"
	role := "SOUL_EVAL_ROLE_UNIT Answer briefly."
	first := newSoulEvalRevision("unit", "unit", "v1")
	second := newSoulEvalRevision("unit", "unit", "v2")
	composed := func(revision soulEvalRevision) string {
		soul := &agentcontext.ResolvedSoul{Text: revision.text, Digest: agentcontext.Digest(revision.text)}
		return agentcontext.Compose(role, soul)
	}
	persona := strings.TrimPrefix(composed(first), role)
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read"}}}
	// raw builds a captured request to path whose fixture matched fixtureMarker.
	raw := func(path, fixtureMarker string, body map[string]any) mockLLMEvalRequest {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		var journal mockLLMRequest
		if err := json.Unmarshal([]byte(`{"body":`+string(encoded)+`}`), &journal); err != nil {
			t.Fatal(err)
		}
		fixture, err := json.Marshal(map[string]any{"match": map[string]any{"userMessage": fixtureMarker}})
		if err != nil {
			t.Fatal(err)
		}
		journal.Response.Fixture = fixture
		return mockLLMEvalRequest{capture: mockLLMCaptureEntry{Path: path, Body: string(encoded)}, journal: journal}
	}
	// chat builds a Chat Completions Agent turn; title builds a tool-free one.
	chat := func(fixtureMarker, system, user string) mockLLMEvalRequest {
		return raw("/v1/chat/completions", fixtureMarker, map[string]any{"tools": tools, "messages": []any{
			map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user},
		}})
	}
	title := func(fixtureMarker, system, user string) mockLLMEvalRequest {
		return raw("/v1/chat/completions", fixtureMarker, map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user},
		}})
	}
	user := func(text string) []any { return []any{map[string]any{"role": "user", "content": text}} }
	valid := chat(marker, composed(first), "hi "+marker)
	const chatPath, messagesPath, responsesPath = "/v1/chat/completions", "/v1/messages", "/v1/responses"
	anthropic := raw(messagesPath, marker, map[string]any{"tools": tools, "messages": user("hi " + marker),
		"system": []any{
			map[string]any{"type": "text", "text": "preset"}, map[string]any{"type": "text", "text": composed(first)},
		}})
	responses := raw(responsesPath, marker, map[string]any{"tools": tools, "input": append([]any{map[string]any{
		"role": "developer", "content": []any{map[string]any{"type": "input_text", "text": composed(first)}},
	}}, user("hi "+marker)...)})
	metadata := raw(chatPath, marker, map[string]any{
		"tools": tools, "messages": user("hi " + marker), "metadata": map[string]any{"debug": composed(first)},
	})
	// Each protocol's instruction field is not an instruction field elsewhere.
	chatInstructions := raw(chatPath, marker, map[string]any{
		"tools": tools, "messages": user("hi " + marker), "instructions": composed(first),
	})
	responsesSystem := raw(responsesPath, marker, map[string]any{
		"tools": tools, "input": user("hi " + marker), "system": composed(first),
	})
	messagesSystemRole := raw(messagesPath, marker, map[string]any{"tools": tools, "messages": append([]any{
		map[string]any{"role": "system", "content": composed(first)}}, user("hi "+marker)...)})
	// Content parts count only with their protocol's text type.
	anthropicWrongPart := raw(messagesPath, marker, map[string]any{"tools": tools, "messages": user("hi " + marker),
		"system": []any{map[string]any{"type": "input_text", "text": composed(first)}}})
	responsesUntypedPart := raw(responsesPath, marker, map[string]any{"tools": tools, "input": append([]any{
		map[string]any{"role": "developer", "content": []any{map[string]any{"text": composed(first)}}},
	}, user("hi "+marker)...)})
	responsesInstructionParts := raw(responsesPath, marker, map[string]any{"tools": tools, "input": user("hi " + marker),
		"instructions": []any{map[string]any{"type": "input_text", "text": composed(first)}}})
	responsesInstructions := raw(responsesPath, marker, map[string]any{
		"tools": tools, "input": user("hi " + marker), "instructions": composed(first),
	})
	reversed := strings.TrimPrefix(persona, "\n\n") + "\n\n" + role
	rewritten := strings.Replace(composed(first), "$$", "$", 1)
	other := "[e2e:other]"
	for _, tc := range []struct {
		name     string
		requests []mockLLMEvalRequest
		want     string
	}{
		{"chat system prompt", []mockLLMEvalRequest{valid}, ""},
		{"anthropic system blocks", []mockLLMEvalRequest{anthropic}, ""},
		{"responses developer input", []mockLLMEvalRequest{responses}, ""},
		{"tool-free AI worker turn", []mockLLMEvalRequest{title(marker, composed(first), "hi "+marker)}, ""},
		{"auxiliary title request", []mockLLMEvalRequest{valid, title(marker, "title", "hi "+marker)}, ""},
		{"unscripted Agent request", []mockLLMEvalRequest{valid, chat(other, composed(first), "hi")}, ""},
		{"no scripted request", []mockLLMEvalRequest{chat(other, composed(first), "hi "+marker)}, "no request served"},
		{"instructions dropped", []mockLLMEvalRequest{chat(marker, "system", "hi "+marker)}, "0 times"},
		{"dropped after a valid turn", []mockLLMEvalRequest{valid, chat(marker, "system", "hi "+marker)}, "0 times"},
		{"role without persona", []mockLLMEvalRequest{chat(marker, role, "hi "+marker)}, "0 times"},
		{"duplicated persona", []mockLLMEvalRequest{chat(marker, composed(first)+persona, "hi "+marker)}, "2 times"},
		{"persona before role", []mockLLMEvalRequest{chat(marker, reversed, "hi "+marker)}, "framing"},
		{"rewritten persona", []mockLLMEvalRequest{chat(marker, rewritten, "hi "+marker)}, "persona bytes"},
		{"persona as user content", []mockLLMEvalRequest{chat(marker, "system", composed(first)+" "+marker)},
			"outside the model instructions"},
		{"persona in metadata", []mockLLMEvalRequest{metadata}, "outside the model instructions"},
		{"chat top-level instructions", []mockLLMEvalRequest{chatInstructions}, "outside the model instructions"},
		{"responses top-level system", []mockLLMEvalRequest{responsesSystem}, "outside the model instructions"},
		{"messages system role", []mockLLMEvalRequest{messagesSystemRole}, "outside the model instructions"},
		{"anthropic wrong part type", []mockLLMEvalRequest{anthropicWrongPart}, "outside the model instructions"},
		{"responses untyped part", []mockLLMEvalRequest{responsesUntypedPart}, "outside the model instructions"},
		{"responses instructions string", []mockLLMEvalRequest{responsesInstructions}, ""},
		{"responses instruction parts", []mockLLMEvalRequest{responsesInstructionParts}, "outside the model instructions"},
		{"auxiliary full instructions", []mockLLMEvalRequest{valid, title(other, composed(first), "hi")}, ""},
		{"auxiliary persona", []mockLLMEvalRequest{valid, title(other, persona, "hi")}, "without the role instructions"},
		{"unbound revision", []mockLLMEvalRequest{valid, title(other, "title", second.canary)}, "not bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := soulEvalCheck(tc.requests, marker, role, first, second)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("valid delivery rejected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
