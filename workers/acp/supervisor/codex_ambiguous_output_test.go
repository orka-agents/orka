package supervisor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexCompletedMCPOutputRejectsDuplicateInputIdentity(t *testing.T) {
	for _, stage := range []string{"start", "completion"} {
		for _, input := range []struct {
			name string
			raw  string
		}{
			{"server last matches", `{"server":"other","server":"orka","tool":"runtime_feedback","arguments":{}}`},
			{"server first matches", `{"server":"orka","server":"other","tool":"runtime_feedback","arguments":{}}`},
			{"tool last matches", `{"server":"orka","tool":"mutate","tool":"runtime_feedback","arguments":{}}`},
			{"tool first matches", `{"server":"orka","tool":"runtime_feedback","tool":"mutate","arguments":{}}`},
			{"repeated server", `{"server":"orka","server":"orka","tool":"runtime_feedback","arguments":{}}`},
			{"escaped server key", `{"server":"other","\u0073erver":"orka","tool":"runtime_feedback","arguments":{}}`},
			{"nested arguments", `{"server":"orka","tool":"runtime_feedback","arguments":{"scope":{"id":1,"id":2}}}`},
		} {
			t.Run(stage+"/"+input.name, func(t *testing.T) {
				pipeline := codexMCPOutputTestPipeline(t)
				start, _, complete := codexMCPOutputFixture(t)
				if stage == "start" {
					start["rawInput"] = json.RawMessage(input.raw)
				} else {
					complete["rawInput"] = json.RawMessage(input.raw)
				}
				codexMCPOutputTestText(complete, "ambiguous-private-report")
				pipeline.update(t, start)
				mapped := pipeline.update(t, complete)
				listed, body := pipeline.events(t)
				last := listed.Events[len(listed.Events)-1]
				if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace || last.ContentText != "" ||
					last.Truncation == nil || !last.Truncation.ContentTextTruncated || strings.Contains(body, "ambiguous-private-report") {
					t.Fatal("ambiguous MCP input identity authorized public output or cleared omission")
				}
			})
		}
	}
}

func TestCodexCompletedOutputRejectsDuplicateRawOutput(t *testing.T) {
	for _, kind := range []string{"terminal", "read", "search"} {
		for _, output := range []struct {
			name string
			raw  string
		}{
			{"conflicting text", `{"formatted_output":"earlier","formatted_output":"ambiguous-output","exit_code":0}`},
			{"repeated text", `{"formatted_output":"ambiguous-output","formatted_output":"ambiguous-output","exit_code":0}`},
			{"conflicting exit", `{"formatted_output":"ambiguous-output","exit_code":7,"exit_code":0}`},
			{"repeated exit", `{"formatted_output":"ambiguous-output","exit_code":0,"exit_code":0}`},
			{"escaped text key", `{"formatted_output":"earlier","\u0066ormatted_output":"ambiguous-output","exit_code":0}`},
			{"noninteger spelling stays rejected", `{"formatted_output":"ambiguous-output","exit_code":0e0}`},
		} {
			t.Run(kind+"/"+output.name, func(t *testing.T) {
				pipeline := newCodexOutputTestPipeline(t)
				start := codexOutputTestStart(codexOutputTestCallID, "Inspect workspace")
				complete := codexOutputTestComplete(codexOutputTestCallID, "", 0)
				if kind != "terminal" {
					start["kind"] = kind
					delete(start, "content")
					delete(start, "rawInput")
					delete(start, "_meta")
					delete(complete, "_meta")
				}
				complete["rawOutput"] = json.RawMessage(output.raw)
				pipeline.update(t, start)
				mapped := pipeline.update(t, complete)
				listed, body := pipeline.events(t)
				last := listed.Events[len(listed.Events)-1]
				if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace || last.ContentText != "" ||
					last.Truncation == nil || !last.Truncation.ContentTextTruncated || strings.Contains(body, "ambiguous-output") {
					t.Fatal("ambiguous command snapshot became authoritative public output")
				}
			})
		}
	}
}

