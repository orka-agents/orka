package supervisor

import (
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func codexLifecycleFixture(t *testing.T, kind string) (*codexOutputTestPipeline, map[string]any, map[string]any) {
	t.Helper()
	if kind == "mcp" {
		start, _, complete := codexMCPOutputFixture(t)
		codexMCPOutputTestText(complete, "lifecycle-private-output")
		return codexMCPOutputTestPipeline(t), start, complete
	}
	start := codexOutputTestStart(codexOutputTestCallID, "Inspect workspace")
	complete := codexOutputTestComplete(codexOutputTestCallID, "lifecycle-private-output", 0)
	if kind != "terminal" {
		start["kind"] = kind
		delete(start, "content")
		delete(start, "rawInput")
		delete(start, "_meta")
		delete(complete, "_meta")
	}
	return newCodexOutputTestPipeline(t), start, complete
}

func assertCodexLifecycleOutputOmitted(t *testing.T, pipeline *codexOutputTestPipeline, mapped *harnessv2.Event) {
	t.Helper()
	listed, body := pipeline.events(t)
	last := listed.Events[len(listed.Events)-1]
	if mapped == nil || mapped.Update.ToolCall.ContentReplace || !mapped.Update.ToolCall.ContentOmitted ||
		last.ContentText != "" || last.Truncation == nil || !last.Truncation.ContentTextTruncated ||
		strings.Contains(body, "lifecycle-private-output") {
		t.Fatal("ambiguous or out-of-order envelope gained authoritative public output or cleared omission")
	}
}

func TestCodexOutputLifecycleRejectsAmbiguousOuterEnvelope(t *testing.T) {
	for _, kind := range []string{"terminal", "read", "search", "mcp"} {
		for _, stage := range []string{"start", "completion"} {
			keys := []string{"sessionUpdate", "toolCallId", "status"}
			if stage == "start" {
				keys = append(keys, "kind")
				switch kind {
				case "terminal":
					keys = append(keys, "content", "rawInput", "_meta")
				case "mcp":
					keys = append(keys, "rawInput", "_meta")
				}
			} else {
				keys = append(keys, "rawOutput")
				switch kind {
				case "terminal":
					keys = append(keys, "_meta")
				case "mcp":
					keys = append(keys, "rawInput")
				}
			}
			for _, key := range keys {
				for _, variant := range []string{"duplicate", "uppercase alias", "Unicode alias"} {
					if variant == "Unicode alias" && key != "status" && key != "sessionUpdate" && key != "kind" {
						continue
					}
					t.Run(kind+"/"+stage+"/"+key+"/"+variant, func(t *testing.T) {
						pipeline, start, complete := codexLifecycleFixture(t, kind)
						wire := start
						if stage == "completion" {
							wire = complete
						}
						value, exists := wire[key]
						if !exists {
							t.Fatalf("fixture lacks field %q", key)
						}
						raw, err := json.Marshal(wire)
						if err != nil {
							t.Fatal(err)
						}
						if variant == "duplicate" {
							encoded, err := json.Marshal(value)
							if err != nil {
								t.Fatal(err)
							}
							raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"` + key + `":` + string(encoded) + "}")
						} else {
							alias := strings.ToUpper(key)
							if variant == "Unicode alias" {
								alias = strings.ReplaceAll(strings.ReplaceAll(key, "s", "ſ"), "k", "K")
							}
							raw = []byte(strings.Replace(string(raw), `"`+key+`":`, `"`+alias+`":`, 1))
						}
						if stage == "start" {
							pipeline.rawUpdate(t, raw)
						} else {
							pipeline.update(t, start)
							assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.rawUpdate(t, raw))
						}
						// An ambiguous completion must also fence a later valid replay.
						assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.update(t, complete))
					})
				}
			}
		}
	}
}

func TestCodexOutputLifecycleRejectsTerminalFieldAliases(t *testing.T) {
	for _, tt := range []struct{ stage, old, alias string }{
		{"start", "terminalId", "TERMINALID"},
		{"start", "terminal_id", "TERMINAL_ID"},
		{"completion", "terminal_id", "TERMINAL_ID"},
		{"completion", "signal", "ſignal"},
		{"completion", "exit_code", "EXIT_CODE"},
	} {
		t.Run(tt.stage+"/"+tt.old, func(t *testing.T) {
			pipeline, start, complete := codexLifecycleFixture(t, "terminal")
			wire := start
			if tt.stage == "completion" {
				wire = complete
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), `"`+tt.old+`":`, `"`+tt.alias+`":`, 1))
			if tt.stage == "start" {
				pipeline.rawUpdate(t, raw)
			} else {
				pipeline.update(t, start)
				assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.rawUpdate(t, raw))
			}
			assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.update(t, complete))
		})
	}
}

func TestCodexOutputLifecycleRemembersPreStartFrames(t *testing.T) {
	for _, kind := range []string{"terminal", "read", "mcp"} {
		for _, tt := range []struct {
			name       string
			fields     map[string]any
			suppressed bool
		}{
			{"mapped raw output", map[string]any{"status": "in_progress", "rawOutput": map[string]any{"unexpected": "earlier"}}, false},
			{"mapped empty completion", map[string]any{"status": "completed"}, false},
			{"mapped empty progress", map[string]any{"status": "in_progress"}, false},
			{"suppressed raw output", map[string]any{"rawOutput": map[string]any{"unexpected": "earlier"}}, true},
			{"suppressed provider metadata", map[string]any{"_meta": map[string]any{"terminal_output": map[string]any{"data": "earlier"}}}, true},
			{"suppressed empty update", nil, true},
		} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				pipeline, start, complete := codexLifecycleFixture(t, kind)
				before := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": start["toolCallId"]}
				maps.Copy(before, tt.fields)
				mapped := pipeline.update(t, before)
				if tt.suppressed {
					if mapped != nil {
						t.Fatal("provider-only frame fabricated a public event")
					}
				} else {
					assertCodexLifecycleOutputOmitted(t, pipeline, mapped)
				}
				pipeline.update(t, start)
				assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.update(t, complete))
			})
		}
	}
}

func TestCodexOutputLifecycleBoundsPreStartTombstones(t *testing.T) {
	pipeline, start, complete := codexLifecycleFixture(t, "terminal")
	for index := range harnessv2.MaxRuntimeSessionTombstoneOperations + 1 {
		raw, err := json.Marshal(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "before-" + strconv.Itoa(index)})
		if err != nil {
			t.Fatal(err)
		}
		mapped, err := pipeline.server.mapRuntimeEvent(pipeline.session, pipeline.prompt, acp.PromptEvent{
			Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(), Update: &acp.SessionNotification{Update: raw},
		})
		if err != nil || mapped != nil {
			t.Fatalf("suppressed pre-start frame returned mapped=%v, err=%v", mapped != nil, err)
		}
	}
	if len(pipeline.prompt.codexCompletedOutput.calls) != harnessv2.MaxRuntimeSessionTombstoneOperations {
		t.Fatal("pre-start tombstones were not retained within the fixed bound")
	}
	pipeline.update(t, start)
	assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.update(t, complete))
}

func TestCodexOutputLifecycleInvalidatesEveryAmbiguousID(t *testing.T) {
	for _, routing := range []string{"mapped", "suppressed", "other mapped kind"} {
		for _, alias := range []string{"toolCallId", "TOOLCALLID"} {
			for _, id := range []string{"first-call", "last-call"} {
				for _, known := range []bool{false, true} {
					t.Run(routing+"/"+alias+"/"+id+"/known="+strconv.FormatBool(known), func(t *testing.T) {
						pipeline := newCodexOutputTestPipeline(t)
						start := codexOutputTestStart(id, "Inspect workspace")
						if known {
							pipeline.update(t, start)
						}
						raw := `{"sessionUpdate":"tool_call_update","toolCallId":"first-call","` + alias + `":"last-call","status":"in_progress"`
						switch routing {
						case "suppressed":
							raw += `,"sessionUpdate":"session_info_update"`
						case "other mapped kind":
							raw += `,"sessionUpdate":"plan","entries":[]`
						}
						pipeline.rawUpdate(t, []byte(raw+"}"))
						if !known {
							pipeline.update(t, start)
						}
						assertCodexLifecycleOutputOmitted(t, pipeline, pipeline.update(t, codexOutputTestComplete(id, "lifecycle-private-output", 0)))
					})
				}
			}
		}
	}
}

func TestCodexOutputLifecyclePreservesStandardContent(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run("known="+strconv.FormatBool(known)+"/empty="+strconv.FormatBool(empty), func(t *testing.T) {
				pipeline := newCodexOutputTestPipeline(t)
				if known {
					pipeline.update(t, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "standard-call", "status": "in_progress", "kind": "edit"})
				}
				content := []any{}
				if !empty {
					content = append(content, map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "standard-output"}})
				}
				mapped := pipeline.update(t, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "standard-call", "status": "completed", "content": content})
				listed, _ := pipeline.events(t)
				last := listed.Events[len(listed.Events)-1]
				if !mapped.Update.ToolCall.ContentReplace || mapped.Update.ToolCall.ContentOmitted || last.Truncation != nil || (!empty && last.ContentText != "standard-output") {
					t.Fatal("standard ACP content lost its existing replacement semantics")
				}
			})
		}
	}
}

func TestCodexOutputLifecyclePreservesKnownMetadataProgress(t *testing.T) {
	pipeline := newCodexOutputTestPipeline(t)
	pipeline.update(t, codexOutputTestStart(codexOutputTestCallID, "Inspect workspace"))
	mapped := pipeline.update(t, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": codexOutputTestCallID,
		"_meta": map[string]any{"terminal_output": map[string]any{"terminal_id": codexOutputTestCallID, "data": "partial"}}})
	if mapped != nil {
		t.Fatal("metadata-only progress fabricated a public event")
	}
	mapped = pipeline.update(t, codexOutputTestComplete(codexOutputTestCallID, "whole-output", 0))
	listed, _ := pipeline.events(t)
	last := listed.Events[len(listed.Events)-1]
	if !mapped.Update.ToolCall.ContentReplace || mapped.Update.ToolCall.ContentOmitted || last.ContentText != "whole-output" || last.Truncation != nil {
		t.Fatal("valid metadata-only progress invalidated the pinned completion")
	}
}
