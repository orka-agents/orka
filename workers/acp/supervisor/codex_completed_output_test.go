package supervisor

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Fixtures reproduce the wire shapes in codex-acp 1.1.7 at
// https://github.com/agentclientprotocol/codex-acp/tree/307d81018f7cc0c3141ddf71c7532d38310e2cfb
// src/CodexToolCallMapper.ts:81-138,539-612 and
// src/CodexEventHandler.ts:480-489,555-586. Values are harmless test data,
// not native recordings; no provider process or model is invoked by these tests.
const codexOutputTestCallID = "command-1"

func codexOutputTestStart(id, title string) map[string]any {
	return map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": id,
		"kind": "execute", "status": "in_progress", "title": title,
		"content":  []any{map[string]any{"type": "terminal", "terminalId": id}},
		"rawInput": map[string]any{"command": "printf hello", "cwd": "/workspace"},
		"_meta":    map[string]any{"terminal_info": map[string]any{"terminal_id": id, "cwd": "/workspace"}},
	}
}

func codexOutputTestComplete(id, output string, exit any) map[string]any {
	return map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "completed",
		"rawOutput": map[string]any{"formatted_output": output, "exit_code": exit},
		"_meta":     map[string]any{"terminal_exit": map[string]any{"terminal_id": id, "exit_code": exit, "signal": nil}},
	}
}

func codexOutputTestMap(t *testing.T, normalizer *codexCompletedOutputNormalizer, wire map[string]any) *harnessv2.UpdateEvent {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	notification := &acp.SessionNotification{SessionID: "codex-session", Update: raw}
	mapped, _, _, err := mapACPUpdate(notification)
	if err != nil {
		t.Fatal(err)
	}
	normalizer.normalize(notification, mapped, rememberedACPToolCall{})
	if mapped != nil {
		if err := mapped.Validate(); err != nil {
			t.Fatalf("normalized update is invalid: %v", err)
		}
	}
	return mapped
}

func TestCodexCompletedOutputPinnedCommandFixtures(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		exit   any
		status string
	}{
		{name: "stdout and stderr snapshot", output: "hello\nwarning\n", exit: 0, status: "completed"},
		{name: "failed command", output: "command failed\n", exit: 7, status: "failed"},
		{name: "unknown exit", output: "interrupted\n", status: "failed"},
		{name: "empty output", exit: 0, status: "completed"},
		{name: "whitespace output", output: " \n\t", exit: 0, status: "completed"},
		{name: "no inferred success", output: "actual payload\n", exit: 3, status: "completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var normalizer codexCompletedOutputNormalizer
			start := codexOutputTestMap(t, &normalizer, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
			if !start.ToolCall.ContentOmitted {
				t.Fatal("terminal reference should retain its original omission until authoritative completion")
			}
			wire := codexOutputTestComplete(codexOutputTestCallID, test.output, test.exit)
			wire["status"] = test.status
			complete := codexOutputTestMap(t, &normalizer, wire)
			if !complete.ToolCall.ContentReplace || complete.ToolCall.ContentOmitted {
				t.Fatal("completed snapshot was not projected as a complete replacement")
			}
			if test.output == "" {
				if len(complete.ToolCall.Content) != 0 {
					t.Fatal("explicitly empty output gained synthetic content")
				}
			} else if len(complete.ToolCall.Content) != 1 || complete.ToolCall.Content[0].Text != test.output {
				t.Fatal("completed output was changed before logical-field redaction")
			}
			if string(complete.ToolCall.Status) != test.status || start.ToolCall.ToolCallID != complete.ToolCall.ToolCallID {
				t.Fatal("completed output, actual status, or exact call identity changed")
			}
		})
	}
}

func TestCodexCompletedOutputCommandActions(t *testing.T) {
	for _, kind := range []string{"read", "search"} {
		t.Run(kind, func(t *testing.T) {
			var normalizer codexCompletedOutputNormalizer
			codexOutputTestMap(t, &normalizer, map[string]any{
				"sessionUpdate": "tool_call", "toolCallId": codexOutputTestCallID,
				"status": "in_progress", "kind": kind, "title": "Inspect workspace",
			})
			wire := codexOutputTestComplete(codexOutputTestCallID, "found\n", 0)
			delete(wire, "_meta")
			complete := codexOutputTestMap(t, &normalizer, wire)
			if !complete.ToolCall.ContentReplace || complete.ToolCall.ContentOmitted {
				t.Fatal("pinned command-action output was not projected")
			}
		})
	}
}

