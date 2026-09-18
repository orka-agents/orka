package supervisor

import (
	"strconv"
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestCodexCompletedOutputPublicEventsRequireExactCommandEnvelope(t *testing.T) {
	for _, test := range []struct {
		name   string
		action bool
		mutate func(map[string]any)
	}{
		{name: "action missing exit code", action: true, mutate: func(wire map[string]any) {
			delete(wire["rawOutput"].(map[string]any), "exit_code")
		}},
		{name: "terminal missing output exit code", mutate: func(wire map[string]any) {
			delete(wire["rawOutput"].(map[string]any), "exit_code")
		}},
		{name: "terminal missing terminal exit code", mutate: func(wire map[string]any) {
			delete(wire["_meta"].(map[string]any)["terminal_exit"].(map[string]any), "exit_code")
		}},
		{name: "terminal missing both exit codes", mutate: func(wire map[string]any) {
			delete(wire["rawOutput"].(map[string]any), "exit_code")
			delete(wire["_meta"].(map[string]any)["terminal_exit"].(map[string]any), "exit_code")
		}},
		{name: "terminal truncation metadata", mutate: func(wire map[string]any) {
			wire["_meta"].(map[string]any)["truncated"] = true
		}},
		{name: "terminal unknown metadata", mutate: func(wire map[string]any) {
			wire["_meta"].(map[string]any)["unsupported"] = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := newCodexOutputTestPipeline(t)
			start := codexOutputTestStart(codexOutputTestCallID, "Inspect workspace")
			// Explicit null is a supported unknown exit. Missing fields must not
			// gain that authority, even when the other exit is also unknown.
			complete := codexOutputTestComplete(codexOutputTestCallID, "unsupported-complete-output", nil)
			if test.action {
				start["kind"] = "read"
				delete(start, "content")
				delete(start, "rawInput")
				delete(start, "_meta")
				delete(complete, "_meta")
			}
			pipeline.update(t, start)
			test.mutate(complete)
			mapped := pipeline.update(t, complete)
			listed, body := pipeline.events(t)
			if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace ||
				len(listed.Events) != 2 || listed.Events[1].ContentText != "" ||
				!strings.Contains(body, "streamed_text_truncated_or_omitted") || strings.Contains(body, "unsupported-complete-output") {
				t.Fatal("unsupported command envelope became an authoritative public result")
			}
			duplicate := codexOutputTestComplete(codexOutputTestCallID, "replacement-of-unsupported-output", nil)
			if test.action {
				delete(duplicate, "_meta")
			}
			mapped = pipeline.update(t, duplicate)
			_, body = pipeline.events(t)
			if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace || strings.Contains(body, "replacement-of-unsupported-output") {
				t.Fatal("a later completion erased the unsupported snapshot's omission")
			}
		})
	}
}

func TestCodexCompletedOutputPublicEventsMarkUntrackedOutputOmitted(t *testing.T) {
	for _, scenario := range []string{"missing start", "unsupported start", "tracking overflow"} {
		for _, output := range []struct {
			name    string
			raw     any
			content bool
		}{
			{name: "null"},
			{name: "text", raw: map[string]any{"formatted_output": "withheld-untracked-output", "exit_code": 0}},
			{name: "raw alongside content", raw: map[string]any{"formatted_output": "withheld-untracked-output", "exit_code": 0}, content: true},
		} {
			t.Run(scenario+"/"+output.name, func(t *testing.T) {
				pipeline := newCodexOutputTestPipeline(t)
				switch scenario {
				case "unsupported start":
					start := codexOutputTestStart(codexOutputTestCallID, "Unknown tool")
					delete(start, "content")
					pipeline.update(t, start)
				case "tracking overflow":
					// Fill the bounded identity table without emitting unrelated
					// events; the target's real start still crosses the mapper.
					pipeline.prompt.codexCompletedOutput.calls = make(map[string]codexCommandOutputCall)
					for index := range harnessv2.MaxRuntimeSessionTombstoneOperations {
						pipeline.prompt.codexCompletedOutput.calls["previous-"+strconv.Itoa(index)] = codexCommandOutputCall{}
					}
					pipeline.update(t, codexOutputTestStart(codexOutputTestCallID, "Inspect workspace"))
				}
				complete := codexOutputTestComplete(codexOutputTestCallID, "", 0)
				complete["rawOutput"] = output.raw
				if output.content {
					complete["content"] = []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "withheld-untracked-output"}}}
				}
				mapped := pipeline.update(t, complete)
				listed, body := pipeline.events(t)
				if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace ||
					len(listed.Events) == 0 || listed.Events[len(listed.Events)-1].ContentText != "" ||
					!strings.Contains(body, "streamed_text_truncated_or_omitted") || strings.Contains(body, "withheld-untracked-output") {
					t.Fatal("untracked raw output was presented as an empty complete public result")
				}
			})
		}
	}
}