func TestCodexCompletedOutputRejectsExtraTerminalStartMetadata(t *testing.T) {
	for _, extra := range []string{"terminal_output", "truncated", "unrecognized"} {
		t.Run(extra, func(t *testing.T) {
			pipeline := newCodexOutputTestPipeline(t)
			start := codexOutputTestStart(codexOutputTestCallID, "Inspect workspace")
			start["_meta"].(map[string]any)[extra] = map[string]any{"data": "unrecognized-start-output"}
			mapped := pipeline.update(t, start)
			if !mapped.Update.ToolCall.ContentOmitted {
				t.Fatal("terminal start lost its omission marker")
			}
			mapped = pipeline.update(t, codexOutputTestComplete(codexOutputTestCallID, "later-output", 0))
			listed, body := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace || last.ContentText != "" ||
				last.Truncation == nil || !last.Truncation.ContentTextTruncated || strings.Contains(body, "later-output") || strings.Contains(body, "unrecognized-start-output") {
				t.Fatal("completion cleared the omission of an unsupported terminal start")
			}
		})
	}
}

func TestCodexMCPOutputRequiresExactInputKeys(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		accepted  bool
	}{
		{name: "uppercase-only keys", raw: `{"SERVER":"orka","TOOL":"runtime_feedback","arguments":{}}`},
		{name: "Unicode alias cannot override exact server", raw: `{"server":"other","\u017ferver":"orka","tool":"runtime_feedback","arguments":{}}`},
		{name: "server alias cannot replace null", raw: `{"Server":"orka","server":null,"tool":"runtime_feedback","arguments":{}}`},
		{name: "tool alias cannot replace null", raw: `{"server":"orka","Tool":"runtime_feedback","tool":null,"arguments":{}}`},
		{name: "exact server survives Unicode alias", raw: `{"server":"orka","\u017ferver":"other","tool":"runtime_feedback","arguments":{}}`, accepted: true},
		{name: "exact server survives ASCII alias", raw: `{"Server":"other","server":"orka","tool":"runtime_feedback","arguments":{}}`, accepted: true},
		{name: "ASCII alias cannot replace exact server", raw: `{"Server":"orka","server":"other","tool":"runtime_feedback","arguments":{}}`},
		{name: "exact tool survives ASCII alias", raw: `{"server":"orka","Tool":"mutate","tool":"runtime_feedback","arguments":{}}`, accepted: true},
		{name: "ASCII alias cannot replace exact tool", raw: `{"server":"orka","Tool":"runtime_feedback","tool":"mutate","arguments":{}}`},
	} {
		for _, stage := range []string{"start", "completion"} {
			t.Run(tt.name+"/"+stage, func(t *testing.T) {
				pipeline := codexMCPOutputTestPipeline(t)
				start, _, complete := codexMCPOutputFixture(t)
				if stage == "start" {
					start["rawInput"] = json.RawMessage(tt.raw)
				} else {
					complete["rawInput"] = json.RawMessage(tt.raw)
				}
				pipeline.update(t, start)
				mapped := pipeline.update(t, complete)
				listed, _ := pipeline.events(t)
				last := listed.Events[len(listed.Events)-1]
				if tt.accepted {
					if !mapped.Update.ToolCall.ContentReplace || mapped.Update.ToolCall.ContentOmitted || last.ContentText != "Available" || last.Truncation != nil {
						t.Fatal("unknown fields replaced the exact pinned input identity")
					}
				} else if mapped.Update.ToolCall.ContentReplace || !mapped.Update.ToolCall.ContentOmitted || last.ContentText != "" || last.Truncation == nil || !last.Truncation.ContentTextTruncated {
					t.Fatal("case-folded input alias gained public output authority")
				}
			})
		}
	}
}

func TestCodexMCPOutputRejectsAmbiguousStartMarker(t *testing.T) {
	for _, meta := range []string{
		`{"IS_MCP_TOOL_CALL":true}`,
		`{"is_mcp_tool_call":false,"IS_MCP_TOOL_CALL":true}`,
		`{"is_mcp_tool_call":true,"is_mcp_tool_call":null}`,
		`{"is_mcp_tool_call":false,"is_mcp_tool_call":true}`,
	} {
		t.Run(meta, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			start, _, complete := codexMCPOutputFixture(t)
			start["_meta"] = json.RawMessage(meta)
			pipeline.update(t, start)
			mapped := pipeline.update(t, complete)
			listed, _ := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			if mapped.Update.ToolCall.ContentReplace || !mapped.Update.ToolCall.ContentOmitted || last.ContentText != "" || last.Truncation == nil || !last.Truncation.ContentTextTruncated {
				t.Fatal("ambiguous start marker gained public MCP output authority")
			}
		})
	}
}
