package approvals

import (
	"encoding/json"
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
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Content: content(map[string]string{"approvalID": "a1", "reason": "ok", "actor": "alice"}), CreatedAt: now.Add(time.Second)},
	}, now)
	if len(all) != 1 || all[0].Status != StatusApproved || all[0].DecisionSeq != 2 || all[0].DecisionReason != "ok" {
		t.Fatalf("approval state = %#v", all)
	}
}

func TestDeriveApprovalOmitsEmptyDecisionReason(t *testing.T) {
	content := func(v map[string]string) json.RawMessage { data, _ := json.Marshal(v); return data }
	all := Derive([]store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "a1"})},
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Summary: "approval approve", Content: content(map[string]string{"approvalID": "a1"})},
	}, time.Time{})
	if len(all) != 1 || all[0].DecisionReason != "" {
		t.Fatalf("approval = %#v, want empty decision reason", all)
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

func TestDeriveApprovalDuplicateRequestDoesNotReopenTerminal(t *testing.T) {
	content := func(v map[string]string) json.RawMessage { data, _ := json.Marshal(v); return data }
	all := Derive([]store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "a1"})},
		{Seq: 2, Type: events.ExecutionEventTypeApprovalApproved, Content: content(map[string]string{"approvalID": "a1"})},
		{Seq: 3, Type: events.ExecutionEventTypeApprovalRequested, Content: content(map[string]string{"approvalID": "a1"})},
	}, time.Time{})
	if len(all) != 1 || all[0].Status != StatusApproved || all[0].DecisionSeq != 2 {
		t.Fatalf("approval state = %#v, want terminal approval preserved", all)
	}
}
