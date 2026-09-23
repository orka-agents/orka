package approvals

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestDeriveApprovalLifecycle(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	content := func(v map[string]string) json.RawMessage { data, _ := json.Marshal(v); return data }
	all := Derive([]store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Summary: "create PR", Content: content(map[string]string{"approvalID": "a1", "action": "create_pr", "riskSummary": "opens PR"}), CreatedAt: now},
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Content: content(map[string]string{"approvalID": "a1", "reason": "ok"}), CreatedAt: now.Add(time.Second)},
	}, now)
	if len(all) != 1 || all[0].Status != StatusApproved || all[0].DecisionSeq != 2 || all[0].DecisionReason != "ok" {
		t.Fatalf("approval state = %#v", all)
	}
}

func TestDeriveLegacyRuntimeBindingKeepsIdentifiersPrivate(t *testing.T) {
	raw := map[string]any{
		"taskAttempt": 1, "promptID": "prompt-1", "runtimeSessionUID": "session-1", "runtimeSessionGeneration": 1,
		"operationID": "private-operation-marker", "runtimeInstanceID": "https://runtime.example/instance?slot=1",
		"supervisorBootID": "private-boot-marker", "controllerEpoch": 1,
	}
	data, err := json.Marshal(map[string]any{"approvalID": "approval-1", "binding": raw})
	if err != nil {
		t.Fatal(err)
	}
	derived := Derive([]store.ExecutionEvent{{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: data}}, time.Time{})
	if len(derived) != 1 || derived[0].Binding == nil {
		t.Fatal("legacy binding was lost")
	}
	binding := derived[0].Binding
	if binding.OperationIDDigest != store.CanonicalBytesDigest([]byte(raw["operationID"].(string))) ||
		binding.RuntimeInstanceIDDigest != store.CanonicalBytesDigest([]byte(raw["runtimeInstanceID"].(string))) ||
		binding.SupervisorBootIDDigest != store.CanonicalBytesDigest([]byte(raw["supervisorBootID"].(string))) ||
		binding.PromptID != "prompt-1" || binding.RuntimeSessionGeneration != 1 {
		t.Fatal("legacy binding was not preserved through its digests")
	}
	public, err := json.Marshal(derived)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"operationID", "runtimeInstanceID", "supervisorBootID"} {
		if strings.Contains(string(public), raw[key].(string)) || strings.Contains(string(public), `"`+key+`":`) {
			t.Fatalf("public approval exposed legacy %s", key)
		}
	}
	var roundtrip []Approval
	if err := json.Unmarshal(public, &roundtrip); err != nil || len(roundtrip) != 1 || *roundtrip[0].Binding != *binding {
		t.Fatalf("modern binding changed during JSON round trip: %v", err)
	}
}

func TestDeriveApprovalExpiry(t *testing.T) {
	expires := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	data, _ := json.Marshal(map[string]string{"approvalID": "a1", "expiresAt": expires.Format(time.RFC3339)})
	all := Derive([]store.ExecutionEvent{{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: data, CreatedAt: expires.Add(-time.Minute)}}, expires)
	if len(all) != 1 || all[0].Status != StatusExpired {
		t.Fatalf("approval state = %#v, want expired", all)
	}
}

func TestDeriveApprovalFirstTerminalDecisionWins(t *testing.T) {
	content := func(v map[string]string) json.RawMessage { data, _ := json.Marshal(v); return data }
	all := Derive([]store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "a1"})},
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Content: content(map[string]string{"approvalID": "a1"})},
		{Seq: 3, Type: events.ExecutionEventTypeApprovalDeclined, Content: content(map[string]string{"approvalID": "a1"})},
	}, time.Time{})
	if len(all) != 1 || all[0].Status != StatusApproved || all[0].DecisionSeq != 2 {
		t.Fatalf("approval state = %#v, want first terminal approval to win", all)
	}
}

func TestDeriveDuplicateRequestDoesNotResetTerminalApproval(t *testing.T) {
	requested, _ := json.Marshal(map[string]string{"approvalID": "approval-1", "action": "create_pr"})
	approved, _ := json.Marshal(map[string]string{"approvalID": "approval-1", "decision": "approve"})
	laterRequest, _ := json.Marshal(map[string]string{"approvalID": "approval-1", "action": "retry"})
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	derived := Derive([]store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: requested, CreatedAt: now},
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Content: approved, CreatedAt: now.Add(time.Second)},
		{Seq: 3, Type: events.ExecutionEventTypeApprovalRequested, Content: laterRequest, CreatedAt: now.Add(2 * time.Second)},
	}, now.Add(3*time.Second))
	if len(derived) != 1 || derived[0].Status != StatusApproved || derived[0].Action != "create_pr" {
		t.Fatalf("derived = %#v, want duplicate request to preserve approved state", derived)
	}
}

func TestFilterEventsForTaskUIDScopesRequestAndDecisionEvents(t *testing.T) {
	content := func(v map[string]string) json.RawMessage { data, _ := json.Marshal(v); return data }
	filtered := FilterEventsForTaskUID([]store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "old", "taskUID": "old-uid"})},
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Content: content(map[string]string{"approvalID": "old", "taskUID": "old-uid"})},
		{Seq: 3, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "new", "taskUID": "new-uid"})},
		{Seq: 4, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "legacy"})},
	}, "new-uid")
	if len(filtered) != 2 || filtered[0].Seq != 3 || filtered[1].Seq != 4 {
		t.Fatalf("filtered = %#v, want current task UID plus legacy untagged events", filtered)
	}
}

func TestResolvedIncludesTargetArgsPreview(t *testing.T) {
	preview := json.RawMessage(`{"incident":"inc-1"}`)
	resolved := Resolved([]Approval{{
		ID:                "approval-1",
		TargetTool:        "dispatch_work_order",
		TargetArgsPreview: preview,
		Status:            StatusApproved,
	}})
	if len(resolved) != 1 {
		t.Fatalf("resolved length = %d, want 1", len(resolved))
	}
	if string(resolved[0].TargetArgsPreview) != string(preview) {
		t.Fatalf("TargetArgsPreview = %s, want %s", resolved[0].TargetArgsPreview, preview)
	}
}

func TestResolvedBoundsDecisionReason(t *testing.T) {
	longReason := strings.Repeat("because ", maxApprovalTargetTextChars)
	resolved := Resolved([]Approval{{
		ID:             "approval-1",
		TargetTool:     "dispatch_work_order",
		Status:         StatusApproved,
		DecisionReason: longReason,
	}})
	if len(resolved) != 1 {
		t.Fatalf("resolved length = %d, want 1", len(resolved))
	}
	if len([]rune(resolved[0].Reason)) > maxApprovalTargetTextChars {
		t.Fatalf("Reason length = %d, want <= %d", len([]rune(resolved[0].Reason)), maxApprovalTargetTextChars)
	}
	if resolved[0].Reason == longReason {
		t.Fatalf("Reason was not bounded")
	}
}
