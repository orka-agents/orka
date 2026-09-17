package supervisor

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/runtimefeedback"
)

func TestCodexCompletedMCPContractFromProxy(t *testing.T) {
	for _, replayed := range []bool{false, true} {
		name := "initial result"
		if replayed {
			name = "replayed result"
		}
		t.Run(name, func(t *testing.T) {
			report := codexMCPContractFeedbackReport(t)
			result := codexMCPContractProxyResult(t, report, replayed)
			if result["structuredContent"] == nil || (result["_meta"] != nil) != replayed {
				t.Fatal("real proxy result no longer exercises the pinned structured/metadata fields")
			}
			pipeline := codexMCPOutputTestPipeline(t)
			start, progress, complete := codexMCPOutputFixture(t)
			complete["rawOutput"] = map[string]any{"result": result, "error": nil}
			pipeline.update(t, start)
			pipeline.update(t, progress)
			_, pending := pipeline.events(t)
			if strings.Contains(pending, "destinationAddress") {
				t.Fatal("report became public before completion")
			}
			mapped := pipeline.update(t, complete)
			tool := mapped.Update.ToolCall
			if tool.ContentOmitted || !tool.ContentReplace || len(tool.Content) != 1 || tool.Content[0].Text != report {
				t.Fatal("actual proxy/pinned adapter result did not preserve its single complete text snapshot")
			}
			// Compare with the already-supported text-only snapshot through the
			// same real journal/HTTP seam. Producer support must not change the
			// existing privacy decision for this full report, including CRI URI.
			reference := codexMCPOutputTestPipeline(t)
			refStart, _, refComplete := codexMCPOutputFixture(t)
			codexMCPOutputTestText(refComplete, report)
			reference.update(t, refStart)
			reference.update(t, refComplete)
			wantEvents, _ := reference.events(t)
			gotEvents, body := pipeline.events(t)
			want := wantEvents.Events[len(wantEvents.Events)-1]
			got := gotEvents.Events[len(gotEvents.Events)-1]
			if got.ContentText != want.ContentText || got.Summary != want.Summary || got.ToolName != want.ToolName ||
				!reflect.DeepEqual(got.Truncation, want.Truncation) || got.Type != executionevents.ExecutionEventTypeToolCallCompleted {
				t.Fatal("structured result changed the existing public journal projection")
			}
			if strings.Contains(body, "structuredContent") || strings.Contains(body, "orka.replayed") ||
				len(pipeline.session.permissions) != 0 {
				t.Fatal("result metadata leaked or projection manufactured permission state")
			}
		})
	}
}

