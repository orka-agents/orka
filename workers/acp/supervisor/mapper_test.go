package supervisor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestMapACPUpdateDecodesAgentMessageContent(t *testing.T) {
	update, text, ok, err := mapACPUpdate(&acp.SessionNotification{Update: json.RawMessage(`{
		"sessionUpdate":"agent_message_chunk",
		"content":{"type":"text","text":"hello"}
	}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || text != "hello" || update == nil || update.Kind != harnessv2.UpdateAssistantMessageChunk ||
		update.AssistantMessage == nil || update.AssistantMessage.Text != "hello" {
		t.Fatalf("mapped update = %#v text=%q ok=%v", update, text, ok)
	}
}

func TestMapACPUpdatePreservesWhitespaceChunkWithoutEvent(t *testing.T) {
	update, text, ok, err := mapACPUpdate(&acp.SessionNotification{Update: json.RawMessage(`{
		"sessionUpdate":"agent_message_chunk",
		"content":{"type":"text","text":" \n"}
	}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ok || update != nil || text != " \n" {
		t.Fatalf("mapped whitespace update = %#v text=%q ok=%v", update, text, ok)
	}
}

func TestMapACPUpdateIgnoresToolCallContentArray(t *testing.T) {
	update, text, ok, err := mapACPUpdate(&acp.SessionNotification{Update: json.RawMessage(`{
		"sessionUpdate":"tool_call_update",
		"toolCallId":"call-1",
		"title":"Read LICENSE",
		"kind":"read",
		"status":"completed",
		"content":[{"type":"content","content":{"type":"text","text":"tool output"}}]
	}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || text != "" || update == nil || update.Kind != harnessv2.UpdateToolCallUpdate || update.ToolCall == nil {
		t.Fatalf("mapped update = %#v text=%q ok=%v", update, text, ok)
	}
	wantID, err := canonicalACPToolCallID("call-1")
	if err != nil {
		t.Fatal(err)
	}
	if update.ToolCall.ToolCallID != wantID || update.ToolCall.Status != harnessv2.ToolCallStatusCompleted {
		t.Fatalf("tool call = %#v", update.ToolCall)
	}
}

func TestMapACPUpdateCanonicalizesOversizedToolCallIDAcrossEvents(t *testing.T) {
	rawID := strings.Repeat("provider-tool-call-", 24)
	encoded, err := json.Marshal(map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": rawID,
		"title": "Read repository", "kind": "read", "status": "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, _, ok, err := mapACPUpdate(&acp.SessionNotification{Update: encoded})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": rawID,
		"title": "Read repository", "kind": "read", "status": "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, _, updateOK, err := mapACPUpdate(&acp.SessionNotification{Update: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !updateOK || created == nil || completed == nil || created.ToolCall == nil || completed.ToolCall == nil {
		t.Fatalf("mapped tool calls = created %#v completed %#v", created, completed)
	}
	canonicalID, err := canonicalACPToolCallID(rawID)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalID == rawID || !strings.HasPrefix(canonicalID, canonicalACPToolCallIDPrefix) || len(canonicalID) > 253 {
		t.Fatalf("canonical tool call ID = %q", canonicalID)
	}
	spaceVariant, err := canonicalACPToolCallID(" " + rawID)
	if err != nil {
		t.Fatal(err)
	}
	reservedLooking, err := canonicalACPToolCallID(canonicalID)
	if err != nil {
		t.Fatal(err)
	}
	if spaceVariant == canonicalID || reservedLooking == canonicalID {
		t.Fatalf("canonical tool call IDs collided: raw=%q space=%q reserved=%q", canonicalID, spaceVariant, reservedLooking)
	}
	if created.ToolCall.ToolCallID != canonicalID || completed.ToolCall.ToolCallID != canonicalID {
		t.Fatalf("tool call correlation changed: created=%q completed=%q want=%q", created.ToolCall.ToolCallID, completed.ToolCall.ToolCallID, canonicalID)
	}
	if err := created.Validate(); err != nil {
		t.Fatalf("created event validation: %v", err)
	}
	if err := completed.Validate(); err != nil {
		t.Fatalf("completed event validation: %v", err)
	}

	permissionToolCall, err := json.Marshal(map[string]any{
		"toolCallId": rawID, "name": "Read", "title": "Read repository",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	permission, err := mapPermission(&acp.PermissionRequestEvent{
		RequestID: "permission-1",
		Request: acp.RequestPermissionRequest{
			ToolCall: permissionToolCall,
			Options: []acp.PermissionOption{{
				OptionID: "allow-once", Name: "Allow", Kind: string(harnessv2.PermissionOptionAllowOnce),
			}},
		},
	}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if permission.ToolCallID != canonicalID {
		t.Fatalf("permission tool call ID = %q, want %q", permission.ToolCallID, canonicalID)
	}
	if err := permission.Validate(now); err != nil {
		t.Fatalf("permission event validation: %v", err)
	}
}
