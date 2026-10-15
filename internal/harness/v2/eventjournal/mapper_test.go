package eventjournal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
)

const (
	mapperTestToolCallID    = "call-1"
	mapperTestToolKind      = "file_read"
	mapperTestDone          = "done"
	mapperTestNamespace     = "default"
	mapperTestTaskName      = "task-1"
	mapperTestSessionName   = "session-1"
	mapperTestAgentName     = "agent-1"
	mapperTestToolKindShell = "shell"
	mapperTestProvider      = "openai"
	mapperTestSecretPrefix  = "sk-"
	mapperTestServedModel   = "served-model"
	mapperTestPromptID      = "prompt-1"
)

func TestMapUpdateMapsACPUpdateKinds(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	mapCtx := testMapContext()
	tests := []struct {
		name         string
		update       harnessv2.UpdateEvent
		wantType     string
		wantSeverity string
		wantToolName string
		wantToolID   string
	}{
		{
			name: "assistant message",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk,
				AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "hello from the agent"}},
			wantType: executionevents.ExecutionEventTypeModelMessage, wantSeverity: executionevents.ExecutionEventSeverityInfo,
		},
		{
			name: "tool started",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: mapperTestToolCallID, Title: "Read the repository", Kind: mapperTestToolKind, Status: harnessv2.ToolCallStatusPending,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "README.md"}},
			}},
			wantType: executionevents.ExecutionEventTypeToolCallStarted, wantSeverity: executionevents.ExecutionEventSeverityInfo,
			wantToolName: mapperTestToolKind, wantToolID: safeMappedToolCallID(mapperTestToolCallID),
		},
		{
			name: "tool completed",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: mapperTestToolCallID, Title: "Read the repository", Kind: mapperTestToolKind, Status: harnessv2.ToolCallStatusCompleted,
			}},
			wantType: executionevents.ExecutionEventTypeToolCallCompleted, wantSeverity: executionevents.ExecutionEventSeverityInfo,
			wantToolName: mapperTestToolKind, wantToolID: safeMappedToolCallID(mapperTestToolCallID),
		},
		{
			name: "tool failed",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-2", Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusFailed,
			}},
			wantType: executionevents.ExecutionEventTypeToolCallFailed, wantSeverity: executionevents.ExecutionEventSeverityError,
			wantToolName: mapperTestToolKindShell, wantToolID: safeMappedToolCallID("call-2"),
		},
		{
			name: "plan",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
				{Content: "inspect", Status: harnessv2.PlanEntryCompleted},
				{Content: "implement", Status: harnessv2.PlanEntryInProgress},
			}}},
			wantType: executionevents.ExecutionEventTypePlanUpdated, wantSeverity: executionevents.ExecutionEventSeverityInfo,
		},
		{
			name: "usage",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage,
				Usage: &harnessv2.UsageUpdate{InputTokens: 120, OutputTokens: 30, CachedInputTokens: new(uint64(40))}},
			wantType: executionevents.ExecutionEventTypeModelUsageUpdated, wantSeverity: executionevents.ExecutionEventSeverityInfo,
		},
		{
			name: "retryable diagnostic",
			update: harnessv2.UpdateEvent{Kind: harnessv2.UpdateDiagnostic,
				Diagnostic: &harnessv2.DiagnosticUpdate{Code: "provider_retry", Message: "retrying", Retryable: true}},
			wantType: executionevents.ExecutionEventTypeAgentRuntimeCommandStarted, wantSeverity: executionevents.ExecutionEventSeverityWarning,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), test.update)
			mapped, err := mapUpdate(event, mapCtx, mapUpdateOptions{})
			if err != nil {
				t.Fatalf("mapUpdate() error = %v", err)
			}
			if mapped.Type != test.wantType || mapped.Severity != test.wantSeverity {
				t.Fatalf("mapped type/severity = %s/%s, want %s/%s", mapped.Type, mapped.Severity, test.wantType, test.wantSeverity)
			}
			if mapped.ToolName != test.wantToolName || mapped.ToolCallID != test.wantToolID {
				t.Fatalf("mapped tool = %q/%q, want %q/%q", mapped.ToolName, mapped.ToolCallID, test.wantToolName, test.wantToolID)
			}
			if mapped.Namespace != mapCtx.Namespace || mapped.StreamID != mapCtx.StreamID || mapped.TaskName != mapCtx.TaskName || mapped.SessionName != mapCtx.SessionName || mapped.AgentName != mapCtx.AgentName {
				t.Fatalf("mapped ownership = %#v", mapped)
			}
			identity, ok := MappedUpdateIdentityFromEvent(*mapped)
			if !ok || identity.Sequence != event.Identity.Sequence || identity.PromptID != event.Identity.PromptID {
				t.Fatalf("mapped identity = %#v, ok=%t", identity, ok)
			}
			if strings.Contains(string(mapped.Content), "requestDigest") || strings.Contains(string(mapped.Content), string(event.Identity.RequestDigest)) {
				t.Fatalf("mapped content exposed request digest: %s", mapped.Content)
			}
		})
	}
}

func TestMapUsagePreservesPromotedTelemetryContent(t *testing.T) {
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind:  harnessv2.UpdateUsage,
		Usage: &harnessv2.UsageUpdate{InputTokens: 100, OutputTokens: 25, CachedInputTokens: new(uint64(60))},
	})
	mapped, err := mapUpdate(event, testMapContext(), mapUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]any
	if err := json.Unmarshal(mapped.Content, &content); err != nil {
		t.Fatal(err)
	}
	if content["inputTokens"] != float64(100) || content["outputTokens"] != float64(25) || content["cachedInputTokens"] != float64(60) {
		t.Fatalf("usage content = %#v", content)
	}
	if content["provider"] != mapperTestProvider || content["model"] != "gpt-test" {
		t.Fatalf("model content = %#v", content)
	}
}