func TestCodexCompletedOutputIgnoresPartialMetadata(t *testing.T) {
	var normalizer codexCompletedOutputNormalizer
	codexOutputTestMap(t, &normalizer, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
	for _, mode := range []string{"terminal_output_delta", "terminal_output"} {
		mapped := codexOutputTestMap(t, &normalizer, map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": codexOutputTestCallID,
			"_meta": map[string]any{mode: map[string]any{"terminal_id": codexOutputTestCallID, "data": "partial"}},
		})
		if mapped != nil {
			t.Fatal("partial provider metadata became a public update")
		}
	}
	complete := codexOutputTestMap(t, &normalizer, codexOutputTestComplete(codexOutputTestCallID, "hello", 0))
	if !complete.ToolCall.ContentReplace || strings.Contains(complete.ToolCall.Content[0].Text, "partial") {
		t.Fatal("partial metadata contaminated the authoritative completion")
	}
}

func TestCodexCompletedOutputRejectsUnrecognizedStarts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong terminal reference", mutate: func(wire map[string]any) {
			wire["content"] = []any{map[string]any{"type": "terminal", "terminalId": "other-command"}}
		}},
		{name: "wrong terminal info", mutate: func(wire map[string]any) {
			wire["_meta"] = map[string]any{"terminal_info": map[string]any{"terminal_id": "other-command"}}
		}},
		{name: "dynamic tool", mutate: func(wire map[string]any) { delete(wire, "content") }},
		{name: "mcp tool", mutate: func(wire map[string]any) {
			wire["_meta"] = map[string]any{"is_mcp_tool_call": true}
		}},
		{name: "web search", mutate: func(wire map[string]any) {
			wire["kind"] = "search"
			delete(wire, "content")
			delete(wire, "_meta")
		}},
		{name: "terminal start already completed", mutate: func(wire map[string]any) { wire["status"] = "completed" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var normalizer codexCompletedOutputNormalizer
			start := codexOutputTestStart(codexOutputTestCallID, "Inspect workspace")
			test.mutate(start)
			codexOutputTestMap(t, &normalizer, start)
			complete := codexOutputTestMap(t, &normalizer, codexOutputTestComplete(codexOutputTestCallID, "do not expose", 0))
			if len(complete.ToolCall.Content) != 0 || complete.ToolCall.ContentReplace {
				t.Fatal("unrecognized tool rawOutput was exposed")
			}
		})
	}
}

func TestCodexCompletedOutputOmitsIncompleteOrAmbiguousSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing snapshot", mutate: func(wire map[string]any) { delete(wire, "rawOutput") }},
		{name: "null snapshot", mutate: func(wire map[string]any) { wire["rawOutput"] = nil }},
		{name: "missing text", mutate: func(wire map[string]any) { delete(wire["rawOutput"].(map[string]any), "formatted_output") }},
		{name: "null text", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["formatted_output"] = nil }},
		{name: "structured text", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["formatted_output"] = []string{"fragment"}
		}},
		{name: "truncated snapshot", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["truncated"] = true }},
		{name: "oversized snapshot", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["formatted_output"] = strings.Repeat("x", harnessv2.MaxPromptContentBytes+1)
		}},
		{name: "noninteger exit", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["exit_code"] = 1.5 }},
		{name: "string exit", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["exit_code"] = "0" }},
		{name: "oversized exit", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["exit_code"] = int64(1) << 40 }},
		{name: "missing terminal exit", mutate: func(wire map[string]any) { delete(wire, "_meta") }},
		{name: "conflicting terminal identity", mutate: func(wire map[string]any) {
			wire["_meta"].(map[string]any)["terminal_exit"].(map[string]any)["terminal_id"] = "other-command"
		}},
		{name: "conflicting terminal exit", mutate: func(wire map[string]any) {
			wire["_meta"].(map[string]any)["terminal_exit"].(map[string]any)["exit_code"] = 7
		}},
		{name: "unexpected signal", mutate: func(wire map[string]any) {
			wire["_meta"].(map[string]any)["terminal_exit"].(map[string]any)["signal"] = "SIGTERM"
		}},
		{name: "unrecognized content alongside snapshot", mutate: func(wire map[string]any) {
			wire["content"] = []any{map[string]any{"type": "terminal", "terminalId": codexOutputTestCallID}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var normalizer codexCompletedOutputNormalizer
			codexOutputTestMap(t, &normalizer, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
			complete := codexOutputTestComplete(codexOutputTestCallID, "hello", 0)
			test.mutate(complete)
			mapped := codexOutputTestMap(t, &normalizer, complete)
			if !mapped.ToolCall.ContentOmitted || mapped.ToolCall.ContentReplace || len(mapped.ToolCall.Content) != 0 {
				t.Fatal("incomplete or ambiguous output was not entirely omitted")
			}
		})
	}
}

