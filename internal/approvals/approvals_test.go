package approvals

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
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