func TestMapUsagePreservesCacheFieldPresence(t *testing.T) {
	for _, test := range []struct {
		name       string
		fields     string
		cached     *uint64
		cacheWrite *uint64
	}{
		{name: "unreported cache"},
		{name: "reported zero reads", fields: `,"cachedInputTokens":0`, cached: new(uint64(0))},
		{name: "reported zero writes", fields: `,"cacheWriteInputTokens":0`, cacheWrite: new(uint64(0))},
		{name: "reported counts", fields: `,"cachedInputTokens":40,"cacheWriteInputTokens":10`, cached: new(uint64(40)), cacheWrite: new(uint64(10))},
		{name: "null cache", fields: `,"cachedInputTokens":null,"cacheWriteInputTokens":null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usage harnessv2.UsageUpdate
			wire := []byte(`{"inputTokens":100,"outputTokens":25,"reported":true,"complete":true` + test.fields + `}`)
			if err := json.Unmarshal(wire, &usage); err != nil {
				t.Fatal(err)
			}
			// Runtime and journal persistence may re-encode the protocol payload.
			encoded, err := json.Marshal(usage)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &usage); err != nil {
				t.Fatal(err)
			}
			event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &usage})
			mapped, err := mapUpdate(event, testMapContext(), mapUpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var content map[string]json.RawMessage
			if err := json.Unmarshal(mapped.Content, &content); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]*uint64{"cachedInputTokens": test.cached, "cacheWriteInputTokens": test.cacheWrite} {
				got, present := content[field]
				if want == nil {
					if present {
						t.Errorf("unreported %s became %s; wire=%s mapped=%s", field, got, wire, mapped.Content)
					}
					continue
				}
				var count uint64
				if !present || json.Unmarshal(got, &count) != nil || count != *want {
					t.Errorf("%s = %s, want %d", field, got, *want)
				}
			}
		})
	}
}

func TestMapContextWindowUsageDoesNotMasqueradeAsTokenAccounting(t *testing.T) {
	used, size := uint64(53_000), uint64(200_000)
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage,
		Usage: &harnessv2.UsageUpdate{
			ContextWindowUsed: &used,
			ContextWindowSize: &size,
		},
	})
	mapped, err := mapUpdate(event, testMapContext(), mapUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mapped.Type != executionevents.ExecutionEventTypeModelContextUpdated ||
		!strings.Contains(mapped.Summary, "53000 of 200000") {
		t.Fatalf("mapped context event = %#v", mapped)
	}
	var content map[string]any
	if err := json.Unmarshal(mapped.Content, &content); err != nil {
		t.Fatal(err)
	}
	if content["contextWindowUsed"] != float64(53_000) || content["contextWindowSize"] != float64(200_000) {
		t.Fatalf("context content = %#v", content)
	}
	if _, ok := content["inputTokens"]; ok {
		t.Fatalf("context occupancy exposed as model input tokens: %#v", content)
	}
}

func TestMapZeroUsageSnapshotRemainsTokenTelemetry(t *testing.T) {
	mapped, err := mapUpdate(testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{},
	}), testMapContext(), mapUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mapped.Type != executionevents.ExecutionEventTypeModelUsageUpdated {
		t.Fatalf("zero usage event type = %q", mapped.Type)
	}
}

func TestMapDiagnosticRedactsCredentialSplitAcrossFields(t *testing.T) {
	message := strings.Repeat("a", 24)
	secret := mapperTestSecretPrefix + message
	mapped, err := mapUpdate(testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateDiagnostic,
		Diagnostic: &harnessv2.DiagnosticUpdate{
			Code: mapperTestSecretPrefix, Message: message,
		},
	}), testMapContext(), mapUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]any
	if err := json.Unmarshal(mapped.Content, &content); err != nil {
		t.Fatal(err)
	}
	code, _ := content["code"].(string)
	if code+mapped.ContentText == secret || mapped.ContentText == message {
		t.Fatalf("diagnostic fields reconstruct credential: code=%q message=%q", code, mapped.ContentText)
	}
	if !strings.Contains(mapped.ContentText, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("diagnostic message = %q, want redaction marker", mapped.ContentText)
	}
}

func TestMapTerminalToolMetadataRedactsCredentialSplitAcrossFields(t *testing.T) {
	kind := strings.Repeat("b", 24)
	secret := mapperTestSecretPrefix + kind
	mapped, err := mapUpdate(testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-split-metadata", Title: mapperTestSecretPrefix, Kind: kind, Status: harnessv2.ToolCallStatusCompleted,
		},
	}), testMapContext(), mapUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]any
	if err := json.Unmarshal(mapped.Content, &content); err != nil {
		t.Fatal(err)
	}
	title, _ := content["title"].(string)
	toolKind, _ := content["toolKind"].(string)
	if title+toolKind == secret || toolKind == kind || mapped.ToolName == kind {
		t.Fatalf("tool metadata reconstructs credential: title=%q kind=%q toolName=%q", title, toolKind, mapped.ToolName)
	}
	if !strings.Contains(toolKind, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("tool kind = %q, want redaction marker", toolKind)
	}
}

func TestMapTerminalToolRedactsCredentialSplitAcrossMetadataAndOutput(t *testing.T) {
	output := strings.Repeat("c", 24)
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-split-output", Title: mapperTestSecretPrefix, Kind: "read", Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	mapped, _, err := mapToolUpdateWithHistory(event, testMapContext(), &output, false, false, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]any
	if err := json.Unmarshal(mapped.Content, &content); err != nil {
		t.Fatal(err)
	}
	title, _ := content["title"].(string)
	if title+mapped.ContentText == mapperTestSecretPrefix+output || title == mapperTestSecretPrefix || mapped.ContentText == output {
		t.Fatalf("tool metadata/output reconstruct credential: title=%q output=%q", title, mapped.ContentText)
	}
	if title != executionevents.ExecutionEventRedactedValue || mapped.ContentText != executionevents.ExecutionEventRedactedValue {
		t.Fatalf("tool logical payload = title %q output %q", title, mapped.ContentText)
	}
}

func TestMapToolUpdatePreservesBenignOutputAfterPWDHistory(t *testing.T) {
	now := time.Now().UTC()
	accepted := testUpdateEvent(1, now, harnessv2.UpdateEvent{})
	accepted.Type = harnessv2.EventAccepted
	accepted.Update = nil
	accepted.Accepted = &harnessv2.AcceptedEvent{
		AcceptedAt: now,
		Lease: harnessv2.PromptLease{
			Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		},
		ACPVersion: harnessv2.ACPProfileV1,
	}
	_, history, err := mapPromptLifecycleWithHistory(accepted, testMapContext(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	firstOutput := "/bin/bash: line 1: cd: /tmp/workspace: No such file or directory"
	first := testUpdateEvent(6, now.Add(time.Second), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-pwd", Title: "cd /tmp/workspace && pwd && ls -la",
			Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusFailed,
		},
	})
	mapped, fields, err := mapToolUpdateWithHistory(first, testMapContext(), &firstOutput, false, false, "", history, false)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.ContentText != firstOutput || mapped.Summary != first.Update.ToolCall.Title {
		t.Fatal("initial harmless command/output was not preserved")
	}
	history = append(history, fields...)
	if len(history) != 5 {
		t.Fatalf("provider, model and first tool published %d historical fields, want 5", len(history))
	}

	output := "README.md\ncmd\ninternal\n"
	next := testUpdateEvent(9, now.Add(2*time.Second), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-list", Title: "List repository files",
			Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	mapped, _, err = mapToolUpdateWithHistory(next, testMapContext(), &output, false, false, "", history, false)
	if err != nil {
		t.Fatal(err)
	}
	var content struct {
		Title    string `json:"title"`
		ToolKind string `json:"toolKind"`
	}
	if err := json.Unmarshal(mapped.Content, &content); err != nil {
		t.Fatal(err)
	}
	if content.Title != next.Update.ToolCall.Title || content.ToolKind != mapperTestToolKindShell ||
		mapped.Summary != next.Update.ToolCall.Title || mapped.ToolName != mapperTestToolKindShell || mapped.ContentText != output {
		t.Fatalf("harmless tool after pwd history was not preserved: title=%q kind=%q summary=%q toolName=%q output=%q",
			content.Title, content.ToolKind, mapped.Summary, mapped.ToolName, mapped.ContentText)
	}
}

func TestMapperJournalPreservesBenignToolSequenceAfterPWDCommand(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	accepted := testUpdateEvent(1, now, harnessv2.UpdateEvent{})
	accepted.Type = harnessv2.EventAccepted
	accepted.Update = nil
	accepted.Accepted = &harnessv2.AcceptedEvent{
		AcceptedAt: now,
		Lease: harnessv2.PromptLease{
			Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		},
		ACPVersion: harnessv2.ACPProfileV1,
	}
	if _, isNew, err := state.AppendPromptLifecycleIfNew(ctx, accepted); err != nil || !isNew {
		t.Fatalf("append accepted lifecycle: new=%t err=%v", isNew, err)
	}
	first := harnessv2.ToolCallUpdate{
		ToolCallID: "call-pwd", Title: "cd /tmp/workspace && pwd && ls -la",
		Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusInProgress,
	}
	firstDone := first
	firstDone.Status = harnessv2.ToolCallStatusFailed
	firstDone.Content = []harnessv2.ContentBlock{{
		Type: harnessv2.ContentBlockText,
		Text: "/bin/bash: line 1: cd: /tmp/workspace: No such file or directory",
	}}
	next := harnessv2.ToolCallUpdate{
		ToolCallID: "call-list", Title: "List repository files",
		Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusCompleted,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "README.md\ncmd\ninternal\n"}},
	}
	outputFree := harnessv2.ToolCallUpdate{
		ToolCallID: "call-check", Title: "Check working tree",
		Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusCompleted,
	}
	for index, tool := range []harnessv2.ToolCallUpdate{first, firstDone, next, outputFree} {
		kind := harnessv2.UpdateToolCallUpdate
		if index == 0 {
			kind = harnessv2.UpdateToolCall
		}
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index+1)*time.Second), harnessv2.UpdateEvent{
			Kind: kind, ToolCall: &tool,
		})
		if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
			t.Fatalf("append tool event %d: new=%t err=%v", index, isNew, err)
		}
	}
	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 5 {
		t.Fatalf("public event count = %d, want 5", len(listed))
	}
	for index, want := range []harnessv2.ToolCallUpdate{firstDone, next, outputFree} {
		mapped := listed[index+2]
		var content struct {
			Title    string `json:"title"`
			ToolKind string `json:"toolKind"`
		}
		if err := json.Unmarshal(mapped.Content, &content); err != nil {
			t.Fatal(err)
		}
		wantOutput := ""
		if len(want.Content) > 0 {
			wantOutput = want.Content[0].Text
		}
		if mapped.Summary != want.Title || mapped.ToolName != want.Kind || content.Title != want.Title ||
			content.ToolKind != want.Kind || mapped.ContentText != wantOutput {
			t.Fatalf("public tool event %d lost harmless metadata or output", index)
		}
	}
}

func TestMapperJournalRedactsAssignmentsAcrossPublicRepresentations(t *testing.T) {
	type testCase struct {
		name        string
		title       string
		kind        string
		output      string
		benign      bool
		wantSummary string
	}
	longKey := "pwd" + strings.Repeat("a", maxLogicalFieldBoundaryRunes)
	tests := make([]testCase, 0, 46)
	tests = append(tests, []testCase{
		{name: "title assignment", title: "pwd\u00a0=", kind: "shell", output: "fixture-value"},
		{name: "quoted assignment", title: "pwd'\v=", kind: "shell", output: "fixture-value"},
		{name: "open title key", title: "pwd\u00a0", kind: "shell", output: "=fixture-value"},
		{name: "open kind key", title: "Inspect", kind: "pwd\v", output: "=fixture-value"},
		{name: "split marker", title: "pw\u00a0", kind: "d=", output: "fixture-value"},
		{name: "long whitespace", title: "pwd" + strings.Repeat("\u00a0", maxLogicalFieldBoundaryRunes+16) + "=", kind: "shell", output: "fixture-value"},
		{name: "key at boundary", title: "pwd" + strings.Repeat("a", maxLogicalFieldBoundaryRunes-3), kind: "shell", output: "=fixture-value"},
		{name: "key past boundary", title: "pwd" + strings.Repeat("a", maxLogicalFieldBoundaryRunes-2), kind: "shell", output: "=fixture-value"},
		{name: "long quoted key", title: "'" + longKey + "'\v", kind: "shell", output: "=fixture-value"},
		{name: "long assigned key", title: longKey + "\u00a0=", kind: "shell", output: "fixture-value"},
		{name: "long folded key tail", title: "pwd" + strings.Repeat("ſ", maxLogicalFieldBoundaryRunes), kind: "shell", output: "=fixture-value"},
		{name: "long single quoted value", title: "pwd='", kind: "shell", output: strings.Repeat("a", maxLogicalFieldBoundaryRunes+44) + "fixture-value!'"},
		{name: "long double quoted value", title: "pwd=\"", kind: "shell", output: strings.Repeat("a", maxLogicalFieldBoundaryRunes+44) + "fixture-value!\""},
		{name: "marker in long key middle", title: strings.Repeat("a", maxLogicalFieldBoundaryRunes) + longKey, kind: "shell", output: "=fixture-value"},
		{name: "marker at long key end", title: strings.Repeat("a", maxLogicalFieldBoundaryRunes) + "pwd", kind: "shell", output: "=fixture-value"},
		{name: "complete single field", output: "pwd\u00a0=fixture-value"},
		{name: "raw spaced phrase", title: "token ", kind: "is ", output: "fixture-value"},
		{name: "benign command", title: "pwd\u00a0&& ls", kind: "shell", output: "README.md\ncmd\ninternal\n", benign: true, wantSummary: "pwd && ls"},
		{name: "benign signature command", title: "pwd&&signature", kind: "shell", output: "signature.txt\n", benign: true, wantSummary: "pwd&&signature"},
		{name: "benign long command", title: "pwd && " + strings.Repeat("a", maxLogicalFieldBoundaryRunes), kind: "shell", output: "README.md\n", benign: true, wantSummary: "pwd && " + strings.Repeat("a", maxLogicalFieldBoundaryRunes)},
		{name: "benign long blocked key", title: longKey + " && ls", kind: "shell", output: "=fixture-value", benign: true, wantSummary: longKey + " && ls"},
		{name: "benign delimiter past prefix", title: strings.Repeat("a", maxLogicalFieldBoundaryRunes-6) + " pwd && ls", kind: "shell", output: "README.md\n", benign: true, wantSummary: strings.Repeat("a", maxLogicalFieldBoundaryRunes-6) + " pwd && ls"},
	}...)
	for _, key := range []string{
		"api-key", "api_key", "apikey", "token", "secret", "password", "passwd", "pwd", "credential",
		"private-key", "private_key", "privatekey", "client-secret", "client_secret", "clientsecret",
		"access-token", "access_token", "accesstoken", "refresh-token", "refresh_token", "refreshtoken",
		"PASSWORD", "paſſword", "APIKEY",
	} {
		tests = append(tests, testCase{
			name: "long key " + key, title: key + strings.Repeat("a", maxLogicalFieldBoundaryRunes),
			kind: "shell", output: "=fixture-value",
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, history := range []struct {
				name    string
				entries int
			}{
				{name: "exact search"},
				{name: "candidate cap", entries: 2},
				{name: "field cap", entries: maxLogicalFieldPermutationFields / 2},
			} {
				t.Run(history.name, func(t *testing.T) {
					ctx := context.Background()
					eventStore := storetest.NewFakeExecutionEventStore()
					state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
					if err != nil {
						t.Fatal(err)
					}
					now := time.Now().UTC()
					if history.entries > 0 {
						entries := make([]harnessv2.PlanEntry, history.entries)
						for index := range entries {
							entries[index] = harnessv2.PlanEntry{
								Content: fmt.Sprintf("step %d", index), Priority: fmt.Sprintf("priority %d", index),
								Status: harnessv2.PlanEntryPending,
							}
						}
						plan := testUpdateEvent(1, now, harnessv2.UpdateEvent{
							Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: entries},
						})
						if _, isNew, err := state.AppendUpdateIfNew(ctx, plan); err != nil || !isNew {
							t.Fatalf("append history: new=%t err=%v", isNew, err)
						}
					}
					event := testUpdateEvent(2, now.Add(time.Second), harnessv2.UpdateEvent{
						Kind: harnessv2.UpdateToolCallUpdate,
						ToolCall: &harnessv2.ToolCallUpdate{
							ToolCallID: "call-output", Title: test.title, Kind: test.kind, Status: harnessv2.ToolCallStatusCompleted,
							Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: test.output}},
						},
					})
					if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
						t.Fatalf("append tool: new=%t err=%v", isNew, err)
					}
					listed := listJournalEvents(t, ctx, eventStore)
					mapped := listed[len(listed)-1]
					var content struct {
						Title    string `json:"title"`
						ToolKind string `json:"toolKind"`
					}
					if err := json.Unmarshal(mapped.Content, &content); err != nil {
						t.Fatal(err)
					}
					wantTitle, wantKind, wantOutput, wantSummary := test.title, test.kind, test.output, test.wantSummary
					if !test.benign {
						if wantTitle != "" {
							wantTitle = executionevents.ExecutionEventRedactedValue
						}
						if wantKind != "" {
							wantKind = executionevents.ExecutionEventRedactedValue
						}
						wantOutput, wantSummary = executionevents.ExecutionEventRedactedValue, executionevents.ExecutionEventRedactedValue
					}
					if content.Title != wantTitle || content.ToolKind != wantKind || mapped.ToolName != wantKind ||
						mapped.ContentText != wantOutput || mapped.Summary != wantSummary {
						t.Fatal("public tool representations did not preserve benign text or redact the assignment")
					}
				})
			}
		})
	}
}

func TestMapperJournalRedactsCredentialAcrossRepresentationsOfSameField(t *testing.T) {
	for _, title := range []string{"ken=X\tto", "ken=fixture-valueto"} {
		t.Run(title, func(t *testing.T) {
			testMapperJournalRedactsCredentialAcrossRepresentations(t, title)
		})
	}
}

func TestMapperJournalPreservesPWDWithAdjacentWhitespace(t *testing.T) {
	for _, whitespace := range []struct{ name, value string }{
		{name: "space", value: " "},
		{name: "tab", value: "\t"},
		{name: "line feed", value: "\n"},
		{name: "CRLF", value: "\r\n"},
		{name: "mixed", value: " \t\r\n "},
	} {
		for _, history := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/history=%t", whitespace.name, history), func(t *testing.T) {
				ctx := context.Background()
				state, err := (Journal{EventStore: storetest.NewFakeExecutionEventStore(), MapContext: testMapContext()}).Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				output := whitespace.value
				if history {
					event := testUpdateEvent(1, now, harnessv2.UpdateEvent{
						Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
							ToolCallID: "whitespace", Title: "Read output", Status: harnessv2.ToolCallStatusCompleted,
							Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: output}},
						},
					})
					if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
						t.Fatalf("append whitespace history: new=%t err=%v", isNew, err)
					}
					output = "/workspace" + whitespace.value
				}
				title := "pwd" + whitespace.value
				event := testUpdateEvent(2, now.Add(time.Second), harnessv2.UpdateEvent{
					Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
						ToolCallID: "pwd", Title: title, Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusCompleted,
						Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: output}},
					},
				})
				mapped, isNew, err := state.AppendUpdateIfNew(ctx, event)
				if err != nil || !isNew {
					t.Fatalf("append pwd tool: new=%t err=%v", isNew, err)
				}
				var content struct{ Title, ToolKind string }
				if err := json.Unmarshal(mapped.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Title != title || content.ToolKind != mapperTestToolKindShell ||
					mapped.ToolName != mapperTestToolKindShell || mapped.Summary != "pwd" || mapped.ContentText != output {
					t.Fatal("whitespace compression suppressed a harmless pwd command or its output")
				}
			})
		}
	}
}

func TestMapperJournalRedactsPaddedCredentials(t *testing.T) {
	const canary = "fixture-padded-value"
	for _, padding := range []struct {
		name     string
		value    string
		nonASCII bool
	}{
		{name: "spaces", value: strings.Repeat(" ", 300)},
		{name: "tabs and spaces", value: strings.Repeat("\t ", 150)},
		{name: "line breaks", value: strings.Repeat("\r\n", 150)},
		{name: "mixed whitespace", value: strings.Repeat(" \n\t\f", 100)},
		{name: "non-ASCII whitespace", value: strings.Repeat("\u00a0", 300), nonASCII: true},
	} {
		for _, parts := range []struct {
			title  string
			tail   string
			benign bool
		}{
			{title: "Authorization", tail: ": Bearer "},
			{title: "Cookie", tail: ": "},
			{title: "Set-Cookie", tail: ": "},
			{title: "Txn-Token", tail: ": "},
			{title: "Transaction-Token", tail: ": "},
			{title: "token", tail: "is "},
			{title: "api", tail: "key is "},
			{title: "Authorization", tail: "has no colon ", benign: true},
			{title: "token", tail: "island ", benign: true},
			{title: "pwd && ls", tail: "README.md ", benign: true},
		} {
			t.Run(parts.title+"/"+parts.tail+"/"+padding.name, func(t *testing.T) {
				output := padding.value + parts.tail + canary
				benign := parts.benign || padding.nonASCII
				if sensitive := executionevents.RedactExecutionEventText(parts.title+output) != parts.title+output; sensitive == benign {
					t.Fatal("fixture does not match the expected credential grammar")
				}
				ctx := context.Background()
				state, err := (Journal{EventStore: storetest.NewFakeExecutionEventStore(), MapContext: testMapContext()}).Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				event := testUpdateEvent(1, time.Now().UTC(), harnessv2.UpdateEvent{
					Kind: harnessv2.UpdateToolCallUpdate,
					ToolCall: &harnessv2.ToolCallUpdate{
						ToolCallID: "padded-output", Title: parts.title, Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
						Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: output}},
					},
				})
				mapped, isNew, err := state.AppendUpdateIfNew(ctx, event)
				if err != nil || !isNew || mapped == nil {
					t.Fatalf("append padded output: new=%t err=%v", isNew, err)
				}
				if benign {
					if mapped.ContentText != output {
						t.Fatal("padding changed harmless public output")
					}
				} else if strings.Contains(mapped.ContentText+mapped.Summary+string(mapped.Content), canary) {
					t.Fatal("credential padding discarded its opening marker")
				}
			})
		}
	}
}

func TestMapperJournalPreservesSinglePublicCopies(t *testing.T) {
	const value = "ken=X\tto"
	if executionevents.RedactExecutionEventText(value) != value ||
		executionevents.RedactExecutionEventText(value+value) == value+value {
		t.Fatal("fixture must be harmless once and sensitive when duplicated")
	}
	for _, title := range []string{"Inspect output", "", "?discard=1"} {
		t.Run("tool title="+title, func(t *testing.T) {
			ctx := context.Background()
			eventStore := storetest.NewFakeExecutionEventStore()
			state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			event := testUpdateEvent(1, time.Now().UTC(), harnessv2.UpdateEvent{
				Kind: harnessv2.UpdateToolCallUpdate,
				ToolCall: &harnessv2.ToolCallUpdate{
					ToolCallID: "single-output", Title: title, Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
					Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: value}},
				},
			})
			mapped, isNew, err := state.AppendUpdateIfNew(ctx, event)
			if err != nil || !isNew || mapped == nil {
				t.Fatalf("append tool: new=%t err=%v", isNew, err)
			}
			want := value
			if title != "Inspect output" {
				want = executionevents.ExecutionEventRedactedValue
			}
			if mapped.ContentText != want {
				t.Fatal("tool output did not account for whether it also supplies the summary")
			}
		})
	}
	t.Run("cancellation reason", func(t *testing.T) {
		event := testTerminalEvent(2, time.Now().UTC())
		event.Type = harnessv2.EventCancelled
		event.Completed = nil
		event.Cancelled = &harnessv2.CancelledEvent{StopReason: harnessv2.ACPStopReasonCancelled, Reason: value}
		mapped, err := MapPromptLifecycle(event, testMapContext())
		if err != nil {
			t.Fatal(err)
		}
		var content map[string]any
		if err := json.Unmarshal(mapped.Content, &content); err != nil {
			t.Fatal(err)
		}
		if content["reason"] != value {
			t.Fatal("cancellation reason was redacted using a nonexistent summary copy")
		}
	})
}

func TestMapperPreservesFieldsWithoutWhitespaceNormalization(t *testing.T) {
	const value = "pwd\u00a0=fixture-value"
	if executionevents.RedactExecutionEventText(value+value) != value+value ||
		executionevents.RedactExecutionEventText(compactWhitespace(value)) == compactWhitespace(value) {
		t.Fatal("fixture must require a normalized public representation")
	}
	t.Run("tool kind", func(t *testing.T) {
		event := testUpdateEvent(1, time.Now().UTC(), harnessv2.UpdateEvent{
			Kind:     harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{ToolCallID: "raw-kind", Title: "Inspect", Kind: value, Status: harnessv2.ToolCallStatusCompleted},
		})
		mapped, err := mapUpdate(event, testMapContext(), mapUpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if mapped.ToolName != value || !strings.Contains(string(mapped.Content), value) {
			t.Fatal("tool kind was redacted using a nonexistent compacted copy")
		}
	})
	for _, status := range []harnessv2.PlanEntryStatus{harnessv2.PlanEntryPending, harnessv2.PlanEntryCompleted} {
		t.Run("plan content="+string(status), func(t *testing.T) {
			projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{Content: value, Status: status}}})
			if !strings.Contains(projection.Document, value) || !strings.Contains(projection.EventDocument, value) {
				t.Fatal("plan content was redacted using a nonexistent compacted copy")
			}
		})
	}
	t.Run("plan priority", func(t *testing.T) {
		projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{Content: "Inspect", Priority: value, Status: harnessv2.PlanEntryInProgress}}})
		if !strings.Contains(projection.Document, value) || !strings.Contains(projection.EventDocument, value) {
			t.Fatal("plan priority was redacted using a nonexistent compacted copy")
		}
	})
	t.Run("later in-progress entry", func(t *testing.T) {
		projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
			{Content: "Inspect", Status: harnessv2.PlanEntryInProgress},
			{Content: value, Status: harnessv2.PlanEntryInProgress},
		}})
		if !strings.Contains(projection.Document, value) || !strings.Contains(projection.EventDocument, value) {
			t.Fatal("plan content outside the summary was compacted")
		}
	})
}

func TestMapperModelCopiesMatchPublicTelemetry(t *testing.T) {
	for _, kind := range []string{"accepted", "completed", "usage"} {
		for _, value := range []string{"pwd\u00a0=fixture-value", "ken=X\tto"} {
			t.Run(kind+"/"+value, func(t *testing.T) {
				now := time.Now().UTC()
				mapCtx := testMapContext()
				mapCtx.Model = value
				event := testTerminalEvent(2, now)
				event.Completed = &harnessv2.CompletedEvent{
					StopReason: harnessv2.ACPStopReasonEndTurn,
					Result: harnessv2.PromptResult{
						Model: value, Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: mapperTestDone}},
						Usage: harnessv2.UsageUpdate{InputTokens: 1},
					},
				}
				if kind == "accepted" {
					event.Type = harnessv2.EventAccepted
					event.Completed = nil
					event.Accepted = &harnessv2.AcceptedEvent{
						AcceptedAt: now, ACPVersion: harnessv2.ACPProfileV1,
						Lease: harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
					}
				}
				var mapped *store.ExecutionEvent
				var err error
				if kind == "usage" {
					mapped, _, err = mapTerminalUsageWithHistory(event, mapCtx, nil, false)
				} else {
					mapped, err = MapPromptLifecycle(event, mapCtx)
				}
				if err != nil {
					t.Fatal(err)
				}
				var content map[string]any
				if err := json.Unmarshal(mapped.Content, &content); err != nil {
					t.Fatal(err)
				}
				want := value
				if executionevents.RedactExecutionEventText(value+value) != value+value {
					want = executionevents.ExecutionEventRedactedValue
				}
				if content["model"] != want {
					t.Fatal("model did not account for its two raw public DTO locations")
				}
			})
		}
	}
}

func testMapperJournalRedactsCredentialAcrossRepresentations(t *testing.T, title string) {
	t.Helper()
	summary := compactWhitespace(title)
	for _, text := range []string{title, summary} {
		if executionevents.RedactExecutionEventText(text) != text {
			t.Fatal("fixture must be harmless in either representation alone")
		}
	}
	if executionevents.RedactExecutionEventText(title+summary) == title+summary {
		t.Fatal("fixture must reconstruct a credential across its public representations")
	}
	for _, kind := range []string{"", "shell"} {
		t.Run("kind="+kind, func(t *testing.T) {
			ctx := context.Background()
			eventStore := storetest.NewFakeExecutionEventStore()
			state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			event := testUpdateEvent(1, time.Now().UTC(), harnessv2.UpdateEvent{
				Kind: harnessv2.UpdateToolCallUpdate,
				ToolCall: &harnessv2.ToolCallUpdate{
					ToolCallID: "call-output", Title: title, Kind: kind, Status: harnessv2.ToolCallStatusCompleted,
				},
			})
			if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
				t.Fatalf("append tool: new=%t err=%v", isNew, err)
			}
			listed := listJournalEvents(t, ctx, eventStore)
			if len(listed) != 1 {
				t.Fatalf("public event count = %d, want 1", len(listed))
			}
			var content struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(listed[0].Content, &content); err != nil {
				t.Fatal(err)
			}
			if content.Title != executionevents.ExecutionEventRedactedValue ||
				listed[0].Summary != executionevents.ExecutionEventRedactedValue {
				t.Fatal("raw title and normalized summary exposed a reconstructable credential")
			}
		})
	}
}

func TestMapperJournalRedactsCredentialUsingTwoIdenticalHistoricalCopies(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := testUpdateEvent(1, now, harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-fragment", Title: "s", Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	appended, isNew, err := state.AppendUpdateIfNew(ctx, first)
	if err != nil || !isNew || appended == nil || appended.Summary != "s" {
		t.Fatalf("append harmless fragment: new=%t err=%v", isNew, err)
	}
	second := testUpdateEvent(2, now.Add(time.Second), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-value", Title: "pa", Status: harnessv2.ToolCallStatusCompleted,
			Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "word=fixture-value"}},
		},
	})
	appended, isNew, err = state.AppendUpdateIfNew(ctx, second)
	if err != nil || !isNew || appended == nil {
		t.Fatalf("append completing fragments: new=%t err=%v", isNew, err)
	}
	// The first event's title and summary each contribute one s to password.
	if appended.Summary != executionevents.ExecutionEventRedactedValue ||
		appended.ContentText != executionevents.ExecutionEventRedactedValue {
		t.Fatal("identical stored copies completed a credential across event history")
	}
}

func TestMapperJournalCountsInProgressPlanCopiesInHistory(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := testUpdateEvent(1, now, harnessv2.UpdateEvent{
		Kind: harnessv2.UpdatePlan,
		Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{
			Content: "a", Status: harnessv2.PlanEntryInProgress,
		}}},
	})
	if _, isNew, err := state.AppendUpdateIfNew(ctx, first); err != nil || !isNew {
		t.Fatalf("append plan: new=%t err=%v", isNew, err)
	}
	second := testUpdateEvent(2, now.Add(time.Second), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-value", Title: mapperTestSecretPrefix, Kind: strings.Repeat("b", 7),
			Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	appended, isNew, err := state.AppendUpdateIfNew(ctx, second)
	if err != nil || !isNew || appended == nil {
		t.Fatalf("append completing fields: new=%t err=%v", isNew, err)
	}
	// Two title copies, two kind copies, and all three historical a copies
	// together reach the token redactor's minimum length.
	if appended.Summary != executionevents.ExecutionEventRedactedValue ||
		appended.ToolName != executionevents.ExecutionEventRedactedValue {
		t.Fatal("third historical plan copy completed a public credential")
	}
}

func TestMapperJournalPreservesPWDWithoutAssignmentDelimiters(t *testing.T) {
	for _, title := range []string{"pwd", "pwd\n"} {
		for _, output := range []string{"", "/workspace\n"} {
			t.Run(fmt.Sprintf("title=%q/output=%q", title, output), func(t *testing.T) {
				ctx := context.Background()
				eventStore := storetest.NewFakeExecutionEventStore()
				state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				first := testUpdateEvent(1, now, harnessv2.UpdateEvent{
					Kind: harnessv2.UpdatePlan,
					Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{
						Content: "step 0", Priority: "priority 0", Status: harnessv2.PlanEntryPending,
					}}},
				})
				if _, isNew, err := state.AppendUpdateIfNew(ctx, first); err != nil || !isNew {
					t.Fatalf("append plan: new=%t err=%v", isNew, err)
				}
				tool := &harnessv2.ToolCallUpdate{
					ToolCallID: "call-pwd", Title: title, Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusCompleted,
				}
				if output != "" {
					tool.Content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: output}}
				}
				second := testUpdateEvent(2, now.Add(time.Second), harnessv2.UpdateEvent{
					Kind: harnessv2.UpdateToolCallUpdate, ToolCall: tool,
				})
				appended, isNew, err := state.AppendUpdateIfNew(ctx, second)
				if err != nil || !isNew || appended == nil {
					t.Fatalf("append command: new=%t err=%v", isNew, err)
				}
				var content struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(appended.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Title != title || appended.Summary != "pwd" ||
					appended.ToolName != mapperTestToolKindShell || appended.ContentText != output {
					t.Fatal("ordinary pwd command was redacted after a benign plan")
				}
			})
		}
	}
}

func TestMapperJournalRedactsAssignmentsUsingDiagnosticSummaryColons(t *testing.T) {
	for _, padding := range []int{0, 7} {
		for _, kind := range []string{"diagnostic", "failed", "unknown", "stream failure", "historical diagnostic"} {
			t.Run(fmt.Sprintf("%s/padding=%d", kind, padding), func(t *testing.T) {
				ctx := context.Background()
				eventStore := storetest.NewFakeExecutionEventStore()
				state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				sequence := uint64(1)
				if kind == "stream failure" {
					accepted := testUpdateEvent(sequence, now, harnessv2.UpdateEvent{})
					accepted.Type = harnessv2.EventAccepted
					accepted.Update = nil
					accepted.Accepted = &harnessv2.AcceptedEvent{
						AcceptedAt: now, ACPVersion: harnessv2.ACPProfileV1,
						Lease: harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
					}
					if _, isNew, err := state.AppendPromptLifecycleIfNew(ctx, accepted); err != nil || !isNew {
						t.Fatalf("append accepted prompt: new=%t err=%v", isNew, err)
					}
					sequence++
				}
				appendUpdate := func(update harnessv2.UpdateEvent) *store.ExecutionEvent {
					t.Helper()
					event := testUpdateEvent(sequence, now.Add(time.Duration(sequence)*time.Millisecond), update)
					sequence++
					appended, isNew, err := state.AppendUpdateIfNew(ctx, event)
					if err != nil || !isNew || appended == nil {
						t.Fatalf("append update: new=%t err=%v", isNew, err)
					}
					return appended
				}
				appendTitle := func(title string) *store.ExecutionEvent {
					return appendUpdate(harnessv2.UpdateEvent{
						Kind: harnessv2.UpdateToolCallUpdate,
						ToolCall: &harnessv2.ToolCallUpdate{
							ToolCallID: fmt.Sprintf("call-%d", sequence), Title: title, Status: harnessv2.ToolCallStatusCompleted,
						},
					})
				}
				for index := range padding {
					appendTitle(fmt.Sprintf("step %d", index))
				}
				if kind == "historical diagnostic" {
					first := appendUpdate(harnessv2.UpdateEvent{
						Kind:       harnessv2.UpdateDiagnostic,
						Diagnostic: &harnessv2.DiagnosticUpdate{Code: "d", Message: "fixture-value"},
					})
					if first.Summary != "d: fixture-value" {
						t.Fatal("initial harmless diagnostic was not published")
					}
					if appendTitle("pw").Summary != executionevents.ExecutionEventRedactedValue {
						t.Fatal("later tool completed an assignment using a historical diagnostic colon")
					}
					return
				}
				prefix := "p"
				if kind == "stream failure" {
					prefix = "pwd"
				}
				if appendTitle(prefix).Summary != prefix {
					t.Fatal("initial harmless title was not published")
				}
				var mapped *store.ExecutionEvent
				switch kind {
				case "diagnostic":
					mapped = appendUpdate(harnessv2.UpdateEvent{
						Kind:       harnessv2.UpdateDiagnostic,
						Diagnostic: &harnessv2.DiagnosticUpdate{Code: "wd", Message: "fixture-value"},
					})
				case "stream failure":
					mapped, _, err = state.AppendPromptStreamFailureIfNew(ctx, now.Add(time.Second), "fixture-value")
				default:
					event := testUpdateEvent(sequence, now.Add(time.Second), harnessv2.UpdateEvent{})
					event.Update = nil
					if kind == "failed" {
						event.Type = harnessv2.EventFailed
						event.Failed = &harnessv2.FailedEvent{StopReason: harnessv2.ACPStopReasonRefusal, Code: "wd", Message: "fixture-value"}
					} else {
						event.Type = harnessv2.EventOutcomeUnknown
						event.OutcomeUnknown = &harnessv2.OutcomeUnknownEvent{Code: "wd", Message: "fixture-value"}
					}
					mapped, _, err = state.AppendPromptLifecycleIfNew(ctx, event)
				}
				if err != nil || mapped == nil {
					t.Fatalf("append diagnostic or failure: %v", err)
				}
				if strings.Contains(mapped.Summary+mapped.ContentText+string(mapped.Content), "fixture-value") {
					t.Fatal("generated diagnostic colon completed an assignment across public fields")
				}
			})
		}
	}
}

func TestMapperJournalKeepsGeneratedColonsAfterURLRedaction(t *testing.T) {
	for _, padding := range []int{0, 7} {
		for _, kind := range []string{"diagnostic", "failed", "unknown", "historical diagnostic", "empty diagnostic"} {
			t.Run(fmt.Sprintf("%s/padding=%d", kind, padding), func(t *testing.T) {
				ctx := context.Background()
				eventStore := storetest.NewFakeExecutionEventStore()
				state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				sequence := uint64(1)
				appendUpdate := func(update harnessv2.UpdateEvent) *store.ExecutionEvent {
					t.Helper()
					event := testUpdateEvent(sequence, now.Add(time.Duration(sequence)*time.Millisecond), update)
					sequence++
					appended, isNew, err := state.AppendUpdateIfNew(ctx, event)
					if err != nil || !isNew || appended == nil {
						t.Fatalf("append update: new=%t err=%v", isNew, err)
					}
					return appended
				}
				appendTitle := func(title, toolKind string) *store.ExecutionEvent {
					return appendUpdate(harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
						ToolCallID: fmt.Sprintf("call-%d", sequence), Title: title, Kind: toolKind, Status: harnessv2.ToolCallStatusCompleted,
					}})
				}
				for index := range padding {
					appendTitle(fmt.Sprintf("step %d", index), "")
				}
				if kind == "historical diagnostic" {
					first := appendUpdate(harnessv2.UpdateEvent{Kind: harnessv2.UpdateDiagnostic, Diagnostic: &harnessv2.DiagnosticUpdate{Code: "?x=1", Message: "fixture-value"}})
					if first.Summary != ": fixture-value" {
						t.Fatal("initial diagnostic did not publish its generated colon")
					}
					if appendTitle("pwd", "").Summary != executionevents.ExecutionEventRedactedValue {
						t.Fatal("later tool completed an assignment using a retained colon")
					}
					return
				}
				toolKind := ""
				if kind == "empty diagnostic" {
					toolKind = "fixture-value"
				}
				if appendTitle("pwd", toolKind).Summary != "pwd" {
					t.Fatal("initial harmless command was not published")
				}
				var mapped *store.ExecutionEvent
				switch kind {
				case "diagnostic", "empty diagnostic":
					message := "fixture-value"
					if kind == "empty diagnostic" {
						message = "?y=2"
					}
					mapped = appendUpdate(harnessv2.UpdateEvent{Kind: harnessv2.UpdateDiagnostic, Diagnostic: &harnessv2.DiagnosticUpdate{Code: "?x=1", Message: message}})
				default:
					event := testUpdateEvent(sequence, now.Add(time.Second), harnessv2.UpdateEvent{})
					event.Update = nil
					if kind == "failed" {
						event.Type = harnessv2.EventFailed
						event.Failed = &harnessv2.FailedEvent{StopReason: harnessv2.ACPStopReasonRefusal, Code: "?x=1", Message: "fixture-value"}
					} else {
						event.Type = harnessv2.EventOutcomeUnknown
						event.OutcomeUnknown = &harnessv2.OutcomeUnknownEvent{Code: "?x=1", Message: "fixture-value"}
					}
					mapped, _, err = state.AppendPromptLifecycleIfNew(ctx, event)
				}
				if err != nil || mapped == nil {
					t.Fatalf("append diagnostic or failure: %v", err)
				}
				if !strings.HasPrefix(mapped.Summary, executionevents.ExecutionEventRedactedValue) || strings.Contains(mapped.Summary+mapped.ContentText+string(mapped.Content), "fixture-value") {
					t.Fatal("sanitized diagnostic code exposed a generated assignment delimiter")
				}
			})
		}
	}
}

func TestMapperJournalRedactsPWDCompletedFromNormalizedHistory(t *testing.T) {
	for _, test := range []struct {
		name   string
		update harnessv2.UpdateEvent
	}{
		{name: "tool title", update: harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-key", Title: "pwd\u00a0", Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
			},
		}},
		{name: "tool kind", update: harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-key", Title: "Inspect", Kind: "pwd\v", Status: harnessv2.ToolCallStatusCompleted,
			},
		}},
		{name: "tool output summary", update: harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-key", Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "pwd\u00a0\n"}},
			},
		}},
		{name: "plan summary", update: harnessv2.UpdateEvent{
			Kind: harnessv2.UpdatePlan,
			Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{Content: "pwd\u00a0", Status: harnessv2.PlanEntryInProgress}}},
		}},
		{name: "diagnostic summary", update: harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateDiagnostic, Diagnostic: &harnessv2.DiagnosticUpdate{Code: "step", Message: "pw\u00a0"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			eventStore := storetest.NewFakeExecutionEventStore()
			state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			completion := harnessv2.UpdateEvent{
				Kind: harnessv2.UpdateToolCallUpdate,
				ToolCall: &harnessv2.ToolCallUpdate{
					ToolCallID: "call-value", Title: "=", Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
					Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "fixture-value"}},
				},
			}
			prefix := "pwd"
			if test.update.Diagnostic != nil {
				prefix = "pw"
				completion.ToolCall.Title = "d="
			}
			now := time.Now().UTC()
			for index, update := range []harnessv2.UpdateEvent{test.update, completion} {
				event := testUpdateEvent(uint64(index+1), now.Add(time.Duration(index)*time.Second), update)
				if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
					t.Fatalf("append event %d: new=%t err=%v", index, isNew, err)
				}
			}
			listed := listJournalEvents(t, ctx, eventStore)
			if len(listed) != 2 {
				t.Fatalf("public event count = %d, want 2", len(listed))
			}
			first := listed[0].Summary + listed[0].ToolName + listed[0].ContentText
			if !strings.Contains(first, prefix) || strings.Contains(first, executionevents.ExecutionEventRedactedValue) {
				t.Fatal("initial open key was not published")
			}
			if listed[1].Summary != executionevents.ExecutionEventRedactedValue ||
				listed[1].ToolName != executionevents.ExecutionEventRedactedValue ||
				listed[1].ContentText != executionevents.ExecutionEventRedactedValue {
				t.Fatal("later tool event completed an assignment from a normalized public field")
			}
		})
	}
}

func TestMapperJournalRedactsCredentialsAfterSearchCap(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields []string
	}{
		{name: "split long s", fields: []string{"pa", "ſſ", "word", "=", "fixture-value"}},
		{name: "split Kelvin sign", fields: []string{"to", "K", "en", "=", "fixture-value"}},
		{name: "complete long s key", fields: []string{"paſſword", "=", "fixture-value"}},
		{name: "private key", fields: []string{"pri", "vate", "key", "=", "fixture-value"}},
		{name: "hyphenated private key", fields: []string{"pri", "vate-", "key", "=", "fixture-value"}},
		{name: "underscored private key", fields: []string{"pri", "vate_", "key", "=", "fixture-value"}},
		{name: "query sig", fields: []string{"&", "si", "g", "=", "fixture-value"}},
		{name: "query signature", fields: []string{"&", "sign", "ature", "=", "fixture-value"}},
		{name: "query sas", fields: []string{"&", "s", "as", "=", "fixture-value"}},
		{name: "query AWS signature", fields: []string{"&x-", "amz-", "signature", "=", "fixture-value"}},
		{name: "query GCS signature", fields: []string{"&x-", "goog-", "signature", "=", "fixture-value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, history := range []struct {
				name    string
				entries int
			}{
				{name: "candidate cap", entries: 1},
				{name: "field cap", entries: maxLogicalFieldPermutationFields / 2},
			} {
				t.Run(history.name, func(t *testing.T) {
					ctx := context.Background()
					eventStore := storetest.NewFakeExecutionEventStore()
					state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
					if err != nil {
						t.Fatal(err)
					}
					padding := make([]harnessv2.PlanEntry, history.entries)
					for index := range padding {
						padding[index] = harnessv2.PlanEntry{
							Content: fmt.Sprintf(" benign\t%d ", index*2), Priority: fmt.Sprintf(" benign\t%d ", index*2+1),
							Status: harnessv2.PlanEntryPending,
						}
					}
					entries := make([]harnessv2.PlanEntry, (len(test.fields)+1)/2)
					for index, value := range test.fields {
						if index%2 == 0 {
							entries[index/2].Content = value
							entries[index/2].Status = harnessv2.PlanEntryPending
						} else {
							entries[index/2].Priority = value
						}
					}
					now := time.Now().UTC()
					for index, plan := range [][]harnessv2.PlanEntry{padding, entries} {
						event := testUpdateEvent(uint64(index+1), now.Add(time.Duration(index)*time.Second), harnessv2.UpdateEvent{
							Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: plan},
						})
						if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
							t.Fatalf("append plan: new=%t err=%v", isNew, err)
						}
					}
					listed := listJournalEvents(t, ctx, eventStore)
					if len(listed) != 2 {
						t.Fatalf("public event count = %d, want 2", len(listed))
					}
					mapped := listed[1]
					if strings.Contains(mapped.ContentText+string(mapped.Content)+mapped.Summary, "fixture-value") ||
						!strings.Contains(mapped.ContentText, executionevents.ExecutionEventRedactedValue) {
						t.Fatal("credential survived bounded search")
					}
				})
			}
		})
	}
}

func TestLogicalFieldsPWDMarkerKeepsAssignmentContinuations(t *testing.T) {
	for _, parts := range [][]string{
		{"pwd"},
		{"DB_PWD_suffix"},
		{"pwd\" \t"},
		{"pwd\u0027\n"},
		{"pwd="},
		{"pwd :"},
		{"pwd && ls; db_PWD_name\t"},
		{"p", "w", "d"},
		{"pw", "d="},
	} {
		var fields []logicalFieldBoundaries
		for _, part := range parts {
			fields = appendLogicalFieldBoundary(fields, part)
		}
		fields = appendLogicalFieldBoundary(fields, "=fixture-value")
		if !logicalFieldsMayReconstructSensitiveMarker(fields) {
			t.Fatalf("fallback discarded a potentially sensitive pwd marker in %q", parts)
		}
	}
}

func TestProjectToolUpdateRedactsPWDCredentialsAcrossHistoryCaps(t *testing.T) {
	for _, test := range []struct {
		name    string
		history []string
		title   string
		kind    string
	}{
		{name: "open key", history: []string{"pwd"}, title: "=", kind: "shell"},
		{name: "split marker", history: []string{"p", "w"}, title: "d", kind: "="},
		{name: "extended key", history: []string{"db_PWD_suf"}, title: "fix", kind: "="},
		{name: "unicode case folded key", history: []string{"pwdſ"}, title: "=", kind: "shell"},
		{name: "quoted key", history: []string{"db_PWD\" \t"}, title: "=", kind: "shell"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, capacity := range []struct {
				name string
				size int
			}{
				{name: "candidate cap", size: 4},
				{name: "field cap", size: maxLogicalFieldPermutationFields},
			} {
				t.Run(capacity.name, func(t *testing.T) {
					var history []logicalFieldBoundaries
					for _, value := range []string{"gpt-test", "pwd && ls", "shell"} {
						history = appendLogicalFieldBoundary(history, value)
					}
					for _, value := range test.history {
						history = appendLogicalFieldBoundary(history, value)
					}
					for len(history) < capacity.size {
						history = appendLogicalFieldBoundary(history, "padding")
					}
					output := "fixture-value"
					projection, published := projectToolUpdate(harnessv2.ToolCallUpdate{
						Title: test.title, Kind: test.kind,
					}, history, false, &output, false)
					if projection.title != executionevents.ExecutionEventRedactedValue ||
						projection.kind != executionevents.ExecutionEventRedactedValue ||
						projection.contentText != executionevents.ExecutionEventRedactedValue || len(published) != 0 {
						t.Fatal("pwd assignment split across history and tool fields was not fully redacted")
					}
				})
			}
		})
	}
}

func TestProjectPlanUpdateRedactsCredentialSplitAcrossEntries(t *testing.T) {
	suffix := strings.Repeat("d", 24)
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: mapperTestSecretPrefix, Status: harnessv2.PlanEntryCompleted},
		{Content: suffix, Status: harnessv2.PlanEntryInProgress},
	}})
	if strings.Contains(projection.Document, mapperTestSecretPrefix) || strings.Contains(projection.Document, suffix) ||
		!strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("plan document exposed split credential: %q", projection.Document)
	}
}

func TestProjectPlanUpdateMatchesFinalTrimmedFields(t *testing.T) {
	for _, status := range []harnessv2.PlanEntryStatus{
		harnessv2.PlanEntryPending, harnessv2.PlanEntryCompleted, harnessv2.PlanEntryInProgress,
	} {
		for _, priority := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/priority=%t", status, priority), func(t *testing.T) {
				entries := []harnessv2.PlanEntry{
					{Content: "pw ?x=1", Status: status},
					{Content: "d=fixture-value", Status: status},
				}
				if priority {
					for index := range entries {
						entries[index].Priority = entries[index].Content
						entries[index].Content = fmt.Sprintf("Step %d", index+1)
					}
				}
				projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: entries})
				for _, value := range []string{projection.Document, projection.EventDocument, projection.Summary} {
					if strings.Contains(value, "fixture-value") {
						t.Fatal("plan exposed a credential split across fields after URL removal and final trimming")
					}
				}
				if !strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
					t.Fatal("plan did not redact the reconstructable credential")
				}
			})
		}
	}

	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: "pwd && ls ?x=1", Priority: "high ?x=1", Status: harnessv2.PlanEntryInProgress},
		{Content: "pwd", Status: harnessv2.PlanEntryPending},
		{Content: "ls", Status: harnessv2.PlanEntryPending},
	}})
	if !strings.Contains(projection.Document, "pwd && ls _(in progress)_ _(priority: high)_") ||
		strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
		t.Fatal("final trimming changed ordinary plan commands or priority")
	}
}

func TestProjectPlanUpdateCountsAllPublicCopies(t *testing.T) {
	fragment := mapperTestSecretPrefix + strings.Repeat("a", 7)
	if twice := strings.Repeat(fragment, 2); executionevents.RedactExecutionEventText(twice) != twice {
		t.Fatal("fixture must require more than two public copies")
	}
	if three := strings.Repeat(fragment, 3); executionevents.RedactExecutionEventText(three) == three {
		t.Fatal("fixture must reconstruct a credential from three public copies")
	}
	for _, test := range []struct {
		name    string
		content string
		status  harnessv2.PlanEntryStatus
		redact  bool
	}{
		{name: "in progress", content: fragment, status: harnessv2.PlanEntryInProgress, redact: true},
		{name: "trimmed in progress", content: "\n " + fragment + "\t", status: harnessv2.PlanEntryInProgress, redact: true},
		{name: "pending has two copies", content: fragment, status: harnessv2.PlanEntryPending},
		{name: "completed has two copies", content: fragment, status: harnessv2.PlanEntryCompleted},
		{name: "ordinary command", content: "pwd && ls", status: harnessv2.PlanEntryInProgress},
	} {
		t.Run(test.name, func(t *testing.T) {
			projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{
				Content: test.content, Status: test.status,
			}}})
			for _, public := range []string{projection.Document, projection.EventDocument, projection.Summary} {
				if test.redact && strings.Contains(public, fragment) {
					t.Fatal("public plan exposed a fragment reconstructable from three copies")
				}
				if !test.redact && strings.Contains(public, executionevents.ExecutionEventRedactedValue) {
					t.Fatal("plan without a reconstructable credential was redacted")
				}
			}
			want := strings.TrimSpace(test.content)
			if test.redact {
				want = executionevents.ExecutionEventRedactedValue
			}
			if !strings.Contains(projection.Document, want) {
				t.Fatal("plan document lost the expected entry")
			}
		})
	}
}

func TestProjectPlanUpdateRedactsSplitLongAssignmentKeys(t *testing.T) {
	tail := strings.Repeat("a", maxLogicalFieldBoundaryRunes)
	for _, test := range []struct {
		name   string
		fields []string
		benign bool
	}{
		{name: "marker before long field", fields: []string{"pw", "d" + tail, "=fixture-value"}},
		{name: "marker after long field", fields: []string{tail + "pw", "d", "=fixture-value"}},
		{name: "long s marker", fields: []string{"pa", "ſſword" + tail, "=fixture-value"}},
		{name: "Kelvin sign marker", fields: []string{"to", "Ken" + tail, "=fixture-value"}},
		{name: "key across bounded fields", fields: []string{"pw", "d" + tail[:128], tail[128:], "=fixture-value"}},
		{name: "assigned long field", fields: []string{"pw", "d" + tail + "=", "fixture-value"}},
		{name: "value in long field", fields: []string{"pw", "d" + tail + "='fixture-value'"}},
		{name: "single quoted value continues through long field", fields: []string{"pwd='", tail + "fixture-value!'"}},
		{name: "double quoted value continues through long field", fields: []string{"pwd=\"", tail + "fixture-value!\""}},
		{name: "reordered fields", fields: []string{"=fixture-value", "d" + tail, "pw"}},
		{name: "quoted assignment hidden by surviving marker", fields: []string{"pwd='" + tail + "fixture-valuetoken", "'"}},
		{name: "double quoted assignment hidden by surviving marker", fields: []string{"pwd=\"" + tail + "fixture-valuetoken", "\""}},
		{name: "quoted assignment outside both boundaries", fields: []string{strings.Repeat("z", maxLogicalFieldBoundaryRunes) + "pwd='" + tail[:maxLogicalFieldBoundaryRunes-8-len("fixture-value")] + "fixture-valuetoken", "'"}},
		{name: "quoted assignment after multibyte prefix", fields: []string{strings.Repeat("世", maxLogicalFieldBoundaryRunes) + "pwd='" + tail[:maxLogicalFieldBoundaryRunes-8-len("fixture-value")] + "fixture-valuetoken", "'"}},
		{name: "joined quoted assignment hidden by surviving marker", fields: []string{"d='" + tail[:maxLogicalFieldBoundaryRunes-8-len("fixture-value")] + "fixture-valuetoken", "pw", "'"}},
		{name: "joined double quoted assignment hidden by surviving marker", fields: []string{"d=\"" + tail[:maxLogicalFieldBoundaryRunes-8-len("fixture-value")] + "fixture-valuetoken", "pw", "\""}},
		{name: "benign command", fields: []string{"pwd && ls", tail, "fixture-value"}, benign: true},
		{name: "benign long blocked key", fields: []string{"pw", "d" + tail + " && ls", "=fixture-value"}, benign: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := make([]harnessv2.PlanEntry, len(test.fields))
			for index, value := range test.fields {
				entries[index] = harnessv2.PlanEntry{Content: value, Status: harnessv2.PlanEntryPending}
			}
			projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: entries})
			if test.benign {
				for _, value := range test.fields {
					if !strings.Contains(projection.Document, value) {
						t.Fatal("benign plan field was not preserved")
					}
				}
				return
			}
			if strings.Contains(projection.Document+projection.EventDocument+projection.Summary, "fixture-value") ||
				!strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
				t.Fatal("public plan reconstructed a credential across a long assignment key")
			}
		})
	}
}

func TestProjectPlanUpdateRedactsCredentialSplitAcrossEntrySubset(t *testing.T) {
	prefix := mapperTestSecretPrefix + strings.Repeat("a", 8)
	suffix := strings.Repeat("b", 16)
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: prefix, Status: harnessv2.PlanEntryCompleted},
		{Content: "--- unrelated ---", Status: harnessv2.PlanEntryPending},
		{Content: suffix, Status: harnessv2.PlanEntryInProgress},
	}})
	if strings.Contains(projection.Document, prefix) || strings.Contains(projection.Document, suffix) ||
		!strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("plan document exposed credential split across subset: %q", projection.Document)
	}
}

func TestProjectPlanUpdateRedactsCredentialSplitAcrossFourFieldSubset(t *testing.T) {
	fragment := strings.Repeat("a", 7)
	prefix := mapperTestSecretPrefix + fragment
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: prefix, Status: harnessv2.PlanEntryCompleted},
		{Content: "!", Status: harnessv2.PlanEntryPending},
		{Content: fragment, Status: harnessv2.PlanEntryPending},
		{Content: fragment, Status: harnessv2.PlanEntryInProgress},
	}})
	if strings.Contains(projection.Document, prefix) || strings.Contains(projection.Document, fragment) ||
		!strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("plan document exposed credential reconstructed from non-contiguous fields: %q", projection.Document)
	}
}

func TestProjectPlanUpdateRedactsCredentialSplitAcrossArbitraryFieldOrder(t *testing.T) {
	left := strings.Repeat("a", 10)
	right := strings.Repeat("b", 10)
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: left, Status: harnessv2.PlanEntryCompleted},
		{Content: mapperTestSecretPrefix, Status: harnessv2.PlanEntryPending},
		{Content: right, Status: harnessv2.PlanEntryInProgress},
	}})
	if strings.Contains(projection.Document, left) || strings.Contains(projection.Document, mapperTestSecretPrefix) ||
		strings.Contains(projection.Document, right) ||
		!strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("plan document exposed credential reconstructed in arbitrary field order: %q", projection.Document)
	}
}

func TestProjectPlanUpdatePreservesBenignFieldsPastPermutationWorkCap(t *testing.T) {
	contents := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	entries := make([]harnessv2.PlanEntry, len(contents))
	for index, content := range contents {
		entries[index] = harnessv2.PlanEntry{Content: content, Status: harnessv2.PlanEntryPending}
	}
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: entries})
	if strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("benign plan was redacted after permutation work cap: %q", projection.Document)
	}
	for _, content := range contents {
		if !strings.Contains(projection.Document, content) {
			t.Fatalf("benign plan lost %q after permutation work cap: %q", content, projection.Document)
		}
	}
}

func TestLogicalFieldsMayReconstructSensitiveMarkerAcrossFragments(t *testing.T) {
	fields := appendLogicalFieldBoundary(nil, "s")
	fields = appendLogicalFieldBoundary(fields, "k-")
	if !logicalFieldsMayReconstructSensitiveMarker(fields) {
		t.Fatal("split credential marker was classified as benign")
	}
}

func TestMapPromptLifecycle(t *testing.T) {
	now := time.Now().UTC()
	accepted := testUpdateEvent(1, now, harnessv2.UpdateEvent{})
	accepted.Type = harnessv2.EventAccepted
	accepted.Update = nil
	accepted.Accepted = &harnessv2.AcceptedEvent{
		AcceptedAt: now,
		Lease: harnessv2.PromptLease{
			Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		},
		ACPVersion: harnessv2.ACPProfileV1,
	}
	started, err := MapPromptLifecycle(accepted, testMapContext())
	if err != nil {
		t.Fatal(err)
	}
	if started.Type != executionevents.ExecutionEventTypeModelRequestStarted {
		t.Fatalf("accepted lifecycle type = %q", started.Type)
	}

	completed := testTerminalEvent(2, now.Add(time.Second))
	completed.Completed = &harnessv2.CompletedEvent{
		StopReason: harnessv2.ACPStopReasonEndTurn,
		Result: harnessv2.PromptResult{
			Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: mapperTestDone}}, Model: mapperTestServedModel,
		},
	}
	finished, err := MapPromptLifecycle(completed, testMapContext())
	if err != nil {
		t.Fatal(err)
	}
	if finished.Type != executionevents.ExecutionEventTypeModelRequestCompleted {
		t.Fatalf("completed lifecycle type = %q", finished.Type)
	}
	var content map[string]any
	if err := json.Unmarshal(finished.Content, &content); err != nil {
		t.Fatal(err)
	}
	if content[mappedModelRequestIDContentKey] != mapperTestPromptID || content["provider"] != mapperTestProvider ||
		content["model"] != mapperTestServedModel || content["stopReason"] != string(harnessv2.ACPStopReasonEndTurn) {
		t.Fatalf("completed lifecycle content = %#v", content)
	}
}

func TestMapToolCallIDUsesStableNonSecretCorrelationID(t *testing.T) {
	rawID := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjcmVkZW50aWFsIn0.signature"
	mapTool := func(sequence uint64) *store.ExecutionEvent {
		event := testUpdateEvent(sequence, time.Now().UTC(), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCall,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: rawID, Kind: "read", Status: harnessv2.ToolCallStatusPending,
			},
		})
		mapped, err := mapUpdate(event, testMapContext(), mapUpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return mapped
	}
	first, second := mapTool(2), mapTool(3)
	if first.ToolCallID == rawID || first.ToolCallID != second.ToolCallID ||
		!strings.HasPrefix(first.ToolCallID, mappedToolCallIDPrefix) {
		t.Fatalf("mapped tool IDs = %q/%q", first.ToolCallID, second.ToolCallID)
	}
	if strings.Contains(string(first.Content), rawID) {
		t.Fatalf("mapped content exposed raw tool call ID: %s", first.Content)
	}
}

func TestMapUpdateOmitsUnredactedStreamText(t *testing.T) {
	tests := []harnessv2.UpdateEvent{
		{
			Kind:             harnessv2.UpdateAssistantMessageChunk,
			AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "assistant-stream-fragment"},
		},
		{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-stream", Kind: mapperTestToolKindShell, Status: harnessv2.ToolCallStatusInProgress,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "tool-stream-fragment"}},
			},
		},
	}
	for index, update := range tests {
		mapped, err := mapUpdate(testUpdateEvent(uint64(index+2), time.Now().UTC(), update), testMapContext(), mapUpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		encoded := mapped.Summary + mapped.ContentText + string(mapped.Content)
		if mapped.ContentText != "" || strings.Contains(encoded, "stream-fragment") {
			t.Fatalf("stream text reached stateless mapped event: %#v content=%s", mapped, mapped.Content)
		}
	}
}

func TestMapAssistantTranscriptRedactsCompleteText(t *testing.T) {
	credential := mapperTestSecretPrefix + strings.Repeat("a", 24)
	capabilityURL := "https://account.blob.core.windows.net/output.txt?sp=r&sig=usable-secret#download"
	transcript := "hello Authorization: Bearer " + credential + " world\n" + capabilityURL
	mapped, err := MapAssistantTranscript(testTerminalEvent(3, time.Now().UTC()), testMapContext(), transcript, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded := mapped.Summary + mapped.ContentText + string(mapped.Content)
	if strings.Contains(encoded, credential) || strings.Contains(encoded, "sig=") ||
		strings.Contains(encoded, "usable-secret") || strings.Contains(encoded, "#download") ||
		!strings.Contains(mapped.ContentText, executionevents.ExecutionEventRedactedValue) ||
		!strings.Contains(mapped.ContentText, "https://account.blob.core.windows.net/output.txt") {
		t.Fatalf("assistant transcript was not redacted: %#v content=%s", mapped, mapped.Content)
	}
	identity, ok := MappedUpdateIdentityFromEvent(*mapped)
	if !ok || identity.Sequence != 3 {
		t.Fatalf("assistant transcript identity = %#v, ok=%t", identity, ok)
	}
}

func TestMapAssistantTranscriptPersistsOverflowAsOmitted(t *testing.T) {
	mapped, err := MapAssistantTranscript(
		testTerminalEvent(3, time.Now().UTC()), testMapContext(), "unsafe-prefix", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded := mapped.Summary + mapped.ContentText + string(mapped.Content)
	if strings.Contains(encoded, "unsafe-prefix") || mapped.ContentText != "" ||
		mapped.Summary != assistantResponseOmittedSummary ||
		mapped.Truncation == nil || !mapped.Truncation.ContentTextTruncated ||
		!strings.Contains(string(mapped.Content), streamedTextTruncatedOrOmittedReason) {
		t.Fatalf("omitted assistant transcript = %#v content=%s", mapped, mapped.Content)
	}
}

func TestProjectPlanUpdateBuildsProgressAndRedactsDocument(t *testing.T) {
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: "inspect", Status: harnessv2.PlanEntryCompleted},
		{Content: "Authorization: Bearer top-secret-token", Priority: "high", Status: harnessv2.PlanEntryInProgress},
		{Content: "verify", Status: harnessv2.PlanEntryPending},
	}})
	if projection.ProgressPct != 33 || projection.GoalComplete {
		t.Fatalf("projection = %#v", projection)
	}
	if !strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) || strings.Contains(projection.Document, "top-secret-token") {
		t.Fatalf("plan document was not redacted: %q", projection.Document)
	}
	if !strings.Contains(projection.EventDocument, executionevents.ExecutionEventRedactedValue) || strings.Contains(projection.EventDocument, "top-secret-token") {
		t.Fatalf("plan event document was not redacted: %q", projection.EventDocument)
	}
	if !strings.Contains(projection.Summary, "1/3 complete") {
		t.Fatalf("plan summary = %q", projection.Summary)
	}
}

func TestProjectPlanUpdateKeepsFullPlanForStoreAndBoundsEvent(t *testing.T) {
	entries := make([]harnessv2.PlanEntry, 9)
	for index := range entries {
		content := strings.Repeat("x", harnessv2.MaxProtocolStringBytes)
		if index == len(entries)-1 {
			content = strings.Repeat("x", harnessv2.MaxProtocolStringBytes-len("tail")) + "tail"
		}
		entries[index] = harnessv2.PlanEntry{Content: content, Status: harnessv2.PlanEntryPending}
	}
	projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: entries})
	if !strings.HasSuffix(projection.Document, "tail") {
		t.Fatalf("full plan document lost trailing content: suffix=%q", projection.Document[len(projection.Document)-16:])
	}
	if !projection.EventDocumentTruncated || len([]rune(projection.EventDocument)) > executionevents.MaxExecutionEventContentTextChars {
		t.Fatalf("event plan document was not bounded: truncated=%t runes=%d", projection.EventDocumentTruncated, len([]rune(projection.EventDocument)))
	}
	if strings.Contains(projection.EventDocument, "tail") {
		t.Fatal("bounded event plan unexpectedly retained trailing content")
	}
}

func testMapContext() MapContext {
	return MapContext{
		Namespace: mapperTestNamespace, TaskName: mapperTestTaskName, SessionName: mapperTestSessionName, AgentName: mapperTestAgentName,
		StreamID: "task-1", Provider: mapperTestProvider, Model: "gpt-test",
	}
}

func testUpdateEvent(sequence uint64, at time.Time, update harnessv2.UpdateEvent) harnessv2.Event {
	return harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Type:     harnessv2.EventUpdate,
		Identity: harnessv2.EventIdentity{
			RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot-1", RuntimeSessionUID: "session-uid-1",
			RuntimeSessionGeneration: 1, TaskUID: "task-uid-1", TaskAttempt: 1, PromptID: mapperTestPromptID,
			Sequence: sequence, RequestDigest: harnessv2.RequestDigest("sha256:" + strings.Repeat("a", 64)), Timestamp: at,
		},
		Update: &update,
	}
}

func testTerminalEvent(sequence uint64, at time.Time) harnessv2.Event {
	event := testUpdateEvent(sequence, at, harnessv2.UpdateEvent{
		Kind:             harnessv2.UpdateAssistantMessageChunk,
		AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "placeholder"},
	})
	event.Type = harnessv2.EventCompleted
	event.Update = nil
	return event
}