func TestCodexCompletedOutputRequiresExactPromptCallIdentity(t *testing.T) {
	var normalizer codexCompletedOutputNormalizer
	codexOutputTestMap(t, &normalizer, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
	for _, id := range []string{"other-command", " " + codexOutputTestCallID} {
		complete := codexOutputTestMap(t, &normalizer, codexOutputTestComplete(id, "do not expose", 0))
		if len(complete.ToolCall.Content) != 0 || complete.ToolCall.ContentReplace {
			t.Fatal("output crossed exact call identity")
		}
	}
	var nextPrompt codexCompletedOutputNormalizer
	complete := codexOutputTestMap(t, &nextPrompt, codexOutputTestComplete(codexOutputTestCallID, "do not expose", 0))
	if len(complete.ToolCall.Content) != 0 || complete.ToolCall.ContentReplace {
		t.Fatal("output crossed prompt identity")
	}
	complete = codexOutputTestMap(t, &normalizer, codexOutputTestComplete(codexOutputTestCallID, "hello", 0))
	if !complete.ToolCall.ContentReplace {
		t.Fatal("correct completion was rejected after unrelated identities")
	}
	complete = codexOutputTestMap(t, &normalizer, codexOutputTestComplete(codexOutputTestCallID, "duplicate", 0))
	if !complete.ToolCall.ContentOmitted || complete.ToolCall.ContentReplace {
		t.Fatal("duplicate completion reused output authority")
	}
}

func TestCodexCompletedOutputRejectsReusedStartsAndBoundsTracking(t *testing.T) {
	var normalizer codexCompletedOutputNormalizer
	for range 2 {
		codexOutputTestMap(t, &normalizer, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
	}
	complete := codexOutputTestMap(t, &normalizer, codexOutputTestComplete(codexOutputTestCallID, "duplicate", 0))
	if !complete.ToolCall.ContentOmitted || complete.ToolCall.ContentReplace {
		t.Fatal("duplicate start reused output authority")
	}
	for index := range harnessv2.MaxRuntimeSessionTombstoneOperations + 1 {
		codexOutputTestMap(t, &normalizer, codexOutputTestStart("bounded-"+strconv.Itoa(index), "printf hello"))
	}
	if len(normalizer.calls) != harnessv2.MaxRuntimeSessionTombstoneOperations {
		t.Fatal("command identity tracking exceeded its bound")
	}
	complete = codexOutputTestMap(t, &normalizer, codexOutputTestComplete("bounded-"+strconv.Itoa(harnessv2.MaxRuntimeSessionTombstoneOperations), "untracked", 0))
	if complete.ToolCall.ContentReplace || len(complete.ToolCall.Content) != 0 {
		t.Fatal("untracked call gained output authority after tracking overflow")
	}
}

func TestCodexCompletedOutputPreservesUnexpectedContentOmission(t *testing.T) {
	var normalizer codexCompletedOutputNormalizer
	codexOutputTestMap(t, &normalizer, codexOutputTestStart(codexOutputTestCallID, "printf hello"))
	codexOutputTestMap(t, &normalizer, map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": codexOutputTestCallID, "status": "in_progress",
		"content": []any{map[string]any{"type": "terminal", "terminalId": "unrecognized-terminal"}},
	})
	complete := codexOutputTestMap(t, &normalizer, codexOutputTestComplete(codexOutputTestCallID, "hello", 0))
	if !complete.ToolCall.ContentOmitted || complete.ToolCall.ContentReplace || len(complete.ToolCall.Content) != 0 {
		t.Fatal("authoritative output erased an intervening unsupported-content omission")
	}
}
