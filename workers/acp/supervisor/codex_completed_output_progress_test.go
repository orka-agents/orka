package supervisor

import (
	"strings"
	"testing"
)

// Exercise the mapper's intentionally suppressed progress frames through the
// actual controller journal and public events handler. A dropped raw snapshot
// cannot later be presented as a complete command or MCP result.
func TestCodexCompletedOutputPreservesSkippedOmissions(t *testing.T) {
	for _, kind := range []string{"command", "mcp"} {
		for _, test := range []struct {
			name      string
			otherCall bool
			hasOutput bool
			output    any
			omitted   bool
		}{
			{name: "unsupported same-call output", hasOutput: true, output: map[string]any{"unrecognized_fragment": "withheld"}, omitted: true},
			{name: "null same-call output", hasOutput: true, omitted: true},
			{name: "metadata-only progress"},
			{name: "unrelated call output", otherCall: true, hasOutput: true, output: map[string]any{"unrecognized_fragment": "withheld"}},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				const result = "completed-output-must-retain-omission"
				pipeline := newCodexOutputTestPipeline(t)
				start := codexOutputTestStart(codexOutputTestCallID, "printf complete")
				complete := codexOutputTestComplete(codexOutputTestCallID, result, 0)
				if kind == "mcp" {
					pipeline = codexMCPOutputTestPipeline(t)
					start, _, complete = codexMCPOutputFixture(t)
					codexMCPOutputTestText(complete, result)
				}
				pipeline.update(t, start)
				progress := map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    start["toolCallId"],
				}
				if test.otherCall {
					progress["toolCallId"] = "unrelated-call"
				}
				if test.hasOutput {
					progress["rawOutput"] = test.output
				}
				sequence := pipeline.prompt.sequence
				if pipeline.update(t, progress) != nil || pipeline.prompt.sequence != sequence {
					t.Fatal("invisible progress manufactured an event or sequence gap")
				}
				mapped := pipeline.update(t, complete)
				_, body := pipeline.events(t)
				if mapped.Update.ToolCall.ContentReplace == test.omitted ||
					mapped.Update.ToolCall.ContentOmitted != test.omitted ||
					strings.Contains(body, result) == test.omitted {
					t.Fatal("completion did not preserve the exact call's skipped-output omission")
				}
			})
		}
	}
}
