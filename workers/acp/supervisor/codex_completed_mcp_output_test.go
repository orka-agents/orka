package supervisor

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// These fixture notifications follow the pinned adapter sources recorded in
// testdata/codex_mcp_permission.json. The report and negative cases are synthetic
// protocol evidence, not native model execution or a live network denial.
func codexMCPOutputFixture(t *testing.T) (map[string]any, map[string]any, map[string]any) {
	t.Helper()
	data, err := os.ReadFile("testdata/codex_mcp_permission.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Notification   acp.SessionNotification `json:"notification"`
		PostPermission acp.SessionNotification `json:"postPermission"`
		Completion     acp.SessionNotification `json:"completion"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var start, progress, complete map[string]any
	for _, frame := range []struct {
		raw  json.RawMessage
		wire *map[string]any
	}{
		{fixture.Notification.Update, &start},
		{fixture.PostPermission.Update, &progress},
		{fixture.Completion.Update, &complete},
	} {
		if err := json.Unmarshal(frame.raw, frame.wire); err != nil {
			t.Fatal(err)
		}
	}
	return start, progress, complete
}

func codexMCPOutputTestPolicy() harnessv2.MCPToolPolicy {
	return harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{"runtime_feedback"},
		Tools: []harnessv2.MCPToolDescriptor{{
			Name: "runtime_feedback", Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectReadOnly,
		}},
	}
}

func codexMCPOutputTestPipeline(t *testing.T) *codexOutputTestPipeline {
	t.Helper()
	pipeline := newCodexOutputTestPipeline(t)
	pipeline.session.mcpProxy = &mcpProxySession{
		configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: codexMCPOutputTestPolicy()},
	}
	return pipeline
}

func codexMCPOutputTestText(complete map[string]any, text string) {
	complete["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{
		map[string]any{"type": "text", "text": text},
	}
}

func TestCodexCompletedMCPOutputPublicReport(t *testing.T) {
	const report = `{"status":"Collecting","attributionScope":"container","events":[{"decision":"deny","destinationAddress":"203.0.113.8","destinationPort":443,"kernelEnforced":true}],"droppedEvents":0}`
	pipeline := codexMCPOutputTestPipeline(t)
	start, progress, complete := codexMCPOutputFixture(t)
	start["title"] = "Untrusted display title"
	codexMCPOutputTestText(complete, report)
	pipeline.update(t, start)
	pipeline.update(t, progress)
	before, body := pipeline.events(t)
	if len(before.Events) == 0 || strings.Contains(body, "destinationAddress") {
		t.Fatal("MCP report became public before its terminal snapshot")
	}
	mapped := pipeline.update(t, complete)
	if !mapped.Update.ToolCall.ContentReplace || mapped.Update.ToolCall.ContentOmitted ||
		len(mapped.Update.ToolCall.Content) != 1 || mapped.Update.ToolCall.Content[0].Text != report {
		t.Fatal("completed MCP report lost its exact logical text")
	}
	listed, _ := pipeline.events(t)
	last := listed.Events[len(listed.Events)-1]
	if len(listed.Events) != len(before.Events)+1 || last.ContentText != report || last.Type != executionevents.ExecutionEventTypeToolCallCompleted ||
		listed.Events[0].ToolCallID != last.ToolCallID || last.Truncation != nil {
		t.Fatal("public tool timeline did not preserve the exact completed report and call identity")
	}
	if len(pipeline.session.permissions) != 0 {
		t.Fatal("output projection manufactured permission state")
	}
}

func TestCodexCompletedMCPOutputRejectsUnfrozenIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*codexOutputTestPipeline, map[string]any, map[string]any)
	}{
		{name: "unknown source", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.session.mcpProxy.configuration.ToolPolicy.Tools[0].Source = "unknown"
		}},
		{name: "provider native source", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.session.mcpProxy.configuration.ToolPolicy.Tools[0].Source = harnessv2.MCPToolSourceProviderNative
		}},
		{name: "mutating descriptor", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.session.mcpProxy.configuration.ToolPolicy.Tools[0].Effect = harnessv2.MCPToolEffectConsequential
		}},
		{name: "denied tool", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.session.mcpProxy.configuration.ToolPolicy.DisallowedToolNames = []string{"runtime_feedback"}
		}},
		{name: "missing frozen descriptor", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.session.mcpProxy.configuration.ToolPolicy.Tools = nil
		}},
		{name: "other server", mutate: func(_ *codexOutputTestPipeline, start, complete map[string]any) {
			start["rawInput"].(map[string]any)["server"] = "other"
			complete["rawInput"].(map[string]any)["server"] = "other"
		}},
		{name: "unrecognized tool", mutate: func(_ *codexOutputTestPipeline, start, complete map[string]any) {
			start["rawInput"].(map[string]any)["tool"] = "unknown"
			complete["rawInput"].(map[string]any)["tool"] = "unknown"
		}},
		{name: "title cannot supply identity", mutate: func(_ *codexOutputTestPipeline, start, _ map[string]any) {
			delete(start, "rawInput")
		}},
		{name: "missing MCP marker", mutate: func(_ *codexOutputTestPipeline, start, _ map[string]any) { delete(start, "_meta") }},
		{name: "different provider", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) { p.server.cfg.Provider.Kind = providerKindClaude }},
		{name: "different session profile", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.session.profile.ProviderKind = providerKindClaude
		}},
		{name: "different adapter", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) { p.server.cfg.Provider.AdapterName = "external" }},
		{name: "different pinned digest", mutate: func(p *codexOutputTestPipeline, _, _ map[string]any) {
			p.server.cfg.Provider.AdapterDigest = "sha256:" + strings.Repeat("b", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			start, _, complete := codexMCPOutputFixture(t)
			codexMCPOutputTestText(complete, "untrusted-private-report")
			test.mutate(pipeline, start, complete)
			pipeline.update(t, start)
			mapped := pipeline.update(t, complete)
			_, body := pipeline.events(t)
			if mapped.Update.ToolCall.ContentReplace || len(mapped.Update.ToolCall.Content) != 0 || strings.Contains(body, "untrusted-private-report") {
				t.Fatal("an unfrozen read-only identity gained MCP output projection")
			}
		})
	}
}

func TestCodexCompletedMCPOutputOmitsUnsupportedSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing snapshot", mutate: func(wire map[string]any) { delete(wire, "rawOutput") }},
		{name: "null snapshot", mutate: func(wire map[string]any) { wire["rawOutput"] = nil }},
		{name: "missing result", mutate: func(wire map[string]any) { delete(wire["rawOutput"].(map[string]any), "result") }},
		{name: "null result", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["result"] = nil }},
		{name: "approval denied error", mutate: func(wire map[string]any) {
			wire["status"] = "failed"
			wire["rawOutput"] = map[string]any{"result": nil, "error": map[string]any{"message": "declined"}}
		}},
		{name: "conflicting error and result", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["error"] = map[string]any{"message": "failed"}
		}},
		{name: "failed with claimed result", mutate: func(wire map[string]any) { wire["status"] = "failed" }},
		{name: "MCP error result", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["isError"] = true
		}},
		{name: "unknown snapshot metadata", mutate: func(wire map[string]any) { wire["rawOutput"].(map[string]any)["truncated"] = true }},
		{name: "unknown result metadata", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["structuredContent"] = map[string]any{"private": "value"}
		}},
		{name: "missing content", mutate: func(wire map[string]any) {
			delete(wire["rawOutput"].(map[string]any)["result"].(map[string]any), "content")
		}},
		{name: "empty content list", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{}
		}},
		{name: "multiple text blocks", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{
				map[string]any{"type": "text", "text": "sk-"}, map[string]any{"type": "text", "text": "syntheticfixtureabcdefghijklmnop"},
			}
		}},
		{name: "nontext block", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{map[string]any{"type": "image", "data": "private"}}
		}},
		{name: "unknown text metadata", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "report", "truncated": true}}
		}},
		{name: "null text", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": nil}}
		}},
		{name: "structured text", mutate: func(wire map[string]any) {
			wire["rawOutput"].(map[string]any)["result"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": []string{"report"}}}
		}},
		{name: "mismatched terminal server", mutate: func(wire map[string]any) { wire["rawInput"].(map[string]any)["server"] = "other" }},
		{name: "mismatched terminal tool", mutate: func(wire map[string]any) { wire["rawInput"].(map[string]any)["tool"] = "recall_memory" }},
		{name: "missing terminal identity", mutate: func(wire map[string]any) { delete(wire, "rawInput") }},
		{name: "oversized terminal input", mutate: func(wire map[string]any) {
			wire["rawInput"].(map[string]any)["arguments"] = strings.Repeat("x", harnessv2.MaxMCPArgumentsBytes+(1<<10))
		}},
		{name: "unknown terminal metadata", mutate: func(wire map[string]any) { wire["_meta"] = map[string]any{"terminal_output": "private"} }},
		{name: "unsupported content alongside result", mutate: func(wire map[string]any) {
			wire["content"] = []any{map[string]any{"type": "terminal", "terminalId": "feedback-call-1"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			start, _, complete := codexMCPOutputFixture(t)
			pipeline.update(t, start)
			test.mutate(complete)
			mapped := pipeline.update(t, complete)
			listed, _ := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace || last.ContentText != "" ||
				last.Truncation == nil || !last.Truncation.ContentTextTruncated {
				t.Fatal("unsupported MCP snapshot lost its complete omission marker")
			}
		})
	}
}

func TestCodexCompletedMCPOutputPreservesIdentityAndOmission(t *testing.T) {
	for _, mode := range []string{"cross-call", "fresh prompt", "duplicate start", "duplicate completion", "intervening unsupported content"} {
		t.Run(mode, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			start, progress, complete := codexMCPOutputFixture(t)
			pipeline.update(t, start)
			switch mode {
			case "cross-call":
				complete["toolCallId"] = "other-call"
			case "fresh prompt":
				pipeline.prompt = &promptState{request: pipeline.prompt.request, sequence: pipeline.prompt.sequence}
			case "duplicate start":
				pipeline.update(t, start)
			case "duplicate completion":
				pipeline.update(t, complete)
			case "intervening unsupported content":
				progress["content"] = []any{map[string]any{"type": "terminal", "terminalId": "unrecognized"}}
				pipeline.update(t, progress)
			}
			codexMCPOutputTestText(complete, "must-stay-omitted")
			mapped := pipeline.update(t, complete)
			_, body := pipeline.events(t)
			if mapped.Update.ToolCall.ContentReplace || len(mapped.Update.ToolCall.Content) != 0 || strings.Contains(body, "must-stay-omitted") {
				t.Fatal("MCP output crossed a call/prompt identity or erased an omission")
			}
		})
	}
}

func TestCodexCompletedMCPOutputRedactionAndBounds(t *testing.T) {
	const suffix = "syntheticfixtureabcdefghijklmnop"
	for _, test := range []struct {
		name, title, text string
		maxLine           int
		redacted, omitted bool
	}{
		{name: "whole secret", text: "sk-" + suffix, redacted: true},
		{name: "split title and output", title: "sk-", text: suffix, redacted: true},
		{name: "split output and title", title: suffix, text: "sk-", redacted: true},
		{name: "journal bound", text: strings.Repeat("x", executionevents.MaxExecutionEventContentTextChars+1), omitted: true},
		{name: "harness bound", text: strings.Repeat("x", harnessv2.MaxPromptContentBytes+1), omitted: true},
		{name: "event line bound", text: strings.Repeat("x", 5000), maxLine: 2048, omitted: true},
		{name: "explicit empty text"},
		{name: "exact whitespace", text: " \n\t"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			if test.maxLine != 0 {
				pipeline.server.cfg.Capabilities.Limits.MaxEventLineBytes = test.maxLine
			}
			start, _, complete := codexMCPOutputFixture(t)
			if test.title != "" {
				start["title"] = test.title
			}
			codexMCPOutputTestText(complete, test.text)
			pipeline.update(t, start)
			pipeline.update(t, complete)
			listed, body := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			switch {
			case test.redacted:
				if last.ContentText != executionevents.ExecutionEventRedactedValue || strings.Contains(body, suffix) {
					t.Fatal("MCP output changed a logical redaction boundary or exposed a synthetic secret")
				}
			case test.omitted:
				if last.ContentText != "" || last.Truncation == nil || !last.Truncation.ContentTextTruncated {
					t.Fatal("oversized MCP text lost its omission marker")
				}
			default:
				if last.ContentText != test.text || last.Truncation != nil {
					t.Fatal("complete MCP text changed before publication")
				}
			}
		})
	}
}

func TestCodexCompletedMCPOutputRedactsAcrossPriorCommand(t *testing.T) {
	const suffix = "syntheticfixtureabcdefghijklmnop"
	pipeline := codexMCPOutputTestPipeline(t)
	pipeline.update(t, codexOutputTestStart("prior-command", "printf prior"))
	pipeline.update(t, codexOutputTestComplete("prior-command", "sk-", 0))
	start, _, complete := codexMCPOutputFixture(t)
	codexMCPOutputTestText(complete, suffix)
	pipeline.update(t, start)
	pipeline.update(t, complete)
	listed, body := pipeline.events(t)
	if strings.Contains(body, suffix) || listed.Events[len(listed.Events)-1].ContentText != executionevents.ExecutionEventRedactedValue {
		t.Fatal("command-to-MCP output boundaries exposed a reconstructable synthetic secret")
	}
}