// Run the real Orka MCP proxy with the exact frozen read-only grant. The pinned
// codex-acp 307d81018f7cc0c3141ddf71c7532d38310e2cfb generated McpToolCallResult
// keeps content, structuredContent and _meta (nullable), and createMcpRawOutput
// returns that result unchanged. This envelope seam is synthetic, not a native
// model invocation or a reconstruction of a private provider stream.
func codexMCPContractProxyResult(t *testing.T, text string, replayed bool) map[string]any {
	t.Helper()
	var authorization harnessv2.PromptMCPAuthorization
	broker := MCPBrokerFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		assertCodexReadOnlyBrokerGrant(t, request, authorization)
		return harnessv2.MCPBrokerCallResponse{
			Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID,
			Result: json.RawMessage(text), Replayed: replayed,
		}, nil
	})
	proxy, endpoint := newTestMCPProxySession(t, broker, false)
	now := time.Now().UTC()
	var lease harnessv2.PromptLease
	authorization, lease = buildTestMCPAuthorization(t, proxy.fence, now,
		"runtime_feedback", harnessv2.MCPToolEffectReadOnly, false)
	proxy.configuration = authorization.Configuration()
	if err := proxy.activate(t.Context(), authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := proxy.markRunning(authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	response := decodeMCPResponse(t, doMCPRequest(t, endpoint, "credential",
		`{"jsonrpc":"2.0","id":"feedback-call-1","method":"tools/call","params":{"name":"runtime_feedback","arguments":{}}}`))
	result, ok := response.Result.(map[string]any)
	if response.Error != nil || !ok || result["isError"] != false {
		t.Fatal("synthetic feedback report did not pass the real MCP broker/proxy path")
	}
	return map[string]any{
		"content": result["content"], "structuredContent": result["structuredContent"], "_meta": result["_meta"],
	}
}

func codexMCPContractFeedbackReport(t *testing.T) string {
	t.Helper()
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	workload := runtimefeedback.Workload{
		Namespace: "orka-test", PodName: "runtime-fixture", PodUID: "pod-fixture", ContainerName: "runtime",
		ContainerID: "containerd://" + strings.Repeat("a", 64), Node: "node-fixture",
	}
	query := runtimefeedback.Query{RunID: strings.Repeat("b", 64), Workload: workload}
	report := runtimefeedback.Report{
		APIVersion: runtimefeedback.APIVersion, Kind: runtimefeedback.ReportKind, RunID: query.RunID, Workload: workload,
		Status: runtimefeedback.Collecting, SampledAt: now,
		Capture:          &runtimefeedback.Capture{StartedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9 * time.Minute)},
		Source:           runtimefeedback.Source{Name: "gkr-runtime-observer", AgentVersion: "fixture", Gadget: "trace_network", InstanceID: "observer-fixture"},
		AttributionScope: "Container", Completeness: "Partial",
		Events: []runtimefeedback.Event{{
			Timestamp: now.Add(-time.Second), Decision: "deny", DecisionReason: "policy", KernelEnforced: true,
			DestinationAddress: "203.0.113.8", DestinationPort: 443, Protocol: "TCP",
			PolicyUID: "policy-fixture", PolicyGeneration: 1, ActiveGeneration: 1,
		}},
		DroppedEvents: 0, Losses: map[string]uint64{"ringBuffer": 0},
		Limitations: []string{"Container attribution includes descendant traffic."}, Explanation: "Evidence grants no permission.",
	}
	if err := report.Validate(query, now); err != nil {
		t.Fatalf("full synthetic feedback report is invalid: %v", err)
	}
	encoded, err := json.Marshal(map[string]any{
		"execution": map[string]any{
			"taskUID": "task-fixture", "taskAttempt": 1, "promptID": "prompt-fixture", "workload": workload,
			"fence": harnessv2.Fence{
				RuntimeInstanceID: "runtime-fixture", SupervisorBootID: "boot-fixture", ControllerEpoch: 1,
				RuntimePoolUID: "pool-fixture", RuntimePoolGeneration: 1,
				RuntimeSessionUID: "session-fixture", RuntimeSessionGeneration: 1,
				RuntimeProfileDigest:       harnessv2.ProfileDigest(testDigest("profile")),
				ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
			},
		},
		"report": report, "explanation": "Container-scoped evidence grants no permission, policy change, or retry.",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestCodexCompletedMCPContractFields(t *testing.T) {
	for _, test := range []struct {
		name             string
		text             string
		structured, meta any
		accepted         bool
	}{
		{name: "nullable native fields", text: "Available", accepted: true},
		{name: "empty metadata", text: "Available", meta: map[string]any{}, accepted: true},
		{name: "producer replay marker", text: "Available", meta: map[string]any{"orka.replayed": true}, accepted: true},
		{name: "equal structured object", text: `{"value":1.0,"nested":{"ok":true}}`,
			structured: json.RawMessage(`{"nested":{"ok":true},"value":1}`), accepted: true},
		{name: "non-JSON text with structured object", text: "Available", structured: map[string]any{}},
		{name: "different structured object", text: `{"value":1}`, structured: map[string]any{"value": 2}},
		{name: "structured-only private field", text: `{"value":1}`, structured: map[string]any{"value": 1, "private": "hidden"}},
		{name: "number precision mismatch", text: `{"value":9007199254740993}`, structured: json.RawMessage(`{"value":9007199254740992}`)},
		{name: "structured array", text: `[]`, structured: []any{}},
		{name: "structured scalar", text: `true`, structured: true},
		{name: "structured string", text: `"value"`, structured: "value"},
		{name: "structured duplicate keys", text: `{"value":1}`, structured: json.RawMessage(`{"value":2,"value":1}`)},
		{name: "text duplicate keys", text: `{"value":2,"value":1}`, structured: map[string]any{"value": 1}},
		{name: "unknown metadata", text: "Available", meta: map[string]any{"private": "hidden"}},
		{name: "truncation metadata", text: "Available", meta: map[string]any{"truncated": true}},
		{name: "replay with unknown metadata", text: "Available", meta: map[string]any{"orka.replayed": true, "truncated": false}},
		{name: "false replay", text: "Available", meta: map[string]any{"orka.replayed": false}},
		{name: "nonboolean replay", text: "Available", meta: map[string]any{"orka.replayed": "true"}},
		{name: "metadata array", text: "Available", meta: []any{}},
		{name: "metadata scalar", text: "Available", meta: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			start, _, complete := codexMCPOutputFixture(t)
			codexMCPOutputTestText(complete, test.text)
			result := complete["rawOutput"].(map[string]any)["result"].(map[string]any)
			result["structuredContent"], result["_meta"] = test.structured, test.meta
			pipeline.update(t, start)
			mapped := pipeline.update(t, complete)
			tool := mapped.Update.ToolCall
			if test.accepted {
				if tool.ContentOmitted || !tool.ContentReplace || len(tool.Content) != 1 || tool.Content[0].Text != test.text {
					t.Fatal("supported pinned result did not preserve only the exact text snapshot")
				}
				return
			}
			listed, body := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			if !tool.ContentOmitted || tool.ContentReplace || len(tool.Content) != 0 || last.ContentText != "" ||
				last.Truncation == nil || !last.Truncation.ContentTextTruncated || strings.Contains(body, "hidden") {
				t.Fatal("unsupported result escaped complete omission")
			}
		})
	}
}

func TestCodexCompletedMCPContractKeepsRedactionAndBounds(t *testing.T) {
	for _, test := range []struct {
		name    string
		text    string
		maxLine int
		omitted bool
	}{
		{name: "structured text redaction", text: `{"value":"sk-syntheticfixtureabcdefghijklmnop"}`},
		{name: "event line bound", text: `{"value":"` + strings.Repeat("x", 5000) + `"}`, maxLine: 2048, omitted: true},
		{name: "journal bound", text: `{"value":"` + strings.Repeat("x", executionevents.MaxExecutionEventContentTextChars) + `"}`, omitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			if test.maxLine != 0 {
				pipeline.server.cfg.Capabilities.Limits.MaxEventLineBytes = test.maxLine
			}
			start, _, complete := codexMCPOutputFixture(t)
			complete["rawOutput"] = map[string]any{"result": codexMCPContractProxyResult(t, test.text, false), "error": nil}
			pipeline.update(t, start)
			pipeline.update(t, complete)
			listed, body := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			if test.omitted {
				if last.ContentText != "" || last.Truncation == nil || !last.Truncation.ContentTextTruncated {
					t.Fatal("structured MCP result changed an existing size/omission boundary")
				}
			} else if strings.Contains(body, "syntheticfixtureabcdefghijklmnop") ||
				!strings.Contains(last.ContentText, executionevents.ExecutionEventRedactedValue) {
				t.Fatal("structured duplicate bypassed public text redaction")
			}
		})
	}
}

// Raw messages preserve adversarial duplicate keys through the adapter envelope
// fixture. Ordinary maps would silently eliminate the ambiguity under test.
func TestCodexCompletedMCPContractRejectsDuplicateEnvelopeKeys(t *testing.T) {
	for _, test := range []struct {
		name, output string
	}{
		{name: "outer error", output: `{"error":{"message":"failed"},"result":{"content":[{"type":"text","text":"Available"}]},"error":null}`},
		{name: "outer result", output: `{"error":null,"result":null,"result":{"content":[{"type":"text","text":"Available"}]}}`},
		{name: "result content", output: `{"error":null,"result":{"content":[{"type":"image","data":"hidden"}],"content":[{"type":"text","text":"Available"}]}}`},
		{name: "result structured content", output: `{"error":null,"result":{"content":[{"type":"text","text":"{}"}],"structuredContent":{"private":"hidden"},"structuredContent":{}}}`},
		{name: "result error", output: `{"error":null,"result":{"content":[{"type":"text","text":"Available"}],"isError":true,"isError":false}}`},
		{name: "replay marker", output: `{"error":null,"result":{"content":[{"type":"text","text":"Available"}],"_meta":{"orka.replayed":false,"orka.replayed":true}}}`},
		{name: "text block", output: `{"error":null,"result":{"content":[{"type":"text","text":"hidden","text":"Available"}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipeline := codexMCPOutputTestPipeline(t)
			start, _, complete := codexMCPOutputFixture(t)
			complete["rawOutput"] = json.RawMessage(test.output)
			pipeline.update(t, start)
			mapped := pipeline.update(t, complete)
			listed, body := pipeline.events(t)
			last := listed.Events[len(listed.Events)-1]
			if !mapped.Update.ToolCall.ContentOmitted || mapped.Update.ToolCall.ContentReplace ||
				len(mapped.Update.ToolCall.Content) != 0 || last.ContentText != "" ||
				last.Truncation == nil || !last.Truncation.ContentTextTruncated || strings.Contains(body, "hidden") {
				t.Fatal("duplicate producer keys escaped complete omission")
			}
		})
	}
}
