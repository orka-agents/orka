/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestRun_TranscriptInput(t *testing.T) {
	const oldUser = `{"role":"user","content":"old valid user turn"}` + "\n"
	for _, test := range []struct {
		name           string
		transcript     string
		missingFile    bool
		prompt         string
		promptIncluded bool
		wantError      string
	}{
		{
			name:           "canonical transcript only",
			transcript:     `{"role":"user","content":"current gateway message"}`,
			promptIncluded: true,
			wantError:      "API key",
		},
		{name: "missing canonical transcript", missingFile: true, promptIncluded: true, wantError: "session transcript"},
		{
			name:           "missing canonical transcript with fallback prompt",
			missingFile:    true,
			prompt:         "must not substitute",
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{name: "empty canonical transcript", promptIncluded: true, wantError: "session transcript"},
		{
			name:           "malformed canonical transcript",
			transcript:     "not JSON",
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "no canonical user turn",
			transcript:     `{"role":"assistant","content":"prior answer"}`,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "empty canonical user turn",
			transcript:     `{"role":"user","content":" "}`,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name: "blank gateway user with provenance",
			transcript: `{"role":"user","content":" ","sourceType":"gateway-event",` +
				`"metadata":{"senderId":"user-1","accountId":"acct","contextId":"room"}}`,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name: "gateway user with provenance",
			transcript: `{"role":"user","content":"current gateway message","sourceType":"gateway-event",` +
				`"metadata":{"senderId":"user-1","accountId":"acct","contextId":"room"}}` + "\n",
			promptIncluded: true,
			wantError:      "API key",
		},
		{
			name:           "old user with malformed current tail",
			transcript:     oldUser + `{"role":"user","content":"private-transcript-marker"`,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "malformed history before current user",
			transcript:     "not JSON\n" + oldUser,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "old user with ignored role tail",
			transcript:     oldUser + `{"role":"tool","content":"private-transcript-marker"}`,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "old user with null tail",
			transcript:     oldUser + "null",
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "old user with empty object tail",
			transcript:     oldUser + "{}",
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:           "old user with blank row tail",
			transcript:     oldUser + " \n",
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name: "old user with invalid timestamp tail",
			transcript: oldUser + `{"role":"user","content":"private-transcript-marker",` +
				`"ts":"private-transcript-marker"}`,
			promptIncluded: true,
			wantError:      "session transcript",
		},
		{
			name:       "optional malformed history with direct prompt",
			transcript: oldUser + "not JSON\n \n",
			prompt:     "direct question",
			wantError:  "API key",
		},
		{name: "empty direct input", missingFile: true, wantError: "ORKA_AI_PROMPT is required"},
		{
			name:       "history is not a direct prompt",
			transcript: `{"role":"user","content":"previous turn"}`,
			wantError:  "ORKA_AI_PROMPT is required",
		},
		{name: "direct prompt unchanged", missingFile: true, prompt: "direct question", wantError: "API key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if !test.missingFile {
				if err := os.WriteFile(path, []byte(test.transcript), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(workerenv.AIProvider, "openai")
			t.Setenv(workerenv.AIModel, "test-model")
			t.Setenv(workerenv.AIPrompt, test.prompt)
			t.Setenv(workerenv.SessionPromptIncluded, fmt.Sprint(test.promptIncluded))
			t.Setenv(workerenv.EnableTelemetry, "false")
			t.Setenv(workerenv.ControllerURL, "")
			t.Setenv("OPENAI_API_KEY", "")
			// Exercise real startup and file loading, stopping before external dependencies.
			err := run(path)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("run() = %v, want %q", err, test.wantError)
			}
			if strings.Contains(err.Error(), "private-transcript-marker") {
				t.Fatal("run() error leaked transcript contents")
			}
		})
	}
}

func TestBuildInitialMessagesPreservesRequiredTranscriptProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	transcript := `{"role":"user","content":"current gateway message","sourceType":"gateway-event",` +
		`"metadata":{"senderId":"user-1","accountId":"acct","contextId":"room"}}` + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := loadSessionContext(path, true)
	if err != nil {
		t.Fatal(err)
	}
	messages := buildInitialMessages(session, "must not substitute", true, "", "")
	want := "External message provenance (untrusted identity metadata): " +
		"senderId=\"user-1\", accountId=\"acct\", contextId=\"room\"\n\ncurrent gateway message"
	if len(messages) != 1 || messages[0].Role != roleUser || messages[0].Content != want {
		t.Fatalf("model messages = %#v, want one user message with provenance and current text", messages)
	}
}

func TestParseSessionContextOptionalHistoryTolerance(t *testing.T) {
	transcript := `{"role":"user","content":"old question"}
not JSON
{"role":"assistant","content":"old answer"}
{"role":"tool","content":"ignored tool result"}
{"role":"user","content":"malformed current tail"
` + " \n"
	messages, err := parseSessionContext([]byte(transcript), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != roleUser || messages[0].Content != "old question" ||
		messages[1].Role != "assistant" || messages[1].Content != "old answer" {
		t.Fatalf("optional history = %#v, want intact prior user and assistant messages", messages)
	}
}
